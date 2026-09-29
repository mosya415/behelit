package main

import (
	"strings"
)

// A deliberately small YAML subset for markdown frontmatter (agents, commands,
// skills) — enough for the formats opencode and Claude Code use, with no
// dependency: nested maps by indentation, "- item" lists, inline [a, b] lists,
// quoted scalars, # comments. Key order is preserved (it matters for
// permission precedence).

type yNode struct {
	Key      string
	Value    string   // scalar value ("" for maps/lists)
	List     []string // "- item" or [a, b] entries
	Children []*yNode // nested map, in order
}

func (n *yNode) child(key string) *yNode {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Key == key {
			return c
		}
	}
	return nil
}

func (n *yNode) str(key string) string {
	if c := n.child(key); c != nil {
		return c.Value
	}
	return ""
}

// splitFrontmatter separates "---\n…\n---\n" from the body. ok=false when the
// document has no frontmatter (the whole text is the body).
func splitFrontmatter(doc string) (meta *yNode, body string, ok bool) {
	doc = strings.TrimPrefix(doc, "\ufeff")
	norm := strings.ReplaceAll(doc, "\r\n", "\n")
	if !strings.HasPrefix(norm, "---\n") {
		return &yNode{}, doc, false
	}
	rest := norm[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return &yNode{}, doc, false
	}
	head := rest[:end]
	body = rest[end+4:]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	return parseYAMLish(head), strings.TrimSpace(body), true
}

func parseYAMLish(text string) *yNode {
	root := &yNode{}
	type frame struct {
		indent int
		node   *yNode
	}
	stack := []frame{{-1, root}}
	var last *yNode
	lastIndent := -1
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for li := 0; li < len(lines); li++ {
		raw := lines[li]
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		t := strings.TrimSpace(line)

		if strings.HasPrefix(t, "- ") || t == "-" {
			if last != nil && indent >= lastIndent {
				last.List = append(last.List, unquote(strings.TrimSpace(strings.TrimPrefix(t, "-"))))
			}
			continue
		}
		colon := keyColon(t)
		if colon < 0 {
			continue
		}
		key := unquote(strings.TrimSpace(t[:colon]))
		val := strings.TrimSpace(t[colon+1:])

		for len(stack) > 1 && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].node
		n := &yNode{Key: key}
		switch {
		case strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]"):
			for _, item := range strings.Split(val[1:len(val)-1], ",") {
				if item = unquote(strings.TrimSpace(item)); item != "" {
					n.List = append(n.List, item)
				}
			}
		case strings.HasPrefix(val, "{") && strings.HasSuffix(val, "}"):
			// flow mapping: {key: value, key2: value2} (flat)
			for _, kv := range strings.Split(val[1:len(val)-1], ",") {
				kv = strings.TrimSpace(kv)
				if c := keyColon(kv); c > 0 {
					n.Children = append(n.Children, &yNode{Key: unquote(strings.TrimSpace(kv[:c])), Value: unquote(strings.TrimSpace(kv[c+1:]))})
				}
			}
		case val == "|" || val == "|-" || val == ">" || val == ">-":
			// Block scalar: the following lines indented deeper than the key,
			// taken verbatim (comments included — '#' is text here).
			var body []string
			for li+1 < len(lines) {
				next := lines[li+1]
				if strings.TrimSpace(next) != "" && len(next)-len(strings.TrimLeft(next, " \t")) <= indent {
					break
				}
				body = append(body, next)
				li++
			}
			n.Value = blockScalar(body, val[0] == '>')
		default:
			n.Value = unquote(val)
		}
		parent.Children = append(parent.Children, n)
		if val == "" {
			stack = append(stack, frame{indent, n})
		}
		last, lastIndent = n, indent
	}
	return root
}

// blockScalar dedents a block's lines; folded (>) joins them with spaces.
func blockScalar(lines []string, folded bool) string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	minIndent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " \t")); minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) >= minIndent && minIndent > 0 {
			out[i] = l[minIndent:]
		} else {
			out[i] = strings.TrimLeft(l, " \t")
		}
	}
	if folded {
		return strings.Join(strings.Fields(strings.Join(out, " ")), " ")
	}
	return strings.Join(out, "\n")
}

// keyColon finds the ':' separating a key from its value, skipping colons
// inside a quoted key ("git push *": deny).
func keyColon(t string) int {
	if len(t) > 0 && (t[0] == '"' || t[0] == '\'') {
		if end := strings.IndexByte(t[1:], t[0]); end >= 0 {
			if i := strings.IndexByte(t[end+2:], ':'); i >= 0 {
				return end + 2 + i
			}
		}
		return -1
	}
	for i := 0; i < len(t); i++ {
		if t[i] == ':' && (i+1 == len(t) || t[i+1] == ' ' || t[i+1] == '\t') {
			return i
		}
	}
	return -1
}

func stripComment(line string) string {
	inQ := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQ != 0:
			if c == inQ {
				inQ = 0
			}
		case c == '"' || c == '\'':
			inQ = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
