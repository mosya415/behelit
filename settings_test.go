package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layerRoot sets up a HOME with ~/.lca/config.json and a project root with
// .lca/config.json, so the whole precedence table can be exercised in one test.
func layerRoot(t *testing.T) (root, home string) {
	t.Helper()
	home = t.TempDir()
	root = t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	t.Setenv("HOME", home)
	t.Setenv("LCA_ROOT", root)
	t.Setenv("LCA_DIR", filepath.Join(home, ".lca"))
	t.Setenv("LCA_CONFIG", "")
	for _, k := range []string{"LCA_MODEL", "LCA_THINKING", "LCA_BASE_URL", "LCA_API_KEY", "LCA_TIER", "LCA_TOOLS"} {
		t.Setenv(k, "")
	}
	os.MkdirAll(filepath.Join(home, ".lca"), 0o755)
	os.MkdirAll(filepath.Join(root, ".lca"), 0o755)
	return root, home
}

func TestSettingsPrecedence(t *testing.T) {
	root, home := layerRoot(t)
	userFile := filepath.Join(home, ".lca", "config.json")
	projFile := filepath.Join(root, ".lca", "config.json")
	os.WriteFile(userFile, []byte(`{"model":"u"}`), 0o644)
	os.WriteFile(projFile, []byte(`{"model":"p","thinking":"high"}`), 0o644)
	t.Setenv("LCA_MODEL", "e")

	cfg, _, srcs := loadConfigWithSources()
	if cfg.Model != "e" || srcs["model"].Src != SrcEnv {
		t.Fatalf("env must win: model=%q src=%v", cfg.Model, srcs["model"])
	}
	if cfg.Thinking != "high" || srcs["effort"].Src != SrcProjectFile {
		t.Fatalf("effort=%q src=%v, want high from the project file", cfg.Thinking, srcs["effort"])
	}
	if srcs["effort"].Path != projFile {
		t.Fatalf("the source must name the file: %q", srcs["effort"].Path)
	}

	// Unset the variable and the project file is promoted.
	t.Setenv("LCA_MODEL", "")
	cfg, _, srcs = loadConfigWithSources()
	if cfg.Model != "p" || srcs["model"].Src != SrcProjectFile {
		t.Fatalf("model=%q src=%v, want p from the project file", cfg.Model, srcs["model"])
	}

	// Remove the project file and the user file is what is left.
	os.Remove(projFile)
	cfg, _, srcs = loadConfigWithSources()
	if cfg.Model != "u" || srcs["model"].Src != SrcUserFile {
		t.Fatalf("model=%q src=%v, want u from ~/.lca", cfg.Model, srcs["model"])
	}

	// Remove both and the literal in defaultConfig() is in force.
	os.Remove(userFile)
	cfg, _, srcs = loadConfigWithSources()
	if cfg.Model != "local" || srcs["model"].Src != SrcDefault {
		t.Fatalf("model=%q src=%v, want the default", cfg.Model, srcs["model"])
	}

	// $LCA_CONFIG is read last of the files, so it beats the project file.
	os.WriteFile(projFile, []byte(`{"model":"p"}`), 0o644)
	extra := filepath.Join(t.TempDir(), "extra.json")
	os.WriteFile(extra, []byte(`{"model":"x"}`), 0o644)
	t.Setenv("LCA_CONFIG", extra)
	cfg, _, srcs = loadConfigWithSources()
	if cfg.Model != "x" || srcs["model"].Src != SrcExtraFile {
		t.Fatalf("model=%q src=%v, want x from $LCA_CONFIG", cfg.Model, srcs["model"])
	}
}

// A file that relocates the directory it was found in is a paradox, and a file
// that lifts the sandbox for every future run in a directory is a foot-gun.
func TestLocatorsAndUnsafeAreNotFileConfigurable(t *testing.T) {
	root, _ := layerRoot(t)
	elsewhere := t.TempDir()
	os.WriteFile(filepath.Join(root, ".lca", "config.json"),
		[]byte(`{"root":"`+elsewhere+`","dir":"`+elsewhere+`","unsafe":true}`), 0o644)
	cfg, _, _ := loadConfigWithSources()
	if cfg.Root != root {
		t.Errorf("a config file relocated the root to %q", cfg.Root)
	}
	if strings.HasPrefix(cfg.Dir, elsewhere) {
		t.Errorf("a config file relocated the config dir to %q", cfg.Dir)
	}
	if cfg.Unsafe {
		t.Error("a config file lifted the sandbox")
	}
	for _, k := range []string{"root", "dir"} {
		if err := applySetting(&cfg, k, elsewhere); err == nil {
			t.Errorf("applySetting(%q) was accepted", k)
		} else if !strings.Contains(err.Error(), k) {
			t.Errorf("the refusal must name %q: %v", k, err)
		}
	}
	if findSetting("unsafe") != nil {
		t.Error("unsafe must not be in the setting table at all")
	}
}

