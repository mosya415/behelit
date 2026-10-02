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
	"unicode/utf8"
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
// cols is variadic and defaults to 80, which is why the twenty-six calls that
// were written before the program could be any other width did not have to
// change: they still say 80, by saying nothing.
func screenTheme(t *testing.T, name string, unicode bool, cols ...int) {
	t.Helper()
	w := 80
	if len(cols) > 0 {
		w = cols[0]
	}
	t.Setenv("COLUMNS", strconv.Itoa(w))
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
		width := panelRowRoom()
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
	// The structural reserve holds from the 80-column floor up. Below it the LEGEND
	// takes more rows than pickChrome counts — it is the only place ^a, ^n and "type
	// to filter" are documented, so it wraps whole rather than being cut, and wrapTo
	// refuses to wrap below 20 columns at all. That is the height axis: at HEAD the
	// same budget is already overshot by two rows at 40 columns and five at 20, and
	// drawing the legend to the page rather than to the terminal's last column
	// (which is where it used to end at 60, in the pending-wrap column) moves the
	// boundary by one row at 60. Named here rather than papered over.
	for _, cols := range widthCases {
		t.Setenv("COLUMNS", strconv.Itoa(cols))
		for h := 3; h <= 20; h++ {
			p := newPickState(bare, pickOpts{multi: true, title: "setup", detail: "3/6", height: h})
			q := newPickState(cs, pickOpts{multi: true, title: "setup", detail: "3/6", height: h})
			plain, noted := len(p.lines()), len(q.lines())
			if cols >= 80 && plain > h+pickChrome {
				t.Errorf("a viewport of %d rows repainted %d lines at a terminal of %d; the structural chrome is %d", h, plain, cols, pickChrome)
			}
			// And THIS holds at every width, which is what the two-line clamp on the
			// note and the warning buys: the highlighted row's own two sentences cost
			// exactly the four rows the reserve keeps spare for them, where before the
			// clamp a long note at a narrow terminal took four lines on its own.
			if noted-plain > 4 {
				t.Errorf("a highlighted row's note and warning cost %d rows at a terminal of %d; the reserve pays for 4", noted-plain, cols)
			}
			if cols >= 80 && noted > h+pickChrome+4 {
				t.Errorf("a viewport of %d rows repainted %d lines with a note at a terminal of %d; the reserve pays for %d", h, noted, cols, h+pickChrome+4)
			}
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
		if w := visibleWidth(r); w > houseWidth()+1 {
			t.Errorf("a wrapped row is %d columns, the page ends in column %d: %q", w, houseWidth()+1, r)
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

// ── the width, asserted ─────────────────────────────────────────────────────
//
// The program used to be 76 columns wide whatever the terminal was, so a
// 140-column window held the whole interface in its left half and a 200-column
// one in its left third. Structure now takes the window up to a page (pageMax)
// and prose stops at a reading measure (proseMax). Four promises come out of
// that, and all four are checked below: the 80-column floor does not move,
// nothing is drawn past the page, the width is actually USED, and a repainted
// region is measured against ONE snapshot of the terminal.

// widthCases are the terminals the width is asserted at: the clamp floor, two
// narrow windows, the floor this look was designed for, a full-screen laptop, the
// complaint's own case, the page's ceiling and a tmux pane across two monitors.
var widthCases = []int{20, 40, 60, 80, 100, 140, 200, 400}

// chromeCourse is the visible width of l when l is a full-width course of chrome
// — a panel's top or bottom course, or a section's lintel — and 0 when it is
// anything else. These are the lines that are supposed to end in the same column,
// which is the whole claim of houseWidth().
func chromeCourse(l string) int {
	s := stripANSI(l)
	for _, pre := range []string{" " + gPanelTL, " " + gPanelBL, " " + gRule + gRule + " "} {
		if strings.HasPrefix(s, pre) {
			return visibleWidth(s)
		}
	}
	return 0
}

// panelSpan is the two ends a frame's width must lie between: the floor it is
// drawn to when its content asked for nothing, and the page. It is written here
// once so the several width tests cannot each have their own idea of it.
func panelSpan() (floor, ceiling int) {
	return min(panelFloor, houseWidth()), houseWidth() + 1
}

// The chrome, at three widths, in both tiers, as exact lines. Every one of these
// ends in column houseWidth()+1: the frame, the lintel, the input fence and the
// submitted band all agree, which they did not before — the lintel was one course
// short of the frame beside it and the fence and the band had no gutter at all.
//
// In the plain tier the assertion is the opposite one and just as important: with
// Theme.Frames off the panels come back through bareLines(), so there is no frame
// to widen, nothing is split and no path is cut. The only width-sensitive chrome
// left in that tier is the lintel.
func TestChromeGoldenAtEveryWidth(t *testing.T) {
	for _, c := range []struct {
		tier string
		cols int
		want []string
	}{
		{"dungeon", 80, []string{
			" ╔═ THE HOLD ═ where you stand ═════════════════════════════════════════════╗",
			" ╚══════════════════════════════════════════════════════════════════════════╝",
			" ╔═ A DOOR ═ edit sum.go ══════════════════════════════════════ [coder t1] ═╗",
			" ╚══════════════════════════════════════════════════════════════════════════╝",
			" ── PLAN  nightly ───────────────────────────────────────────────────────────",
			" ────────────────────────────────────────────────────────────────────────────",
			" › fix Sum in sum.go                                                         ",
		}},
		{"dungeon", 100, []string{
			" ╔═ THE HOLD ═ where you stand ═════════════════════════════════════════════╗",
			" ╚══════════════════════════════════════════════════════════════════════════╝",
			" ╔═ A DOOR ═ edit sum.go ══════════════════════════════════════ [coder t1] ═╗",
			" ╚══════════════════════════════════════════════════════════════════════════╝",
			" ── PLAN  nightly ───────────────────────────────────────────────────────────",
			" ────────────────────────────────────────────────────────────────────────────────────────────────",
			" › fix Sum in sum.go                                                                             ",
		}},
		{"dungeon", 140, []string{
			" ╔═ THE HOLD ═ where you stand ═════════════════════════════════════════════╗",
			" ╚══════════════════════════════════════════════════════════════════════════╝",
			" ╔═ A DOOR ═ edit sum.go ══════════════════════════════════════ [coder t1] ═╗",
			" ╚══════════════════════════════════════════════════════════════════════════╝",
			" ── PLAN  nightly ───────────────────────────────────────────────────────────",
			" ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────",
			" › fix Sum in sum.go                                                                                                                     ",
		}},
		{"plain", 80, []string{
			" THE HOLD  where you stand",
			" project   cli-agents  ~/work/",
			" A DOOR  edit sum.go  [coder t1]",
			"   - if n < 0 { continue }",
			" ── PLAN  nightly ───────────────────────────────────────────────────────────",
			" ────────────────────────────────────────────────────────────────────────────",
			" › fix Sum in sum.go                                                         ",
		}},
		{"plain", 100, []string{
			" THE HOLD  where you stand",
			" project   cli-agents  ~/work/",
			" A DOOR  edit sum.go  [coder t1]",
			"   - if n < 0 { continue }",
			" ── PLAN  nightly ───────────────────────────────────────────────────────────",
			" ────────────────────────────────────────────────────────────────────────────────────────────────",
			" › fix Sum in sum.go                                                                             ",
		}},
		{"plain", 140, []string{
			" THE HOLD  where you stand",
			" project   cli-agents  ~/work/",
			" A DOOR  edit sum.go  [coder t1]",
			"   - if n < 0 { continue }",
			" ── PLAN  nightly ───────────────────────────────────────────────────────────",
			" ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────",
			" › fix Sum in sum.go                                                                                                                     ",
		}},
	} {
		t.Run(fmt.Sprintf("%s/%d", c.tier, c.cols), func(t *testing.T) {
			screenTheme(t, c.tier, true, c.cols)
			assertLines(t, strings.Join(chromeSpecimen(), "\n"), c.want)
		})
	}
}

// chromeSpecimen is one of each full-width surface: a panel with a row, a lit
// door with a preview line, a lintel, the input fence and a submitted band.
func chromeSpecimen() []string {
	hold := newPanel("the hold", "where you stand")
	hold.Row("project", "cli-agents  ~/work/")
	h := stripEach(hold.Lines())
	door := newPanel("a door", "edit sum.go").door().tag("[coder t1]")
	door.Line("%s", "  - if n < 0 { continue }")
	d := stripEach(door.Lines())
	lintel := strings.Split(strings.Trim(stripANSI(captureStdout(&testing.T{}, func() { section("plan", "nightly") })), "\n"), "\n")
	out := []string{h[0], h[len(h)-1], d[0], d[len(d)-1]}
	out = append(out, lintel...)
	return append(out, stripANSI(inputFence(houseWidth())), stripANSI(inputBand(houseWidth(), "› fix Sum in sum.go")))
}

// drawTables is the two widest DATA screens the program draws, through the real
// primitives: the -dry-run plan and eval's results, each with its own lintel.
// The sweep below had no table in it at all, which is why a 92-column plan row on
// an 80-column terminal and a 94-column results row went unseen.
func drawTables() {
	tb := wideTables[0]
	sectionTable("plan", "nightly", tb.header, tb.rows, tb.mid...)
	ev := wideTables[4]
	sectionTable("results", "", ev.header, ev.rows, ev.mid...)
}

// Nothing may be drawn past the TERMINAL, and the room the terminal has must be
// used.
//
// Two bounds, because the change separated two questions that were one number.
// Decoration — a frame, a lintel, a fence, a band — stops at the page, because a
// 400-column rule is noise. DATA takes the window: a table is budgeted against
// the terminal (fitWidth), so at 200 columns a 165-column plan row goes in whole
// instead of being cut to 161 with 39 columns standing empty beside it. So the
// per-line bound here is the terminal's own room and the chrome bound is the page.
//
// The 80 in max(80, …) is not slack: a gauge row is a fixed-width picture of a
// fraction, so it keeps its width at a terminal narrower than the floor this look
// was designed for.
//
// The one piece of chrome that may be wider than the page is a frame that Row()
// asked to widen for a token it could not break (panel.need) — /stats' trace path
// is the case, and TestPanelKeepsAnUnbreakableValueWhole is where that hatch is
// checked. chromeCourse skips it by only matching a course that is at least the
// floor.
func TestNoScreenExceedsItsTerminal(t *testing.T) {
	for _, tier := range []struct {
		name    string
		theme   string
		unicode bool
	}{
		{"dungeon", themeDungeon, true},
		{"plain", themePlain, true},
		{"ascii", themeDungeon, false},
	} {
		for _, cols := range widthCases {
			t.Run(fmt.Sprintf("%s/%d", tier.name, cols), func(t *testing.T) {
				screenTheme(t, tier.theme, tier.unicode, cols)
				out := captureStdout(t, func() { drawTurn(); drawPanels(); drawPicker(); drawTables() })
				page := houseWidth() + 1
				room := max(80, fitWidth())
				// The two data tables are asserted EXACTLY, and their rows are then
				// exempt from the per-line bound — because a table whose floors cannot
				// reach the budget is the one thing on a screen that is allowed to be
				// wider than the terminal, and it is a decision rather than a leak. A
				// nine-column plan cannot be made to fit 80 columns by any cutting that
				// leaves the row readable, so it goes in whole and the terminal soft-wraps
				// it, which keeps the bytes contiguous. Exempting them by exact line
				// rather than by a looser bound keeps the bound tight for everything else.
				tableRow := map[string]bool{}
				blockWidth := map[int]bool{}
				for _, i := range []int{0, 4} {
					tb := wideTables[i]
					ls, got := tableBlock(tb.header, tb.rows, tb.mid...)
					natural, floor := tableSpan(tb.header, tb.rows, "  ")
					want := natural
					if floor <= fitWidth() && natural > fitWidth() {
						want = fitWidth()
					}
					if got != want {
						t.Errorf("%s is %d columns at a terminal of %d; it should be %d (natural %d, floors %d, room %d)",
							tb.name, got, cols, want, natural, floor, fitWidth())
					}
					for _, l := range ls {
						tableRow[stripANSI(l)] = true
					}
					blockWidth[got] = true
				}
				for _, l := range strings.Split(out, "\n") {
					if tableRow[stripANSI(l)] {
						continue
					}
					if w := visibleWidth(l); w > room {
						t.Errorf("%d columns at a terminal of %d (its room is %d):\n%q", w, cols, room, stripANSI(l))
					}
					// No course of decoration goes past the page — unless it is a lintel
					// drawn to a data block, and then it is exactly that block's width,
					// because a rule one column short of the table it heads reads as a
					// rendering fault rather than as a decision.
					if w := chromeCourse(l); w != 0 && w > page && !blockWidth[w] {
						t.Errorf("a course of chrome is %d columns, the page is %d and no block is that wide:\n%q",
							w, page, stripANSI(l))
					}
				}
			})
		}
	}
}

// A panel's courses must be the same width at every terminal, not just at 80: the
// top one is assembled from styled parts and the bottom is a plain repeat, so the
// two are computed twice and can drift — and now they are computed from a number
// that moves with the content as well as with the window.
//
// The width itself is the contract the change moved. A frame is drawn to what is
// IN it, floored at panelFloor so a two-line panel is still a box and ceilinged at
// the page. Drawn to the page unconditionally, THE HOLD on a 200-column screen was
// a 161-column box whose longest row ended in column 62: 99 blank columns, padded
// and closed with a ║, which reads worse than the same rows in a 76-column box
// because the box measures the emptiness and the rule points at it.
func TestPanelFrameIsSquareAtEveryWidth(t *testing.T) {
	for _, cols := range widthCases {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themeDungeon, true, cols)
			floor, ceiling := panelSpan()
			for _, c := range []struct{ title, detail, right string }{
				{"the hold", "where you stand", ""},
				{"a door", "edit some/very/long/path/that/goes/on.go", "[coder t1]"},
				{"steps", "", "r-20260930-1"},
				{"x", "", ""},
			} {
				p := newPanel(c.title, c.detail).tag(c.right)
				p.Row("label", "value")
				ls := p.Lines()
				want := visibleWidth(ls[0])
				for _, l := range ls {
					if visibleWidth(l) != want {
						t.Errorf("panel %q is ragged at a terminal of %d:\n%s", c.title, cols, strings.Join(stripEach(ls), "\n"))
						break
					}
				}
				if want < floor+1 || want > ceiling {
					t.Errorf("panel %q is %d columns at a terminal of %d; the floor is %d and the page is %d",
						c.title, want, cols, floor+1, ceiling)
				}
				// and it is drawn to its content, not to a constant: the frame is exactly
				// what the widest thing in it asked for, once the floor and the page have
				// had their say
				if got := p.width() + 1; got != want {
					t.Errorf("panel %q draws %d columns but measures %d at a terminal of %d", c.title, want, got, cols)
				}
				if w := min(ceiling-1, max(p.contentWidth(), floor)); p.width() != w {
					t.Errorf("panel %q is %d columns at a terminal of %d; its content asks for %d (floor %d, page %d)",
						c.title, p.width(), cols, w, floor, ceiling-1)
				}
			}
			// At 80 the floor IS the page, so nothing moved there and could not have:
			// every panel is the 76-column frame it has always been.
			if cols == 80 {
				p := newPanel("the hold", "where you stand")
				p.Row("project", "cli-agents  ~/work/")
				if w := visibleWidth(p.Lines()[0]); w != 77 {
					t.Errorf("the 80-column frame is %d columns; it has always been 77", w)
				}
			}
		})
	}
}

// ── the tables ──────────────────────────────────────────────────────────────

// wideTables are the four widest tables the program draws, with the cells the
// display truncates used to cut before the table ever saw them.
var wideTables = []struct {
	name   string
	header []string
	rows   [][]string
	mid    []int
}{
	{"plan",
		[]string{"#", "step", "kind", "role", "member", "tries", "timeout", "when", "check"},
		[][]string{
			{"1", "build", "run", "—", "—", "1", "10m", "always", "go build ./... && go vet ./... && gofmt -l ."},
			{"2", "harden-the-parser", "delegate", "coder → lead", "box", "3", "30m", "conditional", "go test -count=1 -race ./... && ./scripts/check-goldens.sh"},
			// 79 characters, which is the row the suite could not see: firstLine() cut
			// every cell at 60 bytes before the table was ever asked to fit it, so the
			// width-aware caps downstream were dead code for anything longer and the
			// fixture's own longest check was two short of noticing.
			{"3", "exit-status-reaches-the-model", "run", "—", "—", "1", "5m", "always", "go test ./internal/orchestrator/... -run TestExitStatusReachesTheModel -count=1"},
		},
		[]int{1, 8}},
	{"agents",
		[]string{"agent", "kind", "model", "what it does"},
		[][]string{
			{"lead", "primary", "qwen3-coder-480b-a35b-instruct", "plans the work and answers"},
			{"explore", "helper", "inherits", "reads the tree and reports back without changing anything"},
		},
		[]int{0, 2}},
	{"roles",
		[]string{"role", "tier", "models", "temp", "top_p", "effort", "context", "reasoning"},
		[][]string{
			{"lead", "—", "kimi-k3 → qwen3-coder-480b-a35b-instruct", "0.2 (role)", "0.95 (card)", "high", "1.05M (server)", "native"},
			{"coder", "local", "qwen3-coder-480b-a35b-instruct", "0.0 (role)", "1.0 (default)", "provider default", "131k (card)", "—"},
		},
		[]int{0, 2}},
	{"steps",
		[]string{"step", "kind", "role", "status", "time", "tries", "detail"},
		[][]string{
			{"build", "run", "—", "● ok", "1.2s", "1", ""},
			{"harden-the-parser", "delegate", "coder", "✕ failed", "42.1s", "2", "go test -count=1 -race ./... failed: sum_test.go:42 wanted 3 got -1"},
		},
		[]int{0, 6}},
	// eval's results table is the one whose HEADER ROW alone is 94 columns: eleven
	// columns of which ten are naturally short, so it cannot go below its own header
	// and the floors can never reach an 80-column budget. It belongs here because it
	// is the case that proves the shave gate: at 80 there is nothing to be gained by
	// cutting the task name, and it is the only cell that identifies the row.
	{"results",
		[]string{"task", "status", "time", "turns", "in", "cached", "out", "tools", "invalid", "delegated", "fallbacks"},
		[][]string{
			{"exit-status-reaches-the-model", "● pass", "12.4s", "3", "18.2k", "91%", "1.1k", "7", "0", "1/1", "0"},
			{"delegation-survives-a-retry", "✕ fail", "48.0s", "9", "64.0k", "77%", "4.2k", "22", "2/24", "1/2", "1"},
		},
		[]int{0}},
}

// The fit pass, stated as the THREE branches it has.
//
//  1. The natural widths fit: every cell is printed UNCHANGED, which is what keeps
//     every table that fits today byte-identical and what a wide window buys.
//  2. They do not fit and the floors can reach the budget: the widest column that
//     can still afford it gives up one column at a time until the row lands
//     exactly on the budget, and only then is a cell ellipsized.
//  3. They do not fit and the floors CANNOT reach the budget: nothing is shaved at
//     all. This is the branch the suite used to get wrong, because it obtained the
//     floor width by asking for a budget of 1 — which is itself case 3 — and then
//     asserted that the result was the floor. At an 80-column terminal the nine-
//     column plan row shaved `harden-the-parser` to `harde…parser` and the check to
//     twelve columns, and was STILL 84 columns against a budget of 77: it
//     soft-wrapped to two physical rows exactly as the unshaved row did, so the
//     shave bought no rows and cost the two cells that identify the row.
//
// The budget is fitWidth() and not the page, because a table is data and data
// takes the window.
func TestTablesFitAndStopCutting(t *testing.T) {
	widest := func(ls []string) int {
		w := 0
		for _, l := range ls {
			w = max(w, visibleWidth(l))
		}
		return w
	}
	for _, tb := range wideTables {
		for _, cols := range []int{80, 100, 140, 200, 400} {
			t.Run(fmt.Sprintf("%s/%d", tb.name, cols), func(t *testing.T) {
				screenTheme(t, themeDungeon, true, cols)
				budget := fitWidth()
				// the allocator's own numbers, not a second copy of the rule
				natural, floor := tableSpan(tb.header, tb.rows, "  ")
				got := tableLines(tb.header, tb.rows, "  ", budget, tb.mid...)
				cut := strings.Contains(stripANSI(strings.Join(got, "\n")), gEllipsis)
				shown := strings.Join(stripEach(got), "\n")
				switch {
				case natural <= budget:
					if widest(got) != natural || cut {
						t.Errorf("a table that fits (%d into %d) was fitted anyway:\n%s", natural, budget, shown)
					}
				case floor <= budget:
					if w := widest(got); w != budget {
						t.Errorf("fitted to %d columns; the budget is %d and the floors are %d:\n%s", w, budget, floor, shown)
					}
					if !cut {
						t.Errorf("a table shaved from %d to %d columns cut nothing:\n%s", natural, widest(got), shown)
					}
				default:
					// a shave that cannot land buys nothing, so it is not taken: the row
					// overflows at its natural widths, with every cell intact
					if w := widest(got); w != natural {
						t.Errorf("shaved to %d columns although the floors (%d) cannot reach the budget (%d):\n%s",
							w, floor, budget, shown)
					}
					if cut {
						t.Errorf("cut a cell although the floors (%d) cannot reach the budget (%d):\n%s", floor, budget, shown)
					}
				}
				// the payoff, named: a value that fits the WINDOW is printed whole, and at
				// 200 every one of these tables fits the window
				if cols >= 200 && cut {
					t.Errorf("%s still cuts a cell at a terminal of %d (natural %d, budget %d):\n%s", tb.name, cols, natural, budget, shown)
				}
			})
		}
	}
}

// pipedTheme installs what themeFor actually returns for a run whose stdout is
// not a terminal: no decoration AND no width. It is not the same thing as
// themePlain on a tty, which is NO_COLOR or TERM=dumb on a real screen — no
// decoration and a perfectly good width — and standing one in for the other is how
// the sober tier came to get none of the width work.
func pipedTheme(t *testing.T, cols int) {
	t.Helper()
	t.Setenv("COLUMNS", strconv.Itoa(cols))
	saved := activeTheme
	t.Cleanup(func() { setTheme(saved) })
	setTheme(themeFor(themeDungeon, termCaps{tty: false, colour: true, depth: depthTruecolor, unicode: true}))
	if activeTheme.Frames || activeTheme.Screen {
		t.Fatal("a run with no terminal has neither frames nor a width")
	}
}

// With no TERMINAL there is no width to fit to, so nothing is fitted — and that
// now holds for a panel's table too, which is what bareLines() has always promised
// every other line in the panel and could not keep for its rows.
//
// The condition is driven for real. This used to install themePlain on a tty and
// assert the no-fit behaviour there, which locked in the conflation it was meant
// to guard: a sober 80-column terminal then got no fitting at all, and its plan
// row went from bounded to 182 columns.
func TestPipedTableKeepsEveryCellWhole(t *testing.T) {
	pipedTheme(t, 80)
	for _, tb := range wideTables {
		pnl := newPanel("steps")
		pnl.Table(tb.header, tb.rows, pnl.room(), tb.mid...)
		out := strings.Join(stripEach(pnl.Lines()), "\n")
		if strings.Contains(out, gEllipsis) {
			t.Errorf("%s lost a cell with no frame to fit to:\n%s", tb.name, out)
		}
		for _, r := range tb.rows {
			for _, c := range r {
				if c != "" && !strings.Contains(out, stripANSI(c)) {
					t.Errorf("%s dropped %q with no frame to fit to:\n%s", tb.name, c, out)
				}
			}
		}
	}
}

// A four-step run's STEPS table is ONE framed row per step at every width, and at
// 140 it is one row per step with nothing cut.
//
// Before the fit pass, `detail` was truncated to 40 columns before the table saw
// it and no other column could give anything up, so the row measured 87 against a
// frame of 72 and panelSplit put every step on two framed rows — eleven rows for
// four steps, with a step name broken across a ║. Both directions are fixed by
// the same allocator: at 80 the columns share the shortfall and each step keeps
// its row, and at 140 the whole row goes in whole.
func TestStepsTableFitsOneRowPerStep(t *testing.T) {
	tb := wideTables[3]
	rows := append([][]string{}, tb.rows...)
	for len(rows) < 4 {
		rows = append(rows, tb.rows[1])
	}
	for _, cols := range []int{80, 100, 140, 200} {
		screenTheme(t, themeDungeon, true, cols)
		pnl := newPanel("steps")
		// room() and not width(): the frame is drawn to the table, so asking the
		// frame how wide it is before the table is in it is the question the other way
		// round. room() is the ceiling's interior, which is the budget.
		pnl.Table(tb.header, rows, pnl.room(), tb.mid...)
		body := strings.Join(stripEach(pnl.Lines()), "\n")
		if n := len(pnl.lines); n != len(rows)+1 {
			t.Errorf("a four-step STEPS table is %d framed rows at a terminal of %d, want %d:\n%s",
				n, cols, len(rows)+1, body)
		}
		if cols >= 140 && strings.Contains(body, gEllipsis) {
			t.Errorf("STEPS still cuts a cell at a terminal of %d:\n%s", cols, body)
		}
	}
}

// ── the prose ───────────────────────────────────────────────────────────────

// The answer grows with the window and then STOPS. Structure takes the width;
// a sentence takes a reading measure, because a 200-column line of text is
// harder to read than an 80-column one — and at 80 it wraps to exactly the 77
// columns it always did.
func TestProseStopsAtAReadingMeasure(t *testing.T) {
	const para = "The guard was removed because negatives are counted now, and the test that " +
		"asserted the old behaviour was rewritten to assert the new one; nothing else in the " +
		"package reads the counter, so the change is local to Sum and its two callers."
	for _, cols := range []int{80, 100, 140, 200, 400} {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themeDungeon, true, cols)
			pw := newProseWriter(false, false)
			pw.anim = true
			out := stripANSI(captureStdout(t, func() { pw.printLine(para); pw.end() }))
			widest := 0
			for _, l := range strings.Split(out, "\n") {
				widest = max(widest, visibleWidth(l))
			}
			// the answer is printed in its own gutter, so a wrapped line is the
			// measure plus the three columns of " ▌ " in front of it — and the measure is
			// taken from the PAGE, so that sum lands in column houseWidth()+1, where
			// every frame, lintel and fence on the same screen ends. Measured from the
			// terminal it was three columns past all of them at 80, and its last cell
			// rested in the DECAWM pending-wrap column.
			const gutter = 3
			measure := min(houseWidth()-2, proseMax)
			if widest > houseWidth()+1 {
				t.Errorf("the answer's longest line is %d columns at a terminal of %d; the page ends in column %d",
					widest, cols, houseWidth()+1)
			}
			if widest > measure+gutter {
				t.Errorf("the answer ran to %d columns at a terminal of %d; the measure is %d", widest, cols, measure)
			}
			// and it does use the measure it has: wrapTo breaks on spaces, so the
			// longest line lands within one word of it
			if widest < measure+gutter-12 {
				t.Errorf("the answer only reached %d columns at a terminal of %d; the measure is %d", widest, cols, measure)
			}
			if cols >= 100 && widest > proseMax+gutter {
				t.Errorf("the answer ran past the reading measure (%d) at a terminal of %d: %d columns", proseMax, cols, widest)
			}
		})
	}
}

