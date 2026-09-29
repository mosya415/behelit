package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// delegate(role, task) — the unit of orchestration through roles. The
// subagent is the same loop with another role, working in its own git
// worktree: a snapshot of the caller's working tree (uncommitted and new files
// included), so parallel delegations can't step on each other or on the user.
// When it stops, the verifier runs the task's check_cmd there. Only
// {status, diff, test_tail} goes back up — never the subagent's transcript.
// A passed diff is applied to the caller's tree (apply: verified).

func init() {
	registerTool(&ToolDef{
		Name: "delegate",
		Desc: "Delegate a self-contained change to another role.", // per session: delegateDescription
		Params: []Param{
			{Name: "role", Type: "string", Desc: "Role to run the task (see the list)", Required: true},
			{Name: "task", Type: "string", Desc: "Complete, self-contained task: goal, relevant files and facts, constraints, definition of done", Required: true},
			{Name: "check_cmd", Type: "string", Desc: "Command that proves the task is done (exit 0), e.g. \"go test ./pkg/parser/...\". Runs in addition to the role's own check_cmd"},
			{Name: "review", Type: "boolean", Desc: "Let the role's reviewer judge the diff after the check passes (default: on when the role has one); false skips it"},
			{Name: "fork", Type: "boolean", Desc: "Start the subagent from the files you have already read instead of a blank context (default: the role's own setting)"},
		},
		Body: "task",
		TextDoc: `Delegate a self-contained change to another role; it works in its own git
worktree and a verifier runs check_cmd. The result, the statuses and the optional
review= and fork= attributes are described under "Delegating changes to roles".
Several delegates in one reply run in parallel:
<delegate role="coder" check_cmd="go test ./parser/..." review="false" fork="true">
Fix the off-by-one in parser.Lex for trailing newlines; add a test.
</delegate>`,
		Parallel: true,
		Run:      runDelegateTool,
	})
}

