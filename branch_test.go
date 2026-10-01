package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Part 2's tests. Everything here runs against REAL git in a throwaway
// repository, because every claim the design makes is a claim about git's
// behaviour and a fake would only test the fake. The three-way cases skip on a
// git without `merge-tree --write-tree`, probed the way §2.5 probes it.

// branchRoles is testRoles with the one switch flipped and an integrator
// declared. check_cmd is deliberately a CONTENT check and not `test -f`: the
// integrator inherits the failed delegation's check, and a check that passes on
// any tree cannot tell a resolution from a shrug.
const branchRoles = `entry: lead
transport: native
apply: branch
defaults:
  context: 64000
  verify_attempts: 1
  check_timeout: 30
sandbox:
  allow: [ls, cat, echo, git, test, grep, sh, go, true, false]
roles:
  lead:
    description: Plans and delegates.
    models: [lead-a]
    tools: [read_file, grep, list_dir, delegate, write, edit]
  coder:
    description: Makes code changes.
    models: [coder-a]
    tools: [read_file, write, edit, run_command, list_dir]
    context: 32000
    check_cmd: grep -q BOTH f.txt
  integrator:
    description: Resolves merges nobody else could.
    models: [integ-a]
    tools: [read_file, write, edit, run_command, list_dir]
    context: 32000
`

// branchRolesNoIntegrator is the same team with the role withdrawn, which is
// how a conflict reaches the human instead of a third agent.
var branchRolesNoIntegrator = strings.Replace(branchRoles, `  integrator:
    description: Resolves merges nobody else could.
    models: [integ-a]
    tools: [read_file, write, edit, run_command, list_dir]
    context: 32000
`, "", 1)

// lastMsg is how these fakes decide what to do next: the real model sees the
// conversation, and a fake that counts requests instead breaks the moment a
// retry or a parallel sibling changes the order.
func lastMsg(req fakeRequest) (role, content string) {
	if len(req.Messages) == 0 {
		return "", ""
	}
	m := req.Messages[len(req.Messages)-1]
	role, _ = m["role"].(string)
	content, _ = m["content"].(string)
	return role, content
}

// readThenWrite is every coder in this file: read_file first, because Part 1's
// guard refuses a whole-file `write` to a file this session has not been shown
// — which is the point of Part 1 and must stay true inside a delegation too.
func readThenWrite(req fakeRequest, path, body string) fakeReply {
	role, content := lastMsg(req)
	switch {
	case role != "tool":
		return fakeReply{calls: []ToolCall{call("r", "read_file", map[string]any{"path": path})}}
	case strings.HasPrefix(content, path+":\n"):
		return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": path, "content": body})}}
	}
	return fakeReply{content: "wrote " + path}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, errs, code := gitRun(dir, nil, nil, args...)
	if code != 0 {
		t.Fatalf("git %v in %s failed (%d): %s%s", args, dir, code, out, errs)
	}
	return strings.TrimSpace(out)
}

// skipNo3Way gates on the usage STRING and never on the exit code: with no
// arguments inside a repository git exits 129 and prints its usage, and outside
// one it exits 128 with "not a git repository", which is not an answer about the
// flag.
func skipNo3Way(t *testing.T, top string) {
	t.Helper()
	if _, errs, _ := gitRun(top, nil, nil, "merge-tree", "--write-tree"); !strings.Contains(errs, "--write-tree") {
		t.Skip("this git has no `merge-tree --write-tree`; the 3-way path is unavailable by design")
	}
}

// twentyLines is long enough that two edits ten lines apart do not collide:
// git merges hunks with three lines of context, so in a short file almost any
// two agents conflict and a test written on a 3-line file would only ever prove
// that.
func twentyLines(line10 string) string {
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		if i == 10 {
			fmt.Fprintln(&b, line10)
			continue
		}
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func withLine(src, want string, n int) string {
	ls := strings.Split(strings.TrimRight(src, "\n"), "\n")
	ls[n-1] = want
	return strings.Join(ls, "\n") + "\n"
}

func seedFile(t *testing.T, h *harness, body string) {
	t.Helper()
	os.WriteFile(filepath.Join(h.root, "f.txt"), []byte(body), 0o644)
	gitT(t, h.root, "add", "-A")
	gitT(t, h.root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed f.txt")
}

func lcaBranchNames(t *testing.T, top string) []string {
	t.Helper()
	var out []string
	for _, b := range listBranches(top) {
		out = append(out, b.name)
	}
	return out
}

func delegateJSON(t *testing.T, h *harness) []delegateResult {
	t.Helper()
	var out []delegateResult
	for _, m := range h.sess.Msgs {
		if m.Role != "tool" || m.Tool != "delegate" {
			continue
		}
		var keys map[string]any
		if err := json.Unmarshal([]byte(m.Content), &keys); err != nil {
			t.Fatalf("delegate must return JSON: %v\n%s", err, m.Content)
		}
		for k := range keys {
			switch k {
			case "status", "diff", "test_tail", "review":
			default:
				t.Fatalf("delegate grew a result key %q under apply: branch — the contract is byte-stable", k)
			}
		}
		var r delegateResult
		json.Unmarshal([]byte(m.Content), &r)
		out = append(out, r)
	}
	return out
}

// ── 1. a clean caller ───────────────────────────────────────────────────────

func TestBranchCleanCallerMergesAndKeepsTheBranch(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			return readThenWrite(req, "f.txt", twentyLines("BOTH coder"))
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "merged"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "coder", "task": "touch line 10"})}}
	})
	fs.models = []string{"lead-a", "coder-a", "integ-a"}
	h := newRoleHarness(t, fs, branchRoles, true)
	skipNo3Way(t, h.root)
	seedFile(t, h, twentyLines("line 10"))

	headBefore := gitT(t, h.root, "rev-parse", "HEAD")
	refBefore := gitT(t, h.root, "symbolic-ref", "HEAD")
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}

	res := delegateJSON(t, h)
	if len(res) != 1 || res[0].Status != "passed" {
		t.Fatalf("delegate result: %+v", res)
	}
	if !strings.Contains(res[0].TestTail, "merged lca/coder/") {
		t.Fatalf("the caller was not told what merged:\n%s", res[0].TestTail)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "f.txt")); !strings.Contains(string(got), "BOTH coder") {
		t.Fatalf("the merge did not reach the caller's working tree:\n%s", got)
	}

	// The branch outlives the worktree, which is the whole point of the mode.
	names := lcaBranchNames(t, h.root)
	if len(names) != 1 || !strings.HasPrefix(names[0], "lca/coder/") {
		t.Fatalf("expected one lca/coder/* branch, got %v", names)
	}
	branch := names[0]
	if body := gitT(t, h.root, "show", branch+":f.txt"); !strings.Contains(body, "BOTH coder") {
		t.Fatalf("the work is not on the branch:\n%s", body)
	}
	if subj := gitT(t, h.root, "log", "-1", "--format=%s", branch); !strings.HasPrefix(subj, "coder: attempt 1") {
		t.Fatalf("commit subject %q — the engine commits one commit per attempt", subj)
	}

	// §3: HEAD, the checked-out branch and the index are never touched.
	if h := gitT(t, h.root, "rev-parse", "HEAD"); h != headBefore {
		t.Fatalf("HEAD moved: %s -> %s", headBefore, h)
	}
	if r := gitT(t, h.root, "symbolic-ref", "HEAD"); r != refBefore {
		t.Fatalf("the checked-out branch changed: %s -> %s", refBefore, r)
	}
	if st := gitT(t, h.root, "diff", "--cached", "--name-only"); st != "" {
		t.Fatalf("the caller's index was written: %q", st)
	}
	// `git branch` must not list the bookkeeping refs, and the integration commit
	// must make the ancestry question answerable in one command.
	if out := gitT(t, h.root, "branch", "--list"); strings.Contains(out, "refs/lca") || strings.Contains(out, "yours") {
		t.Fatalf("bookkeeping refs leaked into git branch:\n%s", out)
	}
	sid := refWord(h.sess.rootUID())
	if _, _, code := gitRun(h.root, nil, nil, "merge-base", "--is-ancestor", branch, integratedRef(sid)); code != 0 {
		t.Fatalf("%s is not an ancestor of %s, so nothing can answer \"is it integrated?\"", branch, integratedRef(sid))
	}
	for _, b := range listBranches(h.root) {
		if b.name == branch && !b.integrated {
			t.Fatal("listBranches says the merged branch is unintegrated")
		}
	}
}

// ── 2. a dirty caller ───────────────────────────────────────────────────────

