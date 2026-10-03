package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The pipeline: block of roles.yaml, and the skills each of its stages loads.
//
// Everything in here is a fact about ONE TEAM that lca cannot know: what their
// tracker's MCP tools are called, what their forge's are called, which branch
// work lands on, whether this agent is allowed to push at all. Their jira-mcp
// alone exposes about seventy tools, and no two installations name them the
// same way — so a tool name is configuration, never a guess, and a missing one
// is an error that names the key.
//
// That is the whole posture of this file: it never fills a blank in. A default
// that silently does the wrong thing against a real tracker costs a ticket
// nobody can un-comment, while a refusal that names `pipeline: tracker: read`
// costs one alert and one line in a file.
//
// The block is parsed by loadRoles, with every other roles.yaml key, so a typo
// in it is found by `lca`, `lca doctor` and `lca ticket` alike. It is VALIDATED
// later, by `lca ticket` only (see validate): a team whose block is half
// written must still be able to open an interactive session.

// The five stages a model writes content for. Named constants and a closed set,
// because `skills:` is keyed by them and a stage nobody spelled right is a
// stage that silently gets no skills — at 3am, with the deploy runbook the one
// thing missing.
const (
	tktStageTicket   = "ticket"   // -new: the model writes the ticket body
	tktStageCoder    = "coder"    // the implementation, and every rework round
	tktStageReviewer = "reviewer" // the structured verdict against the diff
	tktStageMR       = "mr"       // the merge request's description
	tktStageReport   = "report"   // the comment that goes back on the ticket
)

// tktStages is the set, in the order the stages happen, which is also the order
// -dry-run prints them and the order the state file records them in.
var tktStages = []string{tktStageTicket, tktStageCoder, tktStageReviewer, tktStageMR, tktStageReport}

// PipelineConfig is roles.yaml's pipeline: block. Push and Rounds are POINTERS
// for the reason runResult.CheckExit is one: false and 0 are both meaningful
// answers, and neither must be inventable by a key nobody wrote. `push: false`
// is a team saying "this agent never pushes"; a missing push: is a team that
// has not said, and those two must not become the same run.
type PipelineConfig struct {
	// Src is the roles.yaml that last wrote any of this, which is the file every
	// error in validate names. The block merges across the search path key by key
	// like defaults: does, so this is the newest writer and not necessarily the
	// only one.
	Src string

	Project      string // the tracker project a -new ticket is created under
	BranchPrefix string // "agent/" — the branch for a ticket is prefix + key
	Remote       string // the git remote the branch is pushed to
	Target       string // the branch the work merges into
	Push         *bool  // whether this pipeline may push at all
	Rounds       *int   // rework rounds after a request_changes; 0 is legal

	Coder      string // which role implements
	Reviewer   string // which role judges the diff
	Integrator string // which role writes the MR description and the comment

	Tracker PipelineTracker
	Forge   PipelineForge

	// Skills names, per stage, the skills that stage is given — the team's shared
	// knowledge base (LCA_SKILLS, skills.go) reaching an unattended run. Named
	// and never left to the model to pick: a stage that runs at 3am must not
	// depend on a model noticing an index line.
	Skills map[string][]string
}

// PipelineTracker is the tracker's four verbs. Every value is an MCP tool name
// out of the operator's own mcp.lock.json — lca never composes one.
type PipelineTracker struct {
	Read       string // read a ticket
	Create     string // open one (-new only)
	Comment    string // comment on one
	Transition string // move its status
	// The two statuses, by the tracker's own spelling. They are optional as a
	// PAIR with Transition: a team that does not want lca touching workflow
	// states writes none of the three. Writing a status without the tool that
	// applies it, or the tool without a status to apply, is a half-configured
	// transition and validate says so.
	StatusDone    string // where a proposed ticket goes
	StatusBlocked string // where a blocked one goes

	// Args and Fields are the other half of "lca never guesses a name", and
	// stage 2 is where it bites: a tool NAME alone is not enough to call a tool.
	// See PipelineArgs.
	Args   PipelineArgs
	Fields PipelineFields
}

// PipelineForge is the forge's two verbs. FindMR is not optional and not a
// convenience: it is the only thing between a resumed run and a second merge
// request for one ticket, because the MR lives on a server where this machine's
// state file cannot be the truth.
type PipelineForge struct {
	CreateMR string
	FindMR   string
	Args     PipelineArgs
	Fields   PipelineFields
}

// PipelineArgs is `args:`: per verb, the ARGUMENTS that verb's tool takes, by
// the server's own names, with lca's placeholders as their values.
//
//	args:
//	  read:    {issueIdOrKey: "${key}"}
//	  comment: {issueIdOrKey: "${key}", body: "${text}"}
//
// It exists for exactly the reason the tool names do. Knowing that this team's
// read tool is called jira__issue_get says nothing about whether its argument is
// `issueKey`, `issueIdOrKey`, `key` or `id` — their jira-mcp exposes about
// seventy tools and no two installations agree — and the input schema, which lca
// does have from mcp.lock.json, gives names without saying which of them means
// "the ticket I am asking about". So the mapping is written down once by the
// person who knows, and a missing one is an error naming the key. The schema is
// still used, for what it CAN answer: whether a name exists at all, and whether
// a required one was left out (see validateTools).
//
// Order is kept because a round trip through /role save must not reorder a
// team's file for no reason.
type PipelineArgs struct {
	By    map[string]map[string]string
	Order map[string][]string
}

// PipelineFields is `fields:`: where in the REPLY each value lca needs lives, as
// a dotted path (`fields.summary`, `0.iid`). `.` means the reply itself.
//
// The same argument as PipelineArgs, from the other side: lca has to put the
// ticket's summary in front of the coder and the merge request's URL in the
// ticket comment, and no schema says which key of a reply holds either. A path
// that is not there is an error naming the path and showing the head of the
// reply — never a blank filled in.
type PipelineFields struct {
	By    map[string]string
	Order []string
}

// tktVerbVars is, per verb, every placeholder lca will substitute. A closed set,
// so `${tikcet}` is caught when roles.yaml is read and not at 3am as an argument
// the tracker rejects.
var tktVerbVars = map[string][]string{
	tktVerbRead:     {"key"},
	tktVerbCreate:   {"project", "summary", "body"},
	tktVerbComment:  {"key", "text"},
	tktVerbMove:     {"key", "status"},
	tktVerbCreateMR: {"branch", "target", "title", "body", "key", "summary"},
	tktVerbFindMR:   {"branch", "target"},
}

