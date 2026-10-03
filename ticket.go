package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// `lca ticket` — one ticket, from the tracker to a merge request, unattended.
//
// ── why this is a command and not a workflow file ───────────────────────────
//
// `lca run` executes a LIST of steps. This is a STATE MACHINE: it has gates, it
// has idempotency, and it has a resume that must never produce a second merge
// request for one ticket. A list cannot express "the branch already exists and
// the commit I cut it at is still in it, so that step is done", because the
// answer lives on a remote server and in a git object database, not in a
// cursor.
//
// And the boundary that makes it safe is not the list of steps either. It is
// that every state transition — branch, commit, merge, push, merge request,
// ticket comment — is performed by LCA'S OWN CODE at a step boundary, after its
// gate passed, and never by a model holding a shell. The pipeline profile says
// why in one line: "an agent that pushes has taken a decision nobody reviewed".
// A model writes CONTENT here — the ticket body, the code, the review, the
// merge request description, the comment — and nothing else. There is no
// transition a model can reach by typing a command.
//
// ── why it is spelled `lca ticket` ──────────────────────────────────────────
//
// The obvious name, `lca pipeline`, is taken twice over. "The pipeline" in this
// program already means the operator's own deterministic cron wrapper (the
// whole of oneshot.go's contract is written to it), and "the pipeline profile"
// already means examples/pipeline.roles.yaml — the sandbox an unattended agent
// runs under. A third meaning would make every sentence in the README ambiguous
// at the exact moment a reader needs it to be precise.
//
// `ticket` is also simply the truer name. The argument is a ticket key, the
// state file is per ticket, the lock is per ticket, and the idempotency the
// whole design turns on is "one ticket, one branch, one merge request". It
// stands beside `lca run <name>` and `lca merge <branch>`: a noun naming the
// thing worked on. The CONFIGURATION block stays `pipeline:` — it describes the
// team's delivery pipeline, which is a different noun from the unit of work.
//
//	lca ticket BSK-123              work that ticket
//	lca ticket -new "<one-liner>"   open a ticket first, then work it
//	lca ticket BSK-123 -dry-run     every transition, its gate, its skills
//	lca ticket BSK-123 -json        one result object on stdout
//
// There is deliberately no -resume. Re-running the command IS the resume: the
// state file is found by ticket key, every transition re-proves its own effect
// against the world, and the ones already done are skipped. A pipeline that
// needs a different flag after a crash is a pipeline that does the wrong thing
// at 3am, because the cron line is the same line either way.
//
// ── the shared knowledge base is part of this ───────────────────────────────
//
// The run is unattended, so the knowledge a human would bring — how this team
// deploys to the stand, what a merge request description has to contain here,
// what the reviewer always checks — has to come from somewhere. That somewhere
// is the shared skills repository (LCA_SKILLS, skills.go), and the pipeline:
// block names skills PER STAGE: which the coder gets, which the reviewer gets,
// which the one writing the merge request description gets. Named, never left
// to the model to pick: a stage that runs at 3am must not depend on a model
// noticing an index line. See ticketcfg.go for the resolution and the hashing.

// tktStateVersion is the state file's own version. A state written by another
// build is refused rather than guessed at, because a resume reads this document
// to decide whether a merge request already exists.
const tktStateVersion = 1

// The states, as the operator's arc. Each one is either a transition lca
// performs itself, or a model call whose output lca then acts on.
const (
	tktStart       = ""            // nothing recorded yet
	tktOpened      = "opened"      // the ticket exists (read, or created from -new)
	tktBranched    = "branched"    // a branch and a worktree for this ticket exist
	tktImplemented = "implemented" // the coder changed files and its check passed
	tktReviewed    = "reviewed"    // the reviewer produced a structured verdict
	tktReworked    = "reworked"    // request_changes fed back on the same session
	tktMerged      = "merged"      // the branch merged into the target branch
	tktPushed      = "pushed"      // the branch is on the remote
	tktProposed    = "proposed"    // a merge request exists
	tktReported    = "reported"    // the ticket carries a comment, and its status moved
)

// The transition names. They are the journal's keys and -dry-run's rows, so they
// are constants and not literals typed twice.
const (
	tktOpen      = "open"
	tktBranch    = "branch"
	tktImplement = "implement"
	tktReview    = "review"
	tktRework    = "rework"
	tktMerge     = "merge"
	tktPush      = "push"
	tktPropose   = "propose"
	tktReport    = "report"
)

// The verdicts review.go produces. Repeated here as constants because the gate
// that decides whether anything merges compares against them.
const (
	tktApprove = "approve"
	tktChanges = "request_changes"
)

// The journal's outcome words, the same three a workflow step uses, plus the one
// a state machine needs that a list does not: `already`, meaning the transition
// proved its own effect was in place and did nothing.
const (
	tktOK      = "ok"
	tktAlready = "already"
	tktSkipped = "skipped"
	tktBlocked = "blocked"
	tktFailed  = "failed"
)

// ── the state file ──────────────────────────────────────────────────────────

// TicketState is $LCA_DIR/tickets/<slug>/state.json, rewritten after every
// transition.
//
// It is a JOURNAL, not an authority. That distinction is the whole of the
// idempotency design: a local file cannot be the truth about a branch on a
// remote, a merge request on a forge or a comment on a tracker, so every
// transition re-proves its own effect against the world when the run re-enters
// it (see the probes below). What the state file is for is the EVIDENCE that
// lets a probe tell our effect from somebody else's — the sha this run cut the
// branch at, the sha it pushed, the marker it put in its comment. Without that
// evidence "the branch exists" cannot be told apart from "somebody else made a
// branch with the name we were going to use", and a pipeline that cannot tell
// those apart will eventually commit over a person's work.
//
// 0o600 and redacted at the sink, like state.json in a run directory: a ticket
// body and a review come from outside this program, and this is a file a reader
// opens to find out why a stage stopped.
type TicketState struct {
	Version int    `json:"version"`
	Ticket  string `json:"ticket"`
	Root    string `json:"root"`
	State   string `json:"state"`
	Status  string `json:"status,omitempty"`

	// Blocked and BlockedAt are the gate that stopped the run and the state it
	// was standing in when it did. They are what the ticket comment is written
	// from, and the reason `state` alone is not enough: a blocked run still gets
	// its comment, so after it `state` would otherwise say `reported` and hide
	// where the work actually stands.
	Blocked   string `json:"blocked,omitempty"`
	BlockedAt string `json:"blocked_at,omitempty"`

	Round  int `json:"round"`  // which rework round the run is in; 0 = the first pass
	Rounds int `json:"rounds"` // the ceiling in force, recorded so a resume cannot quietly get more

	Summary string `json:"summary,omitempty"` // the ticket's own title, as the tracker gave it

	Branch string `json:"branch,omitempty"`
	// BranchAt is the commit this run CUT the branch at. It is the evidence the
	// branch probe needs: a branch that no longer contains it is not this run's
	// branch any more, whoever moved it.
	BranchAt string `json:"branch_at,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Target   string `json:"target,omitempty"`
	Remote   string `json:"remote,omitempty"`
	// Head is the commit the implementation left on the branch — what the review
	// judged, what the merge merged and what the push is checked against.
	Head      string `json:"head,omitempty"`
	MergedAt  string `json:"merged_at,omitempty"`
	PushedSha string `json:"pushed_sha,omitempty"`

	MergeRequest *TicketMR `json:"merge_request,omitempty"`
	CommentID    string    `json:"comment_id,omitempty"`
	// MovedTo is the status the transition tool actually applied, recorded after
	// the call returned. The marker in the comment proves the COMMENT half of the
	// report and nothing else, so without this a run killed between the comment
	// and the status move found its own marker on every later night, recorded
	// `report already`, and the board never moved — with the result object and the
	// journal both saying the ticket had been reported.
	MovedTo string `json:"moved_to,omitempty"`
	// Marker is the line this run puts in its own ticket comment. The tracker's
	// comments are the only place a re-run can look to find out whether it already
	// commented, because a comment has no natural key — so the run writes one.
	Marker string `json:"marker,omitempty"`

	Check   TicketCheck   `json:"check"`
	Verdict string        `json:"verdict,omitempty"`
	Review  *reviewReport `json:"review,omitempty"`

	// Skills are the skills that were in force, by stage, name and content hash.
	// Two pipeline runs of the same ticket are otherwise not comparable, which is
	// the same reason results.jsonl carries roles_hash and prompt_hash.
	Skills []TicketSkill `json:"skills"`

	// Coder is the coder's session uid, so a rework round continues the SAME
	// session (-session, sessions.go): the model already knows what it tried, and
	// the gateway's prefix cache makes the second round cheap.
	Coder    string   `json:"coder_session,omitempty"`
	Runs     []string `json:"runs"`   // one rec.id per process that worked this ticket
	Traces   []string `json:"traces"` // the trace each of those wrote
	Sessions []string `json:"sessions"`

	// Pending is the transition whose remote call was in flight when this process
	// last wrote. It is what tells "I was killed before the call" from "I was
	// killed after it": a transition that is safe to re-enter blind just runs
	// again, and the one that is not (opening a ticket, which no tool can undo
	// and no configured tool can search for) refuses and names the key to pass.
	Pending string `json:"pending,omitempty"`

	LCAVersion string `json:"lca_version,omitempty"`
	RolesHash  string `json:"roles_hash,omitempty"`

	Started string       `json:"started"`
	Updated string       `json:"updated"`
	Journal []TicketStep `json:"journal"`
}

// TicketCheck is what the coder's check decided, in the three fields the result
// object already uses for the same purpose and for the same reason: Exit is a
// POINTER because 0 must not be inventable by a check that never ran.
type TicketCheck struct {
	Cmd      string   `json:"cmd"`
	Exit     *int     `json:"exit"`
	Tail     string   `json:"tail"`
	Logs     []string `json:"logs"`
	Attempts int      `json:"attempts"`
}

// TicketMR is a merge request: what lca is about to ask the forge for, and what
// the forge reported back. ID and URL are the forge's own; nothing here is
// composed by lca.
//
// The last five fields are the ARGUMENTS of the create call and are deliberately
// not serialised. The state file is a journal, not a copy of the content — a
// merge request description belongs on the forge, where people read it — and a
// resumed run does not need them: it asks the forge whether the merge request
// exists before it would compose a new one, and only the branch it was written
// for decides that.
type TicketMR struct {
	ID    string `json:"id,omitempty"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`

	Body    string `json:"-"` // the description a model wrote
	Branch  string `json:"-"` // the source branch
	Target  string `json:"-"` // the branch it is asking to merge into
	Ticket  string `json:"-"` // the ticket key, for a team whose MR title carries it
	Summary string `json:"-"` // the ticket's own summary, for the same reason
}

// TicketStep is one transition as the journal records it: what it did, what it
// proved, and how long it took.
type TicketStep struct {
	Name       string `json:"name"`
	From       string `json:"from"`
	To         string `json:"to,omitempty"`
	Round      int    `json:"round"`
	Status     string `json:"status"`
	Gate       string `json:"gate,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
	Run        string `json:"run,omitempty"`
	Started    string `json:"started"`
	Finished   string `json:"finished"`
	DurationMs int64  `json:"duration_ms"`
}

