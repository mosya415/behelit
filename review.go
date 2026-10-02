package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The reviewer run on its own against a finished tree, and the per-line verdict
// a merge request needs.
//
// Inside a delegation a reviewer ends its reply with `VERDICT: approve|reject`
// (delegate.go) and that is enough: the only consumer is the apply, and the only
// question it asks is whether the diff may land. GitLab is a different consumer.
// It wants the verdict AND a comment on a file and a line, which is the one
// thing a word on the last line cannot carry — so the reviewer also has to be
// runnable by itself, after the coder's worktree is finished:
//
//	lca -y -role reviewer -json -prompt-file review.md -diff-base origin/main
//
// HOW THE OBJECT COMES BACK, and why it is not a tool. The reviewer's FINAL
// REPLY is the object, and the instruction asking for it travels in the task's
// user message. A `review` tool with the schema in it was the obvious shape and
// is the one shape that is not available: the gateway keys its KV cache on the
// request prefix — the system prompt plus the tool schemas, computed once per
// session and identical for every session of every role — so a tool only the
// reviewer needs is a cache miss on the first request of every other role in the
// team. A user message is not in the prefix. Nothing above it moves, which is
// also what lets round two on the same session (-session) still hit the cache.
//
// WHAT IS CHECKED, and why here rather than in the wrapper. Every file:line is
// matched against the DIFF. A model reading two thousand lines of diff writes
// down the number it can see, which is the line in the file it opened and not a
// line the hunk covers; GitLab answers a position that is not in the diff with
// one 400 for the whole review, after the pipeline has already decided. So a
// comment lca cannot place is either a format mistake — one retry, named
// precisely — or it is moved into `summary`. It is never dropped: a reviewer that
// found something must not be silenced by its own arithmetic.
//
// AND AN UNREADABLE ANSWER IS `failed`, never an approve. A reviewer whose
// output could not be read has not approved anything, and that is the one
// failure mode in this pipeline that lets a bad change through unseen.
//
// WHAT THIS DELIBERATELY DOES NOT DO: it does not take the edit tool away from
// the reviewer. The in-delegation reviewer has it denied in-process, because the
// diff is about to be applied by the same process and a reviewer grading its own
// patch is not a review. Here the tree belongs to the wrapper, and denying a
// tool would change the role's TOOL SCHEMAS, which is the request prefix — so a
// review run and any other run of the same role would key two different caches,
// for a guard that belongs where the other hard boundaries already are: the
// reviewer role's `tools:` list, or a `permission:` block on it with
// `edit: deny`, and the sandbox profile. `files_changed` in the result object is
// 0 on a review that kept its hands off, which is the wrapper's own check.

// The four severities, exactly as the requirements document spells them. A
// wrapper switches on this string to decide whether a merge request may be
// merged, so an unrecognised fifth word is not quietly passed through.
var reviewSeverities = map[string]bool{"blocker": true, "major": true, "minor": true, "nit": true}

// The two verdicts. `request_changes` and not `reject`, because the word goes
// into a GitLab call and that is GitLab's word for it.
const (
	reviewApprove = "approve"
	reviewChanges = "request_changes"
)

// reviewComment is one comment, as the wrapper posts it: a path the diff
// contains, a line one of its hunks covers, a severity it can switch on, and the
// text.
type reviewComment struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
}

// reviewReport is the `review` field of the result object.
//
// Comments has no omitempty for the reason CheckExit is a pointer (oneshot.go):
// a wrapper that iterates `result["review"]["comments"]` must not meet a null on
// the one review that found nothing, so an empty review carries `[]`.
type reviewReport struct {
	Verdict  string          `json:"verdict"`
	Comments []reviewComment `json:"comments"`
	Summary  string          `json:"summary"`
}

// reviewDiffMax is how much of the diff is put in front of the model. A
// reviewer's whole input IS the diff, so it is larger than the delegation
// reviewer's maxDiffBytes — and it is still a ceiling, because a 4 MB
// generated-file diff would spend the run's whole token budget on one request
// and then overflow the window. Clipping is by whole FILE sections and never by
// bytes in the middle: half a hunk is a diff the model will read wrong, and the
// files that did not fit are named so it knows what it has not seen.
const reviewDiffMax = 120_000

