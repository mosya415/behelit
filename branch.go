package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// apply: branch — a delegation's work becomes a real branch in the project, and
// the branch is merged into the caller's WORKING TREE through the object
// database.
//
// Why a branch at all, when `apply: verified` already works: a delegation's
// worktree dies with the call, so a failed, rejected or conflicted change
// evaporates and nobody can look at what it tried. A branch survives, shows up
// in `git log --graph`, and carries one commit per verifier attempt.
//
// Why a 3-way merge in the object database instead of `git apply`: the caller's
// tree is usually DIRTY, and a text patch against a dirty tree fails as "the
// patch did not apply" whether the two sides genuinely collide or merely sit
// three lines apart. `git merge --no-ff` refuses outright ("your local changes
// would be overwritten"), and `git apply --3way` reads the INDEX, which a dirty
// tree does not match. `merge-tree --write-tree` is the only one of the three
// that answers the real question — and when it says conflict, it is a conflict,
// not a context mismatch.
//
// Why it is one team-wide switch and never the model's choice: a default that
// quietly changed how a verified diff lands would change the outcome of every
// existing workflow on upgrade, and a tool parameter would move the request
// prefix (the gateway's cache key). The operator decides, in one place, once.
//
// What is NEVER touched, and the reason each one is on the list:
//   - the caller's index and HEAD — every snapshot goes through a temporary
//     GIT_INDEX_FILE, so the staged/unstaged split of a half-staged file comes
//     back byte-identical;
//   - the caller's uncommitted work — it is never staged, committed, stashed or
//     reset; the snapshot commits exist only as merge SIDES, never as something
//     that gets merged as a whole;
//   - refs/heads, except a branch named lca/… that lca created itself;
//   - the user's hooks: -c core.hooksPath=/dev/null on worktree add, on every
//     commit and on the resolution merge, because a pre-commit hook otherwise
//     runs inside an agent's scratch copy and can block its commit;
//   - the caller's working tree when the merge conflicted. Conflict markers in
//     the human's own files are the one outcome worse than today's discard.

// integratorRole is the role name a team declares to opt into ONE supervised
// resolution attempt. Unnamed, a conflict goes straight to the human.
const integratorRole = "integrator"

// yoursRef / integratedRef are bookkeeping refs OUTSIDE refs/heads, so
// `git branch` never lists them while the work branches under lca/ are real
// branches and it does. They also keep the snapshot trees reachable, which is
// why `lca clean` deletes them last and only when no branch of that session is
// left: dropping them earlier would let `git gc` reclaim a tree a surviving
// branch still needs as its merge base.
func yoursRef(sid string) string      { return "refs/lca/yours/" + refWord(sid) }
func integratedRef(sid string) string { return "refs/lca/integrated/" + refWord(sid) }

// madeRef is lca's PROOF that it created a branch, written the moment
// `worktree add -b` succeeds and consulted before anything is deleted.
//
// The namespace is not proof. `lca/…` is a name anybody may use — a human's own
// `lca/my-spike` bookmark is an ordinary thing to make — and the ancestry test
// cannot tell them apart either: the integration commit's history reaches back
// through the snapshot to the caller's HEAD, so EVERY commit in the project's
// own history is an ancestor of it and answers "integrated". Measured: a branch
// the user made at an older commit on master satisfied the test exactly like the
// real work branch, was printed as integrated, and was old enough for the first
// `lca clean --branches` to delete it. Never-list item 4 says only a branch lca
// created, so authorship is recorded rather than inferred.
//
// Outside refs/heads, so `git branch` never lists it, and dropped with the
// session's other bookkeeping by `lca clean`.
func madeRef(branch string) string { return "refs/lca/made/" + branch }

// refWord keeps only what git-check-ref-format accepts in a single ref
// component, and never leaves it empty. Role names come from roles.yaml and
// session ids from the recorder, so in practice nothing is replaced — but a ref
// built from a string nobody validated is a branch that cannot be deleted.
func refWord(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	out = strings.ReplaceAll(out, "..", "-")
	if out == "" {
		return "x"
	}
	return strings.TrimSuffix(out, ".lock")
}

// branchFor names a delegation's branch lca/<role>/<session>-<task>, e.g.
// lca/coder/20260930-141233-4412-t3. The session id is the recorder's
// (<date>-<time>-<pid>) and the task id is the child's, which is also what the
// transcript, the trace and the HTML report use — so a branch found next morning
// traces back to the session that made it without a counter file to lock. Sorts
// chronologically under `git branch --list 'lca/*'`, and everything lca creates
// lives under lca/ or refs/lca/, so one glob finds all of it.
func branchFor(role, sid, task string) string {
	return "lca/" + refWord(role) + "/" + refWord(sid) + "-" + refWord(task)
}

// freeBranch is branchFor with the collision case: a reused pid on another day
// can produce a name that already exists, and `worktree add -b` would fail the
// whole delegation over it. Called under lockGit, so the probe and the create
// cannot be interleaved by another lca.
// The suffix is `.2` and not `-2`, because a dash is the task-id separator:
// splitBranch reads the last dash-separated word as the task, so
// `lca/coder/<sid>-t3-2` parsed as session `<sid>-t3` and task `2`, and then
// `/merge` looked up a refs/lca/yours/<sid>-t3 that has never existed and told
// the operator that session's snapshot had been cleaned up — a wrong diagnosis
// pointing at a snapshot that was sitting right there. `lca clean` filed it
// under the wrong session too. A dot is not a separator here, and
// git-check-ref-format accepts it.
func freeBranch(top, want string) string {
	for i := 1; i < 50; i++ {
		name := want
		if i > 1 {
			name = fmt.Sprintf("%s.%d", want, i)
		}
		if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/"+name); code != 0 {
			return name
		}
	}
	return fmt.Sprintf("%s.%d", want, time.Now().UnixNano())
}

// gitRun is gitCmd with the EXIT CODE kept. It exists for exactly two callers
// that cannot work without it:
//
//   - `merge-tree --write-tree` exits 1 on a conflict and STILL writes a tree,
//     printing the tree oid, then the conflicted stage lines, then its own
//     messages. Treating that as a plain failure throws away the three sides of
//     the conflict; treating stdout as a tree oid without looking at the code
//     produces `fatal: failed to stat '<oid>\n100644 …CONFLICT…': File name too
//     long`. Only the code tells them apart.
//   - the capability probe needs git's STDERR without the arguments mixed in:
//     gitCmd's error message contains the command line, so "does the usage
//     mention --write-tree" cannot be asked of it.
func gitRun(dir string, env []string, stdin []byte, args ...string) (stdout, stderr string, code int) {
	return gitRunIn(context.Background(), dir, env, stdin, args...)
}

