package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// `lca ticket` — the command around the state machine in ticket.go: the flags,
// what -dry-run prints, and the one object -json writes.

// ticketResult is the whole of stdout under -json: one object, at the end, and
// nothing else.
//
// It carries the same status word and the same exit table as a one-shot run
// (oneshot.go), because the caller is the same caller and must not have to learn
// a second contract. What it adds is what only a state machine has: the state it
// reached, the gate that blocked it, the rework rounds it spent, and the skills
// that were in force.
type ticketResult struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`

	Ticket  string `json:"ticket"`
	Summary string `json:"summary,omitempty"`

	// State is where the work actually stands — `implemented` on a red check,
	// `reviewed` on a request_changes, `proposed` on a success with push on,
	// `merged` on a success with push off. Blocked is the gate's own sentence and
	// BlockedAt the state it was standing in, so a caller never has to parse prose
	// to find out whether anything merged.
	State     string `json:"state"`
	Blocked   string `json:"blocked,omitempty"`
	BlockedAt string `json:"blocked_at,omitempty"`

	Round  int `json:"round"`
	Rounds int `json:"rounds"`

	Branch   string `json:"branch,omitempty"`
	Target   string `json:"target,omitempty"`
	Remote   string `json:"remote,omitempty"`
	Head     string `json:"head,omitempty"`
	Pushed   string `json:"pushed_sha,omitempty"`
	Worktree string `json:"worktree,omitempty"`

	// The check's three fields keep the shape runResult gave them and for the same
	// reason: Exit is a POINTER because 0 is the one answer that must not be
	// inventable, and its neighbours have no omitempty because a caller written
	// against the documented object indexes them on exactly the row that calls a
	// human.
	CheckCmd  string   `json:"check_cmd"`
	CheckExit *int     `json:"check_exit"`
	CheckTail string   `json:"check_tail"`
	CheckLogs []string `json:"check_logs"`

	// Review is the reviewer's structured verdict, absent when no review was ever
	// read — absent and not an empty object, for runResult.Review's reason: a
	// review that could not be read must not be able to look like one that
	// approved.
	Review *reviewReport `json:"review,omitempty"`

	MergeRequest *TicketMR `json:"merge_request,omitempty"`
	CommentID    string    `json:"comment_id,omitempty"`

	// Skills is what the shared knowledge base contributed, by stage, name and
	// content hash. Always present and never null, so a caller comparing two runs
	// of one ticket can diff it without a key check.
	Skills []TicketSkill `json:"skills"`

	// Transitions is the journal: every state this run reached, skipped, found
	// already done or was blocked at.
	Transitions []TicketStep `json:"transitions"`

	StateFile  string `json:"state_file"`
	Session    string `json:"session,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	Trace      string `json:"trace,omitempty"`

	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationMs int64  `json:"duration_ms"`

	LCAVersion string `json:"lca_version"`
	RolesHash  string `json:"roles_hash"`
}

// exitCode is the one-shot table, with no legacy clause: this command has no
// history of meaning something else, so every status maps straight through.
func (r ticketResult) exitCode() int { return statusExitCode(r.Status) }

func ticketUsage() {
	fmt.Fprintln(os.Stderr, strings.Join([]string{
		"usage: lca ticket <key> [-dry-run] [-json] | lca ticket -new \"<task>\" […] | lca ticket -list",
		"  lca ticket BSK-123           work that ticket: branch, code, review, merge, push, MR, comment",
		"  lca ticket -new \"<task>\"      open a ticket from that one-liner first, then work it",
		"  lca ticket BSK-123 -dry-run  every transition it would make, its gate, and the skills per stage",
		"  lca ticket BSK-123 -json     one result object on stdout; everything else on stderr",
		"  lca ticket -list             the tickets with state on this machine",
		"",
		" There is no -resume. Re-running the command IS the resume: the state is found",
		" by ticket key, every transition re-proves its own effect against the world, and",
		" the ones already done are skipped. One cron line, whatever happened last night.",
		"",
		" Everything else is roles.yaml's pipeline: block — the tracker's and the forge's",
		" MCP tool names, the branch prefix, the remote, the target branch, the roles, the",
		" rework rounds, whether pushing is allowed, and the skills each stage is given.",
		" A missing key is an error naming the key: lca never guesses a tool name.",
	}, "\n"))
}

