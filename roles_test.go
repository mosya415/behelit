package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRoles = `entry: lead
transport: native
defaults:
  context: 64000
  verify_attempts: 2
  check_timeout: 30
sandbox:
  allow: [ls, cat, echo, git, test, grep, sh, go]
roles:
  lead:
    description: Plans and delegates.
    models: [lead-a, lead-b]
    effort: high
    tools: [read_file, grep, glob, list_dir, delegate, todowrite]
    prompt: |
      You are the LEAD.
      # not a comment: part of the prompt
  coder:
    description: Makes code changes.
    models:
      - coder-a
      - coder-b
    effort: medium
    tools: [read_file, write, edit, run_command, list_dir]
    context: 32000
    check_cmd: ls done.txt
  cheap:
    models: [cheap-a]
    effort: off
    tools: []
    context: 16000
`

// newRoleHarness sets up a git repo root with roles.yaml and an orchestrator
// through the real setup path (gateway model validation included).
func newRoleHarness(t *testing.T, fs *fakeServer, roles string, approve bool) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LCA_ROLES", "")
	t.Setenv("LCA_GW_MAX_WAIT", "5")
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	os.MkdirAll(filepath.Join(root, ".lca"), 0o755)
	os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"), []byte(roles), 0o644)
	os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".lca/\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if _, err := gitCmd(root, nil, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Root: root, Dir: filepath.Join(home, ".lca"), BaseURL: fs.URL, Endpoints: []string{fs.URL}, Model: "unused",
		Temperature: 0.2, MaxSteps: 20, Allowed: []string{"echo"}, SubagentMax: 1, KeepSessions: 10}
	ap := NewApprover(newStringInput(""))
	if approve {
		ap.TrustAll()
	}
	orch, err := setupOrchestrator(cfg, ap, filepath.Join(home, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { orch.tracer.Close(); orch.rec.Close() })
	view := &quietView{}
	sess, err := orch.NewPrimary(orch.roles.Entry, "", view)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{orch: orch, sess: sess, view: view, root: root}
}

func readTrace(t *testing.T, h *harness) (turns []TurnRecord, tasks []TaskRecord) {
	t.Helper()
	data, err := os.ReadFile(h.orch.tracer.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var head struct{ Type string }
		json.Unmarshal(line, &head)
		switch head.Type {
		case "turn":
			var r TurnRecord
			json.Unmarshal(line, &r)
			turns = append(turns, r)
		case "task":
			var r TaskRecord
			json.Unmarshal(line, &r)
			tasks = append(tasks, r)
		}
	}
	return
}

func allModels() []string { return []string{"lead-a", "lead-b", "coder-a", "coder-b", "cheap-a"} }

