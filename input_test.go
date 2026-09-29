package main

import (
	"io"
	"strings"
	"testing"
)

func TestInputBufferOrder(t *testing.T) {
	in := newStringInput("bc")
	in.Put("a")
	var got []byte
	for {
		b, err := in.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, b)
	}
	if string(got) != "abc" {
		t.Fatalf("read %q, want abc (returned input comes first)", got)
	}
	in2 := newStringInput("line one\nline two\n")
	if s, err := in2.ReadString('\n'); err != nil || s != "line one\n" {
		t.Fatalf("ReadString = %q, %v", s, err)
	}
	if rest := in2.TakePending(); rest != "line two\n" {
		t.Fatalf("pending = %q", rest)
	}
	if rest := in2.TakePending(); rest != "" {
		t.Fatal("TakePending must consume")
	}
}

// A paste of many questions must become ONE message, not a line-per-turn
// stampede, and nothing may be dropped.
func TestPasteStagedAsOneMessage(t *testing.T) {
	e := NewLineEditor(newStringInput(""))
	var lines []string
	for i := 1; i <= 24; i++ {
		lines = append(lines, "line "+strings.Repeat("x", i))
	}
	paste := "\x1b[200~" + strings.Join(lines, "\r\n") + "\r\n\x1b[201~"
	buf, pos := e.stage(paste, nil, 0)
	if len(buf) != 0 || pos != 0 {
		t.Fatal("a multi-line paste belongs in the staging area, not the input line")
	}
	if !strings.Contains(pasteSummary(e.staged), "24 lines") {
		t.Fatalf("summary: %q", pasteSummary(e.staged))
	}
	msg := e.take(nil)
	if strings.Contains(msg, "\x1b[") || strings.Contains(msg, "\r") {
		t.Fatal("paste markers and CRLF must be stripped")
	}
	for _, l := range lines {
		if !strings.Contains(msg, l) {
			t.Fatalf("lost %q", l)
		}
	}
	if e.staged != "" {
		t.Fatal("sending clears the staging area")
	}

	// a short single-line paste is just typed text
	e2 := NewLineEditor(newStringInput(""))
	buf2, pos2 := e2.stage("fix the parser", nil, 0)
	if string(buf2) != "fix the parser" || pos2 != len(buf2) || e2.staged != "" {
		t.Fatalf("short paste: %q staged %q", string(buf2), e2.staged)
	}

	// typing after a staged paste appends to it
	e3 := NewLineEditor(newStringInput(""))
	e3.stage("a\nb", nil, 0)
	if got := e3.take([]rune("and also this")); got != "a\nb\nand also this" {
		t.Fatalf("combined message: %q", got)
	}
}

// Anything typed before an approval question appeared is not an answer to it,
// and must survive for the next prompt.
func TestApprovalIgnoresTypeAhead(t *testing.T) {
	in := newStringInput("y\nand my next question\n")
	ap := NewApprover(in)
	ok, _ := ap.Confirm("edit", "EDIT x.go", "")
	if ok {
		t.Fatal("type-ahead must not approve an action")
	}
	if got := in.TakePending(); got != "y\nand my next question\n" {
		t.Fatalf("the buffered input was eaten: %q", got)
	}
}

// The wizard's fields used to be the REPL's own editor, so the gateway url and —
// on the plaintext path — the api key landed in the ↑ history of the prompt that
// opens right afterwards, one keystroke from being sent to the model as a message.
// The fences and the status line belong to the REPL prompt too, not under a
// one-field question inside a 76-column section.
//
// render, submit and remember are exercised directly: ReadLine's drawing lives on
// the raw path, and makeRaw needs a real terminal, which `go test` has not got.
func TestLineEditorFieldModeForgetsAndStaysBare(t *testing.T) {
	field := NewLineEditor(newStringInput(""))
	field.bare, field.noHistory, field.secret = true, true, true
	field.status = func() string { return "THE STATUS LINE" }
	const secret = "sk-a-real-looking-key"

	field.remember(secret)
	if len(field.history) != 0 {
		t.Errorf("a forgetful field must not be remembered: %v", field.history)
	}
	out := captureStdout(t, func() { field.render("  › api key ", []rune(secret), len(secret)) })
	if strings.Contains(out, secret) {
		t.Errorf("a secret field echoed the key while it was typed:\n%q", out)
	}
	if !strings.Contains(out, "•") {
		t.Errorf("a secret field must show its width:\n%q", out)
	}
	if strings.Contains(out, "THE STATUS LINE") {
		t.Errorf("a bare field must not draw the REPL status line:\n%q", out)
	}
	out = captureStdout(t, func() { field.submit("  › api key ", []rune(secret)) })
	if strings.Contains(out, secret) {
		t.Errorf("a secret field echoed the key on submit:\n%q", out)
	}
	if strings.Contains(out, "───") {
		t.Errorf("a bare field must not draw a full-width fence:\n%q", out)
	}
	if got := field.suggest("/mo"); got != nil {
		t.Errorf("a bare field completed a command: %v", got)
	}

	// The ordinary prompt is unchanged: it remembers, it completes commands, and it
	// draws its band and hairline.
	plain := NewLineEditor(newStringInput(""))
	plain.remember("hello")
	plain.remember("hello") // the same line twice is one entry, as before
	if len(plain.history) != 1 || plain.history[0] != "hello" {
		t.Errorf("the REPL prompt must still keep its history: %v", plain.history)
	}
	if got := plain.suggest("/mo"); len(got) == 0 {
		t.Error("the REPL prompt must still complete commands")
	}
	out = captureStdout(t, func() { plain.submit(" › ", []rune("hello")) })
	if !strings.Contains(out, "hello") || !strings.Contains(out, "───") {
		t.Errorf("the REPL prompt must still draw its band and hairline:\n%q", out)
	}
}

// askSecret is the only reader the api-key field may use.
func TestAskSecretDoesNotEcho(t *testing.T) {
	in := scriptedTTY("sk-do-not-show-me\n")
	var got string
	out := captureStdout(t, func() {
		var err error
		got, err = askSecret(in, "api key ")
		if err != nil {
			t.Fatal(err)
		}
	})
	if got != "sk-do-not-show-me" {
		t.Fatalf("askSecret read %q", got)
	}
	if strings.Contains(out, "sk-do-not-show-me") {
		t.Errorf("askSecret echoed the key:\n%q", out)
	}
}