// runTicket is the command. It returns the exit code; the table is the one-shot
// table, because the caller is a cron job either way.
func runTicket(cfg Config, args []string) int {
	key, rest := splitLeadingName(args)
	fset := flag.NewFlagSet("ticket", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	fset.Usage = ticketUsage
	newTask := fset.String("new", "", "open a ticket from this one-line task, then work it")
	dry := fset.Bool("dry-run", false, "print every transition, its gate and its skills; change nothing")
	jsonOut := fset.Bool("json", false, "one result object on stdout")
	list := fset.Bool("list", false, "list the tickets with state on this machine")
	if err := fset.Parse(rest); err != nil {
		return exitUsage
	}
	if key == "" && fset.NArg() > 0 {
		key, _ = splitLeadingName(fset.Args())
	}
	if fset.NArg() > 1 || (key != "" && fset.NArg() == 1 && fset.Arg(0) != key) {
		errLine("`lca ticket` takes one ticket key, and %q came after it", fset.Arg(fset.NArg()-1))
		ticketUsage()
		return exitUsage
	}

	if *list {
		printTickets(cfg)
		return exitOK
	}
	switch {
	case key == "" && *newTask == "":
		errLine("`lca ticket` needs a ticket key, or -new with the one-line task to open one")
		ticketUsage()
		return exitUsage
	case key != "" && *newTask != "":
		// A wrapper that passes both means the second and would have got the first,
		// which is -prompt-file's own lesson: it would have worked the named ticket
		// and silently never opened the new one.
		errLine("%s and -new name two different tickets — pass one, not both", key)
		return exitUsage
	}

	// Under -json stdout belongs to the caller's parser and to nothing else, and
	// the whole tree prints through a variable, so moving the variable moves every
	// note, warning and model line to stderr in one go. The same move main.go
	// makes, for the same reason.
	out := os.Stdout
	if *jsonOut {
		os.Stdout = os.Stderr
		applyTheme(themePlain)
	}

	started := time.Now()
	r, err := newTicketRun(cfg, key, *newTask, *dry)
	if err != nil {
		// Row 2 of the table: a missing configuration key, a missing skill, a state
		// file from another tree. Nothing about the task was decided and there is no
		// object to write, because there is no run.
		fmt.Fprintln(os.Stderr, cRed+gDown+cReset+" "+err.Error())
		return exitUsage
	}
	defer r.close()

	if *dry {
		r.printPlan()
		return r.planExit()
	}

	status, reason := r.execute()
	res := r.result(status, reason, started)
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(os.Stderr, "could not write the result object: "+err.Error())
			return exitInfra
		}
	} else {
		r.printOutcome(res)
	}
	return res.exitCode()
}

// ── setting one up ──────────────────────────────────────────────────────────

// close undoes what newTicketRun took, in the order it has to be undone: the
// ticket's lock, then the orchestrator's stdio MCP children (they are PROCESSES,
// and a leaked one outlives lca along with its process group), then the trace
// and the audit log, then the signal handler and the budget's context.
func (r *tktRun) close() {
	if r.unlock != nil {
		r.unlock()
	}
	if r.orch != nil {
		r.orch.rec.Event("session_end", nil)
		r.orch.CloseMCP()
		r.orch.tracer.Close()
		r.orch.rec.Close()
	}
	if r.stop != nil {
		r.stop()
	}
	if r.cancel != nil {
		r.cancel()
	}
}

