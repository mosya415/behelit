package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The apply layer — the most treacherous part. We never do fuzzy matching. A
// search block must match the file byte-for-byte exactly once. Zero matches or
// multiple matches are reported back to the model as an error so it regenerates,
// rather than silently editing the wrong place.

// applyEdit performs a strict verbatim search/replace on the file at path. `name`
// is the display path (as the model wrote it) used in the result message; IO is
// done on the jail-resolved `path`.
func applyEdit(path, name, search, replace string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	content := string(data)

	n := strings.Count(content, search)
	switch {
	case n == 0:
		return "", fmt.Errorf("search text not found in %s — the match must be verbatim (whitespace and all). Re-read the file and copy the exact lines", name)
	case n > 1:
		return "", fmt.Errorf("search text matches %d places in %s — add surrounding context so it is unique", n, name)
	}

	updated := strings.Replace(content, search, replace, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	return fmt.Sprintf("edited %s (1 replacement)", name), nil
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
