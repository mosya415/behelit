package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// delegate(role, task) — the unit of orchestration through roles. The
// subagent is the same loop with another role, working in its own git
// worktree: a snapshot of the caller's working tree (uncommitted and new files
// included), so parallel delegations can't step on each other or on the user.
// When it stops, the verifier runs the task's check_cmd there. Only
// {status, diff, test_tail} goes back up — never the subagent's transcript.
// A passed diff is applied to the caller's tree (apply: verified).

func init() {
	registerTool(&ToolDef{
		Name: "delegate",
		Desc: "Delegate a self-contained change to another role.", // per session: delegateDescription
		Params: []Param{
			{Name: "role", Type: "string", Desc: "Role to run the task (see the list)", Required: true},
			{Name: "task", Type: "string", Desc: "Complete, self-contained task: goal, relevant files and facts, constraints, definition of done", Required: true},
			{Name: "check_cmd", Type: "string", Desc: "Command that proves the task is done (exit 0), e.g. \"go test ./pkg/parser/...\". Runs in addition to the role's own check_cmd"},
		},
		Body: "task",
		TextDoc: `Delegate a self-contained change to another role; it works in its own git
worktree and a verifier runs check_cmd. You get back only {status, diff, test_tail};
a passed diff is applied to your tree. Several delegates in one reply run in parallel:
<delegate role="coder" check_cmd="go test ./parser/...">
Fix the off-by-one in parser.Lex for trailing newlines; add a test.
</delegate>`,
		Parallel: true,
		Run:      runDelegateTool,
	})
}

