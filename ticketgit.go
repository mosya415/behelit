package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The git half of `lca ticket`: the branch, the worktree, the merge and the push,
// every one of them performed by lca and none of them reachable by a model.
//
// Everything here is branch.go's own machinery pointed at a ticket instead of at
// a delegation — gitRun and gitCmd for the commands, lockGit so two lca
// processes cannot interleave a probe and a create, worktreeDir and writeOwner so
// `lca clean` can tell whose worktree it is, madeRef so nothing deletes a branch
// lca did not make, and mergeTree3Way for the merge itself. The never-touched
// list from the top of branch.go holds here word for word:
//
//   - the operator's index and HEAD: nothing in this file stages, commits,
//     stashes, resets or checks anything out in their tree. The merge happens in
//     the object database and the only ref it writes is the target branch's, and
//     only when no working tree is holding it;
//   - the operator's hooks: -c core.hooksPath=/dev/null on `worktree add`, on
//     every commit in the ticket's worktree (RunVerifiedAll's commitWork does it
//     too), on the merge and on the push. A pre-push hook that opens an editor is
//     a cron job that never returns;
//   - refs/heads, except the branch named from the configured prefix, which lca
//     cut itself and recorded cutting.
//
// The one thing that is NOT branch.go's is where the branch is cut from. A
// delegation branches from the caller's dirty tree, because a subagent that
// cannot see the caller's uncommitted work is working on a different program. A
// ticket branches from the TARGET BRANCH, because what it is going to produce is
// a merge request against that branch, and a branch cut from whatever happened to
// be in somebody's tree at 3am produces a diff full of their work.

// tktGit is tktRepo over one repository. It holds the target branch because the
// interface does not pass it to CutBranch — and that is the right way round: the
// branch a ticket is cut from is configuration, not an argument a caller could
// vary per call.
type tktGit struct {
	orch *Orchestrator
	top  string // the repository's top level
	sub  string // where the project sits inside it ("." = the top)
	// keyOf is the ticket, asked for rather than held: a `-new` run has no key
	// until the tracker gives it one, and the branch, the commit message and the
	// worktree's owner file all want the real one.
	keyOf func() string
	targ  string // pipeline: target_branch
	dir   string // LCA_DIR, for worktreeDir and the owner file
}

// key is the ticket this repository is being driven for, and "-" before the
// tracker has named it — which only reaches a worktree name, never a branch:
// nothing cuts a branch before `opened`.
func (g *tktGit) key() string {
	if g.keyOf == nil {
		return "-"
	}
	return firstNonEmpty(strings.TrimSpace(g.keyOf()), "-")
}