func TestBranchDirtyCallerKeepsEveryUncommittedThing(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			// The snapshot it works from already carries the caller's dirty line 3,
			// so its whole-file write must preserve it — a subagent that cannot see
			// the caller's uncommitted work is working on a different program.
			return readThenWrite(req, "f.txt", withLine(twentyLines("BOTH coder"), "line 3 — human, mid-flight", 3))
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "merged"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "coder", "task": "touch line 10"})}}
	})
	fs.models = []string{"lead-a", "coder-a", "integ-a"}
	h := newRoleHarness(t, fs, branchRoles, true)
	skipNo3Way(t, h.root)
	seedFile(t, h, twentyLines("line 10"))
	os.WriteFile(filepath.Join(h.root, "g.txt"), []byte("staged\n"), 0o644)
	gitT(t, h.root, "add", "g.txt")
	gitT(t, h.root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "g")

	// A dirty tracked file, a half-staged file (MM) and an untracked one: the
	// three shapes the snapshot has to carry without disturbing.
	os.WriteFile(filepath.Join(h.root, "f.txt"), []byte(withLine(twentyLines("line 10"), "line 3 — human, mid-flight", 3)), 0o644)
	os.WriteFile(filepath.Join(h.root, "g.txt"), []byte("staged change\n"), 0o644)
	gitT(t, h.root, "add", "g.txt")
	os.WriteFile(filepath.Join(h.root, "g.txt"), []byte("staged change\nand an unstaged one\n"), 0o644)
	os.WriteFile(filepath.Join(h.root, "untracked.txt"), []byte("mine\n"), 0o644)

	headBefore := gitT(t, h.root, "rev-parse", "HEAD")
	stagedBefore := gitT(t, h.root, "diff", "--cached")
	statusBefore := gitT(t, h.root, "status", "--short")

	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	if res := delegateJSON(t, h); len(res) != 1 || res[0].Status != "passed" {
		t.Fatalf("delegate result: %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(h.root, "f.txt"))
	if !strings.Contains(string(got), "BOTH coder") {
		t.Fatalf("the role's change did not land:\n%s", got)
	}
	if !strings.Contains(string(got), "line 3 — human, mid-flight") {
		t.Fatalf("the caller's own dirty line was overwritten:\n%s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(h.root, "untracked.txt")); string(b) != "mine\n" {
		t.Fatalf("an untracked file changed: %q", b)
	}
	if h2 := gitT(t, h.root, "rev-parse", "HEAD"); h2 != headBefore {
		t.Fatalf("HEAD moved: %s -> %s", headBefore, h2)
	}
	if s := gitT(t, h.root, "diff", "--cached"); s != stagedBefore {
		t.Fatalf("the staged/unstaged split moved:\nbefore:\n%s\nafter:\n%s", stagedBefore, s)
	}
	// f.txt's status may legitimately change (it was already M and stays M), but
	// nothing may appear or disappear.
	if s := gitT(t, h.root, "status", "--short"); s != statusBefore {
		t.Fatalf("git status changed shape:\nbefore:\n%s\nafter:\n%s", statusBefore, s)
	}
}

// ── 3. two results, one file ────────────────────────────────────────────────

func TestBranchTwoResultsOneFileBothLand(t *testing.T) {
	var once sync.Once
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			// The two tasks are told apart by their own task text, which is the only
			// thing that distinguishes two identical parallel children.
			if strings.Contains(req.Body, "change line 2") {
				return readThenWrite(req, "f.txt", withLine(twentyLines("BOTH ten"), "two — by coder A", 2))
			}
			return readThenWrite(req, "f.txt", withLine(twentyLines("BOTH ten"), "eighteen — by coder B", 18))
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "both merged"}
		}
		var reply fakeReply
		once.Do(func() {
			reply = fakeReply{calls: []ToolCall{
				call("d1", "delegate", map[string]any{"role": "coder", "task": "change line 2"}),
				call("d2", "delegate", map[string]any{"role": "coder", "task": "change line eighteen"}),
			}}
		})
		if len(reply.calls) == 0 {
			return fakeReply{content: "done"}
		}
		return reply
	})
	fs.models = []string{"lead-a", "coder-a", "integ-a"}
	h := newRoleHarness(t, fs, branchRoles, true)
	skipNo3Way(t, h.root)
	seedFile(t, h, withLine(twentyLines("BOTH ten"), "line 2", 2))

	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	res := delegateJSON(t, h)
	if len(res) != 2 {
		t.Fatalf("expected two delegations, got %d", len(res))
	}
	for _, r := range res {
		if r.Status != "passed" {
			t.Fatalf("a delegation did not pass: %+v", r)
		}
	}
	body, _ := os.ReadFile(filepath.Join(h.root, "f.txt"))
	for _, want := range []string{"two — by coder A", "eighteen — by coder B"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("%q is missing — one result was silently lost:\n%s", want, body)
		}
	}
	// The result line names the order; nothing else about ordering is promised.
	joined := res[0].TestTail + res[1].TestTail
	if !strings.Contains(joined, "integrated 1st of 2") || !strings.Contains(joined, "integrated 2nd of 2") {
		t.Fatalf("the integration order was not reported:\n%s", joined)
	}
	if names := lcaBranchNames(t, h.root); len(names) != 2 {
		t.Fatalf("expected two branches, got %v", names)
	}
}

// ── 4 and 5. a genuine conflict ─────────────────────────────────────────────

// conflictServer makes the caller's tree move UNDER the delegation: the human
// edits line 10 while the role is editing line 10. That is the collision the
// whole design exists for, and it cannot be staged any other way — a snapshot
// taken before the human's edit is exactly what the role works from.
func conflictServer(t *testing.T, root string, integ func(req fakeRequest) fakeReply) *fakeServer {
	var moved sync.Once
	return newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		switch {
		case req.Model == "coder-a":
			r := readThenWrite(req, "f.txt", twentyLines("BOTH coder's line 10"))
			// AFTER the role has read its snapshot copy, so the role's own view is
			// the pre-edit one: this is the human touching the same region while the
			// delegation runs, which is the only way the collision can be staged.
			moved.Do(func() {
				os.WriteFile(filepath.Join(root, "f.txt"),
					[]byte(twentyLines("the human's own line 10")), 0o644)
			})
			return r
		case req.Model == "integ-a":
			return integ(req)
		case strings.Contains(req.Body, `"role":"tool"`):
			return fakeReply{content: "I see the conflict"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "coder", "task": "rewrite line 10"})}}
	})
}

func TestBranchConflictWritesNothingAndKeepsTheBranch(t *testing.T) {
	// The fake endpoint has to WRITE to the caller's tree mid-delegation, so the
	// root exists before the server does.
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	fs := conflictServer(t, root, func(fakeRequest) fakeReply { return fakeReply{content: "no integrator here"} })
	fs.models = []string{"lead-a", "coder-a"}
	h := newRoleHarnessAt(t, root, fs, branchRolesNoIntegrator, true)
	skipNo3Way(t, h.root)
	seedFile(t, h, twentyLines("line 10"))

	headBefore := gitT(t, h.root, "rev-parse", "HEAD")
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	res := delegateJSON(t, h)
	if len(res) != 1 || res[0].Status != "conflict" {
		t.Fatalf("expected status conflict, got %+v", res)
	}
	body, _ := os.ReadFile(filepath.Join(h.root, "f.txt"))
	if strings.Contains(string(body), "BOTH coder's line 10") {
		t.Fatalf("a conflicted merge was written to the caller's tree:\n%s", body)
	}
	if strings.Contains(string(body), "<<<<<<<") {
		t.Fatalf("conflict markers reached the caller's working tree:\n%s", body)
	}
	if !strings.Contains(string(body), "the human's own line 10") {
		t.Fatalf("the caller's own line did not survive:\n%s", body)
	}
	if h2 := gitT(t, h.root, "rev-parse", "HEAD"); h2 != headBefore {
		t.Fatal("HEAD moved on a conflict")
	}
	// The three sides must be in the object store and `cat-file`-able, because
	// that is the only thing the model is given to resolve it with.
	tail := res[0].TestTail
	if !strings.Contains(tail, "did not merge into your tree") || !strings.Contains(tail, "NOTHING was written") {
		t.Fatalf("the conflict was not explained:\n%s", tail)
	}
	oids := 0
	for _, line := range strings.Split(tail, "\n") {
		if i := strings.Index(line, "git cat-file -p "); i >= 0 {
			oid := strings.Fields(line[i+len("git cat-file -p "):])[0]
			if _, _, code := gitRun(h.root, nil, nil, "cat-file", "-p", oid); code != 0 {
				t.Fatalf("the conflict named %s, which is not in the object store", oid)
			}
			oids++
		}
	}
	if oids != 3 {
		t.Fatalf("expected three resolvable sides, got %d:\n%s", oids, tail)
	}
	if names := lcaBranchNames(t, h.root); len(names) != 1 {
		t.Fatalf("the branch must survive a conflict, got %v", names)
	}
}

func TestBranchIntegratorResolvesOnce(t *testing.T) {
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	// A resolution that keeps BOTH intentions — and BOTH is what the inherited
	// check greps for, so the verifier, not the integrator, decides it worked.
	fs := conflictServer(t, root, func(req fakeRequest) fakeReply {
		return readThenWrite(req, "f.txt", twentyLines("BOTH coder's line 10 AND the human's own line 10"))
	})
	fs.models = []string{"lead-a", "coder-a", "integ-a"}
	h := newRoleHarnessAt(t, root, fs, branchRoles, true)
	skipNo3Way(t, h.root)
	seedFile(t, h, twentyLines("line 10"))

	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	res := delegateJSON(t, h)
	if len(res) != 1 || res[0].Status != "passed" {
		t.Fatalf("the integrator resolved it, so the delegation passed: %+v", res)
	}
	body, _ := os.ReadFile(filepath.Join(h.root, "f.txt"))
	if !strings.Contains(string(body), "BOTH coder's line 10 AND the human's own line 10") {
		t.Fatalf("the resolution did not reach the caller's tree:\n%s", body)
	}
	if !strings.Contains(res[0].TestTail, "integrator") {
		t.Fatalf("the caller was not told who resolved it:\n%s", res[0].TestTail)
	}
	// A merge branch exists next to the work branch: both are kept, and only
	// `lca clean` removes either.
	names := lcaBranchNames(t, h.root)
	merge := 0
	for _, n := range names {
		if strings.HasPrefix(n, "lca/merge/") {
			merge++
		}
	}
	if merge != 1 {
		t.Fatalf("expected one lca/merge/* branch, got %v", names)
	}
}

func TestBranchIntegratorFailsAndTheRunStops(t *testing.T) {
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	// No BOTH: the inherited check fails, so this is a resolution the verifier
	// refuses — exactly the case that must not be retried behind anyone's back.
	fs := conflictServer(t, root, func(req fakeRequest) fakeReply {
		return readThenWrite(req, "f.txt", twentyLines("only the human's line 10"))
	})
	fs.models = []string{"lead-a", "coder-a", "integ-a"}
	h := newRoleHarnessAt(t, root, fs, branchRoles, true)
	skipNo3Way(t, h.root)
	seedFile(t, h, twentyLines("line 10"))

	before, _ := os.ReadFile(filepath.Join(h.root, "f.txt"))
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	res := delegateJSON(t, h)
	if len(res) != 1 || res[0].Status != "conflict" {
		t.Fatalf("a failed resolution leaves the conflict standing: %+v", res)
	}
	tail := res[0].TestTail
	for _, want := range []string{"THE RUN STOPS HERE", "one supervised attempt", "tell the user"} {
		if !strings.Contains(tail, want) {
			t.Fatalf("the hand-over to the human is missing %q:\n%s", want, tail)
		}
	}
	// Nothing written, and both the branch and the merge worktree kept.
	now, _ := os.ReadFile(filepath.Join(h.root, "f.txt"))
	if string(now) == string(before) {
		// before was read after the human's mid-flight edit had NOT yet happened,
		// so equality here would mean the edit never ran and the test proved nothing.
		t.Fatal("the caller's tree never moved, so no conflict was staged")
	}
	if !strings.Contains(string(now), "the human's own line 10") || strings.Contains(string(now), "BOTH coder") {
		t.Fatalf("the caller's tree was written after a failed resolution:\n%s", now)
	}
	work, merge := 0, 0
	for _, n := range lcaBranchNames(t, h.root) {
		switch {
		case strings.HasPrefix(n, "lca/merge/"):
			merge++
		case strings.HasPrefix(n, "lca/coder/"):
			work++
		}
	}
	if work != 1 || merge != 1 {
		t.Fatalf("the work branch and the merge worktree are the hand-over; got %d work, %d merge", work, merge)
	}
	// The merge worktree is still checked out, with the markers in it.
	if !strings.Contains(tail, "git merge --abort") {
		t.Fatalf("the human was not told how to look at or abandon the merge:\n%s", tail)
	}
	// And the worktree is actually in the state that sentence promises. commitWork
	// has already committed the integrator's attempt by the time its check is
	// judged, and `git commit` during a merge CONSUMES MERGE_HEAD — so the human
	// used to be sent to run `git merge --abort` where it exits 128, in a tree
	// `git status` called clean, holding the resolution the verifier had REJECTED
	// with nothing to say so.
	dir := mergeWorktreeIn(tail)
	if dir == "" {
		t.Fatalf("the hand-over does not name the merge worktree:\n%s", tail)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the merge worktree is gone: %v", err)
	}
	if _, _, code := gitRun(dir, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD"); code != 0 {
		t.Fatalf("there is no MERGE_HEAD in %s, so `git merge --abort` exits 128 — the hand-over text is a lie", dir)
	}
	if u := gitT(t, dir, "ls-files", "-u"); u == "" {
		t.Fatalf("the three stages are gone from %s, so there is no way to see the three sides", dir)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "f.txt")); err != nil || !strings.Contains(string(b), "<<<<<<< ") {
		t.Fatalf("%s/f.txt holds no conflict markers:\n%s", dir, b)
	}
	if _, _, code := gitRun(dir, nil, nil, "merge", "--abort"); code != 0 {
		t.Fatal("`git merge --abort` in the worktree the human was handed does not work")
	}
	// The rejected attempt is not thrown away by that restore: it is readable on a
	// ref, and the hand-over names it.
	refs := eachRef(h.root, "refs/lca/attempt/")
	if len(refs) != 1 {
		t.Fatalf("the integrator's rejected attempt was discarded: %v", refs)
	}
	if !strings.Contains(tail, refs[0]) {
		t.Fatalf("the hand-over does not say where the rejected attempt is (%s):\n%s", refs[0], tail)
	}
	if body := gitT(t, h.root, "show", refs[0]+":f.txt"); !strings.Contains(body, "only the human's line 10") {
		t.Fatalf("the kept attempt is not the one that was rejected:\n%s", body)
	}
}

