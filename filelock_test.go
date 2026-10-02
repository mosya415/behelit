package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Concurrent-edit protection. Every test here is about one of the ways two
// writers used to land on one file with the loser never told — a sibling writing
// during an approval prompt, two children both creating a file, a `task` child
// editing a file only its parent had read, /undo discarding a subagent's work.
// They are written against the PUBLIC path (the tools, as the model calls them)
// wherever that is possible, because the defects were all in the sequencing
// around the tools, not in the primitives.

func leaseHarness(t *testing.T) *harness {
	t.Helper()
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "ok"} })
	return newHarness(t, fs.URL, "native", true)
}

// leaseOrch is the smallest Orchestrator that can hand out file leases: a jail
// for the root and a config dir for the fallback lock directory.
func leaseOrch(t *testing.T, root string) *Orchestrator {
	t.Helper()
	jl, err := NewJail(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return &Orchestrator{cfg: Config{Root: root, Dir: filepath.Join(root, ".lcastate")}, jl: jl}
}

func toolWrite(s *Session, path, content string) string {
	return runWriteTool(&ToolCtx{Ctx: context.Background(), S: s, Name: "write"}, Args{"path": path, "content": content})
}

func toolEdit(s *Session, path, oldS, newS string) string {
	return runEditTool(&ToolCtx{Ctx: context.Background(), S: s, Name: "edit"}, Args{"path": path, "old_string": oldS, "new_string": newS})
}

func toolRead(s *Session, path, lines string) string {
	a := Args{"path": path}
	if lines != "" {
		a["lines"] = lines
	}
	return resolveToolName("read_file").Run(&ToolCtx{Ctx: context.Background(), S: s, Name: "read_file"}, a)
}

// askingHarness swaps in an approver that really asks, reading its answer from a
// pipe this test controls. That is the only way to be INSIDE the approval window
// — the one place where the file used to be overwritten without a word.
type askingHarness struct {
	*harness
	in     *Input
	answer *os.File
}

func newAskingHarness(t *testing.T) *askingHarness {
	t.Helper()
	h := leaseHarness(t)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pw.Close(); pr.Close() })
	in := &Input{f: pr}
	h.orch.ap = NewApprover(in) // trusts nothing: every edit asks
	// The door is drawn on stdout; a test does not need to look at it.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = saved; devnull.Close() })
	return &askingHarness{harness: h, in: in, answer: pw}
}

// waitAsking blocks until the prompt is actually waiting for a human. Input
// holds readMu for the duration of a blocking read, which is the only
// observable "the question is on the screen right now" — and the whole point of
// these tests is to act while it is.
func waitAsking(t *testing.T, in *Input) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if !in.readMu.TryLock() {
			return
		}
		in.readMu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the approval prompt never reached its read")
}

