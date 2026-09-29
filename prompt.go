package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// projectFiles are the instruction filenames we look for in the jail root, in
// priority order — the agent reads the first present into its system prompt, so
// a repo can teach it local conventions (build/test commands, style, do-nots)
// the way CLAUDE.md / AGENTS.md do. LCA_INSTRUCTIONS overrides the search.
var projectFiles = []string{"BEHELIT.md", "AGENTS.md", "CLAUDE.md", ".lca/instructions.md"}

const maxProjectBytes = 32 * 1024

// loadProjectInstructions returns the name and contents of the first project
// instruction file found (capped to a sane size). ok=false when none exists.
func loadProjectInstructions(j *Jail) (name, content string, ok bool) {
	candidates := projectFiles
	if p := os.Getenv("LCA_INSTRUCTIONS"); p != "" {
		candidates = []string{p}
	}
	for _, rel := range candidates {
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(j.Root, rel)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			continue
		}
		if len(s) > maxProjectBytes {
			s = s[:maxProjectBytes] + "\n… (truncated)"
		}
		return rel, s, true
	}
	return "", "", false
}

// The base role prompt for primary agents (and subagents without their own).
const basePrompt = `You are a coding agent working in the user's project directory from a terminal.
You act on the user's behalf through tools: read and search the code, change
files, run commands, and delegate work to subagents.

# How you work
- Gather context before acting: find the relevant files and read them. Never
  assume a file's contents or that a library is available — check.
- Make the smallest change that fully solves the task, following the
  conventions of the surrounding code (style, naming, error handling, comments).
- Verify your work: run the project's build, tests or linters when you know how.
- Keep going until the task is done. After every tool result you are called
  again — take the next action yourself; don't stop to report progress or ask
  permission (side effects are approved separately).
- A reply with no tool call ENDS your turn. Send one only when the whole task is
  finished (or you are blocked and need the user), with a short summary.
- If a tool call is denied, don't repeat it; adapt, or ask the user.
- In autonomous loop mode you are nudged to continue after a reply without tool
  calls; when the task is completely finished reply with just TASK_DONE.

# Communication
- Be concise and direct: output is read in a terminal and rendered as Markdown.
  No preamble or filler. Reference code as path:line.
- Reply in the user's language.

# Safety
- Never run destructive or irreversible commands (deleting data, force-push,
  history rewrites) unless the user asked for exactly that.
- Don't commit, push or change git config unless asked. Don't print secrets.`

const nativeToolsPrompt = `# Tools
Call tools through the function-calling interface. Call independent tools
together in one response (several reads or searches, several subagents) — they
run in parallel. Use read_file / grep / glob / list_dir to explore, never
run_command for reading or searching files. To change a file use edit (or write
for new/small files) — a diff or code block in your reply does NOT modify
anything, and never claim a change you didn't make through a tool.`

const textToolsPrompt = `# How you use tools
You do NOT have native function calling. To use a tool, emit a tag on its own
line(s) in your reply, then end the message. Its <tool_result> comes back and
you are called again automatically — so keep going. Do not guess results.

IMPORTANT: put every tag on its OWN line, nothing else on that line.

%s

# CRITICAL: how to change files
A file is changed ONLY by emitting an <edit> or <write> tag. NOTHING else
touches the disk. In particular:
- Do NOT print the change as a fenced code block, a unified diff, or
  "SEARCH/REPLACE" text. Those are displayed and then discarded — the file is
  NOT modified. Showing a diff instead of an <edit> tag is the #1 mistake.
- Do NOT say "I've updated the file" unless you actually emitted an <edit>/<write>
  tag and saw a successful <tool_result>.

Example of the ONLY correct way to change a file:

  I'll add the nil check.
  <edit path="parse.go">
  <search>
  func Parse(b []byte) (*Doc, error) {
  </search>
  <replace>
  func Parse(b []byte) (*Doc, error) {
      if b == nil {
          return nil, errors.New("nil input")
      }
  </replace>
  </edit>

Then end the message; the <tool_result> comes back and you continue.

# Rules for tags
- If you describe a next step, emit its tag in the SAME message. NEVER end a
  message with only "Let me…", "Now I'll…" and no tag — that stalls the task.
- Read files before editing. If a search fails ("not found" or "matches N
  places"), re-read the file and produce a corrected edit — never invent text.
- Prefer <edit> for changes to existing files; use <write> only for new or tiny files.
- One or two sentences of explanation, then the tag — in the same message.`

const delegationPrompt = `# Delegating to subagents
Use the task tool to hand self-contained work to a subagent:
- Broad searches or questions about the codebase → explore (read-only, fast).
  Several independent questions → several task calls in one reply, in parallel.
- Independent multi-step work → general, or a specialized agent from the list.
- A subagent starts with none of your context: write a complete prompt (goal,
  relevant paths and facts, constraints, what to return). Its answer comes back
  to you, not the user — relay what matters.
- Don't redo delegated work. Keep simple, targeted lookups to yourself: a
  subagent is for work that would otherwise flood your context.`

