package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// A fleet of members. A member is one machine the team works on: a name, the
// directory the project lives in there, and the sandbox that governs what runs
// there. The unit of execution is a member, not a session — the same team spans
// a laptop, a build box and a GPU node, and a role is pinned to one with
// `member:`.
//
//	members:
//	  local:                        # optional, only to scope THIS machine's sandbox
//	    allow: [go, git, make]
//	  build-box:
//	    host: build01               # ssh target (a ~/.ssh/config alias is fine)
//	    dir: /srv/work/llmbench     # the project on that machine, absolute
//	    ssh: [-o, ConnectTimeout=5] # extra ssh flags, or the whole transport argv
//	    allow: [go, git, make, bsk] # optional: replaces the team's sandbox.allow here
//	    shell: true                 # optional: this member's shell mode
//	  gpu-0:
//	    host: gpu07
//	    dir: /scratch/llmbench
//	    allow: [ls, cat, python3, bsk]
//
//	defaults:
//	  member: build-box             # the member roles use when they name none
//
//	roles:
//	  coder: {member: build-box, …}
//
// The LOCAL member's Rem is nil: the same nil that every file tool, the
// verifier, the workflow runner and the prompt already test for, which is why a
// fleet needs no second code path. Member exists to carry the name and the
// per-member sandbox, which a nil pointer cannot.
//
// The old `remote:` block and LCA_REMOTE keep working and mean one member named
// "remote" that every role which names no other one uses.

const (
	localMemberName  = "local"
	legacyMemberName = "remote"
)

type Member struct {
	Name     string
	Rem      *Remote  // nil: this machine
	Allow    []string // this member's allowlist; nil = the team's sandbox.allow
	Shell    *bool    // this member's shell mode; nil = the team's
	allowSet map[string]bool
	// fromRemote: this member exists only because of the remote: block or
	// LCA_REMOTE, and nothing under members: declared it. /role save writes it
	// back in that spelling; a DECLARED members.remote is the operator's own line
	// and is written under members:, sandbox and all.
	fromRemote bool
}

// IsLocal is the one question the routing asks. A nil member is this machine:
// every caller that could not resolve a name has already failed loudly, and a
// nil here must not be read as "some other machine".
func (m *Member) IsLocal() bool { return m == nil || m.Rem == nil }

// Label names the member for a table or a note.
func (m *Member) Label() string {
	if m.IsLocal() {
		return localMemberName
	}
	return m.Name + " (" + m.Rem.Label() + ")"
}

// Where names the machine in a sentence.
func (m *Member) Where() string {
	if m.IsLocal() {
		return "this machine"
	}
	return m.Rem.Where()
}

// MemberName is the name to record and display; "" never reaches a trace.
func (m *Member) MemberName() string {
	if m == nil || m.Name == "" {
		return localMemberName
	}
	return m.Name
}

// jail is the sandbox for commands that will run on this member. A member that
// sets neither allow: nor shell: gets o.jl ITSELF — the same pointer every
// existing team already uses, so its behaviour cannot drift. Otherwise the copy
// is rebuilt per call from o.jl so that Root and Unsafe are always the team's
// current ones: /unsafe toggles o.jl, and a cached copy would keep the sandbox
// on for one member after the operator lifted it.
//
// The copy is a struct copy, never NewJail: NewJail resolves Root through
// filepath.EvalSymlinks against the LOCAL filesystem, which would fail or lie
// for a member's directory. Root stays the local root and is meaningless for a
// remote member; CheckCommand looks at no path, and a remote member's paths are
// confined by Remote.relPath, not by the jail — nothing may call Resolve on a
// remote session's jail.
func (m *Member) jail(o *Orchestrator) *Jail {
	if o == nil {
		return nil
	}
	base := o.jl
	if m == nil || (m.Allow == nil && m.Shell == nil) {
		return base
	}
	if base == nil {
		base = &Jail{}
	}
	c := *base
	if m.Allow != nil {
		// A member's list REPLACES the team's; intersecting would turn a GPU
		// node's [bsk] into [] on most teams.
		c.Allowed, c.allowed = m.Allow, m.allowSet
	}
	if m.Shell != nil {
		c.Shell = *m.Shell
	}
	if !m.IsLocal() {
		c.Member = m.Name // message only: it changes no decision
	}
	return &c
}

// reach is the reachability gate for this member: nil for this machine.
func (m *Member) reach(ctx context.Context) error {
	if m == nil || m.Rem == nil {
		return nil
	}
	return m.Rem.ensureUp(ctx)
}

// withDir is the same machine and the same sandbox, another directory: a
// delegation's worktree over there. It shares the Remote's reachability state,
// because it is one machine.
func (m *Member) withDir(dir string) *Member {
	if m == nil || m.Rem == nil {
		return m
	}
	c := *m
	c.Rem = m.Rem.withDir(dir)
	return &c
}

var reMemberName = regexp.MustCompile("^[a-z0-9][a-z0-9_-]*$")

var memberKeys = []string{"host", "dir", "ssh", "allow", "shell"}

