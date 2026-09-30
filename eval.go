package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// lca eval — agent evaluation on top of the trace. Each task is a prompt plus
// a check_cmd, run unattended in a fresh workspace; the verifier's exit code
// is the score, and cost/efficiency metrics are read back from the run's
// trace JSONL (turns, tokens, cached_tokens, tool calls, fallbacks,
// delegations).
//
//	lca eval [-out dir] [-role r] [-run regexp] [-transport native,text] [-tier cheap,premium] [-keep] [-v] tasks/
//
// -transport runs every task once per listed tool transport (forcing it for
// all roles) and prints them side by side: pass rate and the share of invalid
// tool calls settle "native vs text" with data from your own fleet. -tier does
// the same over roles.yaml's tiers: the workflow developed against premium
// models, scored again on commodity ones.
//
// A task is tasks/<name>.yaml or tasks/<name>/task.yaml:
//
//	prompt: |
//	  Fix the parser so trailing newlines don't produce an empty token.
//	check_cmd: go test ./parser/...
//	role: lead                 # default: roles.yaml entry
//	repo: fixture              # dir (relative to the task file) copied into a
//	                           # fresh git repo; omit = a worktree of the
//	                           # current repo's HEAD
//	setup: [go mod download]   # run in the workspace first (no sandbox)
//	timeout: 1800              # seconds
//	verify_attempts: 2
//
// Everything that would ask for approval is approved (the run is unattended);
// the sandbox (allowlist, GPU policy) and deny rules still apply.

type EvalTask struct {
	Name           string
	Dir            string
	Prompt         string
	CheckCmd       string
	Role           string
	Repo           string
	Setup          []string
	Timeout        time.Duration
	VerifyAttempts int
}

type EvalResult struct {
	Task              string   `json:"task"`
	Transport         string   `json:"transport,omitempty"`
	Tier              string   `json:"tier,omitempty"`
	Status            string   `json:"status"`
	Attempts          int      `json:"attempts"`
	CheckExit         *int     `json:"check_exit,omitempty"`
	DurationMs        int64    `json:"duration_ms"`
	Turns             int      `json:"turns"`
	PromptTokens      int      `json:"prompt_tokens"`
	CachedTokens      int      `json:"cached_tokens"`
	CacheRatio        float64  `json:"cache_ratio"`
	CompletionTokens  int      `json:"completion_tokens"`
	ToolCalls         int      `json:"tool_calls"`
	ToolErrors        int      `json:"tool_errors"`
	InvalidCalls      int      `json:"invalid_calls"`
	AttemptedCalls    int      `json:"attempted_calls"` // tool calls incl. unparsable text tags
	InvalidRate       float64  `json:"invalid_rate"`    // invalid / attempted tool calls
	Fallbacks         int      `json:"fallbacks"`
	Delegations       int      `json:"delegations"`
	DelegationsPassed int      `json:"delegations_passed"`
	Models            []string `json:"models"`
	Trace             string   `json:"trace"`
	Error             string   `json:"error,omitempty"`
}