// TestIntegratorGetsExactlyOneAttempt pins the operator's own binding decision.
// resolveConflict passed o.verifyAttempts(), which is the TEAM's budget for a
// coder fixing its own work against a check it can read — configurable to any
// number — so with the default of 2 the failed check was fed back and the
// integrator rewrote the merge a second time, while its prompt said "This is the
// only attempt" and the hand-over said "one supervised attempt". The fixture the
// other integrator tests use pins verify_attempts: 1, so nothing could catch it.
func TestIntegratorGetsExactlyOneAttempt(t *testing.T) {
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	var integCalls int
	var mu sync.Mutex
	fs := conflictServer(t, root, func(req fakeRequest) fakeReply {
		mu.Lock()
		if role, _ := lastMsg(req); role != "tool" {
			integCalls++
		}
		mu.Unlock()
		// A resolution the inherited check refuses, every time.
		return readThenWrite(req, "f.txt", twentyLines("only the human's line 10"))
	})
	fs.models = []string{"lead-a", "coder-a", "integ-a"}
	roles := strings.Replace(branchRoles, "verify_attempts: 1", "verify_attempts: 3", 1)
	if roles == branchRoles {
		t.Fatal("the fixture no longer pins verify_attempts, so raising it proves nothing")
	}
	h := newRoleHarnessAt(t, root, fs, roles, true)
	skipNo3Way(t, h.root)
	seedFile(t, h, twentyLines("line 10"))
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	if res := delegateJSON(t, h); len(res) != 1 || res[0].Status != "conflict" {
		t.Fatalf("a failed resolution leaves the conflict standing: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if integCalls != 1 {
		t.Fatalf("the integrator was given %d attempts; with verify_attempts: 3 the cap must still be the 1 this call site owns", integCalls)
	}
}

// TestIntegratorResolutionMeetsTheAskRules: resolveConflict acted only on Deny,
// so an `Ask` verdict fell straight through and mw.integrate wrote the files
// with no prompt. The integrator's jail is the WHOLE merge worktree, so its
// resolution can carry any file it edited — not only the conflicted ones and not
// only those in the delegation's reviewed diff. The same file arriving through
// the delegation's own path would have raised a question.
func TestIntegratorResolutionMeetsTheAskRules(t *testing.T) {
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	fs := conflictServer(t, root, func(req fakeRequest) fakeReply {
		role, content := lastMsg(req)
		switch {
		case role != "tool":
			return fakeReply{calls: []ToolCall{call("r", "read_file", map[string]any{"path": "f.txt"})}}
		case strings.HasPrefix(content, "f.txt:\n"):
			return fakeReply{calls: []ToolCall{call("r2", "read_file", map[string]any{"path": "db/migrations/001.sql"})}}
		case strings.HasPrefix(content, "db/migrations/001.sql:\n"):
			// Resolve the conflict AND tidy an unrelated file nobody reviewed, which
			// its jail — the whole merge worktree — lets it reach.
			return fakeReply{calls: []ToolCall{
				call("w", "write", map[string]any{"path": "f.txt", "content": twentyLines("BOTH coder's line 10 AND the human's")}),
				call("w2", "write", map[string]any{"path": "db/migrations/001.sql", "content": "-- tidied\n"}),
			}}
		}
		return fakeReply{content: "resolved"}
	})
	fs.models = []string{"lead-a", "coder-a", "integ-a"}
	// The rule on the CALLER's role, which is where the gate is evaluated — the
	// same place a delegation's own diff meets it (mergeBranch's MERGE prompt).
	// Not a user-wide rule, because the integrator writing inside its own
	// worktree is the job and must not be asked about; what must be asked about
	// is that file arriving in the caller's tree.
	roles := strings.Replace(branchRoles,
		"    tools: [read_file, grep, list_dir, delegate, write, edit]\n",
		"    tools: [read_file, grep, list_dir, delegate, write, edit]\n    permission:\n      edit:\n        \"*\": allow\n        \"db/migrations/**\": ask\n", 1)
	if roles == branchRoles {
		t.Fatal("the permission block was not inserted, so this test proves nothing")
	}
	// approve=false: every question is answered no, so an unapproved write must
	// not reach the caller's tree.
	h := newRoleHarnessAt(t, root, fs, roles, false)
	skipNo3Way(t, h.root)
	os.MkdirAll(filepath.Join(h.root, "db", "migrations"), 0o755)
	os.WriteFile(filepath.Join(h.root, "db", "migrations", "001.sql"), []byte("-- original\n"), 0o644)
	seedFile(t, h, twentyLines("line 10"))

	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(h.root, "db", "migrations", "001.sql")); string(b) != "-- original\n" {
		t.Fatalf("the integrator's unreviewed migration was written into the caller's tree with no prompt: %q", b)
	}
	res := delegateJSON(t, h)
	if len(res) != 1 || res[0].Status != "conflict" {
		t.Fatalf("an unapproved resolution leaves the conflict standing: %+v", res)
	}
	if !strings.Contains(res[0].TestTail, "not approved") {
		t.Fatalf("the caller was not told the resolution was refused:\n%s", res[0].TestTail)
	}
}

// ── 6. a killed process ─────────────────────────────────────────────────────