// ── a mid-session resize ────────────────────────────────────────────────────

// There is no SIGWINCH handler and there does not need to be one: every width is
// read at draw time, the transcript already printed is history and is never
// re-emitted, and the three regions that DO repaint each take one snapshot per
// repaint. What is asserted here is the last of those — that a region is
// internally consistent, so a window moved between two repaints can leave two
// differently-sized snapshots but never one snapshot at two sizes, which is the
// shear.
func TestResizeDoesNotShearARepaintedRegion(t *testing.T) {
	var cs []choice
	for i := 0; i < 30; i++ {
		cs = append(cs, choice{id: fmt.Sprintf("m%d", i),
			label:  fmt.Sprintf("qwen3-coder-480b-a35b-instruct-%d", i),
			detail: "window 131k (server)",
			note:   "a note long enough to wrap onto a second line inside the frame and then onto a third",
			warn:   "and a warning that is also long enough to need more than one line of the frame it is in"})
	}
	snapshot := func(cols int) []string {
		t.Setenv("COLUMNS", strconv.Itoa(cols))
		p := newPickState(cs, pickOpts{multi: true, title: "setup", detail: "3/6", height: 8})
		return p.lines()
	}
	screenTheme(t, themeDungeon, true, 140)
	for _, order := range [][]int{{140, 80}, {80, 140}, {200, 20}, {60, 400}} {
		var widths []int
		for _, cols := range order {
			ls := snapshot(cols)
			page := houseWidth() + 1
			// one width for the whole region: every framed line is the same width, and
			// no line is past the page the walk-back was counted from
			framed := 0
			for _, l := range ls {
				if w := chromeCourse(l); w != 0 {
					if framed == 0 {
						framed = w
					}
					if w != framed {
						t.Fatalf("the picker mixed %d and %d columns in one repaint at a terminal of %d", framed, w, cols)
					}
				}
				// At the clamp floor the legend is the one thing that cannot comply:
				// wrapTo refuses to wrap below 20 columns, which is wider than a
				// 17-column page, and the legend is the only place ^a, ^n and "type to
				// filter" are documented — so it wraps whole rather than being cut. That
				// is the height axis and it is unchanged in direction by this work.
				if cols > 20 && visibleWidth(l) > page {
					t.Errorf("a picker line is %d columns at a terminal of %d; the page is %d:\n%q",
						visibleWidth(l), cols, page, stripANSI(l))
				}
			}
			// The frame is content-shaped, so what is asserted is the span and not a
			// constant: never narrower than the floor, never past the page.
			if floor, ceiling := panelSpan(); framed < floor+1 || framed > ceiling {
				t.Errorf("the picker's frame is %d columns at a terminal of %d; the floor is %d and the page is %d",
					framed, cols, floor+1, ceiling)
			}
			widths = append(widths, framed)
		}
		// A wider terminal never gives a NARROWER menu. That is the monotonicity the
		// old "two terminals must differ" check was reaching for, and it is the one
		// that still holds once the frame follows its content: above the width at which
		// the notes stop growing (the reading measure) two terminals legitimately
		// produce the same frame.
		if len(widths) == 2 {
			if (order[0] < order[1]) != (widths[0] < widths[1]) && widths[0] != widths[1] {
				t.Errorf("terminals %v produced frames %v: the wider terminal got the narrower menu", order, widths)
			}
		}
	}
	// and the editor's own repainted region: the band, the fence above the live line
	// and the fence under the band are one function drawn to one snapshot, so they
	// cannot disagree — and all three end in the same column as the frames above.
	for _, cols := range widthCases {
		screenTheme(t, themeDungeon, true, cols)
		page := houseWidth() + 1
		const typed = "› fix Sum in sum.go"
		fence, band := inputFence(houseWidth()), inputBand(houseWidth(), typed)
		// the band pads to the page and never cuts, so at the clamp floor — where the
		// operator's own line is wider than a 17-column page — it is the line's width
		wantBand := max(page, 1+visibleWidth(typed))
		if visibleWidth(fence) != page || visibleWidth(band) != wantBand {
			t.Errorf("the fence is %d columns and the band %d at a terminal of %d; the page is %d",
				visibleWidth(fence), visibleWidth(band), cols, page)
		}
		e := &LineEditor{}
		out := captureStdout(t, func() { e.submit("› ", []rune("fix Sum in sum.go")) })
		if !strings.Contains(out, band) || !strings.Contains(out, fence) {
			t.Errorf("submit drew something other than one band and one fence at a terminal of %d: %q", cols, out)
		}
	}
}

