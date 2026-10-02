package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// P1-3's acceptance criterion, word for word: "a check whose error is in the
// middle of twenty thousand lines — the error line reaches the model."
//
// This is the unit of it. The whole path is in
// TestTheErrorInTheMiddleOfAStandLogReachesTheModel below; this one pins the
// selection itself, because that is where the requirement's numbers live and a
// regression here is silent — the run still reports `failed`, the model still
// gets a tail, and the tail simply no longer has the line in it.
func TestTheLineThatMattersSurvivesTwentyThousandLines(t *testing.T) {
	const bad = "error: parser dropped the trailing newline at src/lex.rs:4891"
	var b strings.Builder
	for i := 1; i <= 20000; i++ {
		switch i {
		case 10000:
			b.WriteString(bad + "\n")
		default:
			fmt.Fprintf(&b, "   compiling crate number %d of 20000\n", i)
		}
	}
	// The epilogue a stand's harness prints: this is what `lastLines` used to
	// send, and it is the one part of the output that does not say what broke.
	b.WriteString("test result: FAILED. 411 passed; 1 failed\n")

	tail := checkTailOf(b.String())

	if !strings.Contains(tail, bad) {
		t.Fatalf("the error line did not reach the model; it got:\n%s", tail)
	}
	// Its context came with it: five lines either side is what makes the line
	// actionable rather than just alarming.
	if !strings.Contains(tail, "crate number 9996 of 20000") || !strings.Contains(tail, "crate number 10004 of 20000") {
		t.Fatalf("the five lines of context around it are missing:\n%s", tail)
	}
	// And the end of the log, which is where the count of failures is.
	if !strings.Contains(tail, "411 passed; 1 failed") {
		t.Fatalf("the last forty lines are missing:\n%s", tail)
	}
	// What was cut out is MARKED, with the line numbers it stood at: a tail that
	// silently skips nine thousand lines reads as a complete log.
	if !strings.Contains(tail, "omitted of 20001") {
		t.Fatalf("the omission is not marked, so the tail reads as the whole log:\n%s", tail)
	}
	// And it still fits: the point of selecting rather than truncating is to get
	// the line in front of a model whose context is finite.
	if len(tail) > checkTailBytes+1000 {
		t.Fatalf("the selection is %d bytes, over its own budget of %d", len(tail), checkTailBytes)
	}
}

// Short output is returned byte for byte. Every check that used to arrive whole
// still does: `go test ./...` on a red package prints forty lines and all of
// them are load-bearing, and an operator reading check_tail in the result object
// must see what the command printed rather than an edited version of it.
func TestShortCheckOutputIsNotEdited(t *testing.T) {
	out := "--- FAIL: TestFoo\n    foo_test.go:12: want 3, got 4\nFAIL\nexit status 1\n"
	if got := checkTailOf(out); got != strings.TrimRight(out, "\n") {
		t.Fatalf("short output was edited:\ngot  %q\nwant %q", got, strings.TrimRight(out, "\n"))
	}
	if checkTailOf("") != "" {
		t.Fatal("no output must stay no output")
	}
}

