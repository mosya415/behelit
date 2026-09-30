package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// wizardModels are ids whose families models.go actually recognises, so the
// family-aware defaults (lead/coder preference, a cross-family reviewer) are
// exercised rather than accidentally satisfied.
func wizardServer(t *testing.T) *fakeServer {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{calls: []ToolCall{call("p1", "ping", map[string]any{"value": "ok"})}}
	})
	fs.models = []string{"kimi-k3", "qwen3-coder-480b-a35b-instruct", "glm-5.3", "qwen3-30b-a3b-instruct", "hy3"}
	fs.windows = map[string]int{"qwen3-coder-480b-a35b-instruct": 262144, "qwen3-30b-a3b-instruct": 32768}
	return fs
}

// emptyRepl is a session in a git root with NO roles.yaml and NO config file:
// what /setup is for.
func emptyRepl(t *testing.T, fs *fakeServer, script string) (*Repl, *Orchestrator) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LCA_ROLES", "")
	t.Setenv("LCA_DIR", filepath.Join(home, ".lca"))
	t.Setenv("LCA_CONFIG", "")
	t.Setenv("LCA_SETUP", "")
	t.Setenv("LCA_GW_MAX_WAIT", "5")
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".lca/\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if _, err := gitCmd(root, nil, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultConfig()
	cfg.Root, cfg.Dir = root, filepath.Join(home, ".lca")
	cfg.BaseURL, cfg.Endpoints = fs.URL, []string{fs.URL}
	ap := NewApprover(newStringInput(""))
	orch, err := setupOrchestrator(cfg, ap, filepath.Join(home, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { orch.tracer.Close(); orch.rec.Close() })
	sess, err := orch.NewPrimary(cfg.Agent, "", &quietView{})
	if err != nil {
		t.Fatal(err)
	}
	in := scriptedTTY(script)
	return &Repl{cfg: cfg, orch: orch, sess: sess, local: orch.providers.local, in: in,
		cfgSrc: map[string]settingSource{}}, orch
}

// The keystrokes the wizard needs, spelled out so a change to the screens shows
// up as a readable diff and not as a wall of escapes.
// Enter is spelled "\n" throughout: the pickers read bytes and treat 10 and 13
// alike, while ask() and confirm() go through the cooked line read, which needs a
// newline to terminate. One spelling works for all three.
const (
	kDown  = "\x1b[B"
	kEnter = "\n"
	kAll   = "\x01" // ^a — tick every visible row
	kNone  = "\x0e" // ^n — untick every visible row
)

// fullScript drives: gateway (Enter keeps it) · models (the pre-ticks) · lead ·
// coder · reviewer · cheap · no tiers · check (Enter keeps `go test ./...`) ·
// scope (this project) · no probe · write yes.
func fullScript() string {
	return kEnter + // 1/7 gateway: keep the url
		kEnter + // 3/7 models: accept the pre-ticked set
		kEnter + kEnter + kEnter + kEnter + // 4/7 lead, coder, reviewer, cheap
		"n\n" + // 5/7 tiers: no
		kEnter + // 6/7 check: keep the detected command
		kEnter + // 7/7 scope: this project
		"n\n" + // probe? no
		"y\n" // write
}

func TestSetupWizardWritesAndReloadsLive(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, fullScript())
	recID, tracePath := orch.rec.id, orch.tracer.Path

	out := captureStdout(t, func() { r.cmdSetup("") })
	if strings.Contains(out, "aborted") {
		t.Fatalf("the wizard aborted:\n%s", stripANSI(out))
	}

	rolesFile := filepath.Join(orch.jl.Root, ".lca", "roles.yaml")
	cfgFile := filepath.Join(orch.jl.Root, ".lca", "config.json")
	if _, err := os.Stat(rolesFile); err != nil {
		t.Fatalf("no roles.yaml: %v\n%s", err, stripANSI(out))
	}
	// It must load back as a team, through the real loader.
	rc, err := loadRoles(Config{Root: orch.jl.Root, Dir: r.cfg.Dir})
	if err != nil {
		t.Fatalf("the written roles.yaml does not load: %v", err)
	}
	if rc.Entry != "lead" {
		t.Errorf("entry = %q", rc.Entry)
	}
	for _, want := range []string{"lead", "coder", "reviewer", "cheap"} {
		found := false
		for _, a := range rc.Roles {
			if a.Name == want {
				found = true
				if len(a.Models) == 0 {
					t.Errorf("role %s has no chain", want)
				}
			}
		}
		if !found {
			t.Errorf("role %s missing from the written team", want)
		}
	}
	data, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("no config.json: %v", err)
	}
	if !strings.Contains(string(data), fs.URL) {
		t.Errorf("base_url not written:\n%s", data)
	}
	if strings.Contains(string(data), "api_key\"") {
		t.Errorf("an api_key was written for an anonymous gateway:\n%s", data)
	}

	// Live, in the session the operator is sitting in.
	if r.orch.roles == nil {
		t.Fatal("the session did not adopt the team")
	}
	if r.sess.agent.Name != "lead" {
		t.Errorf("the session runs as %q, want lead", r.sess.agent.Name)
	}
	if got := r.sess.client.Model(); got != r.orch.agents["lead"].Models[0] {
		t.Errorf("the session is on %q, want the lead's first model %q", got, r.orch.agents["lead"].Models[0])
	}
	if !containsStr(r.orch.jl.Allowed, "go") {
		t.Errorf("the sandbox allowlist did not follow: %v", r.orch.jl.Allowed)
	}
	// Reloaded, not restarted: one audit log and one trace per session.
	if orch.rec.id != recID || orch.tracer.Path != tracePath {
		t.Errorf("the recorder or the trace was rotated: %q/%q → %q/%q", recID, tracePath, orch.rec.id, orch.tracer.Path)
	}
}