func TestRolesYAMLParse(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".lca", "prompts"), 0o755)
	os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"), []byte(testRoles), 0o644)
	t.Setenv("LCA_ROLES", "")
	rc, err := loadRoles(Config{Root: root, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*Agent{}
	for _, r := range rc.Roles {
		by[r.Name] = r
	}
	lead, coder, cheap := by["lead"], by["coder"], by["cheap"]
	if rc.Entry != "lead" || rc.Transport != "native" || rc.VerifyAttempts != 2 || rc.CheckTimeout != 30 {
		t.Fatalf("top level: %+v", rc)
	}
	if strings.Join(rc.Allow, ",") != "ls,cat,echo,git,test,grep,sh,go" {
		t.Fatalf("sandbox allow: %v", rc.Allow)
	}
	if strings.Join(lead.Models, ",") != "lead-a,lead-b" || lead.Thinking != "high" || lead.Context != 64000 {
		t.Fatalf("lead: %+v", lead)
	}
	if lead.Prompt != "You are the LEAD.\n# not a comment: part of the prompt" {
		t.Fatalf("block prompt: %q", lead.Prompt)
	}
	if strings.Join(coder.Models, ",") != "coder-a,coder-b" || coder.Context != 32000 || coder.CheckCmd != "ls done.txt" {
		t.Fatalf("coder: %+v", coder)
	}
	if !cheap.ToolsSet || len(cheap.Tools) != 0 || cheap.Thinking != "off" {
		t.Fatalf("cheap: %+v", cheap)
	}

	bad := strings.Replace(testRoles, "[cheap-a]", "[http://10.0.0.5:8000/v1]", 1)
	os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"), []byte(bad), 0o644)
	if _, err := loadRoles(Config{Root: root, Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("an address as a model must be rejected, got %v", err)
	}
	bad = strings.Replace(testRoles, "todowrite]", "nosuchtool]", 1)
	os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"), []byte(bad), 0o644)
	if _, err := loadRoles(Config{Root: root, Dir: t.TempDir()}); err == nil {
		t.Fatal("unknown tool must be rejected")
	}
}

func TestRoleModelsValidatedAgainstGateway(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = []string{"lead-b", "coder-a", "coder-b", "cheap-a"} // lead-a not served
	h := newRoleHarness(t, fs, testRoles, false)
	if got := strings.Join(h.orch.agents["lead"].Models, ","); got != "lead-b" {
		t.Fatalf("unserved model should be dropped from the chain, got %s", got)
	}
	if h.sess.client.Model() != "lead-b" {
		t.Fatalf("primary on %s", h.sess.client.Model())
	}
}

func TestRoleToolSetIsExactAndPrefixStable(t *testing.T) {
	var mu sync.Mutex
	var systems, tools []string
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		mu.Lock()
		systems = append(systems, req.system())
		b, _ := json.Marshal(req.Tools)
		tools = append(tools, string(b))
		mu.Unlock()
		switch n {
		case 1:
			return fakeReply{status: 503, content: "model is drained", header: "X-Berserk-State: drained", header2: "Retry-After: 5"}
		case 2:
			return fakeReply{calls: []ToolCall{call("l", "list_dir", map[string]any{"path": "."})}}
		}
		return fakeReply{content: "done"}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, false)
	if err := h.run(t, "look around"); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range fs.reqs()[1].Tools {
		names = append(names, tl.Function.Name)
	}
	if strings.Join(names, ",") != "list_dir,glob,grep,read_file,todowrite,delegate" {
		t.Fatalf("lead's tool set: %v", names)
	}
	for i := 1; i < len(systems); i++ {
		if systems[i] != systems[0] || tools[i] != tools[0] {
			t.Fatalf("prefix changed between request 1 and %d (fallback must not touch system/tools)", i+1)
		}
	}
	if !strings.HasPrefix(systems[0], "You are the LEAD.") || strings.Contains(systems[0], "lead-a") {
		t.Fatal("system prompt must be the role prompt and must not name the model")
	}
}

func TestGatewayNotUpFallsBackAndSticks(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "lead-a" {
			return fakeReply{status: 503, content: "cold start failed: no GPUs", header: "Retry-After: 5"}
		}
		if n <= 2 {
			return fakeReply{calls: []ToolCall{call("l", "list_dir", map[string]any{})}}
		}
		return fakeReply{content: "done"}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, false)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, r := range fs.reqs() {
		models = append(models, r.Model)
	}
	if strings.Join(models, ",") != "lead-a,lead-b,lead-b" {
		t.Fatalf("want one miss on lead-a then lead-b sticking, got %v", models)
	}
	turns, _ := readTrace(t, h)
	if len(turns) == 0 || len(turns[0].Fallbacks) != 1 || turns[0].Fallbacks[0].Reason != "not up" || turns[0].Model != "lead-b" {
		t.Fatalf("trace fallback: %+v", turns)
	}
}

func TestGatewayOverloadedWaitsWithoutFallback(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n <= 2 {
			return fakeReply{status: 503, content: "service overloaded; retry shortly", header: "X-Berserk-Overload: 1", header2: "Retry-After-Ms: 20"}
		}
		return fakeReply{content: "done"}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, false)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	for _, r := range fs.reqs() {
		if r.Model != "lead-a" {
			t.Fatalf("overload must not fall back, saw %s", r.Model)
		}
	}
	if len(fs.reqs()) != 3 {
		t.Fatalf("want 2 waits then success, got %d requests", len(fs.reqs()))
	}
}

func TestGatewayBeforeFirstByteOneAttemptOnNext(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{hangup: true} })
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, false)
	err := h.run(t, "go")
	if err == nil {
		t.Fatal("expected an error after the single fallback attempt")
	}
	var models []string
	for _, r := range fs.reqs() {
		models = append(models, r.Model)
	}
	if strings.Join(models, ",") != "lead-a,lead-b" {
		t.Fatalf("want exactly one attempt on the next model, got %v", models)
	}
}

func TestGatewayAfterFirstByteRepeatsTurnOnSameModel(t *testing.T) {
	// Always cut: the turn is repeated LCA_GW_CUT_RETRIES (2) times on the same
	// model, then the error surfaces.
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{cut: true} })
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, false)
	if err := h.run(t, "go"); err == nil {
		t.Fatal("a stream that keeps getting cut must eventually surface as an error")
	}
	var models []string
	for _, r := range fs.reqs() {
		models = append(models, r.Model)
	}
	if strings.Join(models, ",") != "lead-a,lead-a,lead-a" {
		t.Fatalf("want 1 + 2 repeats on the same model, got %v", models)
	}
}

func TestCutTurnRepeatDoesNotRunToolsTwice(t *testing.T) {
	var lists int
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		switch n {
		case 1:
			return fakeReply{cut: true} // cut mid-reply: nothing may execute
		case 2:
			return fakeReply{calls: []ToolCall{call("l", "list_dir", map[string]any{"path": "."})}}
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			lists++
		}
		return fakeReply{content: "done"}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, false)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	var results int
	for _, m := range h.sess.Msgs {
		if m.Role == "tool" {
			results++
		}
	}
	if results != 1 || lists != 1 {
		t.Fatalf("the repeated turn must execute its tool exactly once, got %d results", results)
	}
	turns, _ := readTrace(t, h)
	if len(turns[0].Fallbacks) != 1 || turns[0].Fallbacks[0].Reason != "cut after first byte" || turns[0].Fallbacks[0].To != "lead-a" {
		t.Fatalf("trace should record the repeat: %+v", turns[0].Fallbacks)
	}
}