func TestBranchKilledIntegrationIsReportedAndRecoverable(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	// Two files so a half-applied patch is a real state and not a coin toss.
	writeAll(t, top, map[string]string{"a.txt": twentyLines("a10"), "b.txt": twentyLines("b10")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")

	w := delegationBranch(t, top, env, "lca/coder/killed-t1", map[string]string{
		"a.txt": withLine(twentyLines("a10"), "a — by the role", 4),
		"b.txt": withLine(twentyLines("b10"), "b — by the role", 4),
	})
	// Half the patch landed and then the process died: a.txt is the role's,
	// b.txt is not, and a journal says an integration was interrupted.
	writeAll(t, top, map[string]string{"a.txt": withLine(twentyLines("a10"), "a — by the role", 4)})
	os.WriteFile(filepath.Join(top, "mine.txt"), []byte("my own dirty file\n"), 0o644)
	journal := writeJournal(env.stateDir, "killed", "t1", w.branch)
	if journal == "" {
		t.Fatal("no journal was written")
	}
	// A dead pid, which is the only thing that distinguishes wreckage from an
	// integration running right now in another lca.
	os.WriteFile(journal, []byte(`{"branch":"`+w.branch+`","pid":999999,"started":"2026-01-01T00:00:00Z"}`), 0o600)

	js := readJournals(env.stateDir)
	if len(js) != 1 || !strings.Contains(interruptedText(js[0]), "may be half patched") {
		t.Fatalf("an interrupted integration must be reported: %+v", js)
	}

	// Re-running the ordinary integration IS the recovery.
	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	n, conflicts, err := w.integrate(context.Background(), env, "coder", "killed", "t1")
	if err != nil || conflicts != "" {
		t.Fatalf("the recovery must be an ordinary integration: n=%d conflicts=%q err=%v", n, conflicts, err)
	}
	if n != 1 {
		t.Fatalf("exactly the missing file should land, got %d", n)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "b.txt")); !strings.Contains(string(b), "b — by the role") {
		t.Fatalf("the missing half did not land:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "mine.txt")); string(b) != "my own dirty file\n" {
		t.Fatalf("the caller's dirty file was disturbed: %q", b)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("a finished integration must remove its journal")
	}
	// A second run is a no-op: the 0-byte patch is what makes recovery safe to
	// repeat, which is what lets `lca merge` be offered without a dry run.
	n2, c2, err := w.integrate(context.Background(), env, "coder", "killed", "t1")
	if err != nil || c2 != "" || n2 != 0 {
		t.Fatalf("re-integrating must be a no-op: n=%d conflicts=%q err=%v", n2, c2, err)
	}
}

// ── 7. lca clean --branches keeps what it must keep ─────────────────────────

func TestCleanBranchesKeepsWhatItMustKeep(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	writeAll(t, top, map[string]string{"f.txt": twentyLines("ten")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")

	// (a) integrated and old, (b) integrated and fresh, (c) never integrated,
	// (d) integrated but held by a live worktree. Four different lines, because
	// four branches touching one line would simply conflict with each other and
	// the test would prove nothing about cleanup.
	mk := func(name, line string, at int) *worktree {
		return delegationBranch(t, top, env, name, map[string]string{"f.txt": withLine(twentyLines("ten"), line, at)})
	}
	land := func(w *worktree, sid, task string) {
		t.Helper()
		if err := w.changedAgainst(w.branch); err != nil {
			t.Fatal(err)
		}
		if _, c, err := w.integrate(context.Background(), env, "coder", sid, task); err != nil || c != "" {
			t.Fatalf("integrate %s: %v %q", w.branch, err, c)
		}
	}
	// The worktree is removed BEFORE the branch is integrated or backdated:
	// `branch -D` refuses a branch a worktree holds, which is the whole reason
	// `lca clean` has to remove worktrees first.
	//
	// And it is the INTEGRATION that is backdated, not the branch tip. The day is
	// measured from when the work landed, because an integration interrupted on
	// Monday and recovered on Wednesday integrates a branch whose newest commit is
	// two days old — and the tip's date would let the same minute's `lca clean`
	// delete it before the human could look.
	old := mk("lca/coder/old-t1", "by old", 4)
	old.remove()
	land(old, "old", "t1")
	backdate(t, top, integratedRef("old"))

	fresh := mk("lca/coder/fresh-t2", "by fresh", 8)
	fresh.remove()
	land(fresh, "fresh", "t2")

	never := mk("lca/coder/never-t3", "by never", 12)
	never.remove()

	held := mk("lca/coder/held-t4", "by held", 16)
	land(held, "held", "t4")

	cfg := Config{Root: top, Dir: env.stateDir}
	if code := cleanRepo(top, cfg, true, false, nil); code != 0 {
		t.Fatalf("lca clean exited %d", code)
	}
	have := map[string]bool{}
	for _, n := range lcaBranchNames(t, top) {
		have[n] = true
	}
	if have["lca/coder/old-t1"] {
		t.Fatal("an integrated branch older than a day should have been deleted")
	}
	for _, keep := range []string{"lca/coder/fresh-t2", "lca/coder/never-t3", "lca/coder/held-t4"} {
		if !have[keep] {
			t.Fatalf("%s was deleted and must not have been (branches now: %v)", keep, have)
		}
	}
	// Naming an unintegrated branch is the one way to delete it, and git's own
	// refusal is what protects a branch a worktree holds.
	if code := cleanRepo(top, cfg, true, false, []string{"lca/coder/never-t3"}); code != 0 {
		t.Fatalf("lca clean exited %d", code)
	}
	have = map[string]bool{}
	for _, n := range lcaBranchNames(t, top) {
		have[n] = true
	}
	if have["lca/coder/never-t3"] {
		t.Fatal("a named branch should be deleted even unintegrated")
	}
	if !have["lca/coder/held-t4"] {
		t.Fatal("a branch a live worktree holds must survive")
	}
	if _, errs, code := gitRun(top, nil, nil, "branch", "-D", "lca/coder/held-t4"); code == 0 {
		t.Fatal("git deleted a branch a worktree holds — the cleanup order is no longer forced")
	} else if !strings.Contains(errs, "used by worktree") {
		t.Fatalf("git refused for an unexpected reason: %s", errs)
	}
	// --dry-run changes nothing.
	fresh2 := gitT(t, top, "rev-parse", "refs/heads/lca/coder/fresh-t2")
	cleanRepo(top, cfg, true, true, []string{"lca/coder/fresh-t2"})
	if gitT(t, top, "rev-parse", "refs/heads/lca/coder/fresh-t2") != fresh2 {
		t.Fatal("--dry-run deleted a branch")
	}
	// A branch a HUMAN made under lca/, pointing into the project's own history.
	// It is an ancestor of every integration commit (their history reaches back
	// through the snapshot to HEAD), its tip is as old as that commit, and its
	// name is under lca/ — so the name, the ancestry and the clock all say
	// "delete". Only the authorship record says otherwise, and it is the one that
	// counts: never-list item 4 is "a branch named lca/… that LCA CREATED".
	gitT(t, top, "branch", "lca/my-spike", "HEAD")
	for _, b := range listBranches(top) {
		if b.name != "lca/my-spike" {
			continue
		}
		if b.ours {
			t.Fatal("a branch lca never made is claimed as lca's own")
		}
		if b.integrated {
			t.Fatal("a branch lca never made is reported integrated, which is what licenses a delete")
		}
	}
	if code := cleanRepo(top, cfg, true, false, []string{"lca/my-spike"}); code != 0 {
		t.Fatalf("lca clean exited %d", code)
	}
	if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/lca/my-spike"); code != 0 {
		t.Fatal("lca deleted a branch it did not create, even named")
	}
	// And naming something that is not an lca branch at all is refused rather
	// than silently accepted: `lca clean --branches main` used to resolve, enter
	// the want set and print "nothing to clean".
	if n := resolveBranchName(top, "main"); n != "" {
		t.Fatalf("resolveBranchName accepted %q as a branch lca made here", n)
	}
	if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/main"); code != 0 {
		t.Fatal("the lab repository has no main branch, so the assertion above proves nothing")
	}
}

// ── the regression that matters most: apply: verified is untouched ──────────

func TestApplyVerifiedStaysDetachedAndPatchBased(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "created done.txt"}
			}
			return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "done.txt", "content": "ok\n"})}}
		}
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "applied"}
		}
		return fakeReply{calls: []ToolCall{call("d", "delegate", map[string]any{"role": "coder", "task": "create done.txt"})}}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, true)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	if res := delegateJSON(t, h); len(res) != 1 || res[0].Status != "passed" {
		t.Fatalf("delegate result: %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "done.txt")); string(got) != "ok\n" {
		t.Fatalf("the patch path stopped working: %q", got)
	}
	if names := lcaBranchNames(t, h.root); len(names) != 0 {
		t.Fatalf("apply: verified must make no branches, got %v", names)
	}
	if refs := eachRef(h.root, "refs/lca/"); len(refs) != 0 {
		t.Fatalf("apply: verified must make no bookkeeping refs, got %v", refs)
	}
}

// ── the small invariants, each a measured fact ──────────────────────────────

func TestBranchNamesAreRefSafeAndCollisionFree(t *testing.T) {
	if got := branchFor("coder", "20260930-141233-4412", "t3"); got != "lca/coder/20260930-141233-4412-t3" {
		t.Fatalf("branchFor: %s", got)
	}
	// Anything git-check-ref-format would reject must not reach a ref name: a
	// branch that cannot be created is a delegation that cannot run, and one that
	// cannot be deleted is wreckage nobody can remove.
	for _, bad := range []string{"a b", "a..b", "-lead-", "..", "~x^y:z?", ""} {
		got := branchFor(bad, "s", "t")
		if _, _, code := gitRun(t.TempDir(), nil, nil, "check-ref-format", "--branch", got); code != 0 {
			t.Fatalf("branchFor(%q) produced the unusable name %q", bad, got)
		}
	}
	top, _ := bareRepo(t)
	writeAll(t, top, map[string]string{"x": "x\n"})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "x")
	gitT(t, top, "branch", "lca/coder/s-t1")
	taken := freeBranch(top, "lca/coder/s-t1")
	if taken == "lca/coder/s-t1" {
		t.Fatal("a reused name must get a suffix")
	}
	if _, _, code := gitRun(top, nil, nil, "check-ref-format", "--branch", taken); code != 0 {
		t.Fatalf("the collision suffix made an unusable name %q", taken)
	}
	// The suffix must ROUND TRIP through splitBranch, because everything keyed on
	// the session id reads it back out of the branch name: `/merge` looks up
	// refs/lca/yours/<sid> and `lca clean` files the branch under <sid>. With a
	// dash suffix the session id came back one component short, and the operator
	// was told that session's snapshot had been cleaned up while it sat right there.
	for _, want := range [][3]string{
		{"lca/coder/20260930-141233-4412-t3", "20260930-141233-4412", "t3"},
		{taken, "s", "t1"},
	} {
		sid, task := splitBranch(want[0])
		if sid != want[1] {
			t.Fatalf("splitBranch(%q) session = %q, want %q", want[0], sid, want[1])
		}
		if !strings.HasPrefix(task, want[2]) {
			t.Fatalf("splitBranch(%q) task = %q, want it to start with %q", want[0], task, want[2])
		}
	}
}

func TestParseMergeTreeReadsStagesNotPositions(t *testing.T) {
	out := strings.Join([]string{
		"a7b0f9e8c1d2f3a4b5c6d7e8f9a0b1c2d3e4f5a6",
		"100644 6551e4f0000000000000000000000000000000aa 1\tf.txt",
		"100644 c55beed0000000000000000000000000000000bb 2\tf.txt",
		"100644 47ffa480000000000000000000000000000000cc 3\tf.txt",
		"",
		"Auto-merging f.txt",
		"CONFLICT (content): Merge conflict in f.txt",
	}, "\n")
	mc := parseMergeTree(out)
	if len(mc.paths) != 1 || mc.paths[0] != "f.txt" {
		t.Fatalf("paths: %v", mc.paths)
	}
	st := mc.stages["f.txt"]
	if st.base == "" || st.ours == "" || st.theirs == "" {
		t.Fatalf("stages: %+v", st)
	}
	if len(mc.notes) != 2 {
		t.Fatalf("notes: %v", mc.notes)
	}
	// modify/delete: no stage 3, and the text must say so rather than print an
	// empty oid the model would try to cat-file.
	md := parseMergeTree("tree\n100644 aaaaaaa 1\tg.txt\n100644 bbbbbbb 2\tg.txt\n\nCONFLICT (modify/delete): g.txt deleted")
	text := md.text("lca/coder/x-t1", "coder", "")
	if strings.Contains(text, "cat-file -p \n") || strings.Count(text, "cat-file -p") != 2 {
		t.Fatalf("a missing stage must not be printed as a command:\n%s", text)
	}
	if !strings.Contains(text, "no coder's side") {
		t.Fatalf("the missing side must be named:\n%s", text)
	}
}

