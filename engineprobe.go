package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// What lca can find out about the ENGINE behind an endpoint, and the one place
// that decides it. Two rules run through this whole file:
//
//   - Engine detection happens ONCE, before the first request, and never changes
//     a running session's body shape: the gateway keys its KV cache on the
//     request prefix, so a body that differs between turn N and turn N+1 loses
//     every hit after the first. Nothing here is called from Client.body.
//   - Absence is a fact about the GATEWAY, never about the engine. A 404, a 501,
//     a non-JSON body or a timeout on one of the engine's own routes means "this
//     gateway does not proxy it" and licenses no inference at all — SGLang's own
//     Rust router has shipped verbatim-proxy, first-worker-proxy and 501
//     behaviours, and the gateway in front of the operator's deployment is not in
//     this repository.

// doctorEngine resolves the engine for ONE served model exactly as the runtime
// resolves it, with no Providers to ask: roles.yaml models.<id>.engine, then the
// endpoint's own setting, then the card's own owned_by compared exactly, then
// UNKNOWN. It runs the same Client.setEngine ranking the runtime does, so doctor
// cannot name a winner the request would not pick.
func doctorEngine(cfg Config, roles *RolesConfig, model string, info ModelInfo) (string, EngineSrc) {
	c := &Client{provider: &Provider{ID: "doctor", Local: true}, model: model}
	c.setEngine(engineName(info.OwnedBy), EngineFromOwnedBy)
	c.setEngine(cfg.Engine, EngineFromEndpoint)
	if roles != nil {
		c.setEngine(roles.engineOf(model), EngineFromModel)
	}
	return c.Engine(), c.EngineSrc()
}

// endpointEngine is the engine for the ENDPOINT as a whole: the operator's own
// setting, else what the served cards agree on, else UNKNOWN. Cards that
// disagree leave it unknown rather than letting a majority decide — one url
// fronting two engines is the deployment this was written for, and the per-model
// rows below name each one.
func endpointEngine(cfg Config, served map[string]ModelInfo) (string, EngineSrc) {
	agreed, mixed := "", false
	for _, id := range sortedKeys(served) {
		e := engineName(served[id].OwnedBy)
		switch {
		case e == "":
		case agreed == "":
			agreed = e
		case agreed != e:
			mixed = true
		}
	}
	c := &Client{provider: &Provider{ID: "doctor", Local: true}}
	if !mixed {
		c.setEngine(agreed, EngineFromOwnedBy)
	}
	c.setEngine(cfg.Engine, EngineFromEndpoint)
	return c.Engine(), c.EngineSrc()
}

// engineProvenance names WHERE an endpoint-level setting came from — the variable
// or the file — in the style doctor already uses for a window. The generic phrase
// is the fallback and not the answer: "configured for this endpoint" without the
// file is a sentence an operator cannot act on.
func engineProvenance(cfg Config, fc *FileConfig, src EngineSrc) string {
	if src != EngineFromEndpoint {
		return src.String()
	}
	switch {
	case os.Getenv("LCA_ENGINE") != "":
		return "LCA_ENGINE"
	case fc != nil && fc.From["engine"] != "":
		return "engine: in " + prettyPath(fc.From["engine"], cfg.Root)
	}
	return src.String()
}

// ── the engine's own informational routes ───────────────────────────────────