func TestPerModelTransport(t *testing.T) {
	roles := strings.Replace(testRoles, "entry: lead\ntransport: native\n", "entry: lead\ntransport: native\nmodels:\n  coder-a: {transport: text}\n", 1)
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	h := newRoleHarness(t, fs, roles, false)
	if !h.sess.client.Native() {
		t.Fatal("lead should be native")
	}
	child, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "x")
	if err != nil {
		t.Fatal(err)
	}
	if child.client.Native() || !strings.Contains(child.Msgs[0].Content, "<read_file path=") {
		t.Fatal("coder-a is configured for the text protocol")
	}
	child.useModel(1) // fallback to coder-b (native by default) keeps the session's transport
	if child.client.Native() {
		t.Fatal("a fallback must not switch a session's transport")
	}
	found := false
	for _, w := range h.orch.warnings {
		if strings.Contains(w, "mixes tool transports") {
			found = true
		}
	}
	if !found {
		t.Fatalf("mixing transports in a chain should warn: %v", h.orch.warnings)
	}
}

func TestSessionHeaders(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "made it"}
			}
			return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "done.txt", "content": "ok\n"})}}
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "lead done"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "coder", "task": "create done.txt"})}}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, true)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	var leadSID, coderSID, roots = "", "", map[string]bool{}
	for _, r := range fs.reqs() {
		sid, root := r.Header.Get("X-Session-Id"), r.Header.Get("X-Root-Session-Id")
		if sid == "" || root == "" {
			t.Fatalf("missing session headers on %s request", r.Model)
		}
		roots[root] = true
		if r.Model == "coder-a" {
			coderSID = sid
		} else {
			leadSID = sid
		}
	}
	if len(roots) != 1 || leadSID == coderSID || !roots[leadSID] {
		t.Fatalf("lead %q coder %q roots %v: want distinct session ids under one root = the lead's", leadSID, coderSID, roots)
	}
}

func TestDelegateWorktreeVerifiedApplied(t *testing.T) {
	var coderRoots sync.Map
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			coderRoots.Store(req.system(), true)
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "created done.txt"}
			}
			return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "done.txt", "content": "ok\n"})}}
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "integrated"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "coder", "task": "create done.txt"})}}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, true)
	// uncommitted change in the caller's tree must be visible to the subagent snapshot
	os.WriteFile(filepath.Join(h.root, "wip.txt"), []byte("wip\n"), 0o644)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	var result delegateResult
	for _, m := range h.sess.Msgs {
		if m.Role == "tool" && m.Tool == "delegate" {
			if err := json.Unmarshal([]byte(m.Content), &result); err != nil {
				t.Fatalf("delegate must return JSON {status,diff,test_tail}: %v\n%s", err, m.Content)
			}
			var keys map[string]any
			json.Unmarshal([]byte(m.Content), &keys)
			if len(keys) != 3 {
				t.Fatalf("delegate returned extra fields: %v", keys)
			}
		}
	}
	if result.Status != "passed" || !strings.Contains(result.Diff, "done.txt") || !strings.Contains(result.TestTail, "done.txt") {
		t.Fatalf("delegate result: %+v", result)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "done.txt")); string(got) != "ok\n" {
		t.Fatalf("passed diff not applied to the caller's tree: %q", got)
	}
	if strings.Contains(result.Diff, "wip.txt") {
		t.Fatal("the snapshot's own uncommitted files must not show up in the diff")
	}
	// the subagent really ran somewhere else
	coderRoots.Range(func(k, _ any) bool {
		if strings.Contains(k.(string), "Working directory (jail): "+h.root+"\n") {
			t.Fatal("coder ran in the caller's tree, not a worktree")
		}
		return true
	})
	out, _ := exec.Command("git", "-C", h.root, "worktree", "list").Output()
	if strings.Count(string(out), "\n") != 1 {
		t.Fatalf("worktree not cleaned up:\n%s", out)
	}
	if st, _ := exec.Command("git", "-C", h.root, "diff", "--cached", "--name-only").Output(); len(st) != 0 {
		t.Fatalf("the user's index was touched: %s", st)
	}
	_, tasks := readTrace(t, h)
	if len(tasks) != 1 || tasks[0].Status != "passed" || !tasks[0].Applied || tasks[0].Role != "coder" || tasks[0].CheckExit == nil || *tasks[0].CheckExit != 0 {
		t.Fatalf("task trace: %+v", tasks)
	}
}

