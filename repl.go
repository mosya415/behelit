package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The interactive session. Every slash command lives in one registry
// (replCommands below): /help, the "/" completion menu and dispatch are all
// generated from it, so what the help says is exactly what exists — and a
// command that doesn't apply (hosted providers when a gateway team is set up,
// Slurm discovery, skills when there are none) simply isn't shown.

type Repl struct {
	cfg   Config
	orch  *Orchestrator
	sess  *Session
	local *Client
	ed    *LineEditor
	in    *Input
	notes []string

	// Where each effective setting came from, and what this session changed and
	// has not written. /config reads the first, /save the second, and /exit says
	// one line about it — see repl_config.go.
	cfgSrc  map[string]settingSource
	unsaved map[string]string
	// applying is set while /set drives one of the typed commands (/model,
	// /endpoint, /approve, /tier) on its way to writing the file, so noteChange
	// knows not to advise a /save for something about to be persisted two lines
	// later. See applyLive.
	applying bool

	prefill string // /edit: the next prompt starts with this text
}

// editor is the line editor, made on demand: /setup and the pickers ask for a
// line before Loop has built one (first-run setup runs before the prompt opens).
func (r *Repl) editor() *LineEditor {
	if r.ed == nil {
		r.ed = NewLineEditor(r.in)
	}
	return r.ed
}

// fieldEditor is a one-field prompt inside a section: the wizard's, and nothing
// else's. It is NOT r.ed, which Loop wires with the "/" menu, @file completion
// and the status line — under a wizard field that meant an 80-column fence
// straddling a 76-column rule, the REPL status line drawn under "gateway url",
// and every value typed coming back at the next prompt under ↑. A fresh one per
// field, because a field that remembers is the bug.
func (r *Repl) fieldEditor() *LineEditor {
	ed := NewLineEditor(r.in)
	ed.bare, ed.noHistory = true, true
	return ed
}

type replCmd struct {
	name    string
	aliases []string
	args    string
	desc    string
	group   string
	show    func(r *Repl) bool             // nil = always listed
	run     func(r *Repl, arg string) bool // true = run a model turn afterwards
}

var cmdGroups = []string{"session", "agents", "turn", "review", "modes", "model", "custom"}

func (r *Repl) teamMode() bool { return r.orch.roles != nil }

func replRegistry() []replCmd {
	notTeam := func(r *Repl) bool { return !r.teamMode() }
	return []replCmd{
		{name: "/help", aliases: []string{"/?"}, args: "[all]", desc: "commands and keys", group: "session", run: (*Repl).cmdHelp},
		{name: "/resume", args: "[list|<n>]", desc: "continue a previous session", group: "session", run: (*Repl).cmdResume},
		{name: "/compact", desc: "summarize the conversation to free context", group: "session", run: (*Repl).cmdCompact},
		{name: "/reset", desc: "start over (clears the conversation and change log)", group: "session", run: (*Repl).cmdReset},
		{name: "/exit", aliases: []string{"/quit"}, desc: "quit", group: "session"},
		{name: "/setup", args: "[models]", desc: "pick the models and give them roles", group: "session", run: (*Repl).cmdSetup},
		{name: "/config", args: "[<key>]", desc: "every effective setting and where it comes from", group: "session", run: (*Repl).cmdConfig},
		{name: "/set", args: "[-user] <key> <value>", desc: "change a setting and keep it in the config file", group: "session", run: (*Repl).cmdSet},
		{name: "/save", args: "[-user] [<key> …]", desc: "keep this session's settings in the config file", group: "session", run: (*Repl).cmdSave},

		{name: "/agent", args: "[<name>]", desc: "show or switch the primary agent / role", group: "agents", run: (*Repl).cmdAgent},
		{name: "/agents", desc: "the team: roles and agents", group: "agents", run: (*Repl).cmdAgents},
		{name: "/role", aliases: []string{"/roles"}, args: "[<name> <setting> <value>]", desc: "show or change roles by hand (model, effort, tools, check)", group: "agents", run: (*Repl).cmdRole},
		{name: "/delegate", args: "<role> <task>", desc: "hand one task to a role now (worktree + verifier)", group: "agents",
			show: func(r *Repl) bool { return r.teamMode() && r.orch.canDelegate(r.sess.memberOf()) }, run: (*Repl).cmdDelegate},
		{name: "/members", desc: "the machines the team works on, and whether they answer", group: "agents",
			show: func(r *Repl) bool { return len(r.orch.memberNames()) > 1 }, run: (*Repl).cmdMembers},
		{name: "/run", args: "<name> [k=v …]", desc: "run a workflow (deterministic steps)", group: "agents", run: (*Repl).cmdRun},
		{name: "/tasks", args: "[<id>]", desc: "subagent runs and their results", group: "agents", run: (*Repl).cmdTasks},
		{name: "/todo", aliases: []string{"/todos"}, desc: "the agent's todo list", group: "agents", run: (*Repl).cmdTodo},
		{name: "/skills", desc: "skills agents can load", group: "agents", show: func(r *Repl) bool { return len(r.orch.skills) > 0 }, run: (*Repl).cmdSkills},

		{name: "/retry", desc: "regenerate the last answer", group: "turn", run: (*Repl).cmdRetry},
		{name: "/edit", desc: "edit and resend your last message", group: "turn", run: (*Repl).cmdEdit},

		{name: "/diff", desc: "what the agents changed this session", group: "review", run: (*Repl).cmdDiff},
		{name: "/undo", desc: "revert the last change", group: "review", run: (*Repl).cmdUndo},
		{name: "/context", desc: "context size, cache, biggest outputs", group: "review", run: (*Repl).cmdContext},
		{name: "/stats", desc: "this session: turns, tokens, cache, tool-call failures", group: "review", run: (*Repl).cmdStats},
		// Short arg strings on purpose: /help pads its name column to the WIDEST
		// entry, so one 50-character synopsis pushed all twenty-five descriptions
		// past column 56 and wrapped every one of them at 80. Each command's own
		// usage line carries the flags.
		{name: "/doctor", args: "[flags]", desc: "check gateway, roles, tool calls, sandbox, members", group: "review", run: (*Repl).cmdDoctor},
		{name: "/report", args: "[<id>] [-open]", desc: "render this session's trace as one HTML file", group: "review", run: (*Repl).cmdReport},
		{name: "/eval", args: "<dir> [flags]", desc: "run evaluation tasks", group: "review", run: (*Repl).cmdEval},

		{name: "/approve", args: "[on|off|run|edit]", desc: "what runs without asking", group: "modes", run: (*Repl).cmdApprove},
		{name: "/think", args: "[on|off|last]", desc: "show the model's reasoning", group: "modes", run: (*Repl).cmdThink},
		{name: "/loop", args: "[on|off]", desc: "keep working until the task is done", group: "modes", run: (*Repl).cmdLoop},
		{name: "/unsafe", args: "[on|off]", desc: "lift the sandbox (any path, any command)", group: "modes", run: (*Repl).cmdUnsafe},

		{name: "/model", args: "[<name>]", desc: "show or switch the model", group: "model", run: (*Repl).cmdModel},
		{name: "/tier", args: "[<name>]", desc: "show or switch the active model tier", group: "model", show: hasTiers, run: (*Repl).cmdTier},
		{name: "/providers", desc: "hosted API presets and keys", group: "model", show: notTeam, run: (*Repl).cmdProviders},
		{name: "/endpoint", aliases: []string{"/ep"}, args: "[<n>|url]", desc: "list or switch endpoints", group: "model", run: (*Repl).cmdEndpoint},
		{name: "/discover", aliases: []string{"/disc"}, desc: "find models on the Slurm cluster", group: "model", show: notTeam, run: (*Repl).cmdDiscover},
	}
}

