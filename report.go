package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// lca report — the JSONL trace (trace.go) as one self-contained HTML file.
//
//	lca report [<trace.jsonl>|<run-id>|<session-id>] [-out file.html] [-open]
//
// The premise of this binary is that one static file is the whole tool, so the
// observability answer has to be one static file too: no daemon, no server, no
// CDN, no network at render time. The page carries its own CSS and draws its
// one chart as inline SVG, so it opens from file:// on a machine with nothing
// installed, survives being mailed to someone, and still reads a year later
// when the run's worktrees and gateway are long gone.
//
// Three rules the rest of the file follows:
//
//  1. Nothing is guessed. A field the records do not carry renders as "—".
//     There is no cost column filled from a price table we made up: cost shows
//     only if a record carried one (see repHead.Cost).
//  2. Nothing is dropped in silence. A trace is appended to by several
//     processes and a kill -9 leaves half a line behind, so unreadable lines
//     are counted and printed on the page. Every detail list is capped, and
//     every cap is stated next to the list it truncated.
//  3. Nothing from a model is trusted. Tool arguments, raw model text, file
//     paths and commands are attacker-chosen strings; they go through esc()
//     exactly once and are never interpolated raw.
//
// Memory is bounded, not merely "flat in the trace": the file is streamed twice
// (pass one only for the time range the chart needs, pass two for everything
// else) and nothing but bounded aggregates is kept. Bounded needs a global
// budget and not only a per-session one — 4000 sessions × 300 turns is 1.2M
// rows, which is a page no browser opens — so the detail rows the page keeps
// are capped across the whole report (reportTotalTurnRows, reportTotalToolRows)
// as well as per session, and the footer states both. An hour-long run's trace
// is tens of megabytes; the report must never hold it.

// Caps. Every one of these is printed on the page beside the list it bounds —
// a truncated list that admits its cap is honest, a page that quietly stops at
// 200 rows is not.
const (
	reportBuckets   = 120 // columns in the activity chart, whatever the run's length
	reportTurnRows  = 300 // turns detailed per session
	reportToolRows  = 300 // tool calls listed per session
	reportInvalids  = 60  // invalid calls shown with the model's raw text
	reportFallbacks = 120
	reportTaskRows  = 200
	reportStepRows  = 300 // per workflow run
	reportSessions  = 4000
	reportRuns      = 200
	reportToolNames = 200 // distinct tool names in the aggregate; the rest fold into one row
	reportSetCap    = 12  // labels in one of a session's small label sets

	// The whole page's row budget. The per-session caps above bound one session
	// and say nothing about a fleet of them, so these bound the page: a 380MB
	// trace of 160 sessions used to render a 137MB file that no browser would
	// open, which is a cap that failed at the only job a cap has.
	reportTotalTurnRows = 20000
	reportTotalToolRows = 20000

	// reportOpenRows is how much detail a session may hold and still render
	// expanded. A root always opens, because the shape of the run is the first
	// thing to read; a deep tree stays collapsed, because a browser lays out
	// every open <details> at load and the summary line already carries the
	// numbers.
	reportOpenRows = 40

	// reportMaxLine is how much of one JSONL line we will hold. trace.go already
	// truncates what a model can put in a record (args 300 bytes, raw 1000,
	// reply 2000), so a line past this is damage, not data.
	reportMaxLine = 256 << 10
)

// reSessionID is the session id shape rec.id mints: date-time-pid. `lca report
// <session-id>` means "$LCA_DIR/traces/<that>.jsonl".
var reSessionID = regexp.MustCompile(`^\d{8}-\d{6}-\d+$`)

// ── the aggregate ───────────────────────────────────────────────────────────

// repSource is one trace file the report covers. A resumed workflow run wrote
// several, so the page names each with its own line and error counts rather
// than pretending one file is the whole story.
type repSource struct {
	Path                 string
	Size                 int64
	Lines, Bad, Oversize int
	Err                  string // set when the file could not be read at all

	// limit is how many bytes this file had when the report started, or -1 when
	// we could not stat it. Both passes read exactly that prefix, so the chart
	// cannot be bucketed against a range the rows have already outgrown.
	limit int64
}

type repTurnRow struct {
	TS                  time.Time
	Step                int
	Role, Model, Member string
	Tier, Transport     string
	Finish, Err         string
	In, Out, Cached     int
	TTFT, Dur           int64
	Tools, Invalid      int
	Falls               int
}

type repToolRow struct {
	Step            int
	Name, Args, Err string
	OK, Invalid     bool
	Bytes           int
	Ms              int64
}

type repToolAgg struct {
	Name                        string
	Calls, Errs, Invalid, Bytes int
	Ms, Max                     int64
}

type repInvalidRow struct {
	TS                   time.Time
	Session, Role, Model string
	Step                 int
	Name, Args, Raw      string
	Reply                string
	NoCall               bool // the turn counted an invalid call that produced no call at all
}

type repFallRow struct {
	TS               time.Time
	Session, Role    string
	Step             int
	From, To, Reason string
	WaitMs           int64
	Kind             string // wait | switch | repeat
}

type repTaskRow struct {
	TS  time.Time
	Rec TaskRecord
}

type repRun struct {
	Run, Workflow       string
	First, Last         time.Time
	Steps               []StepRecord
	Dropped             int
	OK, Failed, Skipped int
	In, Out, Cached     int
	DurMs               int64
	Roles, Members      *strSet
}

type repSession struct {
	UID, Parent, Root, Kind                   string
	Roles, Models, Members, Tiers, Transports *strSet
	Turns, Errors                             int
	In, Out, Cached                           int
	Tools, ToolErrs, Invalid, Falls           int
	DurMs, TTFTSum                            int64
	TTFTn                                     int
	First, Last                               time.Time

	rows         []repTurnRow
	rowsDropped  int
	tools        []repToolRow
	toolsDropped int
	tasks        []repTaskRow
	tasksDropped int

	hits []int  // turns per chart bucket
	errs []bool // a bucket holding a turn that errored

	children []*repSession
}

type report struct {
	Name      string // what the user asked for, or what we defaulted to
	Sources   []repSource
	Generated time.Time

	Lines, Bad, Oversize, Unknown int
	First, Last                   time.Time

	Turns, TaskCount, StepCount         int
	In, Out, Cached                     int
	Calls, CallErrs, Invalid, Attempted int
	Falls, Waits, Switches, Repeats     int
	WaitMs, TurnMs                      int64
	Passed, Failed, OtherTasks          int
	Cost                                float64
	HasCost                             bool

	// The compaction helper's share of the totals above. lca eval leaves it out
	// as housekeeping (scoreTrace); this page counts it, because it is spend the
	// run really incurred — and prints it on its own line so the two commands'
	// different numbers are subtractable rather than mysterious.
	CompactTurns, CompactIn, CompactOut, CompactCached int

	sessions map[string]*repSession
	order    []string
	roots    []*repSession
	// sessDropped counts distinct sessions past the cap, not lookups of them:
	// droppedUIDs is what makes it distinct, and is itself capped, after which
	// the page says "at least" rather than inventing a total.
	sessDropped     int
	droppedUIDs     map[string]bool
	sessDroppedOver bool

	// What is left of the page-wide row budget, and what it has already cut.
	turnBudget, toolBudget int
	turnsCut, toolsCut     int

	// dated is set by render: once a run crosses a UTC day, a bare 23:00:00 →
	// 00:06:01 reads as running backwards, so every in-run moment wears its date.
	dated bool

	toolAgg   map[string]*repToolAgg
	toolOrder []string
	toolOther *repToolAgg

	invalids       []repInvalidRow
	invalidDropped int
	falls          []repFallRow
	fallDropped    int
	tasks          []repTaskRow
	taskDropped    int

	runs       map[string]*repRun
	runOrder   []string
	runDropped int

	Roles, Models, Members, Tiers, Transports *strSet
}

// strSet is an insertion-ordered set of the small label sets a report shows —
// the roles a session ran as, the models it reached. Insertion order, not
// sorted: the order a trace introduces a model in is itself information (the
// first one is the role's primary, a later one is a fallback).
type strSet struct {
	list []string
	seen map[string]bool
	over bool
}

func newSet() *strSet { return &strSet{seen: map[string]bool{}} }

func (s *strSet) add(v string) {
	if v == "" || s.seen[v] {
		return
	}
	if len(s.list) >= reportSetCap {
		s.over = true
		return
	}
	s.seen[v] = true
	s.list = append(s.list, v)
}

// join renders the set already escaped, or "—" when the trace said nothing.
func (s *strSet) join() string {
	if s == nil || len(s.list) == 0 {
		return "—"
	}
	out := esc(strings.Join(s.list, " · "))
	if s.over {
		out += " · …"
	}
	return out
}

// ── reading the trace ───────────────────────────────────────────────────────

// repHead is the cheap decode every line gets first: enough to route the record
// and to place it in time.
//
// Cost is not written by anything in this binary. It is read here, and only
// here, because the contract for this page is "dollars if the trace carries
// any cost" — a gateway-side writer may one day append cost_usd, and until one
// does the page says "—" rather than multiplying tokens by a price we invented.
type repHead struct {
	Type       string   `json:"type"`
	TS         string   `json:"ts"`
	DurationMs int64    `json:"duration_ms"`
	Cost       *float64 `json:"cost_usd"`
}

func repTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// eachLine feeds fn one JSONL line at a time and is bounded: a line longer than
// reportMaxLine is reported with over=true and no content instead of ending the
// scan, which is why this is a bufio.Reader and not a bufio.Scanner (a Scanner
// stops for good on the first token that will not fit, so one corrupt line would
// hide every record after it). The slice handed to fn is reused — fn must not
// keep it.
//
// over is its own signal rather than "not ok" because the two are different
// facts: a line we refused to hold may be perfectly well-formed JSON, and
// telling the reader it "would not parse" sends them looking for damage that is
// not there.
func eachLine(r io.Reader, fn func(line []byte, over bool)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			chunk = chunk[:len(chunk)-1]
		case err == bufio.ErrBufferFull:
		case err == io.EOF:
		default:
			return err
		}
		if !over {
			if len(buf)+len(chunk) > reportMaxLine {
				over, buf = true, buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		line := bytes.TrimRight(buf, "\r")
		switch {
		case over:
			fn(nil, true)
		case len(bytes.TrimSpace(line)) > 0:
			fn(line, false)
		}
		buf, over = buf[:0], false
		if err == io.EOF {
			return nil
		}
	}
}

// buildReport aggregates the given trace files. Pass one establishes the time
// range, which the activity chart needs before it can bucket anything; pass two
// does the rest. Two passes over a file on local disk is the price of never
// holding a turn we are not going to print.
func buildReport(paths []string, name string) (*report, error) {
	r := &report{Name: name, Generated: time.Now(),
		sessions: map[string]*repSession{}, toolAgg: map[string]*repToolAgg{}, runs: map[string]*repRun{},
		droppedUIDs: map[string]bool{},
		turnBudget:  reportTotalTurnRows, toolBudget: reportTotalToolRows,
		Roles: newSet(), Models: newSet(), Members: newSet(), Tiers: newSet(), Transports: newSet()}

	readable := 0
	for _, p := range dedupPaths(paths) {
		src := repSource{Path: p, limit: -1}
		if fi, err := os.Stat(p); err == nil {
			src.Size, src.limit = fi.Size(), fi.Size()
		}
		if err := withFile(p, func(f *os.File) error { return r.scanRange(clip(f, src.limit)) }); err != nil {
			src.Err = err.Error()
			r.Sources = append(r.Sources, src)
			continue
		}
		readable++
		r.Sources = append(r.Sources, src)
	}
	if readable == 0 {
		if len(r.Sources) == 1 && r.Sources[0].Err != "" {
			return nil, fmt.Errorf("%s: %s", r.Sources[0].Path, r.Sources[0].Err)
		}
		return nil, fmt.Errorf("no readable trace in %s", strings.Join(paths, ", "))
	}
	for i := range r.Sources {
		src := &r.Sources[i]
		if src.Err != "" {
			continue
		}
		if err := withFile(src.Path, func(f *os.File) error { return r.scanRecords(clip(f, src.limit), src) }); err != nil {
			// The file was readable a moment ago (pass one), so losing it here is
			// worth saying out loud rather than rendering a source with no lines.
			src.Err = err.Error()
		}
	}
	r.link()
	sort.Strings(r.toolOrder)
	return r, nil
}

// dedupPaths keeps the first spelling of each file and drops the rest. A run's
// state lists one trace per process it ran in (attachRun), and with $LCA_TRACE
// set those are all one file — scanning it twice would double every number on
// the page with nothing to hint at it. The list a user types is no more
// trustworthy, so this is the only place either is believed.
func dedupPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	stats := make([]os.FileInfo, 0, len(paths))
	for _, p := range paths {
		key := p
		if abs, err := filepath.Abs(p); err == nil {
			key = abs
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		// Two spellings of one file (a symlink, a ./ prefix, a bind mount) are the
		// same double count, so identity is settled by the filesystem, not by text.
		fi, err := os.Stat(p)
		if err == nil {
			same := false
			for _, prev := range stats {
				if os.SameFile(prev, fi) {
					same = true
					break
				}
			}
			if same {
				continue
			}
			stats = append(stats, fi)
		}
		out = append(out, p)
	}
	return out
}

func withFile(p string, fn func(*os.File) error) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return fn(f)
}

// clip bounds a pass to the bytes the file held when the report started. The
// trace is usually still being appended to — `lca report` is what you run on a
// run that is still going — and the two passes must see the same file or the
// chart ends up bucketing rows against a range that no longer contains them.
func clip(f *os.File, limit int64) io.Reader {
	if limit < 0 {
		return f
	}
	return io.LimitReader(f, limit)
}

// scanRange is pass one: timestamps only.
//
// Only a turn is stamped at its start (trace.go's traceTurn uses traceTS(start),
// so a turn still streaming when the page is made still stretches the range it
// drew). A task and a step are stamped when they finish (nowTS), so adding their
// duration would push the range past the end of the run — a 30-minute delegation
// would report an hour — and the wall clock is the most prominent number here.
func (r *report) scanRange(rd io.Reader) error {
	return eachLine(rd, func(line []byte, over bool) {
		if over {
			return
		}
		var h repHead
		if json.Unmarshal(line, &h) != nil {
			return
		}
		t, okTS := repTime(h.TS)
		if !okTS {
			return
		}
		if r.First.IsZero() || t.Before(r.First) {
			r.First = t
		}
		end := t
		if h.Type == "turn" {
			end = t.Add(time.Duration(h.DurationMs) * time.Millisecond)
		}
		if end.After(r.Last) {
			r.Last = end
		}
	})
}

func (r *report) scanRecords(rd io.Reader, src *repSource) error {
	return eachLine(rd, func(line []byte, over bool) {
		r.Lines++
		src.Lines++
		if over {
			// Counted, and counted apart from a parse failure: this line may be a
			// flawless record that is simply bigger than we will hold.
			r.Oversize++
			src.Oversize++
			return
		}
		var h repHead
		if json.Unmarshal(line, &h) != nil {
			r.Bad++
			src.Bad++
			return
		}
		if h.Cost != nil {
			r.HasCost = true
			r.Cost += *h.Cost
		}
		switch h.Type {
		case "turn":
			var t TurnRecord
			if json.Unmarshal(line, &t) != nil {
				r.Bad++
				src.Bad++
				return
			}
			r.addTurn(t)
		case "task":
			var t TaskRecord
			if json.Unmarshal(line, &t) != nil {
				r.Bad++
				src.Bad++
				return
			}
			r.addTask(t)
		case "step":
			var s StepRecord
			if json.Unmarshal(line, &s) != nil {
				r.Bad++
				src.Bad++
				return
			}
			r.addStep(s)
		default:
			r.Unknown++
		}
	})
}

// session returns the node for uid, or nil once reportSessions distinct
// sessions have been seen. Totals are accumulated by the callers before this is
// consulted, so a dropped session never costs the summary its numbers.
func (r *report) session(uid, parent, root string) *repSession {
	if uid == "" {
		uid = "(unnamed)"
	}
	if s := r.sessions[uid]; s != nil {
		if s.Parent == "" {
			s.Parent = parent
		}
		if s.Root == "" {
			s.Root = root
		}
		return s
	}
	if len(r.sessions) >= reportSessions {
		// Once per session, not once per turn it took: this is the one number on
		// the page whose whole job is to be honest about what the page hid, and
		// "2004 further sessions" for 501 of them is exactly the invented figure
		// rule 1 forbids. The uid set is capped like everything else here.
		switch {
		case r.droppedUIDs[uid]:
		case len(r.droppedUIDs) < reportSessions:
			r.droppedUIDs[uid] = true
			r.sessDropped++
		default:
			r.sessDroppedOver = true
		}
		return nil
	}
	s := &repSession{UID: uid, Parent: parent, Root: root, Kind: sessionKind(uid, root),
		Roles: newSet(), Models: newSet(), Members: newSet(), Tiers: newSet(), Transports: newSet()}
	r.sessions[uid] = s
	r.order = append(r.order, uid)
	return s
}

func sessionKind(uid, root string) string {
	switch {
	case strings.HasSuffix(uid, "-compact"):
		return "compaction"
	case root == "" || uid == root:
		return "root"
	default:
		return "subagent"
	}
}

