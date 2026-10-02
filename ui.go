package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

// pageMax is THE PAGE: the widest our own CHROME is ever drawn, whatever the
// terminal. It is the ceiling on houseWidth() and so on every frame, lintel,
// fence, band, status line and identifier cap.
//
// It is a ceiling on DECORATION and deliberately not on data. A table is data,
// and data takes the window: tables are budgeted against the terminal instead
// (fitWidth), because a value that fits the window must be printed whole. A
// 400-column rule, by contrast, is noise whatever the window.
//
// It used to be justified partly on the -dry-run plan row "measuring about 160
// columns once its check is handed over whole". Measured, a real plan row with a
// two-command check is 165, so that derivation disagreed with the number it was
// offered for — and the plan is no longer budgeted here at all. What the number
// is actually tied to is the door: a door must never hard-split a source or diff
// line it is asking you to approve, real source stops around 100–120 columns, and
// 120 + 4 of frame + 1 of gutter is comfortably inside. It is also exactly double
// the 80-column floor, so the program's two settled widths are an octave apart
// rather than arbitrary neighbours.
//
// Above it the surplus is left empty ON PURPOSE. An eye travelling from a label
// in column 2 to a value that ended in column 40, across 120 blank columns, is
// the failure — not the fix. panelFloor is that same rule one level down: the
// page is where a frame STOPS growing, and panelFloor is where it stops growing
// for content that never asked for the room.
const pageMax = 160

// proseMax is the reading measure for SENTENCES: the ceiling on proseWidth().
// Typography puts comfortable reading at 45–90 characters and this is the top of
// that band. It is tied at both ends — it must be at least 77, because 77 is what
// the answer already wraps to at a terminal of 80 and the floor may not narrow,
// and at most 90, or it stops being a reading measure. Being a ceiling and not a
// target, it only bites from a terminal of 92 up: the answer reads the way it
// reads in a full-screen laptop window, at every window wider than that.
const proseMax = 88

// houseWidth is the one width our own chrome is drawn to: the panels, the lintel,
// the prompt's band and fences, the status line, the table budget and the
// picker's frame all ask for it. It was a fixed 76 whatever the terminal, which
// is why a 200-column window held the whole program in its left third — and why a
// 140-column terminal used to get a 140-column hairline above a 76-column frame,
// which reads as one of them being broken.
//
// The four columns it gives up are load-bearing and not slack for its own sake.
// One is the gutter every frame is already printed in. The other three mean
// nothing is ever drawn closer than three columns to the right edge: the cursor
// never rests in the DECAWM pending-wrap column, and a window narrowed between a
// region being drawn and its next repaint has to shrink by MORE THAN THREE
// columns before it can wrap a line the editor's walk-back counted. At a terminal
// of 80 the arithmetic yields 76, which is what it has always been — the floor
// did not move, the ceiling went away.
func houseWidth() int { return min(max(termWidth()-4, 16), pageMax) }

// proseWidth is the measure a SENTENCE is wrapped to. Structure takes the window;
// prose stops where reading stops being comfortable, because a 200-column line of
// text is harder to read than an 80-column one. Where the window is wide, the
// extra room goes to something that earns it — a wider data column, a path
// printed in full, a table that no longer truncates — and never to longer
// sentences.
//
// The two columns are the answer's own gutter. The prose writer prefixes every
// row with a three-column " ▌ ", so a measure of houseWidth()-2 lands the last
// cell of a wrapped line in column houseWidth()+1 — where every frame, lintel and
// fence on the same screen ends. Measured from termWidth() instead, the answer
// stuck out three columns past all of them at a terminal of 80 and its last cell
// rested in the DECAWM pending-wrap column that houseWidth() gives up four
// columns to avoid. At 80 the measure is 74, still well inside the 45–90 band.
func proseWidth() int { return min(houseWidth()-2, proseMax) }

// panelRowRoom is the room a Row's value has past its label column. It was
// written out twice — here and in the test that checks it — so it exists once.
func panelRowRoom() int { return nominalContent() - 10 }

