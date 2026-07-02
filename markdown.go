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

// renderMarkdownLine formats a single line (already right-trimmed). Leading
// indentation is preserved so nested lists keep their shape.
func renderMarkdownLine(line string) string {
	lead := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	body := strings.TrimLeft(line, " \t")

	switch {
	case reHr.MatchString(body):
		return lead + cFaint + strings.Repeat("─", 24) + cReset
	case reHeading.MatchString(body):
		m := reHeading.FindStringSubmatch(body)
		return lead + cBold + renderInline(m[2]) + cReset
	case reOrdered.MatchString(body):
		m := reOrdered.FindStringSubmatch(body)
		return lead + cFaint + m[1] + "." + cReset + " " + renderInline(m[2])
	case reList.MatchString(body):
		m := reList.FindStringSubmatch(body)
		return lead + cFaint + "•" + cReset + " " + renderInline(m[1])
	case reQuote.MatchString(body):
		m := reQuote.FindStringSubmatch(body)
		return lead + cFaint + "▏ " + cReset + renderInline(m[1])
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
			b.WriteString("\033[7m" + seg + "\033[27m") // reverse video = inline code
		} else {
			b.WriteString(inlineEmph(seg))
		}
	}
	return b.String()
}

func inlineEmph(s string) string {
	s = renderMathSpans(s)
	s = reBold.ReplaceAllString(s, "\033[1m$1\033[22m")   // **bold**
	s = reItalic.ReplaceAllString(s, "\033[3m$1\033[23m") // *italic*
	return s
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
