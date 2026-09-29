package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// One descriptor table, walked by /config, /set, /save, the wizard and the
// layered merge. A new setting is one row, and /config cannot list a key /set
// refuses. Application and reading are exhaustive switches over Key, in the
// shape cmdRole's typed assignment already uses, so a renamed Config field is a
// compile error and not a dead string.

type kind uint8

const (
	kStr kind = iota
	kInt
	kFloat
	kBool
	kEnum
	kList
	kSecret  // never printed, never written without saying so
	kEnvName // the NAME of a variable, which is not a secret
	kLocator // where the files are; read-only, see below
)

type setting struct {
	Key     string   // what /set takes
	Aliases []string // other spellings, including the JSON one
	JSON    string   // key in config.json ("" = not persistable)
	Env     string   // LCA_* ("" = no env override)
	Group   string   // endpoint | model | limits | modes | sandbox | locators
	Help    string
	Kind    kind
	Enum    []string
	Live    bool // a change takes effect in the running session at once
}

var settingGroups = []string{"endpoint", "model", "limits", "modes", "sandbox", "locators"}

var effortLevels = []string{"off", "on", "low", "medium", "high", "xhigh", "max"}

// settings is the whole surface of what a session can configure and persist.
//
// Two deliberate absences, both foot-guns a config writer must not hand anybody:
//   - `unsafe` — a file that lifts the sandbox for every future run in a
//     directory. /unsafe stays a session switch.
//   - `root` and `dir` — a file that relocates the directory it was found in is
//     a paradox, so they come from flags, env and cwd only and are listed
//     read-only.
var settings = []setting{
	{Key: "endpoint", Aliases: []string{"base_url", "url"}, JSON: "base_url", Env: "LCA_BASE_URL", Group: "endpoint", Kind: kStr, Live: true,
		Help: "the gateway or endpoint, including the API base path (…/v1)"},
	{Key: "endpoints", JSON: "endpoints", Env: "LCA_ENDPOINTS", Group: "endpoint", Kind: kList,
		Help: "other endpoints /endpoint can switch to, comma-separated"},
	{Key: "api_key", JSON: "api_key", Env: "LCA_API_KEY", Group: "endpoint", Kind: kSecret,
		Help: "the token sent to the endpoint — prefer api_key_env"},
	{Key: "api_key_env", JSON: "api_key_env", Group: "endpoint", Kind: kEnvName,
		Help: "the NAME of the variable holding the key; the file records the name, not the secret"},

	{Key: "model", JSON: "model", Env: "LCA_MODEL", Group: "model", Kind: kStr, Live: true,
		Help: "a served model id, or provider/model for a hosted API"},
	{Key: "tier", JSON: "tier", Env: "LCA_TIER", Group: "model", Kind: kStr, Live: true,
		Help: "the active tier: every tier-declaring role runs that chain (roles.yaml tiers:)"},
	{Key: "effort", Aliases: []string{"thinking"}, JSON: "thinking", Env: "LCA_THINKING", Group: "model", Kind: kEnum, Enum: effortLevels,
		Help: "how hard the model thinks (this is the level SENT; /think only shows or hides it)"},
	{Key: "transport", Aliases: []string{"tools"}, JSON: "tools", Env: "LCA_TOOLS", Group: "model", Kind: kEnum, Enum: []string{"native", "text", "auto"},
		Help: "tool-call format: the model's own (native), tags in the text, or decide per model"},
	{Key: "temperature", Aliases: []string{"temp"}, JSON: "temperature", Env: "LCA_TEMPERATURE", Group: "model", Kind: kFloat,
		Help: "0…2, or \"unset\" to send none and let the model card decide"},

	{Key: "context", Aliases: []string{"ctx_tokens"}, JSON: "ctx_tokens", Env: "LCA_CTX_TOKENS", Group: "limits", Kind: kInt,
		Help: "transcript budget in tokens (0 = from the model's window)"},
	{Key: "max_tokens", JSON: "max_tokens", Env: "LCA_MAX_TOKENS", Group: "limits", Kind: kInt,
		Help: "reply ceiling per request (0 = let the server decide)"},
	{Key: "steps", Aliases: []string{"max_steps"}, JSON: "max_steps", Env: "LCA_MAX_STEPS", Group: "limits", Kind: kInt,
		Help: "tool-call iterations allowed in one turn"},
	{Key: "cmd_timeout", JSON: "cmd_timeout", Env: "LCA_CMD_TIMEOUT", Group: "limits", Kind: kInt,
		Help: "run_command timeout in seconds"},
	{Key: "subagent_depth", JSON: "subagent_depth", Env: "LCA_SUBAGENT_DEPTH", Group: "limits", Kind: kInt,
		Help: "how deep subagents may nest"},

	{Key: "agent", Aliases: []string{"role"}, JSON: "agent", Env: "LCA_AGENT", Group: "modes", Kind: kStr,
		Help: "the primary agent or role a session starts as"},
	{Key: "approve", JSON: "approve", Group: "modes", Kind: kEnum, Enum: []string{"off", "run", "edit", "web", "all"}, Live: true,
		Help: "what runs without asking"},
	{Key: "show_thinking", JSON: "show_thinking", Env: "LCA_SHOW_THINKING", Group: "modes", Kind: kBool, Live: true,
		Help: "show the model's reasoning in full instead of one status line"},
	{Key: "loop", JSON: "loop", Env: "LCA_LOOP", Group: "modes", Kind: kBool, Live: true,
		Help: "keep working until the task reports itself done"},

	{Key: "allow", JSON: "allow", Env: "LCA_ALLOW", Group: "sandbox", Kind: kList,
		Help: "the command allowlist (roles.yaml sandbox.allow overrides it)"},

	{Key: "root", Env: "LCA_ROOT", Group: "locators", Kind: kLocator, Help: "the project root — from -C/cwd or LCA_ROOT, never from a file"},
	{Key: "dir", Env: "LCA_DIR", Group: "locators", Kind: kLocator, Help: "where config.json, roles.yaml and state live"},
}

