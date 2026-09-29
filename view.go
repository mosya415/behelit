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

// checkLine renders one verifier run: ✓ go test ./...  1.2s
func checkText(cmd string, exit int, d time.Duration, attempt, attempts int) string {
	switch {
	case exit == 0:
		return cGreen + "✓" + cReset + " " + cmd + "  " + faint("%s", fmtDurShort(d))
	case attempt < attempts:
		return cRed + "✗" + cReset + " " + cmd + "  " + faint("exit %d · %s · output sent back, attempt %d/%d", exit, fmtDurShort(d), attempt+1, attempts)
	default:
		return cRed + "✗" + cReset + " " + cmd + "  " + faint("exit %d · %s", exit, fmtDurShort(d))
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
	case "edit", "write", "run_command":
		return // the approval prompt / live output stands in for a marker
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
			toolOK(fmt.Sprintf("verified and applied %s", plural(files, "file", "files")))
		case "unverified":
			toolInfo(fmt.Sprintf("%s changed, not applied — no check_cmd to verify it", plural(files, "file", "files")))
		case "not_applied":
			toolInfo(fmt.Sprintf("verified, not applied (%s)", plural(files, "file", "files")))
		case "conflict":
			toolErr("verified, but the diff no longer applies to your tree")
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

func (v *termView) Note(text string) { sayLine(" " + faint("%s %s", gNone, text)) }
func (v *termView) Warn(text string) { sayLine(" " + warn("%s", text)) }

// Error prints the failure in red and any following lines as hints.
func (v *termView) Error(text string) {
	first, rest, _ := strings.Cut(text, "\n")
	out := " " + cRed + gDown + " " + first + cReset
	for _, h := range strings.Split(rest, "\n") {
		if h != "" {
			out += "\n   " + faint("↳ %s", h)
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
	sayLine(" " + faint("%s verify", gNone) + "   " + checkText(cmd, exit, d, attempt, attempts))
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
			fmt.Println("   " + cGreen + "✓" + cReset + " " + faint("%s", x.Content))
		case "in_progress":
			fmt.Println("   " + cYellow + "▸" + cReset + " " + cBold + x.Content + cReset)
		case "cancelled":
			fmt.Println("   " + faint("✕ %s", x.Content))
		default:
			fmt.Println("   " + faint("○") + " " + x.Content)
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
//	┌ coder · t1 · qwen3-coder-480b-a35b-instruct
//	│ Sum in sum.go skips negative numbers; make it add all of them.
//	│ read    sum.go
//	│ ✓ go test ./...  1.2s
//	└ ● passed · 2.9s
//
// Parallel subagents interleave by line; the id on the frame lines and the
// tag on each inner line tell them apart.
func (v *childView) tag() string {
	return cFaint + v.s.ID + cReset + " "
}

func (v *childView) line(glyph, text string) {
	w := termWidth() - 10
	if w > 20 && visibleWidth(text) > w {
		text = ellipsize(stripANSI(text), w)
	}
	sayLine("   " + cFaint + glyph + cReset + " " + text)
}

func (v *childView) Stream() StreamView { return nullStream{} }

func (v *childView) ToolStart(name, summary string) {
	if v.quiet {
		return
	}
	v.line("│", v.tag()+faint("%-7s", toolVerb(name))+" "+summary)
}

func (v *childView) ToolDone(name string, args Args, res string) {
	if strings.HasPrefix(res, "error:") && !v.quiet {
		v.line("│", v.tag()+cRed+gDown+cReset+" "+faint("%s", summarize(strings.TrimSpace(strings.TrimPrefix(res, "error:")))))
	}
}

func (v *childView) Note(text string) {
	if !v.quiet {
		v.line("│", v.tag()+faint("%s", text))
	}
}
func (v *childView) Warn(text string)  { v.line("│", v.tag()+warn("%s", text)) }
func (v *childView) Error(text string) { v.line("│", v.tag()+cRed+text+cReset) }
func (v *childView) Check(cmd string, exit int, d time.Duration, attempt, attempts int) {
	v.line("│", v.tag()+checkText(cmd, exit, d, attempt, attempts))
}
func (v *childView) Perf(u Usage)    {}
func (v *childView) Todos(t []Todo)  {}
func (v *childView) Live() io.Writer { return nil }

func (v *childView) Begin() {
	head := cDim + v.s.agent.Name + cReset + faint(" · %s · %s", v.s.ID, v.s.client.Model())
	if v.quiet {
		head += faint(" · background")
	}
	v.line("┌", head)
	if v.quiet && v.s.title != "" { // a foreground call already showed its task on the line above
		v.line("│", v.tag()+faint("%s", v.s.title))
	}
}

func (v *childView) Finish(state string, d time.Duration) {
	v.line("└", v.tag()+statusWord(state)+faint(" · %s", fmtDurShort(d)))
}
