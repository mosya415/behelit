package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Message is one transcript entry. It doubles as the on-disk transcript format,
// so it carries a little metadata the API never sees (Tool/Path/Agent); wire()
// projects it to the chat-completions shape for a given model.
//
// Two tool transports share it:
//   - text:   tool calls are tags inside Content; results come back as a user
//     message whose content is <tool_result …> blocks (protocol.go).
//   - native: ToolCalls on the assistant message; one role "tool" message per
//     call with ToolCallID set.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`

	Tool  string `json:"tool,omitempty"`  // tool name of a native result (metadata)
	Path  string `json:"path,omitempty"`  // path argument of a native result (metadata)
	Agent string `json:"agent,omitempty"` // agent that produced an assistant message (metadata)
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// wireMessage is exactly what goes over the wire — no transcript metadata.
type wireMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// wire converts the transcript for one request. Reasoning is replayed only
// where the model family needs it (models.go): "all" keeps every assistant's
// reasoning_content (Kimi, GLM preserved thinking), "turn" keeps it only after
// the last real user message (DeepSeek, Hunyuan: required inside a tool-call
// chain, dropped between turns), "" never sends it (Qwen, MiniMax inline <think>).
func wire(msgs []Message, replay string) []wireMessage {
	lastUser := -1
	if replay == "turn" {
		lastUser = lastUserTurn(msgs)
	}
	out := make([]wireMessage, 0, len(msgs))
	for i, m := range msgs {
		w := wireMessage{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
		if m.Role == "assistant" && m.Reasoning != "" {
			switch replay {
			case "all":
				w.Reasoning = m.Reasoning
			case "turn":
				if i > lastUser {
					w.Reasoning = m.Reasoning
				}
			}
		}
		out = append(out, w)
	}
	return out
}

// dropFinalReasoning blanks the reasoning on the LAST assistant message, for a
// continuation against SGLang: there, only a plain-string assistant message with
// no tool calls and no reasoning content can be continued — anything else renders
// as a closed historical turn, the model starts a NEW assistant turn, and the
// partial text we asked it to extend is orphaned. No error, no signal, which is
// why it has to be fixed rather than hinted.
//
// Earlier turns keep theirs: the interleaved-thinking models are trained with
// their own reasoning in the context, and this is a statement about the one
// message being continued. It is also only bytes AFTER the cached prefix — the
// last message changes every step anyway — so no KV hit is lost for it.
//
// It lives beside wire rather than inside it on purpose: wire's signature is the
// transcript projection every other caller and test reads, and this is one
// caller's concern.
func dropFinalReasoning(ws []wireMessage) {
	for i := len(ws) - 1; i >= 0; i-- {
		if ws[i].Role == "assistant" {
			ws[i].Reasoning = ""
			return
		}
	}
}

// ToolSchema is a native function-calling tool definition.
type ToolSchema struct {
	Type     string         `json:"type"`
	Function ToolSchemaFunc `json:"function"`
}

type ToolSchemaFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ChatRequest is one model call, independent of transport and provider.
type ChatRequest struct {
	Messages      []Message
	Tools         []ToolSchema // native transport only
	Temperature   *float64     // nil → profile / provider default
	TopP          *float64
	TopK          *int
	Thinking      string // "", on, off, low, medium, high, max
	ContinueFinal bool   // vLLM/SGLang: extend the final assistant message verbatim
	MaxTokens     int    // 0 → client default
	Headers       map[string]string
}

// StreamSink receives display deltas; any field may be nil.
type StreamSink struct {
	Content   func(string)
	Reasoning func(string)
	ToolArgs  func(name string) // a native tool call's arguments are streaming
	Discard   func()            // the partial reply shown so far is void (the turn is being repeated)
}

// ChatResult is the assembled reply of one streamed call.
type ChatResult struct {
	Content   string
	Reasoning string
	ToolCalls []ToolCall
	Finish    string
	Usage     Usage
	Started   bool // at least one byte of the response stream arrived
}

// Usage is the token accounting + timing for one streamed completion. Cached is
// how many prompt tokens the server served from its KV prefix cache (0 if the
// server doesn't report it); it's the payoff of keeping the prefix byte-stable.
type Usage struct {
	PromptTokens     int
	CachedTokens     int
	CompletionTokens int
	TTFT             time.Duration // time to first token
	GenDur           time.Duration // first token → last token
}

// CacheHitPct is the fraction of the prompt served from the KV prefix cache.
func (u Usage) CacheHitPct() int {
	if u.PromptTokens <= 0 {
		return 0
	}
	return u.CachedTokens * 100 / u.PromptTokens
}

// TokPerSec is decode throughput over the generation window.
func (u Usage) TokPerSec() int {
	if u.GenDur <= 0 || u.CompletionTokens <= 0 {
		return 0
	}
	return int(float64(u.CompletionTokens) / u.GenDur.Seconds())
}

// APIError is a non-2xx response, kept structured so retry and overflow
// detection can look at the status, body and Retry-After.
type APIError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
	Endpoint   string
	Type       string // error.type from the JSON body (berserk-gw: berserk_gw_overload, …)
	// Envelope is the SHAPE the body arrived in: "wrapped" is {"error":{…}} (vLLM,
	// or a gateway that re-wrapped), "flat" is {"object":"error","message":…,"code":…}
	// (SGLang's own). It is doctor corroboration only and NEVER a source for
	// Client.engine: a heuristic that is harmless as a label becomes load-bearing
	// the moment a request field depends on it.
	Envelope string
	Overload bool   // X-Berserk-Overload: the model is up but at capacity
	State    string // X-Berserk-State: paused | drained (admin-imposed, not serving)
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("endpoint returned %d: %s", e.Status, truncate(e.Body, 500))
	if e.Status == http.StatusNotFound {
		msg += "  (check the endpoint URL includes the right base path, e.g. .../v1)"
	}
	return msg
}

