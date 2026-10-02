package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P1-1 and P1-2: the reviewer with a structured verdict, and the next round on
// the same session.
//
// The acceptance criteria these are written against, from the requirements
// document:
//
//   - P1-1: "on a diff with a planted bug the reviewer returns request_changes
//     and a comment on the right file, and every `line` falls inside the changed
//     lines" — TestReviewAcceptance, with the line checked against the FILE
//     rather than against the parser that placed it.
//   - P1-2: "the second round uses the first's history (visible in the
//     transcript) and the rounds share an x-root-session-id" —
//     TestSessionContinuesTheRound.

// gwBase is twenty lines with nothing wrong with them. Line 10 is `g()`, which
// is where every test below plants its bug: a known line number is what lets the
// placement be checked without asking the code under test where the change was.
const gwBase = `package gw

func serve(x int) {
	a()
	b()
	c()
	d()
	e()
	f()
	g()
	h()
	i()
	j()
	k()
	l()
	m()
	n()
	o()
	p()
}
`

// gwBugged is gwBase with an assignment where a comparison belongs, on line 10
// and on no other line: the hunk around it covers 7..13, so a comment on 1 or on
// 19 is outside it.
var gwBugged = strings.Replace(gwBase, "\tg()\n", "\tif x = 1 { panic(\"bug\") }\n", 1)

const plantedBug = "if x = 1"

// reviewRepo is the tree the pipeline hands the reviewer: a base commit, the
// coder's change on top of it, and — the case `git diff` alone does not show at
// all — an untracked file the coder added. The wrapper commits AFTER the review,
// so uncommitted work in the diff is the normal shape and not an edge case.
func reviewRepo(t *testing.T, root string) {
	t.Helper()
	gitT(t, root, "init", "-q")
	gitT(t, root, "config", "user.email", "t@t")
	gitT(t, root, "config", "user.name", "t")
	gitT(t, root, "config", "commit.gpgsign", "false")
	writeAll(t, root, map[string]string{"src/gw.go": gwBase})
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-q", "-m", "base")
	gitT(t, root, "branch", "-f", "pipeline-base")
	writeAll(t, root, map[string]string{"src/gw.go": gwBugged, "notes.md": "scratch\n"})
}

// reviewHarness is a harness whose root is that tree, with -diff-base already
// resolved against it.
func reviewHarness(t *testing.T, gw string) (*harness, *reviewRun) {
	t.Helper()
	h := newHarness(t, gw, "native", true)
	reviewRepo(t, h.root)
	rr, err := prepareReview(Config{Root: h.root, Dir: t.TempDir()}, "pipeline-base")
	if err != nil {
		t.Fatalf("prepareReview: %v", err)
	}
	h.orch.review = rr
	return h, rr
}

func reviewJSON(verdict string, comments ...reviewComment) string {
	b, _ := json.Marshal(reviewReport{Verdict: verdict, Comments: comments, Summary: "the change rewrites one condition"})
	return string(b)
}