func settingKeys() []string {
	var out []string
	for _, s := range settings {
		if s.Kind != kLocator {
			out = append(out, s.Key)
		}
	}
	return out
}

// findSetting resolves a key or one of its aliases, case-insensitively.
func findSetting(name string) *setting {
	n := strings.ToLower(strings.TrimSpace(name))
	for i := range settings {
		if settings[i].Key == n {
			return &settings[i]
		}
		for _, a := range settings[i].Aliases {
			if a == n {
				return &settings[i]
			}
		}
	}
	return nil
}

var reEnvName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ── layers ──────────────────────────────────────────────────────────────────

// Source is where an effective setting came from, so /config can say WHY a value
// is in force and not merely what it is. A later source wins.
type Source uint8

const (
	SrcDefault     Source = iota // the literal in defaultConfig()
	SrcRoles                     // .lca/roles.yaml, for the three keys the team file owns
	SrcUserFile                  // $LCA_DIR/config.json
	SrcProjectFile               // <root>/.lca/config.json
	SrcExtraFile                 // $LCA_CONFIG
	SrcEnv                       // LCA_* present and non-empty
	SrcFlag                      // -model, -tier, -y, -unsafe, -role
	SrcSession                   // /set, /model, /endpoint, /tier, /approve, /think, /loop, /setup
)

func (s Source) String() string {
	switch s {
	case SrcRoles:
		return "roles.yaml"
	case SrcUserFile, SrcProjectFile, SrcExtraFile:
		return "config file"
	case SrcEnv:
		return "env"
	case SrcFlag:
		return "flag"
	case SrcSession:
		return "session"
	}
	return "default"
}

// settingSource is one setting's origin: which layer, and which file or command
// inside it.
type settingSource struct {
	Src  Source
	Path string // the file, or the command that set it
}

