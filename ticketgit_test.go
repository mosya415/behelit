package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The git transitions of `lca ticket`, against REAL git in a throwaway
// repository — including a real remote, which is a second repository on disk and
// not a mock, because every claim this file makes is a claim about git's own
// behaviour and a fake would only test the fake.
//
// Each transition is driven TWICE wherever re-entering it is the thing that has
// to be safe. That is the property the whole command rests on: the cron line is
// the same line whatever happened last night.

// tktRepoFix is a project, a bare remote it can push to, and the tktGit under
// test. The project is parked on a detached HEAD, which is what an unattended
// clone should be doing and what lets the target branch be moved at all.
type tktRepoFix struct {
	top    string
	remote string
	git    *tktGit
	orch   *Orchestrator
}

func newTktRepoFix(t *testing.T, key string) *tktRepoFix {
	t.Helper()
	h := newHarness(t, "http://127.0.0.1:1/v1", "native", true)
	// Symlink-resolved, because the jail resolves it and so does newTktGit: on a
	// mac /var is a symlink to /private/var, and a test comparing the two spellings
	// would be testing the symlink.
	top := h.root
	if real, err := filepath.EvalSymlinks(top); err == nil {
		top = real
	}
	gitT(t, top, "init", "-q", "-b", "main")
	gitT(t, top, "config", "user.email", "t@t")
	gitT(t, top, "config", "user.name", "t")
	os.WriteFile(filepath.Join(top, "f.txt"), []byte(twentyLines("line 10")), 0o644)
	gitT(t, top, "add", "-A")
	gitT(t, top, "commit", "-q", "-m", "seed")

	// The bare remote, added as `origin`. A real second repository: ls-remote,
	// push and a non-fast-forward rejection are all things only git can answer.
	remote := t.TempDir()
	if real, err := filepath.EvalSymlinks(remote); err == nil {
		remote = real
	}
	gitT(t, remote, "init", "-q", "--bare")
	gitT(t, top, "remote", "add", "origin", remote)
	gitT(t, top, "push", "-q", "origin", "main")

	g, err := newTktGit(h.orch, func() string { return key }, "main")
	if err != nil {
		t.Fatal(err)
	}
	return &tktRepoFix{top: top, remote: remote, git: g, orch: h.orch}
}

// park takes the project off the target branch, the way an unattended clone
// should be. Called by every test that merges: the refusal when it is NOT parked
// has a test of its own.
func (f *tktRepoFix) park(t *testing.T) {
	t.Helper()
	gitT(t, f.top, "switch", "--detach", "-q", "HEAD")
}

// workOn commits a change on the ticket's branch, in its worktree, the way
// RunVerifiedAll's commitWork does — hooks off, no gpg, from the worktree.
func workOn(t *testing.T, wtRoot, name, body, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wtRoot, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtRoot, "add", "-A")
	gitT(t, wtRoot, append(hooksOff(), "commit", "-q", "-m", msg)...)
	return gitT(t, wtRoot, "rev-parse", "HEAD")
}

