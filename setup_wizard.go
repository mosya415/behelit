package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// /setup: the session configures itself. The operator types `lca`, a session
// opens, and the gateway, the models and the team are chosen inside it — no
// restart and no environment variable.
//
// Nothing touches disk until the last step. Everything the wizard decides is
// held in a setupPlan; only write() writes, so Ctrl-C at any screen leaves the
// tree exactly as it was: no half-written roles.yaml, no .bak, no lock file.

type keyMode uint8

const (
	keyNone keyMode = iota
	keyEnv
	keyPlain
)

type scope uint8

const (
	scopeProject scope = iota
	scopeUser
)

type setupPlan struct {
	endpoint  string
	reached   bool        // the gateway answered /v1/models
	served    []ModelInfo // gateway order, with MaxLen where reported
	keyMode   keyMode
	keyEnv    string // written as api_key_env
	keySecret string // keyPlain only, and only after the warning
	picked    []string
	roles     map[string]string // lead|coder|reviewer|cheap → model ("" = none)
	chains    map[string][]string
	tiers     map[string][]string
	tierOrder []string
	check     string   // the coder's check_cmd
	allow     []string // detectToolchain
	scope     scope
	probes    map[string]probeResult // only if the operator asked for probes
	textOnly  map[string]bool        // models whose native tool call came back as text
	keepKey   string                 // an api_key_env already in the config that keyNone leaves alone
	wantRetry bool                   // the gateway step's menu asked to re-enter the url
	tierRole  string                 // the tier lead and coder follow, if the operator asked for that ("" = keep their picks)

	steps []setupStep // the steps this run will take, for the "3/5" on each header
	step  int
}

// setupStep is one screen. The name and the position among the NUMBERED steps are
// what the header prints, so a step that does not apply cannot leave a gap.
type setupStep struct {
	name string
	fn   func(*setupPlan) error
	// aside is a conditional screen — credentials, which an anonymous gateway never
	// shows. It is left out of the count rather than numbered, because the gateway
	// header is printed before anyone knows whether a key will be wanted, and
	// "1/7 → 3/7" was the operator wondering what they had missed.
	aside bool
}

// stepLabel is "2/6 models", or just "credentials" for an aside. Every screen in
// the wizard takes its header from here, so no literal can drift from the steps
// that actually run: the numbers used to be hand-written, and said 1/7 → 3/7 →
// "7/7" followed by two more screens.
func (p *setupPlan) stepLabel() string {
	if p.step < 0 || p.step >= len(p.steps) {
		return ""
	}
	st := p.steps[p.step]
	if st.aside {
		return st.name
	}
	n, total := 0, 0
	for i, s := range p.steps {
		if s.aside {
			continue
		}
		total++
		if i <= p.step {
			n = total
		}
	}
	return fmt.Sprintf("%d/%d %s", n, total, st.name)
}

// head is the section header for the step being run.
func (p *setupPlan) head(detail ...string) {
	d := faint("%s", p.stepLabel())
	if len(detail) > 0 && detail[0] != "" {
		d += " " + detail[0]
	}
	section("setup", d)
}

// The team /setup writes IS the team lca init writes: the same descriptions,
// prompts and tool lists, lifted out of runInit's template so the two cannot
// produce different teams from the same gateway.
//
// roleWhat is what the operator is actually choosing on each role screen: four
// screens that differ only by a word in the header read as the same question
// asked four times.
var roleWhat = map[string]string{
	"lead":     "plans, splits the work and delegates — it reads the most code",
	"coder":    "writes the change in its own worktree until the check passes",
	"reviewer": "second opinion on a passed diff; pick another family, not the coder's",
	"cheap":    "summaries and compaction only — a small model is enough",
}

const (
	leadDesc  = "Understands the task, splits it, delegates, integrates and reports."
	coderDesc = "Implements one well-specified change and makes its check pass."
	revDesc   = "A second opinion on a diff the verifier already passed."
	cheapDesc = "Summaries and compaction."

	leadPrompt = `You lead a software task. Read enough code to understand it, then split it
into self-contained changes and delegate each to the coder role with a
precise task and a narrow check_cmd that fails before and passes after.
Launch independent delegations in one reply. A passed diff is already
applied; on failed, sharpen the task or split it. Finish with a short
report of what changed and what was verified.`

	coderPrompt = `You implement exactly one change in an isolated worktree. Read the
relevant code first, make the smallest correct change, and run the check
command yourself until it passes. Don't refactor unrelated code.`

	revPrompt = `You review one diff that already passed its check. Say what is wrong and
why it matters; say nothing about style. Answer with a short verdict.`
)

var (
	leadTools  = []string{"list_dir", "glob", "grep", "read_file", "todowrite", "delegate", "run_command"}
	coderTools = []string{"list_dir", "glob", "grep", "read_file", "edit", "write", "run_command"}
	revTools   = []string{"list_dir", "glob", "grep", "read_file"}
)

// ── the command ─────────────────────────────────────────────────────────────

func (r *Repl) cmdSetup(arg string) bool {
	// Both gates come before anything is drawn, and neither reads a byte.
	if !r.in.IsTTY() {
		errLine("/setup needs a terminal — stdin is not one")
		hint("non-interactive: lca init writes the team from the gateway's models")
		hint("%s", ".lca/config.json (or /set <key> <value> from a session) sets the endpoint"+gSep+"lca doctor checks both")
		return false
	}
	if n := r.sess.BackgroundRunning(); n > 0 {
		// Reload rebuilds o.agents, and a running child holds an *Agent pointer;
		// swapping the sandbox allowlist under a live subagent is a security
		// change mid-flight.
		errLine("%s still running in the background — /setup reloads the team and would pull it out from under it",
			plural(n, "a subagent is", "subagents are"))
		hint("%s", "/tasks shows them"+gSep+"run /setup when they are done")
		return false
	}

	p := &setupPlan{
		roles:    map[string]string{},
		chains:   map[string][]string{},
		tiers:    map[string][]string{},
		textOnly: map[string]bool{},
		probes:   map[string]probeResult{},
		endpoint: r.cfg.BaseURL,
	}
	p.check, p.allow = detectToolchain(r.orch.jl.Root)
	if r.orch.roles != nil && r.orch.roles.Sources != nil {
		p.scope = scopeProject
	}
	// An api_key_env already in the config is left alone by the "no key" path, so
	// the review screen has to say so rather than printing "key none" over a file
	// that still names a variable.
	if r.orch.fc != nil {
		p.keepKey = r.orch.fc.APIKeyEnv
	}

	steps := []setupStep{
		{name: "gateway", fn: r.setupGateway},
		{name: "credentials", fn: r.setupCredentials, aside: true},
		{name: "models", fn: r.setupModels},
		{name: "team", fn: r.setupTeam},
		{name: "tiers", fn: r.setupTiers},
		{name: "verification", fn: r.setupCheck},
		{name: "where it goes", fn: r.setupWrite},
	}
	if strings.TrimSpace(arg) == "models" {
		// Keep the current endpoint and credentials; go straight to the picks.
		p.keyMode = keyNone
		if err := r.probeGateway(p, p.endpoint); err != nil {
			errLine("%s", shortErr(err))
		}
		steps = steps[2:]
	}
	// The step numbers come from the slice that actually runs, never from a literal
	// in each screen: an anonymous gateway skips credentials, `/setup models` runs
	// five steps, and the hand-written numbers said 1/7 → 3/7 → "7/7" followed by
	// two more screens.
	p.steps = steps
	for i := range p.steps {
		p.step = i
		if err := p.steps[i].fn(p); err != nil {
			if errors.Is(err, errPickCancel) || errors.Is(err, errLineCancel) || errors.Is(err, errNoTTY) {
				fmt.Println()
				warnLine("setup aborted — nothing was written")
				hint("%s", "/setup starts again"+gSep+"/config shows what is in force now")
				return false
			}
			errLine("%v", err)
			return false
		}
	}
	return false
}

