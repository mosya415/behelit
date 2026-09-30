package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// The seam's own tests. They are about the DECISION and the two tiers, not about
// how any screen looks: a look is reviewed by eye, but "a piped run carries no
// escape" is a promise, and a promise needs a test.

func TestThemeForPrecedence(t *testing.T) {
	tty := termCaps{tty: true, colour: true, depth: depth256, unicode: true}
	noColour := termCaps{tty: true, colour: false, depth: depthNone, unicode: true, noWhy: "NO_COLOR is set"}
	piped := termCaps{tty: false, colour: true, depth: depth256, unicode: true}

	cases := []struct {
		name string
		pref string
		caps termCaps
		want string
	}{
		{"a terminal with no preference is the dungeon", "", tty, themeDungeon},
		{"auto is the absence of a preference", themeAuto, tty, themeDungeon},
		{"plain is honoured on a terminal", themePlain, tty, themePlain},
		{"a terminal that cannot colour falls back", "", noColour, themePlain},
		{"an explicit dungeon beats detection", themeDungeon, noColour, themeDungeon},
		{"a pipe is plain whatever the file says", themeDungeon, piped, themePlain},
		{"a pipe is plain with no preference", "", piped, themePlain},
		{"case and spacing do not matter", "  PLAIN ", tty, themePlain},
	}
	for _, c := range cases {
		if got := themeFor(c.pref, c.caps).Name; got != c.want {
			t.Errorf("%s: themeFor(%q) = %q, want %q", c.name, c.pref, got, c.want)
		}
	}
	// the reason travels with the theme, because /theme has to be able to say why
	if why := themeFor(themeDungeon, piped).Why; !strings.Contains(why, "not a terminal") {
		t.Errorf("a piped run must say why it went plain, got %q", why)
	}
	if why := themeFor("", noColour).Why; why != noColour.noWhy {
		t.Errorf("the detected reason must survive, got %q", why)
	}
}

// A new colour field added to Palette and forgotten in the plain theme is the
// one way an escape gets back into piped output, so the emptiness is checked by
// reflection and not field by field.
func TestPlainThemeEmitsNothing(t *testing.T) {
	p := reflect.ValueOf(plainTheme(termCaps{unicode: true}, "test").Palette)
	for i := 0; i < p.NumField(); i++ {
		if got := p.Field(i).String(); got != "" {
			t.Errorf("plain theme sets %s = %q — piped output would carry it", p.Type().Field(i).Name, got)
		}
	}
}

// Every glyph in the ASCII tier must be 7-bit, or LC_ALL=C gets mojibake in the
// one place it was promised none — and a half-written rune shears the line.
func TestASCIIGlyphsAreASCII(t *testing.T) {
	g := reflect.ValueOf(glyphsASCII())
	for i := 0; i < g.NumField(); i++ {
		f, name := g.Field(i), g.Type().Field(i).Name
		var vals []string
		switch f.Kind() {
		case reflect.String:
			vals = []string{f.String()}
		case reflect.Slice:
			for j := 0; j < f.Len(); j++ {
				vals = append(vals, f.Index(j).String())
			}
		}
		for _, v := range vals {
			if v == "" {
				t.Errorf("%s is empty in the ASCII tier — a glyph that prints nothing hides a state", name)
			}
			for k := 0; k < len(v); k++ {
				if v[k] >= 0x80 {
					t.Errorf("%s = %q is not ASCII", name, v)
					break
				}
			}
		}
	}
}