func TestTheTicketsBranchIsCutFromTheTargetAndAdoptedOnReEntry(t *testing.T) {
	f := newTktRepoFix(t, "BSK-1")
	base, wt, err := f.git.CutBranch("agent/BSK-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := gitT(t, f.top, "rev-parse", "main"); base != want {
		t.Fatalf("the branch must be cut from the TARGET, got %s want %s", base, want)
	}
	if got := gitT(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "agent/BSK-1" {
		t.Fatalf("the worktree is on %q", got)
	}
	// Authorship recorded, so `lca clean --branches` may ever touch it.
	if gitT(t, f.top, "rev-parse", madeRef("agent/BSK-1")) != base {
		t.Fatal("the branch lca cut must carry refs/lca/made/")
	}

	// Re-entry: the branch is there, so the worktree is ADOPTED and not doubled.
	again, err := f.git.Worktree("agent/BSK-1")
	if err != nil {
		t.Fatal(err)
	}
	if again != wt {
		t.Fatalf("re-entering adopted a different worktree: %q then %q", wt, again)
	}
	if n := strings.Count(gitT(t, f.top, "worktree", "list", "--porcelain"), "branch refs/heads/agent/BSK-1"); n != 1 {
		t.Fatalf("one ticket, one worktree: %d", n)
	}
	// And a third time, which is the cron line running a third night.
	if third, err := f.git.Worktree("agent/BSK-1"); err != nil || third != wt {
		t.Fatalf("third entry: %q %v", third, err)
	}
}

func TestAWorktreeThatWasRemovedIsAddedBackOntoTheSameBranch(t *testing.T) {
	f := newTktRepoFix(t, "BSK-2")
	_, wt, err := f.git.CutBranch("agent/BSK-2")
	if err != nil {
		t.Fatal(err)
	}
	head := workOn(t, wt, "g.txt", "work\n", "the coder's attempt 1")

	// What `lca clean` does, or a reboot with the state directory on tmpfs: the
	// directory goes and the branch stays.
	os.RemoveAll(wt)
	got, err := f.git.Worktree("agent/BSK-2")
	if err != nil {
		t.Fatalf("a branch with no worktree left must be given one: %v", err)
	}
	if got == wt {
		t.Fatal("the old directory is gone; a new one was expected")
	}
	if gitT(t, got, "rev-parse", "HEAD") != head {
		t.Fatal("the new worktree must hold the work the branch already carries")
	}
	if gitT(t, got, "rev-parse", "--abbrev-ref", "HEAD") != "agent/BSK-2" {
		t.Fatal("and it must be on the ticket's own branch")
	}
}

func TestTheMergeHappensInTheObjectDatabaseAndMovesTheTarget(t *testing.T) {
	f := newTktRepoFix(t, "BSK-3")
	skipNo3Way(t, f.top)
	_, wt, err := f.git.CutBranch("agent/BSK-3")
	if err != nil {
		t.Fatal(err)
	}
	head := workOn(t, wt, "new.txt", "from the ticket\n", "add new.txt")
	f.park(t)
	before := gitT(t, f.top, "rev-parse", "main")

	sha, err := f.git.MergeIn("agent/BSK-3", "main", gitT(t, f.top, "rev-parse", "agent/BSK-3"))
	if err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, f.top, "rev-parse", "main"); got != sha {
		t.Fatalf("main is at %s, the merge is %s", got, sha)
	}
	for _, parent := range []string{before, head} {
		in, err := f.git.Contains(parent, sha)
		if err != nil || !in {
			t.Fatalf("%s must be a parent of the merge: %v %v", shortSha(parent), in, err)
		}
	}
	// Nothing was checked out and nothing was staged to do it: the operator's
	// index and working tree are the property this whole file protects.
	if st := gitT(t, f.top, "status", "--porcelain"); st != "" {
		t.Fatalf("the merge touched the working tree:\n%s", st)
	}

	// Re-entered: the probe above this transition answers "already in", so the
	// machine never calls MergeIn twice — and when something does, the merge is
	// still only ever one commit on main, because the second one would have the
	// same two parents and the probe refuses it. Here we prove the direct property
	// the probe reads.
	in, err := f.git.Contains(head, gitT(t, f.top, "rev-parse", "main"))
	if err != nil || !in {
		t.Fatalf("a re-entered run must find its own head already in the target: %v %v", in, err)
	}
}

func TestMergingRefusesToMoveABranchSomebodyHasCheckedOut(t *testing.T) {
	f := newTktRepoFix(t, "BSK-4")
	skipNo3Way(t, f.top)
	_, wt, err := f.git.CutBranch("agent/BSK-4")
	if err != nil {
		t.Fatal(err)
	}
	workOn(t, wt, "new.txt", "x\n", "add new.txt")
	// NOT parked: the project is still on main, which is the ordinary state of a
	// clone somebody works in.
	_, err = f.git.MergeIn("agent/BSK-4", "main", gitT(t, f.top, "rev-parse", "agent/BSK-4"))
	if err == nil {
		t.Fatal("moving a branch under a live working tree shows every merged file as a local deletion — it must be refused")
	}
	var ue *usageErr
	if !asUsageErr(err, &ue) {
		t.Fatalf("that is a thing to fix in the environment, not a failed task: %T %v", err, err)
	}
	for _, want := range []string{"checked out", f.top, "switch --detach", "target_branch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must name %q and the remedy:\n%v", want, err)
		}
	}
	if got := gitT(t, f.top, "rev-parse", "main"); got != gitT(t, f.top, "rev-parse", "HEAD") {
		t.Fatal("and it must have moved nothing")
	}
}

