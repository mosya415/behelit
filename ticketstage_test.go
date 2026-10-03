package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The whole command, end to end, with everything real except the two things that
// are somebody else's server: the gateway is a fake endpoint and the tracker and
// the forge are the stub from ticketmcp_test.go. The repository is real git, the
// sessions are real sessions, the verifier is the real verifier, the review is
// read by review.go's own reader, and the branch, the commit, the merge and the
// push are made by lca.
//
// This is the test that would have caught a feature that builds and does
// nothing.

// tktRoles is the team an unattended ticket run needs: three roles, a check the
// coder has to satisfy, and a sandbox narrow enough that the check is the only
// command in it.
const tktRoles = `entry: integrator
transport: native
defaults:
  context: 64000
  verify_attempts: 1
  check_timeout: 30
sandbox:
  allow: [test, echo, ls, cat, grep, true, false]
roles:
  coder:
    description: Makes code changes.
    models: [coder-a]
    tools: [read_file, write, edit, run_command, list_dir]
    check_cmd: test -f done.txt
  reviewer:
    description: Judges a diff and nothing else.
    models: [rev-a]
    tools: [read_file, grep, list_dir]
  integrator:
    description: Writes prose for people.
    models: [int-a]
    tools: [read_file, grep, list_dir]
`

// tktStageFix is the whole world: a repository with a bare remote, a team, a
// fake gateway that answers as each stage, and the stub tracker and forge.
type tktStageFix struct {
	h      *harness
	stub   *stubServer
	pc     *PipelineConfig
	world  tktWorld
	root   string
	remote string
	// verdicts is what the reviewer answers, one per round; the last repeats.
	verdicts []string
	// wrote counts the coder's rounds, so a rework round can write something
	// different from round one.
	rounds int
}

func newTktStageFix(t *testing.T) *tktStageFix {
	t.Helper()
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	f := &tktStageFix{root: root, verdicts: []string{tktApprove}}

	// One fake gateway for every stage, dispatching on the role's own MODEL and,
	// for the two integrator stages, on what the stage asked for. The model and not
	// the system prompt, because a stage's session is a CHILD and its Msgs[0] is
	// newChild's subagent prompt — the role description never appears in it. And
	// not the request count either: a fake that counted would break the moment a
	// retry or a rework round changed the order.
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		role, last := lastMsg(req)
		switch req.Model {
		case "coder-a":
			if role == "tool" {
				return fakeReply{content: "wrote done.txt"}
			}
			f.rounds++
			body := "the fix\n"
			if strings.Contains(last, "asked for changes") {
				body = "the fix, reworked\n"
			}
			return fakeReply{calls: []ToolCall{call("w", "write",
				map[string]any{"path": "done.txt", "content": body})}}
		case "rev-a":
			v := f.verdicts[min(max(f.rounds-1, 0), len(f.verdicts)-1)]
			return fakeReply{content: `{"verdict":"` + v +
				`","comments":[],"summary":"round ` + strconv.Itoa(f.rounds) + `"}`}
		}
		switch {
		case strings.Contains(last, "Write the merge request"):
			return fakeReply{content: `{"title":"BSK-1 accept bodies over 8k","description":"what changed, and how it was checked"}`}
		case strings.Contains(last, "A ticket has to be opened"):
			return fakeReply{content: `{"title":"the stand drops requests over 8k","body":"Anything over 8k comes back 400."}`}
		}
		return fakeReply{content: "Done: the change is on the branch, merged and proposed."}
	})

	f.h = newRoleHarnessAt(t, root, fs, tktRoles, true)
	// A name for the target branch that does not depend on this machine's git
	// default, and a bare remote that is a real second repository.
	gitT(t, root, "branch", "-M", "main")
	f.remote = t.TempDir()
	if real, err := filepath.EvalSymlinks(f.remote); err == nil {
		f.remote = real
	}
	gitT(t, f.remote, "init", "-q", "--bare")
	gitT(t, root, "remote", "add", "origin", f.remote)
	gitT(t, root, "push", "-q", "origin", "main")
	// Parked, which is what an unattended clone should be and what lets the target
	// branch be moved at all.
	gitT(t, root, "switch", "--detach", "-q", "HEAD")

	f.stub = newStubServer(t)
	f.pc = pipeFrom(t, stubPipeline)
	return f
}

