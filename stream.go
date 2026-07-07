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

	line         strings.Builder // current line buffer (until newline)
	table        []string        // buffered consecutive table rows (rendered on flush)
	thinking     bool            // inside a <think>…</think> reasoning block
	reason       strings.Builder // partial line of reasoning_content (separate field)
	showThink    bool            // expand reasoning; else collapse to an animated marker
	reasonLog    []string        // all reasoning lines this step (for /think last)
	markerActive bool            // the animated "thinking" marker is on the current line
	markerPrefix string          // gutter prefix used to redraw the marker in place
	dotPhase     int             // animation phase for the marker's dots
	anim         bool            // redraw in place (only when stdout is a real terminal)
}

func newProseWriter(raw, showThink bool) *proseWriter {
	return &proseWriter{raw: raw, showThink: showThink, anim: osTermWidth() > 0}
}

// leadMarker opens the marker line with the assistant gutter.
func (p *proseWriter) leadMarker() {
	if !p.started {
		p.markerPrefix = " " + cBold + gUp + cReset + " "
		p.started = true
	} else {
		p.markerPrefix = "   "
	}
	p.out("\n" + p.markerPrefix)
}

// animateThinking shows the collapsed-reasoning marker. On a terminal it redraws
// in place so the dots move (thinking. → thinking.. → …); when piped it prints a
// single static marker (no escape codes to pollute the output).
func (p *proseWriter) animateThinking() {
	if !p.anim {
		if !p.markerActive {
			p.leadMarker()
			p.out(cFaint + "thinking…" + cReset)
			p.markerActive = true
		}
		return
	}
	label := cFaint + "thinking" + strings.Repeat(".", 1+p.dotPhase%3) + cReset
	p.dotPhase++
	if !p.markerActive {
		p.leadMarker()
		p.out(label)
		p.markerActive = true
		return
	}
	p.out("\r\033[K" + p.markerPrefix + label) // redraw the same line
}

// closeMarker finalizes the marker (steady "thinking…") when the answer begins.
func (p *proseWriter) closeMarker() {
	if !p.markerActive {
		return
	}
	if p.anim {
		p.out("\r\033[K" + p.markerPrefix + cFaint + "thinking…" + cReset)
	}
	p.markerActive = false
	p.pendingNewline = true
}

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
	if !p.showThink {
		p.animateThinking() // advance the marker per delta so it visibly moves
	}
}

func (p *proseWriter) flushReason() {
	t := strings.TrimSpace(p.reason.String())
	p.reason.Reset()
	if t == "" {
		return
	}
	p.reasonLog = append(p.reasonLog, t) // captured for /think last regardless of mode
	if p.showThink {
		p.printLine(faint("%s", t))
	}
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
	p.closeMarker()
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

	// (1b) reasoning: <think>…</think> in the content — dim when expanded, else a
	// compact "thinking…" marker; the tags are always dropped.
	if !p.thinking {
		if trimmed == "<think>" || trimmed == "<thinking>" {
			p.thinking = true
			if !p.showThink {
				p.animateThinking()
			}
			return
		}
	} else {
		if trimmed == "</think>" || trimmed == "</thinking>" {
			p.thinking = false
			return
		}
		if trimmed != "" {
			p.reasonLog = append(p.reasonLog, trimmed)
			if p.showThink {
				p.printLine(faint("%s", trimmed))
			} else {
				p.animateThinking()
			}
		}
		return
	}

	// The answer proper begins — finalize any animated reasoning marker.
	p.closeMarker()

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
