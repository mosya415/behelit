package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Setting the team up by hand, without leaving the session: /role shows what
// each role is, changes its models and sampling, adds a role, and writes the
// result back to roles.yaml. /delegate hands one task to a role right now.

func (r *Repl) cmdRole(arg string) bool {
	o := r.orch
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return r.showRoles()
	}
	switch fields[0] {
	case "save":
		return r.saveRoles()
	case "new":
		if len(fields) < 2 {
			errLine("usage: /role new <name> [model <m1,m2>]")
			return false
		}
		name := fields[1]
		if o.agents[name] != nil {
			errLine("%s already exists", name)
			return false
		}
		a := &Agent{Name: name, Mode: "all", IsRole: true, Source: "session", Description: "added in this session"}
		o.agents[name] = a
		if o.roles == nil {
			o.roles = &RolesConfig{Apply: "verified", VerifyAttempts: 2, CheckTimeout: 600, ModelOpts: map[string]*ModelOpts{}}
		}
		o.roles.Roles = append(o.roles.Roles, a)
		okLine("role %s added", name)
		if len(fields) > 2 {
			return r.cmdRole(strings.Join(append([]string{name}, fields[2:]...), " "))
		}
		hint("/role %s model <name[,fallback]> gives it a model"+gSep+"/role save writes roles.yaml", name)
		return false
	}

	name := fields[0]
	a := o.agents[name]
	if a == nil {
		errLine("no role or agent %q", name)
		hint("/role lists them"+gSep+"/role new %s creates one", name)
		return false
	}
	if len(fields) == 1 {
		return r.showRole(a)
	}
	if len(fields) < 2 || (len(fields) < 3 && fields[1] != "use" && !r.canOfferRoleValue(fields[1])) {
		errLine("usage: /role %s <model|tier|effort|temperature|top_p|context|steps|check|review|fork|tools|use> <value>", name)
		return false
	}
	key, value := fields[1], strings.TrimSpace(strings.Join(fields[2:], " "))
	value = strings.Trim(value, "\"'")
	if value == "" {
		// No value given: offer the same choices by hand rather than making the
		// operator type ids. Off a terminal this falls through to the usage line
		// the command has always printed.
		v, ok := r.offerRoleValue(a, key)
		if !ok {
			errLine("usage: /role %s <model|tier|effort|temperature|top_p|context|steps|check|review|fork|tools|use> <value>", name)
			return false
		}
		value = v
	}
	switch key {
	case "use":
		return r.cmdAgent(name)
	case "model", "models":
		if t, ok := strings.CutPrefix(value, keepTierID); ok {
			okLine("%s keeps tier %s", name, t)
			return false
		}
		var models []string
		for _, m := range strings.FieldsFunc(value, func(c rune) bool { return c == ',' || c == ' ' }) {
			if m = strings.TrimSpace(m); m != "" {
				models = append(models, m)
			}
		}
		if len(models) == 0 {
			errLine("usage: /role %s model <name[,fallback]>", name)
			return false
		}
		// A hand-set chain replaces the tier, or /role save would write the tier
		// back and silently drop what was just set.
		a.Models, a.IsRole, a.Tier = models, true, ""
		if o.gatewayModels > 0 { // warn about names the gateway doesn't serve
			if served, err := r.local.ListModels(); err == nil {
				have := map[string]bool{}
				for _, m := range served {
					have[m.ID] = true
				}
				for _, m := range models {
					if !have[m] {
						warnLine("%s is not in the gateway's list — it will fail until it is served", m)
					}
				}
			}
		}
	case "effort":
		if _, ok := map[string]bool{"off": true, "none": true, "on": true, "low": true, "medium": true, "high": true, "max": true, "xhigh": true}[value]; !ok {
			errLine("effort must be off|on|low|medium|high|max")
			return false
		}
		a.Thinking = value
	case "temperature", "temp":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			errLine("temperature must be a number")
			return false
		}
		a.Temperature = &f
	case "top_p":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			errLine("top_p must be a number")
			return false
		}
		a.TopP = &f
	case "context":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			errLine("context must be a positive number of tokens")
			return false
		}
		a.Context = n
	case "steps":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			errLine("steps must be a positive number")
			return false
		}
		a.Steps = n
	case "tier":
		if o.roles == nil || len(o.roles.Tiers) == 0 {
			errLine("this team has no tiers: block in roles.yaml")
			return false
		}
		if _, ok := o.roles.Tiers[value]; !ok {
			errLine("no tier %q (tiers: %s)", value, o.roles.tierList())
			return false
		}
		a.Tier, a.IsRole = value, true
		o.roles.applyTier(a)
	case "fork":
		switch strings.ToLower(value) {
		case "true", "yes", "on":
			a.Fork = true
		case "false", "no", "off":
			a.Fork = false
		default:
			errLine("fork is true or false, got %q", value)
			return false
		}
	case "check", "check_cmd":
		a.CheckCmd = value
	case "review", "reviewer":
		if isNone(value) {
			a.Review = "none"
			break
		}
		rev := o.agents[value]
		if rev == nil || !rev.IsRole {
			errLine("no role %q — /role lists them, /role new %s creates one", value, value)
			return false
		}
		if value == name {
			errLine("a role cannot review itself")
			return false
		}
		if len(rev.Models) == 0 && rev.Model == "" {
			// With no chain of its own the reviewer would run on the reviewed
			// subagent's model, which is the model reviewing its own diff.
			errLine("%s has no model of its own — /role %s model <name[,fallback]> first", value, value)
			return false
		}
		if w := sameFamilyWarning(firstOf(a.Models), firstOf(rev.Models)); w != "" {
			warnLine("%s", w)
		}
		a.Review = value
	case "tools":
		var tools []string
		for _, t := range strings.FieldsFunc(value, func(c rune) bool { return c == ',' || c == ' ' }) {
			if t = strings.TrimSpace(t); t == "" {
				continue
			}
			if toolRegistry[t] == nil {
				errLine("unknown tool %q", t)
				hint("known: %s", strings.Join(toolOrder, ", "))
				return false
			}
			tools = append(tools, t)
		}
		a.Tools, a.ToolsSet = tools, true
	default:
		errLine("don't know how to set %q", key)
		hint("%s", "model"+gSep+"tier"+gSep+"effort"+gSep+"temperature"+gSep+"top_p"+gSep+"context"+gSep+"steps"+gSep+"check"+gSep+"review"+gSep+"fork"+gSep+"tools"+gSep+"use")
		return false
	}
	// the running session picks the change up immediately
	if a == r.sess.agent {
		if len(a.Models) > 0 {
			r.sess.models = append([]string(nil), a.Models...)
			r.sess.transport = ""
			r.sess.useModel(0)
		}
		r.sess.RefreshSystem()
	}
	o.rec.Event("role_change", map[string]any{"role": name, "key": key, "value": value})
	okLine("%s"+gSep+"%s = %s", name, key, value)
	hint("/role %s shows it"+gSep+"/role save keeps it in roles.yaml", name)
	return false
}

