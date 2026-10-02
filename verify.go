package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The verifier decides whether a task is done — not the model. A session runs
// until the model stops calling tools; then the task's check_cmd runs in the
// sandbox (allowlist + GPU policy) in the session's working tree. Exit 0 is
// "passed". Otherwise the failing output goes back to the same session as a
// user message and it runs again, up to verify_attempts; then "failed". With
// no check_cmd the result is "unverified": nothing claims it's done.

type Verdict struct {
	Status   string // passed | failed | unverified | error | cancelled
	Checked  bool
	Exit     int
	Tail     string // last lines of the check output
	Attempts int
	Err      error

	// CheckLogs are the files holding the FULL output of every check this run
	// ran, in the order they ran (checktail.go's CheckLog). The tail above is a
	// selection and says so; these are the bytes it was selected from, which for
	// a stand is the only copy of twenty thousand lines that will exist once the
	// process is gone. Empty for a run whose check printed nothing, and for every
	// run that had no check.
	CheckLogs []string

	// Review is the structured verdict a -diff-base run asked the reviewer for,
	// and ReviewErr is why there is none (review.go). They live on the verdict
	// because statusOf decides from them: a review that could not be read is a
	// failed run, and a review that could is the thing a reviewer run was for.
	// Both are zero for every run that did not ask for one, which is every run
	// that existed before.
	Review    *reviewReport
	ReviewErr string
}

const (
	tailLines = 60
	tailBytes = 4000
)

// lastLines keeps the end of command output, where test failures and
// summaries are.
func lastLines(s string, n, maxBytes int) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = "…" + out[len(out)-maxBytes:]
	}
	return out
}

func (o *Orchestrator) verifyAttempts() int {
	if o.roles != nil && o.roles.VerifyAttempts > 0 {
		return o.roles.VerifyAttempts
	}
	return 2
}

// checkTimeout is the team-wide ceiling on ONE check: defaults: check_timeout:,
// else ten minutes. It is what a workflow run step inherits when its author
// named no timeout of its own.
func (o *Orchestrator) checkTimeout() time.Duration {
	if o.roles != nil && o.roles.CheckTimeout > 0 {
		return time.Duration(o.roles.CheckTimeout) * time.Second
	}
	return 10 * time.Minute
}

// checkTimeoutOf is the ceiling for a check run by one ROLE's session, and it
// exists because the default is ten minutes while their real check is a stand:
// build, deploy, check.sh — five to fifteen minutes before anything is known.
// Raising defaults: check_timeout for the whole team to cover the one role that
// talks to the stand also gives every cheap `go test` role an hour to hang in,
// which is why `check_timeout` is now a role's own key too (roles.go, capped at
// an hour).
//
// a == nil is the team-wide answer, so a caller with no role in hand keeps the
// behaviour it had.
func (o *Orchestrator) checkTimeoutOf(a *Agent) time.Duration {
	if a != nil && a.CheckTimeout > 0 {
		return time.Duration(a.CheckTimeout) * time.Second
	}
	return o.checkTimeout()
}

// maxCheckTimeout is the ceiling on one check: an hour, which is the figure the
// requirement names ("check_timeout on a role, up to an hour"). It is a real
// bound and not advice, because this timeout is the only thing that ends a check
// that hangs — a stand that stopped answering, an ssh that never returns — and
// an unattended run with no ceiling on it holds its worktree and its ticket
// until somebody notices. A run that needs longer than an hour of ONE command is
// not a check; -timeout bounds the whole run and is where that belongs.
const maxCheckTimeout = 3600

// parseCheckTimeout reads a check_timeout value: a bare number of seconds, as
// the key has always been written, or a duration (`25m`), which is how the
// operator thinks about a fifteen-minute stand run. what names the key for the
// error, because the same value appears under defaults: and on a role.
func parseCheckTimeout(what, v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil // nobody said: the team default stands
	}
	secs := 0
	if n, err := strconv.Atoi(v); err == nil {
		secs = n
	} else if d, err := time.ParseDuration(v); err == nil {
		secs = int(d / time.Second)
	} else {
		return 0, fmt.Errorf("%s: %q is not a number of seconds or a duration (900, 15m)", what, v)
	}
	switch {
	case secs <= 0:
		return 0, fmt.Errorf("%s: %q is not a positive time — a check needs a bound, and `0` would kill it the moment it started", what, v)
	case secs > maxCheckTimeout:
		return 0, fmt.Errorf("%s: %q is over the one-hour ceiling on a single check — use -timeout (or defaults: timeout:) to give the whole run longer", what, v)
	}
	return secs, nil
}

// RunVerified drives s (its task message already appended) to a verdict.
func (s *Session) RunVerified(ctx context.Context, check string, attempts int) Verdict {
	if check == "" {
		return s.RunVerifiedAll(ctx, nil, attempts)
	}
	return s.RunVerifiedAll(ctx, []string{check}, attempts)
}