// The wizard's defaults ARE lca init's: same functions, same team.
func TestSetupDefaultsMatchInit(t *testing.T) {
	names := []string{"kimi-k3", "qwen3-coder-480b-a35b-instruct", "glm-5.3", "qwen3-30b-a3b-instruct"}
	pre := map[string]bool{}
	for _, m := range pickModels(names, leadPref, 2) {
		pre[m] = true
	}
	for _, m := range pickModels(names, coderPref, 2) {
		pre[m] = true
	}
	pre[pickCheap(names)] = true

	// What lca init would write for the same served list.
	if got, want := firstOf(pickModels(names, leadPref, 1)), pickModels(names, leadPref, 2)[0]; got != want {
		t.Fatalf("lead default %q != init's first lead %q", got, want)
	}
	if got, want := firstOf(pickModels(names, coderPref, 1)), pickModels(names, coderPref, 2)[0]; got != want {
		t.Fatalf("coder default %q != init's first coder %q", got, want)
	}
	if !pre[pickCheap(names)] {
		t.Fatal("init's cheap model is not pre-ticked")
	}
	for _, m := range pickModels(names, leadPref, 2) {
		if !pre[m] {
			t.Fatalf("init would write %s in the lead chain, but the wizard does not pre-tick it", m)
		}
	}
}

func TestSetupAbortWritesNothing(t *testing.T) {
	fs := wizardServer(t)
	// Everything as usual, then Ctrl-C at the write confirmation.
	script := strings.TrimSuffix(fullScript(), "y\n") + "\x03"
	r, orch := emptyRepl(t, fs, script)
	out := captureStdout(t, func() { r.cmdSetup("") })
	if !strings.Contains(out, "nothing was written") {
		t.Errorf("an abort must say so:\n%s", stripANSI(out))
	}
	lca := filepath.Join(orch.jl.Root, ".lca")
	ents, _ := os.ReadDir(lca)
	for _, e := range ents {
		t.Errorf("the abort left %s behind", e.Name())
	}
	if r.orch.roles != nil {
		t.Error("the team was adopted despite the abort")
	}
}

func TestSetupRefusesWithoutTTY(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, "")
	r.in = newStringInput("") // a pipe: CI, or `lca … < file`
	out := captureStdout(t, func() {
		if r.cmdSetup("") {
			t.Error("cmdSetup must not ask for a turn")
		}
	})
	if !strings.Contains(out, "lca init") {
		t.Errorf("the refusal must name the non-interactive path:\n%s", stripANSI(out))
	}
	if _, err := os.Stat(filepath.Join(orch.jl.Root, ".lca", "roles.yaml")); err == nil {
		t.Error("a refused wizard wrote a file")
	}
}

func TestSetupRefusedWhileBackgroundSubagentRuns(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, fullScript())
	r.sess.mu.Lock()
	r.sess.bgRunning = 1
	r.sess.mu.Unlock()
	out := captureStdout(t, func() { r.cmdSetup("") })
	if !strings.Contains(out, "/tasks") {
		t.Errorf("the refusal must send the operator to /tasks:\n%s", stripANSI(out))
	}
	if _, err := os.Stat(filepath.Join(orch.jl.Root, ".lca", "roles.yaml")); err == nil {
		t.Error("a refused wizard wrote a file")
	}
}

func TestSameFamilyWarningIsShared(t *testing.T) {
	// Two DIFFERENT ids of one family: the sentence fires.
	same := sameFamilyWarning("kimi-k3", "kimi-k2-0711")
	if !strings.Contains(same, "same family") {
		t.Fatalf("same-family pair produced %q", same)
	}
	// The same model on both sides is the worse case and must be named as such:
	// "X and X are the same family" invited the reader to think two models were
	// involved, when it is one set of weights reviewing its own diff.
	itself := sameFamilyWarning("kimi-k3", "kimi-k3")
	if !strings.Contains(itself, "reviewing itself") {
		t.Errorf("coder == reviewer must say so plainly: %q", itself)
	}
	// An unknown name is no evidence of anything.
	if got := sameFamilyWarning("kimi-k3", "totally-made-up-9000"); got != "" {
		t.Errorf("an unrecognised name must not warn: %q", got)
	}
	if got := sameFamilyWarning("", "kimi-k3"); got != "" {
		t.Errorf("an empty role must not warn: %q", got)
	}
}

