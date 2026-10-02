package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Agents (ported from opencode's agent/). An agent is a named configuration of
// the same loop: its own prompt, model, sampling, step budget and permission
// rules. Primary agents talk to the user (build, plan); subagents are started
// by the task tool (explore, general, or your own) and run in child sessions —
// in parallel, each on its own model if configured. That is the unit of
// orchestration: a workflow is agents delegating to agents.
//
// Sources, later overriding earlier (same name = merge, set fields win):
//
//	built-ins (below)
//	~/.lca/agents/*.md, ~/.config/opencode/agent(s)/*.md, ~/.claude/agents/*.md
//	<root>/.opencode/agent(s)/*.md, <root>/.claude/agents/*.md, <root>/.lca/agents/*.md
//	"agents" in config.json
//
// Markdown agents: YAML frontmatter + the body as the system prompt, e.g.
//
//	---
//	description: Reviews a diff for bugs. Use after non-trivial edits.
//	mode: subagent
//	model: deepseek/deepseek-v4-pro
//	thinking: high
//	steps: 30
//	permission:
//	  edit: deny
//	  run:
//	    "*": deny
//	    "git diff *": allow
//	---
//	You are a meticulous code reviewer…

type Agent struct {
	Name        string
	Description string
	Mode        string // primary | subagent | all
	Model       string // provider/model; "" = inherit the caller's
	Prompt      string // replaces the base role prompt; "" = default
	Temperature *float64
	TopP        *float64
	Thinking    string
	Steps       int // max model calls per run; 0 = config default
	Hidden      bool
	BaseRules   Ruleset // built-in agent rules: the user's global permission overrides them
	Rules       Ruleset // rules from the agent's own definition: override the global permission
	Source      string
	disabled    bool

	// roles.yaml (roles.go)
	IsRole   bool
	Models   []string // gateway model names, in fallback order
	Tools    []string // explicit tool set (ToolsSet: even when empty)
	ToolsSet bool
	Context  int    // context limit in tokens
	CheckCmd string // default verifier command for tasks given to this role
	// CheckTimeout is roles.yaml `check_timeout:` on the ROLE — seconds, 0 = the
	// team's defaults:. It exists because one role's check is a stand (build,
	// deploy, check.sh: five to fifteen minutes) while the rest run `go test` in
	// seconds, and raising the team default to cover the first gives the others an
	// hour each to hang in. Capped at an hour by the loader.
	CheckTimeout int
	Review       string // roles.yaml `review:` — the role that reviews this role's diffs
	Fork         bool   // roles.yaml `fork:` — delegations to this role start from the caller's reads
	Tier         string // roles.yaml `tier:` — the chain this role names instead of models:
	Member       string // roles.yaml `member:` — the machine this role's sessions work on
}

func (a *Agent) isPrimary() bool  { return a.Mode == "primary" || a.Mode == "all" || a.Mode == "" }
func (a *Agent) isSubagent() bool { return a.Mode == "subagent" || a.Mode == "all" }

func builtinAgents() []*Agent {
	return []*Agent{
		{Name: "build", Mode: "primary", Source: "built-in",
			Description: "Default agent: reads, edits and runs, delegating research to subagents."},
		{Name: "plan", Mode: "primary", Source: "built-in", Prompt: planPrompt,
			Description: "Read-only planning: investigates and writes a plan to .lca/plans/, no code edits.",
			BaseRules: Ruleset{
				{"edit", "*", Deny},
				{"edit", ".lca/plans/*", Ask},
				{"run", "*", Ask},
			}},
		{Name: "explore", Mode: "subagent", Source: "built-in", Prompt: explorePrompt, Steps: 40,
			Description: `Fast read-only codebase explorer. Use it to find files by pattern, search code for keywords, or answer questions about how the codebase works. Say how thorough it should be: "quick", "medium" or "very thorough".`,
			BaseRules: Ruleset{
				{"*", "*", Deny},
				{"read", "*", Allow},
				{"read", "*.env", Ask},
				{"read", "*.env.*", Ask},
				{"skill", "*", Allow},
			}},
		{Name: "general", Mode: "subagent", Source: "built-in", Prompt: generalPrompt, Steps: 60,
			Description: "General-purpose agent for researching complex questions and executing multi-step tasks, including edits. Use it to run several independent units of work in parallel.",
			BaseRules: Ruleset{
				{"todo", "*", Deny},
			}},
	}
}

const explorePrompt = `You are a file search specialist working inside a codebase. You navigate and
explore it quickly and thoroughly to answer the question you were given.

- Use glob for file-name patterns, grep for content, read_file for specific
  files, list_dir to orient yourself.
- Match the requested thoroughness: "quick" — a couple of targeted searches;
  "medium" — follow the obvious leads; "very thorough" — check multiple
  locations and naming conventions.
- Issue independent searches together in one response when you can.
- You are read-only: never create or modify files.
- Your final message is returned to the agent that called you, not shown to
  the user. Make it self-contained: concrete findings, relative file paths with
  line numbers where useful, and short code excerpts only when they matter.`

const generalPrompt = `You are a general-purpose subagent. Another agent delegated one task to you;
complete it end to end with your tools, then reply with a concise report.

- Do what was asked — no more. If the task says research only, do not edit.
- Verify your work where possible (read back, run the tests you were told to).
- Your final message is returned to the calling agent, not shown to the user:
  state what you did, what you found, files changed, and anything left undone.`

const planPrompt = `You are in PLAN mode: a read-only phase. Investigate and produce a plan; do not
change the code.

- You may NOT edit or create files, except the plan file under .lca/plans/.
- Explore first: read the relevant code, and launch explore subagents in
  parallel for broad or independent questions.
- When the approach is clear, write the plan to .lca/plans/<short-name>.md:
  goal, the files to change and how, risks, and how to verify. Then summarize
  it for the user and ask them to switch to the build agent (/agent build) to
  execute it.`

// loadAgents builds the agent table from built-ins, markdown files and config.
// Returned warnings are non-fatal load problems to show the user.
func loadAgents(root, dir string, fc *FileConfig, ps *Providers) (map[string]*Agent, []string) {
	agents := map[string]*Agent{}
	for _, a := range builtinAgents() {
		agents[a.Name] = a
	}
	var warns []string
	home, _ := os.UserHomeDir()
	var dirs []string
	add := func(base string, names ...string) {
		for _, n := range names {
			dirs = append(dirs, filepath.Join(base, n))
		}
	}
	add(dir, "agents")
	if home != "" {
		add(filepath.Join(home, ".config", "opencode"), "agent", "agents")
		add(filepath.Join(home, ".claude"), "agents")
	}
	add(filepath.Join(root, ".opencode"), "agent", "agents")
	add(filepath.Join(root, ".claude"), "agents")
	add(filepath.Join(root, ".lca"), "agents")

	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.md"))
		sort.Strings(files)
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			meta, body, _ := splitFrontmatter(string(data))
			name := meta.str("name")
			if name == "" {
				name = strings.TrimSuffix(filepath.Base(f), ".md")
			}
			a := agentFromMeta(name, meta, body, f, ps, &warns)
			mergeAgent(agents, a)
		}
	}
	if fc != nil {
		for _, name := range sortedKeys(fc.Agents) {
			ac := fc.Agents[name]
			a := &Agent{Name: name, Description: ac.Description, Mode: ac.Mode, Prompt: ac.Prompt,
				Temperature: ac.Temperature, TopP: ac.TopP, Thinking: ac.Thinking, Steps: ac.Steps,
				Hidden: ac.Hidden, Rules: Ruleset(ac.Permission), Source: "config", disabled: ac.Disable}
			a.Model = checkModelRef(ac.Model, name, ps, &warns)
			mergeAgent(agents, a)
		}
	}
	for name, a := range agents {
		if a.disabled {
			delete(agents, name)
		}
	}
	return agents, warns
}

