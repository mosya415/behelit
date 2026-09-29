package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests for `lca report`. Two kinds: a trace built by hand from the very structs
// trace.go writes (so a renamed field breaks the test, not the page), and one
// real fake-gateway workflow run whose own trace is rendered end to end.

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const reportT0 = "2026-03-01T10:00:00Z"

func reportTS(t *testing.T, offset time.Duration) string {
	t.Helper()
	base, err := time.Parse(time.RFC3339, reportT0)
	if err != nil {
		t.Fatal(err)
	}
	return traceTS(base.Add(offset))
}

// hostileArgs is what a model is free to emit into a tool call: markup that
// would end the table it is printed in and start a script if anything on the
// page were interpolated raw.
const hostileArgs = `{"path":"</td></tr></table><script>alert(1)</script>","q":"\"><img src=x onerror=alert(2)>","c":"'--></style>"}`

// writeFixtureTrace lays down a trace with one root turn, one delegated
// subagent that emitted an unparsable call, a compaction helper, a task, two
// workflow steps, a gateway wait, a switch and a repeat — plus three lines no
// reader can make sense of.
func writeFixtureTrace(t *testing.T, dir string) string {
	t.Helper()
	root := "20260301-100000-777"
	sub := root + "-t1"
	exit0, exit2 := 0, 2
	lines := []string{
		mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 0), RootSession: root, Session: root,
			Role: "lead", Model: "lead-a", Step: 0, Usage: traceUsage{PromptTokens: 1000, CompletionTokens: 200, CachedTokens: 400},
			TTFTMs: 120, DurationMs: 3000, Finish: "tool_calls", Transport: "native", Tier: "premium", Member: "local",
			ToolCalls: []traceToolCall{{Name: "read_file", Args: `{"path":"main.go"}`, OK: true, ResultBytes: 4096, Ms: 12}}}),
		// the delegated subagent: one bad call, one failed call, and a raw reply
		mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 5*time.Second), RootSession: root, Session: sub, ParentSession: root,
			Role: "coder", Model: "coder-b", Step: 3, Usage: traceUsage{PromptTokens: 500, CompletionTokens: 100},
			TTFTMs: 90, DurationMs: 2000, Finish: "tool_calls", Transport: "text", Tier: "premium", Member: "gpu-1",
			InvalidCalls: 1, RawReply: "I will now <write> the file\n" + hostileArgs,
			ToolCalls: []traceToolCall{
				// an invalid call is also a failed one: execCall returns an
				// "error:" result for it, which is what sets ok=false (engine.go)
				{Name: "write", Args: hostileArgs, Invalid: true, Ms: 1,
					Error: "error: the write tool was called with invalid arguments: unexpected end of JSON input",
					Raw:   "<write>\n" + hostileArgs + "\n\x1b[31mbad\x1b[0m\xff"},
				{Name: "run_command", Args: `{"cmd":"go test ./..."}`, OK: false, Error: "exit status 1", ResultBytes: 220, Ms: 4500},
			},
			Fallbacks: []fallbackEvent{
				{From: "coder-a", Reason: "overloaded", WaitMs: 2000},
				{From: "coder-a", To: "coder-b", Reason: "not up"},
				{From: "coder-b", To: "coder-b", Reason: "cut after first byte"},
			}}),
		// the compaction helper carries no parent_session of its own
		mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 9*time.Second), RootSession: root, Session: root + "-compact",
			Role: "cheap", Model: "cheap-a", Step: 0, Usage: traceUsage{PromptTokens: 8000, CompletionTokens: 400, CachedTokens: 6000},
			TTFTMs: 40, DurationMs: 1000, Finish: "stop", Transport: "native", Member: "local", ToolCalls: []traceToolCall{}}),
		mustJSON(t, TaskRecord{Type: "task", TS: reportTS(t, 12*time.Second), RootSession: root, Session: sub, ParentSession: root,
			Role: "coder", Task: "make the parser drop trailing newlines", Status: "passed", CheckCmd: "go test ./parser/...",
			CheckExit: &exit0, Attempts: 2, DiffBytes: 1240, FilesChanged: 3, Applied: true, DurationMs: 11000,
			Reviewer: "reviewer", ReviewModel: "glm-a", ReviewVerdict: "approve", Member: "gpu-1", CallerMember: "local"}),
		mustJSON(t, TaskRecord{Type: "task", TS: reportTS(t, 13*time.Second), RootSession: root, Session: root + "-t2", ParentSession: root,
			Role: "coder", Task: "the one that did not work", Status: "failed", CheckCmd: "go build ./...",
			CheckExit: &exit2, Attempts: 3, DiffBytes: 0, FilesChanged: 0, DurationMs: 4000, Member: "local"}),
		mustJSON(t, StepRecord{Type: "step", TS: reportTS(t, 0), RootSession: root, Run: "ship-20260301-100000-777",
			Workflow: "ship", Step: "build", Index: 0, Kind: stepDelegate, TaskSession: sub, Role: "coder", Member: "gpu-1",
			Reviewer: "reviewer", Review: "approve", Model: "coder-b", Status: stepOK, Check: "go test ./...", CheckExit: &exit0,
			Checked: true, Attempts: 2, DurationMs: 11000, Usage: &traceUsage{PromptTokens: 500, CompletionTokens: 100},
			Detail: "delegate status passed"}),
		mustJSON(t, StepRecord{Type: "step", TS: reportTS(t, 14*time.Second), RootSession: root, Run: "ship-20260301-100000-777",
			Workflow: "ship", Step: "publish", Index: 1, Kind: stepRun, Member: "local", Status: stepFailed, Exit: 1,
			DurationMs: 900, Detail: "exit 1"}),
		// a record type this build does not know: counted, never rendered as fact
		`{"type":"weather","ts":"` + reportTS(t, 15*time.Second) + `"}`,
		// two unreadable lines — truncated JSON and not JSON at all — plus a line
		// longer than reportMaxLine, which is a different fact and counted apart
		`{"type":"turn","ts":"2026-03-01T10:00:1`,
		`this is not json`,
		`{"type":"turn","junk":"` + strings.Repeat("A", reportMaxLine+64) + `"}`,
	}
	p := filepath.Join(dir, "20260301-100000-777.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func buildFixtureReport(t *testing.T) (*report, string) {
	t.Helper()
	dir := t.TempDir()
	p := writeFixtureTrace(t, dir)
	rep, err := buildReport([]string{p}, "20260301-100000-777")
	if err != nil {
		t.Fatal(err)
	}
	return rep, p
}