func delegateDescription(s *Session) string {
	var b strings.Builder
	b.WriteString(`Delegate a self-contained change to another role. The subagent runs in an isolated git worktree (a snapshot of the current tree) with its own model and tools. When it finishes, a VERIFIER runs check_cmd there: the verifier, not the model, decides whether the task is done.

Returns JSON {"status", "diff", "test_tail", "review"}:
- status: passed (check exit 0; the diff has been applied to your tree), failed (check still failing; not applied), unverified (no check_cmd; not applied), rejected (the check passed but the reviewer blocked it; not applied — the reason is in review.reasons), conflict (passed but didn't apply cleanly to your tree), error, cancelled
- diff: the subagent's changes as a unified diff
- test_tail: the last lines of the check output
- review: present only when a reviewer judged the diff: {role, model, verdict, reasons}

Rules:
- The subagent sees none of your context: state the goal, the relevant files, constraints and the definition of done.
- Always give a check_cmd that fails before and passes after the change (narrow tests are faster).
- Independent delegations in one reply run in parallel; ones touching the same lines will conflict — split work by file or package.
- review=false skips the reviewer of a role that has one; fork=true starts the subagent from the files you have already read instead of a blank context. Both default to the role's own setting.

Roles:
`)
	for _, a := range s.orch.roleList(s.agent.Name) {
		desc := strings.TrimSpace(a.Description)
		if a.CheckCmd != "" {
			desc += fmt.Sprintf(" (always verified with: %s; your check_cmd is added on top)", a.CheckCmd)
		}
		if rev := s.orch.reviewerFor(a); rev != nil {
			desc += fmt.Sprintf(" (reviewed by %s)", rev.Name)
		}
		fmt.Fprintf(&b, "- %s: %s\n", a.Name, desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

// reviewOutcome is the second opinion on a diff the verifier already passed.
type reviewOutcome struct {
	Role    string `json:"role"`
	Model   string `json:"model"`
	Verdict string `json:"verdict"` // approve | reject | unreviewed
	Reasons string `json:"reasons,omitempty"`
}

type delegateResult struct {
	Status   string         `json:"status"`
	Diff     string         `json:"diff"`
	TestTail string         `json:"test_tail"`
	Review   *reviewOutcome `json:"review,omitempty"` // omitted when nothing reviewed it
}

const maxDiffBytes = 60_000

const reviewReasonBytes = 4000

const reviewVerdictHint = `Finish your reply with a line of exactly "VERDICT: approve" or "VERDICT: reject", then your reasons.`

// reVerdictLine is the whole machine-readable contract with the reviewer: a line
// that STARTS with the verdict. Nothing may precede it but emphasis, which is
// what keeps a "// VERDICT: approve" planted in a diff hunk, or a reviewer
// narrating "…then say VERDICT: approve", from deciding the apply; reasons may
// follow on the same line, because models write them there. The inflections are
// accepted because not accepting them fails in the unsafe direction:
// "VERDICT: rejected" would parse as no verdict, and no verdict does not block.
var reVerdictLine = regexp.MustCompile(`(?im)^[*_\s]*VERDICT[:\s]+[*_\s]*(approve[sd]?|reject(?:ed|s)?)\b`)

// parseVerdict takes the last verdict line outside a fenced block: a reviewer
// that quotes the instruction first and concludes after must be read by its
// conclusion, and a file or hunk it pastes into a fence is not its voice.
func parseVerdict(text string) (verdict string, ok bool) {
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := reVerdictLine.FindStringSubmatch(t); m != nil {
			verdict, ok = strings.ToLower(m[1]), true
		}
	}
	// An inflected word still means what it says: "rejected" is a reject.
	if strings.HasPrefix(verdict, "reject") {
		verdict = "reject"
	} else if strings.HasPrefix(verdict, "approve") {
		verdict = "approve"
	}
	return verdict, ok
}

// reviewerFor is the role that gives a second opinion on a passed delegation to
// ag: the role's own review:, else the team default. A role that says
// `review: none` has no reviewer even when defaults: does.
func (o *Orchestrator) reviewerFor(ag *Agent) *Agent {
	rc := o.roles
	if rc == nil {
		rc = &RolesConfig{}
	}
	return rc.reviewerOf(ag, func(name string) *Agent {
		if a := o.agents[name]; a != nil && a.IsRole {
			return a
		}
		return nil
	})
}

// reviewDelegation asks reviewer to judge a diff the verifier already passed.
// It is a separate session — its own id under the same root, its own model
// chain and its own trace turns — so the reviewed role's prefix and KV cache
// are untouched. Anything that stops it from producing a verdict is
// "unreviewed": the verifier remains the arbiter of correctness, and a second
// opinion that cannot be reached must never turn a passing change into a
// failure.
func (o *Orchestrator) reviewDelegation(ctx context.Context, child *Session, reviewer *Agent, task, diff, tail string) reviewOutcome {
	out := reviewOutcome{Role: reviewer.Name, Verdict: "unreviewed"}
	if len(reviewer.Models) == 0 && reviewer.Model == "" {
		// newChild would fall back to the caller's client, which here is the
		// REVIEWED subagent: the model that wrote the diff would approve it.
		out.Reasons = fmt.Sprintf("reviewer %s has no model chain — give it one with /role %s model <name>", reviewer.Name, reviewer.Name)
		return out
	}
	rev, err := o.newChild(child, reviewer, "review of "+truncate(firstLine(task), 40))
	if err != nil {
		out.Reasons = "could not start the reviewer: " + err.Error()
		return out
	}
	defer o.forgetChild(rev.ID) // the worktree dies with this call: not resumable via task_id
	// The reviewer reads the worktree, so it runs WHERE the worktree is, whatever
	// its own member: says. A reviewer pinned elsewhere produced a warning at
	// load time; moving it silently is the thing that must not happen.
	rev.jl, rev.isolated = child.jl, true
	rev.member, rev.wtRem = child.member, child.wtRem
	// rules() puts s.extra last, so this beats isolationRules' edit:* Allow: a
	// reviewer must not be able to edit its way into the diff it is judging. The
	// apply itself is safe regardless — diff and wt.changedFiles were captured
	// before the review started — but a reviewer grading its own patch is not.
	rev.extra = append(rev.extra, Rule{"edit", "*", Deny}, Rule{"delegate", "*", Deny})
	rev.RefreshSystem()
	out.Model = rev.client.Model()

	rev.Msgs = append(rev.Msgs, Message{Role: "user", Content: fmt.Sprintf(`Another role just finished this task in an isolated git worktree and the
verifier's check PASSED. Review the diff before it is applied to the project.

<task>
%s
</task>

<check_output>
%s
</check_output>

<diff>
%s
</diff>

You are inside that worktree: read the files the diff touches and run whatever
convinces you. Judge correctness, not style — a change that passes its test and
is still wrong is what this review exists to catch. You cannot edit anything.

%s
Reject only for a defect you can name: a reject blocks the change, and no
verdict line at all lets it proceed.`, task, orNone(tail), headTail(diff, maxDiffBytes), reviewVerdictHint)})

	start := time.Now()
	rev.view.Begin()
	err = rev.Run(ctx)
	text := lastAssistantText(rev)
	verdict, ok := parseVerdict(text)
	if err == nil && ctx.Err() == nil && !ok {
		// Exactly one re-ask: a reviewer that forgot the line gets told once,
		// and a second miss is "unreviewed" rather than a stalled pipeline.
		rev.Msgs = append(rev.Msgs, Message{Role: "user", Content: "[review] Your reply carried no verdict line, so nothing was decided. Reply with the single line VERDICT: approve or VERDICT: reject, then the reasons."})
		err = rev.Run(ctx)
		text = lastAssistantText(rev)
		verdict, ok = parseVerdict(text)
	}
	rev.view.Finish(firstNonEmpty(verdict, "unreviewed"), time.Since(start))
	rev.saveTranscript()

	// A parsed verdict wins over everything, an interrupt included: a reject that
	// was received and then thrown away by a Ctrl-C would be an interrupt that
	// merges a diff.
	switch {
	case ok:
		out.Verdict = verdict
		out.Reasons = truncate(strings.TrimSpace(reVerdictLine.ReplaceAllString(text, "")), reviewReasonBytes)
	case ctx.Err() != nil:
		out.Reasons = "the review was cancelled"
	case err != nil:
		out.Reasons = err.Error()
	default:
		out.Reasons = "the reviewer gave no verdict line twice; the verifier's pass stands"
	}
	return out
}

// lastAssistantText is the reply a child session is judged by: the last
// assistant message that actually said something.
func lastAssistantText(s *Session) string {
	for i := len(s.Msgs) - 1; i > 0; i-- {
		if s.Msgs[i].Role == "assistant" {
			if t := finalText(s.Msgs[i]); t != "" {
				return t
			}
		}
	}
	return ""
}

func runDelegateTool(tc *ToolCtx, a Args) string {
	s, o := tc.S, tc.S.orch
	start := time.Now()
	if s.depth >= o.subagentDepth() {
		return fmt.Sprintf("error: subagent depth limit reached (%d) — do this work yourself", o.subagentDepth())
	}
	roleName, task := a.Str("role"), strings.TrimSpace(a.Str("task"))
	role := o.agents[roleName]
	if role == nil || !role.IsRole || roleName == s.agent.Name {
		var names []string
		for _, r := range o.roleList(s.agent.Name) {
			names = append(names, r.Name)
		}
		return fmt.Sprintf("error: unknown role %q. Available: %s", roleName, strings.Join(names, ", "))
	}
	if task == "" {
		return "error: task is required"
	}
	// The role's check_cmd always runs — the calling model can add a check
	// but not replace it (a caller-chosen "echo ok" must not pass the task).
	var checks []string
	if role.CheckCmd != "" {
		checks = append(checks, role.CheckCmd)
	}
	if extra := strings.TrimSpace(a.Str("check_cmd")); extra != "" && extra != role.CheckCmd {
		checks = append(checks, extra)
	}
	check := strings.Join(checks, " && ")
	// The machine this role's sessions work on, overridden by a workflow step's
	// member:. The check is asked of THAT machine's sandbox, not the caller's: a
	// GPU member's allowlist is the one the check will meet.
	target := o.memberFor(role)
	if tc.Member != "" {
		m := o.member(tc.Member)
		if m == nil {
			// Not a fallback to the role's own machine: a caller that named a member
			// has to be told its name is not one, or the work quietly runs elsewhere.
			return marshalDelegate(delegateResult{Status: "error", TestTail: fmt.Sprintf(
				"member %s is not a member (members: %s)", tc.Member, strings.Join(o.memberNames(), ", "))})
		}
		target = m
	}
	for _, c := range checks {
		if err := checkOn(o.policyOf(target), target, c); err != nil {
			return marshalDelegate(delegateResult{Status: "error", TestTail: "check_cmd rejected by the sandbox: " + err.Error()})
		}
	}
	if msg, ok := tc.Ask("delegate", roleName, "DELEGATE "+roleName, "  "+truncate(task, 300)+"\n  check: "+orNone(check)); !ok {
		return msg
	}
	// Top-level delegations share the subagent concurrency bound.
	if s.depth == 0 {
		select {
		case o.slots <- struct{}{}:
			defer func() { <-o.slots }()
		case <-tc.Ctx.Done():
			return marshalDelegate(delegateResult{Status: "cancelled"})
		}
	}

	// The caller's TREE, which is not the same question as the caller's member: a
	// nested delegation's caller is itself a worktree on that member (s.remote()
	// is that worktree's transport), and applying a verified diff into the
	// member's real checkout instead is an isolation escape the local path does
	// not have.
	caller := s.memberOf()
	if rem := s.remote(); rem != nil && rem != caller.Rem {
		c := *caller
		c.Rem = rem
		caller = &c
	}
	// Where the worktree is made depends on the TARGET alone; whether the caller's
	// tree is that same tree is the second question. Branching both on one flag
	// refused a role pinned to member local whenever the caller lived on a member
	// — the laptop half of "build on the box, commit on the laptop" — with
	// createOn's internal precondition note.
	sameTree := target.IsLocal() && caller.IsLocal()
	var wt *worktree
	var err error
	if target.IsLocal() {
		wt, err = o.worktrees.create(s.jail().Root, o.cfg.stateDir(), s.rootUID())
	} else {
		wt, err = o.worktrees.createOn(tc.Ctx, target, s.rootUID())
	}
	if err != nil {
		// A member that could not be reached, has no git, or is not a repository
		// is a delegation that did not happen — reported in the result contract,
		// so a workflow step reads it like any other failure.
		return marshalDelegate(delegateResult{Status: "error", TestTail: "cannot create a worktree: " + err.Error()})
	}
	wt.mem = target // createOn already knows; create does not, and heads() names it
	if !sameTree {
		// git diff paths are repository-relative, so the subagent's project must
		// sit at the same place in its repository as the caller's does in the
		// caller's. Compared right after create (one round trip fewer than probing
		// first); a mismatch is a patch nothing could survive, so it is refused
		// here rather than surfacing later as a mysterious conflict.
		top, sub, cerr := o.callerLayout(tc.Ctx, caller, s.jail().Root)
		if cerr != nil {
			wt.remove()
			return marshalDelegate(delegateResult{Status: "error", TestTail: cerr.Error()})
		}
		if sub != wt.sub {
			wt.remove()
			return marshalDelegate(delegateResult{Status: "error", TestTail: fmt.Sprintf(
				"delegate to %s on member %s: the project sits at %s of its repository there, but at %s of yours on %s — a diff's paths are relative to the repository, so the two must match. Point members.%s dir: at the same subdirectory, or run %s on member local.",
				roleName, target.MemberName(), layoutWord(wt.sub), layoutWord(sub), caller.MemberName(), target.MemberName(), roleName)})
		}
		wt.caller, wt.callerTop = caller, top
	}
	defer wt.remove()

	child, err := o.newChild(s, role, truncate(firstLine(task), 60))
	if err != nil {
		return "error: " + err.Error()
	}
	o.forgetChild(child.ID) // its worktree dies with this call: not resumable via task_id
	tc.TaskSession = child.UID
	if target.IsLocal() {
		// A worktree on this machine, so the child gets a local jail rooted in it —
		// built from the TARGET member's policy, not the team's: the allowlist the
		// check_cmd was just validated against has to be the one the subagent runs
		// under, or a member's narrower allow:/shell: is bypassed by one delegate
		// call. The member name travels with it so a refusal still names the machine.
		base := o.policyOf(target)
		child.member = target.MemberName()
		child.jl, err = NewJail(wt.root, base.Allowed, base.Unsafe)
		if err != nil {
			return "error: " + err.Error()
		}
		child.jl.Shell, child.jl.Member = base.Shell, base.Member
	} else {
		// The worktree's own transport, so Remote.relPath confines the subagent to
		// its worktree's project directory exactly as it confines a session to a
		// project. No local jail: there is no local tree to confine.
		child.member, child.wtRem, child.jl = target.MemberName(), wt.rem.withDir(wt.root), nil
		child.view.Note(fmt.Sprintf("DELEGATE %s on %s — the subagent starts from THAT machine's working tree, not yours; its diff applies here only where the two agree",
			roleName, target.Label()))
	}
	child.isolated = true
	child.RefreshSystem()
	// Before the task message and after RefreshSystem: the child's own system
	// prompt stays Msgs[0], byte-identical to any other session of that role,
	// because the prefix is the gateway's cache key.
	wantFork := argBool(a, "fork", role.Fork)
	if wantFork && !sameTree {
		// forkedContext resolves paths with child.jail().Resolve and seeds mtimes
		// with os.ReadFile: inheriting one machine's bytes as another's truth would
		// seed noteRead for files the child has never seen and let it edit without
		// reading.
		child.view.Note(fmt.Sprintf("FORK  skipped: your reads are from %s, %s works in its own tree on %s — it reads those files there itself",
			caller.Label(), roleName, target.Label()))
		wantFork = false
	}
	if wantFork {
		src := s
		if tc.ForkFrom != nil {
			src = tc.ForkFrom
		}
		inherited, files, dropped := o.forkedContext(src, child)
		if len(inherited) > 0 {
			child.Msgs = append(child.Msgs, inherited...)
			child.view.Note(fmt.Sprintf("FORK  %s inherited from the caller (~%dk tokens, %d dropped for context)",
				plural(files, "file", "files"), estimateTokens(inherited)/1000, dropped))
			s.event("delegate_fork", map[string]any{"task_id": child.ID, "files": files, "dropped": dropped, "tokens": estimateTokens(inherited)})
		}
	}
	prompt := task
	if check != "" {
		prompt += fmt.Sprintf("\n\n---\nDefinition of done: `%s` exits 0 in this working tree. A verifier runs it after you stop; it decides, not you.", check)
	}
	child.Msgs = append(child.Msgs, Message{Role: "user", Content: prompt})
	s.event("delegate", map[string]any{"task_id": child.ID, "role": roleName, "check": check, "worktree": wt.dir, "member": target.MemberName()})

	entry := o.trackStart(child.ID, "delegate", roleName, task)
	child.view.Begin()
	v := child.RunVerifiedAll(tc.Ctx, checks, o.verifyAttempts())
	child.view.Finish(v.Status, time.Since(start))

	diff, files, derr := wt.diff()
	if derr != nil && v.Status != "error" {
		v.Status, v.Tail = "error", "collecting the diff failed: "+derr.Error()
	}
	var review *reviewOutcome
	reviewer := o.reviewerFor(role)
	if tc.Reviewer != "" {
		reviewer = o.agents[tc.Reviewer]
	}
	// Nothing to review on an unchanged tree, and a failed check is the
	// verifier's business, not a second opinion's.
	if reviewer != nil && argBool(a, "review", true) && v.Status == "passed" && diff != "" {
		ro := o.reviewDelegation(tc.Ctx, child, reviewer, task, diff, v.Tail)
		review = &ro
		switch {
		case ro.Verdict == "reject":
			v.Status = "rejected"
			v.Tail = strings.TrimSpace(v.Tail + "\n\n" + ro.Role + " rejected the change:\n" + ro.Reasons)
		case tc.Ctx.Err() != nil:
			// The review was cut short: it may have been about to block this
			// diff, so the interrupt cannot be the thing that merges it.
			v.Status = "cancelled"
		}
	}
	applied := false
	var apply bool
	switch o.applyPolicy() {
	case "verified":
		apply = v.Status == "passed"
	// "rejected" is none of these, so apply: always cannot override a reject.
	case "always":
		apply = v.Status == "passed" || v.Status == "failed" || v.Status == "unverified"
	}
	if apply && diff != "" {
		preview := fmt.Sprintf("   %s by %s:\n     %s", plural(files, "file changed", "files changed"), roleName+map[bool]string{true: ", check passed", false: ", check " + v.Status}[v.Status == "passed"], strings.Join(wt.changedFiles, "\n     "))
		// Every file the diff touches is checked against the edit rules; the
		// strictest decides (one denied file blocks the whole apply).
		pattern, act := "*", Allow
		for _, f := range wt.changedFiles {
			switch Evaluate("edit", permPath(s.jail(), f), s.rules()...) {
			case Deny:
				pattern, act = f, Deny
			case Ask:
				if act == Allow {
					pattern, act = f, Ask
				}
			}
			if act == Deny {
				break
			}
		}
		var msg string
		ok := act == Allow
		switch act {
		case Deny:
			msg = fmt.Sprintf("error: the diff touches %s, which a permission rule denies editing", pattern)
		case Ask:
			msg, ok = tc.Ask("edit", pattern, "APPLY diff from "+roleName, preview)
		}
		if !ok {
			v.Status = "not_applied"
			v.Tail = strings.TrimSpace(v.Tail + "\n" + msg)
		} else if err := wt.applyTo(s.jail().Root, diff); err != nil {
			v.Status = "conflict"
			why := "applying the diff to the caller's tree failed:\n" + err.Error()
			if !sameTree {
				why = fmt.Sprintf("applying %s's diff from %s to your tree on %s failed:\n%s\n%s",
					roleName, target.MemberName(), caller.MemberName(), err.Error(), wt.heads(roleName))
			}
			v.Tail = strings.TrimSpace(v.Tail + "\n\n" + why)
		} else {
			applied = true
			for _, f := range wt.changedFiles {
				s.noteRead(tc.Ctx, f) // the caller has "seen" what it just merged
			}
		}
	}
	callerMember := ""
	if !sameTree {
		callerMember = caller.MemberName()
	}
	child.traceTask(task, v, check, len(diff), files, applied, review, start, callerMember)
	detail := v.Tail
	if applied {
		detail = fmt.Sprintf("applied %s: %s\n\n%s", plural(files, "file", "files"), strings.Join(wt.changedFiles, ", "), v.Tail)
	}
	o.trackEnd(entry, v.Status, detail)
	child.saveTranscript()

	out := delegateResult{Status: v.Status, Diff: headTail(diff, maxDiffBytes), TestTail: v.Tail, Review: review}
	if v.Status == "error" && v.Err != nil && out.TestTail == "" {
		out.TestTail = v.Err.Error()
	}
	return marshalDelegate(out)
}

func marshalDelegate(r delegateResult) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

func orNone(s string) string {
	if s == "" {
		return "(none — result will be unverified)"
	}
	return s
}

func (o *Orchestrator) applyPolicy() string {
	if o.roles != nil && o.roles.Apply != "" {
		return o.roles.Apply
	}
	return "verified"
}

// ── git worktrees ───────────────────────────────────────────────────────────

type worktrees struct{ mu sync.Mutex } // git's worktree bookkeeping isn't concurrency-safe

type worktree struct {
	mgr *worktrees
	// mem is the machine this worktree lives on (nil = this one) and rem is that
	// machine's transport pointed AT the worktree. caller is the machine a passed
	// diff is applied to, with callerTop that machine's repository top: the two
	// are not the same question, because a build box's diff lands on the laptop.
	mem          *Member
	rem          *Remote
	caller       *Member
	callerTop    string
	top          string   // the worktree's repository top level (on mem)
	dir          string   // worktree directory
	root         string   // the subagent's jail root inside it (same subdir as the caller's)
	sub          string   // jail root relative to the repo top ("." = top)
	base         string   // snapshot commit
	changedFiles []string // relative to the jail root
}

// pathspec limits git to the caller's jail: a subagent's changes above its
// root (e.g. via make -C ..) never reach the caller's tree.
func (w *worktree) pathspec() []string {
	if w.sub == "." || w.sub == "" {
		return nil
	}
	return []string{"--", w.sub}
}

// pathspecSh is pathspec for a shell line on a member.
func (w *worktree) pathspecSh() string {
	if w.sub == "." || w.sub == "" {
		return ""
	}
	return " -- " + shellQuote(w.sub)
}

// normSub turns git's --show-prefix ("pkg/lex/", "" at the top) into the form
// worktree.sub holds.
func normSub(p string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	if p == "" || p == "." {
		return "."
	}
	return p
}

// layoutWord names where a project sits in its repository, for the refusal.
func layoutWord(sub string) string {
	if sub == "." || sub == "" {
		return "the top"
	}
	return sub
}

func gitCmd(dir string, env []string, stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=lca", "GIT_AUTHOR_EMAIL=lca@localhost",
		"GIT_COMMITTER_NAME=lca", "GIT_COMMITTER_EMAIL=lca@localhost", "GIT_TERMINAL_PROMPT=0")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// create snapshots the working tree under jailRoot — tracked changes and new
// non-ignored files, without touching the user's index or HEAD (a temporary
// index) — and checks the snapshot out as a detached worktree.
func (m *worktrees) create(jailRoot, lcaDir, session string) (*worktree, error) {
	top, err := gitCmd(jailRoot, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("delegate needs a git repository: %w", err)
	}
	top = strings.TrimSpace(top)
	if real, err := filepath.EvalSymlinks(top); err == nil {
		top = real
	}
	sub, err := filepath.Rel(top, jailRoot)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	idx, err := os.CreateTemp("", "lca-index-*")
	if err != nil {
		return nil, err
	}
	idx.Close()
	os.Remove(idx.Name()) // git wants to create it
	defer os.Remove(idx.Name())
	env := []string{"GIT_INDEX_FILE=" + idx.Name()}
	head, headErr := gitCmd(top, nil, nil, "rev-parse", "--verify", "-q", "HEAD")
	head = strings.TrimSpace(head)
	if headErr == nil && head != "" {
		if _, err := gitCmd(top, env, nil, "read-tree", head); err != nil {
			return nil, err
		}
	}
	if _, err := gitCmd(top, env, nil, "add", "-A"); err != nil {
		return nil, err
	}
	tree, err := gitCmd(top, env, nil, "write-tree")
	if err != nil {
		return nil, err
	}
	args := []string{"commit-tree", strings.TrimSpace(tree), "-m", "lca delegate snapshot"}
	if head != "" {
		args = append(args, "-p", head)
	}
	commit, err := gitCmd(top, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	commit = strings.TrimSpace(commit)

	base, err := filepath.Abs(filepath.Join(lcaDir, "worktrees"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(base, session+"-")
	if err != nil {
		return nil, err
	}
	os.Remove(dir) // worktree add wants to create it
	// no user hooks: post-checkout scripts don't belong in an agent's scratch copy
	if _, err := gitCmd(top, nil, nil, "-c", "core.hooksPath="+os.DevNull, "worktree", "add", "--detach", dir, commit); err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	w := &worktree{mgr: m, top: top, dir: dir, root: filepath.Join(dir, sub), sub: filepath.ToSlash(sub), base: commit}
	if info, err := os.Stat(w.root); err != nil || !info.IsDir() {
		w.removeLocked()
		return nil, fmt.Errorf("%s has no tracked or untracked non-ignored files, so it doesn't exist in the snapshot", sub)
	}
	return w, nil
}

// remoteWorktreeSentinel frames createOn's reply. It is read from the LAST line
// of stdout that starts with it, so a login shell's chatty rc file printing to
// stdout cannot be mistaken for our output.
const remoteWorktreeSentinel = "lca-member-ok"

// maxRemoteDiffBytes caps what a member may send back as a patch. A truncated
// patch cannot apply, so going over it is an error, never a silent cut.
const maxRemoteDiffBytes = 8 << 20

// createOn is create for a member: the same sequence, in one ssh round trip,
// because the far side has git and a shell and nothing of ours. Every reason is
// the local create's reason — a temporary GIT_INDEX_FILE so the operator's index
// and HEAD on that machine are untouched; add -A so uncommitted and new
// non-ignored files are in the snapshot; core.hooksPath=/dev/null because
// post-checkout scripts don't belong in an agent's scratch copy; the worktree
// under that machine's own ${LCA_DIR:-$HOME/.lca}/worktrees and never inside the
// project, because the next snapshot's `add -A` would swallow it (and /tmp may be
// small or noexec).
//
// What it deliberately is NOT: a snapshot of the CALLER's tree. Locally that is
// the whole safety property, but shipping the caller's dirty tree over ssh needs
// a shared ancestor (git bundle) or copies the project behind the operator's
// back (rsync). So the base is the MEMBER's working tree, and runDelegateTool
// says so out loud rather than papering over it.
//
// Two lca processes delegating onto one member can still race git's worktree
// bookkeeping there: worktrees.mu serialises one process only. A lock file on
// the member, taken in this same round trip, is the honest fix and is not in
// this pass.
func (m *worktrees) createOn(ctx context.Context, mem *Member, session string) (*worktree, error) {
	rem := mem.Rem
	if rem == nil {
		return nil, fmt.Errorf("member %s is this machine — use create", mem.MemberName())
	}
	if err := rem.ensureUp(ctx); err != nil {
		return nil, err
	}
	script := `set -e
command -v git >/dev/null 2>&1 || exit 97
top=$(git rev-parse --show-toplevel)
sub=$(git rev-parse --show-prefix)
st=${LCA_DIR:-$HOME/.lca}/worktrees
mkdir -p "$st"
wt=$(mktemp -d "$st/` + shellWord(session) + `-XXXXXX")
rm -rf "$wt"
idx="$wt.index"
cd "$top"
export GIT_INDEX_FILE="$idx" GIT_TERMINAL_PROMPT=0
export GIT_AUTHOR_NAME=lca GIT_AUTHOR_EMAIL=lca@localhost
export GIT_COMMITTER_NAME=lca GIT_COMMITTER_EMAIL=lca@localhost
head=$(git rev-parse --verify -q HEAD || true)
if [ -n "$head" ]; then git read-tree "$head"; fi
git add -A
tree=$(git write-tree)
if [ -n "$head" ]; then commit=$(git commit-tree "$tree" -p "$head" -m 'lca delegate snapshot')
else commit=$(git commit-tree "$tree" -m 'lca delegate snapshot'); fi
unset GIT_INDEX_FILE
rm -f "$idx"
git -c core.hooksPath=/dev/null worktree add --detach "$wt" "$commit" >/dev/null
[ -d "$wt/$sub" ] || { git -c core.hooksPath=/dev/null worktree remove --force "$wt" >/dev/null 2>&1 || rm -rf "$wt"; exit 96; }
printf '` + remoteWorktreeSentinel + `\t%s\t%s\t%s\t%s\n' "$top" "$sub" "$wt" "$commit"
`
	m.mu.Lock()
	defer m.mu.Unlock()
	out, errs, exit := rem.plumb(ctx, script, nil, 10*time.Minute)
	tail := strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 6, 600))
	switch {
	case exit == 0:
	case exit == 97:
		return nil, fmt.Errorf("member %s has no git at %s, and a delegation needs a worktree created there: install git on %s, or run this role on another member",
			mem.MemberName(), rem.Dir, rem.Where())
	case exit == 96:
		return nil, fmt.Errorf("%s has no tracked or untracked non-ignored files on member %s, so it doesn't exist in the snapshot", rem.Dir, mem.MemberName())
	case exit == 98:
		return nil, fmt.Errorf("member %s: %s does not exist on %s", mem.MemberName(), rem.Dir, rem.Where())
	case strings.Contains(errs, "not a git repository"):
		return nil, fmt.Errorf("member %s: %s is not a git repository, so a delegation there has nothing to snapshot — git init or clone the project there, or run this role on another member",
			mem.MemberName(), rem.Dir)
	default:
		return nil, fmt.Errorf("creating the worktree on %s failed: %s", rem.Where(), tail)
	}
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), remoteWorktreeSentinel) {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		return nil, fmt.Errorf("the worktree script on %s produced no result: %s — check that ssh %s prints nothing to stdout from a shell rc file",
			rem.Where(), orNone(tail), firstNonEmpty(rem.Host, rem.Where()))
	}
	f := strings.Split(line, "\t")
	if len(f) != 5 {
		return nil, fmt.Errorf("the worktree script on %s produced %q, which is not its reply", rem.Where(), truncate(line, 200))
	}
	top, sub, dir, commit := f[1], normSub(f[2]), f[3], f[4]
	w := &worktree{mgr: m, mem: mem, top: top, dir: dir, sub: sub, base: commit}
	w.root = dir
	if sub != "." {
		w.root = dir + "/" + sub
	}
	w.rem = rem.withDir(dir)
	return w, nil
}

// shellWord keeps only characters that are inert in a shell word. The session id
// lca generates has none others, and a script built from a constant plus this is
// one fewer thing to be wrong about later.
func shellWord(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "lca"
	}
	return b.String()
}