// The whole path, with a real check: the error line reaches the MODEL (the
// message the verifier appends to the conversation for the next attempt), and
// the full twenty thousand lines are on disk with their path in the verdict.
func TestTheErrorInTheMiddleOfAStandLogReachesTheModel(t *testing.T) {
	const bad = "error: the stand refused the deploy: no slot free"
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "I have fixed it"} })
	h := newHarness(t, fs.URL, "native", true)
	// sh, so the check can be a script; cat, so it can print the log. The point
	// of the test is the SIZE of the output, not what produced it.
	jl, err := NewJail(h.root, []string{"sh", "cat", "ls"}, false)
	if err != nil {
		t.Fatal(err)
	}
	h.orch.jl = jl

	var b strings.Builder
	for i := 1; i <= 20000; i++ {
		if i == 10000 {
			b.WriteString(bad + "\n")
			continue
		}
		fmt.Fprintf(&b, "deploy step %d: ok\n", i)
	}
	b.WriteString("check.sh: 1 of 412 checks failed, see above\n")
	if err := os.WriteFile(filepath.Join(h.root, "stand.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(h.root, "check.sh"), []byte("cat stand.log\nexit 1\n"), 0o755)

	// Two attempts, because the criterion is about what the model is TOLD: with
	// one attempt the verifier gives up before it says anything to anybody.
	v := h.sess.RunVerifiedAll(context.Background(), []string{"sh check.sh"}, 2)
	if v.Status != "failed" {
		t.Fatalf("a check exiting 1 twice is a failed run, got %s (%v)", v.Status, v.Err)
	}

	told := ""
	for _, m := range h.sess.Msgs {
		if m.Role == "user" && strings.Contains(m.Content, "The verifier ran") {
			told = m.Content
		}
	}
	if told == "" {
		t.Fatal("the model was never told the check failed")
	}
	if !strings.Contains(told, bad) {
		t.Fatalf("the error line did not reach the model:\n%s", ellipsize(told, 1200))
	}

	// The full output of every attempt, beside the transcript, with its path in
	// the verdict — and from there in the result object.
	if len(v.CheckLogs) != 2 {
		t.Fatalf("one full log per attempt, got %d: %v", len(v.CheckLogs), v.CheckLogs)
	}
	for _, p := range v.CheckLogs {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("the full output was not written: %v", err)
		}
		lines := strings.Count(string(data), "\n")
		if lines < 20000 {
			t.Fatalf("%s holds %d lines, not the whole output", p, lines)
		}
		if !strings.Contains(string(data), bad) {
			t.Fatalf("%s does not hold the error line", p)
		}
		// The markers in the tail are line numbers into THIS file, so the two have
		// to be cut from the same bytes: line 10000 of the file is the error.
		if got := strings.Split(string(data), "\n")[9999]; got != bad {
			t.Fatalf("line 10000 of the full log is %q, so the tail's omission markers point at the wrong lines", got)
		}
	}
	// And the result object carries them, never as null.
	r := h.orch.resultOf(h.sess, v, "sh check.sh", 0, 0, time.Now(), nil)
	if len(r.CheckLogs) != 2 {
		t.Fatalf("the result object must name every full log: %v", r.CheckLogs)
	}
}

// A run with no check reports an empty list and not null: the wrapper indexes
// the key, and `[]` is an answer while a KeyError on the row that calls a human
// is not.
func TestCheckLogsIsNeverNull(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "done"} })
	h := newHarness(t, fs.URL, "native", true)
	v := h.sess.RunVerifiedAll(context.Background(), nil, 1)
	r := h.orch.resultOf(h.sess, v, "", 0, 0, time.Now(), nil)
	if r.CheckLogs == nil {
		t.Fatal("check_logs must marshal as [] and not null")
	}
}

// The three reproducibility fields on a real one-shot's object, including the
// prompt hash: `lca_version` and `roles_hash` existed, and the third one is what
// notices a prompt edit that moved neither of them.
func TestTheResultObjectNamesTheBuildTheTeamAndThePrompt(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "done"} })
	h := newHarness(t, fs.URL, "native", true)
	r, _, _ := oneShotResult(t, h, "say done", "")
	if r.LCAVersion == "" {
		t.Fatal("lca_version must say which binary produced the object")
	}
	if len(r.PromptHash) != 64 {
		t.Fatalf("prompt_hash must be a sha256 of the role's system prompt, got %q", r.PromptHash)
	}
	if r.CheckLogs == nil {
		t.Fatal("check_logs must be [] and not null on a run with no check")
	}
}

