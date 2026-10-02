package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The secret the fixtures leak. Spelled out as a constant so a failure message
// can say which of the four artefacts it was found in.
const fakeToken = "sk-live-9d41-this-is-not-a-real-token"

// armSecrets puts credentials in the environment and makes the scrub read them.
// envSecrets is a sync.Once, so a test states what it needs rather than relying
// on the order tests run in — and puts it back, because every other test in the
// package shares this process.
func armSecrets(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
	secretsOnce = sync.Once{}
	secretVals = nil
	t.Cleanup(func() { secretsOnce = sync.Once{}; secretVals = nil })
}

// P2-2: the values of *TOKEN* / *KEY* / *SECRET* / *PASSWORD* variables are
// [redacted] in EVERYTHING written to the transcript, the trace, the report and
// the summary. The transcript is the one that matters most and was not covered
// before: it goes to Jira as an attachment.
//
// One test for all four, because the requirement is "find every writer" and a
// test per writer would pass while the fifth one leaked.
func TestSecretsAreOutOfEveryArtefact(t *testing.T) {
	armSecrets(t, map[string]string{"BSK_GW_TOKEN": fakeToken, "JIRA_PASSWORD": "hunter2hunter2"})

	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "the deploy logged " + fakeToken} })
	h := newHarness(t, fs.URL, "native", true)
	jl, err := NewJail(h.root, []string{"sh", "cat", "ls"}, false)
	if err != nil {
		t.Fatal(err)
	}
	h.orch.jl = jl

	// ── the transcript ────────────────────────────────────────────────────────
	// Everything the model saw is in here, which is how a tool result holding a
	// token becomes an attachment on a ticket.
	h.sess.Msgs = append(h.sess.Msgs,
		Message{Role: "user", Content: "the stand is refusing: Authorization: Bearer " + fakeToken},
		Message{Role: "assistant", Content: "I will retry with " + fakeToken})
	if err := h.sess.saveTranscript(); err != nil {
		t.Fatal(err)
	}
	tr, err := os.ReadFile(h.orch.rec.SessionPath())
	if err != nil {
		t.Fatal(err)
	}
	assertScrubbed(t, "the transcript", string(tr))
	// Still a transcript: the scrub must not have broken the JSON a reviewer's
	// tooling (and `lca report`) reads it with.
	var msgs []Message
	if err := json.Unmarshal(tr, &msgs); err != nil {
		t.Fatalf("the scrub left the transcript unparseable: %v", err)
	}
	if len(msgs) != len(h.sess.Msgs) {
		t.Fatalf("the transcript lost messages: %d of %d", len(msgs), len(h.sess.Msgs))
	}

	// ── the audit ─────────────────────────────────────────────────────────────
	h.orch.rec.Event("run_command", map[string]any{"cmd": "curl -H 'Authorization: Bearer " + fakeToken + "'"})

	// ── the trace ─────────────────────────────────────────────────────────────
	// A turn record carries the arguments of every tool call and the text of
	// every tool error.
	h.orch.tracer.write(TurnRecord{Type: "turn", TS: nowTS(), Session: h.sess.UID, RootSession: h.sess.UID,
		Role: "coder", Model: "m", Step: 1, Transport: transportNative, Member: localMemberName,
		ToolCalls: []traceToolCall{{Name: "run_command", Args: `{"cmd":"deploy --token ` + fakeToken + `"}`, OK: false,
			Error: "401 for token " + fakeToken}}})
	h.orch.tracer.Close()
	tc, err := os.ReadFile(h.orch.tracer.Path)
	if err != nil {
		t.Fatal(err)
	}
	assertScrubbed(t, "the trace", string(tc))
	for _, line := range strings.Split(strings.TrimSpace(string(tc)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("the scrub left a trace line unparseable: %v\n%s", err, line)
		}
	}

	// ── the check's own output, and the full log beside it ────────────────────
	os.WriteFile(filepath.Join(h.root, "check.sh"),
		[]byte("echo \"deploying with token "+fakeToken+"\"\nexit 1\n"), 0o755)
	v := h.sess.RunVerifiedAll(context.Background(), []string{"sh check.sh"}, 1)
	if !v.Checked {
		t.Fatalf("the check did not run: %+v", v)
	}
	// check_tail AS THE WRAPPER RECEIVES IT. v.Tail itself is the model's copy and
	// is deliberately left alone — see TestTheModelIsShownTheCheckOutputItHasToFix
	// — so the boundary to assert on is the one the result object is built at.
	assertScrubbed(t, "check_tail", forPublication(v.Tail))
	if len(v.CheckLogs) == 0 {
		t.Fatal("no full check log was written")
	}
	for _, p := range v.CheckLogs {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		assertScrubbed(t, "the full check log "+filepath.Base(p), string(b))
	}

	// ── the report ────────────────────────────────────────────────────────────
	rep, err := buildReport([]string{h.orch.tracer.Path}, "t")
	if err != nil {
		t.Fatal(err)
	}
	html := filepath.Join(t.TempDir(), "r.html")
	if err := writeReport(html, rep); err != nil {
		t.Fatal(err)
	}
	hb, err := os.ReadFile(html)
	if err != nil {
		t.Fatal(err)
	}
	assertScrubbed(t, "the HTML report", string(hb))
	if !strings.HasSuffix(strings.TrimSpace(string(hb)), "</html>") {
		t.Fatal("the scrubbing writer must not have swallowed the end of the page")
	}

	// ── the audit, read back last so every event above is in it ───────────────
	ab, err := os.ReadFile(filepath.Join(h.orch.rec.dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	assertScrubbed(t, "the audit log", string(ab))
}

func assertScrubbed(t *testing.T, what, body string) {
	t.Helper()
	if strings.Contains(body, fakeToken) {
		t.Fatalf("%s carries a live token", what)
	}
	// The distinctive middle of it, in case something re-wrapped the ends.
	if strings.Contains(body, "9d41-this-is-not-a-real") {
		t.Fatalf("%s carries part of a live token", what)
	}
	if strings.Contains(body, "hunter2hunter2") {
		t.Fatalf("%s carries a live password", what)
	}
	if !strings.Contains(body, redactedMark) {
		t.Fatalf("%s should say where the redaction happened, not hide it silently:\n%s", what, ellipsize(body, 400))
	}
}

// A token with a quote or an ampersand in it is written ESCAPED in the
// artefacts — `\"` by the JSON encoder, `&#34;` by the report's own escaper —
// and a scrub that knew only the raw spelling would walk straight past it. This
// is the case that made the scrub search for every spelling rather than one.
func TestAnEscapedSecretIsStillFound(t *testing.T) {
	const quoted = `pw"&<secret-9d41-value>`
	armSecrets(t, map[string]string{"STAND_SECRET": quoted})

	// The transcript's spelling.
	data, _ := json.Marshal([]Message{{Role: "user", Content: "it said " + quoted}})
	got := string(redactBytes(data))
	if strings.Contains(got, `secret-9d41-value`) {
		t.Fatalf("the JSON-escaped spelling was missed: %s", got)
	}
	if !strings.Contains(got, redactedMark) {
		t.Fatalf("nothing was redacted: %s", got)
	}

	// The report's spelling.
	htmlish := "<pre>" + esc("it said "+quoted) + "</pre>"
	if strings.Contains(htmlish, "secret-9d41-value") {
		// Sanity: esc() escapes the punctuation but not the distinctive middle, so
		// the raw-only scrub really would have missed the surrounding bytes.
		if g := redactSecrets(htmlish); strings.Contains(g, "secret-9d41-value") {
			t.Fatalf("the HTML-escaped spelling was missed: %s", g)
		}
	}
}

// The streaming scrub holds back the tail of every write, because the report is
// rendered through a 64 KiB buffer and a token can straddle two of them. Written
// one byte at a time, which is the worst case the holdback exists for.
func TestASecretSplitAcrossTwoWritesIsStillFound(t *testing.T) {
	armSecrets(t, map[string]string{"SPLIT_TOKEN": fakeToken})
	var sink strings.Builder
	rw := newRedactWriter(&sink)
	body := "before " + fakeToken + " after"
	for i := 0; i < len(body); i++ {
		if _, err := rw.Write([]byte{body[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	got := sink.String()
	if strings.Contains(got, fakeToken) {
		t.Fatalf("a token split across writes got through: %q", got)
	}
	if got != "before "+redactedMark+" after" {
		t.Fatalf("the rest of the stream must arrive exactly once and in order: %q", got)
	}
}

// With no credential in the environment — every development machine, and every
// test but these — the scrub is a nil check and the bytes are not copied at all.
func TestWithNoSecretsTheScrubIsFree(t *testing.T) {
	// The environment this test runs in may well hold a real *_TOKEN of its own,
	// so the list is emptied directly rather than by unsetting variables nobody
	// here can enumerate: the Once is consumed with a no-op, which is what
	// "nothing to look for" looks like from the inside.
	secretsOnce = sync.Once{}
	secretVals = nil
	secretsOnce.Do(func() {})
	t.Cleanup(func() { secretsOnce = sync.Once{}; secretVals = nil })
	b := []byte("a transcript with nothing to hide in it")
	if got := redactBytes(b); &got[0] != &b[0] {
		t.Fatal("with no secrets redactBytes must return the same slice, not a copy")
	}
	var sink strings.Builder
	rw := newRedactWriter(&sink)
	rw.Write([]byte("straight through"))
	if sink.String() != "straight through" {
		t.Fatalf("the writer must be a pass-through with nothing to look for: %q", sink.String())
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
}

// The scrub runs on every write of a file that is rewritten after every turn,
// so its cost is a property of the program and not a detail. Measured here
// rather than assumed, and printed so the number in the commit message is one
// somebody can reproduce.
//
// The bound is deliberately loose — a test that asserts a wall-clock figure on
// somebody else's laptop is a test that fails for no reason — but it is a
// bound: a scrub that went quadratic in the number of secrets, or that scanned
// once per message instead of once per write, would blow through it.
func TestRedactionCostOnALongTranscript(t *testing.T) {
	armSecrets(t, map[string]string{
		"BSK_GW_TOKEN":   fakeToken,
		"JIRA_PASSWORD":  "hunter2hunter2",
		"GITLAB_API_KEY": "glpat-0000000000000000000",
	})
	// A long session: 2 000 messages of a kilobyte each is a transcript of about
	// 2 MB, which is what a hundred-turn run with file reads in it looks like.
	msgs := make([]Message, 0, 2000)
	chunk := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 23)
	for i := 0; i < 2000; i++ {
		msgs = append(msgs, Message{Role: "assistant", Content: fmt.Sprintf("%d %s", i, chunk)})
	}
	// One real secret in it, so the replacing path is measured and not only the
	// scanning one.
	msgs[1000].Content += " token=" + fakeToken
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	const runs = 10
	for i := 0; i < runs; i++ {
		if out := redactBytes(data); strings.Contains(string(out), fakeToken) {
			t.Fatal("the token survived")
		}
	}
	per := time.Since(start) / runs
	mb := float64(len(data)) / (1 << 20)
	t.Logf("redactBytes: %s per write of %.1f MB with %d spellings to look for (%.0f MB/s)",
		per.Round(time.Microsecond), mb, len(envSecrets()), mb/per.Seconds())
	if per > 2*time.Second {
		t.Fatalf("%s to scrub %.1f MB is not a cost a per-turn write can carry", per, mb)
	}
}

// The other half of P2-2, and the half that cost more than the leak: the scrub
// must not edit the text the MODEL is asked to fix.
//
// looksSecretName is a substring match on four very common English words, so
// `API_KEY_HEADER=Authorization` is an ordinary variable in the operator's own
// environment — and the check's own failure line reached the model as "missing
// [redacted] header", which it cannot act on and spends every attempt guessing
// at. The artefacts still have to be clean, so this asserts both halves at once:
// the ordinary word survives everywhere, the real token survives nowhere but the
// model's own copy.
func TestTheModelIsShownTheCheckOutputItHasToFix(t *testing.T) {
	armSecrets(t, map[string]string{
		"API_KEY_HEADER":    "Authorization",
		"TOKENIZER_PATH":    "/models/qwen3-coder-30b/tokenizer.json",
		"GITLAB_TOKEN_FILE": "/run/secrets/gitlab_token",
		"TOKEN_PREFIX":      "Bearer",
		"SSH_KEY_ALGO":      "ed25519",
		"BSK_API_KEY_ENV":   "BSK_GW_TOKEN",
		"BSK_GW_TOKEN":      fakeToken,
	})
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "I see it"} })
	h := newHarness(t, fs.URL, "native", true)
	jl, err := NewJail(h.root, []string{"sh"}, false)
	if err != nil {
		t.Fatal(err)
	}
	h.orch.jl = jl
	os.WriteFile(filepath.Join(h.root, "check.sh"), []byte(
		"echo 'FAIL: request rejected: missing Authorization header'\n"+
			"echo 'FAIL: cannot open /run/secrets/gitlab_token: permission denied'\n"+
			"echo 'FAIL: tokenizer /models/qwen3-coder-30b/tokenizer.json is for another model'\n"+
			"echo 'FAIL: key algorithm ed25519 refused, prefix Bearer'\n"+
			"echo 'FAIL: set BSK_GW_TOKEN; the one it had was "+fakeToken+"'\n"+
			"exit 1\n"), 0o755)

	v := h.sess.RunVerifiedAll(context.Background(), []string{"sh check.sh"}, 2)
	told := ""
	for _, m := range h.sess.Msgs {
		if m.Role == "user" && strings.Contains(m.Content, "The verifier ran") {
			told = m.Content
		}
	}
	if told == "" {
		t.Fatal("the model was never told the check failed")
	}
	for _, want := range []string{
		"missing Authorization header",
		"/run/secrets/gitlab_token",
		"/models/qwen3-coder-30b/tokenizer.json",
		"ed25519",
		"Bearer",
		"BSK_GW_TOKEN",
	} {
		if !strings.Contains(told, want) {
			t.Errorf("the model was not shown %q — it cannot fix what it is not told:\n%s", want, ellipsize(told, 800))
		}
	}
	// The real credential, in the same output, in the same attempt: the model sees
	// it (it is a tool result, and redact.go says a tool result is never rewritten)
	// and every FILE of the run is clean of it.
	if !strings.Contains(told, fakeToken) {
		t.Error("the model's copy is the raw output: see verify.go")
	}
	if len(v.CheckLogs) == 0 {
		t.Fatal("no full check log was written")
	}
	for _, p := range v.CheckLogs {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), fakeToken) {
			t.Errorf("%s carries a live token", filepath.Base(p))
		}
		if !strings.Contains(string(b), "missing Authorization header") {
			t.Errorf("%s lost the line that says which header: %s", filepath.Base(p), ellipsize(string(b), 400))
		}
	}
	if got := forPublication(v.Tail); strings.Contains(got, fakeToken) {
		t.Error("check_tail carries a live token")
	}
	if err := h.sess.saveTranscript(); err != nil {
		t.Fatal(err)
	}
	tr, _ := os.ReadFile(h.orch.rec.SessionPath())
	assertScrubbed(t, "the transcript", string(tr))
	if !strings.Contains(string(tr), "missing Authorization header") {
		t.Error("the transcript lost the line that says which header")
	}
}

