package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Terminal styling. Three rules, from which everything follows:
//  1. Monochrome + one accent — grays for chrome, color ONLY as status
//     (green / yellow / red, muted phosphor tones, never acid).
//  2. Monospace, UPPERCASE, wide feel — but we uppercase only our own chrome
//     (labels, section titles, status words), NEVER data (paths, model ids,
//     commands, file contents), which must stay verbatim.
//  3. Hairlines — separation is a thin rule, not color or fill.
//  4. One owner. The palette and the glyph alphabet live in theme.go, which
//     sets the cXxx and gXxx names below from the theme in force, because a
//     terminal that cannot carry an escape or a rune has to be able to say so.

// behelitArt is the startup title in the ANSI-Shadow block font.
const behelitArt = `██████╗ ███████╗██╗  ██╗███████╗██╗     ██╗████████╗
██╔══██╗██╔════╝██║  ██║██╔════╝██║     ██║╚══██╔══╝
██████╔╝█████╗  ███████║█████╗  ██║     ██║   ██║
██╔══██╗██╔══╝  ██╔══██║██╔══╝  ██║     ██║   ██║
██████╔╝███████╗██║  ██║███████╗███████╗██║   ██║
╚═════╝ ╚══════╝╚═╝  ╚═╝╚══════╝╚══════╝╚═╝   ╚═╝   `

// darkOutline are the box-drawing runes rendered as the darker 3D/outline tone;
// everything else non-blank (block faces, the Behelit's features) reads bright.
const darkOutline = "╗╔╝╚═║╭╮╰╯╱╲─│▏▕▁▔"

// colorizeCrimson two-tones a line: bright crimson for solid faces/features,
// dark crimson for the outline runes.
func colorizeCrimson(line string) string {
	var b strings.Builder
	cur := ""
	for _, r := range line {
		c := cBlood
		if r == ' ' {
			c = ""
		} else if strings.ContainsRune(darkOutline, r) {
			c = cBloodDark
		}
		if c != cur {
			b.WriteString(cReset + c)
			cur = c
		}
		b.WriteRune(r)
	}
	b.WriteString(cReset)
	return b.String()
}

// printBehelit renders the block title in two-tone crimson.
func printBehelit() {
	// The block font is half-blocks and box-drawing. On a locale that never said
	// UTF-8 it arrives as mojibake, so the title falls back to being a word. So
	// does a terminal too narrow to hold the art: 52 columns of block runes folded
	// at 40 is not a banner, it is two rows of rubble.
	if !activeTheme.Unicode || termWidth() < 54 {
		fmt.Println(" " + cBold + "BEHELIT" + cReset)
		return
	}
	for _, t := range strings.Split(behelitArt, "\n") {
		fmt.Println(" " + colorizeCrimson(t))
	}
}

// stripANSI removes SGR escape sequences, leaving visible text (UTF-8 intact).
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// runeWidth approximates the terminal column width of a rune: 0 for combining
// marks / joiners / variation selectors, 2 for CJK-wide and emoji, else 1.
func runeWidth(r rune) int {
	switch {
	case r == 0x200D || r == 0xFE0F || r == 0x2060 || (r >= 0x0300 && r <= 0x036F):
		return 0
	case (r >= 0x1100 && r <= 0x115F), (r >= 0x2E80 && r <= 0xA4CF),
		(r >= 0xAC00 && r <= 0xD7A3), (r >= 0xF900 && r <= 0xFAFF),
		(r >= 0xFE30 && r <= 0xFE4F), (r >= 0xFF00 && r <= 0xFF60),
		(r >= 0x1F000 && r <= 0x1FAFF), (r >= 0x2B00 && r <= 0x2BFF):
		return 2
	case r >= 0x2600 && r <= 0x27BF:
		// This block is HALF wide, and counting all of it either way shears a row.
		// ⚠ ✓ ✗ ✕ are East-Asian AMBIGUOUS and every terminal we target draws them
		// at one column: charging them two sheared every table cell and padTo()
		// field that held one, which is the bug the retired glyphs made visible.
		// But the block also holds forty runes UAX#11 calls WIDE — ✅ ❌ ☕ ⚽ — and
		// those arrive as DATA, in the model's own prose and in the files it reads,
		// where a markdown table cell measured one short shears every row below it.
		// So the subranges are listed rather than the block waved through.
		return dingbatWidth(r)
	default:
		return 1
	}
}

