package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Workflow tests run against the fake gateway. testRoles has no sandbox.shell,
// so a test workflow uses single commands (echo, cat, test, ls) or `sh -c '…'`
// — never `&&`.

const wfRoles = testRoles + `  reviewer:
    description: Reviews diffs.
    models: [cheap-a]
    effort: low
    tools: [read_file, grep, write]
`

func wfHarness(t *testing.T, fs *fakeServer) (*harness, string) {
	t.Helper()
	fs.models = allModels()
	h := newRoleHarness(t, fs, wfRoles, true)
	return h, t.TempDir()
}

// alwaysReply answers every model call with the same text.
func alwaysReply(t *testing.T, text string) *fakeServer {
	return newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: text} })
}

func readSteps(t *testing.T, h *harness) []StepRecord {
	t.Helper()
	data, err := os.ReadFile(h.orch.tracer.Path)
	if err != nil {
		t.Fatal(err)
	}
	var out []StepRecord
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var head struct{ Type string }
		if json.Unmarshal(line, &head) != nil || head.Type != "step" {
			continue
		}
		var r StepRecord
		if json.Unmarshal(line, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// buildWF writes the workflow into dir, validates it against the harness team
// and opens a run directory — the same path runWorkflow takes, minus the flags.
func buildWF(t *testing.T, h *harness, dir, text string, vars map[string]string) *wfRunner {
	t.Helper()
	wf := loadWF(t, h, dir, text, vars)
	rdir, st := newRunState(h.orch.cfg, h.orch, wf, wf.effectiveVars(vars))
	r, err := newRunner(h.orch, h.sess, wf, rdir, st)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func loadWF(t *testing.T, h *harness, dir, text string, vars map[string]string) *Workflow {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, err := loadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := wf.bind(h.orch, h.sess.agent.Name, wf.effectiveVars(vars)); err != nil {
		t.Fatal(err)
	}
	return wf
}

func runWF(t *testing.T, h *harness, dir, text string, vars map[string]string) (int, *WorkflowState, *wfRunner) {
	t.Helper()
	r := buildWF(t, h, dir, text, vars)
	code := runRunner(t, r)
	return code, r.st, r
}

func runRunner(t *testing.T, r *wfRunner) int {
	t.Helper()
	code := 0
	captureStdout(t, func() { code = r.Run(context.Background()) })
	return code
}

// resumeRunner reopens a recorded run the way `lca run -resume` does.
func resumeRunner(t *testing.T, h *harness, wf *Workflow, dir string, cliVars map[string]string) *wfRunner {
	t.Helper()
	st, err := loadRunState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkResume(wf, st, cliVars); err != nil {
		t.Fatal(err)
	}
	attachRun(h.orch, dir, st)
	r, err := newRunner(h.orch, h.sess, wf, dir, st)
	if err != nil {
		t.Fatal(err)
	}
	r.log.header("== resume %s (session %s) ==", wfNow(), h.orch.rec.id)
	return r
}

func readLog(t *testing.T, r *wfRunner) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, "run.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func stepByName(st *WorkflowState, name string) *StepState {
	for i := range st.Steps {
		if st.Steps[i].Name == name {
			return &st.Steps[i]
		}
	}
	return nil
}

// ── parsing ─────────────────────────────────────────────────────────────────

// The shipped example is the reference workflow: it must parse and bind.
func TestWorkflowParse(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("examples", "workflows", "harden.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := wfHarness(t, alwaysReply(t, "ok"))
	wf, err := parseWorkflow("harden", "examples/workflows/harden.yaml", string(data))
	if err != nil {
		t.Fatal(err)
	}
	if wf.Name != "harden" || wf.Role != "lead" || !strings.HasPrefix(wf.Desc, "Plan a change,") {
		t.Fatalf("top level: %+v", wf)
	}
	var names, kinds []string
	for _, s := range wf.Steps {
		names = append(names, s.Name)
		kinds = append(kinds, s.Kind)
	}
	if strings.Join(names, ",") != "baseline,plan,build,fmt,review,integrate" {
		t.Fatalf("document order is execution order: %v", names)
	}
	if strings.Join(kinds, ",") != "run,prompt,delegate,run,prompt,run" {
		t.Fatalf("kinds: %v", kinds)
	}
	if wf.Vars["tests"] != "./..." || wf.Vars["task"] != "" {
		t.Fatalf("vars: %#v", wf.Vars)
	}
	build := wf.byName["build"]
	if build.Text != "${steps.plan.out}\n\nImplement exactly this. Stay inside the files named above." {
		t.Fatalf("delegate block scalar: %q", build.Text)
	}
	if build.Retries != 1 || build.OnFail != "stop" || build.When != nil {
		t.Fatalf("build: %+v", build)
	}
	plan := wf.byName["plan"]
	if !strings.HasPrefix(plan.Text, "Write a build brief for this change:\n${vars.task}\n\n") ||
		!strings.Contains(plan.Text, "sees none of this conversation.") {
		t.Fatalf("prompt block scalar: %q", plan.Text)
	}
	if !strings.Contains(wf.byName["review"].Text, `"VERDICT: ship"`) {
		t.Fatalf("review prompt: %q", wf.byName["review"].Text)
	}
	if len(wf.byName["integrate"].When) != 1 || len(wf.byName["integrate"].When[0].terms) != 2 {
		t.Fatalf("integrate when: %+v", wf.byName["integrate"].When)
	}
	if wf.byName["baseline"].Timeout != 1800*time.Second {
		t.Fatalf("baseline timeout: %s", wf.byName["baseline"].Timeout)
	}

	if err := wf.bind(h.orch, "lead", wf.effectiveVars(map[string]string{"task": "harden the runner"})); err != nil {
		t.Fatal(err)
	}
	// check_timeout: 30 in wfRoles is the fallback for a run step that declares none.
	if got := wf.byName["fmt"].Timeout; got != 30*time.Second {
		t.Fatalf("fmt timeout after bind: %s", got)
	}
	if got := wf.byName["review"].Role; got != "reviewer" {
		t.Fatalf("review role: %q", got)
	}
	// A model step inherits no bound: killing a long delegation is worse.
	if got := wf.byName["build"].Timeout; got != 0 {
		t.Fatalf("delegate timeout: %s", got)
	}

	// '#' inside a block scalar is text, not a comment.
	wf2, err := parseWorkflow("hash", "hash.yaml", "steps:\n  a:\n    prompt: |\n      line one\n      # part of the prompt\n")
	if err != nil {
		t.Fatal(err)
	}
	if wf2.Steps[0].Text != "line one\n# part of the prompt" {
		t.Fatalf("block scalar comment: %q", wf2.Steps[0].Text)
	}
}

func TestWorkflowParseErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{"two actions", "steps:\n  a:\n    run: echo x\n    prompt: hi\n", []string{"step a", "two actions"}},
		{"no action", "steps:\n  a:\n    check: ls\n", []string{"step a", "no action"}},
		{"unknown step key", "steps:\n  a:\n    run: echo x\n    on-fail: stop\n", []string{"step a", `"on-fail"`}},
		{"typo retries", "steps:\n  a:\n    run: echo x\n    retires: 1\n", []string{"step a", `"retires"`}},
		{"unknown top key", "rolez: lead\nsteps:\n  a:\n    run: echo x\n", []string{`"rolez"`}},
		{"duplicate step", "steps:\n  a:\n    run: echo x\n  a:\n    run: echo y\n", []string{"step a", "duplicate"}},
		{"bad step name", "steps:\n  Build!:\n    run: echo x\n", []string{"Build!", "bad name"}},
		{"role on run", "steps:\n  a:\n    run: echo x\n    role: coder\n", []string{"step a", "run step"}},
		{"bad retries", "steps:\n  a:\n    run: echo x\n    retries: soon\n", []string{"step a", "retries"}},
		{"zero timeout", "steps:\n  a:\n    run: echo x\n    timeout: 0\n", []string{"step a", "timeout"}},
		{"bad on_fail", "steps:\n  a:\n    run: echo x\n    on_fail: halt\n", []string{"step a", "on_fail"}},
		{"bad when", "steps:\n  a:\n    run: echo x\n  b:\n    run: echo y\n    when: ${steps.a.status} =~ ok\n", []string{"step b", "when"}},
		{"no steps", "role: lead\n", []string{"no steps"}},
		{"empty action", "steps:\n  a:\n    run:\n", []string{"step a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseWorkflow("t", "t.yaml", c.yaml)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q lacks %q", err, w)
				}
			}
			if !strings.HasPrefix(err.Error(), "t.yaml: ") {
				t.Fatalf("error must name the file: %q", err)
			}
		})
	}
}

