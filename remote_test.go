package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// localRemote is a Remote whose transport runs the script locally, so the whole
// remote path (quoting, cd into the project, stdin, exit codes) is exercised
// without an ssh server.
func localRemote(dir string) *Remote {
	return &Remote{Host: "", Dir: dir, SSH: []string{"sh", "-c"}}
}

func TestRemoteFileTools(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte("package pkg\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pkg", "sub", "b_test.go"), []byte("package sub\n"), 0o644)
	rem := localRemote(dir)
	ctx := context.Background()

	if got := rem.readFile(ctx, "pkg/a.go", ""); !strings.Contains(got, "func Add") {
		t.Fatalf("read: %q", got)
	}
	if got := rem.readFile(ctx, "pkg/a.go", "1-1"); !strings.Contains(got, "package pkg") || strings.Contains(got, "func Add") {
		t.Fatalf("read range: %q", got)
	}
	if got := rem.listDir(ctx, "."); !strings.Contains(got, "main.go") || !strings.Contains(got, "pkg") {
		t.Fatalf("list: %q", got)
	}
	if got := rem.glob(ctx, "**/*.go", ""); !strings.Contains(got, "pkg/a.go") || !strings.Contains(got, "main.go") {
		t.Fatalf("glob: %q", got)
	}
	if got := rem.glob(ctx, "*_test.go", ""); got != "pkg/sub/b_test.go" {
		t.Fatalf("basename glob: %q", got)
	}
	if got := rem.grep(ctx, "func Add", "", "*.go", nil); !strings.Contains(got, "pkg/a.go:3") {
		t.Fatalf("grep: %q", got)
	}
	if got := rem.grep(ctx, "nothinghere", "", "", nil); got != "no matches" {
		t.Fatalf("grep empty: %q", got)
	}
	// permission filter drops files the rules don't allow
	if got := rem.grep(ctx, "package", "", "", func(f string) bool { return !strings.HasPrefix(f, "pkg/") }); strings.Contains(got, "pkg/") {
		t.Fatalf("grep ignored the read rules: %q", got)
	}
	if errs := rem.write(ctx, "new/dir/file.txt", "hello\n"); errs != "" {
		t.Fatal(errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "new", "dir", "file.txt")); string(b) != "hello\n" {
		t.Fatalf("write: %q", b)
	}
	if _, ok := rem.stat(ctx, "new/dir/file.txt"); !ok {
		t.Fatal("stat should find the written file")
	}
	if _, ok := rem.stat(ctx, "nope.txt"); ok {
		t.Fatal("stat should not find a missing file")
	}
	out, exit := rem.run(ctx, "echo hi && pwd", 30*time.Second, nil, nil)
	if exit != 0 || !strings.Contains(out, "hi") {
		t.Fatalf("run: %q exit %d", out, exit)
	}
	if _, exit := rem.run(ctx, "exit 3", 30*time.Second, nil, nil); exit != 3 {
		t.Fatalf("exit code not propagated: %d", exit)
	}
}