// ── gateway ─────────────────────────────────────────────────────────────────

func (r *Repl) setupGateway(p *setupPlan) error {
	p.head()
	row("now", p.endpoint+faint(gSep+"%s", r.sources()["endpoint"].render(r.orch.jl.Root)))
	for {
		v, err := ask(r.in, r.fieldEditor(), faint("gateway url "), p.endpoint)
		if err != nil {
			return err
		}
		if v == "" {
			v = p.endpoint
		}
		u, note := normalizeEndpoint(v)
		if u == "" {
			errLine("that is not a url")
			continue
		}
		if note != "" {
			hint("%s", note)
		}
		p.endpoint = u
		err = r.probeGateway(p, u)
		if err == nil {
			okLine("up"+gSep+"%s listed at %s", plural(len(p.served), "model", "models"), hostOf(u))
			var rows [][]string
			for _, m := range p.served {
				rows = append(rows, []string{m.ID, faint("%s", windowOf(m))})
			}
			table(nil, rows)
			return nil
		}
		errLine("%s", shortErr(err))
		if h := errorHint(err); h != "" {
			hint("%s", h)
		}
		if isAuthErr(err) {
			p.keyMode = keyEnv // the credentials step will ask for it properly
			return nil
		}
		if err := r.gatewayRecovery(p, u); err != nil {
			return err
		}
		if p.reached || !p.wantRetry {
			return nil
		}
	}
}

// gatewayRecovery is the menu under a failed probe. wantRetry says whether the
// loop should ask for the url again.
func (r *Repl) gatewayRecovery(p *setupPlan, u string) error {
	cs := []choice{
		{id: "retry", label: "try again", detail: faint("the gateway may still be starting"), on: true},
		{id: "edit", label: "edit the url", detail: faint("%s", u)},
		{id: "anyway", label: "configure it anyway", detail: faint("name the models by hand for a gateway that is down right now")},
	}
	i, err := pickOne(r.in, cs, pickOpts{title: "setup", detail: faint("%s"+gSep+"not reachable", p.stepLabel())})
	if err != nil {
		return err
	}
	p.wantRetry = cs[max(i, 0)].id != "anyway"
	if !p.wantRetry {
		// Deliberately allowed: a gateway that is down at 02:00 must not stop an
		// operator writing the team for it. The models step then asks for the ids by
		// hand, and the review screen repeats that it was never reached.
		p.reached = false
	}
	return nil
}

// probeGateway asks the endpoint what it serves and records it on the plan. It
// probes with the credentials on the PLAN — the ones about to be written — and
// not with the session's: a wizard that validates a key it is not going to use
// answers a question nobody asked.
func (r *Repl) probeGateway(p *setupPlan, url string) error {
	cfg := r.cfg
	cfg.BaseURL, cfg.Endpoints = url, []string{url}
	switch p.keyMode {
	case keyEnv:
		if p.keyEnv != "" {
			cfg.APIKey, cfg.APIKeyEnv = os.Getenv(p.keyEnv), p.keyEnv
		}
	case keyPlain:
		cfg.APIKey = p.keySecret
	}
	c := NewClient(cfg)
	models, err := c.ListModels()
	if err != nil {
		p.reached, p.served = false, nil
		return err
	}
	p.reached, p.served = true, models
	return nil
}

func isAuthErr(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403)
}

// windowOf is a served model's window with its origin attached: the deployment's
// own max_model_len when it reports one, else the card's number, else the plain
// statement that nobody published it. short drops the sentence-length suffix for
// a narrow table cell — the WORDS still have to match, or the same screen says
// "window unknown" in one column and something else in another.
func windowOf(m ModelInfo) string { return windowText(m, false) }

func windowText(m ModelInfo, short bool) string {
	if m.MaxLen > 0 {
		return srcNum(m.MaxLen, OriginServer)
	}
	prof := lookupProfile(m.ID)
	if prof.Context > 0 {
		if short {
			return srcNum(prof.Context, prof.Src.Context)
		}
		return srcNum(prof.Context, prof.Src.Context) + gSep + "the server reports none"
	}
	if short {
		return "window ?"
	}
	return "window unknown"
}

// ── credentials ─────────────────────────────────────────────────────────────

func (r *Repl) setupCredentials(p *setupPlan) error {
	if p.keyMode != keyEnv {
		// Only when the gateway ACTUALLY answered. Claiming an anonymous success one
		// line under "✕ endpoint returned 401" was the wizard contradicting itself on
		// the way to dying two lines later.
		if p.reached {
			fmt.Println("  " + faint("%s no key needed — the gateway served /v1/models anonymously", gNone))
		} else {
			fmt.Println("  " + faint("%s the gateway was never reached — no credentials asked for", gNone))
		}
		p.keyMode = keyNone
		return nil
	}
	for {
		if err := r.askCredentials(p); err != nil {
			return err
		}
		if p.keyMode == keyNone {
			return nil
		}
		// Probe again with what was just given, BEFORE the picks: the models step has
		// nothing to offer on a gateway that is still refusing, and its bare "the
		// gateway listed no models" never mentioned the credential.
		err := r.probeGateway(p, p.endpoint)
		if err == nil {
			okLine("the key works"+gSep+"%s listed at %s", plural(len(p.served), "model", "models"), hostOf(p.endpoint))
			return nil
		}
		errLine("%s", shortErr(err))
		if isAuthErr(err) {
			warnLine("the gateway still rejects that key")
			again, cerr := confirm(r.in, "try another credential?", true)
			if cerr != nil {
				return cerr
			}
			if again {
				continue
			}
		}
		// Not the credential, or the operator gave up on it: the same retry / edit /
		// configure-anyway menu the gateway step has.
		if err := r.gatewayRecovery(p, p.endpoint); err != nil {
			return err
		}
		if !p.wantRetry {
			return nil // "configure it anyway": the models step asks for the ids
		}
		// Back to the url. It probes with the credentials now on the plan, so it can
		// succeed outright — or come back here with another 401.
		if err := r.setupGateway(p); err != nil {
			return err
		}
		if p.reached || p.keyMode != keyEnv {
			return nil
		}
	}
}

