package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// The screens. /mcp answers "what can this reach" and "what can this change"
// from the two files alone, and lca doctor is the one that actually goes and
// looks.
//
// /mcp PERFORMS NO NETWORK I/O. On a monitored corporate host, lca making a TCP
// connection the operator did not ask for is a question from the security team,
// and a listing command is not a request to connect. /mcp probe, /mcp refresh
// and lca doctor are the only paths here that contact anything, and each of them
// is something the operator typed.

func (r *Repl) cmdMCP(arg string) bool {
	m := r.orch.mcp
	if !m.configured() {
		// The entry point, because /mcp is listed before anything is configured: an
		// operator who types it is asking how, so answer that instead of only saying no.
		section("mcp", faint("none configured"))
		fmt.Println("  " + faint("an internal server goes in the mcp block of .lca/config.json:"))
		fmt.Println("    " + faint(`"mcp": {"allow_hosts": ["jira.corp:443"], "servers": {"jira": {`))
		fmt.Println("    " + faint(`  "transport": "http", "url": "https://jira.corp/mcp",`))
		fmt.Println("    " + faint(`  "headers": {"Authorization": "Bearer ${env:JIRA_MCP_TOKEN}"},`))
		fmt.Println("    " + faint(`  "expose": []}}}`))
		hint("%s", "then /mcp refresh prints the tool names it serves, ready to paste into expose:"+gSep+"a token is never stored, only the variable's name")
		return false
	}
	fields := strings.Fields(arg)
	switch {
	case len(fields) == 0:
		m.show(r.sess)
	case fields[0] == "probe":
		name := ""
		if len(fields) > 1 {
			name = fields[1]
		}
		tc := &ToolCtx{Ctx: context.Background(), S: r.sess, Name: "/mcp"}
		if err := m.probe(tc, name); err != nil {
			errLine("%v", err)
		}
		m.show(r.sess)
	case fields[0] == "refresh":
		user := false
		var only string
		for _, f := range fields[1:] {
			switch f {
			case "-user", "--user":
				user = true
			default:
				only = f
			}
		}
		tc := &ToolCtx{Ctx: context.Background(), S: r.sess, Name: "/mcp"}
		if err := m.refresh(tc, only, user); err != nil {
			errLine("%v", err)
		}
	default:
		errLine("usage: /mcp [probe|refresh] [<server>]")
	}
	return false
}

