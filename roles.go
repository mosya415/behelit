package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// roles.yaml — the team definition for orchestration through the LLM gateway.
// A role is an agent bound to an ordered list of gateway models (fallback
// chain), a reasoning effort, a prompt, a tool set and a context limit. Models
// are named exactly as the gateway's /v1/models lists them — no hosts, no
// ports: LCA_BASE_URL points at the gateway and it does the routing.
//
// Read from $LCA_DIR/roles.yaml, then <root>/.lca/roles.yaml, then $LCA_ROLES
// (roles merge by name; later files win per field).
//
//	entry: lead                 # role of the REPL / one-shot session
//	transport: native           # tool calling through the gateway: native | text
//	apply: verified             # delegate diffs: verified (default) | always | never
//	remote:                     # work on another machine (outbound ssh only)
//	  host: cab-node
//	  dir: /home/u/llmbench
//	models:                     # per-model settings (gateway names)
//	  some-model: {transport: text}   # its native tool parser is broken/missing
//	defaults:
//	  context: 128000
//	  steps: 50
//	  verify_attempts: 2        # verifier failures fed back before giving up
//	  check_timeout: 600        # seconds
//	sandbox:
//	  allow: [go, git, make, pytest, python3, ls, cat, grep, bsk]
//	  shell: true             # run commands through sh: pipes and && work,
//	                          # every command in the line is still allowlisted
//	roles:
//	  lead:
//	    description: Plans, delegates, integrates.
//	    models: [kimi-k2.6, glm-5.2]
//	    effort: high
//	    tools: [read_file, grep, glob, list_dir, delegate, todowrite]
//	    context: 200000
//	    prompt: |
//	      You lead the task…
//	  coder:
//	    models: [qwen3-coder-480b, deepseek-v4-pro]
//	    effort: medium
//	    tools: [read_file, grep, glob, list_dir, edit, write, run_command]
//	    check_cmd: go test ./...
//	    prompt_file: prompts/coder.md
//	  cheap:                    # used for compaction
//	    models: [qwen3-30b-a3b]
//	    effort: off
//	    tools: []
//	    context: 32000

type RolesConfig struct {
	Entry          string
	Transport      string
	Apply          string // verified | always | never
	Allow          []string
	Shell          bool                  // run commands through sh: pipes work, every segment is still checked
	Remote         *Remote               // work on another machine over ssh
	ModelOpts      map[string]*ModelOpts // per-model settings from the model card
	VerifyAttempts int
	CheckTimeout   int
	Roles          []*Agent
	Sources        []string
	Warnings       []string
}

// ModelOpts is what roles.yaml says about one model: its transport and the
// sampling numbers from its model card. Vendor recommendations differ per
// model, and a reasoning model run at temperature 0 repeats itself, so these
// are taken from the card rather than guessed.
//
//	models:
//	  kimi-k3:     {temperature: 1.0, top_p: 0.95, effort: max}
//	  qwen3.8-27b: {temperature: 0.6, top_p: 0.95, top_k: 20}
//	  some-model:  {transport: text}   # its native tool parser is broken
type ModelOpts struct {
	Transport   string
	NoReplay    bool // reasoning_replay: off — this server won't take the field back
	Temperature *float64
	TopP        *float64
	TopK        int
	Effort      string
}

// transportOf is the configured transport for a model ("" = the default).
func (rc *RolesConfig) transportOf(model string) string {
	if o := rc.ModelOpts[model]; o != nil && o.Transport != "" {
		return o.Transport
	}
	return rc.Transport
}

