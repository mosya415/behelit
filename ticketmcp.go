package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The tracker and the forge, as `lca ticket` actually talks to them: lca's own
// code making the calls the pipeline: block named, through the MCP client and the
// approval gate that already exist.
//
// ── why lca makes these calls and not a model ───────────────────────────────
//
// Because a model that can call the tracker can comment on a ticket nobody asked
// about, and a model that can call the forge can open a merge request for work
// nobody reviewed. The whole posture of this command is that a model writes
// CONTENT and lca performs TRANSITIONS, and this file is the half of that
// sentence that would otherwise be a wish: the write tool is granted by name, to
// one session lca holds, for the one call lca is about to make.
//
// ── why that still is not "lca knows how to call Jira" ──────────────────────
//
// It is not, and it must not be. Nothing in here knows an argument name, a
// result field, a status word or an API path. Every one of those is in the
// block (ticketcfg.go), under the key an error names when it is not. The code
// here does three things with a configured name and nothing else: substitute
// lca's placeholders into the arguments, send the call, and read one dotted path
// out of the reply. Their jira-mcp exposes about seventy tools; the only honest
// position for a program that has never seen their instance is to be told.
//
// ── what is read back, and how it is read ───────────────────────────────────
//
// An MCP reply is TEXT. Usually it is JSON, often it is JSON wrapped in prose by
// a server that means well. tktReply parses what it can and says plainly what it
// could not: a path that does not resolve is an error naming the path and showing
// the head of the reply, because a blank filled in here becomes a branch named
// after nothing, or a merge request lca thinks it already opened.

// tktCaller is one MCP call. The interface exists so the state machine's tests
// can stand a stub where the server would be — the layer below this one (the
// client, the handshake, the reconnect, the secret scrub) has its own tests, and
// re-testing it here would only test the mock.
type tktCaller interface {
	Call(ctx context.Context, tool string, args map[string]any) (string, error)
}

// tktReplyMax is how much of a reply lca will parse. A tracker that answers a
// read with four megabytes of changelog is not a reason to spend the run's
// memory on it, and a truncated JSON document cannot be parsed anyway — so this
// is a refusal, never a silent cut.
const tktReplyMax = 4 << 20

// ── the caller lca actually uses ────────────────────────────────────────────

// tktMCP makes the calls through the registry, the approval gate and the audit
// log that every other MCP call goes through (mcp.go's mcpTool.call).
//
// The GRANT is the point of the session it holds. newChild denies mcp_write
// structurally — a subagent never even sees a write schema — and -y deliberately
// does not grant it, so an unattended run has no way to write anywhere outside
// its tree unless somebody named the tool. Here the names come from the block,
// they are appended as Allow rules on this one session, and nothing else in the
// run holds them: the coder's session, the reviewer's and the integrator's are
// ordinary children with the ordinary deny.
type tktMCP struct {
	orch *Orchestrator
	sess *Session
}

// newTktMCP builds that session and grants exactly the tools the block named for
// the calls this invocation will make.
//
// The grants are computed from the same tktNeed validate used, so a run without
// -new is not holding a grant for the tool that opens tickets. A read tool is
// not granted and does not need to be: mcp_read is allowed by default
// (permission.go), because the server is on the operator's own allowlist and a
// read that asks every time teaches people to type y without reading.
func newTktMCP(o *Orchestrator, lead *Session, pc *PipelineConfig, need tktNeed) (*tktMCP, error) {
	ag := o.agents[pc.Integrator]
	if ag == nil {
		return nil, usageErrf("pipeline: roles: integrator: %q is not a role in this roles.yaml", pc.Integrator)
	}
	sess, err := o.newChild(lead, ag, "the transitions lca performs itself")
	if err != nil {
		return nil, usageErrf("%v", err)
	}
	// Not resumable by task id and not addressable by anything: this session
	// exists to carry a jail, a recorder and six permission rules. Nothing will
	// ever send it to a gateway.
	o.forgetChild(sess.ID)
	for _, tool := range pc.writeTools(need) {
		sess.extra = append(sess.extra, Rule{"mcp_write", tool, Allow})
	}
	return &tktMCP{orch: o, sess: sess}, nil
}

