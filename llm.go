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
	for _, e := range c.endpoints {
		if e == u {
			return
		}
	}
	c.endpoints = append(c.endpoints, u)
}

func (c *Client) Model() string     { return c.model }
func (c *Client) SetModel(m string) { c.model = m }
func (c *Client) Endpoint() string  { return c.baseURL }
func (c *Client) SetCtxLen(n int)   { c.ctxLen = n }

// CtxLen is the model's context window: what discovery reported, else the
// built-in profile for the model family, else 0 (unknown).
func (c *Client) CtxLen() int {
	if c.ctxLen > 0 {
		return c.ctxLen
	}
	return c.Profile().Context
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
