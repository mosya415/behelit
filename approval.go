package main

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// The soft gate. Permission rules (permission.go) decide allow / deny / ask;
// on "ask" the action is shown to the user and requires an explicit "y". By
// default reads run automatically and edit, write, run_command and webfetch ask.
// This is deliberately advisory — the jail (jail.go) is the hard boundary.
// Both are required: the gate catches intent, the jail catches reach.

// Side-effecting tools group into two trust CLASSES so the user can grant
// standing approval to one kind of action without the other — e.g. auto-run
// allowlisted commands while still confirming every file change.
//
//	"edit" — file mutations: edit, write
//	"run"  — command execution: run_command
//	"web"  — network fetches: webfetch
//	"mcp"  — opening a connection to a configured internal MCP server
//
// Other permissions a user rule sets to "ask" (task, skill, doom_loop…) are
// their own class; "a" at the prompt trusts every class in allClasses.
func classOf(kind string) string {
	switch kind {
	case "edit", "write":
		return "edit"
	case "run_command", "run":
		return "run"
	case "webfetch", "web":
		return "web"
	case "mcp":
		return "mcp"
	case "mcp_read":
		return "mcp_read"
	case "mcp_write":
		return "mcp_write"
	}
	return kind
}

// allClasses is what "a" and -y trust. It holds "mcp" and deliberately NOT
// "mcp_write": -y is how an overnight run is started (main.go, eval.go,
// workflow.go all call TrustAll), usage() documents -y as "approve edits,
// commands and fetches", and a run nobody is watching is exactly the run that
// must not comment on a ticket because a model thought it would help.
var allClasses = []string{"edit", "run", "web", "mcp"}

// Approver owns the interactive gate and the session's per-class trust set.
// It is the single place a side-effecting action is confirmed, so every path is
// recorded consistently — including whether the yes was typed by the user or
// granted automatically by a trusted class (audited via the `auto` return).
type Approver struct {
	in       *Input
	promptMu sync.Mutex      // one prompt at a time (subagents ask concurrently)
	stateMu  sync.Mutex      // guards trusted/all (short holds only)
	trusted  map[string]bool // trusted classes: "edit", "run", "web", …
	all      bool            // "a" / -y: trust every class, including ones not listed
	quiet    bool            // don't print auto-approvals (unattended runs: eval)
	// noAsk is why there is nobody to ask: "stdin is not a terminal", "-json".
	// Non-empty turns every question into an immediate refusal — see Confirm.
	noAsk string
	// interrupted: Ctrl-C was pressed at a question. The turn ends; the loop takes
	// this with TakeInterrupt.
	interrupted atomic.Bool
}

// Unattended records that no human can answer a question on this run, with the
// reason to put in the refusal. Set from stdin not being a terminal, or from
// -json, or by eval and workflow, which are unattended by construction.
//
// It is NOT the same thing as trust. -y grants edits, commands and fetches and
// deliberately withholds mcp_write (see allClasses); a run with -y and nobody
// watching still reached a door for a ticket write, and a door with no keyboard
// behind it is a hang — in a cron job, a claimed ticket and a held worktree until
// somebody notices in the morning. So the two live side by side: trust decides
// what needs no answer, this decides what happens to the questions that remain.
func (a *Approver) Unattended(why string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.noAsk = why
}

// unattendedWhy is the reason, or "" when there is a human to ask.
func (a *Approver) unattendedWhy() string {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.noAsk
}

// TakeInterrupt reports (once) that the operator pressed Ctrl-C at a question.
// The loop reads it to end the turn: answering "no" to one command and stopping
// the whole turn are different intentions, and Ctrl-C is the second.
func (a *Approver) TakeInterrupt() bool { return a.interrupted.Swap(false) }

func NewApprover(in *Input) *Approver {
	return &Approver{in: in, trusted: map[string]bool{}}
}

func (a *Approver) Trust(class string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.trusted[classOf(class)] = true
}

func (a *Approver) TrustAll() {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	for _, c := range allClasses {
		a.trusted[c] = true
	}
	a.all = true
}

