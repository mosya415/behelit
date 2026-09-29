package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// scriptedTTY is an Input that answers IsTTY but has no terminal behind it: the
// keystrokes come from the buffer, and there is no line discipline to switch
// off, so the picker takes its raw path exactly as it does on a real terminal.
func scriptedTTY(s string) *Input {
	in := newStringInput(s)
	in.tty = true
	return in
}

func rows(n int) []choice {
	var cs []choice
	for i := 0; i < n; i++ {
		cs = append(cs, choice{id: string(rune('a' + i%26)), label: "row" + strings.Repeat("x", i%3)})
	}
	return cs
}

func TestPickStateKeys(t *testing.T) {
	cs := []choice{{id: "a", label: "aa"}, {id: "b", label: "bb"}, {id: "c", label: "cc"}}

	p := newPickState(cs, pickOpts{})
	p.key("down")
	p.key("down")
	p.key("enter")
	if got := p.result(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("down down enter = %v, want [2]", got)
	}

	p = newPickState(cs, pickOpts{})
	p.key("up") // wraps to the end
	if p.cur != 2 {
		t.Fatalf("up from the top = %d, want 2 (it wraps)", p.cur)
	}
	p.key("down") // wraps back to the top
	if p.cur != 0 {
		t.Fatalf("down from the end = %d, want 0", p.cur)
	}

	p = newPickState(cs, pickOpts{})
	p.key("end")
	if p.cur != 2 {
		t.Fatalf("end = %d", p.cur)
	}
	p.key("home")
	if p.cur != 0 {
		t.Fatalf("home = %d", p.cur)
	}

	p = newPickState(cs, pickOpts{multi: true})
	p.key("space")
	p.key("down")
	p.key("down")
	p.key("space")
	p.key("enter")
	got := p.result()
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("two ticks = %v, want [0 2] in ROW order", got)
	}

	// A long list keeps the cursor inside the window and never scrolls past the end.
	long := newPickState(rows(40), pickOpts{height: 12})
	for i := 0; i < 39; i++ {
		long.key("down")
		if long.cur < long.top || long.cur >= long.top+long.height {
			t.Fatalf("cursor %d left the window [%d,%d)", long.cur, long.top, long.top+long.height)
		}
		if long.top > len(long.view)-long.height {
			t.Fatalf("top %d scrolled past the end", long.top)
		}
	}
	long.key("pgdn")
	long.key("pgup")
	if long.cur < 0 || long.cur >= 40 {
		t.Fatalf("pgdn/pgup left the list: %d", long.cur)
	}

	p = newPickState(cs, pickOpts{})
	p.key("cancel")
	if !p.canc || len(p.result()) != 0 {
		t.Fatalf("cancel: canc=%v result=%v", p.canc, p.result())
	}
}

// Filtering must never drop a tick: sel is keyed on the row index, so a row the
// filter hides stays chosen.
func TestPickStateFilterKeepsTicks(t *testing.T) {
	cs := []choice{{id: "kimi-k3", label: "kimi-k3"}, {id: "q480", label: "qwen3-coder-480b"}, {id: "glm", label: "glm-5.3"}}
	p := newPickState(cs, pickOpts{multi: true})
	p.key("space") // tick kimi-k3
	for _, r := range []string{"4", "8", "0"} {
		p.key(r)
	}
	if len(p.view) != 1 || p.rows[p.view[0]].id != "q480" {
		t.Fatalf("typing 480 narrowed to %d rows", len(p.view))
	}
	p.key("space") // tick the one match too
	p.key("enter")
	got := p.result()
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("result after filtering = %v, want [0 1] — a filtered-out tick was dropped", got)
	}
}

func TestPickLoopDecodesArrowsAndCancel(t *testing.T) {
	cs := []choice{{id: "a", label: "aa"}, {id: "b", label: "bb"}, {id: "c", label: "cc"}}

	for _, script := range []string{"\x1b[B\x1b[B \r", "\x1bOB\x1bOB \r"} {
		p := newPickState(cs, pickOpts{multi: true})
		if err := pickLoop(scriptedTTY(script), p, nil); err != nil {
			t.Fatalf("%q: %v", script, err)
		}
		got := p.result()
		if len(got) != 1 || got[0] != 2 {
			t.Fatalf("%q selected %v, want [2]", script, got)
		}
	}

	for _, script := range []string{"\x03", "", "\x1b\x1b"} {
		p := newPickState(cs, pickOpts{})
		if err := pickLoop(scriptedTTY(script), p, nil); !errors.Is(err, errPickCancel) {
			t.Fatalf("%q = %v, want errPickCancel", script, err)
		}
	}

	p := newPickState(cs, pickOpts{multi: true})
	if err := pickLoop(scriptedTTY("\x01\r"), p, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.result()) != 3 {
		t.Fatalf("^a ticked %d of 3", len(p.result()))
	}
	p = newPickState([]choice{{id: "a", label: "aa", on: true}, {id: "b", label: "bb", on: true}}, pickOpts{multi: true})
	if err := pickLoop(scriptedTTY("\x0e\r"), p, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.result()) != 0 {
		t.Fatalf("^n left %d ticked", len(p.result()))
	}
}