// check_timeout on a ROLE, up to an hour. The team default has to stay what it
// was for every other role: raising it for the whole team to cover the one role
// that talks to the stand gives every cheap `go test` role an hour to hang in,
// which is the thing the per-role key exists to avoid.
func TestCheckTimeoutIsPerRoleAndCappedAtAnHour(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".lca"), 0o755)
	t.Setenv("LCA_ROLES", "")
	write := func(body string) (*RolesConfig, error) {
		os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"), []byte(body), 0o644)
		return loadRoles(Config{Root: root, Dir: t.TempDir()})
	}

	rc, err := write(`defaults:
  check_timeout: 600
roles:
  stand:
    models: [m-a]
    check_timeout: 3600
  quick:
    models: [m-a]
  halfhour:
    models: [m-a]
    check_timeout: 30m
`)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*Agent{}
	for _, r := range rc.Roles {
		by[r.Name] = r
	}
	if by["stand"].CheckTimeout != 3600 {
		t.Fatalf("an hour on a role must be accepted: %d", by["stand"].CheckTimeout)
	}
	if by["halfhour"].CheckTimeout != 1800 {
		t.Fatalf("a duration must be read as one: %d", by["halfhour"].CheckTimeout)
	}
	if by["quick"].CheckTimeout != 0 {
		t.Fatalf("a role that says nothing keeps the team default: %d", by["quick"].CheckTimeout)
	}

	o := &Orchestrator{roles: rc}
	if got := o.checkTimeoutOf(by["stand"]); got != time.Hour {
		t.Fatalf("the stand role's check gets its own hour, got %s", got)
	}
	if got := o.checkTimeoutOf(by["quick"]); got != 10*time.Minute {
		t.Fatalf("every other role keeps defaults: check_timeout, got %s", got)
	}
	if got := o.checkTimeoutOf(nil); got != 10*time.Minute {
		t.Fatalf("no role in hand is the team-wide answer, got %s", got)
	}
	// Round-tripped, or /role save would quietly drop it and the stand role would
	// be back on ten minutes after the next edit.
	if !strings.Contains(rc.YAML(), "check_timeout: 3600") {
		t.Fatalf("a role's check_timeout must be written back:\n%s", rc.YAML())
	}

	// Over the ceiling, and unparsable, are both ERRORS. A value silently
	// dropped leaves the default in place and looks obeyed, which is how a stand
	// check ends up killed at ten minutes and reported to the model as a failing
	// check — the one failure this key exists to remove.
	for _, bad := range []string{"7200", "2h", "forever", "0", "-5"} {
		_, err := write("roles:\n  stand:\n    models: [m-a]\n    check_timeout: " + bad + "\n")
		if err == nil {
			t.Fatalf("check_timeout: %s must be refused", bad)
		}
		if !strings.Contains(err.Error(), "check_timeout") {
			t.Fatalf("the refusal must name the key, got %q", err)
		}
	}
	// And the same ceiling under defaults:, so the two spellings cannot disagree.
	if _, err := write("defaults:\n  check_timeout: 7200\nroles:\n  stand:\n    models: [m-a]\n"); err == nil {
		t.Fatal("defaults: check_timeout over an hour must be refused too")
	}
}

// The same criterion, in the shape that broke it: the last forty lines are LONG.
//
// A stand's epilogue is forty JSON deploy records of about 1.5 KB each, and the
// line that says what went wrong is twenty rows above them. The selection used
// to serve the last forty first and stop at the first line it could not afford,
// which spent the whole budget on eight records and then had nothing left for
// the region holding `FAIL cannot bind port 8080`. The tail was 10 869 bytes of
// padding and the line the ticket turns on was simply absent.
func TestTheFailLineSurvivesALongEpilogue(t *testing.T) {
	const bad = "deploy: FAIL cannot bind port 8080, address already in use"
	var b strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&b, "step %d: ok\n", i)
	}
	// Forty records of ~1.5 KB, the twentieth of them the failure.
	for i := 1; i <= 40; i++ {
		if i == 20 {
			b.WriteString(bad + "\n")
			continue
		}
		fmt.Fprintf(&b, `{"deploy":%d,"host":"stand-07","payload":"%s"}`+"\n", i, strings.Repeat("x", 1450))
	}
	tail := checkTailOf(b.String())
	if !strings.Contains(tail, bad) {
		t.Fatalf("the failure line was crowded out by the epilogue around it:\n%s", ellipsize(tail, 900))
	}
	if len(tail) > checkTailBytes {
		t.Fatalf("the tail is %d bytes against a budget of %d", len(tail), checkTailBytes)
	}
}