// parseMembers reads one file's members: block. The returned map never holds
// the remote:/LCA_REMOTE member — loadRoles adds that one after every file has
// merged, because members: and remote: may live in different files and the name
// collision can only be seen once both are known.
//
// The order slice is declaration order, which is what /members, doctor and
// YAML() print.
func parseMembers(n *yNode, src string) (map[string]*Member, []string, error) {
	out := map[string]*Member{}
	var order []string
	for _, mn := range n.Children {
		name := mn.Key
		if !reMemberName.MatchString(name) {
			return nil, nil, fmt.Errorf("members: bad member name %q (want lower-case letters, digits, - and _)", name)
		}
		m := &Member{Name: name}
		var host, dir string
		var ssh []string
		for _, k := range mn.Children {
			if !contains(memberKeys, k.Key) {
				return nil, nil, fmt.Errorf("members.%s: unknown key %q (want %s)", name, k.Key, strings.Join(memberKeys, ", "))
			}
			switch k.Key {
			case "host":
				host = strings.TrimSpace(k.Value)
			case "dir":
				dir = strings.TrimSpace(k.Value)
			case "ssh", "allow":
				// parseYAMLish's flow form {k: v, k2: v2} is split on commas and is
				// flat, so {host: h, ssh: [-o, X]} yields ssh: "[-o" and drops the
				// rest. A truncated ssh command line must never be run, so the
				// shape is an error naming the fix.
				if len(k.List) == 0 {
					if v := strings.TrimSpace(k.Value); v != "" {
						return nil, nil, fmt.Errorf("members.%s: %s: %q is not a list — a one-line {…} member cannot hold one; write %s: on its own line under members.%s", name, k.Key, v, k.Key, name)
					}
					if k.Key == "allow" {
						return nil, nil, fmt.Errorf("members.%s: allow: is empty — omit it to use the team's sandbox.allow, or list the commands this member may run", name)
					}
					return nil, nil, fmt.Errorf("members.%s: ssh: is empty — omit it for the default transport, or list the flags (or the whole command) to use", name)
				}
				if k.Key == "ssh" {
					ssh = k.List
				} else {
					m.Allow = k.List
				}
			case "shell":
				switch strings.ToLower(strings.TrimSpace(k.Value)) {
				case "", "true", "yes", "on":
					t := true
					m.Shell = &t
				case "false", "no", "off":
					f := false
					m.Shell = &f
				default:
					return nil, nil, fmt.Errorf("members.%s: shell must be true or false, got %q", name, k.Value)
				}
			}
		}
		if m.Allow != nil {
			m.allowSet = map[string]bool{}
			for _, c := range m.Allow {
				m.allowSet[c] = true
			}
		}
		if name == localMemberName {
			if host != "" || dir != "" || len(ssh) > 0 {
				return nil, nil, fmt.Errorf("members.local: host: and dir: are not allowed — local always means this machine and this working tree; give the far side another name")
			}
			if m.Allow == nil && m.Shell == nil {
				return nil, nil, fmt.Errorf("members.local: declares nothing — omit it, or give it allow: or shell: to scope this machine's sandbox")
			}
		} else {
			if host == "" && len(ssh) == 0 {
				return nil, nil, fmt.Errorf("members.%s: host is required (the ssh target), unless ssh: gives the whole transport command", name)
			}
			if dir == "" {
				return nil, nil, fmt.Errorf("members.%s: dir is required (the project's path on %s)", name, firstNonEmpty(host, "that machine"))
			}
			// A ~ is not expanded: Remote.script single-quotes Dir, so `cd '~/p'` is
			// a directory literally named "~" and the member would be reported as
			// unreachable with ssh advice for a one-character mistake. `lca init
			// -member` has always required a leading /.
			if strings.HasPrefix(dir, "~") {
				return nil, nil, fmt.Errorf("members.%s: dir must be an absolute path on %s (~ is not expanded over ssh), got %q", name, firstNonEmpty(host, "that machine"), dir)
			}
			if !strings.HasPrefix(dir, "/") {
				return nil, nil, fmt.Errorf("members.%s: dir must be absolute, got %q", name, dir)
			}
			m.Rem = &Remote{Name: name, Host: host, Dir: dir, SSH: ssh, Src: src}
		}
		if out[name] == nil {
			order = append(order, name)
		}
		out[name] = m
	}
	return out, order, nil
}

// ── the fleet on the orchestrator ───────────────────────────────────────────

// member resolves a name, or nil when the fleet has no member by it. Nothing
// here may panic on a zero Orchestrator: a test builds one.
func (o *Orchestrator) member(name string) *Member {
	if o == nil {
		return nil
	}
	switch name {
	case "", localMemberName:
		return o.localMember()
	case legacyMemberName:
		declared := o.members[legacyMemberName]
		if o.remote == nil {
			return declared
		}
		lm := o.legacyMember()
		if declared != nil && (declared.Allow != nil || declared.Shell != nil) {
			// LCA_REMOTE overrode members.remote's transport; its sandbox stands.
			c := *declared
			c.Rem = lm.Rem
			return &c
		}
		return lm
	}
	return o.members[name]
}

