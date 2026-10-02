package main

import (
	"sort"
	"strconv"
	"strings"
)

// Per-model profiles for the open models this agent orchestrates: Qwen, Kimi,
// GLM, DeepSeek, Hunyuan (hy3) and MiniMax. Everything is built in so the binary
// needs no model catalog download.
//
// THE RULE THIS FILE LIVES BY: the client never invents a sampling number. When
// a profile has no value for a field, chat.go's body() leaves the field out of
// the request entirely and the server's own default applies. A reasoning model
// run at temperature 0 repeats itself and a wrong context window silently
// truncates or overflows, so a number copied from a sibling version is worse
// than no number — it looks sourced. Every value below either names where it
// came from (Src) or is absent.
//
//   - Context/Output: window and max output, used for the context budget and
//     max_tokens (capped at outputTokenMax). The running deployment's
//     max_model_len outranks Context — see Client.CtxLen.
//   - Temperature/TopP/TopK: the vendor's own numbers; nil / 0 = send nothing.
//   - Replay: how reasoning_content is sent back (see wire in chat.go).
//   - Reasoning: the model thinks (or can), so a thinking toggle applies.
//   - Efforts: the reasoning_effort levels the vendor documents. Empty on a
//     known family means the model takes no effort field at all.
//   - Switch: the chat-template switch the model's OWN template reads. This is a
//     property of the checkpoint, not of the engine, so it lives here and not in
//     the provider's dialect.
//   - ToolParser/ReasonParse: the --tool-call-parser / --reasoning-parser VALUE
//     each ENGINE documents for this model, so doctor can name the flag when a
//     probe shows tool calling or reasoning is not working. The flag names are the
//     same on vLLM and SGLang; the values are not interchangeable, and a value one
//     engine's argparse rejects fails server STARTUP — so they are recorded per
//     engine and never borrowed across.
type ModelProfile struct {
	Family      string
	Context     int
	Output      int
	Temperature *float64
	TopP        *float64
	TopK        int
	Replay      string // "", "turn", "all"
	Reasoning   bool
	Efforts     []string // accepted reasoning_effort values; empty = nothing recorded
	// The narrower list the ENGINE's own template accepts, when it is not the
	// hosted API's. A hosted API resolves compat spellings for you; a Jinja
	// template does not, and an out-of-vocabulary level reaching one is the hy3
	// failure in a different house. Empty = the same list both ways.
	EffortsLocal []string
	EffortNone   bool   // the vendor documents that this model takes NO effort field
	EffortOn     string // the level to send for a bare "think" (the vendor default where there is one)
	EffortOff    string // the level that means "don't think" ("" = use Switch.Off)
	Switch       ThinkSwitch
	ToolParser   ParserNames // --tool-call-parser, per engine
	ReasonParse  ParserNames // --reasoning-parser, per engine
	// The card's sampling describes a SELF-HOST and the vendor's own hosted API
	// fixes these values server-side while documenting that they be omitted, so
	// chat.go sends them to a local endpoint only (top_k is gated the same way).
	SampleLocalOnly bool
	Note            string // the one caveat an operator has to see
	Src             Src
	Key             string // the table entry that matched, filled by lookupProfile
}

// ThinkSwitch is how one model's own chat template turns thinking on and off.
// Against the gateway (Dialect "vllm") sending enable_thinking to everything is
// right for Qwen and wrong or inert for the other five served models, which is
// why the kwarg name is per-model data rather than per-dialect code.
type ThinkSwitch struct {
	Kwarg    string // chat_template_kwargs key that switches thinking ("" = no switch)
	On, Off  any    // the values that key accepts
	Effort   string // request key carrying the effort level ("" = the model takes none)
	TopLevel bool   // Effort is a top-level field, not a chat_template_kwargs entry
	Keep     string // kwarg that preserves reasoning history ("" = none)
	KeepVal  any    // the value of that kwarg which means "keep"
}

// ParserNames is the --tool-call-parser / --reasoning-parser VALUE each engine
// documents for this model. Empty = nothing is sourced for that engine, so doctor
// names `auto` and says so rather than borrowing the other engine's token: the
// values are registry members on each side and a name that is not in the registry
// is a hard failure at launch (vLLM and SGLang both pass them through argparse
// `choices`), not a silently ignored flag.
//
// The SGLang values are spellings from SGLang's own registry modules
// (function_call/parser_names.py and parser/reasoning_parser_names.py). Those
// modules are quoted at `main`, which is the only revision there is a quote of —
// so a name here is known to be spelled SGLang's way and is NOT known to be in
// the operator's released tag. Which tag they run is unknown (no /v1 route
// reports a version), which is why the advice leads with `auto` and names an
// explicit value only in brackets.
type ParserNames struct{ VLLM, SGLang string }

// forEngine is the value this flag takes on ONE engine, with that engine named so
// a line an operator reads says which server it is for. With no engine named
// there is no answer at all: the two spellings are not interchangeable — each is
// a value the other engine's argparse refuses — so nothing is compared and
// nothing is advised, rather than one of them being picked by the reader.
func (p ParserNames) forEngine(engine string) (value, who string) {
	switch engine {
	case engineSGLang:
		return p.SGLang, "SGLang"
	case engineVLLM:
		return p.VLLM, "vLLM"
	}
	return "", "an engine nobody has named"
}

// Origin is where a profile number came from, so /model and doctor can say
// "card" or "server" instead of printing an anonymous integer. A number nobody
// published is not stored at all, so OriginUnset and a zero value coincide —
// and a number that IS stored with OriginUnset is one nobody has sourced yet,
// which the display calls "unsourced" rather than dressing up.
type Origin uint8

const (
	OriginUnset  Origin = iota // nothing published — the field is left out of the request
	OriginCard                 // the vendor's model card / config.json / chat template
	OriginAPI                  // the vendor's hosted-API reference or pricing table
	OriginEngine               // vLLM recipe or SGLang cookbook only, not the card
	OriginServer               // /v1/models max_model_len, i.e. the running deployment
)

