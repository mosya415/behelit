package main

import (
	"context"
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

// RemoteHead and PushBranch take the RUN'S context, and they are the only two
// methods on this interface that do. They are the only two that talk to a
// network: everything else here is local plumbing against the object database,
// which cannot block on anybody else's machine. A remote that accepts the
// connection and then says nothing is the shape that cost a whole night — the
// ticket lock held, the ticket claimed, and SIGTERM unable to end it — so the
// deadline the operator named has to reach these two. gitRunIn carries it.
func (g *tktGit) RemoteHead(ctx context.Context, remote, branch string) (string, bool, error) {
	if strings.TrimSpace(remote) == "" {
		return "", false, usageErrf("pipeline: remote: nothing says which remote to push %s to", branch)
	}
	out, errs, code := gitRunIn(ctx, g.top, nil, nil, "ls-remote", "--heads", "--exit-code", remote, "refs/heads/"+branch)
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
func (g *tktGit) PushBranch(ctx context.Context, remote, branch, sha string) error {
	if strings.TrimSpace(sha) == "" {
		return fmt.Errorf("nothing is recorded as the commit to push for %s, so there is nothing to push", branch)
	}
	args := append(hooksOff(), "push", "--no-verify", remote, sha+":refs/heads/"+branch)
	out, errs, code := gitRunIn(ctx, g.top, nil, nil, args...)
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

// ── the one supervised attempt at a conflict ─────────────────────────────────
//
// Four operations, and the shape of them is the point: the TARGET BRANCH is
// never touched by any of it. A conflict is resolved by merging the target INTO
// the ticket's branch, inside the worktree lca cut for this ticket, so every
// write below lands on a ref lca made and can throw away — and what reaches the
// target afterwards is still MergeIn's ordinary merge of a branch whose check is
// green and whose diff a reviewer read. The gate does not move; only the branch
// standing in front of it does.
//
// `reset --hard` and `merge --abort` appear here, which the top of this file
// swears never happens. They happen in LCA'S OWN WORKTREE and nowhere else —
// the directory CutBranch made, with the owner file beside it that says so —
// which is the one place §3's never-list does not reach, exactly as handBack
// says for the resolution worktree of a delegation. Nothing here runs in the
// operator's tree, and the one ref outside this ticket's branch that any of it
// writes is refs/lca/resolve/<branch>, which `git branch` never lists.

// pathspec confines a question to the project inside the repository, the same
// confinement commitWork's commit and diff() already use: a file above the
// project's own directory is not this ticket's work, whoever changed it.
func (g *tktGit) pathspec() []string {
	if g.sub == "" || g.sub == "." {
		return nil
	}
	return []string{"--", g.sub}
}

// worktreeTop is the worktree's own root, read from the worktree rather than
// remembered. Every git question below is asked THERE and not in the project
// directory inside it, because `git diff` prints paths relative to where it was
// run and a path list that means something different per project layout is a
// path list no comparison can be written against.
func (g *tktGit) worktreeTop(dir string) (string, error) {
	out, err := gitCmd(dir, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not a git worktree any more, so the conflict cannot be resolved in it: %w", dir, err)
	}
	top := strings.TrimSpace(out)
	if top == "" {
		return "", fmt.Errorf("%s is not a git worktree any more", dir)
	}
	return top, nil
}

// MergeConflicts is MergeIn's question with none of its consequences: it reads
// the same two tips and runs the same `merge-tree --write-tree`, and the only
// thing it writes is a tree object nobody references.
//
// It exists so the question is asked BEFORE an integrator is bought. MergeIn
// asks it at the moment it matters and must keep doing so — a target branch that
// moved in between is the whole reason any of this exists — so this is a
// cheaper, earlier copy of the same question and never a substitute for it.
func (g *tktGit) MergeConflicts(branch, target string) (string, bool, error) {
	if ok, ver := g.orch.mergeTree3Way(g.top); !ok {
		return "", false, usageErrf("asking whether %s merges into %s needs `git merge-tree --write-tree`, which arrived in git 2.38, and this git is %s",
			branch, target, ver)
	}
	old, found, err := g.BranchHead(target)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, usageErrf("pipeline: target_branch: %q is not a branch in %s", target, g.top)
	}
	head, found, err := g.BranchHead(branch)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, fmt.Errorf("%s is gone, so there is nothing to ask about merging into %s", branch, target)
	}
	out, errs, code := gitRun(g.top, nil, nil, "merge-tree", "--write-tree", old, head)
	switch code {
	case 0:
		return "", false, nil
	case 1:
		return mergeConflictFiles(out), true, nil
	}
	return "", false, fmt.Errorf("asking git whether %s merges into %s failed: %s", branch, target,
		strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 4, 400)))
}

