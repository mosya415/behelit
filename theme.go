package main

import (
	"os"
	"strings"
)

// The one owner of colour and glyphs.
//
// ui.go's three rules still hold — monochrome plus one accent, uppercase only
// our own chrome, separation is a hairline — but until now the palette was a
// const block, so nothing could be swapped at runtime and nothing asked whether
// the terminal could carry it. The result was escape sequences in piped output
// and box-drawing runes on a latin-1 locale, with no way to say no.
//
// The seam is deliberately dull: the names ui.go always used (cFaint, gUp, …)
// stay exactly where the other 22 files expect them, but they are vars that a
// Theme sets. Every existing call site keeps compiling and starts obeying the
// theme by construction, which is why introducing this owner does not touch a
// single screen. Step one changes nothing on screen on purpose: if a look had
// changed here, nobody could tell a wiring mistake from a design decision.
//
// TORCHLIT's palette words map onto the fields below, so the next step retunes
// values in one place instead of renaming 127 references:
//
//	BONE   the terminal's own foreground (never set; data is printed verbatim)
//	MORTAR Faint    STONE  Dim      TORCH  Yellow
//	MOSS   Green    RUST   Red      BLOOD  Blood / BloodDark

// theme names, as the `theme` setting and /theme spell them.
const (
	themeDungeon = "dungeon"
	themePlain   = "plain"
	themeAuto    = "auto" // the absence of a preference: detection decides
)

// colourDepth is what the terminal admits to carrying. It is resolved here and
// not at the point of use because the banner and the prompt band emit truecolor
// unconditionally today, and a 16-colour terminal renders those as a guess.
type colourDepth uint8

const (
	depthNone colourDepth = iota
	depth16
	depth256
	depthTruecolor
)

func (d colourDepth) String() string {
	switch d {
	case depth16:
		return "16 colours"
	case depth256:
		return "256 colours"
	case depthTruecolor:
		return "truecolor"
	}
	return "no colour"
}

// Palette is every escape the UI is allowed to emit. Plain sets all of them to
// "" — the call sites then concatenate empty strings and print clean text, with
// no branch anywhere and no second code path to rot.
//
// Bold/BoldOff and Italic/ItalicOff are four fields and not two because
// markdown.go closes a span with the *off* code rather than a full reset, so
// emphasis inside a coloured line keeps its colour. Collapsing them onto Reset
// would silently kill the surrounding style.
type Palette struct {
	Reset     string
	Bold      string
	BoldOff   string
	Italic    string
	ItalicOff string
	Dim       string
	Faint     string
	Green     string
	Yellow    string
	Red       string
	Code      string
	FgOff     string // reset foreground only, so it composes inside other styles
	BandBg    string
	Blood     string
	BloodDark string

	// Ember is the hotter end of the torch, for the tall frames of the waiting
	// flame. It is not a status colour and nothing states anything in it: a
	// flame that is briefly orange rather than gold says only "still burning".
	Ember string
}