// ticketsDir is where per-ticket state lives. Beside runs/ and transcripts/ in
// the state directory, because it is the same kind of thing: what a run left
// behind, bounded by LCA_DIR and not by the project.
func ticketsDir(cfg Config) string { return filepath.Join(cfg.stateDir(), "tickets") }

// ticketDir is one ticket's own directory. Keyed by the ticket and by nothing
// else — not by a run id — because "one ticket, one branch, one merge request"
// is enforced by there being exactly one of these.
func ticketDir(cfg Config, key string) string {
	return filepath.Join(ticketsDir(cfg), tktSlug(key))
}

// newTicketDir is where a -new run keeps state BEFORE the tracker has given it a
// key. Named by the hash of the task text, so re-running the same cron line
// twice finds the same directory and the same half-finished work instead of
// opening a second ticket.
func newTicketDir(cfg Config, task string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(task)))
	return filepath.Join(ticketsDir(cfg), "new-"+hex.EncodeToString(sum[:])[:12])
}

func loadTicketState(dir string) (*TicketState, error) {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	st := &TicketState{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "state.json"), err)
	}
	if st.Version != tktStateVersion {
		return nil, fmt.Errorf("%s: state version %d, this build writes %d — a resume must not read a document it may not understand, and this one decides whether a merge request already exists",
			dir, st.Version, tktStateVersion)
	}
	return st, nil
}

// redactedTicketState is st with the fields that carry text from OUTSIDE this
// program scrubbed: the ticket's title, the check's tail, the review's prose.
//
// Field by field and deliberately not over the marshalled bytes, for the reason
// redactedRunState gives: this document is read BACK. The shas, the branch name,
// the marker and the skills' hashes are what the probes compare against, and a
// [redacted] in any of them would make a resumed run either refuse to start or
// take a transition it had already taken.
func redactedTicketState(st *TicketState) *TicketState {
	if st == nil || len(envSecrets()) == 0 {
		return st
	}
	c := *st
	c.Summary = redactSecrets(c.Summary)
	c.Blocked = redactSecrets(c.Blocked)
	c.Check.Tail = redactSecrets(c.Check.Tail)
	if c.MergeRequest != nil {
		mr := *c.MergeRequest
		mr.Title = redactSecrets(mr.Title)
		c.MergeRequest = &mr
	}
	if c.Review != nil {
		rv := *c.Review
		rv.Summary = redactSecrets(rv.Summary)
		rv.Comments = append([]reviewComment(nil), c.Review.Comments...)
		for i := range rv.Comments {
			rv.Comments[i].Body = redactSecrets(rv.Comments[i].Body)
		}
		c.Review = &rv
	}
	c.Journal = append([]TicketStep(nil), st.Journal...)
	for i := range c.Journal {
		c.Journal[i].Detail = redactSecrets(c.Journal[i].Detail)
		c.Journal[i].Evidence = redactSecrets(c.Journal[i].Evidence)
	}
	return &c
}

// saveTicketState writes through a temp file in the same directory and renames
// it: a crash mid-write must not leave a half-parsed document, which is the
// whole point of writing state after every transition.
func saveTicketState(dir string, st *TicketState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st.Updated = nowTS()
	b, err := json.MarshalIndent(redactedTicketState(st), "", "  ")
	if err != nil {
		return err
	}
	// Per process, for the reason saveRunState's temp path is: if a lock bug ever
	// let two processes share a ticket, two writers of one fixed path would publish
	// a mixture of both documents.
	tmp := filepath.Join(dir, fmt.Sprintf("state.json.%d.tmp", os.Getpid()))
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "state.json")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// lockTicket keeps two processes off one ticket. Both would walk every remaining
// transition — two merges, two pushes, two merge requests — and both would
// rewrite state.json, so the loser's write would drop the winner's journal. A
// lock whose pid is gone is a crash, not a conflict: it is taken over, because a
// killed run must stay resumable by the same cron line that killed it.
func lockTicket(dir, key string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "lock")
	for try := 0; try < 2; try++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		b, rerr := os.ReadFile(path)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if rerr == nil && pid > 0 && pidAlive(pid) {
			return nil, usageErrf("ticket %s is being worked by pid %d — two processes on one ticket produce two merge requests", key, pid)
		}
		warnLine("taking over %s: the process that held it (pid %d) is gone", key, pid)
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return nil, usageErrf("ticket %s: another process is opening it right now", key)
}

// ── the journal, read back ──────────────────────────────────────────────────

// did reports that this transition completed in THIS round, which is what
// -dry-run marks as `done`. It reads the JOURNAL, which is the right source for
// "what happened"; the probes deliberately do not use it, because what they need
// to know is "is the effect there", and only the world can answer that.
func (st *TicketState) did(name string) bool {
	for _, s := range st.Journal {
		if s.Name == name && s.Round == st.Round && (s.Status == tktOK || s.Status == tktAlready) {
			return true
		}
	}
	return false
}

// record appends one journal entry and is the only writer of State: a state is
// reached because a transition recorded reaching it, never because a cursor was
// incremented.
func (st *TicketState) record(s TicketStep) {
	st.Journal = append(st.Journal, s)
	if s.To != "" && (s.Status == tktOK || s.Status == tktAlready) {
		st.State = s.To
	}
}

// ── the gates ───────────────────────────────────────────────────────────────

// A gate's three answers. Blocked and skipped are not the same thing and
// conflating them is how an unattended pipeline reports success: `push: false`
// SKIPS the push and the run is a success that ends at `merged`, while a red
// check BLOCKS the review and the run is a failure that ends at `implemented`.
type tktGateResult int

const (
	gateOpen tktGateResult = iota
	gateBlocked
	gateSkip
)

