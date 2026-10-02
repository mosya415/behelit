package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both engines, as fixtures. Everything these servers answer is a shape read off
// SGLang v0.5.20/v0.5.21 or vLLM v0.11.0 — a mock that answers a shape nobody
// serves tests nothing but itself.

// sglangModelInfo is GET /model_info at v0.5.20. reasoning_parser and
// tool_call_parser arrived in v0.5.18; the published key list in
// native_api.mdx still omits them, served_model_name and three more, so every
// key here is optional on the reading side.
const sglangModelInfo = `{"model_path":"/models/HY3","tokenizer_path":"/models/HY3","is_generation":true,
 "preferred_sampling_params":null,"weight_version":"default","served_model_name":"hy3",
 "model_type":"hy_v3","architectures":["HunYuanV3ForCausalLM"],
 "reasoning_parser":"hunyuan","tool_call_parser":"hunyuan"}`

// sglangServerInfo is GET /server_info at v0.5.20, cut to the keys doctor reads.
// version is the ONLY place an SGLang version is reported: no /v1 route carries
// one, which is why every version-gated field stays optional.
const sglangServerInfo = `{"version":"0.5.20","launch_command":"python3 -m sglang.launch_server --model-path /models/HY3 --reasoning-parser hunyuan --tool-call-parser hunyuan --context-length 262144",
 "max_req_input_len":262139,"allow_auto_truncate":false,"context_length":262144,
 "max_total_num_tokens":524288,"sampling_defaults":"model","default_chat_template_kwargs":{},
 "trust_request_chat_template":false}`

// newSGLangServer is a direct SGLang deployment: cards owned_by "sglang" with the
// real max_model_len, errors in SGLang's FLAT envelope, and both informational
// routes outside /v1 answering (with their /get_* aliases, which is what a server
// older than v0.5.8 answers instead).
func newSGLangServer(t *testing.T, respond func(fakeRequest, int) fakeReply) *fakeServer {
	fs := newFakeServer(t, respond)
	fs.ownedBy = engineSGLang
	fs.flatErr = true
	fs.native = map[string]string{
		"/model_info": sglangModelInfo, "/get_model_info": sglangModelInfo,
		"/server_info": sglangServerInfo, "/get_server_info": sglangServerInfo,
	}
	return fs
}

// newVLLMServer is the vLLM-shaped deployment lca assumed everywhere: owned_by
// "vllm", the wrapped error envelope, nothing outside /v1.
func newVLLMServer(t *testing.T, respond func(fakeRequest, int) fakeReply) *fakeServer {
	fs := newFakeServer(t, respond)
	fs.ownedBy = engineVLLM
	return fs
}

// newBlindGateway is the aggregating gateway in front of either engine: it
// answers /v1/* and 404s every other path, and synthesizes its own cards —
// {"id","object","owned_by":"local"} with no max_model_len, which is exactly what
// SGLang's own Rust router does. Nothing about the engine can be inferred from
// it, and that has to stay a sentence rather than a guess.
func newBlindGateway(t *testing.T, respond func(fakeRequest, int) fakeReply) *fakeServer {
	fs := newFakeServer(t, respond)
	fs.ownedBy = "local"
	fs.v1Only = true
	return fs
}

func okReply(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} }