// dingbatWidth is 2 for the East-Asian-Wide members of 0x2600–0x27BF and 1 for
// the ambiguous rest. The list is UAX#11's, transcribed: a range test cannot
// stand in for it because Wide and Ambiguous alternate inside the block.
//
// A variation selector (U+FE0F) promotes an ambiguous base to wide, and this
// function cannot see one — it is handed a single rune, and the selector is the
// NEXT one, measured as 0. So "⚠️" measures 1 where a terminal draws 2. Naming
// the case is the honest thing to do: fixing it means measuring pairs, and every
// caller here measures runes.
func dingbatWidth(r rune) int {
	switch r {
	case 0x267F, 0x2693, 0x26A1, 0x26CE, 0x26D4, 0x26EA, 0x26F5, 0x26FA, 0x26FD,
		0x2705, 0x2728, 0x274C, 0x274E, 0x2757, 0x27B0, 0x27BF:
		return 2
	}
	switch {
	case r >= 0x2614 && r <= 0x2615, r >= 0x2648 && r <= 0x2653,
		r >= 0x26AA && r <= 0x26AB, r >= 0x26BD && r <= 0x26BE,
		r >= 0x26C4 && r <= 0x26C5, r >= 0x26F2 && r <= 0x26F3,
		r >= 0x270A && r <= 0x270B, r >= 0x2753 && r <= 0x2755,
		r >= 0x2795 && r <= 0x2797:
		return 2
	}
	return 1
}

// visibleWidth is the printed width of s, skipping ANSI SGR escapes.
func visibleWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b { // ESC — skip a CSI sequence up to its final byte
			j := i + 1
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += runeWidth(r)
		i += size
	}
	return w
}

// padTo pads s (measured by visible width) to width columns. align: 0 left, 1
// right, 2 center.
func padTo(s string, width, align int) string {
	pad := width - visibleWidth(s)
	if pad <= 0 {
		return s
	}
	switch align {
	case 1:
		return strings.Repeat(" ", pad) + s
	case 2:
		l := pad / 2
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", pad-l)
	default:
		return s + strings.Repeat(" ", pad)
	}
}

// termWidth is the full terminal width for hairlines and right-alignment: the
// real column count from the terminal (osTermWidth), else $COLUMNS, else 80.
func termWidth() int {
	if w := osTermWidth(); w > 0 {
		return clampWidth(w)
	}
	if c := strings.TrimSpace(os.Getenv("COLUMNS")); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 0 {
			return clampWidth(n)
		}
	}
	return 80
}

// houseWidth is the one width our own chrome is drawn to: the 76-column house, or
// the terminal when it is narrower. The panels, the lintel and the prompt's band
// and fences all ask for it — a 140-column terminal used to get a 140-column
// hairline above a 76-column frame, which reads as one of them being broken.
func houseWidth() int { return min(termWidth(), 76) }

func clampWidth(n int) int {
	switch {
	case n < 20:
		return 20
	case n > 220:
		return 220
	default:
		return n
	}
}

// clearScreen homes the cursor and clears the VISIBLE screen so a session starts
// at the top — but deliberately does NOT emit \033[3J (erase-scrollback), so the
// terminal's scrollback is preserved and you can scroll up past the banner into
// earlier output / your shell history. No-op unless stdout is a real terminal.
func clearScreen() {
	if osTermWidth() > 0 {
		fmt.Print("\033[H\033[2J")
	}
}

// eyebrow prints a faint uppercase super-label with a leading hairline tick.
func eyebrow(s string) {
	fmt.Printf(" %s%s %s%s\n", cFaint, gRule, strings.ToUpper(s), cReset)
}

// kv prints a key/value row: faint uppercase label, verbatim value.
func kv(label, value string) {
	fmt.Printf("  %s%-8s%s %s\n", cFaint, strings.ToUpper(label), cReset, value)
}

// contValue prints a continuation line aligned under the value column.
func contValue(value string) {
	fmt.Printf("  %-8s %s\n", "", value)
}

