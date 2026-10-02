package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// evalFixture is the smallest matrix that can be run against a fake gateway: one
// task that passes and one that cannot. It returns the tasks directory.
func evalFixture(t *testing.T) string {
	t.Helper()
	tasks := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(filepath.Join(tasks, "answer", "fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tasks, "answer", "fixture", "README"), []byte("puzzle\n"), 0o644)
	os.WriteFile(filepath.Join(tasks, "answer", "task.yaml"),
		[]byte("prompt: |\n  Write the answer to answer.txt\ncheck_cmd: cat answer.txt\nrole: coder\nrepo: fixture\n"), 0o644)
	os.WriteFile(filepath.Join(tasks, "never.yaml"),
		[]byte("prompt: Do nothing useful\ncheck_cmd: ls missing.txt\nrole: coder\nrepo: answer/fixture\nverify_attempts: 1\n"), 0o644)
	return tasks
}

// evalRun runs the eval with args and reads the rows back out of results.jsonl.
func evalRun(t *testing.T, args ...string) (int, []EvalResult, string) {
	t.Helper()
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if strings.Contains(req.Body, `"role":"tool"`) {
			return fakeReply{content: "fixed"}
		}
		return fakeReply{calls: []ToolCall{call("w", "write", map[string]any{"path": "answer.txt", "content": "42\n"})}}
	})
	fs.models = allModels()
	home := t.TempDir()
	t.Setenv("HOME", home)
	rolesPath := filepath.Join(home, "roles.yaml")
	os.WriteFile(rolesPath, []byte(testRoles), 0o644)
	t.Setenv("LCA_ROLES", rolesPath)
	t.Setenv("LCA_GW_MAX_WAIT", "5")

	out := filepath.Join(t.TempDir(), "out")
	cfg := Config{Root: t.TempDir(), Dir: filepath.Join(home, ".lca"), BaseURL: fs.URL, Endpoints: []string{fs.URL},
		Model: "x", Temperature: 0.2, MaxSteps: 10, SubagentMax: 1, KeepSessions: 10}
	code := 0
	printed := captureStdout(t, func() {
		code = runEval(context.Background(), cfg, append([]string{"-out", out}, args...))
	})
	data, err := os.ReadFile(filepath.Join(out, "results.jsonl"))
	if err != nil {
		t.Fatalf("no results.jsonl: %v", err)
	}
	var rows []EvalResult
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var r EvalResult
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("results.jsonl line is not JSON: %v\n%s", err, line)
		}
		rows = append(rows, r)
	}
	return code, rows, printed
}

// P1-4: every line of results.jsonl carries lca_version, roles_hash AND the hash
// of the ROLE'S SYSTEM PROMPT. Without the three, two eval runs before and after
// a prompt edit are not comparable — which is the whole reason to run one twice.
func TestEveryEvalRowNamesTheBuildTheTeamAndThePrompt(t *testing.T) {
	_, rows, _ := evalRun(t, evalFixture(t))
	if len(rows) != 2 {
		t.Fatalf("two tasks, two rows, got %d", len(rows))
	}
	for _, r := range rows {
		if r.LCAVersion == "" {
			t.Fatalf("%s: lca_version is empty — the row does not say which binary produced it", r.Task)
		}
		if len(r.RolesHash) != 64 {
			t.Fatalf("%s: roles_hash is %q, not a sha256 of the loaded roles.yaml", r.Task, r.RolesHash)
		}
		if len(r.PromptHash) != 64 {
			t.Fatalf("%s: prompt_hash is %q, not a sha256 of the role's system prompt", r.Task, r.PromptHash)
		}
		if r.Repeat != 1 {
			t.Fatalf("%s: a plain run is run 1, got %d", r.Task, r.Repeat)
		}
	}
	// Both tasks ran the same role from the same file, so they agree — which is
	// what makes the hash usable as the identity of a RUN and not of a row.
	if rows[0].RolesHash != rows[1].RolesHash || rows[0].PromptHash != rows[1].PromptHash {
		t.Fatal("two tasks on one role in one team must report the same two hashes")
	}
}

