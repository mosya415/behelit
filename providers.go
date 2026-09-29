package main

import (
	"fmt"
	"net/http"
	"os"
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
	Local     bool     // the LCA_BASE_URL endpoint (vLLM/SGLang): vLLM-only extras allowed
	Models    []string // suggested model ids (completion / listing)
	Headers   map[string]string
	Extra     map[string]any // extra request-body fields
}

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
	return &Provider{ID: "local", Name: "local endpoint", BaseURL: cfg.BaseURL, Dialect: "vllm", Transport: t, Local: true}
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
}

func NewProviders(cfg Config, fc *FileConfig, local *Client) *Providers {
	ps := &Providers{cfg: cfg, local: local, byID: map[string]*Provider{}, clients: map[string]*Client{},
		shared: &http.Client{Timeout: 30 * time.Minute}}
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
		c.ctxLen = 0
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
