package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Compaction (ported from opencode's session/compaction.ts): when a
// conversation outgrows the context window, the older part is replaced by an
// anchored, structured summary another agent could continue from, and the most
// recent user turn is kept verbatim if it fits. Runs on /compact, automatically
// when trimming old tool output isn't enough, and on a context-overflow error.

const compactedPrefix = "[Earlier conversation compacted to save context]\n\n"

const compactionSystem = `You are a context summarization agent. You produce a structured summary of a
coding-agent conversation so another agent can continue the work from the
summary alone. Keep every section, preserve exact file paths, identifiers,
commands and error messages. Do not continue the conversation and do not answer
questions in it. Write in the same language as the conversation.`

const summaryTemplate = `Use exactly this structure (write "(none)" for an empty section):

## Objective
What the user wants, in their terms, including constraints.

## Important Details
Decisions made and why, conventions discovered, facts that must not be lost.

## Work State
### Completed
### Active
### Blocked

## Next Move
1. The concrete next steps, in order.

## Relevant Files
- path — what it is / what changed

Terse bullets. If the conversation starts with an earlier summary, carry its
content forward and update it (the conversation wins on conflicts; move finished
items from Active to Completed). Do not mention that this is a summary.`

// Compact replaces the older transcript with a summary. auto=true adds a
// continue instruction when no recent turn is kept, so the loop resumes.
func (s *Session) Compact(ctx context.Context, auto bool) error {
	if len(s.Msgs) <= 2 {
		return fmt.Errorf("nothing to compact yet")
	}
	before := estimateTokens(s.Msgs)
	s.view.Note("compacting conversation…")

	// Keep the last real user turn verbatim if it's no more than a quarter of
	// the budget; everything before it is summarized.
	tail := len(s.Msgs)
	budget := s.budget()
	if idx := lastUserTurn(s.Msgs); idx > 1 && estimateTokens(s.Msgs[idx:]) <= budget/4 {
		tail = idx
	}
	head := s.Msgs[1:tail]
	if len(head) == 0 {
		return fmt.Errorf("nothing to compact yet")
	}

	prompt := "Here is the conversation so far:\n\n<conversation>\n" + serializeForSummary(head) +
		"</conversation>\n\nCreate an anchored summary of the conversation above so another coding agent can continue the work.\n\n" + summaryTemplate
	// The summary is written by the "cheap" role when roles.yaml defines one
	// (its own session id and model chain, same root), else by this session's
	// model. It is a separate request, so this conversation's cached prefix
	// (system prompt + tools) is untouched.
	cs := s.compactor()
	start := time.Now()
	res, fb, err := cs.chat(ctx, ChatRequest{Messages: []Message{
		{Role: "system", Content: compactionSystem},
		{Role: "user", Content: prompt},
	}, Thinking: cs.thinking()}, StreamSink{})
	cs.traceTurn(0, res, fb, start, nil, err)
	summary := strings.TrimSpace(reThinkBlock.ReplaceAllString(res.Content, ""))
	if err != nil {
		return err
	}
	if summary == "" {
		return fmt.Errorf("the model produced no summary")
	}

	msgs := []Message{
		s.Msgs[0],
		{Role: "user", Content: compactedPrefix + summary},
		{Role: "assistant", Content: "Understood — I have the summary and will continue from it.", Agent: s.agent.Name},
	}
	if tail < len(s.Msgs) {
		msgs = append(msgs, s.Msgs[tail:]...)
	} else if auto {
		msgs = append(msgs, Message{Role: "user", Content: "Continue with the next steps if there are any, or stop and ask for clarification if you are unsure how to proceed."})
	}
	s.Msgs = msgs
	after := estimateTokens(s.Msgs)
	s.stats.Compactions++
	s.event("compact", map[string]any{"auto": auto, "before_tokens": before, "after_tokens": after})
	s.saveTranscript()
	s.view.Note(fmt.Sprintf("COMPACT  ~%s → ~%s tokens", kfmt(before), kfmt(after)))
	return nil
}

// compactor is the session that writes summaries for s.
func (s *Session) compactor() *Session {
	cheap := s.orch.agents["cheap"]
	if cheap == nil || len(cheap.Models) == 0 {
		return s
	}
	cs := &Session{ID: s.ID + "-compact", UID: s.UID + "-compact", rootOverride: s.rootUID(), orch: s.orch, parent: s.parent,
		agent: cheap, view: s.view, member: s.member, models: append([]string(nil), cheap.Models...)}
	cs.useModel(0)
	return cs
}

// serializeForSummary renders messages as labeled text, clipping tool output.
func serializeForSummary(msgs []Message) string {
	const clip = 2000
	var b strings.Builder
	for _, m := range msgs {
		switch {
		case m.Role == "tool":
			fmt.Fprintf(&b, "[Tool result %s %s]: %s\n\n", m.Tool, m.Path, clipText(m.Content, clip))
		case m.Role == "user" && strings.HasPrefix(m.Content, "<tool_result"):
			fmt.Fprintf(&b, "[Tool results]: %s\n\n", clipText(m.Content, clip))
		case m.Role == "user":
			fmt.Fprintf(&b, "[User]: %s\n\n", m.Content)
		case m.Role == "assistant":
			if t := strings.TrimSpace(reThinkBlock.ReplaceAllString(m.Content, "")); t != "" {
				fmt.Fprintf(&b, "[Assistant]: %s\n\n", clipText(t, clip*2))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[Assistant tool call]: %s(%s)\n\n", tc.Function.Name, clipText(tc.Function.Arguments, 500))
			}
		}
	}
	return b.String()
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[truncated]"
}
