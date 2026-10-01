package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// The task tool — subagent orchestration (ported from opencode's tool/task.ts).
// A call creates a child session for the named subagent: its own system
// prompt, model, step budget and permissions; the prompt is its first user
// message; the same Run loop drives it; its final answer comes back as the
// tool result. Several task calls in one reply run concurrently (bounded by
// LCA_MAX_PARALLEL). task_id resumes a child with its context intact;
// background=true returns at once and delivers the result to the caller's
// next step. Nesting is limited by subagent_depth (default 1: subagents cannot
// spawn subagents).

func init() {
	registerTool(&ToolDef{
		Name: "task",
		Desc: "Delegate a task to a subagent.", // replaced per session by taskDescription
		Params: []Param{
			{Name: "description", Type: "string", Desc: "A short (3-5 words) description of the task", Required: true},
			{Name: "prompt", Type: "string", Desc: "The complete task for the subagent: goal, context, constraints, and what to return", Required: true},
			{Name: "agent", Type: "string", Desc: "Which subagent to use (see the list in the description)", Required: true},
			{Name: "task_id", Type: "string", Desc: "Resume a previous subagent session (its id from an earlier result) instead of starting fresh"},
			{Name: "background", Type: "boolean", Desc: "Run in the background and continue working; the result is delivered automatically when it finishes"},
		},
		Body: "prompt",
		TextDoc: `Delegate self-contained work to a subagent (see the agent list below); several
<task> tags in one reply run in parallel, background="true" returns immediately:
<task agent="explore" description="find auth middleware">
Find where HTTP auth middleware is defined and which routes use it. Medium thoroughness.
Return file paths with line numbers.
</task>`,
		Parallel: true,
		Run:      runTaskTool,
	})
}

func taskDescription(s *Session) string {
	var b strings.Builder
	b.WriteString(`Launch a subagent to handle a task autonomously in its own context.

When to use: broad codebase searches and questions (explore), independent multi-step work that can run in parallel (general), or any specialized agent below whose description matches.

Rules:
- Launch several subagents in ONE response when their work is independent — they run concurrently.
- The subagent starts with none of your context. Write a complete prompt: the goal, relevant paths and facts, constraints, whether it may change code or only research, and exactly what to return.
- Its result is returned to you, not shown to the user — summarize what matters for the user.
- Don't redo work you delegated. Use task_id to continue a previous subagent with its context.
- background=true: the subagent runs while you continue; its result is delivered automatically on a later step. Don't poll or wait for it.

Available agents:
`)
	for _, a := range s.orch.subagentsFor(s) {
		fmt.Fprintf(&b, "- %s: %s\n", a.Name, strings.TrimSpace(a.Description))
	}
	return strings.TrimRight(b.String(), "\n")
}

func runTaskTool(tc *ToolCtx, a Args) string {
	s, o := tc.S, tc.S.orch
	if s.depth >= o.subagentDepth() {
		return fmt.Sprintf("error: subagent depth limit reached (%d) — do this work yourself (raise subagent_depth to allow nesting)", o.subagentDepth())
	}
	name := firstNonEmpty(a.Str("agent"), a.Str("subagent_type"))
	desc := firstNonEmpty(a.Str("description"), "task")
	prompt := strings.TrimSpace(a.Str("prompt"))
	if prompt == "" {
		return "error: prompt is required"
	}

	var child *Session
	if id := a.Str("task_id"); id != "" {
		o.mu.Lock()
		child = o.children[id]
		o.mu.Unlock()
		if child == nil || child.parent != s {
			return fmt.Sprintf("error: unknown task_id %q for this agent — start a new task without task_id", id)
		}
		// Claim it atomically: two resumes of one child must not run it twice.
		if !child.running.CompareAndSwap(false, true) {
			return fmt.Sprintf("error: subagent %s is still running — wait for its result", id)
		}
		name = child.agent.Name
	} else {
		ag := o.agents[name]
		if ag == nil || !ag.isSubagent() || ag.Hidden {
			var names []string
			for _, x := range o.subagentsFor(s) {
				names = append(names, x.Name)
			}
			return fmt.Sprintf("error: unknown agent %q. Available: %s", name, strings.Join(names, ", "))
		}
		if msg, ok := tc.Ask("task", name, "TASK "+name+": "+desc, "  "+truncate(prompt, 300)); !ok {
			return msg
		}
		var err error
		child, err = o.newChild(s, ag, desc)
		if err != nil {
			return "error: " + err.Error()
		}
		child.running.Store(true)
	}
	child.Msgs = append(child.Msgs, Message{Role: "user", Content: prompt})
	s.event("task", map[string]any{"task_id": child.ID, "subagent": name, "description": desc, "model": child.client.Ref(), "background": a.Bool("background")})

	if a.Bool("background") {
		s.mu.Lock()
		s.bgRunning++
		s.mu.Unlock()
		child.view = newChildView(child, true)
		child.background = true
		go func() {
			res := o.runChild(context.Background(), child)
			s.deliver(res)
		}()
		return fmt.Sprintf("<task id=%q agent=%q state=\"running\">\nStarted in the background. Its result will be delivered automatically when it finishes — do not wait or poll; continue with other work, or end your reply if nothing else remains.\n</task>", child.ID, name)
	}
	return o.runChild(tc.Ctx, child)
}

