package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The short version, for the two places a person actually reads: the merge
// request's description and the comment on the ticket.
//
// There is a full transcript and an HTML report already, and neither is what a
// reviewer opens. A transcript is the whole conversation and the report is a
// browser tab; what the wrapper needs to paste is five hundred words that say
// what changed, how it was checked, what is still wrong and what it cost — and
// the model that just did the work is the only thing in the room that can write
// the "what is still wrong" line.
//
// So the summary is the model's, written in ONE short call after the verdict,
// and everything that must be exact is lca's: the file list, the check command
// and its exit, the attempt count, the cost and the paths. A model asked to
// recall a token count invents one.
//
// It is written for a failed run too. A failed run is the one that gets read.

const (
	// summaryMax is the ceiling the requirement names: "markdown up to about
	// 3 KB". It is a hard byte limit and not a target, because a Jira comment and
	// an MR description both have limits of their own and the wrapper pastes this
	// without looking at it.
	summaryMax = 3 * 1024
	// summaryBudget is the longest that one closing call may take when the run has
	// all the time in the world, and summaryGrace is all it gets when the run's own
	// clock has already run out. Both are in budget.go's closingDeadline, which is
	// what keeps `-timeout 30s` honest.
	summaryBudget = 60 * time.Second
	summaryGrace  = 3 * time.Second
	// summaryMaxTokens bounds the closing generation itself. The prompt asks for
	// 2500 bytes and 1000 tokens is generous for that in any language; a model
	// that wants more is writing something that will be clamped away anyway.
	summaryMaxTokens = 1000
)

const summarySystem = `You write the closing summary of one automated coding run. Your reader is a
human reviewing a merge request or a ticket comment — not an agent, and not the
operator of the harness. Write plain markdown, at most 2500 bytes, no preamble
and no sign-off.

Use exactly these sections, in this order, and write "(none)" under one that has
nothing in it:

## What changed
One line per file: the path, then what changed in it and why, in one clause.

## How it was verified
The check command, its exit code and how many attempts it took, then one line on
what the output actually showed.

## What did not work
What is still broken, what you could not reach, what you would do next. Be
specific and do not soften it. On a failed run this is the section that matters.

Facts are given to you below. Do not invent paths, numbers, commands or exit
codes, and do not repeat the cost or the file links — the harness appends those.`

// summaryFacts is everything lca knows for certain about the run. The model is
// handed this and asked only to explain it; nothing in it is the model's to
// recall, because every number here is one lca counted while it ran.
type summaryFacts struct {
	role     string
	status   string
	reason   string
	check    string
	exit     int
	checked  bool
	tail     string
	attempts int

	files []fileEdit
	turns int
	tools int
	errs  int

	promptTok, outputTok int
	elapsed              time.Duration

	transcript, trace string
}

// fileEdit is one file as the summary lists it: what the model's own tools did
// to it, counted the way /diff counts it.
type fileEdit struct {
	name           string
	added, removed int
	created        bool
	deleted        bool
}