func (r *Repl) askCredentials(p *setupPlan) error {
	cs := []choice{
		{id: "env", label: "name an environment variable", detail: faint("api_key_env: LCA_API_KEY — the file records the NAME"), on: true},
		{id: "none", label: "no key", detail: faint("this gateway takes any token (%s)", noAuthKey)},
		{id: "plain", label: "write the key into the file", detail: cYellow + gPartial + cReset + faint(" plain text — see the warning")},
	}
	i, err := pickOne(r.in, cs, pickOpts{title: "setup",
		detail: faint("%s"+gSep+"a file is readable by anyone who can read the project; a variable's name is not a secret", p.stepLabel())})
	if err != nil {
		return err
	}
	switch cs[max(i, 0)].id {
	case "none":
		p.keyMode = keyNone
	case "env":
		name, err := ask(r.in, r.fieldEditor(), faint("variable name "), firstNonEmpty(r.cfg.APIKeyEnv, "LCA_API_KEY"))
		if err != nil {
			return err
		}
		if !reEnvName.MatchString(name) {
			return fmt.Errorf("%q is not a variable name (upper case, digits, _)", name)
		}
		p.keyMode, p.keyEnv = keyEnv, name
		if os.Getenv(name) == "" {
			warnLine("$%s is not set in this shell — export it before the next request", name)
		}
	case "plain":
		path := p.configPath(r)
		// Said BEFORE a keystroke of the secret is accepted.
		warnLine("the key will be written in plain text to %s, mode 0600 — anyone who can read that file can use it",
			prettyPath(path, r.orch.jl.Root))
		yes, err := confirm(r.in, "write the key into the file anyway?", false)
		if err != nil {
			return err
		}
		if !yes {
			p.keyMode = keyNone
			return nil
		}
		// askSecret, never ask(): the shared editor echoes the buffer on every
		// keystroke and appends every submit to the ↑ history of the prompt that opens
		// afterwards — one keystroke from sending the key to the model as a message.
		secret, err := askSecret(r.in, faint("api key "))
		if err != nil {
			return err
		}
		if secret == "" {
			warnLine("nothing typed — no key will be written")
			p.keyMode = keyNone
			return nil
		}
		p.keyMode, p.keySecret = keyPlain, secret
	}
	return nil
}

// ── models ──────────────────────────────────────────────────────────────────

func (r *Repl) setupModels(p *setupPlan) error {
	if len(p.served) == 0 {
		return r.setupModelsByHand(p)
	}
	var names []string
	for _, m := range p.served {
		names = append(names, m.ID)
	}
	// The team that is loaded RIGHT NOW is the default when there is one: `/setup
	// models` is documented as "keep the endpoint, re-pick the models", and starting
	// from lca init's heuristics made re-picking start from the program's guess
	// instead of from what the operator chose last time. seq keeps the chain's own
	// order, so pressing Enter cannot reorder it (see pickState.result).
	pre, seq := map[string]bool{}, map[string]int{}
	if r.orch.roles != nil {
		served := map[string]bool{}
		for _, n := range names {
			served[n] = true
		}
		for _, a := range r.orch.roles.Roles {
			for i, m := range a.Models {
				// Only what this gateway actually serves: a team written against another
				// gateway would otherwise open the picker with nothing ticked at all.
				if !served[m] {
					continue
				}
				pre[m] = true
				if seq[m] == 0 {
					seq[m] = i + 1
				}
			}
		}
	}
	// why each model arrives ticked. Without it the screen opens with three or
	// four ticks the operator did not make and cannot account for.
	why := map[string]string{}
	mark := func(m, role string) {
		if m == "" {
			return
		}
		pre[m] = true
		if why[m] == "" {
			why[m] = role
		} else if !strings.Contains(why[m], role) {
			why[m] += "+" + role
		}
	}
	var proposal []string
	if len(pre) == 0 {
		// No team yet. The proposal IS lca init's, from the same functions — two per
		// role, because a role's models: is a chain and the second is its fallback —
		// but it is NOT ticked: a screen that opens with ticks the operator did not
		// make turns their first deliberate space into an untick of what they wanted.
		// Enter with nothing ticked takes it.
		for _, m := range pickModels(names, leadPref, 2) {
			mark(m, "lead")
		}
		for _, m := range pickModels(names, coderPref, 2) {
			mark(m, "coder")
		}
		mark(pickCheap(names), "cheap")
		for _, m := range names {
			if pre[m] {
				proposal = append(proposal, m)
			}
		}
		pre = map[string]bool{}
	}

	for {
		var cs []choice
		scale := windowScale(p.served)
		for _, m := range p.served {
			c := modelChoice(m, pre[m.ID], scale)
			c.seq = seq[m.ID]
			if r := why[m.ID]; r != "" {
				c.detail += faint("  (proposed as %s)", r)
				// FIRST in the note, not last: on a gateway with long model ids the
				// row itself has no room left for the annotation, and a note that
				// wraps must not be able to break the one phrase the operator is
				// looking for across two lines.
				c.note = "proposed as " + r + gSep + "" + c.note
			}
			cs = append(cs, c)
		}
		opts := pickOpts{multi: true, title: "setup",
			detail: faint("%s"+gSep+"%d served at %s", p.stepLabel(), len(cs), hostOf(p.endpoint))}
		// Above the menu, not as its hint line: the hint line carries the navigation
		// keys, and a cooked fallback (no raw mode) never prints it at all.
		if len(proposal) > 0 {
			hint("space ticks the models you want"+gSep+"enter with none ticked takes the proposal: %s",
				strings.Join(proposal, ", "))
		}
		idx, err := pick(r.in, cs, opts)
		if err != nil {
			return err
		}
		if len(idx) == 0 {
			if len(proposal) == 0 {
				errLine("nothing picked — a team needs at least one model")
				continue
			}
			p.picked = append([]string(nil), proposal...)
			okLine("%s (the proposal): %s", plural(len(p.picked), "model", "models"),
				strings.Join(p.picked, faint("%s", gSep)))
			return nil
		}
		p.picked = nil
		for _, i := range idx {
			p.picked = append(p.picked, cs[i].id)
		}
		okLine("%s: %s", plural(len(p.picked), "model", "models"), strings.Join(p.picked, faint("%s", gSep)))
		return nil
	}
}

