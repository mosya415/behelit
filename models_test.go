package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The six models the gateway serves, in every spelling a self-host or a vendor
// path can produce, plus the near neighbours whose numbers are NOT
// interchangeable with theirs. This table is the reason rule order stopped being
// load-bearing: before it, "glm-5.3" silently took the glm-5 rule's 200k window
// and "glm5.3" took the generic glm rule's 128k, and nothing failed.
func TestProfileResolvesEverySixGatewaySpelling(t *testing.T) {
	for _, tc := range []struct{ id, key string }{
		// kimi-k3
		{"kimi-k3", "kimi-k3"},
		{"moonshotai/Kimi-K3", "kimi-k3"},
		{"moonshotai/kimi-k3", "kimi-k3"},
		{"kimi-k3-fp8", "kimi-k3"},
		{"kimi-k3-bf16", "kimi-k3"},
		// deepseek-v4.1-flash and its accepted aliases
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"deepseek-v4p1-flash", "deepseek-v4.1-flash"},
		{"deepseek-flash", "deepseek-v4.1-flash"},
		{"deepseek-v4-flash", "deepseek-v4.1-flash"},
		{"deepseek-v4-flash-vision-exp", "deepseek-v4.1-flash"},
		{"deepseek-ai/DeepSeek-V4.1-Flash", "deepseek-v4.1-flash"},
		// glm-5.3 — both gateway spellings, one profile
		{"glm-5.3", "glm-5.3"},
		{"glm5.3", "glm-5.3"},
		{"GLM-5.3", "glm-5.3"},
		{"zai-org/GLM-5.3", "glm-5.3"},
		{"zai-org/GLM-5.3-BF16", "glm-5.3"},
		{"glm-5.3[1m]", "glm-5.3"},
		// qwen3.6, size-suffixed or not
		{"qwen3.6", "qwen3.6"},
		{"qwen3.6-27b", "qwen3.6"},
		{"qwen3.6-35b-a3b", "qwen3.6"},
		{"Qwen/Qwen3.6-27B", "qwen3.6"},
		{"Qwen/Qwen3.6-27B-FP8", "qwen3.6"},
		{"Qwen/Qwen3.6-35B-A3B", "qwen3.6"},
		{"Qwen/Qwen3.6-35B-A3B-FP8", "qwen3.6"},
		// MiniMax-M3
		{"MiniMax-M3", "minimax-m3"},
		{"minimax-m3", "minimax-m3"},
		{"MiniMaxAI/MiniMax-M3", "minimax-m3"},
		{"MiniMaxAI/MiniMax-M3-MXFP8", "minimax-m3"},
		{"amd/MiniMax-M3-MXFP4", "minimax-m3"},
		{"nvidia/MiniMax-M3-NVFP4", "minimax-m3"},
		{"minimax/minimax-m3", "minimax-m3"},
		// hy3
		{"hy3", "hy3"},
		{"hy3-fp8", "hy3"},
		{"hy_v3", "hy3"},
		{"tencent/Hy3", "hy3"},
		{"tencent/Hy3-FP8", "hy3"},
		{"Tencent-Hunyuan/Hy3", "hy3"},
		{"RedHatAI/Hy3-NVFP4-FP8", "hy3"},
		// hy3 IS Hunyuan 3, so a gateway spelling it out must not land on the
		// Hunyuan-2-era generic rule (half the window, no switch, no replay).
		{"hunyuan-3", "hy3"},
		{"Hunyuan-3", "hy3"},
		{"hunyuan-v3", "hy3"},
	} {
		if got := lookupProfile(tc.id); got.Key != tc.key {
			t.Errorf("%s resolved to %q, want %q (normalised %q)", tc.id, got.Key, tc.key, normalizeModelID(tc.id))
		}
	}
}

// The near neighbours. Each of these shares a prefix with one of the six and has
// numbers that are not interchangeable with it, so taking the served model's
// profile would be the exact failure this work exists to stop.
func TestProfileNearNeighboursDoNotCollide(t *testing.T) {
	for _, id := range []string{
		"kimi-k2", "kimi-k2.5", "kimi-k2.6", "kimi-k2.7", "kimi-k2.7-code", "kimi-k2-thinking",
		"glm-5", "glm-5.2", "glm-5.3-flash", "glm-5.3-flashx", "glm-4.7",
		"qwen3.5", "qwen3.7", "qwen3.8", "qwen3.6-plus", "qwen3.6-flash", "qwen3.6-max-preview",
		"MiniMax-M3.1-Flash-Preview", "minimax-m2.5",
		"deepseek-v4-pro", "deepseek-v3.2", "deepseek-reasoner",
		"hunyuan-t1-latest", "alchy3x", "hy3-preview",
	} {
		got := lookupProfile(id)
		for _, six := range []string{"kimi-k3", "deepseek-v4.1-flash", "glm-5.3", "qwen3.6", "minimax-m3", "hy3"} {
			if got.Key == six {
				t.Errorf("%s took %s's profile (normalised %q)", id, six, normalizeModelID(id))
			}
		}
	}
	// Two that must reach nothing at all: unset beats wrong, and hy3 must stop
	// being a three-character substring that Contains finds anywhere.
	for _, id := range []string{"alchy3x", "hy3-preview"} {
		if p := lookupProfile(id); p.Family != "" {
			t.Errorf("%s matched family %q; an id nobody enumerated must match nothing", id, p.Family)
		}
	}
	// The near neighbours that DO keep a family entry must keep their own numbers.
	if p := lookupProfile("kimi-k2.6"); p.Family != "kimi" || p.Context != 262_144 {
		t.Errorf("kimi-k2.6: %+v", p)
	}
	if p := lookupProfile("glm-5"); p.Context != 200_000 {
		t.Errorf("glm-5 must keep its own window, got %d", p.Context)
	}
	// Checking against the six was not enough: a token matches at token
	// boundaries, so "deepseek-v4" sat inside "deepseek-v4.1-pro" and handed it
	// V4-Pro's rule — and "glm-5" sits inside every future glm-5.x.
	for id, notKey := range map[string]string{
		"deepseek-v4.1-pro":   "deepseek-v4",
		"deepseek-v4.1":       "deepseek-v4",
		"deepseek-v4.2-flash": "deepseek-v4",
		"deepseek-v5":         "deepseek",
		"glm-5.4":             "glm-5",
		"glm-5.5":             "glm-5",
		"kimi-k4":             "kimi",
		"kimi-k5":             "kimi",
	} {
		if got := lookupProfile(id); got.Key == notKey {
			t.Errorf("%s took %q's rule (normalised %q)", id, notKey, normalizeModelID(id))
		}
	}
}

