package main

import "strings"

// Per-family model profiles for the open models this agent orchestrates: Qwen,
// Kimi, GLM, DeepSeek, Hunyuan (hy3) and MiniMax. Everything is built in so the
// binary needs no model catalog download. Values come from the providers' docs
// as mirrored by models.dev / opencode's ProviderTransform:
//
//   - Context/Output: window and max output, used for the context budget and
//     max_tokens (capped at outputTokenMax) when the server doesn't report them.
//   - Temperature/TopP/TopK: vendor-recommended sampling; nil = server default.
//   - Replay: how reasoning_content is sent back (see wire in chat.go).
//   - Reasoning: the model thinks (or can), so a thinking toggle applies.
type ModelProfile struct {
	Family      string
	Context     int
	Output      int
	Temperature *float64
	TopP        *float64
	TopK        int
	Replay      string // "", "turn", "all"
	Reasoning   bool
	Effort      bool // accepts reasoning_effort levels
}

// outputTokenMax caps max_tokens even for models allowing more (opencode's
// OUTPUT_TOKEN_MAX): big enough for any single tool call, small enough to keep
// the reply budget from crowding out the prompt.
const outputTokenMax = 32_000

func f64(v float64) *float64 { return &v }

type profileRule struct {
	match []string // any substring (lowercased model id)
	prof  ModelProfile
}

// profileRules are checked in order, most specific first.
var profileRules = []profileRule{
	// ── DeepSeek ───────────────────────────────────────────────────────────
	{[]string{"deepseek-v4-flash", "deepseek-v4.1-flash", "deepseek-v4p1-flash"}, ModelProfile{Family: "deepseek", Context: 1_000_000, Output: 384_000, TopP: f64(0.95), Replay: "turn", Reasoning: true, Effort: true}},
	{[]string{"deepseek-v4", "deepseek-v5"}, ModelProfile{Family: "deepseek", Context: 1_000_000, Output: 384_000, Replay: "turn", Reasoning: true, Effort: true}},
	{[]string{"deepseek-reasoner", "deepseek-r1"}, ModelProfile{Family: "deepseek", Context: 128_000, Output: 64_000, Replay: "turn", Reasoning: true}},
	{[]string{"deepseek-v3.2", "deepseek-v3.1"}, ModelProfile{Family: "deepseek", Context: 128_000, Output: 64_000, Replay: "turn", Reasoning: true}},
	{[]string{"deepseek"}, ModelProfile{Family: "deepseek", Context: 128_000, Output: 8_192}},

	// ── Kimi (Moonshot) ────────────────────────────────────────────────────
	{[]string{"kimi-k3", "kimi-k4", "kimi-k5"},
		ModelProfile{Family: "kimi", Context: 262_144, Output: 262_144, Temperature: f64(1.0), TopP: f64(0.95), Replay: "all", Reasoning: true, Effort: true}},
	{[]string{"kimi-k2-thinking", "kimi-k2.5", "kimi-k2.6", "kimi-k2.7", "kimi-k2p", "kimi-k2-5", "kimi-for-coding"},
		ModelProfile{Family: "kimi", Context: 262_144, Output: 262_144, Temperature: f64(1.0), TopP: f64(0.95), Replay: "all", Reasoning: true}},
	{[]string{"kimi-k2-0711"}, ModelProfile{Family: "kimi", Context: 131_072, Output: 16_384, Temperature: f64(0.6)}},
	{[]string{"kimi"}, ModelProfile{Family: "kimi", Context: 262_144, Output: 262_144, Temperature: f64(0.6)}},

	// ── GLM (Zhipu / Z.ai) ─────────────────────────────────────────────────
	{[]string{"glm-5.2", "glm-5-2", "glm-5p2"}, ModelProfile{Family: "glm", Context: 1_000_000, Output: 131_072, Replay: "all", Reasoning: true, Effort: true}},
	{[]string{"glm-5"}, ModelProfile{Family: "glm", Context: 200_000, Output: 131_072, Replay: "all", Reasoning: true, Effort: true}},
	{[]string{"glm-4.7", "glm-4.6"}, ModelProfile{Family: "glm", Context: 204_800, Output: 131_072, Temperature: f64(1.0), Replay: "all", Reasoning: true}},
	{[]string{"glm-4.5v", "glm-4.6v"}, ModelProfile{Family: "glm", Context: 64_000, Output: 16_384, Reasoning: true}},
	{[]string{"glm-4.5"}, ModelProfile{Family: "glm", Context: 131_072, Output: 98_304, Reasoning: true}},
	{[]string{"glm"}, ModelProfile{Family: "glm", Context: 128_000, Output: 32_768}},

	// ── MiniMax (thinking is inline <think> in content; kept verbatim) ─────
	{[]string{"minimax-m3"}, ModelProfile{Family: "minimax", Context: 1_000_000, Output: 128_000, Temperature: f64(1.0), TopP: f64(0.95), TopK: 40, Reasoning: true}},
	{[]string{"minimax-m2.", "minimax-m25", "minimax-m21"}, ModelProfile{Family: "minimax", Context: 204_800, Output: 131_072, Temperature: f64(1.0), TopP: f64(0.95), TopK: 40, Reasoning: true}},
	{[]string{"minimax-m2"}, ModelProfile{Family: "minimax", Context: 196_608, Output: 128_000, Temperature: f64(1.0), TopP: f64(0.95), TopK: 20, Reasoning: true}},
	{[]string{"minimax"}, ModelProfile{Family: "minimax", Context: 204_800, Output: 131_072, Temperature: f64(1.0), TopP: f64(0.95)}},

	// ── Hunyuan (Tencent) ──────────────────────────────────────────────────
	{[]string{"hy3"}, ModelProfile{Family: "hunyuan", Context: 256_000, Output: 64_000, Replay: "turn", Reasoning: true, Effort: true}},
	{[]string{"hunyuan-t1", "hunyuan-2.0-thinking", "hunyuan-a13b"}, ModelProfile{Family: "hunyuan", Context: 131_072, Output: 16_384, Replay: "turn", Reasoning: true}},
	{[]string{"hunyuan"}, ModelProfile{Family: "hunyuan", Context: 131_072, Output: 16_384}},

	// ── Qwen (Alibaba) ─────────────────────────────────────────────────────
	{[]string{"qwen3-coder-plus", "qwen3-coder-flash", "qwen-plus"}, ModelProfile{Family: "qwen", Context: 1_000_000, Output: 65_536}},
	{[]string{"qwen3.5", "qwen3.6", "qwen3.7", "qwen3.8", "qwen3.9", "qwen4"}, ModelProfile{Family: "qwen", Context: 262_144, Output: 65_536, Reasoning: true}},
	{[]string{"qwen3-coder"}, ModelProfile{Family: "qwen", Context: 262_144, Output: 65_536}},
	{[]string{"qwen3-max"}, ModelProfile{Family: "qwen", Context: 262_144, Output: 65_536}},
	{[]string{"qwen3-235b", "qwen3-30b", "qwen3-32b", "qwen3-14b", "qwen3-8b"}, ModelProfile{Family: "qwen", Context: 131_072, Output: 16_384, Reasoning: true}},
	{[]string{"qwen", "qwq"}, ModelProfile{Family: "qwen", Context: 131_072, Output: 32_768}},
}