var registry []replCmd

func init() { registry = replRegistry() } // deferred: handlers refer back to the registry

func findCmd(name string) *replCmd {
	for i := range registry {
		c := &registry[i]
		if c.name == name {
			return c
		}
		for _, a := range c.aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

func isBuiltinCommand(name string) bool { return findCmd(name) != nil }

// menu feeds the line editor's "/" completion from the registry.
func (r *Repl) menu() []cmdInfo {
	var out []cmdInfo
	for _, c := range registry {
		if c.show == nil || c.show(r) {
			out = append(out, cmdInfo{c.name, c.desc})
		}
	}
	for _, c := range sortedCommands(r.orch.commands) {
		if !isBuiltinCommand("/" + c.Name) {
			out = append(out, cmdInfo{"/" + c.Name, firstNonEmpty(c.Description, "custom command")})
		}
	}
	return out
}

// Loop reads and dispatches input until /exit or EOF.
func (r *Repl) Loop() {
	r.sess.TTY = true
	replCommands = r.menu()
	r.ed = NewLineEditor(r.in)
	r.ed.models = func() []string {
		if r.teamMode() {
			var ms []string
			for _, a := range r.orch.roles.Roles {
				ms = append(ms, a.Models...)
			}
			return ms
		}
		return append(r.local.KnownModels(), r.orch.providers.Refs()...)
	}
	r.ed.files = func(frag string) []string { return jailFiles(r.orch.jl, frag) }
	r.ed.status = r.statusLine

	for {
		fmt.Print("\n")
		prefill := r.prefill
		r.prefill = ""
		line, err := r.ed.ReadLine(" "+cFaint+"›"+cReset+" ", prefill)
		if err == errLineCancel {
			continue
		}
		if err != nil { // EOF (Ctrl-D / stream end)
			return
		}
		// A line ending in \ continues on the next, for long prompts.
		for strings.HasSuffix(line, "\\") {
			cont, err := r.ed.ReadLine("   "+cFaint+"…"+cReset+" ", "")
			if err != nil {
				break
			}
			line = line[:len(line)-1] + "\n" + cont
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") && !strings.Contains(strings.Fields(line)[0][1:], "/") {
			name, arg, _ := strings.Cut(line, " ")
			if c := findCmd(name); c != nil {
				if c.run == nil { // /exit
					r.exitNotice()
					return
				}
				if c.run(r, strings.TrimSpace(arg)) {
					r.runTurn()
				}
				continue
			}
			if handled, content := handleCustomCommand(line, r.sess, r.orch.rec); handled {
				if content != "" {
					r.runTurn()
				}
				continue
			}
			errLine("unknown command %s", name)
			hint("/help lists the commands")
			continue
		}
		r.submit(line)
	}
}

// submit sends a typed message, attaching @file mentions.
func (r *Repl) submit(line string) {
	content := line
	blocks, names := expandMentions(r.orch.jl, line)
	if len(names) > 0 {
		content += blocks
		for _, n := range names {
			r.orch.noteRead(r.orch.jl, n)
		}
		fmt.Println(" " + faint("%s attached @%s", gNone, strings.Join(names, " @")))
	}
	r.sess.Msgs = append(r.sess.Msgs, Message{Role: "user", Content: content})
	r.orch.rec.Event("user", map[string]any{"text": line, "attached": names})
	r.runTurn()
}

func (r *Repl) runTurn() {
	// Keep reading the keyboard while the agent works: a paste must not be cut
	// by the terminal's line buffer, and type-ahead becomes the next message.
	r.in.StartCapture()
	defer r.in.StopCapture()
	r.sess.Run(context.Background())
	r.sess.saveTranscript()
}

// statusLine is shown under the input: role · model · approvals · dir.
func (r *Repl) statusLine() string {
	s := r.sess
	parts := []string{s.agent.Name, s.client.Model(), approvalShort(r.orch.ap)}
	if s.Loop {
		parts = append(parts, "loop")
	}
	if s.jail().Unsafe {
		parts = append(parts, cBlood+"unsafe"+cFaint)
	}
	if n := s.BackgroundRunning(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d running in background", n))
	}
	if rem := s.remote(); rem != nil {
		parts = append(parts, s.memberName()+" · "+rem.Host+":"+path.Base(rem.Dir))
	} else {
		parts = append(parts, shortDir(s.jail().Root))
	}
	return strings.Join(parts, " · ")
}

func approvalShort(ap *Approver) string {
	switch ap.ModeShort() {
	case "ask":
		return "asks first"
	case "auto":
		return "auto-approve"
	default:
		return "auto: " + strings.TrimPrefix(ap.ModeShort(), "auto:")
	}
}

// approvalPhrase describes the approval posture in a sentence.
func approvalPhrase(ap *Approver) string {
	cs := ap.TrustedClasses()
	switch {
	case ap.Trusts("*"):
		return "auto-approves everything (sandbox and deny rules still apply)"
	case len(cs) == 0:
		return "asks before edits, commands and web fetches"
	default:
		return "auto-approves " + strings.Join(cs, ", ") + "; asks for the rest"
	}
}

// ── banner ──────────────────────────────────────────────────────────────────

func (r *Repl) Banner() {
	o, s := r.orch, r.sess
	fmt.Println()
	printBehelit()
	fmt.Println()

	root := o.jl.Root
	proj := cBold + filepath.Base(root) + cReset + "  " + faint("%s", ellipsizeMiddle(shortDir(filepath.Dir(root))+"/", 48))
	var facts []string
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		facts = append(facts, "git")
	}
	if name, _, ok := loadProjectInstructions(o.jl); ok {
		facts = append(facts, name)
	}
	if len(facts) > 0 {
		proj += faint(" · %s", strings.Join(facts, " · "))
	}
	if rem := s.remote(); rem != nil {
		proj = cBold + path.Base(rem.Dir) + cReset + "  " + faint("%s · %s on %s · over ssh", s.memberName(), ellipsizeMiddle(rem.Dir, 40), rem.Host)
	}
	row("project", proj)

	chain := s.client.Model()
	if len(s.models) > 1 {
		chain = strings.Join(s.models[s.modelIdx:], faint(" → "))
	}
	roleLine := cBold + s.agent.Name + cReset + "  " + chain
	if s.agent.Thinking != "" {
		roleLine += faint(" · effort %s", s.agent.Thinking)
	}
	row("role", roleLine)

	if r.teamMode() {
		var team []string
		for _, a := range o.roleList(s.agent.Name) {
			team = append(team, a.Name)
		}
		if len(team) > 0 {
			row("team", strings.Join(team, faint(" · ")))
		}
		gw := hostOf(r.local.Endpoint())
		switch {
		case o.gatewayModels > 0:
			gw += "  " + statusText(cGreen, gUp, "up") + faint(" · %d models", o.gatewayModels)
		case o.gatewayModels == 0:
			gw += "  " + statusText(cRed, gDown, "unreachable") + faint(" · lca doctor")
		}
		gw += faint(" · %s tools", transportName(s.client))
		row("gateway", gw)
	} else {
		subs := []string{}
		for _, a := range o.subagentsFor(s) {
			subs = append(subs, a.Name)
		}
		if len(subs) > 0 {
			row("helpers", strings.Join(subs, faint(" · ")))
		}
		row("endpoint", hostOf(s.client.Endpoint())+faint(" · %s tools", transportName(s.client)))
		for _, n := range r.notes {
			contValue(n)
		}
	}

	mode := approvalPhrase(o.ap)
	if s.Loop {
		mode += faint(" · loop")
	}
	row("mode", mode)
	if s.jail().Unsafe {
		row("", cBlood+"⚠ unsafe: sandbox off — any path, any command"+cReset)
	}
	var cfgs []string
	if o.roles != nil {
		for _, p := range o.roles.Sources {
			cfgs = append(cfgs, prettyPath(p, root))
		}
	}
	for _, p := range o.fc.Sources {
		cfgs = append(cfgs, prettyPath(p, root))
	}
	if len(cfgs) > 0 {
		row("config", faint("%s", strings.Join(cfgs, ", ")))
	}
	for _, w := range o.warnings {
		fmt.Println("  " + cYellow + gPartial + cReset + " " + faint("%s", w))
	}
	fmt.Println()
	if !r.teamMode() && o.gatewayModels < 0 {
		fmt.Println(" " + faint("Type a task to start. /help for commands · @file attaches a file · Ctrl-C interrupts"))
		fmt.Println(" " + faint("/setup picks the models and gives them roles — or lca init from the shell"))
		return
	}
	fmt.Println(" " + faint("Type a task to start. /help for commands · @file attaches a file · Ctrl-C interrupts"))
}

// ── session ─────────────────────────────────────────────────────────────────

func (r *Repl) cmdHelp(arg string) bool {
	all := arg == "all"
	section("commands")
	type line struct{ syn, desc string }
	groups := map[string][]line{}
	width := 0
	for _, g := range cmdGroups {
		var rows [][]string
		if g == "custom" {
			for _, c := range sortedCommands(r.orch.commands) {
				desc := firstNonEmpty(c.Description, "custom command")
				if c.Agent != "" {
					desc += faint(" → %s", c.Agent)
				}
				rows = append(rows, []string{"/" + c.Name + faint(" [args]"), faint("%s", desc)})
			}
		} else {
			for _, c := range registry {
				if c.group != g || (!all && c.show != nil && !c.show(r)) {
					continue
				}
				syn := c.name
				if c.args != "" {
					syn += " " + faint("%s", c.args)
				}
				rows = append(rows, []string{syn, faint("%s", c.desc)})
			}
		}
		for _, r := range rows {
			groups[g] = append(groups[g], line{r[0], r[1]})
			width = max(width, visibleWidth(r[0]))
		}
	}
	// The column is capped: it is padded to the widest synopsis, and one long one
	// used to wrap every description on the screen. Anything over the cap keeps its
	// args on a faint continuation line of its own instead of taxing the other
	// twenty-four commands.
	width = min(width, 30)
	for _, g := range cmdGroups {
		if len(groups[g]) == 0 {
			continue
		}
		fmt.Printf("  %s%s%s\n", cDim, g, cReset)
		for _, l := range groups[g] {
			if visibleWidth(l.syn) > width {
				name, args, _ := strings.Cut(stripANSI(l.syn), " ")
				fmt.Printf("    %s  %s\n", padTo(name, width, 0), l.desc)
				fmt.Printf("    %s  %s\n", padTo("", width, 0), faint("%s", args))
				continue
			}
			fmt.Printf("    %s  %s\n", padTo(l.syn, width, 0), l.desc)
		}
	}
	fmt.Println()
	fmt.Println("  " + faint("keys   Enter send · \\ at line end continues · ↑/↓ history · Tab completes · Ctrl-C interrupts · Ctrl-D quits"))
	fmt.Println("  " + faint("input  @path attaches a file · /name runs a command"))
	if !all {
		hint("/help all also lists commands hidden in this setup")
	}
	return false
}

func (r *Repl) cmdResume(arg string) bool {
	sessions := listSessions(filepath.Join(r.cfg.stateDir(), "transcripts"), r.orch.rec.SessionPath())
	if len(sessions) == 0 {
		fmt.Println("  " + faint("no previous sessions yet"))
		return false
	}
	if arg == "list" || arg == "ls" {
		section("previous sessions")
		var rows [][]string
		for i, s := range sessions {
			if i == 12 {
				break
			}
			rows = append(rows, []string{faint("%d", i+1), sessionWhen(s.id), faint("%d turns", s.turns), s.preview})
		}
		table(nil, rows)
		hint("/resume <n> continues one (default: the most recent)")
		return false
	}
	idx := 0
	if arg != "" {
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 || n > len(sessions) {
			errLine("no session %q", arg)
			hint("/resume list shows them")
			return false
		}
		idx = n - 1
	}
	restored, err := resumeInto(&r.sess.Msgs, sessions[idx])
	if err != nil {
		errLine("resume failed: %v", err)
		return false
	}
	r.orch.rec.Event("resume", map[string]any{"from": sessions[idx].id, "messages": restored})
	r.sess.saveTranscript()
	okLine("resumed %s %s", sessionWhen(sessions[idx].id), faint("· %d messages", restored))
	return false
}

func (r *Repl) cmdCompact(string) bool {
	if err := r.sess.Compact(context.Background(), false); err != nil {
		errLine("compact: %v", err)
	}
	return false
}

func (r *Repl) cmdReset(string) bool {
	r.sess.Msgs = r.sess.Msgs[:1]
	r.sess.setTodosQuiet(nil)
	resetChanges()
	r.orch.rec.Event("reset", nil)
	okLine("fresh start — conversation and change log cleared")
	return false
}

// ── agents ──────────────────────────────────────────────────────────────────

func (r *Repl) cmdAgent(arg string) bool {
	if arg == "" {
		section("primary")
		var rows [][]string
		for _, a := range r.orch.primaryAgents() {
			mark := " "
			if a == r.sess.agent {
				mark = cGreen + gUp + cReset
			}
			rows = append(rows, []string{mark, a.Name, faint("%s", firstNonEmpty(a.Description, "—"))})
		}
		table(nil, rows)
		hint("/agent <name> switches · /agents shows the whole team")
		return false
	}
	prev := r.sess.agent.Name
	if err := r.sess.SetAgent(arg); err != nil {
		errLine("%v", err)
		hint("/agent lists the choices")
		return false
	}
	r.orch.rec.Event("agent_change", map[string]any{"from": prev, "to": arg})
	okLine("now %s %s", cBold+arg+cReset, faint("· %s", r.sess.client.Model()))
	return false
}

func (r *Repl) cmdAgents(string) bool {
	o := r.orch
	root := o.jl.Root
	if o.roles != nil {
		var ps []string
		for _, p := range o.roles.Sources {
			ps = append(ps, prettyPath(p, root))
		}
		what := strings.Join(ps, ", ")
		if t := o.activeTier(); t != "" {
			what += " · tier " + t
		}
		section("team", faint("%s", what))
		// The member column appears only on a fleet, so a single-machine team's
		// output is byte-identical to what it has always been.
		fleet := len(o.memberNames()) > 1
		var rows [][]string
		for _, a := range o.roles.Roles {
			name := a.Name
			if a == r.sess.agent {
				name = cBold + a.Name + cReset
			}
			tools := "all"
			if a.ToolsSet {
				tools = strconv.Itoa(len(a.Tools))
			}
			ctx := "—"
			if a.Context > 0 {
				ctx = kfmt(a.Context)
			}
			cols := []string{name, orDash(a.Tier), strings.Join(a.Models, faint(" → ")), firstNonEmpty(a.Thinking, "—"), ctx, tools, faint("%s", firstNonEmpty(a.CheckCmd, "—"))}
			if fleet {
				cols = append(cols, o.memberFor(a).MemberName())
			}
			rows = append(rows, cols)
		}
		head := []string{"role", "tier", "models", "effort", "context", "tools", "check"}
		if fleet {
			head = append(head, "member")
		}
		table(head, rows)
	}
	section("agents")
	var rows [][]string
	names := sortedKeys(o.agents)
	for _, n := range names {
		a := o.agents[n]
		if a.Hidden || a.IsRole {
			continue
		}
		kind := "helper"
		switch {
		case a.Mode == "primary":
			kind = "primary"
		case a.Mode == "all":
			kind = "both"
		}
		model := faint("inherits")
		if a.Model != "" {
			model = a.Model
		}
		rows = append(rows, []string{a.Name, faint("%s", kind), model, faint("%s", firstNonEmpty(a.Description, "—"))})
	}
	table([]string{"agent", "kind", "model", "what it does"}, rows)
	if o.roles != nil {
		hint("delegate(role, task) sends a change to a role · task(agent, …) asks a helper")
	}
	hint("add agents in .lca/agents/<name>.md · roles in .lca/roles.yaml")
	return false
}

func (r *Repl) cmdTasks(arg string) bool {
	o := r.orch
	hist := o.History()
	if arg != "" {
		for _, t := range o.Tasks() {
			if t.ID != arg {
				continue
			}
			if t.running.Load() {
				fmt.Println("  " + faint("%s is still running — its report appears when it finishes", t.ID))
				return false
			}
			section(t.ID, t.agent.Name)
			row("task", t.title)
			row("model", t.client.Model())
			for i := len(t.Msgs) - 1; i > 0; i-- {
				if t.Msgs[i].Role == "assistant" {
					if txt := finalText(t.Msgs[i]); txt != "" {
						fmt.Println()
						for _, l := range strings.Split(txt, "\n") {
							fmt.Println("  " + renderMarkdownLine(l))
						}
						break
					}
				}
			}
			return false
		}
		for _, h := range hist {
			if h.ID == arg {
				section(h.ID, h.Agent)
				row("task", h.Title)
				row("status", statusWord(h.Status))
				if h.Detail != "" {
					fmt.Println()
					for _, l := range strings.Split(h.Detail, "\n") {
						fmt.Println("  " + faint("%s", l))
					}
				}
				return false
			}
		}
		errLine("no task %q", arg)
		return false
	}
	if len(hist) == 0 {
		fmt.Println("  " + faint("no subagent has run yet in this session"))
		return false
	}
	section("subagent runs")
	var rows [][]string
	for _, h := range hist {
		dur := "…"
		if h.Duration > 0 {
			dur = fmtDurShort(h.Duration)
		}
		rows = append(rows, []string{faint("%s", h.ID), h.Kind, h.Agent, statusWord(h.Status), faint("%s", dur), firstLine(h.Title)})
	}
	table([]string{"id", "kind", "agent", "status", "time", "task"}, rows)
	hint("/tasks <id> shows a run's result")
	return false
}

// statusWord colors a task status.
func statusWord(s string) string {
	switch s {
	case "passed", "completed", "approve":
		return cGreen + gUp + " " + s + cReset
	case "running":
		return cYellow + gPartial + " " + s + cReset
	// unreviewed is deliberately non-blocking, so it must not read as a failure.
	case "unverified", "not_applied", "unreviewed":
		return cYellow + gPartial + " " + s + cReset
	}
	return cRed + gDown + " " + s + cReset
}

func (r *Repl) cmdTodo(string) bool {
	if t := r.sess.Todos(); len(t) == 0 {
		fmt.Println("  " + faint("no todo list yet — agents keep one for multi-step work"))
	} else {
		printTodos(t)
	}
	return false
}

func (r *Repl) cmdSkills(string) bool {
	section("skills")
	var rows [][]string
	for _, n := range sortedKeys(r.orch.skills) {
		sk := r.orch.skills[n]
		rows = append(rows, []string{n, faint("%s", firstNonEmpty(sk.Description, "—"))})
	}
	table(nil, rows)
	return false
}

// ── turn ────────────────────────────────────────────────────────────────────

func (r *Repl) cmdRetry(string) bool {
	idx := lastUserTurn(r.sess.Msgs)
	if idx < 0 {
		fmt.Println("  " + faint("nothing to retry yet"))
		return false
	}
	r.sess.Msgs = r.sess.Msgs[:idx+1]
	r.orch.rec.Event("retry", nil)
	return true
}

func (r *Repl) cmdEdit(string) bool {
	idx := lastUserTurn(r.sess.Msgs)
	if idx < 0 {
		fmt.Println("  " + faint("nothing to edit yet"))
		return false
	}
	orig := r.sess.Msgs[idx].Content
	if i := strings.Index(orig, "\n<file "); i >= 0 {
		orig = orig[:i] // shed @-attached file blocks; keep the typed text
	}
	r.prefill = orig
	r.sess.Msgs = r.sess.Msgs[:idx]
	return false
}

// ── review ──────────────────────────────────────────────────────────────────

func (r *Repl) cmdDiff(string) bool {
	diffs := sessionDiffs()
	if len(diffs) == 0 {
		fmt.Println("  " + faint("no file changes this session"))
		return false
	}
	section("changes this session")
	for _, l := range diffs {
		fmt.Println(l)
	}
	hint("/undo reverts the last one")
	return false
}

func (r *Repl) cmdUndo(string) bool {
	msg, ok := undoLast()
	if !ok {
		fmt.Println("  " + faint("nothing to undo"))
		return false
	}
	r.orch.rec.Event("undo", map[string]any{"result": msg})
	if strings.HasPrefix(msg, "error") {
		errLine("%s", msg)
	} else {
		okLine("%s", msg)
	}
	return false
}

func (r *Repl) cmdContext(string) bool {
	msgs, budget := r.sess.Msgs, r.sess.budget()
	tok := estimateTokens(msgs)
	section("context")
	pct := 0
	if budget > 0 {
		pct = tok * 100 / budget
	}
	row("used", fmt.Sprintf("~%s of ~%s tokens %s", kfmt(tok), kfmt(budget), faint("(%d%%, %d messages)", pct, len(msgs))))
	if budget <= 0 || tok <= budget {
		row("cache", statusText(cGreen, gUp, "aligned")+faint(" — the prefix is sent unchanged, so the server's KV cache keeps hitting"))
	} else {
		row("cache", statusText(cYellow, gPartial, "compressing")+faint(" — over budget; old tool output is trimmed"))
	}
	if u := r.sess.lastUsage; u.PromptTokens > 0 {
		row("last", fmt.Sprintf("%s in · %d%% cached", kfmt(u.PromptTokens), u.CacheHitPct()))
	}
	type item struct {
		label string
		bytes int
	}
	var items []item
	for _, m := range msgs {
		if name, path, ok := toolResultKey(m); ok && !isStub(m.Content) {
			items = append(items, item{strings.TrimSpace(toolVerb(name) + " " + path), len(m.Content)})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].bytes > items[j].bytes })
	if len(items) > 0 {
		fmt.Println()
		fmt.Println("  " + faint("largest tool outputs"))
		var rows [][]string
		for i, it := range items {
			if i == 5 {
				break
			}
			rows = append(rows, []string{faint("%s", byteCount(it.bytes)), it.label})
		}
		table(nil, rows)
	}
	hint("/compact summarizes the conversation to free space")
	return false
}

