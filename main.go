package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	// The theme is resolved before anything can print. usage() and fatalCode() write
	// chrome to stderr and can fire before the config is read, and a run whose
	// stdout is a pipe must not have put an escape in it by then.
	applyTheme("")
	cfg, _, cfgSrc := loadConfigWithSources()
	applyTheme(cfg.Theme) // now the `theme` key and LCA_THEME get their say
	if cfg.CmdTimeout > 0 {
		cmdTimeout = time.Duration(cfg.CmdTimeout) * time.Second
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "eval":
			os.Exit(runEval(context.Background(), cfg, os.Args[2:]))
		case "run":
			os.Exit(runWorkflow(cfg, os.Args[2:]))
		case "init":
			os.Exit(runInit(cfg, os.Args[2:]))
		case "doctor":
			os.Exit(runDoctor(context.Background(), cfg, os.Args[2:]))
		case "report":
			os.Exit(runReport(cfg, os.Args[2:]))
		case "version", "--version":
			os.Exit(runVersion(cfg, os.Args[2:]))
		// clean and merge are the two things only a human does, so they are
		// subcommands and not tools: removing what a session left behind, and
		// finishing a merge a model was told to hand over. Neither of them talks to
		// a gateway, which is why they are here and not after the client is built.
		case "clean":
			os.Exit(runClean(cfg, os.Args[2:]))
		case "merge":
			os.Exit(runMerge(cfg, os.Args[2:]))
		case "help", "--help":
			usage()
			return
		}
	}

	yes := flag.Bool("y", false, "")
	yesLong := flag.Bool("yes", false, "")
	unsafe := flag.Bool("unsafe", false, "")
	resume := flag.Bool("resume", false, "")
	agentFlag := flag.String("agent", "", "")
	roleFlag := flag.String("role", "", "")
	modelFlag := flag.String("model", "", "")
	checkFlag := flag.String("check", "", "")
	tierFlag := flag.String("tier", "", "")
	// A flag.Value and not flag.String, so a SECOND -prompt-file is a usage error
	// rather than the quiet winner. A wrapper whose base command already carries
	// `-prompt-file ticket.md` and which appends a per-role `-prompt-file
	// review.md` ran the review prompt against the ticket's worktree, reported
	// `passed` on a green check, and had the wrapper commit, push and open a merge
	// request for work nobody asked for. The sibling mistake — -prompt-file plus a
	// positional — was already refused, and resolvePrompt's own comment gives the
	// principle: a wrapper that passes both has a bug.
	promptFile := &oncePath{flag: "-prompt-file"}
	flag.Var(promptFile, "prompt-file", "")
	jsonFlag := flag.Bool("json", false, "")
	// The run's budgets and its closing summary. -timeout and -summary describe
	// one RUN and are refused below when there is no task: a -timeout silently
	// ignored by an interactive session is the kind of flag a wrapper keeps passing
	// for a year while nothing enforces it. The two ceilings are meaningful in a
	// session too — they bound each turn and its subagents — so they are not.
	timeoutFlag := flag.Duration("timeout", 0, "")
	maxStepsFlag := flag.Int("max-steps", 0, "")
	maxTokensFlag := flag.Int("max-tokens", 0, "")
	summaryFlag := flag.String("summary", "", "")
	flag.Usage = usage
	// flag's own parse failure already exits 2, which is the table's "lca was
	// called wrong" — the same code the two usage errors below produce.
	flag.Parse()

	// Under -json stdout belongs to the caller's parser and to nothing else. The
	// whole tree prints through fmt.Print*, which reaches os.Stdout as a variable,
	// so moving the variable moves every note, warning, door, tool line and the
	// model's own prose to stderr in one move — and the result object is written to
	// the descriptor stdout WAS. Colour goes too: a program reading stderr for a
	// log should not have to strip escapes.
	result := os.Stdout
	if *jsonFlag {
		os.Stdout = os.Stderr
		applyTheme(themePlain)
	}

	perr := checkTrailingFlags(flag.Args(), os.Args)
	var prompt string
	if perr == nil {
		prompt, perr = resolvePrompt(flag.Args(), promptFile.val, os.Stdin)
	}
	if perr == nil && *jsonFlag && prompt == "" {
		perr = usageErrf("-json reports one task's result: give the task as an argument, or with -prompt-file")
	}
	if perr == nil && prompt == "" {
		switch {
		case *timeoutFlag != 0:
			perr = usageErrf("-timeout bounds one run: give the task as an argument, or with -prompt-file")
		case *summaryFlag != "":
			perr = usageErrf("-summary is written at the end of one run: give the task as an argument, or with -prompt-file")
		}
	}
	// A -summary path that cannot be written is discovered at the END of the run
	// otherwise — after the work, after the check and after one more model call
	// bought to write a file that then goes nowhere. The commonest shape is a
	// wrapper interpolating an empty variable, so `-summary $WORKTREE/summary.md`
	// becomes `/summary.md`: MkdirAll("/") succeeds, the write gets EACCES, and the
	// run reports `passed` / exit 0 with an empty merge request description and
	// nothing alerting anybody. Probing it here makes it row 2, before the gateway
	// is touched.
	if perr == nil && prompt != "" && *summaryFlag != "" {
		perr = probeWritable(*summaryFlag, "-summary")
	}
	if perr != nil {
		fatalCode(exitUsage, perr)
	}

	cfg.Unsafe = cfg.Unsafe || *unsafe
	if *tierFlag != "" {
		cfg.Tier = *tierFlag
		mark(cfgSrc, "tier", SrcFlag, "-tier")
	}
	if *modelFlag != "" {
		mark(cfgSrc, "model", SrcFlag, "-model")
	}
	in := NewInput(os.Stdin)
	ap := NewApprover(in)
	// The persisted posture first, then the flag on top: -y is what the operator
	// asked for on THIS run, and a config file must not be able to take it back.
	applyApproveTo(ap, cfg.Approve)
	if *yes || *yesLong {
		ap.TrustAll()
		mark(cfgSrc, "approve", SrcFlag, "-y")
	}
	// Nobody at the keyboard: every question becomes an immediate, recorded
	// refusal. -y does not cover this — it deliberately does not grant mcp_write,
	// and that door would have been the one thing left waiting for an answer that
	// is never typed. A hang in a cron job is a worktree and a claimed ticket held
	// until somebody notices.
	if *jsonFlag {
		ap.Unattended("-json: this run answers to a program, not to a person")
	} else if !in.IsTTY() {
		ap.Unattended("stdin is not a terminal")
	}
	orch, err := setupOrchestrator(cfg, ap, os.Getenv("LCA_TRACE"))
	if err != nil {
		// Not 1, which every startup failure used to be: a roles.yaml that will not
		// parse or a config file with an
		// unknown key is the table's row 2 — alert somebody, do not touch the ticket.
		fatalCode(exitUsage, err)
	}
	// The budgets after the orchestrator, because roles.yaml's defaults: are the
	// floor the flags override and the file has only just been read.
	budget, berr := newRunBudget(orch.roles, *timeoutFlag, *maxStepsFlag, *maxTokensFlag)
	if berr != nil {
		fatalCode(exitUsage, berr)
	}
	orch.budget, orch.summary = budget, *summaryFlag
	defer orch.rec.Close()
	defer orch.tracer.Close()
	// A stdio MCP server is a child process of ours: reap it on the way out, the
	// same way the recorder and the tracer are closed.
	defer orch.CloseMCP()
	local := orch.providers.local

	// Model: -model > LCA_MODEL when it names a provider > config "model" > the
	// role's chain / the endpoint's LCA_MODEL.
	ref := *modelFlag
	if _, _, ok := orch.providers.Split(cfg.Model); ok {
		local.SetModel("local")
		ref = firstNonEmpty(ref, cfg.Model)
	}
	if ref == "" && os.Getenv("LCA_MODEL") == "" && orch.fc.Model != "" {
		ref = orch.fc.Model
	}
	agentName := firstNonEmpty(*roleFlag, *agentFlag, os.Getenv("LCA_AGENT"))
	if agentName == "" && orch.roles != nil {
		agentName = orch.roles.Entry
	}
	agentName = firstNonEmpty(agentName, cfg.Agent)
	sess, err := orch.NewPrimary(agentName, ref, nil)
	if err != nil {
		// An unknown role or an unknown model is a name in a file or on the command
		// line, not a failure of the task: row 2 again.
		fatalCode(exitUsage, err)
	}
	sess.view = newTermView(sess)
	sess.Loop, sess.ShowThink, sess.Raw = cfg.Loop, cfg.ShowThinking, cfg.Raw

	var notes []string
	if cfg.Discover && sess.client == local {
		notes = reconcileModel(orch.providers, local, orch.rec)
	} else {
		// One cached /v1/models for the whole process: the window the deployment
		// reports outranks the card everywhere else, and startup was the one
		// place that never asked. A role chain has already warmed the cache in
		// NewPrimary, so this costs nothing there.
		orch.providers.Learn(sess.client)
	}
	orch.rec.Event("session_start", map[string]any{
		"root": orch.jl.Root, "model": sess.client.Ref(), "endpoint": sess.client.Endpoint(), "agent": sess.agent.Name,
		"tier": orch.activeTier(),
	})
	defer orch.rec.Event("session_end", nil)
	pruneTranscripts(filepath.Join(cfg.stateDir(), "transcripts"), cfg.KeepSessions)

	if prompt != "" {
		code := oneShot(orch, sess, prompt, *checkFlag, *jsonFlag, result, append(notes, orch.warnings...))
		// os.Exit runs no defers, and a one-shot has the same things to close as a
		// session: the stdio MCP children first, because they are PROCESSES and a leaked
		// one outlives lca along with its process group, then the trace and the audit
		// log. Ctrl-C during a one-shot took this path too.
		orch.rec.Event("session_end", nil)
		orch.CloseMCP()
		orch.tracer.Close()
		orch.rec.Close()
		os.Exit(code)
	}

	// roles.yaml owns the entry role's chain, its effort and the transport, and
	// /config has to be able to say so — see markRoleSources.
	markRoleSources(cfgSrc, orch.jl.Root, orch.roles, sess.agent)
	r := &Repl{cfg: cfg, orch: orch, sess: sess, local: local, in: in, notes: notes, cfgSrc: cfgSrc}
	if os.Getenv("LCA_NO_CLEAR") == "" {
		clearScreen()
	}
	r.Banner()
	if *resume {
		r.cmdResume("")
	}
	// Nothing configured here and a terminal to ask on: offer the wizard once,
	// before the prompt opens. It is an offer, not a gate — Enter accepts, n
	// falls straight through to the prompt.
	r.offerSetup()
	r.Loop()
}

