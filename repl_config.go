package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Configuration from inside the session: what is in force, where it came from,
// how to change it, and how to keep it. Everything here walks the one table in
// settings.go, so /config cannot list a key /set refuses.
//
// Persistence is an explicit /save plus one faint line, never a prompt.
// approval.go is the only thing in this program allowed to stop the session and
// demand a keystroke, and it pays for that because the stakes are a side effect
// on the operator's machine. A settings change is not that, and /think,
// /approve, /loop and /model are moods an operator toggles repeatedly — a y/n
// after each one steals a keystroke mid-work and wrecks the in-session flow.

// projectConfigPath / userConfigPath are the two files /set can write.
func (r *Repl) projectConfigPath() string {
	return filepath.Join(r.orch.jl.Root, ".lca", "config.json")
}

func (r *Repl) userConfigPath() string { return filepath.Join(r.cfg.Dir, "config.json") }

func (r *Repl) configTarget(user bool) string {
	if user {
		return r.userConfigPath()
	}
	return r.projectConfigPath()
}

// sources is where each effective setting came from, seeded from the layered
// load and then overwritten as the session changes things.
func (r *Repl) sources() map[string]settingSource {
	if r.cfgSrc == nil {
		r.cfgSrc = map[string]settingSource{}
	}
	return r.cfgSrc
}

// noteChange records a session change and points at /save without blocking.
// Nothing is silently lost and nothing stops to ask.
func (r *Repl) noteChange(key, by string) {
	if findSetting(key) == nil {
		return
	}
	// applyLive drives these same command handlers on the way to writing the file,
	// and /set deletes the entry two lines later. Telling the operator to /save
	// something already persisted — and printing the change twice — is noise, and
	// the flag is the only thing that tells the two callers apart.
	if r.applying {
		mark(r.sources(), key, SrcSession, by)
		return
	}
	if r.unsaved == nil {
		r.unsaved = map[string]string{}
	}
	// The value stored is the one applySetting takes, not the one /config prints:
	// a reload re-applies these on top of the files, and "auto-approves run; asks
	// for the rest" is a sentence, not a setting.
	r.unsaved[key] = rawSettingValue(r.cfg, key)
	mark(r.sources(), key, SrcSession, by)
	// A model key in the config outranks every role's chain at the next start, so
	// with a team loaded /save is the wrong advice — it would pin all four roles.
	if key == "model" && r.teamMode() && len(r.orch.roles.Roles) > 0 {
		hint("this session only — /save would pin every role to it; /role %s model %s then /role save is the team change",
			firstNonEmpty(r.orch.roles.Entry, "lead"), r.cfg.Model)
		return
	}
	hint("/save keeps it in %s (%s)", prettyPath(r.projectConfigPath(), r.orch.jl.Root),
		plural(len(r.unsaved), "unsaved change", "unsaved changes"))
}

// ── /config ─────────────────────────────────────────────────────────────────

func (r *Repl) cmdConfig(arg string) bool {
	if k := strings.TrimSpace(arg); k != "" {
		return r.explainSetting(k)
	}
	section("config", faint("effective settings and where each comes from"))
	srcs := r.sources()
	// ONE set of column widths for the whole screen. Four table() calls, one per
	// group, put the source column at four different offsets: a staircase, on the
	// screen whose entire job is lining that column up. And table() ellipsizes only
	// its LAST column, so a long allow list or root path wrapped and took its own
	// source with it onto the next line.
	type crow struct{ group, key, val, src string }
	var rows []crow
	keyW, srcW := 0, 0
	for _, g := range settingGroups {
		for _, s := range settings {
			if s.Group != g {
				continue
			}
			src := srcs[s.Key].render(r.orch.jl.Root)
			if s.Kind == kLocator {
				src += "  (read-only)"
			} else if _, ok := r.unsaved[s.Key]; ok {
				src += gSep + "unsaved"
			}
			rows = append(rows, crow{g, s.Key, r.shownSetting(s.Key), src})
			keyW, srcW = max(keyW, visibleWidth(s.Key)), max(srcW, visibleWidth(src))
		}
	}
	valW := max(termWidth()-keyW-srcW-8, 20)
	group := ""
	for _, c := range rows {
		if c.group != group {
			group = c.group
			fmt.Printf("  %s%s%s\n", cDim, strings.ToUpper(group), cReset)
		}
		val := c.val
		if visibleWidth(val) > valW {
			val = ellipsizeMiddle(stripANSI(val), valW)
		}
		fmt.Printf("  %s  %s  %s\n", padTo(c.key, keyW, 0), padTo(val, valW, 0), faint("%s", c.src))
	}
	// A file value the environment shadows is the one thing a settings screen must
	// not hide: without it, /set looks broken. The value goes through the setting's
	// kind — printing the stored api_key here in the clear is the one exposure
	// maskKey exists to prevent.
	for _, s := range settings {
		if s.Env == "" || srcs[s.Key].Src != SrcEnv {
			continue
		}
		if fileVal, path := r.fileValue(s.JSON); fileVal != "" {
			hint("%s says %s = %s — the environment wins", prettyPath(path, r.orch.jl.Root), s.Key, s.showFileValue(fileVal))
		}
	}
	fmt.Println()
	hint("/set <key> <value> writes %s"+gSep+"/set -user … writes %s",
		prettyPath(r.projectConfigPath(), r.orch.jl.Root), prettyPath(r.userConfigPath(), r.orch.jl.Root))
	if n := len(r.unsaved); n > 0 {
		hint("/save keeps this session's %s"+gSep+"an env var overrides a file for one run", plural(n, "unsaved change", "unsaved changes"))
	} else {
		hint("%s", "an env var overrides a file for one run"+gSep+"/setup rebuilds the whole team")
	}
	return false
}

