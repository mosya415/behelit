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
//	<glob pattern="**/*.go"/>  <skill name="x"/>  <webfetch url="…"/>
//	<todowrite>[…json…]</todowrite>
//	<task agent="explore" description="…">prompt</task>
//
// The tag set mirrors the tool registry (toolset.go): a tool with a Body param
// is a block tag, one without is void.

type Block struct {
	Name    string
	Attr    map[string]string
	Body    string // raw inner text (write, run_command)
	Search  string // edit only
	Replace string // edit only
}

var (
	blockNames = map[string]bool{
		"read_file": true, "grep": true, "list_dir": true, "glob": true,
		"run_command": true, "write": true, "edit": true,
		"todowrite": true, "task": true, "delegate": true, "skill": true, "webfetch": true,
	}
	// voidTools have no body and are semantically self-closing; we accept them
	// even when a model forgets the trailing "/" (a common dirty-output case).
	voidTools = map[string]bool{"read_file": true, "grep": true, "list_dir": true, "glob": true, "skill": true, "webfetch": true}

	// Matches an opening tag line: <name ...attrs...>  or self-closing <name .../>.
	// Attribute values may be double- OR single-quoted, with optional spaces
	// around "=", to tolerate models that don't emit the canonical form.
	reOpen = regexp.MustCompile(`^<([a-z_]+)((?:\s+[a-z_]+\s*=\s*(?:"[^"]*"|'[^']*'))*)\s*(/?)>$`)
	reAttr = regexp.MustCompile(`([a-z_]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

	// Reasoning tags to strip: <think>, <thinking>, and model-namespaced variants
	// like <mm:think> (MiniMax) or <reasoning>.
	reReasonTag   = regexp.MustCompile(`(?i)</?(?:[a-z0-9_]+:)?(?:think(?:ing)?|reason(?:ing)?)>`)
	reReasonOpen  = regexp.MustCompile(`(?i)<(?:[a-z0-9_]+:)?(?:think(?:ing)?|reason(?:ing)?)>`)
	reReasonClose = regexp.MustCompile(`(?i)</(?:[a-z0-9_]+:)?(?:think(?:ing)?|reason(?:ing)?)>`)

	// A tool (or edit sub-) tag anywhere in the text — used only to re-separate
	// tags that a model glued to surrounding text (it is NOT the block grammar).
	toolTag      = `</?(?:read_file|grep|list_dir|glob|run_command|write|edit|search|replace|todowrite|task|delegate|skill|webfetch)(?:\s+[a-z_]+\s*=\s*(?:"[^"]*"|'[^']*'))*\s*/?>`
	reGlueBefore = regexp.MustCompile(`([^\n])(` + toolTag + `)`)
	reGlueAfter  = regexp.MustCompile(`(` + toolTag + `)([^\n])`)
)

// normalizeTags makes the line-anchored grammar tolerant of models that don't
// put tags on their own line — MiniMax-M3, for instance, emits
// "</mm:think><run_command>" and "cmd</run_command>". We drop reasoning tags and
// break any tool tag that is glued to neighbouring text onto its own line. Tags
// already alone on a line are left untouched, so well-formed <write>/<edit>
// bodies keep their exact content.
func normalizeTags(text string) string {
	return splitGluedTools(reReasonTag.ReplaceAllString(text, "\n"))
}

// splitGluedTools breaks any tool tag glued to neighbouring text onto its own
// line, leaving tags that are already alone untouched. Used by the parser and
// the display so both see the same structure.
func splitGluedTools(text string) string {
	for i := 0; i < 4; i++ {
		before := text
		text = reGlueBefore.ReplaceAllString(text, "$1\n$2")
		text = reGlueAfter.ReplaceAllString(text, "$1\n$2")
		if text == before {
			break
		}
	}
	return text
}

// ParseBlocks scans assistant text and returns every well-formed tool block, in
// order. Malformed or unknown tags are ignored (the model's prose is left
// alone); if nothing parses, the caller treats the turn as a final answer.
func ParseBlocks(text string) []Block {
	lines := strings.Split(normalizeTags(text), "\n")
	var blocks []Block
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		m := reOpen.FindStringSubmatch(trimmed)
		if m == nil || !blockNames[m[1]] {
			i++
			continue
		}
		name, attrs, selfClose := m[1], m[2], m[3] == "/" || voidTools[m[1]]
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
		out[m[1]] = m[2] + m[3] // exactly one of the (double|single)-quoted groups is set
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