// fatalCode prints and exits with the code the pipeline's table gives the
// reason: 2 for "lca was called wrong, or configured wrong". The message goes to
// stderr, which is where it goes under -json too, because stdout there carries
// the result object and nothing else — and a run that dies here has no result.
func fatalCode(code int, err error) {
	fmt.Fprintln(os.Stderr, cRed+gDown+cReset+" "+err.Error())
	if h := errorHint(err); h != "" {
		fmt.Fprintln(os.Stderr, cFaint+gHint+" "+h+cReset)
	}
	os.Exit(code)
}

// setupOrchestrator builds everything a run needs from cfg: jail (with the
// roles.yaml sandbox allowlist), file config, roles (validated against the
// gateway's /v1/models), recorder and trace. tracePath "" = the default
// $LCA_DIR/traces/<session>.jsonl. It keeps its signature and its callers; the
// half a live reload can repeat is buildOrchestrator below.
func setupOrchestrator(cfg Config, ap *Approver, tracePath string) (*Orchestrator, error) {
	rec, err := NewRecorder(cfg)
	if err != nil {
		return nil, fmt.Errorf("recorder init failed: %w", err)
	}
	if tracePath == "" {
		tracePath = filepath.Join(cfg.stateDir(), "traces", rec.id+".jsonl")
	}
	tracer, err := NewTracer(tracePath)
	if err != nil {
		rec.Close()
		return nil, fmt.Errorf("trace: %w", err)
	}
	o, err := buildOrchestrator(cfg, ap, rec, tracer)
	if err != nil {
		tracer.Close()
		rec.Close()
		return nil, err
	}
	return o, nil
}