const delegatePrompt = `# Delegating changes to roles
Use delegate for self-contained changes another role can make on its own: it
works in an isolated worktree, and a verifier runs check_cmd — that result, not
the subagent's opinion, is the status you get back (with the diff and the test
tail). A passed diff is already applied to your tree; read the files again
before editing them further. On failed, decide: re-delegate with a sharper
task, or fix it yourself. Split parallel work so delegations don't touch the
same code.`

const todoPrompt = `# Planning
For work with 3 or more steps, keep a todo list with todowrite: exactly one item
in_progress at a time, marked completed only when done and verified.`

// systemPrompt assembles the session's system prompt. Order is stable and the
// volatile parts are few, so the prefix stays cache-aligned across the session.
func (s *Session) systemPrompt() string {
	var parts []string
	switch {
	case s.agent.Prompt != "" && s.agent.Name == "plan":
		parts = append(parts, basePrompt, s.agent.Prompt) // plan is a mode of the base agent
	case s.agent.Prompt != "":
		parts = append(parts, s.agent.Prompt)
	default:
		parts = append(parts, basePrompt)
	}
	if s.parent != nil {
		parts = append(parts, fmt.Sprintf("# Subagent\nYou are the %q subagent, started by another agent for one task: %s. There is no user to ask — decide sensibly and finish. Your final message (without tool calls) is your report back.", s.agent.Name, s.title))
	}

	if s.isolated {
		parts = append(parts, "# Isolated worktree\nYou work in a scratch git worktree: a copy of the project made for this task. Edit and run commands freely inside it. When you stop, a verifier runs the task's check command here — only its result decides whether the task is done, so run the check yourself before you finish. Your diff is returned to the caller; don't commit.")
	}

	tools, _ := s.tools()
	has := map[string]bool{}
	for _, t := range tools {
		has[t.Name] = true
	}
	if s.client.Native() {
		parts = append(parts, nativeToolsPrompt)
	} else {
		parts = append(parts, fmt.Sprintf(textToolsPrompt, textToolDocs(tools)))
		if has["task"] {
			var b strings.Builder
			b.WriteString("Subagents available to <task agent=\"…\">:\n")
			for _, a := range s.orch.subagentsFor(s) {
				fmt.Fprintf(&b, "- %s: %s\n", a.Name, strings.TrimSpace(a.Description))
			}
			parts = append(parts, strings.TrimRight(b.String(), "\n"))
		}
	}
	if has["task"] {
		parts = append(parts, delegationPrompt)
	}
	if has["delegate"] {
		parts = append(parts, delegatePrompt)
		if !s.client.Native() {
			// A text-transport model has no tool schema to carry a description, so
			// the one the native path puts there goes into the prompt instead —
			// the same text, so the result contract, the statuses and the role
			// list cannot drift between the two transports we ship at parity.
			parts = append(parts, delegateDescription(s))
		}
	}
	if has["todowrite"] {
		parts = append(parts, todoPrompt)
	}

	j := s.jail()
	env := fmt.Sprintf(`# Environment
- Working directory (jail): %s
  All paths are relative to here.`, j.Root)
	if rem := s.remote(); rem != nil {
		env = fmt.Sprintf(`# Environment
- The project is on another machine: %s:%s
  You reach it over ssh, which is already set up — file paths are relative to
  that directory, and run_command executes there. You are NOT confined to a
  container on this side: reading, editing and running commands all happen on
  %s. Don't try to ssh by hand; just use the tools.`, rem.Host, rem.Dir, rem.Host)
	}
	if j.Unsafe {
		env += "\n- Unsafe mode: paths outside the directory are reachable and commands run through sh."
	} else {
		shell := " (no shell: no pipes, redirects or &&)"
		if j.Shell {
			shell = " (run through sh: pipes, redirects and && work; every command in the line must be on the list)"
		}
		env += "\n  You cannot read or write outside it.\n- Allowlisted commands" + shell + ": " + strings.Join(j.Allowed, ", ")
	}
	isGit := "no"
	if _, err := os.Stat(filepath.Join(j.Root, ".git")); err == nil {
		isGit = "yes"
	}
	// No model name here: a gateway fallback switches models mid-session, and
	// the system prompt must stay byte-identical for the prefix cache.
	env += fmt.Sprintf("\n- Git repository: %s\n- Platform: %s/%s\n- Today's date: %s",
		isGit, runtime.GOOS, runtime.GOARCH, time.Now().Format("2006-01-02"))
	if !j.Unsafe {
		env += "\n- GPU work only through the scheduler: bsk submit -g <gpus> -- <command> (srun/sbatch/torchrun and CUDA_VISIBLE_DEVICES are blocked)"
	}
	parts = append(parts, env)

	if has["skill"] {
		if idx := skillsIndex(s); idx != "" {
			parts = append(parts, idx)
		}
	}
	if name, content, ok := loadProjectInstructions(j); ok {
		parts = append(parts, fmt.Sprintf("# Project instructions (from %s)\n"+
			"The user maintains these project-specific instructions. Follow them; they take precedence over your defaults where they conflict.\n\n%s", name, content))
	}
	return strings.Join(parts, "\n\n") + "\n"
}
