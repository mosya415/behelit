package main

import (
	"fmt"
	"os"
	"sync"
)

// Session change tracking. Every applied edit/write snapshots the file's prior
// bytes, so the user can review the agent's work (`/diff`) and revert it
// (`/undo`) — a safety net that matches the approval-first design. Backups live
// in memory for the session; the full audit log on disk is the durable record.

type fileChange struct {
	name    string // display path (as the model wrote it)
	abs     string // jail-resolved path
	before  []byte // file contents before this change
	existed bool   // whether the file existed before this change
	tool    string // "edit" | "write"
}

var (
	changeMu  sync.Mutex // subagents apply changes concurrently
	changeLog []fileChange
)

// snapshot reads a file's current bytes, reporting whether it existed — call it
// BEFORE applying a change so recordChange can capture the prior state.
func snapshot(abs string) (before []byte, existed bool) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, false
	}
	return data, true
}

func recordChange(name, abs, tool string, before []byte, existed bool) {
	changeMu.Lock()
	changeLog = append(changeLog, fileChange{name, abs, before, existed, tool})
	changeMu.Unlock()
}

func resetChanges() {
	changeMu.Lock()
	changeLog = nil
	changeMu.Unlock()
}

// undoLast reverts the most recent applied change: restores the prior bytes, or
// deletes the file if the change had created it. Returns a description.
func undoLast() (string, bool) {
	changeMu.Lock()
	defer changeMu.Unlock()
	if len(changeLog) == 0 {
		return "", false
	}
	c := changeLog[len(changeLog)-1]
	changeLog = changeLog[:len(changeLog)-1]
	if !c.existed {
		if err := os.Remove(c.abs); err != nil && !os.IsNotExist(err) {
			return "error removing " + c.name + ": " + err.Error(), true
		}
		return "removed " + c.name + " (undo of create)", true
	}
	if err := os.WriteFile(c.abs, c.before, 0o644); err != nil {
		return "error restoring " + c.name + ": " + err.Error(), true
	}
	return "reverted " + c.name, true
}

// sessionDiffs renders one diff per changed file, comparing the file's state at
// its FIRST touch this session against its current contents on disk.
func sessionDiffs() []string {
	changeMu.Lock()
	defer changeMu.Unlock()
	type orig struct {
		before  []byte
		existed bool
		name    string
	}
	firsts := map[string]*orig{}
	var order []string
	for _, c := range changeLog {
		if _, ok := firsts[c.abs]; !ok {
			firsts[c.abs] = &orig{c.before, c.existed, c.name}
			order = append(order, c.abs)
		}
	}
	var out []string
	for _, abs := range order {
		o := firsts[abs]
		cur, _ := os.ReadFile(abs)
		beforeStr := ""
		if o.existed {
			beforeStr = string(o.before)
		}
		added, removed, body := lineDiff(beforeStr, string(cur))
		tag := ""
		if !o.existed {
			tag = faint("  (new)")
		} else if len(cur) == 0 {
			tag = faint("  (deleted)")
		}
		head := fmt.Sprintf("  %s%s%s  %s+%d%s %s-%d%s%s",
			cBold, o.name, cReset, cGreen, added, cReset, cRed, removed, cReset, tag)
		out = append(out, head)
		if body != "" {
			out = append(out, body)
		}
	}
	return out
}
