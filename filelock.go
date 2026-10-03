package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// File leases. One lease per FILE, never a directory and never the tree: a tree
// lease turns a parallel reply into a queue, and three subagents editing three
// files in one reply would serialise behind the first one's approval prompt.
//
// A lease is held for the handful of milliseconds between the staleness re-check
// and the rename — never across an approval prompt. Approver.Confirm holds
// promptMu for the whole human answer (approval.go), so a lease taken at check
// time and kept across the question would block every sibling for as long as the
// human is away, and three children asking about one file would deadlock: child
// 2 cannot get the lease until child 1 writes, and child 1 cannot write until the
// human answers a question child 2 is queued ahead of. The window between the
// check and the bytes landing is closed by re-checking UNDER the lease instead.
//
// The locking idiom is lockConfig's (configfile.go), deliberately: a sync.Map of
// mutexes for this process's own goroutines (they share a pid, so the pid file
// below cannot tell them apart) plus an O_CREATE|O_EXCL pid file for other lca
// processes, taken over when the owner is gone, because a SIGKILL mid-write must
// not make a file permanently unwritable.

const (
	// Bounded, then a message the model can act on. Contention must degrade to a
	// retryable tool result, never to a hang: the model can retry, a wedged reply
	// cannot be rescued by anything.
	leaseWait = 2 * time.Second
	leasePoll = 25 * time.Millisecond

	// How long a lock file is BELIEVED, past which it is taken over even though
	// its recorded pid is alive. Pids are reused — macOS wraps at 99999 — so a
	// lock a SIGKILL left behind sits there until some unrelated process happens
	// to take that number, and from then on pidAlive says "held" for ever: every
	// edit of that file waits the full bound and returns retryable advice that can
	// never come true, with no way out but finding a sha-named file under
	// .git/lca-locks by hand. A file lease is milliseconds (it is never held
	// across a prompt), so a minute is far above any honest holder and far below
	// the hours a reused pid needs.
	leaseStale = time.Minute

	// The repository lock is the same primitive held for much longer — a whole
	// integration, which is a snapshot, a merge-tree, a diff and an apply — so it
	// gets git's own expiry horizon instead. Taking this one over from a live
	// integration would be worse than waiting.
	gitLockStale = time.Hour

	// Above this the guard does not hash a file on disk. A whole read is capped at
	// maxReadBytes long before this, so the only thing this bound protects is the
	// re-hash in checkStale: a 40 MB file is not read twice to answer a question
	// its size and mtime can answer, and the message says the fingerprint is
	// unavailable rather than pretending the bytes were compared.
	maxFingerprintBytes = 8 << 20
)

// fileLeases is the in-process half: lock-file path → *sync.Mutex. Keyed on the
// lock path rather than the file path so two spellings of one file (a symlink and
// its target both resolve to the same abs path through Jail.Resolve) cannot take
// two different mutexes for one pid file.
var fileLeases sync.Map

// leaseDir is where cross-process leases live: <git-common-dir>/lca-locks.
//
// NOT the config directory. Two terminals in one project may have different
// LCA_DIRs — which is exactly the operator's setup — and a lock under the config
// directory would be invisible to the other process, so both would "hold" the
// lease and one write would be lost silently. The git common dir is the same
// absolute path from the main worktree and from every linked worktree, it is
// shared by every lca in that repository whatever its configuration, it never
// shows up in `git status`, and it never lands in a snapshot.
//
// Outside a git repository there is no shared place, so the fallback is the state
// directory and the guarantee shrinks to "processes that share an LCA_DIR".
func leaseDir(root, fallback string) string {
	common := func() string {
		// --path-format=absolute needs git 2.31; an older git errors on the flag,
		// so ask again without it and join the relative answer with the toplevel.
		if out, err := gitCmd(root, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
			if d := strings.TrimSpace(out); d != "" && filepath.IsAbs(d) {
				return d
			}
		}
		out, err := gitCmd(root, nil, nil, "rev-parse", "--git-common-dir")
		if err != nil {
			return ""
		}
		d := strings.TrimSpace(out)
		switch {
		case d == "":
			return ""
		case filepath.IsAbs(d):
			return d
		}
		top, err := gitCmd(root, nil, nil, "rev-parse", "--show-toplevel")
		if err != nil {
			return ""
		}
		return filepath.Join(strings.TrimSpace(top), d)
	}()
	if common == "" {
		return filepath.Join(fallback, "locks")
	}
	return filepath.Join(common, "lca-locks")
}

