package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxReadBytes   = 200_000
	maxGrepMatches = 200
	maxListEntries = 400
	maxCmdOutput   = 64_000
	maxToolOutput  = 50_000
)

// cmdTimeout bounds run_command; overridable via LCA_CMD_TIMEOUT (see main).
var cmdTimeout = 120 * time.Second

// listDir renders a bounded, indented tree of a directory (default: jail root)
// so the model can orient itself without dumping the repo. Skips .git, marks
// directories with a trailing slash, and caps the entry count.
func listDir(j *Jail, path string) string {
	if path == "" {
		path = "."
	}
	abs, err := j.Resolve(path)
	if err != nil {
		return "error: " + err.Error()
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "error: " + err.Error()
	}
	if !info.IsDir() {
		return "error: not a directory: " + path
	}

	var b strings.Builder
	count := 0
	truncated := false
	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == abs {
			return nil // skip the root itself
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if count >= maxListEntries {
			truncated = true
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(abs, p)
		depth := strings.Count(rel, string(filepath.Separator))
		name := d.Name()
		if d.IsDir() {
			name += "/"
		}
		fmt.Fprintf(&b, "%s%s\n", strings.Repeat("  ", depth), name)
		count++
		return nil
	})
	if walkErr != nil {
		return "error: " + walkErr.Error()
	}

	out := b.String()
	if out == "" {
		out = "(empty directory)\n"
	}
	if truncated {
		out += fmt.Sprintf("... (truncated at %d entries)\n", maxListEntries)
	}
	return strings.TrimRight(path, "/") + "/\n" + out
}

// readFile returns raw file content (no line-number prefixes, so the model can
// copy exact text into a <search> block). An optional lines="a-b" range slices
// it; oversized files without a range are truncated with a note.
func readFile(j *Jail, path, lines string) string {
	abs, err := j.Resolve(path)
	if err != nil {
		return "error: " + err.Error()
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "error: " + err.Error()
	}
	content := string(data)

	if lines != "" {
		all := strings.Split(content, "\n")
		lo, hi, ok := parseRange(lines, len(all))
		if !ok {
			return fmt.Sprintf("error: bad lines=%q (want e.g. 10-40)", lines)
		}
		return fmt.Sprintf("%s (lines %d-%d):\n%s", path, lo, hi, strings.Join(all[lo-1:hi], "\n"))
	}

	if len(content) > maxReadBytes {
		return fmt.Sprintf("%s (truncated, head+tail kept — request a line range for the middle):\n%s",
			path, headTail(content, maxReadBytes))
	}
	return fmt.Sprintf("%s:\n%s", path, content)
}

// headTail shrinks s to about max bytes while keeping BOTH ends, with a marker
// for the elided middle, and snaps the cuts to line boundaries. Errors and
// summaries usually sit at the END of logs/command output, so a head-only cut
// (which is what a naive s[:max] does) would hide exactly what matters.
func headTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head
	if i := strings.LastIndexByte(s[:head], '\n'); i > 0 {
		head = i
	}
	tailStart := len(s) - tail
	if i := strings.IndexByte(s[tailStart:], '\n'); i >= 0 && i+1 < tail {
		tailStart += i + 1
	}
	elided := tailStart - head
	if elided <= 0 {
		return s
	}
	return fmt.Sprintf("%s\n… %s elided (head+tail kept) …\n%s", s[:head], byteCount(elided), s[tailStart:])
}

// byteCount formats a byte length compactly (e.g. 4.2K, 1.1M).
func byteCount(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}

func parseRange(s string, max int) (lo, hi int, ok bool) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	lo, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if e1 != nil || e2 != nil || lo < 1 || hi < lo {
		return 0, 0, false
	}
	if hi > max {
		hi = max
	}
	if lo > max {
		return 0, 0, false
	}
	return lo, hi, true
}