// checkGreen is the first of the two gates the whole feature exists for: the
// coder's check ran, to its last attempt, and was green. A check that never ran
// is not green — Exit is a pointer so that cannot be faked.
func (st *TicketState) checkGreen() bool {
	return st.Check.Exit != nil && *st.Check.Exit == 0
}

// approved is the second: the reviewer said approve. Anything else — including a
// verdict that could not be read — is not an approval, which is review.go's own
// rule: unreadable is failed, never a silent approve.
func (st *TicketState) approved() bool { return st.Verdict == tktApprove }

// ── the world outside the state file ────────────────────────────────────────
//
// Four narrow interfaces, one per thing that can be unreachable, so the run can
// say WHICH of them was down — the tracker being gone is a different row of the
// exit table from the gateway being gone. Stage 2 lands the implementations;
// stage 1 runs the machine against stubs.

// TicketBody is a ticket's content: what the tracker gave back on a read, and
// what a model wrote on a -new.
type TicketBody struct {
	Key     string
	Summary string
	Body    string
}

// tktTracker is the tracker, through the four MCP tools the pipeline: block
// names. Every method takes the tool name from configuration — there is no
// default and no guess, because their jira-mcp exposes about seventy tools and
// no two installations name them alike.
type tktTracker interface {
	Read(ctx context.Context, tool, key string) (TicketBody, error)
	Create(ctx context.Context, tool, project string, body TicketBody) (string, error)
	Comment(ctx context.Context, tool, key, body string) (string, error)
	// FindComment looks for a comment carrying this run's own marker. It is what
	// makes the report transition idempotent, and it is the reason the run writes
	// a marker at all: a comment has no key a re-run could remember. It is given
	// the READ tool — the block configures no comment-search tool, because lca
	// does not get to assume this tracker has one.
	FindComment(ctx context.Context, tool, key, marker string) (string, bool, error)
	Move(ctx context.Context, tool, key, status string) error
}

// tktRepo is the git side. Every method is a question or a transition lca
// performs itself; none of them is reachable by a model.
type tktRepo interface {
	BranchHead(branch string) (string, bool, error)
	CutBranch(branch string) (sha, worktree string, err error)
	// Worktree is where the ticket's branch is checked out, adopting the worktree
	// that is already there and adding one when there is none.
	//
	// It is separate from CutBranch because the branch and the worktree have
	// different lifetimes, and a resume discovers the difference: the branch is in
	// the repository and survives everything, while the worktree is a DIRECTORY
	// that `lca clean` removes, a full disk takes and a reboot with the state
	// directory on tmpfs takes. Without this, a re-entered run would prove the
	// branch, skip the transition and send the coder to a path that is not there.
	Worktree(branch string) (dir string, err error)
	// MadeAt is the commit lca cut this branch at, read from the authorship ref
	// CutBranch writes. It is how a branch whose BranchAt never reached the state
	// file is still told from somebody else's branch of the same name: the ref
	// exists the moment the branch does, which is one instruction before the save.
	MadeAt(branch string) (string, bool, error)
	// Contains answers "is `ancestor` in the history of `of`", which is how every
	// git-side probe tells our effect from somebody else's.
	Contains(ancestor, of string) (bool, error)
	// MergeIn merges the SHA the gate approved, and refuses if the branch is no
	// longer exactly it. The gate reads a check and a verdict that belong to one
	// commit; a branch that grew past it carries work nothing judged, and a merge
	// is the half of this machine that cannot be re-done.
	MergeIn(branch, target, sha string) (string, error)
	RemoteHead(remote, branch string) (string, bool, error)
	PushBranch(remote, branch, sha string) error
}

// tktForge is the forge's two tools. Find is not a convenience: it is the only
// thing between a resumed run and a second merge request, because the merge
// request lives where this machine's state file cannot be the truth.
type tktForge interface {
	Find(ctx context.Context, tool, branch, target string) (TicketMR, bool, error)
	Create(ctx context.Context, tool string, mr TicketMR) (TicketMR, error)
}

// tktStageIn is what every model stage is given. Instructions is the stage's
// named skills, already loaded and framed (stageInstructions): the team's shared
// knowledge base reaching the one stage it was named for.
type tktStageIn struct {
	Stage        string
	Ticket       TicketBody
	Instructions string
	Branch       string
	Worktree     string
	Target       string
	Round        int
	// Session is the session a round two continues (sessions.go). Empty on a first
	// pass; set from TicketState.Coder on every rework round.
	Session string
	Check   TicketCheck
	Review  *reviewReport
	State   *TicketState
}

// tktCodeOut is what the coder stage produced: lca reads the check's verdict
// from it and the session to continue, and nothing else. The model changed files
// in its worktree; it did not commit them, and it could not have.
type tktCodeOut struct {
	Session string
	Head    string
	Check   TicketCheck
}

// tktModels is the model side: five stages, each of which writes CONTENT that
// lca then acts on.
type tktModels interface {
	WriteTicket(ctx context.Context, in tktStageIn, task string) (TicketBody, error)
	Implement(ctx context.Context, in tktStageIn) (tktCodeOut, error)
	Review(ctx context.Context, in tktStageIn) (*reviewReport, error)
	WriteMR(ctx context.Context, in tktStageIn) (TicketMR, error)
	WriteComment(ctx context.Context, in tktStageIn) (string, error)
}

// tktWorld bundles the four so a run can be handed a whole world at once — the
// real one, or a test's.
type tktWorld struct {
	Tracker tktTracker
	Repo    tktRepo
	Forge   tktForge
	Models  tktModels
}

// ── the world, wired up ─────────────────────────────────────────────────────

// newTktWorld builds the real world: the tracker and the forge over MCP
// (ticketmcp.go), the repository over git (ticketgit.go), and the five model
// stages over the engine (ticketstage.go).
//
// It is one function and not four, because the four have to agree about three
// things and a caller assembling them by hand would eventually have them
// disagree: the SESSION that holds the write grants is lca's own and no stage
// shares it, the repository's target branch is the configured one and not an
// argument, and the lead session every stage is a child of is the one whose
// root id joins this run's transcripts, trace and journal together.
func newTktWorld(o *Orchestrator, cfg Config, pc *PipelineConfig, keyOf func() string, need tktNeed) (tktWorld, error) {
	// The lead reads nothing and is sent nowhere: it exists so each stage can be a
	// CHILD with its own uid, which is what the trace, the transcripts and -session
	// are keyed on. Primary of the integrator role because a lead has to be some
	// role and that is the one every roles.yaml running this command declares.
	lead, err := o.NewPrimary(pc.Integrator, "", nil)
	if err != nil {
		return tktWorld{}, usageErrf("pipeline: roles: integrator: %v", err)
	}
	lead.view = newChildView(lead, false)
	mc, err := newTktMCP(o, lead, pc, need)
	if err != nil {
		return tktWorld{}, err
	}
	repo, err := newTktGit(o, keyOf, pc.Target)
	if err != nil {
		return tktWorld{}, err
	}
	return tktWorld{
		Tracker: tktTrackerCalls{call: mc, pc: pc},
		Forge:   tktForgeCalls{call: mc, pc: pc},
		Repo:    repo,
		Models:  &tktStageRun{orch: o, lead: lead, pc: pc, cfg: cfg},
	}, nil
}

// ── the machine ─────────────────────────────────────────────────────────────

// tktProof is what a probe found. It is the answer to the question that makes a
// resume safe: not "did I record doing this" but "is the effect there, and is it
// mine".
type tktProof struct {
	kind tktProofKind
	why  string
}

type tktProofKind int

const (
	proofAbsent  tktProofKind = iota // the effect is not there: do it
	proofMine                        // it is there and the evidence says this run made it
	proofForeign                     // it is there and it is not ours: stop, and say so on the ticket
)

// tktTransition is one edge of the machine.
type tktTransition struct {
	Name  string // the verb: the journal's key and -dry-run's row
	To    string // the state it records on success
	Does  string // what LCA ITSELF performs here, for -dry-run
	Stage string // which model stage writes the content, "" when none does
	Gate  string // the gate's name, "" when nothing waits
	Why   string // the sentence -dry-run prints under the gate

	// Redo says this transition is safe to re-enter blind after a crash that may
	// have landed its remote call. Everything here is — a branch cut twice is one
	// branch, a merge already made is a no-op, a push of the same sha changes
	// nothing, a merge request is found before it is created, a comment carries a
	// marker — except opening a ticket, which no configured tool can search for
	// and no tool can undo.
	Redo bool

	gate  func(*tktRun) (tktGateResult, string)
	probe func(*tktRun) (tktProof, error)
	do    func(*tktRun) error
}