// The criterion itself: a planted bug comes back as request_changes with a
// comment on the right file, and the line is one of the changed lines.
func TestReviewAcceptance(t *testing.T) {
	reply := reviewJSON(reviewChanges, reviewComment{File: "src/gw.go", Line: 10, Severity: "blocker",
		Body: "`if x = 1` assigns where it should compare, so the branch is always taken"})
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: reply} })
	h, rr := reviewHarness(t, fs.URL)

	// The diff really is the diff of the whole tree: the committed base, the
	// uncommitted edit and the untracked file.
	if !strings.Contains(rr.diff, plantedBug) {
		t.Fatalf("the diff does not contain the change:\n%s", rr.diff)
	}
	if !strings.Contains(rr.diff, "notes.md") {
		t.Fatalf("an untracked file the coder added is missing from the diff:\n%s", rr.diff)
	}

	r, code, _ := oneShotResult(t, h, "review the change", "")
	if r.Status != statusPassed || code != exitOK {
		t.Fatalf("a review that was produced is the run's success: status %s, exit %d, reason %q", r.Status, code, r.Reason)
	}
	if r.Review == nil {
		t.Fatal("the result object carries no review")
	}
	if r.Review.Verdict != reviewChanges {
		t.Fatalf("verdict %q, want %q", r.Review.Verdict, reviewChanges)
	}
	if len(r.Review.Comments) != 1 {
		t.Fatalf("want one comment, got %+v", r.Review.Comments)
	}
	c := r.Review.Comments[0]
	if c.File != "src/gw.go" || c.Severity != "blocker" || c.Body == "" {
		t.Fatalf("the comment is not on the right file: %+v", c)
	}
	// "every line falls inside the changed lines", checked against the FILE and
	// not against the parser that accepted it: line N of the file as it stands has
	// to be a line this change wrote.
	body, err := os.ReadFile(filepath.Join(h.root, c.File))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")
	if c.Line < 1 || c.Line > len(lines) {
		t.Fatalf("line %d is not a line of %s (%d lines)", c.Line, c.File, len(lines))
	}
	if !strings.Contains(lines[c.Line-1], plantedBug) {
		t.Fatalf("line %d of %s is %q — not the line the change touched", c.Line, c.File, lines[c.Line-1])
	}
	if n := len(fs.reqs()); n != 1 {
		t.Fatalf("a readable review cost %d requests; it must cost one", n)
	}
}

// The constraint the whole design is built around: asking for the object must
// not move the request prefix, because the gateway keys its KV cache on it. The
// instruction and the diff ride in the user message, so the system prompt and
// the tool schemas of a review run are the ones every other run of that role
// sends.
func TestReviewKeepsThePrefixIdentical(t *testing.T) {
	reply := reviewJSON(reviewApprove)
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: reply} })
	h, rr := reviewHarness(t, fs.URL)
	h.orch.review = nil
	if err := h.run(t, "an ordinary task"); err != nil {
		t.Fatal(err)
	}
	h.orch.review = rr
	oneShotResult(t, h, "review the change", "")
	rs := fs.reqs()
	if len(rs) < 2 {
		t.Fatalf("want two requests, got %d", len(rs))
	}
	plain, review := rs[0], rs[len(rs)-1]
	if plain.system() != review.system() {
		t.Fatalf("the review run's system prompt differs from the ordinary one — every cached prefix for this role is lost:\n%q\n%q",
			plain.system(), review.system())
	}
	if len(plain.Tools) != len(review.Tools) {
		t.Fatalf("the review run offers %d tools and an ordinary one %d: a schema that reaches a role which does not need it is a cache miss for every one of them",
			len(review.Tools), len(plain.Tools))
	}
	for i := range plain.Tools {
		if plain.Tools[i].Function.Name != review.Tools[i].Function.Name {
			t.Fatalf("tool %d is %q on an ordinary run and %q on a review", i, plain.Tools[i].Function.Name, review.Tools[i].Function.Name)
		}
	}
	// And the diff did reach the model — in the user message, where it costs
	// nothing above it.
	last := review.Messages[len(review.Messages)-1]
	if txt, _ := last["content"].(string); !strings.Contains(txt, plantedBug) {
		t.Fatalf("the diff is not in the task message: %q", truncate(fmt.Sprint(last["content"]), 200))
	}
}

// The line map, against a diff written by hand so that the expected answers are
// arithmetic and not whatever git produced today.
const handDiff = `diff --git a/src/gw.go b/src/gw.go
index 1111111..2222222 100644
--- a/src/gw.go
+++ b/src/gw.go
@@ -7,7 +7,7 @@ func serve() {
 	d()
 	e()
 	f()
-	g()
+	if x = 1 { panic("bug") }
 	h()
 	i()
 	j()
@@ -40,2 +40,3 @@ func stop() {
 	y()
+	z()
 	w()
diff --git a/new.go b/new.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package main
+// new
diff --git a/gone.go b/gone.go
deleted file mode 100644
index 4444444..0000000
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package main
-// gone
`

