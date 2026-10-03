package main

import (
	"context"
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

// `worktree list --porcelain` names the MAIN worktree with its branch line like
// any other, so what git says holds this branch may be the OPERATOR'S own
// checkout — their repository root, switched onto the agent's branch in the
// morning to look at last night's work, with their uncommitted edits in it.
// Adopting that jails the coder there and `add -A` sweeps their work into this
// ticket's commit, which is the one thing the top of ticketgit.go swears never
// happens. So an adopted worktree has to be one lca made, and the owner file
// beside it is what says so.
func TestAWorktreeLcaDidNotMakeIsNeverAdopted(t *testing.T) {
	f := newTktRepoFix(t, "BSK-15")
	_, wt, err := f.git.CutBranch("agent/BSK-15")
	if err != nil {
		t.Fatal(err)
	}
	// Night one's worktree is removed, which is the case `lca clean` exists for.
	os.RemoveAll(wt)
	gitT(t, f.top, "worktree", "prune")

	// In the morning a developer switches their own checkout onto the branch to
	// read the agent's work, and leaves an edit in it.
	gitT(t, f.top, "switch", "-q", "agent/BSK-15")
	mine := filepath.Join(f.top, "f.txt")
	if err := os.WriteFile(mine, []byte("what a person was in the middle of\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := f.git.Worktree("agent/BSK-15")
	if err == nil {
		t.Fatalf("lca will not work in a tree it does not own, and it was handed %q", got)
	}
	var ue *usageErr
	if !asUsageErr(err, &ue) {
		t.Fatalf("that is a thing to fix in the environment, not a failed task: %T %v", err, err)
	}
	for _, want := range []string{f.top, "agent/BSK-15", "not a worktree lca made", "switch --detach"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must name %q and the remedy, as MergeIn's does:\n%v", want, err)
		}
	}
	// And the person's work is exactly where they left it.
	if b, rerr := os.ReadFile(mine); rerr != nil || !strings.Contains(string(b), "in the middle of") {
		t.Fatalf("nothing here touches a tree lca does not own: %q %v", b, rerr)
	}
	if out := gitT(t, f.top, "status", "--porcelain"); !strings.Contains(out, "f.txt") {
		t.Fatalf("their edit must still be uncommitted: %q", out)
	}

	// A worktree lca DID make is adopted, so the check is ownership and not fear
	// of adoption.
	gitT(t, f.top, "checkout", "-q", "--", "f.txt")
	f.park(t)
	again, err := f.git.Worktree("agent/BSK-15")
	if err != nil {
		t.Fatalf("a branch with no worktree of ours left must be given one: %v", err)
	}
	if third, err := f.git.Worktree("agent/BSK-15"); err != nil || third != again {
		t.Fatalf("and that one is adopted on the next night: %q %v", third, err)
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

	if sha, found, err := f.git.RemoteHead(context.Background(), "origin", "agent/BSK-7"); err != nil || found {
		t.Fatalf("nothing is on the remote yet: %q %v %v", sha, found, err)
	}
	if err := f.git.PushBranch(context.Background(), "origin", "agent/BSK-7", head); err != nil {
		t.Fatal(err)
	}
	sha, found, err := f.git.RemoteHead(context.Background(), "origin", "agent/BSK-7")
	if err != nil || !found || sha != head {
		t.Fatalf("remote head %q found=%v err=%v, want %s", sha, found, err, head)
	}
	// Only that branch. A push that also sent main, or the tags, would be a push
	// nobody reviewed.
	if _, on, _ := f.git.RemoteHead(context.Background(), "origin", "agent/other"); on {
		t.Fatal("something else reached the remote")
	}

	// Pushed a second time, which is the resume: the same sha, no error, nothing
	// moved. "Pushing the same commits again is a no-op where it did land."
	if err := f.git.PushBranch(context.Background(), "origin", "agent/BSK-7", head); err != nil {
		t.Fatalf("re-pushing the same sha must be a no-op: %v", err)
	}
	if again, _, _ := f.git.RemoteHead(context.Background(), "origin", "agent/BSK-7"); again != head {
		t.Fatalf("the remote moved: %s", again)
	}

	// The SHA and not the branch name: the local branch moves on, and a push of
	// the reviewed sha still sends the reviewed sha.
	moved := workOn(t, wt, "a.txt", "two\n", "attempt 2, unreviewed")
	if err := f.git.PushBranch(context.Background(), "origin", "agent/BSK-7", head); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := f.git.RemoteHead(context.Background(), "origin", "agent/BSK-7"); got != head {
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
	remote, found, err := f.git.RemoteHead(context.Background(), "origin", "agent/BSK-8")
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
	err = f.git.PushBranch(context.Background(), "origin", "agent/BSK-8", ours)
	if err == nil {
		t.Fatal("a non-fast-forward push must fail, not force")
	}
	if !strings.Contains(err.Error(), "has commits this run has not seen") || !strings.Contains(err.Error(), "never forces") {
		t.Fatalf("the refusal must say what it is refusing and that it does not force:\n%v", err)
	}
	if got, _, _ := f.git.RemoteHead(context.Background(), "origin", "agent/BSK-8"); got != theirs {
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
	_, _, err := f.git.RemoteHead(context.Background(), "origin", "agent/BSK-9")
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
	if err := f.git.PushBranch(context.Background(), "origin", "agent/BSK-10", head); err != nil {
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
	if err := f.git.PushBranch(context.Background(), "origin", "agent/BSK-14", ""); err == nil ||
		!strings.Contains(err.Error(), "agent/BSK-14") {
		t.Fatalf("a push with nothing recorded must say which branch: %v", err)
	}
	if _, _, err := f.git.RemoteHead(context.Background(), "", "agent/BSK-14"); err == nil ||
		!strings.Contains(err.Error(), "pipeline: remote") {
		t.Fatalf("no remote configured must name the key: %v", err)
	}
	if got := fmt.Sprint(f.git.key()); got != "BSK-14" {
		t.Fatalf("the repository must know which ticket it is driving: %q", got)
	}
}

// ── the one supervised attempt at a conflict ─────────────────────────────────
//
// Against real git, and the whole machine over it: the repository is the real
// tktGit, the tracker and the forge are fakes, and the integrator is a function
// that does to the worktree what a model would do to it. A fake repository could
// not carry any of these claims — every one of them is a claim about what git
// leaves in a tree mid-merge, which commit has which parents, and which paths a
// three-way diff names.

// resolveOnPipeline is the team that turned the attempt on. Written as YAML and
// not as a field poke, because the key an operator types is half the feature.
const resolveOnPipeline = fullPipeline + "  resolve_conflicts: true\n"

// gitStages is the model side of a git-backed run: the fake world's five stages,
// with the integrator's replaced by something that really edits the worktree.
type gitStages struct {
	*fakeWorld
	resolve func(t *testing.T, wt string, in tktStageIn) (tktCodeOut, error)
}

func (g gitStages) Resolve(_ context.Context, in tktStageIn) (tktCodeOut, error) {
	g.hit("stage:" + in.Stage)
	if in.Conflict.Files == "" {
		g.t.Fatal("the resolve stage must be told which files collided")
	}
	if in.Worktree == "" {
		g.t.Fatal("the resolve stage must be given the ticket's own worktree")
	}
	return g.resolve(g.t, in.Worktree, in)
}

// tktConflictFix is a ticket whose work is finished, checked and approved on its
// own branch, standing in front of a target branch that has moved under it — the
// commonest real conflict there is, and the only one worth one attempt.
//
// The ticket changes two things: the line that collides, and a file nobody else
// touched. The second one is what makes "it dropped the work" a question with an
// answer: a resolution that keeps only the uncontested half still passes a check.
type tktConflictFix struct {
	*tktRepoFix
	wt       string // the ticket's worktree
	head     string // the approved commit on the branch
	branch   string
	mainTip  string
	ticketLn string
	personLn string
}

func newTktConflictFix(t *testing.T, key string) *tktConflictFix {
	t.Helper()
	f := newTktRepoFix(t, key)
	skipNo3Way(t, f.top)
	branch := "agent/" + key
	_, wt, err := f.git.CutBranch(branch)
	if err != nil {
		t.Fatal(err)
	}
	c := &tktConflictFix{tktRepoFix: f, wt: wt, branch: branch,
		ticketLn: "the ticket's line", personLn: "a person's line"}
	// The ticket's work: one collision and one file of its own.
	os.WriteFile(filepath.Join(wt, "f.txt"), []byte(withLine(twentyLines("line 10"), c.ticketLn, 10)), 0o644)
	c.head = workOn(t, wt, "new.txt", "only this ticket wrote this\n", "the coder's attempt 1")

	// And the target branch moves under it, in the same region.
	os.WriteFile(filepath.Join(f.top, "f.txt"), []byte(withLine(twentyLines("line 10"), c.personLn, 10)), 0o644)
	gitT(t, f.top, "add", "-A")
	gitT(t, f.top, append(hooksOff(), "commit", "-q", "-m", "somebody else edits line 10")...)
	c.mainTip = gitT(t, f.top, "rev-parse", "main")
	f.park(t)
	return c
}

// run builds the machine over this repository, standing where a conflict is
// first discovered: implemented, checked, reviewed and approved.
func (c *tktConflictFix) run(t *testing.T, yaml string, resolve func(*testing.T, string, tktStageIn) (tktCodeOut, error)) (*tktRun, *fakeWorld) {
	t.Helper()
	w := newFakeWorld(t)
	w.ticket = TicketBody{Key: c.git.key(), Summary: "the stand drops requests over 8k"}
	pc := pipeFrom(t, yaml)
	st := &TicketState{Ticket: c.git.key(), State: tktReviewed, Summary: "x",
		Branch: c.branch, BranchAt: gitT(t, c.top, "rev-parse", madeRef(c.branch)),
		Worktree: c.wt, Head: c.head, Verdict: tktApprove,
		Review: &reviewReport{Verdict: tktApprove, Comments: []reviewComment{}},
		Check:  TicketCheck{Cmd: "go test", Exit: &zeroExit, Attempts: 1}}
	r := newRun(t, w, pc, st)
	r.w.Repo = c.git
	r.w.Models = gitStages{fakeWorld: w, resolve: resolve}
	return r, w
}

// commitResolution is what the engine's own commitWork does to a resolved merge:
// hooks off, no signing, `add -A` and a commit, which with MERGE_HEAD present
// makes the two-parent merge commit ResolvedAt insists on.
func commitResolution(t *testing.T, wt, msg string) string {
	t.Helper()
	gitT(t, wt, "add", "-A")
	gitT(t, wt, append(hooksOff(), "commit", "-q", "-m", msg)...)
	return gitT(t, wt, "rev-parse", "HEAD")
}

// midMerge is "this worktree is in the middle of a merge", asked without gitT,
// which fatals on a non-zero exit — and a missing MERGE_HEAD is an ANSWER here,
// not a failure.
func midMerge(dir string) bool {
	_, _, code := gitRun(dir, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD")
	return code == 0
}

func greenCheck() TicketCheck {
	zero := 0
	return TicketCheck{Cmd: "go test", Exit: &zero, Attempts: 1}
}

func redCheck(tail string) TicketCheck {
	one := 1
	return TicketCheck{Cmd: "go test", Exit: &one, Attempts: 1, Tail: tail}
}

//  1. The integrator resolves it. The attempt happens on the TICKET BRANCH, the
//     check runs again over the result, the REVIEWER reads the resolution, and
//     what reaches the target afterwards is a merge of a branch whose check is
//     green — the same gate as before and not a weaker one.
func TestAConflictTheIntegratorResolvesIsReviewedAgainAndThenMerged(t *testing.T) {
	c := newTktConflictFix(t, "BSK-20")
	r, w := c.run(t, resolveOnPipeline, func(t *testing.T, wt string, in tktStageIn) (tktCodeOut, error) {
		// Both intentions survive, which is what the prompt asks for.
		body := withLine(twentyLines("line 10"), c.ticketLn+" / "+c.personLn, 10)
		if b, err := os.ReadFile(filepath.Join(wt, "f.txt")); err != nil || !strings.Contains(string(b), "<<<<<<<") {
			t.Fatalf("the integrator must be handed a tree with the markers in it: %q %v", b, err)
		}
		if !midMerge(wt) {
			t.Fatal("and an unfinished merge it can abort")
		}
		os.WriteFile(filepath.Join(wt, "f.txt"), []byte(body), 0o644)
		return tktCodeOut{Session: "int-1", Head: commitResolution(t, wt, "integrator: resolve the merge"), Check: greenCheck()}, nil
	})

	status, reason := r.execute()
	if status != statusPassed {
		t.Fatalf("status %q (%s)\njournal: %+v", status, reason, r.st.Journal)
	}
	res := r.st.Resolve
	if res == nil || res.Outcome != tktResolveDone {
		t.Fatalf("the attempt must be recorded as resolved: %+v", res)
	}
	if !strings.Contains(res.Files, "f.txt") {
		t.Fatalf("the conflict must be recorded by the paths git named: %+v", res)
	}
	// The resolution is a change nobody reviewed, so the reviewer read it — and
	// the order is the claim: the run arrived with a verdict already recorded
	// (`review already`, nothing bought), the resolution cleared it, and a review
	// was bought AFTER the resolve. That is walk's second backward edge.
	if n := w.calls["stage:reviewer"]; n != 1 {
		t.Fatalf("the resolved diff must go back to the reviewer, exactly once: %d review stages", n)
	}
	if order := transitionOrder(r.st, tktOK, tktResolve, tktReview); order != "resolve,review" {
		t.Fatalf("the reviewer has to read the resolution, which means it runs after it: %q\n%+v", order, r.st.Journal)
	}
	if r.st.Round != 0 {
		t.Fatalf("a resolution is not a rework round: round %d", r.st.Round)
	}
	if r.st.Head != res.Head {
		t.Fatalf("the merge must be about the resolution (%s), not the commit under it (%s)", res.Head, r.st.Head)
	}
	// The target branch moved exactly once, to a merge of the resolution, and it
	// holds both intentions.
	if !strings.Contains(stepNames(r.st, tktResolve), tktOK) {
		t.Fatalf("the journal must carry the resolution as its own transition: %+v", stepsNamed(r.st, tktResolve))
	}
	mergedF := gitT(t, c.top, "show", "main:f.txt")
	for _, want := range []string{c.ticketLn, c.personLn} {
		if !strings.Contains(mergedF, want) {
			t.Fatalf("%q did not survive to the target branch:\n%s", want, mergedF)
		}
	}
	if out := gitT(t, c.top, "show", "main:new.txt"); !strings.Contains(out, "only this ticket wrote this") {
		t.Fatalf("the ticket's uncontested work must land too: %q", out)
	}
	// And the ticket is told, in lca's own words: the facts the report stage is
	// handed are the facts it is told to invent nothing beyond, and they are what
	// the fallback comment is written from when no model can be bought.
	facts := tktOutcomeText(r.st)
	for _, want := range []string{"merge conflict", "f.txt", "resolved it as", "a reviewer read the resolution"} {
		if !strings.Contains(facts, want) {
			t.Fatalf("the ticket has to hear %q:\n%s", want, facts)
		}
	}
	if len(w.comments) == 0 {
		t.Fatal("and a finished run comments")
	}
}

//  2. It does not resolve it. commitWork refuses to put marker text on a branch,
//     so the stage comes back with nothing committed — and that is a conflict for
//     a person, with the files named, the branch back where it was and the
//     worktree no longer mid-merge.
func TestAConflictTheIntegratorDoesNotResolveGoesToAPerson(t *testing.T) {
	c := newTktConflictFix(t, "BSK-21")
	r, w := c.run(t, resolveOnPipeline, func(t *testing.T, wt string, in tktStageIn) (tktCodeOut, error) {
		// It stopped. The engine's own commitWork is what refuses the markers; the
		// stage reports that refusal and leaves no commit behind.
		return tktCodeOut{Session: "int-2"}, fmt.Errorf("the merge in %s is not resolved — f.txt still hold conflict markers", wt)
	})

	status, reason := r.execute()
	if status == statusPassed {
		t.Fatalf("an unresolved conflict is not a passed run: %s", reason)
	}
	if r.st.Resolve == nil || r.st.Resolve.Outcome != tktResolveNone {
		t.Fatalf("the attempt must be recorded as unresolved: %+v", r.st.Resolve)
	}
	for _, want := range []string{"f.txt", "merge by hand", c.branch} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the refusal must name %q:\n%s", want, reason)
		}
	}
	if got := gitT(t, c.top, "rev-parse", "main"); got != c.mainTip {
		t.Fatalf("nothing may be merged: main is at %s, was %s", got, c.mainTip)
	}
	if got := gitT(t, c.top, "rev-parse", c.branch); got != c.head {
		t.Fatalf("the branch must be left at the approved commit: %s, want %s", got, c.head)
	}
	if st := gitT(t, c.wt, "status", "--porcelain"); strings.Contains(st, "UU") {
		t.Fatalf("the worktree must not be left mid-merge for the next night to trip over:\n%s", st)
	}
	if b, _ := os.ReadFile(filepath.Join(c.wt, "f.txt")); strings.Contains(string(b), "<<<<<<<") {
		t.Fatalf("and no marker text may be left on the branch:\n%s", b)
	}
	// Never twice. The next night finds the attempt spent and buys nobody.
	again, w2 := c.run(t, resolveOnPipeline, func(*testing.T, string, tktStageIn) (tktCodeOut, error) {
		t.Fatal("the integrator was given a second attempt at one conflict")
		return tktCodeOut{}, nil
	})
	again.st.Resolve = r.st.Resolve
	status2, reason2 := again.execute()
	if status2 == statusPassed {
		t.Fatalf("the second night must not pass either: %s", reason2)
	}
	if w2.calls["stage:resolve"] != 0 {
		t.Fatal("one attempt, ever")
	}
	if !strings.Contains(reason2, "one supervised attempt is spent") {
		t.Fatalf("and it must say why it is not trying again:\n%s", reason2)
	}
	_ = w
}

//  3. It resolves it into a red check. A resolution that does not pass the check
//     is not a resolution, and the attempt is kept where a person can read it.
func TestAResolutionWithARedCheckGoesToAPersonAndIsKept(t *testing.T) {
	c := newTktConflictFix(t, "BSK-22")
	r, _ := c.run(t, resolveOnPipeline, func(t *testing.T, wt string, in tktStageIn) (tktCodeOut, error) {
		os.WriteFile(filepath.Join(wt, "f.txt"), []byte(withLine(twentyLines("line 10"), "something that does not build", 10)), 0o644)
		head := commitResolution(t, wt, "integrator: a resolution that does not pass")
		return tktCodeOut{Session: "int-3", Head: head, Check: redCheck("f.txt:10: undefined: nope")}, nil
	})

	status, reason := r.execute()
	if status == statusPassed {
		t.Fatalf("a red check over a resolution is not a pass: %s", reason)
	}
	if r.st.Resolve == nil || r.st.Resolve.Outcome != tktResolveRed {
		t.Fatalf("the attempt must be recorded as a failed check: %+v", r.st.Resolve)
	}
	if got := gitT(t, c.top, "rev-parse", "main"); got != c.mainTip {
		t.Fatal("nothing may reach the target branch")
	}
	if got := gitT(t, c.top, "rev-parse", c.branch); got != c.head {
		t.Fatalf("the branch must be put back at the approved commit: %s", got)
	}
	// The rejected attempt is not thrown away: the refusal names where it is, and
	// it is there.
	ref := r.st.Resolve.Kept
	if ref == "" || !strings.Contains(reason, ref) {
		t.Fatalf("a rejected resolution a person cannot read is a resolution thrown away: %q\n%s", ref, reason)
	}
	if gitT(t, c.top, "rev-parse", "--verify", "-q", ref) == "" {
		t.Fatalf("%s must exist", ref)
	}
	if out := gitT(t, c.top, "show", ref+":f.txt"); !strings.Contains(out, "does not build") {
		t.Fatalf("and hold what the integrator actually wrote:\n%s", out)
	}
	// Outside refs/heads, so `git branch` never lists it.
	if strings.Contains(gitT(t, c.top, "branch", "--list"), "resolve") {
		t.Fatal("the kept attempt must not look like a branch")
	}
	// The rejected resolution's red check must NOT have been written over the
	// implementation's green one. The resolution is thrown away; recording its
	// exit code as this ticket's check would make every later night report
	// "`go test` still failed" about a commit whose check passed, and block the
	// review on a change nobody had changed.
	if !r.st.checkGreen() {
		t.Fatalf("the implementation's own check must survive a rejected resolution: %+v", r.st.Check)
	}
	if !strings.Contains(reason, "undefined: nope") {
		t.Fatalf("and the refusal must still carry the check's tail, which is what says why it is red:\n%s", reason)
	}
}

//  4. It "resolves" it by throwing the ticket's own change away — which passes a
//     check as easily as a correct resolution does, and is caught by comparing
//     what the merge lands on the target against the diff that was approved.
func TestAResolutionThatDropsTheTicketsWorkIsRefused(t *testing.T) {
	c := newTktConflictFix(t, "BSK-23")
	r, _ := c.run(t, resolveOnPipeline, func(t *testing.T, wt string, in tktStageIn) (tktCodeOut, error) {
		// `git checkout --theirs` in one line: take the target's side whole. The
		// markers are gone, the tree builds, and the ticket's change to f.txt is not
		// in it any more.
		os.WriteFile(filepath.Join(wt, "f.txt"), []byte(withLine(twentyLines("line 10"), c.personLn, 10)), 0o644)
		head := commitResolution(t, wt, "integrator: take the target's side")
		return tktCodeOut{Session: "int-4", Head: head, Check: greenCheck()}, nil
	})

	status, reason := r.execute()
	if status == statusPassed {
		t.Fatalf("a resolution that drops the reviewed change must not merge: %s", reason)
	}
	res := r.st.Resolve
	if res == nil || res.Outcome != tktResolveDropped {
		t.Fatalf("the attempt must be recorded as having dropped the work: %+v", res)
	}
	if res.Lost != "f.txt" {
		t.Fatalf("and it must name the reviewed path that did not survive: %q", res.Lost)
	}
	if !strings.Contains(reason, "f.txt") || !strings.Contains(reason, "dropping") {
		t.Fatalf("the refusal has to say what was dropped:\n%s", reason)
	}
	if got := gitT(t, c.top, "rev-parse", "main"); got != c.mainTip {
		t.Fatal("nothing may reach the target branch")
	}
	if got := gitT(t, c.top, "rev-parse", c.branch); got != c.head {
		t.Fatalf("the branch must be put back at the approved commit: %s", got)
	}
}

//  5. A run killed inside the attempt. Two shapes, because the record is written
//     in two steps and a SIGKILL can land between them — and neither of them may
//     try a second time.
func TestARunKilledInsideTheAttemptNeverTriesASecondTime(t *testing.T) {
	// Killed before the attempt was even recorded: `resolve` is pending and
	// nothing says what it left. The probe still finds the conflict, so the
	// transition would ordinarily run — and Redo is false precisely so it does
	// not.
	c := newTktConflictFix(t, "BSK-24")
	r, w := c.run(t, resolveOnPipeline, func(*testing.T, string, tktStageIn) (tktCodeOut, error) {
		t.Fatal("a run killed inside the attempt must not make another one")
		return tktCodeOut{}, nil
	})
	r.st.Pending = tktResolve
	status, reason := r.execute()
	if status == statusPassed {
		t.Fatalf("that is not a pass: %s", reason)
	}
	if w.calls["stage:resolve"] != 0 {
		t.Fatal("one attempt, ever")
	}
	for _, want := range []string{"killed inside", "by hand", c.branch} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the refusal must say what it is afraid of and name the branch (%q):\n%s", want, reason)
		}
	}
	if got := gitT(t, c.top, "rev-parse", "main"); got != c.mainTip {
		t.Fatal("nothing may be merged")
	}

	// Killed after the attempt was recorded and before anything judged what it
	// left. The gate is what refuses now, and it says the same thing.
	d := newTktConflictFix(t, "BSK-25")
	r2, w2 := d.run(t, resolveOnPipeline, func(*testing.T, string, tktStageIn) (tktCodeOut, error) {
		t.Fatal("a recorded attempt is a spent attempt")
		return tktCodeOut{}, nil
	})
	r2.st.Resolve = &TicketResolve{Round: 0, Base: d.head, Target: d.mainTip, Files: "f.txt"}
	status2, reason2 := r2.execute()
	if status2 == statusPassed {
		t.Fatalf("that is not a pass either: %s", reason2)
	}
	if w2.calls["stage:resolve"] != 0 {
		t.Fatal("one attempt, ever")
	}
	if !strings.Contains(reason2, "nothing judged what it left") {
		t.Fatalf("an attempt nobody judged must be named as one:\n%s", reason2)
	}
	if got := gitT(t, d.top, "rev-parse", "main"); got != d.mainTip {
		t.Fatal("nothing may be merged")
	}
}

//  6. The configuration off, which is the default and today's behaviour: the
//     conflict goes straight to a person, in MergeIn's own words, and no
//     integrator is bought.
func TestWithTheKeyOffAConflictIsTodaysRefusalAndNothingElse(t *testing.T) {
	c := newTktConflictFix(t, "BSK-26")
	r, w := c.run(t, fullPipeline, func(*testing.T, string, tktStageIn) (tktCodeOut, error) {
		t.Fatal("nothing may resolve a conflict unless the operator turned it on by name")
		return tktCodeOut{}, nil
	})
	if r.pc.resolveConflicts() {
		t.Fatal("resolve_conflicts must default to off: resolving a conflict is a decision, and a default that decides is a default nobody chose")
	}

	status, reason := r.execute()
	if status == statusPassed {
		t.Fatalf("a conflict is not a pass: %s", reason)
	}
	if w.calls["stage:resolve"] != 0 || r.st.Resolve != nil {
		t.Fatalf("nothing was turned on, so nothing was attempted: %d %+v", w.calls["stage:resolve"], r.st.Resolve)
	}
	// The sentence MergeIn has always used, and the transition it has always come
	// from. The one new thing in the journal is a skipped row saying why.
	if !strings.Contains(reason, "without a decision somebody has to make") {
		t.Fatalf("the words a person reads must not have changed:\n%s", reason)
	}
	if steps := stepsNamed(r.st, tktResolve); len(steps) != 1 || steps[0].Status != tktSkipped ||
		!strings.Contains(steps[0].Detail, "resolve_conflicts is false") {
		t.Fatalf("the journal must say the key is off: %+v", steps)
	}
	if got := gitT(t, c.top, "rev-parse", "main"); got != c.mainTip {
		t.Fatal("nothing may be merged")
	}
	if got := gitT(t, c.top, "rev-parse", c.branch); got != c.head {
		t.Fatalf("and the branch is untouched: %s", got)
	}
}

// The gate may not become weaker: a resolution the re-review sends back does not
// merge, however green its check is.
func TestAResolutionTheReviewerRejectsDoesNotMerge(t *testing.T) {
	c := newTktConflictFix(t, "BSK-27")
	r, w := c.run(t, resolveOnPipeline, func(t *testing.T, wt string, in tktStageIn) (tktCodeOut, error) {
		os.WriteFile(filepath.Join(wt, "f.txt"), []byte(withLine(twentyLines("line 10"), c.ticketLn+" / "+c.personLn, 10)), 0o644)
		return tktCodeOut{Session: "int-5", Head: commitResolution(t, wt, "integrator: resolve the merge"), Check: greenCheck()}, nil
	})
	// The first verdict was the approve this run started from; every reviewer
	// after it asks for changes, so the resolution is read, sent back, reworked
	// once, read again and never approved.
	w.verdicts = []string{tktChanges}
	r.st.Coder = "coder-sess"

	status, reason := r.execute()
	if status == statusPassed {
		t.Fatalf("a resolution nobody approved must not merge: %s", reason)
	}
	if r.st.Resolve == nil || r.st.Resolve.Outcome != tktResolveDone {
		t.Fatalf("the resolution itself worked; it is the review that stopped it: %+v", r.st.Resolve)
	}
	if got := gitT(t, c.top, "rev-parse", "main"); got != c.mainTip {
		t.Fatalf("the target branch moved on a change the reviewer rejected: %s", got)
	}
	if !strings.Contains(reason, "asked for changes") {
		t.Fatalf("and the gate that stopped it must be the review:\n%s", reason)
	}
}

// A verdict that could not be READ over a resolution is not an approval — and
// the run has to STOP there rather than ask again, which is the one shape the
// backward edge to the reviewer could have got wrong: the resolution is made
// once, the reviewer is asked once, and nothing merges.
func TestAnUnreadableVerdictOverAResolutionStopsRatherThanLooping(t *testing.T) {
	c := newTktConflictFix(t, "BSK-31")
	r, w := c.run(t, resolveOnPipeline, func(t *testing.T, wt string, in tktStageIn) (tktCodeOut, error) {
		os.WriteFile(filepath.Join(wt, "f.txt"), []byte(withLine(twentyLines("line 10"), c.ticketLn+" / "+c.personLn, 10)), 0o644)
		return tktCodeOut{Session: "int-6", Head: commitResolution(t, wt, "integrator: resolve the merge"), Check: greenCheck()}, nil
	})
	w.verdicts = []string{"unreadable"} // the fake answers with no report at all

	status, reason := r.execute()
	if status == statusPassed {
		t.Fatalf("an unreadable verdict is not an approval: %s", reason)
	}
	if n := w.calls["stage:reviewer"]; n != 1 {
		t.Fatalf("the reviewer is asked once about the resolution, not repeatedly: %d", n)
	}
	if w.calls["stage:resolve"] != 1 {
		t.Fatalf("and the resolution is made once: %d", w.calls["stage:resolve"])
	}
	if got := gitT(t, c.top, "rev-parse", "main"); got != c.mainTip {
		t.Fatalf("nothing may merge without a verdict: %s", got)
	}
}

// MergeTargetIn's two refusals before it starts, both of them a merge nobody
// could have judged afterwards.
func TestTheAttemptRefusesADirtyWorktreeAndTheWrongBranch(t *testing.T) {
	c := newTktConflictFix(t, "BSK-28")
	os.WriteFile(filepath.Join(c.wt, "scratch.txt"), []byte("something uncommitted\n"), 0o644)
	_, _, err := c.git.MergeTargetIn(c.branch, "main", c.wt)
	if err == nil {
		t.Fatal("a merge on top of work no check ran over is a merge nobody can judge")
	}
	if !strings.Contains(err.Error(), "uncommitted work") {
		t.Fatalf("say which: %v", err)
	}
	if midMerge(c.wt) {
		t.Fatal("and it must not have started one")
	}
	os.Remove(filepath.Join(c.wt, "scratch.txt"))

	// A worktree holding something else is a path the state file is wrong about.
	gitT(t, c.wt, "switch", "--detach", "-q", "HEAD")
	if _, _, err := c.git.MergeTargetIn(c.branch, "main", c.wt); err == nil ||
		!strings.Contains(err.Error(), "checked out") {
		t.Fatalf("a worktree that is not on the ticket's branch must be refused: %v", err)
	}
}

// MergeConflicts is MergeIn's question with none of its consequences, and the
// two must agree — the probe buys an integrator on the strength of it.
func TestAskingWhetherABranchMergesChangesNothing(t *testing.T) {
	c := newTktConflictFix(t, "BSK-29")
	files, conflicted, err := c.git.MergeConflicts(c.branch, "main")
	if err != nil || !conflicted || !strings.Contains(files, "f.txt") {
		t.Fatalf("the question must answer the conflict and name the file: %q %v %v", files, conflicted, err)
	}
	if got := gitT(t, c.top, "rev-parse", "main"); got != c.mainTip {
		t.Fatal("asking must move nothing")
	}
	if st := gitT(t, c.top, "status", "--porcelain"); st != "" {
		t.Fatalf("and touch no working tree:\n%s", st)
	}
	// MergeIn, asked for real, says the same thing.
	if _, err := c.git.MergeIn(c.branch, "main", c.head); err == nil || !strings.Contains(err.Error(), "f.txt") {
		t.Fatalf("the cheap question and the real one must agree: %v", err)
	}

	// A branch that does merge answers no, and that is the ninety-nine nights in a
	// hundred where nothing collided.
	d := newTktRepoFix(t, "BSK-30")
	skipNo3Way(t, d.top)
	_, wt, err := d.git.CutBranch("agent/BSK-30")
	if err != nil {
		t.Fatal(err)
	}
	workOn(t, wt, "elsewhere.txt", "no collision here\n", "the coder's attempt 1")
	if files, conflicted, err := d.git.MergeConflicts("agent/BSK-30", "main"); err != nil || conflicted || files != "" {
		t.Fatalf("a branch that merges must answer no: %q %v %v", files, conflicted, err)
	}
}

// transitionOrder is the order two transitions reached a status in, so a test can
// say "the reviewer ran AFTER the resolution" rather than only that both ran.
func transitionOrder(st *TicketState, status string, names ...string) string {
	var out []string
	for _, s := range st.Journal {
		if s.Status == status && contains(names, s.Name) {
			out = append(out, s.Name)
		}
	}
	return strings.Join(out, ",")
}

// stepNames is the statuses one transition was recorded with, joined, for the
// assertions that only care that it happened.
func stepNames(st *TicketState, name string) string {
	var out []string
	for _, s := range stepsNamed(st, name) {
		out = append(out, s.Status)
	}
	return strings.Join(out, ",")
}
