package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// What ran, and what it ran as. Two eval runs, or two tickets, are only
// comparable if both say which binary and which team produced them: a pass rate
// that moved after a prompt edit is a different measurement, not a better one,
// and without these fields on every record there is nothing to notice that with.

// buildVersion is stamped at link time:
//
//	go build -ldflags "-X main.buildVersion=$(git rev-parse --short HEAD)" .
//
// Left empty — the ordinary `go build .` — the revision comes from the build
// info Go stamps itself, which is the same sha. The flag exists for the release
// build, where the source is a tarball with no .git in it.
var buildVersion string

// lcaVersion is the build's git sha, with "+dirty" when it was built from a
// working tree with changes in it. "unknown" is a real answer and not an error:
// a binary built with -buildvcs=false or from a tarball has nothing to report,
// and a record that says so is more use than one that invents a sha.
func lcaVersion() string {
	if buildVersion != "" {
		return buildVersion
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		// A `go test` binary and a `go install module@version` build both land here.
		// The module version is the only identity either of them has.
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		return rev + "+dirty"
	}
	return rev
}

// rolesHash fingerprints the team as it was LOADED: every roles.yaml that merged
// into this run, in merge order, hashed as bytes. The file NAME goes into the
// hash with its length, so a line moved from the project file to the user file
// is a different team even when the bytes are the same — because the merge order
// decides which one wins.
//
// "" when there is no roles.yaml at all: the built-in agents are a property of
// the binary, which lca_version already names.
func rolesHash(rc *RolesConfig) string {
	if rc == nil || len(rc.Sources) == 0 {
		return ""
	}
	h := sha256.New()
	for _, p := range rc.Sources {
		b, err := os.ReadFile(p)
		if err != nil {
			// A file that was readable a moment ago and is not now makes every hash
			// after it meaningless. Report nothing rather than a hash of a partial team.
			return ""
		}
		fmt.Fprintf(h, "%s\n%d\n", filepath.Base(p), len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// runVersion is `lca version`: the build, and the team it would run with here.
// It talks to nothing — no gateway, no ssh — so it answers in a broken
// environment, which is when somebody usually asks.
func runVersion(cfg Config, args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Fprintln(os.Stderr, "   lca version                  the build's git sha and the hash of the roles.yaml in force")
			return exitOK
		}
	}
	fmt.Println("lca " + lcaVersion())
	// Deliberately NOT loadRoles. Validation needs the MCP set to exist first —
	// roles.yaml's tools: list is checked against the tool registry, which is where
	// `tix__get_issue` comes from (setup_cmds.go states that ordering in so many
	// words) — and this subcommand loads no MCP, so a perfectly valid roles.yaml
	// naming MCP tools came back "unreadable" and printed no hash at all. The one
	// place whose job is to print the run's identity was the one place it was
	// unavailable, which defeats the purpose: this hash exists so two eval runs can
	// be compared.
	//
	// And the hash needs none of that. It is over the file PATHS and their BYTES,
	// so resolving the paths the way loadRoles resolves them and hashing them is
	// the right answer even for a file this binary would reject.
	paths := rolesFilePaths(cfg)
	if len(paths) == 0 {
		fmt.Println("roles " + faint("%s", "none"+gSep+"the built-in agents"))
		return exitOK
	}
	var short []string
	for _, p := range paths {
		short = append(short, prettyPath(p, cfg.Root))
	}
	fmt.Println("roles " + rolesHash(&RolesConfig{Sources: paths}) + faint(gSep+"%s", strings.Join(short, ", ")))
	// Whether it would LOAD is a separate question from what it is, and the answer
	// belongs here too — `lca doctor` is where it is diagnosed, so this only has to
	// say that there is something to diagnose.
	if _, err := loadRoles(cfg); err != nil {
		fmt.Println("roles " + faint("would not load here"+gSep+"%s"+gSep+"see `lca doctor`", shortErr(err)))
	}
	return exitOK
}