func mergeAgent(agents map[string]*Agent, a *Agent) {
	cur, ok := agents[a.Name]
	if !ok {
		if a.Mode == "" {
			a.Mode = "all"
		}
		agents[a.Name] = a
		return
	}
	if a.Description != "" {
		cur.Description = a.Description
	}
	if a.Mode != "" {
		cur.Mode = a.Mode
	}
	if a.Model != "" {
		cur.Model = a.Model
	}
	if a.Prompt != "" {
		cur.Prompt = a.Prompt
	}
	if a.Temperature != nil {
		cur.Temperature = a.Temperature
	}
	if a.TopP != nil {
		cur.TopP = a.TopP
	}
	if a.Thinking != "" {
		cur.Thinking = a.Thinking
	}
	if a.Steps > 0 {
		cur.Steps = a.Steps
	}
	if a.Hidden {
		cur.Hidden = true
	}
	cur.disabled = cur.disabled || a.disabled
	cur.Rules = append(cur.Rules, a.Rules...)
	cur.Source = a.Source
}

func agentFromMeta(name string, meta *yNode, body, source string, ps *Providers, warns *[]string) *Agent {
	a := &Agent{Name: name, Description: meta.str("description"), Mode: meta.str("mode"), Prompt: body,
		Thinking: meta.str("thinking"), Source: source}
	a.Model = checkModelRef(meta.str("model"), name, ps, warns)
	if v, err := strconv.ParseFloat(meta.str("temperature"), 64); err == nil {
		a.Temperature = &v
	}
	if v, err := strconv.ParseFloat(meta.str("top_p"), 64); err == nil {
		a.TopP = &v
	}
	for _, k := range []string{"steps", "maxSteps", "max_steps"} {
		if n, err := strconv.Atoi(meta.str(k)); err == nil && n > 0 {
			a.Steps = n
		}
	}
	a.Hidden = meta.str("hidden") == "true"
	a.disabled = meta.str("disable") == "true"
	// Claude Code subagents have no mode; they are subagents.
	if a.Mode == "" && strings.Contains(source, string(filepath.Separator)+".claude"+string(filepath.Separator)) {
		a.Mode = "subagent"
	}

	// tools: either a map {tool: bool} (opencode) or a list / comma string of
	// allowed tools (Claude Code), which denies everything else.
	if t := meta.child("tools"); t != nil {
		switch {
		case len(t.Children) > 0:
			for _, c := range t.Children {
				act := Deny
				if c.Value == "true" {
					act = Allow
				}
				a.Rules = append(a.Rules, Rule{toolPermKey(c.Key), "*", act})
			}
		default:
			items := t.List
			if len(items) == 0 && t.Value != "" {
				items = strings.Split(t.Value, ",")
			}
			// An explicit empty list is "no tools at all", the same as in
			// roles.yaml — a plain conversational agent. Splitting "[]" on commas
			// used to yield one tool literally named "[]", which denied nothing and
			// left the agent asking for approval to run commands.
			if v := strings.TrimSpace(t.Value); v == "[]" || (len(items) == 0 && v == "") {
				a.Rules = append(a.Rules, Rule{"*", "*", Deny})
				a.Tools, a.ToolsSet = nil, true
				break
			}
			if len(items) > 0 {
				a.Rules = append(a.Rules, Rule{"*", "*", Deny})
				for _, it := range items {
					if it = strings.TrimSpace(it); it != "" {
						a.Rules = append(a.Rules, Rule{toolPermKey(it), "*", Allow})
					}
				}
			}
		}
	}
	if p := meta.child("permission"); p != nil {
		if act, ok := validAction(p.Value); ok {
			a.Rules = append(a.Rules, Rule{"*", "*", act})
		}
		for _, c := range p.Children {
			key := toolPermKey(c.Key)
			if act, ok := validAction(c.Value); ok {
				a.Rules = append(a.Rules, Rule{key, "*", act})
				continue
			}
			for _, pc := range c.Children {
				if act, ok := validAction(pc.Value); ok {
					a.Rules = append(a.Rules, Rule{key, expandHome(pc.Key), act})
				} else {
					*warns = append(*warns, fmt.Sprintf("%s: permission %s.%s: bad action %q", source, c.Key, pc.Key, pc.Value))
				}
			}
		}
	}
	return a
}

