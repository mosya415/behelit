package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	"sync"
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

	// Repeat is which run of -repeat N this row is, 1-based, and 1 for a plain
	// run. It is what makes the rows of one task distinguishable: model variance
	// is large enough that a single run proves nothing, so the interesting number
	// is "3 of 5 passed" and that cannot be read off rows that all look alike.
	Repeat int `json:"repeat"`

	// The three fields without which two eval runs are not comparable, which is
	// the only reason anybody runs an eval twice (requirement P1-4). The build,
	// the team as it was loaded, and the hash of the system prompt this task's
	// role actually ran with. A pass rate that moved after a prompt edit is a
	// different measurement, not a better one, and these are what let a reader
	// tell the two apart months later.
	LCAVersion string `json:"lca_version"`
	RolesHash  string `json:"roles_hash"`
	PromptHash string `json:"prompt_hash"`
}

// evalJob is one cell of the matrix: one task, in one tier, with one tool
// transport, on one of -repeat N runs, in its own directory. It exists so that
// -j has something to hand a worker: the dimensions are decided once, up front,
// and a worker only runs what it is given.
type evalJob struct {
	task   *EvalTask
	tier   string
	mode   string
	repeat int
	dir    string
}

func (j evalJob) label() string {
	l := j.task.Name
	if j.tier != "" {
		l += faint(gSep+"%s", j.tier)
	}
	if j.mode != "" {
		l += faint(gSep+"%s", j.mode)
	}
	if j.repeat > 1 {
		l += faint(gSep+"run %d", j.repeat)
	}
	return l
}

// repeatDir is the path segment that keeps -repeat N's runs apart. Empty for a
// plain run, so `-repeat 1` and no -repeat at all write the same tree and an
// existing layout does not move under anybody.
func repeatDir(rep, of int) string {
	if of <= 1 {
		return ""
	}
	return fmt.Sprintf("run-%d", rep)
}

