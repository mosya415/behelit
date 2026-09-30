package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── fake OpenAI-compatible server ──────────────────────────────────────────

type fakeReply struct {
	content   string
	reasoning string
	calls     []ToolCall
	status    int    // non-200 → error response
	header    string // "Key: value" extra header on errors
	header2   string
	hangup    bool // close the connection before any response
	cut       bool // stream some content, then drop the connection
}

type fakeRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Tools    []ToolSchema     `json:"tools"`
	Raw      map[string]any   `json:"-"`
	Header   http.Header      `json:"-"`
	Body     string           `json:"-"`
}

func (r fakeRequest) system() string {
	if len(r.Messages) > 0 {
		s, _ := r.Messages[0]["content"].(string)
		return s
	}
	return ""
}

func (r fakeRequest) last() map[string]any { return r.Messages[len(r.Messages)-1] }

type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []fakeRequest
	respond  func(req fakeRequest, n int) fakeReply
	models   []string // served by GET /models (nil → any model name accepted as listed)
	// windows lets /models report max_model_len per id. Window provenance —
	// "262k (server)" versus "200k (card)" — cannot be tested at all without it.
	windows map[string]int
}

func newFakeServer(t *testing.T, respond func(req fakeRequest, n int) fakeReply) *fakeServer {
	fs := &fakeServer{respond: respond}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			var data []map[string]any
			for _, m := range fs.models {
				row := map[string]any{"id": m, "object": "model"}
				if n := fs.windows[m]; n > 0 {
					row["max_model_len"] = n
				}
				data = append(data, row)
			}
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req fakeRequest
		json.Unmarshal(body, &req)
		json.Unmarshal(body, &req.Raw)
		req.Header, req.Body = r.Header.Clone(), string(body)
		fs.mu.Lock()
		fs.requests = append(fs.requests, req)
		n := len(fs.requests)
		fs.mu.Unlock()
		rep := fs.respond(req, n)
		if rep.hangup {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
			}
			return
		}
		if rep.cut {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"partial"}}]}`)
			w.(http.Flusher).Flush()
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
			}
			return
		}
		if rep.status != 0 && rep.status != 200 {
			for _, h := range []string{rep.header, rep.header2} {
				if k, v, ok := strings.Cut(h, ": "); ok {
					w.Header().Set(k, v)
				}
			}
			w.WriteHeader(rep.status)
			io.WriteString(w, `{"error":{"message":"`+rep.content+`"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if rep.reasoning != "" {
			send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": rep.reasoning}}}})
		}
		if rep.content != "" {
			// split to exercise delta assembly
			mid := len(rep.content) / 2
			send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": rep.content[:mid]}}}})
			send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": rep.content[mid:]}}}})
		}
		for i, c := range rep.calls {
			args := c.Function.Arguments
			cut := len(args) / 2
			send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": i, "id": c.ID, "type": "function", "function": map[string]any{"name": c.Function.Name, "arguments": args[:cut]}}}}}}})
			send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": i, "function": map[string]any{"arguments": args[cut:]}}}}}}})
		}
		finish := "stop"
		if len(rep.calls) > 0 {
			finish = "tool_calls"
		}
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": finish}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10}})
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fakeServer) reqs() []fakeRequest {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]fakeRequest(nil), fs.requests...)
}

func call(id, name string, args map[string]any) ToolCall {
	b, _ := json.Marshal(args)
	return ToolCall{ID: id, Type: "function", Function: ToolFunction{Name: name, Arguments: string(b)}}
}

// ── harness ────────────────────────────────────────────────────────────────

type quietView struct {
	mu    sync.Mutex
	tools []string
	notes []string
}

func (v *quietView) Stream() StreamView { return nullStream{} }
func (v *quietView) ToolStart(name, summary string) {
	v.mu.Lock()
	v.tools = append(v.tools, name)
	v.mu.Unlock()
}
func (v *quietView) ToolDone(string, Args, string) {}
func (v *quietView) Note(t string) {
	v.mu.Lock()
	v.notes = append(v.notes, t)
	v.mu.Unlock()
}
func (v *quietView) Warn(t string)                              { v.Note(t) }
func (v *quietView) Error(t string)                             { v.Note(t) }
func (v *quietView) Perf(Usage)                                 {}
func (v *quietView) Todos([]Todo)                               {}
func (v *quietView) Live() io.Writer                            { return nil }
func (v *quietView) Begin()                                     {}
func (v *quietView) Finish(string, time.Duration)               {}
func (v *quietView) Check(string, int, time.Duration, int, int) {}

type harness struct {
	orch *Orchestrator
	sess *Session
	view *quietView
	root string
}

// newHarness builds an orchestrator against a fake endpoint. transport is
// "native" or "text"; approve=true trusts all side effects.
func newHarness(t *testing.T, url, transport string, approve bool) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home) // keep ~/.claude/agents etc. out of tests
	root := t.TempDir()
	cfg := Config{Root: root, Dir: filepath.Join(home, ".lca"), BaseURL: url, Endpoints: []string{url}, Model: "test-model",
		Temperature: 0.2, MaxSteps: 20, Allowed: []string{"echo", "ls"}, Tools: transport, SubagentMax: 1, KeepSessions: 10}
	jail, err := NewJail(root, cfg.Allowed, false)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := NewRecorder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.Close)
	ap := NewApprover(newStringInput(""))
	if approve {
		ap.TrustAll()
	}
	fc, err := loadFileConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tracer, err := NewTracer(filepath.Join(home, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracer.Close)
	orch := NewOrchestrator(cfg, fc, jail, ap, rec, NewClient(cfg), nil, tracer)
	view := &quietView{}
	sess, err := orch.NewPrimary("build", "", view)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range orch.Tasks() {
		c.view = view
	}
	return &harness{orch: orch, sess: sess, view: view, root: root}
}

func (h *harness) run(t *testing.T, prompt string) error {
	t.Helper()
	h.sess.Msgs = append(h.sess.Msgs, Message{Role: "user", Content: prompt})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return h.sess.Run(ctx)
}

func isExploreReq(r fakeRequest) bool { return strings.Contains(r.system(), "file search specialist") }

// ── tests ──────────────────────────────────────────────────────────────────