func loadRoles(cfg Config) (*RolesConfig, error) {
	rc := &RolesConfig{Apply: "verified", VerifyAttempts: 2, CheckTimeout: 600, ModelOpts: map[string]*ModelOpts{}}
	paths := []string{filepath.Join(cfg.Dir, "roles.yaml"), filepath.Join(cfg.Root, ".lca", "roles.yaml")}
	if p := os.Getenv("LCA_ROLES"); p != "" {
		paths = append(paths, p)
	}
	byName := map[string]*Agent{}
	var order []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil { // the same file can be reached by two paths
			if contains(rc.Sources, abs) {
				continue
			}
			p = abs
		}
		rc.Sources = append(rc.Sources, p)
		doc := parseYAMLish(string(data))
		if v := doc.str("entry"); v != "" {
			rc.Entry = v
		}
		if v := doc.str("transport"); v != "" {
			if v != transportNative && v != transportText {
				return nil, fmt.Errorf("%s: transport must be native or text, got %q", p, v)
			}
			rc.Transport = v
		}
		if v := doc.str("apply"); v != "" {
			if v != "verified" && v != "always" && v != "never" {
				return nil, fmt.Errorf("%s: apply must be verified, always or never, got %q", p, v)
			}
			rc.Apply = v
		}
		defs := doc.child("defaults")
		defContext, _ := strconv.Atoi(defs.str("context"))
		defSteps, _ := strconv.Atoi(defs.str("steps"))
		if n, err := strconv.Atoi(defs.str("verify_attempts")); err == nil && n > 0 {
			rc.VerifyAttempts = n
		}
		if n, err := strconv.Atoi(defs.str("check_timeout")); err == nil && n > 0 {
			rc.CheckTimeout = n
		}
		if rn := doc.child("remote"); rn != nil || os.Getenv("LCA_REMOTE") != "" {
			rem, err := parseRemote(rn)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			rc.Remote = rem
		}
		if ms := doc.child("models"); ms != nil {
			for _, m := range ms.Children {
				o := rc.ModelOpts[m.Key]
				if o == nil {
					o = &ModelOpts{}
					rc.ModelOpts[m.Key] = o
				}
				if t := m.str("transport"); t != "" {
					if t != transportNative && t != transportText {
						return nil, fmt.Errorf("%s: models.%s.transport must be native or text, got %q", p, m.Key, t)
					}
					o.Transport = t
				}
				if v := m.str("temperature"); v != "" {
					f, err := strconv.ParseFloat(v, 64)
					if err != nil {
						return nil, fmt.Errorf("%s: models.%s.temperature: %w", p, m.Key, err)
					}
					o.Temperature = &f
				}
				if v := m.str("top_p"); v != "" {
					f, err := strconv.ParseFloat(v, 64)
					if err != nil {
						return nil, fmt.Errorf("%s: models.%s.top_p: %w", p, m.Key, err)
					}
					o.TopP = &f
				}
				if v := m.str("top_k"); v != "" {
					n, err := strconv.Atoi(v)
					if err != nil {
						return nil, fmt.Errorf("%s: models.%s.top_k: %w", p, m.Key, err)
					}
					o.TopK = n
				}
				if v := m.str("effort"); v != "" {
					o.Effort = v
				}
				switch m.str("reasoning_replay") {
				case "off", "none", "false":
					o.NoReplay = true
				case "", "on", "auto", "true":
				default:
					return nil, fmt.Errorf("%s: models.%s.reasoning_replay must be on or off", p, m.Key)
				}
			}
		}
		if sb := doc.child("sandbox"); sb != nil {
			if a := sb.child("allow"); a != nil {
				rc.Allow = listOrCSV(a)
			}
			if sh := sb.child("shell"); sh != nil {
				switch strings.ToLower(strings.TrimSpace(sh.Value)) {
				case "", "true", "yes", "on":
					rc.Shell = true
				case "false", "no", "off":
					rc.Shell = false
				default:
					return nil, fmt.Errorf("%s: sandbox.shell must be true or false", p)
				}
			}
		}

		roles := doc.child("roles")
		if roles == nil {
			continue
		}
		for _, rn := range roles.Children {
			a := byName[rn.Key]
			if a == nil {
				a = &Agent{Name: rn.Key, Mode: "all", Source: p, IsRole: true, Context: defContext, Steps: defSteps}
				byName[rn.Key] = a
				order = append(order, rn.Key)
			}
			if err := applyRole(a, rn, filepath.Dir(p)); err != nil {
				return nil, fmt.Errorf("%s: role %s: %w", p, rn.Key, err)
			}
		}
	}
	for _, n := range order {
		a := byName[n]
		if len(a.Models) == 0 {
			return nil, fmt.Errorf("role %s: models is required (names from the gateway's /v1/models)", n)
		}
		rc.Roles = append(rc.Roles, a)
	}
	for _, a := range rc.Roles {
		for _, m := range a.Models[1:] {
			if rc.transportOf(m) != rc.transportOf(a.Models[0]) {
				rc.Warnings = append(rc.Warnings, fmt.Sprintf("role %s mixes tool transports (%s: %s, %s: %s) — a session keeps the transport of the first model, even after a fallback",
					a.Name, a.Models[0], rc.transportOf(a.Models[0]), m, rc.transportOf(m)))
				break
			}
		}
	}
	if rc.Entry != "" && byName[rc.Entry] == nil {
		return nil, fmt.Errorf("entry role %q is not defined", rc.Entry)
	}
	return rc, nil
}