func TestDelegateVerifierDecidesNotTheModel(t *testing.T) {
	var feedback int
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			if strings.Contains(req.Body, "The verifier ran") {
				feedback++
			}
			return fakeReply{content: "All done, everything works!"} // claims success, changes nothing
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "ok"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "coder", "task": "create done.txt"})}}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, true)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	var result delegateResult
	for _, m := range h.sess.Msgs {
		if m.Tool == "delegate" {
			json.Unmarshal([]byte(m.Content), &result)
		}
	}
	if result.Status != "failed" || result.TestTail == "" {
		t.Fatalf("verifier must fail the task despite the model's claim: %+v", result)
	}
	if feedback != 1 {
		t.Fatalf("want the failure fed back once (verify_attempts: 2), got %d", feedback)
	}
	if _, err := os.Stat(filepath.Join(h.root, "done.txt")); err == nil {
		t.Fatal("nothing should have been applied")
	}
}

func TestCompactionUsesCheapRole(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if strings.Contains(req.system(), "context summarization agent") {
			return fakeReply{content: "## Objective\nsummary"}
		}
		return fakeReply{content: "ok"}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, false)
	for i := 0; i < 3; i++ {
		h.sess.Msgs = append(h.sess.Msgs, Message{Role: "user", Content: fmt.Sprint("q", i)}, Message{Role: "assistant", Content: "a"})
	}
	system := h.sess.Msgs[0].Content
	if err := h.sess.Compact(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	r := fs.reqs()[0]
	if r.Model != "cheap-a" {
		t.Fatalf("compaction ran on %s, want the cheap role", r.Model)
	}
	if r.Header.Get("X-Root-Session-Id") != h.sess.UID || r.Header.Get("X-Session-Id") == h.sess.UID {
		t.Fatal("compaction must be its own session under the same root")
	}
	if h.sess.Msgs[0].Content != system {
		t.Fatal("compaction changed the system prompt")
	}
	turns, _ := readTrace(t, h)
	if len(turns) != 1 || turns[0].Role != "cheap" {
		t.Fatalf("trace: %+v", turns)
	}
}

func TestTraceTurnRecord(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{calls: []ToolCall{call("g", "glob", map[string]any{"pattern": "*.md"}), call("r", "read_file", map[string]any{"path": "nope.txt"})}}
		}
		return fakeReply{content: "done"}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, false)
	h.run(t, "go")
	turns, _ := readTrace(t, h)
	if len(turns) != 2 {
		t.Fatalf("want 2 turn records, got %d", len(turns))
	}
	t0 := turns[0]
	if t0.Role != "lead" || t0.Model != "lead-a" || t0.Usage.PromptTokens != 100 || t0.RootSession != h.sess.UID || len(t0.ToolCalls) != 2 {
		t.Fatalf("turn record: %+v", t0)
	}
	if !t0.ToolCalls[0].OK || t0.ToolCalls[1].OK || t0.ToolCalls[1].Error == "" {
		t.Fatalf("tool outcomes: %+v", t0.ToolCalls)
	}
}

func TestSandboxGPUPolicy(t *testing.T) {
	j, _ := NewJail(t.TempDir(), []string{"bsk", "python3", "env", "srun"}, false)
	for cmd, ok := range map[string]bool{
		"bsk submit -g 2 -- python3 train.py":           true,
		"bsk ps":                                        true,
		"bsk gw drain qwen":                             false,
		"srun -G 1 python3 x.py":                        false,
		"env CUDA_VISIBLE_DEVICES=0 python3 x.py":       false,
		"python3 -m vllm.entrypoints.openai.api_server": false,
		"python3 script.py":                             true,
		"rm -rf /":                                      false, // not allowlisted
	} {
		if err := j.CheckCommand(cmd); (err == nil) != ok {
			t.Errorf("%q: allowed=%v, want %v (%v)", cmd, err == nil, ok, err)
		}
	}
	u, _ := NewJail(t.TempDir(), nil, true)
	for cmd, ok := range map[string]bool{
		"make test && CUDA_VISIBLE_DEVICES=1 python x.py": false,
		"ls | torchrun --nproc 8 train.py":                false,
		"go test ./... ; echo done":                       true,
	} {
		if err := u.CheckCommand(cmd); (err == nil) != ok {
			t.Errorf("unsafe %q: allowed=%v, want %v (%v)", cmd, err == nil, ok, err)
		}
	}
}