func (o Origin) String() string {
	switch o {
	case OriginCard:
		return "card"
	case OriginAPI:
		return "api"
	case OriginEngine:
		return "engine docs"
	case OriginServer:
		return "server"
	}
	return "unsourced"
}

// Src names the origin of each number in the profile it sits on. A struct and
// not a map: the table stays a gofmt-clean literal, a renamed field is a compile
// error rather than a silently dead string key, and there is nothing to allocate.
type Src struct {
	Context, Output, Temperature, TopP, TopK Origin
}

// outputTokenMax caps max_tokens even for models allowing more: big enough for
// any single tool call, small enough to keep the reply budget from crowding out
// the prompt. The served ceilings below are an order of magnitude above it — they
// are recorded because they are published, not because we send them.
const outputTokenMax = 32_000

func f64(v float64) *float64 { return &v }

// ── the six the gateway serves ──────────────────────────────────────────────
// Keyed by id, not matched by substring: the gateway's own spelling must never
// depend on a heuristic. ids are written the way a vendor writes them and are
// normalised into the lookup map at init, so "glm5.3" and "zai-org/GLM-5.3-BF16"
// land on the same entry without either being listed twice.

type canonProfile struct {
	ids  []string // every spelling that IS this model
	prof ModelProfile
}

var canonicalProfiles = []canonProfile{
	// ── Kimi K3 (Moonshot) ─────────────────────────────────────────────────
	// Thinking cannot be turned off, so no switch is ever sent; the effort level
	// is a TOP-LEVEL field, not a chat-template kwarg.
	{[]string{"kimi-k3", "moonshotai/kimi-k3"}, ModelProfile{
		Family:      "kimi",
		Context:     1_048_576, // card: HF spec table, generation_config max_length, pricing page, vLLM recipe --max-model-len
		Output:      1_048_576, // api: max_completion_tokens "defaults to 131072 and can be set up to 1048576"
		Temperature: f64(1.0),  // card: eval footnote "reasoning effort max and temperature = 1.0"
		TopP:        f64(1.0),  // card: "for agentic tasks, we set top-p = 1.0" — this client is agentic
		Replay:      "all",     // card: the complete assistant message, reasoning_content included, goes back as-is
		Reasoning:   true,
		Efforts:     []string{"low", "high", "max"},
		EffortOn:    "max", // card: the vendor's own default
		Switch:      ThinkSwitch{Effort: "reasoning_effort", TopLevel: true},
		ToolParser:  ParserNames{VLLM: "kimi_k3", SGLang: "kimi_k3"},
		ReasonParse: ParserNames{VLLM: "kimi_k3", SGLang: "kimi_k3"},
		// api: platform.kimi.ai's K3 quickstart fixes temperature 1.0 / top_p 0.95
		// server-side and says to omit both — and the agentic top_p above is 1.0,
		// i.e. not even the value it fixes. So the card's numbers go to a
		// self-host and nothing goes to Moonshot; the Note below is enforced now.
		SampleLocalOnly: true,
		Note:            "thinking cannot be disabled; Moonshot's hosted API fixes temperature/top_p server-side and says to omit them, so these self-host values are sent to a local endpoint only",
		Src:             Src{Context: OriginCard, Output: OriginAPI, Temperature: OriginCard, TopP: OriginCard},
	}},

	// ── DeepSeek V4.1 Flash ────────────────────────────────────────────────
	// Replay is mandatory once tools are in the request: the hosted API answers
	// 400 without reasoning_content, and the open-weights encoder disables
	// drop_thinking by itself when tools are present.
	{[]string{"deepseek-v4.1-flash", "deepseek-v4p1-flash", "deepseek-flash",
		"deepseek-v4-flash", "deepseek-v4-flash-vision-exp", "deepseek-ai/deepseek-v4.1-flash"}, ModelProfile{
		Family:      "deepseek",
		Context:     1_048_576, // card: config.json text_config.max_position_embeddings; vLLM recipe --max-model-len (the API's "1M" is this rounded)
		Output:      393_216,   // api: "must be between 1 and 384K (393216)"
		Temperature: f64(1.0),  // card: recommended-sampling table, also the API default
		TopP:        f64(0.95), // card: table says "0.95 or 1.0", evals use 0.95
		Replay:      "all",
		Reasoning:   true,
		// api: the compat spellings are documented as accepted (minimal→low,
		// medium/xhigh→high, ultra→max), so they are listed rather than mapped here.
		Efforts: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
		// engine docs: vLLM's V4.1-Flash recipe documents the template kwarg as
		// low|high|xhigh|max (or an integer 1-100) — the four aliases above are
		// resolved by the HOSTED API and mean nothing to the checkpoint's own
		// template, so they are not sent to a self-host.
		EffortsLocal: []string{"low", "high", "xhigh", "max"},
		EffortOn:     "high", // api: the server's own default
		EffortOff:    "none",
		Switch:       ThinkSwitch{Kwarg: "thinking", On: true, Off: false, Effort: "reasoning_effort"},
		// Nothing is recorded for SGLang: no source maps THIS checkpoint to a
		// parser name there. A `deepseekv41` is in the registry, but a name that
		// looks like the model is not a statement that it detects the model's
		// format — and inheriting deepseekv4's value is the sibling guess this file
		// forbids. So doctor prints `auto` and says nothing is sourced.
		ToolParser:  ParserNames{VLLM: "deepseek_v41"},
		ReasonParse: ParserNames{VLLM: "deepseek_v41"},
		Note:        "replay is mandatory with tools (the API returns 400 without reasoning_content); lca sends the thinking kwarg AND the effort explicitly, so the request does not depend on either engine's default for thinking",
		Src:         Src{Context: OriginCard, Output: OriginAPI, Temperature: OriginCard, TopP: OriginCard},
	}},

	// ── GLM-5.3 (Z.ai / Zhipu) ─────────────────────────────────────────────
	// Both gateway spellings ("glm5.3", "glm-5.3") normalise to one key, which is
	// the point: before this table "glm-5.3" took the glm-5 rule (200k, 5× low)
	// and "glm5.3" took the generic glm rule (128k, no replay, no switch).
	{[]string{"glm-5.3", "glm5.3", "zai-org/glm-5.3"}, ModelProfile{
		Family:      "glm",
		Context:     1_048_576, // card: config.json max_position_embeddings; SGLang spec table; docs "1M-token context window"
		Output:      131_072,   // api: chat-completion schema maximum
		Temperature: f64(1.0),  // card: generation_config.json (and the API default for the series)
		TopP:        f64(0.95), // card: generation_config.json (and the API default)
		Replay:      "all",     // api: "Preserved Thinking" — clear_thinking:false, forward the full history verbatim
		Reasoning:   true,
		Efforts:     []string{"low", "high", "max"},
		EffortOn:    "max", // api: the vendor's default, and "for coding we recommend max"
		// no EffortOff: thinking.type accepts only "enabled" — "disabled" fails the request.
		Switch: ThinkSwitch{Effort: "reasoning_effort", Keep: "clear_thinking", KeepVal: false},
		// glm45 as the TOOL-call parser silently breaks tool calling, and on SGLang
		// `glm47` is a TOOL-call parser only: it is in function_call's registry and
		// not in the reasoning one, so --reasoning-parser glm47 fails startup there.
		ToolParser:  ParserNames{VLLM: "glm47", SGLang: "glm47"},
		ReasonParse: ParserNames{VLLM: "glm47", SGLang: "glm45"},
		Note:        "temperature is clamped to [0,1]; thinking cannot be disabled; an effort level outside low|high|max is silently promoted to max",
		Src:         Src{Context: OriginCard, Output: OriginAPI, Temperature: OriginCard, TopP: OriginCard},
	}},

	// ── Qwen3.6 (Alibaba) ──────────────────────────────────────────────────
	// No reasoning_effort: it is documented for Qwen3.8 only, and 3.6's analogue
	// is thinking_budget (a token cap), which is not an effort level.
	{[]string{"qwen3.6", "qwen3.6-27b", "qwen3.6-35b-a3b", "qwen/qwen3.6-27b", "qwen/qwen3.6-35b-a3b"}, ModelProfile{
		Family:      "qwen",
		Context:     262_144,   // card: "Context Length: 262,144 natively"; config.json max_position_embeddings
		Output:      32_768,    // card: "we recommend an output length of 32,768 tokens for most queries" — a recommendation, not a cap
		Temperature: f64(1.0),  // card: generation_config.json (0.6 is the precise-coding variant)
		TopP:        f64(0.95), // card: both cards, both modes; generation_config.json
		TopK:        20,        // card: both cards; generation_config.json top_k: 20
		Replay:      "all",     // card: trained to "preserve and leverage thinking traces from historical messages"
		Reasoning:   true,
		EffortNone:  true, // reasoning_effort is documented for Qwen3.8 only; 3.6's analogue is thinking_budget, a token cap
		Switch:      ThinkSwitch{Kwarg: "enable_thinking", On: true, Off: false, Keep: "preserve_thinking", KeepVal: true},
		ToolParser:  ParserNames{VLLM: "qwen3_coder", SGLang: "qwen3_coder"}, // card's own flags, not vLLM's generic doc
		ReasonParse: ParserNames{VLLM: "qwen3", SGLang: "qwen3"},
		Note:        "no reasoning_effort exists for 3.6; presence_penalty is left out because the 27B and 35B-A3B cards disagree (0.0 vs 1.5); the 1,010,000 YaRN window is not the served default",
		Src:         Src{Context: OriginCard, Output: OriginCard, Temperature: OriginCard, TopP: OriginCard, TopK: OriginCard},
	}},

	// ── MiniMax-M3 ─────────────────────────────────────────────────────────
	// An interleaved-thinking model: stripping its reasoning degrades it silently
	// rather than erroring, which is why Replay is "all" and not "".
	{[]string{"minimax-m3", "minimaxai/minimax-m3", "minimax/minimax-m3"}, ModelProfile{
		Family:      "minimax",
		Context:     1_048_576, // card: config.json text_config.max_position_embeddings; vLLM recipe (the hosted API's 1,000,000 is a COMBINED in+out budget)
		Output:      524_288,   // api: "the maximum is 524288 (512K)", recommended 131072
		Temperature: f64(1.0),  // card: generation_config.json
		TopP:        f64(0.95), // card: generation_config.json
		TopK:        40,        // engine docs only: the vLLM recipe and SGLang cookbook state it, the card does not — so chat.go sends it to a local server only
		Replay:      "all",     // api + card: all three vendor surfaces require verbatim replay; the template re-renders reasoning for every assistant turn
		Reasoning:   true,
		// api: "only MiniMax-M3.1-Flash-Preview supports real thinking-depth
		// tuning; other models ignore this field."
		EffortNone: true,
		Switch:     ThinkSwitch{Kwarg: "thinking_mode", On: "enabled", Off: "disabled"},
		// SGLang spells it with a HYPHEN in both registries; lca's underscore is
		// vLLM's spelling and fails SGLang's argparse. vLLM additionally needs a
		// mandatory --block-size 128, which is vLLM-only.
		ToolParser:  ParserNames{VLLM: "minimax_m3", SGLang: "minimax-m3"},
		ReasonParse: ParserNames{VLLM: "minimax_m3", SGLang: "minimax-m3"},
		Note:        "top_k 40 is engine docs, not the card, so it goes to a local server only; reasoning_effort is ignored by M3; vLLM also needs --block-size 128, which SGLang has no equivalent of; the hosted API's 1,000,000 window counts input+output together",
		Src:         Src{Context: OriginCard, Output: OriginAPI, Temperature: OriginCard, TopP: OriginCard, TopK: OriginEngine},
	}},

	// ── Hunyuan 3 (Tencent) ────────────────────────────────────────────────
	// The sharpest edge of the six: reasoning_effort IS the switch, it must go
	// through chat_template_kwargs, and anything outside no_think|low|high reaches
	// the Jinja template and fires its raise_exception — an error on every turn
	// (SGLang surfaces a template render failure as a 400).
	// hy3 IS Hunyuan 3 (HF tencent/HY3, config.json model_type "hy_v3"), so the
	// spellings an operator may serve it under belong here: "hunyuan-3" used to
	// fall through to the Hunyuan-2-era generic rule and take half the window,
	// no thinking switch and no replay — under a confident green doctor line.
	{[]string{"hy3", "hy_v3", "hunyuan-3", "hunyuan-v3", "tencent/hy3", "tencent-hunyuan/hy3"}, ModelProfile{
		Family:      "hunyuan",
		Context:     262_144,  // card: config.json max_position_embeddings; vLLM recipe context_length (the card's "256K" is this number)
		Output:      131_072,  // api: Tencent Cloud's "max output 128k" read as tokens
		Temperature: f64(0.9), // card: "Recommended parameters: temperature=0.9, top_p=1.0"; generation_config.json
		TopP:        f64(1.0), // card: same
		Replay:      "all",    // card: the template sets preserved_thinking=true whenever tools are passed, which is this agent's normal case
		Reasoning:   true,
		Efforts:     []string{"no_think", "low", "high"},
		// policy, not a vendor default: high is the only deep level this template
		// accepts, so a bare "think" picks the deeper of the two, by our choice.
		// What "no level at all" means is NOT a fact about hy3 but about the
		// deployment's template variant (see Note), which is why lca always names
		// a level and never relies on the default.
		EffortOn:  "high",
		EffortOff: "no_think",
		// Effort and nothing else. A `preserved_thinking: true` kwarg was proposed
		// here and is NOT sent: no source names it, and the template's own default
		// (true whenever tools are present) is what the Replay comment above already
		// records — so naming it would have been a guess at the wire in exchange for
		// nothing, and admitting it required weakening the two assertions in
		// models_test.go that say hy3 is sent NOTHING outside no_think|low|high.
		Switch: ThinkSwitch{Effort: "reasoning_effort"},
		// `hy_v3` is the model_type and vLLM's own parser name; SGLang registers
		// this checkpoint under `hunyuan` in BOTH registries and has no hy_v3 in
		// either, so the two spellings are not interchangeable.
		ToolParser:  ParserNames{VLLM: "hy_v3", SGLang: "hunyuan"},
		ReasonParse: ParserNames{VLLM: "hy_v3", SGLang: "hunyuan"},
		Note:        "only no_think|low|high are safe on BOTH engines (the wider minimal|medium|xhigh|max set is folded by SGLang >=0.5.20 and only with the hunyuan_effort template, which no /v1 route reveals); whether hy3 thinks when no level is sent depends on the deployment's template variant — unset means high on a hunyuan_effort template and no thinking at all on an Hy3-preview one, both served as --reasoning-parser hunyuan, so lca always sends a level and doctor probes which variant answered",
		Src:         Src{Context: OriginCard, Output: OriginAPI, Temperature: OriginCard, TopP: OriginCard},
		// TopK is deliberately unset: generation_config.json says -1 (disabled),
		// which the TopK int field cannot express and must not be rounded to 0.
	}},
}