// ── modes ───────────────────────────────────────────────────────────────────

func (r *Repl) cmdApprove(arg string) bool {
	ap := r.orch.ap
	switch arg {
	case "on", "all":
		ap.TrustAll()
	case "off":
		ap.Clear()
	case "run":
		ap.Trust("run")
	case "edit", "write":
		ap.Trust("edit")
	case "", "status":
		row("approve", approvalPhrase(ap))
		hint("/approve on · off · run · edit")
		return false
	default:
		errLine("usage: /approve [on|off|run|edit]")
		return false
	}
	r.orch.rec.Event("approve_mode", map[string]any{"trusted": ap.TrustedClasses()})
	okLine("%s", approvalPhrase(ap))
	// "on" is the command's spelling; "all" is the setting's, and the file has to
	// hold a value /set would accept.
	r.cfg.Approve = map[string]string{"on": "all", "write": "edit"}[arg]
	r.cfg.Approve = firstNonEmpty(r.cfg.Approve, arg)
	r.noteChange("approve", "/approve")
	return false
}

func toggle(arg string, cur bool) (bool, bool) {
	switch arg {
	case "on", "show", "expand":
		return true, true
	case "off", "hide", "collapse":
		return false, true
	case "", "toggle":
		return !cur, true
	}
	return cur, false
}

