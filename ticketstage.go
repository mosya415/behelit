package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The five model stages of `lca ticket`: the only places in the command where a
// model is bought, and the only things it is asked for.
//
// Every one of them writes CONTENT — a ticket body, code, a review, a merge
// request description, a comment — and lca does something with it afterwards. No
// stage can reach a transition: the coder cannot commit (RunVerifiedAll commits,
// once per attempt, from the engine), nobody can push, nobody holds a write
// schema for the tracker or the forge. That last one is structural rather than a
// rule to remember: newChild appends `mcp_write * Deny` to every child, toolsFor
// drops a tool whose permission is Disabled, so a stage never SEES a write
// schema, cannot emit the call, and cannot be talked into one by injected ticket
// text. The one session that does hold those grants is lca's own (ticketmcp.go),
// and nothing sends it to a gateway.
//
// Each stage is a FRESH child except the coder's rework, and that asymmetry is
// the design:
//
//   - fresh, because the reviewer must not inherit the coder's account of its own
//     work, the one writing the comment must not inherit the reviewer's prose, and
//     a long pipeline that reused one session would grow its context without bound
//     and trigger a compaction in the middle of a merge;
//   - the same session for a rework round, because the model already knows what it
//     tried and the gateway's prefix cache makes round two cheap. That is what
//     `-session` exists for, and it is why TicketState carries the coder's uid.
//
// The system prompt is never touched for a stage, and the stage's own
// instructions and task go in the USER message. The request prefix is the
// gateway's KV cache key, and it has to stay byte-identical for a session's life
// — and identical between two runs of the same role, which is what makes the
// second ticket of the night cheaper than the first.

// tktStageRun is ticket.go's tktModels over the real engine: five stages, one
// orchestrator, one lead to be a child of.
type tktStageRun struct {
	orch *Orchestrator
	lead *Session
	pc   *PipelineConfig
	cfg  Config

	// coder is the live session of the current round, kept so a rework in THIS
	// process continues the conversation in memory instead of re-reading its own
	// transcript. A resume in a new process has no live session and loads the
	// transcript instead; both paths end up on the same history.
	coder *Session
}

// ── the ticket body (-new) ──────────────────────────────────────────────────

// WriteTicket turns the operator's one-liner into a ticket a person would
// recognise. lca then opens the ticket; the model never does.
//
// It asks for one JSON object because the two halves go to two different
// arguments of the create call and a prose reply would have to be split by
// guessing where the title ends.
func (m *tktStageRun) WriteTicket(ctx context.Context, in tktStageIn, task string) (TicketBody, error) {
	var b strings.Builder
	m.instruct(&b, in)
	fmt.Fprintf(&b, `A ticket has to be opened for this work, and you are writing it:

<task>
%s
</task>

Write it as this team writes tickets: a title somebody scanning a board can act
on, and a body that says what is wrong, what "done" means, and how it will be
verified. You are not implementing anything and you are not opening the ticket —
lca does that with what you write.

Your LAST message must be one JSON object and nothing else — no prose before it,
no prose after it:

{"title": "one line, no ticket key, no prefix",
 "body":  "the ticket's description, as many paragraphs as it needs"}
`, strings.TrimSpace(task))

	text, err := m.ask(ctx, tktStageTicket, m.pc.Integrator, b.String(), nil)
	if err != nil {
		return TicketBody{}, err
	}
	var out struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := tktReadJSON(text, &out); err != nil {
		return TicketBody{}, fmt.Errorf("the %s stage did not answer with the object it was asked for, so there is no ticket text to open a ticket with: %w", tktStageTicket, err)
	}
	if strings.TrimSpace(out.Title) == "" {
		return TicketBody{}, fmt.Errorf("the %s stage answered with an empty title, and lca will not name a ticket itself", tktStageTicket)
	}
	return TicketBody{Summary: strings.TrimSpace(out.Title), Body: strings.TrimSpace(out.Body)}, nil
}

// ── the implementation, and every rework round ──────────────────────────────