func TestNormalizeModelID(t *testing.T) {
	for in, want := range map[string]string{
		"glm5.3":                       "glm-5.3",
		"glm-5.3":                      "glm-5.3",
		"zai-org/GLM-5.3-BF16":         "glm-5.3",
		"glm-5.3[1m]":                  "glm-5.3",
		"hy3":                          "hy-3",
		"hy_v3":                        "hy-v-3",
		"deepseek-v4p1-flash":          "deepseek-v-4.1-flash",
		"accounts/fireworks/kimi-k2p6": "kimi-k-2.6",
		"MiniMaxAI/MiniMax-M3-MXFP8":   "minimax-m-3",
	} {
		if got := normalizeModelID(in); got != want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

// The sourced values, spot-checked where the audit found the old table wrong.
// These are the numbers a wrong one silently truncates or overflows behind.
func TestSixProfilesCarrySourcedValues(t *testing.T) {
	for _, tc := range []struct {
		id      string
		ctx     int
		out     int
		temp    float64
		topP    float64
		replay  string
		ctxFrom Origin
	}{
		{"kimi-k3", 1_048_576, 1_048_576, 1.0, 1.0, "all", OriginCard},
		{"deepseek-v4.1-flash", 1_048_576, 393_216, 1.0, 0.95, "all", OriginCard},
		{"glm5.3", 1_048_576, 131_072, 1.0, 0.95, "all", OriginCard},
		{"qwen3.6", 262_144, 32_768, 1.0, 0.95, "all", OriginCard},
		{"MiniMax-M3", 1_048_576, 524_288, 1.0, 0.95, "all", OriginCard},
		{"hy3", 262_144, 131_072, 0.9, 1.0, "all", OriginCard},
	} {
		p := lookupProfile(tc.id)
		switch {
		case p.Context != tc.ctx:
			t.Errorf("%s context %d, want %d", tc.id, p.Context, tc.ctx)
		case p.Output != tc.out:
			t.Errorf("%s output %d, want %d", tc.id, p.Output, tc.out)
		case p.Temperature == nil || *p.Temperature != tc.temp:
			t.Errorf("%s temperature %v, want %v", tc.id, p.Temperature, tc.temp)
		case p.TopP == nil || *p.TopP != tc.topP:
			t.Errorf("%s top_p %v, want %v", tc.id, p.TopP, tc.topP)
		case p.Replay != tc.replay:
			t.Errorf("%s replay %q, want %q", tc.id, p.Replay, tc.replay)
		case p.Src.Context != tc.ctxFrom:
			t.Errorf("%s context provenance %v, want %v", tc.id, p.Src.Context, tc.ctxFrom)
		case p.ToolParser == "" || p.ReasonParse == "":
			t.Errorf("%s: doctor cannot name the engine flags: %+v", tc.id, p)
		}
	}
	// Deliberately absent, because nobody published them: hy3's top_k is -1
	// (disabled) in generation_config.json and cannot be expressed as an int > 0,
	// and neither Kimi-K3 nor DeepSeek document one at all.
	for _, id := range []string{"hy3", "kimi-k3", "deepseek-v4.1-flash", "glm5.3"} {
		if p := lookupProfile(id); p.TopK != 0 {
			t.Errorf("%s: top_k %d is not sourced and must be unset", id, p.TopK)
		}
	}
	// Documented as accepting no effort field at all — a level sent to either is
	// meaningless, so the profile must not invent a vocabulary for it.
	for _, id := range []string{"qwen3.6", "MiniMax-M3"} {
		if p := lookupProfile(id); len(p.Efforts) != 0 {
			t.Errorf("%s documents no reasoning_effort, got %v", id, p.Efforts)
		}
	}
}

// bodyOf renders the request a client would send, so a test can assert on the
// fields that are NOT in it.
func bodyOf(t *testing.T, c *Client, req ChatRequest) map[string]any {
	t.Helper()
	raw, err := c.body(req, false)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return b
}

// The honest default: a model nobody wrote numbers for gets no numbers. A guessed
// temperature is worse than none, because the server's own default is at least
// the vendor's.
func TestNoProfileSendsNoSampling(t *testing.T) {
	c := NewClient(Config{BaseURL: "http://x/v1", Model: "brand-new-model-nobody-has-carded", Temperature: -1})
	b := bodyOf(t, c, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	for _, k := range []string{"temperature", "top_p", "top_k", "max_tokens", "chat_template_kwargs", "reasoning_effort"} {
		if v, ok := b[k]; ok {
			t.Errorf("an unprofiled model must send no %s, got %v", k, v)
		}
	}
	if p := lookupProfile("brand-new-model-nobody-has-carded"); p.Family != "" {
		t.Fatalf("test model accidentally matches %q", p.Key)
	}
}

// A profiled model sends exactly the card's numbers, and a roles.yaml models:
// entry overrides them — the operator's file outranks the card, and both outrank
// sending nothing.
func TestModelOptsOverrideCardValues(t *testing.T) {
	c := NewClient(Config{BaseURL: "http://x/v1", Model: "hy3", Temperature: -1})
	b := bodyOf(t, c, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if b["temperature"] != 0.9 || b["top_p"] != 1.0 {
		t.Fatalf("hy3 must send the card's 0.9 / 1.0, got %v / %v", b["temperature"], b["top_p"])
	}

	// roles.yaml: models: {hy3: {temperature: 0.2, top_p: 0.5, top_k: 7}}
	rc := &RolesConfig{ModelOpts: map[string]*ModelOpts{"hy3": {Temperature: f64(0.2), TopP: f64(0.5), TopK: 7}}}
	s := &Session{client: c, orch: &Orchestrator{roles: rc}, agent: &Agent{}}
	req := ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}
	s.applyModelOpts(&req)
	b = bodyOf(t, c, req)
	if b["temperature"] != 0.2 || b["top_p"] != 0.5 || b["top_k"] != 7.0 {
		t.Fatalf("roles.yaml must win over the card, got %v / %v / %v", b["temperature"], b["top_p"], b["top_k"])
	}

	// The key may be spelled the way the vendor writes it while the gateway serves
	// another spelling of the same model; the normalised key still finds it.
	rc = &RolesConfig{ModelOpts: map[string]*ModelOpts{"GLM-5.3": {Temperature: f64(0.3)}}}
	if o := rc.modelOpts("glm5.3"); o == nil || o.Temperature == nil || *o.Temperature != 0.3 {
		t.Fatalf("models.GLM-5.3 must reach a model served as glm5.3, got %v", o)
	}
}

// The running deployment outranks the table for the context window: an operator
// serving a 1M-context model at 128k on the GPUs they have is the truth, and the
// card is then a lie that builds prompts the server refuses.
func TestServerMaxModelLenWinsOverTable(t *testing.T) {
	c := NewClient(Config{BaseURL: "http://x/v1", Model: "kimi-k3", Temperature: -1})
	if got := c.CtxLen(); got != 1_048_576 || c.CtxSrc() != OriginCard {
		t.Fatalf("without a server, the table is the fallback: %d (%v)", got, c.CtxSrc())
	}
	c.SetCtxLen(131_072)
	if got := c.CtxLen(); got != 131_072 || c.CtxSrc() != OriginServer {
		t.Fatalf("the deployment must win: %d (%v)", got, c.CtxSrc())
	}
	if got := ctxBudget(0, c.CtxLen()); got != 131_072*3/4 {
		t.Fatalf("the budget must follow the server: %d", got)
	}
	// A server that omits max_model_len sends 0, and must not clobber a window we
	// already know. SGLang frequently omits it.
	c.SetCtxLen(0)
	if got := c.CtxLen(); got != 131_072 {
		t.Fatalf("max_model_len 0 must be ignored, got %d", got)
	}
	// Switching model or endpoint forgets it: keeping the previous model's window
	// is a swing between 262,144 and 1,048,576 across the six served models.
	c.SetModel("hy3")
	if got := c.CtxLen(); got != 262_144 || c.CtxSrc() != OriginCard {
		t.Fatalf("/model must re-derive the window: %d (%v)", got, c.CtxSrc())
	}
	c.SetCtxLen(200_000)
	c.SetEndpoint("http://other:8000/v1")
	if got := c.CtxLen(); got != 262_144 {
		t.Fatalf("/endpoint must forget another machine's window, got %d", got)
	}
	// A hosted client never learns a window — there is no max_model_len on a
	// hosted /v1/models — so its provenance must never read "server".
	h := &Client{provider: &Provider{ID: "moonshot", Dialect: "moonshot"}, model: "kimi-k3"}
	h.SetCtxLen(131_072)
	if h.CtxSrc() == OriginServer || h.CtxLen() != 1_048_576 {
		t.Fatalf("a hosted client must stay on the card: %d (%v)", h.CtxLen(), h.CtxSrc())
	}
	// And max_tokens is bounded by the window the deployment reports, or vLLM
	// refuses the request with an error the overflow check will not recognise.
	c2 := NewClient(Config{BaseURL: "http://x/v1", Model: "kimi-k3", Temperature: -1})
	c2.SetCtxLen(40_000)
	b := bodyOf(t, c2, ChatRequest{MaxTokens: 32_000, Messages: []Message{{Role: "user", Content: "hi"}}})
	if b["max_tokens"] != 10_000.0 {
		t.Fatalf("max_tokens must be bounded by ctxLen/4, got %v", b["max_tokens"])
	}
}

// The effort vocabulary is per model, and a level outside it is dropped: hy3
// raises inside its Jinja template on medium or max (a 500 every turn), and
// glm-5.3 silently promotes an unknown level to the most expensive setting.
func TestEffortVocabularyIsPerModel(t *testing.T) {
	for _, tc := range []struct{ id, level, want string }{
		{"hy3", "high", "high"},
		{"hy3", "low", "low"},
		{"hy3", "max", ""},
		{"hy3", "medium", ""},
		{"glm5.3", "max", "max"},
		{"glm5.3", "medium", ""},
		{"kimi-k3", "max", "max"},
		{"qwen3.6", "high", ""},    // documents no effort field
		{"MiniMax-M3", "high", ""}, // ignores the field
		{"deepseek-v4.1-flash", "medium", "medium"},
	} {
		if got := lookupProfile(tc.id).effortFor(tc.level); got != tc.want {
			t.Errorf("%s effort %q → %q, want %q", tc.id, tc.level, got, tc.want)
		}
	}
	// A model with no profile takes what the operator typed: that is their
	// instruction, not the client's guess.
	if got := lookupProfile("brand-new-model-nobody-has-carded").effortFor("high"); got != "high" {
		t.Errorf("an unprofiled model should forward the operator's level, got %q", got)
	}
}

// The thinking switch is the model's, not the engine's. Against the local vllm
// dialect the old code sent enable_thinking to all six, which is right only for
// Qwen and wrong or inert for the other five.
func TestLocalThinkingSwitchComesFromTheProfile(t *testing.T) {
	p := &Provider{ID: "local", Dialect: "vllm", Local: true}
	kwargs := func(id, mode string) map[string]any {
		prof := lookupProfile(id)
		out := thinkingParams(p, id, prof, mode, prof.Replay)
		kw, _ := out["chat_template_kwargs"].(map[string]any)
		if kw == nil {
			kw = map[string]any{}
		}
		for k, v := range out {
			if k != "chat_template_kwargs" {
				kw[k] = v
			}
		}
		return kw
	}
	// Qwen3.6: enable_thinking, and preserve_thinking because we replay.
	if kw := kwargs("qwen3.6", "on"); kw["enable_thinking"] != true || kw["preserve_thinking"] != true {
		t.Errorf("qwen3.6: %v", kw)
	}
	// MiniMax-M3 reads thinking_mode, not enable_thinking, and takes no effort.
	if kw := kwargs("MiniMax-M3", "on"); kw["thinking_mode"] != "enabled" || kw["enable_thinking"] != nil {
		t.Errorf("minimax-m3: %v", kw)
	}
	// GLM-5.3 cannot be switched off; it takes reasoning_effort + clear_thinking.
	if kw := kwargs("glm5.3", "on"); kw["reasoning_effort"] != "max" || kw["clear_thinking"] != false {
		t.Errorf("glm-5.3: %v", kw)
	}
	// hy3: reasoning_effort IS the switch, and no_think is its off.
	if kw := kwargs("hy3", "on"); kw["reasoning_effort"] != "high" {
		t.Errorf("hy3 on: %v", kw)
	}
	if kw := kwargs("hy3", "off"); kw["reasoning_effort"] != "no_think" {
		t.Errorf("hy3 off: %v", kw)
	}
	if kw := kwargs("hy3", "max"); len(kw) != 0 {
		t.Errorf("hy3 must not be sent an effort its template raises on: %v", kw)
	}
	// Kimi-K3 has no switch at all and takes a TOP-LEVEL reasoning_effort.
	out := thinkingParams(p, "kimi-k3", lookupProfile("kimi-k3"), "on", "all")
	if out["reasoning_effort"] != "max" || out["chat_template_kwargs"] != nil {
		t.Errorf("kimi-k3: %v", out)
	}
	// DeepSeek-V4.1 reads chat_template_kwargs.thinking.
	if kw := kwargs("deepseek-v4.1-flash", "off"); kw["thinking"] != false {
		t.Errorf("deepseek off: %v", kw)
	}
}

// Preserved thinking is only coherent when we are actually replaying it: with
// reasoning_replay: off the kwarg asking the server to keep history must go too.
func TestPreservedThinkingFollowsReplay(t *testing.T) {
	c := NewClient(Config{BaseURL: "http://x/v1", Model: "qwen3.6", Temperature: -1})
	b := bodyOf(t, c, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	kw, _ := b["chat_template_kwargs"].(map[string]any)
	if kw["preserve_thinking"] != true {
		t.Fatalf("qwen3.6 replays every step, so preserve_thinking must be sent: %v", b)
	}
	c.noReplay = true
	b = bodyOf(t, c, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := b["chat_template_kwargs"]; ok {
		t.Fatalf("with replay off, nothing should ask the server to preserve it: %v", b)
	}
}

// The provenance an operator reads in /model and doctor.
func TestProvenanceRendering(t *testing.T) {
	p := lookupProfile("kimi-k3")
	if got := srcNum(p.Context, p.Src.Context); got != "1.05M (card)" {
		t.Errorf("context rendering: %q", got)
	}
	if got := srcFloat(p.Temperature, p.Src.Temperature); got != "1 (card)" {
		t.Errorf("temperature rendering: %q", got)
	}
	if got := srcNum(p.TopK, p.Src.TopK); got != "unset" {
		t.Errorf("an unsourced field must read unset, got %q", got)
	}
	if got := srcNum(lookupProfile("MiniMax-M3").TopK, lookupProfile("MiniMax-M3").Src.TopK); got != "40 (engine docs)" {
		t.Errorf("minimax top_k rendering: %q", got)
	}
	// A number with no recorded origin says so rather than borrowing authority.
	if got := srcNum(200_000, OriginUnset); !strings.Contains(got, "unsourced") {
		t.Errorf("an origin-less number must say so, got %q", got)
	}
	if got := srcNum(lookupProfile("glm5.3").Context, OriginServer); got != "1.05M (server)" {
		t.Errorf("server rendering: %q", got)
	}
}

// ── the fixes ───────────────────────────────────────────────────────────────

// The precedence "the deployment's max_model_len outranks the card" has to reach
// a role's models: chain, which is where this binary actually runs. A chain
// client is a copy of the local client with the model swapped, and nothing on
// that path ever asked the endpoint: main.go reconciles only when the session IS
// the local client, and relearnCtxLen is reachable only from /model and
// /endpoint. So a role on a 1M-card model served at 131k budgeted 786k of prompt
// against a 131k deployment — the silent overflow, in the one place it matters.
func TestRoleChainSessionLearnsTheServerWindow(t *testing.T) {
	cfg := Config{BaseURL: "http://gateway.invalid/v1", Model: "hy3", Temperature: -1}
	ps := NewProviders(cfg, nil, NewClient(cfg))
	// Seeded, not probed: the point is that the chain path consults what the
	// endpoint said, wherever it was learned (discovery, doctor, or its own ask).
	ps.LearnWindows(cfg.BaseURL, []ModelInfo{{ID: "hy3", MaxLen: 131_072}, {ID: "glm5.3", MaxLen: 2_097_152}})

	o := &Orchestrator{cfg: cfg, providers: ps, agents: map[string]*Agent{}}
	s := &Session{orch: o, models: []string{"hy3", "glm5.3"}}
	s.useModel(0)
	if got, src := s.client.CtxLen(), s.client.CtxSrc(); got != 131_072 || src != OriginServer {
		t.Fatalf("a chain session must budget from the deployment: %d (%v)", got, src)
	}
	// And the reply budget follows it, rather than the card's 131,072 output.
	b := bodyOf(t, s.client, ChatRequest{MaxTokens: 64_000, Messages: []Message{{Role: "user", Content: "hi"}}})
	if b["max_tokens"] != float64(131_072/4) {
		t.Fatalf("max_tokens must follow the learned window, got %v", b["max_tokens"])
	}
	// A failover is a model switch on the same deployment, so it re-learns too —
	// here upwards, which is equally the server's truth (YaRN).
	s.useModel(1)
	if got, src := s.client.CtxLen(), s.client.CtxSrc(); got != 2_097_152 || src != OriginServer {
		t.Fatalf("a fallback model must learn its own window: %d (%v)", got, src)
	}
	// The same for a subagent or an `lca run` step on another served model.
	c, err := ps.Client("glm5.3")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if got, src := c.CtxLen(), c.CtxSrc(); got != 2_097_152 || src != OriginServer {
		t.Fatalf("a derived client must learn the window too: %d (%v)", got, src)
	}
	// A model the endpoint does not list keeps the table as its fallback, and the
	// cache must not be mistaken for an answer about it.
	s2 := &Session{orch: o, models: []string{"kimi-k3"}}
	s2.useModel(0)
	if got, src := s2.client.CtxLen(), s2.client.CtxSrc(); got != 1_048_576 || src != OriginCard {
		t.Fatalf("an unlisted model keeps the card: %d (%v)", got, src)
	}
}

// A named effort level on a model that documents no vocabulary must still send
// the model's own thinking switch. Dropping both meant `effort: max` asked for
// strictly less than `thinking: on` did — on two of the six served models, and
// on the shipped examples/roles.yaml's premium and coder roles.
func TestNamedEffortStillSendsTheProfileSwitch(t *testing.T) {
	p := &Provider{ID: "local", Dialect: "vllm", Local: true}
	kwargs := func(id, mode string) map[string]any {
		prof := lookupProfile(id)
		out := thinkingParams(p, id, prof, mode, prof.Replay)
		kw, _ := out["chat_template_kwargs"].(map[string]any)
		return kw
	}
	for _, tc := range []struct{ id, mode, key string }{
		{"MiniMax-M3", "max", "thinking_mode"},
		{"MiniMax-M3", "high", "thinking_mode"},
		{"qwen3.6", "high", "enable_thinking"},
	} {
		kw := kwargs(tc.id, tc.mode)
		on := lookupProfile(tc.id).Switch.On
		if kw[tc.key] != on {
			t.Errorf("%s effort %q: %s = %v, want %v (%v)", tc.id, tc.mode, tc.key, kw[tc.key], on, kw)
		}
	}
	// The level itself is still dropped, and EffortOn is NOT substituted for it:
	// "think this much" is the operator's number, and hy3's template raises on a
	// level outside no_think|low|high.
	if kw := kwargs("hy3", "medium"); len(kw) != 0 {
		t.Errorf("hy3 must be sent no effort it does not accept: %v", kw)
	}
	if kw := kwargs("qwen3.6", "high"); kw["reasoning_effort"] != nil {
		t.Errorf("qwen3.6 documents no effort field, so no level may be sent: %v", kw)
	}
}

// A model that cannot be switched off keeps the request shape every other
// request has. GLM-5.3 replays reasoning_content on every step whatever the
// thinking mode, so dropping clear_thinking:false on `off` asked the template to
// clear a history we were still sending.
func TestOffKeepsPreservedThinkingWhenThinkingCannotStop(t *testing.T) {
	p := &Provider{ID: "local", Dialect: "vllm", Local: true}
	out := thinkingParams(p, "glm5.3", lookupProfile("glm5.3"), "off", "all")
	kw, _ := out["chat_template_kwargs"].(map[string]any)
	if kw["clear_thinking"] != false {
		t.Fatalf("glm-5.3 off must keep clear_thinking:false while we replay: %v", out)
	}
	if kw["reasoning_effort"] != nil {
		t.Fatalf("off must not ask for a level: %v", out)
	}
	// With replay off it goes too: the kwarg is about the history we send.
	out = thinkingParams(p, "glm5.3", lookupProfile("glm5.3"), "off", "")
	if out != nil {
		t.Fatalf("no replay, nothing to preserve: %v", out)
	}
}

// doctor's parser hint has to follow the effort the CLIENT SENT. hy3 accepts only
// no_think|low|high, so `effort: medium` is dropped and the probe asks for
// nothing — a run that then printed "--reasoning-parser hy_v3" contradicted its
// own warning two lines above.
func TestThinkingExpectedFollowsTheEffortActuallySent(t *testing.T) {
	for _, tc := range []struct {
		id, effort string
		want       bool
	}{
		{"hy3", "medium", false}, // dropped, and reasoning_effort IS hy3's switch
		{"hy3", "max", false},    // same
		{"hy3", "high", true},    // accepted
		{"hy3", "no_think", false},
		{"hy3", "", false},   // hy3 does not think unless told to
		{"glm5.3", "", true}, // cannot be switched off, so silence is a symptom
		{"glm5.3", "medium", true},
		{"kimi-k3", "medium", true},
		{"qwen3.6", "high", true}, // the level is dropped but enable_thinking is not
		{"qwen3.6", "off", false},
		{"qwen3.6", "", false},
	} {
		if got := thinkingExpected(lookupProfile(tc.id), tc.effort); got != tc.want {
			t.Errorf("thinkingExpected(%s, %q) = %v, want %v", tc.id, tc.effort, got, tc.want)
		}
	}
}

// The reply budget is one number with one reason, and /model prints both. The
// window clamp is correct, but it can rewrite a budget the operator configured,
// and an unexplained rewrite is the same failure as an unsourced value.
func TestReplyCeilingNamesItsSource(t *testing.T) {
	c := NewClient(Config{BaseURL: "http://x/v1", Model: "hy3", Temperature: -1})
	// Local and nothing configured: no max_tokens at all, and it says so.
	if n, why := c.replyCeiling(0); n != 0 || !strings.Contains(why, "deployment's own default") {
		t.Fatalf("local default: %d %q", n, why)
	}
	c.maxTokens = 64_000
	if n, why := c.replyCeiling(0); n != 64_000 || !strings.Contains(why, "configured") {
		t.Fatalf("configured: %d %q", n, why)
	}
	// Learned window: clamped, and the reason names the clamp and the window.
	c.SetCtxLen(131_072)
	n, why := c.replyCeiling(0)
	if n != 32_768 || !strings.Contains(why, "configured") || !strings.Contains(why, "clamped") || !strings.Contains(why, "131k") {
		t.Fatalf("clamped: %d %q", n, why)
	}
	b := bodyOf(t, c, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if b["max_tokens"] != float64(n) {
		t.Fatalf("/model must report the number body() sends: %v vs %d", b["max_tokens"], n)
	}
	// A hosted client with no configured budget: the profile's ceiling, capped.
	h := &Client{provider: &Provider{ID: "moonshot", Dialect: "moonshot"}, model: "kimi-k3"}
	if n, why := h.replyCeiling(0); n != outputTokenMax || !strings.Contains(why, "capped") {
		t.Fatalf("hosted default: %d %q", n, why)
	}
}

// Kimi K3's temperature and top_p are the SELF-HOST's numbers: Moonshot fixes
// both server-side and documents omitting them, and the card's agentic top_p 1.0
// is not even the 0.95 it fixes. The Note said so and nothing acted on it.
func TestSelfHostSamplingDoesNotReachTheHostedAPI(t *testing.T) {
	local := NewClient(Config{BaseURL: "http://x/v1", Model: "kimi-k3", Temperature: -1})
	b := bodyOf(t, local, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if b["temperature"] != 1.0 || b["top_p"] != 1.0 {
		t.Fatalf("a self-host gets the card's numbers: %v / %v", b["temperature"], b["top_p"])
	}
	h := &Client{provider: &Provider{ID: "moonshot", Dialect: "moonshot", Transport: transportNative}, model: "kimi-k3"}
	b = bodyOf(t, h, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := b[k]; ok {
			t.Errorf("Moonshot fixes %s server-side and says to omit it, got %v", k, v)
		}
	}
	// What the operator set is their instruction, not our guess, and still goes.
	b = bodyOf(t, h, ChatRequest{Temperature: f64(0.3), TopP: f64(0.7), Messages: []Message{{Role: "user", Content: "hi"}}})
	if b["temperature"] != 0.3 || b["top_p"] != 0.7 {
		t.Fatalf("an explicit value must survive: %v / %v", b["temperature"], b["top_p"])
	}
	// And the gate is per profile, not per provider: GLM's numbers are the API's
	// as well as the card's, so a hosted GLM still gets them.
	z := &Client{provider: &Provider{ID: "zai", Dialect: "zai"}, model: "glm-5.3"}
	b = bodyOf(t, z, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if b["temperature"] != 1.0 || b["top_p"] != 0.95 {
		t.Fatalf("glm-5.3's sampling is not self-host-only: %v / %v", b["temperature"], b["top_p"])
	}
}

// A hosted API resolves its own compat spellings; a chat template resolves
// nothing. vLLM's V4.1-Flash recipe documents low|high|xhigh|max, so the four
// aliases DeepSeek's API maps for you are not sent to a self-host.
func TestDeepSeekEffortVocabularyIsPerPath(t *testing.T) {
	prof := lookupProfile("deepseek-v4.1-flash")
	for _, lvl := range []string{"low", "high", "xhigh", "max"} {
		if prof.effortForLocal(lvl) != lvl {
			t.Errorf("the engine documents %q", lvl)
		}
	}
	for _, lvl := range []string{"none", "minimal", "medium", "ultra"} {
		if prof.effortFor(lvl) != lvl {
			t.Errorf("the hosted API documents %q", lvl)
		}
		if got := prof.effortForLocal(lvl); got != "" {
			t.Errorf("%q is not in the engine's vocabulary, got %q", lvl, got)
		}
	}
	local := &Provider{ID: "local", Dialect: "vllm", Local: true}
	out := thinkingParams(local, "deepseek-v4.1-flash", prof, "medium", "all")
	kw, _ := out["chat_template_kwargs"].(map[string]any)
	if kw["reasoning_effort"] != nil {
		t.Errorf("a level the template does not know must not be sent: %v", out)
	}
	if kw["thinking"] != true {
		t.Errorf("but the switch still goes, because thinking was asked for: %v", out)
	}
	// The hosted path keeps the full ladder.
	hosted := &Provider{ID: "deepseek", Dialect: "deepseek"}
	out = thinkingParams(hosted, "deepseek-v4-pro", lookupProfile("deepseek-v4-pro"), "medium", "all")
	if out["reasoning_effort"] != "medium" {
		t.Errorf("the hosted API resolves medium itself: %v", out)
	}
	// And a profile with no local list is the same list both ways.
	if got := lookupProfile("hy3").effortForLocal("low"); got != "low" {
		t.Errorf("hy3 has one vocabulary: %q", got)
	}
}

// The values two reviewers could not source, now absent. Each of these was a
// number or a vocabulary copied from a sibling version, which reads as knowledge
// in /model and doctor while being a guess.
func TestUnsourcedValuesAreAbsentNotGuessed(t *testing.T) {
	// deepseek-v4-pro: hosted-only, so no card and no window. The output ceiling
	// stays because the API reference documents it for this id.
	if p := lookupProfile("deepseek-v4-pro"); p.Context != 0 || p.Src.Context != OriginUnset || p.Output != 393_216 || p.Src.Output != OriginAPI {
		t.Errorf("deepseek-v4-pro: %+v", p)
	}
	// glm-5 and glm-5.2 carried GLM-5.3's three levels; Z.ai documents
	// reasoning_effort for 5.2 and above, per version, and an unknown level is
	// promoted to max — the most expensive setting there is.
	for _, id := range []string{"glm-5", "glm-5.1", "glm-5.2"} {
		p := lookupProfile(id)
		if len(p.Efforts) != 0 || p.EffortOn != "" || p.Switch.Effort != "" {
			t.Errorf("%s must send no effort level: %+v", id, p)
		}
		if p.Switch.Keep != "clear_thinking" {
			t.Errorf("%s keeps Preserved Thinking, a series-wide API feature: %+v", id, p)
		}
	}
	// GLM-5.3-Flash has no window of its own, but its effort vocabulary IS
	// published for that exact id — suppressing it was its own omission.
	if p := lookupProfile("glm-5.3-flash"); p.Context != 0 || p.effortFor("max") != "max" || p.Switch.Effort != "reasoning_effort" {
		t.Errorf("glm-5.3-flash: %+v", p)
	}
	// 0.6 is kimi-k2-0711's temperature and is sourced for nothing else.
	if p := lookupProfile("kimi-k9000"); p.Temperature != nil {
		t.Errorf("the generic kimi rule must send no temperature: %v", *p.Temperature)
	}
	if p := lookupProfile("kimi-k2-0711"); p.Temperature == nil || *p.Temperature != 0.6 {
		t.Errorf("the one Kimi that IS carded at 0.6 keeps it: %+v", p)
	}
	// The versions a removed entry used to cover: family only, numbers absent.
	for _, id := range []string{"kimi-k4", "kimi-k5", "deepseek-v5", "deepseek-v4.1-pro", "deepseek-v4.2-flash", "glm-5.4", "glm-6"} {
		p := lookupProfile(id)
		if p.Family == "" {
			t.Errorf("%s must still resolve to its family", id)
		}
		if p.Context != 0 || p.Output != 0 || p.Temperature != nil || p.TopP != nil || p.TopK != 0 {
			t.Errorf("%s inherited a sibling's numbers: %+v", id, p)
		}
		if p.Note == "" {
			t.Errorf("%s must say out loud that nothing is recorded", id)
		}
	}
}

// isSmall decides which model is offered as the cheap summariser and which are
// candidates for lead and coder. It matched raw substrings, so "MiniMax-M3" — a
// 428B flagship — was "small" because the vendor's name contains "mini": it was
// proposed for compaction and dropped from the roles that needed it.
func TestIsSmallMatchesTokensNotSubstrings(t *testing.T) {
	big := []string{"MiniMax-M3", "kimi-k3", "glm5.3", "qwen3.6", "hy3", "deepseek-v4.1", "qwen3.6-235b-a22b"}
	small := []string{"deepseek-v4.1-flash", "qwen3-30b-a3b-instruct", "glm-4.5-air", "gpt-4o-mini",
		"qwen3.6-27b", "some-8b-instruct", "model-lite", "nano-2"}
	for _, m := range big {
		if isSmall(m) {
			t.Errorf("%s must not be treated as a small model", m)
		}
	}
	for _, m := range small {
		if !isSmall(m) {
			t.Errorf("%s is a small model", m)
		}
	}
}

// With no small model served, there is no cheap role: an arbitrary "last name the
// gateway listed" made a flagship the summariser, and a role with an empty
// models: list does not load at all.
func TestPickCheapIsEmptyWithoutASmallModel(t *testing.T) {
	fleet := []string{"hy3", "kimi-k3", "MiniMax-M3", "qwen3.6", "glm5.3"}
	if got := pickCheap(fleet); got != "" {
		t.Fatalf("pickCheap(%v) = %q, want none", fleet, got)
	}
	if cheapBlock("") != "" {
		t.Fatal("no cheap model must write no cheap role")
	}
	withFlash := append(fleet, "deepseek-v4.1-flash")
	if got := pickCheap(withFlash); got != "deepseek-v4.1-flash" {
		t.Fatalf("pickCheap picked %q", got)
	}
	if !strings.Contains(cheapBlock("deepseek-v4.1-flash"), "models: [deepseek-v4.1-flash]") {
		t.Fatalf("cheap role block: %q", cheapBlock("deepseek-v4.1-flash"))
	}
}

// The default team over the operator's own fleet: the flagship must be a
// candidate for the roles that do the work, not the summariser.
func TestDefaultTeamOverTheServedFleet(t *testing.T) {
	names := []string{"hy3", "kimi-k3", "MiniMax-M3", "deepseek-v4.1-flash", "qwen3.6", "glm5.3"}
	lead, coder := pickModels(names, leadPref, 2), pickModels(names, coderPref, 2)
	if !contains(append(lead, coder...), "MiniMax-M3") {
		t.Errorf("MiniMax-M3 is a flagship; lead=%v coder=%v", lead, coder)
	}
	if got := pickCheap(names); got != "deepseek-v4.1-flash" {
		t.Errorf("the flash build is the cheap one, got %q", got)
	}
}

// A deployment that does not publish max_model_len still states its window when
// it refuses a prompt. That number is the cheapest truth available about it, and
// it used to be thrown away: the session kept the 24k placeholder, compacted
// every other turn and re-read the same files — which is what "multi-agent mode
// is slow" turned out to be.
func TestWindowLearnedFromTheServersRefusal(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`This model's maximum context length is 131072 tokens. However, you requested 1000000032 tokens (32 in the messages, 1000000000 in the completion).`, 131072},
		{`{"error":{"message":"the input length (26000 tokens) is longer than the model's context length 32768","type":"BadRequestError"}}`, 32768},
		{`max_model_len is 262144, got 300000`, 262144},
		{`context window exceeds limit`, 0},       // no number: learn nothing
		{`you requested 1000000032 tokens`, 0},    // a request is not a window
		{`maximum context length is 8 tokens`, 0}, // implausible
	}
	for _, c := range cases {
		got := statedWindow(&APIError{Status: 400, Body: c.body})
		if got != c.want {
			t.Errorf("statedWindow(%.60s…) = %d, want %d", c.body, got, c.want)
		}
	}
}

// What a refusal taught us survives the session that paid for it.
func TestLearnedWindowIsRemembered(t *testing.T) {
	dir := t.TempDir()
	ps := NewProviders(Config{}, nil, &Client{provider: &Provider{ID: "local", Local: true}})
	ps.UseStateDir(dir)
	ps.RememberWindow("http://gw:8080/v1/", "Kimi-K3", 131072)

	if got := ps.LearnedWindow("http://gw:8080/v1", "kimi-k3"); got != 131072 {
		t.Fatalf("in this process: %d", got) // endpoint slash and id case must not matter
	}
	next := NewProviders(Config{}, nil, &Client{provider: &Provider{ID: "local", Local: true}})
	next.UseStateDir(dir)
	if got := next.LearnedWindow("http://gw:8080/v1", "kimi-k3"); got != 131072 {
		t.Fatalf("after a restart: %d, want the remembered window", got)
	}
}

// doctor asks a deployment that publishes no max_model_len how big its window is,
// by requesting an impossible completion: engines validate the budget before they
// generate, so the refusal names the real number and nothing is decoded. The
// answer is remembered, so the next session starts with a real budget instead of
// the 24k placeholder that spends a session compacting.
func TestDoctorProbesAndRemembersTheWindow(t *testing.T) {
	var askedFor int
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if mt, ok := req.Raw["max_tokens"].(float64); ok {
			askedFor = int(mt)
		}
		if askedFor > 131072 {
			return fakeReply{status: 400,
				content: `This model's maximum context length is 131072 tokens. However, you requested 1073741856 tokens`}
		}
		return fakeReply{content: "ok"}
	})
	fs.models = []string{"nameless-model"}

	dir := t.TempDir()
	cfg := Config{Root: t.TempDir(), Dir: dir, BaseURL: fs.URL, Endpoints: []string{fs.URL}, Model: "nameless-model"}
	c := NewClient(cfg)
	res := probeResult{model: "nameless-model", role: "lead"}
	probeWindow(context.Background(), c, &res, nil, cfg)

	if askedFor <= 131072 {
		t.Fatalf("the probe has to ask for an impossible completion, asked for %d", askedFor)
	}
	if c.CtxLen() != 131072 {
		t.Fatalf("the window the server named is not in force: %d", c.CtxLen())
	}
	if len(res.notes) == 0 || !strings.Contains(strings.Join(res.notes, " "), "131k") {
		t.Fatalf("doctor says nothing about what it learned: %v", res.notes)
	}
	// and the next process starts knowing it
	ps := NewProviders(cfg, nil, NewClient(cfg))
	ps.UseStateDir(cfg.stateDir())
	if got := ps.LearnedWindow(fs.URL, "nameless-model"); got != 131072 {
		t.Fatalf("not remembered for the next session: %d", got)
	}
}