// The value gate, as a table, because every row here is a variable somebody in
// this shop really has set. The name is not the question — all of these have
// token, key, secret or password in it — the question is whether the VALUE is a
// credential.
func TestLooksSecretValue(t *testing.T) {
	env := map[string]bool{"BSK_GW_TOKEN": true, "JIRA_PASSWORD": true}
	secret := map[string]string{
		fakeToken:                   "an issued token",
		"hunter2hunter2":            "a password",
		"glpat-0000000000000000":    "a GitLab PAT",
		`pw"&<secret-9d41-value>`:   "a password with punctuation in it",
		"correcthorsebatterystaple": "a passphrase: four words and no spaces is still a secret",
		"aGVsbG8gd29ybGQgdGhpcw==":  "base64",
	}
	for v, why := range secret {
		if !looksSecretValue(v, env) {
			t.Errorf("%q is %s and was let through", v, why)
		}
	}
	plain := map[string]string{
		"Authorization":                          "a header name (API_KEY_HEADER)",
		"Bearer":                                 "a scheme (TOKEN_PREFIX)",
		"strong":                                 "a policy (JIRA_PASSWORD_POLICY)",
		"ed25519":                                "an algorithm (SSH_KEY_ALGO)",
		"/models/qwen3-coder-30b/tokenizer.json": "a path (TOKENIZER_PATH)",
		"/run/secrets/gitlab_token":              "a path (GITLAB_TOKEN_FILE)",
		"~/.ssh/id_ed25519":                      "a path",
		"./secrets.env":                          "a relative path",
		"BSK_GW_TOKEN":                           "the NAME of another variable (BSK_API_KEY_ENV)",
		"SOME_OTHER_VAR":                         "a variable name, shouting",
		"true":                                   "too short to be anything",
	}
	for v, why := range plain {
		if looksSecretValue(v, env) {
			t.Errorf("%q is %s and would be redacted out of every artefact", v, why)
		}
	}
}

