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
	"sync"
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
// Or LCA_REMOTE=cab-node:/home/u/llmbench. `members:` in roles.yaml is the same
// thing per machine (members.go); this block is the one-machine spelling and
// means a member named "remote".
//
// The sandbox still applies: the allowlist is checked against the command we
// send, paths stay inside dir, and GPU work still has to go through bsk (a
// remote srun is refused just like a local one).

type Remote struct {
	Name string // the member's name, so a failure can say which machine it was
	Host string
	Dir  string
	SSH  []string // extra flags for the default transport, or the whole transport argv
	Src  string   // the roles.yaml that declared it, so a failure names the line to fix
	up   *reach   // shared by withDir copies: it is one machine
}

// sshCmd builds the transport. SSH is either extra flags for the default
// transport (its first element starts with "-") or the whole transport command.
// Both spellings are live: the first is what this file's own documentation
// always promised and what an operator writes, and the second is what lets a
// test stand in for ssh with [sh, -c] — the only network-free harness there is.
// Returning r.SSH as the whole argv for a flag list would exec("-o", …).
func (r *Remote) sshCmd() []string {
	def := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	switch {
	case len(r.SSH) == 0:
		return def
	case strings.HasPrefix(r.SSH[0], "-"):
		return append(def, r.SSH...)
	}
	return r.SSH
}

// memberName names this machine in a message; "" happens for a Remote built
// before members existed (LCA_REMOTE, a test), and it is the legacy member.
func (r *Remote) memberName() string {
	if r == nil || r.Name == "" {
		return legacyMemberName
	}
	return r.Name
}

// ── reachability ────────────────────────────────────────────────────────────

// reach is a member's reachability. Success is cached until the transport itself
// fails (forget); failure is cached for memberRetryAfter, so a laptop that joins
// the VPN mid-session recovers without a restart, a machine that is down is not
// re-probed on every tool call, and one that dies mid-run is not still reported
// as ok. There is no else-branch anywhere below: a failed gate fails
// the caller, and no code path chooses a different machine. Silently running on
// the wrong machine is the single worst thing this feature could do, so it is
// made structurally impossible rather than merely avoided.
type reach struct {
	mu   sync.Mutex
	ok   bool
	err  error
	last time.Time
}

const memberRetryAfter = 30 * time.Second

// memberDown is why a member cannot be used, and WHICH of the two things the
// probe proves failed. The fixes have nothing in common — ssh (keys,
// ~/.ssh/config, VPN) or the directory (mkdir, clone, a corrected dir:) — so the
// distinction travels in the error instead of being re-read out of its text.
type memberDown struct {
	dirGone bool // ssh answered; the shell over there could not cd into Dir
	msg     string
}

func (e *memberDown) Error() string { return e.msg }

// memberDirMissing reports the second case, which is what doctor's hint turns on.
func memberDirMissing(err error) bool {
	var d *memberDown
	return errors.As(err, &d) && d.dirGone
}

// reachAlloc guards the lazy creation of Remote.up. A Remote is shared by every
// session pinned to that member, so two of them may gate at once.
var reachAlloc sync.Mutex

func (r *Remote) gate() *reach {
	reachAlloc.Lock()
	defer reachAlloc.Unlock()
	if r.up == nil {
		r.up = &reach{}
	}
	return r.up
}

// probeTimeout bounds the probe itself. On top of the default transport's
// BatchMode=yes it can never hang on a password prompt.
func (r *Remote) probeTimeout() time.Duration {
	if n := atoiDefault(os.Getenv("LCA_MEMBER_PROBE"), 0); n > 0 {
		return time.Duration(n) * time.Second
	}
	return 10 * time.Second
}

