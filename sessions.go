package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Session resume. Each session's full transcript is rewritten to
// transcripts/<id>.json after every turn (recorder.go); /resume reloads one so a
// crashed, killed, or just-closed session can be picked up where it left off.

type sessionMeta struct {
	path    string
	id      string
	mtime   time.Time
	turns   int    // user turns (excludes tool_result messages)
	preview string // first user turn, for the picker
}

// listSessions returns saved transcripts (newest first), skipping the current
// one, with a small preview parsed from each.
func listSessions(dir, exclude string) []sessionMeta {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []sessionMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if p == exclude {
			continue
		}
		msgs, err := loadSession(p)
		if err != nil || len(msgs) == 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		turns, preview := 0, ""
		for _, m := range msgs {
			if m.Role == "user" && !strings.HasPrefix(m.Content, "<tool_result") {
				turns++
				if preview == "" {
					preview = firstLine(m.Content)
				}
			}
		}
		if turns == 0 {
			continue // nothing meaningful to resume
		}
		out = append(out, sessionMeta{p, strings.TrimSuffix(e.Name(), ".json"), info.ModTime(), turns, preview})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mtime.After(out[j].mtime) })
	return out
}

// transcriptPath is where one session's transcript lives. One expression, so
// that "the session the wrapper named" and "the file the recorder writes" can
// never drift apart.
func transcriptPath(cfg Config, id string) string {
	return filepath.Join(cfg.stateDir(), "transcripts", id+".json")
}

// resumableSession finds the session `-session <uid>` names, for the next round
// of a pipeline: the reviewer's comments and a fresh check log go back to the
// SAME session, with its history and its own x-session-id, because the gateway's
// prefix cache makes that cheaper than starting over — and because the model
// already knows what it tried.
//
// The uid is validated before anything is opened, and strictly, because it
// becomes a path: `-session ../../../etc/passwd` is a wrapper interpolating the
// wrong variable, and the answer to it is an error and not a read. Only the
// characters the recorder's own ids are made of are accepted.
func resumableSession(cfg Config, uid string) (sessionMeta, error) {
	if uid == "" {
		return sessionMeta{}, usageErrf("-session needs a session id; `lca report` and the `session` field of a previous -json result both name one")
	}
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return sessionMeta{}, usageErrf("-session %q: a session id is letters, digits, dots, dashes and underscores — that is not one", uid)
		}
	}
	if strings.Contains(uid, "..") {
		return sessionMeta{}, usageErrf("-session %q: that is not a session id", uid)
	}
	p := transcriptPath(cfg, uid)
	msgs, err := loadSession(p)
	if err != nil {
		// Named rather than described: the state directory moves with LCA_DIR, and
		// a wrapper that ran round one with a different LCA_DIR is the likeliest way
		// to be here with a uid that is otherwise perfectly real.
		return sessionMeta{}, usageErrf("-session %s: no transcript at %s — nothing here can continue that session", uid, p)
	}
	if len(msgs) == 0 {
		return sessionMeta{}, usageErrf("-session %s: %s is empty", uid, p)
	}
	// Its project, when the audit remembers it. A uid is only unique within a
	// state directory, and a wrapper that points LCA_DIR at one place for several
	// worktrees — one per ticket, which is exactly their shape — can hand round two
	// the uid of a different ticket's round one. The transcript would load, the
	// model would be told it had already edited files it has never seen, and the
	// diff would be judged against the wrong work. Refused rather than noted: a
	// uid from another tree is a wrapper bug, and continuing would corrupt a
	// ticket quietly.
	if was, ok := sessionRoot(cfg, uid); ok && cfg.Root != "" && was != cfg.Root {
		return sessionMeta{}, usageErrf("-session %s ran in %s, not in %s — that uid belongs to another tree's round; a session id is only unique inside one LCA_DIR", uid, was, cfg.Root)
	}
	return sessionMeta{path: p, id: uid}, nil
}