// render names the origin the way /config prints it: the layer plus the thing
// that carries it, so "a config file" is never the answer.
func (ss settingSource) render(root string) string {
	switch ss.Src {
	case SrcEnv:
		return "env " + ss.Path
	case SrcSession:
		return "session (" + firstNonEmpty(ss.Path, "/set") + ")"
	case SrcFlag:
		return "flag " + ss.Path
	case SrcRoles:
		return prettyPath(ss.Path, root) + " (team file)"
	case SrcUserFile, SrcProjectFile, SrcExtraFile:
		return prettyPath(ss.Path, root)
	}
	return "default"
}

// ── applying one value ──────────────────────────────────────────────────────

// applySetting validates raw and assigns it to c. It is the single place a
// setting's type is enforced, so /set, the file merge and the env merge cannot
// disagree about what a valid value is.
func applySetting(c *Config, key, raw string) error {
	s := findSetting(key)
	if s == nil {
		return fmt.Errorf("don't know the setting %q", key)
	}
	v := strings.TrimSpace(raw)
	switch s.Kind {
	case kLocator:
		return fmt.Errorf("%s is where lca looks for its files, not a setting — set it with LCA_%s before starting, or cd there", s.Key, strings.ToUpper(s.Key))
	case kEnum:
		if !containsStr(s.Enum, strings.ToLower(v)) {
			return fmt.Errorf("%s is one of %s, got %q", s.Key, strings.Join(s.Enum, " "), v)
		}
		v = strings.ToLower(v)
	case kInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s wants a whole number, got %q", s.Key, v)
		}
		switch s.Key {
		case "steps", "cmd_timeout", "subagent_depth":
			if n <= 0 {
				return fmt.Errorf("%s must be greater than 0", s.Key)
			}
		default:
			if n < 0 {
				return fmt.Errorf("%s cannot be negative", s.Key)
			}
		}
	case kFloat:
		if !isUnsetWord(v) {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s wants a number between 0 and 2, or \"unset\", got %q", s.Key, v)
			}
			if f < 0 || f > 2 {
				return fmt.Errorf("%s must be between 0 and 2 (got %s) — \"unset\" sends none at all", s.Key, v)
			}
		}
	case kBool:
		if _, ok := parseBool(v); !ok {
			return fmt.Errorf("%s is on or off, got %q", s.Key, v)
		}
	case kEnvName:
		if !reEnvName.MatchString(v) {
			return fmt.Errorf("%s wants a variable NAME like LCA_API_KEY (upper case, digits, _), got %q", s.Key, v)
		}
	}

	switch s.Key {
	case "endpoint":
		u, note := normalizeEndpoint(v)
		if u == "" {
			return fmt.Errorf("endpoint wants a url like http://node:18080/v1, got %q", v)
		}
		c.BaseURL = u
		c.Endpoints = appendUnique(append([]string{u}, c.Endpoints...), u)
		if note != "" {
			hint("%s", note)
		}
	case "endpoints":
		var eps []string
		for _, e := range splitFields(v) {
			if u, _ := normalizeEndpoint(e); u != "" {
				eps = appendUnique(eps, u)
			}
		}
		c.Endpoints = eps
		if c.BaseURL != "" {
			c.Endpoints = appendUnique(append([]string{c.BaseURL}, eps...), c.BaseURL)
		}
	case "api_key":
		c.APIKey = v
	case "api_key_env":
		c.APIKeyEnv = v
		if got := os.Getenv(v); got != "" {
			c.APIKey = got
		}
	case "model":
		c.Model = v
	case "tier":
		c.Tier = v
	case "effort":
		c.Thinking = v
	case "transport":
		c.Tools = v
	case "temperature":
		if isUnsetWord(v) {
			c.Temperature = -1 // Config's own encoding of "send nothing"
			break
		}
		f, _ := strconv.ParseFloat(v, 64)
		c.Temperature = f
	case "context":
		c.CtxTokens, _ = strconv.Atoi(v)
	case "max_tokens":
		c.MaxTokens, _ = strconv.Atoi(v)
	case "steps":
		c.MaxSteps, _ = strconv.Atoi(v)
	case "cmd_timeout":
		c.CmdTimeout, _ = strconv.Atoi(v)
	case "subagent_depth":
		c.SubagentMax, _ = strconv.Atoi(v)
	case "agent":
		c.Agent = v
	case "approve":
		c.Approve = v
	case "show_thinking":
		c.ShowThinking, _ = parseBool(v)
	case "loop":
		c.Loop, _ = parseBool(v)
	case "allow":
		if list := splitFields(v); len(list) > 0 {
			c.Allowed = list
		}
	default:
		return fmt.Errorf("%s cannot be set (no assignment is defined for it)", s.Key)
	}
	return nil
}

