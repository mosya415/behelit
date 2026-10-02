package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The pipeline profile denies `git push *` so an unattended run cannot publish
// anything; the wrapper owns pushing. That deny used to bind the model's
// run_command and nothing else, so `-check 'git push origin HEAD'` cleared the
// allowlist on `git` alone and pushed for real. A check is the operator's own
// line, but a profile whose rules "apply to EVERY role" has to mean it.
func TestADeniedCheckCommandIsRefused(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "x"} })
	h := newHarness(t, fs.URL, "native", false)
	h.orch.userRules = Ruleset{{"run", "git push *", Deny}}
	// git and go are on the allowlist, as they are in the pipeline profile: the
	// point of the test is that passing the allowlist is no longer enough.
	jl, err0 := NewJail(h.sess.jail().Root, []string{"git", "go"}, false)
	if err0 != nil {
		t.Fatal(err0)
	}
	h.orch.jl = jl

	err := h.sess.checkCmd("git push origin HEAD")
	if err == nil {
		t.Fatal("a check command the operator's own profile denies must not run")
	}
	if !strings.Contains(err.Error(), "permission rule") || !strings.Contains(err.Error(), "git push origin HEAD") {
		t.Fatalf("the refusal must name the rule and the line, got %q", err)
	}
	// The same profile leaves every ordinary check alone: defaultRules() has no
	// run deny at all, so only what the operator denied is refused.
	if err := h.sess.checkCmd("go test ./..."); err != nil {
		t.Fatalf("an ordinary check must still run: %v", err)
	}
}

// The sandbox and the deny rules see a command LINE, never the inside of a
// script. With -check pointing into the worktree the model edits — which is
// what the wrapper's invocation does — one edit to that script turns the
// verifier into a way to run anything with the operator's own hand.
func TestACheckTheRunRewroteIsRefused(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "x"} })
	h := newHarness(t, fs.URL, "native", false)
	root := h.sess.jail().Root

	if f := h.sess.rewroteCheck("bash check.sh"); f != "" {
		t.Fatalf("nothing is changed yet, so no check is tainted; got %q", f)
	}

	h.orch.noteApplied(root, []string{"check.sh"}, 12)

	if f := h.sess.rewroteCheck("bash check.sh"); f != "check.sh" {
		t.Fatalf("a check running a file this run wrote must be refused, got %q", f)
	}
	if f := h.sess.rewroteCheck(filepath.Join(root, "check.sh")); f == "" {
		t.Fatal("the absolute spelling of the same file must be caught too")
	}
	// Narrow on purpose: these name no file the run applied, so a run that
	// rewrote the Makefile still gets to run `make test`.
	for _, ok := range []string{"go test ./...", "make test", "cargo check --all"} {
		if f := h.sess.rewroteCheck(ok); f != "" {
			t.Fatalf("%q names no applied file, yet %q was reported", ok, f)
		}
	}
}

// In a team the children are where the context fills while the primary stays
// nearly empty — and a subagent's session is dropped from the registry the
// moment it stops being resumable, so its fill had to be recorded on the
// history entry before it goes. Without this, /context answered for the one
// session that had nothing in it, which is how "multi-agent mode does not show
// how full the context is" reads from the inside.
func TestAFinishedSubagentsContextFillIsRemembered(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", false)

	entry := h.orch.trackStart("t1", "task", "coder", "count the lines")
	child := h.sess // any session with messages and a known window will do
	child.client.SetCtxLen(262144)
	child.Msgs = append(child.Msgs, Message{Role: "user", Content: strings.Repeat("a measured amount of context ", 200)})

	h.orch.noteCtx(entry, child)
	h.orch.trackEnd(entry, "passed", "done")

	hist := h.orch.History()
	if len(hist) != 1 {
		t.Fatalf("one entry, got %d", len(hist))
	}
	e := hist[0]
	if e.Ctx <= 0 {
		t.Fatal("the child's own fill was not recorded, so /context cannot show it")
	}
	// The denominator is the TRANSCRIPT budget and not the raw window — the same
	// number the gauge divides by — so the panel and the status line can never
	// disagree about what "80% full" means.
	if e.CtxOf != child.budget() {
		t.Fatalf("the budget it was measured against must be recorded too: %d, want %d", e.CtxOf, child.budget())
	}
	if e.Model == "" {
		t.Fatal("which model spent that context is half the answer")
	}
	// A child with no window of its own is handled by the panel, which prints the
	// token count with no denominator rather than dividing by a placeholder —
	// asserted there rather than here, because budget() has four sources and
	// zeroing them one by one in a test says nothing about the panel.
}