// setupModelsByHand is the models step for a gateway that never answered: the
// ids typed, because nothing can list them. It is the other half of "configure it
// anyway" — that row promises to write the team for a gateway that is down right
// now, and the wizard used to die one screen later with "the gateway listed no
// models — nothing to pick from", having written nothing at all.
func (r *Repl) setupModelsByHand(p *setupPlan) error {
	p.head(faint("%snothing listed — name the models", gSep))
	hint("%s", "comma-separated ids, the lead's first"+gSep+"/doctor checks them against the gateway once it is up")
	v, err := ask(r.in, r.fieldEditor(), faint("model ids "), strings.Join(p.picked, ","))
	if err != nil {
		return err
	}
	p.picked, p.served = nil, nil
	for _, m := range splitFields(v) {
		p.picked = append(p.picked, m)
		p.served = append(p.served, ModelInfo{ID: m})
	}
	if len(p.picked) == 0 {
		return fmt.Errorf("no model ids given — a team needs at least one")
	}
	okLine("%s: %s", plural(len(p.picked), "model", "models"), strings.Join(p.picked, faint("%s", gSep)))
	return nil
}

// modelChoice is one served model with everything the client knows about it, and
// every number carrying its origin — a picker that prints an anonymous integer
// lends authority to a guess.
// windowScale is the largest window among the served models — the denominator
// every window gauge on a picker is drawn against. It is the models' own numbers
// and never a constant, so the bars compare the things on the screen with each
// other and claim nothing about what is "big".
func windowScale(ms []ModelInfo) int {
	n := 0
	for _, m := range ms {
		w := m.MaxLen
		if w == 0 {
			w = lookupProfile(m.ID).Context
		}
		n = max(n, w)
	}
	return n
}

// windowBar is one model's window against that scale. A model whose window
// nobody knows gets an EMPTY track rather than a short bar: "we do not know" has
// to look nothing like "small", which is the whole reason the bar is scaled and
// labelled instead of being drawn from a guess.
func windowBar(m ModelInfo, scale int) string {
	if scale <= 0 {
		return ""
	}
	w := m.MaxLen
	if w == 0 {
		w = lookupProfile(m.ID).Context
	}
	return gaugeFrac(float64(w)/float64(scale), 13, cYellow)
}

// windowCell is the picker's window column: the NUMBER right-aligned in its own
// field and the provenance left-aligned after it.
//
// srcNum returns them as one string, and right-aligning "1.05M (server)" against
// "1.05M (card)" as a unit lines up the closing bracket and not the digits — which
// is the one thing a right-aligned numeric column exists for, and it made two
// identical windows read two columns apart. "window ?" is a phrase and not a
// number, so it keeps the whole field.
func windowCell(m ModelInfo) string {
	s := windowText(m, true)
	if n, prov, ok := strings.Cut(s, " ("); ok {
		return padTo(n, 6, 1) + " " + padTo("("+prov, 9, 0)
	}
	return padTo(s, 16, 1)
}

func modelChoice(m ModelInfo, on bool, scale ...int) choice {
	prof := lookupProfile(m.ID)
	// windowOf's own short form, not a second reconstruction of it: srcNum returns
	// the literal "unset" for 0, so the picker said "unset" — a sampling word, and
	// the opposite of what is meant — for a model the table two lines above called
	// "window unknown".
	window := windowCell(m)
	// the bar goes BEFORE the number and never instead of it: srcNum already says
	// whether the number came from the server or from the card, and a bar cannot
	// say that
	bar := ""
	if len(scale) > 0 {
		if b := windowBar(m, scale[0]); b != "" {
			bar = b + "  "
		}
	}
	c := choice{id: m.ID, label: m.ID, on: on}
	if prof.Family == "" {
		c.detail = bar + window + faint("  no profile")
		c.note = fmt.Sprintf("no profile for %q (normalised %q) — nothing is overridden", m.ID, normalizeModelID(m.ID))
		return c
	}
	// The columns stay narrow enough to survive a narrow terminal; the rest of
	// the provenance is on the note, which costs a line only while the row is
	// highlighted.
	c.detail = bar + window + faint("  %s", prof.Family)
	c.note = fmt.Sprintf("matched %q"+gSep+"temperature %s"+gSep+"top_p %s"+gSep+"tools and reasoning are probed after the picks",
		prof.Key, srcFloat(prof.Temperature, prof.Src.Temperature), srcFloat(prof.TopP, prof.Src.TopP))
	return c
}

// ── team ────────────────────────────────────────────────────────────────────

func (r *Repl) setupTeam(p *setupPlan) error {
	if len(p.picked) == 1 {
		// The one-model path: there is no team to build, and a roles.yaml naming
		// one role would only get in the way.
		p.roles["lead"] = p.picked[0]
		warnLine("one model picked — writing it as the session's model, with no roles.yaml")
		hint("/setup again after more models are served, or lca init, builds a team")
		return nil
	}
	defaults := map[string]string{
		"lead":  firstOf(pickModels(p.picked, leadPref, 1)),
		"coder": firstOf(pickModels(p.picked, coderPref, 1)),
		"cheap": pickCheap(p.picked), // "" when none is small: the none row is then the default
	}
	for _, role := range []string{"lead", "coder", "reviewer", "cheap"} {
		def := defaults[role]
		if role == "reviewer" {
			def = differentFamily(p.picked, p.roles["coder"])
		}
		var cs []choice
		switch role {
		case "reviewer":
			cs = append(cs, choice{id: "", label: "none", detail: faint("the verifier decides alone"), on: def == ""})
		case "cheap":
			cs = append(cs, choice{id: "", label: "none", detail: faint("compaction runs on the lead's model"), on: def == ""})
		}
		for _, m := range p.picked {
			c := modelChoice(infoFor(p.served, m), m == def)
			if role == "reviewer" {
				c.warn = sameFamilyWarning(p.roles["coder"], m)
			}
			cs = append(cs, c)
		}
		i, err := pickOne(r.in, cs, pickOpts{title: "setup",
			detail: faint("%s"+gSep+"%s — %s", p.stepLabel(), role, roleWhat[role])})
		if err != nil {
			return err
		}
		if i < 0 {
			i = 0
		}
		p.roles[role] = cs[i].id
		if cs[i].id == "" {
			row(role, faint("none"))
			continue
		}
		p.chains[role] = chainFor(p.picked, cs[i].id)
		row(role, strings.Join(p.chains[role], faint(" %s ", gFlow)))
		if w := sameFamilyWarning(p.roles["coder"], p.roles["reviewer"]); role == "reviewer" && w != "" {
			warnLine("%s", w)
		}
	}
	hint("first is preferred, the rest are fallbacks")
	return nil
}