func (r *report) addTurn(t TurnRecord) {
	ts, _ := repTime(t.TS)
	r.Turns++
	r.In += t.Usage.PromptTokens
	r.Out += t.Usage.CompletionTokens
	r.Cached += t.Usage.CachedTokens
	r.TurnMs += t.DurationMs
	r.Falls += len(t.Fallbacks)
	r.Roles.add(t.Role)
	r.Models.add(t.Model)
	r.Members.add(t.Member)
	r.Tiers.add(t.Tier)
	r.Transports.add(t.Transport)

	if sessionKind(t.Session, t.RootSession) == "compaction" {
		// Kept in the totals, and kept subtractable from them: lca eval drops
		// these turns as housekeeping, and a reader comparing the two commands
		// deserves the difference spelled out rather than discovered.
		r.CompactTurns++
		r.CompactIn += t.Usage.PromptTokens
		r.CompactOut += t.Usage.CompletionTokens
		r.CompactCached += t.Usage.CachedTokens
	}

	// The invalid-call share counts what lca eval's does (scoreTrace): parsed
	// calls plus the unparsable text tags that never became calls. Two commands
	// reading one trace must not print two different rates — with one stated
	// difference, that eval excludes the compaction helper and this page does not.
	//
	// The two invalid figures a record carries can disagree if a foreign or older
	// writer made it, and then the larger is the truth: a denominator that counts
	// fewer attempts than there are listed calls would print a share of a number
	// smaller than the rows underneath it.
	invalidInCalls := 0
	for _, c := range t.ToolCalls {
		if c.Invalid {
			invalidInCalls++
		}
	}
	invalid, noCall := t.InvalidCalls, t.InvalidCalls-invalidInCalls
	if invalidInCalls > invalid {
		invalid = invalidInCalls
	}
	if noCall < 0 {
		noCall = 0
	}
	r.Calls += len(t.ToolCalls)
	r.Invalid += invalid
	r.Attempted += len(t.ToolCalls) + noCall

	// Make sure the root exists even when it never took a turn of its own: a
	// workflow lead that only delegates is the normal case, and a tree missing
	// its root reads as if the subagents ran on their own.
	if t.RootSession != "" && t.RootSession != t.Session {
		r.session(t.RootSession, "", t.RootSession)
	}
	s := r.session(t.Session, t.ParentSession, t.RootSession)
	if s == nil {
		return
	}
	s.Turns++
	s.In += t.Usage.PromptTokens
	s.Out += t.Usage.CompletionTokens
	s.Cached += t.Usage.CachedTokens
	s.DurMs += t.DurationMs
	s.Tools += len(t.ToolCalls)
	s.Invalid += invalid
	s.Falls += len(t.Fallbacks)
	s.Roles.add(t.Role)
	s.Models.add(t.Model)
	s.Members.add(t.Member)
	s.Tiers.add(t.Tier)
	s.Transports.add(t.Transport)
	if t.TTFTMs > 0 {
		s.TTFTSum += t.TTFTMs
		s.TTFTn++
	}
	if t.Error != "" {
		s.Errors++
	}
	s.span(ts, t.DurationMs) // a turn's ts is its start, so its duration runs forward
	r.bucket(s, ts, t.Error != "")

	if len(s.rows) < reportTurnRows && r.turnBudget > 0 {
		r.turnBudget--
		s.rows = append(s.rows, repTurnRow{TS: ts, Step: t.Step, Role: t.Role, Model: t.Model, Member: t.Member,
			Tier: t.Tier, Transport: t.Transport, Finish: t.Finish, Err: t.Error,
			In: t.Usage.PromptTokens, Out: t.Usage.CompletionTokens, Cached: t.Usage.CachedTokens,
			TTFT: t.TTFTMs, Dur: t.DurationMs, Tools: len(t.ToolCalls), Invalid: invalid, Falls: len(t.Fallbacks)})
	} else {
		// The session's own count still goes up, so its line stays true whichever
		// cap stopped the row; the page-wide count is what names the other one.
		s.rowsDropped++
		if len(s.rows) < reportTurnRows {
			r.turnsCut++
		}
	}

	for _, c := range t.ToolCalls {
		if !c.OK {
			r.CallErrs++
			s.ToolErrs++
		}
		r.tool(c.Name).count(c)
		if len(s.tools) < reportToolRows && r.toolBudget > 0 {
			r.toolBudget--
			s.tools = append(s.tools, repToolRow{Step: t.Step, Name: c.Name, Args: c.Args, Err: c.Error,
				OK: c.OK, Invalid: c.Invalid, Bytes: c.ResultBytes, Ms: c.Ms})
		} else {
			s.toolsDropped++
			if len(s.tools) < reportToolRows {
				r.toolsCut++
			}
		}
		if c.Invalid {
			r.addInvalid(repInvalidRow{TS: ts, Session: t.Session, Role: t.Role, Model: t.Model, Step: t.Step,
				Name: c.Name, Args: c.Args, Raw: c.Raw, Reply: t.RawReply})
		}
	}
	// A turn can count more invalid calls than it has invalid entries: an
	// unparsable text-protocol tag never becomes a call, so the reply is the
	// only evidence there is.
	if n := t.InvalidCalls - invalidInCalls; n > 0 {
		r.addInvalid(repInvalidRow{TS: ts, Session: t.Session, Role: t.Role, Model: t.Model, Step: t.Step,
			Reply: t.RawReply, NoCall: true})
	}

	for _, fb := range t.Fallbacks {
		kind := "switch"
		switch {
		case fb.To == "":
			kind = "wait"
			r.Waits++
			r.WaitMs += fb.WaitMs
		case fb.To == fb.From:
			kind = "repeat"
			r.Repeats++
		default:
			r.Switches++
		}
		if len(r.falls) < reportFallbacks {
			r.falls = append(r.falls, repFallRow{TS: ts, Session: t.Session, Role: t.Role, Step: t.Step,
				From: fb.From, To: fb.To, Reason: fb.Reason, WaitMs: fb.WaitMs, Kind: kind})
		} else {
			r.fallDropped++
		}
	}
}

func (r *report) addInvalid(row repInvalidRow) {
	if len(r.invalids) < reportInvalids {
		r.invalids = append(r.invalids, row)
		return
	}
	r.invalidDropped++
}

func (r *report) tool(name string) *repToolAgg {
	if name == "" {
		name = "(unnamed)"
	}
	if a := r.toolAgg[name]; a != nil {
		return a
	}
	if len(r.toolAgg) >= reportToolNames {
		// Past the cap the calls still count, they just stop having a name of
		// their own: a trace carrying thousands of distinct tool names is a
		// symptom, and one row saying so is the useful rendering of it.
		if r.toolOther == nil {
			r.toolOther = &repToolAgg{Name: fmt.Sprintf("(tool names past the cap of %d)", reportToolNames)}
		}
		return r.toolOther
	}
	a := &repToolAgg{Name: name}
	r.toolAgg[name] = a
	r.toolOrder = append(r.toolOrder, name)
	return a
}

func (a *repToolAgg) count(c traceToolCall) {
	a.Calls++
	if !c.OK {
		a.Errs++
	}
	if c.Invalid {
		a.Invalid++
	}
	a.Bytes += c.ResultBytes
	a.Ms += c.Ms
	if c.Ms > a.Max {
		a.Max = c.Ms
	}
}

func (r *report) addTask(t TaskRecord) {
	ts, _ := repTime(t.TS)
	r.TaskCount++
	switch t.Status {
	case "passed":
		r.Passed++
	case "failed", "error", "conflict":
		r.Failed++
	default:
		r.OtherTasks++
	}
	r.Roles.add(t.Role)
	r.Members.add(t.Member)
	row := repTaskRow{TS: ts, Rec: t}
	if len(r.tasks) < reportTaskRows {
		r.tasks = append(r.tasks, row)
	} else {
		r.taskDropped++
	}
	if t.RootSession != "" && t.RootSession != t.Session {
		r.session(t.RootSession, "", t.RootSession)
	}
	if s := r.session(t.Session, t.ParentSession, t.RootSession); s != nil {
		// Capped like the session's other lists, and with its own counter: the
		// global table beside this one states its cap, and a sibling list that
		// could not state one would be the one a reader wrongly trusts.
		if len(s.tasks) < reportTaskRows {
			s.tasks = append(s.tasks, row)
		} else {
			s.tasksDropped++
		}
		s.Roles.add(t.Role)
		s.Members.add(t.Member)
		// A task's ts is stamped when it finished (trace.go's traceTask), so its
		// duration runs backwards out of that moment, not forwards into the future.
		s.span(ts, 0)
	}
}

func (r *report) addStep(rec StepRecord) {
	ts, _ := repTime(rec.TS)
	r.StepCount++
	r.Roles.add(rec.Role)
	r.Members.add(rec.Member)
	r.Models.add(rec.Model)
	if rec.RootSession != "" {
		r.session(rec.RootSession, "", rec.RootSession)
	}
	id := rec.Run
	if id == "" {
		id = "(unnamed run)"
	}
	run := r.runs[id]
	if run == nil {
		if len(r.runs) >= reportRuns {
			r.runDropped++
			return
		}
		run = &repRun{Run: id, Workflow: rec.Workflow, Roles: newSet(), Members: newSet()}
		r.runs[id] = run
		r.runOrder = append(r.runOrder, id)
	}
	if !ts.IsZero() {
		if run.First.IsZero() || ts.Before(run.First) {
			run.First = ts
		}
		// A step record is written when the step finished (traceStep), so its ts
		// is already the end: adding the duration would report a run that ends
		// after its own last step.
		if ts.After(run.Last) {
			run.Last = ts
		}
	}
	run.DurMs += rec.DurationMs
	run.Roles.add(rec.Role)
	run.Members.add(rec.Member)
	if rec.Usage != nil {
		run.In += rec.Usage.PromptTokens
		run.Out += rec.Usage.CompletionTokens
		run.Cached += rec.Usage.CachedTokens
	}
	switch rec.Status {
	case stepOK:
		run.OK++
	case stepSkipped:
		run.Skipped++
	default:
		run.Failed++
	}
	if len(run.Steps) < reportStepRows {
		run.Steps = append(run.Steps, rec)
	} else {
		run.Dropped++
	}
}

// span widens the session's window to hold one record. durMs is how far the
// record reaches past its own ts, which is its duration only for the record
// types the trace stamps at their start — the caller knows which it has.
func (s *repSession) span(ts time.Time, durMs int64) {
	if ts.IsZero() {
		return
	}
	if s.First.IsZero() || ts.Before(s.First) {
		s.First = ts
	}
	if end := ts.Add(time.Duration(durMs) * time.Millisecond); end.After(s.Last) {
		s.Last = end
	}
}

// bucket places one turn in the session's activity strip. The strip has a fixed
// column count whatever the run's length, which is what keeps a 50,000-turn
// trace from producing 50,000 SVG rects.
func (r *report) bucket(s *repSession, ts time.Time, failed bool) {
	if ts.IsZero() || r.First.IsZero() {
		return
	}
	if s.hits == nil {
		s.hits, s.errs = make([]int, reportBuckets), make([]bool, reportBuckets)
	}
	span := r.Last.Sub(r.First)
	i := 0
	if span > 0 {
		i = int(float64(ts.Sub(r.First)) / float64(span) * float64(reportBuckets))
	}
	if i < 0 {
		i = 0
	}
	if i >= reportBuckets {
		i = reportBuckets - 1
	}
	s.hits[i]++
	if failed {
		s.errs[i] = true
	}
}

// link hangs every session off its parent. A trace is data, not a promise: a
// record claiming a parent that is really a descendant would make the renderer
// recurse forever, so a link that closes a loop is refused and the session is
// shown at top level instead.
func (r *report) link() {
	for _, uid := range r.order {
		s := r.sessions[uid]
		p := r.parentOf(s)
		if p == nil || r.descends(p, s) {
			r.roots = append(r.roots, s)
			continue
		}
		p.children = append(p.children, s)
	}
}