func TestNativeToolLoop(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		switch n {
		case 1:
			return fakeReply{content: "Reading.", calls: []ToolCall{call("c1", "read_file", map[string]any{"path": "a.txt"})}}
		default:
			return fakeReply{content: "The file says hello."}
		}
	})
	h := newHarness(t, fs.URL, "native", false)
	os.WriteFile(filepath.Join(h.root, "a.txt"), []byte("hello\n"), 0o644)
	if err := h.run(t, "what does a.txt say?"); err != nil {
		t.Fatal(err)
	}
	rs := fs.reqs()
	if len(rs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(rs))
	}
	if len(rs[0].Tools) == 0 {
		t.Fatal("native request carried no tools")
	}
	last := rs[1].last()
	if last["role"] != "tool" || last["tool_call_id"] != "c1" || !strings.Contains(last["content"].(string), "hello") {
		t.Fatalf("second request should end with the tool result, got %v", last)
	}
	prev := rs[1].Messages[len(rs[1].Messages)-2]
	if tcs, _ := prev["tool_calls"].([]any); len(tcs) != 1 {
		t.Fatalf("assistant tool_calls not replayed: %v", prev)
	}
	if final := h.sess.Msgs[len(h.sess.Msgs)-1]; final.Role != "assistant" || final.Content != "The file says hello." {
		t.Fatalf("bad final message: %+v", final)
	}
}

func TestTextProtocolLoop(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{content: "Looking.\n<read_file path=\"a.txt\"/>"}
		}
		return fakeReply{content: "Done."}
	})
	h := newHarness(t, fs.URL, "text", false)
	os.WriteFile(filepath.Join(h.root, "a.txt"), []byte("hi\n"), 0o644)
	if err := h.run(t, "read it"); err != nil {
		t.Fatal(err)
	}
	rs := fs.reqs()
	if len(rs) != 2 || len(rs[0].Tools) != 0 {
		t.Fatalf("text transport: want 2 requests without tools, got %d (tools %d)", len(rs), len(rs[0].Tools))
	}
	if !strings.Contains(rs[0].system(), "<read_file path=") {
		t.Fatal("text system prompt lacks tag docs")
	}
	if c := rs[1].last()["content"].(string); !strings.HasPrefix(c, "<tool_result name=\"read_file\"") {
		t.Fatalf("want a tool_result user message, got %q", c)
	}
}

func TestParallelSubagents(t *testing.T) {
	var inflight, peak atomic.Int32
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if isExploreReq(req) {
			cur := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
			prompt, _ := req.Messages[1]["content"].(string)
			return fakeReply{content: "report for: " + prompt}
		}
		last := req.last()
		if last["role"] == "tool" {
			return fakeReply{content: "Both explored."}
		}
		return fakeReply{calls: []ToolCall{
			call("a", "task", map[string]any{"agent": "explore", "description": "find A", "prompt": "question A"}),
			call("b", "task", map[string]any{"agent": "explore", "description": "find B", "prompt": "question B"}),
		}}
	})
	h := newHarness(t, fs.URL, "native", false)
	if err := h.run(t, "explore two things"); err != nil {
		t.Fatal(err)
	}
	if peak.Load() < 2 {
		t.Fatalf("subagents did not run concurrently (peak %d)", peak.Load())
	}
	var results []string
	for _, m := range h.sess.Msgs {
		if m.Role == "tool" {
			results = append(results, m.Content)
		}
	}
	if len(results) != 2 || !strings.Contains(results[0], "report for: question A") || !strings.Contains(results[1], "report for: question B") {
		t.Fatalf("results out of order or missing: %q", results)
	}
	if !strings.Contains(results[0], `state="completed"`) {
		t.Fatalf("missing task envelope: %s", results[0])
	}
	// explore is read-only and cannot delegate further
	for _, r := range fs.reqs() {
		if isExploreReq(r) {
			for _, tl := range r.Tools {
				switch tl.Function.Name {
				case "task", "edit", "write", "run_command", "todowrite":
					t.Fatalf("explore was offered %s", tl.Function.Name)
				}
			}
		}
	}
	if len(h.orch.Tasks()) != 2 {
		t.Fatalf("want 2 registered child sessions, got %d", len(h.orch.Tasks()))
	}
}

func TestBackgroundSubagentDelivers(t *testing.T) {
	release := make(chan struct{})
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if isExploreReq(req) {
			<-release
			return fakeReply{content: "background finding"}
		}
		content, _ := req.last()["content"].(string)
		switch {
		case strings.Contains(content, "[Background subagent results]"):
			return fakeReply{content: "Got it: background finding relayed."}
		case req.last()["role"] == "tool":
			close(release) // let the subagent finish only after the parent moved on
			return fakeReply{content: "Started; waiting."}
		}
		return fakeReply{calls: []ToolCall{call("bg", "task", map[string]any{"agent": "explore", "description": "slow search", "prompt": "look", "background": true})}}
	})
	h := newHarness(t, fs.URL, "native", false)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	final := h.sess.Msgs[len(h.sess.Msgs)-1]
	if !strings.Contains(final.Content, "relayed") {
		t.Fatalf("background result was not delivered and processed; final: %+v", final)
	}
	if h.sess.BackgroundRunning() != 0 {
		t.Fatal("background counter not released")
	}
}

func TestTextProtocolTaskTag(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if isExploreReq(req) {
			return fakeReply{content: "found it in x.go:3"}
		}
		if c, _ := req.last()["content"].(string); strings.HasPrefix(c, "<tool_result") {
			return fakeReply{content: "Answer: x.go:3"}
		}
		return fakeReply{content: "Delegating.\n<task agent=\"explore\" description=\"find x\">\nwhere is x?\n</task>"}
	})
	h := newHarness(t, fs.URL, "text", false)
	if err := h.run(t, "where is x"); err != nil {
		t.Fatal(err)
	}
	var sawTaskResult bool
	for _, m := range h.sess.Msgs {
		if m.Role == "user" && strings.Contains(m.Content, "found it in x.go:3") {
			sawTaskResult = true
		}
	}
	if !sawTaskResult {
		t.Fatal("task tag result missing from transcript")
	}
}

func TestDeniedEditWithoutApproval(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		switch n {
		case 1:
			return fakeReply{calls: []ToolCall{call("r", "read_file", map[string]any{"path": "a.txt"})}}
		case 2:
			return fakeReply{calls: []ToolCall{call("e", "edit", map[string]any{"path": "a.txt", "old_string": "one", "new_string": "two"})}}
		}
		return fakeReply{content: "ok"}
	})
	h := newHarness(t, fs.URL, "native", false) // no stdin → approval denied
	os.WriteFile(filepath.Join(h.root, "a.txt"), []byte("one\n"), 0o644)
	if err := h.run(t, "edit"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "a.txt")); string(got) != "one\n" {
		t.Fatalf("edit applied without approval: %q", got)
	}
	var res string
	for _, m := range h.sess.Msgs {
		if m.Role == "tool" && m.ToolCallID == "e" {
			res = m.Content
		}
	}
	if !strings.HasPrefix(res, "user denied") {
		t.Fatalf("want a denial result, got %q", res)
	}
}

