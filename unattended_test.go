package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// What holds when nobody is watching.
//
// Every test here is about the same two properties, and about the one thing that
// made a guard inert: a program driven from cron has no keyboard, so an `Ask`
// rule is not a control, a wait for a human is not a wait, and a record written
// after a remote call is a record the kill window can lose. The findings these
// came from were all reachable through a real `lca ticket` or `lca run`.

// unattendedAp makes an approver the way newTicketRun does: standing trust for
// every class, quiet, and told there is nobody there. That combination is the
// premise of the next three tests and not an exaggeration of one — `lca ticket`
// has no -y flag and no approve: key, so this is the ONLY posture it runs in.
func unattendedAp(t *testing.T, o *Orchestrator) {
	t.Helper()
	o.ap.TrustAll()
	o.ap.quiet = true
	o.ap.Unattended("test: unattended")
}

// allowGit puts git on the sandbox allowlist, so that what refuses a push below
// is the permission rule under test and not the jail. git IS on lca's default
// allowlist, which is why the rule has to be the thing that holds.
func allowGit(t *testing.T, o *Orchestrator, root string) {
	t.Helper()
	jl, err := NewJail(root, append(append([]string{}, o.jl.Allowed...), "git"), false)
	if err != nil {
		t.Fatal(err)
	}
	jl.Shell, jl.Member = o.jl.Shell, o.jl.Member
	o.jl = jl
}

// A `lca ticket` stage runs in the ticket's worktree, and that worktree shares
// its refs, its remotes and its config with the repository it was cut from.
// isolationRules rates those git commands `Ask` — a door with a keyboard behind
// it — and `lca ticket` auto-approves every door by construction, so the guard
// was an auto-yes in exactly the run it was written for: a coder stage could
// type `git push origin HEAD:refs/heads/main` and land unreviewed, unchecked
// work on the remote's main, defeating ticket.go's "there is no transition a
// model can reach by typing a command".
func TestATicketStageCannotPushOrMoveTheOperatorsRefs(t *testing.T) {
	f := newTktStageFix(t)
	unattendedAp(t, f.h.orch)
	allowGit(t, f.h.orch, f.root)

	m := &tktStageRun{orch: f.h.orch, lead: f.h.sess, pc: f.pc, cfg: f.h.orch.cfg}
	sess, err := m.child(f.h.orch.agents["coder"], "one stage of a ticket")
	if err != nil {
		t.Fatal(err)
	}
	wt := &worktree{top: f.root, dir: f.root, root: f.root, sub: ".",
		branch: "agent/BSK-1", ctx: context.Background()}
	if err := m.bindTree(sess, wt); err != nil {
		t.Fatal(err)
	}

	// The premise, both halves of it: git runs, and the trust really is standing.
	// Without this a test asserting refusals passes on a session that can run
	// nothing at all.
	if msg, ok := askRun(sess, "git status --porcelain"); !ok {
		t.Fatalf("the premise is gone — reading git must still work: %s", msg)
	}
	if !f.h.orch.ap.Trusts("run") {
		t.Fatal("the premise is gone — this command grants standing trust for run")
	}

	for _, line := range []string{
		"git push origin HEAD:refs/heads/main",
		"git push --force origin agent/BSK-1",
		"git update-ref refs/heads/main deadbeef",
		"git branch -f main deadbeef",
		"git tag -f v9 deadbeef",
		"git remote add elsewhere git@example.com:x/y.git",
		"git config core.hooksPath /tmp/hooks",
		"git worktree add /tmp/w main",
		"git stash push -u",
		"git reflog expire --expire=now --all",
		"git gc --prune=now",
		"git commit --allow-empty -m anything",
		"git merge main",
		"git rebase main",
		"git reset --hard main",
		"git cherry-pick deadbeef",
		"git -C /etc status",
		// The two option forms that carry any of the above past a glob on the
		// subcommand. `git -c` is the one that matters: it is still a push.
		"git -c protocol.version=2 push origin HEAD:refs/heads/main",
	} {
		msg, ok := askRun(sess, line)
		if ok {
			t.Errorf("%q was approved — an unattended stage must not reach a ref the operator owns", line)
			continue
		}
		if !strings.Contains(msg, "permission rule") {
			t.Errorf("%q: the refusal must name the rule, got %q", line, msg)
		}
	}

	// And where a shell DOES split the line — a team with sandbox: shell: true —
	// the push segment still decides, because runAction walks every segment and
	// the most restrictive answer wins.
	if got := runAction("git status && git push origin main", true, sess.rules()...); got != Deny {
		t.Errorf("a shell would run the push, so the line must be denied: %s", got)
	}
}