func (r *Repl) showRoles() bool {
	o := r.orch
	var roles []*Agent
	for _, n := range sortedKeys(o.agents) {
		if a := o.agents[n]; a.IsRole && !a.Hidden {
			roles = append(roles, a)
		}
	}
	if len(roles) == 0 {
		fmt.Println("  " + faint("no roles yet — /setup builds one, or /role new <name> model <model>"))
		return false
	}
	var rows [][]string
	for _, a := range roles {
		mark := " "
		if a == r.sess.agent {
			mark = cGreen + gUp + cReset
		}
		tools := "all"
		if a.ToolsSet {
			tools = strconv.Itoa(len(a.Tools))
		}
		ctx := gNil
		if a.Context > 0 {
			ctx = kfmt(a.Context)
		}
		rows = append(rows, []string{mark, a.Name, strings.Join(a.Models, faint(" %s ", gFlow)), firstNonEmpty(a.Thinking, gNil), ctx, tools, faint("%s", firstNonEmpty(a.CheckCmd, gNil))})
	}
	sectionTable("roles", "", []string{"", "role", "models", "effort", "context", "tools", "check"}, rows, 1, 2, 6)
	hint("%s", "/role <name> <model|tier|effort|temperature|top_p|context|steps|check|review|fork|tools|use> <value> — e.g. model <m1,m2>"+gSep+"effort high"+gSep+"check \"go test ./...\""+gSep+"use")
	hint("%s", "/role save writes the team to .lca/roles.yaml"+gSep+"/delegate <role> <task> hands one task over")
	return false
}