// newTktGit resolves the repository once. A ticket pipeline with no repository
// is a mistake in the call, not a failure of the task: there is nothing to
// branch, nothing to check and nothing to merge.
func newTktGit(o *Orchestrator, keyOf func() string, target string) (*tktGit, error) {
	root := o.jl.Root
	top, err := gitCmd(root, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(top) == "" {
		return nil, usageErrf("%s is not inside a git repository, and `lca ticket` branches, commits, merges and pushes — there is nothing here to do that to", root)
	}
	t := strings.TrimSpace(top)
	if real, err := filepath.EvalSymlinks(t); err == nil {
		t = real
	}
	sub := "."
	if p, err := gitCmd(root, nil, nil, "rev-parse", "--show-prefix"); err == nil {
		sub = normSub(p)
	}
	return &tktGit{orch: o, top: t, sub: sub, keyOf: keyOf, targ: target, dir: o.cfg.stateDir()}, nil
}

// hooksOff is the flag pair every git invocation in this file carries, written
// once so a new call cannot be added without it.
func hooksOff() []string {
	return []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgsign=false"}
}

func (g *tktGit) BranchHead(branch string) (string, bool, error) {
	if strings.TrimSpace(branch) == "" {
		return "", false, usageErrf("pipeline: branch_prefix: produced an empty branch name for ticket %s", g.key())
	}
	out, _, code := gitRun(g.top, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch+"^{commit}")
	if code != 0 {
		// Not an error: "there is no such branch" is an ANSWER, and the probe above
		// this is written to act on it. A git that could not run at all answers -1,
		// which is the one case worth reporting.
		if code < 0 {
			return "", false, fmt.Errorf("git could not be run in %s", g.top)
		}
		return "", false, nil
	}
	return strings.TrimSpace(out), true, nil
}

// CutBranch creates the ticket's branch at the target branch's tip and checks it
// out as a worktree. It is called only when the probe found no branch, so it does
// NOT invent a free name the way freeBranch does for a delegation: one ticket,
// one branch, and a name already taken is somebody else's work — which probeBranch
// has already refused by then.
func (g *tktGit) CutBranch(branch string) (string, string, error) {
	unlock, err := lockGit(g.orch.lockDir())
	if err == nil {
		defer unlock()
	}
	base, found, err := g.BranchHead(g.targ)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", usageErrf("pipeline: target_branch: %q is not a branch in %s, so there is nothing to cut %s from. An unattended pipeline fetches before it runs; lca does not fetch on its own, because a fetch is a change to the repository nobody asked for.",
			g.targ, g.top, branch)
	}
	dir, err := worktreeDir(g.dir, "ticket-"+refWord(g.key()))
	if err != nil {
		return "", "", err
	}
	// Authorship FIRST, and the branch second. The ref is what tells a later run
	// that this branch is lca's — the namespace is not proof, and the ancestry
	// cannot tell lca's branch from a human's bookmark at the same commit — and
	// writing it after `worktree add` left a gap one git exec wide in which a
	// crash produced a branch nobody could claim: probeBranch read it as somebody
	// else's on every re-run, so the ticket could never continue and the operator
	// had to delete a branch by hand.
	//
	// Written first, the same crash leaves a ref pointing at a commit with no
	// branch beside it, which costs nothing and claims nothing: the next run finds
	// no branch, cuts one, and overwrites this ref with the same value. The only
	// cost of the order is that `lca clean --branches` may see a ref with no
	// branch, which it already tolerates — it deletes branches, not refs.
	if _, err := gitCmd(g.top, nil, nil, "update-ref", madeRef(branch), base); err != nil {
		return "", "", err
	}
	if _, err := gitCmd(g.top, nil, nil, append(hooksOff(), "worktree", "add", "-b", branch, dir, base)...); err != nil {
		return "", "", err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	writeOwner(dir, owner{session: "ticket-" + refWord(g.key()), role: "ticket", branch: branch, base: base})
	root := filepath.Join(dir, g.sub)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("%s does not exist in %s, so the ticket's worktree has no project in it", g.sub, branch)
	}
	return base, root, nil
}

// MadeAt is the commit lca cut this branch at, read from the authorship ref
// CutBranch writes the moment the branch exists.
//
// It is the evidence a crash can leave behind when the state file cannot. The
// window is one instruction wide — `worktree add -b` creates the ref and the
// save that records BranchAt comes after it — and a run killed inside it left a
// branch with no recorded base, which probeBranch then read as somebody else's
// branch on every single re-run. The ref was already being written for `lca
// clean --branches`; this is the same fact asked the other way round.
func (g *tktGit) MadeAt(branch string) (string, bool, error) {
	if strings.TrimSpace(branch) == "" {
		return "", false, nil
	}
	out, _, code := gitRun(g.top, nil, nil, "rev-parse", "--verify", "-q", madeRef(branch)+"^{commit}")
	if code != 0 {
		if code < 0 {
			return "", false, fmt.Errorf("git could not be run in %s", g.top)
		}
		return "", false, nil
	}
	return strings.TrimSpace(out), true, nil
}