// The same hole in the other entry point. `lca run` calls TrustAll() unless
// -ask, and a delegate: step's subagent is isolated, so isolationRules' Ask on
// the shared-ref commands auto-approved and the delegated model could push its
// scratch branch — around the parent's apply: step, which is supposed to be the
// only way a delegation's work reaches anything.
func TestAnUnattendedDelegationCannotPushItsScratchBranch(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "it would not let me"}
			}
			return fakeReply{calls: []ToolCall{call("p", "run_command",
				map[string]any{"command": "git push origin HEAD:refs/heads/main"})}}
		}
		return fakeReply{content: "ok"}
	})
	fs.models = []string{"lead-a", "coder-a"}
	h := newRoleHarness(t, fs, testRoles, true)
	unattendedAp(t, h.orch)
	seedFile(t, h, twentyLines("line 10"))

	delegateOnMember(t, h, Args{"role": "coder", "task": "push it"})

	var refused bool
	for _, r := range fs.reqs() {
		if strings.Contains(r.Body, "permission rule") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("an isolated subagent with nobody to ask pushed to the remote")
	}

	// And the REPL's door is untouched: with a human there, this is still a
	// question rather than a refusal.
	if got := Evaluate("run", "git push origin main", isolationRules); got != Ask {
		t.Fatalf("an interactive delegation still asks: %s", got)
	}
}

// newChild denies task, todo and mcp_write structurally — a child never SEES a
// schema its role was not granted — and did not deny `delegate`, which falls
// through defaultRules' `* * Allow`. A ticket stage could therefore buy a
// delegation to any role in roles.yaml, including one the pipeline: block never
// named, on that role's member, under that member's sandbox and rules.
func TestATicketStageCannotDelegateToARoleThePipelineNeverNamed(t *testing.T) {
	f := newTktStageFix(t)
	// Depth for a nested call, which is what made the tool appear at all.
	f.h.orch.cfg.SubagentMax = 3
	// And the role NAMES delegate, which is how a team would grant it for the
	// REPL and for `lca run` without meaning to grant it to a ticket stage: a
	// stage's authority is the pipeline: block's, and the block says three roles.
	coder := f.h.orch.agents["coder"]
	coder.Tools = append(coder.Tools, "delegate")

	m := &tktStageRun{orch: f.h.orch, lead: f.h.sess, pc: f.pc, cfg: f.h.orch.cfg}
	sess, err := m.child(coder, "one stage of a ticket")
	if err != nil {
		t.Fatal(err)
	}
	if Evaluate("delegate", "*", sess.rules()...) != Deny {
		t.Fatalf("a stage's roles are the three the pipeline: block named: %v", sess.rules())
	}
	if !Disabled("delegate", sess.rules()...) {
		t.Fatal("and the denial is structural: the stage must not be shown the schema")
	}
	for _, d := range toolsFor(sess) {
		if d.Name == "delegate" {
			t.Fatal("the delegate schema is still in this stage's prefix")
		}
	}

	// The same denial one level up, for every child of a role that does not name
	// the key — the chokepoint task, todo and mcp_write already go through.
	plain := f.h.orch.agents["reviewer"]
	child, err := f.h.orch.newChild(f.h.sess, plain, "a subagent")
	if err != nil {
		t.Fatal(err)
	}
	if Evaluate("delegate", "*", child.rules()...) != Deny {
		t.Fatal("a subagent of a role that never named delegate must not reach a fourth role")
	}
	// And a role that DOES name it keeps it, or the nested delegations that exist
	// on purpose stop working.
	lead := f.h.orch.agents["integrator"]
	lead.ToolsSet, lead.Tools = true, []string{"read_file", "delegate"}
	named, err := f.h.orch.newChild(f.h.sess, lead, "a subagent that may delegate")
	if err != nil {
		t.Fatal(err)
	}
	if Evaluate("delegate", "*", named.rules()...) == Deny {
		t.Fatal("a role that names delegate in its tools: set has named it")
	}
}