// statusText wraps a status word in its color with a leading glyph.
func statusText(color, glyph, word string) string {
	return color + glyph + " " + strings.ToUpper(word) + cReset
}

// toolGutter is the class mark a tool call opens its line with: a probe that
// only looks at the world, a carve that changes it, a trap you set yourself, or
// a way down into a side passage. The verb and its argument say WHAT; the gutter
// says what KIND, so the left edge of a long scrollback reads as a map.
func toolGutter(name string) string {
	switch name {
	case "edit", "write":
		return gCarve
	case "run_command":
		return gCmd
	case "delegate", "task":
		return gDeeper
	}
	return gProbe
}

// toolGutterTone lights the gutter only where the world is about to change: a
// read is mortar, an edit or a command is torch.
func toolGutterTone(name string) string {
	switch name {
	case "edit", "write", "run_command", "delegate", "task":
		return cYellow
	}
	return cFaint
}

// toolLine prints the activity marker for an auto-running tool: the class mark,
// then the verb (chrome) and its verbatim argument (data).
func toolLine(name, arg string) {
	fmt.Printf(" %s%s%s %s%-8s%s %s\n", toolGutterTone(name), toolGutter(name), cReset, cDim, toolVerb(name), cReset, arg)
}

// toolInfo prints a faint informational outcome under a tool marker (read-only
// tools: counts). toolOK / toolErr print an action outcome with a status glyph.
// All three indent to the data column of the line above, so an outcome can
// never be mistaken for a gutter class of its own.
func toolInfo(text string) { fmt.Printf("     %s%s %s%s\n", cFaint, gFlow, text, cReset) }
func toolOK(text string)   { fmt.Printf("     %s%s%s %s\n", cGreen, gUp, cReset, text) }
func toolErr(text string)  { fmt.Printf("     %s%s%s %s\n", cRed, gDown, cReset, text) }

// toolLoot is what a side passage sent back. It is deliberately not a status
// mark: the verifier already said whether the diff passed, and this line says
// only how much came back — so it takes the map's loot glyph and not ●.
func toolLoot(text string) { fmt.Printf("     %s%s%s %s\n", cYellow, gLoot, cReset, text) }

func warn(format string, a ...any) string {
	return cYellow + fmt.Sprintf(format, a...) + cReset
}

func faint(format string, a ...any) string {
	return cFaint + fmt.Sprintf(format, a...) + cReset
}

