package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Formatting and small helpers shared by the engine and the views.

// reconcileModel discovers what the endpoint actually serves and reconciles it
// with the configured model name. If the configured name isn't served but the
// endpoint offers exactly one model, we adopt it (the common vLLM/SGLang case:
// one model per endpoint, whose id rarely matches a hand-typed guess). Returns
// pre-colored display lines for the banner; never fatal.
func reconcileModel(ps *Providers, client *Client, rec *Recorder) []string {
	// Deferred, and registered before the first return: this is the -discover
	// startup path, which never calls Providers.Learn, so without it the engine
	// stays UNKNOWN exactly where the window is learned. After the defer, because
	// the adopt branch below calls SetModel, which forgets both facts.
	defer ps.applyEngine(client)
	models, err := client.ListModels()
	if err != nil {
		ps.LearnWindows(client.Endpoint(), nil)
		return []string{warn("model discovery unavailable (%v) — using %q as-is", err, client.Model())}
	}
	// The answer is the whole endpoint's, not just this model's: a role chain
	// switching models later must not have to ask again.
	ps.LearnWindows(client.Endpoint(), models)
	if len(models) == 0 {
		return []string{warn("endpoint advertises no models — using configured name as-is")}
	}
	if info, ok := findModel(models, client.Model()); ok {
		client.SetCtxLen(info.MaxLen)
		return []string{readyLine(info)}
	}
	if len(models) == 1 {
		prev := client.Model()
		client.SetModel(models[0].ID)
		client.SetCtxLen(models[0].MaxLen)
		rec.Event("model_adopt", map[string]any{"from": prev, "to": models[0].ID})
		return []string{
			readyLine(models[0]),
			faint("adopted (configured %q not served)", prev),
		}
	}
	lines := []string{warn("configured %q not served; choose one with /model:", client.Model())}
	return append(lines, modelTable(models, client.Model())...)
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
			mark = cBold + gFlow + cReset
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

// looksLikeStrayEdit reports whether a reply describes a file change in a format
// that does NOT apply — a diff fence, unified diff, or SEARCH/REPLACE markers —
// rather than an <edit>/<write> tool call. Kept to strong signals to avoid
// nudging a legitimate "show me a diff" answer.
func looksLikeStrayEdit(s string) bool {
	switch {
	case strings.Contains(s, "```diff"):
		return true
	case strings.Contains(s, "<<<<<<< SEARCH"), strings.Contains(s, ">>>>>>> REPLACE"):
		return true
	case strings.Contains(s, "\n@@ ") && (strings.Contains(s, "\n--- ") || strings.Contains(s, "\n+++ ")):
		return true
	}
	return false
}

// looksStalled reports whether a no-tool reply seems to have announced a next
// step without doing it — its last non-empty line trails off on a colon or an
// ellipsis. Used only to auto-continue a stalled turn (bounded).
func looksStalled(s string) bool {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return strings.HasSuffix(t, ":") || strings.HasSuffix(t, "…") || strings.HasSuffix(t, "...")
		}
	}
	return false
}

// lastUserTurn is the index of the most recent real user message (not a synthetic
// tool_result), or -1 if there is none — the anchor for /retry and /edit.
func lastUserTurn(msgs []Message) int {
	for i := len(msgs) - 1; i >= 1; i-- {
		if msgs[i].Role == "user" && !strings.HasPrefix(msgs[i].Content, "<tool_result") {
			return i
		}
	}
	return -1
}

// ctxBudget resolves the trim budget in tokens. An explicit LCA_CTX_TOKENS wins;
// otherwise we derive it from the model's real context window (reserving ~25%
// for the reply), so we only compress near the true limit and keep the KV cache
// warm as long as possible. Falls back to 24k when the window is unknown.
func ctxBudget(explicit, modelCtxLen int) int {
	if explicit > 0 {
		return explicit
	}
	if modelCtxLen > 0 {
		return modelCtxLen * 3 / 4
	}
	return ctxBudgetFallback
}

// ctxBudgetFallback is what the budget becomes when neither the deployment nor
// the table knows the window. It is a placeholder, not a window, so doctor names
// the number out loud — otherwise it passes for a real one.
const ctxBudgetFallback = 24000

// printPerf prints a dim one-line performance summary after a streamed step:
// prompt tokens and how many hit the server's KV prefix cache (the payoff of
// keeping the prefix byte-stable), completion tokens, decode throughput, and
// time-to-first-token. Silent when the server reports no usage.
func printPerf(u Usage) {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return
	}
	var parts []string
	if u.PromptTokens > 0 {
		s := kfmt(u.PromptTokens) + " in"
		if u.CachedTokens > 0 {
			s += fmt.Sprintf(" (%d%% cached)", u.CacheHitPct())
			// The cache gauge has a real denominator — the prompt this turn actually
			// sent — so it is allowed to be a picture, and its number is already the
			// parenthesis in front of it.
			//
			// It is INLINE and not a second row, which is where the mockup drew it,
			// because printPerf runs once per model CALL and not once per turn: a
			// five-call turn was spending ten rows on accounting around four rows of
			// work. And it is drawn only where there is a screen — a redirected
			// one-shot's payload gained twelve block runes per call, which is new
			// decoration in the one path the brief says must stay clean.
			if g := gaugePct(u.CachedTokens, u.PromptTokens, 12, cGreen); g != "" {
				s += " " + g
			}
		}
		parts = append(parts, s)
	}
	if u.CompletionTokens > 0 {
		parts = append(parts, kfmt(u.CompletionTokens)+" out")
	}
	// Throughput only means something over a real generation window.
	if tps := u.TokPerSec(); tps > 0 && u.GenDur >= 300*time.Millisecond {
		parts = append(parts, fmt.Sprintf("%d tok/s", tps))
	}
	if u.TTFT >= 50*time.Millisecond {
		parts = append(parts, "first token "+fmtDurShort(u.TTFT))
	}
	// The cost line opens with the map's cost glyph, so a turn's price is findable
	// by scanning one column instead of reading every line.
	fmt.Println(" " + cDim + gCost + cReset + " " + faint("%s", strings.Join(parts, gSep)))
}

// kfmt formats a token count compactly: 873, 12.3k, 128k.
func kfmt(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return strconv.Itoa(n/1000) + "k"
	}
}

func fmtDurShort(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
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
		switch {
		case strings.HasPrefix(res, "user denied"):
			toolInfo("denied")
		case name == "run_command":
			// output + exit status were already streamed live by runCommand
		default:
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

// ctxfmt is kfmt with a megatoken step. The served windows are now large enough
// that "1048k" reads as noise next to "262k", and the difference between a 1M
// and a 262k window is the difference between compacting once and four times.
func ctxfmt(n int) string {
	if n >= 1_000_000 {
		return strconv.FormatFloat(float64(n)/1e6, 'f', 2, 64) + "M"
	}
	return kfmt(n)
}
