package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Workflows: explore with agents, operate with programs. A hardened pipeline
// is a YAML file of steps run as a deterministic program — shell and git steps
// cost zero tokens, and a model is invoked only where judgement is actually
// bought (prompt/delegate steps). State is written after every step, so a
// crash, a Ctrl-C or a pause leaves a resumable run and never a half-step.
//
// Three things a reader would otherwise guess wrong:
//
//   - steps: is a MAPPING keyed by step name, not a list. parseYAMLish has no
//     list-of-maps: "- name: x" would become the string "name: x". File order
//     is execution order, and unique names are what the placeholders need.
//   - a step with no `when` RUNS. There is no hidden dependency graph: a later
//     step can only observe a failure because an earlier one said
//     on_fail: continue, and then the author must guard it with `when`.
//   - `run` steps are not approved. They were authored in a file, not chosen by
//     a model, so they are harness commands exactly like a check_cmd. The
//     sandbox allowlist, the GPU policy and the deny rules still apply, on a
//     remote project too.
//   - a `run:` or `check:` line is ONE argv, not a shell line, unless the team
//     sets sandbox: {shell: true} (or -unsafe): `a && b` would hand "&&" to a
//     as an argument, so bind refuses it and asks for two steps.
//
// An unquoted " #" is a comment anywhere in a scalar (stripComment), so a value
// that contains one must be quoted: run: "git commit -m fix #42".
const (
	stepRun      = "run"
	stepPrompt   = "prompt"
	stepDelegate = "delegate"

	stepOK      = "ok"
	stepFailed  = "failed"
	stepSkipped = "skipped"

	wfStateVersion = 1
)

// Workflow is one <name>.yaml: an ordered program whose shell steps cost no
// tokens and whose model steps are the only places judgement is bought.
type Workflow struct {
	Name    string
	Desc    string
	Path    string
	Sum     string            // sha256 of the file: a resume must not straddle an edit
	Role    string            // default role for prompt/delegate steps
	Vars    map[string]string // defaults from the file; -var wins
	Timeout time.Duration     // default per-step timeout (0 = none declared)
	Steps   []*WorkflowStep

	byName   map[string]*WorkflowStep
	varRefs  []string // every ${vars.k} the file names; resolved by bind
	attempts int      // verify_attempts, for the -dry-run budget
}

// WorkflowStep is one node: exactly one action, plus what verifies it and what
// makes it run at all.
type WorkflowStep struct {
	Name    string
	Index   int
	Kind    string // stepRun | stepPrompt | stepDelegate
	Cmd     string // Kind == stepRun
	Text    string // prompt text / delegate task
	Role    string // resolved by bind; "" on a run step
	Check   string
	Retries int
	Timeout time.Duration // resolved by bind; always > 0 for a run step
	OnFail  string        // "stop" | "continue"
	When    []whenClause  // nil = always
	Review  string        // delegate: "" (the role's own) | "on" | "off" | a role name
	Fork    string        // delegate: "" (the role's own) | "true" | "false"

	reviewer string // the role that reviews this step's diff, resolved by bind
}

// whenClause is one OR-branch: all of its terms must hold.
type whenClause struct{ terms []whenTerm }

// whenTerm compares two operands as strings. always=true is the literal
// `always`. A ref operand is resolved when the step is reached, never
// substituted into the condition text — a step's output must not be able to
// rewrite the condition that gates it.
type whenTerm struct {
	lhs, rhs whenOperand
	eq       bool // == when true, != when false
	always   bool
}

type whenOperand struct {
	ref     string // "vars.k" | "steps.x.f"; "" when this is a literal
	literal string
}

// stepOutcome is what one step produced, before it becomes state.
type stepOutcome struct {
	Status      string // stepOK | stepFailed | stepSkipped
	Out         string // capped: what ${steps.x.out} yields
	Diff        string // delegate only: the full diff
	Exit        int
	Check       string // the expanded check, for the trace
	CheckExit   *int   // what the check decided; nil when none ran
	Checked     bool   // a check ran and decided this
	Attempts    int
	Detail      string // why it failed: check tail, delegate status, sandbox refusal, "cancelled"
	Reviewer    string // delegate: the role that judged the diff
	Review      string // delegate: approve | reject | unreviewed
	Model       string
	Session     string      // the step's own session UID (prompt steps)
	TaskSession string      // delegate only: the subagent session its TaskRecord carries
	Usage       *traceUsage // non-nil only when a model ran
}

// StepState is one step as state.json and a resume see it.
type StepState struct {
	Name       string      `json:"name"`
	Index      int         `json:"index"`
	Kind       string      `json:"kind"`
	Role       string      `json:"role,omitempty"`
	Reviewer   string      `json:"reviewer,omitempty"`
	Review     string      `json:"review,omitempty"`
	Model      string      `json:"model,omitempty"`
	Session    string      `json:"session,omitempty"`
	Status     string      `json:"status"`
	Exit       int         `json:"exit"`
	Checked    bool        `json:"checked"`
	Attempts   int         `json:"attempts"`
	Out        string      `json:"out,omitempty"`
	DiffFile   string      `json:"diff_file,omitempty"` // steps/<name>.diff, relative to the run dir
	DiffBytes  int         `json:"diff_bytes,omitempty"`
	Detail     string      `json:"detail,omitempty"`
	Started    string      `json:"started"`
	Finished   string      `json:"finished"`
	DurationMs int64       `json:"duration_ms"`
	Usage      *traceUsage `json:"usage,omitempty"`
}

// WorkflowState is $LCA_DIR/runs/<runid>/state.json, rewritten after every step
// so a crash or a Ctrl-C leaves a resumable run and never a half-step.
type WorkflowState struct {
	Version  int               `json:"version"`
	Run      string            `json:"run"`
	Workflow string            `json:"workflow"`
	Path     string            `json:"path"`
	Sum      string            `json:"sum"`
	Root     string            `json:"root"`
	Sessions []string          `json:"sessions"` // one rec.id per process that worked on this run
	Traces   []string          `json:"traces"`   // the trace file each of those wrote
	Vars     map[string]string `json:"vars"`
	Lead     string            `json:"lead,omitempty"`    // the lead role the run bound to
	Tier     string            `json:"tier,omitempty"`    // the active tier the run bound to
	NSteps   int               `json:"n_steps,omitempty"` // how many steps the file had: Cursor alone cannot say whether work is left
	Started  string            `json:"started"`
	Updated  string            `json:"updated"`
	Cursor   int               `json:"cursor"` // index of the NEXT step to execute
	Status   string            `json:"status"` // running | ok | failed | paused | interrupted
	Steps    []StepState       `json:"steps"`
}

// StepRecord is one workflow step in the trace: what ran, what decided it,
// what it cost. Delegate steps also produce their normal TaskRecord.
type StepRecord struct {
	Type        string      `json:"type"` // "step"
	TS          string      `json:"ts"`
	RootSession string      `json:"root_session"`
	Session     string      `json:"session,omitempty"`
	Run         string      `json:"run"`
	Workflow    string      `json:"workflow"`
	Step        string      `json:"step"`
	Index       int         `json:"index"`
	Kind        string      `json:"kind"`                   // run | prompt | delegate
	TaskSession string      `json:"task_session,omitempty"` // delegate: the subagent session, joining this step to its TaskRecord
	Role        string      `json:"role,omitempty"`
	Reviewer    string      `json:"reviewer,omitempty"`
	Review      string      `json:"review,omitempty"`
	Model       string      `json:"model,omitempty"`
	Status      string      `json:"status"` // ok | failed | skipped
	Exit        int         `json:"exit"`
	Check       string      `json:"check,omitempty"`
	CheckExit   *int        `json:"check_exit,omitempty"` // what decided the step
	Checked     bool        `json:"checked"`
	Attempts    int         `json:"attempts"`
	DurationMs  int64       `json:"duration_ms"`
	Usage       *traceUsage `json:"usage,omitempty"` // nil when no model ran
	Detail      string      `json:"detail,omitempty"`
}

// ── parsing ─────────────────────────────────────────────────────────────────

var reStepName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ${vars.<k>} and ${steps.<name>.<field>}. Deliberately not expandTemplate
// (skills.go): that fills $1/$ARGUMENTS for slash commands and appends leftover
// arguments when a template has no placeholder — a typo there is silently
// harmless, and here it must stop the run before it spends a token.
var reWFRef = regexp.MustCompile(`\$\{(vars|steps)\.([A-Za-z0-9_-]+)(?:\.([A-Za-z0-9_]+))?\}`)

// reWFBareRef is the same reference without the braces, as `when` may spell it.
var reWFBareRef = regexp.MustCompile(`^(vars|steps)\.([A-Za-z0-9_-]+)(?:\.([A-Za-z0-9_]+))?$`)

var wfTopKeys = []string{"name", "description", "role", "timeout", "vars", "steps"}

var wfStepKeys = []string{"run", "prompt", "delegate", "role", "check", "retries", "timeout", "on_fail", "when", "review", "fork"}

// wfFields are the placeholder fields each kind of step produces.
var wfFields = map[string][]string{
	stepRun:      {"out", "status", "exit"},
	stepPrompt:   {"out", "status", "exit"},
	stepDelegate: {"out", "status", "exit", "diff", "review"},
}

func loadWorkflow(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".yaml"), ".yml")
	wf, err := parseWorkflow(name, path, string(data))
	if err != nil {
		return nil, err
	}
	wf.Sum = fmt.Sprintf("%x", sha256.Sum256(data))
	return wf, nil
}