// newTicketRun resolves everything a run needs BEFORE the tracker is touched or
// a model is bought: the configuration, the skills, the state file, the lock.
// Every failure here is a mistake in the call or in a file, which is the row of
// the table that says "alert somebody, do not touch the ticket" — and finding
// any of them out after a model has read a tree costs a run for nothing.
func newTicketRun(cfg Config, key, newTask string, dry bool) (*tktRun, error) {
	// Nobody at the keyboard, ever: this command is made to be a cron line, so
	// every question becomes an immediate recorded refusal rather than a worktree
	// and a claimed ticket held until somebody notices.
	in := NewInput(os.Stdin)
	ap := NewApprover(in)
	ap.TrustAll()
	ap.quiet = true
	ap.Unattended("lca ticket is unattended: it answers to a program, not to a person")

	orch, err := setupOrchestrator(cfg, ap, os.Getenv("LCA_TRACE"))
	if err != nil {
		return nil, usageErrf("%v", err)
	}
	r := &tktRun{cfg: cfg, orch: orch, newTask: newTask, spine: tktSpine(), runID: orch.rec.id}

	need := tktNeed{creating: newTask != ""}
	pc := orch.roles.pipelineOf()
	if err := pc.validate(orch.roles, need); err != nil {
		r.close()
		return nil, err
	}
	// The tool names and their arguments against the pinned manifest, which is the
	// one source of truth about these tools lca genuinely has. A typo here would
	// otherwise be found by the tracker, at 3am, after the ticket had been read.
	if err := pc.validateTools(need); err != nil {
		r.close()
		return nil, err
	}
	r.pc = pc

	// The skills, resolved here and nowhere later: a skill named in the config and
	// missing from every skills directory stops the run before it starts, because
	// a pipeline silently running without the team's deploy runbook is worse than
	// one that refuses to start.
	dirs := skillDirs(cfg.Root, cfg.Dir)
	sk, err := resolveStageSkills(pc, orch.skills, dirs, need)
	if err != nil {
		r.close()
		return nil, err
	}
	r.skills = sk

	root, _ := realRoot(cfg.Root)
	if err := r.openState(key, root); err != nil {
		r.close()
		return nil, err
	}

	if !dry {
		unlock, lerr := lockTicket(r.dir, r.st.Ticket)
		if lerr != nil {
			r.close()
			return nil, lerr
		}
		r.unlock = unlock
	}

	// The world, after the state file has named the ticket — the repository wants
	// the key for its commit messages and its worktree's owner file — and before
	// any budget is armed, because everything that can go wrong in here is a
	// mistake in a file and belongs to the row of the table that touches nothing.
	//
	// Not under -dry-run: a plan resolves the configuration and the skills, and
	// building the world would mint a session, open a lead and resolve the
	// repository for a run that is not going to happen.
	if !dry {
		w, werr := newTktWorld(orch, cfg, pc, func() string { return r.st.Ticket }, need)
		if werr != nil {
			r.close()
			return nil, werr
		}
		r.w = w
	}

	// The budgets from roles.yaml's defaults:, and the signals that bound a cron
	// job. Everything the run starts hangs off this context — a check command's
	// process group, a stdio MCP server, a subagent — so cancelling it once is
	// what makes "no child left behind" a property of the shape.
	budget, berr := newRunBudget(orch.roles, 0, 0, 0)
	if berr != nil {
		r.close()
		return nil, berr
	}
	orch.budget = budget
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	ctx, cancel := budget.start(ctx)
	orch.setRunContext(ctx)
	r.ctx, r.stop, r.cancel = ctx, stop, cancel

	// The three questions that can only be asked of the world this configuration
	// will meet. After budget.start on purpose: a budget answers for its own
	// ceilings only once it is armed, so asking before that line reads every run
	// as uncapped.
	if err := r.preflight(dry, need); err != nil {
		r.close()
		return nil, err
	}
	return r, nil
}

// preflight is the last of newTicketRun, and the half of it a PLAN is held to
// exactly as a run is.
//
// -dry-run exists so an operator can prove a configuration before letting this
// near a shared remote, which makes a plan that exits 0 where the real run exits
// 2 worse than no plan at all: it is the one output that was supposed to be
// checkable. Every question here is local, deterministic, needs no network, no
// model and no tracker, and is asked of the same facts the run itself is stopped
// by — so there is no `dry` branch on any of them. The one thing a plan does
// differently is the LAST one, and it is noted there.
func (r *tktRun) preflight(dry bool, need tktNeed) error {
	// Unattended and unbounded in every dimension is the one combination nobody
	// can afford, and this command is unattended by construction — there is no
	// person here to read the turns and press Ctrl-C. oneShot refuses the same
	// pairing for the same reason; this one has more of it to refuse, because a
	// ticket buys up to five stages and a rework round buys two of them again.
	b := r.orch.budget
	if b.steps() == stepsUnlimited && !b.bounded() {
		return usageErrf("roles.yaml defaults: max_steps is unlimited and nothing else bounds this run — add timeout: (say 2700) or max_tokens:, or give a step count. `lca ticket` answers to a cron line, and an unattended run has to have one real ceiling.")
	}
	// The credentials the configured servers were given, by name. The run would
	// otherwise reach its first call and stop there.
	if err := r.pc.validateCreds(need); err != nil {
		return err
	}
	if !dry {
		return nil
	}
	// And the mistake that is not in roles.yaml at all: the clone this runs in has
	// the target branch checked out, which is the ordinary state of a clone
	// somebody works in and the default state of one `git clone` just made.
	// MergeIn refuses it — correctly, naming the directory — but it refuses LAST,
	// after a coder and a reviewer have been bought and a branch has been cut, and
	// `git worktree list` answers it now for nothing.
	//
	// A plan only, because the real run must ask at the moment of the merge:
	// between a plan and 3am somebody parks their checkout or switches onto the
	// target, and neither a stale yes nor a stale no may decide a merge.
	g, gerr := newTktGit(r.orch, func() string { return r.st.Ticket }, r.pc.Target)
	if gerr != nil {
		return gerr
	}
	if held, ok := g.worktreeOf(r.pc.Target); ok {
		r.targetHeldBy = held
	}
	// And the git on THIS machine. The merge needs `merge-tree --write-tree`
	// (git 2.38), and refusing for the want of it is as local and deterministic as
	// a missing key — it just arrives last, after the night has been spent, and on
	// the host where it matters it is a property of the image rather than of
	// anything the operator wrote. A plan that let it through was a plan an
	// operator could not use as a gate, which is the one thing this is for.
	if ok, ver := r.orch.mergeTree3Way(g.top); !ok {
		return usageErrf("the merge needs `git merge-tree --write-tree`, which arrived in git 2.38, and the git here is %s. Nothing in this pipeline can merge on this machine: install a newer git on the host that runs the cron line.", ver)
	}
	return nil
}

