package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// View is how a session shows its work. The engine calls it and never prints
// directly, so the same loop drives the interactive primary (termView), quiet
// indented subagent lines (childView), and whatever comes next (a JSON event
// stream for workflow runners, a server).
type View interface {
	Stream() StreamView
	ToolStart(name, summary string)
	ToolDone(name string, args Args, result string)
	Note(text string)
	Warn(text string)
	Error(text string)
	Perf(u Usage)
	Todos(t []Todo)
	Live() io.Writer // live command output; nil = don't stream
	Begin()
	Finish(state string, d time.Duration)
	Check(cmd string, exit int, d time.Duration, attempt, attempts int) // a verifier run
}

// checkText renders one verifier run: go test ./...  PASS  1.2s
//
// The verdict is a WORD now. ✓ and ✗ were two more runes for an idea the tree
// already had two of, both charged a column they do not occupy, and neither
// survives a locale that never said UTF-8 — while PASS and FAIL are what an
// operator reads out loud, and the command they belong to is printed verbatim
// before them because the verifier, not the model, is what decided.
func checkText(cmd string, exit int, d time.Duration, attempt, attempts int) string {
	switch {
	case exit == 0:
		return cmd + "  " + cGreen + cBold + "PASS" + cReset + faint("  %s", fmtDurShort(d))
	case attempt < attempts:
		return cmd + "  " + cRed + cBold + "FAIL" + cReset + faint("  exit %d"+gSep+"%s"+gSep+"output sent back, attempt %d/%d", exit, fmtDurShort(d), attempt+1, attempts)
	default:
		return cmd + "  " + cRed + cBold + "FAIL" + cReset + faint("  exit %d"+gSep+"%s", exit, fmtDurShort(d))
	}
}

// StreamView receives one model call's deltas; End returns reasoning lines.
// Start is called when the request goes out, before anything comes back.
type StreamView interface {
	Start(model string)
	Sink() StreamSink
	End() []string
}

// outMu serializes terminal writes from concurrent sessions (parallel tools,
// subagents), so lines never interleave mid-line.
var outMu sync.Mutex

func sayLine(s string) {
	outMu.Lock()
	fmt.Println(s)
	outMu.Unlock()
}

// lockedWriter serializes live command output with other terminal writes.
type lockedWriter struct{ w io.Writer }

func (l lockedWriter) Write(b []byte) (int, error) {
	outMu.Lock()
	defer outMu.Unlock()
	return l.w.Write(b)
}

// ── primary: the full terminal experience ──────────────────────────────────

type termView struct{ s *Session }

func newTermView(s *Session) *termView { return &termView{s: s} }

type proseStream struct{ pw *proseWriter }

func (p proseStream) Sink() StreamSink {
	return StreamSink{Content: p.pw.feed, Reasoning: p.pw.feedReasoning, ToolArgs: p.pw.feedToolArgs, Discard: p.pw.discard}
}
func (p proseStream) Start(model string) { p.pw.begin(model) }
func (p proseStream) End() []string      { p.pw.end(); return p.pw.reasonLog }

func (v *termView) Stream() StreamView {
	return proseStream{newProseWriter(v.s.Raw, v.s.ShowThink)}
}

func (v *termView) ToolStart(name, summary string) {
	switch name {
	case "run_command":
		return // runCommand prints its own ^ header with the command on it
	case "todowrite":
		return // the list itself is printed on update
	}
	outMu.Lock()
	toolLine(name, summary)
	outMu.Unlock()
}