func TestEvalEndToEnd(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "fixed"}
		}
		return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "answer.txt", "content": "42\n"})}}
	})
	fs.models = allModels()
	home := t.TempDir()
	t.Setenv("HOME", home)
	rolesPath := filepath.Join(home, "roles.yaml")
	os.WriteFile(rolesPath, []byte(testRoles), 0o644)
	t.Setenv("LCA_ROLES", rolesPath)
	t.Setenv("LCA_GW_MAX_WAIT", "5")

	tasks := filepath.Join(t.TempDir(), "tasks")
	os.MkdirAll(filepath.Join(tasks, "answer", "fixture"), 0o755)
	os.WriteFile(filepath.Join(tasks, "answer", "fixture", "README"), []byte("puzzle\n"), 0o644)
	os.WriteFile(filepath.Join(tasks, "answer", "task.yaml"), []byte("prompt: |\n  Write the answer to answer.txt\ncheck_cmd: cat answer.txt\nrole: coder\nrepo: fixture\n"), 0o644)
	os.WriteFile(filepath.Join(tasks, "never.yaml"), []byte("prompt: Do nothing useful\ncheck_cmd: ls missing.txt\nrole: coder\nrepo: answer/fixture\nverify_attempts: 1\n"), 0o644)

	out := filepath.Join(t.TempDir(), "out")
	cfg := Config{Root: t.TempDir(), Dir: filepath.Join(home, ".lca"), BaseURL: fs.URL, Endpoints: []string{fs.URL}, Model: "x",
		Temperature: 0.2, MaxSteps: 10, SubagentMax: 1, KeepSessions: 10}
	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	code := runEval(cfg, []string{"-out", out, filepath.Join(tasks)})
	os.Stdout = stdout
	if code != 1 {
		t.Fatalf("one task passes and one fails → exit 1, got %d", code)
	}
	data, err := os.ReadFile(filepath.Join(out, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]EvalResult{}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var r EvalResult
		json.Unmarshal(line, &r)
		results[r.Task] = r
	}
	a, nv := results["answer"], results["never"]
	if a.Status != "passed" || a.Turns != 2 || a.PromptTokens != 200 || a.ToolCalls != 1 || len(a.Models) != 1 || a.Models[0] != "coder-a" {
		t.Fatalf("answer: %+v", a)
	}
	if nv.Status != "failed" || nv.CheckExit == nil || *nv.CheckExit == 0 {
		t.Fatalf("never: %+v", nv)
	}
	if _, err := os.Stat(filepath.Join(out, "answer", "work")); err == nil {
		t.Fatal("workspace should be removed without -keep")
	}
	_ = io.Discard
	_ = time.Second
}

// ── regressions from the second review ─────────────────────────────────────

func TestGPUPolicyBypasses(t *testing.T) {
	safe, _ := NewJail(t.TempDir(), []string{"bash", "nohup", "timeout", "env", "xargs", "python3", "bsk", "go"}, false)
	for cmd, ok := range map[string]bool{
		"bash -c 'srun x'": false,
		"nohup srun x":     false,
		"timeout 5 srun x": false,
		"env -i srun x":    false,
		"env -u X CUDA_VISIBLE_DEVICES=0 python3 x.py": false,
		"xargs srun":             false,
		"python3 -mvllm serve m": false,
		"python3 -m torch.distributed.run train.py":  false,
		"bsk -v gw drain m":                          false,
		"bsk submit -g 1 -- python3 -m vllm serve m": true,
		"bsk submit -- torchrun --nproc 8 train.py":  true,
		"python3 -m pytest -q":                       true,
		"timeout 60 go test ./...":                   true,
	} {
		if err := safe.CheckCommand(cmd); (err == nil) != ok {
			t.Errorf("safe %q: allowed=%v want %v (%v)", cmd, err == nil, ok, err)
		}
	}
	unsafe, _ := NewJail(t.TempDir(), nil, true)
	for cmd, ok := range map[string]bool{
		"sh -c 'srun x'": false,
		"export CUDA_VISIBLE_DEVICES=0; python x.py": false,
		"(srun x)":                 false,
		"{ srun x; }":              false,
		"if true; then srun x; fi": false,
		"exec srun x":              false,
		"time srun x":              false,
		"sudo srun x":              false,
		`"srun" x`:                 false,
		"make test && echo ok":     true,
	} {
		if err := unsafe.CheckCommand(cmd); (err == nil) != ok {
			t.Errorf("unsafe %q: allowed=%v want %v (%v)", cmd, err == nil, ok, err)
		}
	}
}

func TestCheckTimeoutKillsProcessGroup(t *testing.T) {
	j, _ := NewJail(t.TempDir(), nil, true)
	start := time.Now()
	_, exit := execCheck(context.Background(), j, "sleep 6 & sleep 6", time.Second)
	if exit != -1 || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout didn't stop the check: exit %d after %s", exit, time.Since(start))
	}
}

// delegateHarness: lead delegates once; the coder runs `coder` steps.
func delegateOnce(t *testing.T, roles string, lead map[string]any, coder func(n int) fakeReply, setup func(root string)) (*harness, delegateResult) {
	t.Helper()
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			return coder(strings.Count(req.Body, `"role":"tool"`))
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "ok"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", lead)}}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, roles, true)
	if setup != nil {
		setup(h.root)
	}
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	var r delegateResult
	for _, m := range h.sess.Msgs {
		if m.Tool == "delegate" {
			json.Unmarshal([]byte(m.Content), &r)
		}
	}
	return h, r
}

