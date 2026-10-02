package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The one way this feature could do real harm. The rule used to be "the last
// complete object that parses wins", so a reviewer that sent its verdict and
// then added a note had its whole review replaced by the note: request_changes
// with a blocker became approve, comments: [], exit 0, no retry — a bad change
// reaching master under the word "approve".
func TestATrailingObjectCannotReplaceTheReview(t *testing.T) {
	reply := `{"verdict":"request_changes","comments":[{"file":"a.go","line":4,"severity":"blocker","body":"this frees twice"}],"summary":"one blocker"}

Hope this helps! {"note":"let me know if you want more detail"}`

	raw, err := extractJSONObject(reply)
	if err != nil {
		t.Fatalf("the review is right there: %v", err)
	}
	if !strings.Contains(raw, "request_changes") || !strings.Contains(raw, "frees twice") {
		t.Fatalf("the object carrying the verdict must win, got %s", raw)
	}
}

// A reviewer that wraps its answer in the field name the README itself uses is
// read, not refused.
func TestAWrappedReviewIsStillRead(t *testing.T) {
	raw, err := extractJSONObject(`{"review": {"verdict":"approve","comments":[],"summary":"fine"}}`)
	if err != nil {
		t.Fatalf("unwrapping is one field deep: %v", err)
	}
	if !strings.Contains(raw, `"verdict":"approve"`) {
		t.Fatalf("got %s", raw)
	}
}

// Two verdicts in one reply are not a review. Unreadable is the safe answer:
// the document says an unreadable object is `status: failed`, never a silent
// approve, and picking either one of two disagreeing objects would be a guess
// about which way the reviewer meant to decide.
func TestTwoDisagreeingVerdictsAreUnreadable(t *testing.T) {
	_, err := extractJSONObject(`{"verdict":"request_changes","comments":[],"summary":"no"}
later: {"verdict":"approve","comments":[],"summary":"yes"}`)
	if err == nil {
		t.Fatal("two disagreeing verdicts must not resolve to one of them")
	}
	if !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("the message has to say what is wrong: %v", err)
	}
	// The same object twice — a fence and then prose — is NOT a disagreement.
	if _, err := extractJSONObject(`{"verdict":"approve","comments":[],"summary":"ok"} and again {"verdict":"approve","comments":[],"summary":"ok"}`); err != nil {
		t.Fatalf("an identical repeat is one answer: %v", err)
	}
}

// The retry is asked for because something was wrong with the first answer, not
// because the first answer was imaginary. Replacing it wholesale dropped every
// finding the first read produced — and the verdict with them, so a
// request_changes came back as approve from a reviewer that never changed its
// mind.
func TestTheRetryAddsToTheFirstReadInsteadOfReplacingIt(t *testing.T) {
	first := &reviewReport{Verdict: reviewChanges, Summary: "two problems",
		Comments: []reviewComment{{File: "a.go", Line: 4, Severity: "blocker", Body: "frees twice"}}}
	second := &reviewReport{Verdict: reviewApprove, Summary: "actually fine",
		Comments: []reviewComment{{File: "b.go", Line: 9, Severity: "minor", Body: "name it better"}}}

	got := mergeReviews(first, second)
	if got.Verdict != reviewChanges {
		t.Fatalf("the stricter verdict wins: a found defect does not become unfound, got %q", got.Verdict)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("both findings survive, got %d: %+v", len(got.Comments), got.Comments)
	}
	// A reviewer re-sending the same finding corrected is the normal case, and it
	// must not be reported twice.
	again := mergeReviews(first, &reviewReport{Verdict: reviewChanges, Comments: first.Comments, Summary: "same"})
	if len(again.Comments) != 1 {
		t.Fatalf("an identical finding is one finding, got %d", len(again.Comments))
	}
}