// shownSetting is the effective value as /config prints it, including the layer
// readSetting cannot see. Config holds defaultConfig()'s literals for model,
// tier, effort and transport when a team file owns them, so /config printed
// `model local · default` in a session where every request carried the lead's
// chain — on the screen whose entire purpose is "every setting WITH ITS SOURCE",
// for the two settings an operator most wants sourced.
//
// It reads the live team, not Config: the source map decided the layer, so this
// only has to say what that layer holds.
func (r *Repl) shownSetting(key string) string {
	if r.sources()[key].Src != SrcRoles {
		return readSetting(r.cfg, r.orch.ap, key)
	}
	a := r.sess.agent
	switch key {
	case "model":
		if a != nil && len(a.Models) > 0 {
			return strings.Join(a.Models, faint(" %s ", gFlow))
		}
	case "tier":
		if t := r.sess.tier(); t != "" {
			return t
		}
	case "effort":
		if a != nil && a.Thinking != "" {
			return a.Thinking
		}
	case "transport":
		if r.orch.roles != nil && r.orch.roles.Transport != "" {
			return r.orch.roles.Transport
		}
	}
	return readSetting(r.cfg, r.orch.ap, key)
}

// markRoleSources records the keys a TEAM FILE owns. SrcRoles was declared with
// two render arms and nothing anywhere assigning it, so roles.yaml's ownership of
// the model, the tier, the effort and the transport was invisible and /config
// credited defaultConfig() instead.
//
// It only ever beats the default: a file, an env var or a flag that names one of
// these outranks the team at load (main.go prefers fc.Model over the entry role's
// chain), and /config must say which one actually decides.
func markRoleSources(srcs map[string]settingSource, root string, roles *RolesConfig, entry *Agent) {
	if srcs == nil || roles == nil || len(roles.Roles) == 0 {
		return
	}
	path := rolesPath(root, roles)
	own := func(key string, when bool) {
		if when && srcs[key].Src <= SrcRoles {
			mark(srcs, key, SrcRoles, path)
		}
	}
	own("model", entry != nil && len(entry.Models) > 0)
	own("tier", entry != nil && entry.Tier != "")
	own("effort", entry != nil && entry.Thinking != "")
	own("transport", roles.Transport != "")
}

// fileValue is what the config files say about one JSON key, whatever is
// actually in force.
func (r *Repl) fileValue(jsonKey string) (string, string) {
	if jsonKey == "" || r.orch.fc == nil {
		return "", ""
	}
	return r.orch.fc.Raw[jsonKey], r.orch.fc.From[jsonKey]
}