func (a *Approver) Clear() {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.trusted = map[string]bool{}
	a.all = false
}

// Trusts reports standing approval for a class. It never waits on a pending
// prompt, so background subagents and the status line can check it freely.
func (a *Approver) Trusts(class string) bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	// mcp_write is the one class "a" / -y does not reach. Standing trust for it is
	// typed at this terminal, for this session, with /approve mcp-write, and
	// nothing else grants it — not a flag, not a config file's approve: key.
	if classOf(class) == "mcp_write" {
		return a.trusted["mcp_write"]
	}
	return a.all || a.trusted[classOf(class)]
}

// TrustedClasses returns the trusted classes in stable order (for audit/display).
func (a *Approver) TrustedClasses() []string {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	var cs []string
	for c := range a.trusted {
		if a.trusted[c] {
			cs = append(cs, c)
		}
	}
	sort.Strings(cs)
	return cs
}

// Confirm reports whether an action of the given kind may proceed and whether it
// was auto-granted by a trusted class. Prompt answers:
//
//	y / yes  approve just this action
//	n / <Enter>  deny (default)
//	a / all  approve this and auto-approve every later action this session
//
// At an mcp_write door "a" is NOT offered and does not grant anything. It used to
// call TrustAll(), which printed "everything is auto-approved for this session",
// really did turn on standing trust for every file edit, command and fetch — and
// did not grant mcp_write, because Trusts() short-circuits that class, so the next
// ticket write asked again. One keystroke answering a Jira question was a lie and
// an escalation at the same time. Standing trust for a write is /approve mcp-write,
// typed on purpose, and nothing else.
func (a *Approver) Confirm(kind, header, preview string) (approved, auto bool) {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	header = humanHeader(header)
	writeDoor := classOf(kind) == "mcp_write"
	if a.Trusts(kind) {
		// Silent for the two classes the transcript already draws a gutter line for
		// with the same target on it: an auto-approved edit printed "/ edit sum.go"
		// and then "· edit sum.go (auto-approved)", and an auto-approved command
		// printed the pair the other way round. Two rows per action for one fact, in
		// the transcript the gutter exists to make scannable. The posture itself is
		// on the status line, in the banner and in the audit log.
		if !a.quiet && classOf(kind) != "edit" && classOf(kind) != "run" {
			sayLine(fmt.Sprintf(" %s%s %s  %s(auto-approved)%s", cFaint, gNone, header, cFaint, cReset))
		}
		return true, true
	}
	// No keyboard: refuse here, before a byte is read and before the door is
	// drawn. This is the whole of "never wait for a human" — every question in the
	// program that is a tool approval comes through here, and the ones that are
	// not (the pickers, the wizard's yes/no, its text fields) refuse on
	// in.IsTTY() before they read either. The caller turns this into the error the
	// model sees and writes the audit line; this is the operator's copy, in the
	// transcript, where "why did it not do the thing" gets answered.
	if why := a.unattendedWhy(); why != "" {
		if !a.quiet {
			sayLine(fmt.Sprintf(" %s%s %s  %s(refused — %s)%s", cFaint, gDown, header, cFaint, why, cReset))
		}
		return false, false
	}
	// Hold the terminal while waiting, so concurrent subagent output can't
	// scroll the prompt away.
	outMu.Lock()
	defer outMu.Unlock()

	// A door: the one frame on the screen that is lit, because it is the one thing
	// on the screen that is waiting for you. The permission class and its target
	// are humanHeader()'s output verbatim — "edit sum.go", "run" — and the asker's
	// own bracketed tag, when a subagent is the one knocking, goes to the right so
	// the question reads the same whoever asked it.
	tag, detail := splitAsker(header)
	pnl := newPanel("a door", detail).door()
	if tag != "" {
		pnl.tag(tag)
	}
	// The target is the whole question, and fitTop gives the title course up before
	// it gives up the frame — so a long path or a URL was silently ellipsized there.
	// For read, list, glob, grep, webfetch, skill and edit there is no second copy
	// of it on the screen: webfetch passes an EMPTY preview and edit's preview is a
	// diff with no filename in it, so a cut header is somebody typing y without
	// having seen what they agreed to. When it does not fit the course WHOLE it
	// comes out of the course and goes on the first content line instead, where
	// panelSplit hard-splits it and never loses a byte.
	if _, fits, _ := pnl.fitTop(pnl.ceiling()); fits != detail {
		pnl.detail = ""
		pnl.needs(visibleWidth(detail))
		pnl.Line("%s%s%s", cBold, detail, cReset)
	}
	for _, l := range previewLines(preview) {
		pnl.Line("%s", l)
	}
	fmt.Println()
	pnl.Print()
	answers := cBold + "y" + cReset + cFaint + " yes" + gSep + cReset + cBold + "n" + cReset + cFaint + " no" + cReset
	if !writeDoor {
		answers += cFaint + gSep + cReset + cBold + "a" + cReset + cFaint + " yes to everything this session" + cReset
	}
	fmt.Print("   open it?  " + answers + "  " + cFaint + gPrompt + cReset + " ")

	// The terminal has to be the ordinary one for a question: under the turn mode
	// the agent's input capture installs, a read returns nothing at once and the
	// answer would not even be echoed.
	defer a.in.PauseCapture()()

	// Anything typed or pasted before the question appeared is not an answer to
	// it: set it aside and give it back to the next prompt.
	a.in.Drain()
	stashed := a.in.TakePending()
	defer a.in.Put(stashed)
	// Ctrl-C while the question is open means "stop", not "kill the program". The
	// prompt runs with the ordinary terminal discipline (that is what makes the
	// answer readable and echoed), so ISIG is live and an unhandled SIGINT would
	// end the process where the operator expected to abandon one command.
	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, os.Interrupt)
	defer signal.Stop(sigch)
	type answer struct {
		line string
		err  error
	}
	got := make(chan answer, 1)
	// abandoned is closed when this question is over without an answer — the
	// Ctrl-C path below. The goroutine is still inside ReadString then and there
	// is no way to interrupt a blocking read of a terminal, so it will finish, and
	// the line it finishes with belongs to whatever asked NEXT. Sending it into
	// `got` — which is buffered, so the send always succeeds — dropped it on the
	// floor: the first character the operator typed at the next question was
	// swallowed by a question that was already over, and the `y` they typed
	// arrived as the empty line that means deny. Handed back through in.Put
	// instead, which is where Drain/TakePending already put anything typed before
	// a question appeared, so the next prompt treats it as the type-ahead it is.
	abandoned := make(chan struct{})
	go func() {
		l, e := a.in.ReadString('\n')
		select {
		case <-abandoned:
			if e == nil {
				a.in.Put(l)
			}
		default:
			got <- answer{l, e}
		}
	}()
	var line string
	select {
	case <-sigch:
		a.interrupted.Store(true)
		close(abandoned)
		fmt.Println("   " + warn("%s interrupted — nothing ran", gDown))
		return false, false
	case res := <-got:
		if res.err != nil {
			return false, false
		}
		line = res.line
	}
	approved, trustAll, note := doorAnswer(line, writeDoor)
	if trustAll {
		a.TrustAll()
	}
	if note != "" {
		// This is the line that tells the operator they have just granted standing
		// approval for the session, so it is the worst one on the screen to render
		// badly — and at 90 columns (118 for the mcp-write variant) it soft-wrapped
		// with its continuation in column 1, directly under a 77-column door frame.
		// Same measure and same indent as a tool error's hints.
		for _, l := range wrapHint(note, min(houseWidth()-4, proseMax)) {
			fmt.Println("   " + faint("%s", l))
		}
	}
	return approved, false
}

