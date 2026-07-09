package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60] + "…"
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