// planExit is -dry-run's exit code. A plan that found the target branch checked
// out has proved a run cannot finish here, and saying so only in prose would
// leave `lca ticket X -dry-run && …` reading it as a pass — which is the whole
// use an operator puts a plan to.
func (r *tktRun) planExit() int {
	if r.targetHeldBy != "" {
		return exitUsage
	}
	return exitOK
}

// openState finds or creates this ticket's state file, and refuses the two
// documents that must not be continued.
func (r *tktRun) openState(key, root string) error {
	if err := tktCheckKey(key); err != nil {
		return err
	}
	dir := ticketDir(r.cfg, key)
	if key == "" {
		// A -new run keeps state by the hash of its task text until the tracker names
		// the ticket, so the same cron line run twice finds the same half-finished
		// work instead of opening a second ticket. A redirect left by a previous run
		// that got as far as the create points at the real directory.
		dir = newTicketDir(r.cfg, r.newTask)
		if b, err := os.ReadFile(filepath.Join(dir, "redirect")); err == nil {
			if k := strings.TrimSpace(string(b)); k != "" {
				key, dir = k, ticketDir(r.cfg, k)
				r.newTask = "" // it exists now: this is an ordinary continuation
			}
		}
	}
	r.dir = dir

	st, err := loadTicketState(dir)
	if err != nil && !os.IsNotExist(err) {
		return usageErrf("%v", err)
	}
	if st == nil {
		st = &TicketState{Version: tktStateVersion, Ticket: key, Root: root, State: tktStart,
			Started: nowTS(), Journal: []TicketStep{}, Skills: []TicketSkill{}}
	}
	// A state directory is shared by every project under one LCA_DIR, exactly as
	// runs/ and transcripts/ are. A ticket worked in another tree is another
	// project's work, and continuing it would cut a branch in the wrong repository
	// — the refusal -session already makes, for the same reason.
	if st.Root != "" && root != "" && st.Root != root {
		return usageErrf("ticket %s was worked in %s, not in %s — a ticket's state is only unique inside one LCA_DIR", st.Ticket, st.Root, root)
	}
	// And a document for another TICKET, which is the same refusal one line down
	// from the tree one and for a worse failure. Adopting the file's own key over
	// the one on the command line meant that where two keys ever shared a
	// directory, the run worked, commented on, merged and proposed the FIRST
	// ticket while the one asked for was never touched — and said so in its own
	// output, every night. tktSlug is injective now, so this cannot be reached by
	// two live keys; it stays because a state directory moved, copied or renamed
	// by hand must not quietly redirect a cron line.
	if st.Ticket != "" && key != "" && st.Ticket != key {
		return usageErrf("%s holds the state of ticket %s, and this run was asked for %s — lca will not work one ticket under another's record. Move that directory aside, or pass %s.",
			filepath.Join(dir, "state.json"), st.Ticket, key, st.Ticket)
	}
	if st.Ticket == "" {
		st.Ticket = key
	}
	st.Root, st.Rounds = root, r.pc.pipelineRounds()
	st.Target, st.Remote = r.pc.Target, r.pc.Remote
	st.LCAVersion, st.RolesHash = lcaVersion(), rolesHash(r.orch.roles)
	st.Skills = r.skills
	if st.Ticket != "" && st.Branch == "" {
		st.Branch = tktBranchFor(r.pc, st.Ticket)
	}
	if !contains(st.Runs, r.runID) {
		st.Runs = append(st.Runs, r.runID)
		st.Traces = append(st.Traces, r.orch.tracer.Path)
	}
	r.st = st
	return nil
}

// ── running it ──────────────────────────────────────────────────────────────

