package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
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
		ap.TrustAll()
	}

	var notes []string
	if cfg.Discover {
		notes = reconcileModel(client, rec)
	}
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
		runTurn(client, jail, ap, rec, &msgs, cfg.MaxSteps, cfg.CtxTokens, cfg.Raw)
		rec.Transcript(msgs)
		return
	}

	if os.Getenv("LCA_NO_CLEAR") == "" {
		clearScreen()
	}
	banner(cfg, jail, rec, ap, client, notes)

	ed := NewLineEditor(in)
	for {
		fmt.Print("\n")
		line, err := ed.ReadLine(" " + cFaint + "›" + cReset + " ")
		if err == errLineCancel {
			continue
		}
		if err != nil { // EOF (Ctrl-D / stream end)
			return
		}
		// Backslash line-continuation: a line ending in \ keeps reading, so a
		// long prompt can be typed across several lines.
		for strings.HasSuffix(line, "\\") {
			cont, err := ed.ReadLine("   " + cFaint + "…" + cReset + " ")
			if err != nil {
				break
			}
			line = line[:len(line)-1] + "\n" + cont
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
			fmt.Println(" " + faint("%s TRANSCRIPT CLEARED", gNone))
			continue
		}
		if handleApproveCmd(line, ap, rec) {
			continue
		}
		if handleModelCmd(line, client, rec, cfg.Discover) {
			continue
		}
		if handleEndpointCmd(line, client, rec, cfg.Discover) {
			continue
		}
		if handleDiscoverCmd(line, client, rec, cfg) {
			continue
		}

		msgs = append(msgs, Message{Role: "user", Content: line})
		rec.Event("user", map[string]any{"text": line})
		runTurn(client, jail, ap, rec, &msgs, cfg.MaxSteps, cfg.CtxTokens, cfg.Raw)
		rec.Transcript(msgs)
	}
}

// reconcileModel discovers what the endpoint actually serves and reconciles it
// with the configured model name. If the configured name isn't served but the
// endpoint offers exactly one model, we adopt it (the common vLLM/SGLang case:
// one model per endpoint, whose id rarely matches a hand-typed guess). Returns
// pre-colored display lines for the banner; never fatal.
func reconcileModel(client *Client, rec *Recorder) []string {
	models, err := client.ListModels()
	if err != nil {
		return []string{warn("model discovery unavailable (%v) — using %q as-is", err, client.Model())}
	}
	if len(models) == 0 {
		return []string{warn("endpoint advertises no models — using configured name as-is")}
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
			faint("adopted (configured %q not served)", prev),
		}
	}
	lines := []string{warn("configured %q not served; choose one with /model:", client.Model())}
	return append(lines, modelTable(models, client.Model())...)
}

// handleModelCmd processes the /model REPL command. "/model <name>" sets the
// model for subsequent turns (no validation — we trust the name). "/model" shows
// the current model; only when discovery is enabled does it probe /models and
// list what the router serves.
func handleModelCmd(line string, client *Client, rec *Recorder, discover bool) bool {
	arg, ok := commandArg(line, "/model")
	if !ok {
		return false
	}

	if arg == "" {
		eyebrow("models")
		kv("current", client.Model()+"  "+faint("@ %s", client.Endpoint()))
		if !discover {
			fmt.Println("  " + faint("set with /model <name>  ·  discovery off (LCA_DISCOVER=1 to probe)"))
			return true
		}
		models, err := client.ListModels()
		switch {
		case err != nil:
			fmt.Println("  " + warn("discovery unavailable: %v", err))
		case len(models) == 0:
			fmt.Println("  " + faint("endpoint advertises no models"))
		default:
			for _, l := range modelTable(models, client.Model()) {
				fmt.Println(l)
			}
		}
		return true
	}

	prev := client.Model()
	client.SetModel(arg)
	rec.Event("model_change", map[string]any{"from": prev, "to": arg})
	fmt.Printf("  %sMODEL%s %s → %s\n", cFaint, cReset, prev, arg)
	if discover {
		if models, err := client.ListModels(); err == nil && len(models) > 0 {
			if _, ok := findModel(models, arg); !ok {
				fmt.Println("  " + warn("warning: %q is not in the endpoint's served list", arg))
			}
		}
	}
	return true
}