// show prints the servers, their reachability, and which of their tools are in
// THIS session's prefix. Every fact on it comes from the config, the lock and
// what this process already learned.
func (m *MCPSet) show(s *Session) {
	section("mcp", faint("%s"+gSep+"from %s", plural(len(m.order), "server", "servers"), orElseStr(m.from, "no config file")))
	if len(m.allow.order) > 0 {
		row("hosts", strings.Join(m.allow.order, ", "))
	} else {
		warnLine("mcp.allow_hosts is empty — no http server may be reached until a host:port is listed there")
	}
	if len(m.allow.cidrText) > 0 {
		row("networks", strings.Join(m.allow.cidrText, ", "))
	}
	stdio := "deny"
	if m.stdio {
		stdio = "allow"
	}
	row("stdio", stdio)
	ann := "not trusted (a server's own read-only hint does not make a tool a read)"
	if m.trustAnn {
		ann = "trusted (a server's read_only_hint makes a tool a read)"
	}
	row("hints", ann)
	row("lock", m.lockSummary())

	inPrefix := map[string]bool{}
	if s != nil {
		defs, _ := s.tools()
		for _, d := range defs {
			inPrefix[d.Name] = true
		}
	}
	role := "this session"
	if s != nil {
		role = s.agent.Name
	}

	for _, name := range m.order {
		sv := m.servers[name]
		// "(refused at load)" and not the raw url: a server refused for carrying a
		// credential in its url must not have that url printed here of all places.
		endpoint := firstNonEmpty(sv.URL, strings.Join(append([]string{sv.Command}, sv.Args...), " "), "(refused at load)")
		transport := sv.Transport
		if sv.Plaintext {
			transport = "http (plaintext — the token crosses the network in clear)"
		}
		section("server", faint("%s"+gSep+"%s"+gSep+"%s", name, transport, endpoint))
		_, state, reason := sv.snapshot()
		switch state {
		case mcpStateConnected:
			sv.mu.Lock()
			info := sv.info
			sv.mu.Unlock()
			sid := "no session id"
			if info.SessionID {
				sid = "session id issued"
			}
			okLine("connected"+gSep+"MCP %s"+gSep+"%s %s"+gSep+"%s", orElseStr(info.Protocol, "?"),
				orElseStr(info.Name, "?"), info.Version, sid)
		case mcpStateStale:
			errLine("stale — %s", reason)
		case mcpStateFailed:
			errLine("failed: %s", reason)
		case mcpStateDisabled:
			warnLine("disabled — %s", reason)
			hint("set \"stdio\": \"allow\" in the mcp block of %s, and put %s on sandbox.allow in roles.yaml",
				orElseStr(m.from, ".lca/config.json"), baseName(sv.Command))
		default:
			fmt.Println("  " + faint("not contacted — nothing in this session has used it"))
		}
		if sv.loadErr != "" && state != mcpStateFailed {
			errLine("%s", sv.loadErr)
		}
		for _, h := range sv.Headers {
			if h.set() {
				row("token", fmt.Sprintf("%s: %s$%s   %s", h.Name, h.Prefix, h.EnvVar, "set"))
			} else {
				warnLine("$%s is not set, so header %s cannot be built", h.EnvVar, h.Name)
			}
		}
		for _, e := range sv.Env {
			if e.set() {
				row("env", fmt.Sprintf("%s=$%s   set", e.Name, e.EnvVar))
			} else {
				warnLine("$%s is not set, so env %s cannot be built", e.EnvVar, e.Name)
			}
		}
		if sv.CAFile != "" {
			row("ca", sv.CAFile)
		}
		if sv.Query {
			warnLine("its url carries a query string — a token in a url is recorded in the config, the lock and every error message")
		}
		served := 0
		if sv.lock != nil {
			served = len(sv.lock.Tools)
		}
		regs := mcpNamesOf(name)
		usable := 0
		for _, reg := range regs {
			if inPrefix[reg] {
				usable++
			}
		}
		row("tools", fmt.Sprintf("%d served"+gSep+"%d exposed"+gSep+"%d usable by %s", served, len(regs), usable, role))
		if sv.lock == nil {
			warnLine("no lock entry — run /mcp refresh")
		}
		for _, tool := range sv.Expose {
			if sv.lock != nil && sv.lock.Tools[tool] == nil {
				warnLine("expose: names %q, which the server does not serve%s", tool, didYouMean(tool, sortedKeys(sv.lock.Tools)))
			}
		}
		if sv.lock != nil && sv.lock.Truncated {
			warnLine("%d tools listed and truncated — name the ones you need in expose:", len(sv.lock.Tools))
		}
		var rows [][]string
		for _, reg := range regs {
			mt := mcpTools[reg]
			class := "read"
			if mt.Write {
				class = "write"
			}
			rows = append(rows, []string{reg, class, mt.From, mcpPrefixNote(mt, s, inPrefix[reg])})
		}
		if len(rows) > 0 {
			table([]string{"tool", "class", "from", "in this session's prefix"}, rows)
		}
		for _, reg := range regs {
			if mcpTools[reg].NativeOnly {
				fmt.Println("  " + faint("%s — native only (the text transport's tag grammar is lower-case)", reg))
			}
		}
		sv.mu.Lock()
		tail, dropped := sv.stderrTail, sv.dropped
		sv.mu.Unlock()
		if strings.TrimSpace(tail) != "" {
			row("stderr", truncate(collapseWS(scrubSecrets(tail, sv.secrets())), 200))
		}
		if dropped > 0 {
			warnLine("%s was dropped (lca advertises no capabilities)", plural(dropped, "server→client request", "server→client requests"))
		}
	}
	fmt.Println()
	fmt.Println("  " + faint("frozen    this session's tool list came from the lock at start and does not change;"))
	fmt.Println("  " + faint("          a server that now serves something else fails its calls instead"))
	fmt.Println("  " + faint("results   go into the transcript and to the model's endpoint"))
	fmt.Println("  " + faint("not done  resources and prompts are not implemented — ignored"))
}

// mcpPrefixNote says why a tool is or is not in this session's request prefix.
// "no" with no reason is the answer an operator cannot act on.
func mcpPrefixNote(mt *mcpTool, s *Session, in bool) string {
	if in {
		if mt.Write {
			return "yes — needs approval"
		}
		return "yes"
	}
	if s == nil {
		return "no"
	}
	if s.agent.ToolsSet && !contains(s.agent.Tools, mt.Reg) {
		return "no — not in " + s.agent.Name + "'s tools:"
	}
	if Disabled(mt.permKey(), s.rules()...) {
		return "no — " + mt.permKey() + " is denied here"
	}
	return "no"
}

