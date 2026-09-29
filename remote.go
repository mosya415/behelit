package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Working on another machine. With a remote configured, the project lives
// there: the file tools read and write over ssh, commands and the verifier run
// there, and nothing listens locally — ssh is an outbound connection using the
// keys and ~/.ssh/config you already have.
//
//	remote:
//	  host: cab-node               # ssh target (alias from ~/.ssh/config works)
//	  dir: /home/u/llmbench        # the project on that machine
//	  ssh: [-o, BatchMode=yes]     # extra ssh options (optional)
//
// Or LCA_REMOTE=cab-node:/home/u/llmbench.
//
// The sandbox still applies: the allowlist is checked against the command we
// send, paths stay inside dir, and GPU work still has to go through bsk (a
// remote srun is refused just like a local one).

type Remote struct {
	Host string
	Dir  string
	SSH  []string // the transport command; default: ssh -o BatchMode=yes
}

func (r *Remote) sshCmd() []string {
	if len(r.SSH) > 0 {
		return r.SSH
	}
	return []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
}

// Label is how the remote is shown in the UI.
func (r *Remote) Label() string {
	switch {
	case r == nil:
		return ""
	case r.Host == "":
		return r.Dir
	}
	return r.Host + ":" + r.Dir
}

// Where names the machine in messages ("cab-node", or "the remote").
func (r *Remote) Where() string {
	if r == nil || r.Host == "" {
		return "the remote machine"
	}
	return r.Host
}

// parseRemote reads the roles.yaml block, then LCA_REMOTE (which wins).
func parseRemote(n *yNode) (*Remote, error) {
	var r *Remote
	if n != nil {
		r = &Remote{Host: n.str("host"), Dir: n.str("dir")}
		if s := n.child("ssh"); s != nil {
			r.SSH = listOrCSV(s)
		}
	}
	if v := strings.TrimSpace(os.Getenv("LCA_REMOTE")); v != "" {
		host, dir, ok := strings.Cut(v, ":")
		if !ok {
			return nil, fmt.Errorf("LCA_REMOTE must be host:/path, got %q", v)
		}
		r = &Remote{Host: host, Dir: dir}
	}
	if r == nil {
		return nil, nil
	}
	if r.Dir == "" {
		return nil, fmt.Errorf("remote: dir is required (the project's path on %s)", firstNonEmpty(r.Host, "the remote"))
	}
	if !strings.HasPrefix(r.Dir, "/") && !strings.HasPrefix(r.Dir, "~") {
		return nil, fmt.Errorf("remote: dir must be absolute, got %q", r.Dir)
	}
	return r, nil
}

// argv builds the local command that runs script on the remote. A host may be
// empty when the transport already names the target (or in tests).
func (r *Remote) argv(script string) []string {
	out := append([]string(nil), r.sshCmd()...)
	if r.Host != "" {
		out = append(out, r.Host)
	}
	return append(out, script)
}

// script wraps a command so it runs in the project directory.
func (r *Remote) script(cmd string) string {
	return "cd " + shellQuote(r.Dir) + " && " + cmd
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// run executes a command in the project directory and returns its combined
// output and exit code (-1 when it could not run). stdin, if non-nil, is fed to
// the remote command.
func (r *Remote) run(ctx context.Context, cmd string, timeout time.Duration, stdin []byte, live io.Writer) (string, int) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := r.argv(r.script(cmd))
	c := exec.CommandContext(cctx, argv[0], argv[1:]...)
	inProcessGroup(c)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var buf bytes.Buffer
	if live != nil {
		pw := &prefixWriter{w: live, prefix: "   " + cFaint + "│ " + cReset}
		c.Stdout, c.Stderr = io.MultiWriter(&buf, pw), io.MultiWriter(&buf, pw)
		defer pw.flush()
	} else {
		c.Stdout, c.Stderr = &buf, &buf
	}
	err := c.Run()
	out := buf.String()
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		return out + fmt.Sprintf("\n(timed out after %s)", timeout), -1
	case err == nil:
		return out, 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// 255 is ssh's own failure (host unreachable, auth), not the command's
		if ee.ExitCode() == 255 && strings.Contains(out, "ssh") {
			return out + "\n(ssh could not run the command on " + r.Host + ")", -1
		}
		return out, ee.ExitCode()
	}
	return out + "\n" + err.Error(), -1
}

