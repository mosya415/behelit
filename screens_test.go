package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The look, asserted rather than described.
//
// A palette is reviewed by eye, but the three things that make this one usable
// are promises and so need tests: that nothing is drawn wider than the terminal,
// that the class mark of every transcript line sits in the same column, and that
// the whole look survives being stripped of colour and of every rune above 0x7f.
// The exact lines below are here so that a change to them shows up in review as a
// diff instead of as a claim.

// screenTheme installs a theme for one test and gives the inherited one back. The
// suite otherwise runs under the theme the process started with, which is
// deliberately not the environment's — so a screen test has to say which theme it
// is about.
func screenTheme(t *testing.T, name string, unicode bool) {
	t.Helper()
	t.Setenv("COLUMNS", "80")
	saved := activeTheme
	t.Cleanup(func() { setTheme(saved) })
	caps := termCaps{tty: true, colour: name == themeDungeon, depth: depth256, unicode: unicode}
	if name == themePlain {
		setTheme(plainTheme(caps, "test"))
		return
	}
	setTheme(dungeonTheme(caps, "test"))
}

// drawTurn is a turn in flight, drawn through the real primitives: the map
// gutter, a read, a carve with its diff, a command with its own bytes behind the
// │ prefix, the model's prose, and what it cost.
func drawTurn() {
	fmt.Println(" " + cDim + gMe + cReset + " " + cFaint + gPrompt + cReset + " fix Sum in sum.go")
	toolLine("read_file", "sum.go")
	toolInfo("48 lines")
	toolLine("edit", "sum.go")
	_, _, d := lineDiff("a\nif n < 0 { continue }\nb\n", "a\n// negatives count too\nb\n")
	fmt.Println(d)
	toolOK("+1 -1")
	fmt.Println(" " + cYellow + gCmd + cReset + " " + cDim + padTo("run", 8, 0) + cReset +
		" $ go test ./...   " + faint("(Ctrl-C to interrupt)"))
	fmt.Println("   " + cFaint + gVBar + " " + cReset + "ok  lca  0.412s")
	fmt.Println("     " + cGreen + gUp + cReset + faint(" exit 0"))
	pw := newProseWriter(false, false)
	// a screen, because the prose gutter and the wrap are two of the things this
	// golden is about. captureStdout leaves stdout a pipe, where the answer is
	// somebody's payload and gets neither — TestPipedTurnEmitsNoCursorMotion is
	// where that case is asserted.
	pw.anim = true
	pw.printLine(renderMarkdownLine("Sum skipped negatives; the guard is gone."))
	fmt.Println()
	printPerf(Usage{PromptTokens: 4200, CachedTokens: 3822, CompletionTokens: 512,
		GenDur: 7 * time.Second, TTFT: 310 * time.Millisecond})
}

// drawPanels is every framed surface plus the failure lines and the verifier.
func drawPanels() {
	hold := newPanel("the hold", "where you stand")
	hold.Row("project", cBold+"cli-agents"+cReset+"  "+faint("~/work/")+faint("%sgit", gSep))
	hold.Row("gateway", "gw.lan:8080  "+statusText(cGreen, gUp, "up")+faint("%s6 models", gSep))
	hold.Print()

	door := newPanel("a door", "run").door().tag("kimi-k3")
	for _, l := range previewLines("  $ go build -o lca .") {
		door.Line("%s", l)
	}
	door.Print()

	sheet := newPanel("the sheet", "this session")
	sheet.Row("cache", gaugePct(15000, 20000, gaugeCells, cGreen)+"  75%"+faint(" of the prompt served from the KV cache"))
	sheet.Row("tools", gaugeSplit(9, 1, gaugeCells, cGreen, cRed)+"  9 calls"+gSep+"1 didn't parse (10.0%)")
	sheet.Print()

	section("gateway", "gw.lan:8080")
	okLine("6 models")
	warnLine("glm-5.3 is not up — falling back to qwen3.6")
	errLine("gateway returned 503 after 3 attempts")
	hint("lca doctor checks the gateway, the roles and the tool transport")
	fmt.Println(" " + cDim + gGate + cReset + " " + cDim + padTo("verify", 8, 0) + cReset + " " +
		checkText("go test ./...", 0, 1200*time.Millisecond, 1, 3))
	fmt.Println(" " + cDim + gGate + cReset + " " + cDim + padTo("verify", 8, 0) + cReset + " " +
		checkText("go test ./...", 1, 900*time.Millisecond, 3, 3))
	printTodos([]Todo{{Content: "read sum.go", Status: "completed"}, {Content: "fix it", Status: "in_progress"}})
}

func drawPicker() {
	served := []ModelInfo{{ID: "kimi-k3", MaxLen: 1048576}, {ID: "qwen3.6", MaxLen: 131072}, {ID: "hy3-unknown-9000"}}
	scale := windowScale(served)
	var cs []choice
	for i, m := range served {
		cs = append(cs, modelChoice(m, i == 0, scale))
	}
	p := newPickState(cs, pickOpts{multi: true, title: "setup", detail: "3/6" + gSep + "3 served", height: 6})
	for _, l := range p.lines() {
		fmt.Println(l)
	}
}