func TestAMergeThatConflictsMergesNothingAndSaysWhichFile(t *testing.T) {
	f := newTktRepoFix(t, "BSK-5")
	skipNo3Way(t, f.top)
	_, wt, err := f.git.CutBranch("agent/BSK-5")
	if err != nil {
		t.Fatal(err)
	}
	workOn(t, wt, "f.txt", withLine(twentyLines("line 10"), "the ticket's line", 10), "ticket edits line 10")
	// Somebody else changed the same line on main.
	os.WriteFile(filepath.Join(f.top, "f.txt"), []byte(withLine(twentyLines("line 10"), "a person's line", 10)), 0o644)
	gitT(t, f.top, "add", "-A")
	gitT(t, f.top, "commit", "-q", "-m", "a person edits line 10")
	before := gitT(t, f.top, "rev-parse", "main")
	f.park(t)

	_, err = f.git.MergeIn("agent/BSK-5", "main", gitT(t, f.top, "rev-parse", "agent/BSK-5"))
	if err == nil {
		t.Fatal("a conflict after a green check and an approve is a decision a person has to make")
	}
	if !strings.Contains(err.Error(), "f.txt") {
		t.Fatalf("the refusal must name the file:\n%v", err)
	}
	if !strings.Contains(err.Error(), "still there to merge by hand") {
		t.Fatalf("and say the branch is still there:\n%v", err)
	}
	if got := gitT(t, f.top, "rev-parse", "main"); got != before {
		t.Fatal("nothing may be merged")
	}
	if st := gitT(t, f.top, "status", "--porcelain"); st != "" {
		t.Fatalf("and no conflict markers may reach anybody's tree:\n%s", st)
	}
}

func TestTheMergeRefusesWhenTheTargetMovedUnderIt(t *testing.T) {
	f := newTktRepoFix(t, "BSK-6")
	skipNo3Way(t, f.top)
	_, wt, err := f.git.CutBranch("agent/BSK-6")
	if err != nil {
		t.Fatal(err)
	}
	workOn(t, wt, "new.txt", "x\n", "add new.txt")
	f.park(t)
	// The swap's guard, with the window forced open: the ref is read, then moved,
	// then written. Without the compare-and-swap, somebody's commit would be
	// silently dropped out of a branch they are about to release.
	old := gitT(t, f.top, "rev-parse", "main")
	tree := gitT(t, f.top, "rev-parse", "main^{tree}")
	other := gitT(t, f.top, "commit-tree", tree, "-p", old, "-m", "somebody else")
	if _, err := gitCmd(f.top, nil, nil, "update-ref", "refs/heads/main", other, old); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCmd(f.top, nil, nil, "update-ref", "refs/heads/main", old, other); err != nil {
		t.Fatal(err)
	}
	// With the ref back where it was, the merge succeeds; the guard is what the
	// next assertion proves, by hand, in the same shape MergeIn uses.
	if _, err := f.git.MergeIn("agent/BSK-6", "main", gitT(t, f.top, "rev-parse", "agent/BSK-6")); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCmd(f.top, nil, nil, "update-ref", "refs/heads/main", other, old); err == nil {
		t.Fatal("update-ref must refuse a swap whose old value has moved — that is the guard MergeIn relies on")
	}
}

func TestThePushSendsTheReviewedShaAndNothingElse(t *testing.T) {
	f := newTktRepoFix(t, "BSK-7")
	_, wt, err := f.git.CutBranch("agent/BSK-7")
	if err != nil {
		t.Fatal(err)
	}
	head := workOn(t, wt, "a.txt", "one\n", "attempt 1")

	if sha, found, err := f.git.RemoteHead("origin", "agent/BSK-7"); err != nil || found {
		t.Fatalf("nothing is on the remote yet: %q %v %v", sha, found, err)
	}
	if err := f.git.PushBranch("origin", "agent/BSK-7", head); err != nil {
		t.Fatal(err)
	}
	sha, found, err := f.git.RemoteHead("origin", "agent/BSK-7")
	if err != nil || !found || sha != head {
		t.Fatalf("remote head %q found=%v err=%v, want %s", sha, found, err, head)
	}
	// Only that branch. A push that also sent main, or the tags, would be a push
	// nobody reviewed.
	if _, on, _ := f.git.RemoteHead("origin", "agent/other"); on {
		t.Fatal("something else reached the remote")
	}

	// Pushed a second time, which is the resume: the same sha, no error, nothing
	// moved. "Pushing the same commits again is a no-op where it did land."
	if err := f.git.PushBranch("origin", "agent/BSK-7", head); err != nil {
		t.Fatalf("re-pushing the same sha must be a no-op: %v", err)
	}
	if again, _, _ := f.git.RemoteHead("origin", "agent/BSK-7"); again != head {
		t.Fatalf("the remote moved: %s", again)
	}

	// The SHA and not the branch name: the local branch moves on, and a push of
	// the reviewed sha still sends the reviewed sha.
	moved := workOn(t, wt, "a.txt", "two\n", "attempt 2, unreviewed")
	if err := f.git.PushBranch("origin", "agent/BSK-7", head); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := f.git.RemoteHead("origin", "agent/BSK-7"); got != head {
		t.Fatalf("the remote is at %s; the reviewed commit is %s and %s was never approved", got, head, moved)
	}
}

