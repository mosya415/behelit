package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// lca init and lca doctor: the first-run path. init writes a working
// .lca/roles.yaml from what the gateway actually serves and what the project
// is built with; doctor checks the whole chain end to end — gateway, roles,
// a real tool call on every role's model, the workspace and the sandbox — and
// says what to fix.

// ── lca init ────────────────────────────────────────────────────────────────

// memberFlags collects repeated -member name=host:/dir so a whole fleet can be
// written in one command. The parse is strict: a mistyped member would write a
// roles.yaml that fails to load, and the failure would be nowhere near the
// command that caused it.
type memberFlags struct {
	list []struct{ name, host, dir string }
}

func (m *memberFlags) String() string {
	var out []string
	for _, e := range m.list {
		out = append(out, e.name+"="+e.host+":"+e.dir)
	}
	return strings.Join(out, ",")
}

func (m *memberFlags) Set(v string) error {
	name, target, ok := strings.Cut(v, "=")
	name = strings.TrimSpace(name)
	host, dir, ok2 := strings.Cut(target, ":")
	if !ok || name == "" || !ok2 || host == "" || !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("-member wants name=host:/absolute/path, got %q", v)
	}
	if !reMemberName.MatchString(name) {
		return fmt.Errorf("-member %q: a member name is lower-case letters, digits, - and _", name)
	}
	if name == localMemberName {
		return fmt.Errorf("-member local=… is not allowed: local always means this machine")
	}
	for _, e := range m.list {
		if e.name == name {
			return fmt.Errorf("-member %s given twice", name)
		}
	}
	m.list = append(m.list, struct{ name, host, dir string }{name, host, dir})
	return nil
}

func runInit(cfg Config, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing roles.yaml")
	remote := fs.String("remote", "", "work on another machine over ssh: host:/path/to/project")
	members := &memberFlags{}
	fs.Var(members, "member", "a machine the team works on: name=host:/path/to/project (repeatable)")
	out := fs.String("o", filepath.Join(".lca", "roles.yaml"), "where to write")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path := *out
	if !filepath.IsAbs(path) {
		path = filepath.Join(cfg.Root, path)
	}
	if _, err := os.Stat(path); err == nil && !*force {
		errLine("%s already exists", prettyPath(path, cfg.Root))
		hint("%s", "lca init -force overwrites it"+gSep+"lca doctor checks it")
		return 1
	}

	gw := NewClient(cfg)
	applyRememberedWindow(cfg, gw)
	models, err := gw.ListModels()
	if err != nil {
		errLine("can't list models at %s: %v", gw.Endpoint(), err)
		hint("in a session: lca, then /setup asks for the gateway and shows what it serves")
		hint("non-interactive: export LCA_BASE_URL=http://node:18080/v1, or put base_url in .lca/config.json")
		return 1
	}
	var names []string
	for _, m := range models {
		names = append(names, m.ID)
	}
	if len(names) == 0 {
		errLine("the gateway at %s lists no models", hostOf(gw.Endpoint()))
		return 1
	}
	var remoteBlock, defaultsMember string
	switch {
	case *remote != "" && len(members.list) > 0:
		errLine("-remote and -member both given: -remote writes the single-member block, -member writes a members: fleet — pick one")
		return 1
	case *remote != "":
		host, rdir, ok := strings.Cut(*remote, ":")
		if !ok || host == "" || !strings.HasPrefix(rdir, "/") {
			errLine("-remote wants host:/absolute/path, got %q", *remote)
			return 1
		}
		remoteBlock = fmt.Sprintf("# the project lives on another machine; ssh is outbound only, no ports opened\nremote:\n  host: %s\n  dir: %s\n\n", host, rdir)
	case len(members.list) > 0:
		var b strings.Builder
		b.WriteString("# the machines this team works on; ssh is outbound only, no ports opened.\n# pin a role to one with \"member: <name>\" under it.\nmembers:\n")
		for _, m := range members.list {
			fmt.Fprintf(&b, "  %s:\n    host: %s\n    dir: %s\n", m.name, m.host, m.dir)
		}
		b.WriteString("\n")
		remoteBlock = b.String()
		// One member is unambiguous: it is where the team works. Several are a
		// fleet, and guessing which role belongs where would be worse than saying
		// nothing.
		if len(members.list) == 1 {
			defaultsMember = "  member: " + members.list[0].name + "\n"
		}
	}
	lead := pickModels(names, leadPref, 2)
	coder := pickModels(names, coderPref, 2)
	cheap := pickCheap(names)
	check, allow := detectToolchain(cfg.Root)

	var b strings.Builder
	fmt.Fprintf(&b, `# Team for %s — written by lca init from %s/models.
# Model names are the gateway's; order = fallback chain. Edit freely;
# lca doctor checks it (including a real tool call on the first model of each
# role; lca doctor -all probes every model in every chain).

entry: lead
transport: native      # the models' own tool-call format (engines need their tool-call parser)
apply: verified        # a delegate's diff reaches your tree only when its check passed
#                      # verified | always | never | branch (README: apply: branch)

%s# models:               # per-model settings, e.g. a model whose native tool parser is broken:
#   some-model: {transport: text}

defaults:
%s  context: 128000
  verify_attempts: 2
  check_timeout: 900

sandbox:
  allow: [%s]
  # shell: true   # run commands through sh (pipes, redirects, &&); every
  #               # command in the line is still checked against allow

roles:
  lead:
    description: Understands the task, splits it, delegates, integrates and reports.
    models: [%s]
    effort: high
    tools: [list_dir, glob, grep, read_file, todowrite, delegate, run_command]
    prompt: |
      You lead a software task. Read enough code to understand it, then split it
      into self-contained changes and delegate each to the coder role with a
      precise task and a narrow check_cmd that fails before and passes after.
      Launch independent delegations in one reply. A passed diff is already
      applied; on failed, sharpen the task or split it. Finish with a short
      report of what changed and what was verified.

  coder:
    description: Implements one well-specified change and makes its check pass.
    models: [%s]
    effort: medium
    tools: [list_dir, glob, grep, read_file, edit, write, run_command]
%s    prompt: |
      You implement exactly one change in an isolated worktree. Read the
      relevant code first, make the smallest correct change, and run the check
      command yourself until it passes. Don't refactor unrelated code.

%s`, filepath.Base(cfg.Root), strings.TrimRight(gw.Endpoint(), "/"), remoteBlock, defaultsMember, strings.Join(allow, ", "),
		strings.Join(lead, ", "), strings.Join(coder, ", "), checkLine(check), cheapBlock(cheap))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		errLine("%v", err)
		return 1
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		errLine("%v", err)
		return 1
	}
	section("lca init")
	okLine("wrote %s", prettyPath(path, cfg.Root))
	for _, m := range members.list {
		row("member", m.name+faint(gSep+"%s:%s", m.host, m.dir))
	}
	if len(members.list) > 1 {
		hint("pin a role with \"member: <name>\" under it")
	}
	rows := [][]string{
		{"lead", strings.Join(lead, faint(" %s ", gFlow)), faint("%s", gNil)},
		{"coder", strings.Join(coder, faint(" %s ", gFlow)), faint("%s", firstNonEmpty(check, "— add check_cmd"))},
	}
	if cheap != "" {
		rows = append(rows, []string{"cheap", cheap, faint("%s", gNil)})
	}
	// role and models are identifiers: they cut in the middle, where both ends survive
	table([]string{"role", "models", "check"}, rows, 0, 1)
	if cheap == "" {
		hint("no small model is served, so no cheap role was written — compaction runs on the lead's model")
	}
	row("sandbox", faint("%s", strings.Join(allow, " ")))
	if *remote != "" {
		row("remote", *remote+faint("%s", gSep+"files and commands go there over ssh"))
	}
	fmt.Println()
	hint("next: lca doctor — the gateway, the roles and one real tool call per role (-all: every model)")
	return 0
}