// ── family fallbacks ───────────────────────────────────────────────────────
// Matched by token, longest token first, at token boundaries only. Order is no
// longer load-bearing (it was: "deepseek-v4.1-flash" resolved correctly only
// because the specific rule happened to be listed above "deepseek-v4").
//
// Several entries carry a Family and no numbers on purpose. Those are the models
// whose ids sit next to a served one and whose numbers are NOT interchangeable:
// inheriting a sibling's window is the failure this file exists to stop, and
// unset beats wrong. lca doctor says so out loud, because a silent "no numbers"
// is how this class of bug hides.

type profileRule struct {
	match []string // any token of the normalised id, at a token boundary
	prof  ModelProfile
}

var profileRules = []profileRule{
	// ── DeepSeek ───────────────────────────────────────────────────────────
	// V4-Pro is hosted-only: no open weights, so no card and no window of its
	// own here. It once carried V4.1-Flash's 1,048,576 as OriginCard on the
	// grounds that DeepSeek routes v4-pro to Flash — a routing DeepSeek's own
	// changelog reversed ("we have decided to continue providing API services
	// for DeepSeek V4 Pro"), and which named a different model id (v4-flash)
	// even while it held. That is the sibling inheritance this file exists to
	// stop, so the window is gone; the max_tokens ceiling stays because the
	// chat-completions reference documents it for this id.
	{[]string{"deepseek-v4", "deepseek-v4-pro"}, ModelProfile{Family: "deepseek", Output: 393_216, Replay: "all", Reasoning: true,
		Efforts: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, EffortOn: "high", EffortOff: "none",
		EffortsLocal: []string{"low", "high", "xhigh", "max"},
		Switch:       ThinkSwitch{Kwarg: "thinking", On: true, Off: false, Effort: "reasoning_effort"},
		Note:         "V4-Pro is a hosted-only DeepSeek model: its max_tokens ceiling is the API reference's 393216 and it has no published card, so no window is recorded here",
		Src:          Src{Output: OriginAPI}}},
	// The rule above is matched by token, and "deepseek-v4" sits at a token
	// boundary inside "deepseek-v4.1-pro" — so a V4.1/V4.2 that is NOT the Flash
	// enumerated above used to inherit V4-Pro's numbers wholesale. It gets a
	// family and nothing else instead, which is what we actually know.
	{[]string{"deepseek-v4.1", "deepseek-v4.2"}, ModelProfile{Family: "deepseek", Reasoning: true,
		Note: "a DeepSeek V4.1/V4.2 variant other than the Flash listed above — no window or sampling is published here"}},
	{[]string{"deepseek-v5"}, ModelProfile{Family: "deepseek", Reasoning: true,
		Note: "no window or sampling is published here for DeepSeek V5 — V4.1-Flash's numbers are not interchangeable"}},
	{[]string{"deepseek-reasoner", "deepseek-r1"}, ModelProfile{Family: "deepseek", Context: 128_000, Output: 64_000, Replay: "turn", Reasoning: true}},
	{[]string{"deepseek-v3.2", "deepseek-v3.1"}, ModelProfile{Family: "deepseek", Context: 128_000, Output: 64_000, Replay: "turn", Reasoning: true}},
	{[]string{"deepseek"}, ModelProfile{Family: "deepseek", Context: 128_000, Output: 8_192}},

	// ── Kimi (Moonshot) ────────────────────────────────────────────────────
	{[]string{"kimi-k2-thinking", "kimi-k2.5", "kimi-k2.6", "kimi-k2.7", "kimi-k2p", "kimi-k2-5", "kimi-for-coding"},
		ModelProfile{Family: "kimi", Context: 262_144, Output: 262_144, Temperature: f64(1.0), TopP: f64(0.95), Replay: "all", Reasoning: true}},
	{[]string{"kimi-k2-0711"}, ModelProfile{Family: "kimi", Context: 131_072, Output: 16_384, Temperature: f64(0.6)}},
	// A Kimi after K3 keeps its family and nothing else: K3's window is 1M and
	// K2's is 262k, and guessing which one a K4 has is the sibling inheritance
	// this file forbids.
	{[]string{"kimi-k4", "kimi-k5"}, ModelProfile{Family: "kimi", Reasoning: true,
		Note: "a Kimi version after K3 — no window or sampling is published here; K3's numbers are not interchangeable"}},
	// No temperature: 0.6 is kimi-k2-0711's value and is sourced for nothing
	// else, so the field is left out and the server's own default applies.
	{[]string{"kimi"}, ModelProfile{Family: "kimi", Context: 262_144, Output: 262_144}},

	// ── GLM (Zhipu / Z.ai) ─────────────────────────────────────────────────
	// glm-5.3-flash / -flashx are a separately architected 320B-A18B model: same
	// version number, numbers that are not interchangeable with GLM-5.3's — so no
	// window. The effort vocabulary IS published for this exact id, though, and
	// leaving it out was an unsourced omission of its own: doctor said "nothing
	// recorded" about something that is recorded.
	{[]string{"glm-5.3-flash", "glm-5.3-flashx"}, ModelProfile{Family: "glm", Reasoning: true,
		Efforts: []string{"low", "high", "max"}, EffortOn: "max", // api: docs.z.ai Deep Thinking lists GLM-5.3-FLASH with GLM-5.3
		Switch: ThinkSwitch{Effort: "reasoning_effort"},
		Note:   "GLM-5.3-Flash is a different architecture from GLM-5.3 — no window or sampling of its own is published here"}},
	// Efforts on the two rules below were GLM-5.3's three values copied onto a
	// sibling version: Z.ai documents reasoning_effort for 5.2 and above, with a
	// different vocabulary per version and none at all for 5 and 5.1. An unknown
	// level is silently promoted to max — the most expensive setting there is —
	// so with no sourced list the honest thing is to send no level. clear_thinking
	// stays: "Preserved Thinking" is an API feature of the whole series.
	{[]string{"glm-5.2", "glm-5-2", "glm-5p2"}, ModelProfile{Family: "glm", Context: 1_000_000, Output: 131_072, Replay: "all", Reasoning: true,
		Switch: ThinkSwitch{Keep: "clear_thinking", KeepVal: false}}},
	{[]string{"glm-5"}, ModelProfile{Family: "glm", Context: 200_000, Output: 131_072, Replay: "all", Reasoning: true,
		Switch: ThinkSwitch{Keep: "clear_thinking", KeepVal: false}}},
	// "glm-5" matches at a token boundary inside every future glm-5.x, which
	// handed glm-5.4 GLM-5's 200,000-token window. Longest-token-first keeps
	// these numberless guards in front of it, the way the Qwen block does.
	{[]string{"glm-5.4", "glm-5.5", "glm-6"}, ModelProfile{Family: "glm", Reasoning: true,
		Note: "no window or sampling is published here for this GLM version — GLM-5.3's numbers are not interchangeable"}},
	{[]string{"glm-4.7", "glm-4.6"}, ModelProfile{Family: "glm", Context: 204_800, Output: 131_072, Temperature: f64(1.0), Replay: "all", Reasoning: true}},
	{[]string{"glm-4.5v", "glm-4.6v"}, ModelProfile{Family: "glm", Context: 64_000, Output: 16_384, Reasoning: true}},
	{[]string{"glm-4.5"}, ModelProfile{Family: "glm", Context: 131_072, Output: 98_304, Reasoning: true}},
	{[]string{"glm"}, ModelProfile{Family: "glm", Context: 128_000, Output: 32_768}},

	// ── MiniMax ────────────────────────────────────────────────────────────
	// M3.1-Flash-Preview is the only MiniMax where reasoning_effort does anything,
	// so it must not inherit M3's profile (which documents the opposite).
	{[]string{"minimax-m3.1", "minimax-m3p1"}, ModelProfile{Family: "minimax", Reasoning: true,
		Note: "M3.1-Flash-Preview is a different model and the only MiniMax that tunes thinking depth — no numbers of its own are published here"}},
	{[]string{"minimax-m2.1", "minimax-m2.5", "minimax-m25", "minimax-m21"}, ModelProfile{Family: "minimax", Context: 204_800, Output: 131_072, Temperature: f64(1.0), TopP: f64(0.95), TopK: 40, Reasoning: true}},
	{[]string{"minimax-m2"}, ModelProfile{Family: "minimax", Context: 196_608, Output: 128_000, Temperature: f64(1.0), TopP: f64(0.95), TopK: 20, Reasoning: true}},
	{[]string{"minimax"}, ModelProfile{Family: "minimax", Context: 204_800, Output: 131_072, Temperature: f64(1.0), TopP: f64(0.95)}},

	// ── Hunyuan (Tencent) ──────────────────────────────────────────────────
	{[]string{"hunyuan-t1", "hunyuan-2.0-thinking", "hunyuan-a13b"}, ModelProfile{Family: "hunyuan", Context: 131_072, Output: 16_384, Replay: "turn", Reasoning: true}},
	{[]string{"hunyuan"}, ModelProfile{Family: "hunyuan", Context: 131_072, Output: 16_384}},

	// ── Qwen (Alibaba) ─────────────────────────────────────────────────────
	// The hosted qwen3.6-* ids are different models with different windows; only
	// -plus has a published one.
	{[]string{"qwen3.6-plus"}, ModelProfile{Family: "qwen", Context: 1_000_000, Output: 65_536, Reasoning: true,
		Src: Src{Context: OriginAPI, Output: OriginAPI}}},
	{[]string{"qwen3.6-flash", "qwen3.6-max"}, ModelProfile{Family: "qwen", Reasoning: true,
		Note: "a hosted Qwen3.6 variant, not the open weights — no window of its own is published here"}},
	{[]string{"qwen3-coder-plus", "qwen3-coder-flash", "qwen-plus"}, ModelProfile{Family: "qwen", Context: 1_000_000, Output: 65_536}},
	// One rule asserting one window for 3.5 through 4 was the sibling guess this
	// file forbids: 3.6's numbers are above, and the others carry none.
	{[]string{"qwen3.5", "qwen3.7", "qwen3.8", "qwen3.9", "qwen4"}, ModelProfile{Family: "qwen", Reasoning: true,
		Note: "no window or sampling is published here for this Qwen version — the numbers are 3.6's and are not interchangeable"}},
	{[]string{"qwen3-coder"}, ModelProfile{Family: "qwen", Context: 262_144, Output: 65_536}},
	{[]string{"qwen3-max"}, ModelProfile{Family: "qwen", Context: 262_144, Output: 65_536}},
	{[]string{"qwen3-235b", "qwen3-30b", "qwen3-32b", "qwen3-14b", "qwen3-8b"}, ModelProfile{Family: "qwen", Context: 131_072, Output: 16_384, Reasoning: true}},
	{[]string{"qwen", "qwq"}, ModelProfile{Family: "qwen", Context: 131_072, Output: 32_768}},
}

