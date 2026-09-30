package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
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
//
// Other permissions a user rule sets to "ask" (task, skill, doom_loop…) are
// their own class; "a" at the prompt trusts every class.
func classOf(kind string) string {
	switch kind {
	case "edit", "write":
		return "edit"
	case "run_command", "run":
		return "run"
	case "webfetch", "web":
		return "web"
	}
	return kind
}

var allClasses = []string{"edit", "run", "web"}

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
}

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
func (a *Approver) Confirm(kind, header, preview string) (approved, auto bool) {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	header = humanHeader(header)
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
	if _, fits, _ := pnl.fitTop(pnl.width()); fits != detail {
		pnl.detail = ""
		pnl.needs(visibleWidth(detail))
		pnl.Line("%s%s%s", cBold, detail, cReset)
	}
	for _, l := range previewLines(preview) {
		pnl.Line("%s", l)
	}
	fmt.Println()
	pnl.Print()
	fmt.Print("   open it?  " + cBold + "y" + cReset + cFaint + " yes" + gSep + cReset + cBold + "n" + cReset + cFaint + " no" + gSep + cReset +
		cBold + "a" + cReset + cFaint + " yes to everything this session" + cReset + "  " + cFaint + gPrompt + cReset + " ")

	// The terminal has to be the ordinary one for a question: under the turn mode
	// the agent's input capture installs, a read returns nothing at once and the
	// answer would not even be echoed.
	defer a.in.PauseCapture()()

	// Anything typed or pasted before the question appeared is not an answer to
	// it: set it aside and give it back to the next prompt.
	a.in.Drain()
	stashed := a.in.TakePending()
	defer a.in.Put(stashed)
	line, err := a.in.ReadString('\n')
	if err != nil {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, false
	case "a", "all":
		a.TrustAll()
		fmt.Println("   " + faint("everything is auto-approved for this session — /approve off undoes it"))
		return true, false
	default:
		return false, false
	}
}

// Mode renders the current trust posture for the banner / status.
func (a *Approver) Mode() string {
	cs := a.TrustedClasses()
	switch {
	case len(cs) == 0:
		return "prompt each action"
	case len(cs) == len(allClasses):
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
	case len(cs) == len(allClasses):
		return "auto"
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
		if w == strings.ToUpper(w) && strings.ToLower(w) != w && !strings.ContainsAny(w, "/.") {
			words[i] = strings.ToLower(w)
		}
	}
	return strings.Join(words, " ")
}