// The summary's FALLBACK path. The model's prose was scrubbed and the skeleton
// that replaces it when the model says nothing was not — so a gateway that went
// away mid-run put the check command line, credential and all, into the one file
// lca writes to be pasted into a merge request.
func TestTheSummaryFallbackIsScrubbed(t *testing.T) {
	armSecrets(t, map[string]string{"BSK_GW_TOKEN": fakeToken})
	// A gateway that answers the summary call with nothing: the clock ran out, or
	// it went away between the last turn and this one.
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: ""} })
	h := newHarness(t, fs.URL, "native", true)
	path := filepath.Join(t.TempDir(), "summary.md")
	h.orch.summary = path
	h.sess.stats.Turns, h.sess.stats.Replies = 2, 2

	cmd := "sh -c 'echo Authorization: Bearer " + fakeToken + "; exit 1'"
	f := factsFixture(statusFailed)
	f.check = cmd
	f.reason = "`" + cmd + "` still failed (exit 1) after 2 attempts"
	f.tail = "Authorization: Bearer " + fakeToken
	if got := h.orch.writeSummary(h.sess, f); got != path {
		t.Fatalf("writeSummary returned %q", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "## How it was verified") {
		t.Fatalf("this test has to exercise the skeleton, and the body is not one:\n%s", string(b))
	}
	assertScrubbed(t, "the summary's fallback body", string(b))
}