// An added line whose own text begins with "++ " reads as a `+++ ` file header
// unless the hunk's counts are honoured — and the real file then loses every
// hunk after it, so a comment on a line that IS in the diff is told it is not.
func TestADiffBodyLineIsNotReadAsAFileHeader(t *testing.T) {
	diff := "diff --git a/m.c b/m.c\n--- a/m.c\n+++ b/m.c\n@@ -1,2 +1,4 @@\n ok\n+++ a line of C that starts with plus signs\n+int x = 1;\n ok2\n"
	cl := parseUnifiedDiff(diff)
	if len(cl.order) != 1 || cl.order[0] != "m.c" {
		t.Fatalf("one file, m.c, got %v", cl.order)
	}
	// Lines 1-4 of the new side are the hunk, and 2 and 3 are the added ones.
	for _, line := range []int{1, 2, 3, 4} {
		if p, ok := cl.place("m.c", line); !ok {
			t.Fatalf("line %d is inside the hunk: place(%q) = %v", line, p, ok)
		}
	}
	if k := cl.kindOf("m.c", 3); k != "added" {
		t.Fatalf("line 3 was added, got %q", k)
	}
	if k := cl.kindOf("m.c", 1); k != "context" {
		t.Fatalf("line 1 is context, got %q", k)
	}
}

// A comment is prose, and prose that mentions a control character should still
// read as prose: stripANSI deleted everything from an ESC to the next letter
// `m`, which silently ate the sentence after it.
func TestACommentBodyKeepsItsWordsAroundAnEscape(t *testing.T) {
	body := "the colour code \x1b[31m is written by hand here, which breaks the log parser"
	got := forComment(body)
	for _, must := range []string{"the colour code", "which breaks the log parser"} {
		if !strings.Contains(got, must) {
			t.Fatalf("%q lost %q", got, must)
		}
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("the escape itself must not reach the forge: %q", got)
	}
	if strings.ContainsRune(forComment("a\rb"), '\r') {
		t.Fatal("a CR rewrites the line a reader is looking at")
	}
}