// tktSpine is the machine, in order. Every state is reachable, every transition
// records one, and the resume point is found by looking this list up by state —
// never by a cursor, because a cursor cannot be checked against the world.
func tktSpine() []*tktTransition {
	return []*tktTransition{{
		Name: tktOpen, To: tktOpened, Stage: tktStageTicket, Redo: false,
		Does: "read the ticket from the tracker; with -new, write a body and open one",
		probe: func(r *tktRun) (tktProof, error) {
			if r.st.Ticket != "" && r.st.Summary != "" {
				return tktProof{proofMine, "the ticket is already known: " + r.st.Ticket}, nil
			}
			return tktProof{proofAbsent, ""}, nil
		},
		do: (*tktRun).doOpen,
	}, {
		Name: tktBranch, To: tktBranched, Redo: true,
		Does: "create the branch and its worktree",
		Gate: "ticket", Why: "the ticket exists",
		gate: func(r *tktRun) (tktGateResult, string) {
			if r.st.Ticket == "" {
				return gateBlocked, "there is no ticket to work"
			}
			return gateOpen, ""
		},
		probe: (*tktRun).probeBranch,
		do:    (*tktRun).doBranch,
	}, {
		Name: tktImplement, To: tktImplemented, Stage: tktStageCoder, Redo: true,
		Does: "run the coder in the worktree and its check_cmd over the result",
		Gate: "branched", Why: "a branch and a worktree for this ticket exist",
		gate: func(r *tktRun) (tktGateResult, string) {
			if r.st.Branch == "" {
				return gateBlocked, "there is no branch to work on"
			}
			return gateOpen, ""
		},
		// Proved from the EVIDENCE and from GIT: the artifacts of a finished
		// implementation are a commit that is still on the branch and a green check,
		// and doRework clears the check — so the same questions answer both "has this
		// round been implemented" and "is last round's work still the current work".
		probe: (*tktRun).probeImplement,
		do:    (*tktRun).doImplement,
	}, {
		Name: tktReview, To: tktReviewed, Stage: tktStageReviewer, Redo: true,
		Does: "take the diff and record the reviewer's structured verdict",
		Gate: "check", Why: "the check ran to its last attempt and was green",
		gate: func(r *tktRun) (tktGateResult, string) {
			if !r.st.checkGreen() {
				return gateBlocked, r.checkWhy()
			}
			return gateOpen, ""
		},
		// A recorded verdict IS the artifact of a finished review, and doRework clears
		// it, so this one question answers both "has this round been judged" and "is
		// the verdict under me the one for the code under me".
		probe: func(r *tktRun) (tktProof, error) {
			if r.st.Verdict != "" {
				return tktProof{proofMine, "round " + strconv.Itoa(r.st.Round) + " is already judged: " + r.st.Verdict}, nil
			}
			return tktProof{proofAbsent, ""}, nil
		},
		do: (*tktRun).doReview,
	}, {
		Name: tktRework, To: tktReworked, Stage: tktStageCoder, Redo: true,
		Does: "feed the comments back on the coder's own session and start the next round",
		Gate: "changes asked for, and a round left",
		Why:  "the verdict is request_changes and the rework rounds are not spent",
		gate: func(r *tktRun) (tktGateResult, string) {
			switch {
			case r.st.approved():
				return gateSkip, "the verdict is approve, so there is nothing to rework"
			case r.st.Verdict != tktChanges:
				return gateSkip, "there is no verdict asking for changes"
			case r.st.Round >= r.st.Rounds:
				return gateSkip, roundsSpent(r.st.Rounds)
			}
			return gateOpen, ""
		},
		probe: func(r *tktRun) (tktProof, error) { return tktProof{proofAbsent, ""}, nil },
		do:    (*tktRun).doRework,
	}, {
		Name: tktMerge, To: tktMerged, Redo: true,
		Does: "merge the branch into the target branch",
		Gate: "check and review", Why: "the check is green AND the verdict is approve",
		gate: func(r *tktRun) (tktGateResult, string) {
			if !r.st.checkGreen() {
				return gateBlocked, r.checkWhy()
			}
			if !r.st.approved() {
				return gateBlocked, r.reviewWhy()
			}
			return gateOpen, ""
		},
		probe: (*tktRun).probeMerge,
		do:    (*tktRun).doMerge,
	}, {
		Name: tktPush, To: tktPushed, Redo: true,
		Does: "push the branch to the remote",
		Gate: "merged, and pushing allowed", Why: "the merge happened and pipeline: push is true",
		gate: func(r *tktRun) (tktGateResult, string) {
			if !r.pc.pipelinePush() {
				return gateSkip, "pipeline: push is false, so this run finishes at merged"
			}
			// The STATE and not the journal: `merge` records `merged` whether it made
			// the merge commit or proved one was already there, and only one of those
			// two leaves a sha behind.
			if tktOrder(r.st.State) < tktOrder(tktMerged) {
				return gateBlocked, "nothing merged, so there is nothing to push"
			}
			return gateOpen, ""
		},
		probe: (*tktRun).probePush,
		do:    (*tktRun).doPush,
	}, {
		Name: tktPropose, To: tktProposed, Stage: tktStageMR, Redo: true,
		Does: "find the merge request for this branch, and open one if there is none",
		Gate: "pushed", Why: "the branch is on the remote",
		gate: func(r *tktRun) (tktGateResult, string) {
			if !r.pc.pipelinePush() {
				return gateSkip, "pipeline: push is false, so there is no remote branch to propose from"
			}
			if tktOrder(r.st.State) < tktOrder(tktPushed) {
				return gateBlocked, "the branch is not on the remote, so no merge request can point at it"
			}
			return gateOpen, ""
		},
		probe: (*tktRun).probePropose,
		do:    (*tktRun).doPropose,
	}, {
		Name: tktReport, To: tktReported, Stage: tktStageReport, Redo: true,
		Does:  "comment on the ticket, and move its status if one is configured",
		Why:   "a blocked run is reported too, because an unattended pipeline that goes quiet is worse than one that fails loudly",
		probe: (*tktRun).probeReport,
		do:    (*tktRun).doReport,
	}}
}

// ── the run ─────────────────────────────────────────────────────────────────

// tktRun is one invocation working one ticket.
type tktRun struct {
	cfg  Config
	orch *Orchestrator
	pc   *PipelineConfig
	w    tktWorld
	st   *TicketState
	dir  string
	ctx  context.Context

	newTask string // the -new one-liner, empty when a key was given
	// body is the ticket's own text, as the tracker has it. It is held in MEMORY
	// and not in the state file, because the state file is a journal and not a copy
	// of a document somebody else owns — and because a ticket a person edited
	// overnight has to reach tonight's coder, which a cached copy would prevent.
	// ensureBody fetches it for any stage that needs it.
	body   string
	spine  []*tktTransition
	skills []TicketSkill
	runID  string

	// targetHeldBy is the directory that has the TARGET branch checked out, as
	// `git worktree list` answers it. It is filled by a PLAN and left empty by a
	// run: MergeIn asks the same question itself, at the moment it matters, and a
	// stale answer taken minutes earlier must not be what a merge is decided on.
	// A plan has the opposite need — it is read before the first night, and this
	// is the mistake that otherwise surfaces last, after a coder and a reviewer
	// have been paid for.
	targetHeldBy string

	// commented says the ticket already carries THIS run's comment, so the report
	// transition owes only the status move. The two halves of the report are
	// proved separately (probeReport), and re-posting a comment the marker has
	// already found is the one thing the marker exists to prevent.
	commented bool

	// The three things close() has to undo, in the order it undoes them: the
	// ticket's lock, the signal handler, and the budget's context — which every
	// check command, stdio MCP server and subagent hangs off.
	unlock func()
	stop   func()
	cancel context.CancelFunc
}

// configuredCheck is the coder role's own check_cmd as the FILE has it, which is
// a different question from st.Check.Cmd (what ran). Empty when the roles are
// not loaded — a unit test driving the machine against fakes — in which case the
// caller falls back to the sentence about a missing key, as it did before.
func (r *tktRun) configuredCheck() string {
	if r.orch == nil || r.orch.roles == nil {
		return ""
	}
	if ag := r.orch.roles.role(r.pc.Coder); ag != nil {
		return strings.TrimSpace(ag.CheckCmd)
	}
	return ""
}