// check_cmd was the one foreign string in the result object nothing scrubbed,
// while `reason` on the next line — which quotes the same command — was.
func TestTheResultObjectsCheckCmdIsScrubbed(t *testing.T) {
	armSecrets(t, map[string]string{"BSK_GW_TOKEN": fakeToken})
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "done"} })
	h := newHarness(t, fs.URL, "native", true)
	cmd := "sh -c 'echo Authorization: Bearer " + fakeToken + "; exit 1'"
	exit := 1
	r := h.orch.resultOf(h.sess, Verdict{Checked: true, Exit: exit, Status: "failed", Attempts: 2,
		Tail: "Authorization: Bearer " + fakeToken}, cmd, 0, 0, time.Now(), nil)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	assertScrubbed(t, "the result object", string(b))
}

// run.log and state.json are the two sinks a workflow writes that the scrub did
// not reach, and run.log is the file the run itself tells the operator to read
// when a step fails — the first thing pasted into a ticket.
func TestAWorkflowRunLogAndStateAreScrubbed(t *testing.T) {
	armSecrets(t, map[string]string{"BSK_GW_TOKEN": fakeToken})
	dir := t.TempDir()
	l, err := newRunLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	// As execCheck tees it: whole chunks, live, as the child produces them.
	l.line("$ bash stage/stage.sh check")
	l.line("curl -H \"Authorization: Bearer " + fakeToken + "\" https://stand/")
	l.f.Close()
	b, err := os.ReadFile(filepath.Join(dir, "run.log"))
	if err != nil {
		t.Fatal(err)
	}
	assertScrubbed(t, "run.log", string(b))
}

