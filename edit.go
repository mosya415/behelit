package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The apply layer. A search block is matched against the file exactly first;
// only if that fails do the tolerant strategies in replacers.go run, and every
// strategy must land on a single literal location (or replace_all). Zero or
// ambiguous matches are reported back to the model as an error so it
// regenerates — never a silent pick of one of several places.

// applyEdit performs a single search/replace on the file at path. `name` is the
// display path (as the model wrote it) used in the result message; IO is done
// on the jail-resolved `path`.
func applyEdit(path, name, search, replace string) (string, error) {
	return applyEditMode(path, name, search, replace, false)
}

// applyEditMode is applyEdit with replace_all. Line endings follow the file:
// a CRLF file gets CRLF in both search and replacement.
func applyEditMode(path, name, search, replace string, replaceAll bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	content := string(data)
	if strings.Contains(content, "\r\n") {
		search = toCRLF(search)
		replace = toCRLF(replace)
	}

	updated, strategy, err := fuzzyReplace(content, search, replace, replaceAll)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if updated == content {
		return "", fmt.Errorf("%s: the edit produced no change", name)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	msg := fmt.Sprintf("edited %s", name)
	if replaceAll {
		msg += " (all occurrences)"
	} else {
		msg += " (1 replacement)"
	}
	if strategy != "exact" {
		msg += " — matched via " + strategy + " fallback; re-read before further edits nearby"
	}
	return msg, nil
}

func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// writeWholeFile replaces (or creates) a file with the given content, creating
// any missing parent directories along the way. Intended for small files where
// a full rewrite is clearer than a search/replace. `path` is already
// jail-resolved by the caller (so created directories stay inside the jail);
// `name` is the display path used in the result message.
func writeWholeFile(path, name, content string) (string, error) {
	dir := filepath.Dir(path)
	createdDir := false
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create dir for %s: %w", name, err)
		}
		createdDir = true
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	msg := fmt.Sprintf("wrote %s (%d bytes)", name, len(content))
	if createdDir {
		msg += ", created parent directory"
	}
	return msg, nil
}

// unifiedPreview renders a minimal, human-readable diff for the approval prompt.
// It is display-only — not fed to the model and not used to apply anything.
func unifiedPreview(search, replace string) string {
	var b strings.Builder
	for _, ln := range strings.Split(search, "\n") {
		b.WriteString("  - " + ln + "\n")
	}
	for _, ln := range strings.Split(replace, "\n") {
		b.WriteString("  + " + ln + "\n")
	}
	return b.String()
}