// grepTree walks path (default: jail root) and returns "file:line:text" matches
// for a regexp, skipping .git, heavy dependency dirs and obviously binary
// files. include, if set, is a file-name glob ("*.go", "*.{ts,tsx}"). Capped.
// allow, if non-nil, filters files by jail-relative path (per-file read rules).
func grepTree(j *Jail, pattern, path, include string, allow ...func(rel string) bool) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "error: bad pattern: " + err.Error()
	}
	if path == "" {
		path = j.Root
	}
	root, err := j.Resolve(path)
	if err != nil {
		return "error: " + err.Error()
	}

	var out []string
	count := 0
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || count >= maxGrepMatches {
			return nil
		}
		if d.IsDir() {
			if p != root && (d.Name() == ".git" || heavyDir[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if include != "" && !matchInclude(include, d.Name()) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || isBinary(data) {
			return nil
		}
		rel, _ := filepath.Rel(j.Root, p)
		for _, ok := range allow {
			if !ok(filepath.ToSlash(rel)) {
				return nil
			}
		}
		for i, ln := range strings.Split(string(data), "\n") {
			if re.MatchString(ln) {
				out = append(out, fmt.Sprintf("%s:%d:%s", rel, i+1, strings.TrimRight(ln, "\r")))
				count++
				if count >= maxGrepMatches {
					out = append(out, fmt.Sprintf("... (stopped at %d matches)", maxGrepMatches))
					break
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return "error: " + walkErr.Error()
	}
	if len(out) == 0 {
		return "no matches"
	}
	return strings.Join(out, "\n")
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// runCommand executes an allowlisted command with NO shell — argv is tokenized
// and exec'd directly, so pipes, redirects and substitutions are inert. Runs
// with cwd pinned to the jail root under a timeout. When live is non-nil the
// output is streamed LIVE (behind a dim gutter) as it is produced — so a slow
// command looks like it is working, not frozen — and Ctrl-C interrupts just
// this command, not the agent; subagents pass nil and are canceled through
// ctx. The output is always captured for the model. stdin is the null device,
// so a command that would wait for input gets EOF instead of hanging.
func runCommand(parent context.Context, j *Jail, cmdline string, timeout time.Duration, live io.Writer) string {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	if err := j.CheckCommand(cmdline); err != nil {
		return "error: " + err.Error()
	}
	cmd := commandFor(ctx, j, cmdline)

	cmd.Dir = j.Root
	cmd.Stdin = nil // null device → reads get EOF, no interactive hang
	inProcessGroup(cmd)

	var buf bytes.Buffer
	var pw *prefixWriter
	if live != nil {
		fmt.Fprintln(live, " "+faint("%s $ %s   %s", gNone, cmdline, faint("(Ctrl-C to interrupt)")))
		pw = &prefixWriter{w: live, prefix: "   " + cFaint + "│ " + cReset}
		mw := io.MultiWriter(&buf, pw)
		cmd.Stdout = mw
		cmd.Stderr = mw // same writer ⇒ os/exec serializes the two streams for us
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}

	var userInt atomic.Bool
	done := make(chan struct{})
	if live != nil {
		// Ctrl-C interrupts just this command (cancels its context), not the agent.
		sigch := make(chan os.Signal, 1)
		signal.Notify(sigch, os.Interrupt)
		defer signal.Stop(sigch)
		go func() {
			select {
			case <-sigch:
				userInt.Store(true)
				cancel()
			case <-done:
			}
		}()
	}

	err := cmd.Run()
	close(done)
	if pw != nil {
		pw.flush()
	}
	say := func(s string) {
		if live != nil {
			fmt.Fprintln(live, s)
		}
	}

	res := headTail(buf.String(), maxCmdOutput)
	switch {
	case userInt.Load() || parent.Err() != nil:
		say("   " + warn("%s interrupted", gDown))
		return res + "\n(interrupted by user)"
	case ctx.Err() == context.DeadlineExceeded:
		say("   " + warn("%s timed out after %s", gDown, timeout))
		return fmt.Sprintf("error: the command timed out after %s — retry with a larger timeout if it is expected to take longer\n", timeout) + res
	case err != nil:
		_, desc := exitInfo(err)
		say("   " + cRed + gDown + cReset + faint(" %s", desc))
		if strings.TrimSpace(res) == "" {
			return "(no output, " + desc + ")"
		}
		return res + "\n(" + desc + ")"
	default:
		say("   " + cGreen + gUp + cReset + faint(" exit 0"))
		if strings.TrimSpace(res) == "" {
			return "(no output, exit 0)"
		}
		return res
	}
}

// commandFor builds the process for a command line the sandbox has already
// accepted: through sh when the sandbox runs a shell (unsafe mode, or
// sandbox: {shell: true}), so pipes, redirects and && work; otherwise exec
// directly, with no shell to interpret anything.
func commandFor(ctx context.Context, j *Jail, cmdline string) *exec.Cmd {
	if j.Unsafe || j.Shell {
		return exec.CommandContext(ctx, "sh", "-c", cmdline)
	}
	argv, _ := tokenize(cmdline) // validated by CheckCommand
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

// exitInfo renders a failed command's exit for the model: the status number it
// can reason about, or the signal that killed it.
func exitInfo(err error) (int, string) {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return -1, "could not run: " + err.Error()
	}
	if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		return -1, fmt.Sprintf("killed by signal %d (%v)", int(st.Signal()), st.Signal())
	}
	return ee.ExitCode(), fmt.Sprintf("exit status %d", ee.ExitCode())
}

// execCheck runs a verifier command in the sandbox (allowlist + GPU policy,
// no approval prompt: the verifier is the harness, not the model) and returns
// its combined output and exit code. exit is -1 when it could not run or
// timed out.
func execCheck(parent context.Context, j *Jail, cmdline string, timeout time.Duration, live ...io.Writer) (string, int) {
	if err := j.CheckCommand(cmdline); err != nil {
		return "sandbox: " + err.Error(), -1
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := commandFor(ctx, j, cmdline)
	cmd.Dir = j.Root
	inProcessGroup(cmd)
	var buf bytes.Buffer
	sink := io.Writer(&buf)
	// A workflow step's output must reach run.log as it is produced: a run that
	// crashes mid-check is still diagnosable.
	if len(live) > 0 && live[0] != nil {
		sink = io.MultiWriter(&buf, live[0])
	}
	cmd.Stdout, cmd.Stderr = sink, sink // one writer ⇒ os/exec serializes the two streams
	err := cmd.Run()
	out := buf.String()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return out + fmt.Sprintf("\n(check timed out after %s)", timeout), -1
	case err == nil:
		return out, 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode()
	}
	return out + "\n" + err.Error(), -1
}

// prefixWriter writes each line of the command's live output behind a fixed
// prefix (a dim gutter), tracking whether it is mid-line across Writes.
type prefixWriter struct {
	w      io.Writer
	prefix string
	mid    bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	total := len(b)
	for len(b) > 0 {
		if !p.mid {
			io.WriteString(p.w, p.prefix)
			p.mid = true
		}
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			p.w.Write(b[:i+1])
			p.mid = false
			b = b[i+1:]
		} else {
			p.w.Write(b)
			b = nil
		}
	}
	return total, nil
}

func (p *prefixWriter) flush() {
	if p.mid {
		io.WriteString(p.w, "\n")
		p.mid = false
	}
}

// tokenize splits a command line into argv, honoring single/double quotes. It
// intentionally understands nothing else — no operators, no expansion.
func tokenize(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inWord := false
	quote := rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			inWord = true
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				argv = append(argv, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced quote in command")
	}
	if inWord {
		argv = append(argv, cur.String())
	}
	return argv, nil
}
