package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A decode/re-encode writer would shuffle the permission block, and order IS
// precedence there (last match wins) — so this is the regression the
// RawMessage design exists to prevent, and it must not be weakened.
func TestConfigWriterPreservesUnknownBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := `{
  "model": "m1",
  "providers": {"gpu2": {"base_url": "http://gpu2:8000/v1", "tools": "native"}},
  "agents": {"explore": {"model": "x", "steps": 30}},
  "permission": {"run": {"*": "ask", "git status *": "allow"}, "web": "deny"},
  "a_key_this_binary_never_heard_of": {"nested": [1, 2, 3]}
}`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := setConfigValues(path, map[string]any{"model": "m2"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("the writer produced invalid JSON: %v\n%s", err, data)
	}
	if string(got["model"]) != `"m2"` {
		t.Fatalf("model = %s", got["model"])
	}
	var before map[string]json.RawMessage
	json.Unmarshal([]byte(orig), &before)
	for _, k := range []string{"providers", "agents", "a_key_this_binary_never_heard_of"} {
		var a, b any
		json.Unmarshal(before[k], &a)
		json.Unmarshal(got[k], &b)
		if !jsonEqual(a, b) {
			t.Errorf("%s was rewritten:\n before %s\n after  %s", k, before[k], got[k])
		}
	}
	// Order is precedence: the ruleset must come back in the same order.
	t.Setenv("HOME", dir)
	t.Setenv("LCA_ROLES", "")
	fc, err := loadFileConfig(Config{Dir: dir, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var pats []string
	for _, r := range fc.Permission {
		if r.Permission == "run" {
			pats = append(pats, r.Pattern)
		}
	}
	if len(pats) != 2 || pats[0] != "*" || pats[1] != "git status *" {
		t.Fatalf("permission order lost: %v — precedence depends on it", pats)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestConfigWriterIsAtomicAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := `{"model":"old","keep":true}`
	os.WriteFile(path, []byte(orig), 0o644)

	if _, err := setConfigValues(path, map[string]any{"model": "new"}); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != orig {
		t.Fatalf(".bak = %q, %v; want the previous bytes exactly", bak, err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") || e.Name() == "config.lock" {
			t.Errorf("left behind %s", e.Name())
		}
	}
	first, _ := os.ReadFile(path)
	if err := json.Unmarshal(first, &map[string]any{}); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, err := setConfigValues(path, map[string]any{"model": "new"}); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("the writer is not deterministic:\n%s\n---\n%s", first, second)
	}
}

func TestConfigWriterModeDependsOnSecret(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "config.json")
	if _, err := setConfigValues(plain, map[string]any{"model": "m"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(plain); st.Mode().Perm() != 0o644 {
		t.Errorf("a file with no key is %v, want 0644", st.Mode().Perm())
	}
	if _, err := setConfigValues(plain, map[string]any{"api_key": "sk-x"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(plain); st.Mode().Perm() != 0o600 {
		t.Errorf("adding api_key left the file at %v, want 0600", st.Mode().Perm())
	}
}

func TestConfigLockKeepsTwoProcessesApart(t *testing.T) {
	dir := t.TempDir()
	// A live holder: another pid that exists (pid 1 always does).
	os.WriteFile(filepath.Join(dir, "config.lock"), []byte("1\n"), 0o600)
	if _, err := lockConfig(dir); err == nil {
		t.Fatal("a live pid must hold the lock")
	} else if !strings.Contains(err.Error(), "pid 1") {
		t.Fatalf("the refusal must name the holding pid: %v", err)
	}
	// A dead holder: a crashed session must not leave a project unconfigurable.
	os.WriteFile(filepath.Join(dir, "config.lock"), []byte("2147480000\n"), 0o600)
	out := captureStdout(t, func() {
		unlock, err := lockConfig(dir)
		if err != nil {
			t.Errorf("a dead pid must be taken over: %v", err)
			return
		}
		unlock()
	})
	if !strings.Contains(out, "2147480000") {
		t.Errorf("the takeover must name the dead pid: %q", out)
	}
}

func TestConfigWriterMergesConcurrentKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	var wg sync.WaitGroup
	for _, kv := range []map[string]any{{"model": "m"}, {"approve": "run"}} {
		wg.Add(1)
		go func(v map[string]any) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if _, err := setConfigValues(path, v); err != nil {
					// A momentary collision on the lock is fine; a lost key is not.
					continue
				}
			}
		}(kv)
	}
	wg.Wait()
	data, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("invalid JSON after concurrent writes: %v\n%s", err, data)
	}
	if got["model"] != "m" || got["approve"] != "run" {
		t.Fatalf("two writers clobbered per file instead of merging per key: %v", got)
	}
}

func TestConfigDeleteRemovesKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"model":"m","approve":"run"}`), 0o644)
	if _, err := setConfigValues(path, map[string]any{"model": nil}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &got)
	if _, ok := got["model"]; ok {
		t.Fatal("a nil value must delete the key")
	}
	if got["approve"] != "run" {
		t.Fatalf("the rest was disturbed: %v", got)
	}
}

// The backup beside config.json carries whatever the config carried. os.WriteFile
// applies its perm argument only to a file it CREATES, so once a .bak existed at
// 0644 from a key-free write, the next write copied a key-bearing config into it
// world-readable — 0600 on config.json, 0600 in the message on screen, 0644 on the
// bytes lying next to it.
func TestConfigBackupNeverLooserThanTheConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	bak := path + ".bak"
	for _, step := range []map[string]any{
		{"base_url": "http://gw:8000/v1"},
		{"model": "m1"},
		{"api_key": "sk-supersecret"},
		{"model": "m2"},
		{"model": "m3"},
	} {
		if _, err := setConfigValues(path, step); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("no backup: %v", err)
	}
	if !strings.Contains(string(data), "sk-supersecret") {
		t.Fatalf("this test needs a key-bearing backup to be meaningful:\n%s", data)
	}
	st, err := os.Stat(bak)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("config.json.bak holds the key at mode %v, want 0600", st.Mode().Perm())
	}
	main, _ := os.Stat(path)
	if main.Mode().Perm() != 0o600 {
		t.Errorf("config.json mode %v, want 0600", main.Mode().Perm())
	}
}

// writeFileMode must chmod a file that is already there — the whole point.
func TestWriteFileModeTightensAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("loose"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileMode(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode().Perm())
	}
}

// lockDirs takes one lock per distinct directory and releases them all, in either
// order of arguments — /setup writes roles.yaml and config.json into two
// directories when the scope says so, and used to hold only one of the locks.
func TestLockDirsTakesBothAndReleasesBoth(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	unlock, err := lockDirs(b, a, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{a, b} {
		if _, err := os.Stat(filepath.Join(d, "config.lock")); err != nil {
			t.Errorf("no lock taken in %s: %v", d, err)
		}
	}
	unlock()
	for _, d := range []string{a, b} {
		if _, err := os.Stat(filepath.Join(d, "config.lock")); err == nil {
			t.Errorf("the lock in %s was not released", d)
		}
	}
	// And it can be taken again afterwards, which is what "released" has to mean.
	unlock2, err := lockDirs(a, b)
	if err != nil {
		t.Fatalf("the directories stayed locked: %v", err)
	}
	unlock2()
}