// replFor builds a Repl over a harness with an empty-but-layered config, for the
// /config, /set and /save screens.
func replFor(t *testing.T, fs *fakeServer, roles string) (*Repl, *harness) {
	t.Helper()
	h := newRoleHarness(t, fs, roles, false)
	cfg, _, srcs := loadConfigWithSources()
	cfg.Root, cfg.Dir = h.orch.jl.Root, h.orch.cfg.Dir
	cfg.BaseURL, cfg.Endpoints = fs.URL, []string{fs.URL}
	h.orch.cfg = cfg
	return &Repl{cfg: cfg, orch: h.orch, sess: h.sess, local: h.orch.providers.local,
		in: newStringInput(""), cfgSrc: srcs}, h
}

func TestConfigShowsEveryKeyWithSource(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, _ := replFor(t, fs, testRoles)
	r.cfg.APIKey = "sk-a-very-real-looking-secret-value-9f2"
	out := captureStdout(t, func() { r.cmdConfig("") })
	plain := stripANSI(out)
	for _, s := range settings {
		if !strings.Contains(plain, s.Key) {
			t.Errorf("/config never lists %q", s.Key)
		}
	}
	if strings.Contains(plain, "a-very-real-looking-secret") {
		t.Fatalf("/config printed the key:\n%s", plain)
	}
	if !strings.Contains(plain, "read-only") {
		t.Errorf("the locators must be shown read-only:\n%s", plain)
	}
}

func TestSetRefusesPlaintextKeyByDefault(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	target := filepath.Join(h.orch.jl.Root, ".lca", "config.json")

	out := captureStdout(t, func() { r.cmdSet("api_key sk-realsecret") })
	if _, err := os.Stat(target); err == nil {
		t.Fatal("/set api_key wrote a file")
	}
	if !strings.Contains(out, "api_key_env") {
		t.Errorf("the refusal must name api_key_env:\n%s", out)
	}

	out = captureStdout(t, func() { r.cmdSet("api_key sk-realsecret -plaintext") })
	if st, err := os.Stat(target); err != nil {
		t.Fatalf("-plaintext did not write: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode().Perm())
	}
	if !strings.Contains(out, "plain text") || !strings.Contains(stripANSI(out), "config.json") {
		t.Errorf("the write must name the path and the exposure:\n%s", out)
	}
	if strings.Contains(readRecorder(t, h), "sk-realsecret") {
		t.Error("the audit log recorded the literal key")
	}

	t.Setenv("LCA_API_KEY", "sk-from-the-env")
	captureStdout(t, func() { r.cmdSet("api_key_env LCA_API_KEY") })
	out = captureStdout(t, func() { r.cmdConfig("") })
	if !strings.Contains(stripANSI(out), "from $LCA_API_KEY") {
		t.Errorf("/config must say where the key came from:\n%s", stripANSI(out))
	}
}

// readRecorder is the audit log, where Recorder.Event writes: the transcript
// (SessionPath) holds messages, not events.
func readRecorder(t *testing.T, h *harness) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.orch.cfg.stateDir(), "audit.jsonl"))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestSetWarnsWhenEnvShadowsTheWrite(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	t.Setenv("LCA_BASE_URL", "http://from-the-shell:9/v1")
	out := captureStdout(t, func() { r.cmdSet("endpoint http://written:8000/v1") })
	data, err := os.ReadFile(filepath.Join(h.orch.jl.Root, ".lca", "config.json"))
	if err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	if !strings.Contains(string(data), "http://written:8000/v1") {
		t.Fatalf("base_url not in the file:\n%s", data)
	}
	if !strings.Contains(out, "LCA_BASE_URL") {
		t.Errorf("the warning must name the variable that wins:\n%s", out)
	}
}

func TestSetRejectsUnknownAndBadValues(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	target := filepath.Join(h.orch.jl.Root, ".lca", "config.json")
	for _, arg := range []string{"nope 1", "transport sideways", "temperature hot", "context -3", "steps 0"} {
		out := captureStdout(t, func() { r.cmdSet(arg) })
		// the refusal must be MARKED as a failure, not merely mentioned — but the
		// mark is the theme's, so the theme's own glyph is what is looked for
		if !strings.Contains(stripANSI(out), gDown+" ") {
			t.Errorf("/set %s was accepted:\n%s", arg, out)
		}
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("/set %s wrote a file", arg)
		}
	}
}