func (r *Repl) cmdThink(arg string) bool {
	if arg == "last" {
		if strings.TrimSpace(r.sess.LastReason) == "" {
			fmt.Println("  " + faint("no reasoning captured for the last answer"))
			return false
		}
		section("reasoning", faint("last answer"))
		for _, ln := range strings.Split(r.sess.LastReason, "\n") {
			fmt.Println("  " + faint("%s", ln))
		}
		return false
	}
	v, ok := toggle(arg, r.sess.ShowThink)
	if !ok {
		errLine("usage: /think [on|off|last]")
		return false
	}
	r.sess.ShowThink = v
	r.orch.rec.Event("think_mode", map[string]any{"show": v})
	if v {
		okLine("reasoning is shown in full")
	} else {
		okLine("reasoning collapses to a one-line status")
	}
	r.cfg.ShowThinking = v
	r.noteChange("show_thinking", "/think")
	return false
}

func (r *Repl) cmdLoop(arg string) bool {
	v, ok := toggle(arg, r.sess.Loop)
	if !ok {
		errLine("usage: /loop [on|off]")
		return false
	}
	r.sess.Loop = v
	r.orch.rec.Event("loop_mode", map[string]any{"on": v})
	if v {
		okLine("loop on — the agent keeps going until it reports the task done")
	} else {
		okLine("loop off")
	}
	r.cfg.Loop = v
	r.noteChange("loop", "/loop")
	return false
}