// checkTailBytes is documented as a ceiling, so it has to be one. The omission
// markers used to be emitted after the accounting and never charged to it, which
// on the first of these came to 15 160 bytes against a stated 12 000 and on the
// second to 43 960 — 3.7x, because an eleven-line region of blank lines costs
// eleven bytes while the marker announcing the gap in front of it costs
// thirty-eight. This text is appended to the conversation once per failed
// attempt, so the overshoot is context nobody budgeted for.
func TestTheTailStaysInsideItsBudget(t *testing.T) {
	cases := map[string]func() string{
		// A test harness: twenty thousand lines, every fortieth one a failure.
		"a harness log": func() string {
			var b strings.Builder
			for i := 1; i <= 20000; i++ {
				if i%40 == 0 {
					fmt.Fprintf(&b, "✗ case_%05d  assertion failed\n", i)
					continue
				}
				fmt.Fprintf(&b, "ok case_%05d\n", i)
			}
			return b.String()
		},
		// A build log with blank lines between its failures: the shape where the
		// markers cost more than the lines they stand in for.
		"short lines": func() string {
			var b strings.Builder
			for i := 1; i <= 20000; i++ {
				if i%12 == 0 {
					b.WriteString("error\n")
					continue
				}
				b.WriteString("\n")
			}
			return b.String()
		},
		// One matching line per ten thousand: the ordinary case, which must not
		// have been made worse by bounding the pathological ones.
		"one failure": func() string {
			var b strings.Builder
			for i := 1; i <= 20000; i++ {
				if i == 9000 {
					b.WriteString("error: the one that matters\n")
					continue
				}
				fmt.Fprintf(&b, "step %d: ok\n", i)
			}
			return b.String()
		},
	}
	for name, gen := range cases {
		t.Run(name, func(t *testing.T) {
			tail := checkTailOf(gen())
			if len(tail) > checkTailBytes {
				t.Fatalf("%d bytes against a budget of %d", len(tail), checkTailBytes)
			}
			if !strings.Contains(strings.ToLower(tail), "error") && !strings.Contains(tail, "✗") {
				t.Fatalf("a budget that fits but says nothing about the failure:\n%s", ellipsize(tail, 600))
			}
		})
	}
}

// A check whose progress output uses only \r prints its whole run as ONE line.
// Clipping that through the middle, which is right for a minified bundle, cuts
// the error out of the selection that selected it — and because the line was
// still "kept", nothing said anything had been lost.
func TestAnErrorInsideOneEnormousLineIsNotClippedOut(t *testing.T) {
	const bad = "error: cannot bind port 8080"
	one := strings.Repeat("progress.", 100_000) + bad + strings.Repeat(".progress", 100_000)
	tail := checkTailOf(one)
	if !strings.Contains(tail, bad) {
		t.Fatalf("the error was cut out of the one line that held it:\n%s", ellipsize(tail, 400))
	}
	if len(tail) > checkTailBytes {
		t.Fatalf("the tail is %d bytes against a budget of %d", len(tail), checkTailBytes)
	}
}