func TestParseUnifiedDiff(t *testing.T) {
	cl := parseUnifiedDiff(handDiff)
	want := map[string][]lineSpan{
		"src/gw.go": {{7, 13}, {40, 42}},
		"new.go":    {{1, 2}},
	}
	for path, spans := range want {
		got := cl.files[path]
		if len(got) != len(spans) {
			t.Fatalf("%s: %v, want %v", path, got, spans)
		}
		for i := range spans {
			if got[i] != spans[i] {
				t.Errorf("%s hunk %d: %v, want %v", path, i, got[i], spans[i])
			}
		}
	}
	// A deleted file has no new side, so there is no line on it to comment on.
	if _, ok := cl.files["gone.go"]; ok {
		t.Errorf("a deleted file got line spans: %v", cl.files["gone.go"])
	}
	if len(cl.order) != 2 {
		t.Errorf("the diff touches %v", cl.order)
	}
}

func TestPlaceAgainstTheDiff(t *testing.T) {
	cl := parseUnifiedDiff(handDiff)
	for _, tc := range []struct {
		name string
		file string
		line int
		want bool
	}{
		{"the changed line", "src/gw.go", 10, true},
		{"a context line inside the hunk", "src/gw.go", 7, true},
		{"one line above the hunk", "src/gw.go", 6, false},
		{"one line below the hunk", "src/gw.go", 14, false},
		{"the second hunk", "src/gw.go", 41, true},
		{"between the hunks", "src/gw.go", 25, false},
		{"the diff's own prefix", "b/src/gw.go", 10, true},
		{"a path the model shortened", "gw.go", 10, true},
		{"an absolute path", "/src/gw.go", 10, true},
		{"a file not in the diff", "src/other.go", 10, false},
		{"a new file", "new.go", 1, true},
		{"a deleted file", "gone.go", 1, false},
		{"line zero", "src/gw.go", 0, false},
		{"a negative line", "src/gw.go", -3, false},
		{"no file at all", "", 10, false},
	} {
		if _, ok := cl.place(tc.file, tc.line); ok != tc.want {
			t.Errorf("%s (%s:%d) → %v, want %v", tc.name, tc.file, tc.line, ok, tc.want)
		}
	}
}

// Every way a comment can be unusable, and what happens to it: it is REPORTED,
// so the one retry can name it, and it is moved into the summary, so a reviewer
// that found something is never silenced by its own arithmetic.
func TestValidateMovesWhatItCannotPlace(t *testing.T) {
	rr := &reviewRun{base: "origin/main", diff: handDiff, lines: parseUnifiedDiff(handDiff)}
	for _, tc := range []struct {
		name    string
		comment reviewComment
		inBody  string // what the summary must then carry
	}{
		{"outside every hunk", reviewComment{File: "src/gw.go", Line: 1, Severity: "major", Body: "shadowed import"}, "shadowed import"},
		{"a file not in the diff", reviewComment{File: "src/elsewhere.go", Line: 4, Severity: "major", Body: "stale constant"}, "stale constant"},
		{"line zero", reviewComment{File: "src/gw.go", Line: 0, Severity: "minor", Body: "whole file is odd"}, "whole file is odd"},
		{"a negative line", reviewComment{File: "src/gw.go", Line: -2, Severity: "nit", Body: "naming"}, "naming"},
		{"an unknown severity", reviewComment{File: "src/gw.go", Line: 10, Severity: "critical", Body: "assignment in a condition"}, "assignment in a condition"},
		{"no body at all", reviewComment{File: "src/gw.go", Line: 10, Severity: "major", Body: "   "}, "src/gw.go:10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, problems := rr.validate(&reviewReport{Verdict: reviewChanges, Comments: []reviewComment{tc.comment}, Summary: "a summary"})
			if len(problems) == 0 {
				t.Fatal("nothing was reported, so the one retry has nothing to say")
			}
			if len(out.Comments) != 0 {
				t.Fatalf("an unusable comment survived into the object: %+v", out.Comments)
			}
			if !strings.Contains(out.Summary, tc.inBody) {
				t.Fatalf("the finding was dropped instead of moved into the summary:\n%s", out.Summary)
			}
			if !strings.Contains(out.Summary, "a summary") {
				t.Fatalf("the reviewer's own summary was overwritten:\n%s", out.Summary)
			}
		})
	}
}

