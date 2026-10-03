package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `lca ticket` — the state machine, its gates, its resume and the two refusals
// the whole design turns on: never a second ticket, never a second merge
// request.
//
// Every test here drives the machine against a fake world, which is the seam
// stage 2's implementations land on. The fake counts its calls, because the
// interesting property of an idempotent transition is not what it returns — it
// is that it was never made twice.

// ── the fake world ──────────────────────────────────────────────────────────

type fakeWorld struct {
	t *testing.T

	// the tracker
	ticket   TicketBody
	comments []string
	nextKey  string
	readErr  error
	moveErr  error

	// the repo
	heads       map[string]string   // branch → sha
	madeAt      map[string]string   // branch → the commit it was cut at (the authorship ref)
	remotes     map[string]string   // "<remote>/<branch>" → sha
	ancestry    map[string][]string // sha → everything in its history
	worktrees   map[string]string   // branch → where it is checked out
	worktreeErr error

	// the conflict. conflictFiles is what MergeConflicts answers — empty means the
	// branch merges into the target — and the rest is what the one supervised
	// attempt at it does.
	conflictFiles string
	resolveFiles  string // what MergeTargetIn reports; defaults to conflictFiles
	resolveErr    error  // the integrator stage could not finish
	resolveHead   string // the commit it left, "" for "it committed nothing"
	resolveCheck  *TicketCheck
	lost          string // the reviewed paths ResolvedAt says did not survive
	abandonRef    string

	// the forge
	mr    *TicketMR
	mrNum int

	// the models
	commentBody func(tktStageIn) string // what the report stage writes, when a test cares
	commentErr  error                   // the report stage could not be bought
	check       TicketCheck
	verdicts    []string // one per round; the last one repeats
	head        string
	codeErr     error

	calls map[string]int
}

func newFakeWorld(t *testing.T) *fakeWorld {
	zero := 0
	return &fakeWorld{t: t,
		ticket:    TicketBody{Key: "BSK-1", Summary: "the stand drops requests over 8k"},
		heads:     map[string]string{"main": "base000"},
		remotes:   map[string]string{},
		worktrees: map[string]string{},
		ancestry:  map[string][]string{"base000": {}},
		check:     TicketCheck{Cmd: "go test ./...", Exit: &zero, Attempts: 1},
		verdicts:  []string{tktApprove},
		head:      "work111",
		calls:     map[string]int{},
	}
}

func (w *fakeWorld) world() tktWorld {
	return tktWorld{Tracker: w, Repo: w, Forge: fakeForge{w}, Models: w}
}

func (w *fakeWorld) hit(name string) { w.calls[name]++ }

// Every tracker call takes the CONTEXT seriously, because the real ones do: an
// MCP call on a context that is already done comes back "was interrupted", and
// the transition that has to survive exactly that is the report.
func (w *fakeWorld) Read(ctx context.Context, tool, key string) (TicketBody, error) {
	w.hit("read")
	if err := ctx.Err(); err != nil {
		return TicketBody{}, err
	}
	if tool == "" {
		w.t.Fatalf("Read was called with no tool name — a tool name is configuration, never a guess")
	}
	if w.readErr != nil {
		return TicketBody{}, w.readErr
	}
	b := w.ticket
	b.Key = key
	return b, nil
}

func (w *fakeWorld) Create(_ context.Context, tool, project string, body TicketBody) (string, error) {
	w.hit("create")
	if tool == "" || project == "" {
		w.t.Fatalf("Create needs the configured tool and project, got %q / %q", tool, project)
	}
	if w.nextKey == "" {
		return "", fmt.Errorf("the tracker is not answering")
	}
	w.ticket = body
	return w.nextKey, nil
}

func (w *fakeWorld) Comment(ctx context.Context, tool, key, body string) (string, error) {
	w.hit("comment")
	if err := ctx.Err(); err != nil {
		return "", err
	}
	w.comments = append(w.comments, body)
	return fmt.Sprintf("c%d", len(w.comments)), nil
}

func (w *fakeWorld) FindComment(ctx context.Context, tool, key, marker string) (string, bool, error) {
	w.hit("findcomment")
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	for i, c := range w.comments {
		if strings.Contains(c, marker) {
			return fmt.Sprintf("c%d", i+1), true, nil
		}
	}
	return "", false, nil
}

func (w *fakeWorld) Move(ctx context.Context, tool, key, status string) error {
	w.hit("move")
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.moveErr != nil {
		return w.moveErr
	}
	w.comments = append(w.comments, "status="+status)
	return nil
}

// MadeAt mirrors what the real one reads off the authorship ref: the commit the
// branch was cut at, remembered by CutBranch. The fake has to have it, because
// the window it exists for — a crash between creating the ref and recording
// BranchAt — is a state this machine is tested in.
func (w *fakeWorld) MadeAt(branch string) (string, bool, error) {
	w.hit("madeat")
	base, ok := w.madeAt[branch]
	return base, ok, nil
}

func (w *fakeWorld) BranchHead(branch string) (string, bool, error) {
	w.hit("branchhead")
	sha, ok := w.heads[branch]
	return sha, ok, nil
}

func (w *fakeWorld) CutBranch(branch string) (string, string, error) {
	w.hit("cutbranch")
	base := w.heads["main"]
	if w.madeAt == nil {
		w.madeAt = map[string]string{}
	}
	w.madeAt[branch] = base
	w.heads[branch] = base
	w.worktrees[branch] = "/tmp/wt/" + branch
	return base, w.worktrees[branch], nil
}

func (w *fakeWorld) Worktree(branch string) (string, error) {
	w.hit("worktree")
	if w.worktreeErr != nil {
		return "", w.worktreeErr
	}
	if w.worktrees[branch] == "" {
		w.worktrees[branch] = "/tmp/wt/" + branch + "-adopted"
	}
	return w.worktrees[branch], nil
}

func (w *fakeWorld) Contains(ancestor, of string) (bool, error) {
	w.hit("contains")
	if ancestor == of {
		return true, nil
	}
	return contains(w.ancestry[of], ancestor), nil
}

// MergeIn takes the SHA the gate approved, and refuses when the branch has moved
// off it — the real one's whole point, mirrored here because the tests that
// matter most are the ones about work nobody reviewed.
func (w *fakeWorld) MergeIn(branch, target, sha string) (string, error) {
	w.hit("mergein")
	if sha == "" {
		return "", fmt.Errorf("merging %s into %s without a reviewed commit", branch, target)
	}
	if head := w.heads[branch]; head != sha {
		return "", fmt.Errorf("%s is at %s and the approved commit is %s", branch, head, sha)
	}
	out := "merged" + target
	w.ancestry[out] = append(append([]string{}, w.ancestry[sha]...), sha, w.heads[target])
	w.heads[target] = out
	return out, nil
}

// The conflict side of the repository. The fake keeps it as one string because
// the machine only ever asks two things of it — does it collide, and which paths
// — and the git that answers those for real has tests of its own against a real
// repository (ticketgit_test.go).
func (w *fakeWorld) MergeConflicts(branch, target string) (string, bool, error) {
	w.hit("mergeconflicts")
	return w.conflictFiles, w.conflictFiles != "", nil
}

func (w *fakeWorld) MergeTargetIn(branch, target, dir string) (string, string, error) {
	w.hit("mergetargetin")
	if dir == "" {
		w.t.Fatal("MergeTargetIn was called with no worktree, so the merge would have happened in the operator's own tree")
	}
	return w.heads[target], firstNonEmpty(w.resolveFiles, w.conflictFiles), nil
}

func (w *fakeWorld) ResolvedAt(branch, dir, approved, target string) (string, string, error) {
	w.hit("resolvedat")
	head := w.heads[branch]
	if head == "" || head == approved {
		return "", "", fmt.Errorf("nothing was committed on %s", branch)
	}
	return head, w.lost, nil
}

func (w *fakeWorld) AbandonResolution(branch, dir, approved string) (string, error) {
	w.hit("abandon")
	w.heads[branch] = approved
	return w.abandonRef, nil
}

// Resolve is the integrator's one attempt, as the machine sees it: a commit on
// the branch and a check verdict. The default is a resolution that worked, so a
// test that cares about a failure says which failure it is.
func (w *fakeWorld) Resolve(_ context.Context, in tktStageIn) (tktCodeOut, error) {
	w.hit("stage:" + in.Stage)
	w.hit("instr:" + in.Stage + ":" + in.Instructions)
	if in.Conflict.Files == "" {
		w.t.Fatal("the resolve stage must be told which files collided")
	}
	if w.resolveErr != nil {
		return tktCodeOut{Session: "integrator-sess", Head: w.resolveHead}, w.resolveErr
	}
	head := firstNonEmpty(w.resolveHead, "resolved999")
	w.heads[in.Branch] = head
	w.ancestry[head] = append([]string{in.Conflict.Base, w.heads[in.Target]}, w.ancestry[in.Conflict.Base]...)
	// The branch now carries the target, which is what makes the attempt
	// re-enterable: the next pass through the probe finds no conflict left.
	w.conflictFiles = ""
	check := w.check
	if w.resolveCheck != nil {
		check = *w.resolveCheck
	}
	return tktCodeOut{Session: "integrator-sess", Head: head, Check: check}, nil
}

func (w *fakeWorld) RemoteHead(_ context.Context, remote, branch string) (string, bool, error) {
	w.hit("remotehead")
	sha, ok := w.remotes[remote+"/"+branch]
	return sha, ok, nil
}

func (w *fakeWorld) PushBranch(_ context.Context, remote, branch, sha string) error {
	w.hit("pushbranch")
	w.remotes[remote+"/"+branch] = sha
	return nil
}

type fakeForge struct{ w *fakeWorld }

func (f fakeForge) Find(_ context.Context, tool, branch, target string) (TicketMR, bool, error) {
	f.w.hit("findmr")
	if tool == "" {
		f.w.t.Fatalf("Find was called with no tool name")
	}
	if f.w.mr == nil {
		return TicketMR{}, false, nil
	}
	return *f.w.mr, true, nil
}

func (f fakeForge) Create(_ context.Context, tool string, mr TicketMR) (TicketMR, error) {
	f.w.hit("createmr")
	f.w.mrNum++
	made := mr
	made.ID = fmt.Sprintf("%d", f.w.mrNum)
	made.URL = "https://forge/mr/" + made.ID
	f.w.mr = &made
	return made, nil
}

func (w *fakeWorld) WriteTicket(_ context.Context, in tktStageIn, task string) (TicketBody, error) {
	w.hit("stage:" + in.Stage)
	w.hit("instr:" + in.Stage + ":" + in.Instructions)
	return TicketBody{Summary: task, Body: "written by a model"}, nil
}

func (w *fakeWorld) Implement(_ context.Context, in tktStageIn) (tktCodeOut, error) {
	w.hit("stage:" + in.Stage)
	w.hit("instr:" + in.Stage + ":" + in.Instructions)
	if in.Round > 0 && in.Session == "" {
		w.t.Fatalf("round %d must continue the coder's own session, and none was passed", in.Round)
	}
	if w.codeErr != nil {
		return tktCodeOut{}, w.codeErr
	}
	head := fmt.Sprintf("%s-r%d", w.head, in.Round)
	w.heads[in.Branch] = head
	w.ancestry[head] = []string{w.heads["main"], "base000"}
	return tktCodeOut{Session: "coder-sess", Head: head, Check: w.check}, nil
}

func (w *fakeWorld) Review(_ context.Context, in tktStageIn) (*reviewReport, error) {
	w.hit("stage:" + in.Stage)
	w.hit("instr:" + in.Stage + ":" + in.Instructions)
	v := w.verdicts[min(in.Round, len(w.verdicts)-1)]
	if v == "unreadable" {
		return nil, nil
	}
	return &reviewReport{Verdict: v, Comments: []reviewComment{}, Summary: "round " + fmt.Sprint(in.Round)}, nil
}

func (w *fakeWorld) WriteMR(_ context.Context, in tktStageIn) (TicketMR, error) {
	w.hit("stage:" + in.Stage)
	w.hit("instr:" + in.Stage + ":" + in.Instructions)
	return TicketMR{Title: in.Ticket.Key + " " + in.Ticket.Summary}, nil
}

func (w *fakeWorld) WriteComment(ctx context.Context, in tktStageIn) (string, error) {
	w.hit("stage:" + in.Stage)
	w.hit("instr:" + in.Stage + ":" + in.Instructions)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if w.commentErr != nil {
		return "", w.commentErr
	}
	if w.commentBody != nil {
		return w.commentBody(in), nil
	}
	return "lca worked " + in.Ticket.Key, nil
}

// ── fixtures ────────────────────────────────────────────────────────────────