func renderFixture(t *testing.T) (*report, string) {
	t.Helper()
	rep, p := buildFixtureReport(t)
	out := filepath.Join(filepath.Dir(p), "r.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return rep, string(b)
}

// The numbers the summary header prints are the numbers the trace says.
func TestReportTotals(t *testing.T) {
	rep, html := renderFixture(t)

	if rep.Turns != 3 {
		t.Errorf("turns = %d, want 3", rep.Turns)
	}
	if rep.In != 9500 || rep.Out != 700 || rep.Cached != 6400 {
		t.Errorf("tokens = %d in / %d out / %d cached, want 9500/700/6400", rep.In, rep.Out, rep.Cached)
	}
	// an invalid call is a failed call too, so 2 of the 3 did not return a result
	if rep.Calls != 3 || rep.CallErrs != 2 || rep.Invalid != 1 {
		t.Errorf("calls = %d (%d failed, %d invalid), want 3/2/1", rep.Calls, rep.CallErrs, rep.Invalid)
	}
	// attempted = parsed calls + invalid calls that never became one; here every
	// invalid call did parse into an entry, so attempted == calls. The same
	// formula lca eval uses (scoreTrace), so the two commands cannot disagree.
	if rep.Attempted != 3 {
		t.Errorf("attempted = %d, want 3", rep.Attempted)
	}
	if rep.Passed != 1 || rep.Failed != 1 || rep.TaskCount != 2 {
		t.Errorf("tasks = %d passed / %d failed of %d", rep.Passed, rep.Failed, rep.TaskCount)
	}
	if rep.StepCount != 2 || len(rep.runOrder) != 1 {
		t.Errorf("steps = %d in %d runs, want 2 in 1", rep.StepCount, len(rep.runOrder))
	}
	if rep.Waits != 1 || rep.Switches != 1 || rep.Repeats != 1 || rep.Falls != 3 {
		t.Errorf("fallbacks = %d (%d wait, %d switch, %d repeat), want 3/1/1/1", rep.Falls, rep.Waits, rep.Switches, rep.Repeats)
	}
	if rep.Unknown != 1 {
		t.Errorf("unknown records = %d, want 1", rep.Unknown)
	}
	// 10:00:00 → the last record's own timestamp. Only a turn is stamped at its
	// start, so only a turn's duration reaches forward; a task and a step are
	// written when they finish, and adding their duration would report a run that
	// ends after the last thing that happened in it.
	if want := 15 * time.Second; rep.wall() != want {
		t.Errorf("wall clock = %s, want %s", rep.wall(), want)
	}

	for _, want := range []string{
		">9.5k<",  // tokens in
		">700<",   // tokens out
		">67.4%<", // cache hit: 6400 / 9500
		">33.3%<", // invalid share: 1 of 3 attempted
		">1 / 1<", // tasks passed / failed
		">15.0s<", // wall clock: the last record to end closes the run
		"1 wait · 1 switch · 1 repeat",
		"2026-03-01 10:00:00Z", // the range the page says it covers
		"20260301-100000-777.jsonl",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// the session tree, including the two shapes that are easy to lose
	for _, want := range []string{"20260301-100000-777-t1", "20260301-100000-777-compact", "compaction", "delegation"} {
		if !strings.Contains(html, want) {
			t.Errorf("page is missing session %q", want)
		}
	}
	// no cost anywhere in the trace, so no cost on the page
	if rep.HasCost {
		t.Error("HasCost with no cost_usd in the trace")
	}
	if !strings.Contains(html, "no record in this trace carries a cost") {
		t.Error("the page must say the cost is unknown, not print a number")
	}
	if strings.Contains(html, "$0.0000") {
		t.Error("the page invented a cost")
	}
}

// A field the records do not carry renders as an em dash, never as a zero.
func TestReportMissingFieldsAreDashes(t *testing.T) {
	dir := t.TempDir()
	// A turn with no tier, no finish reason, no ttft and no member.
	line := mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 0), RootSession: "s", Session: "s",
		Role: "lead", Model: "m", Usage: traceUsage{PromptTokens: 10}, ToolCalls: []traceToolCall{}})
	p := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "t")
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	out := filepath.Join(dir, "t.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	sb.Write(b)
	html := sb.String()
	if !strings.Contains(html, "—") {
		t.Fatal("no em dash on a page whose trace is missing most fields")
	}
	// cached 0 of a prompt of 10 is a measured 0%, but a cached share of a turn
	// with no prompt tokens at all would be a dash — check the dash wins where
	// there is genuinely nothing to divide.
	if got := pct(0, 0); got != "—" {
		t.Errorf("pct(0,0) = %q, want an em dash", got)
	}
	if got := pct(0, 10); got != "0.0%" {
		t.Errorf("pct(0,10) = %q, want a measured zero", got)
	}
	if got := msOrDash(0); got != "—" {
		t.Errorf("msOrDash(0) = %q, want an em dash", got)
	}
}