// chainFor is a role's models: the chosen model, then the next pick of a
// DIFFERENT family, so the chain is a real fallback and not the same weights
// twice.
func chainFor(picked []string, first string) []string {
	out := []string{first}
	fam := lookupProfile(first).Family
	for _, m := range picked {
		if m == first {
			continue
		}
		if f := lookupProfile(m).Family; fam == "" || f == "" || f != fam {
			out = append(out, m)
			break
		}
	}
	return out
}

// differentFamily is the reviewer's default: a model that does not share the
// coder's blind spots.
func differentFamily(picked []string, coder string) string {
	if coder == "" {
		return firstOf(picked)
	}
	fam := lookupProfile(coder).Family
	for _, m := range picked {
		if m == coder {
			continue
		}
		if f := lookupProfile(m).Family; fam == "" || f == "" || f != fam {
			return m
		}
	}
	return ""
}

// sameFamilyWarning is the one sentence both `/role x review y` and the wizard's
// reviewer step say about a same-family pair — and says nothing when either
// family is unknown, because two unrecognised gateway names are no evidence.
func sameFamilyWarning(coder, reviewer string) string {
	if coder == "" || reviewer == "" {
		return ""
	}
	// The worse case first: "X and X are the same family" described the same weights
	// reviewing their own diff as a family overlap, and invited the reader to think
	// two different models were involved.
	if coder == reviewer {
		return fmt.Sprintf("that is the coder's own model — %s would be reviewing itself", coder)
	}
	fc, fr := lookupProfile(coder).Family, lookupProfile(reviewer).Family
	if fc == "" || fc != fr {
		return ""
	}
	return fmt.Sprintf("%s and %s are the same family — a same-family second opinion shares the blind spots", coder, reviewer)
}

func firstOf(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[0]
}

func infoFor(served []ModelInfo, id string) ModelInfo {
	for _, m := range served {
		if m.ID == id {
			return m
		}
	}
	return ModelInfo{ID: id}
}

// ── tiers ───────────────────────────────────────────────────────────────────

func (r *Repl) setupTiers(p *setupPlan) error {
	p.head()
	// With fewer than three models a tier is a rename of one chain. The header is
	// printed anyway, and says why there is nothing to ask: a step that runs and
	// prints nothing is a gap in the numbering the operator has to explain to
	// themselves.
	if len(p.picked) < 3 {
		fmt.Println("  " + faint("%s %s picked — a tier over them would just rename one chain", gNone,
			plural(len(p.picked), "one model", "two models")))
		return nil
	}
	yes, err := confirm(r.in, fmt.Sprintf("name two tiers over these %d models? (cheap / premium, switchable with /tier)", len(p.picked)), false)
	if err != nil {
		return err
	}
	if !yes {
		return nil
	}
	for _, tier := range []string{"cheap", "premium"} {
		var cs []choice
		for _, m := range p.picked {
			on := isSmall(m)
			if tier == "premium" {
				on = !on
			}
			cs = append(cs, modelChoice(infoFor(p.served, m), on))
		}
		idx, err := pick(r.in, cs, pickOpts{multi: true, title: "setup",
			detail: faint("%s"+gSep+"%s", p.stepLabel(), tier)})
		if err != nil {
			return err
		}
		var chain []string
		for _, i := range idx {
			chain = append(chain, cs[i].id)
		}
		// Both of these used to happen silently, and together they made saying yes to
		// the offer strictly worse than saying no: with no model the client thinks is
		// small, `on := isSmall(m)` ticked nothing on the cheap screen and everything on
		// the premium one, so Enter Enter dropped the cheap tier and wrote a premium
		// tier identical to no tiering — and /tier then had nothing to switch between.
		switch {
		case len(chain) == 0:
			warnLine("nothing ticked — no %s tier", tier)
			continue
		case len(chain) == len(p.picked) && len(p.tierOrder) == 0:
			warnLine("that is every picked model — a %s tier the same as no tiering is not written", tier)
			hint("^n unticks the lot, then space picks the ones that belong in %s", tier)
			continue
		}
		p.tiers[tier] = chain
		p.tierOrder = append(p.tierOrder, tier)
		row(tier, strings.Join(chain, faint(" %s ", gFlow)))
	}
	// ASKED, and defaulting to no. `tier:` and `models:` are mutually exclusive in
	// the format — a role that declares a tier has its chain replaced at load — so
	// pointing lead and coder at one throws away the models they were just asked to
	// choose. That used to happen silently, one screen after the picks and with the
	// discarded chains still on the review screen. A role with no tier is "pinned"
	// and /tier cannot move it, so this is the whole of what tiering buys and it is
	// worth one plain question.
	if len(p.tierOrder) > 0 && p.roles["lead"] != "" {
		yes, err := confirm(r.in, fmt.Sprintf(
			"point lead and coder at the %s tier? their picked chains are REPLACED by it, and /tier can then switch them",
			p.tierOrder[len(p.tierOrder)-1]), false)
		if err != nil {
			return err
		}
		if yes {
			p.tierRole = p.tierOrder[len(p.tierOrder)-1]
		} else {
			hint("%s", "lead and coder keep their picks"+gSep+"the tiers stay in the file for a role that asks for one")
		}
	}
	return nil
}

// ── verification ────────────────────────────────────────────────────────────

func (r *Repl) setupCheck(p *setupPlan) error {
	p.head()
	if len(p.picked) == 1 {
		fmt.Println("  " + faint("%s one model, no team — nothing is delegated, so nothing needs a verifier", gNone))
		return nil
	}
	hint("the command that proves a delegated change works — it decides, not the model")
	// Labelled: detectToolchain finds nothing in a repo with no go.mod, and the
	// operator was left staring at a bare "›" with no idea what was wanted.
	v, err := ask(r.in, r.fieldEditor(), faint("check command "), p.check)
	if err != nil {
		return err
	}
	p.check = strings.TrimSpace(v)
	if p.check == "" {
		warnLine("no check_cmd — a delegated task will come back unverified")
	}
	return nil
}

// ── scope, probes, review, write ────────────────────────────────────────────

func (p *setupPlan) configPath(r *Repl) string {
	if p.scope == scopeUser {
		return r.userConfigPath()
	}
	return r.projectConfigPath()
}

// rolesPathIn is decided by the SCOPE the operator picked and by nothing else.
// It used to delegate to rolesPath, which returns rc.Sources[0] — the FIRST file
// loadRoles read, i.e. $LCA_DIR/roles.yaml whenever a global team exists. So
// "this project" overwrote the operator's team for every other repository, left
// <root>/.lca/roles.yaml untouched, and, because the project file wins per role
// name at load, persisted the new team nowhere that takes effect.
//
// rolesPath keeps its Sources[0] behaviour for /role save, which is round-tripping
// the file it read.
func (p *setupPlan) rolesPathIn(r *Repl) string {
	if p.scope == scopeUser {
		return filepath.Join(r.cfg.Dir, "roles.yaml")
	}
	return filepath.Join(r.orch.jl.Root, ".lca", "roles.yaml")
}

