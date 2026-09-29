package main

import (
	"errors"
	"regexp"
	"strings"
)

// Match strategies for edit, ported from opencode's tool/edit.ts. Open models
// often get whitespace, indentation or escaping slightly wrong; each strategy
// proposes candidate substrings that literally exist in the file, and a
// candidate is used only if it occurs exactly once (unless replace_all). The
// exact match is always tried first, so a correct edit behaves exactly as the
// strict apply layer did.

type replacer struct {
	name string
	find func(content, find string) []string
}

var replacers = []replacer{
	{"exact", simpleReplacer},
	{"line-trimmed", lineTrimmedReplacer},
	{"block-anchor", blockAnchorReplacer},
	{"whitespace-normalized", whitespaceNormalizedReplacer},
	{"indentation-flexible", indentationFlexibleReplacer},
	{"escape-normalized", escapeNormalizedReplacer},
	{"trimmed-boundary", trimmedBoundaryReplacer},
	{"context-aware", contextAwareReplacer},
}

var (
	errEditNotFound  = errors.New("could not find old_string in the file — it must match the file's text, including whitespace and indentation. Re-read the file and copy the exact lines")
	errEditAmbiguous = errors.New("found multiple matches for old_string — add surrounding lines to make it unique, or set replace_all")
	errEditTooBroad  = errors.New("refusing replacement: the matched span is much larger than old_string. Re-read the file and provide the exact text")
)

// fuzzyReplace applies the first strategy that yields a unique literal match.
// It returns the new content and the strategy used.
func fuzzyReplace(content, oldS, newS string, replaceAll bool) (string, string, error) {
	notFound := true
	for _, r := range replacers {
		for _, search := range r.find(content, oldS) {
			idx := strings.Index(content, search)
			if idx < 0 || search == "" {
				continue
			}
			notFound = false
			if disproportionate(search, oldS) {
				return "", "", errEditTooBroad
			}
			if replaceAll {
				return strings.ReplaceAll(content, search, newS), r.name, nil
			}
			if idx != strings.LastIndex(content, search) {
				continue // ambiguous — try the next candidate / strategy
			}
			return content[:idx] + newS + content[idx+len(search):], r.name, nil
		}
	}
	if notFound {
		return "", "", errEditNotFound
	}
	return "", "", errEditAmbiguous
}

func disproportionate(search, old string) bool {
	sl, ol := len(strings.Split(search, "\n")), len(strings.Split(old, "\n"))
	if sl >= max(ol+3, ol*2) {
		return true
	}
	if ol == 1 {
		return false
	}
	st, ot := len(strings.TrimSpace(search)), len(strings.TrimSpace(old))
	return st > max(ot+500, ot*4)
}

func simpleReplacer(_, find string) []string { return []string{find} }

// lineOffsets returns the byte offset of each line start (split on "\n").
func lineOffsets(lines []string) []int {
	off := make([]int, len(lines)+1)
	for i, l := range lines {
		off[i+1] = off[i] + len(l) + 1
	}
	return off
}

// span returns content[start line .. end line] inclusive, without the final newline.
func span(content string, off []int, start, end int) string {
	return content[off[start] : off[end+1]-1]
}

func dropTrailingEmpty(lines []string) []string {
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}
	return lines
}

func lineTrimmedReplacer(content, find string) []string {
	orig := strings.Split(content, "\n")
	search := dropTrailingEmpty(strings.Split(find, "\n"))
	if len(search) == 0 {
		return nil
	}
	off := lineOffsets(orig)
	var out []string
	for i := 0; i+len(search) <= len(orig); i++ {
		ok := true
		for j := range search {
			if strings.TrimSpace(orig[i+j]) != strings.TrimSpace(search[j]) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, span(content, off, i, i+len(search)-1))
		}
	}
	return out
}

const (
	singleCandidateSimilarity    = 0.65
	multipleCandidatesSimilarity = 0.65
)