// plain is doctor's output as a person reads it: the colour codes removed and
// every run of whitespace collapsed, so an assertion is about the sentence and
// not about where the terminal width happened to wrap it.
func plain(out string) string {
	var b strings.Builder
	for i := 0; i < len(out); i++ {
		if out[i] == 0x1b {
			for i < len(out) && out[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(out[i])
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// T1. The precedence is fixed and a weaker source may never overwrite a stronger
// one: roles.yaml > the endpoint's own setting > owned_by > UNKNOWN. It is the
// inverse of the window's rule on purpose — a window is something a deployment
// reports and an operator guesses, an engine is something a gateway can rewrite
// and an operator who had to type it did so for a reason.
func TestEngineResolutionPrecedence(t *testing.T) {
	cfg := Config{BaseURL: "http://gateway.invalid/v1", Model: "hy3", Temperature: -1}
	newPS := func() *Providers {
		ps := NewProviders(cfg, nil, NewClient(cfg))
		ps.LearnWindows(cfg.BaseURL, []ModelInfo{
			{ID: "hy3", OwnedBy: "sglang", MaxLen: 262_144},
			{ID: "glm5.3", OwnedBy: "vllm", MaxLen: 1_048_576},
			{ID: "kimi-k3", OwnedBy: "local", MaxLen: 131_072}, // a router's synthesized card
		})
		return ps
	}

	// tier 3: the card's own owned_by, compared exactly.
	ps := newPS()
	c := NewClient(cfg)
	ps.applyEngine(c)
	if c.Engine() != engineSGLang || c.EngineSrc() != EngineFromOwnedBy {
		t.Fatalf("owned_by: %q (%v)", c.Engine(), c.EngineSrc())
	}
	// "local" is not a tiebreak, it is UNKNOWN.
	c.SetModel("kimi-k3")
	ps.applyEngine(c)
	if c.Engine() != "" {
		t.Fatalf("owned_by \"local\" must stay unknown, got %q", c.Engine())
	}

	// tier 2: the endpoint's own setting beats the card, both ways round.
	ps = newPS()
	c = NewClient(Config{BaseURL: cfg.BaseURL, Model: "hy3", Engine: engineVLLM})
	ps.applyEngine(c)
	if c.Engine() != engineVLLM || c.EngineSrc() != EngineFromEndpoint {
		t.Fatalf("LCA_ENGINE must outrank owned_by: %q (%v)", c.Engine(), c.EngineSrc())
	}
	// …and learning again does not undo it: Learn runs per endpoint, repeatedly.
	ps.applyEngine(c)
	if c.Engine() != engineVLLM {
		t.Fatalf("a weaker source overwrote a stronger one: %q (%v)", c.Engine(), c.EngineSrc())
	}

	// tier 1: roles.yaml models.<id>.engine beats everything.
	rc := &RolesConfig{ModelOpts: map[string]*ModelOpts{"hy3": {Engine: engineSGLang}}}
	o := &Orchestrator{cfg: cfg, providers: ps, roles: rc, agents: map[string]*Agent{}}
	o.applyModelEngine(c)
	if c.Engine() != engineSGLang || c.EngineSrc() != EngineFromModel {
		t.Fatalf("roles.yaml must win: %q (%v)", c.Engine(), c.EngineSrc())
	}
	ps.applyEngine(c) // the probe may not take it back
	if c.Engine() != engineSGLang || c.EngineSrc() != EngineFromModel {
		t.Fatalf("the probe overruled roles.yaml: %q (%v)", c.Engine(), c.EngineSrc())
	}

	// "auto" means probe, and nothing else is ever stored.
	c = NewClient(Config{BaseURL: cfg.BaseURL, Model: "hy3", Engine: engineAuto})
	ps.applyEngine(c)
	if c.Engine() != engineSGLang || c.EngineSrc() != EngineFromOwnedBy {
		t.Fatalf("auto must fall through to the probe: %q (%v)", c.Engine(), c.EngineSrc())
	}
	c.setEngine("TensorRT-LLM", EngineFromModel)
	if c.Engine() != engineSGLang {
		t.Fatalf("an engine lca cannot speak to must be dropped, got %q", c.Engine())
	}

	// A hosted provider has no engine and must not acquire one.
	hosted := &Client{provider: &Provider{ID: "tencent", Dialect: "tencent"}, model: "hy3", baseURL: cfg.BaseURL}
	ps.applyEngine(hosted)
	if hosted.Engine() != "" {
		t.Fatalf("a hosted API must have no engine, got %q", hosted.Engine())
	}

	// The provenance strings are the ones doctor prints.
	for _, tc := range []struct{ engine, want string }{
		{engineSGLang, "sglang"}, {"", "lca sends nothing engine-specific"},
	} {
		cl := NewClient(cfg)
		cl.setEngine(tc.engine, EngineFromEndpoint)
		if !strings.Contains(engineLine(cl), tc.want) {
			t.Errorf("engineLine(%q) = %q, want it to mention %q", tc.engine, engineLine(cl), tc.want)
		}
	}
}

// T2. SGLang's own wire shape parses: the engine from owned_by, the window from
// max_model_len as server truth — and a LoRA card (parent set, max_model_len
// null) never becomes the base model's window.
func TestSGLangModelsCardIsParsed(t *testing.T) {
	fs := newSGLangServer(t, okReply)
	fs.models = []string{"hy3"}
	fs.windows = map[string]int{"hy3": 262_144}
	fs.lora = "hy3-tuned"

	cfg := Config{BaseURL: fs.URL + "/v1", Model: "hy3", Temperature: -1}
	c := NewClient(cfg)
	ps := NewProviders(cfg, nil, c)
	ps.Learn(c)
	if c.Engine() != engineSGLang || c.EngineSrc() != EngineFromOwnedBy {
		t.Fatalf("engine: %q (%v)", c.Engine(), c.EngineSrc())
	}
	if c.CtxLen() != 262_144 || c.CtxSrc() != OriginServer {
		t.Fatalf("window: %d (%v)", c.CtxLen(), c.CtxSrc())
	}
	lora, err := ps.Client("hy3-tuned")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if lora.ServerCtxLen() != 0 {
		t.Fatalf("a LoRA card has max_model_len null and must teach no window, got %d", lora.ServerCtxLen())
	}
	// The adapter is served by the same engine, though, which its own card says.
	if lora.Engine() != engineSGLang {
		t.Fatalf("the LoRA card's owned_by is the engine's too: %q", lora.Engine())
	}
}

// T15. The engine is a fact about one (endpoint, model) pair: when either moves,
// it is forgotten and learned again. And roles.yaml's per-model engine survives a
// chain failover, which is the one place an operator's "hy3 is on SGLang" has to
// keep holding.
func TestEngineResetsWithModelAndEndpoint(t *testing.T) {
	cfg := Config{BaseURL: "http://gw.invalid/v1", Model: "hy3", Temperature: -1}
	ps := NewProviders(cfg, nil, NewClient(cfg))
	ps.LearnWindows(cfg.BaseURL, []ModelInfo{{ID: "hy3", OwnedBy: "sglang", MaxLen: 262_144}})

	c := NewClient(cfg)
	ps.applyEngine(c)
	if c.Engine() != engineSGLang {
		t.Fatalf("learned engine: %q", c.Engine())
	}
	c.SetModel("glm5.3")
	if c.Engine() != "" || c.EngineSrc() != EngineUnset {
		t.Fatalf("SetModel must forget the engine: %q (%v)", c.Engine(), c.EngineSrc())
	}
	ps.applyEngine(c)
	c.setEngine(engineSGLang, EngineFromOwnedBy)
	c.SetEndpoint("http://other.invalid/v1")
	if c.Engine() != "" || c.EngineSrc() != EngineUnset {
		t.Fatalf("SetEndpoint must forget the engine: %q (%v)", c.Engine(), c.EngineSrc())
	}

	// A chain failover: both models on one gateway, one of them named in the file.
	rc := &RolesConfig{ModelOpts: map[string]*ModelOpts{"hy3": {Engine: engineSGLang}}}
	o := &Orchestrator{cfg: cfg, providers: ps, roles: rc, agents: map[string]*Agent{}}
	s := &Session{orch: o, models: []string{"glm5.3", "hy3"}}
	s.useModel(0)
	if s.client.Engine() != "" {
		t.Fatalf("glm5.3 is named by nothing here and must stay unknown, got %q", s.client.Engine())
	}
	s.useModel(1)
	if s.client.Engine() != engineSGLang || s.client.EngineSrc() != EngineFromModel {
		t.Fatalf("a failover lost models.hy3.engine: %q (%v)", s.client.Engine(), s.client.EngineSrc())
	}
}

// T16. `dialect: "sglang"` in a config file names an ENGINE, and until now it was
// accepted and silently harmful: no dialect of that name exists, so hy3 fell into
// the generic arm and was sent a bare top-level reasoning_effort — the one shape
// its Jinja template raises on. It is translated, not obeyed and not refused.
func TestDialectSglangIsTranslatedNotObeyed(t *testing.T) {
	p := &Provider{ID: "gpu2", Name: "gpu2"}
	ProviderConfig{BaseURL: "http://gpu2:8000/v1", Dialect: engineSGLang}.apply(p)
	if p.Engine != engineSGLang {
		t.Fatalf("dialect: sglang must become engine: sglang, got %q", p.Engine)
	}
	if !p.selfHosted() {
		t.Fatal("naming an engine for an endpoint says it is self-hosted")
	}
	// engine: wins where both are given, and dialect: vllm keeps meaning a
	// self-hosted endpoint.
	p2 := &Provider{ID: "gpu3"}
	ProviderConfig{Dialect: engineVLLM}.apply(p2)
	if p2.Engine != engineVLLM || !p2.selfHosted() {
		t.Fatalf("dialect: vllm → %q", p2.Engine)
	}

	// The body: hy3's effort still travels in chat_template_kwargs, never as a
	// top-level field.
	c := &Client{provider: p, model: "hy3"}
	c.setEngine(p.Engine, EngineFromEndpoint)
	b := bodyOf(t, c, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, Thinking: "high"})
	if _, top := b["reasoning_effort"]; top {
		t.Fatalf("hy3 must never get a top-level reasoning_effort: %v", b)
	}
	kw, _ := b["chat_template_kwargs"].(map[string]any)
	if kw["reasoning_effort"] != "high" {
		t.Fatalf("hy3's effort must travel as a template kwarg: %v", b)
	}
}

// doctorText runs doctor with stdout captured, so a test can assert on the lines
// an operator actually reads.
func doctorText(t *testing.T, cfg Config, args ...string) (string, int) {
	t.Helper()
	code := 0
	out := captureStdout(t, func() { code = runDoctor(context.Background(), cfg, args) })
	return out, code
}

// doctorCfg is a project root with a roles.yaml naming one model, which is what
// doctor needs to say anything at all.
func doctorCfg(t *testing.T, url, model string) Config {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".lca"), 0o755)
	roles := "entry: lead\ntransport: native\n\nroles:\n  lead:\n    description: d\n    models: [" + model + "]\n    prompt: p\n"
	os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"), []byte(roles), 0o644)
	t.Setenv("LCA_ROLES", "")
	t.Setenv("LCA_ENGINE", "")
	return Config{Root: root, Dir: t.TempDir(), BaseURL: url, Endpoints: []string{url}, Model: model,
		Allowed: []string{"ls"}, EngineProbe: true, Temperature: -1}
}

// T17. doctor names the engine and who said so, in the gateway section where the
// url is — and when nobody has said, it says that out loud: both ways to set it,
// and what lca therefore sends.
func TestDoctorNamesTheEngineAndItsProvenance(t *testing.T) {
	sgl := newSGLangServer(t, okReply)
	sgl.models, sgl.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
	out, _ := doctorText(t, doctorCfg(t, sgl.URL+"/v1", "hy3"), "-no-probe")
	if !strings.Contains(plain(out), "sglang") || !strings.Contains(plain(out), "owned_by") {
		t.Fatalf("doctor must name the engine and its source:\n%s", out)
	}

	vllm := newVLLMServer(t, okReply)
	vllm.models, vllm.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
	out, _ = doctorText(t, doctorCfg(t, vllm.URL+"/v1", "hy3"), "-no-probe")
	if !strings.Contains(plain(out), "vllm") {
		t.Fatalf("doctor must name vllm when the cards say so:\n%s", out)
	}

	// The operator's own setting, named as such.
	cfg := doctorCfg(t, vllm.URL+"/v1", "hy3")
	cfg.Engine = engineSGLang
	t.Setenv("LCA_ENGINE", engineSGLang)
	out, _ = doctorText(t, cfg, "-no-probe")
	if !strings.Contains(plain(out), "sglang") || !strings.Contains(plain(out), "LCA_ENGINE") {
		t.Fatalf("the operator's setting must outrank owned_by and be named:\n%s", out)
	}

	// Unknown: the warn, both set-commands, and what lca therefore sends.
	gw := newBlindGateway(t, okReply)
	gw.models = []string{"hy3"}
	out, code := doctorText(t, doctorCfg(t, gw.URL+"/v1", "hy3"), "-no-probe")
	for _, want := range []string{"nothing engine-specific", "/set engine", "models.hy3.engine"} {
		if !strings.Contains(plain(out), want) {
			t.Errorf("an unknown engine must say %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Errorf("an unknown engine is a working configuration, not a failure (exit %d)", code)
	}
}

// T3. A gateway that proxies /v1 and nothing else: doctor says the engine's own
// routes are not proxied, infers NOTHING from the 404, and still resolves the
// window from /v1/models. "The gateway does not proxy it" is never "the engine
// lacks it".
func TestGatewayThatDoesNotProxyNonV1(t *testing.T) {
	gw := newBlindGateway(t, okReply)
	gw.models, gw.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
	cfg := doctorCfg(t, gw.URL+"/v1", "hy3")
	cfg.Engine = engineSGLang // so the parser advice is SGLang's and the probe is attempted
	out, code := doctorText(t, cfg, "-no-probe")
	if !strings.Contains(plain(out), "does not proxy") {
		t.Fatalf("doctor must say the routes are not proxied:\n%s", out)
	}
	for _, never := range []string{"no parser is loaded", "no reasoning parser"} {
		if strings.Contains(plain(out), never) {
			t.Errorf("a 404 must infer nothing about the parsers: %q\n%s", never, out)
		}
	}
	if !strings.Contains(plain(out), "262k") || code != 0 {
		t.Fatalf("the window must still come from /v1/models (exit %d):\n%s", code, out)
	}
}

// T4. The synthesized card — owned_by "local", no max_model_len — leaves the
// engine UNKNOWN, and UNKNOWN sends nothing engine-specific.
func TestGatewaySynthesizedCardsLeaveEngineUnknown(t *testing.T) {
	gw := newBlindGateway(t, okReply)
	gw.models = []string{"hy3"}
	cfg := Config{BaseURL: gw.URL + "/v1", Model: "hy3", Temperature: -1}
	c := NewClient(cfg)
	ps := NewProviders(cfg, nil, c)
	ps.Learn(c)
	if c.Engine() != "" || c.EngineSrc() != EngineUnset {
		t.Fatalf("a synthesized card must leave the engine unknown: %q (%v)", c.Engine(), c.EngineSrc())
	}
	b := bodyOf(t, c, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, ContinueFinal: true, Thinking: "high"})
	raw, _ := json.Marshal(b)
	for _, never := range []string{"separate_reasoning", "stream_reasoning", "thinking_budget", "max_completion_tokens"} {
		if strings.Contains(string(raw), never) {
			t.Errorf("an unknown engine must send no %s: %s", never, raw)
		}
	}
}

// T5. UNKNOWN means "send nothing engine-specific", never a guess: with no
// engine named, the body is byte-for-byte the one vLLM gets — today's status quo,
// which is not a new guess — and it carries none of the fields a plausible guess
// would have added.
func TestUnknownEngineSendsNothingEngineSpecific(t *testing.T) {
	models := []string{"hy3", "glm5.3", "kimi-k3", "qwen3.6", "MiniMax-M3", "deepseek-v4.1-flash"}
	for _, model := range models {
		for _, mode := range []string{"", "on", "off", "low", "high", "max"} {
			bodyFor := func(engine string) string {
				c := &Client{provider: &Provider{ID: "local", Local: true, Dialect: "vllm"}, model: model}
				c.setEngine(engine, EngineFromEndpoint)
				raw, err := c.body(ChatRequest{Thinking: mode, Messages: []Message{
					{Role: "user", Content: "hi"},
					{Role: "assistant", Content: "partial", Reasoning: "r"},
				}}, false)
				if err != nil {
					t.Fatalf("body: %v", err)
				}
				return string(raw)
			}
			unknown, vllm := bodyFor(""), bodyFor(engineVLLM)
			if unknown != vllm {
				t.Errorf("%s/%s: an unknown engine must send exactly what vllm sends\n unknown %s\n vllm    %s", model, mode, unknown, vllm)
			}
			var b map[string]any
			json.Unmarshal([]byte(unknown), &b)
			for _, never := range []string{"separate_reasoning", "stream_reasoning", "reasoning", "thinking_budget", "max_completion_tokens"} {
				if _, ok := b[never]; ok {
					t.Errorf("%s/%s: %s is unsourced for this client and must not be sent: %s", model, mode, never, unknown)
				}
			}
		}
	}
}

// T6. The whole engine difference in a request body is one key, and only on a
// continuation: add_generation_prompt is not a declared SGLang request field (it
// passes add_generation_prompt=True to the template as a literal), and SGLang
// suppresses the generation prompt from continue_final_message by itself.
func TestSGLangBodyDiffersOnlyInAddGenerationPrompt(t *testing.T) {
	bodyFor := func(engine string, cont bool) map[string]any {
		c := &Client{provider: &Provider{ID: "local", Local: true, Dialect: "vllm"}, model: "hy3"}
		c.setEngine(engine, EngineFromEndpoint)
		// No reasoning on the final message: the reasoning drop is its own change.
		return bodyOf(t, c, ChatRequest{ContinueFinal: cont, Thinking: "high", Messages: []Message{
			{Role: "user", Content: "hi"}, {Role: "assistant", Content: "partial"},
		}})
	}
	diff := func(a, b map[string]any) []string {
		var out []string
		for _, m := range []map[string]any{a, b} {
			for k := range m {
				x, _ := json.Marshal(a[k])
				y, _ := json.Marshal(b[k])
				if string(x) != string(y) && !contains(out, k) {
					out = append(out, k)
				}
			}
		}
		return out
	}
	if got := diff(bodyFor(engineSGLang, true), bodyFor(engineVLLM, true)); len(got) != 1 || got[0] != "add_generation_prompt" {
		t.Fatalf("a continuation must differ in exactly add_generation_prompt, got %v", got)
	}
	if got := diff(bodyFor(engineSGLang, false), bodyFor(engineVLLM, false)); len(got) != 0 {
		t.Fatalf("without a continuation the bodies must be identical, got %v", got)
	}
	if _, ok := bodyFor(engineSGLang, true)["continue_final_message"]; !ok {
		t.Fatal("the continuation itself must still be requested on SGLang")
	}
}

// T7. The regression guard. hy3's effort travels in chat_template_kwargs on
// EVERY engine, and never as a top-level field: the "portable" top-level
// reasoning_effort: "none" is a 400 on every turn on any SGLang without the
// hunyuan_effort normalizer and on vLLM, because the string reaches Jinja and the
// template raises. The kwarg carrier is the one that is safe in all four
// deployments lca can meet.
func TestHy3EffortCarrierIsTheKwargOnEveryEngine(t *testing.T) {
	for _, engine := range []string{"", engineVLLM, engineSGLang} {
		for mode, want := range map[string]string{"on": "high", "off": "no_think", "low": "low", "high": "high"} {
			c := &Client{provider: &Provider{ID: "local", Local: true, Dialect: "vllm"}, model: "hy3"}
			c.setEngine(engine, EngineFromEndpoint)
			b := bodyOf(t, c, ChatRequest{Thinking: mode, Messages: []Message{{Role: "user", Content: "hi"}}})
			if _, top := b["reasoning_effort"]; top {
				t.Fatalf("engine %q, %s: a top-level reasoning_effort is a 400 every turn: %v", engine, mode, b)
			}
			kw, _ := b["chat_template_kwargs"].(map[string]any)
			if kw["reasoning_effort"] != want {
				t.Fatalf("engine %q, %s: chat_template_kwargs.reasoning_effort = %v, want %q", engine, mode, kw["reasoning_effort"], want)
			}
		}
		// Nothing outside the safe-everywhere intersection, ever — and for hy3 that
		// is now the whole of the rule, because an undocumented level FOLDS to a
		// documented one rather than vanishing: hy3 is the one model whose card insists a level is always NAMED: its
		// two template variants disagree about what "no level" means (high on one, no
		// thinking at all on the other, under the same parser name), so a dropped
		// level used to send the one request shape the card says never to send. An
		// undocumented level now folds to the deeper documented one — the invariant
		// that matters is unchanged: what goes on the wire is always inside
		// no_think|low|high.
		for _, mode := range []string{"none", "medium", "max", "xhigh", "minimal"} {
			c := &Client{provider: &Provider{ID: "local", Local: true, Dialect: "vllm"}, model: "hy3"}
			c.setEngine(engine, EngineFromEndpoint)
			raw, _ := c.body(ChatRequest{Thinking: mode, Messages: []Message{{Role: "user", Content: "hi"}}}, false)
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			kw, _ := got["chat_template_kwargs"].(map[string]any)
			lvl, _ := kw["reasoning_effort"].(string)
			switch mode {
			case "none":
				if lvl != "no_think" {
					t.Fatalf("engine %q: off must be spelled no_think, got %q: %s", engine, lvl, raw)
				}
			default:
				if lvl != "high" {
					t.Fatalf("engine %q: %q must fold to high, got %q: %s", engine, mode, lvl, raw)
				}
			}
			if _, top := got["reasoning_effort"]; top {
				t.Fatalf("engine %q: a top-level reasoning_effort is a 400 every turn: %s", engine, raw)
			}
		}
	}
}

// T8. hy3's chat_template_kwargs carries the EFFORT and nothing else. A
// `preserved_thinking: true` kwarg was proposed here; no source names it, so it
// is not sent — and this is the assertion that says so, in place of the one that
// would have required weakening the two guards in models_test.go.
func TestHy3SendsNoUnsourcedTemplateKwarg(t *testing.T) {
	for _, noReplay := range []bool{false, true} {
		for _, engine := range []string{"", engineVLLM, engineSGLang} {
			c := NewClient(Config{BaseURL: "http://x/v1", Model: "hy3", Temperature: -1})
			c.noReplay = noReplay
			c.setEngine(engine, EngineFromEndpoint)
			b := bodyOf(t, c, ChatRequest{Thinking: "high", Messages: []Message{{Role: "user", Content: "hi"}}})
			kw, _ := b["chat_template_kwargs"].(map[string]any)
			if kw["reasoning_effort"] != "high" {
				t.Fatalf("engine %q: the effort is the one thing hy3's kwargs carry: %v", engine, b)
			}
			// Every kwarg a reader of the sources might have been tempted by, named
			// so this stays a test about the rule and not about one word.
			for _, never := range []string{"preserved_thinking", "preserve_thinking", "interleaved_thinking", "enable_thinking", "thinking", "clear_thinking", "thinking_budget"} {
				if _, ok := kw[never]; ok {
					t.Errorf("engine %q: %s has no source and must not be sent: %v", engine, never, b)
				}
			}
			if len(kw) != 1 {
				t.Errorf("engine %q: hy3's kwargs must hold the effort alone, got %v", engine, kw)
			}
		}
	}
}

// sglangToolRegistry is --tool-call-parser's choices as the source quotes them,
// which is from `main` — NOT from the released tag. The list therefore contains
// main-only names (deepseekv41 is one the source says so of), so membership here
// proves a name is spelled the way SGLang spells it and does NOT prove a released
// server's argparse accepts it. What is pinned per model below is the spelling;
// the release boundary is a thing no source gives, so nothing asserts it.
//
// The prose docs lag this module badly: tool_parser.mdx lists 14 names and has no
// row for hunyuan, glm47, kimi_k3 or minimax-m3, and DOES have a row for llama4,
// which is in no registry.
//
//	https://raw.githubusercontent.com/sgl-project/sglang/main/python/sglang/srt/function_call/parser_names.py
var sglangToolRegistry = []string{
	"apertus2509", "cohere_command4", "deepseekv3", "deepseekv31", "deepseekv32", "deepseekv4",
	"deepseekv41", "dots", "glm", "glm45", "glm47", "gpt-oss", "k2_horizon", "kimi_k2", "kimi_k3",
	"lfm2", "ling3", "llama3", "mimo", "minicpm5", "mistral", "muse", "poolside_v1", "pythonic",
	"qwen", "qwen25", "qwen3_coder", "spark25", "step3", "step3p5", "minimax-m2", "minimax-m3",
	"nanbeige", "trinity", "interns1", "hermes", "hunyuan", "gigachat3", "gigachat35", "gemma4",
	"inkling", "iquest_q1",
}

// T9. Every SGLang parser value lca records is spelled the way SGLang's own
// registry spells it, because a value argparse rejects fails server startup
// rather than being ignored — and the three traps are pinned by name. The
// registry quoted is `main`'s, which is the only one the source gives, so this
// catches a misspelling and not a value too new for the operator's tag.
//
// The reasoning registry is not reproduced as a list: the source quotes
// --reasoning-parser's choices only in part, and a list half-copied from a
// truncated quote is exactly the unsourced data this project refuses. What IS
// sourced per model is asserted instead.
func TestSGLangParserValuesAreRegistryMembers(t *testing.T) {
	for _, id := range []string{"hy3", "glm5.3", "kimi-k3", "qwen3.6", "MiniMax-M3", "deepseek-v4.1-flash"} {
		p := lookupProfile(id)
		if v := p.ToolParser.SGLang; v != "" && !contains(sglangToolRegistry, v) {
			t.Errorf("%s: --tool-call-parser %q is in no SGLang registry the source quotes (main's) — the launch would fail with unrecognized choice", id, v)
		}
	}
	for _, tc := range []struct{ id, tool, reason string }{
		// hunyuan in BOTH registries; hy_v3 (the model_type, and vLLM's own name) is
		// in neither.
		{"hy3", "hunyuan", "hunyuan"},
		// glm47 is a TOOL-call parser only; the reasoning one is glm45, and
		// --reasoning-parser glm47 fails SGLang's startup.
		{"glm5.3", "glm47", "glm45"},
		// Hyphen, not lca's vLLM-spelled underscore.
		{"MiniMax-M3", "minimax-m3", "minimax-m3"},
		{"qwen3.6", "qwen3_coder", "qwen3"},
		{"kimi-k3", "kimi_k3", "kimi_k3"},
		// Nothing is recorded: no source maps this checkpoint to a parser name on
		// SGLang, and inheriting deepseekv4's value is the sibling guess this file
		// forbids.
		{"deepseek-v4.1-flash", "", ""},
	} {
		p := lookupProfile(tc.id)
		if p.ToolParser.SGLang != tc.tool || p.ReasonParse.SGLang != tc.reason {
			t.Errorf("%s: SGLang parsers are %q/%q, want %q/%q", tc.id, p.ToolParser.SGLang, p.ReasonParse.SGLang, tc.tool, tc.reason)
		}
		// And the vLLM values are untouched by any of this.
		if p.ToolParser.VLLM == "" && tc.id != "deepseek-v4.1-flash" {
			t.Errorf("%s: the vLLM value must stay as it was", tc.id)
		}
	}
	if lookupProfile("hy3").ToolParser.VLLM != "hy_v3" || lookupProfile("glm5.3").ReasonParse.VLLM != "glm47" {
		t.Error("the vLLM spellings are unchanged by the SGLang ones")
	}
}

// T10. Advice is in the resolved engine's own spelling, and never cross-quoted: a
// vLLM parser value fails SGLang's argparse, and --enable-auto-tool-choice does
// not exist in SGLang at all — argparse fails the launch with "unrecognized
// arguments", so that string in an SGLang line is advice that stops the server.
func TestEngineFlagsNeverCrossQuote(t *testing.T) {
	hy3, flash := lookupProfile("hy3"), lookupProfile("deepseek-v4.1-flash")

	sgl := plain(hy3.engineFlags(engineSGLang))
	if strings.Contains(sgl, "--enable-auto-tool-choice") {
		t.Errorf("SGLang has no such flag: %q", sgl)
	}
	if !strings.HasPrefix(sgl, "--tool-call-parser auto --reasoning-parser auto") {
		t.Errorf("SGLang advice must lead with auto: %q", sgl)
	}
	if strings.Contains(sgl, "hy_v3") {
		t.Errorf("the vLLM token must never appear in SGLang advice: %q", sgl)
	}

	// Nothing sourced for this engine: say so, do not borrow the other token.
	none := plain(flash.engineFlags(engineSGLang))
	if strings.Contains(none, "deepseek_v41") || !strings.Contains(none, "no parser value is recorded") {
		t.Errorf("an unrecorded value must say so: %q", none)
	}

	if got := plain(hy3.engineFlags(engineVLLM)); got != "--enable-auto-tool-choice --tool-call-parser hy_v3 --reasoning-parser hy_v3" {
		t.Errorf("the vLLM line is what it always was, got %q", got)
	}
	both := plain(hy3.engineFlags(""))
	if !strings.Contains(both, "vLLM:") || !strings.Contains(both, "SGLang:") {
		t.Errorf("with no engine known, both are named: %q", both)
	}
}

// T11. Both of SGLang's own refusal messages name the window, and the existing
// extractor already reads them — pinned here so a future edit to the regex
// cannot quietly stop learning the number that costs a session to re-learn.
func TestSGLangWindowRefusalStringsAreLearned(t *testing.T) {
	for _, c := range []struct {
		body string
		want int
	}{
		{`The input (300000 tokens) is longer than the model's context length (262144 tokens).`, 262144},
		{`{"object":"error","message":"Requested token count exceeds the model's maximum context length of 262144 tokens. You requested a total of 300016 tokens: 16 tokens from the messages and 300000 tokens for the completion. Please reduce the number of tokens.","type":"BadRequestError","param":null,"code":400}`, 262144},
	} {
		if got := statedWindow(&APIError{Status: 400, Body: c.body}); got != c.want {
			t.Errorf("statedWindow(%.50s…) = %d, want %d", c.body, got, c.want)
		}
	}
}

// T12. A tool-call parse failure on SGLang is an HTTP 200: tool_calls null,
// finish_reason "stop", the raw markup left in content. hy3's markup contains no
// "{" at all, so the old name-plus-brace detector called it "answered without
// calling the tool" and hid the cause on the operator's own model.
func TestSGLangToolParseFailureIsA200(t *testing.T) {
	const markup = `<tool_calls><tool_call>ping<tool_sep><arg_key>value</arg_key><arg_value>ok</arg_value></tool_call></tool_calls>`
	fs := newSGLangServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: markup} })
	fs.models, fs.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
	cfg := doctorCfg(t, fs.URL+"/v1", "hy3")
	cfg.Engine = engineSGLang
	gw := NewClient(cfg)
	rc, _ := loadRoles(cfg)

	res := probeModel(context.Background(), cfg, gw, rc, "lead", "hy3")
	if res.status != "fail" || !strings.Contains(res.detail, "tool-call parser") {
		t.Fatalf("a 200 with the markup in content is a parser fault, not a refusal to call: %+v", res)
	}
	if !strings.Contains(plain(res.fix), "SGLang: --tool-call-parser auto") {
		t.Fatalf("the fix must be in SGLang's spelling: %q", plain(res.fix))
	}
	if strings.Contains(plain(res.fix), "--enable-auto-tool-choice --tool-call-parser auto") {
		t.Fatalf("and must not carry vLLM's flag: %q", plain(res.fix))
	}
	// Markers only, no braces anywhere: the whole point of the broadened detector.
	if strings.Contains(markup, "{") {
		t.Fatal("this fixture is meant to have no JSON braces in it")
	}
	if !unparsedToolCall(`<tool_call>note<arg_key>spec<arg_value>{"title":"a"}`, "note") {
		t.Error("GLM-5.3's markup must be recognised too")
	}
	if unparsedToolCall("I would call ping, but I will explain instead.", "ping") {
		t.Error("prose that merely mentions the tool is not unparsed markup")
	}
}