// A placeable comment keeps its own line, and a `comments` list is never null:
// a wrapper iterating it must not meet one on the review that found nothing.
func TestValidateKeepsWhatItCanPlace(t *testing.T) {
	rr := &reviewRun{lines: parseUnifiedDiff(handDiff)}
	out, problems := rr.validate(&reviewReport{Verdict: reviewChanges, Summary: "fine",
		Comments: []reviewComment{
			{File: "new.go", Line: 2, Severity: "NIT", Body: "stray comment"},
			{File: "b/src/gw.go", Line: 10, Severity: "blocker", Body: "assignment"},
		}})
	if len(problems) != 0 {
		t.Fatalf("a usable review was reported as a problem: %v", problems)
	}
	if len(out.Comments) != 2 {
		t.Fatalf("%+v", out.Comments)
	}
	// Sorted by file and line, so two runs of the same review produce the same
	// object.
	if out.Comments[0].File != "new.go" || out.Comments[1].File != "src/gw.go" {
		t.Fatalf("not in a stable order: %+v", out.Comments)
	}
	if out.Comments[0].Severity != "nit" {
		t.Fatalf("severity was not normalised: %q", out.Comments[0].Severity)
	}
	empty, _ := rr.validate(&reviewReport{Verdict: reviewApprove})
	b, _ := json.Marshal(empty)
	if !strings.Contains(string(b), `"comments":[]`) {
		t.Fatalf("an empty review must carry [] and not null: %s", b)
	}
}

// request_changes with nothing under it is unactionable: the merge request would
// be blocked by a reviewer that named no defect.
func TestValidateRefusesABlankRejection(t *testing.T) {
	rr := &reviewRun{lines: parseUnifiedDiff(handDiff)}
	if _, problems := rr.validate(&reviewReport{Verdict: reviewChanges}); len(problems) == 0 {
		t.Fatal("request_changes with no comment and no summary was accepted")
	}
	if _, problems := rr.validate(&reviewReport{Verdict: reviewApprove}); len(problems) != 0 {
		t.Fatalf("approve with no comments is a complete answer: %v", problems)
	}
}