func delegateDescription(s *Session) string {
	var b strings.Builder
	b.WriteString(`Delegate a self-contained change to another role. The subagent runs in an isolated git worktree (a snapshot of the current tree) with its own model and tools. When it finishes, a VERIFIER runs check_cmd there: the verifier, not the model, decides whether the task is done.

Returns JSON {"status", "diff", "test_tail"}:
- status: passed (check exit 0; the diff has been applied to your tree), failed (check still failing; not applied), unverified (no check_cmd; not applied), conflict (passed but didn't apply cleanly to your tree), error, cancelled
- diff: the subagent's changes as a unified diff
- test_tail: the last lines of the check output

Rules:
- The subagent sees none of your context: state the goal, the relevant files, constraints and the definition of done.
- Always give a check_cmd that fails before and passes after the change (narrow tests are faster).
- Independent delegations in one reply run in parallel; ones touching the same lines will conflict — split work by file or package.

Roles:
`)
	for _, a := range s.orch.roleList(s.agent.Name) {
		desc := strings.TrimSpace(a.Description)
		if a.CheckCmd != "" {
			desc += fmt.Sprintf(" (always verified with: %s; your check_cmd is added on top)", a.CheckCmd)
		}
		fmt.Fprintf(&b, "- %s: %s\n", a.Name, desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

type delegateResult struct {
	Status   string `json:"status"`
	Diff     string `json:"diff"`
	TestTail string `json:"test_tail"`
}

const maxDiffBytes = 60_000

func runDelegateTool(tc *ToolCtx, a Args) string {
	s, o := tc.S, tc.S.orch
	start := time.Now()
	if s.depth >= o.subagentDepth() {
		return fmt.Sprintf("error: subagent depth limit reached (%d) — do this work yourself", o.subagentDepth())
	}
	roleName, task := a.Str("role"), strings.TrimSpace(a.Str("task"))
	role := o.agents[roleName]
	if role == nil || !role.IsRole || roleName == s.agent.Name {
		var names []string
		for _, r := range o.roleList(s.agent.Name) {
			names = append(names, r.Name)
		}
		return fmt.Sprintf("error: unknown role %q. Available: %s", roleName, strings.Join(names, ", "))
	}
	if task == "" {
		return "error: task is required"
	}
	// The role's check_cmd always runs — the calling model can add a check
	// but not replace it (a caller-chosen "echo ok" must not pass the task).
	var checks []string
	if role.CheckCmd != "" {
		checks = append(checks, role.CheckCmd)
	}
	if extra := strings.TrimSpace(a.Str("check_cmd")); extra != "" && extra != role.CheckCmd {
		checks = append(checks, extra)
	}
	check := strings.Join(checks, " && ")
	for _, c := range checks {
		if err := s.jail().CheckCommand(c); err != nil {
			return marshalDelegate(delegateResult{Status: "error", TestTail: "check_cmd rejected by the sandbox: " + err.Error()})
		}
	}
	if msg, ok := tc.Ask("delegate", roleName, "DELEGATE "+roleName, "  "+truncate(task, 300)+"\n  check: "+orNone(check)); !ok {
		return msg
	}
	// Top-level delegations share the subagent concurrency bound.
	if s.depth == 0 {
		select {
		case o.slots <- struct{}{}:
			defer func() { <-o.slots }()
		case <-tc.Ctx.Done():
			return marshalDelegate(delegateResult{Status: "cancelled"})
		}
	}

	wt, err := o.worktrees.create(s.jail().Root, o.cfg.stateDir(), s.rootUID())
	if err != nil {
		return "error: cannot create a worktree: " + err.Error()
	}
	defer wt.remove()

	child, err := o.newChild(s, role, truncate(firstLine(task), 60))
	if err != nil {
		return "error: " + err.Error()
	}
	o.forgetChild(child.ID) // its worktree dies with this call: not resumable via task_id
	child.jl, err = NewJail(wt.root, o.jl.Allowed, o.jl.Unsafe)
	if child.jl != nil {
		child.jl.Shell = o.jl.Shell
	}
	if err != nil {
		return "error: " + err.Error()
	}
	child.isolated = true
	child.RefreshSystem()
	prompt := task
	if check != "" {
		prompt += fmt.Sprintf("\n\n---\nDefinition of done: `%s` exits 0 in this working tree. A verifier runs it after you stop; it decides, not you.", check)
	}
	child.Msgs = append(child.Msgs, Message{Role: "user", Content: prompt})
	s.event("delegate", map[string]any{"task_id": child.ID, "role": roleName, "check": check, "worktree": wt.dir})

	entry := o.trackStart(child.ID, "delegate", roleName, task)
	child.view.Begin()
	v := child.RunVerifiedAll(tc.Ctx, checks, o.verifyAttempts())
	child.view.Finish(v.Status, time.Since(start))

	diff, files, derr := wt.diff()
	if derr != nil && v.Status != "error" {
		v.Status, v.Tail = "error", "collecting the diff failed: "+derr.Error()
	}
	applied := false
	var apply bool
	switch o.applyPolicy() {
	case "verified":
		apply = v.Status == "passed"
	case "always":
		apply = v.Status == "passed" || v.Status == "failed" || v.Status == "unverified"
	}
	if apply && diff != "" {
		preview := fmt.Sprintf("   %s by %s:\n     %s", plural(files, "file changed", "files changed"), roleName+map[bool]string{true: ", check passed", false: ", check " + v.Status}[v.Status == "passed"], strings.Join(wt.changedFiles, "\n     "))
		// Every file the diff touches is checked against the edit rules; the
		// strictest decides (one denied file blocks the whole apply).
		pattern, act := "*", Allow
		for _, f := range wt.changedFiles {
			switch Evaluate("edit", permPath(s.jail(), f), s.rules()...) {
			case Deny:
				pattern, act = f, Deny
			case Ask:
				if act == Allow {
					pattern, act = f, Ask
				}
			}
			if act == Deny {
				break
			}
		}
		var msg string
		ok := act == Allow
		switch act {
		case Deny:
			msg = fmt.Sprintf("error: the diff touches %s, which a permission rule denies editing", pattern)
		case Ask:
			msg, ok = tc.Ask("edit", pattern, "APPLY diff from "+roleName, preview)
		}
		if !ok {
			v.Status = "not_applied"
			v.Tail = strings.TrimSpace(v.Tail + "\n" + msg)
		} else if err := wt.applyTo(s.jail().Root, diff); err != nil {
			v.Status = "conflict"
			v.Tail = strings.TrimSpace(v.Tail + "\n\napplying the diff to the caller's tree failed:\n" + err.Error())
		} else {
			applied = true
			for _, f := range wt.changedFiles {
				o.noteRead(s.jail(), f) // the caller has "seen" what it just merged
			}
		}
	}
	child.traceTask(task, v, check, len(diff), files, applied, start)
	detail := v.Tail
	if applied {
		detail = fmt.Sprintf("applied %s: %s\n\n%s", plural(files, "file", "files"), strings.Join(wt.changedFiles, ", "), v.Tail)
	}
	o.trackEnd(entry, v.Status, detail)
	child.saveTranscript()

	out := delegateResult{Status: v.Status, Diff: headTail(diff, maxDiffBytes), TestTail: v.Tail}
	if v.Status == "error" && v.Err != nil && out.TestTail == "" {
		out.TestTail = v.Err.Error()
	}
	return marshalDelegate(out)
}

func marshalDelegate(r delegateResult) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

func orNone(s string) string {
	if s == "" {
		return "(none — result will be unverified)"
	}
	return s
}

func (o *Orchestrator) applyPolicy() string {
	if o.roles != nil && o.roles.Apply != "" {
		return o.roles.Apply
	}
	return "verified"
}

// ── git worktrees ───────────────────────────────────────────────────────────

type worktrees struct{ mu sync.Mutex } // git's worktree bookkeeping isn't concurrency-safe

type worktree struct {
	mgr          *worktrees
	top          string   // caller's repository top level
	dir          string   // worktree directory
	root         string   // the subagent's jail root inside it (same subdir as the caller's)
	sub          string   // jail root relative to the repo top ("." = top)
	base         string   // snapshot commit
	changedFiles []string // relative to the jail root
}

// pathspec limits git to the caller's jail: a subagent's changes above its
// root (e.g. via make -C ..) never reach the caller's tree.
func (w *worktree) pathspec() []string {
	if w.sub == "." || w.sub == "" {
		return nil
	}
	return []string{"--", w.sub}
}

func gitCmd(dir string, env []string, stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=lca", "GIT_AUTHOR_EMAIL=lca@localhost",
		"GIT_COMMITTER_NAME=lca", "GIT_COMMITTER_EMAIL=lca@localhost", "GIT_TERMINAL_PROMPT=0")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// create snapshots the working tree under jailRoot — tracked changes and new
// non-ignored files, without touching the user's index or HEAD (a temporary
// index) — and checks the snapshot out as a detached worktree.
func (m *worktrees) create(jailRoot, lcaDir, session string) (*worktree, error) {
	top, err := gitCmd(jailRoot, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("delegate needs a git repository: %w", err)
	}
	top = strings.TrimSpace(top)
	if real, err := filepath.EvalSymlinks(top); err == nil {
		top = real
	}
	sub, err := filepath.Rel(top, jailRoot)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	idx, err := os.CreateTemp("", "lca-index-*")
	if err != nil {
		return nil, err
	}
	idx.Close()
	os.Remove(idx.Name()) // git wants to create it
	defer os.Remove(idx.Name())
	env := []string{"GIT_INDEX_FILE=" + idx.Name()}
	head, headErr := gitCmd(top, nil, nil, "rev-parse", "--verify", "-q", "HEAD")
	head = strings.TrimSpace(head)
	if headErr == nil && head != "" {
		if _, err := gitCmd(top, env, nil, "read-tree", head); err != nil {
			return nil, err
		}
	}
	if _, err := gitCmd(top, env, nil, "add", "-A"); err != nil {
		return nil, err
	}
	tree, err := gitCmd(top, env, nil, "write-tree")
	if err != nil {
		return nil, err
	}
	args := []string{"commit-tree", strings.TrimSpace(tree), "-m", "lca delegate snapshot"}
	if head != "" {
		args = append(args, "-p", head)
	}
	commit, err := gitCmd(top, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	commit = strings.TrimSpace(commit)

	base, err := filepath.Abs(filepath.Join(lcaDir, "worktrees"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(base, session+"-")
	if err != nil {
		return nil, err
	}
	os.Remove(dir) // worktree add wants to create it
	// no user hooks: post-checkout scripts don't belong in an agent's scratch copy
	if _, err := gitCmd(top, nil, nil, "-c", "core.hooksPath="+os.DevNull, "worktree", "add", "--detach", dir, commit); err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	w := &worktree{mgr: m, top: top, dir: dir, root: filepath.Join(dir, sub), sub: filepath.ToSlash(sub), base: commit}
	if info, err := os.Stat(w.root); err != nil || !info.IsDir() {
		w.removeLocked()
		return nil, fmt.Errorf("%s has no tracked or untracked non-ignored files, so it doesn't exist in the snapshot", sub)
	}
	return w, nil
}

// diff returns the subagent's changes against the snapshot (new files
// included) and how many files changed.
func (w *worktree) diff() (string, int, error) {
	if _, err := gitCmd(w.dir, nil, nil, "add", "-A"); err != nil {
		return "", 0, err
	}
	names, err := gitCmd(w.dir, nil, nil, append([]string{"diff", "--cached", "--name-only", w.base}, w.pathspec()...)...)
	if err != nil {
		return "", 0, err
	}
	w.changedFiles = nil
	for _, n := range strings.Split(strings.TrimSpace(names), "\n") {
		if n != "" {
			if w.sub != "." && w.sub != "" {
				n = strings.TrimPrefix(n, w.sub+"/")
			}
			w.changedFiles = append(w.changedFiles, n)
		}
	}
	d, err := gitCmd(w.dir, nil, nil, append([]string{"diff", "--cached", "--binary", w.base}, w.pathspec()...)...)
	return d, len(w.changedFiles), err
}

// applyTo applies the diff to the caller's working tree (not its index).
func (w *worktree) applyTo(jailRoot, diff string) error {
	w.mgr.mu.Lock()
	defer w.mgr.mu.Unlock()
	if _, err := gitCmd(w.top, nil, []byte(diff), "apply", "--check", "--binary", "-"); err != nil {
		return err
	}
	_, err := gitCmd(w.top, nil, []byte(diff), "apply", "--binary", "-")
	return err
}

func (w *worktree) remove() {
	if os.Getenv("LCA_KEEP_WORKTREES") != "" {
		return
	}
	w.mgr.mu.Lock()
	defer w.mgr.mu.Unlock()
	w.removeLocked()
}

func (w *worktree) removeLocked() {
	if _, err := gitCmd(w.top, nil, nil, "worktree", "remove", "--force", w.dir); err != nil {
		os.RemoveAll(w.dir)
	}
	gitCmd(w.top, nil, nil, "worktree", "prune")
}