func TestWorkflowValidateReferences(t *testing.T) {
	fs := alwaysReply(t, "ok")
	h, dir := wfHarness(t, fs)
	cases := []struct {
		name, yaml, want string
	}{
		{"forward reference", "steps:\n  a:\n    run: echo ${steps.later.out}\n  later:\n    run: echo x\n", "does not run before"},
		{"unknown step", "steps:\n  a:\n    run: echo ${steps.nope.out}\n", "no step named"},
		{"diff on a prompt step", "steps:\n  plan:\n    prompt: hi\n  b:\n    run: echo ${steps.plan.diff}\n", "has no diff"},
		{"unknown field", "steps:\n  a:\n    run: echo x\n  b:\n    run: echo ${steps.a.nope}\n", "has no nope"},
		{"when names a later step", "steps:\n  a:\n    run: echo x\n    when: ${steps.b.status} == ok\n  b:\n    run: echo y\n", "does not run before"},
		{"malformed placeholder", "steps:\n  a:\n    run: echo ${vars}\n", "bad placeholder"},
		{"unknown var", "steps:\n  a:\n    run: echo ${vars.nope}\n", "neither declared"},
		{"required var missing", "vars:\n  task:\nsteps:\n  a:\n    run: echo ${vars.task}\n", "declared without a value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf, err := parseWorkflow("t", filepath.Join(dir, "t.yaml"), c.yaml)
			if err == nil {
				err = wf.bind(h.orch, "lead", wf.effectiveVars(nil))
			}
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q lacks %q", err, c.want)
			}
		})
	}
	if n := len(fs.reqs()); n != 0 {
		t.Fatalf("validation must cost no tokens, got %d requests", n)
	}
	if runs := listRuns(h.orch.cfg, 0); len(runs) != 0 {
		t.Fatalf("validation must create no run directory, got %d", len(runs))
	}
}

func TestWorkflowBindErrors(t *testing.T) {
	fs := alwaysReply(t, "ok")
	h, dir := wfHarness(t, fs)
	cases := []struct {
		name, yaml, want string
		remote           bool
	}{
		{name: "delegate with no check", yaml: "steps:\n  a:\n    delegate: do it\n    role: cheap\n", want: "needs check:"},
		{name: "delegate to the lead", yaml: "steps:\n  a:\n    delegate: do it\n    role: lead\n    check: ls\n", want: "must differ from the lead"},
		{name: "unknown role", yaml: "steps:\n  a:\n    prompt: hi\n    role: nope\n", want: `no role "nope"`},
		{name: "delegate on a remote team", yaml: "steps:\n  a:\n    delegate: do it\n    role: coder\n", want: "local git worktrees", remote: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.remote {
				h.orch.remote = localRemote(dir)
				defer func() { h.orch.remote = nil }()
			}
			wf, err := parseWorkflow("t", filepath.Join(dir, "t.yaml"), c.yaml)
			if err != nil {
				t.Fatal(err)
			}
			err = wf.bind(h.orch, "lead", wf.effectiveVars(nil))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("bind error %v, want %q", err, c.want)
			}
		})
	}
	if n := len(fs.reqs()); n != 0 {
		t.Fatalf("bind must cost no tokens, got %d requests", n)
	}
}

// ── running ─────────────────────────────────────────────────────────────────