// lookupProfile resolves the profile for a model id (vendor prefixes such as
// "Qwen/", "deepseek-ai/" or "accounts/fireworks/models/" are fine — matching
// is by substring). Unknown models get a zero profile: nothing is overridden.
func lookupProfile(model string) ModelProfile {
	id := strings.ToLower(model)
	for _, r := range profileRules {
		for _, m := range r.match {
			if strings.Contains(id, m) {
				return r.prof
			}
		}
	}
	return ModelProfile{}
}

// thinkingParams returns the request fields that switch reasoning on/off or set
// its effort, in each provider's dialect. mode: "" (provider default), "on",
// "off", or an effort level (low|medium|high|max). Only fields the provider is
// known to accept are sent — hosted APIs may reject unknown ones.
func thinkingParams(p *Provider, model string, prof ModelProfile, mode string) map[string]any {
	mode = strings.ToLower(strings.TrimSpace(mode))
	effort := mode == "low" || mode == "medium" || mode == "high" || mode == "max" || mode == "xhigh"
	on := mode == "on" || effort
	off := mode == "off" || mode == "none"
	id := strings.ToLower(model)

	switch p.Dialect {
	case "deepseek":
		if !strings.Contains(id, "deepseek-v4") {
			return nil // chat/reasoner are fixed-mode model ids
		}
		switch {
		case off:
			return map[string]any{"thinking": map[string]any{"type": "disabled"}}
		case effort:
			return map[string]any{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": mode}
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
		case on:
			return map[string]any{"thinking": map[string]any{"type": "enabled"}}
		}
	case "zai":
		// Preserved thinking (clear_thinking:false) keeps reasoning across tool
		// calls; opencode enables it by default for Z.ai / Zhipu.
		if !prof.Reasoning {
			return nil
		}
		if off {
			return map[string]any{"thinking": map[string]any{"type": "disabled"}}
		}
		out := map[string]any{"thinking": map[string]any{"type": "enabled", "clear_thinking": false}}
		if effort && prof.Effort {
			out["reasoning_effort"] = mode
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
		if effort && prof.Effort {
			return map[string]any{"reasoning_effort": mode}
		}
	case "openrouter":
		switch {
		case off:
			return map[string]any{"reasoning": map[string]any{"enabled": false}}
		case effort:
			return map[string]any{"reasoning": map[string]any{"effort": mode}}
		case on:
			return map[string]any{"reasoning": map[string]any{"enabled": true}}
		}
	case "vllm":
		// Chat-template switches understood by the Qwen3, GLM, Hunyuan and
		// DeepSeek-V3.x templates; unknown kwargs are ignored by the others.
		if on || off {
			return map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": on, "thinking": on}}
		}
	default:
		if effort {
			return map[string]any{"reasoning_effort": mode}
		}
	}
	return nil
}
