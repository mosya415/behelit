package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// A small raw-mode line editor: cursor movement, history (↑/↓), the usual
// control keys, and a live command menu when the line starts with "/". It falls
// back to a plain cooked read when stdin is not a terminal (pipes, non-unix), so
// scripted/one-shot use is unaffected.

var errLineCancel = errors.New("line cancelled") // Ctrl-C on a line

type cmdInfo struct{ name, desc string }

var replCommands = []cmdInfo{
	{"/help", "show all commands"},
	{"/discover", "find live models on the cluster"},
	{"/endpoint", "list / switch endpoints"},
	{"/model", "show / set the model"},
	{"/approve", "approval mode (on|off|run|edit)"},
	{"/think", "show / hide model reasoning"},
	{"/loop", "autonomous mode: run until done"},
	{"/unsafe", "disable jail + allowlist (danger)"},
	{"/context", "context size, cache, big outputs"},
	{"/compact", "summarize to reclaim context"},
	{"/resume", "continue a previous session"},
	{"/retry", "regenerate the last turn"},
	{"/edit", "amend & resend the last message"},
	{"/diff", "review files the agent changed"},
	{"/undo", "revert the agent's last change"},
	{"/reset", "clear the transcript"},
	{"/exit", "quit"},
}

type LineEditor struct {
	in      *Input
	staged  string // a pasted block waiting to be sent (shown as a summary)
	history []string
	models  func() []string       // known model names, for /model completion
	files   func(string) []string // jail files matching a fragment, for @-completion
	status  func() string         // one-line status shown under the input (model · mode · dir)

	// The three switches a one-field prompt inside a section needs. The wizard's
	// fields are not the REPL's prompt: a full-width fence straddles a 76-column
	// section, the status line has no business under "gateway url", a "/" menu is
	// noise there, and nothing typed at a wizard field — least of all an api key —
	// belongs in the ↑ history of the prompt that opens afterwards.
	noHistory bool // never append to history
	bare      bool // no fences, no status line, no completion menu
	secret    bool // render • per rune (implies noHistory, set together)
}

func NewLineEditor(in *Input) *LineEditor { return &LineEditor{in: in} }

// remember puts a submitted line in the ↑ history — unless this editor is a
// forgetful field. A wizard field is one: the gateway url coming back at the next
// prompt is untidy, and the api key coming back there is the secret one ↑ and one
// Enter from being sent to the model as a message.
func (e *LineEditor) remember(line string) {
	s := strings.TrimSpace(line)
	if s == "" || e.noHistory {
		return
	}
	if len(e.history) == 0 || e.history[len(e.history)-1] != s {
		e.history = append(e.history, s)
	}
}

// display is the buffer as drawn. A secret field shows its own width and nothing
// else: the raw path renders on every keystroke, so without this the key is on
// screen in clear while it is typed.
func (e *LineEditor) display(buf []rune) string {
	if e.secret {
		return strings.Repeat(gMask, len(buf))
	}
	return displayRunes(buf)
}

// stage decides what to do with text that arrived at once (a paste, or what was
// typed while the agent was working): a short single line is put in the input as
// if typed; anything longer is held as one message, shown as a summary, and sent
// when you press Enter.
func (e *LineEditor) stage(text string, buf []rune, pos int) ([]rune, int) {
	text = cleanPaste(text)
	if text == "" {
		return buf, pos
	}
	if !strings.Contains(text, "\n") && len(text) < 200 && e.staged == "" {
		rs := []rune(text)
		out, p, _ := insertRunes(buf, pos, rs, 0)
		return out, p
	}
	if e.staged != "" {
		e.staged += "\n" + text
	} else {
		e.staged = text
	}
	return buf, pos
}