// The feature's whole premise: shell steps are a program, not a conversation.
func TestWorkflowShellStepsCostNoTokens(t *testing.T) {
	fs := alwaysReply(t, "should not be called")
	h, dir := wfHarness(t, fs)
	code, st, r := runWF(t, h, dir, `steps:
  one:
    run: echo first
  two:
    run: echo second
  three:
    run: echo third
`, nil)
	if code != 0 || st.Status != stepOK || st.Cursor != 3 {
		t.Fatalf("exit %d status %s cursor %d", code, st.Status, st.Cursor)
	}
	if n := len(fs.reqs()); n != 0 {
		t.Fatalf("shell steps must not call a model, got %d requests", n)
	}
	for _, name := range []string{"one", "two", "three"} {
		ss := stepByName(st, name)
		if ss == nil || ss.Status != stepOK || ss.Usage != nil || ss.Role != "" {
			t.Fatalf("step %s: %+v", name, ss)
		}
	}
	log := readLog(t, r)
	for _, want := range []string{"== 1/3", "== 2/3", "== 3/3", "first", "second", "third"} {
		if !strings.Contains(log, want) {
			t.Fatalf("run.log lacks %q:\n%s", want, log)
		}
	}
}

func TestWorkflowExpandPlaceholders(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	r := buildWF(t, h, dir, "steps:\n  z:\n    run: echo z\n", nil)
	defer r.Close()
	r.st.Vars = map[string]string{"k": "v", "args": "-n hello"}
	r.res["a"] = &StepState{Name: "a", Kind: stepRun, Status: stepOK, Exit: 2, Out: "hello world"}
	r.res["s"] = &StepState{Name: "s", Kind: stepRun, Status: stepSkipped}

	for _, c := range []struct{ in, want string }{
		{"${vars.k}", "v"},
		{"${vars.args}", "-n hello"},
		{"x ${steps.a.out} y", "x hello world y"},
		{"${steps.a.status}", stepOK},
		{"${steps.a.exit}", "2"},
		{"${steps.s.out}", ""},
		{"${steps.s.status}", stepSkipped},
		{"${steps.s.exit}", "0"},
		{"nothing", "nothing"},
	} {
		got, err := r.expand(c.in, false)
		if err != nil || got != c.want {
			t.Fatalf("expand(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := r.expand("${steps.ghost.out}", false); err == nil {
		t.Fatal("an unknown reference must be an error")
	}
	if _, err := r.expand("${vars.ghost}", false); err == nil {
		t.Fatal("an unknown variable must be an error")
	}
}

// A step's recorded out is capped, because a resume reads it back from
// state.json; the full output stays in run.log.
func TestWorkflowStepOutputIsCapped(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	big := strings.Repeat(strings.Repeat("x", 99)+"\n", 200)
	if err := os.WriteFile(filepath.Join(h.root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	_, st, r := runWF(t, h, dir, "steps:\n  dump:\n    run: cat big.txt\n", nil)
	ss := stepByName(st, "dump")
	if ss.Status != stepOK {
		t.Fatalf("step: %+v", ss)
	}
	if len(ss.Out) > tailBytes+8 || !strings.HasPrefix(ss.Out, "…") {
		t.Fatalf("out not capped to the tail: %d bytes, starts %q", len(ss.Out), truncate(ss.Out, 20))
	}
	if log := readLog(t, r); len(log) < len(big) {
		t.Fatalf("run.log must keep the full output: %d bytes for %d", len(log), len(big))
	}
}

// A step's output is model/command output, i.e. untrusted: it must reach the
// next command as one argument, never as shell syntax.
func TestWorkflowStepOutputIsQuotedIntoCommands(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	_, st, r := runWF(t, h, dir, `steps:
  a:
    run: echo "; touch pwned"
  b:
    run: echo ${steps.a.out}
  c:
    run: echo '$(touch pwned2)'
  d:
    run: echo ${steps.c.out}
`, nil)
	for _, bad := range []string{"pwned", "pwned2"} {
		if _, err := os.Stat(filepath.Join(h.root, bad)); err == nil {
			t.Fatalf("%s was created: a step's output became shell syntax", bad)
		}
	}
	if got := stepByName(st, "b").Out; got != "; touch pwned" {
		t.Fatalf("step b out: %q", got)
	}
	if got := stepByName(st, "d").Out; got != "$(touch pwned2)" {
		t.Fatalf("step d out: %q", got)
	}
	if log := readLog(t, r); !strings.Contains(log, "; touch pwned") {
		t.Fatalf("run.log lacks the literal output:\n%s", log)
	}
}

// A var comes from the file or the command line — the same trust as roles.yaml
// allow: — so it stays several argv words.
func TestWorkflowVarsAreVerbatim(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	_, st, _ := runWF(t, h, dir, "steps:\n  a:\n    run: echo ${vars.args}\n", map[string]string{"args": "-n hello there"})
	if got := stepByName(st, "a").Out; got != "hello there" {
		t.Fatalf("out %q: a var must not be quoted into one word", got)
	}
}

func TestWorkflowPromptStepFeedsNextStep(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "PLAN-" + req.Model}
	})
	h, dir := wfHarness(t, fs)
	code, st, r := runWF(t, h, dir, "steps:\n  plan:\n    prompt: write a plan\n  echo:\n    run: echo ${steps.plan.out}\n", nil)
	if code != 0 || st.Status != stepOK {
		t.Fatalf("exit %d status %s", code, st.Status)
	}
	plan := stepByName(st, "plan")
	if plan.Role != "lead" || plan.Model != "lead-a" || plan.Checked {
		t.Fatalf("plan step: %+v", plan)
	}
	if plan.Usage == nil || plan.Usage.PromptTokens <= 0 {
		t.Fatalf("a model step must record its usage: %+v", plan.Usage)
	}
	if plan.Session == "" || plan.Session == h.sess.UID {
		t.Fatalf("a prompt step needs its own session uid, got %q (root %q)", plan.Session, h.sess.UID)
	}
	if !strings.Contains(readLog(t, r), "PLAN-lead-a") {
		t.Fatalf("the prompt's answer must reach the next step:\n%s", readLog(t, r))
	}
}

// The retry contract for model steps is RunVerifiedAll's, not a second loop.
func TestWorkflowPromptStepCheckRetries(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if !strings.Contains(req.Body, "The verifier ran") {
			return fakeReply{content: "I think it is done"}
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "created done.txt"}
		}
		return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "done.txt", "content": "ok\n"})}}
	})
	h, dir := wfHarness(t, fs)
	code, st, _ := runWF(t, h, dir, "steps:\n  work:\n    prompt: create done.txt\n    role: coder\n    check: ls done.txt\n    retries: 1\n", nil)
	ss := stepByName(st, "work")
	if code != 0 || ss.Status != stepOK || ss.Attempts != 2 || !ss.Checked {
		t.Fatalf("exit %d step %+v", code, ss)
	}
	reqs := fs.reqs()
	if len(reqs) < 2 || !strings.Contains(reqs[1].Body, "The verifier ran") {
		t.Fatalf("the failing check must go back as a user message; %d requests", len(reqs))
	}
}