// Implement runs the coder in the ticket's own worktree, with the coder role's
// check_cmd over the result — the existing verifier loop (verify.go), the
// existing budgets (budget.go), the existing per-attempt commit (branch.go). No
// new engine, and no new way for a model to leave the tree.
//
// What it hands back is a COMMIT and a check verdict. The commit is the engine's:
// RunVerifiedAll calls commitWork once per attempt, so a failing round still
// leaves on the branch what it tried, which is the whole value of a branch that
// outlives the run.
func (m *tktStageRun) Implement(ctx context.Context, in tktStageIn) (tktCodeOut, error) {
	role := m.orch.agents[m.pc.Coder]
	if role == nil {
		return tktCodeOut{}, usageErrf("pipeline: roles: coder: %q is not a role in this roles.yaml", m.pc.Coder)
	}
	wt, err := m.worktreeOf(in)
	if err != nil {
		return tktCodeOut{}, err
	}
	sess, fresh, err := m.coderSession(in, role, wt)
	if err != nil {
		return tktCodeOut{}, err
	}
	m.coder = sess

	var b strings.Builder
	if fresh {
		m.instruct(&b, in)
		fmt.Fprintf(&b, `Implement ticket %s in this working tree.

<ticket summary>
%s
</ticket summary>

<ticket>
%s
</ticket>
`, in.Ticket.Key, in.Ticket.Summary, strings.TrimSpace(in.Ticket.Body))
		if check := m.checkCmd(role); check != "" {
			fmt.Fprintf(&b, "\nThe verifier will run `%s` over your result, and that command's exit code is the only thing that decides whether this round passed. Your own judgement does not count.\n", check)
		}
		b.WriteString(`
Change files. Do not commit, do not branch, do not merge and do not push: lca
commits what you leave in the tree, on a branch it made for this ticket, once per
attempt. There is no tool here that reaches the tracker or the forge.
`)
		// A fresh session in a round that is NOT the first: the transcript of the
		// round the reviewer read could not be loaded, so this model has no memory of
		// writing the code it is being asked to fix. The review goes in the message
		// anyway — dropping it would silently throw away the findings that are the
		// whole reason this round exists, and the code itself is in the tree where it
		// can be read.
		if in.Round > 0 {
			fmt.Fprintf(&b, "\nThe tree already holds an earlier attempt at this ticket, and a reviewer asked for changes to it. This is round %d of %d. Read what is there before you change it.\n\n%s\n",
				in.Round, in.State.Rounds, tktReviewText(in.Review))
		}
	} else {
		// A rework round, on the same session: the model is told what was asked for
		// and nothing else. Re-stating the ticket would cost the whole prefix and
		// tell it something it is already holding.
		fmt.Fprintf(&b, "A reviewer read your change and asked for changes. This is round %d of %d.\n\n%s\n\nFix what it names, in this same tree. Where you disagree with a comment, say so in your final message and leave that line alone — do not change something you believe is right to satisfy a reviewer.\n",
			in.Round, in.State.Rounds, tktReviewText(in.Review))
	}

	check := m.checkCmd(role)
	sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: b.String()})
	sess.view.Begin()
	start := time.Now()
	v := sess.RunVerifiedAll(ctx, tktCheckList(check), m.orch.verifyAttempts())
	sess.view.Finish(v.Status, time.Since(start))
	// Saved before anything can fail below: the next round continues this session
	// by reading this file, and a crash between the run and the save would cost
	// the conversation that round two is cheap because of.
	if err := sess.saveTranscript(); err != nil {
		warnLine("the coder's transcript could not be saved, so a resumed rework round would start a fresh session: %v", err)
	}
	m.orch.rec.Event("ticket_stage", map[string]any{"stage": tktStageCoder, "round": in.Round,
		"session": sess.UID, "status": v.Status, "attempts": v.Attempts, "exit": v.Exit})

	out := tktCodeOut{Session: sess.UID, Check: TicketCheck{Cmd: check, Attempts: v.Attempts,
		Tail: v.Tail, Logs: v.CheckLogs}}
	if v.Checked {
		exit := v.Exit
		out.Check.Exit = &exit
	}
	switch v.Status {
	case "error", "cancelled":
		// Not a red check and not a verdict: the gateway went away, a member went
		// away, the budget ran out. Handed back as an error so classifyRunErr decides
		// the row, and the ticket is left alone where that is the right answer.
		return out, firstErr(v.Err, fmt.Errorf("the coder stopped without a result: %s", orNone(v.Tail)))
	}
	// The commit the round left on the branch. Read from git and not from the
	// session, because the engine made it and the branch is the record.
	head, found, err := m.headOf(wt, in.Branch)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("nothing was committed on %s, so there is nothing to review: the coder left the tree unchanged", in.Branch)
	}
	out.Head = head
	return out, nil
}

// coderSession is the fresh child of round one, or the continuation of the SAME
// session for a rework round.
//
// A resume in a new process has no live session, so the transcript is read back
// the way `-session` reads one. When it is not there — a cleaned state directory,
// a round one that died before it could save — the round starts fresh and says
// so out loud rather than pretending to continue: a model told "fix what the
// reviewer named" with no memory of what it wrote would be guessing.
func (m *tktStageRun) coderSession(in tktStageIn, role *Agent, wt *worktree) (*Session, bool, error) {
	if in.Session != "" && m.coder != nil && m.coder.UID == in.Session {
		return m.coder, false, nil
	}
	sess, err := m.child(role, "the implementation of one ticket")
	if err != nil {
		return nil, false, err
	}
	if err := m.bindTree(sess, wt); err != nil {
		return nil, false, err
	}
	if in.Session == "" {
		return sess, true, nil
	}
	p, perr := tktCoderTranscript(m.cfg, in.Session)
	if perr != nil {
		warnLine("round %d cannot continue session %q (%v), so the coder starts fresh", in.Round, in.Session, perr)
		return sess, true, nil
	}
	n, err := resumeInto(&sess.Msgs, sessionMeta{path: p, id: in.Session})
	if err != nil {
		warnLine("round %d cannot continue session %s (%v), so the coder starts fresh and is told the ticket again", in.Round, in.Session, err)
		return sess, true, nil
	}
	sess.view.Note(fmt.Sprintf("SESSION %s — round %d continues it, %s restored", in.Session, in.Round, plural(n, "message", "messages")))
	return sess, false, nil
}