// ── the matcher ────────────────────────────────────────────────────────────

// quantSuffixes describe the checkpoint's precision, not the model: a served
// "Hy3-NVFP4-FP8" is hy3 and takes hy3's numbers.
var quantSuffixes = []string{"-fp8", "-fp16", "-bf16", "-fp4", "-mxfp8", "-mxfp4", "-nvfp4", "-awq", "-gptq", "-int4", "-int8"}

// normalizeModelID reduces a served id to the one spelling the tables are keyed
// on. The table's own keys go through it too, so the rules can be written the
// way a vendor writes them and "glm5.3" == "glm-5.3" without listing both.
func normalizeModelID(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:] // zai-org/GLM-5.3 → glm-5.3
	}
	// A trailing [...] is this client's own context hint ("glm-5.3[1m]"), never
	// part of the served id.
	if i := strings.LastIndex(id, "["); i >= 0 && strings.HasSuffix(id, "]") {
		id = strings.TrimSpace(id[:i])
	}
	for trimmed := true; trimmed; {
		trimmed = false
		for _, q := range quantSuffixes {
			if strings.HasSuffix(id, q) {
				id, trimmed = strings.TrimSuffix(id, q), true
			}
		}
	}
	// A p or _ between two digits is a decimal point: kimi-k2p6 → kimi-k2.6.
	b := []byte(id)
	for i := 1; i+1 < len(b); i++ {
		if (b[i] == 'p' || b[i] == '_') && isDigitByte(b[i-1]) && isDigitByte(b[i+1]) {
			b[i] = '.'
		}
	}
	id = strings.ReplaceAll(string(b), "_", "-")
	// A digit straight after a letter starts a version number: hy3 → hy-3,
	// glm5.3 → glm-5.3. This is what collapses the two gateway spellings of
	// GLM-5.3 onto one key, and it is why hy3 needs a real entry rather than a
	// three-character token that Contains would find anywhere.
	var out strings.Builder
	out.Grow(len(id) + 4)
	for i := 0; i < len(id); i++ {
		if i > 0 && isDigitByte(id[i]) && isLetterByte(id[i-1]) {
			out.WriteByte('-')
		}
		out.WriteByte(id[i])
	}
	return out.String()
}