// Glyphs is every rune the UI draws as chrome. Data — paths, model ids,
// commands, diff signs, file contents — is never in here: it is printed
// verbatim, which is rule 2 and the reason a fallback tier can be this blunt.
type Glyphs struct {
	// status, shared by every screen
	Up      string // ready / up / ok
	Partial string // unknown / partial / running
	Down    string // down / failed
	None    string // none / muted

	// structure
	Rule     string // hairlines: section rules, the prompt band, markdown rules
	VBar     string // containment: the command-output and subagent gutters
	FrameTop string // a subagent opens
	FrameBot string // a subagent closes
	Collapse string // skipped diff context

	// pointing and prose
	Cursor   string // here / now: the picker cursor, a todo in progress, a step
	Pending  string // a todo not started
	Bullet   string // a markdown list item
	Quote    string // a markdown block quote
	Hint     string // what follows / what to do next
	Prompt   string // your turn to type
	Flow     string // selection / a model chain
	Ellipsis string // truncation
	Warning  string // the unsafe banner

	// keys, named in legends
	Back  string
	Enter string

	// Sep is the field separator every status line, perf line, legend and panel
	// row joins with, and Dash is the rule between a chrome word and its gloss.
	// They are here and not literals because they are the most frequent chrome on
	// the screen: "·" is one rune that appears more often than all the map glyphs
	// together, and a tier that maps twenty runes and skips that one is not the
	// ASCII tier it claims to be.
	Sep  string
	Dash string

	// Nil is the mark in a cell that has no value. It is chrome, so it belongs to
	// the tier: under LC_ALL=C an em dash was the one rune left drawing itself as a
	// question mark in a table that was otherwise honest ASCII.
	Nil string

	// the waiting ring. Index 0 is the resting frame; the ticker starts at 1.
	Spinner []string

	// Mask is what a secret field echoes per rune. It must never be the key and
	// must never be empty, so the operator can see the field taking input.
	Mask string

	// ── the map gutter ──────────────────────────────────────────────────────
	// One column at the left edge of every transcript line, so scrollback reads
	// as a map of the session rather than as a wall. These are class marks, not
	// status: what KIND of thing happened. The outcome still gets its own word
	// and its own Up/Partial/Down marker on the line below.
	//
	// Nine of them also occur inside data — $ in a shell command, / in a path,
	// . in a filename — so they are gutter glyphs only because of the column
	// they sit in. That makes the column load-bearing, which is why there is a
	// test for it and not just a convention.
	Me     string // @ you: the prompt line
	Torch  string // ~ the torch: waiting, thinking, the model is alive
	Probe  string // . a look at the world that does not change it
	Carve  string // / a change to the world
	Cmd    string // ^ a command, which is a trap you set yourself
	Cost   string // $ what the turn spent
	Prose  string // ▌ the model's own words
	Deeper string // > down into a side passage (a subagent)
	Out    string // < and back out of it again
	Loot   string // * what came back from one
	Gate   string // = the verifier, which decides

	// ── gauges ──────────────────────────────────────────────────────────────
	// A bar is never the whole statement: its number is printed beside it and
	// its denominator is named. Open/Close bracket the bar so its extent is
	// readable with no colour at all, and are empty where the block runes
	// already show it.
	GaugeFull  string
	GaugeHalf  string
	GaugeEmpty string
	GaugeBad   string // the fraction that failed, inside the same bar
	GaugeOpen  string
	GaugeClose string

	// ── panels ──────────────────────────────────────────────────────────────
	// A panel is a closed frame with its title in the top rule. It is used only
	// where the content's length is known before it is printed; a subagent's
	// output is not, which is why childView keeps a spine and never a box.
	PanelTL string
	PanelTR string
	PanelBL string
	PanelBR string
	PanelH  string
	PanelV  string
	PanelML string // ╠ a division inside a panel
	PanelMR string
}

// Theme is a palette, a glyph set, and the record of why they were chosen — the
// last part so /theme and /config can answer "why is this screen grey?" without
// the operator guessing at env vars.
type Theme struct {
	Name    string
	Why     string
	Colour  bool
	Depth   colourDepth
	Unicode bool

	// Frames is whether to draw decoration at all: the panels, the block-rune
	// gauges and the run's map. It is false for a pipe or a file — and false for
	// the plain theme too, because "plain" is the word an operator reaches for when
	// they want the sober screen, not a monochrome dungeon: colourless boxes are
	// still boxes. Colour is a separate question. An escape in a redirected payload is the obvious bug, but a frame
	// around somebody's output, a block-rune gauge in it or a corridor of rooms
	// drawn across it are decoration too, and the brief forbids NEW decoration in
	// a machine path as firmly as it forbids escapes. So the panels print their
	// rows bare, the gauges draw nothing and the run's map is not drawn at all
	// when this is false — every one of them inherits the rule from here instead
	// of each caller remembering it.
	Frames bool
	Palette
	Glyphs
}

// activeTheme and the legacy names are initialised as var declarations rather
// than in an init(): Go orders var initialisation by dependency, so any other
// package-level var that reaches a colour gets the theme's value and not "".
var activeTheme = dungeonTheme(termCaps{tty: true, colour: true, depth: depthTruecolor, unicode: true}, "the default")