// T13. The two SGLang failures that are identifiable from the body alone, and the
// flat envelope that used to leave APIError.Type empty.
func TestSGLangDiagnosticHints(t *testing.T) {
	five := errorHint(&APIError{Status: 500, Body: `{"object":"error","message":"Failed to parse reasoning content","type":"InternalServerError","param":null,"code":500}`})
	if !strings.Contains(five, "--reasoning-parser") {
		t.Errorf("a 500 \"Failed to parse reasoning content\" names the parser: %q", five)
	}
	four := errorHint(&APIError{Status: 400, Body: `{"object":"error","message":"function.arguments must be a JSON object.","type":"BadRequest","param":null,"code":400}`})
	if !strings.Contains(four, "/compact") {
		t.Errorf("the replayed-arguments 400 names the way out: %q", four)
	}
	typ, env := errorType([]byte(`{"object":"error","message":"nope","type":"BadRequest","param":null,"code":400}`))
	if typ != "BadRequest" || env != "flat" {
		t.Errorf("the flat envelope must fill Type: %q (%q)", typ, env)
	}
	typ, env = errorType([]byte(`{"error":{"message":"nope","type":"berserk_gw_overload"}}`))
	if typ != "berserk_gw_overload" || env != "wrapped" {
		t.Errorf("the wrapped envelope is unchanged: %q (%q)", typ, env)
	}
	if _, env = errorType([]byte(`<html>502</html>`)); env != "" {
		t.Errorf("a non-JSON body has no envelope, got %q", env)
	}
}