const (
	// forkBudgetShare bounds the inherited context to half the child's budget: a
	// subagent that arrives with three-quarters of its window already full has
	// nowhere to do the work, and Run's auto-compact cannot rescue it (it needs
	// len(Msgs) > 4, and a forked transcript is exactly system+context+ack+task).
	forkBudgetShare = 2
	forkHeader      = "[Context inherited from the calling agent: the files it had already read, verbatim. Read a file again before changing it if the tool tells you to.]"
	forkAck         = "Understood — I have the files above and will work from them."
)

// forkedContext hands a child the caller's accumulated file knowledge instead of
// a blank transcript. Only the latest read of each path is inherited (an earlier
// read is superseded waste, the same argument dedupeReads makes), and it is
// rendered in the text transport's own <tool_result …> spelling so that (a) it is
// valid for a native and a text child alike — a native parent's tool_calls can
// never be replayed to a text child — and (b) the existing trimming, dedupe and
// compaction machinery already recognises it. The synthetic acknowledgement is
// there so the block does not hand the model a dangling tool result with nothing
// said about it; what keeps the transcript from ending on an assistant message is
// the task that runDelegateTool appends after this returns.
func (o *Orchestrator) forkedContext(parent, child *Session) (msgs []Message, files, dropped int) {
	type read struct{ path, body string }
	var found []read
	seen := map[string]bool{}
	add := func(path, body string) {
		// An error is no knowledge and a stub is knowledge already thrown away.
		if path == "" || seen[path] || isStub(body) || strings.HasPrefix(body, "error:") {
			return
		}
		// A fork cannot run an approval prompt on the child's behalf, so only an
		// outright Allow is inheritable: what the child would have to ask for
		// (.env under the default rules) it is not handed either. A path that does
		// not resolve inside its jail is one it could not have read at all.
		if _, err := child.jail().Resolve(path); err != nil {
			return
		}
		if Evaluate("read", permPath(child.jail(), path), child.rules()...) != Allow {
			return
		}
		// The body is rendered back into that same framing, so a file carrying it
		// could close its own block and forge a read of another path. Such a file
		// is not inherited; the child reads it for itself.
		if reToolFraming.MatchString(body) {
			return
		}
		seen[path] = true
		found = append(found, read{path, body})
	}
	// A result carries the name the model called the tool by, so an alias
	// ("read", "cat") has to resolve to read_file like everywhere else.
	isRead := func(name string) bool {
		d := resolveToolName(name)
		return d != nil && d.Name == "read_file"
	}
	// Backwards: the first sighting of a path is its latest read.
	for i := len(parent.Msgs) - 1; i >= 1; i-- {
		m := parent.Msgs[i]
		switch {
		case m.Role == "tool" && isRead(m.Tool):
			add(m.Path, m.Content)
		case m.Role == "user" && strings.HasPrefix(m.Content, "<tool_result"):
			blocks, ok := parseToolResults(m.Content)
			if !ok {
				continue // ambiguous framing: nothing in it can be trusted
			}
			for j := len(blocks) - 1; j >= 0; j-- {
				if isRead(blocks[j].name) {
					add(blocks[j].path, blocks[j].body)
				}
			}
		}
	}
	if len(found) == 0 {
		return nil, 0, 0
	}
	// The parent's own order: a stable prefix, with the most recently read file
	// last, nearest the task.
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	block := func(r read) Message {
		// Each file its own message, exactly as appendResults spells it, so
		// dedupeReads and trimForContext can collapse them one by one.
		return Message{Role: "user", Content: strings.TrimRight(toolResultText("read_file", r.path, r.body), "\n")}
	}
	render := func(rs []read) []Message {
		out := []Message{{Role: "user", Content: forkHeader}}
		for _, r := range rs {
			out = append(out, block(r))
		}
		return append(out, Message{Role: "assistant", Content: forkAck, Agent: child.agent.Name})
	}
	if budget := child.budget() / forkBudgetShare; budget > 0 {
		// estimateTokens is a per-message sum, so dropping the front block is
		// its own cost off the total — no need to re-render to weigh what is left.
		total := estimateTokens(render(found))
		for len(found) > 0 && total > budget {
			total -= estimateTokens([]Message{block(found[0])})
			found = found[1:] // the oldest read first: the task is about the newest
			dropped++
		}
	}
	if len(found) == 0 {
		return nil, 0, dropped
	}
	for _, r := range found {
		// Seeded only when the inherited bytes ARE the worktree's bytes: then the
		// child has genuinely seen the file and may edit it without re-reading.
		// Otherwise nothing is seeded and checkStale still forces a fresh read.
		prefix := r.path + ":\n"
		if !strings.HasPrefix(r.body, prefix) {
			continue // a line range or a truncated read is not the whole file
		}
		abs, err := child.jail().Resolve(r.path)
		if err != nil {
			continue
		}
		body := strings.TrimPrefix(r.body, prefix)
		if data, err := os.ReadFile(abs); err == nil && string(data) == body {
			// The CHILD's own read set, not the parent's. The records are per-session
			// now, so seeding the parent's would hand the child nothing and quietly
			// refresh the caller's knowledge of a file it has not looked at again.
			child.noteLocalRead(child.jail(), r.path, body, true)
		}
	}
	return render(found), len(found), dropped
}

