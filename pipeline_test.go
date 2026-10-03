package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// The tests for lca as the executor of somebody's pipeline. Three guarantees are
// under test here, and each of them is a thing a wrapper cannot be written
// without:
//
//	P0-1  the task arrives from a file or stdin, byte for byte, and not in argv
//	P0-2  the result is one JSON object and an exit code from a fixed table
//	P0-4  no question ever waits for a human
//
// The last one is why several of these tests run the real binary in a child
// process with stdin on /dev/null, and why every one of them has a deadline: a
// hang is the failure mode, and a test that hangs to report a hang reports
// nothing.

// ── the binary, built once ──────────────────────────────────────────────────

var (
	e2eOnce sync.Once
	e2eBin  string
	e2eDir  string
	e2eErr  error
	// origEnv is the environment as the test process started, kept because
	// t.Setenv (newHarness sets HOME) changes the real process environment and a
	// `go build` with HOME pointing at a scratch directory loses its build cache.
	origEnv []string
)

func TestMain(m *testing.M) {
	origEnv = os.Environ()
	code := m.Run()
	if e2eDir != "" {
		os.RemoveAll(e2eDir)
	}
	os.Exit(code)
}

// lcaBinary builds the real program once for the whole test run. The guarantees
// about `ps`, about what is on stdout and about an exit code are properties of a
// PROCESS, and nothing short of one can be made to stand in for it.
func lcaBinary(t *testing.T) string {
	t.Helper()
	e2eOnce.Do(func() {
		e2eDir, e2eErr = os.MkdirTemp("", "lca-e2e")
		if e2eErr != nil {
			return
		}
		e2eBin = filepath.Join(e2eDir, "lca")
		cmd := exec.Command("go", "build", "-o", e2eBin, ".")
		cmd.Env = origEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			e2eErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if e2eErr != nil {
		t.Fatal(e2eErr)
	}
	return e2eBin
}

// cli is one configured invocation of the built binary: its own project tree,
// its own state directory, its own HOME, and a gateway it can reach.
type cli struct {
	t                      *testing.T
	bin, root, state, home string
	gw                     string // base URL of the fake gateway, with no /v1
	extra                  []string
}

func newCLI(t *testing.T, gw string) *cli {
	t.Helper()
	bin := lcaBinary(t) // before any t.Setenv: the build needs the real HOME
	base := t.TempDir()
	c := &cli{t: t, bin: bin, gw: gw,
		root: filepath.Join(base, "proj"), state: filepath.Join(base, "state"), home: filepath.Join(base, "home")}
	for _, d := range []string{c.root, c.state, c.home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (c *cli) env() []string {
	env := []string{
		"HOME=" + c.home,
		"PATH=" + os.Getenv("PATH"),
		"LCA_DIR=" + c.state,
		"LCA_ROOT=" + c.root,
		"LCA_BASE_URL=" + c.gw + "/v1",
		"LCA_TOOLS=native", // the fixture speaks native tool calls
		"LCA_NO_CLEAR=1",
		"LCA_SETUP=off",
		"TERM=dumb",
	}
	return append(env, c.extra...)
}

// start launches the binary without waiting for it.
func (c *cli) start(stdin io.Reader, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	c.t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Env = c.env()
	cmd.Dir = c.root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin == nil {
		f, err := os.Open(os.DevNull)
		if err != nil {
			c.t.Fatal(err)
		}
		c.t.Cleanup(func() { f.Close() })
		cmd.Stdin = f
	} else {
		cmd.Stdin = stdin
	}
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	return cmd, &out, &errb
}

// wait waits with a deadline and returns the exit code. A run that has to be
// killed is reported as a failure here and not as a 20-minute test.
func (c *cli) wait(cmd *exec.Cmd, d time.Duration) (int, error) {
	c.t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 0, err
	case <-time.After(d):
		cmd.Process.Kill()
		<-done
		return -1, fmt.Errorf("lca did not finish within %s — it waited for something", d)
	}
}

// run is start+wait: stdout, stderr and the exit code.
func (c *cli) run(stdin io.Reader, args ...string) (string, string, int) {
	c.t.Helper()
	cmd, out, errb := c.start(stdin, args...)
	code, err := c.wait(cmd, 60*time.Second)
	if err != nil {
		c.t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errb.String())
	}
	return out.String(), errb.String(), code
}

func (c *cli) auditLog() string {
	b, err := os.ReadFile(filepath.Join(c.state, "audit.jsonl"))
	if err != nil {
		c.t.Fatalf("no audit log: %v", err)
	}
	return string(b)
}

// decodeOneObject is the acceptance criterion "nothing on stdout but the JSON",
// enforced the way `python3 -m json.tool` would be: decode one value, then
// require the end of the stream.
func decodeOneObject(t *testing.T, stdout string) runResult {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var r runResult
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\nstdout was:\n%q", err, stdout)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("stdout carries more than the result object (token %v) — it must be the only thing there:\n%q", err, stdout)
	}
	return r
}

// ── P0-1: the task from a file or stdin ─────────────────────────────────────

// ticketMarker is a string that exists nowhere else, so finding it in `ps`
// output means it came from this ticket and from nothing else.
const ticketMarker = "МАРКЕР-7f3a-кириллица"

// bigTicket is a ticket of the shape the acceptance criterion names: over 50 KB
// of markdown with double quotes, $VAR, backticks and Cyrillic in it. Every one
// of those is a character that argv, a shell, or a JSON encoder could mangle.
func bigTicket() string {
	var b strings.Builder
	b.WriteString("# OPS-412 — сборка падает на `$PATH`\n\n" + ticketMarker + "\n\n")
	for b.Len() < 50<<10 {
		b.WriteString("- шаг: `echo \"$VAR\" | grep -q 'ok'` — \"кавычки\", $HOME, ₽42, \\n\n")
		b.WriteString("  > не доверяй `rm -rf $HOME/*`; см. tabs:\t|\tи emoji: ✓\n")
	}
	b.WriteString("\nконец тикета\n")
	return b.String()
}

func TestResolvePromptSources(t *testing.T) {
	dir := t.TempDir()
	ticket := filepath.Join(dir, "t.md")
	body := "line one\n\n  line two with `$VAR` and \"quotes\"\n"
	if err := os.WriteFile(ticket, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.md")
	os.WriteFile(empty, []byte("   \n\t\n"), 0o600)

	t.Run("positional arguments still work", func(t *testing.T) {
		got, err := resolvePrompt([]string{"fix", "the", "thing"}, "", nil)
		if err != nil || got != "fix the thing" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("a file arrives untouched", func(t *testing.T) {
		got, err := resolvePrompt(nil, ticket, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != body {
			// Trailing newline included: "byte for byte" has no exceptions, or a
			// quoted ticket in a transcript cannot be compared with the ticket.
			t.Fatalf("the file was modified on the way in:\n got %q\nwant %q", got, body)
		}
	})
	t.Run("- reads stdin", func(t *testing.T) {
		got, err := resolvePrompt(nil, "-", strings.NewReader(body))
		if err != nil || got != body {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("both is a usage error, never a concatenation", func(t *testing.T) {
		got, err := resolvePrompt([]string{"also", "this"}, ticket, nil)
		var ue *usageErr
		if !errors.As(err, &ue) {
			t.Fatalf("want a usage error, got %q, %v", got, err)
		}
		if strings.Contains(got, "also") {
			t.Fatal("the two were glued together")
		}
	})
	t.Run("an empty file is a usage error", func(t *testing.T) {
		var ue *usageErr
		if _, err := resolvePrompt(nil, empty, nil); !errors.As(err, &ue) {
			t.Fatalf("want a usage error, got %v", err)
		}
	})
	t.Run("a missing file is a usage error", func(t *testing.T) {
		var ue *usageErr
		if _, err := resolvePrompt(nil, filepath.Join(dir, "nope.md"), nil); !errors.As(err, &ue) {
			t.Fatalf("want a usage error, got %v", err)
		}
	})
}

// The acceptance criterion for P0-1, end to end: a 50 KB ticket with quotes,
// $VAR, backticks and Cyrillic in it reaches the MODEL byte for byte, and is
// visible in the transcript. Both halves are checked — the request the gateway
// received, and the file on disk — because a transcript that agrees with a
// request that was already wrong proves nothing.
func TestFiftyKilobyteTicketReachesTheModelByteForByte(t *testing.T) {
	ticket := bigTicket()
	if len(ticket) < 50<<10 {
		t.Fatalf("the fixture is only %d bytes", len(ticket))
	}
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "done"}
	})
	c := newCLI(t, fs.URL)
	path := filepath.Join(c.root, "ticket.md")
	if err := os.WriteFile(path, []byte(ticket), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := c.run(nil, "-y", "-json", "-prompt-file", path)
	if code != exitOK && code != exitFailed {
		t.Fatalf("exit %d\nstderr:\n%s", code, stderr)
	}
	r := decodeOneObject(t, stdout)

	rs := fs.reqs()
	if len(rs) == 0 {
		t.Fatalf("the gateway was never called\nstderr:\n%s", stderr)
	}
	var sent string
	for _, m := range rs[0].Messages {
		if m["role"] == "user" {
			sent, _ = m["content"].(string)
		}
	}
	if sent != ticket {
		t.Fatalf("the model was sent %d bytes, the ticket is %d; first difference at %d",
			len(sent), len(ticket), firstDiff(sent, ticket))
	}

	msgs, err := loadSession(r.Transcript)
	if err != nil {
		t.Fatalf("the result points at a transcript that is not there (%s): %v", r.Transcript, err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "user" && m.Content == ticket {
			found = true
		}
	}
	if !found {
		t.Fatalf("the ticket is not in the transcript byte for byte (%s)", r.Transcript)
	}
}

func firstDiff(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// The other half of P0-1: the ticket must not be in argv, because argv is world
// readable through `ps` and a ticket is somebody's internal bug report.
//
// The test proves both directions. A ticket passed POSITIONALLY is found in the
// process's command line — which is what makes the negative half meaningful — and
// the same ticket passed with -prompt-file is not.
func TestAPromptFileKeepsTheTicketOutOfPs(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps here")
	}
	// A gateway that holds the request open, so the child is certainly alive and
	// certainly past argument parsing when ps runs. One per subtest, with its own
	// channel: a shared one would be written by the test goroutine while a handler
	// goroutine reads it.
	heldGateway := func(t *testing.T) (*fakeServer, func()) {
		release := make(chan struct{})
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			<-release
			return fakeReply{content: "done"}
		})
		return fs, sync.OnceFunc(func() { close(release) })
	}
	psOf := func(pid int) string {
		out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
		if err != nil {
			t.Fatalf("ps: %v (%s)", err, out)
		}
		return string(out)
	}

	short := "fix OPS-412: " + ticketMarker + " — `$VAR` in \"quotes\""
	t.Run("a positional task is visible, which is the problem", func(t *testing.T) {
		fs, release := heldGateway(t)
		defer release()
		c := newCLI(t, fs.URL)
		cmd, _, _ := c.start(nil, "-y", "-json", short)
		ps := waitForPs(t, cmd.Process.Pid, psOf, ticketMarker)
		if !strings.Contains(ps, ticketMarker) {
			t.Fatalf("ps never showed the positional task, so this test cannot see argv at all:\n%s", ps)
		}
		release()
		c.wait(cmd, 30*time.Second)
	})

	t.Run("a prompt file is not", func(t *testing.T) {
		fs, release := heldGateway(t)
		defer release()
		c := newCLI(t, fs.URL)
		path := filepath.Join(c.root, "ticket.md")
		if err := os.WriteFile(path, []byte(bigTicket()), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd, _, _ := c.start(nil, "-y", "-json", "-prompt-file", path)
		ps := waitForPs(t, cmd.Process.Pid, psOf, "-prompt-file")
		if !strings.Contains(ps, "-prompt-file") {
			t.Fatalf("ps did not show the running lca at all:\n%s", ps)
		}
		if strings.Contains(ps, ticketMarker) {
			t.Fatalf("the ticket is in the command line:\n%s", ps)
		}
		release()
		c.wait(cmd, 30*time.Second)
	})
}

// waitForPs polls until the child's command line is readable and carries want,
// so neither half of the test above depends on scheduling.
func waitForPs(t *testing.T, pid int, psOf func(int) string, want string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var ps string
	for time.Now().Before(deadline) {
		ps = psOf(pid)
		if strings.Contains(ps, want) {
			return ps
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ps
}

// ── P0-2: the machine-readable result and the exit-code table ───────────────

func TestExitCodeTable(t *testing.T) {
	for _, tc := range []struct {
		status            string
		check             string
		machine, wantCode int
	}{
		// The table as the requirements document fixes it.
		{status: statusPassed, check: "ls", machine: 1, wantCode: 0},
		{status: statusFailed, check: "ls", machine: 1, wantCode: 1},
		{status: statusUnverified, machine: 1, wantCode: 1},
		{status: statusConfig, machine: 1, wantCode: 2},
		{status: statusInfra, check: "ls", machine: 1, wantCode: 3},
		{status: statusBudget, check: "ls", machine: 1, wantCode: 4},
		{status: statusCancelled, check: "ls", machine: 1, wantCode: 130},
		// Without -json and without a check, `lca "task"` keeps the contract it
		// always had: 0 when the model answered. Everything that is actually broken
		// is still reported to everyone.
		{status: statusUnverified, wantCode: 0},
		{status: statusBudget, wantCode: 0},
		{status: statusInfra, wantCode: 3},
		{status: statusCancelled, wantCode: 130},
		{status: statusFailed, check: "ls", wantCode: 1},
		{status: statusBudget, check: "ls", wantCode: 4},
	} {
		r := runResult{Status: tc.status, CheckCmd: tc.check}
		if got := r.exitCode(tc.machine == 1); got != tc.wantCode {
			t.Errorf("%s (check=%q, json=%v) → exit %d, want %d", tc.status, tc.check, tc.machine == 1, got, tc.wantCode)
		}
	}
}

// The classification is the whole reason the flag exists: "the model did not
// manage it" goes to a person, "the infrastructure fell over" goes back in the
// queue, and a bad key or a wrong URL goes to whoever configured it. Getting any
// of the three wrong is expensive, and none of them is visible from exit 1.
func TestClassifyRunErr(t *testing.T) {
	refused := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a refused connection", fmt.Errorf("request failed: %w", refused), statusInfra},
		{"dns", &net.DNSError{Name: "gw.corp", Err: "no such host"}, statusInfra},
		{"a cut stream", fmt.Errorf("stream ended: %w", errCutStream), statusInfra},
		{"a gateway 503", &APIError{Status: 503, Body: "no backend"}, statusInfra},
		{"a gateway 429", &APIError{Status: 429, Body: "slow down"}, statusInfra},
		{"an overloaded gateway", &APIError{Status: 400, Overload: true}, statusInfra},
		{"a drained gateway", &APIError{Status: 400, State: "drained"}, statusInfra},
		{"a member that is away", &infraErr{what: "member stage", err: errors.New("ssh: host down")}, statusInfra},
		{"a rejected key", &APIError{Status: 401, Body: "bad key"}, statusConfig},
		{"the wrong base path", &APIError{Status: 404, Body: "not found"}, statusConfig},
		{"a check the sandbox refuses", usageErrf("check rejected by the sandbox: no"), statusConfig},
		{"ctrl-c", context.Canceled, statusCancelled},
		{"a deadline", context.DeadlineExceeded, statusBudget},
		{"anything else", errors.New("the model emitted nonsense"), statusFailed},
	} {
		got, reason := classifyRunErr(tc.err)
		if got != tc.want {
			t.Errorf("%s → %s, want %s", tc.name, got, tc.want)
		}
		if reason == "" {
			t.Errorf("%s → %s with no reason; the wrapper puts this in a Jira comment", tc.name, got)
		}
	}
	if got, _ := classifyRunErr(nil); got != statusFailed {
		t.Errorf("no error at all → %s", got)
	}
}

// oneShotResult runs the real one-shot against a fake gateway and returns the
// object it wrote and the code it would have exited with.
func oneShotResult(t *testing.T, h *harness, prompt, check string) (runResult, int, string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "result")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	var code int
	// Both streams are held: under -json main() points os.Stdout at stderr before
	// anything runs, and the result goes to the descriptor stdout was. In-process
	// the equivalent is to let neither reach the test's own output.
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			code = oneShot(h.orch, h.sess, prompt, check, true, out, nil)
		})
	})
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return decodeOneObject(t, string(b)), code, stderr
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = wr
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		io.Copy(&b, rd)
		done <- b.String()
	}()
	fn()
	os.Stderr = saved
	wr.Close()
	out := <-done
	rd.Close()
	return out
}

func TestOneShotResultStatuses(t *testing.T) {
	t.Run("a green check is passed", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
		h := newHarness(t, fs.URL, "native", true)
		r, code, stderr := oneShotResult(t, h, "do it", "ls")
		if r.Status != statusPassed || code != exitOK {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
		if r.CheckExit == nil || *r.CheckExit != 0 {
			t.Fatalf("check_exit = %v, want 0", r.CheckExit)
		}
		if r.Attempts != 1 || r.CheckCmd != "ls" || r.Turns == 0 {
			t.Fatalf("%+v", r)
		}
		if r.LCAVersion == "" {
			t.Fatal("lca_version is empty: two runs cannot be compared")
		}
		if r.Transcript == "" || r.Trace == "" {
			t.Fatalf("the result must say where to look: %+v", r)
		}
		// Every human line goes to stderr, including the verdict.
		if !strings.Contains(stripANSI(stderr), "passed") {
			t.Fatalf("the verdict is not on stderr:\n%s", stderr)
		}
	})

	t.Run("a red check after every attempt is failed", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
		h := newHarness(t, fs.URL, "native", true)
		r, code, _ := oneShotResult(t, h, "do it", "ls /no-such-path-here-7f3a")
		if r.Status != statusFailed || code != exitFailed {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
		if r.Attempts != h.orch.verifyAttempts() {
			t.Fatalf("attempts = %d, want every one of the %d", r.Attempts, h.orch.verifyAttempts())
		}
		if r.CheckExit == nil || *r.CheckExit == 0 {
			t.Fatalf("check_exit = %v, want the check's own non-zero exit", r.CheckExit)
		}
		if r.CheckTail == "" {
			t.Fatal("check_tail is empty: the wrapper has nothing to put in the ticket")
		}
	})

	t.Run("an unreachable gateway is infra_error, not failed", func(t *testing.T) {
		h := newHarness(t, "http://127.0.0.1:1/v1", "native", true)
		r, code, _ := oneShotResult(t, h, "do it", "ls")
		if r.Status != statusInfra || code != exitInfra {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
		if r.CheckExit != nil {
			t.Fatalf("check_exit = %v: the check never ran, and 0 there would read as green", *r.CheckExit)
		}
	})

	t.Run("running out of steps is budget_exceeded", func(t *testing.T) {
		// A model that never stops calling tools — the loop the step cap exists for.
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			return fakeReply{content: "looking", calls: []ToolCall{call(fmt.Sprintf("c%d", n), "list_dir", map[string]any{"path": "."})}}
		})
		h := newHarness(t, fs.URL, "native", true)
		h.orch.cfg.MaxSteps = 3
		r, code, _ := oneShotResult(t, h, "go in circles", "")
		if r.Status != statusBudget || code != exitBudget {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
		if !strings.Contains(r.Reason, "step") {
			t.Fatalf("the reason must say what ran out: %q", r.Reason)
		}
	})

	t.Run("no check at all is unverified", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
		h := newHarness(t, fs.URL, "native", true)
		r, code, _ := oneShotResult(t, h, "say something", "")
		if r.Status != statusUnverified {
			t.Fatalf("status %s, reason %q", r.Status, r.Reason)
		}
		if code != exitFailed {
			t.Fatalf("exit %d: under -json nothing-checked-it is not a success", code)
		}
		if !r.exitCodeIsLegacyZero() {
			t.Fatal("without -json the same run must keep exiting 0")
		}
	})

	t.Run("a check the sandbox refuses is a config error with no object", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
		h := newHarness(t, fs.URL, "native", true)
		out, err := os.CreateTemp(t.TempDir(), "result")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var code int
		captureStderr(t, func() {
			captureStdout(t, func() {
				code = oneShot(h.orch, h.sess, "do it", "curl https://evil.example", true, out, nil)
			})
		})
		if code != exitUsage {
			t.Fatalf("exit %d, want %d: a check the sandbox will never run is not a verdict on the change", code, exitUsage)
		}
		if b, _ := os.ReadFile(out.Name()); len(b) != 0 {
			t.Fatalf("stdout must stay empty when nothing about the task was decided, got %q", b)
		}
	})

	t.Run("an MCP server that never answered outranks a red check", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
		h := newHarness(t, fs.URL, "native", true)
		h.orch.mcp = &MCPSet{order: []string{"jira"}, servers: map[string]*MCPServer{"jira": {Name: "jira"}}}
		h.orch.mcp.servers["jira"].setState(mcpStateFailed, "dial tcp: connection refused")
		r, code, _ := oneShotResult(t, h, "do it", "ls /no-such-path-here-7f3a")
		if r.Status != statusInfra || code != exitInfra {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
		if !strings.Contains(r.Reason, "jira") {
			t.Fatalf("the reason must name the server: %q", r.Reason)
		}
	})
}