// handleEndpointCmd processes the /endpoint (alias /ep) REPL command.
// "/endpoint" lists known endpoints with a reachability check (current marked);
// "/endpoint <n|url>" switches — by list index, or to any URL (handy when a
// SLURM allocation hands out a fresh host:port). Re-discovery on switch only
// happens when discovery is enabled.
func handleEndpointCmd(line string, client *Client, rec *Recorder, discover bool) bool {
	arg, ok := commandArg(line, "/endpoint", "/ep")
	if !ok {
		return false
	}
	eps := client.Endpoints()

	if arg == "" {
		eyebrow("endpoints")
		probes := probeEndpoints(client, eps)
		for i, e := range eps {
			mark := " "
			if e == client.Endpoint() {
				mark = cBold + "→" + cReset
			}
			fmt.Printf("  %s %d  %s %s  %s\n",
				mark, i+1, probes[i].glyph(), e, probes[i].detail(e == client.Endpoint(), client.Model()))
		}
		fmt.Println("  " + faint("switch: /endpoint <n|url>"))
		return true
	}

	target := arg
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(eps) {
			fmt.Println("  " + warn("no endpoint #%d (have %d)", n, len(eps)))
			return true
		}
		target = eps[n-1]
	}

	prev := client.Endpoint()
	client.SetEndpoint(target)
	rec.Event("endpoint_change", map[string]any{"from": prev, "to": client.Endpoint()})
	fmt.Printf("  %sENDPOINT%s %s → %s\n", cFaint, cReset, prev, client.Endpoint())

	// If discovery learned which model this endpoint serves, select it too.
	if m := client.EndpointModel(client.Endpoint()); m != "" && m != client.Model() {
		client.SetModel(m)
		rec.Event("model_change", map[string]any{"from": prev, "to": m, "via": "endpoint"})
		fmt.Printf("  %sMODEL%s → %s\n", cFaint, cReset, m)
	}

	if discover {
		for _, n := range reconcileModel(client, rec) {
			fmt.Println("  " + n)
		}
	}
	return true
}

// handleDiscoverCmd runs native Slurm discovery (squeue → scontrol → job log /
// startup script → HTTP probe), refreshes the endpoint list from the result, and
// prints a picker. Addresses go stale on requeue/preemption, so it is re-run on
// demand and never cached.
func handleDiscoverCmd(line string, client *Client, rec *Recorder, cfg Config) bool {
	if _, ok := commandArg(line, "/discover", "/disc"); !ok {
		return false
	}

	fmt.Println(" " + faint("%s DISCOVER  squeue → scontrol → logs → probe…", gNone))
	res, err := discoverSlurm(cfg.Reservation, cfg.DiscoverUser, cfg.Scheme)
	if err != nil {
		fmt.Println("  " + warn("%v", err))
		rec.Event("discover", map[string]any{"error": err.Error()})
		return true
	}

	// Refresh endpoints (only ones with a resolved port) and remember the model
	// each serves. Kept in discovery order so the picker index matches /endpoint.
	var urls []string
	for _, m := range res.Models {
		if m.Endpoint() == "" {
			continue
		}
		u := m.baseURL(cfg.Scheme)
		urls = append(urls, u)
		client.SetEndpointModel(u, m.modelName())
	}
	client.SetEndpoints(urls)
	rec.Event("discover", map[string]any{"count": len(res.Models), "usable": len(urls), "reservation": cfg.Reservation})

	eyebrow("discovered")
	for _, w := range res.Warnings {
		fmt.Println("  " + faint("! %s", w))
	}
	idx := 0
	for _, m := range res.Models {
		if m.Port == 0 {
			fmt.Printf("  %s      %s  %s\n", healthGlyph(m.Health), m.Node, faint("%s", m.Err))
			continue
		}
		idx++
		detail := m.display()
		if m.Engine != "" {
			detail += "  " + faint("%s", m.Engine)
		}
		if m.MaxModelLen > 0 {
			detail += "  " + faint("ctx %d", m.MaxModelLen)
		}
		if m.GpuCount > 0 {
			detail += "  " + faint("%d gpu", m.GpuCount)
		}
		fmt.Printf("  %s %2d  %s:%d  %s\n", healthGlyph(m.Health), idx, m.Node, m.Port, detail)
	}
	switch {
	case len(res.Models) == 0:
		fmt.Println("  " + faint("no running jobs found"))
	case idx > 0:
		fmt.Println("  " + faint("switch: /endpoint <n>  (endpoint + model applied together)"))
	}
	return true
}

// endpointProbe is the reachability result for one endpoint.
type endpointProbe struct {
	up     bool
	models []ModelInfo // present only if /models answered 200
}

func (p endpointProbe) glyph() string {
	if !p.up {
		return cRed + gDown + cReset // ✕ down
	}
	return cGreen + gUp + cReset // ● up
}