// ── what the width work claimed and did not do ──────────────────────────────

// firstLine takes the FIRST LINE and nothing else.
//
// It also cut at 60 bytes, which is two jobs in one name, and every width-aware
// cap downstream of it was dead code for anything longer: the plan's check, a
// failed step's detail, a /tasks title and the run summary were all cut to 60
// before the table or the truncate meant to decide their length ever saw them. So
// the headline claim — that the check goes in WHOLE — was false at every width,
// including in a pipe, where the promise is that every cell is printed whole.
func TestFirstLineBoundsALineAndNotALength(t *testing.T) {
	const check = "go test ./internal/orchestrator/... -run TestExitStatusReachesTheModel -count=1"
	if len(check) != 79 {
		t.Fatalf("the sample check is %d bytes, not the 79 this test is about", len(check))
	}
	if got := firstLine(check + "\nand a second line"); got != check {
		t.Errorf("firstLine cut a 79-byte line:\n want %q\n  got %q", check, got)
	}
	// and a multi-byte rune is never split: the old cut was a byte slice
	cjk := strings.Repeat("項目", 40)
	if got := firstLine(cjk); got != cjk || !utf8.ValidString(got) {
		t.Errorf("firstLine returned %d bytes of %d, valid=%v", len(got), len(cjk), utf8.ValidString(got))
	}

	// In a pipe the cell goes through byte-identical, at every width, because there
	// is no width to fit to at all.
	for _, cols := range []int{80, 120, 200, 400} {
		pipedTheme(t, cols)
		rows := [][]string{{"1", "step", "run", firstLine(check)}}
		out := strings.Join(tableLines([]string{"#", "step", "kind", "check"}, rows, "  ", fitWidth()), "\n")
		if !strings.Contains(out, check) {
			t.Errorf("a piped plan cut the check at COLUMNS=%d:\n%s", cols, out)
		}
	}
	// and on a screen wide enough to hold it, likewise
	screenTheme(t, themeDungeon, true, 200)
	rows := [][]string{{"1", "step", "run", firstLine(check)}}
	out := strings.Join(stripEach(tableLines([]string{"#", "step", "kind", "check"}, rows, "  ", fitWidth())), "\n")
	if !strings.Contains(out, check) {
		t.Errorf("a 200-column terminal cut a 79-column check:\n%s", out)
	}
}