func TestEditRequiresRead(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{calls: []ToolCall{call("e", "edit", map[string]any{"path": "a.txt", "old_string": "one", "new_string": "two"})}}
		}
		return fakeReply{content: "ok"}
	})
	h := newHarness(t, fs.URL, "native", true)
	os.WriteFile(filepath.Join(h.root, "a.txt"), []byte("one\n"), 0o644)
	h.run(t, "edit")
	if got, _ := os.ReadFile(filepath.Join(h.root, "a.txt")); string(got) != "one\n" {
		t.Fatalf("edit of an unread file was applied: %q", got)
	}
}

func TestDoomLoopStops(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{calls: []ToolCall{call(fmt.Sprint("c", n), "list_dir", map[string]any{"path": "."})}}
	})
	h := newHarness(t, fs.URL, "native", false) // doom_loop asks → denied (no stdin)
	h.run(t, "loop forever")
	if n := len(fs.reqs()); n != 3 {
		t.Fatalf("want the turn to stop at the 3rd identical call, got %d requests", n)
	}
}

func TestRetryOn429(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{status: 429, content: "rate limit", header: "retry-after-ms: 10"}
		}
		return fakeReply{content: "fine"}
	})
	h := newHarness(t, fs.URL, "native", false)
	if err := h.run(t, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(fs.reqs()) != 2 {
		t.Fatalf("want a retry, got %d requests", len(fs.reqs()))
	}
}

func TestContextOverflowCompacts(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if strings.Contains(req.system(), "context summarization agent") {
			return fakeReply{content: "## Objective\nkeep going"}
		}
		for _, m := range req.Messages {
			if c, _ := m["content"].(string); strings.HasPrefix(c, compactedPrefix) {
				return fakeReply{content: "continued after compaction"}
			}
		}
		return fakeReply{status: 400, content: "This model's maximum context length is 1000 tokens"}
	})
	h := newHarness(t, fs.URL, "native", false)
	h.sess.Msgs = append(h.sess.Msgs,
		Message{Role: "user", Content: "old question"}, Message{Role: "assistant", Content: "old answer"},
		Message{Role: "user", Content: "older question"}, Message{Role: "assistant", Content: "older answer"})
	if err := h.run(t, "new question"); err != nil {
		t.Fatal(err)
	}
	if last := h.sess.Msgs[len(h.sess.Msgs)-1]; last.Content != "continued after compaction" {
		t.Fatalf("want recovery after compaction, got %+v", last)
	}
	if !strings.HasPrefix(h.sess.Msgs[1].Content, compactedPrefix) {
		t.Fatal("transcript not compacted")
	}
}

func TestReasoningReplay(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1", Reasoning: "r1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Reasoning: "r2", ToolCalls: []ToolCall{call("x", "glob", map[string]any{"pattern": "*"})}},
		{Role: "tool", ToolCallID: "x", Content: "res", Tool: "glob"},
	}
	all := wire(msgs, "all")
	if all[2].Reasoning != "r1" || all[4].Reasoning != "r2" {
		t.Fatal("all: reasoning not replayed everywhere")
	}
	turn := wire(msgs, "turn")
	if turn[2].Reasoning != "" || turn[4].Reasoning != "r2" {
		t.Fatalf("turn: want only current-turn reasoning, got %q %q", turn[2].Reasoning, turn[4].Reasoning)
	}
	none := wire(msgs, "")
	if none[4].Reasoning != "" {
		t.Fatal("none: reasoning leaked")
	}
	b, _ := json.Marshal(none[5])
	if strings.Contains(string(b), `"tool"`) && strings.Contains(string(b), "glob") {
		t.Fatalf("transcript metadata leaked to the wire: %s", b)
	}
}

func TestRequestBodyPerProvider(t *testing.T) {
	cases := []struct {
		provider, model, thinking string
		want                      []string
		absent                    []string
	}{
		{"deepseek", "deepseek-v4-pro", "high", []string{`"reasoning_effort":"high"`, `"thinking":{"type":"enabled"}`, `"max_tokens":32000`}, []string{`"temperature"`, `"top_k"`}},
		{"moonshot", "kimi-k2.6", "", []string{`"temperature":1`, `"top_p":0.95`}, []string{`"thinking"`}},
		{"zai", "glm-4.7", "", []string{`"clear_thinking":false`, `"temperature":1`}, nil},
		{"dashscope", "qwen3.7-plus", "", []string{`"enable_thinking":true`}, []string{`"temperature"`}},
		{"minimax", "MiniMax-M3", "", []string{`"temperature":1`, `"top_p":0.95`}, []string{`"top_k"`}},
		// hy3 accepts no_think|low|high and nothing else: vLLM passes "medium"
		// through and the chat template then raises, so it must be dropped here.
		{"tencent", "hy3", "high", []string{`"reasoning_effort":"high"`, `"temperature":0.9`}, nil},
		{"tencent", "hy3", "medium", nil, []string{`"reasoning_effort"`}},
	}
	ps := NewProviders(Config{}, nil, &Client{provider: &Provider{ID: "local", Local: true}})
	for _, c := range cases {
		p := ps.byID[c.provider]
		cl := &Client{provider: p, model: c.model}
		b, err := cl.body(ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}, Thinking: c.thinking}, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range c.want {
			if !strings.Contains(string(b), w) {
				t.Errorf("%s/%s: body lacks %s: %s", c.provider, c.model, w, b)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(string(b), a) {
				t.Errorf("%s/%s: body should not contain %s: %s", c.provider, c.model, a, b)
			}
		}
	}
	local := &Client{provider: &Provider{ID: "local", Local: true, Dialect: "vllm"}, model: "Qwen/Qwen3-32B", temp: f64(0.2)}
	b, _ := local.body(ChatRequest{Thinking: "off"}, true)
	if !strings.Contains(string(b), `"enable_thinking":false`) || !strings.Contains(string(b), `"temperature":0.2`) {
		t.Errorf("local vllm body: %s", b)
	}
}