func (r *Repl) setupWrite(p *setupPlan) error {
	root := r.orch.jl.Root
	cs := []choice{
		{id: "project", label: "this project", detail: faint("%s", filepath.Join(root, ".lca")), on: true},
		{id: "every", label: "every project", detail: faint("%s", r.cfg.Dir)},
	}
	i, err := pickOne(r.in, cs, pickOpts{title: "setup", detail: faint("%s", p.stepLabel())})
	if err != nil {
		return err
	}
	if max(i, 0) == 1 {
		p.scope = scopeUser
	}

	if p.reached && len(p.picked) > 0 {
		yes, err := confirm(r.in, fmt.Sprintf("probe %s? one real tool call each", plural(len(p.picked), "model", "models")), false)
		if err != nil {
			return err
		}
		if yes {
			r.runProbes(p)
		}
	}

	// The review screen: everything that is about to happen, and nothing has
	// happened yet.
	section("setup", faint("review"))
	// "reached", lower case: statusText upper-cases its word, and a shouted REACHED
	// sat next to a sentence-case "never answered" in a program that is sentence-case
	// everywhere else.
	reach := cGreen + gUp + cReset + " reached" + faint(gSep+"%d models", len(p.served))
	if !p.reached {
		reach = cYellow + gPartial + cReset + " never answered" + faint("%s", gSep+"writing it anyway")
	}
	row("endpoint", p.endpoint+"  "+reach)
	switch {
	case p.keyMode == keyEnv:
		row("key", "api_key_env: "+p.keyEnv+faint("  the file records the name, not the secret"))
	case p.keyMode == keyPlain:
		row("key", warn("written into the file in plain text"))
	case p.keepKey != "":
		// "none" while leaving api_key_env in the file is not what the file will say.
		row("key", faint("unchanged"+gSep+"api_key_env: %s from %s", p.keepKey, prettyPath(r.orch.fc.From["api_key_env"], root)))
	default:
		row("key", faint("none"))
	}
	// The chains as the FILE will hold them, not as they were picked: with tiers
	// accepted, rolesConfig used to replace lead's and coder's chains with the tier
	// and the review screen still printed the discarded pick, so an operator who
	// chose kimi confirmed kimi and got qwen. The tier no longer replaces a role's
	// chain at all (see rolesConfig), and this reads from the same place the writer
	// does so the two cannot drift again.
	rc := p.rolesConfig(r)
	for _, role := range []string{"lead", "coder", "reviewer", "cheap"} {
		a := rc.role(role)
		if a == nil {
			continue
		}
		detail := windowText(infoFor(p.served, firstOf(a.Models)), true)
		if a.Tier != "" {
			detail = "tier " + a.Tier + faint(gSep+"%s", detail)
		}
		if role == "coder" && p.check != "" {
			detail += faint(gSep+"check %s", p.check)
		}
		row(role, strings.Join(a.Models, faint(" %s ", gFlow))+faint("  %s", detail))
	}
	// One row for all the tiers: row()'s label field is 9 characters, so "tier
	// premium" overran it and lost the value column's alignment.
	if len(p.tierOrder) > 0 {
		var ts []string
		for _, t := range p.tierOrder {
			ts = append(ts, t+faint(" %s ", gFlow)+strings.Join(p.tiers[t], faint(" %s ", gFlow)))
		}
		row("tiers", strings.Join(ts, faint("%s", " "+gSep+" "))+faint("  /tier switches"))
	}
	row("sandbox", faint("%s", strings.Join(p.allow, " ")))
	if len(p.probes) > 0 {
		okN, failN := 0, 0
		for _, pr := range p.probes {
			if pr.status == "ok" {
				okN++
			} else {
				failN++
			}
		}
		row("probes", faint("%d ok"+gSep+"%d failed", okN, failN))
		for _, id := range p.picked {
			pr, ok := p.probes[id]
			if !ok || pr.status == "ok" {
				continue
			}
			errLine("%s"+gSep+"%s", id, pr.detail)
			if pr.fix != "" {
				hint("%s", pr.fix)
			}
		}
	}
	cfgPath, rolesFile := p.configPath(r), p.rolesPathIn(r)
	if len(p.picked) > 1 {
		row("write", prettyPath(rolesFile, root)+"   "+faint("%s", existsWord(rolesFile)))
	}
	row("", prettyPath(cfgPath, root)+"   "+faint("%s", existsWord(cfgPath)))
	hint("nothing has been written yet")

	// A failed native probe: offer the transport the model actually works with.
	for _, id := range p.picked {
		if pr, ok := p.probes[id]; !ok || !strings.Contains(pr.detail, "came back as text") {
			continue
		} else {
			yes, err := confirm(r.in, fmt.Sprintf("take %s as it is — models: {%s: {transport: text}}?", id, id), true)
			if err != nil {
				return err
			}
			p.textOnly[id] = yes
		}
	}

	what := "write it and use it in this session?"
	if len(p.picked) > 1 {
		what = "write these two files and use them in this session?"
	}
	yes, err := confirm(r.in, what, true)
	if err != nil {
		return err
	}
	if !yes {
		return errPickCancel
	}
	written, err := p.write(r)
	if err != nil {
		errLine("%v", err)
		return fmt.Errorf("nothing was changed — both files were left as they were")
	}
	okLine("wrote %s", strings.Join(written, faint("%s", gSep)))
	if err := r.reload("setup"); err != nil {
		errLine("the files are written, but this session could not adopt them: %v", err)
		hint("%s", "restart lca to pick them up"+gSep+"/config shows what is in force now")
		return nil
	}
	okLine("the team is live in this session — /agents shows it, /doctor checks it")
	return nil
}

func existsWord(p string) string {
	if _, err := os.Stat(p); err == nil {
		return "overwrite  (keeps " + filepath.Base(p) + ".bak)"
	}
	return "create"
}

// runProbes asks each picked model for one real tool call. Offered AFTER the
// picks and never before: six served models is six real model calls and minutes
// of gateway time, and a wizard that spends a silent minute before showing
// anything is the wizard people quit. Ctrl-C stops the probing and returns to
// the review screen without aborting the wizard.
func (r *Repl) runProbes(p *setupPlan) {
	cfg := r.cfg
	cfg.BaseURL, cfg.Endpoints = p.endpoint, []string{p.endpoint}
	gw := NewClient(cfg)
	// The team that is ABOUT TO EXIST, not the one loaded at start-up: the wizard
	// is going to write transport: native, so probing with whatever the provider
	// currently defaults to would answer a question nobody asked.
	roles := p.rolesConfig(r)
	var mu sync.Mutex
	interruptible(r.sess, func(ctx context.Context) {
		sem := make(chan struct{}, 4) // bounded like probeMembers
		var wg sync.WaitGroup
		for _, id := range p.picked {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				res := probeModel(ctx, cfg, gw, roles, "lead", id)
				mu.Lock()
				p.probes[id] = res
				mu.Unlock()
				fmt.Printf("\r\033[K  %s %s %s\n", probeGlyph(res.status), id, faint("%s", ellipsize(res.detail, max(60, houseWidth()-16))))
			}(id)
		}
		wg.Wait()
	})
}