func isDigitByte(c byte) bool  { return c >= '0' && c <= '9' }
func isLetterByte(c byte) bool { return c >= 'a' && c <= 'z' }
func isIDSep(c byte) bool      { return c == '-' || c == '.' }

// profileByID is the exact map of the canonical ids, checked before any token
// scan: O(1), order-independent, and a prefix can never reach it.
var profileByID = func() map[string]ModelProfile {
	m := map[string]ModelProfile{}
	for _, c := range canonicalProfiles {
		p := c.prof
		p.Key = c.ids[0]
		for _, id := range c.ids {
			m[normalizeModelID(id)] = p
		}
	}
	return m
}()

type profileToken struct {
	norm string // the token, normalised
	key  string // the token as written, for doctor
	prof ModelProfile
}

// profileTokens is every fallback rule's tokens, longest first. Longest-match is
// what makes a versioned id prefer its own token over a shorter family one, now
// that rule order no longer decides anything.
var profileTokens = func() []profileToken {
	var out []profileToken
	for _, r := range profileRules {
		for _, tok := range r.match {
			out = append(out, profileToken{normalizeModelID(tok), tok, r.prof})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].norm) > len(out[j].norm) })
	return out
}()

// tokenMatch reports whether tok occurs in id between token boundaries (start or
// end of the id, or a - / . beside it). Without the boundary check "hy-3" would
// match inside "alchy-3x" and a bare family token would claim a versioned id it
// knows nothing about.
func tokenMatch(id, tok string) bool {
	for i := 0; i+len(tok) <= len(id); i++ {
		if id[i:i+len(tok)] != tok {
			continue
		}
		if i > 0 && !isIDSep(id[i-1]) {
			continue
		}
		if j := i + len(tok); j < len(id) && !isIDSep(id[j]) {
			continue
		}
		return true
	}
	return false
}