// readSetting is the effective value as /config prints it. A secret is masked
// here and nowhere else, so a future caller cannot forget to.
func readSetting(c Config, ap *Approver, key string) string {
	s := findSetting(key)
	if s == nil {
		return ""
	}
	switch s.Key {
	case "endpoint":
		return c.BaseURL
	case "endpoints":
		if len(c.Endpoints) == 0 {
			return faint("none")
		}
		var hs []string
		for _, e := range c.Endpoints {
			hs = append(hs, hostOf(e))
		}
		return strings.Join(hs, ", ")
	case "api_key":
		return maskKey(c.APIKey, c.APIKeyEnv)
	case "api_key_env":
		if c.APIKeyEnv == "" {
			return faint("unset")
		}
		return c.APIKeyEnv
	case "model":
		return orUnset(c.Model)
	case "tier":
		return orUnset(c.Tier)
	case "effort":
		return orElse(c.Thinking, "the model's own default")
	case "transport":
		return orElse(c.Tools, "as the provider or roles.yaml says")
	case "temperature":
		if c.Temperature < 0 {
			return faint("unset · the profile decides")
		}
		return strconv.FormatFloat(c.Temperature, 'f', -1, 64)
	case "context":
		if c.CtxTokens <= 0 {
			return faint("auto · from the model's window")
		}
		return kfmt(c.CtxTokens)
	case "max_tokens":
		if c.MaxTokens <= 0 {
			return faint("auto · the server decides")
		}
		return kfmt(c.MaxTokens)
	case "steps":
		return strconv.Itoa(c.MaxSteps)
	case "cmd_timeout":
		return strconv.Itoa(c.CmdTimeout) + faint("s")
	case "subagent_depth":
		return strconv.Itoa(c.SubagentMax)
	case "agent":
		return orUnset(c.Agent)
	case "approve":
		if ap != nil {
			return approvalShort(ap)
		}
		return orElse(c.Approve, "asks first")
	case "show_thinking":
		return onOff(c.ShowThinking)
	case "loop":
		return onOff(c.Loop)
	case "allow":
		return strings.Join(c.Allowed, " ")
	case "root":
		return shortDir(c.Root)
	case "dir":
		return shortDir(c.Dir)
	}
	return ""
}

// noAuthKey is defaultConfig()'s placeholder: a gateway that wants no token
// still gets a bearer header, and "sk-noauth" is what it gets. It is not a
// configured key and must not be shown as one.
const noAuthKey = "sk-noauth"

// maskKey is how a key is ALWAYS shown. The literal never appears in any
// command's output: a screen is shared, recorded and pasted into issues.
func maskKey(key, fromEnv string) string {
	switch {
	case fromEnv != "" && os.Getenv(fromEnv) == "":
		// api_key_env naming a variable nobody exported is how an operator gets a 401
		// having been told twice that the key is fine. The NAME is not a secret, so it
		// is printed: it is the whole of what has to be fixed.
		return faint("unset · $%s names it, but it is not set in this shell", fromEnv)
	case key == "":
		return faint("unset")
	case fromEnv != "":
		return "set " + faint("· from $%s", fromEnv)
	case key == noAuthKey:
		return faint("none · the gateway needs none")
	case len(key) <= 8:
		return "set " + faint("· %d chars", len(key))
	}
	return key[:3] + faint("…") + key[len(key)-3:] + faint(" (%d chars)", len(key))
}