// shortDir abbreviates the home directory to ~ for a compact status display.
func shortDir(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(os.PathSeparator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}

// ── components ──────────────────────────────────────────────────────────────
// Small building blocks every screen is made of, so the whole UI reads as one
// system: a section header, label/value rows, aligned tables, hints, and
// paths shortened the same way everywhere.

// section prints a titled hairline: " ── TITLE  detail ───────────". The title
// is chrome (upper-cased); the optional detail is data (paths, ids) and is
// shown verbatim.
func section(title string, detail ...string) {
	t := strings.ToUpper(title)
	d := ""
	if len(detail) > 0 && detail[0] != "" {
		d = strings.Join(detail, " ")
	}
	plain := t
	if d != "" {
		plain += "  " + d
	}
	// a fixed total width (or the terminal's, if narrower) so rules line up
	w := min(termWidth(), 76) - visibleWidth(plain) - 5
	if w < 3 {
		w = 3
	}
	// The lintel: ONE weight of stone the whole way across, the title lit. It was
	// two courses of ═ butted against a run of ─, which reads as a rule that
	// changed weight two characters in — a rendering fault, not a lintel — and it
	// sits directly beside panels drawn entirely in ═. The title stays the
	// caller's own noun ("gateway", "steps", "context"): the dungeon is in the
	// stone and the light, never in place of the word that tells you what you are
	// looking at.
	lit := cYellow + t + cReset
	if d != "" {
		lit += "  " + cDim + d + cReset
	}
	fmt.Printf("\n %s%s%s %s %s%s%s\n", cDim, strings.Repeat(gRule, 2), cReset, lit, cFaint, strings.Repeat(gRule, w), cReset)
}

// row prints a label/value line with the label column aligned.
func row(label, value string) {
	fmt.Printf("  %s%-9s%s %s\n", cFaint, label, cReset, value)
}

// hint prints a faint follow-up line: what to do next.
//
// It is deliberately NOT wrapped. A hint is one sentence and a good third of them
// name a command meant to be pasted; wrapping one puts a line break inside the
// sentence, and the tests that check a hint says what it should say — that Enter
// takes the proposal, that this run resumes with this command — look for the
// sentence whole. The terminal's own soft wrap keeps the bytes contiguous, which
// is the property that matters. The picker's legend is the exception and wraps
// itself, because there the line COUNT has to stay known for the walk-back.
func hint(format string, a ...any) {
	fmt.Println("  " + cFaint + gHint + " " + fmt.Sprintf(format, a...) + cReset)
}

// okLine / warnLine / errLine are standalone status lines with the shared glyphs.
func okLine(format string, a ...any) {
	fmt.Println("  " + cGreen + gUp + cReset + " " + fmt.Sprintf(format, a...))
}
func warnLine(format string, a ...any) {
	fmt.Println("  " + cYellow + gPartial + cReset + " " + fmt.Sprintf(format, a...))
}
func errLine(format string, a ...any) {
	fmt.Println("  " + cRed + gDown + cReset + " " + fmt.Sprintf(format, a...))
}

// table prints rows as aligned columns (ANSI-aware widths). The header row, if
// any, is faint; the last column may be truncated to fit the terminal.
func table(header []string, rows [][]string) {
	for _, l := range tableLines(header, rows, "  ", termWidth()) {
		fmt.Println(l)
	}
}

// tableLines is table() as a value, so a panel can put the same aligned columns
// inside a frame instead of at the left margin. lead is the indent every row
// carries; width is the room the last column may grow into.
func tableLines(header []string, rows [][]string, lead string, width int) []string {
	cols := len(header)
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	widths := make([]int, cols)
	all := rows
	if header != nil {
		all = append([][]string{header}, rows...)
	}
	for _, r := range all {
		for i, c := range r {
			widths[i] = max(widths[i], visibleWidth(c))
		}
	}
	buildRow := func(r []string, faintRow bool) string {
		var b strings.Builder
		b.WriteString(lead)
		used := visibleWidth(lead)
		for i, c := range r {
			if i == len(r)-1 {
				if room := width - used - 1; room > 8 && visibleWidth(c) > room {
					c = ellipsize(stripANSI(c), room)
				}
				b.WriteString(c)
				break
			}
			b.WriteString(padTo(c, widths[i], 0) + "  ")
			used += widths[i] + 2
		}
		if faintRow {
			return cFaint + stripANSI(b.String()) + cReset
		}
		return b.String()
	}
	var out []string
	if header != nil {
		out = append(out, buildRow(header, true))
	}
	for _, r := range rows {
		out = append(out, buildRow(r, false))
	}
	return out
}

// ── gauges ──────────────────────────────────────────────────────────────────
// A resource bar, under two rules that are the whole reason it is allowed on a
// working tool's screen:
//
//  1. It never appears without its own number beside it. The bar is the shape of
//     the answer; the digits are the answer. Every caller prints both.
//  2. It is not drawn at all when the denominator is a guess. ctxBudget() falls
//     back to a placeholder when the model's window is unknown, and a confident
//     76% against an invented 24k is a lie in picture form — worse than no
//     picture, because a picture is believed before it is read.
//
// Hence the signatures: gauge() takes the real numerator and denominator and
// returns "" for a denominator it cannot trust, so "no gauge" is the default and
// drawing one is the thing that takes an argument.

// gaugeCells is the length of a bar that shares a panel with another bar. Two
// stacked gauges at two lengths end at two columns and put the numbers beside
// them two columns apart, which reads as one of them being clipped — so the
// screens that stack them all say gaugeCells and not a number of their own.
const gaugeCells = 16

// gaugeWidth is the columns a gauge of n cells occupies, brackets included. The
// ASCII tier brackets its bar, so a caller laying out a row cannot assume n.
func gaugeWidth(cells int) int {
	return cells + visibleWidth(gaOpen) + visibleWidth(gaClose)
}

// gauge draws num/den across cells columns in tone, or nothing when den is not a
// real denominator.
func gauge(num, den, cells int, tone string) string {
	if den <= 0 {
		return ""
	}
	return gaugeFrac(float64(num)/float64(den), cells, tone)
}

// gaugePct is gauge for the bars whose number beside them is a percentage that
// the caller computed by TRUNCATING. There the "present but tiny" floor is wrong:
// a visible half cell next to a printed 0% is the bar and the digits
// contradicting each other, which is the smallest version of exactly the lie the
// gauge policy exists to prevent. So the bar rounds the way the number does, and
// both come from the one fraction.
func gaugePct(num, den, cells int, tone string) string {
	if den <= 0 {
		return ""
	}
	return gaugeFracFloor(float64(num)/float64(den), cells, tone, false)
}

// gaugeFrac is gauge for a fraction that has already been computed. The half
// cell matters: at thirteen cells a window of 131k next to one of 1.05M is a
// tenth of a bar, and rounding it away makes "small" look like "nothing".
func gaugeFrac(frac float64, cells int, tone string) string {
	return gaugeFracFloor(frac, cells, tone, true)
}

// gaugeFracFloor is gaugeFrac with the floor made a choice instead of an
// assumption. floor is right for a window bar — a 131k window beside a 1.05M one
// is a tenth of a cell and it is not nothing — and wrong wherever the number
// printed beside the bar was rounded down to zero.
func gaugeFracFloor(frac float64, cells int, tone string, floor bool) string {
	if cells <= 0 || !activeTheme.Frames {
		return ""
	}
	switch {
	case frac < 0:
		frac = 0
	case frac > 1:
		frac = 1
	}
	halves := int(frac*float64(cells)*2 + 0.5)
	if halves == 0 && frac > 0 && floor {
		halves = 1 // present but tiny is not the same as absent
	}
	full, rest := halves/2, halves%2
	var b strings.Builder
	b.WriteString(gaOpen + tone)
	b.WriteString(strings.Repeat(gaFull, full))
	if rest == 1 && full < cells {
		b.WriteString(gaHalf)
	}
	if n := cells - full - rest; n > 0 {
		b.WriteString(cFaint + strings.Repeat(gaEmpty, n))
	}
	b.WriteString(cReset + gaClose)
	return b.String()
}

// gaugeSplit is the one two-tone bar: the part that worked and the part that did
// not, in the same track. The bad segment is drawn at a minimum of one cell
// whenever the count is non-zero, so "2 calls out of 142 didn't parse" never
// renders as a bar with no failure in it at all.
func gaugeSplit(good, bad, cells int, goodTone, badTone string) string {
	den := good + bad
	if den <= 0 || cells <= 0 || !activeTheme.Frames {
		return ""
	}
	badCells := bad * cells / den
	if bad > 0 && badCells == 0 {
		badCells = 1
	}
	goodCells := cells - badCells
	if good == 0 {
		goodCells = 0
		badCells = cells
	}
	var b strings.Builder
	b.WriteString(gaOpen + goodTone + strings.Repeat(gaFull, goodCells))
	b.WriteString(badTone + strings.Repeat(gaBad, badCells))
	if n := cells - goodCells - badCells; n > 0 {
		b.WriteString(cFaint + strings.Repeat(gaEmpty, n))
	}
	b.WriteString(cReset + gaClose)
	return b.String()
}

// ── panels ──────────────────────────────────────────────────────────────────
// A closed frame with its title set into the top course. It is used ONLY where
// the whole content is known before anything is printed — a banner, a question,
// a finished table — because a frame that is opened and then interleaved with a
// parallel subagent's line never closes. Everything that streams keeps a spine
// (childView) or a gutter (the transcript) instead, which is the same reason the
// tree never grew a box around a subagent.
//
// The title is chrome and is upper-cased; the detail and the right-hand tag are
// data and are printed verbatim, which is ui.go's rule 2 and the reason a panel
// may be called THE HOLD while every noun inside it stays the operator's own.
type panelLine struct {
	div  bool   // a division rule rather than content
	text string // the content, or the division's own title
}

type panel struct {
	title, detail, right         string
	frame, titleTone, detailTone string
	rightTone                    string
	lines                        []panelLine

	// need is the content width some row asked for because it holds a token that
	// cannot be broken. The frame is otherwise deliberately independent of its
	// content (see width()); this is the one thing allowed to move it, because the
	// alternative is a ║ between the halves of a path.
	need int
}

func newPanel(title string, detail ...string) *panel {
	p := &panel{
		title: strings.ToUpper(title),
		frame: cDim, titleTone: cYellow + cBold, detailTone: cDim, rightTone: cDim,
	}
	if len(detail) > 0 {
		p.detail = detail[0]
	}
	return p
}

// door lights the whole frame. A door is the one thing on screen that is waiting
// for you, and it is the only frame in the look that is not grey — which is the
// entire signal, so nothing else may take it.
//
// A door is a lit frame and NOT a gutter class: the map's classes say what kind
// of thing happened in a transcript that scrolls past, and this is the one thing
// on the screen that stops you, so it gets the frame rather than a column.
func (p *panel) door() *panel {
	p.frame, p.detailTone, p.rightTone = cYellow, cBold, ""
	return p
}

// tag sets the right-hand end of the top course: who is asking, which run it is.
func (p *panel) tag(s string) *panel { p.right = s; return p }

func (p *panel) Line(format string, a ...any) {
	p.lines = append(p.lines, panelLine{text: fmt.Sprintf(format, a...)})
}

// Row is row() inside a frame: the label column aligned the same way, so a panel
// and a bare section read as the same screen.
//
// A value too long for the frame WRAPS, indented to its own column, rather than
// being hard-split like a preview line: a row's value is a sentence, and a
// sentence broken at column 72 and continued at column 1 is unreadable. Wrapping
// re-measures it plain and so loses its styling, which is the cheaper loss.
func (p *panel) Row(label, value string) {
	room := nominalContent() - 10
	if room > 20 && visibleWidth(value) > room {
		segs := wrapTo(stripANSI(value), room)
		// wrapTo breaks on spaces and never inside a word, so ONE segment still
		// wider than the room it was measured against is a single unbreakable
		// token: a path, a run id, a URL — which is exactly the thing on a panel an
		// operator copies. Indenting it under the value column would cost it ten
		// more columns and hand the overflow to panelSplit, which cuts at the
		// frame: the last four characters of a trace filename on the next framed
		// row, with ║ between them, and a copy of the two lines drags the stone
		// into the path. So it gets the whole content width on a row of its own,
		// with the label above it, and the frame is asked to widen to hold it.
		if len(segs) == 1 {
			p.need = max(p.need, visibleWidth(segs[0]))
			p.Line("%s%-9s%s", p.frame, label, cReset)
			p.Line("%s", value)
			return
		}
		for i, l := range segs {
			if i == 0 {
				p.Line("%s%-9s%s %s", p.frame, label, cReset, l)
			} else {
				p.Line("%-10s%s", "", l)
			}
		}
		return
	}
	p.Line("%s%-9s%s %s", p.frame, label, cReset, value)
}

// needs records that a line the caller is adding itself holds a token that cannot
// be broken, so width() may widen the frame for it if the terminal has the room.
// Row() does this for a value it wrapped; the approval door does it for the target
// it moved out of the title course.
func (p *panel) needs(n int) { p.need = max(p.need, n) }

func (p *panel) Div(title string) {
	p.lines = append(p.lines, panelLine{div: true, text: strings.ToUpper(title)})
}

// Table puts aligned columns inside the frame. The width it is given is the
// frame's, not the terminal's, so the last column is cut to the frame and never
// pushed through it.
func (p *panel) Table(header []string, rows [][]string, width int) {
	for _, l := range tableLines(header, rows, "", width) {
		p.lines = append(p.lines, panelLine{text: l})
	}
}

// nominalContent is the content width a panel lays out for: the house 76-column
// frame, or the terminal when it is narrower. Rows wrap against THIS and not
// against the panel's final width, because the final width depends on the rows —
// and a layout that depends on its own output cannot be reasoned about.
func nominalContent() int {
	w := min(termWidth()-2, 76)
	if w < 16 {
		w = 16
	}
	return w - 4
}

// width is the frame width, and it deliberately does NOT depend on the content. A
// panel that grew to fit its widest row left two panels on the same screen at two
// different widths, which reads as one of them being broken — and a title course
// that grew had nothing stopping it at the terminal's edge. So the frame is the
// 76-column house width (or the terminal, when narrower), the title course is cut
// to fit it, and over-long content is split by panelSplit or wrapped by Row.
func (p *panel) width() int {
	w := min(termWidth()-2, 76)
	// ...with the one exception Row() records: a value that cannot be broken gets
	// the room it needs if the TERMINAL has it. A 140-column terminal holding a
	// 76-column frame that shears a path has 64 columns going spare, and a frame
	// that gives way is better than an identifier that does.
	if p.need+4 > w {
		w = max(w, min(termWidth()-2, p.need+4))
	}
	if w < 12 {
		w = 12
	}
	return w
}

// fitTop cuts the title course to the frame. Things are given up in the order they
// matter least: the detail first, then the right-hand tag, and the title only if
// it will not fit on its own.
func (p *panel) fitTop(w int) (title, detail, right string) {
	title, detail, right = p.title, p.detail, p.right
	fixed := func() int { // everything but the detail and the one course of fill
		n := visibleWidth(gPanelTL+gPanelH+" ") + visibleWidth(title) + 1 + visibleWidth(gPanelH+gPanelTR)
		if right != "" {
			n += visibleWidth(" "+right+" ") + visibleWidth(gPanelH)
		}
		return n
	}
	if detail != "" {
		room := w - fixed() - visibleWidth(" "+gPanelH+" ")
		switch {
		case room < 4:
			detail = ""
		case visibleWidth(detail) > room:
			detail = ellipsize(stripANSI(detail), room)
		}
	}
	if right != "" && fixed() > w {
		right = ""
	}
	if n := fixed(); n > w {
		title = ellipsize(title, max(1, visibleWidth(title)-(n-w)))
	}
	return title, detail, right
}

// topPlain is the title course with no escapes, for measuring. fill is how many
// courses of stone go between the detail and the right-hand tag.
func topPlain(title, detail, right string, fill int) string {
	s := gPanelTL + gPanelH + " " + title
	if detail != "" {
		s += " " + gPanelH + " " + detail
	}
	s += " " + strings.Repeat(gPanelH, fill)
	if right != "" {
		s += " " + right + " " + gPanelH
	}
	return s + gPanelTR
}

func (p *panel) Print() {
	for _, l := range p.Lines() {
		fmt.Println(l)
	}
}

// expandTabs turns tabs into spaces on the way into a frame. visibleWidth() counts
// a tab as one column and the terminal draws it as up to eight, so a single tab in
// a diff preview walks the frame's right-hand border off the row. Only the frame
// cares: a tab in a streamed transcript line is the file's own indentation and is
// left exactly as the file has it.
func expandTabs(s string) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		if r != 0x1b {
			col += runeWidth(r)
		}
	}
	return b.String()
}