// tktCoderTranscript is where a coder round's transcript is. A stage session is a
// CHILD, so its transcript is under transcripts/subagents/<root>-<task>.json and
// not under transcripts/<uid>.json — and the uid is exactly `<root>-<task>`, so
// the path is derivable from it and nothing extra has to be recorded.
//
// The uid is validated first, and strictly, because it becomes a PATH and it
// comes out of a file: state.json is a document a person can edit and a wrapper
// can overwrite, and `coder_session: "../../../etc/passwd"` must be an error and
// not a read. The same refusal `-session` makes (sessions.go), for the same
// reason, and only the characters the recorder's own ids are made of pass.
func tktCoderTranscript(cfg Config, uid string) (string, error) {
	if uid == "" {
		return "", fmt.Errorf("no session is recorded")
	}
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return "", fmt.Errorf("that is not a session id")
		}
	}
	if strings.Contains(uid, "..") {
		return "", fmt.Errorf("that is not a session id")
	}
	if i := strings.LastIndex(uid, "-t"); i > 0 {
		return childTranscriptPath(cfg.stateDir(), uid[:i], uid[i+1:]), nil
	}
	return transcriptPath(cfg, uid), nil
}

// ── the review ──────────────────────────────────────────────────────────────

// Review judges the branch's diff through review.go's own machinery, so the
// verdict is the same structured object, validated the same way, with the same
// one retry and the same rule: unreadable is failed, never a silent approve.
//
// The base is the commit this run CUT the branch at and not the target branch's
// tip. They differ the moment anything lands on the target while the coder works,
// and diffing against the tip puts somebody else's commits in front of the
// reviewer — which produces correctly-placed comments on work this ticket never
// touched, and then posts them on a merge request.
// ── the one supervised attempt at a conflict ────────────────────────────────

