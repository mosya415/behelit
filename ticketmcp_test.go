package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tracker and the forge, against a STUB SERVER written here.
//
// The stub answers at the one seam that matters: the MCP call. Below it — the
// handshake, the reconnect, the timeout, the secret scrub, the approval gate —
// has its own tests in mcp_test.go, and standing a second mock under them here
// would only test the mock. Above it is everything this file is about: that lca
// substitutes the operator's own argument names and nothing else, that it reads
// the reply only down the paths the block gave it, and that calling any of it
// twice does what calling it once did.
//
// The stub is deliberately NOT the Jira or GitLab shape. It spells its arguments
// and its reply keys the way a third installation would, because the whole claim
// of ticketcfg.go is that lca does not know those names — and a test written
// against the shapes the README happens to use would let a hardcoded `issueKey`
// through.

// ── the stub ────────────────────────────────────────────────────────────────

type stubCall struct {
	tool string
	args map[string]any
}

// stubServer is a tracker and a forge with just enough memory to be asked the
// same thing twice.
type stubServer struct {
	t *testing.T

	summary  string
	body     string
	comments []string
	status   string
	nextKey  string
	created  []TicketBody

	mrs    []map[string]any
	mrSeq  int
	ignore bool // a find tool that pays no attention to the branch it was asked about

	fail map[string]error
	seen []stubCall
}

func newStubServer(t *testing.T) *stubServer {
	return &stubServer{t: t, summary: "the stand drops requests over 8k",
		body:    "Anything over 8k comes back 400. It should be accepted.",
		nextKey: "BSK-9", fail: map[string]error{}}
}

// stubPipeline is the complete block for the stub's own spelling. Every argument
// name and every path here is this imaginary installation's, which is the point:
// nothing in lca may recognise any of them.
const stubPipeline = `
pipeline:
  project: BSK
  branch_prefix: agent/
  remote: origin
  target_branch: main
  push: true
  rework_rounds: 1
  roles:
    coder: coder
    reviewer: reviewer
    integrator: integrator
  tracker:
    read: trk__get
    create: trk__open
    comment: trk__note
    transition: trk__move
    status_done: Ready for review
    status_blocked: Needs a human
    args:
      read: {ticket_ref: "${key}"}
      create: {in_project: "${project}", headline: "${summary}", detail: "${body}", kind: Task, points: 3, urgent: false}
      comment: {ticket_ref: "${key}", note: "${text}"}
      transition: {ticket_ref: "${key}", to_state: "${status}"}
    fields:
      key: data.ref
      summary: data.headline
      body: data.detail
      comment_id: data.note_id
  forge:
    create_merge_request: fg__open_request
    find_merge_request: fg__requests
    args:
      create_merge_request: {from_ref: "${branch}", onto_ref: "${target}", headline: "${title}", detail: "${body}"}
      find_merge_request: {from_ref: "${branch}", onto_ref: "${target}", open: true}
    fields:
      list: data.requests
      branch: from_ref
      target: onto_ref
      id: number
      url: at
      title: headline
`

func (s *stubServer) Call(_ context.Context, tool string, args map[string]any) (string, error) {
	s.seen = append(s.seen, stubCall{tool: tool, args: args})
	if err := s.fail[tool]; err != nil {
		return "", err
	}
	switch tool {
	case "trk__get":
		notes := make([]map[string]any, 0, len(s.comments))
		for i, c := range s.comments {
			notes = append(notes, map[string]any{"note_id": fmt.Sprintf("n%d", i+1), "note": c})
		}
		return s.json(map[string]any{"data": map[string]any{
			"ref": args["ticket_ref"], "headline": s.summary, "detail": s.body,
			"state": s.status, "notes": notes}}), nil
	case "trk__open":
		s.created = append(s.created, TicketBody{
			Summary: fmt.Sprint(args["headline"]), Body: fmt.Sprint(args["detail"])})
		s.summary, s.body = fmt.Sprint(args["headline"]), fmt.Sprint(args["detail"])
		return s.json(map[string]any{"data": map[string]any{"ref": s.nextKey}}), nil
	case "trk__note":
		s.comments = append(s.comments, fmt.Sprint(args["note"]))
		return s.json(map[string]any{"data": map[string]any{"note_id": fmt.Sprintf("n%d", len(s.comments))}}), nil
	case "trk__move":
		s.status = fmt.Sprint(args["to_state"])
		return s.json(map[string]any{"data": map[string]any{"state": s.status}}), nil
	case "fg__requests":
		want, _ := args["from_ref"].(string)
		out := []map[string]any{}
		for _, mr := range s.mrs {
			if s.ignore || mr["from_ref"] == want {
				out = append(out, mr)
			}
		}
		return s.json(map[string]any{"data": map[string]any{"requests": out}}), nil
	case "fg__open_request":
		s.mrSeq++
		mr := map[string]any{"number": s.mrSeq, "at": fmt.Sprintf("https://forge/r/%d", s.mrSeq),
			"headline": args["headline"], "from_ref": args["from_ref"], "onto_ref": args["onto_ref"],
			"detail": args["detail"]}
		s.mrs = append(s.mrs, mr)
		return s.json(mr), nil
	}
	s.t.Fatalf("the stub was called with %q, which no key in the block names", tool)
	return "", nil
}