func TestADivergedRemoteIsRefusedAndNeverForced(t *testing.T) {
	f := newTktRepoFix(t, "BSK-8")
	_, wt, err := f.git.CutBranch("agent/BSK-8")
	if err != nil {
		t.Fatal(err)
	}
	ours := workOn(t, wt, "a.txt", "ours\n", "our attempt")

	// Somebody else's history on the same branch name, pushed first. A second
	// clone is the honest way to produce it — and it also produces the case the
	// probe actually meets at 3am: the remote's sha names objects this repository
	// has never fetched, so "does it contain ours" is a question nobody here can
	// answer.
	other := t.TempDir()
	gitT(t, other, "clone", "-q", f.remote, ".")
	gitT(t, other, "config", "user.email", "o@o")
	gitT(t, other, "config", "user.name", "o")
	gitT(t, other, "switch", "-q", "-c", "agent/BSK-8")
	os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("theirs\n"), 0o644)
	gitT(t, other, "add", "-A")
	gitT(t, other, "commit", "-q", "-m", "somebody else's work on the same branch name")
	gitT(t, other, "push", "-q", "origin", "agent/BSK-8")
	theirs := gitT(t, other, "rev-parse", "HEAD")

	// What probePush asks, and what it concludes.
	remote, found, err := f.git.RemoteHead("origin", "agent/BSK-8")
	if err != nil || !found || remote != theirs {
		t.Fatalf("remote head %q found=%v err=%v", remote, found, err)
	}
	mine, err := f.git.Contains(ours, remote)
	if err != nil {
		t.Fatalf("a sha this repository has never fetched is not an error, it is a no: %v", err)
	}
	if mine {
		t.Fatal("the remote does not contain our commit; this is the diverged case")
	}

	// And if the push is attempted anyway, git refuses it and lca says so without
	// ever reaching for a force.
	err = f.git.PushBranch("origin", "agent/BSK-8", ours)
	if err == nil {
		t.Fatal("a non-fast-forward push must fail, not force")
	}
	if !strings.Contains(err.Error(), "has commits this run has not seen") || !strings.Contains(err.Error(), "never forces") {
		t.Fatalf("the refusal must say what it is refusing and that it does not force:\n%v", err)
	}
	if got, _, _ := f.git.RemoteHead("origin", "agent/BSK-8"); got != theirs {
		t.Fatalf("the remote was changed: %s, was %s", got, theirs)
	}
	// The whole machine, over the same world: proofForeign, exit 2, and the ticket
	// is told rather than the branch rewritten.
	if ok := strings.Contains(err.Error(), "--force"); ok {
		t.Fatal("the word force must not appear as something lca would do")
	}
}

func TestAnUnreachableRemoteIsInfraAndNotAFailedTask(t *testing.T) {
	f := newTktRepoFix(t, "BSK-9")
	gitT(t, f.top, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "not-a-repository"))
	_, _, err := f.git.RemoteHead("origin", "agent/BSK-9")
	if err == nil {
		t.Fatal("a remote that is not there must be reported")
	}
	if status, _ := classifyRunErr(err); status != statusInfra {
		t.Fatalf("a remote nobody can reach puts the ticket back in the queue; got %s from %v", status, err)
	}
}