func TestModelCheckCannotReplaceRoleCheck(t *testing.T) {
	_, r := delegateOnce(t, testRoles, map[string]any{"role": "coder", "task": "do nothing", "check_cmd": "echo ok"},
		func(n int) fakeReply { return fakeReply{content: "done"} }, nil)
	if r.Status != "failed" {
		t.Fatalf("role check (ls done.txt) must still run and fail: %+v", r)
	}
}

func TestSandboxRejectedCheckIsErrorUpFront(t *testing.T) {
	var coderCalls int
	_, r := delegateOnce(t, testRoles, map[string]any{"role": "coder", "task": "x", "check_cmd": "srun pytest"},
		func(n int) fakeReply { coderCalls++; return fakeReply{content: "done"} }, nil)
	if r.Status != "error" || !strings.Contains(r.TestTail, "sandbox") || coderCalls != 0 {
		t.Fatalf("want an immediate sandbox error without running the subagent: %+v (coder calls %d)", r, coderCalls)
	}
}

func TestDeniedFileBlocksApply(t *testing.T) {
	// now with a deny rule on the file
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "done"}
			}
			return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "done.txt", "content": "x\n"})}}
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "ok"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "coder", "task": "create done.txt"})}}
	})
	fs.models = allModels()
	h3 := newRoleHarness(t, fs, testRoles, true)
	h3.orch.userRules = Ruleset{{"edit", "done.txt", Deny}}
	if err := h3.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h3.root, "done.txt")); err == nil {
		t.Fatal("a diff touching a denied file was applied")
	}
}

func TestDelegateDiffLimitedToCallerJail(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "done"}
			}
			return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "done.txt", "content": "x\n"})}}
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "ok"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "coder", "task": "t"})}}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, true)
	sub := filepath.Join(h.root, "sub dir")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "keep.txt"), []byte("k\n"), 0o644)
	os.WriteFile(filepath.Join(h.root, "outside.txt"), []byte("orig\n"), 0o644)
	gitCmd(h.root, nil, nil, "add", "-A")
	gitCmd(h.root, nil, nil, "commit", "-q", "-m", "sub")
	// the caller works in "sub dir"; simulate the subagent also touching a file above it
	jl, _ := NewJail(sub, h.orch.jl.Allowed, false)
	h.orch.jl = jl
	h.sess.RefreshSystem()
	wt, err := h.orch.worktrees.create(sub, t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.remove()
	os.WriteFile(filepath.Join(wt.root, "done.txt"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(wt.dir, "outside.txt"), []byte("hacked\n"), 0o644)
	diff, files, err := wt.diff()
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || strings.Contains(diff, "outside.txt") || wt.changedFiles[0] != "done.txt" {
		t.Fatalf("diff escaped the jail: files=%d changed=%v\n%s", files, wt.changedFiles, diff)
	}
	if err := wt.applyTo(sub, diff); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "outside.txt")); string(got) != "orig\n" {
		t.Fatal("file outside the caller's jail was modified")
	}
	if got, _ := os.ReadFile(filepath.Join(sub, "done.txt")); string(got) != "x\n" {
		t.Fatal("in-jail change not applied")
	}
}

func TestIsolationRulesAskForSharedGitState(t *testing.T) {
	s := &Session{isolated: true, agent: &Agent{Name: "coder"}, orch: &Orchestrator{}}
	for cmd, want := range map[string]Action{
		"go test ./...":                   Allow,
		"git status":                      Allow,
		"git push origin main":            Ask,
		"git stash":                       Ask,
		"git branch -f master HEAD":       Ask,
		"find . -name x -exec rm {} ;":    Ask,
		"bsk submit -g 1 -- python3 t.py": Ask,
	} {
		if got := Evaluate("run", cmd, s.rules()...); got != want {
			t.Errorf("%q: %s, want %s", cmd, got, want)
		}
	}
}