// exitCodeIsLegacyZero is the one assertion the table test cannot make from a
// literal: that THIS result, with no check in it, still exits 0 for a human.
func (r runResult) exitCodeIsLegacyZero() bool { return r.exitCode(false) == exitOK }

// The acceptance criterion for P0-2, against the real binary: `lca … -json`
// prints one JSON object on stdout and nothing else, with every human line on
// stderr, and the exit code comes from the table.
func TestJSONRunPrintsOneObjectAndNothingElse(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "all done"} })
	c := newCLI(t, fs.URL)
	stdout, stderr, code := c.run(nil, "-y", "-json", "-check", "ls", "do the thing")
	if code != exitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	r := decodeOneObject(t, stdout)
	if r.Status != statusPassed {
		t.Fatalf("%+v", r)
	}
	if r.Role == "" || r.Session == "" || len(r.Models) == 0 {
		t.Fatalf("the object must identify the run: %+v", r)
	}
	if !strings.Contains(stdout, `"lca_version"`) || !strings.Contains(stdout, `"roles_hash"`) {
		t.Fatalf("the object must carry the version and the roles hash:\n%s", stdout)
	}
	// The model's own words are a human line and must not be on stdout.
	if strings.Contains(stdout, "all done") {
		t.Fatalf("the model's prose reached stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "all done") {
		t.Fatalf("the model's prose went nowhere; stderr:\n%s", stderr)
	}
	if bytes.ContainsRune([]byte(stdout), 0x1b) {
		t.Fatal("stdout carries terminal escapes")
	}
}

func TestExitCodesOfTheRealBinary(t *testing.T) {
	green := func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} }

	t.Run("passed is 0", func(t *testing.T) {
		fs := newFakeServer(t, green)
		c := newCLI(t, fs.URL)
		stdout, stderr, code := c.run(nil, "-y", "-json", "-check", "ls", "x")
		if code != exitOK {
			t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
		}
	})
	t.Run("a red check after all attempts is 1", func(t *testing.T) {
		fs := newFakeServer(t, green)
		c := newCLI(t, fs.URL)
		stdout, _, code := c.run(nil, "-y", "-json", "-check", "ls /no-such-path-here-7f3a", "x")
		if code != exitFailed {
			t.Fatalf("exit %d, want 1\n%s", code, stdout)
		}
		if r := decodeOneObject(t, stdout); r.Status != statusFailed {
			t.Fatalf("status %s", r.Status)
		}
	})
	t.Run("an unreachable gateway is 3", func(t *testing.T) {
		// A port nothing is listening on: bind one, learn its number, close it.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		dead := "http://" + ln.Addr().String()
		ln.Close()
		c := newCLI(t, dead)
		stdout, stderr, code := c.run(nil, "-y", "-json", "-check", "ls", "x")
		if code != exitInfra {
			t.Fatalf("exit %d, want 3\nstdout:%s\nstderr:%s", code, stdout, stderr)
		}
		r := decodeOneObject(t, stdout)
		if r.Status != statusInfra || r.Reason == "" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("running out of steps is 4", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			return fakeReply{content: "looking", calls: []ToolCall{call(fmt.Sprintf("c%d", n), "list_dir", map[string]any{"path": "."})}}
		})
		c := newCLI(t, fs.URL)
		c.extra = []string{"LCA_MAX_STEPS=3"}
		stdout, stderr, code := c.run(nil, "-y", "-json", "go in circles")
		if code != exitBudget {
			t.Fatalf("exit %d, want 4\nstdout:%s\nstderr:%s", code, stdout, stderr)
		}
		if r := decodeOneObject(t, stdout); r.Status != statusBudget {
			t.Fatalf("status %s", r.Status)
		}
	})
	t.Run("a bad flag is 2, with nothing on stdout", func(t *testing.T) {
		fs := newFakeServer(t, green)
		c := newCLI(t, fs.URL)
		stdout, _, code := c.run(nil, "-json", "-no-such-flag", "x")
		if code != exitUsage {
			t.Fatalf("exit %d, want 2", code)
		}
		if stdout != "" {
			t.Fatalf("stdout must be empty: %q", stdout)
		}
	})
	t.Run("two prompt sources is 2", func(t *testing.T) {
		fs := newFakeServer(t, green)
		c := newCLI(t, fs.URL)
		path := filepath.Join(c.root, "t.md")
		os.WriteFile(path, []byte("from the file\n"), 0o600)
		stdout, stderr, code := c.run(nil, "-json", "-prompt-file", path, "and from argv")
		if code != exitUsage {
			t.Fatalf("exit %d, want 2\n%s", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout must be empty: %q", stdout)
		}
	})
	t.Run("-json with no task at all is 2", func(t *testing.T) {
		fs := newFakeServer(t, green)
		c := newCLI(t, fs.URL)
		stdout, _, code := c.run(nil, "-y", "-json")
		if code != exitUsage {
			t.Fatalf("exit %d, want 2: -json must never open a session", code)
		}
		if stdout != "" {
			t.Fatalf("stdout must be empty: %q", stdout)
		}
	})
	t.Run("an interrupted run is 130", func(t *testing.T) {
		release := make(chan struct{})
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			<-release
			return fakeReply{content: "done"}
		})
		c := newCLI(t, fs.URL)
		cmd, stdout, stderr := c.start(nil, "-y", "-json", "-check", "ls", "x")
		// Wait until the gateway has the request, so the signal lands inside the run.
		deadline := time.Now().Add(20 * time.Second)
		for len(fs.reqs()) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		code, err := c.wait(cmd, 30*time.Second)
		close(release)
		if err != nil {
			t.Fatalf("%v\nstderr:%s", err, stderr.String())
		}
		if code != exitCancelled {
			t.Fatalf("exit %d, want 130\nstdout:%s\nstderr:%s", code, stdout.String(), stderr.String())
		}
		if r := decodeOneObject(t, stdout.String()); r.Status != statusCancelled {
			t.Fatalf("status %s", r.Status)
		}
	})
}

// ── P0-4: never wait for a human ────────────────────────────────────────────

// blockingInput is an Input whose descriptor is a pipe nobody writes to: every
// read on it blocks until the test closes the far end. It is the shape of the
// failure being tested — a cron job's stdin is an open pipe, not a closed file,
// so "the read returns EOF" is not what saves an unattended run.
func blockingInput(t *testing.T) *Input {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wr.Close(); rd.Close() })
	return NewInput(rd)
}

// The premise of every test below: a read on that pipe really does block, so a
// door that reads instead of refusing really would hang forever.
func TestABlockingInputReallyBlocks(t *testing.T) {
	in := blockingInput(t)
	done := make(chan struct{})
	go func() {
		in.ReadString('\n')
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("the fixture does not block, so the tests below prove nothing")
	case <-time.After(200 * time.Millisecond):
	}
}