func TestProvidersSplit(t *testing.T) {
	ps := NewProviders(Config{}, nil, &Client{provider: &Provider{ID: "local", Local: true}})
	if p, m, ok := ps.Split("deepseek/deepseek-v4-pro"); !ok || p.ID != "deepseek" || m != "deepseek-v4-pro" {
		t.Fatal("provider ref not split")
	}
	if p, m, ok := ps.Split("openrouter/moonshotai/kimi-k2.6"); !ok || p.ID != "openrouter" || m != "moonshotai/kimi-k2.6" {
		t.Fatal("nested model id not preserved")
	}
	if _, m, ok := ps.Split("Qwen/Qwen3-Coder-480B"); ok || m != "Qwen/Qwen3-Coder-480B" {
		t.Fatal("HF-style local model name treated as a provider")
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := ps.Client("deepseek/deepseek-chat"); err == nil {
		t.Fatal("expected missing-key error")
	}
}

func TestPermissionEvaluate(t *testing.T) {
	rs := Ruleset{
		{"*", "*", Allow},
		{"run", "*", Ask},
		{"run", "git status *", Allow},
		{"run", "rm *", Deny},
		{"edit", "*.env", Deny},
	}
	cases := []struct {
		perm, pat string
		want      Action
	}{
		{"read", "x.go", Allow},
		{"run", "go test ./...", Ask},
		{"run", "git status", Allow},
		{"run", "git status --short", Allow},
		{"run", "rm -rf /", Deny},
		{"edit", "config/.env", Deny},
		{"edit", "main.go", Allow},
	}
	for _, c := range cases {
		if got := Evaluate(c.perm, c.pat, rs); got != c.want {
			t.Errorf("Evaluate(%s, %q) = %s, want %s", c.perm, c.pat, got, c.want)
		}
	}
	if !Disabled("task", Ruleset{{"*", "*", Allow}, {"task", "*", Deny}}) {
		t.Error("task should be disabled")
	}
	if Disabled("run", Ruleset{{"run", "*", Deny}, {"run", "ls *", Allow}}) {
		t.Error("run with a specific allow must stay offered")
	}
}

func TestPermissionConfigKeepsOrder(t *testing.T) {
	var fc FileConfig
	err := json.Unmarshal([]byte(`{"permission": {"run": {"*": "ask", "go test *": "allow", "go test ./danger/*": "deny"}, "web": "deny"}}`), &fc)
	if err != nil {
		t.Fatal(err)
	}
	rs := Ruleset(fc.Permission)
	if len(rs) != 4 || rs[2].Pattern != "go test ./danger/*" || rs[3].Permission != "web" {
		t.Fatalf("order lost: %+v", rs)
	}
	if Evaluate("run", "go test ./danger/x", rs) != Deny || Evaluate("run", "go test ./ok", rs) != Allow {
		t.Fatal("ordered precedence broken")
	}
}

func TestMarkdownAgentLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	dir := filepath.Join(root, ".lca", "agents")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte(`---
description: Reviews diffs  # trailing comment
mode: subagent
model: deepseek/deepseek-v4-pro
thinking: high
steps: 12
temperature: 0.3
permission:
  edit: deny
  run:
    "*": deny
    "git diff *": allow
---
You review code.
`), 0o644)
	os.WriteFile(filepath.Join(dir, "claude-style.md"), []byte("---\nname: scout\ndescription: finds things\ntools: Read, Grep, Glob\nmodel: sonnet\n---\nScout.\n"), 0o644)
	ps := NewProviders(Config{}, nil, &Client{provider: &Provider{ID: "local", Local: true}})
	agents, warns := loadAgents(root, filepath.Join(root, "nodir"), nil, ps)
	a := agents["reviewer"]
	if a == nil || a.Mode != "subagent" || a.Model != "deepseek/deepseek-v4-pro" || a.Steps != 12 || a.Thinking != "high" || *a.Temperature != 0.3 {
		t.Fatalf("bad agent: %+v", a)
	}
	if a.Description != "Reviews diffs" || a.Prompt != "You review code." {
		t.Fatalf("bad description/prompt: %q %q", a.Description, a.Prompt)
	}
	if Evaluate("run", "git diff HEAD", a.Rules) != Allow || Evaluate("run", "ls", a.Rules) != Deny || Evaluate("edit", "x", a.Rules) != Deny {
		t.Fatalf("bad rules: %+v", a.Rules)
	}
	s := agents["scout"]
	if s == nil || Evaluate("read", "x", s.Rules) != Allow || Evaluate("run", "ls", s.Rules) != Deny || s.Model != "" {
		t.Fatalf("claude-style agent: %+v", s)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "sonnet") {
		t.Fatalf("want a warning for the unroutable model alias, got %v", warns)
	}
}

func TestFuzzyReplace(t *testing.T) {
	content := "func a() {\n\tif x {\n\t\treturn 1\n\t}\n}\n"
	// indentation drift (spaces instead of tabs)
	got, strat, err := fuzzyReplace(content, "if x {\n    return 1\n}", "if y {\n\t\treturn 2\n\t}", false)
	if err != nil || !strings.Contains(got, "return 2") || strat == "exact" {
		t.Fatalf("line-trimmed fallback failed: %v %q %s", err, got, strat)
	}
	// ambiguous stays an error
	if _, _, err := fuzzyReplace("x\nx\n", "x", "y", false); err != errEditAmbiguous {
		t.Fatalf("want ambiguous, got %v", err)
	}
	if out, _, err := fuzzyReplace("x\nx\n", "x", "y", true); err != nil || out != "y\ny\n" {
		t.Fatalf("replace_all: %v %q", err, out)
	}
	if _, _, err := fuzzyReplace(content, "nothing like this", "z", false); err != errEditNotFound {
		t.Fatalf("want not found, got %v", err)
	}
	// block anchor: first/last lines match, middle slightly different
	block := "start()\nalpha(1)\nbeta(2)\nend()\n"
	if out, strat, err := fuzzyReplace(block, "start()\nalpha(1)\nbeta(3)\nend()", "done()", false); err != nil || out != "done()\n" {
		t.Fatalf("block anchor: %v %q %s", err, out, strat)
	}
}