// nativeInfo is what /model_info and /server_info said, or why they said
// nothing. Every field is optional on purpose: the published key list in
// SGLang's own native_api.mdx omits five of the keys the server actually
// returns (served_model_name, load_format, reasoning_parser, tool_call_parser,
// disaggregation_mode) at both v0.5.20 and main, so parsing against the document
// rather than the source would drop exactly the keys doctor needs.
type nativeInfo struct {
	skipped string // the endpoint's path shape makes the probe impossible to address
	infoWhy string // why /model_info said nothing
	srvWhy  string // why /server_info said nothing

	gotInfo, gotSrv bool
	// /model_info. The parser keys arrived in v0.5.18; an older server answers the
	// route without them, which is NOT "no parser is loaded" — hence both a
	// presence flag and a pointer: key absent, key null and key set are three
	// different diagnoses and two of them used to read the same.
	hasReasonKey, hasToolKey        bool
	reasoningParser, toolCallParser *string
	servedModelName, modelType      string
	// /server_info. version is the ONLY place any SGLang version is reported: no
	// /v1 route carries one, which is why every version-gated field stays optional.
	version, launchCommand string
	maxReqInputLen         int
	allowAutoTruncate      *bool
	samplingDefaults       string
	templateKwargs         map[string]any
	// Deliberately NOT read, though /server_info reports both: context_length is
	// the launch ARGUMENT (null unless --context-length was passed) and
	// max_total_num_tokens is profiled KV capacity, and max_req_len is
	// min(context_len-1, kv_capacity-1) — so reading either as the window is wrong
	// in both directions. The window stays /v1/models' max_model_len.
}

// answered reports whether anything at all came back, so doctor can say "not
// proxied" once instead of once per key.
func (n nativeInfo) answered() bool { return n.gotInfo || n.gotSrv }

type modelInfoResp struct {
	ReasoningParser *string  `json:"reasoning_parser"`
	ToolCallParser  *string  `json:"tool_call_parser"`
	ServedModelName *string  `json:"served_model_name"`
	ModelType       *string  `json:"model_type"`
	Architectures   []string `json:"architectures"`
}

type serverInfoResp struct {
	Version                  *string        `json:"version"`
	LaunchCommand            any            `json:"launch_command"`
	MaxReqInputLen           *int           `json:"max_req_input_len"`
	AllowAutoTruncate        *bool          `json:"allow_auto_truncate"`
	SamplingDefaults         any            `json:"sampling_defaults"`
	DefaultChatTemplateKwarg map[string]any `json:"default_chat_template_kwargs"`
}

// readNativeInfo asks the engine's own two routes, new name first and the
// deprecated /get_* alias second: /model_info and /server_info do not exist at
// v0.5.5, and /get_* logs a deprecation warning on main but is what an old
// server answers. Everything it learns is a label; nothing it learns may change
// a request or override /v1/models.
func readNativeInfo(ctx context.Context, c *Client, cfg Config) nativeInfo {
	var n nativeInfo
	if !cfg.EngineProbe {
		n.skipped = "engine_probe is off, so lca asked nothing outside /v1"
		return n
	}
	root := engineRoot(c.Endpoint())
	if root == "" {
		// lca cannot know a gateway's mount prefix: a url that is not …/v1 gives no
		// way to address the engine's own routes, and inventing one would be a
		// request at a path nobody published.
		n.skipped = "the endpoint url does not end in /v1, so lca cannot address the engine's own routes"
		return n
	}
	if raw, why := fetchFirst(ctx, c, root, "/model_info", "/get_model_info"); raw != nil {
		var got modelInfoResp
		if json.Unmarshal(raw, &got) == nil {
			n.gotInfo = true
			n.reasoningParser, n.toolCallParser = got.ReasoningParser, got.ToolCallParser
			n.servedModelName, n.modelType = derefStr(got.ServedModelName), derefStr(got.ModelType)
			var keys map[string]json.RawMessage
			if json.Unmarshal(raw, &keys) == nil {
				_, n.hasReasonKey = keys["reasoning_parser"]
				_, n.hasToolKey = keys["tool_call_parser"]
			}
		} else {
			n.infoWhy = "it answered something that is not JSON"
		}
	} else {
		n.infoWhy = why
	}
	if raw, why := fetchFirst(ctx, c, root, "/server_info", "/get_server_info"); raw != nil {
		var got serverInfoResp
		if json.Unmarshal(raw, &got) == nil {
			n.gotSrv = true
			n.version = derefStr(got.Version)
			n.launchCommand = flatten(got.LaunchCommand)
			n.samplingDefaults = flatten(got.SamplingDefaults)
			n.allowAutoTruncate, n.templateKwargs = got.AllowAutoTruncate, got.DefaultChatTemplateKwarg
			if got.MaxReqInputLen != nil {
				n.maxReqInputLen = *got.MaxReqInputLen
			}
		} else {
			n.srvWhy = "it answered something that is not JSON"
		}
	} else {
		n.srvWhy = why
	}
	return n
}