// parseWorkflow checks syntax, shape and every reference a step names. It needs
// no orchestrator, so a typo costs nothing: nothing runs and no token is spent.
func parseWorkflow(name, path, text string) (*Workflow, error) {
	wf := &Workflow{Name: name, Path: path, Vars: map[string]string{}, byName: map[string]*WorkflowStep{}}
	base := filepath.Base(path)
	if base == "." || base == "/" {
		base = name
	}
	fail := func(format string, a ...any) (*Workflow, error) {
		return nil, fmt.Errorf("%s: %s", base, fmt.Sprintf(format, a...))
	}

	var stepsNode *yNode
	for _, n := range parseYAMLish(text).Children {
		if !contains(wfTopKeys, n.Key) {
			return fail("unknown key %q (want %s)", n.Key, strings.Join(wfTopKeys, ", "))
		}
		switch n.Key {
		case "name":
			if n.Value != "" {
				wf.Name = n.Value
			}
		case "description":
			wf.Desc = n.Value
		case "role":
			wf.Role = n.Value
		case "timeout":
			secs, err := strconv.Atoi(strings.TrimSpace(n.Value))
			if err != nil || secs < 1 {
				return fail("timeout: %q is not a positive number of seconds", n.Value)
			}
			wf.Timeout = time.Duration(secs) * time.Second
		case "vars":
			for _, v := range n.Children {
				wf.Vars[v.Key] = v.Value
			}
		case "steps":
			stepsNode = n
		}
	}
	if stepsNode == nil {
		return fail("no steps: a workflow is an ordered mapping of steps")
	}
	// parseYAMLish has no list-of-maps, so "- name: build" would arrive as the
	// string key "name: build" and every other key of that step would hang off
	// steps: itself — an error naming a step nobody wrote.
	if len(stepsNode.List) > 0 {
		return fail(`steps: is a mapping keyed by step name, not a list — write "build:" with "run: …" indented under it, not "- name: build"`)
	}
	if len(stepsNode.Children) == 0 {
		return fail("no steps: a workflow is an ordered mapping of steps")
	}

	for _, sn := range stepsNode.Children {
		if !reStepName.MatchString(sn.Key) {
			return fail("step %s: bad name (want lower-case letters, digits, - and _)", sn.Key)
		}
		if wf.byName[sn.Key] != nil {
			return fail("step %s: duplicate step name", sn.Key)
		}
		s := &WorkflowStep{Name: sn.Key, Index: len(wf.Steps), OnFail: "stop"}
		var actions []string
		for _, k := range sn.Children {
			if !contains(wfStepKeys, k.Key) {
				return fail("step %s: unknown key %q (want %s)", sn.Key, k.Key, strings.Join(wfStepKeys, ", "))
			}
			switch k.Key {
			case stepRun:
				s.Kind, s.Cmd = stepRun, k.Value
				actions = append(actions, stepRun)
			case stepPrompt:
				s.Kind, s.Text = stepPrompt, k.Value
				actions = append(actions, stepPrompt)
			case stepDelegate:
				s.Kind, s.Text = stepDelegate, k.Value
				actions = append(actions, stepDelegate)
			case "role":
				s.Role = k.Value
			case "check":
				s.Check = k.Value
			case "retries":
				n, err := strconv.Atoi(strings.TrimSpace(k.Value))
				if err != nil || n < 0 {
					return fail("step %s: retries: %q is not a whole number >= 0", sn.Key, k.Value)
				}
				s.Retries = n
			case "timeout":
				n, err := strconv.Atoi(strings.TrimSpace(k.Value))
				if err != nil || n < 1 {
					return fail("step %s: timeout: %q is not a positive number of seconds (omit it for the default)", sn.Key, k.Value)
				}
				s.Timeout = time.Duration(n) * time.Second
			case "on_fail":
				if k.Value != "stop" && k.Value != "continue" {
					return fail("step %s: on_fail: %q is not stop or continue", sn.Key, k.Value)
				}
				s.OnFail = k.Value
			case "when":
				w, err := parseWhen(k.Value)
				if err != nil {
					return fail("step %s: when: %v", sn.Key, err)
				}
				s.When = w
			case "review":
				switch strings.ToLower(strings.TrimSpace(k.Value)) {
				case "":
					return fail("step %s: review: is empty (want a role name, or off)", sn.Key)
				case "off", "false", "no", "none":
					s.Review = "off"
				case "on", "true", "yes":
					s.Review = "on"
				default:
					s.Review = strings.TrimSpace(k.Value)
				}
			case "fork":
				switch strings.ToLower(strings.TrimSpace(k.Value)) {
				case "true", "yes", "on":
					s.Fork = "true"
				case "false", "no", "off":
					s.Fork = "false"
				default:
					return fail("step %s: fork: %q is not true or false", sn.Key, k.Value)
				}
			}
		}
		switch len(actions) {
		case 1:
		case 0:
			return fail("step %s: no action (want one of run, prompt, delegate)", sn.Key)
		default:
			return fail("step %s: %s are two actions — a step is exactly one", sn.Key, strings.Join(actions, " and "))
		}
		if s.Kind == stepRun && s.Role != "" {
			return fail("step %s: role: on a run step — a shell step costs no tokens and has no role", sn.Key)
		}
		if s.Review != "" && s.Kind != stepDelegate {
			return fail("step %s: review: on a %s step — only a delegate step's diff is reviewed", sn.Key, s.Kind)
		}
		if s.Fork != "" && s.Kind != stepDelegate {
			return fail("step %s: fork: on a %s step — only a delegate step inherits the caller's context", sn.Key, s.Kind)
		}
		if strings.TrimSpace(s.Cmd+s.Text) == "" {
			return fail("step %s: %s: is empty", sn.Key, s.Kind)
		}
		wf.Steps = append(wf.Steps, s)
		wf.byName[s.Name] = s
	}

	// Every reference, before anything runs: a typo in ${steps.buidl.out} must
	// not surface at step 9 of a pipeline that has already spent an hour.
	for _, s := range wf.Steps {
		for _, text := range []string{s.Cmd, s.Check} {
			if span := quotedStepRef(text); span != "" {
				return fail("step %s: %s sits inside quotes — the runner already quotes a step's output, and your quotes would let that output close them and start a second command; drop them", s.Name, span)
			}
		}
		for _, text := range []string{s.Cmd, s.Text, s.Check} {
			refs, err := scanRefs(text)
			if err != nil {
				return fail("step %s: %v", s.Name, err)
			}
			for _, ref := range refs {
				if err := wf.checkRef(s, ref); err != nil {
					return fail("step %s: %v", s.Name, err)
				}
			}
		}
		for _, c := range s.When {
			for _, t := range c.terms {
				for _, op := range []whenOperand{t.lhs, t.rhs} {
					if op.ref == "" {
						continue
					}
					if err := wf.checkRef(s, op.ref); err != nil {
						return fail("step %s: when: %v", s.Name, err)
					}
				}
			}
		}
	}
	return wf, nil
}

// checkRef validates one reference against the step that uses it: a var is
// remembered for bind, a step reference must name an earlier step and a field
// that step's kind produces.
func (wf *Workflow) checkRef(user *WorkflowStep, ref string) error {
	kind, rest, _ := strings.Cut(ref, ".")
	if kind == "vars" {
		wf.varRefs = append(wf.varRefs, ref)
		return nil
	}
	name, field, ok := strings.Cut(rest, ".")
	if !ok {
		return fmt.Errorf("${steps.%s} needs a field: out, status, exit, diff or review", rest)
	}
	target := wf.byName[name]
	if target == nil {
		return fmt.Errorf("${steps.%s.%s}: no step named %s", name, field, name)
	}
	if target.Index >= user.Index {
		return fmt.Errorf("${steps.%s.%s}: %s does not run before %s", name, field, name, user.Name)
	}
	if !contains(wfFields[target.Kind], field) {
		return fmt.Errorf("${steps.%s.%s}: a %s step has no %s (want %s)", name, field, target.Kind, field, strings.Join(wfFields[target.Kind], ", "))
	}
	return nil
}

// scanRefs returns every reference in text and rejects anything else spelled
// ${…}: a malformed placeholder is a mistake, not a literal.
func scanRefs(text string) ([]string, error) {
	var refs []string
	for i := 0; i+1 < len(text); i++ {
		if text[i] != '$' || text[i+1] != '{' {
			continue
		}
		end := strings.IndexByte(text[i:], '}')
		if end < 0 {
			return nil, fmt.Errorf("unterminated placeholder %q", truncate(text[i:], 40))
		}
		span := text[i : i+end+1]
		if reWFRef.FindString(span) != span {
			return nil, fmt.Errorf("bad placeholder %s (want ${vars.<k>} or ${steps.<name>.<field>})", span)
		}
		refs = append(refs, refOf(reWFRef.FindStringSubmatch(span)))
		i += end
	}
	return refs, nil
}

// quotedStepRef returns the first ${steps.…} placeholder that sits inside a
// quoted region of a command line, or "". expand shell-quotes an untrusted step
// output, which is only safe in an unquoted position: with the author's own
// quotes around it, `run: echo '${steps.x.out}'` and the output
// `x; git reset --hard origin/main` would close the quote and run a command
// nobody wrote. Refusing at load is the only enforcement there can be, since
// the value does not exist yet.
func quotedStepRef(cmd string) string {
	var q byte
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; {
		case q == 0 && (c == '\'' || c == '"'):
			q = c
		case q != 0 && c == q:
			q = 0
		case q != 0 && c == '$' && strings.HasPrefix(cmd[i:], "${steps."):
			if end := strings.IndexByte(cmd[i:], '}'); end > 0 {
				return cmd[i : i+end+1]
			}
			return cmd[i:]
		}
	}
	return ""
}

// shellOperatorOutsideQuotes finds an operator that only a shell would act on.
// Without sandbox: {shell: true} the line is one argv, so `go build && go test`
// runs go with "&&" as an argument and can only ever fail.
func shellOperatorOutsideQuotes(cmd string) string {
	var q byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case q == 0 && (c == '\'' || c == '"'):
			q = c
		case q != 0 && c == q:
			q = 0
		case q != 0:
		case strings.HasPrefix(cmd[i:], "&&"):
			return "&&"
		case strings.HasPrefix(cmd[i:], "||"):
			return "||"
		case c == '|' || c == ';' || c == '>' || c == '<':
			return string(c)
		}
	}
	return ""
}

func refOf(m []string) string {
	ref := m[1] + "." + m[2]
	if m[3] != "" {
		ref += "." + m[3]
	}
	return ref
}

// parseWhen builds the condition tree: an OR of ANDs of string comparisons.
// Deliberately not an expression language, and parsed here rather than
// substituted at run time — a step's output containing && must not be able to
// rewrite the condition that gates the next step.
func parseWhen(expr string) ([]whenClause, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil
	}
	var out []whenClause
	for _, or := range strings.Split(expr, "||") {
		var c whenClause
		for _, and := range strings.Split(or, "&&") {
			t, err := parseWhenTerm(strings.TrimSpace(and))
			if err != nil {
				return nil, err
			}
			c.terms = append(c.terms, t)
		}
		out = append(out, c)
	}
	return out, nil
}

func parseWhenTerm(t string) (whenTerm, error) {
	if t == "always" {
		return whenTerm{always: true}, nil
	}
	eq, at := true, strings.Index(t, "==")
	if i := strings.Index(t, "!="); i >= 0 && (at < 0 || i < at) {
		eq, at = false, i
	}
	if at < 0 {
		return whenTerm{}, fmt.Errorf("%q is not `always` or a comparison (== or !=)", t)
	}
	lhs, err := parseWhenOperand(strings.TrimSpace(t[:at]))
	if err != nil {
		return whenTerm{}, err
	}
	rhs, err := parseWhenOperand(strings.TrimSpace(t[at+2:]))
	if err != nil {
		return whenTerm{}, err
	}
	return whenTerm{lhs: lhs, rhs: rhs, eq: eq}, nil
}

