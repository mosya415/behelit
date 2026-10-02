package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// What a whole RUN is allowed to spend, and the one thing all three limits have
// to do when it is spent: stop gracefully.
//
// Until now the only cap was per-agent (`steps`) and per-check (`check_timeout`),
// which together bound nothing: an agent that reads the same three files in a
// loop ends every turn inside its step budget, the verifier feeds it the same
// red check, and the run keeps buying gateway tokens until somebody looks at the
// GPU queue in the morning. A cron job cannot look.
//
// So a run carries three ceilings — wall clock, steps and tokens — and all three
// are REPORTED the same way, because that is the wrapper's whole input: the
// transcript is saved, the status is budget_exceeded, the exit code is 4 ("needs
// a human, and tell them it did not fit").
//
// They do not all stop the same things, and that difference is deliberate. The
// clock cancels the run's context, because when the time is gone there is
// nothing left to run a check in. The step and token ceilings stop the MODEL and
// let the verifier finish — the verifier costs no tokens, and a run whose last
// reply said "done" and went one token over must still have its check run, or a
// finished ticket is reported as "did not fit" and goes to a person for nothing.
//
// What is common is the context. Almost everything a run starts hangs off it —
// the check command's process group, a background subagent and whatever it
// launched — so the deferred cancel at the end of oneShot reaps all of those
// however the run ended, and a spent clock reaps them early. That is what makes
// "no child left behind" true by construction rather than by a list somebody has
// to remember to extend: a check command is a process group with a cargo build
// and three linkers in it, and leaking it leaves work on a machine with nothing
// holding a reference to it.
//
// The one exception, stated because the next person will rely on this paragraph:
// a stdio MCP server does NOT hang off the run context. MCPSet roots its own
// context at context.Background() (mcp.go), so each child outlives a cancelled
// run and is reaped only by an explicit orch.CloseMCP() — which main.go, eval.go
// and workflow.go each call on every exit path they have. A third caller of
// oneShot, or an early return added before that line, leaks a server process and
// its session with nothing left holding a reference to it. Adding a path means
// adding the call.
type runBudget struct {
	// The limits as resolved: roles.yaml's defaults:, with the flag on top. Zero
	// means "no ceiling of this kind", which is what a run in a terminal gets.
	timeout   time.Duration
	maxSteps  int
	maxTokens int

	mu       sync.Mutex
	deadline time.Time // zero until start(), and zero forever with no timeout
	// The run's spend, every session in it: the primary, the compactor, every
	// subagent and the closing summary call. Split three ways because the result
	// object reports it that way and a single total cannot be unsplit — and
	// reported from HERE and not from one session's stats, which is the number a
	// `reason` of "1.1k of 900 tokens" used to sit next to a `tokens` of 220.
	prompt, completion, cached int
	steps_                     int    // model requests made by the whole run
	reason                     string // why the run must stop; "" while it may continue
	// armed says a RUN is in progress. The ceilings are a property of the run and
	// not of the process: start() arms them and only a one-shot, an eval task and
	// a workflow call it. Without this a `defaults: max_tokens` in roles.yaml
	// wedged an interactive session permanently — once the day's conversation
	// crossed the ceiling, every later prompt was refused before it reached the
	// gateway, /reset did not clear it and no command could, because there was
	// nothing in the REPL that had ever meant to arm a run-wide ceiling.
	armed bool
}

// newRunBudget resolves the three ceilings. The flag wins over the file for each
// one independently: an operator overriding the timeout for one ticket must not
// silently lose the team's token ceiling with it.
//
// A negative value is a usage error and not a clamp — "-timeout -5m" is a
// wrapper building its command line wrong, and running forever is the one
// interpretation that cannot be right.
// stepsUnlimited is a step ceiling the operator removed on purpose. It is a
// sentinel and not 0, because 0 has always meant "nobody said" and a run in a
// terminal gets it: the two have to stay tellable apart, or leaving the flag
// out would silently uncap every role.
//
// The step cap was always a proxy for the thing actually worth bounding — an
// agent grinding tokens with nothing to show — and as a proxy it cut long
// legitimate work short: in loop mode a turn spends a step per reply, so 50 is
// gone before a real task is half done. Now that a run has a clock and a token
// ceiling, the proxy can be switched off and the real bound used instead, which
// is why removeing the step cap in an unattended run REQUIRES one of those two
// (checked in oneShot: nothing unattended may be unbounded in every dimension).
const stepsUnlimited = -1

// parseStepCeiling reads a step ceiling the way an operator writes one: a
// number, or a word for "no ceiling". An explicit 0 is taken as the word,
// because a run that may take zero steps is not a thing anybody asks for, while
// `-max-steps 0` is exactly what a person types when they mean "no cap".
func parseStepCeiling(what, v string) (int, error) {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "":
		return 0, nil // nobody said
	case "unlimited", "none", "off", "no", "0":
		return stepsUnlimited, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, usageErrf("%s wants a number of steps or the word unlimited, got %q", what, v)
	}
	if n < 0 {
		return 0, usageErrf("%s %d is negative — give a positive number of steps, or the word unlimited", what, n)
	}
	return n, nil
}