// The names ui.go defined as consts. Every one of the 127 references in the
// other 22 files still reads these.
var (
	cReset     = activeTheme.Reset
	cBold      = activeTheme.Bold
	cBoldOff   = activeTheme.BoldOff
	cItalic    = activeTheme.Italic
	cItalicOff = activeTheme.ItalicOff
	cDim       = activeTheme.Dim
	cFaint     = activeTheme.Faint
	cGreen     = activeTheme.Green
	cYellow    = activeTheme.Yellow
	cRed       = activeTheme.Red
	cCode      = activeTheme.Code
	cFgOff     = activeTheme.FgOff
	cBandBg    = activeTheme.BandBg
	cBlood     = activeTheme.Blood
	cBloodDark = activeTheme.BloodDark
	cEmber     = activeTheme.Ember

	gUp       = activeTheme.Up
	gPartial  = activeTheme.Partial
	gDown     = activeTheme.Down
	gNone     = activeTheme.None
	gRule     = activeTheme.Rule
	gVBar     = activeTheme.VBar
	gFrameTop = activeTheme.FrameTop
	gFrameBot = activeTheme.FrameBot
	gCollapse = activeTheme.Collapse
	gCursor   = activeTheme.Cursor
	gPending  = activeTheme.Pending
	gBullet   = activeTheme.Bullet
	gQuote    = activeTheme.Quote
	gHint     = activeTheme.Hint
	gPrompt   = activeTheme.Prompt
	gFlow     = activeTheme.Flow
	gEllipsis = activeTheme.Ellipsis
	gWarning  = activeTheme.Warning
	gBack     = activeTheme.Back
	gEnter    = activeTheme.Enter
	gNil      = activeTheme.Nil
	gSep      = activeTheme.Sep
	gDash     = activeTheme.Dash
	gMask     = activeTheme.Mask
	gSpinner  = activeTheme.Spinner

	gMe     = activeTheme.Me
	gTorch  = activeTheme.Torch
	gProbe  = activeTheme.Probe
	gCarve  = activeTheme.Carve
	gCmd    = activeTheme.Cmd
	gCost   = activeTheme.Cost
	gProse  = activeTheme.Prose
	gDeeper = activeTheme.Deeper
	gOut    = activeTheme.Out
	gLoot   = activeTheme.Loot
	gGate   = activeTheme.Gate

	gaFull  = activeTheme.GaugeFull
	gaHalf  = activeTheme.GaugeHalf
	gaEmpty = activeTheme.GaugeEmpty
	gaBad   = activeTheme.GaugeBad
	gaOpen  = activeTheme.GaugeOpen
	gaClose = activeTheme.GaugeClose

	gPanelTL = activeTheme.PanelTL
	gPanelTR = activeTheme.PanelTR
	gPanelBL = activeTheme.PanelBL
	gPanelBR = activeTheme.PanelBR
	gPanelH  = activeTheme.PanelH
	gPanelV  = activeTheme.PanelV
	gPanelML = activeTheme.PanelML
	gPanelMR = activeTheme.PanelMR
)