// tktVerbNeed is, per verb, the placeholders that MUST appear somewhere in its
// arguments. Each one is a call that would otherwise be silently wrong rather
// than refused: a read with no ${key} reads whatever the tool's default is, a
// comment with no ${text} posts an empty comment, a find with no ${branch} asks
// for every open merge request in the project.
var tktVerbNeed = map[string][]string{
	tktVerbRead:     {"key"},
	tktVerbCreate:   {"project", "summary", "body"},
	tktVerbComment:  {"key", "text"},
	tktVerbMove:     {"key", "status"},
	tktVerbCreateMR: {"branch", "target"},
	tktVerbFindMR:   {"branch"},
}

// The verb names, which are the keys of args:. They are the tool keys of the
// block they sit in, so `tracker: args: read:` sits under `tracker: read:`.
const (
	tktVerbRead     = "read"
	tktVerbCreate   = "create"
	tktVerbComment  = "comment"
	tktVerbMove     = "transition"
	tktVerbCreateMR = "create_merge_request"
	tktVerbFindMR   = "find_merge_request"
)

// The fields: keys, per block. Closed sets for tktVerbVars' reason: a
// `fields: sumary:` that parses and resolves nothing is a coder handed an empty
// ticket.
var (
	tktTrackerFields = []string{"key", "summary", "body", "comment_id"}
	tktForgeFields   = []string{"list", "branch", "target", "id", "url", "title"}
)

// tktWholeReply is the path that means "the reply itself", for a tool that
// answers with the value and not with a document around it. Spelled as a lone
// dot because "" is how YAML spells a key nobody wrote, and those two must not
// be the same answer.
const tktWholeReply = "."

func (pa PipelineArgs) of(verb string) map[string]string { return pa.By[verb] }

func (pa PipelineArgs) has(verb string) bool { return len(pa.By[verb]) > 0 }

func (pf PipelineFields) path(key string) string { return pf.By[key] }

// pipelineRounds and pipelinePush read the two pointers with the defaults that
// only apply once validate has proved they were written.
func (pc *PipelineConfig) pipelineRounds() int {
	if pc == nil || pc.Rounds == nil {
		return 0
	}
	return *pc.Rounds
}

func (pc *PipelineConfig) pipelinePush() bool {
	return pc != nil && pc.Push != nil && *pc.Push
}

// parsePipeline merges one roles.yaml's pipeline: block into pc, key by key, so
// a team file and a $LCA_ROLES overlay compose the way defaults: does — the
// later file wins per key and does not erase the keys it is silent about.
//
// It returns an error only for a value that cannot MEAN anything: a rework
// count that is not a number, a push: that is not a boolean, a skills: stage
// that is not one of the five. Missing keys are validate's business, because
// this runs inside every `lca` and a half-written block must not close the door
// to the session where it gets finished.
func parsePipeline(pc *PipelineConfig, doc *yNode, path string) (*PipelineConfig, error) {
	n := doc.child("pipeline")
	if n == nil {
		return pc, nil
	}
	if pc == nil {
		pc = &PipelineConfig{Skills: map[string][]string{}}
	}
	if pc.Skills == nil {
		pc.Skills = map[string][]string{}
	}
	pc.Src = path

	set := func(dst *string, key string) {
		if v := strings.TrimSpace(n.str(key)); v != "" {
			*dst = v
		}
	}
	set(&pc.Project, "project")
	set(&pc.BranchPrefix, "branch_prefix")
	set(&pc.Remote, "remote")
	set(&pc.Target, "target_branch")

	if v := strings.TrimSpace(n.str("push")); v != "" {
		b, err := parsePipelineBool(v)
		if err != nil {
			return nil, fmt.Errorf("%s: pipeline: push: %w", path, err)
		}
		pc.Push = &b
	}
	if v := strings.TrimSpace(n.str("rework_rounds")); v != "" {
		k, err := strconv.Atoi(v)
		if err != nil || k < 0 {
			return nil, fmt.Errorf("%s: pipeline: rework_rounds: %q is not a number of rounds (0 means one review and no rework)", path, v)
		}
		pc.Rounds = &k
	}

	if r := n.child("roles"); r != nil {
		set2 := func(dst *string, key string) {
			if v := strings.TrimSpace(r.str(key)); v != "" {
				*dst = v
			}
		}
		set2(&pc.Coder, "coder")
		set2(&pc.Reviewer, "reviewer")
		set2(&pc.Integrator, "integrator")
	}
	if t := n.child("tracker"); t != nil {
		set2 := func(dst *string, key string) {
			if v := strings.TrimSpace(t.str(key)); v != "" {
				*dst = v
			}
		}
		set2(&pc.Tracker.Read, "read")
		set2(&pc.Tracker.Create, "create")
		set2(&pc.Tracker.Comment, "comment")
		set2(&pc.Tracker.Transition, "transition")
		set2(&pc.Tracker.StatusDone, "status_done")
		set2(&pc.Tracker.StatusBlocked, "status_blocked")
		if err := parseCallArgs(&pc.Tracker.Args, t, path, "tracker",
			[]string{tktVerbRead, tktVerbCreate, tktVerbComment, tktVerbMove}); err != nil {
			return nil, err
		}
		if err := parseCallFields(&pc.Tracker.Fields, t, path, "tracker", tktTrackerFields); err != nil {
			return nil, err
		}
	}
	if f := n.child("forge"); f != nil {
		set2 := func(dst *string, key string) {
			if v := strings.TrimSpace(f.str(key)); v != "" {
				*dst = v
			}
		}
		set2(&pc.Forge.CreateMR, "create_merge_request")
		set2(&pc.Forge.FindMR, "find_merge_request")
		if err := parseCallArgs(&pc.Forge.Args, f, path, "forge",
			[]string{tktVerbCreateMR, tktVerbFindMR}); err != nil {
			return nil, err
		}
		if err := parseCallFields(&pc.Forge.Fields, f, path, "forge", tktForgeFields); err != nil {
			return nil, err
		}
	}
	if sk := n.child("skills"); sk != nil {
		for _, c := range sk.Children {
			if !contains(tktStages, c.Key) {
				// Loud, and at load time. `skills: reviewers:` parses, loads nothing and
				// leaves the reviewer running without the team's checklist — which is a
				// silence nobody discovers until a bad change is approved.
				return nil, fmt.Errorf("%s: pipeline: skills: %q is not a stage — the stages are %s",
					path, c.Key, strings.Join(tktStages, ", "))
			}
			var names []string
			for _, s := range c.List {
				if s = strings.TrimSpace(s); s != "" {
					names = append(names, s)
				}
			}
			if v := strings.TrimSpace(c.Value); v != "" {
				names = append(names, v) // a single skill may be written without brackets
			}
			pc.Skills[c.Key] = names
		}
	}
	return pc, nil
}