// The prompt hash moves when the prompt moves, and that is the whole point: an
// edit to a role's prompt: changes neither the binary nor — if it were hashed
// from the role's own text only — necessarily anything a reader would notice.
func TestThePromptHashMovesWithThePrompt(t *testing.T) {
	a := promptHash("You are the LEAD.")
	b := promptHash("You are the LEAD. Be brief.")
	if a == b {
		t.Fatal("two different system prompts must not share a hash")
	}
	if a != promptHash("You are the LEAD.") {
		t.Fatal("the same prompt must hash the same, or no two runs are ever comparable")
	}
	if promptHash("") != "" {
		t.Fatal("no prompt must report nothing, not the hash of an empty string")
	}
	// And it is read from the session that ran, not re-assembled: compaction
	// rewrites Msgs[0], and a hash of a prompt no request ever used is worse than
	// none.
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", false)
	if got := systemPromptOf(h.sess); got != h.sess.Msgs[0].Content {
		t.Fatal("systemPromptOf must return the session's own first message")
	}
	if systemPromptOf(nil) != "" {
		t.Fatal("no session is no prompt")
	}
	h.sess.Msgs[0].Role = "user" // a session whose first message is not a prompt
	if systemPromptOf(h.sess) != "" {
		t.Fatal("only a system message is a system prompt")
	}
}

// The fingerprint has to survive the two facts about one RUN that the assembled
// prompt happens to name: the working directory and today's date. An eval gives
// every task a fresh workspace under <out>/<timestamp>/, so a hash that kept
// them would differ between every two tasks of one run and between every two
// runs of one task — never matching, and telling nobody anything.
func TestThePromptFingerprintIgnoresTheWorkspaceAndTheDate(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	a := newHarness(t, fs.URL, "native", false)
	b := newHarness(t, fs.URL, "native", false)
	if a.root == b.root {
		t.Fatal("the fixture must give the two sessions different trees")
	}
	if !strings.Contains(a.sess.Msgs[0].Content, a.root) {
		t.Fatal("the fixture assumes the prompt names the working directory")
	}
	if promptFingerprint(a.sess) != promptFingerprint(b.sess) {
		t.Fatal("the same role in two workspaces must fingerprint the same, or two eval runs are never comparable")
	}
	// The rules themselves, on the text, because the session's own answer is taken
	// once at build time and deliberately does not move after that (below).
	pa, pb := a.sess.Msgs[0].Content, b.sess.Msgs[0].Content
	// A date from another day must not move it either.
	pb = strings.Replace(pb, "- Today's date: "+time.Now().Format("2006-01-02"), "- Today's date: 1999-12-31", 1)
	if fingerprintOf(pa) != fingerprintOf(pb) {
		t.Fatal("the calendar must not be part of the fingerprint")
	}
	// But a real change to the prompt still moves it.
	if fingerprintOf(pa) == fingerprintOf(pb+"\n\n# One more instruction\nBe brief.") {
		t.Fatal("an added instruction must move the fingerprint — that is what it is for")
	}
}

// prompt_hash has to name what the run STARTED from, and Msgs[0] does not stay
// still: compaction and the project-instruction reload rewrite it in place. Its
// two callers read it at different moments — eval before the first turn,
// oneshot's resultOf after the last — so a run during which the model edited
// AGENTS.md reported two different prompts for one prompt, and an eval row and a
// one-shot row of the same configuration disagreed about the configuration.
func TestThePromptHashNamesWhatTheRunStartedFrom(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", false)
	started := promptFingerprint(h.sess)
	if started == "" {
		t.Fatal("a built session has a prompt and so has a fingerprint")
	}
	// What a mid-run AGENTS.md edit does, through the one function that does it.
	h.sess.Msgs[0].Content += "\n\n# Project instructions\nNever touch gw/.\n"
	if got := promptFingerprint(h.sess); got != started {
		t.Fatalf("the run's identity moved under it: %s then %s", shortHash(started), shortHash(got))
	}
	// And resultOf, the late caller, reports that same one.
	r := h.orch.resultOf(h.sess, Verdict{Status: "passed"}, "", 0, 0, time.Now(), nil)
	if r.PromptHash != started {
		t.Fatalf("the result object reports %s, the run started from %s", shortHash(r.PromptHash), shortHash(started))
	}
}