func probeGlyph(status string) string {
	switch status {
	case "ok":
		return cGreen + gUp + cReset
	case "warn":
		return cYellow + gPartial + cReset
	}
	return cRed + gDown + cReset
}

// ── the write ───────────────────────────────────────────────────────────────

// write commits the wizard in one shot: both files under one lock, each by
// atomic rename, roles.yaml first. If the second write fails the first is put
// back from the backup just made, because a team file naming an endpoint the
// config never recorded is the one half-written state that would confuse the
// next start.
func (p *setupPlan) write(r *Repl) ([]string, error) {
	root := r.orch.jl.Root
	cfgPath, rolesFile := p.configPath(r), p.rolesPathIn(r)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return nil, err
	}
	// Both directories, ordered by path so two sessions cannot deadlock taking them
	// in opposite orders. They are the same directory in the normal case; when the
	// scope puts roles.yaml somewhere else, writing it under the config's lock left
	// a concurrent /role save in another window free to interleave with it.
	unlock, err := lockDirs(filepath.Dir(cfgPath), filepath.Dir(rolesFile))
	if err != nil {
		return nil, err
	}
	defer unlock()

	var written []string
	var rolesBefore []byte
	rolesExisted := false
	if len(p.picked) > 1 {
		rolesBefore, err = os.ReadFile(rolesFile)
		rolesExisted = err == nil
		rc := p.rolesConfig(r)
		rc.Sources = []string{rolesFile}
		path, err := writeRolesLocked(root, rc, r.orch.rec)
		if err != nil {
			return nil, err
		}
		written = append(written, prettyPath(path, root))
	}

	// base_url and the credentials, and nothing else. It used to delete `endpoints`
	// on every multi-model gateway — a hand-written list of spare urls silently
	// dropped one line after a review screen whose whole contract is that it lists
	// everything that is about to happen. The wizard has no opinion about the
	// operator's other endpoints.
	vals := map[string]any{"base_url": p.endpoint}
	switch p.keyMode {
	case keyEnv:
		vals["api_key_env"], vals["api_key"] = p.keyEnv, nil
	case keyPlain:
		vals["api_key"], vals["api_key_env"] = p.keySecret, nil
	}
	if len(p.picked) == 1 {
		// The ONE case where a model key in the config is right: with a team the
		// chains own the model choice, and a model key would silently outrank them.
		vals["model"] = p.picked[0]
	}
	path, cerr := setConfigLocked(cfgPath, vals)
	if cerr != nil {
		if rolesExisted {
			os.WriteFile(rolesFile, rolesBefore, 0o644)
		} else if len(written) > 0 {
			os.Remove(rolesFile)
			os.Remove(rolesFile + ".bak")
		}
		return nil, cerr
	}
	written = append(written, prettyPath(path, root))
	return written, nil
}

// rolesConfig is the plan as a *RolesConfig, emitted by the existing
// RolesConfig.YAML() — there is no second roles writer.
func (p *setupPlan) rolesConfig(r *Repl) *RolesConfig {
	rc := &RolesConfig{
		Entry: "lead", Transport: transportNative, Apply: "verified",
		VerifyAttempts: 2, CheckTimeout: 900,
		Allow: p.allow, ModelOpts: map[string]*ModelOpts{},
		Tiers: map[string][]string{}, TierOrder: p.tierOrder,
		Members: map[string]*Member{localMemberName: {Name: localMemberName}},
	}
	for t, chain := range p.tiers {
		rc.Tiers[t] = chain
	}
	for id, on := range p.textOnly {
		if on {
			rc.ModelOpts[id] = &ModelOpts{Transport: transportText}
		}
	}
	add := func(name, desc, prompt string, tools []string, effort string, ctx int) {
		if p.roles[name] == "" {
			return
		}
		a := &Agent{Name: name, Description: desc, Prompt: prompt, Tools: tools, ToolsSet: true,
			Mode: "all", IsRole: true, Thinking: effort, Context: ctx, Models: p.chains[name], Source: p.rolesPathIn(r)}
		// The picked chain unless the operator ASKED for the tier: `tier:` and `models:`
		// are mutually exclusive in the format and YAML() emits one XOR the other, so a
		// tier silently discarded the chains chosen two screens earlier — pick kimi as
		// the lead, get whatever happens to be first in premium — while the review
		// screen still confirmed the discarded pick. Only lead and coder can be tiered:
		// the reviewer's whole job is to be a different family from the coder, and cheap
		// is chosen for being small, so remapping either would defeat its own reason.
		//
		// applyTier here as well, so a.Models IS the chain that will run and the review
		// screen reading this rc cannot print something the file does not mean.
		if p.tierRole != "" && (name == "lead" || name == "coder") && len(rc.Tiers[p.tierRole]) > 0 {
			a.Tier = p.tierRole
			rc.applyTier(a)
		}
		rc.Roles = append(rc.Roles, a)
	}
	add("lead", leadDesc, leadPrompt, leadTools, "high", 0)
	add("coder", coderDesc, coderPrompt, coderTools, "medium", 0)
	add("reviewer", revDesc, revPrompt, revTools, "medium", 0)
	add("cheap", cheapDesc, "", nil, "off", 32000)
	for _, a := range rc.Roles {
		switch a.Name {
		case "coder":
			a.CheckCmd = p.check
			if p.roles["reviewer"] != "" {
				a.Review = "reviewer"
			}
		case "cheap":
			a.Tools, a.ToolsSet = nil, true
		}
	}
	return rc
}

// ── live reload ─────────────────────────────────────────────────────────────

// adopt takes the configuration-derived half of n and leaves the live half
// alone. Every Session — including running children — holds this *Orchestrator
// pointer, so the pointer cannot be replaced; the fields are swapped under o.mu.
//
// Configuration-derived, copied:  cfg fc providers agents skills commands
//
//	userRules warnings roles remote members memOrd defMem hasMembers gatewayModels
//	mcp
//
// Live, kept:  jl rec tracer ap children nextTask slots reads history worktrees
//
//	locMem legMem
//
// jl is LIVE and not copied: every Session holds o.jl, so the allowlist is
// mutated in place (Jail.SetAllowed) and a fresh jail would orphan them.
//
// EVERY field added to Orchestrator later must be placed in one of those two
// lists. A field in neither is either stale after a reload or clobbers running
// state, and the compiler cannot tell you which.
func (o *Orchestrator) adopt(n *Orchestrator) {
	old := o.swap(n)
	// The MCP set being replaced owns live connections and, for a stdio server, child
	// PROCESSES. registerMCPTools has already pointed the tool bindings at the new
	// set's servers, so nothing holds a reference to these any more — and CloseMCP at
	// exit walks o.mcp, which is now the new one. Not closing them here left a stdio
	// server spawned before a reload running after lca was gone.
	if old != nil && old != o.mcp {
		closeMCPSet(old)
	}
}