// Worktree is CutBranch's other half: the branch is already there and ours, and
// what is missing is somewhere to work. A re-entered run needs this because a
// worktree is a DIRECTORY — `lca clean` removes it, a full disk takes it, a
// reboot with /tmp on tmpfs takes it — while the branch it held survives in the
// repository. Without it a resume would have a branch, a green probe and nowhere
// to put the coder.
//
// It adopts the live worktree when git still knows about one for this branch, and
// adds one when it does not. Never two: git refuses a second worktree on one
// branch, and that refusal is the thing that makes this safe rather than a race.
func (g *tktGit) Worktree(branch string) (string, error) {
	unlock, err := lockGit(g.orch.lockDir())
	if err == nil {
		defer unlock()
	}
	if dir, ok := g.worktreeOf(branch); ok {
		// `worktree list --porcelain` names the MAIN worktree with its branch line
		// like any other, so what git says holds this branch may be the OPERATOR'S
		// own checkout — their repository root, switched to the agent's branch in the
		// morning to look at last night's work, with their uncommitted edits in it.
		// Adopting that jails the coder there and `add -A` sweeps their work into
		// this ticket's commit, which is the one thing the top of this file swears
		// never happens. An adopted worktree has to be one lca made, and the owner
		// file beside it is what says so — the refusal MergeIn already makes for the
		// target branch's checkout, in the same words.
		if o := readOwner(dir); o.role != "ticket" || o.branch != branch {
			return "", usageErrf("%s holds %s and is not a worktree lca made, so lca will not work in it: anything uncommitted in that tree would be swept into this ticket's commit. Park that checkout somewhere else (`git -C %s switch --detach`), or remove that worktree.",
				dir, branch, dir)
		}
		root := filepath.Join(dir, g.sub)
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return root, nil
		}
		// The WORKTREE's own directory and not the project's inside it: a project
		// that does not exist in this branch's tree is a live worktree with a
		// legitimate answer, and pruning its record would delete git's only note of
		// a directory somebody may be working in.
		if _, err := os.Stat(dir); err == nil {
			return "", fmt.Errorf("%s holds %s and has no %s in it, so the ticket's worktree has no project to work in", dir, branch, g.sub)
		}
		// git lists it and the directory is gone: a stale administrative entry, which
		// blocks `worktree add` until it is pruned. Pruning removes the bookkeeping
		// only; nothing of the branch is touched.
		gitCmd(g.top, nil, nil, "worktree", "prune")
	}
	dir, err := worktreeDir(g.dir, "ticket-"+refWord(g.key()))
	if err != nil {
		return "", err
	}
	if _, err := gitCmd(g.top, nil, nil, append(hooksOff(), "worktree", "add", dir, branch)...); err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	writeOwner(dir, owner{session: "ticket-" + refWord(g.key()), role: "ticket", branch: branch})
	return filepath.Join(dir, g.sub), nil
}

// worktreeOf asks git which worktree holds a branch. `worktree list --porcelain`
// is the only answer that is not a guess: it names the directory and the branch
// of every worktree including the main one, and it is what `git worktree add`
// itself consults before refusing.
func (g *tktGit) worktreeOf(branch string) (string, bool) {
	out, err := gitCmd(g.top, nil, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false
	}
	dir := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "worktree "):
			dir = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				dir = real
			}
			return dir, dir != ""
		}
	}
	return "", false
}

// Contains is "is `ancestor` in the history of `of`", and it is how every
// git-side probe tells our effect from somebody else's.
//
// A commit this repository does not HAVE answers no, and that is the one
// judgement in here worth stating. It is the ordinary case on the push probe: a
// remote branch somebody else pushed to names a sha whose objects were never
// fetched, so the question cannot be answered at all. Every caller reads no as
// "that is not ours, stop" — probePush refuses to force, probeBranch refuses to
// work on it — and stopping is the safe direction for a question nobody can
// answer. Answering yes, or failing the transition, would both end with a
// pipeline that either overwrote work or stalled forever on a branch somebody
// pushed once.
func (g *tktGit) Contains(ancestor, of string) (bool, error) {
	if ancestor == "" || of == "" {
		return false, nil
	}
	if ancestor == of {
		return true, nil
	}
	_, errs, code := gitRun(g.top, nil, nil, "merge-base", "--is-ancestor", ancestor, of)
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	for _, rev := range []string{ancestor, of} {
		if _, _, c := gitRun(g.top, nil, nil, "cat-file", "-e", rev+"^{commit}"); c != 0 {
			return false, nil
		}
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %s", shortSha(ancestor), shortSha(of),
		strings.TrimSpace(lastLines(errs, 3, 300)))
}