func TestSetupWarnsSameFamilyReviewer(t *testing.T) {
	fs := wizardServer(t)
	// Pick only two models of the SAME family, so the reviewer cannot avoid it:
	// filter to "qwen", tick both, then take the defaults through.
	// ^n clears the pre-ticks (no filter yet, so every row), then the filter
	// narrows to the two qwen ids and ^a ticks exactly those. A tick the filter
	// hides stays ticked on purpose, which is why the clear comes first.
	script := kEnter + // gateway
		kNone + "qwen" + kAll + kEnter + // models: exactly the two qwen ids
		kEnter + kEnter + kDown + kEnter + kEnter + // lead, coder, reviewer (a real row), cheap
		kEnter + // check
		kEnter + // scope
		"n\n" + // probe
		"y\n" // write
	r, orch := emptyRepl(t, fs, script)
	out := captureStdout(t, func() { r.cmdSetup("") })
	plain := stripANSI(out)
	if !strings.Contains(plain, "same family") {
		t.Errorf("a same-family reviewer must be warned about:\n%s", plain)
	}
	// A warning, never a refusal: the role is still assigned.
	if _, err := os.Stat(filepath.Join(orch.jl.Root, ".lca", "roles.yaml")); err != nil {
		t.Errorf("the warning blocked the write: %v", err)
	}
}

func TestSetupOneModelWritesNoRoles(t *testing.T) {
	fs := wizardServer(t)
	// Filter to the single 480b id, tick it, confirm — then scope, no probe, write.
	script := kEnter + // gateway
		kNone + "480" + kAll + kEnter + // models: exactly one row matches
		kEnter + // scope
		"n\n" + // probe
		"y\n" // write
	r, orch := emptyRepl(t, fs, script)
	out := captureStdout(t, func() { r.cmdSetup("") })
	plain := stripANSI(out)
	if _, err := os.Stat(filepath.Join(orch.jl.Root, ".lca", "roles.yaml")); err == nil {
		t.Error("one model must not produce a roles.yaml")
	}
	data, err := os.ReadFile(filepath.Join(orch.jl.Root, ".lca", "config.json"))
	if err != nil {
		t.Fatalf("no config.json: %v\n%s", err, plain)
	}
	if !strings.Contains(string(data), "qwen3-coder-480b-a35b-instruct") {
		t.Errorf("the one model must be written as the session's model:\n%s", data)
	}
	if !strings.Contains(plain, "one model picked") {
		t.Errorf("the screen must say what it is doing:\n%s", plain)
	}
}

func TestSetupWriteRollsBackOnSecondFailure(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, "")
	root := orch.jl.Root
	lca := filepath.Join(root, ".lca")
	os.MkdirAll(lca, 0o755)
	rolesFile := filepath.Join(lca, "roles.yaml")
	before := []byte("# an operator's own file\nentry: lead\nroles:\n  lead:\n    models: [kimi-k3]\n")
	os.WriteFile(rolesFile, before, 0o644)

	p := &setupPlan{endpoint: fs.URL, reached: true, picked: []string{"kimi-k3", "glm-5.3"},
		roles:  map[string]string{"lead": "kimi-k3", "coder": "glm-5.3"},
		chains: map[string][]string{"lead": {"kimi-k3"}, "coder": {"glm-5.3"}},
		tiers:  map[string][]string{}, textOnly: map[string]bool{}, probes: map[string]probeResult{},
		allow: []string{"go", "git"}}
	// config.json is a DIRECTORY: the second write cannot succeed.
	os.MkdirAll(filepath.Join(lca, "config.json"), 0o755)

	captureStdout(t, func() {
		if _, err := p.write(r); err == nil {
			t.Error("write must fail when config.json cannot be written")
		}
	})
	after, err := os.ReadFile(rolesFile)
	if err != nil || string(after) != string(before) {
		t.Fatalf("roles.yaml was not restored:\n%s", after)
	}
}

func TestFirstRunOfferSkippedOffTTY(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, "y\n")
	r.in = newStringInput("y\n")
	out := captureStdout(t, func() { r.offerSetup() })
	if out != "" {
		t.Errorf("the offer must be silent off a terminal: %q", out)
	}
	if _, err := os.Stat(filepath.Join(orch.jl.Root, ".lca", "roles.yaml")); err == nil {
		t.Error("the offer ran the wizard off a terminal")
	}
	// A config file that says nothing about a gateway does NOT count as configured:
	// {"approve":"run"} is what the operator gets from their first /set -user, and it
	// used to silence the offer permanently.
	r.in = scriptedTTY("n\n")
	r.orch.fc.Sources = []string{"somewhere/config.json"}
	r.orch.fc.Raw = map[string]string{"approve": "run"}
	out = captureStdout(t, func() { r.offerSetup() })
	if !strings.Contains(stripANSI(out), "nothing is configured here") {
		t.Errorf("a file with no gateway in it must not silence the offer: %q", stripANSI(out))
	}
	// A file that names the gateway does.
	r.in = scriptedTTY("y\n")
	r.orch.fc.Raw = map[string]string{"base_url": "http://gw:8000/v1"}
	out = captureStdout(t, func() { r.offerSetup() })
	if out != "" {
		t.Errorf("a configured directory must not be offered the wizard: %q", out)
	}
}

