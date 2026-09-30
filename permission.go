package main

import (
	"regexp"
	"strings"
	"sync"
)

// Permission rules (ported from opencode's permission/). A rule is
// {permission, pattern, action}; a ruleset is an ordered list and the LAST
// matching rule wins, so later layers (user config, agent config, session
// restrictions) override earlier ones by simply being appended.
//
// Permission keys and the pattern each tool checks:
//
//	read       read_file / list_dir / grep / glob → path
//	edit       edit / write                        → path
//	run        run_command                         → the command line
//	task       task                                → subagent name
//	delegate   delegate                            → role name (a reviewer session
//	                                                 gets delegate: deny)
//	skill      skill                               → skill name
//	todo       todowrite                           → *
//	web        webfetch                            → url
//	mcp        opening a connection to an mcp server → server name
//	mcp_read   an mcp tool the operator listed read-only → registry name
//	mcp_write  every other mcp tool                 → registry name
//	doom_loop  a tool called 3× with identical args → tool name
//
// Actions: allow (run), ask (the approval gate decides), deny (the model is
// told the call was refused by a rule). The jail stays the hard boundary
// underneath all of this.

type Action string

const (
	Allow Action = "allow"
	Ask   Action = "ask"
	Deny  Action = "deny"
)

type Rule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     Action `json:"action"`
}

type Ruleset []Rule

func validAction(s string) (Action, bool) {
	switch a := Action(strings.ToLower(strings.TrimSpace(s))); a {
	case Allow, Ask, Deny:
		return a, true
	}
	return "", false
}

// Evaluate returns the action of the last rule matching permission and
// pattern across the given rulesets (in order), defaulting to ask.
func Evaluate(permission, pattern string, sets ...Ruleset) Action {
	for i := len(sets) - 1; i >= 0; i-- {
		rs := sets[i]
		for j := len(rs) - 1; j >= 0; j-- {
			r := rs[j]
			if wildcardMatch(permission, r.Permission) && wildcardMatch(pattern, r.Pattern) {
				return r.Action
			}
		}
	}
	return Ask
}

// Disabled reports whether a permission is denied outright (last matching rule
// is "*" → deny), in which case its tools are not offered to the model at all.
func Disabled(permission string, sets ...Ruleset) bool {
	for i := len(sets) - 1; i >= 0; i-- {
		rs := sets[i]
		for j := len(rs) - 1; j >= 0; j-- {
			r := rs[j]
			if wildcardMatch(permission, r.Permission) {
				return r.Pattern == "*" && r.Action == Deny
			}
		}
	}
	return false
}

var (
	wildMu    sync.Mutex
	wildCache = map[string]*regexp.Regexp{}
)

// wildcardMatch: '*' matches anything (including '/'), '?' one char; anchored.
// A trailing " *" also matches the bare command, so "git status *" covers
// "git status".
func wildcardMatch(s, pattern string) bool {
	if pattern == "*" || pattern == s {
		return true
	}
	wildMu.Lock()
	re, ok := wildCache[pattern]
	if !ok {
		var b strings.Builder
		b.WriteString("(?s)^")
		p := pattern
		tailOpt := strings.HasSuffix(p, " *")
		if tailOpt {
			p = strings.TrimSuffix(p, " *")
		}
		for _, r := range p {
			switch r {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		if tailOpt {
			b.WriteString("( .*)?")
		}
		b.WriteString("$")
		re = regexp.MustCompile(b.String())
		wildCache[pattern] = re
	}
	wildMu.Unlock()
	return re.MatchString(s)
}

// permissionOf maps a tool to its permission key.
func permissionOf(tool string) string {
	switch tool {
	case "read_file", "list_dir", "grep", "glob":
		return "read"
	case "edit", "write":
		return "edit"
	case "run_command":
		return "run"
	case "todowrite":
		return "todo"
	case "webfetch":
		return "web"
	}
	// mcpTools is written once, before any session exists (registerMCPTools), and
	// is read-only afterwards — so this lookup needs no lock and -race stays clean.
	if mt := mcpTools[tool]; mt != nil {
		return mt.permKey() // "mcp_read" | "mcp_write"
	}
	return tool // task, delegate, skill: their own keys
}

// defaultRules is the base posture for every agent: reading and orchestration
// run freely, side effects ask (the approval-first default), and a call
// repeated identically three times asks before continuing.
func defaultRules() Ruleset {
	return Ruleset{
		{"*", "*", Allow},
		{"edit", "*", Ask},
		{"run", "*", Ask},
		{"web", "*", Ask},
		// Opening a connection asks, because that is where outbound traffic to a
		// named internal host becomes visible. A write asks, because it lands in
		// somebody's ticket. A read is allowed: the host is on the operator's own
		// allowlist and the tool was named by hand in their own file — and a read
		// that asks every time teaches the operator to type y without reading, which
		// is how the write gate gets defeated. mcp_read is spelled out although
		// {"*","*",Allow} above already covers it, so all three keys are visible in
		// one place and Disabled() has a row to find.
		{"mcp", "*", Ask},
		{"mcp_read", "*", Allow},
		{"mcp_write", "*", Ask},
		{"doom_loop", "*", Ask},
		{"read", "*.env", Ask},
		{"read", "*.env.*", Ask},
		{"read", "*.env.example", Allow},
	}
}