// P2-1, -repeat N: model variance is large and one run proves nothing, so every
// task runs N times and every run is its own row.
func TestEvalRepeatRunsEveryTaskNTimes(t *testing.T) {
	code, rows, printed := evalRun(t, "-repeat", "3", evalFixture(t))
	if code != 1 {
		t.Fatalf("one task passes and one cannot, so exit 1; got %d", code)
	}
	if len(rows) != 6 {
		t.Fatalf("two tasks × three runs is six rows, got %d", len(rows))
	}
	seen := map[string][]int{}
	for _, r := range rows {
		seen[r.Task] = append(seen[r.Task], r.Repeat)
	}
	for task, reps := range seen {
		if len(reps) != 3 {
			t.Fatalf("%s ran %d times, not three", task, len(reps))
		}
		got := map[int]bool{}
		for _, n := range reps {
			got[n] = true
		}
		for n := 1; n <= 3; n++ {
			if !got[n] {
				t.Fatalf("%s: no row for run %d — the repetitions are indistinguishable: %v", task, n, reps)
			}
		}
	}
	// Each repetition is its own tree, so one run cannot leave the next a file it
	// did not write.
	plain := strings.ToLower(stripANSI(printed))
	if !strings.Contains(plain, "spread") || !strings.Contains(plain, "first try") {
		t.Fatalf("with -repeat the per-task spread is the number worth printing:\n%s", plain)
	}
	if !strings.Contains(plain, "3 runs") {
		t.Fatalf("the header must say how many runs each task got:\n%s", plain)
	}
	// "3/3" and "0/3" and not three rows of status words: the spread is the one
	// number a reader can act on.
	if !strings.Contains(plain, "3/3") || !strings.Contains(plain, "0/3") {
		t.Fatalf("the spread must count the passes per task:\n%s", plain)
	}
}

// -j N: in parallel, each task in its own tree. The rows must all be there and
// each must be complete — a shared results file written from several goroutines
// is the obvious way to lose one.
func TestEvalInParallelLosesNothing(t *testing.T) {
	code, rows, _ := evalRun(t, "-repeat", "2", "-j", "4", evalFixture(t))
	if code != 1 {
		t.Fatalf("exit 1 (one task cannot pass), got %d", code)
	}
	if len(rows) != 4 {
		t.Fatalf("two tasks × two runs is four rows, got %d", len(rows))
	}
	passed := 0
	for _, r := range rows {
		if r.Status == "" || r.Trace == "" || r.LCAVersion == "" {
			t.Fatalf("a row written under -j is incomplete: %+v", r)
		}
		if r.Status == "passed" {
			passed++
		}
	}
	// The task that writes answer.txt passes in its own tree every time, which is
	// the part -j could break: two workers in one tree would race over the file.
	if passed != 2 {
		t.Fatalf("each repetition works in its own tree, so the passing task passes twice; got %d", passed)
	}
	// And every repetition got its own directory, or they overwrote each other.
	dirs := map[string]bool{}
	for _, r := range rows {
		dirs[filepath.Dir(r.Trace)] = true
	}
	if len(dirs) != 4 {
		t.Fatalf("four runs need four trees, got %d: %v", len(dirs), dirs)
	}
}

func TestEvalRefusesANonsenseRepeatOrParallelism(t *testing.T) {
	for _, args := range [][]string{{"-repeat", "0"}, {"-j", "0"}, {"-j", "-2"}} {
		cfg := Config{Root: t.TempDir(), Dir: t.TempDir()}
		code := runEval(context.Background(), cfg, append(args, t.TempDir()))
		if code != 2 {
			t.Fatalf("%v is a mistake in the call: exit 2, got %d", args, code)
		}
	}
}