func newRunBudget(rc *RolesConfig, timeout time.Duration, maxSteps, maxTokens int) (*runBudget, error) {
	b := &runBudget{}
	if rc != nil {
		b.timeout, b.maxSteps, b.maxTokens = rc.RunTimeout, rc.RunMaxSteps, rc.RunMaxTokens
	}
	switch {
	case timeout < 0:
		return nil, usageErrf("-timeout %s is negative — give a duration like 30m, or leave it out for no limit", timeout)
	case maxSteps < stepsUnlimited:
		return nil, usageErrf("-max-steps %d is negative — give a positive number of steps, the word unlimited, or leave it out", maxSteps)
	case maxTokens < 0:
		return nil, usageErrf("-max-tokens %d is negative — give a positive token count, or leave it out", maxTokens)
	}
	if timeout > 0 {
		b.timeout = timeout
	}
	if maxSteps != 0 {
		b.maxSteps = maxSteps // including stepsUnlimited: the flag outranks the file
	}
	if maxTokens > 0 {
		b.maxTokens = maxTokens
	}
	return b, nil
}

// steps is the per-SESSION ceiling the step budget also implies, or 0 when there
// is none. It is read by Session.maxSteps for every session in the run, the
// primary and its subagents alike: "-max-steps over the role's own" means
// exactly that, and a subagent left on its role's 80 would spend the run's
// budget on behalf of a primary capped at 20.
//
// It is NOT the whole of the ceiling, and that is the fix recorded in
// spendStep: one session's loop restarting at 0 on every verifier attempt made
// `-max-steps 200` with `verify_attempts: 3` and one delegation into 1200 model
// requests. This bounds a session; spendStep bounds the run.
// bounded answers whether SOMETHING other than the step count would stop this
// run: a clock or a token ceiling. Those two are the real bounds — the clock
// cancels the context and the tokens stop the model — and one of them has to
// exist before the step ceiling may be removed in a run nobody is watching.
func (b *runBudget) bounded() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.timeout > 0 || b.maxTokens > 0
}

func (b *runBudget) steps() int {
	if b == nil || !b.armedNow() {
		return 0
	}
	return b.maxSteps
}

// spendStep records one model request and reports the run-wide step ceiling as
// spent when it is. Counted here rather than from the session's own loop
// variable because that variable is per-session AND per-attempt: Session.Run
// starts at 0 again on every verifier retry, and every subagent gets a fresh
// one. roles.go and the header above both promise these ceilings bound "the
// whole run — every role, every subagent, every retry", and a looping agent is
// precisely what the number exists to stop, so the number has to count the run.
func (b *runBudget) spendStep() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.armed {
		return
	}
	b.steps_++
	if b.maxSteps > 0 && b.steps_ >= b.maxSteps && b.reason == "" {
		b.reason = fmt.Sprintf("the run's %d-step budget is spent", b.maxSteps)
	}
}

func (b *runBudget) armedNow() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.armed
}

// declared reports whether this run was given any ceiling at all. It is what
// tells a run somebody budgeted from the bare `lca "task"` that oneShot's legacy
// exit rule protects: a flag or a defaults: block is something that intends to
// read the exit code.
func (b *runBudget) declared() bool {
	if b == nil {
		return false
	}
	return b.timeout > 0 || b.maxSteps > 0 || b.maxTokens > 0
}

// start hangs the run off parent and arms the wall clock. The returned cancel
// must be deferred by the caller: it is what reaps the children on every exit
// path, including the ones nobody planned.
// It is also what ARMS the ceilings, which is what makes them a property of one
// run: a one-shot, an eval task and a workflow call it, and the REPL does not.
func (b *runBudget) start(parent context.Context) (context.Context, context.CancelFunc) {
	if b == nil {
		return context.WithCancel(parent)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armed = true
	if b.timeout <= 0 {
		return context.WithCancel(parent)
	}
	b.deadline = time.Now().Add(b.timeout)
	return context.WithDeadline(parent, b.deadline)
}

// spend records what a reply cost. Every session in the run calls it with its own
// usage, so the count is the run's and not one agent's — a lead that delegates
// four coders spends four agents' tokens on one ticket, and a ceiling that only
// saw the lead's own replies would be a ceiling on the cheapest part of the run.
//
// It deliberately does NOT cancel the run. A spent token budget stops the MODEL,
// and the verifier costs no tokens: a run whose last reply said "done" and went
// one token over must still have its check run, or a finished ticket is reported
// as "did not fit" and goes to a human for nothing. The clock is the budget that
// cancels, because when the time is gone there is nothing left to run the check
// in either.
func (b *runBudget) spend(prompt, completion, cached int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prompt += prompt
	b.completion += completion
	b.cached += cached
	// Counted unconditionally, armed or not, because the result object and the
	// summary's footer read these numbers for the cost report and a REPL turn
	// costs real tokens too. Only the CEILING is a property of a run.
	if !b.armed {
		return
	}
	spent := b.prompt + b.completion
	if b.maxTokens > 0 && spent >= b.maxTokens && b.reason == "" {
		b.reason = fmt.Sprintf("the run's token budget is spent: %s of %s prompt+completion tokens",
			ctxfmt(spent), ctxfmt(b.maxTokens))
	}
}

// tokens is what the run has spent so far, across every session in it. It goes
// into the trace beside the reason, because "the budget is spent" is only
// actionable next to the number that spent it.
func (b *runBudget) tokens() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.prompt + b.completion
}