func (r *Repl) explainSetting(key string) bool {
	s := findSetting(key)
	if s == nil {
		errLine("don't know the setting %q", key)
		hint("%s", strings.Join(settingKeys(), gSep))
		return false
	}
	section("setting", s.Key)
	row("what", faint("%s", s.Help))
	row("value", r.shownSetting(s.Key))
	row("source", faint("%s", r.sources()[s.Key].render(r.orch.jl.Root)))
	row("accepts", faint("%s", s.accepts()))
	switch {
	case s.Kind == kLocator:
		row("file", faint("never — %s locates the files, so no file may relocate it", s.Key))
	case s.JSON == "":
		row("file", faint("not persistable"))
	default:
		row("file", faint("%s: \"%s\"", prettyPath(r.projectConfigPath(), r.orch.jl.Root), s.JSON))
	}
	if s.Env != "" {
		env := faint("$%s overrides the file for one run", s.Env)
		if v, ok := os.LookupEnv(s.Env); ok && v != "" {
			env = warn("$%s is set in this shell and wins", s.Env)
		}
		row("env", env)
	}
	// Through the setting's kind, never raw: this row sat two lines under the masked
	// one and printed the stored api_key in full — on a screen shared, recorded and
	// pasted into issues, from the command whose stated job is to mask it.
	if v, path := r.fileValue(s.JSON); v != "" {
		row("in file", faint("%s = %s", prettyPath(path, r.orch.jl.Root), s.showFileValue(v)))
	}
	return false
}

// accepts is the one line a refusal quotes, so the error message and the help
// cannot drift apart.
func (s setting) accepts() string {
	switch s.Kind {
	case kEnum:
		return strings.Join(s.Enum, gSep)
	case kInt:
		return "a whole number"
	case kFloat:
		return "a number 0…2, or \"unset\""
	case kBool:
		return "on" + gSep + "off"
	case kList:
		return "names, comma-separated"
	case kSecret:
		return "a token — but see /set api_key_env"
	case kEnvName:
		return "a variable NAME, e.g. LCA_API_KEY"
	case kLocator:
		return "read-only"
	}
	return "text"
}

// ── /set ────────────────────────────────────────────────────────────────────

func (r *Repl) cmdSet(arg string) bool {
	fields := strings.Fields(arg)
	user, plaintext, force := false, false, false
	var rest []string
	for _, f := range fields {
		switch f {
		case "-user", "--user":
			user = true
		case "-plaintext", "--plaintext":
			plaintext = true
		case "-force", "--force":
			force = true
		default:
			rest = append(rest, f)
		}
	}
	if len(rest) == 0 {
		return r.listSettings()
	}
	name := rest[0]
	s := findSetting(name)
	if s == nil {
		errLine("don't know the setting %q", name)
		hint("%s", strings.Join(settingKeys(), gSep))
		return false
	}
	if len(rest) == 1 {
		return r.explainSetting(s.Key)
	}
	value := strings.Trim(strings.Join(rest[1:], " "), "\"'")

	switch {
	case s.Kind == kLocator:
		errLine("%s is where lca looks for its files, not a setting", s.Key)
		hint("set LCA_%s in the shell before starting, or start lca in the directory you mean", strings.ToUpper(s.Key))
		return false
	case s.Key == "api_key" && !plaintext:
		// A refusal, not a confirm: /set must behave identically in a pipe, and a
		// prompt would make the behaviour depend on the terminal.
		errLine("not writing a key into a file in plain text")
		hint("/set api_key_env LCA_API_KEY records the variable's NAME instead — .lca is inside")
		hint("  your repository, and one `git add -A` publishes a secret")
		hint("/set api_key <value> -plaintext does it anyway")
		return false
	case s.Key == "model" && r.teamPinWarning(force):
		return false
	}

	prev := readSetting(r.cfg, r.orch.ap, s.Key)
	if err := r.applyLive(s, value); err != nil {
		errLine("%v", err)
		if s.Kind == kEnum {
			hint("%s accepts %s", s.Key, s.accepts())
		}
		return false
	}
	shown := value
	if s.Kind == kSecret {
		shown = "<redacted>"
	}
	r.orch.rec.Event("config_set", map[string]any{"key": s.Key, "value": shown})
	okLine("%s %s %s", s.Key, faint("%s →", prev), readSetting(r.cfg, r.orch.ap, s.Key))

	if s.JSON == "" {
		hint("%s is a session switch — nothing to write", s.Key)
		return false
	}
	val, err := jsonValueOf(r.cfg, s.Key)
	if err != nil {
		errLine("%v", err)
		return false
	}
	path := r.configTarget(user)
	written, err := setConfigValues(path, map[string]any{s.JSON: val})
	if err != nil {
		errLine("could not write %s: %v", prettyPath(path, r.orch.jl.Root), err)
		return false
	}
	delete(r.unsaved, s.Key)
	mark(r.sources(), s.Key, fileSourceOf(r.cfg, written), written)
	r.reloadFileConfig()
	okLine("wrote %s   %s", prettyPath(written, r.orch.jl.Root), faint("%s", s.JSON))
	hint("rewritten as canonical JSON — key order and spacing change, values do not")
	if s.Kind == kSecret {
		warnLine("wrote api_key to %s in plain text"+gSep+"mode 0600", prettyPath(written, r.orch.jl.Root))
		warnLine("anything that can read that file can read the key, and it will show in `git diff`")
		// Both files, because the backup written beside it carries the same key and is
		// the one an operator never thinks to ignore.
		hint("add %s and %s.bak to .gitignore, or use /set api_key_env instead",
			prettyPath(written, r.orch.jl.Root), prettyPath(written, r.orch.jl.Root))
	}
	// The wizard's credentials step has always said this; /set reported success and
	// then /config swore the key was "set · from $VAR" for a variable nobody
	// exported, so the first request came back 401 after two reassurances.
	if s.Key == "api_key_env" && os.Getenv(value) == "" {
		warnLine("$%s is not set in this shell — export it before the next request", value)
	}
	if s.Env != "" {
		if v, ok := os.LookupEnv(s.Env); ok && v != "" {
			warnLine("%s is set in this shell and wins — this file takes effect where that is unset", s.Env)
		}
	}
	r.warnShadowedWrite(s, written)
	return false
}