// RunVerifiedAll is RunVerified with several checks, all of which must pass
// (run in order; the first failure is the one reported).
func (s *Session) RunVerifiedAll(ctx context.Context, checks []string, attempts int) Verdict {
	if attempts < 1 {
		attempts = 1
	}
	v := Verdict{}
	// A check the sandbox would refuse is an error up front, not after a whole
	// agent run spent attempts on it.
	for _, check := range checks {
		if err := s.checkCmd(check); err != nil {
			// Typed: a check the sandbox will never run is a MISTAKE IN THE CALL, not
			// a result. Retrying it tonight changes nothing and no model failed, so the
			// pipeline's table gives it 2 — alert somebody, leave the ticket alone.
			v.Err = usageErrf("check refused before it ran: %v", err)
			v.Status, v.Tail = "error", v.Err.Error()
			return v
		}
	}
	// An unreachable member is an ERROR, not a failed check: feeding an ssh
	// failure back to a model as "your change did not pass" asks it to fix
	// something it cannot reach.
	if len(checks) > 0 {
		if err := s.memberOf().reach(ctx); err != nil {
			// Typed, for the same reason the message says it: this is the machine
			// being away, so the result is infra_error and the ticket goes back in the
			// queue instead of to a person.
			v.Err = &infraErr{what: "member " + s.memberName(), err: err}
			v.Status, v.Tail = "error", err.Error()
			return v
		}
	}
	for attempt := 1; ; attempt++ {
		v.Attempts = attempt
		err := s.Run(ctx)
		switch {
		case err == context.Canceled || ctx.Err() != nil:
			v.Status, v.Err = "cancelled", err
			return v
		case err != nil:
			v.Status, v.Err, v.Tail = "error", err, err.Error()
			return v
		}
		// Under `apply: branch` the ENGINE commits what this attempt produced, here
		// and nowhere else: one commit per attempt, before the check runs, so a
		// failed delegation's branch still shows what it tried. No new tool, so the
		// request prefix does not move and a role cannot forget to commit.
		//
		// A commit that fails is an ERROR and not a warning, because under this mode
		// the BRANCH is the deliverable. Warned about and carried on, the sequence
		// is: the branch stays at the snapshot, the check passes, the diff is
		// non-empty so the integration runs, the merge of an empty branch produces a
		// 0-byte patch, and the caller is handed `status: passed` with the words
		// "already in your working tree" while the worktree holding the only copy of
		// the work is deleted by `defer wt.remove()`. Measured against a project with
		// commit.gpgsign on: nothing is written and the loss is reported as a
		// success. In an unattended run nobody reads a warnLine.
		//
		// A session without a branch — every session under `apply: verified`, which
		// is the default — returns nil from commitWork immediately, so nothing here
		// changes for them.
		if cerr := s.commitWork(ctx, s.wt, attempt); cerr != nil {
			v.Status, v.Err = "error", cerr
			v.Tail = fmt.Sprintf("committing %s's attempt %d onto %s failed, so the branch does not hold the work and NOTHING was merged: %s",
				s.agent.Name, attempt, s.branch, cerr)
			return v
		}
		if len(checks) == 0 {
			v.Status = "unverified"
			return v
		}

		// A check this attempt rewrote is refused before it runs, not after. The
		// up-front sandbox pass above happens before the model has touched
		// anything, so this is the only place that can see it: the model edits the
		// tree, and under `-check "$WORKTREE/check.sh"` the script it runs lives in
		// that same tree. Typed as a mistake in the call — exit 2, leave the ticket
		// alone — because no model failed and retrying tonight changes nothing.
		for _, check := range checks {
			if f := s.rewroteCheck(check); f != "" {
				v.Err = usageErrf("check %q runs %s, which this run changed — a verifier the run rewrote proves nothing; point -check at a copy outside the tree the agent edits", check, f)
				v.Status, v.Tail = "error", v.Err.Error()
				return v
			}
		}

		var check, out, lastLog string
		exit := 0
		for i, c := range checks {
			check = c
			cstart := time.Now()
			out, exit = s.runCheck(ctx, check, s.orch.checkTimeoutOf(s.agent), s.checkLive)
			// NOT scrubbed here, and that is the whole of it: this string is what the
			// model is about to be asked to fix. The scrub reads the environment and
			// four very common words, so a run with API_KEY_HEADER=Authorization set
			// turned `request rejected: missing Authorization header` into `missing
			// [redacted] header` and then spent every attempt asking the model to fix
			// a cause it had been shown a hole where. redact.go says this in so many
			// words — a write boundary and not the source — and this was a source.
			//
			// Every sink downstream scrubs at its own boundary instead: the recorder
			// and the tracer on the bytes of each record, CheckLog on the file it
			// writes, resultOf on check_tail, writeSummary on the facts. The omission
			// markers still index the full-output file across that difference, because
			// [redacted] carries no newline: the two copies differ in bytes and agree,
			// line for line, on what line a line is.
			s.stats.VerifyRuns++
			s.view.Check(check, exit, time.Since(cstart), attempt, attempts)
			// The whole output of this attempt, beside the transcript. Written for
			// every attempt and not only the failing one, because the question a
			// stand run leaves behind — "was this already broken last time?" — can
			// only be answered by the attempt before. A write that fails is a
			// warning and not a verdict: the check's own result stands, and the
			// wrapper reads an empty check_logs rather than a path to nothing.
			logPath, lerr := s.orch.rec.CheckLog(s.UID, attempt, i, out)
			if lerr != nil {
				s.view.Warn(fmt.Sprintf("the check's full output could not be saved: %v", lerr))
			} else if logPath != "" {
				v.CheckLogs = append(v.CheckLogs, logPath)
				lastLog = logPath
			}
			s.event("verify", map[string]any{"check": check, "member": s.memberName(), "exit": exit, "attempt": attempt,
				"bytes": len(out), "log": logPath})
			if exit != 0 {
				// A check that "failed" because the machine went away mid-run is not
				// a failing check: ssh's own error fed back as "your change did not
				// pass" asks the model to fix a VPN, and burns every attempt doing
				// it. The transport error already dropped the gate, so this asks
				// once, and only after something has already failed.
				if rerr := s.memberOf().reach(ctx); rerr != nil {
					v.Err = &infraErr{what: "member " + s.memberName(), err: rerr}
					v.Status, v.Tail = "error", rerr.Error()
					return v
				}
				break
			}
		}
		// checkTailOf and not lastLines: on a stand the last sixty lines are the
		// harness's own epilogue ("3 of 412 failed, see above") and the line that
		// says WHAT failed is eight thousand lines up. Short output still arrives
		// whole and unmarked — see checktail.go.
		v.Checked, v.Exit, v.Tail = true, exit, checkTailOf(out)
		if exit == 0 {
			v.Status = "passed"
			return v
		}
		if ctx.Err() != nil {
			v.Status = "cancelled"
			return v
		}
		if attempt >= attempts {
			v.Status = "failed"
			return v
		}
		// A spent budget is the same thing as "that was the last attempt". The
		// token and step ceilings deliberately stop the MODEL and let the verifier
		// finish — one more check, because a run whose last reply said "done" and
		// went one token over must still have its check run. One. Session.Run returns
		// nil immediately once the budget is over, so without this the loop went on
		// committing, re-running the identical check against a byte-identical tree
		// and appending "the verifier ran … and it FAILED" to a conversation nobody
		// will ever answer, for all the remaining attempts: with the pipeline
		// profile's check_timeout of 1800 and verify_attempts of 3, up to an hour of
		// stand builds and deploys bought after the run had already decided to stop.
		if why := s.orch.budget.over(); why != "" {
			v.Status = "failed"
			v.Tail = strings.TrimSpace(v.Tail + "\n\n(no attempt left: " + why + ")")
			return v
		}
		s.Msgs = append(s.Msgs, Message{Role: "user", Content: fmt.Sprintf(
			"The verifier ran `%s` and it FAILED (exit %d). The task is done only when this check passes — your own judgement doesn't count. Fix the cause, then stop.\n\nOutput — the lines that name a failure, with their context and the end of the log; anything cut out is marked:\n```\n%s\n```%s", check, exit, v.Tail, s.fullOutputNote(lastLog))})
	}
}

// fullOutputNote tells the model where the whole output of this attempt is —
// and only when it can actually get at it.
//
// The selection above says when it left something out, and the obvious remedy is
// to go and read the file it was selected from. But that file is under $LCA_DIR,
// which defaults to $HOME/.lca while the sandbox confines the model to the
// worktree, so `read_file` and `grep` on it are refused: a run with LCA_DIR
// outside the tree would be promising a remedy that does not exist and spending
// the attempt on discovering that. The jail is asked rather than guessed at, so
// a pipeline that puts its state dir inside the worktree gets the sentence and
// one that does not gets silence, which is the truth in both cases. The path is
// in check_logs either way, for the wrapper and for the operator.
func (s *Session) fullOutputNote(path string) string {
	if path == "" || s.orch == nil || s.orch.jl == nil {
		return ""
	}
	if _, err := s.orch.jl.Resolve(path); err != nil {
		return ""
	}
	return fmt.Sprintf("\n\nThe whole output of this attempt is in %s — the line numbers in the markers above are its line numbers, so `grep -n error %s` finds what was cut.", path, path)
}
