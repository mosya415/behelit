package main

import (
	"errors"
	"net"
	"strings"
)

// errorHint turns a failure into the next thing to try — the difference
// between an error message and a usable one.
func errorHint(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 401 || ae.Status == 403:
			return "the endpoint rejected the credentials — check the API key (LCA_API_KEY or the provider's key variable)"
		case ae.Status == 404 && strings.Contains(msg, "model"):
			return "that model isn't served here — lca doctor lists what the gateway serves; fix the name in roles.yaml"
		case ae.Status == 404:
			return "wrong URL? LCA_BASE_URL must include the API base path, e.g. http://node:18080/v1"
		case isContextOverflow(err):
			return "the conversation no longer fits — /compact summarizes it, /reset starts over"
		case ae.Status == 400 && (strings.Contains(msg, "tool") || strings.Contains(msg, "auto")):
			return "the engine refused tool calling — enable its tool-call parser, or set transport: text for this model (lca doctor checks)"
		}
		return ""
	}
	var ne net.Error
	switch {
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host"):
		return "nothing answers at LCA_BASE_URL — is the gateway up? lca doctor checks the setup"
	case errors.As(err, &ne) && ne.Timeout():
		return "the endpoint timed out — a cold model may still be starting; try again, or lca doctor"
	case strings.Contains(msg, "no api key"):
		return "export the provider's key, or use a gateway via LCA_BASE_URL"
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