func TestNormalizeVerdict(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"approve", reviewApprove, true},
		{"APPROVED", reviewApprove, true},
		{"request_changes", reviewChanges, true},
		{"request changes", reviewChanges, true},
		{"Request-Changes", reviewChanges, true},
		{"changes_requested", reviewChanges, true},
		// Not accepting the words that plainly mean "do not merge" would fail in
		// the unsafe direction, so they are a request for changes.
		{"reject", reviewChanges, true},
		{"rejected", reviewChanges, true},
		{"", "", false},
		{"maybe", "", false},
		{"looks good", "", false},
	} {
		got, ok := normalizeVerdict(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q → %q,%v; want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestExtractJSONObject(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		fail bool
	}{
		{"bare", `{"verdict":"approve"}`, `{"verdict":"approve"}`, false},
		{"fenced", "Here it is:\n```json\n{\"verdict\":\"approve\"}\n```\n", `{"verdict":"approve"}`, false},
		{"prose after", `{"verdict":"approve"} — done.`, `{"verdict":"approve"}`, false},
		{"a brace inside a body", `{"summary":"use a map{} here"}`, `{"summary":"use a map{} here"}`, false},
		{"nested", `{"a":{"b":1}}`, `{"a":{"b":1}}`, false},
		// A reviewer that quotes the shape it was asked for and answers after must
		// be read by its answer.
		{"the last object wins", `the shape is {"verdict":"…"} and mine is {"verdict":"approve"}`, `{"verdict":"approve"}`, false},
		{"nothing", "looks fine to me", "", true},
		{"empty", "   ", "", true},
		{"never closes", `{"verdict":"approve"`, "", true},
	} {
		got, err := extractJSONObject(tc.in)
		if tc.fail {
			if err == nil {
				t.Errorf("%s: wanted an error, got %q", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// One retry, and only one. A reviewer that forgot the object is told exactly
// what was wrong and asked again; a second miss is a failed run.
func TestReviewRetriesOnce(t *testing.T) {
	good := reviewJSON(reviewChanges, reviewComment{File: "src/gw.go", Line: 10, Severity: "major", Body: "assignment in a condition"})
	t.Run("the retry is used", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			if n == 1 {
				return fakeReply{content: "Looks fine to me, ship it."}
			}
			return fakeReply{content: good}
		})
		h, _ := reviewHarness(t, fs.URL)
		r, code, _ := oneShotResult(t, h, "review the change", "")
		if r.Status != statusPassed || code != exitOK || r.Review == nil {
			t.Fatalf("status %s, exit %d, review %+v, reason %q", r.Status, code, r.Review, r.Reason)
		}
		if len(r.Review.Comments) != 1 {
			t.Fatalf("%+v", r.Review)
		}
		rs := fs.reqs()
		if len(rs) != 2 {
			t.Fatalf("want two requests (the review and the one retry), got %d", len(rs))
		}
		txt, _ := rs[1].last()["content"].(string)
		if !strings.Contains(txt, "[review]") || !strings.Contains(txt, "no JSON object") {
			t.Fatalf("the retry did not say what was wrong:\n%s", txt)
		}
	})

	// The one failure mode that would let a bad change through unseen.
	t.Run("a second miss is failed and never approve", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			return fakeReply{content: "Looks fine to me, ship it."}
		})
		h, _ := reviewHarness(t, fs.URL)
		r, code, _ := oneShotResult(t, h, "review the change", "")
		if r.Status != statusFailed || code != exitFailed {
			t.Fatalf("status %s, exit %d, want failed/1", r.Status, code)
		}
		if r.Review != nil {
			t.Fatalf("an unreadable review reached the wrapper as %+v", r.Review)
		}
		if !strings.Contains(r.Reason, "could not be read") {
			t.Fatalf("reason %q", r.Reason)
		}
		if n := len(fs.reqs()); n != 2 {
			t.Fatalf("the reviewer was asked %d times; the contract is one retry", n)
		}
	})

	// Invalid JSON, as opposed to no JSON at all: the same row.
	t.Run("invalid json twice is failed", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			return fakeReply{content: `{"verdict": "approve", "comments": [`}
		})
		h, _ := reviewHarness(t, fs.URL)
		r, code, _ := oneShotResult(t, h, "review the change", "")
		if r.Status != statusFailed || code != exitFailed || r.Review != nil {
			t.Fatalf("status %s, exit %d, review %+v", r.Status, code, r.Review)
		}
	})

	// A verdict word nobody can read is as unreadable as broken JSON — and must
	// not be rounded up to an approve.
	t.Run("an unreadable verdict twice is failed", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			return fakeReply{content: `{"verdict": "mostly fine", "comments": [], "summary": "ok"}`}
		})
		h, _ := reviewHarness(t, fs.URL)
		r, code, _ := oneShotResult(t, h, "review the change", "")
		if r.Status != statusFailed || code != exitFailed || r.Review != nil {
			t.Fatalf("status %s, exit %d, review %+v", r.Status, code, r.Review)
		}
	})

	// The reviewer stands by a comment it cannot place: after the retry it is
	// moved into the summary, and the run still succeeds. Nothing is dropped.
	t.Run("what survives the retry is salvaged", func(t *testing.T) {
		stubborn := reviewJSON(reviewChanges, reviewComment{File: "src/gw.go", Line: 1, Severity: "major", Body: "the import list is wrong"})
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: stubborn} })
		h, _ := reviewHarness(t, fs.URL)
		r, code, _ := oneShotResult(t, h, "review the change", "")
		if r.Status != statusPassed || code != exitOK || r.Review == nil {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
		if len(r.Review.Comments) != 0 {
			t.Fatalf("a comment outside every hunk was posted anyway: %+v", r.Review.Comments)
		}
		if !strings.Contains(r.Review.Summary, "the import list is wrong") {
			t.Fatalf("the finding was dropped:\n%s", r.Review.Summary)
		}
		if n := len(fs.reqs()); n != 2 {
			t.Fatalf("%d requests, want the review and one retry", n)
		}
	})
}