// gitRunIn is gitRun with the caller's context, which is what lets the RUN's
// deadline reach a git invocation that talks to a network. It is gitCmdIn's
// treatment with gitRun's exit code kept, and the two halves of that treatment
// are both load-bearing here:
//
//   - the DEADLINE. `ls-remote` and `push` are the only two git operations in
//     this program that touch a remote, and both used to run with no deadline of
//     any kind. A forge host that accepts the TCP connection and then sends
//     nothing — a half-open firewall state, a hung gitlab-shell, a load balancer
//     that stopped forwarding — left git blocked in read(2) for ever, with the
//     ticket lock held and the ticket claimed, and `-timeout` could not reach it
//     because nothing passed a context in. Worse: `lca ticket` holds SIGTERM
//     through signal.NotifyContext, so cron's `timeout 3600 lca ticket …`
//     cancelled a context nobody was selecting on and the process survived every
//     TERM. Only SIGKILL ended it, and nothing alerted anybody. gitCmdTimeout is
//     the inner bound — the same ten minutes, and for the same reason its own
//     comment gives — so a caller with no deadline of its own is still bounded.
//   - NO CONTROLLING TERMINAL. GIT_TERMINAL_PROMPT governs git's own prompt and
//     nothing else: ssh's key-passphrase prompt and gpg's pinentry open /dev/tty
//     directly, so a run started from a tmux pane or a pty-ful CI runner could
//     sit for ever on `Enter passphrase for key …` printed to a screen nobody is
//     watching — and to neither of the two streams captured here, so the
//     transcript, the trace and the state file all show the run simply stopping
//     at `push` with no explanation. noPromptEnv() sets the three askpass
//     variables for the programs that read them and inProcessGroup's Setsid
//     removes the terminal outright, which is the half that holds against a
//     program that reads none of them.
func gitRunIn(ctx context.Context, dir string, env []string, stdin []byte, args ...string) (stdout, stderr string, code int) {
	ctx, cancel := context.WithTimeout(ctx, gitCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(noPromptEnv(), env...)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=lca", "GIT_AUTHOR_EMAIL=lca@localhost",
		"GIT_COMMITTER_NAME=lca", "GIT_COMMITTER_EMAIL=lca@localhost")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	inProcessGroup(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	switch {
	case err == nil:
		code = 0
	case cmd.ProcessState != nil:
		code = cmd.ProcessState.ExitCode()
	default:
		code = -1 // git did not start at all
	}
	// A killed child reports exit code -1, which every caller already reads as "no
	// answer from git" — the right classification for a timeout — but says nothing
	// about WHY, and the why is the whole value of the record. Appended to stderr
	// because that is the stream the callers that compose a message read.
	if cerr := ctx.Err(); cerr != nil {
		errb.WriteString(fmt.Sprintf("\ngit %s: %v after %s", strings.Join(args, " "), cerr, gitCmdTimeout))
		if code == 0 {
			code = -1
		}
	}
	return out.String(), errb.String(), code
}

// mergeTree3Way says whether this git can merge in the object database, and
// names its version for the message when it cannot.
//
// Probed by running `merge-tree --write-tree` with NO arguments INSIDE the
// repository: git then exits 129 and prints its usage to stderr, and the usage
// names the flag only on a git that has it. Gated on the usage STRING and never
// on the exit code — outside a repository the same command exits 128 with "not a
// git repository", which is not an answer about the flag — and cached, because
// every integration would otherwise pay for a subprocess to learn something
// about the binary that cannot change while we run.
func (o *Orchestrator) mergeTree3Way(top string) (bool, string) {
	o.mt3Once.Do(func() {
		_, errs, _ := gitRun(top, nil, nil, "merge-tree", "--write-tree")
		o.mt3 = strings.Contains(errs, "--write-tree")
		v, _, _ := gitRun(top, nil, nil, "--version")
		o.mt3Ver = strings.TrimPrefix(strings.TrimSpace(v), "git version ")
	})
	return o.mt3, o.mt3Ver
}

// ── snapshots ───────────────────────────────────────────────────────────────

// excludePathspecs keeps the agents' own worktrees out of a snapshot when the
// state directory happens to live inside the project. Default LCA_DIR is
// $HOME/.lca so this is usually latent — but the README documents
// <root>/.lca/roles.yaml, and an operator who points LCA_DIR there gets
// `warning: adding embedded git repository` and a 160000 gitlink recorded in
// every snapshot: the subagent then inherits a submodule pointing at a directory
// that is about to be deleted. A bug fix, so it applies under every apply:
// policy, not only under branch.
func excludePathspecs(top, lcaDir string) []string {
	if lcaDir == "" || top == "" {
		return nil
	}
	wt, err := filepath.Abs(filepath.Join(lcaDir, "worktrees"))
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(top, wt)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	r := filepath.ToSlash(rel)
	return []string{"--", ".", ":(exclude)" + r, ":(exclude,glob)" + r + "/**"}
}

// snapshotOf commits top's CURRENT working tree — tracked changes, deletions and
// new non-ignored files — without touching the user's index or HEAD.
//
// ONE function, used by both the delegation snapshot and the "ours" side of
// every integration, deliberately: the whole safety argument rests on the two
// being the same sequence, and two copies of it would drift. readFrom seeds the
// temporary index (empty on an unborn HEAD, which then yields a root commit) and
// parent is the commit the snapshot hangs off — HEAD for a delegation, the
// previous integration for an integration, and irrelevant to correctness either
// way because every merge passes --merge-base explicitly.
func snapshotOf(top, lcaDir, msg, readFrom, parent string) (string, error) {
	idx, err := os.CreateTemp("", "lca-index-*")
	if err != nil {
		return "", err
	}
	idx.Close()
	os.Remove(idx.Name()) // git wants to create it
	defer os.Remove(idx.Name())
	env := []string{"GIT_INDEX_FILE=" + idx.Name()}
	if readFrom != "" {
		if _, err := gitCmd(top, env, nil, "read-tree", readFrom); err != nil {
			return "", err
		}
	}
	if _, err := gitCmd(top, env, nil, append([]string{"add", "-A"}, excludePathspecs(top, lcaDir)...)...); err != nil {
		return "", err
	}
	tree, err := gitCmd(top, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", strings.TrimSpace(tree), "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit, err := gitCmd(top, nil, nil, args...)
	return strings.TrimSpace(commit), err
}

// ── the work commits ────────────────────────────────────────────────────────

// commitWork commits the worktree's current state onto the session's branch. A
// no-op when the session has no branch or the tree is clean.
//
// The ENGINE commits, not the model: no new tool, no change to the request
// prefix, and a role cannot forget. Called once per verifier attempt from
// RunVerifiedAll, so a failed delegation's branch still shows what it tried —
// nothing is squashed, because "what it tried" is the value a kept branch has.
//
// -c core.hooksPath=/dev/null is mandatory and not decoration: `worktree add`
// passes it once, a later `git commit` in that worktree does NOT inherit it, and
// a user's pre-commit hook then runs inside the agent's scratch copy. Measured:
// a hook exiting 1 blocked the commit outright. commit.gpgsign=false is on the
// list for the same reason and was missing: a project that signs its commits
// makes this one exit 128 ("gpg failed to sign the data") inside a worktree no
// human will ever push from, and the agent cannot be asked for a passphrase.
//
// The pathspec is the same confinement diff() uses, so a subagent's changes
// above its jail root (make -C .., say) never reach a branch either.
func (s *Session) commitWork(ctx context.Context, w *worktree, attempt int) error {
	if s.branch == "" || w == nil {
		return nil
	}
	msg := fmt.Sprintf("%s: attempt %d", s.agent.Name, attempt)
	if t := firstLine(lastAssistantText(s)); strings.TrimSpace(t) != "" {
		msg += " — " + truncate(strings.TrimSpace(t), 72)
	}
	if w.rem != nil {
		return w.commitOn(ctx, msg)
	}
	// ctx, not Background: the commit is the one local git call that happens once
	// per verifier attempt, and the run's deadline has to be able to end it. The
	// signature already accepted a ctx and forwarded it only on the remote path.
	st, err := gitCmdIn(ctx, w.dir, nil, nil, append([]string{"status", "--porcelain"}, w.pathspec()...)...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(st) == "" {
		return nil
	}
	// `add -A` in a worktree mid-merge does not so much stage a resolution as
	// RESOLVE the index with whatever text is in the file. Writing the file IS how
	// a resolution is made, so unmerged index entries are perfectly normal here —
	// what must not be committed is marker TEXT, which is what a half-done
	// resolution leaves. Committing that puts `<<<<<<< HEAD` on a branch, and the
	// only thing then standing between it and the user's own files is a check_cmd
	// that happens to compile the conflicted path. Refusing leaves the three
	// stages and an abortable MERGE_HEAD where the human can still use them.
	if bad := markerFiles(w.dir); len(bad) != 0 {
		return fmt.Errorf("the merge in %s is not resolved — %s still hold conflict markers, and committing that text would put it on %s",
			w.dir, strings.Join(bad, ", "), w.branch)
	}
	if _, err := gitCmdIn(ctx, w.dir, nil, nil, append([]string{"add", "-A"}, w.pathspec()...)...); err != nil {
		return err
	}
	_, err = gitCmdIn(ctx, w.dir, nil, nil, "-c", "core.hooksPath="+os.DevNull, "-c", "commit.gpgsign=false", "commit", "-m", msg)
	return err
}

// commitOn is commitWork's member arm: the same three commands in one script,
// because three ssh round trips per verifier attempt is three chances for the
// link to drop in the middle of a commit.
func (w *worktree) commitOn(ctx context.Context, msg string) error {
	script := "set -e\ncd " + shellQuote(w.dir) + "\n" +
		"[ -n \"$(git status --porcelain" + w.pathspecSh() + ")\" ] || exit 0\n" +
		// The member's half of markerFiles. `if` and a captured variable rather than
		// an && chain, because under `set -e` a failing grep in a chain kills the
		// script instead of meaning "this file is clean".
		"bad=$(git diff --name-only --diff-filter=U | while read -r p; do if grep -q '^<<<<<<< ' \"$p\" 2>/dev/null && grep -q '^>>>>>>> ' \"$p\" 2>/dev/null; then echo \"$p\"; fi; done)\n" +
		"[ -z \"$bad\" ] || { echo \"the merge is not resolved: $bad still hold conflict markers\" >&2; exit 1; }\n" +
		"git add -A" + w.pathspecSh() + "\n" +
		"git -c core.hooksPath=/dev/null -c commit.gpgsign=false commit -m " + shellQuote(msg) + " >/dev/null\n"
	out, errs, exit := w.rem.plumb(ctx, script, nil, 10*time.Minute)
	if exit != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 4, 400)))
	}
	return nil
}