// runEval takes a ctx for the same reason doctor does: a matrix of tasks is
// minutes of gateway time, and Ctrl-C must stop the next task from starting.
func runEval(ctx context.Context, cfg Config, args []string) int {
	fset := flag.NewFlagSet("eval", flag.ContinueOnError)
	out := fset.String("out", "", "output directory (default $LCA_DIR/eval/<timestamp>)")
	role := fset.String("role", "", "role for every task (overrides the task's)")
	runRe := fset.String("run", "", "only tasks whose name matches this regexp")
	keep := fset.Bool("keep", false, "keep task workspaces")
	verbose := fset.Bool("v", false, "print every tool call")
	transports := fset.String("transport", "", "comma-separated tool transports to compare (native,text); default: as configured")
	tierList := fset.String("tier", "", "comma-separated tiers to compare (roles.yaml tiers:); default: as configured")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: lca eval [-out dir] [-role r] [-run regexp] [-transport native,text] [-tier cheap,premium] [-keep] [-v] tasks/")
		return 2
	}
	var filter *regexp.Regexp
	if *runRe != "" {
		var err error
		if filter, err = regexp.Compile(*runRe); err != nil {
			fmt.Fprintln(os.Stderr, "eval: -run:", err)
			return 2
		}
	}
	var tasks []*EvalTask
	for _, p := range fset.Args() {
		ts, err := loadEvalTasks(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "eval:", err)
			return 2
		}
		for _, t := range ts {
			if filter == nil || filter.MatchString(t.Name) {
				tasks = append(tasks, t)
			}
		}
	}
	if len(tasks) == 0 {
		// Styled like every other refusal in the program: a naked lowercase line in the
		// middle of a session reads as output from something else.
		errLine("eval: no tasks found")
		hint("a task is a directory holding task.md (or task.yaml) with a prompt and a check — README has the format")
		return 2
	}
	if *out == "" {
		*out = filepath.Join(cfg.Dir, "eval", time.Now().Format("20060102-150405"))
	}
	if abs, err := filepath.Abs(*out); err == nil {
		*out = abs
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 2
	}
	// The project's roles.yaml lives in the repo eval runs from, which a fixture
	// workspace doesn't contain; the user's ~/.lca/roles.yaml is read as usual.
	if os.Getenv("LCA_ROLES") == "" {
		for _, p := range []string{filepath.Join(cfg.Root, ".lca", "roles.yaml")} {
			if _, err := os.Stat(p); err == nil {
				abs, _ := filepath.Abs(p)
				os.Setenv("LCA_ROLES", abs)
				break
			}
		}
	}

	// Up front, so a typo does not burn the matrix: the tiers are read from the
	// team the tasks will run with, before the first task starts.
	tiers := []string{""}
	if *tierList != "" {
		rc, err := loadRoles(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "eval: roles:", err)
			return 2
		}
		tiers = nil
		for _, ti := range strings.Split(*tierList, ",") {
			ti = strings.TrimSpace(ti)
			if _, ok := rc.Tiers[ti]; !ok {
				fmt.Fprintf(os.Stderr, "eval: -tier: %q is not a tier in this team (tiers: %s)\n", ti, rc.tierList())
				return 2
			}
			tiers = append(tiers, ti)
		}
	}

	resultsPath := filepath.Join(*out, "results.jsonl")
	rf, err := os.OpenFile(resultsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 2
	}
	defer rf.Close()

	modes := []string{""}
	if *transports != "" {
		modes = nil
		for _, m := range strings.Split(*transports, ",") {
			m = strings.TrimSpace(m)
			if m != transportNative && m != transportText {
				fmt.Fprintf(os.Stderr, "eval: -transport: %q is not native or text\n", m)
				return 2
			}
			modes = append(modes, m)
		}
	}

	what := plural(len(tasks), "task", "tasks")
	if len(tiers) > 1 {
		what += " × " + strings.Join(tiers, ", ")
	}
	if len(modes) > 1 {
		what += " × " + strings.Join(modes, ", ")
	}
	section("eval", faint("%s", what))
	passed, total := 0, 0
	interrupted := false
	planned := len(tiers) * len(modes) * len(tasks)
	var all []EvalResult
	for _, tier := range tiers {
		for _, mode := range modes {
			for _, t := range tasks {
				if ctx.Err() != nil {
					interrupted = true
					break
				}
				if *role != "" {
					t.Role = *role
				}
				tcfg := cfg
				tcfg.TransportOverride = mode
				tcfg.Tier = tier
				dir := filepath.Join(append([]string{*out}, nonEmpty(tier, mode, t.Name)...)...)
				res := runEvalTask(tcfg, t, dir, *keep, *verbose)
				res.Transport, res.Tier = mode, tier
				total++
				all = append(all, res)
				b, _ := json.Marshal(res)
				rf.Write(append(b, '\n'))
				if res.Status == "passed" {
					passed++
				}
				label := t.Name
				if tier != "" {
					label += faint(gSep+"%s", tier)
				}
				if mode != "" {
					label += faint(gSep+"%s", mode)
				}
				fmt.Printf("  %s  %s\n", padTo(statusWord(res.Status), 14, 0), label+faint("  %s", fmtDurShort(time.Duration(res.DurationMs)*time.Millisecond)))
				if res.Error != "" {
					hint("%s", truncate(res.Error, 200))
				}
			}
		}
	}

	section("results")
	var rows [][]string
	for _, r := range all {
		cached := gNil
		if r.PromptTokens > 0 {
			cached = fmt.Sprintf("%d%%", int(r.CacheRatio*100))
		}
		invalid := "0"
		if r.AttemptedCalls > 0 {
			invalid = fmt.Sprintf("%d/%d", r.InvalidCalls, r.AttemptedCalls)
		}
		row := []string{r.Task}
		if len(tiers) > 1 {
			row = append(row, r.Tier)
		}
		if len(modes) > 1 {
			row = append(row, r.Transport)
		}
		row = append(row, statusWord(r.Status), fmtDurShort(time.Duration(r.DurationMs)*time.Millisecond), strconv.Itoa(r.Turns),
			kfmt(r.PromptTokens), cached, kfmt(r.CompletionTokens), strconv.Itoa(r.ToolCalls), invalid,
			fmt.Sprintf("%d/%d", r.DelegationsPassed, r.Delegations), strconv.Itoa(r.Fallbacks))
		rows = append(rows, row)
	}
	head := []string{"task"}
	if len(tiers) > 1 {
		head = append(head, "tier")
	}
	if len(modes) > 1 {
		head = append(head, "transport")
	}
	head = append(head, "status", "time", "turns", "in", "cached", "out", "tools", "invalid", "delegated", "fallbacks")
	table(head, rows)
	if len(modes) > 1 {
		printVariantComparison("native vs text", "transport", modes, all, func(r EvalResult) string { return r.Transport })
	}
	if len(tiers) > 1 {
		printVariantComparison("tiers", "tier", tiers, all, func(r EvalResult) string { return r.Tier })
	}
	fmt.Println()
	if interrupted {
		warnLine("interrupted after %d of %d — a task already running finished, the next did not start", total, planned)
	}
	if passed == total && !interrupted {
		okLine("%d/%d passed", passed, total)
	} else {
		errLine("%d/%d passed", passed, total)
	}
	hint("per-task results: %s"+gSep+"traces next to them", shortDir(resultsPath))
	if passed != total {
		return 1
	}
	return 0
}