// The UTF-8 tier is TORCHLIT's alphabet, frozen so that a change to it is a
// decision somebody made on purpose. It was the inherited alphabet until the look
// landed; ✓ ✗ ✕ are gone (the verifier states PASS or FAIL in words, and % is the
// one failure mark), and the braille ring is a torch.
func TestUTF8GlyphsAreTheTorchlitAlphabet(t *testing.T) {
	g := glyphsUTF8()
	for _, c := range []struct{ got, want, what string }{
		{g.Up, "●", "ok"}, {g.Partial, "◐", "partial"}, {g.Down, "%", "failed"}, {g.None, "·", "muted"},
		{g.Rule, "─", "hairline"}, {g.VBar, "│", "gutter"},
		{g.Bullet, "•", "list item"}, {g.Mask, "•", "secret mask"},
		{g.Hint, "↳", "what to do next"}, {g.Prompt, "›", "your turn"},
		// the map gutter, which is 7-bit on purpose: a roguelike map always was
		{g.Me, "@", "you"}, {g.Torch, "~", "the torch"}, {g.Probe, ".", "a look"},
		{g.Carve, "/", "a change"}, {g.Cmd, "^", "a command"}, {g.Cost, "$", "the cost"},
		{g.Prose, "▌", "the model's own words"},
		// Sep is the most frequent chrome on the screen and so is a tier value like
		// any other: a fallback that maps twenty runes and skips this one is not
		// the ASCII tier it claims to be.
		{g.Sep, " · ", "the field separator"}, {g.Dash, " — ", "a word and its gloss"},
		{g.Deeper, ">", "down a passage"}, {g.Out, "<", "back out"},
		{g.Loot, "*", "what came back"}, {g.Gate, "=", "the verifier"},
	} {
		if c.got != c.want {
			t.Errorf("%s glyph is %q, want %q", c.what, c.got, c.want)
		}
	}
	if len(g.Spinner) != 10 || g.Spinner[0] != "▁" {
		t.Errorf("the torch changed: %q", g.Spinner)
	}
	// Every glyph the look draws must measure the width it occupies, or one of
	// them shears every table cell and padTo() field it lands in. This is the ✓ ✗
	// ✕ ⚠ bug, which is why those three are gone and this is a test.
	rv := reflect.ValueOf(g)
	for i := 0; i < rv.NumField(); i++ {
		f, name := rv.Field(i), rv.Type().Field(i).Name
		var vals []string
		switch f.Kind() {
		case reflect.String:
			vals = []string{f.String()}
		case reflect.Slice:
			for j := 0; j < f.Len(); j++ {
				vals = append(vals, f.Index(j).String())
			}
		}
		for _, v := range vals {
			if n := utf8.RuneCountInString(v); n == 1 && visibleWidth(v) != 1 {
				t.Errorf("%s = %q is one rune but measures %d columns", name, v, visibleWidth(v))
			}
		}
	}
}

// setTheme is the only writer of the names the other 22 files read. If it misses
// one, that one keeps the previous theme's colour and the screen goes half-plain.
func TestSetThemeReachesTheLegacyNames(t *testing.T) {
	saved := activeTheme
	defer setTheme(saved)

	setTheme(plainTheme(termCaps{unicode: true}, "test"))
	for name, got := range map[string]string{
		"cReset": cReset, "cBold": cBold, "cBoldOff": cBoldOff, "cItalic": cItalic,
		"cItalicOff": cItalicOff, "cDim": cDim, "cFaint": cFaint, "cGreen": cGreen,
		"cYellow": cYellow, "cRed": cRed, "cCode": cCode, "cFgOff": cFgOff,
		"cBandBg": cBandBg, "cBlood": cBlood, "cBloodDark": cBloodDark,
	} {
		if got != "" {
			t.Errorf("%s = %q under the plain theme", name, got)
		}
	}
	// the primitives, not only the variables: this is what the screens call
	if s := faint("x") + warn("y") + okLine2(); strings.ContainsRune(s, 0x1b) {
		t.Errorf("a primitive still emits an escape under the plain theme: %q", s)
	}
	if gUp != "●" {
		t.Errorf("plain must keep the glyph tier: gUp = %q", gUp)
	}

	setTheme(plainTheme(termCaps{unicode: false}, "test"))
	if gUp != "*" || gRule != "-" || gSpinner[0] != "." || gProse != "|" || gPanelV != "|" {
		t.Errorf("the ASCII tier did not reach the glyph names: %q %q %q %q %q",
			gUp, gRule, gSpinner[0], gProse, gPanelV)
	}
	// the gauge is the one place the two tiers differ in WIDTH, so its own width
	// helper has to know: [####------] is twelve columns where ██████░░░░ is ten
	if gaugeWidth(10) != 12 {
		t.Errorf("the ASCII gauge must charge for its brackets: %d", gaugeWidth(10))
	}

	setTheme(saved)
	if cFaint == "" || gUp != "●" {
		t.Error("the theme did not come back")
	}
}

// okLine2 is a string-returning stand-in for the okLine/warnLine/errLine family,
// which print. It exists only so the escape check above covers the composition
// those three use.
func okLine2() string {
	return cGreen + gUp + cReset + " " + statusText(cRed, gDown, "down") + faint("%s", gNone)
}