// cleanPaste strips bracketed-paste markers and normalizes line endings.
func cleanPaste(s string) string {
	s = strings.ReplaceAll(s, "\x1b[200~", "")
	s = strings.ReplaceAll(s, "\x1b[201~", "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimRight(s, "\n")
}

// suggestion is one menu entry: what to show, and the full line Tab completes to.
type suggestion struct{ name, desc, complete string }

// suggest returns menu items for the current buffer: model names after
// "/model ", otherwise matching command names.
func (e *LineEditor) suggest(buf string) []suggestion {
	if e.bare {
		return nil // a wizard field is not the REPL prompt: no "/" menu, no @files
	}
	if arg, ok := strings.CutPrefix(buf, "/model "); ok {
		if e.models == nil {
			return nil
		}
		var out []suggestion
		for _, m := range e.models() {
			if strings.HasPrefix(strings.ToLower(m), strings.ToLower(arg)) {
				out = append(out, suggestion{name: m, complete: "/model " + m})
			}
		}
		return out
	}
	if strings.HasPrefix(buf, "/") && !strings.Contains(buf, " ") {
		var out []suggestion
		for _, c := range replCommands {
			if strings.HasPrefix(c.name, buf) {
				out = append(out, suggestion{name: c.name, desc: c.desc, complete: c.name + " "})
			}
		}
		return out
	}
	// @path file mention, anywhere in the line: complete the current @token.
	if at := strings.LastIndexByte(buf, '@'); at >= 0 && e.files != nil && (at == 0 || buf[at-1] == ' ') {
		if frag := buf[at+1:]; !strings.ContainsAny(frag, " \t") {
			var out []suggestion
			for _, f := range e.files(frag) {
				out = append(out, suggestion{name: "@" + f, complete: buf[:at] + "@" + f})
			}
			return out
		}
	}
	return nil
}

func (e *LineEditor) out(s string) { fmt.Print(s) }

// inputFence and inputBand are the prompt's own chrome: the hairline that bounds
// the input area, and the gray-green slab a submitted line collapses into. They
// are two functions rather than five expressions so the fence above the line and
// the fence below the band cannot be drawn to two widths.
//
// Both take the ONE leading space every frame is printed in. They had none, which
// is why at a terminal of 60 the fence measured 60 against a 59-column frame
// directly above it — the off-by-one is fixed at its source rather than by
// patching the number.
func inputFence(w int) string { return " " + cFaint + strings.Repeat(gRule, w) + cReset }

// The band PADS and never cuts: the text in it is what the operator just typed,
// and a prompt longer than the page is one the terminal may soft-wrap but that we
// must not shorten. So a band is houseWidth()+1 columns or exactly as wide as the
// line it holds, whichever is more — and it is drawn once, after the line is
// submitted, where nothing walks back over it.
func inputBand(w int, text string) string {
	return " " + cBandBg + padTo(text, w, 0) + cReset
}

// ReadLine prints prompt and returns the entered line. Returns errLineCancel on
// Ctrl-C (caller should just continue) and io.EOF on Ctrl-D / stream end.
func (e *LineEditor) ReadLine(prompt, initial string) (string, error) {
	// The editor's own Input, not os.Stdin: a scripted Input (tests, `lca <<EOF`)
	// has no terminal to switch, and raw-moding a descriptor it does not read
	// from would change the operator's terminal for nothing.
	fd := e.in.fd()
	if fd < 0 {
		return e.cooked(prompt, initial)
	}
	restore, err := makeRaw(fd)
	if err != nil {
		return e.cooked(prompt, initial)
	}
	defer restore()
	e.out("\033[?2004h") // ask the terminal to bracket pastes
	defer e.out("\033[?2004l")

	buf := []rune(initial)
	pos := len(buf)
	hist := len(e.history)
	// Whatever was typed or pasted while the agent worked is waiting in the
	// buffer: take it as one message rather than a series of lines.
	e.in.Drain()
	if pending := e.in.TakePending(); strings.TrimSpace(pending) != "" {
		buf, pos = e.stage(pending, buf, pos)
	}

	// top fence of the input area, at the house width and not the terminal's: the
	// fence and the frames above it are the same chrome and have to agree
	if !e.bare {
		e.out(inputFence(houseWidth()) + "\r\n")
	}
	e.render(prompt, buf, pos)
	for {
		b, err := e.in.ReadByte()
		if err != nil {
			return "", io.EOF
		}
		switch b {
		case '\r', '\n':
			e.submit(prompt, buf)
			line := e.take(buf)
			e.remember(line)
			return line, nil
		case 3: // Ctrl-C
			e.out("\r\033[J" + prompt + string(buf) + "^C\r\n")
			return "", errLineCancel
		case 4: // Ctrl-D
			if len(buf) == 0 {
				e.out("\r\n")
				return "", io.EOF
			}
		case 127, 8: // Backspace
			if pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
			}
		case 1: // Ctrl-A
			pos = 0
		case 5: // Ctrl-E
			pos = len(buf)
		case 21: // Ctrl-U — clear the line (and any staged paste)
			buf, pos, e.staged = nil, 0, ""
		case 23: // Ctrl-W — delete previous word
			buf, pos = deleteWord(buf, pos)
		case 9: // Tab — complete a command / model name
			buf, pos = e.complete(buf)
		case 27: // ESC — an escape sequence (arrows etc.)
			buf, pos, hist = e.escape(buf, pos, hist)
		default:
			if b >= 32 {
				r := e.readRune(b)
				buf = append(buf, 0)
				copy(buf[pos+1:], buf[pos:])
				buf[pos] = r
				pos++
			}
		}
		e.render(prompt, buf, pos)
	}
}