// fetchFirst tries the current path and then the deprecated alias, and turns
// every failure into the same sentence: the gateway does not proxy it. A 404, a
// 501, a timeout and a refused connection are indistinguishable from here, and
// all three of SGLang's own router behaviours are in that set.
func fetchFirst(ctx context.Context, c *Client, root string, paths ...string) ([]byte, string) {
	why := ""
	for _, p := range paths {
		raw, err := c.getJSON(ctx, root+p)
		if err == nil {
			return raw, ""
		}
		if why != "" {
			continue // the first path tried is the one to name: the alias is a fallback
		}
		var ae *APIError
		switch {
		case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
			why = "the gateway does not proxy " + p
		case errors.As(err, &ae):
			why = fmt.Sprintf("the gateway answered %d on %s", ae.Status, p)
		default:
			why = "the gateway did not answer " + p + " (" + shortErr(err) + ")"
		}
	}
	return nil, why
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// flatten renders a value whose JSON type is not pinned by any source — SGLang's
// launch_command has been seen as a string and as an argv list — without
// asserting which one it is.
func flatten(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var parts []string
		for _, e := range t {
			parts = append(parts, fmt.Sprint(e))
		}
		return strings.Join(parts, " ")
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// ── what doctor prints ──────────────────────────────────────────────────────

// reportEngine is the engine's half of doctor's gateway section: which engine
// serves this endpoint, who said so, and — when the gateway lets us ask — which
// parsers are actually in force rather than which ones we would advise.
//
// Unknown is a working configuration and not a failure: it gets a warn, both
// ways to set it, and the sentence saying what lca therefore sends. That is the
// same treatment reportWindow gives a window nobody published.
func reportEngine(cfg Config, fc *FileConfig, roles *RolesConfig, served map[string]ModelInfo, n nativeInfo) {
	engine, src := endpointEngine(cfg, served)
	switch {
	case engine != "":
		row("engine", engine+faint(" (%s)", engineProvenance(cfg, fc, src)))
	default:
		warnLine("the engine serving this endpoint is unknown — lca therefore sends nothing engine-specific")
		hint("name it for the whole endpoint: /set engine sglang (also LCA_ENGINE, or \"engine\" in config.json)")
		hint("name it for ONE model, when this url fronts both engines: models.%s.engine: sglang in roles.yaml",
			firstNonEmpty(cfg.Model, firstKey(served)))
		hint("a card's owned_by is read only when it is exactly \"sglang\" or \"vllm\" — an aggregating gateway's synthesized \"local\" says nothing")
	}
	// A file that said dialect: "sglang" | "vllm" meant the engine, and lca now
	// reads it that way — but the key it should be under is named, because the two
	// questions are different and only one of them has an answer called "sglang".
	if fc != nil {
		for _, id := range sortedKeys(fc.Providers) {
			if eng := engineName(fc.Providers[id].Dialect); eng != "" {
				warnLine("providers.%s.dialect is %q, which is an ENGINE and not a vendor HTTP dialect — lca reads it as engine: %s", id, fc.Providers[id].Dialect, eng)
				hint("move it: \"providers\": {\"%s\": {\"engine\": \"%s\"}} — dialect: is where the vendor's HTTP dialect goes", id, eng)
			}
		}
	}
	// Per-model, and only where it differs from the endpoint's: the whole reason
	// this is per (endpoint, model) is the one model on the other engine.
	for _, id := range sortedKeys(served) {
		if e, esrc := doctorEngine(cfg, roles, id, served[id]); e != engine {
			row("engine", id+" "+gFlow+" "+firstNonEmpty(e, "unknown")+faint(" (%s)", esrc))
		}
	}
	if engine == engineSGLang {
		// Two facts about this engine that change what a green line means, and
		// neither of which changes any arithmetic: the reserve depends on launch
		// flags only /server_info reports, and the id check does not exist.
		hint("%s", faint("SGLang refuses at input_token_num >= context_len (not >) and counts num_reserved_tokens first — 4 under the hy3 MTP recipe — so the last few tokens of the stated window are not yours"))
		hint("%s", faint("SGLang's /v1/chat/completions never compares request.model: a wrong id still returns 200 from the one loaded model, so only the /v1/models comparison proves the name"))
	}
	// The parser rows below are about the ONE model the engine says it loaded, so
	// they are spelled in THAT model's engine and not the endpoint's: the per-model
	// rows just printed exist precisely because the two can differ, and a model
	// pinned to the other engine must not be handed flags its argparse refuses.
	// The endpoint's answer is the fallback, for a loaded id the cards do not list.
	loaded := engine
	if id := firstNonEmpty(n.servedModelName, cfg.Model); id != "" {
		if e, _ := doctorEngine(cfg, roles, id, served[id]); e != "" {
			loaded = e
		}
	}
	reportNativeInfo(cfg, loaded, n)
}

// reportNativeInfo prints what the engine's own routes said. Nothing here may
// override /v1/models' max_model_len (§the window's ranking), and nothing it
// fails to say licenses an inference: the line names the GATEWAY as the reason.
//
// engine is the one resolved for the model these routes say is LOADED, not the
// endpoint's: it only ever travels on to reportParsers, where it decides which
// engine's flag spellings an operator is handed.
func reportNativeInfo(cfg Config, engine string, n nativeInfo) {
	switch {
	case n.skipped != "":
		row("engine info", faint("%s", n.skipped))
		return
	case !n.answered():
		row("engine info", faint("%s"+gSep+"nothing is inferred from that — a gateway that does not proxy a path says nothing about the engine behind it",
			firstNonEmpty(n.infoWhy, n.srvWhy)))
		return
	}
	if n.version != "" {
		row("engine info", faint("version %s"+gSep+"the only place any version is reported — no /v1 route carries one", n.version))
	} else if n.gotSrv {
		row("engine info", faint("it answered, but reports no version — every version-gated field stays optional"))
	}
	if n.launchCommand != "" {
		// Not truncated: this is the single most actionable artefact doctor can
		// print — the flags in force, in the operator's own words — and the row
		// wraps it like every other long value.
		row("launched as", faint("%s", n.launchCommand))
	}
	if n.maxReqInputLen > 0 {
		// Labelled "usable input" and NOT used as a window: it is max_req_len - 5,
		// strictly below the window, and a budget that changes depending on whether
		// a gateway proxies a path is worse than a stable one.
		row("usable input", faint("%s"+gSep+"the engine's own max_req_input_len; lca still budgets from max_model_len", kfmt(n.maxReqInputLen)))
	}
	if n.allowAutoTruncate != nil && *n.allowAutoTruncate {
		warnLine("this deployment was launched with --allow-auto-truncate: a prompt that does not fit is silently shortened instead of refused")
		hint("so a request that is NOT refused does not prove it fit — set context: on the role from the window above")
	}
	if n.samplingDefaults != "" && n.samplingDefaults != "model" {
		row("sampling defaults", faint("%s"+gSep+"not \"model\", so omitting temperature/top_p falls back to this engine's own values and not the checkpoint's", n.samplingDefaults))
	}
	if len(n.templateKwargs) > 0 {
		// The server injects template kwargs of its own, which meet the ones lca
		// sends: an operator debugging "why is it thinking" has to see both halves.
		raw, err := json.Marshal(n.templateKwargs)
		if err == nil {
			row("template kwargs", faint("the server adds %s to every request's chat_template_kwargs", raw))
		}
	}
	reportParsers(cfg, engine, n)
}

// reportParsers compares the parser in force against the one this model's vendor
// documents, in the spelling of the engine actually resolved for THIS model — the
// engine argument, not the route the answer arrived on. Quoting one engine's
// token at the other is advice that fails its argparse and stops the server from
// starting, so with no engine named nothing is compared and the row says so.
func reportParsers(cfg Config, engine string, n nativeInfo) {
	if !n.gotInfo {
		return
	}
	id := firstNonEmpty(n.servedModelName, cfg.Model)
	prof := lookupProfile(id)
	if n.servedModelName != "" {
		// Which model is REALLY loaded, which matters because SGLang never compares
		// request.model: this is the only place a wrong id can be caught.
		row("loaded model", faint("%s%s", n.servedModelName, orEmpty(n.modelType, gSep+"model_type "+n.modelType)))
	}
	if !n.hasReasonKey && !n.hasToolKey {
		row("parsers", faint("this server does not report which parsers are loaded (SGLang reports them from v0.5.18)"))
		return
	}
	// One row for what is in force, and a warn per parser that is wrong or
	// missing: the row is the fact, the warn is the thing to do about it.
	var agree []string
	for _, p := range []struct {
		flag  string
		names ParserNames
		has   bool
		got   *string
	}{
		{"--reasoning-parser", prof.ReasonParse, n.hasReasonKey, n.reasoningParser},
		{"--tool-call-parser", prof.ToolParser, n.hasToolKey, n.toolCallParser},
	} {
		name := firstNonEmpty(prof.Key, id)
		want, who := p.names.forEngine(engine)
		switch {
		case !p.has:
		case p.got == nil || *p.got == "":
			// Not "the engine lacks it": the key is there and says nothing is loaded.
			warnLine("no parser is loaded for %s — that is why thinking arrives inside content and tool calls arrive as text", p.flag)
			hint("launch it with %s auto%s", p.flag, parserParen(want))
		case *p.got == "auto":
			// `auto` is NOT agreement with anything: it resolves from the chat
			// template at launch, first-match-wins over an ordered rule list, and no
			// source says what the server logs when it resolves — so what it picked is
			// a fact lca does not have, and claiming otherwise would hide the one
			// failure mode that looks exactly like a correct configuration.
			row("parsers", faint("%s auto"+gSep+"in force, and what that resolved to is not reported anywhere lca can read%s",
				p.flag, parserWanted(want, name, who)))
		case want == "":
			agree = append(agree, p.flag+" "+*p.got+faint(" (nothing is recorded for %s on %s, so nothing is compared)", name, who))
		case *p.got == want:
			agree = append(agree, p.flag+" "+*p.got)
		default:
			warnLine("%s is %s, but %s's own on %s is %s", p.flag, *p.got, name, who, want)
			hint("change it to %s auto (or %s %s) and restart the server", p.flag, p.flag, want)
		}
	}
	if len(agree) > 0 {
		row("parsers", faint("%s"+gSep+"in force, and they agree with what %s's vendor documents",
			strings.Join(agree, gSep), firstNonEmpty(prof.Key, id)))
	}
}

// parserParen is the explicit value in brackets, or nothing when none is sourced
// for this engine. The other engine's token is never offered: it is a value this
// engine's argparse refuses, so it would stop the server from starting.
func parserParen(want string) string {
	if want == "" {
		return ""
	}
	return faint(" (or %s)", want)
}

// parserWanted is what to pin the flag to if the operator wants the resolution
// off the chat template's hands, said beside an `auto` that cannot be read back.
// Empty when nothing is sourced for this engine, which is the honest half of the
// same sentence.
func parserWanted(want, name, who string) string {
	if want == "" {
		return faint(gSep+"and nothing is recorded for %s on %s to pin it to", name, who)
	}
	return faint(gSep+"%s's own on %s is %s, if it should be pinned", name, who, want)
}

// firstKey is any served id, for a hint that has to name one.
func firstKey(served map[string]ModelInfo) string {
	for _, id := range sortedKeys(served) {
		return id
	}
	return "<model>"
}

// orEmpty is "this suffix, but only when the value behind it exists" — a label
// for an absent key reads as a key whose value is empty, which is a different
// diagnosis in this file.
func orEmpty(v, suffix string) string {
	if v == "" {
		return ""
	}
	return suffix
}