// ── integration ─────────────────────────────────────────────────────────────

// integration is the journal written between `apply --check` and `apply`. It is
// NOT a replay mechanism: re-running the ordinary integration IS the recovery,
// because it re-snapshots and re-merges and an already-integrated branch
// produces a 0-byte patch. The file exists so that something can TELL the
// operator an integration was interrupted — between the kill and the re-run the
// tree really is half patched, nothing can fix that, and being told is the whole
// improvement.
type integration struct {
	Branch  string `json:"branch"`
	Pid     int    `json:"pid"`
	Started string `json:"started"`
}

func integrationsDir(stateDir string) string { return filepath.Join(stateDir, "integrations") }

func writeJournal(stateDir, sid, task, branch string) string {
	dir := integrationsDir(stateDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	p := filepath.Join(dir, refWord(sid)+"-"+refWord(task)+".json")
	b, _ := json.Marshal(integration{Branch: branch, Pid: os.Getpid(), Started: time.Now().Format(time.RFC3339)})
	if os.WriteFile(p, b, 0o600) != nil {
		return ""
	}
	return p
}

// readJournals lists the interrupted integrations: a journal whose pid is gone.
// A journal whose pid is alive is an integration in flight in another process,
// and reporting that as damage would make every parallel session alarming.
func readJournals(stateDir string) []integration {
	ents, err := os.ReadDir(integrationsDir(stateDir))
	if err != nil {
		return nil
	}
	var out []integration
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(integrationsDir(stateDir), e.Name()))
		if err != nil {
			continue
		}
		var j integration
		if json.Unmarshal(b, &j) != nil || j.Branch == "" {
			continue
		}
		if j.Pid > 0 && j.Pid != os.Getpid() && pidAlive(j.Pid) {
			continue
		}
		out = append(out, j)
	}
	return out
}

// interruptedText is what /branches and `lca clean` say about a journal nobody
// finished. It names the recovery and, before it, the one command that lets the
// operator look first: `git diff` on a half-patched tree is readable, and a
// recovery run on a tree the operator has not seen is a second surprise.
func interruptedText(j integration) string {
	return fmt.Sprintf("the integration of %s was interrupted (pid %d is gone) — your tree may be half patched; run `lca merge %s` to finish it, or `git diff` first to look.",
		j.Branch, j.Pid, j.Branch)
}

// mergeStage holds the three sides of one conflicted path: the common base, the
// caller's working tree, and the role's branch. "" means the stage is absent,
// which is itself the answer for modify/delete (no stage 3) and add/add (no
// stage 1).
type mergeStage struct{ base, ours, theirs string }

type mergeConflict struct {
	paths  []string
	stages map[string]mergeStage
	notes  []string // merge-tree's own Auto-merging / CONFLICT lines
}

var reStageLine = regexp.MustCompile(`^[0-7]{6} ([0-9a-f]{7,64}) ([123])\t(.+)$`)

// parseMergeTree reads merge-tree's rc-1 output: the tree oid on line 1, then
// `100644 <oid> 1|2|3\t<path>` for every conflicted path, a blank line, then its
// own messages. Parsed by SHAPE rather than by position, because the
// informational section's wording is not a stable interface and a stage line is.
func parseMergeTree(out string) mergeConflict {
	mc := mergeConflict{stages: map[string]mergeStage{}}
	for i, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if i == 0 {
			continue // the tree oid: a conflicted tree is never applied, so it is not used
		}
		if m := reStageLine.FindStringSubmatch(line); m != nil {
			p := m[3]
			st, seen := mc.stages[p]
			if !seen {
				mc.paths = append(mc.paths, p)
			}
			switch m[2] {
			case "1":
				st.base = m[1]
			case "2":
				st.ours = m[1]
			case "3":
				st.theirs = m[1]
			}
			mc.stages[p] = st
			continue
		}
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "CONFLICT") || strings.HasPrefix(t, "Auto-merging") {
			mc.notes = append(mc.notes, t)
		}
	}
	return mc
}

// conflictText is what the CALLING MODEL is told, in test_tail — which is
// already prose it reads, so no new key appears in the result JSON and status
// stays the existing "conflict".
//
// It names the three sides as `git cat-file -p` commands because the model has
// run_command and the task context, and a small region is something it can
// resolve itself. That is the first of three routes; the other two are an
// integrator role, if the team declared one, and the human.
func (mc mergeConflict) text(branch, role string, more string) string {
	var b strings.Builder
	what := "the same region of " + strings.Join(mc.paths, ", ")
	if len(mc.paths) == 0 {
		what = "the same files"
	}
	fmt.Fprintf(&b, "%s did not merge into your tree: you and %s changed %s.\nNOTHING was written to your tree; the branch is kept.\n", branch, role, what)
	for _, p := range mc.paths {
		st := mc.stages[p]
		fmt.Fprintf(&b, "\nThe sides of %s are in the object store:\n", p)
		for _, s := range []struct{ oid, what string }{
			{st.base, "the common base"},
			{st.ours, "your working tree"},
			{st.theirs, role + "'s"},
		} {
			if s.oid == "" {
				fmt.Fprintf(&b, "  (no %s side — see the CONFLICT line below)\n", s.what)
				continue
			}
			fmt.Fprintf(&b, "  run_command: git cat-file -p %s    %s\n", s.oid, s.what)
		}
	}
	if len(mc.notes) > 0 {
		fmt.Fprintf(&b, "\ngit's own account:\n  %s\n", strings.Join(mc.notes, "\n  "))
	}
	if more != "" {
		fmt.Fprintf(&b, "\n%s\n", more)
	}
	fmt.Fprintf(&b, "\nResolve it yourself with edit and re-delegate, or tell the user to run\n  lca merge %s\nwhich checks the merge out in its own worktree with conflict markers.", branch)
	return b.String()
}

// ── the wave counter ────────────────────────────────────────────────────────

// waveEnter / waveLeave / waveOrdinal number the integrations of one burst of
// parallel delegations, so a result can say "integrated 2nd of 3".
//
// Results are deliberately NOT buffered into reply order: that would make a
// finished delegation wait on a slower sibling, and the only thing the wait
// would buy is a wider window for the human to touch the file. The ordinal is
// therefore all the caller gets — and because a genuine conflict is refused
// rather than resolved behind its back, the order only decides which side is
// "ours", never whether something is silently lost.
//
// The wave restarts once no delegation is in flight, so a later pair is
// "1st of 2" again rather than "7th of 8".
func (o *Orchestrator) waveEnter() {
	o.waveMu.Lock()
	defer o.waveMu.Unlock()
	if o.waveInflight == 0 {
		o.waveStarted, o.waveDone = 0, 0
	}
	o.waveInflight++
	o.waveStarted++
}

func (o *Orchestrator) waveLeave() {
	o.waveMu.Lock()
	defer o.waveMu.Unlock()
	if o.waveInflight > 0 {
		o.waveInflight--
	}
}