// sessionRoot is the project root the audit says that session ran in, or false
// when the audit cannot say. Missing evidence is not evidence: an audit that was
// rotated, or a session from before this field existed, must not block a resume
// the operator is entitled to.
func sessionRoot(cfg Config, uid string) (string, bool) {
	f, err := os.Open(filepath.Join(cfg.stateDir(), "audit.jsonl"))
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	root, found := "", false
	for sc.Scan() {
		var e struct {
			Kind    string `json:"kind"`
			Session string `json:"session"`
			Root    string `json:"root"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Kind == "session_start" && e.Session == uid && e.Root != "" {
			root, found = e.Root, true // the last one wins: a session may have been resumed before
		}
	}
	return root, found
}

// lockSession keeps two processes off one session. The transcript is rewritten
// WHOLE after every turn (recorder.go), so two rounds on the same uid do not
// interleave — the loser's turns are silently gone, and the wrapper is told both
// succeeded. A lock whose pid is dead is a crash and is taken over, because a
// killed round two must leave the session resumable.
func lockSession(cfg Config, uid string) (func(), error) {
	path := transcriptPath(cfg, uid) + ".lock"
	for try := 0; try < 2; try++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, nil // an unwritable state dir is not a reason to refuse the run
		}
		b, rerr := os.ReadFile(path)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if rerr == nil && pid > 0 && pidAlive(pid) {
			return nil, usageErrf("-session %s is in use by pid %d — two rounds on one session overwrite each other's turns", uid, pid)
		}
		if err := os.Remove(path); err != nil {
			return nil, nil
		}
	}
	return nil, nil
}

// continueSession puts a saved conversation under the session that is about to
// run, and returns how many messages came back.
//
// The CURRENT system prompt is kept and the saved one dropped, as /resume does:
// the saved one advertises the tools and the project instructions of the round
// that wrote it, and a model told it can call a tool this build no longer has is
// being lied to. But the two being different is the whole cost argument of this
// flag — the gateway keys its cache on the prefix — so when they differ the
// operator is told, in the one place where "the second round was cheap" is
// either true or false.
func continueSession(o *Orchestrator, s *Session, m sessionMeta) (int, error) {
	loaded, err := loadSession(m.path)
	if err != nil {
		return 0, usageErrf("-session %s: %v", m.id, err)
	}
	saved, body := "", loaded
	if len(body) > 0 && body[0].Role == "system" {
		saved, body = body[0].Content, body[1:]
	}
	if len(body) == 0 {
		return 0, usageErrf("-session %s: that transcript holds nothing but a system prompt — there is no round to continue", m.id)
	}
	// A new backing array (cap forced to 1), so appending here can never write
	// into whatever else happens to share the session's slice.
	s.Msgs = append(s.Msgs[:1:1], body...)
	same := saved == "" || saved == s.Msgs[0].Content
	if !same {
		warnLine("-session %s: this round's system prompt is not the one that session ran with — the role, its tools or the project instructions changed, so the gateway has no cached prefix for it and the round costs what a fresh session costs", m.id)
	}
	o.rec.Event("session_continue", map[string]any{"from": m.path, "messages": len(body), "same_prefix": same})
	return len(body), nil
}

// pruneTranscripts keeps only the `keep` most recent transcript files, deleting
// older ones so transcripts/ doesn't grow without bound. keep<=0 disables it.
func pruneTranscripts(dir string, keep int, spare string) {
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type ent struct {
		path  string
		mtime time.Time
	}
	var files []ent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// The session this run is continuing is never collected. The comment at the
		// call site already claimed this; it was not true, so a round two on a
		// session older than `keep` others deleted the very transcript it was about
		// to read — and deleted it for good.
		if spare != "" && strings.TrimSuffix(e.Name(), ".json") == spare {
			continue
		}
		files = append(files, ent{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	if len(files) <= keep {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	for _, x := range files[keep:] {
		os.Remove(x.path)
	}
}

func loadSession(path string) ([]Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var msgs []Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// resumeInto replaces the transcript with a saved one, keeping the CURRENT
// system prompt (protocol/project instructions may have changed) and appending
// the saved conversation. Returns how many messages were restored.
func resumeInto(msgs *[]Message, s sessionMeta) (int, error) {
	loaded, err := loadSession(s.path)
	if err != nil {
		return 0, err
	}
	body := loaded
	if len(body) > 0 && body[0].Role == "system" {
		body = body[1:]
	}
	sys := (*msgs)[0]
	*msgs = append([]Message{sys}, body...)
	return len(body), nil
}

// sessionWhen renders a transcript id's timestamp prefix as a friendly date.
func sessionWhen(id string) string {
	if len(id) >= 15 {
		if t, err := time.Parse("20060102-150405", id[:15]); err == nil {
			return t.Format("2006-01-02 15:04")
		}
	}
	return id
}

// firstLine takes the FIRST LINE of s, and nothing else. It used to also cut it
// at 60 bytes, which is two jobs in one name and quietly defeated every
// width-aware cap downstream of it: the plan's check, the failed step's detail,
// the /tasks title and the run summary were all cut to 60 before the table or the
// truncate that was supposed to decide their length ever saw them. So the check
// went 40 → 60 columns rather than 40 → whole, at every terminal width and even
// in a pipe, where the promise is that every cell is printed whole. The cut was
// also a byte slice and would split a multi-byte rune, and it appended a literal
// UTF-8 "…" into rows the ASCII tier had otherwise rendered with "...".
//
// A caller that wants a length says so, with truncate(), which measures columns
// and uses the tier's own marker.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// sessionLines formats the picker rows for /resume list.
func sessionLines(sessions []sessionMeta) []string {
	var out []string
	for i, s := range sessions {
		if i == 12 {
			break
		}
		out = append(out, fmt.Sprintf("  %s%2d%s  %s  %s%d turns%s  %s",
			cFaint, i+1, cReset, sessionWhen(s.id), cFaint, s.turns, cReset, faint("%s", s.preview)))
	}
	return out
}
