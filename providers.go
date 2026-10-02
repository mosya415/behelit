package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Providers. A model is addressed as "provider/model" (e.g.
// "deepseek/deepseek-v4-pro", "moonshot/kimi-k2.6"). A bare name, or one whose
// prefix is not a known provider (e.g. "Qwen/Qwen3-Coder-480B"), goes to the
// local endpoint (LCA_BASE_URL) exactly as before. Presets for every vendor of
// the supported open models are built in; keys come from the usual env vars,
// and the config file can add or override providers.

const (
	transportNative = "native" // API function calling (tools / tool_calls)
	transportText   = "text"   // line-anchored tags in plain text (protocol.go)
)

type Provider struct {
	ID        string
	Name      string
	BaseURL   string
	KeyEnv    []string // first set env var wins
	Key       string   // explicit key (config)
	Dialect   string   // thinking-parameter dialect (models.go)
	Transport string   // transportNative | transportText
	Local     bool     // the LCA_BASE_URL endpoint (vLLM/SGLang): self-host-only extras allowed
	// Engine is the inference engine this ENDPOINT is configured to be served by
	// ("sglang" | "vllm" | ""): LCA_ENGINE, providers.<id>.engine, /set engine. It
	// is the per-endpoint default; the per-(endpoint, model) fact in force lives on
	// the Client, because one gateway fronts both engines at once.
	Engine  string
	Models  []string // suggested model ids (completion / listing)
	Headers map[string]string
	Extra   map[string]any // extra request-body fields
}

// selfHosted is "this is the operator's own vLLM or SGLang", the question three
// sites used to ask as `Dialect == "vllm"`. Naming an engine for an endpoint is
// itself a statement that the endpoint is self-hosted — there is no hosted API
// whose engine lca would be told.
func (p *Provider) selfHosted() bool { return p != nil && (p.Local || p.Engine != "") }

func (p *Provider) apiKey() string {
	if p.Key != "" {
		return p.Key
	}
	for _, e := range p.KeyEnv {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	return ""
}

// presetProviders are the built-in hosted endpoints. All speak the
// OpenAI-compatible chat-completions API.
func presetProviders() []*Provider {
	return []*Provider{
		{ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", KeyEnv: []string{"DEEPSEEK_API_KEY"}, Dialect: "deepseek",
			Models: []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"}},
		{ID: "moonshot", Name: "Moonshot (Kimi)", BaseURL: "https://api.moonshot.ai/v1", KeyEnv: []string{"MOONSHOT_API_KEY", "KIMI_API_KEY"}, Dialect: "moonshot",
			Models: []string{"kimi-k2.6", "kimi-k2.5", "kimi-k2-thinking", "kimi-k2-0905-preview", "kimi-k2-turbo-preview"}},
		{ID: "moonshot-cn", Name: "Moonshot CN", BaseURL: "https://api.moonshot.cn/v1", KeyEnv: []string{"MOONSHOT_API_KEY", "KIMI_API_KEY"}, Dialect: "moonshot",
			Models: []string{"kimi-k2.6", "kimi-k2.5", "kimi-k2-thinking"}},
		{ID: "zai", Name: "Z.ai (GLM)", BaseURL: "https://api.z.ai/api/paas/v4", KeyEnv: []string{"ZAI_API_KEY", "ZHIPU_API_KEY"}, Dialect: "zai",
			Models: []string{"glm-5.2", "glm-5.1", "glm-5", "glm-4.7", "glm-4.6", "glm-4.5-air"}},
		{ID: "zai-coding", Name: "Z.ai Coding Plan", BaseURL: "https://api.z.ai/api/coding/paas/v4", KeyEnv: []string{"ZAI_API_KEY", "ZHIPU_API_KEY"}, Dialect: "zai",
			Models: []string{"glm-5.2", "glm-5.1", "glm-4.7"}},
		{ID: "zhipu", Name: "Zhipu BigModel (GLM)", BaseURL: "https://open.bigmodel.cn/api/paas/v4", KeyEnv: []string{"ZHIPU_API_KEY", "ZAI_API_KEY"}, Dialect: "zai",
			Models: []string{"glm-5.2", "glm-5.1", "glm-4.7", "glm-4.6"}},
		{ID: "dashscope", Name: "Alibaba DashScope (Qwen)", BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", KeyEnv: []string{"DASHSCOPE_API_KEY"}, Dialect: "dashscope",
			Models: []string{"qwen3-coder-plus", "qwen3-coder-flash", "qwen3.7-plus", "qwen3-max", "qwen3-coder-480b-a35b-instruct"}},
		{ID: "dashscope-cn", Name: "Alibaba DashScope CN", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", KeyEnv: []string{"DASHSCOPE_API_KEY"}, Dialect: "dashscope",
			Models: []string{"qwen3-coder-plus", "qwen3.7-plus", "qwen3-max"}},
		{ID: "minimax", Name: "MiniMax", BaseURL: "https://api.minimax.io/v1", KeyEnv: []string{"MINIMAX_API_KEY"}, Dialect: "minimax",
			Models: []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5"}},
		{ID: "minimax-cn", Name: "MiniMax CN", BaseURL: "https://api.minimaxi.com/v1", KeyEnv: []string{"MINIMAX_API_KEY"}, Dialect: "minimax",
			Models: []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5"}},
		{ID: "tencent", Name: "Tencent TokenHub (Hunyuan)", BaseURL: "https://tokenhub.tencentmaas.com/v1", KeyEnv: []string{"TENCENT_TOKENHUB_API_KEY", "HUNYUAN_API_KEY"}, Dialect: "tencent",
			Models: []string{"hy3", "hy3-preview"}},
		{ID: "hunyuan", Name: "Tencent Hunyuan", BaseURL: "https://api.hunyuan.cloud.tencent.com/v1", KeyEnv: []string{"HUNYUAN_API_KEY"}, Dialect: "tencent",
			Models: []string{"hunyuan-t1-latest", "hunyuan-turbos-latest"}},
		{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1", KeyEnv: []string{"OPENROUTER_API_KEY"}, Dialect: "openrouter",
			Models: []string{"qwen/qwen3-coder", "moonshotai/kimi-k2.6", "z-ai/glm-5.2", "deepseek/deepseek-v4-pro", "minimax/minimax-m3", "tencent/hy3"}},
		{ID: "siliconflow", Name: "SiliconFlow", BaseURL: "https://api.siliconflow.com/v1", KeyEnv: []string{"SILICONFLOW_API_KEY"},
			Models: []string{"Qwen/Qwen3-Coder-480B-A35B-Instruct", "moonshotai/Kimi-K2-Instruct-0905", "zai-org/GLM-4.6", "deepseek-ai/DeepSeek-V3.2"}},
		{ID: "fireworks", Name: "Fireworks", BaseURL: "https://api.fireworks.ai/inference/v1", KeyEnv: []string{"FIREWORKS_API_KEY"},
			Models: []string{"accounts/fireworks/models/kimi-k2p6", "accounts/fireworks/models/glm-5p2", "accounts/fireworks/models/deepseek-v4-pro"}},
		{ID: "together", Name: "Together", BaseURL: "https://api.together.xyz/v1", KeyEnv: []string{"TOGETHER_API_KEY"},
			Models: []string{"Qwen/Qwen3-Coder-480B-A35B-Instruct-FP8", "moonshotai/Kimi-K2-Instruct-0905", "deepseek-ai/DeepSeek-V3.1"}},
		{ID: "nvidia", Name: "NVIDIA NIM", BaseURL: "https://integrate.api.nvidia.com/v1", KeyEnv: []string{"NVIDIA_API_KEY"},
			Models: []string{"qwen/qwen3-coder-480b-a35b-instruct", "moonshotai/kimi-k2-instruct-0905", "deepseek-ai/deepseek-v3.1"}},
	}
}

// localProvider is the configured LCA_BASE_URL endpoint (vLLM / SGLang on the
// inference host). It keeps the text tag protocol by default — native tool
// parsers vary across checkpoints — unless LCA_TOOLS says otherwise.
func localProvider(cfg Config) *Provider {
	t := transportText
	if cfg.Tools == transportNative {
		t = transportNative
	}
	// Engine carries the operator's own answer (LCA_ENGINE / config.json engine)
	// for the whole endpoint; "auto" and "" both mean "read it from /v1/models".
	eng := strings.ToLower(strings.TrimSpace(cfg.Engine))
	if eng == engineAuto {
		eng = ""
	}
	return &Provider{ID: "local", Name: "local endpoint", BaseURL: cfg.BaseURL, Dialect: "vllm", Transport: t, Local: true, Engine: eng}
}

// Providers resolves model refs to clients. Clients for hosted models are
// created on demand and cached per ref, so every agent can run its own model
// concurrently while sharing HTTP connections.
type Providers struct {
	cfg     Config
	local   *Client
	list    []*Provider
	byID    map[string]*Provider
	mu      sync.Mutex
	clients map[string]*Client
	shared  *http.Client
	// What each endpoint answered on /v1/models: endpoint → normalised model id →
	// max_model_len. An endpoint that could not answer is stored as an empty map,
	// so "asked already" and "serves nothing" are the same fast answer.
	windows map[string]map[string]int
	// Which engine each endpoint's own cards claimed: endpoint → normalised model
	// id → "sglang" | "vllm". Learned from the SAME /v1/models response as the
	// window, and only when owned_by is exactly one of those two words — anything
	// else (a router's "local", an org name) leaves the model UNKNOWN rather than
	// voting.
	engines map[string]map[string]string
	// learned: what a deployment's own refusal told us, endpoint → normalised model
	// id → window. Kept in a file beside the state so the lesson outlives the
	// session that paid for it, and treated as server truth because it came from
	// the server. dir is where that file lives ("" = nowhere, tests and one-shots).
	learned  map[string]map[string]int
	stateDir string
}

// windowFile is the remembered-windows file. One small JSON object; a corrupt or
// unreadable one is ignored rather than fatal — a forgotten window costs a
// refusal, a failed startup costs the session.
func (ps *Providers) windowFile() string {
	if ps.stateDir == "" {
		return ""
	}
	return filepath.Join(ps.stateDir, "windows.json")
}

// UseStateDir gives Providers somewhere to remember windows, and loads what is
// already there.
func (ps *Providers) UseStateDir(dir string) {
	ps.mu.Lock()
	ps.stateDir = dir
	ps.mu.Unlock()
	path := ps.windowFile()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var got map[string]map[string]int
	if json.Unmarshal(data, &got) != nil {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.learned = got
}

// RememberWindow records what a deployment said about itself, for this process
// and for the next one. Write failures are silent on purpose: the number is
// already in force for this session, and a read-only state directory must not
// turn a recovered turn into an error.
func (ps *Providers) RememberWindow(endpoint, model string, n int) {
	if n <= 0 {
		return
	}
	ep, id := strings.TrimRight(endpoint, "/"), normalizeModelID(model)
	ps.mu.Lock()
	if ps.learned == nil {
		ps.learned = map[string]map[string]int{}
	}
	if ps.learned[ep] == nil {
		ps.learned[ep] = map[string]int{}
	}
	ps.learned[ep][id] = n
	dir := ps.stateDir
	ps.mu.Unlock()
	rememberWindowIn(dir, endpoint, model, n)
}

// rememberWindowIn is the file half, shared with doctor, which learns the same
// thing from the same refusal but holds no Providers. Read-modify-write so two
// processes lose at most each other's newest entry, never the file; every failure
// is silent because the number is already in force where it was learned.
func rememberWindowIn(stateDir, endpoint, model string, n int) {
	if stateDir == "" || n <= 0 {
		return
	}
	path := filepath.Join(stateDir, "windows.json")
	got := map[string]map[string]int{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &got)
	}
	ep, id := strings.TrimRight(endpoint, "/"), normalizeModelID(model)
	if got[ep] == nil {
		got[ep] = map[string]int{}
	}
	got[ep][id] = n
	snapshot, err := json.MarshalIndent(got, "", " ")
	if err != nil {
		return
	}
	if os.MkdirAll(stateDir, 0o700) != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, append(snapshot, '\n'), 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// rememberedWindow is what an earlier session learned about this model at this
// endpoint, or 0. Read from the file, because doctor holds no Providers.
func rememberedWindow(cfg Config, endpoint, model string) int {
	dir := cfg.stateDir()
	if dir == "" {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(dir, "windows.json"))
	if err != nil {
		return 0
	}
	var got map[string]map[string]int
	if json.Unmarshal(data, &got) != nil {
		return 0
	}
	return got[strings.TrimRight(endpoint, "/")][normalizeModelID(model)]
}

// applyRememberedWindow puts a window learned by an earlier session in force for
// a bare client — doctor's and init's, which have no Providers to ask. Without it
// doctor re-probes and re-warns about a window it already knows.
func applyRememberedWindow(cfg Config, c *Client) {
	if c == nil || c.CtxLen() > 0 {
		return
	}
	dir := cfg.stateDir()
	if dir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, "windows.json"))
	if err != nil {
		return
	}
	var got map[string]map[string]int
	if json.Unmarshal(data, &got) != nil {
		return
	}
	c.SetCtxLen(got[strings.TrimRight(c.Endpoint(), "/")][normalizeModelID(c.Model())])
}

// LearnedWindow is what a refusal from this endpoint taught us about this model.
func (ps *Providers) LearnedWindow(endpoint, model string) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.learned[strings.TrimRight(endpoint, "/")][normalizeModelID(model)]
}

func NewProviders(cfg Config, fc *FileConfig, local *Client) *Providers {
	ps := &Providers{cfg: cfg, local: local, byID: map[string]*Provider{}, clients: map[string]*Client{},
		windows: map[string]map[string]int{}, engines: map[string]map[string]string{}, shared: &http.Client{Timeout: 30 * time.Minute}}
	for _, p := range presetProviders() {
		ps.add(p)
	}
	if fc != nil {
		for id, pc := range fc.Providers {
			p := ps.byID[id]
			if p == nil {
				p = &Provider{ID: id, Name: id}
				ps.add(p)
			}
			pc.apply(p)
		}
	}
	for _, p := range ps.list {
		switch {
		case cfg.Tools != "" && cfg.Tools != "auto":
			p.Transport = cfg.Tools
		case p.Transport == "":
			p.Transport = transportNative
		}
	}
	return ps
}

func (ps *Providers) add(p *Provider) {
	if _, ok := ps.byID[p.ID]; !ok {
		ps.list = append(ps.list, p)
	}
	ps.byID[p.ID] = p
}

// Split parses "provider/model". ok=false means the ref is for the local endpoint.
func (ps *Providers) Split(ref string) (p *Provider, model string, ok bool) {
	if i := strings.IndexByte(ref, '/'); i > 0 {
		if p := ps.byID[ref[:i]]; p != nil && ref[i+1:] != "" {
			return p, ref[i+1:], true
		}
	}
	return nil, ref, false
}

// LearnWindows records what an endpoint answered on /v1/models, so every client
// built for it afterwards budgets from the running deployment rather than from a
// model card. Learned once per endpoint and reused: the window has to reach a
// role chain's client too, and that client is a copy of local with the model
// swapped — a path that never went through reconcileModel, which left "the
// server's max_model_len outranks the card" inert exactly where this binary
// spends its time. An endpoint that cannot answer is recorded as answering
// nothing, so a failover does not pay a round-trip per retry.
func (ps *Providers) LearnWindows(endpoint string, ms []ModelInfo) {
	w := map[string]int{}
	e := map[string]string{}
	for _, m := range ms {
		if m.MaxLen > 0 {
			w[normalizeModelID(m.ID)] = m.MaxLen
		}
		if eng := engineName(m.OwnedBy); eng != "" {
			e[normalizeModelID(m.ID)] = eng
		}
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.windows[strings.TrimRight(endpoint, "/")] = w
	ps.engines[strings.TrimRight(endpoint, "/")] = e
}

// engineName is the ONLY way a string becomes an engine, for every source: the
// comparison is exact (after lower-casing) and anything else is UNKNOWN.
//
// Applied to /v1/models' owned_by — the one engine signal that lives inside /v1,
// the only informational route a gateway is sure to proxy — that exactness is the
// whole point: SGLang's ModelCard defaults owned_by to "sglang" and vLLM's to
// "vllm", while "local" (what SGLang's own router synthesizes), "organization_owner"
// or a company name says nothing. A heuristic that is harmless as a label becomes
// load-bearing the moment a request field depends on it.
func engineName(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case engineSGLang:
		return engineSGLang
	case engineVLLM:
		return engineVLLM
	}
	return ""
}

// applyEngine resolves tiers 2 and 3 of the engine precedence onto a client:
// what the endpoint is configured to be (providers.<id>.engine / LCA_ENGINE /
// /set engine), then what its own card claimed in owned_by. Tier 1 —
// roles.yaml models.<id>.engine — is applied by the caller that knows the roles
// file, and UNKNOWN is left UNKNOWN.
//
// It is called exactly where the window is learned and NEVER on the request path:
// the gateway keys its KV cache on the request prefix, so the body shape of a
// running session must not change under it. One decision, before the first
// request.
func (ps *Providers) applyEngine(c *Client) {
	if c == nil || c.provider == nil || !c.provider.selfHosted() {
		return // a hosted API's engine is not ours to know, and nothing reads it
	}
	ps.mu.Lock()
	eng := ps.engines[strings.TrimRight(c.Endpoint(), "/")][normalizeModelID(c.Model())]
	ps.mu.Unlock()
	c.setEngine(eng, EngineFromOwnedBy)
	c.setEngine(c.provider.Engine, EngineFromEndpoint)
}

// Learn points a client at the window its own endpoint reports for its own
// model, asking the endpoint at most once. Silent when it cannot say: the table
// is the fallback, never the override, and a model served without max_model_len
// must not clobber a window we already have (SetCtxLen guards both).
func (ps *Providers) Learn(c *Client) {
	if c == nil || c.provider == nil || !c.provider.Local {
		return // only a local deployment publishes max_model_len at all
	}
	ep := strings.TrimRight(c.Endpoint(), "/")
	ps.mu.Lock()
	w, asked := ps.windows[ep]
	ps.mu.Unlock()
	if !asked {
		ms, err := c.ProbeModels(ep)
		if err != nil {
			ms = nil
		}
		ps.LearnWindows(ep, ms)
		ps.mu.Lock()
		w = ps.windows[ep]
		ps.mu.Unlock()
	}
	// The engine is a fact about the same (endpoint, model) learned from the same
	// response, so it is resolved here and not in a second round-trip.
	ps.applyEngine(c)
	if n := w[normalizeModelID(c.Model())]; n > 0 {
		c.SetCtxLen(n)
		return
	}
	// A deployment that does not publish max_model_len may have told us its window
	// once already, by refusing a prompt. That lesson is server truth too.
	c.SetCtxLen(ps.LearnedWindow(ep, c.Model()))
}

// Client returns the client for a model ref. "" means the local client as
// currently configured. A local ref ("local/x" or a bare name) returns a copy
// of the local client with that model, so a subagent can't disturb the REPL's.
func (ps *Providers) Client(ref string) (*Client, error) {
	if ref == "" {
		return ps.local, nil
	}
	if strings.HasPrefix(ref, "local/") {
		ref = strings.TrimPrefix(ref, "local/")
	}
	p, model, ok := ps.Split(ref)
	if !ok {
		if model == ps.local.Model() {
			return ps.local, nil
		}
		c := *ps.local
		c.model = model
		c.ctxLen, c.ctxSrc = 0, OriginUnset
		c.resetEngine()
		// A subagent, a delegate or an `lca run` step on another served model is
		// a new model on the SAME deployment, so its window is knowable — and
		// nothing else on this path would ever ask.
		ps.Learn(&c)
		return &c, nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if c := ps.clients[ref]; c != nil {
		return c, nil
	}
	key := p.apiKey()
	if key == "" {
		return nil, fmt.Errorf("no API key for provider %q (set %s)", p.ID, strings.Join(p.KeyEnv, " or "))
	}
	c := &Client{http: ps.shared, provider: p, baseURL: strings.TrimRight(p.BaseURL, "/"), endpoints: []string{p.BaseURL},
		epModel: map[string]string{}, model: model, apiKey: key, temp: tempOrNil(ps.cfg.Temperature), maxTokens: ps.cfg.MaxTokens}
	// providers.<id>.engine reaches the client HERE, inline, and not through
	// applyEngine: this path holds ps.mu, and the only tier applyEngine could add
	// is owned_by, which is learned per endpoint and never learned for one of these
	// — Learn asks only the local deployment. Without this line the key doctor
	// tells an operator to move `dialect:` into would have had no effect at all.
	c.setEngine(p.Engine, EngineFromEndpoint)
	ps.clients[ref] = c
	return c, nil
}

// Refs lists "provider/model" suggestions for providers that have a key, for
// /model completion and the /providers listing.
func (ps *Providers) Refs() []string {
	var out []string
	for _, p := range ps.list {
		if p.apiKey() == "" {
			continue
		}
		for _, m := range p.Models {
			out = append(out, p.ID+"/"+m)
		}
	}
	return out
}

// Sorted returns providers ordered: keyed first, then by id.
func (ps *Providers) Sorted() []*Provider {
	out := append([]*Provider(nil), ps.list...)
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := out[i].apiKey() != "", out[j].apiKey() != ""
		if ki != kj {
			return ki
		}
		return out[i].ID < out[j].ID
	})
	return out
}