// A refused picker must not eat the command that follows it.
func TestPickRefusesWithoutTTY(t *testing.T) {
	in := newStringInput("\r")
	out := captureStdout(t, func() {
		if _, err := pick(in, rows(3), pickOpts{title: "models"}); !errors.Is(err, errNoTTY) {
			t.Fatalf("err = %v, want errNoTTY", err)
		}
	})
	if out != "" {
		t.Fatalf("a refused picker printed %q", out)
	}
	if got := in.TakePending(); got != "\r" {
		t.Fatalf("the input was read: %q", got)
	}
}

// stdin IS a terminal but raw mode is refused (Windows, or a terminal that will
// not have it): the picker degrades to a numbered cooked read instead of hanging.
func TestPickCookedFallback(t *testing.T) {
	mk := func(script string) *Input {
		rd, wr, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rd.Close() })
		go func() { wr.WriteString(script); wr.Close() }()
		in := NewInput(rd)
		in.tty = true // claims a terminal; makeRaw on a pipe still fails
		return in
	}
	cs := rows(3)
	captureStdout(t, func() {
		got, err := pick(mk("2\n"), cs, pickOpts{})
		if err != nil || len(got) != 1 || got[0] != 1 {
			t.Errorf("cooked pick = %v, %v; want [1]", got, err)
		}
	})
	captureStdout(t, func() {
		if _, err := pick(mk("nope\n"), cs, pickOpts{}); !errors.Is(err, errPickCancel) {
			t.Errorf("a bad answer then EOF = %v, want errPickCancel", err)
		}
	})
}

func TestPickStateLinesCarryProvenance(t *testing.T) {
	served := modelChoice(ModelInfo{ID: "qwen3-coder-480b-a35b-instruct", MaxLen: 262144}, false)
	card := modelChoice(ModelInfo{ID: "kimi-k3"}, false)
	unknown := modelChoice(ModelInfo{ID: "totally-made-up-9000"}, false)

	p := newPickState([]choice{served, card, unknown}, pickOpts{})
	all := strings.Join(p.lines(), "\n")
	if !strings.Contains(all, "(server)") {
		t.Errorf("a window from /v1/models must be labelled (server):\n%s", all)
	}
	// The unknown row's sentence is the one /model already prints, verbatim.
	p.key("down")
	p.key("down")
	all = strings.Join(p.lines(), "\n")
	want := `no profile for "totally-made-up-9000" (normalised "totally-made-up-9000") — nothing is overridden`
	// Compared with the line breaks collapsed: a long caveat wraps, and where it
	// wraps is the terminal's business, not the sentence's.
	flat := strings.Join(strings.Fields(stripANSI(all)), " ")
	if !strings.Contains(flat, want) {
		t.Errorf("the unknown-model sentence is missing:\n%s", stripANSI(all))
	}
	if lookupProfile("kimi-k3").Family != "" && !strings.Contains(stripANSI(strings.Join(newPickState([]choice{card}, pickOpts{}).lines(), "\n")), "(card)") {
		t.Errorf("a window from the table must be labelled (card)")
	}
}

func TestReadEscapeSharedByEditorAndPicker(t *testing.T) {
	cases := map[string]string{
		"[A":    "A",
		"OA":    "A",
		"[3~":   "3~",
		"[5~":   "5~",
		"[200~": "200~",
		"[Z":    "Z",
		"x":     "",
		"\x1b":  "esc",
	}
	for seq, want := range cases {
		if got := readEscape(newStringInput(seq)); got != want {
			t.Errorf("readEscape(%q) = %q, want %q", seq, got, want)
		}
	}
	// The editor's own behaviour is unchanged by the extraction: history through
	// ESC [ A still recalls the previous line.
	e := NewLineEditor(newStringInput("[A"))
	e.history = []string{"first", "second"}
	buf, pos, hist := e.escape([]rune(""), 0, len(e.history))
	if string(buf) != "second" || pos != 6 || hist != 1 {
		t.Fatalf("ESC [ A after the extraction gave %q pos=%d hist=%d", string(buf), pos, hist)
	}
}

// -y must never answer a wizard question: it writes files.
func TestConfirmIgnoresApprover(t *testing.T) {
	in := scriptedTTY("n\n")
	ap := NewApprover(in)
	ap.TrustAll()
	var got bool
	captureStdout(t, func() {
		var err error
		got, err = confirm(in, "write these files?", false)
		if err != nil {
			t.Error(err)
		}
	})
	if got {
		t.Fatal("confirm went through the approval gate — -y would silently write roles.yaml")
	}
	if !ap.Trusts("edit") {
		t.Fatal("the approver's own state must be untouched")
	}
}