// capture runs a command and returns its output, or an "error: …" string in the
// shape the tools return.
func (r *Remote) capture(ctx context.Context, cmd string) (string, string) {
	out, exit := r.run(ctx, cmd, 2*time.Minute, nil, nil)
	if exit != 0 {
		return "", "error: " + strings.TrimSpace(lastLines(out, 5, 500))
	}
	return out, ""
}

// ── path safety ─────────────────────────────────────────────────────────────

// relPath keeps a tool-supplied path inside the project: relative, cleaned, no
// "..", no absolute paths. It is the remote equivalent of the jail.
// remotePath is the "./x" form used in remote commands, always cleaned.
func remotePath(rel string) string {
	if rel == "" || rel == "." {
		return "."
	}
	return "./" + path.Clean(rel)
}

func (r *Remote) relPath(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("path %q must be relative to the project directory", p)
	}
	if strings.HasPrefix(p, "/") {
		rel, err := filepathRel(r.Dir, p)
		if err != nil {
			return "", fmt.Errorf("path %q is outside the project directory %s", p, r.Dir)
		}
		p = rel
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes the project directory", p)
	}
	return clean, nil
}

// filepathRel is path.Rel for slash paths (the remote is unix).
func filepathRel(base, target string) (string, error) {
	base, target = path.Clean(base), path.Clean(target)
	if base == target {
		return ".", nil
	}
	if !strings.HasPrefix(target, base+"/") {
		return "", fmt.Errorf("outside")
	}
	return strings.TrimPrefix(target, base+"/"), nil
}

// ── file tools over ssh ─────────────────────────────────────────────────────

func (r *Remote) readFile(ctx context.Context, p, lines string) string {
	rel, err := r.relPath(p)
	if err != nil {
		return "error: " + err.Error()
	}
	q := shellQuote(remotePath(rel))
	if lines != "" {
		out, errs := r.capture(ctx, "cat -- "+q)
		if errs != "" {
			return errs
		}
		all := strings.Split(out, "\n")
		lo, hi, ok := parseRange(lines, len(all))
		if !ok {
			return fmt.Sprintf("error: bad lines=%q (want e.g. 10-40)", lines)
		}
		return fmt.Sprintf("%s (lines %d-%d):\n%s", p, lo, hi, strings.Join(all[lo-1:hi], "\n"))
	}
	// head+tail in one go: the remote sends at most maxReadBytes+1
	out, errs := r.capture(ctx, fmt.Sprintf("head -c %d -- %s", maxReadBytes+1, q))
	if errs != "" {
		return errs
	}
	if len(out) > maxReadBytes {
		return fmt.Sprintf("%s (truncated, head kept — request a line range for the rest):\n%s", p, out[:maxReadBytes])
	}
	return fmt.Sprintf("%s:\n%s", p, out)
}

func (r *Remote) listDir(ctx context.Context, p string) string {
	rel, err := r.relPath(p)
	if err != nil {
		return "error: " + err.Error()
	}
	cmd := fmt.Sprintf("find %s -name .git -prune -o -print 2>/dev/null | sed -n '2,%dp'", shellQuote(remotePath(rel)), maxListEntries+1)
	out, errs := r.capture(ctx, cmd)
	if errs != "" {
		return errs
	}
	if strings.TrimSpace(out) == "" {
		return strings.TrimRight(p, "/") + "/\n(empty directory)"
	}
	return strings.TrimRight(p, "/") + "/\n" + out
}

// cleanRemoteRel turns find's output ("././pkg/a.go") into a plain relative path.
func cleanRemoteRel(p string) string {
	c := path.Clean(strings.TrimSpace(p))
	return strings.TrimPrefix(c, "./")
}

// remoteFiles lists the project's files with modification times, for glob.
func (r *Remote) files(ctx context.Context, sub string) ([]remoteFile, string) {
	prune := ""
	for _, d := range sortedKeys(heavyDir) {
		prune += fmt.Sprintf(" -name %s -prune -o", d)
	}
	cmd := fmt.Sprintf("find %s%s -type f -printf '%%T@ %%p\\n' 2>/dev/null | head -n 20000", shellQuote(remotePath(sub)), prune)
	out, errs := r.capture(ctx, cmd)
	if errs != "" || strings.TrimSpace(out) == "" {
		// BSD find has no -printf: fall back without times
		cmd = fmt.Sprintf("find %s%s -type f 2>/dev/null | head -n 20000", shellQuote(remotePath(sub)), prune)
		out, errs = r.capture(ctx, cmd)
		if errs != "" {
			return nil, errs
		}
		var fs []remoteFile
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l != "" {
				fs = append(fs, remoteFile{path: cleanRemoteRel(l)})
			}
		}
		return fs, ""
	}
	var fs []remoteFile
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		ts, p, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok {
			continue
		}
		secs, _ := strconv.ParseFloat(ts, 64)
		fs = append(fs, remoteFile{path: cleanRemoteRel(p), mtime: secs})
	}
	return fs, ""
}