// The allocator measures every cell in COLUMNS and then asks for a cut, so the cut
// has to be in columns too. ellipsize and ellipsizeMiddle counted runes, which for
// a wide-rune cell handed back up to twice the columns they were asked for — and
// because every column but the last is padded, the row does not merely overflow,
// it SHEARS: padTo sees a cell already past its promised width and pads nothing,
// so every column to the right slides on that one line and the reader can no
// longer tell which column a value belongs to.
func TestACutIsMeasuredInColumnsAndNotInRunes(t *testing.T) {
	const cjk = "box01:/home/u/项目文档目录/深层子目录/更深一层/工作区"
	screenTheme(t, themeDungeon, true, 80)
	for _, n := range []int{8, 12, 20, 24, 40} {
		if w := visibleWidth(ellipsize(cjk, n)); w > n {
			t.Errorf("ellipsize(%d) returned %d columns", n, w)
		}
		if w := visibleWidth(ellipsizeMiddle(cjk, n)); w > n {
			t.Errorf("ellipsizeMiddle(%d) returned %d columns", n, w)
		}
	}
	if got := ellipsize("abcdefgh", 40); got != "abcdefgh" {
		t.Errorf("ellipsize shortened something that fits: %q", got)
	}

	// and the grid stays a grid: the next column starts in the SAME screen column
	// on every row, with the wide-rune cell in a padded middle column
	header := []string{"member", "where", "sandbox", "roles", "status"}
	rows := [][]string{
		{"local", "/home/u/proj", "off", "lead, coder", "ok"},
		{"box01", cjk, "on", "reviewer", "ok"},
		{"box02", "box02:/srv/p", "on", "—", "unreachable"},
	}
	for _, cols := range []int{80, 100, 140} {
		screenTheme(t, themeDungeon, true, cols)
		ls := stripEach(tableLines(header, rows, "  ", fitWidth(), 1))
		budget := fitWidth()
		at := -1
		for i, l := range ls {
			if w := visibleWidth(l); w > budget {
				t.Errorf("a row with a wide-rune cell is %d columns against a budget of %d at a terminal of %d:\n%s",
					w, budget, cols, strings.Join(ls, "\n"))
			}
			// where does "sandbox" start? every row must agree
			j := strings.Index(l, "  on")
			if j < 0 {
				j = strings.Index(l, "  off")
			}
			if j < 0 {
				continue
			}
			c := visibleWidth(l[:j])
			if at < 0 {
				at = c
			}
			if c != at {
				t.Errorf("the sandbox column starts in column %d on row %d and %d elsewhere at a terminal of %d:\n%s",
					c, i, at, cols, strings.Join(ls, "\n"))
			}
		}
	}
}