// Ctrl-C at a wizard yes/no used to kill lca with SIGINT: confirm read cooked,
// so ISIG was on and the `case "\x03"` below the read was unreachable. The
// decision now comes from confirmKey, which has no terminal in it — makeRaw needs
// a real tty, which `go test` has not got.
func TestConfirmKeyCancelsInsteadOfKilling(t *testing.T) {
	for _, tc := range []struct {
		b                    byte
		def                  bool
		ans, decided, cancel bool
	}{
		{'y', false, true, true, false},
		{'Y', false, true, true, false},
		{'n', true, false, true, false},
		{13, true, true, true, false},   // Enter takes the default
		{10, false, false, true, false}, // …either spelling of it
		{3, true, false, true, true},    // Ctrl-C aborts the wizard
		{4, true, false, true, true},    // Ctrl-D too
		{'q', true, false, false, false},
	} {
		ans, decided, cancel := confirmKey(tc.b, tc.def)
		if ans != tc.ans || decided != tc.decided || cancel != tc.cancel {
			t.Errorf("confirmKey(%d, def=%v) = %v %v %v, want %v %v %v",
				tc.b, tc.def, ans, decided, cancel, tc.ans, tc.decided, tc.cancel)
		}
	}
	// The reader itself, which is what the wizard runs on a real terminal: Ctrl-C
	// comes back as errPickCancel, which cmdSetup turns into "setup aborted".
	captureStdout(t, func() {
		if _, err := confirmRaw(scriptedTTY("\x03"), true); !errors.Is(err, errPickCancel) {
			t.Errorf("Ctrl-C at a wizard question must abort it, got %v", err)
		}
		if got, err := confirmRaw(scriptedTTY("\r"), true); err != nil || !got {
			t.Errorf("Enter must take the default: %v %v", got, err)
		}
		if got, err := confirmRaw(scriptedTTY("qn"), true); err != nil || got {
			t.Errorf("an unknown key must be ignored and the next one read: %v %v", got, err)
		}
		// ESC ESC is the pickers' way out and means the same here.
		if _, err := confirmRaw(scriptedTTY("\x1b\x1b"), true); !errors.Is(err, errPickCancel) {
			t.Errorf("ESC ESC must abort: %v", err)
		}
	})
}

// A multi picker seeded from an ordered list gives that order back. Row order is
// the gateway's /v1/models order, so merely opening /role reviewer model and
// pressing Enter used to rewrite the chain — and put the coder's own family first.
func TestPickKeepsSeededChainOrder(t *testing.T) {
	// Rows in gateway order; the chain is the other way round.
	cs := []choice{
		{id: "qwen", label: "qwen", on: true, seq: 2},
		{id: "glm", label: "glm", on: true, seq: 1},
		{id: "kimi", label: "kimi"},
	}
	p := newPickState(cs, pickOpts{multi: true})
	p.key("enter")
	var got []string
	for _, i := range p.result() {
		got = append(got, cs[i].id)
	}
	if strings.Join(got, ",") != "glm,qwen" {
		t.Fatalf("a bare Enter reordered the seeded chain: %v", got)
	}
	// A row ticked now is appended after the seeded ones, in row order.
	p = newPickState(cs, pickOpts{multi: true})
	p.key("end")
	p.key("space")
	p.key("enter")
	got = nil
	for _, i := range p.result() {
		got = append(got, cs[i].id)
	}
	if strings.Join(got, ",") != "glm,qwen,kimi" {
		t.Fatalf("a newly ticked row must come after the seeded chain: %v", got)
	}
	// With nothing seeded the order is row order, exactly as before.
	plain := []choice{{id: "a", label: "a", on: true}, {id: "b", label: "b", on: true}}
	p = newPickState(plain, pickOpts{multi: true})
	p.key("enter")
	if got := p.result(); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("unseeded ticks must stay in row order: %v", got)
	}
}

// The legend is the only place ^a, ^n and "type to filter" are documented, and at
// 80 columns it was ellipsized to "^a a…" — on exactly the screens that need it.
func TestPickLegendWrapsInsteadOfBeingCut(t *testing.T) {
	p := newPickState(rows(4), pickOpts{multi: true})
	var legend []string
	for _, l := range p.lines() {
		if s := stripANSI(l); strings.Contains(s, "move") || strings.Contains(s, "filter") || strings.Contains(s, "aborts") {
			legend = append(legend, strings.TrimSpace(s))
		}
	}
	joined := strings.Join(legend, " ")
	for _, want := range []string{"^a all", "^n none", "type to filter", "ctrl-c aborts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the legend must survive whole; %q is missing from %q", want, joined)
		}
	}
}