// T14. The prefix is what the gateway's KV cache is keyed on, so the system
// prompt, the tool schemas and the affinity header must be byte-identical
// whatever the engine is. Nothing engine-derived may reach them.
func TestPrefixIsIndependentOfEngine(t *testing.T) {
	fs := newFakeServer(t, okReply)
	fs.models = []string{"hy3"}
	h := newHarness(t, fs.URL, "native", true)
	want, wantHdr := "", ""
	for _, e := range []string{"", engineVLLM, engineSGLang} {
		h.sess.client.resetEngine()
		h.sess.client.setEngine(e, EngineFromEndpoint)
		h.sess.RefreshSystem()
		_, schemas := h.sess.tools()
		raw, _ := json.Marshal(schemas)
		got := h.sess.Msgs[0].Content + "\x00" + string(raw)
		hdr, _ := json.Marshal(h.sess.headers())
		if want == "" {
			want, wantHdr = got, string(hdr)
			continue
		}
		if got != want {
			t.Fatalf("engine %q changed the cached prefix", e)
		}
		if string(hdr) != wantHdr {
			t.Fatalf("engine %q changed the KV-affinity headers", e)
		}
	}
}

// T18. Reading the parsers instead of advising them, and the three answers that
// are not interchangeable: the parser that is loaded, the key that says none is,
// and a gateway that does not proxy the route at all.
func TestDoctorReadsTheParsersWhenReachable(t *testing.T) {
	agrees := newSGLangServer(t, okReply)
	agrees.models, agrees.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
	out, _ := doctorText(t, doctorCfg(t, agrees.URL+"/v1", "hy3"), "-no-probe")
	if !strings.Contains(plain(out), "--reasoning-parser hunyuan") || !strings.Contains(plain(out), "they agree with what hy3's vendor documents") {
		t.Fatalf("doctor must report the parser in force and that it agrees:\n%s", plain(out))
	}
	if !strings.Contains(plain(out), "version 0.5.20") {
		t.Errorf("the version is only reported here, so it must be printed:\n%s", plain(out))
	}

	none := newSGLangServer(t, okReply)
	none.models, none.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
	none.native = map[string]string{"/model_info": `{"served_model_name":"hy3","reasoning_parser":null,"tool_call_parser":null}`}
	out, _ = doctorText(t, doctorCfg(t, none.URL+"/v1", "hy3"), "-no-probe")
	if !strings.Contains(plain(out), "no parser is loaded for --reasoning-parser") {
		t.Fatalf("a null parser is the cause of inline reasoning and must be named:\n%s", plain(out))
	}

	// A wrong parser: the exact flag to change, in SGLang's spelling.
	wrong := newSGLangServer(t, okReply)
	wrong.models, wrong.windows = []string{"glm5.3"}, map[string]int{"glm5.3": 1_048_576}
	wrong.native = map[string]string{"/model_info": `{"served_model_name":"glm5.3","reasoning_parser":"glm47","tool_call_parser":"glm47"}`}
	out, _ = doctorText(t, doctorCfg(t, wrong.URL+"/v1", "glm5.3"), "-no-probe")
	// Both values AND the engine whose spelling the right-hand one is: glm47 is the
	// correct --reasoning-parser nowhere, but which token replaces it depends on
	// the engine, so a line without the engine named is a line that cannot be acted
	// on.
	if !strings.Contains(plain(out), "--reasoning-parser is glm47, but glm-5.3's own on SGLang is glm45") {
		t.Fatalf("a disagreement must name both values and the engine:\n%s", plain(out))
	}

	// Not proxied: no claim either way.
	blind := newBlindGateway(t, okReply)
	blind.models, blind.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
	out, _ = doctorText(t, doctorCfg(t, blind.URL+"/v1", "hy3"), "-no-probe")
	if strings.Contains(plain(out), "no parser is loaded") || strings.Contains(plain(out), "they agree with what") {
		t.Fatalf("a 404 licenses no claim about the parsers:\n%s", plain(out))
	}
}