// run builds a tktRun over the real world and walks it, the way runTicket does.
func (f *tktStageFix) run(t *testing.T, st *TicketState) (*tktRun, string, string) {
	t.Helper()
	cfg := f.h.orch.cfg
	repo, err := newTktGit(f.h.orch, func() string { return st.Ticket }, f.pc.Target)
	if err != nil {
		t.Fatal(err)
	}
	st.Version = tktStateVersion
	st.Rounds = f.pc.pipelineRounds()
	st.Target, st.Remote = f.pc.Target, f.pc.Remote
	if st.Branch == "" && st.Ticket != "" {
		st.Branch = tktBranchFor(f.pc, st.Ticket)
	}
	if st.Started == "" {
		st.Started = nowTS()
	}
	r := &tktRun{cfg: cfg, orch: f.h.orch, pc: f.pc, st: st, spine: tktSpine(),
		runID: f.h.orch.rec.id, dir: ticketDir(cfg, firstNonEmpty(st.Ticket, "new")),
		ctx: context.Background()}
	r.w = tktWorld{
		Tracker: tktTrackerCalls{call: f.stub, pc: f.pc},
		Forge:   tktForgeCalls{call: f.stub, pc: f.pc},
		Repo:    repo,
		Models:  &tktStageRun{orch: f.h.orch, lead: f.h.sess, pc: f.pc, cfg: cfg},
	}
	f.h.orch.setRunContext(r.ctx)
	status, reason := r.execute()
	return r, status, reason
}

func TestOneTicketGoesAllTheWayWithEveryTransitionMadeByLca(t *testing.T) {
	f := newTktStageFix(t)
	skipNo3Way(t, f.root)
	before := gitT(t, f.root, "rev-parse", "main")

	r, status, reason := f.run(t, &TicketState{Ticket: "BSK-1", State: tktStart})
	if status != statusPassed {
		t.Fatalf("%s: %s\n%s", status, reason, tktJournalText(r.st))
	}
	if r.st.State != tktReported {
		t.Fatalf("reached %q:\n%s", r.st.State, tktJournalText(r.st))
	}

	// The ticket was read, through the configured tool, and its summary is what
	// the coder was told to implement.
	if r.st.Summary != f.stub.summary {
		t.Fatalf("summary %q", r.st.Summary)
	}
	// The branch exists, lca cut it, and the work is on it as a COMMIT lca made —
	// no model was given a shell.
	if r.st.Branch != "agent/BSK-1" || r.st.Head == "" {
		t.Fatalf("branch %q head %q", r.st.Branch, r.st.Head)
	}
	if gitT(t, f.root, "rev-parse", "refs/heads/agent/BSK-1") != r.st.Head {
		t.Fatal("the recorded head is not the branch's")
	}
	if got := gitT(t, f.root, "show", "-s", "--format=%an", r.st.Head); got != "lca" {
		t.Fatalf("the commit was made by %q; the engine commits, not the model", got)
	}
	if !strings.Contains(gitT(t, f.root, "show", "--stat", "--format=", r.st.Head), "done.txt") {
		t.Fatal("the coder's file is not in the commit")
	}
	// The check ran and was green, and the verdict was read by review.go.
	if !r.st.checkGreen() || r.st.Check.Cmd != "test -f done.txt" {
		t.Fatalf("check %+v", r.st.Check)
	}
	if r.st.Verdict != tktApprove || r.st.Review == nil || r.st.Review.Summary == "" {
		t.Fatalf("verdict %q review %+v", r.st.Verdict, r.st.Review)
	}
	// The merge moved the target branch, with our head in it, and touched nothing
	// of the working tree.
	if r.st.MergedAt == "" || gitT(t, f.root, "rev-parse", "main") != r.st.MergedAt {
		t.Fatalf("merged %q, main is %s", r.st.MergedAt, gitT(t, f.root, "rev-parse", "main"))
	}
	if gitT(t, f.root, "rev-parse", "main") == before {
		t.Fatal("main did not move")
	}
	if st := gitT(t, f.root, "status", "--porcelain"); st != "" {
		t.Fatalf("the operator's tree was touched:\n%s", st)
	}
	// The push, the merge request and the comment.
	out, _, code := gitRun(f.root, nil, nil, "ls-remote", "--heads", "origin", "refs/heads/agent/BSK-1")
	if code != 0 || !strings.Contains(out, r.st.Head) {
		t.Fatalf("the branch is not on the remote at %s: %q", shortSha(r.st.Head), out)
	}
	if r.st.MergeRequest == nil || r.st.MergeRequest.URL == "" {
		t.Fatalf("merge request %+v", r.st.MergeRequest)
	}
	if n := len(f.stub.calls("fg__open_request")); n != 1 {
		t.Fatalf("%d merge requests", n)
	}
	mr := f.stub.mrs[0]
	if mr["from_ref"] != "agent/BSK-1" || mr["onto_ref"] != "main" {
		t.Fatalf("the branch and the target are lca's facts, not the model's: %v", mr)
	}
	if !strings.Contains(mr["headline"].(string), "8k") {
		t.Fatalf("the model wrote the title: %v", mr["headline"])
	}
	if len(f.stub.comments) != 1 || !strings.Contains(f.stub.comments[0], "lca-ticket:") {
		t.Fatalf("comments %v", f.stub.comments)
	}
	if f.stub.status != "Ready for review" {
		t.Fatalf("status %q", f.stub.status)
	}

	// And every state was recorded, in order, by a transition that said so.
	want := []string{tktOpened, tktBranched, tktImplemented, tktReviewed, tktMerged,
		tktPushed, tktProposed, tktReported}
	if got := strings.Join(states(r.st), " "); got != strings.Join(want, " ") {
		t.Fatalf("states:\n  %s\nwant\n  %s", got, strings.Join(want, " "))
	}
	for _, s := range r.st.Journal {
		if s.Started == "" || s.Finished == "" {
			t.Fatalf("%s has no two ends, so a crash between them is not detectable: %+v", s.Name, s)
		}
	}
	if r.st.Pending != "" {
		t.Fatalf("a finished run leaves no pending call: %q", r.st.Pending)
	}
}