// printVariantComparison summarizes each variant over the same tasks: pass rate,
// invalid-call rate, turns, tokens and cache ratio side by side.
func printVariantComparison(title, column string, labels []string, all []EvalResult, of func(EvalResult) string) {
	section(title)
	var rows [][]string
	for _, m := range labels {
		var n, pass, turns, prompt, cached, invalid, attempted int
		for _, r := range all {
			if of(r) != m {
				continue
			}
			n++
			if r.Status == "passed" {
				pass++
			}
			turns += r.Turns
			prompt += r.PromptTokens
			cached += r.CachedTokens
			invalid += r.InvalidCalls
			attempted += r.AttemptedCalls
		}
		if n == 0 {
			continue
		}
		rate, cache := 0.0, 0.0
		if attempted > 0 {
			rate = float64(invalid) / float64(attempted)
		}
		if prompt > 0 {
			cache = float64(cached) / float64(prompt)
		}
		rows = append(rows, []string{m, fmt.Sprintf("%d/%d", pass, n), fmt.Sprintf("%.1f%%", rate*100),
			fmt.Sprintf("%.1f", float64(turns)/float64(n)), kfmt(prompt), fmt.Sprintf("%.0f%%", cache*100)})
	}
	table([]string{column, "passed", "invalid calls", "turns/task", "tokens in", "cached"}, rows)
}