func (o *Orchestrator) localMember() *Member {
	if o == nil {
		return nil
	}
	o.memMu.Lock()
	defer o.memMu.Unlock()
	if o.locMem == nil {
		if m := o.members[localMemberName]; m != nil {
			o.locMem = m
		} else {
			o.locMem = &Member{Name: localMemberName}
		}
	}
	return o.locMem
}

// legacyMember synthesises the remote:/LCA_REMOTE member from o.remote on every
// call (keyed on the pointer, so a reassignment repoints the fleet) rather than
// copying it at startup: o.remote is assigned after the orchestrator exists in
// more than one place, and routing has to follow it.
func (o *Orchestrator) legacyMember() *Member {
	if o == nil || o.remote == nil {
		return nil
	}
	o.memMu.Lock()
	defer o.memMu.Unlock()
	if o.legMem == nil || o.legMem.Rem != o.remote {
		o.legMem = &Member{Name: legacyMemberName, Rem: o.remote}
	}
	return o.legMem
}

// memberFor is where a role's sessions work: the role's member:, else
// defaults.member, else the legacy remote: block, else this machine.
func (o *Orchestrator) memberFor(ag *Agent) *Member {
	if o == nil {
		return nil
	}
	if ag != nil && ag.Member != "" {
		if m := o.member(ag.Member); m != nil {
			return m
		}
	}
	if o.defMem != "" {
		if m := o.member(o.defMem); m != nil {
			return m
		}
	}
	if m := o.legacyMember(); m != nil {
		return m
	}
	return o.localMember()
}

// fleetNames is the fleet as every message lists it: local first, then
// declaration order, with the legacy remote: member last when it is not declared
// under members:. One implementation, because a load-time error listing a
// different fleet than /members and doctor show is a bug report about the wrong
// thing.
func fleetNames(order []string, legacy bool) []string {
	out := []string{localMemberName}
	for _, n := range order {
		if n != localMemberName {
			out = append(out, n)
		}
	}
	if legacy && !contains(out, legacyMemberName) {
		out = append(out, legacyMemberName)
	}
	return out
}

func (o *Orchestrator) memberNames() []string {
	if o == nil {
		return []string{localMemberName}
	}
	return fleetNames(o.memOrd, o.remote != nil)
}

// setFleet wires the fleet from a loaded roles.yaml, LCA_REMOTE-without-a-file
// included. NewOrchestrator and doctor both go through it: doctor's whole job is
// to report the fleet the runtime will use, and a field wired in one and
// forgotten in the other makes it report another one.
func (o *Orchestrator) setFleet(roles *RolesConfig) {
	if roles != nil {
		o.remote = roles.Remote
		o.members, o.memOrd = roles.Members, roles.MemberOrder
		o.defMem, o.hasMembers = roles.DefaultMember, roles.HasMembers
	}
	if o.remote == nil {
		if rem, err := parseRemote(nil); err == nil && rem != nil {
			o.remote = rem // LCA_REMOTE without a roles.yaml
		}
	}
}

// policyOf is the sandbox that decides for commands on m.
func (o *Orchestrator) policyOf(m *Member) *Jail { return m.jail(o) }

// checkOn is the sandbox check for a command that is about to run on m: the
// member's own policy, with the semantics of the machine that will run it. Every
// caller that can reach a member goes through it, so the two questions (whose
// allowlist, and which executor) are answered in one place.
func checkOn(j *Jail, m *Member, cmd string) error {
	if m.IsLocal() {
		return j.CheckCommand(cmd)
	}
	return j.CheckRemote(cmd)
}

// legacyFleet reports that this team's other machine comes from the old
// single-member spelling (remote: / LCA_REMOTE) and no members: block exists.
// o.remote is read live, not a flag frozen at load, because it is assigned
// after construction (LCA_REMOTE without a roles.yaml, and tests).
func (o *Orchestrator) legacyFleet() bool {
	return o != nil && o.remote != nil && !o.hasMembers
}

// canDelegate: cross-machine delegation is a members: feature. A legacy fleet
// keeps today's behaviour exactly — the delegate tool is withheld and a
// delegate step is refused at bind — so migrating is one line in roles.yaml and
// nothing about the wire contract depends on a network probe.
func (o *Orchestrator) canDelegate(m *Member) bool {
	return m.IsLocal() || !o.legacyFleet()
}

// ── a session's member ──────────────────────────────────────────────────────

// memberOf is the machine this session works on: the one pinned to it (a
// workflow step's member:, or the caller's, for a subagent that shares its
// tree), else its role's. The NAME is stored, not the pointer, so resolution
// stays dynamic.
func (s *Session) memberOf() *Member {
	if s.member != "" {
		if m := s.orch.member(s.member); m != nil {
			return m
		}
	} else if m := s.orch.memberFor(s.agent); m != nil {
		return m
	}
	if m := s.orch.localMember(); m != nil {
		return m
	}
	return &Member{Name: localMemberName}
}

func (s *Session) memberName() string { return s.memberOf().MemberName() }