// reviewRun is what -diff-base produced: the diff the reviewer is judging, and
// the line map every comment it writes is checked against. It rides on the
// orchestrator like the budget and -summary do, because it is a property of the
// RUN and not of the task.
type reviewRun struct {
	base    string // what the operator named: "origin/main"
	from    string // the commit it was diffed from, after the merge base with HEAD
	diff    string // the unified diff, as git printed it
	lines   *changedLines
	retried bool
}

// prepareReview resolves -diff-base and takes the diff, before the gateway is
// touched: a base that does not exist is a mistake in the CALL, and finding that
// out after a model has read the tree costs a run for nothing.
//
// It works inside a linked worktree, which is where the pipeline always runs it.
// `rev-parse --show-toplevel` answers with the worktree's own root, and a
// worktree shares refs/ with the main checkout, so `origin/main` resolves there
// without a fetch of its own.
func prepareReview(cfg Config, base string) (*reviewRun, error) {
	root := cfg.Root
	top, err := gitCmd(root, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(top) == "" {
		return nil, usageErrf("-diff-base %s: %s is not inside a git repository, so there is no diff to review", base, root)
	}
	top = strings.TrimSpace(top)
	// ^{commit} and not a bare name: `-diff-base v1.2` naming an annotated tag
	// resolves to a tag object, which `git diff` then refuses. Peeling it here
	// makes one error message instead of two.
	rev, err := gitCmd(top, nil, nil, "rev-parse", "--verify", "--quiet", base+"^{commit}")
	if err != nil || strings.TrimSpace(rev) == "" {
		return nil, usageErrf("-diff-base %s: no such commit in %s — a worktree shares its refs with the main checkout, so `git -C %s fetch origin` is usually what is missing",
			base, top, top)
	}
	from := strings.TrimSpace(rev)
	// The MERGE BASE, which is the diff a merge request shows. Diffing the base
	// ref itself puts everything that landed on it since the branch was cut into
	// the review: the reviewer then comments on lines this ticket never touched,
	// every one of those comments is correctly placed, and the wrapper posts them
	// on somebody else's work. Unrelated histories have no merge base, and then
	// the base itself is the only honest answer.
	if mb, err := gitCmd(top, nil, nil, "merge-base", base, "HEAD"); err == nil && strings.TrimSpace(mb) != "" {
		from = strings.TrimSpace(mb)
	}
	// The diff is taken against a SNAPSHOT of the working tree and not against
	// HEAD, because at this point in the pipeline the coder's work is usually
	// still uncommitted — the wrapper commits after the review — and a new file
	// it added is untracked, which `git diff` does not show at all. snapshotOf
	// writes a tree through a temporary index: the user's index, HEAD and
	// uncommitted work are untouched, which is the property the whole pipeline
	// rests on (branch.go).
	snap, err := snapshotOf(top, cfg.stateDir(), "lca review: the tree as the reviewer found it", "HEAD", "")
	if err != nil {
		return nil, usageErrf("-diff-base %s: could not read the working tree of %s: %v", base, top, err)
	}
	// The prefixes are passed explicitly so that a project with diff.noprefix or
	// diff.srcPrefix set in its config still produces the `a/` and `b/` this
	// file's parser strips; core.quotePath=false keeps a Cyrillic path readable
	// instead of octal-escaped; --no-ext-diff keeps a configured external diff
	// driver from replacing the unified format with something that has no hunks
	// in it at all.
	diff, err := gitCmd(top, nil, nil, "-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff",
		"--src-prefix=a/", "--dst-prefix=b/", "--unified=3", "--find-renames", from, snap)
	if err != nil {
		return nil, usageErrf("-diff-base %s: %v", base, err)
	}
	if strings.TrimSpace(diff) == "" {
		// Not an empty review: a reviewer asked to judge nothing produces a
		// meaningless approve, and an approve nobody meant is the thing this whole
		// file exists to prevent. The wrapper should not have reached this step, so
		// it is row 2 of the table — alert somebody, leave the ticket alone.
		return nil, usageErrf("-diff-base %s: nothing in %s differs from it, so there is no diff to review", base, top)
	}
	rr := &reviewRun{base: base, from: from, diff: diff, lines: parseUnifiedDiff(diff)}
	if len(rr.lines.order) == 0 {
		return nil, usageErrf("-diff-base %s: the diff against it has no line changes in it (only modes, renames or binary files), so there is nothing to comment on", base)
	}
	return rr, nil
}

// ── the diff's line map ─────────────────────────────────────────────────────

// lineSpan is one hunk's extent on the NEW side, inclusive.
type lineSpan struct{ from, to int }

// changedLines is which lines of which files a comment may be placed on.
//
// A hunk's whole new-side range, and not only its `+` lines. That is what
// GitLab accepts as a position (a comment on a context line inside the diff is
// postable, one outside it is not), and it is what a reviewer means: a defect
// introduced by an added line is often best pointed at three lines above it, on
// the `if` the new line now sits under. The requirement is written the same way
// — "a line that is not inside a changed hunk".
type changedLines struct {
	files map[string][]lineSpan
	order []string // the paths in the order the diff lists them, for messages
}

// parseUnifiedDiff reads git's own output. It is deliberately tolerant: a line
// it does not recognise is skipped rather than failing the run, because the only
// thing this map decides is whether a comment can be placed, and the worst a
// missed hunk does is move one comment into the summary.
func parseUnifiedDiff(diff string) *changedLines {
	cl := &changedLines{files: map[string][]lineSpan{}}
	path := ""
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path = "" // a new file section; its +++ line decides the name
		case strings.HasPrefix(line, "+++ "):
			path = newSidePath(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "@@"):
			if path == "" {
				continue // a deleted file (+++ /dev/null) has no new-side lines
			}
			from, count, ok := newSideRange(line)
			if !ok || count <= 0 {
				continue // a pure-deletion hunk adds nothing to comment on
			}
			if _, seen := cl.files[path]; !seen {
				cl.order = append(cl.order, path)
			}
			cl.files[path] = append(cl.files[path], lineSpan{from, from + count - 1})
		}
	}
	return cl
}