func listOrCSV(n *yNode) []string {
	items := n.List
	if len(items) == 0 && n.Value != "" {
		items = strings.Split(n.Value, ",")
	}
	var out []string
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" {
			out = append(out, it)
		}
	}
	return out
}

func applyRole(a *Agent, n *yNode, baseDir string) error {
	if v := n.str("description"); v != "" {
		a.Description = v
	}
	if m := n.child("models"); m != nil {
		a.Models = nil
		for _, name := range listOrCSV(m) {
			if strings.Contains(name, "://") || strings.Count(name, ":") > 0 && strings.Count(name, ".") >= 3 {
				return fmt.Errorf("model %q looks like an address — use the name from the gateway's /v1/models", name)
			}
			a.Models = append(a.Models, name)
		}
	}
	if v := n.str("effort"); v != "" {
		switch v {
		case "off", "none", "on", "low", "medium", "high", "max", "xhigh":
			a.Thinking = v
		default:
			return fmt.Errorf("effort must be off|on|low|medium|high|max, got %q", v)
		}
	}
	if v := n.str("prompt"); v != "" {
		a.Prompt = v
	}
	if v := n.str("prompt_file"); v != "" {
		p := v
		if !filepath.IsAbs(p) {
			p = filepath.Join(baseDir, p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("prompt_file: %w", err)
		}
		a.Prompt = strings.TrimSpace(string(data))
	}
	if t := n.child("tools"); t != nil {
		a.Tools = listOrCSV(t)
		a.ToolsSet = true
		for _, name := range a.Tools {
			if toolRegistry[name] == nil {
				return fmt.Errorf("unknown tool %q (known: %s)", name, strings.Join(sortedKeys(toolRegistry), ", "))
			}
		}
	}
	if v := n.str("context"); v != "" {
		c, err := strconv.Atoi(v)
		if err != nil || c <= 0 {
			return fmt.Errorf("context must be a positive token count, got %q", v)
		}
		a.Context = c
	}
	if v := n.str("steps"); v != "" {
		if c, err := strconv.Atoi(v); err == nil && c > 0 {
			a.Steps = c
		}
	}
	if v := n.str("temperature"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			a.Temperature = &f
		}
	}
	if v := n.str("check_cmd"); v != "" {
		a.CheckCmd = v
	}
	if v := n.str("mode"); v != "" {
		a.Mode = v
	}
	return nil
}

// validateRoleModels checks every role's models against the gateway's
// /v1/models. Unknown names are dropped from the chain (with a warning) unless
// that would empty it; an unreachable gateway only warns.
func validateRoleModels(rc *RolesConfig, gw *Client) ([]string, int) {
	models, err := gw.ListModels()
	if err != nil {
		return []string{fmt.Sprintf("gateway unreachable at %s — role models not checked (lca doctor)", hostOf(gw.Endpoint()))}, 0
	}
	served := map[string]bool{}
	for _, m := range models {
		served[m.ID] = true
	}
	var warns []string
	for _, r := range rc.Roles {
		var keep, unknown []string
		for _, m := range r.Models {
			if served[m] {
				keep = append(keep, m)
			} else {
				unknown = append(unknown, m)
			}
		}
		if len(unknown) == 0 {
			continue
		}
		if len(keep) == 0 {
			warns = append(warns, fmt.Sprintf("role %s: none of %s is listed by the gateway — keeping them (it may be reloading)", r.Name, strings.Join(unknown, ", ")))
			continue
		}
		r.Models = keep
		warns = append(warns, fmt.Sprintf("role %s: %s not listed by the gateway — dropped from the chain", r.Name, strings.Join(unknown, ", ")))
	}
	return warns, len(models)
}