// P2-1, -compare: the table that answers the question an eval exists for. Pass
// rate, passed on the first attempt, median tokens and median time, per task and
// in total — read from two finished runs and nothing else.
func TestEvalCompareTabulatesTwoRuns(t *testing.T) {
	dir := t.TempDir()
	before := filepath.Join(dir, "before", "results.jsonl")
	after := filepath.Join(dir, "after", "results.jsonl")
	os.MkdirAll(filepath.Dir(before), 0o755)
	os.MkdirAll(filepath.Dir(after), 0o755)

	// Before: parser passes once in three and never on the first attempt.
	writeEvalRows(t, before, []EvalResult{
		{Task: "parser", Repeat: 1, Status: "passed", Attempts: 2, PromptTokens: 100_000, CompletionTokens: 20_000, DurationMs: 120_000, LCAVersion: "aaaaaaaaaaaa", RolesHash: strings.Repeat("1", 64), PromptHash: strings.Repeat("2", 64)},
		{Task: "parser", Repeat: 2, Status: "failed", Attempts: 2, PromptTokens: 200_000, CompletionTokens: 40_000, DurationMs: 240_000, LCAVersion: "aaaaaaaaaaaa", RolesHash: strings.Repeat("1", 64), PromptHash: strings.Repeat("2", 64)},
		{Task: "parser", Repeat: 3, Status: "failed", Attempts: 2, PromptTokens: 150_000, CompletionTokens: 30_000, DurationMs: 180_000, LCAVersion: "aaaaaaaaaaaa", RolesHash: strings.Repeat("1", 64), PromptHash: strings.Repeat("2", 64)},
	})
	// After the prompt edit: three of three, two of them first try, and cheaper.
	writeEvalRows(t, after, []EvalResult{
		{Task: "parser", Repeat: 1, Status: "passed", Attempts: 1, PromptTokens: 50_000, CompletionTokens: 10_000, DurationMs: 60_000, LCAVersion: "bbbbbbbbbbbb", RolesHash: strings.Repeat("1", 64), PromptHash: strings.Repeat("3", 64)},
		{Task: "parser", Repeat: 2, Status: "passed", Attempts: 1, PromptTokens: 60_000, CompletionTokens: 12_000, DurationMs: 70_000, LCAVersion: "bbbbbbbbbbbb", RolesHash: strings.Repeat("1", 64), PromptHash: strings.Repeat("3", 64)},
		{Task: "parser", Repeat: 3, Status: "passed", Attempts: 2, PromptTokens: 70_000, CompletionTokens: 14_000, DurationMs: 80_000, LCAVersion: "bbbbbbbbbbbb", RolesHash: strings.Repeat("1", 64), PromptHash: strings.Repeat("3", 64)},
	})

	code := 0
	printed := captureStdout(t, func() { code = runEval(context.Background(), Config{}, []string{"-compare", before, after}) })
	if code != 0 {
		t.Fatalf("a comparison that could be made is exit 0, got %d\n%s", code, printed)
	}
	plain := stripANSI(printed)
	for _, want := range []string{
		"parser",  // per task
		"total",   // and in total
		"1/3",     // before: one of three passed
		"0/3",     // before: none on the first attempt
		"3/3",     // after: all three
		"2/3",     // after: two on the first attempt
		"before",  // each file named by the part of its path that differs
		"after",   //
		"aaaaaaa", // and the identity of each run, so a reader can see what moved
		"bbbbbbb",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("the comparison does not show %q:\n%s", want, plain)
		}
	}
	// The medians, not the means: before's tokens are 120k, 240k, 180k — median
	// 180k, mean 180k; after's are 60k, 72k, 84k — median 72k. The 72k is the
	// number that must appear.
	if !strings.Contains(plain, "72.0k") && !strings.Contains(plain, "72k") {
		t.Fatalf("the median token figure (72k) is missing:\n%s", plain)
	}

	// One file is not a comparison, and a file that is not there is a mistake in
	// the call rather than an empty table.
	if code := runEval(context.Background(), Config{}, []string{"-compare", before}); code != 2 {
		t.Fatalf("one file is exit 2, got %d", code)
	}
	if code := runEval(context.Background(), Config{}, []string{"-compare", before, filepath.Join(dir, "nope.jsonl")}); code != 2 {
		t.Fatalf("a missing file is exit 2, got %d", code)
	}
}

// A task present in one run and missing from the other is a GAP in the table
// and not a silently dropped row: that is the shape of "we added a task", and a
// total computed over different task sets is a total nobody should compare.
func TestEvalCompareShowsATaskOnlyOneRunHas(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a", "results.jsonl"), filepath.Join(dir, "b", "results.jsonl")
	os.MkdirAll(filepath.Dir(a), 0o755)
	os.MkdirAll(filepath.Dir(b), 0o755)
	writeEvalRows(t, a, []EvalResult{{Task: "old", Repeat: 1, Status: "passed", Attempts: 1}})
	writeEvalRows(t, b, []EvalResult{
		{Task: "old", Repeat: 1, Status: "passed", Attempts: 1},
		{Task: "new", Repeat: 1, Status: "failed", Attempts: 1},
	})
	printed := stripANSI(captureStdout(t, func() { runEval(context.Background(), Config{}, []string{"-compare", a, b}) }))
	if !strings.Contains(printed, "new") {
		t.Fatalf("the task only one run has must appear:\n%s", printed)
	}
	if !strings.Contains(printed, gNil) {
		t.Fatalf("the run that does not have it must show a gap, not a zero:\n%s", printed)
	}
}