func (r *Repl) cmdUnsafe(arg string) bool {
	// The team's jail, not this session's view of it: a member that scopes its
	// own allowlist gets a COPY of o.jl rebuilt per call, so lifting the sandbox
	// on the copy would lift it for nobody. The operator means the whole fleet.
	jail := r.orch.jl
	v, ok := toggle(arg, jail.Unsafe)
	if !ok {
		errLine("usage: /unsafe [on|off]")
		return false
	}
	jail.Unsafe = v
	r.sess.RefreshSystem() // the environment section describes the sandbox
	r.orch.rec.Event("unsafe_mode", map[string]any{"on": v})
	if v {
		fmt.Println("  " + cBlood + "⚠ unsafe on" + cReset + faint(" — sandbox off: any path, any command through sh (GPU policy still holds)"))
	} else {
		okLine("sandbox back on")
	}
	return false
}

// ── model ───────────────────────────────────────────────────────────────────

func (r *Repl) cmdModel(arg string) bool {
	s, local := r.sess, r.local
	if arg == "" {
		section("model")
		chain := s.client.Model()
		if len(s.models) > 1 {
			chain = strings.Join(s.models, faint(" → "))
		}
		row("current", chain)
		row("via", hostOf(s.client.Endpoint())+faint(" · %s tools", transportName(s.client)))
		if s.agent.Tier != "" {
			// The declared tier and the one actually in force, because a -tier
			// run answers "which models am I on?" differently from the file.
			tier := s.agent.Tier
			if act := r.orch.activeTier(); act != "" && act != s.agent.Tier {
				tier += faint(" (running %s)", act)
			}
			row("tier", tier)
		}
		// Provenance, not anonymous integers: a window from the running deployment
		// and one guessed from a sibling version look identical until they are
		// labelled, and only one of them is worth trusting.
		p := s.client.Profile()
		if p.Family != "" {
			row("profile", faint("%s · matched %q", p.Family, p.Key))
		} else {
			row("profile", faint("no profile for %q (normalised %q) — nothing is overridden", s.client.Model(), normalizeModelID(s.client.Model())))
		}
		row("context", faint("%s · budget %s", srcNum(s.client.CtxLen(), s.client.CtxSrc()), kfmt(s.budget())))
		// What this client will actually put in max_tokens, with the reason: the
		// window clamp can rewrite a configured budget, and a number that is
		// rewritten has to name its source like every other number here.
		if n, why := s.client.replyCeiling(0); n > 0 {
			row("output", faint("max %s · this client sends %s — %s", srcNum(p.Output, p.Src.Output), kfmt(n), why))
		} else {
			row("output", faint("max %s · %s", srcNum(p.Output, p.Src.Output), why))
		}
		temp, topP, effort := s.sampling()
		row("sampling", faint("temperature %s · top_p %s · top_k %s · effort %s",
			temp, topP, srcNum(p.TopK, p.Src.TopK), effort))
		if p.Note != "" {
			row("caveat", faint("%s", p.Note))
		}
		replay := p.Replay
		switch replay {
		case "all":
			row("reasoning", faint("replayed to the model on every step (interleaved thinking)"))
		case "turn":
			row("reasoning", faint("replayed within the current turn (tool-call chain)"))
		default:
			row("reasoning", faint("not replayed — this family keeps its plan in the answer text"))
		}
		if r.teamMode() {
			hint("models come from the role's chain in roles.yaml; /model <name> overrides it for this session")
		} else if refs := r.orch.providers.Refs(); len(refs) > 0 {
			hint("hosted: %s", strings.Join(refs, ", "))
		} else {
			hint("/model <name> sets the endpoint's model · /model <provider>/<model> uses a hosted API (/providers)")
		}
		if r.cfg.Discover {
			if models, err := local.ListModels(); err == nil {
				for _, l := range modelTable(models, local.Model()) {
					fmt.Println(l)
				}
			}
		}
		// The report survives and the menu comes AFTER it: it is the most useful
		// screen in the program, and replacing it with a picker would lose the
		// window, sampling and replay provenance above. Off a terminal the report
		// and its hint are the whole answer, byte for byte.
		if !r.in.IsTTY() {
			return false
		}
		served, err := local.ListModels()
		if err != nil || len(served) == 0 {
			return false
		}
		var cs []choice
		for _, m := range served {
			cs = append(cs, modelChoice(m, m.ID == s.client.Model()))
		}
		i, perr := pickOne(r.in, cs, pickOpts{title: "model", detail: faint("%d served at %s · enter keeps the current one", len(cs), hostOf(local.Endpoint()))})
		if perr != nil || i < 0 || cs[i].id == s.client.Model() {
			return false
		}
		// Straight into the branch that already exists: no second implementation,
		// so the typed and the picked forms cannot drift.
		return r.cmdModel(cs[i].id)
	}

	prev := s.client.Model()
	if _, _, hosted := r.orch.providers.Split(arg); hosted {
		if err := s.SetModel(arg); err != nil {
			errLine("%v", err)
			return false
		}
	} else {
		local.SetModel(arg)
		s.client, s.models = local, nil
		if ep := local.EndpointForModel(arg); ep != "" && ep != local.Endpoint() {
			local.SetEndpoint(ep)
			r.orch.rec.Event("endpoint_change", map[string]any{"to": ep, "via": "model"})
		}
		// SetModel/SetEndpoint just forgot the previous model's max_model_len, so
		// ask this one's endpoint for the new one — unconditionally, not only
		// under -discover: without it the window falls back to the table and the
		// budget silently changes under the operator.
		relearnCtxLen(local)
	}
	s.RefreshSystem()
	r.orch.rec.Event("model_change", map[string]any{"from": prev, "to": arg})
	okLine("model %s %s", faint("%s →", prev), arg)
	r.cfg.Model = arg
	r.noteChange("model", "/model")
	return false
}

