package main

import (
	"regexp"
	"strings"
)

// Context management. The repo is never dumped into the prompt — the model pulls
// what it needs via read_file/grep — but the resulting tool_result messages
// still accumulate and, resent on every call, blow up the prefill. We keep the
// FULL transcript on disk for audit and send the model a trimmed copy. Two
// passes, cheapest-first:
//
//  1. Dedupe superseded reads: if a file was read again later, the earlier copy
//     is pure waste (the newer one is authoritative), so collapse it — always,
//     regardless of budget. This is the big win in agentic loops that re-read a
//     file after editing it.
//  2. Budget collapse: if still over budget, collapse the oldest remaining tool
//     outputs to a stub, since the model has already extracted what it needed —
//     while the user's instructions and the assistant's reasoning stay intact.

const (
	trimStub       = "[earlier tool output trimmed to save context]"
	supersededStub = "[earlier read of this file superseded by a later one]"
)

var reToolHdr = regexp.MustCompile(`^<tool_result name="([^"]*)" path="([^"]*)">`)

// estimateTokens is a tokenizer-free approximation (~4 chars/token). Good enough
// to drive trimming without pulling in a model-specific tokenizer on an
// air-gapped host.
func estimateTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		n := len(m.Content) + len(m.Role) + len(m.Reasoning) + 4
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments) + 16
		}
		total += n / 4
	}
	return total
}

// trimForContext returns a copy of msgs whose estimated size fits within
// budgetTokens (best effort), plus how many messages were collapsed.
//
// Cache alignment: while we're comfortably under budget we DON'T rewrite
// anything — the request is byte-identical to the growing prefix of the previous
// one, so the server's KV prefix cache hits and prefill is near-free. We only
// start compressing (which necessarily changes the prefix, a one-time cache
// bust) once the transcript would blow the budget. On a local GPU where tokens
// are "free" but prefill time is not, warm cache beats a smaller prompt.
func trimForContext(msgs []Message, budgetTokens int) ([]Message, int) {
	if budgetTokens <= 0 || estimateTokens(msgs) <= budgetTokens {
		return msgs, 0 // under budget → send the byte-identical prefix, keep cache warm
	}

	out := make([]Message, len(msgs))
	copy(out, msgs)

	// Over budget: first drop reads made redundant by a later read of the same
	// file, then, if still over, collapse the oldest remaining tool outputs.
	trimmed := dedupeReads(out)
	const keepRecent = 6
	limit := len(out) - keepRecent
	for i := 1; i < limit && estimateTokens(out) > budgetTokens; i++ {
		if isToolResult(out[i]) && !isStub(out[i].Content) {
			out[i].Content = trimStub
			trimmed++
		}
	}
	if trimmed == 0 {
		return msgs, 0
	}
	return out, trimmed
}

// dedupeReads collapses every read_file result for a path that is read again
// later in the transcript, keeping only the most recent read of each file.
// Mutates out in place; returns how many it collapsed.
func dedupeReads(out []Message) int {
	seen := map[string]bool{}
	trimmed := 0
	for i := len(out) - 1; i >= 1; i-- {
		name, path, ok := toolResultKey(out[i])
		if !ok || name != "read_file" || path == "" || isStub(out[i].Content) {
			continue
		}
		if seen[path] {
			out[i].Content = supersededStub
			trimmed++
		} else {
			seen[path] = true
		}
	}
	return trimmed
}

func toolResultKey(m Message) (name, path string, ok bool) {
	if m.Role == "tool" {
		return m.Tool, m.Path, true
	}
	if m.Role != "user" {
		return "", "", false
	}
	mt := reToolHdr.FindStringSubmatch(m.Content)
	if mt == nil {
		return "", "", false
	}
	return mt[1], mt[2], true
}

func isStub(s string) bool { return s == trimStub || s == supersededStub }

func isToolResult(m Message) bool {
	return m.Role == "tool" || m.Role == "user" && strings.HasPrefix(m.Content, "<tool_result")
}