// lockDir resolves the lease directory once per Orchestrator. Once, because it
// costs a git subprocess and every write asks for it; lazily, because most of
// the ways an Orchestrator is built never write a file.
func (o *Orchestrator) lockDir() string {
	o.locksOnce.Do(func() {
		root, state := ".", ""
		if o.jl != nil {
			root = o.jl.Root
		}
		state = o.cfg.stateDir()
		if state == "" {
			state = filepath.Join(os.TempDir(), "lca-locks")
		}
		o.locks = leaseDir(root, state)
	})
	return o.locks
}

// leaseFile takes the write lease for one file. `name` is the display path, so
// the refusal names the file the way the model wrote it.
func (o *Orchestrator) leaseFile(name, abs string) (func(), error) {
	return leaseFile(o.lockDir(), name, abs)
}

// leaseFiles takes several leases at once, in sorted path order and releasing in
// reverse — lockDirs' anti-wedge trick (configfile.go). Two writers that took
// {a,b} in opposite orders would each hold what the other waits for, and the
// 2 s bound would turn that into two failures instead of two writes.
func (o *Orchestrator) leaseFiles(paths map[string]string) (func(), error) {
	return leaseFilesIn(o.lockDir(), paths)
}

// leaseFilesIn is leaseFiles with the directory handed in, for the one caller
// that has a repository but no Orchestrator: `lca merge` finishing a human's
// resolution. The lock files are the same ones, which is the point — a merge
// finished from the command line must contend with a running lca, not beside it.
func leaseFilesIn(dir string, paths map[string]string) (func(), error) {
	abs := make([]string, 0, len(paths))
	for a := range paths {
		abs = append(abs, a)
	}
	sort.Strings(abs)
	var held []func()
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i]()
		}
	}
	for _, a := range abs {
		rel, err := leaseFile(dir, paths[a], a)
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, rel)
	}
	return release, nil
}

func leaseFile(dir, name, abs string) (func(), error) {
	return leaseAt(filepath.Join(dir, leaseName(abs)), name, leaseStale)
}

// lockGit is the same primitive, one lock per repository, for git's own
// bookkeeping (worktree create / remove / prune) and for a whole integration —
// the honest fix for the in-memory serialisation delegate.go admits is not one.
// It lives beside the file leases so one sweep finds every lock lca leaves.
func lockGit(dir string) (func(), error) {
	return leaseAt(filepath.Join(dir, gitLockName), "this repository's git bookkeeping", gitLockStale)
}

// gitLockName is named rather than spelled twice because `lca clean`'s lease
// sweep must be able to leave this one alone: it is the lock the sweep itself is
// holding while it runs.
const gitLockName = "lca-git.lock"

// leaseName keys a lock file by the hash of the absolute path: the path itself
// contains separators and may be longer than a filename may be, and a flattened
// spelling ("-Users-me-x") collides with a real file called "Users-me-x".
func leaseName(abs string) string {
	return sumBytes([]byte(abs))[:16] + ".lock"
}

