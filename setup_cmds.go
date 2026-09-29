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
	"strconv"
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
		hint("lca init -force overwrites it · lca doctor checks it")
		return 1
	}

	gw := NewClient(cfg)
	models, err := gw.ListModels()
	if err != nil {
		errLine("can't list models at %s: %v", gw.Endpoint(), err)
		hint("point LCA_BASE_URL at the gateway, e.g. export LCA_BASE_URL=http://node:18080/v1")
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

  cheap:
    description: Summaries and compaction.
    models: [%s]
    effort: off
    context: 32000
    tools: []
`, filepath.Base(cfg.Root), strings.TrimRight(gw.Endpoint(), "/"), remoteBlock, defaultsMember, strings.Join(allow, ", "),
		strings.Join(lead, ", "), strings.Join(coder, ", "), checkLine(check), cheap)

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
		row("member", m.name+faint(" · %s:%s", m.host, m.dir))
	}
	if len(members.list) > 1 {
		hint("pin a role with \"member: <name>\" under it")
	}
	table([]string{"role", "models", "check"}, [][]string{
		{"lead", strings.Join(lead, faint(" → ")), faint("—")},
		{"coder", strings.Join(coder, faint(" → ")), faint("%s", firstNonEmpty(check, "— add check_cmd"))},
		{"cheap", cheap, faint("—")},
	})
	row("sandbox", faint("%s", strings.Join(allow, " ")))
	if *remote != "" {
		row("remote", *remote+faint(" · files and commands go there over ssh"))
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
	smallHint = []string{"flash", "mini", "air", "lite", "small", "turbo", "a3b", "30b", "14b", "8b", "7b"}
)

func isSmall(m string) bool {
	l := strings.ToLower(m)
	for _, h := range smallHint {
		if strings.Contains(l, h) {
			return true
		}
	}
	return false
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

func pickCheap(names []string) string {
	for _, m := range names {
		if isSmall(m) {
			return m
		}
	}
	return names[len(names)-1]
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

func runDoctor(cfg Config, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	noProbe := fs.Bool("no-probe", false, "don't call the models (only list and configuration checks)")
	all := fs.Bool("all", false, "probe every model in every chain, not just the first of each role")
	tierFlag := fs.String("tier", "", "run every tier-declaring role on that chain (roles.yaml tiers:)")
	memberFlag := fs.String("member", "", "probe only this member (roles.yaml members:)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// doctor is the subcommand whose job is showing the resolved team, so it has
	// to be pointable at a tier like every other one.
	cfg.Tier = firstNonEmpty(*tierFlag, cfg.Tier)
	failed := false
	fail := func(format string, a ...any) { failed = true; errLine(format, a...) }

	section("gateway")
	gw := NewClient(cfg)
	row("url", gw.Endpoint())
	models, err := gw.ListModels()
	served := map[string]bool{}
	switch {
	case err != nil:
		fail("unreachable: %v", err)
		hint("%s", firstNonEmpty(errorHint(err), "is the gateway up? LCA_BASE_URL must point at it (…/v1)"))
	default:
		for _, m := range models {
			served[m.ID] = true
		}
		okLine("up · %s listed", plural(len(models), "model", "models"))
	}

	section("roles")
	roles, rerr := loadRoles(cfg)
	switch {
	case rerr != nil:
		fail("%v", rerr)
	case len(roles.Roles) == 0:
		warnLine("no roles.yaml — single-agent mode")
		hint("lca init writes one from the gateway's models")
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
		var rows [][]string
		for _, r := range roles.Roles {
			prof := lookupProfile(firstNonEmpty(r.Models[0], ""))
			budget := r.Context
			if budget == 0 {
				budget = prof.Context
			}
			if prof.Context > 0 && r.Context > prof.Context {
				warnLine("role %s: context %s is larger than %s's known window %s — requests will be refused",
					r.Name, kfmt(r.Context), r.Models[0], kfmt(prof.Context))
			}
			if prof.Family == "" && r.Context == 0 {
				warnLine("role %s: %s is not a family we have numbers for — set context: in roles.yaml (the model card's window)", r.Name, r.Models[0])
			}
			var chain []string
			for _, m := range r.Models {
				switch {
				case len(served) == 0:
					chain = append(chain, m)
				case served[m]:
					chain = append(chain, cGreen+m+cReset)
				default:
					chain = append(chain, cRed+m+cReset)
					failed = true
				}
			}
			temp, topP := "model default", "model default"
			if r.Temperature != nil {
				temp = strconv.FormatFloat(*r.Temperature, 'f', -1, 64)
			} else if o := roles.ModelOpts[r.Models[0]]; o != nil && o.Temperature != nil {
				temp = strconv.FormatFloat(*o.Temperature, 'f', -1, 64)
			} else if prof.Temperature != nil {
				temp = strconv.FormatFloat(*prof.Temperature, 'f', -1, 64)
			}
			if r.TopP != nil {
				topP = strconv.FormatFloat(*r.TopP, 'f', -1, 64)
			} else if o := roles.ModelOpts[r.Models[0]]; o != nil && o.TopP != nil {
				topP = strconv.FormatFloat(*o.TopP, 'f', -1, 64)
			} else if prof.TopP != nil {
				topP = strconv.FormatFloat(*prof.TopP, 'f', -1, 64)
			}
			effort := firstNonEmpty(r.Thinking, func() string {
				if o := roles.ModelOpts[r.Models[0]]; o != nil {
					return o.Effort
				}
				return ""
			}(), "provider default")
			replay := map[string]string{"all": "every step", "turn": "this turn", "": "not replayed"}[prof.Replay]
			rows = append(rows, []string{r.Name, orDash(r.Tier), strings.Join(chain, faint(" → ")), temp, topP, effort, kfmt(budget), replay})
		}
		table([]string{"role", "tier", "models", "temp", "top_p", "effort", "context", "reasoning"}, rows)
		if failed && len(served) > 0 {
			hint("red models aren't listed by the gateway — use names from: %s", strings.Join(sortedKeys(served), ", "))
		}
	}

	// The fleet as the runtime will resolve it, built once: the checks below all
	// ask questions about a member, and two orchestrators could answer them
	// differently.
	orch := doctorOrchestrator(cfg, roles)

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
		type target struct{ role, model string }
		var targets []target
		seen := map[string]bool{}
		for _, r := range roles.Roles {
			for i, m := range r.Models {
				if (i > 0 && !*all) || seen[m] || !served[m] {
					continue
				}
				seen[m] = true
				targets = append(targets, target{r.Name, m})
			}
		}
		results := make([]probeResult, len(targets))
		var wg sync.WaitGroup
		fmt.Println("  " + faint("asking each probed model for one tool call…"))
		for i, t := range targets {
			wg.Add(1)
			go func(i int, t target) {
				defer wg.Done()
				results[i] = probeModel(cfg, gw, roles, t.role, t.model)
			}(i, t)
		}
		wg.Wait()
		for _, r := range results {
			label := r.model + faint(" (%s)", r.role)
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
			reach := probeMembers(context.Background(), orch, names)
			for _, n := range names {
				m := orch.member(n)
				if m.IsLocal() {
					continue // the workspace section below is this machine's
				}
				rem := m.Rem
				row("member", n+faint(" · %s", rem.Label()))
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
				okLine("ssh works · %s exists", rem.Dir)
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
				okLine("%s · %s%s", wf.Name, steps, faint(" (parsed; a prompt or delegate step needs a roles.yaml)"))
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
			okLine("%s · %s", wf.Name, steps)
		}
	}

	section("workspace")
	row("root", ellipsizeMiddle(shortDir(cfg.Root), 70))
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
func probeModel(cfg Config, gw *Client, roles *RolesConfig, role, model string) probeResult {
	res := probeResult{model: model, role: role}
	c := *gw
	c.model = model
	c.transport = roles.transportOf(model)
	var effort string
	for _, r := range roles.Roles {
		if r.Name == role {
			effort = r.Thinking
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
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
			res.fix = "the engine rejected tools — enable its tool-call parser (vLLM: --enable-auto-tool-choice --tool-call-parser <family>; SGLang: --tool-call-parser <family>), or set models." + model + ".transport: text"
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
		case strings.Contains(out.Content, "ping") && strings.Contains(out.Content, "{"):
			res.status = "fail"
			res.detail = "the call came back as text, not tool_calls — the engine's tool-call parser is off or wrong for this model"
			res.fix = "vLLM: --enable-auto-tool-choice --tool-call-parser <family> · SGLang: --tool-call-parser <family> · or models." + model + ".transport: text"
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
		parts = append(parts, "reasoning inline (<think>)")
	}
	if out.Usage.TTFT > 0 {
		parts = append(parts, "first token "+fmtDurShort(out.Usage.TTFT))
	}
	parts = append(parts, "total "+fmtDurShort(took))
	res.detail = strings.Join(parts, " · ")
	if c.Native() {
		probeParser(ctx, &c, &res)
	}
	probeReplay(ctx, &c, roles, &res)
	if len(res.notes) > 0 {
		res.detail += " · " + strings.Join(res.notes, " · ")
	}
	return res
}

// probeParser exercises what server-side tool-call parsers actually break on:
// two calls in one reply, and an argument with nested JSON.
func probeParser(ctx context.Context, c *Client, res *probeResult) {
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
		res.warns = append(res.warns, "asked for two tool calls, got none — the parser may drop batched calls")
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
		res.detail += " · the server rejected the model's own reasoning in the history: " + shortErr(err)
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
func doctorOrchestrator(cfg Config, roles *RolesConfig) *Orchestrator {
	o := &Orchestrator{agents: map[string]*Agent{}}
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
	return o
}

// allowlistOf is the sandbox allowlist in force: roles.yaml's, else the config's.
func allowlistOf(cfg Config, roles *RolesConfig) []string {
	if roles != nil && len(roles.Allow) > 0 {
		return roles.Allow
	}
	return cfg.Allowed
}