// lookupProfile resolves the profile for a served model id. Vendor prefixes
// ("Qwen/", "accounts/fireworks/models/"), quantisation suffixes and the two
// spellings of a version number all land on the same entry. An id nobody
// enumerated gets at most a family-level entry, and an unrecognised one gets the
// zero profile: nothing is overridden and nothing is sent.
func lookupProfile(model string) ModelProfile {
	id := normalizeModelID(model)
	if p, ok := profileByID[id]; ok {
		return p
	}
	for _, t := range profileTokens {
		if tokenMatch(id, t.norm) {
			p := t.prof
			p.Key = t.key
			return p
		}
	}
	return ModelProfile{}
}

// effortFor maps a requested effort level onto what this model documents. An
// unrecognised level is DROPPED rather than forwarded: hy3 raises inside its
// Jinja template on anything but no_think|low|high (a 500 every turn), and
// glm-5.3 silently promotes an unknown level to max — the most expensive setting
// there is. A known family with no documented vocabulary (Qwen3.6, MiniMax-M3)
// takes no effort field at all. A model we have no profile for takes what the
// operator typed: that is their instruction, not our guess.
func (p ModelProfile) effortFor(level string) string {
	if level == "" {
		return ""
	}
	if len(p.Efforts) == 0 {
		if p.Family == "" {
			return level
		}
		return ""
	}
	for _, e := range p.Efforts {
		if e == level {
			return level
		}
	}
	return ""
}

