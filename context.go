package main

import "strings"

// Context management. The repo is never dumped into the prompt — the model pulls
// what it needs via read_file/grep — but the resulting tool_result messages
// still accumulate and, resent on every call, blow up the prefill. We keep the
// FULL transcript on disk for audit and send the model a trimmed copy: the
// oldest tool outputs are collapsed to a stub first, since the model has already
// extracted what it needed from them, while the user's instructions and the
// assistant's own reasoning/plan are preserved intact.

const trimStub = "[earlier tool output trimmed to save context]"

// estimateTokens is a tokenizer-free approximation (~4 chars/token). Good enough
// to drive trimming without pulling in a model-specific tokenizer on an
// air-gapped host.
func estimateTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += (len(m.Content) + len(m.Role) + 4) / 4
	}
	return total
}

// trimForContext returns a copy of msgs whose estimated size fits within
// budgetTokens (best effort), plus how many messages were collapsed. It only
// ever collapses synthetic tool_result messages, oldest first, and never touches
// the system prompt or the most recent `keepRecent` messages.
func trimForContext(msgs []Message, budgetTokens int) ([]Message, int) {
	if budgetTokens <= 0 || estimateTokens(msgs) <= budgetTokens {
		return msgs, 0
	}

	out := make([]Message, len(msgs))
	copy(out, msgs)

	const keepRecent = 6
	limit := len(out) - keepRecent
	trimmed := 0
	for i := 1; i < limit && estimateTokens(out) > budgetTokens; i++ {
		if isToolResult(out[i]) && out[i].Content != trimStub {
			out[i].Content = trimStub
			trimmed++
		}
	}
	return out, trimmed
}

func isToolResult(m Message) bool {
	return m.Role == "user" && strings.HasPrefix(m.Content, "<tool_result")
}