// The acceptance criterion for P0-4, at the one place every tool approval goes
// through: an unattended Approver refuses without reading, and -y does not make
// the one class it withholds (mcp_write) an exception to that.
func TestUnattendedApproverNeverReads(t *testing.T) {
	in := blockingInput(t)
	ap := NewApprover(in)
	ap.TrustAll() // -y
	ap.Unattended("stdin is not a terminal")
	ap.quiet = true

	for _, kind := range []string{"mcp_write", "edit", "run", "web", "mcp", "task", "doom_loop"} {
		done := make(chan [2]bool, 1)
		go func() {
			ok, auto := ap.Confirm(kind, strings.ToUpper(kind)+" something", "")
			done <- [2]bool{ok, auto}
		}()
		select {
		case got := <-done:
			trusted := ap.Trusts(kind)
			if got[0] != trusted {
				t.Fatalf("%s: approved=%v but Trusts=%v", kind, got[0], trusted)
			}
			if kind == "mcp_write" && got[0] {
				t.Fatal("mcp_write was granted by -y, which is exactly what it must never do")
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("Confirm(%q) waited for a human", kind)
		}
	}
}

// Every other door to the keyboard in the program, in one table. These are the
// paths the requirement names — approval, /setup, the wizard, a model picker, an
// apply conflict — reduced to the functions that actually read a byte:
// Approver.Confirm (every tool approval, including an apply conflict and an MCP
// write), pick/pickOne (model picker, endpoint picker, tier and role pickers,
// every wizard menu), confirm (the wizard's yes/no and the first-start offer),
// and ask/askSecret (its text fields). None of them may read when there is
// nobody there.
func TestEveryDoorToTheKeyboardRefusesWithoutATerminal(t *testing.T) {
	cs := []choice{{id: "a", label: "one"}, {id: "b", label: "two"}}
	doors := map[string]func(in *Input) error{
		"pick": func(in *Input) error {
			_, err := pick(in, cs, pickOpts{})
			return err
		},
		"pickOne": func(in *Input) error {
			_, err := pickOne(in, cs, pickOpts{})
			return err
		},
		"confirm": func(in *Input) error {
			_, err := confirm(in, "do it?", true)
			return err
		},
		"ask": func(in *Input) error {
			_, err := ask(in, nil, "value ", "current")
			return err
		},
		"askSecret": func(in *Input) error {
			_, err := askSecret(in, "api key ")
			return err
		},
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			in := blockingInput(t)
			if in.IsTTY() {
				t.Fatal("a pipe must not answer IsTTY")
			}
			done := make(chan error, 1)
			go func() { done <- door(in) }()
			select {
			case err := <-done:
				if !errors.Is(err, errNoTTY) {
					t.Fatalf("returned %v, want errNoTTY", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("waited for a human")
			}
		})
	}
	t.Run("Approver.Confirm", func(t *testing.T) {
		in := blockingInput(t)
		ap := NewApprover(in)
		ap.Unattended("stdin is not a terminal")
		ap.quiet = true
		done := make(chan bool, 1)
		go func() {
			ok, _ := ap.Confirm("edit", "EDIT a.go", "")
			done <- ok
		}()
		select {
		case ok := <-done:
			if ok {
				t.Fatal("an unasked question was approved")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("waited for a human")
		}
	})
}

// A guard for the next person: every read of the keyboard buffer lives in one of
// five files, and in each of them it is behind a refusal — the Approver's
// unattended check, or in.IsTTY(). A read that appears anywhere else has not
// been thought about, and the failure mode of not thinking about it is an
// unattended run that hangs until somebody notices in the morning.
//
//	approval.go  Approver.Confirm, after the unattended refusal
//	pick.go      pick / pickCooked / confirm / confirmRaw / ask / askSecret, after !IsTTY
//	lineedit.go  LineEditor.ReadLine, reached only from ask/askSecret and the REPL prompt
//	repl.go      the REPL's own prompt: the session itself, which ends on EOF
//	input.go     the buffer
func TestEveryKeyboardReadLivesBehindARefusal(t *testing.T) {
	allowed := map[string]bool{"approval.go": true, "pick.go": true, "lineedit.go": true, "repl.go": true, "input.go": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, read := range []string{"in.ReadByte()", "in.ReadString(", "ed.ReadLine(", "ReadLine("} {
			if strings.Contains(string(body), read) {
				t.Errorf("%s reads the keyboard (%s). Put it behind in.IsTTY() or the Approver's unattended refusal, then add the file here with the reason.", name, read)
			}
		}
	}
}

// /dev/null is a character device, which is what the old test for "is this a
// terminal" actually asked. A run with `</dev/null` in a terminal therefore
// claimed a tty, and every picker and every wizard question believed it.
func TestDevNullAndPipesAreNotTerminals(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if isTerminal(int(devnull.Fd())) {
		t.Fatal("/dev/null answered as a terminal")
	}
	if st, err := devnull.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		t.Skip("this /dev/null is not a character device, so the bug could not happen here")
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	defer wr.Close()
	if isTerminal(int(rd.Fd())) {
		t.Fatal("a pipe answered as a terminal")
	}
	if isTerminal(-1) {
		t.Fatal("a scripted Input has no descriptor and must not claim a terminal")
	}
	// And the positive half, so the check is not simply always false.
	m, sl, err := openPTY()
	if err != nil {
		t.Skipf("no pty here: %v", err)
	}
	defer m.Close()
	defer sl.Close()
	if !isTerminal(int(sl.Fd())) {
		t.Fatal("a pty did not answer as a terminal")
	}
}

// The refusal has to be WRITTEN DOWN in both places the requirement names: the
// audit log, which is the forensic record, and the transcript, which is what the
// model and the next reader see.
func TestAnUnattendedRefusalIsRecordedAndNamesTheClass(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", true) // -y
	h.orch.ap.Unattended("-json: this run answers to a program, not to a person")
	h.orch.ap.quiet = true

	tc := &ToolCtx{Ctx: context.Background(), S: h.sess, Name: "jira__issue_comment"}
	msg, ok := tc.Ask("mcp_write", "jira", "MCP WRITE jira OPS-412", "")
	if ok {
		t.Fatal("an mcp write was allowed in an unattended run")
	}
	for _, want := range []string{"mcp_write", "unattended", "NOT retry"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the model is told %q, which does not mention %q", msg, want)
		}
	}
	// The transcript is where this message ends up: it is a tool result.
	h.sess.Msgs = append(h.sess.Msgs, Message{Role: "user", Content: msg})
	h.sess.saveTranscript()
	body, err := os.ReadFile(h.orch.rec.SessionPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "mcp_write is not granted in an unattended run") {
		t.Fatalf("the transcript does not record the refusal:\n%s", body)
	}
	audit, err := os.ReadFile(filepath.Join(h.orch.cfg.stateDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := ""
	for _, l := range strings.Split(string(audit), "\n") {
		if strings.Contains(l, "permission_denied") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no refusal in the audit log:\n%s", audit)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["permission"] != "mcp_write" || rec["unattended"] == nil {
		t.Fatalf("the audit line must say what was refused and why: %s", line)
	}
}

// The acceptance criterion for P0-4, end to end: a run with stdin on /dev/null,
// where the model tries something that needs approval, finishes BY ITSELF, and
// the audit holds the refusal.
func TestRunWithStdinOnDevNullNeverWaits(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			// A write needs approval, and this run was started without -y.
			return fakeReply{content: "writing", calls: []ToolCall{call("c1", "write", map[string]any{"path": "a.txt", "content": "x\n"})}}
		}
		return fakeReply{content: "I was not allowed to write"}
	})
	c := newCLI(t, fs.URL)
	// -prompt-file - with /dev/null on stdin: an empty task must be refused, not
	// turned into an interactive session nobody is sitting at.
	_, stderr, code := c.run(nil, "-json", "-prompt-file", "-")
	if code != exitUsage {
		t.Fatalf("-prompt-file - with an empty stdin must be a usage error, got %d\n%s", code, stderr)
	}

	path := filepath.Join(c.root, "ticket.md")
	os.WriteFile(path, []byte("write a.txt please\n"), 0o600)
	stdout, stderr, code := c.run(nil, "-json", "-prompt-file", path)
	if code != exitFailed {
		t.Fatalf("exit %d\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	r := decodeOneObject(t, stdout)
	if r.Status != statusUnverified {
		t.Fatalf("status %s, reason %q", r.Status, r.Reason)
	}
	if r.FilesChanged != 0 {
		t.Fatal("the refused write happened anyway")
	}
	if _, err := os.Stat(filepath.Join(c.root, "a.txt")); err == nil {
		t.Fatal("the file was written without approval")
	}
	audit := c.auditLog()
	if !strings.Contains(audit, "permission_denied") || !strings.Contains(audit, "unattended") {
		t.Fatalf("the audit log does not record the refusal:\n%s", audit)
	}
	msgs, err := loadSession(r.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(msgs), "unattended run") {
		t.Fatal("the transcript does not say why nothing was written")
	}
}

// ── P1-4: version and reproducibility, as far as the result object needs it ──

func TestVersionAndRolesHash(t *testing.T) {
	if lcaVersion() == "" {
		t.Fatal("lca_version must never be empty — \"unknown\" is the answer when there is nothing to report")
	}
	if rolesHash(nil) != "" {
		t.Fatal("no roles.yaml must hash to nothing, not to the hash of nothing")
	}
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LCA_ROLES", "")
	t.Setenv("LCA_CONFIG", "")
	dir := filepath.Join(home, ".lca")
	os.MkdirAll(dir, 0o755)
	cfg := Config{Root: root, Dir: dir}
	write := func(body string) *RolesConfig {
		if err := os.WriteFile(filepath.Join(dir, "roles.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		rc, err := loadRoles(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return rc
	}
	one := rolesHash(write("roles:\n  coder:\n    models: [m1]\n    prompt: |\n      Be careful.\n"))
	again := rolesHash(write("roles:\n  coder:\n    models: [m1]\n    prompt: |\n      Be careful.\n"))
	edited := rolesHash(write("roles:\n  coder:\n    models: [m1]\n    prompt: |\n      Be very careful.\n"))
	if one == "" || len(one) != 64 {
		t.Fatalf("roles_hash = %q, want a sha256", one)
	}
	if one != again {
		t.Fatal("the same team hashed differently twice")
	}
	if one == edited {
		t.Fatal("an edited prompt did not move the hash: two eval runs would look comparable when they are not")
	}
}

func TestLcaVersionSubcommand(t *testing.T) {
	c := newCLI(t, "http://127.0.0.1:1")
	stdout, stderr, code := c.run(nil, "version")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.HasPrefix(stdout, "lca ") || !strings.Contains(stdout, "roles") {
		t.Fatalf("lca version printed:\n%s", stdout)
	}
}

// ── what the result counts ──────────────────────────────────────────────────

func TestSessionChangeStatsCountsWhatTheModelWrote(t *testing.T) {
	resetChanges()
	t.Cleanup(resetChanges)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	os.WriteFile(a, []byte("one\ntwo\n"), 0o600)
	before, existed := snapshot(a)
	os.WriteFile(a, []byte("one\ntwo\nthree\n"), 0o600)
	recordChange("a.txt", a, "edit", before, existed, sumFile(a))

	b := filepath.Join(dir, "b.txt")
	nb, nexisted := snapshot(b)
	os.WriteFile(b, []byte("new\n"), 0o600)
	recordChange("b.txt", b, "write", nb, nexisted, sumFile(b))

	// Touched and left exactly as it was: not a change, and must not be counted.
	cpath := filepath.Join(dir, "c.txt")
	os.WriteFile(cpath, []byte("same\n"), 0o600)
	cb, cex := snapshot(cpath)
	recordChange("c.txt", cpath, "write", cb, cex, sumFile(cpath))

	files, diffBytes := sessionChangeStats()
	if files != 2 {
		t.Fatalf("files_changed = %d, want 2", files)
	}
	if diffBytes == 0 {
		t.Fatal("diff_bytes = 0 for two changed files")
	}
}

// ── P0-3: the budgets on one run ────────────────────────────────────────────

// The three ceilings resolve the same way: roles.yaml's defaults: are the floor,
// the flag is the override, and each one overrides independently — an operator
// who stretches the timeout for one awkward ticket must not silently lose the
// team's token ceiling with it.
func TestRunBudgetResolution(t *testing.T) {
	rc := &RolesConfig{RunTimeout: 45 * time.Minute, RunMaxSteps: 200, RunMaxTokens: 4_000_000}

	b, err := newRunBudget(rc, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.timeout != 45*time.Minute || b.maxSteps != 200 || b.maxTokens != 4_000_000 {
		t.Fatalf("the file's defaults did not survive: %+v", b)
	}

	b, err = newRunBudget(rc, 30*time.Second, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.timeout != 30*time.Second {
		t.Fatalf("-timeout did not override the file: %s", b.timeout)
	}
	if b.maxSteps != 200 || b.maxTokens != 4_000_000 {
		t.Fatalf("overriding the timeout dropped the other two ceilings: %+v", b)
	}

	b, err = newRunBudget(nil, 0, 7, 99)
	if err != nil {
		t.Fatal(err)
	}
	if b.timeout != 0 || b.maxSteps != 7 || b.maxTokens != 99 {
		t.Fatalf("with no roles.yaml the flags are the whole budget: %+v", b)
	}

	// A negative value is a wrapper building its command line wrong. "Forever" is
	// the one reading that cannot be what they meant, so it is a usage error (exit
	// 2, alert somebody) and not a clamp.
	for _, tc := range []struct {
		name          string
		d             time.Duration
		steps, tokens int
	}{
		{"timeout", -5 * time.Minute, 0, 0},
		// -1 is stepsUnlimited and a legitimate value now, so the negative that
		// must still be refused is one below it.
		{"max-steps", 0, -5, 0},
		{"max-tokens", 0, 0, -1},
	} {
		_, err := newRunBudget(rc, tc.d, tc.steps, tc.tokens)
		var ue *usageErr
		if !errors.As(err, &ue) {
			t.Errorf("a negative %s gave %v, want a usage error", tc.name, err)
		}
	}
}

// defaults: timeout: is a duration, and a bare number next to check_timeout's
// seconds has to mean seconds too — a file where one key counts seconds and its
// neighbour counts nanoseconds is a trap nobody can read their way out of.
func TestRolesDefaultsCarryTheRunBudgets(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) *RolesConfig {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "roles.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		rc, err := loadRoles(Config{Dir: dir, Root: dir})
		if err != nil {
			t.Fatalf("%v\nin:\n%s", err, body)
		}
		return rc
	}
	rc := write("defaults:\n  timeout: 45m\n  max_steps: 120\n  max_tokens: 2000000\n")
	if rc.RunTimeout != 45*time.Minute || rc.RunMaxSteps != 120 || rc.RunMaxTokens != 2_000_000 {
		t.Fatalf("%+v", rc)
	}
	if rc = write("defaults:\n  timeout: 90\n"); rc.RunTimeout != 90*time.Second {
		t.Fatalf("a bare number must be seconds, like check_timeout beside it: %s", rc.RunTimeout)
	}
	// A value that does not parse is an ERROR. Silently reading "30 minutes" as
	// "no limit" is a mistake whose only symptom is the GPU bill.
	if err := os.WriteFile(filepath.Join(dir, "roles.yaml"), []byte("defaults:\n  timeout: half an hour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRoles(Config{Dir: dir, Root: dir}); err == nil {
		t.Fatal("a timeout that is not a duration must not load as 'no limit'")
	}
	// It survives a round trip through YAML(), or /role save silently drops the
	// team's ceilings the first time anybody edits a role.
	rc = write("defaults:\n  timeout: 45m\n  max_steps: 120\n  max_tokens: 2000000\n")
	out := rc.YAML()
	for _, want := range []string{"timeout: 45m", "max_steps: 120", "max_tokens: 2000000"} {
		if !strings.Contains(out, want) {
			t.Errorf("YAML() lost %q:\n%s", want, out)
		}
	}
}

// -max-steps is a CEILING over the role's own, and it has to reach a subagent:
// that is where a lead's tokens actually go, and a subagent left on its role's 80
// would spend the run's budget on behalf of a primary capped at 3.
func TestMaxStepsIsACeilingOverEveryRole(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", true)
	h.sess.agent.Steps = 80
	if got := h.sess.maxSteps(); got != 80 {
		t.Fatalf("with no run budget the role decides: %d", got)
	}
	h.orch.budget = &runBudget{maxSteps: 3}
	// Unarmed, the ceiling is not in force: a run-wide ceiling is a property of a
	// RUN, and the REPL never starts one. This is what keeps a `defaults:
	// max_steps` out of an interactive session's way.
	if got := h.sess.maxSteps(); got != 80 {
		t.Fatalf("an unarmed budget must not cap a session, got %d", got)
	}
	_, cancel := h.orch.budget.start(context.Background())
	defer cancel()
	if got := h.sess.maxSteps(); got != 3 {
		t.Fatalf("the run's ceiling must beat the role's 80, got %d", got)
	}
	// A role that asked for LESS keeps it: the ceiling is a maximum, not a target.
	h.sess.agent.Steps = 2
	if got := h.sess.maxSteps(); got != 2 {
		t.Fatalf("a role under the ceiling must keep its own limit, got %d", got)
	}
}

// The token ceiling is the run's, not one session's: a lead and the coders it
// delegated to spend one ticket's tokens between them, and a ceiling that only
// saw the lead's own replies would bound the cheapest part of the run.
func TestTokenBudgetIsCountedAcrossTheWholeRun(t *testing.T) {
	b := &runBudget{maxTokens: 300}
	_, cancel := b.start(context.Background())
	defer cancel()
	if b.over() != "" {
		t.Fatal("a fresh budget is not over")
	}
	b.spend(90, 10, 5) // the primary
	b.spend(90, 10, 0) // a subagent
	if b.over() != "" {
		t.Fatalf("200 of 300 must not trip: %q", b.over())
	}
	b.spend(140, 10, 0)
	why := b.over()
	if why == "" {
		t.Fatal("350 of 300 must trip")
	}
	if b.tokens() != 350 {
		t.Fatalf("tokens() = %d, want 350", b.tokens())
	}
	// Split the way the result object reports it, from the same counters, so the
	// reason and the tokens field cannot disagree about one run.
	if p, c, ca := b.spentTokens(); p != 320 || c != 30 || ca != 5 {
		t.Fatalf("spentTokens() = %d/%d/%d, want 320/30/5", p, c, ca)
	}
	// The reason is CACHED, so the line in the transcript, the reason in the JSON
	// and the sentence in the summary are one string and cannot disagree.
	if b.over() != why || b.tripped() != why {
		t.Fatalf("the reason changed between readings: %q then %q", why, b.over())
	}
}

// A time budget that has passed reads as spent even though nobody fired an
// event: the deadline went by while the model was thinking.
func TestTimeBudgetReadsTheClock(t *testing.T) {
	b := &runBudget{timeout: time.Hour}
	ctx, cancel := b.start(context.Background())
	defer cancel()
	if b.over() != "" || ctx.Err() != nil {
		t.Fatal("an hour from now is not spent")
	}
	b.mu.Lock()
	b.deadline = time.Now().Add(-time.Second)
	b.mu.Unlock()
	if why := b.over(); !strings.Contains(why, "time budget") {
		t.Fatalf("a deadline in the past must read as spent, got %q", why)
	}
}

// The closing summary call must not be able to break the promise the timeout
// just made: `-timeout 30s` is accepted on the strength of "out within 35 s".
func TestTheClosingCallCannotOutliveTheTimeout(t *testing.T) {
	now := time.Now()
	b := &runBudget{timeout: 30 * time.Second}
	b.deadline = now.Add(-2 * time.Second) // the run is already over its clock
	if d := b.closingDeadline(now).Sub(now); d > summaryGrace+time.Millisecond || d <= 0 {
		t.Fatalf("a spent clock leaves only the grace, got %s", d)
	}
	b.deadline = now.Add(10 * time.Minute)
	if d := b.closingDeadline(now).Sub(now); d != summaryBudget {
		t.Fatalf("with the clock wide open the call gets its own budget, got %s", d)
	}
	var none *runBudget
	if d := none.closingDeadline(now).Sub(now); d != summaryBudget {
		t.Fatalf("no budget at all is no limit but the call's own, got %s", d)
	}
}

// The whole of P0-3 in process: a spent budget is status budget_exceeded and
// exit 4 — never "cancelled", which is what the cancellation it causes looks
// like, and which would send the ticket back to todo instead of to a person.
func TestASpentBudgetIsBudgetExceededAndExitFour(t *testing.T) {
	t.Run("tokens", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			// It would go round forever: each reply asks for another tool call.
			return fakeReply{content: "looking", calls: []ToolCall{call(fmt.Sprintf("c%d", n), "list_dir", map[string]any{"path": "."})}}
		})
		h := newHarness(t, fs.URL, "native", true)
		// The fixture reports 100 prompt + 10 completion per reply, so one reply
		// is already over.
		h.orch.budget = &runBudget{maxTokens: 50}
		r, code, _ := oneShotResult(t, h, "go in circles", "")
		if r.Status != statusBudget || code != exitBudget {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
		if !strings.Contains(r.Reason, "token") {
			t.Fatalf("the reason must say which budget: %q", r.Reason)
		}
		if n := len(fs.reqs()); n > 2 {
			t.Fatalf("%d requests: the run kept buying tokens after the ceiling", n)
		}
		// The transcript is the whole point of "gracefully".
		if b, err := os.ReadFile(r.Transcript); err != nil || len(b) == 0 {
			t.Fatalf("transcript %s: %v", r.Transcript, err)
		}
	})

	t.Run("time", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			time.Sleep(2 * time.Second) // longer than the budget below, on purpose
			return fakeReply{content: "done"}
		})
		h := newHarness(t, fs.URL, "native", true)
		h.orch.budget = &runBudget{timeout: 150 * time.Millisecond}
		start := time.Now()
		r, code, _ := oneShotResult(t, h, "take your time", "")
		if r.Status != statusBudget || code != exitBudget {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
		if elapsed := time.Since(start); elapsed > 90*time.Second {
			t.Fatalf("the run took %s — the deadline did not end it", elapsed)
		}
		if b, err := os.ReadFile(r.Transcript); err != nil || len(b) == 0 {
			t.Fatalf("transcript %s: %v", r.Transcript, err)
		}
	})

	t.Run("steps", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
			return fakeReply{content: "looking", calls: []ToolCall{call(fmt.Sprintf("c%d", n), "list_dir", map[string]any{"path": "."})}}
		})
		h := newHarness(t, fs.URL, "native", true)
		h.orch.budget = &runBudget{maxSteps: 2}
		r, code, _ := oneShotResult(t, h, "go in circles", "")
		if r.Status != statusBudget || code != exitBudget {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
	})

	// A green check outranks a spent budget: if the work was done and verified
	// before the clock ran out, it was done, and telling the wrapper otherwise
	// would send a finished ticket to a human.
	t.Run("a green check still wins", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
		h := newHarness(t, fs.URL, "native", true)
		h.orch.budget = &runBudget{maxTokens: 50}
		r, code, _ := oneShotResult(t, h, "do it", "ls")
		if r.Status != statusPassed || code != exitOK {
			t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
		}
	})
}