// A gateway that goes away is not a reviewer that failed: the ticket goes back
// in the queue rather than to a person, and there is still no review field.
func TestReviewInfraIsNotAFailedReview(t *testing.T) {
	t.Run("down before the first reply", func(t *testing.T) {
		t.Setenv("LCA_GW_MAX_WAIT", "1")
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{cut: true} })
		h, _ := reviewHarness(t, fs.URL)
		r, code, _ := oneShotResult(t, h, "review the change", "")
		if r.Status != statusInfra || code != exitInfra {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
		if r.Review != nil {
			t.Fatalf("a run that reached no reply reported a review: %+v", r.Review)
		}
	})

	t.Run("down between the review and its retry", func(t *testing.T) {
		t.Setenv("LCA_GW_MAX_WAIT", "1")
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			if n == 1 {
				return fakeReply{content: "Looks fine to me."} // unreadable: the retry is bought
			}
			return fakeReply{cut: true}
		})
		h, _ := reviewHarness(t, fs.URL)
		r, code, _ := oneShotResult(t, h, "review the change", "")
		if r.Status != statusInfra || code != exitInfra {
			t.Fatalf("a dropped connection became %s/%d: %q", r.Status, code, r.Reason)
		}
		if r.Review != nil {
			t.Fatal("a review appeared out of a dropped connection")
		}
	})
}

// A readable review wins over the other reasons a run would have been called
// unverified, and a red -check beside it still does not become a pass.
func TestReviewAndACheck(t *testing.T) {
	good := reviewJSON(reviewApprove)
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: good} })
	h, _ := reviewHarness(t, fs.URL)
	r, code, _ := oneShotResult(t, h, "review the change", "ls")
	if r.Status != statusPassed || code != exitOK {
		t.Fatalf("a green check and a readable review: status %s, exit %d", r.Status, code)
	}
	if r.Review == nil || r.Review.Verdict != reviewApprove {
		t.Fatalf("%+v", r.Review)
	}

	fs2 := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: good} })
	h2, _ := reviewHarness(t, fs2.URL)
	r2, code2, _ := oneShotResult(t, h2, "review the change", "ls /definitely-no-such-path-xyz")
	if r2.Status != statusFailed || code2 != exitFailed {
		t.Fatalf("a red check is still a failed run: status %s, exit %d", r2.Status, code2)
	}
	// And the review is still reported: the wrapper needs it for the comment it
	// posts, whatever the check said.
	if r2.Review == nil {
		t.Fatal("the review was thrown away because the check was red")
	}
}