func TestUTF8LocaleDetection(t *testing.T) {
	for _, c := range []struct {
		lcAll, lcCtype, lang string
		want                 bool
	}{
		{"en_US.UTF-8", "", "", true},
		{"C", "", "en_US.UTF-8", false}, // LC_ALL wins, as the locale rules say
		{"", "en_US.utf8", "", true},
		{"", "", "en_GB.UTF-8", true},
		{"", "", "C", false},
		{"", "", "", false}, // nothing said UTF-8: assume it cannot
	} {
		t.Setenv("LC_ALL", c.lcAll)
		t.Setenv("LC_CTYPE", c.lcCtype)
		t.Setenv("LANG", c.lang)
		if got := utf8Locale(); got != c.want {
			t.Errorf("utf8Locale() with LC_ALL=%q LC_CTYPE=%q LANG=%q = %v, want %v",
				c.lcAll, c.lcCtype, c.lang, got, c.want)
		}
	}
}

func TestDetectCapsRefusesColour(t *testing.T) {
	// t.Setenv registers the restore; os.Unsetenv then gives us a genuinely
	// absent variable, which t.Setenv cannot express and which is the case that
	// matters for NO_COLOR.
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "")
	os.Unsetenv("NO_COLOR")
	if c := detectCaps(); !c.colour || c.depth != depth256 {
		t.Errorf("a 256-colour TERM must be believed: %+v", c)
	}
	// presence, not value: NO_COLOR=0 and NO_COLOR= both still mean no colour
	for _, v := range []string{"0", "1", ""} {
		t.Setenv("NO_COLOR", v)
		if c := detectCaps(); c.colour || !strings.Contains(c.noWhy, "NO_COLOR") {
			t.Errorf("NO_COLOR=%q must refuse colour: %+v", v, c)
		}
	}
	os.Unsetenv("NO_COLOR")
	t.Setenv("TERM", "dumb")
	if c := detectCaps(); c.colour || !strings.Contains(c.noWhy, "dumb") {
		t.Errorf("TERM=dumb must refuse colour: %+v", c)
	}
	t.Setenv("TERM", "")
	if c := detectCaps(); c.colour {
		t.Errorf("an unset TERM must refuse colour: %+v", c)
	}
	t.Setenv("TERM", "xterm")
	t.Setenv("COLORTERM", "truecolor")
	if c := detectCaps(); c.depth != depthTruecolor {
		t.Errorf("COLORTERM=truecolor must be believed: %+v", c)
	}
}

// The `theme` setting has to round-trip through the one table every other
// setting uses, or it survives the session and not the restart.
func TestThemeSettingRoundTrips(t *testing.T) {
	s := findSetting("theme")
	if s == nil {
		t.Fatal("/config cannot list a setting /set refuses — theme has no row")
	}
	if s.JSON != "theme" || s.Env != "LCA_THEME" || !s.Live {
		t.Errorf("theme must be persistable, env-overridable and live: %+v", *s)
	}
	var c Config
	if err := applySetting(&c, "theme", "plain"); err != nil || c.Theme != themePlain {
		t.Fatalf("applySetting(plain) = %v, Theme = %q", err, c.Theme)
	}
	if v, err := jsonValueOf(c, "theme"); err != nil || v != "plain" {
		t.Errorf("jsonValueOf = %v, %v", v, err)
	}
	if err := applySetting(&c, "theme", "auto"); err != nil || c.Theme != "" {
		t.Errorf("auto must be stored as no preference at all: %q, %v", c.Theme, err)
	}
	if v, err := jsonValueOf(c, "theme"); err != nil || v != nil {
		t.Errorf("auto must delete the key from the file, got %v, %v", v, err)
	}
	if err := applySetting(&c, "theme", "torchlit"); err == nil {
		t.Error("an unknown theme must be refused")
	}
}

// ── widths that arrive as data ───────────────────────────────────────────────