// roleNames lists roles for display / delegate descriptions.
func (o *Orchestrator) roleList(exclude string) []*Agent {
	var out []*Agent
	for _, a := range o.agents {
		if a.IsRole && a.Name != exclude && a.Name != "cheap" {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// YAML renders the team as roles.yaml — what /role save writes, so a team put
// together in a session can be kept.
func (rc *RolesConfig) YAML() string {
	var b strings.Builder
	b.WriteString("# Team for lca. Written by /role save; edit freely.\n\n")
	if rc.Entry != "" {
		fmt.Fprintf(&b, "entry: %s\n", rc.Entry)
	}
	if rc.Transport != "" {
		fmt.Fprintf(&b, "transport: %s\n", rc.Transport)
	}
	if rc.Apply != "" {
		fmt.Fprintf(&b, "apply: %s\n", rc.Apply)
	}
	if rc.Remote != nil {
		fmt.Fprintf(&b, "\nremote:\n  host: %s\n  dir: %s\n", rc.Remote.Host, rc.Remote.Dir)
		if len(rc.Remote.SSH) > 0 {
			fmt.Fprintf(&b, "  ssh: [%s]\n", strings.Join(rc.Remote.SSH, ", "))
		}
	}
	fmt.Fprintf(&b, "\ndefaults:\n  verify_attempts: %d\n  check_timeout: %d\n", rc.VerifyAttempts, rc.CheckTimeout)
	if len(rc.Allow) > 0 || rc.Shell {
		b.WriteString("\nsandbox:\n")
		if len(rc.Allow) > 0 {
			fmt.Fprintf(&b, "  allow: [%s]\n", strings.Join(rc.Allow, ", "))
		}
		if rc.Shell {
			b.WriteString("  shell: true\n")
		}
	}
	if len(rc.ModelOpts) > 0 {
		b.WriteString("\nmodels:\n")
		for _, name := range sortedKeys(rc.ModelOpts) {
			o := rc.ModelOpts[name]
			var parts []string
			if o.Transport != "" {
				parts = append(parts, "transport: "+o.Transport)
			}
			if o.Temperature != nil {
				parts = append(parts, "temperature: "+strconv.FormatFloat(*o.Temperature, 'f', -1, 64))
			}
			if o.TopP != nil {
				parts = append(parts, "top_p: "+strconv.FormatFloat(*o.TopP, 'f', -1, 64))
			}
			if o.TopK > 0 {
				parts = append(parts, "top_k: "+strconv.Itoa(o.TopK))
			}
			if o.Effort != "" {
				parts = append(parts, "effort: "+o.Effort)
			}
			if o.NoReplay {
				parts = append(parts, "reasoning_replay: off")
			}
			if len(parts) > 0 {
				fmt.Fprintf(&b, "  %s: {%s}\n", name, strings.Join(parts, ", "))
			}
		}
	}
	b.WriteString("\nroles:\n")
	for _, a := range rc.Roles {
		fmt.Fprintf(&b, "  %s:\n", a.Name)
		if a.Description != "" {
			fmt.Fprintf(&b, "    description: %s\n", a.Description)
		}
		if len(a.Models) > 0 {
			fmt.Fprintf(&b, "    models: [%s]\n", strings.Join(a.Models, ", "))
		}
		if a.Thinking != "" {
			fmt.Fprintf(&b, "    effort: %s\n", a.Thinking)
		}
		if a.Temperature != nil {
			fmt.Fprintf(&b, "    temperature: %s\n", strconv.FormatFloat(*a.Temperature, 'f', -1, 64))
		}
		if a.TopP != nil {
			fmt.Fprintf(&b, "    top_p: %s\n", strconv.FormatFloat(*a.TopP, 'f', -1, 64))
		}
		if a.Context > 0 {
			fmt.Fprintf(&b, "    context: %d\n", a.Context)
		}
		if a.Steps > 0 {
			fmt.Fprintf(&b, "    steps: %d\n", a.Steps)
		}
		if a.CheckCmd != "" {
			fmt.Fprintf(&b, "    check_cmd: %s\n", a.CheckCmd)
		}
		if a.ToolsSet {
			fmt.Fprintf(&b, "    tools: [%s]\n", strings.Join(a.Tools, ", "))
		}
		if p := strings.TrimRight(a.Prompt, "\n"); p != "" {
			b.WriteString("    prompt: |\n")
			for _, l := range strings.Split(p, "\n") {
				b.WriteString("      " + l + "\n")
			}
		}
	}
	return b.String()
}
