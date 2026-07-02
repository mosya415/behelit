package main

import (
	"fmt"
	"os"
	"strings"
)

// The apply layer — the most treacherous part. We never do fuzzy matching. A
// search block must match the file byte-for-byte exactly once. Zero matches or
// multiple matches are reported back to the model as an error so it regenerates,
// rather than silently editing the wrong place.

// applyEdit performs a strict verbatim search/replace on the file at path.
func applyEdit(path, search, replace string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	content := string(data)

	n := strings.Count(content, search)
	switch {
	case n == 0:
		return "", fmt.Errorf("search text not found in %s — the match must be verbatim (whitespace and all). Re-read the file and copy the exact lines", path)
	case n > 1:
		return "", fmt.Errorf("search text matches %d places in %s — add surrounding context so it is unique", n, path)
	}

	updated := strings.Replace(content, search, replace, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return fmt.Sprintf("edited %s (1 replacement)", path), nil
}

// writeWholeFile replaces (or creates) a file with the given content. Intended
// for small files where a full rewrite is clearer than a search/replace.
func writeWholeFile(path, content string) (string, error) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return fmt.Sprintf("wrote %s (%d bytes)", path, len(content)), nil
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