// body builds the JSON request for this client's provider and model family.
func (c *Client) body(req ChatRequest, stream bool) ([]byte, error) {
	prof := c.Profile()
	// Interleaved-thinking models (Kimi, GLM, DeepSeek) are trained with their
	// own reasoning from earlier steps in the context. Dropping it — what a
	// plain OpenAI client does — makes the agent lose its plan on every step
	// and look like it is going in circles, so it is replayed whatever the tool
	// transport is. replay: off in roles.yaml turns it off for a server that
	// rejects the field.
	replay := prof.Replay
	if c.noReplay {
		replay = ""
	}
	msgs := wire(req.Messages, replay)
	// Source: serving_chat.py#L698-700 (sgl-project/sglang, v0.5.21 — byte-identical
	// on main) — only a plain-string assistant message with no tool calls and no
	// reasoning content can be continued; anything else renders as a closed
	// historical turn. vLLM's behaviour here is not sourced, so vLLM and UNKNOWN
	// keep today's body.
	if req.ContinueFinal && c.engine == engineSGLang {
		dropFinalReasoning(msgs)
	}
	b := map[string]any{
		"model":    c.model,
		"messages": msgs,
		"stream":   stream,
	}
	if stream {
		b["stream_options"] = map[string]any{"include_usage": true}
	}

	// Sampling: what the caller set (role / per-model config) > the family
	// profile's vendor numbers > nothing at all, so the model's own defaults
	// apply. A reasoning model at temperature 0 repeats itself, so we never
	// invent a value.
	//
	// A profile's numbers can be the self-host's only (SampleLocalOnly): Moonshot
	// fixes K3's temperature/top_p server-side and documents omitting them, and
	// the card's agentic top_p is not even the value it fixes — so against a
	// hosted API the card is not evidence about that deployment and the field is
	// left out, exactly as top_k is. What the operator set still goes: that is
	// their instruction, not our guess.
	card := !prof.SampleLocalOnly || c.provider.Local
	switch {
	case req.Temperature != nil:
		b["temperature"] = *req.Temperature
	case prof.Temperature != nil && card:
		b["temperature"] = *prof.Temperature
	case c.temp != nil:
		b["temperature"] = *c.temp
	}
	switch {
	case req.TopP != nil:
		b["top_p"] = *req.TopP
	case prof.TopP != nil && card:
		b["top_p"] = *prof.TopP
	}
	switch {
	case req.TopK != nil: // configured explicitly for this model
		b["top_k"] = *req.TopK
	case prof.TopK > 0 && c.provider.Local:
		// Non-standard but accepted by both: vLLM's own field, and SGLang declares
		// top_k: Optional[int] = None and maps it into its sampling params. A hosted
		// API may refuse it, which is what the gate is for.
		b["top_k"] = prof.TopK
	}

	// One place decides the reply budget, and /model reads it back from there:
	// a number this client rewrites has to be as reportable as one it copies.
	if maxTok, _ := c.replyCeiling(req.MaxTokens); maxTok > 0 {
		b["max_tokens"] = maxTok
	}

	if len(req.Tools) > 0 {
		b["tools"] = req.Tools
	}
	if req.ContinueFinal && c.provider.Local {
		b["continue_final_message"] = true
		// Source: protocol.py#L933 declares continue_final_message, and the whole
		// non-OpenAI request block (protocol.py#L923-978) has no add_generation_prompt
		// — serving_chat.py#L1792-1799 passes add_generation_prompt=True to the
		// template as a literal instead (sgl-project/sglang, v0.5.21, byte-identical
		// on main). So on SGLang the field is a silently dropped unknown key, and the
		// engine suppresses the generation prompt from continue_final_message itself.
		// On vLLM it is load-bearing, and on an engine nobody has named we keep
		// today's pair: the status quo is not a new guess, and doctor says which
		// engine it is going to.
		if c.engine != engineSGLang {
			b["add_generation_prompt"] = false
		}
	}
	for k, v := range thinkingParams(c.provider, c.model, prof, req.Thinking, replay) {
		// template_kwargs: off in roles.yaml. A chat template is a property of the
		// checkpoint AS SERVED: a deployment built without the one the card
		// describes rejects every kwarg we send, so the model 400s on every turn
		// and the operator sees "it cannot print". The top-level fields still go —
		// they are the engine's own API, not the template's — and only the
		// template's bag is dropped.
		if c.noKwargs && k == "chat_template_kwargs" {
			continue
		}
		b[k] = v
	}
	for k, v := range c.provider.Extra {
		b[k] = v
	}
	return json.Marshal(b)
}