func TestGlobFiles(t *testing.T) {
	root := t.TempDir()
	j, _ := NewJail(root, nil, false)
	for _, p := range []string{"a.go", "pkg/b.go", "pkg/sub/c_test.go", "node_modules/x.go", "web/d.ts", "web/e.tsx"} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte("x"), 0o644)
	}
	if got := globFiles(j, "**/*.go", ""); strings.Contains(got, "node_modules") || lineCount(got) != 3 {
		t.Fatalf("**/*.go: %q", got)
	}
	if got := globFiles(j, "*_test.go", ""); got != "pkg/sub/c_test.go" {
		t.Fatalf("basename glob: %q", got)
	}
	if got := globFiles(j, "web/*.{ts,tsx}", ""); lineCount(got) != 2 {
		t.Fatalf("brace glob: %q", got)
	}
	if got := grepTree(j, "x", ".", "*.ts"); !strings.Contains(got, "web/d.ts") || strings.Contains(got, ".go") {
		t.Fatalf("grep include: %q", got)
	}
}

func TestExpandTemplate(t *testing.T) {
	if got := expandTemplate("Review $1 focusing on $2", `main.go "error handling and tests"`); got != "Review main.go focusing on error handling and tests" {
		t.Fatalf("positional: %q", got)
	}
	if got := expandTemplate("Fix: $ARGUMENTS", "the flaky test"); got != "Fix: the flaky test" {
		t.Fatalf("ARGUMENTS: %q", got)
	}
	if got := expandTemplate("Summarize.", "extra words"); got != "Summarize.\n\nextra words" {
		t.Fatalf("append: %q", got)
	}
	if got := expandTemplate("$1 then rest: $2", "a b c d"); got != "a then rest: b c d" {
		t.Fatalf("last placeholder takes the rest: %q", got)
	}
}

func TestProfilesForFamilies(t *testing.T) {
	for id, fam := range map[string]string{
		"kimi-k2.6": "kimi", "glm-5.2": "glm", "Qwen/Qwen3-Coder-480B-A35B-Instruct": "qwen",
		"MiniMax-M3": "minimax", "hy3": "hunyuan", "hunyuan-t1-latest": "hunyuan", "accounts/fireworks/models/kimi-k2p6": "kimi",
	} {
		if p := lookupProfile(id); p.Family != fam || p.Context == 0 {
			t.Errorf("%s: got %+v, want family %s", id, p, fam)
		}
	}
	// deepseek-v4-pro is the exception that proves the rule: hosted-only, no open
	// weights, so no card and deliberately no window. It used to carry
	// V4.1-Flash's 1M as "card" on the strength of a routing DeepSeek reversed.
	if p := lookupProfile("deepseek-v4-pro"); p.Family != "deepseek" || p.Context != 0 {
		t.Errorf("deepseek-v4-pro must keep its family and no window: %+v", p)
	}
	if p := lookupProfile("kimi-k2.6"); p.Replay != "all" {
		t.Error("kimi thinking must replay reasoning")
	}
	// V4.1-Flash replays EVERY turn, not just the current one: with tools in the
	// request the API returns 400 when an earlier turn's reasoning_content is
	// missing. The older V3.x reasoners are the ones that only need the turn.
	if p := lookupProfile("deepseek-v4-flash"); p.Replay != "all" {
		t.Error("deepseek-v4.1-flash must replay reasoning on every step")
	}
	if p := lookupProfile("deepseek-v3.2"); p.Replay != "turn" {
		t.Error("deepseek-v3.2 must replay reasoning within the turn")
	}
}

func TestPermPathNormalization(t *testing.T) {
	root := t.TempDir()
	j, _ := NewJail(root, nil, false)
	rules := Ruleset{{"edit", "*", Deny}, {"edit", ".lca/plans/*", Allow}}
	for _, p := range []string{".lca/plans/x.md", "./.lca/plans/x.md", ".lca//plans/x.md", filepath.Join(j.Root, ".lca/plans/x.md")} {
		if got := Evaluate("edit", permPath(j, p), rules); got != Allow {
			t.Errorf("%q → %q: %s", p, permPath(j, p), got)
		}
	}
	if got := Evaluate("edit", permPath(j, ".lca/plans/../../main.go"), rules); got != Deny {
		t.Errorf("traversal out of the plans dir must not match: %s", got)
	}
}

func TestRepairTranscript(t *testing.T) {
	s := &Session{Msgs: []Message{
		{Role: "system"}, {Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{call("a", "glob", nil), call("b", "grep", nil)}},
		{Role: "tool", ToolCallID: "a", Content: "ok"},
		{Role: "user", Content: "next"},
	}}
	s.repairTranscript()
	if len(s.Msgs) != 6 || s.Msgs[4].ToolCallID != "b" || s.Msgs[5].Content != "next" {
		t.Fatalf("bad repair: %+v", s.Msgs)
	}
	s.repairTranscript()
	if len(s.Msgs) != 6 {
		t.Fatal("repair is not idempotent")
	}
}

// ── regressions from review ────────────────────────────────────────────────

func TestBackgroundStaleWakeDoesNotResendAssistantLast(t *testing.T) {
	releaseB := make(chan struct{})
	var badRequest atomic.Bool
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.last()["role"] == "assistant" {
			badRequest.Store(true)
		}
		if isExploreReq(req) {
			if p, _ := req.Messages[1]["content"].(string); p == "B" {
				<-releaseB
				return fakeReply{content: "result B"}
			}
			return fakeReply{content: "result A"}
		}
		content, _ := req.last()["content"].(string)
		switch {
		case strings.Contains(content, "result B"):
			return fakeReply{content: "all results in"}
		case strings.Contains(content, "result A"):
			close(releaseB)
			return fakeReply{content: "A arrived, waiting for B"}
		case req.last()["role"] == "tool":
			time.Sleep(100 * time.Millisecond) // A finishes meanwhile → delivered + wake token left
			return fakeReply{content: "started both"}
		}
		return fakeReply{calls: []ToolCall{
			call("a", "task", map[string]any{"agent": "explore", "description": "A", "prompt": "A", "background": true}),
			call("b", "task", map[string]any{"agent": "explore", "description": "B", "prompt": "B", "background": true}),
		}}
	})
	h := newHarness(t, fs.URL, "native", false)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	if badRequest.Load() {
		t.Fatal("a request ended with an assistant message (stale wake token)")
	}
	if last := h.sess.Msgs[len(h.sess.Msgs)-1]; last.Content != "all results in" {
		t.Fatalf("final: %+v", last)
	}
}

