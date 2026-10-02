package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// What lca leaves behind, who looks at it, and who removes it.
//
// Under `apply: branch` a delegation leaves real things in the repository: a
// branch, its commits, a worktree while it runs, a lock file while it writes, a
// journal line while it integrates. All of it is deliberate — the whole point of
// the mode is that a failed or conflicted delegation can still be read next
// morning — so NOTHING here happens on a timer or on the way out of a session.
// `/branches` shows it; `lca clean` removes it, and only what it was asked for.
//
// The three rules this file exists to keep:
//
//   - An unintegrated branch is never deleted unless the operator names it. It
//     is the only copy of work a verifier passed or a role attempted.
//   - An integrated branch is kept for a DAY. "It landed, so it can go" is true
//     of the objects and false of the human who wants to see what landed; a day
//     is the span in which somebody comes back to a run they left.
//   - A worktree is removed only when its owner process is gone, and the prune
//     always carries --expire: a bare `worktree prune` in one lca deletes the
//     admin entry of a worktree another lca created milliseconds ago, and that
//     worktree's next git command fails with nothing anyone can act on.
//   - A branch goes only when lca RECORDED creating it. The name `lca/…` is one
//     anybody may use and the ancestry test says yes to the project's whole
//     history, so neither is proof of authorship (see madeRef).
//   - A resolution worktree belongs to a PERSON, not to a process: the pid that
//     made it is a `lca merge` CLI that exited a second later, so the pid test
//     alone would delete the hand-over the moment it was handed over.

// keptFor is how long an integrated branch survives `lca clean --branches`, and
// how long a resolution worktree is left for its human. Measured from the
// INTEGRATION and not from the branch's last commit: an interruption recovered
// two days later integrates a branch whose newest commit is two days old, and
// the tip's date would let the same minute's cleanup delete it before anybody
// could look at what landed.
//
// The operator's own number, and not configurable on purpose: a knob here
// invites "0" and the first thing anyone loses is the branch they were about to
// look at.
const keptFor = 24 * time.Hour