func (c *Client) newRequest(ctx context.Context, body []byte, stream bool, headers map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range c.provider.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"` // OpenRouter / some gateways
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"` // DeepSeek
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat streams one completion over SSE, reporting deltas to sink, and returns
// the assembled reply. Native tool-call deltas are accumulated by index. The
// tool protocol only ever sees the assembled result, so streaming is purely a
// UX layer.
func (c *Client) Chat(ctx context.Context, req ChatRequest, sink StreamSink) (ChatResult, error) {
	var res ChatResult
	body, err := c.body(req, true)
	if err != nil {
		return res, err
	}
	hreq, err := c.newRequest(ctx, body, true, req.Headers)
	if err != nil {
		return res, err
	}

	start := time.Now()
	resp, err := c.http.Do(hreq)
	if err != nil {
		return res, fmt.Errorf("request to %s failed: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		ae := &APIError{Status: resp.StatusCode, Body: string(raw), RetryAfter: retryAfter(resp.Header), Endpoint: c.baseURL,
			Overload: resp.Header.Get("X-Berserk-Overload") != "", State: resp.Header.Get("X-Berserk-State")}
		ae.Type, ae.Envelope = errorType(raw)
		return res, ae
	}

	type pending struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*pending{}
	slot := map[int]int{} // server index → our slot (a new id at a reused index opens a new slot)
	nextSlot := 0
	gotDone := false
	reader := bufio.NewReaderSize(resp.Body, 1<<16)
	var content, reasoning strings.Builder
	var firstTok time.Time
	deltas := 0
	mark := func() {
		if firstTok.IsZero() {
			firstTok = time.Now()
		}
		deltas++
	}
	for {
		line, rerr := reader.ReadString('\n')
		if line != "" {
			res.Started = true
		}
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "data:") {
			data := strings.TrimSpace(s[len("data:"):])
			if data == "[DONE]" {
				gotDone = true
				break
			}
			var chunk streamChunk
			if json.Unmarshal([]byte(data), &chunk) == nil {
				if chunk.Error != nil && chunk.Error.Message != "" {
					return c.finish(res, &content, &reasoning, nil, start, firstTok, deltas),
						&APIError{Status: 500, Body: chunk.Error.Message, Endpoint: c.baseURL}
				}
				if len(chunk.Choices) > 0 {
					d := chunk.Choices[0].Delta
					if r := d.ReasoningContent + d.Reasoning; r != "" {
						mark()
						reasoning.WriteString(r)
						if sink.Reasoning != nil {
							sink.Reasoning(r)
						}
					}
					if d.Content != "" {
						mark()
						content.WriteString(d.Content)
						if sink.Content != nil {
							sink.Content(d.Content)
						}
					}
					for _, tc := range d.ToolCalls {
						mark()
						k, seen := slot[tc.Index]
						// Servers that omit index (→ 0) but send distinct ids
						// for distinct calls get one slot per id.
						if !seen || (tc.ID != "" && calls[k].id != "" && calls[k].id != tc.ID) {
							k = nextSlot
							nextSlot++
							slot[tc.Index] = k
							calls[k] = &pending{}
						}
						p := calls[k]
						if tc.ID != "" {
							p.id = tc.ID
						}
						// Some servers repeat the full name in every chunk.
						if n := tc.Function.Name; n != "" && !strings.HasSuffix(p.name, n) {
							p.name += n
						}
						p.args.WriteString(tc.Function.Arguments)
						if sink.ToolArgs != nil {
							sink.ToolArgs(p.name)
						}
					}
					if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
						res.Finish = *fr
					}
				}
				if u := chunk.Usage; u != nil {
					res.Usage.PromptTokens = u.PromptTokens
					res.Usage.CompletionTokens = u.CompletionTokens
					if u.PromptTokensDetails != nil {
						res.Usage.CachedTokens = u.PromptTokensDetails.CachedTokens
					}
					if u.PromptCacheHitTokens > 0 {
						res.Usage.CachedTokens = u.PromptCacheHitTokens
					}
				}
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				return c.finish(res, &content, &reasoning, nil, start, firstTok, deltas), rerr
			}
			break
		}
	}
	// A stream that just stops — no [DONE], no finish reason — was cut off
	// (proxy timeout, dropped connection): don't treat a partial reply or a
	// half-streamed tool call as complete.
	if !gotDone && res.Finish == "" {
		// Wrapped in a sentinel, because this is the one transport failure with no
		// net.Error left to inspect — a [DONE] that never came looks like a clean end
		// of file — and an unattended caller has to be able to tell it from a model
		// that failed a task. See errCutStream.
		return c.finish(res, &content, &reasoning, nil, start, firstTok, deltas),
			fmt.Errorf("stream from %s ended unexpectedly: %w", c.baseURL, errCutStream)
	}
	// Flatten accumulated tool calls in index order.
	var idx []int
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var out []ToolCall
	for n, i := range idx {
		p := calls[i]
		if p.name == "" {
			continue
		}
		id := p.id
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano()%1e6, n)
		}
		args := strings.TrimSpace(p.args.String())
		if args == "" {
			args = "{}"
		}
		out = append(out, ToolCall{ID: id, Type: "function", Function: ToolFunction{Name: p.name, Arguments: args}})
	}
	return c.finish(res, &content, &reasoning, out, start, firstTok, deltas), nil
}

