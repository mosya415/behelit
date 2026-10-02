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