// checkWhy and reviewWhy are the two blocked sentences, written once because
// they go into the journal, into the ticket comment and into the result object
// and must say the same thing in all three.
func (r *tktRun) checkWhy() string {
	if r.st.Check.Cmd == "" {
		// Two different things, and saying the wrong one sends an operator to edit a
		// key that is already there. st.Check.Cmd is what RAN, so it is empty both
		// before the implement transition and when no command exists at all —
		// which is how a plan for a team whose coder HAS a check_cmd told them it
		// did not, with the file path, in the sentence they would act on. The
		// config is the authority on whether one exists; the state is the authority
		// on whether it ran.
		if cmd := r.configuredCheck(); cmd != "" {
			return fmt.Sprintf("nothing has checked this change yet: the coder's own check (%s) runs at the implement transition", cmd)
		}
		return fmt.Sprintf("nothing checked this change: no check command is configured for the coder role, so there is no green to gate on — write it as roles: %s: check_cmd in %s", r.pc.Coder, firstNonEmpty(r.pc.Src, "roles.yaml"))
	}
	if r.st.Check.Exit == nil {
		return fmt.Sprintf("`%s` never got to run, so nothing verified this change", r.st.Check.Cmd)
	}
	return fmt.Sprintf("`%s` still failed (exit %d) after %s", r.st.Check.Cmd, *r.st.Check.Exit,
		plural(r.st.Check.Attempts, "attempt", "attempts"))
}

func (r *tktRun) reviewWhy() string {
	switch r.st.Verdict {
	case "":
		return "the reviewer's verdict could not be read, so nothing reviewed this change"
	case tktChanges:
		return "the reviewer asked for changes and " + roundsSpent(r.st.Rounds)
	}
	return "the reviewer did not approve this change: " + r.st.Verdict
}

// roundsSpent is the back half of both rework sentences — the gate's skip reason
// and the blocked sentence that goes on the ticket. Written out rather than
// through plural(), which prefixes the count and so produced "the 1 rework round
// are spent" in the one sentence a person reads in the morning.
func roundsSpent(n int) string {
	switch n {
	case 0:
		return "there is no rework round to spend (pipeline: rework_rounds is 0, which means one review and no rework)"
	case 1:
		return "its one rework round is spent"
	}
	return fmt.Sprintf("all %d rework rounds are spent", n)
}

// stageIn builds one stage's input, including the instructions its named skills
// carry. One function, so no stage can accidentally be given a different world
// than the others — or no skills at all because somebody forgot the line.
func (r *tktRun) stageIn(stage string) tktStageIn {
	in := tktStageIn{
		Stage:        stage,
		Ticket:       TicketBody{Key: r.st.Ticket, Summary: r.st.Summary, Body: r.body},
		Instructions: stageInstructions(r.skills, stage),
		Branch:       r.st.Branch,
		Worktree:     r.st.Worktree,
		Target:       r.st.Target,
		Round:        r.st.Round,
		Check:        r.st.Check,
		Review:       r.st.Review,
		State:        r.st,
	}
	if stage == tktStageCoder {
		in.Session = r.st.Coder
	}
	return in
}

// walk is the machine. It always enters at the FIRST transition and runs
// forward, and that is the design and not an inefficiency.
//
// A resume that jumped to the transition after the recorded state would be
// trusting the state word, and the state word is the one thing that cannot be
// trusted: it says `pushed` while the remote has nothing, because the push was
// killed after the journal entry and before the packet left. Entering at the
// top and letting every transition re-prove its own effect is what turns that
// into "the push is simply made again" instead of a merge request pointing at a
// branch nobody has.
//
// It is cheap because that is what the probes are for: a ticket read is one MCP
// call, a branch and a merge are a rev-parse each, and the two expensive
// transitions — the coder and the reviewer — prove themselves from the journal
// and the evidence under it without buying a model. And it is exactly what a
// cron line needs: a ticket blocked last night on a red check re-enters at
// `implement`, because that is the transition whose effect is no longer there.
//
// It returns the gate that blocked, and the state the run was standing in when
// it did — both empty when the walk completed.
func (r *tktRun) walk() (blocked, at string, err error) {
	if tktOrder(r.st.State) < 0 {
		return "", "", usageErrf("%s: this ticket's state file says %q, which is not a state this build knows", r.dir, r.st.State)
	}
	// Before anything: a resumed run re-derives its gates from the recorded
	// EVIDENCE instead of from the state word. A document that says `merged` while
	// its recorded verdict asks for changes is a journal somebody edited or a
	// build that wrote it wrong, and continuing from it would push unreviewed work.
	if err := r.auditState(); err != nil {
		return "", "", err
	}
	for i := r.index(tktOpen); i >= 0 && i < len(r.spine); {
		tr := r.spine[i]
		// `report` is the last transition and it is reached by falling off the end of
		// the loop too, so the walk stops before it: the caller runs it, once, on
		// both the finished and the blocked path.
		if tr.Name == tktReport {
			return "", "", nil
		}
		res, why := gateOf(tr, r)
		switch res {
		case gateBlocked:
			r.st.record(TicketStep{Name: tr.Name, From: r.st.State, Round: r.st.Round,
				Status: tktBlocked, Gate: tr.Gate, Detail: why, Run: r.runID,
				Started: nowTS(), Finished: nowTS()})
			r.save()
			return why, r.st.State, nil
		case gateSkip:
			r.st.record(TicketStep{Name: tr.Name, From: r.st.State, Round: r.st.Round,
				Status: tktSkipped, Gate: tr.Gate, Detail: why, Run: r.runID,
				Started: nowTS(), Finished: nowTS()})
			r.save()
			i++
			continue
		}
		if err := r.enter(tr); err != nil {
			return "", "", err
		}
		// The one edge that is not a straight line: a rework round goes back to
		// `implement`, with the round already advanced by doRework.
		if tr.Name == tktRework {
			i = r.index(tktImplement)
			continue
		}
		i++
	}
	return "", "", nil
}

// gateOf evaluates a transition's gate; a transition with no gate is open.
func gateOf(tr *tktTransition, r *tktRun) (tktGateResult, string) {
	if tr.gate == nil {
		return gateOpen, ""
	}
	return tr.gate(r)
}

// enter runs one transition: probe, then either skip it, refuse it, or do it.
func (r *tktRun) enter(tr *tktTransition) error {
	start := time.Now()
	from := r.st.State
	step := TicketStep{Name: tr.Name, From: from, Round: r.st.Round, Gate: tr.Gate,
		Run: r.runID, Started: nowTS()}
	finish := func(status, detail, evidence, to string) {
		step.Status, step.Detail, step.Evidence, step.To = status, detail, evidence, to
		step.Finished, step.DurationMs = nowTS(), time.Since(start).Milliseconds()
		r.st.record(step)
		r.save()
	}

	// The probe FIRST, and the pending refusal only if it answers "the effect is
	// not there".
	//
	// A pending record says a process died inside this very transition, and for
	// the one transition that cannot be re-entered blind — opening a ticket, which
	// no tool can undo and no configured tool can search for — that used to end
	// the run. But the probe is the thing that can tell "the call landed" from "it
	// did not", and `adopt()` saves the key while Pending still says `open`: its
	// own comment promises "a crash after this line is recoverable", and a refusal
	// evaluated before the probe broke that promise. The remedy in the message
	// could not work either, because the state file the re-run reads is the same
	// one. So the order is the fix: a ticket that exists is simply continued, and
	// only a run killed with nothing to show for it is refused.
	pr, err := tr.probe(r)
	if err != nil {
		finish(tktFailed, err.Error(), "", "")
		return err
	}
	if pr.kind == proofAbsent && r.st.Pending == tr.Name && !tr.Redo {
		why := fmt.Sprintf("a previous run was killed inside `%s` and this build has no configured way to ask the tracker whether that call landed — refusing to open a second ticket. If the ticket exists, re-run as `lca ticket <KEY>`; if it does not, remove %s.",
			tr.Name, filepath.Join(r.dir, "state.json"))
		finish(tktFailed, why, "", "")
		return usageErrf("%s", why)
	}
	switch pr.kind {
	case proofMine:
		finish(tktAlready, "", pr.why, tr.To)
		return nil
	case proofForeign:
		// Not an error in lca and not a failure of the model: somebody else did
		// something to this ticket's branch. Row 2 of the table — alert a person,
		// touch nothing — because every remaining transition would build on work this
		// run cannot account for.
		finish(tktFailed, pr.why, "", "")
		return usageErrf("%s", pr.why)
	}

	// Every stage that writes content is given the ticket's own text, and a
	// resumed run does not have it: `open` proved its effect from the state file
	// and so never read the ticket. One read, here, where a tracker that is down
	// is still infra_error and the ticket is still untouched — and it is the right
	// place for another reason, which is that a ticket somebody edited overnight
	// then reaches tonight's coder instead of last night's copy of it.
	if tr.Stage != "" && tr.Stage != tktStageTicket {
		if err := r.ensureBody(); err != nil {
			finish(tktFailed, err.Error(), "", "")
			return err
		}
	}

	// The intent, on disk, before the call. A crash between these two lines is the
	// one window where the world and the journal can disagree, and this is what
	// makes the disagreement visible rather than silent.
	r.st.Pending = tr.Name
	r.save()
	derr := tr.do(r)
	r.st.Pending = ""
	if derr != nil {
		finish(tktFailed, derr.Error(), "", "")
		return derr
	}
	finish(tktOK, "", "", tr.To)
	return nil
}