// T19. A continuation on SGLang carries no reasoning on the message being
// continued: only a plain-string assistant message with no tool calls and no
// reasoning content can be continued there, and anything else is silently
// demoted to a closed historical turn — the model starts a new turn and the
// partial text is orphaned, with no error at all.
func TestContinuationDropsReasoningOnSGLang(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "earlier", Reasoning: "kept"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "partial", Reasoning: "dropped"},
	}
	for _, tc := range []struct{ engine, final string }{
		{engineSGLang, ""}, {engineVLLM, "dropped"}, {"", "dropped"},
	} {
		c := &Client{provider: &Provider{ID: "local", Local: true, Dialect: "vllm"}, model: "kimi-k3"}
		c.setEngine(tc.engine, EngineFromEndpoint)
		b := bodyOf(t, c, ChatRequest{ContinueFinal: true, Messages: msgs})
		ws, _ := b["messages"].([]any)
		last, _ := ws[len(ws)-1].(map[string]any)
		first, _ := ws[1].(map[string]any)
		if got, _ := last["reasoning_content"].(string); got != tc.final {
			t.Errorf("engine %q: the continued message's reasoning_content is %q, want %q", tc.engine, got, tc.final)
		}
		if got, _ := first["reasoning_content"].(string); got != "kept" {
			t.Errorf("engine %q: an earlier turn must keep its reasoning, got %q", tc.engine, got)
		}
	}
	// Without a continuation, SGLang gets the whole history exactly as vLLM does.
	c := &Client{provider: &Provider{ID: "local", Local: true, Dialect: "vllm"}, model: "kimi-k3"}
	c.setEngine(engineSGLang, EngineFromEndpoint)
	b := bodyOf(t, c, ChatRequest{Messages: msgs})
	ws, _ := b["messages"].([]any)
	last, _ := ws[len(ws)-1].(map[string]any)
	if got, _ := last["reasoning_content"].(string); got != "dropped" {
		t.Errorf("only a continuation drops it, got %q", got)
	}
}