func checkLine(check string) string {
	if check == "" {
		return "    # check_cmd: <command that proves a change works, e.g. make test>\n"
	}
	return "    check_cmd: " + check + "\n"
}

// Preference lists: first substring match wins; within a family the order
// the gateway lists them is kept.
var (
	leadPref  = []string{"kimi", "glm-5", "deepseek-v4-pro", "deepseek", "glm", "qwen3.7-max", "qwen3-max", "minimax", "hy3", "qwen"}
	coderPref = []string{"coder", "deepseek-v4", "glm-5", "kimi", "minimax", "deepseek", "qwen", "glm", "hy3"}
	smallHint = []string{"flash", "mini", "air", "lite", "small", "turbo", "nano", "tiny"}
)

func isSmall(m string) bool {
	// Per TOKEN, never as a raw substring: "mini" inside MiniMax made a 428B
	// flagship look like the cheap model, which then lost it as a candidate for
	// lead and coder. A parameter count is its own rule rather than a list of
	// spellings, so 27b and 32b are recognised without being enumerated.
	biggest := 0
	for _, t := range strings.FieldsFunc(strings.ToLower(m), func(r rune) bool {
		return r != '.' && (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if contains(smallHint, t) {
			return true
		}
		if n := paramCountB(t); n > biggest {
			biggest = n
		}
	}
	// The LARGEST size token decides, because a mixture-of-experts id carries two:
	// qwen3.6-235b-a22b is a 235B model that activates 22B, and reading the active
	// count alone would file a flagship as small.
	return biggest > 0 && biggest <= 32
}

// paramCountB reads a size token: "8b", "30b", "a3b" (active parameters) → the
// number of billions, 0 when the token is not a size.
func paramCountB(t string) int {
	t = strings.TrimPrefix(t, "a")
	if !strings.HasSuffix(t, "b") || len(t) < 2 {
		return 0
	}
	n := 0
	for _, r := range t[:len(t)-1] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// pickModels chooses up to n distinct big models by preference.
func pickModels(names, pref []string, n int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(m string) {
		if !seen[m] && len(out) < n {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, p := range pref {
		for _, m := range names {
			if strings.Contains(strings.ToLower(m), p) && !isSmall(m) {
				add(m)
			}
		}
	}
	for _, m := range names {
		if !isSmall(m) {
			add(m)
		}
	}
	for _, m := range names {
		add(m)
	}
	return out
}

// pickCheap is the small model compaction should run on, or "" when the gateway
// serves none. It used to fall back to the last name the gateway listed, which is
// arbitrary: it made a flagship the summariser on a fleet of six big models.
// cheapBlock is the compaction role, or nothing when the gateway serves no small
// model: a role with an empty models: list does not load, and inventing a
// flagship as the summariser is worse than leaving compaction on the lead.
func cheapBlock(model string) string {
	if model == "" {
		return ""
	}
	return fmt.Sprintf(`
  cheap:
    description: Summaries and compaction.
    models: [%s]
    effort: off
    context: 32000
    tools: []
`, model)
}

func pickCheap(names []string) string {
	for _, m := range names {
		if isSmall(m) {
			return m
		}
	}
	return ""
}

// detectToolchain guesses the project's test command and the commands its
// agents will need.
func detectToolchain(root string) (check string, allow []string) {
	has := func(f string) bool { _, err := os.Stat(filepath.Join(root, f)); return err == nil }
	allow = []string{"git", "ls", "cat", "head", "tail", "wc", "grep", "find", "echo", "ssh", "scp", "rsync"}
	switch {
	case has("go.mod"):
		check = "go test ./..."
		allow = append(allow, "go", "gofmt")
	case has("Cargo.toml"):
		check = "cargo test"
		allow = append(allow, "cargo")
	case has("package.json"):
		check = "npm test"
		allow = append(allow, "npm", "node", "npx")
	case has("pyproject.toml") || has("setup.py") || has("pytest.ini"):
		check = "pytest -q"
		allow = append(allow, "python3", "pytest", "pip")
	}
	if has("Makefile") {
		allow = append(allow, "make")
		if data, err := os.ReadFile(filepath.Join(root, "Makefile")); err == nil && strings.Contains(string(data), "\ntest:") && check == "" {
			check = "make test"
		}
	}
	allow = append(allow, "bsk")
	return check, allow
}

// ── lca doctor ──────────────────────────────────────────────────────────────

type probeResult struct {
	model, role string
	status      string // ok | warn | fail
	detail      string
	fix         string
	notes       []string // what else was tried: multiple calls, nested JSON, reasoning replay
	warns       []string
}

// runDoctor takes a ctx because `lca doctor -all` probes every model in every
// chain at a three-minute timeout each: Ctrl-C has to stop it for real, not stop
// WAITING for it. Cancellation is at loop boundaries, and the screen says so —
// a probe already in flight finishes, the next one does not start.
func runDoctor(ctx context.Context, cfg Config, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	noProbe := fs.Bool("no-probe", false, "don't call the models (only list and configuration checks)")
	all := fs.Bool("all", false, "probe every model in every chain, not just the first of each role")
	tierFlag := fs.String("tier", "", "run every tier-declaring role on that chain (roles.yaml tiers:)")
	memberFlag := fs.String("member", "", "probe only this member (roles.yaml members:)")
	mcpRefresh := fs.Bool("mcp-refresh", false, "rewrite .lca/mcp.lock.json from what each mcp server serves now")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// doctor is the subcommand whose job is showing the resolved team, so it has
	// to be pointable at a tier like every other one.
	cfg.Tier = firstNonEmpty(*tierFlag, cfg.Tier)
	failed := false
	fail := func(format string, a ...any) { failed = true; errLine(format, a...) }

	// MCP first, before loadRoles below, for the same reason buildOrchestrator does
	// it in that order: roles.yaml's tools: list is validated against toolRegistry,
	// so a role naming jira__issue_get needs the tool to exist by then. It is two
	// file reads and no network — -no-probe stays literally true.
	var mset *MCPSet
	var mcpWarns []string
	dfc, derr := loadFileConfig(cfg)
	if derr != nil {
		// The error must not be dropped. Dropped, doctor printed no MCP section and no
		// message at all for a typo in the mcp block — and then loadRoles ran with an
		// empty registry, so every role naming an mcp tool failed below as "unknown
		// tool" and sent the operator to roles.yaml for a mistake in config.json. This
		// is the command whose job is saying exactly what to fix, and it comes first
		// because everything after it is a consequence.
		section("config")
		fail("%v", derr)
		hint("fix that first — until the file parses, no mcp tool is registered, so a role naming one fails as \"unknown tool\" below")
	} else {
		mset, mcpWarns = loadMCP(cfg, dfc)
		mcpWarns = append(mcpWarns, registerMCPTools(mset)...)
	}

	// The team file is READ here, before the gateway section, and still REPORTED
	// in its own section below: the gateway's engine row has to name a per-model
	// engine the file set, and the file is two reads and no network, so -no-probe
	// stays literally true.
	roles, rerr := loadRoles(cfg)

	section("gateway")
	gw := NewClient(cfg)
	applyRememberedWindow(cfg, gw)
	row("url", gw.Endpoint())
	models, err := gw.ListModels()
	// The whole ModelInfo, not just a bool: MaxLen is what makes a server/profile
	// comparison possible at all, and throwing it away is why doctor could not see
	// a five-times-too-small window.
	served := map[string]ModelInfo{}
	switch {
	case err != nil:
		fail("unreachable: %v", err)
		hint("%s", firstNonEmpty(errorHint(err), "is the gateway up? LCA_BASE_URL must point at it (…/v1)"))
	default:
		for _, m := range models {
			served[m.ID] = m
		}
		okLine("up"+gSep+"%s listed", plural(len(models), "model", "models"))
	}
	// The engine, with its provenance, before the per-model report: it is a fact
	// about the ENDPOINT, and every piece of advice below is spelled in its flags.
	// gw is given the answer too, so a client copied from it starts with the engine
	// a session would have had — the probes below narrow it per model.
	native := readNativeInfo(ctx, gw, cfg)
	// With its OWN source, not a flattened one: endpointEngine answers from
	// cfg.Engine or from what the cards agreed on, and recording a card-derived
	// answer at the endpoint tier would let one model's owned_by outrank another
	// model's own card — the exact collapse the per-(endpoint, model) design exists
	// to prevent. The ranking is the same one the runtime uses, so it decides.
	gwEngine, gwSrc := endpointEngine(cfg, served)
	gw.setEngine(gwEngine, gwSrc)
	reportEngine(cfg, dfc, roles, served, native)
	if len(served) > 0 {
		reportProfiles(cfg, gw.Endpoint(), served, roles, gwEngine)
	}

	section("roles")
	switch {
	case rerr != nil:
		fail("%v", rerr)
	case len(roles.Roles) == 0:
		warnLine("no roles.yaml — single-agent mode")
		hint("%s", "in a session: /setup picks the models and gives them roles"+gSep+"from the shell: lca init")
	default:
		row("files", faint("%s", strings.Join(func() []string {
			var ps []string
			for _, p := range roles.Sources {
				ps = append(ps, prettyPath(p, cfg.Root))
			}
			return ps
		}(), ", ")))
		for _, w := range roles.Warnings {
			warnLine("%s", w)
		}
		if roles.Tier != "" {
			row("tier", roles.Tier+faint(" (active for every role that declares one)"))
		}
		// A models: key that matches nothing served is sampling an operator wrote
		// down and nobody reads — and the likeliest reason is a spelling the
		// gateway does not use.
		for _, k := range sortedKeys(roles.ModelOpts) {
			if len(served) == 0 {
				break
			}
			hit := false
			for m := range served {
				if m == k || normalizeModelID(m) == normalizeModelID(k) {
					hit = true
				}
			}
			if !hit {
				warnLine("models.%s in roles.yaml matches no served model — its sampling is never used", k)
				hint("use one of: %s", strings.Join(sortedKeys(served), ", "))
			}
		}
		var rows [][]string
		for _, r := range roles.Roles {
			model := firstNonEmpty(r.Models[0], "")
			prof := lookupProfile(model)
			opts := roles.modelOpts(model)
			// The window in force, in the order the runtime resolves it: the
			// deployment's own max_model_len, then the role, then the table.
			window, wsrc := prof.Context, prof.Src.Context
			// A window an earlier refusal taught us is server truth too, and the
			// runtime already prefers it — so the table has to, or doctor warns
			// about a placeholder that is no longer in use.
			if n := rememberedWindow(cfg, gw.Endpoint(), model); n > 0 {
				window, wsrc = n, OriginServer
			}
			if info, ok := served[model]; ok && info.MaxLen > 0 {
				window, wsrc = info.MaxLen, OriginServer
			}
			budget, bsrc := r.Context, "role"
			switch {
			case budget == 0:
				budget, bsrc = window, wsrc.String()
			case window > 0 && budget > window:
				// Session.budget caps the role at the deployment's window, so the
				// table has to print what is in force and not what the file says.
				budget, bsrc = window, wsrc.String()+", capped from the role's "+kfmt(r.Context)
			}
			guessed := budget == 0
			// Compared against the SERVER's window when there is one: comparing
			// only against the table let a role configured above the running
			// deployment pass doctor and fail on the first request.
			if window > 0 && r.Context > window {
				warnLine("role %s: context %s is larger than %s's window %s (%s) — requests will be refused",
					r.Name, kfmt(r.Context), model, ctxfmt(window), wsrc)
			}
			// The engine's own usable-input number, when it answered: it is strictly
			// below the window (max_req_len - 5) and lca deliberately does NOT cap the
			// budget with it — a budget that changes depending on whether a gateway
			// proxies a path is worse than a stable one — so the warning is the whole
			// of what lca does with it.
			if native.maxReqInputLen > 0 && r.Context > native.maxReqInputLen {
				warnLine("role %s: context %s is above the engine's own usable input %s (max_req_input_len) — the prompt is refused or silently truncated before the window matters",
					r.Name, kfmt(r.Context), kfmt(native.maxReqInputLen))
			}
			if guessed {
				// Say the placeholder out loud: a fallback that prints as a number
				// is indistinguishable from a real window, which is how a role ends
				// up quietly running on 24k.
				budget, bsrc = ctxBudgetFallback, "fallback"
				warnLine("role %s: no window known for %s — the budget falls back to %d tokens, a placeholder and not a real window",
					r.Name, model, ctxBudgetFallback)
				hint("set context: on the role (the model card's window), or add %s to models.go", model)
			}
			// Only where the vendor has actually said what the model accepts: a
			// warning about a model nobody recorded a vocabulary for would fire on
			// every legacy id and teach the operator to skip the section.
			if eff := firstNonEmpty(r.Thinking, optEffort(opts)); eff != "" && eff != "off" && eff != "on" {
				switch {
				case prof.EffortNone && prof.Switch.Kwarg != "":
					warnLine("role %s: %s documents no reasoning_effort at all, so the level %q is not sent — only its %s switch is",
						r.Name, model, eff, prof.Switch.Kwarg)
				case prof.EffortNone:
					warnLine("role %s: %s documents no reasoning_effort at all, so effort %q is not sent", r.Name, model, eff)
				case len(prof.Efforts) > 0 && prof.effortForLocal(eff) == "":
					// The gateway is the self-hosted path, so the list quoted is the
					// one the template accepts, not the hosted API's wider aliases.
					warnLine("role %s: effort %q is not accepted by %s (%s) — lca sends no effort for it",
						r.Name, eff, model, strings.Join(prof.effortVocab(true), "|"))
				}
			}
			var chain []string
			for _, m := range r.Models {
				switch {
				case len(served) == 0:
					chain = append(chain, m)
				default:
					if _, ok := served[m]; ok {
						chain = append(chain, cGreen+m+cReset)
					} else {
						chain = append(chain, cRed+m+cReset)
						failed = true
					}
				}
			}
			// Every column carries where its value came from, so "1.0 (card)" is
			// distinguishable from "1.0 (role)" and from a number nobody sourced.
			temp := sampleSrc(r.Temperature, optTemp(opts), prof.Temperature, prof.Src.Temperature)
			topP := sampleSrc(r.TopP, optTopP(opts), prof.TopP, prof.Src.TopP)
			effort := firstNonEmpty(r.Thinking, optEffort(opts), "provider default")
			rows = append(rows, []string{r.Name, orDash(r.Tier), strings.Join(chain, faint(" %s ", gFlow)),
				temp, topP, effort, ctxfmt(budget) + faint(" (%s)", bsrc), replayName(prof.Replay)})
		}
		table([]string{"role", "tier", "models", "temp", "top_p", "effort", "context", "reasoning"}, rows, 0, 2)
		if failed && len(served) > 0 {
			hint("red models aren't listed by the gateway — use names from: %s", strings.Join(sortedKeys(served), ", "))
		}
	}

	// The fleet as the runtime will resolve it, built once: the checks below all
	// ask questions about a member, and two orchestrators could answer them
	// differently.
	orch := doctorOrchestrator(cfg, roles, mset)

	// A role's check_cmd meets the sandbox of the machine that role works on, not
	// the team's: a verifier the allowlist refuses fails every delegation to that
	// role, twenty minutes in and with a message about the sandbox rather than
	// about the change. Silent otherwise, because the common case is fine.
	if orch != nil && roles != nil {
		for _, r := range roles.Roles {
			if r.CheckCmd == "" {
				continue
			}
			m := orch.memberFor(r)
			if cerr := checkOn(orch.policyOf(m), m, r.CheckCmd); cerr != nil {
				fail("role %s: check_cmd %q cannot run on member %s: %v", r.Name, r.CheckCmd, m.MemberName(), cerr)
				hint("add it to sandbox.allow (or to members.%s allow:), or change the role's check_cmd", m.MemberName())
			}
		}
	}

	if !*noProbe && err == nil && rerr == nil && roles != nil && len(roles.Roles) > 0 {
		section("tool calling")
		type target struct{ role, model, engine string }
		var targets []target
		seen := map[string]bool{}
		for _, r := range roles.Roles {
			for i, m := range r.Models {
				if _, up := served[m]; (i > 0 && !*all) || seen[m] || !up {
					continue
				}
				seen[m] = true
				e, _ := doctorEngine(cfg, roles, m, served[m])
				targets = append(targets, target{r.Name, m, e})
			}
		}
		results := make([]probeResult, len(targets))
		var wg sync.WaitGroup
		fmt.Println("  " + faint("asking each probed model for one tool call…"))
		started := 0
		for i, t := range targets {
			if ctx.Err() != nil {
				warnLine("interrupted after %d of %d — a probe already in flight finishes, the next does not start", started, len(targets))
				break
			}
			started++
			wg.Add(1)
			go func(i int, t target) {
				defer wg.Done()
				// The engine resolved for THIS model travels in cfg.Engine: probeModel
				// is handed the engine in force for the target it probes, so the flags
				// it names are the ones that server's argparse accepts. The signature
				// is unchanged, which keeps every caller — including the tests — as it was.
				tcfg := cfg
				tcfg.Engine = t.engine
				results[i] = probeModel(ctx, tcfg, gw, roles, t.role, t.model)
			}(i, t)
		}
		wg.Wait()
		// After the wait, not only in the launch loop: every target is launched before
		// anything is awaited, so a human Ctrl-C always arrives with started ==
		// len(targets) and the notice above was unreachable. The cancelled probes then
		// rendered as red failures and flipped the verdict to "problems found", with no
		// mention of the interrupt — /eval got this right and doctor did not.
		if ctx.Err() != nil && started == len(targets) {
			warnLine("interrupted — the probes below did not finish; nothing here is a verdict on the gateway")
		}
		for _, r := range results {
			if r.status == "" {
				continue // never started: the run was interrupted before it
			}
			label := r.model + faint(" (%s)", r.role)
			if ctx.Err() != nil && r.status != "ok" && strings.Contains(r.detail, "context canceled") {
				warnLine("%s  %s", label, faint("interrupted"))
				continue
			}
			switch r.status {
			case "ok":
				okLine("%s  %s", label, faint("%s", r.detail))
			case "warn":
				warnLine("%s  %s", label, r.detail)
			default:
				failed = true
				errLine("%s  %s", label, r.detail)
			}
			for _, w := range r.warns {
				warnLine("%s  %s", label, w)
			}
			// Notes fold into detail on the happy path; a probe that ran after a
			// failure has news of its own and no line to sit on.
			for _, n := range r.notes {
				if !strings.Contains(r.detail, n) {
					hint("%s  %s", label, n)
				}
			}
			if r.fix != "" {
				hint("%s", r.fix)
			}
		}
	}

	// The fleet. Every non-local member is probed: ssh reachable, dir exists, git
	// present and the dir inside a repository, the worktree base writable, and
	// THAT member's allowlist resolved there — an operator has to find all of this
	// before a run does, not twenty minutes into one.
	if orch != nil {
		names := orch.memberNames()
		if *memberFlag != "" {
			if orch.member(*memberFlag) == nil {
				fail("-member %q is not a member (members: %s)", *memberFlag, strings.Join(names, ", "))
				names = nil
			} else {
				names = []string{*memberFlag}
			}
		}
		remotes := 0
		for _, n := range names {
			if !orch.member(n).IsLocal() {
				remotes++
			}
		}
		if remotes > 0 {
			section("members", faint("%s", plural(remotes, "other machine", "other machines")))
			reach := probeMembers(ctx, orch, names)
			for _, n := range names {
				m := orch.member(n)
				if m.IsLocal() {
					continue // the workspace section below is this machine's
				}
				rem := m.Rem
				row("member", n+faint(gSep+"%s", rem.Label()))
				if err := reach[n]; err != nil {
					fail("%s", err.Error())
					if memberDirMissing(err) {
						// ssh demonstrably works: sending the operator to their keys
						// would be advice for a problem they do not have.
						hint("create or clone the project at %s on %s, or point members.%s dir: at where it is", rem.Dir, rem.Where(), n)
					} else {
						hint("check `ssh %s` by hand: keys, ~/.ssh/config, VPN — only outbound ssh is needed", firstNonEmpty(rem.Host, rem.Where()))
					}
					continue
				}
				okLine("ssh works"+gSep+"%s exists", rem.Dir)
				if _, e := rem.run(context.Background(), "command -v git >/dev/null", 20*time.Second, nil, nil); e != 0 {
					fail("git is not installed on %s — a delegation there cannot create a worktree", rem.Where())
					hint("install git on %s, or run that role on another member", rem.Where())
				} else if _, e := rem.run(context.Background(), "git rev-parse --show-toplevel >/dev/null 2>&1", 20*time.Second, nil, nil); e != 0 {
					warnLine("%s is not a git repository on %s — delegate has nothing to snapshot there", rem.Dir, rem.Where())
					hint("git init or clone the project at %s on %s", rem.Dir, rem.Where())
				} else {
					okLine("git repository — delegate can create a worktree there")
				}
				if _, e := rem.run(context.Background(), `mkdir -p "${LCA_DIR:-$HOME/.lca}/worktrees" && test -w "${LCA_DIR:-$HOME/.lca}/worktrees"`, 20*time.Second, nil, nil); e != 0 {
					warnLine("the worktree base ${LCA_DIR:-$HOME/.lca}/worktrees is not writable on %s", rem.Where())
				}
				// One connection for the whole list: an eight-member fleet over a VPN
				// would otherwise pay a handshake per allowlisted command, and doctor
				// is the command an operator runs when something is already wrong.
				var missing, noBsk []string
				if list := memberAllowlist(cfg, roles, m); len(list) > 0 {
					var words []string
					for _, c := range list {
						words = append(words, shellQuote(c))
					}
					out, e := rem.run(context.Background(), "for c in "+strings.Join(words, " ")+`; do command -v "$c" >/dev/null 2>&1 || echo "$c"; done`, 60*time.Second, nil, nil)
					if e != 0 {
						warnLine("could not check the allowlist on %s: %s", rem.Where(), strings.TrimSpace(lastLines(out, 2, 200)))
					}
					for _, c := range strings.Fields(out) {
						if c == "bsk" {
							noBsk = append(noBsk, c)
							continue
						}
						missing = append(missing, c)
					}
				}
				switch {
				case len(missing) > 0:
					warnLine("not installed on %s: %s", rem.Where(), strings.Join(missing, ", "))
				default:
					okLine("every allowlisted command exists on %s", rem.Where())
				}
				if len(noBsk) > 0 {
					warnLine("bsk is not installed on %s — that member cannot schedule GPU jobs, and bsk submit is the only path to one", rem.Where())
				}
			}
			if roles != nil && len(roles.Roles) > 0 {
				var rows [][]string
				for _, a := range roles.Roles {
					rows = append(rows, []string{a.Name, orch.memberFor(a).MemberName()})
				}
				table([]string{"role", "member"}, rows)
			}
			if orch.legacyFleet() {
				hint("delegate is off on the old remote: block (its worktree would be local) — move it into members: for cross-machine worktrees")
			}
		}
	}

	// MCP. Skipped entirely when no mcp block exists, and it honours -no-probe: an
	// operator who asked for configuration checks only must not have had a
	// connection opened on their behalf, on a host whose security team watches for
	// exactly that.
	if orch != nil && mcpDoctor(ctx, orch, roles, mcpWarns, *noProbe, *mcpRefresh) {
		failed = true
	}

	// Workflows. A file that does not parse, or whose steps name a role, a member,
	// a tier or a command this team cannot honour, should be found by the command
	// an operator already runs — not at step 9 of the pipeline that needed it.
	// These are `lca run -dry-run`'s checks without the plan table: one line per
	// workflow, because a fleet may hold a dozen and doctor is read at a glance.
	if wfs, wfWarns := listWorkflows(cfg); len(wfs)+len(wfWarns) > 0 {
		section("workflows", faint("%s", plural(len(wfs)+len(wfWarns), "file", "files")))
		for _, w := range wfWarns {
			fail("%s", w) // it did not parse: nothing could run it
		}
		lead := ""
		if roles != nil {
			lead = roles.Entry
		}
		lead = firstNonEmpty(lead, cfg.Agent)
		for _, wf := range wfs {
			steps := plural(len(wf.Steps), "step", "steps")
			if orch == nil || roles == nil || len(roles.Roles) == 0 {
				okLine("%s"+gSep+"%s%s", wf.Name, steps, faint(" (parsed; a prompt or delegate step needs a roles.yaml)"))
				continue
			}
			// A declared variable with no value is supplied with -var at run time,
			// so a stand-in keeps the contract from reading as a broken workflow.
			vars := wf.effectiveVars(nil)
			for k, v := range vars {
				if strings.TrimSpace(v) == "" {
					vars[k] = "doctor"
				}
			}
			if err := wf.bind(orch, lead, vars); err != nil {
				fail("%v", err)
				continue
			}
			okLine("%s"+gSep+"%s", wf.Name, steps)
		}
	}

	section("workspace")
	row("root", ellipsizeMiddle(shortDir(cfg.Root), max(70, houseWidth()-6)))
	if _, err := gitCmd(cfg.Root, nil, nil, "rev-parse", "--show-toplevel"); err == nil {
		okLine("git repository — delegate can use worktrees")
	} else {
		warnLine("not a git repository — delegate needs one (git init)")
	}
	allow := allowlistOf(cfg, roles)
	var missing []string
	for _, c := range allow {
		if c == "bsk" {
			continue // reported on its own below
		}
		if _, err := exec.LookPath(c); err != nil {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		okLine("sandbox: every allowlisted command is installed")
	} else {
		warnLine("sandbox: not on PATH: %s", strings.Join(missing, ", "))
	}
	if _, err := exec.LookPath("bsk"); err != nil {
		warnLine("bsk not found — agents can't schedule GPU jobs")
	}
	if err := os.MkdirAll(cfg.stateDir(), 0o700); err != nil {
		fail("can't write %s: %v", cfg.stateDir(), err)
	} else {
		okLine("state in %s", shortDir(cfg.stateDir()))
	}

	fmt.Println()
	if failed {
		errLine("problems found — fix the red lines above")
		return 1
	}
	okLine("ready")
	return 0
}

// probeModel asks one model for one tool call through the gateway, with the
// same request shape the agent uses, and reports what came back.
// The result is NAMED because probeWindow runs in a defer: with an unnamed
// result, `return res` copies the value before the defer runs and everything it
// learned is thrown away.
func probeModel(ctx context.Context, cfg Config, gw *Client, roles *RolesConfig, role, model string) (res probeResult) {
	res = probeResult{model: model, role: role}
	c := *gw
	c.model = model

	c.transport = roles.transportOf(model)
	// The vendor documents an engine flag per model. Naming it turns "tool calling
	// is broken" into a line an operator can paste into the launch command — but
	// only when a probe actually failed: advice next to a green line is noise.
	prof := lookupProfile(model)
	// The engine serving THIS model, resolved before the first request and never
	// after it: cfg.Engine carries what runDoctor resolved for this target, and
	// roles.yaml's per-model key outranks it. Every flag named below is then in
	// that engine's own spelling — a vLLM parser value fails SGLang's argparse and
	// stops the server from starting, so cross-quoting is worse than silence.
	c.setEngine(cfg.Engine, EngineFromEndpoint)
	c.setEngine(roles.engineOf(model), EngineFromModel)
	engine := c.Engine()
	toolFix := engineAdvice(prof, engine)
	if prof.Key != "" {
		toolFix += faint(" (the flags %s's vendor documents)", prof.Key)
	}
	var effort string
	for _, r := range roles.Roles {
		if r.Name == role {
			effort = r.Thinking
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	// Whether the window is known is not a question about tool calling, so it is
	// asked whatever the protocol probe does — including when it fails and the
	// function returns early below. Registered AFTER cancel so it runs BEFORE it:
	// defers are LIFO, and the first version of this ran with a cancelled context.
	defer func() { probeWindow(ctx, &c, &res, gw, cfg) }()
	req := ChatRequest{Thinking: effort, MaxTokens: 2048,
		Headers: map[string]string{"x-session-id": "lca-doctor-" + model, "x-root-session-id": "lca-doctor"}}
	if !c.Native() {
		req.Messages = []Message{{Role: "system", Content: "Reply with exactly this line and nothing else:\n<ping value=\"ok\"/>"}, {Role: "user", Content: "go"}}
	} else {
		req.Messages = []Message{
			{Role: "system", Content: "You are a connectivity test. Use the ping tool when asked."},
			{Role: "user", Content: `Call the ping tool with value "ok". Do not reply with text.`},
		}
		req.Tools = []ToolSchema{{Type: "function", Function: ToolSchemaFunc{Name: "ping", Description: "Connectivity check.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []string{"value"}}}}}
	}
	start := time.Now()
	out, err := c.Chat(ctx, req, StreamSink{})
	took := time.Since(start)
	if err != nil {
		res.status = "fail"
		res.detail = truncate(err.Error(), 160)
		switch classifyGW(err, out.Started) {
		case gwNotUp:
			res.status, res.detail = "warn", "not up right now (scaled to zero or paused) — it starts on first use"
		case gwOverloaded:
			res.status, res.detail = "warn", "overloaded right now — try again"
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 400 {
			res.fix = "the engine rejected tools — enable its tool-call parser (" + toolFix + "), or set models." + model + ".transport: text"
		} else if res.status == "fail" {
			res.fix = errorHint(err)
		}
		return res
	}
	var parts []string
	if !c.Native() {
		if strings.Contains(out.Content, "<ping") {
			res.status = "ok"
			parts = append(parts, "text protocol")
		} else {
			res.status, res.detail = "fail", "didn't follow the text tool protocol"
			return res
		}
	} else {
		got := ""
		for _, tc := range out.ToolCalls {
			got = tc.Function.Name
		}
		switch {
		case got == "ping":
			res.status = "ok"
			parts = append(parts, "native tool call")
		case unparsedToolCall(out.Content, "ping"):
			res.status = "fail"
			res.detail = "the call came back as text, not tool_calls — the engine's tool-call parser is off or wrong for this model"
			res.fix = toolFix + gSep + "or models." + model + ".transport: text"
			return res
		default:
			res.status = "warn"
			res.detail = "answered without calling the tool: " + truncate(strings.TrimSpace(out.Content), 80)
			return res
		}
	}
	switch {
	case out.Reasoning != "":
		parts = append(parts, "reasoning separate")
	case reThinkBlock.MatchString(out.Content):
		// Thinking arrived, but glued into content: the engine has no reasoning
		// parser loaded, so the trace cannot be replayed as its own field.
		parts = append(parts, "reasoning inline (<think>)")
		res.warns = append(res.warns, "the thinking came back inside content — no reasoning parser is loaded, so it cannot be replayed as its own field: "+engineAdvice(prof, engine))
		if engine == engineSGLang {
			// One cause, named: separate_reasoning defaults to true on SGLang and
			// does nothing at all until the server was launched with a reasoning
			// parser — which is a launch flag lca cannot set from a request.
			res.warns = append(res.warns, faint("SGLang splits reasoning by default (separate_reasoning defaults to true), but only when --reasoning-parser was passed at launch"))
		}
	case prof.Reasoning && thinkingExpected(prof, effort):
		// Only when thinking was actually asked for, or cannot be turned off:
		// self-hosted hy3 answers without thinking by design, and a parser warning
		// there would be advice for a problem the operator does not have.
		res.warns = append(res.warns, "this model thinks, but no reasoning came back at all — "+engineAdvice(prof, engine))
	}
	if out.Usage.TTFT > 0 {
		parts = append(parts, "first token "+fmtDurShort(out.Usage.TTFT))
	}
	parts = append(parts, "total "+fmtDurShort(took))
	res.detail = strings.Join(parts, gSep)
	if c.Native() {
		probeParser(ctx, &c, &res)
	}
	probeReplay(ctx, &c, roles, &res)
	probeHy3Variant(ctx, &c, &res)
	if len(res.notes) > 0 {
		res.detail += gSep + "" + strings.Join(res.notes, gSep)
	}
	return res
}

// engineAdvice is engineFlags with the engine named, so a line an operator pastes
// says which server it is for. With no engine known, engineFlags labels both
// halves itself — and that generic string is the honest answer, not a fallback.
func engineAdvice(prof ModelProfile, engine string) string {
	switch engine {
	case engineSGLang:
		return "SGLang: " + prof.engineFlags(engine)
	case engineVLLM:
		return "vLLM: " + prof.engineFlags(engine)
	}
	return prof.engineFlags("")
}

// probeWindow asks a deployment that does not publish max_model_len how big its
// window is, by requesting an impossible completion: engines validate the budget
// before they generate, so the refusal states the real number and nothing is
// decoded. It runs ONLY when the window is still unknown — a deployment that
// answered /v1/models, or a model whose card we have, is not asked — because the
// alternative is finding out mid-task, after the placeholder budget has spent the
// session compacting and re-reading the same files.
func probeWindow(ctx context.Context, c *Client, res *probeResult, gw *Client, cfg Config) {
	if c.CtxLen() > 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := c.Chat(ctx, ChatRequest{
		MaxTokens: 1 << 30,
		Messages:  []Message{{Role: "user", Content: "hi"}},
		Headers:   map[string]string{"x-session-id": "lca-doctor-window-" + c.Model(), "x-root-session-id": "lca-doctor"},
	}, StreamSink{})
	n := statedWindow(err)
	switch {
	case n > 0:
		c.SetCtxLen(n)
		if gw != nil {
			gw.SetCtxLen(n) // the session's own client, not only doctor's copy
		}
		rememberWindowIn(cfg.stateDir(), c.Endpoint(), c.Model(), n)
		res.notes = append(res.notes, fmt.Sprintf("window %s (its own refusal, remembered)", kfmt(n)))
	case err == nil:
		// It accepted a billion-token completion, so it clamps silently instead of
		// refusing. Nothing was learned and nothing is pretended.
		res.warns = append(res.warns, "window unknown: the server accepted an impossible max_tokens instead of naming its limit — set it with /set context <n>")
		if c.Engine() == engineSGLang {
			// The cause, named: with --allow-auto-truncate SGLang logs "Truncating the
			// input." and serves a silently shortened prompt, so "no refusal" never
			// means "it fit" — and the shortened prompt is the one the model answers.
			res.warns = append(res.warns, faint("on SGLang that is what --allow-auto-truncate does: an oversized prompt is truncated and served, not refused"))
		}
	default:
		res.warns = append(res.warns, "window unknown: its refusal names no limit ("+truncate(shortErr(err), 80)+") — set it with /set context <n>")
	}
}

// toolCallMarkers are the shapes a tool call takes when it reaches this client as
// TEXT, i.e. when the engine's tool-call parser is off or is the wrong one for
// the model. On SGLang that failure is an HTTP 200 with tool_calls null,
// finish_reason "stop" and the raw markup left in content, so the markup is the
// only evidence there is — and an engine with NO parser configured behaves
// identically, because the parse is gated on the parser existing.
//
// hy3's markup is <tool_calls><tool_call>name<tool_sep><arg_key>…</arg_key>… and
// GLM-5.3's is <tool_call>…<arg_key>…<arg_value>…: NEITHER contains a "{", so the
// name-plus-brace heuristic this list replaces reported both of the operator's
// most likely models as "answered without calling the tool" and hid the cause.
var toolCallMarkers = []string{"<tool_call", "<tool_calls", "<arg_key", "<arg_value", "<tool_sep",
	"<|tool_calls_begin|>", "[tool_calls]"}

// unparsedToolCall reports whether a reply is a tool call that arrived as text.
func unparsedToolCall(content, name string) bool {
	low := strings.ToLower(content)
	for _, m := range toolCallMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	// The original heuristic, kept rather than replaced: it also catches the
	// tool_choice "required" with no parser case, where the engine applies the
	// json_schema constraint outside the parser gate and the model emits a
	// well-formed JSON array INTO content, still with finish_reason "stop".
	return name != "" && strings.Contains(content, name) && strings.Contains(content, "{")
}

// probeParser exercises what server-side tool-call parsers actually break on:
// two calls in one reply, and an argument with nested JSON.
func probeParser(ctx context.Context, c *Client, res *probeResult) {
	// The nested parameter is declared as {"type":"object"}, and that is REQUIRED
	// rather than tidy: hy3's hunyuan detector is schema-driven and keeps a
	// JSON-looking value in a string-typed parameter as a literal string. Declare
	// spec as a string and this probe reports a parser failure that is the probe's
	// own schema omission. A test pins it.
	schema := ToolSchema{Type: "function", Function: ToolSchemaFunc{Name: "note", Description: "Record one note.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"spec": map[string]any{"type": "object", "properties": map[string]any{
				"title": map[string]any{"type": "string"},
				"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			}, "required": []string{"title", "tags"}},
		}, "required": []string{"spec"}}}}
	out, err := c.Chat(ctx, ChatRequest{MaxTokens: 2048, Tools: []ToolSchema{schema},
		Headers: map[string]string{"x-session-id": "lca-doctor-parser-" + c.model, "x-root-session-id": "lca-doctor"},
		Messages: []Message{
			{Role: "system", Content: "You are a connectivity test."},
			{Role: "user", Content: `Call the note tool TWICE in this one reply: first with spec {"title":"a","tags":["x","y"]}, then with spec {"title":"b","tags":["z"]}. No text.`},
		}}, StreamSink{})
	if err != nil {
		res.warns = append(res.warns, "parser stress test could not run: "+shortErr(err))
		return
	}
	switch n := len(out.ToolCalls); {
	case n >= 2:
		res.notes = append(res.notes, "2 calls in one reply")
	case n == 1:
		res.notes = append(res.notes, "one call per reply (the server or model won't batch them)")
	default:
		if unparsedToolCall(out.Content, "note") {
			// 100% unparsed, WITH markers: that is a configuration fault, not a
			// batching one, and the two used to read the same.
			res.warns = append(res.warns, "asked for two tool calls and got the raw markup in content — no tool-call parser is loaded, or the wrong one for this model")
		} else {
			res.warns = append(res.warns, "asked for two tool calls, got none — the parser may drop batched calls")
		}
		if c.Engine() == engineSGLang {
			res.warns = append(res.warns, faint("SGLang also drops a call to a function that is not in tools, silently, unless the server has SGLANG_FORWARD_UNKNOWN_TOOLS=1"))
		}
		return
	}
	nested := false
	for _, tc := range out.ToolCalls {
		var a struct {
			Spec struct {
				Title string   `json:"title"`
				Tags  []string `json:"tags"`
			} `json:"spec"`
		}
		if json.Unmarshal([]byte(tc.Function.Arguments), &a) == nil && a.Spec.Title != "" && len(a.Spec.Tags) > 0 {
			nested = true
		}
	}
	if nested {
		res.notes = append(res.notes, "nested JSON args ok")
	} else {
		res.warns = append(res.warns, "nested JSON arguments came back malformed — see the raw arguments in the trace")
	}
}

// probeHy3Variant answers the one question about hy3 that the OpenAI surface
// cannot be asked. Two chat-template variants of this checkpoint are in
// circulation, they disagree about what "no reasoning_effort" means — unset is
// `high` on a hunyuan_effort template and NO thinking at all on an Hy3-preview
// one — and both run under the same parser name, with nothing in /v1/models, in
// an error message or in any header to tell them apart.
//
// So it is OBSERVED, and reported as an observation. One request with no level
// on a one-word prompt is one sample, and the two directions are not equally
// strong: reasoning that came back proves the deployment thinks with no level
// sent, while reasoning that did not come back is also what a model that simply
// had nothing to think about on "say done" looks like. Neither line names a
// template variant as a finding — it names what was seen and what that would be
// consistent with, because an identification from one negative sample is a claim
// the sources explicitly list as UNKNOWN.
//
// lca sends an explicit level on every real request precisely so that this
// default decides nothing — the probe exists to tell the operator which
// deployment they may have, not to feed a default back into the request.
func probeHy3Variant(ctx context.Context, c *Client, res *probeResult) {
	if prof := c.Profile(); prof.Key != "hy3" || !c.selfHosted() {
		return
	}
	out, err := c.Chat(ctx, ChatRequest{MaxTokens: 256,
		Messages: []Message{{Role: "user", Content: "Reply with the single word: done."}},
		Headers:  map[string]string{"x-session-id": "lca-doctor-variant-" + c.model, "x-root-session-id": "lca-doctor"}}, StreamSink{})
	switch {
	case err != nil:
		res.warns = append(res.warns, "could not check what this hy3 deployment does with no reasoning_effort: "+shortErr(err))
	case out.Reasoning != "" || reThinkBlock.MatchString(out.Content):
		res.notes = append(res.notes, "hy3 with no reasoning_effort sent: it thought — consistent with a hunyuan_effort template, where unset means high")
	default:
		res.notes = append(res.notes, "hy3 with no reasoning_effort sent: it did NOT think on one trivial prompt — consistent with an Hy3-preview template (unset means no thinking), but one sample on \"say done\" does not identify the template")
	}
}

// probeReplay checks that the server accepts the model's own reasoning back in
// the history. Interleaved-thinking models need it; a server that rejects the
// field needs reasoning_replay: off for that model.
func probeReplay(ctx context.Context, c *Client, roles *RolesConfig, res *probeResult) {
	if c.Profile().Replay == "" {
		return
	}
	msgs := []Message{
		{Role: "user", Content: "list the files"},
		{Role: "assistant", Reasoning: "I should call the tool first.", Content: "", ToolCalls: []ToolCall{
			{ID: "d1", Type: "function", Function: ToolFunction{Name: "note", Arguments: `{"spec":{"title":"a","tags":["x"]}}`}}}},
		{Role: "tool", ToolCallID: "d1", Content: "ok"},
		{Role: "user", Content: "Reply with the single word: done."},
	}
	_, err := c.Chat(ctx, ChatRequest{MaxTokens: 256, Messages: msgs,
		Headers: map[string]string{"x-session-id": "lca-doctor-replay-" + c.model, "x-root-session-id": "lca-doctor"}}, StreamSink{})
	if err == nil {
		res.notes = append(res.notes, "reasoning replay accepted")
		return
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
		res.status = "fail"
		res.detail += gSep + "the server rejected the model's own reasoning in the history: " + shortErr(err)
		res.fix = "set models." + c.model + ".reasoning_replay: off (the model then loses its plan between steps — better: fix the server's chat template)"
		return
	}
	res.warns = append(res.warns, "could not check reasoning replay: "+shortErr(err))
}

// memberAllowlist is the list in force ON a member: its own if it has one, else
// the team's. A member's list replaces the team's rather than intersecting it,
// so a GPU node that allows only [python3, bsk] is checked for exactly those.
func memberAllowlist(cfg Config, roles *RolesConfig, m *Member) []string {
	if m != nil && m.Allow != nil {
		return m.Allow
	}
	return allowlistOf(cfg, roles)
}

// doctorOrchestrator is the fleet as the runtime resolves it, without starting a
// session: doctor must report the members that will actually be used (including
// the one LCA_REMOTE alone defines), not just what roles.yaml spelled out.
func doctorOrchestrator(cfg Config, roles *RolesConfig, mset *MCPSet) *Orchestrator {
	// cfg, because with no roles.yaml doctor still has to name the one agent there is
	// (cfg.Agent) when it reports what that agent's request prefix carries.
	o := &Orchestrator{cfg: cfg, agents: map[string]*Agent{}, mcp: mset}
	if roles != nil {
		o.roles = roles
		// Only the roles, not the markdown agents: what a workflow's steps name is
		// a role, and Workflow.bind resolves it through o.agents like the runner.
		for _, r := range roles.Roles {
			o.agents[r.Name] = r
		}
	}
	o.setFleet(roles)
	jl, err := NewJail(cfg.Root, allowlistOf(cfg, roles), cfg.Unsafe)
	if err != nil {
		return nil
	}
	if roles != nil && roles.Shell {
		jl.Shell = true
	}
	o.jl = jl
	if mset != nil {
		mset.jl = jl // consulted only when a stdio server is spawned
	}
	return o
}

// allowlistOf is the sandbox allowlist in force: roles.yaml's, else the config's.
func allowlistOf(cfg Config, roles *RolesConfig) []string {
	if roles != nil && len(roles.Allow) > 0 {
		return roles.Allow
	}
	return cfg.Allowed
}

// reportProfiles is the per-served-model provenance report: which table entry a
// served id matched, what the client will therefore send, and where the running
// deployment and the table disagree about the context window.
//
// Nothing here fails doctor. Sending nothing is a valid and safe configuration —
// this codebase's whole rule is that it beats guessing — so a model nobody has
// written numbers for is a warn, and doctor's exit code does not turn red because
// a model is new. What it must never be is silent: a quiet "no numbers" is how
// this class of bug hides.
// endpointEng is the fallback, for a card that names no engine of its own; roles
// is read because the per-model engine is the whole reason this section is a loop
// — one url can front hy3 on SGLang and the next model on vLLM, and advising a
// pinned model the OTHER engine's flag spellings is advice its argparse refuses.
func reportProfiles(cfg Config, endpoint string, served map[string]ModelInfo, roles *RolesConfig, endpointEng string) {
	section("models", faint("what lca knows about each served model"))
	for _, id := range sortedKeys(served) {
		info, prof := served[id], lookupProfile(id)
		// This model's OWN engine, resolved exactly as the runtime resolves it, so
		// every flag and every switch name in this model's rows is the one the
		// server that serves IT accepts.
		engine := endpointEng
		if e, _ := doctorEngine(cfg, roles, id, info); e != "" {
			engine = e
		}
		norm := normalizeModelID(id)
		label := id
		if norm != strings.ToLower(id) {
			label += faint(" (normalised %q)", norm)
		}
		if prof.Family == "" {
			warnLine("%s matched no profile", label)
			hint("lca will send: no temperature, no top_p, no top_k, no max_tokens, no thinking switch, and will not replay the model's reasoning — the server's own defaults apply")
			hint("fix: add a profile in models.go, or set models.%s: {temperature, top_p, effort, reasoning_replay} in roles.yaml from the model card", id)
			reportWindow(id, info, prof, rememberedWindow(cfg, endpoint, id))
			reportReplyBudget(cfg, id, info)
			continue
		}
		okLine("%s %s", label, faint("%s%s"+gSep+"matched %q", gSep, prof.Family, prof.Key))
		row("sends", faint("temperature %s"+gSep+"top_p %s"+gSep+"top_k %s"+gSep+"max output %s",
			srcFloat(prof.Temperature, prof.Src.Temperature), srcFloat(prof.TopP, prof.Src.TopP),
			srcNum(prof.TopK, prof.Src.TopK), srcNum(prof.Output, prof.Src.Output)+" capped at "+kfmt(outputTokenMax)))
		effort := "nothing recorded — no level is sent"
		switch {
		case prof.EffortNone:
			effort = "none — the vendor documents no reasoning_effort for it, so only its switch is sent"
		case len(prof.EffortsLocal) > 0:
			// The gateway is self-hosted, so quote the list the template accepts:
			// the wider one is the hosted API resolving its own aliases.
			effort = strings.Join(prof.EffortsLocal, "|") + gSep + "default " + prof.EffortOn +
				faint(" (self-hosted vocabulary; the hosted API also resolves %s)", strings.Join(prof.Efforts, "|"))
		case len(prof.Efforts) > 0:
			effort = strings.Join(prof.Efforts, "|") + gSep + "default " + prof.EffortOn
		}
		row("thinking", faint("%s"+gSep+"replay %s"+gSep+"effort %s", reasoningSwitchName(prof, engine), replayName(prof.Replay), effort))
		// One renderer, so the flags doctor reports and the flags doctor advises
		// after a failed probe cannot drift — and in the resolved engine's own
		// spelling, because the values are not interchangeable.
		row("engine flags", faint("%s", prof.engineFlags(engine)))
		if prof.Note != "" {
			row("caveat", faint("%s", prof.Note))
		}
		reportWindow(id, info, prof, rememberedWindow(cfg, endpoint, id))
		reportReplyBudget(cfg, id, info)
	}
}

// reportWindow is the three-way comparison between the deployment and the table.
// Which one wins is not in question — Client.CtxLen prefers max_model_len, and
// that precedence is right — but the difference has to be visible rather than
// silently reconciled.
func reportWindow(id string, info ModelInfo, prof ModelProfile, learned int) {
	switch {
	case info.MaxLen > 0 && prof.Context > 0 && info.MaxLen < prof.Context:
		warnLine("%s: the deployment serves %s; the table says %s (%s)", id, ctxfmt(info.MaxLen), ctxfmt(prof.Context), prof.Src.Context)
		hint("lca budgets from %s (the server is the truth of the running deployment) — a role whose context: is above it will be refused", ctxfmt(info.MaxLen))
		hint("fix: raise --max-model-len on the gateway, or lower the role's context:")
	case info.MaxLen > 0 && prof.Context > 0 && info.MaxLen > prof.Context:
		warnLine("%s: the deployment serves %s; the profile says %s (matched rule %q)", id, ctxfmt(info.MaxLen), ctxfmt(prof.Context), prof.Key)
		hint("lca already budgets from the server, so nothing is broken right now")
		hint("fix: the profile is stale or matched the wrong entry — check lookupProfile in models.go")
	case info.MaxLen > 0:
		row("context", faint("%s — the deployment's own max_model_len", srcNum(info.MaxLen, OriginServer)))
	case prof.Context > 0:
		row("context", faint("%s — the endpoint does not report max_model_len; verify it matches the server's --max-model-len / --context-length", srcNum(prof.Context, prof.Src.Context)))
	case learned > 0:
		// Third source, and as authoritative as the first: the deployment told us
		// this number itself, by refusing a prompt that did not fit.
		row("context", faint("%s — learned from this deployment's own refusal, remembered in windows.json", srcNum(learned, OriginServer)))
	default:
		warnLine("%s: neither the endpoint nor the table knows this model's window", id)
		hint("the context budget falls back to %d tokens, which is a placeholder and not a real window — set context: on the role, or add the model's window to models.go", ctxBudgetFallback)
	}
}

// reportReplyBudget says when a configured max_tokens will not be sent as
// configured. body() clamps the reply budget to a quarter of the window the
// deployment reports, because BOTH engines refuse a larger one outright — each
// with its own message, and neither recognised by the overflow retry — but an operator's own number being
// rewritten to a different one was the last silent reconciliation on this path,
// and this file's rule is that a number names where it came from.
func reportReplyBudget(cfg Config, id string, info ModelInfo) {
	if cfg.MaxTokens <= 0 || info.MaxLen <= 0 || cfg.MaxTokens <= info.MaxLen/4 {
		return
	}
	warnLine("%s: the configured max_tokens %s is more than a quarter of the deployment's %s window — lca sends %s instead",
		id, kfmt(cfg.MaxTokens), ctxfmt(info.MaxLen), kfmt(info.MaxLen/4))
	// Both engines refuse it, each with its own message: "vLLM refuses" was a
	// vLLM-shaped sentence about a clamp that is not vLLM-specific.
	hint("the clamp is deliberate (vLLM and SGLang both refuse a reply budget they cannot honour, each with its own message); lower LCA_MAX_TOKENS or raise --max-model-len / --context-length to make the two agree")
}

// reasoningSwitchName names the switch this model's own chat template reads, so
// an operator can see that the four served models the vLLM dialect used to send
// enable_thinking to are now sent what they actually read. The engine matters to
// the sentence and not to the key: SGLang pops chat_template_kwargs.reasoning_effort
// and promotes it to the top level itself, so one key covers both carriers there.
func reasoningSwitchName(prof ModelProfile, engine string) string {
	sw := prof.Switch
	switch {
	case sw.Kwarg != "" && sw.Effort != "":
		return "chat_template_kwargs." + sw.Kwarg + " + ." + sw.Effort
	case sw.Kwarg != "":
		return "chat_template_kwargs." + sw.Kwarg
	case sw.Effort != "" && sw.TopLevel:
		return "top-level " + sw.Effort + " (no on/off switch)"
	case sw.Effort != "" && engine == engineSGLang:
		// Sourced: SGLang pops the kwarg, assigns request.reasoning_effort by plain
		// attribute set (bypassing its own tier validation) and re-injects it as a
		// template kwarg verbatim — so the one key lca sends reaches the template
		// whichever carrier the engine prefers.
		return "chat_template_kwargs." + sw.Effort + faint(" (SGLang promotes it to the top level itself)")
	case sw.Effort != "":
		return "chat_template_kwargs." + sw.Effort
	case prof.Reasoning:
		return "no documented switch — the server's default applies"
	}
	return "not a thinking model"
}

func replayName(replay string) string {
	switch replay {
	case "all":
		return "every step"
	case "turn":
		return "this turn"
	}
	return "not replayed"
}

// optTemp / optTopP / optEffort read a roles.yaml models: block that may not be
// there, so the callers above stay one expression per column.
func optTemp(o *ModelOpts) *float64 {
	if o == nil {
		return nil
	}
	return o.Temperature
}

func optTopP(o *ModelOpts) *float64 {
	if o == nil {
		return nil
	}
	return o.TopP
}

func optEffort(o *ModelOpts) string {
	if o == nil {
		return ""
	}
	return o.Effort
}

// thinkingExpected reports whether a reply with no reasoning in it is a symptom.
// It is one when the role asked for thinking, and also when the model documents
// no way to switch thinking off (Kimi-K3, GLM-5.3) — those two always think, so
// silence means the engine is not surfacing the trace.
//
// It has to ask about the effort the CLIENT SENT, not the one the role wrote: a
// level outside the model's vocabulary is dropped before the request is built
// (models.go effortFor), and on a model whose only switch IS reasoning_effort
// that leaves the probe asking for nothing at all. Asking the role's question
// made doctor print "--reasoning-parser hy_v3" for a model it had just told the
// server not to make think, two lines under its own warning that the level was
// not sent.
func thinkingExpected(prof ModelProfile, effort string) bool {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e != "" && e != "on" && e != "off" && e != "none" && prof.effortForLocal(e) == "" && prof.Switch.Kwarg == "" {
		e = "" // dropped, and no switch went in its place: nothing was asked
	}
	switch {
	case e == "":
		// Nothing asked: only the models that cannot stop thinking are expected to.
		return prof.Switch.Kwarg == "" && prof.EffortOff == ""
	case e == "off" || e == "none":
		return false
	case prof.EffortOff != "" && e == prof.EffortOff:
		return false // hy3's no_think
	}
	return true
}