// doorAnswer is what one typed line means at a door, split out of Confirm so it
// can be tested without a terminal — the "a" case is the one that was wrong for a
// whole feature and could not be exercised otherwise (Confirm stashes anything
// typed before the question, on purpose, so a scripted Input can only ever deny).
//
// At an mcp_write door "a" grants NOTHING standing. It approves the write in front
// of the operator and says what it did not do, because that keystroke used to
// print "everything is auto-approved for this session", really did trust every
// later edit, command and fetch, and still left mcp_write untrusted so the next
// ticket write asked again.
func doorAnswer(line string, writeDoor bool) (approved, trustAll bool, note string) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, false, ""
	case "a", "all":
		if writeDoor {
			return true, false, `this write only — "a" does not auto-approve mcp writes; /approve mcp-write grants them for the session`
		}
		return true, true, "everything except mcp writes is auto-approved for this session — /approve off undoes it"
	default:
		return false, false, ""
	}
}

// trustsEvery reports standing trust for every class in allClasses.
//
// Counting was a lie as soon as a class OUTSIDE allClasses could be trusted:
// /approve task plus /approve edit plus /approve run is three trusted classes and
// not "everything", and with mcp_write in the set it would have been four. Both
// callers below test the set instead.
func (a *Approver) trustsEvery() bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	for _, c := range allClasses {
		if !a.trusted[c] {
			return false
		}
	}
	return true
}

