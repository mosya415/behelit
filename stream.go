package main

import (
	"fmt"
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
// Filtering is line-oriented because the protocol is line-anchored: a tag must
// occupy a whole line. In raw mode the stream is passed through verbatim (for
// debugging a model's protocol adherence).
type proseWriter struct {
	raw      bool
	line     strings.Builder // current partial line
	inBlock  bool            // inside a multi-line tool block
	closeTag string          // the </tag> line that ends the current block
	started  bool            // the assistant bullet has been printed
}

func newProseWriter(raw bool) *proseWriter { return &proseWriter{raw: raw} }

// feed consumes a streamed fragment (any size, may split lines).
func (p *proseWriter) feed(s string) {
	if p.raw {
		if !p.started {
			fmt.Print("\n " + cBold + gUp + cReset + " ")
			p.started = true
		}
		fmt.Print(s)
		return
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			p.flush(p.line.String())
			p.line.Reset()
		} else {
			p.line.WriteByte(s[i])
		}
	}
}

// end flushes any trailing partial line and closes the assistant block.
func (p *proseWriter) end() {
	if p.raw {
		if p.started {
			fmt.Println()
		}
		return
	}
	if p.line.Len() > 0 {
		p.flush(p.line.String())
		p.line.Reset()
	}
	if p.started {
		fmt.Println()
	}
}

func (p *proseWriter) flush(line string) {
	trimmed := strings.TrimSpace(line)

	if p.inBlock {
		if trimmed == p.closeTag {
			p.inBlock = false
		}
		return // suppress everything inside a tool block
	}
	if m := reOpen.FindStringSubmatch(trimmed); m != nil && blockNames[m[1]] {
		if m[3] != "/" { // not self-closing → suppress until the close tag
			p.inBlock = true
			p.closeTag = "</" + m[1] + ">"
		}
		return // suppress the tag line itself
	}
	if trimmed == "" {
		return // drop blank lines to keep the view tight
	}
	p.printProse(trimmed)
}

func (p *proseWriter) printProse(text string) {
	if !p.started {
		fmt.Print("\n " + cBold + gUp + cReset + " ")
		p.started = true
	} else {
		fmt.Print("\n   ")
	}
	fmt.Print(text)
}