// Session.checkLive is the opt-in that keeps contract 6 for a model step's
// check without changing interactive output.
func TestWorkflowPromptCheckOutputReachesLog(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "done, I promise"))
	code, st, r := runWF(t, h, dir, "steps:\n  work:\n    prompt: create done.txt\n    role: coder\n    check: ls done.txt\n", nil)
	ss := stepByName(st, "work")
	if code != 1 || ss.Status != stepFailed || !ss.Checked {
		t.Fatalf("exit %d step %+v", code, ss)
	}
	log := strings.ToLower(readLog(t, r))
	if !strings.Contains(log, "no such file") && !strings.Contains(log, "cannot access") {
		t.Fatalf("the check's own output must reach run.log:\n%s", log)
	}

	// An interactive-shaped session leaves checkLive nil, so nothing is teed.
	before := len(readLog(t, r))
	child, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer h.orch.forgetChild(child.ID)
	if child.checkLive != nil {
		t.Fatal("a normal child must not stream check output anywhere")
	}
	child.Msgs = append(child.Msgs, Message{Role: "user", Content: "do nothing"})
	captureStdout(t, func() { child.RunVerified(context.Background(), "ls done.txt", 1) })
	if after := len(readLog(t, r)); after != before {
		t.Fatalf("run.log grew by %d bytes from a session that is not part of the run", after-before)
	}
}

func TestWorkflowDelegateStep(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "created done.txt"}
			}
			return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "done.txt", "content": "ok\n"})}}
		}
		return fakeReply{content: "ok"}
	})
	h, dir := wfHarness(t, fs)
	code, st, r := runWF(t, h, dir, "steps:\n  build:\n    delegate: create done.txt\n    role: coder\n  show:\n    run: echo ${steps.build.diff}\n", nil)
	ss := stepByName(st, "build")
	if code != 0 || ss.Status != stepOK {
		t.Fatalf("exit %d step %+v", code, ss)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "done.txt")); string(got) != "ok\n" {
		t.Fatalf("a passed diff must be applied to the caller's tree: %q", got)
	}
	out, err := gitCmd(h.root, nil, nil, "worktree", "list")
	if err != nil || strings.Count(out, "\n") != 1 {
		t.Fatalf("worktree not cleaned up (%v):\n%s", err, out)
	}
	if ss.DiffFile == "" {
		t.Fatal("a delegate step must keep its full diff on disk")
	}
	diff, err := os.ReadFile(filepath.Join(r.dir, ss.DiffFile))
	if err != nil || !strings.Contains(string(diff), "done.txt") {
		t.Fatalf("diff file: %v %q", err, truncate(string(diff), 80))
	}
	if !strings.Contains(stepByName(st, "show").Out, "done.txt") {
		t.Fatalf("${steps.build.diff} must reach the next step: %q", stepByName(st, "show").Out)
	}
	_, tasks := readTrace(t, h)
	if len(tasks) != 1 {
		t.Fatalf("a delegate step still writes its TaskRecord, got %d", len(tasks))
	}
	steps := readSteps(t, h)
	if len(steps) != 2 || steps[0].Step != "build" || steps[0].Kind != stepDelegate {
		t.Fatalf("step records: %+v", steps)
	}
}

// Only "passed" applied a diff, so only "passed" moved the pipeline forward.
func TestWorkflowDelegateNotAppliedIsFailed(t *testing.T) {
	t.Run("check fails", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "nothing to do"} })
		h, dir := wfHarness(t, fs)
		code, st, _ := runWF(t, h, dir, "steps:\n  build:\n    delegate: create done.txt\n    role: coder\n", nil)
		ss := stepByName(st, "build")
		if code != 1 || ss.Status != stepFailed || !strings.Contains(ss.Detail, "delegate status failed") {
			t.Fatalf("exit %d step %+v", code, ss)
		}
	})
	t.Run("subagent errors", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			if req.Model == "coder-a" || req.Model == "coder-b" {
				return fakeReply{status: 500, content: "boom"}
			}
			return fakeReply{content: "ok"}
		})
		h, dir := wfHarness(t, fs)
		code, st, _ := runWF(t, h, dir, "steps:\n  build:\n    delegate: create done.txt\n    role: coder\n", nil)
		ss := stepByName(st, "build")
		if code != 1 || ss.Status != stepFailed || strings.Contains(ss.Detail, "passed") {
			t.Fatalf("exit %d step %+v", code, ss)
		}
	})
}

// Both the action and the check must pass: a check that overrode the action's
// exit would pass a broken build whose output file still says ok.
func TestWorkflowRunStepCheckMustAlsoPass(t *testing.T) {
	t.Run("check fails", func(t *testing.T) {
		h, dir := wfHarness(t, alwaysReply(t, "ok"))
		code, st, _ := runWF(t, h, dir, "steps:\n  a:\n    run: echo hi\n    check: test -f nope\n", nil)
		ss := stepByName(st, "a")
		if code != 1 || ss.Status != stepFailed || ss.Exit != 0 || !ss.Checked {
			t.Fatalf("exit %d step %+v", code, ss)
		}
		steps := readSteps(t, h)
		if len(steps) != 1 || steps[0].CheckExit == nil || *steps[0].CheckExit != 1 {
			t.Fatalf("trace must record the check's exit: %+v", steps)
		}
	})
	t.Run("action fails", func(t *testing.T) {
		h, dir := wfHarness(t, alwaysReply(t, "ok"))
		code, st, _ := runWF(t, h, dir, "steps:\n  a:\n    run: test -f nope\n    check: echo fine\n", nil)
		ss := stepByName(st, "a")
		if code != 1 || ss.Status != stepFailed || ss.Exit == 0 {
			t.Fatalf("exit %d step %+v", code, ss)
		}
	})
}