// ensureBody reads the ticket once per process, for the stages that are given
// its text. A ticket with no body at all is not an error: some teams write
// everything in the title, and the summary is still handed over.
func (r *tktRun) ensureBody() error {
	if r.body != "" || r.st.Ticket == "" {
		return nil
	}
	b, err := r.w.Tracker.Read(r.ctx, r.pc.Tracker.Read, r.st.Ticket)
	if err != nil {
		return err
	}
	r.body = b.Body
	if r.st.Summary == "" {
		r.st.Summary = b.Summary
	}
	return nil
}

// auditState re-derives what the recorded state claims and refuses a document
// whose claims its own evidence does not support.
func (r *tktRun) auditState() error {
	past := func(s string) bool { return tktOrder(r.st.State) >= tktOrder(s) }
	switch {
	case past(tktMerged) && !r.st.checkGreen():
		return usageErrf("%s: this state file says %q, and no green check is recorded under it — nothing may merge without one, so this document cannot be resumed",
			filepath.Join(r.dir, "state.json"), r.st.State)
	case past(tktMerged) && !r.st.approved():
		return usageErrf("%s: this state file says %q with the verdict %q — nothing may merge without an approve, so this document cannot be resumed",
			filepath.Join(r.dir, "state.json"), r.st.State, r.st.Verdict)
	case r.st.Rounds != r.pc.pipelineRounds():
		// Not fatal: the operator is entitled to change the ceiling. But a run that
		// silently got three more rounds than the record says is a run nobody can
		// read afterwards, so it is said out loud and the NEW number is recorded.
		warnLine("%s started with %s and pipeline: rework_rounds is %d now", r.st.Ticket,
			plural(r.st.Rounds, "rework round", "rework rounds"), r.pc.pipelineRounds())
		r.st.Rounds = r.pc.pipelineRounds()
	}
	return nil
}

// tktOrder is a state's position on the spine, for the audit's "past" question.
// -1 for a state the build does not know, which walk has already refused.
func tktOrder(state string) int {
	for i, s := range []string{tktStart, tktOpened, tktBranched, tktImplemented, tktReviewed,
		tktReworked, tktMerged, tktPushed, tktProposed, tktReported} {
		if s == state {
			return i
		}
	}
	return -1
}

func (r *tktRun) index(name string) int {
	for i, tr := range r.spine {
		if tr.Name == name {
			return i
		}
	}
	return -1
}

// stageOf is which model stage a transition buys IN THIS INVOCATION, which is
// the transition's own Stage everywhere except `open`.
//
// `open` has two shapes and that is the whole reason doOpen is two functions: a
// `-new` run has a model write the body, and a run given a key only READS the
// tracker, so no stage runs there and the `ticket:` skills reach nobody. A plan
// that printed the stage and its skills on row 1 anyway was contradicting its
// own CALLS section one screen further down, where `open one (-new)` is already
// hidden — and resolution held every keyed run to a skill lca was never going to
// load, so a team with no -new workflow could not start at all until somebody
// wrote one.
func (r *tktRun) stageOf(tr *tktTransition) string {
	if tr.Name == tktOpen && r.newTask == "" {
		return ""
	}
	return tr.Stage
}

func (r *tktRun) save() {
	if err := saveTicketState(r.dir, r.st); err != nil {
		// A state file that cannot be written means the next run cannot tell what
		// this one did, which is the one failure that turns a resumable pipeline into
		// a duplicating one. It is said every time rather than once.
		warnLine("could not write %s: %v", filepath.Join(r.dir, "state.json"), err)
	}
}

// ── the transitions ─────────────────────────────────────────────────────────

// doOpen is the only transition with two shapes, because `lca ticket KEY` and
// `lca ticket -new "…"` start from different places: one reads a ticket that
// exists, the other has a model write a body and then opens one.
//
// The -new half is also the only transition in the machine that is not safe to
// re-enter blind (Redo is false): creating a ticket cannot be undone, and the
// four tools the block configures include no search, so after a crash inside
// the call nothing here can ask the tracker whether it landed. The honest
// answer to that is a refusal naming the key to pass, not a second ticket.
func (r *tktRun) doOpen() error {
	if r.newTask != "" && r.st.Ticket == "" {
		body, err := r.w.Models.WriteTicket(r.ctx, r.stageIn(tktStageTicket), r.newTask)
		if err != nil {
			return err
		}
		key, err := r.w.Tracker.Create(r.ctx, r.pc.Tracker.Create, r.pc.Project, body)
		if err != nil {
			return err
		}
		if strings.TrimSpace(key) == "" {
			return usageErrf("pipeline: tracker: create: %s returned no ticket key, so nothing here knows what was opened — this is the one call that cannot be retried safely", r.pc.Tracker.Create)
		}
		r.st.Ticket, r.st.Summary, r.body = key, firstNonEmpty(body.Summary, r.newTask), body.Body
		// The key, on disk, before anything else happens to it. A crash after this
		// line is recoverable; a crash before it is what Redo: false refuses.
		r.adopt()
		return nil
	}
	body, err := r.w.Tracker.Read(r.ctx, r.pc.Tracker.Read, r.st.Ticket)
	if err != nil {
		return err
	}
	r.st.Summary, r.body = firstNonEmpty(body.Summary, r.st.Ticket), body.Body
	return nil
}

// adopt moves a -new run's state out of its by-task-hash directory and into the
// ticket's own, once the tracker has named it. The key is saved to the old
// directory FIRST and a redirect is left behind, so every order of crash leads
// the next run to the one ticket that exists rather than to a second one.
func (r *tktRun) adopt() {
	r.st.Branch = tktBranchFor(r.pc, r.st.Ticket)
	r.save()
	want := ticketDir(r.cfg, r.st.Ticket)
	if want == r.dir {
		return
	}
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		warnLine("could not move this ticket's state to %s: %v", want, err)
		return
	}
	if _, err := os.Stat(want); err == nil {
		// A directory for this key already exists, which means the tracker handed
		// back a key somebody already worked. Not ours to overwrite: the redirect
		// below still points the next run at it, and the audit will read its journal.
		warnLine("%s already has a state directory at %s — leaving it alone", r.st.Ticket, want)
	} else if err := os.Rename(r.dir, want); err != nil {
		warnLine("could not move this ticket's state to %s: %v", want, err)
		return
	}
	os.MkdirAll(r.dir, 0o700)
	os.WriteFile(filepath.Join(r.dir, "redirect"), []byte(r.st.Ticket+"\n"), 0o600)
	r.dir = want
}