// waveOrdinal is called at integration time, so "of N" counts the delegations
// that had actually started by then. A sibling that starts later makes the next
// line say "of 3" where this one said "of 2"; that is honest about what was
// known when, and no number here is load-bearing for anything but reading.
func (o *Orchestrator) waveOrdinal() string {
	o.waveMu.Lock()
	defer o.waveMu.Unlock()
	o.waveDone++
	if o.waveStarted < 2 {
		return ""
	}
	return fmt.Sprintf("integrated %s of %d", ordinal(o.waveDone), o.waveStarted)
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// integratorFor is the one opt-in in Part 2 beyond the switch itself: a team
// that names an `integrator` role gets ONE supervised resolution attempt, and a
// team that does not gets the human. Looked up by name and not configurable per
// call, for the same reason the switch is team-wide — who may rewrite a merge
// nobody wrote is the operator's decision, never the model's.
func (o *Orchestrator) integratorFor() *Agent {
	if a := o.agents[integratorRole]; a != nil && a.IsRole {
		return a
	}
	return nil
}

// mergeBranch is the whole of apply: branch seen from runDelegateTool — the
// capability-gated 3-way merge, the integrator's single attempt, and the words
// the caller is told. It returns (applied, status, tail) and never writes
// anything the per-file edit rules did not allow.
func (o *Orchestrator) mergeBranch(tc *ToolCtx, child *Session, w *worktree, sid, task string, checks []string, status, tail string) (bool, string, string) {
	s, role := tc.S, child.agent.Name
	add := func(text string) string { return strings.TrimSpace(tail + "\n\n" + text) }

	// What the BRANCH holds, not what the worktree holds now. The two differ when
	// a check_cmd wrote after the last commit (its output is not part of the
	// merge) or when it reverted something the model did (the branch still
	// carries it). The merged set is the honest one for the leases, for the
	// "applied N files" line and, above all, for the permission gate.
	gated := map[string]bool{}
	for _, f := range w.changedFiles {
		gated[f] = true
	}
	if err := w.changedAgainst(w.branch); err != nil {
		return false, "error", add("reading what " + w.branch + " changed failed: " + err.Error())
	}
	// Paths the earlier prompt did not cover meet the rules now. Normally none,
	// so normally there is no second question.
	var extra []string
	for _, f := range w.changedFiles {
		if !gated[f] {
			extra = append(extra, f)
		}
	}
	if len(extra) > 0 {
		switch act, pattern := s.editGate(extra); act {
		case Deny:
			return false, "not_applied", add(fmt.Sprintf("error: %s also changes %s, which a permission rule denies editing — nothing was written, and the branch is kept.", w.branch, pattern))
		case Ask:
			msg, ok := tc.Ask("edit", pattern, "MERGE "+w.branch, "   also changes, outside the reviewed diff:\n     "+strings.Join(extra, "\n     "))
			if !ok {
				return false, "not_applied", add(msg)
			}
		}
	}

	n, conflicts, err := w.integrate(tc.Ctx, s.integrateEnv(), role, sid, task)
	ord := o.waveOrdinal()
	switch {
	case err != nil:
		// An error is not a conflict: nothing was merged and nothing is claimed.
		// The branch is kept either way, which is the point of the mode.
		return false, "error", add(fmt.Sprintf("merging %s into your tree failed: %v\nThe branch is kept.", w.branch, err))

	case conflicts != "":
		ir := o.integratorFor()
		if ir == nil {
			s.tellTheHuman(w.branch, "")
			return false, "conflict", add(conflicts)
		}
		// ONE attempt, then it is the human's. A third agent resolving a merge it
		// did not write, twice, is the silent-overwrite failure wearing a hat.
		_, text, ok := o.resolveConflict(tc, w, ir, sid, task, conflicts, checks)
		if !ok {
			s.tellTheHuman(w.branch, mergeWorktreeIn(text))
			return false, "conflict", add(text)
		}
		return true, status, add(joinNonEmpty("\n", text, ord))

	case n == 0:
		// Idempotent by construction: the merge of an already-integrated branch is
		// a 0-byte patch. This is also what makes recovering an interrupted
		// integration a matter of running the same thing again.
		return true, status, add(w.branch + " is already in your working tree — the merge produced nothing to write.")
	}
	return true, status, add(joinNonEmpty("\n", fmt.Sprintf("merged %s into your working tree: %s, no conflict. The branch is kept — `/branches` lists it.",
		w.branch, plural(n, "file written", "files written")), ord))
}

// tellTheHuman puts the hand-over on the OPERATOR's screen, not only in the
// tool result.
//
// Everything a conflict produces — the merge worktree, "THE RUN STOPS HERE",
// the `lca merge` pointer — used to live inside the delegate result, i.e. inside
// the model's prompt, and whether the human ever saw it depended on how the lead
// chose to summarise. Measured on a real integrator failure: the lead summarised
// the whole hand-over to the single word "Reported.", so the human learned
// neither that the run had stopped nor where the two resolution artefacts were.
// The design's own escalation order (the model, then the human) depends on the
// human being told something when the model is out of attempts.
func (s *Session) tellTheHuman(branch, worktree string) {
	lines := []string{
		"THE RUN STOPS HERE: " + branch + " conflicts with your working tree.",
		"nothing was written, and the branch is kept",
	}
	if worktree != "" {
		// `git merge --abort` only where there is a merge to abort — the same test
		// the model-facing hand-over makes. Naming a command that exits 128 is the
		// defect this whole line exists to correct, and it would be no better for
		// being on screen.
		hint := "git status"
		if _, _, code := gitRun(worktree, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD"); code == 0 {
			hint = "git status, git merge --abort"
		}
		lines = append(lines, "the merge is checked out at "+worktree+"   ("+hint+")")
	}
	lines = append(lines, "resolve it with: lca merge "+branch, "/branches lists what is outstanding")
	s.view.Error(strings.Join(lines, "\n"))
}

// mergeWorktreeIn digs the resolution worktree's path out of the hand-over text
// rather than threading it back through resolveConflict's three return values.
// The text is built one frame down and is the only place that knows whether a
// worktree was made at all.
func mergeWorktreeIn(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if _, after, ok := strings.Cut(line, "the merge worktree  "); ok {
			return strings.TrimSpace(firstField(after))
		}
	}
	return ""
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func joinNonEmpty(sep string, parts ...string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, sep)
}

// integrateEnv is everything an integration needs from whoever asked for it,
// and nothing else. A delegation supplies a Session's; `lca merge` supplies the
// repository alone, because there is no model there whose read records could go
// stale and no rules to evaluate that the operator has not already decided by
// typing the command. Keeping it this narrow is also what lets the integration
// be tested against a real repository without a gateway, a model or a session.
type integrateEnv struct {
	jailRoot string // changedFiles are relative to this
	lockDir  string // where the file leases and the repository lock live
	stateDir string // the journal, and the snapshot's exclude pathspecs
	// forget drops a read record for one path, so the caller cannot later
	// overwrite the merge it just accepted from a file it never re-read. nil
	// outside a session.
	forget func(context.Context, string)
}

// fileLeaseMap turns a changedFiles list into leaseFiles' {abs: display} shape.
// One place, because the patch path and the branch path must lease the SAME lock
// files or a delegation in one mode cannot see a delegation in the other.
func fileLeaseMap(jailRoot string, files []string) map[string]string {
	leases := make(map[string]string, len(files))
	for _, f := range files {
		leases[filepath.Join(jailRoot, filepath.FromSlash(f))] = f
	}
	return leases
}

// applyPatch is applyTo under the same file leases §2.5 takes for the branch
// path, and it is what makes the README's guarantee true of "a delegation's
// integration" rather than only of the branch mode.
//
// `git apply --check` then `git apply` is two passes over the same files, and
// git has already decided the patch applies by the time it starts writing: a
// sibling subagent or the user's editor that writes between the two is lost
// inside git's own read→write with no message at all. A context collision IS
// caught (--check fails and nothing is written), so the common case was already
// honest — this closes the window the common case leaves.
//
// A caller on another machine is not leased, and that is said rather than
// quietly skipped: the files are over there and a lock file here would guard
// nothing.
func (s *Session) applyPatch(w *worktree, diff string) error {
	if w.caller != nil && !w.caller.IsLocal() {
		return w.applyTo(s.jail().Root, diff)
	}
	release, err := s.orch.leaseFiles(fileLeaseMap(s.jail().Root, w.changedFiles))
	if err != nil {
		return err
	}
	defer release()
	return w.applyTo(s.jail().Root, diff)
}

func (s *Session) integrateEnv() integrateEnv {
	return integrateEnv{
		jailRoot: s.jail().Root,
		lockDir:  s.orch.lockDir(),
		stateDir: s.orch.cfg.stateDir(),
		forget:   s.forgetRead,
	}
}