// swap copies the configuration-derived half under the lock and hands back the MCP
// set it replaced, so the closing happens with no lock held.
func (o *Orchestrator) swap(n *Orchestrator) *MCPSet {
	o.mu.Lock()
	defer o.mu.Unlock()
	old := o.mcp
	// mcp is configuration-derived like the rest: without it /mcp and lca doctor
	// rendered the pre-reload allowlist, stdio posture and server list while the
	// model's calls went through the new set — the one screen a security review reads,
	// stale, and a server ADDED by the reload missing from it entirely.
	o.mcp = n.mcp
	o.cfg, o.fc = n.cfg, n.fc
	o.providers, o.agents, o.skills, o.commands = n.providers, n.agents, n.skills, n.commands
	o.userRules, o.warnings = n.userRules, n.warnings
	o.roles, o.remote = n.roles, n.remote
	o.members, o.memOrd, o.defMem, o.hasMembers = n.members, n.memOrd, n.defMem, n.hasMembers
	o.gatewayModels = n.gatewayModels
	// The member caches are keyed on the fleet that just changed.
	o.memMu.Lock()
	o.locMem, o.legMem = nil, nil
	o.memMu.Unlock()
	return old
}

// reload re-resolves the configuration and moves the running session onto it.
// The transcript, the trace and the audit log belong to this session, not to its
// configuration — a wizard that costs the operator their conversation is a
// restart with extra steps, which is what they asked not to do.
func (r *Repl) reload(why string) error {
	o := r.orch
	if n := r.sess.BackgroundRunning(); n > 0 {
		return fmt.Errorf("%s still running in the background — /tasks shows them", plural(n, "a subagent is", "subagents are"))
	}
	cfg, _, srcs := loadConfigWithSources()
	// The session's own overrides survive a reload: /model and /endpoint were
	// asked for by the operator a minute ago, and a reload that silently undid
	// them would be worse than no reload.
	for key, ss := range r.sources() {
		if ss.Src != SrcSession {
			continue
		}
		if v, ok := r.unsaved[key]; ok && v != "" {
			if err := applySetting(&cfg, key, v); err != nil {
				warnLine("could not carry %s = %q across the reload: %v", key, v, err)
			} else {
				srcs[key] = ss
			}
		}
	}
	cfg.Root, cfg.Unsafe = o.jl.Root, o.jl.Unsafe
	n, err := buildOrchestrator(cfg, o.ap, o.rec, o.tracer)
	if err != nil {
		return err
	}
	o.jl.SetAllowed(allowlistOf(cfg, n.roles))
	o.jl.Shell = n.roles != nil && n.roles.Shell
	o.adopt(n)
	r.cfg, r.cfgSrc = o.cfg, srcs
	r.local = o.providers.local
	entry := firstNonEmpty(cfg.Agent, "build")
	if o.roles != nil {
		entry = firstNonEmpty(o.roles.Entry, entry)
	}
	if o.agents[entry] == nil {
		entry = r.sess.agent.Name
	}
	if o.agents[entry] != nil {
		// Re-points s.agent at the NEW *Agent, re-chains the models and rebuilds
		// the system prompt.
		if err := r.sess.SetAgent(entry); err != nil {
			return err
		}
	}
	// SetAgent touches s.client ONLY when the agent declares Models or Model, and
	// the builtin "build" agent declares neither — which is exactly the single-model
	// /setup path that writes no roles.yaml. The session stayed on the pre-reload
	// *Client, with the old endpoint, the old model and the old token, one line under
	// "the team is live in this session". Team mode escaped it only because useModel
	// copies from providers.local.
	if len(r.sess.models) == 0 && o.providers.local != nil {
		if _, _, hosted := o.providers.Split(cfg.Model); hosted {
			if err := r.sess.SetModel(cfg.Model); err != nil {
				warnLine("%s is configured but this session could not switch to it: %v", cfg.Model, err)
			}
		} else {
			r.sess.client = o.providers.local
			o.providers.Learn(r.sess.client)
		}
		r.sess.RefreshSystem()
	}
	// The assertion the comment above is worth nothing without: after a reload the
	// session must be reachable from the NEW providers, or the next request goes
	// somewhere the operator did not configure.
	if r.sess.client != nil && r.local != nil && r.sess.client.Endpoint() != r.local.Endpoint() && len(r.sess.models) == 0 {
		warnLine("this session is still on %s while the configuration says %s — restart lca",
			hostOf(r.sess.client.Endpoint()), hostOf(r.local.Endpoint()))
	}
	markRoleSources(srcs, o.jl.Root, o.roles, r.sess.agent)
	registry = replRegistry()
	replCommands = r.menu() // /delegate, /members and /tier appear as the team gains them
	endpoint := ""
	if r.local != nil {
		endpoint = r.local.Endpoint()
	}
	roleCount := 0
	if o.roles != nil {
		roleCount = len(o.roles.Roles)
	}
	o.rec.Event("reload", map[string]any{"why": why, "roles": roleCount, "endpoint": endpoint})
	r.Banner() // the redrawn card is the proof
	return nil
}

// ── first start ─────────────────────────────────────────────────────────────

// offerSetup is the one line a brand-new directory gets. Offered, never forced:
// `lca` in a directory with a gateway already in the environment works today and
// must keep working. Not a TTY: nothing printed, nothing asked.
func (r *Repl) offerSetup() {
	// What actually matters, not the mere existence of a file at any scope: a
	// ~/.lca/config.json holding only {"approve":"run"} — what the operator gets the
	// first time they use /set -user — said nothing about whether this project has a
	// gateway and a team, and it silenced the offer permanently.
	configured := r.orch.fc != nil && (r.orch.fc.Raw["base_url"] != "" || r.orch.fc.Raw["model"] != "")
	switch {
	case !r.in.IsTTY(), r.orch.roles != nil, configured, os.Getenv("LCA_SETUP") == "off":
		return
	}
	fmt.Println()
	warnLine("nothing is configured here — no roles.yaml, no config file")
	hint("/setup asks for the gateway, shows what it serves, and gives the models roles")
	yes, err := confirm(r.in, "set up now?", true)
	if err != nil || !yes {
		return
	}
	r.cmdSetup("")
}