// effortForLocal is effortFor for the self-hosted path. The hosted API resolves
// its own compat spellings (DeepSeek maps minimal→low, medium/xhigh→high,
// ultra→max); the checkpoint's Jinja template resolves nothing, and a level it
// does not know is at best inert and at worst a raise — hy3's failure mode. So a
// profile may record the narrower list its engine documents, and only that list
// reaches a local endpoint.
func (p ModelProfile) effortForLocal(level string) string {
	if len(p.EffortsLocal) == 0 {
		return p.effortFor(level)
	}
	if level == "" {
		return ""
	}
	for _, e := range p.EffortsLocal {
		if e == level {
			return level
		}
	}
	return ""
}

// effortVocab is the list a level is checked against for a given path, so /model
// and doctor quote the same vocabulary the request will use.
func (p ModelProfile) effortVocab(local bool) []string {
	if local && len(p.EffortsLocal) > 0 {
		return p.EffortsLocal
	}
	return p.Efforts
}

// srcNum renders a stored number next to where it came from — "1.05M (card)",
// "262k (server)" — so an operator can tell a published window from a guess. A
// number with no recorded origin says so rather than borrowing authority.
func srcNum(n int, o Origin) string {
	if n <= 0 {
		return "unset"
	}
	return ctxfmt(n) + " (" + o.String() + ")"
}

// srcFloat is srcNum for a sampling value, which is absent rather than zero when
// nobody published it.
func srcFloat(v *float64, o Origin) string {
	if v == nil {
		return "unset"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64) + " (" + o.String() + ")"
}

// engineFlags is the launch flags THIS engine documents for this model, so doctor
// can turn "tool calling is broken" into a line an operator can paste. One
// renderer for the advice and the report, because the three places that spelled
// these flags out independently meant an engine fix made twice was a fix not made.
//
// engine == "" is the honest answer when nobody has said which engine serves the
// endpoint: both, labelled. It is never a guess at one of them.
func (p ModelProfile) engineFlags(engine string) string {
	switch engine {
	case engineSGLang:
		return p.sglangFlags()
	case engineVLLM:
		return p.vllmFlags()
	}
	return "vLLM: " + p.vllmFlags() + gSep + "SGLang: " + p.sglangFlags()
}

// vllmFlags is what it has always been, including --enable-auto-tool-choice,
// which vLLM requires before it will consider a tool-call parser at all.
func (p ModelProfile) vllmFlags() string {
	return "--enable-auto-tool-choice --tool-call-parser " + parserValue(p.ToolParser.VLLM) +
		" --reasoning-parser " + parserValue(p.ReasonParse.VLLM) + p.noValueNote(p.ToolParser.VLLM, p.ReasonParse.VLLM, "vLLM")
}

// sglangFlags leads with `auto`, which is sourced ("Use 'auto' to detect from
// chat template", a member of --tool-call-parser's and --reasoning-parser's
// choices at v0.5.20 and main) and is what both the Hy3 and GLM-5.3 cookbooks'
// own verified launch cells use. It NEVER emits --enable-auto-tool-choice: that
// flag does not exist in SGLang, and argparse fails the launch with
// "unrecognized arguments" — advice that stops the server from starting.
func (p ModelProfile) sglangFlags() string {
	out := "--tool-call-parser auto --reasoning-parser auto"
	tool, reason := p.ToolParser.SGLang, p.ReasonParse.SGLang
	if tool != "" && reason != "" {
		out += faint(" (or --tool-call-parser %s --reasoning-parser %s)", tool, reason)
	}
	return out + p.noValueNote(tool, reason, "SGLang")
}

// parserValue names the flag with a placeholder when nothing is recorded for that
// engine. The placeholder is deliberate: the other engine's token is a value this
// engine's argparse refuses, so printing it would be advice that breaks the launch.
func parserValue(v string) string {
	if v == "" {
		return "<family>"
	}
	return v
}

// noValueNote says so when a value is missing, rather than letting <family> read
// as an oversight. This is the no-guessing rule applied to advice.
func (p ModelProfile) noValueNote(tool, reason, engine string) string {
	if tool != "" && reason != "" {
		return ""
	}
	return faint(" (no parser value is recorded for %s on %s)", firstNonEmpty(p.Key, "this model"), engine)
}