// The median is the middle value, and of an even count the mean of the two
// middle ones. Asserted directly, because every number in the comparison table
// rests on it.
func TestMedianOfEvalNumbers(t *testing.T) {
	for _, c := range []struct {
		in   []int
		want int
	}{
		{nil, 0},
		{[]int{5}, 5},
		{[]int{3, 1, 2}, 2},
		{[]int{4, 1, 3, 2}, 2}, // (2+3)/2
		{[]int{1, 1, 1, 1_000_000}, 1},
	} {
		if got := medianInt(c.in); got != c.want {
			t.Fatalf("medianInt(%v) = %d, want %d", c.in, got, c.want)
		}
	}
	// And one outlier does not move it, which is why the table reports it.
	rows := []EvalResult{
		{Status: "passed", Attempts: 1, PromptTokens: 1000, DurationMs: 1000},
		{Status: "passed", Attempts: 1, PromptTokens: 1000, DurationMs: 1000},
		{Status: "failed", Attempts: 3, PromptTokens: 900_000, DurationMs: 900_000},
	}
	s := summarizeEvalRows(rows)
	if s.n != 3 || s.passed != 2 || s.first != 2 {
		t.Fatalf("passed and first-try: %+v", s)
	}
	if s.medTokens != 1000 || s.medMs != 1000 {
		t.Fatalf("one run that hit its ceiling must not move the median: %+v", s)
	}
}

// A row written before -repeat existed has no repeat field, and a comparison
// that dropped those rows could not compare anything written last month.
func TestEvalCompareReadsRowsFromBeforeTheseFieldsExisted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.jsonl")
	os.WriteFile(p, []byte(`{"task":"parser","status":"passed","attempts":1,"prompt_tokens":10,"duration_ms":5}`+"\n"+
		`{"this is":"not a result"}`+"\n"+
		`{broken`+"\n"), 0o644)
	rows, err := readEvalResults(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("one readable row of three lines, got %d", len(rows))
	}
	if rows[0].Repeat != 1 {
		t.Fatalf("a row with no repeat field is run 1, got %d", rows[0].Repeat)
	}
}

func writeEvalRows(t *testing.T, path string, rows []EvalResult) {
	t.Helper()
	var b bytes.Buffer
	for _, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s\n", line)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// `-j N` and an mcp: block cannot share a process, and this is the refusal.
//
// The MCP tool table is process-wide, every orchestrator registers into it, and
// the sessions that then read each tool's server binding do so with no lock. So
// N orchestrators in one process is a data race at best; what it is in practice
// is every task's Jira call going over one task's connection, and the first task
// to finish cancelling it under the others. A readable refusal before anything
// runs is worth more than a matrix of rows that blame the server.
func TestEvalRefusesParallelismWhenMCPIsConfigured(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".lca"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ".lca", "config.json"), []byte(
		`{"mcp": {"allow_hosts": ["jira.example:443"], "servers": {"jira": {`+
			`"transport":"http","url":"https://jira.example/mcp","expose":["issue_get"]}}}}`), 0o600)
	cfg := Config{Root: root, Dir: t.TempDir()}

	var code int
	stderr := captureStderr(t, func() {
		code = runEval(context.Background(), cfg, []string{"-j", "2", t.TempDir()})
	})
	if code != 2 {
		t.Fatalf("a call that cannot be honoured is exit 2, got %d", code)
	}
	for _, want := range []string{"jira", "-j 1"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal must name %q so the operator knows what to do:\n%s", want, stderr)
		}
	}
	// -j 1 is the supported way to run that matrix, so it must get past this.
	stderr = captureStderr(t, func() {
		runEval(context.Background(), cfg, []string{"-j", "1", t.TempDir()})
	})
	if strings.Contains(stderr, "shared by the whole process") {
		t.Errorf("-j 1 is one orchestrator at a time and has nothing to refuse:\n%s", stderr)
	}
	if mcpRefuseSecondRegistration.Load() {
		t.Error("the flag outlived the run that set it")
	}
}