// A session id is only unique inside one state directory, and a wrapper that
// points LCA_DIR at one place for several worktrees — one per ticket, which is
// exactly their shape — can hand round two the uid of another ticket's round
// one. The transcript would load, and the model would be told it had already
// edited files it has never seen.
func TestASessionFromAnotherTreeIsRefused(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, Root: "/work/ticket-2"}
	tp := transcriptPath(cfg, "20261002-1-1")
	if err := os.MkdirAll(filepath.Dir(tp), 0o755); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "round one"}}
	b, _ := json.Marshal(msgs)
	if err := os.WriteFile(tp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(cfg.stateDir(), "audit.jsonl")
	line := `{"kind":"session_start","session":"20261002-1-1","root":"/work/ticket-1"}` + "\n"
	if err := os.WriteFile(audit, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := resumableSession(cfg, "20261002-1-1")
	if err == nil {
		t.Fatal("a uid from another tree's round must not be continued")
	}
	if !strings.Contains(err.Error(), "another tree") {
		t.Fatalf("the message must say what is wrong: %v", err)
	}
	// The same uid from the SAME tree continues normally, and a missing audit
	// entry is not evidence of anything: it must not block a resume.
	cfg.Root = "/work/ticket-1"
	if _, err := resumableSession(cfg, "20261002-1-1"); err != nil {
		t.Fatalf("same tree, same session: %v", err)
	}
	if err := os.Remove(audit); err != nil {
		t.Fatal(err)
	}
	cfg.Root = "/work/anywhere"
	if _, err := resumableSession(cfg, "20261002-1-1"); err != nil {
		t.Fatalf("an audit that cannot say must not refuse: %v", err)
	}
}

// The transcript is rewritten WHOLE after every turn, so two rounds on one
// session do not interleave: the loser's turns are gone and the wrapper is told
// both succeeded.
func TestTwoRoundsCannotShareOneSession(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, Root: "/work/t"}
	if err := os.MkdirAll(filepath.Join(cfg.stateDir(), "transcripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockSession(cfg, "s1")
	if err != nil || unlock == nil {
		t.Fatalf("the first round takes the lock: %v", err)
	}
	if _, err := lockSession(cfg, "s1"); err == nil {
		t.Fatal("the second round must be refused while the first holds it")
	} else if !strings.Contains(err.Error(), "in use by pid") {
		t.Fatalf("the refusal must name the holder: %v", err)
	}
	unlock()
	if u2, err := lockSession(cfg, "s1"); err != nil || u2 == nil {
		t.Fatalf("released, so the next round may have it: %v", err)
	} else {
		u2()
	}
	// A dead holder is a crash, not a conflict: a killed round two must leave the
	// session resumable.
	lp := transcriptPath(cfg, "s1") + ".lock"
	if err := os.WriteFile(lp, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if u3, err := lockSession(cfg, "s1"); err != nil || u3 == nil {
		t.Fatalf("a lock whose process is gone is taken over: %v", err)
	} else {
		u3()
	}
}

// The session being continued is never collected. The call site's comment
// already claimed this; it was not true, so a round two on a session older than
// `keep` others deleted the transcript it was about to read.
func TestThePruneSparesTheSessionBeingContinued(t *testing.T) {
	dir := t.TempDir()
	var names []string
	for i := 0; i < 5; i++ {
		n := fmt.Sprintf("s%d", i)
		names = append(names, n)
		p := filepath.Join(dir, n+".json")
		if err := os.WriteFile(p, []byte(`[{"role":"user","content":"x"}]`), 0o600); err != nil {
			t.Fatal(err)
		}
		// s0 is the oldest, so it is the first thing a prune to 2 would take.
		mt := time.Now().Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	pruneTranscripts(dir, 2, "s0")
	if _, err := os.Stat(filepath.Join(dir, "s0.json")); err != nil {
		t.Fatalf("the oldest transcript was the one being continued: %v", err)
	}
	left := 0
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n+".json")); err == nil {
			left++
		}
	}
	// The spared one does not use up the keep budget, so 2 others survive beside it.
	if left != 3 {
		t.Fatalf("want the spared one plus 2 kept, got %d", left)
	}
}

// How long a task took does not say WHEN it finished, and a long run is read
// after the fact — often beside somebody else's incident: "the stand went down
// at 02:14, which of these had finished by then" is only answerable against a
// recorded clock time.
func TestAClockTimeSaysWhichDayItWas(t *testing.T) {
	now := time.Now()
	if got := clockOf(now); !strings.Contains(got, now.Local().Format("15:04")) {
		t.Fatalf("today is the time alone, got %q", got)
	}
	// Once it is not today any more, the date goes in front: "02:14" for
	// something that happened yesterday is worse than no answer.
	y := now.AddDate(0, 0, -1)
	got := clockOf(y)
	if !strings.Contains(got, y.Local().Format("Jan")) || !strings.Contains(got, y.Local().Format("15:04")) {
		t.Fatalf("yesterday needs its date, got %q", got)
	}
	if clockOf(time.Time{}) != gEllipsis {
		t.Fatalf("a time nobody recorded says so, got %q", clockOf(time.Time{}))
	}
}

// The wrapper has the clock time it launched lca, but not the one lca stopped
// at, and a duration cannot answer the question a ticket is read with: "the
// stand went down at 02:14 — had this run finished by then".
func TestTheResultObjectSaysWhenTheRunEnded(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", false)
	start := time.Now().Add(-90 * time.Second)
	r := h.orch.resultOf(h.sess, Verdict{Status: "passed"}, "", 0, 0, start, nil)

	for name, got := range map[string]string{"started_at": r.StartedAt, "finished_at": r.FinishedAt} {
		ts, err := time.Parse(time.RFC3339, got)
		if err != nil {
			t.Fatalf("%s must be RFC3339 a wrapper can parse, got %q: %v", name, got, err)
		}
		if ts.Location() != time.UTC {
			t.Fatalf("%s must be UTC, got %q", name, got)
		}
	}
	a, _ := time.Parse(time.RFC3339, r.StartedAt)
	b, _ := time.Parse(time.RFC3339, r.FinishedAt)
	if !b.After(a) {
		t.Fatalf("the run ended after it started: %s then %s", r.StartedAt, r.FinishedAt)
	}
	// The two ends and the duration are the same measurement, so they must agree.
	if d := b.Sub(a).Milliseconds(); d-r.DurationMs > 50 || r.DurationMs-d > 50 {
		t.Fatalf("duration_ms %d does not match %s..%s (%d ms)", r.DurationMs, r.StartedAt, r.FinishedAt, d)
	}
}