// -diff-base is resolved before the gateway is touched, and the message names
// what is missing.
func TestDiffBaseErrors(t *testing.T) {
	t.Run("no such base", func(t *testing.T) {
		root := t.TempDir()
		reviewRepo(t, root)
		_, err := prepareReview(Config{Root: root, Dir: t.TempDir()}, "origin/main")
		if err == nil {
			t.Fatal("a base that does not exist was accepted")
		}
		var ue *usageErr
		if !errors.As(err, &ue) {
			t.Fatalf("a base that does not exist must be row 2 of the table, got %T: %v", err, err)
		}
		for _, want := range []string{"origin/main", "fetch"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message does not mention %q: %v", want, err)
			}
		}
	})

	t.Run("not a repository", func(t *testing.T) {
		if _, err := prepareReview(Config{Root: t.TempDir(), Dir: t.TempDir()}, "main"); err == nil {
			t.Fatal("a directory that is not a repository was accepted")
		}
	})

	// Nothing to review is not an empty approve: the wrapper should not have
	// called the reviewer, and an approve nobody meant is the thing this whole
	// path exists to prevent.
	t.Run("nothing changed", func(t *testing.T) {
		root := t.TempDir()
		gitT(t, root, "init", "-q")
		gitT(t, root, "config", "user.email", "t@t")
		gitT(t, root, "config", "user.name", "t")
		gitT(t, root, "config", "commit.gpgsign", "false")
		writeAll(t, root, map[string]string{"a.go": "package a\n"})
		gitT(t, root, "add", "-A")
		gitT(t, root, "commit", "-q", "-m", "one")
		_, err := prepareReview(Config{Root: root, Dir: t.TempDir()}, "HEAD")
		if err == nil || !strings.Contains(err.Error(), "no diff to review") {
			t.Fatalf("an unchanged tree: %v", err)
		}
	})

	// The pipeline always runs the reviewer in a worktree, and a worktree shares
	// its refs with the main checkout.
	t.Run("inside a worktree", func(t *testing.T) {
		top := t.TempDir()
		reviewRepo(t, top)
		gitT(t, top, "add", "-A")
		gitT(t, top, "commit", "-q", "-m", "the coder's work")
		wt := filepath.Join(t.TempDir(), "agent-PROJ-1")
		gitT(t, top, "-c", "core.hooksPath="+os.DevNull, "worktree", "add", "-q", "-b", "agent/PROJ-1", wt, "HEAD")
		t.Cleanup(func() { gitRun(top, nil, nil, "worktree", "remove", "--force", wt) })
		writeAll(t, wt, map[string]string{"src/gw.go": gwBugged + "\n// one more\n"})
		rr, err := prepareReview(Config{Root: wt, Dir: t.TempDir()}, "pipeline-base")
		if err != nil {
			t.Fatalf("a base in the main checkout was not visible from the worktree: %v", err)
		}
		if !strings.Contains(rr.diff, plantedBug) {
			t.Fatalf("the worktree's diff is not the worktree's:\n%s", rr.diff)
		}
	})
}

// ── P1-2: the next round on the same session ────────────────────────────────

// roundOf is what main() does for one one-shot round: the same home, the same
// root, and the session id the round is to adopt ("" mints a new one).
func roundOf(t *testing.T, gw, home, root, sessionID string) (*Orchestrator, *Session, Config) {
	t.Helper()
	t.Setenv("HOME", home)
	cfg := Config{Root: root, Dir: filepath.Join(home, ".lca"), BaseURL: gw, Endpoints: []string{gw},
		Model: "test-model", MaxSteps: 20, Allowed: []string{"echo", "ls"}, Tools: "native", SubagentMax: 1, KeepSessions: 10}
	ap := NewApprover(newStringInput(""))
	ap.TrustAll()
	orch, err := setupOrchestratorOn(cfg, ap, filepath.Join(home, "trace-"+firstNonEmpty(sessionID, "one")+".jsonl"), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		orch.CloseMCP()
		orch.tracer.Close()
		orch.rec.Close()
	})
	sess, err := orch.NewPrimary("build", "", &quietView{})
	if err != nil {
		t.Fatal(err)
	}
	return orch, sess, cfg
}

func runRound(t *testing.T, orch *Orchestrator, sess *Session, prompt string) runResult {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "result")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	captureStderr(t, func() {
		captureStdout(t, func() { oneShot(orch, sess, prompt, "", true, out, nil) })
	})
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return decodeOneObject(t, string(b))
}

