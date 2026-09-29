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

// fakeSSH puts an executable named "ssh" on PATH that appends its own argv to a
// log and then runs the trailing script here, in place of the far side: member
// routing is asserted by EXACT ARGV, with no sshd and no network, and the
// "remote" side is a real directory with real git. Three switches make the
// failures testable too:
//
//	$LCA_TEST_SSH_DOWN=<host>   that host exits 255 with a canned ssh error
//	$LCA_TEST_SSH_NOGIT=<host>  `command -v git` fails there
//	$LCA_TEST_SSH_BANNER=<text> the transport prints that to stdout first
//
// It sets PATH process-wide with t.Setenv, so these tests must not be parallel.
// The log is one record per call: fields separated by \034, and newlines inside
// an argument replaced by \036, so a multi-line script stays one record.
func fakeSSH(t *testing.T) func() [][]string {
	t.Helper()
	bin := t.TempDir()
	empty := filepath.Join(bin, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(bin, "ssh.log")
	script := `#!/bin/sh
SEP=$(printf '\034')
n=$#
i=0
host=""
scr=""
rec=$(basename "$0")
for a in "$@"; do
  i=$((i+1))
  [ $i -eq $((n-1)) ] && host="$a"
  [ $i -eq $n ] && scr="$a"
  a=$(printf '%s' "$a" | tr '\n' '\036')
  rec="$rec$SEP$a"
done
printf '%s\n' "$rec" >> "$LCA_TEST_SSH_LOG"
case " $LCA_TEST_SSH_DOWN " in
  *" $host "*) echo "ssh: connect to host $host port 22: Operation timed out" >&2; exit 255;;
esac
case " $LCA_TEST_SSH_NOGIT " in
  *" $host "*) PATH="$LCA_TEST_SSH_EMPTY"; export PATH; exec /bin/sh -c "$scr";;
esac
[ -n "$LCA_TEST_SSH_BANNER" ] && printf '%s\n' "$LCA_TEST_SSH_BANNER"
exec /bin/sh -c "$scr"
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCA_TEST_SSH_LOG", log)
	t.Setenv("LCA_TEST_SSH_EMPTY", empty)
	t.Setenv("LCA_TEST_SSH_DOWN", "")
	t.Setenv("LCA_TEST_SSH_NOGIT", "")
	t.Setenv("LCA_TEST_SSH_BANNER", "")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() [][]string {
		data, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		var out [][]string
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			var argv []string
			for _, f := range strings.Split(line, "\034") {
				argv = append(argv, strings.ReplaceAll(f, "\036", "\n"))
			}
			out = append(out, argv)
		}
		return out
	}
}

// sshScripts is the trailing script of every logged call, which is what says
// WHAT ran on the far side.
func sshScripts(calls [][]string) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c[len(c)-1])
	}
	return out
}

func countScripts(calls [][]string, substr string) int {
	n := 0
	for _, s := range sshScripts(calls) {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

// forgetReach drops a member's cached reachability, so one test can watch a
// machine go down after it has already answered.
func forgetReach(m *Member) {
	if m == nil || m.Rem == nil {
		return
	}
	g := m.Rem.gate()
	g.mu.Lock()
	g.ok, g.err = false, nil
	g.mu.Unlock()
}

// loadMembersYAML runs a roles.yaml through the real loader.
func loadMembersYAML(t *testing.T, yaml string) (*RolesConfig, error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if os.Getenv("LCA_REMOTE") != "" {
		t.Setenv("LCA_REMOTE", "")
	}
	p := filepath.Join(dir, "roles.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCA_ROLES", p)
	return loadRoles(Config{Root: dir, Dir: filepath.Join(dir, "cfg")})
}

const memberRolesTail = `
roles:
  lead:
    models: [lead-a]
  coder:
    member: build-box
    models: [coder-a]
    check_cmd: ls done.txt
  cheap:
    models: [cheap-a]
`

func TestMembersParse(t *testing.T) {
	rc, err := loadMembersYAML(t, `entry: lead
members:
  local:
    allow: [go, git]
    shell: true
  build-box:
    host: build01
    dir: /srv/work/llmbench
    ssh: [-o, ConnectTimeout=5]
    allow: [go, git, make, bsk]
  cab: {host: cab, dir: /p}
defaults:
  member: build-box
sandbox:
  allow: [ls, cat]
`+memberRolesTail)
	if err != nil {
		t.Fatal(err)
	}
	if got := rc.MemberOrder; strings.Join(got, ",") != "local,build-box,cab" {
		t.Fatalf("MemberOrder = %v", got)
	}
	if l := rc.Members["local"]; l == nil || l.Rem != nil || strings.Join(l.Allow, ",") != "go,git" || l.Shell == nil || !*l.Shell {
		t.Fatalf("local member: %+v", l)
	}
	b := rc.Members["build-box"]
	if b == nil || b.Rem == nil {
		t.Fatal("build-box missing")
	}
	if b.Rem.Host != "build01" || b.Rem.Dir != "/srv/work/llmbench" || strings.Join(b.Rem.SSH, " ") != "-o ConnectTimeout=5" {
		t.Fatalf("build-box transport: %+v", b.Rem)
	}
	if strings.Join(b.Allow, ",") != "go,git,make,bsk" || b.Shell != nil {
		t.Fatalf("build-box sandbox: %v %v", b.Allow, b.Shell)
	}
	if c := rc.Members["cab"]; c == nil || c.Rem == nil || c.Rem.Host != "cab" || c.Rem.Dir != "/p" {
		t.Fatalf("flow-form member: %+v", c)
	}
	if rc.DefaultMember != "build-box" || !rc.HasMembers {
		t.Fatalf("defaults: %q hasMembers=%v", rc.DefaultMember, rc.HasMembers)
	}
}

func TestMemberConfigErrors(t *testing.T) {
	for _, c := range []struct{ name, yaml, want string }{
		{"bad name", "members:\n  Build Box:\n    host: h\n    dir: /p\n", `bad member name "Build Box"`},
		{"unknown key", "members:\n  build-box:\n    hosts: h\n    dir: /p\n", `members.build-box: unknown key "hosts"`},
		{"no host", "members:\n  build-box:\n    dir: /p\n", "host is required"},
		{"no dir", "members:\n  build-box:\n    host: build01\n", "dir is required (the project's path on build01)"},
		{"relative dir", "members:\n  build-box:\n    host: h\n    dir: work/proj\n", `dir must be absolute, got "work/proj"`},
		{"tilde dir", "members:\n  build-box:\n    host: h\n    dir: ~/proj\n", `dir must be an absolute path on h (~ is not expanded over ssh)`},
		{"local host", "members:\n  local:\n    host: h\n    dir: /p\n", "local always means this machine"},
		{"local empty", "members:\n  local:\n    allow:\n", "allow: is empty"},
		{"empty allow", "members:\n  build-box:\n    host: h\n    dir: /p\n    allow:\n", "allow: is empty"},
		{"bad shell", "members:\n  build-box:\n    host: h\n    dir: /p\n    shell: maybe\n", `shell must be true or false, got "maybe"`},
		{"flow ssh", "members:\n  build-box: {host: h, dir: /p, ssh: [-o, X]}\n", `ssh: "[-o" is not a list`},
		{"bad default", "members:\n  build-box:\n    host: h\n    dir: /p\ndefaults:\n  member: buildbox\n", `defaults: member: "buildbox" is not a member (members: local, build-box)`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadMembersYAML(t, c.yaml+"\nroles:\n  lead:\n    models: [lead-a]\n")
			if err == nil {
				t.Fatalf("want an error mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			if !strings.Contains(err.Error(), "roles.yaml") {
				t.Fatalf("every member error names the file: %v", err)
			}
		})
	}
	// A role pinned at a member nobody declared.
	_, err := loadMembersYAML(t, "members:\n  build-box:\n    host: h\n    dir: /p\nroles:\n  coder:\n    member: buildbox\n    models: [coder-a]\n")
	if err == nil || !strings.Contains(err.Error(), `role coder: member: "buildbox" is not a member (members: local, build-box)`) {
		t.Fatalf("role member error: %v", err)
	}
	// members.remote next to the old remote: block is one name for two things.
	_, err = loadMembersYAML(t, "remote:\n  host: r\n  dir: /r\nmembers:\n  remote:\n    host: h\n    dir: /p\nroles:\n  lead:\n    models: [lead-a]\n")
	if err == nil || !strings.Contains(err.Error(), "both define a member named") {
		t.Fatalf("collision error: %v", err)
	}
}

func TestRemoteBlockBecomesMemberRemote(t *testing.T) {
	rc, err := loadMembersYAML(t, "remote:\n  host: cab\n  dir: /home/u/proj\nroles:\n  lead:\n    models: [lead-a]\n  coder:\n    models: [coder-a]\n")
	if err != nil {
		t.Fatal(err)
	}
	m := rc.Members[legacyMemberName]
	if m == nil || m.Rem != rc.Remote {
		t.Fatalf("the remote: block must BE member remote: %+v", m)
	}
	if rc.DefaultMember != legacyMemberName || rc.HasMembers {
		t.Fatalf("legacy fleet: default=%q hasMembers=%v", rc.DefaultMember, rc.HasMembers)
	}
	for _, a := range rc.Roles {
		if got := rc.memberOf(a); got != legacyMemberName {
			t.Fatalf("role %s resolves to %q", a.Name, got)
		}
	}
	o := &Orchestrator{remote: rc.Remote, members: rc.Members, memOrd: rc.MemberOrder, defMem: rc.DefaultMember}
	if !o.legacyFleet() || o.canDelegate(o.member(legacyMemberName)) {
		t.Fatal("a legacy fleet must keep delegate off on its remote member")
	}
	if !o.canDelegate(o.localMember()) {
		t.Fatal("delegate stays on for local work")
	}
}

func TestLCARemoteStillWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	p := filepath.Join(dir, "roles.yaml")
	os.WriteFile(p, []byte("members:\n  remote:\n    host: old\n    dir: /old\n    allow: [ls]\n  box:\n    host: box\n    dir: /b\nroles:\n  lead:\n    models: [lead-a]\n"), 0o644)
	t.Setenv("LCA_ROLES", p)
	t.Setenv("LCA_REMOTE", "new-host:/new/dir")
	rc, err := loadRoles(Config{Root: dir, Dir: filepath.Join(dir, "cfg")})
	if err != nil {
		t.Fatal(err)
	}
	m := rc.Members[legacyMemberName]
	if m == nil || m.Rem.Host != "new-host" || m.Rem.Dir != "/new/dir" {
		t.Fatalf("LCA_REMOTE must override members.remote: %+v", m.Rem)
	}
	if strings.Join(m.Allow, ",") != "ls" {
		t.Fatal("the overridden member keeps its own sandbox")
	}
	if rc.DefaultMember != legacyMemberName {
		t.Fatalf("LCA_REMOTE becomes the default member, got %q", rc.DefaultMember)
	}
	o := &Orchestrator{remote: rc.Remote, members: rc.Members, memOrd: rc.MemberOrder, defMem: rc.DefaultMember, hasMembers: rc.HasMembers}
	if o.legacyFleet() {
		t.Fatal("a fleet that declares members: is not a legacy fleet")
	}
	var warned bool
	for _, w := range rc.Warnings {
		if strings.Contains(w, "LCA_REMOTE overrides members.remote") && strings.Contains(w, "new-host:/new/dir") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("the override must be warned about: %v", rc.Warnings)
	}
}

func TestMemberArgvPerRole(t *testing.T) {
	calls := fakeSSH(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dirA, "a.txt"), []byte("A\n"), 0o644)
	os.WriteFile(filepath.Join(dirB, "a.txt"), []byte("B\n"), 0o644)
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  build-box:\n    host: build01\n    dir: "+dirA+
		"\n  gpu-0:\n    host: gpu07\n    dir: "+dirB+"\n    ssh: [-p, 2222]\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: build-box\n", 1)
	roles += "  trainer:\n    member: gpu-0\n    models: [coder-a]\n    tools: [read_file]\n"
	h := newRoleHarness(t, fs, roles, true)
	os.WriteFile(filepath.Join(h.root, "a.txt"), []byte("LOCAL\n"), 0o644)

	read := func(role string) string {
		child, err := h.orch.newChild(h.sess, h.orch.agents[role], "x")
		if err != nil {
			t.Fatal(err)
		}
		child.view = &quietView{}
		tc := &ToolCtx{Ctx: context.Background(), S: child, Name: "read_file"}
		return toolRegistry["read_file"].Run(tc, Args{"path": "a.txt"})
	}
	if got := read("coder"); !strings.Contains(got, "A\n") {
		t.Fatalf("coder read the wrong machine: %q", got)
	}
	// One probe, the read itself, and the stat that records "the agent has seen
	// this file" (read-before-edit is per member, keyed on host+dir).
	got := calls()
	if len(got) != 3 {
		t.Fatalf("want probe + read + stat, got %d: %v", len(got), sshScripts(got))
	}
	wantProbe := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "build01", "cd '" + dirA + "' && pwd"}
	if strings.Join(got[0], "\x00") != strings.Join(wantProbe, "\x00") {
		t.Fatalf("probe argv:\n got %q\nwant %q", got[0], wantProbe)
	}
	if last := got[1]; len(last) != 7 || last[5] != "build01" || !strings.HasPrefix(last[6], "cd '"+dirA+"' && head -c ") {
		t.Fatalf("read argv: %q", last)
	}
	if got := read("trainer"); !strings.Contains(got, "B\n") {
		t.Fatalf("trainer read the wrong machine: %q", got)
	}
	gpu := calls()[3]
	if strings.Join(gpu[:8], " ") != "ssh -o BatchMode=yes -o ConnectTimeout=10 -p 2222 gpu07" {
		t.Fatalf("gpu-0 transport: %q", gpu)
	}
	before := len(calls())
	if got := read("lead"); !strings.Contains(got, "LOCAL\n") {
		t.Fatalf("an unpinned role must read THIS machine: %q", got)
	}
	if after := len(calls()); after != before {
		t.Fatalf("a local read opened %d ssh connections", after-before)
	}
	// One probe per member, however many tool calls follow.
	read("coder")
	read("coder")
	if n := countScripts(calls(), "&& pwd"); n != 2 {
		t.Fatalf("probes = %d, want one per member", n)
	}
}

func TestMemberUnreachableNeverFallsBack(t *testing.T) {
	calls := fakeSSH(t)
	t.Setenv("LCA_TEST_SSH_DOWN", "build01")
	boxDir := t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  build-box:\n    host: build01\n    dir: "+boxDir+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: build-box\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	child, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "x")
	if err != nil {
		t.Fatal(err)
	}
	child.view = &quietView{}

	res := child.runTool(context.Background(), "sh -c 'echo x > sentinel'", 30*time.Second, nil)
	for _, want := range []string{"member build-box", "is unreachable", "Operation timed out", "nothing runs on another machine instead"} {
		if !strings.Contains(res, want) {
			t.Fatalf("run_command on a down member: %q (want %q)", res, want)
		}
	}
	v := child.RunVerifiedAll(context.Background(), []string{"ls"}, 2)
	if v.Status != "error" || !strings.Contains(v.Tail, "member build-box") {
		t.Fatalf("an unreachable member is an error, not a failed check: %+v", v)
	}
	if len(fs.reqs()) != 0 {
		t.Fatalf("an unreachable member must cost no tokens, got %d requests", len(fs.reqs()))
	}
	for _, dir := range []string{h.root, boxDir, os.TempDir()} {
		if _, err := os.Stat(filepath.Join(dir, "sentinel")); err == nil {
			t.Fatalf("the command ran somewhere: %s", dir)
		}
	}
	if n := countScripts(calls(), "&& pwd"); n != 1 {
		t.Fatalf("a failed probe is cached: %d probes", n)
	}
}

func TestMemberReachCachedAndRetried(t *testing.T) {
	calls := fakeSSH(t)
	t.Setenv("LCA_TEST_SSH_DOWN", "down01")
	t.Setenv("LCA_MEMBER_PROBE", "5")
	up := &Remote{Name: "up", Host: "up01", Dir: t.TempDir()}
	down := &Remote{Name: "down", Host: "down01", Dir: t.TempDir()}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := up.ensureUp(ctx); err != nil {
			t.Fatal(err)
		}
		if err := down.ensureUp(ctx); err == nil {
			t.Fatal("a down member must stay down")
		}
	}
	if n := countScripts(calls(), "&& pwd"); n != 2 {
		t.Fatalf("five calls each, want one probe per member, got %d", n)
	}
	// A failure is re-probed once the retry window has passed, so a laptop that
	// joins the VPN mid-session recovers without a restart.
	g := down.gate()
	g.mu.Lock()
	g.last = time.Now().Add(-2 * memberRetryAfter)
	g.mu.Unlock()
	t.Setenv("LCA_TEST_SSH_DOWN", "")
	if err := down.ensureUp(ctx); err != nil {
		t.Fatalf("a member that came back must be usable again: %v", err)
	}
	if n := countScripts(calls(), "&& pwd"); n != 3 {
		t.Fatalf("want one re-probe, got %d in total", n)
	}
}

func TestMemberSandboxTravels(t *testing.T) {
	calls := fakeSSH(t)
	gpuDir, boxDir := t.TempDir(), t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  build-box:\n    host: build01\n    dir: "+boxDir+
		"\n  gpu-0:\n    host: gpu07\n    dir: "+gpuDir+"\n    allow: [ls, cat, python3, bsk, ssh]\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: build-box\n", 1)
	roles += "  trainer:\n    member: gpu-0\n    models: [coder-a]\n    tools: [run_command]\n"
	h := newRoleHarness(t, fs, roles, true)
	sess := func(role string) *Session {
		c, err := h.orch.newChild(h.sess, h.orch.agents[role], "x")
		if err != nil {
			t.Fatal(err)
		}
		c.view = &quietView{}
		return c
	}
	gpu, box := sess("trainer"), sess("coder")
	before := len(calls())
	if got := gpu.runTool(context.Background(), "git status", 30*time.Second, nil); !strings.Contains(got, "not on the allowlist for member gpu-0") {
		t.Fatalf("the member's own allowlist must decide: %q", got)
	}
	if after := len(calls()); after != before {
		t.Fatal("a refused command must not leave this machine")
	}
	// A GPU launcher the member's list does not name is refused by the list
	// (naming the member); one reached through a command it DOES name is refused
	// by the GPU policy, which is unconditional on every machine. What is checked
	// is the payload we are about to send, never the ssh argv we build.
	if got := gpu.runTool(context.Background(), "srun -G1 python t.py", 30*time.Second, nil); !strings.Contains(got, "not on the allowlist for member gpu-0") {
		t.Fatalf("srun on a member: %q", got)
	}
	for _, c := range []string{"ssh node srun -G1 python t.py", "python3 -m torch.distributed.run t.py"} {
		if got := gpu.runTool(context.Background(), c, 30*time.Second, nil); !strings.Contains(got, "bsk submit") {
			t.Fatalf("%q on a member: %q", c, got)
		}
	}
	if got := gpu.runTool(context.Background(), "bsk submit -g 1 -- python t.py", 30*time.Second, nil); strings.Contains(got, "not on the allowlist") {
		t.Fatalf("bsk submit is the scheduler path: %q", got)
	}
	// The team's list still governs a member that declares none of its own, and a
	// member's own list REPLACES it rather than adding to it.
	if got := box.runTool(context.Background(), "echo hi", 30*time.Second, nil); !strings.Contains(got, "hi") {
		t.Fatalf("build-box uses the team list: %q", got)
	}
	if got := gpu.runTool(context.Background(), "echo hi", 30*time.Second, nil); !strings.Contains(got, "not on the allowlist for member gpu-0") {
		t.Fatalf("gpu-0 replaces the team list, not adds to it: %q", got)
	}
}

// The live hole this closes: the remote branch of run_command called Remote.run
// with no CheckCommand at all, so the allowlist and the GPU policy were not
// enforced for the model's own commands on a remote project.
func TestRunCommandToolChecksMemberPolicy(t *testing.T) {
	calls := fakeSSH(t)
	dir := t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  gpu-0:\n    host: gpu07\n    dir: "+dir+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: gpu-0\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	child, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "x")
	if err != nil {
		t.Fatal(err)
	}
	child.view = &quietView{}
	tc := &ToolCtx{Ctx: context.Background(), S: child, Name: "run_command"}
	// sh is on the team's list and the member declares none of its own, so the
	// line reaches the look-through — and the GPU policy still refuses it.
	got := runCommandTool(tc, Args{"command": `sh -c "srun -G1 python t.py"`})
	if !strings.Contains(got, "bsk submit") {
		t.Fatalf("the GPU policy must hold on a member: %q", got)
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("a refused command opened %d ssh connections", n)
	}
}

func TestMemberJailIsACopyAndTracksUnsafe(t *testing.T) {
	jl, err := NewJail(t.TempDir(), []string{"go", "git"}, false)
	if err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{jl: jl}
	yes := true
	own := &Member{Name: "gpu-0", Rem: &Remote{Name: "gpu-0", Host: "g", Dir: "/p"}, Allow: []string{"python3"}, allowSet: map[string]bool{"python3": true}, Shell: &yes}
	plain := &Member{Name: "box", Rem: &Remote{Name: "box", Host: "b", Dir: "/p"}}
	if got := own.jail(o); got == jl {
		t.Fatal("a member with its own allowlist must get a copy")
	}
	if strings.Join(jl.Allowed, ",") != "go,git" {
		t.Fatalf("the team's jail was mutated: %v", jl.Allowed)
	}
	if own.jail(o).CheckCommand("go build ./...") == nil {
		t.Fatal("the member's list replaces the team's")
	}
	if !own.jail(o).Shell {
		t.Fatal("the member's shell mode must apply")
	}
	if plain.jail(o) != jl {
		t.Fatal("a member that declares no sandbox must get the team's jail itself")
	}
	// /unsafe toggles the team's jail; a cached copy would keep the sandbox on
	// for one member after the operator lifted it.
	jl.Unsafe = true
	if !own.jail(o).Unsafe {
		t.Fatal("a member's jail must follow /unsafe")
	}
	if own.jail(o).Member != "gpu-0" || plain.jail(o).Member != "" {
		t.Fatal("Where names the member a refusal is about, and only for a member's own policy")
	}
}

// ── cross-machine worktrees ─────────────────────────────────────────────────

func gitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if _, err := gitCmd(dir, nil, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoteWorktreeRoundTrip(t *testing.T) {
	calls := fakeSSH(t)
	lcaDir := t.TempDir()
	t.Setenv("LCA_DIR", lcaDir)
	repo := t.TempDir()
	if real, err := filepath.EvalSymlinks(repo); err == nil {
		repo = real
	}
	gitRepo(t, repo, map[string]string{"a.txt": "one\n"})
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("uncommitted\n"), 0o644)

	mem := &Member{Name: "box", Rem: &Remote{Name: "box", Host: "box01", Dir: repo}}
	var mgr worktrees
	wt, err := mgr.createOn(context.Background(), mem, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(wt.dir, filepath.Join(lcaDir, "worktrees")) {
		t.Fatalf("the worktree belongs under the member's own state dir, got %s", wt.dir)
	}
	if strings.HasPrefix(wt.dir, repo) {
		t.Fatal("a worktree inside the project would be swallowed by the next add -A")
	}
	if b, _ := os.ReadFile(filepath.Join(wt.root, "a.txt")); string(b) != "uncommitted\n" {
		t.Fatalf("the snapshot must carry uncommitted work: %q", b)
	}
	os.WriteFile(filepath.Join(wt.root, "done.txt"), []byte("x\n"), 0o644)
	diff, n, err := wt.diff()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || !strings.Contains(diff, "done.txt") {
		t.Fatalf("diff: %d files\n%s", n, diff)
	}
	for _, abs := range []string{repo, lcaDir, wt.dir} {
		if strings.Contains(diff, abs) {
			t.Fatalf("a patch that crosses machines must carry no absolute path (%s):\n%s", abs, diff)
		}
	}
	wt.remove()
	if _, err := os.Stat(wt.dir); err == nil {
		t.Fatal("the worktree was not removed")
	}
	out, _ := gitCmd(repo, nil, nil, "worktree", "list")
	if len(strings.Split(strings.TrimSpace(out), "\n")) != 1 {
		t.Fatalf("worktree list:\n%s", out)
	}
	if n := countScripts(calls(), "worktree prune"); n != 0 {
		t.Fatal("prune on a member can prune a worktree another lca process just added")
	}
}

// memberDelegateHarness is a lead on THIS machine and a coder on member "box"
// whose directory is a clone of the caller's tree at the same commit, at a
// different absolute path.
func memberDelegateHarness(t *testing.T, fs *fakeServer, extra string) (*harness, string, func() [][]string) {
	t.Helper()
	calls := fakeSSH(t)
	t.Setenv("LCA_DIR", t.TempDir())
	box := filepath.Join(t.TempDir(), "clone")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  farside:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: farside\n", 1)
	roles += extra
	h := newRoleHarness(t, fs, roles, true)
	if _, err := gitCmd(filepath.Dir(box), nil, nil, "clone", "-q", h.root, box); err != nil {
		t.Fatal(err)
	}
	return h, box, calls
}

func delegateOnMember(t *testing.T, h *harness, args Args) delegateResult {
	t.Helper()
	tc := &ToolCtx{Ctx: context.Background(), S: h.sess, Name: "delegate"}
	var out delegateResult
	raw := captureStdout(t, func() {
		if err := json.Unmarshal([]byte(runDelegateTool(tc, args)), &out); err != nil {
			t.Fatalf("delegate result is not the contract: %v", err)
		}
	})
	_ = raw
	return out
}

func TestDelegateAcrossMembers(t *testing.T) {
	fs := coderWritesFile(t)
	h, box, calls := memberDelegateHarness(t, fs, "")
	res := delegateOnMember(t, h, Args{"role": "coder", "task": "create done.txt"})
	if res.Status != "passed" {
		t.Fatalf("status %q, tail %q", res.Status, res.TestTail)
	}
	if n := countScripts(calls(), "worktree add --detach"); n != 1 {
		t.Fatalf("the worktree must be created ON the member: %d", n)
	}
	for _, abs := range []string{h.root, box} {
		if strings.Contains(res.Diff, abs) {
			t.Fatalf("the diff leaked an absolute path (%s):\n%s", abs, res.Diff)
		}
	}
	if b, err := os.ReadFile(filepath.Join(h.root, "done.txt")); err != nil || string(b) != "ok\n" {
		t.Fatalf("the diff must land in the CALLER's tree: %v %q", err, b)
	}
	if _, err := os.Stat(filepath.Join(box, "done.txt")); err == nil {
		t.Fatal("the member's own project must be untouched — only its worktree changed")
	}
	// Only the contract goes back up.
	var raw map[string]any
	b, _ := json.Marshal(res)
	json.Unmarshal(b, &raw)
	for k := range raw {
		if k != "status" && k != "diff" && k != "test_tail" && k != "review" {
			t.Fatalf("unexpected field %q in the delegate result", k)
		}
	}
	_, tasks := readTrace(t, h)
	if len(tasks) != 1 || tasks[0].Member != "farside" || tasks[0].CallerMember != "local" {
		t.Fatalf("task record: %+v", tasks)
	}
	// The wire contract: the member NAME reaches no system prompt and no tool
	// schema. (The HOST does appear in the prompt's environment block, and always
	// has: the model has to know where the project is.)
	for _, req := range fs.reqs() {
		if strings.Contains(req.system(), "farside") {
			t.Fatalf("a member name must not reach the prompt:\n%s", req.system())
		}
		for _, sc := range req.Tools {
			blob, _ := json.Marshal(sc)
			if strings.Contains(string(blob), "box01") || strings.Contains(string(blob), "farside") {
				t.Fatalf("a member must not reach a tool schema: %s", blob)
			}
		}
	}
}

func TestCrossMachineDelegateRefusals(t *testing.T) {
	t.Run("not a repository", func(t *testing.T) {
		fakeSSH(t)
		lcaDir := t.TempDir()
		t.Setenv("LCA_DIR", lcaDir)
		box := t.TempDir() // a plain directory, no git
		fs := coderWritesFile(t)
		fs.models = allModels()
		roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
		roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: box\n", 1)
		h := newRoleHarness(t, fs, roles, true)
		res := delegateOnMember(t, h, Args{"role": "coder", "task": "t"})
		if res.Status != "error" || !strings.Contains(res.TestTail, "git init or clone the project there") {
			t.Fatalf("%+v", res)
		}
		if _, err := os.Stat(filepath.Join(lcaDir, "worktrees")); err == nil {
			t.Fatal("nothing may be created when the member has no repository")
		}
	})
	t.Run("no git", func(t *testing.T) {
		h, _, _ := memberDelegateHarness(t, coderWritesFile(t), "")
		_ = h
		t.Setenv("LCA_TEST_SSH_NOGIT", "box01")
		res := delegateOnMember(t, h, Args{"role": "coder", "task": "t"})
		if res.Status != "error" || !strings.Contains(res.TestTail, "install git on") {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("different layout", func(t *testing.T) {
		fakeSSH(t)
		lcaDir := t.TempDir()
		t.Setenv("LCA_DIR", lcaDir)
		// The member's dir is a SUBDIRECTORY of its repository; the caller's is
		// the top of its own. A patch could not survive that.
		outer := t.TempDir()
		box := filepath.Join(outer, "services", "api")
		fs := coderWritesFile(t)
		fs.models = allModels()
		roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
		roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: box\n", 1)
		h := newRoleHarness(t, fs, roles, true)
		gitRepo(t, outer, map[string]string{"services/api/a.txt": "one\n"})
		res := delegateOnMember(t, h, Args{"role": "coder", "task": "t"})
		if res.Status != "error" || !strings.Contains(res.TestTail, "the two must match") {
			t.Fatalf("%+v", res)
		}
		if !strings.Contains(res.TestTail, "services/api") {
			t.Fatalf("the refusal must name both layouts: %q", res.TestTail)
		}
		left, _ := os.ReadDir(filepath.Join(lcaDir, "worktrees"))
		if len(left) != 0 {
			t.Fatalf("the worktree must be removed once the layouts disagree: %v", left)
		}
	})
	t.Run("diverged caller", func(t *testing.T) {
		h, _, _ := memberDelegateHarness(t, coderWritesFile(t), "")
		// The caller's tree now holds a conflicting done.txt, so the patch that
		// creates it cannot apply: no shared object store, no 3-way merge.
		os.WriteFile(filepath.Join(h.root, "done.txt"), []byte("mine\n"), 0o644)
		gitCmd(h.root, nil, nil, "add", "-A")
		gitCmd(h.root, nil, nil, "commit", "-q", "-m", "mine")
		res := delegateOnMember(t, h, Args{"role": "coder", "task": "t"})
		if res.Status != "conflict" {
			t.Fatalf("status %q tail %q", res.Status, res.TestTail)
		}
		for _, want := range []string{"farside", "local", "a patch only applies where its context matches"} {
			if !strings.Contains(res.TestTail, want) {
				t.Fatalf("conflict message %q, want %q", res.TestTail, want)
			}
		}
	})
}

func TestDelegateRefusesUnreachableMember(t *testing.T) {
	h, _, calls := memberDelegateHarness(t, coderWritesFile(t), "")
	t.Setenv("LCA_TEST_SSH_DOWN", "box01")
	before, _ := gitCmd(h.root, nil, nil, "status", "--porcelain")
	res := delegateOnMember(t, h, Args{"role": "coder", "task": "t"})
	if res.Status != "error" || !strings.Contains(res.TestTail, "member farside") || !strings.Contains(res.TestTail, "is unreachable") {
		t.Fatalf("%+v", res)
	}
	after, _ := gitCmd(h.root, nil, nil, "status", "--porcelain")
	if before != after {
		t.Fatalf("the caller's tree changed: %q → %q", before, after)
	}
	if n := countScripts(calls(), "worktree add"); n != 0 {
		t.Fatal("no worktree may be created for an unreachable member")
	}
}

func TestDelegateWorktreeOnRemoteCaller(t *testing.T) {
	calls := fakeSSH(t)
	t.Setenv("LCA_DIR", t.TempDir())
	box := filepath.Join(t.TempDir(), "proj")
	fs := coderWritesFile(t)
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "defaults:\n", "defaults:\n  member: box\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	gitRepo(t, box, map[string]string{"a.txt": "one\n"})
	// Both the lead and the coder live on box now.
	if !strings.Contains(h.sess.memberName(), "box") {
		t.Fatalf("defaults.member must move the lead too, got %q", h.sess.memberName())
	}
	res := delegateOnMember(t, h, Args{"role": "coder", "task": "t"})
	if res.Status != "passed" {
		t.Fatalf("status %q tail %q", res.Status, res.TestTail)
	}
	if n := countScripts(calls(), "apply --check --binary"); n != 1 {
		t.Fatalf("the apply must run on the caller's machine: %d", n)
	}
	if b, err := os.ReadFile(filepath.Join(box, "done.txt")); err != nil || string(b) != "ok\n" {
		t.Fatalf("the diff must land in the member's tree: %v %q", err, b)
	}
	if _, err := os.Stat(filepath.Join(h.root, "done.txt")); err == nil {
		t.Fatal("nothing may be written on this machine")
	}
}

func TestForkAcrossMembersSkipped(t *testing.T) {
	h, _, _ := memberDelegateHarness(t, coderWritesFile(t), "")
	// The caller has read a file here; a cross-machine child must not inherit it.
	h.sess.Msgs = append(h.sess.Msgs, Message{Role: "tool", Tool: "read_file", Path: "README.md", Content: "README.md:\nhello\n"})
	var child *Session
	out := captureStdout(t, func() {
		tc := &ToolCtx{Ctx: context.Background(), S: h.sess, Name: "delegate"}
		runDelegateTool(tc, Args{"role": "coder", "task": "t", "fork": true})
	})
	if !strings.Contains(out, "FORK  skipped") {
		t.Fatalf("the skip must be visible: %q", out)
	}
	for _, c := range h.orch.Tasks() {
		if c.agent.Name == "coder" {
			child = c
		}
	}
	if child != nil {
		for _, m := range child.Msgs {
			if strings.Contains(m.Content, "<tool_result") {
				t.Fatal("a cross-machine child must inherit no reads")
			}
		}
	}
}

func TestReviewerFollowsTheWorktree(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		switch req.Model {
		case "coder-a":
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "created done.txt"}
			}
			return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "done.txt", "content": "ok\n"})}}
		case "lead-b":
			return fakeReply{content: "VERDICT: approve\nfine"}
		}
		return fakeReply{content: "ok"}
	})
	h, _, _ := memberDelegateHarness(t, fs, "  auditor:\n    models: [lead-b]\n    tools: [read_file]\n")
	h.orch.agents["coder"].Review = "auditor"
	res := delegateOnMember(t, h, Args{"role": "coder", "task": "t"})
	if res.Review == nil || res.Review.Verdict != "approve" {
		t.Fatalf("review: %+v (%q)", res.Review, res.TestTail)
	}
	var rev *Session
	for _, c := range h.orch.Tasks() {
		if c.agent.Name == "auditor" {
			rev = c
		}
	}
	if rev == nil {
		t.Skip("the reviewer session was already forgotten")
	}
	if rem := rev.remote(); rem == nil || rem.Host != "box01" {
		t.Fatalf("the reviewer must run where the worktree is: %+v", rem)
	}
	if Evaluate("edit", "x", rev.rules()...) != Deny || Evaluate("delegate", "x", rev.rules()...) != Deny {
		t.Fatal("the reviewer must still be unable to edit or delegate")
	}
}

func TestRemoteDiffRejectsShellBanner(t *testing.T) {
	h, _, _ := memberDelegateHarness(t, coderWritesFile(t), "")
	t.Setenv("LCA_TEST_SSH_BANNER", "Welcome to build01")
	res := delegateOnMember(t, h, Args{"role": "coder", "task": "t"})
	if res.Status == "passed" {
		t.Fatal("a banner on stdout must not be read as a patch")
	}
	if !strings.Contains(res.TestTail, "[ -t 1 ]") {
		t.Fatalf("the failure must name the fix: %q", res.TestTail)
	}
	if _, err := os.Stat(filepath.Join(h.root, "done.txt")); err == nil {
		t.Fatal("nothing may be applied from an unreadable patch")
	}
}

// ── roles, prefix, YAML round trip ──────────────────────────────────────────

func TestRoleMemberRouting(t *testing.T) {
	fakeSSH(t)
	dir := t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+dir+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: box\n", 1)
	roles += "  helper:\n    models: [cheap-a]\n    tools: [read_file]\n"
	h := newRoleHarness(t, fs, roles, true)
	if h.sess.memberName() != localMemberName {
		t.Fatalf("an unpinned lead stays here, got %q", h.sess.memberName())
	}
	coder, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "x")
	if err != nil {
		t.Fatal(err)
	}
	coder.view = &quietView{}
	if coder.memberName() != "box" || coder.remote() == nil {
		t.Fatalf("a pinned role's sessions work on its member, got %q", coder.memberName())
	}
	// A child that names no member inherits the CALLER's machine, not the team
	// default: the task tool's contract is a shared tree.
	sub, err := h.orch.newChild(coder, h.orch.agents["helper"], "x")
	if err != nil {
		t.Fatal(err)
	}
	sub.view = &quietView{}
	if sub.memberName() != "box" {
		t.Fatalf("a task child must share the caller's machine, got %q", sub.memberName())
	}
	back, err := h.orch.newChild(h.sess, h.orch.agents["helper"], "x")
	if err != nil {
		t.Fatal(err)
	}
	back.view = &quietView{}
	if back.memberName() != localMemberName {
		t.Fatalf("a local caller's child stays local, got %q", back.memberName())
	}
}

func TestMembersSurviveRoleSave(t *testing.T) {
	rc, err := loadMembersYAML(t, `entry: lead
members:
  local:
    allow: [go, git]
  build-box:
    host: build01
    dir: /srv/proj
    ssh: [-o, ConnectTimeout=5]
    allow: [go, bsk]
    shell: true
defaults:
  member: build-box
`+memberRolesTail)
	if err != nil {
		t.Fatal(err)
	}
	back, err := loadMembersYAML(t, rc.YAML())
	if err != nil {
		t.Fatalf("YAML() does not reload: %v\n%s", err, rc.YAML())
	}
	if back.DefaultMember != "build-box" || back.Members["build-box"] == nil {
		t.Fatalf("round trip lost the fleet:\n%s", rc.YAML())
	}
	b := back.Members["build-box"]
	if b.Rem.Host != "build01" || b.Rem.Dir != "/srv/proj" || strings.Join(b.Rem.SSH, " ") != "-o ConnectTimeout=5" {
		t.Fatalf("round trip lost the transport: %+v", b.Rem)
	}
	if strings.Join(b.Allow, ",") != "go,bsk" || b.Shell == nil || !*b.Shell {
		t.Fatalf("round trip lost the member's sandbox: %v %v", b.Allow, b.Shell)
	}
	if l := back.Members["local"]; l == nil || strings.Join(l.Allow, ",") != "go,git" {
		t.Fatalf("round trip lost local's sandbox: %+v", l)
	}
	for _, a := range back.Roles {
		if a.Name == "coder" && a.Member != "build-box" {
			t.Fatalf("round trip lost the role's member: %q", a.Member)
		}
	}
	// A legacy remote: block is written back as remote:, not silently migrated.
	leg, err := loadMembersYAML(t, "remote:\n  host: cab\n  dir: /p\nroles:\n  lead:\n    models: [lead-a]\n")
	if err != nil {
		t.Fatal(err)
	}
	y := leg.YAML()
	if !strings.Contains(y, "remote:\n  host: cab") || strings.Contains(y, "members:") {
		t.Fatalf("a legacy team must round-trip as remote:\n%s", y)
	}
}

func TestMemberDoesNotChangeThePrefix(t *testing.T) {
	fakeSSH(t)
	dir := t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  build-box:\n    host: build01\n    dir: "+dir+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: build-box\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	a, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "x")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "x")
	if err != nil {
		t.Fatal(err)
	}
	if a.Msgs[0].Content != b.Msgs[0].Content {
		t.Fatal("two sessions of one role on one member must send a byte-identical prefix")
	}
	if strings.Contains(a.Msgs[0].Content, "build-box") {
		t.Fatalf("the member NAME must not reach the prompt:\n%s", a.Msgs[0].Content)
	}
	_, schemas := a.tools()
	blob, _ := json.Marshal(schemas)
	if strings.Contains(string(blob), "build-box") || strings.Contains(string(blob), "member") {
		t.Fatalf("the member must not reach a tool schema: %s", blob)
	}
	if !strings.Contains(a.Msgs[0].Content, "on another machine") {
		t.Fatal("the prompt still has to say where the project lives")
	}
}

// ── workflow steps ──────────────────────────────────────────────────────────

func memberWFHarness(t *testing.T, fs *fakeServer, boxDir string) (*harness, string) {
	t.Helper()
	fs.models = allModels()
	roles := strings.Replace(wfRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+boxDir+"\nsandbox:\n", 1)
	if !strings.Contains(roles, "members:") {
		roles += "\nmembers:\n  box:\n    host: box01\n    dir: " + boxDir + "\n"
	}
	h := newRoleHarness(t, fs, roles, true)
	return h, t.TempDir()
}

func TestWorkflowStepMemberOverride(t *testing.T) {
	calls := fakeSSH(t)
	box := t.TempDir()
	h, dir := memberWFHarness(t, alwaysReply(t, "ok"), box)
	_, st, r := runWF(t, h, dir, "steps:\n  build:\n    run: echo built\n    member: box\n  here:\n    run: echo local\n", nil)
	r.Close()
	for _, n := range []string{"build", "here"} {
		if ss := stepByName(st, n); ss == nil || ss.Status != stepOK {
			t.Fatalf("step %s: %+v", n, ss)
		}
	}
	if got := stepByName(st, "build").Member; got != "box" {
		t.Fatalf("StepState.Member = %q", got)
	}
	if got := stepByName(st, "here").Member; got != localMemberName {
		t.Fatalf("a step without member: stays on the role's machine, got %q", got)
	}
	if n := countScripts(calls(), "echo built"); n != 1 {
		t.Fatalf("the step's command must run on its member: %d", n)
	}
	if n := countScripts(calls(), "echo local"); n != 0 {
		t.Fatal("a local step must not be sent over ssh")
	}
	var found bool
	for _, rec := range readSteps(t, h) {
		if rec.Step == "build" && rec.Member == "box" {
			found = true
		}
	}
	if !found {
		t.Fatal("StepRecord.Member must carry the machine")
	}
	wf := loadWF(t, h, dir, "steps:\n  build:\n    run: echo built\n    member: box\n", nil)
	rows := wf.plan()
	if len(rows) != 1 || !contains(rows[0], "box") {
		t.Fatalf("-dry-run must show the member column: %v", rows)
	}
}

func TestWorkflowStepMemberBindErrors(t *testing.T) {
	calls := fakeSSH(t)
	box := t.TempDir()
	fs := alwaysReply(t, "ok")
	_ = calls
	h, dir := memberWFHarness(t, fs, box)
	if _, err := parseWorkflow("t", filepath.Join(dir, "t.yaml"), "steps:\n  a:\n    run: ls\n    member:\n"); err == nil || !strings.Contains(err.Error(), "member: is empty") {
		t.Fatalf("empty member: %v", err)
	}
	bind := func(yaml string) error {
		wf, err := parseWorkflow("t", filepath.Join(dir, "t.yaml"), yaml)
		if err != nil {
			return err
		}
		return wf.bind(h.orch, "lead", wf.effectiveVars(nil))
	}
	if err := bind("steps:\n  a:\n    run: ls\n    member: nope\n"); err == nil || !strings.Contains(err.Error(), `member: "nope" is not a member`) {
		t.Fatalf("unknown member: %v", err)
	}
	if err := bind("steps:\n  a:\n    delegate: do it\n    role: coder\n    check: ls\n    member: box\n"); err == nil || !strings.Contains(err.Error(), "is not a git repository") {
		t.Fatalf("a delegate step needs a repository on its member: %v", err)
	}
	t.Setenv("LCA_TEST_SSH_DOWN", "box01")
	forgetReach(h.orch.member("box"))
	if err := bind("steps:\n  a:\n    run: ls\n    member: box\n"); err == nil || !strings.Contains(err.Error(), "member box") {
		t.Fatalf("an unreachable member must refuse the run at bind: %v", err)
	}
	if n := len(fs.reqs()); n != 0 {
		t.Fatalf("bind must cost no tokens, got %d requests", n)
	}
	if runs := mustRuns(t, h.orch.cfg); len(runs) != 0 {
		t.Fatalf("bind must create no run directory, got %d", len(runs))
	}
}

func TestWorkflowRunStepUsesMemberAllowlist(t *testing.T) {
	fakeSSH(t)
	box := t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(wfRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+box+
		"\n    allow: [echo]\n    shell: true\n  plain:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	dir := t.TempDir()
	err := func() error {
		wf, e := parseWorkflow("t", filepath.Join(dir, "t.yaml"), "steps:\n  a:\n    run: ls\n    member: box\n")
		if e != nil {
			return e
		}
		return wf.bind(h.orch, "lead", wf.effectiveVars(nil))
	}()
	if err == nil || !strings.Contains(err.Error(), "allowlist") || !strings.Contains(err.Error(), "member box") {
		t.Fatalf("the member's list must decide, naming it: %v", err)
	}
	// The same line on a member that allows it binds and runs; and `&&` binds
	// because THAT member has shell: true while the team does not.
	_, st, r := runWF(t, h, dir, "steps:\n  a:\n    run: echo one && echo two\n    member: box\n", nil)
	r.Close()
	if ss := stepByName(st, "a"); ss == nil || ss.Status != stepOK {
		t.Fatalf("%+v", ss)
	}
}

func TestWorkflowDelegateStepPerMember(t *testing.T) {
	calls := fakeSSH(t)
	t.Setenv("LCA_DIR", t.TempDir())
	box := filepath.Join(t.TempDir(), "clone")
	fs := coderWritesFile(t)
	fs.models = allModels()
	roles := strings.Replace(wfRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	dir := t.TempDir()
	if _, err := gitCmd(filepath.Dir(box), nil, nil, "clone", "-q", h.root, box); err != nil {
		t.Fatal(err)
	}
	// The legacy spelling keeps its blanket refusal, verbatim — a team with no
	// members: block has nowhere to put a worktree but this machine.
	legacy, ldir := wfHarness(t, alwaysReply(t, "ok"))
	legacy.orch.remote = localRemote(ldir)
	wf, err := parseWorkflow("t", filepath.Join(ldir, "t.yaml"), "steps:\n  a:\n    delegate: do it\n    role: coder\n    check: ls\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := wf.bind(legacy.orch, "lead", nil); err == nil || !strings.Contains(err.Error(), "local git worktrees") {
		t.Fatalf("a legacy fleet keeps the blanket refusal: %v", err)
	}
	// A members: fleet whose member has git binds and runs.
	_, st, r := runWF(t, h, dir, "steps:\n  a:\n    delegate: create done.txt\n    role: coder\n    check: ls done.txt\n    member: box\n", nil)
	r.Close()
	if ss := stepByName(st, "a"); ss == nil || ss.Status != stepOK || ss.Member != "box" {
		t.Fatalf("%+v", ss)
	}
	if n := countScripts(calls(), "worktree add --detach"); n != 1 {
		t.Fatalf("the worktree must be created on the member: %d", n)
	}
}

func TestWorkflowStepMemberUnreachableFailsStep(t *testing.T) {
	fakeSSH(t)
	box := t.TempDir()
	h, dir := memberWFHarness(t, alwaysReply(t, "ok"), box)
	// Reachable at bind, gone by the time the step runs.
	wf := loadWF(t, h, dir, "steps:\n  a:\n    run: echo x\n    member: box\n  b:\n    run: echo after\n", nil)
	rdir, st := newRunState(h.orch.cfg, h.orch, wf, h.sess.agent.Name, wf.effectiveVars(nil))
	r, err := newRunner(h.orch, h.sess, wf, rdir, st)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	forgetReach(h.orch.member("box"))
	t.Setenv("LCA_TEST_SSH_DOWN", "box01")
	code := runRunner(t, r)
	ss := stepByName(r.st, "a")
	if ss == nil || ss.Status != stepFailed || !strings.Contains(ss.Detail, "member box") {
		t.Fatalf("step: %+v", ss)
	}
	if code == 0 {
		t.Fatal("on_fail: stop must stop the run")
	}
	if stepByName(r.st, "b") != nil {
		t.Fatal("nothing after a stopped step may run")
	}
	if !strings.Contains(readLog(t, r), "is unreachable") {
		t.Fatal("run.log must hold the refusal")
	}
}

// ── visibility ──────────────────────────────────────────────────────────────

func TestMembersCommandAndTrace(t *testing.T) {
	calls := fakeSSH(t)
	up, down := t.TempDir(), t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+up+
		"\n  gpu-0:\n    host: gpu07\n    dir: "+down+"\n    allow: [ls, bsk]\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: box\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	t.Setenv("LCA_TEST_SSH_DOWN", "gpu07")
	r := &Repl{orch: h.orch, sess: h.sess}
	out := captureStdout(t, func() { r.cmdMembers("") })
	for _, want := range []string{"local", "box", "box01:" + up, "gpu-0", "2 own", "coder", "unreachable", "Operation timed out", "fix ssh"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/members output missing %q:\n%s", want, out)
		}
	}
	if n := countScripts(calls(), "&& pwd"); n != 2 {
		t.Fatalf("/members probes each member once: %d", n)
	}
	out = captureStdout(t, func() { r.cmdAgents("") })
	if !strings.Contains(out, "member") || !strings.Contains(out, "box") {
		t.Fatalf("/agents must show which member each role uses:\n%s", out)
	}
	// A turn on a pinned role carries its member in the trace.
	child, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "x")
	if err != nil {
		t.Fatal(err)
	}
	child.view = &quietView{}
	child.Msgs = append(child.Msgs, Message{Role: "user", Content: "hi"})
	child.Run(context.Background())
	// TurnRecord.Member is filled from the resolved member NAME, so on its own it
	// would still read "box" with the routing removed. Anchor it to where this
	// session's tools actually go.
	before := len(calls())
	if got := child.runTool(context.Background(), "echo hi", 30*time.Second, nil); !strings.Contains(got, "hi") {
		t.Fatalf("run_command on box: %q", got)
	}
	sent := sshScripts(calls()[before:])
	if len(sent) != 1 || !strings.Contains(sent[0], "cd '"+up+"' && echo hi") {
		t.Fatalf("a pinned role's commands must leave for box01:%s: %v", up, sent)
	}
	turns, _ := readTrace(t, h)
	var seen map[string]bool = map[string]bool{}
	for _, tr := range turns {
		seen[tr.Role+":"+tr.Member] = true
	}
	if !seen["coder:box"] {
		t.Fatalf("turn records must carry the member: %v", seen)
	}
}

func TestInitWritesMembers(t *testing.T) {
	m := &memberFlags{}
	if err := m.Set("build01:/srv"); err == nil || !strings.Contains(err.Error(), "name=host:/absolute/path") {
		t.Fatalf("%v", err)
	}
	if err := m.Set("local=h:/p"); err == nil || !strings.Contains(err.Error(), "local always means this machine") {
		t.Fatalf("%v", err)
	}
	if err := m.Set("box=box01:relative"); err == nil {
		t.Fatal("the member's dir must be absolute")
	}
	if err := m.Set("box=box01:/srv/proj"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("gpu-0=gpu07:/scratch"); err != nil {
		t.Fatal(err)
	}
	if len(m.list) != 2 {
		t.Fatalf("-member is repeatable: %v", m.list)
	}
	// What init writes has to load back as a fleet.
	yaml := "members:\n"
	for _, e := range m.list {
		yaml += "  " + e.name + ":\n    host: " + e.host + "\n    dir: " + e.dir + "\n"
	}
	rc, err := loadMembersYAML(t, yaml+"\nroles:\n  lead:\n    models: [lead-a]\n")
	if err != nil {
		t.Fatal(err)
	}
	if rc.Members["box"] == nil || rc.Members["gpu-0"] == nil || rc.Members[localMemberName] == nil {
		t.Fatalf("members: %v", rc.MemberOrder)
	}
}

// Cross-machine delegation is a members: feature, and the tool list says so
// without asking the network: the schema is part of the cached prefix.
func TestDelegateToolOnAMemberFleet(t *testing.T) {
	fakeSSH(t)
	dir := t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+dir+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "defaults:\n", "defaults:\n  member: box\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	if h.sess.memberName() != "box" {
		t.Fatalf("the lead must be on box, got %q", h.sess.memberName())
	}
	has := func(s *Session, name string) bool {
		defs, _ := s.tools()
		for _, d := range defs {
			if d.Name == name {
				return true
			}
		}
		return false
	}
	if !has(h.sess, "delegate") {
		t.Fatal("a session on a members: member gets the real delegate tool")
	}
	r := &Repl{orch: h.orch, sess: h.sess}
	if !r.orch.canDelegate(r.sess.memberOf()) {
		t.Fatal("/delegate must be offered on a members: fleet")
	}
	// The old single-remote spelling keeps it off, with no probe of any kind.
	legacy := newRoleHarness(t, alwaysReply(t, "ok"), strings.Replace(testRoles, "sandbox:\n",
		"remote:\n  host: \"\"\n  dir: "+dir+"\n  ssh: [sh, -c]\nsandbox:\n", 1), true)
	if has(legacy.sess, "delegate") {
		t.Fatal("delegate stays hidden on the old remote: spelling")
	}
	if legacy.orch.canDelegate(legacy.sess.memberOf()) {
		t.Fatal("/delegate stays hidden on the old remote: spelling")
	}
}

// ── the sandbox on the far side ──────────────────────────────────────────────

// A command bound for a member is executed by a SHELL over there (the transport
// sends `cd <dir> && <line>` as one ssh argument), so checking it with the local
// executor's one-argv semantics let `echo hi; touch x` past the allowlist and
// past the GPU policy on argv[0] alone. The local path does not have the hole:
// it execs an argv directly.
func TestMemberCommandCheckedWithShellSemantics(t *testing.T) {
	calls := fakeSSH(t)
	box := t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: box\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	child, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "x")
	if err != nil {
		t.Fatal(err)
	}
	child.view = &quietView{}
	pwned := filepath.Join(box, "pwned")
	for _, cmd := range []string{
		"echo hi; /usr/bin/touch " + pwned,
		"echo hi && /usr/bin/touch " + pwned,
		"echo hi | /usr/bin/touch " + pwned,
		"echo $(/usr/bin/touch " + pwned + ")",
		"echo `/usr/bin/touch " + pwned + "`",
		"echo hi; srun -n8 hostname",
	} {
		got := child.runTool(context.Background(), cmd, 30*time.Second, nil)
		if !strings.Contains(got, "not on the allowlist") && !strings.Contains(got, "bsk submit") {
			t.Fatalf("%q was not refused: %q", cmd, got)
		}
		if _, err := os.Stat(pwned); err == nil {
			t.Fatalf("%q ran an off-allowlist command on the member", cmd)
		}
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("a refused command must not leave this machine: %v", sshScripts(calls()))
	}
	// A pipeline whose every segment is allowlisted still runs: the far side has
	// always been a shell, and closing the hole must not take from a remote team
	// what it does today — what must not happen is an unlisted command running.
	if got := child.runTool(context.Background(), "echo hi | cat", 30*time.Second, nil); !strings.Contains(got, "hi") {
		t.Fatalf("an allowlisted pipeline on a member: %q", got)
	}
}

// The same hole through a model-supplied check_cmd, which is validated by the
// target member's sandbox before any worktree is made.
func TestDelegateCheckCmdCheckedAsAShellLine(t *testing.T) {
	fs := coderWritesFile(t)
	h, box, calls := memberDelegateHarness(t, fs, "")
	pwned := filepath.Join(box, "pwned")
	res := delegateOnMember(t, h, Args{"role": "coder", "task": "t", "check_cmd": "ls . ; /usr/bin/touch " + pwned})
	if res.Status != "error" || !strings.Contains(res.TestTail, "rejected by the sandbox") {
		t.Fatalf("a check_cmd smuggling a second command: %+v", res)
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("the smuggled command ran on the member")
	}
	if n := countScripts(calls(), "touch"); n != 0 {
		t.Fatalf("it reached the member: %v", sshScripts(calls()))
	}
}

// And through a run step built out of a step's output, which bind cannot check
// because the value does not exist yet — so the check at run time is the only one.
func TestWorkflowRunStepOnAMemberIsCheckedAsAShellLine(t *testing.T) {
	calls := fakeSSH(t)
	box := t.TempDir()
	h, dir := memberWFHarness(t, alwaysReply(t, "ok"), box)
	pwned := filepath.Join(box, "pwned")
	_, st, r := runWF(t, h, dir, "vars:\n  inj: x\nsteps:\n  a:\n    run: echo hi\n    member: box\n  b:\n    run: echo ${vars.inj} ${steps.a.out}\n    member: box\n",
		map[string]string{"inj": "; /usr/bin/touch " + pwned})
	r.Close()
	ss := stepByName(st, "b")
	if ss == nil || ss.Status != stepFailed || !strings.Contains(ss.Detail, "not on the allowlist") {
		t.Fatalf("step b: %+v", ss)
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("the step ran an off-allowlist command on the member")
	}
	if n := countScripts(calls(), "touch"); n != 0 {
		t.Fatalf("it reached the member: %v", sshScripts(calls()))
	}
}

// ── honest failures across machines ─────────────────────────────────────────

// A delegate step on a member that is down must be refused with the ssh error,
// not with a claim about a directory nothing could look at.
func TestDelegateStepOnADownMemberNamesSSH(t *testing.T) {
	fakeSSH(t)
	box := t.TempDir()
	h, dir := memberWFHarness(t, alwaysReply(t, "ok"), box)
	t.Setenv("LCA_TEST_SSH_DOWN", "box01")
	forgetReach(h.orch.member("box"))
	wf, err := parseWorkflow("t", filepath.Join(dir, "t.yaml"), "steps:\n  a:\n    delegate: do it\n    role: coder\n    check: ls\n    member: box\n")
	if err != nil {
		t.Fatal(err)
	}
	err = wf.bind(h.orch, "lead", wf.effectiveVars(nil))
	if err == nil {
		t.Fatal("a delegate step on an unreachable member must be refused")
	}
	for _, want := range []string{"member box", "is unreachable", "Operation timed out"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in the refusal: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("ssh never connected, so nothing is known about the directory: %v", err)
	}
}

// "Build on the box, commit on the laptop": a role pinned to member local,
// delegated to from a caller that lives on a member.
func TestDelegateToALocalRoleFromAMember(t *testing.T) {
	calls := fakeSSH(t)
	t.Setenv("LCA_DIR", t.TempDir())
	box := filepath.Join(t.TempDir(), "clone")
	fs := coderWritesFile(t)
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "defaults:\n", "defaults:\n  member: box\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: local\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	if _, err := gitCmd(filepath.Dir(box), nil, nil, "clone", "-q", h.root, box); err != nil {
		t.Fatal(err)
	}
	if h.sess.memberName() != "box" {
		t.Fatalf("the lead must be on box, got %q", h.sess.memberName())
	}
	res := delegateOnMember(t, h, Args{"role": "coder", "task": "create done.txt"})
	if res.Status != "passed" {
		t.Fatalf("delegating to a role on member local: %q %q", res.Status, res.TestTail)
	}
	if strings.Contains(res.TestTail, "use create") {
		t.Fatal("an internal precondition note reached the operator")
	}
	if b, err := os.ReadFile(filepath.Join(box, "done.txt")); err != nil || string(b) != "ok\n" {
		t.Fatalf("the diff must land in the CALLER's tree on box: %v %q", err, b)
	}
	if n := countScripts(calls(), "worktree add --detach"); n != 0 {
		t.Fatalf("a local target's worktree is made here, not over ssh: %v", sshScripts(calls()))
	}
}

// A same-machine delegation must run under the TARGET member's sandbox: the
// allowlist the check_cmd was validated against is the one the subagent gets.
func TestDelegateChildUsesTheMembersSandbox(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "done"}
			}
			return fakeReply{calls: []ToolCall{call("c", "run_command", map[string]any{"command": "go env GOPATH"})}}
		}
		return fakeReply{content: "ok"}
	})
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  local:\n    allow: [ls, echo]\nsandbox:\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	delegateOnMember(t, h, Args{"role": "coder", "task": "t"})
	var refused bool
	for _, r := range fs.reqs() {
		if strings.Contains(r.Body, "not on the allowlist") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("the subagent ran a command members.local's allowlist does not name")
	}
}

// A nested delegation's diff belongs in the CALLER's worktree. The caller here
// is itself a worktree on a member, and applying into the member's real project
// instead is an isolation escape the local path does not have: the coder's own
// check then decides on files the subagent never put there.
func TestNestedDelegateStaysInTheCallersWorktree(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		switch req.Model {
		case "coder-a":
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "delegated"}
			}
			return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "helper", "task": "write nested.txt"})}}
		case "cheap-a":
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "written"}
			}
			return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "nested.txt", "content": "nested\n"})}}
		}
		return fakeReply{content: "ok"}
	})
	fs.models = allModels()
	calls := fakeSSH(t)
	t.Setenv("LCA_DIR", t.TempDir())
	box := filepath.Join(t.TempDir(), "clone")
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  farside:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n    description: Makes code changes.\n", "  coder:\n    member: farside\n    description: Makes code changes.\n", 1)
	roles = strings.Replace(roles, "    tools: [read_file, write, edit, run_command, list_dir]", "    tools: [read_file, write, edit, run_command, list_dir, delegate]", 1)
	roles = strings.Replace(roles, "    check_cmd: ls done.txt", "    check_cmd: ls nested.txt", 1)
	roles += "  helper:\n    member: farside\n    models: [cheap-a]\n    tools: [write]\n    check_cmd: ls nested.txt\n"
	h := newRoleHarness(t, fs, roles, true)
	h.orch.cfg.SubagentMax = 2 // one nested delegation
	if _, err := gitCmd(filepath.Dir(box), nil, nil, "clone", "-q", h.root, box); err != nil {
		t.Fatal(err)
	}
	res := delegateOnMember(t, h, Args{"role": "coder", "task": "have helper write nested.txt"})
	if _, err := os.Stat(filepath.Join(box, "nested.txt")); err == nil {
		t.Fatal("the nested diff landed in the member's real project, not in the caller's worktree")
	}
	if res.Status != "passed" {
		t.Fatalf("the caller's own check runs in its worktree, where the nested diff went: %q %q", res.Status, res.TestTail)
	}
	if b, err := os.ReadFile(filepath.Join(h.root, "nested.txt")); err != nil || string(b) != "nested\n" {
		t.Fatalf("the nested change must come up through the caller's diff: %v %q", err, b)
	}
	if n := countScripts(calls(), "worktree add --detach"); n != 2 {
		t.Fatalf("both worktrees belong on the member: %d", n)
	}
}

// ── resume ──────────────────────────────────────────────────────────────────

// A resumed run must not finish on another machine than the one its recorded
// steps ran on: one green result over two trees is the failure the tier guard
// exists to prevent, in another dress.
func TestResumeRefusesAMemberChange(t *testing.T) {
	fakeSSH(t)
	box := t.TempDir()
	h, dir := memberWFHarness(t, alwaysReply(t, "ok"), box)
	text := "steps:\n  build:\n    run: echo built\n    member: box\n  after:\n    run: echo done\n"
	_, st, r := runWF(t, h, dir, text, nil)
	r.Close()
	if ss := stepByName(st, "after"); ss == nil || ss.Member != localMemberName {
		t.Fatalf("after: %+v", ss)
	}
	// Re-binding the same file against the same fleet is fine.
	if err := checkResumeFleet(loadWF(t, h, dir, text, nil), st); err != nil {
		t.Fatalf("an unchanged fleet must resume: %v", err)
	}
	// The operator edits roles.yaml between the two invocations: defaults: member:
	// now points at another machine, so `after` would run there.
	other := t.TempDir()
	h.orch.members["other"] = &Member{Name: "other", Rem: &Remote{Name: "other", Host: "box01", Dir: other}}
	h.orch.memOrd = append(h.orch.memOrd, "other")
	h.orch.defMem = "other"
	err := checkResumeFleet(loadWF(t, h, dir, text, nil), st)
	if err == nil {
		t.Fatal("a resume that would move a step to another machine must be refused")
	}
	for _, want := range []string{"after", localMemberName, "other"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in the refusal: %v", want, err)
		}
	}
}

// ── /role save ──────────────────────────────────────────────────────────────

// LCA_REMOTE repoints a DECLARED members.remote; /role save must not drop that
// member, and with it the narrower allowlist the operator gave that machine.
func TestRoleSaveKeepsADeclaredRemoteMember(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	p := filepath.Join(dir, "roles.yaml")
	t.Setenv("LCA_ROLES", p)
	t.Setenv("LCA_REMOTE", "new-host:/new/dir")
	cfg := Config{Root: dir, Dir: filepath.Join(dir, "cfg")}
	write := func(s string) *RolesConfig {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		rc, err := loadRoles(cfg)
		if err != nil {
			t.Fatalf("does not load: %v\n%s", err, s)
		}
		return rc
	}
	rc := write("members:\n  remote:\n    host: old\n    dir: /old\n    allow: [ls]\n  box:\n    host: box\n    dir: /b\nroles:\n  lead:\n    models: [lead-a]\n")
	saved := rc.YAML()
	back := write(saved)
	m := back.Members[legacyMemberName]
	if m == nil || strings.Join(m.Allow, ",") != "ls" {
		t.Fatalf("/role save widened member remote's sandbox:\n%s", saved)
	}
	if back.Members["box"] == nil {
		t.Fatalf("/role save lost a member:\n%s", saved)
	}
}

// ── what a failed probe is about ────────────────────────────────────────────

// The probe proves two things at once (ssh answers, and the project directory is
// there), so its failure has to say WHICH — one is fixed with ssh keys or a VPN,
// the other with mkdir or a corrected dir:.
func TestProbeTellsSSHFromAMissingDirectory(t *testing.T) {
	fakeSSH(t)
	gone := filepath.Join(t.TempDir(), "does-not-exist")
	rem := &Remote{Name: "gone-box", Host: "box01", Dir: gone}
	err := rem.ensureUp(context.Background())
	if err == nil {
		t.Fatal("a missing project directory must fail the member")
	}
	if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "fix ssh") {
		t.Fatalf("ssh answered, so the directory is the fault: %v", err)
	}
	for _, want := range []string{"member gone-box", gone} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in the failure: %v", want, err)
		}
	}
	if !memberDirMissing(err) {
		t.Fatal("doctor has to be able to tell the two apart without reading the text")
	}
	// The transport's own failure keeps its wording, and its hint.
	t.Setenv("LCA_TEST_SSH_DOWN", "box01")
	down := &Remote{Name: "dead-box", Host: "box01", Dir: t.TempDir()}
	err = down.ensureUp(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is unreachable") {
		t.Fatalf("a host that does not answer is unreachable: %v", err)
	}
	if memberDirMissing(err) {
		t.Fatal("ssh never connected, so nothing is known about the directory")
	}
}

// A member that answered once and then went away must not turn ssh's own exit
// into "your change did not pass": the model cannot fix a VPN, and every verify
// attempt would be spent on it.
func TestMemberThatDiesMidRunIsAnError(t *testing.T) {
	fakeSSH(t)
	box := t.TempDir()
	fs := alwaysReply(t, "ok")
	fs.models = allModels()
	roles := strings.Replace(testRoles, "sandbox:\n", "members:\n  box:\n    host: box01\n    dir: "+box+"\nsandbox:\n", 1)
	roles = strings.Replace(roles, "  coder:\n", "  coder:\n    member: box\n", 1)
	h := newRoleHarness(t, fs, roles, true)
	child, err := h.orch.newChild(h.sess, h.orch.agents["coder"], "x")
	if err != nil {
		t.Fatal(err)
	}
	child.view = &quietView{}
	if got := child.runTool(context.Background(), "echo hi", 30*time.Second, nil); !strings.Contains(got, "hi") {
		t.Fatalf("the member answers first: %q", got)
	}
	t.Setenv("LCA_TEST_SSH_DOWN", "box01") // the VPN drops
	v := child.RunVerifiedAll(context.Background(), []string{"ls"}, 2)
	if v.Status != "error" || !strings.Contains(v.Tail, "member box") {
		t.Fatalf("a transport failure is an error naming the member: %+v", v)
	}
	if v.Attempts > 1 {
		t.Fatalf("it must not burn the verifier's attempts: %d", v.Attempts)
	}
	if err := h.orch.member("box").reach(context.Background()); err == nil {
		t.Fatal("/members and doctor must not keep reporting a machine that stopped answering")
	}
}

// Neither of the two remaining "if the member is unknown" spots may fall back to
// another machine. Bind cannot produce an unknown name today; a new door into
// either of them must fail loudly rather than run somewhere else.
func TestUnknownMemberFailsInsteadOfFallingBack(t *testing.T) {
	fakeSSH(t)
	box := t.TempDir()
	h, dir := memberWFHarness(t, alwaysReply(t, "ok"), box)
	r := buildWF(t, h, dir, "steps:\n  a:\n    run: echo hi\n", nil)
	defer r.Close()
	step := r.wf.Steps[0]
	step.Member = "ghost"
	out, exit := r.shellStep(context.Background(), step, "echo hi", 10*time.Second)
	if exit == 0 || !strings.Contains(out, "ghost") {
		t.Fatalf("a step on a member that is not one: %q exit %d", out, exit)
	}
	var res delegateResult
	captureStdout(t, func() {
		tc := &ToolCtx{Ctx: context.Background(), S: h.sess, Name: "delegate", Member: "ghost"}
		if err := json.Unmarshal([]byte(runDelegateTool(tc, Args{"role": "coder", "task": "t"})), &res); err != nil {
			t.Fatal(err)
		}
	})
	if res.Status != "error" || !strings.Contains(res.TestTail, "ghost") {
		t.Fatalf("a delegation on a member that is not one: %+v", res)
	}
}

// bind asks the machine that will run the step: on a member the far side's shell
// really does act on the operators, so a pipeline whose every segment is
// allowlisted is legal there while a smuggled command is refused before the run —
// and a local step keeps the one-argv refusal, which is what happens locally.
func TestWorkflowBindChecksAMemberStepAsAShellLine(t *testing.T) {
	fakeSSH(t)
	box := t.TempDir()
	h, dir := memberWFHarness(t, alwaysReply(t, "ok"), box)
	bind := func(yaml string) error {
		wf, err := parseWorkflow("t", filepath.Join(dir, "t.yaml"), yaml)
		if err != nil {
			return err
		}
		return wf.bind(h.orch, "lead", wf.effectiveVars(nil))
	}
	if err := bind("steps:\n  a:\n    run: echo hi | cat\n    member: box\n"); err != nil {
		t.Fatalf("an allowlisted pipeline on a member: %v", err)
	}
	err := bind("steps:\n  a:\n    run: echo hi ; /usr/bin/touch x\n    member: box\n")
	if err == nil || !strings.Contains(err.Error(), "not on the allowlist") {
		t.Fatalf("a smuggled command must be refused at bind: %v", err)
	}
	if err := bind("steps:\n  a:\n    run: echo hi | cat\n"); err == nil || !strings.Contains(err.Error(), "one argv per command") {
		t.Fatalf("a local step: %v", err)
	}
}

// A command that never ran anywhere — the member is unreachable — and one killed
// by its own timeout are FAILED tool calls. They used to be recorded as clean:
// the trace said ok and /stats counted no error for work that did not happen.
func TestUnreachableAndTimedOutCommandsAreFailedCalls(t *testing.T) {
	t.Run("unreachable member", func(t *testing.T) {
		res := remoteCmdResult("ssh: connect to host box01 port 22: Operation timed out", -1, time.Second)
		if !strings.HasPrefix(res, "error:") {
			t.Fatalf("execCall marks a call failed by this prefix: %q", res)
		}
	})
	t.Run("local timeout", func(t *testing.T) {
		root := t.TempDir()
		j, err := NewJail(root, []string{"sleep"}, false)
		if err != nil {
			t.Fatal(err)
		}
		res := runCommand(context.Background(), j, "sleep 5", 200*time.Millisecond, nil)
		if !strings.HasPrefix(res, "error:") || !strings.Contains(res, "timed out") {
			t.Fatalf("a killed command is a failed call: %q", res)
		}
	})
}

// A member failure must name the file that declared it. Three roles.yaml files
// merge into one team, so a stale member in the project's own .lca/roles.yaml
// silently becomes every role's default machine — and "member remote: /proj is
// not there" leaves the operator grepping for the line to fix.
func TestMemberFailureNamesItsSource(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".lca"), 0o755)
	path := filepath.Join(root, ".lca", "roles.yaml")
	os.WriteFile(path, []byte("members:\n  box:\n    host: nosuchhost.invalid\n    dir: /proj\n"+
		"roles:\n  onbox:\n    member: box\n    models: [m1]\n"), 0o644)
	rc, err := loadRoles(Config{Root: root, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := rc.Members["box"]
	if m == nil || m.Rem == nil {
		t.Fatalf("member not parsed: %+v", rc.Members)
	}
	if m.Rem.Src != path {
		t.Fatalf("member source = %q, want %q", m.Rem.Src, path)
	}
	// and the source reaches the message the operator actually reads
	if got := m.Rem.declaredIn(); !strings.Contains(got, path) {
		t.Fatalf("failure text does not name the file: %q", got)
	}
}
