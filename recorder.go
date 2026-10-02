package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Recorder owns the two on-disk artifacts of a session, both created 0600 under
// the user's own uid (the process runs as them — the audit trail is naturally
// per-user, no extra plumbing):
//
//   - audit.jsonl : append-only, one JSON event per line. The forensic record of
//     every tool the agent invoked, whether it was approved, and how it turned
//     out. Never rewritten.
//   - transcripts/<session>.json : the full running message transcript, rewritten
//     after every turn so a crashed or killed session is still reviewable.
type Recorder struct {
	mu      sync.Mutex
	dir     string
	audit   *os.File
	session string // path to this session's transcript file
	id      string
	uid     string
	user    string
	pid     int

	// checkRound is the suffix this process's check logs carry, decided once on
	// the first one written (checktail.go's checkRoundOf): "" for a session's
	// first round and ".r2" for the round a `-session <uid>` continues, so a
	// second round cannot overwrite the file the first round's result object
	// named. checkRoundSet separates "no suffix" from "not asked yet".
	checkRound    string
	checkRoundSet bool
}

func NewRecorder(cfg Config) (*Recorder, error) { return NewRecorderOn(cfg, "") }

// NewRecorderOn is NewRecorder continuing an EXISTING session: `-session <uid>`
// adopts that uid instead of minting one from the clock and the pid.
//
// One field decides the whole of the second round's identity, which is why it is
// done here and not in four places. Everything downstream reads this id: the
// session's UID (NewPrimary), and therefore the x-session-id header the gateway
// keys its KV cache on; the transcript, so the second round is appended to the
// first's file and the history is visible in one place; the trace, which is
// opened O_APPEND, so both rounds' turns land in one file; and the `session`
// field of the result object. Minting a new id and then patching those back up
// would have been four chances to leave one of them pointing at a session that
// never existed.
//
// The id is validated by the caller, because it becomes a FILE NAME: see
// resumableSession.
func NewRecorderOn(cfg Config, id string) (*Recorder, error) {
	dir := cfg.stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tdir := filepath.Join(dir, "transcripts")
	if err := os.MkdirAll(tdir, 0o700); err != nil {
		return nil, err
	}

	auditPath := filepath.Join(dir, "audit.jsonl")
	f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}

	pid := os.Getpid()
	if id == "" {
		id = time.Now().Format("20060102-150405") + "-" + strconv.Itoa(pid)
	}

	r := &Recorder{
		dir:     dir,
		audit:   f,
		session: filepath.Join(tdir, id+".json"),
		id:      id,
		pid:     pid,
	}
	if u, err := user.Current(); err == nil {
		r.uid, r.user = u.Uid, u.Username
	}
	return r, nil
}

// Event appends one structured line to the audit log. Base identity/time fields
// are added automatically; callers pass only the event-specific fields.
func (r *Recorder) Event(kind string, fields map[string]any) {
	if r == nil {
		return
	}
	rec := map[string]any{
		"ts":      time.Now().Format(time.RFC3339),
		"session": r.id,
		"pid":     r.pid,
		"uid":     r.uid,
		"user":    r.user,
		"kind":    kind,
	}
	for k, v := range fields {
		rec[k] = v
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	// The audit records the command lines and the tool results of a run, so a
	// `curl -H "Authorization: Bearer $TOK"` the model ran is in here verbatim.
	// It is not an artefact anybody attaches to a ticket, but it is one more file
	// under $LCA_DIR that somebody will eventually copy somewhere, and one line
	// of JSON costs nothing to scrub.
	line = redactBytes(line)
	r.mu.Lock()
	r.audit.Write(append(line, '\n'))
	r.mu.Unlock()
}

// Transcript rewrites the session file with the current message list.
//
// It RETURNS the failure now, and writes through a temporary file. The old
// shape — os.WriteFile with its error discarded — was two problems in one line.
// O_TRUNC comes first, so on ENOSPC, EDQUOT or a read-only state directory the
// previous good transcript was destroyed and nothing was written in its place;
// and nobody was told, so the run reported `status: passed` with the path of a
// 0-byte file, and the wrapper attached that to the ticket as the record of the
// run. Writing beside it and renaming makes a failed write leave the last good
// transcript standing, which is the only outcome where something is still
// readable afterwards.
func (r *Recorder) Transcript(msgs []Message) error {
	if r == nil {
		return nil
	}
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	// This file goes to Jira as an attachment (requirement P2-2), which is the
	// reason the scrub is here and not at the two publication sinks it started
	// at: the transcript holds every tool result the model saw, and a check that
	// echoed its environment on failure put a live token in one of them.
	return writeRecordAtomic(r.session, redactBytes(data), 0o600)
}

// writeRecordAtomic writes one of the recorder's own files through a temporary
// file beside it and renames it over the target, so a write that fails halfway
// cannot destroy what was there. It is deliberately not edit.go's
// writeFileAtomic: that one is for files in the USER's repository and carries
// their mode, their owner and their symlinks across. These are lca's own, in
// lca's own state directory, and the only thing they need is that a failure
// leaves the last good copy standing.
func writeRecordAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := tempBeside(path, data, perm)
	if err != nil {
		return err
	}
	return renameRecord(tmp, path)
}

// writeRecordAtomicString is writeRecordAtomic for a record that is already a
// string, which is the check log: a stand's output is megabytes and []byte(s)
// copies all of them for nothing.
func writeRecordAtomicString(path, data string, perm fs.FileMode) error {
	tmp, err := tempBesideString(path, data, perm)
	if err != nil {
		return err
	}
	return renameRecord(tmp, path)
}

func renameRecord(tmp, path string) error {
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ChildTranscript writes a subagent session's transcript beside the main one
// (transcripts/subagents/<session>-<task id>.json), so delegated work is as
// reviewable as the primary conversation.
func (r *Recorder) ChildTranscript(taskID string, msgs []Message) {
	if r == nil {
		return
	}
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return
	}
	p := childTranscriptPath(r.dir, r.id, taskID)
	os.MkdirAll(filepath.Dir(p), 0o700)
	writeRecordAtomic(p, redactBytes(data), 0o600)
}

func (r *Recorder) SessionPath() string { return r.session }

func (r *Recorder) Close() {
	if r != nil && r.audit != nil {
		r.audit.Close()
	}
}

// summarize collapses a tool result to a short single line for the audit log —
// we record that a file was touched and the outcome, not its full contents.
func summarize(s string) string {
	for i, c := range s {
		if c == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