// integrate merges w.branch into the caller's WORKING TREE through the object
// database. It never touches HEAD, the index, or any ref under refs/heads.
//
// The sequence is the SAME whether the caller's tree is clean or dirty — on a
// clean caller the "ours" snapshot's tree simply equals HEAD's and the patch
// equals the branch's own diff. There is no second code path, which is the
// simplicity that matters most here.
//
// --merge-base is not optional. The "ours" commit hangs off HEAD or off the
// previous integration, so git's own merge-base computation would pick HEAD and
// put the caller's PRE-DELEGATION dirty lines on both sides of the merge.
// Passing the base explicitly makes the three sides exactly: where they started,
// what the caller has done since, what the role did — and makes the choice of
// parent irrelevant to correctness, which is why the parent can be chosen for
// the readability of `git log --graph` instead.
//
// Returns merged == 0 with no conflict and no error when the patch was empty:
// the branch is already in the tree. That is also what makes recovery from a
// killed `git apply` trivial — re-running this IS the recovery.
func (w *worktree) integrate(ctx context.Context, env integrateEnv, role, sid, task string) (merged int, conflicts string, err error) {
	top := firstNonEmpty(w.callerTop, w.top)
	lcaDir := env.stateDir

	// One lock per repository for the WHOLE integration, so two parallel results
	// land one at a time: each re-snapshots, so "the first one moved the tree" is
	// simply the second one's ours side. This is also the honest fix for the race
	// worktrees.mu only pretends to serialise across processes.
	unlock, err := lockGit(env.lockDir)
	if err != nil {
		return 0, "", err
	}
	defer unlock()

	// A lease on every path the branch touches, sorted, for the window between
	// `apply --check` and `apply`: a sibling delegation integrating the same file
	// in another process would otherwise land between the two.
	release, err := leaseFilesIn(env.lockDir, fileLeaseMap(env.jailRoot, w.changedFiles))
	if err != nil {
		return 0, "", err
	}
	defer release()

	// An EMPTY branch and an ALREADY-INTEGRATED branch both produce a 0-byte
	// patch, and reporting the first as the second is how a verified change gets
	// discarded while the caller is told it landed. They are distinguishable
	// before anything is computed: a branch with no commit of its own still points
	// at the snapshot it was cut from. Asked here rather than at the 0-file return
	// because it is a precondition, and because the answer does not depend on the
	// caller's tree.
	if tip, terr := gitCmd(top, nil, nil, "rev-parse", "--verify", w.branch); terr == nil && strings.TrimSpace(tip) == w.base {
		return 0, "", fmt.Errorf("%s holds no commit of its own — it still points at the snapshot it was cut from, so there is nothing to merge", w.branch)
	}

	head := ""
	if out, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", "HEAD"); code == 0 {
		head = strings.TrimSpace(out)
	}
	parent := head
	if _, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", integratedRef(sid)); code == 0 {
		parent = integratedRef(sid)
	}

	// Two passes at most. The retry exists for the one failure this design is
	// proudest of removing: today a rival writer between the patch and the apply
	// throws away a verified, reviewed delegation, even locally where every blob
	// is in the same object store. A fresh snapshot turns that into either a clean
	// merge or an honest conflict — never a loss.
	for attempt := 0; attempt < 2; attempt++ {
		d, err := snapshotOf(top, lcaDir, "lca: your working tree", head, parent)
		if err != nil {
			return 0, "", err
		}
		if _, err := gitCmd(top, nil, nil, "update-ref", yoursRef(sid), d); err != nil {
			return 0, "", err
		}
		// Ref names, not raw oids: a raw oid gives `<<<<<<< 75954fff…` as the marker
		// label in the resolution worktree, and a ref gives `<<<<<<< refs/lca/yours/…`.
		out, errs, code := gitRun(top, nil, nil, "merge-tree", "--write-tree", "--merge-base="+w.base, yoursRef(sid), w.branch)
		switch {
		case code == 0:
		case code == 1:
			return 0, parseMergeTree(out).text(w.branch, role, ""), nil
		default:
			return 0, "", fmt.Errorf("merging %s into your tree failed: %s", w.branch,
				strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 4, 400)))
		}
		m := firstLine(strings.TrimSpace(out))

		names, err := gitCmd(top, nil, nil, append([]string{"diff", "--name-only", d, m}, w.pathspec()...)...)
		if err != nil {
			return 0, "", err
		}
		var files []string
		for _, n := range strings.Split(strings.TrimSpace(names), "\n") {
			if n != "" {
				files = append(files, n)
			}
		}
		if len(files) == 0 {
			// Already integrated — and the bookkeeping is done HERE and not skipped,
			// because this is the path a recovered interruption takes. Killing lca does
			// not kill its `git apply` child, so the common interruption is one where
			// the patch finished and commit-tree did not: the recovery then finds
			// nothing to write, and returning early left the integration commit
			// unwritten, the branch reported unintegrated for ever, and `lca clean
			// --branches` unable to reclaim it — while the branch's work sat in the
			// tree. The commit costs nothing, keeps `git log --graph` honest about a
			// recovered integration, and is the only thing that makes the ancestry
			// question answerable afterwards.
			//
			// The journal goes too, by BRANCH: a branch wholly in the tree is exactly
			// clean.go's "nothing left to tell anyone", and a warning that survives the
			// documented recovery is a signal the operator learns to ignore.
			w.recordIntegration(top, m, d, sid)
			dropJournal(lcaDir, w.branch)
			return 0, "", nil
		}
		patch, err := gitCmd(top, nil, nil, append([]string{"diff", "--binary", d, m}, w.pathspec()...)...)
		if err != nil {
			return 0, "", err
		}

		// Never-list item 8, enforced where every route passes through. Nothing
		// upstream of here can be relied on to have checked: `git add -A` in a
		// worktree with unmerged index entries resolves them WITH the marker text,
		// and the inherited check_cmd passes happily over a conflicted docs/x.md, a
		// golden fixture, a YAML file or a comment block. Then the patch writes
		// `<<<<<<< HEAD` into the caller's own file and the caller is told the
		// integrator merged it. One scan of a patch already in memory, cheaper than
		// any of the ways to be wrong about it.
		if bad := conflictMarkers(patch); len(bad) != 0 {
			return 0, fmt.Sprintf("the merge of %s is NOT resolved: conflict markers are still in %s.\n"+
				"Nothing was written — markers in your own working files are the one outcome worse than not merging at all.\n"+
				"Resolve them where the resolution lives, commit there, and finish it with\n  lca merge --finish",
				w.branch, strings.Join(bad, ", ")), nil
		}

		// --check before apply, ALWAYS. Measured: it catches a rival writer and
		// writes nothing, so a failure here costs a re-snapshot and not a half
		// patched tree.
		if _, errs, code := gitRun(top, nil, []byte(patch), "apply", "--check", "--binary", "-"); code != 0 {
			if attempt == 0 {
				continue
			}
			// git's own stderr as the REASON, rather than asserting a cause. `--check`
			// also fails for things no rival did — a path that became a directory or
			// a symlink, a mode or permission change, a file it cannot read — and
			// stating "something wrote those files" as fact sent the operator hunting
			// a writer who did not exist.
			return 0, fmt.Sprintf("%s was merged cleanly but git refused to write it into your tree: %s\n"+
				"A rival writer is the usual cause; a path that became a directory or a symlink, or a mode change, does it too.\n"+
				"NOTHING was written; the branch is kept.\n"+
				"Read the files again and re-delegate, or run\n  lca merge %s",
				w.branch, strings.TrimSpace(lastLines(errs, 4, 400)), w.branch), nil
		}

		journal := writeJournal(lcaDir, sid, task, w.branch)
		if _, err := gitCmd(top, nil, []byte(patch), "apply", "--binary", "-"); err != nil {
			// The journal STAYS: git apply is not atomic across files, so the tree may
			// be genuinely half patched and the next /branches has to say so.
			return 0, "", err
		}
		w.recordIntegration(top, m, d, sid)
		if journal != "" {
			os.Remove(journal)
		}
		// By branch as well as by filename: the journal that sent an operator here
		// may have been written by an earlier, interrupted run under a different task
		// id, and that is the one whose warning has to stop.
		dropJournal(lcaDir, w.branch)
		if env.forget != nil {
			for _, f := range w.changedFiles {
				// DROPPED, not refreshed. The caller has seen a DIFF, not the file: a
				// record here says "you know these bytes" and licenses its next
				// whole-file write to erase the merge it just accepted.
				env.forget(ctx, f)
			}
		}
		return len(files), "", nil
	}
	return 0, "", fmt.Errorf("the merge of %s could not be written after two attempts", w.branch)
}

// recordIntegration writes the integration commit and points
// refs/lca/integrated/<sid> at it, for three reasons: `git log --graph` tells
// the truth about what was merged into the working tree, the snapshot commits
// stop dangling, and `merge-base --is-ancestor` becomes a one-command answer to
// "is this branch integrated?" — which is the question `lca clean --branches`
// has to ask, because `git branch -d` consults HEAD and the upstream and nothing
// else.
//
// One function, called from BOTH of integrate's success paths. It used to be
// inline on one of them only, and the other — the recovered interruption — left
// a branch nothing could ever classify.
func (w *worktree) recordIntegration(top, mergeTree, ours, sid string) {
	i, err := gitCmd(top, nil, nil, "commit-tree", mergeTree, "-p", ours, "-p", w.branch,
		"-m", "lca: integrated "+w.branch+" (check passed) into your working tree")
	if err != nil {
		return
	}
	gitCmd(top, nil, nil, "update-ref", integratedRef(sid), strings.TrimSpace(i))
}