func TestRemotePathStaysInProject(t *testing.T) {
	rem := localRemote("/home/u/proj")
	for _, p := range []string{"../etc/passwd", "/etc/passwd", "~/secrets", "sub/../../out"} {
		if _, err := rem.relPath(p); err == nil {
			t.Errorf("%q should be refused", p)
		}
	}
	for in, want := range map[string]string{"": ".", "a/b.go": "a/b.go", "./a/b.go": "a/b.go", "/home/u/proj/a/b.go": "a/b.go"} {
		got, err := rem.relPath(in)
		if err != nil || got != want {
			t.Errorf("relPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// a path with a quote can't break out of the shell command
	dir := t.TempDir()
	r2 := localRemote(dir)
	os.WriteFile(filepath.Join(dir, "we'ird.txt"), []byte("ok\n"), 0o644)
	if got := r2.readFile(context.Background(), "we'ird.txt", ""); !strings.Contains(got, "ok") {
		t.Fatalf("quoting: %q", got)
	}
}

func TestRemoteSessionEditAndVerify(t *testing.T) {
	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "sum.py"), []byte("def total(xs):\n    return sum(x for x in xs if x > 0)\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "check.sh"), []byte("#!/bin/sh\ngrep -q 'if x > 0' sum.py && exit 1\nexit 0\n"), 0o755)

	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		switch {
		case strings.Contains(req.Body, `"role":"tool"`) && strings.Count(req.Body, `"role":"tool"`) == 1:
			return fakeReply{calls: []ToolCall{call("e", "edit", map[string]any{
				"path": "sum.py", "old_string": "sum(x for x in xs if x > 0)", "new_string": "sum(xs)"})}}
		case strings.Contains(req.Body, `"role":"tool"`):
			return fakeReply{content: "fixed"}
		}
		return fakeReply{calls: []ToolCall{call("r", "read_file", map[string]any{"path": "sum.py"})}}
	})
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "remote:\n  host: \"\"\n  dir: "+proj+"\n  ssh: [sh, -c]\nsandbox:\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	if h.orch.remote == nil || h.orch.remote.Dir != proj {
		t.Fatalf("remote not configured: %+v", h.orch.remote)
	}
	// delegate is withheld: its worktree would be local
	for _, tl := range func() []ToolSchema { _, sc := h.sess.tools(); return sc }() {
		if tl.Function.Name == "delegate" {
			t.Fatal("delegate must be hidden in remote mode")
		}
	}
	if err := h.run(t, "fix total()"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(proj, "sum.py")); !strings.Contains(string(b), "sum(xs)") {
		t.Fatalf("the remote file was not edited: %q", b)
	}
	if !strings.Contains(h.sess.Msgs[0].Content, "on another machine") {
		t.Fatal("the system prompt must say where the project lives")
	}
	// the verifier runs on the remote too
	v := h.sess.RunVerifiedAll(context.Background(), []string{"sh check.sh"}, 1)
	if v.Status != "passed" || !v.Checked {
		t.Fatalf("verify on remote: %+v", v)
	}
}

func TestRemoteEditRequiresReadFirst(t *testing.T) {
	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "a.txt"), []byte("one\n"), 0o644)
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{calls: []ToolCall{call("e", "edit", map[string]any{"path": "a.txt", "old_string": "one", "new_string": "two"})}}
		}
		return fakeReply{content: "ok"}
	})
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "remote:\n  host: \"\"\n  dir: "+proj+"\n  ssh: [sh, -c]\nsandbox:\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	h.run(t, "edit it")
	if b, _ := os.ReadFile(filepath.Join(proj, "a.txt")); string(b) != "one\n" {
		t.Fatalf("an unread remote file was edited: %q", b)
	}
	var res string
	for _, m := range h.sess.Msgs {
		if m.Role == "tool" {
			res = m.Content
		}
	}
	if !strings.Contains(res, "has not been read") {
		t.Fatalf("want the read-first guard, got %q", res)
	}
}

func TestSSHCannotSmuggleGPUWork(t *testing.T) {
	j, _ := NewJail(t.TempDir(), []string{"ssh", "rsync", "bsk", "go"}, false)
	for cmd, ok := range map[string]bool{
		"ssh node ls -la":                          true,
		"ssh -o BatchMode=yes node go test ./...":  true,
		"ssh node srun -G 1 python train.py":       false,
		"ssh -p 22 node torchrun --nproc 8 t.py":   false,
		"ssh node CUDA_VISIBLE_DEVICES=0 python x": false,
		"ssh node bsk submit -g 1 -- python t.py":  true,
		"ssh node bsk gw drain m":                  false,
		"rsync -a ./ node:/tmp/x":                  true,
	} {
		if err := j.CheckCommand(cmd); (err == nil) != ok {
			t.Errorf("%q: allowed=%v want %v (%v)", cmd, err == nil, ok, err)
		}
	}
}

func TestRemoteConfigFromEnv(t *testing.T) {
	t.Setenv("LCA_REMOTE", "cab-node:/home/u/llmbench")
	r, err := parseRemote(nil)
	if err != nil || r == nil || r.Host != "cab-node" || r.Dir != "/home/u/llmbench" {
		t.Fatalf("env remote: %+v %v", r, err)
	}
	if got := strings.Join(r.argv("x"), " "); !strings.HasPrefix(got, "ssh -o BatchMode=yes") || !strings.Contains(got, "cab-node") {
		t.Fatalf("argv: %q", got)
	}
	t.Setenv("LCA_REMOTE", "no-colon")
	if _, err := parseRemote(nil); err == nil {
		t.Fatal("host:/path is required")
	}
	t.Setenv("LCA_REMOTE", "host:relative/path")
	if _, err := parseRemote(nil); err == nil {
		t.Fatal("the remote dir must be absolute")
	}
	_ = json.Marshal
}