// newSidePath is the path out of a `+++ b/path` line. git quotes a path with a
// quote or a newline in it C-style, and strconv.Unquote reads exactly that
// spelling.
func newSidePath(s string) string {
	s = strings.TrimSpace(s)
	// git appends a tab and a timestamp in some configurations.
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			s = u
		}
	}
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, "b/")
}

// newSideRange reads the `+c,d` half of an @@ header. A hunk of one line omits
// the count, which is the shape this used to get wrong.
func newSideRange(hdr string) (from, count int, ok bool) {
	i := strings.IndexByte(hdr, '+')
	if i < 0 {
		return 0, 0, false
	}
	rest := hdr[i+1:]
	if j := strings.IndexAny(rest, " @"); j >= 0 {
		rest = rest[:j]
	}
	start, n, found := strings.Cut(rest, ",")
	from, err := strconv.Atoi(start)
	if err != nil || from < 0 {
		return 0, 0, false
	}
	count = 1
	if found {
		count, err = strconv.Atoi(n)
		if err != nil {
			return 0, 0, false
		}
	}
	return from, count, true
}

// place answers whether a comment can be posted where it says, and on what path
// the diff spells it. The path is normalised rather than demanded verbatim: a
// model handed `b/src/gw.rs` in the diff writes `src/gw.rs` about as often as
// not, and an absolute path is what it gets from its own file tools. A suffix is
// accepted only when exactly ONE file in the diff ends with it on a component
// boundary — an ambiguous `mod.rs` is not placed, because guessing which of four
// would post the comment on the wrong file.
func (cl *changedLines) place(file string, line int) (string, bool) {
	p := normalizeDiffPath(file)
	if cl == nil || p == "" || line <= 0 {
		return "", false
	}
	spans, ok := cl.files[p]
	if !ok {
		match := ""
		for _, cand := range cl.order {
			if strings.HasSuffix(cand, "/"+p) {
				if match != "" {
					return "", false
				}
				match = cand
			}
		}
		if match == "" {
			return "", false
		}
		p, spans = match, cl.files[match]
	}
	for _, s := range spans {
		if line >= s.from && line <= s.to {
			return p, true
		}
	}
	return p, false
}