func (s *stubServer) json(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

func (s *stubServer) calls(tool string) []stubCall {
	var out []stubCall
	for _, c := range s.seen {
		if c.tool == tool {
			out = append(out, c)
		}
	}
	return out
}

func stubWorld(t *testing.T) (*stubServer, *PipelineConfig, tktTrackerCalls, tktForgeCalls) {
	t.Helper()
	s := newStubServer(t)
	pc := pipeFrom(t, stubPipeline)
	return s, pc, tktTrackerCalls{call: s, pc: pc}, tktForgeCalls{call: s, pc: pc}
}

// ── the arguments lca sends are the operator's own ──────────────────────────

func TestEveryCallSendsTheArgumentsTheBlockNamedAndNothingElse(t *testing.T) {
	s, pc, trk, fg := stubWorld(t)
	ctx := context.Background()
	if _, err := trk.Read(ctx, pc.Tracker.Read, "BSK-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := trk.Comment(ctx, pc.Tracker.Comment, "BSK-1", "what happened"); err != nil {
		t.Fatal(err)
	}
	if err := trk.Move(ctx, pc.Tracker.Transition, "BSK-1", "Ready for review"); err != nil {
		t.Fatal(err)
	}
	if _, err := trk.Create(ctx, pc.Tracker.Create, "BSK", TicketBody{Summary: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fg.Find(ctx, pc.Forge.FindMR, "agent/BSK-1", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := fg.Create(ctx, pc.Forge.CreateMR, TicketMR{Title: "T", Body: "B",
		Branch: "agent/BSK-1", Target: "main"}); err != nil {
		t.Fatal(err)
	}

	want := []struct {
		tool string
		args map[string]any
	}{
		{"trk__get", map[string]any{"ticket_ref": "BSK-1"}},
		{"trk__note", map[string]any{"ticket_ref": "BSK-1", "note": "what happened"}},
		{"trk__move", map[string]any{"ticket_ref": "BSK-1", "to_state": "Ready for review"}},
		// The literals are read as the JSON they spell: a tool whose `points` is an
		// integer and whose `urgent` is a boolean cannot be called with "3" and
		// "false", and a quoted value is how a string is forced.
		{"trk__open", map[string]any{"in_project": "BSK", "headline": "t", "detail": "b",
			"kind": "Task", "points": float64(3), "urgent": false}},
		// Both ends: a merge request from this branch into a target lca did not ask
		// about is somebody else's and must not be adopted as this ticket's.
		{"fg__requests", map[string]any{"from_ref": "agent/BSK-1", "onto_ref": "main", "open": true}},
		{"fg__open_request", map[string]any{"from_ref": "agent/BSK-1", "onto_ref": "main",
			"headline": "T", "detail": "B"}},
	}
	for i, w := range want {
		got := s.calls(w.tool)
		if len(got) != 1 {
			t.Fatalf("%s was called %d times", w.tool, len(got))
		}
		if fmt.Sprint(sortedKeys(got[0].args)) != fmt.Sprint(sortedKeys(w.args)) {
			t.Fatalf("%s got the arguments %v, the block names %v", w.tool,
				sortedKeys(got[0].args), sortedKeys(w.args))
		}
		for k, v := range w.args {
			if fmt.Sprintf("%#v", got[0].args[k]) != fmt.Sprintf("%#v", v) {
				t.Fatalf("call %d: %s: %s = %#v, want %#v", i, w.tool, k, got[0].args[k], v)
			}
		}
	}
}

func TestTheTicketIsReadDownThePathsTheBlockNamed(t *testing.T) {
	s, pc, trk, _ := stubWorld(t)
	got, err := trk.Read(context.Background(), pc.Tracker.Read, "BSK-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != s.summary || got.Body != s.body || got.Key != "BSK-1" {
		t.Fatalf("read gave %+v", got)
	}

	// With no `fields: body`, the whole reply is handed over: it names nothing and
	// guesses nothing, and the alternative is a coder sent to implement a title.
	delete(pc.Tracker.Fields.By, "body")
	got, err = trk.Read(context.Background(), pc.Tracker.Read, "BSK-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Body, s.body) || !strings.HasPrefix(strings.TrimSpace(got.Body), "{") {
		t.Fatalf("want the whole reply as the body, got %q", truncate(got.Body, 120))
	}
}

func TestAPathThatIsNotThereNamesTheKeyThePathAndTheReply(t *testing.T) {
	_, pc, trk, _ := stubWorld(t)
	pc.Tracker.Fields.By["summary"] = "data.title"
	_, err := trk.Read(context.Background(), pc.Tracker.Read, "BSK-1")
	if err == nil {
		t.Fatal("a path that resolves to nothing must not become an empty summary")
	}
	for _, want := range []string{"tracker: fields: summary", "data.title", "trk__get", "headline"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must name %q — the person reading it has a ticket that did not move and one line of log:\n%v", want, err)
		}
	}
	var ue *usageErr
	if !asUsageErr(err, &ue) {
		t.Fatalf("a path nobody wrote correctly is a configuration mistake: %T", err)
	}
}

func TestOpeningATicketWithNoReadableKeyIsRefused(t *testing.T) {
	s, pc, trk, _ := stubWorld(t)
	s.nextKey = ""
	_, err := trk.Create(context.Background(), pc.Tracker.Create, "BSK", TicketBody{Summary: "t", Body: "b"})
	if err == nil {
		t.Fatal("a ticket was opened and its key could not be read — that must be said, not papered over")
	}
	if !strings.Contains(err.Error(), "tracker: fields: key") {
		t.Fatalf("and the key named:\n%v", err)
	}
}

// ── the comment, and finding it again ───────────────────────────────────────

func TestTheRunFindsItsOwnCommentAndOnlyItsOwn(t *testing.T) {
	s, pc, trk, _ := stubWorld(t)
	ctx := context.Background()
	marker := "<!-- lca-ticket: BSK-1 state=proposed round=0 blocked=no -->"

	if _, found, err := trk.FindComment(ctx, pc.Tracker.Read, "BSK-1", marker); err != nil || found {
		t.Fatalf("nothing is there yet: %v %v", found, err)
	}
	// Somebody else's comment, and a previous night's marker for a different state.
	s.comments = append(s.comments, "a person: please rebase",
		"<!-- lca-ticket: BSK-1 state=implemented round=0 blocked=yes -->")
	if _, found, _ := trk.FindComment(ctx, pc.Tracker.Read, "BSK-1", marker); found {
		t.Fatal("last night's marker is not this run's")
	}

	id, err := trk.Comment(ctx, pc.Tracker.Comment, "BSK-1", "the work is done\n\n"+marker)
	if err != nil {
		t.Fatal(err)
	}
	if id != "n3" {
		t.Fatalf("the comment's own id comes from the configured path, got %q", id)
	}
	// Twice, which is the resume: found both times, and nothing is posted again.
	for i := 0; i < 2; i++ {
		_, found, err := trk.FindComment(ctx, pc.Tracker.Read, "BSK-1", marker)
		if err != nil || !found {
			t.Fatalf("re-entry %d: %v %v", i, found, err)
		}
	}
	if n := len(s.calls("trk__note")); n != 1 {
		t.Fatalf("the ticket was commented on %d times", n)
	}
}

func TestACommentIdPathIsOptionalAndAWrongOneIsNot(t *testing.T) {
	_, pc, trk, _ := stubWorld(t)
	delete(pc.Tracker.Fields.By, "comment_id")
	if id, err := trk.Comment(context.Background(), pc.Tracker.Comment, "BSK-1", "x"); err != nil || id != "" {
		t.Fatalf("an unnamed comment id is bookkeeping nobody asked for: %q %v", id, err)
	}
	pc.Tracker.Fields.By["comment_id"] = "data.nope"
	if _, err := trk.Comment(context.Background(), pc.Tracker.Comment, "BSK-1", "x"); err == nil {
		t.Fatal("a path that WAS named and misses is how a state file starts lying")
	}
}

// ── never a second merge request ────────────────────────────────────────────

func TestTheForgeIsAskedBeforeItIsToldAndOnlyOurBranchCounts(t *testing.T) {
	s, pc, _, fg := stubWorld(t)
	ctx := context.Background()
	branch := "agent/BSK-1"

	if _, found, err := fg.Find(ctx, pc.Forge.FindMR, branch, "main"); err != nil || found {
		t.Fatalf("there is none yet: %v %v", found, err)
	}
	made, err := fg.Create(ctx, pc.Forge.CreateMR, TicketMR{Title: "BSK-1 accept 8k",
		Body: "what changed", Branch: branch, Target: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if made.URL != "https://forge/r/1" || made.ID != "1" {
		t.Fatalf("the merge request came back as %+v", made)
	}
	// The resume, twice. Find answers and Create is never reached again.
	for i := 0; i < 2; i++ {
		got, found, err := fg.Find(ctx, pc.Forge.FindMR, branch, "main")
		if err != nil || !found {
			t.Fatalf("re-entry %d: %v %v", i, found, err)
		}
		if got.URL != made.URL {
			t.Fatalf("re-entry %d found %q, ours is %q", i, got.URL, made.URL)
		}
	}
	if n := len(s.calls("fg__open_request")); n != 1 {
		t.Fatalf("%d merge requests were opened for one ticket", n)
	}
}

func TestAFindToolThatIgnoresTheFilterCannotHandUsSomebodyElsesWork(t *testing.T) {
	s, pc, _, fg := stubWorld(t)
	// The failure this guard exists for: a tool that pays no attention to an
	// argument it does not recognise and answers with every open request there is.
	s.ignore = true
	s.mrs = []map[string]any{
		{"number": 7, "at": "https://forge/r/7", "from_ref": "feature/somebody-else", "onto_ref": "main", "headline": "not ours"},
		{"number": 8, "at": "https://forge/r/8", "from_ref": "hotfix/ops", "onto_ref": "main", "headline": "also not ours"},
		// The other half of the same hole: OUR branch, somebody else's target. A
		// find that matched the source alone adopted this and reported the ticket
		// proposed, while nothing was ever proposed against the branch the work
		// merged into.
		{"number": 11, "at": "https://forge/r/11", "from_ref": "agent/BSK-1", "onto_ref": "staging", "headline": "ours, but into staging"},
	}
	got, found, err := fg.Find(context.Background(), pc.Forge.FindMR, "agent/BSK-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatalf("a merge request for another branch or another target was adopted as this ticket's: %+v", got)
	}
	// And ours, in the same unfiltered answer, is found.
	s.mrs = append(s.mrs, map[string]any{"number": 9, "at": "https://forge/r/9",
		"from_ref": "agent/BSK-1", "onto_ref": "main", "headline": "ours"})
	got, found, err = fg.Find(context.Background(), pc.Forge.FindMR, "agent/BSK-1", "main")
	if err != nil || !found || got.ID != "9" {
		t.Fatalf("ours must be picked out of the list: %+v %v %v", got, found, err)
	}
}

func TestAForgeThatAnswersInProseIsRefusedRatherThanGuessedAt(t *testing.T) {
	_, pc, _, _ := stubWorld(t)
	pc.Forge.Fields.By["list"] = tktWholeReply
	// A server that says "there are no open requests" in words. Reading that as
	// "none" would be a guess, and the cost of guessing wrong here is a second
	// merge request for one ticket.
	fg := tktForgeCalls{call: &proseCaller{text: "There are currently no open requests for that branch."}, pc: pc}
	_, _, err := fg.Find(context.Background(), pc.Forge.FindMR, "agent/BSK-1", "main")
	if err == nil {
		t.Fatal("prose where a list was expected must be refused")
	}
	for _, want := range []string{"forge: fields: list", "cannot tell whether one exists"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in:\n%v", want, err)
		}
	}
}

type proseCaller struct{ text string }

func (p *proseCaller) Call(context.Context, string, map[string]any) (string, error) {
	return p.text, nil
}

func TestAMergeRequestWithNoAddressIsRefused(t *testing.T) {
	_, pc, _, _ := stubWorld(t)
	fg := tktForgeCalls{call: &proseCaller{text: `{"ok":true}`}, pc: pc}
	_, err := fg.Create(context.Background(), pc.Forge.CreateMR, TicketMR{Title: "T", Branch: "agent/x", Target: "main"})
	if err == nil {
		t.Fatal("a comment that says a merge request exists without saying where is a comment nobody can act on")
	}
	if !strings.Contains(err.Error(), "forge: fields") {
		t.Fatalf("and the keys are named:\n%v", err)
	}
}

// ── which row of the table a failure is ─────────────────────────────────────

func TestATransportFailureIsInfraAndAnAnsweredCallIsNot(t *testing.T) {
	_, pc, _, _ := stubWorld(t)
	ctx := context.Background()

	// The server was not there. That is somebody else's machine: the ticket goes
	// back in the queue and nothing on it is touched.
	trkDown := tktTrackerCalls{call: &failCaller{err: tktCallErr(context.Background(), "trk__get",
		&mcpCallErr{msg: "error: connection refused", infra: true, err: errors.New("dial tcp: connection refused")})}, pc: pc}
	_, err := trkDown.Read(ctx, pc.Tracker.Read, "BSK-1")
	if status, _ := classifyRunErr(err); status != statusInfra {
		t.Fatalf("a tracker that is down is infra_error, got %s from %v", status, err)
	}

	// The server answered, with "no". The tracker has an opinion about this call
	// and it is not an outage.
	trkNo := tktTrackerCalls{call: &failCaller{err: tktCallErr(context.Background(), "trk__get",
		&mcpCallErr{msg: "error: trk__get failed: no such ticket"})}, pc: pc}
	_, err = trkNo.Read(ctx, pc.Tracker.Read, "BSK-1")
	if status, _ := classifyRunErr(err); status != statusFailed {
		t.Fatalf("a server that answered is not an outage, got %s from %v", status, err)
	}

	// The gate said no, so nothing left this machine. A grant is missing from a
	// file, which is a thing to fix and not a thing to retry tonight.
	trkDenied := tktTrackerCalls{call: &failCaller{err: tktCallErr(context.Background(), "trk__note",
		&mcpCallErr{msg: "error: mcp_write is not granted in an unattended run", denied: true})}, pc: pc}
	_, err = trkDenied.Comment(ctx, pc.Tracker.Comment, "BSK-1", "x")
	if status, _ := classifyRunErr(err); status != statusConfig {
		t.Fatalf("an ungranted write is row 2, got %s from %v", status, err)
	}
	if !strings.Contains(err.Error(), "not -y") {
		t.Fatalf("and it says what does not grant it:\n%v", err)
	}
	// Cancellation keeps its identity all the way out, so a SIGTERM is 130 and
	// never "the agent did not manage it".
	trkCut := tktTrackerCalls{call: &failCaller{err: tktCallErr(context.Background(), "trk__get",
		&mcpCallErr{msg: "error: trk__get was interrupted", err: context.Canceled})}, pc: pc}
	_, err = trkCut.Read(ctx, pc.Tracker.Read, "BSK-1")
	if status, _ := classifyRunErr(err); status != statusCancelled {
		t.Fatalf("an interrupted call is cancelled, got %s from %v", status, err)
	}
}

type failCaller struct{ err error }

func (f *failCaller) Call(context.Context, string, map[string]any) (string, error) {
	return "", f.err
}

func TestAnUnconfiguredCallNamesTheKeyAndNeverGuesses(t *testing.T) {
	s, pc, trk, fg := stubWorld(t)
	delete(pc.Tracker.Args.By, tktVerbRead)
	if _, err := trk.Read(context.Background(), pc.Tracker.Read, "BSK-1"); err == nil ||
		!strings.Contains(err.Error(), "tracker: args: read") {
		t.Fatalf("want the key named: %v", err)
	}
	delete(pc.Forge.Args.By, tktVerbFindMR)
	if _, _, err := fg.Find(context.Background(), pc.Forge.FindMR, "agent/x", "main"); err == nil ||
		!strings.Contains(err.Error(), "forge: args: find_merge_request") {
		t.Fatalf("want the key named: %v", err)
	}
	if len(s.seen) != 0 {
		t.Fatalf("nothing may be sent with a half-configured call: %v", s.seen)
	}
}

// ── the whole machine over the real tracker and forge ───────────────────────

// TestTheWholeArcOverTheStubServerIsIdempotent drives the state machine itself
// through the real tracker and forge implementations, twice, which is the
// property the command exists for: the cron line is the same line whatever
// happened last night.
func TestTheWholeArcOverTheStubServerIsIdempotent(t *testing.T) {
	s, pc, trk, fg := stubWorld(t)
	fake := newFakeWorld(t)
	world := tktWorld{Tracker: trk, Forge: fg, Repo: fake, Models: fake}

	r := newRun(t, fake, pc, &TicketState{Ticket: "BSK-1", State: tktStart})
	r.w = world
	if status, reason := r.execute(); status != statusPassed {
		t.Fatalf("the first run must finish: %s %s", status, reason)
	}
	if r.st.State != tktReported {
		t.Fatalf("it reached %q", r.st.State)
	}
	if r.st.Summary != s.summary {
		t.Fatalf("the ticket's own summary must come from the tracker: %q", r.st.Summary)
	}
	if r.st.MergeRequest == nil || r.st.MergeRequest.URL != "https://forge/r/1" {
		t.Fatalf("merge request: %+v", r.st.MergeRequest)
	}
	if s.status != "Ready for review" {
		t.Fatalf("the status move is lca's own call: %q", s.status)
	}

	// Night two, and night three. Nothing is opened, nothing is commented, nothing
	// is moved that was not already.
	for night := 2; night <= 3; night++ {
		again := newRun(t, fake, pc, r.st)
		again.w = world
		if status, reason := again.execute(); status != statusPassed {
			t.Fatalf("night %d: %s %s", night, status, reason)
		}
	}
	if n := len(s.calls("fg__open_request")); n != 1 {
		t.Fatalf("%d merge requests for one ticket", n)
	}
	if n := len(s.calls("trk__note")); n != 1 {
		t.Fatalf("%d comments for one state — a re-run that reached the same state has nothing new to say", n)
	}
	if n := len(s.calls("trk__open")); n != 0 {
		t.Fatalf("a run given a key must not open a ticket: %d", n)
	}
	if n := fake.calls["cutbranch"]; n != 1 {
		t.Fatalf("the branch was cut %d times", n)
	}
}

// And the blocked half: the ticket is told, with the blocked status, and told
// once per state and not once per night.
func TestABlockedRunTellsTheTicketThroughTheRealTracker(t *testing.T) {
	s, pc, trk, fg := stubWorld(t)
	fake := newFakeWorld(t)
	two := 2
	fake.check = TicketCheck{Cmd: "go test ./...", Exit: &two, Attempts: 2, Tail: "FAIL: 1 of 412"}
	world := tktWorld{Tracker: trk, Forge: fg, Repo: fake, Models: fake}

	r := newRun(t, fake, pc, &TicketState{Ticket: "BSK-1", State: tktStart})
	r.w = world
	if status, _ := r.execute(); status != statusFailed {
		t.Fatal("a red check ends the run at implemented")
	}
	if s.status != "Needs a human" {
		t.Fatalf("the blocked status is the one the block names for blocked: %q", s.status)
	}
	if len(s.calls("trk__note")) != 1 {
		t.Fatalf("the ticket must hear about it: %v", s.comments)
	}
	if !strings.Contains(s.comments[0], "lca-ticket:") {
		t.Fatalf("and the comment carries the marker a re-run finds it by: %q", s.comments[0])
	}

	again := newRun(t, fake, pc, r.st)
	again.w = world
	if status, _ := again.execute(); status != statusFailed {
		t.Fatal("still blocked")
	}
	if n := len(s.calls("trk__note")); n != 1 {
		t.Fatalf("a ticket must not collect one identical comment per night: %d", n)
	}
}

// ── the paths themselves ────────────────────────────────────────────────────

func TestADottedPathWalksObjectsAndArraysAndNothingElse(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(`{"a":{"b":[{"c":"x"},{"c":"y"}]},"n":42,"t":true,"z":null}`), &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, want string
		ok         bool
	}{
		{".", "", true}, // the whole reply, rendered as JSON by the caller
		{"a.b.0.c", "x", true},
		{"a.b.1.c", "y", true},
		{"n", "42", true},
		{"t", "true", true},
		{"z", "", true},
		{"a.b.2.c", "", false},
		{"a.c", "", false},
		{"n.anything", "", false},
	} {
		got, ok := tktPick(v, c.path)
		if ok != c.ok {
			t.Fatalf("%q resolved=%v, want %v", c.path, ok, c.ok)
		}
		if ok && c.path != "." && tktText(got) != c.want {
			t.Fatalf("%q = %q, want %q", c.path, tktText(got), c.want)
		}
	}
	// A rich-text field is a DOCUMENT in more than one tracker. It is handed over
	// as the JSON it is rather than described, because the model reading it can
	// read JSON and inventing a renderer for somebody's format would be a guess.
	doc, _ := tktPick(v, "a")
	if !strings.HasPrefix(tktText(doc), `{"b":[`) {
		t.Fatalf("an object must come back as its own JSON: %q", tktText(doc))
	}
}

func TestAPlaceholderTheVerbHasNoValueForIsRefusedAtLoad(t *testing.T) {
	for _, bad := range []struct{ yaml, want string }{
		{"pipeline:\n  tracker:\n    args:\n      read: {k: \"${tikcet}\"}\n", "${tikcet} is not a value"},
		{"pipeline:\n  tracker:\n    args:\n      read: {k: \"${status}\"}\n", "for read it substitutes ${key}"},
		{"pipeline:\n  tracker:\n    args:\n      reed: {k: \"${key}\"}\n", "is not one of this block's calls"},
		{"pipeline:\n  forge:\n    args:\n      find_merge_request: {k: \"${title}\"}\n", "${title} is not a value"},
		{"pipeline:\n  tracker:\n    fields:\n      sumary: x\n", "is not a value lca looks for"},
		{"pipeline:\n  forge:\n    fields:\n      url:\n", "has no path"},
	} {
		_, err := parsePipeline(nil, parseYAMLish(bad.yaml), "roles.yaml")
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Fatalf("%q: want %q, got %v", bad.yaml, bad.want, err)
		}
	}
}

func TestACallWithNoPlaceholderForWhatItIsAboutIsNamed(t *testing.T) {
	// Present but useless: a read with no ${key} reads whatever the tool's default
	// is, which is a run that works on the wrong ticket instead of refusing.
	yaml := strings.Replace(stubPipeline, `read: {ticket_ref: "${key}"}`, `read: {ticket_ref: "BSK-1"}`, 1)
	err := pipeFrom(t, yaml).validate(nil, tktNeed{})
	if err == nil || !strings.Contains(err.Error(), "tracker: args: read") ||
		!strings.Contains(err.Error(), "${key}") {
		t.Fatalf("want the key and the missing placeholder named:\n%v", err)
	}
	yaml = strings.Replace(stubPipeline, `find_merge_request: {from_ref: "${branch}", onto_ref: "${target}", open: true}`,
		`find_merge_request: {open: true}`, 1)
	err = pipeFrom(t, yaml).validate(nil, tktNeed{})
	if err == nil || !strings.Contains(err.Error(), "forge: args: find_merge_request") {
		t.Fatalf("a find with no branch asks for every open merge request there is:\n%v", err)
	}
}

func TestTheStubBlockValidatesAsAWholeAndSoDoesTheDocumentedOne(t *testing.T) {
	for _, yaml := range []string{stubPipeline, fullPipeline} {
		if err := pipeFrom(t, yaml).validate(nil, tktNeed{creating: true}); err != nil {
			t.Fatalf("a complete block must validate: %v", err)
		}
	}
}

// The two implementations satisfy the interfaces the machine drives.
var (
	_ tktTracker = tktTrackerCalls{}
	_ tktForge   = tktForgeCalls{}
	_ tktModels  = (*tktStageRun)(nil)
)

// ── the grant, through the real machinery ───────────────────────────────────

// TestOnlyLcasOwnSessionHoldsTheWriteGrant is the control the whole command
// rests on, exercised against the real MCP client, the real registry and the
// real approval gate: the tracker's write tool is reachable by lca and by
// nothing else in the run.
func TestOnlyLcasOwnSessionHoldsTheWriteGrant(t *testing.T) {
	t.Setenv("JIRA_MCP_TOKEN", "tok-abcdef")
	m := newMockMCP(t, func(m *mockMCP) {
		m.callResult = []map[string]any{{"type": "text", "text": `{"data":{"note_id":"n1"}}`}}
	})
	f := mcpFix(t, jiraConfig(m.hostPort(), m.URL, ""), lockOf("jira", m.URL, m.tools))
	h := mcpSession(t, f, "mcp")

	pc := pipeFrom(t, stubPipeline)
	// This installation's own names, which is the whole point: the block is
	// re-pointed at the mock's tools and nothing in lca notices or cares.
	pc.Integrator = "build"
	pc.Tracker.Read, pc.Tracker.Comment = "jira__issue_get", "jira__issue_comment_add"
	pc.Tracker.Args.By[tktVerbRead] = map[string]string{"issueKey": "${key}"}
	pc.Tracker.Args.Order[tktVerbRead] = []string{"issueKey"}
	pc.Tracker.Args.By[tktVerbComment] = map[string]string{"issueKey": "${key}", "body": "${text}"}
	pc.Tracker.Args.Order[tktVerbComment] = []string{"issueKey", "body"}

	mc, err := newTktMCP(h.orch, h.sess, pc, tktNeed{})
	if err != nil {
		t.Fatal(err)
	}
	trk := tktTrackerCalls{call: mc, pc: pc}
	if _, err := trk.Comment(context.Background(), pc.Tracker.Comment, "OPS-1", "what happened"); err != nil {
		t.Fatalf("lca's own session holds the grant for the tool the block named: %v", err)
	}

	// The same call from a STAGE's session. newChild denies mcp_write structurally
	// — toolsFor never even shows a write schema to the model — and this proves the
	// other half: the call itself is refused at the gate, so injected ticket text
	// could not reach it either.
	stage, err := h.orch.newChild(h.sess, h.orch.agents["build"], "one stage of a ticket")
	if err != nil {
		t.Fatal(err)
	}
	ungranted := tktTrackerCalls{call: &tktMCP{orch: h.orch, sess: stage}, pc: pc}
	_, err = ungranted.Comment(context.Background(), pc.Tracker.Comment, "OPS-1", "from a stage")
	if err == nil {
		t.Fatal("a model's stage must not be able to write to the tracker")
	}
	if status, _ := classifyRunErr(err); status != statusConfig {
		t.Fatalf("an ungranted write is row 2 of the table, got %s from %v", status, err)
	}

	// A tool the block did NOT name is not granted either, so one grant is not a
	// grant for the server.
	pc.Tracker.Comment = "jira__issue_get" // a read tool, which needs none
	if _, err := trk.Comment(context.Background(), "jira__issue_get", "OPS-1", "x"); err != nil {
		t.Fatalf("a read needs no grant: %v", err)
	}
	if n := m.count("tools/call"); n < 2 {
		t.Fatalf("the calls must have actually reached the server: %d", n)
	}
}

// A tracker comment and a merge request description are WRITE boundaries, and
// what goes through them is composed from a check's output.
func TestSecretsDoNotReachTheTrackerOrTheForge(t *testing.T) {
	t.Setenv("BSK_DEPLOY_TOKEN", "glpat-abcdefghij1234567890")
	// envSecrets() is read once per process, so a test that sets a variable has to
	// drop the memo — the same thing redact_test.go does.
	secretsOnce = sync.Once{}
	secretVals = nil
	t.Cleanup(func() { secretsOnce = sync.Once{}; secretVals = nil })
	s, pc, trk, fg := stubWorld(t)
	fake := newFakeWorld(t)
	two := 2
	fake.check = TicketCheck{Cmd: "./check.sh", Exit: &two, Attempts: 2,
		Tail: "deploy failed: Authorization: Bearer glpat-abcdefghij1234567890"}
	fake.commentBody = func(in tktStageIn) string {
		// A model doing the ordinary thing: quoting the output it was handed. It was
		// handed a scrubbed tail, and the sink scrubs again whatever it writes.
		return "the check failed:\n" + in.Check.Tail + "\nand the raw token glpat-abcdefghij1234567890"
	}
	world := tktWorld{Tracker: trk, Forge: fg, Repo: fake, Models: fake}
	r := newRun(t, fake, pc, &TicketState{Ticket: "BSK-1", State: tktStart})
	r.w = world
	if status, _ := r.execute(); status != statusFailed {
		t.Fatal("a red check ends the run at implemented")
	}
	if len(s.comments) != 1 {
		t.Fatalf("comments %v", s.comments)
	}
	if strings.Contains(s.comments[0], "glpat-abcdefghij1234567890") {
		t.Fatalf("a secret reached the tracker, which cannot be unpublished:\n%s", s.comments[0])
	}
	if !strings.Contains(s.comments[0], redactedMark) {
		t.Fatalf("and it must be visible that something was removed:\n%s", s.comments[0])
	}
	// The state file too: it is a file a person opens to find out why a stage
	// stopped, and it carries the tail.
	on, err := loadTicketState(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(on.Check.Tail, "glpat-abcdefghij1234567890") {
		t.Fatalf("the state file carries the secret: %q", on.Check.Tail)
	}
	// And the tail the stage was handed was already scrubbed, so the model never
	// saw it in the first place.
	if strings.Contains(tktCheckText(fake.check), "glpat-abcdefghij1234567890") {
		t.Fatal("the stage was handed the secret")
	}
	// And the result object, which is the field a wrapper pastes into a public
	// merge request.
	res := r.result(statusFailed, r.st.Blocked, time.Now())
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "glpat-abcdefghij1234567890") {
		t.Fatalf("the result object carries the secret:\n%s", b)
	}
	if !strings.Contains(res.CheckTail, redactedMark) {
		t.Fatalf("check_tail must say something was removed: %q", res.CheckTail)
	}
}

// `forge: fields: branch` missing must never answer "there is none": the next
// thing the machine does with that answer is open one.
func TestAMissingBranchPathIsRefusedAndNeverReadAsNone(t *testing.T) {
	_, pc, _, fg := stubWorld(t)
	delete(pc.Forge.Fields.By, "branch")
	_, found, err := fg.Find(context.Background(), pc.Forge.FindMR, "agent/BSK-1", "main")
	if err == nil || found {
		t.Fatalf("found=%v err=%v — answering none here opens a second merge request", found, err)
	}
	if !strings.Contains(err.Error(), "forge: fields: branch") ||
		!strings.Contains(err.Error(), "would open a second one") {
		t.Fatalf("the key and the consequence must both be named:\n%v", err)
	}
}

// And a `fields: list` that is wrong looks exactly like a `fields: branch` that
// is wrong, so the message names both.
func TestAWrongListPathNamesBothKeysItCouldBe(t *testing.T) {
	s, pc, _, fg := stubWorld(t)
	s.mrs = []map[string]any{{"number": 1, "at": "u", "from_ref": "agent/BSK-1"}}
	pc.Forge.Fields.By["list"] = tktWholeReply // the document, not the array inside it
	_, _, err := fg.Find(context.Background(), pc.Forge.FindMR, "agent/BSK-1", "main")
	if err == nil {
		t.Fatal("a document where a merge request was expected must be refused")
	}
	for _, want := range []string{"forge: fields: branch", "forge: fields: list"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in:\n%v", want, err)
		}
	}
}