// fullPipeline is a complete pipeline: block, written as a team would write it.
// Every value is a fact lca cannot know, which is why every one of them is here.
const fullPipeline = `
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
    read: jira__issue_get
    create: jira__issue_create
    comment: jira__issue_comment
    transition: jira__issue_transition
    status_done: In Review
    status_blocked: Needs human
    args:
      read: {issueIdOrKey: "${key}"}
      create: {project: "${project}", summary: "${summary}", description: "${body}", issuetype: Task}
      comment: {issueIdOrKey: "${key}", body: "${text}"}
      transition: {issueIdOrKey: "${key}", transition: "${status}"}
    fields:
      key: key
      summary: fields.summary
      body: fields.description
      comment_id: id
  forge:
    create_merge_request: forge__create_mr
    find_merge_request: forge__list_mrs
    args:
      create_merge_request: {source_branch: "${branch}", target_branch: "${target}", title: "${title}", description: "${body}"}
      find_merge_request: {source_branch: "${branch}", state: opened}
    fields:
      list: .
      branch: source_branch
      target: target_branch
      id: iid
      url: web_url
      title: title
`

func pipeFrom(t *testing.T, yaml string) *PipelineConfig {
	t.Helper()
	pc, err := parsePipeline(nil, parseYAMLish(yaml), "roles.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pc == nil {
		t.Fatal("no pipeline: block parsed out of that file")
	}
	return pc
}

// newRun wires a run against the fake world, with no orchestrator and no
// gateway: the machine, its gates and its state file are the whole subject.
func newRun(t *testing.T, w *fakeWorld, pc *PipelineConfig, st *TicketState) *tktRun {
	t.Helper()
	cfg := Config{Dir: t.TempDir()}
	cfg.StateDir = cfg.Dir
	dir := ticketDir(cfg, st.Ticket)
	if st.Ticket == "" {
		dir = filepath.Join(ticketsDir(cfg), "new-pending")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st.Version, st.Rounds = tktStateVersion, pc.pipelineRounds()
	if st.Target == "" {
		st.Target, st.Remote = pc.Target, pc.Remote
	}
	if st.Branch == "" && st.Ticket != "" {
		st.Branch = tktBranchFor(pc, st.Ticket)
	}
	if st.Started == "" {
		st.Started = nowTS()
	}
	return &tktRun{cfg: cfg, pc: pc, w: w.world(), st: st,
		dir: dir, ctx: context.Background(), spine: tktSpine(), runID: "test-run"}
}

func fresh(t *testing.T, w *fakeWorld, pc *PipelineConfig) *tktRun {
	return newRun(t, w, pc, &TicketState{Ticket: "BSK-1", State: tktStart})
}

// states lists the states the journal says were reached, in order.
func states(st *TicketState) []string {
	var out []string
	for _, s := range st.Journal {
		if s.To != "" && (s.Status == tktOK || s.Status == tktAlready) {
			out = append(out, s.To)
		}
	}
	return out
}

func stepsNamed(st *TicketState, name string) []TicketStep {
	var out []TicketStep
	for _, s := range st.Journal {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// ── every state is reachable, recorded and resumable ────────────────────────

func TestTheWholeArcRunsEndToEndAndRecordsEveryState(t *testing.T) {
	w := newFakeWorld(t)
	r := fresh(t, w, pipeFrom(t, fullPipeline))

	status, reason := r.execute()
	if status != statusPassed {
		t.Fatalf("status %q (%s)\njournal: %+v", status, reason, r.st.Journal)
	}
	want := []string{tktOpened, tktBranched, tktImplemented, tktReviewed, tktMerged, tktPushed, tktProposed, tktReported}
	if got := strings.Join(states(r.st), " "); got != strings.Join(want, " ") {
		t.Fatalf("the arc must record every state it reached\n got: %s\nwant: %s", got, strings.Join(want, " "))
	}
	if r.st.MergeRequest == nil || r.st.MergeRequest.URL == "" {
		t.Fatalf("a proposed ticket carries the forge's own merge request: %+v", r.st.MergeRequest)
	}
	if r.st.CommentID == "" || len(w.comments) == 0 {
		t.Fatal("a finished run tells the ticket what happened")
	}
	if !strings.Contains(w.comments[0], r.st.Marker) {
		t.Fatalf("the comment must carry the marker a re-run finds it by: %q", w.comments[0])
	}
	if w.calls["move"] != 1 {
		t.Fatalf("status_done is configured, so the ticket moves once: %d", w.calls["move"])
	}
	if r.result(status, reason, time.Now()).exitCode() != exitOK {
		t.Fatal("passed is exit 0")
	}
	// Every state is on disk, not just in memory: the next run reads this file.
	if _, err := loadTicketState(r.dir); err != nil {
		t.Fatalf("the state file must be readable after the run: %v", err)
	}
}

func TestResumeEntersAtEveryStateAndRedoesNothingBeforeIt(t *testing.T) {
	// One case per state: the run is handed a state file standing there, and the
	// transitions BEFORE it must not be taken again.
	cases := []struct {
		state   string
		seed    func(*fakeWorld, *TicketState)
		notHit  []string
		wantEnd string
	}{
		{state: tktOpened, notHit: []string{"create"}, wantEnd: tktReported,
			seed: func(w *fakeWorld, st *TicketState) { st.Summary = "already read" }},
		{state: tktBranched, notHit: []string{"cutbranch"}, wantEnd: tktReported,
			seed: func(w *fakeWorld, st *TicketState) {
				st.Summary, st.BranchAt = "x", "base000"
				w.heads[st.Branch] = "base000"
			}},
		{state: tktImplemented, notHit: []string{"stage:coder"}, wantEnd: tktReported,
			seed: func(w *fakeWorld, st *TicketState) {
				zero := 0
				st.Summary, st.BranchAt, st.Head = "x", "base000", "work111-r0"
				st.Check = TicketCheck{Cmd: "go test", Exit: &zero, Attempts: 1}
				w.heads[st.Branch] = "work111-r0"
				w.ancestry["work111-r0"] = []string{"base000"}
			}},
		{state: tktReviewed, notHit: []string{"stage:coder", "stage:reviewer"}, wantEnd: tktReported,
			seed: func(w *fakeWorld, st *TicketState) {
				zero := 0
				st.Summary, st.BranchAt, st.Head = "x", "base000", "work111-r0"
				st.Check = TicketCheck{Cmd: "go test", Exit: &zero, Attempts: 1}
				st.Verdict = tktApprove
				st.Review = &reviewReport{Verdict: tktApprove, Comments: []reviewComment{}}
				w.heads[st.Branch] = "work111-r0"
				w.ancestry["work111-r0"] = []string{"base000"}
			}},
		{state: tktMerged, notHit: []string{"mergein", "stage:coder"}, wantEnd: tktReported,
			seed: func(w *fakeWorld, st *TicketState) {
				zero := 0
				st.Summary, st.BranchAt, st.Head = "x", "base000", "work111-r0"
				st.Check = TicketCheck{Cmd: "go test", Exit: &zero, Attempts: 1}
				st.Verdict = tktApprove
				st.MergedAt = "mergedmain"
				w.heads[st.Branch] = "work111-r0"
				w.heads["main"] = "mergedmain"
				w.ancestry["mergedmain"] = []string{"work111-r0", "base000"}
				w.ancestry["work111-r0"] = []string{"base000"}
			}},
		{state: tktPushed, notHit: []string{"pushbranch", "mergein"}, wantEnd: tktReported,
			seed: func(w *fakeWorld, st *TicketState) {
				zero := 0
				st.Summary, st.BranchAt, st.Head = "x", "base000", "work111-r0"
				st.Check = TicketCheck{Cmd: "go test", Exit: &zero, Attempts: 1}
				st.Verdict, st.PushedSha = tktApprove, "work111-r0"
				w.heads[st.Branch] = "work111-r0"
				w.heads["main"] = "mergedmain"
				w.remotes["origin/agent/BSK-1"] = "work111-r0"
				w.ancestry["work111-r0"] = []string{"base000"}
				w.ancestry["mergedmain"] = []string{"work111-r0", "base000"}
			}},
		{state: tktProposed, notHit: []string{"createmr", "pushbranch"}, wantEnd: tktReported,
			seed: func(w *fakeWorld, st *TicketState) {
				zero := 0
				st.Summary, st.BranchAt, st.Head = "x", "base000", "work111-r0"
				st.Check = TicketCheck{Cmd: "go test", Exit: &zero, Attempts: 1}
				st.Verdict, st.PushedSha = tktApprove, "work111-r0"
				st.MergeRequest = &TicketMR{ID: "7", URL: "https://forge/mr/7"}
				w.mr = st.MergeRequest
				w.heads[st.Branch] = "work111-r0"
				w.heads["main"] = "mergedmain"
				w.remotes["origin/agent/BSK-1"] = "work111-r0"
				w.ancestry["work111-r0"] = []string{"base000"}
				w.ancestry["mergedmain"] = []string{"work111-r0", "base000"}
			}},
	}
	for _, c := range cases {
		t.Run(c.state, func(t *testing.T) {
			w := newFakeWorld(t)
			st := &TicketState{Ticket: "BSK-1", State: c.state, Branch: "agent/BSK-1"}
			c.seed(w, st)
			r := newRun(t, w, pipeFrom(t, fullPipeline), st)
			status, reason := r.execute()
			if status != statusPassed {
				t.Fatalf("a resume from %s must finish: %s (%s)\n%+v", c.state, status, reason, r.st.Journal)
			}
			if r.st.State != c.wantEnd {
				t.Fatalf("a resume from %s ended at %s, want %s", c.state, r.st.State, c.wantEnd)
			}
			for _, k := range c.notHit {
				if w.calls[k] != 0 {
					t.Fatalf("resuming from %s re-did %s (%d times) — a transition already done must be proved, not repeated",
						c.state, k, w.calls[k])
				}
			}
		})
	}
}

// ── the gates ───────────────────────────────────────────────────────────────

func TestARedCheckEndsTheRunAtImplementedAndTellsTheTicket(t *testing.T) {
	w := newFakeWorld(t)
	two := 2
	w.check = TicketCheck{Cmd: "go test ./...", Exit: &two, Attempts: 3, Tail: "FAIL\tlca"}
	r := fresh(t, w, pipeFrom(t, fullPipeline))

	status, reason := r.execute()
	if status != statusFailed {
		t.Fatalf("a red check is failed, got %s", status)
	}
	if statusExitCode(status) != exitFailed {
		t.Fatalf("failed is exit 1, got %d", statusExitCode(status))
	}
	if r.st.BlockedAt != tktImplemented {
		t.Fatalf("the run ends AT the state it reached: %q", r.st.BlockedAt)
	}
	if !strings.Contains(reason, "exit 2") || !strings.Contains(reason, "3 attempts") {
		t.Fatalf("the blocked sentence names the check, its exit and its attempts: %q", reason)
	}
	for _, forbidden := range []string{"mergein", "pushbranch", "createmr", "stage:reviewer"} {
		if w.calls[forbidden] != 0 {
			t.Fatalf("nothing past a red check may happen, and %s did", forbidden)
		}
	}
	if len(w.comments) == 0 {
		t.Fatal("a blocked run comments on the ticket: an unattended pipeline that goes quiet is worse than one that fails loudly")
	}
	if w.calls["move"] != 1 || !contains(w.comments, "status=Needs human") {
		t.Fatalf("a blocked ticket moves to status_blocked: %v", w.comments)
	}
}

func TestRequestChangesAfterTheLastRoundEndsTheRunAtReviewed(t *testing.T) {
	w := newFakeWorld(t)
	w.verdicts = []string{tktChanges}
	pc := pipeFrom(t, strings.Replace(fullPipeline, "rework_rounds: 1", "rework_rounds: 0", 1))
	r := fresh(t, w, pc)

	status, reason := r.execute()
	if status != statusFailed || r.st.BlockedAt != tktReviewed {
		t.Fatalf("status %s at %s (%s)", status, r.st.BlockedAt, reason)
	}
	if w.calls["mergein"] != 0 || w.calls["createmr"] != 0 {
		t.Fatal("nothing merges or proposes without an approve")
	}
	if w.calls["stage:coder"] != 1 {
		t.Fatalf("with no rounds the coder runs once: %d", w.calls["stage:coder"])
	}
	if !strings.Contains(reason, "asked for changes") {
		t.Fatalf("the reason says what the reviewer did: %q", reason)
	}
}

func TestRequestChangesWithARoundLeftGoesBackToTheSameSession(t *testing.T) {
	w := newFakeWorld(t)
	w.verdicts = []string{tktChanges, tktApprove}
	r := fresh(t, w, pipeFrom(t, fullPipeline))

	status, reason := r.execute()
	if status != statusPassed {
		t.Fatalf("an approved second round finishes: %s (%s)", status, reason)
	}
	if r.st.Round != 1 {
		t.Fatalf("one rework round was spent, round is %d", r.st.Round)
	}
	if w.calls["stage:coder"] != 2 || w.calls["stage:reviewer"] != 2 {
		t.Fatalf("a rework round runs the coder and the reviewer again: coder %d reviewer %d",
			w.calls["stage:coder"], w.calls["stage:reviewer"])
	}
	if len(stepsNamed(r.st, tktRework)) != 2 {
		// One `ok` in round 0, then one `skipped` in round 1 once the verdict is
		// approve: both are recorded, because a skip is not a silence.
		t.Fatalf("the journal records the rework and the skip: %+v", stepsNamed(r.st, tktRework))
	}
	if r.st.Coder == "" {
		t.Fatal("the coder's session is recorded, because the next round continues it")
	}
}

func TestAnUnreadableVerdictIsNeverAnApprove(t *testing.T) {
	w := newFakeWorld(t)
	w.verdicts = []string{"unreadable"}
	pc := pipeFrom(t, strings.Replace(fullPipeline, "rework_rounds: 1", "rework_rounds: 0", 1))
	r := fresh(t, w, pc)

	status, reason := r.execute()
	if status != statusFailed {
		t.Fatalf("unreadable is failed, never a silent approve: %s", status)
	}
	if w.calls["mergein"] != 0 {
		t.Fatal("a verdict nobody could read must not merge anything")
	}
	if !strings.Contains(reason, "could not be read") {
		t.Fatalf("the reason says the verdict could not be read: %q", reason)
	}
}

func TestPushFalseFinishesAtMergedAndIsASuccess(t *testing.T) {
	w := newFakeWorld(t)
	pc := pipeFrom(t, strings.Replace(fullPipeline, "push: true", "push: false", 1))
	r := fresh(t, w, pc)

	status, reason := r.execute()
	if status != statusPassed {
		t.Fatalf("a configured no-push run is a success, not a block: %s (%s)", status, reason)
	}
	if w.calls["pushbranch"] != 0 || w.calls["createmr"] != 0 || w.calls["findmr"] != 0 {
		t.Fatal("push: false reaches neither the remote nor the forge")
	}
	if r.st.Blocked != "" {
		t.Fatalf("a skip is not a block: %q", r.st.Blocked)
	}
	for _, name := range []string{tktPush, tktPropose} {
		ss := stepsNamed(r.st, name)
		if len(ss) != 1 || ss[0].Status != tktSkipped {
			t.Fatalf("%s must be recorded as skipped, got %+v", name, ss)
		}
	}
	if !contains(states(r.st), tktMerged) {
		t.Fatalf("the work still merged: %v", states(r.st))
	}
}

// ── idempotency: never a second anything ────────────────────────────────────

func TestARerunNeverOpensASecondMergeRequest(t *testing.T) {
	w := newFakeWorld(t)
	r := fresh(t, w, pipeFrom(t, fullPipeline))
	if status, reason := r.execute(); status != statusPassed {
		t.Fatalf("first run: %s (%s)", status, reason)
	}
	if w.calls["createmr"] != 1 {
		t.Fatalf("one merge request on the first run: %d", w.calls["createmr"])
	}
	before := len(w.comments)

	// The same state file, the same command, again — which is what a cron line
	// does after a crash, because there is no -resume to forget.
	again := newRun(t, w, pipeFrom(t, fullPipeline), r.st)
	if status, reason := again.execute(); status != statusPassed {
		t.Fatalf("second run: %s (%s)", status, reason)
	}
	if w.calls["createmr"] != 1 {
		t.Fatalf("a re-run must not open a second merge request: %d", w.calls["createmr"])
	}
	if w.calls["findmr"] < 2 {
		t.Fatal("the forge is asked every time: the state file is not the truth about a server")
	}
	if len(w.comments) != before {
		t.Fatalf("a re-run that reached the same state must not post a second identical comment: %v", w.comments[before:])
	}
}

func TestTheStateFileSaysPushedAndTheRemoteSaysOtherwise(t *testing.T) {
	zero := 0
	seed := func(w *fakeWorld) *TicketState {
		w.heads["agent/BSK-1"] = "work111-r0"
		w.heads["main"] = "mergedmain"
		w.ancestry["work111-r0"] = []string{"base000"}
		w.ancestry["mergedmain"] = []string{"work111-r0", "base000"}
		return &TicketState{Ticket: "BSK-1", State: tktPushed, Branch: "agent/BSK-1",
			Summary: "x", BranchAt: "base000", Head: "work111-r0", PushedSha: "work111-r0",
			Verdict: tktApprove, Check: TicketCheck{Cmd: "go test", Exit: &zero, Attempts: 1}}
	}

	t.Run("the remote has nothing, so the push is made again", func(t *testing.T) {
		w := newFakeWorld(t)
		r := newRun(t, w, pipeFrom(t, fullPipeline), seed(w))
		if status, reason := r.execute(); status != statusPassed {
			t.Fatalf("%s (%s)", status, reason)
		}
		if w.calls["pushbranch"] != 1 {
			t.Fatalf("an absent remote branch is repaired by pushing again, not refused: %d", w.calls["pushbranch"])
		}
	})

	t.Run("the remote has our commit and more on top", func(t *testing.T) {
		w := newFakeWorld(t)
		st := seed(w)
		w.remotes["origin/agent/BSK-1"] = "someone-else"
		w.ancestry["someone-else"] = []string{"work111-r0", "base000"}
		r := newRun(t, w, pipeFrom(t, fullPipeline), st)
		if status, reason := r.execute(); status != statusPassed {
			t.Fatalf("a remote that already contains our work is done, not broken: %s (%s)", status, reason)
		}
		if w.calls["pushbranch"] != 0 {
			t.Fatal("there is nothing to push when the remote already contains our commit")
		}
	})

	t.Run("the remote does not contain our commit", func(t *testing.T) {
		w := newFakeWorld(t)
		st := seed(w)
		w.remotes["origin/agent/BSK-1"] = "unrelated"
		w.ancestry["unrelated"] = []string{"base000"}
		r := newRun(t, w, pipeFrom(t, fullPipeline), st)
		status, reason := r.execute()
		if status != statusConfig {
			t.Fatalf("somebody else's branch is row 2 of the table — alert a person, touch nothing: %s", status)
		}
		if statusExitCode(status) != exitUsage {
			t.Fatalf("want exit 2, got %d", statusExitCode(status))
		}
		if !strings.Contains(reason, "refusing to force") {
			t.Fatalf("the refusal says what it will not do: %q", reason)
		}
		if w.calls["pushbranch"] != 0 || w.calls["createmr"] != 0 {
			t.Fatal("a foreign remote branch stops the run where it stands")
		}
	})
}

func TestABranchThisRunDidNotCutIsRefused(t *testing.T) {
	w := newFakeWorld(t)
	w.heads["agent/BSK-1"] = "somebodys-work" // there before we got here
	r := fresh(t, w, pipeFrom(t, fullPipeline))

	status, reason := r.execute()
	if status != statusConfig {
		t.Fatalf("a branch nobody here cut is row 2: %s (%s)", status, reason)
	}
	// The wording now also says the run has no base recorded for the branch — the
	// other half of the same question, which MadeAt answers off the authorship ref.
	if !strings.Contains(reason, "no record of lca having cut it") {
		t.Fatalf("the refusal says why: %q", reason)
	}
	if w.calls["cutbranch"] != 0 || w.calls["stage:coder"] != 0 {
		t.Fatal("nothing is built on a branch this run cannot account for")
	}
}

func TestABranchThatNoLongerHoldsOurCommitIsRefused(t *testing.T) {
	zero := 0
	w := newFakeWorld(t)
	w.heads["agent/BSK-1"] = "rewritten"
	w.ancestry["rewritten"] = []string{} // our cut point is gone
	st := &TicketState{Ticket: "BSK-1", State: tktBranched, Branch: "agent/BSK-1",
		Summary: "x", BranchAt: "base000", Check: TicketCheck{Exit: &zero}}
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)

	status, reason := r.execute()
	if status != statusConfig || !strings.Contains(reason, "no longer contains") {
		t.Fatalf("a rewritten branch is refused by name: %s (%s)", status, reason)
	}
}

func TestAKilledTicketCreateRefusesToOpenASecond(t *testing.T) {
	w := newFakeWorld(t)
	w.nextKey = "BSK-9"
	st := &TicketState{Ticket: "", State: tktStart, Pending: tktOpen}
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)
	r.newTask = "make the stand stop dropping big requests"

	status, reason := r.execute()
	if status != statusConfig {
		t.Fatalf("the one transition that cannot be re-entered blind refuses: %s (%s)", status, reason)
	}
	if w.calls["create"] != 0 {
		t.Fatal("a run killed inside the create must never create a second ticket")
	}
	if !strings.Contains(reason, "lca ticket <KEY>") {
		t.Fatalf("the refusal names the way out: %q", reason)
	}
}

func TestANewTicketIsOpenedOnceAndThenWorked(t *testing.T) {
	w := newFakeWorld(t)
	w.nextKey = "BSK-42"
	st := &TicketState{Ticket: "", State: tktStart}
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)
	r.newTask = "make the stand stop dropping big requests"

	status, reason := r.execute()
	if status != statusPassed {
		t.Fatalf("%s (%s)\n%+v", status, reason, r.st.Journal)
	}
	if w.calls["create"] != 1 {
		t.Fatalf("one ticket: %d", w.calls["create"])
	}
	if r.st.Ticket != "BSK-42" || r.st.Branch != "agent/BSK-42" {
		t.Fatalf("the key the tracker gave decides the branch: %q / %q", r.st.Ticket, r.st.Branch)
	}
	if w.calls["stage:ticket"] != 1 {
		t.Fatal("a model writes the body, and lca does the opening")
	}
}