// callerLayout is where the caller's project sits inside its repository, on
// whatever machine that is. It is the other half of the layout guard.
func (o *Orchestrator) callerLayout(ctx context.Context, m *Member, root string) (top, sub string, err error) {
	if m.IsLocal() {
		t, err := gitCmd(root, nil, nil, "rev-parse", "--show-toplevel")
		if err != nil {
			return "", "", fmt.Errorf("the caller's tree is not a git repository: %w", err)
		}
		p, err := gitCmd(root, nil, nil, "rev-parse", "--show-prefix")
		if err != nil {
			return "", "", err
		}
		top = strings.TrimSpace(t)
		if real, e := filepath.EvalSymlinks(top); e == nil {
			top = real
		}
		return top, normSub(p), nil
	}
	out, errs, exit := m.Rem.plumb(ctx, "git rev-parse --show-toplevel && git rev-parse --show-prefix", nil, 2*time.Minute)
	if exit != 0 {
		return "", "", fmt.Errorf("member %s: %s is not a git repository, so a diff cannot be applied there: %s",
			m.MemberName(), m.Rem.Dir, strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 3, 300)))
	}
	// --show-prefix prints an empty line at the repository top, so a trimmed
	// reply may be one line.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	top, sub = strings.TrimSpace(lines[0]), "."
	if len(lines) > 1 {
		sub = normSub(lines[1])
	}
	return top, sub, nil
}