// parsePipelineBool refuses everything but the two words. YAML's dozen spellings
// of truth are a liability here: `push: maybe` must not become false, because a
// pipeline that quietly stops at `merged` looks exactly like a pipeline that
// finished.
func parsePipelineBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("%q is not true or false", v)
}

// flowClosed refuses a `{…}` that is not closed on its own line.
//
// This reader takes a flow mapping only when the line it starts on also ends it,
// so a wrapped one is read as a plain string value and the keys on the second
// line become SIBLINGS of the call they belong to. The error that came out of
// that named an argument as though it were a call — "description is not one of
// this block's calls" — which sends an operator hunting for a misspelled tool
// name they never wrote. The cause is worth naming where it is.
func flowClosed(v string) error {
	t := strings.TrimSpace(v)
	if strings.HasPrefix(t, "{") && !strings.HasSuffix(t, "}") {
		return fmt.Errorf("the `{…}` is not closed on this line, and this reader does not continue a flow mapping onto the next one — put the whole mapping on one line, or write its keys indented under it")
	}
	return nil
}

// parseCallArgs merges one block's args: into pa, verb by verb and key by key,
// so a $LCA_ROLES overlay can add one argument without restating the call.
//
// Three things are errors here, and all three are "this cannot MEAN anything":
// a verb that is not one of this block's verbs, a placeholder that is not in
// that verb's set, and an argument whose name is empty. Everything missing is
// validate's business, because this runs inside every `lca` and a half-written
// block must not close the door to the session where it gets finished.
func parseCallArgs(pa *PipelineArgs, block *yNode, path, where string, verbs []string) error {
	n := block.child("args")
	if n == nil {
		return nil
	}
	if pa.By == nil {
		pa.By, pa.Order = map[string]map[string]string{}, map[string][]string{}
	}
	for _, v := range n.Children {
		if err := flowClosed(v.Value); err != nil {
			return fmt.Errorf("%s: pipeline: %s: args: %s: %w", path, where, v.Key, err)
		}
		if !contains(verbs, v.Key) {
			return fmt.Errorf("%s: pipeline: %s: args: %q is not one of this block's calls — they are %s",
				path, where, v.Key, strings.Join(verbs, ", "))
		}
		if pa.By[v.Key] == nil {
			pa.By[v.Key] = map[string]string{}
		}
		for _, kv := range v.Children {
			name := strings.TrimSpace(kv.Key)
			if name == "" {
				return fmt.Errorf("%s: pipeline: %s: args: %s: an argument with no name", path, where, v.Key)
			}
			if err := checkCallVars(kv.Value, v.Key); err != nil {
				return fmt.Errorf("%s: pipeline: %s: args: %s: %s: %w", path, where, v.Key, name, err)
			}
			if _, seen := pa.By[v.Key][name]; !seen {
				pa.Order[v.Key] = append(pa.Order[v.Key], name)
			}
			pa.By[v.Key][name] = kv.Value
		}
	}
	return nil
}

// parseCallFields merges one block's fields: into pf. Same posture as
// parseCallArgs: an unknown key is loud at load time, because `fields: sumary:`
// parses, resolves nothing, and hands the coder a ticket with no text in it.
func parseCallFields(pf *PipelineFields, block *yNode, path, where string, keys []string) error {
	n := block.child("fields")
	if n == nil {
		return nil
	}
	if pf.By == nil {
		pf.By = map[string]string{}
	}
	for _, c := range n.Children {
		if !contains(keys, c.Key) {
			return fmt.Errorf("%s: pipeline: %s: fields: %q is not a value lca looks for — they are %s",
				path, where, c.Key, strings.Join(keys, ", "))
		}
		v := strings.TrimSpace(c.Value)
		if v == "" {
			return fmt.Errorf("%s: pipeline: %s: fields: %s: has no path. Write a dotted path into the reply (fields.summary), or %q for the reply itself",
				path, where, c.Key, tktWholeReply)
		}
		if _, seen := pf.By[c.Key]; !seen {
			pf.Order = append(pf.Order, c.Key)
		}
		pf.By[c.Key] = v
	}
	return nil
}

// checkCallVars refuses a ${placeholder} this verb has no value for. Written
// against the verb's own set and not a global one: `${status}` in a merge
// request's title is a typo for something, and substituting nothing would put a
// literal "${status}" on a forge.
func checkCallVars(tmpl, verb string) error {
	for _, name := range callVars(tmpl) {
		if !contains(tktVerbVars[verb], name) {
			return fmt.Errorf("${%s} is not a value lca has here — for %s it substitutes %s",
				name, verb, varList(tktVerbVars[verb]))
		}
	}
	return nil
}

// callVars lists the ${name} placeholders in a template, in order, duplicates
// included: the caller only ever asks "is each of these known".
func callVars(tmpl string) []string {
	var out []string
	for i := 0; i+1 < len(tmpl); i++ {
		if tmpl[i] != '$' || tmpl[i+1] != '{' {
			continue
		}
		end := strings.IndexByte(tmpl[i+2:], '}')
		if end < 0 {
			return out
		}
		out = append(out, tmpl[i+2:i+2+end])
		i += 2 + end
	}
	return out
}

func varList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "${" + n + "}"
	}
	return strings.Join(out, " ")
}