func TestReloadKeepsRecorderTraceAndTasks(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, "")
	recID, tracePath := orch.rec.id, orch.tracer.Path
	// Something in flight that must survive: a task record and a conversation.
	orch.trackStart("t1", "task", "build", "an earlier delegation")
	r.sess.Msgs = append(r.sess.Msgs,
		Message{Role: "user", Content: "hello"}, Message{Role: "assistant", Content: "hi"})

	// A team appears on disk between the two loads, the way /setup leaves one.
	os.MkdirAll(filepath.Join(orch.jl.Root, ".lca"), 0o755)
	os.WriteFile(filepath.Join(orch.jl.Root, ".lca", "roles.yaml"), []byte(`entry: lead
sandbox:
  allow: [go, git, echo]
roles:
  lead:
    description: Leads.
    models: [kimi-k3]
    tools: [read_file, delegate]
  coder:
    description: Codes.
    models: [glm-5.3]
`), 0o644)
	t.Setenv("LCA_ROOT", orch.jl.Root)
	t.Setenv("LCA_BASE_URL", fs.URL)

	captureStdout(t, func() {
		if err := r.reload("test"); err != nil {
			t.Fatalf("reload: %v", err)
		}
	})
	if orch.rec.id != recID || orch.tracer.Path != tracePath {
		t.Errorf("the recorder or trace was rotated")
	}
	if len(orch.History()) != 1 {
		t.Errorf("the task history was lost: %v", orch.History())
	}
	if orch.roles == nil || len(orch.roles.Roles) != 2 {
		t.Fatalf("the new team was not adopted: %v", orch.roles)
	}
	if !containsStr(orch.jl.Allowed, "go") {
		t.Errorf("the new allowlist did not reach the live jail: %v", orch.jl.Allowed)
	}
	if !strings.Contains(r.sess.Msgs[0].Content, "go") {
		t.Errorf("the system prompt was not rebuilt with the new sandbox")
	}
	found := 0
	for _, m := range r.sess.Msgs {
		if m.Content == "hello" || m.Content == "hi" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("the conversation did not survive the reload (%d of 2 messages)", found)
	}
	var names []string
	for _, c := range r.menu() {
		names = append(names, c.name)
	}
	for _, want := range []string{"/delegate", "/agents"} {
		if !containsStr(names, want) {
			t.Errorf("%s is missing from the menu after the reload: %v", want, names)
		}
	}
	if n := strings.Count(readRecorderPath(t, r.cfg.Dir), `"reload"`); n != 1 {
		t.Errorf("recorded %d reload events, want 1", n)
	}
}

// readRecorderPath is the audit log — where Recorder.Event writes. (SessionPath
// is the transcript, which holds messages and not events.)
func readRecorderPath(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		return ""
	}
	return string(data)
}