// Resolve gives the integrator the merge MergeTargetIn left in the ticket's own
// worktree: the markers, the unmerged index, and the coder's own check over
// whatever it writes.
//
// It rides the ordinary path and adds nothing to it — the same jail rooted at
// the ticket's worktree, the same sandbox, the same per-attempt commitWork,
// which refuses to commit a tree that still holds `<<<<<<<` and so makes "did
// not finish" a thing lca learns here rather than on a shared branch. No
// permission gate of its own, and that is deliberate: a delegation's resolution
// needed one because it was written into the OPERATOR'S files, while this one
// lands on a branch lca cut, in front of the same three gates as the code it is
// reconciling — a green check, a reviewer, and a merge lca performs.
//
// A FRESH child and never the coder's session, which is the opposite of a rework
// round. A rework continues the coder because the model is being asked to finish
// its own thought; a resolution is being asked to hold two intentions at once,
// and the one thing it must not do is arrive already committed to one of them.
func (m *tktStageRun) Resolve(ctx context.Context, in tktStageIn) (tktCodeOut, error) {
	role := m.orch.agents[m.pc.Integrator]
	if role == nil {
		return tktCodeOut{}, usageErrf("pipeline: roles: integrator: %q is not a role in this roles.yaml", m.pc.Integrator)
	}
	// The CODER's check and not the integrator's: the question a resolution has to
	// answer is the same question the implementation answered — is this ticket
	// done — and a pipeline whose conflict path is verified by a different command
	// than its code path has two definitions of done.
	check := m.checkCmd(m.orch.agents[m.pc.Coder])
	wt, err := m.worktreeOf(in)
	if err != nil {
		return tktCodeOut{}, err
	}
	sess, err := m.child(role, "one stage of a ticket")
	if err != nil {
		return tktCodeOut{}, err
	}
	if err := m.bindTree(sess, wt); err != nil {
		return tktCodeOut{}, err
	}

	var b strings.Builder
	m.instruct(&b, in)
	fmt.Fprintf(&b, `You are in a git worktree holding an UNFINISHED MERGE. Ticket %s was
implemented on %s, its check passed and a reviewer approved it — and then %s,
the branch it is going to merge into, moved underneath it. %s has just been
merged into this branch and the two sides touched the same region.

Conflicted files:
  %s

The files contain conflict markers: the part under <<<<<<< HEAD is this
ticket's own approved change, and the part under ======= down to >>>>>>> is
what %s has grown since this branch was cut. Resolve every one of them so that
BOTH intentions survive — this ticket's change AND what the target branch now
does. Delete every marker. Then stop.

Do not undo this ticket's work to make the conflict go away. Taking the target
branch's side of every file would pass the check and land nothing, and lca
compares the result against the approved diff: a resolution that drops this
ticket's change is refused and handed to a person.

`, in.Ticket.Key, in.Branch, in.Target, in.Target,
		strings.Join(strings.Split(orNone(in.Conflict.Files), ", "), "\n  "), in.Target)
	if check != "" {
		fmt.Fprintf(&b, "Definition of done: `%s` exits 0 in this tree. A verifier runs it after you stop; its exit code decides, not your judgement.\n", check)
	} else {
		b.WriteString("There is no check command, so leave the tree so that it builds.\n")
	}
	b.WriteString(`
Do not commit, do not branch, do not merge anything else and do not push: lca
commits what you leave in the tree. There is no tool here that reaches the
tracker or the forge.

This is the ONLY attempt. If the check does not pass, or the merge is not
finished, or this ticket's change does not survive it, the conflict goes to a
person and nothing is merged.
`)

	sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: b.String()})
	sess.view.Begin()
	start := time.Now()
	// ONE, the literal number, and not o.verifyAttempts() — branch.go's reason
	// word for word: verify_attempts is the team's budget for a CODER fixing its
	// own work against a check it can read, and feeding a failed check back to a
	// third role so it can rewrite a merge it did not write, twice, is the
	// silent-overwrite failure wearing a hat. The prompt above says "the only
	// attempt", and a number an operator could raise in roles.yaml would make that
	// sentence a lie.
	v := sess.RunVerifiedAll(ctx, tktCheckList(check), 1)
	sess.view.Finish(v.Status, time.Since(start))
	if err := sess.saveTranscript(); err != nil {
		warnLine("the integrator's transcript could not be saved: %v", err)
	}
	m.orch.rec.Event("ticket_stage", map[string]any{"stage": tktStageResolve, "round": in.Round,
		"session": sess.UID, "status": v.Status, "attempts": v.Attempts, "exit": v.Exit})

	out := tktCodeOut{Session: sess.UID, Check: TicketCheck{Cmd: check, Attempts: v.Attempts,
		Tail: v.Tail, Logs: v.CheckLogs}}
	if v.Checked {
		exit := v.Exit
		out.Check.Exit = &exit
	}
	// The head is read BEFORE the status is judged, which Implement does not need
	// to do and this does: doResolve tells "nothing was committed, so nothing was
	// resolved" from "a resolution exists and nothing judged it" by whether there
	// is a commit, and those two go to different places — one is the integrator
	// not managing it, the other is a gateway or a clock that stopped mid-attempt.
	if head, found, herr := m.headOf(wt, in.Branch); herr == nil && found && head != in.Conflict.Base {
		out.Head = head
	}
	switch v.Status {
	case "error", "cancelled":
		return out, firstErr(v.Err, fmt.Errorf("the %s stage stopped without a result: %s", tktStageResolve, orNone(v.Tail)))
	}
	return out, nil
}

func (m *tktStageRun) Review(ctx context.Context, in tktStageIn) (*reviewReport, error) {
	role := m.orch.agents[m.pc.Reviewer]
	if role == nil {
		return nil, usageErrf("pipeline: roles: reviewer: %q is not a role in this roles.yaml", m.pc.Reviewer)
	}
	wt, err := m.worktreeOf(in)
	if err != nil {
		return nil, err
	}
	base := firstNonEmpty(in.State.BranchAt, in.Target)
	cfg := m.cfg
	cfg.Root = wt.root
	rr, err := prepareReview(cfg, base)
	if err != nil {
		// prepareReview words its refusals for -diff-base, which is the flag a person
		// would have typed. Here nobody typed anything, so the sentence is placed:
		// what was being reviewed, and against what.
		return nil, usageErrf("reviewing %s against %s: %v", in.Branch, shortSha(base), err)
	}
	prev := m.orch.review
	m.orch.review = rr
	defer func() { m.orch.review = prev }()

	sess, err := m.child(role, "the review of one ticket's branch")
	if err != nil {
		return nil, err
	}
	if err := m.bindTree(sess, wt); err != nil {
		return nil, err
	}
	// The reviewer reads the tree and runs what convinces it; it does not write to
	// it. isolationRules would have granted edit and run wholesale, so the branch
	// is pinned off this session: a reviewer that edited the code it is judging has
	// reviewed something nobody else will see.
	//
	// RefreshSystem again after the rule, and that is not belt and braces: Msgs[0]
	// MEMOISES the tool schemas, so a deny appended afterwards leaves the edit
	// tools advertised in the prefix and refuses them only at call time. The
	// reviewer would then spend an attempt discovering it cannot do the thing it
	// was shown. promptStep's note in workflow.go is the same fix for the same
	// reason, from the granting side.
	sess.branch, sess.wt = "", nil
	sess.extra = append(sess.extra, Rule{"edit", "*", Deny})
	sess.RefreshSystem()

	var b strings.Builder
	m.instruct(&b, in)
	fmt.Fprintf(&b, "You are reviewing the change made for ticket %s.\n\n<ticket summary>\n%s\n</ticket summary>\n\n<ticket>\n%s\n</ticket>\n\nThe check `%s` already passed on this change — a green check is what let it reach you, so there is no point confirming it. Judge whether the change does what the ticket asks and whether it is correct.\n",
		in.Ticket.Key, in.Ticket.Summary, strings.TrimSpace(in.Ticket.Body), orNone(in.Check.Cmd))

	// review.go's own task message, with the diff and the reply contract, below
	// everything above: the prefix stays this role's prefix.
	sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: rr.taskMessage(b.String())})
	sess.view.Begin()
	m.orch.rec.Event("review_diff", map[string]any{"base": rr.base, "from": rr.from,
		"diff_bytes": len(rr.diff), "files": rr.lines.order})
	start := time.Now()
	v := sess.RunVerified(ctx, "", 1)
	sess.view.Finish(v.Status, time.Since(start))
	if v.Status != "error" && v.Status != "cancelled" {
		m.orch.finishReview(ctx, sess, &v)
	}
	sess.saveTranscript()
	m.orch.rec.Event("ticket_stage", map[string]any{"stage": tktStageReviewer, "round": in.Round,
		"session": sess.UID, "status": v.Status, "verdict": tktVerdictOf(v.Review)})
	if v.Status == "error" || v.Status == "cancelled" {
		return nil, firstErr(v.Err, fmt.Errorf("the reviewer stopped without a verdict: %s", orNone(v.Tail)))
	}
	// nil, deliberately, when nothing could be read. The gate reads Verdict, so an
	// unreadable review behaves as "not approved" with no second branch to forget
	// — and the reason is on the record above.
	if v.Review == nil {
		warnLine("the reviewer's verdict could not be read (%s), which is not an approval", orNone(v.ReviewErr))
	}
	return v.Review, nil
}

