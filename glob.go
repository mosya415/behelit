package main

import (
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxGlobResults = 100

// globToRegexp compiles a glob to an anchored regexp over slash paths:
// "**/" any directory prefix (including none), "**" anything, "*" and "?"
// within one segment, "{a,b}" alternation, "[…]" classes passed through.
func globToRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	depth := 0
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '{':
			depth++
			b.WriteString("(?:")
		case '}':
			if depth > 0 {
				depth--
				b.WriteString(")")
			} else {
				b.WriteString(`\}`)
			}
		case ',':
			if depth > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		case '[':
			j := strings.IndexByte(glob[i:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := glob[i+1 : i+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// matchInclude matches a file name against a grep include glob.
func matchInclude(include, name string) bool {
	re, err := globToRegexp(include)
	return err == nil && re.MatchString(name)
}

// globFiles returns files under path matching pattern, newest first, capped. A
// pattern without "/" matches the base name at any depth (like ripgrep --glob).
func globFiles(j *Jail, pattern, path string) string {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	if pattern == "" {
		return "error: pattern is required"
	}
	re, err := globToRegexp(pattern)
	if err != nil {
		return "error: bad glob: " + err.Error()
	}
	baseOnly := !strings.Contains(pattern, "/")
	root, err := j.Resolve(orDot(path))
	if err != nil {
		return "error: " + err.Error()
	}
	if info, err := os.Stat(root); err != nil {
		return "error: " + err.Error()
	} else if !info.IsDir() {
		return "error: glob path must be a directory: " + path
	}

	type hit struct {
		rel   string
		mtime time.Time
	}
	var hits []hit
	scanned := 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if scanned++; scanned > 200_000 {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if p != root && (d.Name() == ".git" || heavyDir[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		subject := rel
		if baseOnly {
			subject = d.Name()
		}
		if re.MatchString(subject) {
			info, err := d.Info()
			if err != nil {
				return nil
			}
			jrel, _ := filepath.Rel(j.Root, p)
			hits = append(hits, hit{filepath.ToSlash(jrel), info.ModTime()})
		}
		return nil
	})
	if len(hits) == 0 {
		return "no files found"
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].mtime.After(hits[b].mtime) })
	var out strings.Builder
	for i, h := range hits {
		if i == maxGlobResults {
			fmt.Fprintf(&out, "(results truncated: showing first %d of %d — use a more specific path or pattern)\n", maxGlobResults, len(hits))
			break
		}
		out.WriteString(h.rel + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

var (
	reDropBlocks = regexp.MustCompile(`(?is)<(script|style|noscript|iframe|svg|head)\b.*?</(script|style|noscript|iframe|svg|head)>`)
	reBlockTags  = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|h[1-6]|tr|table|section|article|pre|blockquote|header|footer|nav)\b[^>]*>`)
	reTags       = regexp.MustCompile(`(?s)<[^>]+>`)
	reBlankRuns  = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
	reSpaceRuns  = regexp.MustCompile(`[ \t]+`)
)

// htmlToText is a dependency-free HTML → readable text reduction: drop
// script/style blocks, turn block tags into newlines, strip the rest, unescape
// entities and squeeze whitespace.
func htmlToText(s string) string {
	s = reDropBlocks.ReplaceAllString(s, "")
	s = reBlockTags.ReplaceAllString(s, "\n")
	s = reTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpaceRuns.ReplaceAllString(s, " ")
	s = reBlankRuns.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