// sessionFileEdits is sessionChangeStats' per-file half: the same first-touch
// snapshot, reported file by file so the summary can keep its promise of one
// line each. It is lca's own account of its work and deliberately not `git
// diff` — the wrapper owns git, and a tree with somebody else's uncommitted
// work in it would make git's numbers say things about this run that are false.
func sessionFileEdits() []fileEdit {
	changeMu.Lock()
	defer changeMu.Unlock()
	type orig struct {
		before  []byte
		existed bool
		name    string
	}
	firsts := map[string]*orig{}
	var order []string
	for _, c := range changeLog {
		if _, ok := firsts[c.abs]; !ok {
			firsts[c.abs] = &orig{c.before, c.existed, c.name}
			order = append(order, c.abs)
		}
	}
	var out []fileEdit
	for _, abs := range order {
		o := firsts[abs]
		cur, _ := os.ReadFile(abs)
		was := ""
		if o.existed {
			was = string(o.before)
		}
		if was == string(cur) {
			continue // touched and left exactly as it was: not a change
		}
		add, del := countLineDiff(was, string(cur))
		out = append(out, fileEdit{name: o.name, added: add, removed: del,
			created: !o.existed, deleted: o.existed && len(cur) == 0})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// countLineDiff is lineDiff's counts without its rendering. lineDiff's output is
// a TERMINAL rendering — colour escapes, three lines of context — and a number
// taken from it would depend on the theme.
func countLineDiff(was, now string) (added, removed int) {
	a, b := splitLines(was), splitLines(now)
	// The same ceiling lineDiff uses: above it the LCS walk is O(n·m) over a
	// generated file nobody will read either, and both sides' own length is the
	// honest answer — every line of the new file added, every line of the old one
	// removed. (That order, b then a, is deliberate: lineDiff's own fallback
	// returns them the other way round, which reads as a slip and is not worth
	// copying into a number somebody puts in a merge request.)
	if len(a) > 5000 || len(b) > 5000 {
		return len(b), len(a)
	}
	for _, o := range diffOps(a, b) {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

// writeSummary is the whole of -summary: gather the facts, ask the model once,
// append the parts that must be exact, clamp to 3 KB and write the file.
//
// It returns the path it wrote, or "" and nothing else. A summary is an extra,
// and a run whose work is done must not fail because a markdown file could not
// be written — the JSON result, the transcript and the trace are the contract.
func (o *Orchestrator) writeSummary(s *Session, f summaryFacts) string {
	path := o.summary
	if path == "" {
		return ""
	}
	// The one exception the requirement names, and the one that is the same
	// exception: an infra_error before the first model reply, and lca having been
	// called wrong, with no reply either. In both the run decided NOTHING about
	// the task — oneShot writes no result object for the second one at all — so
	// there is nothing to summarise and, with no gateway behind it, nobody to write
	// it. A file saying "the gateway was not there" is the reason field in a second
	// place and one more gateway call spent finding out.
	// Gated on a reply having ARRIVED, not on a request having been sent.
	// stats.Turns is incremented whether or not the request succeeded, so it is
	// never 0 once anything has been attempted and this branch could not fire from
	// a real run: a dead endpoint still spent one more round trip discovering it
	// was still dead, and still wrote a file whose "what did not work" section was
	// the reason field repeated.
	if s.stats.Replies == 0 && (f.status == statusInfra || f.status == statusConfig) {
		return ""
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			warnLine("-summary %s: %v", path, err)
			return ""
		}
	}
	// Everything that reaches this file is sanitised, and it is done HERE so the
	// fallback path is covered with the model's prose. This is the one artefact
	// lca writes to be pasted somewhere else — into a merge request description, a
	// wider audience than the transcript's Jira attachment — and the model is
	// handed its own tool RESULTS and told to be specific about what did not work.
	// So `stage/stage.sh logs gw` printing an Authorization header, quoted back
	// under "What did not work", is the ordinary path from a live token to a public
	// description. ANSI and C0 controls go the same way: a build tool emits colour
	// whether or not anybody is watching, and a NUL in the middle of a comment
	// body is an API call the wrapper cannot make.
	//
	// All THREE fields that carry text from outside this program, and not just the
	// one the model quotes back. `check` is the operator's own command line and
	// `reason` quotes it — `sh -c 'curl -H "Authorization: Bearer $T"'`, with the
	// shell having expanded $T before lca ever saw it — and both of them reach this
	// file twice over: through the prompt below, and through skeleton() on the
	// fallback path, which runs AFTER the model's prose has been scrubbed and so
	// was never covered by scrubbing only the prose. Doing it on the facts instead
	// of on the output covers every path out of them by construction, including the
	// one request this file sends to the gateway.
	f.tail = forPublication(f.tail)
	f.check = forPublication(f.check)
	f.reason = forPublication(f.reason)
	body := forPublication(o.askForSummary(s, f))
	if strings.TrimSpace(body) == "" {
		// The model did not answer — the gateway went away mid-run, or the clock ran
		// out before it finished a sentence. The facts are still facts, so lca writes
		// them itself: the criterion is that a summary EXISTS, and a wrapper that
		// finds an empty file has learnt nothing about the run.
		body = f.skeleton()
	}
	out := clampMarkdown(body, summaryMax-len(f.footer())) + f.footer()
	if len(out) > summaryMax {
		// Only reachable when the footer ALONE is bigger than the whole limit — a
		// transcript path long enough to be a problem of its own. The byte ceiling is
		// the promise the wrapper relies on, so it wins over the footer's shape — but
		// the cut still backs up to a rune boundary, for the reason clampMarkdown
		// gives.
		out = trimToRune(out[:summaryMax])
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		warnLine("-summary %s: %v", path, err)
		return ""
	}
	s.event("summary", map[string]any{"path": path, "bytes": len(out), "status": f.status})
	return path
}

// askForSummary is the one short call. It is a SEPARATE request with its own
// system prompt, exactly as compaction is: this session's cached prefix (its
// system prompt and its tool schemas) is not touched, so the gateway's KV cache
// for the run is still warm for whatever comes next, and no tool schema is
// offered to a model that must only write prose.
func (o *Orchestrator) askForSummary(s *Session, f summaryFacts) string {
	ctx, cancel := context.WithDeadline(context.Background(), o.budget.closingDeadline(time.Now()))
	defer cancel()
	start := time.Now()
	res, fb, err := s.chat(ctx, ChatRequest{Messages: []Message{
		{Role: "system", Content: summarySystem},
		{Role: "user", Content: f.prompt() + conversationTail(s.Msgs)},
		// Bounded, because the prompt asking for 2500 bytes is a request and not a
		// limit. Without this the only thing stopping that generation on a run with
		// no -timeout was summaryBudget's 60 seconds, so the ceiling the operator
		// wrote was overshot by however much the model felt like writing — on the
		// run that had just been stopped for being out of tokens.
	}, Thinking: "off", MaxTokens: summaryMaxTokens}, StreamSink{})
	s.traceTurn(0, res, fb, start, nil, err)
	// Counted whether or not it answered: a request that timed out mid-stream was
	// still bought. The RUN's budget as well as the session's — the two used to
	// disagree by exactly this call, so the trace's budget_exceeded event reported
	// half the number the result object did for the same run.
	s.orch.budget.spend(res.Usage.PromptTokens, res.Usage.CompletionTokens, res.Usage.CachedTokens)
	s.stats.PromptTokens += res.Usage.PromptTokens
	s.stats.CachedTokens += res.Usage.CachedTokens
	s.stats.OutputTokens += res.Usage.CompletionTokens
	if err != nil {
		s.event("summary_failed", map[string]any{"err": err.Error()})
		return ""
	}
	return strings.TrimSpace(reThinkBlock.ReplaceAllString(res.Content, ""))
}

// prompt is the facts, as text, with the end of the check output. The tail is
// clipped hard: this is a short call on purpose, and 20 000 lines of a failed
// build in it would cost more than the run it is summarising.
func (f summaryFacts) prompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Role: %s\nVerdict: %s\n", f.role, f.status)
	if f.reason != "" {
		fmt.Fprintf(&b, "Why: %s\n", f.reason)
	}
	switch {
	case f.check == "":
		b.WriteString("Check: none was given, so nothing verified this run.\n")
	case f.checked:
		fmt.Fprintf(&b, "Check: `%s` — exit %d after %s\n", f.check, f.exit,
			plural(f.attempts, "attempt", "attempts"))
	default:
		fmt.Fprintf(&b, "Check: `%s` — it never ran to completion\n", f.check)
	}
	b.WriteString("\nFiles this run changed, with line counts:\n")
	if len(f.files) == 0 {
		b.WriteString("- (none: no file was changed)\n")
	}
	for _, e := range f.files {
		fmt.Fprintf(&b, "- %s +%d -%d%s\n", e.name, e.added, e.removed, e.tag())
	}
	if f.tail != "" {
		fmt.Fprintf(&b, "\nLast lines of the check output:\n```\n%s\n```\n", clipText(f.tail, 2000))
	}
	fmt.Fprintf(&b, "\n%d turns, %d tool calls, %d of them errors.\n", f.turns, f.tools, f.errs)
	return b.String()
}

// conversationTail is the end of the run in the model's own words. Without it
// the model can restate the facts but not what it TRIED, and "what did not work"
// is the section a failed run gets read for.
//
// Bounded hard, because this is one SHORT call: the whole transcript would cost
// more than the run it is summarising, and the useful part of it is at the end
// anyway — the last attempt, its tool results and whatever the model said about
// them.
func conversationTail(msgs []Message) string {
	const keepMsgs = 8
	if len(msgs) > 1 {
		msgs = msgs[1:] // the system prompt is not a thing that happened
	}
	if len(msgs) > keepMsgs {
		msgs = msgs[len(msgs)-keepMsgs:]
	}
	tail := strings.TrimSpace(serializeForSummary(msgs))
	if tail == "" {
		return ""
	}
	return "\nThe end of the run, in your own words and your tools' results:\n<conversation>\n" +
		clipText(tail, 4000) + "\n</conversation>\n"
}

func (e fileEdit) tag() string {
	switch {
	case e.created:
		return " (new)"
	case e.deleted:
		return " (deleted)"
	}
	return ""
}

// skeleton is what gets written when the model could not: the same sections, the
// same order, filled from the facts alone. It is terse on purpose — it is a
// fallback, not a second implementation of the summary.
func (f summaryFacts) skeleton() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n", f.role, f.status)
	if f.reason != "" {
		fmt.Fprintf(&b, "\n%s\n", f.reason)
	}
	b.WriteString("\n## What changed\n\n")
	if len(f.files) == 0 {
		b.WriteString("(none)\n")
	}
	for _, e := range f.files {
		fmt.Fprintf(&b, "- `%s` +%d -%d%s\n", e.name, e.added, e.removed, e.tag())
	}
	b.WriteString("\n## How it was verified\n\n")
	switch {
	case f.check == "":
		b.WriteString("(none) — no check was given, so nothing verified this run.\n")
	case f.checked:
		fmt.Fprintf(&b, "`%s` — exit %d after %s.\n", f.check, f.exit, plural(f.attempts, "attempt", "attempts"))
	default:
		fmt.Fprintf(&b, "`%s` — it never ran to completion.\n", f.check)
	}
	b.WriteString("\n## What did not work\n\n")
	switch {
	case f.status == statusPassed:
		b.WriteString("(none)\n")
	case f.tail != "":
		fmt.Fprintf(&b, "The run ended %s. The end of the check output:\n\n```\n%s\n```\n",
			f.status, clipText(lastLines(f.tail, 12, 600), 600))
	default:
		fmt.Fprintf(&b, "The run ended %s. The model wrote no summary of its own — see the transcript.\n", f.status)
	}
	return b.String()
}

// footer is the part no model writes: what it cost and where to look. Both have
// to be exact — a wrapper pastes these paths into a ticket — so they are
// appended after the model's prose and counted against the 3 KB first.
func (f summaryFacts) footer() string {
	var b strings.Builder
	b.WriteString("\n## What it cost\n\n")
	fmt.Fprintf(&b, "- %s wall clock, %d turns, %d tool calls\n", f.elapsed.Round(time.Second), f.turns, f.tools)
	fmt.Fprintf(&b, "- %s prompt + %s completion tokens\n", kfmt(f.promptTok), kfmt(f.outputTok))
	b.WriteString("\n## Where to look\n\n")
	if f.transcript != "" {
		fmt.Fprintf(&b, "- transcript: `%s`\n", f.transcript)
	}
	if f.trace != "" {
		fmt.Fprintf(&b, "- trace: `%s`\n", f.trace)
		// The report is a FILE, named here rather than described, because the point of
		// a link is that somebody can open it. It does not exist until it is rendered
		// — the trace is what the run writes — so the command that makes it is on the
		// same line, and the path is the one that command will use by default.
		fmt.Fprintf(&b, "- report: `%s` — `lca report %s` renders it\n",
			defaultReportPath([]string{f.trace}, ""), f.trace)
	}
	return b.String()
}

// clampMarkdown cuts the body to fit, at a line boundary, with a line saying so.
// A markdown file truncated mid-sentence reads as a bug in lca; one that says
// "[truncated]" reads as a long run, which is the truth.
func clampMarkdown(s string, limit int) string {
	s = strings.TrimRight(s, "\n") + "\n"
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit {
		return s
	}
	const note = "\n[truncated to fit the summary's 3 KB limit — the transcript has the rest]\n"
	room := limit - len(note)
	if room <= 0 {
		return ""
	}
	cut := s[:room]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	// And then back up to a RUNE boundary. The line-boundary cut above is the
	// usual case, but a model writing Russian prose writes its first paragraph as
	// one long line, and then there is no newline inside the limit at all and the
	// raw byte cut stands — landing in the middle of a two- or four-byte rune.
	// What the wrapper does next is read this file to put it in a merge request
	// description, so an invalid final byte is a UnicodeDecodeError, or a rejected
	// GitLab API call, on a run that otherwise passed and whose work is already
	// done.
	return trimToRune(cut) + note
}

// trimToRune drops the trailing bytes of an incomplete rune, and nothing else.
func trimToRune(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