func TestSaveWritesOnlyUnsavedAndClearsThem(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)

	captureStdout(t, func() { r.cmdModel("coder-b") })
	if _, ok := r.unsaved["model"]; !ok {
		t.Fatalf("/model left nothing unsaved: %v", r.unsaved)
	}
	out := captureStdout(t, func() { r.cmdConfig("") })
	if !strings.Contains(out, "unsaved") {
		t.Errorf("/config must mark a session value unsaved:\n%s", stripANSI(out))
	}
	out = captureStdout(t, func() { r.exitNotice() })
	if !strings.Contains(out, "model") {
		t.Errorf("the /exit notice must name the key:\n%s", out)
	}

	// A team is loaded, and a top-level model key outranks every role's chain at the
	// next start — /save refuses and points at /role, and -force is the way past it.
	out = captureStdout(t, func() { r.cmdSave("") })
	if !strings.Contains(stripANSI(out), "/role") {
		t.Fatalf("/save must refuse to pin a team's model and name /role:\n%s", stripANSI(out))
	}
	target := filepath.Join(h.orch.jl.Root, ".lca", "config.json")
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the refused /save wrote a file")
	}
	out = captureStdout(t, func() { r.cmdSave("-force") })
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("/save wrote nothing: %v", err)
	}
	var got map[string]any
	json.Unmarshal(data, &got)
	if got["model"] != "coder-b" {
		t.Fatalf("model not saved: %v", got)
	}
	if len(r.unsaved) != 0 {
		t.Errorf("/save must clear what it wrote: %v", r.unsaved)
	}
	out = captureStdout(t, func() { r.cmdSave("") })
	if !strings.Contains(out, "nothing to save") {
		t.Errorf("a second /save should say there is nothing:\n%s", out)
	}

	// -user targets the other file, and an unknown key writes nothing at all.
	captureStdout(t, func() { r.cmdModel("coder-a") })
	captureStdout(t, func() { r.cmdSave("-user -force") })
	if _, err := os.Stat(filepath.Join(r.cfg.Dir, "config.json")); err != nil {
		t.Errorf("/save -user did not write ~/.lca/config.json: %v", err)
	}
	before, _ := os.ReadFile(target)
	out = captureStdout(t, func() { r.cmdSave("nosuchkey") })
	if !strings.Contains(out, "nosuchkey") {
		t.Errorf("the refusal must name the key:\n%s", out)
	}
	after, _ := os.ReadFile(target)
	if string(before) != string(after) {
		t.Error("/save nosuchkey wrote something")
	}
}

// A setting is only real if a later session obeys it. approve was the one that
// could be written and then ignored.
func TestPersistedSettingsReachANewSession(t *testing.T) {
	root, home := layerRoot(t)
	os.WriteFile(filepath.Join(root, ".lca", "config.json"), []byte(`{
  "approve": "run",
  "thinking": "high",
  "max_steps": 7,
  "temperature": 0.4,
  "loop": true,
  "allow": ["go", "git"]
}`), 0o644)
	_ = home
	cfg, _, _ := loadConfigWithSources()
	if cfg.Approve != "run" || cfg.Thinking != "high" || cfg.MaxSteps != 7 ||
		cfg.Temperature != 0.4 || !cfg.Loop || strings.Join(cfg.Allowed, ",") != "go,git" {
		t.Fatalf("the file did not reach Config: %+v", cfg)
	}
	ap := NewApprover(newStringInput(""))
	applyApproveTo(ap, cfg.Approve)
	if !ap.Trusts("run") {
		t.Error("approve: run in the file did not reach the Approver")
	}
	if ap.Trusts("edit") {
		t.Error("approve: run must not trust edits too")
	}
	// "unset" is a value, and it must survive a round trip as the absence of a key.
	c := cfg
	if err := applySetting(&c, "temperature", "unset"); err != nil {
		t.Fatal(err)
	}
	if c.Temperature != -1 {
		t.Fatalf("temperature unset = %v, want -1 (send nothing)", c.Temperature)
	}
	if v, err := jsonValueOf(c, "temperature"); err != nil || v != nil {
		t.Fatalf("an unset temperature must delete its key, got %v, %v", v, err)
	}
}

