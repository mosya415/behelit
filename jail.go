package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Jail is the hard boundary (defense-in-depth alongside the soft approval gate).
// Every path a tool touches is realpath-resolved and must land inside Root;
// every command's argv[0] must be on the allowlist. Symlinks are followed before
// the prefix check, so a link pointing outside Root is rejected.
type Jail struct {
	Root    string   // absolute, symlink-resolved root
	Allowed []string // command allowlist, for display
	Unsafe  bool     // OFF the jail: any path, any command (see /unsafe)
	allowed map[string]bool
}

func NewJail(root string, allowed []string, unsafe bool) (*Jail, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("root %q: %w", root, err)
	}
	set := map[string]bool{}
	for _, c := range allowed {
		set[c] = true
	}
	return &Jail{Root: real, Allowed: allowed, Unsafe: unsafe, allowed: set}, nil
}

// Resolve turns a tool-supplied path into an absolute path guaranteed to be
// inside the jail. It resolves symlinks on the deepest existing ancestor (the
// target itself may not exist yet, e.g. a new file being written).
func (j *Jail) Resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(j.Root, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if j.Unsafe {
		return abs, nil // no confinement — any path
	}

	// Walk up to the nearest existing ancestor and resolve its symlinks, then
	// re-append the non-existent tail. This defeats symlink escapes on both the
	// target and any intermediate directory.
	real, tail := abs, ""
	for {
		if r, err := filepath.EvalSymlinks(real); err == nil {
			real = r
			break
		}
		parent := filepath.Dir(real)
		if parent == real {
			return "", fmt.Errorf("cannot resolve %q", p)
		}
		tail = filepath.Join(filepath.Base(real), tail)
		real = parent
	}
	resolved := real
	if tail != "" {
		resolved = filepath.Join(real, tail)
	}

	if !within(j.Root, resolved) {
		return "", fmt.Errorf("path %q escapes jail root %q", p, j.Root)
	}
	return resolved, nil
}

// AllowCommand reports whether argv[0]'s basename is on the allowlist (always
// true in unsafe mode).
func (j *Jail) AllowCommand(argv0 string) bool {
	if j.Unsafe {
		return true
	}
	return j.allowed[filepath.Base(argv0)]
}

func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