// The raw text of an invalid call is on the page, and it is escaped.
func TestReportInvalidCallRawTextEscaped(t *testing.T) {
	_, html := renderFixture(t)

	if !strings.Contains(html, "&lt;write&gt;") {
		t.Error("the raw text the model emitted is not on the page")
	}
	if !strings.Contains(html, "I will now &lt;write&gt; the file") {
		t.Error("the reply the invalid call came in is not on the page")
	}
	// the escape trip: the raw text also carried an escape sequence and a byte
	// that is not UTF-8 at all
	if strings.Contains(html, "\x1b") {
		t.Error("an ANSI escape from a model reached the page verbatim")
	}
	if strings.Contains(html, "\xff") {
		t.Error("an invalid UTF-8 byte from a model reached the page verbatim")
	}
	if !strings.Contains(html, "�") {
		t.Error("the replacement character for those bytes is missing")
	}
}

// A hostile tool argument cannot get out of the markup it is printed in.
func TestReportEscapesHostileToolArguments(t *testing.T) {
	_, html := renderFixture(t)

	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the hostile argument is not on the page at all")
	}
	for _, bad := range []string{"<script", "<img", "onerror=\"", "<iframe", "javascript:"} {
		if strings.Contains(html, bad) {
			t.Errorf("unescaped %q reached the page", bad)
		}
	}
	// the model wrote "--></style>" — the page's own one stylesheet must still
	// be the only thing that closes a style element
	if n := strings.Count(html, "</style>"); n != 1 {
		t.Errorf("%d </style> in the page, want exactly the stylesheet's own", n)
	}
	// the table-breaking prefix must survive only in escaped form
	if !strings.Contains(html, "&lt;/td&gt;&lt;/tr&gt;&lt;/table&gt;") {
		t.Error("the markup-breaking prefix was not escaped as text")
	}
	if strings.Contains(html, `"><img`) {
		t.Error("an attribute breakout reached the page")
	}
	// every quote a model wrote is a numeric entity, so no attribute can end early
	if strings.Contains(html, `alert(2)>`) {
		t.Error("an attribute value from a model closed its tag")
	}
	if n := strings.Count(html, "<html"); n != 1 {
		t.Errorf("%d <html elements, want exactly 1", n)
	}
	if !strings.HasSuffix(strings.TrimSpace(html), "</html>") {
		t.Error("the page does not end where it should — something closed it early")
	}
}