type remoteFile struct {
	path  string
	mtime float64
}

func (r *Remote) glob(ctx context.Context, pattern, p string) string {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	if pattern == "" {
		return "error: pattern is required"
	}
	re, err := globToRegexp(pattern)
	if err != nil {
		return "error: bad glob: " + err.Error()
	}
	rel, err := r.relPath(p)
	if err != nil {
		return "error: " + err.Error()
	}
	files, errs := r.files(ctx, rel)
	if errs != "" {
		return errs
	}
	baseOnly := !strings.Contains(pattern, "/")
	var hits []remoteFile
	for _, f := range files {
		subject := f.path
		if rel != "." {
			subject = strings.TrimPrefix(subject, rel+"/")
		}
		if baseOnly {
			subject = path.Base(subject)
		}
		if re.MatchString(subject) {
			hits = append(hits, f)
		}
	}
	if len(hits) == 0 {
		return "no files found"
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].mtime > hits[b].mtime })
	var out strings.Builder
	for i, h := range hits {
		if i == maxGlobResults {
			fmt.Fprintf(&out, "(results truncated: showing first %d of %d)\n", maxGlobResults, len(hits))
			break
		}
		out.WriteString(h.path + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

var reGrepMeta = regexp.MustCompile(`\(\?[a-zA-Z]`)

func (r *Remote) grep(ctx context.Context, pattern, p, include string, allow func(string) bool) string {
	if pattern == "" {
		return "error: pattern is required"
	}
	if reGrepMeta.MatchString(pattern) {
		return "error: this pattern uses Go-specific regexp syntax that the remote grep doesn't support — use a plain regexp"
	}
	rel, err := r.relPath(p)
	if err != nil {
		return "error: " + err.Error()
	}
	inc := ""
	if include != "" {
		inc = " --include=" + shellQuote(include)
	}
	cmd := fmt.Sprintf("grep -rnIE%s --exclude-dir=.git -e %s -- %s 2>/dev/null | head -n %d",
		inc, shellQuote(pattern), shellQuote(remotePath(rel)), maxGrepMatches+1)
	out, exit := r.run(ctx, cmd, 2*time.Minute, nil, nil)
	if exit > 1 { // 1 = no matches
		return "error: " + strings.TrimSpace(lastLines(out, 5, 500))
	}
	var kept []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		file := cleanRemoteRel(strings.SplitN(l, ":", 2)[0])
		if allow != nil && !allow(file) {
			continue
		}
		kept = append(kept, strings.TrimPrefix(strings.TrimPrefix(l, "./"), "./"))
		if len(kept) >= maxGrepMatches {
			kept = append(kept, fmt.Sprintf("... (stopped at %d matches)", maxGrepMatches))
			break
		}
	}
	if len(kept) == 0 {
		return "no matches"
	}
	return strings.Join(kept, "\n")
}

// stat returns the file's size and mtime (unix seconds), or exists=false.
func (r *Remote) stat(ctx context.Context, rel string) (mtime int64, exists bool) {
	out, exit := r.run(ctx, "stat -c %Y -- "+shellQuote(remotePath(rel))+" 2>/dev/null || stat -f %m -- "+shellQuote(remotePath(rel)), 30*time.Second, nil, nil)
	if exit != 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(lastLines(out, 1, 64)), 10, 64)
	if err != nil {
		return 0, true
	}
	return n, true
}

// write replaces a file's contents (creating parent directories).
func (r *Remote) write(ctx context.Context, rel, content string) string {
	dir := path.Dir(rel)
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(remotePath(dir)), shellQuote(remotePath(rel)))
	out, exit := r.run(ctx, cmd, 2*time.Minute, []byte(content), nil)
	if exit != 0 {
		return "error: " + strings.TrimSpace(lastLines(out, 5, 500))
	}
	return ""
}

// read returns a file's whole contents for editing.
func (r *Remote) read(ctx context.Context, rel string) (string, string) {
	out, errs := r.capture(ctx, "cat -- "+shellQuote(remotePath(rel)))
	return out, errs
}