func TestResumeSameTaskTwiceIsRejected(t *testing.T) {
	gate := make(chan struct{})
	var childRuns atomic.Int32
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if isExploreReq(req) {
			if len(req.Messages) > 2 { // a resumed run
				childRuns.Add(1)
				<-gate
			}
			return fakeReply{content: "ok"}
		}
		tools := 0
		for _, m := range req.Messages {
			if m["role"] == "tool" {
				tools++
			}
		}
		switch tools {
		case 0:
			return fakeReply{calls: []ToolCall{call("t", "task", map[string]any{"agent": "explore", "description": "x", "prompt": "first"})}}
		case 1:
			go func() { time.Sleep(200 * time.Millisecond); close(gate) }()
			return fakeReply{calls: []ToolCall{
				call("r1", "task", map[string]any{"agent": "explore", "description": "x", "prompt": "again", "task_id": "t1"}),
				call("r2", "task", map[string]any{"agent": "explore", "description": "x", "prompt": "again", "task_id": "t1"}),
			}}
		}
		return fakeReply{content: "done"}
	})
	h := newHarness(t, fs.URL, "native", false)
	if err := h.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	if n := childRuns.Load(); n != 1 {
		t.Fatalf("resumed child ran %d times concurrently, want 1", n)
	}
}

func TestLastStepSendsNoToolsAndRunsNothing(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{calls: []ToolCall{call(fmt.Sprint("c", n), "glob", map[string]any{"pattern": fmt.Sprint("*", n)})}}
	})
	h := newHarness(t, fs.URL, "native", false)
	h.orch.cfg.MaxSteps = 3
	h.run(t, "go")
	rs := fs.reqs()
	if len(rs) != 3 || len(rs[2].Tools) != 0 {
		t.Fatalf("want 3 requests with no tools on the last, got %d (last tools %d)", len(rs), len(rs[len(rs)-1].Tools))
	}
	for _, m := range h.sess.Msgs {
		if m.Role == "tool" && m.ToolCallID == "c3" {
			t.Fatal("a tool from the final step was executed")
		}
	}
	h.sess.repairTranscript()
	if last := h.sess.Msgs[len(h.sess.Msgs)-1]; last.Role != "assistant" || len(last.ToolCalls) != 0 {
		t.Fatalf("last message should be a tool-less assistant reply: %+v", last)
	}
}

func TestGrepSkipsFilesNotAllowed(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{calls: []ToolCall{call("g", "grep", map[string]any{"pattern": "SECRET"})}}
		}
		return fakeReply{content: "done"}
	})
	h := newHarness(t, fs.URL, "native", false)
	os.WriteFile(filepath.Join(h.root, ".env"), []byte("SECRET=1\n"), 0o644)
	os.WriteFile(filepath.Join(h.root, "code.go"), []byte("// SECRET handling\n"), 0o644)
	h.run(t, "go")
	for _, m := range h.sess.Msgs {
		if m.Role == "tool" && strings.Contains(m.Content, ".env") {
			t.Fatalf("grep leaked an ask-protected file: %s", m.Content)
		}
	}
}

func TestPlanDenyCarriesToSubagents(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "x"} })
	h := newHarness(t, fs.URL, "native", true)
	if err := h.sess.SetAgent("plan"); err != nil {
		t.Fatal(err)
	}
	child, err := h.orch.newChild(h.sess, h.orch.agents["general"], "edit something")
	if err != nil {
		t.Fatal(err)
	}
	if got := Evaluate("edit", "main.go", child.rules()...); got != Deny {
		t.Fatalf("general under plan may edit: %s", got)
	}
}