// detail renders the right-hand description. For the current endpoint it names
// the selected model; otherwise it shows any models the probe happened to see.
func (p endpointProbe) detail(isCurrent bool, currentModel string) string {
	if !p.up {
		return faint("down")
	}
	switch {
	case isCurrent:
		return faint("%s", currentModel)
	case len(p.models) == 1:
		d := p.models[0].ID
		if extra := describeModel(p.models[0]); extra != "" {
			d += "  " + faint("%s", extra)
		}
		return d
	case len(p.models) > 1:
		return faint("%d models", len(p.models))
	default:
		return faint("up")
	}
}

// probeEndpoints reachability-checks every endpoint concurrently (bounded by the
// per-request timeout in ProbeHealth), preserving input order.
func probeEndpoints(client *Client, eps []string) []endpointProbe {
	out := make([]endpointProbe, len(eps))
	var wg sync.WaitGroup
	for i, e := range eps {
		wg.Add(1)
		go func(i int, e string) {
			defer wg.Done()
			up, m := client.ProbeHealth(e)
			out[i] = endpointProbe{up: up, models: m}
		}(i, e)
	}
	wg.Wait()
	return out
}

// commandArg matches a slash command (or its aliases) and returns its trimmed
// argument. It requires either an exact match or a space-separated argument, so
// "/endpoints" does not match "/endpoint".
func commandArg(line string, names ...string) (string, bool) {
	for _, name := range names {
		if line == name {
			return "", true
		}
		if strings.HasPrefix(line, name+" ") {
			return strings.TrimSpace(line[len(name):]), true
		}
	}
	return "", false
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
// ready, a bold → marks the current selection, followed by its detail.
func modelTable(ms []ModelInfo, current string) []string {
	lines := make([]string, 0, len(ms))
	for _, m := range ms {
		mark := " "
		if m.ID == current {
			mark = cBold + "→" + cReset
		}
		line := fmt.Sprintf("  %s%s%s %s %s", cGreen, gUp, cReset, mark, m.ID)
		if d := describeModel(m); d != "" {
			line += "  " + faint("%s", d)
		}
		lines = append(lines, line)
	}
	return lines
}

// readyLine is the banner status for the active model.
func readyLine(m ModelInfo) string {
	s := statusText(cGreen, gUp, "ready")
	if d := describeModel(m); d != "" {
		s += " " + faint("— %s", d)
	}
	return s
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
		ap.TrustAll()
	case "off":
		ap.Clear()
	case "run":
		ap.Trust("run")
	case "edit", "write":
		ap.Trust("edit")
	case "", "status":
		kv("approve", strings.ToUpper(ap.Mode()))
		return true
	default:
		fmt.Println("  " + faint("usage: /approve [on|off|run|edit|status]"))
		return true
	}
	rec.Event("approve_mode", map[string]any{"trusted": ap.TrustedClasses()})
	kv("approve", strings.ToUpper(ap.Mode()))
	return true
}