// The literal api_key must not appear in ANY command's output. Three paths
// printed it in the clear: /config <key> and /set <key> (both through
// explainSetting's "in file" row), and /config's env-shadow hint — each a line or
// two under maskKey's own masked value, on a screen that gets shared and recorded.
func TestConfigNeverPrintsTheStoredKey(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	const secret = "sk-realsecret-do-not-print-1234"
	captureStdout(t, func() { r.cmdSet("api_key " + secret + " -plaintext") })
	data, err := os.ReadFile(filepath.Join(h.orch.jl.Root, ".lca", "config.json"))
	if err != nil || !strings.Contains(string(data), secret) {
		t.Fatalf("-plaintext did not write the key: %v\n%s", err, data)
	}
	for _, screen := range []struct {
		what string
		fn   func()
	}{
		{"/config api_key", func() { r.cmdConfig("api_key") }},
		{"/set api_key", func() { r.cmdSet("api_key") }},
		{"/config", func() { r.cmdConfig("") }},
	} {
		out := stripANSI(captureStdout(t, screen.fn))
		if strings.Contains(out, secret) {
			t.Errorf("%s printed the stored key:\n%s", screen.what, out)
		}
	}
	// And with the environment shadowing the file, the hint that names the shadowed
	// file value must mask it too.
	t.Setenv("LCA_API_KEY", "sk-from-the-env")
	r.cfgSrc["api_key"] = settingSource{Src: SrcEnv, Path: "LCA_API_KEY"}
	out := stripANSI(captureStdout(t, func() { r.cmdConfig("") }))
	if strings.Contains(out, secret) {
		t.Errorf("the env-shadow hint printed the stored key:\n%s", out)
	}
}

// api_key_env naming a variable nobody exported is not a configured key, and
// defaultConfig()'s sk-noauth placeholder is not one either. Both were shown as
// "set", which is how an operator gets a 401 having been told twice it was fine.
func TestMaskKeyTellsTheTruthAboutUnsetAndPlaceholder(t *testing.T) {
	t.Setenv("LCA_TEST_KEY_VAR", "")
	os.Unsetenv("LCA_TEST_KEY_VAR")
	if got := stripANSI(maskKey(noAuthKey, "")); !strings.Contains(got, "none") || strings.Contains(got, "chars") {
		t.Errorf("the sk-noauth placeholder must not read as a configured key: %q", got)
	}
	if got := stripANSI(maskKey(noAuthKey, "LCA_TEST_KEY_VAR")); !strings.Contains(got, "not set in this shell") {
		t.Errorf("an unset api_key_env must say so: %q", got)
	}
	t.Setenv("LCA_TEST_KEY_VAR", "sk-yes")
	if got := stripANSI(maskKey("sk-yes", "LCA_TEST_KEY_VAR")); !strings.Contains(got, "from $LCA_TEST_KEY_VAR") {
		t.Errorf("a resolved api_key_env must name the variable: %q", got)
	}
}

// /set api_key_env warns when the variable is not exported — the wizard has always
// said this and /set reported plain success.
func TestSetApiKeyEnvWarnsWhenUnset(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, _ := replFor(t, fs, testRoles)
	os.Unsetenv("MY_GW_KEY_NOT_EXPORTED")
	out := stripANSI(captureStdout(t, func() { r.cmdSet("api_key_env MY_GW_KEY_NOT_EXPORTED") }))
	if !strings.Contains(out, "not set in this shell") {
		t.Errorf("/set api_key_env must warn about an unset variable:\n%s", out)
	}
}

// A team is loaded: a top-level model key outranks every role's chain at the next
// start, so the banner and /agents disagree for ever. /set and /save refused
// nothing and printed SAVED.
func TestSetModelRefusesToPinATeam(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	target := filepath.Join(h.orch.jl.Root, ".lca", "config.json")
	out := stripANSI(captureStdout(t, func() { r.cmdSet("model coder-b") }))
	if !strings.Contains(out, "/role") {
		t.Errorf("the refusal must point at /role:\n%s", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("/set model wrote a model key over a team")
	}
	out = stripANSI(captureStdout(t, func() { r.cmdSet("model coder-b -force") }))
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("-force must write it anyway: %v\n%s", err, out)
	}
}