// esc is the single gate everything hostile passes through, so test it directly.
func TestReportEsc(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"a<b>&c", "a&lt;b&gt;&amp;c"},
		{`"x" 'y'`, "&#34;x&#34; &#39;y&#39;"},
		{"keep\nthe\tshape", "keep\nthe\tshape"},
		{"drop\rthe\rcarriage", "dropthecarriage"},
		{"bell\ahere", "bell�here"},
		{"\x1b[31mred", "�[31mred"},
		{"bad\xffbyte", "bad�byte"},
		{"c1\u0085next", "c1�next"},
		{"emoji ok 🜏", "emoji ok 🜏"},
	} {
		if got := esc(c.in); got != c.want {
			t.Errorf("esc(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A line no reader can parse is counted and named on the page, never dropped —
// and a line that is merely too long to hold is counted as its own thing, because
// it may be a flawless record and "would not parse" would send its reader
// looking for damage that is not there.
func TestReportCountsUnreadableLines(t *testing.T) {
	rep, html := renderFixture(t)

	if rep.Bad != 2 || rep.Oversize != 1 {
		t.Fatalf("lines = %d unparsable + %d over-long, want 2 + 1", rep.Bad, rep.Oversize)
	}
	if len(rep.Sources) != 1 || rep.Sources[0].Bad != 2 || rep.Sources[0].Oversize != 1 {
		t.Fatalf("the per-file count disagrees: %+v", rep.Sources)
	}
	if !strings.Contains(html, "1 over 256.0KB, skipped") {
		t.Error("the page does not say a record was skipped for its size")
	}
	// the readable records are all still there: a corrupt line must not end the scan
	if rep.Turns != 3 || rep.TaskCount != 2 || rep.StepCount != 2 {
		t.Fatalf("a corrupt line swallowed later records: %d turns, %d tasks, %d steps", rep.Turns, rep.TaskCount, rep.StepCount)
	}
	if !strings.Contains(html, "2 unreadable") {
		t.Error("the page does not report the unreadable lines")
	}
	if !strings.Contains(html, "JSONL lines that would not parse") {
		t.Error("the page does not explain what the unreadable count is")
	}
}

// An over-long line is reported once, as over-long, and does not end the scan.
func TestReportEachLineBoundsOneLine(t *testing.T) {
	in := "a\n" + strings.Repeat("B", reportMaxLine+1) + "\nc\n\n" + strings.Repeat("D", reportMaxLine/2) + "\ne"
	var got []string
	over := 0
	if err := eachLine(strings.NewReader(in), func(line []byte, tooLong bool) {
		if tooLong {
			over++
			return
		}
		got = append(got, string(line[:min(len(line), 1)])+strconv.Itoa(len(line)))
	}); err != nil {
		t.Fatal(err)
	}
	if over != 1 {
		t.Errorf("over-long lines reported = %d, want 1", over)
	}
	want := []string{"a1", "c1", "D" + strconv.Itoa(reportMaxLine/2), "e1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("lines = %v, want %v", got, want)
	}
}

// The page is one file: nothing it needs comes off a network.
func TestReportHasNoExternalReferences(t *testing.T) {
	_, html := renderFixture(t)
	for _, bad := range []string{"http://", "https://", "<script src", "<script", "<link ", "@import", "url(", "//cdn", "srcset", "<iframe"} {
		if strings.Contains(html, bad) {
			t.Errorf("the page references something outside itself: %q", bad)
		}
	}
	if !strings.Contains(html, "<style>") || !strings.Contains(html, "<svg ") {
		t.Error("the page should carry its own stylesheet and draw its chart inline")
	}
	if !strings.Contains(html, "<details") {
		t.Error("the collapsible sections are the only interactivity allowed, and they are missing")
	}
}

// resolveTrace: a bare `lca report` finds the newest trace; the three argument
// shapes each resolve; anything else fails by saying what it tried.
func TestReportResolveTrace(t *testing.T) {
	home := t.TempDir()
	cfg := Config{Dir: home}
	traces := filepath.Join(home, "traces")
	if err := os.MkdirAll(traces, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveTrace(cfg, ""); err == nil {
		t.Error("an empty traces directory must be an error, not an empty report")
	}
	old := filepath.Join(traces, "20260301-100000-1.jsonl")
	newer := filepath.Join(traces, "20260101-100000-2.jsonl")
	for _, p := range []string{old, newer} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// newest by modification time, deliberately not by name: the file last
	// written to is the run you want, even when it started first.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	got, name, err := resolveTrace(cfg, "")
	if err != nil || len(got) != 1 || got[0] != newer {
		t.Fatalf("default = %v %q (%v), want %s", got, name, err, newer)
	}
	if got, _, err := resolveTrace(cfg, old); err != nil || got[0] != old {
		t.Fatalf("a path argument must be used as is: %v %v", got, err)
	}
	if got, _, err := resolveTrace(cfg, "20260301-100000-1"); err != nil || got[0] != old {
		t.Fatalf("a session id must resolve inside traces/: %v %v", got, err)
	}
	if _, _, err := resolveTrace(cfg, "20260301-100000-9"); err == nil {
		t.Error("a session id with no trace must fail")
	}
	if _, _, err := resolveTrace(cfg, "nonsense"); err == nil {
		t.Error("an argument that is none of the three shapes must fail")
	}

	// a run id yields every trace the run wrote, because a resumed run wrote one
	// per process and reporting on the last would hide the first's work
	runID := "ship-20260301-100000-1"
	rdir := filepath.Join(home, "runs", runID)
	if err := os.MkdirAll(rdir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := &WorkflowState{Version: wfStateVersion, Run: runID, Workflow: "ship", Traces: []string{old, newer}}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(rdir, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, name, err = resolveTrace(cfg, runID)
	if err != nil || len(got) != 2 || name != runID {
		t.Fatalf("run id = %v %q (%v), want both traces", got, name, err)
	}
}

// With no -out the report lands next to its trace and the path is all stdout
// carries, so `lca report | xargs open` works.
func TestReportCommandWritesNextToTheTrace(t *testing.T) {
	home := t.TempDir()
	traces := filepath.Join(home, "traces")
	if err := os.MkdirAll(traces, 0o700); err != nil {
		t.Fatal(err)
	}
	p := writeFixtureTrace(t, traces)
	cfg := Config{Dir: home}
	var code int
	out := captureStdout(t, func() { code = runReport(cfg, nil) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := strings.TrimSuffix(p, ".jsonl") + ".html"
	if strings.TrimSpace(out) != want {
		t.Fatalf("stdout = %q, want just %q", out, want)
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "<!doctype html>") {
		t.Fatalf("not an HTML file: %q", truncate(string(b), 60))
	}
	// -out puts it where it is told
	elsewhere := filepath.Join(home, "sub", "report.html")
	captureStdout(t, func() { code = runReport(cfg, []string{"-out", elsewhere}) })
	if code != 0 {
		t.Fatalf("-out exit %d", code)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatalf("-out did not write it: %v", err)
	}
	// a trace that is not there is an error, not an empty page
	if c := runReport(cfg, []string{filepath.Join(home, "nope.jsonl")}); c == 0 {
		t.Error("reporting on a missing trace must fail")
	}
}

// A cost the trace does carry is summed and shown; nothing invents one.
func TestReportShowsCostOnlyWhenTheTraceCarriesIt(t *testing.T) {
	dir := t.TempDir()
	base := mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 0), RootSession: "s", Session: "s", Role: "lead",
		Model: "m", Usage: traceUsage{PromptTokens: 100}, ToolCalls: []traceToolCall{}})
	// the same record with a cost a future writer appended
	withCost := strings.TrimSuffix(base, "}") + `,"cost_usd":0.125}`
	p := filepath.Join(dir, "c.jsonl")
	if err := os.WriteFile(p, []byte(withCost+"\n"+withCost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "c")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasCost || rep.Cost != 0.25 {
		t.Fatalf("cost = %v (has=%v), want 0.25", rep.Cost, rep.HasCost)
	}
	out := filepath.Join(dir, "c.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "$0.2500") {
		t.Error("a cost the trace carried is missing from the page")
	}
}

// Caps are stated where they truncate, and the totals still count what they hid.
func TestReportCapsAreStated(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < reportTurnRows+7; i++ {
		lines = append(lines, mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, time.Duration(i)*time.Second),
			RootSession: "s", Session: "s", Role: "lead", Model: "m", Step: i,
			Usage: traceUsage{PromptTokens: 1}, Transport: "native", Member: "local", ToolCalls: []traceToolCall{}}))
	}
	p := filepath.Join(dir, "big.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "big")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Turns != reportTurnRows+7 || rep.In != reportTurnRows+7 {
		t.Fatalf("the totals must count every turn, capped or not: %d turns, %d tokens", rep.Turns, rep.In)
	}
	s := rep.sessions["s"]
	if len(s.rows) != reportTurnRows || s.rowsDropped != 7 {
		t.Fatalf("detail rows = %d, dropped = %d", len(s.rows), s.rowsDropped)
	}
	out := filepath.Join(dir, "big.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "7 further turns of this session are counted in the totals but not listed (cap 300)") {
		t.Error("the page truncated a list without saying so")
	}
	// the chart is bucketed, so it does not grow a rect per turn
	if n := strings.Count(string(b), "<rect"); n > reportBuckets+8 {
		t.Errorf("%d rects for %d turns — the chart is not bucketed", n, rep.Turns)
	}
}

// A trace claiming a parent that is really a descendant must not hang the
// renderer; the session is shown at top level instead.
func TestReportSurvivesASessionLoop(t *testing.T) {
	dir := t.TempDir()
	turn := func(sess, parent string) string {
		return mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 0), RootSession: "a", Session: sess,
			ParentSession: parent, Role: "r", Model: "m", Transport: "native", Member: "local", ToolCalls: []traceToolCall{}})
	}
	p := filepath.Join(dir, "loop.jsonl")
	if err := os.WriteFile(p, []byte(turn("a", "b")+"\n"+turn("b", "a")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "loop")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		out := filepath.Join(dir, "loop.html")
		if err := writeReport(out, rep); err != nil {
			done <- ""
			return
		}
		b, _ := os.ReadFile(out)
		done <- string(b)
	}()
	select {
	case html := <-done:
		if !strings.Contains(html, ">a ") && !strings.Contains(html, "a<") {
			t.Error("the looping sessions are missing from the page")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rendering a session loop did not finish")
	}
}

// End to end on a trace nothing in this file wrote: a real workflow run against
// the fake gateway, rendered.
func TestReportOnARealRun(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			if strings.Contains(req.Body, `"role":"tool"`) {
				return fakeReply{content: "created done.txt"}
			}
			return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "done.txt", "content": "ok\n"})}}
		}
		return fakeReply{content: "ok"}
	})
	h, dir := wfHarness(t, fs)
	code, _, _ := runWF(t, h, dir, "steps:\n  build:\n    delegate: create done.txt\n    role: coder\n  show:\n    run: echo done\n", nil)
	if code != 0 {
		t.Fatalf("the run itself failed: exit %d", code)
	}
	rep, err := buildReport([]string{h.orch.tracer.Path}, "real")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Bad != 0 {
		t.Fatalf("%d unreadable lines in a trace this binary just wrote", rep.Bad)
	}
	if rep.Turns == 0 || rep.TaskCount == 0 || rep.StepCount == 0 {
		t.Fatalf("the report missed a record kind: %d turns, %d tasks, %d steps", rep.Turns, rep.TaskCount, rep.StepCount)
	}
	out := filepath.Join(t.TempDir(), "real.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{"coder", "coder-a", "write", "delegate", "passed", "done.txt", "local"} {
		if !strings.Contains(html, want) {
			t.Errorf("the page is missing %q from a real run", want)
		}
	}
	for _, bad := range []string{"http://", "https://", "<script"} {
		if strings.Contains(html, bad) {
			t.Errorf("a real run's report references %q", bad)
		}
	}
}