// sortEvalResults puts the rows back into the plan's order — tier, transport,
// task, repetition — whatever order the workers finished in.
func sortEvalResults(all []EvalResult) {
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		switch {
		case a.Tier != b.Tier:
			return a.Tier < b.Tier
		case a.Transport != b.Transport:
			return a.Transport < b.Transport
		case a.Task != b.Task:
			return a.Task < b.Task
		}
		return a.Repeat < b.Repeat
	})
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
	repeat := fset.Int("repeat", 1, "run every task this many times (model variance is large; one run proves nothing)")
	par := fset.Int("j", 1, "run this many tasks at once, each in its own tree")
	compare := fset.Bool("compare", false, "read the named results.jsonl files and print them side by side instead of running anything")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	// -compare reads finished runs and talks to nothing: no gateway, no roles, no
	// workspace. It is handled before any of that is set up, so it answers in an
	// environment where a run would not start — which is most of the environments
	// somebody compares two old runs in.
	if *compare {
		return runEvalCompare(fset.Args())
	}
	if fset.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: lca eval [-out dir] [-role r] [-run regexp] [-transport native,text] [-tier cheap,premium] [-repeat N] [-j N] [-keep] [-v] tasks/")
		fmt.Fprintln(os.Stderr, "       lca eval -compare a/results.jsonl b/results.jsonl")
		return 2
	}
	if *repeat < 1 {
		fmt.Fprintf(os.Stderr, "eval: -repeat %d: give a count of 1 or more\n", *repeat)
		return 2
	}
	if *par < 1 {
		fmt.Fprintf(os.Stderr, "eval: -j %d: give a count of 1 or more\n", *par)
		return 2
	}
	// -j and MCP cannot share a process, and this is the only place that can say
	// so before anything runs.
	//
	// The MCP tool table is process-wide (mcp.go's mcpTools / toolRegistry /
	// toolOrder), and every orchestrator registers into it. One process, N
	// orchestrators: the second registration rebinds every tool's *MCPServer to
	// the newest task's connection — an unlocked write under the sessions already
	// reading that field, and semantically worse than the race, because all N
	// tasks' Jira calls then go over one task's connection and one task's jail,
	// and the first `defer orch.CloseMCP()` cancels it under the others, which
	// reads in results.jsonl as Jira being down. With differing configs the full
	// path runs instead and deletes from toolRegistry while a running session
	// resolves names out of it: a hard "concurrent map read and map write" that
	// takes the whole matrix with it and writes no rows for anything in flight.
	//
	// Refusing is the honest fix rather than a lock: the unsynchronised reads are
	// in the RUNNING sessions, not in registration, so serialising registration
	// would hide the race and keep the cross-talk. A matrix that needs MCP runs
	// with -j 1, which is slower and correct.
	if *par > 1 {
		if fc, err := loadFileConfig(cfg); err == nil && fc.MCP != nil && len(fc.MCP.Order) > 0 {
			fmt.Fprintf(os.Stderr, "eval: -j %d with an mcp: block (%s): the MCP tool table is shared by the whole process, so parallel tasks would send each other's calls over one connection and lose every MCP tool when the first task finishes — run this matrix with -j 1\n",
				*par, strings.Join(fc.MCP.Order, ", "))
			return 2
		}
		// A task can still bring MCP with it through its own LCA_ROOT and
		// .lca/config.yaml, which nothing here can see. buildOrchestrator refuses
		// that one when it finds it, so the task fails with a reason instead of
		// racing; this flag is written before any worker starts and only read
		// afterwards.
		mcpRefuseSecondRegistration.Store(true)
		defer mcpRefuseSecondRegistration.Store(false)
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

	// The role override is applied ONCE, here, and not inside the loop. It
	// mutates the shared task, and with -j the loop body runs on several
	// goroutines at once: writing t.Role from each of them is a data race over a
	// field they all then read.
	if *role != "" {
		for _, t := range tasks {
			t.Role = *role
		}
	}

	// The whole matrix as a flat list before anything runs: tier × transport ×
	// task × repetition. Flat because -j hands them to workers, and a worker
	// taking the next job off one list cannot leave a hole in the matrix the way
	// four nested loops with a shared counter could.
	var jobs []evalJob
	for _, tier := range tiers {
		for _, mode := range modes {
			for _, t := range tasks {
				for rep := 1; rep <= *repeat; rep++ {
					jobs = append(jobs, evalJob{task: t, tier: tier, mode: mode, repeat: rep,
						dir: filepath.Join(append([]string{*out}, nonEmpty(tier, mode, t.Name, repeatDir(rep, *repeat))...)...)})
				}
			}
		}
	}

	what := plural(len(tasks), "task", "tasks")
	if len(tiers) > 1 {
		what += " × " + strings.Join(tiers, ", ")
	}
	if len(modes) > 1 {
		what += " × " + strings.Join(modes, ", ")
	}
	if *repeat > 1 {
		what += fmt.Sprintf(" × %d runs", *repeat)
	}
	workers := *par
	if workers > len(jobs) {
		// Clamped, and said out loud below: `-j 16` over four jobs is four, and a
		// line claiming sixteen in parallel is a line somebody will quote when they
		// wonder why the gateway was not busier.
		workers = len(jobs)
	}
	if workers > 1 {
		what += faint(gSep+"%s", plural(workers, "at a time", "at a time"))
	}
	section("eval", faint("%s", what))

	passed, total := 0, 0
	interrupted := false
	planned := len(jobs)
	var all []EvalResult
	// One mutex over the three things a finished job touches: the results file,
	// which is append-only but whose lines must not interleave; the tallies; and
	// stdout, because two half-printed rows are two rows nobody can read.
	var mu sync.Mutex
	// git worktree add and worktree remove take a lock on the repository, and
	// every job without a `repo:` fixture makes a worktree of the current repo:
	// run those at once and git fails them with "unable to lock", which would be
	// reported as a task that could not be prepared. Serialising the two git legs
	// costs a second each and makes -j safe in the shape the document's own
	// example uses.
	var gitMu sync.Mutex
	next := 0
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(jobs) || ctx.Err() != nil {
					if next < len(jobs) {
						interrupted = true
					}
					mu.Unlock()
					return
				}
				j := jobs[next]
				next++
				mu.Unlock()

				tcfg := cfg
				tcfg.TransportOverride = j.mode
				tcfg.Tier = j.tier
				res := runEvalTask(tcfg, j, &gitMu, *keep, *verbose)

				mu.Lock()
				total++
				all = append(all, res)
				b, _ := json.Marshal(res)
				rf.Write(append(b, '\n'))
				if res.Status == "passed" {
					passed++
				}
				// Under outMu as well as under mu. The workers' own live output goes
				// through view.go's sayLine, which holds outMu; these two prints held
				// only eval's own mutex, so with six workers a finished-job row could
				// land in the middle of another task's error message. Two locks over
				// one stdout is one lock too few.
				outMu.Lock()
				fmt.Printf("  %s  %s\n", padTo(statusWord(res.Status), 14, 0),
					j.label()+faint("  %s", fmtDurShort(time.Duration(res.DurationMs)*time.Millisecond)))
				if res.Error != "" {
					hint("%s", ellipsize(res.Error, 200))
				}
				outMu.Unlock()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Finished jobs arrive in whatever order the workers got through them, and a
	// results table that reorders itself between two runs of the same matrix is a
	// table nobody can diff. The file keeps completion order — it is a log — and
	// the table is sorted back into the plan's order.
	sortEvalResults(all)

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
		if *repeat > 1 {
			row = append(row, strconv.Itoa(r.Repeat))
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
	if *repeat > 1 {
		head = append(head, "run")
	}
	head = append(head, "status", "time", "turns", "in", "cached", "out", "tools", "invalid", "delegated", "fallbacks")
	// the task name is the identifier here; every other column is a number that is
	// naturally short, so the allocator cannot shave it at all — and the lintel is
	// drawn to the table, so the two end in the same column at every width
	sectionTable("results", "", head, rows, 0)
	if len(modes) > 1 {
		printVariantComparison("native vs text", "transport", modes, all, func(r EvalResult) string { return r.Transport })
	}
	if len(tiers) > 1 {
		printVariantComparison("tiers", "tier", tiers, all, func(r EvalResult) string { return r.Tier })
	}
	// With -repeat the per-task spread is the whole point of having run it more
	// than once: "3 of 5" is a number somebody can act on and five rows reading
	// passed, failed, passed, passed, failed are not.
	if *repeat > 1 {
		printRepeatSpread(all, *repeat)
	}
	fmt.Println()
	if interrupted {
		warnLine("interrupted after %d of %d — whatever was already running finished, nothing new started", total, planned)
	}
	if passed == total && !interrupted {
		okLine("%d/%d passed", passed, total)
	} else {
		errLine("%d/%d passed", passed, total)
	}
	hint("per-task results: %s"+gSep+"traces next to them", shortDir(resultsPath))
	if *repeat > 1 || len(jobs) > 1 {
		hint("compare this run with another: lca eval -compare %s other/results.jsonl", shortDir(resultsPath))
	}
	if passed != total {
		return 1
	}
	return 0
}

// printRepeatSpread is what -repeat N was run for: per task, how many of the N
// runs passed and how many passed on the FIRST attempt, with the medians beside
// them. Medians and not means, for the reason the medians in -compare are
// medians: one run that burned its whole step budget moves a mean by more than
// it tells anybody.
func printRepeatSpread(all []EvalResult, repeat int) {
	section("spread", faint("%s each", plural(repeat, "run", "runs")))
	var rows [][]string
	absent := 0
	for _, g := range groupEvalRows(all, func(r EvalResult) string { return evalCellKey(r) }) {
		s := summarizeEvalRows(g.rows)
		absent += s.n - s.ran
		rows = append(rows, []string{g.key, fmt.Sprintf("%d/%d", s.passed, s.n), fmt.Sprintf("%d/%d", s.first, s.n),
			s.tokensCol(), s.timeCol()})
	}
	table([]string{"task", "passed", "first try", "tokens", "time"}, rows)
	if absent > 0 {
		hint("the token and time medians are over the runs that reached the gateway"+gSep+"%s never ran",
			plural(absent, "run", "runs"))
	}
}

// ── lca eval -compare ───────────────────────────────────────────────────────

// Two results.jsonl files, side by side. This is the question an eval exists to
// answer and the one a single run cannot: did the prompt edit (or the model
// swap, or the tier) actually move anything?
//
// Four numbers per task per file, and they are chosen rather than collected:
//
//   - pass rate, because that is the score;
//   - passed on the FIRST attempt, because a task that needs the verifier to
//     push back twice before it passes is a different result from one that gets
//     it right, and both are "passed";
//   - median tokens and median time, because that is the cost, and the MEDIAN
//     because a run that ran into its step ceiling moves a mean by hundreds of
//     thousands of tokens and tells the reader nothing they can act on.
//
// It reads nothing but the files: no gateway, no roles.yaml, no workspace. A
// comparison of two runs from last month must not need this month's config to
// be loadable.
func runEvalCompare(files []string) int {
	if len(files) < 2 {
		errLine("eval -compare needs two results.jsonl files (or more)")
		hint("lca eval -compare before/results.jsonl after/results.jsonl")
		return 2
	}
	type set struct {
		label string
		rows  []EvalResult
	}
	var sets []set
	for _, f := range files {
		rows, err := readEvalResults(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "eval -compare:", err)
			return 2
		}
		if len(rows) == 0 {
			errLine("eval -compare: %s has no result rows", shortDir(f))
			return 2
		}
		sets = append(sets, set{label: compareLabel(f, files), rows: rows})
	}

	// The identity of each run, printed before the numbers and not instead of
	// them: two files whose lca_version, roles_hash and prompt_hash all match are
	// the same measurement twice, and a reader comparing them should be told that
	// before they conclude anything about the difference. Rows written by a binary
	// from before those fields existed say so rather than showing a blank.
	var idRows [][]string
	for _, s := range sets {
		// firstNonEmpty and not report.go's dash(): that one escapes for HTML, and
		// this table is printed to a terminal.
		idRows = append(idRows, []string{s.label, strconv.Itoa(len(s.rows)),
			firstNonEmpty(commonField(s.rows, func(r EvalResult) string { return r.LCAVersion }), gNil),
			firstNonEmpty(shortHash(commonField(s.rows, func(r EvalResult) string { return r.RolesHash })), gNil),
			firstNonEmpty(shortHash(commonField(s.rows, func(r EvalResult) string { return r.PromptHash })), gNil)})
	}
	section("compared runs")
	table([]string{"file", "rows", "lca", "roles", "prompt"}, idRows)

	// Every task either file mentions, in one order, so a task present in one run
	// and missing from the other is visible as a gap instead of silently dropped.
	var keys []string
	seen := map[string]bool{}
	for _, s := range sets {
		for _, r := range s.rows {
			if k := evalCellKey(r); !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)

	var rows [][]string
	for _, k := range keys {
		for i, s := range sets {
			var group []EvalResult
			for _, r := range s.rows {
				if evalCellKey(r) == k {
					group = append(group, r)
				}
			}
			task := k
			if i > 0 {
				task = "" // the task names itself once; the rows under it are its files
			}
			if len(group) == 0 {
				rows = append(rows, []string{task, s.label, gNil, gNil, gNil, gNil})
				continue
			}
			sm := summarizeEvalRows(group)
			rows = append(rows, []string{task, s.label, fmt.Sprintf("%d/%d", sm.passed, sm.n),
				fmt.Sprintf("%d/%d", sm.first, sm.n), sm.tokensCol(), sm.timeCol()})
		}
	}
	absent := 0
	for i, s := range sets {
		sm := summarizeEvalRows(s.rows)
		absent += sm.n - sm.ran
		task := "total"
		if i > 0 {
			task = ""
		}
		rows = append(rows, []string{task, s.label, fmt.Sprintf("%d/%d", sm.passed, sm.n),
			fmt.Sprintf("%d/%d", sm.first, sm.n), sm.tokensCol(), sm.timeCol()})
	}
	sectionTable("comparison", "", []string{"task", "file", "passed", "first try", "tokens", "time"}, rows, 0)
	// Said out loud, because the medians left those rows out and a reader
	// comparing two files has to know the cost columns describe fewer runs than
	// the pass rate beside them.
	if absent > 0 {
		hint("the token and time medians are over the rows that reached the gateway"+gSep+"%s never ran",
			plural(absent, "row", "rows"))
	}
	fmt.Println()
	return 0
}

// readEvalResults reads a results.jsonl. A line that will not parse is COUNTED
// and skipped rather than fatal: a run killed mid-write leaves a half line, and
// refusing to compare the other four hundred rows because of it helps nobody.
func readEvalResults(path string) ([]EvalResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []EvalResult
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r EvalResult
		if json.Unmarshal([]byte(line), &r) != nil || r.Task == "" {
			bad++
			continue
		}
		// A trace.jsonl is not a results.jsonl, and it used to be read as one: a
		// trace's own task record carries `task`, `status`, `attempts` and
		// `duration_ms`, so every field this reader asked for was there. The run's
		// closing hint says "per-task results: <out>/results.jsonl · traces next to
		// them", which puts the neighbour one tab-complete away — and the comparison
		// then printed `1/1 passed, 0 tokens, 5.0s` against a real file's `5/5, 420k,
		// 120.0s`, with the only signal being the `—` in the identity columns that
		// this reader documents as meaning "written before those fields existed".
		//
		// `type` is the discriminator: every trace record has one and no eval row
		// does. Counted as unreadable, so the warning says the file is not one.
		if evalRowType(line) != "" {
			bad++
			continue
		}
		// A row from before -repeat existed has no repeat field; it is run 1.
		if r.Repeat == 0 {
			r.Repeat = 1
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", shortDir(path), err)
	}
	if bad > 0 {
		warnLine("%s: %s skipped", shortDir(path), plural(bad, "unreadable line", "unreadable lines"))
		if len(out) == 0 {
			hint("%s", "a trace is not a results.jsonl"+gSep+"-compare wants the results.jsonl beside it")
		}
	}
	return out, nil
}

// evalRowType is the `type` field of one line, or "" when it has none. A trace
// record always has one ("turn", "task", "run"); an eval row has no such field
// at all, so this is the one question that separates the two files.
func evalRowType(line string) string {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil {
		return ""
	}
	return probe.Type
}

// evalCellKey names one cell of the matrix — the thing whose repetitions are
// grouped and whose numbers are compared. The tier and the transport are part of
// it: a file holding `native` and `text` rows for one task holds two
// measurements, and averaging them would compare a mixture with a mixture.
func evalCellKey(r EvalResult) string {
	k := r.Task
	if r.Tier != "" {
		k += "/" + r.Tier
	}
	if r.Transport != "" {
		k += "/" + r.Transport
	}
	return k
}

type evalGroup struct {
	key  string
	rows []EvalResult
}

// groupEvalRows groups rows by key, keeping first-seen order.
func groupEvalRows(all []EvalResult, key func(EvalResult) string) []evalGroup {
	var out []evalGroup
	at := map[string]int{}
	for _, r := range all {
		k := key(r)
		if i, ok := at[k]; ok {
			out[i].rows = append(out[i].rows, r)
			continue
		}
		at[k] = len(out)
		out = append(out, evalGroup{key: k, rows: []EvalResult{r}})
	}
	return out
}

type evalSummary struct {
	n, passed, first int
	// ran is how many of the n rows got as far as a model call. The cost medians
	// are over THOSE rows and not over all of them: see summarizeEvalRows.
	ran                int
	medTokens, medMs   int
	tokens, durationMs []int
}

// evalRowRan says whether a row describes a run that happened. A row whose
// workspace could not be made, or whose gateway was not there, is written with
// status "error", 0 tokens and a duration of however long it took to find that
// out — it is an ABSENT run, not a cheap one.
//
// By status and not by "did it report tokens": a row that ran and reported no
// usage (a gateway that does not send it) is still a run, and leaving it out of
// the median would be the same mistake from the other side.
func evalRowRan(r EvalResult) bool {
	switch r.Status {
	case "passed", "failed", statusBudget, "timeout", statusUnverified:
		return true
	}
	return false
}

// tokensCol and timeCol are the two cost columns. "—" and not 0 when no row in
// the group ran: 0 tokens is a claim about a run, and there was no run.
func (s evalSummary) tokensCol() string {
	if s.ran == 0 {
		return gNil
	}
	return kfmt(s.medTokens)
}

func (s evalSummary) timeCol() string {
	if s.ran == 0 {
		return gNil
	}
	return fmtDurShort(time.Duration(s.medMs) * time.Millisecond)
}

// summarizeEvalRows is the four numbers, over however many rows.
//
// "first try" is passed AND attempts == 1. A row written before attempts was
// recorded reports 0, and 0 is not 1, so it does not count — the alternative is
// crediting a first-try pass to a run that may have taken three.
func summarizeEvalRows(rows []EvalResult) evalSummary {
	var s evalSummary
	s.n = len(rows)
	for _, r := range rows {
		if r.Status == "passed" {
			s.passed++
			if r.Attempts == 1 {
				s.first++
			}
		}
		// The cost medians are over the rows that PRODUCED a run. A task that died
		// at `workspace:` reports 0 tokens and 80 ms, and three of those in five
		// rows made the median 0 — so a file holding three infra failures and two
		// expensive passes read as "free and instant" next to the run it was being
		// compared with. A zero-token row is not a cheap run; it is an absent one.
		if !evalRowRan(r) {
			continue
		}
		s.ran++
		s.tokens = append(s.tokens, r.PromptTokens+r.CompletionTokens)
		s.durationMs = append(s.durationMs, int(r.DurationMs))
	}
	s.medTokens, s.medMs = medianInt(s.tokens), medianInt(s.durationMs)
	return s
}

// medianInt is the middle value, or the mean of the two middle ones. It sorts a
// COPY: the caller's slice is the row order, which the table still needs.
func medianInt(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	c := append([]int(nil), xs...)
	sort.Ints(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// commonField is the value every row agrees on, or "" when they do not — which
// is itself the answer worth printing: a results.jsonl whose rows disagree about
// the binary or the prompt is a file somebody appended two different runs to,
// and no single hash describes it.
func commonField(rows []EvalResult, of func(EvalResult) string) string {
	v := ""
	for i, r := range rows {
		got := of(r)
		if i == 0 {
			v = got
			continue
		}
		if got != v {
			return ""
		}
	}
	return v
}

// shortHash is a sha256 as a reader uses it: enough to tell two apart, short
// enough to sit in a column. Anything that is not a hash — "unknown", a module
// version — is left alone.
func shortHash(s string) string {
	if len(s) > 12 && !strings.Contains(s, ".") {
		return s[:12]
	}
	return s
}

// compareLabel names a file in a column. Two runs are almost always
// <out>/<timestamp>/results.jsonl, so the base name alone would print
// "results.jsonl" twice; the parent directory is the part that differs.
func compareLabel(path string, all []string) string {
	base := filepath.Base(path)
	parent := filepath.Base(filepath.Dir(path))
	if parent == "" || parent == "." || parent == string(filepath.Separator) {
		return base
	}
	same := 0
	for _, p := range all {
		if filepath.Base(p) == base {
			same++
		}
	}
	if same > 1 {
		return parent
	}
	return base
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

// runEvalTask runs one cell of the matrix. gitMu serialises the two legs that
// take a lock on the enclosing repository — making the workspace and removing
// it — because with -j several of these run at once and git's own lock failure
// would be reported as a task that could not be prepared.
func runEvalTask(cfg Config, j evalJob, gitMu *sync.Mutex, keep, verbose bool) (res EvalResult) {
	t, dir := j.task, j.dir
	// The identity of the row goes on FIRST, before anything can fail: a task
	// whose workspace could not be made still writes a line in results.jsonl, and
	// that line has to say which run of which cell it was and what binary was
	// trying. The roles hash and the prompt hash come later, when there is a team
	// and a session to read them from.
	res = EvalResult{Task: t.Name, Status: "error", Trace: filepath.Join(dir, "trace.jsonl"),
		Transport: j.mode, Tier: j.tier, Repeat: j.repeat, LCAVersion: lcaVersion()}
	start := time.Now()
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		res.Error = err.Error()
		return res
	}

	work := filepath.Join(dir, "work")
	gitMu.Lock()
	cleanup, err := prepareWorkspace(cfg, t, work)
	gitMu.Unlock()
	if err != nil {
		res.Error = "workspace: " + err.Error()
		return res
	}
	if !keep {
		defer func() {
			gitMu.Lock()
			cleanup()
			gitMu.Unlock()
		}()
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
	// An eval task has no keyboard by construction, and the one class -y does not
	// grant (mcp_write) would otherwise reach a door. A hung task holds its
	// worktree and never writes its row in results.jsonl.
	ap.Unattended("lca eval runs unattended")
	orch, err := setupOrchestrator(tcfg, ap, res.Trace)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer orch.rec.Close()
	// Every eval task builds its own orchestrator, so a stdio MCP server would
	// otherwise be left running once per task.
	defer orch.CloseMCP()
	// The team as it was LOADED, recorded as soon as there is one: an eval whose
	// rows do not say which roles.yaml produced them cannot be compared with the
	// one after the edit, which is the only reason to keep the rows.
	res.RolesHash = rolesHash(orch.roles)
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
	// Read from the session that will run, not re-assembled: this is the prompt
	// the gateway is about to be sent, including the project's own instructions
	// and the transport's tool documentation. Taken before the first turn, because
	// compaction rewrites Msgs[0] and the hash has to name what the run STARTED
	// from.
	res.PromptHash = promptFingerprint(sess)
	attempts := t.VerifyAttempts
	if attempts == 0 {
		attempts = orch.verifyAttempts()
	}
	// roles.yaml's defaults: are in force HERE too. Only main.go used to build a
	// budget, so `defaults: {max_steps: 200, max_tokens: 4000000}` was silently
	// ignored by the eval — the document's own second caller of lca — and by
	// `lca run`: every method on a nil budget is nil-safe, so the ceilings
	// evaluated to "none" without a word, and `doctor -role` printed them anyway.
	// A task that loops burns whatever the gateway will give it. The task's own
	// `timeout:` overrides only the clock, which is the one ceiling a task is
	// allowed an opinion about.
	if b, err := newRunBudget(orch.roles, t.Timeout, 0, 0); err == nil {
		orch.budget = b
	}
	ctx, cancel := orch.budget.start(context.Background())
	orch.setRunContext(ctx)
	sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: t.Prompt})
	vstart := time.Now()
	v := sess.RunVerified(ctx, t.CheckCmd, attempts)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		v.Status = "timeout"
	// A task that ran out of steps or tokens did not fail the task: it never
	// finished it. Rolling that into "failed" makes a pass rate that moved because
	// a ceiling was lowered look like a model that got worse.
	case v.Status != "passed" && orch.budget.tripped() != "":
		v.Status = statusBudget
		if v.Err == nil {
			v.Err = errors.New(orch.budget.tripped())
		}
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
		// --expire, never bare: a bare prune in one lca deletes the admin entry of a
		// worktree another lca created milliseconds ago, and that worktree's next git
		// command fails with "not a git repository" — which nobody can act on. It
		// used to cost a scratch copy here; under `apply: branch` the entry can
		// belong to a worktree holding a verified delegation's branch, and running
		// `lca eval` in the same repository as an unattended session is ordinary.
		// Same reason as delegate.go's and clean.go's prunes, which already say so.
		gitCmd(top, nil, nil, "worktree", "prune", "--expire=1.hour.ago")
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
		sayLine(v.prefixed(s, nil))
	}
}

// prefixed puts the task name on EVERY line, not only the first. With -j six
// tasks write to one terminal, and a message whose second line carries no name —
// "nothing answers at that endpoint — is the gateway up?" under "a local failed:
// connection refused" — belongs to one of six runs with nothing to say which.
// style, when given, is applied per line, so a colour closes where its line does.
func (v *evalView) prefixed(s string, style func(string) string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if style != nil {
			l = style(l)
		}
		lines[i] = "   " + faint("%s", v.name) + " " + l
	}
	return strings.Join(lines, "\n")
}
func (v *evalView) Stream() StreamView { return nullStream{} }
func (v *evalView) ToolStart(name, summary string) {
	v.say(faint("%s", strings.ToUpper(name)) + " " + summary)
}
func (v *evalView) ToolDone(name string, a Args, r string) {}
func (v *evalView) Note(t string)                          { v.say(faint("%s", t)) }
func (v *evalView) Warn(t string)                          { v.say(warn("%s", t)) }
func (v *evalView) Error(t string) {
	sayLine(v.prefixed(t, func(l string) string { return cRed + l + cReset }))
}
func (v *evalView) Perf(Usage)                   {}
func (v *evalView) Todos([]Todo)                 {}
func (v *evalView) Live() io.Writer              { return nil }
func (v *evalView) Begin()                       {}
func (v *evalView) Finish(string, time.Duration) {}
func (v *evalView) Check(cmd string, exit int, d time.Duration, attempt, attempts int) {
	v.say(checkText(cmd, exit, d, attempt, attempts))
}

func runIn(dir string, argv []string) (string, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