// A merge request opened twice for one ticket is the single failure the whole
// command is built to prevent. `propose` is Redo: true, so enter()'s pending
// refusal is skipped — and the record enter() writes BEFORE the create call,
// whose whole purpose is to say "this call may have landed", was never read by
// anything. The forge's documented filter is `state: opened`, so a merge request
// a reviewer has since merged or closed answers "none" quite legitimately.
func TestASecondMergeRequestIsRefusedWhenTheRecordSaysOneMayHaveLanded(t *testing.T) {
	zero := 0
	st := &TicketState{Ticket: "BSK-1", Summary: "x", State: tktPushed,
		BranchAt: "base000", Head: "work111", PushedSha: "work111", MergedAt: "mergedmain",
		Check:   TicketCheck{Cmd: "go test ./...", Exit: &zero, Attempts: 1},
		Verdict: tktApprove,
		Pending: tktPropose, // killed inside the create call
	}
	// The world a night-one run would have left: the branch cut, the work on it,
	// the merge made and the branch on the remote.
	w := newFakeWorld(t)
	w.heads["agent/BSK-1"] = "work111"
	w.ancestry["work111"] = []string{"base000"}
	w.ancestry["mergedmain"] = []string{"work111", "base000"}
	w.heads["main"] = "mergedmain"
	w.remotes["origin/agent/BSK-1"] = "work111"
	w.mr = nil // the forge answers "no OPEN merge request", which is honest

	r := newRun(t, w, pipeFrom(t, fullPipeline), st)
	status, reason := r.execute()
	if n := w.calls["createmr"]; n != 0 {
		t.Fatalf("a second merge request was opened for one ticket (%d create calls)", n)
	}
	if status == statusPassed {
		t.Fatalf("a run that cannot tell a landed create from a lost one is row 2: %s", status)
	}
	for _, want := range []string{"state: opened", r.pc.Forge.FindMR, "agent/BSK-1"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the refusal must name %q so a person knows where to look: %s", want, reason)
		}
	}
	// And it still tells the ticket, because going quiet is the other failure.
	if w.calls["comment"] == 0 {
		t.Fatal("the report is made even from row 2")
	}
}

// The status transition has no marker on the ticket to prove it, only MovedTo —
// and MovedTo was written AFTER the call, so a process killed in the window
// between the two lost the only record that the move had happened. The next
// night read "never made", called Move again, and a tracker whose transition is
// only available from the original status refused it: a ticket whose work was
// merged and proposed then failed its report every night for ever.
func TestTheTrackerStatusIsNotMovedTwiceForOneTicket(t *testing.T) {
	w := newFakeWorld(t)
	r := fresh(t, w, pipeFrom(t, fullPipeline))
	if status, reason := r.execute(); status != statusPassed {
		t.Fatalf("the whole arc must pass first: %s (%s)", status, reason)
	}
	if w.calls["move"] != 1 || r.st.MovedTo == "" {
		t.Fatalf("the premise: one move, recorded: %v / %q", w.calls, r.st.MovedTo)
	}

	// The kill window: the call returned and the save did not. Moving is what the
	// run wrote BEFORE the call, and it is the whole difference between "the move
	// may have landed" and "the move was never made".
	if r.st.Moving != "" {
		t.Fatalf("the intent is cleared once the call has returned: %q", r.st.Moving)
	}
	r.st.Moving, r.st.MovedTo = r.reportStatus(), ""

	again := newRun(t, w, pipeFrom(t, fullPipeline), r.st)
	status, reason := again.execute()
	if status != statusPassed {
		t.Fatalf("the work is done and the ticket was told: %s (%s)", status, reason)
	}
	if n := w.calls["move"]; n != 1 {
		t.Fatalf("the transition was applied %d times for one ticket", n)
	}
	if n := w.calls["comment"]; n != 1 {
		t.Fatalf("the comment half was already idempotent and must stay so: %d", n)
	}
	if again.st.MovedTo == "" {
		t.Fatal("and the record is settled, so tomorrow night does not ask again")
	}
}