// The documented order is `lca report <what> -out file.html`, and flag.Parse
// stops at the first non-flag, so the bare argument has to come off the front
// first — the way `lca run <name> -resume` does it.
func TestReportTakesFlagsAfterTheArgument(t *testing.T) {
	home := t.TempDir()
	traces := filepath.Join(home, "traces")
	if err := os.MkdirAll(traces, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureTrace(t, traces)
	cfg := Config{Dir: home}
	sess := "20260301-100000-777"

	dst := filepath.Join(home, "documented.html")
	var code int
	captureStdout(t, func() { code = runReport(cfg, []string{sess, "-out", dst}) })
	if code != 0 {
		t.Fatalf("`lca report <session> -out f` exited %d", code)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("it wrote nothing: %v", err)
	}
	// the other order keeps working: nobody's muscle memory breaks either way
	flagsFirst := filepath.Join(home, "flags-first.html")
	captureStdout(t, func() { code = runReport(cfg, []string{"-out", flagsFirst, sess}) })
	if code != 0 {
		t.Fatalf("`lca report -out f <session>` exited %d", code)
	}
	// two traces at once is still a usage error, not a silently ignored argument
	if c := runReport(cfg, []string{sess, "extra"}); c != 2 {
		t.Errorf("two positionals exited %d, want the usage error", c)
	}
}

// Only a turn is stamped at its start, so only a turn's duration reaches
// forward. A task or a step whose duration is added to its own end timestamp
// reports a run as longer than it was — on the page's most prominent number.
func TestReportDoesNotStretchEndStampedRecords(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 0), RootSession: "s", Session: "s", Role: "lead",
			Model: "m", DurationMs: 1000, Transport: "native", Member: "local", ToolCalls: []traceToolCall{}}),
		// a 30-minute delegation, recorded when it finished
		mustJSON(t, TaskRecord{Type: "task", TS: reportTS(t, 30*time.Minute), RootSession: "s", Session: "s-t1",
			ParentSession: "s", Role: "coder", Task: "long one", Status: "passed", Attempts: 1,
			DurationMs: 30 * 60 * 1000, Member: "local"}),
	}
	p := filepath.Join(dir, "stamp.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "stamp")
	if err != nil {
		t.Fatal(err)
	}
	if want := 30 * time.Minute; rep.wall() != want {
		t.Fatalf("wall clock = %s, want %s — the records span exactly that", rep.wall(), want)
	}
	out := filepath.Join(dir, "stamp.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	html := string(b)
	if !strings.Contains(html, "<span>10:30:00</span>") {
		t.Error("the activity axis does not end where the run does")
	}
	if strings.Contains(html, "11:00:00") {
		t.Error("the page invented half an hour after the last record")
	}
}

// The cap note counts sessions, not lookups of them: a session past the cap is
// looked up once per turn it took, and multiplying the two is a number the trace
// never contained.
func TestReportCountsDroppedSessionsNotLookups(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < reportSessions+2; i++ {
		uid := "s" + strconv.Itoa(i)
		for turn := 0; turn < 3; turn++ {
			lines = append(lines, mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, time.Duration(i)*time.Millisecond),
				RootSession: uid, Session: uid, Role: "lead", Model: "m", Step: turn,
				Usage: traceUsage{PromptTokens: 1}, Transport: "native", Member: "local", ToolCalls: []traceToolCall{}}))
		}
	}
	p := filepath.Join(dir, "many.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "many")
	if err != nil {
		t.Fatal(err)
	}
	if rep.sessDropped != 2 {
		t.Fatalf("dropped sessions = %d, want 2 (not 6, which is 2 sessions × 3 turns)", rep.sessDropped)
	}
	out := filepath.Join(dir, "many.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	html := string(b)
	if !strings.Contains(html, "2 further sessions are counted") {
		t.Error("the page does not state how many sessions it hid")
	}
	// and the tile carries both halves, so the two cannot be added into a total
	// the trace never had
	if !strings.Contains(html, strconv.Itoa(reportSessions)+" of "+strconv.Itoa(reportSessions+2)+" sessions") {
		t.Error("the turns tile does not say how many of the trace's sessions are shown")
	}
}

// One file listed twice is one file. A run's state appends a trace per process
// (attachRun) and with $LCA_TRACE set those are all the same path, so scanning
// the list as given would double every number on the page.
func TestReportScansOneFileOnce(t *testing.T) {
	dir := t.TempDir()
	p := writeFixtureTrace(t, dir)
	once, err := buildReport([]string{p}, "once")
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "same.jsonl")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	// three spellings of one file: repeated, dot-segmented, and through a symlink
	twice, err := buildReport([]string{p, p, dir + "/./" + filepath.Base(p), link}, "twice")
	if err != nil {
		t.Fatal(err)
	}
	if len(twice.Sources) != 1 {
		t.Fatalf("sources = %d, want the one file", len(twice.Sources))
	}
	if twice.Turns != once.Turns || twice.In != once.In || twice.TaskCount != once.TaskCount || twice.Calls != once.Calls {
		t.Fatalf("the same trace twice = %d turns / %d in / %d tasks / %d calls, want %d / %d / %d / %d",
			twice.Turns, twice.In, twice.TaskCount, twice.Calls, once.Turns, once.In, once.TaskCount, once.Calls)
	}
}