// buildOrchestrator is everything setupOrchestrator does except creating the
// recorder and the tracer, so a reload can reuse them. One session must write
// ONE audit log and ONE trace: rotating them mid-session would make /report
// render half a conversation.
func buildOrchestrator(cfg Config, ap *Approver, rec *Recorder, tracer *Tracer) (*Orchestrator, error) {
	// The config file is read FIRST, before the roles, because the mcp block lives
	// in it and MCP tools have to be in toolRegistry before loadRoles runs:
	// roles.yaml's tools: list is validated against the registry, so a role that
	// names jira__issue_get needs the tool to exist by then. Registration is two
	// file reads and no network, which is what makes that ordering affordable on
	// every lca, lca doctor, lca run and every eval task.
	fc, err := loadFileConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	mset, mcpWarns := loadMCP(cfg, fc)
	mcpWarns = append(mcpWarns, registerMCPTools(mset)...)

	roles, err := loadRoles(cfg)
	if err != nil {
		return nil, fmt.Errorf("roles: %w", err)
	}
	if len(roles.Roles) == 0 {
		roles = nil
	}
	jail, err := NewJail(cfg.Root, allowlistOf(cfg, roles), cfg.Unsafe)
	if err != nil {
		return nil, fmt.Errorf("jail init failed: %w", err)
	}
	if roles != nil && roles.Shell {
		jail.Shell = true
	}
	// The jail is consulted only when a stdio server is spawned, which is why MCP
	// could be registered before it existed.
	mset.jl = jail
	local := NewClient(cfg)
	var warns []string
	gwModels := -1
	if roles != nil {
		warns, gwModels = validateRoleModels(roles, local)
	}
	orch := NewOrchestrator(cfg, fc, jail, ap, rec, local, roles, tracer)
	orch.mcp = mset
	orch.gatewayModels = gwModels
	warns = append(warns, mcpWarns...)
	if roles != nil {
		warns = append(warns, roles.Warnings...)
	}
	orch.warnings = append(warns, orch.warnings...)
	return orch, nil
}