// thinkingParams returns the request fields that switch reasoning on/off or set
// its effort. mode: "" (provider default), "on", "off", or an effort level.
// replay is the reasoning replay actually in force, because a kwarg asking the
// server to preserve thinking history is incoherent when we are not sending it.
// Only fields the model itself is known to accept are sent.
func thinkingParams(p *Provider, model string, prof ModelProfile, mode, replay string) map[string]any {
	mode = strings.ToLower(strings.TrimSpace(mode))
	on := mode == "on"
	off := mode == "off" || mode == "none"
	// named is "the caller asked for thinking by naming a level", which survives
	// the level itself being dropped: a model that documents no vocabulary still
	// has a switch, and `effort: max` must not end up asking for less than
	// `thinking: on`.
	named := mode != "" && !on && !off
	lvl := ""
	if named {
		if p.selfHosted() {
			// The narrower list is the CHECKPOINT TEMPLATE's, not vLLM's: a hosted
			// API resolves its own compat spellings and a Jinja template resolves
			// nothing, on either engine.
			lvl = prof.effortForLocal(mode)
		} else {
			lvl = prof.effortFor(mode)
		}
	}
	id := strings.ToLower(model)

	// Self-hosted first, ABOVE the per-vendor dialect switch: the switch a model
	// gets must come from its own chat template whatever the Dialect field happens
	// to hold, and while this was one arm of that switch, any other value —
	// including the "sglang" an operator could write in a config file — bypassed it
	// and sent a bare top-level reasoning_effort.
	if p.selfHosted() {
		return localThinking(prof, on, off, named, lvl, replay)
	}

	switch p.Dialect {
	case "deepseek":
		if !strings.Contains(id, "deepseek-v4") {
			return nil // chat/reasoner are fixed-mode model ids
		}
		switch {
		case off:
			return map[string]any{"thinking": map[string]any{"type": "disabled"}}
		case lvl != "":
			return map[string]any{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": lvl}
		case on:
			return map[string]any{"thinking": map[string]any{"type": "enabled"}}
		}
	case "moonshot":
		if !prof.Reasoning || strings.Contains(id, "thinking") {
			return nil
		}
		switch {
		case off:
			return map[string]any{"thinking": map[string]any{"type": "disabled"}}
		case lvl != "" && prof.Switch.TopLevel:
			// K3 takes a top-level effort and has no on/off switch at all.
			return map[string]any{"reasoning_effort": lvl}
		case on:
			return map[string]any{"thinking": map[string]any{"type": "enabled"}}
		}
	case "zai":
		if !prof.Reasoning {
			return nil
		}
		if off {
			return map[string]any{"thinking": map[string]any{"type": "disabled"}}
		}
		th := map[string]any{"type": "enabled"}
		if replay == "all" {
			// Preserved thinking, which is only coherent while we are replaying it:
			// with reasoning_replay: off, asking the server to keep the history is
			// asking it to keep something we never send.
			th["clear_thinking"] = false
		}
		out := map[string]any{"thinking": th}
		if lvl != "" {
			out["reasoning_effort"] = lvl
		}
		return out
	case "dashscope":
		// DashScope returns reasoning_content only with enable_thinking:true.
		if !prof.Reasoning && !on {
			return nil
		}
		if off {
			return map[string]any{"enable_thinking": false}
		}
		return map[string]any{"enable_thinking": true}
	case "tencent":
		switch {
		case off && prof.EffortOff != "":
			return map[string]any{"reasoning_effort": prof.EffortOff}
		case lvl != "":
			return map[string]any{"reasoning_effort": lvl}
		case on && prof.EffortOn != "":
			return map[string]any{"reasoning_effort": prof.EffortOn}
		}
	case "openrouter":
		switch {
		case off:
			return map[string]any{"reasoning": map[string]any{"enabled": false}}
		case lvl != "":
			return map[string]any{"reasoning": map[string]any{"effort": lvl}}
		case on:
			return map[string]any{"reasoning": map[string]any{"enabled": true}}
		}
	default:
		if lvl != "" {
			return map[string]any{"reasoning_effort": lvl}
		}
	}
	return nil
}

// localThinking is the self-hosted (vLLM / SGLang) path. The switch comes from
// the model's own chat template — which is what the profile describes — because
// the dialect's one-size answer is wrong for four of the six served models:
// GLM-5.3 wants reasoning_effort + clear_thinking, hy3 wants reasoning_effort
// with no_think|low|high, MiniMax-M3 wants thinking_mode, and Kimi-K3 has no
// switch at all and takes a top-level field.
// named means the caller asked for thinking by naming a level, and stays true
// when the level itself was dropped: the card's own switch must still be sent,
// or `effort: max` asks for strictly less than `thinking: on` does. It does NOT
// stand in for on when choosing a level, because EffortOn is our answer to
// "think", not to "think this much" — sending it would be inventing the level
// the operator did not get.
func localThinking(prof ModelProfile, on, off, named bool, lvl, replay string) map[string]any {
	sw := prof.Switch
	if sw.Kwarg == "" && sw.Effort == "" && sw.Keep == "" {
		// Nothing documented for this model: the old best-effort kwargs, read by
		// the Qwen3 / GLM-4 / Hunyuan-2 / DeepSeek-V3 templates and ignored by
		// the rest. Sent only when the caller actually asked for a change, and a
		// named level counts as asking. The level itself stays home: vLLM
		// validates a top-level reasoning_effort against a fixed list and 422s on
		// anything else, and we do not know what this model's template accepts.
		if want := on || named; want || off {
			return map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": want, "thinking": want}}
		}
		return nil
	}
	kw, top := map[string]any{}, map[string]any{}
	setEffort := func(l string) {
		if l == "" || sw.Effort == "" {
			return
		}
		if sw.TopLevel {
			top[sw.Effort] = l
		} else {
			kw[sw.Effort] = l
		}
	}
	switch {
	case off && sw.Kwarg != "":
		kw[sw.Kwarg] = sw.Off
	case off:
		setEffort(prof.EffortOff) // hy3: no_think IS the off switch
	default:
		if (on || named) && sw.Kwarg != "" {
			kw[sw.Kwarg] = sw.On
		}
		switch {
		case lvl != "":
			setEffort(lvl)
		case on:
			setEffort(prof.EffortOn)
		}
	}
	// Preserved thinking is a property of the HISTORY, not of this turn: while we
	// replay earlier reasoning_content, the kwarg that tells the template to keep
	// it belongs in every request. Leaving it out of the off branch is how
	// `thinking: off` on a model that cannot be switched off (GLM-5.3) produced
	// the one request shape that replays reasoning and then asks the template to
	// clear it.
	if sw.Keep != "" && replay == "all" {
		kw[sw.Keep] = sw.KeepVal
	}
	if len(kw) > 0 {
		top["chat_template_kwargs"] = kw
	}
	if len(top) == 0 {
		return nil
	}
	return top
}