func TestWorkflowOnFailStopHalts(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	code, st, _ := runWF(t, h, dir, `steps:
  one:
    run: echo one
  two:
    run: test -f nope
  three:
    run: echo three
`, nil)
	if code != 1 || st.Status != stepFailed || st.Cursor != 1 {
		t.Fatalf("exit %d status %s cursor %d", code, st.Status, st.Cursor)
	}
	if stepByName(st, "three") != nil {
		t.Fatal("a stopped run must not record steps after the failure")
	}
}

func TestWorkflowOnFailContinueAndWhen(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	code, st, _ := runWF(t, h, dir, `steps:
  one:
    run: test -f nope
    on_fail: continue
  good:
    run: echo good
    when: ${steps.one.status} == ok
  bad:
    run: echo bad
    when: ${steps.one.status} == failed
`, nil)
	if code != 1 {
		t.Fatalf("a failure under on_fail: continue still fails the run, got %d", code)
	}
	if st.Cursor != 3 || st.Status != stepFailed {
		t.Fatalf("cursor %d status %s", st.Cursor, st.Status)
	}
	if got := stepByName(st, "good"); got.Status != stepSkipped || got.DurationMs != 0 {
		t.Fatalf("good: %+v", got)
	}
	if got := stepByName(st, "bad"); got.Status != stepOK {
		t.Fatalf("bad: %+v", got)
	}
	var skipped bool
	for _, rec := range readSteps(t, h) {
		if rec.Step == "good" && rec.Status == stepSkipped {
			skipped = true
		}
	}
	if !skipped {
		t.Fatal("a skipped step must be in the trace: otherwise nothing answers why it never ran")
	}
}

func TestWorkflowWhenExpressions(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	r := buildWF(t, h, dir, "steps:\n  z:\n    run: echo z\n", nil)
	defer r.Close()
	r.st.Vars = map[string]string{"mode": "ship"}
	r.res["a"] = &StepState{Name: "a", Kind: stepRun, Status: stepOK, Exit: 0, Out: "ship it"}
	r.res["b"] = &StepState{Name: "b", Kind: stepRun, Status: stepFailed, Exit: 1}

	for _, c := range []struct {
		expr string
		want bool
	}{
		{"always", true},
		{"${steps.a.status} == ok", true},
		{"${steps.a.status} == failed", false},
		{"${steps.a.status} != failed", true},
		{"steps.a.status == ok", true},
		{"ok == ${steps.a.status}", true},
		{"${steps.a.status} == ok && ${steps.b.status} == failed", true},
		{"${steps.a.status} == ok && ${steps.b.status} == ok", false},
		{"${steps.b.status} == ok || ${steps.a.status} == ok", true},
		{"${steps.b.status} == ok || ${steps.a.status} == failed", false},
		{"${steps.a.status} == failed && ${steps.b.status} == ok || always", true},
		{"${vars.mode} == ship", true},
		{"${steps.a.out} != blocked", true},
		{`${steps.a.out} == "ship it"`, true},
	} {
		when, err := parseWhen(c.expr)
		if err != nil {
			t.Fatalf("parseWhen(%q): %v", c.expr, err)
		}
		got, err := r.ready(&WorkflowStep{Name: "x", When: when})
		if err != nil {
			t.Fatalf("ready(%q): %v", c.expr, err)
		}
		if got != c.want {
			t.Fatalf("%q = %v, want %v", c.expr, got, c.want)
		}
	}
	// An unresolvable reference is a load error, so a typo can never quietly skip.
	if _, err := parseWorkflow("t", "t.yaml", "steps:\n  a:\n    run: echo x\n  b:\n    run: echo y\n    when: steps.typo.status == ok\n"); err == nil {
		t.Fatal("a when naming an unknown step must fail at load")
	}
}

// ── resume, pause, interrupt ────────────────────────────────────────────────