func usage() {
	b := func(s string) string { return cBold + s + cReset }
	f := func(s string) string { return cFaint + s + cReset }
	lines := []string{
		"",
		" " + b("BEHELIT") + f(" — coding agent and orchestrator for open models"),
		"",
		" " + f("USAGE"),
		"   lca                          interactive session",
		"   lca \"<task>\"                 run one task and exit",
		"   lca -check \"<cmd>\" \"<task>\"  run one task; succeed only if <cmd> passes",
		"   lca -prompt-file t.md        take the task from a file (- = stdin), not from argv",
		"   lca -json …                  one JSON result object on stdout, everything else on stderr",
		"   lca init                     create .lca/roles.yaml from the gateway's models",
		"   lca doctor                   check gateway, roles, tool calls, members, workflows",
		"   lca doctor -role <name>      also the sandbox and permission rules in force for that role",
		"   lca doctor -role <name> -y   the same, answered as `lca -y` would: an ask becomes an allow",
		"   lca run <name>               run a workflow (deterministic steps; -list, -dry-run)",
		"   lca eval tasks/              run evaluation tasks (see README)",
		"   lca report                   render the newest trace as one HTML file",
		"   lca version                  the build's sha and the hash of the roles.yaml in force",
		"   lca merge <branch>           check a delegation's branch out as a real merge (apply: branch)",
		"   lca clean [--branches]       remove what dead sessions left behind",
		"",
		" " + f("FLAGS"),
		"   -role <name>                 role (roles.yaml) or agent to run as",
		"   -model <name>                model override: gateway name or provider/model",
		"   -tier <name>                 run every tier-declaring role on that chain (roles.yaml tiers:)",
		"   -prompt-file <path>          read the task from a file, or from stdin with -",
		"   -json                        result object on stdout; exit 0 passed, 1 failed, 2 usage,",
		"                                3 infra, 4 budget, 130 cancelled",
		"   -timeout <dur>               budget for the whole run; exceeding it is exit 4",
		"   -max-steps <n>               step ceiling for the run, over every role's own",
		"   -max-tokens <n>              prompt+completion ceiling for the run, subagents included",
		"   -summary <path.md>           write a short markdown summary of the run there",
		"   -y                           approve edits, commands and fetches without asking",
		"   -resume                      continue the most recent session",
		"   -unsafe                      lift the sandbox (any path, any command)",
		"",
		" " + f("SETUP"),
		"   lca                          interactive session; /setup picks the models and gives them roles",
		"   .lca/config.json             the endpoint and the settings (/set writes it, /config explains it)",
		"   .lca/roles.yaml              the team (/setup or lca init writes one)",
		"   LCA_BASE_URL                 an override for one run — you never need it to get started",
		"   DEEPSEEK_API_KEY, …          keys for hosted presets, when not using a gateway",
		"",
		" " + f("All settings: README.md. In a session, /help lists the commands."),
		"",
	}
	fmt.Fprintln(os.Stderr, strings.Join(lines, "\n"))
}