// execute walks the machine and then reports, whatever happened. The report is
// not conditional on success: an unattended pipeline that goes quiet is worse
// than one that fails loudly, so a blocked run comments on the ticket saying
// what is blocked before it exits.
func (r *tktRun) execute() (status, reason string) {
	// Last night's blocked sentence belongs to last night's run. It is cleared
	// here, where a run begins, and not in openState, because the marker the
	// report is found by is derived from it: a run that carried yesterday's
	// `blocked` into today would look for yesterday's comment, find it, and say
	// nothing about the work it has just finished.
	r.st.Blocked, r.st.BlockedAt, r.st.Status = "", "", ""
	blocked, at, err := r.walk()
	switch {
	case err != nil:
		status, reason = classifyRunErr(err)
		// The error path records a blocked sentence too, and that is not cosmetic.
		// Everything downstream reads it: the status the ticket is moved to, the
		// `blocked=` flag in the comment's marker, whether `reported` may be written
		// over the state. With it empty, a forge outage and a coder that produced
		// nothing both reported "ready for review" on a board where no merge request
		// existed — a run that failed loudly on stdout, reporting success on the
		// tracker, where nobody looks at the blocked column again.
		r.st.Blocked, r.st.BlockedAt = firstNonEmpty(reason, err.Error()), r.st.State
	case blocked != "":
		status, reason = statusFailed, blocked
		r.st.Blocked, r.st.BlockedAt = blocked, at
	default:
		status, reason = statusPassed, ""
	}
	// A spent budget outranks the shape of the stop it caused, which is statusOf's
	// own rule and its own reason: the budget ends a run by CANCELLING its
	// context, so by the time anything reports, every symptom reads as an
	// interrupt. The two go to different queues — 130 says "the operator stopped
	// it" and sends the ticket back to be retried for ever, while 4 says "it did
	// not fit" and sends it to a person — and this command documents the one-shot
	// table, so it has to mean the same thing by it. Only a completed walk
	// outranks the budget: work that was done and verified before the clock ran
	// out was done.
	if status != statusPassed {
		if why := r.tripped(); why != "" {
			status, reason = statusBudget, why
			if r.st.Blocked == "" {
				r.st.Blocked, r.st.BlockedAt = why, r.st.State
			}
		}
	}
	r.st.Status = status
	r.save()

	// The comment, now. A run that could not even be configured never got here; a
	// run whose gateway is down cannot buy the sentence, and says so.
	//
	// Except when there is nothing to comment ON. A -new run whose create call
	// failed has no key, and neither the probe nor the transition used to guard on
	// that: it spent a model stage and then made three tracker calls with an empty
	// ticket — a read, a comment, and a status move — every one of them a write
	// aimed at no identified ticket, which a server that is lenient about a
	// missing key resolves somewhere nobody asked.
	if r.st.Ticket == "" {
		r.st.record(TicketStep{Name: tktReport, From: r.st.State, Round: r.st.Round,
			Status: tktSkipped, Detail: "there is no ticket to comment on", Run: r.runID,
			Started: nowTS(), Finished: nowTS()})
		r.save()
		return status, reason
	}
	if rerr := r.reportOut(status); rerr != nil {
		if status == statusPassed {
			// The work is merged and the merge request is open; a tracker that is down
			// must not turn that into a failure. It must not be silent either, so the
			// reason carries it and the journal has the error.
			reason = "the work is done and the ticket could not be told: " + rerr.Error()
			warnLine("%s", reason)
		} else if reason == "" {
			reason = rerr.Error()
		}
	}
	return status, reason
}

// tripped is the run's budget, asked safely: the machine's own tests stand a run
// up with no orchestrator at all, and a nil one has no budget to be over.
func (r *tktRun) tripped() string {
	if r.orch == nil {
		return ""
	}
	return r.orch.budget.tripped()
}

// reportOut runs the report transition. It is the one transition the walk does
// not take, because it is reached from both ends: the finished run and the
// blocked one.
//
// A blocked run's report must not advance the state — the answer to "where does
// this work stand" is `implemented` or `reviewed`, and a `reported` written over
// it would hide exactly the thing a person is about to read. So the transition
// is entered with its To cleared.
func (r *tktRun) reportOut(status string) error {
	tr := *r.spine[r.index(tktReport)]
	// Only a run that walked the WHOLE arc may record `reported`. The old test was
	// `st.Blocked != ""`, which is empty on every error path — so an errored run
	// wrote `reported` over `opened`, and auditState then refused that document
	// for ever ("no green check is recorded under it"), which is a correct audit
	// of a file lca itself corrupted. Three runs and the ticket was unresumable.
	if status != statusPassed {
		tr.To = ""
	}
	// And its own clock. The run's context is already cancelled when the budget
	// ran out or a signal arrived, and this is the one transition that still has
	// to happen: the commonest unattended failure there is — the night's time
	// running out — otherwise ended with no comment on the ticket and its status
	// untouched, which is the silence this whole transition exists to prevent. The
	// closing deadline is the same bound the summary's equivalent problem uses, so
	// a report cannot outlive the promise the budget made either.
	ctx, cancel := r.reportCtx()
	defer cancel()
	prev := r.ctx
	r.ctx = ctx
	defer func() { r.ctx = prev }()
	return r.enter(&tr)
}