// T20. The parser probe declares its nested parameter as {"type":"object"},
// which is required rather than tidy: hy3's hunyuan detector is schema-driven and
// keeps a JSON-looking value in a string-typed parameter as a literal string, so
// a schema omission here would be reported as the server's parser failure.
func TestParserProbeSchemaDeclaresTheNestedParamAsObject(t *testing.T) {
	fs := newSGLangServer(t, func(fakeRequest, int) fakeReply {
		return fakeReply{calls: []ToolCall{
			call("n1", "note", map[string]any{"spec": map[string]any{"title": "a", "tags": []string{"x", "y"}}}),
			call("n2", "note", map[string]any{"spec": map[string]any{"title": "b", "tags": []string{"z"}}}),
		}}
	})
	fs.models = []string{"hy3"}
	c := NewClient(Config{BaseURL: fs.URL + "/v1", Model: "hy3", Temperature: -1})
	var res probeResult
	probeParser(context.Background(), c, &res)

	reqs := fs.reqs()
	if len(reqs) == 0 {
		t.Fatal("the parser probe sent nothing")
	}
	params := reqs[len(reqs)-1].Tools[0].Function.Parameters
	props, _ := params["properties"].(map[string]any)
	spec, _ := props["spec"].(map[string]any)
	if spec["type"] != "object" {
		t.Fatalf("the nested parameter must be declared as an object: %v", params)
	}
	if len(res.warns) != 0 {
		t.Fatalf("a server that parses both calls must raise no warning: %v", res.warns)
	}
}