func TestAStateFileFromAnotherTreeIsRefused(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, StateDir: dir, Root: "/projects/b"}
	st := &TicketState{Version: tktStateVersion, Ticket: "BSK-1", Root: "/projects/a", State: tktBranched}
	td := ticketDir(cfg, "BSK-1")
	if err := saveTicketState(td, st); err != nil {
		t.Fatal(err)
	}
	r := &tktRun{cfg: cfg, pc: pipeFrom(t, fullPipeline), spine: tktSpine(), runID: "x",
		orch: nil}
	err := r.openState("BSK-1", "/projects/b")
	if err == nil || !strings.Contains(err.Error(), "/projects/a") {
		t.Fatalf("a ticket worked in another tree is another project's work: %v", err)
	}
}

func TestAStateFileFromAnotherBuildIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"ticket":"BSK-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadTicketState(dir)
	if err == nil || !strings.Contains(err.Error(), "state version 99") {
		t.Fatalf("a document this build may not understand decides whether a merge request exists: %v", err)
	}
}

func TestAMergedStateWithNoApproveCannotBeResumed(t *testing.T) {
	zero := 0
	w := newFakeWorld(t)
	st := &TicketState{Ticket: "BSK-1", State: tktMerged, Branch: "agent/BSK-1", Summary: "x",
		BranchAt: "base000", Head: "work111-r0", Verdict: tktChanges,
		Check: TicketCheck{Cmd: "go test", Exit: &zero, Attempts: 1}}
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)

	status, reason := r.execute()
	if status != statusConfig || !strings.Contains(reason, "without an approve") {
		t.Fatalf("a journal whose evidence contradicts its state cannot be continued: %s (%s)", status, reason)
	}
	if w.calls["pushbranch"] != 0 {
		t.Fatal("a resume re-derives its gates from the evidence, it does not trust the state word")
	}
}

func TestTwoProcessesCannotWorkOneTicket(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockTicket(dir, "BSK-1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := lockTicket(dir, "BSK-1"); err == nil || !strings.Contains(err.Error(), "two merge requests") {
		t.Fatalf("two processes on one ticket produce two merge requests: %v", err)
	}
}

// ── configuration: a missing key names the key ──────────────────────────────