// nonEmpty drops the unset dimensions of the matrix from a path, so a plain run
// still writes <out>/<task> and not <out>///<task>.
func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func loadEvalTasks(path string) ([]*EvalTask, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		t, err := loadEvalTask(path)
		if err != nil {
			return nil, err
		}
		return []*EvalTask{t}, nil
	}
	var files []string
	entries, _ := os.ReadDir(path)
	for _, e := range entries {
		p := filepath.Join(path, e.Name())
		switch {
		case !e.IsDir() && (strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")):
			files = append(files, p)
		case e.IsDir():
			if _, err := os.Stat(filepath.Join(p, "task.yaml")); err == nil {
				files = append(files, filepath.Join(p, "task.yaml"))
			}
		}
	}
	sort.Strings(files)
	var out []*EvalTask
	for _, f := range files {
		t, err := loadEvalTask(f)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func loadEvalTask(file string) (*EvalTask, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	doc := parseYAMLish(string(data))
	name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if name == "task" {
		name = filepath.Base(filepath.Dir(file))
	}
	t := &EvalTask{Name: name, Dir: filepath.Dir(file), Prompt: strings.TrimSpace(doc.str("prompt")), CheckCmd: doc.str("check_cmd"),
		Role: doc.str("role"), Repo: doc.str("repo"), Timeout: 30 * time.Minute}
	if t.Prompt == "" || t.CheckCmd == "" {
		return nil, fmt.Errorf("%s: prompt and check_cmd are required", file)
	}
	if s := doc.child("setup"); s != nil {
		t.Setup = s.List
		if len(s.List) == 0 && strings.TrimSpace(s.Value) != "" {
			t.Setup = []string{s.Value}
		}
	}
	if n, err := strconv.Atoi(doc.str("timeout")); err == nil && n > 0 {
		t.Timeout = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(doc.str("verify_attempts")); err == nil && n > 0 {
		t.VerifyAttempts = n
	}
	return t, nil
}

func runEvalTask(cfg Config, t *EvalTask, dir string, keep, verbose bool) (res EvalResult) {
	res = EvalResult{Task: t.Name, Status: "error", Trace: filepath.Join(dir, "trace.jsonl")}
	start := time.Now()
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		res.Error = err.Error()
		return res
	}

	work := filepath.Join(dir, "work")
	cleanup, err := prepareWorkspace(cfg, t, work)
	if err != nil {
		res.Error = "workspace: " + err.Error()
		return res
	}
	if !keep {
		defer cleanup()
	}
	for _, c := range t.Setup {
		argv, err := tokenize(c)
		if err != nil || len(argv) == 0 {
			res.Error = fmt.Sprintf("setup %q: bad command", c)
			return res
		}
		if out, err := runIn(work, argv); err != nil {
			res.Error = fmt.Sprintf("setup %q: %v\n%s", c, err, lastLines(out, 20, 2000))
			return res
		}
	}

	tcfg := cfg
	tcfg.Root = work
	tcfg.StateDir = filepath.Join(dir, "lca") // audit, transcripts, worktrees; config still comes from cfg.Dir
	ap := NewApprover(newStringInput(""))
	ap.TrustAll() // unattended; sandbox and deny rules still hold
	ap.quiet = true
	orch, err := setupOrchestrator(tcfg, ap, res.Trace)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer orch.rec.Close()
	// Every eval task builds its own orchestrator, so a stdio MCP server would
	// otherwise be left running once per task.
	defer orch.CloseMCP()
	view := &evalView{name: t.Name, verbose: verbose}
	roleName := t.Role
	if roleName == "" && orch.roles != nil {
		roleName = orch.roles.Entry
	}
	sess, err := orch.NewPrimary(firstNonEmpty(roleName, "build"), "", view)
	if err != nil {
		orch.tracer.Close()
		res.Error = err.Error()
		return res
	}
	attempts := t.VerifyAttempts
	if attempts == 0 {
		attempts = orch.verifyAttempts()
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.Timeout)
	sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: t.Prompt})
	vstart := time.Now()
	v := sess.RunVerified(ctx, t.CheckCmd, attempts)
	if ctx.Err() == context.DeadlineExceeded {
		v.Status = "timeout"
	}
	cancel()
	sess.traceTask(t.Prompt, v, t.CheckCmd, 0, 0, false, nil, vstart, "")
	sess.saveTranscript()
	orch.tracer.Close()

	res.Status, res.Attempts = v.Status, v.Attempts
	if v.Checked {
		exit := v.Exit
		res.CheckExit = &exit
	}
	if v.Err != nil {
		res.Error = v.Err.Error()
	}
	scoreTrace(res.Trace, sess.UID, &res)
	return res
}