func (r *tktRun) reportCtx() (context.Context, context.CancelFunc) {
	base := r.ctx
	if base == nil {
		base = context.Background()
	}
	if base.Err() == nil {
		return base, func() {}
	}
	var b *runBudget
	if r.orch != nil {
		b = r.orch.budget
	}
	return context.WithDeadline(context.Background(), b.closingDeadline(time.Now()))
}

// result is the object -json writes and the text outcome is printed from. One
// builder, so the two can never disagree about what happened.
func (r *tktRun) result(status, reason string, started time.Time) ticketResult {
	res := ticketResult{
		Status: status, Reason: forPublication(reason),
		Ticket: r.st.Ticket, Summary: forPublication(r.st.Summary),
		State: r.st.State, Blocked: forPublication(r.st.Blocked), BlockedAt: r.st.BlockedAt,
		Round: r.st.Round, Rounds: r.st.Rounds,
		Branch: r.st.Branch, Target: r.st.Target, Remote: r.st.Remote,
		Head: r.st.Head, Pushed: r.st.PushedSha, Worktree: r.st.Worktree,
		// forPublication on everything that came from OUTSIDE this program, for
		// runResult's reason and with the same words: a check.sh that echoes its
		// environment on failure, or a test that prints the failing request with its
		// Authorization header, puts a live token in the tail — and these are the
		// fields the wrapper pastes into a public merge request and a ticket comment.
		// The blocked sentence is composed from the tail, so it goes through too.
		CheckCmd: r.st.Check.Cmd, CheckExit: r.st.Check.Exit,
		CheckTail: forPublication(r.st.Check.Tail), CheckLogs: r.st.Check.Logs,
		Review: r.st.Review, MergeRequest: r.st.MergeRequest, CommentID: r.st.CommentID,
		Skills:      r.st.Skills,
		Transitions: r.st.Journal,
		StateFile:   filepath.Join(r.dir, "state.json"),
		Session:     r.st.Coder,
		StartedAt:   traceTS(started), FinishedAt: nowTS(),
		DurationMs: time.Since(started).Milliseconds(),
		LCAVersion: lcaVersion(), RolesHash: r.st.RolesHash,
	}
	if res.CheckLogs == nil {
		res.CheckLogs = []string{}
	}
	if res.Skills == nil {
		res.Skills = []TicketSkill{}
	}
	if res.Transitions == nil {
		res.Transitions = []TicketStep{}
	}
	// The journal's own prose is composed from the same places — a gate's sentence
	// is the check's tail, a failure's detail is an error from a server — and it is
	// in the object a wrapper reads and quotes.
	res.Transitions = append([]TicketStep(nil), res.Transitions...)
	for i := range res.Transitions {
		res.Transitions[i].Detail = forPublication(res.Transitions[i].Detail)
		res.Transitions[i].Evidence = forPublication(res.Transitions[i].Evidence)
	}
	if r.orch != nil {
		res.Transcript = transcriptPath(r.cfg, r.orch.rec.id)
		res.Trace = r.orch.tracer.Path
	}
	return res
}

// ── what it prints ──────────────────────────────────────────────────────────