// The translated dialect is also SAID: a file whose dialect: names an engine is
// read the way it was meant and told which key that belongs under, because the
// two questions are different and only one of them has an answer called
// "sglang".
func TestDoctorNamesTheTranslatedDialect(t *testing.T) {
	fs := newVLLMServer(t, okReply)
	fs.models, fs.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
	cfg := doctorCfg(t, fs.URL+"/v1", "hy3")
	os.WriteFile(filepath.Join(cfg.Dir, "config.json"),
		[]byte(`{"providers":{"gpu2":{"base_url":"http://gpu2:8000/v1","dialect":"sglang"}}}`), 0o644)
	out, code := doctorText(t, cfg, "-no-probe")
	if !strings.Contains(plain(out), "providers.gpu2.dialect is \"sglang\"") || !strings.Contains(plain(out), `"engine": "sglang"`) {
		t.Fatalf("doctor must name the key to move it to:\n%s", plain(out))
	}
	if code != 0 {
		t.Errorf("a translated dialect is a warning, not a failure (exit %d)", code)
	}
}

// The hy3 variant question, answered by observation. Two chat-template variants
// of this checkpoint disagree about what "no reasoning_effort" means — unset is
// high on a hunyuan_effort template and no thinking at all on an Hy3-preview one
// — both run under --reasoning-parser hunyuan, and nothing in the OpenAI surface
// distinguishes them. So doctor sends one request with no level and reports what
// came back; lca itself never relies on either default.
func TestDoctorReportsWhichHy3VariantAnswered(t *testing.T) {
	variant := func(thinks bool) probeResult {
		fs := newSGLangServer(t, func(req fakeRequest, n int) fakeReply {
			// The probe is the request that carries NO reasoning_effort.
			if strings.Contains(req.Body, "reasoning_effort") {
				return fakeReply{content: "done", reasoning: "asked for a level"}
			}
			if thinks {
				return fakeReply{content: "done", reasoning: "unset meant high here"}
			}
			return fakeReply{content: "done"}
		})
		fs.models, fs.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
		c := NewClient(Config{BaseURL: fs.URL + "/v1", Model: "hy3", Temperature: -1, Engine: engineSGLang})
		c.setEngine(engineSGLang, EngineFromEndpoint)
		var res probeResult
		probeHy3Variant(context.Background(), c, &res)
		if len(res.notes) != 1 {
			t.Fatalf("the variant must be reported exactly once: %v / %v", res.notes, res.warns)
		}
		return res
	}
	// Both lines report the OBSERVATION first and name the template only as what
	// the observation is consistent with. One request on a one-word prompt is one
	// sample, and the negative direction is the weak one: a model with nothing to
	// think about on "say done" looks exactly like a template that cannot think.
	// The sources list the variant as UNKNOWN, so neither line may close it.
	pos := variant(true).notes[0]
	if !strings.Contains(pos, "it thought") || !strings.Contains(pos, "consistent with a hunyuan_effort template") {
		t.Errorf("the positive observation must be reported as one: %q", pos)
	}
	neg := variant(false).notes[0]
	if !strings.Contains(neg, "did NOT think") || !strings.Contains(neg, "consistent with an Hy3-preview template") {
		t.Errorf("the negative observation must be reported as one: %q", neg)
	}
	if !strings.Contains(neg, "does not identify the template") {
		t.Errorf("one negative sample identifies nothing, and the line has to say so: %q", neg)
	}

	// And it asks only about hy3: no other model has two variants under one parser
	// name, so no other model pays for the request.
	fs := newSGLangServer(t, okReply)
	fs.models = []string{"glm5.3"}
	c := NewClient(Config{BaseURL: fs.URL + "/v1", Model: "glm5.3", Temperature: -1})
	var res probeResult
	probeHy3Variant(context.Background(), c, &res)
	if len(res.notes) != 0 || len(fs.reqs()) != 0 {
		t.Errorf("only hy3 is probed for a template variant: %v / %d requests", res.notes, len(fs.reqs()))
	}
}

// T21. The blocker. A roles.yaml per-model engine is the strongest source there
// is, which means a copy that carries it cannot be corrected by anything: a
// client copied for ANOTHER model must forget it, exactly where the same copy
// forgets the window. Both are facts about one (endpoint, model) pair, and the
// model is what just changed.
func TestAPerModelEnginePinDoesNotEscapeItsModel(t *testing.T) {
	cfg := Config{BaseURL: "http://gw.invalid/v1", Model: "hy3", Temperature: -1}
	ps := NewProviders(cfg, nil, NewClient(cfg))
	// One gateway, two models, neither card naming an engine — so the file is the
	// only thing that says anything, and it says it about one of them.
	ps.LearnWindows(cfg.BaseURL, []ModelInfo{
		{ID: "hy3", MaxLen: 262_144}, {ID: "glm5.3", MaxLen: 1_048_576},
	})
	rc := &RolesConfig{ModelOpts: map[string]*ModelOpts{"hy3": {Engine: engineSGLang}}}
	o := &Orchestrator{cfg: cfg, providers: ps, roles: rc, agents: map[string]*Agent{}}

	// /model does this to the REPL's own client, for the model it holds.
	o.applyModelEngine(ps.local)
	if ps.local.Engine() != engineSGLang || ps.local.EngineSrc() != EngineFromModel {
		t.Fatalf("models.hy3.engine must reach the client it was written for: %q (%v)", ps.local.Engine(), ps.local.EngineSrc())
	}

	s := &Session{orch: o, models: []string{"glm5.3", "hy3"}}
	s.useModel(0)
	if s.client.Engine() != "" || s.client.EngineSrc() != EngineUnset {
		t.Fatalf("hy3's pin escaped onto glm5.3: %q (%v)", s.client.Engine(), s.client.EngineSrc())
	}
	// And the wire proves it: a leaked "sglang" would drop the reasoning off the
	// message being continued and omit add_generation_prompt, on a model nobody
	// said anything about.
	b := bodyOf(t, s.client, ChatRequest{ContinueFinal: true, Messages: []Message{
		{Role: "user", Content: "q"}, {Role: "assistant", Content: "partial", Reasoning: "mine"},
	}})
	if b["add_generation_prompt"] != false {
		t.Errorf("an unknown engine keeps today's continuation pair: %v", b)
	}
	ws, _ := b["messages"].([]any)
	last, _ := ws[len(ws)-1].(map[string]any)
	if got, _ := last["reasoning_content"].(string); got != "mine" {
		t.Errorf("an unknown engine must not drop the continued message's reasoning, got %q", got)
	}
	// The model it WAS written for still gets it, on the way back.
	s.useModel(1)
	if s.client.Engine() != engineSGLang || s.client.EngineSrc() != EngineFromModel {
		t.Fatalf("hy3 lost its own pin: %q (%v)", s.client.Engine(), s.client.EngineSrc())
	}
}

// T22. /endpoint moves the ENDPOINT. The model it is serving did not move, so
// roles.yaml's models.<id>.engine still applies — and SetEndpoint forgets the
// engine on purpose, so something has to put tier 1 back. Without it the pin is
// lost silently, while the model it names is still loaded.
func TestEndpointSwitchKeepsThePerModelEnginePin(t *testing.T) {
	next := newFakeServer(t, okReply) // a gateway whose cards name no engine
	next.models, next.windows = []string{"hy3"}, map[string]int{"hy3": 131_072}
	here := newSGLangServer(t, okReply)
	here.models, here.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}

	h := newHarness(t, here.URL+"/v1", "native", true)
	h.orch.roles = &RolesConfig{ModelOpts: map[string]*ModelOpts{"hy3": {Engine: engineSGLang}}}
	local := h.orch.providers.local
	local.SetModel("hy3")
	h.orch.applyModelEngine(local)
	if local.Engine() != engineSGLang {
		t.Fatalf("setup: %q", local.Engine())
	}
	h.sess.client = local
	r := &Repl{cfg: h.orch.cfg, orch: h.orch, sess: h.sess, local: local, in: newStringInput("")}
	captureStdout(t, func() { r.cmdEndpoint(next.URL + "/v1") })

	// The window came from the new deployment, which is what proves the switch
	// actually happened and that the relearn ran.
	if local.CtxLen() != 131_072 {
		t.Fatalf("the window did not follow the endpoint: %d", local.CtxLen())
	}
	if local.Engine() != engineSGLang || local.EngineSrc() != EngineFromModel {
		t.Fatalf("/endpoint dropped models.hy3.engine: %q (%v)", local.Engine(), local.EngineSrc())
	}
}