// newChild creates a subagent session under parent.
func (o *Orchestrator) newChild(parent *Session, ag *Agent, desc string) (*Session, error) {
	o.mu.Lock()
	o.nextTask++
	id := fmt.Sprintf("t%d", o.nextTask)
	o.mu.Unlock()

	child := &Session{ID: id, UID: parent.rootUID() + "-" + id, orch: o, parent: parent, depth: parent.depth + 1, agent: ag, title: desc,
		wake: make(chan struct{}, 1), Raw: parent.Raw}
	// A workflow's delegate step sets this on the caller so the subagent's
	// verifier streams into run.log: a delegation killed after twenty minutes
	// must have left something diagnosable behind.
	child.checkLive = parent.checkLive
	// Model: the role's chain, the agent's own, else the caller's.
	if len(ag.Models) > 0 {
		child.models = append([]string(nil), ag.Models...)
		child.useModel(0)
	} else if ag.Model != "" {
		if err := child.SetModel(ag.Model); err != nil {
			return nil, fmt.Errorf("agent %s: %w", ag.Name, err)
		}
	} else {
		// A private copy: /model or /endpoint on the REPL must not retarget a
		// subagent mid-conversation.
		c := *parent.client
		child.client = &c
		child.models, child.modelIdx = parent.models, parent.modelIdx
	}
	// Session restrictions: the parent's denies carry down; a subagent can't
	// keep a todo list or spawn further subagents unless its agent explicitly
	// grants it (and the depth limit allows).
	// The parent agent's denies carry down too, so e.g. plan mode's no-edit
	// rule can't be sidestepped by delegating the edit.
	for _, rs := range []Ruleset{parent.agent.BaseRules, parent.agent.Rules, parent.extra} {
		for _, r := range rs {
			if r.Action == Deny && !(r.Permission == "*" && r.Pattern == "*") {
				child.extra = append(child.extra, r)
			}
		}
	}
	if !mentions(ag.Rules, "todo") && !mentions(ag.BaseRules, "todo") {
		child.extra = append(child.extra, Rule{"todo", "*", Deny})
	}
	if !mentions(ag.Rules, "task") && !mentions(ag.BaseRules, "task") {
		child.extra = append(child.extra, Rule{"task", "*", Deny})
	}
	// A write leaves this machine and lands in somebody's ticket. An overnight run
	// must not be able to comment on one because a model thought it would help, and
	// a background child cannot be asked. This is the single chokepoint — task,
	// delegate, the delegate reviewer, custom commands and workflow prompt steps all
	// come through newChild — and the denial is structural rather than a refusal:
	// toolsFor drops a tool whose permission is Disabled, so an ungranted child
	// never SEES a write schema, cannot emit a call, cannot be talked into one by
	// injected ticket text, and costs fewer prefix tokens. The role grants it by
	// naming the key; nothing else does.
	if !mentions(ag.Rules, "mcp_write") && !mentions(ag.BaseRules, "mcp_write") {
		child.extra = append(child.extra, Rule{"mcp_write", "*", Deny})
	}
	child.view = newChildView(child, false)
	if ev, ok := parent.view.(*evalView); ok {
		child.view = ev // eval output stays compact
	}
	// The machine: the role's own member:, else the CALLER's. Not
	// defaults.member — the task tool's contract is that a subagent shares the
	// caller's tree, so it must share the caller's machine. A delegation resolves
	// through memberFor instead, because it creates a tree of its own. Set before
	// Msgs[0] so the environment block names the right machine from the first
	// byte.
	child.member = firstNonEmpty(ag.Member, parent.memberName())
	child.Msgs = []Message{{Role: "system", Content: child.systemPrompt()}}

	o.mu.Lock()
	o.children[id] = child
	o.mu.Unlock()
	return child, nil
}