// A record whose two invalid-call figures disagree (an older or foreign writer)
// must not produce a denominator smaller than the calls listed under it.
func TestReportInvalidCountsNeverUndercountAttempts(t *testing.T) {
	dir := t.TempDir()
	line := mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 0), RootSession: "s", Session: "s", Role: "lead",
		Model: "m", Transport: "native", Member: "local", InvalidCalls: 0,
		ToolCalls: []traceToolCall{
			{Name: "write", Args: "{", Invalid: true},
			{Name: "write", Args: "}", Invalid: true},
		}})
	p := filepath.Join(dir, "skew.jsonl")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "skew")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Invalid != 2 || rep.Attempted != 2 {
		t.Fatalf("invalid = %d of %d attempted, want 2 of 2 — the calls are right there in the record", rep.Invalid, rep.Attempted)
	}
	out := filepath.Join(dir, "skew.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "2 of 2 attempted") {
		t.Error("the header contradicts the invalid calls listed below it")
	}
}

// A session's task list is capped like its other lists, and says so. Its sibling
// — the global table — states a cap, which is what makes this the list a reader
// would otherwise wrongly trust.
func TestReportCapsASessionsTaskList(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < reportTaskRows+3; i++ {
		lines = append(lines, mustJSON(t, TaskRecord{Type: "task", TS: reportTS(t, time.Duration(i)*time.Second),
			RootSession: "s", Session: "s-t1", ParentSession: "s", Role: "coder", Task: "one of many",
			Status: "passed", Attempts: 1, Applied: true, Member: "local"}))
	}
	p := filepath.Join(dir, "tasks.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "tasks")
	if err != nil {
		t.Fatal(err)
	}
	s := rep.sessions["s-t1"]
	if len(s.tasks) != reportTaskRows || s.tasksDropped != 3 {
		t.Fatalf("session task rows = %d, dropped = %d", len(s.tasks), s.tasksDropped)
	}
	if rep.TaskCount != reportTaskRows+3 {
		t.Fatalf("the totals must count every task, capped or not: %d", rep.TaskCount)
	}
	out := filepath.Join(dir, "tasks.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	// twice: the global table says it, and now so does the session's own list
	if n := strings.Count(string(b), "3 further tasks are counted in the totals but not listed (cap 200)"); n != 2 {
		t.Errorf("%d task lists state their cap, want both of them", n)
	}
}

// The row budget is the page's, not only each session's: 4000 sessions at 300
// rows apiece is a file no browser opens, and a cap that only ever bounds one
// session bounds nothing.
func TestReportBudgetsRowsAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	sessions := reportTotalTurnRows/reportTurnRows + 1
	var lines []string
	for i := 0; i < sessions; i++ {
		uid := "s" + strconv.Itoa(i)
		for turn := 0; turn < reportTurnRows; turn++ {
			lines = append(lines, mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, time.Duration(turn)*time.Second),
				RootSession: uid, Session: uid, Role: "lead", Model: "m", Step: turn,
				Usage: traceUsage{PromptTokens: 1}, Transport: "native", Member: "local", ToolCalls: []traceToolCall{}}))
		}
	}
	p := filepath.Join(dir, "fleet.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "fleet")
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, s := range rep.sessions {
		kept += len(s.rows)
	}
	if kept != reportTotalTurnRows {
		t.Fatalf("the page kept %d turn rows, want its budget of %d", kept, reportTotalTurnRows)
	}
	want := sessions*reportTurnRows - reportTotalTurnRows
	if rep.turnsCut != want {
		t.Fatalf("rows cut by the page budget = %d, want %d", rep.turnsCut, want)
	}
	if rep.Turns != sessions*reportTurnRows {
		t.Fatalf("the totals must count every turn the budget hid: %d", rep.Turns)
	}
	out := filepath.Join(dir, "fleet.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "the page's row budget stopped "+strconv.Itoa(want)+" turn rows") {
		t.Error("the page thinned itself without saying so")
	}
}