// A model whose native tool call comes back as text is offered the transport it
// actually works with, and accepting writes it where loadRoles reads it back.
func TestSetupOffersTextTransportForBrokenParser(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "glm-5.3" { // the engine's tool-call parser is off for it
			return fakeReply{content: `<tool_call>{"name": "ping", "arguments": {"value": "ok"}}</tool_call>`}
		}
		return fakeReply{calls: []ToolCall{call("p", "ping", map[string]any{"value": "ok"})}}
	})
	fs.models = []string{"kimi-k3", "glm-5.3"}
	script := kEnter + // gateway
		kNone + kAll + kEnter + // models: both
		kEnter + kEnter + kEnter + kEnter + // lead, coder, reviewer, cheap
		kEnter + // check
		kEnter + // scope
		"y\n" + // probe: yes
		"y\n" + // take glm-5.3 as text
		"y\n" // write
	r, orch := emptyRepl(t, fs, script)
	out := stripANSI(captureStdout(t, func() { r.cmdSetup("") }))
	if !strings.Contains(out, "tool-call-parser") {
		t.Errorf("the summary must name the engine flag the vendor documents:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(orch.jl.Root, ".lca", "roles.yaml"))
	if err != nil {
		t.Fatalf("no roles.yaml: %v\n%s", err, out)
	}
	if !strings.Contains(string(data), "glm-5.3: {transport: text}") {
		t.Fatalf("the accepted transport is not in the YAML:\n%s", data)
	}
	rc, err := loadRoles(Config{Root: orch.jl.Root, Dir: r.cfg.Dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := rc.transportOf("glm-5.3"); got != transportText {
		t.Fatalf("loadRoles reads the transport back as %q, want text", got)
	}
}

// A /model the operator chose a minute ago must survive a reload: a reload that
// silently undid it would be worse than no reload.
func TestReloadKeepsSessionOverrides(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, "")
	t.Setenv("LCA_ROOT", orch.jl.Root)
	t.Setenv("LCA_BASE_URL", fs.URL)

	captureStdout(t, func() { r.cmdModel("glm-5.3") })
	if r.unsaved["model"] != "glm-5.3" {
		t.Fatalf("unsaved carries %q, want the raw value applySetting takes", r.unsaved["model"])
	}
	out := captureStdout(t, func() {
		if err := r.reload("test"); err != nil {
			t.Fatalf("reload: %v", err)
		}
	})
	if strings.Contains(out, "could not carry") {
		t.Fatalf("the override did not survive:\n%s", stripANSI(out))
	}
	if r.cfg.Model != "glm-5.3" {
		t.Errorf("after the reload the model is %q, want glm-5.3", r.cfg.Model)
	}
	if r.cfgSrc["model"].Src != SrcSession {
		t.Errorf("the source should still be the session: %v", r.cfgSrc["model"])
	}
}

// The tier path: two tiers declared, the lead and coder pointed at one, and the
// file loading back so /tier can switch the whole team.
func TestSetupWritesLoadableTiers(t *testing.T) {
	fs := wizardServer(t)
	script := kEnter + // gateway
		kEnter + // models: the pre-ticks (4 of 5, so tiers are offered)
		kEnter + kEnter + kEnter + kEnter + // lead, coder, reviewer, cheap
		"y\n" + // tiers: yes
		kEnter + // cheap tier: the small ones
		kEnter + // premium tier: the rest
		"y\n" + // point lead and coder at the tier: yes (their picks are replaced)
		kEnter + // check
		kEnter + // scope
		"n\n" + // probe
		"y\n" // write
	r, orch := emptyRepl(t, fs, script)
	out := stripANSI(captureStdout(t, func() { r.cmdSetup("") }))
	data, err := os.ReadFile(filepath.Join(orch.jl.Root, ".lca", "roles.yaml"))
	if err != nil {
		t.Fatalf("no roles.yaml: %v\n%s", err, out)
	}
	if !strings.Contains(string(data), "tiers:") {
		t.Fatalf("no tiers: block:\n%s", data)
	}
	rc, err := loadRoles(Config{Root: orch.jl.Root, Dir: r.cfg.Dir})
	if err != nil {
		t.Fatalf("the written team does not load: %v\n%s", err, data)
	}
	if len(rc.TierOrder) != 2 {
		t.Fatalf("tiers did not round-trip: %v", rc.TierOrder)
	}
	byName := map[string]*Agent{}
	for _, a := range rc.Roles {
		byName[a.Name] = a
	}
	if byName["lead"].Tier != "premium" || byName["coder"].Tier != "premium" {
		t.Errorf("lead/coder should run a tier: %q / %q", byName["lead"].Tier, byName["coder"].Tier)
	}
	// A tier must not be put on the roles chosen for what they are: a cross-family
	// reviewer and a small cheap model.
	if byName["reviewer"].Tier != "" || byName["cheap"].Tier != "" {
		t.Errorf("reviewer/cheap must keep their own chains: %q / %q", byName["reviewer"].Tier, byName["cheap"].Tier)
	}
	// And /tier can switch them.
	r.orch.roles = rc
	for _, a := range rc.Roles {
		r.orch.agents[a.Name] = a
	}
	if err := r.setTier("cheap"); err != nil {
		t.Fatalf("setTier: %v", err)
	}
	if strings.Join(byName["lead"].Models, ",") != strings.Join(rc.Tiers["cheap"], ",") {
		t.Errorf("/tier cheap did not remap the lead: %v", byName["lead"].Models)
	}
}

// "this project" must write the PROJECT's roles.yaml. It used to go through
// rolesPath, which returns rc.Sources[0] — the first file loadRoles read, i.e.
// $LCA_DIR/roles.yaml whenever a global team exists. So the operator's team for
// every other repository was overwritten, the project file was left alone, and
// because the project file wins per role name at load, the wizard's team was
// persisted nowhere that takes effect.
func TestSetupProjectScopeWritesTheProjectFile(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, fullScript())
	userRoles := filepath.Join(r.cfg.Dir, "roles.yaml")
	projRoles := filepath.Join(orch.jl.Root, ".lca", "roles.yaml")
	global := "entry: lead\n\nroles:\n  lead:\n    models: [a-global-team-model]\n"
	os.MkdirAll(r.cfg.Dir, 0o755)
	if err := os.WriteFile(userRoles, []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	// The team as loadRoles sees it: user file FIRST, so Sources[0] is the global one.
	rc, err := loadRoles(Config{Root: orch.jl.Root, Dir: r.cfg.Dir})
	if err != nil {
		t.Fatal(err)
	}
	r.orch.roles = rc
	if len(rc.Sources) == 0 || rc.Sources[0] != userRoles {
		t.Fatalf("this test needs Sources[0] to be the user file, got %v", rc.Sources)
	}

	out := stripANSI(captureStdout(t, func() { r.cmdSetup("") }))
	if after, _ := os.ReadFile(userRoles); string(after) != global {
		t.Errorf("the global team was overwritten by a project-scoped setup:\n%s", after)
	}
	data, err := os.ReadFile(projRoles)
	if err != nil {
		t.Fatalf("the project roles.yaml was not written: %v\n%s", err, out)
	}
	if strings.Contains(string(data), "a-global-team-model") {
		t.Errorf("the project file got the global team's models:\n%s", data)
	}
}

// reload must re-point the session's client even when the entry agent declares no
// chain — the single-model /setup path, which writes no roles.yaml and leaves the
// builtin "build" agent in charge. SetAgent touches s.client only for an agent with
// Models or Model, so the session stayed on the OLD client, endpoint and token one
// line under "the team is live in this session".
func TestReloadRepointsTheSessionWithoutARoleChain(t *testing.T) {
	first := wizardServer(t)
	r, orch := emptyRepl(t, first, "")
	before := r.sess.client
	second := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	second.models = []string{"a-model-on-the-second-gateway"}
	// Exactly what the single-model wizard path writes: base_url and model, no roles.
	if _, err := setConfigValues(filepath.Join(orch.jl.Root, ".lca", "config.json"), map[string]any{
		"base_url": second.URL + "/v1", "model": "a-model-on-the-second-gateway",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCA_ROOT", orch.jl.Root)
	t.Setenv("LCA_BASE_URL", "")
	t.Setenv("LCA_MODEL", "")
	captureStdout(t, func() {
		if err := r.reload("test"); err != nil {
			t.Fatalf("reload: %v", err)
		}
	})
	if r.orch.roles != nil {
		t.Fatal("this test is about the NO-team path")
	}
	if r.sess.client == before {
		t.Fatal("the session is still on the pre-reload client")
	}
	if got, want := r.sess.client.Endpoint(), second.URL+"/v1"; got != want {
		t.Errorf("the session is on %q, want the newly configured %q", got, want)
	}
	if got := r.sess.client.Model(); got != "a-model-on-the-second-gateway" {
		t.Errorf("the session is on model %q, want the newly configured one", got)
	}
	if r.sess.client != r.orch.providers.local {
		t.Error("the session's client is not reachable from the new providers")
	}
}

// /setup must be able to configure a gateway that wants a key. isAuthErr looked for
// an *APIError that ProbeModels never produced, so a 401 was reported as "the
// gateway may still be starting", the whole credentials step was unreachable, and
// the operator was pushed back to export LCA_API_KEY — the one thing this work was
// for.
func TestSetupConfiguresAnAuthenticatedGateway(t *testing.T) {
	const token = "sk-the-right-token"
	fs := wizardServer(t)
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"missing token"}}`)
			return
		}
		fs.Config.Handler.ServeHTTP(w, req)
	}))
	t.Cleanup(guard.Close)

	// The gateway 401s, so the credentials step fires; name the variable holding the
	// key, and the re-probe finds the models.
	t.Setenv("LCA_GW_TOKEN_FOR_TEST", token)
	script := guard.URL + "/v1" + kEnter + // gateway: the guarded url
		kEnter + // credentials: "name an environment variable"
		"LCA_GW_TOKEN_FOR_TEST" + kEnter +
		kEnter + // models: the pre-ticks
		kEnter + kEnter + kEnter + kEnter + // lead, coder, reviewer, cheap
		"n\n" + // tiers: no
		kEnter + // check
		kEnter + // scope
		"n\n" + // probe
		"y\n" // write
	r, orch := emptyRepl(t, fs, script)
	out := stripANSI(captureStdout(t, func() { r.cmdSetup("") }))
	if strings.Contains(out, "listed no models") {
		t.Fatalf("the wizard died on the authenticated gateway:\n%s", out)
	}
	if !strings.Contains(out, "the key works") {
		t.Errorf("the re-probe with the credential must be reported:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(orch.jl.Root, ".lca", "config.json"))
	if err != nil {
		t.Fatalf("nothing was written: %v\n%s", err, out)
	}
	if !strings.Contains(string(data), `"api_key_env": "LCA_GW_TOKEN_FOR_TEST"`) {
		t.Errorf("the variable's NAME must be recorded:\n%s", data)
	}
	if strings.Contains(string(data), token) {
		t.Errorf("the literal token was written to the file:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(orch.jl.Root, ".lca", "roles.yaml")); err != nil {
		t.Errorf("no team was written: %v", err)
	}
}

// A 401 from /v1/models has to be a typed status, or every consumer that decides on
// one is wrong about it.
func TestProbeModelsReturnsATypedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"no"}}`)
	}))
	t.Cleanup(srv.Close)
	cfg := defaultConfig()
	cfg.BaseURL, cfg.Endpoints = srv.URL+"/v1", []string{srv.URL + "/v1"}
	_, err := NewClient(cfg).ListModels()
	if err == nil {
		t.Fatal("a 401 must be an error")
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Fatalf("want a typed 401, got %T %v", err, err)
	}
	if !isAuthErr(err) {
		t.Error("isAuthErr must recognise it — the credentials step depends on it")
	}
	if errorHint(err) == "" {
		t.Error("errorHint must have something to say about a 401")
	}
}

