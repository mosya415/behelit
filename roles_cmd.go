package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		hint("/role %s model <name[,fallback]> gives it a model · /role save writes roles.yaml", name)
		return false
	}

	name := fields[0]
	a := o.agents[name]
	if a == nil {
		errLine("no role or agent %q", name)
		hint("/role lists them · /role new %s creates one", name)
		return false
	}
	if len(fields) == 1 {
		return r.showRole(a)
	}
	if len(fields) < 3 && fields[1] != "use" {
		errLine("usage: /role %s <model|tier|effort|temperature|top_p|context|steps|check|review|fork|tools|use> <value>", name)
		return false
	}
	key, value := fields[1], strings.TrimSpace(strings.Join(fields[2:], " "))
	value = strings.Trim(value, "\"'")
	switch key {
	case "use":
		return r.cmdAgent(name)
	case "model", "models":
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
		if sameFamily(a, rev) {
			warnLine("%s and %s are the same family — a same-family second opinion shares the blind spots", a.Models[0], rev.Models[0])
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
		hint("model · tier · effort · temperature · top_p · context · steps · check · review · fork · tools · use")
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
	okLine("%s · %s = %s", name, key, value)
	hint("/role %s shows it · /role save keeps it in roles.yaml", name)
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
		fmt.Println("  " + faint("no roles yet — lca init writes a team, or /role new <name> model <model>"))
		return false
	}
	section("roles")
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
		ctx := "—"
		if a.Context > 0 {
			ctx = kfmt(a.Context)
		}
		rows = append(rows, []string{mark, a.Name, strings.Join(a.Models, faint(" → ")), firstNonEmpty(a.Thinking, "—"), ctx, tools, faint("%s", firstNonEmpty(a.CheckCmd, "—"))})
	}
	table([]string{"", "role", "models", "effort", "context", "tools", "check"}, rows)
	hint("/role <name> <model|tier|effort|temperature|top_p|context|steps|check|review|fork|tools|use> <value> — e.g. model <m1,m2> · effort high · check \"go test ./...\" · use")
	hint("/role save writes the team to .lca/roles.yaml · /delegate <role> <task> hands one task over")
	return false
}

func (r *Repl) showRole(a *Agent) bool {
	section("role", a.Name)
	row("models", firstNonEmpty(strings.Join(a.Models, faint(" → ")), faint("inherits the caller's")))
	row("effort", firstNonEmpty(a.Thinking, faint("provider default")))
	temp, topP := faint("model default"), faint("model default")
	if a.Temperature != nil {
		temp = strconv.FormatFloat(*a.Temperature, 'f', -1, 64)
	}
	if a.TopP != nil {
		topP = strconv.FormatFloat(*a.TopP, 'f', -1, 64)
	}
	row("sampling", fmt.Sprintf("temperature %s · top_p %s", temp, topP))
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

// saveRoles writes the current team back to .lca/roles.yaml.
func (r *Repl) saveRoles() bool {
	o := r.orch
	if o.roles == nil || len(o.roles.Roles) == 0 {
		errLine("there is no team to save")
		return false
	}
	path := filepath.Join(o.jl.Root, ".lca", "roles.yaml")
	if len(o.roles.Sources) > 0 {
		path = o.roles.Sources[0]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		errLine("%v", err)
		return false
	}
	if old, err := os.ReadFile(path); err == nil { // keep the previous version next to it
		if err := os.WriteFile(path+".bak", old, 0o644); err != nil {
			warnLine("could not keep a backup: %v", err)
		}
	}
	if err := os.WriteFile(path, []byte(o.roles.YAML()), 0o644); err != nil {
		errLine("%v", err)
		return false
	}
	o.rec.Event("roles_saved", map[string]any{"path": path})
	okLine("wrote %s", prettyPath(path, o.jl.Root))
	return false
}

// cmdDelegate hands a task to a role by hand: the same path the lead's
// delegate tool takes — isolated worktree, verifier, diff applied on pass.
func (r *Repl) cmdDelegate(arg string) bool {
	role, task, _ := strings.Cut(strings.TrimSpace(arg), " ")
	if role == "" || strings.TrimSpace(task) == "" {
		errLine("usage: /delegate <role> <task>")
		hint("/roles lists the roles · the role's check_cmd decides when it's done")
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