// /set -user writes the file that is read FIRST, and the project file wins at the
// next start. That was silent, unlike the env case.
func TestSetUserWarnsWhenTheProjectFileWins(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	os.MkdirAll(filepath.Join(h.orch.jl.Root, ".lca"), 0o755)
	if err := os.WriteFile(filepath.Join(h.orch.jl.Root, ".lca", "config.json"), []byte(`{"max_tokens": 1111}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.reloadFileConfig()
	out := stripANSI(captureStdout(t, func() { r.cmdSet("-user max_tokens 2222") }))
	if !strings.Contains(out, "wins at the next start") {
		t.Errorf("a shadowed -user write must say so:\n%s", out)
	}
	if !strings.Contains(out, "1111") {
		t.Errorf("the warning must quote the value that wins:\n%s", out)
	}
}

// /set model with a hosted ref and no key: cmdModel prints ✕ and leaves the
// session where it was, and applyLive used to discard that and write the bad ref.
func TestSetModelDoesNotPersistARefusedModel(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	for _, p := range r.orch.providers.Sorted() {
		if p.apiKey() != "" {
			continue
		}
		before, cfgBefore := r.sess.client.Ref(), r.cfg.Model
		// -force only gets past the team guard; the hosted switch still fails for want
		// of a key, which is what this is about.
		out := stripANSI(captureStdout(t, func() { r.cmdSet("model " + p.ID + "/some-model -force") }))
		if strings.Contains(out, "wrote ") {
			t.Fatalf("/set model persisted a model the session refused:\n%s", out)
		}
		if got := r.sess.client.Ref(); got != before {
			t.Errorf("the session moved anyway: %q → %q", before, got)
		}
		if r.cfg.Model != cfgBefore {
			t.Errorf("cfg.Model kept the refused ref: %q (was %q)", r.cfg.Model, cfgBefore)
		}
		if _, err := os.Stat(filepath.Join(h.orch.jl.Root, ".lca", "config.json")); err == nil {
			t.Error("a refused /set model wrote a file")
		}
		return
	}
	t.Skip("every provider has a key in this environment")
}

// /set tells the operator to /save something it is about to write two lines later,
// and announced the change twice.
func TestSetDoesNotAdviseSavingWhatItWrites(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, _ := replFor(t, fs, testRoles)
	out := stripANSI(captureStdout(t, func() { r.cmdSet("endpoint " + fs.URL) }))
	if strings.Contains(out, "/save keeps it") {
		t.Errorf("/set must not advise /save for a value it persists:\n%s", out)
	}
	if len(r.unsaved) != 0 {
		t.Errorf("/set left an unsaved entry for a written value: %v", r.unsaved)
	}
}

// SrcRoles was declared with two render arms and nothing ever assigning it, so
// /config credited defaultConfig() for the model and the tier — the two settings an
// operator most wants sourced — on the screen whose entire purpose is to name the
// source of every setting.
func TestConfigNamesTheTeamFileAsTheModelsSource(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	markRoleSources(r.cfgSrc, h.orch.jl.Root, h.orch.roles, r.sess.agent)
	if got := r.cfgSrc["model"].Src; got != SrcRoles {
		t.Fatalf("model's source is %v, want SrcRoles", got)
	}
	out := stripANSI(captureStdout(t, func() { r.cmdConfig("") }))
	chain := r.sess.agent.Models
	if len(chain) == 0 {
		t.Fatal("the entry role has no chain in this fixture")
	}
	if !strings.Contains(out, "roles.yaml") {
		t.Errorf("/config must name the team file:\n%s", out)
	}
	if !strings.Contains(out, chain[0]) {
		t.Errorf("/config must show the role's own chain (%v), not defaultConfig()'s literal:\n%s", chain, out)
	}
	// And a file or an env var still outranks the team, so the source must say so.
	r.cfgSrc["model"] = settingSource{Src: SrcEnv, Path: "LCA_MODEL"}
	out = stripANSI(captureStdout(t, func() { r.cmdConfig("model") }))
	if !strings.Contains(out, "LCA_MODEL") {
		t.Errorf("a higher layer must keep the credit:\n%s", out)
	}
}

// One long synopsis used to pad /help's name column to 50 characters and wrap all
// twenty-five descriptions at 80 columns.
func TestHelpKeepsItsColumnNarrow(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, _ := replFor(t, fs, testRoles)
	out := stripANSI(captureStdout(t, func() { r.cmdHelp("all") }))
	// The column is padded to the widest synopsis, so ONE long one taxes every other
	// row. It may not push the descriptions past column 40, whatever is in the
	// registry, and a synopsis over the cap keeps its args on a line of its own.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "    /") {
			continue
		}
		name, rest, ok := strings.Cut(strings.TrimLeft(line, " "), "  ")
		if !ok {
			continue
		}
		if col := len(line) - len(strings.TrimLeft(rest, " ")); col > 40 {
			t.Errorf("a /help description starts at column %d (name %q):\n%s", col, name, line)
		}
	}
	// And the flags of the longest one are still reachable.
	if !strings.Contains(out, "/eval") || !strings.Contains(out, "<dir>") {
		t.Errorf("/eval must still show what it takes:\n%s", out)
	}
}