// blockAnchorReplacer matches a block by its first and last lines (trimmed),
// scoring the middle lines by Levenshtein similarity.
func blockAnchorReplacer(content, find string) []string {
	orig := strings.Split(content, "\n")
	search := strings.Split(find, "\n")
	if len(search) < 3 {
		return nil
	}
	search = dropTrailingEmpty(search)
	if len(search) < 3 {
		return nil
	}
	first, last := strings.TrimSpace(search[0]), strings.TrimSpace(search[len(search)-1])
	size := len(search)
	maxDelta := max(1, size/4)

	type cand struct{ start, end int }
	var cands []cand
	for i := range orig {
		if strings.TrimSpace(orig[i]) != first {
			continue
		}
		for j := i + 2; j < len(orig); j++ {
			if strings.TrimSpace(orig[j]) == last {
				if abs(j-i+1-size) <= maxDelta {
					cands = append(cands, cand{i, j})
				}
				break
			}
		}
	}
	if len(cands) == 0 {
		return nil
	}
	off := lineOffsets(orig)
	similarity := func(c cand) float64 {
		actual := c.end - c.start + 1
		toCheck := min(size-2, actual-2)
		if toCheck <= 0 {
			return 1
		}
		sum := 0.0
		for j := 1; j < size-1 && j < actual-1; j++ {
			a, b := strings.TrimSpace(orig[c.start+j]), strings.TrimSpace(search[j])
			m := max(len(a), len(b))
			if m == 0 {
				continue
			}
			sum += 1 - float64(levenshtein(a, b))/float64(m)
		}
		return sum / float64(toCheck)
	}
	if len(cands) == 1 {
		if similarity(cands[0]) >= singleCandidateSimilarity {
			return []string{span(content, off, cands[0].start, cands[0].end)}
		}
		return nil
	}
	best, bestSim := cand{-1, -1}, -1.0
	for _, c := range cands {
		if s := similarity(c); s > bestSim {
			best, bestSim = c, s
		}
	}
	if bestSim >= multipleCandidatesSimilarity && best.start >= 0 {
		return []string{span(content, off, best.start, best.end)}
	}
	return nil
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

var reWS = regexp.MustCompile(`\s+`)

func normWS(s string) string { return strings.TrimSpace(reWS.ReplaceAllString(s, " ")) }

func whitespaceNormalizedReplacer(content, find string) []string {
	nf := normWS(find)
	if nf == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	var out []string
	for _, line := range lines {
		nl := normWS(line)
		if nl == nf {
			out = append(out, line)
		} else if strings.Contains(nl, nf) {
			words := strings.Fields(find)
			for i, w := range words {
				words[i] = regexp.QuoteMeta(w)
			}
			if re, err := regexp.Compile(strings.Join(words, `\s+`)); err == nil {
				if m := re.FindString(line); m != "" {
					out = append(out, m)
				}
			}
		}
	}
	findLines := strings.Split(find, "\n")
	if len(findLines) > 1 {
		for i := 0; i+len(findLines) <= len(lines); i++ {
			block := strings.Join(lines[i:i+len(findLines)], "\n")
			if normWS(block) == nf {
				out = append(out, block)
			}
		}
	}
	return out
}

var reLeadWS = regexp.MustCompile(`^\s*`)

func removeIndent(text string) string {
	lines := strings.Split(text, "\n")
	minIndent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n := len(reLeadWS.FindString(l)); minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	if minIndent <= 0 {
		return text
	}
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			lines[i] = l[minIndent:]
		}
	}
	return strings.Join(lines, "\n")
}

func indentationFlexibleReplacer(content, find string) []string {
	target := removeIndent(find)
	lines := strings.Split(content, "\n")
	n := len(strings.Split(find, "\n"))
	var out []string
	for i := 0; i+n <= len(lines); i++ {
		block := strings.Join(lines[i:i+n], "\n")
		if removeIndent(block) == target {
			out = append(out, block)
		}
	}
	return out
}

var reEscape = regexp.MustCompile("\\\\(n|t|r|'|\"|`|\\\\|\n|\\$)")

func unescapeString(s string) string {
	return reEscape.ReplaceAllStringFunc(s, func(m string) string {
		switch m[1:] {
		case "n":
			return "\n"
		case "t":
			return "\t"
		case "r":
			return "\r"
		case "\n":
			return "\n"
		default:
			return m[1:]
		}
	})
}

func escapeNormalizedReplacer(content, find string) []string {
	u := unescapeString(find)
	var out []string
	if strings.Contains(content, u) {
		out = append(out, u)
	}
	lines := strings.Split(content, "\n")
	n := len(strings.Split(u, "\n"))
	for i := 0; i+n <= len(lines); i++ {
		block := strings.Join(lines[i:i+n], "\n")
		if unescapeString(block) == u {
			out = append(out, block)
		}
	}
	return out
}

func trimmedBoundaryReplacer(content, find string) []string {
	t := strings.TrimSpace(find)
	if t == find || t == "" {
		return nil
	}
	var out []string
	if strings.Contains(content, t) {
		out = append(out, t)
	}
	lines := strings.Split(content, "\n")
	n := len(strings.Split(find, "\n"))
	for i := 0; i+n <= len(lines); i++ {
		block := strings.Join(lines[i:i+n], "\n")
		if strings.TrimSpace(block) == t {
			out = append(out, block)
		}
	}
	return out
}

// contextAwareReplacer anchors on first/last lines and requires at least half
// of the non-empty middle lines to match exactly (trimmed).
func contextAwareReplacer(content, find string) []string {
	findLines := strings.Split(find, "\n")
	if len(findLines) < 3 {
		return nil
	}
	findLines = dropTrailingEmpty(findLines)
	if len(findLines) < 3 {
		return nil
	}
	lines := strings.Split(content, "\n")
	off := lineOffsets(lines)
	first, last := strings.TrimSpace(findLines[0]), strings.TrimSpace(findLines[len(findLines)-1])
	var out []string
	for i := range lines {
		if strings.TrimSpace(lines[i]) != first {
			continue
		}
		for j := i + 2; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) != last {
				continue
			}
			if j-i+1 == len(findLines) {
				matching, total := 0, 0
				for k := 1; k < len(findLines)-1; k++ {
					a, b := strings.TrimSpace(lines[i+k]), strings.TrimSpace(findLines[k])
					if a == "" && b == "" {
						continue
					}
					total++
					if a == b {
						matching++
					}
				}
				if total == 0 || float64(matching)/float64(total) >= 0.5 {
					out = append(out, span(content, off, i, j))
				}
			}
			break
		}
	}
	return out
}