// runTurn drives the agentic loop for one user message: stream the model, execute
// any tool blocks, feed results back, repeat until the model stops emitting
// tools (a final answer) or we hit the step cap.
func runTurn(client *Client, jail *Jail, ap *Approver, rec *Recorder, msgs *[]Message, maxSteps, ctxTokens int, raw bool) {
	for step := 0; step < maxSteps; step++ {
		pw := newProseWriter(raw)

		send, trimmed := trimForContext(*msgs, ctxTokens)
		if trimmed > 0 {
			fmt.Println(" " + faint("%s CONTEXT  trimmed %d old tool outputs (~%dk budget)", gNone, trimmed, ctxTokens/1000))
			rec.Event("context_trim", map[string]any{"collapsed": trimmed, "budget_tokens": ctxTokens})
		}
		reply, err := client.CompleteStream(send, pw.feed)
		pw.end()
		if err != nil {
			fmt.Println(" " + cRed + gDown + " ENDPOINT ERROR" + cReset + " " + err.Error())
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
	fmt.Println(" " + warn("%s STOPPED — hit %d-step cap", gPartial, maxSteps))
	rec.Event("step_cap", map[string]any{"steps": maxSteps})
}

func executeBlocks(jail *Jail, ap *Approver, rec *Recorder, blocks []Block) string {
	var out strings.Builder
	for _, b := range blocks {
		var res string
		switch b.Name {
		case "read_file":
			toolLine("read_file", b.Attr["path"])
			res = readFile(jail, b.Attr["path"], b.Attr["lines"])
			rec.Event("read_file", map[string]any{"path": b.Attr["path"], "result": summarize(res)})
		case "grep":
			toolLine("grep", fmt.Sprintf("%q %s", b.Attr["pattern"], b.Attr["path"]))
			res = grepTree(jail, b.Attr["pattern"], b.Attr["path"])
			rec.Event("grep", map[string]any{"pattern": b.Attr["pattern"], "path": b.Attr["path"], "result": summarize(res)})
		case "list_dir":
			toolLine("list_dir", b.Attr["path"])
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
		printOutcome(b.Name, res)
		fmt.Fprintf(&out, "<tool_result name=\"%s\" path=\"%s\">\n%s\n</tool_result>\n",
			b.Name, b.Attr["path"], res)
	}
	return out.String()
}

// printOutcome shows a one-line result under a tool's marker: counts for the
// read-only tools, a status glyph for the side-effecting ones.
func printOutcome(name, res string) {
	if strings.HasPrefix(res, "error:") {
		toolErr(strings.TrimSpace(strings.TrimPrefix(res, "error:")))
		return
	}
	switch name {
	case "read_file":
		n := lineCount(res) - 1 // minus the "path:" header line
		if n < 0 {
			n = 0
		}
		toolInfo(plural(n, "line", "lines"))
	case "grep":
		if res == "no matches" {
			toolInfo("no matches")
		} else {
			toolInfo(plural(lineCount(res), "match", "matches"))
		}
	case "list_dir":
		n := lineCount(res) - 1 // minus the header line
		if n < 0 {
			n = 0
		}
		toolInfo(plural(n, "entry", "entries"))
	default: // edit / write / run_command
		if strings.HasPrefix(res, "user denied") {
			toolInfo("denied")
		} else {
			toolOK(summarize(res))
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// lineCount counts non-empty lines in a tool result (an at-a-glance hint).
func lineCount(s string) int {
	n := 0
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

func gatedEdit(jail *Jail, ap *Approver, rec *Recorder, b Block) string {
	abs, err := jail.Resolve(b.Attr["path"])
	if err != nil {
		rec.Event("edit", map[string]any{"path": b.Attr["path"], "error": err.Error()})
		return "error: " + err.Error()
	}
	approved, auto := ap.Confirm("edit", "EDIT "+b.Attr["path"], unifiedPreview(b.Search, b.Replace))
	if !approved {
		rec.Event("edit", map[string]any{"path": b.Attr["path"], "approved": false})
		return "user denied this edit"
	}
	res, err := applyEdit(abs, b.Attr["path"], b.Search, b.Replace)
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
	approved, auto := ap.Confirm("write", strings.ToUpper(action)+" "+b.Attr["path"], preview)
	if !approved {
		rec.Event("write", map[string]any{"path": b.Attr["path"], "approved": false})
		return "user denied this write"
	}
	res, err := writeWholeFile(abs, b.Attr["path"], b.Body)
	if err != nil {
		rec.Event("write", map[string]any{"path": b.Attr["path"], "approved": true, "auto": auto, "error": err.Error()})
		return "error: " + err.Error()
	}
	rec.Event("write", map[string]any{"path": b.Attr["path"], "approved": true, "auto": auto, "bytes": len(b.Body)})
	return res
}

func gatedRun(jail *Jail, ap *Approver, rec *Recorder, b Block) string {
	cmd := strings.TrimSpace(b.Body)
	approved, auto := ap.Confirm("run_command", "RUN", "  $ "+cmd)
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
		who = fmt.Sprintf("%s · uid %s", u.Username, u.Uid)
	}

	hr()
	ticket(os.Getenv("LCA_ORG"), gUp+" LIVE ▲")
	fmt.Println()
	eyebrow("session")
	title("Latent Coding Agent")
	hr()

	kv("user", who)
	kv("jail", jail.Root)
	endpointNote := ""
	if len(client.Endpoints()) > 1 {
		endpointNote = faint("  (+%d more — /endpoint)", len(client.Endpoints())-1)
	}
	kv("model", client.Model()+"  "+faint("@ %s", client.Endpoint())+endpointNote)
	for _, n := range notes {
		contValue(n)
	}
	kv("audit", cfg.Dir)
	kv("log", rec.SessionPath())
	kv("approve", strings.ToUpper(ap.Mode()))
	hr()
	fmt.Println(" " + faint("/discover   /endpoint [n|url]   /model [name]   /approve [on|off|run|edit|status]   /reset   /exit"))
}