// The status line sits INSIDE the region the line editor walks back over by
// counting rows, so "it must not be able to wrap" is an invariant the cursor
// arithmetic depends on. With a CJK project root the rune-counting ellipsize
// returned the path unchanged at 60 columns against a room of 43, the final clamp
// passed it through too, and the 92-column line wrapped: the "\033[1A" then landed
// on the status line instead of the input line and the next keystroke's "\r\033[J"
// erased from there, stranding the input line and redrawing the prompt one row
// lower on every press.
func TestStatusLineNeverWrapsWithWideRunes(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	h.sess.Loop = true
	h.sess.jail().Root = "/Users/u/文档/项目/服务端/工作目录"
	for _, cols := range []int{40, 60, 80, 100, 140, 200} {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themeDungeon, true, cols)
			line := r.statusLine()
			// the editor prints it at a two-column indent
			if w := visibleWidth(line) + 2; w > max(cols, houseWidth()+1) {
				t.Errorf("the status line is %d columns at a terminal of %d: %q", w, cols, stripANSI(line))
			}
		})
	}
}

// A redirected answer is the payload, and its bytes may not depend on the window
// the run happened in any more than they may carry an escape. The markdown rules
// are computed before the prose writer decides anything, so an ungated width in
// them wrote 77 dashes at COLUMNS=80 and 88 at COLUMNS=140 into the same file.
func TestARedirectedAnswerDoesNotDependOnTheWindow(t *testing.T) {
	const answer = "first paragraph\n\n---\n\n| model | window | note |\n| --- | --- | --- |\n" +
		"| qwen3-coder-480b-a35b-instruct | 262144 | served |\n| glm-5.3 | 131072 | falling back |\n\nlast paragraph"
	render := func(cols string) string {
		pipedTheme(t, 80)
		t.Setenv("COLUMNS", cols)
		return captureStdout(t, func() {
			pw := newProseWriter(false, false) // anim is false: this is a pipe
			pw.feed(answer)
			pw.end()
		})
	}
	base := render("80")
	for _, cols := range []string{"", "100", "140", "200", "400"} {
		if got := render(cols); got != base {
			t.Errorf("the same answer redirected at COLUMNS=%q differs from COLUMNS=80:\n want %q\n  got %q", cols, base, got)
		}
	}
	if strings.ContainsRune(base, 0x1b) {
		t.Errorf("a redirected answer carried an escape: %q", base)
	}
}