// A negative duration formats in the same units as a positive one. It reaches
// the page from any duration_ms the report did not compute itself.
func TestReportFmtSpanIgnoresTheSign(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{-3 * time.Minute, "3m00s"},
		{-2 * time.Hour, "2h00m"},
		{-1500 * time.Millisecond, "1.5s"},
		{-250 * time.Millisecond, "250ms"},
		{3 * time.Minute, "3m00s"},
	} {
		if got := fmtSpan(c.d); got != c.want {
			t.Errorf("fmtSpan(%s) = %q, want %q", c.d, got, c.want)
		}
	}
	if got := fmtMs(-180000); got != "3m00s" {
		t.Errorf("fmtMs(-180000) = %q, want %q", got, "3m00s")
	}
}

// Only a `run:` step ran a process, so only a `run:` step has an exit status.
func TestReportStepExitOnlyWhereAProcessRan(t *testing.T) {
	for _, c := range []struct {
		kind string
		exit int
		want string
	}{
		{stepRun, 0, "0"},
		{stepRun, 1, "1"},
		{stepPrompt, 0, "—"},
		{stepDelegate, 0, "—"},
		{stepPrompt, -1, "-1"}, // a step that could not start says so
	} {
		if got := stepExit(StepRecord{Kind: c.kind, Exit: c.exit}); got != c.want {
			t.Errorf("stepExit(%s, %d) = %q, want %q", c.kind, c.exit, got, c.want)
		}
	}
	// and on the page: the fixture's delegate step carries exit 0 and check exit
	// 0, and only the second of those is a status anything reported
	_, html := renderFixture(t)
	if !strings.Contains(html, `<td class="n">—</td><td class="n">0</td>`) {
		t.Error("a delegate step prints an exit status for a process it never ran")
	}
}

