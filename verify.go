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
		if err := s.jail().CheckCommand(check); err != nil {
			v.Status, v.Tail = "error", "check rejected by the sandbox: "+err.Error()
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
		case len(checks) == 0:
			v.Status = "unverified"
			return v
		}

		var check, out string
		exit := 0
		for _, check = range checks {
			cstart := time.Now()
			if rem := s.remote(); rem != nil {
				out, exit = rem.run(ctx, check, s.orch.checkTimeout(), nil, s.checkLive)
			} else {
				out, exit = execCheck(ctx, s.jail(), check, s.orch.checkTimeout(), s.checkLive)
			}
			s.stats.VerifyRuns++
			s.view.Check(check, exit, time.Since(cstart), attempt, attempts)
			s.event("verify", map[string]any{"check": check, "exit": exit, "attempt": attempt})
			if exit != 0 {
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
		s.Msgs = append(s.Msgs, Message{Role: "user", Content: fmt.Sprintf(
			"The verifier ran `%s` and it FAILED (exit %d). The task is done only when this check passes — your own judgement doesn't count. Fix the cause, then stop.\n\nLast lines of output:\n```\n%s\n```", check, exit, v.Tail)})
	}
}
