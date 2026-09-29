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
//	members:                    # a fleet: one entry per machine (members.go)
//	  build-box: {host: build01, dir: /srv/work/llmbench}
//	  gpu-0:
//	    host: gpu07
//	    dir: /scratch/llmbench
//	    allow: [ls, cat, python3, bsk]   # this member's sandbox
//	models:                     # per-model settings (gateway names)
//	  some-model: {transport: text}   # its native tool parser is broken/missing
//	tiers:                      # named chains: develop on premium, operate on cheap
//	  cheap:   [qwen3-30b-a3b]
//	  premium: [kimi-k2.6, glm-5.2]
//	defaults:
//	  member: build-box         # the member roles use when they name none
//	  context: 128000
//	  steps: 50
//	  verify_attempts: 2        # verifier failures fed back before giving up
//	  check_timeout: 600        # seconds
//	  review: reviewer          # team-wide reviewer for passed delegations
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
//	    member: build-box       # this role's sessions work on that machine
//	    models: [qwen3-coder-480b, deepseek-v4-pro]  # or: tier: premium
//	    review: reviewer        # a second opinion on a diff the verifier passed
//	    fork: true              # start from the caller's reads, not a blank context
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
	Remote         *Remote               // work on another machine over ssh (the "remote" member)
	Members        map[string]*Member    // the fleet; always contains "local"
	MemberOrder    []string              // declaration order, for /members, doctor and YAML()
	DefaultMember  string                // defaults: member:
	HasMembers     bool                  // a members: block was declared somewhere
	ModelOpts      map[string]*ModelOpts // per-model settings from the model card
	VerifyAttempts int
	CheckTimeout   int
	Review         string              // defaults.review: the reviewer for roles that name none
	Tiers          map[string][]string // tiers: — named model chains, validated like a role's
	TierOrder      []string            // declaration order, for messages and YAML()
	Tier           string              // the active tier (cfg.Tier): -tier / LCA_TIER
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

// modelOpts is roles.yaml's block for a served model. The exact key is tried
// first — an operator can always pin the literal served id, whatever spelling the
// gateway uses — and then the normalised one, so a file written as
// "models: {glm-5.3: …}" still reaches a model served as "glm5.3". lca doctor
// warns about a models: key that matches nothing served.
func (rc *RolesConfig) modelOpts(model string) *ModelOpts {
	if rc == nil {
		return nil
	}
	if o := rc.ModelOpts[model]; o != nil {
		return o
	}
	norm := normalizeModelID(model)
	for k, o := range rc.ModelOpts {
		if normalizeModelID(k) == norm {
			return o
		}
	}
	return nil
}

// transportOf is the configured transport for a model ("" = the default).
func (rc *RolesConfig) transportOf(model string) string {
	if o := rc.modelOpts(model); o != nil && o.Transport != "" {
		return o.Transport
	}
	return rc.Transport
}