// The memory guard cuts a cell on a RUNE boundary and hands back the reset of a
// styled one. A byte slice made the emitted row invalid UTF-8 and dropped the
// cell's own cReset, so the faint attribute leaked into every line printed after
// the table until something else happened to reset it. And it only applies where
// there is a width to apply it for: with no terminal the promise is that every
// cell goes through unchanged.
func TestTheCellGuardCutsCleanlyAndOnlyWithAWidth(t *testing.T) {
	screenTheme(t, themeDungeon, true, 80)
	big := faint("%s", strings.Repeat("項目", 3000))
	got := capCell(big)
	if !utf8.ValidString(got) {
		t.Error("capCell produced invalid UTF-8")
	}
	if !strings.HasSuffix(got, cReset) {
		t.Errorf("capCell dropped the cell's reset: %q", got[max(0, len(got)-20):])
	}
	out := strings.Join(tableLines([]string{"step", "detail"}, [][]string{{"x", big}}, "  ", fitWidth(), 1), "\n")
	if !utf8.ValidString(out) {
		t.Error("a table row with an over-long styled cell is not valid UTF-8")
	}
	pipedTheme(t, 80)
	whole := strings.Repeat("a", cellBytes*2)
	out = strings.Join(tableLines([]string{"step", "detail"}, [][]string{{"x", whole}}, "  ", fitWidth(), 1), "\n")
	if !strings.Contains(out, whole) {
		t.Error("a piped table cut a cell although there was no width to fit to")
	}
}

// A marker line that wraps is the one thing that ruins the scrollback: every later
// tick's "\r\033[K" repaints only the last visual row and leaves a trail of
// half-erased flames behind it. The guard was a magic 12 and at the clamp floor
// the room is exactly 12, so it did not fire.
func TestTheMarkerNeverExceedsItsTerminal(t *testing.T) {
	for _, cols := range widthCases {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themeDungeon, true, cols)
			pw := newProseWriter(false, false)
			pw.anim = true
			pw.markerPrefix = " " + cDim + gTorch + cReset + " "
			pw.markerLabel = "waiting for meta-llama/Llama-3.1-405B-Instruct-FP8"
			line := pw.markerPrefix + pw.markerText(gSpinner[0], pw.markerLabel)
			if w := visibleWidth(line); w > max(cols, houseWidth()+1) {
				t.Errorf("the marker is %d columns at a terminal of %d: %q", w, cols, stripANSI(line))
			}
		})
	}
}