// MergeTargetIn merges the target branch into the ticket's branch in the
// ticket's own worktree and stops at the conflict, leaving the markers, the
// unmerged index and MERGE_HEAD where a resolution is made.
//
// Three refusals before it starts, and each of them is a merge that could not be
// judged afterwards:
//
//   - the worktree has to be on this branch. One ticket has one worktree and git
//     refuses a second on one branch, so a worktree holding something else is a
//     path the state file is wrong about;
//   - it has to be CLEAN. A merge made on top of uncommitted work merges that
//     work too, and nothing checked it, nothing reviewed it and nobody asked for
//     it. The implementation's own commitWork leaves a clean tree after every
//     attempt, so a dirty one here is a surprise and surprises stop;
//   - the collision has to be inside the project's own directory. `git commit`
//     refuses an index with unmerged entries, so a conflict above the project is
//     one commitWork can never resolve — it would come back as "the integrator
//     did not manage it" three minutes later, blaming the wrong thing.
//
// On any error nothing is left behind: the merge is aborted, the branch is where
// it was, and no attempt has been spent.
func (g *tktGit) MergeTargetIn(branch, target, dir string) (string, string, error) {
	unlock, err := lockGit(g.orch.lockDir())
	if err == nil {
		defer unlock()
	}
	top, err := g.worktreeTop(dir)
	if err != nil {
		return "", "", err
	}
	on, err := gitCmd(top, nil, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(on) != branch {
		return "", "", fmt.Errorf("%s has %s checked out and not %s, so lca will not merge %s into it",
			top, strings.TrimSpace(on), branch, target)
	}
	if st, err := gitCmd(top, nil, nil, "status", "--porcelain"); err != nil {
		return "", "", err
	} else if strings.TrimSpace(st) != "" {
		return "", "", fmt.Errorf("%s has uncommitted work in it, and merging %s on top of work no check ran over makes a merge nobody can judge — %s was not merged into %s",
			top, target, target, branch)
	}
	sha, found, err := g.BranchHead(target)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", usageErrf("pipeline: target_branch: %q is not a branch in %s", target, g.top)
	}
	// --no-ff, so the merge is always a commit with two parents and ResolvedAt can
	// check that it is one. A fast-forward here would be a target that already
	// contains the branch, which probeMerge has answered long before.
	msg := fmt.Sprintf("Merge %s into %s\n\nlca ticket %s: the target branch moved under this change", target, branch, g.key())
	out, errs, code := gitRun(top, nil, nil, append(hooksOff(), "merge", "--no-ff", "--no-edit", "-m", msg, sha)...)
	if code == 0 {
		return sha, "", nil
	}
	files := unmergedPaths(top)
	if len(files) == 0 {
		// Not a conflict: git could not do the merge at all. The exit code alone
		// cannot tell the two apart — branch.go's note on gitRun says why — so the
		// index is what is asked, and an index with nothing unmerged means nothing is
		// half done either.
		g.abortMerge(top)
		return sha, "", fmt.Errorf("merging %s into %s in %s failed: %s", target, branch, top,
			strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 4, 400)))
	}
	if ps := g.pathspec(); len(ps) > 0 {
		var above []string
		for _, f := range files {
			if f != g.sub && !strings.HasPrefix(f, g.sub+"/") {
				above = append(above, f)
			}
		}
		if len(above) > 0 {
			g.abortMerge(top)
			return sha, strings.Join(files, ", "), fmt.Errorf("%s and %s collide in %s, which is above %s — lca commits only inside the project's own directory, so a resolution up there could never be committed. %s was not merged into %s",
				target, branch, strings.Join(above, ", "), g.sub, target, branch)
		}
	}
	return sha, strings.Join(files, ", "), nil
}

