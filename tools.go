package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxReadBytes   = 200_000
	maxGrepMatches = 200
	maxCmdOutput   = 64_000
	cmdTimeout     = 60 * time.Second
)

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
		return fmt.Sprintf("%s (truncated to %d bytes — request a line range for more):\n%s",
			path, maxReadBytes, content[:maxReadBytes])
	}
	return fmt.Sprintf("%s:\n%s", path, content)
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
// for a regexp, skipping .git and obviously binary files. Capped.
func grepTree(j *Jail, pattern, path string) string {
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
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || isBinary(data) {
			return nil
		}
		rel, _ := filepath.Rel(j.Root, p)
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
// with cwd pinned to the jail root under a timeout.
func runCommand(j *Jail, cmdline string) string {
	argv, err := tokenize(cmdline)
	if err != nil {
		return "error: " + err.Error()
	}
	if len(argv) == 0 {
		return "error: empty command"
	}
	if !j.AllowCommand(argv[0]) {
		return fmt.Sprintf("error: command %q is not on the allowlist", argv[0])
	}

	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = j.Root
	out, err := cmd.CombinedOutput()

	res := string(out)
	if len(res) > maxCmdOutput {
		res = res[:maxCmdOutput] + "\n... (output truncated)"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return res + fmt.Sprintf("\n(command timed out after %s)", cmdTimeout)
	}
	if err != nil {
		return res + "\n(exit: " + err.Error() + ")"
	}
	if res == "" {
		return "(no output, exit 0)"
	}
	return res
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