// setTheme makes t the theme every screen draws with. It is the only place the
// legacy names are assigned.
func setTheme(t Theme) {
	activeTheme = t
	cReset, cBold, cBoldOff, cItalic, cItalicOff = t.Reset, t.Bold, t.BoldOff, t.Italic, t.ItalicOff
	cDim, cFaint = t.Dim, t.Faint
	cGreen, cYellow, cRed = t.Green, t.Yellow, t.Red
	cCode, cFgOff, cBandBg = t.Code, t.FgOff, t.BandBg
	cBlood, cBloodDark, cEmber = t.Blood, t.BloodDark, t.Ember

	gUp, gPartial, gDown, gNone = t.Up, t.Partial, t.Down, t.None
	gRule, gVBar, gFrameTop, gFrameBot, gCollapse = t.Rule, t.VBar, t.FrameTop, t.FrameBot, t.Collapse
	gCursor, gPending, gBullet, gQuote = t.Cursor, t.Pending, t.Bullet, t.Quote
	gHint, gPrompt, gFlow, gEllipsis, gWarning = t.Hint, t.Prompt, t.Flow, t.Ellipsis, t.Warning
	gBack, gEnter, gMask, gSpinner = t.Back, t.Enter, t.Mask, t.Spinner
	gSep, gDash, gNil = t.Sep, t.Dash, t.Nil

	gMe, gTorch, gProbe, gCarve, gCmd, gCost = t.Me, t.Torch, t.Probe, t.Carve, t.Cmd, t.Cost
	gProse, gDeeper, gOut, gLoot, gGate = t.Prose, t.Deeper, t.Out, t.Loot, t.Gate

	gaFull, gaHalf, gaEmpty = t.GaugeFull, t.GaugeHalf, t.GaugeEmpty
	gaBad, gaOpen, gaClose = t.GaugeBad, t.GaugeOpen, t.GaugeClose

	gPanelTL, gPanelTR, gPanelBL, gPanelBR = t.PanelTL, t.PanelTR, t.PanelBL, t.PanelBR
	gPanelH, gPanelV, gPanelML, gPanelMR = t.PanelH, t.PanelV, t.PanelML, t.PanelMR
}

// ── the palettes ────────────────────────────────────────────────────────────

// dungeonPalette is TORCHLIT: a torch in a stone corridor. Warm greys for the
// masonry, one amber light source, and status kept in the three tones the whole
// program has always used, so nothing that means something changed hue.
//
//	MORTAR  Faint      the cold grey between the stones: labels, rules, gauge track
//	STONE   Dim        the stone itself: our own chrome, frames, gutter classes
//	TORCH   Yellow     the one light source: a door, a budget, a title
//	EMBER   Ember      the hot end of the flame — decoration only, never a status
//	MOSS    Green      what grew because it was left alone: ok, up, passed, added
//	RUST    Red        what corroded: down, failed, removed
//	BLOOD   Blood      the brand crimson of the banner and of "unsafe"
//	BONE    (no field) the terminal's own foreground: data, printed verbatim
//
// A terminal that carries only the sixteen ANSI colours gets sixteenPalette()
// below instead, because these 256-colour and truecolor escapes are not
// approximated there — they are ignored, and both greys collapse onto the default
// foreground, which erases the chrome-is-grey / data-is-bone distinction the whole
// palette rests on. Depth was already detected and already reported by /theme;
// this is the tier acting on it, so the answer /theme gives is true.
func dungeonPalette(d colourDepth) Palette {
	if d <= depth16 {
		return sixteenPalette()
	}
	return Palette{
		Reset:     "\033[0m",
		Bold:      "\033[1m",
		BoldOff:   "\033[22m",
		Italic:    "\033[3m",
		ItalicOff: "\033[23m",
		Dim:       "\033[38;5;247m", // STONE  #9e9e9e — lit masonry
		Faint:     "\033[38;5;241m", // MORTAR #626262 — the joints between it
		Green:     "\033[38;5;108m", // MOSS   #87af87
		Yellow:    "\033[38;5;179m", // TORCH  #e3c873
		Red:       "\033[38;5;167m", // RUST   #d8635b
		Code:      "\033[38;5;73m",  // inline code / code blocks — muted teal, not a box
		FgOff:     "\033[39m",       // reset foreground only
		Ember:     "\033[38;5;173m", // EMBER  #d7875f — the flame's hot frames
		// The band behind a submitted prompt is the one fill in the program, and
		// it is now warm stone rather than grey-green: it is the wall the torch is
		// mounted on, and it is the only "this is where I spoke" anchor there is
		// when scrolling back through a long session.
		BandBg:    "\033[48;2;48;40;32m",
		Blood:     "\033[38;2;186;33;38m",
		BloodDark: "\033[38;2;92;14;16m",
	}
}