func (r *Repl) showRole(a *Agent) bool {
	section("role", a.Name)
	row("models", firstNonEmpty(strings.Join(a.Models, faint(" %s ", gFlow)), faint("inherits the caller's")))
	row("effort", firstNonEmpty(a.Thinking, faint("provider default")))
	temp, topP := faint("model default"), faint("model default")
	if a.Temperature != nil {
		temp = strconv.FormatFloat(*a.Temperature, 'f', -1, 64)
	}
	if a.TopP != nil {
		topP = strconv.FormatFloat(*a.TopP, 'f', -1, 64)
	}
	row("sampling", fmt.Sprintf("temperature %s"+gSep+"top_p %s", temp, topP))
	if a.Context > 0 {
		row("context", kfmt(a.Context))
	}
	if a.Steps > 0 {
		row("steps", strconv.Itoa(a.Steps))
	}
	row("tier", firstNonEmpty(a.Tier, faint("— (its own chain)")))
	row("check", firstNonEmpty(a.CheckCmd, faint("— (a delegated task will come back unverified)")))
	row("review", firstNonEmpty(a.Review, faint("— (the verifier decides alone)")))
	fork := faint("— (starts from a blank context)")
	if a.Fork {
		fork = "true" + faint(" (starts from the caller's reads)")
	}
	row("fork", fork)
	if a.ToolsSet {
		row("tools", strings.Join(a.Tools, ", "))
	} else {
		row("tools", faint("everything its permissions allow"))
	}
	row("source", faint("%s", prettyPath(a.Source, r.orch.jl.Root)))
	if a.Prompt != "" {
		fmt.Println()
		for _, l := range strings.Split(ellipsize(a.Prompt, 600), "\n") {
			fmt.Println("  " + faint("%s", l))
		}
	}
	return false
}

// saveRoles writes the current team back to .lca/roles.yaml, through the shared
// writeRoles so /setup and /role save keep one .bak policy and one lock.
func (r *Repl) saveRoles() bool {
	o := r.orch
	if o.roles == nil || len(o.roles.Roles) == 0 {
		errLine("there is no team to save")
		return false
	}
	path, err := writeRoles(o.jl.Root, o.roles, o.rec)
	if err != nil {
		errLine("%v", err)
		return false
	}
	okLine("wrote %s", prettyPath(path, o.jl.Root))
	return false
}

// keepTierID marks the picker row that means "change nothing": the id travels
// back through the same string the typed form takes, so the no-op needs no second
// return value and no state on the Repl.
const keepTierID = "@keep-tier:"