func TestAgentCommitDoesNotRunTheUsersHooks(t *testing.T) {
	top, env := bareRepo(t)
	writeAll(t, top, map[string]string{"f.txt": "one\n"})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	hooks := filepath.Join(top, ".git", "hooks")
	os.MkdirAll(hooks, 0o755)
	// `worktree add` passes -c core.hooksPath once; a later `git commit` in that
	// worktree does NOT inherit it, which is the measured fact this test pins.
	os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\necho HOOK-RAN >&2\nexit 1\n"), 0o755)

	w, err := (&worktrees{}).create(top, env.stateDir, "s", "lca/coder/hook-t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.remove() })
	writeAll(t, w.root, map[string]string{"f.txt": "two\n"})

	// The control: without the flag the user's hook really does block an agent's
	// commit, so the assertion below is about commitWork and not about nothing.
	gitT(t, w.dir, "add", "-A")
	if _, errs, code := gitRun(w.dir, nil, nil, "commit", "-m", "would be blocked"); code == 0 || !strings.Contains(errs, "HOOK-RAN") {
		t.Fatalf("the hook did not block a plain commit, so this test proves nothing: %d %s", code, errs)
	}

	s := &Session{agent: &Agent{Name: "coder"}, branch: w.branch}
	if err := s.commitWork(context.Background(), w, 1); err != nil {
		t.Fatalf("commitWork ran the user's hook: %v", err)
	}
	if body := gitT(t, top, "show", w.branch+":f.txt"); body != "two" {
		t.Fatalf("the commit did not reach the branch: %q", body)
	}
	if subj := gitT(t, top, "log", "-1", "--format=%s", w.branch); subj != "coder: attempt 1" {
		t.Fatalf("commit subject %q", subj)
	}
	// A second call with nothing to commit is a no-op, not an empty commit.
	before := gitT(t, top, "rev-parse", w.branch)
	if err := s.commitWork(context.Background(), w, 2); err != nil {
		t.Fatal(err)
	}
	if gitT(t, top, "rev-parse", w.branch) != before {
		t.Fatal("commitWork made an empty commit on a clean tree")
	}
	// A session with no branch is today's behaviour to the byte.
	none := &Session{agent: &Agent{Name: "coder"}}
	if err := none.commitWork(context.Background(), w, 1); err != nil {
		t.Fatalf("commitWork must be a no-op without a branch: %v", err)
	}
}

func TestSnapshotExcludesTheAgentWorktreesInBothModes(t *testing.T) {
	top, env := bareRepo(t)
	writeAll(t, top, map[string]string{"f.txt": "one\n"})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	// The hazard case: the state directory inside the project, which the README
	// documents as <root>/.lca.
	inside := filepath.Join(top, ".lca")
	os.MkdirAll(filepath.Join(inside, "worktrees"), 0o700)
	wt, err := (&worktrees{}).create(top, inside, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.remove()
	snap, err := snapshotOf(top, inside, "probe", gitT(t, top, "rev-parse", "HEAD"), gitT(t, top, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	tree := gitT(t, top, "ls-tree", "-r", snap)
	if strings.Contains(tree, "160000") {
		t.Fatalf("the agent's worktree was recorded as a gitlink:\n%s", tree)
	}
	if strings.Contains(tree, ".lca/worktrees") {
		t.Fatalf("the agent's worktree is inside the snapshot:\n%s", tree)
	}
	_ = env
}

func TestUnbornHeadSnapshotBranchAndIntegration(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	// Nothing committed at all: the snapshot becomes a root commit.
	writeAll(t, top, map[string]string{"f.txt": twentyLines("ten")})
	w := delegationBranch(t, top, env, "lca/coder/unborn-t1",
		map[string]string{"f.txt": withLine(twentyLines("ten"), "by the role", 4)})
	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	n, conflicts, err := w.integrate(context.Background(), env, "coder", "unborn", "t1")
	if err != nil || conflicts != "" || n != 1 {
		t.Fatalf("unborn HEAD: n=%d conflicts=%q err=%v", n, conflicts, err)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "f.txt")); !strings.Contains(string(b), "by the role") {
		t.Fatalf("nothing landed:\n%s", b)
	}
	if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "HEAD"); code == 0 {
		t.Fatal("HEAD was born — the integration committed in the user's checkout")
	}
}

func TestIntegrateRefusesWhenAPatchNoLongerApplies(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	writeAll(t, top, map[string]string{"f.txt": twentyLines("ten")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	w := delegationBranch(t, top, env, "lca/coder/race-t1",
		map[string]string{"f.txt": withLine(twentyLines("ten"), "by the role", 4)})
	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	// A rival writer on the same region. The single retry re-snapshots, so this
	// becomes an honest conflict rather than the thrown-away delegation it is today.
	os.WriteFile(filepath.Join(top, "f.txt"), []byte(withLine(twentyLines("ten"), "by a rival", 4)), 0o644)
	n, conflicts, err := w.integrate(context.Background(), env, "coder", "race", "t1")
	if err != nil {
		t.Fatalf("a collision is not an error: %v", err)
	}
	if n != 0 || conflicts == "" {
		t.Fatalf("expected a refusal, got n=%d conflicts=%q", n, conflicts)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "f.txt")); !strings.Contains(string(b), "by a rival") {
		t.Fatalf("the rival's bytes were overwritten:\n%s", b)
	}
}

func TestMergeTreeProbeGatesOnTheUsageString(t *testing.T) {
	top, _ := bareRepo(t)
	writeAll(t, top, map[string]string{"f.txt": "x\n"})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "x")
	o := &Orchestrator{}
	ok, ver := o.mergeTree3Way(top)
	if ver == "" {
		t.Fatal("the probe must name the git version, because the fallback message carries it")
	}
	// Whatever this git answers, the probe must be stable and must not depend on
	// the exit code: run outside a repository the same command exits 128.
	ok2, _ := o.mergeTree3Way(top)
	if ok != ok2 {
		t.Fatal("the probe is not cached")
	}
	out, errs, code := gitRun(top, nil, nil, "merge-tree", "--write-tree")
	if code == 0 {
		t.Fatalf("a bare `merge-tree --write-tree` must fail, not succeed: %s", out)
	}
	if ok != strings.Contains(errs, "--write-tree") {
		t.Fatalf("the probe disagrees with git's own usage text: ok=%v stderr=%q", ok, errs)
	}
}

func TestApplyBranchIsValidatedAndEverythingElseRefused(t *testing.T) {
	for _, v := range []string{"verified", "always", "never", "branch"} {
		root := t.TempDir()
		os.MkdirAll(filepath.Join(root, ".lca"), 0o755)
		os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"),
			[]byte("entry: lead\napply: "+v+"\nroles:\n  lead:\n    models: [m]\n"), 0o644)
		rc, err := loadRoles(Config{Root: root})
		if err != nil {
			t.Fatalf("apply: %s must be accepted: %v", v, err)
		}
		if rc.Apply != v {
			t.Fatalf("apply: %s was read as %q", v, rc.Apply)
		}
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".lca"), 0o755)
	os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"),
		[]byte("entry: lead\napply: branchy\nroles:\n  lead:\n    models: [m]\n"), 0o644)
	_, err := loadRoles(Config{Root: root})
	if err == nil || !strings.Contains(err.Error(), "verified, always, never or branch") {
		t.Fatalf("the validator must name all four values: %v", err)
	}
}

// ── fixtures ────────────────────────────────────────────────────────────────

// bareRepo is a throwaway repository and the integration environment that goes
// with it. No Orchestrator, no gateway, no session: integrateEnv exists so that
// the integration can be tested against real git and nothing else.
func bareRepo(t *testing.T) (string, integrateEnv) {
	t.Helper()
	top := t.TempDir()
	if real, err := filepath.EvalSymlinks(top); err == nil {
		top = real
	}
	gitT(t, top, "init", "-q")
	gitT(t, top, "config", "user.email", "t@t")
	gitT(t, top, "config", "user.name", "t")
	state := t.TempDir()
	return top, integrateEnv{jailRoot: top, lockDir: leaseDir(top, state), stateDir: state}
}

func writeAll(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// delegationBranch is what a delegation leaves behind, without a model: a
// snapshot of the caller's tree, a worktree on a real branch cut from it, the
// role's files written there, and one commit per attempt made by commitOnBranch
// — the same commands commitWork runs.
func delegationBranch(t *testing.T, top string, env integrateEnv, branch string, files map[string]string) *worktree {
	t.Helper()
	m := &worktrees{}
	w, err := m.create(top, env.stateDir, "s", branch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.remove() })
	writeAll(t, w.root, files)
	if st, _, _ := gitRun(w.dir, nil, nil, "status", "--porcelain"); strings.TrimSpace(st) == "" {
		t.Fatal("the fixture wrote nothing")
	}
	gitT(t, w.dir, "add", "-A")
	gitT(t, w.dir, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "coder: attempt 1 — fixture")
	return w
}

// backdate rewrites a ref's tip with a committer date more than keptFor ago.
// Done with commit-tree and the date in the environment, because that is the
// only thing listBranches can read and the only thing `lca clean` decides on.
// backdate rewrites a ref's tip two days into the past, keeping EVERY parent.
// Keeping them is not cosmetic: an integration commit's second parent is the
// branch it merged, and dropping it makes `merge-base --is-ancestor` answer "not
// integrated" — so a backdate that kept only the first parent would quietly test
// the wrong branch state.
func backdate(t *testing.T, top, ref string) {
	t.Helper()
	old := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	args := []string{"commit-tree", gitT(t, top, "rev-parse", ref+"^{tree}")}
	for _, p := range strings.Fields(gitT(t, top, "log", "-1", "--format=%P", ref)) {
		args = append(args, "-p", p)
	}
	args = append(args, "-m", gitT(t, top, "log", "-1", "--format=%s", ref)+" — backdated")
	out, errs, code := gitRun(top, []string{"GIT_AUTHOR_DATE=" + old, "GIT_COMMITTER_DATE=" + old}, nil, args...)
	if code != 0 {
		t.Fatalf("commit-tree: %s", errs)
	}
	gitT(t, top, "update-ref", ref, strings.TrimSpace(out))
}

// ── the human's route: /merge and /merge --finish ───────────────────────────