// state.json is the other one, and the reason it is scrubbed field by field
// rather than over the bytes: a resume reads this document BACK.
func TestAWorkflowStateIsScrubbedWithoutBreakingAResume(t *testing.T) {
	armSecrets(t, map[string]string{"BSK_GW_TOKEN": fakeToken})
	dir := t.TempDir()
	st := &WorkflowState{Version: wfStateVersion, Run: "r1", Sum: "0123456789abcdef",
		Vars: map[string]string{"token": fakeToken},
		Steps: []StepState{{Name: "check", Out: "Authorization: Bearer " + fakeToken,
			Detail: "check `stage.sh` failed (exit 1)\nBearer " + fakeToken}}}
	if err := saveRunState(dir, st); err != nil {
		t.Fatal(err)
	}
	sb, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sb), fakeToken) {
		// vars is the one field that must keep it, so say which one leaked.
		var got WorkflowState
		json.Unmarshal(sb, &got)
		if got.Steps[0].Out != "" && strings.Contains(got.Steps[0].Out, fakeToken) {
			t.Error("state.json holds a live token in steps[].out")
		}
		if strings.Contains(got.Steps[0].Detail, fakeToken) {
			t.Error("state.json holds a live token in steps[].detail")
		}
	}
	// And vars survive byte for byte: a resume substitutes them into the next
	// step's command line and compares them with the operator's -var, so a
	// [redacted] there makes a resumed run refuse to start or run the wrong thing.
	var got WorkflowState
	if err := json.Unmarshal(sb, &got); err != nil {
		t.Fatal(err)
	}
	if got.Vars["token"] != fakeToken {
		t.Errorf("a resume reads vars back: %q", got.Vars["token"])
	}
	if got.Sum != st.Sum {
		t.Errorf("the workflow's sha256 was rewritten: %q", got.Sum)
	}
	// The live state is untouched, because the rest of the run expands
	// ${steps.check.out} from it.
	if st.Steps[0].Out != "Authorization: Bearer "+fakeToken {
		t.Error("the scrub edited the live state")
	}
}
