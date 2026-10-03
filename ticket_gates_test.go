package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// zeroExit is a check that passed, for the states these tests start from.
var zeroExit = 0

// My own assertions on the three findings whose failure mode is a change to a
// shared remote that nobody authorised. The reviewers found all three in the
// first cut and reported them as blockers; these are written against the
// behaviour rather than against the fix, so they would catch a regression that
// restored any of them by another route.

// A branch may grow a commit after the review — the probe tolerates it on
// purpose, because the coder's own session may have written one — and that
// commit has had no check run on it and no reviewer look at it. The merge takes
// the SHA the gate approved, or it refuses.
func TestOnlyTheApprovedCommitIsMerged(t *testing.T) {
	w := newFakeWorld(t)
	w.heads["main"] = "base000"
	w.heads["agent/BSK-1"] = "unreviewed-999" // the branch moved after the verdict
	w.ancestry["unreviewed-999"] = []string{"approved-111", "base000"}
	w.ancestry["approved-111"] = []string{"base000"}

	st := &TicketState{Ticket: "BSK-1", State: tktReviewed, Branch: "agent/BSK-1",
		Summary: "x", BranchAt: "base000", Head: "approved-111", Verdict: tktApprove,
		Check: TicketCheck{Cmd: "go test", Exit: &zeroExit, Attempts: 1}}
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)

	status, reason := r.execute()
	if w.calls["mergein"] == 0 {
		t.Fatal("the merge must be attempted, so that it can be the thing that refuses")
	}
	if status == statusPassed {
		t.Fatalf("a commit no check ran on and no reviewer saw must not reach the target branch: %s", reason)
	}
	if w.heads["main"] != "base000" {
		t.Fatalf("the target branch moved: %s", w.heads["main"])
	}
	if w.calls["pushbranch"] != 0 || w.calls["createmr"] != 0 {
		t.Fatal("nothing downstream of the merge may run when the merge refused")
	}
	if !strings.Contains(reason, "approved") && !strings.Contains(reason, "approved-111") {
		t.Fatalf("the refusal has to name what it compared: %q", reason)
	}
}

// One ticket, one merge request. The find tool answering "none" is not evidence
// that the recorded one is gone — a forge whose filter is ignored, or which is
// eventually consistent, answers none while the merge request is right there —
// and creating a second is a change to a shared forge that nobody asked for.
func TestARecordedMergeRequestStopsASecondOne(t *testing.T) {
	w := newFakeWorld(t)
	w.heads["main"] = "mergedmain"
	w.heads["agent/BSK-1"] = "work111"
	w.ancestry["work111"] = []string{"base000"}
	w.ancestry["mergedmain"] = []string{"work111", "base000"}
	w.mr = nil // the forge says there is no open merge request for this branch

	st := &TicketState{Ticket: "BSK-1", State: tktPushed, Branch: "agent/BSK-1",
		Summary: "x", BranchAt: "base000", Head: "work111", PushedSha: "work111",
		Verdict: tktApprove, Check: TicketCheck{Cmd: "go test", Exit: &zeroExit, Attempts: 1},
		MergeRequest: &TicketMR{ID: "7", URL: "https://forge/r/7"}} // ...but we opened one last night
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)

	status, reason := r.execute()
	if w.calls["createmr"] != 0 {
		t.Fatalf("a second merge request was opened for one ticket (%s / %s)", status, reason)
	}
	if !strings.Contains(reason, "second merge request") && !strings.Contains(reason, "already has one") {
		t.Fatalf("the operator has to be told which merge request to look at: %q", reason)
	}
}

// A run that ends in an ERROR has not finished the work. Moving the ticket to
// the done status on the way out is the one outcome that loses a ticket
// silently: nobody looks at a ticket that says it is done.
func TestAnErroredRunNeverMovesTheTicketToDone(t *testing.T) {
	w := newFakeWorld(t)
	w.heads["main"] = "base000"
	// An error that is NOT a gate: the branch moved off the approved commit, so
	// the merge refuses. The run ends in an error with the gates themselves
	// satisfied, which is the state the finding was about.
	w.heads["agent/BSK-1"] = "moved-222"
	w.ancestry["moved-222"] = []string{"work111", "base000"}
	w.ancestry["work111"] = []string{"base000"}

	st := &TicketState{Ticket: "BSK-1", State: tktReviewed, Branch: "agent/BSK-1",
		Summary: "x", BranchAt: "base000", Head: "work111", Verdict: tktApprove,
		Check: TicketCheck{Cmd: "go test", Exit: &zeroExit, Attempts: 1}}
	r := newRun(t, w, pipeFrom(t, fullPipeline), st)

	status, _ := r.execute()
	if status == statusPassed {
		t.Fatal("a failed merge is not a passed run")
	}
	for _, c := range w.comments {
		if strings.Contains(c, "status=Done") {
			t.Fatalf("an errored run moved the ticket to the done status: %v", w.comments)
		}
	}
}

// Two ticket keys that differ only in a character a path cannot hold must not
// share one state directory: the second ticket would adopt the first's branch,
// its head, its verdict and its merge request.
func TestTwoTicketKeysNeverShareOneStateDirectory(t *testing.T) {
	seen := map[string]string{}
	for _, key := range []string{"BSK-1", "BSK/1", "BSK:1", "BSK 1", "bsk-1", "../BSK-1", "BSK-1/../BSK-2"} {
		slug := tktSlug(key)
		// One path component, and not a traversal. ".." INSIDE a name is harmless
		// (`BSK-1-..-BSK-2` is a directory like any other); what must not happen is a
		// separator, or the component being `.` or `..` itself.
		if slug == "" || slug == "." || slug == ".." || strings.ContainsRune(slug, '/') ||
			strings.ContainsRune(slug, filepath.Separator) || filepath.Clean(slug) != slug {
			t.Fatalf("%q became %q, which is not one directory name", key, slug)
		}
		if other, dup := seen[slug]; dup {
			t.Fatalf("%q and %q both became %q — the second ticket would adopt the first's branch and merge request", other, key, slug)
		}
		seen[slug] = key
	}
}