// normalizeDiffPath puts a path the model wrote into the diff's own spelling as
// far as it can without guessing: slashes, no `./`, no `a/` or `b/` prefix, and
// a leading `/` dropped so an absolute path still has a chance of matching by
// suffix.
func normalizeDiffPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	return strings.TrimPrefix(p, "/")
}

// hunkWords describes a file's hunks for the retry message. Being told "lines
// 4880-4895 and 4931-4940" is what lets a model move a comment instead of
// repeating it.
func (cl *changedLines) hunkWords(path string) string {
	spans := cl.files[path]
	if len(spans) == 0 {
		return ""
	}
	var parts []string
	for _, s := range spans {
		if s.from == s.to {
			parts = append(parts, strconv.Itoa(s.from))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d-%d", s.from, s.to))
	}
	return strings.Join(parts, ", ")
}

// ── reading the reviewer's answer ───────────────────────────────────────────

// read turns the reviewer's final reply into the object, or says why it could
// not. The error is reserved for "there is no readable verdict here at all",
// which is the only outcome that becomes `status: failed`; everything else is a
// problem the one retry is spent on and, after it, salvaged into the summary.
func (rr *reviewRun) read(text string) (*reviewReport, []string, error) {
	raw, err := extractJSONObject(text)
	if err != nil {
		return nil, nil, err
	}
	var rep reviewReport
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return nil, nil, fmt.Errorf("the object the reviewer sent is not the review object: %v", err)
	}
	verdict, ok := normalizeVerdict(rep.Verdict)
	if !ok {
		return nil, nil, fmt.Errorf("the reviewer's verdict was %q, which is neither %q nor %q", truncate(rep.Verdict, 40), reviewApprove, reviewChanges)
	}
	rep.Verdict = verdict
	out, problems := rr.validate(&rep)
	return out, problems, nil
}

// normalizeVerdict accepts the inflections a model writes, for the reason
// delegate.go's parseVerdict accepts them: not accepting them fails in the
// UNSAFE direction. "rejected" read as no verdict at all would be an unreadable
// review, and an unreadable review is a failure — but "changes_requested" read
// as nothing would have been a reviewer blocking a change that nobody then
// heard, so every spelling that plainly means "do not merge this" is one.
func normalizeVerdict(v string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(v))
	s = strings.NewReplacer(" ", "_", "-", "_").Replace(s)
	switch {
	case s == "approve", s == "approved", s == "approves":
		return reviewApprove, true
	case strings.HasPrefix(s, "request_change"), strings.HasPrefix(s, "changes_request"),
		strings.HasPrefix(s, "reject"):
		return reviewChanges, true
	}
	return "", false
}