// showFileValue is how a value read out of a config FILE is printed. Every
// render of a file value goes through the setting's own kind, so a secret cannot
// be printed in the clear by a caller that forgot: /config <key>, /set <key> with
// no value and /config's env-shadow hint each used to print the literal api_key
// from config.json a line or two under its own mask.
func (s setting) showFileValue(v string) string {
	if s.Kind == kSecret {
		return maskKey(v, "")
	}
	return v
}

// normalizeEndpoint canonicalises a url the way loadConfig does, and says what
// it changed. A missing /v1 is the single most common setup error in this
// program, so appending it is announced rather than done quietly.
func normalizeEndpoint(v string) (string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	var notes []string
	if !strings.Contains(v, "://") {
		v = "http://" + v
		notes = append(notes, "no scheme given — assuming http://")
	}
	v = strings.TrimRight(v, "/")
	// host[:port] with no path at all: the API base path is what a request is
	// built on, and hints.go already carries this sentence.
	rest := v[strings.Index(v, "://")+3:]
	if !strings.Contains(rest, "/") {
		v += "/v1"
		notes = append(notes, "the API base path matters — appended /v1")
	}
	return v, strings.Join(notes, " · ")
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func isUnsetWord(s string) bool {
	switch strings.ToLower(s) {
	case "unset", "none", "-", "off", "default":
		return true
	}
	return false
}

func parseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "yes", "1":
		return true, true
	case "off", "false", "no", "0":
		return false, true
	}
	return false, false
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return faint("off")
}

func orUnset(s string) string {
	if s == "" {
		return faint("unset")
	}
	return s
}

// orElse is a value or the sentence that explains its absence — "unset" alone
// does not say what happens instead.
func orElse(s, what string) string {
	if s == "" {
		return faint("%s", what)
	}
	return s
}

// jsonValueOf renders a setting for config.json in its own type, so a number is
// a number and a flag is a bool — a file full of quoted numbers is one the next
// reader distrusts.
func jsonValueOf(c Config, key string) (any, error) {
	s := findSetting(key)
	if s == nil {
		return nil, fmt.Errorf("don't know the setting %q", key)
	}
	if s.JSON == "" {
		return nil, fmt.Errorf("%s is a session switch, not a setting", s.Key)
	}
	switch s.Key {
	case "endpoint":
		return c.BaseURL, nil
	case "endpoints":
		var out []string
		for _, e := range c.Endpoints {
			if e != c.BaseURL {
				out = append(out, e)
			}
		}
		return out, nil
	case "api_key":
		return c.APIKey, nil
	case "api_key_env":
		return c.APIKeyEnv, nil
	case "model":
		return c.Model, nil
	case "tier":
		return c.Tier, nil
	case "effort":
		return c.Thinking, nil
	case "transport":
		return c.Tools, nil
	case "temperature":
		if c.Temperature < 0 {
			return nil, nil // nil deletes the key: "unset" means send nothing
		}
		return c.Temperature, nil
	case "context":
		return c.CtxTokens, nil
	case "max_tokens":
		return c.MaxTokens, nil
	case "steps":
		return c.MaxSteps, nil
	case "cmd_timeout":
		return c.CmdTimeout, nil
	case "subagent_depth":
		return c.SubagentMax, nil
	case "agent":
		return c.Agent, nil
	case "approve":
		return c.Approve, nil
	case "show_thinking":
		return c.ShowThinking, nil
	case "loop":
		return c.Loop, nil
	case "allow":
		return c.Allowed, nil
	}
	return nil, fmt.Errorf("%s has no file representation", s.Key)
}