func (r *tktRun) probeBranch() (tktProof, error) {
	head, found, err := r.w.Repo.BranchHead(r.st.Branch)
	if err != nil {
		return tktProof{}, err
	}
	if !found {
		// The state file may well say `branched`. The branch is what matters, and it
		// is not there — somebody deleted it, or the worktree was cleaned up. Cutting
		// it again from the ticket's base is the right move and is why Redo is true.
		return tktProof{proofAbsent, ""}, nil
	}
	if r.st.BranchAt == "" {
		// No recorded base, and the branch is there. That is either somebody else's
		// branch of this name, or this ticket's own — cut by a run that was killed in
		// the one instruction between `worktree add -b` and the save that records
		// BranchAt. CutBranch writes an authorship ref at that same moment, for
		// exactly this question, so git is asked before anybody is woken up: a branch
		// carrying lca's made-ref for this ticket is ours, and the commit it was cut
		// at is recoverable from the ref itself.
		made, ok, merr := r.w.Repo.MadeAt(r.st.Branch)
		if merr != nil {
			return tktProof{}, merr
		}
		if !ok {
			return tktProof{proofForeign, fmt.Sprintf("a branch named %s already exists (at %s), it carries no record of lca having cut it, and this run has no base recorded for it — refusing to work on somebody else's branch. If it is not somebody's, delete it (`git branch -D %s`) and re-run: the next run cuts it again from %s.",
				r.st.Branch, shortSha(head), r.st.Branch, r.st.Target)}, nil
		}
		r.st.BranchAt = made
	}
	ok, err := r.w.Repo.Contains(r.st.BranchAt, head)
	if err != nil {
		return tktProof{}, err
	}
	if !ok {
		return tktProof{proofForeign, fmt.Sprintf("%s no longer contains %s, the commit this run cut it at — somebody rewrote it, and nothing here can tell what is now on it", r.st.Branch, shortSha(r.st.BranchAt))}, nil
	}
	// The branch is ours; the other half of this transition's effect is somewhere
	// to work. The gate below is worded "a branch and a worktree for this ticket
	// exist", and a probe that proved only the first half would hand the coder a
	// path that a cleanup removed last week.
	wt, err := r.w.Repo.Worktree(r.st.Branch)
	if err != nil {
		return tktProof{}, err
	}
	r.st.Worktree = wt
	return tktProof{proofMine, fmt.Sprintf("%s exists at %s and still contains %s", r.st.Branch, shortSha(head), shortSha(r.st.BranchAt))}, nil
}

func (r *tktRun) doBranch() error {
	sha, wt, err := r.w.Repo.CutBranch(r.st.Branch)
	if err != nil {
		return err
	}
	r.st.BranchAt, r.st.Worktree = sha, wt
	return nil
}

// probeImplement asks the STATE FILE what this round left behind and GIT whether
// it is still there.
//
// The state file alone was not enough, and the gap was the worst kind: a branch
// deleted between two nights — by `lca clean --branches`, which exists to delete
// it, or by a forge-side cleanup — is re-cut EMPTY by doBranch, and a probe that
// read only `Head != "" && checkGreen()` then answered "round 0 is implemented",
// skipped the coder and the reviewer, merged the empty branch, pushed it, opened
// a merge request for it and told the ticket the work had landed. So the commit
// has to be on the branch, and when it is not, the evidence under it belongs to
// a commit nothing can find and is cleared rather than trusted.
func (r *tktRun) probeImplement() (tktProof, error) {
	if r.st.Head == "" || !r.st.checkGreen() {
		return tktProof{proofAbsent, ""}, nil
	}
	head, found, err := r.w.Repo.BranchHead(r.st.Branch)
	if err != nil {
		return tktProof{}, err
	}
	if found {
		in, cerr := r.w.Repo.Contains(r.st.Head, head)
		if cerr != nil {
			return tktProof{}, cerr
		}
		if in {
			return tktProof{proofMine, fmt.Sprintf("round %d is implemented at %s, which is still on %s, and its check was green",
				r.st.Round, shortSha(r.st.Head), r.st.Branch)}, nil
		}
	}
	// The round's own evidence, every piece of it, because all of it was about a
	// commit that is no longer on the branch — including the verdict, which the
	// review probe would otherwise read as "this round is already judged".
	warnLine("%s is not on %s any more, so this round is implemented again from what is there now", shortSha(r.st.Head), r.st.Branch)
	r.st.Head, r.st.Check, r.st.Verdict, r.st.Review = "", TicketCheck{}, "", nil
	return tktProof{proofAbsent, ""}, nil
}

func (r *tktRun) doImplement() error {
	out, err := r.w.Models.Implement(r.ctx, r.stageIn(tktStageCoder))
	if err != nil {
		return err
	}
	r.st.Check, r.st.Head = out.Check, out.Head
	if out.Session != "" {
		r.st.Coder = out.Session
		if !contains(r.st.Sessions, out.Session) {
			r.st.Sessions = append(r.st.Sessions, out.Session)
		}
	}
	return nil
}

func (r *tktRun) doReview() error {
	rep, err := r.w.Models.Review(r.ctx, r.stageIn(tktStageReviewer))
	if err != nil {
		return err
	}
	// review.go's rule, held here too: a verdict that could not be read is not an
	// approval. The gate below reads Verdict, so leaving it empty is what makes
	// "unreadable" behave as "not approved" with no extra branch to forget.
	r.st.Review = rep
	if rep != nil {
		r.st.Verdict = rep.Verdict
	}
	return nil
}

func (r *tktRun) doRework() error {
	// The round advances BEFORE the coder runs, so a crash inside the rework
	// re-enters at `implement` in the new round rather than re-judging the old one.
	r.st.Round++
	r.st.Verdict = ""
	r.st.Check = TicketCheck{}
	return nil
}

func (r *tktRun) probeMerge() (tktProof, error) {
	if r.st.Head == "" {
		return tktProof{proofAbsent, ""}, nil
	}
	target, found, err := r.w.Repo.BranchHead(r.st.Target)
	if err != nil {
		return tktProof{}, err
	}
	if !found {
		return tktProof{}, usageErrf("pipeline: target_branch: %q is not a branch in this repository", r.st.Target)
	}
	in, err := r.w.Repo.Contains(r.st.Head, target)
	if err != nil {
		return tktProof{}, err
	}
	if in {
		// Already merged, and it does not matter who did it: the effect this
		// transition exists to produce is in place, and merging again would be a
		// no-op commit on somebody's history.
		return tktProof{proofMine, fmt.Sprintf("%s is already in %s", shortSha(r.st.Head), r.st.Target)}, nil
	}
	return tktProof{proofAbsent, ""}, nil
}

func (r *tktRun) doMerge() error {
	// The reviewed SHA and not the branch name. The gate just read st.Check and
	// st.Verdict, and both of them are about st.Head; the branch may have grown a
	// commit since — probeBranch deliberately tolerates a branch that advanced —
	// and that commit has had no check run on it and no reviewer look at it.
	// PushBranch was written with exactly this care and the merge is the half that
	// cannot be taken back, so MergeIn refuses unless the branch is still the
	// commit the gate approved.
	sha, err := r.w.Repo.MergeIn(r.st.Branch, r.st.Target, r.st.Head)
	if err != nil {
		return err
	}
	r.st.MergedAt = sha
	return nil
}

func (r *tktRun) probePush() (tktProof, error) {
	sha, found, err := r.w.Repo.RemoteHead(r.st.Remote, r.st.Branch)
	if err != nil {
		return tktProof{}, err
	}
	if !found {
		// The state file says pushed and the remote says otherwise. The remote wins:
		// either the push never landed, or somebody deleted the branch. Pushing the
		// same commits again is a no-op where it did land and the repair where it did
		// not, so this is absent and not foreign.
		return tktProof{proofAbsent, ""}, nil
	}
	if r.st.Head != "" && sha == r.st.Head {
		return tktProof{proofMine, fmt.Sprintf("%s on %s is at %s", r.st.Branch, r.st.Remote, shortSha(sha))}, nil
	}
	if r.st.Head == "" {
		return tktProof{proofForeign, fmt.Sprintf("%s already exists on %s (at %s) and this run has nothing recorded to compare it with", r.st.Branch, r.st.Remote, shortSha(sha))}, nil
	}
	ours, err := r.w.Repo.Contains(r.st.Head, sha)
	if err != nil {
		return tktProof{}, err
	}
	if ours {
		// The remote has our commit and more on top of it: somebody built on this
		// branch. There is nothing to push, and nothing to force.
		return tktProof{proofMine, fmt.Sprintf("%s on %s is at %s, which already contains %s — somebody built on it", r.st.Branch, r.st.Remote, shortSha(sha), shortSha(r.st.Head))}, nil
	}
	return tktProof{proofForeign, fmt.Sprintf("%s on %s is at %s, which does not contain %s — refusing to force anything over work this run cannot account for", r.st.Branch, r.st.Remote, shortSha(sha), shortSha(r.st.Head))}, nil
}

func (r *tktRun) doPush() error {
	if err := r.w.Repo.PushBranch(r.st.Remote, r.st.Branch, r.st.Head); err != nil {
		return err
	}
	r.st.PushedSha = r.st.Head
	return nil
}