// ── the exact lines ─────────────────────────────────────────────────────────

func TestTurnGoldenDungeon(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	got := stripANSI(captureStdout(t, drawTurn))
	want := []string{
		" @ › fix Sum in sum.go",
		" . read     sum.go",
		"     → 48 lines",
		" / edit     sum.go",
		"     a",
		"   - if n < 0 { continue }",
		"   + // negatives count too",
		"     b",
		"     ● +1 -1",
		" ^ run      $ go test ./...   (Ctrl-C to interrupt)",
		"   │ ok  lca  0.412s",
		"     ● exit 0",
		"",
		" ▌ Sum skipped negatives; the guard is gone.",
		// one row, with the cache gauge inline: printPerf runs once per model CALL,
		// so a second row is a second row per call (theme.go records the departure)
		" $ 4.2k in (91% cached) ███████████░ · 512 out · 73 tok/s · first token 310ms",
	}
	assertLines(t, got, want)
}

func TestTurnGoldenASCII(t *testing.T) {
	screenTheme(t, themeDungeon, false)
	got := stripANSI(captureStdout(t, drawTurn))
	// the map gutter is unchanged: it was 7-bit to begin with, which is the whole
	// reason this tier loses only the frames, the gauges and the flame
	for _, want := range []string{
		" @ > fix Sum in sum.go",
		" . read     sum.go",
		"     -> 48 lines",
		" / edit     sum.go",
		"     * +1 -1",
		" ^ run      $ go test ./...   (Ctrl-C to interrupt)",
		"   | ok  lca  0.412s",
		" | Sum skipped negatives; the guard is gone.",
		" $ 4.2k in (91% cached) [###########-] . 512 out . 73 tok/s . first token 310ms",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ASCII tier is missing\n%q\nin:\n%s", want, got)
		}
	}
}

func TestDoorGoldenDungeon(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	got := stripANSI(captureStdout(t, func() {
		door := newPanel("a door", "edit sum.go").door().tag("[coder t1]")
		for _, l := range previewLines("  - if n < 0 { continue }\n  + // negatives count too") {
			door.Line("%s", l)
		}
		door.Print()
	}))
	assertLines(t, got, []string{
		" ╔═ A DOOR ═ edit sum.go ══════════════════════════════════════ [coder t1] ═╗",
		" ║   - if n < 0 { continue }                                                ║",
		" ║   + // negatives count too                                               ║",
		" ╚══════════════════════════════════════════════════════════════════════════╝",
	})
}

func TestPickerGoldenDungeon(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	got := stripANSI(captureStdout(t, drawPicker))
	for _, want := range []string{
		" ╔═ SETUP ═ 3/6 · 3 served ═",
		"[●] ▸ kimi-k3",
		"[ ]   qwen3.6",
		// an unknown window gets an empty track and says so in words: "we do not
		// know" must look nothing like "small"
		"░░░░░░░░░░░░░",
		"window ?",
		"arrow keys move",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the picker is missing %q:\n%s", want, got)
		}
	}
}

// ── the invariants ──────────────────────────────────────────────────────────

// Nothing may be drawn wider than the terminal. A row that wraps inside a
// repainted region desynchronises the "\033[<n>A" walk-back, and the next
// repaint's "\r\033[J" then erases whatever was above the menu.
func TestEveryScreenFitsEightyColumns(t *testing.T) {
	for _, tier := range []struct {
		name    string
		theme   string
		unicode bool
	}{
		{"dungeon", themeDungeon, true},
		{"plain", themePlain, true},
		{"ascii", themeDungeon, false},
	} {
		t.Run(tier.name, func(t *testing.T) {
			screenTheme(t, tier.theme, tier.unicode)
			out := captureStdout(t, func() { drawTurn(); drawPanels(); drawPicker() })
			for _, l := range strings.Split(out, "\n") {
				if w := visibleWidth(l); w > 80 {
					t.Errorf("%d columns wide at a terminal of 80:\n%q", w, stripANSI(l))
				}
			}
		})
	}
}

// A panel's top and bottom courses must be the same width, or the frame reads as
// broken — and the top one is assembled from styled parts while the bottom is a
// plain repeat, so the two are computed twice and can drift.
func TestPanelFrameIsSquare(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	for _, c := range []struct{ title, detail, right string }{
		{"the hold", "where you stand", ""},
		{"a door", "edit some/very/long/path/that/goes/on.go", "[coder t1]"},
		{"steps", "", "r-20260930-1"},
		{"x", "", ""},
	} {
		p := newPanel(c.title, c.detail).tag(c.right)
		p.Row("label", "value")
		ls := p.Lines()
		top, bot := visibleWidth(ls[0]), visibleWidth(ls[len(ls)-1])
		if top != bot {
			t.Errorf("panel %q: top is %d columns, bottom is %d\n%s", c.title, top, bot, strings.Join(stripEach(ls), "\n"))
		}
		for _, l := range ls {
			if visibleWidth(l) != top {
				t.Errorf("panel %q is ragged:\n%s", c.title, strings.Join(stripEach(ls), "\n"))
				break
			}
		}
	}
}