// envSecrets was a pure name heuristic, so a credential the operator declared BY
// NAME — an MCP header's ${env:GH_PAT}, api_key_env, a redact_env: entry — was
// scrubbed out of that server's own replies and out of nothing else. A failing
// check whose output carried it reached the merge request body and the ticket
// comment verbatim, and neither can be unpublished.
func TestACredentialTheOperatorNamedByNameIsScrubbed(t *testing.T) {
	const tok = "ghp-ZxQ9f1-not-a-real-one-7731"
	t.Setenv("GH_PAT", tok)
	t.Setenv("DATABASE_URL", "postgres://svc:S3cr3tPassw0rd@db.internal:5432/app")
	dropSecrets(t)

	// The premise: the guess does not find either of these, which is the bug.
	if looksSecretName("GH_PAT") || looksSecretName("DATABASE_URL") {
		t.Skip("the name heuristic now matches these, so this test is about nothing")
	}
	line := "request rejected: curl -H 'Authorization: Bearer " + tok + "'"
	if got := redactSecrets(line); strings.Contains(got, tok) {
		// Not a Fatal before the declaration: this IS the state the finding describes.
		t.Logf("undeclared, the token is in the clear (as expected): %s", got)
	}

	declareSecretEnv("GH_PAT", "DATABASE_URL")
	if got := redactSecrets(line); strings.Contains(got, tok) {
		t.Fatalf("a declared credential must not reach a merge request body: %s", got)
	}
	if got := redactSecrets("dsn=postgres://svc:S3cr3tPassw0rd@db.internal:5432/app"); strings.Contains(got, "S3cr3tPassw0rd") {
		t.Fatalf("a DSN carries its password: %s", got)
	}

	// A declared variable whose value is a PATH to the credential is still left
	// alone: redacting it destroys the one line that said which file was missing.
	t.Setenv("GITLAB_TOKEN_FILE", "/run/secrets/gitlab_token")
	dropSecrets(t)
	declareSecretEnv("GITLAB_TOKEN_FILE")
	if got := redactSecrets("cannot open /run/secrets/gitlab_token: permission denied"); strings.Contains(got, redactedMark) {
		t.Fatalf("where to FIND a credential is not the credential: %s", got)
	}
}

// roles.yaml's defaults: redact_env: reaches the scrub, because a credential no
// config block references — the one a check_cmd or a deploy script reads — can
// be named nowhere else.
func TestRedactEnvFromRolesReachesTheScrub(t *testing.T) {
	const tok = "bsk-A7-not-a-real-one-44219"
	t.Setenv("BSK_AUTH", tok)
	dropSecrets(t)
	rc, err := loadRolesFrom(t, "defaults:\n  redact_env: [BSK_AUTH]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(rc.RedactEnv, "BSK_AUTH") {
		t.Fatalf("the key did not parse: %v", rc.RedactEnv)
	}
	if got := redactSecrets("deploy failed: auth=" + tok); strings.Contains(got, tok) {
		t.Fatalf("loading the file is what arms the scrub: %s", got)
	}
	// And it round-trips, or /role save deletes it the first time somebody writes
	// a team back out.
	if !strings.Contains(rc.YAML(), "redact_env: [BSK_AUTH]") {
		t.Fatalf("redact_env must survive /role save:\n%s", rc.YAML())
	}
}