// markerFiles names the files in a worktree that still carry conflict markers.
//
// Scoped to the paths git itself reports as UNMERGED, so a repository holding a
// file full of marker-looking text for its own reasons — a test fixture, a
// document about merges — is never caught by it. Both ends of a pair are
// required, for the reason conflictMarkers gives.
func markerFiles(dir string) []string {
	out, _, code := gitRun(dir, nil, nil, "diff", "--name-only", "--diff-filter=U")
	if code != 0 {
		return nil
	}
	var bad []string
	for _, rel := range strings.Split(strings.TrimSpace(out), "\n") {
		if rel == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		opened, shut := false, false
		for _, line := range strings.Split(string(b), "\n") {
			switch {
			case strings.HasPrefix(line, "<<<<<<< "):
				opened = true
			case strings.HasPrefix(line, ">>>>>>> "):
				shut = true
			}
		}
		if opened && shut {
			bad = append(bad, rel)
		}
	}
	return bad
}

// conflictMarkers names the files a patch would write conflict markers into.
//
// It looks only at ADDED lines, and it demands BOTH ends of a marker pair in one
// file before it says anything: `<<<<<<< ` and `>>>>>>> ` with the trailing space
// git puts before the ref name. The middle `=======` is deliberately not a
// trigger on its own — it is also how anyone underlines a heading in Markdown or
// reStructuredText, and a sweep that refused those would make the guard the thing
// operators switch off.
func conflictMarkers(patch string) []string {
	file, open := "", map[string]int{}
	var order []string
	// Line by line without materialising the whole patch as a slice of strings: a
	// binary hunk makes a patch arbitrarily large, and this runs on every
	// integration.
	for rest := patch; rest != ""; {
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = ""
		}
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			file = strings.TrimPrefix(line, "+++ b/")
			continue
		case strings.HasPrefix(line, "+++ ") || !strings.HasPrefix(line, "+"):
			continue
		}
		body := line[1:]
		if file == "" {
			continue
		}
		bit := 0
		switch {
		case strings.HasPrefix(body, "<<<<<<< "):
			bit = 1
		case strings.HasPrefix(body, ">>>>>>> "):
			bit = 2
		default:
			continue
		}
		if _, seen := open[file]; !seen {
			order = append(order, file)
		}
		open[file] |= bit
	}
	var out []string
	for _, f := range order {
		if open[f] == 3 {
			out = append(out, f)
		}
	}
	return out
}

// ── the resolution worktree ─────────────────────────────────────────────────

// mergeWorktree checks out a REAL merge of the caller's snapshot and the role's
// branch, in its own worktree, with conflict markers in the files.
//
// Why a checkout here when the integration deliberately avoids one: this tree is
// ours, not the user's, and a clean checkout is what makes git do a real merge —
// `UU` in status, all three stages in `git ls-files -u`, labelled markers, and
// `git merge --abort` available. The caller's own working tree is never where
// markers go.
//
// The branch it is cut from is refs/lca/yours/<sid>, the very snapshot the merge
// was computed against: a resolution cut from the DELEGATION snapshot instead
// re-conflicts with itself when it is integrated (measured).
func (o *Orchestrator) mergeWorktree(top, sid, task, branch string) (*worktree, string, error) {
	lcaDir := o.cfg.stateDir()
	sub := "."
	if p, err := gitCmd(o.jl.Root, nil, nil, "rev-parse", "--show-prefix"); err == nil {
		sub = normSub(p)
	}
	unlock, err := lockGit(o.lockDir())
	if err != nil {
		return nil, "", err
	}
	defer unlock()

	yours, err := gitCmd(top, nil, nil, "rev-parse", "--verify", yoursRef(sid))
	if err != nil {
		return nil, "", fmt.Errorf("%s is gone, so there is no merge to check out: %w", yoursRef(sid), err)
	}
	base := strings.TrimSpace(yours)
	mb := freeBranch(top, "lca/merge/"+refWord(sid)+"-"+refWord(task))
	if _, err := gitCmd(top, nil, nil, "branch", mb, base); err != nil {
		return nil, "", err
	}
	gitCmd(top, nil, nil, "update-ref", madeRef(mb), base)
	dir, err := worktreeDir(lcaDir, refWord(sid)+"-merge")
	if err != nil {
		return nil, "", err
	}
	if _, err := gitCmd(top, nil, nil, "-c", "core.hooksPath="+os.DevNull, "worktree", "add", dir, mb); err != nil {
		gitCmd(top, nil, nil, "branch", "-D", mb)
		return nil, "", err
	}
	if real, e := filepath.EvalSymlinks(dir); e == nil {
		dir = real
	}
	writeOwner(dir, owner{session: sid, role: "merge", branch: mb, source: branch, base: base})
	w := &worktree{mgr: &o.worktrees, top: top, dir: dir, root: filepath.Join(dir, sub), sub: filepath.ToSlash(sub), base: base, branch: mb}
	// rc 1 is the expected answer — that is what the markers are for — and only a
	// rc that is neither 0 nor 1 is a failure worth reporting.
	_, errs, code := gitRun(dir, nil, nil, "-c", "core.hooksPath="+os.DevNull, "-c", "commit.gpgsign=false", "merge", "--no-ff", "-m", "lca: integrate "+branch, branch)
	if code != 0 && code != 1 {
		return w, "", fmt.Errorf("checking out the merge failed: %s", strings.TrimSpace(lastLines(errs, 4, 400)))
	}
	conflicted, _ := gitCmd(dir, nil, nil, "diff", "--name-only", "--diff-filter=U")
	return w, strings.TrimSpace(conflicted), nil
}

// changedAgainst lists what this worktree's branch changes against its base, in
// the form changedFiles holds (relative to the jail root, confined to w.sub).
// integrate needs it for the per-file permission gate and the leases, and the
// resolution branch is not something diff() can be asked about: diff() compares
// the worktree's INDEX, and the resolution is already committed.
func (w *worktree) changedAgainst(rev string) error {
	names, err := gitCmd(w.top, nil, nil, append([]string{"diff", "--name-only", w.base, rev}, w.pathspec()...)...)
	if err != nil {
		return err
	}
	w.changedFiles = nil
	for _, n := range strings.Split(strings.TrimSpace(names), "\n") {
		if n == "" {
			continue
		}
		if w.sub != "." && w.sub != "" {
			n = strings.TrimPrefix(n, w.sub+"/")
		}
		w.changedFiles = append(w.changedFiles, n)
	}
	return nil
}

// ── the integrator's one attempt ────────────────────────────────────────────