// leaseAt takes one lock. `stale` is how long the lock file on disk is believed
// before it is taken over anyway (leaseStale for a file, gitLockStale for the
// repository).
func leaseAt(lp, name string, stale time.Duration) (func(), error) {
	// The deadline starts when the caller ASKED, before the process-local mutex
	// and not after it. Taken after, a goroutine queued behind another does not
	// begin its own budget until the one in front has spent all of its, so with an
	// external holder and four parallel subagents on one file the fourth reply
	// blocks about eight seconds — and the README's "bounded at two seconds" is
	// two seconds per waiter ahead of you, which is a different promise.
	deadline := time.Now().Add(leaseWait)
	mu, _ := fileLeases.LoadOrStore(lp, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	if !lockBefore(m, deadline) {
		return nil, fmt.Errorf("%s is being written by another part of this lca right now — retry the change in a moment", name)
	}
	// From here on every return path must unlock: a goroutine that failed to get
	// the pid file still holds the process-local mutex.
	if err := os.MkdirAll(filepath.Dir(lp), 0o700); err != nil {
		m.Unlock()
		return nil, err
	}
	for {
		err := claimLease(lp, name)
		if err == nil {
			return func() { releaseLease(lp); m.Unlock() }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			m.Unlock()
			return nil, err
		}
		pid, held, born, ok := leaseOwner(lp)
		if !ok {
			// It was released between our claim and this read. Go round and try to
			// CLAIM it, rather than "taking over" a path whose file we never
			// inspected: a third process can claim it in that window, and taking over
			// then hands two writers the one lease. Measured — two racing processes
			// were inside at once, with the lock recording the other one's pid.
			if time.Now().After(deadline) {
				m.Unlock()
				return nil, fmt.Errorf("%s is being written by another lca right now — retry the change in a moment", name)
			}
			// Paced like the two other retries in this file, and it was not: four
			// subagents editing one file is the contention this header describes, and
			// a racer that keeps losing link(2) and then finding the lock released
			// looped as fast as the filesystem would answer for the whole two-second
			// deadline — a CreateTemp and an unlink per iteration under
			// .git/lca-locks. It ended on time, so it was churn rather than a hang,
			// but on a slow or networked .git it made the contention it was reacting
			// to worse. The holder is milliseconds away; the same 25 ms tick.
			time.Sleep(leasePoll)
			continue
		}
		// The clock for "held too long" is the recorded time, or the file's mtime
		// when there is no readable record: a lock with no owner in it cannot be
		// reasoned about, only waited out. claimLease makes an empty lock
		// impossible, so one can only come from an older binary or from a crash
		// inside the O_EXCL fallback — and stealing it on sight is how that window
		// becomes two writers too.
		since := born
		if since.IsZero() {
			if info, serr := os.Stat(lp); serr == nil {
				since = info.ModTime()
			}
		}
		// A recorded time in the future (a clock that moved, a filesystem shared
		// with a machine an hour ahead) counts as fresh: believing a lock too long
		// costs a bounded wait, taking it over too early costs somebody's bytes.
		expired := !since.IsZero() && time.Since(since) >= stale
		switch {
		case pid == os.Getpid():
			// Our own leftover, from a process that held this pid before us. Nothing
			// alive can be behind it — this process's own goroutines are behind the
			// mutex — so it is taken over without waiting.
		case expired:
		case pid <= 0 || pidAlive(pid):
			if time.Now().After(deadline) {
				m.Unlock()
				if pid > 0 {
					return nil, fmt.Errorf("%s is being written by another lca (pid %d) right now — retry the change in a moment", name, pid)
				}
				return nil, fmt.Errorf("%s is being written by another lca right now — retry the change in a moment", name)
			}
			time.Sleep(leasePoll)
			continue
		}
		// Take it over as one atomic claim, and then CHECK WHICH FILE WE TOOK. A
		// check followed by an os.Remove removes whatever is at that path NOW and
		// not the file it inspected: process B that read the stale pid a microsecond
		// before A finished taking over would delete A's LIVE lock and then win its
		// own create, so both would sit inside their write windows at once — the
		// silent overwrite this whole design exists to prevent, one layer down. A
		// rename of a path succeeds for exactly one racer, and comparing the renamed
		// file against the one we judged turns the pair into a compare-and-swap.
		if time.Now().After(deadline) {
			// Out of budget before taking anything over: leave the lock where it is.
			m.Unlock()
			return nil, fmt.Errorf("%s is being written by another lca right now — retry the change in a moment", name)
		}
		gone := fmt.Sprintf("%s.stale-%d-%d", lp, os.Getpid(), time.Now().UnixNano())
		if err := os.Rename(lp, gone); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			m.Unlock()
			return nil, err
		}
		if gp, _, gb, gok := leaseOwner(gone); !gok || gp != pid || !gb.Equal(born) {
			// Somebody claimed the name between our read and our rename, and what we
			// hold is THEIR live lock. Put it back — the same inode with the same
			// bytes, so their own release still recognises and removes it — and go
			// round. If the name has been claimed again in the meantime, theirs
			// stands and this one is moot.
			os.Link(gone, lp)
			os.Remove(gone)
			continue
		}
		os.Remove(gone)
		// Named by the FILE and not by the 16-hex lock name: the lock lives under
		// .git/lca-locks, nothing in the UI mentions that directory, and the sha is
		// of the absolute path so it cannot be reversed — the operator was being
		// handed a hash they could not map to anything, while the one fact that
		// makes the line actionable sat on line 2 of the file just read.
		switch {
		case pid <= 0 || pid == os.Getpid():
			// Our own leftover, or a lock with no readable owner: nothing to report.
		case pidAlive(pid):
			warnLine("taking over the lease on %s: pid %d has held it since %s, longer than any write takes",
				firstNonEmpty(held, filepath.Base(lp)), pid, since.Format(time.RFC3339))
		default:
			warnLine("taking over the lease on %s: its process (pid %d) is gone", firstNonEmpty(held, filepath.Base(lp)), pid)
		}
	}
}

