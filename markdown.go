package main

import (
	"regexp"
	"strings"
)

// A small, dependency-free Markdown renderer for the terminal, in the project's
// monochrome aesthetic: emphasis via bold/italic (brightness, not color), inline
// code as reverse video, headings bold, lists with a • bullet, block quotes with
// a faint bar, and inline math ($...$) rendered via latexToUnicode. It works on
// one already-de-fenced line at a time; fenced code blocks are handled by the
// caller (proseWriter), which prints them verbatim.
//
// Emphasis is deliberately limited to ** (bold) and * (italic); underscores are
// left literal because they are common in code identifiers (snake_case, dunder).

var (
	reHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reList    = regexp.MustCompile(`^[-*+]\s+(.*)$`)
	reOrdered = regexp.MustCompile(`^(\d+)[.)]\s+(.*)$`)
	reQuote   = regexp.MustCompile(`^>\s?(.*)$`)
	reHr      = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)

	reBold        = regexp.MustCompile(`\*\*([^*]+?)\*\*`)
	reItalic      = regexp.MustCompile(`\*([^*\s][^*]*?)\*`)
	reDisplayMath = regexp.MustCompile(`\$\$(.+?)\$\$`)
	reInlineMath  = regexp.MustCompile(`\$([^$]+?)\$`)
)

// mdRuleMax is the `---` rule's length where there is no terminal to measure: the
// length it has always had in a redirected answer, kept as a constant so those
// bytes stay the same bytes whatever window the run happened in.
const mdRuleMax = 24

// renderMarkdownLine formats a single line (already right-trimmed). Leading
// indentation is preserved so nested lists keep their shape.
func renderMarkdownLine(line string) string {
	lead := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	body := strings.TrimLeft(line, " \t")

	switch {
	case reHr.MatchString(body):
		// A `---` between two paragraphs belongs to the paragraphs: on a screen it is
		// drawn to the measure they are wrapped to, and not to a number from nowhere.
		//
		// ONLY on a screen. This runs before the prose writer decides anything, so
		// with no gate `lca -p "…" > notes.md` wrote 77 dashes at COLUMNS=80 and 88 at
		// COLUMNS=140 — two runs of the same prompt into the same file producing
		// different bytes because of the window the operator happened to have open.
		// A redirected answer is the payload, and its bytes may not depend on the
		// terminal any more than they may carry an escape.
		w := mdRuleMax
		if hasScreen() {
			w = proseWidth()
		}
		return lead + cFaint + strings.Repeat(gRule, w) + cReset
	case reHeading.MatchString(body):
		m := reHeading.FindStringSubmatch(body)
		return lead + cBold + renderInline(m[2]) + cReset
	case reOrdered.MatchString(body):
		m := reOrdered.FindStringSubmatch(body)
		return lead + cFaint + m[1] + "." + cReset + " " + renderInline(m[2])
	case reList.MatchString(body):
		m := reList.FindStringSubmatch(body)
		return lead + cFaint + gBullet + cReset + " " + renderInline(m[1])
	case reQuote.MatchString(body):
		m := reQuote.FindStringSubmatch(body)
		return lead + cFaint + gQuote + " " + cReset + renderInline(m[1])
	default:
		return lead + renderInline(body)
	}
}

// renderInline applies inline formatting. Inline code spans are extracted first
// so their contents are not treated as emphasis or math.
func renderInline(s string) string {
	if strings.Count(s, "`")%2 != 0 {
		return inlineEmph(s) // unbalanced backticks → treat as literal text
	}
	parts := strings.Split(s, "`")
	var b strings.Builder
	for i, seg := range parts {
		if i%2 == 1 {
			b.WriteString(cCode + seg + cFgOff) // inline code = muted teal, no box
		} else {
			b.WriteString(inlineEmph(seg))
		}
	}
	return b.String()
}

func inlineEmph(s string) string {
	s = renderMathSpans(s)
	// the *off* codes, not cReset: emphasis inside a coloured line must give
	// the colour back, not end it.
	s = reBold.ReplaceAllString(s, cBold+"$1"+cBoldOff)
	s = reItalic.ReplaceAllString(s, cItalic+"$1"+cItalicOff)
	return s
}

