package main

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
	audit   *os.File
	session string // path to this session's transcript file
	id      string
	uid     string
	user    string
	pid     int
}

func NewRecorder(cfg Config) (*Recorder, error) {
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	tdir := filepath.Join(cfg.Dir, "transcripts")
	if err := os.MkdirAll(tdir, 0o700); err != nil {
		return nil, err
	}

	auditPath := filepath.Join(cfg.Dir, "audit.jsonl")
	f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}

	pid := os.Getpid()
	id := time.Now().Format("20060102-150405") + "-" + strconv.Itoa(pid)

	r := &Recorder{
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
	r.audit.Write(append(line, '\n'))
}

// Transcript rewrites the session file with the current message list.
func (r *Recorder) Transcript(msgs []Message) {
	if r == nil {
		return
	}
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(r.session, data, 0o600)
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