func parseWhenOperand(s string) (whenOperand, error) {
	if s == "" {
		return whenOperand{}, fmt.Errorf("a comparison needs a value on both sides")
	}
	if strings.HasPrefix(s, "${") {
		refs, err := scanRefs(s)
		if err != nil {
			return whenOperand{}, err
		}
		if len(refs) != 1 || reWFRef.FindString(s) != s {
			return whenOperand{}, fmt.Errorf("%q is not a single reference", s)
		}
		return whenOperand{ref: refs[0]}, nil
	}
	if strings.HasPrefix(s, "vars.") || strings.HasPrefix(s, "steps.") {
		m := reWFBareRef.FindStringSubmatch(s)
		if m == nil {
			return whenOperand{}, fmt.Errorf("%q is not a reference (want vars.<k> or steps.<name>.<field>)", s)
		}
		return whenOperand{ref: refOf(m)}, nil
	}
	// A dotted bare word is a misspelt reference, not a literal: `step.build.status`
	// would compare two literals, hold never, and skip the step on every run
	// without a word of diagnostic.
	if strings.Contains(s, ".") && unquote(s) == s {
		return whenOperand{}, fmt.Errorf("%q looks like a misspelt reference (did you mean ${steps.<name>.status}?) — quote it if you really meant the literal", s)
	}
	return whenOperand{literal: unquote(s)}, nil
}

// ── binding ─────────────────────────────────────────────────────────────────

// effectiveVars are the file's defaults with -var on top.
func (wf *Workflow) effectiveVars(cli map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range wf.Vars {
		out[k] = v
	}
	for k, v := range cli {
		out[k] = v
	}
	return out
}

// bind resolves each step's role and timeout against the team and rejects what
// could only ever fail — still before the first token is spent.
func (wf *Workflow) bind(o *Orchestrator, lead string, vars map[string]string) error {
	base := filepath.Base(wf.Path)
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%s: %s", base, fmt.Sprintf(format, a...))
	}
	for _, k := range sortedKeys(wf.Vars) {
		if strings.TrimSpace(vars[k]) == "" {
			return fail("vars: %s is declared without a value — pass it with -var %s=…", k, k)
		}
	}
	used := map[string]bool{}
	for _, ref := range wf.varRefs {
		k := strings.TrimPrefix(ref, "vars.")
		used[k] = true
		if _, ok := vars[k]; !ok {
			return fail("${vars.%s} is neither declared in vars: nor passed with -var", k)
		}
	}
	// The other direction, or a typo'd -var does nothing and the run goes green
	// with the operator believing they changed it.
	for _, k := range sortedKeys(vars) {
		if _, declared := wf.Vars[k]; declared || used[k] {
			continue
		}
		return fail("-var %s=… is not a variable this workflow uses (it uses: %s)", k, orNone(strings.Join(sortedKeys(wf.Vars), ", ")))
	}
	delegates := false
	for _, s := range wf.Steps {
		// Ask the sandbox now. A command it would refuse, or a shell line this
		// team cannot run, must not surface at step 9 of a pipeline that has
		// already spent an hour; only a command built from a step's output
		// (unknown until then) is left to run time.
		for _, cmd := range []string{s.Cmd, s.Check} {
			if cmd == "" || strings.Contains(cmd, "${steps.") {
				continue
			}
			line, err := substVars(cmd, vars)
			if err != nil {
				return fail("step %s: %v", s.Name, err)
			}
			if !o.jl.Shell && !o.jl.Unsafe {
				if op := shellOperatorOutsideQuotes(line); op != "" {
					return fail("step %s: this team runs one argv per command (no sandbox.shell), so %q is passed to the command as an argument — use two steps, or set sandbox: {shell: true} in roles.yaml", s.Name, op)
				}
			}
			if err := o.jl.CheckCommand(line); err != nil {
				return fail("step %s: %v", s.Name, err)
			}
		}
		if s.Kind != stepRun {
			s.Role = firstNonEmpty(s.Role, wf.Role, lead)
			if ag := o.agents[s.Role]; ag == nil || !ag.IsRole {
				return fail("step %s: no role %q in roles.yaml", s.Name, s.Role)
			}
		}
		if s.Timeout == 0 {
			s.Timeout = wf.Timeout
		}
		// A run step needs a bound: context.WithTimeout(ctx, 0) fires at once.
		// A model step gets one only when the author asked for it — inheriting
		// the verifier's check timeout would kill a long delegation mid-work.
		if s.Kind == stepRun && s.Timeout == 0 {
			s.Timeout = o.checkTimeout()
		}
		if s.Kind != stepDelegate {
			continue
		}
		delegates = true
		if s.Role == lead {
			return fail("step %s: a delegate step's role must differ from the lead role %q", s.Name, lead)
		}
		// Without a check runDelegateTool returns "unverified", and
		// apply: verified never applies the diff — the step could only fail.
		if s.Check == "" && o.agents[s.Role].CheckCmd == "" {
			return fail("step %s: a delegate step needs check: or a role with check_cmd, or its diff is never applied", s.Name)
		}
		switch s.Review {
		case "off":
			s.reviewer = ""
		case "", "on":
			if rev := o.reviewerFor(o.agents[s.Role]); rev != nil {
				s.reviewer = rev.Name
			}
			if s.Review == "on" && s.reviewer == "" {
				return fail("step %s: review: on, but role %s names no review: and there is no defaults.review", s.Name, s.Role)
			}
		default:
			ag := o.agents[s.Review]
			if ag == nil || !ag.IsRole {
				return fail("step %s: review: no role %q in roles.yaml", s.Name, s.Review)
			}
			if s.Review == s.Role {
				return fail("step %s: review: %s is also the step's role — a role cannot review itself", s.Name, s.Review)
			}
			s.reviewer = s.Review
		}
	}
	if delegates {
		if o.remote != nil {
			return fail("delegate steps need local git worktrees; this team is configured for %s — use run/prompt steps instead", o.remote.Where())
		}
		if _, err := gitCmd(o.jl.Root, nil, nil, "rev-parse", "--show-toplevel"); err != nil {
			return fail("delegate steps need a git repository (%s is not one)", o.jl.Root)
		}
	}
	wf.attempts = o.verifyAttempts()
	return nil
}

// substVars fills in ${vars.*} only, for a bind-time sandbox check: a var comes
// from the file or the command line, so its value is known before anything runs.
func substVars(text string, vars map[string]string) (string, error) {
	var bad error
	out := reWFRef.ReplaceAllStringFunc(text, func(span string) string {
		m := reWFRef.FindStringSubmatch(span)
		if m[1] != "vars" {
			return span
		}
		v, ok := vars[m[2]]
		if !ok && bad == nil {
			bad = fmt.Errorf("unknown variable ${vars.%s}", m[2])
		}
		return v
	})
	return out, bad
}

// plan renders the bound workflow for -dry-run. A delegate step's two budgets
// multiply (retries around runDelegateTool, verify_attempts inside it), so the
// effective number is spelled out rather than left to be discovered.
func (wf *Workflow) plan() [][]string {
	rows := [][]string{}
	for i, s := range wf.Steps {
		attempts := strconv.Itoa(s.Retries + 1)
		if s.Kind == stepDelegate && wf.attempts > 1 {
			attempts = fmt.Sprintf("%d × %d = %d", s.Retries+1, wf.attempts, (s.Retries+1)*wf.attempts)
		}
		to := "—"
		if s.Timeout > 0 {
			to = s.Timeout.String() // a per-step bound is minutes, not milliseconds
		}
		when := "always"
		if len(s.When) > 0 {
			when = "conditional"
		}
		role := orDash(s.Role)
		if s.reviewer != "" {
			role += " → " + s.reviewer
		}
		rows = append(rows, []string{strconv.Itoa(i + 1), s.Name, s.Kind, role, attempts, to, when, orDash(truncate(firstLine(s.Check), 40))})
	}
	return rows
}

// orTierNone names the absent tier: no tier active means every role runs the
// chain it declares, which is a state a message has to be able to say.
func orTierNone(s string) string {
	if s == "" {
		return "(no)"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ── discovery ───────────────────────────────────────────────────────────────

// workflowDirs is searched in order and the FIRST match wins, so a project
// workflow shadows a personal one of the same name. That is the reverse of
// loadRoles, which reads $LCA_DIR first and lets later files win per field: a
// workflow is a whole program, not a merged field set, and the repo should win.
func workflowDirs(cfg Config) []string {
	dirs := []string{filepath.Join(cfg.Root, ".lca", "workflows"), filepath.Join(cfg.Dir, "workflows")}
	for _, d := range filepath.SplitList(os.Getenv("LCA_WORKFLOWS")) {
		if strings.TrimSpace(d) != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func findWorkflow(cfg Config, name string) (*Workflow, error) {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("%q is not a workflow name", name)
	}
	for _, dir := range workflowDirs(cfg) {
		for _, ext := range []string{".yaml", ".yml"} {
			p := filepath.Join(dir, name+ext)
			if _, err := os.Stat(p); err == nil {
				return loadWorkflow(p)
			}
		}
	}
	return nil, fmt.Errorf("no workflow %q in %s", name, strings.Join(workflowDirs(cfg), ", "))
}

// listWorkflows returns what the search path holds, deduped by name. A file
// that does not parse becomes a warning: one bad workflow must not hide the rest.
func listWorkflows(cfg Config) ([]*Workflow, []string) {
	var found []*Workflow
	var warns []string
	seen := map[string]bool{}
	for _, dir := range workflowDirs(cfg) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !(strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
				continue
			}
			name := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".yaml"), ".yml")
			if seen[name] {
				continue
			}
			seen[name] = true
			wf, err := loadWorkflow(filepath.Join(dir, e.Name()))
			if err != nil {
				warns = append(warns, err.Error())
				continue
			}
			found = append(found, wf)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	sort.Strings(warns)
	return found, warns
}

// ── run directory ───────────────────────────────────────────────────────────

func runsDir(cfg Config) string { return filepath.Join(cfg.stateDir(), "runs") }

func newRunID(workflow, recID string) string { return workflow + "-" + recID }

// reRunID matches what newRunID produces, so one bare argument can be either a
// workflow name or a run id without an -id flag.
var reRunID = regexp.MustCompile(`^.+-\d{8}-\d{6}-\d+$`)

func loadRunState(dir string) (*WorkflowState, error) {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	st := &WorkflowState{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "state.json"), err)
	}
	if st.Version != wfStateVersion {
		return nil, fmt.Errorf("%s: state version %d, this build writes %d", dir, st.Version, wfStateVersion)
	}
	return st, nil
}