// expandCall is one call's arguments, with the placeholders substituted and
// nothing else touched.
//
// A value that came from a PLACEHOLDER is always a string — a ticket key, a
// branch name, a comment body all are — and a value the operator typed as a
// bare literal is sent as the JSON it looks like, so `state: opened` is a string
// while `project_id: 42` and `draft: false` are a number and a boolean. That is
// reading the operator's own literal, not guessing a schema: a tool whose
// argument is an integer cannot be called with "42", and quoting forces a string
// for the one case where the two disagree.
func expandCall(args map[string]string, order []string, vals map[string]string) map[string]any {
	out := map[string]any{}
	for _, name := range order {
		tmpl, ok := args[name]
		if !ok {
			continue
		}
		vars := callVars(tmpl)
		s := tmpl
		for _, v := range vars {
			s = strings.ReplaceAll(s, "${"+v+"}", vals[v])
		}
		if len(vars) > 0 {
			out[name] = s
			continue
		}
		out[name] = literalArg(s)
	}
	return out
}

// literalArg reads a bare literal as the JSON scalar it spells, and as a string
// when it spells none. json.Unmarshal into `any` is the judge rather than a list
// of words, so `0`, `1e3` and `null` all mean what they mean in a request body.
func literalArg(s string) any {
	t := strings.TrimSpace(s)
	switch t {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if t == "" {
		return s
	}
	if c := t[0]; c != '-' && c != '+' && c != '.' && c != '[' && c != '{' && (c < '0' || c > '9') {
		return s
	}
	var v any
	if err := json.Unmarshal([]byte(t), &v); err == nil {
		return v
	}
	return s
}

// tktNeed is what THIS invocation needs the block to have said. Only -new moves
// it: creating a ticket needs a project to create it under and a tool to create
// it with, and a team that never uses -new must not be made to configure either.
type tktNeed struct{ creating bool }

// missingKeys collects what was not written, so one alert names all of it. An
// unattended pipeline that has to be fixed five times, one key per night, is
// five nights of tickets going nowhere.
type missingKeys struct {
	src  string
	list []string
}

func (m *missingKeys) want(have, key, why string) {
	if strings.TrimSpace(have) == "" {
		m.list = append(m.list, fmt.Sprintf("pipeline: %s — %s", key, why))
	}
}

func (m *missingKeys) wantRaw(key, why string) {
	m.list = append(m.list, fmt.Sprintf("pipeline: %s — %s", key, why))
}

// wantArgs is want() for one call's arguments: absent is missing, and present
// but without the placeholder that makes the call ABOUT something is worse than
// absent — a read with no ${key} reads the tool's default, which is a run that
// works on the wrong ticket instead of refusing.
func (m *missingKeys) wantArgs(pa PipelineArgs, verb, where, why string) {
	if !pa.has(verb) {
		m.wantRaw(where+": args: "+verb, why)
		return
	}
	var have []string
	for _, tmpl := range pa.of(verb) {
		have = append(have, callVars(tmpl)...)
	}
	var missing []string
	for _, need := range tktVerbNeed[verb] {
		if !contains(have, need) {
			missing = append(missing, "${"+need+"}")
		}
	}
	if len(missing) > 0 {
		m.wantRaw(where+": args: "+verb,
			fmt.Sprintf("it names no argument carrying %s, so the call would not be about this ticket at all. %s",
				strings.Join(missing, " or "), why))
	}
}

func (m *missingKeys) err() error {
	if len(m.list) == 0 {
		return nil
	}
	src := m.src
	if src == "" {
		src = "roles.yaml"
	}
	// plural() prefixes the count itself, so the noun goes in without the article:
	// "does not say 1 one thing lca cannot guess" is the first sentence an operator
	// meets and the one they quote in a bug report.
	head := fmt.Sprintf("%s: the pipeline: block does not say %s", src, plural(len(m.list), "thing lca cannot guess", "things lca cannot guess"))
	return usageErrf("%s:\n  %s", head, strings.Join(m.list, "\n  "))
}

// validate is the gate on the whole command: every key this run will need,
// named, before the tracker is touched or a model is bought. It is the one
// place allowed to say "this is missing", and it never says "so I assumed".
//
// The conditional requirements are all the same shape — a key is required when
// the run will actually use it:
//
//   - project / tracker.create    only with -new
//   - remote / forge.*            only when push: true; a no-push pipeline ends
//     at `merged` and cannot have a merge request
//   - tracker.transition          only when a status was named, and vice versa
func (pc *PipelineConfig) validate(rc *RolesConfig, need tktNeed) error {
	if pc == nil {
		return usageErrf("roles.yaml has no pipeline: block, and `lca ticket` is made entirely of facts about your team that lca cannot guess — the tracker's and the forge's MCP tool names, the branch prefix, the remote, the target branch, the roles and the rework rounds. README.md has the block to copy.")
	}
	m := &missingKeys{src: pc.Src}

	m.want(pc.BranchPrefix, "branch_prefix", "the branch for a ticket is this prefix plus the ticket key, and lca will not invent your naming convention")
	m.want(pc.Target, "target_branch", "the branch the work merges into")
	if pc.Push == nil {
		m.wantRaw("push", "true or false — whether this pipeline may push at all. There is no safe default: false would make a run that finished look like one that stopped, and true would push on a repository nobody meant it to")
	}
	if pc.Rounds == nil {
		m.wantRaw("rework_rounds", "how many times a request_changes verdict goes back to the coder. 0 is a legal answer and means one review with no rework")
	}

	m.want(pc.Coder, "roles: coder", "which role implements the ticket")
	m.want(pc.Reviewer, "roles: reviewer", "which role judges the diff")
	m.want(pc.Integrator, "roles: integrator", "which role writes the merge request description and the ticket comment")

	m.want(pc.Tracker.Read, "tracker: read", "the MCP tool that reads a ticket — your tracker's own tool name, out of mcp.lock.json")
	m.want(pc.Tracker.Comment, "tracker: comment", "the MCP tool that comments on a ticket. It is not optional: a blocked run has to say so on the ticket, because an unattended pipeline that goes quiet is worse than one that fails loudly")
	// The arguments and the reply paths, which are the other half of a tool name:
	// knowing the read tool is called jira__issue_get says nothing about whether
	// its argument is issueKey or issueIdOrKey, and nothing about which key of the
	// reply holds the summary. lca will not pick one.
	m.wantArgs(pc.Tracker.Args, tktVerbRead, "tracker",
		"the arguments your read tool takes, by its own names, e.g. {issueIdOrKey: \"${key}\"}")
	m.wantArgs(pc.Tracker.Args, tktVerbComment, "tracker",
		"the arguments your comment tool takes, e.g. {issueIdOrKey: \"${key}\", body: \"${text}\"}")
	m.want(pc.Tracker.Fields.path("summary"), "tracker: fields: summary",
		"where the ticket's own summary is in the read tool's reply, as a dotted path (fields.summary) — it is what the coder is told to implement")
	if need.creating {
		m.want(pc.Project, "project", "the tracker project -new creates the ticket under")
		m.want(pc.Tracker.Create, "tracker: create", "the MCP tool that opens a ticket — needed because this run was called with -new")
		m.wantArgs(pc.Tracker.Args, tktVerbCreate, "tracker",
			"the arguments your create tool takes, e.g. {project: \"${project}\", summary: \"${summary}\", description: \"${body}\"}")
		m.want(pc.Tracker.Fields.path("key"), "tracker: fields: key",
			"where the NEW ticket's key is in the create tool's reply. Without it nothing here knows what was opened, and opening a ticket is the one call that cannot be retried safely")
	}
	if pc.Tracker.Transition != "" {
		m.wantArgs(pc.Tracker.Args, tktVerbMove, "tracker",
			"the arguments your transition tool takes, e.g. {issueIdOrKey: \"${key}\", transition: \"${status}\"}")
	}
	// The transition trio, both ways round. Either half alone is a team that
	// believes lca is moving their ticket and a run that never does.
	if pc.Tracker.Transition == "" && (pc.Tracker.StatusDone != "" || pc.Tracker.StatusBlocked != "") {
		m.wantRaw("tracker: transition", "a status was named (status_done / status_blocked) and nothing says which MCP tool applies it")
	}
	if pc.Tracker.Transition != "" && pc.Tracker.StatusDone == "" && pc.Tracker.StatusBlocked == "" {
		m.wantRaw("tracker: status_done or status_blocked", "a transition tool was named and no status was, so the tool could never be called — name the statuses by your tracker's own spelling, or remove the tool")
	}
	if pc.pipelinePush() {
		m.want(pc.Remote, "remote", "the git remote the branch is pushed to, because push: true")
		m.want(pc.Forge.CreateMR, "forge: create_merge_request", "the MCP tool that opens a merge request")
		m.want(pc.Forge.FindMR, "forge: find_merge_request", "the MCP tool that finds an EXISTING merge request. It is the only thing between a resumed run and a second merge request for one ticket, because the merge request lives on a server where this machine's state file cannot be the truth")
		m.wantArgs(pc.Forge.Args, tktVerbCreateMR, "forge",
			"the arguments your create tool takes, e.g. {source_branch: \"${branch}\", target_branch: \"${target}\", title: \"${title}\", description: \"${body}\"}")
		m.wantArgs(pc.Forge.Args, tktVerbFindMR, "forge",
			"the arguments your find tool takes, e.g. {source_branch: \"${branch}\", state: opened}")
		// The branch of a FOUND merge request, and it is not bookkeeping: a find
		// tool that ignores an argument it does not know answers with every open
		// merge request in the project, and lca would adopt somebody else's as this
		// ticket's. The answer is re-checked against the branch we asked for, which
		// needs this path.
		m.want(pc.Forge.Fields.path("branch"), "forge: fields: branch",
			"where a merge request's SOURCE BRANCH is in the find tool's reply. The reply is re-checked against the branch lca asked for, because a tool that ignored the filter would otherwise hand this ticket somebody else's merge request")
		// And the TARGET, for the other half of the same hole. A merge request
		// somebody opened from this branch into `staging` is not this ticket's
		// proposal, and a find that matched the source branch alone adopted it —
		// after which the run reported `proposed` and nothing was ever proposed
		// against the branch the work merged into.
		m.want(pc.Forge.Fields.path("target"), "forge: fields: target",
			"where a merge request's TARGET BRANCH is in the find tool's reply. Both ends are checked, because a merge request from this branch into a branch lca did not ask about is somebody else's and must not be adopted as this ticket's proposal")
		if pc.Forge.Fields.path("url") == "" && pc.Forge.Fields.path("id") == "" {
			m.wantRaw("forge: fields: url (or id)", "where a merge request's address is in the forge's reply — it is what goes in the ticket comment, and a comment that says a merge request exists without saying where is a comment nobody can act on")
		}
	}
	if err := m.err(); err != nil {
		return err
	}

	// The three roles have to exist. A role name is checked here and not against
	// the gateway: an unknown role is the same class of mistake as an unknown key,
	// and finding it out after the ticket has been read costs a run for nothing.
	if rc != nil {
		for _, pair := range [][2]string{{"coder", pc.Coder}, {"reviewer", pc.Reviewer}, {"integrator", pc.Integrator}} {
			if rc.role(pair[1]) == nil {
				return usageErrf("%s: pipeline: roles: %s: %q is not a role in this roles.yaml — the roles are %s",
					pc.Src, pair[0], pair[1], tktRoleNames(rc))
			}
		}
		// The coder's check_cmd is the first of the two gates, and without it NOTHING
		// in this pipeline can ever merge: the review gate and the merge gate both
		// want a green check, and a check that never ran is not green (Exit is a
		// pointer so that cannot be faked). A pipeline in that shape pays for a coder
		// every night, stops at `implemented` and says "nothing checked this change"
		// — which reads as a broken check rather than as a key nobody wrote. So it is
		// refused here, by name, with the file it belongs in.
		if ag := rc.role(pc.Coder); ag != nil && strings.TrimSpace(ag.CheckCmd) == "" {
			return usageErrf("%s: roles: %s: check_cmd: the coder role has no check command, and `lca ticket` gates both the review and the merge on a green one — so this pipeline could pay for a coder every night and never merge anything. Write the command that proves a change is done here (it is the role's own key, beside its models), or point pipeline: roles: coder at a role that has one.",
				pc.Src, pc.Coder)
		}
	}
	if !pc.pipelinePush() && (pc.Forge.CreateMR != "" || pc.Forge.FindMR != "") {
		// Not fatal: a team that turned push off for a night left the forge keys in
		// place, and refusing the run over them would be lca being clever.
		return nil
	}
	return nil
}

// validateTools checks the configured tool names and arguments against the
// MANIFEST — mcp.lock.json, through the registry the session itself uses — which
// is the one source of truth about these tools that lca genuinely has.
//
// It deliberately asks only what a schema can answer: does this tool exist,
// is it a write where a write is needed, does it have an argument by this name,
// and did the block leave out one the tool declares required. It never infers
// WHICH argument means "the ticket" — that is what args: is for.
//
// Separate from validate because validate is pure: it is given a roles.yaml and
// nothing else, and the tests drive it that way. This one needs the registry,
// so it runs where the registry exists, before the tracker is touched.
func (pc *PipelineConfig) validateTools(need tktNeed) error {
	type call struct {
		where, verb, key, tool string
		write                  bool
	}
	calls := []call{
		{"tracker", tktVerbRead, "tracker: read", pc.Tracker.Read, false},
		{"tracker", tktVerbComment, "tracker: comment", pc.Tracker.Comment, true},
	}
	if need.creating {
		calls = append(calls, call{"tracker", tktVerbCreate, "tracker: create", pc.Tracker.Create, true})
	}
	if pc.Tracker.Transition != "" {
		calls = append(calls, call{"tracker", tktVerbMove, "tracker: transition", pc.Tracker.Transition, true})
	}
	if pc.pipelinePush() {
		calls = append(calls,
			call{"forge", tktVerbFindMR, "forge: find_merge_request", pc.Forge.FindMR, false},
			call{"forge", tktVerbCreateMR, "forge: create_merge_request", pc.Forge.CreateMR, true})
	}
	for _, c := range calls {
		mt := mcpTools[c.tool]
		if mt == nil {
			if err := mcpToolNameError(c.tool); err != nil {
				return usageErrf("%s: pipeline: %s: %v", pc.Src, c.key, err)
			}
			return usageErrf("%s: pipeline: %s: %q is not a tool any configured mcp server exposes. `lca mcp` lists what there is; the name is the one in .lca/mcp.lock.json, server__tool.",
				pc.Src, c.key, c.tool)
		}
		args := pc.Args(c.where).of(c.verb)
		props, required := mcpSchemaNames(mt)
		if len(props) == 0 {
			continue // a tool that declares no properties cannot be checked, only called
		}
		for _, name := range sortedKeys(args) {
			if !contains(props, name) {
				return usageErrf("%s: pipeline: %s: args: %s: %q is not an argument %s takes — its own schema names %s",
					pc.Src, c.where, c.verb, name, c.tool, strings.Join(props, ", "))
			}
		}
		for _, req := range required {
			if _, ok := args[req]; !ok {
				return usageErrf("%s: pipeline: %s: args: %s: %s declares %q required and the block does not give it a value — the call would be refused by the server, at 3am, after the ticket was read",
					pc.Src, c.where, c.verb, c.tool, req)
			}
		}
	}
	return nil
}

// Args is the args: of one block by name, so validateTools can walk both blocks
// in one loop instead of in two copies of the same loop.
func (pc *PipelineConfig) Args(where string) PipelineArgs {
	if where == "forge" {
		return pc.Forge.Args
	}
	return pc.Tracker.Args
}

// mcpSchemaNames reads a pinned tool's input schema: the argument names it
// declares, sorted, and the ones it declares required. Both empty for a schema
// that says nothing, which is a tool that can be called but not checked.
func mcpSchemaNames(mt *mcpTool) (props, required []string) {
	lt := mt.lockTool()
	if lt == nil || lt.InputSchema == nil {
		return nil, nil
	}
	if m, ok := lt.InputSchema["properties"].(map[string]any); ok {
		props = sortedKeys(m)
	}
	if list, ok := lt.InputSchema["required"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	sort.Strings(required)
	return props, required
}

// tktRoleNames lists the team, for the error above. Declaration order, which is
// the order roles.yaml was written in and the order /setup's screen shows.
func tktRoleNames(rc *RolesConfig) string {
	if rc == nil || len(rc.Roles) == 0 {
		return "none — this roles.yaml declares no roles at all"
	}
	names := make([]string, 0, len(rc.Roles))
	for _, a := range rc.Roles {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}

// tktBranchFor is the one expression that turns a ticket key into a branch
// name. One expression, so "the branch the run cut" and "the branch the push
// looks for" can never drift apart — and no slug invention: the key goes in as
// the tracker spells it, with only the characters git forbids replaced.
func tktBranchFor(pc *PipelineConfig, key string) string {
	return pc.BranchPrefix + tktSafeRef(key)
}

// tktSafeRef makes a ref component out of a ticket key. Git's own rules, not a
// style: a space, a tilde, a caret, a colon, a question mark, an asterisk, a
// bracket, a backslash and a run of dots are all refused by git check-ref-format.
func tktSafeRef(key string) string {
	var b strings.Builder
	prevDot := false
	for _, r := range strings.TrimSpace(key) {
		switch {
		case r == '.':
			if prevDot {
				continue
			}
			prevDot = true
			b.WriteRune(r)
			continue
		case r <= ' ' || r == 0x7f, r == '~', r == '^', r == ':', r == '?', r == '*',
			r == '[', r == ']', r == '\\', r == '@', r == '/':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
		prevDot = false
	}
	return strings.Trim(b.String(), "-.")
}

// tktSlug is the state directory's name for a ticket. Strict, because it becomes
// a path: `lca ticket ../../etc/passwd` is a wrapper interpolating the wrong
// variable, and the answer to it is a refusal and not a write. Only the
// characters a tracker key is actually made of survive.
//
// And it is INJECTIVE, which is the property that matters more than the
// readability. Dropping every other rune to '-' is not a cosmetic loss: this
// team's tracker has Cyrillic project keys, so `ЗАД-5` and `ПРО-5` both reduce
// to `5`, and one state directory for two tickets is a run that comments on one
// ticket, merges its branch and opens its merge request while the ticket on the
// command line is never worked — every night, silently. So a key whose safe
// spelling is not the key itself carries a short hash of the RAW key, and two
// different keys can no longer name one directory.
func tktSlug(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	safe := strings.Trim(b.String(), "-.")
	if safe == key && safe != "" {
		// Nothing was dropped and nothing was trimmed: the directory IS the key, which
		// is what makes `ls $LCA_DIR/tickets` readable for the teams whose keys are
		// plain ASCII.
		return safe
	}
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])[:10]
	if safe == "" {
		// Nothing of the key survives as a path component. The hash alone, so the
		// directory is still this ticket's own and never the tickets directory itself
		// — state.json and the lock written one level up would land among every other
		// ticket's, where `lca ticket -list` cannot see them and where every such key
		// collides.
		return "t-" + h
	}
	return safe + "-" + h
}

// tktCheckKey refuses a ticket key lca cannot name anything after. tktSlug can
// always make a directory out of one (a hash, if it has to), but a BRANCH cannot
// be a hash: it is the prefix plus the key, people read it on the forge, and git
// refuses an empty ref component outright. A key with nothing in it git accepts
// is a wrapper interpolating the wrong variable, and the answer to that is a
// refusal naming the key — not a run that gets as far as `branch` and dies
// there with the key missing from half its sentences.
func tktCheckKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return nil // a -new run has no key until the tracker gives it one
	}
	if tktSafeRef(key) == "" {
		return usageErrf("%q has nothing in it git will accept in a branch name, so this ticket can have no branch and no worktree — pass the tracker's own key for it", key)
	}
	return nil
}

