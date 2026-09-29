package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The trace: one JSONL record per model turn and one per finished task, for
// the whole session tree. It is the data eval (eval.go) scores runs from, and
// the place to look for cache efficiency (cached_tokens), fallbacks and where
// the tool calls went. Written to $LCA_TRACE, or $LCA_DIR/traces/<root>.jsonl.

type Tracer struct {
	mu   sync.Mutex
	f    *os.File
	Path string
}

func NewTracer(path string) (*Tracer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Tracer{f: f, Path: path}, nil
}

func (t *Tracer) write(v any) {
	if t == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	t.mu.Lock()
	t.f.Write(append(b, '\n'))
	t.mu.Unlock()
}

func (t *Tracer) Close() {
	if t != nil {
		t.f.Close()
	}
}

type traceUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

type traceToolCall struct {
	Name        string `json:"name"`
	Args        string `json:"args,omitempty"`
	Raw         string `json:"raw,omitempty"` // what the model actually emitted, when it didn't parse
	OK          bool   `json:"ok"`
	Invalid     bool   `json:"invalid,omitempty"`
	Error       string `json:"error,omitempty"`
	ResultBytes int    `json:"result_bytes"`
	Ms          int64  `json:"ms"`
}

// TurnRecord is one model call and the tools it triggered.
type TurnRecord struct {
	Type          string          `json:"type"` // "turn"
	TS            string          `json:"ts"`
	RootSession   string          `json:"root_session"`
	Session       string          `json:"session"`
	ParentSession string          `json:"parent_session,omitempty"`
	Role          string          `json:"role"`
	Model         string          `json:"model"`
	Step          int             `json:"step"`
	Usage         traceUsage      `json:"usage"`
	TTFTMs        int64           `json:"ttft_ms"`
	DurationMs    int64           `json:"duration_ms"`
	Finish        string          `json:"finish,omitempty"`
	Fallbacks     []fallbackEvent `json:"fallbacks,omitempty"`
	ToolCalls     []traceToolCall `json:"tool_calls"`
	InvalidCalls  int             `json:"invalid_calls"`       // malformed calls (bad JSON / unknown tool / schema) + unparsable text tags
	RawReply      string          `json:"raw_reply,omitempty"` // the reply's own text, kept when a call didn't parse
	Transport     string          `json:"transport"`
	Tier          string          `json:"tier,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// TaskRecord is the outcome of a verified task (a delegation, a -check run,
// an eval task) — decided by the verifier, not the model.
type TaskRecord struct {
	Type          string `json:"type"` // "task"
	TS            string `json:"ts"`
	RootSession   string `json:"root_session"`
	Session       string `json:"session"`
	ParentSession string `json:"parent_session,omitempty"`
	Role          string `json:"role"`
	Task          string `json:"task"`
	Status        string `json:"status"` // passed | failed | unverified | conflict | error | cancelled
	CheckCmd      string `json:"check_cmd,omitempty"`
	CheckExit     *int   `json:"check_exit,omitempty"`
	Attempts      int    `json:"attempts"`
	DiffBytes     int    `json:"diff_bytes"`
	FilesChanged  int    `json:"files_changed"`
	Applied       bool   `json:"applied"`
	DurationMs    int64  `json:"duration_ms"`
	Reviewer      string `json:"reviewer,omitempty"`
	ReviewModel   string `json:"review_model,omitempty"`
	ReviewVerdict string `json:"review_verdict,omitempty"` // approve | reject | unreviewed
}

func (s *Session) rootUID() string {
	if s.rootOverride != "" {
		return s.rootOverride
	}
	r := s
	for r.parent != nil {
		r = r.parent
	}
	return r.UID
}

// traceTS is the one timestamp spelling of the trace and of a run's state:
// UTC and RFC3339Nano, so records from several processes sort as text.
func traceTS(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nowTS() string { return traceTS(time.Now()) }

func (s *Session) parentUID() string {
	if s.parent != nil {
		return s.parent.UID
	}
	return ""
}

func (s *Session) traceTurn(step int, res ChatResult, fb []fallbackEvent, start time.Time, calls []pendingCall, err error) {
	rec := TurnRecord{Type: "turn", TS: traceTS(start), RootSession: s.rootUID(), Session: s.UID,
		ParentSession: s.parentUID(), Role: s.agent.Name, Model: s.client.Model(), Step: step,
		Usage:  traceUsage{res.Usage.PromptTokens, res.Usage.CompletionTokens, res.Usage.CachedTokens},
		TTFTMs: res.Usage.TTFT.Milliseconds(), DurationMs: time.Since(start).Milliseconds(), Finish: res.Finish,
		Fallbacks: fb, ToolCalls: []traceToolCall{}, InvalidCalls: s.malformed, Transport: transportName(s.client), Tier: s.tier()}
	s.malformed = 0
	for _, c := range calls {
		tc := traceToolCall{Name: c.name, Args: truncate(canonicalArgs(c.args), 300), OK: !c.failed, Invalid: c.invalid, ResultBytes: c.resultBytes, Ms: c.ms}
		if c.invalid {
			rec.InvalidCalls++
			tc.Raw = truncate(c.raw, 1000)
		}
		if c.failed {
			tc.Error = truncate(c.errText, 300)
		}
		rec.ToolCalls = append(rec.ToolCalls, tc)
	}
	if rec.InvalidCalls > 0 {
		// Keep the model's own words next to the parsed calls: without them a
		// parse failure is unreproducible.
		rec.RawReply = truncate(res.Content, 2000)
	}
	if err != nil {
		rec.Error = err.Error()
	}
	s.orch.tracer.write(rec)
}

func transportName(c *Client) string {
	if c.Native() {
		return transportNative
	}
	return transportText
}

func (s *Session) traceTask(task string, v Verdict, check string, diffBytes, files int, applied bool, review *reviewOutcome, start time.Time) {
	rec := TaskRecord{Type: "task", TS: nowTS(), RootSession: s.rootUID(), Session: s.UID,
		ParentSession: s.parentUID(), Role: s.agent.Name, Task: truncate(task, 500), Status: v.Status, CheckCmd: check,
		Attempts: v.Attempts, DiffBytes: diffBytes, FilesChanged: files, Applied: applied, DurationMs: time.Since(start).Milliseconds()}
	if review != nil {
		rec.Reviewer, rec.ReviewModel, rec.ReviewVerdict = review.Role, review.Model, review.Verdict
	}
	if v.Checked {
		exit := v.Exit
		rec.CheckExit = &exit
	}
	s.orch.tracer.write(rec)
}
