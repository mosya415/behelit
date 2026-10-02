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

// promptHash fingerprints the SYSTEM PROMPT a session actually ran with — the
// assembled text, not the role's `prompt:` key: prompt.go composes it from the
// role's prose, the tool transport's instructions, the subagent list, the
// project's own instructions and the environment block, and any of those moving
// changes what the model was asked to do.
//
// It is the third of the three fields an eval row needs, and the one the other
// two cannot stand in for. lca_version moves when the binary is rebuilt and
// roles_hash moves when roles.yaml is edited, but an AGENTS.md edit, a tool
// added to the registry or a role switched from native to text tool calling
// moves neither — and each of them rewrites the prompt. Two runs whose pass rate
// differs and whose three hashes match are the same measurement twice; two whose
// prompt_hash differs are two measurements, which is the thing worth knowing
// before anybody claims the prompt edit helped.
//
// "" for no prompt at all, for the same reason rolesHash reports "" for no
// roles.yaml: a hash of nothing says more than the hash of an empty string.
func promptHash(prompt string) string {
	if prompt == "" {
		return ""
	}
	h := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(h[:])
}

// systemPromptOf is the system prompt a built session is carrying. It reads
// Msgs[0] rather than calling systemPrompt() again, because that is the text the
// gateway was sent: compaction and the project-instruction reload both rewrite
// Msgs[0] in place (engine.go), and re-assembling it here would fingerprint a
// prompt no request ever used.
func systemPromptOf(s *Session) string {
	if s == nil || len(s.Msgs) == 0 || s.Msgs[0].Role != "system" {
		return ""
	}
	return s.Msgs[0].Content
}

// promptFingerprint is promptHash over the prompt with the two facts about THIS
// run taken out of it. Without that it is a hash of the working directory and
// the calendar, and useless for the one job it has.
//
// The assembled prompt names the jail root and today's date (prompt.go's
// Environment block). An eval gives every task a fresh workspace under
// <out>/<timestamp>/, so a raw hash differs between every two tasks of the same
// run and between every two runs of the same task — which is to say it never
// matches and never tells anybody anything. Two runs a week apart on the same
// prompt have to produce the same hash, or there is nothing to compare.
//
// Nothing else is normalised. The allowlist, the tool documentation, the
// platform, the project instructions and the role's own prose all stay in: each
// of them is a real change to what the model was asked to do, and noticing
// those is the point.
// It reads both facts out of the PROMPT rather than out of the session — the
// root off the line that names it, the date off the line that names it — and so
// touches nothing else on a Session. That matters because one of its callers is
// the panic handler in oneshot.go, where everything may be half-built: a
// fingerprint is not worth a second panic inside the handler that exists to
// report the first.
const promptCwdLine = "- Working directory (jail): "

func promptFingerprint(s *Session) string {
	// Whatever the session was built with, if it was built: NewPrimary takes this
	// once, before the first turn, and both callers read that one answer. Reading
	// Msgs[0] again at the end of a run reported the POST-compaction, post-reload
	// prompt, so a one-shot row and an eval row of the same configuration could
	// disagree about the configuration.
	if s != nil && s.promptFP != "" {
		return s.promptFP
	}
	return fingerprintOf(systemPromptOf(s))
}

// fingerprintOf is the normalisation, over the prompt text itself. Separate from
// the session so the rules above can be asserted on a prompt a test wrote, and
// so promptFingerprint's memo is the only thing that decides WHEN the prompt is
// read.
func fingerprintOf(p string) string {
	if p == "" {
		return ""
	}
	// By prefix and not by today's literal date or the jail's current root: a run
	// that started before midnight and is fingerprinted after it, or one whose
	// root moved mid-session, would otherwise keep a per-run fact in the hash and
	// never match anything again.
	lines := strings.Split(p, "\n")
	root := ""
	for _, l := range lines {
		if strings.HasPrefix(l, promptCwdLine) {
			root = strings.TrimSpace(strings.TrimPrefix(l, promptCwdLine))
			break
		}
	}
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "- Today's date: "):
			lines[i] = "- Today's date: <today>"
		case strings.HasPrefix(l, promptCwdLine):
			lines[i] = promptCwdLine + "<root>"
		}
	}
	p = strings.Join(lines, "\n")
	// The root again, everywhere else it appears — a skills index, the name of a
	// project-instructions file. Only when it is a real path: replacing "/"
	// throughout a prompt would hash something nobody wrote.
	if len(root) > 1 {
		p = strings.ReplaceAll(p, root, "<root>")
	}
	return promptHash(p)
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