// A background subagent used to take context.Background(), which made it the one
// child a budget could not reach: the run ended, lca exited, and whatever the
// delegation had started kept a core busy with nobody left to read its output.
func TestABackgroundSubagentHangsOffTheRunContext(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", true)
	if h.orch.runContext() != context.Background() {
		t.Fatal("with no run started the fallback is Background")
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.orch.setRunContext(ctx)
	if h.orch.runContext().Err() != nil {
		t.Fatal("a live run context must not be cancelled")
	}
	cancel()
	if h.orch.runContext().Err() == nil {
		t.Fatal("cancelling the run must reach whatever hangs off its context")
	}
}

// The acceptance criterion for P0-3, against the real binary and the real
// processes: `-timeout 30s` on a task that is deliberately longer is out within
// 35 s with exit 4, the transcript is on disk, and NO child is left behind —
// neither the check command nor the grandchild it backgrounded.
//
// The grandchild is the half that matters. A check command is a process group
// with a build and three linkers in it; killing only the process lca started
// leaves the rest holding the machine, and the only way to know which happened
// is to ask the operating system afterwards.
func TestTimeoutEndsTheRunAndLeavesNoChildBehind(t *testing.T) {
	if testing.Short() {
		t.Skip("the criterion is a 30-second timeout; -short runs the rest of the budget tests")
	}
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	c := newCLI(t, fs.URL)
	// A check that outlives the run, and backgrounds something that would outlive
	// the check. Both pids are written where the test can read them.
	script := filepath.Join(c.root, "slow-check.sh")
	body := "#!/bin/sh\nsleep 600 &\necho $! > " + filepath.Join(c.root, "grandchild.pid") +
		"\necho $$ > " + filepath.Join(c.root, "child.pid") + "\nsleep 600\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c.extra = []string{"LCA_ALLOW=ls,cat,echo,sh,git"}

	start := time.Now()
	cmd, stdout, stderr := c.start(nil, "-y", "-json", "-timeout", "30s", "-check", "sh "+script, "x")
	code, err := c.wait(cmd, 90*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("%v\nstderr:\n%s", err, stderr.String())
	}
	if elapsed > 35*time.Second {
		t.Fatalf("-timeout 30s took %s — the criterion is 35 s", elapsed)
	}
	if code != exitBudget {
		t.Fatalf("exit %d, want 4\nstdout:%s\nstderr:%s", code, stdout.String(), stderr.String())
	}
	r := decodeOneObject(t, stdout.String())
	if r.Status != statusBudget || r.Reason == "" {
		t.Fatalf("%+v", r)
	}
	if b, rerr := os.ReadFile(r.Transcript); rerr != nil || len(b) == 0 {
		t.Fatalf("the transcript must survive the budget: %s: %v", r.Transcript, rerr)
	}
	// And now the operating system's own answer.
	for _, name := range []string{"child.pid", "grandchild.pid"} {
		pid := readPid(t, filepath.Join(c.root, name))
		if alive(pid) {
			syscall.Kill(pid, syscall.SIGKILL) // don't leak it out of the test either
			t.Errorf("%s (pid %d) outlived the run — the process group was not killed", name, pid)
		}
	}
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	// The check writes them as it starts; a few milliseconds of slack beats a
	// flake that depends on scheduling.
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			if pid, cerr := strconv.Atoi(strings.TrimSpace(string(b))); cerr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never written — the check did not run, so the test proves nothing", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// alive asks the kernel, not a process table: signal 0 is delivered to nothing
// and fails with ESRCH when the pid is gone.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	for i := 0; i < 40; i++ { // a killed process is reaped asynchronously
		if err := syscall.Kill(pid, 0); err != nil {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// ── P0-5: the pipeline sandbox profile ──────────────────────────────────────

// pipelineProfile is the two halves of the profile in examples/, as paths. The
// tests load the files that SHIP, not copies of them: a profile whose tests pass
// against their own transcription proves nothing about what a team includes.
func pipelineProfile(t *testing.T) (rolesPath, configPath string) {
	t.Helper()
	for _, name := range []string{"pipeline.roles.yaml", "pipeline.config.json"} {
		p, err := filepath.Abs(filepath.Join("examples", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the profile a team is told to include is not there: %v", err)
		}
		if name == "pipeline.roles.yaml" {
			rolesPath = p
		} else {
			configPath = p
		}
	}
	return rolesPath, configPath
}

// pipelineEnv is the one line a team adds. Both halves, because each one holds
// the layer the other cannot.
func pipelineEnv(t *testing.T) []string {
	t.Helper()
	r, c := pipelineProfile(t)
	return []string{"LCA_ROLES=" + r, "LCA_CONFIG=" + c}
}

// pipelineSession builds the run a wrapper would: the team's own roles.yaml, the
// profile included on top of it in one line (LCA_ROLES, which lca merges last),
// and -y. member="" works on this machine; a member name puts the role on
// another one, where the command crosses a login shell.
func pipelineSession(t *testing.T, gw, role, member string) (*Orchestrator, *Session) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".lca"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The team's file: the models and the machines. The profile names neither —
	// it is an overlay over whatever team is already there.
	team := "entry: coder\n"
	if member != "" {
		team += "members:\n  " + member + ":\n    host: stage-host\n    dir: /srv/work/proj\n"
	}
	team += "roles:\n"
	for _, r := range []string{"coder", "reviewer"} {
		team += "  " + r + ":\n    description: " + r + "\n    models: [test-model]\n"
		if member != "" {
			team += "    member: " + member + "\n"
		}
		team += "    prompt: |\n      do the work\n"
	}
	if err := os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"), []byte(team), 0o600); err != nil {
		t.Fatal(err)
	}
	rolesPath, configPath := pipelineProfile(t)
	t.Setenv("LCA_ROLES", rolesPath)
	t.Setenv("LCA_CONFIG", configPath)

	cfg := Config{Root: root, Dir: filepath.Join(home, ".lca"), BaseURL: gw, Endpoints: []string{gw},
		Model: "test-model", MaxSteps: 20, Tools: "native", SubagentMax: 1, KeepSessions: 10}
	ap := NewApprover(newStringInput(""))
	ap.TrustAll() // -y: the posture every one of these tests is about
	orch, err := setupOrchestrator(cfg, ap, filepath.Join(home, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		orch.CloseMCP()
		orch.tracer.Close()
		orch.rec.Close()
	})
	sess, err := orch.NewPrimary(role, "", &quietView{})
	if err != nil {
		t.Fatal(err)
	}
	return orch, sess
}

// askRun is what run_command asks before it runs anything: the permission layer,
// through the same ToolCtx a tool call uses.
func askRun(s *Session, cmd string) (string, bool) {
	tc := &ToolCtx{Ctx: context.Background(), S: s, Name: "run_command"}
	return tc.Ask("run", cmd, "RUN", "  $ "+cmd)
}

// The profile parses, and it is an OVERLAY: the team's models and member survive
// it while its sandbox and its rules win.
func TestPipelineProfileIsAnOverlay(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	orch, sess := pipelineSession(t, fs.URL, "coder", "stage")
	if got := sess.client.Model(); got != "test-model" {
		t.Fatalf("the team's model did not survive the overlay: %q", got)
	}
	if got := sess.memberName(); got != "stage" {
		t.Fatalf("the team's member did not survive the overlay: %q", got)
	}
	if orch.roles.RunTimeout == 0 || orch.roles.RunMaxTokens == 0 {
		t.Fatalf("the profile's run budgets did not load: %+v", orch.roles)
	}
	allow := orch.jl.Allowed
	for _, want := range []string{"cargo", "go", "python3", "bash", "rg", "grep", "git", "stage.sh"} {
		if !contains(allow, want) {
			t.Errorf("the profile's sandbox does not allow %q: %v", want, allow)
		}
	}
	// The hard half of the refusal: these cannot run at all, whatever any rule says.
	for _, forbidden := range []string{"curl", "wget", "scancel", "berserk-ctl.sh"} {
		if contains(allow, forbidden) {
			t.Errorf("%q is on the sandbox allowlist — the hard boundary must refuse it", forbidden)
		}
		if orch.jl.AllowCommand(forbidden) {
			t.Errorf("the jail would run %q", forbidden)
		}
	}
	// webfetch off: denied outright, so the tool is not even offered to the model.
	if !Disabled("web", sess.rules()...) {
		t.Error("webfetch is not off")
	}
	defs, _ := sess.tools()
	for _, d := range defs {
		if d.Name == "webfetch" {
			t.Error("webfetch is in the request prefix although the profile denies it")
		}
	}
}

// The acceptance criterion's middle part: a test per DENIED rule, with -y, on
// this machine and on a member — and, on the member, in the shape a shell over
// there would split, because that is where the deny used not to hold.
func TestPipelineProfileDeniesWhatTheWrapperOwns(t *testing.T) {
	denied := []string{
		"git commit -am wip",
		"git push origin HEAD",
		"git push --force origin main",
		"git reset --hard HEAD~1",
		"git checkout -- .",
		"git remote add evil http://example.invalid/r.git",
		"git config user.email agent@example.invalid",
		"ssh gpu07 nvidia-smi",
		"curl http://example.invalid/x",
		"wget http://example.invalid/x",
		"scancel 4711",
		"berserk-ctl.sh restart",
		"bsk gw drain",
	}
	// On a member the line is sent as one argument to a login shell over there, so
	// these are the same refusals hidden behind an operator the pattern does not
	// mention. Before the segment check, "git status *" matched the first one whole
	// and ALLOWED the push in it.
	chained := []string{
		"git status && git push origin HEAD",
		"git diff; git commit -am wip",
		"git log -1 | grep x && git push",
		"go test ./... && git push origin HEAD",
	}

	for _, member := range []string{"", "stage"} {
		where := "this machine"
		if member != "" {
			where = "member " + member
		}
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
		_, sess := pipelineSession(t, fs.URL, "coder", member)
		if member != "" && sess.remote() == nil {
			t.Fatalf("the session is not on %s, so this leg tests nothing", where)
		}
		if !sess.orch.ap.Trusts("run") {
			t.Fatal("-y did not take: the test would prove nothing about a deny beating trust")
		}
		cases := denied
		if member != "" {
			cases = append(append([]string(nil), denied...), chained...)
		}
		for _, cmd := range cases {
			msg, ok := askRun(sess, cmd)
			if ok {
				t.Errorf("%s: %q was ALLOWED with -y", where, cmd)
				continue
			}
			// A reason the model can read, naming the rule — not "user denied", which
			// in an unattended run is a sentence about a person who is not there.
			if !strings.Contains(msg, "denied by a permission rule") {
				t.Errorf("%s: %q was refused with %q, which does not name the rule", where, cmd, msg)
			}
			if !strings.Contains(msg, "don't retry") {
				t.Errorf("%s: %q: the refusal must tell the model not to retry: %q", where, cmd, msg)
			}
		}
	}
}

// The other half: the work itself runs without asking. A profile that denies
// everything is safe and useless, and an allow that quietly became an "ask" is a
// refusal in an unattended run.
func TestPipelineProfileAllowsTheWork(t *testing.T) {
	for _, member := range []string{"", "stage"} {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
		_, sess := pipelineSession(t, fs.URL, "coder", member)
		for _, cmd := range []string{
			"cargo build --release",
			"cargo test -p gw",
			"go test ./...",
			"gofmt -l .",
			"python3 -m pytest -q",
			"bash check.sh",
			"rg --no-heading TODO",
			"grep -rn TODO .",
			"git status --porcelain",
			"git diff --stat",
			"git log --oneline -5",
			"git show HEAD",
			"stage/stage.sh check",
			"stage/stage.sh logs gw",
			"stage/stage.sh status",
			"ssh stage-host stage/stage.sh logs gw",
		} {
			if msg, ok := askRun(sess, cmd); !ok {
				t.Errorf("member %q: %q was refused: %s", member, cmd, msg)
			}
		}
	}
}

// The segment rule on its own, away from any session: where a shell splits the
// line, the most restrictive answer wins — and where it does not, the question is
// exactly the one it always was.
func TestRunActionChecksEverySegmentWhereAShellSplitsTheLine(t *testing.T) {
	rules := Ruleset{
		{"run", "*", Ask},
		{"run", "git status *", Allow},
		{"run", "go test *", Allow},
		{"run", "git push *", Deny},
	}
	line := "git status && git push origin main"
	if got := Evaluate("run", line, rules); got != Allow {
		t.Fatalf("the premise of this test is gone: the whole line used to match the allow, got %s", got)
	}
	if got := runAction(line, true, rules); got != Deny {
		t.Fatalf("a shell will run the push, so it must be denied, got %s", got)
	}
	if got := runAction(line, false, rules); got != Allow {
		t.Fatalf("with no shell the operators are inert arguments and the answer is unchanged, got %s", got)
	}
	// No deny in sight, but a segment nobody allowed: the most restrictive answer
	// is "ask", which is the old reShellChain rule generalised.
	if got := runAction("go test ./... && rm -rf /", true, rules); got != Ask {
		t.Fatalf("an unlisted segment must pull the answer back to ask, got %s", got)
	}
	// A quoted operator is data, not a second command.
	if got := runAction(`git status --format='a && b'`, true, rules); got != Allow {
		t.Fatalf("a quoted && is an argument, got %s", got)
	}
}

// The acceptance criterion's last part: an attempted `git push` is a refusal the
// MODEL READS and carries on from — not an error, not a dead session. The run
// finishes by itself and the refusal is in the transcript and the audit log.
func TestAnAttemptedGitPushIsRefusedAndTheRunCarriesOn(t *testing.T) {
	var second string
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{content: "pushing", calls: []ToolCall{
				call("p1", "run_command", map[string]any{"command": "git push origin HEAD"})}}
		}
		// What came back from the refused call, as the model saw it.
		if c, ok := req.last()["content"].(string); ok {
			second = c
		}
		return fakeReply{content: "I cannot push — the wrapper does that. Done."}
	})
	orch, sess := pipelineSession(t, fs.URL, "coder", "")
	r, code, _ := oneShotResultWith(t, orch, sess, "fix it and push", "")
	if code != exitFailed || r.Status != statusUnverified {
		// No -check was given, so "unverified" is the honest verdict; what matters
		// is that the run ENDED on its own with a verdict at all.
		t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
	}
	if !strings.Contains(second, "denied by a permission rule") {
		t.Fatalf("the model was told %q — it must be able to read why", second)
	}
	if r.ToolErrors == 0 && r.ToolCalls == 0 {
		t.Fatalf("the refused call is not accounted for: %+v", r)
	}
	// The forensic copy and the operator's copy.
	audit, err := os.ReadFile(filepath.Join(orch.cfg.stateDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), "permission_denied") || !strings.Contains(string(audit), "git push") {
		t.Fatalf("the audit log does not hold the refusal:\n%s", audit)
	}
	tr, err := os.ReadFile(r.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tr), "denied by a permission rule") {
		t.Fatalf("the transcript does not hold the refusal:\n%s", tr)
	}
}