// runeWidth is asked about runes the THEME draws (the test above) and about runes
// the MODEL sends. 0x2600–0x27BF holds both: ⚠ ✓ ✗ ✕ are East-Asian Ambiguous and
// drawn at one column, while ✅ ❌ ☕ ⚽ are Wide and drawn at two. Retiring the
// whole block fixed the first half and broke the second — and the second arrives
// in prose and in markdown tables, where renderMarkdownTable sizes every column
// with visibleWidth, so a cell measured one short shears every row below it.
func TestRuneWidthMeasuresDingbatsThatArriveAsData(t *testing.T) {
	for _, c := range []struct {
		r    rune
		want int
		what string
	}{
		{'⚠', 1, "ambiguous, and the theme's own warning glyph"},
		{'✓', 1, "ambiguous"}, {'✗', 1, "ambiguous"}, {'✕', 1, "ambiguous"},
		{'☑', 1, "ambiguous"}, {'✦', 1, "ambiguous"},
		{'✅', 2, "wide: a model's status table"},
		{'❌', 2, "wide"}, {'☕', 2, "wide"}, {'⚽', 2, "wide"}, {'⛔', 2, "wide"},
		{'✨', 2, "wide"}, {'❗', 2, "wide"}, {'➕', 2, "wide"},
		{'⭐', 2, "wide, and outside the block"},
		{'あ', 2, "wide, and outside the block"},
	} {
		if got := runeWidth(c.r); got != c.want {
			t.Errorf("runeWidth(%q) = %d, want %d (%s)", c.r, got, c.want, c.what)
		}
	}
	// and a table cell holding one is measured as the terminal draws it
	rows := []string{"| state | note |", "|---|---|", "| ✅ done | shipped |", "| ab done | shipped |"}
	out := stripEach(renderMarkdownTable(rows))
	if visibleWidth(out[2]) != visibleWidth(out[3]) {
		t.Errorf("a wide dingbat sheared the table:\n%s", strings.Join(out, "\n"))
	}
}

// The depth was detected, reported to the operator as "16 colours", and then
// ignored: a terminal that honours only SGR 30-37/90-97 was sent 256-colour and
// truecolor escapes, including a banner and a prompt band with no fallback. /theme
// exists to answer "why does this screen look like this?", so it may not answer
// wrongly.
func TestSixteenColourTierEmitsNoDeepEscapes(t *testing.T) {
	th := dungeonTheme(termCaps{tty: true, colour: true, depth: depth16, unicode: true}, "test")
	if th.Depth != depth16 {
		t.Fatalf("the theme forgot the depth: %v", th.Depth)
	}
	rv := reflect.ValueOf(th.Palette)
	for i := 0; i < rv.NumField(); i++ {
		v, name := rv.Field(i).String(), rv.Type().Field(i).Name
		for _, deep := range []string{"38;5;", "48;5;", "38;2;", "48;2;"} {
			if strings.Contains(v, deep) {
				t.Errorf("the 16-colour tier emits %s in %s: %q", deep, name, v)
			}
		}
	}
	// and the 256-colour tier is still the 256-colour tier
	deep := dungeonTheme(termCaps{tty: true, colour: true, depth: depth256, unicode: true}, "test")
	if !strings.Contains(deep.Dim, "38;5;") {
		t.Errorf("the 256-colour tier lost its depth: %q", deep.Dim)
	}
	if themeDetail(th) == themeDetail(deep) {
		t.Errorf("/theme cannot tell the two tiers apart: %q", themeDetail(th))
	}
}

// A bar and the number printed beside it can never disagree. gaugeFrac's "present
// but tiny" floor is right for a window bar — a 131k window beside a 1.05M one is
// not nothing — and wrong wherever the number was truncated to 0%, which is every
// fresh session against a large window.
func TestGaugeAgreesWithItsOwnNumber(t *testing.T) {
	saved := activeTheme
	t.Cleanup(func() { setTheme(saved) })
	setTheme(dungeonTheme(termCaps{tty: true, colour: true, depth: depth256, unicode: true}, "test"))
	const tok, budget = 863, 786432
	if pct := tok * 100 / budget; pct != 0 {
		t.Fatalf("this case is only interesting while the percentage truncates to 0; got %d", pct)
	}
	if g := stripANSI(gaugePct(tok, budget, 10, cYellow)); strings.ContainsAny(g, gaFull+gaHalf) {
		t.Errorf("a bar showed something beside a printed 0%%: %q", g)
	}
	// the window bars keep the floor: "tiny" must not look like "unknown"
	if g := stripANSI(gaugeFrac(float64(131072)/float64(1048576), 13, cYellow)); !strings.Contains(g, gaFull) {
		t.Errorf("a 131k window against 1.05M drew nothing: %q", g)
	}
	// and a real percentage still draws
	if g := stripANSI(gaugePct(9, 10, 10, cGreen)); !strings.Contains(g, gaFull) {
		t.Errorf("90%% drew no bar: %q", g)
	}
}