// writeTools is every MCP tool this invocation may WRITE through, named. The
// list is the grant, so it is derived from what the run will actually do: a run
// with no -new cannot open a ticket even if the key is in the file, and a
// pipeline with push: false holds no forge grant at all.
func (pc *PipelineConfig) writeTools(need tktNeed) []string {
	var keep []string
	for _, c := range pc.configuredCalls(need) {
		if !c.write || strings.TrimSpace(c.tool) == "" || contains(keep, c.tool) {
			continue
		}
		keep = append(keep, c.tool)
	}
	return keep
}

func (c *tktMCP) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	mt := mcpTools[tool]
	if mt == nil {
		// validateTools already refused this before the run started, so reaching it
		// means the registry changed underneath us. Still row 2 and not a guess.
		return "", usageErrf("pipeline: %q is not a tool any configured mcp server exposes", tool)
	}
	tc := &ToolCtx{Ctx: ctx, S: c.sess, Name: "ticket"}
	text, err := mt.call(tc, Args(args))
	if err != nil {
		return "", tktCallErr(ctx, tool, err)
	}
	if len(text) > tktReplyMax {
		return "", usageErrf("%s answered with %s, more than lca will parse — a reply that big cannot be read as one document, and a cut one cannot be read at all", tool, byteCount(len(text)))
	}
	return text, nil
}

// tktCallErr translates an MCP failure into the row of the exit table it belongs
// to, which is the whole reason mcpTool.call returns a typed error.
//
//   - the server was not reachable → infra_error: the ticket goes back in the
//     queue and nothing on it is touched;
//   - the gate said no → row 2: a grant is missing from the configuration, which
//     is a thing to fix in a file and not a thing to retry tonight;
//   - the server answered with an error → an ordinary failure, because the
//     tracker has an opinion about this call and it is "no".
func tktCallErr(ctx context.Context, tool string, err error) error {
	var ce *mcpCallErr
	if !errors.As(err, &ce) {
		return err
	}
	switch {
	case ce.infra:
		return &infraErr{what: "mcp tool " + tool, err: ce}
	case ce.setup:
		// Reachable only by fixing a file or an environment: a credential variable
		// nobody exported into cron's environment, a server the lock file does not
		// pin, a server refused at load. Row 2 and not row 3 — "retry later, leave
		// the ticket alone" is how a permanent mistake becomes a pipeline that goes
		// silent for weeks, which is the one failure this design says is worse than
		// failing loudly.
		return usageErrf("%s: %s", tool, ce.msg)
	case ce.denied:
		return usageErrf("%s was refused before it left: %s. An unattended run writes only through a tool the pipeline: block names, and nothing else grants it — not -y, not an approve: key.", tool, ce.msg)
	}
	// A deadline with the run's own clock still running is the SERVER's deadline,
	// not ours: every MCP call is wrapped in its server's timeout (up to 120s), and
	// an error carrying context.DeadlineExceeded is tested before anything else, so
	// a tracker that was merely slow came out as "the run's deadline passed" and
	// exit 4 — which is false, and which a wrapper that escalates 4 and re-queues 3
	// never retries. A slow forge on find_merge_request is the precise case.
	if errors.Is(err, context.DeadlineExceeded) && ctx != nil && ctx.Err() == nil {
		return &infraErr{what: "mcp tool " + tool, err: fmt.Errorf("%s did not answer within its server's timeout: %s", tool, ce.msg)}
	}
	// %w and not %s: the cause travels, so an interrupted call still answers
	// errors.Is(context.Canceled) and exits 130 instead of being reported as a
	// task the agent did not manage.
	return fmt.Errorf("%s: %w", tool, ce)
}

// ── the tracker ─────────────────────────────────────────────────────────────

// tktTrackerCalls is tktTracker over a tktCaller and the block's args: and
// fields:. Stateless on purpose: every method's whole input is its arguments and
// the configuration, so two runs of one ticket make byte-identical calls.
type tktTrackerCalls struct {
	call tktCaller
	pc   *PipelineConfig
}