// MergeIn merges the ticket's branch into the target branch IN THE OBJECT
// DATABASE and moves the target's ref — and that is the whole design, for two
// reasons that are really one reason.
//
// First, `git merge` needs a working tree, and the only working tree that has the
// target branch checked out is the operator's. branch.go's note says what that
// costs: a text patch against a dirty tree fails whether the two sides collide or
// merely sit three lines apart, `merge --no-ff` refuses outright, and nothing in
// this program is allowed to stage, commit or reset in somebody's checkout.
// `merge-tree --write-tree` answers the real question without a tree at all, and
// when it says conflict it is a conflict.
//
// Second, the ref move is a compare-and-swap. `update-ref <ref> <new> <old>`
// fails if the target has moved since we read it, which is exactly the window a
// slow merge leaves open — and a lost update here is unreviewed work silently
// dropped out of a branch somebody is about to release.
//
// It REFUSES when a working tree holds the target branch, naming the directory.
// Moving a branch under a live checkout leaves every merged file showing as a
// local deletion in that tree's `git status`, which is indistinguishable from
// somebody having deleted them; the remedy is in the message, and it is what an
// unattended clone should be doing anyway.
func (g *tktGit) MergeIn(branch, target, sha string) (string, error) {
	if ok, ver := g.orch.mergeTree3Way(g.top); !ok {
		return "", usageErrf("merging %s into %s needs `git merge-tree --write-tree`, which arrived in git 2.38, and this git is %s. lca will not merge through a working tree instead: the only tree with %s checked out is the operator's, and nothing here stages, commits or resets in it.",
			branch, target, ver, target)
	}
	unlock, err := lockGit(g.orch.lockDir())
	if err == nil {
		defer unlock()
	}
	if dir, ok := g.worktreeOf(target); ok {
		return "", usageErrf("%s is checked out in %s, so lca will not move it: every file this merge brings in would show up as a local deletion in that tree's `git status`, and lca does not touch a working tree it does not own. Park that checkout somewhere else (`git -C %s switch --detach`), or point pipeline: target_branch at a branch nobody has checked out.",
			target, dir, dir)
	}
	old, found, err := g.BranchHead(target)
	if err != nil {
		return "", err
	}
	if !found {
		return "", usageErrf("pipeline: target_branch: %q is not a branch in %s", target, g.top)
	}
	head, found, err := g.BranchHead(branch)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%s is gone, so there is nothing to merge into %s", branch, target)
	}
	// PushBranch's rule, held here too, where it matters more: the gate approved a
	// COMMIT, not a branch name. The check ran over `sha` and the reviewer read
	// `sha`; anything the branch has grown since is work no check ran on and no
	// reviewer saw, and a merge cannot be taken back the way a push can be
	// re-pushed. So the branch has to still be exactly the commit that was
	// approved, and both shas are named when it is not.
	if strings.TrimSpace(sha) == "" {
		return "", fmt.Errorf("nothing is recorded as the reviewed commit on %s, so there is nothing lca may merge into %s", branch, target)
	}
	if head != sha {
		return "", usageErrf("%s is at %s and the commit this run's check and review were about is %s — somebody added to the branch after it was approved, and lca merges only what was reviewed. Review the branch again (re-run after `git update-ref refs/heads/%s %s` if those commits were not meant to be there), or merge it by hand.",
			branch, shortSha(head), shortSha(sha), branch, sha)
	}
	out, errs, code := gitRun(g.top, nil, nil, "merge-tree", "--write-tree", old, head)
	switch code {
	case 0:
	case 1:
		// A conflict after a green check and an approve is a person's decision, not a
		// model's: the two sides both passed review and disagree about the same lines,
		// and resolving that unattended is how a release gets a silent wrong answer.
		// The three stages are in the output, the branch is still there, and the
		// ticket comment says so.
		return "", fmt.Errorf("%s does not merge into %s without a decision somebody has to make — %s conflict. Nothing was merged, nothing was pushed, and %s is still there to merge by hand",
			branch, target, orNone(mergeConflictFiles(out)), branch)
	default:
		return "", fmt.Errorf("merging %s into %s failed: %s", branch, target,
			strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 4, 400)))
	}
	tree := strings.TrimSpace(firstLine(out))
	if tree == "" {
		return "", fmt.Errorf("git merge-tree wrote no tree for %s into %s", branch, target)
	}
	msg := fmt.Sprintf("Merge %s into %s\n\nlca ticket %s", branch, target, g.key())
	commit, err := gitCmd(g.top, nil, nil, "commit-tree", tree, "-p", old, "-p", head, "-m", msg)
	if err != nil {
		return "", err
	}
	merge := strings.TrimSpace(commit)
	// The old value is the swap's guard. Without it, a merge that took four
	// minutes while somebody else pushed to the target would overwrite their
	// commit with a merge that never saw it.
	if _, err := gitCmd(g.top, nil, nil, "update-ref", "refs/heads/"+target, merge, old); err != nil {
		return "", fmt.Errorf("%s moved while %s was being merged into it, so lca did not move it: %w", target, branch, err)
	}
	return merge, nil
}