// T23. Doctor advises each model's OWN engine's spellings. One url fronting both
// engines is the deployment this was written for: a model the file pins to vLLM
// must be compared against, and advised, vLLM's parser value — quoting SGLang's
// at it is a flag vLLM's argparse rejects, which stops the server from starting.
func TestDoctorAdvisesEachModelsOwnEngine(t *testing.T) {
	// The cards say sglang, /model_info reports glm47 for both flags, and the file
	// says THIS model is on vLLM — where glm47 is exactly what the vendor documents
	// for both. So there is nothing to fix, and the SGLang answer (glm45) must not
	// appear anywhere.
	fs := newSGLangServer(t, okReply)
	fs.models, fs.windows = []string{"glm5.3"}, map[string]int{"glm5.3": 1_048_576}
	fs.native = map[string]string{"/model_info": `{"served_model_name":"glm5.3","reasoning_parser":"glm47","tool_call_parser":"glm47"}`}
	cfg := doctorCfg(t, fs.URL+"/v1", "glm5.3")
	os.WriteFile(filepath.Join(cfg.Root, ".lca", "roles.yaml"), []byte(
		"entry: lead\ntransport: native\n\nmodels:\n  glm5.3: {engine: vllm}\n\nroles:\n  lead:\n    description: d\n    models: [glm5.3]\n    prompt: p\n"), 0o644)
	out, _ := doctorText(t, cfg, "-no-probe")
	if !strings.Contains(plain(out), "they agree with what glm-5.3's vendor documents") {
		t.Errorf("glm47 IS glm-5.3's vLLM parser, so there is nothing to fix:\n%s", plain(out))
	}
	if strings.Contains(plain(out), "glm45") {
		t.Errorf("SGLang's token must not be quoted at a model pinned to vLLM:\n%s", plain(out))
	}
	// And the models section's flags row is the vLLM one, for the same model.
	if !strings.Contains(plain(out), "--enable-auto-tool-choice --tool-call-parser glm47 --reasoning-parser glm47") {
		t.Errorf("the per-model flags row must be the pinned engine's:\n%s", plain(out))
	}
	if strings.Contains(plain(out), "--tool-call-parser auto") {
		t.Errorf("--tool-call-parser auto is SGLang advice and must not reach a vLLM model:\n%s", plain(out))
	}
}

// T24. `auto` is not agreement with anything. It resolves from the chat template
// at launch, first-match-wins over an ordered rule list, and no source says what
// the server reports it resolved to — so counting it as agreement claims a fact
// lca does not have, and hides the one failure that looks exactly like a correct
// configuration.
func TestReportedAutoParserIsNotCountedAsAgreement(t *testing.T) {
	fs := newSGLangServer(t, okReply)
	fs.models, fs.windows = []string{"hy3"}, map[string]int{"hy3": 262_144}
	fs.native = map[string]string{"/model_info": `{"served_model_name":"hy3","reasoning_parser":"auto","tool_call_parser":"auto"}`}
	out, _ := doctorText(t, doctorCfg(t, fs.URL+"/v1", "hy3"), "-no-probe")
	if strings.Contains(plain(out), "they agree with what") {
		t.Errorf("auto agrees with nothing — what it picked is not reported anywhere:\n%s", plain(out))
	}
	for _, want := range []string{"--reasoning-parser auto", "not reported anywhere lca can read", "hy3's own on SGLang is hunyuan"} {
		if !strings.Contains(plain(out), want) {
			t.Errorf("the auto row must say %q:\n%s", want, plain(out))
		}
	}
}

// T25. providers.<id>.engine reaches the client, because doctor tells an operator
// whose file said dialect: "sglang" to move it into exactly that key. A key that
// is read, warned about and then ignored is worse than one that was refused.
func TestProviderEngineReachesTheClient(t *testing.T) {
	cfg := Config{BaseURL: "http://local.invalid/v1", Model: "hy3", Temperature: -1}
	fc := &FileConfig{Providers: map[string]ProviderConfig{
		"gpu2": {BaseURL: "http://gpu2:8000/v1", APIKey: "k", Engine: engineSGLang},
		// The translated form, which is the one doctor's warning is about.
		"gpu3": {BaseURL: "http://gpu3:8000/v1", APIKey: "k", Dialect: engineSGLang},
	}}
	ps := NewProviders(cfg, fc, NewClient(cfg))
	for _, id := range []string{"gpu2", "gpu3"} {
		c, err := ps.Client(id + "/hy3")
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if c.Engine() != engineSGLang || c.EngineSrc() != EngineFromEndpoint {
			t.Errorf("providers.%s.engine never reached the client: %q (%v)", id, c.Engine(), c.EngineSrc())
		}
		// Naming an engine for an endpoint says it is self-hosted, so the
		// template-vocabulary path is the one that applies.
		if !c.selfHosted() {
			t.Errorf("providers.%s: an endpoint with an engine is self-hosted", id)
		}
	}
	// A provider nobody named an engine for acquires none.
	plain, err := ps.Client("openai/gpt-5")
	if err == nil && plain.Engine() != "" {
		t.Errorf("a hosted provider must have no engine, got %q", plain.Engine())
	}
}

// T26. An answer the CARDS gave is recorded at the cards' tier, and the
// operator's at the operator's. endpointEngine reports which it was, and doctor
// records exactly that: flattening a card-derived answer onto the endpoint tier
// would let one model's owned_by outrank another model's own card, which is the
// collapse the per-(endpoint, model) design exists to prevent.
func TestEndpointEngineReportsItsOwnSource(t *testing.T) {
	served := map[string]ModelInfo{
		"hy3":     {ID: "hy3", OwnedBy: "sglang"},
		"kimi-k3": {ID: "kimi-k3", OwnedBy: "local"}, // a router's synthesized card votes not at all
	}
	if e, src := endpointEngine(Config{}, served); e != engineSGLang || src != EngineFromOwnedBy {
		t.Fatalf("the cards' own answer must come back at the cards' tier: %q (%v)", e, src)
	}
	if e, src := endpointEngine(Config{Engine: engineVLLM}, served); e != engineVLLM || src != EngineFromEndpoint {
		t.Fatalf("the operator's setting must come back at theirs: %q (%v)", e, src)
	}
	// Cards that disagree decide nothing: one url fronting two engines is the case
	// the per-model rows exist for, and a majority is not a fact.
	served["glm5.3"] = ModelInfo{ID: "glm5.3", OwnedBy: "vllm"}
	if e, _ := endpointEngine(Config{}, served); e != "" {
		t.Fatalf("mixed cards must leave the endpoint unknown, got %q", e)
	}
	// Recorded at the cards' tier, another model's own card can still speak for
	// itself. Recorded at the endpoint's, it could not.
	c := NewClient(Config{BaseURL: "http://gw.invalid/v1", Model: "glm5.3", Temperature: -1})
	c.setEngine(engineSGLang, EngineFromOwnedBy)
	c.setEngine(engineVLLM, EngineFromOwnedBy)
	if c.Engine() != engineVLLM {
		t.Fatalf("a card may be replaced by a card, got %q", c.Engine())
	}
}