// spentTokens is the run's cost as the result object and the summary report it:
// every session, split the three ways the gateway names them.
func (b *runBudget) spentTokens() (prompt, completion, cached int) {
	if b == nil {
		return 0, 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.prompt, b.completion, b.cached
}

// over is the reason the run must stop NOW, or "" while it may continue. The
// token ceiling is recorded by spend; the clock is read here, because a deadline
// that passed while the model was thinking is not an event anybody fired.
func (b *runBudget) over() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.armed {
		return ""
	}
	if b.reason != "" {
		return b.reason
	}
	if !b.deadline.IsZero() && !time.Now().Before(b.deadline) {
		b.reason = fmt.Sprintf("the run's %s time budget is spent", b.timeout)
		return b.reason
	}
	return ""
}

// tripped is over() for the status decision: the same question asked after the
// run, when the only thing left is to name what ended it. It is deliberately the
// same cached reason, so the line in the transcript, the reason in the JSON and
// the sentence in the summary are one string and cannot disagree.
func (b *runBudget) tripped() string { return b.over() }

// closingDeadline bounds the one model call that happens AFTER the work is over
// — the human-readable summary (P0-6). It exists because that call must not be
// able to break the promise the budget just made: `-timeout 30s` is accepted on
// the strength of "out within 35 s", and a summary that streams for a minute
// after the deadline would make the flag a lie.
//
// So the summary gets whatever is left of the run's own clock, plus a fixed
// grace — and when the deadline has already passed, the grace alone. A summary
// that does not finish in it is still WRITTEN; what the model did not supply,
// lca writes itself (see summary.go).
func (b *runBudget) closingDeadline(now time.Time) time.Time {
	hard := now.Add(summaryBudget)
	if b == nil {
		return hard
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deadline.IsZero() {
		return hard
	}
	if lim := b.deadline.Add(summaryGrace); lim.Before(hard) {
		hard = lim
	}
	if floor := now.Add(summaryGrace); hard.Before(floor) {
		hard = floor
	}
	return hard
}

// describe is the budget as doctor and the banner print it: only the ceilings
// that exist, because "max_tokens: unlimited" is a row that teaches nobody
// anything.
func (b *runBudget) describe() string {
	if b == nil {
		return "none"
	}
	var parts []string
	if b.timeout > 0 {
		parts = append(parts, b.timeout.String())
	}
	if b.maxSteps > 0 {
		parts = append(parts, strconv.Itoa(b.maxSteps)+" steps")
	}
	if b.maxTokens > 0 {
		parts = append(parts, ctxfmt(b.maxTokens)+" tokens")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, gSep)
}

// runContext is the context every child of this run hangs off. Background
// subagents used to take context.Background(), which made them the one thing a
// budget could not reach: the run ended, lca exited, and a `cargo build` started
// by a background delegation kept a core busy with nobody left to read its
// output.
func (o *Orchestrator) runContext() context.Context {
	if o == nil {
		return context.Background()
	}
	o.runMu.Lock()
	defer o.runMu.Unlock()
	if o.runCtx == nil {
		return context.Background()
	}
	return o.runCtx
}

// stepRules is the rule stack a workflow step is checked against: the base
// posture plus the json layer's rules. Deliberately NOT a role's own block —
// a step belongs to the workflow and not to any one role, and a step's member
// may differ from the lead's.
func (o *Orchestrator) stepRules() []Ruleset {
	if o == nil {
		return []Ruleset{defaultRules()}
	}
	return []Ruleset{defaultRules(), o.userRules}
}

func (o *Orchestrator) setRunContext(ctx context.Context) {
	if o == nil {
		return
	}
	o.runMu.Lock()
	o.runCtx = ctx
	o.runMu.Unlock()
}

// parseRunDuration reads a duration the way roles.yaml's defaults: block spells
// them. `check_timeout` next to it is a bare number of seconds, so a bare number
// here means seconds too — a file where one key counts seconds and its neighbour
// counts nanoseconds would be a trap.
func parseRunDuration(key, v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("defaults: %s: %q is negative", key, v)
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("defaults: %s: %q is not a duration (30m, 45s) or a number of seconds", key, v)
	}
	if d < 0 {
		return 0, fmt.Errorf("defaults: %s: %q is negative", key, v)
	}
	return d, nil
}