// Re-running the same cron line is the resume. Nothing is cut twice, nothing is
// merged twice, nothing is proposed twice, and the ticket hears nothing new.
func TestTheSameCronLineRunAgainChangesNothing(t *testing.T) {
	f := newTktStageFix(t)
	skipNo3Way(t, f.root)
	r, status, reason := f.run(t, &TicketState{Ticket: "BSK-1", State: tktStart})
	if status != statusPassed {
		t.Fatalf("%s: %s", status, reason)
	}
	head, merged, mr := r.st.Head, r.st.MergedAt, r.st.MergeRequest.URL
	coderRuns := f.rounds

	for night := 2; night <= 3; night++ {
		again, status, reason := f.run(t, r.st)
		if status != statusPassed {
			t.Fatalf("night %d: %s: %s\n%s", night, status, reason, tktJournalText(again.st))
		}
		if again.st.Head != head || again.st.MergedAt != merged {
			t.Fatalf("night %d moved the work: head %q merged %q", night, again.st.Head, again.st.MergedAt)
		}
		if again.st.MergeRequest == nil || again.st.MergeRequest.URL != mr {
			t.Fatalf("night %d: merge request %+v", night, again.st.MergeRequest)
		}
		r = again
	}
	if f.rounds != coderRuns {
		t.Fatalf("the coder was bought again on a ticket that was already done: %d then %d", coderRuns, f.rounds)
	}
	if n := len(f.stub.calls("fg__open_request")); n != 1 {
		t.Fatalf("%d merge requests for one ticket", n)
	}
	if n := len(f.stub.calls("trk__note")); n != 1 {
		t.Fatalf("%d comments: a re-run that reached the same state has nothing new to say", n)
	}
	if n := strings.Count(gitT(t, f.root, "log", "--format=%s", "main"), "Merge agent/BSK-1"); n != 1 {
		t.Fatalf("%d merge commits on main", n)
	}
}

// request_changes goes back to the coder on the SAME session, and only as many
// times as the block allows.
func TestAReworkRoundContinuesTheCodersOwnSessionAndThenRuns(t *testing.T) {
	f := newTktStageFix(t)
	skipNo3Way(t, f.root)
	f.verdicts = []string{tktChanges, tktApprove}

	r, status, reason := f.run(t, &TicketState{Ticket: "BSK-1", State: tktStart})
	if status != statusPassed {
		t.Fatalf("%s: %s\n%s", status, reason, tktJournalText(r.st))
	}
	if r.st.Round != 1 {
		t.Fatalf("round %d, and the block allows one", r.st.Round)
	}
	if f.rounds != 2 {
		t.Fatalf("the coder ran %d times", f.rounds)
	}
	if r.st.Verdict != tktApprove {
		t.Fatalf("the second round was approved and the state says %q", r.st.Verdict)
	}
	// The reworked content is what merged, and the branch carries both attempts.
	wt := r.st.Worktree
	body, err := os.ReadFile(filepath.Join(wt, "done.txt"))
	if err != nil || !strings.Contains(string(body), "reworked") {
		t.Fatalf("the rework did not land: %q %v", body, err)
	}
	if n := strings.Count(gitT(t, f.root, "log", "--format=%s", "agent/BSK-1"), "coder: attempt"); n < 2 {
		t.Fatalf("the branch must carry one commit per attempt, and shows %d:\n%s", n,
			gitT(t, f.root, "log", "--format=%s", "agent/BSK-1"))
	}
	if len(r.st.Sessions) != 1 {
		t.Fatalf("a rework round continues the coder's own session, and %d were started: %v",
			len(r.st.Sessions), r.st.Sessions)
	}
}

