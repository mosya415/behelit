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
	// The theme is resolved before anything can print. usage() and fatal() write
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
	flag.Usage = usage
	flag.Parse()
	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))

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
	orch, err := setupOrchestrator(cfg, ap, os.Getenv("LCA_TRACE"))
	if err != nil {
		fatal(err)
	}
	defer orch.rec.Close()
	defer orch.tracer.Close()
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
		fatal(err)
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
		os.Exit(oneShot(orch, sess, prompt, *checkFlag, append(notes, orch.warnings...)))
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

// oneShot runs a single task and returns the exit code. stdout carries only
// the answer; notes and the verdict go to stderr. With -check the verifier,
// not the model, decides success.
func oneShot(orch *Orchestrator, sess *Session, prompt, check string, notes []string) int {
	for _, n := range notes {
		// reconcileModel() hands these back pre-coloured for the banner's
		// contValue() rows, so they are stripped on the way to a stderr that may
		// well be a file. The theme already makes a piped run plain; this is the
		// case it cannot see, a terminal stdout with stderr redirected.
		fmt.Fprintln(os.Stderr, stripANSI(n))
	}
	orch.rec.Event("user", map[string]any{"text": prompt, "mode": "one-shot"})
	sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: prompt})
	if check == "" {
		err := sess.Run(context.Background())
		sess.saveTranscript()
		if err != nil {
			return 1
		}
		return 0
	}
	start := time.Now()
	v := sess.RunVerified(context.Background(), check, orch.verifyAttempts())
	sess.traceTask(prompt, v, check, 0, 0, false, nil, start, "")
	sess.saveTranscript()
	fmt.Fprintf(os.Stderr, "\n%s  %s\n", statusWord(v.Status), faint("%s"+gSep+"%s", check, plural(v.Attempts, "attempt", "attempts")))
	if v.Status != "passed" {
		if v.Tail != "" {
			fmt.Fprintln(os.Stderr, v.Tail)
		}
		return 1
	}
	return 0
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, cRed+gDown+cReset+" "+err.Error())
	if h := errorHint(err); h != "" {
		fmt.Fprintln(os.Stderr, cFaint+gHint+" "+h+cReset)
	}
	os.Exit(1)
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
	fc, err := loadFileConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	local := NewClient(cfg)
	var warns []string
	gwModels := -1
	if roles != nil {
		warns, gwModels = validateRoleModels(roles, local)
	}
	orch := NewOrchestrator(cfg, fc, jail, ap, rec, local, roles, tracer)
	orch.gatewayModels = gwModels
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
		"   lca init                     create .lca/roles.yaml from the gateway's models",
		"   lca doctor                   check gateway, roles, tool calls, members, workflows",
		"   lca run <name>               run a workflow (deterministic steps; -list, -dry-run)",
		"   lca eval tasks/              run evaluation tasks (see README)",
		"   lca report                   render the newest trace as one HTML file",
		"",
		" " + f("FLAGS"),
		"   -role <name>                 role (roles.yaml) or agent to run as",
		"   -model <name>                model override: gateway name or provider/model",
		"   -tier <name>                 run every tier-declaring role on that chain (roles.yaml tiers:)",
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