// loadRolesFrom writes one roles.yaml and loads it, for the tests that are about
// one key rather than about a team.
func loadRolesFrom(t *testing.T, body string) (*RolesConfig, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "roles.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCA_ROLES", p)
	return loadRoles(Config{Root: dir, Dir: dir})
}

// ls-remote and push are the only two git operations in this program that touch
// a network, and gitRun took no context: a forge host that accepts the
// connection and then sends nothing blocked git in read(2) for ever, with the
// ticket lock held and the ticket claimed. `-timeout` could not reach it, and
// `lca ticket` holds SIGTERM through signal.NotifyContext, so cron's
// `timeout 3600 lca ticket …` cancelled a context nobody was selecting on and
// the process survived every TERM.
func TestARemoteThatAcceptsAndNeverAnswersDoesNotHangTheRun(t *testing.T) {
	f := newTktRepoFix(t, "BSK-30")

	// A listener that accepts the connection and says nothing, which is the
	// half-open shape: a hung gitlab-shell, a load balancer that stopped
	// forwarding, a firewall that swallowed the reset.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // kept open, never written to
			mu.Unlock()
		}
	}()
	// The listener first, so the accept loop is finished before the connections
	// it holds are closed under it.
	defer func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	}()
	dead := "git://" + ln.Addr().String() + "/x"
	gitT(t, f.top, "remote", "add", "dead", dead)

	head := strings.TrimSpace(gitT(t, f.top, "rev-parse", "HEAD"))

	// The run's own clock, as budget.start hands it to a transition.
	for _, name := range []string{"RemoteHead", "PushBranch"} {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		done := make(chan error, 1)
		start := time.Now()
		go func() {
			if name == "RemoteHead" {
				_, _, err := f.git.RemoteHead(ctx, "dead", "agent/BSK-30")
				done <- err
				return
			}
			done <- f.git.PushBranch(ctx, "dead", "agent/BSK-30", head)
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s: a remote that never answers is not a success", name)
			}
			if d := time.Since(start); d > 25*time.Second {
				t.Fatalf("%s: came back only after %s — the run's clock has to reach it", name, d)
			}
		case <-time.After(30 * time.Second):
			cancel()
			t.Fatalf("%s hung on a remote that accepts and never answers — the whole night is gone", name)
		}
		cancel()
	}
}

// GIT_TERMINAL_PROMPT governs git's own prompt and nothing else: ssh's
// key-passphrase prompt and gpg's pinentry open /dev/tty directly. gitRun set
// that one variable and never called inProcessGroup, so a push from a tmux pane
// or a pty-ful CI runner could sit for ever on `Enter passphrase for key …`
// printed to a screen nobody is watching — and to neither of the two streams
// gitRun captures, so the transcript, the trace and the state file all showed
// the run simply stopping at `push`.
func TestGitRunLeavesNoDoorForAPassphrasePrompt(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")

	// A `!`-alias runs through a shell, which is how the child's own environment
	// can be read back out of it.
	out, errs, code := gitRun(dir, nil, nil,
		"-c", `alias.env=!echo prompt=$GIT_TERMINAL_PROMPT git=$GIT_ASKPASS ssh=$SSH_ASKPASS require=$SSH_ASKPASS_REQUIRE`, "env")
	if code != 0 {
		t.Fatalf("the alias did not run: %d %s", code, errs)
	}
	for _, want := range []string{"prompt=0", "git=/bin/true", "ssh=/bin/true", "require=force"} {
		if !strings.Contains(out, want) {
			t.Errorf("gitRun's child must have %s: %q", want, strings.TrimSpace(out))
		}
	}

	// And the half that holds against a program which reads none of them: Setsid
	// leaves the child with no controlling terminal at all, which shows up as a
	// process group of its own.
	pg, _, code := gitRun(dir, nil, nil, "-c", `alias.pg=!ps -o pgid= -p $$`, "pg")
	if code != 0 {
		t.Skipf("ps is not answering here, so the process group cannot be read: %d", code)
	}
	got, err := strconv.Atoi(strings.TrimSpace(pg))
	if err != nil {
		t.Skipf("ps printed %q", pg)
	}
	if mine, err := syscall.Getpgid(os.Getpid()); err == nil && got == mine {
		t.Fatalf("git ran in lca's own process group (%d), so it kept lca's controlling terminal", got)
	}
}