// The acceptance criterion's first part: `lca doctor -role coder` prints the
// permissions in force. Against the real binary, because the question it answers
// is "what did including this profile actually grant", and the answer has to be
// readable without a Go test in the loop.
func TestDoctorRolePrintsThePermissionsInForce(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	c := newCLI(t, fs.URL)
	if err := os.MkdirAll(filepath.Join(c.root, ".lca"), 0o755); err != nil {
		t.Fatal(err)
	}
	team := "entry: coder\nroles:\n  coder:\n    description: coder\n    models: [test-model]\n    prompt: |\n      work\n"
	if err := os.WriteFile(filepath.Join(c.root, ".lca", "roles.yaml"), []byte(team), 0o600); err != nil {
		t.Fatal(err)
	}
	c.extra = pipelineEnv(t)
	stdout, stderr, _ := c.run(nil, "doctor", "-role", "coder", "-no-probe")
	out := stripANSI(stdout + stderr)
	for _, want := range []string{
		"PERMISSIONS", "role coder",
		"git push *", "git commit *", "git reset --hard *", "git checkout -- *",
		"git remote *", "git config *", "ssh *", "curl *", "wget *",
		"scancel *", "berserk-ctl.sh *",
		"cargo *", "stage/stage.sh check *",
		"webfetch is off",
		"last match wins",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor -role coder does not print %q", want)
		}
	}
	// And the sandbox allowlist itself, which is the half no rule can widen.
	if !strings.Contains(out, "sandbox") || !strings.Contains(out, "stage.sh") {
		t.Errorf("doctor -role coder does not print the sandbox in force:\n%s", out)
	}
	// An unknown role is a mistake in the call and says so, instead of printing
	// somebody else's permissions.
	stdout, stderr, code := c.run(nil, "doctor", "-role", "nosuchrole", "-no-probe")
	if code == 0 {
		t.Error("doctor -role nosuchrole exited 0")
	}
	if !strings.Contains(stripANSI(stdout+stderr), "no role") {
		t.Errorf("doctor -role nosuchrole did not say so:\n%s%s", stdout, stderr)
	}
}

// oneShotResultWith is oneShotResult for an orchestrator a test built itself —
// one with a roles.yaml and a profile behind it, rather than the bare harness.
func oneShotResultWith(t *testing.T, orch *Orchestrator, sess *Session, prompt, check string) (runResult, int, string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "result")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	var code int
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			code = oneShot(orch, sess, prompt, check, true, out, nil)
		})
	})
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return decodeOneObject(t, string(b)), code, stderr
}

// ── P0-6: the short summary a person reads ──────────────────────────────────

// factsFixture is a run that did something: two files changed, a check that went
// red twice, and a cost. Everything the requirement says the summary must carry.
func factsFixture(status string) summaryFacts {
	return summaryFacts{
		role: "coder", status: status, reason: "`./check.sh` still failed (exit 1) after 2 attempts",
		check: "./check.sh", exit: 1, checked: true, tail: "FAILED: gw::tests::affinity", attempts: 2,
		files: []fileEdit{
			{name: "src/bin/gw.rs", added: 12, removed: 3},
			{name: "src/affinity.rs", added: 40, removed: 0, created: true},
		},
		turns: 17, tools: 42, errs: 1,
		promptTok: 41000, outputTok: 2100, elapsed: 3*time.Minute + 12*time.Second,
		transcript: "/state/transcripts/abc.md", trace: "/state/traces/abc.jsonl",
	}
}

// The acceptance criterion for P0-6: a summary exists for every status except an
// infra_error before the first model reply, and it is at most 3 KB.
func TestASummaryExistsForEveryStatus(t *testing.T) {
	for _, status := range []string{statusPassed, statusFailed, statusUnverified, statusBudget, statusInfra, statusCancelled} {
		t.Run(status, func(t *testing.T) {
			fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
				return fakeReply{content: "## What changed\n\n- src/bin/gw.rs — pinned the session id\n"}
			})
			h := newHarness(t, fs.URL, "native", true)
			path := filepath.Join(t.TempDir(), "summary.md")
			h.orch.summary = path
			// A reply has ARRIVED: every status but the one exception below is
			// reached after the model has said something. Replies and not Turns —
			// Turns counts requests sent, successful or not, so it says nothing about
			// whether anything answered.
			h.sess.stats.Turns, h.sess.stats.Replies = 3, 3
			if got := h.orch.writeSummary(h.sess, factsFixture(status)); got != path {
				t.Fatalf("writeSummary returned %q, want the path it was given", got)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no summary for status %s: %v", status, err)
			}
			if len(b) == 0 {
				t.Fatalf("the summary for status %s is empty", status)
			}
			if len(b) > summaryMax {
				t.Fatalf("the summary for status %s is %d bytes, over the 3 KB limit", status, len(b))
			}
		})
	}

	// The one exception the requirement names, and the row of the exit table that
	// is the same exception. Nothing was decided about the task and there is no
	// gateway behind it: nothing to summarise, and nobody to write it.
	for _, status := range []string{statusInfra, statusConfig} {
		t.Run("not for "+status+" before the first reply", func(t *testing.T) {
			fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "x"} })
			h := newHarness(t, fs.URL, "native", true)
			path := filepath.Join(t.TempDir(), "summary.md")
			h.orch.summary = path
			// One request was SENT and nothing came back, which is the real shape of
			// this case and the shape the old Turns==0 gate could not express.
			h.sess.stats.Turns, h.sess.stats.Replies = 1, 0
			if got := h.orch.writeSummary(h.sess, factsFixture(status)); got != "" {
				t.Fatalf("writeSummary returned %q", got)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("a file was written anyway: %v", err)
			}
			if len(fs.reqs()) != 0 {
				t.Fatal("a gateway call was spent summarising a run that decided nothing")
			}
		})
	}
	// But once the model HAS replied, an infra_error is summarised like anything
	// else: the gateway died mid-run, and what was done before it died is exactly
	// what somebody needs to read.
	t.Run("but an infra_error after a reply is summarised", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{status: 503, content: "gone"} })
		h := newHarness(t, fs.URL, "native", true)
		path := filepath.Join(t.TempDir(), "summary.md")
		h.orch.summary = path
		h.sess.stats.Turns, h.sess.stats.Replies = 2, 2
		// A run whose clock has already run out, which is also the only thing that
		// stops a 503 being retried for the closing call's whole minute.
		b := &runBudget{timeout: 30 * time.Second}
		b.armed = true
		b.deadline = time.Now().Add(-time.Second)
		h.orch.budget = b
		if got := h.orch.writeSummary(h.sess, factsFixture(statusInfra)); got != path {
			t.Fatalf("writeSummary returned %q", got)
		}
		if b, err := os.ReadFile(path); err != nil || len(b) == 0 {
			t.Fatalf("%v", err)
		}
	})

	// And with no -summary at all, nothing happens and nothing is reported.
	t.Run("not asked for, not written", func(t *testing.T) {
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "x"} })
		h := newHarness(t, fs.URL, "native", true)
		if got := h.orch.writeSummary(h.sess, factsFixture(statusPassed)); got != "" {
			t.Fatalf("writeSummary returned %q with no -summary", got)
		}
		if len(fs.reqs()) != 0 {
			t.Fatal("a gateway request was spent on a summary nobody asked for")
		}
	})
}

// What the requirement says has to be IN it: one line per file, the check with
// its exit and its attempts, what did not work, the cost, and the two links.
func TestTheSummaryCarriesWhatTheRequirementLists(t *testing.T) {
	var asked string
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		asked, _ = req.last()["content"].(string)
		return fakeReply{content: "## What changed\n\n- src/bin/gw.rs — pinned the session id\n- src/affinity.rs — new, the lookup\n\n## How it was verified\n\n`./check.sh` exit 1 after 2 attempts.\n\n## What did not work\n\nThe affinity test still fails.\n"}
	})
	h := newHarness(t, fs.URL, "native", true)
	path := filepath.Join(t.TempDir(), "summary.md")
	h.orch.summary = path
	h.sess.stats.Turns = 3
	h.orch.writeSummary(h.sess, factsFixture(statusFailed))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{
		"src/bin/gw.rs", "src/affinity.rs", // one line per file
		"./check.sh",                      // the command
		"## What it cost", "3m12s", "41k", // time and tokens
		"## Where to look", "/state/transcripts/abc.md", "/state/traces/abc.jsonl",
		"/state/traces/abc.html", "lca report", // the report file, and what renders it
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, out)
		}
	}

	// The model is HANDED the facts rather than asked to recall them: a model
	// asked for a token count invents one.
	for _, want := range []string{"src/bin/gw.rs +12 -3", "src/affinity.rs +40 -0 (new)", "exit 1", "2 attempts"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the model was not told %q:\n%s", want, asked)
		}
	}
	// It is ONE call, on its own prefix, and it offers the model no tools: the
	// session's cached prefix must not move, and a closing summary must not be
	// able to call anything.
	reqs := fs.reqs()
	if len(reqs) != 1 {
		t.Fatalf("%d requests for one summary", len(reqs))
	}
	if len(reqs[0].Tools) != 0 {
		t.Errorf("the summary request carried %d tool schemas", len(reqs[0].Tools))
	}
	if sys := reqs[0].system(); sys != summarySystem {
		t.Errorf("the summary call did not use its own system prompt:\n%s", sys)
	}
}

// 3 KB is a hard limit, not a target: a wrapper pastes this into a Jira comment
// and an MR description without looking at it.
func TestTheSummaryIsClampedToThreeKilobytes(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: strings.Repeat("## What changed\n\n- a file — a change nobody asked for\n", 400)}
	})
	h := newHarness(t, fs.URL, "native", true)
	path := filepath.Join(t.TempDir(), "summary.md")
	h.orch.summary = path
	h.sess.stats.Turns = 3
	h.orch.writeSummary(h.sess, factsFixture(statusPassed))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > summaryMax {
		t.Fatalf("%d bytes, over the limit", len(b))
	}
	out := string(b)
	// The parts that must be exact are appended AFTER the prose and counted
	// against the limit first, so a long-winded model cannot push the links out.
	if !strings.Contains(out, "## Where to look") || !strings.Contains(out, "/state/traces/abc.jsonl") {
		t.Fatalf("clamping dropped the links:\n%s", out)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("a cut summary must say it was cut:\n%s", out)
	}
}

func TestClampMarkdownCutsAtALineBoundary(t *testing.T) {
	in := "alpha\n" + strings.Repeat("beta is a whole line of prose\n", 8) + "omega\n"
	if got := clampMarkdown(in, 4096); got != in {
		t.Fatalf("a body that fits is untouched: %q", got)
	}
	const limit = 160
	got := clampMarkdown(in, limit)
	if len(got) > limit {
		t.Fatalf("%d bytes for a limit of %d", len(got), limit)
	}
	if !strings.HasPrefix(got, "alpha\n") || strings.Contains(got, "omega") {
		t.Fatalf("cut badly: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("a cut body must say so: %q", got)
	}
	// Cut at a LINE boundary: a markdown file ending mid-sentence reads as a bug
	// in lca, and the note has to be the last thing in it.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	for _, l := range lines {
		if l != "" && !strings.HasPrefix(l, "[truncated") && l != "alpha" && l != "beta is a whole line of prose" {
			t.Fatalf("a partial line survived: %q", l)
		}
	}
	if got := clampMarkdown(in, 4); got != "" {
		t.Fatalf("no room for even the note, so nothing is claimed: %q", got)
	}
}

// The gateway can go away between the verdict and the summary. The facts are
// still facts, so lca writes them itself: the criterion is that a summary EXISTS,
// and a wrapper that finds an empty file has learnt nothing about the run.
func TestTheSummaryFallsBackToLcaWhenTheModelCannotWriteIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply fakeReply
	}{
		{"the gateway refuses", fakeReply{status: 500, content: "upstream is gone"}},
		{"the gateway hangs up", fakeReply{hangup: true}},
		{"the model answers nothing", fakeReply{content: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return tc.reply })
			h := newHarness(t, fs.URL, "native", true)
			path := filepath.Join(t.TempDir(), "summary.md")
			h.orch.summary = path
			h.sess.stats.Turns = 3
			if got := h.orch.writeSummary(h.sess, factsFixture(statusFailed)); got != path {
				t.Fatalf("writeSummary returned %q", got)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			out := string(b)
			if len(b) > summaryMax {
				t.Fatalf("%d bytes", len(b))
			}
			for _, want := range []string{"## What changed", "src/bin/gw.rs", "./check.sh",
				"## What did not work", "## What it cost", "/state/traces/abc.jsonl"} {
				if !strings.Contains(out, want) {
					t.Errorf("the fallback does not carry %q:\n%s", want, out)
				}
			}
		})
	}
}

// sessionFileEdits is where the "one line per file" comes from, and it counts
// what the model's own tools did — not `git diff`, because the wrapper owns git
// and somebody else's uncommitted work would make git's numbers say things about
// this run that are false.
func TestSessionFileEditsCountsPerFile(t *testing.T) {
	resetChanges()
	t.Cleanup(resetChanges)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	os.WriteFile(a, []byte("one\ntwo\n"), 0o600)
	before, existed := snapshot(a)
	os.WriteFile(a, []byte("one\nTWO\nthree\n"), 0o600)
	recordChange("a.txt", a, "edit", before, existed, sumFile(a))

	b := filepath.Join(dir, "b.txt")
	nb, nex := snapshot(b)
	os.WriteFile(b, []byte("new\n"), 0o600)
	recordChange("b.txt", b, "write", nb, nex, sumFile(b))

	// Touched and left exactly as it was: not a change, and not a line in the
	// summary either.
	c := filepath.Join(dir, "c.txt")
	os.WriteFile(c, []byte("same\n"), 0o600)
	cb, cex := snapshot(c)
	recordChange("c.txt", c, "write", cb, cex, sumFile(c))

	edits := sessionFileEdits()
	if len(edits) != 2 {
		t.Fatalf("%d files, want 2: %+v", len(edits), edits)
	}
	byName := map[string]fileEdit{}
	for _, e := range edits {
		byName[e.name] = e
	}
	if e := byName["a.txt"]; e.added != 2 || e.removed != 1 || e.created {
		t.Errorf("a.txt: %+v", e)
	}
	if e := byName["b.txt"]; e.added != 1 || !e.created {
		t.Errorf("b.txt: %+v", e)
	}
}