// teamPinWarning refuses a top-level `model` key while a team is loaded. main.go
// prefers the config's model over the entry role's chain, so the key outranks
// every role's `models:` at every future start — the banner says one model and
// /agents says another, in this project, forever, and nothing used to say a word.
// setup_wizard.go already knows this and writes a model key only when there is no
// team; /set and /save did it and printed SAVED.
func (r *Repl) teamPinWarning(force bool) bool {
	if force || !r.teamMode() || len(r.orch.roles.Roles) == 0 {
		return false
	}
	entry := firstNonEmpty(r.orch.roles.Entry, "lead")
	errLine("a team is loaded — a model key in %s outranks every role's chain, at every future start",
		prettyPath(r.projectConfigPath(), r.orch.jl.Root))
	hint("/role %s model <name> changes that role, then /role save keeps it", entry)
	hint("/set model <name> -force does it anyway (every role runs it)")
	return true
}

// warnShadowedWrite says so when the file just written is read BEFORE another one
// that sets the same key: loadFileConfig walks user → project → $LCA_CONFIG and
// each later file overwrites the earlier, so `/set -user max_tokens 2222` against
// a project file that says 1111 is a setting that vanishes at the next start. The
// env case was warned about from the start; the file case was not.
func (r *Repl) warnShadowedWrite(s *setting, written string) {
	v, path := r.fileValue(s.JSON)
	if path == "" || path == written || v == "" {
		return
	}
	warnLine("%s also sets %s = %s and is read after this one — it wins at the next start",
		prettyPath(path, r.orch.jl.Root), s.Key, s.showFileValue(v))
	hint("/set %s <value> (without -user) writes that file instead", s.Key)
}