// The cost medians are over the rows that RAN. A task that died at `workspace:`
// writes a row with status error, 0 tokens and the 80 ms it took to find out, so
// a file with three of those and two expensive passes reported "0 tokens, 80ms"
// — and a reader comparing a prompt edit concluded the new one was free and
// instant when the two tasks that actually ran cost 2.3x the tokens.
func TestEvalCompareIgnoresRowsWhereNothingRan(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a", "results.jsonl"), filepath.Join(dir, "b", "results.jsonl")
	os.MkdirAll(filepath.Dir(a), 0o755)
	os.MkdirAll(filepath.Dir(b), 0o755)
	var rowsA, rowsB []EvalResult
	for i := 1; i <= 5; i++ {
		rowsA = append(rowsA, EvalResult{Task: fmt.Sprintf("t%d", i), Repeat: 1, Status: "passed", Attempts: 1,
			PromptTokens: 400_000, CompletionTokens: 20_000, DurationMs: 120_000})
	}
	for i := 1; i <= 3; i++ {
		// Never started: no workspace, no gateway call, no cost.
		rowsB = append(rowsB, EvalResult{Task: fmt.Sprintf("t%d", i), Repeat: 1, Status: "error",
			Error: "workspace: no such fixture", DurationMs: 80})
	}
	for i := 4; i <= 5; i++ {
		rowsB = append(rowsB, EvalResult{Task: fmt.Sprintf("t%d", i), Repeat: 1, Status: "passed", Attempts: 1,
			PromptTokens: 900_000, CompletionTokens: 50_000, DurationMs: 300_000})
	}
	writeEvalRows(t, a, rowsA)
	writeEvalRows(t, b, rowsB)

	printed := stripANSI(captureStdout(t, func() {
		runEval(context.Background(), Config{}, []string{"-compare", a, b})
	}))
	// b's two real runs cost 950k each and took 300 s. The old answer was 0 / 80ms.
	if !strings.Contains(printed, "950k") {
		t.Fatalf("the median of the rows that ran (950k) is missing:\n%s", printed)
	}
	if !strings.Contains(printed, "300.0s") && !strings.Contains(printed, "5.0m") {
		t.Fatalf("the median time of the rows that ran is missing:\n%s", printed)
	}
	if strings.Contains(printed, "80ms") {
		t.Fatalf("80ms is how long it took to fail to start, not how long a run took:\n%s", printed)
	}
	// And the reader is told the cost columns describe fewer rows than the pass
	// rate beside them.
	if !strings.Contains(printed, "never ran") {
		t.Fatalf("nothing says rows were left out of the medians:\n%s", printed)
	}
	// A cell where NOTHING ran shows a gap and not a zero.
	c := filepath.Join(dir, "c", "results.jsonl")
	os.MkdirAll(filepath.Dir(c), 0o755)
	writeEvalRows(t, c, []EvalResult{{Task: "t1", Repeat: 1, Status: "error", DurationMs: 80}})
	printed = stripANSI(captureStdout(t, func() {
		runEval(context.Background(), Config{}, []string{"-compare", a, c})
	}))
	if !strings.Contains(printed, gNil) {
		t.Fatalf("a cell with no run in it must show %q:\n%s", gNil, printed)
	}
}

// A trace.jsonl is not a results.jsonl. Its own task record carries task,
// status, attempts and duration_ms, so every field the reader asked for was
// there — and `lca eval -compare out/trace.jsonl other/results.jsonl` scored it
// as a one-row run with 0 tokens, with the only signal being the `—` that this
// reader documents as meaning "written before those fields existed".
func TestEvalCompareRefusesATrace(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "run", "trace.jsonl")
	real := filepath.Join(dir, "other", "results.jsonl")
	os.MkdirAll(filepath.Dir(trace), 0o755)
	os.MkdirAll(filepath.Dir(real), 0o755)
	tr, err := NewTracer(trace)
	if err != nil {
		t.Fatal(err)
	}
	tr.write(TaskRecord{Type: "task", TS: nowTS(), Session: "s1", RootSession: "s1", Role: "coder",
		Task: "parser", Status: "passed", Attempts: 1, DurationMs: 5000})
	tr.Close()
	writeEvalRows(t, real, []EvalResult{{Task: "parser", Repeat: 1, Status: "passed", Attempts: 1,
		PromptTokens: 400_000, CompletionTokens: 20_000, DurationMs: 120_000}})

	var code int
	printed := stripANSI(captureStdout(t, func() {
		code = runEval(context.Background(), Config{}, []string{"-compare", trace, real})
	}))
	if code != 2 {
		t.Fatalf("a file that is not a results.jsonl is a mistake in the call: exit 2, got %d\n%s", code, printed)
	}
}