// The class mark is in column 1 and the verb starts in column 3. Nine of the
// eleven marks also occur inside data — $ in a shell command, / in a path, . in a
// filename — so they are marks only BECAUSE of the column, which makes the column
// load-bearing rather than tidy.
func TestGutterColumnIsFixed(t *testing.T) {
	screenTheme(t, themePlain, true)
	marks := map[string]bool{gMe: true, gTorch: true, gProbe: true, gCarve: true,
		gCmd: true, gCost: true, gProse: true, gDeeper: true, gOut: true,
		gLoot: true, gGate: true, gNone: true, gUp: true, gPartial: true, gDown: true}
	out := captureStdout(t, drawTurn)
	seen := 0
	for _, l := range strings.Split(out, "\n") {
		if l == "" || strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "\t") {
			continue // an outcome or a continuation line, indented out of the gutter
		}
		if !strings.HasPrefix(l, " ") {
			t.Errorf("a transcript line starts in column 0: %q", l)
			continue
		}
		r := strings.SplitN(l[1:], " ", 2)
		if !marks[r[0]] {
			t.Errorf("column 1 of %q is %q, which is not a class mark", l, r[0])
			continue
		}
		seen++
	}
	if seen < 5 {
		t.Errorf("only %d gutter lines were checked — the turn stopped drawing them", seen)
	}
}

// The plain theme is the one every piped run gets, and an escape in a piped run
// is a byte in somebody's input file that surfaces a week later.
func TestPlainScreensCarryNoEscape(t *testing.T) {
	screenTheme(t, themePlain, true)
	out := captureStdout(t, func() { drawTurn(); drawPanels(); drawPicker() })
	if i := strings.IndexByte(out, 0x1b); i >= 0 {
		t.Errorf("an escape at byte %d of the plain screens: %q", i, out[max(0, i-40):min(len(out), i+40)])
	}
}

// LC_ALL=C gets ASCII for every glyph the look DRAWS, and the allowance is down to
// one rune.
//
// "·" used to be on this list on the grounds that substituting it is a rewording:
// it is not. It is the field separator between a model id and a token count on
// every status line, perf line, legend and panel row — chrome by any reading, and
// more of it than all the map glyphs together — so it is a theme glyph now (Sep),
// and the design's own ASCII mockup prints "." for it.
//
// The em dash inside a message SENTENCE stays. Those sentences are the messages
// themselves, the same bytes the trace, the audit log and run.log carry, so
// rewriting them would change data rather than chrome; there are 316 of them and
// theme.go's departures block records the decision. The chrome dash between a
// status word and its gloss is Dash and does degrade.
func TestASCIIScreensAreSevenBitChrome(t *testing.T) {
	screenTheme(t, themeDungeon, false)
	out := stripANSI(captureStdout(t, func() { drawTurn(); drawPanels(); drawPicker() }))
	const allowed = "—" // an em dash inside a message: see above, and theme.go
	for _, r := range out {
		if r > 0x7f && !strings.ContainsRune(allowed, r) {
			t.Errorf("the ASCII tier drew %q (U+%04X)", r, r)
		}
	}
	// and the two lines the design's ASCII mockup draws literally
	for _, want := range []string{"(91% cached)", " . 512 out"} {
		if !strings.Contains(out, want) {
			t.Errorf("the ASCII cost line lost %q:\n%s", want, out)
		}
	}
}

// The same promise over the REAL commands, because the separators that broke it
// are in the banner's rows and the status line, not in the primitives.
func TestASCIIRealScreensAreSevenBitChrome(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	h.sess.stats = SessionStats{Turns: 4, ToolCalls: 9, InvalidCalls: 1, ToolErrors: 2,
		PromptTokens: 20000, CachedTokens: 15000, OutputTokens: 800, Fallbacks: 1, VerifyRuns: 3}
	screenTheme(t, themeDungeon, false)
	out := stripANSI(captureStdout(t, func() {
		r.Banner()
		fmt.Println("   " + r.statusLine())
		r.cmdStats("")
		r.cmdContext("")
		r.cmdTheme("")
		testRunner(5, []string{stepOK, stepOK, stepOK, stepFailed, stepSkipped}).summary()
	}))
	const allowed = "—" // see TestASCIIScreensAreSevenBitChrome
	bad := map[rune]int{}
	for _, c := range out {
		if c > 0x7f && !strings.ContainsRune(allowed, c) {
			bad[c]++
		}
	}
	for c, n := range bad {
		t.Errorf("the ASCII tier drew %q (U+%04X) %d times:\n%s", c, c, n, out)
	}
}

