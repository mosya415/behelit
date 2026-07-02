package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
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
	{"/discover", "find live models on the cluster"},
	{"/endpoint", "list / switch endpoints"},
	{"/model", "show / set the model"},
	{"/approve", "approval mode (on|off|run|edit)"},
	{"/reset", "clear the transcript"},
	{"/exit", "quit"},
}

type LineEditor struct {
	rd      *bufio.Reader
	history []string
	models  func() []string // known model names, for /model completion
}

func NewLineEditor(rd *bufio.Reader) *LineEditor { return &LineEditor{rd: rd} }

// suggestion is one menu entry: what to show, and the full line Tab completes to.
type suggestion struct{ name, desc, complete string }

// suggest returns menu items for the current buffer: model names after
// "/model ", otherwise matching command names.
func (e *LineEditor) suggest(buf string) []suggestion {
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
	return nil
}

func (e *LineEditor) out(s string) { fmt.Print(s) }

// ReadLine prints prompt and returns the entered line. Returns errLineCancel on
// Ctrl-C (caller should just continue) and io.EOF on Ctrl-D / stream end.
func (e *LineEditor) ReadLine(prompt string) (string, error) {
	restore, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return e.cooked(prompt)
	}
	defer restore()

	var buf []rune
	pos := 0
	hist := len(e.history)

	// top fence of the input area
	e.out(cFaint + strings.Repeat("─", termWidth()) + cReset + "\r\n")
	e.render(prompt, buf, pos)
	for {
		b, err := e.rd.ReadByte()
		if err != nil {
			return "", io.EOF
		}
		switch b {
		case '\r', '\n':
			e.submit(prompt, buf)
			line := string(buf)
			if s := strings.TrimSpace(line); s != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != s) {
				e.history = append(e.history, s)
			}
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
		case 21: // Ctrl-U — clear line
			buf, pos = nil, 0
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

func (e *LineEditor) escape(buf []rune, pos, hist int) ([]rune, int, int) {
	b1, err := e.rd.ReadByte()
	if err != nil || (b1 != '[' && b1 != 'O') {
		return buf, pos, hist
	}
	b2, err := e.rd.ReadByte()
	if err != nil {
		return buf, pos, hist
	}
	switch b2 {
	case 'A': // ↑ history back
		if hist > 0 {
			hist--
			buf = []rune(e.history[hist])
			pos = len(buf)
		}
	case 'B': // ↓ history forward
		if hist < len(e.history)-1 {
			hist++
			buf = []rune(e.history[hist])
		} else {
			hist = len(e.history)
			buf = nil
		}
		pos = len(buf)
	case 'C': // → right
		if pos < len(buf) {
			pos++
		}
	case 'D': // ← left
		if pos > 0 {
			pos--
		}
	case 'H': // Home
		pos = 0
	case 'F': // End
		pos = len(buf)
	case '3': // Delete (ESC [ 3 ~)
		e.rd.ReadByte() // consume '~'
		if pos < len(buf) {
			buf = append(buf[:pos], buf[pos+1:]...)
		}
	}
	return buf, pos, hist
}

// readRune assembles a full UTF-8 rune from its first byte (Cyrillic etc. are
// multi-byte), reading continuation bytes as needed.
func (e *LineEditor) readRune(first byte) rune {
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
		c, err := e.rd.ReadByte()
		if err != nil {
			break
		}
		bytes = append(bytes, c)
	}
	r, _ := utf8.DecodeRune(bytes)
	return r
}

// submit collapses the live input into the past-prompt presentation: the entered
// text as a full-width gray-green band, closed by a hairline below (the top fence
// was drawn when the prompt opened). An empty line just advances.
func (e *LineEditor) submit(prompt string, buf []rune) {
	if len(buf) == 0 {
		e.out("\r\033[J" + prompt + "\r\n")
		return
	}
	w := termWidth()
	bar := cFaint + strings.Repeat("─", w) + cReset
	band := cBandBg + padTo(stripANSI(prompt)+string(buf), w, 0) + cReset
	e.out("\r\033[J" + band + "\r\n" + bar + "\r\n")
}

// render redraws the input line and, when the line is a "/command" prefix, a
// menu of matching commands below it, leaving the cursor at the right column.
func (e *LineEditor) render(prompt string, buf []rune, pos int) {
	menu := e.suggest(string(buf))
	e.out("\r\033[J") // clear from line start down (input + any old menu)
	e.out(prompt + string(buf))
	for _, m := range menu {
		line := "\r\n  " + cFaint + fmt.Sprintf("%-11s", m.name) + cReset
		if m.desc != "" {
			line += " " + cFaint + m.desc + cReset
		}
		e.out(line)
	}
	if len(menu) > 0 {
		e.out(fmt.Sprintf("\033[%dA", len(menu))) // back up to the input line
	}
	e.out("\r")
	if col := visibleWidth(prompt) + visibleWidth(string(buf[:pos])); col > 0 {
		e.out(fmt.Sprintf("\033[%dC", col))
	}
}

// cooked is the fallback line read for non-terminal stdin.
func (e *LineEditor) cooked(prompt string) (string, error) {
	e.out(prompt)
	line, err := e.rd.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
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
