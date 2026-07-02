package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strings"
)

func main() {
	cfg := loadConfig()

	yes := flag.Bool("y", false, "auto-approve side-effecting actions (for one-shot / non-interactive use)")
	yesLong := flag.Bool("yes", false, "alias for -y")
	flag.Usage = usage
	flag.Parse()
	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))

	jail, err := NewJail(cfg.Root, cfg.Allowed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "jail init failed:", err)
		os.Exit(1)
	}
	rec, err := NewRecorder(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "recorder init failed:", err)
		os.Exit(1)
	}
	defer rec.Close()

	client := NewClient(cfg)
	in := bufio.NewReader(os.Stdin)
	ap := NewApprover(in)
	if *yes || *yesLong {
		ap.SetAuto(true)
	}

	notes := reconcileModel(client, rec)
	rec.Event("session_start", map[string]any{
		"root": jail.Root, "model": client.Model(), "endpoint": cfg.BaseURL,
	})
	defer rec.Event("session_end", nil)

	msgs := []Message{{Role: "system", Content: systemPrompt(jail)}}

	// One-shot mode: a prompt on the command line runs a single turn and exits.
	// Model-reconciliation notes go to stderr so stdout carries only the answer.
	if prompt != "" {
		for _, n := range notes {
			fmt.Fprintln(os.Stderr, n)
		}
		rec.Event("user", map[string]any{"text": prompt, "mode": "one-shot"})
		msgs = append(msgs, Message{Role: "user", Content: prompt})
		runTurn(client, jail, ap, rec, &msgs, cfg.MaxSteps, cfg.CtxTokens)
		rec.Transcript(msgs)
		return
	}

	banner(cfg, jail, rec, ap, client, notes)

	for {
		fmt.Print("\n\033[36m›\033[0m ")
		line, err := in.ReadString('\n')
		if err != nil { // EOF (Ctrl-D)
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return
		case "/reset":
			msgs = msgs[:1]
			rec.Event("reset", nil)
			fmt.Println("(transcript cleared)")
			continue
		}
		if handleApproveCmd(line, ap, rec) {
			continue
		}
		if handleModelCmd(line, client, rec) {
			continue
		}

		msgs = append(msgs, Message{Role: "user", Content: line})
		rec.Event("user", map[string]any{"text": line})
		runTurn(client, jail, ap, rec, &msgs, cfg.MaxSteps, cfg.CtxTokens)
		rec.Transcript(msgs)
	}
}

// runTurn drives the agentic loop for one user message: stream the model, execute
// any tool blocks, feed results back, repeat until the model stops emitting
// tools (a final answer) or we hit the step cap.
// reconcileModel discovers what the endpoint actually serves and reconciles it
// with the configured model name. If the configured name isn't served but the
// endpoint offers exactly one model, we adopt it (the common vLLM/SGLang case:
// one model per endpoint, whose id rarely matches a hand-typed guess). Returns
// pre-colored display lines for the banner; never fatal.
func reconcileModel(client *Client, rec *Recorder) []string {
	models, err := client.ListModels()
	if err != nil {
		return []string{warnln("model discovery unavailable (%v) — using %q as-is", err, client.Model())}
	}
	if len(models) == 0 {
		return []string{warnln("endpoint advertises no models — using configured name as-is")}
	}
	if info, ok := findModel(models, client.Model()); ok {
		return []string{readyLine(info)}
	}
	if len(models) == 1 {
		prev := client.Model()
		client.SetModel(models[0].ID)
		rec.Event("model_adopt", map[string]any{"from": prev, "to": models[0].ID})
		return []string{
			readyLine(models[0]),
			dimln("adopted (configured %q not served)", prev),
		}
	}
	lines := []string{warnln("configured %q not served; choose one with /model:", client.Model())}
	return append(lines, modelTable(models, client.Model())...)
}

// handleModelCmd processes the /model REPL command. "/model" lists served models
// with their status; "/model <name>" switches for subsequent turns.
func handleModelCmd(line string, client *Client, rec *Recorder) bool {
	if line != "/model" && !strings.HasPrefix(line, "/model ") {
		return false
	}
	arg := strings.TrimSpace(strings.TrimPrefix(line, "/model"))
	models, err := client.ListModels()

	if arg == "" {
		fmt.Printf("current model: %s @ %s\n", client.Model(), client.Endpoint())
		switch {
		case err != nil:
			fmt.Printf("  \033[33mdiscovery unavailable: %v\033[0m\n", err)
		case len(models) == 0:
			fmt.Println("  (endpoint advertises no models)")
		default:
			fmt.Println("served models:")
			for _, l := range modelTable(models, client.Model()) {
				fmt.Println(l)
			}
		}
		return true
	}

	prev := client.Model()
	client.SetModel(arg)
	rec.Event("model_change", map[string]any{"from": prev, "to": arg})
	fmt.Printf("model: %s → %s\n", prev, arg)
	if err == nil && len(models) > 0 {
		if _, ok := findModel(models, arg); !ok {
			fmt.Printf("\033[33m  warning: %q is not in the endpoint's served list\033[0m\n", arg)
		}
	}
	return true
}

