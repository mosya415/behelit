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
	noKwargs  bool     // never send chat_template_kwargs (this deployment 400s on them)
	maxTokens int
	ctxLen    int    // active model's context window (max_model_len), 0 if unknown
	ctxSrc    Origin // where ctxLen came from; only ever OriginServer or unset
	transport string // per-session override of the provider's transport ("" = provider's)
	// Which inference engine serves THIS (endpoint, model) — "sglang", "vllm", or
	// "" for "nobody has said". Per (endpoint, model) and not per provider because
	// one gateway URL fronts both: hy3 on SGLang and the next model on vLLM is the
	// deployment this client was written for, and `c := *ps.local` copies the
	// Client while sharing the Provider, so a per-provider field would rewrite
	// every model on the endpoint.
	engine    string
	engineSrc EngineSrc
}

// EngineSrc is WHO decided which engine serves this (endpoint, model), so doctor
// names a winner instead of asserting one. Ascending by authority: a weaker
// source may never overwrite a stronger one, which is what keeps Providers.Learn
// from clobbering what the operator typed.
type EngineSrc uint8

const (
	EngineUnset        EngineSrc = iota
	EngineFromSlurm              // /discover's substring match over job logs — a doctor HINT, never set
	EngineFromOwnedBy            // the card's own owned_by, compared exactly
	EngineFromEndpoint           // LCA_ENGINE / providers.<id>.engine / /set engine
	EngineFromModel              // roles.yaml models.<id>.engine
)

// The two engines lca can name, and the word that means "read it from the
// endpoint". Nothing else is ever stored in Client.engine: an unrecognised value
// is UNKNOWN, which means "send nothing engine-specific".
const (
	engineVLLM   = "vllm"
	engineSGLang = "sglang"
	engineAuto   = "auto"
)

// String is how doctor and /model name the source, in the same style the window's
// provenance is named.
func (s EngineSrc) String() string {
	switch s {
	case EngineFromSlurm:
		return "/discover's log scan"
	case EngineFromOwnedBy:
		return "the endpoint's own owned_by"
	case EngineFromEndpoint:
		return "configured for this endpoint"
	case EngineFromModel:
		return "roles.yaml models.<id>.engine"
	}
	return "nobody has said"
}

// Engine is the engine serving this (endpoint, model), or "" for unknown. UNKNOWN
// is a real answer and means "send nothing engine-specific" — never a guess.
func (c *Client) Engine() string { return c.engine }

// EngineSrc is who decided that, for doctor and /model.
func (c *Client) EngineSrc() EngineSrc { return c.engineSrc }

// setEngine records an engine only when its source outranks the one in force, so
// the order the call sites happen to run in cannot decide the answer. An engine
// lca does not know how to speak to is dropped rather than stored: a name we
// cannot act on is UNKNOWN wearing a label.
func (c *Client) setEngine(e string, src EngineSrc) {
	if e = engineName(e); e == "" {
		return // "", "auto" and anything unrecognised all mean UNKNOWN
	}
	if src < c.engineSrc {
		return // a weaker source never overwrites a stronger one
	}
	c.engine, c.engineSrc = e, src
}

// engineLine is how the engine is printed wherever the window's provenance is
// printed: the winner and who named it, or a sentence saying what lca therefore
// does. "unknown" is a real answer here and the sentence beside it is the point —
// a blank would read as a failure, and it is a working configuration.
func engineLine(c *Client) string {
	if c == nil || c.Engine() == "" {
		return "unknown" + gSep + "lca sends nothing engine-specific"
	}
	return c.Engine() + faint(" (%s)", c.EngineSrc())
}

// resetEngine forgets what was learned, for the two moments the fact itself
// changes: the model or the endpoint moved, or the operator asked for `auto`.
func (c *Client) resetEngine() { c.engine, c.engineSrc = "", EngineUnset }

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
	// same model id can be served with a different --max-model-len on each. Nor
	// does an engine: the same model id behind two gateways may be served by two
	// different engines, so both facts are forgotten and learned again here.
	c.ctxLen, c.ctxSrc = 0, OriginUnset
	c.resetEngine()
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
// The engine goes with it, for the same reason: one gateway URL can front hy3 on
// SGLang and the next model on vLLM, so a learned engine says nothing about the
// model that replaces it.
func (c *Client) SetModel(m string) {
	c.model = m
	c.ctxLen, c.ctxSrc = 0, OriginUnset
	c.resetEngine()
}

// SetCtxLen records the window the running deployment reports. Two guards, both
// load-bearing: a server that omits max_model_len sends 0 and must not clobber a
// window we already know (an aggregating gateway in front of either engine does
// omit it — the engines themselves do not), and only a local endpoint
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
	if c.selfHosted() {
		return c.Profile().effortForLocal(level)
	}
	return c.Profile().effortFor(level)
}

func (c *Client) EffortVocab() []string { return c.Profile().effortVocab(c.selfHosted()) }

// selfHosted is "this endpoint is the operator's own vLLM or SGLang", which is
// the question every caller here was really asking. It used to be spelled
// `Dialect == "vllm"`: a VENDOR HTTP dialect standing in for "self-hosted", which
// made `dialect: "sglang"` in a config file mean the opposite of what it says.
func (c *Client) selfHosted() bool { return c.provider != nil && c.provider.selfHosted() }

// Native reports whether this client uses the API's native function calling
// (true) or the line-anchored text tag protocol (false).
func (c *Client) Native() bool {
	if c.transport != "" {
		return c.transport == transportNative
	}
	return c.provider != nil && c.provider.Transport == transportNative
}

// ModelInfo is what we surface about a served model. Fields beyond ID are
// best-effort, but NOT because an engine is lean: SGLang's ModelCard has
// defaulted owned_by to "sglang" since v0.3.6 and has carried max_model_len
// (model_config.context_len) since v0.4.5, and vLLM's says "vllm". What omits
// them is the AGGREGATING GATEWAY in front of them — SGLang's own Rust router
// synthesizes {"id","object","owned_by":"local"} with no max_model_len — so an
// empty field is a fact about the proxy and never a tiebreak about the engine.
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
		// Typed, not fmt.Errorf: /setup's credentials step, errorHint and doctor all
		// decide on the STATUS, and a status they have to scrape out of a sentence is
		// a status they get wrong — a 401 read as "the gateway may still be starting"
		// left the wizard with no route to ask for a key at all.
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw), Endpoint: baseURL}
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

// getJSON is a best-effort GET of one absolute URL on this client's HTTP client,
// for doctor's reads of routes OUTSIDE /v1 (SGLang's /model_info and
// /server_info). The error is typed so the caller can tell a 404 — "the gateway
// does not proxy this path" — from a timeout, and nothing here is ever allowed to
// influence a request body: the two callers are diagnostics.
func (c *Client) getJSON(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw), Endpoint: url}
	}
	return raw, nil
}

// engineRoot is BaseURL with a trailing /v1 trimmed, case-insensitively — the
// prefix an engine's own informational routes would hang off. It is "" when
// BaseURL does not end in /v1: lca cannot know a gateway's mount prefix, and
// guessing one would send a request to a path nobody published.
func engineRoot(baseURL string) string {
	u := strings.TrimRight(baseURL, "/")
	if len(u) < 3 || !strings.EqualFold(u[len(u)-3:], "/v1") {
		return ""
	}
	return u[:len(u)-3]
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
