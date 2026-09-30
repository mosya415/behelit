package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// REPL commands for agents, orchestration and workflows.

func (s *Session) setTodosQuiet(t []Todo) {
	s.mu.Lock()
	s.todos = t
	s.mu.Unlock()
}

// handleCustomCommand runs a markdown command template. A command whose agent
// is a subagent (or subtask: true) runs in a child session and its report is
// handed to the primary agent to relay; otherwise the expanded prompt becomes
// the user's message, under the command's agent and model if it names them.
// Returns content != "" when the primary session should now run.
func handleCustomCommand(line string, sess *Session, rec *Recorder) (bool, string) {
	if !strings.HasPrefix(line, "/") {
		return false, ""
	}
	name, args, _ := strings.Cut(line[1:], " ")
	cmd := sess.orch.commands[name]
	if cmd == nil {
		return false, ""
	}
	prompt := expandTemplate(cmd.Template, args)
	rec.Event("command", map[string]any{"name": name, "args": args})

	ag := sess.orch.agents[cmd.Agent]
	if cmd.Agent != "" && ag == nil {
		toolErr(fmt.Sprintf("/%s: unknown agent %q", name, cmd.Agent))
		return true, ""
	}
	if ag != nil && (cmd.Subtask || !ag.isPrimary()) {
		if !ag.isSubagent() {
			toolErr(fmt.Sprintf("/%s: agent %q can't run as a subtask (mode %s)", name, ag.Name, ag.Mode))
			return true, ""
		}
		child, err := sess.orch.newChild(sess, ag, "/"+name)
		if err != nil {
			toolErr(err.Error())
			return true, ""
		}
		if cmd.Model != "" {
			if err := child.SetModel(cmd.Model); err != nil {
				toolErr(err.Error())
				return true, ""
			}
			child.RefreshSystem()
		}
		child.Msgs = append(child.Msgs, Message{Role: "user", Content: prompt})
		ctx, cancel := context.WithCancel(context.Background())
		var interrupted atomic.Bool
		stop := sess.watchInterrupt(ctx, cancel, &interrupted)
		result := sess.orch.runChild(ctx, child)
		stop()
		cancel()
		content := fmt.Sprintf("[The user ran /%s %s, executed by the %s subagent]\n%s\n\nRelay the result above to the user and continue if there is follow-up work.", name, args, ag.Name, result)
		sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: content})
		return true, content
	}

	if ag != nil && ag != sess.agent {
		if err := sess.SetAgent(ag.Name); err != nil {
			toolErr(err.Error())
			return true, ""
		}
		kv("agent", ag.Name)
	}
	if cmd.Model != "" {
		if err := sess.SetModel(cmd.Model); err != nil {
			toolErr(err.Error())
			return true, ""
		}
		sess.RefreshSystem()
		kv("model", cmd.Model)
	}
	sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: prompt})
	return true, prompt
}

// cmdStats is the session's own numbers — the ones that tell a bad model from a
// bad harness: how many tool calls the model made, how many didn't parse, how
// much of the prompt the gateway served from cache.
func (r *Repl) cmdStats(string) bool {
	st := r.sess.stats
	// THE SHEET: the session's own numbers. Both gauges have real denominators —
	// the prompt that was actually sent, and the calls that were actually made —
	// so both are allowed to be pictures, and both print their number beside them.
	pnl := newPanel("the sheet", "this session")
	pnl.Row("turns", fmt.Sprintf("%d model calls"+gSep+"%s", st.Turns, plural(st.Compactions, "compaction", "compactions")))
	pnl.Row("tokens", fmt.Sprintf("%s in"+gSep+"%s out", kfmt(st.PromptTokens), kfmt(st.OutputTokens)))
	cache := gNil
	if st.PromptTokens > 0 {
		cache = fmt.Sprintf("%d%%", st.CachedTokens*100/st.PromptTokens)
	}
	cacheRow := cache + faint(" of the prompt served from the KV cache")
	if g := gaugePct(st.CachedTokens, st.PromptTokens, gaugeCells, cGreen); g != "" {
		cacheRow = g + "  " + cacheRow
	}
	pnl.Row("cache", cacheRow)
	rate := "0%"
	if st.ToolCalls+st.InvalidCalls > 0 {
		rate = fmt.Sprintf("%.1f%%", float64(st.InvalidCalls)*100/float64(st.ToolCalls+st.InvalidCalls))
	}
	line := fmt.Sprintf("%d calls"+gSep+"%d didn't parse (%s)", st.ToolCalls, st.InvalidCalls, rate)
	if st.InvalidCalls > 0 {
		line = cYellow + gPartial + cReset + " " + line
	}
	// the one two-tone bar: what parsed and what did not, in the same track, with
	// the failed fraction never rounded down to nothing
	if g := gaugeSplit(st.ToolCalls, st.InvalidCalls, gaugeCells, cGreen, cRed); g != "" {
		line = g + "  " + line
	}
	pnl.Row("tools", line)
	// The error count is a second sentence about the same calls, so it goes on its
	// own line under them rather than pushing the parse rate off the frame — and it
	// is printed WITH its zero, like the parse count one line above it. Gated on
	// being non-zero, the reader could not tell "none returned an error" from "this
	// build does not report it", and two adjacent counts on one row followed two
	// different rules.
	pnl.Line("%-10s%d returned an error", "", st.ToolErrors)
	if st.Fallbacks > 0 {
		pnl.Row("gateway", fmt.Sprintf("%s (model switches, waits, repeated turns)", plural(st.Fallbacks, "event", "events")))
	}
	if st.VerifyRuns > 0 {
		pnl.Row("verify", plural(st.VerifyRuns, "check run", "check runs"))
	}
	pnl.Row("trace", faint("%s", shortDir(r.orch.tracer.Path)))
	fmt.Println()
	pnl.Print()
	if st.InvalidCalls > 0 {
		// two short hints rather than one long one: a hint is not wrapped (see
		// ui.go), so a sentence that does not fit at 80 columns is a sentence that
		// has to be two
		hint("above a fraction of a percent, that is the harness or the server")
		hint("the raw text of each failed call is in the trace, under \"raw\"")
	}
	return false
}