// --- Tables ---------------------------------------------------------------
//
// A pipe table is a header row, a separator row (dashes/colons), then data rows.
// We render it as aligned columns with a hairline under the header — no vertical
// bars, monochrome.

func isTableRow(t string) bool {
	return strings.Contains(t, "|") && (strings.HasPrefix(t, "|") || strings.Count(t, "|") >= 2)
}

func isTableSeparator(t string) bool {
	cells := parseCells(t)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' && r != ':' && r != ' ' {
				return false
			}
		}
	}
	return true
}

func parseCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	parts := strings.Split(row, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseAligns(sep string) []int { // 0 left, 1 right, 2 center
	cells := parseCells(sep)
	a := make([]int, len(cells))
	for i, c := range cells {
		l, r := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case l && r:
			a[i] = 2
		case r:
			a[i] = 1
		}
	}
	return a
}

// renderMarkdownTable renders buffered table rows into aligned display lines. If
// the block is not actually a table (no separator row), each line is rendered as
// ordinary Markdown instead.
func renderMarkdownTable(rows []string) []string {
	if len(rows) < 2 || !isTableSeparator(rows[1]) {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = renderMarkdownLine(r)
		}
		return out
	}

	header := parseCells(rows[0])
	aligns := parseAligns(rows[1])
	var data [][]string
	for _, r := range rows[2:] {
		data = append(data, parseCells(r))
	}

	ncols := len(header)
	for _, d := range data {
		if len(d) > ncols {
			ncols = len(d)
		}
	}

	cell := func(cells []string, j int) string {
		if j < len(cells) {
			return renderInline(cells[j])
		}
		return ""
	}
	align := func(j int) int {
		if j < len(aligns) {
			return aligns[j]
		}
		return 0
	}

	width := make([]int, ncols)
	rHeader := make([]string, ncols)
	for j := 0; j < ncols; j++ {
		rHeader[j] = cell(header, j)
		width[j] = visibleWidth(rHeader[j])
	}
	rData := make([][]string, len(data))
	for i, d := range data {
		rData[i] = make([]string, ncols)
		for j := 0; j < ncols; j++ {
			rData[i][j] = cell(d, j)
			if w := visibleWidth(rData[i][j]); w > width[j] {
				width[j] = w
			}
		}
	}

	var out []string
	hc := make([]string, ncols)
	total := 0
	for j := 0; j < ncols; j++ {
		hc[j] = cBold + padTo(rHeader[j], width[j], align(j)) + cBoldOff
		total += width[j]
	}
	total += 2 * (ncols - 1)
	out = append(out, strings.Join(hc, "  "))
	// The hairline underlines the header, so it is the width of the table AS
	// RENDERED and nothing else. Bounded by the page instead, it was 76 columns
	// under a 126-column header row on an 80-column terminal: a rule that stops
	// short of the thing it underlines reads as a rendering fault, and its length
	// changed with $COLUMNS in a redirected answer. The table's cells are the
	// model's data and are neither reflowed nor cut, so when the table overflows the
	// terminal its own rule overflows with it — consistently, which is the readable
	// failure.
	out = append(out, cFaint+strings.Repeat(gRule, total)+cReset)
	for _, d := range rData {
		for j := 0; j < ncols; j++ {
			d[j] = padTo(d[j], width[j], align(j))
		}
		out = append(out, strings.Join(d, "  "))
	}
	return out
}

func renderMathSpans(s string) string {
	s = reDisplayMath.ReplaceAllStringFunc(s, func(m string) string {
		return latexToUnicode(reDisplayMath.FindStringSubmatch(m)[1])
	})
	s = reInlineMath.ReplaceAllStringFunc(s, func(m string) string {
		inner := reInlineMath.FindStringSubmatch(m)[1]
		if looksMath(inner) {
			return latexToUnicode(inner)
		}
		return m // not math ($5 and $10) — leave untouched
	})
	return s
}
