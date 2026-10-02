package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// hy3's own card says lca must always NAME a level, because the two chat
// template variants disagree about what "no level" means — high on one, no
// thinking at all on the other, both served under the same parser name. Asking
// for a level hy3 does not document used to send no level at all, which is
// exactly the request shape that lands in that ambiguity.
func TestAnUnknownLevelOnHy3FoldsInsteadOfVanishing(t *testing.T) {
	prof := lookupProfile("hy3")
	if !prof.EffortAlways {
		t.Fatal("hy3 is the model this policy exists for")
	}
	for _, level := range []string{"max", "xhigh", "medium", "minimal"} {
		if got := prof.effortForLocal(level); got != "high" {
			t.Fatalf("effort %q on hy3 → %q, want high (the deeper level its template accepts)", level, got)
		}
	}
	// What it documents is still passed through untouched.
	for _, level := range []string{"no_think", "low", "high"} {
		if got := prof.effortForLocal(level); got != level {
			t.Fatalf("effort %q on hy3 → %q, want it unchanged", level, got)
		}
	}
	// And the fold reaches the wire: a named level must arrive as a level.
	fields := thinkingParams(&Provider{Local: true}, "hy3", prof, "max", "all")
	kw, _ := fields["chat_template_kwargs"].(map[string]any)
	if kw["reasoning_effort"] != "high" {
		t.Fatalf("the request must carry a level hy3's template accepts, got %#v", fields)
	}
}

// A model whose policy does NOT say "always name a level" keeps the old
// behaviour: an unknown level is dropped rather than replaced with our own.
func TestAnUnknownLevelIsStillDroppedElsewhere(t *testing.T) {
	prof := lookupProfile("glm5.3")
	if prof.EffortAlways {
		t.Fatal("glm-5.3 does not have hy3's template ambiguity, so it must not fold")
	}
	if got := prof.effortForLocal("ultra"); got != "" {
		t.Fatalf("an undocumented level on glm-5.3 → %q, want it dropped", got)
	}
}

// A deployment served without the chat template the card describes rejects
// every kwarg lca sends, so the model 400s on every turn and the operator sees
// "it cannot print". The escape hatch drops the template's bag and keeps the
// engine's own API fields.
func TestTemplateKwargsCanBeTurnedOffPerModel(t *testing.T) {
	c := NewClient(Config{})
	c.provider = &Provider{Local: true, ID: "local"}
	c.model = "hy3"

	body, err := c.body(ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, Thinking: "high"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "chat_template_kwargs") {
		t.Fatalf("hy3 steers its template through kwargs, so they belong in the body by default:\n%s", body)
	}

	c.noKwargs = true
	body, err = c.body(ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, Thinking: "high"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["chat_template_kwargs"]; ok {
		t.Fatalf("template_kwargs: off must drop the template's bag:\n%s", body)
	}
	for _, must := range []string{"model", "messages"} {
		if _, ok := got[must]; !ok {
			t.Fatalf("%s went missing with the kwargs:\n%s", must, body)
		}
	}
}

// The one error an operator cannot act on from the message alone: the engine
// says "unknown field enable_thinking" and nothing on screen connects that to a
// chat template lca is steering. It fires every turn and reads as "the model
// cannot print".
func TestARejectedFieldOfOursNamesTheRemedy(t *testing.T) {
	for _, body := range []string{
		`{"object":"error","message":"Unknown field: enable_thinking","code":400}`,
		`extra_forbidden: chat_template_kwargs`,
		`unrecognized keys: ["clear_thinking"]`,
	} {
		h := errorHint(&APIError{Status: 400, Body: body})
		// One remedy, and it is the one that works: for every model whose switch is
		// a kwarg, the OFF position is a kwarg too, so `/think off` would send the
		// same bag the server just refused.
		if !strings.Contains(h, "template_kwargs: off") {
			t.Fatalf("a refusal of our own template field must name the remedy, got %q for %s", h, body)
		}
		if strings.Contains(h, "/think off") {
			t.Fatalf("`/think off` is not a remedy here and must not be offered: %q", h)
		}
	}
	// A 400 about something else keeps whatever hint it had.
	if h := errorHint(&APIError{Status: 400, Body: "no model named x"}); strings.Contains(h, "template_kwargs") {
		t.Fatalf("an unrelated 400 must not be read as a rejected template field: %q", h)
	}
}
