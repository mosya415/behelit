package main

import (
	"fmt"
	"os"
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
const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[38;5;246m"      // --dim  #8c8c92
	cFaint  = "\033[38;5;240m"      // --faint #54545a (labels, rules, muted)
	cGreen  = "\033[38;5;114m"      // --green #6fdc8c
	cYellow = "\033[38;5;179m"      // --yellow #e3c873
	cRed    = "\033[38;5;167m"      // --red   #d8635b
	cCode   = "\033[38;5;73m"       // inline code / code blocks — muted teal, not a box
	cFgOff  = "\033[39m"            // reset foreground only (composes inside other styles)
	cBandBg = "\033[48;2;45;60;50m" // gray-green fill for a submitted prompt band

	// Berserk / Behelit theme — blood crimson on near-black.
	cBlood     = "\033[38;2;186;33;38m" // fill: brand crimson
	cBloodDark = "\033[38;2;92;14;16m"  // outline / 3D shadow (box-drawing)
)

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
		(r >= 0x1F000 && r <= 0x1FAFF), (r >= 0x2600 && r <= 0x27BF),
		(r >= 0x2B00 && r <= 0x2BFF):
		return 2
	default:
		return 1
	}
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

// Status glyphs, shared across the whole UI.
const (
	gUp      = "●" // ready / up / ok
	gPartial = "◐" // unknown / partial
	gDown    = "✕" // down / failed
	gNone    = "·" // none / muted
)

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

// clearScreen wipes the screen and scrollback and homes the cursor, so an
// interactive session starts from the top instead of wherever the cursor was.
// It emits nothing unless stdout is a real terminal (osTermWidth > 0), so piped
// or redirected output is never polluted with escape codes.
func clearScreen() {
	if osTermWidth() > 0 {
		fmt.Print("\033[3J\033[H\033[2J")
	}
}

// eyebrow prints a faint uppercase super-label with a leading hairline tick.
func eyebrow(s string) {
	fmt.Printf(" %s─ %s%s\n", cFaint, strings.ToUpper(s), cReset)
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

// toolLine prints a dim activity marker for an auto-running tool: uppercase tool
// name (chrome) with its verbatim argument (data).
func toolLine(name, arg string) {
	fmt.Printf(" %s%s %-9s%s %s\n", cFaint, gNone, strings.ToUpper(name), cReset, arg)
}

// toolInfo prints a faint informational outcome under a tool marker (read-only
// tools: counts). toolOK / toolErr print an action outcome with a status glyph.
func toolInfo(text string) { fmt.Printf("   %s→ %s%s\n", cFaint, text, cReset) }
func toolOK(text string)   { fmt.Printf("   %s%s%s %s\n", cGreen, gUp, cReset, text) }
func toolErr(text string)  { fmt.Printf("   %s%s%s %s\n", cRed, gDown, cReset, text) }

func warn(format string, a ...any) string {
	return cYellow + fmt.Sprintf(format, a...) + cReset
}

func faint(format string, a ...any) string {
	return cFaint + fmt.Sprintf(format, a...) + cReset
}