func tktVerdictOf(rep *reviewReport) string {
	if rep == nil {
		return "unreadable"
	}
	return rep.Verdict
}

// ── the merge request's description ─────────────────────────────────────────

// WriteMR writes what a reviewer on the forge will read. lca finds or creates the
// merge request itself, with these two strings as two of the arguments the block
// named.
func (m *tktStageRun) WriteMR(ctx context.Context, in tktStageIn) (TicketMR, error) {
	var b strings.Builder
	m.instruct(&b, in)
	fmt.Fprintf(&b, `Write the merge request for ticket %s.

<ticket summary>
%s
</ticket summary>

<ticket>
%s
</ticket>

<branch>%s → %s, at commit %s</branch>

<how it was verified>
%s
</how it was verified>

<the review it passed>
%s
</the review it passed>

Describe what changed and why, how it was verified, and anything a reviewer
should look at first. Say what was left undone if anything was. Invent nothing:
if you do not know something, leave it out rather than writing a plausible
sentence about it.

Your LAST message must be one JSON object and nothing else:

{"title": "one line", "description": "markdown, as long as it needs to be"}
`, in.Ticket.Key, in.Ticket.Summary, strings.TrimSpace(in.Ticket.Body),
		in.Branch, in.Target, shortSha(in.State.Head), tktCheckText(in.Check), tktReviewText(in.Review))

	text, err := m.ask(ctx, tktStageMR, m.pc.Integrator, b.String(), nil)
	if err != nil {
		return TicketMR{}, err
	}
	var out struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := tktReadJSON(text, &out); err != nil {
		return TicketMR{}, fmt.Errorf("the %s stage did not answer with the object it was asked for: %w", tktStageMR, err)
	}
	if strings.TrimSpace(out.Title) == "" {
		return TicketMR{}, fmt.Errorf("the %s stage answered with an empty title, and lca will not name a merge request itself", tktStageMR)
	}
	return TicketMR{Title: strings.TrimSpace(out.Title), Body: strings.TrimSpace(out.Description),
		Branch: in.Branch, Target: in.Target, Ticket: in.Ticket.Key, Summary: in.Ticket.Summary}, nil
}

// ── the comment that goes back on the ticket ────────────────────────────────

// WriteComment writes the one thing a person will actually read tomorrow
// morning. It is asked for on BOTH paths — the run that finished and the run that
// stopped at a gate — because an unattended pipeline that goes quiet is worse
// than one that fails loudly.
//
// The reply is prose, not an object: it goes into one argument, whole, and asking
// a model to JSON-escape a page of markdown is a way to lose the last paragraph.
func (m *tktStageRun) WriteComment(ctx context.Context, in tktStageIn) (string, error) {
	st := in.State
	var b strings.Builder
	m.instruct(&b, in)
	fmt.Fprintf(&b, `Write the comment that goes on ticket %s. It is the only thing anybody will read
about this run.

<ticket summary>
%s
</ticket summary>

<what happened>
%s
</what happened>

<how it was verified>
%s
</how it was verified>

<the review>
%s
</the review>

Say, in this order: what changed, how it was verified (the command and its exit
code), where the merge request is if there is one, and what was left undone or
could not be finished. Put the blocked part FIRST if something is blocked.

Invent nothing. If a fact is not above, it is not known: say that instead of
writing a plausible sentence. Answer with the comment itself — no preamble, no
JSON, no closing offer of help.
`, st.Ticket, in.Ticket.Summary, tktOutcomeText(st), tktCheckText(in.Check), tktReviewText(in.Review))

	text, err := m.ask(ctx, tktStageReport, m.pc.Integrator, b.String(), nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("the %s stage answered with nothing, and lca will not write a ticket comment itself", tktStageReport)
	}
	return strings.TrimSpace(text), nil
}