func (c *Client) finish(res ChatResult, content, reasoning *strings.Builder, calls []ToolCall, start, firstTok time.Time, deltas int) ChatResult {
	res.Content = content.String()
	res.Reasoning = reasoning.String()
	res.ToolCalls = calls
	// Some servers finish "stop" while emitting tool calls — normalize.
	if len(calls) > 0 && (res.Finish == "stop" || res.Finish == "") {
		res.Finish = "tool_calls"
	}
	if !firstTok.IsZero() {
		res.Usage.TTFT = firstTok.Sub(start)
		res.Usage.GenDur = time.Since(firstTok)
	}
	if res.Usage.CompletionTokens == 0 {
		res.Usage.CompletionTokens = deltas // fallback when the server omits usage
	}
	return res
}

func retryAfter(h http.Header) time.Duration {
	if v := h.Get("retry-after-ms"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Millisecond))
		}
	}
	if v := h.Get("retry-after"); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
			return time.Duration(s * float64(time.Second))
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 0
}

// errorType reads the error type out of a body in either envelope, because only
// one of the two was ever parsed: SGLang answers FLAT
// ({"object":"error","message":…,"type":"BadRequest","param":null,"code":400}) and
// a gateway that passes the upstream body through verbatim hands that straight to
// us, where a reader looking only for {"error":{"type"}} found an empty string.
//
// Note for anyone matching on the result: a chat error's type can be "Bad
// Request" (the schema), "BadRequest" (a handler ValueError), "BadRequestError"
// (request validation), "InternalServerError", or a bare status string — so match
// on the status plus the message text, never on this.
func errorType(raw []byte) (typ, envelope string) {
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil {
		return "", "" // not JSON at all: a proxy's HTML page, say
	}
	if inner, ok := keys["error"]; ok {
		var wrapped struct {
			Type string `json:"type"`
		}
		json.Unmarshal(inner, &wrapped) // a string-valued "error" leaves the type empty
		return wrapped.Type, "wrapped"
	}
	var flat struct {
		Object string `json:"object"`
		Type   string `json:"type"`
	}
	if json.Unmarshal(raw, &flat) == nil && flat.Object == "error" {
		return flat.Type, "flat"
	}
	return "", ""
}