// The gate the whole feature exists for, from the other side: request_changes
// with the rounds spent merges nothing, pushes nothing, proposes nothing, and
// tells the ticket.
func TestRequestChangesWithNoRoundLeftMergesNothingAndTellsTheTicket(t *testing.T) {
	f := newTktStageFix(t)
	skipNo3Way(t, f.root)
	f.verdicts = []string{tktChanges}
	before := gitT(t, f.root, "rev-parse", "main")

	r, status, _ := f.run(t, &TicketState{Ticket: "BSK-1", State: tktStart})
	if status != statusFailed {
		t.Fatalf("a reviewer that never approved must not produce a success: %s", status)
	}
	if r.st.BlockedAt != tktReviewed {
		t.Fatalf("blocked at %q:\n%s", r.st.BlockedAt, tktJournalText(r.st))
	}
	if gitT(t, f.root, "rev-parse", "main") != before {
		t.Fatal("something merged without an approve")
	}
	if _, _, code := gitRun(f.root, nil, nil, "ls-remote", "--heads", "--exit-code", "origin", "refs/heads/agent/BSK-1"); code == 0 {
		t.Fatal("something was pushed without an approve")
	}
	if n := len(f.stub.calls("fg__open_request")); n != 0 {
		t.Fatalf("%d merge requests without an approve", n)
	}
	if f.stub.status != "Needs a human" {
		t.Fatalf("the blocked status is the one the block names: %q", f.stub.status)
	}
	if len(f.stub.comments) != 1 {
		t.Fatalf("the ticket must be told: %v", f.stub.comments)
	}
	// The comment is written from facts lca supplied, and the run that wrote it
	// knew what was blocked.
	if !strings.Contains(tktOutcomeText(r.st), "BLOCKED at reviewed") {
		t.Fatalf("the stage was not told what was blocked:\n%s", tktOutcomeText(r.st))
	}
}

// -new: a model writes the ticket, lca opens it, and the work goes on under the
// key the tracker gave back.
func TestNewOpensTheTicketOnceAndThenWorksIt(t *testing.T) {
	f := newTktStageFix(t)
	skipNo3Way(t, f.root)
	f.stub.nextKey = "BSK-77"

	r2, status, reason := f.runNew(t, &TicketState{State: tktStart}, "the stand drops requests over 8k")
	if status != statusPassed {
		t.Fatalf("%s: %s\n%s", status, reason, tktJournalText(r2.st))
	}
	if r2.st.Ticket != "BSK-77" {
		t.Fatalf("the key came from the tracker, not from lca: %q", r2.st.Ticket)
	}
	if r2.st.Branch != "agent/BSK-77" {
		t.Fatalf("branch %q", r2.st.Branch)
	}
	if n := len(f.stub.created); n != 1 {
		t.Fatalf("%d tickets were opened", n)
	}
	if !strings.Contains(f.stub.created[0].Summary, "8k") {
		t.Fatalf("the model wrote the title: %q", f.stub.created[0].Summary)
	}
	if n := len(f.stub.calls("trk__open")); n != 1 {
		t.Fatalf("trk__open was called %d times — it is the one call that cannot be retried safely", n)
	}
}

func (f *tktStageFix) runNew(t *testing.T, st *TicketState, task string) (*tktRun, string, string) {
	t.Helper()
	cfg := f.h.orch.cfg
	repo, err := newTktGit(f.h.orch, func() string { return st.Ticket }, f.pc.Target)
	if err != nil {
		t.Fatal(err)
	}
	st.Version, st.Rounds = tktStateVersion, f.pc.pipelineRounds()
	st.Target, st.Remote = f.pc.Target, f.pc.Remote
	st.Started = nowTS()
	r := &tktRun{cfg: cfg, orch: f.h.orch, pc: f.pc, st: st, spine: tktSpine(), newTask: task,
		runID: f.h.orch.rec.id, dir: newTicketDir(cfg, task), ctx: context.Background()}
	r.w = tktWorld{
		Tracker: tktTrackerCalls{call: f.stub, pc: f.pc},
		Forge:   tktForgeCalls{call: f.stub, pc: f.pc},
		Repo:    repo,
		Models:  &tktStageRun{orch: f.h.orch, lead: f.h.sess, pc: f.pc, cfg: cfg},
	}
	f.h.orch.setRunContext(r.ctx)
	status, reason := r.execute()
	return r, status, reason
}