// sixteenPalette is TORCHLIT in the sixteen colours every terminal has had since
// 1979. Nine values, and no call site changes — which is the whole point of the
// seam.
//
// The two greys are the load-bearing part: STONE takes white (37) and MORTAR the
// bright black (90), so our own chrome still recedes from the data printed in the
// terminal's own foreground. EMBER cannot be distinguished from TORCH here and is
// deliberately the same yellow: it is decoration and states nothing, so losing it
// costs nothing. BandBg is empty rather than an approximated background, because a
// wrong fill behind a whole line is worse than no fill, and the banner's crimson
// becomes plain red — which is what makes the block art legible instead of a slab
// of unresolved truecolor.
func sixteenPalette() Palette {
	return Palette{
		Reset:     "\033[0m",
		Bold:      "\033[1m",
		BoldOff:   "\033[22m",
		Italic:    "\033[3m",
		ItalicOff: "\033[23m",
		Dim:       "\033[37m", // STONE
		Faint:     "\033[90m", // MORTAR
		Green:     "\033[32m", // MOSS
		Yellow:    "\033[33m", // TORCH
		Red:       "\033[31m", // RUST
		Code:      "\033[36m",
		FgOff:     "\033[39m",
		Ember:     "\033[33m", // no hotter yellow to be had: see above
		BandBg:    "",
		Blood:     "\033[31m",
		BloodDark: "\033[90m",
	}
}

// ── the glyph tiers ─────────────────────────────────────────────────────────

// glyphSet picks the tier. Colour and glyph capability are different questions:
// a UTF-8 xterm with NO_COLOR keeps every rune and loses every escape, and a
// colour-capable terminal on LC_ALL=C is the other way round.
func glyphSet(unicode bool) Glyphs {
	if unicode {
		return glyphsUTF8()
	}
	return glyphsASCII()
}

// glyphsUTF8 is TORCHLIT's alphabet. The map gutter is deliberately the ASCII
// half of it — a roguelike map always was 7-bit — so the fallback tier below
// loses only the frames, the gauges and the flame, and never a class mark.
//
// The retired runes and why: ✓ and ✗ are gone because the gate now states PASS
// or FAIL in words, which is the thing an operator reads anyway, and because
// runeWidth() charged them two columns while every terminal draws them at one,
// so every table cell holding one was already a column short. ✕ is gone for the
// same width reason and because % — a corpse on the map — is the one failure
// mark on every screen now, instead of two runes for one idea.
func glyphsUTF8() Glyphs {
	return Glyphs{
		Up: "●", Partial: "◐", Down: "%", None: "·",
		Rule: "─", VBar: "│", FrameTop: "┌", FrameBot: "└", Collapse: "⋮",
		Cursor: "▸", Pending: "○", Bullet: "•", Quote: "▏",
		Hint: "↳", Prompt: "›", Flow: "→", Ellipsis: "…", Warning: "⚠",
		Back: "⌫", Enter: "⏎",
		Sep: " · ", Dash: " — ", Nil: "—",
		// the torch, rising and falling on the 90 ms ticker. [0] is the resting
		// frame the line is first drawn with; the ticker starts at 1.
		Spinner: []string{"▁", "▂", "▃", "▄", "▅", "▆", "▅", "▄", "▃", "▂"},
		Mask:    "•",

		Me: "@", Torch: "~", Probe: ".", Carve: "/", Cmd: "^", Cost: "$",
		Prose: "▌", Deeper: ">", Out: "<", Loot: "*", Gate: "=",

		// Half-block gauges: the half cell is what lets a bar be honest at 13
		// cells instead of rounding a fifth of a window away.
		GaugeFull: "█", GaugeHalf: "▌", GaugeEmpty: "░", GaugeBad: "▒",
		GaugeOpen: "", GaugeClose: "",

		PanelTL: "╔", PanelTR: "╗", PanelBL: "╚", PanelBR: "╝",
		PanelH: "═", PanelV: "║", PanelML: "╠", PanelMR: "╣",
	}
}