func TestWorkflowResumeSkipsFinishedSteps(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	text := `steps:
  one:
    run: sh -c 'echo one >> counter.txt'
  two:
    run: test -f gate.txt
  three:
    run: echo three
`
	code, st, r := runWF(t, h, dir, text, nil)
	if code != 1 || st.Cursor != 1 || st.Status != stepFailed {
		t.Fatalf("first run: exit %d cursor %d status %s", code, st.Cursor, st.Status)
	}
	r.Close()

	if err := os.WriteFile(filepath.Join(h.root, "gate.txt"), []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wf := loadWF(t, h, dir, text, nil)
	r2 := resumeRunner(t, h, wf, r.dir, nil)
	code = runRunner(t, r2)
	r2.Close()
	if code != 0 || r2.st.Status != stepOK || r2.st.Cursor != 3 {
		t.Fatalf("resume: exit %d status %s cursor %d", code, r2.st.Status, r2.st.Cursor)
	}
	counter, err := os.ReadFile(filepath.Join(h.root, "counter.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(counter), "\n"); n != 1 {
		t.Fatalf("a finished step must not run again: counter has %d lines", n)
	}
	log := readLog(t, r2)
	if got := strings.Count(log, "== 1/3"); got != 1 {
		t.Fatalf("step one ran %d times", got)
	}
	if got := strings.Count(log, "== 2/3"); got != 2 {
		t.Fatalf("step two ran %d times", got)
	}
	if len(r2.st.Sessions) != 2 || len(r2.st.Traces) != 2 {
		t.Fatalf("a resume records its own session and trace: %+v %+v", r2.st.Sessions, r2.st.Traces)
	}
}

func TestWorkflowResumeReusesEarlierStepOutputs(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	text := `steps:
  one:
    run: echo MARKER
  two:
    run: test -f gate.txt
  three:
    run: echo ${steps.one.out}
`
	_, _, r := runWF(t, h, dir, text, nil)
	r.Close()
	if err := os.WriteFile(filepath.Join(h.root, "gate.txt"), []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wf := loadWF(t, h, dir, text, nil)
	r2 := resumeRunner(t, h, wf, r.dir, nil)
	code := runRunner(t, r2)
	r2.Close()
	if code != 0 {
		t.Fatalf("resume exit %d", code)
	}
	if got := stepByName(r2.st, "three").Out; got != "MARKER" {
		t.Fatalf("step three read %q: a resumed run must reuse recorded outputs", got)
	}
	if got := strings.Count(readLog(t, r2), "== 1/3"); got != 1 {
		t.Fatalf("step one ran %d times", got)
	}
}

func TestWorkflowResumeRefusesChangedWorkflow(t *testing.T) {
	fs := alwaysReply(t, "ok")
	h, _ := wfHarness(t, fs)
	wfdir := t.TempDir()
	t.Setenv("LCA_WORKFLOWS", wfdir)
	path := filepath.Join(wfdir, "chg.yaml")
	text := "vars:\n  who: world\nsteps:\n  one:\n    run: echo ${vars.who}\n  two:\n    run: test -f nope\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runCLI(t, h, "chg"); code != 1 {
		t.Fatalf("first run exit %d", code)
	}
	runs := listRuns(h.orch.cfg, 0)
	if len(runs) != 1 {
		t.Fatalf("want one run, got %d", len(runs))
	}
	statePath := filepath.Join(runsDir(h.orch.cfg), runs[0].Run, "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(text+"  three:\n    run: echo three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := ""
	code := 0
	out = captureStdout(t, func() { code = runWorkflow(h.orch.cfg, []string{"chg", "-resume"}) })
	if code != 2 || !strings.Contains(out, "changed since this run started") {
		t.Fatalf("exit %d output:\n%s", code, out)
	}
	after, _ := os.ReadFile(statePath)
	if !bytes.Equal(before, after) {
		t.Fatal("a refused resume must leave state.json untouched")
	}

	// A -var that contradicts the recorded value is refused for the same reason.
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() { code = runWorkflow(h.orch.cfg, []string{"chg", "-resume", "-var", "who=elsewhere"}) })
	if code != 2 || !strings.Contains(out, "contradicts") {
		t.Fatalf("exit %d output:\n%s", code, out)
	}
	if n := len(fs.reqs()); n != 0 {
		t.Fatalf("a shell-only workflow must make no request, got %d", n)
	}
}

// runCLI drives runWorkflow the way a shell would, with its output captured.
func runCLI(t *testing.T, h *harness, args ...string) int {
	t.Helper()
	code := 0
	captureStdout(t, func() { code = runWorkflow(h.orch.cfg, args) })
	return code
}

func TestWorkflowPauseFile(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	// The run id is derived from the recorder, so the pause file's path is known
	// before the run starts and step one can create it.
	expect := filepath.Join(runsDir(h.orch.cfg), newRunID("wf", h.orch.rec.id))
	text := fmt.Sprintf("steps:\n  one:\n    run: sh -c 'echo now > %s/pause'\n  two:\n    run: echo two\n", expect)
	r := buildWF(t, h, dir, text, nil)
	if r.dir != expect {
		t.Fatalf("run dir %s, want %s", r.dir, expect)
	}
	code := runRunner(t, r)
	r.Close()
	if code != 0 || r.st.Status != "paused" || r.st.Cursor != 1 {
		t.Fatalf("exit %d status %s cursor %d", code, r.st.Status, r.st.Cursor)
	}
	if stepByName(r.st, "two") != nil {
		t.Fatal("a paused run must not record the step it stopped before")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "pause")); err != nil {
		t.Fatal("the pause file must still be there until a resume clears it")
	}

	wf := loadWF(t, h, dir, text, nil)
	r2 := resumeRunner(t, h, wf, r.dir, nil)
	code = runRunner(t, r2)
	r2.Close()
	if code != 0 || r2.st.Status != stepOK || r2.st.Cursor != 2 {
		t.Fatalf("resume: exit %d status %s cursor %d", code, r2.st.Status, r2.st.Cursor)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "pause")); err == nil {
		t.Fatal("a resume must clear the pause file")
	}
	// pauseRun writes the same file from another terminal.
	if err := pauseRun(h.orch.cfg, r2.st.Run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "pause")); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowStopAtStepBoundary(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	text := "steps:\n  one:\n    run: sh -c 'sleep 1'\n  two:\n    run: echo two\n"
	r := buildWF(t, h, dir, text, nil)
	go func() {
		time.Sleep(200 * time.Millisecond)
		r.stop.Store(true) // the first Ctrl-C
	}()
	code := runRunner(t, r)
	r.Close()
	if code != 1 || r.st.Status != "interrupted" || r.st.Cursor != 1 {
		t.Fatalf("exit %d status %s cursor %d", code, r.st.Status, r.st.Cursor)
	}
	if got := stepByName(r.st, "one"); got == nil || got.Status != stepOK {
		t.Fatalf("the step in flight must finish: %+v", got)
	}
	if stepByName(r.st, "two") != nil {
		t.Fatal("the next step must not have started")
	}
	wf := loadWF(t, h, dir, text, nil)
	r2 := resumeRunner(t, h, wf, r.dir, nil)
	if code := runRunner(t, r2); code != 0 || r2.st.Status != stepOK {
		t.Fatalf("resume: exit %d status %s", code, r2.st.Status)
	}
	r2.Close()
}

