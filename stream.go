package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
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
	markerActive bool            // the "thinking" marker is on the current line
	markerPrefix string          // gutter prefix used to redraw the marker in place
	markerLabel  string          // what the marker says: "thinking", or "calling <tool>"
	anim         bool            // animate on a real terminal (not when piped)
	spinning     bool            // the timer-driven spinner goroutine is running
	spinStop     chan struct{}   // signals the spinner to exit
	spinDone     chan struct{}   // closed when the spinner goroutine has exited

	waiting     bool      // the request is out, nothing has come back yet
	markerStart time.Time // when the current marker line appeared
	genStart    time.Time // when the first delta of this response arrived
	tok         int64     // approx tokens streamed this response (atomic; ~1 per delta)
}

func newProseWriter(raw, showThink bool) *proseWriter {
	return &proseWriter{raw: raw, showThink: showThink, anim: osTermWidth() > 0}
}

// noteTok records that a stream delta arrived: starts the generation clock on
// the first one and bumps the running token estimate (~one token per delta).
func (p *proseWriter) noteTok() {
	if p.genStart.IsZero() {
		p.genStart = time.Now()
	}
	atomic.AddInt64(&p.tok, 1)
}

// thinkStat renders the live "elapsed · tokens" suffix for the marker line
// (while waiting for the first token there are no tokens to count yet).
func (p *proseWriter) thinkStat() string {
	base := p.genStart
	if base.IsZero() {
		base = p.markerStart
	}
	el := time.Duration(0)
	if !base.IsZero() {
		el = time.Since(base)
	}
	s := int(el.Seconds())
	var d string
	if s < 60 {
		d = fmt.Sprintf("%ds", s)
	} else {
		d = fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	if p.waiting {
		return d
	}
	return fmt.Sprintf("%s · %d tok", d, atomic.LoadInt64(&p.tok))
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

// beginReasoning shows the collapsed-reasoning marker once. On a terminal it
// starts a timer-driven braille spinner (smooth, independent of token speed);
// piped, it prints a single static marker with no escape codes.
// begin shows a live "waiting for <model>" line the moment the request goes
// out: a slow prefill or a cold model must not look like a freeze. It is
// replaced in place by the reasoning/answer as soon as anything arrives.
func (p *proseWriter) begin(model string) {
	if p.raw || p.markerActive {
		return
	}
	p.waiting = true
	p.markerLabel = "waiting for " + model
	p.beginReasoning()
}

func (p *proseWriter) beginReasoning() {
	if p.markerActive {
		return
	}
	p.markerStart = time.Now()
	p.leadMarker()
	p.markerActive = true
	if p.markerLabel == "" {
		p.markerLabel = "thinking"
	}
	label := p.markerLabel
	if !p.anim {
		p.out(cFaint + label + "…" + cReset)
		return
	}
	p.out(cFaint + "⠋ " + label + " · " + p.thinkStat() + cReset)
	p.spinning = true
	p.spinStop = make(chan struct{})
	p.spinDone = make(chan struct{})
	prefix := p.markerPrefix
	go func() {
		defer close(p.spinDone)
		frames := []rune("⠙⠹⠸⠼⠴⠦⠧⠇⠏⠋")
		tk := time.NewTicker(90 * time.Millisecond)
		defer tk.Stop()
		i := 0
		for {
			select {
			case <-p.spinStop:
				return
			case <-tk.C:
				p.out("\r\033[K" + prefix + cFaint + string(frames[i%len(frames)]) + " " + label + " · " + p.thinkStat() + cReset)
				i++
			}
		}
	}()
}

// closeMarker stops the spinner and finalizes the marker (steady "thinking…")
// when the answer begins. Waits for the spinner goroutine to exit, so no writes
// race with the answer output that follows.
func (p *proseWriter) closeMarker() {
	if !p.markerActive {
		return
	}
	// The waiting line leaves no trace: whatever arrives takes its place.
	if p.waiting {
		if p.spinning {
			close(p.spinStop)
			<-p.spinDone
			p.spinning = false
		}
		p.out("\r\033[K")
		p.waiting, p.markerActive, p.markerLabel = false, false, ""
		p.started, p.pendingNewline = false, false
		return
	}
	if p.spinning {
		close(p.spinStop)
		<-p.spinDone
		p.spinning = false
		done := "thought"
		if p.markerLabel != "thinking" {
			done = p.markerLabel
		}
		p.out("\r\033[K" + p.markerPrefix + cFaint + done + " · " + p.thinkStat() + cReset)
	}
	p.markerActive = false
	p.markerLabel = ""
	p.pendingNewline = true
}

func (p *proseWriter) out(s string) {
	outMu.Lock()
	fmt.Print(s)
	outMu.Unlock()
}

// discard ends the display of a reply that was cut off and will be repeated:
// flush what's pending, mark it void, and start the repeat on a fresh line.
func (p *proseWriter) discard() {
	p.end()
	p.out(" " + faint("%s (cut off — repeating)", gNone) + "\n")
	p.started, p.pendingNewline, p.reasonLog = false, false, nil
}

// feedToolArgs notes a native tool call's arguments streaming in (a large
// write can take a while) with the same live marker as reasoning.
func (p *proseWriter) feedToolArgs(name string) {
	if p.raw {
		return
	}
	if p.waiting {
		p.closeMarker()
	}
	p.noteTok()
	p.flushReason()
	if p.line.Len() > 0 {
		p.flushLine(p.line.String())
		p.line.Reset()
	}
	label := "calling " + name
	if p.markerActive && p.markerLabel != label {
		p.closeMarker()
	}
	if !p.markerActive {
		p.markerLabel = label
		p.beginReasoning()
	}
}

func (p *proseWriter) feed(s string) {
	if p.waiting {
		p.closeMarker()
	}
	if p.raw {
		if !p.started {
			p.out("\n " + cBold + gUp + cReset + " ")
			p.started = true
		}
		p.out(s)
		return
	}
	p.noteTok()
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
	if p.waiting {
		p.closeMarker()
	}
	if p.raw {
		if !p.started {
			p.out("\n " + cBold + gUp + cReset + " ")
			p.started = true
		}
		p.out(cFaint + s + cReset)
		return
	}
	p.noteTok()
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			p.flushReason()
		} else {
			p.reason.WriteByte(s[i])
		}
	}
	if !p.showThink {
		if p.markerActive && p.markerLabel != "thinking" {
			p.closeMarker()
		}
		p.beginReasoning() // marker + spinner (started once)
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

// flushLine splits a physical line on any glued tool tags (so the display sees
// the same structure the parser does) and renders each resulting logical line.
func (p *proseWriter) flushLine(raw string) {
	for _, part := range strings.Split(splitGluedTools(raw), "\n") {
		p.flushLogicalLine(part)
	}
}

func (p *proseWriter) flushLogicalLine(raw string) {
	line := strings.TrimRight(raw, " \t\r")
	trimmed := strings.TrimSpace(line)

	// (1) inside a tool block: swallow until the close tag — tolerate it glued to
	// the last body line, e.g. "cmd</run_command>".
	if p.inBlock {
		if trimmed == p.closeTag || strings.Contains(trimmed, p.closeTag) {
			p.inBlock = false
		}
		return
	}

	// (1b) reasoning tags — <think>/<thinking> and model-namespaced ones like
	// </mm:think>, possibly glued to other content. Drop the tag, toggle state;
	// reasoning text is dimmed (or a spinner when collapsed).
	if reReasonTag.MatchString(trimmed) {
		if reReasonClose.MatchString(trimmed) {
			p.thinking = false
			p.closeMarker()
		}
		if reReasonOpen.MatchString(trimmed) {
			p.thinking = true
		}
		rest := strings.TrimSpace(reReasonTag.ReplaceAllString(trimmed, " "))
		if rest == "" {
			if p.thinking && !p.showThink {
				p.beginReasoning()
			}
			return
		}
		if p.thinking {
			p.reasonLog = append(p.reasonLog, rest)
			if p.showThink {
				p.printLine(faint("%s", rest))
			} else {
				p.beginReasoning()
			}
			return
		}
		line, trimmed = rest, rest // remaining text is answer/tool content
	} else if p.thinking {
		if trimmed != "" {
			p.reasonLog = append(p.reasonLog, trimmed)
			if p.showThink {
				p.printLine(faint("%s", trimmed))
			} else {
				p.beginReasoning()
			}
		}
		return
	}

	// (1c) tool-tag suppression — on the (reasoning-stripped) line, so a tool tag
	// the model glued after a reasoning tag is still hidden and executed.
	if m := reOpen.FindStringSubmatch(trimmed); m != nil && blockNames[m[1]] {
		if m[3] != "/" && !voidTools[m[1]] {
			p.inBlock = true
			p.closeTag = "</" + m[1] + ">"
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