// Context-overflow detection (ported from opencode's provider-error.ts): the
// phrasing each server uses when a prompt exceeds the window.
var reOverflow = regexp.MustCompile(`(?i)prompt is too long|request_too_large|input is too long|exceeds the context window|` +
	`maximum context length|context length is only|exceeds the maximum allowed input length|longer than the model'?s context length|` +
	`exceeds the available context size|greater than the context length|context window exceeds limit|exceeded model token limit|` +
	`context[_ ]length[_ ]exceeded|model_context_window_exceeded|reduce the length of the messages|maximum prompt length|` +
	`tokens in request more than max tokens allowed|input token count.*exceeds|token limit exceeded|too many tokens|` +
	`range of input length|input length.*exceeds`)

// reStatedWindow pulls the real window out of an overflow refusal. Every engine
// says it: vLLM "This model's maximum context length is 131072 tokens. However,
// you requested …", SGLang "the input length … is longer than the model's context
// length 131072", and the OpenAI-compatible wrappers echo one of the two. It is
// the cheapest truth available about a deployment that does not publish
// max_model_len — and it arrives exactly when we most need it.
var reStatedWindow = regexp.MustCompile(`(?i)(?:maximum context length|context length|max_model_len|context window|maximum prompt length)` +
	`[^0-9]{0,24}?(\d{3,9})`)

// statedWindow is the window an error names, or 0. It refuses a number that is
// smaller than the prompt it complains about would allow us to believe: a
// "requested 1000032 tokens" is not a window.
func statedWindow(err error) int {
	if err == nil {
		return 0
	}
	body := err.Error()
	var ae *APIError
	if errors.As(err, &ae) && ae.Body != "" {
		body = ae.Body
	}
	m := reStatedWindow.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	n, e := strconv.Atoi(m[1])
	if e != nil || n < 1024 || n > 100_000_000 {
		return 0
	}
	return n
}

func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.Status == http.StatusRequestEntityTooLarge {
			return true
		}
		if strings.Contains(strings.ToLower(ae.Body), "rate limit") {
			return false
		}
		return reOverflow.MatchString(ae.Body)
	}
	return reOverflow.MatchString(err.Error())
}

// truncate bounds a string to n columns for a MACHINE path: a trace record, a
// stored run detail, an error message, a child session's name, a tool-call label.
// Its marker is the literal "…" and deliberately not the theme's, because these
// bytes are written to files and sent to models and may not depend on the
// operator's locale — LC_ALL=C must not change what a trace says.
//
// It cuts on a rune boundary, which a byte slice did not: a cut inside a UTF-8
// sequence writes an invalid record. A display cut wants ellipsize() instead,
// which measures the same way and uses the tier's own marker so an ASCII row does
// not get a UTF-8 ellipsis in it.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