func (t tktTrackerCalls) Read(ctx context.Context, tool, key string) (TicketBody, error) {
	raw, err := t.send(ctx, tool, tktVerbRead, map[string]string{"key": key})
	if err != nil {
		return TicketBody{}, err
	}
	rep := &tktReply{tool: tool, text: raw}
	summary, err := rep.text4(t.pc.Tracker.Fields.path("summary"), "tracker: fields: summary")
	if err != nil {
		return TicketBody{}, err
	}
	body := raw
	// A ticket with no `fields: body` configured is handed over WHOLE. That names
	// nothing and guesses nothing — the reply is the only thing lca has — and the
	// alternative is a coder sent to implement a one-line title. A team that would
	// rather hand over just the description names the path.
	if p := t.pc.Tracker.Fields.path("body"); p != "" {
		if body, err = rep.text4(p, "tracker: fields: body"); err != nil {
			return TicketBody{}, err
		}
	}
	return TicketBody{Key: key, Summary: strings.TrimSpace(summary), Body: body}, nil
}

func (t tktTrackerCalls) Create(ctx context.Context, tool, project string, body TicketBody) (string, error) {
	raw, err := t.send(ctx, tool, tktVerbCreate, map[string]string{
		"project": project, "summary": body.Summary, "body": body.Body})
	if err != nil {
		return "", err
	}
	// The key, by the path the block names. There is no fallback and must not be:
	// a created ticket whose key lca could not read is the one failure this whole
	// design cannot recover from, and a wrong guess at it would cut a branch named
	// after somebody else's work.
	rep := &tktReply{tool: tool, text: raw}
	return rep.text4(t.pc.Tracker.Fields.path("key"), "tracker: fields: key")
}

func (t tktTrackerCalls) Comment(ctx context.Context, tool, key, body string) (string, error) {
	raw, err := t.send(ctx, tool, tktVerbComment, map[string]string{"key": key, "text": body})
	if err != nil {
		return "", err
	}
	// The comment's own id is optional: it is bookkeeping for a person reading the
	// state file, and the thing that makes the report idempotent is the MARKER in
	// the comment's text, not an id. A path that was named and does not resolve is
	// still an error — a configured path that silently misses is how a state file
	// starts lying.
	p := t.pc.Tracker.Fields.path("comment_id")
	if p == "" {
		return "", nil
	}
	rep := &tktReply{tool: tool, text: raw}
	return rep.text4(p, "tracker: fields: comment_id")
}

// FindComment looks for this run's own marker in the ticket, and it is given the
// READ tool because the block configures no comment-search tool: lca does not
// get to assume this tracker has one.
//
// The marker is looked for in the reply's TEXT, not down a path. That is
// deliberate — a marker is a string the run put there itself, so finding it
// anywhere in what the tracker says about this ticket is proof it is there, and
// it costs no third configuration key. The consequence is worth stating: a read
// tool whose reply does not include comments can never show lca its own marker,
// and the run will comment again every night. Name a read tool that returns the
// comments, or accept a nightly note.
func (t tktTrackerCalls) FindComment(ctx context.Context, tool, key, marker string) (string, bool, error) {
	raw, err := t.send(ctx, tool, tktVerbRead, map[string]string{"key": key})
	if err != nil {
		return "", false, err
	}
	if !tktCarriesMarker(raw, marker) {
		return "", false, nil
	}
	return "", true, nil
}

// tktCarriesMarker looks for the marker in the reply, and then in the reply's
// own strings.
//
// The second half is not belt and braces: a JSON encoder escapes `<` as `\u003c`
// by default — Go's own does, and so do others — and the marker is an HTML
// comment, because that is what hides it in a rendered tracker comment. A raw
// substring scan therefore missed the run's own marker in exactly the shape a
// real tracker answers in, and the ticket collected one identical comment per
// night. Walking the parsed document asks the question of the DECODED text,
// where the marker is the marker whatever the encoder did to it.
func tktCarriesMarker(raw, marker string) bool {
	if marker == "" {
		return false
	}
	if strings.Contains(raw, marker) {
		return true
	}
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err != nil {
		return false
	}
	return tktAnyString(v, func(s string) bool { return strings.Contains(s, marker) })
}