// The wizard writes base_url and the credentials. It used to delete the operator's
// `endpoints` list on every multi-model gateway, one line after a review screen
// whose whole contract is that it lists everything about to happen.
func TestSetupKeepsSpareEndpoints(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, fullScript())
	cfgFile := filepath.Join(orch.jl.Root, ".lca", "config.json")
	os.MkdirAll(filepath.Dir(cfgFile), 0o755)
	if err := os.WriteFile(cfgFile, []byte(`{
  "base_url": "http://old:8000/v1",
  "endpoints": ["http://spare-one:8080/v1", "http://spare-two:8080/v1"],
  "max_tokens": 8192
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.reloadFileConfig()
	out := stripANSI(captureStdout(t, func() { r.cmdSetup("") }))
	data, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"spare-one", "spare-two", "8192", fs.URL} {
		if !strings.Contains(string(data), want) {
			t.Errorf("%s is gone from the config:\n%s\n%s", want, data, out)
		}
	}
}

// Saying yes to tiers used to discard the lead's and coder's picked chains, while
// the review screen still confirmed them: pick kimi, run qwen. The pick is now the
// default, and the tier is asked for separately and printed as what will run.
func TestSetupTiersDoNotDiscardThePicksUnlessAsked(t *testing.T) {
	// A width, explicitly. What this test asserts is CONTENT — that the screen says
	// a particular thing — and the screen is fitted to the terminal it is drawn
	// for, so at a narrow $COLUMNS the assertion below is about ellipsis rather
	// than about the sentence. It failed at COLUMNS=41 before this was here.
	t.Setenv("COLUMNS", "200")
	fs := wizardServer(t)
	script := kEnter + // gateway
		kEnter + // models: the pre-ticks
		kEnter + kEnter + kEnter + kEnter + // lead, coder, reviewer, cheap
		"y\n" + // tiers: yes
		kEnter + // cheap tier
		kEnter + // premium tier
		"n\n" + // point lead and coder at it? NO — keep the picks
		kEnter + // check
		kEnter + // scope
		"n\n" + // probe
		"y\n" // write
	r, orch := emptyRepl(t, fs, script)
	out := stripANSI(captureStdout(t, func() { r.cmdSetup("") }))
	rc, err := loadRoles(Config{Root: orch.jl.Root, Dir: r.cfg.Dir})
	if err != nil {
		t.Fatalf("the written team does not load: %v\n%s", err, out)
	}
	if len(rc.TierOrder) == 0 {
		t.Fatal("the tiers were not written")
	}
	lead := rc.role("lead")
	if lead == nil {
		t.Fatal("no lead")
	}
	if lead.Tier != "" {
		t.Errorf("the lead was tiered although the operator said no: %q", lead.Tier)
	}
	// The review screen must have named the chain the file actually holds.
	if !strings.Contains(out, strings.Join(lead.Models, " → ")) {
		t.Errorf("the review screen did not print the lead's written chain %v:\n%s", lead.Models, out)
	}
}

// "configure it anyway" promises to write the team for a gateway that is down
// right now. It used to die one screen later with "the gateway listed no models —
// nothing to pick from", having written nothing at all.
func TestSetupConfiguresADownGatewayByHand(t *testing.T) {
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, "")
	// A url nothing answers on, then "configure it anyway" (the third row), then the
	// ids typed by hand.
	dead := "http://127.0.0.1:1/v1"
	r.in = scriptedTTY(dead + kEnter +
		kDown + kDown + kEnter + // not reachable: ↓ ↓ → "configure it anyway"
		"lead-model, coder-model" + kEnter + // the ids, the lead's first
		kEnter + kEnter + kEnter + kEnter + // lead, coder, reviewer, cheap
		kEnter + // check
		kEnter + // scope
		"y\n") // write (no probe is offered: the gateway never answered)
	out := stripANSI(captureStdout(t, func() { r.cmdSetup("") }))
	if strings.Contains(out, "nothing to pick from") {
		t.Fatalf("the down-gateway path still dies:\n%s", out)
	}
	if !strings.Contains(out, "never answered") {
		t.Errorf("the review screen must repeat that the gateway was never reached:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(orch.jl.Root, ".lca", "config.json"))
	if err != nil {
		t.Fatalf("the endpoint was not written: %v\n%s", err, out)
	}
	if !strings.Contains(string(data), dead) {
		t.Errorf("base_url is not the url that was entered:\n%s", data)
	}
	roles, err := os.ReadFile(filepath.Join(orch.jl.Root, ".lca", "roles.yaml"))
	if err != nil {
		t.Fatalf("no team was written: %v\n%s", err, out)
	}
	if !strings.Contains(string(roles), "lead-model") {
		t.Errorf("the hand-typed ids did not reach the team:\n%s", roles)
	}
}

// The step numbers were hand-written literals and did not match the steps that
// run: an anonymous gateway went 1/7 → 3/7, `/setup models` opened on 3/7, and
// 7/7 was followed by two more screens.
func TestSetupStepNumbersMatchTheStepsThatRun(t *testing.T) {
	// A screen title is chrome and the stone around it belongs to the theme: the
	// lintel spells it "══ SETUP  3/6 …" and a framed screen "╔═ SETUP ═ 3/6 …".
	// The TITLE and its numbers are the meaning, so that is what is matched and the
	// frame is allowed to be whatever is drawn.
	reHead := regexp.MustCompile(`^[^A-Za-z0-9]*SETUP\b`)
	headers := func(out string) []string {
		var hs []string
		for _, l := range strings.Split(stripANSI(out), "\n") {
			if reHead.MatchString(strings.TrimSpace(l)) {
				hs = append(hs, strings.TrimSpace(l))
			}
		}
		return hs
	}
	// Every numbered screen must appear, in order, with one total.
	check := func(t *testing.T, hs []string) {
		t.Helper()
		re := regexp.MustCompile(`(\d+)/(\d+)`)
		last, total := 0, 0
		for _, h := range hs {
			m := re.FindStringSubmatch(h)
			if m == nil {
				continue // the unnumbered review screen
			}
			n, _ := strconv.Atoi(m[1])
			tot, _ := strconv.Atoi(m[2])
			if total == 0 {
				total = tot
			}
			if tot != total {
				t.Errorf("the total changes between screens (%d then %d):\n%s", total, tot, strings.Join(hs, "\n"))
			}
			if n != last && n != last+1 {
				t.Errorf("the numbering jumps from %d to %d:\n%s", last, n, strings.Join(hs, "\n"))
			}
			last = n
		}
		if last != total {
			t.Errorf("the last numbered screen is %d of %d:\n%s", last, total, strings.Join(hs, "\n"))
		}
		if len(hs) == 0 || !strings.Contains(hs[len(hs)-1], "review") {
			t.Errorf("the last screen must be the unnumbered review:\n%s", strings.Join(hs, "\n"))
		}
	}

	fs := wizardServer(t)
	r, _ := emptyRepl(t, fs, fullScript())
	check(t, headers(captureStdout(t, func() { r.cmdSetup("") })))

	// /setup models runs fewer steps, and must open on its own first one.
	fs2 := wizardServer(t)
	r2, _ := emptyRepl(t, fs2, kEnter+kEnter+kEnter+kEnter+kEnter+"n\n"+kEnter+kEnter+"n\n"+"y\n")
	hs := headers(captureStdout(t, func() { r2.cmdSetup("models") }))
	if len(hs) == 0 || !strings.Contains(hs[0], "1/") {
		t.Errorf("/setup models must open on its first step:\n%s", strings.Join(hs, "\n"))
	}
	check(t, hs)
}

// The models screen must open with NOTHING ticked and offer its proposal to
// Enter. It used to open with three ticks the operator never made, so their first
// deliberate space UNTICKED a model they wanted — and with the remaining picks
// collapsing onto one model, lead, coder and reviewer all ended up on it, the
// reviewer warning that it was reviewing itself.
func TestSetupModelsScreenOpensUntickedAndOffersTheProposal(t *testing.T) {
	// A width, explicitly. What this test asserts is CONTENT — that the screen says
	// a particular thing — and the screen is fitted to the terminal it is drawn
	// for, so at a narrow $COLUMNS the assertion below is about ellipsis rather
	// than about the sentence. It failed at COLUMNS=41 before this was here.
	t.Setenv("COLUMNS", "200")
	fs := wizardServer(t)
	r, orch := emptyRepl(t, fs, fullScript())
	out := stripANSI(captureStdout(t, func() { r.cmdSetup("") }))

	if strings.Contains(out, "space selects") {
		t.Error(`the hint must say what space does to a tick ("ticks"), not "selects"`)
	}
	if !strings.Contains(out, "enter with none ticked takes the proposal") {
		t.Errorf("the proposal is not offered to Enter:\n%s", out)
	}
	if !strings.Contains(out, "proposed as lead") {
		t.Errorf("a proposed row must say what it is proposed for:\n%s", out)
	}
	if !strings.Contains(out, "(the proposal)") {
		t.Errorf("Enter on an untouched screen takes the proposal:\n%s", out)
	}
	// and each role screen says what the role is for
	for _, want := range []string{"plans, splits the work and delegates", "writes the change in its own worktree",
		"second opinion on a passed diff", "summaries and compaction only"} {
		if !strings.Contains(out, want) {
			t.Errorf("a role screen does not say what the role does (%q missing)", want)
		}
	}
	// the team that lands must not put the reviewer on the coder's own model
	rc, err := loadRoles(Config{Root: orch.jl.Root, Dir: r.cfg.Dir})
	if err != nil {
		t.Fatalf("roles.yaml does not load: %v", err)
	}
	coder, rev := rc.role("coder"), rc.role("reviewer")
	if coder == nil {
		t.Fatal("no coder role")
	}
	if rev != nil && len(rev.Models) > 0 && len(coder.Models) > 0 && rev.Models[0] == coder.Models[0] {
		t.Errorf("reviewer %v shares the coder's first model %v", rev.Models, coder.Models)
	}
}