// -summary against the real binary, on a run that failed: the file is there, it
// fits, and the JSON result says where it is so the wrapper can find it without
// being told twice.
func TestSummaryFlagOnTheRealBinary(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "## What changed\n\n(none)\n\n## How it was verified\n\nThe check stayed red.\n\n## What did not work\n\nThe path does not exist.\n"}
	})
	c := newCLI(t, fs.URL)
	path := filepath.Join(c.root, "summary.md")
	stdout, stderr, code := c.run(nil, "-y", "-json", "-summary", path,
		"-check", "ls /no-such-path-here-7f3a", "x")
	if code != exitFailed {
		t.Fatalf("exit %d\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	r := decodeOneObject(t, stdout)
	if r.Summary != path {
		t.Fatalf("the result does not point at the summary: %q", r.Summary)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a failed run must still be summarised: %v", err)
	}
	if len(b) == 0 || len(b) > summaryMax {
		t.Fatalf("%d bytes", len(b))
	}
	out := string(b)
	for _, want := range []string{"## What it cost", "## Where to look", r.Transcript, r.Trace} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, out)
		}
	}
	// -summary describes ONE run; with no task it is a usage error rather than a
	// flag that quietly does nothing.
	stdout, _, code = c.run(nil, "-y", "-summary", path)
	if code != exitUsage {
		t.Fatalf("-summary with no task exited %d, want 2\n%s", code, stdout)
	}
}

// The other two children a budget has to reach, at the one place all three of
// them are the same code. A check command, a workflow step and a subagent's
// run_command all go through commandFor + inProcessGroup with a context derived
// from the run's, so cancelling the run has to take the whole process GROUP with
// it — the grandchild is the proof, because killing only the process lca started
// leaves a build holding the machine.
//
// The MCP stdio server's half of this is TestMCPStdioReaped, which already
// proves CloseMCP kills that group and its grandchild; main.go calls CloseMCP on
// every one-shot exit path, budget_exceeded included.
func TestCancellingTheRunKillsACommandsWholeGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh(1)")
	}
	dir := t.TempDir()
	jl, err := NewJail(dir, []string{"sh"}, false)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "spawn.sh")
	body := "#!/bin/sh\nsleep 600 &\necho $! > " + filepath.Join(dir, "grandchild.pid") +
		"\necho $$ > " + filepath.Join(dir, "child.pid") + "\nsleep 600\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	// The run's context, cancelled the way a spent clock cancels it.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		execCheck(ctx, jl, "sh "+script, 10*time.Minute)
		close(done)
	}()
	child := readPid(t, filepath.Join(dir, "child.pid"))
	grand := readPid(t, filepath.Join(dir, "grandchild.pid"))
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the check did not return after the run was cancelled")
	}
	for name, pid := range map[string]int{"the command": child, "its grandchild": grand} {
		if alive(pid) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("%s (pid %d) outlived the run", name, pid)
		}
	}
}

// `lca -timeout 30s "task"` without -json must still exit 4. The legacy rule
// that makes a bare one-shot exit 0 "when the model answered" protects
// `lca "task"` in a terminal; a run somebody budgeted was budgeted by something
// that means to read the code.
func TestABudgetedRunDoesNotFallBackToTheLegacyExitZero(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "looking", calls: []ToolCall{call(fmt.Sprintf("c%d", n), "list_dir", map[string]any{"path": "."})}}
	})
	c := newCLI(t, fs.URL)
	_, stderr, code := c.run(nil, "-y", "-max-steps", "2", "-max-tokens", "50", "go in circles")
	if code != exitBudget {
		t.Fatalf("exit %d, want 4\nstderr:\n%s", code, stderr)
	}
	// And the bare one-shot it protects is unchanged: no budget, no check, the
	// model answered, exit 0.
	fs2 := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	c2 := newCLI(t, fs2.URL)
	if _, stderr, code := c2.run(nil, "-y", "say something"); code != exitOK {
		t.Fatalf("a bare one-shot exited %d, want 0\nstderr:\n%s", code, stderr)
	}
}

// ── the result object, on the paths that used to lose it ────────────────────

// A flag written after the positional task is a usage error, not part of the
// prompt. Without this the model is handed "do the thing -json", the whole human
// terminal output goes to the caller's JSON parser, and the exit code comes from
// the legacy mapping.
func TestAFlagAfterTheTaskIsAUsageError(t *testing.T) {
	if err := checkTrailingFlags([]string{"do the thing", "-json"}, []string{"lca", "do the thing", "-json"}); err == nil {
		t.Fatal("a -json after the task must be refused, not swallowed into the prompt")
	} else if !strings.Contains(err.Error(), "-json") {
		t.Fatalf("the refusal must name the flag it found: %v", err)
	}
	if err := checkTrailingFlags([]string{"do the thing"}, []string{"lca", "-y", "do the thing"}); err != nil {
		t.Fatalf("an ordinary task must pass: %v", err)
	}
	// After an explicit -- the caller has said they mean it.
	if err := checkTrailingFlags([]string{"-json"}, []string{"lca", "--", "-json"}); err != nil {
		t.Fatalf("-- is the escape hatch: %v", err)
	}
	// And the real binary: stdout must be EMPTY, not a terminal session.
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	c := newCLI(t, fs.URL)
	stdout, stderr, code := c.run(nil, "-y", "do the thing", "-json")
	if code != exitUsage {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitUsage, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty on a usage error, got %q", stdout)
	}
	if len(fs.reqs()) != 0 {
		t.Fatalf("the mangled prompt reached the gateway: %d requests", len(fs.reqs()))
	}
}

// Two -prompt-file flags are a bug in the caller, and the one that is silently
// dropped is a ticket nobody read. The sibling spelling (-prompt-file plus a
// positional) was already refused; this makes the pair consistent.
func TestARepeatedPromptFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.md"), filepath.Join(dir, "b.md")
	if err := os.WriteFile(a, []byte("TASK-A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("TASK-B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &oncePath{flag: "-prompt-file"}
	if err := p.Set(a); err != nil {
		t.Fatalf("the first one is fine: %v", err)
	}
	err := p.Set(b)
	if err == nil {
		t.Fatal("the second -prompt-file must be an error, not the winner")
	}
	for _, want := range []string{"a.md", "b.md", "twice"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must name both values: %v", err)
		}
	}
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	c := newCLI(t, fs.URL)
	stdout, stderr, code := c.run(nil, "-y", "-json", "-prompt-file", a, "-prompt-file", b)
	if code != exitUsage {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, exitUsage, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	if len(fs.reqs()) != 0 {
		t.Fatal("one of the two tickets was run anyway")
	}
}

// -prompt-file must be a FILE. A fifo blocks in open(2) before any signal
// handler, any deadline or any budget exists, so nothing in the program can end
// it; /dev/zero is the same open read until the OOM killer arrives.
func TestAPromptFileThatIsNotAFileIsRefusedAndNeverWaits(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{"a directory": dir}
	fifo := filepath.Join(dir, "t.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err == nil {
		cases["a named pipe"] = fifo
	}
	if _, err := os.Stat(os.DevNull); err == nil {
		cases["a device"] = "/dev/zero"
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := resolvePrompt(nil, path, strings.NewReader(""))
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("%s was accepted as a ticket", path)
				}
				var ue *usageErr
				if !errors.As(err, &ue) {
					t.Fatalf("%s must be a usage error (exit 2), got %T: %v", path, err, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("resolvePrompt is still waiting on %s — this is the hang", path)
			}
		})
	}
}

// A prompt that is not UTF-8 is refused at the door. Accepted, every consumer is
// encoding/json, which rewrites each bad byte as U+FFFD and reports no error —
// so the model is handed a mangled ticket AND the transcript agrees with it,
// which is precisely the pair the byte-for-byte criterion is checked against.
func TestANonUTF8PromptFileIsRefusedWithAnOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp1251.md")
	// "привет " in cp1251 followed by a lone 0xff: valid in that encoding, not in
	// this one.
	bad := append([]byte("ticket: "), 0xef, 0xf0, 0xe8, 0xe2, 0xe5, 0xf2, '\n')
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePrompt(nil, path, nil)
	if err == nil {
		t.Fatal("a non-UTF-8 ticket was accepted")
	}
	if !strings.Contains(err.Error(), "offset 8") {
		t.Fatalf("the refusal must name the byte offset so the file can be seeked to: %v", err)
	}
	// And valid UTF-8 with the same characters is of course fine.
	if err := os.WriteFile(path, []byte("ticket: привет\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolvePrompt(nil, path, nil); err != nil || got != "ticket: привет\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A prompt file larger than the guard is refused rather than read into memory.
func TestAnEnormousPromptFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.md")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), promptFileMax+1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePrompt(nil, path, nil); err == nil {
		t.Fatal("a file over the guard was accepted")
	}
	// And one just under it is not.
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), promptFileMax-1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePrompt(nil, path, nil); err != nil {
		t.Fatalf("a file inside the guard must be read: %v", err)
	}
}

// SIGTERM is the ordinary way a cron job is bounded — `timeout`, systemd,
// docker stop, pkill — and unhandled it killed the run outright: no object on
// stdout, an exit code outside the table, and the stdio MCP children orphaned.
// All three signals land on the one cancellation path.
func TestEverySignalThatBoundsACronJobEndsTheRunCleanly(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			// A gateway that answers slowly, so the signal arrives mid-request.
			fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
				time.Sleep(20 * time.Second)
				return fakeReply{content: "too late"}
			})
			c := newCLI(t, fs.URL)
			cmd, out, errb := c.start(nil, "-y", "-json", "-check", "ls", "do the thing")
			// Wait until the request is in flight, so the signal cannot land before
			// the handler is armed.
			deadline := time.Now().Add(10 * time.Second)
			for len(fs.reqs()) == 0 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			code, err := c.wait(cmd, 30*time.Second)
			if err != nil {
				t.Fatalf("%v\nstderr:\n%s", err, errb.String())
			}
			if code != exitCancelled {
				t.Fatalf("exit %d, want %d (one row of the table for all three)\nstdout:\n%s\nstderr:\n%s",
					code, exitCancelled, out.String(), errb.String())
			}
			r := decodeOneObject(t, out.String())
			if r.Status != statusCancelled {
				t.Fatalf("status %s, want %s", r.Status, statusCancelled)
			}
			if r.Transcript == "" {
				t.Fatal("the object must still say where the transcript is")
			}
			if b, err := os.ReadFile(r.Transcript); err != nil || len(b) == 0 {
				t.Fatalf("no transcript was written: %v", err)
			}
		})
	}
}

// A context overflow that survived the one compaction retry is "it did not fit"
// (exit 4), not "lca was called wrong" (exit 2) — and the object is written,
// because by then the model may already have edited files.
func TestAContextOverflowIsBudgetExceededAndStillReportsAnObject(t *testing.T) {
	st, why := classifyRunErr(&APIError{Status: 400,
		Body: `{"error":{"message":"This model's maximum context length is 8192 tokens, however you requested 90000"}}`})
	if st != statusBudget {
		t.Fatalf("status %s (%q), want %s: row 2 leaves the ticket claimed forever", st, why, statusBudget)
	}
	if (runResult{Status: st}).exitCode(true) != exitBudget {
		t.Fatalf("an overflow must exit %d", exitBudget)
	}
	// A plain malformed request stays a config error: that one really is ours.
	if st, _ := classifyRunErr(&APIError{Status: 400, Body: `{"error":{"message":"unknown field \"foo\""}}`}); st != statusConfig {
		t.Fatalf("a malformed request is %s, want %s", st, statusConfig)
	}
}

// A statusConfig that lands MID-run still writes the object: there is a session
// id, a transcript with the model's edits in it and a token bill, and the
// wrapper was told to alert a human — it needs something to attach.
func TestAMidRunConfigErrorStillWritesTheObject(t *testing.T) {
	resetChanges()
	t.Cleanup(resetChanges)
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if n == 1 {
			return fakeReply{content: "writing", calls: []ToolCall{call("c1", "write",
				map[string]any{"path": "a.txt", "content": "hello\n"})}}
		}
		return fakeReply{status: 400, content: `{"error":{"message":"unknown field \"foo\""}}`}
	})
	h := newHarness(t, fs.URL, "native", true)
	r, code, _ := oneShotResult(t, h, "write a file", "")
	if code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
	if r.Status != statusConfig {
		t.Fatalf("status %s, want %s (reason %q)", r.Status, statusConfig, r.Reason)
	}
	if r.Transcript == "" || r.Session == "" {
		t.Fatalf("the object must carry what there is to attach: %+v", r)
	}
	if r.FilesChanged != 1 {
		t.Fatalf("files_changed = %d: the model had already edited the tree", r.FilesChanged)
	}
	// And a config error before anything happened still writes nothing, which is
	// the row the table gives no status word.
	resetChanges()
	fs2 := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	h2 := newHarness(t, fs2.URL, "native", true)
	out, err := os.CreateTemp(t.TempDir(), "result")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	captureStderr(t, func() {
		captureStdout(t, func() {
			if c := oneShot(h2.orch, h2.sess, "do it", "curl https://evil.example", true, out, nil); c != exitUsage {
				t.Errorf("exit %d, want %d", c, exitUsage)
			}
		})
	})
	if b, _ := os.ReadFile(out.Name()); len(b) != 0 {
		t.Fatalf("stdout must stay empty when nothing happened at all, got %q", b)
	}
}

// check_cmd, check_exit and check_tail are all present on every row. A wrapper
// written against the documented object read r["check_cmd"] and got a KeyError
// on exactly the row that sends the ticket to a human.
func TestTheCheckFieldsArePresentOnEveryRow(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	c := newCLI(t, fs.URL)
	for _, args := range [][]string{
		{"-y", "-json", "do it"},                              // unverified
		{"-y", "-json", "-check", "ls", "do it"},              // passed
		{"-y", "-json", "-check", "ls /no-such-zz9", "do it"}, // failed
	} {
		stdout, stderr, _ := c.run(nil, args...)
		var raw map[string]any
		if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
			t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
		}
		for _, k := range []string{"check_cmd", "check_exit", "check_tail"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("%v: %q is absent — the three fields describing one thing must appear together\n%s", args, k, stdout)
			}
		}
	}
}