// tktOutcomeText is what the run DID, as facts and not as prose: the states it
// reached, the gate that stopped it, the branch, the merge and the merge request.
// Written by lca from the journal so that the one thing a model cannot get wrong
// is what happened.
func tktOutcomeText(st *TicketState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "the work stands at: %s\n", st.workState())
	fmt.Fprintf(&b, "branch: %s", orNone(st.Branch))
	if st.Head != "" {
		fmt.Fprintf(&b, " at %s", shortSha(st.Head))
	}
	b.WriteString("\n")
	if st.MergedAt != "" {
		fmt.Fprintf(&b, "merged into %s as %s\n", st.Target, shortSha(st.MergedAt))
	} else {
		fmt.Fprintf(&b, "not merged into %s\n", st.Target)
	}
	if st.PushedSha != "" {
		fmt.Fprintf(&b, "pushed to %s as %s\n", st.Remote, shortSha(st.PushedSha))
	}
	// The conflict, where there was one. It is a fact the journal holds and the
	// comment is the only place anybody will see it: a target branch that moved
	// under a finished change is the one thing in this arc that a person may have
	// to finish by hand, and a comment that says "not merged" without saying why
	// sends them to read a diff to find out.
	if st.Resolve != nil {
		fmt.Fprintf(&b, "merge conflict: %s\n", tktResolveText(st.Resolve))
	}
	if st.MergeRequest != nil {
		fmt.Fprintf(&b, "merge request: %s\n", orNone(firstNonEmpty(st.MergeRequest.URL, st.MergeRequest.ID)))
	} else {
		b.WriteString("merge request: none\n")
	}
	fmt.Fprintf(&b, "rework rounds used: %d of %d\n", st.Round, st.Rounds)
	if st.Blocked != "" {
		fmt.Fprintf(&b, "BLOCKED at %s: %s\n", orNone(st.BlockedAt), st.Blocked)
	}
	for _, s := range st.Journal {
		if s.Status == tktFailed || s.Status == tktBlocked {
			fmt.Fprintf(&b, "%s: %s — %s\n", s.Name, s.Status, firstLine(orNone(s.Detail)))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// tktResolveText is the conflict and what became of the one attempt at it, as
// the ticket comment carries it. Facts only, in lca's own words, because the
// stage that writes the comment is told to invent nothing and this is the part
// of the run it has no other way of knowing about.
func tktResolveText(res *TicketResolve) string {
	what := map[string]string{
		tktResolveDone:    "the integrator merged " + shortSha(res.Target) + " in and resolved it as " + shortSha(res.Head) + "; the check ran again over the result and a reviewer read the resolution",
		tktResolveNone:    "the integrator did not resolve it, so nothing was merged",
		tktResolveRed:     "the integrator resolved it and the check over the result was red, so nothing was merged",
		tktResolveDropped: "the integrator resolved it by dropping this ticket's own change to " + orNone(res.Lost) + ", which lca refuses, so nothing was merged",
	}[res.Outcome]
	if what == "" {
		what = "the run stopped inside the one supervised attempt at it, so nothing judged what was left and nothing was merged"
	}
	out := fmt.Sprintf("%s collided with the target branch (%s) — %s", orNone(res.Files), shortSha(res.Target), what)
	if res.Kept != "" {
		out += ". The rejected attempt is kept at " + res.Kept
	}
	return out
}

// tktCheckText is the check as a fact, and the tail with it: the tail is what
// says WHY a red check is red, and a comment that says "the tests failed" without
// it sends a person to go and find the log.
//
// SCRUBBED here, and that is the one place in this file where the rule differs
// from the verifier's. RunVerifiedAll deliberately hands the coder the tail
// unredacted, because the model is being asked to fix the cause and a
// [redacted] where the cause was makes it spend every attempt discovering a hole
// (verify.go says so at length). These two stages are not fixing anything: they
// are writing text that ends up on a ticket and on a forge, so this is a write
// boundary, and a stand's output full of a token nobody meant to publish must
// not reach a model that is about to paraphrase it into a public comment.
// doReport and doPropose scrub again at the call itself, because a sink is where
// this belongs and a model is not one.
func tktCheckText(c TicketCheck) string {
	if c.Cmd == "" {
		return "nothing checked this change: no check command is configured for the coder role"
	}
	if c.Exit == nil {
		return fmt.Sprintf("`%s` never got to run", c.Cmd)
	}
	out := fmt.Sprintf("`%s` exited %d after %s", c.Cmd, *c.Exit, plural(c.Attempts, "attempt", "attempts"))
	if *c.Exit != 0 && strings.TrimSpace(c.Tail) != "" {
		out += "\n" + redactSecrets(c.Tail)
	}
	return out
}

// tktReviewText is the verdict and its comments as the next stage reads them.
// The same rendering a person gets (review.go's reviewLines), because the one
// thing a rework round must not do is read a different review from the one in
// the result object.
func tktReviewText(rep *reviewReport) string {
	if rep == nil {
		return "no verdict could be read, which is not an approval"
	}
	lines := []string{"verdict: " + rep.Verdict}
	if strings.TrimSpace(rep.Summary) != "" {
		lines = append(lines, "summary: "+strings.TrimSpace(rep.Summary))
	}
	for _, c := range rep.Comments {
		lines = append(lines, fmt.Sprintf("%s:%d  %s  %s", c.File, c.Line, c.Severity, strings.TrimSpace(c.Body)))
	}
	return strings.Join(lines, "\n")
}

// ── the shared parts ────────────────────────────────────────────────────────

// instruct puts the stage's NAMED skills at the top of its task message — the
// team's shared knowledge base reaching the one stage it was named for. First,
// because an instruction a model reads after the task is an instruction it reads
// as an afterthought, and because this is how a stage running at 3am is given
// what a person in the room would have said.
func (m *tktStageRun) instruct(b *strings.Builder, in tktStageIn) {
	if strings.TrimSpace(in.Instructions) == "" {
		return
	}
	b.WriteString(in.Instructions)
	b.WriteString("\n\n")
}

// ask is a one-shot stage: a fresh child of that role, one user message, no
// check, the reply as text. Used by the three stages that produce prose or a
// small object and touch no tree.
func (m *tktStageRun) ask(ctx context.Context, stage, roleName, task string, wt *worktree) (string, error) {
	role := m.orch.agents[roleName]
	if role == nil {
		return "", usageErrf("pipeline: roles: %q is not a role in this roles.yaml", roleName)
	}
	sess, err := m.child(role, "one stage of a ticket")
	if err != nil {
		return "", err
	}
	if wt != nil {
		if err := m.bindTree(sess, wt); err != nil {
			return "", err
		}
	}
	// These stages write text. They may read the tree to do it — a merge request
	// description that names files has to have looked at them — and they may not
	// change it: the change was made, checked and reviewed three states ago, and an
	// edit here would be unreviewed work riding into the merge request that
	// describes something else. RefreshSystem for the reason Review gives: a deny
	// appended after Msgs[0] was built is a tool still advertised in the prefix.
	// …and they run nothing and delegate nothing. These three stages are handed
	// every fact they are asked to write about — tktOutcomeText, tktCheckText and
	// tktReviewText compose them from the journal, and doReport falls back to the
	// first of them when no model can be bought at all — so a paragraph is the
	// whole job, and `run_command` and `task` buy a child process and whole
	// subagent sessions for it. The authority half matters more than the spend:
	// these stages are the ones called with no worktree, so bindTree never ran and
	// sharedRefDenies were never appended, and an unattended run trusts what it is
	// asked — which left `git push` inside a stage that exists to write prose.
	// A prose stage reads: read_file, grep, glob, list_dir. Nothing else.
	sess.extra = append(sess.extra, Rule{"edit", "*", Deny}, Rule{"run", "*", Deny}, Rule{"task", "*", Deny})
	sess.RefreshSystem()
	sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: task})
	sess.view.Begin()
	start := time.Now()
	v := sess.RunVerified(ctx, "", 1)
	sess.view.Finish(v.Status, time.Since(start))
	sess.saveTranscript()
	m.orch.rec.Event("ticket_stage", map[string]any{"stage": stage, "role": roleName,
		"session": sess.UID, "status": v.Status})
	if v.Status == "error" || v.Status == "cancelled" {
		return "", firstErr(v.Err, fmt.Errorf("the %s stage stopped without an answer: %s", stage, orNone(v.Tail)))
	}
	return lastAssistantText(sess), nil
}

