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
	// after is the hex sha256 of what this change LEFT on disk. /undo is a writer
	// like any other, and without this it was the only one with no guard at all:
	// it restored in-memory bytes with os.WriteFile and no check, so a subagent's
	// or the user's editor's later work on that file was discarded silently.
	after string
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

func recordChange(name, abs, tool string, before []byte, existed bool, after string) {
	changeMu.Lock()
	changeLog = append(changeLog, fileChange{name, abs, before, existed, tool, after})
	changeMu.Unlock()
}

func resetChanges() {
	changeMu.Lock()
	changeLog = nil
	changeMu.Unlock()
}

// undoLast reverts the most recent applied change: restores the prior bytes, or
// deletes the file if the change had created it. Returns a description.
//
// It is a writer, so it takes the file's lease and checks staleness like every
// other writer: if the file no longer holds what that change left behind,
// someone wrote it afterwards and /undo would throw their work away. `force`
// says the human looked and wants it anyway. The log entry is only popped once
// the undo is actually going ahead — a refusal must leave /undo able to try
// again after the human has looked.
func undoLast(o *Orchestrator, force bool) (string, bool) {
	changeMu.Lock()
	n := len(changeLog)
	if n == 0 {
		changeMu.Unlock()
		return "", false
	}
	c := changeLog[n-1]
	changeMu.Unlock()

	// The lease FIRST and changeMu second, never the other way round: a writer
	// holds the file's lease and then takes changeMu to record what it did, so
	// taking them in the opposite order here would wedge /undo against a subagent
	// that is mid-write. That is why this peeks, leases, and only then re-takes
	// the log — a lock order is a property of the whole program, not of one
	// function.
	release, err := o.leaseFile(c.name, c.abs)
	if err != nil {
		return "error: " + err.Error(), true
	}
	defer release()
	if !force && c.after != "" {
		if now := sumFile(c.abs); now != c.after {
			return fmt.Sprintf("%s changed after that edit (a subagent or your editor wrote it) — /undo would discard that; /undo force to do it anyway", c.name), true
		}
	}
	changeMu.Lock()
	defer changeMu.Unlock()
	// A subagent may have recorded a change while we waited for the lease. Undo
	// the entry that was actually checked, or nothing.
	if len(changeLog) != n || changeLog[n-1].abs != c.abs || changeLog[n-1].after != c.after {
		return "another change landed while /undo was waiting for the file — run /undo again", true
	}
	changeLog = changeLog[:len(changeLog)-1]
	if !c.existed {
		if err := os.Remove(c.abs); err != nil && !os.IsNotExist(err) {
			return "error removing " + c.name + ": " + err.Error(), true
		}
		return "removed " + c.name + " (undo of create)", true
	}
	if err := writeFileAtomic(c.abs, c.before); err != nil {
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