// resolveConflict gives a conflict to the integrator role, ONCE, and reports
// what the caller should be told either way.
//
// One attempt, then it is the human's. An unsupervised third agent resolving a
// merge it did not write, twice, is the silent-overwrite failure wearing a hat —
// but an operator running unattended for hours can opt in by naming the role,
// and that is the operator's decision to make, not the model's.
//
// The integrator works in the resolution worktree through the ordinary path: its
// own jail rooted there (the mechanism reviewDelegation already uses), the FAILED
// delegation's own check_cmd as its verifier, the same sandbox, the same
// approvals. Its resolution is committed per attempt by commitWork, exactly like
// any other role's work, and integrated with the resolution worktree's own base.
func (o *Orchestrator) resolveConflict(tc *ToolCtx, w *worktree, role *Agent, sid, task, firstText string, checks []string) (merged int, text string, ok bool) {
	ctx, s := tc.Ctx, tc.S
	top := firstNonEmpty(w.callerTop, w.top)
	mw, conflicted, err := o.mergeWorktree(top, sid, task, w.branch)
	if err != nil {
		return 0, firstText + "\n\n" + fmt.Sprintf("the %s role could not be given the merge: %v", role.Name, err), false
	}
	// The resolution worktree is KEPT on every failure path below: it holds the
	// markers, the three stages and `git merge --abort`, and it is the thing the
	// human is being handed.
	child, err := o.newChild(s, role, "resolve "+w.branch)
	if err != nil {
		return 0, firstText + "\n\n" + fmt.Sprintf("the %s role could not be started: %v", role.Name, err), false
	}
	defer o.forgetChild(child.ID)
	jl, err := NewJail(mw.root, o.jl.Allowed, o.jl.Unsafe)
	if err != nil {
		return 0, firstText + "\n\n" + fmt.Sprintf("the %s role could not be confined to the merge worktree: %v", role.Name, err), false
	}
	jl.Shell, jl.Member = o.jl.Shell, o.jl.Member
	child.jl, child.isolated = jl, true
	child.branch, child.wt = mw.branch, mw
	child.RefreshSystem()
	child.Msgs = append(child.Msgs, Message{Role: "user", Content: fmt.Sprintf(`You are in a git worktree holding an UNFINISHED MERGE. Another role's
verified change (%s) could not be merged into the project's working tree
because the two sides touched the same region.

Conflicted files:
%s

The files contain conflict markers: the part under <<<<<<< HEAD is the
project's own working tree as it stands, and the part under ======= down to
>>>>>>> %s is the role's work. Resolve every one of them so that
both intentions survive — the role's change AND what the project's tree already
had. Delete every marker. Then stop.

%s

This is the only attempt: if the check does not pass, the merge is handed to the
human and your work is left here for them to read.`,
		w.branch, "  "+strings.Join(strings.Split(firstNonEmpty(conflicted, "(none reported — read `git status`)"), "\n"), "\n  "),
		w.branch,
		map[bool]string{true: "Definition of done: `" + strings.Join(checks, " && ") + "` exits 0 here. A verifier runs it after you stop; it decides, not you.",
			false: "There is no check command, so leave the tree so that it builds."}[len(checks) > 0])})

	entry := o.trackStart(child.ID, "integrate", role.Name, "resolve "+w.branch)
	child.view.Begin()
	start := time.Now()
	// ONE, the literal number, and not o.verifyAttempts(). The cap belongs to this
	// call site: verify_attempts is the team's budget for a CODER that is fixing
	// its own work against a check it can read, and feeding a failed check back to
	// an unsupervised third agent so it can rewrite a merge it did not write,
	// twice, is the silent-overwrite failure wearing a hat. It is also what the
	// prompt above and the hand-over below both say, and a number an operator can
	// raise in roles.yaml made both of them false.
	v := child.RunVerifiedAll(ctx, checks, 1)
	child.view.Finish(v.Status, time.Since(start))
	child.saveTranscript()
	o.noteCtx(entry, child)
	o.trackEnd(entry, v.Status, v.Tail)

	stop := func(why string) string {
		// The resolution worktree is handed back in the state this text PROMISES,
		// which it was not before: commitWork has already committed the integrator's
		// attempt by the time the check is judged, and `git commit` during a merge
		// CONSUMES MERGE_HEAD. The human was told to run `git merge --abort` in a
		// tree where it exits 128, `git status` was clean, and the file held the
		// resolution the verifier had REJECTED with nothing on screen to say it was
		// rejected. o.handBack puts the markers, the three stages and MERGE_HEAD back
		// and keeps the attempt readable on a ref.
		attempt := o.handBack(top, mw, w.branch, sid, task)
		// `git merge --abort` is offered only where there is a merge to abort. The
		// whole reason this text had to be rebuilt is that it named a command that
		// exits 128, and naming it again on the one path handBack cannot restore
		// would be the same mistake in a smaller place.
		abort := "git status"
		if _, _, code := gitRun(mw.dir, nil, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD"); code == 0 {
			abort = "git status, git merge --abort"
		}
		lines := []string{
			"", "", fmt.Sprintf("THE RUN STOPS HERE. %s had its one supervised attempt at the merge and %s.", role.Name, why),
			"Nothing was written to your tree. Both are kept for the human:",
			"  the role's branch   " + w.branch,
			"  the merge worktree  " + mw.dir + "  (" + abort + ")",
		}
		if attempt != "" {
			lines = append(lines, "  its rejected attempt "+attempt+"  (git show)")
		}
		lines = append(lines, "Resolve it there, then `lca merge --finish "+mw.branch+"`.",
			"Do not try to resolve this yourself and do not re-delegate: tell the user.")
		return strings.Join(lines, "\n")
	}
	if v.Status != "passed" {
		return 0, firstText + stop("its check "+v.Status) + "\n\n" + strings.TrimSpace(v.Tail), false
	}
	resolution, err := gitCmd(mw.dir, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return 0, firstText + stop("its resolution could not be read"), false
	}
	if err := mw.changedAgainst(strings.TrimSpace(resolution)); err != nil {
		return 0, firstText + stop("what it changed could not be read"), false
	}
	// The same two-case treatment mergeBranch gives a delegation's own diff, and
	// for the same reason: the integrator's jail is the WHOLE merge worktree, so
	// its resolution can touch any file it edited — not only the conflicted ones
	// and not only the ones in the reviewed diff. An `Ask` rule on
	// db/migrations/** that the delegation's own path would have raised must raise
	// here too, or "resolving a conflict in handler.go" becomes a way to write an
	// unprompted migration.
	switch act, pattern := s.editGate(mw.changedFiles); act {
	case Deny:
		return 0, firstText + "\n\n" + fmt.Sprintf("%s resolved the merge, but it touches %s, which a permission rule denies editing — nothing was written. The resolution is at %s.",
			role.Name, pattern, mw.dir), false
	case Ask:
		msg, ok := tc.Ask("edit", pattern, "MERGE "+mw.branch, "   "+role.Name+"'s resolution writes:\n     "+strings.Join(mw.changedFiles, "\n     "))
		if !ok {
			return 0, firstText + "\n\n" + fmt.Sprintf("%s resolved the merge, but writing it was not approved — nothing was written. The resolution is at %s.\n%s",
				role.Name, mw.dir, msg), false
		}
	}
	n, conflicts, err := mw.integrate(ctx, s.integrateEnv(), role.Name, sid, task)
	switch {
	case err != nil:
		return 0, firstText + stop("writing the resolution failed: "+err.Error()), false
	case conflicts != "":
		// The resolution itself collided with something written while it was being
		// made. Honest: the rival and the role really did collide.
		return 0, conflicts + stop("its resolution collided with something written while it was being made"), false
	}
	// A clean merge with nothing to write is still a success: the branch is in the
	// tree, which is the only thing the caller asked about.
	return n, fmt.Sprintf("%s merged %s into your tree: the merge conflicted and %s resolved it in %s, then %s passed.\n%s",
		role.Name, w.branch, role.Name, mw.dir, firstNonEmpty(strings.Join(checks, " && "), "no check"),
		fmt.Sprintf("Resolved: %s", strings.Join(mw.changedFiles, ", "))), true
}