// ensureUp proves the member is reachable and its project directory exists —
// exec wraps every command in `cd <Dir> && …`, so one `pwd` proves both. Which of
// the two failed is in the exit code: 255 is ssh's own code and -1 is "could not
// run it at all", so the far side never answered; any other code came from the
// shell over there, which answered and could not cd. Blaming ssh for a missing
// directory sends the operator after keys and a VPN when the fix is a mkdir.
func (r *Remote) ensureUp(ctx context.Context) error {
	g := r.gate()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ok {
		return nil
	}
	if g.err != nil && time.Since(g.last) < memberRetryAfter {
		return g.err
	}
	out, exit := r.exec(ctx, "pwd", r.probeTimeout(), nil, nil)
	g.last = time.Now()
	switch {
	case exit == 0:
		g.ok, g.err = true, nil
		return nil
	case exit == 255 || exit == -1:
		g.err = &memberDown{msg: fmt.Sprintf(
			"member %s (%s) is unreachable: %s\nnothing ran there, and nothing runs on another machine instead — fix ssh (keys, ~/.ssh/config, VPN), or point this role or step at another member",
			r.memberName(), r.Label(), probeFail(out)) + r.declaredIn()}
	default:
		g.err = &memberDown{dirGone: true, msg: fmt.Sprintf(
			"member %s: ssh to %s works, but %s is not there: %s\nnothing ran there, and nothing runs on another machine instead — create or clone the project at that path, or point this member's dir: at it",
			r.memberName(), r.Where(), r.Dir, probeFail(out)) + r.declaredIn()}
	}
	return g.err
}

// declaredIn names the file the member came from. Three roles.yaml files merge
// into one team, so "which line do I fix" is not answerable from the member's
// name alone — and a stale member in the project's own .lca/roles.yaml becomes
// every role's default machine without ever being mentioned again.
func (r *Remote) declaredIn() string {
	if r == nil || r.Src == "" {
		return ""
	}
	return "\ndeclared in " + r.Src
}

// forget drops a cached "ok" once the transport has failed mid-session: a
// machine that answered an hour ago proves nothing now, and a stale gate is what
// turns ssh's own exit into "your check failed" and leaves /members and doctor
// printing ok for a machine that is gone. It records no error of its own — the
// next ensureUp probes and produces the authoritative one, so a command that
// merely looked like an ssh failure costs one probe instead of declaring a
// healthy member down for the whole retry window.
func (r *Remote) forget() {
	g := r.gate()
	g.mu.Lock()
	g.ok, g.err = false, nil
	g.mu.Unlock()
}

// sshOwnFailure recognises the transport's own complaint as opposed to a remote
// command that happens to exit 255: ssh reuses the remote command's exit code for
// everything else, so its own diagnostics are the only signal there is.
func sshOwnFailure(out string) bool {
	if strings.Contains(out, "ssh") { // "ssh: connect to host …", "ssh: Could not resolve …"
		return true
	}
	for _, s := range []string{"Permission denied (publickey", "Host key verification failed",
		"kex_exchange_identification", "Connection closed by "} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}

// probeFail compresses the transport's own complaint to one line: exec appends a
// note of its own about ssh, and a blank line between the two would put the fix
// hint a screen away from the cause it belongs to.
func probeFail(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "(ssh could not run the command") {
			continue
		}
		keep = append(keep, l)
	}
	if len(keep) > 2 {
		keep = keep[len(keep)-2:]
	}
	if len(keep) == 0 {
		return "ssh gave no reason"
	}
	return truncate(strings.Join(keep, " · "), 300)
}

// withDir is the same machine, another directory (a delegation's worktree over
// there). The reachability state is shared, because it is one machine.
func (r *Remote) withDir(dir string) *Remote {
	g := r.gate()
	c := *r
	c.Dir, c.up = dir, g
	return &c
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
	r.Name = legacyMemberName // the remote: block IS the member named "remote"
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

// run is the single funnel every remote operation passes through — the file
// tools, the verifier, the workflow runner, doctor — which is why the
// reachability gate lives here and not in each of them. A failed gate returns
// (message, -1), the shape every caller already treats as "could not run".
func (r *Remote) run(ctx context.Context, cmd string, timeout time.Duration, stdin []byte, live io.Writer) (string, int) {
	if err := r.ensureUp(ctx); err != nil {
		return err.Error(), -1
	}
	out, exit, down := r.execT(ctx, cmd, timeout, stdin, live)
	if down {
		r.forget()
	}
	return out, exit
}

// spawn is the one place a transport process is started. exec and plumb differ
// only in how they frame the script and shape the result, and a fix to the
// timeout, the process group or the exit code has to reach both of them: two
// copies is how the git-plumbing path ends up reporting a raw exit code where a
// tool call reports "ssh could not run the command on …". stdout and stderr are
// the caller's, because plumb has to keep them apart — a git warning spliced into
// the middle of a patch is a patch that cannot apply.
func (r *Remote) spawn(ctx context.Context, script string, stdin []byte, stdout, stderr io.Writer, timeout time.Duration) (exit int, timedOut bool) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := r.argv(script)
	c := exec.CommandContext(cctx, argv[0], argv[1:]...)
	inProcessGroup(c)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	c.Stdout, c.Stderr = stdout, stderr
	err := c.Run()
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		return -1, true
	case err == nil:
		return 0, false
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), false
	}
	// The transport could not be started at all (no ssh on PATH, fork failure):
	// there is no exit code, and -1 is what every caller reads as "could not run".
	fmt.Fprintf(stderr, "\n%s", err.Error())
	return -1, false
}