func TestStreamToolCallQuirks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			// name repeated in every chunk, no index on a second call with its own id
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"glob","arguments":"{\"pattern\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"glob","arguments":"\"*.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"list_dir","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := &Client{http: srv.Client(), provider: &Provider{ID: "local", Local: true}, baseURL: srv.URL, model: "m"}
	res, err := c.Chat(context.Background(), ChatRequest{}, StreamSink{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 2 || res.ToolCalls[0].Function.Name != "glob" || res.ToolCalls[0].Function.Arguments != `{"pattern":"*.go"}` ||
		res.ToolCalls[1].Function.Name != "list_dir" {
		t.Fatalf("bad assembly: %+v", res.ToolCalls)
	}

	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"half a rep"}}]}`+"\n\n")
	}))
	defer cut.Close()
	c.baseURL = cut.URL
	res2, err := c.Chat(context.Background(), ChatRequest{}, StreamSink{})
	if err == nil || !res2.Started || classifyGW(err, res2.Started) != gwAfterFirstByte {
		t.Fatalf("a stream cut after the first byte must be classified as such, got %v started=%v", err, res2.Started)
	}
}

func TestUnsafeChainedCommandAsks(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "x"} })
	h := newHarness(t, fs.URL, "native", false)
	h.orch.userRules = Ruleset{{"run", "git status *", Allow}}
	h.orch.jl.Unsafe = true
	tc := &ToolCtx{Ctx: context.Background(), S: h.sess}
	if _, ok := tc.Ask("run", "git status", "RUN", ""); !ok {
		t.Fatal("plain allowed command should run")
	}
	if _, ok := tc.Ask("run", "git status; rm -rf ~", "RUN", ""); ok {
		t.Fatal("chained command slipped through an allow rule")
	}
}

// A slow prefill or a cold model must not look like a freeze: the request going
// out shows a live "waiting for <model>" line, which whatever arrives replaces.
func TestWaitingIndicator(t *testing.T) {
	pw := newProseWriter(false, false)
	pw.anim = false // no spinner goroutine in tests
	pw.begin("kimi-k3")
	if !pw.waiting || !pw.markerActive || pw.markerLabel != "waiting for kimi-k3" {
		t.Fatalf("no waiting marker: %+v", struct {
			W, A bool
			L    string
		}{pw.waiting, pw.markerActive, pw.markerLabel})
	}
	if s := pw.thinkStat(); strings.Contains(s, "tok") {
		t.Fatalf("no tokens to count while waiting: %q", s)
	}
	pw.feed("hello")
	if pw.waiting || pw.markerActive {
		t.Fatal("the first delta must clear the waiting marker")
	}
	pw.end()

	// raw mode streams verbatim: no marker at all
	raw := newProseWriter(true, false)
	raw.begin("kimi-k3")
	if raw.waiting {
		t.Fatal("raw mode must not draw a marker")
	}
}

// Interleaved-thinking models must get their own reasoning back on every step,
// whichever tool transport is in use — dropping it is what makes an agent lose
// its plan and start going in circles.
func TestReasoningReplayedOnBothTransports(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "q"},
		{Role: "assistant", Reasoning: "step 1 plan", ToolCalls: []ToolCall{call("x", "glob", nil)}},
		{Role: "tool", ToolCallID: "x", Content: "res"},
	}
	for _, transport := range []string{transportNative, transportText} {
		c := &Client{provider: &Provider{ID: "local", Local: true, Transport: transport}, model: "kimi-k3"}
		b, err := c.body(ChatRequest{Messages: msgs}, true)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "step 1 plan") {
			t.Fatalf("%s: reasoning was dropped from the history: %s", transport, b)
		}
	}
	// a server that rejects the field can be told to stop sending it
	c := &Client{provider: &Provider{ID: "local", Local: true, Transport: transportNative}, model: "kimi-k3", noReplay: true}
	b, _ := c.body(ChatRequest{Messages: msgs}, true)
	if strings.Contains(string(b), "step 1 plan") {
		t.Fatal("reasoning_replay: off must stop the replay")
	}
}

// Shell mode (sandbox: {shell: true}) must buy pipes without buying an escape
// from the allowlist: every command in the line is checked, including the ones
// after |, && and inside a loop body.
func TestShellSandboxChecksEverySegment(t *testing.T) {
	root := t.TempDir()
	j, _ := NewJail(root, []string{"echo", "grep", "wc"}, false)
	j.Shell = true
	ok := []string{
		"echo hi | grep hi",
		"echo hi | wc -l > out.txt",
		"for f in a b; do echo $f; done",
		"if echo a; then echo b; fi",
		"echo one && echo two; echo three",
	}
	for _, line := range ok {
		if err := j.CheckCommand(line); err != nil {
			t.Errorf("%q: %v", line, err)
		}
	}
	bad := map[string]string{
		"echo hi | curl http://x":                "allowlist",
		"echo hi && rm -rf /":                    "allowlist",
		"for f in a; do nc x; done":              "allowlist",
		"echo hi | torchrun train.py":            "scheduler",
		"echo hi; CUDA_VISIBLE_DEVICES=0 echo x": "GPU",
	}
	for line, want := range bad {
		err := j.CheckCommand(line)
		if err == nil {
			t.Errorf("%q was allowed", line)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v (want mention of %q)", line, err, want)
		}
	}
	// and the pipe actually runs
	out := runCommand(context.Background(), j, "echo hi | grep -c hi", 5*time.Second, nil)
	if strings.TrimSpace(out) != "1" {
		t.Fatalf("pipe did not run through sh: %q", out)
	}
	// without shell mode the same jail exec's directly: no shell, no pipe
	j.Shell = false
	if out := runCommand(context.Background(), j, "echo hi | grep -c hi", 5*time.Second, nil); !strings.Contains(out, "|") {
		t.Fatalf("non-shell mode should pass the pipe through as an argument: %q", out)
	}
}

// A failed command must tell the model the exit status, and say so even when
// the command printed nothing.
func TestCommandFailureReportsExitStatus(t *testing.T) {
	root := t.TempDir()
	j, _ := NewJail(root, []string{"sh", "ls"}, false)
	j.Shell = true
	if out := runCommand(context.Background(), j, "sh -c 'exit 3'", 5*time.Second, nil); !strings.Contains(out, "no output, exit status 3") {
		t.Fatalf("want a bare exit status, got %q", out)
	}
	out := runCommand(context.Background(), j, "ls nosuchfile", 5*time.Second, nil)
	if !strings.Contains(out, "nosuchfile") || !strings.Contains(out, "exit status 1") {
		t.Fatalf("want stderr and the status, got %q", out)
	}
}

// A tool call that doesn't parse is counted and its raw arguments go into the
// trace: without the model's own text a parse failure can't be reproduced.
func TestInvalidToolCallCountedAndRawTraced(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{content: "Reading.", calls: []ToolCall{{ID: "c1", Type: "function",
				Function: ToolFunction{Name: "read_file", Arguments: `{"path": "a.txt"`}}}}
		}
		return fakeReply{content: "Gave up."}
	})
	h := newHarness(t, fs.URL, "native", false)
	if err := h.run(t, "read a.txt"); err != nil {
		t.Fatal(err)
	}
	st := h.sess.stats
	if st.ToolCalls != 1 || st.InvalidCalls != 1 {
		t.Fatalf("stats: %+v (want 1 call, 1 invalid)", st)
	}
	if st.Turns != 2 {
		t.Fatalf("want 2 turns, got %d", st.Turns)
	}
	b, err := os.ReadFile(h.orch.tracer.Path)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(b)
	if !strings.Contains(trace, `"invalid":true`) || !strings.Contains(trace, `{\"path\": \"a.txt\"`) {
		t.Fatalf("trace lacks the invalid call or its raw arguments:\n%s", trace)
	}
	if !strings.Contains(trace, `"raw_reply":"Reading."`) {
		t.Fatalf("trace lacks the reply text:\n%s", trace)
	}
}