// offerRoleValue is the picker behind a bare "/role <name> model" or
// "/role <name> tier": it produces a STRING for the branch that already exists,
// so no selection logic is duplicated and the typed form stays authoritative.
func (r *Repl) offerRoleValue(a *Agent, key string) (string, bool) {
	if !r.canOfferRoleValue(key) || !r.in.IsTTY() {
		return "", false
	}
	switch key {
	case "model", "models":
		served, err := r.local.ListModels()
		if err != nil || len(served) == 0 {
			errLine("the gateway lists no models to pick from")
			return "", false
		}
		// seq is the chain's own order. Without it the rows come back in the gateway's
		// /v1/models order, so merely opening the menu and pressing Enter rewrote the
		// chain — and for the reviewer, whose first model was chosen precisely for not
		// being the coder's family, it promoted the coder's family to the front.
		seq := map[string]int{}
		for i, m := range a.Models {
			seq[m] = i + 1
		}
		var cs []choice
		// A tiered role's a.Models is the tier's EXPANDED chain, so a bare Enter used
		// to freeze a copy of it and drop `tier:` without a word. This row makes Enter
		// a real no-op, and anything else says what it costs.
		if a.Tier != "" {
			cs = append(cs, choice{id: keepTierID + a.Tier, label: "keep tier " + a.Tier,
				detail: strings.Join(r.orch.roles.Tiers[a.Tier], faint(" %s ", gFlow)), on: true, seq: 1})
		}
		for _, m := range served {
			c := modelChoice(m, a.Tier == "" && seq[m.ID] > 0)
			if a.Tier == "" {
				c.seq = seq[m.ID]
			}
			cs = append(cs, c)
		}
		idx, err := pick(r.in, cs, pickOpts{multi: true, title: "role " + a.Name,
			detail: faint("its model chain — first is preferred, the rest are fallbacks")})
		if err != nil || len(idx) == 0 {
			return "", false
		}
		var picked []string
		for _, i := range idx {
			if strings.HasPrefix(cs[i].id, keepTierID) {
				return cs[i].id, true // handed straight back, so Enter changes nothing
			}
			picked = append(picked, cs[i].id)
		}
		if len(picked) == 0 {
			return "", false
		}
		if a.Tier != "" {
			warnLine("%s will no longer follow tier %s — a chain of its own replaces it", a.Name, a.Tier)
		}
		return strings.Join(picked, ","), true
	case "tier":
		o := r.orch
		if o.roles == nil || len(o.roles.TierOrder) == 0 {
			errLine("this team has no tiers: block in roles.yaml")
			return "", false
		}
		var cs []choice
		for _, t := range o.roles.TierOrder {
			cs = append(cs, choice{id: t, label: t, detail: strings.Join(o.roles.Tiers[t], faint(" %s ", gFlow)), on: t == a.Tier})
		}
		i, err := pickOne(r.in, cs, pickOpts{title: "role " + a.Name, detail: faint("which tier's chain it runs")})
		if err != nil || i < 0 {
			return "", false
		}
		return cs[i].id, true
	}
	return "", false
}

func (r *Repl) canOfferRoleValue(key string) bool {
	switch key {
	case "model", "models", "tier":
		return true
	}
	return false
}

// cmdDelegate hands a task to a role by hand: the same path the lead's
// delegate tool takes — isolated worktree, verifier, diff applied on pass.
func (r *Repl) cmdDelegate(arg string) bool {
	role, task, _ := strings.Cut(strings.TrimSpace(arg), " ")
	if role == "" || strings.TrimSpace(task) == "" {
		errLine("usage: /delegate <role> <task>")
		hint("%s", "/roles lists the roles"+gSep+"the role's check_cmd decides when it's done")
		return false
	}
	if a := r.orch.agents[role]; a == nil || !a.IsRole {
		errLine("no role %q", role)
		return false
	}
	tc := &ToolCtx{Ctx: nil, S: r.sess, Name: "delegate"}
	var out string
	interruptible(r.sess, func(ctx context.Context) {
		tc.Ctx = ctx
		out = runDelegateTool(tc, Args{"role": role, "task": strings.TrimSpace(task)})
	})
	r.sess.view.ToolDone("delegate", Args{"role": role}, out)
	// keep the conversation coherent: the model sees what was delegated
	r.sess.Msgs = append(r.sess.Msgs, Message{Role: "user", Content: fmt.Sprintf(
		"[The user delegated a task to the %s role by hand]\nTask: %s\nResult:\n%s", role, strings.TrimSpace(task), out)},
		Message{Role: "assistant", Content: "Understood.", Agent: r.sess.agent.Name})
	r.sess.saveTranscript()
	return false
}
