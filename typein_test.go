package main

import (
	"strings"
	"testing"
)

// A finished line typed while the agent works is a message for the agent, not
// type-ahead for the next prompt. Pressing Enter is the difference: half a
// sentence is not a message, so an unfinished line stays pending.
func TestOnlyWholeLinesLeaveTheKeyboardMidTurn(t *testing.T) {
	for _, tc := range []struct {
		in    string
		lines []string
		rest  string
	}{
		{"", nil, ""},
		{"half a sentence", nil, "half a sentence"},
		{"stop, look at f.txt\r", []string{"stop, look at f.txt"}, ""},
		{"one\rtwo\rthree", []string{"one", "two"}, "three"},
		{"\r\r\r", nil, ""},                      // bare Enters say nothing
		{"a\nb\r\n", []string{"a", "b"}, ""},     // CR, LF and CRLF all end a line
		{"  \r keep \r", []string{" keep "}, ""}, // blank lines dropped, spacing kept
		{"стой, смотри f.txt\r", []string{"стой, смотри f.txt"}, ""}, // bytes, not runes
	} {
		lines, rest := cutLines([]byte(tc.in))
		if strings.Join(lines, "|") != strings.Join(tc.lines, "|") || string(rest) != tc.rest {
			t.Errorf("cutLines(%q) = %q, %q; want %q, %q", tc.in, lines, rest, tc.lines, tc.rest)
		}
	}
}

// The message is delivered at a step boundary and labelled, because it arrives
// in the middle of work and is usually a correction — and a request already on
// the wire cannot be amended, so this is the earliest honest moment.
func TestATypedMessageReachesTheNextStep(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", false)
	s := h.sess

	if s.pendingInbox() {
		t.Fatal("nothing has been said yet")
	}
	s.Say("stop, look at f.txt only")
	if !s.pendingInbox() {
		t.Fatal("a typed message must hold the turn open: it would otherwise be read as dropped")
	}

	before := len(s.Msgs)
	if !s.drainTyped() {
		t.Fatal("the queued message was not delivered")
	}
	if len(s.Msgs) != before+1 {
		t.Fatalf("one message, got %d new", len(s.Msgs)-before)
	}
	m := s.Msgs[len(s.Msgs)-1]
	if m.Role != "user" || !strings.Contains(m.Content, "stop, look at f.txt only") {
		t.Fatalf("the operator's words must arrive verbatim: %+v", m)
	}
	if !strings.Contains(m.Content, "while you were working") {
		t.Fatalf("it has to be told apart from the task it was given: %q", m.Content)
	}
	// Drained once, not twice: a correction repeated on every later step would
	// read as the operator insisting.
	if s.drainTyped() || s.pendingInbox() {
		t.Fatal("the queue must be empty after a drain")
	}
}

// A slash command belongs to the REPL, which is not running during a turn.
// Sending "/compact" to the model as prose would be worse than waiting, so the
// hook refuses it — and a refusal HOLDS the line for the prompt instead of
// losing what was typed.
func TestARefusedLineComesBackToThePrompt(t *testing.T) {
	in := newStringInput("")
	var seen []string
	in.SetLineHook(func(line string) bool {
		seen = append(seen, line)
		return !strings.HasPrefix(line, "/")
	})

	// Simulates one Drain: what the capture goroutine does with the bytes.
	in.mu.Lock()
	in.pending = append(in.pending, []byte("a message\r/compact\rtail")...)
	lines, rest := cutLines(in.pending)
	in.pending = rest
	in.mu.Unlock()
	for _, l := range lines {
		if !in.lineHook(l) {
			in.mu.Lock()
			in.held = append(append(in.held, l...), '\r')
			in.mu.Unlock()
		}
	}

	if strings.Join(seen, "|") != "a message|/compact" {
		t.Fatalf("both lines reach the hook, in order: %q", seen)
	}
	if string(in.pending) != "tail" {
		t.Fatalf("the unfinished line stays pending, got %q", in.pending)
	}
	if string(in.held) != "/compact\r" {
		t.Fatalf("the refused command must be held for the prompt, got %q", in.held)
	}
}