// A panic in a tool is an ordinary tool error for the model, not the end of the
// process. This is tested through execCalls with the panicking tool marked
// Parallel, because that is where it matters: a panic in a goroutine runs NO
// deferred function anywhere — no CloseMCP, no tracer, no encoder — so the
// wrapper got Go's exit 2 with an empty stdout and a goroutine dump in its log.
func TestAPanicInAToolIsAnErrorForTheModelAndNotTheEndOfTheProcess(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	h := newHarness(t, fs.URL, "native", true)
	boom := &ToolDef{Name: "explode", Desc: "panics", Parallel: true,
		Run: func(tc *ToolCtx, a Args) string { panic("a nil map write in a tool") }}
	fine := &ToolDef{Name: "quiet", Desc: "does not", Parallel: true,
		Run: func(tc *ToolCtx, a Args) string { return "all well" }}
	calls := []pendingCall{
		{id: "c1", name: "explode", def: boom, args: Args{}, native: true},
		{id: "c2", name: "quiet", def: fine, args: Args{}, native: true},
	}
	var stop atomic.Bool
	var results []string
	captureStderr(t, func() {
		results = h.sess.execCalls(context.Background(), calls, &stop)
	})
	if !strings.Contains(results[0], "crashed") {
		t.Fatalf("the crash must come back as a tool error: %q", results[0])
	}
	// Not the model's fault, and not an invitation to retry.
	if !strings.Contains(results[0], "bug in lca") {
		t.Fatalf("the refusal must say whose bug it is: %q", results[0])
	}
	if !calls[0].failed {
		t.Fatal("the crash must be counted as a tool error")
	}
	// The sibling call in the same batch is unaffected.
	if results[1] != "all well" {
		t.Fatalf("the other parallel call was lost: %q", results[1])
	}
	// And it is on the record: the stack goes to the audit log and the trace, not
	// to stderr, which the wrapper is reading as a log.
	b, err := os.ReadFile(filepath.Join(filepath.Dir(h.orch.rec.SessionPath()), "..", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "tool_panic") {
		t.Fatalf("the crash is not in the audit log:\n%s", b)
	}
}

// A transcript the disk refused to hold is not ASSERTED. os.WriteFile truncates
// first, so the previous good transcript was destroyed, and the object went on
// naming the path: the wrapper attached a 0-byte file as the record of the run.
func TestATranscriptThatCouldNotBeWrittenIsNotReported(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	h := newHarness(t, fs.URL, "native", true)
	// A transcript path inside a directory that cannot be written: the atomic
	// write creates its temp file there, so this is the ENOSPC shape without
	// filling a disk.
	dir := t.TempDir()
	locked := filepath.Join(dir, "ro")
	if err := os.MkdirAll(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	good := filepath.Join(locked, "session.json")
	if err := os.WriteFile(good, []byte(`["the previous good transcript"]`), 0o600); err != nil {
		// 0500 refused the write, which is the point; make it after the fact.
		os.Chmod(locked, 0o700)
		if err := os.WriteFile(good, []byte(`["the previous good transcript"]`), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chmod(locked, 0o500)
	}
	h.orch.rec.session = good
	r, code, _ := oneShotResult(t, h, "do it", "ls")
	if code != exitOK || r.Status != statusPassed {
		t.Fatalf("the status is a fact about the check and must not move: %s / %d", r.Status, code)
	}
	if r.Transcript != "" {
		t.Fatalf("transcript = %q, but it could not be written — the wrapper must not attach it", r.Transcript)
	}
	if !strings.Contains(r.Reason, "transcript could not be written") {
		t.Fatalf("the reason must say so: %q", r.Reason)
	}
	// And the previous good one is still there: a failed write destroys nothing.
	if b, err := os.ReadFile(good); err != nil || !strings.Contains(string(b), "previous good") {
		t.Fatalf("the last good transcript was destroyed: %q %v", b, err)
	}
}

// ── the budgets, where they did not bound what they claimed ─────────────────

// A spent budget stops the model AND the verifier's retries. The ceilings
// deliberately let the verifier finish — one more check, because a run whose
// last reply said "done" and went one token over must still have its check run.
// One. Without this the identical check was re-run verify_attempts times against
// a byte-identical tree the model could no longer touch: with the pipeline
// profile's check_timeout of 1800, up to an hour of stand builds bought after
// the run had decided to stop.
func TestASpentBudgetLeavesTheVerifierExactlyOneCheck(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "looking", calls: []ToolCall{call(fmt.Sprintf("c%d", n), "list_dir", map[string]any{"path": "."})}}
	})
	h := newHarness(t, fs.URL, "native", true)
	// A check that counts its own runs, so the assertion is about what RAN and not
	// about what the verdict says.
	counter := filepath.Join(t.TempDir(), "runs")
	check := "sh -c " + strconv.Quote("echo x >> "+counter+"; exit 3")
	h.orch.jl.SetAllowed([]string{"echo", "ls", "sh"})
	h.orch.roles = &RolesConfig{VerifyAttempts: 4}
	// The fixture reports 100 prompt + 10 completion per reply, so one reply is
	// already over.
	h.orch.budget = &runBudget{maxTokens: 50}
	r, code, _ := oneShotResult(t, h, "go in circles", check)
	if r.Status != statusBudget || code != exitBudget {
		t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
	}
	b, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("the check never ran at all — the one check is the point: %v", err)
	}
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Fatalf("the check ran %d times; a spent budget leaves exactly one", n)
	}
	if r.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", r.Attempts)
	}
}

// -max-steps bounds the RUN. Counted from the session's own loop variable it
// bounded one attempt of one session: Session.Run restarts at 0 on every
// verifier attempt and every subagent gets the ceiling of its own, so
// `max_steps: 200` with `verify_attempts: 3` and one delegation was 1200 model
// requests and not 200.
func TestMaxStepsBoundsTheWholeRunAndNotOneAttempt(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "looking", calls: []ToolCall{call(fmt.Sprintf("c%d", n), "list_dir", map[string]any{"path": "."})}}
	})
	h := newHarness(t, fs.URL, "native", true)
	h.orch.roles = &RolesConfig{VerifyAttempts: 3}
	h.orch.budget = &runBudget{maxSteps: 2}
	r, code, _ := oneShotResult(t, h, "go in circles", "ls /no-such-path-here-7f3a")
	if r.Status != statusBudget || code != exitBudget {
		t.Fatalf("status %s, exit %d, reason %q", r.Status, code, r.Reason)
	}
	// Two steps for the whole run, not two per attempt times three attempts.
	if n := len(fs.reqs()); n > 2 {
		t.Fatalf("%d model requests for a 2-step run over 3 verifier attempts — the ceiling counts one attempt of one session", n)
	}
	if !strings.Contains(r.Reason, "step") {
		t.Fatalf("the reason must say which ceiling: %q", r.Reason)
	}
}

// The ceilings are a property of a RUN, so an interactive session is not wedged
// by a `defaults: max_tokens` it crossed over a long day. Unarmed, the budget
// still COUNTS — the cost report needs it — and refuses nothing.
func TestAnUnarmedBudgetCountsButRefusesNothing(t *testing.T) {
	b := &runBudget{maxTokens: 50, maxSteps: 1}
	b.spend(100, 10, 0)
	b.spendStep()
	b.spendStep()
	if why := b.over(); why != "" {
		t.Fatalf("an unarmed budget must refuse nothing, got %q", why)
	}
	if b.steps() != 0 {
		t.Fatalf("an unarmed budget must cap no session, got %d", b.steps())
	}
	if b.tokens() != 110 {
		t.Fatalf("it must still count what was spent: %d", b.tokens())
	}
	// And once a run arms it, the same spend is over.
	_, cancel := b.start(context.Background())
	defer cancel()
	b.spend(1, 0, 0)
	if why := b.over(); why == "" {
		t.Fatal("an armed budget past its ceiling must be over")
	}
}

// The run's cost is the RUN's: the compactor's request — the single most
// expensive one a long run makes, since the whole conversation goes up as the
// prompt — and the closing summary call are both in it. Uncounted, -max-tokens
// never saw them and the ticket's cost line understated the real spend.
func TestCompactionAndTheClosingCallAreCounted(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "a summary of the conversation"} })
	h := newHarness(t, fs.URL, "native", true)
	b := &runBudget{}
	_, cancel := b.start(context.Background())
	defer cancel()
	h.orch.budget = b
	h.sess.Msgs = []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
		{Role: "assistant", Content: "four"},
	}
	if err := h.sess.Compact(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	afterCompact := b.tokens()
	if afterCompact == 0 {
		t.Fatal("compaction's tokens are counted nowhere — it is the most expensive request in a long run")
	}
	h.orch.summary = filepath.Join(t.TempDir(), "s.md")
	h.sess.stats.Replies = 1
	if got := h.orch.writeSummary(h.sess, factsFixture(statusPassed)); got == "" {
		t.Fatal("no summary was written")
	}
	if b.tokens() <= afterCompact {
		t.Fatalf("the closing call's tokens are not in the run's total: %d then %d", afterCompact, b.tokens())
	}
	// And the request was BOUNDED, so the ceiling cannot be overshot by whatever
	// the model felt like writing.
	last := fs.reqs()[len(fs.reqs())-1]
	if mt, _ := last.Raw["max_tokens"].(float64); int(mt) != summaryMaxTokens {
		t.Fatalf("the closing call carries max_tokens %v, want %d", last.Raw["max_tokens"], summaryMaxTokens)
	}
}

// The object's `tokens` is the RUN's spend, so it cannot contradict the `reason`
// standing next to it. Read from one session's stats, a reason of "1.1k of 900
// tokens" sat beside a tokens of 220 in the same object, and an eval comparing
// two prompts where one delegates compared a number wrong by whatever the
// subagents spent.
func TestTheObjectsTokensAreTheRunsAndNotOneSessions(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	h := newHarness(t, fs.URL, "native", true)
	b := &runBudget{}
	_, cancel := b.start(context.Background())
	defer cancel()
	// Tokens spent by a session that is not the primary — a subagent, the
	// compactor — before the primary says anything.
	b.spend(1000, 100, 7)
	h.orch.budget = b
	r, _, _ := oneShotResult(t, h, "do it", "ls")
	if r.Tokens.Prompt < 1000 || r.Tokens.Completion < 100 {
		t.Fatalf("tokens = %+v, and another session in the run had already spent 1000/100", r.Tokens)
	}
	if r.Tokens.Cached < 7 {
		t.Fatalf("cached = %d, want at least 7", r.Tokens.Cached)
	}
	if p, c, _ := b.spentTokens(); r.Tokens.Prompt != p || r.Tokens.Completion != c {
		t.Fatalf("the object and the budget disagree about one run: %+v vs %d/%d", r.Tokens, p, c)
	}
}

// roles.yaml's defaults: are in force for `lca eval` too, which the document
// names as the second caller of lca. Only main.go used to build a budget, and
// every method on a nil one is nil-safe, so the ceilings evaluated to "none"
// without a word.
func TestEvalArmsTheBudgetFromRolesDefaults(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "looking", calls: []ToolCall{call(fmt.Sprintf("c%d", n), "list_dir", map[string]any{"path": "."})}}
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".lca"), 0o755); err != nil {
		t.Fatal(err)
	}
	roles := "entry: build\ndefaults:\n  max_steps: 2\nroles:\n  build:\n    description: b\n    models: [test-model]\n    prompt: |\n      work\n"
	if err := os.WriteFile(filepath.Join(root, ".lca", "roles.yaml"), []byte(roles), 0o600); err != nil {
		t.Fatal(err)
	}
	// An eval task works in a worktree, so the project has to be a repository.
	os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".lca/\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if _, err := gitCmd(root, nil, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	tasks := filepath.Join(t.TempDir(), "t")
	if err := os.MkdirAll(tasks, 0o755); err != nil {
		t.Fatal(err)
	}
	task := "prompt: |\n  go in circles\ncheck_cmd: ls\ntimeout: 60\n"
	if err := os.WriteFile(filepath.Join(tasks, "loop.yaml"), []byte(task), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Root: root, Dir: filepath.Join(home, ".lca"), BaseURL: fs.URL, Endpoints: []string{fs.URL},
		Model: "test-model", MaxSteps: 50, Tools: "native", SubagentMax: 1, KeepSessions: 10,
		Allowed: []string{"ls", "echo"}, StateDir: filepath.Join(home, "state")}
	captureStderr(t, func() {
		captureStdout(t, func() { runEval(context.Background(), cfg, []string{tasks}) })
	})
	// The task must have RUN — a vacuous zero would make the ceiling below prove
	// nothing — and then stopped at two steps, not at the role's fallback of 50.
	n := len(fs.reqs())
	if n == 0 {
		t.Fatal("the eval task never reached the gateway, so this proves nothing about its ceiling")
	}
	if n > 2 {
		t.Fatalf("%d model requests: defaults.max_steps was ignored by the eval", n)
	}
}

// ── the hangs ───────────────────────────────────────────────────────────────

// Nothing lca starts has a controlling terminal, so no child can ask the
// operator a question on /dev/tty — which is a stall nobody can see and whose
// prompt text goes into neither captured stream.
func TestNoChildOfARunHasATerminalOrACredentialPrompt(t *testing.T) {
	// The tty half is asserted on the ATTRIBUTE and not on the child's own
	// observation, because `go test` already has no controlling terminal: a child
	// that inherited one would see exactly what a child in its own session sees,
	// so the behavioural check below cannot distinguish them and only the request
	// to the kernel can.
	probe := exec.Command("true")
	inProcessGroup(probe)
	if probe.SysProcAttr == nil || !probe.SysProcAttr.Setsid {
		t.Fatal("a child is not put in its own SESSION, so it keeps lca's controlling terminal and can block asking on it")
	}
	root := t.TempDir()
	jl, err := NewJail(root, []string{"sh"}, false)
	if err != nil {
		t.Fatal(err)
	}
	// A command that reads /dev/tty is the shape a credential helper and a
	// pinentry both have. With no controlling terminal the open fails at once
	// instead of waiting.
	// One line, and no nested double quotes: tokenize understands quotes but
	// neither backslash escapes nor newlines, so either would split the argument
	// rather than survive inside it.
	script := `if head -c1 /dev/tty >/dev/null 2>&1; then echo HAVE_TTY; else echo NO_TTY; fi; echo prompt=$GIT_TERMINAL_PROMPT; echo askpass=$GIT_ASKPASS`
	out, exit := execCheck(context.Background(), jl, "sh -c "+strconv.Quote(script), 10*time.Second)
	if exit != 0 {
		t.Fatalf("exit %d: %s", exit, out)
	}
	if strings.Contains(out, "HAVE_TTY") {
		t.Fatalf("a child kept lca's controlling terminal, so it can still block on it:\n%s", out)
	}
	if !strings.Contains(out, "prompt=0") || !strings.Contains(out, "askpass=/bin/true") {
		t.Fatalf("the credential doors are not shut:\n%s", out)
	}
}

// When the RUN's deadline kills a check, the tail says so. Blamed on the
// check's own timeout it named a limit that was never approached — a sentence
// that is fed back to the model, reaches the summary and lands in the ticket,
// where an operator reads it and raises check_timeout, which changes nothing.
func TestARunDeadlineDoesNotBlameTheChecksOwnTimeout(t *testing.T) {
	root := t.TempDir()
	jl, err := NewJail(root, []string{"sleep"}, false)
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	out, exit := execCheck(parent, jl, "sleep 30", 30*time.Minute)
	if exit != -1 {
		t.Fatalf("exit %d, want -1: %s", exit, out)
	}
	if strings.Contains(out, "check timed out after 30m") {
		t.Fatalf("the run's deadline fired, not the check's 30-minute one:\n%s", out)
	}
	if !strings.Contains(out, "run's time budget") {
		t.Fatalf("the tail must name the deadline that actually fired:\n%s", out)
	}
	// And the check's OWN timeout is still worded as itself.
	out2, _ := execCheck(context.Background(), jl, "sleep 30", 300*time.Millisecond)
	if !strings.Contains(out2, "check timed out after") {
		t.Fatalf("the check's own limit must still say so:\n%s", out2)
	}
}

// ── telling a mistake in a file from a machine that is away ─────────────────

// A server that was never DIALLED is not a machine that is away. One typo in the
// mcp block of config.json used to make every non-passing run in that project
// infra_error / exit 3: the wrapper put the ticket back in todo and re-ran it on
// every tick forever, because a file does not fix itself — and while that entry
// stayed broken no genuinely red check could reach a human either, since exit 3
// outranks exit 1.
func TestAMalformedMCPEntryIsNotAnUnreachableMachine(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	h := newHarness(t, fs.URL, "native", true)
	bad := &MCPServer{Name: "typo"}
	bad.loadErr = `servers.typo: transport: is missing (want http or stdio)`
	bad.setState(mcpStateFailed, bad.loadErr)
	h.orch.mcp = &MCPSet{order: []string{"typo"}, servers: map[string]*MCPServer{"typo": bad}}
	if down := h.orch.mcp.unreachable(); len(down) != 0 {
		t.Fatalf("a server nothing ever dialled is reported as unreachable: %v", down)
	}
	r, code, _ := oneShotResult(t, h, "do it", "ls /no-such-path-here-7f3a")
	if r.Status != statusFailed || code != exitFailed {
		t.Fatalf("status %s, exit %d, reason %q: a red check must reach a human, not the retry queue", r.Status, code, r.Reason)
	}
	// And a server that really was tried and did not answer is still infra.
	h.orch.mcp.servers["typo"].loadErr = ""
	h.orch.mcp.servers["typo"].setState(mcpStateFailed, "dial tcp 127.0.0.1:1: connect: connection refused")
	if down := h.orch.mcp.unreachable(); len(down) != 1 {
		t.Fatalf("a machine that is away must still be reported: %v", down)
	}
}