// toolPermKey maps a tool or permission name from any ecosystem (ours,
// opencode, Claude Code) to our permission key.
func toolPermKey(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "*", "read", "edit", "run", "task", "skill", "todo", "web", "doom_loop":
		return n
	case "bash", "shell", "run_command":
		return "run"
	case "write", "multiedit", "patch", "apply_patch", "notebookedit":
		return "edit"
	case "list", "ls", "glob", "grep", "read_file", "list_dir":
		return "read"
	case "webfetch", "websearch", "fetch":
		return "web"
	case "todowrite", "todoread":
		return "todo"
	case "agent":
		return "task"
	}
	if t := resolveToolName(n); t != nil {
		return permissionOf(t.Name)
	}
	return n
}

// checkModelRef accepts a model only if it can be routed: a known
// "provider/model" ref, or an explicit "local/…". Anything else (e.g. a Claude
// alias like "sonnet") is ignored with a warning, and the agent inherits.
func checkModelRef(ref, agent string, ps *Providers, warns *[]string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "inherit" {
		return ""
	}
	if strings.HasPrefix(ref, "local/") {
		return ref
	}
	if ps != nil {
		if _, _, ok := ps.Split(ref); ok {
			return ref
		}
	}
	*warns = append(*warns, fmt.Sprintf("agent %s: model %q is not provider/model — inheriting the caller's model", agent, ref))
	return ""
}