// handBack puts a resolution worktree back into the state the hand-over text
// promises — markers in the files, the three stages in the index, an abortable
// MERGE_HEAD — and returns a ref the rejected attempt can still be read from,
// or "" when there was nothing to put back.
//
// It has to exist because commitWork has already committed the integrator's
// attempt by the time its check is judged, and `git commit` during a merge
// consumes MERGE_HEAD. Measured on the failure path: `git merge --abort` exited
// 128 with "There is no merge to abort", `git status` was clean, `git ls-files
// -u` was empty, the file held no markers, and what it did hold was the
// resolution the verifier had REJECTED, committed as the branch tip. The human
// who opted into an integrator was handed a clean tree containing a rejected
// answer and no way to see the three sides.
//
// `reset --hard` here is on LCA'S OWN merge worktree, which is the one place
// §3's never-list does not reach: we made it, it exists only to hold a merge,
// and nothing of the user's is in it. The attempt is not thrown away — it goes
// on refs/lca/attempt/<sid>-<task>, outside refs/heads so `git branch` never
// lists it, and `lca clean` drops it with the session's other bookkeeping.
func (o *Orchestrator) handBack(top string, mw *worktree, source, sid, task string) string {
	head, err := gitCmd(mw.dir, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	tip := strings.TrimSpace(head)
	if tip == mw.base {
		// Nothing was committed, so the worktree is still mid-merge and already
		// holds everything the text names. This is the path commitWork's own marker
		// refusal takes, and it is the better one.
		return ""
	}
	ref := "refs/lca/attempt/" + refWord(sid) + "-" + refWord(task)
	if _, err := gitCmd(top, nil, nil, "update-ref", ref, tip); err != nil {
		// Without somewhere to keep it, the attempt would become unreachable the
		// moment the reset lands. Leave the worktree as it is instead: a committed
		// rejected answer that can be read beats a reproducible merge that silently
		// discarded one.
		return ""
	}
	if _, _, code := gitRun(mw.dir, nil, nil, "reset", "--hard", mw.base); code != 0 {
		return ref
	}
	gitRun(mw.dir, nil, nil, "-c", "core.hooksPath="+os.DevNull, "-c", "commit.gpgsign=false",
		"merge", "--no-ff", "-m", "lca: integrate "+source, source)
	return ref
}

// editGate evaluates the edit rules over a file list, strictest first: one
// denied file blocks the whole write. Lifted out of runDelegateTool so the
// integrator's resolution meets exactly the same gate the delegation's own diff
// does — a resolution is still a write to the user's files.
func (s *Session) editGate(files []string) (Action, string) {
	pattern, act := "*", Allow
	for _, f := range files {
		switch Evaluate("edit", permPath(s.jail(), f), s.rules()...) {
		case Deny:
			return Deny, f
		case Ask:
			if act == Allow {
				pattern, act = f, Ask
			}
		}
	}
	return act, pattern
}

// ── where a worktree lives, and who owns it ─────────────────────────────────

// worktreeDir makes the directory a worktree will be checked out into, with the
// PID in its name: `lca clean` has to tell a worktree a live lca is working in
// from one a SIGKILL orphaned, and the owner file beside it is only readable if
// something wrote it — the name is the fallback.
func worktreeDir(lcaDir, session string) (string, error) {
	base, err := filepath.Abs(filepath.Join(lcaDir, "worktrees"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	// `p<pid>`, not a bare `<pid>`, and that one letter is the whole of a fix.
	// os.MkdirTemp appends a DECIMAL random suffix, so a name built as
	// `<session>-<pid>-XXXXXX` ends in two numbers and readOwner's fallback read
	// the random one: every worktree whose .owner file could not be read reported
	// a pid that has never existed, pidAlive said "gone", and `lca clean` force
	// removed a worktree a live lca was working in. A labelled token cannot be
	// confused with a random number.
	dir, err := os.MkdirTemp(base, fmt.Sprintf("%s-p%d-", session, os.Getpid()))
	if err != nil {
		return "", err
	}
	os.Remove(dir) // worktree add wants to create it
	return dir, nil
}

// writeOwner records who a worktree belongs to, NEXT TO it rather than inside
// it: a file inside would be picked up by the subagent's own `add -A` and land
// in its diff. One k=v per line, because this is read by `lca clean` and by a
// human looking at a directory that outlived its session.
//
// base and source matter only for a resolution worktree, and they matter a lot:
// `lca merge --finish` integrates the resolution against the commit the merge
// worktree was CUT FROM, and inferring that from the commit graph afterwards is
// guesswork the moment the human commits twice.
func writeOwner(dir string, o owner) {
	body := fmt.Sprintf("pid=%d\nsession=%s\nrole=%s\nbranch=%s\nsource=%s\nbase=%s\nstarted=%s\n",
		os.Getpid(), o.session, o.role, o.branch, o.source, o.base, time.Now().Format(time.RFC3339))
	// A failure here is not cosmetic: without the file `lca clean` has only the
	// directory name to go on, and `lca merge --finish` loses the commit the
	// resolution must be integrated against. Said out loud rather than discarded.
	if err := os.WriteFile(dir+".owner", []byte(body), 0o600); err != nil {
		warnLine("could not record who owns the worktree %s (%v) — `lca clean` will leave it alone rather than guess", filepath.Base(dir), err)
	}
}

type owner struct {
	dir, session, role, branch string
	source                     string    // the branch a resolution worktree is merging
	base                       string    // the commit it was cut from
	started                    time.Time // zero when unrecorded or unparseable
	pid                        int
}

func readOwner(dir string) owner {
	o := owner{dir: dir}
	b, err := os.ReadFile(dir + ".owner")
	if err != nil {
		// No owner file: fall back to the pid worktreeDir LABELLED in the directory
		// name, and only to that. A bare number in the name is not a pid — MkdirTemp
		// appends a random decimal one — and reading it as a pid made every
		// unreadable-owner worktree an orphan whose owner "is gone", including the
		// one a live lca was working in and every worktree an older build left with
		// no owner file at all. A worktree whose owner cannot be established stays
		// pid 0, which orphanWorktrees reads as "leave it alone": leaking a
		// directory is cheaper than destroying a delegation's uncommitted work.
		// From the RIGHT: worktreeDir appends the labelled pid immediately before
		// MkdirTemp's random suffix, so the last such token is the one it wrote and
		// any earlier match would be something in the session or role name.
		parts := strings.Split(filepath.Base(dir), "-")
		for i := len(parts) - 1; i >= 0; i-- {
			if part := parts[i]; len(part) > 1 && part[0] == 'p' {
				if n := atoiDefault(part[1:], 0); n > 0 {
					o.pid = n
					break
				}
			}
		}
		return o
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			o.pid = atoiDefault(v, 0)
		case "session":
			o.session = v
		case "role":
			o.role = v
		case "branch":
			o.branch = v
		case "source":
			o.source = v
		case "base":
			o.base = v
		case "started":
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				o.started = t
			}
		}
	}
	return o
}

// ── listing what lca made ───────────────────────────────────────────────────

// lcaBranch is one row of /branches.
type lcaBranch struct {
	name     string
	head     string
	subject  string
	worktree string // the directory holding it, "" when none does
	// when the DAY starts counting: the integration's date when there is one, and
	// the tip commit's otherwise. Not the tip's in the integrated case, because an
	// integration interrupted on Monday and recovered on Wednesday — the
	// documented recovery — integrates a branch whose newest commit is two days
	// old, and the same minute's `lca clean --branches` would delete it before the
	// human could look at what landed. The day exists for exactly that human.
	when       time.Time
	integrated bool
	// ours says lca recorded creating this branch (madeRef). Nothing is deleted
	// without it, whatever the name and whatever the ancestry say.
	ours bool
}

// listBranches joins `git branch --list 'lca/*'` with `git worktree list` and
// asks ancestry for the one thing neither can answer: whether a branch is in the
// tree. `git branch -d` refuses a branch that is merged only into
// refs/lca/integrated/* — it consults HEAD and the upstream and nothing else —
// so "is it integrated?" is ours to ask, with merge-base --is-ancestor against
// every integration ref.
func listBranches(top string) []lcaBranch {
	out, _, code := gitRun(top, nil, nil, "for-each-ref",
		"--format=%(refname:short)%09%(objectname:short)%09%(committerdate:unix)%09%(contents:subject)", "refs/heads/lca/")
	if code != 0 {
		return nil
	}
	held := map[string]string{}
	wl, _, _ := gitRun(top, nil, nil, "worktree", "list", "--porcelain")
	dir := ""
	for _, line := range strings.Split(wl, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			dir = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			held[strings.TrimPrefix(line, "branch refs/heads/")] = dir
		}
	}
	var refs []string
	for _, line := range strings.Split(mustOut(gitRun(top, nil, nil, "for-each-ref", "--format=%(refname)", "refs/lca/integrated/")), "\n") {
		if strings.TrimSpace(line) != "" {
			refs = append(refs, strings.TrimSpace(line))
		}
	}
	var rows []lcaBranch
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		b := lcaBranch{name: f[0], head: f[1], worktree: held[f[0]]}
		if len(f) > 2 {
			if n := atoiDefault(f[2], 0); n > 0 {
				b.when = time.Unix(int64(n), 0)
			}
		}
		if len(f) > 3 {
			b.subject = f[3]
		}
		_, _, code := gitRun(top, nil, nil, "rev-parse", "--verify", "-q", madeRef(b.name))
		b.ours = code == 0
		// Only a branch lca recorded making can be called integrated, because the
		// ancestry test alone says yes to the project's whole history (see madeRef)
		// and "integrated" is what licenses a delete.
		for _, r := range refs {
			if !b.ours {
				break
			}
			if _, _, code := gitRun(top, nil, nil, "merge-base", "--is-ancestor", b.name, r); code == 0 {
				b.integrated = true
				if ct, _, code := gitRun(top, nil, nil, "log", "-1", "--format=%ct", r); code == 0 {
					if n := atoiDefault(strings.TrimSpace(ct), 0); n > 0 {
						b.when = time.Unix(int64(n), 0)
					}
				}
				break
			}
		}
		rows = append(rows, b)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	return rows
}

func mustOut(out, _ string, _ int) string { return out }