func loadRoles(cfg Config) (*RolesConfig, error) {
	rc := &RolesConfig{Apply: "verified", VerifyAttempts: 2, CheckTimeout: 600, ModelOpts: map[string]*ModelOpts{},
		Members: map[string]*Member{localMemberName: {Name: localMemberName}}}
	// Which spelling the other machine came from decides whether cross-machine
	// delegation is available and what a round trip through YAML() writes back,
	// and members: may sit in a different merged file than remote:.
	remoteBlock, envRemote := false, strings.TrimSpace(os.Getenv("LCA_REMOTE")) != ""
	lastPath := ""
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
		lastPath = p
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
		if v := defs.str("review"); v != "" {
			rc.Review = v
		}
		if v := defs.str("member"); v != "" {
			rc.DefaultMember = v
		}
		if rn := doc.child("remote"); rn != nil || envRemote {
			if rn != nil {
				remoteBlock = true
			}
			rem, err := parseRemote(rn)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			if rem != nil && rem.Src == "" {
				if rn != nil {
					rem.Src = p // the remote: block in this file
				} else {
					rem.Src = "LCA_REMOTE"
				}
			}
			rc.Remote = rem
		}
		if mn := doc.child("members"); mn != nil {
			ms, order, err := parseMembers(mn, p)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			rc.HasMembers = true
			for _, n := range order {
				if rc.Members[n] == nil || !contains(rc.MemberOrder, n) {
					rc.MemberOrder = append(rc.MemberOrder, n)
				}
				rc.Members[n] = ms[n] // a later file replaces the member wholesale
			}
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
		if ts := doc.child("tiers"); ts != nil {
			if rc.Tiers == nil {
				rc.Tiers = map[string][]string{}
			}
			for _, tn := range ts.Children {
				names := listOrCSV(tn)
				if len(names) == 0 {
					return nil, fmt.Errorf("%s: tiers.%s: is empty — a tier is an ordered chain of gateway model names", p, tn.Key)
				}
				for _, m := range names {
					if err := checkModelName(m); err != nil {
						return nil, fmt.Errorf("%s: tiers.%s: %w", p, tn.Key, err)
					}
				}
				if rc.Tiers[tn.Key] == nil {
					rc.TierOrder = append(rc.TierOrder, tn.Key)
				}
				rc.Tiers[tn.Key] = names
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
	rc.Tier = cfg.Tier
	if rc.Tier != "" {
		if len(rc.Tiers) == 0 {
			return nil, fmt.Errorf("-tier %s: this team has no tiers: block (looked in %s)", rc.Tier, orNoFiles(rc.Sources))
		}
		if _, ok := rc.Tiers[rc.Tier]; !ok {
			return nil, fmt.Errorf("tier %q is not defined (tiers: %s)", rc.Tier, rc.tierList())
		}
	}
	remapped := 0
	var pinned []string
	for _, n := range order {
		a := byName[n]
		if a.Tier != "" {
			if _, ok := rc.Tiers[a.Tier]; !ok {
				if len(rc.Tiers) == 0 {
					return nil, fmt.Errorf("role %s: tier: %q, but this team has no tiers: block in roles.yaml", n, a.Tier)
				}
				return nil, fmt.Errorf("role %s: tier: %q is not defined (tiers: %s)", n, a.Tier, rc.tierList())
			}
			rc.applyTier(a)
			remapped++
		} else {
			pinned = append(pinned, n)
		}
		if len(a.Models) == 0 {
			// tier: is only worth offering as the alternative when the team
			// declares one — "or tier: one of none declared" points at nothing.
			if len(rc.Tiers) > 0 {
				return nil, fmt.Errorf("role %s: models is required (names from the gateway's /v1/models), or tier: one of %s", n, rc.tierList())
			}
			return nil, fmt.Errorf("role %s: models is required (names from the gateway's /v1/models)", n)
		}
		rc.Roles = append(rc.Roles, a)
	}
	if rc.Tier != "" && remapped == 0 {
		// The active tier is reported by /agents, doctor and every trace record,
		// so a selection that moved no role at all has to say so: the alternative
		// is a run that looks like it switched and did not.
		rc.Warnings = append(rc.Warnings, fmt.Sprintf(
			"-tier %s changed nothing: no role declares tier: — %s each name their own models:",
			rc.Tier, strings.Join(pinned, ", ")))
	}
	// The fleet resolves only once every file has merged, for the same reason
	// reviewers do: members: and remote: may live in different files, and the
	// name collision between them can only be seen once both are known.
	if rc.Remote != nil {
		rc.Remote.Name = legacyMemberName
		switch declared := rc.Members[legacyMemberName]; {
		case declared != nil && declared.Rem != nil && remoteBlock:
			return nil, fmt.Errorf(`%s: members.remote and the remote: block both define a member named "remote" — keep one (remote: is the old single-member spelling)`, lastPath)
		case declared != nil && declared.Rem != nil:
			// LCA_REMOTE wins over the file, the way it always has, but a fleet
			// that names its machines must be told which one it just repointed.
			rc.Warnings = append(rc.Warnings, fmt.Sprintf(
				"LCA_REMOTE overrides members.remote (%s) and is the team's default member", rc.Remote.Label()))
			declared.Rem = rc.Remote
			rc.DefaultMember = legacyMemberName
		default:
			rc.Members[legacyMemberName] = &Member{Name: legacyMemberName, Rem: rc.Remote, fromRemote: true}
			if !contains(rc.MemberOrder, legacyMemberName) {
				rc.MemberOrder = append(rc.MemberOrder, legacyMemberName)
			}
			if envRemote || rc.DefaultMember == "" {
				rc.DefaultMember = legacyMemberName
			}
		}
	}
	if rc.DefaultMember != "" && rc.Members[rc.DefaultMember] == nil {
		return nil, fmt.Errorf("%s: defaults: member: %q is not a member (members: %s)", lastPath, rc.DefaultMember, strings.Join(rc.memberNames(), ", "))
	}
	for _, a := range rc.Roles {
		if a.Member != "" && rc.Members[a.Member] == nil {
			return nil, fmt.Errorf("%s: role %s: member: %q is not a member (members: %s)", lastPath, a.Name, a.Member, strings.Join(rc.memberNames(), ", "))
		}
	}
	// Reviewers resolve only once every file has merged: a merged team's
	// reviewer can come from another file, so this is not checked in applyRole.
	defined := func(name string) *Agent {
		if a := byName[name]; a != nil && a.IsRole {
			return a
		}
		return nil
	}
	names := strings.Join(order, ", ")
	if rc.Review != "" && !isNone(rc.Review) && defined(rc.Review) == nil {
		return nil, fmt.Errorf("defaults: review: %q is not a role (defined: %s)", rc.Review, names)
	}
	for _, a := range rc.Roles {
		if a.Review != "" && !isNone(a.Review) {
			if a.Review == a.Name {
				return nil, fmt.Errorf("role %s: review: a role cannot review itself", a.Name)
			}
			if defined(a.Review) == nil {
				return nil, fmt.Errorf("role %s: review: %q is not a role (defined: %s)", a.Name, a.Review, names)
			}
		}
		// reviewerFor's resolution, not a.Review alone: one reviewer named under
		// defaults: covers the whole team, which is exactly where a family
		// collision is most likely and where the warning is worth the most.
		rev := rc.reviewerOf(a, defined)
		if rev == nil {
			continue
		}
		if sameFamily(a, rev) {
			rc.Warnings = append(rc.Warnings, fmt.Sprintf(
				"role %s: its reviewer %s runs %s, the same family as %s — a same-family second opinion shares the blind spots the review exists to find",
				a.Name, rev.Name, rev.Models[0], a.Models[0]))
		}
		// The reviewer reads the worktree, so it runs where the worktree is,
		// whatever its own member: says. A silent move is worse than a warning.
		if am, rm := rc.memberOf(a), rc.memberOf(rev); am != rm {
			rc.Warnings = append(rc.Warnings, fmt.Sprintf(
				"role %s: its reviewer %s is pinned to member %s, but a review happens inside the worktree on %s — the reviewer will run there",
				a.Name, rev.Name, rm, am))
		}
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

// checkModelName rejects an endpoint where a gateway model name belongs: the
// gateway does the routing, roles.yaml only names models.
func checkModelName(name string) error {
	if strings.Contains(name, "://") || strings.Count(name, ":") > 0 && strings.Count(name, ".") >= 3 {
		return fmt.Errorf("model %q looks like an address — use the name from the gateway's /v1/models", name)
	}
	return nil
}

// isNone is how roles.yaml switches an inherited setting off by name.
func isNone(s string) bool { return s == "none" || s == "off" || s == "-" }

// reviewerOf resolves a role's effective reviewer against a lookup — the role's
// own review:, else defaults: review: — in the same order Orchestrator.reviewerFor
// uses at runtime, so what the loader warns about is what will actually run.
func (rc *RolesConfig) reviewerOf(a *Agent, lookup func(string) *Agent) *Agent {
	name := a.Review
	if name == "" {
		name = rc.Review
	}
	if name == "" || isNone(name) {
		return nil
	}
	rev := lookup(name)
	if rev == a { // the team default cannot make a role review itself
		return nil
	}
	return rev
}

// memberOf is the member name a role's sessions resolve to at load time, in the
// same order Orchestrator.memberFor uses at runtime, so what the loader warns
// about is what will actually run.
func (rc *RolesConfig) memberOf(a *Agent) string {
	return firstNonEmpty(a.Member, rc.DefaultMember, localMemberName)
}

// memberNames lists the fleet for a load-time message, through the same helper
// /members and doctor list it with.
func (rc *RolesConfig) memberNames() []string {
	return fleetNames(rc.MemberOrder, rc.Remote != nil)
}

// sameFamily reports whether two roles' first models come from one family per
// models.go. A gateway name no profile rule recognises has no family, and two
// unrecognised names are no evidence of anything: warning on them would bury the
// case the warning exists for under noise on every fleet with its own names.
func sameFamily(a, b *Agent) bool {
	if len(a.Models) == 0 || len(b.Models) == 0 {
		return false
	}
	fa := lookupProfile(a.Models[0]).Family
	return fa != "" && fa == lookupProfile(b.Models[0]).Family
}

// orNoFiles names the empty search: a message about a missing key has to be able
// to say that no file was found to hold it.
func orNoFiles(paths []string) string {
	if len(paths) == 0 {
		return "no roles.yaml"
	}
	return strings.Join(paths, ", ")
}

// applyTier points a role at a tier's chain: the tier it declares, or the active
// tier when the run selected one. A tier is a chain, not a profile — whatever it
// resolves to still picks up its own settings under models:.
func (rc *RolesConfig) applyTier(a *Agent) {
	name := a.Tier
	if rc.Tier != "" {
		name = rc.Tier
	}
	a.Models = append([]string(nil), rc.Tiers[name]...)
}

// role is the named role, or nil. /setup's review screen reads the team it is
// about to write through this, so the screen and the file cannot disagree.
func (rc *RolesConfig) role(name string) *Agent {
	for _, a := range rc.Roles {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// tierList names the declared tiers in declaration order.
func (rc *RolesConfig) tierList() string {
	if len(rc.TierOrder) == 0 {
		return "none declared"
	}
	return strings.Join(rc.TierOrder, ", ")
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
	// models: and tier: each replace the other, the way a second models: replaces
	// the first: layered files exist so a project can repoint a role the global
	// team declared, and a merged role carrying both would refuse to load at all.
	// Both keys in ONE node is still the authoring mistake it always was.
	if m := n.child("models"); m != nil {
		if v := n.str("tier"); v != "" {
			return fmt.Errorf("tier: %s and models: are both set — a role names a tier or its own chain, not both", v)
		}
		a.Models, a.Tier = nil, ""
		for _, name := range listOrCSV(m) {
			if err := checkModelName(name); err != nil {
				return err
			}
			a.Models = append(a.Models, name)
		}
	}
	if v := n.str("tier"); v != "" {
		a.Tier, a.Models = v, nil
	}
	// No validation here: a reviewer's name resolves only once every file has
	// merged (loadRoles does it). A member: is the same — it may be declared in
	// another merged file.
	if v := n.str("review"); v != "" {
		a.Review = v
	}
	if v := n.str("member"); v != "" {
		a.Member = v
	}
	if f := n.child("fork"); f != nil {
		switch strings.ToLower(strings.TrimSpace(f.Value)) {
		case "true", "yes", "on":
			a.Fork = true
		case "", "false", "no", "off":
			a.Fork = false
		default:
			return fmt.Errorf("fork must be true or false, got %q", f.Value)
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
	// A tier is warned about once, not once per role that names it.
	for _, t := range rc.TierOrder {
		var keep, unknown []string
		for _, m := range rc.Tiers[t] {
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
			warns = append(warns, fmt.Sprintf("tier %s: none of %s is listed by the gateway — keeping them (it may be reloading)", t, strings.Join(unknown, ", ")))
			continue
		}
		rc.Tiers[t] = keep
		warns = append(warns, fmt.Sprintf("tier %s: %s not listed by the gateway — dropped from the chain", t, strings.Join(unknown, ", ")))
	}
	for _, r := range rc.Roles {
		if r.Tier != "" {
			rc.applyTier(r)
			continue
		}
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

// remoteIsItsOwnSpelling reports that the member named "remote" exists only
// because of the remote: block or LCA_REMOTE. YAML() then writes it back in that
// spelling and leaves it out of members:, because a file carrying both is one the
// loader refuses. A DECLARED members.remote goes under members: instead, with its
// own allow:/shell: — dropping it there is how /role save silently widened a
// member's sandbox.
func (rc *RolesConfig) remoteIsItsOwnSpelling() bool {
	m := rc.Members[legacyMemberName]
	return m == nil || m.fromRemote
}

// YAML renders the team as roles.yaml — what /role save writes, so a team put
// together in a session can be kept.
func (rc *RolesConfig) YAML() string {
	var b strings.Builder
	b.WriteString("# Team for lca. Written from a session (/setup or /role save); edit freely.\n\n")
	if rc.Entry != "" {
		fmt.Fprintf(&b, "entry: %s\n", rc.Entry)
	}
	if rc.Transport != "" {
		fmt.Fprintf(&b, "transport: %s\n", rc.Transport)
	}
	if rc.Apply != "" {
		fmt.Fprintf(&b, "apply: %s\n", rc.Apply)
	}
	if rc.Remote != nil && rc.remoteIsItsOwnSpelling() {
		fmt.Fprintf(&b, "\nremote:\n  host: %s\n  dir: %s\n", rc.Remote.Host, rc.Remote.Dir)
		if len(rc.Remote.SSH) > 0 {
			fmt.Fprintf(&b, "  ssh: [%s]\n", strings.Join(rc.Remote.SSH, ", "))
		}
	}
	// The fleet. A member that came from a legacy remote: block is written back
	// as remote: above, not moved into members:, so a round trip does not
	// silently rewrite the operator's file into the new form.
	var mem strings.Builder
	for _, n := range rc.MemberOrder {
		m := rc.Members[n]
		switch {
		case m == nil:
			continue
		case n == legacyMemberName && rc.remoteIsItsOwnSpelling():
			continue // written as the remote: block above
		case n == localMemberName && m.Allow == nil && m.Shell == nil:
			continue // declaring local says nothing unless it scopes the sandbox
		}
		fmt.Fprintf(&mem, "  %s:\n", n)
		if m.Rem != nil {
			if m.Rem.Host != "" {
				fmt.Fprintf(&mem, "    host: %s\n", m.Rem.Host)
			}
			fmt.Fprintf(&mem, "    dir: %s\n", m.Rem.Dir)
			if len(m.Rem.SSH) > 0 {
				fmt.Fprintf(&mem, "    ssh: [%s]\n", strings.Join(m.Rem.SSH, ", "))
			}
		}
		if len(m.Allow) > 0 {
			fmt.Fprintf(&mem, "    allow: [%s]\n", strings.Join(m.Allow, ", "))
		}
		if m.Shell != nil {
			fmt.Fprintf(&mem, "    shell: %t\n", *m.Shell)
		}
	}
	if mem.Len() > 0 {
		b.WriteString("\nmembers:\n" + mem.String())
	}
	fmt.Fprintf(&b, "\ndefaults:\n  verify_attempts: %d\n  check_timeout: %d\n", rc.VerifyAttempts, rc.CheckTimeout)
	if rc.Review != "" {
		fmt.Fprintf(&b, "  review: %s\n", rc.Review)
	}
	if rc.DefaultMember != "" && !(rc.DefaultMember == legacyMemberName && rc.remoteIsItsOwnSpelling()) {
		fmt.Fprintf(&b, "  member: %s\n", rc.DefaultMember)
	}
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
	if len(rc.TierOrder) > 0 {
		// The run's -tier is deliberately not written back: it is an operating
		// choice, and registration is what this file records.
		b.WriteString("\ntiers:\n")
		for _, t := range rc.TierOrder {
			fmt.Fprintf(&b, "  %s: [%s]\n", t, strings.Join(rc.Tiers[t], ", "))
		}
	}
	b.WriteString("\nroles:\n")
	for _, a := range rc.Roles {
		fmt.Fprintf(&b, "  %s:\n", a.Name)
		if a.Description != "" {
			fmt.Fprintf(&b, "    description: %s\n", a.Description)
		}
		if a.Tier != "" {
			fmt.Fprintf(&b, "    tier: %s\n", a.Tier)
		} else if len(a.Models) > 0 {
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
		if a.Review != "" {
			fmt.Fprintf(&b, "    review: %s\n", a.Review)
		}
		if a.Member != "" {
			fmt.Fprintf(&b, "    member: %s\n", a.Member)
		}
		if a.Fork {
			b.WriteString("    fork: true\n")
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