func TestWorkflowCancelRecordsCancelledStep(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	text := "steps:\n  slow:\n    run: sh -c 'sleep 2'\n  after:\n    run: echo after\n"
	r := buildWF(t, h, dir, text, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel() // the second Ctrl-C
	}()
	code := 0
	captureStdout(t, func() { code = r.Run(ctx) })
	cancel()
	r.Close()
	ss := stepByName(r.st, "slow")
	if code != 1 || r.st.Status != "interrupted" || r.st.Cursor != 0 {
		t.Fatalf("exit %d status %s cursor %d", code, r.st.Status, r.st.Cursor)
	}
	if ss == nil || ss.Status != stepFailed || !strings.Contains(ss.Detail, "cancelled") {
		t.Fatalf("a cancelled step is recorded, not lost: %+v", ss)
	}
	if stepByName(r.st, "after") != nil {
		t.Fatal("nothing after a cancelled step may run")
	}
	wf := loadWF(t, h, dir, text, nil)
	r2 := resumeRunner(t, h, wf, r.dir, nil)
	if code := runRunner(t, r2); code != 0 || r2.st.Status != stepOK {
		t.Fatalf("resume: exit %d status %s (%+v)", code, r2.st.Status, r2.st.Steps)
	}
	r2.Close()
}

// ── state and trace ─────────────────────────────────────────────────────────

func TestWorkflowStateIsAtomicAndComplete(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	expect := filepath.Join(runsDir(h.orch.cfg), newRunID("wf", h.orch.rec.id))
	text := fmt.Sprintf("steps:\n  first:\n    run: echo first\n  peek:\n    run: cat %s/state.json\n", expect)
	r := buildWF(t, h, dir, text, nil)
	if r.dir != expect {
		t.Fatalf("run dir %s, want %s", r.dir, expect)
	}
	code := runRunner(t, r)
	r.Close()
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	peek := stepByName(r.st, "peek").Out
	for _, want := range []string{`"name": "first"`, `"status": "ok"`} {
		if !strings.Contains(peek, want) {
			t.Fatalf("state.json mid-run lacks %s:\n%s", want, peek)
		}
	}
	if _, err := os.Stat(filepath.Join(r.dir, "state.json.tmp")); err == nil {
		t.Fatal("the temp file must not survive the run")
	}
	if _, err := loadRunState(r.dir); err != nil {
		t.Fatalf("final state must parse: %v", err)
	}
}

func TestWorkflowTraceStepRecords(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	code, _, _ := runWF(t, h, dir, `steps:
  ask:
    prompt: say hi
    role: cheap
  shell:
    run: echo hi
  never:
    run: echo no
    when: ${steps.shell.status} == failed
`, nil)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	steps := readSteps(t, h)
	if len(steps) != 3 {
		t.Fatalf("one record per executed or skipped step, got %d: %+v", len(steps), steps)
	}
	byName := map[string]StepRecord{}
	for _, s := range steps {
		if s.Run == "" || s.Workflow != "wf" || s.Step == "" || s.Status == "" || s.RootSession != h.sess.UID {
			t.Fatalf("incomplete step record: %+v", s)
		}
		byName[s.Step] = s
	}
	if r := byName["ask"]; r.Kind != stepPrompt || r.Role != "cheap" || r.Model != "cheap-a" || r.Usage == nil || r.DurationMs < 0 {
		t.Fatalf("prompt record: %+v", r)
	}
	if r := byName["shell"]; r.Kind != stepRun || r.Role != "" || r.Model != "" || r.Usage != nil {
		t.Fatalf("a shell step costs no tokens: %+v", r)
	}
	if r := byName["never"]; r.Status != stepSkipped || r.DurationMs != 0 || r.Index != 2 {
		t.Fatalf("skipped record: %+v", r)
	}
	// A new record type is additive: eval must still score the run.
	var res EvalResult
	scoreTrace(h.orch.tracer.Path, h.sess.UID, &res)
	if res.Turns == 0 {
		t.Fatalf("scoreTrace stopped counting turns: %+v", res)
	}
}

func TestWorkflowRunLogSurvivesFailure(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	text := "steps:\n  boom:\n    run: sh -c 'echo before; exit 3'\n"
	code, st, r := runWF(t, h, dir, text, nil)
	r.Close()
	ss := stepByName(st, "boom")
	if code != 1 || ss.Status != stepFailed || ss.Exit != 3 {
		t.Fatalf("exit %d step %+v", code, ss)
	}
	if !strings.Contains(readLog(t, r), "before") {
		t.Fatalf("output produced before the failure must be in run.log:\n%s", readLog(t, r))
	}
	wf := loadWF(t, h, dir, text, nil)
	r2 := resumeRunner(t, h, wf, r.dir, nil)
	runRunner(t, r2)
	r2.Close()
	log := readLog(t, r2)
	at := strings.Index(log, "== resume")
	if at <= 0 {
		t.Fatalf("a resumed run's log must be a continuation:\n%s", log)
	}
	if !strings.Contains(log[:at], "before") {
		t.Fatalf("the first run's output was truncated:\n%s", log[:at])
	}
	if !strings.Contains(log[at:], "before") {
		t.Fatalf("the resumed step's output was not appended:\n%s", log[at:])
	}
}

func TestWorkflowSandboxRefusalIsFailedStep(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	code, st, r := runWF(t, h, dir, "steps:\n  nope:\n    run: curl http://example.invalid\n", nil)
	ss := stepByName(st, "nope")
	if code != 1 || ss.Status != stepFailed || ss.Exit != -1 {
		t.Fatalf("exit %d step %+v", code, ss)
	}
	if !strings.Contains(ss.Detail, "allowlist") {
		t.Fatalf("the refusal must be the step's detail: %q", ss.Detail)
	}
	if !strings.Contains(readLog(t, r), "allowlist") {
		t.Fatalf("a refusal must be in run.log too:\n%s", readLog(t, r))
	}
}

// ── discovery and CLI ───────────────────────────────────────────────────────

