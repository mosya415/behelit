package main

import (
	"errors"
	"net"
	"strings"
)

// errorHint turns a failure into the next thing to try — the difference
// between an error message and a usable one. Every route names the in-session
// command beside the environment variable: an error that can only be acted on by
// leaving the program is the same complaint in a different place.
func errorHint(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 401 || ae.Status == 403:
			return "the endpoint rejected the credentials — /set api_key_env <VAR> names the variable holding the key (or export LCA_API_KEY)"
		case ae.Status == 404 && strings.Contains(msg, "model"):
			return "that model isn't served here — /model lists what the gateway serves (lca doctor from the shell); fix the name in roles.yaml"
		case ae.Status == 404:
			return "wrong URL? the endpoint must include the API base path — /set endpoint http://node:18080/v1 (or LCA_BASE_URL)"
		case isContextOverflow(err):
			return "the conversation no longer fits — /compact summarizes it, /reset starts over"
		case ae.Status >= 500 && strings.Contains(msg, "failed to parse reasoning content"):
			// SGLang's own message, with type "InternalServerError" and status 500:
			// the signature of a --reasoning-parser that does not match this model's
			// output, and identifiable from the body alone — which is why it belongs
			// here, where there is no provider and no profile and neither is needed.
			return "the server's --reasoning-parser doesn't match this model's output — on SGLang set it to auto (/doctor names the value)"
		case ae.Status == 400 && strings.Contains(msg, "function.arguments must be"):
			// SGLang raises on history tool-call arguments that are not valid JSON
			// OBJECTS, and lca replays tc.Function.Arguments verbatim — so a blob the
			// model emitted once, and the engine accepted once, 400s on the next turn.
			// Deliberately not rewritten to {} behind the operator: that would edit
			// the history they are looking at.
			return "the model emitted tool-call arguments that are not a JSON object; SGLang refuses them when they are replayed in the history — /compact drops that turn"
		case (ae.Status == 400 || ae.Status == 422) && ourField(msg) != "":
			// A 400 that names a field THIS CLIENT put in the body, which is the one
			// error an operator cannot act on from the message alone: the engine says
			// "unknown field enable_thinking" and nothing on screen connects that to
			// a chat template lca is trying to steer. It fires every turn and reads
			// as "the model cannot print". Both remedies are named because they are
			// different scopes: the session's switch for right now, the model card's
			// for the deployment.
			f := ourField(msg)
			if f == "chat_template_kwargs" || templateKwargNames[f] {
				// Only the remedy that works: `/think off` is NOT one, because for
				// every model whose switch is a kwarg (hy3's no_think, GLM's
				// clear_thinking) the OFF position is itself a kwarg — so turning
				// thinking off keeps sending the bag the server just refused, and the
				// next turn fails identically.
				return "the server rejected " + f + ", which lca sends to steer this model's chat template — a deployment served without that template refuses it on every turn, and turning thinking off does not help because the off switch is a kwarg too: put models." + "<id>" + ".template_kwargs: off in roles.yaml to stop sending them for this model"
			}
			return "the server rejected the field " + f + ", which lca put in the request for this model — /doctor shows what it sends and from which source; models.<id>: in roles.yaml overrides it"
		case ae.Status == 400 && (strings.Contains(msg, "tool") || strings.Contains(msg, "auto")):
			return "the engine refused tool calling — enable its tool-call parser (vLLM: --enable-auto-tool-choice --tool-call-parser <family>; SGLang: --tool-call-parser auto), or set transport: text for this model (/doctor checks it)"
		}
		return ""
	}
	var ne net.Error
	switch {
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host"):
		return "nothing answers at that endpoint — is the gateway up? /setup asks for it again, /doctor checks the setup"
	case errors.As(err, &ne) && ne.Timeout():
		return "the endpoint timed out — a cold model may still be starting; try again, or /doctor"
	case strings.Contains(msg, "no api key"):
		return "/set api_key_env <VAR> names the variable holding the key, or /setup points lca at a gateway instead"
	case strings.Contains(msg, "stayed overloaded") || strings.Contains(msg, "came up within"):
		return "the gateway is saturated or the model can't start — check it with bsk / the ops console, or raise LCA_GW_MAX_WAIT"
	}
	return ""
}

// shortErr is the part of an error a person needs on screen: the root cause
// without the transport wrapping ("connection refused", "endpoint returned
// 503: model is paused"). The full text still goes to the trace and audit log.
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return truncate(ae.Error(), 200)
	}
	msg := err.Error()
	for _, cut := range []string{"connect: ", "dial tcp ", "read: ", "write: "} {
		if i := strings.LastIndex(msg, cut); i >= 0 {
			msg = msg[i+len(cut):]
		}
	}
	if i := strings.LastIndex(msg, "\": "); i >= 0 {
		msg = msg[i+3:]
	}
	return truncate(msg, 200)
}

// templateKwargNames are the chat_template_kwargs keys lca itself sends (the
// Switch fields of every card in models.go). They are listed so a refusal that
// names one can be told from a refusal about the engine's own API: the remedy
// differs, and guessing wrong sends the operator to the wrong file.
var templateKwargNames = map[string]bool{
	"enable_thinking": true, "thinking": true, "thinking_mode": true,
	"clear_thinking": true, "preserve_thinking": true, "reasoning_effort": true,
}

// ourField returns the name of a field lca put in the body that the server's
// message names, or "" when the message is about something else. Matched
// against a fixed list of OUR OWN field names rather than parsed out of the
// message, because every engine words this differently ("unknown field",
// "extra_forbidden", "unrecognized keys") and a parser would be a guess about
// each one's phrasing.
func ourField(msg string) string {
	for _, f := range []string{
		"chat_template_kwargs", "reasoning_effort", "enable_thinking", "thinking_mode",
		"clear_thinking", "preserve_thinking", "continue_final_message",
		"add_generation_prompt", "reasoning_content", "separate_reasoning",
	} {
		if strings.Contains(msg, f) {
			return f
		}
	}
	return ""
}