// A gauge whose denominator is a guess is not drawn. ctxBudget() falls back to a
// placeholder when the window is unknown, and a confident percentage against an
// invented budget is a lie in picture form.
func TestGaugeRefusesAnInventedDenominator(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	if g := gauge(1000, 0, 10, cYellow); g != "" {
		t.Errorf("a gauge was drawn with no denominator: %q", g)
	}
	if g := gaugeSplit(0, 0, 10, cGreen, cRed); g != "" {
		t.Errorf("a split gauge was drawn with nothing in it: %q", g)
	}
	if g := windowBar(ModelInfo{ID: "nothing-known-9000"}, 0); g != "" {
		t.Errorf("a window bar was drawn against no scale: %q", g)
	}
	// present but tiny must not round away to absent
	if g := stripANSI(gaugeFrac(0.001, 10, cYellow)); !strings.HasPrefix(g, gaHalf) {
		t.Errorf("a tiny fraction rounded to nothing: %q", g)
	}
	// and full must not overflow its cells
	if g := stripANSI(gaugeFrac(1, 10, cYellow)); visibleWidth(g) != gaugeWidth(10) {
		t.Errorf("a full gauge is %d columns, want %d: %q", visibleWidth(g), gaugeWidth(10), g)
	}
}

func assertLines(t *testing.T, got string, want []string) {
	t.Helper()
	have := strings.Split(strings.Trim(got, "\n"), "\n")
	for i, w := range want {
		if i >= len(have) {
			t.Fatalf("line %d is missing; wanted %q\ngot:\n%s", i, w, got)
		}
		if have[i] != w {
			t.Errorf("line %d\n want %q\n  got %q", i, w, have[i])
		}
	}
	if len(have) != len(want) {
		t.Errorf("%d lines, want %d:\n%s", len(have), len(want), got)
	}
}

func stripEach(ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = stripANSI(l)
	}
	return out
}

// ── the real screens, and the machine-readable output beside them ───────────

// The screens above are drawn from the primitives; these are the actual commands,
// with a fake gateway behind them. Nothing here asserts a look — it asserts that
// the look cannot push a real screen past the terminal's edge, which is the one
// failure that also breaks the repainted regions.
func TestRealScreensFitEightyColumns(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	h.sess.stats = SessionStats{Turns: 4, ToolCalls: 9, InvalidCalls: 1, ToolErrors: 2,
		PromptTokens: 20000, CachedTokens: 15000, OutputTokens: 800, Fallbacks: 1, VerifyRuns: 3}
	screenTheme(t, themeDungeon, true)
	out := captureStdout(t, func() {
		r.Banner()
		fmt.Println("   " + cFaint + r.statusLine() + cReset)
		r.cmdStats("")
		r.cmdContext("")
		r.cmdTheme("")
	})
	for _, l := range strings.Split(out, "\n") {
		if w := visibleWidth(l); w > 80 {
			t.Errorf("%d columns wide at a terminal of 80:\n%q", w, stripANSI(l))
		}
	}
	// and the screens still say the things they are for
	for _, want := range []string{"project", "gateway", "mode", "9 calls", "didn't parse", "used", "cache", "theme"} {
		if !strings.Contains(stripANSI(out), want) {
			t.Errorf("the dungeon lost the word %q:\n%s", want, stripANSI(out))
		}
	}
}

// A skin that leaks a gutter glyph or an escape into the trace is a blocker: the
// trace and the transcript are what a failure is reconstructed from a week later,
// and an escape in them is a byte nobody can grep past.
func TestThemedSessionWritesCleanTrace(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "the answer"} })
	fs.models = allModels()
	h := newRoleHarness(t, fs, testRoles, true)
	screenTheme(t, themeDungeon, true)
	captureStdout(t, func() {
		h.sess.Msgs = append(h.sess.Msgs, Message{Role: "user", Content: "say something"})
		if err := h.sess.Run(context.Background()); err != nil {
			t.Fatalf("the turn failed: %v", err)
		}
		h.sess.saveTranscript()
	})
	h.orch.tracer.Close()
	h.orch.rec.Close()
	for _, p := range []string{h.orch.tracer.Path, h.orch.rec.dir} {
		if p == "" {
			continue
		}
		filepath.Walk(p, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			if i := bytes.IndexByte(b, 0x1b); i >= 0 {
				t.Errorf("%s carries an escape at byte %d: %q", path, i, b[max(0, i-60):min(len(b), i+60)])
			}
			return nil
		})
	}
}

// Cursor motion is behaviour and stays unconditional — except where there is no
// cursor. "\r\033[K" erases the row the waiting marker is on, and a redirected
// one-shot has no row: the escape used to land in the payload, which is somebody's
// input file a week later.
func TestPipedTurnEmitsNoCursorMotion(t *testing.T) {
	screenTheme(t, themePlain, true)
	out := captureStdout(t, func() {
		pw := newProseWriter(false, false) // stdout is a pipe here, so anim is false
		if pw.anim {
			t.Fatal("captureStdout left the writer animating; the test proves nothing")
		}
		pw.begin("kimi-k3")
		pw.feed("the answer\n")
		pw.end()
	})
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("a piped turn carried an escape: %q", out)
	}
	// and no gutter glyph either. plainTheme empties the palette but keeps the
	// glyph tier, so the answer still carried a decorative " ▌ " on every line of a
	// redirected payload — `lca -p "…" >> CHANGELOG.md` went from usable text to
	// text with a U+258C down the left edge.
	if strings.Contains(out, gProse) {
		t.Errorf("a piped turn drew the prose gutter %q: %q", gProse, out)
	}
	for _, want := range []string{"waiting for kimi-k3", "the answer"} {
		if !strings.Contains(out, want) {
			t.Errorf("a piped turn lost %q: %q", want, out)
		}
	}
}