func (m *MCPSet) lockSummary() string {
	seen := map[string]bool{}
	var parts []string
	for _, name := range m.order {
		sv := m.servers[name]
		if sv.lock == nil || seen[sv.lockPath] {
			continue
		}
		seen[sv.lockPath] = true
		when := sv.lock.FetchedAt
		if t, err := time.Parse(time.RFC3339, when); err == nil {
			when = t.Local().Format("2006-01-02 15:04")
		}
		parts = append(parts, sv.lockPath+gSep+"refreshed "+when)
	}
	if len(parts) == 0 {
		return faint("%s — nothing recorded yet; run /mcp refresh", m.lockPath)
	}
	return strings.Join(parts, ", ")
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ── probe and refresh ───────────────────────────────────────────────────────

// probe is the explicit way to contact a server now. It connects, handshakes and
// lists, and leaves the server's state where /mcp will show it.
func (m *MCPSet) probe(tc *ToolCtx, name string) error {
	names := m.order
	if name != "" {
		if m.servers[name] == nil {
			return fmt.Errorf("no mcp server %q is configured%s", name, mcpConfiguredList(m))
		}
		names = []string{name}
	}
	var firstErr error
	for _, n := range names {
		sv := m.servers[n]
		if sv.loadErr != "" {
			errLine("%s: %s", n, sv.loadErr)
			continue
		}
		if _, state, reason := sv.snapshot(); state == mcpStateDisabled {
			warnLine("%s: %s", n, reason)
			continue
		}
		// A stale server is retried by an explicit probe: the operator asked.
		sv.setState(mcpStateNew, "")
		res := m.probeOne(tc, sv)
		if res.err != nil {
			errLine("%s: %v", n, res.err)
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		okLine("%s"+gSep+"MCP %s"+gSep+"%s"+gSep+"%s", n, res.info.Protocol,
			orElseStr(res.info.Name, "?"), plural(len(res.tools), "tool", "tools"))
		// A probe is the look-without-writing command, so a server that exposes nothing
		// yet gets the same paste-ready block refresh prints.
		mcpServedBlock(sv, res.tools)
	}
	return firstErr
}

type mcpProbeResult struct {
	sv        *MCPServer
	info      mcpServerInfo
	tools     []mcpRawTool
	truncated bool
	addrs     []string
	err       error
}

// probeOne opens a connection of its own and closes it again, deliberately
// NEITHER caching it nor applying the drift check.
//
// Not cached, because a cached connection that skipped the drift check would be
// reused by a later tool call and the check would never run. Not drift-checked,
// because a stale lock is the very thing a probe is for: /mcp refresh has to be
// able to look at a server whose tools have changed, which is exactly when the
// drift check refuses.
func (m *MCPSet) probeOne(tc *ToolCtx, sv *MCPServer) mcpProbeResult {
	res := mcpProbeResult{sv: sv}
	if err := sv.askConnect(tc); err != nil {
		res.err = err
		return res
	}
	ctx, cancel := context.WithTimeout(tc.Ctx, sv.Timeout)
	defer cancel()
	var c mcpConn
	var err error
	switch sv.Transport {
	case "http":
		c, err = dialMCPHTTP(sv)
	default:
		c, err = dialMCPStdio(sv)
	}
	if err != nil {
		sv.setState(mcpStateFailed, err.Error())
		res.err = err
		return res
	}
	defer c.close()
	info, err := mcpHandshake(ctx, c)
	if err != nil {
		sv.stash(c)
		sv.setState(mcpStateFailed, err.Error())
		res.err = err
		return res
	}
	tools, truncated, err := mcpListTools(ctx, c)
	sv.stash(c)
	if err != nil {
		sv.setState(mcpStateFailed, err.Error())
		res.err = err
		return res
	}
	sv.mu.Lock()
	sv.info, sv.state, sv.reason = info, mcpStateConnected, ""
	sv.mu.Unlock()
	res.info, res.tools, res.truncated = info, tools, truncated
	return res
}

// refresh rewrites the lock from what the servers serve now. It prints the delta
// FIRST, because an operator who is about to change every new session's request
// prefix should see what changes, and asks under the mcp class before writing.
func (m *MCPSet) refresh(tc *ToolCtx, only string, user bool) error {
	names := m.order
	if only != "" {
		if m.servers[only] == nil {
			return fmt.Errorf("no mcp server %q is configured%s", only, mcpConfiguredList(m))
		}
		names = []string{only}
	}
	// Grouped by the file each entry will be written to: /mcp refresh rewrites
	// whichever file already holds that server, else the project one.
	byPath := map[string]*MCPLock{}
	wrote := 0
	for _, n := range names {
		sv := m.servers[n]
		if sv.loadErr != "" {
			errLine("%s: %s", n, sv.loadErr)
			continue
		}
		if _, state, reason := sv.snapshot(); state == mcpStateDisabled {
			warnLine("%s: %s", n, reason)
			continue
		}
		sv.setState(mcpStateNew, "")
		res := m.probeOne(tc, sv)
		if res.err != nil {
			errLine("%s: %v", n, res.err)
			continue
		}
		entry := mcpLockEntry(sv, res)
		section("delta", faint("%s", n))
		for _, l := range diffMCPLock(sv.lock, entry) {
			fmt.Println("  " + l)
		}
		// The way out of "I do not know my server's tool names yet". An operator on a
		// monitored host should not have to invent a fake tool name to get past
		// validation and then read the real ones out of an error message.
		mcpServedBlock(sv, res.tools)
		path := sv.lockPath
		if user {
			path = userLockPath(m)
		}
		l := byPath[path]
		if l == nil {
			existing, _, err := readMCPLock(path)
			if err != nil {
				return err
			}
			existing.Version = 1
			l = existing
			byPath[path] = l
		}
		l.Servers[n] = entry
		wrote++
	}
	if wrote == 0 {
		return nil
	}
	if msg, ok := tc.Ask("mcp", "refresh", "MCP REFRESH", "  rewrites the lock; the running session keeps the tools it started with"); !ok {
		return fmt.Errorf("%s", msg)
	}
	for _, path := range sortedKeys(byPath) {
		if err := writeMCPLock(path, byPath[path]); err != nil {
			return err
		}
		okLine("wrote %s", path)
	}
	hint("the running session keeps the tools it started with; restart to pick these up")
	return nil
}

// userLockPath is where -user writes: next to the user's config.json, recorded by
// loadMCP. Derived from the entries instead, it found nothing on a first run —
// every server's lockPath is the project lock until a user lock exists — and
// returned the PROJECT lock, so `/mcp refresh -user`, typed precisely to keep a
// corporate server's tool inventory out of the repository, wrote it into the
// repository. The flag only began working once it was no longer needed.
func userLockPath(m *MCPSet) string {
	return firstNonEmpty(m.userLockPath, m.lockPath)
}

// mcpLockEntry records EVERY tool the server served, not just the exposed ones,
// so doctor can print the list the operator pastes from. The lock is a record of
// the server; the config is the operator's decision.
func mcpLockEntry(sv *MCPServer, res mcpProbeResult) *MCPLockServer {
	e := &MCPLockServer{
		Endpoint:        firstNonEmpty(sv.URL, strings.Join(append([]string{sv.Command}, sv.Args...), " ")),
		Transport:       sv.Transport,
		ProtocolVersion: res.info.Protocol,
		FetchedAt:       time.Now().UTC().Format(time.RFC3339),
		Truncated:       res.truncated,
		Tools:           map[string]*MCPLockTool{},
	}
	e.ServerInfo.Name, e.ServerInfo.Version = res.info.Name, res.info.Version
	for _, t := range res.tools {
		lt := &MCPLockTool{Description: firstNonEmpty(t.Description, t.Title), InputSchema: t.InputSchema}
		if t.Annotations != nil {
			if t.Annotations.ReadOnlyHint != nil {
				lt.ReadOnlyHint = *t.Annotations.ReadOnlyHint
			}
			if t.Annotations.DestructiveHint != nil {
				lt.DestructiveHint = *t.Annotations.DestructiveHint
			}
		}
		e.Tools[t.Name] = lt
	}
	return e
}

// mcpServedBlock prints what the server serves as a block the operator can paste,
// for a server that exposes nothing yet. It suggests no split: naming which tools
// only read would be the name heuristic readWrite() refuses on purpose, so every
// tool starts as a write and the line under the block says so.
func mcpServedBlock(sv *MCPServer, tools []mcpRawTool) {
	if len(sv.Expose) > 0 || len(tools) == 0 {
		return
	}
	var names []string
	for _, t := range tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	section("served", faint("%s"+gSep+"%s"+gSep+"none exposed", sv.Name, plural(len(names), "tool", "tools")))
	fmt.Println("  " + faint("paste into mcp.servers.%s, keeping only the tools you need:", sv.Name))
	// Wrapped rather than truncated: the point of this block is that it can be
	// pasted, and a cut list is invalid JSON. A server with fifty tools gets fifty
	// names over several lines, which is also the clearest possible argument for
	// deleting most of them.
	if one := strings.Join(quoted, ", "); len(one) <= 60 {
		fmt.Printf("    \"expose\":    [%s],\n", one)
	} else {
		fmt.Println(`    "expose":    [`)
		for _, line := range wrapJoin(quoted, 60) {
			fmt.Println("      " + line)
		}
		fmt.Println("    ],")
	}
	fmt.Println("    \"read_only\": []")
	hint("a tool you leave out of read_only: is treated as a write and asks every time — that is the safe default, not an oversight")
}

// wrapJoin lays items out over lines of at most width, comma-separated, so the
// result is still one JSON array.
func wrapJoin(items []string, width int) []string {
	var out []string
	cur := ""
	for i, it := range items {
		piece := it
		if i < len(items)-1 {
			piece += ","
		}
		switch {
		case cur == "":
			cur = piece
		case len(cur)+1+len(piece) <= width:
			cur += " " + piece
		default:
			out = append(out, cur)
			cur = piece
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// ── lca doctor ──────────────────────────────────────────────────────────────

// mcpDoctor is doctor's mcp section: what is configured, what resolves, what
// answers, and how many schemas each role is carrying. It returns true when
// something is wrong enough to fail the command.
//
// A server that is down must not slow down or break a session that does not use
// it, so the verdict depends on whether a role actually exposes its tools:
// configured-but-unneeded is a warn, and configured-and-used is a fail, because
// that role's sessions will run without tools they were given.
func mcpDoctor(ctx context.Context, orch *Orchestrator, roles *RolesConfig, loadWarns []string, noProbe, doRefresh bool) bool {
	m := orch.mcp
	if !m.configured() {
		return false
	}
	failed := false
	section("mcp", faint("%s", plural(len(m.order), "server", "servers")))
	row("config", orElseStr(m.from, "none"))
	if len(m.allow.order) == 0 {
		errLine("mcp.allow_hosts is empty — no http server may be reached")
		hint("list the host and port you mean, e.g. \"allow_hosts\": [\"jira.corp.example:443\"]")
		failed = true
	}
	for _, h := range m.allow.order {
		if noProbe {
			row("hosts", h)
			continue
		}
		host, _, _ := net.SplitHostPort(h)
		ips, err := net.LookupHost(strings.Trim(host, "[]"))
		if err != nil {
			row("hosts", h+faint(" → does not resolve: %v", err))
			continue
		}
		row("hosts", h+faint(" → %s", strings.Join(ips, ", ")))
	}
	if len(m.allow.cidrText) > 0 {
		row("networks", strings.Join(m.allow.cidrText, ", "))
	}
	row("lock", m.lockSummary())
	for _, w := range loadWarns {
		warnLine("%s", w)
	}
	// An operator who has just written a host allowlist will reasonably assume it
	// covers all egress. It does not, and a silent assumption is worse than a line.
	fmt.Println("  " + faint("note      mcp.allow_hosts governs MCP only — webfetch still reaches any host with one approval"))

	usedBy := mcpRoleUsage(orch, roles)

	// Probes run in parallel, each with its own deadline, like probeModel: a dozen
	// servers must not cost a dozen timeouts in series.
	results := map[string]mcpProbeResult{}
	if !noProbe {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, n := range m.order {
			sv := m.servers[n]
			if sv.loadErr != "" {
				continue
			}
			if _, state, _ := sv.snapshot(); state == mcpStateDisabled {
				continue
			}
			wg.Add(1)
			go func(sv *MCPServer) {
				defer wg.Done()
				pctx, cancel := context.WithTimeout(ctx, sv.Timeout)
				defer cancel()
				res := mcpDoctorProbe(pctx, m, sv)
				mu.Lock()
				results[sv.Name] = res
				mu.Unlock()
			}(sv)
		}
		wg.Wait()
	}

	for _, n := range m.order {
		sv := m.servers[n]
		endpoint := firstNonEmpty(sv.URL, strings.Join(append([]string{sv.Command}, sv.Args...), " "), "(refused at load)")
		scheme := sv.Transport
		if sv.Transport == "http" {
			scheme = "https"
			if sv.Plaintext {
				scheme = "http"
			}
		}
		section("server", faint("%s"+gSep+"%s"+gSep+"%s", n, scheme, endpoint))
		bad := func(format string, a ...any) {
			if usedBy[n] > 0 {
				errLine(format, a...)
				failed = true
				return
			}
			warnLine(format, a...)
		}
		if sv.loadErr != "" {
			bad("%s", sv.loadErr)
			continue
		}
		if _, state, reason := sv.snapshot(); state == mcpStateDisabled {
			warnLine("disabled — %s", reason)
			hint("set \"stdio\": \"allow\" in the mcp block, and put %s on sandbox.allow in roles.yaml", baseName(sv.Command))
			continue
		}
		if sv.Plaintext {
			warnLine("plaintext http — the token crosses the network in clear")
			hint("use https, or accept that anything on the path can read the credential")
		}
		if sv.Query {
			warnLine("its url carries a query string — a token there is recorded in the config, the lock and error messages")
		}
		for _, h := range sv.Headers {
			if h.set() {
				okLine("$%s is set (%s)", h.EnvVar, h.Name)
			} else {
				bad("$%s is not set, so header %s cannot be built", h.EnvVar, h.Name)
				hint("export %s before starting lca — the config records the name, never the token", h.EnvVar)
			}
		}
		for _, e := range sv.Env {
			if e.set() {
				okLine("$%s is set (%s)", e.EnvVar, e.Name)
			} else {
				bad("$%s is not set, so env %s cannot be built", e.EnvVar, e.Name)
			}
		}
		if sv.Transport == "stdio" {
			mcpDoctorStdio(orch, sv, bad)
		}
		if sv.lock == nil {
			bad("no entry in %s", prettyLockPath(sv.lockPath))
			hint("run /mcp refresh (or lca doctor -mcp-refresh) to record what it serves")
		}
		if noProbe {
			fmt.Println("  " + faint("-no-probe: configuration only, nothing was contacted"))
			continue
		}
		res, ok := results[n]
		if !ok {
			continue
		}
		if len(res.addrs) > 0 {
			okLine("%s", strings.Join(res.addrs, ", "))
		}
		if res.err != nil {
			bad("%s", scrubSecrets(res.err.Error(), sv.secrets()))
			hint("%s", mcpFixHint(sv, res.err))
			continue
		}
		if sv.CAFile != "" {
			okLine("tls verified against %s", sv.CAFile)
		}
		sid := "no session id"
		if res.info.SessionID {
			sid = "session id issued"
		}
		okLine("handshake ok — MCP %s, %s %s, %s", orElseStr(res.info.Protocol, "?"),
			orElseStr(res.info.Name, "?"), res.info.Version, sid)
		if res.info.Protocol != "" && res.info.Protocol != mcpProtocolVersion {
			fmt.Println("  " + faint("lca asked for MCP %s and the server answered %s — accepted; these four messages have not changed", mcpProtocolVersion, res.info.Protocol))
		}
		if res.info.NoTools {
			warnLine("it reports no tools capability — it contributes nothing, and lca does not guess otherwise")
		}
		mcpDoctorTools(m, sv, res, bad)
		mcpServedBlock(sv, res.tools)
		if doRefresh {
			entry := mcpLockEntry(sv, res)
			l, _, err := readMCPLock(sv.lockPath)
			if err != nil {
				bad("%v", err)
				continue
			}
			l.Version = 1
			l.Servers[n] = entry
			if err := writeMCPLock(sv.lockPath, l); err != nil {
				bad("%v", err)
				continue
			}
			okLine("wrote %s", sv.lockPath)
		}
	}

	mcpDoctorRoles(orch, roles, usedBy)
	fmt.Println("  " + faint("resources and prompts are not implemented — ignored"))
	return failed
}

// mcpDoctorProbe resolves the address, then connects. It never goes through
// ensure() and so never asks: doctor runs unattended, and it closes the
// connection it opened rather than leaving one warm.
func mcpDoctorProbe(ctx context.Context, m *MCPSet, sv *MCPServer) mcpProbeResult {
	res := mcpProbeResult{sv: sv}
	if sv.Transport == "http" {
		host, port, _ := net.SplitHostPort(sv.Host)
		start := time.Now()
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, strings.Trim(host, "[]"))
		if err != nil {
			res.err = fmt.Errorf("%s does not resolve: %v", host, err)
			return res
		}
		for _, ip := range ips {
			addr := net.JoinHostPort(ip.IP.String(), port)
			if err := m.allow.permitAddr(sv.Host, addr); err != nil {
				res.err = err
				return res
			}
			conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
			if err != nil {
				res.err = fmt.Errorf("%s is not reachable: %v", addr, err)
				return res
			}
			conn.Close()
			inside := ""
			for _, c := range m.allow.cidrText {
				if _, n, e := net.ParseCIDR(c); e == nil && n.Contains(ip.IP) {
					inside = ", inside " + c
				}
			}
			res.addrs = append(res.addrs, fmt.Sprintf("%s reachable (%dms)%s", addr, time.Since(start).Milliseconds(), inside))
		}
	}
	var c mcpConn
	var err error
	switch sv.Transport {
	case "http":
		c, err = dialMCPHTTP(sv)
	default:
		c, err = dialMCPStdio(sv)
	}
	if err != nil {
		res.err = err
		return res
	}
	defer c.close()
	hctx, cancel := context.WithTimeout(ctx, mcpHandshakeTimeout)
	defer cancel()
	info, err := mcpHandshake(hctx, c)
	if err != nil {
		sv.stash(c)
		res.err = err
		return res
	}
	res.info = info
	tools, truncated, err := mcpListTools(ctx, c)
	sv.stash(c)
	if err != nil {
		res.err = err
		return res
	}
	res.tools, res.truncated = tools, truncated
	return res
}

// mcpDoctorStdio is the up-front version of the spawn-time gates, so an operator
// learns about them from the command they already run.
func mcpDoctorStdio(orch *Orchestrator, sv *MCPServer, bad func(string, ...any)) {
	argv0 := baseName(sv.Command)
	if _, err := os.Stat(sv.Command); err != nil && strings.ContainsAny(sv.Command, `/\`) {
		bad("%s does not exist", sv.Command)
		hint("install it, or point mcp.servers.%s command: at where it is", sv.Name)
		return
	}
	if orch.jl != nil && !orch.jl.AllowCommand(argv0) {
		bad("command %q is not on the sandbox allowlist — it cannot be spawned", argv0)
		hint("add it to sandbox.allow in roles.yaml, or use an http transport")
		return
	}
	okLine("command %q is on the sandbox allowlist", argv0)
	switch argv0 {
	case "npx", "uvx", "pnpx", "bunx":
		// Not a blocklist — a blocklist of package managers is theatre, and this is
		// excluded by consequence (it is not on a corporate sandbox.allow). If it IS,
		// the operator should hear it from doctor.
		warnLine("command %q is on the sandbox allowlist and will fetch code from the network at spawn time", argv0)
	}
	// The honest admission, in the command an operator runs before a review.
	fmt.Println("  " + faint("a stdio server is a child process, not a jailed one: the allowlist checks argv[0],"))
	fmt.Println("  " + faint("not behaviour, and lca cannot see or attribute the connections it opens"))
}

func mcpDoctorTools(m *MCPSet, sv *MCPServer, res mcpProbeResult, bad func(string, ...any)) {
	live := map[string]mcpRawTool{}
	for _, t := range res.tools {
		live[t.Name] = t
	}
	reads, writes := 0, 0
	for _, tool := range sv.Expose {
		if mt := mcpTools[mcpJoin(sv.Name, tool)]; mt != nil {
			if mt.Write {
				writes++
			} else {
				reads++
			}
		}
	}
	match := "schemas match the lock"
	if msg := sv.checkDrift(res.tools, res.truncated); msg != "" {
		match = "the lock no longer matches what it serves"
		bad("%s", msg)
	}
	okLine("tools/list — %s; %d exposed (%d read, %d write); %s",
		plural(len(res.tools), "tool", "tools"), reads+writes, reads, writes, match)
	if res.truncated {
		warnLine("its list was truncated at %d tools / %d pages — name the ones you need in expose:", maxMCPListTools, maxMCPListPages)
	}
	for _, tool := range sv.Expose {
		if _, ok := live[tool]; !ok {
			bad("expose: names %q, which it does not serve%s", tool, didYouMean(tool, sortedKeys(live)))
			hint("run /mcp refresh; the lock then records what it does serve")
		}
	}
	// An annotation the operator has not accepted is a suggestion, not a downgrade.
	// Printing it turns it into one edit instead of a mystery.
	for _, tool := range sv.Expose {
		t, ok := live[tool]
		if !ok || t.Annotations == nil || t.Annotations.ReadOnlyHint == nil || !*t.Annotations.ReadOnlyHint {
			continue
		}
		if t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
			continue
		}
		mt := mcpTools[mcpJoin(sv.Name, tool)]
		if mt == nil || !mt.Write || mt.From == "config" {
			continue
		}
		warnLine("%s — the server says read-only, annotations are not trusted → a write", mt.Reg)
		// Not "do this": the server's own hint is the claim readWrite() exists not to
		// believe, and a copy-pasteable instruction to downgrade a state-changing tool
		// is the exact scenario the default protects against. The operator decides, and
		// the line says on whose authority.
		hint("if YOU agree it only reads, add %q to mcp.servers.%s.read_only — the server's own hint is not evidence", tool, sv.Name)
	}
	if sv.lock != nil && len(res.tools) > len(sv.Expose) {
		var unexposed []string
		for _, t := range res.tools {
			if !contains(sv.Expose, t.Name) {
				unexposed = append(unexposed, t.Name)
			}
		}
		sort.Strings(unexposed)
		if len(unexposed) > 0 {
			fmt.Println("  " + faint("also served, not exposed: %s", truncate(strings.Join(unexposed, ", "), 400)))
		}
	}
}

// doctorAgents is the set of agents doctor reasons about. Without a roles.yaml —
// the default, and the first hour of any install — doctorOrchestrator has no
// agents at all, and an empty list meant the commonest configuration got no schema
// count, no warning and no verdict on a server being down. There is still exactly
// one agent there: the entry one, carrying every registered tool because it names
// none.
func doctorAgents(orch *Orchestrator, roles *RolesConfig) []*Agent {
	// len(Roles) and not roles != nil: loadRoles hands doctor a config with NO roles
	// where buildOrchestrator would have nil, and treating that as "a team with no
	// members" is what left single-agent mode — the default — with no table at all.
	if roles != nil && len(roles.Roles) > 0 {
		return roles.Roles
	}
	var out []*Agent
	for _, k := range sortedKeys(orch.agents) {
		out = append(out, orch.agents[k])
	}
	if len(out) == 0 {
		out = append(out, &Agent{Name: firstNonEmpty(orch.cfg.Agent, "build")})
	}
	return out
}

// mcpRoleUsage counts, per server, how many roles name at least one of its tools.
// It is what decides whether a down server is a warn or a fail.
func mcpRoleUsage(orch *Orchestrator, roles *RolesConfig) map[string]int {
	out := map[string]int{}
	if orch.mcp == nil {
		return out
	}
	for _, a := range doctorAgents(orch, roles) {
		for _, name := range orch.mcp.order {
			for _, reg := range mcpNamesOf(name) {
				if !a.ToolsSet || contains(a.Tools, reg) {
					out[name]++
					break
				}
			}
		}
	}
	return out
}

// mcpDoctorRoles prints how many schemas each role carries. Every exposed tool is
// another schema in the request prefix and another chance for an open model to
// emit an invalid call, which /stats already measures — so the number is printed
// and warned about rather than left to be discovered.
func mcpDoctorRoles(orch *Orchestrator, roles *RolesConfig, usedBy map[string]int) {
	agents := doctorAgents(orch, roles)
	if len(agents) == 0 {
		return
	}
	var rows [][]string
	var warns []string
	for _, a := range agents {
		builtin, mcpN, writes, dropped := 0, 0, 0, 0
		rules := []Ruleset{defaultRules(), a.BaseRules, orch.userRules, a.Rules}
		// newChild's structural deny, so the count is the one the role will really
		// carry: a mode: subagent role only ever runs as a child, and every child of a
		// role that does not name mcp_write gets Rule{"mcp_write","*",Deny}. Without it
		// doctor over-reported exactly the number requirement 8 asks it to report, and
		// printed "mcp_write: ask" where the real answer was "deny". mentions() is the
		// same escape hatch task.go uses, so the two cannot drift apart.
		asChild := a.Mode == "subagent" && !mentions(a.Rules, "mcp_write") && !mentions(a.BaseRules, "mcp_write")
		if asChild {
			rules = append(rules, Ruleset{{"mcp_write", "*", Deny}})
		}
		for _, name := range toolOrder {
			if toolRegistry[name] == nil {
				continue
			}
			if a.ToolsSet && !contains(a.Tools, name) {
				continue
			}
			if Disabled(permissionOf(name), rules...) {
				if mt := mcpTools[name]; mt != nil && mt.Write {
					dropped++
				}
				continue
			}
			if mt := mcpTools[name]; mt != nil {
				mcpN++
				if mt.Write {
					writes++
				}
			} else {
				builtin++
			}
		}
		// Only for a role that actually carries a write schema: "mcp_write: ask" next
		// to a role that cannot write anything is a fact about nothing. And when the
		// writes were dropped, say by what — the note used to read like a grant.
		note := ""
		switch {
		case writes > 0:
			note = faint("(mcp_write: %s at the door)", Evaluate("mcp_write", "*", rules...))
		case dropped > 0 && asChild:
			note = faint("(%s dropped: a subagent that does not name mcp_write)", plural(dropped, "write", "writes"))
		case dropped > 0:
			note = faint("(%s dropped: mcp_write is denied here)", plural(dropped, "write", "writes"))
		}
		rows = append(rows, []string{a.Name, fmt.Sprint(builtin), fmt.Sprint(mcpN), fmt.Sprint(builtin + mcpN), note})
		// On the MCP count and not the total. The builtin set is twelve, so a total of
		// fifteen left a three-tool budget and the FIRST useful Jira exposure
		// (search, get, comment, transition) warned on day one — which is how an
		// operator learns to ignore a warning. The total is in the table either way.
		if mcpN > 6 {
			warns = append(warns, fmt.Sprintf("role %s carries %d mcp tool schemas (%d in all, with %d builtin)", a.Name, mcpN, builtin+mcpN, builtin))
		}
	}
	section("mcp roles", faint("schemas in the request prefix"))
	table([]string{"role", "builtin", "mcp", "schemas", ""}, rows)
	for _, w := range warns {
		warnLine("%s", w)
		fmt.Println("  " + faint("↳ every schema is prefix tokens and another chance at an invalid call; /stats measures it."))
		fmt.Println("  " + faint("  Name fewer tools in expose: or in the role."))
	}
	for _, a := range agents {
		for _, name := range orch.mcp.order {
			if a.ToolsSet && containsMCPWildcardNote(a, name) {
				warnLine("role %s uses %s__* — it expands to %s's read-only tools only; its prefix changes the next time you refresh the lock", a.Name, name, name)
			}
		}
	}
}

// containsMCPWildcardNote reports whether a role's tools: holds every read-only
// tool of a server, which is what a server__* wildcard expanded to.
func containsMCPWildcardNote(a *Agent, server string) bool {
	reads := 0
	for _, reg := range mcpNamesOf(server) {
		if mt := mcpTools[reg]; mt != nil && !mt.Write {
			reads++
			if !contains(a.Tools, reg) {
				return false
			}
		}
	}
	return reads > 1
}

// mcpFixHint turns a probe failure into the one line that fixes it.
func mcpFixHint(sv *MCPServer, err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "HTTP 401"), strings.Contains(msg, "HTTP 403"):
		// The server answered, and it answered "no". The body is already on screen,
		// scrubbed; what it never says is which variable lca read the credential from.
		if len(sv.Headers) > 0 {
			return fmt.Sprintf("the server rejected the credential — is $%s current?", sv.Headers[0].EnvVar)
		}
		return "the server rejected the credential — mcp.servers." + sv.Name + ".headers is where it comes from"
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "x509"):
		return "point mcp.servers." + sv.Name + ".ca_file at the PEM holding your internal CA — there is no verification switch to turn off"
	case strings.Contains(msg, "allow_cidrs"):
		return "add the network it resolves into to mcp.allow_cidrs, or correct the DNS entry"
	case strings.Contains(msg, "allow_hosts"):
		return "add its host:port to mcp.allow_hosts"
	case strings.Contains(msg, "redirect"):
		return "point mcp.servers." + sv.Name + ".url at where it actually serves MCP"
	case strings.Contains(msg, "not set"):
		return "export the variable before starting lca"
	case strings.Contains(msg, "refusing a"):
		return "this is the egress policy refusing, not the server failing"
	}
	return "is it up? " + firstNonEmpty(sv.URL, sv.Command) + " is what lca was told to use"
}