// diff returns the subagent's changes against the snapshot (new files
// included) and how many files changed.
func (w *worktree) diff() (string, int, error) {
	if w.rem != nil {
		return w.diffOn()
	}
	if _, err := gitCmd(w.dir, nil, nil, "add", "-A"); err != nil {
		return "", 0, err
	}
	names, err := gitCmd(w.dir, nil, nil, append([]string{"diff", "--cached", "--name-only", w.base}, w.pathspec()...)...)
	if err != nil {
		return "", 0, err
	}
	w.changedFiles = nil
	for _, n := range strings.Split(strings.TrimSpace(names), "\n") {
		if n != "" {
			if w.sub != "." && w.sub != "" {
				n = strings.TrimPrefix(n, w.sub+"/")
			}
			w.changedFiles = append(w.changedFiles, n)
		}
	}
	d, err := gitCmd(w.dir, nil, nil, append([]string{"diff", "--cached", "--binary", w.base}, w.pathspec()...)...)
	return d, len(w.changedFiles), err
}

// diffOn is diff for a worktree on a member: two plumb calls, deliberately not
// one framed script, because any sentinel byte can occur inside a text hunk.
//
// The two -c settings are on the REMOTE leg only — the local-to-local path keeps
// today's exact invocation because it never crosses machines — and they are what
// stop a patch made on one machine from failing to apply on another with
// different git defaults. What keeps the patch free of either machine's absolute
// paths is `diff --cached --binary <base>` itself, with its a/… b/… prefixes:
// paths are repository-relative, which is also why the layout guard exists.
//
// There is no ctx here because diff()'s signature is the one the caller already
// has; plumb's own timeout is the bound.
func (w *worktree) diffOn() (string, int, error) {
	ctx := context.Background()
	git := "git -c core.quotepath=false -c core.autocrlf=false "
	names, errs, exit := w.rem.plumb(ctx, "git add -A && "+git+"diff --cached --name-only "+shellQuote(w.base)+w.pathspecSh(), nil, 10*time.Minute)
	if exit != 0 {
		return "", 0, fmt.Errorf("listing the changed files on %s failed: %s", w.rem.Where(), strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), names), 4, 400)))
	}
	w.changedFiles = nil
	for _, n := range strings.Split(strings.TrimSpace(names), "\n") {
		if n == "" {
			continue
		}
		if w.sub != "." && w.sub != "" {
			n = strings.TrimPrefix(n, w.sub+"/")
		}
		w.changedFiles = append(w.changedFiles, n)
	}
	// The patch goes to a file on the member and is then capped from it, rather
	// than piped into `head -c`: a pipe would give us head's exit status, so a git
	// that failed halfway would come back as a short but successful diff — and a
	// short diff that applies is worse than one that does not.
	cmd := "d=$(mktemp) || exit 1\n" +
		git + "--no-pager diff --cached --binary --no-ext-diff --no-textconv " + shellQuote(w.base) + w.pathspecSh() + " > \"$d\"\n" +
		"rc=$?\nif [ $rc -eq 0 ]; then head -c " + strconv.Itoa(maxRemoteDiffBytes+1) + " \"$d\"; fi\nrm -f \"$d\"\nexit $rc\n"
	d, errs, exit := w.rem.plumb(ctx, cmd, nil, 10*time.Minute)
	if exit != 0 {
		return "", 0, fmt.Errorf("collecting the diff on %s failed: %s", w.rem.Where(), strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), d), 4, 400)))
	}
	switch {
	case len(d) > maxRemoteDiffBytes:
		return "", 0, fmt.Errorf("the diff from member %s is %d MiB or larger; it was NOT applied — a truncated patch cannot apply, and that change is too big for a delegation",
			w.mem.MemberName(), maxRemoteDiffBytes>>20)
	case d != "" && !strings.HasPrefix(d, "diff --git "):
		return "", 0, fmt.Errorf("member %s printed unexpected output before the diff — something on %s writes to stdout for non-interactive ssh (a shell rc file); guard it with [ -t 1 ], or the patch cannot be read",
			w.mem.MemberName(), w.rem.Where())
	}
	return d, len(w.changedFiles), nil
}