// ── the machine paths, which the look must not reach ────────────────────────

// A skin is allowed on a SCREEN. Redirected or piped, the escapes were already
// gone, but a frame around somebody's payload, a block-rune gauge in it and a
// corridor of rooms drawn across it are decoration too, and the brief forbids new
// decoration there as firmly as it forbids escapes: `lca "task" > out.txt` gained
// five gauge rows per turn and `lca run nightly | head` printed the whole
// ╔═ STEPS ═╗ box into the pipe.
//
// Theme.Frames is the one gate, so this test is the one place the promise is
// checked — for every rune the look draws that is not a map glyph.
func TestPipedScreensCarryNoNewDecoration(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	saved := activeTheme
	t.Cleanup(func() { setTheme(saved) })
	// exactly what themeFor returns for a run whose stdout is not a terminal
	setTheme(themeFor(themeDungeon, termCaps{tty: false, colour: true, depth: depthTruecolor, unicode: true}))
	if activeTheme.Frames {
		t.Fatal("a run with no terminal must not draw frames")
	}

	out := captureStdout(t, func() {
		printPerf(Usage{PromptTokens: 4200, CachedTokens: 3822, CompletionTokens: 512,
			GenDur: 7 * time.Second, TTFT: 310 * time.Millisecond})
		p := newPanel("steps", "nightly")
		p.tag("r-20260930-1")
		p.Row("map", "unreachable: mapStrip is not called at all")
		p.Div("largest tool outputs")
		p.Row("trace", "/var/log/lca/traces/20260930-022035-2339.jsonl")
		p.Print()
		pw := newProseWriter(false, false) // anim is false: this is a pipe
		pw.printLine("the answer, which is somebody's payload")
		pw.end()
	})

	// the frames, the gauges, the prose gutter — every rune the look adds
	for _, r := range []string{gPanelTL, gPanelTR, gPanelBL, gPanelBR, gPanelH, gPanelV,
		gPanelML, gPanelMR, gaFull, gaHalf, gaEmpty, gaBad, gProse} {
		if strings.Contains(out, r) {
			t.Errorf("a piped run drew %q:\n%s", r, out)
		}
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("a piped run carried an escape: %q", out)
	}
	// and it still says everything it is for
	for _, want := range []string{"4.2k in (91% cached)", "512 out", "STEPS", "nightly",
		"/var/log/lca/traces/20260930-022035-2339.jsonl", "the answer, which is somebody's payload"} {
		if !strings.Contains(out, want) {
			t.Errorf("a piped run lost %q:\n%s", want, out)
		}
	}
}

// The run's map is a picture, so it is not drawn into a pipe at all — the table
// under it states every one of the same facts in words.
func TestPipedRunSummaryDrawsNoMap(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	saved := activeTheme
	t.Cleanup(func() { setTheme(saved) })
	setTheme(themeFor(themeDungeon, termCaps{tty: false, colour: true, depth: depth256, unicode: true}))
	r := testRunner(3, []string{stepOK, stepFailed, stepSkipped})
	out := stripANSI(captureStdout(t, r.summary))
	if strings.Contains(out, "map") {
		t.Errorf("the map was drawn into a pipe:\n%s", out)
	}
	for _, want := range []string{"failed", "skipped"} { // the table still speaks
		if !strings.Contains(out, want) {
			t.Errorf("the piped summary lost %q:\n%s", want, out)
		}
	}
}

// ── the map ─────────────────────────────────────────────────────────────────

// testRunner is the smallest wfRunner that can draw: a workflow of n steps and
// the statuses they ended on.
func testRunner(n int, statuses []string) *wfRunner {
	wf := &Workflow{Name: "nightly"}
	st := &WorkflowState{Run: "r-20260930-1"}
	for i := 0; i < n; i++ {
		wf.Steps = append(wf.Steps, &WorkflowStep{Name: fmt.Sprintf("s%d", i+1), Kind: "run"})
		s := ""
		if i < len(statuses) {
			s = statuses[i]
		}
		st.Steps = append(st.Steps, StepState{Name: fmt.Sprintf("s%d", i+1), Kind: "run", Status: s})
	}
	return &wfRunner{wf: wf, st: st}
}