// `-session <uid>` starts the attempt counter at 1 again, so round two's first
// attempt wrote the name round one's first attempt already had — over the top of
// it, at the path round one's JSON result had handed the wrapper, whose
// check_tail markers index the bytes that write replaced.
func TestASecondRoundDoesNotOverwriteTheFirstRoundsCheckLog(t *testing.T) {
	cfg := Config{Dir: t.TempDir()}
	const uid = "20261003-010203-999"

	r1, err := NewRecorderOn(cfg, uid)
	if err != nil {
		t.Fatal(err)
	}
	first, err := r1.CheckLog(uid, 1, 0, "round one, attempt one\n")
	if err != nil {
		t.Fatal(err)
	}
	r1.Close()

	// A second process, the same session: exactly what `-session <uid>` builds.
	r2, err := NewRecorderOn(cfg, uid)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r2.CheckLog(uid, 1, 0, "round two, attempt one\n")
	if err != nil {
		t.Fatal(err)
	}
	r2.Close()

	if first == second {
		t.Fatalf("both rounds wrote %s, so round one's output is gone", first)
	}
	b, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("round one's log is gone: %v", err)
	}
	if !strings.Contains(string(b), "round one") {
		t.Fatalf("round one's log holds round two's bytes: %q", string(b))
	}
	// And both rounds still count as ONE session for the prune, so a run with a
	// second round does not evict twice its share of the history.
	if got := checkLogSession(filepath.Base(second)); got != uid {
		t.Fatalf("the prune reads %q as the session of %s, not %q", got, filepath.Base(second), uid)
	}
}

// The prune bounds the directory in BYTES as well as in sessions, because one
// attempt of a stand check was measured at 500 MB and KeepSessions — 200, a
// number chosen for kilobyte transcripts — would have held ~200 GB of them
// before deleting a single file. The newest session survives whatever its size:
// it is the run whose result object just named these paths.
func TestTheCheckLogsAreBoundedInBytes(t *testing.T) {
	dir := t.TempDir()
	write := func(session string, size int, age time.Duration) string {
		p := filepath.Join(dir, session+"-1.1.log")
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Three sessions, each over the whole directory budget on its own.
	old := write("s1", checkLogDirBytes/2+1, 3*time.Hour)
	mid := write("s2", checkLogDirBytes/2+1, 2*time.Hour)
	newest := write("s3", checkLogDirBytes/2+1, time.Hour)
	// A leftover from an interrupted write, which a *.log sweep cannot see.
	stale := filepath.Join(dir, ".lca-tmp-123456")
	os.WriteFile(stale, []byte("half a gigabyte, once"), 0o600)
	when := time.Now().Add(-2 * checkLogTmpAge)
	os.Chtimes(stale, when, when)

	pruneCheckLogs(dir, 200, "")

	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("the newest session's logs were collected: %v", err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("the oldest session is over the byte budget and was kept")
	}
	if _, err := os.Stat(mid); err == nil {
		t.Fatal("the directory is still over the byte budget")
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("an interrupted write's leftover is never collected by anything else")
	}
}

// The session a `-session <uid>` round is continuing is never collected, for the
// same reason pruneTranscripts takes the same argument: its paths are in the
// JSON the wrapper already recorded against the ticket.
func TestThePruneSparesTheResumedSession(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []string{"s1", "s2", "s3"} {
		p := filepath.Join(dir, s+"-1.1.log")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-time.Hour)
		if s == "s1" {
			when = time.Now().Add(-3 * time.Hour) // the oldest: first to go
		}
		os.Chtimes(p, when, when)
	}
	pruneCheckLogs(dir, 1, "s1")
	if _, err := os.Stat(filepath.Join(dir, "s1-1.1.log")); err != nil {
		t.Fatalf("the resumed session's logs were deleted under it: %v", err)
	}
}

// A delegation's logs belong to its lead's slot. `<uid>-t1` and `<uid>-t2` each
// took one of the two hundred, so a lead with ten subagents consumed eleven and
// evicted ten earlier runs whose transcripts outlived their logs.
func TestADelegationsCheckLogsCountAsItsLeads(t *testing.T) {
	for name, want := range map[string]string{
		"20261003-010203-999-1.1.log":         "20261003-010203-999",
		"20261003-010203-999.r2-1.1.log":      "20261003-010203-999",
		"20261003-010203-999-t1-2.1.log":      "20261003-010203-999",
		"20261003-010203-999-t12.r3-1.2.log":  "20261003-010203-999",
		"20261003-010203-999-compact-1.1.log": "20261003-010203-999",
	} {
		if got := checkLogSession(name); got != want {
			t.Errorf("checkLogSession(%q) = %q, want %q", name, got, want)
		}
	}
}