func mentions(rs Ruleset, perm string) bool {
	for _, r := range rs {
		if r.Permission == perm {
			return true
		}
	}
	return false
}

// runChild runs a child session to completion and formats its result.
func (o *Orchestrator) runChild(ctx context.Context, child *Session) string {
	child.running.Store(true)
	defer child.running.Store(false)
	// Only top-level subagents take a concurrency slot: a nested one waiting
	// for a slot its own ancestor holds would deadlock.
	if child.depth == 1 {
		select {
		case o.slots <- struct{}{}:
		case <-ctx.Done():
			return formatTask(child, "cancelled", "the task was cancelled before it started")
		}
		defer func() { <-o.slots }()
	}

	start := time.Now()
	entry := o.trackStart(child.ID, "task", child.agent.Name, child.title)
	child.view.Begin()
	err := child.Run(ctx)
	state, text := "completed", lastAssistantText(child)
	switch {
	case err == context.Canceled:
		state = "cancelled"
		if text == "" {
			text = "the task was interrupted before it finished"
		}
	case err != nil:
		state = "error"
		text = strings.TrimSpace(text + "\n\nsubagent failed: " + err.Error())
	case text == "":
		text = "(the subagent finished without a final message)"
	}
	child.view.Finish(state, time.Since(start))
	o.trackEnd(entry, state, text)
	child.event("task_done", map[string]any{"state": state, "ms": time.Since(start).Milliseconds(), "bytes": len(text)})
	return formatTask(child, state, text)
}

func formatTask(child *Session, state, text string) string {
	tag := "task_result"
	if state == "error" {
		tag = "task_error"
	}
	return fmt.Sprintf("<task id=%q agent=%q description=%q state=%q>\n<%s>\n%s\n</%s>\n</task>\n(continue this subagent with task_id %q)",
		child.ID, child.agent.Name, child.title, state, tag, text, tag, child.ID)
}

func (o *Orchestrator) forgetChild(id string) {
	o.mu.Lock()
	delete(o.children, id)
	o.mu.Unlock()
}

// Tasks lists the subagent sessions of this orchestrator, for /tasks.
func (o *Orchestrator) Tasks() []*Session {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]*Session, 0, len(o.children))
	for i := 1; i <= o.nextTask; i++ {
		if c := o.children[fmt.Sprintf("t%d", i)]; c != nil {
			out = append(out, c)
		}
	}
	return out
}