// The map states its counts, always, and never cuts a room in half.
//
// The counts were never printed and the key was unreachable from four steps up,
// so a real workflow drew an unexplained row of brackets — a picture with no
// number, which is the one thing the gauge rule forbids. And from thirteen steps
// up the strip was 82 columns, wrapTo could not break it (it has no spaces) and
// panelSplit cut it: a "[" with its "]" on the next framed row.
func TestRunMapStatesItsCountsAndKeepsItsRoomsWhole(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	for _, n := range []int{1, 3, 5, 14, 40} {
		statuses := make([]string, n)
		for i := range statuses {
			statuses[i] = []string{stepOK, stepFailed, stepSkipped, ""}[i%4]
		}
		r := testRunner(n, statuses)
		width := nominalContent() - 10
		rows := r.mapStrip(width)
		if len(rows) == 0 {
			t.Fatalf("%d steps drew no map", n)
		}
		all := stripANSI(strings.Join(rows, "\n"))
		// the counts are never dropped; a zero category is, the way the tally line
		// under the table drops one, so what is asserted is every category that
		// actually happened
		want := []string{"ok"}
		for _, c := range []struct{ status, word string }{
			{stepFailed, "failed"}, {stepSkipped, "skipped"}, {"", "not entered"},
		} {
			for _, got := range statuses {
				if got == c.status {
					want = append(want, c.word)
					break
				}
			}
		}
		for _, w := range want {
			if !strings.Contains(all, w) {
				t.Errorf("%d steps: the map does not count %q:\n%s", n, w, all)
			}
		}
		for i, l := range rows {
			if w := visibleWidth(l); w > width {
				t.Errorf("%d steps: map row %d is %d columns, the frame holds %d: %q", n, i, w, width, stripANSI(l))
			}
			if o, c := strings.Count(stripANSI(l), "["), strings.Count(stripANSI(l), "]"); o != c {
				t.Errorf("%d steps: map row %d cuts a room in half: %q", n, i, stripANSI(l))
			}
		}
	}
}

// ── the door ────────────────────────────────────────────────────────────────

// The door's target is the whole question, and it must survive whole. It moved
// into the panel's title course, where fitTop ellipsizes — and for webfetch, read,
// list, glob, grep and edit that header is the ONLY statement of what is being
// approved, because their previews are empty or carry no path. A truncated URL in
// an approval prompt defeats the gate.
func TestDoorNeverCutsItsTarget(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	for _, c := range []struct{ kind, header, preview string }{
		{"web", "FETCH https://raw.githubusercontent.com/some-org/some-repo/main/scripts/bootstrap.sh", ""},
		{"edit", "EDIT internal/services/authentication/providers/oauth2/handler_test.go", "  - a\n  + b"},
		{"run", "RUN go build -o lca .", "  $ go build -o lca ."},
	} {
		ap := NewApprover(newStringInput(""))
		out := stripANSI(captureStdout(t, func() { ap.Confirm(c.kind, c.header, c.preview) }))
		want := humanHeader(c.header)
		// At 80 columns a target longer than the frame's 74 cannot be contiguous
		// anywhere, so what is asserted is that not one byte of it was dropped:
		// panelSplit hard-splits and ellipsize does not, which is the whole
		// difference between reading a URL over two rows and approving the wrong one.
		if !strings.Contains(squash(out), squash(want)) {
			t.Errorf("the door cut its own target:\n want %q\n got:\n%s", want, out)
		}
		if !strings.Contains(out, "open it?") {
			t.Errorf("the door asked nothing:\n%s", out)
		}
		for _, l := range strings.Split(out, "\n") {
			if w := visibleWidth(l); w > 80 {
				t.Errorf("the door is %d columns wide: %q", w, l)
			}
		}
	}
}

// ── widths ──────────────────────────────────────────────────────────────────

// A path is one token and must not be cut, and the frame gives way before the
// identifier does. wrapTo never breaks a word, so an over-long path fell through
// to panelSplit, which hard-cut it at the frame: the last four characters of a
// trace filename on the next framed row with ║ between them, which is the one
// string in /stats an operator copies.
func TestPanelKeepsAnUnbreakableValueWhole(t *testing.T) {
	const p = "/private/tmp/claude-501/skin/lcadir/traces/20260930-022035-2339.jsonl"
	for _, cols := range []string{"80", "100", "140"} {
		t.Run(cols, func(t *testing.T) {
			screenTheme(t, themeDungeon, true)
			t.Setenv("COLUMNS", cols)
			pnl := newPanel("the sheet", "this session")
			pnl.Row("turns", "3 model calls")
			pnl.Row("trace", faint("%s", p))
			ls := stripEach(pnl.Lines())
			joined := strings.Join(ls, "\n")
			if !strings.Contains(joined, p) {
				t.Errorf("the path was sheared:\n%s", joined)
			}
			w := visibleWidth(ls[0])
			for _, l := range ls {
				if visibleWidth(l) != w {
					t.Errorf("the frame is ragged:\n%s", joined)
					break
				}
			}
			if lim, _ := strconv.Atoi(cols); w > lim {
				t.Errorf("the frame is %d columns at a terminal of %s", w, cols)
			}
		})
	}
}

// The status line lives inside the line editor's repainted region and so cannot be
// allowed to wrap: only the path and the CTX gauge were droppable, and the role,
// the model id and three postures are 104 columns on their own.
func TestStatusLineNeverWraps(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	h.sess.Loop = true
	for _, cols := range []string{"60", "80", "120"} {
		t.Run(cols, func(t *testing.T) {
			screenTheme(t, themeDungeon, true)
			t.Setenv("COLUMNS", cols)
			lim, _ := strconv.Atoi(cols)
			if w := visibleWidth(r.statusLine()) + 2; w > lim {
				t.Errorf("the status line is %d columns at a terminal of %s: %q", w, cols, stripANSI(r.statusLine()))
			}
		})
	}
}