// saveRunState writes through a temp file in the same directory and renames it:
// a crash mid-write must not leave a half-parsed state, which is the whole
// point of writing state after every step.
func saveRunState(dir string, st *WorkflowState) error {
	st.Updated = nowTS()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// Per process: if a lock bug ever let two processes share a run, two writers
	// of one fixed temp path would publish a mixture of both documents as
	// state.json, which is the corruption temp+rename exists to prevent.
	tmp := filepath.Join(dir, fmt.Sprintf("state.json.%d.tmp", os.Getpid()))
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "state.json")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// listRuns returns recorded runs, newest first. limit <= 0 means all of them. A
// state that will not parse becomes a warning, like listWorkflows': dropping it
// silently would hide a run whose directory, log and diffs are all on disk.
func listRuns(cfg Config, limit int) ([]*WorkflowState, []string) {
	ents, err := os.ReadDir(runsDir(cfg))
	if err != nil {
		return nil, nil
	}
	var out []*WorkflowState
	var warns []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		st, err := loadRunState(filepath.Join(runsDir(cfg), e.Name()))
		if err != nil {
			warns = append(warns, err.Error())
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	sort.Strings(warns)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, warns
}

// findRun locates the run to resume: that run id, or the newest unfinished run
// of this workflow in this project. runs/ is shared by every project on the
// machine, so a candidate from another root is not this user's work; and a run
// whose cursor is past its last step has nothing left to do, so picking it would
// hide the run that has.
func findRun(cfg Config, workflow, runid, root string) (string, *WorkflowState, error) {
	if runid != "" {
		dir := filepath.Join(runsDir(cfg), runid)
		st, err := loadRunState(dir)
		if err != nil {
			return "", nil, fmt.Errorf("no run %q: %w", runid, err)
		}
		return dir, st, nil
	}
	runs, warns := listRuns(cfg, 0)
	finished := ""
	for _, st := range runs {
		if st.Workflow != workflow || st.Status == stepOK {
			continue
		}
		if st.Root != "" && root != "" && st.Root != root {
			continue // another project's run of a workflow with the same name
		}
		if st.NSteps > 0 && st.Cursor >= st.NSteps && st.Status != "paused" {
			if finished == "" {
				finished = st.Run
			}
			continue
		}
		return filepath.Join(runsDir(cfg), st.Run), st, nil
	}
	if len(warns) > 0 {
		return "", nil, fmt.Errorf("a run is on disk but unreadable, so it cannot be resumed: %s", warns[0])
	}
	if finished != "" {
		return "", nil, fmt.Errorf("no unfinished run of %q to resume: %s reached its last step (lca run -list shows it)", workflow, finished)
	}
	return "", nil, fmt.Errorf("no unfinished run of %q to resume", workflow)
}

// pauseRun asks a running pipeline to stop at the next step boundary. It is
// cooperative on purpose: no signal is sent to a run that may be mid-test.
func pauseRun(cfg Config, runid string) error {
	if !reRunID.MatchString(runid) {
		return fmt.Errorf("%q is not a run id — -pause takes one, not a workflow name (lca run -list shows them)", runid)
	}
	dir := filepath.Join(runsDir(cfg), runid)
	if _, err := loadRunState(dir); err != nil {
		return fmt.Errorf("no run %q: %w", runid, err)
	}
	return os.WriteFile(filepath.Join(dir, "pause"), []byte(nowTS()+"\n"), 0o600)
}

// pruneRuns drops the oldest finished runs. An unfinished run is someone's
// resumable work, not garbage, so it is never deleted.
func pruneRuns(cfg Config, keep int) {
	runs, _ := listRuns(cfg, 0)
	n := len(runs)
	for i := len(runs) - 1; i >= 0 && n > keep; i-- {
		if runs[i].Status != stepOK {
			continue
		}
		if os.RemoveAll(filepath.Join(runsDir(cfg), runs[i].Run)) == nil {
			n--
		}
	}
}

// lockRun keeps two processes off one run directory. Both would execute every
// remaining step — a delegation's diff applied twice, a `git commit` step
// committing twice — and both would rewrite state.json, so the loser's write
// drops the winner's step records. A lock whose pid is gone is a crash, not a
// conflict: it is taken over, because a crashed run must stay resumable.
func lockRun(dir string) (func(), error) {
	path := filepath.Join(dir, "lock")
	id := filepath.Base(dir)
	for try := 0; try < 2; try++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		b, rerr := os.ReadFile(path)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if rerr == nil && pid > 0 && pidAlive(pid) {
			return nil, fmt.Errorf("run %s is still going (pid %d) — `lca run %s -pause` stops it at its next step boundary", id, pid, id)
		}
		warnLine("taking over run %s: its process (pid %d) is gone", id, pid)
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("run %s: another process is opening it right now", id)
}

// pidAlive is deliberately coarse: os.FindProcess plus a zero signal is what the
// standard library offers on every platform this ships to, and a wrong "alive"
// only costs a refused resume with the pid in the message.
func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return !errors.Is(proc.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

// ── run.log ─────────────────────────────────────────────────────────────────

// runLog is run.log: the raw combined output of every step, appended as it is
// produced, so a run that crashed mid-check is still diagnosable. Writes are
// whole chunks under a mutex and ANSI-stripped — a subagent view can write
// while a command streams, and a log full of escape sequences is not greppable.
type runLog struct {
	mu sync.Mutex
	f  *os.File
}

func newRunLog(dir string) (*runLog, error) {
	f, err := os.OpenFile(filepath.Join(dir, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &runLog{f: f}, nil
}

func (l *runLog) Write(b []byte) (int, error) {
	if l == nil || l.f == nil {
		return len(b), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.WriteString(stripANSI(string(b)))
	return len(b), nil
}

// line appends one whole line.
func (l *runLog) line(text string) {
	l.Write([]byte(strings.TrimRight(text, "\n") + "\n"))
}

func (l *runLog) header(format string, a ...any) {
	l.line(fmt.Sprintf(format, a...))
}

func (l *runLog) Close() {
	if l != nil && l.f != nil {
		l.f.Close()
	}
}

// workflowView tees a model step's activity into run.log. Embedding View means
// only the methods that must also reach the log are written here. Delegate steps
// keep their own childView (newChild assigns it and there is no hook to redirect
// a subagent's output), so for them the log holds the step header, the
// subagent's verifier runs (Session.checkLive) and the returned
// {status, test_tail} — not the subagent's own tool lines.
type workflowView struct {
	View
	log  *runLog
	step string
}

// Stream tees the reply into run.log as it arrives, so a step killed during a
// long model call still leaves what the model said. Reasoning stays out: it is
// not the step's output and it can dwarf everything else in the log.
func (v *workflowView) Stream() StreamView {
	return &wfStream{inner: v.View.Stream(), log: v.log, step: v.step}
}

type wfStream struct {
	inner StreamView
	log   *runLog
	step  string
	any   bool
}

func (s *wfStream) Start(model string) { s.inner.Start(model) }

func (s *wfStream) Sink() StreamSink {
	sink := s.inner.Sink()
	content := sink.Content
	sink.Content = func(delta string) {
		if !s.any {
			s.log.line(s.step + " says:")
			s.any = true
		}
		s.log.Write([]byte(delta))
		if content != nil {
			content(delta)
		}
	}
	discard := sink.Discard
	sink.Discard = func() {
		// The turn is being repeated, so what is in the log is void: two replies
		// with nothing between them would read as one.
		if s.any {
			s.log.line("\n(that reply was discarded and the turn repeated)")
			s.any = false
		}
		if discard != nil {
			discard()
		}
	}
	return sink
}

func (s *wfStream) End() []string {
	if s.any {
		s.log.line("")
		s.any = false
	}
	return s.inner.End()
}

func (v *workflowView) Live() io.Writer {
	if inner := v.View.Live(); inner != nil {
		return io.MultiWriter(inner, v.log)
	}
	return v.log
}

// The step's name goes on every teed line: a subagent can write while a command
// streams, and run.log is read long after the step header has scrolled past.
func (v *workflowView) Note(text string) {
	v.log.line(v.step + ": " + text)
	v.View.Note(text)
}

func (v *workflowView) Warn(text string) {
	v.log.line(v.step + ": warning: " + text)
	v.View.Warn(text)
}

func (v *workflowView) Error(text string) {
	v.log.line(v.step + ": error: " + text)
	v.View.Error(text)
}

func (v *workflowView) ToolStart(name, summary string) {
	v.log.line(v.step + ": " + toolVerb(name) + " " + summary)
	v.View.ToolStart(name, summary)
}

func (v *workflowView) ToolDone(name string, args Args, result string) {
	if strings.HasPrefix(result, "error:") {
		v.log.line(v.step + ": " + firstLine(result))
	}
	v.View.ToolDone(name, args, result)
}

func (v *workflowView) Check(cmd string, exit int, d time.Duration, attempt, attempts int) {
	v.log.line(checkText(cmd, exit, d, attempt, attempts))
	v.View.Check(cmd, exit, d, attempt, attempts)
}

// ── the runner ──────────────────────────────────────────────────────────────

// wfRunner executes a workflow. It owns the run directory and is the only
// writer of state.json and run.log. Executing a step and recording it are kept
// apart so that the day steps run in parallel, only Run changes.
type wfRunner struct {
	wf      *Workflow
	orch    *Orchestrator
	lead    *Session // the run's root session: rootUID for the trace, jail, caller for delegates
	dir     string   // $LCA_DIR/runs/<runid>
	log     *runLog
	st      *WorkflowState
	res     map[string]*StepState // by name, including steps restored from a resume
	unlock  func()                // releases the run directory's lock
	forkSrc *Session              // the newest prompt step's session: what `fork: true` inherits
	stop    atomic.Bool           // Ctrl-C: stop at the next step boundary
	signals bool                  // own SIGINT (the CLI); the REPL passes an interruptible ctx instead
}

func newRunner(o *Orchestrator, lead *Session, wf *Workflow, dir string, st *WorkflowState) (*wfRunner, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockRun(dir)
	if err != nil {
		return nil, err
	}
	lg, err := newRunLog(dir)
	if err != nil {
		unlock()
		return nil, err
	}
	r := &wfRunner{wf: wf, orch: o, lead: lead, dir: dir, log: lg, st: st, res: map[string]*StepState{}, unlock: unlock}
	// Written on a resume too: a state from before this field existed cannot say
	// whether the cursor has work left.
	st.NSteps = len(wf.Steps)
	for i := range st.Steps {
		ss := st.Steps[i]
		r.res[ss.Name] = &ss
	}
	// State exists from the moment the run does, so `lca run -list` and -pause
	// can see a run that has not reached its first step yet.
	if err := saveRunState(dir, st); err != nil {
		lg.Close()
		unlock()
		return nil, err
	}
	return r, nil
}

func (r *wfRunner) Close() {
	r.log.Close()
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
}

func (r *wfRunner) paused() bool {
	_, err := os.Stat(filepath.Join(r.dir, "pause"))
	return err == nil
}

func (r *wfRunner) exitStatus() int {
	for _, ss := range r.st.Steps {
		if ss.Status == stepFailed {
			return 1
		}
	}
	if r.st.Status == "interrupted" {
		return 1
	}
	return 0
}

func (r *wfRunner) save() {
	if err := saveRunState(r.dir, r.st); err != nil {
		errLine("state: %v", err)
	}
}

// Run executes from state.Cursor and returns the exit status. Three SIGINT
// stages: the first stops at the next step boundary (fully resumable), the
// second cancels the step in flight — stage one alone would leave a long
// delegation unkillable without SIGKILL, which loses the state write — and the
// third hands the signal back to the runtime, so a step that honours neither
// (a model call wedged on a half-open socket) is still escapable.
func (r *wfRunner) Run(ctx context.Context) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if r.signals {
		sigch := make(chan os.Signal, 1)
		signal.Notify(sigch, os.Interrupt)
		defer signal.Stop(sigch)
		done := make(chan struct{})
		defer close(done)
		go func() {
			n := 0
			for {
				select {
				case <-sigch:
					n++
					switch n {
					case 1:
						r.stop.Store(true)
						warnLine("stopping after this step — Ctrl-C again to abort it")
					case 2:
						cancel()
						warnLine("aborting the step in flight — Ctrl-C again to kill the process")
					default:
						// The state written so far is the resumable one; the step
						// in flight is not honouring cancellation, so stop
						// intercepting and let SIGINT do what it normally does.
						signal.Stop(sigch)
						errLine("%s did not stop; resume with: lca run %s -resume", r.wf.Name, r.st.Run)
						if pr, err := os.FindProcess(os.Getpid()); err == nil {
							pr.Signal(os.Interrupt)
						}
						return
					}
				case <-done:
					return
				}
			}
		}()
	}

	section("run", r.wf.Name+" · "+r.st.Run)
	r.st.Status = "running"
	r.save()

	halted := ""
	for i := r.st.Cursor; i < len(r.wf.Steps); i++ {
		s := r.wf.Steps[i]
		if r.paused() {
			r.st.Cursor, r.st.Status, halted = i, "paused", "paused"
			r.log.header("== paused before %s ==", s.Name)
			r.save()
			hint("paused before %s · resume: lca run %s -resume", s.Name, r.st.Run)
			break
		}
		if r.stop.Load() || ctx.Err() != nil {
			r.st.Cursor, r.st.Status, halted = i, "interrupted", "interrupted"
			r.log.header("== interrupted before %s ==", s.Name)
			r.save()
			break
		}

		start := time.Now()
		ready, werr := r.ready(s)
		if werr == nil && !ready {
			skipped := stepOutcome{Status: stepSkipped}
			ss := r.record(s, &skipped, start, 0)
			r.announce(i, s, "")
			fmt.Println("    " + stepStatusWord(stepSkipped) + "  " + faint("condition not met"))
			r.log.header("== %d/%d %s (%s) skipped ==", i+1, len(r.wf.Steps), s.Name, s.Kind)
			r.traceStep(s, ss, skipped)
			r.st.Cursor = i + 1
			r.save()
			continue
		}

		// Announced by the step itself, once its placeholders are expanded: a
		// line showing ${steps.plan.out} says nothing about what actually ran.
		announce := func(detail string) {
			r.announce(i, s, detail)
			r.log.header("== %d/%d %s (%s%s) %s ==", i+1, len(r.wf.Steps), s.Name, s.Kind, roleSuffix(s), nowTS())
		}
		out := stepOutcome{Status: stepFailed, Exit: -1, Attempts: 1}
		if werr != nil {
			// Every reference was validated at load, so this is a real failure
			// (a deleted diff file, say) — a failed step, never a silent skip.
			announce(r.headline(s, s.Cmd))
			out.Detail = "when: " + werr.Error()
			r.log.line(out.Detail)
		} else {
			out = r.runStep(ctx, s, announce)
		}
		d := time.Since(start)
		ss := r.record(s, &out, start, d)
		r.traceStep(s, ss, out)
		r.log.header("-- %s, exit %d, %s --", out.Status, out.Exit, fmtDurShort(d))
		r.report(s, out, d)

		switch {
		case out.Status != stepFailed:
			r.st.Cursor = i + 1
		case ctx.Err() != nil:
			// Cancelled mid-step: record it (the log and trace show something
			// ran) but leave the cursor on it, so a resume re-runs it whole.
			r.st.Status, halted = "interrupted", "interrupted"
		case s.OnFail == "continue":
			r.st.Cursor = i + 1
		default:
			r.st.Status, halted = stepFailed, stepFailed
		}
		r.save()
		if halted != "" {
			break
		}
	}
	if halted == "" {
		r.st.Status = stepOK
		if r.exitStatus() != 0 {
			r.st.Status = stepFailed
		}
		r.save()
	}
	r.summary()
	return r.exitStatus()
}

func roleSuffix(s *WorkflowStep) string {
	if s.Role == "" {
		return ""
	}
	return " " + s.Role
}

// headline is what the step's line shows after its kind: the command as it will
// run, the check, or the role.
func (r *wfRunner) headline(s *WorkflowStep, cmd string) string {
	switch s.Kind {
	case stepRun:
		return truncate(firstLine(cmd), 60)
	case stepDelegate:
		if s.Check != "" {
			return s.Role + "  " + faint("check: %s", truncate(firstLine(s.Check), 40))
		}
		return s.Role
	default:
		return s.Role
	}
}

func (r *wfRunner) announce(i int, s *WorkflowStep, detail string) {
	fmt.Printf("  %s%s%s %s  %s %s %s\n", cYellow, "▸", cReset,
		faint("%d/%d", i+1, len(r.wf.Steps)), padTo(s.Name, 12, 0), faint("%-8s", s.Kind), detail)
}

func (r *wfRunner) report(s *WorkflowStep, o stepOutcome, d time.Duration) {
	line := "    " + stepStatusWord(o.Status) + "  "
	var bits []string
	if o.Status == stepFailed {
		bits = append(bits, fmt.Sprintf("exit %d", o.Exit))
	}
	// Only a model step can be unverified; a shell step's exit code IS the verdict.
	if o.Status == stepOK && !o.Checked && s.Kind == stepPrompt {
		bits = append(bits, "unverified")
	}
	bits = append(bits, fmtDurShort(d))
	if o.Attempts > 1 {
		bits = append(bits, plural(o.Attempts, "attempt", "attempts"))
	}
	if det := firstLine(o.Detail); o.Status == stepFailed && det != "" {
		bits = append(bits, truncate(det, 60))
	}
	fmt.Println(line + faint("%s", strings.Join(bits, " · ")))
}

// stepStatusWord is not statusWord (repl.go): that vocabulary is
// passed/completed, so it would print "ok" in red.
func stepStatusWord(status string) string {
	switch status {
	case stepOK:
		return statusText(cGreen, gUp, stepOK)
	case stepSkipped:
		return statusText(cFaint, gNone, stepSkipped)
	default:
		return statusText(cRed, gDown, status)
	}
}

func (r *wfRunner) summary() {
	section("steps")
	rows := [][]string{}
	ok, failed, skipped := 0, 0, 0
	for _, ss := range r.st.Steps {
		switch ss.Status {
		case stepOK:
			ok++
		case stepSkipped:
			skipped++
		default:
			failed++
		}
		rows = append(rows, []string{ss.Name, ss.Kind, orDash(ss.Role), stepStatusWord(ss.Status),
			fmtDurShort(time.Duration(ss.DurationMs) * time.Millisecond), strconv.Itoa(ss.Attempts), truncate(firstLine(ss.Detail), 40)})
	}
	table([]string{"step", "kind", "role", "status", "time", "tries", "detail"}, rows)
	tally := fmt.Sprintf("%d/%d ok", ok, len(r.wf.Steps))
	if failed > 0 {
		tally += fmt.Sprintf(" · %d failed", failed)
	}
	if skipped > 0 {
		tally += fmt.Sprintf(" · %d skipped", skipped)
	}
	if failed > 0 || r.st.Status == "interrupted" {
		errLine("%s", tally)
	} else {
		okLine("%s", tally)
	}
	if r.st.Cursor < len(r.wf.Steps) {
		hint("resume: lca run %s -resume", r.st.Run)
	}
	hint("log: %s", shortDir(filepath.Join(r.dir, "run.log")))
}

// ── one step ────────────────────────────────────────────────────────────────

// runStep expands the step's placeholders, announces what that produced and
// executes it. timeout bounds ONE attempt of a run or delegate step; on a prompt
// step the retry loop is RunVerified's, which has no per-attempt hook, so there
// it bounds the turn and its retries together.
func (r *wfRunner) runStep(ctx context.Context, s *WorkflowStep, announce func(string)) stepOutcome {
	if s.Kind != stepRun {
		announce(r.headline(s, ""))
	}
	fail := func(err error) stepOutcome {
		if s.Kind == stepRun {
			announce(r.headline(s, s.Cmd))
		}
		return stepOutcome{Status: stepFailed, Exit: -1, Detail: err.Error(), Attempts: 1}
	}
	check, err := r.expand(s.Check, true)
	if err != nil {
		return fail(err)
	}
	switch s.Kind {
	case stepRun:
		cmd, err := r.expand(s.Cmd, true)
		if err != nil {
			return fail(err)
		}
		announce(r.headline(s, cmd))
		return r.shellSteps(ctx, s, cmd, check)
	case stepPrompt:
		text, err := r.expand(s.Text, false)
		if err != nil {
			return fail(err)
		}
		return r.promptStep(ctx, s, text, check)
	default:
		text, err := r.expand(s.Text, false)
		if err != nil {
			return fail(err)
		}
		return r.delegateStep(ctx, s, text, check)
	}
}

// shellStep runs one command line in the sandbox — the same branch
// RunVerifiedAll takes, so a remote project works too. A command the sandbox
// refuses is a failed step whose Detail is the refusal, never a silent skip.
func (r *wfRunner) shellStep(ctx context.Context, s *WorkflowStep, cmd string, timeout time.Duration) (string, int) {
	if rem := r.lead.remote(); rem != nil {
		// Remote.run consults no policy of its own, so the allowlist and the GPU
		// rules are applied here — the same line must not be refused locally and
		// waved through on a remote host.
		if err := r.lead.jail().CheckCommand(cmd); err != nil {
			return "sandbox: " + err.Error(), -1
		}
		return rem.run(ctx, cmd, timeout, nil, r.log)
	}
	return execCheck(ctx, r.lead.jail(), cmd, timeout, r.log)
}

// shellSteps is the action plus its check, retried as a unit. The step is ok
// only when BOTH exited 0: letting the check override the action's exit would
// pass `run: go build ./...` with `check: grep -q ok out.txt` while the build
// is broken.
func (r *wfRunner) shellSteps(ctx context.Context, s *WorkflowStep, cmd, check string) stepOutcome {
	o := stepOutcome{Check: check}
	attempts := s.Retries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		o.Attempts = attempt
		if attempts > 1 {
			r.log.header("-- attempt %d/%d --", attempt, attempts)
		}
		r.log.header("$ %s", cmd)
		out, exit := r.shellStep(ctx, s, cmd, s.Timeout)
		if exit == -1 {
			// execCheck words a timeout for a verifier, and this leg is the
			// step's own action — the summary table is the one place an author
			// reads it.
			out = strings.Replace(out, "(check timed out after", "(the command timed out after", 1)
		}
		o.Out, o.Exit, o.Detail, o.Checked, o.CheckExit = lastLines(strings.TrimSpace(out), tailLines, tailBytes), exit, "", false, nil
		if exit == -1 && strings.HasPrefix(out, "sandbox: ") {
			r.log.line(out) // refused before it ran, so nothing streamed
		}
		if exit != 0 {
			o.Detail = o.Out
		} else if check != "" {
			r.log.header("$ %s", check)
			cout, cexit := r.shellStep(ctx, s, check, s.Timeout)
			o.Checked, o.CheckExit = true, &cexit
			if cexit != 0 {
				o.Detail = fmt.Sprintf("check `%s` failed (exit %d)\n%s", check, cexit, lastLines(strings.TrimSpace(cout), tailLines, tailBytes))
			}
		}
		o.Status = stepOK
		if o.Exit != 0 || (o.CheckExit != nil && *o.CheckExit != 0) {
			o.Status = stepFailed
		}
		if o.Status == stepOK {
			return o
		}
		if ctx.Err() != nil {
			o.Detail = "cancelled"
			return o
		}
	}
	return o
}

// promptStep is one agent turn in the caller's tree, in a FRESH child session.
// newChild, not NewPrimary: NewPrimary hands every session UID = rec.id, so
// several prompt steps would collide in the trace. And a fresh child per step,
// not one reused session per role: reuse would let step 5 see step 2's
// conversation, grow context without bound over a long pipeline and trigger a
// compaction mid-run. Steps communicate through declared placeholders.
//
// The description is one constant for every step, not s.Name: newChild puts it
// in the system prompt ("You are the %q subagent … for one task: %s"), so a step
// name there would move bytes near the FRONT of the prefix and cost the
// gateway's KV cache the whole system message on every step after the first. The
// step's name is in run.log, the trace and state.json instead.
func (r *wfRunner) promptStep(ctx context.Context, s *WorkflowStep, text, check string) stepOutcome {
	o := stepOutcome{Check: check, Attempts: 1}
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	child, err := r.orch.newChild(r.lead, r.orch.agents[s.Role], "one step of a workflow")
	if err != nil {
		o.Status, o.Exit, o.Detail = stepFailed, -1, err.Error()
		return o
	}
	defer r.orch.forgetChild(child.ID) // a workflow must not leak children
	// Under `lca run` the run's lead reads nothing — every model step happens in
	// a child like this one — so the reads a later `fork: true` can inherit are
	// the ones a prompt step made. The session outlives forgetChild; only its
	// resumability by task_id goes away.
	r.forkSrc = child
	child.view = &workflowView{View: child.view, log: r.log, step: s.Name}
	child.checkLive = r.log
	child.Msgs = append(child.Msgs, Message{Role: "user", Content: text})
	o.Model, o.Session = child.client.Model(), child.UID

	child.view.Begin()
	start := time.Now()
	// RunVerifiedAll already implements the retry contract for model steps: a
	// failing check goes back as a user message and the session runs again.
	v := child.RunVerified(ctx, check, s.Retries+1)
	child.view.Finish(v.Status, time.Since(start))
	child.saveTranscript()

	o.Attempts, o.Checked = v.Attempts, v.Checked
	o.Usage = &traceUsage{child.stats.PromptTokens, child.stats.OutputTokens, child.stats.CachedTokens}
	if v.Checked {
		exit := v.Exit
		o.CheckExit = &exit
	}
	if t := lastAssistantText(child); t != "" {
		o.Out = lastLines(t, tailLines, tailBytes)
	}
	switch v.Status {
	case "passed":
		o.Status = stepOK
	case "unverified":
		// A prompt step with no check is a text-producing node whose output a
		// later step's check decides on; failing it would fail every plan.
		o.Status = stepOK
	case "cancelled":
		o.Status, o.Exit, o.Detail = stepFailed, -1, "cancelled"
	case "failed":
		o.Status, o.Exit, o.Detail = stepFailed, v.Exit, v.Tail
	default:
		o.Status, o.Exit, o.Detail = stepFailed, -1, v.Tail
	}
	return o
}

// delegateStep hands the step to the existing tool: own worktree, verifier,
// apply: verified, TaskRecord, the live subagent tree — all unchanged. Only
// `passed` applied a diff, so only `passed` moved the pipeline forward.
// retries wrap runDelegateTool (which has no attempts parameter of its own), so
// the two budgets multiply; -dry-run prints the effective number.
//
// Model and Usage stay empty: the delegation's cost is in its own turn records
// under this root session, not in anything the tool hands back, and TaskSession
// is the key that joins this step's record to the TaskRecord holding them.
func (r *wfRunner) delegateStep(ctx context.Context, s *WorkflowStep, text, check string) stepOutcome {
	o := stepOutcome{Check: check}
	task := text
	attempts := s.Retries + 1
	// The subagent's verifier streams into run.log for as long as this step runs:
	// a delegation is the longest thing a workflow does, and a crash twenty
	// minutes in must not leave one header line behind. Restored afterwards,
	// because on the REPL path the lead is the user's own session.
	prevLive := r.lead.checkLive
	r.lead.checkLive = r.log
	defer func() { r.lead.checkLive = prevLive }()
	for attempt := 1; attempt <= attempts; attempt++ {
		o.Attempts = attempt
		if attempts > 1 {
			r.log.header("-- attempt %d/%d --", attempt, attempts)
		}
		actx, cancel := ctx, context.CancelFunc(func() {})
		if s.Timeout > 0 {
			actx, cancel = context.WithTimeout(ctx, s.Timeout)
		}
		tc := &ToolCtx{Ctx: actx, S: r.lead, Name: "delegate", Reviewer: s.reviewer, ForkFrom: r.forkSrc}
		args := Args{"role": s.Role, "task": task, "check_cmd": check, "review": s.reviewer != ""}
		if s.Fork != "" {
			args["fork"] = s.Fork == "true"
		}
		raw := runDelegateTool(tc, args)
		cancel()
		o.TaskSession = tc.TaskSession
		o.Review, o.Reviewer = "", "" // a later attempt's verdict replaces an earlier one
		var dr delegateResult
		if err := json.Unmarshal([]byte(raw), &dr); err != nil {
			o.Status, o.Exit, o.Detail = stepFailed, -1, strings.TrimSpace(raw)
			r.log.line(o.Detail)
			return o
		}
		o.Out, o.Diff = lastLines(dr.TestTail, tailLines, tailBytes), dr.Diff
		o.Detail = "delegate status " + dr.Status
		if dr.Review != nil {
			// run.log has to say why a passing check still failed the step.
			o.Review, o.Reviewer = dr.Review.Verdict, dr.Review.Role
			o.Detail += " (reviewer " + dr.Review.Role + ": " + dr.Review.Verdict + ")"
		}
		if dr.Status == "passed" && dr.Diff == "" {
			// The verifier passed on an unchanged tree. Not a failure by itself
			// (a step may exist to confirm something), but it must be visible:
			// a later ${steps.x.diff} would otherwise review nothing.
			o.Detail += " (no diff: nothing changed)"
		}
		r.log.line(o.Detail)
		r.log.line(o.Out)
		o.Checked = dr.Status == "passed" || dr.Status == "failed"
		if o.Checked {
			exit := map[bool]int{true: 0, false: 1}[dr.Status == "passed"]
			o.CheckExit = &exit
		}
		if dr.Status == "passed" {
			o.Status, o.Exit = stepOK, 0
			return o
		}
		o.Status, o.Exit = stepFailed, 1
		if ctx.Err() != nil {
			o.Detail = "cancelled"
			return o
		}
		if actx.Err() != nil {
			o.Detail = fmt.Sprintf("the delegation timed out after %s", s.Timeout)
			r.log.line(o.Detail)
		}
		if attempt < attempts {
			// The check passing and the reviewer blocking are different problems,
			// and a retry told to fix a test failure that did not happen is a
			// wasted attempt.
			why := "did not pass the verifier. Last lines of its output:"
			if dr.Status == "rejected" && dr.Review != nil {
				why = "passed the check, but " + dr.Review.Role + " rejected it. Its reasons:"
			}
			task = text + "\n\n---\nA previous attempt " + why + "\n```\n" + o.Out + "\n```"
		}
	}
	return o
}

// ── placeholders ────────────────────────────────────────────────────────────

// expand substitutes references immediately before the step runs, so a step
// sees the real outputs of the steps before it.
//
// quote=true (a run or check command): a ${steps.x.*} value is model or command
// output, i.e. untrusted, so it is shellQuote'd — with shell: true the quotes
// stop `; rm -rf ~` becoming a second segment, and without a shell tokenize
// keeps it one argv word. A ${vars.k} goes in verbatim: it comes from the file
// or the command line, the same trust as roles.yaml allow:, and a var like
// "-count=1 ./..." must stay several words. The result still goes through
// Jail.CheckCommand.
func (r *wfRunner) expand(text string, quote bool) (string, error) {
	if text == "" {
		return "", nil
	}
	var bad error
	out := reWFRef.ReplaceAllStringFunc(text, func(span string) string {
		m := reWFRef.FindStringSubmatch(span)
		v, err := r.lookup(refOf(m))
		if err != nil {
			if bad == nil {
				bad = err
			}
			return span
		}
		if quote && m[1] == "steps" {
			return shellQuote(v)
		}
		return v
	})
	if bad != nil {
		return "", bad
	}
	return out, nil
}

func (r *wfRunner) lookup(ref string) (string, error) {
	kind, rest, _ := strings.Cut(ref, ".")
	switch kind {
	case "vars":
		v, ok := r.st.Vars[rest]
		if !ok {
			return "", fmt.Errorf("unknown variable ${vars.%s}", rest)
		}
		return v, nil
	case "steps":
		name, field, ok := strings.Cut(rest, ".")
		if !ok {
			return "", fmt.Errorf("${steps.%s} needs a field: out, status, exit, diff or review", rest)
		}
		ss := r.res[name]
		if ss == nil {
			return "", fmt.Errorf("step %s has not run yet", name)
		}
		switch field {
		case "status":
			return ss.Status, nil
		case "exit":
			return strconv.Itoa(ss.Exit), nil
		case "out":
			// A FAILED step's output is meaningful — handing it to a repair step
			// is why on_fail: continue exists. A skipped step has none.
			if ss.Status == stepSkipped {
				return "", fmt.Errorf("step %s was skipped, so ${steps.%s.out} has nothing in it", name, name)
			}
			return ss.Out, nil
		case "review":
			// An empty string is the honest answer for a step nothing reviewed,
			// so `when: ${steps.x.review} != reject` works on every delegate step.
			return ss.Review, nil
		case "diff":
			if ss.DiffFile == "" {
				return "", fmt.Errorf("step %s (%s) stored no diff, so ${steps.%s.diff} would hand this step nothing", name, ss.Status, name)
			}
			// The full diff is on disk, not in state.json: handing a reviewer a
			// truncated diff would be worse than handing it none.
			b, err := os.ReadFile(filepath.Join(r.dir, ss.DiffFile))
			if err != nil {
				return "", fmt.Errorf("step %s: reading its diff: %w", name, err)
			}
			return string(b), nil
		}
		return "", fmt.Errorf("step %s has no field %q", name, field)
	}
	return "", fmt.Errorf("unknown reference ${%s}", ref)
}

// ready evaluates the step's `when`. No condition means the step runs.
func (r *wfRunner) ready(s *WorkflowStep) (bool, error) {
	if len(s.When) == 0 {
		return true, nil
	}
	for _, c := range s.When {
		all := true
		for _, t := range c.terms {
			ok, err := r.term(t)
			if err != nil {
				return false, err
			}
			if !ok {
				all = false
				break
			}
		}
		if all {
			return true, nil
		}
	}
	return false, nil
}

func (r *wfRunner) term(t whenTerm) (bool, error) {
	if t.always {
		return true, nil
	}
	lhs, err := r.operand(t.lhs)
	if err != nil {
		return false, err
	}
	rhs, err := r.operand(t.rhs)
	if err != nil {
		return false, err
	}
	return (strings.TrimSpace(lhs) == strings.TrimSpace(rhs)) == t.eq, nil
}

func (r *wfRunner) operand(op whenOperand) (string, error) {
	if op.ref == "" {
		return op.literal, nil
	}
	return r.lookup(op.ref)
}

// ── recording ───────────────────────────────────────────────────────────────

func (r *wfRunner) record(s *WorkflowStep, o *stepOutcome, start time.Time, d time.Duration) *StepState {
	rel := ""
	if o.Diff != "" {
		var err error
		if rel, err = r.storeDiff(s, o.Diff); err != nil {
			// The diff is nowhere else (state.json deliberately does not hold
			// it) and a later ${steps.x.diff} depends on this file, so a step
			// whose diff was lost is not ok.
			o.Status, o.Exit, o.Detail = stepFailed, 1, "could not store the diff: "+err.Error()
			r.log.line(o.Detail)
			rel = ""
		}
	}
	ss := &StepState{Name: s.Name, Index: s.Index, Kind: s.Kind, Role: s.Role, Reviewer: o.Reviewer, Review: o.Review, Model: o.Model, Session: o.Session,
		Status: o.Status, Exit: o.Exit, Checked: o.Checked, Attempts: o.Attempts, Out: o.Out,
		Detail: truncate(o.Detail, 2000), Started: traceTS(start),
		Finished: nowTS(), DurationMs: d.Milliseconds(), Usage: o.Usage}
	if rel != "" {
		ss.DiffFile, ss.DiffBytes = rel, len(o.Diff)
	}
	r.res[s.Name] = ss
	for i := range r.st.Steps {
		if r.st.Steps[i].Name == s.Name {
			r.st.Steps[i] = *ss // a resumed step replaces its earlier record
			return ss
		}
	}
	r.st.Steps = append(r.st.Steps, *ss)
	return ss
}

// storeDiff keeps a delegation's full diff on disk under the run.
func (r *wfRunner) storeDiff(s *WorkflowStep, diff string) (string, error) {
	rel := filepath.ToSlash(filepath.Join("steps", s.Name+".diff"))
	if err := os.MkdirAll(filepath.Join(r.dir, "steps"), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(r.dir, rel), []byte(diff), 0o600); err != nil {
		return "", err
	}
	return rel, nil
}

func (r *wfRunner) traceStep(s *WorkflowStep, ss *StepState, o stepOutcome) {
	rec := StepRecord{Type: "step", TS: nowTS(), RootSession: r.lead.rootUID(), Session: ss.Session,
		Run: r.st.Run, Workflow: r.wf.Name, Step: ss.Name, Index: ss.Index, Kind: ss.Kind, TaskSession: o.TaskSession, Role: ss.Role,
		Reviewer: ss.Reviewer, Review: ss.Review,
		Model: ss.Model, Status: ss.Status, Exit: ss.Exit, Check: o.Check, CheckExit: o.CheckExit,
		Checked: ss.Checked, Attempts: ss.Attempts, DurationMs: ss.DurationMs, Usage: ss.Usage,
		Detail: truncate(firstLine(ss.Detail), 300)}
	r.orch.tracer.write(rec)
}

// ── CLI ─────────────────────────────────────────────────────────────────────

type kvFlag map[string]string

func (k kvFlag) String() string {
	var out []string
	for _, key := range sortedKeys(k) {
		out = append(out, key+"="+k[key])
	}
	return strings.Join(out, ",")
}

func (k kvFlag) Set(s string) error {
	key, val, ok := strings.Cut(s, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return fmt.Errorf("want k=v, got %q", s)
	}
	k[key] = val
	return nil
}

// parseVarWords reads `k=v` pairs from a REPL argument line, where there are no
// shell quotes to lean on: a word with no `=` belongs to the value before it, so
// `/run harden task=fix the parser` passes the whole phrase.
func parseVarWords(rest string) (kvFlag, error) {
	vars := kvFlag{}
	last := ""
	for _, w := range strings.Fields(rest) {
		if key, _, ok := strings.Cut(w, "="); ok && key != "" && !strings.Contains(key, " ") {
			if err := vars.Set(w); err != nil {
				return nil, err
			}
			last = key
			continue
		}
		if last == "" {
			return nil, fmt.Errorf("want k=v, got %q", w)
		}
		vars[last] += " " + w
	}
	return vars, nil
}

// splitLeadingName takes a bare leading argument off the front, so flags may
// follow the name (flag.Parse stops at the first non-flag).
func splitLeadingName(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func runUsage() {
	fmt.Fprintln(os.Stderr, strings.Join([]string{
		"usage: lca run [<name>|<runid>] [-var k=v]… [-resume] [-pause] [-dry-run] [-list] [-ask] [-role r] [-tier t]",
		"  lca run                      list the workflows found",
		"  lca run <name>               run it",
		"  lca run <name> -dry-run      validate and print the plan",
		"  lca run <name> -resume       continue its newest unfinished run",
		"  lca run <name> -resume <runid>  continue that run",
		"  lca run <runid> -pause       stop it at the next step boundary",
		"",
		" FLAGS",
		"   -role <name>                override the workflow's default role",
		"   -tier <name>                run every tier-declaring role on that chain (roles.yaml tiers:)",
		"   -var k=v                    set a declared workflow variable (repeatable)",
		"   -ask                        prompt for approvals instead of trusting the file",
	}, "\n"))
}

func runWorkflow(cfg Config, args []string) int {
	bare, rest := splitLeadingName(args)
	fset := flag.NewFlagSet("run", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	fset.Usage = runUsage
	vars := kvFlag{}
	fset.Var(vars, "var", "workflow variable, k=v (repeatable)")
	resume := fset.Bool("resume", false, "continue an unfinished run")
	pause := fset.Bool("pause", false, "ask a run to stop at its next step boundary")
	dry := fset.Bool("dry-run", false, "validate and print the plan, run nothing")
	list := fset.Bool("list", false, "list recent runs")
	ask := fset.Bool("ask", false, "prompt for approvals instead of trusting the file")
	roleFlag := fset.String("role", "", "override the workflow's default role")
	tierFlag := fset.String("tier", "", "run this team on that tier (roles.yaml tiers:)")
	if err := fset.Parse(rest); err != nil {
		return 2
	}
	if bare == "" && fset.NArg() > 0 {
		bare, _ = splitLeadingName(fset.Args())
	}
	// The pinned spelling `lca run <name> -resume <runid>` leaves the run id in
	// Args(), which every other shape would call a second workflow name.
	second := ""
	if *resume && bare != "" && fset.NArg() == 1 && fset.Arg(0) != bare {
		if reRunID.MatchString(bare) {
			errLine("%s and %s are both run ids — name one", bare, fset.Arg(0))
			runUsage()
			return 2
		}
		if !reRunID.MatchString(fset.Arg(0)) {
			errLine("%q is not a run id (lca run -list shows them)", fset.Arg(0))
			runUsage()
			return 2
		}
		second = fset.Arg(0)
	} else if fset.NArg() > 1 || (bare != "" && fset.NArg() == 1 && fset.Arg(0) != bare) {
		errLine("`lca run` takes one workflow name or run id, and %q came after it", fset.Arg(fset.NArg()-1))
		runUsage()
		return 2
	}

	if *list {
		printRuns(cfg)
		return 0
	}
	if *pause {
		if bare == "" {
			errLine("-pause needs the run id (lca run -list shows them)")
			return 2
		}
		if err := pauseRun(cfg, bare); err != nil {
			errLine("%v", err)
			return 2
		}
		okLine("pause requested — %s stops at its next step boundary", bare)
		return 0
	}
	if bare == "" && !*resume {
		printWorkflows(cfg)
		return 0
	}

	// One bare argument, told apart by shape: a run id or a workflow name.
	name, runid := bare, second
	if reRunID.MatchString(bare) {
		if !*resume {
			errLine("%s is a run id — did you mean `lca run %s -resume`?", bare, bare)
			return 2
		}
		name, runid = "", bare
	}
	if *resume && bare == "" {
		errLine("-resume needs the workflow name or a run id")
		hint("lca run -list shows the runs")
		return 2
	}
	if *resume && *roleFlag != "" {
		// Steps 1..n already ran with the recorded team; finishing the rest with
		// another one would report a single green result over two teams.
		errLine("-role cannot be combined with -resume — start a fresh run to change the team")
		return 2
	}
	cfg.Tier = firstNonEmpty(*tierFlag, cfg.Tier)

	root, _ := realRoot(cfg.Root)
	var dir string
	var st *WorkflowState
	var err error
	if *resume {
		if dir, st, err = findRun(cfg, name, runid, root); err != nil {
			errLine("%v", err)
			return 2
		}
		name = st.Workflow
		if cfg.Tier == "" {
			// A bare -resume re-establishes the tier the run recorded: the resume
			// line the run itself printed has to work, and knowing that LCA_TIER
			// stands in for the flag is not something to require. A -tier naming
			// a DIFFERENT tier still trips the mismatch guard below.
			cfg.Tier = st.Tier
		}
	}

	// A resume reloads the very file the run started from, not whatever the
	// search path resolves that name to today.
	var wf *Workflow
	if st != nil {
		wf, err = loadWorkflow(st.Path)
	} else {
		wf, err = findWorkflow(cfg, name)
	}
	if err != nil {
		errLine("%v", err)
		return 2
	}
	if *roleFlag != "" {
		wf.Role = *roleFlag
	}

	cliVars := map[string]string(vars)
	if st != nil {
		if err := checkResume(wf, st, cliVars, root); err != nil {
			errLine("%v", err)
			hint("start a fresh run: lca run %s", wf.Name)
			return 2
		}
		cliVars = st.Vars
	}
	effective := wf.effectiveVars(cliVars)

	ap := NewApprover(NewInput(os.Stdin))
	if !*ask {
		// Unattended by default: every action in a workflow was authored in a
		// file, exactly like a check_cmd. The sandbox, the GPU policy and the
		// deny rules still hold, so a diff touching a denied path still blocks.
		ap.TrustAll()
		ap.quiet = true
	}
	orch, err := setupOrchestrator(cfg, ap, os.Getenv("LCA_TRACE"))
	if err != nil {
		errLine("%v", err)
		return 2
	}
	defer orch.rec.Close()
	defer orch.tracer.Close()

	entry := ""
	if orch.roles != nil {
		entry = orch.roles.Entry
	}
	leadName := firstNonEmpty(wf.Role, entry, cfg.Agent)
	if st != nil && st.Tier != orch.activeTier() {
		// Steps 1..n ran on another chain; one green result over two tiers is
		// exactly the dishonest failure this design exists to prevent.
		errLine("this run started on the %s tier and %s is active now — start a fresh run", orTierNone(st.Tier), orTierNone(orch.activeTier()))
		return 2
	}
	if st != nil && st.Lead != "" && st.Lead != leadName {
		// roles.yaml's entry: can change between two invocations, and nothing
		// else would notice that the rest of the run bound to another lead.
		errLine("this run started with the %s lead role, and %s leads now — start a fresh run", st.Lead, leadName)
		return 2
	}
	if err := wf.bind(orch, leadName, effective); err != nil {
		errLine("%v", err)
		return 2
	}
	for _, w := range orch.warnings {
		warnLine("%s", w)
	}

	if *dry {
		section("plan", wf.Name+" · "+shortDir(wf.Path))
		if wf.Desc != "" {
			row("about", wf.Desc)
		}
		table([]string{"#", "step", "kind", "role", "tries", "timeout", "when", "check"}, wf.plan())
		hint("nothing ran: -dry-run validates and prints the plan")
		return 0
	}

	sess, err := orch.NewPrimary(leadName, "", nil)
	if err != nil {
		errLine("%v", err)
		return 2
	}
	sess.view = newTermView(sess)

	resumed := st != nil
	if !resumed {
		pruneRuns(cfg, atoiDefault(os.Getenv("LCA_KEEP_RUNS"), 50))
		dir, st = newRunState(cfg, orch, wf, leadName, effective)
	}

	runner, err := newRunner(orch, sess, wf, dir, st)
	if err != nil {
		errLine("%v", err)
		return 2
	}
	defer runner.Close()
	runner.signals = true
	if resumed {
		// Only now that the lock is held: a resume refused because the run is
		// still going must not have cleared the pause someone just asked for.
		attachRun(orch, dir, st)
		runner.save()
		runner.log.header("== resume %s (session %s) ==", nowTS(), orch.rec.id)
	}
	if *ask {
		for _, s := range wf.Steps {
			if s.Kind == stepDelegate {
				hint("-ask was given, so this run blocks on approval prompts — someone must watch it")
				break
			}
		}
	}
	orch.rec.Event("workflow_start", map[string]any{"workflow": wf.Name, "run": st.Run, "steps": len(wf.Steps), "resume": *resume})
	code := runner.Run(context.Background())
	orch.rec.Event("workflow_end", map[string]any{"workflow": wf.Name, "run": st.Run, "status": st.Status, "exit": code})
	return code
}

// checkResume refuses a resume that would continue against different work. A
// cursor into a rewritten step list points at the wrong step, and earlier steps
// already ran with the recorded vars — continuing anyway is exactly the
// dishonest failure this design exists to prevent. It also re-runs the step that
// failed, so a step with side effects (git commit, a migration) belongs last.
func checkResume(wf *Workflow, st *WorkflowState, cliVars map[string]string, root string) error {
	if wf.Sum != st.Sum {
		return fmt.Errorf("the workflow changed since this run started (%s)", wf.Path)
	}
	// runs/ is shared by every project on the machine, so a run of the same
	// workflow name can belong to another repository; its cursor, its recorded
	// outputs and its `git commit` steps would land in this tree.
	if st.Root != "" && root != "" && st.Root != root {
		return fmt.Errorf("run %s belongs to %s, not to %s", st.Run, shortDir(st.Root), shortDir(root))
	}
	for _, k := range sortedKeys(cliVars) {
		old, ok := st.Vars[k]
		if ok && old != cliVars[k] {
			return fmt.Errorf("-var %s=%s contradicts the value %q this run started with", k, cliVars[k], old)
		}
		// Silently dropping it would leave the operator believing they had
		// narrowed the remaining steps.
		if !ok {
			return fmt.Errorf("-var %s=%s was not part of this run, and a resume cannot add one", k, cliVars[k])
		}
	}
	return nil
}

// attachRun records that this process is now working on the run, and un-pauses
// it: a resume is what clears a pause.
func attachRun(o *Orchestrator, dir string, st *WorkflowState) {
	st.Sessions = append(st.Sessions, o.rec.id)
	st.Traces = append(st.Traces, o.tracer.Path)
	os.Remove(filepath.Join(dir, "pause"))
}

// newRunState opens a fresh run: its own directory under runs/, the vars it was
// started with, and the trace this process writes. The id carries the workflow
// name so `lca run -list` reads without a lookup.
func newRunState(cfg Config, o *Orchestrator, wf *Workflow, lead string, vars map[string]string) (string, *WorkflowState) {
	id := newRunID(wf.Name, o.rec.id)
	// Two runs in one process (two /run calls) must not share a directory.
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(runsDir(cfg), id)); err != nil {
			break
		}
		id = newRunID(wf.Name, fmt.Sprintf("%s-%d%d", time.Now().Format("20060102-150405"), os.Getpid(), n))
	}
	st := &WorkflowState{Version: wfStateVersion, Run: id, Workflow: wf.Name, Path: wf.Path, Sum: wf.Sum,
		Root: o.jl.Root, Lead: lead, Tier: o.activeTier(), NSteps: len(wf.Steps), Sessions: []string{o.rec.id}, Traces: []string{o.tracer.Path},
		Vars: vars, Started: nowTS(), Status: "running"}
	return filepath.Join(runsDir(cfg), id), st
}

// digest is the run as one block of text a model can read: what ran, what
// decided it, and the tail of anything that failed.
func (r *wfRunner) digest() string {
	var b strings.Builder
	fmt.Fprintf(&b, "workflow %s (run %s) finished %s\n", r.wf.Name, r.st.Run, r.st.Status)
	for _, ss := range r.st.Steps {
		fmt.Fprintf(&b, "- %s (%s): %s", ss.Name, ss.Kind, ss.Status)
		if ss.Status == stepFailed {
			fmt.Fprintf(&b, " — exit %d: %s", ss.Exit, truncate(firstLine(ss.Detail), 200))
		}
		b.WriteString("\n")
	}
	if r.st.Cursor < len(r.wf.Steps) {
		fmt.Fprintf(&b, "stopped before step %d of %d; resumable with `lca run %s -resume`.\n", r.st.Cursor+1, len(r.wf.Steps), r.st.Run)
	}
	return strings.TrimRight(b.String(), "\n")
}

func printWorkflows(cfg Config) {
	wfs, warns := listWorkflows(cfg)
	section("workflows")
	if len(wfs) == 0 {
		fmt.Println("  " + faint("none — put one in %s", shortDir(filepath.Join(cfg.Root, ".lca", "workflows"))))
		hint("a workflow is an ordered YAML program: run/prompt/delegate steps; see examples/workflows")
	} else {
		rows := [][]string{}
		for _, wf := range wfs {
			rows = append(rows, []string{wf.Name, plural(len(wf.Steps), "step", "steps"), shortDir(filepath.Dir(wf.Path)), wf.Desc})
		}
		table([]string{"name", "steps", "from", "description"}, rows)
		hint("lca run <name> [-var k=v …] · -dry-run prints the plan")
	}
	for _, w := range warns {
		warnLine("%s", w)
	}
}

func printRuns(cfg Config) {
	runs, warns := listRuns(cfg, 20)
	section("runs")
	if len(runs) == 0 && len(warns) == 0 {
		fmt.Println("  " + faint("none yet"))
		return
	}
	rows := [][]string{}
	for _, st := range runs {
		at := st.Cursor
		step := "done"
		if at < len(st.Steps) || st.Status != stepOK {
			step = fmt.Sprintf("step %d", at+1)
		}
		rows = append(rows, []string{st.Run, st.Workflow, stepStatusWord(st.Status), step, st.Updated})
	}
	table([]string{"run", "workflow", "status", "at", "updated"}, rows)
	for _, w := range warns {
		warnLine("%s", w)
	}
	hint("resume one: lca run <runid> -resume · pause: lca run <runid> -pause")
}
