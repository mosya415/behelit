package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Gateway failure policy. Every model call goes through Session.chat, which
// walks the role's model chain (roles.yaml: models: [primary, fallback, …])
// according to what kind of failure the gateway reported:
//
//	not up      (model paused/drained, cold start failed, no backend, unknown
//	             model)   → move to the next model of the role; with none left,
//	                        wait Retry-After and walk the chain again
//	overloaded  (503 + X-Berserk-Overload, 429)
//	                      → wait Retry-After on the SAME model; never fall back
//	                        (a fallback would dump load and a cold KV cache on a
//	                        model that isn't the one this role was tuned for)
//	before the first byte (connection refused/reset, timeout, 5xx)
//	                      → exactly one attempt on the next model
//	after the first byte (stream cut mid-reply)
//	                      → repeat the turn on the SAME model, up to
//	                        LCA_GW_CUT_RETRIES (default 2). A turn is
//	                        idempotent: tool calls execute only once they have
//	                        arrived whole, so nothing ran; the prefix is in the
//	                        KV cache, so a repeat costs only the decode. (The
//	                        gateway doesn't migrate requests carrying tools.)
//
// Anything else (400s, context overflow) is returned to the loop as is.
// Waiting is bounded by LCA_GW_MAX_WAIT (seconds, default 900) per call.

type gwFailure int

const (
	gwOther gwFailure = iota
	gwNotUp
	gwOverloaded
	gwBeforeFirstByte
	gwAfterFirstByte
)

func (f gwFailure) String() string {
	switch f {
	case gwNotUp:
		return "not up"
	case gwOverloaded:
		return "overloaded"
	case gwBeforeFirstByte:
		return "failed before first byte"
	case gwAfterFirstByte:
		return "cut after first byte"
	}
	return "error"
}

var notUpPhrases = []string{"cold start failed", "no backend", "no healthy", "no decode", "no prefill", "unknown model", "model not found", "does not exist", "is paused", "is drained"}

// classifyGW maps a failed call to the policy above. started = at least one
// byte of the response stream had arrived.
func classifyGW(err error, started bool) gwFailure {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return gwOther
	}
	if started {
		return gwAfterFirstByte
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		return gwBeforeFirstByte // transport: no HTTP response at all
	}
	body := strings.ToLower(ae.Body)
	switch {
	case ae.Overload || ae.Type == "berserk_gw_overload" || ae.Status == 429:
		return gwOverloaded
	case ae.State != "" || ae.Type == "berserk_gw_paused" || ae.Type == "berserk_gw_drained":
		return gwNotUp
	case isContextOverflow(err):
		return gwOther
	case ae.Status == 404 || ae.Status == 503:
		for _, p := range notUpPhrases {
			if strings.Contains(body, p) {
				return gwNotUp
			}
		}
		if ae.Status == 503 {
			return gwOverloaded // a bare 503 means "busy" on most servers
		}
		return gwOther
	case ae.Status >= 500:
		return gwBeforeFirstByte // 500/502/504: the request never produced output
	}
	return gwOther
}

func gwMaxWait() time.Duration {
	return time.Duration(atoiDefault(os.Getenv("LCA_GW_MAX_WAIT"), 900)) * time.Second
}

// fallbackEvent is one model switch or wait, for the trace.
type fallbackEvent struct {
	From   string `json:"from"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason"`
	WaitMs int64  `json:"wait_ms,omitempty"`
}

// chat performs one model call under the gateway policy. It returns the
// result, the model that produced it, and the switches/waits it took.
func (s *Session) chat(ctx context.Context, req ChatRequest, sink StreamSink) (ChatResult, []fallbackEvent, error) {
	req.Headers = s.headers()
	deadline := time.Now().Add(gwMaxWait())
	var events []fallbackEvent
	settled := s.modelIdx // the model this session has been using
	cuts := 0
	triedNotUp := map[int]bool{s.modelIdx: true}
	preByteUsed := false

	sleep := func(d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}

	for {
		res, err := s.client.Chat(ctx, req, sink)
		if err == nil || ctx.Err() != nil {
			return res, events, err
		}
		from := s.client.Model()
		kind := classifyGW(err, res.Started)
		switch kind {
		case gwOverloaded:
			wait := orDuration(retryAfterOf(err), 2*time.Second)
			if time.Now().Add(wait).After(deadline) {
				return res, events, fmt.Errorf("%s stayed overloaded for %s: %w", from, gwMaxWait(), err)
			}
			events = append(events, fallbackEvent{From: from, Reason: kind.String(), WaitMs: wait.Milliseconds()})
			s.view.Note(fmt.Sprintf("%s overloaded — waiting %s (no fallback)", from, fmtDurShort(wait)))
			if err := sleep(wait); err != nil {
				return res, events, err
			}

		case gwNotUp:
			if next := s.nextModel(triedNotUp); next >= 0 {
				triedNotUp[next] = true
				s.useModel(next)
				events = append(events, fallbackEvent{From: from, To: s.client.Model(), Reason: kind.String()})
				s.view.Warn(fmt.Sprintf("%s is not up — falling back to %s", from, s.client.Model()))
				continue
			}
			wait := orDuration(retryAfterOf(err), 5*time.Second)
			if time.Now().Add(wait).After(deadline) {
				return res, events, fmt.Errorf("no model of role %s came up within %s: %w", s.agent.Name, gwMaxWait(), err)
			}
			events = append(events, fallbackEvent{From: from, Reason: kind.String(), WaitMs: wait.Milliseconds()})
			s.view.Note(fmt.Sprintf("no model of role %s is up — waiting %s", s.agent.Name, fmtDurShort(wait)))
			if err := sleep(wait); err != nil {
				return res, events, err
			}
			// walk the chain again, starting from the model the session settled on
			s.useModel(settled)
			triedNotUp = map[int]bool{settled: true}

		case gwBeforeFirstByte:
			if preByteUsed {
				return res, events, err
			}
			preByteUsed = true
			next := s.modelIdx
			if n := len(s.models); n > 1 {
				next = (s.modelIdx + 1) % n
			}
			s.useModel(next)
			events = append(events, fallbackEvent{From: from, To: s.client.Model(), Reason: kind.String()})
			if s.client.Model() == from {
				s.view.Warn(fmt.Sprintf("%s: %s — retrying once", from, shortErr(err)))
			} else {
				s.view.Warn(fmt.Sprintf("%s: %s — one attempt on %s", from, shortErr(err), s.client.Model()))
			}

		case gwAfterFirstByte:
			if cuts >= gwCutRetries() {
				return res, events, err
			}
			cuts++
			events = append(events, fallbackEvent{From: from, To: from, Reason: kind.String()})
			s.view.Warn(fmt.Sprintf("%s: reply cut off (%s) — repeating the turn (%d/%d), nothing was executed", from, shortErr(err), cuts, gwCutRetries()))
			if sink.Discard != nil {
				sink.Discard()
			}

		default:
			return res, events, err
		}
	}
}