// The picker repaints a known number of lines and walks the cursor back over
// exactly that many. The frame is drawn INSIDE lines(), so the region is two lines
// taller than the reserve was tuned for — and a region taller than the screen
// scrolls before the "\033[<n>A" runs, which lands the walk-back above the menu.
func TestPickerRepaintFitsItsReserve(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	var cs []choice
	for i := 0; i < 30; i++ {
		cs = append(cs, choice{id: fmt.Sprintf("m%d", i), label: fmt.Sprintf("model-number-%d", i),
			detail: "a detail", note: "a note that is long enough to wrap onto a second line inside the frame"})
	}
	var bare []choice
	for _, c := range cs {
		bare = append(bare, choice{id: c.id, label: c.label, detail: c.detail})
	}
	for h := 3; h <= 20; h++ {
		p := newPickState(bare, pickOpts{multi: true, title: "setup", detail: "3/6", height: h})
		if n := len(p.lines()); n > h+pickChrome {
			t.Errorf("a viewport of %d rows repainted %d lines; the structural chrome is %d", h, n, pickChrome)
		}
		// and the highlighted row's note and warning stay inside the four rows the
		// reserve keeps spare for them
		q := newPickState(cs, pickOpts{multi: true, title: "setup", detail: "3/6", height: h})
		if n := len(q.lines()); n > h+pickChrome+4 {
			t.Errorf("a viewport of %d rows repainted %d lines with a note; the reserve pays for %d", h, n, h+pickChrome+4)
		}
	}
	// and the reserve itself is what pickHeight hands out
	if h := pickHeight(); h < 3 {
		t.Errorf("pickHeight gave %d rows", h)
	}
}

// squash removes the frame and the whitespace, so a value hard-split across two
// framed rows can still be proved whole.
func squash(s string) string {
	for _, r := range []string{gPanelV, gPanelH, gPanelTL, gPanelTR, gPanelBL, gPanelBR, " ", "\n"} {
		s = strings.ReplaceAll(s, r, "")
	}
	return s
}

// ── the transcript's own rows ───────────────────────────────────────────────

// A hazard printed while the model is being waited on gets its own gutter row. The
// spinner owns the terminal's current row and repaints it every 90 ms, so a
// gwpolicy retry appended to it put two class glyphs on one line — the one line on
// the screen where the left column is not a map. markerRowLead takes the row back;
// the spinner redraws itself one row down on its next tick.
func TestHazardLinesDoNotShareTheMarkerRow(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	saved := markerLive.Load()
	t.Cleanup(func() { markerLive.Store(saved) })

	markerLive.Store(false)
	quiet := captureStdout(t, func() { (&termView{}).Warn("glm-5.3 is not up") })
	if strings.Contains(quiet, "\033[K") {
		t.Errorf("a warning erased a row with no marker on it: %q", quiet)
	}

	markerLive.Store(true)
	for name, fn := range map[string]func(*termView, string){
		"Note":  (*termView).Note,
		"Warn":  (*termView).Warn,
		"Error": (*termView).Error,
	} {
		out := captureStdout(t, func() { fn(&termView{}, "kimi-k3 overloaded") })
		if !strings.HasPrefix(out, "\r\033[K") {
			t.Errorf("%s landed on the marker's row: %q", name, out)
		}
		// the class glyph is still the first thing after the lead and the one space
		line := strings.TrimPrefix(strings.TrimRight(out, "\n"), "\r\033[K")
		if !strings.HasPrefix(line, " ") {
			t.Errorf("%s starts in column 0: %q", name, line)
		}
		if !strings.Contains(line, "kimi-k3 overloaded") {
			t.Errorf("%s lost its text: %q", name, line)
		}
	}
}

// The finished marker keeps the flame's resting frame, so the line does not change
// width at the moment it stops moving — and the closing line is measured against
// the terminal the same way every tick was.
func TestClosedMarkerKeepsItsRestingFrame(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	out := captureStdout(t, func() {
		pw := newProseWriter(false, false)
		pw.anim = true
		pw.markerLabel = "thinking"
		pw.beginReasoning()
		pw.closeMarker()
	})
	final := stripANSI(out)
	final = final[strings.LastIndex(final, "\r\033[K")+len("\r\033[K"):]
	if !strings.Contains(final, " "+gSpinner[0]+" ") {
		t.Errorf("the closing marker lost its resting frame %q: %q", gSpinner[0], final)
	}
	if !strings.Contains(final, "thought") {
		t.Errorf("the closing marker lost its word: %q", final)
	}
}