func TestMergeWorktreeShowsMarkersAndFinishWritesTheResolution(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	writeAll(t, top, map[string]string{"f.txt": twentyLines("ten")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")

	w := delegationBranch(t, top, env, "lca/coder/human-t1",
		map[string]string{"f.txt": twentyLines("the role's ten")})
	// The human touches the same region while the delegation runs.
	os.WriteFile(filepath.Join(top, "f.txt"), []byte(twentyLines("the human's ten")), 0o644)
	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	n, conflicts, err := w.integrate(context.Background(), env, "coder", "human", "t1")
	if err != nil || conflicts == "" || n != 0 {
		t.Fatalf("expected a refused merge: n=%d conflicts=%q err=%v", n, conflicts, err)
	}
	callerBefore, _ := os.ReadFile(filepath.Join(top, "f.txt"))

	o := &Orchestrator{cfg: Config{Root: top, Dir: env.stateDir}}
	jl, err := NewJail(top, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	o.jl = jl
	mw, conflicted, err := o.mergeWorktree(top, "human", "t1", w.branch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mw.remove() })
	if conflicted != "f.txt" {
		t.Fatalf("git should report f.txt unmerged, got %q", conflicted)
	}
	// A real merge: UU, three stages, and markers labelled with the REF names —
	// a raw oid would give `<<<<<<< 75954fff…` and tell the human nothing.
	if st := gitT(t, mw.dir, "status", "--porcelain"); !strings.Contains(st, "UU f.txt") {
		t.Fatalf("not a real merge:\n%s", st)
	}
	if u := gitT(t, mw.dir, "ls-files", "-u"); strings.Count(u, "\n") != 2 {
		t.Fatalf("expected three stages:\n%s", u)
	}
	body, _ := os.ReadFile(filepath.Join(mw.dir, "f.txt"))
	// HEAD, not refs/lca/yours/<sid>: the merge branch is CHECKED OUT here, so
	// git labels our side HEAD. The spec's own §2.6 measurement says the same;
	// its test list's "naming refs/lca/yours" was the stale half, and the
	// integrator's prompt was wrong until this test said so.
	for _, want := range []string{"<<<<<<< HEAD", ">>>>>>> " + w.branch} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing marker label %q:\n%s", want, body)
		}
	}
	// The caller's own tree is untouched while all that exists.
	if now, _ := os.ReadFile(filepath.Join(top, "f.txt")); string(now) != string(callerBefore) {
		t.Fatal("the caller's working tree changed while a merge was checked out elsewhere")
	}

	// The human resolves and commits.
	os.WriteFile(filepath.Join(mw.dir, "f.txt"), []byte(twentyLines("both tens, by hand")), 0o644)
	gitT(t, mw.dir, "add", "-A")
	gitT(t, mw.dir, "-c", "user.email=h@h", "-c", "user.name=h", "-c", "core.hooksPath="+os.DevNull,
		"commit", "-q", "-m", "resolved by hand")

	// §2.6, before anything is written: the base must be the D this worktree was
	// cut from and NOT the delegation's snapshot. Against the snapshot the
	// resolution re-conflicts with itself, because it carries the caller's side
	// of the conflict and the snapshot never saw it.
	if own := readOwner(mw.dir); own.base == "" || own.source != w.branch {
		t.Fatalf("the merge worktree does not record what it was cut from: %+v", own)
	}
	snapBased := &worktree{top: top, dir: mw.dir, root: mw.root, sub: mw.sub, base: w.base, branch: mw.branch}
	if err := snapBased.changedAgainst(mw.branch); err != nil {
		t.Fatal(err)
	}
	if n, c, err := snapBased.integrate(context.Background(), env, "coder", "human", "t1"); err != nil || c == "" || n != 0 {
		t.Fatalf("the snapshot as base must re-conflict: n=%d conflicts=%q err=%v", n, c, err)
	}
	if now, _ := os.ReadFile(filepath.Join(top, "f.txt")); string(now) != string(callerBefore) {
		t.Fatal("the refused attempt wrote to the caller's tree")
	}

	// And now the real one, with the recorded base.
	mergeFinish(top, Config{Root: top, Dir: env.stateDir}, env, "", "lca merge")
	got, _ := os.ReadFile(filepath.Join(top, "f.txt"))
	if !strings.Contains(string(got), "both tens, by hand") {
		t.Fatalf("the resolution did not reach the caller's tree:\n%s", got)
	}
	if strings.Contains(string(got), "<<<<<<<") {
		t.Fatalf("markers reached the caller's tree:\n%s", got)
	}
}