func gwCutRetries() int {
	if v := os.Getenv("LCA_GW_CUT_RETRIES"); v == "0" {
		return 0
	}
	return atoiDefault(os.Getenv("LCA_GW_CUT_RETRIES"), 2)
}

// transportFor resolves a gateway model's tool transport: an explicit run
// override (eval -transport), then roles.yaml's per-model setting, then its
// top-level transport, then the endpoint default ("" = the provider's).
func (o *Orchestrator) transportFor(model string) string {
	if o.cfg.TransportOverride != "" {
		return o.cfg.TransportOverride
	}
	if o.roles != nil {
		return o.roles.transportOf(model)
	}
	return ""
}

func retryAfterOf(err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.RetryAfter
	}
	return 0
}

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// nextModel is the first model after the current one (wrapping) not yet tried.
func (s *Session) nextModel(tried map[int]bool) int {
	n := len(s.models)
	for k := 1; k < n; k++ {
		i := (s.modelIdx + k) % n
		if !tried[i] {
			return i
		}
	}
	return -1
}

// useModel points the session at models[i] of its role chain (no-op without
// a chain). The switch sticks for the rest of the session: the fallback now
// holds this conversation's KV cache on the gateway. The prefix (system
// prompt, tools) does not mention the model, so it stays byte-identical.
func (s *Session) useModel(i int) {
	if len(s.models) == 0 || i < 0 || i >= len(s.models) {
		return
	}
	if s.transport == "" {
		// Fixed for the session by its first model: switching transports
		// mid-conversation would change the prefix and the history format.
		s.transport = s.orch.transportFor(s.models[i])
	}
	s.modelIdx = i
	c := *s.orch.providers.local
	c.model = s.models[i]
	c.ctxLen, c.ctxSrc = 0, OriginUnset
	// The engine is forgotten exactly where the window is, and for the same
	// reason: both are facts about one (endpoint, model) pair, and this line
	// changes the model. Without it the copy inherits the local client's answer —
	// including a roles.yaml per-model pin, which is the STRONGEST source there
	// is, so nothing below could take it back — and the one model an operator
	// wrote `engine: sglang` down for would decide the body shape of every other
	// model the same endpoint serves.
	c.resetEngine()
	// …and then learn this model's window from the deployment that serves it.
	// Without this a chain client keeps ctxLen 0 forever — main.go reconciles
	// only when the session IS the local client, which a chain session never is,
	// and relearnCtxLen is reachable only from /model and /endpoint — so a role
	// with `models:` budgeted a 1M card window against a 131k deployment: the
	// silent overflow models.go's header exists to prevent. Cached per endpoint,
	// so a failover does not add a round-trip per retry.
	s.orch.providers.Learn(&c)
	c.transport = s.transport
	if s.orch.roles != nil {
		if o := s.orch.roles.modelOpts(c.model); o != nil {
			c.noReplay = o.NoReplay
		}
	}
	// After Learn, so the file beats the probe: roles.yaml is the strongest engine
	// source there is, and without this line a chain failover silently loses it —
	// the one model on the other engine is the one an operator wrote it down for.
	s.orch.applyModelEngine(&c)
	s.client = &c
}

// applyModelEngine puts roles.yaml's models.<id>.engine — tier 1 of the engine
// precedence, and the only source a probe may never overrule — onto a client. It
// runs where a client's MODEL is decided and nowhere else: engine detection
// happens once, before the first request, because the gateway keys its KV cache
// on the request prefix and a body shape that changes mid-session loses every hit.
func (o *Orchestrator) applyModelEngine(c *Client) {
	if o == nil || o.roles == nil || c == nil {
		return
	}
	c.setEngine(o.roles.engineOf(c.Model()), EngineFromModel)
}

// headers identify the conversation to the gateway: x-session-id is this
// session (its KV-cache affinity key), x-root-session-id the whole task tree.
func (s *Session) headers() map[string]string {
	return map[string]string{"x-session-id": s.UID, "x-root-session-id": s.rootUID()}
}