// validate splits the comments into the ones a wrapper can post and the ones it
// cannot, and says what was wrong with each of the second kind.
//
// Nothing is dropped. An unplaceable comment is appended to `summary` with the
// file and line the reviewer wrote, because the alternative is a found defect
// that nobody is ever told about — and a wrong line number is a clerical
// mistake, not a reason to discard a finding.
func (rr *reviewRun) validate(rep *reviewReport) (*reviewReport, []string) {
	out := &reviewReport{Verdict: rep.Verdict, Comments: []reviewComment{}, Summary: strings.TrimSpace(rep.Summary)}
	var problems, salvaged []string
	for _, c := range rep.Comments {
		body := strings.TrimSpace(forPublication(c.Body))
		sev := strings.ToLower(strings.TrimSpace(c.Severity))
		where := strings.TrimSpace(c.File)
		if where == "" {
			where = "(no file)"
		}
		if c.Line > 0 {
			where = fmt.Sprintf("%s:%d", where, c.Line)
		}
		switch {
		case body == "":
			// A comment with nothing in it cannot be posted and cannot be moved into
			// the summary either — there is no text to move. It is still REPORTED,
			// because "the reviewer emitted an empty comment" is a fact about the
			// review that the operator reading the summary should have.
			problems = append(problems, fmt.Sprintf("the comment on %s has an empty `body`; a comment with no text cannot be posted", where))
			salvaged = append(salvaged, fmt.Sprintf("%s — the reviewer left this comment with no text", where))
			continue
		case !reviewSeverities[sev]:
			problems = append(problems, fmt.Sprintf("the comment on %s has severity %q; it must be one of blocker, major, minor, nit", where, truncate(c.Severity, 20)))
			salvaged = append(salvaged, fmt.Sprintf("%s — %s", where, body))
			continue
		case c.Line <= 0:
			problems = append(problems, fmt.Sprintf("the comment on %s has line %d; a line number starts at 1", where, c.Line))
			salvaged = append(salvaged, fmt.Sprintf("%s (%s) — %s", where, sev, body))
			continue
		}
		path, ok := rr.lines.place(c.File, c.Line)
		if !ok {
			if _, known := rr.lines.files[path]; known && path != "" {
				problems = append(problems, fmt.Sprintf("line %d of %s is not inside any changed hunk; the diff's hunks there cover lines %s",
					c.Line, path, rr.lines.hunkWords(path)))
			} else {
				// The file list is clipped: a 300-file diff would otherwise put its
				// whole manifest in a message whose job is to be read and acted on.
				problems = append(problems, fmt.Sprintf("%s is not in the diff at all; the diff touches %s",
					where, truncate(strings.Join(rr.lines.order, ", "), 600)))
			}
			salvaged = append(salvaged, fmt.Sprintf("%s (%s) — %s", where, sev, body))
			continue
		}
		out.Comments = append(out.Comments, reviewComment{File: path, Line: c.Line, Severity: sev, Body: body})
	}
	// Sorted, so two runs of the same review produce the same object and a
	// wrapper diffing two reviews is diffing the reviews and not the order the
	// model happened to list them in.
	sort.SliceStable(out.Comments, func(i, j int) bool {
		if out.Comments[i].File != out.Comments[j].File {
			return out.Comments[i].File < out.Comments[j].File
		}
		return out.Comments[i].Line < out.Comments[j].Line
	})
	if len(salvaged) > 0 {
		out.Summary = strings.TrimSpace(out.Summary + "\n\nComments that could not be placed in the diff:\n- " + strings.Join(salvaged, "\n- "))
	}
	// A block with nothing in it anywhere is not a review. It is the one shape
	// that would otherwise reach the wrapper as a verdict with no evidence under
	// it, and for `request_changes` it is also unactionable: the merge request
	// would be blocked by a reviewer that named nothing.
	if out.Verdict == reviewChanges && len(out.Comments) == 0 && out.Summary == "" {
		problems = append(problems, "the verdict is request_changes but there is no comment and no summary; name the defect")
	}
	out.Summary = forPublication(out.Summary)
	return out, problems
}

// extractJSONObject finds the object in a reply that may also contain prose or a
// code fence around it. Models put the object in ```json … ``` about as often as
// they return it bare, and told not to they still sometimes open with "Here is
// the review:".
//
// It matches braces from each `{` to its close, skipping over string literals
// so that a `{` inside a comment body does not end the object early. The LAST
// complete object that parses wins: a reviewer that quotes the requested shape
// first and answers after must be read by its answer, exactly as parseVerdict
// takes the last verdict line.
func extractJSONObject(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("the reviewer replied with nothing at all")
	}
	best, spent := "", 0
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		// A brace that never closes costs a scan to the end of the reply, and prose
		// can hold any number of them ("use a map{"), so the total is bounded
		// rather than left quadratic in the length of a reply a model controls. A
		// reply large enough to exhaust this budget is not a review object.
		if spent > jsonScanBudget {
			break
		}
		raw, end := matchObject(text, i)
		spent += end - i + 1
		if raw != "" {
			if json.Valid([]byte(raw)) {
				best = raw
			}
			i = end // never rescan inside an object we already matched
		}
	}
	if best == "" {
		return "", fmt.Errorf("the reviewer's reply has no JSON object in it")
	}
	return best, nil
}

// jsonScanBudget is how many bytes extractJSONObject may examine in total.
const jsonScanBudget = 4 << 20

// matchObject returns the substring from start to its matching brace, and where
// that was, or "" and the end of the text when the object never closes — the
// caller charges the distance to its budget either way.
func matchObject(s string, start int) (string, int) {
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], i
			}
		}
	}
	return "", len(s)
}

// ── the request, and the one retry ──────────────────────────────────────────