// tktAnyString walks every string in a decoded JSON document. Keys as well as
// values: a tracker that returns its comments as an object keyed by their text
// is an odd tracker, and walking both costs nothing.
func tktAnyString(v any, pred func(string) bool) bool {
	switch t := v.(type) {
	case string:
		return pred(t)
	case []any:
		for _, e := range t {
			if tktAnyString(e, pred) {
				return true
			}
		}
	case map[string]any:
		for k, e := range t {
			if pred(k) || tktAnyString(e, pred) {
				return true
			}
		}
	}
	return false
}

func (t tktTrackerCalls) Move(ctx context.Context, tool, key, status string) error {
	_, err := t.send(ctx, tool, tktVerbMove, map[string]string{"key": key, "status": status})
	return err
}

// send is the one place a tracker call is built, so no verb can be given a
// different treatment than the others by accident.
func (t tktTrackerCalls) send(ctx context.Context, tool, verb string, vals map[string]string) (string, error) {
	if strings.TrimSpace(tool) == "" {
		return "", usageErrf("pipeline: tracker: %s: no tool is configured, so this transition has nothing to call", verb)
	}
	args := t.pc.Tracker.Args
	if !args.has(verb) {
		return "", usageErrf("pipeline: tracker: args: %s: nothing says what arguments %s takes, and lca does not guess an argument name", verb, tool)
	}
	return t.call.Call(ctx, tool, expandCall(args.of(verb), args.Order[verb], vals))
}

// ── the forge ───────────────────────────────────────────────────────────────

type tktForgeCalls struct {
	call tktCaller
	pc   *PipelineConfig
}

// Find is asked before every create, and that is the single most important call
// in this file: it is the only thing between a resumed run and a second merge
// request for one ticket, because the merge request lives on a server where this
// machine's state file cannot be the truth.
//
// The answer is re-checked against the branch lca asked for. A find tool that
// quietly ignores an argument it does not recognise answers with every open
// merge request there is, and adopting the first of those as this ticket's would
// make the run report somebody else's work as its own — and then comment the
// wrong link on the ticket. That is what `forge: fields: branch` is required for.
func (f tktForgeCalls) Find(ctx context.Context, tool, branch, target string) (TicketMR, bool, error) {
	if strings.TrimSpace(tool) == "" {
		return TicketMR{}, false, usageErrf("pipeline: forge: find_merge_request: no tool is configured, and nothing may create a merge request without first asking whether one exists")
	}
	args := f.pc.Forge.Args
	if !args.has(tktVerbFindMR) {
		return TicketMR{}, false, usageErrf("pipeline: forge: args: find_merge_request: nothing says what arguments %s takes", tool)
	}
	raw, err := f.call.Call(ctx, tool, expandCall(args.of(tktVerbFindMR), args.Order[tktVerbFindMR],
		map[string]string{"branch": branch, "target": target}))
	if err != nil {
		return TicketMR{}, false, err
	}
	rep := &tktReply{tool: tool, text: raw}
	items, err := rep.list(f.pc.Forge.Fields.path("list"), "forge: fields: list")
	if err != nil {
		return TicketMR{}, false, err
	}
	bp := f.pc.Forge.Fields.path("branch")
	if strings.TrimSpace(bp) == "" {
		// Never a silent "none": the answer to "is there one already" would then be
		// no every single time, and the next line of the machine opens one. That is
		// the second merge request this whole design exists to prevent, so it is a
		// refusal and not a default.
		return TicketMR{}, false, usageErrf("pipeline: forge: fields: branch: nothing says where a merge request's source branch is in %s's reply, so lca cannot tell whether one already exists for %s — and answering \"none\" would open a second one", tool, branch)
	}
	// The TARGET as well, because a merge request from our branch into a branch we
	// did not ask about is not ours. The documented filter is
	// `{source_branch: "${branch}", state: opened}`, so a merge request somebody
	// opened from agent/BSK-1 into `staging` matched on the source alone and was
	// adopted as this ticket's proposal — after which the run exited 0 at
	// `proposed`, the ticket comment linked the staging merge request, and nothing
	// was ever proposed against the branch the work actually merged into. This is
	// the same hole `fields: branch` is required for, one field short.
	tp := f.pc.Forge.Fields.path("target")
	if strings.TrimSpace(tp) == "" {
		return TicketMR{}, false, usageErrf("pipeline: forge: fields: target: nothing says where a merge request's target branch is in %s's reply, so lca cannot tell a merge request into %s from one into any other branch — and adopting the wrong one reports this ticket as proposed when nothing was", tool, target)
	}
	for _, it := range items {
		got, ok := tktPick(it, bp)
		if !ok {
			// Two keys can be wrong here and the message names both, because from the
			// outside they look identical: `fields: branch` may be the wrong path, or
			// `fields: list` may be missing and this "merge request" is really the
			// document the list is inside.
			return TicketMR{}, false, usageErrf("pipeline: forge: fields: branch: %q is not in what %s answered about a merge request, so lca cannot tell whether it is for %s. Either that path is wrong, or forge: fields: list does not name where the list of merge requests is in the reply (it is %q now) — the reply begins %s",
				bp, tool, branch, f.pc.Forge.Fields.path("list"), tktHead(raw))
		}
		if tktText(got) != branch {
			continue
		}
		gotT, ok := tktPick(it, tp)
		if !ok {
			return TicketMR{}, false, usageErrf("pipeline: forge: fields: target: %q is not in what %s answered about the merge request for %s, so lca cannot tell which branch it is asking to merge into — the reply begins %s",
				tp, tool, branch, tktHead(raw))
		}
		if tktText(gotT) != target {
			continue
		}
		return f.mrOf(it), true, nil
	}
	return TicketMR{}, false, nil
}