// Mode renders the current trust posture for the banner / status.
func (a *Approver) Mode() string {
	cs := a.TrustedClasses()
	switch {
	case len(cs) == 0:
		return "prompt each action"
	case a.trustsEvery() && !a.Trusts("mcp_write"):
		return "auto-approve all (session); mcp writes still ask"
	case a.trustsEvery():
		return "auto-approve all (session)"
	default:
		return "auto-approve: " + strings.Join(cs, ", ")
	}
}

// ModeShort is a compact form of Mode for the status line: "ask", "auto", or
// "auto:run,edit".
func (a *Approver) ModeShort() string {
	cs := a.TrustedClasses()
	switch {
	case len(cs) == 0:
		return "ask"
	case a.trustsEvery() && !a.Trusts("mcp_write"):
		return "auto"
	case a.trustsEvery():
		return "auto+w"
	default:
		return "auto:" + strings.Join(cs, ",")
	}
}

// splitAsker pulls a subagent's "[coder t2]" tag off the front of a header, so
// the frame can put WHO is asking at the far end of its top course and leave the
// question itself reading the same whoever asked it.
func splitAsker(h string) (tag, rest string) {
	if strings.HasPrefix(h, "[") {
		if i := strings.IndexByte(h, ']'); i > 0 {
			return h[:i+1], strings.TrimSpace(h[i+1:])
		}
	}
	return "", h
}

// previewLines colours the two signs a diff preview carries and nothing else.
// The signs are conventional data — everyone reads unified-diff notation, and
// edit.go hands the same ones to the model — so they are tinted and never
// renamed, and every other line of a preview is left exactly as its caller built
// it, because a write preview's body is the file.
func previewLines(preview string) []string {
	if preview == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(preview, "\n"), "\n") {
		switch t := strings.TrimLeft(l, " "); {
		case strings.HasPrefix(t, "- "):
			out = append(out, cRed+l+cReset)
		case strings.HasPrefix(t, "+ "):
			out = append(out, cGreen+l+cReset)
		default:
			out = append(out, l)
		}
	}
	return out
}

// humanHeader turns a tool's approval header ("EDIT a.go", "[coder t2] RUN")
// into sentence case: "edit a.go", "[coder t2] run".
func humanHeader(h string) string {
	words := strings.Fields(h)
	for i, w := range words {
		if allCapLetters(w) {
			words[i] = strings.ToLower(w)
		}
	}
	return strings.Join(words, " ")
}

// allCapLetters: a word of nothing but capital letters is a keyword of OURS (EDIT,
// RUN, MCP WRITE) and reads better lower-cased. A word with a digit, a dash or a
// dot in it is somebody's identifier — an issue key like OPS-412, now that the mcp
// door names the ticket it is about — and is left exactly as it came.
func allCapLetters(w string) bool {
	for _, r := range w {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return w != ""
}