// parentOf resolves a session's parent. The compaction helper carries no
// parent_session of its own (compaction.go detaches it), so its root is the
// only place it can hang from.
func (r *report) parentOf(s *repSession) *repSession {
	if p := r.sessions[s.Parent]; p != nil && p != s {
		return p
	}
	if s.Root != "" && s.Root != s.UID {
		if p := r.sessions[s.Root]; p != nil && p != s {
			return p
		}
	}
	return nil
}

// descends reports whether a is b or sits under b. Running out of steps counts
// as a loop: refusing to link is always safe, an infinite render is not.
func (r *report) descends(a, b *repSession) bool {
	for i := 0; a != nil; i++ {
		if a == b {
			return true
		}
		if i > len(r.order) {
			return true
		}
		a = r.parentOf(a)
	}
	return false
}

func (r *report) wall() time.Duration {
	if r.First.IsZero() || r.Last.Before(r.First) {
		return 0
	}
	return r.Last.Sub(r.First)
}

// ── the command ─────────────────────────────────────────────────────────────

// runReport is the shell form, and its stdout is the path and nothing else, so
// `lca report | xargs open` keeps working. The in-session form wants the opposite
// — a named, relative path with the program's own glyph — so the one line that
// differs is a callback and the rest is shared. See cmdReport.
func runReport(cfg Config, args []string) int {
	return reportRun(cfg, args, func(dst string) { fmt.Println(dst) })
}

func reportRun(cfg Config, args []string, wrote func(string)) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	out := fs.String("out", "", "write the HTML here (default: next to the trace)")
	openIt := fs.Bool("open", false, "open the finished report with the desktop's own opener")
	// The documented order is `lca report <what> -out file.html`, and flag.Parse
	// stops at the first non-flag — so the bare argument comes off the front
	// first, exactly as `lca run <name> -resume` does it (splitLeadingName).
	bare, rest := splitLeadingName(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if bare == "" {
		bare = fs.Arg(0)
	}
	if fs.NArg() > 1 || (bare != "" && fs.NArg() > 0 && fs.Arg(0) != bare) {
		fmt.Fprintln(os.Stderr, "usage: lca report [<trace.jsonl>|<run-id>|<session-id>] [-out file.html] [-open]")
		return 2
	}
	paths, name, err := resolveTrace(cfg, bare)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return 1
	}
	rep, err := buildReport(paths, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return 1
	}
	dst := *out
	if dst == "" {
		dst = defaultReportPath(paths, name)
	}
	if err := writeReport(dst, rep); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return 1
	}
	// Everything the reader might want to know goes to stderr; only the caller's
	// own line names the file.
	wrote(dst)
	for _, s := range rep.Sources {
		if s.Err != "" {
			fmt.Fprintln(os.Stderr, warn("skipped %s: %s", shortDir(s.Path), s.Err))
		}
	}
	if rep.Bad > 0 {
		fmt.Fprintln(os.Stderr, warn("%s in the trace — counted on the page, not dropped", plural(rep.Bad, "unreadable line", "unreadable lines")))
	}
	if rep.Oversize > 0 {
		// Said separately from the unreadable count: such a line is often a
		// well-formed record that is simply bigger than the report will hold, and
		// sending someone to look for truncated JSON that is not there wastes
		// their evening.
		fmt.Fprintln(os.Stderr, warn("%s over %s in the trace — counted on the page, not parsed",
			plural(rep.Oversize, "record", "records"), fmtBytes(reportMaxLine)))
	}
	if *openIt {
		if err := openFile(dst); err != nil {
			fmt.Fprintln(os.Stderr, warn("could not open it: %v", err))
			return 1
		}
	}
	return 0
}

// resolveTrace turns the single optional argument into the trace files to read.
// A run id is the one case that yields more than one: a resumed run wrote a
// trace per process, and reporting on only the newest would hide the work the
// first process did.
func resolveTrace(cfg Config, arg string) ([]string, string, error) {
	dir := filepath.Join(cfg.stateDir(), "traces")
	if arg == "" {
		p, err := newestTrace(dir)
		if err != nil {
			return nil, "", err
		}
		return []string{p}, strings.TrimSuffix(filepath.Base(p), ".jsonl"), nil
	}
	if fi, err := os.Stat(arg); err == nil && !fi.IsDir() {
		return []string{arg}, strings.TrimSuffix(filepath.Base(arg), ".jsonl"), nil
	}
	if reSessionID.MatchString(arg) {
		p := filepath.Join(dir, arg+".jsonl")
		if _, err := os.Stat(p); err != nil {
			return nil, "", fmt.Errorf("no trace for session %s: %s is not there", arg, shortDir(p))
		}
		return []string{p}, arg, nil
	}
	if reRunID.MatchString(arg) {
		st, err := loadRunState(filepath.Join(runsDir(cfg), filepath.Base(arg)))
		if err != nil {
			return nil, "", fmt.Errorf("no run %q: %w", arg, err)
		}
		if len(st.Traces) == 0 {
			return nil, "", fmt.Errorf("run %s recorded no trace file", st.Run)
		}
		return st.Traces, filepath.Base(st.Run), nil
	}
	return nil, "", fmt.Errorf("%q is not a trace file, a run id or a session id — pass a path, or leave it off for the newest trace in %s", arg, shortDir(dir))
}

// newestTrace picks by modification time, not by name: a name is the moment a
// session started, and the trace you almost always want is the one last written
// to — which for a long run is not the one that started last.
func newestTrace(dir string) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("no traces yet: %s (%v)", shortDir(dir), err)
	}
	best, bestAt := "", time.Time{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if best == "" || fi.ModTime().After(bestAt) || (fi.ModTime().Equal(bestAt) && e.Name() > filepath.Base(best)) {
			best, bestAt = filepath.Join(dir, e.Name()), fi.ModTime()
		}
	}
	if best == "" {
		return "", fmt.Errorf("no traces yet in %s — run something first", shortDir(dir))
	}
	return best, nil
}

func defaultReportPath(paths []string, name string) string {
	dir := filepath.Dir(paths[0])
	if len(paths) > 1 {
		return filepath.Join(dir, filepath.Base(name)+".html")
	}
	return strings.TrimSuffix(paths[0], filepath.Ext(paths[0])) + ".html"
}

// writeReport renders through a temp file in the same directory and renames it,
// the way saveRunState does: a half-written report that a browser is already
// showing is worse than no report.
func writeReport(dst string, r *report) error {
	if d := filepath.Dir(dst); d != "" {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".report-*.html")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 64<<10)
	r.render(w)
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 0600, like every other artifact under $LCA_DIR: this page quotes model
	// output, file paths and command lines out of a repository that may be
	// private, and the person who asked for it decides who else reads it.
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// openFile hands the report to the desktop's own opener. There is nothing else
// to do: the report is a local file, so there is no address to point at.
func openFile(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", abs)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", abs)
	default:
		cmd = exec.Command("xdg-open", abs)
	}
	return cmd.Start()
}

// ── escaping ────────────────────────────────────────────────────────────────

// esc turns a trace value into text safe both in an HTML text node and inside a
// double- or single-quoted attribute. Everything on this page that came from a
// model, a file path or a command line is hostile input — a tool argument is
// literally attacker-chosen text — so it is escaped once, here.
//
// Invalid UTF-8, C0 and C1 control bytes become U+FFFD rather than passing
// through: a model that emits a stray 0x1b or a lone 0x80 must not be able to
// hand a browser bytes it decodes into markup we did not write. \n and \t
// survive because a <pre> block needs them; \r is dropped, because HTML treats
// it as a line break of its own and a CRLF would otherwise double every line.
func esc(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString("&#34;")
		case r == '\'':
			b.WriteString("&#39;")
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			b.WriteRune('�')
		case r == 0xFFFD:
			// range over a string yields U+FFFD for every invalid byte, and a
			// genuine U+FFFD in the input renders the same glyph anyway.
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// dash escapes a value, or renders "—" when the trace did not carry one. Every
// blank on this page goes through here, so an absent field can never be
// mistaken for a measured zero.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return esc(s)
}

// pct is a share, or "—" when there was nothing to take a share of. Zero of
// zero is not "0%".
func pct(num, den int) string {
	if den <= 0 {
		return "—"
	}
	return strconv.FormatFloat(100*float64(num)/float64(den), 'f', 1, 64) + "%"
}