// glyphsASCII is the same alphabet in 7 bits, for LC_ALL=C and any locale that
// does not say UTF-8. Each substitute keeps the distinction its rune carried —
// ok is not warning is not failed — because the word beside it is the meaning
// and the glyph only has to stay distinguishable.
//
// The gauge gains brackets here: with no block runes and possibly no colour, [
// and ] are the only thing left that says where the bar ends and the track
// begins, and a bar whose extent is unreadable states nothing at all.
func glyphsASCII() Glyphs {
	return Glyphs{
		Up: "*", Partial: "!", Down: "%", None: "-",
		Rule: "-", VBar: "|", FrameTop: "+", FrameBot: "+", Collapse: ":",
		Cursor: ">", Pending: "o", Bullet: "*", Quote: "|",
		Hint: "->", Prompt: ">", Flow: "->", Ellipsis: "...", Warning: "!",
		Back: "bksp", Enter: "enter",
		Sep: " . ", Dash: " - ", Nil: "-",
		Spinner: []string{".", "o", "O", "o"},
		Mask:    "*",

		Me: "@", Torch: "~", Probe: ".", Carve: "/", Cmd: "^", Cost: "$",
		Prose: "|", Deeper: ">", Out: "<", Loot: "*", Gate: "=",

		GaugeFull: "#", GaugeHalf: "=", GaugeEmpty: "-", GaugeBad: "!",
		GaugeOpen: "[", GaugeClose: "]",

		PanelTL: "+", PanelTR: "+", PanelBL: "+", PanelBR: "+",
		PanelH: "-", PanelV: "|", PanelML: "+", PanelMR: "+",
	}
}

// ── the two themes ──────────────────────────────────────────────────────────

func dungeonTheme(c termCaps, why string) Theme {
	return Theme{
		Name: themeDungeon, Why: why,
		Colour: true, Depth: c.depth, Unicode: c.unicode, Frames: c.tty,
		Palette: dungeonPalette(c.depth),
		Glyphs:  glyphSet(c.unicode),
	}
}

func plainTheme(c termCaps, why string) Theme {
	return Theme{
		Name: themePlain, Why: why,
		Colour: false, Depth: depthNone, Unicode: c.unicode, Frames: false,
		Palette: Palette{}, // every escape empty: one code path, no branches
		Glyphs:  glyphSet(c.unicode),
	}
}

// ── detection ───────────────────────────────────────────────────────────────

// termCaps is what the terminal can carry. It is read from the environment once
// so the decision below can be a pure function of it and therefore testable
// without a terminal.
type termCaps struct {
	tty     bool
	colour  bool
	depth   colourDepth
	unicode bool
	noWhy   string // when colour is false: why, in words, for /theme
}

// detectCaps asks the four questions, in the order that a "no" outranks a
// preference. osTermWidth() ioctls stdout, which is exactly the right file: the
// screen is stdout, and a run whose stdout is a pipe has no screen.
func detectCaps() termCaps {
	c := termCaps{
		tty:     osTermWidth() > 0,
		colour:  true,
		depth:   depth16,
		unicode: utf8Locale(),
	}
	// NO_COLOR is presence, not value: setting it at all means no colour, so
	// NO_COLOR=0 refuses too. That includes NO_COLOR= — looser than
	// no-color.org, which exempts the empty string — because a variable somebody
	// exported is a statement, and /theme says out loud which rule fired, so the
	// one operator it surprises can see why and undo it in one word.
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		c.colour, c.noWhy = false, "NO_COLOR is set"
	} else {
		switch t := strings.TrimSpace(os.Getenv("TERM")); {
		case t == "":
			c.colour, c.noWhy = false, "TERM is not set"
		case strings.EqualFold(t, "dumb"):
			c.colour, c.noWhy = false, `TERM is "dumb"`
		}
	}
	if !c.colour {
		c.depth = depthNone
		return c
	}
	switch ct := strings.ToLower(os.Getenv("COLORTERM")); {
	case ct == "truecolor", ct == "24bit":
		c.depth = depthTruecolor
	default:
		t := strings.ToLower(os.Getenv("TERM"))
		if strings.Contains(t, "256color") || strings.Contains(t, "direct") {
			c.depth = depth256
		}
	}
	return c
}

// utf8Locale reports whether the locale can carry the box-drawing and block
// runes. With no locale set at all we assume it cannot: a terminal that never
// said UTF-8 and gets half a mojibake frame is worse off than one that gets
// hyphens, and this is the one guess where being wrong is cheap in one
// direction only.
func utf8Locale() bool {
	for _, v := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if s := os.Getenv(v); s != "" {
			u := strings.ToUpper(s)
			return strings.Contains(u, "UTF-8") || strings.Contains(u, "UTF8")
		}
	}
	return false
}