// A paragraph wider than the terminal keeps the gutter on every visual row. Without
// a wrap the claim that you can find where the model spoke by scanning one
// character wide was false for the common case, and the terminal broke the line
// mid-word.
func TestProseWrapsWithItsGutter(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	const long = "Sum skipped negatives because of the n < 0 guard on line 6. I removed it, and the table test in sum_test.go now covers -3 and -1 so the regression cannot come back quietly."
	out := stripANSI(captureStdout(t, func() {
		pw := newProseWriter(false, false)
		pw.anim = true
		pw.printLine(long)
		pw.out("\n")
	}))
	rows := strings.Split(strings.Trim(out, "\n"), "\n")
	if len(rows) < 2 {
		t.Fatalf("the paragraph did not wrap:\n%s", out)
	}
	var words []string
	for _, r := range rows {
		if !strings.HasPrefix(r, " "+gProse+" ") {
			t.Errorf("a wrapped row lost the gutter: %q", r)
		}
		if w := visibleWidth(r); w > 80 {
			t.Errorf("a wrapped row is %d columns: %q", w, r)
		}
		words = append(words, strings.TrimSpace(strings.TrimPrefix(r, " "+gProse+" ")))
	}
	// no word was broken: the rejoined rows are the paragraph
	if got := strings.Join(words, " "); got != long {
		t.Errorf("the wrap changed the words:\n want %q\n  got %q", long, got)
	}
	// a fenced code line is NOT re-flowed: it keeps the file's own line breaks
	fixed := stripANSI(captureStdout(t, func() {
		pw := newProseWriter(false, false)
		pw.anim = true
		pw.printFixed(long)
		pw.out("\n")
	}))
	if n := len(strings.Split(strings.Trim(fixed, "\n"), "\n")); n != 1 {
		t.Errorf("a fixed line was re-flowed onto %d rows:\n%s", n, fixed)
	}
}

// Two counts about the same calls, one row apart, must follow the same rule: the
// parse count printed its zero and the error count did not, so the reader could
// not tell "none returned an error" from "this build does not report it".
func TestStatsReportsAZeroErrorCount(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	screenTheme(t, themeDungeon, true)
	h.sess.stats = SessionStats{Turns: 1, ToolCalls: 2, PromptTokens: 4000, CachedTokens: 3600}
	out := stripANSI(captureStdout(t, func() { r.cmdStats("") }))
	for _, want := range []string{"2 calls", "0 didn't parse", "0 returned an error"} {
		if !strings.Contains(out, want) {
			t.Errorf("/stats did not say %q:\n%s", want, out)
		}
	}
	// and the two bars in one panel end at the same column
	var bars []int
	for _, l := range strings.Split(out, "\n") {
		if i := strings.IndexAny(l, gaFull+gaEmpty+gaBad); i >= 0 {
			bars = append(bars, strings.LastIndexAny(l, gaFull+gaEmpty+gaBad))
		}
	}
	if len(bars) == 2 && bars[0] != bars[1] {
		t.Errorf("the two gauges end at columns %d and %d:\n%s", bars[0], bars[1], out)
	}
}

// The picker's window column is a NUMBER and a provenance word, and the number is
// what a column is right-aligned for. Right-aligning "1.05M (server)" against
// "1.05M (card)" as one string lines up the closing bracket instead, so two
// identical windows read two columns apart.
func TestPickerWindowDigitsLineUp(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	served := []ModelInfo{{ID: "glm-5.3", MaxLen: 1048576}, {ID: "kimi-k3", MaxLen: 1048576}}
	scale := windowScale(served)
	var at []int
	for _, m := range served {
		c := modelChoice(m, false, scale)
		at = append(at, strings.Index(stripANSI(c.detail), "1.05M"))
	}
	if at[0] < 0 || at[0] != at[1] {
		t.Errorf("two identical windows sit at columns %v", at)
	}
	// and a window nobody published keeps the words rather than a number
	unknown := stripANSI(modelChoice(ModelInfo{ID: "frobnicator-9"}, false, scale).detail)
	if !strings.Contains(unknown, "window ?") {
		t.Errorf("an unknown window lost its words: %q", unknown)
	}
}

// The one screen that only appears when something has already gone wrong was also
// the one that spilled: hints.go's longest sentence is 109 columns, and Error
// printed it on one row.
func TestErrorHintsFitTheTerminal(t *testing.T) {
	screenTheme(t, themeDungeon, true)
	long := "nothing answers at that endpoint — is the gateway up? /setup asks for it again, /doctor checks the setup"
	out := stripANSI(captureStdout(t, func() {
		(&termView{}).Error("kimi-k3 failed: connection refused\n" + long)
	}))
	var got []string
	for _, l := range strings.Split(strings.Trim(out, "\n"), "\n") {
		if w := visibleWidth(l); w > 80 {
			t.Errorf("an error line is %d columns: %q", w, l)
		}
		if i := strings.Index(l, gHint); i >= 0 {
			got = append(got, strings.TrimSpace(l[i+len(gHint):]))
		} else if strings.HasPrefix(l, "    ") {
			got = append(got, strings.TrimSpace(l))
		}
	}
	if rejoined := strings.Join(got, " "); rejoined != long {
		t.Errorf("the wrap changed the sentence:\n want %q\n  got %q", long, rejoined)
	}
}