// The one place chrome was printed AFTER the payload, so the payload could never
// be the thing that fit. The command itself is what you would paste into a shell
// and is never cut; the hint is droppable and drops.
func TestTheRunHeadlineKeepsItsCommandAndDropsItsHint(t *testing.T) {
	const cmdline = "go test ./internal/orchestrator/... -run TestExitStatusReachesTheModel -count=1 -race"
	for _, cols := range []int{80, 100, 140, 200} {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themeDungeon, true, cols)
			line := runHeadline(cmdline)
			if !strings.Contains(stripANSI(line), cmdline) {
				t.Errorf("the headline cut the command at a terminal of %d: %q", cols, stripANSI(line))
			}
			// the command is the payload and is never cut, so the bound is the page OR
			// the bare command line when even that does not fit — and never more
			bare := visibleWidth(runHeadline("x")) - 1 + visibleWidth(cmdline)
			if w := visibleWidth(line); w > max(houseWidth()+1, bare) {
				t.Errorf("the headline is %d columns at a terminal of %d: %q", w, cols, stripANSI(line))
			}
			if hinted := strings.Contains(stripANSI(line), "Ctrl-C"); hinted != (visibleWidth(line) <= houseWidth()+1) {
				t.Errorf("the hint is %v at a terminal of %d, where the bare command line measures %d against a page of %d",
					hinted, cols, bare, houseWidth()+1)
			}
		})
	}
}

// Every one-sentence follow-up line — a hint, an ok/warn/err line, a tool error's
// hints, the note after answering "a" at a door — wraps to the reading measure and
// indents its continuations under itself. hint() used to be the one prose surface
// with no measure at all: doctor's longest is 169 columns, which broke to column 1
// at 80 and was the widest line on the whole screen at 200 and 400.
//
// A line whose payload is a single unbreakable token is the exception, because
// wrapping it puts the token alone on a line that is still too long and splits it
// away from the sentence that introduced it.
func TestASentenceLineWrapsButAPasteDoesNot(t *testing.T) {
	const long = "add a profile in models.go, or set models.llama-4.2-scout-17b-16e-instruct-fp8: " +
		"{temperature, top_p, effort, reasoning_replay} in roles.yaml from the model card"
	_, trust, note := doorAnswer("a", false)
	if !trust || note == "" {
		t.Fatal("answering \"a\" at a read door must grant the session and say so")
	}
	for _, cols := range widthCases {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themeDungeon, true, cols)
			// Below a terminal of about 40 the reading measure is narrower than wrapTo's
			// own 20-column floor, so a sentence is printed whole and the terminal soft
			// wraps it — the same call the picker's legend makes, for the same reason:
			// documentation that has been cut has not been written.
			measured := min(houseWidth()-3, proseMax) > 24
			out := captureStdout(t, func() {
				hint("%s", long)
				warnLine("%s", long)
				errLine("%s", long)
			})
			for _, l := range strings.Split(strings.Trim(out, "\n"), "\n") {
				// an unbreakable token in the middle of the sentence overflows its own
				// row, whole: that is the one thing wider than the page here
				if w := visibleWidth(l); measured && w > max(80, houseWidth()+1) && !strings.Contains(l, "instruct-fp8:") {
					t.Errorf("a hint is %d columns at a terminal of %d: %q", w, cols, stripANSI(l))
				}
			}
			// the door's note is printed through the same wrap
			for _, l := range wrapHint(note, min(houseWidth()-4, proseMax)) {
				if w := visibleWidth(l) + 3; measured && w > max(80, houseWidth()+1) {
					t.Errorf("the trust-all note is %d columns at a terminal of %d: %q", w, cols, l)
				}
			}
			// and a sentence whose value is one unbreakable token is not split away
			// from it
			const path = "/var/folders/gj/6_3zn_0d6/T/TestMCPLockAndRefresh2864069200/002/.lca/mcp.lock.json"
			got := captureStdout(t, func() { okLine("wrote %s", path) })
			if !strings.Contains(stripANSI(got), "wrote "+path) {
				t.Errorf("a paste was split at a terminal of %d: %q", cols, stripANSI(got))
			}
		})
	}
	// and it does wrap where it can: at 80 the 160-column hint is more than one row
	screenTheme(t, themeDungeon, true, 80)
	if n := len(strings.Split(strings.Trim(captureStdout(t, func() { hint("%s", long) }), "\n"), "\n")); n < 2 {
		t.Errorf("a %d-column hint printed as %d row(s) at a terminal of 80", visibleWidth(long), n)
	}
}

// NO_COLOR and TERM=dumb on a real terminal are the sober tier, not a pipe: no
// decoration and a perfectly good width. Deciding the fit budget from
// activeTheme.Frames handed that tier none of the width work — its tables were not
// fitted and its label/value rows were not wrapped, so the plan row the dungeon
// tier fits to a bounded width came out at 182 columns and the terminal's own soft
// wrap dropped the remainder into column 1, destroying the column alignment that
// is the whole point of a table.
func TestTheSoberTierGetsTheWidthWork(t *testing.T) {
	tb := wideTables[0]
	for _, cols := range []int{80, 100, 140} {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themePlain, true, cols)
			if activeTheme.Frames {
				t.Fatal("the sober tier draws no frames")
			}
			if !activeTheme.Screen {
				t.Fatal("the sober tier is on a terminal and has a width")
			}
			plain, _ := tableBlock(tb.header, tb.rows, tb.mid...)
			screenTheme(t, themeDungeon, true, cols)
			dungeon, _ := tableBlock(tb.header, tb.rows, tb.mid...)
			if strings.Join(stripEach(plain), "\n") != strings.Join(stripEach(dungeon), "\n") {
				t.Errorf("the sober tier and the dungeon tier fitted the plan differently at a terminal of %d:\nsober:\n%s\ndungeon:\n%s",
					cols, strings.Join(stripEach(plain), "\n"), strings.Join(stripEach(dungeon), "\n"))
			}
			// and a long label/value row wraps in both tiers rather than running off
			const long = "the model was asked for native tool calls and answered with a text block, so the call was reparsed from prose"
			screenTheme(t, themePlain, true, cols)
			for _, l := range strings.Split(strings.Trim(captureStdout(t, func() { row("thinking", long) }), "\n"), "\n") {
				if w := visibleWidth(l); w > houseWidth()+1 {
					t.Errorf("a sober-tier row is %d columns at a terminal of %d: %q", w, cols, l)
				}
			}
		})
	}
	// and a real pipe still gets neither
	pipedTheme(t, 80)
	out := strings.Join(tableLines(tb.header, tb.rows, "  ", fitWidth(), tb.mid...), "\n")
	if strings.Contains(out, gEllipsis) {
		t.Errorf("a piped plan cut a cell:\n%s", out)
	}
}