func (r *Repl) cmdProviders(string) bool {
	section("providers")
	var rows [][]string
	rows = append(rows, []string{cGreen + gUp + cReset, "local", faint("%s", r.local.provider.Transport), hostOf(r.local.Endpoint()), faint("LCA_BASE_URL")})
	for _, p := range r.orch.providers.Sorted() {
		glyph, key := cGreen+gUp+cReset, faint("key set")
		if p.apiKey() == "" {
			glyph, key = faint("%s", gNone), faint("set %s", strings.Join(p.KeyEnv, " or "))
		}
		rows = append(rows, []string{glyph, p.ID, faint("%s", p.Transport), hostOf(p.BaseURL), key})
	}
	table([]string{"", "provider", "tools", "host", "key"}, rows)
	hint("/model <provider>/<model> switches to one")
	return false
}

func (r *Repl) cmdEndpoint(arg string) bool {
	s, client := r.sess, r.local
	eps := client.Endpoints()
	if arg == "" {
		section("endpoints")
		probes := probeEndpoints(client, eps)
		var rows [][]string
		for i, e := range eps {
			mark := " "
			if e == client.Endpoint() {
				mark = cBold + "→" + cReset
			}
			rows = append(rows, []string{mark, faint("%d", i+1), probes[i].glyph(), e, probes[i].detail(e == client.Endpoint(), client.Model())})
		}
		table(nil, rows)
		hint("/endpoint <n|url> switches")
		// A one-row picker under a one-row table cannot change anything, so it is
		// two screens of chrome asking a question with one answer.
		if len(eps) < 2 {
			hint("/set endpoints <url>,<url> adds more · /set endpoint <url> moves this one")
			return false
		}
		if !r.in.IsTTY() {
			return false
		}
		var cs []choice
		for i, e := range eps {
			// A down endpoint is selectable on purpose: switching to a gateway that
			// is starting is a thing people do.
			cs = append(cs, choice{id: e, label: e, detail: probes[i].glyph() + " " + probes[i].detail(e == client.Endpoint(), client.Model()), on: e == client.Endpoint()})
		}
		i, perr := pickOne(r.in, cs, pickOpts{title: "endpoint", detail: faint("enter keeps the current one")})
		if perr != nil || i < 0 || cs[i].id == client.Endpoint() {
			return false
		}
		return r.cmdEndpoint(cs[i].id)
	}
	target := arg
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(eps) {
			errLine("no endpoint #%d (have %d)", n, len(eps))
			return false
		}
		target = eps[n-1]
	}
	prev := client.Endpoint()
	client.SetEndpoint(target)
	if s.client != client {
		s.client, s.models = client, nil
	}
	s.RefreshSystem()
	r.orch.rec.Event("endpoint_change", map[string]any{"from": prev, "to": client.Endpoint()})
	okLine("endpoint %s", client.Endpoint())
	r.cfg.BaseURL = client.Endpoint()
	r.noteChange("endpoint", "/endpoint")
	if m := client.EndpointModel(client.Endpoint()); m != "" && m != client.Model() {
		client.SetModel(m)
		row("model", m)
	}
	if r.cfg.Discover {
		for _, n := range reconcileModel(r.orch.providers, client, r.orch.rec) {
			fmt.Println("  " + n)
		}
	} else {
		// The new endpoint may serve the same model id with a different
		// --max-model-len; the old window was dropped with the old baseURL.
		relearnCtxLen(client)
	}
	return false
}