// scoreTrace aggregates a run's trace into the result's metrics.
func scoreTrace(path, rootUID string, res *EvalResult) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	models := map[string]bool{}
	attempted := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var head struct {
			Type string `json:"type"`
		}
		line := sc.Bytes()
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		switch head.Type {
		case "turn":
			var t TurnRecord
			if json.Unmarshal(line, &t) != nil {
				continue
			}
			if strings.HasSuffix(t.Session, "-compact") {
				continue // housekeeping on the cheap role, not the agent's work
			}
			res.Turns++
			res.PromptTokens += t.Usage.PromptTokens
			res.CachedTokens += t.Usage.CachedTokens
			res.CompletionTokens += t.Usage.CompletionTokens
			res.ToolCalls += len(t.ToolCalls)
			invalidInCalls := 0
			for _, c := range t.ToolCalls {
				if !c.OK {
					res.ToolErrors++
				}
				if c.Invalid {
					invalidInCalls++
				}
			}
			res.InvalidCalls += t.InvalidCalls
			attempted += len(t.ToolCalls) + (t.InvalidCalls - invalidInCalls) // + unparsable text tags
			res.Fallbacks += len(t.Fallbacks)
			if t.Model != "" {
				models[t.Model] = true
			}
		case "task":
			var t TaskRecord
			if json.Unmarshal(line, &t) != nil || t.Session == rootUID {
				continue
			}
			res.Delegations++
			if t.Status == "passed" {
				res.DelegationsPassed++
			}
		}
	}
	if res.PromptTokens > 0 {
		res.CacheRatio = float64(res.CachedTokens) / float64(res.PromptTokens)
	}
	res.AttemptedCalls = attempted
	if attempted > 0 {
		res.InvalidRate = float64(res.InvalidCalls) / float64(attempted)
	}
	res.Models = sortedKeys(models)
}

// prepareWorkspace makes the task's working tree: a copy of its fixture repo
// as a fresh git repository, or a detached worktree of the current repo's HEAD.
func prepareWorkspace(cfg Config, t *EvalTask, work string) (func(), error) {
	if t.Repo != "" {
		src := t.Repo
		if !filepath.IsAbs(src) {
			src = filepath.Join(t.Dir, src)
		}
		if err := copyTree(src, work); err != nil {
			return nil, err
		}
		for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "--allow-empty", "-m", "eval fixture"}} {
			if _, err := gitCmd(work, nil, nil, args...); err != nil {
				return nil, err
			}
		}
		return func() { os.RemoveAll(work) }, nil
	}
	top, err := gitCmd(cfg.Root, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("no repo: for tasks without repo:, run eval inside a git repository: %w", err)
	}
	top = strings.TrimSpace(top)
	if _, err := gitCmd(top, nil, nil, "worktree", "add", "--detach", work, "HEAD"); err != nil {
		return nil, err
	}
	return func() {
		gitCmd(top, nil, nil, "worktree", "remove", "--force", work)
		gitCmd(top, nil, nil, "worktree", "prune")
	}, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		o, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(o, in)
		o.Close()
		return err
	})
}

// evalView prints a run's activity compactly, prefixed by the task name.
type evalView struct {
	name    string
	verbose bool
}

func (v *evalView) say(s string) {
	if v.verbose {
		sayLine("   " + faint("%s", v.name) + " " + s)
	}
}
func (v *evalView) Stream() StreamView { return nullStream{} }
func (v *evalView) ToolStart(name, summary string) {
	v.say(faint("%s", strings.ToUpper(name)) + " " + summary)
}
func (v *evalView) ToolDone(name string, a Args, r string) {}
func (v *evalView) Note(t string)                          { v.say(faint("%s", t)) }
func (v *evalView) Warn(t string)                          { v.say(warn("%s", t)) }
func (v *evalView) Error(t string)                         { sayLine("   " + faint("%s", v.name) + " " + cRed + t + cReset) }
func (v *evalView) Perf(Usage)                             {}
func (v *evalView) Todos([]Todo)                           {}
func (v *evalView) Live() io.Writer                        { return nil }
func (v *evalView) Begin()                                 {}
func (v *evalView) Finish(string, time.Duration)           {}
func (v *evalView) Check(cmd string, exit int, d time.Duration, attempt, attempts int) {
	v.say(checkText(cmd, exit, d, attempt, attempts))
}

func runIn(dir string, argv []string) (string, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