// The frames and the lintels are drawn to what is IN them. Grown to the page
// instead, a 200-column screen showed THE HOLD as a 161-column box whose longest
// row ended in column 62 and `doctor` as four 161-column rules over lines of 12 to
// 52 columns: text squeezed left inside a box that measures the emptiness, with
// the rule pointing at it. The session's own shell — the input fence and the
// submitted band — still takes the whole page, because the line it frames is the
// operator's own and has no content width to be sized to.
func TestFramesAreDrawnToTheirContents(t *testing.T) {
	for _, cols := range []int{80, 140, 200, 400} {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themeDungeon, true, cols)
			hold := newPanel("the hold", "where you stand")
			hold.Row("project", "cli-agents  ~/work/")
			hold.Row("gateway", "gw.lan:8080  ● UP · 6 models")
			ls := hold.Lines()
			frame := visibleWidth(ls[0])
			content := 0
			for _, l := range ls[1 : len(ls)-1] {
				content = max(content, visibleWidth(strings.TrimRight(stripANSI(l), " ║|")))
			}
			// a frame is at most four columns of stone and gutter past its widest row,
			// or the floor, whichever is more
			floor, _ := panelSpan()
			if want := max(content+2, floor+1); frame > want {
				t.Errorf("the frame is %d columns around %d columns of content at a terminal of %d:\n%s",
					frame, content, cols, strings.Join(stripEach(ls), "\n"))
			}
			// the shell keeps the page
			if w := visibleWidth(inputFence(houseWidth())); w != houseWidth()+1 {
				t.Errorf("the input fence is %d columns; the page is %d", w, houseWidth()+1)
			}
			// and an unmeasured lintel is the floor, never the page
			lintel := stripANSI(strings.Trim(captureStdout(t, func() { section("gateway", "gw.lan:8080") }), "\n"))
			if w := visibleWidth(lintel); w != min(panelFloor, houseWidth())+1 {
				t.Errorf("an unmeasured lintel is %d columns at a terminal of %d; the floor is %d",
					w, cols, min(panelFloor, houseWidth())+1)
			}
		})
	}
	// A lintel over a MEASURED block is that block's width, so the rule and the
	// table it heads end in the same column.
	for _, cols := range []int{100, 140, 200} {
		screenTheme(t, themeDungeon, true, cols)
		tb := wideTables[0]
		_, w := tableBlock(tb.header, tb.rows, tb.mid...)
		out := strings.Split(strings.Trim(stripANSI(captureStdout(t, func() {
			sectionTable(tb.name, "nightly", tb.header, tb.rows, tb.mid...)
		})), "\n"), "\n")
		want := min(max(w, min(panelFloor, houseWidth())+1), max(fitWidth(), houseWidth()+1))
		if got := visibleWidth(out[0]); got != want {
			t.Errorf("the lintel is %d columns over a %d-column table at a terminal of %d; want %d", got, w, cols, want)
		}
	}
}

// Under LC_ALL=C the tier spells its ellipsis "..." and measures it as three
// columns, so nothing it cuts may carry a U+2026 — the row would then hold the
// tier's own marker in one place and a UTF-8 ellipsis in another, in the locale
// that was chosen because it cannot carry one. firstLine used to append a literal
// "…" to every cell over 60 bytes, so the plan's check did exactly that.
func TestTheASCIITierCutsInASCII(t *testing.T) {
	for _, cols := range []int{80, 100} {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themeDungeon, false, cols)
			out := captureStdout(t, func() { drawTables() })
			if strings.ContainsRune(out, '…') {
				for _, l := range strings.Split(out, "\n") {
					if strings.ContainsRune(l, '…') {
						t.Errorf("the ASCII tier emitted a UTF-8 ellipsis: %q", stripANSI(l))
					}
				}
			}
			if !strings.Contains(out, "...") && cols == 100 {
				t.Error("nothing was cut at a terminal of 100, so this asserts nothing")
			}
		})
	}
}

// A lintel gives its DETAIL up before it gives up its stone, and the retry that
// shortens the detail has to land: it re-measures with visibleWidth, so a detail
// of wide runes used to leave the fill below two courses and the rule ragged.
func TestALintelGivesUpItsDetailAndStaysSquare(t *testing.T) {
	for _, cols := range widthCases {
		for _, detail := range []string{
			"nightly",
			"/private/tmp/claude-501/widelab/.lca/workflows/harden-the-workflow-parser.yaml",
			"项目文档目录/深层子目录/更深一层/工作区/配置文件.yaml",
		} {
			screenTheme(t, themeDungeon, true, cols)
			got := stripANSI(strings.Trim(captureStdout(t, func() { section("plan", detail) }), "\n"))
			want := min(panelFloor, houseWidth()) + 1
			if w := visibleWidth(got); w != want {
				t.Errorf("a lintel is %d columns at a terminal of %d, want %d:\n%q", w, cols, want, got)
			}
		}
	}
}

// A pipe's bytes may not depend on the window. The answer is the case that was
// caught — the markdown rules were computed before the prose writer decided
// anything — but a lintel and a shortened path had the same shape, so the whole
// piped screen is asserted rather than the one line.
func TestPipedBytesDoNotDependOnTheWindow(t *testing.T) {
	render := func(cols string) string {
		pipedTheme(t, 80)
		t.Setenv("COLUMNS", cols)
		return captureStdout(t, func() {
			drawTables()
			section("gateway", "gw.lan:8080")
			row("trace", prettyPath("/var/log/lca/traces/20260930-022035-2339.jsonl", ""))
			okLine("wrote %s", "/private/tmp/claude-501/widelab/.lca/mcp.lock.json")
			hint("per-task results: %s", "/private/tmp/claude-501/widelab/.lca/evals/20260930")
		})
	}
	base := render("80")
	for _, cols := range []string{"", "24", "100", "140", "200", "400"} {
		if got := render(cols); got != base {
			t.Errorf("a piped screen at COLUMNS=%q differs from COLUMNS=80:\n want %q\n  got %q", cols, base, got)
		}
	}
}

// The context fill is the only field on the status line that MOVES, and it was
// the first one given up: measured at 80 columns with a team's own model id
// (`qwen3-coder-480b-a35b-instruct` as the role's model), the gauge vanished
// while 25 columns of a path the operator already knows stayed — which is how
// "multi-agent mode does not show how full the context is" happens. The ladder
// gives up the bar, then the path's comfort, then the path entirely, and the
// percentage last.
func TestTheContextFillSurvivesANarrowStatusLine(t *testing.T) {
	fs := newFakeServer(t, func(fakeRequest, int) fakeReply { return fakeReply{content: "ok"} })
	fs.models = allModels()
	r, h := replFor(t, fs, testRoles)
	h.sess.client.SetCtxLen(262144)                        // a window the server reported, so the gauge draws
	h.sess.client.model = "qwen3-coder-480b-a35b-instruct" // a real served id, 30 columns of it
	h.sess.jail().Root = "/Users/u/work/some/deep/project/root"

	for _, cols := range []int{80, 90, 100, 140} {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			screenTheme(t, themeDungeon, true, cols)
			line := stripANSI(r.statusLine())
			if !strings.Contains(line, "CTX") || !strings.Contains(line, "%") {
				t.Fatalf("the fill must survive at %d columns: %q", cols, line)
			}
			// And the no-wrap invariant the editor's cursor arithmetic depends on
			// still holds, which is what made the gauge droppable in the first place.
			if w := visibleWidth(r.statusLine()) + 2; w > max(cols, houseWidth()+1) {
				t.Fatalf("the status line is %d columns at a terminal of %d: %q", w, cols, line)
			}
		})
	}
}