// fmtSpan spells a wall clock the way a report reads it: 940ms, 12.4s, 3m20s,
// 1h04m. fmtDurShort (format.go) stops at seconds because a single turn does;
// an autonomous run does not.
func fmtSpan(d time.Duration) string {
	// A negative duration reaches here from any duration_ms we did not compute
	// ourselves (a foreign writer, a clock that went backwards). Strip the sign
	// before the switch, not inside it: a `fallthrough` out of a `d < 0` case
	// lands in the sub-second body unconditionally, and -3m used to print as
	// "180000ms" beside neighbours reading "3m00s".
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func fmtMs(ms int64) string { return fmtSpan(time.Duration(ms) * time.Millisecond) }

// clock is the one timestamp spelling on the page: UTC, to the second. The
// trace is RFC3339Nano UTC (trace.go) and several machines write into it, so
// showing a local time would invent a timezone the records never had.
func clock(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func hhmmss(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("15:04:05")
}

// stamp is how a moment inside the run is spelled: the time of day, which is
// what a reader wants while scanning a column of them, or the full date once the
// run crossed a UTC day. Overnight autonomous runs are this project's normal
// case, and "23:00:00 → 00:06:01" reads as a run going backwards.
func (r *report) stamp(t time.Time) string {
	if r.dated {
		return clock(t)
	}
	return hhmmss(t)
}

// crossesDay decides that, once, for the whole page: one spelling everywhere is
// what makes two rows comparable.
func (r *report) crossesDay() bool {
	if r.First.IsZero() || r.Last.IsZero() {
		return false
	}
	return r.wall() > 12*time.Hour || r.First.UTC().Format("2006-01-02") != r.Last.UTC().Format("2006-01-02")
}

// cacheShare is the cache hit rate, or "—" when nothing reported a cached figure
// at all. traceUsage.CachedTokens is 0 both for "the gateway served nothing from
// cache" and for "the server does not report it" (chat.go says so in as many
// words), and a measured cold cache sends a reader hunting for prefix
// instability while silence should send them to the provider — so the page
// refuses to tell the two apart, exactly as the cost tile does.
func cacheShare(cached, in int) string {
	if cached == 0 {
		return "—"
	}
	return pct(cached, in)
}

// statusClass maps a status word to the CLI's own colour rule (repl.go's
// statusWord): green for a pass, yellow for the deliberately non-blocking
// outcomes, red for everything else. The word is always printed next to the
// colour, so nothing here is carried by colour alone.
func statusClass(s string) string {
	switch s {
	case "passed", "ok", "approve", "applied":
		return "ok"
	case "", "—":
		return "mute"
	case "unverified", "not_applied", "unreviewed", "skipped", "cancelled", "running":
		return "warn"
	}
	return "bad"
}

// ── rendering ───────────────────────────────────────────────────────────────

type htmlOut struct{ *bufio.Writer }

func (o htmlOut) p(format string, a ...any) { fmt.Fprintf(o.Writer, format, a...) }
func (o htmlOut) s(text string)             { o.WriteString(text) }

// reportCSS is the whole stylesheet. It is inline because the page must be one
// file, and it is the terminal's own look (ui.go): monochrome plus one accent,
// colour only as status, separation by hairline rather than by fill. The
// palette here is the 256-colour palette ui.go names in its own comments.
//
// One deliberate difference from the terminal: ui.go's --faint (#54545a) is
// used there for labels, and at 2.5:1 on this background it is not readable as
// text, so on the page it draws rules and borders only and labels wear --dim.
//
// The page is dark and has no light mode — that is the pinned look, not an
// oversight — and no @font-face: a font file would be a second file or a
// network request, and this page is allowed neither.
const reportCSS = `
:root{
  --bg:#0b0b0d; --surface:#121214; --sunken:#08080a; --rule:#26262a;
  --ink:#e4e4e6; --dim:#8c8c92; --faint:#54545a;
  --green:#6fdc8c; --yellow:#e3c873; --red:#d8635b;
  --t1:#2e5c5c; --t2:#417f7f; --t3:#5fafaf;
  --crimson:#ba2126;
}
*{box-sizing:border-box}
html,body{background:var(--bg)}
body{margin:0;padding:22px 16px 72px;color:var(--ink);
  font:13px/1.55 ui-monospace,SFMono-Regular,Menlo,Consolas,"DejaVu Sans Mono",monospace;
  -webkit-text-size-adjust:100%}
.wrap{max-width:1120px;margin:0 auto}
a{color:var(--t3)}
h1{margin:0;font-size:14px;letter-spacing:.22em;text-transform:uppercase;color:var(--crimson)}
h2{margin:0;font-size:11px;letter-spacing:.2em;text-transform:uppercase;color:var(--dim);font-weight:400}
.meta{color:var(--dim);margin:6px 0 0;word-break:break-all}
.meta b{color:var(--ink);font-weight:400}
.rule{height:1px;background:var(--rule);margin:18px 0}
section{margin:26px 0 0;overflow-x:auto}
.head{display:flex;flex-wrap:wrap;align-items:baseline;gap:10px;border-bottom:1px solid var(--rule);padding-bottom:6px;margin-bottom:12px}
.head .note{color:var(--faint);font-size:11px}
.tiles{display:flex;flex-wrap:wrap;gap:1px;background:var(--rule);border:1px solid var(--rule);margin-top:14px}
.tile{background:var(--surface);padding:10px 13px;flex:1 1 128px;min-width:128px}
.tile .k{color:var(--dim);font-size:10px;letter-spacing:.14em;text-transform:uppercase}
.tile .v{font-size:18px;margin-top:3px;font-variant-numeric:tabular-nums}
.tile .sub{color:var(--faint);font-size:11px;margin-top:2px}
table{width:100%;border-collapse:collapse;margin-top:8px}
th{text-align:left;font-weight:400;color:var(--dim);font-size:10px;letter-spacing:.14em;text-transform:uppercase;
  border-bottom:1px solid var(--rule);padding:4px 8px 4px 0;white-space:nowrap}
td{padding:3px 8px 3px 0;border-bottom:1px solid var(--sunken);vertical-align:top;color:var(--ink)}
td.n,th.n{text-align:right;font-variant-numeric:tabular-nums;padding-right:14px}
td.w{word-break:break-all;max-width:0}
tbody tr:hover td{background:var(--surface)}
.ok{color:var(--green)}.warn{color:var(--yellow)}.bad{color:var(--red)}
.mute{color:var(--dim)}.faint{color:var(--faint)}
code{color:var(--t3)}
pre{white-space:pre-wrap;overflow-wrap:anywhere;margin:6px 0 10px;padding:8px 10px;background:var(--sunken);
  border-left:2px solid var(--red);color:var(--dim);max-height:20em;overflow:auto;font-size:12px}
pre.plain{border-left-color:var(--rule)}
details{margin:0}
details>summary{cursor:pointer;list-style:none;padding:7px 0;border-bottom:1px solid var(--rule);color:var(--ink)}
details>summary::-webkit-details-marker{display:none}
details>summary::before{content:"+ ";color:var(--faint)}
details[open]>summary::before{content:"- "}
details>summary:hover{background:var(--surface)}
.body{padding:8px 0 16px 14px;border-left:1px solid var(--rule);margin-left:2px;overflow-x:auto}
.kids{margin-left:14px;border-left:1px solid var(--rule);padding-left:10px}
.tag{color:var(--dim)}
.tl{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,3fr);gap:10px;align-items:center;margin:2px 0}
.tl .lbl{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--dim);font-size:11px}
.tl svg{display:block;width:100%;height:16px}
.axis{display:flex;justify-content:space-between;color:var(--faint);font-size:10px;margin-top:4px}
.legend{display:flex;flex-wrap:wrap;gap:12px;color:var(--dim);font-size:11px;margin-top:10px}
.legend i{display:inline-block;width:9px;height:9px;margin-right:5px;vertical-align:-1px}
footer{margin-top:34px;padding-top:10px;border-top:1px solid var(--rule);color:var(--faint);font-size:11px}
@media (max-width:640px){
  .tl{grid-template-columns:1fr;gap:2px}
  td.w{word-break:break-all}
  body{padding:16px 12px 56px}
}
`

func (r *report) render(bw *bufio.Writer) {
	o := htmlOut{bw}
	r.dated = r.crossesDay()
	// No xmlns on the inline SVG below, no webfont, no favicon, no analytics:
	// this page must contain no absolute URL at all, or "self-contained" is a
	// claim and not a fact.
	o.s("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	o.s("<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\n")
	o.p("<title>BEHELIT report — %s</title>\n", esc(r.Name))
	o.p("<style>%s</style>\n</head>\n<body>\n<div class=\"wrap\">\n", reportCSS)
	r.renderHeader(o)
	r.renderTimeline(o)
	r.renderSessions(o)
	r.renderTools(o)
	r.renderInvalid(o)
	r.renderGateway(o)
	r.renderTasks(o)
	r.renderRuns(o)
	r.renderFooter(o)
	o.s("</div>\n</body>\n</html>\n")
}

func (r *report) renderHeader(o htmlOut) {
	o.p("<h1>BEHELIT · TRACE REPORT</h1>\n")
	o.p("<p class=\"meta\">covers <b>%s</b>", dash(r.Name))
	o.p(" · %s · <b>%s</b> → <b>%s</b> UTC</p>\n", plural(len(r.Sources), "trace file", "trace files"), clock(r.First), clock(r.Last))
	for _, s := range r.Sources {
		o.p("<p class=\"meta faint\">%s · %s", esc(shortDir(s.Path)), plural(s.Lines, "line", "lines"))
		if s.Size > 0 {
			o.p(" · %s", esc(fmtBytes(s.Size)))
		}
		if s.Bad > 0 {
			o.p(" · <span class=\"bad\">%s unreadable</span>", strconv.Itoa(s.Bad))
		}
		if s.Oversize > 0 {
			o.p(" · <span class=\"warn\">%s over %s, skipped</span>", strconv.Itoa(s.Oversize), esc(fmtBytes(reportMaxLine)))
		}
		if s.Err != "" {
			o.p(" · <span class=\"bad\">unreadable: %s</span>", esc(s.Err))
		}
		o.s("</p>\n")
	}

	cost, costSub := "—", "no record in this trace carries a cost"
	if r.HasCost {
		cost, costSub = "$"+strconv.FormatFloat(r.Cost, 'f', 4, 64), "summed from the records that carried one"
	}
	cache, cacheSub := "—", "no record reported a cache figure"
	if r.Cached > 0 {
		cache, cacheSub = pct(r.Cached, r.In), esc(kfmt(r.Cached))+" cached of prompt"
	}
	// The sessions tile carries both numbers when some were hidden, so nobody can
	// add the shown count to the dropped note and arrive at a total the trace
	// never had.
	sessSub := plural(len(r.sessions), "session", "sessions")
	if r.sessDropped > 0 {
		total := strconv.Itoa(len(r.sessions) + r.sessDropped)
		if r.sessDroppedOver {
			total = "at least " + total
		}
		sessSub = fmt.Sprintf("%d of %s sessions", len(r.sessions), total)
	}
	badSub := "JSONL lines that would not parse"
	if r.Oversize > 0 {
		// Two different facts, two different sentences: an over-long line may be
		// a perfectly good record that we would not hold.
		badSub += fmt.Sprintf(" · %d over %s, skipped", r.Oversize, esc(fmtBytes(reportMaxLine)))
	}
	// The values here are already escaped or are numbers we formatted ourselves;
	// cls is the only status colour on the header, and it never travels alone —
	// the number and its explanation sit right under it.
	tiles := []struct{ k, v, sub, cls string }{
		// Not "of it in models": delegations run concurrently (engine.go runs a
		// reply's parallel-safe calls at once), so the sum of every turn's duration
		// routinely exceeds the wall clock it would have to fit inside.
		{"wall clock", fmtSpan(r.wall()),
			fmt.Sprintf("%s in models, summed across %s", fmtMs(r.TurnMs), plural(len(r.sessions), "session", "sessions")), ""},
		{"turns", strconv.Itoa(r.Turns), sessSub, ""},
		{"tokens in", esc(kfmt(r.In)), "prompt tokens", ""},
		{"tokens out", esc(kfmt(r.Out)), "completion tokens", ""},
		{"cache hit", cache, cacheSub, ""},
		{"invalid calls", pct(r.Invalid, r.Attempted), fmt.Sprintf("%d of %d attempted", r.Invalid, r.Attempted), zeroMute(r.Invalid, "warn")},
		{"tool calls", strconv.Itoa(r.Calls), fmt.Sprintf("%d failed", r.CallErrs), ""},
		{"tasks", fmt.Sprintf("%d / %d", r.Passed, r.Failed), "passed / failed", zeroMute(r.Failed, "bad")},
		{"fallbacks", strconv.Itoa(r.Falls), fmt.Sprintf("%d wait · %d switch · %d repeat", r.Waits, r.Switches, r.Repeats), ""},
		{"unreadable", strconv.Itoa(r.Bad), badSub, zeroMute(r.Bad, "bad")},
		{"cost", cost, costSub, ""},
	}
	o.s("<div class=\"tiles\">\n")
	for _, t := range tiles {
		o.p("<div class=\"tile\"><div class=\"k\">%s</div><div class=\"v%s\">%s</div><div class=\"sub\">%s</div></div>\n",
			esc(t.k), withSpace(t.cls), t.v, t.sub)
	}
	o.s("</div>\n")

	rows := [][2]string{
		{"roles", r.Roles.join()}, {"models", r.Models.join()}, {"members", r.Members.join()},
		{"tiers", r.Tiers.join()}, {"transports", r.Transports.join()},
		{"records", fmt.Sprintf("%d turn · %d task · %d step · %d unknown", r.Turns, r.TaskCount, r.StepCount, r.Unknown)},
	}
	if r.CompactTurns > 0 {
		// Compaction is the cheap role warming a long prefix, so its cache share is
		// always high and always flatters the tile above. It is counted there —
		// it is spend the run incurred — and printed here so the figure lca eval
		// prints for the same trace, which leaves these turns out, is reachable by
		// subtraction instead of looking like a disagreement.
		rows = append(rows, [2]string{"of that, compaction", fmt.Sprintf("%s · %s in · %s out · %s cached — counted above; <code>lca eval</code> leaves it out as housekeeping",
			plural(r.CompactTurns, "turn", "turns"), esc(kfmt(r.CompactIn)), esc(kfmt(r.CompactOut)), esc(kfmt(r.CompactCached)))})
	}
	o.s("<table>\n")
	for _, kv := range rows {
		o.p("<tr><th>%s</th><td class=\"w\">%s</td></tr>\n", esc(kv[0]), kv[1])
	}
	o.s("</table>\n")
	if r.sessDropped > 0 {
		at := ""
		if r.sessDroppedOver {
			at = "at least "
		}
		o.p("<p class=\"meta warn\">%s%d further sessions are counted in the totals but not shown (cap %d).</p>\n", at, r.sessDropped, reportSessions)
	}
}

// fmtBytes is a file size, not a token count, so it does not reuse kfmt.
func fmtBytes(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(n, 10) + "B"
	case n < 1<<20:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	case n < 1<<30:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	}
}

// renderTimeline draws one activity strip per session: x is wall-clock time
// across the whole trace, one column per reportBuckets slice of it, and the
// column's shade is how many turns that session took in that slice. A single
// measure on a single axis — a run's shape is "who was busy when", and that is
// all this chart claims.
//
// A bucket holding a turn that errored is drawn red AND full height, so the
// exception is never carried by colour alone.
func (r *report) renderTimeline(o htmlOut) {
	if len(r.roots) == 0 || r.First.IsZero() {
		return
	}
	const cw, bw, rh, bh = 8, 6, 15, 9
	width := cw * reportBuckets
	o.s("<section>\n<div class=\"head\"><h2>activity</h2>")
	o.p("<span class=\"note\">%s per column · shade = turns in that column</span></div>\n", fmtSpan(r.wall()/reportBuckets))
	r.walk(func(s *repSession, depth int) {
		if s.hits == nil {
			return
		}
		o.s("<div class=\"tl\">")
		o.p("<div class=\"lbl\">%s%s <span class=\"faint\">%s</span></div>", strings.Repeat("· ", depth), esc(s.UID), esc(s.Kind))
		o.p("<svg viewBox=\"0 0 %d %d\" preserveAspectRatio=\"none\" role=\"img\" aria-label=\"%s\">", width, rh, plural(s.Turns, "turn", "turns"))
		o.p("<rect x=\"0\" y=\"%d\" width=\"%d\" height=\"1\" fill=\"#26262a\"/>", rh-1, width)
		for i, n := range s.hits {
			if n == 0 {
				continue
			}
			fill, h := "#2e5c5c", bh
			switch {
			case s.errs[i]:
				fill, h = "#d8635b", rh-1
			case n >= 5:
				fill = "#5fafaf"
			case n >= 2:
				fill = "#417f7f"
			}
			o.p("<rect x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" rx=\"1\" fill=\"%s\"/>", i*cw, rh-1-h, bw, h, fill)
		}
		o.s("</svg></div>\n")
	})
	o.p("<div class=\"axis\"><span>%s</span><span>%s</span><span>%s</span></div>\n",
		esc(r.stamp(r.First)), esc(r.stamp(r.First.Add(r.wall()/2))), esc(r.stamp(r.Last)))
	o.s("<div class=\"legend\">")
	for _, l := range [][2]string{{"#2e5c5c", "1 turn"}, {"#417f7f", "2–4 turns"}, {"#5fafaf", "5+ turns"}, {"#d8635b", "held a turn that errored (full height)"}} {
		o.p("<span><i style=\"background:%s\"></i>%s</span>", l[0], esc(l[1]))
	}
	o.s("</div>\n</section>\n")
}

// walk visits the session tree depth-first in trace order. The tree is acyclic
// by construction (link refuses a loop), so no depth guard is needed here.
func (r *report) walk(fn func(s *repSession, depth int)) {
	var rec func(s *repSession, depth int)
	rec = func(s *repSession, depth int) {
		fn(s, depth)
		for _, c := range s.children {
			rec(c, depth+1)
		}
	}
	for _, s := range r.roots {
		rec(s, 0)
	}
}

func (r *report) renderSessions(o htmlOut) {
	o.s("<section>\n<div class=\"head\"><h2>sessions</h2>")
	o.p("<span class=\"note\">root, subagents, delegations and the compaction helper · up to %d turns and %d tool calls detailed per session, %d and %d over the whole page</span></div>\n",
		reportTurnRows, reportToolRows, reportTotalTurnRows, reportTotalToolRows)
	if len(r.roots) == 0 {
		o.s("<p class=\"mute\">no session records in this trace.</p>\n</section>\n")
		return
	}
	for _, s := range r.roots {
		r.renderSession(o, s)
	}
	// The page's own budget states itself here, beside the sessions it thinned —
	// the per-session lines above each say what they lost, and this says what the
	// page as a whole refused to hold.
	if r.turnsCut > 0 || r.toolsCut > 0 {
		o.p("<p class=\"warn\">the page's row budget stopped %d turn rows and %d tool-call rows before their session's own cap did (%d and %d for the whole page); every one of them is still counted in the totals.</p>\n",
			r.turnsCut, r.toolsCut, reportTotalTurnRows, reportTotalToolRows)
	}
	o.s("</section>\n")
}

func (r *report) renderSession(o htmlOut, s *repSession) {
	kind := s.Kind
	if len(s.tasks) > 0 && kind == "subagent" {
		kind = "delegation"
	}
	ttft := "—"
	if s.TTFTn > 0 {
		ttft = fmtMs(s.TTFTSum / int64(s.TTFTn))
	}
	o.p("<details%s><summary>%s <span class=\"tag\">· %s · %s · %s in / %s out · cache %s · %s</span>",
		openIf(s.openByDefault()), esc(s.UID), esc(kind),
		plural(s.Turns, "turn", "turns"), esc(kfmt(s.In)), esc(kfmt(s.Out)), cacheShare(s.Cached, s.In), fmtMs(s.DurMs))
	if s.Errors > 0 {
		o.p(" <span class=\"bad\">· %s</span>", plural(s.Errors, "error", "errors"))
	}
	if s.Invalid > 0 {
		o.p(" <span class=\"warn\">· %s invalid</span>", strconv.Itoa(s.Invalid))
	}
	o.s("</summary>\n<div class=\"body\">\n")

	o.s("<table>\n")
	for _, kv := range [][2]string{
		{"roles", s.Roles.join()}, {"models", s.Models.join()}, {"members", s.Members.join()},
		{"tier", s.Tiers.join()}, {"transport", s.Transports.join()},
		{"window", esc(r.stamp(s.First)) + " → " + esc(r.stamp(s.Last))},
		{"mean ttft", ttft},
		{"parent", dash(s.Parent)},
	} {
		o.p("<tr><th>%s</th><td class=\"w\">%s</td></tr>\n", esc(kv[0]), kv[1])
	}
	o.s("</table>\n")

	if len(s.rows) > 0 {
		o.s("<details><summary>turns</summary><div class=\"body\">\n<table>\n")
		o.s("<tr><th>at</th><th class=\"n\">step</th><th>role</th><th>model</th><th>member</th><th>tier</th><th>transport</th>" +
			"<th class=\"n\">in</th><th class=\"n\">out</th><th class=\"n\">cached</th><th class=\"n\">ttft</th><th class=\"n\">took</th>" +
			"<th class=\"n\">tools</th><th>finish</th></tr>\n")
		for _, t := range s.rows {
			o.p("<tr><td>%s</td><td class=\"n\">%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td>",
				esc(r.stamp(t.TS)), t.Step, dash(t.Role), dash(t.Model), dash(t.Member), dash(t.Tier), dash(t.Transport))
			// The cached count and its share, both: three columns of raw numbers
			// with a bare percentage in the third reads as a token count until the
			// sign registers, and this is the only place the count itself appears.
			o.p("<td class=\"n\">%d</td><td class=\"n\">%d</td><td class=\"n\">%s</td>", t.In, t.Out, cachedCell(t.Cached, t.In))
			o.p("<td class=\"n\">%s</td><td class=\"n\">%s</td><td class=\"n\">%d</td>", msOrDash(t.TTFT), fmtMs(t.Dur), t.Tools)
			cls := "mute"
			word := t.Finish
			if t.Err != "" {
				cls, word = "bad", "error: "+t.Err
			} else if t.Invalid > 0 {
				cls, word = "warn", firstNonEmpty(t.Finish, "—")+fmt.Sprintf(" · %d invalid", t.Invalid)
			}
			o.p("<td class=\"w %s\">%s</td></tr>\n", cls, dash(word))
		}
		o.s("</table>\n")
		if s.rowsDropped > 0 {
			o.p("<p class=\"warn\">%d further turns of this session are counted in the totals but not listed (cap %d).</p>\n", s.rowsDropped, reportTurnRows)
		}
		o.s("</div></details>\n")
	}

	if len(s.tools) > 0 {
		o.p("<details><summary>tool calls <span class=\"tag\">· %d</span></summary><div class=\"body\">\n<table>\n", s.Tools)
		o.s("<tr><th class=\"n\">step</th><th>tool</th><th>status</th><th class=\"n\">took</th><th class=\"n\">result</th><th>arguments</th></tr>\n")
		for _, c := range s.tools {
			cls, word := "ok", "ok"
			switch {
			case c.Invalid:
				cls, word = "warn", "invalid"
			case !c.OK:
				cls, word = "bad", "failed"
			}
			o.p("<tr><td class=\"n\">%d</td><td>%s</td><td class=\"%s\">%s</td><td class=\"n\">%s</td><td class=\"n\">%dB</td><td class=\"w\">%s</td></tr>\n",
				c.Step, dash(c.Name), cls, word, fmtMs(c.Ms), c.Bytes, dash(c.Args))
			if c.Err != "" {
				o.p("<tr><td></td><td colspan=\"5\" class=\"w bad\">%s</td></tr>\n", esc(c.Err))
			}
		}
		o.s("</table>\n")
		if s.toolsDropped > 0 {
			o.p("<p class=\"warn\">%d further tool calls of this session are counted in the totals but not listed (cap %d).</p>\n", s.toolsDropped, reportToolRows)
		}
		o.s("</div></details>\n")
	}

	if len(s.tasks) > 0 {
		o.s("<details open><summary>verified tasks</summary><div class=\"body\">\n")
		r.taskTable(o, s.tasks, s.tasksDropped)
		o.s("</div></details>\n")
	}

	if len(s.children) > 0 {
		o.s("<div class=\"kids\">\n")
		for _, c := range s.children {
			r.renderSession(o, c)
		}
		o.s("</div>\n")
	}
	o.s("</div>\n</details>\n")
}

func openIf(b bool) string {
	if b {
		return " open"
	}
	return ""
}

// openByDefault decides whether a session's details render expanded. A browser
// lays out every open <details> at load, so on a fleet run with a hundred
// sessions opening all of them is what turns a readable page into one that
// stalls for minutes. A root opens because the shape of the run is the point; a
// small session opens because there is nothing to save; a deep, busy one waits to
// be asked, and its summary line already carries the numbers.
func (s *repSession) openByDefault() bool {
	if s.Turns == 0 && len(s.tasks) == 0 {
		return false
	}
	return s.Kind == "root" || len(s.rows)+len(s.tools)+len(s.tasks) <= reportOpenRows
}

// cachedCell is one turn's cached tokens and their share of its prompt, or a dash
// when the record reported no cache figure at all (see cacheShare).
func cachedCell(cached, in int) string {
	if cached == 0 {
		return "—"
	}
	return esc(kfmt(cached)) + " <span class=\"faint\">" + pct(cached, in) + "</span>"
}

// msOrDash keeps a missing measurement distinct from a fast one: trace.go
// writes ttft_ms 0 both for "no first-token time was observed" and for a turn
// that answered inside a millisecond, and only the first deserves a dash.
func msOrDash(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return fmtMs(ms)
}

func (r *report) renderTools(o htmlOut) {
	o.s("<section>\n<div class=\"head\"><h2>tools</h2>")
	o.p("<span class=\"note\">every call in the trace, aggregated by name · no row cap short of %d distinct names</span></div>\n", reportToolNames)
	if len(r.toolOrder) == 0 && r.toolOther == nil {
		o.s("<p class=\"mute\">no tool calls in this trace.</p>\n</section>\n")
		return
	}
	o.s("<table>\n<tr><th>tool</th><th class=\"n\">calls</th><th class=\"n\">failed</th><th class=\"n\">invalid</th>" +
		"<th class=\"n\">total time</th><th class=\"n\">mean</th><th class=\"n\">slowest</th><th class=\"n\">result bytes</th></tr>\n")
	rows := make([]*repToolAgg, 0, len(r.toolOrder)+1)
	for _, n := range r.toolOrder {
		rows = append(rows, r.toolAgg[n])
	}
	if r.toolOther != nil {
		rows = append(rows, r.toolOther)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Calls > rows[j].Calls })
	for _, a := range rows {
		mean := int64(0)
		if a.Calls > 0 {
			mean = a.Ms / int64(a.Calls)
		}
		o.p("<tr><td>%s</td><td class=\"n\">%d</td><td class=\"n %s\">%d</td><td class=\"n %s\">%d</td>",
			dash(a.Name), a.Calls, zeroMute(a.Errs, "bad"), a.Errs, zeroMute(a.Invalid, "warn"), a.Invalid)
		o.p("<td class=\"n\">%s</td><td class=\"n\">%s</td><td class=\"n\">%s</td><td class=\"n\">%s</td></tr>\n",
			fmtMs(a.Ms), fmtMs(mean), fmtMs(a.Max), esc(fmtBytes(int64(a.Bytes))))
	}
	o.s("</table>\n</section>\n")
}

// withSpace joins a class onto the one before it, or adds nothing at all — an
// empty class attribute value is valid but reads like a bug in view-source.
func withSpace(cls string) string {
	if cls == "" {
		return ""
	}
	return " " + cls
}

func zeroMute(n int, cls string) string {
	if n == 0 {
		return "faint"
	}
	return cls
}

func (r *report) renderInvalid(o htmlOut) {
	o.s("<section>\n<div class=\"head\"><h2>invalid tool calls</h2>")
	o.p("<span class=\"note\">%s of %s attempted · up to %d shown with the text the model actually emitted</span></div>\n",
		pct(r.Invalid, r.Attempted), strconv.Itoa(r.Attempted), reportInvalids)
	if len(r.invalids) == 0 {
		o.s("<p class=\"mute\">none — every tool call in this trace parsed.</p>\n</section>\n")
		return
	}
	for _, v := range r.invalids {
		o.p("<details open><summary><span class=\"warn\">%s</span> <span class=\"tag\">· %s · step %d · %s · %s</span></summary>\n",
			dash(firstNonEmpty(v.Name, "no call parsed")), esc(r.stamp(v.TS)), v.Step, dash(v.Role), dash(v.Model))
		o.s("<div class=\"body\">\n")
		o.p("<p class=\"mute\">session %s</p>\n", dash(v.Session))
		if v.NoCall {
			o.s("<p class=\"mute\">the model emitted a call the parser could not turn into one at all, so there are no arguments to show — only the reply.</p>\n")
		}
		if v.Args != "" {
			o.p("<p class=\"mute\">arguments as parsed</p>\n<pre class=\"plain\">%s</pre>\n", esc(v.Args))
		}
		if v.Raw != "" {
			o.p("<p class=\"mute\">raw call text</p>\n<pre>%s</pre>\n", esc(v.Raw))
		}
		if v.Reply != "" {
			o.p("<p class=\"mute\">the reply it came in</p>\n<pre>%s</pre>\n", esc(v.Reply))
		}
		if v.Raw == "" && v.Reply == "" {
			o.s("<p class=\"mute\">this record carries no raw text — it was written by a build that did not keep it.</p>\n")
		}
		o.s("</div>\n</details>\n")
	}
	if r.invalidDropped > 0 {
		o.p("<p class=\"warn\">%d further invalid calls are counted in the totals but not shown (cap %d).</p>\n", r.invalidDropped, reportInvalids)
	}
	o.s("</section>\n")
}

func (r *report) renderGateway(o htmlOut) {
	o.s("<section>\n<div class=\"head\"><h2>gateway</h2>")
	o.p("<span class=\"note\">waits, model switches and repeated turns · %s waited in all · up to %d shown</span></div>\n",
		fmtMs(r.WaitMs), reportFallbacks)
	if len(r.falls) == 0 {
		o.s("<p class=\"mute\">none — every turn was answered by the model it was sent to, first try.</p>\n</section>\n")
		return
	}
	o.s("<table>\n<tr><th>at</th><th>kind</th><th>from</th><th>to</th><th>reason</th><th class=\"n\">waited</th><th>session</th><th class=\"n\">step</th></tr>\n")
	for _, f := range r.falls {
		cls, to := "warn", dash(f.To)
		switch f.Kind {
		case "switch":
			cls = "bad"
		case "repeat":
			// A repeated turn is not a failure: nothing had executed, and the
			// prefix is still in the KV cache (gwpolicy.go).
			to = esc(f.From) + " <span class=\"faint\">(same model)</span>"
		}
		o.p("<tr><td>%s</td><td class=\"%s\">%s</td><td>%s</td><td class=\"w\">%s</td><td class=\"w\">%s</td><td class=\"n\">%s</td><td class=\"w\">%s</td><td class=\"n\">%d</td></tr>\n",
			esc(r.stamp(f.TS)), cls, esc(f.Kind), dash(f.From), to, dash(f.Reason), msOrDash(f.WaitMs), dash(f.Session), f.Step)
	}
	o.s("</table>\n")
	if r.fallDropped > 0 {
		o.p("<p class=\"warn\">%d further fallback events are counted in the totals but not listed (cap %d).</p>\n", r.fallDropped, reportFallbacks)
	}
	o.s("</section>\n")
}

func (r *report) renderTasks(o htmlOut) {
	o.s("<section>\n<div class=\"head\"><h2>verified tasks</h2>")
	o.p("<span class=\"note\">decided by the verifier, not by a model · %d passed · %d failed · %d other · up to %d shown</span></div>\n",
		r.Passed, r.Failed, r.OtherTasks, reportTaskRows)
	if len(r.tasks) == 0 {
		o.s("<p class=\"mute\">no task records in this trace.</p>\n</section>\n")
		return
	}
	r.taskTable(o, r.tasks, r.taskDropped)
	o.s("</section>\n")
}

func (r *report) taskTable(o htmlOut, rows []repTaskRow, dropped int) {
	o.s("<table>\n<tr><th>at</th><th>status</th><th>role</th><th>member</th><th class=\"n\">attempts</th><th class=\"n\">check exit</th>" +
		"<th>check</th><th class=\"n\">took</th><th class=\"n\">diff</th><th class=\"n\">files</th><th>applied</th><th>review</th><th>task</th></tr>\n")
	for _, t := range rows {
		rec := t.Rec
		exit := "—"
		if rec.CheckExit != nil {
			exit = strconv.Itoa(*rec.CheckExit)
		}
		applied, appliedCls := "no", "warn"
		if rec.Applied {
			applied, appliedCls = "yes", "ok"
		}
		review := "—"
		if rec.ReviewVerdict != "" {
			review = fmt.Sprintf("<span class=\"%s\">%s</span> <span class=\"faint\">%s</span>",
				statusClass(rec.ReviewVerdict), esc(rec.ReviewVerdict), dash(firstNonEmpty(rec.Reviewer, rec.ReviewModel)))
		}
		member := dash(rec.Member)
		if rec.CallerMember != "" {
			member += " <span class=\"faint\">→ " + esc(rec.CallerMember) + "</span>"
		}
		o.p("<tr><td>%s</td><td class=\"%s\">%s</td><td>%s</td><td class=\"w\">%s</td><td class=\"n\">%d</td><td class=\"n\">%s</td>",
			esc(r.stamp(t.TS)), statusClass(rec.Status), dash(rec.Status), dash(rec.Role), member, rec.Attempts, exit)
		o.p("<td class=\"w\"><code>%s</code></td><td class=\"n\">%s</td><td class=\"n\">%s</td><td class=\"n\">%d</td><td class=\"%s\">%s</td><td class=\"w\">%s</td><td class=\"w\">%s</td></tr>\n",
			dash(rec.CheckCmd), fmtMs(rec.DurationMs), esc(fmtBytes(int64(rec.DiffBytes))), rec.FilesChanged, appliedCls, applied, review, dash(rec.Task))
	}
	o.s("</table>\n")
	if dropped > 0 {
		o.p("<p class=\"warn\">%d further tasks are counted in the totals but not listed (cap %d).</p>\n", dropped, reportTaskRows)
	}
}

func (r *report) renderRuns(o htmlOut) {
	o.s("<section>\n<div class=\"head\"><h2>workflow runs</h2>")
	o.p("<span class=\"note\">%s · up to %d steps listed per run</span></div>\n", plural(len(r.runOrder), "run", "runs"), reportStepRows)
	if len(r.runOrder) == 0 {
		o.s("<p class=\"mute\">no workflow steps in this trace — this was not an <code>lca run</code>.</p>\n</section>\n")
		return
	}
	for _, id := range r.runOrder {
		run := r.runs[id]
		o.p("<details open><summary>%s <span class=\"tag\">· workflow %s · <span class=\"ok\">%d ok</span> · <span class=\"%s\">%d failed</span> · %d skipped · %s</span></summary>\n",
			esc(run.Run), dash(run.Workflow), run.OK, zeroMute(run.Failed, "bad"), run.Failed, run.Skipped, fmtMs(run.DurMs))
		o.s("<div class=\"body\">\n<table>\n")
		for _, kv := range [][2]string{
			{"window", esc(r.stamp(run.First)) + " → " + esc(r.stamp(run.Last))},
			{"roles", run.Roles.join()}, {"members", run.Members.join()},
			{"tokens", esc(kfmt(run.In)) + " in · " + esc(kfmt(run.Out)) + " out · cache " + cacheShare(run.Cached, run.In)},
		} {
			o.p("<tr><th>%s</th><td class=\"w\">%s</td></tr>\n", esc(kv[0]), kv[1])
		}
		o.s("</table>\n<table>\n")
		o.s("<tr><th class=\"n\">#</th><th>step</th><th>kind</th><th>status</th><th>role</th><th>member</th><th>model</th>" +
			"<th class=\"n\">exit</th><th class=\"n\">check exit</th><th class=\"n\">attempts</th><th class=\"n\">took</th><th>review</th><th>detail</th></tr>\n")
		for _, s := range run.Steps {
			exit := "—"
			if s.CheckExit != nil {
				exit = strconv.Itoa(*s.CheckExit)
			}
			review := "—"
			if s.Review != "" {
				review = fmt.Sprintf("<span class=\"%s\">%s</span> <span class=\"faint\">%s</span>", statusClass(s.Review), esc(s.Review), dash(s.Reviewer))
			}
			o.p("<tr><td class=\"n\">%d</td><td>%s</td><td>%s</td><td class=\"%s\">%s</td><td>%s</td><td>%s</td><td>%s</td>",
				s.Index, dash(s.Step), dash(s.Kind), statusClass(s.Status), dash(s.Status), dash(s.Role), dash(s.Member), dash(s.Model))
			o.p("<td class=\"n\">%s</td><td class=\"n\">%s</td><td class=\"n\">%d</td><td class=\"n\">%s</td><td class=\"w\">%s</td><td class=\"w\">%s</td></tr>\n",
				stepExit(s), exit, s.Attempts, fmtMs(s.DurationMs), review, dash(s.Detail))
			if s.Check != "" {
				o.p("<tr><td></td><td colspan=\"12\" class=\"w mute\">check <code>%s</code></td></tr>\n", esc(s.Check))
			}
		}
		o.s("</table>\n")
		if run.Dropped > 0 {
			o.p("<p class=\"warn\">%d further steps of this run are counted in the totals but not listed (cap %d).</p>\n", run.Dropped, reportStepRows)
		}
		o.s("</div>\n</details>\n")
	}
	if r.runDropped > 0 {
		o.p("<p class=\"warn\">%d step records belong to runs past the cap of %d and are not shown.</p>\n", r.runDropped, reportRuns)
	}
	o.s("</section>\n")
}

// stepExit is a step's process exit status, or a dash for a step that ran no
// process. StepRecord.Exit is a plain int, so a prompt or a delegate that
// succeeded carries a zero that means nothing — printing it under "exit" tells
// an author their command succeeded for a step that had no command. A non-zero
// value is always shown: only a failure to start sets it (workflow.go).
func stepExit(s StepRecord) string {
	if s.Kind != stepRun && s.Exit == 0 {
		return "—"
	}
	return strconv.Itoa(s.Exit)
}

func (r *report) renderFooter(o htmlOut) {
	o.s("<footer>\n")
	o.p("<p>BEHELIT · generated by <code>lca report</code> at %s · one file, no scripts, no network.</p>\n", esc(clock(r.Generated)))
	o.p("<p>Read %s over %s. A field the trace does not carry is shown as an em dash and is never guessed; "+
		"a line that would not parse, and one too long to hold, are each counted and named, not dropped. "+
		"Detail lists are capped at %d turns and %d tool calls per session, %d tasks per session, "+
		"%d invalid calls, %d fallback events, %d tasks and %d steps per run — every truncation says so where it happens.</p>\n",
		plural(r.Lines, "line", "lines"), plural(len(r.Sources), "trace file", "trace files"),
		reportTurnRows, reportToolRows, reportTaskRows, reportInvalids, reportFallbacks, reportTaskRows, reportStepRows)
	// The page's size has to be predictable from the page itself, or "capped" is
	// a claim about one session and not about the file you are holding.
	o.p("<p>The page is bounded as a whole, not only per session: at most %d sessions, and across all of them at most "+
		"%d turn rows and %d tool-call rows. Sessions past those are still counted in every total above.</p>\n",
		reportSessions, reportTotalTurnRows, reportTotalToolRows)
	o.s("</footer>\n")
}
