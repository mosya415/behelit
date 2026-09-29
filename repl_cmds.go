package main

import (
	"context"
	"fmt"
	"os"
	"strings"
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
	section("this session")
	row("turns", fmt.Sprintf("%d model calls · %s", st.Turns, plural(st.Compactions, "compaction", "compactions")))
	row("tokens", fmt.Sprintf("%s in · %s out", kfmt(st.PromptTokens), kfmt(st.OutputTokens)))
	cache := "—"
	if st.PromptTokens > 0 {
		cache = fmt.Sprintf("%d%%", st.CachedTokens*100/st.PromptTokens)
	}
	row("cache", cache+faint(" of the prompt served from the gateway's KV cache"))
	rate := "0%"
	if st.ToolCalls+st.InvalidCalls > 0 {
		rate = fmt.Sprintf("%.1f%%", float64(st.InvalidCalls)*100/float64(st.ToolCalls+st.InvalidCalls))
	}
	line := fmt.Sprintf("%d calls · %d didn't parse (%s) · %d returned an error", st.ToolCalls, st.InvalidCalls, rate, st.ToolErrors)
	if st.InvalidCalls > 0 {
		line = cYellow + gPartial + cReset + " " + line
	}
	row("tools", line)
	if st.Fallbacks > 0 {
		row("gateway", fmt.Sprintf("%s (model switches, waits, repeated turns)", plural(st.Fallbacks, "event", "events")))
	}
	if st.VerifyRuns > 0 {
		row("verify", plural(st.VerifyRuns, "check run", "check runs"))
	}
	row("trace", faint("%s", shortDir(r.orch.tracer.Path)))
	if st.InvalidCalls > 0 {
		hint("a parse failure rate above a fraction of a percent is the harness or the server, not the model — the raw text is in the trace (\"raw\")")
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