// readEscape reads what follows an ESC and names the key: "A".."D" for the
// arrows in both the CSI and the application-cursor spelling, "H"/"F", "3~"
// Delete, "5~"/"6~" PgUp/PgDn, "200~" paste start, "esc" for a second ESC, and
// "" for a sequence we do not know. Shared with the picker (pick.go), so the
// editor and the menu cannot disagree about what ↑ is.
func readEscape(in *Input) string {
	b1, err := in.ReadByte()
	if err != nil {
		return ""
	}
	switch {
	case b1 == 'O': // application cursor keys: ESC O A/B/C/D
		b2, err := in.ReadByte()
		if err != nil {
			return ""
		}
		return string(b2)
	case b1 == 27: // ESC ESC — the picker's "get me out of here"
		return "esc"
	case b1 != '[':
		return ""
	}
	// read the rest of the CSI sequence up to its final byte
	var seq []byte
	for {
		c, err := in.ReadByte()
		if err != nil {
			return ""
		}
		seq = append(seq, c)
		if c >= 0x40 && c <= 0x7e {
			break
		}
	}
	return string(seq)
}

func (e *LineEditor) escape(buf []rune, pos, hist int) ([]rune, int, int) {
	switch k := readEscape(e.in); k {
	case "200~": // bracketed paste: inline if short, else staged as one message
		buf, pos = e.stage(e.readPaste(), buf, pos)
		return buf, pos, hist
	case "3~": // Delete
		if pos < len(buf) {
			buf = append(buf[:pos], buf[pos+1:]...)
		}
		return buf, pos, hist
	default:
		return e.applyKey(k, buf, pos, hist)
	}
}

func (e *LineEditor) applyKey(k string, buf []rune, pos, hist int) ([]rune, int, int) {
	switch k {
	case "A": // ↑ history back
		if hist > 0 {
			hist--
			buf = []rune(e.history[hist])
			pos = len(buf)
		}
	case "B": // ↓ history forward
		if hist < len(e.history)-1 {
			hist++
			buf = []rune(e.history[hist])
		} else {
			hist = len(e.history)
			buf = nil
		}
		pos = len(buf)
	case "C": // → right
		if pos < len(buf) {
			pos++
		}
	case "D": // ← left
		if pos > 0 {
			pos--
		}
	case "H":
		pos = 0
	case "F":
		pos = len(buf)
	}
	return buf, pos, hist
}