func (r *Repl) listSettings() bool {
	section("settings", faint("/set <key> <value> changes one and writes it"))
	for _, g := range settingGroups {
		var rows [][]string
		for _, s := range settings {
			if s.Group == g {
				rows = append(rows, []string{s.Key, faint("%s", s.Help)})
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Printf("  %s%s%s\n", cDim, strings.ToUpper(g), cReset)
		table(nil, rows)
	}
	hint("/config shows what each one is right now and where it came from")
	return false
}

// applyLive assigns a setting and moves the running session onto it through the
// SAME code paths the individual commands use, so the typed and the /set form
// cannot drift.
func (r *Repl) applyLive(s *setting, value string) error {
	next := r.cfg
	if err := applySetting(&next, s.Key, value); err != nil {
		return err
	}
	// The delegated handlers print their own line and record their own change; the
	// caller writes the file and then deletes the unsaved entry, so a "/save keeps
	// it" hint from inside here is advice about something already persisted.
	r.applying = true
	defer func() { r.applying = false }()
	switch s.Key {
	case "model":
		prev := r.cfg
		r.cfg = next
		r.syncCfg()
		r.cmdModel(value)
		// cmdModel returns "run a turn?", not "did it work": a hosted ref with no key
		// prints ✕ and leaves the session on the old model. Without this check /set
		// reported success, wrote the bad ref, and the NEXT start read it back from the
		// file and failed on its first request.
		if r.sess.client == nil || r.sess.client.Ref() != value {
			r.cfg = prev
			r.syncCfg()
			return fmt.Errorf("the session did not switch to %q — nothing was written", value)
		}
		return nil
	case "endpoint":
		r.cfg = next
		r.syncCfg()
		r.cmdEndpoint(next.BaseURL)
		return nil
	case "approve":
		r.cfg = next
		r.syncCfg()
		r.applyApprove(value)
		return nil
	case "tier":
		r.cfg = next
		r.syncCfg()
		return r.setTier(value)
	case "show_thinking":
		r.sess.ShowThink = next.ShowThinking
	case "loop":
		r.sess.Loop = next.Loop
	case "allow":
		r.orch.jl.SetAllowed(next.Allowed)
	case "cmd_timeout":
		cmdTimeout = time.Duration(next.CmdTimeout) * time.Second
	case "theme":
		applyTheme(next.Theme)
	}
	r.cfg = next
	r.syncCfg()
	r.sess.RefreshSystem()
	return nil
}

// syncCfg pushes the session's configuration into the orchestrator every child
// session reads, so a change is not visible only to the REPL.
func (r *Repl) syncCfg() {
	r.orch.cfg = r.cfg
	if r.local != nil {
		if r.cfg.APIKey != "" {
			r.local.apiKey = r.cfg.APIKey
		}
		r.local.maxTokens = r.cfg.MaxTokens
	}
}

// reloadFileConfig re-reads the config files so /config's "in file" column is
// what is on disk, not what it was at start-up.
func (r *Repl) reloadFileConfig() {
	if fc, err := loadFileConfig(r.cfg); err == nil {
		r.orch.fc = fc
	}
}

func (r *Repl) applyApprove(mode string) {
	applyApproveTo(r.orch.ap, mode)
	r.orch.rec.Event("approve_mode", map[string]any{"trusted": r.orch.ap.TrustedClasses()})
}

// applyApproveTo is the one place the `approve` setting becomes an Approver
// state, so a value that came from a config file means exactly what the same
// value typed at /approve means. Without it, /set approve run would write a file
// no later session read.
func applyApproveTo(ap *Approver, mode string) {
	switch mode {
	case "all", "on":
		ap.TrustAll()
	case "off":
		ap.Clear()
	case "run", "edit", "web":
		ap.Trust(mode)
	}
}

// ── /save ───────────────────────────────────────────────────────────────────

func (r *Repl) cmdSave(arg string) bool {
	fields := strings.Fields(arg)
	user, force := false, false
	var keys []string
	for _, f := range fields {
		switch f {
		case "-user", "--user":
			user = true
		case "-force", "--force":
			force = true
		default:
			keys = append(keys, f)
		}
	}
	if len(keys) == 0 {
		for k := range r.unsaved {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	if len(keys) == 0 {
		fmt.Println("  " + faint("nothing to save — /config shows what is in force"))
		return false
	}
	vals := map[string]any{}
	var named []string
	for _, k := range keys {
		s := findSetting(k)
		if s == nil {
			errLine("don't know the setting %q — nothing was written", k)
			hint("%s", strings.Join(settingKeys(), gSep))
			return false
		}
		if s.JSON == "" {
			errLine("%s is a session switch, not a setting — nothing was written", s.Key)
			return false
		}
		if s.Kind == kSecret {
			errLine("a key is never written by /save — /set api_key_env <VAR> records the variable's name")
			return false
		}
		// The one-line /save offer after /model used to walk the operator straight
		// into pinning every role — see teamPinWarning.
		if s.Key == "model" && r.teamPinWarning(force) {
			hint("/save -force writes it anyway")
			return false
		}
		v, err := jsonValueOf(r.cfg, s.Key)
		if err != nil {
			errLine("%v", err)
			return false
		}
		vals[s.JSON] = v
		named = append(named, s.Key)
	}
	path := r.configTarget(user)
	written, err := setConfigValues(path, vals)
	if err != nil {
		errLine("could not write %s: %v", prettyPath(path, r.orch.jl.Root), err)
		return false
	}
	section("saved", prettyPath(written, r.orch.jl.Root))
	for _, k := range named {
		row(k, readSetting(r.cfg, r.orch.ap, k))
		delete(r.unsaved, k)
		mark(r.sources(), k, fileSourceOf(r.cfg, written), written)
	}
	r.reloadFileConfig()
	for _, k := range named {
		if s := findSetting(k); s != nil {
			r.warnShadowedWrite(s, written)
		}
	}
	r.orch.rec.Event("config_saved", map[string]any{"path": written, "keys": named})
	return false
}

// rawSettingValue is a setting's current value in the spelling applySetting
// accepts — the JSON representation flattened back to text.
func rawSettingValue(c Config, key string) string {
	v, err := jsonValueOf(c, key)
	if err != nil || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return onOffWord(t)
	case []string:
		return strings.Join(t, ",")
	}
	return fmt.Sprint(v)
}

func onOffWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// exitNotice is the one line /exit prints when a session change was never
// written. It blocks nothing and asks nothing: it just refuses to let a change
// disappear without the operator having been told.
func (r *Repl) exitNotice() {
	if len(r.unsaved) == 0 {
		return
	}
	var keys []string
	for k := range r.unsaved {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	verb, them := "changed this session and is", "it"
	if len(keys) > 1 {
		verb, them = "changed this session and are", "them"
	}
	warnLine("%s %s not in a file — /save keeps %s", strings.Join(keys, ", "), verb, them)
}

// ── /tier ───────────────────────────────────────────────────────────────────

func hasTiers(r *Repl) bool {
	return r.orch.roles != nil && len(r.orch.roles.TierOrder) > 0
}

func (r *Repl) cmdTier(arg string) bool {
	o := r.orch
	if o.roles == nil || len(o.roles.TierOrder) == 0 {
		errLine("this team declares no tiers")
		hint("%s", "tiers: in .lca/roles.yaml names model chains roles can share"+gSep+"/setup offers them")
		return false
	}
	name := strings.TrimSpace(arg)
	if name == "" {
		if !r.in.IsTTY() {
			section("tiers")
			for _, t := range o.roles.TierOrder {
				mark := " "
				if t == o.roles.Tier {
					mark = cGreen + gUp + cReset
				}
				fmt.Printf("  %s %s  %s\n", mark, t, faint("%s", strings.Join(o.roles.Tiers[t], " "+gFlow+" ")))
			}
			hint("/tier <name> switches"+gSep+"tiers: %s", o.roles.tierList())
			return false
		}
		cs := []choice{{id: "", label: "none", detail: faint("each role runs the tier it declares"), on: o.roles.Tier == ""}}
		for _, t := range o.roles.TierOrder {
			cs = append(cs, choice{id: t, label: t, detail: strings.Join(o.roles.Tiers[t], faint(" %s ", gFlow)), on: t == o.roles.Tier})
		}
		i, err := pickOne(r.in, cs, pickOpts{title: "tier", detail: faint("every tier-declaring role runs this chain")})
		if err != nil || i < 0 {
			return false
		}
		name = cs[i].id
	}
	if err := r.setTier(name); err != nil {
		errLine("%v", err)
		return false
	}
	r.cfg.Tier = name
	r.syncCfg()
	okLine("tier %s", firstNonEmpty(name, faint("off — each role runs the tier it declares")))
	r.noteChange("tier", "/tier")
	return false
}

// setTier remaps every tier-declaring role and re-points the running session, so
// the switch is visible in the very next turn and not after a restart.
func (r *Repl) setTier(name string) error {
	o := r.orch
	if o.roles == nil {
		return fmt.Errorf("this team declares no tiers")
	}
	if name != "" {
		if _, ok := o.roles.Tiers[name]; !ok {
			return fmt.Errorf("no tier %q (tiers: %s)", name, o.roles.tierList())
		}
	}
	o.roles.Tier = name
	for _, a := range o.roles.Roles {
		if a.Tier != "" {
			o.roles.applyTier(a)
		}
	}
	if a := r.sess.agent; len(a.Models) > 0 {
		r.sess.models = append([]string(nil), a.Models...)
		r.sess.transport = ""
		r.sess.useModel(0)
		r.sess.RefreshSystem()
	}
	o.rec.Event("tier_change", map[string]any{"tier": name})
	return nil
}