// The criterion: the second round uses the first's history, it is visible in the
// transcript, and the rounds share an x-root-session-id.
func TestSessionContinuesTheRound(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{content: "I changed the condition in gw.go."}
		}
		return fakeReply{content: "I have addressed the reviewer's comment."}
	})
	home, root := t.TempDir(), t.TempDir()

	orch1, sess1, _ := roundOf(t, fs.URL, home, root, "")
	r1 := runRound(t, orch1, sess1, "fix the condition in gw.go")
	uid := r1.Session
	if uid == "" || r1.Transcript == "" {
		t.Fatalf("round one reported no session: %+v", r1)
	}

	orch2, sess2, cfg2 := roundOf(t, fs.URL, home, root, uid)
	m, err := resumableSession(cfg2, uid)
	if err != nil {
		t.Fatalf("resumableSession: %v", err)
	}
	n, err := continueSession(orch2, sess2, m)
	if err != nil {
		t.Fatalf("continueSession: %v", err)
	}
	if n == 0 {
		t.Fatal("no history came back")
	}
	r2 := runRound(t, orch2, sess2, "the reviewer says the guard is still wrong — here are the comments")

	// Its own x-session-id, which is the gateway's cache key, and therefore the
	// same root session: the two rounds are one conversation.
	if sess2.UID != uid {
		t.Fatalf("round two runs as %q, not as %q — the gateway has no cached prefix for it", sess2.UID, uid)
	}
	rs := fs.reqs()
	first, second := rs[0], rs[len(rs)-1]
	if got := second.Header.Get("x-session-id"); got != uid {
		t.Fatalf("x-session-id %q, want %q", got, uid)
	}
	if a, b := first.Header.Get("x-root-session-id"), second.Header.Get("x-root-session-id"); a == "" || a != b {
		t.Fatalf("the rounds do not share an x-root-session-id: %q and %q", a, b)
	}

	// The first round's history really was sent: its task and its reply are in
	// the second round's request, under the same system prompt.
	body, _ := json.Marshal(second.Messages)
	for _, want := range []string{"fix the condition in gw.go", "I changed the condition in gw.go"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("round two did not carry %q:\n%s", want, body)
		}
	}
	if first.system() != second.system() {
		t.Fatal("the system prompt moved between the rounds, so the prefix cache was cold")
	}

	// And it is visible in the transcript: one file, both rounds, which is what
	// the wrapper attaches to the ticket.
	if r2.Session != uid {
		t.Fatalf("round two reported session %q", r2.Session)
	}
	if r2.Transcript != r1.Transcript {
		t.Fatalf("the rounds wrote two transcripts: %q and %q", r1.Transcript, r2.Transcript)
	}
	saved, err := loadSession(r2.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, msg := range saved {
		all.WriteString(msg.Role + ": " + msg.Content + "\n")
	}
	for _, want := range []string{"fix the condition in gw.go", "I changed the condition in gw.go",
		"the reviewer says the guard is still wrong", "I have addressed the reviewer's comment"} {
		if !strings.Contains(all.String(), want) {
			t.Fatalf("the transcript does not hold both rounds — %q is missing:\n%s", want, all.String())
		}
	}
}

// A -session that names nothing is a mistake in the CALL: row 2, before the
// gateway is touched. And a uid is a file name, so it is validated as one.
func TestResumableSessionRefusals(t *testing.T) {
	home := t.TempDir()
	cfg := Config{Root: t.TempDir(), Dir: filepath.Join(home, ".lca")}
	if err := os.MkdirAll(filepath.Join(cfg.stateDir(), "transcripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		uid  string
		say  string
	}{
		{"a session that does not exist", "20260102-030405-999", "no transcript"},
		{"nothing at all", "", "session id"},
		{"a path", "../../../etc/passwd", "not one"},
		{"a slash", "transcripts/x", "not one"},
		{"a dotdot", "a..b", "not a session id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resumableSession(cfg, tc.uid)
			if err == nil {
				t.Fatal("accepted")
			}
			var ue *usageErr
			if !errors.As(err, &ue) {
				t.Fatalf("not a usage error: %T %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.say) {
				t.Fatalf("the message does not say %q: %v", tc.say, err)
			}
		})
	}

	// A transcript with nothing but a system prompt in it is not a round to
	// continue: there is no history, and saying so is better than a silent
	// fresh start.
	p := transcriptPath(cfg, "20260102-030405-1")
	b, _ := json.Marshal([]Message{{Role: "system", Content: "you are an agent"}})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := resumableSession(cfg, "20260102-030405-1")
	if err != nil {
		t.Fatal(err)
	}
	orch, sess, _ := roundOf(t, "http://127.0.0.1:1", home, cfg.Root, "20260102-030405-1")
	if _, err := continueSession(orch, sess, m); err == nil {
		t.Fatal("a transcript with no round in it was continued")
	}
}