// readPaste reads a bracketed-paste body up to the end marker (ESC[201~) and
// returns it with newlines normalized. The block is inserted as literal text —
// its newlines do NOT submit the line.
func (e *LineEditor) readPaste() string {
	var b []byte
	end := []byte("\x1b[201~")
	for {
		c, err := e.in.ReadByte()
		if err != nil {
			break
		}
		b = append(b, c)
		if bytes.HasSuffix(b, end) {
			b = b[:len(b)-len(end)]
			break
		}
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func insertRunes(buf []rune, pos int, rs []rune, hist int) ([]rune, int, int) {
	out := make([]rune, 0, len(buf)+len(rs))
	out = append(out, buf[:pos]...)
	out = append(out, rs...)
	out = append(out, buf[pos:]...)
	return out, pos + len(rs), hist
}

// displayRunes flattens embedded newlines (from a paste) to a visible marker so
// the single-line editor renders cleanly while buf keeps the real newlines.
func displayRunes(buf []rune) string {
	var b strings.Builder
	for _, r := range buf {
		if r == '\n' {
			b.WriteString(gEnter + " ")
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// readRune assembles a full UTF-8 rune from its first byte (Cyrillic etc. are
// multi-byte), reading continuation bytes as needed.
func (e *LineEditor) readRune(first byte) rune { return readRuneIn(e.in, first) }

// readRuneIn is the same assembly for any Input — the picker's filter takes
// Cyrillic too, and one decoder means one set of edge cases.
func readRuneIn(in *Input, first byte) rune {
	if first < 0x80 {
		return rune(first)
	}
	var n int
	switch {
	case first&0xE0 == 0xC0:
		n = 1
	case first&0xF0 == 0xE0:
		n = 2
	case first&0xF8 == 0xF0:
		n = 3
	default:
		return utf8.RuneError
	}
	bytes := make([]byte, 1, 4)
	bytes[0] = first
	for i := 0; i < n; i++ {
		c, err := in.ReadByte()
		if err != nil {
			break
		}
		bytes = append(bytes, c)
	}
	r, _ := utf8.DecodeRune(bytes)
	return r
}

// take is the message being sent: a staged paste plus anything typed after it.
func (e *LineEditor) take(buf []rune) string {
	typed := string(buf)
	staged := e.staged
	e.staged = ""
	switch {
	case staged == "":
		return typed
	case strings.TrimSpace(typed) == "":
		return staged
	}
	return staged + "\n" + typed
}

// submit collapses the live input into the past-prompt presentation: the entered
// text as a full-width gray-green band, closed by a hairline below (the top fence
// was drawn when the prompt opened). An empty line just advances.
func (e *LineEditor) submit(prompt string, buf []rune) {
	// A bare field leaves the answer as a plain line inside its section: the band
	// and its hairline are the REPL prompt's chrome, drawn at the terminal's full
	// width, and they straddled the 76-column rule of the section above them.
	if e.bare {
		e.out("\r\033[J" + prompt + e.display(buf) + "\r\n")
		return
	}
	// ONE width snapshot for the band and the hairline under it, so the two can
	// never disagree because the window moved between them.
	w := houseWidth()
	if e.staged != "" {
		e.out("\r\033[J" + inputBand(w, stripANSI(prompt)+pasteSummary(e.staged)+"  "+displayRunes(buf)) +
			"\r\n" + inputFence(w) + "\r\n")
		return
	}
	if len(buf) == 0 {
		e.out("\r\033[J" + prompt + "\r\n")
		return
	}
	e.out("\r\033[J" + inputBand(w, stripANSI(prompt)+displayRunes(buf)) + "\r\n" + inputFence(w) + "\r\n")
}

// render redraws the input line and, when the line is a "/command" prefix, a
// menu of matching commands below it, leaving the cursor at the right column.
func (e *LineEditor) render(prompt string, buf []rune, pos int) {
	menu := e.suggest(string(buf))
	e.out("\r\033[J") // clear from line start down (input + any old menu / status)
	// The input is kept to ONE terminal row, by showing a window over the text
	// instead of all of it. Printed whole, a line longer than the terminal is
	// SOFT-WRAPPED by the terminal onto a second row, and from there every
	// assumption below is off by that row: the walk-back counts menu rows only,
	// so "\033[<n>A" lands on the row the wrap ended on, the next repaint's
	// "\r\033[J" clears from there down and leaves the first row standing, and the
	// cursor's "\033[<col>C" is one row too low. The operator's screen filled with
	// dozens of stacked copies of their own half-typed question — a 153-column
	// line in an 80-column terminal, repainted once per keystroke.
	//
	// A window needs no state: it is computed from the cursor every repaint, the
	// same way the rest of this file recomputes everything it draws.
	lead := 0
	stagedTag := ""
	if e.staged != "" {
		stagedTag = cBold + "[" + pasteSummary(e.staged) + "]" + cReset + " "
		lead = visibleWidth("[" + pasteSummary(e.staged) + "] ")
	}
	disp := []rune(e.display(buf))
	dpos := len([]rune(e.display(buf[:pos])))
	// One column is left unwritten: writing in the last one is what makes a
	// terminal wrap, and the cursor has to be able to sit after the last rune.
	room := termWidth() - visibleWidth(prompt) - lead - 1
	lo, hi, cutL, cutR := inputWindow(disp, dpos, room)
	shown := string(disp[lo:hi])
	if cutL {
		shown = cFaint + gEllipsis + cReset + shown
	}
	if cutR {
		shown += cFaint + gEllipsis + cReset
	}
	e.out(prompt + stagedTag + shown)
	below := 0
	// ONE width snapshot for the whole menu: this region is walked back over by
	// COUNTING its rows (below, further down), so a row that soft-wraps puts the
	// "\033[<n>A" one row out and the next repaint's "\r\033[J" erases the line
	// above the input. The descriptions were never bounded at all, so a long one at
	// a narrow terminal already did exactly that.
	// "  " + %-11s + " " = 14, against a printed H+1 — and then bounded by the
	// reading measure, because a description is PROSE. At 140 the /mcp description
	// printed as one 112-column sentence, 24 columns past the measure the answer is
	// held to and the longest run of prose anywhere on the screen; the walk-back
	// only needs the row count to stay known, and a tighter bound keeps it known
	// for free.
	descRoom := min(houseWidth()-13, proseMax)
	for _, m := range menu {
		line := "\r\n  " + cFaint + fmt.Sprintf("%-11s", m.name) + cReset
		if m.desc != "" {
			d := m.desc
			if descRoom > 8 && visibleWidth(d) > descRoom {
				d = ellipsize(stripANSI(d), descRoom)
			}
			line += " " + cFaint + d + cReset
		}
		e.out(line)
		below++
	}
	// With no command menu open, show the persistent status line under the input.
	if below == 0 && !e.bare && e.status != nil {
		if s := e.status(); s != "" {
			e.out("\r\n  " + cFaint + s + cReset)
			below++
		}
	}
	if below > 0 {
		e.out(fmt.Sprintf("\033[%dA", below)) // back up to the input line
	}
	e.out("\r")
	// The cursor is placed inside the WINDOW: the text left of it that scrolled
	// off costs no columns, and the left marker costs its own measured width.
	col := visibleWidth(prompt) + lead + visibleWidth(string(disp[lo:dpos]))
	if cutL {
		col += visibleWidth(gEllipsis)
	}
	if col > 0 {
		e.out(fmt.Sprintf("\033[%dC", col))
	}
}

// inputWindow picks the slice of a line that is shown on the input row, so the
// row never wraps. It keeps the cursor on screen with a little room to its
// right, so typing forward does not re-scroll on every keystroke, and reports
// which ends were cut so the caller can mark them.
//
// Widths, not rune counts: one Cyrillic rune is one column and one CJK rune is
// two, and a window measured in runes shears exactly the lines this exists to
// keep whole.
func inputWindow(rs []rune, pos, room int) (lo, hi int, cutL, cutR bool) {
	if room <= 0 || len(rs) == 0 {
		return 0, 0, false, false
	}
	if pos > len(rs) {
		pos = len(rs)
	}
	w := make([]int, len(rs))
	total := 0
	for i, r := range rs {
		w[i] = visibleWidth(string(r))
		total += w[i]
	}
	if total <= room {
		return 0, len(rs), false, false
	}
	mark := visibleWidth(gEllipsis)
	slack := min(8, room/4) // columns kept ahead of the cursor
	// Two passes at most: the budget depends on which ends are cut, and which
	// ends are cut depends on the budget. It settles immediately because cutting
	// an end only ever shrinks the window.
	for iter := 0; iter < 3; iter++ {
		budget := room
		if cutL {
			budget -= mark
		}
		if cutR {
			budget -= mark
		}
		if budget < 1 {
			budget = 1
		}
		hi = min(len(rs), pos+slack)
		used := 0
		lo = hi
		for lo > 0 && used+w[lo-1] <= budget {
			lo--
			used += w[lo]
		}
		// The cursor must be inside the window even when the budget is tiny: show
		// the text to its right rather than a window it has fallen off.
		if lo > pos {
			lo = pos
			used = 0
			for i := lo; i < hi; i++ {
				used += w[i]
			}
			for hi > pos && used > budget {
				hi--
				used -= w[hi]
			}
		}
		for hi < len(rs) && used+w[hi] <= budget {
			used += w[hi]
			hi++
		}
		nl, nr := lo > 0, hi < len(rs)
		if nl == cutL && nr == cutR {
			break
		}
		cutL, cutR = nl, nr
	}
	return lo, hi, cutL, cutR
}

// cooked is the fallback line read for non-terminal stdin. It shows the prefill
// and returns it on an empty line, because a prompt that offers a default and
// then discards it when you press Enter is worse than no default — and raw mode
// is exactly where you cannot see that happen.
func (e *LineEditor) cooked(prompt, initial string) (string, error) {
	if initial != "" {
		e.out(prompt + "[" + initial + "] ")
	} else {
		e.out(prompt)
	}
	line, err := e.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return initial, nil
	}
	return line, nil
}

// complete completes the buffer against the current suggestions: fully if there
// is one, else to the longest common prefix.
func (e *LineEditor) complete(buf []rune) ([]rune, int) {
	items := e.suggest(string(buf))
	if len(items) == 0 {
		return buf, len(buf)
	}
	if len(items) == 1 {
		s := []rune(items[0].complete)
		return s, len(s)
	}
	common := items[0].complete
	for _, it := range items[1:] {
		common = commonPrefix(common, it.complete)
	}
	if len([]rune(common)) > len(buf) {
		s := []rune(common)
		return s, len(s)
	}
	return buf, len(buf)
}

func commonPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

func deleteWord(buf []rune, pos int) ([]rune, int) {
	i := pos
	for i > 0 && buf[i-1] == ' ' {
		i--
	}
	for i > 0 && buf[i-1] != ' ' {
		i--
	}
	return append(buf[:i], buf[pos:]...), i
}