// applyTo applies the diff to the caller's working tree (not its index) — on
// whatever machine that tree is. jailRoot is unused (and was before members
// existed): the repository top is what git apply needs.
func (w *worktree) applyTo(jailRoot, diff string) error {
	w.mgr.mu.Lock()
	defer w.mgr.mu.Unlock()
	top := firstNonEmpty(w.callerTop, w.top)
	if w.caller != nil && !w.caller.IsLocal() {
		rem := w.caller.Rem.withDir(top)
		git := "git -c core.quotepath=false -c core.autocrlf=false apply"
		for _, args := range []string{" --check --binary -", " --binary -"} {
			out, errs, exit := rem.plumb(context.Background(), git+args, []byte(diff), 10*time.Minute)
			if exit != 0 {
				return fmt.Errorf("%s", strings.TrimSpace(lastLines(firstNonEmpty(strings.TrimSpace(errs), out), 6, 800)))
			}
		}
		return nil
	}
	if _, err := gitCmd(top, nil, []byte(diff), "apply", "--check", "--binary", "-"); err != nil {
		return err
	}
	_, err := gitCmd(top, nil, []byte(diff), "apply", "--binary", "-")
	return err
}

// heads names both checkouts and their commits when an apply across machines
// failed: the two share no object store and the patch is text, so there is no
// 3-way merge to fall back on and the operator needs to know what to line up.
func (w *worktree) heads(role string) string {
	short := func(m *Member, dir string) string {
		if m.IsLocal() {
			out, _ := gitCmd(dir, nil, nil, "rev-parse", "--short", "HEAD")
			return strings.TrimSpace(out)
		}
		out, _, _ := m.Rem.withDir(dir).plumb(context.Background(), "git rev-parse --short HEAD", nil, time.Minute)
		return strings.TrimSpace(out)
	}
	caller := w.caller
	return fmt.Sprintf("%s is at %s, %s is at %s — a patch only applies where its context matches. Put the two checkouts on the same commit, or run %s on member %s.",
		w.mem.Label(), short(w.mem, w.top), caller.Label(), short(caller, firstNonEmpty(w.callerTop, w.top)), role, caller.MemberName())
}

func (w *worktree) remove() {
	if os.Getenv("LCA_KEEP_WORKTREES") != "" {
		return
	}
	w.mgr.mu.Lock()
	defer w.mgr.mu.Unlock()
	w.removeLocked()
}

func (w *worktree) removeLocked() {
	if w.mem != nil && w.mem.Rem != nil {
		// No `worktree prune` on a member: prune is safe locally because
		// worktrees.mu serialises it, but across two lca processes it can prune a
		// worktree another has just added.
		w.mem.Rem.plumb(context.Background(), "git -C "+shellQuote(w.top)+" worktree remove --force "+shellQuote(w.dir)+
			" >/dev/null 2>&1 || rm -rf "+shellQuote(w.dir), nil, 5*time.Minute)
		return
	}
	if _, err := gitCmd(w.top, nil, nil, "worktree", "remove", "--force", w.dir); err != nil {
		os.RemoveAll(w.dir)
	}
	gitCmd(w.top, nil, nil, "worktree", "prune")
}