// A malformed endpoint never reached the network, so it is a config error
// (exit 2, alert somebody) and not infrastructure (exit 3, retry forever). A
// typo in LCA_BASE_URL is the likeliest configuration mistake in this pipeline.
func TestAMalformedEndpointIsConfigAndNotInfra(t *testing.T) {
	for _, url := range []string{"htp://127.0.0.1:9/v1/chat/completions", "http://127.0.0.1:99999/v1/chat/completions"} {
		req, err := http.NewRequest("POST", url, strings.NewReader("{}"))
		if err != nil {
			// A URL the request builder itself rejects never gets as far as Do; the
			// two above do.
			t.Fatalf("%s: %v", url, err)
		}
		_, derr := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if derr == nil {
			t.Fatalf("%s: expected the client to refuse it", url)
		}
		if why := infraReason(derr); why != "" {
			t.Errorf("%s was called infrastructure (%q) — exit 3 returns the ticket to todo and retries it forever", url, why)
		}
		if st, _ := classifyRunErr(derr); st != statusConfig {
			t.Errorf("%s classified as %s, want %s (alert somebody, leave the ticket alone)", url, st, statusConfig)
		}
	}
	// The sibling cases are genuinely ambiguous or genuinely transport, and must
	// stay infra: a VPN may be down, and a refused connection is a machine.
	for _, url := range []string{"http://127.0.0.1:1/v1/chat/completions", "http://no-such-host-7f3a.invalid/v1/chat/completions"} {
		req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
		_, derr := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if derr == nil {
			continue
		}
		if why := infraReason(derr); why == "" {
			t.Errorf("%s must stay infrastructure: %v", url, derr)
		}
	}
}

// ── the profile, fail-closed ────────────────────────────────────────────────

// Every spelling of a denied command that execs byte-identically to the denied
// line is denied too. With `"*": "ask"` as the base and -y — the profile's own
// documented command line — Approver.Confirm consulted trust before "nobody is
// here to ask", so every spelling the patterns did not match LITERALLY ran.
func TestThePipelineProfileIsFailClosedUnderDashY(t *testing.T) {
	for _, member := range []string{"", "stage"} {
		where := "this machine"
		if member != "" {
			where = "member " + member
		}
		fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
		_, sess := pipelineSession(t, fs.URL, "coder", member)
		if !sess.orch.ap.Trusts("run") {
			t.Fatal("-y did not take: the test would prove nothing")
		}
		for _, cmd := range []string{
			"git  push origin HEAD",     // two spaces: tokenize collapses them
			"git\tpush origin HEAD",     // a tab, likewise
			"git 'push' origin HEAD",    // a quoted subcommand
			"git -C . push origin HEAD", // a flag before the subcommand
			"git --git-dir=.git push",   // and another
			"bash -c 'git push origin HEAD'",
			"sh -c 'git push'",
			"python3 -c 'import subprocess'",
			"nvidia-smi", // nothing in the file mentions it at all
			"git config alias.p push",
		} {
			msg, ok := askRun(sess, cmd)
			if ok {
				t.Errorf("%s: %q was ALLOWED with -y — the base rule is not fail-closed", where, cmd)
				continue
			}
			if !strings.Contains(msg, "denied by a permission rule") {
				t.Errorf("%s: %q was refused with %q, which does not name the rule", where, cmd, msg)
			}
		}
	}
}

// Canonicalisation on its own: the line is matched as the kernel will see it.
func TestRunActionCanonicalisesBeforeMatching(t *testing.T) {
	rules := Ruleset{{"run", "*", Allow}, {"run", "git push *", Deny}}
	for _, line := range []string{"git  push origin HEAD", "git\tpush origin HEAD", "git 'push' origin HEAD", `git "push" origin HEAD`} {
		if got := Evaluate("run", line, rules); got != Allow {
			t.Fatalf("the premise is gone: %q already matched the deny as written", line)
		}
		if got := runAction(line, false, rules); got != Deny {
			t.Errorf("%q execs identically to the denied line, got %s", line, got)
		}
	}
	// An unbalanced quote has nothing to canonicalise, and the raw answer stands.
	if got := runAction(`git push "origin`, false, rules); got != Deny {
		t.Errorf("an unbalanced quote must not lose the raw match, got %s", got)
	}
}

// doctor answers the question the pipeline actually asks. It printed "asks — and
// an unattended run refuses every question" for `run` under the profile, while
// the pipeline's own command line is `lca -y …`, where that same ask is an
// ALLOW: an operator read it, concluded `git -C . push` would be refused, and
// shipped a run that pushes.
func TestDoctorSaysWhatDashYActuallyGrants(t *testing.T) {
	plain := NewApprover(newStringInput(""))
	plain.Unattended("no terminal")
	yes := NewApprover(newStringInput(""))
	yes.TrustAll()
	yes.Unattended("no terminal")
	if got := permNote("run", Ask, plain); !strings.Contains(got, "refuses every question") {
		t.Fatalf("without -y an ask is a refusal: %q", got)
	}
	if got := permNote("run", Ask, yes); !strings.Contains(got, "ALLOWED") {
		t.Fatalf("with -y an ask is an allow, and doctor must say so: %q", got)
	}
	// mcp_write is the deliberate exception: -y does not trust it, so an ask there
	// really is refused in an unattended run.
	if got := permNote("mcp_write", Ask, yes); strings.Contains(got, "ALLOWED") {
		t.Fatalf("-y must not be reported as granting mcp_write: %q", got)
	}
	// And every class -y DOES reach reads as allowed, which is the half a
	// hand-written list of classes got wrong: -y trusts everything but mcp_write.
	for _, k := range []string{"edit", "run", "web", "mcp", "doom_loop", "task", "skill"} {
		if got := permNote(k, Ask, yes); !strings.Contains(got, "ALLOWED") {
			t.Errorf("-y trusts %s (Approver.Trusts says so), but doctor prints %q", k, got)
		}
	}
}

// A workflow `run:` step is checked by the permission rules too. It used to meet
// the jail allowlist alone, so every deny in a profile was inert for `lca run`:
// `git` is on the allowlist, and a step reading `run: git push …` ran and
// pushed — while the same line through the model's run_command was refused.
func TestAWorkflowRunStepMeetsThePermissionRules(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	orch, sess := pipelineSession(t, fs.URL, "coder", "")
	dir := t.TempDir()
	lg, err := newRunLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	r := &wfRunner{orch: orch, lead: sess, dir: dir, log: lg,
		res: map[string]*StepState{}, st: &WorkflowState{}}
	step := &WorkflowStep{Name: "pushit", Member: localMemberName}
	out, exit := r.shellStep(context.Background(), step, "git push origin HEAD:refs/heads/evil", 10*time.Second)
	if exit != -1 {
		t.Fatalf("exit %d: the step ran\n%s", exit, out)
	}
	if !strings.Contains(out, "denied by a permission rule") {
		t.Fatalf("the refusal must name the rule, so it reaches run.log: %q", out)
	}
	// And a step the profile allows still runs: the rules are a narrowing, not an
	// off switch for `lca run`.
	if out, exit := r.shellStep(context.Background(), step, "echo ok", 10*time.Second); exit != 0 {
		t.Fatalf("an allowed step was refused (exit %d): %s", exit, out)
	}
}

// ── what goes out to a wider audience ───────────────────────────────────────

// check_tail and reason are the fields a wrapper pastes into a public merge
// request and a ticket comment, and the check's output is not ours: a check.sh
// that echoes its environment on failure puts a live token there.
func TestTheResultObjectAndTheSummaryAreRedacted(t *testing.T) {
	t.Setenv("BSK_GW_TOKEN", "sk-live-7f3a-not-a-real-secret")
	t.Setenv("JIRA_PASSWORD", "hunter2hunter2")
	// envSecrets is read once per process, so this test states what it needs
	// rather than relying on the order tests run in.
	dropSecrets(t)

	// First the sinks, which is what the wrapper publishes: check_tail and reason
	// in the result object, and the summary file.
	leaky := "token=sk-live-7f3a-not-a-real-secret\x1b[31m FAILED\x1b[0m"
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "## What changed\n\nthe log said " + leaky + "\n"}
	})
	h := newHarness(t, fs.URL, "native", true)
	h.orch.jl.SetAllowed([]string{"echo", "ls", "sh"})
	h.orch.summary = filepath.Join(t.TempDir(), "s.md")
	r, _, _ := oneShotResult(t, h, "do it", "sh -c "+strconv.Quote("echo "+leaky+"; exit 3"))
	if r.CheckTail == "" {
		t.Fatalf("the fixture produced no tail: %+v", r)
	}
	if strings.Contains(r.CheckTail, "sk-live-7f3a") {
		t.Fatalf("check_tail carries a live token into a public merge request: %q", r.CheckTail)
	}
	if strings.ContainsRune(r.CheckTail, 0x1b) {
		t.Fatalf("check_tail carries terminal escapes into a ticket comment: %q", r.CheckTail)
	}
	if r.Summary == "" {
		t.Fatal("no summary was written")
	}
	sb, err := os.ReadFile(r.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sb), "sk-live-7f3a") {
		t.Fatalf("the summary quotes the token the model was handed:\n%s", sb)
	}
	if strings.ContainsRune(string(sb), 0x1b) {
		t.Fatalf("the summary carries terminal escapes:\n%s", sb)
	}

	// Then the primitive, including the bytes a check's own output can contain.
	tail := "curl -H Authorization: Bearer sk-live-7f3a-not-a-real-secret\nFAILED\n\x1b[31mred\x1b[0m\x00"
	got := forPublication(tail)
	if strings.Contains(got, "sk-live-7f3a") {
		t.Fatalf("the token survived: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("the redaction must be visible, not silent: %q", got)
	}
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0) {
		t.Fatalf("escapes and NUL must not reach a ticket comment: %q", got)
	}
	if !strings.Contains(got, "FAILED") {
		t.Fatalf("the useful part of the output must survive: %q", got)
	}
	// A short value is not a secret, and redacting it would destroy the output.
	t.Setenv("TINY_KEY", "ok")
	dropSecrets(t)
	if got := forPublication("everything is ok here"); got != "everything is ok here" {
		t.Fatalf("a two-character value must not redact the whole log: %q", got)
	}
}

// clampMarkdown cuts at a rune boundary. Cut by bytes, a model writing Russian
// prose as one long paragraph produced invalid UTF-8 — and what the wrapper does
// next is read the file to put it in a merge request description, so the failure
// lands at the "post the comment" step of a run that otherwise passed.
func TestClampMarkdownNeverProducesInvalidUTF8(t *testing.T) {
	for _, r := range []string{"я", "№", "😀"} {
		// One long line, which is what a first paragraph of prose is.
		body := strings.Repeat(r, 4096)
		for footer := 0; footer <= 11; footer++ {
			out := clampMarkdown(body, summaryMax-footer)
			if !utf8.ValidString(out) {
				t.Fatalf("rune %q, footer %d: the clamp produced invalid UTF-8", r, footer)
			}
			if len(out) > summaryMax-footer {
				t.Fatalf("rune %q, footer %d: %d bytes over the limit", r, footer, len(out))
			}
		}
	}
}

// `lca version` prints the hash for a roles.yaml this binary would REFUSE to
// load, which is the common case in a project configured for this pipeline: the
// file names MCP tools, validation needs the MCP set to exist first, and this
// subcommand loads no MCP. The one place whose job is printing the run's
// identity was the one place it was unavailable.
func TestVersionPrintsTheRolesHashEvenForARolesFileItWouldReject(t *testing.T) {
	c := newCLI(t, "http://127.0.0.1:1")
	if err := os.MkdirAll(filepath.Join(c.root, ".lca"), 0o755); err != nil {
		t.Fatal(err)
	}
	// tix__get_issue resolves against the tool registry, which only an MCP load
	// fills, so loadRoles refuses this file.
	roles := "roles:\n  coder:\n    models: [m1]\n    tools: [read_file, write, tix__get_issue]\n    prompt: |\n      work\n"
	path := filepath.Join(c.root, ".lca", "roles.yaml")
	if err := os.WriteFile(path, []byte(roles), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := c.run(nil, "version")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := rolesHash(&RolesConfig{Sources: []string{path}})
	if want == "" {
		t.Fatal("the fixture produced no hash")
	}
	if !strings.Contains(stdout, want) {
		t.Fatalf("lca version did not print the hash the run itself uses (%s):\n%s", want, stdout)
	}
	// And it still says the file would not load here, because that is a different
	// question from what the file is.
	if !strings.Contains(stdout, "would not load") {
		t.Fatalf("a roles.yaml this binary rejects must still be reported as such:\n%s", stdout)
	}
}

// files_changed answers "did lca change the tree", which is the only question
// the wrapper asks of it — it decides from it whether there is anything to
// commit. A delegation's merge is a git operation and never enters the
// tool-level change log, so for every delegating role (and `lead` is the default
// entry role) the field read zero for a run that had rewritten files.
func TestFilesChangedCountsWhatADelegationBroughtHome(t *testing.T) {
	resetChanges()
	t.Cleanup(resetChanges)
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	h := newHarness(t, fs.URL, "native", true)
	// What delegate.go records at the apply site, with no gateway in the way: the
	// files the merge put into the caller's tree and the patch's size.
	h.orch.noteApplied(h.root, []string{"src/main.rs", "src/lib.rs"}, 1840)
	files, bytes := h.orch.runChangeStats()
	if files != 2 || bytes != 1840 {
		t.Fatalf("runChangeStats() = %d files / %d bytes, want 2 / 1840", files, bytes)
	}
	// And it is a UNION, not a sum: a file the caller edited and a delegation then
	// merged into is one changed file, and "3 files" in a merge request that
	// touched two is the same kind of wrong as "0 files".
	p := filepath.Join(h.root, "src", "main.rs")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	before, existed := snapshot(p)
	if err := os.WriteFile(p, []byte("fn main(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordChange("src/main.rs", p, "write", before, existed, sumFile(p))
	if files, _ := h.orch.runChangeStats(); files != 2 {
		t.Fatalf("files_changed = %d after the caller edited one of the two, want 2", files)
	}
	r := h.orch.resultOf(h.sess, Verdict{Status: statusPassed}, "", files, bytes, time.Now(), nil)
	if r.FilesChanged == 0 {
		t.Fatalf("the object says lca changed nothing: %+v", r)
	}
}

// A -summary path that cannot be written is a usage error BEFORE the gateway is
// touched. Discovered at the end, the run had already spent the work, the check
// and one more model call, and then reported `passed` / exit 0 with an empty
// merge request description and nothing alerting anybody.
func TestABadSummaryPathIsRefusedAtStartup(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "done"} })
	c := newCLI(t, fs.URL)
	locked := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	stdout, stderr, code := c.run(nil, "-y", "-json", "-summary", filepath.Join(locked, "s.md"), "do it")
	if code != exitUsage {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, exitUsage, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	if len(fs.reqs()) != 0 {
		t.Fatal("the gateway was called for a run whose summary could never be written")
	}
	if !strings.Contains(stderr, "-summary") {
		t.Fatalf("the refusal must name the flag: %s", stderr)
	}
	// And a good path is not disturbed: the probe must leave no file behind.
	good := filepath.Join(t.TempDir(), "sub", "s.md")
	if err := probeWritable(good, "-summary"); err != nil {
		t.Fatalf("a writable path was refused: %v", err)
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatalf("the probe left a file behind: %v", err)
	}
}

// The transcript is saved AFTER the closing summary call, so the file a reviewer
// is handed as a Jira attachment holds the call the trace and the audit log both
// have. Saved before, the two accounts of what the run cost disagreed by exactly
// that call.
func TestTheTranscriptIsSavedAfterTheSummary(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		return fakeReply{content: "## What changed\n\n- nothing much\n"}
	})
	h := newHarness(t, fs.URL, "native", true)
	h.orch.summary = filepath.Join(t.TempDir(), "s.md")
	r, _, _ := oneShotResult(t, h, "do it", "ls")
	if r.Summary == "" {
		t.Fatal("no summary was written")
	}
	b, err := os.ReadFile(r.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []Message
	if err := json.Unmarshal(b, &msgs); err != nil {
		t.Fatalf("the transcript is not valid JSON: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("the transcript has %d messages", len(msgs))
	}
	// The ordering itself, which is the fix: the transcript is the file written
	// LAST, so it holds every turn the run made including the closing call's. The
	// closing call is a separate request with its own system prompt, so it adds no
	// message to this conversation — the only thing that can be asserted about it
	// from outside is which write came second.
	ts, err := os.Stat(r.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := os.Stat(r.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if ts.ModTime().Before(ss.ModTime()) {
		t.Fatalf("the transcript (%s) was saved before the summary (%s), so the closing call's turn is in the trace and not in the file the ticket gets",
			ts.ModTime(), ss.ModTime())
	}
	// And the run's cost is in the footer, from the budget, not from one session.
	sb, err := os.ReadFile(r.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sb), "prompt + ") {
		t.Fatalf("the footer's cost block is missing:\n%s", sb)
	}
}