// taskMessage is the reviewer's user message: the operator's own review prompt,
// the diff, and the contract for the reply. All of it below the system prompt,
// so the request prefix the gateway caches is byte-identical to every other run
// of this role.
func (rr *reviewRun) taskMessage(prompt string) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "You are reviewing the change in this working tree against `%s`", rr.base)
	if rr.from != rr.base {
		fmt.Fprintf(&b, " (merge base %s)", shortRev(rr.from))
	}
	b.WriteString(". The diff below includes work that is not committed yet.\n\n<diff>\n")
	b.WriteString(clipDiffByFile(rr.diff, reviewDiffMax))
	b.WriteString("\n</diff>\n\n")
	b.WriteString(`You are inside the tree the diff was taken from: read the files it touches
and run whatever convinces you. Judge correctness, not style — a change that
passes its tests and is still wrong is what this review exists to catch.

Your LAST message must be one JSON object and nothing else — no prose before it,
no prose after it:

{"verdict": "approve" | "request_changes",
 "comments": [{"file": "src/bin/gw.rs", "line": 4891, "severity": "blocker" | "major" | "minor" | "nit",
               "body": "what is wrong here, and what to do about it"}],
 "summary": "two or three sentences: what the change does, and what you decided"}

How the object is read, so that nothing you write is wasted:
- "line" is a line number on the NEW side of the diff — the numbering of the
  "+++" file — and it must fall inside one of that file's hunks above. A line
  outside every hunk cannot be posted on a merge request, so it is moved into
  "summary" instead of being shown where you put it.
- "file" is the path as the diff spells it, without the "b/" prefix.
- A finding that is not about one line — the shape of the change, a missing
  test, something you could not check — belongs in "summary". That is where it
  is read, not a line it does not really concern.
- "comments" may be empty. "approve" with an empty list is a complete answer.
- Ask for changes only for a defect you can name: "request_changes" blocks the
  merge request.`)
	return b.String()
}

// clipDiffByFile keeps as many whole file sections as fit and names the rest.
// Cutting a diff by bytes leaves half a hunk, and half a hunk is a diff a model
// reads as a complete one — it then comments on lines that are not where it
// thinks they are. Naming what was left out is the honest alternative: the
// reviewer can say in its summary that it did not see those files.
func clipDiffByFile(diff string, max int) string {
	if len(diff) <= max {
		return diff
	}
	var kept []string
	var left []string
	n := 0
	for _, sec := range splitDiffFiles(diff) {
		if n+len(sec.text) <= max && len(left) == 0 {
			kept = append(kept, sec.text)
			n += len(sec.text)
			continue
		}
		left = append(left, sec.path)
	}
	out := strings.Join(kept, "")
	if len(left) > 0 {
		out += fmt.Sprintf("\n[the diff is larger than %s: %s not shown — say so in your summary rather than guessing at them]\n",
			byteCount(max), strings.Join(left, ", "))
	}
	return out
}

type diffSection struct {
	path string
	text string
}

// splitDiffFiles cuts git's output at each `diff --git` line. Anything before
// the first one (there is normally nothing) is kept as its own leading section so
// no byte is silently lost.
func splitDiffFiles(diff string) []diffSection {
	lines := strings.Split(diff, "\n")
	var out []diffSection
	cur := diffSection{path: "(header)"}
	flush := func() {
		if cur.text != "" {
			out = append(out, cur)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "diff --git ") {
			flush()
			cur = diffSection{path: diffGitPath(l)}
		}
		cur.text += l + "\n"
	}
	flush()
	return out
}