// A red check never reaches the reviewer, and the branch still shows what was
// tried — which is the whole value of a branch that outlives the run.
func TestARedCheckStopsBeforeTheReviewerAndLeavesTheAttemptOnTheBranch(t *testing.T) {
	f := newTktStageFix(t)
	// The check cannot pass: the coder writes done.txt, and this wants another file.
	f.h.orch.agents["coder"].CheckCmd = "test -f never.txt"

	r, status, _ := f.run(t, &TicketState{Ticket: "BSK-1", State: tktStart})
	if status != statusFailed {
		t.Fatalf("a red check is a failure: %s", status)
	}
	if r.st.BlockedAt != tktImplemented {
		t.Fatalf("blocked at %q:\n%s", r.st.BlockedAt, tktJournalText(r.st))
	}
	if r.st.Verdict != "" {
		t.Fatal("nothing may be reviewed on a red check")
	}
	if r.st.Head == "" {
		t.Fatal("the attempt must still be on the branch")
	}
	if !strings.Contains(gitT(t, f.root, "show", "--stat", "--format=", r.st.Head), "done.txt") {
		t.Fatal("...with what it tried in it")
	}
	if len(f.stub.comments) != 1 {
		t.Fatalf("the ticket must be told once: %v", f.stub.comments)
	}
}

// tktJournalText is the journal as a test failure prints it: without it, a
// failing arc says only which state it did not reach.
func tktJournalText(st *TicketState) string {
	var b strings.Builder
	for _, s := range st.Journal {
		b.WriteString("  " + s.Name + " -> " + firstNonEmpty(s.To, "-") + " " + s.Status + " " +
			firstLine(firstNonEmpty(s.Detail, s.Evidence)) + "\n")
	}
	return b.String()
}

// A recorded session id becomes a PATH, and state.json is a document a person
// can edit and a wrapper can overwrite.
func TestARecordedSessionIdNeverBecomesAPath(t *testing.T) {
	cfg := Config{Dir: "/tmp/x", StateDir: "/tmp/x"}
	for _, bad := range []string{"", "../../../etc/passwd", "a/b-t1", "..-t1", "x\x00-t1"} {
		if p, err := tktCoderTranscript(cfg, bad); err == nil {
			t.Fatalf("%q was accepted and became %q", bad, p)
		}
	}
	p, err := tktCoderTranscript(cfg, "20261003-035812-4412-t2")
	if err != nil {
		t.Fatal(err)
	}
	if want := childTranscriptPath("/tmp/x", "20261003-035812-4412", "t2"); p != want {
		t.Fatalf("a child's transcript is at %q, got %q", want, p)
	}
	// A primary's uid has no task suffix, and answers with the primary path.
	if p, err := tktCoderTranscript(cfg, "20261003-035812-4412"); err != nil ||
		p != transcriptPath(cfg, "20261003-035812-4412") {
		t.Fatalf("%q %v", p, err)
	}
}

// The rework round whose transcript is gone must still be told what the reviewer
// asked for: dropping it would throw away the findings the round exists for.
func TestAReworkRoundWithNoTranscriptIsStillGivenTheReview(t *testing.T) {
	f := newTktStageFix(t)
	skipNo3Way(t, f.root)
	f.verdicts = []string{tktChanges, tktApprove}

	// Round one, and then its transcript is removed, which is what a cleaned state
	// directory leaves behind.
	r, status, reason := f.run(t, &TicketState{Ticket: "BSK-1", State: tktStart})
	if status != statusPassed {
		t.Fatalf("%s: %s", status, reason)
	}
	if f.rounds != 2 || r.st.Round != 1 {
		t.Fatalf("rounds %d state round %d", f.rounds, r.st.Round)
	}
	if r.st.Coder == "" {
		t.Fatal("the coder's session must be recorded, or a resumed rework cannot continue it")
	}
	p, err := tktCoderTranscript(f.h.orch.cfg, r.st.Coder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the coder's transcript must be where a resume looks for it (%s): %v", p, err)
	}
}
