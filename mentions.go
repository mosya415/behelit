package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// @file mentions. Typing "@path" in a prompt attaches that file's contents so
// the model has it immediately, without spending a tool round-trip on read_file.
// The line editor completes "@" against files in the jail (mentions completion),
// and expandMentions injects the referenced files at submit time.

// heavyDir names are skipped when listing files for @-completion.
var heavyDir = map[string]bool{
	"node_modules": true, "vendor": true, ".git": true, "dist": true,
	"target": true, "build": true, "__pycache__": true, ".venv": true,
}

// jailFiles lists files under the jail root whose relative path contains frag
// (case-insensitive), prefix matches first, capped — for @-mention completion.
func jailFiles(jail *Jail, frag string) []string {
	frag = strings.ToLower(frag)
	var out []string
	visited := 0
	filepath.WalkDir(jail.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if visited > 8000 {
			return filepath.SkipAll
		}
		visited++
		if d.IsDir() {
			if p != jail.Root && (strings.HasPrefix(d.Name(), ".") || heavyDir[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(jail.Root, p)
		if err != nil {
			return nil
		}
		if frag == "" || strings.Contains(strings.ToLower(rel), frag) {
			out = append(out, rel)
			if len(out) >= 400 {
				return filepath.SkipAll
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		pi := strings.HasPrefix(strings.ToLower(out[i]), frag)
		pj := strings.HasPrefix(strings.ToLower(out[j]), frag)
		if pi != pj {
			return pi // prefix matches first
		}
		return len(out[i]) < len(out[j]) // then shortest path
	})
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

var reMention = regexp.MustCompile(`(^|\s)@([^\s]+)`)

// expandMentions reads every @file referenced in line that resolves to a real
// file inside the jail, returning injectable <file> blocks and the names
// attached. Tokens that don't resolve to a readable file are left untouched, so
// stray @handles in prose are harmless.
// mentionRead is one attached file: the path as it was written, and whether the
// block carries the WHOLE file. headTail truncates an oversized one, and a
// truncated attachment is not knowledge of the file — recording it as one would
// let the next `write` erase everything the model never saw.
type mentionRead struct {
	path  string
	body  string
	whole bool
}

func expandMentions(jail *Jail, line string) (blocks string, reads []mentionRead) {
	seen := map[string]bool{}
	for _, m := range reMention.FindAllStringSubmatch(line, -1) {
		tok := strings.TrimRight(m[2], ".,;:!?)") // shed trailing prose punctuation
		if tok == "" || seen[tok] {
			continue
		}
		abs, err := jail.Resolve(tok)
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		seen[tok] = true
		blocks += fmt.Sprintf("\n<file path=\"%s\">\n%s\n</file>\n", tok, headTail(string(data), maxReadBytes))
		reads = append(reads, mentionRead{path: tok, body: string(data), whole: len(data) <= maxReadBytes})
	}
	return blocks, reads
}
