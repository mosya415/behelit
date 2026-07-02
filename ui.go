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
	cDim    = "\033[38;5;246m" // --dim  #8c8c92
	cFaint  = "\033[38;5;240m" // --faint #54545a (labels, rules, muted)
	cGreen  = "\033[38;5;114m" // --green #6fdc8c
	cYellow = "\033[38;5;179m" // --yellow #e3c873
	cRed    = "\033[38;5;167m" // --red   #d8635b
)

// Status glyphs, shared across the whole UI.
const (
	gUp      = "●" // ready / up / ok
	gPartial = "◐" // unknown / partial
	gDown    = "✕" // down / failed
	gNone    = "·" // none / muted
)

// termWidth bounds the hairline width. Uses $COLUMNS when present (no cgo, no
// ioctl, nothing for a security reviewer to frown at), else a sane fixed width.
func termWidth() int {
	if c := strings.TrimSpace(os.Getenv("COLUMNS")); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n >= 40 {
			if n > 100 {
				n = 100
			}
			return n
		}
	}
	return 60
}

// hr prints a full-width hairline rule.
func hr() {
	fmt.Println(cFaint + strings.Repeat("─", termWidth()) + cReset)
}

// eyebrow prints a faint uppercase super-label with a leading hairline tick.
func eyebrow(s string) {
	fmt.Printf(" %s─ %s%s\n", cFaint, strings.ToUpper(s), cReset)
}

// title prints the big bold uppercase heading (terminal analog of the clamp title).
func title(s string) {
	fmt.Printf(" %s%s%s\n", cBold, strings.ToUpper(s), cReset)
}

// kv prints a key/value row: faint uppercase label, verbatim value.
func kv(label, value string) {
	fmt.Printf("  %s%-8s%s %s\n", cFaint, strings.ToUpper(label), cReset, value)
}

// contValue prints a continuation line aligned under the value column.
func contValue(value string) {
	fmt.Printf("  %-8s %s\n", "", value)
}

// ticket prints the top "ticket header" bar: left brand, right est/live, spread
// to the full width with a green LIVE dot.
func ticket(left, right string) {
	w := termWidth()
	// right is composed with a colored dot; measure its plain form for spacing.
	rightPlain := right
	pad := w - 1 - utf8.RuneCountInString(left) - utf8.RuneCountInString(rightPlain)
	if pad < 1 {
		pad = 1
	}
	coloredRight := strings.Replace(right, gUp, cGreen+gUp+cFaint, 1)
	fmt.Printf(" %s%s%s%s%s\n", cFaint, left, strings.Repeat(" ", pad), coloredRight, cReset)
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