// printPlan is -dry-run: every transition it WOULD make, with the gate each one
// waits on and the skills each stage would load. It touches no tracker, buys no
// model and writes no state — but it does resolve the configuration and the
// skills, so the two mistakes that stop a real run (a missing key, a missing
// skill) are the two that stop this one.
func (r *tktRun) printPlan() {
	facts := [][]string{
		{"ticket", firstNonEmpty(r.st.Ticket, "-new "+quoteShort(r.newTask))},
		{"branch", firstNonEmpty(r.st.Branch, tktBranchFor(r.pc, "<key>"))},
		{"target", r.pc.Target},
		{"remote", pipeOrNone(r.pc.Remote)},
		{"push", fmt.Sprintf("%t", r.pc.pipelinePush())},
		{"rework rounds", fmt.Sprintf("%d", r.pc.pipelineRounds())},
		{"roles", fmt.Sprintf("coder %s"+gSep+"reviewer %s"+gSep+"integrator %s", r.pc.Coder, r.pc.Reviewer, r.pc.Integrator)},
		{"state", filepath.Join(r.dir, "state.json")},
		{"at", firstNonEmpty(r.st.State, "nothing recorded yet")},
	}
	sectionTable("ticket", r.pc.Src, []string{"", ""}, facts)
	// Next to the target it is about, and in MergeIn's own words, because it is
	// MergeIn's refusal arriving early: this run would code, check and review and
	// then stop at the merge. Said as a line and not as a cell, because the
	// remedy is the half that matters and a cell is where a table clips.
	if r.targetHeldBy != "" {
		errLine("%s is checked out in %s, so lca will not move it — this run would code, check and review and then refuse at the merge. Park that checkout somewhere else (`git -C %s switch --detach`), or point pipeline: target_branch at a branch nobody has checked out.",
			r.pc.Target, r.targetHeldBy, r.targetHeldBy)
	}

	rows := [][]string{}
	for i, tr := range r.spine {
		res, _ := gateOf(tr, r)
		mark := "would"
		switch {
		case r.st.did(tr.Name):
			mark = "done"
		case res == gateSkip:
			mark = "skip"
		case res == gateBlocked:
			mark = "waits"
		}
		// stageOf and not tr.Stage: with a ticket key the open transition reads the
		// tracker and buys nothing, so row 1's "writes" and "skills" columns have to
		// say so — the CALLS section below already hides `open one (-new)`.
		stage := r.stageOf(tr)
		rows = append(rows, []string{fmt.Sprintf("%d", i+1), tr.Name, tr.To, mark,
			firstNonEmpty(stage, "—"), tktSkillList(r.skills, stage)})
	}
	sectionTable("transitions", "lca performs every one of these; a model only writes content",
		[]string{"#", "transition", "state", "", "writes", "skills"}, rows, 2)

	// The gate sentences go under the table and not in it, printed whole: they are
	// the point of the whole feature, and a column would middle-ellipsize the half
	// that says what is waited on.
	section("what each transition does, and what it waits on")
	for i, tr := range r.spine {
		_, why := gateOf(tr, r)
		gate := tr.Gate
		if gate == "" {
			gate = "nothing"
		}
		if tr.Why != "" {
			gate += " (" + tr.Why + ")"
		}
		if why != "" {
			gate += gSep + why
		}
		fmt.Printf("  %s %-10s %s\n", faint("%d", i+1), tr.Name, faint("%s", tr.Does))
		fmt.Printf("    %s %s\n", faint("waits on"), gate)
	}

	if len(r.skills) == 0 {
		section("skills in force")
		fmt.Println("  " + faint("none named — pipeline: skills: gives a stage the team's instructions by name"))
	} else {
		srows := [][]string{}
		for _, s := range r.skills {
			srows = append(srows, []string{s.Stage, s.Name, shortSha(s.Sum), fmt.Sprintf("%d", s.Files), shortDir(s.Path)})
		}
		sectionTable("skills in force", "recorded by name and content hash, so two runs of one ticket are comparable",
			[]string{"stage", "skill", "sha256", "files", "from"}, srows)
	}
	// The calls, with the arguments substituted. This is the one thing an operator
	// should read before the first night: every one of these names is theirs, and
	// a plan that printed only the tool names would hide the half that actually
	// goes over the wire. The values are shown with THIS ticket's key and branch
	// in them, because "${key}" is not what anybody needs to check.
	// Printed as lines and not as a table, for the reason the gate sentences are:
	// an argument list is the widest thing on the page and a column would
	// middle-ellipsize exactly the half that says what is being sent.
	section("the calls lca makes")
	fmt.Println("  " + faint("your tool names, your argument names, and nothing lca composed"))
	for _, c := range r.callPlan() {
		fmt.Printf("  %-22s %s\n", c[0], c[1])
		fmt.Printf("    %s\n", faint("%s", c[2]))
	}

	if !r.pc.pipelinePush() {
		hint("pipeline: push is false, so this run would finish at %s: nothing is pushed and no merge request is opened", tktMerged)
	}
}