// TestWriteAcrossApprovalLosesNothing is the silent loss itself: `write` checked
// the mtime, asked the human, and then called os.WriteFile without ever looking
// again. A sibling that wrote during the question was erased. It must now fail.
func TestWriteAcrossApprovalLosesNothing(t *testing.T) {
	resetChanges()
	defer resetChanges()
	h := newAskingHarness(t)
	p := filepath.Join(h.root, "a.txt")
	if err := os.WriteFile(p, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolRead(h.sess, "a.txt", "")

	done := make(chan string, 1)
	go func() { done <- toolWrite(h.sess, "a.txt", "mine\n") }()
	waitAsking(t, h.in)

	// While the human is being asked, the lease MUST be free: one held across the
	// question would stall every other session for as long as the human is away.
	start := time.Now()
	release, err := h.orch.leaseFile("a.txt", p)
	if err != nil {
		t.Fatalf("the lease was held across the approval prompt: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("taking the lease during the prompt took %s", took)
	}
	if err := writeFileAtomic(p, []byte("the rival's bytes\n")); err != nil {
		t.Fatal(err)
	}
	release()
	fmt.Fprintln(h.answer, "y")

	res := <-done
	if !strings.Contains(res, "was modified since it was last read") {
		t.Fatalf("the write was allowed to erase a rival's bytes: %q", res)
	}
	if got, _ := os.ReadFile(p); string(got) != "the rival's bytes\n" {
		t.Fatalf("the rival's bytes did not survive: %q", got)
	}
}

// TestEditAcrossApprovalIsRefused pins the same window for `edit`. `edit` used
// to survive it by ACCIDENT — applyEditMode re-reads, and fuzzyReplace errors
// when the search text is gone — which is no protection at all when the rival
// touched a different part of the file. Now it is refused deliberately.
func TestEditAcrossApprovalIsRefused(t *testing.T) {
	resetChanges()
	defer resetChanges()
	h := newAskingHarness(t)
	p := filepath.Join(h.root, "a.txt")
	if err := os.WriteFile(p, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolRead(h.sess, "a.txt", "")

	done := make(chan string, 1)
	go func() { done <- toolEdit(h.sess, "a.txt", "two", "TWO") }()
	waitAsking(t, h.in)
	// A DIFFERENT region, so the search text is still there and the edit would
	// have applied cleanly on top of work it never saw.
	if err := writeFileAtomic(p, []byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(h.answer, "y")

	res := <-done
	if !strings.Contains(res, "was modified since it was last read") {
		t.Fatalf("edit applied over a rival's change: %q", res)
	}
	if got, _ := os.ReadFile(p); string(got) != "one\ntwo\nthree\nfour\n" {
		t.Fatalf("the file moved under a refused edit: %q", got)
	}
}

// TestCreateRaceIsReported closes builtin_tools' create path, which skipped the
// staleness check entirely: two writers that both saw "does not exist" both
// wrote, and the loser was never told.
func TestCreateRaceIsReported(t *testing.T) {
	resetChanges()
	defer resetChanges()
	h := newAskingHarness(t)
	p := filepath.Join(h.root, "new.txt")

	done := make(chan string, 1)
	go func() { done <- toolWrite(h.sess, "new.txt", "mine\n") }()
	waitAsking(t, h.in)
	if err := os.WriteFile(p, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(h.answer, "y")

	res := <-done
	if !strings.Contains(res, "was created by someone else while you were composing it") {
		t.Fatalf("a create race was not reported: %q", res)
	}
	if got, _ := os.ReadFile(p); string(got) != "theirs\n" {
		t.Fatalf("the winner's file was overwritten: %q", got)
	}
}

// TestCreateIsExclusive is the primitive under that: a complete temp file hard
// linked into place, so the name either appears holding the whole content or
// does not appear at all.
func TestCreateIsExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := createFileAtomic(p, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	err := createFileAtomic(p, []byte("second\n"))
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("a second create must report ErrExist, got %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "first\n" {
		t.Fatalf("the loser overwrote the winner: %q", got)
	}
}

// TestParallelWritersNoneLost is the stress test: two sessions, one file, every
// write through the public tool path. What it proves is that no write is LOST
// and every refusal is told — over 400 interleaved writes, each result is either
// "wrote" or a refusal the model can act on, and the final file is one version.
//
// It does NOT prove atomicity, and its torn-read counter below cannot fire at
// this size: a version is 640 bytes, which one write(2) delivers whole, so the
// test stays green against a plain os.WriteFile. The atomicity claim belongs to
// TestAtomicWriteIsNeverHalfVisible, whose 4 MiB versions really can be caught
// half written; the counter is kept here only because it costs nothing and would
// catch a splice this shape happened to produce.
func TestParallelWritersNoneLost(t *testing.T) {
	resetChanges()
	defer resetChanges()
	h := leaseHarness(t)
	second, err := h.orch.NewPrimary("build", "", &quietView{})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(h.root, "hot.txt")
	if err := os.WriteFile(p, []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const rounds = 200
	version := func(tag string, n int) string {
		return strings.Repeat(fmt.Sprintf("%s-%04d\n", tag, n), 64)
	}
	// whole reports whether s is one version and not a splice of two.
	whole := func(s string) bool {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		for _, l := range lines {
			if l != lines[0] {
				return false
			}
		}
		return true
	}

	var mu sync.Mutex
	wrote, refused, torn := 0, 0, 0
	var unexpected []string
	run := func(s *Session, tag string) {
		for n := 0; n < rounds; n++ {
			if body, ok := wholeRead("hot.txt", toolRead(s, "hot.txt", "")); ok && !whole(body) {
				mu.Lock()
				torn++
				mu.Unlock()
			}
			res := toolWrite(s, "hot.txt", version(tag, n))
			mu.Lock()
			switch {
			case strings.HasPrefix(res, "wrote "):
				wrote++
			case strings.Contains(res, "was modified since it was last read"),
				strings.Contains(res, "retry the change in a moment"):
				refused++
			default:
				unexpected = append(unexpected, res)
			}
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	for _, pair := range []struct {
		s   *Session
		tag string
	}{{h.sess, "aaa"}, {second, "bbb"}} {
		wg.Add(1)
		go func(s *Session, tag string) { defer wg.Done(); run(s, tag) }(pair.s, pair.tag)
	}
	wg.Wait()

	if len(unexpected) > 0 {
		t.Fatalf("%d writes failed for an unexpected reason, first: %q", len(unexpected), unexpected[0])
	}
	if torn > 0 {
		t.Fatalf("%d reads saw a spliced file at a size one write(2) delivers whole: %s", torn, "something is wrong beyond atomicity")
	}
	if wrote == 0 {
		t.Fatal("no write landed at all")
	}
	// Every refusal is a loss PREVENTED and told about. Zero of them over 400
	// interleaved writes would mean the two goroutines never actually met.
	if refused == 0 {
		t.Fatal("not one write was refused: the two writers never collided, so nothing was proved")
	}
	final, _ := os.ReadFile(p)
	if !whole(string(final)) {
		t.Fatalf("the final file is a splice:\n%s", headTail(string(final), 200))
	}
}

// TestAtomicWriteIsNeverHalfVisible proves the rename, not the guard: a reader
// racing a writer sees one whole version or the other, never a truncated file.
// os.WriteFile truncates and then streams, which is a window of arbitrary length
// holding a file that is neither version.
func TestAtomicWriteIsNeverHalfVisible(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.bin")
	const size = 4 << 20
	if err := os.WriteFile(p, bytes.Repeat([]byte{'a'}, size), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c := byte('a' + i%4)
			if err := writeFileAtomic(p, bytes.Repeat([]byte{c}, size)); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if len(data) != size {
			t.Fatalf("read %d saw %d bytes, want %d — a half-written file", i, len(data), size)
		}
		if bytes.Count(data, data[:1]) != size {
			t.Fatalf("read %d saw two versions spliced together", i)
		}
	}
	close(stop)
	wg.Wait()
}

// TestAtomicWriteKeepsModeAndSymlink: os.WriteFile's perm applies only to a file
// it creates, so today's mode survived an overwrite by accident. os.Rename
// preserves nothing, so both of these had to become deliberate.
func TestAtomicWriteKeepsModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(script, []byte("#!/bin/sh\necho hi\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755 — an executable came back unrunnable", info.Mode().Perm())
	}
	target := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeFileAtomic(link, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if li, err := os.Lstat(link); err != nil || li.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the rename replaced the user's symlink with a regular file (%v)", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new\n" {
		t.Fatalf("the symlink's target was not written: %q", got)
	}
	if ti, err := os.Stat(target); err != nil || ti.Mode().Perm() != 0o640 {
		t.Fatalf("mode through a symlink = %v", ti.Mode().Perm())
	}
}

// TestStaleGuardComparesContent: the record is content-addressed, so a timestamp
// that moved without the bytes moving is not a change, and bytes that moved
// inside one mtime (or at the same size) are.
func TestStaleGuardComparesContent(t *testing.T) {
	h := leaseHarness(t)
	p := filepath.Join(h.root, "a.txt")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolRead(h.sess, "a.txt", "")

	// A touch: a formatter that changed nothing, a checkout that restored
	// identical bytes. The old guard sent the model back to re-read the same file.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	if msg := h.sess.checkStale(context.Background(), "a.txt"); msg != "" {
		t.Fatalf("a touch alone must not stale a file: %q", msg)
	}
	// Same size, different bytes: the size cannot see it and the mtime alone
	// could not tell it from the touch above. The hash decides.
	if err := os.WriteFile(p, []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.sess.checkStale(context.Background(), "a.txt")
	if !strings.Contains(msg, "was modified since it was last read") {
		t.Fatalf("a same-size, same-mtime rewrite went unnoticed: %q", msg)
	}
}

// TestWholeReadLicensesWriteRangeDoesNot: `write` replaces every byte, so a
// model that saw lines 1-1 would silently drop everything it never looked at.
// `edit` is a targeted change and stays allowed.
func TestWholeReadLicensesWriteRangeDoesNot(t *testing.T) {
	resetChanges()
	defer resetChanges()
	h := leaseHarness(t)
	p := filepath.Join(h.root, "a.txt")
	if err := os.WriteFile(p, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := toolRead(h.sess, "a.txt", "1-1"); !strings.Contains(got, "one") {
		t.Fatalf("range read: %q", got)
	}
	if res := toolWrite(h.sess, "a.txt", "rewritten\n"); !strings.Contains(res, "was only read in part") {
		t.Fatalf("a line range licensed a whole-file write: %q", res)
	}
	if res := toolEdit(h.sess, "a.txt", "two", "TWO"); !strings.HasPrefix(res, "edited ") {
		t.Fatalf("a line range must still license a targeted edit: %q", res)
	}
	toolRead(h.sess, "a.txt", "")
	if res := toolWrite(h.sess, "a.txt", "rewritten\n"); !strings.HasPrefix(res, "wrote ") {
		t.Fatalf("a whole read must license a write: %q", res)
	}
}

// TestRunCommandIsBlamed: run_command and the verifier's check_cmd write
// arbitrarily and cannot be leased, so they are accounted for instead. Without
// this the guard blamed a stranger for the session's own `sed -i`.
func TestRunCommandIsBlamed(t *testing.T) {
	h := leaseHarness(t)
	p := filepath.Join(h.root, "a.txt")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolRead(h.sess, "a.txt", "")
	if out := h.sess.runTool(context.Background(), "echo formatting", 5*time.Second, nil); strings.HasPrefix(out, "error:") {
		t.Fatalf("the command did not run: %q", out)
	}
	if err := os.WriteFile(p, []byte("reformatted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.sess.checkStale(context.Background(), "a.txt")
	if !strings.Contains(msg, "`echo formatting`") {
		t.Fatalf("the stale message must name the session's own command: %q", msg)
	}
}

// TestLeaseTakenOverFromDeadPid: a SIGKILL between the O_EXCL and the rename
// leaves one lock behind. It must not make the file unwritable until someone
// runs a cleanup command.
func TestLeaseTakenOverFromDeadPid(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "a.txt")
	lp := filepath.Join(dir, leaseName(abs))
	dead := deadPid(t)
	if err := os.WriteFile(lp, []byte(fmt.Sprintf("%d\na.txt\n%s\n", dead, time.Now().Format(time.RFC3339))), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := leaseFile(dir, "a.txt", abs)
	if err != nil {
		t.Fatalf("a lock whose owner is gone must be taken over: %v", err)
	}
	release()
	if _, err := os.Stat(lp); !os.IsNotExist(err) {
		t.Fatal("the lease was not released")
	}
}

// TestLeaseWaitIsBounded: contention must degrade to a message the model can act
// on, never to a hang. A live foreign owner is the only case that waits.
func TestLeaseWaitIsBounded(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "a.txt")
	lp := filepath.Join(dir, leaseName(abs))
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Skipf("no sleep available: %v", err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	if err := os.WriteFile(lp, []byte(fmt.Sprintf("%d\na.txt\n%s\n", sleeper.Process.Pid, time.Now().Format(time.RFC3339))), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	release, err := leaseFile(dir, "a.txt", abs)
	took := time.Since(start)
	if err == nil {
		release()
		t.Fatal("a lock held by a LIVE process must not be taken over")
	}
	if !strings.Contains(err.Error(), "retry the change in a moment") {
		t.Fatalf("the refusal must be retryable advice: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", sleeper.Process.Pid)) {
		t.Fatalf("the refusal must name the holder: %v", err)
	}
	if took < leaseWait || took > leaseWait+2*time.Second {
		t.Fatalf("waited %s, want about %s", took, leaseWait)
	}
}

// TestLeasesTakenInSortedOrder: two writers taking {a,b} in opposite orders
// would each hold what the other waits for. leaseFiles sorts, so they cannot.
func TestLeasesTakenInSortedOrder(t *testing.T) {
	root := t.TempDir()
	o := leaseOrch(t, root)
	a, b := filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")
	// BOTH writers are waited for, not just the first. The second one used to be
	// launched and forgotten, which made this test fail in two ways that had
	// nothing to do with lease ordering: a t.Error from a goroutine still running
	// after the test returned panics the whole binary ("Fail in goroutine after
	// TestLeasesTakenInSortedOrder has completed"), and a lock file written after
	// the test returned made t.TempDir's cleanup fail with "directory not empty".
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			for _, set := range []map[string]string{{a: "a.txt", b: "b.txt"}, {b: "b.txt", a: "a.txt"}} {
				rel, err := o.leaseFiles(set)
				if err != nil {
					t.Error(err)
					return
				}
				rel()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			rel, err := o.leaseFiles(map[string]string{b: "b.txt", a: "a.txt"})
			if err != nil {
				t.Error(err)
				return
			}
			rel()
		}
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("leasing two files from two goroutines wedged")
	}
}

// TestLeasesAreSharedAcrossConfigDirs is why the lock directory is the git
// common dir and not the config dir: two terminals in one project may have
// different LCA_DIRs, and a lock under the config dir would be invisible to the
// other process — so both would "hold" the lease and one write would vanish.
func TestLeasesAreSharedAcrossConfigDirs(t *testing.T) {
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	if _, err := gitCmd(root, nil, nil, "init", "-q"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	o1 := leaseOrch(t, root)
	o1.cfg.Dir = filepath.Join(t.TempDir(), "one")
	o2 := leaseOrch(t, root)
	o2.cfg.Dir = filepath.Join(t.TempDir(), "two")
	if o1.lockDir() != o2.lockDir() {
		t.Fatalf("two config dirs got two lock dirs:\n%s\n%s", o1.lockDir(), o2.lockDir())
	}
	if !strings.Contains(o1.lockDir(), ".git") {
		t.Fatalf("the lock dir must live in the repository, got %s", o1.lockDir())
	}
	// And it is genuinely one lock file: a live foreign owner recorded through
	// o1's directory refuses o2.
	abs := filepath.Join(root, "a.txt")
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Skipf("no sleep available: %v", err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	lp := filepath.Join(o1.lockDir(), leaseName(abs))
	if err := os.MkdirAll(o1.lockDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, []byte(fmt.Sprintf("%d\na.txt\n", sleeper.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if rel, err := o2.leaseFile("a.txt", abs); err == nil {
		rel()
		t.Fatal("the second process did not see the first one's lock")
	}
}

// TestTaskChildStartsWithNoReads: the read records used to hang off the
// Orchestrator, so a `task` child could edit a file only its parent had read —
// and its own post-write note refreshed the PARENT's record, which then licensed
// the parent to erase the child's work. The task tool's description promises
// "the subagent starts with none of your context".
func TestTaskChildStartsWithNoReads(t *testing.T) {
	resetChanges()
	defer resetChanges()
	h := leaseHarness(t)
	p := filepath.Join(h.root, "a.txt")
	if err := os.WriteFile(p, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolRead(h.sess, "a.txt", "")
	// The parent's own agent, so a permission rule is not what refuses the edit.
	child, err := h.orch.newChild(h.sess, h.sess.agent, "look around")
	if err != nil {
		t.Fatal(err)
	}
	child.view = &quietView{}
	if res := toolEdit(child, "a.txt", "two", "TWO"); !strings.Contains(res, "has not been read in this session") {
		t.Fatalf("a child inherited its parent's reads: %q", res)
	}

	// And the other half: once the child has read and written, the PARENT must be
	// told, instead of finding its own record silently refreshed.
	toolRead(child, "a.txt", "")
	if res := toolEdit(child, "a.txt", "two", "TWO"); !strings.HasPrefix(res, "edited ") {
		t.Fatalf("the child could not edit what it had read: %q", res)
	}
	if res := toolWrite(h.sess, "a.txt", "parent\n"); !strings.Contains(res, "was modified since it was last read") {
		t.Fatalf("the parent was allowed to erase the child's work: %q", res)
	}
}

// TestForkSeedsTheChildsOwnReads: an inherited body that IS the worktree's bytes
// counts as the child having seen the file; a line range does not.
func TestForkSeedsTheChildsOwnReads(t *testing.T) {
	h := leaseHarness(t)
	for name, body := range map[string]string{"whole.txt": "alpha\nbeta\n", "part.txt": "gamma\ndelta\n"} {
		if err := os.WriteFile(filepath.Join(h.root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.sess.Msgs = append(h.sess.Msgs,
		Message{Role: "assistant", Content: "Reading.", ToolCalls: []ToolCall{call("r1", "read_file", map[string]any{"path": "whole.txt"}), call("r2", "read_file", map[string]any{"path": "part.txt"})}},
		Message{Role: "tool", ToolCallID: "r1", Tool: "read_file", Path: "whole.txt", Content: readFile(h.sess.jail(), "whole.txt", "")},
		Message{Role: "tool", ToolCallID: "r2", Tool: "read_file", Path: "part.txt", Content: readFile(h.sess.jail(), "part.txt", "1-1")})

	child, err := h.orch.newChild(h.sess, h.orch.agents["explore"], "fork me")
	if err != nil {
		t.Fatal(err)
	}
	child.view = &quietView{}
	if msgs, files, _ := h.orch.forkedContext(h.sess, child); len(msgs) == 0 || files != 2 {
		t.Fatalf("fork inherited %d files", files)
	}
	ctx := context.Background()
	if msg := child.checkStale(ctx, "whole.txt"); msg != "" {
		t.Fatalf("an inherited whole file must count as read: %q", msg)
	}
	if msg := child.checkStale(ctx, "part.txt"); !strings.Contains(msg, "has not been read in this session") {
		t.Fatalf("an inherited line range must not count as read: %q", msg)
	}
	// The parent's own record is untouched by the seeding.
	if _, ok := h.sess.readSet().get(filepath.Join(h.root, "whole.txt")); ok {
		t.Fatal("seeding the child wrote into the parent's read set")
	}
}

// TestUndoChecksStaleness: /undo was the one writer with no guard at all. It
// restored in-memory bytes with os.WriteFile and no check of any kind, so a
// subagent's later work on that file was discarded without a word.
func TestUndoChecksStaleness(t *testing.T) {
	resetChanges()
	defer resetChanges()
	dir := t.TempDir()
	o := leaseOrch(t, dir)
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, existed := snapshot(p)
	if err := os.WriteFile(p, []byte("agent's edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordChange("a.txt", p, "edit", before, existed, sumBytes([]byte("agent's edit\n")))

	// Somebody else wrote afterwards.
	if err := os.WriteFile(p, []byte("a subagent's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, ok := undoLast(o, false)
	if !ok || !strings.Contains(msg, "/undo force") {
		t.Fatalf("undo must refuse and say how to override: %q (%v)", msg, ok)
	}
	if got, _ := os.ReadFile(p); string(got) != "a subagent's work\n" {
		t.Fatalf("a refused undo wrote anyway: %q", got)
	}
	msg, ok = undoLast(o, true)
	if !ok || !strings.Contains(msg, "reverted") {
		t.Fatalf("/undo force must proceed: %q (%v)", msg, ok)
	}
	if got, _ := os.ReadFile(p); string(got) != "original\n" {
		t.Fatalf("forced undo did not restore: %q", got)
	}
}

// TestEditAndWriteAreNotParallel pins protection that exists by accident:
// execCalls drops a whole batch to sequential if any call is not parallel-safe,
// so two edits in one reply are already serialised. It was NEVER the protection
// — it does nothing across sessions or processes — but turning it on would make
// two edits in one reply race each other inside one process.
func TestEditAndWriteAreNotParallel(t *testing.T) {
	for _, name := range []string{"edit", "write", "run_command"} {
		d := resolveToolName(name)
		if d == nil {
			t.Fatalf("no tool named %s", name)
		}
		if d.Parallel {
			t.Fatalf("%s must not be Parallel: one reply's writes have to stay sequential", name)
		}
	}
}

// deadPid returns a pid that is certainly gone: a process started, waited for,
// and reaped.
func deadPid(t *testing.T) int {
	t.Helper()
	c := exec.Command("sh", "-c", "exit 0")
	if err := c.Run(); err != nil {
		t.Skipf("cannot spawn a process: %v", err)
	}
	return c.Process.Pid
}

// TestRemoteCompareAndSwap drives the member's own compare-and-swap through the
// local transport (sh -c), which exercises the real quoting, the real cd, stdin
// and the real exit codes. There is no cross-process lock on a member and none
// is invented: this is what replaces it.
func TestRemoteCompareAndSwap(t *testing.T) {
	dir := t.TempDir()
	rem := localRemote(dir)
	ctx := context.Background()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mt, size, sum, exists := rem.statSum(ctx, "a.txt")
	if !exists || size != 4 || mt == 0 {
		t.Fatalf("statSum: %d %d %q %v", mt, size, sum, exists)
	}
	if sum != sumBytes([]byte("one\n")) {
		t.Fatalf("statSum hash = %q, want the file's sha256", sum)
	}

	// The expectation holds: the write lands, and the mode is kept.
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if code, errs := rem.write(ctx, "a.txt", "two\n", sum, false); code != 0 {
		t.Fatalf("a matching expectation must write: %d %s", code, errs)
	}
	if got, _ := os.ReadFile(p); string(got) != "two\n" {
		t.Fatalf("content = %q", got)
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("the far side lost the file's mode: %v", info.Mode().Perm())
	}

	// A same-SIZE change inside the same mtime second: invisible to `stat -c %Y`,
	// which is the whole-second blindness the hash exists to kill.
	if err := os.WriteFile(p, []byte("XYZ\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _ := rem.write(ctx, "a.txt", "three\n", sumBytes([]byte("two\n")), false)
	if code != remoteWriteStale {
		t.Fatalf("a stale expectation must refuse, got code %d", code)
	}
	if got, _ := os.ReadFile(p); string(got) != "XYZ\n" {
		t.Fatalf("a refused write wrote anyway: %q", got)
	}
	if msg := remoteWriteMsg("a.txt", rem, remoteWriteStale, ""); !strings.Contains(msg, "was modified on") {
		t.Fatalf("stale message: %q", msg)
	}

	// A create that finds the file already there.
	if code, _ := rem.write(ctx, "a.txt", "mine\n", "", true); code != remoteWriteAppeared {
		t.Fatalf("a create race must refuse, got code %d", code)
	}
	if msg := remoteWriteMsg("a.txt", rem, remoteWriteAppeared, ""); !strings.Contains(msg, "created by someone else") {
		t.Fatalf("create message: %q", msg)
	}

	// An overwrite whose file was deleted under it.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if code, _ := rem.write(ctx, "a.txt", "back\n", sum, false); code != remoteWriteGone {
		t.Fatalf("a vanished target must refuse, got code %d", code)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("a refused write resurrected the file")
	}

	// And no temp file is left behind by any of that, in either shape: the suffix
	// form the first version used and the unique prefix form that replaced it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lca-tmp") || strings.HasPrefix(e.Name(), ".lca-tmp-") {
			t.Fatalf("a temp file was left behind: %s", e.Name())
		}
	}
}

// TestRemoteWritersDoNotShareATempFile is the member's half of the atomicity
// guarantee, and the one the fixed `.lca-tmp` name broke outright. There is
// deliberately no lock on a member, so two writers of one file (two lca
// processes on that machine, or a background subagent and the REPL session) run
// the CAS script concurrently: with one temp name per PATH they both `cat >` the
// same temp and both `mv -f` it, and the file ends up holding an interleave of
// both payloads — content neither writer ever had.
//
// Driven through the real shell transport, so the quoting, the stdin and the
// `mv` are the product's own.
func TestRemoteWritersDoNotShareATempFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rem := localRemote(dir)
	// Big enough that one write cannot land in a single write(2), which is what
	// makes a splice observable at all.
	a := strings.Repeat("aaaaaaaa\n", 20_000)
	b := strings.Repeat("bbbbbbbb\n", 20_000)
	for round := 0; round < 20; round++ {
		if err := os.WriteFile(p, []byte("start\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for _, body := range []string{a, b} {
			wg.Add(1)
			go func(body string) {
				defer wg.Done()
				// expectSum "" and expectNew false: the file exists, so the script skips
				// the hash compare and goes straight for the temp file. That is exactly
				// the window the shared name opened.
				rem.write(ctx, "a.txt", body, "", false)
			}(body)
		}
		wg.Wait()
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(got); s != a && s != b && s != "start\n" {
			na, nb := strings.Count(s, "aaaaaaaa\n"), strings.Count(s, "bbbbbbbb\n")
			t.Fatalf("round %d: the file holds neither version — %d bytes, %d lines of one payload and %d of the other", round, len(s), na, nb)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".lca-tmp") {
			t.Fatalf("a temp file was left behind: %s", e.Name())
		}
	}
}

// TestRepositoryLockIsOnePerRepo: the git bookkeeping lock is one lock for the
// whole repository, beside the file leases so one sweep finds every lock lca
// leaves. Part 2 holds it around worktree creation and a whole integration,
// which is the honest fix for the race delegate.go only serialises in memory.
func TestRepositoryLockIsOnePerRepo(t *testing.T) {
	dir := t.TempDir()
	release, err := lockGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lca-git.lock")); err != nil {
		t.Fatalf("the repository lock is not where a sweep would look: %v", err)
	}
	release()
	if _, err := os.Stat(filepath.Join(dir, "lca-git.lock")); !os.IsNotExist(err) {
		t.Fatal("the repository lock was not released")
	}
}

// TestHugeFileSaysTheFingerprintIsUnavailable: above the fingerprint bound the
// guard can only compare the size and the timestamp, and it says so rather than
// implying the bytes were compared.
func TestHugeFileSaysTheFingerprintIsUnavailable(t *testing.T) {
	h := leaseHarness(t)
	p := filepath.Join(h.root, "big.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(9 << 20); err != nil { // sparse: no bytes are written
		f.Close()
		t.Skipf("cannot make a sparse file: %v", err)
	}
	f.Close()
	toolRead(h.sess, "big.bin", "") // a truncated read: no fingerprint is recorded
	g, err := os.OpenFile(p, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Truncate(10 << 20); err != nil {
		g.Close()
		t.Fatal(err)
	}
	g.Close()
	msg := h.sess.checkStale(context.Background(), "big.bin")
	if !strings.Contains(msg, "too large to fingerprint") {
		t.Fatalf("want the honest message about the missing fingerprint, got %q", msg)
	}
}

// TestIntegrationDropsTheCallersReads: after a delegation's diff is merged the
// caller's record for those files is DROPPED, not refreshed. It has seen a diff,
// not the file, and a record there licensed its next whole-file write to erase
// the merge it had just accepted.
func TestIntegrationDropsTheCallersReads(t *testing.T) {
	resetChanges()
	defer resetChanges()
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply {
		if req.Model == "coder-a" {
			// The coder reads before it rewrites — in its own worktree, with its own
			// read set. Without the read the guard refuses the whole-file write, which
			// is itself the point.
			switch strings.Count(req.Body, `"role":"tool"`) {
			case 0:
				return fakeReply{calls: []ToolCall{call("r", "read_file", map[string]any{"path": "a.txt"})}}
			case 1:
				return fakeReply{calls: []ToolCall{
					call("w", "write", map[string]any{"path": "a.txt", "content": "the coder's work\n"}),
					call("d", "write", map[string]any{"path": "done.txt", "content": "ok\n"})}}
			}
			return fakeReply{content: "rewrote a.txt"}
		}
		return fakeReply{content: "ok"}
	})
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, true)
	p := filepath.Join(h.root, "a.txt")
	if err := os.WriteFile(p, []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCmd(h.root, nil, nil, "add", "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCmd(h.root, nil, nil, "commit", "-q", "-m", "a"); err != nil {
		t.Fatal(err)
	}
	toolRead(h.sess, "a.txt", "")
	if msg := h.sess.checkStale(context.Background(), "a.txt"); msg != "" {
		t.Fatalf("the caller should have read a.txt: %q", msg)
	}
	tc := &ToolCtx{Ctx: context.Background(), S: h.sess, Name: "delegate"}
	raw := runDelegateTool(tc, Args{"role": "coder", "task": "rewrite a.txt", "review": false})
	if !strings.Contains(raw, "passed") {
		t.Fatalf("the delegation did not pass: %s", headTail(raw, 400))
	}
	if got, _ := os.ReadFile(p); string(got) != "the coder's work\n" {
		t.Fatalf("the diff did not land: %q (%s)", got, headTail(raw, 600))
	}
	if msg := h.sess.checkStale(context.Background(), "a.txt"); !strings.Contains(msg, "has not been read in this session") {
		t.Fatalf("the caller kept a record of a file it only saw as a diff: %q", msg)
	}
}

// ── the lease is one lease, across processes ────────────────────────────────

// TestLeaseIsNeverHeldTwiceAcrossProcesses needs two real processes, because the
// defect it pins is invisible to the in-process mutex: the takeover used to be a
// check followed by an unconditional os.Remove, which removes whatever is at
// that path NOW and not the file it inspected. Process B that read the stale pid
// a microsecond before A finished taking over deleted A's LIVE lock and then won
// its own create, so both sat inside their write windows at once and one write
// was lost without a word — the very failure the lease exists to prevent, one
// layer down. A second trigger needed no stale lock at all: between an O_EXCL
// create and the Fprintf of the pid line the lock file is empty, reads back as
// pid 0, and is therefore "abandoned" to every rival.
//
// The racers detect it two ways: an O_EXCL holder file (two holders at once
// cannot both create it) and a check that the lock they hold still records their
// own pid.
func TestLeaseIsNeverHeldTwiceAcrossProcesses(t *testing.T) {
	if os.Getenv("LCA_LEASE_RACER") != "" {
		leaseRacer()
		return
	}
	dir := t.TempDir()
	abs := filepath.Join(dir, "hot.txt")
	// Start from a stale lock, which is the takeover path. Every racer sees it,
	// and exactly one of them may win it.
	if err := os.WriteFile(filepath.Join(dir, leaseName(abs)),
		[]byte(fmt.Sprintf("%d\nhot.txt\n%s\n", deadPid(t), time.Now().Format(time.RFC3339))), 0o600); err != nil {
		t.Fatal(err)
	}
	const racers = 4
	outs := make([]string, racers)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestLeaseIsNeverHeldTwiceAcrossProcesses", "-test.timeout=4m")
			cmd.Env = append(os.Environ(), "LCA_LEASE_RACER=1", "LCA_LEASE_DIR="+dir)
			b, _ := cmd.CombinedOutput()
			outs[i] = string(b)
		}(i)
	}
	wg.Wait()
	for i, o := range outs {
		if strings.Contains(o, "DOUBLE-HOLD") {
			t.Fatalf("two processes held one lease at the same time (racer %d):\n%s", i, o)
		}
	}
}

func leaseRacer() {
	dir := os.Getenv("LCA_LEASE_DIR")
	abs := filepath.Join(dir, "hot.txt")
	lp := filepath.Join(dir, leaseName(abs))
	holder := filepath.Join(dir, "holder")
	for i := 0; i < 50; i++ {
		release, err := leaseFile(dir, "hot.txt", abs)
		if err != nil {
			continue // a refusal is a correct outcome; only a second holder is not
		}
		f, cerr := os.OpenFile(holder, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if cerr != nil {
			inside, _ := os.ReadFile(holder)
			owner, _, _, _ := leaseOwner(lp)
			fmt.Printf("DOUBLE-HOLD pid %d round %d: somebody else is already inside: %v | holder=%q alive=%v | lock owner=%d\n",
				os.Getpid(), i, cerr, inside, pidAlive(atoiDefault(strings.TrimSpace(string(inside)), 0)), owner)
			release()
			return
		}
		fmt.Fprintf(f, "%d", os.Getpid())
		f.Close()
		time.Sleep(time.Millisecond)
		if pid, _, _, _ := leaseOwner(lp); pid != os.Getpid() {
			fmt.Printf("DOUBLE-HOLD pid %d round %d: the lock it holds now records pid %d\n", os.Getpid(), i, pid)
		}
		os.Remove(holder)
		release()
	}
}

// TestLockFileIsNeverSeenEmpty pins the second half of the same blocker without
// needing a race to be lucky: a reader looking at the lock path must never find
// it existing and empty, because an empty lock reads back as pid 0 and pid 0 is
// what every reader is entitled to treat as "the owner is gone".
func TestLockFileIsNeverSeenEmpty(t *testing.T) {
	dir := t.TempDir()
	lp := filepath.Join(dir, "x.lock")
	stop := make(chan struct{})
	done := make(chan int)
	go func() {
		empty := 0
		for {
			select {
			case <-stop:
				done <- empty
				return
			default:
			}
			if b, err := os.ReadFile(lp); err == nil && len(b) == 0 {
				empty++
			}
		}
	}()
	for i := 0; i < 3000; i++ {
		if err := claimLease(lp, "x.txt"); err != nil {
			close(stop)
			<-done
			t.Fatal(err)
		}
		if pid, held, born, ok := leaseOwner(lp); !ok || pid != os.Getpid() || held != "x.txt" || born.IsZero() {
			close(stop)
			<-done
			t.Fatalf("a lock must appear fully formed: pid %d held %q born %v", pid, held, born)
		}
		os.Remove(lp)
	}
	close(stop)
	if n := <-done; n != 0 {
		t.Fatalf("a reader saw the lock file existing and empty %d times — that reads back as pid 0 and gets the lease taken away", n)
	}
}

// TestLeaseExpiresWhenAPidWasReused pins the other way a lock became permanent.
// Pids are reused (macOS wraps at 99999), so a lock a SIGKILL left behind
// eventually names some unrelated live process, pidAlive says "held" for ever,
// and every edit of that file returns retryable advice that can never come true.
// The recorded time is the way out.
func TestLeaseExpiresWhenAPidWasReused(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "a.txt")
	lp := filepath.Join(dir, leaseName(abs))
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Skipf("no sleep available: %v", err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	// A live pid, and a lock older than any write takes: the pid was reused.
	old := time.Now().Add(-10 * leaseStale).Format(time.RFC3339)
	if err := os.WriteFile(lp, []byte(fmt.Sprintf("%d\na.txt\n%s\n", sleeper.Process.Pid, old)), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	release, err := leaseFile(dir, "a.txt", abs)
	if err != nil {
		t.Fatalf("a lock older than any write must be taken over, whoever holds that pid now: %v", err)
	}
	release()
	if took := time.Since(start); took > leaseWait {
		t.Fatalf("the takeover waited %s; an expired lock must not cost the full bound", took)
	}
	// And the repository lock, held across a whole integration, keeps the longer
	// horizon: the same age must NOT expire it.
	gl := filepath.Join(dir, gitLockName)
	if err := os.WriteFile(gl, []byte(fmt.Sprintf("%d\nthis repository\n%s\n", sleeper.Process.Pid, old)), 0o600); err != nil {
		t.Fatal(err)
	}
	if rel, err := lockGit(dir); err == nil {
		rel()
		t.Fatal("a ten-minute-old repository lock held by a live pid must not be taken over: an integration can legitimately take that long")
	}
}

// TestLeaseBoundIsPerWaiterNotPerQueue pins the advertised two seconds. The
// deadline used to be computed AFTER the process-local mutex, so a goroutine
// behind another did not start its own budget until the one in front had spent
// all of its: with a holder that never goes away, four subagents on one file
// meant the fourth reply blocked about eight seconds.
func TestLeaseBoundIsPerWaiterNotPerQueue(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "a.txt")
	lp := filepath.Join(dir, leaseName(abs))
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Skipf("no sleep available: %v", err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	if err := os.WriteFile(lp, []byte(fmt.Sprintf("%d\na.txt\n%s\n", sleeper.Process.Pid, time.Now().Format(time.RFC3339))), 0o600); err != nil {
		t.Fatal(err)
	}
	const waiters = 4
	took := make([]time.Duration, waiters)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t0 := time.Now()
			if release, err := leaseFile(dir, "a.txt", abs); err == nil {
				release()
			}
			took[i] = time.Since(t0)
		}(i)
	}
	wg.Wait()
	for i, d := range took {
		if d > leaseWait+2*time.Second {
			t.Fatalf("waiter %d was refused after %s; every waiter's refusal must arrive within %s of when it ASKED, not of when the queue ahead of it drained (whole wave %s)",
				i, d, leaseWait, time.Since(start))
		}
	}
}

// ── the read→stat window, and the hash that closes it ───────────────────────

// TestSameSizeRivalInTheReadWindowIsNotVouchedFor is the last silent-loss window
// Part 1 left open. noteRead stats the file AFTER the read, so a rival write
// landing in between gets ITS mtime and size recorded against bytes the model
// never saw. Comparing only the length waved through every rival whose write
// happened to be the same size — a one-character substitution, a formatter, two
// versions of one line — and the next whole-file `write` then erased it while
// reporting success.
//
// The window is simulated exactly rather than raced for: the record is taken
// against bytes that are not the ones on disk, which is what the window
// produces, and the test is then deterministic.
func TestSameSizeRivalInTheReadWindowIsNotVouchedFor(t *testing.T) {
	resetChanges()
	defer resetChanges()
	h := leaseHarness(t)
	p := filepath.Join(h.root, "a.txt")
	// What the rival left on disk, and what the model was shown: same length.
	if err := os.WriteFile(p, []byte("BBBB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.sess.noteLocalRead(h.sess.jail(), "a.txt", "AAAA\n", true)

	if msg := h.sess.checkStaleWhole(context.Background(), "a.txt"); msg == "" {
		t.Fatal("a record taken against bytes the file does not hold must never vouch for it")
	}
	if res := toolWrite(h.sess, "a.txt", "the model's rewrite of AAAA\n"); !strings.Contains(res, "was modified since it was last read") {
		t.Fatalf("a same-size rival write inside the read window was erased silently: %q", res)
	}
	if b, _ := os.ReadFile(p); string(b) != "BBBB\n" {
		t.Fatalf("the rival's bytes are gone: %q", b)
	}
	// And the honest case still works: re-read, then write.
	toolRead(h.sess, "a.txt", "")
	if res := toolWrite(h.sess, "a.txt", "mine\n"); !strings.HasPrefix(res, "wrote ") {
		t.Fatalf("a re-read must clear it: %q", res)
	}
}

// TestLargeFileWriteNamesEditNotAnImpossibleRead: read_file truncates above
// maxReadBytes, so no read of a bigger file can ever record a whole-file
// fingerprint. Telling the model to "read the whole file before overwriting it"
// there names a remedy the tool cannot perform, and it loops read → write →
// refused for ever — regenerating a large generated file, a lockfile or a big
// JSON through `write` became impossible with no way to find that out.
func TestLargeFileWriteNamesEditNotAnImpossibleRead(t *testing.T) {
	resetChanges()
	defer resetChanges()
	h := leaseHarness(t)
	p := filepath.Join(h.root, "big.json")
	var sb strings.Builder
	for i := 0; sb.Len() <= maxReadBytes+10_000; i++ {
		fmt.Fprintf(&sb, "{\"k\": \"value number %d\"},\n", i)
	}
	big := sb.String()
	if int64(len(big)) <= maxReadBytes {
		t.Fatalf("the fixture is only %d bytes", len(big))
	}
	if err := os.WriteFile(p, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := toolRead(h.sess, "big.json", ""); !strings.Contains(got, "truncated") {
		t.Fatalf("the fixture was not truncated, so this test proves nothing: %.80q", got)
	}
	res := toolWrite(h.sess, "big.json", "{}\n")
	switch {
	case strings.Contains(res, "read the whole file"):
		t.Fatalf("the refusal asks for something read_file cannot do: %q", res)
	case !strings.Contains(res, "use edit"):
		t.Fatalf("the refusal must name the route that works: %q", res)
	}
	if res2 := toolWrite(h.sess, "big.json", "{}\n"); res2 != res {
		t.Fatalf("a re-read changed the verdict, so the loop is still reachable: %q then %q", res, res2)
	}
	// edit really is that route.
	if out := toolEdit(h.sess, "big.json", `"value number 7"`, `"CHANGED"`); !strings.HasPrefix(out, "edited ") {
		t.Fatalf("edit must work on a file too large to read whole: %q", out)
	}
}

// TestStaleMessageIsOneLine: a tool result is not a page. The two-line form with
// a seven-space hanging indent was the design document's own wrapping, and it
// went into the model's prompt, the transcript and the trace exactly as written.
func TestStaleMessageIsOneLine(t *testing.T) {
	h := leaseHarness(t)
	p := filepath.Join(h.root, "a.txt")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolRead(h.sess, "a.txt", "")
	if err := os.WriteFile(p, []byte("rival\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.sess.checkStale(context.Background(), "a.txt")
	if msg == "" || !strings.Contains(msg, "you read it at") {
		t.Fatalf("want the timestamped staleness message, got %q", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("a guard message must be one line:\n%q", msg)
	}
}

// ── the member's leg ────────────────────────────────────────────────────────

// remoteSession is a session pinned to a Remote, which is what s.remote()
// answers for. Built directly because the guard under test is the member's leg
// and nothing above it matters here.
func remoteSession(t *testing.T, dir string) (*Session, *Remote) {
	t.Helper()
	rem := localRemote(dir)
	o := leaseOrch(t, dir)
	return &Session{orch: o, agent: &Agent{Name: "x"}, wtRem: rem}, rem
}

// TestRemoteGuardTrustsTheHashNotTheClock: statSum brings the member's mtime,
// size AND sha256 back in one round trip, and the guard used to take the cheap
// mtime+size branch first. Member `stat` has one-second granularity, so a
// rival's same-size overwrite inside that second matched both — and then
// runRemoteWrite handed the far side a freshly statted hash as its expectation,
// i.e. the RIVAL's hash, so the compare-and-swap compared a value with itself
// and let the overwrite through.
func TestRemoteGuardTrustsTheHashNotTheClock(t *testing.T) {
	dir := t.TempDir()
	s, rem := remoteSession(t, dir)
	ctx := context.Background()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// What the model was shown.
	shown := rem.readFile(ctx, "a.txt", "")
	s.noteRead(ctx, "a.txt", strings.TrimPrefix(shown, "a.txt:\n"), true)
	mt, _, _, _ := rem.statSum(ctx, "a.txt")

	// A rival, same size, inside the same mtime SECOND — which is all the member's
	// stat can resolve.
	if err := os.WriteFile(p, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(mt, 0)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	if _, size, sum, _ := rem.statSum(ctx, "a.txt"); size != 4 || sum != sumBytes([]byte("two\n")) {
		t.Fatalf("the fixture is wrong: size %d sum %q", size, sum)
	}
	if msg := s.checkStaleWhole(ctx, "a.txt"); !strings.Contains(msg, "was modified on") {
		t.Fatalf("a same-size rival inside one mtime second went unnoticed although the hash was in hand: %q", msg)
	}
	// And the expectation the far side is given is what the MODEL saw, so even if
	// the guard above were bypassed the swap would refuse.
	if got := s.seenSum("a.txt"); got != sumBytes([]byte("one\n")) {
		t.Fatalf("the recorded hash is not the hash of what the model was shown: %q", got)
	}
	if code, _ := rem.write(ctx, "a.txt", "rewritten\n", s.seenSum("a.txt"), false); code != remoteWriteStale {
		t.Fatalf("the compare-and-swap must refuse, got code %d", code)
	}
	if b, _ := os.ReadFile(p); string(b) != "two\n" {
		t.Fatalf("the rival's bytes are gone: %q", b)
	}
}

// TestNoisyMemberBlamesTheRcFileAndNotTheFile: Remote.run merges stdout and
// stderr, a condition this codebase already treats as real and nameable. On a
// member whose non-interactive shell prints a banner, the bytes the model is
// SHOWN are banner+content while the member's own hash is of content alone —
// and recording that as a torn read refused every later change to every file
// with "<file> was modified on <machine>", which blames the wrong thing and
// cannot be cleared, because re-reading reproduces the banner.
func TestNoisyMemberBlamesTheRcFileAndNotTheFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrap := filepath.Join(t.TempDir(), "noisy.sh")
	if err := os.WriteFile(wrap, []byte("#!/bin/sh\necho 'Welcome to build2!'\nsh -c \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rem := &Remote{Host: "", Dir: dir, SSH: []string{"sh", wrap}}
	s := &Session{orch: leaseOrch(t, dir), agent: &Agent{Name: "x"}, wtRem: rem}
	ctx := context.Background()
	shown := rem.readFile(ctx, "a.txt", "")
	if !strings.Contains(shown, "Welcome to build2!") {
		t.Skipf("the wrapper did not pollute the read, so this test proves nothing: %q", shown)
	}
	s.noteRead(ctx, "a.txt", strings.TrimPrefix(shown, "a.txt:\n"), true)

	// Three rounds, because the defect was that the refusal never cleared.
	for i := 0; i < 3; i++ {
		msg := s.checkStaleWhole(ctx, "a.txt")
		switch {
		case msg == "":
			t.Fatal("a read that came back with a banner must not license a whole-file write")
		case strings.Contains(msg, "was modified"):
			t.Fatalf("round %d blames the file for the member's shell: %q", i, msg)
		case !strings.Contains(msg, "[ -t 1 ]"):
			t.Fatalf("round %d does not name the cause or the fix: %q", i, msg)
		}
		shown = rem.readFile(ctx, "a.txt", "")
		s.noteRead(ctx, "a.txt", strings.TrimPrefix(shown, "a.txt:\n"), true)
	}
	// And a clean member is unaffected: the same file, read without the banner.
	clean, _ := remoteSession(t, dir)
	cs := clean.remote().readFile(ctx, "a.txt", "")
	clean.noteRead(ctx, "a.txt", strings.TrimPrefix(cs, "a.txt:\n"), true)
	if msg := clean.checkStaleWhole(ctx, "a.txt"); msg != "" {
		t.Fatalf("a quiet member must be licensed: %q", msg)
	}
}

// TestAtomicWriteRefusesAReadOnlyFile: a rename needs the directory, not the
// file, so the atomic write quietly acquired a power os.WriteFile never had.
// `chmod 444` is how a person or a build step says "hands off"; the file came
// back still 0444 and holding something else, so the overwrite was invisible
// afterwards.
func TestAtomicWriteRefusesAReadOnlyFile(t *testing.T) {
	resetChanges()
	defer resetChanges()
	h := leaseHarness(t)
	p := filepath.Join(h.root, "ro.txt")
	if err := os.WriteFile(p, []byte("vendored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	// The control: this is the refusal the old writer gave, and it is what makes
	// chmod 444 mean anything.
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err == nil {
		t.Skip("this filesystem (or this uid) ignores the write bit, so there is nothing to preserve")
	}
	toolRead(h.sess, "ro.txt", "")
	if res := toolWrite(h.sess, "ro.txt", "clobbered\n"); !strings.Contains(res, "permission denied") {
		t.Fatalf("a read-only file was overwritten through the rename: %q", res)
	}
	if b, _ := os.ReadFile(p); string(b) != "vendored\n" {
		t.Fatalf("the file was changed: %q", b)
	}
	if res := toolEdit(h.sess, "ro.txt", "vendored", "clobbered"); !strings.Contains(res, "permission denied") {
		t.Fatalf("edit went through where write was refused: %q", res)
	}
	// And making it writable again restores the old behaviour exactly.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := toolWrite(h.sess, "ro.txt", "fine\n"); !strings.HasPrefix(res, "wrote ") {
		t.Fatalf("a writable file must still be writable: %q", res)
	}
}

// TestAtomicWriteKeepsSetgidAndSticky: os.Rename installs a brand-new inode and
// preserves nothing, and copying only Mode().Perm() kept the low nine bits and
// dropped the three above them. A setgid helper script then comes back without
// its setgid bit and stops working for its group, with nothing on screen.
func TestAtomicWriteKeepsSetgidAndSticky(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// setgid where a plain uid may set it, the sticky bit where it may not: the
	// claim is about the three bits above Perm(), not about which one.
	var want fs.FileMode
	for _, m := range []fs.FileMode{0o755 | fs.ModeSetgid, 0o755 | fs.ModeSticky} {
		if os.Chmod(p, m) != nil {
			continue
		}
		if info, err := os.Stat(p); err == nil && info.Mode()&^fs.ModePerm&^fs.ModeType != 0 {
			want = info.Mode()
			break
		}
	}
	if want == 0 {
		t.Skip("neither setgid nor sticky survives a chmod here, so there is nothing to preserve")
	}
	if err := writeFileAtomic(p, []byte("#!/bin/sh\necho hi\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode() != want {
		t.Fatalf("the rename dropped a mode bit above Perm(): %v, want %v", got.Mode(), want)
	}
}

// TestPatchApplyHoldsTheFileLeases closes the last writer the README's
// guarantee names. Under the DEFAULT `apply: verified` the integration is still
// a text patch, and `git apply --check` + `git apply` is two passes over the
// same files: git has decided the patch applies by the time it writes, so
// whatever lands inside its own read→write is gone with no message. The leases
// are what the leased integration takes anyway, so both modes contend on the
// same lock files.
func TestPatchApplyHoldsTheFileLeases(t *testing.T) {
	h := leaseHarness(t)
	w := &worktree{mgr: &worktrees{}, changedFiles: []string{"a.txt", "sub/b.txt"}}
	locks := h.sess.orch.lockDir()
	root := h.sess.jail().Root
	held := make(chan struct{})
	// A sibling takes one of the two leases and holds it: the apply must not be
	// able to start.
	release, err := leaseFile(locks, "a.txt", filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(leaseWait / 4)
		release()
		close(held)
	}()
	start := time.Now()
	errApply := h.sess.applyPatch(w, "not a patch\n")
	took := time.Since(start)
	<-held
	if errApply == nil {
		t.Fatal("the fixture patch is not a patch, so the apply must fail — this test is about what it waited for")
	}
	if took < leaseWait/4 {
		t.Fatalf("the apply ran in %s without waiting for the file's lease: the window between --check and apply is open", took)
	}
	// The leases are released again, so an ordinary write is not wedged behind them.
	for _, f := range w.changedFiles {
		r, err := leaseFile(locks, f, filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			t.Fatalf("applyPatch did not release %s: %v", f, err)
		}
		r()
	}
}