// relearnCtxLen asks the current endpoint what window it serves the current model
// at, and is silent when it cannot say. The table is the fallback, never the
// override: the running deployment is the truth about the running deployment.
func relearnCtxLen(c *Client) {
	models, err := c.ListModels()
	if err != nil {
		return
	}
	if info, ok := findModel(models, c.Model()); ok {
		c.SetCtxLen(info.MaxLen)
	}
}

func (r *Repl) cmdDiscover(string) bool {
	cfg, client := r.cfg, r.local
	fmt.Println(" " + faint("%s looking for models: squeue → scontrol → logs → probe…", gNone))
	res, err := discoverSlurm(cfg.Reservation, cfg.DiscoverUser, cfg.Scheme)
	if err != nil {
		errLine("%v", err)
		r.orch.rec.Event("discover", map[string]any{"error": err.Error()})
		return false
	}
	var urls []string
	for _, m := range res.Models {
		if m.Endpoint() == "" {
			continue
		}
		u := m.baseURL(cfg.Scheme)
		urls = append(urls, u)
		client.SetEndpointModel(u, m.modelName())
	}
	client.SetEndpoints(urls)
	r.orch.rec.Event("discover", map[string]any{"count": len(res.Models), "usable": len(urls), "reservation": cfg.Reservation})

	section("discovered")
	for _, w := range res.Warnings {
		warnLine("%s", w)
	}
	var rows [][]string
	idx := 0
	for _, m := range res.Models {
		if m.Port == 0 {
			continue
		}
		idx++
		detail := m.display()
		if m.Engine != "" {
			detail += faint(" · %s", m.Engine)
		}
		if m.MaxModelLen > 0 {
			detail += faint(" · ctx %s", kfmt(m.MaxModelLen))
		}
		if m.GpuCount > 0 {
			detail += faint(" · %d gpu", m.GpuCount)
		}
		rows = append(rows, []string{healthGlyph(m.Health), faint("%d", idx), fmt.Sprintf("%s:%d", m.Node, m.Port), detail})
	}
	if idx == 0 {
		fmt.Println("  " + faint("no models found"))
		return false
	}
	table(nil, rows)
	hint("/endpoint <n> switches to one (its model comes along)")
	return false
}

// ── misc ────────────────────────────────────────────────────────────────────

func whoAmI() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "?"
}

// interruptible runs fn with a context Ctrl-C cancels.
func interruptible(s *Session, fn func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var flag atomic.Bool
	stop := s.watchInterrupt(ctx, cancel, &flag)
	defer stop()
	fn(ctx)
}

var _ = time.Second
