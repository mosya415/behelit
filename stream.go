package main

import (
	"fmt"
	"os"
	"strings"
)

// proseWriter filters the streamed assistant text for HUMAN display: it prints
// the model's prose but hides the tool-call tags, which are noise on screen (the
// clean action markers in executeBlocks stand in for them). The full text —
// tags and all — is still appended to the transcript, so nothing is lost for
// audit or for feeding back to the model.
//
// Detection reuses the SAME reOpen / blockNames as the parser (protocol.go), so
// what the screen hides is exactly what the parser will execute — the display
// can never disagree with what actually runs.
//
// Streaming is character-level so ordinary prose appears token-by-token. Only a
// line that *could* be a tag is buffered: the protocol is line-anchored, and a
// tag line begins (after optional whitespace) with '<'. A line whose first
// non-blank character is not '<' is streamed live immediately; a line that opens
// with '<' is held until end-of-line, then matched — if it is a tag it is
// hidden, otherwise it is flushed as prose. In raw mode the stream is passed
// through verbatim (for debugging a model's protocol adherence).
type proseWriter struct {
	raw bool

	inBlock  bool   // inside a multi-line tool block
	closeTag string // the </tag> line that ends the current block

	started        bool // the assistant bullet has been printed at least once
	pendingNewline bool // a prose line ended; emit continuation before next prose

	mode int             // per-line: pmUndecided / pmProse / pmBuffer
	buf  strings.Builder // holds the line until we can decide (undecided/buffer)
}

const (
	pmUndecided = iota
	pmProse
	pmBuffer
)

func newProseWriter(raw bool) *proseWriter { return &proseWriter{raw: raw} }

func (p *proseWriter) out(s string) { fmt.Print(s) }

// outByte writes a single raw byte. We must NOT use string(c) here: for a byte
// >127 that would reinterpret it as a rune and re-encode it, corrupting any
// multi-byte UTF-8 (em dashes, non-ASCII) that arrives split across the stream.
func (p *proseWriter) outByte(c byte) { os.Stdout.Write([]byte{c}) }

// feed consumes a streamed fragment (any size, may split lines).
func (p *proseWriter) feed(s string) {
	if p.raw {
		if !p.started {
			p.out("\n " + cBold + gUp + cReset + " ")
			p.started = true
		}
		p.out(s)
		return
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			p.newline()
		} else {
			p.char(s[i])
		}
	}
}

func (p *proseWriter) char(c byte) {
	switch p.mode {
	case pmProse:
		p.outByte(c) // line prefix already emitted; stream live (raw byte, UTF-8 safe)
	case pmBuffer:
		p.buf.WriteByte(c)
	default: // pmUndecided
		if p.inBlock {
			p.buf.WriteByte(c)
			p.mode = pmBuffer
			return
		}
		p.buf.WriteByte(c)
		s := strings.TrimLeft(p.buf.String(), " \t")
		if s == "" {
			return // still only leading whitespace
		}
		if s[0] == '<' {
			p.mode = pmBuffer // might be a tag; hold until end-of-line
			return
		}
		p.mode = pmProse
		p.beginProseLine()
		p.out(s) // flush the buffered start of the line (leading blanks trimmed)
		p.buf.Reset()
	}
}

func (p *proseWriter) newline() {
	switch {
	case p.inBlock:
		if strings.TrimSpace(p.buf.String()) == p.closeTag {
			p.inBlock = false
		}
		// suppressed — nothing printed
	case p.mode == pmProse:
		p.pendingNewline = true // defer; a following tag block may intervene
	case p.mode == pmBuffer:
		t := strings.TrimSpace(p.buf.String())
		if m := reOpen.FindStringSubmatch(t); m != nil && blockNames[m[1]] {
			if m[3] != "/" { // not self-closing → suppress until close tag
				p.inBlock = true
				p.closeTag = "</" + m[1] + ">"
			}
			// tag line suppressed
		} else {
			p.beginProseLine() // prose that merely started with '<'
			p.out(t)
			p.pendingNewline = true
		}
	}
	// pmUndecided (blank/whitespace-only line) → dropped
	p.buf.Reset()
	p.mode = pmUndecided
}

// end flushes any trailing partial line and closes the assistant block.
func (p *proseWriter) end() {
	if p.raw {
		if p.started {
			p.out("\n")
		}
		return
	}
	switch {
	case p.inBlock:
		// unterminated block — suppress remainder
	case p.mode == pmBuffer:
		t := strings.TrimSpace(p.buf.String())
		if m := reOpen.FindStringSubmatch(t); m != nil && blockNames[m[1]] {
			// a complete tag with no trailing newline → suppress
		} else if t != "" {
			p.beginProseLine()
			p.out(t)
		}
	}
	p.buf.Reset()
	if p.started {
		p.out("\n")
	}
}

// beginProseLine emits the line lead-in: the assistant bullet for the first
// prose line, or the continuation indent for later ones.
func (p *proseWriter) beginProseLine() {
	switch {
	case !p.started:
		p.out("\n " + cBold + gUp + cReset + " ")
		p.started = true
	case p.pendingNewline:
		p.out("\n   ")
		p.pendingNewline = false
	}
}