func TestEveryRequiredKeyIsNamedWhenItIsMissing(t *testing.T) {
	// One case per key: remove it from the complete block and the error must name
	// it. "Never a guess, never a default that silently does the wrong thing."
	cases := []struct {
		drop string
		name string
		need tktNeed
	}{
		{drop: "  branch_prefix: agent/\n", name: "branch_prefix"},
		{drop: "  target_branch: main\n", name: "target_branch"},
		{drop: "  push: true\n", name: "push"},
		{drop: "  rework_rounds: 1\n", name: "rework_rounds"},
		{drop: "    coder: coder\n", name: "roles: coder"},
		{drop: "    reviewer: reviewer\n", name: "roles: reviewer"},
		{drop: "    integrator: integrator\n", name: "roles: integrator"},
		{drop: "    read: jira__issue_get\n", name: "tracker: read"},
		{drop: "    comment: jira__issue_comment\n", name: "tracker: comment"},
		{drop: "    create: jira__issue_create\n", name: "tracker: create", need: tktNeed{creating: true}},
		{drop: "  project: BSK\n", name: "project", need: tktNeed{creating: true}},
		{drop: "  remote: origin\n", name: "remote"},
		{drop: "    create_merge_request: forge__create_mr\n", name: "forge: create_merge_request"},
		{drop: "    find_merge_request: forge__list_mrs\n", name: "forge: find_merge_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			yaml := strings.Replace(fullPipeline, c.drop, "", 1)
			if yaml == fullPipeline {
				t.Fatalf("the fixture no longer contains %q, so this case tests nothing", c.drop)
			}
			pc := pipeFrom(t, yaml)
			err := pc.validate(nil, c.need)
			if err == nil {
				t.Fatalf("a missing %s must be an error, not a default", c.name)
			}
			if !strings.Contains(err.Error(), "pipeline: "+c.name) {
				t.Fatalf("the error must name the key %q:\n%v", c.name, err)
			}
			var ue *usageErr
			if !asUsageErr(err, &ue) {
				t.Fatalf("a configuration mistake is row 2 of the table, not a failed task: %T", err)
			}
		})
	}
}

// argText and fieldText render a call's configuration as one comparable string,
// in the order it was written: the order is what reaches the model and the
// server, so a round trip that reordered it would be a round trip that changed
// what lca sends.
func argText(pa PipelineArgs, verb string) string {
	var parts []string
	for _, name := range pa.Order[verb] {
		parts = append(parts, name+"="+pa.By[verb][name])
	}
	if len(parts) != len(pa.By[verb]) {
		return "ORDER LOST: " + fmt.Sprint(pa.By[verb])
	}
	return strings.Join(parts, " ")
}

func fieldText(pf PipelineFields) string {
	var parts []string
	for _, k := range pf.Order {
		parts = append(parts, k+"="+pf.By[k])
	}
	if len(parts) != len(pf.By) {
		return "ORDER LOST: " + fmt.Sprint(pf.By)
	}
	return strings.Join(parts, " ")
}

func asUsageErr(err error, out **usageErr) bool {
	u, ok := err.(*usageErr)
	if ok {
		*out = u
	}
	return ok
}

func TestTheCompleteBlockValidates(t *testing.T) {
	if err := pipeFrom(t, fullPipeline).validate(nil, tktNeed{creating: true}); err != nil {
		t.Fatalf("the documented block must validate: %v", err)
	}
}

func TestOneMissingKeyPerLineAndAllOfThemAtOnce(t *testing.T) {
	pc := pipeFrom(t, "pipeline:\n  project: BSK\n")
	err := pc.validate(nil, tktNeed{})
	if err == nil {
		t.Fatal("an almost empty block is an error")
	}
	for _, key := range []string{"branch_prefix", "target_branch", "push", "rework_rounds",
		"roles: coder", "roles: reviewer", "roles: integrator", "tracker: read", "tracker: comment"} {
		if !strings.Contains(err.Error(), "pipeline: "+key) {
			t.Fatalf("one alert names every missing key, and %s is not in it:\n%v", key, err)
		}
	}
}

func TestNoPipelineBlockAtAllExplainsWhatItIsFor(t *testing.T) {
	var pc *PipelineConfig
	err := pc.validate(nil, tktNeed{})
	if err == nil || !strings.Contains(err.Error(), "pipeline: block") {
		t.Fatalf("a project with no block is told what the block is: %v", err)
	}
}

func TestATransitionToolWithoutAStatusIsHalfConfigured(t *testing.T) {
	// A team that names the tool and no status believes lca is moving their
	// ticket, and nothing ever would.
	yaml := strings.Replace(fullPipeline, "    status_done: In Review\n", "", 1)
	yaml = strings.Replace(yaml, "    status_blocked: Needs human\n", "", 1)
	err := pipeFrom(t, yaml).validate(nil, tktNeed{})
	if err == nil || !strings.Contains(err.Error(), "status_done or status_blocked") {
		t.Fatalf("a transition tool with no status could never be called: %v", err)
	}

	// And the other way round: a status with nothing to apply it.
	yaml = strings.Replace(fullPipeline, "    transition: jira__issue_transition\n", "", 1)
	err = pipeFrom(t, yaml).validate(nil, tktNeed{})
	if err == nil || !strings.Contains(err.Error(), "tracker: transition") {
		t.Fatalf("a status with no tool to apply it: %v", err)
	}
}

func TestAStatusMoveIsOptionalAsAWhole(t *testing.T) {
	yaml := fullPipeline
	for _, drop := range []string{"    transition: jira__issue_transition\n",
		"    status_done: In Review\n", "    status_blocked: Needs human\n"} {
		yaml = strings.Replace(yaml, drop, "", 1)
	}
	pc := pipeFrom(t, yaml)
	if err := pc.validate(nil, tktNeed{}); err != nil {
		t.Fatalf("a team that does not want lca touching workflow states writes none of the three: %v", err)
	}
	w := newFakeWorld(t)
	r := fresh(t, w, pc)
	if status, _ := r.execute(); status != statusPassed {
		t.Fatalf("and the run still works: %s", status)
	}
	if w.calls["move"] != 0 {
		t.Fatal("no transition tool, no status move")
	}
	if len(w.comments) == 0 {
		t.Fatal("the comment is not optional, though")
	}
}

func TestPushFalseNeedsNoForgeAndNoRemote(t *testing.T) {
	yaml := strings.Replace(fullPipeline, "push: true", "push: false", 1)
	for _, drop := range []string{"  remote: origin\n",
		"    create_merge_request: forge__create_mr\n", "    find_merge_request: forge__list_mrs\n"} {
		yaml = strings.Replace(yaml, drop, "", 1)
	}
	if err := pipeFrom(t, yaml).validate(nil, tktNeed{}); err != nil {
		t.Fatalf("a no-push pipeline has no remote branch and no merge request to configure: %v", err)
	}
}

func TestAValueThatCannotMeanAnythingIsRefusedAtLoad(t *testing.T) {
	for _, bad := range []struct{ yaml, want string }{
		{"pipeline:\n  push: maybe\n", "not true or false"},
		{"pipeline:\n  rework_rounds: twice\n", "not a number of rounds"},
		{"pipeline:\n  rework_rounds: -1\n", "not a number of rounds"},
		{"pipeline:\n  skills:\n    reviewers: [x]\n", "is not a stage"},
	} {
		if _, err := parsePipeline(nil, parseYAMLish(bad.yaml), "roles.yaml"); err == nil ||
			!strings.Contains(err.Error(), bad.want) {
			t.Fatalf("%q: want %q, got %v", bad.yaml, bad.want, err)
		}
	}
}