// callPlan is every configured call this invocation could make, with the
// placeholders filled in from the state this ticket is actually in. One row per
// call, in the order the arc reaches them.
func (r *tktRun) callPlan() [][3]string {
	key := firstNonEmpty(r.st.Ticket, "<key>")
	branch := firstNonEmpty(r.st.Branch, tktBranchFor(r.pc, key))
	vals := map[string]string{
		"key": key, "project": r.pc.Project, "branch": branch, "target": r.pc.Target,
		"status": "<status>", "text": "<the comment>", "title": "<the title>",
		"body": "<the body>", "summary": firstNonEmpty(r.st.Summary, "<the summary>"),
	}
	type row struct {
		label, tool, where, verb string
		shown                    bool
	}
	rows := []row{
		{"read the ticket", r.pc.Tracker.Read, "tracker", tktVerbRead, true},
		{"open one (-new)", r.pc.Tracker.Create, "tracker", tktVerbCreate, r.newTask != ""},
		{"find the merge request", r.pc.Forge.FindMR, "forge", tktVerbFindMR, r.pc.pipelinePush()},
		{"open the merge request", r.pc.Forge.CreateMR, "forge", tktVerbCreateMR, r.pc.pipelinePush()},
		{"comment on the ticket", r.pc.Tracker.Comment, "tracker", tktVerbComment, true},
		{"move its status", r.pc.Tracker.Transition, "tracker", tktVerbMove, r.pc.Tracker.Transition != ""},
	}
	var out [][3]string
	for _, rw := range rows {
		if !rw.shown {
			continue
		}
		args := r.pc.Args(rw.where)
		if !args.has(rw.verb) {
			out = append(out, [3]string{rw.label, orNone(rw.tool), faint("no args: configured")})
			continue
		}
		var parts []string
		for _, name := range args.Order[rw.verb] {
			parts = append(parts, fmt.Sprintf("%s=%v", name, expandCall(args.of(rw.verb), []string{name}, vals)[name]))
		}
		out = append(out, [3]string{rw.label, rw.tool, strings.Join(parts, " ")})
	}
	return out
}

// printOutcome is what a person reads when -json was not asked for.
func (r *tktRun) printOutcome(res ticketResult) {
	rows := [][]string{}
	for _, s := range res.Transitions {
		detail := s.Detail
		if s.Status == tktAlready {
			detail = s.Evidence
		}
		rows = append(rows, []string{s.Name, s.To, s.Status, detail})
	}
	sectionTable("ticket "+res.Ticket, res.Branch, []string{"transition", "state", "", "why"}, rows)
	switch res.Status {
	case statusPassed:
		okLine("%s: %s", res.Ticket, firstNonEmpty(res.Reason, "reached "+res.State))
	default:
		errLine("%s stopped at %s: %s", res.Ticket, firstNonEmpty(res.BlockedAt, res.State), res.Reason)
	}
	hint("state: %s", filepath.Join(r.dir, "state.json"))
}

// printTickets lists what has state on this machine, newest first.
func printTickets(cfg Config) {
	ents, err := os.ReadDir(ticketsDir(cfg))
	if err != nil || len(ents) == 0 {
		section("tickets")
		fmt.Println("  " + faint("none yet"))
		return
	}
	var sts []*TicketState
	var warns []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		st, err := loadTicketState(filepath.Join(ticketsDir(cfg), e.Name()))
		if err != nil {
			if !os.IsNotExist(err) {
				warns = append(warns, err.Error())
			}
			continue
		}
		sts = append(sts, st)
	}
	sort.Slice(sts, func(i, j int) bool { return sts[i].Updated > sts[j].Updated })
	rows := [][]string{}
	for _, st := range sts {
		rows = append(rows, []string{st.Ticket, firstNonEmpty(st.State, "—"), firstNonEmpty(st.Status, "—"),
			fmt.Sprintf("%d/%d", st.Round, st.Rounds), st.Updated})
	}
	sectionTable("tickets", "", []string{"ticket", "state", "status", "round", "updated"}, rows)
	for _, w := range warns {
		warnLine("%s", w)
	}
	hint("%s", "continue one: lca ticket <key>"+gSep+"see the plan: lca ticket <key> -dry-run")
}

// tktSkillList is the stage's skills as -dry-run prints them: the names in the
// order the block gave them, because that order is the order the instructions
// reach the model.
func tktSkillList(all []TicketSkill, stage string) string {
	if stage == "" {
		return "—"
	}
	ss := skillsFor(all, stage)
	if len(ss) == 0 {
		return faint("none")
	}
	names := make([]string, 0, len(ss))
	for _, s := range ss {
		names = append(names, s.Name)
	}
	return strings.Join(names, " ")
}

func pipeOrNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// quoteShort is a -new task as one readable cell: the first line, clipped, so a
// fifty-line task description does not become the plan's widest column.
func quoteShort(s string) string {
	s = firstLine(strings.TrimSpace(s))
	if len(s) > 60 {
		s = s[:57] + "…"
	}
	return fmt.Sprintf("%q", s)
}

// pipelineOf is roles.yaml's pipeline: block, or nil — which validate turns into
// the error that explains what the block is for. A method so a nil RolesConfig
// (no roles.yaml at all) answers the same way a roles.yaml with no block does.
func (rc *RolesConfig) pipelineOf() *PipelineConfig {
	if rc == nil {
		return nil
	}
	return rc.Pipeline
}