// sandbox: {shell: true} in roles.yaml reaches the jail and survives /role save.
func TestRolesSandboxShell(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".lca"), 0o755)
	path := filepath.Join(root, ".lca", "roles.yaml")
	os.WriteFile(path, []byte("sandbox:\n  allow: [go, git]\n  shell: true\n\nroles:\n  coder:\n    models: [m1]\n"), 0o644)
	rc, err := loadRoles(Config{Root: root, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Shell {
		t.Fatal("sandbox.shell not parsed")
	}
	if !strings.Contains(rc.YAML(), "shell: true") {
		t.Fatalf("shell mode lost on save:\n%s", rc.YAML())
	}
}

// Quoted operators are data, not command separators: the sandbox must not read
// awk's program or a quoted semicolon as a second command, and must still see
// through the unquoted ones.
func TestShellSegmentSplitRespectsQuotes(t *testing.T) {
	cases := map[string][]string{
		`grep 'a;b' f.txt`:                {`grep 'a;b' f.txt`},
		`awk '{print $1}' f.txt`:          {`awk '{print $1}' f.txt`},
		`echo "a && b"`:                   {`echo "a && b"`},
		`go build ./... && go test ./...`: {"go build ./... ", " go test ./..."},
		`cat f | grep x; echo done`:       {"cat f ", " grep x", " echo done"},
		`echo $(whoami)`:                  {"echo ", "whoami", ""},
	}
	for line, want := range cases {
		got := splitShellSegments(line)
		if len(got) != len(want) {
			t.Errorf("%q → %q, want %q", line, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q → %q, want %q", line, got, want)
				break
			}
		}
	}
	// a quoted GPU launcher is still caught where it is really a command
	j, _ := NewJail(t.TempDir(), []string{"echo", "awk", "grep", "cat", "go"}, false)
	j.Shell = true
	if err := j.CheckCommand(`echo "torchrun train.py"`); err != nil {
		t.Errorf("a quoted word is data: %v", err)
	}
	if err := j.CheckCommand(`echo x | torchrun train.py`); err == nil {
		t.Error("an unquoted GPU launcher after a pipe must be caught")
	}
}

// `tools: []` in an agent file means no tools at all, as it does in roles.yaml:
// a plain conversational agent. It used to split "[]" on commas into one tool
// named "[]", so nothing was denied and the agent still asked to run commands.
func TestAgentWithEmptyToolListHasNoTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	dir := filepath.Join(root, ".lca", "agents")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "chat.md"),
		[]byte("---\ndescription: plain conversation\nmode: primary\ntools: []\n---\nJust talk.\n"), 0o644)
	ps := NewProviders(Config{}, nil, &Client{provider: &Provider{ID: "local", Local: true}})
	agents, _ := loadAgents(root, filepath.Join(root, "nodir"), nil, ps)
	chat := agents["chat"]
	if chat == nil {
		t.Fatal("chat agent not loaded")
	}
	for _, name := range []string{"run_command", "edit", "write", "read_file"} {
		if !Disabled(permissionOf(name), chat.Rules) {
			t.Errorf("%s is still available to a tools: [] agent", name)
		}
	}
}

// An interrupt must be written into the transcript, not only printed. Ctrl-C used
// to leave the model's own "I'll continue reading the files" and its todo list as
// the last thing in the conversation, so the operator's next message — even
// "hello" — made it resume the abandoned work, on whatever endpoint was current
// by then. That burned a production model's capacity for real.
func TestInterruptIsWrittenIntoTheTranscript(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "I'll read the rest of the files now.",
			calls: []ToolCall{call("c1", "run_command", map[string]any{"command": "sleep 5"})}}
	})
	h := newHarness(t, fs.URL, "native", true)
	h.orch.jl.Allowed = append(h.orch.jl.Allowed, "sleep")
	h.orch.jl, _ = NewJail(h.root, append(h.orch.jl.Allowed, "sleep"), false)

	h.sess.Msgs = append(h.sess.Msgs, Message{Role: "user", Content: "read everything"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel() // the operator's Ctrl-C
	}()
	if err := h.sess.Run(ctx); err == nil {
		t.Fatal("an interrupted run returns context.Canceled")
	}

	last := h.sess.Msgs[len(h.sess.Msgs)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "ABANDONED") {
		t.Fatalf("the transcript does not record the interrupt; it ends with %s: %q",
			last.Role, truncate(last.Content, 80))
	}
	// and it tells the model the three ways "continue anyway" happens
	for _, want := range []string{"plan", "todo", "tool calls"} {
		if !strings.Contains(last.Content, want) {
			t.Errorf("the note does not mention the %s", want)
		}
	}
}

// A working directory that disappeared under a running session ends the turn with
// one message. It used to be rediscovered by every tool the model tried: read,
// list, then `ls`, then `go version`, each failing with its own ENOENT.
func TestVanishedRootEndsTheTurnOnce(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "project")
	os.MkdirAll(gone, 0o755)
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{content: "looking", calls: []ToolCall{
				call("c1", "read_file", map[string]any{"path": "a.txt"}),
				call("c2", "list_dir", map[string]any{"path": "."}),
			}}
		}
		return fakeReply{content: "done"}
	})
	h := newHarness(t, fs.URL, "native", true)
	h.orch.jl.Root = gone // the session's tree…
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	} // …and it is gone

	h.sess.Msgs = append(h.sess.Msgs, Message{Role: "user", Content: "read a.txt"})
	if err := h.sess.Run(context.Background()); err != nil {
		t.Fatalf("the turn should end cleanly, not error: %v", err)
	}
	var told int
	for _, m := range h.sess.Msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "no longer exists") {
			told++
		}
	}
	if told == 0 {
		t.Fatal("the model was never told the directory is gone")
	}
	if n := len(fs.reqs()); n > 2 {
		t.Errorf("the turn kept going: %d requests, want the turn to stop", n)
	}
}

// Ctrl-C at an approval question means "stop", not "kill lca" and not "no to this
// one". The prompt runs with the ordinary terminal discipline so the answer is
// readable and echoed, which leaves ISIG live — and before this an unhandled
// SIGINT ended the process where the operator only wanted to abandon a command.
func TestCtrlCAtTheApprovalDoorEndsTheTurn(t *testing.T) {
	ap := NewApprover(newStringInput(""))
	ap.interrupted.Store(true) // as the signal handler does
	if !ap.TakeInterrupt() {
		t.Fatal("the interrupt must be readable by the loop")
	}
	if ap.TakeInterrupt() {
		t.Fatal("and readable exactly once, or every later denial looks like a Ctrl-C")
	}
}

// Switching endpoints carries the whole conversation to the new deployment. When
// it holds an abandoned turn, the operator has to be told: this is how a stopped
// task got resumed on a different production model.
func TestEndpointSwitchWarnsAboutAnAbandonedTurn(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", true)
	r := &Repl{cfg: h.orch.cfg, orch: h.orch, sess: h.sess, local: h.orch.providers.local, in: newStringInput("")}

	h.sess.Msgs = append(h.sess.Msgs,
		Message{Role: "user", Content: "read everything"},
		Message{Role: "assistant", Content: "I'll continue reading the files."},
		Message{Role: "user", Content: interruptNote})
	out := stripANSI(captureStdout(t, func() { r.noteCarriedConversation(fs.URL) }))
	if !strings.Contains(out, "conversation moves with you") {
		t.Errorf("the operator is not told the transcript travels:\n%s", out)
	}
	if !strings.Contains(out, "interrupted") {
		t.Errorf("the abandoned turn is not mentioned:\n%s", out)
	}

	// A message of their own after the interrupt means they moved on: no warning.
	h.sess.Msgs = append(h.sess.Msgs, Message{Role: "user", Content: "now do something else"})
	out = stripANSI(captureStdout(t, func() { r.noteCarriedConversation(fs.URL) }))
	if strings.Contains(out, "interrupted") {
		t.Errorf("stale warning after the operator moved on:\n%s", out)
	}
}