// lockBefore takes a mutex, or gives up at the deadline. sync.Mutex cannot be
// waited on with a timeout, so the wait is a poll on TryLock — the same 25 ms
// tick the pid file uses, against a holder that is in this process and therefore
// milliseconds away.
func lockBefore(m *sync.Mutex, deadline time.Time) bool {
	for {
		if m.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(leasePoll)
	}
}

// claimLease creates the lock file holding pid, path and time, or returns
// fs.ErrExist when somebody else holds it.
//
// The file is built COMPLETE in a sibling temp file and hard linked into place,
// the createFileAtomic idiom. An O_EXCL create followed by a Fprintf leaves a
// window in which the lock exists and is EMPTY: a rival reads pid 0 out of it
// (Atoi("") fails), concludes the owner is gone, and takes over a lease that was
// granted a microsecond ago — and two processes contending normally hit that
// window on every acquisition. link(2) fails with EEXIST if the name is taken,
// so the lock either appears fully formed or loses the race.
func claimLease(lp, name string) error {
	body := fmt.Sprintf("%d\n%s\n%s\n", os.Getpid(), name, time.Now().Format(time.RFC3339))
	f, err := os.CreateTemp(filepath.Dir(lp), ".lca-lock-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, werr := f.WriteString(body)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	switch err := os.Link(tmp, lp); {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return err
	}
	// No hard links on this filesystem. Fall back to the O_EXCL claim with the
	// empty-file window admitted — the expiry above still clears a lock whose
	// writer died inside it, and nothing else in the tree needs links either.
	ex, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr = ex.WriteString(body)
	if cerr := ex.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// releaseLease removes OUR lock and nothing else. After a takeover — ours or
// somebody else's — the path may hold a different process's lock, and a blind
// Remove on the way out would hand a third writer a lease that is still held.
func releaseLease(lp string) {
	if pid, _, _, _ := leaseOwner(lp); pid == os.Getpid() {
		os.Remove(lp)
	}
}

// leaseOwner reads a lock file's three lines: the pid, the display path of the
// file being written, and when the lock was taken. The last result says the file
// was THERE — "absent" and "present but unreadable" lead to opposite decisions,
// and conflating them is what let a third process's live lock be taken over.
func leaseOwner(lp string) (pid int, held string, born time.Time, exists bool) {
	b, err := os.ReadFile(lp)
	if err != nil {
		return 0, "", time.Time{}, false
	}
	lines := strings.Split(string(b), "\n")
	pid, _ = strconv.Atoi(strings.TrimSpace(lines[0]))
	if len(lines) > 1 {
		held = strings.TrimSpace(lines[1])
	}
	if len(lines) > 2 {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(lines[2])); err == nil {
			born = t
		}
	}
	return pid, held, born, true
}

// ── fingerprints ────────────────────────────────────────────────────────────

func sumBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// sumFile hashes a file on disk, or returns "" when it is too big to fingerprint
// or unreadable. "" is never equal to a recorded sum, so a file that cannot be
// hashed is never mistaken for an unchanged one.
func sumFile(abs string) string {
	info, err := os.Stat(abs)
	if err != nil || info.Size() > maxFingerprintBytes {
		return ""
	}
	f, err := os.Open(abs)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