// An overnight run is this project's normal case, and "23:00:00 → 00:06:01"
// reads as a run going backwards.
func TestReportDatesTimesWhenARunCrossesMidnight(t *testing.T) {
	dir := t.TempDir()
	turn := func(off time.Duration) string {
		return mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, off), RootSession: "s", Session: "s",
			Role: "lead", Model: "m", DurationMs: 1000, Transport: "native", Member: "local", ToolCalls: []traceToolCall{}})
	}
	p := filepath.Join(dir, "night.jsonl")
	if err := os.WriteFile(p, []byte(turn(13*time.Hour)+"\n"+turn(14*time.Hour+6*time.Minute)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "night")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "night.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	html := string(b)
	if !strings.Contains(html, "2026-03-01 23:00:00Z → 2026-03-02 00:06:01Z") {
		t.Error("the session window does not say which day each end of it is on")
	}
	if !strings.Contains(html, "<span>2026-03-02 00:06:01Z</span>") {
		t.Error("the activity axis drops the day it crossed")
	}
}

// A cache figure the gateway never reported is not a measured cold cache: a
// prefix that looks unstable is an evening of hunting, and the trace said
// nothing at all.
func TestReportSeparatesNoCacheFigureFromAColdCache(t *testing.T) {
	dir := t.TempDir()
	line := mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 0), RootSession: "s", Session: "s", Role: "lead",
		Model: "m", Usage: traceUsage{PromptTokens: 40000, CompletionTokens: 100}, DurationMs: 1000,
		Transport: "native", Member: "local", ToolCalls: []traceToolCall{}})
	p := filepath.Join(dir, "nocache.jsonl")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "nocache")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "nocache.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	html := string(b)
	if !strings.Contains(html, "no record reported a cache figure") {
		t.Error("the page does not say the cache share is unknown")
	}
	if strings.Contains(html, "0.0%") {
		t.Error("the page reports a measured cold cache the trace never claimed")
	}
	if got := cacheShare(0, 40000); got != "—" {
		t.Errorf("cacheShare(0, 40000) = %q, want an em dash", got)
	}
	if got := cacheShare(400, 1000); got != "40.0%" {
		t.Errorf("cacheShare(400, 1000) = %q, want the measured share", got)
	}
}

// Model time is a sum over sessions that ran at once, so it is not "of" the wall
// clock and the tile must not say it is.
func TestReportWallClockDoesNotContainConcurrentModelTime(t *testing.T) {
	dir := t.TempDir()
	turn := func(sess string) string {
		return mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 0), RootSession: "root", Session: sess,
			ParentSession: "root", Role: "coder", Model: "m", DurationMs: 60000, Transport: "native",
			Member: "local", ToolCalls: []traceToolCall{}})
	}
	p := filepath.Join(dir, "par.jsonl")
	if err := os.WriteFile(p, []byte(turn("root")+"\n"+turn("t1")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "par")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "par.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	html := string(b)
	if rep.wall() != time.Minute || rep.TurnMs != 120000 {
		t.Fatalf("wall = %s, model time = %dms", rep.wall(), rep.TurnMs)
	}
	if !strings.Contains(html, "2m00s in models, summed across 2 sessions") {
		t.Error("the wall-clock tile does not say the model time is a sum")
	}
	if strings.Contains(html, "of it in models") {
		t.Error("the tile still claims 2m00s fits inside 1m00s")
	}
}

// A big tree renders collapsed. The browser lays out every open <details> at
// load, and a fleet run has a hundred sessions.
func TestReportKeepsBusySessionsCollapsed(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < reportOpenRows+5; i++ {
		lines = append(lines, mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, time.Duration(i)*time.Second),
			RootSession: "root", Session: "root-t1", ParentSession: "root", Role: "coder", Model: "m", Step: i,
			Transport: "native", Member: "local", ToolCalls: []traceToolCall{}}))
	}
	lines = append(lines, mustJSON(t, TurnRecord{Type: "turn", TS: reportTS(t, 0), RootSession: "root", Session: "root",
		Role: "lead", Model: "m", Transport: "native", Member: "local", ToolCalls: []traceToolCall{}}))
	p := filepath.Join(dir, "deep.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := buildReport([]string{p}, "deep")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.sessions["root"].openByDefault() {
		t.Error("the root must open: the shape of the run is the first thing to read")
	}
	if rep.sessions["root-t1"].openByDefault() {
		t.Errorf("a session with %d rows must not open by default", len(rep.sessions["root-t1"].rows))
	}
	out := filepath.Join(dir, "deep.html")
	if err := writeReport(out, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "<details><summary>root-t1") {
		t.Error("the busy subagent renders expanded")
	}
}

// The compaction helper is in the totals and is printed apart from them, because
// lca eval leaves it out and two commands must not look like they disagree.
func TestReportShowsCompactionApartFromTheTotals(t *testing.T) {
	rep, html := renderFixture(t)
	if rep.CompactTurns != 1 || rep.CompactIn != 8000 || rep.CompactCached != 6000 {
		t.Fatalf("compaction = %d turns / %d in / %d cached", rep.CompactTurns, rep.CompactIn, rep.CompactCached)
	}
	if !strings.Contains(html, "of that, compaction") || !strings.Contains(html, "housekeeping") {
		t.Error("the page does not separate the compaction helper's share of the totals")
	}
}
