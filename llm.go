package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Client is a tiny OpenAI-compatible chat-completions client bound to one
// provider and one model. stdlib only. The local client is mutable (/endpoint,
// /model, /discover); clients for hosted presets are created per model ref.
type Client struct {
	http      *http.Client
	provider  *Provider
	baseURL   string
	endpoints []string          // known endpoints, for /endpoint switching
	epModel   map[string]string // endpoint URL → model, learned from discovery
	model     string
	apiKey    string
	temp      *float64 // nil = send no temperature (the model card / server decides)
	noReplay  bool     // never send reasoning_content back (server rejects it)
	maxTokens int
	ctxLen    int    // active model's context window (max_model_len), 0 if unknown
	ctxSrc    Origin // where ctxLen came from; only ever OriginServer or unset
	transport string // per-session override of the provider's transport ("" = provider's)
}

func NewClient(cfg Config) *Client {
	eps := cfg.Endpoints
	if len(eps) == 0 {
		eps = []string{cfg.BaseURL}
	}
	return &Client{
		http:      &http.Client{Timeout: 30 * time.Minute},
		provider:  localProvider(cfg),
		baseURL:   cfg.BaseURL,
		endpoints: eps,
		epModel:   map[string]string{},
		model:     cfg.Model,
		apiKey:    cfg.APIKey,
		temp:      tempOrNil(cfg.Temperature),
		maxTokens: cfg.MaxTokens,
	}
}

func (c *Client) Endpoints() []string { return c.endpoints }

// SetEndpoints replaces the known-endpoint list (deduped) with the discovered
// ones first (so their order matches the /discover picker), keeping the current
// endpoint reachable at the end. A no-op when nothing was discovered.
func (c *Client) SetEndpoints(urls []string) {
	seen := map[string]bool{}
	var list []string
	for _, u := range urls {
		u = strings.TrimRight(u, "/")
		if u != "" && !seen[u] {
			seen[u] = true
			list = append(list, u)
		}
	}
	if len(list) == 0 {
		return // nothing discovered → keep the existing list
	}
	if !seen[c.baseURL] {
		list = append(list, c.baseURL) // keep current reachable, at the end
	}
	c.endpoints = list
}

// SetEndpointModel / EndpointModel remember which model a discovered endpoint
// serves, so switching to it can select that model automatically.
func (c *Client) SetEndpointModel(url, model string) {
	c.epModel[strings.TrimRight(url, "/")] = model
}
func (c *Client) EndpointModel(url string) string { return c.epModel[strings.TrimRight(url, "/")] }

// EndpointForModel returns a discovered endpoint that serves the given model, or
// "" if none is known. Used so /model can route to where the model actually runs.
func (c *Client) EndpointForModel(model string) string {
	for url, m := range c.epModel {
		if m == model {
			return url
		}
	}
	return ""
}