// hasScreen is "is there a terminal whose width we are laying out against", and
// it is NOT the same question as "may we decorate". Theme.Screen says why the two
// are separate fields; every width decision in this file asks this one, and only
// the decoration asks activeTheme.Frames. A pipe or a file answers no to both,
// which is what keeps its cells whole and its rows unwrapped.
func hasScreen() bool { return activeTheme.Screen }

// fitWidth is the budget a TABLE is fitted to: 0 when there is no terminal to fit
// to at all — the rows are somebody's payload and every cell is printed whole —
// and otherwise the room the terminal actually has.
//
// It is deliberately NOT the page. The page is the ceiling on decoration, and
// budgeting a table against it cut a 165-column plan row to 161 on a 200-column
// screen with 39 columns standing empty beside it: the operator's own complaint
// happening inside the fix for it. A table is data, and data takes the window.
//
// It cannot STRETCH anything. The fit pass prints natural widths whenever they
// fit, so a wider budget can only ever mean fewer cells are cut — which is why
// this being the one number past the page costs nothing anywhere else. Above the
// clamp's 220 columns a table stops widening with the window like everything
// else; no table this program draws is near that.
func fitWidth() int {
	if !hasScreen() {
		return 0
	}
	return max(termWidth()-3, houseWidth()+1)
}

// clampWidth guards a garbage $COLUMNS. Nothing derives a CHROME number from
// termWidth() directly: every strings.Repeat and every wrap measure goes through
// houseWidth() (<= pageMax) or proseWidth() (<= proseMax). fitWidth() is the one
// number allowed past the page, and 220 is where it stops.
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

// wrapValue is what a bare label/value line does with a value too long for the
// page. It WRAPS onto continuation lines rather than ellipsizing: doctor's
// `thinking` row is 224 columns and every byte of it is the answer, so a row that
// ran off the screen at every terminal width becomes an aligned block that keeps
// all of them.
//
// A single unbreakable token — a path, a model id, a URL — comes back whole and
// is never cut: a cut path is worse than a soft-wrapped one, and no repainted
// region draws through row() or kv(). And only where there are frames: piped or
// redirected, the value stays on one unbroken line, because a line break inserted
// into somebody's payload is as much of a change as a box drawn around it.
func wrapValue(value string, room int) []string {
	if !hasScreen() || room <= 20 || visibleWidth(value) <= room {
		return []string{value}
	}
	segs := wrapTo(stripANSI(value), room)
	if len(segs) <= 1 { // one unbreakable token: keep it whole, with its styling
		return []string{value}
	}
	return segs
}