// panelSplit is what happens to a content line longer than the frame: it is HARD
// SPLIT, not cut. An approval preview is the thing you are being asked to
// approve, and a truncated command or a truncated diff line is how somebody
// approves something they did not read. The split re-measures the line plain and
// so loses its styling; it never loses a byte.
func panelSplit(s string, cw int) []string {
	if cw <= 0 || visibleWidth(s) <= cw {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	w := 0
	for _, r := range stripANSI(s) {
		rw := runeWidth(r)
		if w+rw > cw {
			out = append(out, cur.String())
			cur.Reset()
			w = 0
		}
		cur.WriteRune(r)
		w += rw
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// Lines is Print as a value, so a repainted region (the picker) can frame itself
// and still know exactly how many lines it drew — which is what the "\033[<n>A"
// walk-back depends on.
func (p *panel) Lines() []string {
	if !activeTheme.Frames {
		return p.bareLines()
	}
	var out []string
	w := p.width()
	cw := w - 4 // "║ " + content + " ║"

	// the title course, measured plain and then built styled, so the escapes
	// cannot shift the stone
	title, detail, right := p.fitTop(w)
	fill := w - visibleWidth(topPlain(title, detail, right, 0))
	if fill < 1 {
		fill = 1
	}
	top := p.frame + gPanelTL + gPanelH + " " + cReset + p.titleTone + title + cReset
	if detail != "" {
		top += " " + p.frame + gPanelH + cReset + " " + p.detailTone + detail + cReset
	}
	top += " " + p.frame + strings.Repeat(gPanelH, fill)
	if right != "" {
		top += cReset + " " + p.rightTone + right + cReset + " " + p.frame + gPanelH
	}
	out = append(out, " "+top+gPanelTR+cReset)

	for _, l := range p.lines {
		if l.div {
			if l.text == "" { // a plain course across the frame, with nothing set into it
				out = append(out, " "+p.frame+gPanelML+strings.Repeat(gPanelH, w-2)+gPanelMR+cReset)
				continue
			}
			dfill := w - visibleWidth(gPanelML+gPanelH+" "+l.text+" "+gPanelMR)
			if dfill < 1 {
				dfill = 1
			}
			out = append(out, " "+p.frame+gPanelML+gPanelH+" "+cReset+p.titleTone+l.text+cReset+
				" "+p.frame+strings.Repeat(gPanelH, dfill)+gPanelMR+cReset)
			continue
		}
		for _, t := range panelSplit(expandTabs(l.text), cw) {
			out = append(out, " "+p.frame+gPanelV+cReset+" "+padTo(t, cw, 0)+" "+p.frame+gPanelV+cReset)
		}
	}
	return append(out, " "+p.frame+gPanelBL+strings.Repeat(gPanelH, w-2)+gPanelBR+cReset)
}

// bareLines is the panel with no frame, for a run whose stdout is a file or a
// pipe. A box drawn around somebody's payload is new decoration in a machine
// path, and `lca run nightly | head` used to print the whole ╔═ STEPS ═╗ into
// it. The ROWS are kept — they are what the screen was for — and they are kept
// whole: with no frame there is no width to fit, so nothing is split and no path
// is cut. The title goes on its own line so the block is still findable.
func (p *panel) bareLines() []string {
	head := " " + p.title
	if p.detail != "" {
		head += "  " + p.detail
	}
	if p.right != "" {
		head += "  " + p.right
	}
	out := []string{head}
	for _, l := range p.lines {
		if l.div {
			if l.text != "" {
				out = append(out, " "+l.text)
			}
			continue
		}
		out = append(out, " "+l.text)
	}
	return out
}

// ellipsize cuts s to n visible runes, ending with "…".
func ellipsize(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n < 2 {
		return s
	}
	// the marker is measured, not assumed to be one column: the ASCII tier
	// spells it "...", and three columns charged as one shears every row it
	// lands in.
	m := visibleWidth(gEllipsis)
	if n <= m {
		return string(r[:n])
	}
	return string(r[:n-m]) + gEllipsis
}

// ellipsizeMiddle keeps both ends of long identifiers (paths, model ids).
func ellipsizeMiddle(s string, n int) string {
	r := []rune(s)
	m := visibleWidth(gEllipsis)
	if len(r) <= n || n < 4+m {
		return s
	}
	head := (n - m) / 2
	return string(r[:head]) + gEllipsis + string(r[len(r)-(n-m-head):])
}

// prettyPath shows a path relative to root when inside it, else ~-shortened,
// middle-ellipsized to a sane width.
func prettyPath(p, root string) string {
	if root != "" {
		if rel, err := filepath.Rel(root, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return ellipsizeMiddle(rel, 60)
		}
	}
	return ellipsizeMiddle(shortDir(p), 60)
}

// hostOf renders a base URL as host:port — the part a person recognizes.
func hostOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// toolVerb is how a tool call reads on screen: a verb and its object, e.g.
// "read  sum.go", "search  "func main" in pkg".
func toolVerb(name string) string {
	switch name {
	case "read_file":
		return "read"
	case "list_dir":
		return "list"
	case "grep":
		return "search"
	case "glob":
		return "find"
	case "run_command":
		return "run"
	case "webfetch":
		return "fetch"
	case "todowrite":
		return "todo"
	}
	return name
}