// repoTop answers "which repository am I in" the one way that is correct from a
// linked worktree as well as from the main one.
func repoTop(root string) (string, error) {
	out, err := gitCmd(root, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	top := strings.TrimSpace(out)
	if real, e := filepath.EvalSymlinks(top); e == nil {
		top = real
	}
	return top, nil
}

// ── /branches ───────────────────────────────────────────────────────────────

// cmdBranches is the review command: everything lca made in this repository,
// each row saying whether it is in the working tree, and every piece of
// wreckage a killed process left. It changes nothing — the one thing a review
// command must never do is tidy up behind the reader's back.
func (r *Repl) cmdBranches(arg string) bool {
	graph := strings.Contains(arg, "graph")
	top, err := repoTop(r.orch.jl.Root)
	if err != nil {
		errLine("%v", err)
		return false
	}
	printBranches(top, r.orch.cfg.stateDir(), r.orch.lockDir(), graph)
	return false
}

func printBranches(top, stateDir, lockDir string, graph bool) {
	rows := listBranches(top)
	journals := readJournals(stateDir)
	orphans := orphanWorktrees(stateDir)
	stale := staleLeases(lockDir)
	if len(rows) == 0 && len(journals) == 0 && len(orphans) == 0 && len(stale) == 0 && len(mergeWorktreesOf(stateDir, "")) == 0 {
		fmt.Println("  " + faint("lca has made no branches in this repository — `apply: branch` in roles.yaml makes them"))
		return
	}
	if len(rows) > 0 {
		var table [][]string
		for _, b := range rows {
			state := "unintegrated"
			if b.integrated {
				state = "integrated"
				if time.Since(b.when) > keptFor {
					state = "integrated, ready to clean"
				}
			}
			where := b.subject
			if b.worktree != "" {
				// git's own marker for a branch a worktree holds. It is also the
				// reason `branch -D` would refuse it, which is worth seeing here.
				where = "+ checked out in " + b.worktree
			}
			table = append(table, []string{b.name, b.head, state, where})
		}
		sectionTable("branches lca made", "", []string{"branch", "head", "state", "where / what"}, table)
	}
	for _, j := range journals {
		warnLine("%s", interruptedText(j))
	}
	for _, o := range orphans {
		warnLine("%s was left by pid %d, which is gone — `lca clean` removes it", o.dir, o.pid)
	}
	// Separately, and NOT as wreckage: an open resolution worktree is a person's
	// unfinished work, so the line says how to finish it instead of inviting a
	// cleanup that would delete it.
	for _, o := range mergeWorktreesOf(stateDir, "") {
		if spentMergeWorktree(o) {
			continue
		}
		warnLine("a merge of %s is open at %s — resolve it there, then `lca merge --finish %s`", firstNonEmpty(o.source, "a branch"), o.dir, firstNonEmpty(o.branch, o.dir))
	}
	for _, l := range stale {
		warnLine("a write lease on %s is held by pid %d, which is gone — the next writer takes it over, `lca clean` sweeps it", firstNonEmpty(l.held, filepath.Base(l.path)), l.pid)
	}
	if graph {
		out, _, code := gitRun(top, nil, nil, "log", "--graph", "--oneline", "--decorate",
			"--all", "--glob=refs/lca/*", "--max-count=40")
		if code == 0 {
			section("what was merged into your tree")
			fmt.Println(strings.TrimRight(out, "\n"))
		}
	}
	hint("/merge <branch> checks a branch out as a real merge with markers; lca clean --branches deletes the integrated ones after a day")
}

// ── /merge ──────────────────────────────────────────────────────────────────

// cmdMerge is the human's route through a conflict, in two halves:
//
//	/merge <branch>    check the merge out in its own worktree, with markers
//	/merge --finish    integrate what the human committed there
//
// It is a checkout, which the integration itself never does — but it is OUR
// worktree, not the user's, and a clean checkout is exactly what makes git do a
// real merge: UU in status, three stages in `git ls-files -u`, labelled markers,
// and `git merge --abort` available. Markers never go in the user's own files.
func (r *Repl) cmdMerge(arg string) bool {
	args := strings.Fields(arg)
	finish := false
	var name string
	for _, a := range args {
		switch {
		case a == "--finish" || a == "-finish" || a == "finish":
			finish = true
		case strings.HasPrefix(a, "-"):
			errLine("/merge takes a branch name, or --finish — %q is neither", a)
			return false
		default:
			name = a
		}
	}
	top, err := repoTop(r.orch.jl.Root)
	if err != nil {
		errLine("%v", err)
		return false
	}
	if finish {
		mergeFinish(top, r.orch.cfg, r.sess.integrateEnv(), name, "/merge")
		return false
	}
	if name == "" {
		fmt.Println("  " + faint("/merge <branch> — /branches lists them; /merge --finish integrates what you resolved"))
		return false
	}
	mergeStart(r.orch, top, name, "/merge")
	return false
}

// mergeStart makes the resolution worktree and tells the human the three
// commands that follow. It prints rather than returns them because this is the
// one place in the design where a person, not a model, does the work.
func mergeStart(o *Orchestrator, top, name, invoked string) {
	if !canMergeHere(top) {
		return
	}
	branch := resolveBranchName(top, name)
	if branch == "" {
		errLine("%s is not a branch lca made in this repository (/branches lists them)", name)
		return
	}
	sid, task := splitBranch(branch)
	w, conflicted, err := o.mergeWorktree(top, sid, task, branch)
	if err != nil {
		errLine("%v", err)
		if strings.Contains(err.Error(), "refs/lca/yours") {
			hint("that session's snapshot has been cleaned up, so there is no 'ours' side left to merge against — `git merge %s` in a scratch worktree of your own is the honest fallback", branch)
		}
		return
	}
	okLine("the merge is checked out at %s", w.dir)
	if conflicted != "" {
		fmt.Println("  " + faint("conflicted: %s", strings.Join(strings.Split(conflicted, "\n"), ", ")))
	} else {
		fmt.Println("  " + faint("git merged it cleanly there — nothing to resolve, just commit"))
	}
	section("what to do there")
	for _, l := range []string{
		"cd " + w.dir,
		// HEAD and not refs/lca/yours/<sid>: this worktree has the merge branch
		// checked out, so git labels OUR side HEAD. The ref name only shows up as
		// a marker label when the merge is computed against a raw ref, not in a
		// checkout.
		"$EDITOR <the conflicted files>   # <<<<<<< HEAD is your tree, >>>>>>> " + branch + " is the role's",
		"git add -A && git commit         # or: git merge --abort",
	} {
		fmt.Println("  " + l)
	}
	hint("then `%s --finish %s` writes the resolution into your working tree the same way a delegation's merge would", invoked, w.branch)
}

// mergeFinish integrates what the human committed in a resolution worktree.
//
// The base is the D the merge worktree was CUT FROM — recorded in its owner file
// — and not the delegation's snapshot. With the snapshot the resolution
// re-conflicts with itself: it contains the caller's side of the conflict, which
// against the snapshot reads as a second change to the same region (measured).
func mergeFinish(top string, cfg Config, env integrateEnv, name, invoked string) {
	if !canMergeHere(top) {
		return
	}
	ws := mergeWorktreesOf(cfg.stateDir(), name)
	switch len(ws) {
	case 0:
		errLine("there is no merge worktree to finish — `%s <branch>` makes one", invoked)
		return
	case 1:
	default:
		// The MERGE branch, not the source. Two worktrees resolving one coder branch
		// print the same source, so the line that is supposed to disambiguate was
		// the command that had just failed, printed twice, identically — and typing
		// either reproduced it. A human who aborts the first merge (the tool's own
		// suggestion) and runs `lca merge <branch>` again had no route from a
		// resolved merge back to their working tree, in the one path §2.6 reserves
		// for "nobody else can resolve it".
		errLine("%d merge worktrees are open — name the one to finish:", len(ws))
		for _, o := range ws {
			fmt.Println("  " + faint("%s --finish %s   (resolving %s in %s)", invoked, firstNonEmpty(o.branch, o.dir), o.source, o.dir))
		}
		return
	}
	own := ws[0]
	dirty, _, _ := gitRun(own.dir, nil, nil, "status", "--porcelain")
	for _, line := range strings.Split(dirty, "\n") {
		if strings.HasPrefix(line, "UU") || strings.HasPrefix(line, "AA") || strings.HasPrefix(line, "DU") || strings.HasPrefix(line, "UD") || strings.HasPrefix(line, "AU") || strings.HasPrefix(line, "UA") || strings.HasPrefix(line, "DD") {
			errLine("%s still has unresolved conflicts — resolve them, `git add -A && git commit` there, then %s --finish", own.dir, invoked)
			return
		}
	}
	if strings.TrimSpace(dirty) != "" {
		errLine("%s has uncommitted changes — commit them there first: a resolution is integrated from a commit, not from a working tree", own.dir)
		return
	}
	head, err := gitCmd(own.dir, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		errLine("%v", err)
		return
	}
	sub := "."
	if p, e := gitCmd(cfg.Root, nil, nil, "rev-parse", "--show-prefix"); e == nil {
		sub = normSub(p)
	}
	w := &worktree{top: top, dir: own.dir, root: filepath.Join(own.dir, sub), sub: filepath.ToSlash(sub),
		base: own.base, branch: own.branch}
	if w.base == "" {
		// An owner file from before the base was recorded, or a hand-made
		// worktree: the first parent of the resolution IS the D it was cut from as
		// long as the human did not commit twice, and saying which assumption is
		// being made beats refusing.
		if p, e := gitCmd(top, nil, nil, "rev-parse", strings.TrimSpace(head)+"^1"); e == nil {
			w.base = strings.TrimSpace(p)
			warnLine("this worktree does not record what it was cut from; using %s (the resolution's first parent)", short7(w.base))
		}
	}
	if err := w.changedAgainst(own.branch); err != nil {
		errLine("%v", err)
		return
	}
	if len(w.changedFiles) == 0 {
		okLine("the resolution changes nothing against %s — nothing to write", short7(w.base))
		return
	}
	n, conflicts, err := w.integrate(context.Background(), env, "your resolution", refWord(own.session), "merge")
	switch {
	case err != nil:
		errLine("%v", err)
	case conflicts != "":
		// Honest: something wrote those files while the human was resolving, and
		// the rival really did collide with the resolution.
		warnLine("the resolution did not apply cleanly either:")
		fmt.Println(conflicts)
	case n == 0:
		okLine("already in your working tree — nothing to write")
	default:
		okLine("wrote the resolution into your working tree: %s", plural(n, "file", "files"))
		// The interruption or the conflict that sent the human here was journalled
		// against the SOURCE branch, not against the merge branch the resolution
		// lives on, so integrate's own drop cannot reach it. The resolution landing
		// is what makes that warning obsolete; left behind it repeats "your tree may
		// be half patched" for ever.
		dropJournal(cfg.stateDir(), own.source)
		hint("the merge worktree at %s and the branches are still there — `lca clean` removes them when you are done", own.dir)
	}
}

// canMergeHere runs the same capability probe the delegation path runs, and
// refuses with the same sentence.
//
// Both human routes called integrate directly with no gate, so on a git without
// `merge-tree --write-tree` the operator was handed git's raw usage text as
// "merging lca/merge/… into your tree failed: usage: git merge-tree …" instead
// of being told their git is too old. The design promises this is detected up
// front and never discovered as a failure.
func canMergeHere(top string) bool {
	ok, ver := (&Orchestrator{}).mergeTree3Way(top)
	if !ok {
		errLine("your git is %s; merging a delegation onto a branch needs 2.38 or newer", firstNonEmpty(ver, "too old"))
		hint("`git merge <branch>` in a scratch worktree of your own does the same job by hand")
	}
	return ok
}

// resolveBranchName accepts the exact branch, or a unique suffix of one, because
// the names are long and the one in a conflict message is long enough to mistype.
func resolveBranchName(top, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// The exact-ref shortcut only for a name under lca/. Without that test
	// `lca clean --branches main` resolved to `main`, entered the want set, and
	// printed "nothing to clean" — harmless only by accident, because the deletion
	// loop reads refs/heads/lca/ alone. The cost was that a wrong or mistyped name
	// looked like it had been understood, in the one place §2.8 gives the operator
	// an override.
	if strings.HasPrefix(name, "lca/") {
		if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/"+name); code == 0 {
			return name
		}
	}
	var hits []string
	for _, b := range listBranches(top) {
		if strings.HasSuffix(b.name, name) || strings.Contains(b.name, name) {
			hits = append(hits, b.name)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

// splitBranch recovers the session and task ids from lca/<role>/<sid>-<task>.
// The task id is the last dash-separated word (t3) and the session id is
// everything before it, dashes included (20260930-141233-4412).
//
// It is the exact inverse of branchFor plus freeBranch's collision suffix, which
// is why that suffix is `.2` and not `-2`: with a dash the session id came back
// one component short and everything keyed on it looked at the wrong session.
// TestBranchNamesAreRefSafeAndCollisionFree round-trips the pair.
func splitBranch(branch string) (sid, task string) {
	rest := branch
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		rest = branch[i+1:]
	}
	if i := strings.LastIndex(rest, "-"); i > 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, "x"
}

// mergeWorktreesOf lists the open resolution worktrees, optionally narrowed to
// the one resolving a named branch.
// An EXACT match wins over a substring one. Without that rule
// `lca/merge/<sid>-t1` tied with `lca/merge/<sid>-t1-2`, because the first is a
// prefix of the second — so the name the disambiguation printed could not select
// the row it named. The worktree directory is accepted too, since it is the one
// key that is unique by construction.
func mergeWorktreesOf(stateDir, name string) []owner {
	var loose, exact []owner
	for _, o := range allOwners(stateDir) {
		if o.role != "merge" {
			continue
		}
		if _, err := os.Stat(o.dir); err != nil {
			continue
		}
		switch {
		case name == "":
		case o.branch == name || o.source == name || o.dir == name:
			exact = append(exact, o)
			continue
		case strings.Contains(o.source, name) || strings.Contains(o.branch, name):
		default:
			continue
		}
		loose = append(loose, o)
	}
	if len(exact) > 0 {
		return exact
	}
	return loose
}

// allOwners reads every .owner file beside the worktree directories. The file
// sits NEXT TO its worktree and not inside it, so a subagent's own `add -A`
// cannot sweep it into a diff.
func allOwners(stateDir string) []owner {
	base := filepath.Join(stateDir, "worktrees")
	ents, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []owner
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".owner") {
			continue
		}
		dir := filepath.Join(base, strings.TrimSuffix(e.Name(), ".owner"))
		out = append(out, readOwner(dir))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}

// orphanWorktrees are the worktree directories whose owning process is gone.
// A directory whose owner cannot be established at all is NOT one: removing a
// live worktree another lca is working in is worse than leaking a directory.
//
// A RESOLUTION worktree is never one on the pid test alone, whatever its pid
// says. `lca merge <branch>` records the pid of a CLI process that exits the
// moment it finishes printing, so the worktree the human was just told to go and
// edit is an "orphan" within milliseconds — and the very next `lca clean`, the
// no-flag operation documented as the safe one, would force-remove their
// half-finished resolution and the markers, the three stages and `git merge
// --abort` with it. The REPL path is the same one turn later: the human is handed
// the worktree, the session exits, they come back next morning, which is the
// premise of the whole mode. §2.6 says the merge worktree stays. It belongs to a
// person, not to a process, so it goes only when it is both old AND clean —
// nothing uncommitted and no merge in progress — and `--force` is not used on it.
func orphanWorktrees(stateDir string) []owner {
	base := filepath.Join(stateDir, "worktrees")
	ents, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []owner
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		o := readOwner(filepath.Join(base, e.Name()))
		switch {
		case o.pid <= 0 || o.pid == os.Getpid() || pidAlive(o.pid):
		case o.role == "merge" && !spentMergeWorktree(o):
		default:
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}

// spentMergeWorktree says a resolution worktree is finished with: older than the
// day a branch gets, with nothing uncommitted in it and no merge in progress.
// Unrecorded age counts as young, because the alternative is deleting work on
// the strength of a missing line.
func spentMergeWorktree(o owner) bool {
	st, _, code := gitRun(o.dir, nil, nil, "status", "--porcelain")
	if code != 0 {
		// Not a worktree any more: its admin entry is gone, or the directory was
		// emptied. There is no resolution left in there to protect, and the age test
		// below would otherwise leak a directory nothing can ever reclaim.
		return true
	}
	if o.started.IsZero() || time.Since(o.started) < keptFor {
		return false
	}
	if strings.TrimSpace(st) != "" {
		return false
	}
	_, _, code = gitRun(o.dir, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD")
	return code != 0
}

type staleLease struct {
	path string
	held string
	pid  int
}

// staleLeases are lock files whose holder is gone. They are not dangerous — the
// next writer takes one over with a warning, which is why a SIGKILL cannot make
// a file permanently unwritable — so they are reported quietly and swept.
func staleLeases(lockDir string) []staleLease {
	ents, err := os.ReadDir(lockDir)
	if err != nil {
		return nil
	}
	var out []staleLease
	for _, e := range ents {
		// The repository lock is skipped by NAME as well as by pid: the sweep runs
		// while cleanRepo is holding it, and reclaiming the lock you are standing on
		// is not a tidy-up.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") || e.Name() == gitLockName {
			continue
		}
		p := filepath.Join(lockDir, e.Name())
		pid, held, _, _ := leaseOwner(p)
		if pid > 0 && pid != os.Getpid() && !pidAlive(pid) {
			out = append(out, staleLease{path: p, held: held, pid: pid})
		}
	}
	return out
}

func short7(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// ── lca clean ───────────────────────────────────────────────────────────────

func cleanUsage() {
	fmt.Fprintln(os.Stderr, strings.Join([]string{
		"usage: lca clean [--branches] [--dry-run] [<branch>…]",
		"",
		"  with no flags   remove the worktrees of processes that are gone, sweep",
		"                  stale write leases, temporary indexes, old check output",
		"                  and finished integration journals. No branch is touched,",
		"                  and a merge",
		"                  worktree you were handed is left alone until it is a day",
		"                  old, clean and not mid-merge.",
		"  --branches      also delete the lca/* branches that are integrated and",
		"                  more than a day old, counted from when they landed. Only a",
		"                  branch lca recorded creating ever goes. An unintegrated one",
		"                  is deleted only when you name it.",
		"  --dry-run       print what would happen and do none of it.",
	}, "\n"))
}

func runClean(cfg Config, args []string) int {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = cleanUsage
	branches := fs.Bool("branches", false, "also delete integrated lca/* branches older than a day")
	dry := fs.Bool("dry-run", false, "print what would happen, change nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	named := fs.Args()
	if len(named) > 0 && !*branches {
		errLine("naming a branch only makes sense with --branches")
		cleanUsage()
		return 2
	}
	top, err := repoTop(cfg.Root)
	if err != nil {
		errLine("%v", err)
		return 1
	}
	return cleanRepo(top, cfg, *branches, *dry, named)
}

// cleanRepo is `lca clean`'s body, in the order §2.8 forces: worktrees before
// branches, because git refuses to delete a branch a worktree holds, and the
// bookkeeping refs last, because they are what keeps a surviving branch's merge
// base reachable from `git gc`.
func cleanRepo(top string, cfg Config, branches, dry bool, named []string) int {
	stateDir := cfg.stateDir()
	lockDir := leaseDir(top, stateDir)
	did := 0
	say := func(format string, a ...any) {
		did++
		if dry {
			fmt.Println("  " + faint("would "+format, a...))
			return
		}
		okLine(format, a...)
	}

	// Reported BEFORE anything is removed: an interrupted integration may have
	// left the tree half patched, and the operator deciding what to do about that
	// should not have to read it after a tidy-up scrolled past.
	for _, j := range readJournals(stateDir) {
		warnLine("%s", interruptedText(j))
	}

	// 1. worktrees whose owner is gone. The lock is held across the whole of it:
	// another lca adding a worktree in the middle of our prune is exactly the race
	// that leaves a live worktree without an admin entry.
	unlock, lerr := lockGit(lockDir)
	if lerr != nil {
		warnLine("could not lock this repository's git bookkeeping (%v) — skipping the worktree pass", lerr)
	} else {
		defer unlock()
		for _, o := range mergeWorktreesOf(stateDir, "") {
			if o.pid > 0 && o.pid != os.Getpid() && pidAlive(o.pid) {
				continue
			}
			if !spentMergeWorktree(o) {
				fmt.Println("  " + faint("the merge worktree %s is a person's, not a process's — finish it with `lca merge --finish %s`, or remove it yourself", o.dir, firstNonEmpty(o.branch, o.dir)))
			}
		}
		for _, o := range orphanWorktrees(stateDir) {
			say("remove the worktree %s left by pid %d", o.dir, o.pid)
			if dry {
				continue
			}
			// No --force on a resolution worktree: git's own refusal is the last
			// safety net under a race where somebody started editing in it between
			// spentMergeWorktree and here, and an agent worktree has nothing worth
			// that protection.
			args := []string{"worktree", "remove", "--force", o.dir}
			if o.role == "merge" {
				args = []string{"worktree", "remove", o.dir}
			}
			if _, err := gitCmd(top, nil, nil, args...); err != nil {
				if o.role == "merge" {
					errLine("could not remove the merge worktree %s: %v", o.dir, err)
					continue
				}
				os.RemoveAll(o.dir)
			}
			os.Remove(o.dir + ".owner")
		}
		// 2. --expire and never bare: see this file's header.
		if !dry {
			gitCmd(top, nil, nil, "worktree", "prune", "--expire=1.hour.ago")
		}
	}

	// 3. branches, only when asked. A branch a worktree still holds is skipped
	// rather than forced: git refuses it anyway, and the right fix is to let the
	// session finish.
	if branches {
		want := map[string]bool{}
		for _, n := range named {
			if b := resolveBranchName(top, n); b != "" {
				want[b] = true
			} else {
				errLine("%s is not a branch lca made here", n)
			}
		}
		for _, b := range listBranches(top) {
			switch {
			case b.worktree != "":
				fmt.Println("  " + faint("%s is checked out in %s — git will not delete it, and neither will we", b.name, b.worktree))
				continue
			case !b.ours:
				// Never-list item 4: only a branch lca created. The name is not proof and
				// neither is the ancestry — a branch a human made under lca/ satisfies
				// both — so a branch with no authorship record is left alone even when
				// the operator names it, and the operator is told why rather than left
				// wondering.
				if want[b.name] {
					errLine("%s carries no record that lca created it, so lca will not delete it — `git branch -D %s` is yours to run", b.name, b.name)
				}
				continue
			case want[b.name]:
				// Named explicitly: the operator overrides both the ancestry test and
				// the day, and nothing else can.
			case !b.integrated:
				continue
			case time.Since(b.when) < keptFor:
				continue
			}
			say("delete %s (%s)", b.name, map[bool]string{true: "integrated", false: "named by you, NOT integrated"}[b.integrated])
			if dry {
				continue
			}
			// -D and not -d: `git branch -d` consults HEAD and the upstream only, so
			// it refuses a branch that is merged into refs/lca/integrated/* and
			// nothing else. The ancestry test above is ours to make, and we made it.
			if _, errs, code := gitRun(top, nil, nil, "branch", "-D", b.name); code != 0 {
				errLine("could not delete %s: %s", b.name, strings.TrimSpace(lastLines(errs, 2, 200)))
			}
		}
	}

	// 4. the bookkeeping refs of sessions with no branch left. Earlier than this
	// they keep a surviving branch's merge base alive; later than this they stop
	// `git gc` from ever reclaiming the snapshot trees.
	live, integrated := map[string]bool{}, map[string]bool{}
	for _, b := range listBranches(top) {
		sid, _ := splitBranch(b.name)
		live[refWord(sid)] = true
		integrated[b.name] = b.integrated
	}
	for _, ns := range []string{"refs/lca/yours/", "refs/lca/integrated/", "refs/lca/incoming/"} {
		for _, ref := range eachRef(top, ns) {
			if live[strings.TrimPrefix(ref, ns)] {
				continue
			}
			say("drop the bookkeeping ref %s", ref)
			if !dry {
				gitCmd(top, nil, nil, "update-ref", "-d", ref)
			}
		}
	}
	// The rejected-attempt refs are keyed <sid>-<task>, so they are matched on
	// the session component alone: an attempt is worth keeping exactly as long as
	// some branch of that session is still there to read it against.
	for _, ref := range eachRef(top, "refs/lca/attempt/") {
		name := strings.TrimPrefix(ref, "refs/lca/attempt/")
		if i := strings.LastIndex(name, "-"); i > 0 && live[refWord(name[:i])] {
			continue
		}
		say("drop the rejected merge attempt %s", ref)
		if !dry {
			gitCmd(top, nil, nil, "update-ref", "-d", ref)
		}
	}
	// The authorship markers are keyed by BRANCH, not by session, so they are
	// matched against the branch and not against the sid map above. A marker that
	// outlived its branch would make a later branch of the same name deletable
	// without lca ever having created it.
	for _, ref := range eachRef(top, "refs/lca/made/") {
		branch := strings.TrimPrefix(ref, "refs/lca/made/")
		if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch); code == 0 {
			continue
		}
		say("drop the authorship marker %s", ref)
		if !dry {
			gitCmd(top, nil, nil, "update-ref", "-d", ref)
		}
	}

	// 5. the small litter. Each of these is the signature of a process that was
	// killed between two syscalls, and each is harmless where it lies — swept so
	// that a repository worked in for a month does not accumulate thousands.
	for _, l := range staleLeases(lockDir) {
		say("sweep the write lease on %s (pid %d is gone)", firstNonEmpty(l.held, filepath.Base(l.path)), l.pid)
		if dry {
			continue
		}
		// Swept by ACQUIRING it, never by removing a path on the strength of a read
		// taken earlier in this run. Between staleLeases' read and the removal,
		// process B's own takeover can have recreated that lock with its live pid
		// and started its temp+rename — and removing it then lets process C
		// succeed immediately on the same path, so two writers each believe they
		// hold the one lease. That is the silent overwrite Part 1 exists to prevent,
		// committed by the tidy-up that is documented as the safe operation. Taking
		// the lease waits a live holder out (or is refused at the 2 s bound and
		// skipped), and releasing it removes the file.
		if release, err := leaseAt(l.path, firstNonEmpty(l.held, "a stale write lease"), leaseStale); err == nil {
			release()
		}
	}
	// claimLease's temp file (killed between the create and the link) and the
	// renamed-aside file a takeover leaves if its unlink fails. Both are invisible
	// to the sweep above, deliberately: neither is named *.lock, so neither can
	// ever be mistaken for a lock somebody holds.
	locklitter, _ := filepath.Glob(filepath.Join(lockDir, ".lca-lock-*"))
	aside, _ := filepath.Glob(filepath.Join(lockDir, "*.lock.stale-*"))
	if g := append(locklitter, aside...); true {
		for _, p := range g {
			if info, e := os.Stat(p); e != nil || time.Since(info.ModTime()) < time.Hour {
				continue
			}
			say("sweep the leftover lock file %s", filepath.Base(p))
			if !dry {
				os.Remove(p)
			}
		}
	}
	if g, err := filepath.Glob(filepath.Join(os.TempDir(), "lca-index-*")); err == nil {
		for _, p := range g {
			if info, e := os.Stat(p); e != nil || time.Since(info.ModTime()) < time.Hour {
				continue // a snapshot in flight right now, in this or another lca
			}
			say("sweep the temporary index %s", filepath.Base(p))
			if !dry {
				os.Remove(p)
			}
		}
	}
	for _, p := range strayTemps(top) {
		say("sweep the half-written file %s", p)
		if !dry {
			os.Remove(p)
		}
	}
	// The check logs, which are the biggest thing lca writes: a stand prints
	// megabytes per attempt and every attempt of every run keeps one. They are
	// bounded on every start (pruneCheckLogs), but this is the command an operator
	// runs when the disk is full, and it did not know the directory existed — so
	// the one sweep somebody reaches for missed the one directory worth sweeping.
	// Said with its size, because "a few old logs" and "forty gigabytes" are
	// different decisions.
	if dir := filepath.Join(stateDir, "checks"); true {
		stale, bytes := checkLogsToCollect(dir, cfg.KeepSessions, "")
		if len(stale) > 0 {
			say("sweep %s of check output past the limits (%s) from %s",
				fmtBytes(bytes), plural(len(stale), "file", "files"), shortDir(dir))
			if !dry {
				for _, p := range stale {
					os.Remove(p)
				}
			}
		}
	}
	// A journal whose branch is gone, or whose branch is now in the tree, has
	// nothing left to tell anyone. One whose work is still outstanding STAYS: it
	// is the only record that the tree may be half patched.
	for _, j := range readJournals(stateDir) {
		_, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/"+j.Branch)
		switch {
		case code != 0:
			say("drop the integration journal of %s (the branch is gone)", j.Branch)
		case integrated[j.Branch]:
			// The other half of this comment's own promise, finally kept. Without it
			// the "your tree may be half patched" warning survived the documented
			// recovery and repeated verbatim for ever — and a warning that cannot be
			// cleared is the one the operator learns to ignore, which turns the single
			// honest thing the design says about a half-patched tree into noise.
			say("drop the integration journal of %s (its branch is in your working tree)", j.Branch)
		default:
			continue
		}
		if !dry {
			dropJournal(stateDir, j.Branch)
		}
	}

	if did == 0 {
		okLine("nothing to clean")
	} else if dry {
		hint("run it without --dry-run to do the %s above", plural(did, "thing", "things"))
	}
	return 0
}

func eachRef(top, ns string) []string {
	out, _, code := gitRun(top, nil, nil, "for-each-ref", "--format=%(refname)", ns)
	if code != 0 {
		return nil
	}
	var refs []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			refs = append(refs, l)
		}
	}
	return refs
}

// dropJournal removes the journal of one branch by reading the files rather
// than rebuilding its name: the name is derived from a session and a task id
// that this caller does not have, and matching on the recorded branch cannot
// drift from whatever writeJournal chose to call the file.
func dropJournal(stateDir, branch string) {
	ents, err := os.ReadDir(integrationsDir(stateDir))
	if err != nil {
		return
	}
	for _, e := range ents {
		p := filepath.Join(integrationsDir(stateDir), e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var j integration
		if json.Unmarshal(b, &j) == nil && j.Branch == branch {
			os.Remove(p)
		}
	}
}

// strayTemps finds the sibling temp files an atomic write leaves when the
// process dies between CreateTemp and Rename — and the member's `.lca-tmp`,
// when the project is also checked out locally.
//
// Narrow on purpose: the exact names those two writers use, at least an hour
// old, and never inside .git. Anything looser in a sweep that deletes files is
// how a tidy-up becomes the data loss it was written to prevent.
func strayTemps(top string) []string {
	var out []string
	filepath.WalkDir(top, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		n := d.Name()
		if !strings.HasPrefix(n, ".lca-tmp-") && !strings.HasSuffix(n, ".lca-tmp") {
			return nil
		}
		if info, e := d.Info(); e == nil && time.Since(info.ModTime()) > time.Hour {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// ── lca merge ───────────────────────────────────────────────────────────────

func mergeUsage() {
	fmt.Fprintln(os.Stderr, strings.Join([]string{
		"usage: lca merge <branch>        check the merge out in its own worktree, with markers",
		"       lca merge --finish [<branch>]",
		"                                 write what you resolved there into your working tree",
		"",
		"  /branches (in a session) or `git branch --list 'lca/*'` lists the branches.",
	}, "\n"))
}

// runMerge is the human's half of a conflict from the command line — the thing
// a conflict message tells the model to ask for, so it has to exist outside a
// REPL.
//
// It builds NO gateway client, no roles, no model: a merge is git and a person.
// That is also why there is no edit-rule gate on this path and none is faked —
// the operator typing the command is the approval, and a rule written to keep a
// MODEL out of a file was never about them.
func runMerge(cfg Config, args []string) int {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = mergeUsage
	finish := fs.Bool("finish", false, "integrate the resolution you committed in the merge worktree")
	bare, rest := splitLeadingName(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if bare == "" {
		bare = fs.Arg(0)
	}
	top, err := repoTop(cfg.Root)
	if err != nil {
		errLine("%v", err)
		return 1
	}
	if *finish {
		mergeFinish(top, cfg, integrateEnv{
			jailRoot: cfg.Root,
			lockDir:  leaseDir(top, cfg.stateDir()),
			stateDir: cfg.stateDir(),
			// forget is nil: there is no session here whose read records could
			// license a later overwrite of what we are about to write.
			forget: nil,
		}, bare, "lca merge")
		return 0
	}
	if bare == "" {
		mergeUsage()
		return 2
	}
	// mergeWorktree is an Orchestrator method because it needs the state
	// directory and the lock directory, which is all this one borrows.
	o := &Orchestrator{cfg: cfg}
	jl, jerr := NewJail(cfg.Root, nil, true)
	if jerr != nil {
		errLine("%v", jerr)
		return 1
	}
	o.jl = jl
	mergeStart(o, top, bare, "lca merge")
	return 0
}
