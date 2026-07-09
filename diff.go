package main

import "strings"

// A tiny line-level diff for reviewing what the agent changed (`/diff`). It is
// display-only — the apply layer still uses strict verbatim search/replace, and
// the model is never shown or asked to produce a diff (see prompt.go). This is
// an LCS diff (Hunt–McIlroy style), stdlib only.

type diffOp struct {
	kind byte // ' ' common, '-' removed, '+' added
	text string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffOps returns the edit script turning a into b via a longest-common-
// subsequence walk.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// dp[i][j] = LCS length of a[i:] and b[j:]
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i, j = i+1, j+1
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// lineDiff renders a colored, context-collapsed diff of a→b and returns how many
// lines were added/removed. Huge inputs fall back to a one-line summary so we
// never spend O(n·m) on a megabyte file.
func lineDiff(a, b string) (added, removed int, rendered string) {
	al, bl := splitLines(a), splitLines(b)
	const maxLines = 5000
	if len(al) > maxLines || len(bl) > maxLines {
		return len(al), len(bl), faint("     (too large to diff inline: %d → %d lines)", len(al), len(bl))
	}
	ops := diffOps(al, bl)
	for _, o := range ops {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}

	// Keep changed lines plus a few lines of context; collapse the rest.
	const ctx = 3
	keep := make([]bool, len(ops))
	for idx, o := range ops {
		if o.kind != ' ' {
			for k := idx - ctx; k <= idx+ctx; k++ {
				if k >= 0 && k < len(ops) {
					keep[k] = true
				}
			}
		}
	}
	var sb strings.Builder
	collapsed := false
	for idx, o := range ops {
		if o.kind == ' ' && !keep[idx] {
			if !collapsed {
				sb.WriteString(cFaint + "     ⋮" + cReset + "\n")
				collapsed = true
			}
			continue
		}
		collapsed = false
		switch o.kind {
		case '-':
			sb.WriteString(cRed + "   - " + cReset + cRed + o.text + cReset + "\n")
		case '+':
			sb.WriteString(cGreen + "   + " + cReset + cGreen + o.text + cReset + "\n")
		default:
			sb.WriteString(cFaint + "     " + o.text + cReset + "\n")
		}
	}
	return added, removed, strings.TrimRight(sb.String(), "\n")
}