func TestWorkflowDiscoveryPrecedence(t *testing.T) {
	h, _ := wfHarness(t, alwaysReply(t, "ok"))
	extra := t.TempDir()
	t.Setenv("LCA_WORKFLOWS", extra)
	write := func(dir, name, body string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	project := filepath.Join(h.root, ".lca", "workflows")
	personal := filepath.Join(h.orch.cfg.Dir, "workflows")
	write(project, "x.yaml", "description: project\nsteps:\n  a:\n    run: echo p\n")
	write(personal, "x.yaml", "description: personal\nsteps:\n  a:\n    run: echo h\n")
	write(extra, "x.yaml", "description: extra\nsteps:\n  a:\n    run: echo e\n")
	write(extra, "only.yaml", "description: only here\nsteps:\n  a:\n    run: echo o\n")
	write(extra, "broken.yaml", "steps:\n  a:\n    run: echo x\n    nonsense: 1\n")

	wf, err := findWorkflow(h.orch.cfg, "x")
	if err != nil {
		t.Fatal(err)
	}
	if wf.Desc != "project" {
		t.Fatalf("the repo's workflow must win: %q from %s", wf.Desc, wf.Path)
	}
	found, warns := listWorkflows(h.orch.cfg)
	var names []string
	for _, w := range found {
		names = append(names, w.Name+"="+w.Desc)
	}
	if strings.Join(names, ",") != "only=only here,x=project" {
		t.Fatalf("listing: %v", names)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "broken.yaml") {
		t.Fatalf("a bad file is a warning, not a failure: %v", warns)
	}
}

func TestRunWorkflowFlagParsing(t *testing.T) {
	fs := alwaysReply(t, "ok")
	h, _ := wfHarness(t, fs)
	wfdir := t.TempDir()
	t.Setenv("LCA_WORKFLOWS", wfdir)
	if err := os.WriteFile(filepath.Join(wfdir, "flagwf.yaml"),
		[]byte("vars:\n  k:\nsteps:\n  a:\n    run: echo ${vars.k}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := runCLI(t, h, "flagwf", "-dry-run", "-var", "k=v"); code != 0 {
		t.Fatalf("-dry-run exit %d", code)
	}
	if runs := listRuns(h.orch.cfg, 0); len(runs) != 0 {
		t.Fatalf("-dry-run must create no run directory, got %d", len(runs))
	}
	if n := len(fs.reqs()); n != 0 {
		t.Fatalf("-dry-run must make no request, got %d", n)
	}

	for _, args := range [][]string{{"flagwf", "-var", "k=one"}, {"-var", "k=two", "flagwf"}} {
		if code := runCLI(t, h, args...); code != 0 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
	runs := listRuns(h.orch.cfg, 0)
	if len(runs) != 2 {
		t.Fatalf("want two runs, got %d", len(runs))
	}
	seen := map[string]bool{}
	for _, st := range runs {
		seen[st.Vars["k"]] = true
	}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("both flag orders must bind -var: %v", seen)
	}

	out := captureStdout(t, func() {
		if code := runWorkflow(h.orch.cfg, nil); code != 0 {
			t.Errorf("listing exit %d", code)
		}
	})
	if !strings.Contains(out, "flagwf") {
		t.Fatalf("`lca run` with no name lists the workflows:\n%s", out)
	}
	if code := runCLI(t, h, "ghost"); code != 2 {
		t.Fatalf("unknown workflow exit %d", code)
	}
	if code := runCLI(t, h, "flagwf", "extra", "-var", "k=v"); code != 2 {
		t.Fatalf("two bare arguments exit %d", code)
	}
	if !reRunID.MatchString("harden-20260929-141502-8231") || reRunID.MatchString("harden") {
		t.Fatal("reRunID must tell a run id from a workflow name")
	}
	if !reRunID.MatchString(runs[0].Run) {
		t.Fatalf("newRunID output %q must match reRunID", runs[0].Run)
	}
}

func TestWorkflowExitStatus(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	for _, c := range []struct {
		name, yaml string
		want       int
	}{
		{"all ok", "steps:\n  a:\n    run: echo a\n  b:\n    run: echo b\n", 0},
		{"one skipped", "steps:\n  a:\n    run: echo a\n  b:\n    run: echo b\n    when: ${steps.a.status} == failed\n", 0},
		{"failed with continue", "steps:\n  a:\n    run: test -f nope\n    on_fail: continue\n  b:\n    run: echo b\n", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, _, r := runWF(t, h, filepath.Join(dir, c.name), c.yaml, nil)
			r.Close()
			if code != c.want {
				t.Fatalf("exit %d, want %d", code, c.want)
			}
		})
	}
	// A clean pause with no failure is not a failure.
	t.Run("paused", func(t *testing.T) {
		sub := filepath.Join(dir, "paused")
		r := buildWF(t, h, sub, "steps:\n  a:\n    run: echo a\n", nil)
		if err := pauseRun(h.orch.cfg, r.st.Run); err != nil {
			t.Fatal(err)
		}
		code := runRunner(t, r)
		r.Close()
		if code != 0 || r.st.Status != "paused" {
			t.Fatalf("exit %d status %s", code, r.st.Status)
		}
	})
	// A load error is a usage error, not a run.
	t.Run("load error", func(t *testing.T) {
		wfdir := t.TempDir()
		t.Setenv("LCA_WORKFLOWS", wfdir)
		if err := os.WriteFile(filepath.Join(wfdir, "bad.yaml"), []byte("steps:\n  a:\n    run: echo x\n    nonsense: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := runCLI(t, h, "bad"); code != 2 {
			t.Fatalf("exit %d", code)
		}
	})
}

func TestWorkflowPruneKeepsUnfinishedRuns(t *testing.T) {
	h, dir := wfHarness(t, alwaysReply(t, "ok"))
	_, _, okRun := runWF(t, h, filepath.Join(dir, "a"), "steps:\n  a:\n    run: echo a\n", nil)
	okRun.Close()
	_, _, badRun := runWF(t, h, filepath.Join(dir, "b"), "steps:\n  a:\n    run: test -f nope\n", nil)
	badRun.Close()
	if len(listRuns(h.orch.cfg, 0)) != 2 {
		t.Fatalf("want two runs: %+v", listRuns(h.orch.cfg, 0))
	}
	pruneRuns(h.orch.cfg, 0)
	left := listRuns(h.orch.cfg, 0)
	if len(left) != 1 || left[0].Status != stepFailed {
		t.Fatalf("an unfinished run is someone's resumable work: %+v", left)
	}
}
