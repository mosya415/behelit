package main

import (
	"fmt"
	"strings"
)

// proseWriter filters and formats the streamed assistant text for HUMAN display.
// Two jobs:
//
//  1. Hide the tool-call tags. They are noise on screen (the clean action markers
//     in executeBlocks stand in for them). Detection reuses the SAME reOpen /
//     blockNames as the parser (protocol.go), so what the screen hides is exactly
//     what the parser will execute. The full text — tags and all — is still
//     appended to the transcript and fed back to the model.
//
//  2. Render Markdown (markdown.go) so prose reads nicely: headings, emphasis,
//     inline code, lists, block quotes, fenced code blocks and inline math.
//
// Because Markdown is line/block structured, display is line-buffered: a line is
// rendered once complete, so lines appear as the model generates them. In raw
// mode (LCA_RAW) the stream is passed through verbatim, unformatted — for
// debugging a model's protocol adherence.
type proseWriter struct {
	raw bool

	inBlock  bool   // inside a multi-line tool block (suppressed)
	closeTag string // the </tag> line that ends the current tool block
	fence    bool   // inside a ``` fenced code block

	started        bool // the assistant bullet has been printed at least once
	pendingNewline bool // a line was printed; emit the separator before the next

	line     strings.Builder // current line buffer (until newline)
	table    []string        // buffered consecutive table rows (rendered on flush)
	thinking bool            // inside a <think>…</think> reasoning block
	reason   strings.Builder // partial line of reasoning_content (separate field)
}

func newProseWriter(raw bool) *proseWriter { return &proseWriter{raw: raw} }

func (p *proseWriter) out(s string) { fmt.Print(s) }

func (p *proseWriter) feed(s string) {
	if p.raw {
		if !p.started {
			p.out("\n " + cBold + gUp + cReset + " ")
			p.started = true
		}
		p.out(s)
		return
	}
	p.flushReason() // any pending reasoning line closes before answer content
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			p.flushLine(p.line.String())
			p.line.Reset()
		} else {
			p.line.WriteByte(s[i])
		}
	}
}

// feedReasoning consumes reasoning_content deltas (the separate field some
// reasoning models stream) and renders them dimmed, line by line.
func (p *proseWriter) feedReasoning(s string) {
	if p.raw {
		if !p.started {
			p.out("\n " + cBold + gUp + cReset + " ")
			p.started = true
		}
		p.out(cFaint + s + cReset)
		return
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			p.flushReason()
		} else {
			p.reason.WriteByte(s[i])
		}
	}
}

func (p *proseWriter) flushReason() {
	if t := strings.TrimSpace(p.reason.String()); t != "" {
		p.printLine(faint("%s", t))
	}
	p.reason.Reset()
}

func (p *proseWriter) end() {
	if p.raw {
		if p.started {
			p.out("\n")
		}
		return
	}
	p.flushReason()
	if p.line.Len() > 0 {
		p.flushLine(p.line.String())
		p.line.Reset()
	}
	p.flushTable()
	if p.started {
		p.out("\n")
	}
}

func (p *proseWriter) flushLine(raw string) {
	line := strings.TrimRight(raw, " \t\r")
	trimmed := strings.TrimSpace(line)

	// (1) tool-tag suppression — applied everywhere so the screen can never show
	// a tag that the parser will nonetheless execute.
	if p.inBlock {
		if trimmed == p.closeTag {
			p.inBlock = false
		}
		return
	}
	if m := reOpen.FindStringSubmatch(trimmed); m != nil && blockNames[m[1]] {
		if m[3] != "/" {
			p.inBlock = true
			p.closeTag = "</" + m[1] + ">"
		}
		return
	}

	// (1b) reasoning: <think>…</think> in the content — render dimmed, drop tags.
	if !p.thinking {
		if trimmed == "<think>" || trimmed == "<thinking>" {
			p.thinking = true
			return
		}
	} else {
		if trimmed == "</think>" || trimmed == "</thinking>" {
			p.thinking = false
			return
		}
		if trimmed != "" {
			p.printLine(faint("%s", trimmed))
		}
		return
	}

	// (2) fenced code blocks: toggle on ``` / ~~~, print inner lines verbatim.
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		p.flushTable()
		p.fence = !p.fence
		return
	}
	if p.fence {
		p.printLine(cFaint + "│ " + cReset + cCode + line + cFgOff)
		return
	}

	// (3) pipe tables: buffer consecutive rows, render aligned when the run ends.
	if isTableRow(trimmed) {
		p.table = append(p.table, line)
		return
	}
	p.flushTable()

	if trimmed == "" {
		return // drop blank lines to keep the view tight
	}
	p.printLine(renderMarkdownLine(line))
}

func (p *proseWriter) flushTable() {
	if len(p.table) == 0 {
		return
	}
	rows := p.table
	p.table = nil
	for _, l := range renderMarkdownTable(rows) {
		p.printLine(l)
	}
}

// printLine emits one display line through the assistant gutter: the bullet for
// the first line of the message, the continuation indent for the rest.
func (p *proseWriter) printLine(content string) {
	switch {
	case !p.started:
		p.out("\n " + cBold + gUp + cReset + " ")
		p.started = true
	case p.pendingNewline:
		p.out("\n   ")
	}
	p.out(content)
	p.pendingNewline = true
}