func (f tktForgeCalls) Create(ctx context.Context, tool string, mr TicketMR) (TicketMR, error) {
	if strings.TrimSpace(tool) == "" {
		return TicketMR{}, usageErrf("pipeline: forge: create_merge_request: no tool is configured")
	}
	args := f.pc.Forge.Args
	if !args.has(tktVerbCreateMR) {
		return TicketMR{}, usageErrf("pipeline: forge: args: create_merge_request: nothing says what arguments %s takes", tool)
	}
	raw, err := f.call.Call(ctx, tool, expandCall(args.of(tktVerbCreateMR), args.Order[tktVerbCreateMR],
		map[string]string{"branch": mr.Branch, "target": mr.Target, "title": mr.Title,
			"body": mr.Body, "key": mr.Ticket, "summary": mr.Summary}))
	if err != nil {
		return TicketMR{}, err
	}
	rep := &tktReply{tool: tool, text: raw}
	// The created merge request is read out of the create reply where the paths
	// resolve, and left empty where they do not — this is the one call whose
	// answer lca does not need in order to be correct, because the next run's
	// Find is what proves it exists. What it does need is to say SOMETHING about
	// where the merge request is, so a reply with neither id nor url is refused.
	made := f.mrOf(rep.any())
	made.Title, made.Branch, made.Target = mr.Title, mr.Branch, mr.Target
	if made.ID == "" && made.URL == "" {
		return made, fmt.Errorf("%s answered without the id or url the block's forge: fields: name, so nothing here can say where the merge request is — the reply begins %s",
			tool, tktHead(raw))
	}
	return made, nil
}

// mrOf reads one merge request out of whatever shape it arrived in, by the paths
// the block named and by nothing else.
func (f tktForgeCalls) mrOf(v any) TicketMR {
	out := TicketMR{}
	pick := func(key string) string {
		p := f.pc.Forge.Fields.path(key)
		if p == "" {
			return ""
		}
		got, ok := tktPick(v, p)
		if !ok {
			return ""
		}
		return tktText(got)
	}
	out.ID, out.URL, out.Title = pick("id"), pick("url"), pick("title")
	return out
}

// ── reading a reply ─────────────────────────────────────────────────────────

// tktReply is one MCP reply, parsed once and asked several questions.
type tktReply struct {
	tool string
	text string
	val  any
	done bool
	err  error
}