// exec runs a command in the project directory and returns its combined output
// and exit code (-1 when it could not run). stdin, if non-nil, is fed to the
// remote command. It is deliberately ungated: ensureUp itself calls it.
func (r *Remote) exec(ctx context.Context, cmd string, timeout time.Duration, stdin []byte, live io.Writer) (string, int) {
	out, exit, _ := r.execT(ctx, cmd, timeout, stdin, live)
	return out, exit
}

// execT is exec plus whether the TRANSPORT failed — ssh never ran the command.
// run turns that into forget(), because the reachability gate is only as true as
// its last answer.
func (r *Remote) execT(ctx context.Context, cmd string, timeout time.Duration, stdin []byte, live io.Writer) (string, int, bool) {
	var buf bytes.Buffer
	var w io.Writer = &buf
	if live != nil {
		pw := &prefixWriter{w: live, prefix: "   " + cFaint + "│ " + cReset}
		w = io.MultiWriter(&buf, pw)
		defer pw.flush()
	}
	exit, timedOut := r.spawn(ctx, r.script(cmd), stdin, w, w, timeout)
	out := buf.String()
	switch {
	case timedOut:
		return out + fmt.Sprintf("\n(timed out after %s)", timeout), -1, false
	case exit == 0:
		return out, 0, false
	case exit == 255 && sshOwnFailure(out):
		// 255 is ssh's own failure (host unreachable, auth), not the command's
		return out + "\n(ssh could not run the command on " + r.Host + ")", -1, true
	case exit == -1:
		return out, -1, true
	}
	return out, exit, false
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

// plumb runs one of lca's OWN scripts on the member: the git dance that makes a
// delegation's worktree and the diff it brings back. Two reasons it is not run:
//
//   - stdout and stderr come back SEPARATELY. run merges them into one buffer,
//     which would splice a git warning into the middle of a patch.
//   - it bypasses CheckCommand for the same reason the local path's gitCmd does:
//     every byte is a constant or a path lca computed. Nothing model-supplied may
//     ever reach it, and it is not reachable from a tool.
//
// It still passes the reachability gate. The cd is `|| exit 98`, not
// Remote.script's `cd d && cmd`, whose && would bind to the script's first line
// only and run the rest even when the cd failed.
func (r *Remote) plumb(ctx context.Context, script string, stdin []byte, timeout time.Duration) (stdout, stderr string, exit int) {
	if err := r.ensureUp(ctx); err != nil {
		return "", err.Error(), -1
	}
	var out, errb bytes.Buffer
	code, timedOut := r.spawn(ctx, "cd "+shellQuote(r.Dir)+" || exit 98\n"+script, stdin, &out, &errb, timeout)
	switch {
	case timedOut:
		return out.String(), errb.String() + fmt.Sprintf("\n(timed out after %s)", timeout), -1
	case code == -1, code == 255 && sshOwnFailure(errb.String()):
		// The transport, not the script: the same note a tool call gets, and the
		// gate goes with it so the next caller is told about the machine.
		r.forget()
		return out.String(), errb.String() + "\n(ssh could not run the command on " + r.Where() + ")", -1
	}
	return out.String(), errb.String(), code
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