// cmdRun is the REPL door onto `lca run`: the same runner, the same orchestrator
// and trace, driven inside interruptible so Ctrl-C reaches it as it does
// everywhere else. The CLI path is the deliverable; this keeps a workflow one
// keystroke away mid-conversation.
func (r *Repl) cmdRun(arg string) bool {
	name, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	if name == "" {
		printWorkflows(r.cfg)
		return false
	}
	vars, err := parseVarWords(rest)
	if err != nil {
		errLine("%v", err)
		hint("usage: /run <name> [k=v …]")
		return false
	}
	wf, err := findWorkflow(r.cfg, name)
	if err != nil {
		errLine("%v", err)
		return false
	}
	effective := wf.effectiveVars(vars)
	if err := wf.bind(r.orch, r.sess.agent.Name, effective); err != nil {
		errLine("%v", err)
		return false
	}
	pruneRuns(r.cfg, atoiDefault(os.Getenv("LCA_KEEP_RUNS"), 50))
	dir, st := newRunState(r.cfg, r.orch, wf, r.sess.agent.Name, effective)
	runner, err := newRunner(r.orch, r.sess, wf, dir, st)
	if err != nil {
		errLine("%v", err)
		return false
	}
	defer runner.Close()
	interruptible(r.sess, func(ctx context.Context) { runner.Run(ctx) })
	// Keep the conversation coherent: the lead model sees what the program did.
	r.sess.Msgs = append(r.sess.Msgs,
		Message{Role: "user", Content: fmt.Sprintf("[The user ran the %s workflow]\n%s", wf.Name, runner.digest())},
		Message{Role: "assistant", Content: "Understood.", Agent: r.sess.agent.Name})
	r.sess.saveTranscript()
	return false
}

// probeMembers checks every member's reachability through the cached gate, in
// parallel and bounded, so /members and doctor cost one short ssh per machine
// and not one per row of output. A local member is always reachable.
func probeMembers(ctx context.Context, o *Orchestrator, names []string) map[string]error {
	out := make(map[string]error, len(names))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, min(max(len(names), 1), 8))
	for _, n := range names {
		m := o.member(n)
		if m.IsLocal() {
			continue
		}
		wg.Add(1)
		go func(name string, m *Member) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := m.reach(ctx)
			mu.Lock()
			out[name] = err
			mu.Unlock()
		}(n, m)
	}
	wg.Wait()
	return out
}

// memberSandbox describes what a member may run, for a table cell.
func memberSandbox(o *Orchestrator, m *Member) string {
	j := o.policyOf(m)
	if j == nil {
		return gNil
	}
	switch {
	case j.Unsafe:
		return "unsafe"
	case m != nil && m.Allow != nil:
		return fmt.Sprintf("%d own%s", len(m.Allow), map[bool]string{true: gSep + "sh", false: ""}[j.Shell])
	}
	return fmt.Sprintf("%d team%s", len(j.Allowed), map[bool]string{true: gSep + "sh", false: ""}[j.Shell])
}

func (r *Repl) cmdMembers(string) bool {
	o := r.orch
	names := o.memberNames()
	section("members", faint("%s", plural(len(names), "machine", "machines")))
	reach := probeMembers(context.Background(), o, names)
	byMember := map[string][]string{}
	if o.roles != nil {
		for _, a := range o.roles.Roles {
			n := o.memberFor(a).MemberName()
			byMember[n] = append(byMember[n], a.Name)
		}
	}
	here := r.sess.memberName()
	var rows [][]string
	var fixes []string
	for _, n := range names {
		m := o.member(n)
		label := n
		if n == here {
			label = cBold + n + cReset
		}
		status := cGreen + "ok" + cReset
		if err := reach[n]; err != nil {
			// A machine ssh reaches whose project directory is not there is not
			// "unreachable": the cell names what is actually wrong, and the line
			// below it says what to do.
			status = cRed + map[bool]string{true: "no project dir", false: "unreachable"}[memberDirMissing(err)] + cReset
			fixes = append(fixes, err.Error())
		}
		rows = append(rows, []string{label, faint("%s", m.Where()+dirSuffix(m)), memberSandbox(o, m),
			faint("%s", orDash(strings.Join(byMember[n], ", "))), status})
	}
	// `where` is host:dir — an identifier whose two ends are the two things that
	// name a machine, so when it has to give way it gives way in the middle.
	table([]string{"member", "where", "sandbox", "roles", "status"}, rows, 1)
	for _, f := range fixes {
		errLine("%s", f)
	}
	hint("%s", "pin a role with member: <name> in roles.yaml"+gSep+"a step's member: overrides it")
	if o.legacyFleet() {
		hint("member remote comes from the old remote: block — delegate is off there; move it into members: for cross-machine worktrees")
	}
	return false
}

// dirSuffix shows which directory a member's project is in, next to the host.
func dirSuffix(m *Member) string {
	if m.IsLocal() {
		return ""
	}
	return ":" + m.Rem.Dir
}
