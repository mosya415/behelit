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
		return strings.Repeat("•", len(buf))
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

	// top fence of the input area
	if !e.bare {
		e.out(cFaint + strings.Repeat("─", termWidth()) + cReset + "\r\n")
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
			b.WriteString("⏎ ")
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
	if e.staged != "" {
		w := termWidth()
		band := cBandBg + padTo(stripANSI(prompt)+pasteSummary(e.staged)+"  "+displayRunes(buf), w, 0) + cReset
		e.out("\r\033[J" + band + "\r\n" + cFaint + strings.Repeat("─", w) + cReset + "\r\n")
		return
	}
	if len(buf) == 0 {
		e.out("\r\033[J" + prompt + "\r\n")
		return
	}
	w := termWidth()
	bar := cFaint + strings.Repeat("─", w) + cReset
	band := cBandBg + padTo(stripANSI(prompt)+displayRunes(buf), w, 0) + cReset
	e.out("\r\033[J" + band + "\r\n" + bar + "\r\n")
}

// render redraws the input line and, when the line is a "/command" prefix, a
// menu of matching commands below it, leaving the cursor at the right column.
func (e *LineEditor) render(prompt string, buf []rune, pos int) {
	menu := e.suggest(string(buf))
	e.out("\r\033[J") // clear from line start down (input + any old menu / status)
	if e.staged != "" {
		e.out(prompt + cBold + "[" + pasteSummary(e.staged) + "]" + cReset + " " + e.display(buf))
	} else {
		e.out(prompt + e.display(buf))
	}
	below := 0
	for _, m := range menu {
		line := "\r\n  " + cFaint + fmt.Sprintf("%-11s", m.name) + cReset
		if m.desc != "" {
			line += " " + cFaint + m.desc + cReset
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
	lead := 0
	if e.staged != "" {
		lead = visibleWidth("[" + pasteSummary(e.staged) + "] ")
	}
	if col := visibleWidth(prompt) + lead + visibleWidth(e.display(buf[:pos])); col > 0 {
		e.out(fmt.Sprintf("\033[%dC", col))
	}
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
