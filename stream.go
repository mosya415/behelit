package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// markerLive is whether a waiting/thinking marker owns the terminal's current
// row. It is a package-level atomic and not a field because the view that prints
// a hazard line is not the one that owns the marker: Note/Warn/Error come from
// gwpolicy and the engine, and a gateway retry printed mid-wait used to land on
// the flame's row, putting two class glyphs on one line and breaking the one
// invariant the map column has.
//
// All it carries is "erase this row first". The spinner repaints in place every
// 90 ms anyway, so it redraws itself one row down on the next tick and there is
// nothing to stop and nothing to coordinate.
var markerLive atomic.Bool

// markerRowLead is what a line printed from another goroutine puts in front of
// itself to take the row back. Empty when there is no marker and when there is no
// cursor — piped, "\r\033[K" is a byte in somebody's payload.
func markerRowLead() string {
	if markerLive.Load() {
		return "\r\033[K"
	}
	return ""
}

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
	return fmt.Sprintf("%s"+gSep+"%d tok", d, atomic.LoadInt64(&p.tok))
}

// leadMarker opens the marker line with the torch gutter: the model is alive and
// nothing has been said yet. It is a different column class from the answer that
// follows (▌), because "still burning" and "here is the answer" are different
// facts and used to share one glyph.
func (p *proseWriter) leadMarker() {
	if !p.started {
		p.markerPrefix = " " + cDim + gTorch + cReset + " "
		p.started = true
	} else {
		p.markerPrefix = "   "
	}
	p.out("\n" + p.markerPrefix)
}

// flameTone lights the tall frames of the waiting flame hotter. It is decoration
// and states nothing: a flame that is briefly ember rather than torch says only
// that it is still burning.
func flameTone(frame string) string {
	switch frame {
	case "▄", "▅", "▆", "O":
		return cEmber
	}
	return cYellow
}

// markerText is the whole marker line's payload, measured so it cannot wrap. A
// wrapped marker is the one thing that ruins the scrollback: every later tick's
// "\r\033[K" would then repaint only the last visual row and leave a trail of
// half-erased flames behind it.
func (p *proseWriter) markerText(frame, label string) string {
	body := cFaint + label + gSep + "" + p.thinkStat() + cReset
	// measured against the page and not the terminal, so the marker line ends in
	// column houseWidth()+1 like every other line of chrome
	room := max(houseWidth()-visibleWidth(p.markerPrefix)-visibleWidth(frame), 0)
	// The guard is the marker itself and NOT a magic 12. At the clamp floor — a
	// 20-column pane, or an exported COLUMNS of 20 — the room is exactly 12, so a
	// `> 12` guard did not fire and a 68-column "waiting for <long model id>"
	// printed across four visual rows. Every 90 ms tick then repainted only the
	// last of them with "\r\033[K", leaving three rows of half-erased flames in the
	// scrollback permanently — which is the one failure this function exists to
	// prevent.
	if room > visibleWidth(gEllipsis) && visibleWidth(body) > room {
		body = cFaint + ellipsize(stripANSI(body), room) + cReset
	}
	return flameTone(frame) + frame + cReset + " " + body
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
	p.out(p.markerText(gSpinner[0], label))
	markerLive.Store(true)
	p.spinning = true
	p.spinStop = make(chan struct{})
	p.spinDone = make(chan struct{})
	prefix := p.markerPrefix
	go func() {
		defer close(p.spinDone)
		frames := gSpinner
		tk := time.NewTicker(90 * time.Millisecond)
		defer tk.Stop()
		i := 0
		for {
			select {
			case <-p.spinStop:
				return
			case <-tk.C:
				p.out("\r\033[K" + prefix + p.markerText(frames[(i+1)%len(frames)], label))
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
		// \r\033[K erases the row the marker is on, and only a terminal has a row
		// to erase. Piped — a one-shot redirected to a file, a `lca "task" | jq` —
		// there is no cursor and the escape simply landed in the payload, which is
		// somebody's input a week later. There the static marker stays where it was
		// printed and the answer follows it on its own line.
		if p.anim {
			p.out("\r\033[K")
		}
		markerLive.Store(false)
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
		// markerText and not a label built by hand: the line keeps the flame's
		// RESTING frame, so it does not change width at the moment it stops moving,
		// and the closing line is measured against the terminal the same way every
		// tick was.
		p.out("\r\033[K" + p.markerPrefix + p.markerText(gSpinner[0], done))
	}
	markerLive.Store(false)
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
			p.out("\n" + p.proseGutter())
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
			p.out("\n" + p.proseGutter())
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
		p.printFixed(cFaint + gVBar + " " + cReset + cCode + line + cFgOff)
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
		p.printFixed(l)
	}
}

// printLine emits one display line of the answer through the prose gutter,
// WRAPPED to the terminal so every visual row carries it. Unwrapped, the claim
// that you can find where the model spoke by scanning one character wide was
// false for the common case: a paragraph over the terminal's width lost the
// gutter on rows 2..n and the terminal broke it mid-word.
//
// Wrapping re-measures the line plain and so loses its styling; that is the
// cheaper loss, and only an over-long line pays it.
func (p *proseWriter) printLine(content string) { p.emit(content, true) }

// printFixed is printLine for a line that must NOT be re-flowed: a fenced code
// block keeps the file's own line breaks, and a rendered table would lose the
// columns it was just aligned into.
func (p *proseWriter) printFixed(content string) { p.emit(content, false) }

func (p *proseWriter) emit(content string, wrap bool) {
	switch {
	case !p.started:
		p.out("\n" + p.proseGutter())
		p.started = true
	case p.pendingNewline:
		p.out("\n" + p.proseGutter())
	}
	// Only on a screen. Piped, the answer is the payload — `lca -p "…" >> CHANGELOG.md`
	// — and re-flowing it would change somebody's bytes as surely as decorating it.
	rows := []string{content}
	if wrap && p.anim {
		// The answer is prose: it grows with the window up to a reading measure and
		// stops there. A 200-column line of text is harder to read than an 80-column
		// one, and the spare columns are spent on the tables and the paths instead.
		if room := proseWidth(); room > 24 && visibleWidth(content) > room {
			rows = wrapTo(stripANSI(content), room)
		}
	}
	for i, r := range rows {
		if i > 0 {
			p.out("\n" + p.proseGutter())
		}
		p.out(r)
	}
	p.pendingNewline = true
}

// proseGutter is the answer's own column — and nothing at all where there is no
// screen. plainTheme empties the palette but keeps the glyph tier, so a piped run
// still drew " ▌ " on every line of the answer: a decorative U+258C in the middle
// of a redirected payload, which is the one thing the brief forbids there.
func (p *proseWriter) proseGutter() string {
	if !p.anim {
		return " "
	}
	return " " + cYellow + gProse + cReset + " "
}
