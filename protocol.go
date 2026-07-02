package main

import (
	"regexp"
	"strings"
)

// The tool-call transport. Instead of trusting each model's native
// function-calling parser (--tool-call-parser is unreliable across quantized
// checkpoints), the model emits structured tags in plain text and we parse them
// ourselves. Tags are LINE-ANCHORED: an opening/closing tag must be the entire
// content of a line (after trimming). This is what makes it robust against code
// bodies that contain '<', '>' or quotes — a full-line "</search>" almost never
// collides with real source.
//
// Grammar (each tag on its own line):
//
//	<read_file path="rel/or/abs" lines="10-40"/>      (lines optional)
//	<grep pattern="regexp" path="dir"/>               (path optional)
//	<run_command>
//	git status
//	</run_command>
//	<write path="foo.go">
//	...full file content...
//	</write>
//	<edit path="foo.go">
//	<search>
//	...verbatim existing text...
//	</search>
//	<replace>
//	...new text...
//	</replace>
//	</edit>

type Block struct {
	Name    string
	Attr    map[string]string
	Body    string // raw inner text (write, run_command)
	Search  string // edit only
	Replace string // edit only
}

var (
	blockNames = map[string]bool{
		"read_file": true, "grep": true, "run_command": true,
		"write": true, "edit": true,
	}
	// Matches an opening tag line: <name ...attrs...>  or self-closing <name .../>
	reOpen = regexp.MustCompile(`^<([a-z_]+)((?:\s+[a-z_]+="[^"]*")*)\s*(/?)>$`)
	reAttr = regexp.MustCompile(`([a-z_]+)="([^"]*)"`)
)

// ParseBlocks scans assistant text and returns every well-formed tool block, in
// order. Malformed or unknown tags are ignored (the model's prose is left
// alone); if nothing parses, the caller treats the turn as a final answer.
func ParseBlocks(text string) []Block {
	lines := strings.Split(text, "\n")
	var blocks []Block
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		m := reOpen.FindStringSubmatch(trimmed)
		if m == nil || !blockNames[m[1]] {
			i++
			continue
		}
		name, attrs, selfClose := m[1], m[2], m[3] == "/"
		b := Block{Name: name, Attr: parseAttrs(attrs)}

		if selfClose {
			blocks = append(blocks, b)
			i++
			continue
		}

		// Find the matching closing tag line.
		closeTag := "</" + name + ">"
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) != closeTag {
			j++
		}
		if j >= len(lines) {
			// unterminated block — ignore and move on
			i++
			continue
		}
		body := strings.Join(lines[i+1:j], "\n")
		if name == "edit" {
			s, r, ok := parseEditBody(lines[i+1 : j])
			if !ok {
				i = j + 1
				continue
			}
			b.Search, b.Replace = s, r
		} else {
			b.Body = body
		}
		blocks = append(blocks, b)
		i = j + 1
	}
	return blocks
}

func parseAttrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range reAttr.FindAllStringSubmatch(s, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// parseEditBody extracts the verbatim <search>/<replace> sections, line-anchored.
func parseEditBody(lines []string) (search, replace string, ok bool) {
	search, si := extractSection(lines, "search")
	replace, ri := extractSection(lines, "replace")
	if si < 0 || ri < 0 {
		return "", "", false
	}
	return search, replace, true
}

// extractSection returns the inner text between a full-line <tag> and the next
// full-line </tag>, plus the index of the opening line (-1 if not found).
func extractSection(lines []string, tag string) (string, int) {
	open, close := "<"+tag+">", "</"+tag+">"
	start := -1
	for k, ln := range lines {
		if strings.TrimSpace(ln) == open {
			start = k
			break
		}
	}
	if start < 0 {
		return "", -1
	}
	for k := start + 1; k < len(lines); k++ {
		if strings.TrimSpace(lines[k]) == close {
			return strings.Join(lines[start+1:k], "\n"), start
		}
	}
	return "", -1
}