func TestEvalTransportComparison(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		results := strings.Count(req.Body, `"role":"tool"`) + strings.Count(req.Body, "tool_result name=") + strings.Count(req.Body, "[protocol error]")
		native := len(req.Tools) > 0
		switch {
		case results == 0 && native: // schema violation: missing content
			return fakeReply{calls: []ToolCall{call("w1", "write", map[string]any{"path": "answer.txt"})}}
		case results == 0: // unterminated tag: nothing parses
			return fakeReply{content: "Writing.\n<write path=\"answer.txt\">\n42"}
		case results == 1 && native:
			return fakeReply{calls: []ToolCall{call("w2", "write", map[string]any{"path": "answer.txt", "content": "42\n"})}}
		case results == 1:
			return fakeReply{content: "<write path=\"answer.txt\">\n42\n</write>"}
		}
		return fakeReply{content: "done"}
	})
	fs.models = allModels()
	home := t.TempDir()
	t.Setenv("HOME", home)
	rolesPath := filepath.Join(home, "roles.yaml")
	os.WriteFile(rolesPath, []byte(testRoles), 0o644)
	t.Setenv("LCA_ROLES", rolesPath)
	t.Setenv("LCA_GW_MAX_WAIT", "5")
	tasks := filepath.Join(t.TempDir(), "tasks")
	os.MkdirAll(filepath.Join(tasks, "answer", "fixture"), 0o755)
	os.WriteFile(filepath.Join(tasks, "answer", "fixture", "README"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(tasks, "answer", "task.yaml"), []byte("prompt: write 42 to answer.txt\ncheck_cmd: cat answer.txt\nrole: coder\nrepo: fixture\n"), 0o644)
	out := filepath.Join(t.TempDir(), "out")
	cfg := Config{Root: t.TempDir(), Dir: filepath.Join(home, ".lca"), BaseURL: fs.URL, Endpoints: []string{fs.URL}, Model: "x",
		Temperature: 0.2, MaxSteps: 10, SubagentMax: 1, KeepSessions: 10}
	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	code := runEval(cfg, []string{"-transport", "native,text", "-out", out, tasks})
	os.Stdout = stdout
	data, _ := os.ReadFile(filepath.Join(out, "results.jsonl"))
	if code != 0 {
		t.Fatalf("both transports should pass, exit %d\n%s", code, data)
	}
	got := map[string]EvalResult{}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var r EvalResult
		json.Unmarshal(line, &r)
		got[r.Transport] = r
	}
	for _, m := range []string{"native", "text"} {
		r := got[m]
		if r.Status != "passed" || r.InvalidCalls != 1 || r.AttemptedCalls != 2 || r.InvalidRate != 0.5 {
			t.Errorf("%s: %+v", m, r)
		}
	}
}

func TestInitWritesWorkingRoles(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = []string{"qwen3-30b-a3b-instruct", "kimi-k2.6", "qwen3-coder-480b-a35b-instruct", "glm-5.2", "deepseek-v4-pro"}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644)
	t.Setenv("LCA_ROLES", "")
	cfg := Config{Root: root, Dir: t.TempDir(), BaseURL: fs.URL}
	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	code := runInit(cfg, nil)
	again := runInit(cfg, nil) // refuses to overwrite
	os.Stdout = stdout
	if code != 0 || again != 1 {
		t.Fatalf("init exit %d, second %d", code, again)
	}
	rc, err := loadRoles(cfg)
	if err != nil {
		t.Fatalf("generated roles.yaml doesn't load: %v", err)
	}
	by := map[string]*Agent{}
	for _, r := range rc.Roles {
		by[r.Name] = r
	}
	if by["lead"].Models[0] != "kimi-k2.6" || by["coder"].Models[0] != "qwen3-coder-480b-a35b-instruct" ||
		by["cheap"].Models[0] != "qwen3-30b-a3b-instruct" || by["coder"].CheckCmd != "go test ./..." {
		t.Fatalf("picked: lead %v coder %v (%s) cheap %v", by["lead"].Models, by["coder"].Models, by["coder"].CheckCmd, by["cheap"].Models)
	}
	if !contains(rc.Allow, "go") || rc.Entry != "lead" {
		t.Fatalf("sandbox/entry: %v %s", rc.Allow, rc.Entry)
	}
}

func TestDoctorFindsBrokenToolParser(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" { // parser off: the call comes back as text
			return fakeReply{content: `<tool_call>{"name": "ping", "arguments": {"value": "ok"}}</tool_call>`}
		}
		return fakeReply{calls: []ToolCall{call("p", "ping", map[string]any{"value": "ok"})}}
	})
	fs.models = allModels()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".lca"), 0o755)
	os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"), []byte(testRoles), 0o644)
	t.Setenv("LCA_ROLES", "")
	cfg := Config{Root: root, Dir: t.TempDir(), BaseURL: fs.URL, Allowed: []string{"ls"}}
	gw := NewClient(cfg)
	rc, _ := loadRoles(cfg)
	if r := probeModel(cfg, gw, rc, "lead", "lead-a"); r.status != "ok" {
		t.Fatalf("lead-a: %+v", r)
	}
	r := probeModel(cfg, gw, rc, "coder", "coder-a")
	if r.status != "fail" || !strings.Contains(r.fix, "tool-call-parser") {
		t.Fatalf("coder-a should be diagnosed as a missing tool parser: %+v", r)
	}
	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	code := runDoctor(cfg, nil)
	os.Stdout = stdout
	if code != 1 {
		t.Fatalf("doctor should fail on a broken model, exit %d", code)
	}
}