func TestTheBlockMergesKeyByKeyAcrossFiles(t *testing.T) {
	// The team's file, then a $LCA_ROLES overlay that changes one thing. The
	// overlay must not have to restate the tracker's tools.
	pc, err := parsePipeline(nil, parseYAMLish(fullPipeline), "team.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pc, err = parsePipeline(pc, parseYAMLish("pipeline:\n  target_branch: integration\n  push: false\n"), "overlay.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if pc.Target != "integration" || pc.pipelinePush() {
		t.Fatalf("the later file wins per key: target %q push %t", pc.Target, pc.pipelinePush())
	}
	if pc.Tracker.Read != "jira__issue_get" || pc.Coder != "coder" {
		t.Fatalf("and does not erase the keys it is silent about: %+v", pc)
	}
	if pc.Src != "overlay.yaml" {
		t.Fatalf("the errors name the newest writer: %q", pc.Src)
	}
}

func TestARoleThatIsNotInTheTeamIsNamed(t *testing.T) {
	rc := &RolesConfig{Roles: []*Agent{{Name: "coder"}, {Name: "reviewer"}}}
	err := pipeFrom(t, fullPipeline).validate(rc, tktNeed{})
	if err == nil || !strings.Contains(err.Error(), "integrator") {
		t.Fatalf("an unknown role is the same class of mistake as an unknown key: %v", err)
	}
	if !strings.Contains(err.Error(), "coder, reviewer") {
		t.Fatalf("and the error lists the team: %v", err)
	}
}

// ── the shared knowledge base ───────────────────────────────────────────────

// writeSkill makes one skill in a directory, the way a shared repository holds
// them.
func writeSkill(t *testing.T, at, name, desc, body string, extra map[string]string) string {
	t.Helper()
	d := filepath.Join(at, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	for n, c := range extra {
		if err := os.WriteFile(filepath.Join(d, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

const skilledPipeline = fullPipeline + `  skills:
    coder: [deploy-to-stand, repo-conventions]
    reviewer: [review-checklist]
    mr: [mr-description]
`

func TestEachStageIsGivenTheSkillsTheBlockNamedAndNothingElse(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	writeSkill(t, shared, "deploy-to-stand", "how we deploy", "ssh stage and run stage.sh deploy", nil)
	writeSkill(t, shared, "repo-conventions", "house style", "comments explain why", nil)
	writeSkill(t, shared, "review-checklist", "what we always check", "check the redaction at every sink", nil)
	writeSkill(t, shared, "mr-description", "what an MR must contain", "the verifying command and its exit", nil)
	writeSkill(t, shared, "unnamed", "nobody asked for this", "do something else", nil)

	skills := loadSkills(filepath.Join(base, "proj"), filepath.Join(base, "proj", ".lca"))
	t.Setenv("LCA_SKILLS", shared)
	skills = loadSkills(filepath.Join(base, "proj"), filepath.Join(base, "proj", ".lca"))
	if len(skills) < 5 {
		t.Fatalf("the shared repository is read: %v", sortedKeys(skills))
	}

	pc := pipeFrom(t, skilledPipeline)
	got, err := resolveStageSkills(pc, skills, []string{shared}, tktNeed{creating: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		tktStageCoder:    {"deploy-to-stand", "repo-conventions"},
		tktStageReviewer: {"review-checklist"},
		tktStageMR:       {"mr-description"},
		tktStageTicket:   nil,
		tktStageReport:   nil,
	}
	for stage, names := range want {
		var have []string
		for _, s := range skillsFor(got, stage) {
			have = append(have, s.Name)
		}
		if strings.Join(have, ",") != strings.Join(names, ",") {
			t.Fatalf("%s got %v, want %v — named per stage, never left to the model to pick", stage, have, names)
		}
	}

	// The instructions reach THAT stage's task message, in the block's order, and
	// no other stage's.
	coder := stageInstructions(got, tktStageCoder)
	if !strings.Contains(coder, "stage.sh deploy") || !strings.Contains(coder, "comments explain why") {
		t.Fatalf("the coder is given both of its skills' instructions:\n%s", coder)
	}
	if strings.Index(coder, "stage.sh deploy") > strings.Index(coder, "comments explain why") {
		t.Fatal("the block's order is the order the instructions arrive in")
	}
	if strings.Contains(coder, "redaction at every sink") {
		t.Fatal("the reviewer's checklist is not the coder's business")
	}
	if stageInstructions(got, tktStageReport) != "" {
		t.Fatal("a stage with no skills named gets none")
	}
	if strings.Contains(stageInstructions(got, tktStageMR), "do something else") {
		t.Fatal("a skill nobody named is never loaded")
	}
}

func TestASkillNamedInTheConfigAndMissingStopsTheRun(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	writeSkill(t, shared, "deploy-to-stand", "how we deploy", "body", nil)
	t.Setenv("LCA_SKILLS", shared)
	skills := loadSkills(filepath.Join(base, "proj"), filepath.Join(base, "proj", ".lca"))

	pc := pipeFrom(t, fullPipeline+"  skills:\n    reviewer: [review-checklist]\n")
	_, err := resolveStageSkills(pc, skills, []string{shared, "/elsewhere/skills"}, tktNeed{creating: true})
	if err == nil {
		t.Fatal("a pipeline silently running without the team's instructions is worse than one that refuses to start")
	}
	if !strings.Contains(err.Error(), "review-checklist") {
		t.Fatalf("the error names the skill: %v", err)
	}
	for _, dir := range []string{shared, "/elsewhere/skills"} {
		if !strings.Contains(err.Error(), dir) {
			t.Fatalf("and the directories searched, so the operator knows where to put it — %s missing from:\n%v", dir, err)
		}
	}
	if !strings.Contains(err.Error(), "deploy-to-stand") {
		t.Fatalf("and what WAS found, because the commonest cause is a typo: %v", err)
	}
	var ue *usageErr
	if !asUsageErr(err, &ue) {
		t.Fatalf("a missing skill is row 2, not a failed task: %T", err)
	}
}

func TestSkillsAreRecordedByNameAndContentHash(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	dir := writeSkill(t, shared, "deploy-to-stand", "how we deploy", "run stage.sh deploy",
		map[string]string{"deploy.sh": "#!/bin/sh\nexit 0\n"})
	t.Setenv("LCA_SKILLS", shared)
	load := func() []TicketSkill {
		skills := loadSkills(filepath.Join(base, "proj"), filepath.Join(base, "proj", ".lca"))
		got, err := resolveStageSkills(pipeFrom(t, fullPipeline+"  skills:\n    coder: [deploy-to-stand]\n"),
			skills, []string{shared}, tktNeed{creating: true})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := load()
	if len(first) != 1 || first[0].Sum == "" || first[0].Files != 2 {
		t.Fatalf("a skill is recorded by name, path and content hash: %+v", first)
	}
	if first[0].Stage != tktStageCoder || first[0].Name != "deploy-to-stand" {
		t.Fatalf("by stage and name: %+v", first[0])
	}

	// Two runs of one ticket are comparable only if the hash moves when the
	// knowledge does — including a bundled script the markdown never mentions.
	if err := os.WriteFile(filepath.Join(dir, "deploy.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := load()
	if second[0].Sum == first[0].Sum {
		t.Fatal("a runbook whose script was rewritten is a different runbook, and the hash has to say so")
	}
}

func TestTheRunRecordsWhichSkillsWereInForce(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	writeSkill(t, shared, "review-checklist", "what we check", "check the sinks", nil)
	t.Setenv("LCA_SKILLS", shared)
	skills := loadSkills(filepath.Join(base, "proj"), filepath.Join(base, "proj", ".lca"))
	pc := pipeFrom(t, fullPipeline+"  skills:\n    reviewer: [review-checklist]\n")
	sk, err := resolveStageSkills(pc, skills, []string{shared}, tktNeed{creating: true})
	if err != nil {
		t.Fatal(err)
	}

	w := newFakeWorld(t)
	r := fresh(t, w, pc)
	r.skills = sk
	r.st.Skills = sk
	if status, reason := r.execute(); status != statusPassed {
		t.Fatalf("%s (%s)", status, reason)
	}
	// In the state file, so a later reader can tell two runs apart...
	on, err := loadTicketState(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(on.Skills) != 1 || on.Skills[0].Sum == "" {
		t.Fatalf("the state file records the skills in force: %+v", on.Skills)
	}
	// ...and in the result object, for the same reason results.jsonl carries
	// roles_hash and prompt_hash.
	res := r.result(statusPassed, "", time.Now())
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"sha256"`) || !strings.Contains(string(b), "review-checklist") {
		t.Fatalf("the -json result carries the skills in force:\n%s", b)
	}
	// And the reviewer actually got them.
	if w.calls["instr:reviewer:"+stageInstructions(sk, tktStageReviewer)] != 1 {
		t.Fatal("the named skill's instructions go into THAT stage's task message")
	}
}

// ── -dry-run and -json ──────────────────────────────────────────────────────

func TestADryRunTouchesNothingAndShowsEveryTransitionItsGateAndItsSkills(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	writeSkill(t, shared, "deploy-to-stand", "how we deploy", "body", nil)
	t.Setenv("LCA_SKILLS", shared)
	skills := loadSkills(filepath.Join(base, "proj"), filepath.Join(base, "proj", ".lca"))
	pc := pipeFrom(t, skilledPipeline)
	pc.Skills = map[string][]string{tktStageCoder: {"deploy-to-stand"}}
	sk, err := resolveStageSkills(pc, skills, []string{shared}, tktNeed{creating: true})
	if err != nil {
		t.Fatal(err)
	}

	w := newFakeWorld(t)
	r := fresh(t, w, pc)
	r.skills, r.st.Skills = sk, sk
	out := captureStdout(t, r.printPlan)

	for _, tr := range tktSpine() {
		if !strings.Contains(out, tr.Name) {
			t.Fatalf("-dry-run prints every transition, and %s is missing:\n%s", tr.Name, out)
		}
		if tr.To != "" && !strings.Contains(out, tr.To) {
			t.Fatalf("...and the state each one records, and %s is missing:\n%s", tr.To, out)
		}
	}
	for _, want := range []string{"check and review", "deploy-to-stand", "agent/BSK-1", "main", "origin"} {
		if !strings.Contains(out, want) {
			t.Fatalf("-dry-run must show %q:\n%s", want, out)
		}
	}
	// The calls, with this ticket's own key and branch substituted. A plan that
	// printed only the tool names would hide the half that goes over the wire, and
	// this is the one thing an operator should read before the first night.
	for _, want := range []string{"jira__issue_get", "jira__issue_comment", "forge__list_mrs",
		"issueIdOrKey=BSK-1", "source_branch=agent/BSK-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("-dry-run must show the call %q:\n%s", want, out)
		}
	}
	// And not the one this invocation cannot make: without -new nothing opens a
	// ticket, so the tool that does must not be in the plan.
	if strings.Contains(out, "jira__issue_create") {
		t.Fatalf("a run without -new cannot open a ticket and must not plan to:\n%s", out)
	}
	if len(w.calls) != 0 {
		t.Fatalf("-dry-run reaches no tracker, no forge, no model and no remote: %v", w.calls)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "state.json")); err == nil {
		t.Fatal("-dry-run writes no state")
	}
}

func TestADryRunMarksWhatIsAlreadyDone(t *testing.T) {
	zero := 0
	w := newFakeWorld(t)
	st := &TicketState{Ticket: "BSK-1", State: tktImplemented, Branch: "agent/BSK-1", Summary: "x",
		BranchAt: "base000", Head: "work111-r0", Check: TicketCheck{Cmd: "go test", Exit: &zero, Attempts: 1},
		Journal: []TicketStep{
			{Name: tktOpen, To: tktOpened, Status: tktOK},
			{Name: tktBranch, To: tktBranched, Status: tktOK},
			{Name: tktImplement, To: tktImplemented, Status: tktOK},
		}}
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)
	out := captureStdout(t, r.printPlan)
	if !strings.Contains(out, "done") {
		t.Fatalf("-dry-run on a half-done ticket says which transitions are already done:\n%s", out)
	}
	if !strings.Contains(out, tktImplemented) {
		t.Fatalf("...and where it stands:\n%s", out)
	}
}

// ── a plan that is wrong is worse than no plan ──────────────────────────────
//
// -dry-run is what an operator reads before letting this near a shared remote,
// so the two ways it can lie are the two things these tests pin: it must not
// promise a stage it will not buy, and it must not exit 0 where the run exits 2.

// A keyed run reads the tracker and buys no model at `open`, so the plan's row 1
// must say so — and the `ticket:` skills must not be required at all, or a team
// that never opens tickets from lca cannot start until somebody authors a file
// lca is never going to read.
func TestAKeyedRunNeitherPlansNorRequiresTheTicketStage(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	writeSkill(t, shared, "ticket-brief", "how we write a ticket", "name the stand", nil)
	t.Setenv("LCA_SKILLS", shared)
	skills := loadSkills(filepath.Join(base, "proj"), filepath.Join(base, "proj", ".lca"))
	pc := pipeFrom(t, fullPipeline+"  skills:\n    ticket: [ticket-brief]\n")

	// Resolution first: with a key, the stage is not run and its skills are not
	// asked for — not even when they are there.
	keyed, err := resolveStageSkills(pc, skills, []string{shared}, tktNeed{})
	if err != nil {
		t.Fatalf("a keyed run must not be held to the ticket stage's skills: %v", err)
	}
	if len(skillsFor(keyed, tktStageTicket)) != 0 {
		t.Fatalf("a skill no stage will load must not be recorded as in force: %+v", keyed)
	}
	// And the one that proves it is the REQUIREMENT and not just the listing: a
	// skill that is not on this machine at all stops only the run that would load
	// it.
	none := map[string]*Skill{}
	if _, err := resolveStageSkills(pc, none, []string{shared}, tktNeed{}); err != nil {
		t.Fatalf("a keyed run must start without the ticket stage's skill existing: %v", err)
	}
	if _, err := resolveStageSkills(pc, none, []string{shared}, tktNeed{creating: true}); err == nil {
		t.Fatal("a -new run DOES load it, so a missing one still stops that run")
	}

	// The plan's own row, which is what an operator reads.
	w := newFakeWorld(t)
	r := fresh(t, w, pc)
	r.skills, r.st.Skills = keyed, keyed
	row := planRow(t, captureStdout(t, r.printPlan), tktOpen)
	if strings.Contains(row, tktStageTicket) {
		t.Fatalf("with a key no model writes at `open`, and the plan must not promise one:\n%s", row)
	}

	// With -new it is the other way round, and the same row says so.
	creating, err := resolveStageSkills(pc, skills, []string{shared}, tktNeed{creating: true})
	if err != nil {
		t.Fatal(err)
	}
	nr := newRun(t, w, pc, &TicketState{Ticket: "", State: tktStart})
	nr.newTask = "make the stand stop dropping big requests"
	nr.skills, nr.st.Skills = creating, creating
	row = planRow(t, captureStdout(t, nr.printPlan), tktOpen)
	for _, want := range []string{tktStageTicket, "ticket-brief"} {
		if !strings.Contains(row, want) {
			t.Fatalf("a -new run writes the body, so the plan names %q:\n%s", want, row)
		}
	}
}

// planRow is one row of -dry-run's transitions table, found by its transition
// name: the row is what carries the stage and the skills, and asserting on the
// whole page would pass on the facts table's own word "ticket".
func planRow(t *testing.T, out, name string) string {
	t.Helper()
	for _, l := range strings.Split(stripANSI(out), "\n") {
		fs := strings.Fields(l)
		if len(fs) > 2 && fs[1] == name {
			return l
		}
	}
	t.Fatalf("no %s row in the plan:\n%s", name, out)
	return ""
}

// The budget rule is the run's, and a plan that exited 0 on a configuration the
// run refuses is a plan an operator cannot use as a gate.
//
// Driven against a real repository, because the plan's own last question needs
// one: with the guard skipped the plan would walk straight past this and answer
// nothing at all, which is exactly the regression.
func TestAPlanIsHeldToTheSameBudgetRuleAsTheRun(t *testing.T) {
	f := newTktRepoFix(t, "BSK-21")
	f.park(t)

	// The two configurations with no clock on them, and the second is the one the
	// guard used to let through: it read `max_steps == unlimited`, so a file that
	// merely SET a step count passed. A step is not a dimension a hang moves in —
	// a gateway that streams four tokens and goes silent advances none and spends
	// none — so a run with 200 steps and no clock is as unbounded as one with
	// `unlimited`, and for the one failure neither ceiling can see.
	for _, rc := range []*RolesConfig{
		{RunMaxSteps: stepsUnlimited},
		{RunMaxSteps: 200},
	} {
		f.orch.budget = armedBudget(t, rc)
		for _, dry := range []bool{false, true} {
			w := newFakeWorld(t)
			r := fresh(t, w, pipeFrom(t, fullPipeline))
			r.orch = f.orch
			err := r.preflight(dry, tktNeed{})
			if err == nil {
				t.Fatalf("max_steps=%d dry=%v: unattended with no clock is the one combination nobody can afford", rc.RunMaxSteps, dry)
			}
			// The key to ADD, because only a clock cancels — a message naming the key
			// that is already there sends the operator to the wrong line.
			if !strings.Contains(err.Error(), "timeout:") {
				t.Fatalf("max_steps=%d dry=%v: the refusal names the key: %v", rc.RunMaxSteps, dry, err)
			}
		}
	}

	// And one real ceiling is enough, so what is refused is the configuration and
	// not the plan.
	for _, rc := range []*RolesConfig{
		{RunMaxSteps: stepsUnlimited, RunTimeout: 45 * time.Minute},
		{RunMaxSteps: stepsUnlimited, RunMaxTokens: 4_000_000},
	} {
		f.orch.budget = armedBudget(t, rc)
		w := newFakeWorld(t)
		r := fresh(t, w, pipeFrom(t, fullPipeline))
		r.orch = f.orch
		if err := r.preflight(true, tktNeed{}); err != nil {
			t.Fatalf("one real ceiling is enough: %v", err)
		}
	}
}

// armedBudget is a run budget as newTicketRun arms one, for the tests that drive
// preflight directly.
func armedBudget(t *testing.T, rc *RolesConfig) *runBudget {
	t.Helper()
	b, err := newRunBudget(rc, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := b.start(context.Background())
	t.Cleanup(cancel)
	return b
}

// A credential variable nobody exported is the ordinary first-night mistake — a
// cron job inherits no shell — and the run is row 2 on its first call. A plan
// can prove it without reaching anything, so it has to.
func TestAPlanNamesTheCredentialVariableNobodyExported(t *testing.T) {
	m := newMockMCP(t)
	f := mcpFix(t, jiraConfig(m.hostPort(), m.URL, ""), lockOf("jira", m.URL, m.tools))
	if mcpTools["jira__issue_get"] == nil {
		t.Fatalf("the fixture's tools must be registered: %s", f.warnText())
	}
	pc := pipeFrom(t, fullPipeline)
	pc.Tracker.Read, pc.Tracker.Comment = "jira__issue_get", "jira__issue_comment_add"

	t.Setenv("JIRA_MCP_TOKEN", "tok-abcdef")
	if err := pc.validateCreds(tktNeed{}); err != nil {
		t.Fatalf("an exported token is the whole requirement: %v", err)
	}
	t.Setenv("JIRA_MCP_TOKEN", "")
	// A clock, because preflight asks for one before it asks about credentials and
	// this test is about the credential.
	b := armedBudget(t, &RolesConfig{RunTimeout: 45 * time.Minute})
	for _, dry := range []bool{false, true} {
		r := &tktRun{orch: &Orchestrator{budget: b}, pc: pc}
		err := r.preflight(dry, tktNeed{})
		if err == nil {
			t.Fatalf("dry=%v: every call to that server would be refused before it left", dry)
		}
		for _, want := range []string{"JIRA_MCP_TOKEN", "tracker: read", "cron job"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("dry=%v: the refusal names %q: %v", dry, want, err)
			}
		}
		var ue *usageErr
		if !asUsageErr(err, &ue) {
			t.Fatalf("that is a thing to fix in the environment, not a failed task: %T", err)
		}
	}
}

// The default cron setup: a clone with the target branch checked out. MergeIn
// refuses it, correctly and by name — but last, after a coder and a reviewer
// have been paid for. One `git worktree list` proves it in the plan.
func TestAPlanSaysWhenTheTargetBranchIsCheckedOutHere(t *testing.T) {
	f := newTktRepoFix(t, "BSK-20")
	f.orch.budget = armedBudget(t, &RolesConfig{RunTimeout: 45 * time.Minute})
	w := newFakeWorld(t)
	r := fresh(t, w, pipeFrom(t, fullPipeline))
	r.orch = f.orch

	// Not parked, which is the ordinary state of a clone somebody works in.
	if err := r.preflight(true, tktNeed{}); err != nil {
		t.Fatal(err)
	}
	if r.targetHeldBy != f.top {
		t.Fatalf("the plan must find the target's own checkout: %q, want %q", r.targetHeldBy, f.top)
	}
	out := stripANSI(captureStdout(t, r.printPlan))
	for _, want := range []string{"checked out", f.top, "switch --detach", "target_branch"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the plan must say it and name the remedy, like MergeIn does — %q missing:\n%s", want, out)
		}
	}
	if r.planExit() != exitUsage {
		t.Fatalf("a plan an operator uses as a gate must not exit 0 here: %d", r.planExit())
	}

	// Parked, which is what an unattended clone should be: nothing to say.
	f.park(t)
	parked := fresh(t, w, pipeFrom(t, fullPipeline))
	parked.orch = f.orch
	if err := parked.preflight(true, tktNeed{}); err != nil {
		t.Fatal(err)
	}
	if parked.targetHeldBy != "" || parked.planExit() != exitOK {
		t.Fatalf("a parked clone holds no branch: %q / %d", parked.targetHeldBy, parked.planExit())
	}
	if strings.Contains(stripANSI(captureStdout(t, parked.printPlan)), "checked out") {
		t.Fatal("and a plan with nothing to refuse must not invent a warning")
	}
}

func TestTheResultObjectCarriesTheTableTheStateAndWhatBlockedIt(t *testing.T) {
	w := newFakeWorld(t)
	two := 2
	w.check = TicketCheck{Cmd: "go test ./...", Exit: &two, Attempts: 2, Tail: "FAIL"}
	r := fresh(t, w, pipeFrom(t, fullPipeline))
	status, reason := r.execute()
	res := r.result(status, reason, time.Now())

	if res.Status != statusFailed || res.exitCode() != exitFailed {
		t.Fatalf("status %q exit %d", res.Status, res.exitCode())
	}
	if res.BlockedAt != tktImplemented || res.Blocked == "" {
		t.Fatalf("the object says where the work stands and what stopped it: %+v", res)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	// The unconditional fields, for the reason runResult's have no omitempty: a
	// caller written against the documented object indexes them on exactly the row
	// that sends the ticket to a person.
	for _, k := range []string{"status", "ticket", "state", "check_cmd", "check_exit", "check_tail",
		"check_logs", "skills", "transitions", "state_file", "lca_version", "started_at", "finished_at"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("%q must always be present:\n%s", k, b)
		}
	}
	if m["check_logs"] == nil || m["skills"] == nil || m["transitions"] == nil {
		t.Fatalf("an empty list is an answer; null is a KeyError waiting to happen:\n%s", b)
	}
	if m["merge_request"] != nil {
		t.Fatal("a blocked run has no merge request, and must not look like it has one")
	}

	// A check that never ran reports null, which is the one answer that must not
	// be inventable.
	empty := (&tktRun{st: &TicketState{}, dir: t.TempDir()}).result(statusInfra, "the gateway is down", time.Now())
	b, _ = json.Marshal(empty)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["check_exit"] != nil {
		t.Fatalf("check_exit must be null when nothing checked anything:\n%s", b)
	}
	if statusExitCode(statusInfra) != exitInfra {
		t.Fatal("infra_error is exit 3: put the ticket back, try again later")
	}
}

func TestTheExitTableIsTheOneShotTable(t *testing.T) {
	for _, c := range []struct {
		status string
		code   int
	}{
		{statusPassed, exitOK},
		{statusFailed, exitFailed},
		{statusUnverified, exitFailed},
		{statusConfig, exitUsage},
		{statusInfra, exitInfra},
		{statusBudget, exitBudget},
		{statusCancelled, exitCancelled},
	} {
		if got := (ticketResult{Status: c.status}).exitCode(); got != c.code {
			t.Fatalf("%s must be exit %d, got %d", c.status, c.code, got)
		}
	}
}

// ── this build's seam ───────────────────────────────────────────────────────

func TestEveryTransitionHasAProbeAGateAndADo(t *testing.T) {
	for _, tr := range tktSpine() {
		if tr.probe == nil || tr.do == nil {
			t.Fatalf("%s needs a probe and a do: a transition that cannot prove its own effect cannot be resumed", tr.Name)
		}
		if tr.To == "" {
			t.Fatalf("%s records no state", tr.Name)
		}
		if tktOrder(tr.To) < 0 {
			t.Fatalf("%s records %q, which is not a state this build knows", tr.Name, tr.To)
		}
		if tr.Does == "" && tr.Name != tktReport {
			t.Fatalf("%s must say what lca itself performs, for -dry-run", tr.Name)
		}
	}
	// Every state is reachable: each one is some transition's To, and each has a
	// resume entry.
	for _, state := range []string{tktOpened, tktBranched, tktImplemented, tktReviewed,
		tktReworked, tktResolved, tktMerged, tktPushed, tktProposed, tktReported} {
		found := false
		for _, tr := range tktSpine() {
			if tr.To == state {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is a state no transition reaches", state)
		}
		if tktOrder(state) < 0 {
			t.Fatalf("%s is a state the walk cannot place, so a re-run would refuse the file", state)
		}
	}
}

// Exactly two transitions refuse to be re-entered blind, and each of them has to
// have its own sentence for it: opening a ticket, which no tool can undo and no
// configured tool can search for, and the integrator's one attempt at a
// conflict, which is a budget of one and not a call to repeat. Every other
// transition either proves its own effect or is a no-op when repeated, and a
// `Redo: false` added to one of those would silently turn a recoverable crash
// into a ticket a person has to unstick by hand.
func TestOnlyTwoTransitionsRefuseToBeReEnteredBlind(t *testing.T) {
	once := map[string]bool{tktOpen: true, tktResolve: true}
	for _, tr := range tktSpine() {
		if once[tr.Name] {
			if tr.Redo {
				t.Fatalf("%s must not be re-entered blind after a crash", tr.Name)
			}
			if tr.once == nil {
				t.Fatalf("%s refuses a blind re-entry and says nothing about why", tr.Name)
			}
			continue
		}
		if !tr.Redo {
			t.Fatalf("%s must be safe to re-enter — every other transition either probes its own effect or is a no-op when repeated", tr.Name)
		}
		if tr.once != nil {
			t.Fatalf("%s is re-enterable, so its refusal sentence can never be printed", tr.Name)
		}
	}
}

// TestABranchWithNoWorktreeLeftIsGivenOneAgain is the half of `branched` that a
// resume discovers: the branch outlives everything and the DIRECTORY does not.
// `lca clean` removes a worktree, a reboot with the state directory on tmpfs
// removes it, and a run that proved only the branch would then send the coder to
// a path that is not there.
func TestABranchWithNoWorktreeLeftIsGivenOneAgain(t *testing.T) {
	w, pc := newFakeWorld(t), pipeFrom(t, fullPipeline)
	r := fresh(t, w, pc)
	if blocked, _, err := r.walk(); err != nil || blocked != "" {
		t.Fatalf("walk: %q %v", blocked, err)
	}
	// Somebody removed it. The branch is untouched.
	delete(w.worktrees, r.st.Branch)
	r2 := newRun(t, w, pc, r.st)
	if blocked, _, err := r2.walk(); err != nil || blocked != "" {
		t.Fatalf("re-entry: %q %v", blocked, err)
	}
	if r2.st.Worktree == "" {
		t.Fatal("the run re-entered with no worktree recorded")
	}
	if w.worktrees[r2.st.Branch] != r2.st.Worktree {
		t.Fatalf("the recorded worktree %q is not the one the repository has (%q)", r2.st.Worktree, w.worktrees[r2.st.Branch])
	}
	if n := w.calls["cutbranch"]; n != 1 {
		t.Fatalf("the branch was cut %d times — one ticket, one branch", n)
	}
}

// TestTheWorldIsWiredFromOnePlace is the shape the four implementations have to
// keep: lca's own session holds the write grants and no stage shares one.
func TestTheWorldIsWiredFromOnePlace(t *testing.T) {
	pc := pipeFrom(t, fullPipeline)
	grants := pc.writeTools(tktNeed{})
	for _, want := range []string{"jira__issue_comment", "jira__issue_transition", "forge__create_mr"} {
		if !contains(grants, want) {
			t.Fatalf("%s is written to and is not granted: %v", want, grants)
		}
	}
	if contains(grants, "jira__issue_create") {
		t.Fatalf("a run without -new must not hold the grant that opens tickets: %v", grants)
	}
	if contains(grants, pc.Tracker.Read) {
		t.Fatalf("a read needs no write grant: %v", grants)
	}
	if got := pc.writeTools(tktNeed{creating: true}); !contains(got, "jira__issue_create") {
		t.Fatalf("-new must hold it: %v", got)
	}
	// push: false is a pipeline that cannot reach the forge at all.
	no := false
	pc.Push = &no
	if got := pc.writeTools(tktNeed{}); contains(got, "forge__create_mr") {
		t.Fatalf("push: false must hold no forge grant: %v", got)
	}
}

func TestTheBranchNameIsOneExpression(t *testing.T) {
	pc := pipeFrom(t, fullPipeline)
	if got := tktBranchFor(pc, "BSK-123"); got != "agent/BSK-123" {
		t.Fatalf("got %q", got)
	}
	// A key with characters git refuses in a ref must not produce a branch git
	// will not take — and must not be silently renamed to another ticket's branch.
	for _, bad := range []string{"BSK 1", "BSK~1", "BSK^1", "BSK:1", "BSK?1", "BSK*1", "BSK[1]", "BSK\\1"} {
		got := tktBranchFor(pc, bad)
		for _, ch := range []string{" ", "~", "^", ":", "?", "*", "[", "]", "\\"} {
			if strings.Contains(strings.TrimPrefix(got, pc.BranchPrefix), ch) {
				t.Fatalf("%q became %q, which git check-ref-format refuses", bad, got)
			}
		}
	}
}

func TestATicketKeyNeverBecomesAPath(t *testing.T) {
	for _, bad := range []string{"../../etc/passwd", "a/b", "..", "./x"} {
		got := tktSlug(bad)
		if strings.Contains(got, "/") || strings.Contains(got, "..") {
			t.Fatalf("%q became %q — a wrapper interpolating the wrong variable must not write outside the state directory", bad, got)
		}
	}
	dir := ticketDir(Config{Dir: "/state"}, "../../etc")
	if !strings.HasPrefix(dir, filepath.Join("/state", "tickets")+string(filepath.Separator)) {
		t.Fatalf("every ticket's state stays under tickets/: %q", dir)
	}
}

func TestTheStateFileScrubsSecretsAndLeavesTheEvidenceAlone(t *testing.T) {
	armSecrets(t, map[string]string{"BSK_TOKEN": "sk-not-a-real-token-000000"})

	st := &TicketState{Version: tktStateVersion, Ticket: "BSK-1",
		Summary:  "the stand rejects sk-not-a-real-token-000000",
		BranchAt: "base000", Head: "work111",
		Check:  TicketCheck{Tail: "curl -H 'Bearer sk-not-a-real-token-000000'"},
		Review: &reviewReport{Verdict: tktApprove, Summary: "token sk-not-a-real-token-000000 is hardcoded"},
		Journal: []TicketStep{{Name: tktBranch, Status: tktOK,
			Evidence: "cut at base000", Detail: "sk-not-a-real-token-000000"}},
	}
	dir := t.TempDir()
	if err := saveTicketState(dir, st); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-not-a-real-token-000000") {
		t.Fatalf("a ticket body and a review come from outside this program, and this is a file a person opens:\n%s", b)
	}
	back, err := loadTicketState(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The fields a PROBE compares must survive byte for byte, for the reason
	// redactedRunState leaves `vars` and `sum` alone: a [redacted] here would make
	// a resumed run either refuse to start or re-take a transition it had taken.
	if back.BranchAt != "base000" || back.Head != "work111" {
		t.Fatalf("the evidence the probes compare against must not be scrubbed: %+v", back)
	}
	if st.Summary != "the stand rejects sk-not-a-real-token-000000" {
		t.Fatal("the redaction happens on a copy: the live state is what the rest of the run reads")
	}
}

// /role save rewrites the WHOLE roles.yaml from RolesConfig.YAML(), so a block
// this renderer forgot would be silently deleted the first time somebody saved a
// team from a session — and with it the team's tracker and forge tool names,
// which nothing in lca can reconstruct.
func TestTheBlockSurvivesARoundTripThroughRoleSave(t *testing.T) {
	pc := pipeFrom(t, skilledPipeline)
	rc := &RolesConfig{Entry: "lead", Apply: "verified", VerifyAttempts: 2, CheckTimeout: 600,
		Pipeline: pc, Roles: []*Agent{{Name: "lead", IsRole: true, Models: []string{"m1"}}},
		Members: map[string]*Member{localMemberName: {Name: localMemberName}}}

	back, err := parsePipeline(nil, parseYAMLish(rc.YAML()), "written.yaml")
	if err != nil {
		t.Fatalf("what YAML() wrote must parse: %v\n%s", err, rc.YAML())
	}
	if back == nil {
		t.Fatalf("the pipeline: block was dropped by the round trip:\n%s", rc.YAML())
	}
	for _, c := range [][3]string{
		{"project", pc.Project, back.Project},
		{"branch_prefix", pc.BranchPrefix, back.BranchPrefix},
		{"remote", pc.Remote, back.Remote},
		{"target_branch", pc.Target, back.Target},
		{"coder", pc.Coder, back.Coder},
		{"reviewer", pc.Reviewer, back.Reviewer},
		{"integrator", pc.Integrator, back.Integrator},
		{"tracker read", pc.Tracker.Read, back.Tracker.Read},
		{"tracker create", pc.Tracker.Create, back.Tracker.Create},
		{"tracker comment", pc.Tracker.Comment, back.Tracker.Comment},
		{"tracker transition", pc.Tracker.Transition, back.Tracker.Transition},
		{"status_done", pc.Tracker.StatusDone, back.Tracker.StatusDone},
		{"status_blocked", pc.Tracker.StatusBlocked, back.Tracker.StatusBlocked},
		{"create_merge_request", pc.Forge.CreateMR, back.Forge.CreateMR},
		{"find_merge_request", pc.Forge.FindMR, back.Forge.FindMR},
		{"tracker read args", argText(pc.Tracker.Args, tktVerbRead), argText(back.Tracker.Args, tktVerbRead)},
		{"tracker comment args", argText(pc.Tracker.Args, tktVerbComment), argText(back.Tracker.Args, tktVerbComment)},
		{"tracker create args", argText(pc.Tracker.Args, tktVerbCreate), argText(back.Tracker.Args, tktVerbCreate)},
		{"tracker transition args", argText(pc.Tracker.Args, tktVerbMove), argText(back.Tracker.Args, tktVerbMove)},
		{"forge create args", argText(pc.Forge.Args, tktVerbCreateMR), argText(back.Forge.Args, tktVerbCreateMR)},
		{"forge find args", argText(pc.Forge.Args, tktVerbFindMR), argText(back.Forge.Args, tktVerbFindMR)},
		{"tracker fields", fieldText(pc.Tracker.Fields), fieldText(back.Tracker.Fields)},
		{"forge fields", fieldText(pc.Forge.Fields), fieldText(back.Forge.Fields)},
	} {
		if c[1] != c[2] {
			t.Fatalf("%s did not survive: %q became %q", c[0], c[1], c[2])
		}
	}
	if back.Push == nil || *back.Push != *pc.Push || back.Rounds == nil || *back.Rounds != *pc.Rounds {
		t.Fatalf("push and rework_rounds must survive as the values they are: %v %v", back.Push, back.Rounds)
	}
	for _, stage := range tktStages {
		if strings.Join(pc.Skills[stage], ",") != strings.Join(back.Skills[stage], ",") {
			t.Fatalf("%s skills: %v became %v", stage, pc.Skills[stage], back.Skills[stage])
		}
	}
	if err := back.validate(nil, tktNeed{creating: true}); err != nil {
		t.Fatalf("and what came back must still be a valid block: %v", err)
	}

	// A team with no block must not grow an empty one: validate would then say the
	// nine keys are missing instead of explaining what the block is for.
	rc.Pipeline = nil
	if strings.Contains(rc.YAML(), "pipeline:") {
		t.Fatalf("a team that never configured a pipeline gets no block:\n%s", rc.YAML())
	}
}

// The cron line runs again tomorrow night, and the ticket it blocked on is still
// there. Two things have to hold: the transition whose effect is missing is the
// one re-entered, and the ticket does not collect one identical comment per
// night.
func TestTomorrowNightsRunRetriesWhatIsStillMissingAndSaysNothingTwice(t *testing.T) {
	w := newFakeWorld(t)
	two := 2
	w.check = TicketCheck{Cmd: "go test ./...", Exit: &two, Attempts: 2, Tail: "FAIL"}
	r := fresh(t, w, pipeFrom(t, fullPipeline))
	if status, _ := r.execute(); status != statusFailed {
		t.Fatal("the first night blocks on the red check")
	}
	if r.st.BlockedAt != tktImplemented || len(w.comments) != 2 { // the comment, then the status move
		t.Fatalf("blocked at %q with %v", r.st.BlockedAt, w.comments)
	}

	// Night two: still red. The coder is re-entered — its effect, a green check,
	// is not there — and the ticket hears nothing new.
	again := newRun(t, w, pipeFrom(t, fullPipeline), r.st)
	if status, _ := again.execute(); status != statusFailed {
		t.Fatal("still blocked")
	}
	if w.calls["stage:coder"] != 2 {
		t.Fatalf("the transition whose effect is missing is the one re-entered: coder ran %d times", w.calls["stage:coder"])
	}
	if w.calls["cutbranch"] != 1 {
		t.Fatalf("and the branch is proved, not cut again: %d", w.calls["cutbranch"])
	}
	if w.calls["comment"] != 1 {
		t.Fatalf("a re-run that reached the same state must not comment again: %d comments", w.calls["comment"])
	}

	// Night three: somebody fixed the test. The run carries on from where it was.
	zero := 0
	w.check = TicketCheck{Cmd: "go test ./...", Exit: &zero, Attempts: 1}
	third := newRun(t, w, pipeFrom(t, fullPipeline), again.st)
	if status, reason := third.execute(); status != statusPassed {
		t.Fatalf("a green night finishes the ticket: %s (%s)", status, reason)
	}
	if w.calls["createmr"] != 1 || w.calls["comment"] != 2 {
		t.Fatalf("one merge request, and a second comment because there is something new to say: mr %d comments %d",
			w.calls["createmr"], w.calls["comment"])
	}
	if third.st.State != tktReported || third.st.Blocked != "" {
		t.Fatalf("and the state file no longer says it is blocked: %q / %q", third.st.State, third.st.Blocked)
	}
}

// ── the transitions that are only half proved, and the night that runs out ───
//
// Each of these is a crash in a one-instruction window, or a clock that ran out,
// put back into the machine. They are the failures that cost a night: the
// cheapest of them wedges one ticket for ever, and the run says it passed.

// A -new run killed just AFTER the create has a key on disk and `pending: open`
// in the same file. The probe is the thing that can tell "the call landed" from
// "it did not", so it is asked first — a refusal evaluated before it wedged that
// ticket for ever, and its remedy (re-run with the key) could not work, because
// the state file a re-run reads is the same one.
func TestANewRunKilledJustAfterTheCreateIsContinued(t *testing.T) {
	w := newFakeWorld(t)
	w.nextKey = "BSK-9"
	st := &TicketState{Ticket: "BSK-9", Summary: "the stand drops requests over 8k",
		State: tktStart, Pending: tktOpen}
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)
	r.newTask = "make the stand stop dropping big requests"

	status, reason := r.execute()
	if status != statusPassed {
		t.Fatalf("the ticket exists, so the run continues it: %s (%s)\n%+v", status, reason, r.st.Journal)
	}
	if w.calls["create"] != 0 {
		t.Fatalf("and it must not open a second one: %d", w.calls["create"])
	}
	if r.st.State != tktReported {
		t.Fatalf("a continued ticket goes all the way: %q", r.st.State)
	}
}

// A branch whose ref exists with no recorded base: killed in the one instruction
// between `worktree add -b` and the save. CutBranch writes the authorship ref at
// the same moment the branch appears, so git can still answer whose branch it
// is — without that, the run refused its own branch every night and nothing in
// the message said that deleting the branch was the way out.
func TestABranchCutBeforeItsSaveIsStillOursAndItsBaseIsRecovered(t *testing.T) {
	w := newFakeWorld(t)
	w.heads["agent/BSK-1"] = "base000"
	w.madeAt = map[string]string{"agent/BSK-1": "base000"}
	st := &TicketState{Ticket: "BSK-1", Summary: "x", State: tktOpened,
		Branch: "agent/BSK-1", BranchAt: "", Pending: tktBranch}
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)

	status, reason := r.execute()
	if status != statusPassed {
		t.Fatalf("a branch carrying lca's own made-ref is ours: %s (%s)", status, reason)
	}
	if r.st.BranchAt != "base000" {
		t.Fatalf("and the base it was cut at is recovered from that ref: %q", r.st.BranchAt)
	}
	if w.calls["cutbranch"] != 0 {
		t.Fatalf("the branch is there, so nothing cuts it again: %d", w.calls["cutbranch"])
	}
}

// A run that ERRORS must not write `reported` over the state the work actually
// stands in. auditState refuses a document that says `merged` or past it with no
// green check under it — which is the right audit of a journal somebody edited,
// and was a correct audit of one lca itself had corrupted. Three nights and the
// ticket could not be resumed at all.
func TestAnErroredRunLeavesTheStateResumable(t *testing.T) {
	w := newFakeWorld(t)
	w.heads["agent/BSK-1"] = "somebodys-work" // there before we got here
	r := fresh(t, w, pipeFrom(t, fullPipeline))

	status, _ := r.execute()
	if status != statusConfig {
		t.Fatalf("a branch nobody here cut is row 2: %s", status)
	}
	if r.st.State != tktOpened {
		t.Fatalf("the state says where the work stands, and that is not `reported`: %q", r.st.State)
	}
	for _, s := range stepsNamed(r.st, tktReport) {
		if s.To != "" {
			t.Fatalf("a run that did not walk the whole arc records no state for its report: %+v", s)
		}
	}
	if len(w.comments) == 0 {
		t.Fatal("it still tells the ticket, which is what the report is for")
	}

	// Night two, on the document night one left: the refusal is still the branch
	// and not an integrity problem lca invented.
	again := newRun(t, w, pipeFrom(t, fullPipeline), r.st)
	status, reason := again.execute()
	if status != statusConfig || !strings.Contains(reason, "somebody else's branch") {
		t.Fatalf("every later night must say the same thing: %s (%s)", status, reason)
	}
	if strings.Contains(reason, "cannot be resumed") {
		t.Fatalf("and never that the document itself is unresumable: %s", reason)
	}

	// And the invariant on its own, so it does not depend on some earlier line
	// having remembered to write a blocked sentence: only a run that walked the
	// WHOLE arc may record `reported`.
	clean := newFakeWorld(t)
	third := newRun(t, clean, pipeFrom(t, fullPipeline),
		&TicketState{Ticket: "BSK-1", Summary: "x", State: tktImplemented})
	if rerr := third.reportOut(statusInfra); rerr != nil {
		t.Fatal(rerr)
	}
	if third.st.State != tktImplemented {
		t.Fatalf("an infra_error is not a walk, whatever is in `blocked`: %q", third.st.State)
	}
}

// A -new run whose create call failed has nothing to comment ON. Every tracker
// call it used to make carried an empty key — a read, a comment and a status
// move — and a server that is lenient about a missing key resolves those
// somewhere nobody asked.
func TestARunWithNoTicketKeyWritesNothingToTheTracker(t *testing.T) {
	w := newFakeWorld(t)
	w.nextKey = "" // the create call fails
	r := newRun(t, w, pipeFrom(t, fullPipeline), &TicketState{Ticket: "", State: tktStart})
	r.newTask = "make the stand stop dropping big requests"

	status, _ := r.execute()
	if status == statusPassed || r.st.Ticket != "" {
		t.Fatalf("nothing was opened: %s / %q", status, r.st.Ticket)
	}
	for _, call := range []string{"read", "findcomment", "comment", "move"} {
		if w.calls[call] != 0 {
			t.Fatalf("a write aimed at no identified ticket is never made, and %s was: %v", call, w.calls)
		}
	}
	steps := stepsNamed(r.st, tktReport)
	if len(steps) != 1 || steps[0].Status != tktSkipped {
		t.Fatalf("and the journal says why, rather than going quiet: %+v", steps)
	}
	if !strings.Contains(steps[0].Detail, "no ticket to comment on") {
		t.Fatalf("by name: %q", steps[0].Detail)
	}
}

// A process killed between the comment and the status move finds its own marker
// on every later night. The marker proves the COMMENT and nothing else, so the
// move is recorded separately — otherwise the run recorded `report already`,
// exited 0, and the board never moved while the journal said it had.
func TestAReportKilledAfterItsCommentStillMovesTheStatus(t *testing.T) {
	w := newFakeWorld(t)
	w.moveErr = fmt.Errorf("the tracker refused the transition")
	r := fresh(t, w, pipeFrom(t, fullPipeline))
	if status, _ := r.execute(); status != statusPassed {
		t.Fatal("the work itself is done; only the move failed")
	}
	if r.st.CommentID == "" || r.st.MovedTo != "" {
		t.Fatalf("the comment landed and the move did not: %q / %q", r.st.CommentID, r.st.MovedTo)
	}

	// Night two: the comment is found by the marker, so it is not posted again —
	// and the half still owed is made.
	w.moveErr = nil
	again := newRun(t, w, pipeFrom(t, fullPipeline), r.st)
	if status, reason := again.execute(); status != statusPassed {
		t.Fatalf("%s (%s)", status, reason)
	}
	if again.st.MovedTo == "" {
		t.Fatal("the status move is the half the marker cannot prove, and it has to happen")
	}
	if w.calls["comment"] != 1 {
		t.Fatalf("a second identical comment is what the marker exists to prevent: %d", w.calls["comment"])
	}
	if w.calls["move"] != 2 {
		t.Fatalf("and the move is re-entered: %d", w.calls["move"])
	}
}

// The night's clock running out is the commonest unattended failure there is,
// and the budget ends a run by CANCELLING its context — so by the time anything
// reports, every symptom reads as an interrupt. The two go to different queues:
// 130 says "the operator stopped it" and sends the ticket back to be retried for
// ever, 4 says "it did not fit" and sends it to a person. A slow ticket retried
// every night is the failure that costs the most.
func TestASpentClockIsBudgetExceededAndNotCancelled(t *testing.T) {
	b, err := newRunBudget(&RolesConfig{RunTimeout: time.Nanosecond}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := b.start(context.Background())
	defer cancel()

	w := newFakeWorld(t)
	w.readErr = context.Canceled // what a stage cut off mid-arc comes back as
	r := fresh(t, w, pipeFrom(t, fullPipeline))
	r.orch = &Orchestrator{budget: b}

	status, reason := r.execute()
	if status != statusBudget {
		t.Fatalf("a spent budget outranks the shape of the stop it caused: %s (%s)", status, reason)
	}
	if !strings.Contains(reason, "time budget is spent") {
		t.Fatalf("and it says which ceiling, in the budget's own words: %q", reason)
	}
	if statusExitCode(status) != exitBudget {
		t.Fatalf("4 goes to a person, 130 goes back in the queue: %d", statusExitCode(status))
	}
	// A completed walk still outranks it: work that was done and verified before
	// the clock ran out was done.
	green := newFakeWorld(t)
	done := fresh(t, green, pipeFrom(t, fullPipeline))
	done.orch = &Orchestrator{budget: b}
	if status, reason := done.execute(); status != statusPassed {
		t.Fatalf("a finished arc is a success whatever the clock says: %s (%s)", status, reason)
	}
}

// And the ticket has to hear about that night. The report is the one transition
// that still has to happen when the run's own context is already dead, so it is
// bought on its own short-lived clock — bounded by the same closing deadline the
// summary's equivalent problem uses, so it cannot outlive the promise the budget
// made either.
func TestATrippedClockStillTellsTheTicket(t *testing.T) {
	b, err := newRunBudget(&RolesConfig{RunTimeout: time.Nanosecond}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := b.start(context.Background())
	defer cancel()
	<-ctx.Done() // the night is over before anything was bought

	w := newFakeWorld(t)
	r := fresh(t, w, pipeFrom(t, fullPipeline))
	r.orch, r.ctx = &Orchestrator{budget: b}, ctx

	status, reason := r.execute()
	if status != statusBudget {
		t.Fatalf("%s (%s)", status, reason)
	}
	if len(w.comments) == 0 || !strings.Contains(w.comments[0], r.st.Marker) {
		t.Fatalf("the report gets its own clock, or the ticket hears nothing at all: %v", w.comments)
	}
	if w.calls["move"] != 1 {
		t.Fatalf("and the status it earned is applied: %d", w.calls["move"])
	}
	if r.ctx != ctx {
		t.Fatal("the run's own context is put back, so nothing after the report runs on borrowed time")
	}
}

// And when the stage itself cannot be bought, lca writes the comment from the
// journal. It holds every fact that comment needs; going quiet is the one
// failure this whole transition exists to prevent.
func TestAnUnbuyableReportStageIsWrittenFromTheJournalInstead(t *testing.T) {
	w := newFakeWorld(t)
	two := 2
	w.check = TicketCheck{Cmd: "go test ./...", Exit: &two, Attempts: 2, Tail: "FAIL"}
	w.commentErr = fmt.Errorf("the gateway is not answering")
	r := fresh(t, w, pipeFrom(t, fullPipeline))

	if status, _ := r.execute(); status != statusFailed {
		t.Fatal("the run is blocked on the red check")
	}
	if len(w.comments) == 0 {
		t.Fatal("a model that cannot be bought is not a reason for the ticket to hear nothing")
	}
	for _, want := range []string{"BLOCKED at " + tktImplemented, r.st.Marker} {
		if !strings.Contains(w.comments[0], want) {
			t.Fatalf("the comment lca wrote itself must carry %q:\n%s", want, w.comments[0])
		}
	}
}

// ── the conflict, at the level of the machine ───────────────────────────────
//
// The git of a conflict is tested against a real repository (ticketgit_test.go).
// What is left here is what only the machine can answer: the key that turns it
// on, what it does to the plan, and that the stage gets the skills a team named
// for it — the one thing a stage running at 3am cannot be left to notice.

func TestResolvingConflictsIsOffUntilTheOperatorNamesTheKey(t *testing.T) {
	off := pipeFrom(t, fullPipeline)
	if off.ResolveConflicts != nil {
		t.Fatal("a key nobody wrote must stay unwritten, so /role save cannot add one")
	}
	if off.resolveConflicts() {
		t.Fatal("resolving a conflict is a decision, and a default that decides is a default nobody chose")
	}
	on := pipeFrom(t, fullPipeline+"  resolve_conflicts: true\n")
	if !on.resolveConflicts() {
		t.Fatal("the documented key must turn it on")
	}
	// Round-tripped through the writer /role save uses: a team's file must come
	// back saying what it said.
	back := pipeFrom(t, on.yaml())
	if !back.resolveConflicts() {
		t.Fatalf("a round trip lost the key:\n%s", on.yaml())
	}
	if said := pipeFrom(t, fullPipeline+"  resolve_conflicts: false\n"); said.ResolveConflicts == nil || *said.ResolveConflicts {
		t.Fatalf("`false` written on purpose is not the same as nothing written: %+v", said.ResolveConflicts)
	}
	// And YAML's dozen spellings of truth are refused here for the reason push:
	// refuses them: a pipeline that quietly resolves nothing looks exactly like a
	// pipeline that never had a conflict.
	if _, err := parsePipeline(nil, parseYAMLish(fullPipeline+"  resolve_conflicts: maybe\n"), "roles.yaml"); err == nil ||
		!strings.Contains(err.Error(), "resolve_conflicts") {
		t.Fatalf("a value that is not true or false must name the key: %v", err)
	}
}

func TestTheResolveStageIsGivenTheSkillsItWasNamedFor(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	writeSkill(t, shared, "how-we-resolve", "how this team resolves conflicts", "take both sides", nil)
	t.Setenv("LCA_SKILLS", shared)
	skills := loadSkills(filepath.Join(base, "proj"), filepath.Join(base, "proj", ".lca"))

	pc := pipeFrom(t, fullPipeline+"  resolve_conflicts: true\n  skills:\n    resolve: [how-we-resolve]\n")
	sk, err := resolveStageSkills(pc, skills, []string{shared}, tktNeed{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sk) != 1 || sk[0].Stage != tktStageResolve {
		t.Fatalf("the resolve stage's skills must resolve: %+v", sk)
	}

	w := newFakeWorld(t)
	w.conflictFiles = "f.txt"
	w.ancestry["work111-r0"] = []string{"base000"}
	w.heads["agent/BSK-1"] = "work111-r0"
	st := &TicketState{Ticket: "BSK-1", State: tktReviewed, Summary: "x", Branch: "agent/BSK-1",
		BranchAt: "base000", Worktree: "/tmp/wt/agent/BSK-1", Head: "work111-r0",
		Verdict: tktApprove, Review: &reviewReport{Verdict: tktApprove, Comments: []reviewComment{}},
		Check: TicketCheck{Cmd: "go test", Exit: &zeroExit, Attempts: 1}}
	r := newRun(t, w, pc, st)
	r.skills, r.st.Skills = sk, sk

	if status, reason := r.execute(); status != statusPassed {
		t.Fatalf("%s (%s)\njournal: %+v", status, reason, r.st.Journal)
	}
	if w.calls["stage:resolve"] != 1 {
		t.Fatalf("the conflict must have cost exactly one attempt: %d", w.calls["stage:resolve"])
	}
	if w.calls["instr:resolve:"+stageInstructions(sk, tktStageResolve)] != 1 {
		t.Fatal("the named skill's instructions go into THAT stage's task message")
	}
	// With the key off, the stage's skills are not even required: a team that
	// never resolves conflicts must not be held to a file lca is not going to read.
	offPC := pipeFrom(t, fullPipeline+"  skills:\n    resolve: [not-a-skill-on-this-machine]\n")
	if _, err := resolveStageSkills(offPC, skills, []string{shared}, tktNeed{}); err != nil {
		t.Fatalf("a stage this run will never buy must not stop it: %v", err)
	}
}

// The plan is what an operator reads before the first night, and the conflict
// has a row in it either way: a `skip` saying the key is off, or a row that says
// what lca itself would perform.
func TestThePlanSaysWhatHappensToAConflict(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{fullPipeline, "resolve_conflicts is false"},
		{fullPipeline + "  resolve_conflicts: true\n", "nothing may merge yet"},
	} {
		pc := pipeFrom(t, tc.yaml)
		r := newRun(t, newFakeWorld(t), pc, &TicketState{Ticket: "BSK-1", State: tktStart})
		row := false
		for _, tr := range r.spine {
			if tr.Name != tktResolve {
				continue
			}
			row = true
			res, why := gateOf(tr, r)
			if res != gateSkip {
				t.Fatalf("a plan for a fresh ticket cannot be standing at a conflict: %v %q", res, why)
			}
			if !strings.Contains(why, tc.want) {
				t.Fatalf("the plan must say %q, not %q", tc.want, why)
			}
			if tr.Does == "" || !strings.Contains(tr.Does, "integrator") {
				t.Fatalf("the row has to say who would do it: %q", tr.Does)
			}
		}
		if !row {
			t.Fatal("the conflict is a transition of its own, so it has a row")
		}
	}
}