// KnownModels returns the distinct model names learned from discovery (sorted),
// for /model completion.
func (c *Client) KnownModels() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range c.epModel {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// SetEndpoint switches the active endpoint and remembers it in the known list.
// It normalizes a human-typed address: trailing slash trimmed, http:// prepended
// when no scheme is given, and /v1 appended when no path is present (the common
// footgun — a bare host:port would send chat to /chat/completions and 404).
func (c *Client) SetEndpoint(u string) {
	u = strings.TrimRight(u, "/")
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	if pu, err := url.Parse(u); err == nil && pu.Path == "" {
		u += "/v1"
	}
	c.baseURL = u
	// A window learned from another machine says nothing about this one, and the
	// same model id can be served with a different --max-model-len on each.
	c.ctxLen, c.ctxSrc = 0, OriginUnset
	for _, e := range c.endpoints {
		if e == u {
			return
		}
	}
	c.endpoints = append(c.endpoints, u)
}

func (c *Client) Model() string    { return c.model }
func (c *Client) Endpoint() string { return c.baseURL }

// SetModel points the client at another served model and forgets the window it
// learned for the previous one. Without the reset, /model glm5.3 would keep the
// model-before-last's max_model_len and report it as glm5.3's — between the six
// served models that is a swing from 262,144 to 1,048,576, i.e. the agent either
// compacts four times too early or builds prompts the server refuses.
func (c *Client) SetModel(m string) {
	c.model = m
	c.ctxLen, c.ctxSrc = 0, OriginUnset
}

// SetCtxLen records the window the running deployment reports. Two guards, both
// load-bearing: a server that omits max_model_len sends 0 and must not clobber a
// window we already know (SGLang frequently omits it), and only a local endpoint
// can speak for a deployment — a hosted /v1/models has no max_model_len, so a
// hosted client's window must keep reading card/api and never "server".
func (c *Client) SetCtxLen(n int) {
	if n <= 0 || c.provider == nil || !c.provider.Local {
		return
	}
	c.ctxLen, c.ctxSrc = n, OriginServer
}

// CtxLen is the model's context window: what the deployment reported, else the
// built-in profile for the model, else 0 (unknown).
//
// The server outranking the card is deliberate and asymmetric. An operator may
// serve K3 at --max-model-len 131072 on the hardware they have, and the card's
// 1,048,576 is then a lie that makes the client build prompts the server will
// refuse; a server serving MORE than the card claims (YaRN) is equally the
// server's truth. The card is the default for when the deployment declines to
// say. Full precedence: server max_model_len > role context: > profile > 24000.
func (c *Client) CtxLen() int {
	if c.ctxLen > 0 {
		return c.ctxLen
	}
	return c.Profile().Context
}

// ServerCtxLen is the window the running deployment reported, or 0 when it has
// not said. Separate from CtxLen because one caller needs to know that the
// number is the deployment's: a budget may sit above the table's guess, but not
// above what the server will actually accept.
func (c *Client) ServerCtxLen() int { return c.ctxLen }

// CtxSrc is where CtxLen came from, for /model and doctor.
func (c *Client) CtxSrc() Origin {
	if c.ctxLen > 0 {
		return c.ctxSrc
	}
	return c.Profile().Src.Context
}

// replyCeiling is the max_tokens this client will actually send, and where that
// number came from. Four steps, in the order body() applies them: the request's
// own budget, then the configured one, then the family's published ceiling capped
// by outputTokenMax — and nothing at all on a local endpoint, where the
// deployment's own default beats a number of ours — and finally a clamp to a
// quarter of the window the deployment reports.
//
// The clamp is load-bearing: a reply budget larger than the window is refused
// outright, with an error the overflow retry does not recognise as overflow. But
// it can rewrite a number the operator configured, and in a client whose rule is
// that every number names where it came from, a silent rewrite was the one hole
// left — so the reason travels with the number and /model and doctor print it.
func (c *Client) replyCeiling(want int) (int, string) {
	n, why := want, "asked for by this request"
	switch {
	case n > 0:
	case c.maxTokens > 0:
		n, why = c.maxTokens, "configured (LCA_MAX_TOKENS / max_tokens)"
	case c.provider != nil && c.provider.Local:
		return 0, "unset — no max_tokens is sent, so the deployment's own default applies"
	default:
		out := c.Profile().Output
		if out == 0 {
			return 0, "unset — nothing is published for this model, so the server's default applies"
		}
		n, why = min(out, outputTokenMax), "the profile's ceiling, capped at "+kfmt(outputTokenMax)+" by this client"
	}
	if c.ctxLen > 0 && n > c.ctxLen/4 {
		return c.ctxLen / 4, why + ", clamped to a quarter of the " + ctxfmt(c.ctxLen) + " window the server reports"
	}
	return n, why
}

// Ref is the provider-qualified model name shown in the UI ("deepseek/deepseek-v4-pro").
// The local provider shows the bare served name, as before.
func (c *Client) Ref() string {
	if c.provider == nil || c.provider.Local {
		return c.model
	}
	return c.provider.ID + "/" + c.model
}

// Profile is the per-family tuning for the current model (models.go).
func (c *Client) Profile() ModelProfile { return lookupProfile(c.model) }

// EffortFor / EffortVocab resolve a reasoning level the way this client's own
// request will: the self-hosted vocabulary against a vLLM/SGLang endpoint, the
// hosted API's everywhere else. /model and doctor must quote the list the
// request actually checks against, or they promise a level the server drops.
func (c *Client) EffortFor(level string) string {
	if c.localDialect() {
		return c.Profile().effortForLocal(level)
	}
	return c.Profile().effortFor(level)
}

func (c *Client) EffortVocab() []string { return c.Profile().effortVocab(c.localDialect()) }

func (c *Client) localDialect() bool { return c.provider != nil && c.provider.Dialect == "vllm" }

// Native reports whether this client uses the API's native function calling
// (true) or the line-anchored text tag protocol (false).
func (c *Client) Native() bool {
	if c.transport != "" {
		return c.transport == transportNative
	}
	return c.provider != nil && c.provider.Transport == transportNative
}

// ModelInfo is what we surface about a served model. Fields beyond ID are
// best-effort: vLLM populates owned_by and max_model_len; leaner servers (e.g.
// SGLang) may omit them, in which case they read as empty/0 and are hidden.
type ModelInfo struct {
	ID      string
	OwnedBy string
	MaxLen  int // context window (max_model_len), 0 if unknown
}

type modelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		OwnedBy     string `json:"owned_by"`
		MaxModelLen int    `json:"max_model_len"`
	} `json:"data"`
}

// ListModels queries the current endpoint's /models. Short timeout so a hung or
// absent endpoint never blocks. Not every server implements it — callers treat
// an error as "discovery unavailable", not fatal.
func (c *Client) ListModels() ([]ModelInfo, error) { return c.ProbeModels(c.baseURL) }

// ProbeModels queries an arbitrary endpoint's /models — used both for discovery
// on the active endpoint and for health-probing the others in the list.
func (c *Client) ProbeModels(baseURL string) ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("endpoint returned %d", resp.StatusCode)
	}
	var out modelsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("bad /models json: %w", err)
	}
	infos := make([]ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			infos = append(infos, ModelInfo{ID: m.ID, OwnedBy: m.OwnedBy, MaxLen: m.MaxModelLen})
		}
	}
	return infos, nil
}

// ProbeHealth checks whether an endpoint is reachable, independent of the model
// router: ANY HTTP response (even 404) counts as up — only a transport error is
// down. Models are returned when /models happens to answer 200, otherwise nil.
func (c *Client) ProbeHealth(baseURL string) (up bool, models []ModelInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return false, nil
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, nil // transport error → down
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, nil // reachable, but no usable model list
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out modelsResponse
	if json.Unmarshal(raw, &out) == nil {
		for _, m := range out.Data {
			if m.ID != "" {
				models = append(models, ModelInfo{ID: m.ID, OwnedBy: m.OwnedBy, MaxLen: m.MaxModelLen})
			}
		}
	}
	return true, models
}

// tempOrNil turns the config's "unset" (negative) temperature into nil.
func tempOrNil(t float64) *float64 {
	if t < 0 {
		return nil
	}
	return &t
}