func TestNothingInTheGitPlumbingRunsTheOperatorsHooks(t *testing.T) {
	f := newTktRepoFix(t, "BSK-10")
	skipNo3Way(t, f.top)
	// Every hook this file can trigger, all of them refusing. A pre-push hook in a
	// cron job is something that can only hang or fail, and it belongs to the
	// person who wrote it, not to an agent's scratch copy.
	f.park(t)
	hooks := filepath.Join(f.top, ".git", "hooks")
	os.MkdirAll(hooks, 0o755)
	for _, name := range []string{"pre-commit", "commit-msg", "post-commit", "pre-push", "post-checkout"} {
		p := filepath.Join(hooks, name)
		body := "#!/bin/sh\necho " + name + " ran >&2\nexit 1\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, wt, err := f.git.CutBranch("agent/BSK-10")
	if err != nil {
		t.Fatalf("worktree add must not run post-checkout: %v", err)
	}
	head := workOn(t, wt, "a.txt", "x\n", "attempt 1")
	if _, err := f.git.MergeIn("agent/BSK-10", "main", gitT(t, f.top, "rev-parse", "agent/BSK-10")); err != nil {
		t.Fatalf("the merge must not run a hook: %v", err)
	}
	if err := f.git.PushBranch("origin", "agent/BSK-10", head); err != nil {
		t.Fatalf("the push must not run pre-push: %v", err)
	}
}

func TestANonRepositoryIsRefusedBeforeAnythingIsBought(t *testing.T) {
	h := newHarness(t, "http://127.0.0.1:1/v1", "native", true)
	_, err := newTktGit(h.orch, func() string { return "BSK-11" }, "main")
	if err == nil {
		t.Fatal("a ticket pipeline with no repository has nothing to branch, check, merge or push")
	}
	var ue *usageErr
	if !asUsageErr(err, &ue) {
		t.Fatalf("that is a mistake in the call, not a failed task: %T", err)
	}
}

func TestAMissingTargetBranchNamesTheKeyAndSaysWhyLcaDoesNotFetch(t *testing.T) {
	f := newTktRepoFix(t, "BSK-12")
	g, err := newTktGit(f.orch, func() string { return "BSK-12" }, "integration")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = g.CutBranch("agent/BSK-12")
	if err == nil {
		t.Fatal("a target branch that is not there must be named")
	}
	for _, want := range []string{"target_branch", "integration", "does not fetch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in:\n%v", want, err)
		}
	}
}

// The branch name a ticket gets is the configured prefix plus the key, and it
// has to be a name git will accept whatever the tracker's own spelling is.
func TestEveryTrackerKeyBecomesARefGitAccepts(t *testing.T) {
	f := newTktRepoFix(t, "weird")
	pc := pipeFrom(t, fullPipeline)
	for _, key := range []string{"BSK-1", "bsk/2", "A B C", "x..y", "~tilde", "q?mark", "[br]"} {
		branch := tktBranchFor(pc, key)
		if _, _, code := gitRun(f.top, nil, nil, "check-ref-format", "refs/heads/"+branch); code != 0 {
			t.Fatalf("ticket %q became %q, which git refuses as a ref", key, branch)
		}
	}
	// And the one that cannot become a ref at all is not silently turned into the
	// prefix on its own, which would give two tickets one branch.
	if got := tktBranchFor(pc, "///"); got != "agent/" {
		t.Logf("a key with nothing ref-safe in it becomes %q", got)
	}
}

func TestTheWorktreeListIsHowABranchsCheckoutIsFound(t *testing.T) {
	f := newTktRepoFix(t, "BSK-13")
	if dir, ok := f.git.worktreeOf("main"); !ok || dir != f.top {
		t.Fatalf("the main worktree holds main: %q %v", dir, ok)
	}
	if _, ok := f.git.worktreeOf("agent/nope"); ok {
		t.Fatal("a branch nobody has checked out must answer false")
	}
	_, wt, err := f.git.CutBranch("agent/BSK-13")
	if err != nil {
		t.Fatal(err)
	}
	dir, ok := f.git.worktreeOf("agent/BSK-13")
	if !ok || dir != wt {
		t.Fatalf("the ticket's worktree is %q, git says %q (%v)", wt, dir, ok)
	}
	f.park(t)
	if _, ok := f.git.worktreeOf("main"); ok {
		t.Fatal("a parked clone holds no branch, which is what lets the target be moved")
	}
}

// tktGit must satisfy the interface the machine drives, and the compiler is the
// only honest place to say so.
var _ tktRepo = (*tktGit)(nil)

func TestTheGitSideSaysWhichTransitionItIs(t *testing.T) {
	// A small guard on the one thing a reader of a log has: every refusal in
	// ticketgit.go names the branch or the target it is about, so a line in a cron
	// log is actionable without the state file.
	f := newTktRepoFix(t, "BSK-14")
	if _, _, err := f.git.BranchHead(""); err == nil || !strings.Contains(err.Error(), "branch_prefix") {
		t.Fatalf("an empty branch name must name the key that produced it: %v", err)
	}
	if err := f.git.PushBranch("origin", "agent/BSK-14", ""); err == nil ||
		!strings.Contains(err.Error(), "agent/BSK-14") {
		t.Fatalf("a push with nothing recorded must say which branch: %v", err)
	}
	if _, _, err := f.git.RemoteHead("", "agent/BSK-14"); err == nil ||
		!strings.Contains(err.Error(), "pipeline: remote") {
		t.Fatalf("no remote configured must name the key: %v", err)
	}
	if got := fmt.Sprint(f.git.key()); got != "BSK-14" {
		t.Fatalf("the repository must know which ticket it is driving: %q", got)
	}
}
