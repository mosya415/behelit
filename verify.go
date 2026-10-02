package main

import (
	"context"
	"fmt"
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

func (o *Orchestrator) checkTimeout() time.Duration {
	if o.roles != nil && o.roles.CheckTimeout > 0 {
		return time.Duration(o.roles.CheckTimeout) * time.Second
	}
	return 10 * time.Minute
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

		var check, out string
		exit := 0
		for _, check = range checks {
			cstart := time.Now()
			out, exit = s.runCheck(ctx, check, s.orch.checkTimeout(), s.checkLive)
			s.stats.VerifyRuns++
			s.view.Check(check, exit, time.Since(cstart), attempt, attempts)
			s.event("verify", map[string]any{"check": check, "member": s.memberName(), "exit": exit, "attempt": attempt})
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
		v.Checked, v.Exit, v.Tail = true, exit, lastLines(out, tailLines, tailBytes)
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
			"The verifier ran `%s` and it FAILED (exit %d). The task is done only when this check passes — your own judgement doesn't count. Fix the cause, then stop.\n\nLast lines of output:\n```\n%s\n```", check, exit, v.Tail)})
	}
}