func findModel(ms []ModelInfo, id string) (ModelInfo, bool) {
	for _, m := range ms {
		if m.ID == id {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// describeModel renders the status detail of a served model (context window,
// backend). Empty when the server exposes neither.
func describeModel(m ModelInfo) string {
	var bits []string
	if m.MaxLen > 0 {
		bits = append(bits, fmt.Sprintf("ctx %d", m.MaxLen))
	}
	if m.OwnedBy != "" {
		bits = append(bits, m.OwnedBy)
	}
	return strings.Join(bits, ", ")
}

// modelTable renders one status line per served model: a green ● means served/
// ready, a cyan → marks the current selection, followed by its detail.
func modelTable(ms []ModelInfo, current string) []string {
	lines := make([]string, 0, len(ms))
	for _, m := range ms {
		mark := " "
		if m.ID == current {
			mark = "\033[36m→\033[0m"
		}
		line := fmt.Sprintf("  \033[32m●\033[0m %s %s", mark, m.ID)
		if d := describeModel(m); d != "" {
			line += "  \033[90m" + d + "\033[0m"
		}
		lines = append(lines, line)
	}
	return lines
}

// readyLine is the banner status for the active model.
func readyLine(m ModelInfo) string {
	s := "\033[32m● ready\033[0m"
	if d := describeModel(m); d != "" {
		s += " \033[90m— " + d + "\033[0m"
	}
	return s
}

func warnln(format string, a ...any) string {
	return "\033[33m" + fmt.Sprintf(format, a...) + "\033[0m"
}

func dimln(format string, a ...any) string {
	return "\033[90m" + fmt.Sprintf(format, a...) + "\033[0m"
}

// handleApproveCmd processes the /approve REPL command (on|off|status). Returns
// true if the line was such a command and has been handled.
func handleApproveCmd(line string, ap *Approver, rec *Recorder) bool {
	if !strings.HasPrefix(line, "/approve") {
		return false
	}
	arg := strings.TrimSpace(strings.TrimPrefix(line, "/approve"))
	switch arg {
	case "on", "all":
		ap.SetAuto(true)
		rec.Event("approve_mode", map[string]any{"auto": true})
		fmt.Println("auto-approve: ON (every action approved without prompting)")
	case "off":
		ap.SetAuto(false)
		rec.Event("approve_mode", map[string]any{"auto": false})
		fmt.Println("auto-approve: OFF (prompt for each action)")
	case "", "status":
		fmt.Printf("approval mode: %s\n", ap.Mode())
	default:
		fmt.Println("usage: /approve [on|off|status]")
	}
	return true
}

func runTurn(client *Client, jail *Jail, ap *Approver, rec *Recorder, msgs *[]Message, maxSteps, ctxTokens int) {
	for step := 0; step < maxSteps; step++ {
		printed := false
		onDelta := func(s string) {
			if !printed {
				fmt.Print("\n\033[32m●\033[0m ")
				printed = true
			}
			fmt.Print(s)
		}

		send, trimmed := trimForContext(*msgs, ctxTokens)
		if trimmed > 0 {
			fmt.Printf("\033[90m context: trimmed %d old tool outputs (~%dk tok budget)\033[0m\n", trimmed, ctxTokens/1000)
			rec.Event("context_trim", map[string]any{"collapsed": trimmed, "budget_tokens": ctxTokens})
		}
		reply, err := client.CompleteStream(send, onDelta)
		if printed {
			fmt.Println()
		}
		if err != nil {
			fmt.Println("\033[31mendpoint error:\033[0m", err)
			rec.Event("error", map[string]any{"err": err.Error()})
			return
		}
		*msgs = append(*msgs, Message{Role: "assistant", Content: reply})

		blocks := ParseBlocks(reply)
		if len(blocks) == 0 {
			return // final answer
		}

		results := executeBlocks(jail, ap, rec, blocks)
		*msgs = append(*msgs, Message{Role: "user", Content: results})
		rec.Transcript(*msgs)
	}
	fmt.Printf("\033[33m(stopped: hit %d-step cap)\033[0m\n", maxSteps)
	rec.Event("step_cap", map[string]any{"steps": maxSteps})
}

func executeBlocks(jail *Jail, ap *Approver, rec *Recorder, blocks []Block) string {
	var out strings.Builder
	for _, b := range blocks {
		var res string
		switch b.Name {
		case "read_file":
			fmt.Printf("\033[90m read_file %s\033[0m\n", b.Attr["path"])
			res = readFile(jail, b.Attr["path"], b.Attr["lines"])
			rec.Event("read_file", map[string]any{"path": b.Attr["path"], "result": summarize(res)})
		case "grep":
			fmt.Printf("\033[90m grep %q %s\033[0m\n", b.Attr["pattern"], b.Attr["path"])
			res = grepTree(jail, b.Attr["pattern"], b.Attr["path"])
			rec.Event("grep", map[string]any{"pattern": b.Attr["pattern"], "path": b.Attr["path"], "result": summarize(res)})
		case "list_dir":
			fmt.Printf("\033[90m list_dir %s\033[0m\n", b.Attr["path"])
			res = listDir(jail, b.Attr["path"])
			rec.Event("list_dir", map[string]any{"path": b.Attr["path"]})
		case "edit":
			res = gatedEdit(jail, ap, rec, b)
		case "write":
			res = gatedWrite(jail, ap, rec, b)
		case "run_command":
			res = gatedRun(jail, ap, rec, b)
		default:
			res = "error: unknown tool " + b.Name
		}
		fmt.Fprintf(&out, "<tool_result name=\"%s\" path=\"%s\">\n%s\n</tool_result>\n",
			b.Name, b.Attr["path"], res)
	}
	return out.String()
}

func gatedEdit(jail *Jail, ap *Approver, rec *Recorder, b Block) string {
	abs, err := jail.Resolve(b.Attr["path"])
	if err != nil {
		rec.Event("edit", map[string]any{"path": b.Attr["path"], "error": err.Error()})
		return "error: " + err.Error()
	}
	approved, auto := ap.Confirm("edit "+b.Attr["path"], unifiedPreview(b.Search, b.Replace))
	if !approved {
		rec.Event("edit", map[string]any{"path": b.Attr["path"], "approved": false})
		return "user denied this edit"
	}
	res, err := applyEdit(abs, b.Search, b.Replace)
	if err != nil {
		rec.Event("edit", map[string]any{"path": b.Attr["path"], "approved": true, "auto": auto, "error": err.Error()})
		return "error: " + err.Error()
	}
	rec.Event("edit", map[string]any{"path": b.Attr["path"], "approved": true, "auto": auto, "result": res})
	return res
}

func gatedWrite(jail *Jail, ap *Approver, rec *Recorder, b Block) string {
	abs, err := jail.Resolve(b.Attr["path"])
	if err != nil {
		rec.Event("write", map[string]any{"path": b.Attr["path"], "error": err.Error()})
		return "error: " + err.Error()
	}
	action := "overwrite"
	if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
		action = "create"
	}
	preview := fmt.Sprintf("  %s %s (%d bytes)", action, b.Attr["path"], len(b.Body))
	approved, auto := ap.Confirm(action+" "+b.Attr["path"], preview)
	if !approved {
		rec.Event("write", map[string]any{"path": b.Attr["path"], "approved": false})
		return "user denied this write"
	}
	res, err := writeWholeFile(abs, b.Body)
	if err != nil {
		rec.Event("write", map[string]any{"path": b.Attr["path"], "approved": true, "auto": auto, "error": err.Error()})
		return "error: " + err.Error()
	}
	rec.Event("write", map[string]any{"path": b.Attr["path"], "approved": true, "auto": auto, "bytes": len(b.Body)})
	return res
}

func gatedRun(jail *Jail, ap *Approver, rec *Recorder, b Block) string {
	cmd := strings.TrimSpace(b.Body)
	approved, auto := ap.Confirm("run", "  $ "+cmd)
	if !approved {
		rec.Event("run_command", map[string]any{"cmd": cmd, "approved": false})
		return "user denied this command"
	}
	res := runCommand(jail, cmd)
	rec.Event("run_command", map[string]any{"cmd": cmd, "approved": true, "auto": auto, "result": summarize(res)})
	return res
}

func usage() {
	fmt.Fprint(os.Stderr, `Latent Coding Agent — approval-first CLI agent for local LLM endpoints.

usage:
  lca                 start interactive REPL
  lca [-y] "<prompt>" run a single turn and exit (one-shot)

flags:
  -y, -yes            auto-approve side-effecting actions (edit/write/run_command)

config is via environment (see README): LCA_BASE_URL, LCA_MODEL, LCA_ROOT,
LCA_ALLOW, LCA_DIR, LCA_CTX_TOKENS.
`)
}

func banner(cfg Config, jail *Jail, rec *Recorder, ap *Approver, client *Client, notes []string) {
	who := "?"
	if u, err := user.Current(); err == nil {
		who = fmt.Sprintf("%s (uid %s)", u.Username, u.Uid)
	}
	fmt.Println("\033[1mLatent Coding Agent\033[0m")
	fmt.Printf("  user:    %s\n", who)
	fmt.Printf("  jail:    %s\n", jail.Root)
	fmt.Printf("  model:   %s @ %s\n", client.Model(), cfg.BaseURL)
	for _, n := range notes {
		fmt.Printf("           %s\n", n)
	}
	fmt.Printf("  audit:   %s\n", cfg.Dir)
	fmt.Printf("  log:     %s\n", rec.SessionPath())
	fmt.Printf("  approve: %s\n", ap.Mode())
	fmt.Println("  commands: /model [name]  /approve [on|off|status]  /reset  /exit")
}