// The team can be set up by hand in a session and kept: /role changes models,
// effort and tools, /role save writes a roles.yaml that loads back identically.
func TestRoleCommandAndSave(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = append(allModels(), "qwen3.8-27b")
	h := newRoleHarness(t, fs, testRoles, false)
	r := &Repl{cfg: h.orch.cfg, orch: h.orch, sess: h.sess, local: h.orch.providers.local, in: newStringInput("")}
	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()

	r.cmdRole("coder model qwen3.8-27b,kimi-k3")
	r.cmdRole("coder effort high")
	r.cmdRole("coder temperature 0.6")
	r.cmdRole("coder check python3 -m pytest -q")
	r.cmdRole("coder tools read_file,edit,run_command")
	r.cmdRole("new reviewer model glm-5.2")
	coder := h.orch.agents["coder"]
	if strings.Join(coder.Models, ",") != "qwen3.8-27b,kimi-k3" || coder.Thinking != "high" ||
		coder.Temperature == nil || *coder.Temperature != 0.6 || coder.CheckCmd != "python3 -m pytest -q" ||
		strings.Join(coder.Tools, ",") != "read_file,edit,run_command" {
		t.Fatalf("hand-set role: %+v", coder)
	}
	if rev := h.orch.agents["reviewer"]; rev == nil || !rev.IsRole || strings.Join(rev.Models, ",") != "glm-5.2" {
		t.Fatalf("new role: %+v", rev)
	}
	// the running session follows its own role's change
	r.cmdRole("lead model glm-5.2")
	if h.sess.client.Model() != "glm-5.2" {
		t.Fatalf("session model after the change: %s", h.sess.client.Model())
	}

	r.cmdRole("save")
	saved := filepath.Join(h.root, ".lca", "roles.yaml")
	if _, err := os.Stat(saved + ".bak"); err != nil {
		t.Fatal("the previous roles.yaml should be kept as .bak")
	}
	os.Stdout = stdout
	rc, err := loadRoles(Config{Root: h.root, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("the saved roles.yaml does not load: %v", err)
	}
	by := map[string]*Agent{}
	for _, a := range rc.Roles {
		by[a.Name] = a
	}
	got := by["coder"]
	if got == nil || strings.Join(got.Models, ",") != "qwen3.8-27b,kimi-k3" || got.Thinking != "high" ||
		got.CheckCmd != "python3 -m pytest -q" || !got.ToolsSet || len(got.Tools) != 3 {
		t.Fatalf("round trip lost settings: %+v", got)
	}
	if by["reviewer"] == nil || !strings.HasPrefix(by["lead"].Prompt, "You are the LEAD.") {
		t.Fatalf("round trip lost a role or its prompt: %v / %q", sortedKeys(by), by["lead"].Prompt)
	}
}

// Per-model numbers from the model card reach the request, and a role's own
// values win over them.
func TestPerModelSamplingFromRoles(t *testing.T) {
	roles := strings.Replace(testRoles, "sandbox:\n", "models:\n  lead-a: {temperature: 1.0, top_p: 0.95, effort: max}\n  coder-a: {temperature: 0.6, top_k: 20}\nsandbox:\n", 1)
	var bodies []string
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		bodies = append(bodies, req.Body)
		return fakeReply{content: "ok"}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, roles, false)
	if err := h.run(t, "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[0], `"temperature":1`) || !strings.Contains(bodies[0], `"top_p":0.95`) {
		t.Fatalf("model-card sampling not sent: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], `"reasoning_effort":"max"`) && !strings.Contains(bodies[0], `"thinking"`) {
		t.Fatalf("effort from the model card not sent: %s", bodies[0])
	}
	// the role's own temperature wins
	h.orch.agents["lead"].Temperature = f64(0.3)
	h.sess.Msgs = h.sess.Msgs[:1]
	if err := h.run(t, "again"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[len(bodies)-1], `"temperature":0.3`) {
		t.Fatalf("the role should override the model card: %s", bodies[len(bodies)-1])
	}
}

// /stats renders the session's counters without touching the model.
func TestStatsCommand(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	h := newRoleHarness(t, fs, testRoles, false)
	r := &Repl{cfg: h.orch.cfg, orch: h.orch, sess: h.sess, local: h.orch.providers.local, in: newStringInput("")}
	h.sess.stats = SessionStats{Turns: 4, ToolCalls: 9, InvalidCalls: 1, ToolErrors: 2,
		PromptTokens: 20000, CachedTokens: 15000, OutputTokens: 800, Fallbacks: 1, VerifyRuns: 3}
	out := captureStdout(t, func() { r.cmdStats("") })
	for _, want := range []string{"4 model calls", "75%", "9 calls", "didn't parse (10.0%)", "3 check runs"} {
		if !strings.Contains(out, want) {
			t.Errorf("/stats output lacks %q:\n%s", want, out)
		}
	}
}

// captureStdout collects what a command prints, so its rendering can be checked.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = wr
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		io.Copy(&b, rd)
		done <- b.String()
	}()
	fn()
	os.Stdout = saved
	wr.Close()
	out := <-done
	rd.Close()
	return out
}