// any is the reply as a value: the JSON it parses as, or the text itself. A
// server that answers with a bare string is answering, and a path of "." has to
// reach it.
func (r *tktReply) any() any {
	if !r.done {
		r.done = true
		trimmed := strings.TrimSpace(r.text)
		if trimmed == "" {
			r.val = ""
			return r.val
		}
		if c := trimmed[0]; c == '{' || c == '[' {
			if err := json.Unmarshal([]byte(trimmed), &r.val); err != nil {
				r.err = err
				r.val = r.text
			}
			return r.val
		}
		// Not a document: a key, a url, a confirmation sentence. The text is the
		// value, and tktWholeReply is how a block says so.
		r.val = r.text
	}
	return r.val
}

// text4 resolves one configured path and returns it as text, or says exactly
// what it could not find and where the operator wrote the path.
//
// The error names THREE things on purpose — the key in roles.yaml, the path, and
// the head of the reply — because the person reading it at 08:00 has none of
// them: they have a ticket that did not move and a line in a log.
func (r *tktReply) text4(path, key string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", usageErrf("pipeline: %s: no path is configured, and lca does not guess which part of %s's reply holds it", key, r.tool)
	}
	v, ok := tktPick(r.any(), path)
	if !ok {
		why := ""
		if r.err != nil {
			why = fmt.Sprintf(" (and the reply is not JSON: %v)", r.err)
		}
		return "", usageErrf("pipeline: %s: %q is not in what %s answered%s — the reply begins %s",
			key, path, r.tool, why, tktHead(r.text))
	}
	s := strings.TrimSpace(tktText(v))
	if s == "" {
		return "", usageErrf("pipeline: %s: %q is in what %s answered and is empty, so there is nothing to use — the reply begins %s",
			key, path, r.tool, tktHead(r.text))
	}
	return s, nil
}

// list resolves a path expected to hold several things. A single object where a
// list was expected is read as a list of one: a forge that answers a filtered
// query with the one match is answering correctly, and refusing it would be lca
// insisting on a shape nobody promised.
func (r *tktReply) list(path, key string) ([]any, error) {
	v := r.any()
	if strings.TrimSpace(path) != "" && path != tktWholeReply {
		got, ok := tktPick(v, path)
		if !ok {
			return nil, usageErrf("pipeline: %s: %q is not in what %s answered — the reply begins %s",
				key, path, r.tool, tktHead(r.text))
		}
		v = got
	}
	switch t := v.(type) {
	case []any:
		return t, nil
	case map[string]any:
		return []any{t}, nil
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		// A find tool that answers in prose cannot be read, and reading it wrong
		// means either a second merge request or adopting one that does not exist.
		return nil, usageErrf("pipeline: %s: %s answered with text and not with merge requests, so lca cannot tell whether one exists for this branch — the reply begins %s. Name the path to the list in forge: fields: list, or name a tool that answers in JSON.",
			key, r.tool, tktHead(r.text))
	}
	return []any{v}, nil
}

// tktPick walks a dotted path. A numeric component indexes an array, because
// `0.iid` is how a reply that is a list of one is addressed, and `.` is the whole
// reply — spelled as a lone dot because "" is how YAML spells a key nobody
// wrote, and those two must not mean the same thing.
func tktPick(v any, path string) (any, bool) {
	p := strings.TrimSpace(path)
	if p == "" || p == tktWholeReply {
		return v, true
	}
	for _, part := range strings.Split(p, ".") {
		if part == "" {
			continue
		}
		switch t := v.(type) {
		case map[string]any:
			got, ok := t[part]
			if !ok {
				return nil, false
			}
			v = got
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// tktText is a JSON value as text. An object or an array is re-marshalled rather
// than described, because the one place that happens in practice is a rich-text
// description field — Jira's own `description` is a document, not a string — and
// the model reading it can read JSON perfectly well. Inventing a renderer for
// somebody's rich-text format is the kind of guess this file exists to avoid.
func tktText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// tktHead is the beginning of a reply, for an error message. Collapsed and
// clipped: this goes in one line of a log, and the whole reply is in the trace
// for whoever wants it.
func tktHead(text string) string {
	s := collapseWS(strings.TrimSpace(text))
	if s == "" {
		return "(nothing at all)"
	}
	return strconv.Quote(truncate(s, 200))
}