// gitRun's own callers still get what they always got: the exit code, and
// merge-tree's conflict (1) told apart from a failure.
func TestGitRunStillReportsTheExitCode(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	if _, _, code := gitRun(dir, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/nope"); code == 0 {
		t.Fatal("a missing ref is a non-zero exit, and the code is the answer")
	}
	if _, errs, _ := gitRun(dir, nil, nil, "merge-tree", "--write-tree"); !strings.Contains(errs, "usage") && errs == "" {
		t.Fatal("stderr must still come back without the argument list mixed in")
	}
	if _, _, code := gitRun(dir, nil, nil, "--version"); code != 0 {
		t.Fatal("and an ordinary success is still 0")
	}
}

// Confirm's reader goroutine cannot be interrupted — there is no way to abort a
// blocking read of a terminal — so on the Ctrl-C path it finishes later, with a
// line that belongs to whatever asked NEXT. Sending it into the buffered `got`
// channel dropped it: the first character typed at the next question was
// swallowed by a question that was already over, and the `y` the operator typed
// arrived as the empty line that means deny.
func TestAnInterruptedDoorGivesTheKeystrokeBack(t *testing.T) {
	// Our own handler first, for the whole test: the SIGINT below must never be
	// the default action, whichever side of Confirm's own Notify it lands on.
	mine := make(chan os.Signal, 64)
	signal.Notify(mine, os.Interrupt)
	defer signal.Stop(mine)

	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	defer wp.Close()
	in := &Input{f: rp}
	ap := NewApprover(in) // a human at the keyboard: no trust, not unattended

	type res struct{ approved, auto bool }
	done := make(chan res, 1)
	go func() {
		a, b := ap.Confirm("run", "run git status", "")
		done <- res{a, b}
	}()

	// Nudge until one lands inside Confirm's window. The door is drawn before the
	// handler is installed, so there is nothing to poll for; a repeated signal is
	// the honest way to be sure, and with `mine` registered a stray one is inert.
	deadline := time.Now().Add(10 * time.Second)
	var got res
	for interrupted := false; !interrupted; {
		if time.Now().After(deadline) {
			t.Fatal("Confirm never saw the interrupt")
		}
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case got = <-done:
			interrupted = true
		case <-time.After(25 * time.Millisecond):
		}
	}
	if got.approved {
		t.Fatal("Ctrl-C at a door means nothing ran")
	}
	if !ap.TakeInterrupt() {
		t.Fatal("and the interrupt is recorded")
	}

	// The line the operator types at the NEXT question. The abandoned reader is
	// still inside ReadString and will take it; it has to hand it back.
	if _, err := wp.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if s := in.TakePending(); s != "" {
			if s != "y\n" {
				t.Fatalf("the keystroke came back mangled: %q", s)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the abandoned reader swallowed the next question's first keystroke")
}

// A hung child process is the shape every one of these is about, so the one
// thing gitRunIn promises on cancellation is checked directly: the group dies,
// rather than the call returning while a grandchild holds the pipe.
func TestGitRunInReturnsWhenItsContextIsDone(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, code := gitRunIn(ctx, dir, nil, nil, "-c", "alias.nap=!sleep 30", "nap")
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("a cancelled context has to end the child: took %s", d)
	}
	if code == 0 {
		t.Fatal("a killed child is not a success")
	}
	if ctx.Err() == nil {
		t.Fatal(fmt.Sprintf("the premise is gone: %v", ctx.Err()))
	}
}