// unmergedPaths is the index's own answer to "what collided", which is the only
// one that is not a guess: it is what `git commit` consults before refusing, and
// it is still true after a model has edited half of the files.
func unmergedPaths(top string) []string {
	out, err := gitCmd(top, nil, nil, "diff", "--name-only", "--diff-filter=U", "--no-relative")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.TrimSpace(line); f != "" && !contains(files, f) {
			files = append(files, f)
		}
	}
	return files
}

// abortMerge puts a worktree back the way `git merge` found it. Best effort and
// unchecked on purpose: it is called on paths that are already reporting a
// failure, and a second error on top of the first would only hide it.
func (g *tktGit) abortMerge(top string) {
	if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD"); code == 0 {
		gitRun(top, nil, nil, append(hooksOff(), "merge", "--abort")...)
	}
}

// ResolvedAt judges what the integrator left, by asking git four questions a
// model cannot answer about itself.
//
// The first three are that there IS a resolution: no MERGE_HEAD still sitting
// there, a commit that is not the one the attempt started from, and a commit
// that is really the merge — two parents, the approved commit and the target.
// The parent check is not bookkeeping: the integrator has run_command in its
// jail, so "resolved it" could be a `git commit --amend`, a reset or a cherry
// pick, and a branch tip that is not a merge of those two commits is not the
// thing this transition was going to hand to the merge gate.
//
// The fourth is the one that needs saying out loud, because a resolution that
// DROPS the ticket's change passes a check as easily as a correct one — `git
// checkout --theirs .` is one keystroke, the markers are gone, the tests are
// green and the branch now says exactly what the target already said. It is
// caught by comparing three diffs, all confined to the project:
//
//	changed   = base..approved   the change a reviewer approved, base being the
//	                             merge base of the branch and the target
//	differing = target..approved where the approved change and the target still
//	                             disagree at all
//	landing   = target..head     what this merge actually ADDS to the target
//
// A path in `changed` and in `differing` but NOT in `landing` is a path where
// the ticket had a change, the target did not have it, and the merge no longer
// carries it: the resolution took the target's side whole and the reviewed work
// is gone. `differing` is what keeps the honest cases out — a path where the
// target did the same thing already, or where the ticket only reformatted what
// the target then rewrote, is identical on both sides and nothing was lost by
// keeping one. Where it is wrong it is wrong in the safe direction: a conflict
// for a person, which is where the whole transition defaults anyway.
func (g *tktGit) ResolvedAt(branch, dir, approved, target string) (string, string, error) {
	top, err := g.worktreeTop(dir)
	if err != nil {
		return "", "", err
	}
	if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD"); code == 0 {
		return "", "", fmt.Errorf("the merge in %s was never finished — MERGE_HEAD is still there, so nothing was resolved", top)
	}
	head, found, err := g.BranchHead(branch)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", fmt.Errorf("%s is gone, so there is no resolution on it", branch)
	}
	if head == approved {
		return "", "", fmt.Errorf("%s is still at %s, so nothing was committed and the merge of %s was not resolved",
			branch, shortSha(approved), target)
	}
	line, err := gitCmd(top, nil, nil, "rev-list", "--parents", "-1", head)
	if err != nil {
		return "", "", err
	}
	parents := strings.Fields(strings.TrimSpace(line))
	if len(parents) > 0 {
		parents = parents[1:] // the first field is the commit itself
	}
	if len(parents) != 2 || !contains(parents, approved) || !contains(parents, target) {
		return "", "", fmt.Errorf("%s is at %s, which is not a merge of %s and %s — whatever is there, it is not the resolution of this conflict",
			branch, shortSha(head), shortSha(approved), shortSha(target))
	}
	base, err := gitCmd(g.top, nil, nil, "merge-base", approved, target)
	if err != nil {
		return "", "", err
	}
	changed, err := g.changedNames(top, strings.TrimSpace(base), approved)
	if err != nil {
		return "", "", err
	}
	differing, err := g.changedNames(top, target, approved)
	if err != nil {
		return "", "", err
	}
	landing, err := g.changedNames(top, target, head)
	if err != nil {
		return "", "", err
	}
	var lost []string
	for _, p := range changed {
		if contains(differing, p) && !contains(landing, p) {
			lost = append(lost, p)
		}
	}
	return head, strings.Join(lost, ", "), nil
}