// mergeConflictFiles pulls the conflicted paths out of merge-tree's output. It
// prints the tree, then the conflicted index stages, then its own messages, and
// the stage lines are the only part that names files — see gitRun's note on why
// the exit code is the only thing that tells a conflict from a failure.
func mergeConflictFiles(out string) string {
	mc := parseMergeTree(out)
	var names []string
	for _, f := range mc.paths {
		if !contains(names, f) {
			names = append(names, f)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ", ")
}

func (g *tktGit) RemoteHead(remote, branch string) (string, bool, error) {
	if strings.TrimSpace(remote) == "" {
		return "", false, usageErrf("pipeline: remote: nothing says which remote to push %s to", branch)
	}
	out, errs, code := gitRun(g.top, nil, nil, "ls-remote", "--heads", "--exit-code", remote, "refs/heads/"+branch)
	switch code {
	case 0:
	case 2:
		return "", false, nil // --exit-code: the remote answered and has no such ref
	default:
		// Anything else is the remote not answering: no network, no credentials, a
		// host that is down. That is infra_error — the ticket goes back in the queue
		// and nothing on it is touched — and not "the agent did not manage it".
		return "", false, &infraErr{what: "git remote " + remote,
			err: fmt.Errorf("%s", strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 4, 400)))}
	}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && strings.TrimSpace(ref) == "refs/heads/"+branch {
			return strings.TrimSpace(sha), true, nil
		}
	}
	return "", false, nil
}

// PushBranch pushes ONE commit to ONE branch, by its sha, with no force of any
// kind.
//
//   - the SHA and not the branch name: between the probe that approved this push
//     and this line, nothing may have moved the local branch — but if something
//     did, `git push origin HEAD:branch` would send whatever is there now, and
//     what was reviewed is the sha. Pushing the sha is what makes the review
//     mean something.
//   - one refspec, fully spelled: no `--all`, no `--tags`, no configured
//     push.default to reinterpret it. An unattended run must push exactly what it
//     says it pushes.
//   - never --force and never --force-with-lease. A remote that rejects this
//     push is a remote whose branch holds commits this run cannot account for,
//     and probePush has already refused that case; if it happens anyway, the
//     rejection is the right outcome and the message says so.
//   - hooks off and --no-verify: a pre-push hook belongs to the person who wrote
//     it, and in a cron job it is something that can only hang or fail.
func (g *tktGit) PushBranch(remote, branch, sha string) error {
	if strings.TrimSpace(sha) == "" {
		return fmt.Errorf("nothing is recorded as the commit to push for %s, so there is nothing to push", branch)
	}
	args := append(hooksOff(), "push", "--no-verify", remote, sha+":refs/heads/"+branch)
	out, errs, code := gitRun(g.top, nil, nil, args...)
	if code == 0 {
		return nil
	}
	whole := out + "\n" + errs
	text := strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 6, 600))
	// A rejection is the remote's answer and a verdict on this change's place in
	// the world; it is not the remote being unreachable, and the two go to
	// different queues. Classified on the WHOLE output and not on the clipped tail:
	// git prints "! [rejected]" first and then four lines of hints, so the one line
	// that says what happened is the one a six-line tail drops.
	if strings.Contains(whole, "[rejected]") || strings.Contains(whole, "non-fast-forward") ||
		strings.Contains(whole, "fetch first") || strings.Contains(whole, "Updates were rejected") {
		return fmt.Errorf("%s refused the push of %s to %s: it has commits this run has not seen, and lca never forces. %s",
			remote, shortSha(sha), branch, text)
	}
	return &infraErr{what: "git remote " + remote, err: fmt.Errorf("pushing %s to %s failed: %s", branch, remote, text)}
}