// kv prints a key/value row: faint uppercase label, verbatim value.
func kv(label, value string) {
	// "  " + %-8s + " " is 11 columns, against a printed width of houseWidth()+1
	for i, l := range wrapValue(value, min(houseWidth()-10, proseMax)) {
		if i == 0 {
			fmt.Printf("  %s%-8s%s %s\n", cFaint, strings.ToUpper(label), cReset, l)
			continue
		}
		contValue(l)
	}
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
//
// A lintel is chrome FOR THE BLOCK BENEATH IT, so it is drawn to that block —
// sectionTo, or sectionTable where the block is a table. Drawn to the page
// instead, `doctor` on a 200-column screen was four 161-column rules standing
// over lines of 12 to 52 columns: the rule pointing straight at the emptiness,
// four times. A caller with nothing measured gets the panelFloor lintel, which is
// wide enough to read as a lintel and narrow enough to make no claim about content
// it cannot see.
func section(title string, detail ...string) { sectionTo(0, title, detail...) }

// sectionTable is the lintel and its table as one call — the one pairing where
// the block's width IS known before anything is printed, and the pairing every
// data screen in the program is made of: the plan, doctor's roles, the results
// table, /agents, /skills, /members. The rule ends in the same column as the
// widest row under it at every width.
func sectionTable(title, detail string, header []string, rows [][]string, mid ...int) {
	ls, w := tableBlock(header, rows, mid...)
	sectionTo(w, title, detail)
	for _, l := range ls {
		fmt.Println(l)
	}
}

// tableBlock is table() held back one step: the fitted lines and the printed width
// they came out at, so a caller can draw its lintel to the table instead of the
// table to its lintel. It exists because the measure has to be taken before the
// first line is printed, and taking it twice is how two numbers drift apart.
func tableBlock(header []string, rows [][]string, mid ...int) ([]string, int) {
	ls := tableLines(header, rows, "  ", fitWidth(), mid...)
	w := 0
	for _, l := range ls {
		w = max(w, visibleWidth(l))
	}
	return ls, w
}

// sectionTo is section drawn to the PRINTED width of the widest line that will
// appear under it; 0 means the caller has not measured its block.
//
// The ceiling is fitWidth() and not the page, because the one block that
// legitimately runs past the page is a table — data takes the window — and a
// lintel one column short of its own table reads as a rendering fault rather than
// as a decision. The floor is panelFloor, for the reason panelFloor gives.
func sectionTo(under int, title string, detail ...string) {
	t := strings.ToUpper(title)
	d := ""
	if len(detail) > 0 && detail[0] != "" {
		d = strings.Join(detail, " ")
	}
	// The lintel is drawn to its own block: its printed width is h+1, which is the
	// printed width of the widest line beneath it. With nothing measured it is the
	// floor, and at a terminal of 80 the floor is houseWidth() — so the 80-column
	// lintel is the one it has always been, and it is still exactly where a frame
	// ends rather than one course short of it.
	//
	// With no terminal it is the CONSTANT one an 80-column screen draws. Everything
	// else in a pipe already refuses to depend on the window — no frame, no fit, no
	// wrap, and the answer's own rules are pinned — and a rule whose length came
	// from $COLUMNS meant `lca run -dry-run | tee log` wrote different bytes on two
	// machines for the same plan.
	h := panelFloor
	if hasScreen() {
		h = min(max(under-1, min(panelFloor, houseWidth())), max(fitWidth(), houseWidth()+1)-1)
	}
	plain := func() string {
		if d == "" {
			return t
		}
		return t + "  " + d
	}
	// The fill used to floor at 3 courses and let the line run off the screen: the
	// -dry-run PLAN header, whose detail is a workflow name and a path, measured
	// 164 columns on an 80-column terminal. The DETAIL is what gives way, and it is
	// middle-ellipsized because a path and a run id both end in something that
	// matters — until two courses of stone fit beside it. One fix in both
	// directions: that header stops overflowing at 80 and prints whole inside a
	// 132-column lintel at 140.
	w := h + 1 - visibleWidth(plain()) - 5
	if w < 2 && d != "" {
		if room := visibleWidth(d) - (2 - w); room >= 4+visibleWidth(gEllipsis) {
			d = ellipsizeMiddle(stripANSI(d), room)
		} else {
			d = ""
		}
		w = h + 1 - visibleWidth(plain()) - 5
	}
	if w < 2 {
		w = 2
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
	// "  " + %-9s + " " is 12 columns, against a printed width of houseWidth()+1
	for i, l := range wrapValue(value, min(houseWidth()-11, proseMax)) {
		if i == 0 {
			fmt.Printf("  %s%-9s%s %s\n", cFaint, label, cReset, l)
			continue
		}
		contRow(l)
	}
}

// contRow is row()'s continuation line, aligned under its nine-column label.
func contRow(value string) {
	fmt.Printf("  %-9s %s\n", "", value)
}

// hint prints a faint follow-up line: what to do next.
//
// It DOES wrap now, to the same reading measure a tool error's hints already wrap
// to, because the program was holding two incompatible positions on the one
// surface. doctor's longest hint is 169 columns: at 80 it broke to column 1
// instead of indenting under the ↳, and at 200 and 400 it was the widest line on
// the whole screen — a 169-column sentence on a screen whose answer is held to 88,
// which is the "stretched thin" case the rest of this file exists to prevent.
//
// The old reason for leaving it alone is kept as the exception rather than the
// rule: a good third of these name a command meant to be pasted. wrapTo never
// breaks inside a word, so a hint that comes back as ONE segment still wider than
// the measure is a single unbreakable token — a command, a path, a URL — and is
// printed whole. The sentences wrap; the pastes do not.
func hint(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	// "  " + ↳ + " " is four columns, against a printed width of h+1
	for i, l := range wrapHint(s, min(houseWidth()-3, proseMax)) {
		if i == 0 {
			fmt.Println("  " + cFaint + gHint + " " + l + cReset)
			continue
		}
		fmt.Println("    " + cFaint + l + cReset)
	}
}

// wrapHint is the shared wrap for the program's one-sentence follow-up lines: the
// hints under a screen and the hints under a tool error. It re-measures the
// sentence plain and so loses its styling, which is the cheaper loss, and only an
// over-long line pays it.
func wrapHint(s string, room int) []string {
	if !hasScreen() || room <= 24 || visibleWidth(s) <= room {
		return []string{s}
	}
	segs := wrapTo(stripANSI(s), room)
	// wrapTo breaks on spaces and never inside a word, so a segment still wider than
	// the room it was measured against is a single unbreakable token: a command, a
	// path, a URL.
	//
	// Where that token is the LAST thing on the line, the line is a verb handing you
	// a value — `wrote /a/very/long/path` — and wrapping it buys nothing: the token
	// ends up alone on a row that is still too long, and the word that said what it
	// is has been split away from it. So that whole line goes through unwrapped and
	// the terminal's own soft wrap keeps its bytes contiguous, which is the property
	// that matters for something you are about to copy. A long token in the MIDDLE
	// of a sentence is just a long word: the sentence wraps around it and the word
	// overflows its own row, whole.
	if len(segs) <= 1 || visibleWidth(segs[len(segs)-1]) > room {
		return []string{s}
	}
	return segs
}

// okLine / warnLine / errLine are standalone status lines with the shared glyphs.
// They are the same class of one-sentence line as hint() and they wrap the same
// way, for the same reason: `doctor` at 80 emitted twenty-one over-width lines and
// almost all of them were these, breaking to column 1 rather than indenting under
// their own glyph. A single unbreakable token — a path, a URL, a command — still
// goes through whole.
func okLine(format string, a ...any) {
	glyphLine(cGreen+gUp+cReset, fmt.Sprintf(format, a...))
}
func warnLine(format string, a ...any) {
	glyphLine(cYellow+gPartial+cReset, fmt.Sprintf(format, a...))
}
func errLine(format string, a ...any) {
	glyphLine(cRed+gDown+cReset, fmt.Sprintf(format, a...))
}

// glyphLine prints one sentence behind a status glyph, wrapped to the reading
// measure with its continuations aligned under the sentence rather than under the
// glyph.
func glyphLine(glyph, s string) {
	lead := 2 + visibleWidth(stripANSI(glyph)) + 1
	for i, l := range wrapHint(s, min(houseWidth()+1-lead, proseMax)) {
		if i == 0 {
			fmt.Println("  " + glyph + " " + l)
			continue
		}
		fmt.Println(strings.Repeat(" ", lead) + l)
	}
}

// colFloor is the narrowest a table column may be shaved to, as
// min(natural, max(header width, colFloor)): four head characters, the measured
// ellipsis marker, four tail characters and a little slack — the least that still
// identifies a path or a check. Below that a cell says nothing, and dropping the
// column would be more honest than pretending. Expressed as a min() against the
// natural width it also means a column that is naturally short — a count, a tick,
// a status word, a duration — cannot be shaved at all, so the allocator needs no
// table of column classes.
const colFloor = 12

// tableGap is the columns between two columns. It is a const rather than a local
// because tableBounds' arithmetic and buildRow's padding have to agree about it.
const tableGap = 2

// cellBytes caps each cell before it is measured. It is a memory guard and NOT a
// layout number, and it stands in the one place where every cell passes now that
// the display truncates on the plan's check and the summary's detail are gone:
// firstLine() bounds a cell to one line and no longer to its length, so a step
// whose detail is one 40k line arrives here whole.
const cellBytes = 4096

// capCell applies cellBytes, and applies it the two ways a plain byte slice did
// not. It cuts on a RUNE boundary, because a cut inside a UTF-8 sequence makes the
// whole emitted row invalid output rather than a short one; and it hands back the
// reset of a styled cell, because a cut that lands before a cell's own cReset
// leaks the faint attribute into every line printed after the table until
// something else happens to reset it. It is only ever called where there is a
// width to fit to — with no terminal the promise is that every cell goes through
// unchanged, and a memory guard is not a reason to break it.
func capCell(c string) string {
	if len(c) <= cellBytes {
		return c
	}
	c = c[:cellBytes]
	for len(c) > 0 {
		if r, n := utf8.DecodeLastRuneInString(c); r != utf8.RuneError || n > 1 {
			break
		}
		c = c[:len(c)-1]
	}
	if strings.ContainsRune(c, 0x1b) {
		c += cReset
	}
	return c
}

// tableGrid copies the cells on the way in — they are the caller's slices, and a
// fit pass that edited them in place would change what the caller stores — and
// applies the memory guard where there is a width to apply it for.
func tableGrid(header []string, rows [][]string, cols, width int) [][]string {
	all := rows
	if header != nil {
		all = append([][]string{header}, rows...)
	}
	grid := make([][]string, len(all))
	for i, r := range all {
		grid[i] = make([]string, len(r))
		for j, c := range r {
			if width > 0 {
				c = capCell(c)
			}
			grid[i][j] = c
		}
	}
	return grid
}

// tableBounds is the table's own arithmetic: the natural width of each column, the
// floor each may be shaved to, and what a set of widths adds up to as a printed
// row. It is one function rather than three because the fit pass and the width
// suite both need these numbers, and a suite carrying its own copy of the rule
// asserts that the copy is self-consistent rather than that the allocator is
// right — which is how a fit pass that shaved past what shaving could buy stayed
// green.
func tableBounds(header []string, grid [][]string, cols int, lead string) (natural, floor []int, total func([]int) int) {
	natural = make([]int, cols)
	for _, r := range grid {
		for j, c := range r {
			natural[j] = max(natural[j], visibleWidth(c))
		}
	}
	floor = make([]int, cols)
	for j := range floor {
		h := 0
		if j < len(header) {
			h = visibleWidth(header[j])
		}
		floor[j] = min(natural[j], max(h, colFloor))
	}
	total = func(w []int) int {
		n := visibleWidth(lead) + tableGap*(cols-1)
		for _, x := range w {
			n += x
		}
		return n
	}
	return natural, floor, total
}

// tableSpan is the widest and the narrowest a table of these cells can be drawn:
// its natural printed width, and the width every flexible column at its floor adds
// up to. Between them the fit pass can land exactly on a budget; outside them it
// cannot, and does not try.
func tableSpan(header []string, rows [][]string, lead string) (natural, floor int) {
	cols := len(header)
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	if cols == 0 {
		return 0, 0
	}
	nat, flr, total := tableBounds(header, tableGrid(header, rows, cols, 1), cols, lead)
	return total(nat), total(flr)
}

// table prints rows as aligned columns (ANSI-aware widths), fitted to the width
// the chrome is drawn to. mid names the columns that cut in the MIDDLE — paths,
// model ids, commands, step names, where both ends carry meaning.
func table(header []string, rows [][]string, mid ...int) {
	// A bare table's two-column lead is INSIDE its room while a frame's gutter is
	// outside its width, so at the page the two end in the same column — but a table
	// is data and takes the whole window, so the budget is fitWidth() and not the
	// page. fitWidth() answers 0 where there is no terminal to fit to at all: the
	// rows are somebody's payload in a pipe or a file, and every cell goes through
	// whole.
	for _, l := range tableLines(header, rows, "  ", fitWidth(), mid...) {
		fmt.Println(l)
	}
}

// tableLines is table() as a value, so a panel can put the same aligned columns
// inside a frame instead of at the left margin. lead is the indent every row
// carries; width is the whole room the columns have to fit in, and 0 means there
// is no terminal to fit to and every cell is printed whole.
//
// The fit is a real allocation and not a cut of the last column: natural widths
// first, and if they fit, every cell is printed UNCHANGED — which is what keeps
// every table that fits today byte-identical, and what a wide terminal buys. If
// they do not fit, the widest column that can still afford it gives up one column
// at a time until they do, and only then is a cell ellipsized.
//
// And if the floors cannot reach the budget at all, NOTHING is shaved: the row
// overflows at its natural widths, which is what it did before this allocator
// existed. A shave that cannot land does not save a physical row, so its only
// effect is to spend the two cells that identify the row. See the gate below.
func tableLines(header []string, rows [][]string, lead string, width int, mid ...int) []string {
	cols := len(header)
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	if cols == 0 {
		return nil
	}
	grid := tableGrid(header, rows, cols, width)
	natural, floor, total := tableBounds(header, grid, cols, lead)
	fit := make([]int, cols)
	copy(fit, natural)
	if width > 0 && total(fit) > width {
		// Shave only when shaving can LAND. When even the floors cannot reach the
		// budget the row overflows either way, and a row that overflows with its step
		// name and its command intact is strictly better than one that overflows
		// having spent them: the terminal's own soft wrap keeps the bytes contiguous
		// and the operator can still read what the step is.
		//
		// This was pure loss at the 80-column floor, which is the width that was not
		// allowed to regress. A nine-column plan row shaved `harden-the-workflow-parser`
		// to `harde…parser` and a two-command check to twelve columns of noise, and the
		// row was still 84 columns against a budget of 77 — so it soft-wrapped to two
		// physical rows exactly as the unshaved 130-column row did. The shave bought
		// zero rows and cost the whole payload.
		if total(floor) <= width {
			for total(fit) > width {
				widest, at := 0, -1
				for j := range fit {
					if fit[j] > floor[j] && fit[j] > widest {
						widest, at = fit[j], j
					}
				}
				if at < 0 { // every flexible column is already at its floor
					break
				}
				fit[at]--
			}
		}
	}
	middle := make([]bool, cols)
	for _, j := range mid {
		if j >= 0 && j < cols {
			middle[j] = true
		}
	}
	buildRow := func(r []string, faintRow bool) string {
		var b strings.Builder
		b.WriteString(lead)
		for i, c := range r {
			if visibleWidth(c) > fit[i] {
				// both markers are measured with visibleWidth, so the ASCII tier's
				// three-column "..." does not shear the row it lands in
				if middle[i] {
					c = ellipsizeMiddle(stripANSI(c), fit[i])
				} else {
					c = ellipsize(stripANSI(c), fit[i])
				}
			}
			if i == len(r)-1 { // the last column is never padded: it ends the line
				b.WriteString(c)
				break
			}
			b.WriteString(padTo(c, fit[i], 0) + strings.Repeat(" ", tableGap))
		}
		if faintRow {
			return cFaint + stripANSI(b.String()) + cReset
		}
		return b.String()
	}
	var out []string
	if header != nil {
		out = append(out, buildRow(grid[0], true))
		grid = grid[1:]
	}
	for _, r := range grid {
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

// gaugeCellsInline is a bar that SHARES a line with other fields — the status
// line's. It is shorter than gaugeCells and it is still a constant, for the same
// reason: a bar whose length changed with the window would make two readings in
// one session incomparable, and a picture of a fraction is not a place to spend
// spare columns.
const gaugeCellsInline = 10

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

	// fixed is a width a caller pinned with at(), so a whole repaint is measured
	// against one snapshot of the terminal rather than one reading per line.
	fixed int
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
	// A row's value is a sentence, so it takes the frame's room up to the reading
	// measure and no further: past that the eye has to travel back across the whole
	// page to find the next line.
	room := min(panelRowRoom(), proseMax)
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
func (p *panel) Table(header []string, rows [][]string, width int, mid ...int) {
	// With no TERMINAL there is no width to fit to, so the rows go through whole —
	// which is what bareLines() promises for every other line in the panel and
	// could not keep for its table until the fit pass could be switched off. It is
	// hasScreen() and not Frames: a sober 80-column terminal has a width even
	// though it has no box, and its table has to be fitted to it or the rows lose
	// their columns to the terminal's own soft wrap.
	if !hasScreen() {
		width = 0
	}
	for _, l := range tableLines(header, rows, "", width, mid...) {
		p.lines = append(p.lines, panelLine{text: l})
	}
}

// nominalContent is the content width a panel lays out for: the page's content,
// or the terminal when it is narrower. Rows wrap against THIS and not against the
// panel's final width, because the final width depends on the rows — and a layout
// that depends on its own output cannot be reasoned about. The dependency runs one
// way: the rows are measured against the nominal width, and then the frame is
// drawn to the rows.
func nominalContent() int { return houseWidth() - 4 }

// panelFloor is the narrowest a frame is drawn when its own content did not ask
// for more. It is the 76-column house this look was designed in, so at a terminal
// of 80 — where houseWidth() is exactly 76 — the floor did not move and could not
// have.
//
// Above 80 it is what stops the fix from becoming the complaint. A panel holding
// six short label/value rows and grown to the page is a 161-column box whose
// longest row ends in column 62: 99 blank columns, padded and then closed with a
// ║, so the box measures the emptiness and the rule points straight at it. Text
// squeezed left inside a 161-column frame reads WORSE than the same text squeezed
// left inside a 76-column one. A frame full of window is only honest if its rows
// fill it — so the frame follows the rows, and where the rows do have something to
// say (a table, a path that cannot be broken) it still takes the whole page.
const panelFloor = 76

// ceiling is the widest this frame may be drawn. It is what a caller asks for
// BEFORE it has any content — a table needs a budget, and a budget that came from
// the table would be a layout that depends on its own output.
func (p *panel) ceiling() int {
	// ...unless a caller pinned one. A repainted region has to measure its frame,
	// its rows and its notes against ONE snapshot of the terminal, or a window
	// resized between two of those measurements shears the region it counted.
	if p.fixed > 0 {
		return p.fixed
	}
	w := houseWidth()
	// ...with the one exception Row() records: a value that cannot be broken gets
	// the room it needs if the TERMINAL has it. A 140-column terminal holding a
	// 76-column frame that shears a path has 64 columns going spare, and a frame
	// that gives way is better than an identifier that does. It is bounded by the
	// terminal as it always was, and by the page too: past pageMax a frame that kept
	// growing would be the complaint restated at the other end.
	if p.need+4 > w {
		w = max(w, min(min(termWidth()-2, pageMax), p.need+4))
	}
	return max(w, 12)
}

// room is the content width a caller lays a table out against: the ceiling's
// interior. The table comes back at its own natural widths whenever they fit, and
// width() then draws the frame to what the table actually measured — so a wide
// window buys a wider table and not a wider border.
func (p *panel) room() int { return max(p.ceiling()-4, 1) }

// contentWidth is the frame width this panel's content actually asks for: its
// widest row plus the two borders and their gutters, its widest division course,
// and its own title course — which is content too, because a frame narrower than
// its lintel would have fitTop cut the title of the box to fit the box.
func (p *panel) contentWidth() int {
	need := p.need + 4
	for _, l := range p.lines {
		if l.div {
			need = max(need, visibleWidth(gPanelML+gPanelH+" "+l.text+" "+gPanelMR))
			continue
		}
		need = max(need, visibleWidth(expandTabs(l.text))+4)
	}
	return max(need, visibleWidth(topPlain(p.title, p.detail, p.right, 1)))
}

// width is the frame width: what the content asks for, floored at panelFloor so a
// two-line panel is still a box, and ceilinged at the page.
//
// It used to be the page unconditionally, on the argument that a frame that grew
// to its content left two panels on one screen at two widths. That argument is
// answered instead by at(), which pins one width for a whole repaint — and paying
// for it with a box drawn around 121 blank columns was the wrong trade, because
// the emptiness is on every wide screen while two panels printed together are on
// one of them.
func (p *panel) width() int {
	if p.fixed > 0 {
		return p.fixed
	}
	top := p.ceiling()
	return max(min(top, max(p.contentWidth(), min(panelFloor, top))), 12)
}

// at pins the frame width for a whole repaint. The picker is the one caller: it
// takes a single houseWidth() reading and measures its frame, its rows, its notes
// and its legend against that number, so the line count the "\033[<n>A" walk-back
// uses and the widths that produced it cannot come from two different terminals —
// which is the shear. It also lets a test pin a width without an environment.
func (p *panel) at(w int) *panel { p.fixed = w; return p }

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

// takeCols is the longest PREFIX of s that fits n columns; lastCols is the
// longest suffix. A wide rune that would straddle the budget is left out rather
// than half-drawn, so the result may be one column narrower than asked — which is
// what padTo is for. Both want s plain: they are the cut half of a measure taken
// with visibleWidth, and every caller strips first.
func takeCols(s string, n int) string {
	if n <= 0 {
		return ""
	}
	col := 0
	for i, r := range s {
		w := runeWidth(r)
		if col+w > n {
			return s[:i]
		}
		col += w
	}
	return s
}

func lastCols(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	col := 0
	for i := len(r) - 1; i >= 0; i-- {
		w := runeWidth(r[i])
		if col+w > n {
			return string(r[i+1:])
		}
		col += w
	}
	return s
}

// ellipsize cuts s to n visible COLUMNS, ending with the tier's own marker.
//
// COLUMNS and not runes, which is what it counted. Everything that asks for a cut
// measured the cell with visibleWidth first — the table allocator, statusLine, the
// completion menu, a panel's title course — and a helper that answered in runes
// handed back up to twice the columns it was asked for. In a table that does not
// merely overflow, it SHEARS: every column but the last is padded to the width it
// was promised, padTo sees a cell already past that width and pads nothing, and
// the rest of the row slides right on that one line only. A CJK path in a
// /members row put `sandbox` in column 54 on the header row and column 77 on the
// row below it, so the reader could no longer tell which column a value was in.
// In statusLine it defeated the no-wrap invariant the editor's walk-back counts
// on, which erased the input line on every keystroke.
// clockOf is a wall-clock time for a reader at this terminal: the time alone
// while it is still today, and the date in front of it once it is not — a run
// that spans midnight, or a session reopened the next morning, otherwise reports
// 02:14 for something that happened yesterday.
func clockOf(t time.Time) string {
	if t.IsZero() {
		return gEllipsis
	}
	t = t.Local()
	ny, nm, nd := time.Now().Local().Date()
	if y, m, d := t.Date(); y == ny && m == nm && d == nd {
		return t.Format("15:04:05")
	}
	return t.Format("Jan 2 15:04")
}

func ellipsize(s string, n int) string {
	if visibleWidth(s) <= n || n < 2 {
		return s
	}
	// the marker is measured, not assumed to be one column: the ASCII tier
	// spells it "...", and three columns charged as one shears every row it
	// lands in.
	m := visibleWidth(gEllipsis)
	if n <= m {
		return takeCols(s, n)
	}
	return takeCols(s, n-m) + gEllipsis
}

// ellipsizeMiddle keeps both ends of long identifiers (paths, model ids), in
// columns for the same reason as ellipsize.
func ellipsizeMiddle(s string, n int) string {
	m := visibleWidth(gEllipsis)
	if visibleWidth(s) <= n || n < 4+m {
		return s
	}
	head := (n - m) / 2
	return takeCols(s, head) + gEllipsis + lastCols(s, n-m-head)
}

// prettyPath shows a path relative to root when inside it, else ~-shortened,
// middle-ellipsized to a sane width.
//
// "Sane" is now the page's and not a constant: max(60, …) so the 60 columns it
// has always had at a terminal of 80 cannot narrow, and houseWidth()-16 so a
// window with the room prints the path whole — 120 columns at 140, 144 at 200.
// Every identifier cap in the program is written this way, which is what makes it
// checkable that none of them moved at 80.
//
// With no terminal it is the 60-column floor and not the window's, for the reason
// sectionTo gives: a piped path may not come out at two lengths on two machines.
func prettyPath(p, root string) string {
	n := 60
	if hasScreen() {
		n = max(60, houseWidth()-16)
	}
	if root != "" {
		if rel, err := filepath.Rel(root, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return ellipsizeMiddle(rel, n)
		}
	}
	return ellipsizeMiddle(shortDir(p), n)
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
