package main

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// A persistent input line pinned to the terminal's bottom row while the model
// streams, so the user can read the answer forming above AND type (or queue) the
// next message without waiting. It uses a DECSTBM scroll region: the model's
// output scrolls in the rows above; the bottom row is redrawn as the user types.
//
// screenMu serializes every write to stdout while a footer is active, so the
// footer's redraws (from its own reader goroutine) never interleave with the
// model output printed by proseWriter and the spinner.

var screenMu sync.Mutex

// inputQueue holds what the user typed during streaming: whole lines they
// submitted (Enter) become the next user turns; a trailing unsubmitted fragment
// pre-fills the next prompt.
var inputQueue struct {
	mu      sync.Mutex
	lines   []string
	partial string
}

func queueLine(s string) {
	inputQueue.mu.Lock()
	inputQueue.lines = append(inputQueue.lines, s)
	inputQueue.mu.Unlock()
}

// takeQueuedLine pops the oldest line the user submitted during streaming, if
// any, so the REPL can run it instead of blocking on a fresh prompt.
func takeQueuedLine() (string, bool) {
	inputQueue.mu.Lock()
	defer inputQueue.mu.Unlock()
	if len(inputQueue.lines) == 0 {
		return "", false
	}
	s := inputQueue.lines[0]
	inputQueue.lines = inputQueue.lines[1:]
	return s, true
}

func hasQueuedLine() bool {
	inputQueue.mu.Lock()
	defer inputQueue.mu.Unlock()
	return len(inputQueue.lines) > 0
}

// takePartial returns and clears the unsubmitted fragment, to pre-fill a prompt.
func takePartial() string {
	inputQueue.mu.Lock()
	defer inputQueue.mu.Unlock()
	s := inputQueue.partial
	inputQueue.partial = ""
	return s
}

type footer struct {
	prompt  string
	rows    int
	cols    int
	restore func()
	stopc   chan struct{}
	donec   chan struct{}

	mu  sync.Mutex // guards buf
	buf []byte
}

// startFooter reserves the bottom row and starts reading input into it. Returns
// nil (and changes nothing) when stdout/stdin is not a suitable terminal, so
// piped and non-linux use is unaffected. initial pre-seeds the line with a
// fragment carried over from a previous prompt.
func startFooter(prompt, initial string) *footer {
	cols, rows := osTermSize()
	if rows < 3 || cols < 4 {
		return nil
	}
	restore, err := enterFooterRaw(0)
	if err != nil {
		return nil
	}
	f := &footer{
		prompt:  prompt,
		rows:    rows,
		cols:    cols,
		restore: restore,
		stopc:   make(chan struct{}),
		donec:   make(chan struct{}),
		buf:     []byte(initial),
	}
	screenMu.Lock()
	// Reserve the bottom row for the footer: save the cursor, set the scroll
	// region to everything above it, restore the cursor. Model output now scrolls
	// only in the region; the bottom row stays put.
	fmt.Printf("\0337\033[1;%dr\0338", rows-1)
	f.draw()
	screenMu.Unlock()
	go f.loop()
	return f
}

// draw repaints the footer on the bottom row, leaving the output cursor where it
// was. Caller holds screenMu.
func (f *footer) draw() {
	line := f.prompt + string(f.buf)
	if visibleWidth(line) > f.cols {
		line = truncEnd(line, f.cols)
	}
	fmt.Printf("\0337\033[%d;1H\033[K%s\0338", f.rows, line)
}

func (f *footer) loop() {
	defer close(f.donec)
	for {
		select {
		case <-f.stopc:
			return
		default:
		}
		b, ok := readStdinByte()
		if !ok {
			continue // VTIME timeout — re-check stop
		}
		f.handle(b)
	}
}

func (f *footer) handle(b byte) {
	f.mu.Lock()
	switch b {
	case '\r', '\n':
		line := strings.TrimRight(string(f.buf), "\r\n")
		f.buf = nil
		f.mu.Unlock()
		if strings.TrimSpace(line) != "" {
			queueLine(line)
		}
		screenMu.Lock()
		f.draw()
		screenMu.Unlock()
		return
	case 127, 8: // backspace — drop a whole trailing rune
		if n := len(f.buf); n > 0 {
			_, size := utf8.DecodeLastRune(f.buf)
			f.buf = f.buf[:n-size]
		}
	case 3, 21: // Ctrl-C / Ctrl-U — clear the typed line
		f.buf = nil
	default:
		if b >= 32 || b >= 0x80 { // printable ASCII or a UTF-8 continuation byte
			f.buf = append(f.buf, b)
		}
	}
	f.mu.Unlock()
	screenMu.Lock()
	f.draw()
	screenMu.Unlock()
}

// stop ends input, releases the reserved row and scroll region, and stashes any
// unsubmitted fragment for the next prompt. Safe on a nil footer.
func (f *footer) stop() {
	if f == nil {
		return
	}
	close(f.stopc)
	<-f.donec
	f.mu.Lock()
	partial := string(f.buf)
	f.mu.Unlock()

	screenMu.Lock()
	// Reset the scroll region and clear the footer row (save/restore so output
	// continues exactly where the stream left off).
	fmt.Printf("\033[r\0337\033[%d;1H\033[K\0338", f.rows)
	screenMu.Unlock()
	f.restore()

	if strings.TrimSpace(partial) != "" {
		inputQueue.mu.Lock()
		inputQueue.partial = partial
		inputQueue.mu.Unlock()
	}
}

// truncEnd cuts s to at most w visible columns (ANSI-aware), keeping the head.
func truncEnd(s string, w int) string {
	var b strings.Builder
	width := 0
	for _, r := range s {
		rw := runeWidth(r)
		if width+rw > w {
			break
		}
		b.WriteRune(r)
		width += rw
	}
	return b.String()
}