// child is one stage's session. newChild and not NewPrimary: NewPrimary hands
// every session UID = rec.id, so several stages of one run would collide in the
// trace and in the transcripts — and the per-stage uid is what -session resumes
// and what the result object reports.
//
// The description is one constant for every stage and not the stage's name, for
// promptStep's reason: newChild puts it in the SYSTEM prompt, so a stage name
// there would move bytes near the front of the prefix and cost the gateway's KV
// cache the whole system message on every stage after the first.
func (m *tktStageRun) child(role *Agent, desc string) (*Session, error) {
	sess, err := m.orch.newChild(m.lead, role, desc)
	if err != nil {
		return nil, usageErrf("%v", err)
	}
	// No delegation out of a stage, whatever roles.yaml says about this role. A
	// ticket's roles are the three the pipeline: block named and validate checked;
	// a stage that can reach a fourth is a stage whose authority that block does
	// not describe — and the fourth role's member:, sandbox and rules come with it.
	sess.extra = append(sess.extra, Rule{"delegate", "*", Deny})
	// Not resumable by task id: a stage is reached once per round and the state
	// file is what a resume reads. The session itself lives as long as this call.
	m.orch.forgetChild(sess.ID)
	return sess, nil
}

// bindTree points a session at the ticket's worktree: its jail, its branch and
// the worktree the engine commits from.
//
// isolated, like a delegation's subagent: the ticket's worktree IS a scratch
// copy, editing and running sandboxed commands in it is the job, and what gates
// it reaching anybody is the check, the review and the merge — three gates lca
// holds, none of which a model can reach.
func (m *tktStageRun) bindTree(sess *Session, wt *worktree) error {
	jl, err := NewJail(wt.root, m.orch.jl.Allowed, m.orch.jl.Unsafe)
	if err != nil {
		return usageErrf("%v", err)
	}
	jl.Shell, jl.Member = m.orch.jl.Shell, m.orch.jl.Member
	sess.jl = jl
	sess.isolated = true
	sess.branch, sess.wt = wt.branch, wt
	// And the half isolated does NOT give: isolationRules rates the shared-ref git
	// commands Ask, which is a guard with a keyboard behind it, and this command
	// has no keyboard — newTicketRun calls TrustAll() on purpose, so an Ask here is
	// an auto-yes and a stage could push the branch itself. The three gates named
	// above are the ones a model cannot reach only if the commands that reach past
	// them are DENIED, so they are, in the same words Review and ask use for edit.
	// extra is last in rules(), so this beats isolationRules' `run: * Allow`.
	sess.extra = append(sess.extra, sharedRefDenies...)
	// After the jail and the branch and before the task message: RefreshSystem
	// rebuilds Msgs[0] from this role's own system prompt, which is the prefix the
	// gateway caches, and the environment block in it names the tree.
	sess.RefreshSystem()
	return nil
}