func TestMergeFinishRefusesAnUnresolvedWorktree(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	writeAll(t, top, map[string]string{"f.txt": twentyLines("ten")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	w := delegationBranch(t, top, env, "lca/coder/unres-t1", map[string]string{"f.txt": twentyLines("role")})
	os.WriteFile(filepath.Join(top, "f.txt"), []byte(twentyLines("human")), 0o644)
	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	if _, c, _ := w.integrate(context.Background(), env, "coder", "unres", "t1"); c == "" {
		t.Fatal("expected a conflict")
	}
	o := &Orchestrator{cfg: Config{Root: top, Dir: env.stateDir}}
	jl, _ := NewJail(top, nil, true)
	o.jl = jl
	mw, _, err := o.mergeWorktree(top, "unres", "t1", w.branch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mw.remove() })
	before, _ := os.ReadFile(filepath.Join(top, "f.txt"))
	// Markers still in the file and nothing committed: finishing must write
	// nothing at all, because a half-resolved merge in the user's tree is the one
	// outcome worse than today's discard.
	mergeFinish(top, Config{Root: top, Dir: env.stateDir}, env, "", "lca merge")
	after, _ := os.ReadFile(filepath.Join(top, "f.txt"))
	if string(after) != string(before) {
		t.Fatalf("an unresolved merge was written:\n%s", after)
	}
}

// ── wreckage: what a killed lca leaves, and what survives the sweep ─────────

func TestCleanRemovesOrphansAndLeavesLiveSiblingsAlone(t *testing.T) {
	top, env := bareRepo(t)
	writeAll(t, top, map[string]string{"f.txt": "one\n"})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")

	// A worktree a SIGKILL orphaned: the directory and the branch are real, and
	// only the owner file's dead pid distinguishes it from the live one below.
	dead := delegationBranch(t, top, env, "lca/coder/dead-t1", map[string]string{"f.txt": "dead\n"})
	writeOwner(dead.dir, owner{session: "dead", role: "delegate", branch: dead.branch})
	body, err := os.ReadFile(dead.dir + ".owner")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(dead.dir+".owner", []byte(strings.Replace(string(body),
		fmt.Sprintf("pid=%d", os.Getpid()), "pid=999999", 1)), 0o600)

	// A sibling this very process is working in. `worktree prune` must carry
	// --expire or this one loses its admin entry and its next git command fails
	// with nothing anyone can act on.
	live := delegationBranch(t, top, env, "lca/coder/live-t2", map[string]string{"f.txt": "live\n"})

	// A lease whose holder is gone, and one this process holds.
	os.MkdirAll(env.lockDir, 0o700)
	staleLock := filepath.Join(env.lockDir, "deadbeefdeadbeef.lock")
	os.WriteFile(staleLock, []byte("999999\nf.txt\n2026-01-01T00:00:00Z\n"), 0o600)
	release, err := leaseFile(env.lockDir, "f.txt", filepath.Join(top, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	mine := filepath.Join(env.lockDir, leaseName(filepath.Join(top, "f.txt")))

	// A temporary index from a snapshot nobody finished.
	idx := filepath.Join(os.TempDir(), "lca-index-cleantest")
	os.WriteFile(idx, []byte("x"), 0o600)
	os.Chtimes(idx, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	defer os.Remove(idx)

	// Everything above is reported before it is removed: `/branches` and
	// `lca clean` print the same facts, and a human reads them before deciding.
	if got := orphanWorktrees(env.stateDir); len(got) != 1 || got[0].pid != 999999 {
		t.Fatalf("the orphan was not identified: %+v", got)
	}
	if got := staleLeases(env.lockDir); len(got) != 1 || got[0].pid != 999999 {
		t.Fatalf("the stale lease was not identified: %+v", got)
	}
	printBranches(top, env.stateDir, env.lockDir, true) // must not panic, changes nothing

	cfg := Config{Root: top, Dir: env.stateDir}
	if code := cleanRepo(top, cfg, false, false, nil); code != 0 {
		t.Fatalf("lca clean exited %d", code)
	}
	if _, err := os.Stat(dead.dir); !os.IsNotExist(err) {
		t.Fatal("the orphaned worktree was not removed")
	}
	if _, err := os.Stat(live.dir); err != nil {
		t.Fatalf("a live sibling's worktree was removed: %v", err)
	}
	if _, _, code := gitRun(live.dir, nil, nil, "status", "--porcelain"); code != 0 {
		t.Fatal("a bare prune removed the live worktree's admin entry")
	}
	if _, err := os.Stat(staleLock); !os.IsNotExist(err) {
		t.Fatal("the stale lease was not swept")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("a lease this process holds was swept out from under it")
	}
	if _, err := os.Stat(idx); !os.IsNotExist(err) {
		t.Fatal("the orphaned temporary index was not swept")
	}
	// Without --branches no branch is touched, not even the dead one's.
	have := map[string]bool{}
	for _, n := range lcaBranchNames(t, top) {
		have[n] = true
	}
	if !have["lca/coder/dead-t1"] || !have["lca/coder/live-t2"] {
		t.Fatalf("lca clean deleted a branch without --branches: %v", have)
	}
	if body := gitT(t, top, "show", "lca/coder/dead-t1:f.txt"); body != "dead" {
		t.Fatalf("the killed delegation's commit did not survive: %q", body)
	}
}

func TestIntegrateCarriesBinaryModeDeleteAndRename(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	bin := string([]byte{0, 1, 2, 3, 0xff, 0xfe, 0, 10})
	writeAll(t, top, map[string]string{
		"keep.txt": twentyLines("ten"),
		"gone.txt": "delete me\n",
		"old.txt":  twentyLines("rename me"),
		"b.bin":    bin,
	})
	os.Chmod(filepath.Join(top, "keep.txt"), 0o644)
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")

	m := &worktrees{}
	w, err := m.create(top, env.stateDir, "s", "lca/coder/shapes-t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.remove() })
	os.Remove(filepath.Join(w.root, "gone.txt"))
	os.Rename(filepath.Join(w.root, "old.txt"), filepath.Join(w.root, "new.txt"))
	os.WriteFile(filepath.Join(w.root, "b.bin"), []byte(bin+"\x00tail"), 0o644)
	os.WriteFile(filepath.Join(w.root, "fresh.txt"), []byte("brand new\n"), 0o644)
	os.Chmod(filepath.Join(w.root, "keep.txt"), 0o755)
	gitT(t, w.dir, "add", "-A")
	gitT(t, w.dir, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "coder: attempt 1 — every shape")

	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	n, conflicts, err := w.integrate(context.Background(), env, "coder", "shapes", "t1")
	if err != nil || conflicts != "" {
		t.Fatalf("n=%d conflicts=%q err=%v", n, conflicts, err)
	}
	if _, err := os.Stat(filepath.Join(top, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("a deletion did not cross")
	}
	if _, err := os.Stat(filepath.Join(top, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("the rename's old name is still there")
	}
	if b, _ := os.ReadFile(filepath.Join(top, "new.txt")); !strings.Contains(string(b), "rename me") {
		t.Fatalf("the rename's new name is missing or wrong: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "b.bin")); string(b) != bin+"\x00tail" {
		t.Fatalf("the binary file did not survive --binary: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "fresh.txt")); string(b) != "brand new\n" {
		t.Fatalf("a new file did not cross: %q", b)
	}
	info, err := os.Stat(filepath.Join(top, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the mode change did not cross: %v", info.Mode())
	}
}

// ── a commit that cannot happen ─────────────────────────────────────────────

// TestAgentCommitSurvivesASigningProject is the other half of
// TestAgentCommitDoesNotRunTheUsersHooks: a project that signs its commits makes
// `git commit` exit 128 inside an agent worktree no human will ever push from,
// and nobody can hand the agent a passphrase.
//
// What made that a blocker rather than an inconvenience is what happened NEXT: a
// warned-about commit failure left the branch at the snapshot, the check still
// passed, the diff was still non-empty so the integration ran, the merge of an
// empty branch produced a 0-byte patch, and the caller was handed `passed` plus
// the words "already in your working tree" while `defer wt.remove()` deleted the
// only copy of the work. So this test pins both: the commit succeeds, and a
// commit that does fail is an error and never a success.
func TestAgentCommitSurvivesASigningProject(t *testing.T) {
	top, env := bareRepo(t)
	writeAll(t, top, map[string]string{"f.txt": "one\n"})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	gitT(t, top, "config", "commit.gpgsign", "true")
	gitT(t, top, "config", "gpg.program", filepath.Join(top, "no-such-gpg"))

	w, err := (&worktrees{}).create(top, env.stateDir, "s", "lca/coder/sign-t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.remove() })
	writeAll(t, w.root, map[string]string{"f.txt": "two\n"})

	// The control: without the flag this project really does refuse an agent's
	// commit, so the assertion below is about commitWork and not about nothing.
	gitT(t, w.dir, "add", "-A")
	if _, _, code := gitRun(w.dir, nil, nil, "-c", "core.hooksPath="+os.DevNull, "commit", "-m", "would fail"); code == 0 {
		t.Skip("this git signs without a working gpg, so the control cannot be established")
	}

	s := &Session{agent: &Agent{Name: "coder"}, branch: w.branch}
	if err := s.commitWork(context.Background(), w, 1); err != nil {
		t.Fatalf("commitWork asked a project's signing config to sign an agent's scratch commit: %v", err)
	}
	if body := gitT(t, top, "show", w.branch+":f.txt"); body != "two" {
		t.Fatalf("the work is not on the branch: %q", body)
	}
}

// TestEmptyBranchIsNeverReportedAsIntegrated pins the second half of the same
// blocker, at the place that actually tells the caller. An empty branch and an
// already-integrated branch both yield a 0-byte patch, and reporting the first
// as the second is how a verified change is discarded while the model is told it
// landed.
func TestEmptyBranchIsNeverReportedAsIntegrated(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	writeAll(t, top, map[string]string{"f.txt": twentyLines("ten")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")

	// A branch made exactly as a delegation's is, with nothing committed onto it:
	// the state a failed commitWork leaves behind.
	m := &worktrees{}
	w, err := m.create(top, env.stateDir, "s", "lca/coder/empty-t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.remove() })
	writeAll(t, w.root, map[string]string{"f.txt": withLine(twentyLines("ten"), "by the role", 4)})
	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	n, conflicts, err := w.integrate(context.Background(), env, "coder", "empty", "t1")
	if err == nil {
		t.Fatalf("an empty branch must be an error, not success: n=%d conflicts=%q", n, conflicts)
	}
	if !strings.Contains(err.Error(), "holds no commit of its own") {
		t.Fatalf("the error must say the branch is empty, not that the branch is in the tree: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "f.txt")); strings.Contains(string(b), "by the role") {
		t.Fatalf("nothing was committed, so nothing may have been written:\n%s", b)
	}
}

// TestCommitWorkRefusesAnUnresolvedMerge pins never-list item 8 at its source.
// `git add -A` in a worktree with unmerged index entries does not stage a
// resolution — it resolves the index with whatever text is in the file, which
// after a half-done resolution is the conflict markers themselves.
func TestCommitWorkRefusesAnUnresolvedMerge(t *testing.T) {
	top, env := bareRepo(t)
	writeAll(t, top, map[string]string{"f.txt": twentyLines("ten")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	base := gitT(t, top, "rev-parse", "HEAD")

	// Two branches that genuinely conflict on line 10, merged in a worktree of
	// our own — the shape mergeWorktree leaves behind.
	gitT(t, top, "branch", "lca/side/one-t1", base)
	wt := filepath.Join(t.TempDir(), "mw")
	gitT(t, top, "-c", "core.hooksPath="+os.DevNull, "worktree", "add", "-q", wt, "lca/side/one-t1")
	t.Cleanup(func() { gitRun(top, nil, nil, "worktree", "remove", "--force", wt) })
	os.WriteFile(filepath.Join(wt, "f.txt"), []byte(twentyLines("ten, theirs")), 0o644)
	gitT(t, wt, "add", "-A")
	gitT(t, wt, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "theirs")
	gitT(t, top, "branch", "lca/side/two-t1", base)
	w2 := filepath.Join(t.TempDir(), "mw2")
	gitT(t, top, "-c", "core.hooksPath="+os.DevNull, "worktree", "add", "-q", w2, "lca/side/two-t1")
	t.Cleanup(func() { gitRun(top, nil, nil, "worktree", "remove", "--force", w2) })
	os.WriteFile(filepath.Join(w2, "f.txt"), []byte(twentyLines("ten, ours")), 0o644)
	gitT(t, w2, "add", "-A")
	gitT(t, w2, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "ours")
	if _, _, code := gitRun(w2, nil, nil, "-c", "core.hooksPath="+os.DevNull, "merge", "--no-ff", "-m", "m", "lca/side/one-t1"); code != 1 {
		t.Skipf("the fixture did not conflict (rc %d)", code)
	}
	if u := gitT(t, w2, "ls-files", "-u"); u == "" {
		t.Skip("no unmerged entries, so there is nothing to refuse")
	}

	s := &Session{agent: &Agent{Name: "integrator"}, branch: "lca/side/two-t1"}
	w := &worktree{mgr: &worktrees{}, top: top, dir: w2, root: w2, sub: ".", base: base, branch: "lca/side/two-t1"}
	if err := s.commitWork(context.Background(), w, 1); err == nil {
		body := gitT(t, top, "show", "lca/side/two-t1:f.txt")
		t.Fatalf("an unresolved merge was committed, markers and all:\n%s", body)
	}
	if u := gitT(t, w2, "ls-files", "-u"); u == "" {
		t.Fatal("the refusal consumed the three stages, which are the thing the human is handed")
	}
	if _, _, code := gitRun(w2, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD"); code != 0 {
		t.Fatal("MERGE_HEAD is gone, so `git merge --abort` no longer works")
	}
	_ = env
}

// TestIntegrateRefusesAPatchCarryingMarkers is never-list item 8 at the one
// place all three resolution routes pass through — the integrator role, `/merge
// --finish` and `lca merge --finish`. The route into it is real: a half-resolved
// merge worktree whose conflicted file is one the check does not look at.
func TestIntegrateRefusesAPatchCarryingMarkers(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	marked := "intro\n<<<<<<< HEAD\nyours\n=======\ntheirs\n>>>>>>> lca/coder/x-t1\nouttro\n"
	writeAll(t, top, map[string]string{"f.txt": twentyLines("ten"), "docs/x.md": "intro\nouttro\n"})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	w := delegationBranch(t, top, env, "lca/merge/marker-t1", map[string]string{"docs/x.md": marked})
	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	n, conflicts, err := w.integrate(context.Background(), env, "integrator", "marker", "t1")
	if err != nil {
		t.Fatalf("a marker-carrying patch is a refusal, not a transport error: %v", err)
	}
	if n != 0 || !strings.Contains(conflicts, "NOT resolved") || !strings.Contains(conflicts, "docs/x.md") {
		t.Fatalf("expected a refusal naming the file, got n=%d conflicts=%q", n, conflicts)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "docs", "x.md")); strings.Contains(string(b), "<<<<<<<") {
		t.Fatalf("conflict markers were written into the caller's own file:\n%s", b)
	}

	// And the guard is narrow: a Markdown heading underline is not a conflict.
	w2 := delegationBranch(t, top, env, "lca/merge/heading-t1",
		map[string]string{"docs/x.md": "Heading\n=======\n\nbody\n"})
	if err := w2.changedAgainst(w2.branch); err != nil {
		t.Fatal(err)
	}
	if _, conflicts, err := w2.integrate(context.Background(), env, "integrator", "heading", "t1"); err != nil || conflicts != "" {
		t.Fatalf("an underlined heading must not read as a conflict: conflicts=%q err=%v", conflicts, err)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "docs", "x.md")); !strings.Contains(string(b), "Heading") {
		t.Fatalf("the honest change did not land:\n%s", b)
	}
}

// ── what an interruption leaves, and what clears it ────────────────────────

// TestRecoveredIntegrationClearsTheJournalAndRecordsTheBranch is the
// interruption shape the earlier test did not drive, and it is the COMMON one:
// killing lca does not kill its `git apply` child, so the patch usually
// completes and only the commit-tree does not. The recovery then finds nothing
// to write — and that path returned before doing any bookkeeping at all.
//
// Two things went wrong and neither cleared: the journal survived the documented
// recovery and `lca clean` warned "your tree may be half patched" for ever, and
// no integration ref was written, so the branch was reported unintegrated and
// `lca clean --branches` could never reclaim it.
func TestRecoveredIntegrationClearsTheJournalAndRecordsTheBranch(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	writeAll(t, top, map[string]string{"a.txt": twentyLines("a10")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")

	w := delegationBranch(t, top, env, "lca/coder/recov-t1",
		map[string]string{"a.txt": withLine(twentyLines("a10"), "by the role", 4)})
	w.remove()
	// The apply finished; the commit-tree did not. This is what the tree looks
	// like after that kill.
	writeAll(t, top, map[string]string{"a.txt": withLine(twentyLines("a10"), "by the role", 4)})
	journal := writeJournal(env.stateDir, "recov", "t1", w.branch)
	os.WriteFile(journal, []byte(`{"branch":"`+w.branch+`","pid":999999,"started":"2026-01-01T00:00:00Z"}`), 0o600)

	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	n, conflicts, err := w.integrate(context.Background(), env, "coder", "recov", "t1")
	if err != nil || conflicts != "" {
		t.Fatalf("the recovery of an already-applied patch must be an ordinary no-op: n=%d %q %v", n, conflicts, err)
	}
	if n != 0 {
		t.Fatalf("the patch had already landed, so nothing should be written, got %d", n)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("the journal survived the documented recovery, so the half-patched warning can never be cleared")
	}
	if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", integratedRef("recov")); code != 0 {
		t.Fatalf("%s was never written, so nothing can answer \"is this branch integrated?\"", integratedRef("recov"))
	}
	var row *lcaBranch
	for i, b := range listBranches(top) {
		if b.name == w.branch {
			row = &listBranches(top)[i]
		}
	}
	if row == nil || !row.integrated {
		t.Fatalf("the branch is wholly in the tree and is still reported unintegrated: %+v", row)
	}
	// And `lca clean` has nothing left to say about it.
	cfg := Config{Root: top, Dir: env.stateDir}
	if code := cleanRepo(top, cfg, false, true, nil); code != 0 {
		t.Fatalf("lca clean --dry-run exited %d", code)
	}
	if js := readJournals(env.stateDir); len(js) != 0 {
		t.Fatalf("a journal is still outstanding: %+v", js)
	}
}

// TestCleanDropsAJournalWhoseBranchIsInTheTree is clean.go's own promise — "a
// journal whose branch is gone, OR WHOSE BRANCH IS NOW IN THE TREE, has nothing
// left to tell anyone" — which only the first half of was implemented. It is
// reachable whenever the recovery ran under a different task id from the
// interruption, which `lca merge --finish` always does.
func TestCleanDropsAJournalWhoseBranchIsInTheTree(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	writeAll(t, top, map[string]string{"a.txt": twentyLines("a10")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	w := delegationBranch(t, top, env, "lca/coder/jrnl-t1",
		map[string]string{"a.txt": withLine(twentyLines("a10"), "by the role", 4)})
	w.remove()
	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	if _, c, err := w.integrate(context.Background(), env, "coder", "jrnl", "t1"); err != nil || c != "" {
		t.Fatalf("integrate: %v %q", err, c)
	}
	// A journal from an earlier, interrupted attempt under a DIFFERENT task id:
	// dropping by filename cannot reach it, which is why the clean pass has to.
	journal := writeJournal(env.stateDir, "jrnl", "t0", w.branch)
	os.WriteFile(journal, []byte(`{"branch":"`+w.branch+`","pid":999999,"started":"2026-01-01T00:00:00Z"}`), 0o600)

	cfg := Config{Root: top, Dir: env.stateDir}
	if code := cleanRepo(top, cfg, false, false, nil); code != 0 {
		t.Fatalf("lca clean exited %d", code)
	}
	if js := readJournals(env.stateDir); len(js) != 0 {
		t.Fatalf("a warning that cannot be cleared is one the operator learns to ignore: %+v", js)
	}
}

// ── the resolution worktree belongs to a person ────────────────────────────

// TestCleanKeepsTheHumansMergeWorktree: `lca merge <branch>` from the command
// line records the pid of a process that exits the moment it finishes printing,
// so the worktree the human was just told to go and edit is an "orphan" within
// milliseconds — and `lca clean`, the no-flag operation documented as the safe
// one, force-removed their half-finished resolution, discarding the markers, the
// three stages and `git merge --abort` with it.
func TestCleanKeepsTheHumansMergeWorktree(t *testing.T) {
	top, env := bareRepo(t)
	skipNo3Way(t, top)
	writeAll(t, top, map[string]string{"f.txt": twentyLines("ten")})
	gitT(t, top, "add", "-A")
	gitT(t, top, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	w := delegationBranch(t, top, env, "lca/coder/handover-t1",
		map[string]string{"f.txt": withLine(twentyLines("ten"), "by the role", 10)})
	w.remove()
	// The caller's own side of the same region, so the merge really conflicts.
	os.WriteFile(filepath.Join(top, "f.txt"), []byte(withLine(twentyLines("ten"), "by the human", 10)), 0o644)
	if err := w.changedAgainst(w.branch); err != nil {
		t.Fatal(err)
	}
	if _, c, err := w.integrate(context.Background(), env, "coder", "handover", "t1"); err != nil || c == "" {
		t.Fatalf("the fixture must conflict: %v %q", err, c)
	}

	o := &Orchestrator{cfg: Config{Root: top, Dir: env.stateDir}}
	jl, err := NewJail(top, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	o.jl = jl
	mw, conflicted, err := o.mergeWorktree(top, "handover", "t1", w.branch)
	if err != nil {
		t.Fatal(err)
	}
	if conflicted == "" {
		t.Fatal("the merge worktree holds no conflict, so there is nothing to hand over")
	}
	// Exactly the state the CLI leaves: the owner pid is this process, which from
	// `lca merge`'s point of view has already exited. Stand in for that with a pid
	// that is certainly gone.
	body, err := os.ReadFile(mw.dir + ".owner")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(mw.dir+".owner", []byte(strings.Replace(string(body), fmt.Sprintf("pid=%d", os.Getpid()), "pid=999999", 1)), 0o600)

	if got := orphanWorktrees(env.stateDir); len(got) != 0 {
		t.Fatalf("a resolution worktree is not wreckage: %+v", got)
	}
	cfg := Config{Root: top, Dir: env.stateDir}
	if code := cleanRepo(top, cfg, false, false, nil); code != 0 {
		t.Fatalf("lca clean exited %d", code)
	}
	if _, err := os.Stat(mw.dir); err != nil {
		t.Fatalf("lca clean deleted the worktree the human was handed: %v", err)
	}
	if u := gitT(t, mw.dir, "ls-files", "-u"); u == "" {
		t.Fatal("the three stages are gone")
	}
	if _, _, code := gitRun(mw.dir, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD"); code != 0 {
		t.Fatal("MERGE_HEAD is gone, so `git merge --abort` no longer works")
	}
	// It is not immortal: once it is a day old, clean and not mid-merge, it goes.
	gitT(t, mw.dir, "merge", "--abort")
	old := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	body, _ = os.ReadFile(mw.dir + ".owner")
	os.WriteFile(mw.dir+".owner", []byte(regexp.MustCompile(`started=.*`).ReplaceAllString(string(body), "started="+old)), 0o600)
	if code := cleanRepo(top, cfg, false, false, nil); code != 0 {
		t.Fatalf("lca clean exited %d", code)
	}
	if _, err := os.Stat(mw.dir); !os.IsNotExist(err) {
		t.Fatalf("an abandoned, clean, day-old merge worktree must eventually be removed: %v", err)
	}
}

// TestWorktreeOwnerFallbackReadsAPidAndNotARandomSuffix: os.MkdirTemp appends a
// DECIMAL random suffix, so a directory named `<session>-<pid>-XXXXXX` ends in
// two numbers — and the fallback read the random one. Every worktree whose
// .owner file could not be read therefore reported a pid that has never
// existed, pidAlive said "gone", and `lca clean` force-removed a worktree a live
// lca was working in. Reachable on upgrade too: worktrees from an older build
// have no .owner file at all.
func TestWorktreeOwnerFallbackReadsAPidAndNotARandomSuffix(t *testing.T) {
	state := t.TempDir()
	dir, err := worktreeDir(state, "20260930-141233-4412")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No .owner file: the upgrade case, and the window between `worktree add` and
	// writeOwner.
	o := readOwner(dir)
	if o.pid != os.Getpid() {
		t.Fatalf("the fallback read pid %d out of %q; this process is %d", o.pid, filepath.Base(dir), os.Getpid())
	}
	if got := orphanWorktrees(state); len(got) != 0 {
		t.Fatalf("a worktree this very process owns was called an orphan: %+v", got)
	}
	// And a name with no labelled pid at all stays "owner unknown, leave it
	// alone", which is what readOwner's comment promises.
	odd := filepath.Join(state, "worktrees", "handmade-123456789")
	if err := os.MkdirAll(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	if p := readOwner(odd).pid; p != 0 {
		t.Fatalf("an unlabelled number was read as pid %d", p)
	}
	if got := orphanWorktrees(state); len(got) != 0 {
		t.Fatalf("a worktree with no establishable owner must be left alone: %+v", got)
	}
}

// TestConflictHandoverReachesTheOperatorsScreen: everything a conflict produces
// — the merge worktree, "the run stops here", the `lca merge` pointer — lived
// only inside the delegate tool result, i.e. inside the model's prompt, so
// whether the human ever saw it depended on how the lead chose to summarise.
// Measured on a real integrator failure, the lead summarised the whole hand-over
// to the single word "Reported." The design's escalation order (the model, then
// the human) depends on the human being told something when the model is out of
// attempts.
func TestConflictHandoverReachesTheOperatorsScreen(t *testing.T) {
	for _, tc := range []struct {
		name        string
		roles       string
		wantWorktre bool
	}{
		{"no integrator, straight to the human", branchRolesNoIntegrator, false},
		{"the integrator's one attempt failed", branchRoles, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if real, err := filepath.EvalSymlinks(root); err == nil {
				root = real
			}
			fs := conflictServer(t, root, func(req fakeRequest) fakeReply {
				// No BOTH: the inherited check refuses this resolution.
				return readThenWrite(req, "f.txt", twentyLines("only the human's line 10"))
			})
			fs.models = []string{"lead-a", "coder-a", "integ-a"}
			h := newRoleHarnessAt(t, root, fs, tc.roles, true)
			skipNo3Way(t, h.root)
			seedFile(t, h, twentyLines("line 10"))
			if err := h.run(t, "go"); err != nil {
				t.Fatal(err)
			}
			screen := strings.Join(h.view.notes, "\n")
			for _, want := range []string{"THE RUN STOPS HERE", "nothing was written", "lca merge lca/coder/", "/branches"} {
				if !strings.Contains(screen, want) {
					t.Fatalf("the operator's screen never says %q:\n%s", want, screen)
				}
			}
			if got := strings.Contains(screen, "worktrees/"); got != tc.wantWorktre {
				t.Fatalf("the screen names a merge worktree = %v, want %v:\n%s", got, tc.wantWorktre, screen)
			}
		})
	}
}

// TestMergeFinishDisambiguatesWithAKeyThatWorks: two merge worktrees resolving
// one branch printed the same `source` twice, so the line meant to disambiguate
// was the command that had just failed, byte for byte, and typing either
// reproduced it. The only key that separates the rows is the merge branch — and
// an exact match has to beat a substring one, or `lca/merge/<sid>-t1` ties with
// `lca/merge/<sid>-t1.2` because the first is a prefix of the second.
func TestMergeFinishDisambiguatesWithAKeyThatWorks(t *testing.T) {
	state := t.TempDir()
	base := filepath.Join(state, "worktrees")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, branch := range []string{"lca/merge/s-t1", "lca/merge/s-t1.2"} {
		dir := filepath.Join(base, fmt.Sprintf("s-merge-p%d-%d", os.Getpid(), i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeOwner(dir, owner{session: "s", role: "merge", branch: branch, source: "lca/coder/s-t1"})
	}
	if got := mergeWorktreesOf(state, ""); len(got) != 2 {
		t.Fatalf("both merges must be listed, got %d", len(got))
	}
	// The source is the SAME for both, so it cannot be the key.
	if got := mergeWorktreesOf(state, "lca/coder/s-t1"); len(got) != 1 {
		t.Logf("the shared source selects %d rows, which is why it must not be what is printed", len(got))
	}
	for _, want := range []string{"lca/merge/s-t1", "lca/merge/s-t1.2"} {
		got := mergeWorktreesOf(state, want)
		if len(got) != 1 {
			t.Fatalf("%q selected %d rows; an exact branch name must select exactly its own", want, len(got))
		}
		if got[0].branch != want {
			t.Fatalf("%q selected %q", want, got[0].branch)
		}
	}
	// And the worktree directory works too, because it is unique by construction.
	all := mergeWorktreesOf(state, "")
	if got := mergeWorktreesOf(state, all[0].dir); len(got) != 1 || got[0].dir != all[0].dir {
		t.Fatalf("naming the directory selected %d rows", len(got))
	}
}