func (v *termView) ToolDone(name string, args Args, res string) {
	outMu.Lock()
	defer outMu.Unlock()
	if strings.HasPrefix(res, "error:") {
		toolErr(strings.TrimSpace(strings.TrimPrefix(res, "error:")))
		return
	}
	switch name {
	case "glob":
		if res == "no files found" {
			toolInfo("no files")
		} else {
			toolInfo(plural(lineCount(res), "file", "files"))
		}
	case "task":
		state := "completed"
		if i := strings.Index(res, `state="`); i >= 0 {
			if j := strings.IndexByte(res[i+7:], '"'); j >= 0 {
				state = res[i+7 : i+7+j]
			}
		}
		switch state {
		case "completed":
			toolOK("subagent finished")
		case "running":
			toolInfo("running in background")
		default:
			toolErr("subagent " + state)
		}
	case "delegate":
		var r delegateResult
		json.Unmarshal([]byte(res), &r)
		files := strings.Count(r.Diff, "\ndiff --git ")
		if strings.HasPrefix(r.Diff, "diff --git ") {
			files++
		}
		switch r.Status {
		case "passed":
			toolLoot(fmt.Sprintf("verified and applied %s", plural(files, "file", "files")))
		case "unverified":
			toolInfo(fmt.Sprintf("%s changed, not applied — no check_cmd to verify it", plural(files, "file", "files")))
		case "not_applied":
			toolInfo(fmt.Sprintf("verified, not applied (%s)", plural(files, "file", "files")))
		case "conflict":
			// Patch-era wording would be wrong under `apply: branch` — nothing was
			// patched and nothing failed to apply there, a real three-way merge
			// conflicted — and this one line is all the delegate row can hold. Where
			// the branch, the merge worktree and "the run stops here" go is
			// Session.tellTheHuman, which prints them from the orchestrator rather
			// than hoping the lead relays them.
			toolErr("verified, but it conflicts with your tree — nothing was written")
		default:
			toolErr(fmt.Sprintf("%s — %s", r.Status, summarize(lastLines(r.TestTail, 1, 200))))
		}
	case "skill":
		toolInfo("skill loaded")
	case "webfetch":
		toolInfo(byteCount(len(res)))
	case "todowrite":
	default:
		printOutcome(name, res)
	}
}

// Note, Warn and Error each take the row back from a live waiting marker before
// they print. A gateway retry or a fallback fires exactly while the model is
// being waited on, and appended to the flame's row it put two class glyphs on one
// line — which is the one line on the screen where the left column is not a map.
// markerRowLead erases that row; the spinner redraws itself one row down on its
// next tick, so the hazard line keeps the gutter to itself.
func (v *termView) Note(text string) { sayLine(markerRowLead() + " " + faint("%s %s", gNone, text)) }

// Warn carries the partial glyph, so "being handled" is distinguishable from
// "bookkeeping" (Note) and from "over" (Error) with no colour at all.
func (v *termView) Warn(text string) {
	sayLine(markerRowLead() + " " + cYellow + gPartial + " " + text + cReset)
}

// Error prints the failure in red and any following lines as hints.
func (v *termView) Error(text string) {
	first, rest, _ := strings.Cut(text, "\n")
	out := markerRowLead() + " " + cRed + gDown + " " + first + cReset
	// The hint lines wrap through wrapHint, which is now the one wrap hint() itself
	// uses too — the program was holding two positions on the same surface, and
	// this was the right one. These are hints.go's sentences about what to do next
	// and the longest of them is 109 columns, so the one screen that only ever
	// appears when something has already gone wrong was also the one that spilled.
	// The ↳ stays on the first row and the rest is indented under it; a hint that is
	// a single unbreakable command comes back whole so a paste still works.
	//
	// They are sentences, so they take the window up to the reading measure and
	// stop: a 200-column window would otherwise print the longest as one 195-column
	// line. The three-column lead plus the ↳ is four columns, so the measure is
	// houseWidth()-4 and the wrapped rows end where the frames do.
	room := min(houseWidth()-4, proseMax)
	for _, h := range strings.Split(rest, "\n") {
		if h == "" {
			continue
		}
		ls := wrapHint(h, room)
		for i, l := range ls {
			mark := gHint + " "
			if i > 0 {
				mark = strings.Repeat(" ", visibleWidth(gHint)+1)
			}
			out += "\n   " + faint("%s%s", mark, l)
		}
	}
	sayLine(out)
}
func (v *termView) Perf(u Usage) {
	outMu.Lock()
	printPerf(u)
	outMu.Unlock()
}
func (v *termView) Todos(t []Todo) {
	outMu.Lock()
	defer outMu.Unlock()
	printTodos(t)
}
func (v *termView) Check(cmd string, exit int, d time.Duration, attempt, attempts int) {
	sayLine(markerRowLead() + " " + cDim + gGate + cReset + " " + cDim + padTo("verify", 8, 0) + cReset + " " + checkText(cmd, exit, d, attempt, attempts))
}
func (v *termView) Live() io.Writer                      { return lockedWriter{os.Stdout} }
func (v *termView) Begin()                               {}
func (v *termView) Finish(state string, d time.Duration) {}