// worktreeOf is the ticket's worktree as a *worktree the engine understands. It
// is rebuilt from the recorded path on every stage rather than carried, because a
// resumed run has only the path — and a path that is no longer a worktree is a
// thing to find out here, where it can be said plainly, rather than inside a
// commit three minutes later.
func (m *tktStageRun) worktreeOf(in tktStageIn) (*worktree, error) {
	root := strings.TrimSpace(in.Worktree)
	if root == "" {
		return nil, usageErrf("ticket %s has no worktree recorded, so there is nowhere to work", in.Ticket.Key)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%s is not there any more, so the ticket's worktree is gone: %v", root, err)
	}
	top, err := gitCmd(root, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git worktree any more: %w", root, err)
	}
	sub := "."
	if p, err := gitCmd(root, nil, nil, "rev-parse", "--show-prefix"); err == nil {
		sub = normSub(p)
	}
	// dir is the worktree's own root and root is the project inside it, which is
	// the same distinction a delegation's worktree makes: commitWork's pathspec
	// confines the commit to sub, so a change above the project's directory never
	// reaches the branch.
	dir := root
	if sub != "." && sub != "" {
		dir = strings.TrimSuffix(filepath.Clean(root), string(filepath.Separator)+filepath.FromSlash(sub))
	}
	return &worktree{top: strings.TrimSpace(top), dir: dir, root: root, sub: sub,
		base: in.State.BranchAt, branch: in.Branch, ctx: m.orch.runContext()}, nil
}

// headOf is the commit the round left on the branch, read from the repository.
func (m *tktStageRun) headOf(wt *worktree, branch string) (string, bool, error) {
	out, _, code := gitRun(wt.top, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch+"^{commit}")
	if code != 0 {
		if code < 0 {
			return "", false, fmt.Errorf("git could not be run in %s", wt.top)
		}
		return "", false, nil
	}
	return strings.TrimSpace(out), true, nil
}

// checkCmd is the coder role's own check_cmd, which is where a team writes "what
// proves this ticket is done". There is deliberately no flag to override it: a
// pipeline whose verifier can be chosen per call has a verifier nobody can audit.
func (m *tktStageRun) checkCmd(role *Agent) string {
	if role == nil {
		return ""
	}
	return strings.TrimSpace(role.CheckCmd)
}

// nonEmpty is the check list RunVerifiedAll wants: one check, or none at all. An
// empty string in the slice would have it run “ and call the exit code a verdict.
func tktCheckList(check string) []string {
	if strings.TrimSpace(check) == "" {
		return nil
	}
	return []string{check}
}

// tktReadJSON reads a stage's object out of its last message, through review.go's
// own extractor: a model that wrapped the object in a fence or wrote a sentence
// after it has still answered, and refusing that would cost a run over
// punctuation.
func tktReadJSON(text string, into any) error {
	raw, err := extractJSONObject(text)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), into)
}