// changedNames is `git diff --name-only a b`, confined to the project and
// spelled from the repository root so three of these can be compared as sets.
func (g *tktGit) changedNames(top, a, b string) ([]string, error) {
	args := append([]string{"diff", "--name-only", "--no-relative", a, b}, g.pathspec()...)
	out, err := gitCmd(top, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.TrimSpace(line); f != "" && !contains(names, f) {
			names = append(names, f)
		}
	}
	return names, nil
}

// AbandonResolution puts the branch and its worktree back at the commit the
// attempt started from, and parks whatever the integrator committed on
// refs/lca/resolve/<branch> first.
//
// The order is the whole of it, and it is handBack's lesson in a smaller place:
// the attempt is SAVED BEFORE the branch is moved, and when it cannot be saved
// the branch is not moved at all. A committed answer a person can read beats a
// tidy branch that silently discarded one — and what a person is told by the
// refusal ("the rejected attempt is kept at …") has to be true when they go and
// look.
//
// Nothing committed is the other half: the worktree is still mid-merge, there is
// nothing to keep, and `git merge --abort` leaves the branch exactly at the
// approved commit, which is what the refusal promises. The markers are not put
// back the way handBack puts them back, because there the human was being handed
// a worktree to finish in; here they are being handed a BRANCH to merge by hand,
// and a worktree left mid-merge would only block the next night's run.
func (g *tktGit) AbandonResolution(branch, dir, approved string) (string, error) {
	unlock, err := lockGit(g.orch.lockDir())
	if err == nil {
		defer unlock()
	}
	top, err := g.worktreeTop(dir)
	if err != nil {
		return "", err
	}
	if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD"); code == 0 {
		if _, errs, code := gitRun(top, nil, nil, append(hooksOff(), "merge", "--abort")...); code != 0 {
			return "", fmt.Errorf("the unfinished merge in %s could not be aborted: %s", top,
				strings.TrimSpace(lastLines(errs, 3, 300)))
		}
		return "", nil
	}
	head, found, err := g.BranchHead(branch)
	if err != nil {
		return "", err
	}
	if !found || head == approved {
		return "", nil // nothing was committed, so there is nothing to put back
	}
	ref := "refs/lca/resolve/" + branch
	if _, err := gitCmd(g.top, nil, nil, "update-ref", ref, head); err != nil {
		// Without somewhere to keep it, the reset would make the attempt unreachable.
		// Leave the branch where it is: the gate refuses this ticket from here on, so
		// nothing merges either way, and the work is still there to read.
		return "", fmt.Errorf("the integrator's resolution of %s could not be put on %s, so lca left %s where it is rather than discard it: %w",
			branch, ref, branch, err)
	}
	// `reset --hard` in LCA'S OWN worktree, which is the one place the never-list
	// at the top of this file does not reach: lca made this directory, the owner
	// file beside it says so, and nothing of the operator's is in it. It moves the
	// branch with it, because the branch is what is checked out here.
	if _, errs, code := gitRun(top, nil, nil, append(hooksOff(), "reset", "--hard", approved)...); code != 0 {
		return ref, fmt.Errorf("%s could not be put back at %s after a rejected resolution: %s",
			branch, shortSha(approved), strings.TrimSpace(lastLines(errs, 3, 300)))
	}
	return ref, nil
}