func printTodos(t []Todo) {
	if len(t) == 0 {
		fmt.Println(" " + faint("%s todo list cleared", gNone))
		return
	}
	done := 0
	for _, x := range t {
		if x.Status == "completed" {
			done++
		}
	}
	fmt.Println(" " + faint("%s TODO %d/%d", gNone, done, len(t)))
	for _, x := range t {
		switch x.Status {
		case "completed":
			fmt.Println("   " + cGreen + gUp + cReset + " " + faint("%s", x.Content))
		case "in_progress":
			fmt.Println("   " + cYellow + gCursor + cReset + " " + cBold + x.Content + cReset)
		case "cancelled":
			fmt.Println("   " + faint("%s %s", gDown, x.Content))
		default:
			fmt.Println("   " + faint("%s", gPending) + " " + x.Content)
		}
	}
}

// ── subagents: one indented line per action ─────────────────────────────────

type childView struct {
	s     *Session
	quiet bool // background: only start/finish and errors
}

func newChildView(s *Session, quiet bool) *childView { return &childView{s: s, quiet: quiet} }

type nullStream struct{}

func (nullStream) Start(string)     {}
func (nullStream) Sink() StreamSink { return StreamSink{} }
func (nullStream) End() []string    { return nil }

// Subagent output is a small tree under the call that started it:
//
//	> ┌ coder · t1 · qwen3-coder-480b-a35b-instruct
//	  │ t1 Sum in sum.go skips negative numbers; make it add all of them.
//	  │ t1 . read    sum.go
//	  = t1 go test ./...  PASS  1.2s
//	< └ t1 ● passed · 2.9s
//
// Parallel subagents interleave by line; the id on the frame lines and the
// tag on each inner line tell them apart.
func (v *childView) tag() string {
	return cFaint + v.s.ID + cReset + " "
}

func (v *childView) line(glyph, text string) { v.markLine(" ", glyph, text) }

// markLine is line with the outer map column filled in: > going down into the
// passage, < coming back out, blank while we are inside it. The spine itself
// stays a spine and is never boxed — a parallel subagent's first line would cut
// a box in half and it would never close.
func (v *childView) markLine(mark, glyph, text string) {
	// the spine's own room inside the page: 70 at a terminal of 80, as it always was
	w := max(70, houseWidth()-6)
	if w > 20 && visibleWidth(text) > w {
		text = ellipsize(stripANSI(text), w)
	}
	sayLine(" " + cYellow + mark + cReset + " " + cFaint + glyph + cReset + " " + text)
}

func (v *childView) Stream() StreamView { return nullStream{} }

func (v *childView) ToolStart(name, summary string) {
	if v.quiet {
		return
	}
	v.line(gVBar, v.tag()+toolGutterTone(name)+toolGutter(name)+cReset+" "+faint("%-7s", toolVerb(name))+" "+summary)
}

func (v *childView) ToolDone(name string, args Args, res string) {
	if strings.HasPrefix(res, "error:") && !v.quiet {
		v.line(gVBar, v.tag()+cRed+gDown+cReset+" "+faint("%s", summarize(strings.TrimSpace(strings.TrimPrefix(res, "error:")))))
	}
}

func (v *childView) Note(text string) {
	if !v.quiet {
		v.line(gVBar, v.tag()+faint("%s", text))
	}
}
func (v *childView) Warn(text string)  { v.line(gVBar, v.tag()+warn("%s", text)) }
func (v *childView) Error(text string) { v.line(gVBar, v.tag()+cRed+text+cReset) }
func (v *childView) Check(cmd string, exit int, d time.Duration, attempt, attempts int) {
	v.line(gGate, v.tag()+checkText(cmd, exit, d, attempt, attempts))
}
func (v *childView) Perf(u Usage)    {}
func (v *childView) Todos(t []Todo)  {}
func (v *childView) Live() io.Writer { return nil }

func (v *childView) Begin() {
	head := cDim + v.s.agent.Name + cReset + faint(gSep+"%s"+gSep+"%s", v.s.ID, v.s.client.Model())
	if v.quiet {
		head += faint("%s", gSep+"background")
	}
	v.markLine(gDeeper, gFrameTop, head)
	if v.quiet && v.s.title != "" { // a foreground call already showed its task on the line above
		v.line(gVBar, v.tag()+faint("%s", v.s.title))
	}
}

func (v *childView) Finish(state string, d time.Duration) {
	v.markLine(gOut, gFrameBot, v.tag()+statusWord(state)+faint(gSep+"%s", fmtDurShort(d)))
}