// ── the shared knowledge base, per stage ────────────────────────────────────

// TicketSkill is one skill as it was IN FORCE for one stage of one run: its
// name, where it was found, and the hash of everything under it.
//
// The hash is the point. Two pipeline runs of the same ticket are otherwise not
// comparable — the coder may have had the deploy runbook on Tuesday and a
// rewritten one on Wednesday, and nothing on either record would say so. It is
// the same reason results.jsonl already carries roles_hash and prompt_hash, and
// it is recorded in the state file and in the -json result for the same reason.
type TicketSkill struct {
	Stage string `json:"stage"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   string `json:"dir"`
	Sum   string `json:"sha256"`
	Files int    `json:"files"`
	Bytes int    `json:"bytes"`

	// body is the SKILL.md instructions that go into that stage's task message.
	// Not serialised: the state file is a journal, not a copy of the knowledge
	// base, and the hash above is what makes two runs comparable.
	body string
}

// resolveStageSkills loads, per stage, exactly the skills the block named.
//
// A skill named in the config and missing from every skills directory is an
// ERROR that names the skill and the directories searched. It is not a warning
// and it is not a stage that quietly runs without it: a pipeline running without
// the team's deploy runbook is worse than one that refuses to start, because the
// first kind merges.
//
// The names are resolved against the same index the `skill` tool uses, so the
// precedence is the documented one — a project's own .lca/skills beats the
// shared repository, which beats ~/.claude.
func resolveStageSkills(pc *PipelineConfig, skills map[string]*Skill, dirs []string) ([]TicketSkill, error) {
	var out []TicketSkill
	for _, stage := range tktStages {
		for _, name := range pc.Skills[stage] {
			sk := skills[name]
			if sk == nil {
				return nil, usageErrf("%s: pipeline: skills: %s: no skill named %q.\n  searched: %s\n  found: %s\n  A stage that runs unattended is given its skills by name, so a missing one stops the run rather than letting that stage work without the team's instructions.",
					pc.Src, stage, name, strings.Join(dirs, "\n            "), tktSkillNames(skills))
			}
			ts := TicketSkill{Stage: stage, Name: name, Path: sk.Path, Dir: sk.Dir}
			body, sum, files, n, err := hashSkill(sk)
			if err != nil {
				return nil, usageErrf("%s: pipeline: skills: %s: %q is at %s and could not be read: %v", pc.Src, stage, name, sk.Path, err)
			}
			ts.body, ts.Sum, ts.Files, ts.Bytes = body, sum, files, n
			out = append(out, ts)
		}
	}
	return out, nil
}

func tktSkillNames(skills map[string]*Skill) string {
	if len(skills) == 0 {
		return "no skills at all on this machine"
	}
	return strings.Join(sortedKeys(skills), ", ")
}

// hashSkill reads a skill's instructions and fingerprints the whole directory
// it lives in: every file under it, by sorted relative path, with its length and
// its bytes — the shape rolesHash uses, for the reason rolesHash gives.
//
// The DIRECTORY and not just SKILL.md, because a runbook whose check script was
// rewritten is a different runbook even when its markdown is identical, and the
// model reads those files. heavyDir is skipped, which also keeps the .git of a
// shared skills repository out of the hash.
func hashSkill(sk *Skill) (body, sum string, files, size int, err error) {
	data, err := os.ReadFile(sk.Path)
	if err != nil {
		return "", "", 0, 0, err
	}
	_, body, _ = splitFrontmatter(string(data))

	var paths []string
	err = filepath.WalkDir(sk.Dir, func(p string, e fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if e.IsDir() {
			if p != sk.Dir && heavyDir[e.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(sk.Dir, p)
		if rerr != nil {
			return rerr
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", "", 0, 0, err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, rel := range paths {
		b, rerr := os.ReadFile(filepath.Join(sk.Dir, filepath.FromSlash(rel)))
		if rerr != nil {
			// A file that was listed a moment ago and is not readable now makes every
			// byte after it meaningless, so the whole hash is refused rather than
			// reported short — the same choice rolesHash makes.
			return "", "", 0, 0, rerr
		}
		fmt.Fprintf(h, "%s\n%d\n", rel, len(b))
		h.Write(b)
		size += len(b)
	}
	return strings.TrimSpace(body), hex.EncodeToString(h.Sum(nil)), len(paths), size, nil
}

// skillsFor is the stage's own slice of the resolved set, in the order the block
// named them: that order is what goes into the task message, so the team
// controls which instruction is read first.
func skillsFor(all []TicketSkill, stage string) []TicketSkill {
	var out []TicketSkill
	for _, s := range all {
		if s.Stage == stage {
			out = append(out, s)
		}
	}
	return out
}

// stageInstructions is what a stage's task message carries: the named skills'
// instructions, each framed with its name and its base directory, exactly as the
// skill tool frames one — so a model that has read a skill before reads this the
// same way. Empty when the stage was given no skills.
func stageInstructions(all []TicketSkill, stage string) string {
	ss := skillsFor(all, stage)
	if len(ss) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# The team's instructions for this stage\n")
	b.WriteString("These were named in roles.yaml for this stage, not chosen by you. Follow them.\n\n")
	for _, s := range ss {
		fmt.Fprintf(&b, "<skill_content name=%q>\n# Skill: %s\n\n%s\n\nBase directory for this skill: %s\nRelative paths in this skill are relative to that directory.\n</skill_content>\n",
			s.Name, s.Name, s.body, s.Dir)
	}
	return strings.TrimRight(b.String(), "\n")
}

// yaml renders the block back for RolesConfig.YAML(). /role save rewrites the
// whole file, so without this a team's tracker and forge tool names — which
// nothing in lca can reconstruct — would be deleted the first time somebody
// saved a team from a session.
//
// Only what was actually set is written: a round trip must not invent a `push:
// false` nobody asked for, because validate would then stop asking for it.
func (pc *PipelineConfig) yaml() string {
	if pc == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\npipeline:\n")
	line := func(key, val string) {
		if strings.TrimSpace(val) != "" {
			fmt.Fprintf(&b, "  %s: %s\n", key, val)
		}
	}
	line("project", pc.Project)
	line("branch_prefix", pc.BranchPrefix)
	line("remote", pc.Remote)
	line("target_branch", pc.Target)
	if pc.Push != nil {
		fmt.Fprintf(&b, "  push: %t\n", *pc.Push)
	}
	if pc.Rounds != nil {
		fmt.Fprintf(&b, "  rework_rounds: %d\n", *pc.Rounds)
	}
	sub := func(head string, kv [][2]string, more ...string) {
		var inner strings.Builder
		for _, p := range kv {
			if strings.TrimSpace(p[1]) != "" {
				fmt.Fprintf(&inner, "    %s: %s\n", p[0], p[1])
			}
		}
		for _, m := range more {
			inner.WriteString(m)
		}
		if inner.Len() > 0 {
			fmt.Fprintf(&b, "  %s:\n%s", head, inner.String())
		}
	}
	sub("roles", [][2]string{{"coder", pc.Coder}, {"reviewer", pc.Reviewer}, {"integrator", pc.Integrator}})
	sub("tracker", [][2]string{{"read", pc.Tracker.Read}, {"create", pc.Tracker.Create},
		{"comment", pc.Tracker.Comment}, {"transition", pc.Tracker.Transition},
		{"status_done", pc.Tracker.StatusDone}, {"status_blocked", pc.Tracker.StatusBlocked}},
		callYAML(pc.Tracker.Args, []string{tktVerbRead, tktVerbCreate, tktVerbComment, tktVerbMove}),
		fieldYAML(pc.Tracker.Fields, tktTrackerFields))
	sub("forge", [][2]string{{"create_merge_request", pc.Forge.CreateMR}, {"find_merge_request", pc.Forge.FindMR}},
		callYAML(pc.Forge.Args, []string{tktVerbCreateMR, tktVerbFindMR}),
		fieldYAML(pc.Forge.Fields, tktForgeFields))
	var sk strings.Builder
	for _, stage := range tktStages { // the stages' own order, not the map's
		if names := pc.Skills[stage]; len(names) > 0 {
			fmt.Fprintf(&sk, "    %s: [%s]\n", stage, strings.Join(names, ", "))
		}
	}
	if sk.Len() > 0 {
		b.WriteString("  skills:\n" + sk.String())
	}
	if b.Len() == len("\npipeline:\n") {
		return "" // nothing was ever set: do not write an empty block
	}
	return b.String()
}

// callYAML renders an args: block back for RolesConfig.YAML(). In the flow
// mapping the operator writes it in, and in the order they wrote it: /role save
// rewrites the whole file, and a round trip that reordered a team's arguments
// would make every diff of roles.yaml unreadable.
//
// Every value is quoted, including the bare literals. That is not cosmetic: an
// unquoted `${key}` is fine but an unquoted `false` would come back as a boolean
// and `42` as a number, and a round trip must not change what a call sends.
func callYAML(pa PipelineArgs, verbs []string) string {
	var b strings.Builder
	for _, verb := range verbs {
		args := pa.By[verb]
		if len(args) == 0 {
			continue
		}
		var parts []string
		for _, name := range pa.Order[verb] {
			if v, ok := args[name]; ok {
				parts = append(parts, fmt.Sprintf("%s: %q", name, v))
			}
		}
		// Anything the order never recorded — a hand-edited map, a future caller —
		// is still written, sorted, rather than dropped.
		for _, name := range sortedKeys(args) {
			if !contains(pa.Order[verb], name) {
				parts = append(parts, fmt.Sprintf("%s: %q", name, args[name]))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "      %s: {%s}\n", verb, strings.Join(parts, ", "))
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "    args:\n" + b.String()
}

// fieldYAML renders a fields: block back, in the order it was written.
func fieldYAML(pf PipelineFields, keys []string) string {
	var b strings.Builder
	write := func(k string) {
		if v, ok := pf.By[k]; ok {
			fmt.Fprintf(&b, "      %s: %q\n", k, v)
		}
	}
	for _, k := range pf.Order {
		write(k)
	}
	for _, k := range keys {
		if !contains(pf.Order, k) {
			write(k)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "    fields:\n" + b.String()
}

// tktNoPipeline is the refusal a run with no pipeline: block gets, and it exists
// because the obvious sentence was WRONG for the commonest first-night mistake.
//
// A roles.yaml that holds nothing but a pipeline: block parses perfectly and is
// then thrown away whole, because a roles file with no roles: in it is how this
// program spells "single-agent mode" — and the block goes with it. The operator
// was then told their file "has no pipeline: block" about the file they had just
// copied it into, and sent back to the README to copy it again.
//
// So when the block is missing, the files on the search path are asked whether
// one of them has it. Two file reads, no network, and the answer is the real
// requirement: the three roles the block names have to be DECLARED, in a
// roles.yaml on the same search path.
func tktNoPipeline(cfg Config) error {
	for _, p := range rolesSearchPaths(cfg) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		doc := parseYAMLish(string(data))
		if doc.child("pipeline") == nil {
			continue
		}
		if doc.child("roles") == nil {
			return usageErrf("%s has a pipeline: block and declares no roles:, so lca read it as single-agent mode and kept none of it. `lca ticket` needs the three roles the block names (pipeline: roles: coder, reviewer, integrator) DECLARED in a roles.yaml on the same search path, each with its models and — for the coder — its check_cmd. Add them to this file, or put the block beside the roles.yaml that already has them.", p)
		}
	}
	return usageErrf("roles.yaml has no pipeline: block, and `lca ticket` is made entirely of facts about your team that lca cannot guess — the tracker's and the forge's MCP tool names, the branch prefix, the remote, the target branch, the roles and the rework rounds. README.md has the block to copy.")
}