// diffGitPath is the new path out of a `diff --git a/x b/y` line, for the "not
// shown" list only.
func diffGitPath(l string) string {
	rest := strings.TrimPrefix(l, "diff --git ")
	if i := strings.Index(rest, " b/"); i >= 0 {
		return newSidePath(rest[i+1:])
	}
	return strings.TrimSpace(rest)
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// retryMessage is the one re-ask. It names every problem with the object that
// came back, because "reply with valid JSON" to a model that already believes it
// did produces the same reply again — and it says what happens if it does, so a
// reviewer that stands by a comment knows to move it into the summary itself
// rather than drop it.
func (rr *reviewRun) retryMessage(err error, problems []string) string {
	var b strings.Builder
	b.WriteString("[review] Your review could not be used as it stands.\n\n")
	if err != nil {
		fmt.Fprintf(&b, "- %s\n", err.Error())
	}
	for _, p := range problems {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	b.WriteString(`
Send the WHOLE object again, corrected, as your entire reply: no prose, no code
fence. Keep every finding — move one you cannot place on a line in the diff into
"summary" yourself, in your own words. This is the last time you are asked: an
object that still cannot be read is reported as a failed review, not as an
approve.`)
	return b.String()
}

// finishReview is the whole of -diff-base after the run: read the final reply,
// spend the one retry if anything was wrong with it, and put the result on the
// verdict for statusOf to judge.
//
// The retry is a message on the SAME session, which is the cheapest request in
// the run: the prefix is cached and so is the diff above it, so what is paid for
// is the re-ask and the new reply.
func (o *Orchestrator) finishReview(ctx context.Context, s *Session, v *Verdict) {
	rr := o.review
	if rr == nil {
		return
	}
	rep, problems, err := rr.read(lastAssistantText(s))
	if (err != nil || len(problems) > 0) && ctx.Err() == nil {
		s.event("review_retry", map[string]any{"problems": problems, "err": errText(err)})
		rr.retried = true
		s.Msgs = append(s.Msgs, Message{Role: "user", Content: rr.retryMessage(err, problems)})
		rerr := s.Run(ctx)
		switch {
		case rerr != nil && err != nil:
			// The first answer was unreadable AND the re-ask never arrived. That is
			// not a reviewer that failed to answer, it is the gateway going away
			// mid-run, and calling it `failed` would send a ticket to a person over
			// a dropped connection. Handing the error to the verdict puts it back
			// through classifyRunErr, where infra_error is decided.
			v.Status, v.Err, v.Tail = "error", rerr, rerr.Error()
			s.event("review_retry_failed", map[string]any{"err": rerr.Error()})
			return
		case rerr != nil:
			// The re-ask failed but the first answer was readable: it stands. A
			// review that was received and then thrown away by a lost connection
			// would be the worst of both.
		default:
			// Second read wins when it is readable, and the FIRST stands when it is
			// not: a reviewer that answered once and then lost the thread has still
			// told us what it found, and throwing that away to report "unreadable"
			// would be the one outcome nobody wants.
			if rep2, problems2, err2 := rr.read(lastAssistantText(s)); err2 == nil {
				rep, problems, err = rep2, problems2, nil
			} else if err != nil {
				err = err2
			}
		}
	}
	if err != nil || rep == nil {
		v.ReviewErr = errText(err)
		if v.ReviewErr == "" {
			v.ReviewErr = "the reviewer produced no object"
		}
		s.event("review_unreadable", map[string]any{"err": v.ReviewErr, "retried": rr.retried})
		return
	}
	v.Review = rep
	s.event("review", map[string]any{"verdict": rep.Verdict, "comments": len(rep.Comments),
		"salvaged": len(problems), "retried": rr.retried, "base": rr.base})
}

// reviewOutcomeOf is the review as the TRACE records it, so that `lca eval` can
// score a reviewer run the way it already scores a delegation's review: same
// record, same three fields. nil for every run that asked for no review, which
// is what keeps the record byte-identical for them.
func (o *Orchestrator) reviewOutcomeOf(s *Session, v Verdict) *reviewOutcome {
	if o.review == nil {
		return nil
	}
	out := &reviewOutcome{Role: s.agent.Name, Model: s.client.Model(), Verdict: "unreviewed", Reasons: v.ReviewErr}
	if v.Review != nil {
		out.Verdict, out.Reasons = v.Review.Verdict, truncate(v.Review.Summary, reviewReasonBytes)
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// reviewLines renders the verdict for a person watching, because a run without
// -json has nowhere else to put it: the object itself went past as the model's
// last reply, which is not a thing anybody reads.
func reviewLines(rep *reviewReport, why string) []string {
	if rep == nil {
		return []string{"review: no verdict could be read — " + why}
	}
	out := []string{fmt.Sprintf("review: %s, %s", rep.Verdict, plural(len(rep.Comments), "comment", "comments"))}
	for _, c := range rep.Comments {
		out = append(out, fmt.Sprintf("  %s:%d  %s  %s", c.File, c.Line, c.Severity, firstLine(c.Body)))
	}
	return out
}