// themeFor is THE rule, and the only place it exists. Order matters:
//
//  1. stdout is not a terminal → plain, always, whatever any file says. A piped
//     or redirected run must never carry escapes: that payload is somebody's
//     input, and an escape in it is a bug that surfaces a week later.
//  2. an explicit preference (the `theme` setting, LCA_THEME, /theme) → that
//     theme. The operator looking at the screen outranks a guess about it.
//  3. otherwise detection: no colour possible → plain, else dungeon.
func themeFor(pref string, c termCaps) Theme {
	if !c.tty {
		return plainTheme(c, "stdout is not a terminal")
	}
	switch strings.ToLower(strings.TrimSpace(pref)) {
	case themePlain:
		return plainTheme(c, "set to plain")
	case themeDungeon:
		return dungeonTheme(c, "set to dungeon")
	}
	if !c.colour {
		return plainTheme(c, c.noWhy)
	}
	return dungeonTheme(c, "the default")
}

// applyTheme resolves the preference against this terminal and installs the
// result. It is called twice at startup — once before the config is read, so
// usage() and fatal() are already honest, and once after, so the `theme` key
// takes effect — and again on /theme, which is why it returns what it chose.
func applyTheme(pref string) Theme {
	t := themeFor(pref, detectCaps())
	setTheme(t)
	return t
}

// themeDetail is the account /theme prints beside the name: why this theme is in
// force, and the two capabilities that were resolved separately — because "it
// went grey" has four possible causes and the operator should not have to guess
// which one.
func themeDetail(t Theme) string {
	glyphs := "ASCII glyphs"
	if t.Unicode {
		glyphs = "UTF-8 glyphs"
	}
	return t.Why + gSep + t.Depth.String() + gSep + glyphs
}

// ── departures from the mockups ─────────────────────────────────────────────
//
// The design is a contract, so where the screens do NOT match it the reason is
// written down here rather than left for the next reader to rediscover. A
// contract that is departed from silently cannot be reviewed.
//
//  1. The dungeon NOUNS are not used as labels. The mockups call the roles table
//     PARTY, the verifier's line GATE, and the run THE DESCENT; the operator's
//     own ruling is that glyphs and palette carry the dungeon and the WORDS stay
//     plain English, naming LEVEL / PARTY / GATE / WARD / SEAL specifically. So
//     the verifier's line keeps the word "verify", the roles keep "role" and
//     "team", and the panel titles stay the framing they already were. The
//     dungeon is in the gutter, the stone and the light — never in place of a
//     word that tells the operator what a thing is.
//
//  2. There is no PARTY panel. The mockup's per-role model / window / ctx /
//     effort table is new DATA, not a restyling of the banner — Banner() has no
//     such table and /agents is where that data lives. Adding it here would put
//     two sources of truth on the first screen of every session.
//
//  3. The cost line stays ONE row, with the cache gauge inline, where the mockup
//     drew the gauge on a second row. printPerf runs once per model call, not
//     once per turn, so a second row is a second row per call: five calls in one
//     turn spent ten rows on accounting. The number is beside the bar either way.
//
//  4. The run's map strip carries its COUNTS and not the mockup's key. The key's
//     glyphs are the same ones the status column below it uses, and the counts
//     are the thing a picture may not make a claim without. Under a narrow frame
//     the corridors go first, then the strip folds onto whole-cell rows; a "["
//     and its "]" never land on different lines.
//
//  5. The ASCII tier substitutes the field separator (Sep) and the chrome dash
//     (Dash), but NOT an em dash inside a message sentence. Those sentences are
//     the messages themselves — the same bytes the trace, the audit log and
//     run.log carry — so rewriting them would change data rather than chrome,
//     and 316 of them exist. TestASCIIScreensAreSevenBitChrome allows exactly
//     that one rune and nothing else, which is why the allowance is visible.
