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
		total += (len(m.Content) + len(m.Role) + 4) / 4
	}
	return total
}

// trimForContext returns a copy of msgs whose estimated size fits within
// budgetTokens (best effort), plus how many messages were collapsed. It only
// ever collapses synthetic tool_result messages and never touches the system
// prompt or the most recent `keepRecent` messages.
func trimForContext(msgs []Message, budgetTokens int) ([]Message, int) {
	out := make([]Message, len(msgs))
	copy(out, msgs)

	trimmed := dedupeReads(out)

	if budgetTokens > 0 && estimateTokens(out) > budgetTokens {
		const keepRecent = 6
		limit := len(out) - keepRecent
		for i := 1; i < limit && estimateTokens(out) > budgetTokens; i++ {
			if isToolResult(out[i]) && !isStub(out[i].Content) {
				out[i].Content = trimStub
				trimmed++
			}
		}
	}
	if trimmed == 0 {
		return msgs, 0 // nothing changed — hand back the original slice
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
	return m.Role == "user" && strings.HasPrefix(m.Content, "<tool_result")
}