func (r *tktRun) probePropose() (tktProof, error) {
	// ALWAYS asked, even when the state file records a merge request. The forge is
	// the truth: a run killed between `create` and the save would otherwise open a
	// second merge request for one ticket, which is the exact failure this whole
	// design is built to prevent.
	mr, found, err := r.w.Forge.Find(r.ctx, r.pc.Forge.FindMR, r.st.Branch, r.st.Target)
	if err != nil {
		return tktProof{}, err
	}
	if !found {
		// "None" when this ticket's state file already names one is a CONTRADICTION,
		// not an absence, and the difference is the whole feature: the documented
		// filter is `state: opened`, so the moment a reviewer closes or merges the
		// merge request the find legitimately answers none — and so does a forge that
		// paginates, or a tool that quietly dropped the filter. Creating then is the
		// second merge request for one ticket, which is the single failure this
		// command exists to prevent, and the evidence that stops it is already on
		// disk. Row 2: alert a person, touch nothing.
		if r.st.MergeRequest != nil && (r.st.MergeRequest.ID != "" || r.st.MergeRequest.URL != "") {
			return tktProof{proofForeign, fmt.Sprintf("%s says there is no open merge request for %s, and this ticket already has one recorded: %s. lca will not open a second merge request for one ticket — look at that one (it may have been closed or merged), and remove merge_request from %s if this ticket really does need a new one.",
				r.pc.Forge.FindMR, r.st.Branch, firstNonEmpty(r.st.MergeRequest.URL, r.st.MergeRequest.ID),
				filepath.Join(r.dir, "state.json"))}, nil
		}
		return tktProof{proofAbsent, ""}, nil
	}
	r.st.MergeRequest = &mr
	return tktProof{proofMine, "a merge request for " + r.st.Branch + " already exists: " + firstNonEmpty(mr.URL, mr.ID)}, nil
}

func (r *tktRun) doPropose() error {
	mr, err := r.w.Models.WriteMR(r.ctx, r.stageIn(tktStageMR))
	if err != nil {
		return err
	}
	// Which branch, which target and which ticket are lca's facts and are stamped
	// on here rather than taken from the stage's answer. A model that forgot one of
	// them, or wrote a plausible wrong one, would open the merge request for
	// another branch — and the probe above, which asks the forge about THIS branch,
	// would then never find it and every re-run would open another.
	mr.Branch, mr.Target = r.st.Branch, r.st.Target
	mr.Ticket, mr.Summary = r.st.Ticket, r.st.Summary
	// Scrubbed at the sink, like every other boundary text leaves this program
	// through (redact.go): a merge request description is composed from a check's
	// output and a review, and both of those come from a tree and a stand that may
	// have had a token in them. The stage was already handed a scrubbed tail; this
	// is the write itself, and it is the one that cannot be taken back.
	mr.Title, mr.Body = redactSecrets(mr.Title), redactSecrets(mr.Body)
	made, err := r.w.Forge.Create(r.ctx, r.pc.Forge.CreateMR, mr)
	if err != nil {
		return err
	}
	r.st.MergeRequest = &made
	return nil
}

// marker is the line this run puts in its own ticket comment, and the only way a
// re-run can find out whether it has already commented: a tracker comment has no
// key worth remembering, so the run writes one.
//
// It names the STATE and the round, not the run. That is deliberate — a re-run
// that reaches the same state has nothing new to say and must not post a second
// identical comment, while a re-run that got further (the check was fixed, the
// review passed) has to, and would otherwise be silenced by its own marker.
func (r *tktRun) marker() string {
	blocked := "no"
	if r.st.Blocked != "" {
		blocked = "yes"
	}
	return fmt.Sprintf("<!-- lca-ticket: %s state=%s round=%d blocked=%s -->",
		r.st.Ticket, r.st.workState(), r.st.Round, blocked)
}

// workState is where the WORK stands, which is not always State: a finished run
// records `reported` over it, and a blocked one is reported from the state the
// gate stopped at. The marker is derived from this and not from State, or a
// ticket that was fully reported last night would get a second identical comment
// tonight — the marker would have moved while nothing else had.
func (st *TicketState) workState() string {
	if st.Blocked != "" && st.BlockedAt != "" {
		return st.BlockedAt
	}
	if st.State != tktReported {
		return st.State
	}
	for i := len(st.Journal) - 1; i >= 0; i-- {
		s := st.Journal[i]
		if s.Name != tktReport && s.To != "" && (s.Status == tktOK || s.Status == tktAlready) {
			return s.To
		}
	}
	return st.State
}

// probeReport proves BOTH halves of the report, because it has two and the
// marker only proves one.
//
// The comment is found by the marker; the status move has nothing on the ticket
// that names it, so the run records what it applied. Without that second half, a
// process killed between the comment and the move found its own marker on every
// later night, recorded `report already`, exited 0 — and the ticket sat in its
// original status for ever while the journal and the result object both said it
// had been reported. A team that configured status_done/status_blocked wants the
// move to happen; the block refuses half of that trio precisely because of it.
func (r *tktRun) probeReport() (tktProof, error) {
	r.st.Marker = r.marker()
	// The READ tool, not the comment tool: looking for our own comment is a read of
	// the ticket, and the block configures no comment-search tool because lca does
	// not get to assume this tracker has one.
	id, found, err := r.w.Tracker.FindComment(r.ctx, r.pc.Tracker.Read, r.st.Ticket, r.st.Marker)
	if err != nil {
		return tktProof{}, err
	}
	if !found {
		return tktProof{proofAbsent, ""}, nil
	}
	r.st.CommentID = id
	r.commented = true
	if want := r.reportStatus(); want != "" && r.st.MovedTo != want {
		// The comment is there and the move is not. Re-entered, with the comment call
		// skipped: a second identical comment is exactly what the marker exists to
		// prevent, and the move is the half still owed.
		return tktProof{proofAbsent, ""}, nil
	}
	return tktProof{proofMine, "the ticket already carries this run's comment" + r.movedNote()}, nil
}

func (r *tktRun) movedNote() string {
	if r.st.MovedTo == "" {
		return ""
	}
	return ", and its status was moved to " + r.st.MovedTo
}

// reportStatus is which status the move applies, and it is decided by where the
// WORK stands — not by whether a gate happened to write a sentence.
//
// That used to be `if st.Blocked != "" then blocked else done`, and every path
// that ended in an ERROR rather than at a gate leaves Blocked empty: a forge
// outage, a coder that produced nothing, the row-2 refusal whose own comment says
// "touch nothing". All three moved the ticket to status_done, so the board told a
// person the work was ready for review when no merge request existed anywhere —
// the exact inverse of what this transition is for. Where the work stands is a
// fact lca holds; whether a sentence was written is not.
func (r *tktRun) reportStatus() string {
	if r.pc.Tracker.Transition == "" {
		return ""
	}
	if tktOrder(r.st.workState()) >= tktOrder(r.doneState()) {
		return r.pc.Tracker.StatusDone
	}
	return r.pc.Tracker.StatusBlocked
}

// doneState is the state a finished run of THIS pipeline reaches: `proposed`
// when it may push, `merged` when push: false means it cannot. Anything short of
// it is work a person still has to look at, whatever stopped it.
func (r *tktRun) doneState() string {
	if r.pc.pipelinePush() {
		return tktProposed
	}
	return tktMerged
}

func (r *tktRun) doReport() error {
	if !r.commented {
		body, err := r.w.Models.WriteComment(r.ctx, r.stageIn(tktStageReport))
		if err != nil {
			// The stage could not be bought — the gateway is down, or the night's clock
			// ran out, which is the commonest unattended failure there is. That is not a
			// reason for the ticket to hear nothing: lca holds every fact this comment
			// needs and writes it from the journal, which is what tktOutcomeText is.
			// Going quiet is the failure this whole transition exists to prevent.
			warnLine("the comment could not be written by a model (%v), so lca writes it from the journal instead", err)
			body = tktOutcomeText(r.st)
		}
		// Scrubbed at the sink, for doPropose's reason and with more at stake: this
		// comment is the one thing a person reads tomorrow, it is composed from a
		// check's output, and a tracker comment cannot be unpublished.
		id, cerr := r.w.Tracker.Comment(r.ctx, r.pc.Tracker.Comment, r.st.Ticket,
			redactSecrets(body)+"\n\n"+r.st.Marker)
		if cerr != nil {
			return cerr
		}
		r.st.CommentID = id
		r.commented = true
	}
	// The status, only when the team configured one and only the one the work has
	// earned. Recorded after the call returns, because the marker cannot prove it.
	status := r.reportStatus()
	if status == "" {
		return nil
	}
	if err := r.w.Tracker.Move(r.ctx, r.pc.Tracker.Transition, r.st.Ticket, status); err != nil {
		return err
	}
	r.st.MovedTo = status
	return nil
}

// shortSha is the display form. Seven, because that is what git itself prints
// and what a person compares against `git log --oneline`.
func shortSha(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
