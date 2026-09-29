package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds runtime configuration, resolved from env at startup. The
// optional JSON config file (fileconfig.go) adds providers, agents and
// permission rules on top; there is no auth layer — identity comes from the
// process (uid/gid of whoever ran the binary).
type Config struct {
	Root         string   // realpath jail root; nothing may be touched outside it
	BaseURL      string   // current OpenAI-compatible endpoint, e.g. http://localhost:8000/v1
	Endpoints    []string // known endpoints (BaseURL + LCA_ENDPOINTS), for /endpoint switching
	Model        string   // model name as served by vLLM/SGLang
	APIKey       string   // optional; most local servers accept any token
	Temperature  float64  // low by default for deterministic tool use
	MaxSteps     int      // safety cap on tool-call iterations per user turn
	MaxTokens    int      // max_tokens per request (0 = let the server decide)
	CmdTimeout   int      // run_command timeout in seconds (LCA_CMD_TIMEOUT)
	CtxTokens    int      // approximate token budget for the transcript sent to the model
	Allowed      []string // command allowlist (matched against basename of argv[0])
	Dir          string   // config (config.json, roles.yaml, agents…) and, by default, state
	StateDir     string   // audit log, transcripts, traces, worktrees; "" = Dir
	Raw          bool     // stream raw model text (show tool tags) — for protocol debugging
	Discover     bool     // query /models to adopt/validate the model (off = trust configured name)
	Reservation  string   // Slurm reservation to scope /discover (LCA_RESERVATION)
	DiscoverUser string   // Slurm user filter for /discover (LCA_USER; "$me" = you)
	Scheme       string   // http|https for discovered endpoints (LCA_SCHEME)
	ShowThinking bool     // start with reasoning expanded (LCA_SHOW_THINKING; default collapsed)
	Loop         bool     // autonomous loop mode: keep going until TASK_DONE (LCA_LOOP)
	Unsafe       bool     // disable the jail + command allowlist (LCA_UNSAFE / -unsafe)
	KeepSessions int      // max transcript files to retain (LCA_KEEP_SESSIONS)
	Tools        string   // tool transport override: native | text | auto (LCA_TOOLS)
	Thinking     string   // reasoning mode: on | off | low | medium | high | max (LCA_THINKING)
	Agent        string   // primary agent to start with (LCA_AGENT)
	SubagentMax  int      // max subagent nesting depth (LCA_SUBAGENT_DEPTH)
	Tier         string   // active model tier (-tier / LCA_TIER); remaps every role that names one
	APIKeyEnv    string   // the NAME of the variable APIKey was read from (config.json api_key_env)
	Approve      string   // approval posture: off | run | edit | web | all (config.json approve)

	TransportOverride string // force a tool transport for the whole run (eval -transport)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadConfig resolves the configuration in layers: the literals, then the
// locators (which decide where the files are), then the files, then env. It
// keeps its signature — every caller that had it still has it.
func loadConfig() Config {
	cfg, _, _ := loadConfigWithSources()
	return cfg
}

// loadConfigWithSources is loadConfig plus what /config needs to explain itself:
// the decoded file config, and which layer each setting's effective value came
// from. A value with no provenance is a value nobody can argue with.
func loadConfigWithSources() (Config, *FileConfig, map[string]settingSource) {
	cfg := defaultConfig()
	srcs := map[string]settingSource{}
	applyLocatorEnv(&cfg, srcs)
	fc, err := loadFileConfig(cfg)
	if err != nil {
		// The file is malformed. Refusing to start is loadFileConfig's own policy
		// and setupOrchestrator reports it properly; here we carry on with the
		// defaults so the message comes from there and not from a half-built
		// config.
		fc = &FileConfig{Providers: map[string]ProviderConfig{}, Agents: map[string]AgentConfig{}}
	}
	applyFileConfig(&cfg, fc, srcs)
	applyEnvConfig(&cfg, srcs)
	cfg.Endpoints = endpointList(cfg)
	return cfg, fc, srcs
}

// defaultConfig is the literals and nothing else — no env, no files. Every
// number a default supplies has to be visible in one place, or "where did 50
// steps come from?" has no answer.
func defaultConfig() Config {
	return Config{
		BaseURL:      "http://localhost:8000/v1",
		Model:        "local",
		APIKey:       "sk-noauth",
		Temperature:  -1, // unset: send no temperature unless a profile, role or setting says so
		MaxSteps:     50,
		CtxTokens:    0, // 0 = auto: derive from the model's window
		MaxTokens:    0,
		CmdTimeout:   120,
		Scheme:       "http",
		KeepSessions: 200,
		Agent:        "build",
		SubagentMax:  1,
		Allowed: []string{
			"ls", "cat", "pwd", "head", "tail", "wc",
			"git", "go", "gofmt", "grep", "rg", "find", "echo",
			// reaching another machine is an outbound connection, no ports opened;
			// what they carry is still checked (jail.go: GPU policy inside ssh)
			"ssh", "scp", "rsync", "bsk",
		},
	}
}

// applyLocatorEnv resolves where lca looks for everything. It must run FIRST and
// takes no part in the file layers: a file that relocated the directory it was
// found in would be a paradox.
func applyLocatorEnv(c *Config, srcs map[string]settingSource) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		home = cwd
	}
	c.Root, c.Dir = cwd, filepath.Join(home, ".lca")
	mark(srcs, "root", SrcDefault, "cwd")
	mark(srcs, "dir", SrcDefault, "$HOME/.lca")
	if v, ok := os.LookupEnv("LCA_ROOT"); ok && v != "" {
		c.Root = v
		mark(srcs, "root", SrcEnv, "LCA_ROOT")
	}
	if v, ok := os.LookupEnv("LCA_DIR"); ok && v != "" {
		c.Dir = v
		mark(srcs, "dir", SrcEnv, "LCA_DIR")
	}
}

// applyFileConfig merges the config files' scalars, recording which file set
// each one. fc.From already says which file that was, because "a config file"
// is not an answer an operator can act on.
func applyFileConfig(c *Config, fc *FileConfig, srcs map[string]settingSource) {
	if fc == nil {
		return
	}
	for _, s := range settings {
		if s.JSON == "" || s.Kind == kLocator {
			continue
		}
		raw, ok := fc.Raw[s.JSON]
		if !ok {
			continue
		}
		if err := applySetting(c, s.Key, raw); err != nil {
			warnLine("%s: %v", prettyPath(fc.From[s.JSON], c.Root), err)
			continue
		}
		mark(srcs, s.Key, fileSourceOf(*c, fc.From[s.JSON]), fc.From[s.JSON])
	}
	// api_key_env resolves HERE, where the file that named the variable is known,
	// so /config can say "api_key_env → .lca/config.json" rather than inventing a
	// source for a value it took from the environment.
	if fc.APIKeyEnv != "" {
		if v := os.Getenv(fc.APIKeyEnv); v != "" {
			c.APIKey, c.APIKeyEnv = v, fc.APIKeyEnv
			mark(srcs, "api_key", fileSourceOf(*c, fc.From["api_key_env"]), "api_key_env → "+fc.From["api_key_env"])
		}
	}
	if fc.SubagentDepth > 0 {
		c.SubagentMax = fc.SubagentDepth
	}
}

// fileSourceOf names which of the three file layers a path belongs to, so
// /config's explanation of a shadowed value points at the right file.
func fileSourceOf(c Config, path string) Source {
	switch {
	case path == "":
		return SrcUserFile
	case path == os.Getenv("LCA_CONFIG"):
		return SrcExtraFile
	case strings.HasPrefix(path, filepath.Join(c.Root, ".lca")):
		return SrcProjectFile
	}
	return SrcUserFile
}

// applyEnvConfig is the override layer. It uses os.LookupEnv and not env(): a
// file value must be beaten only when the variable is ACTUALLY present and
// non-empty, which is the whole of "env is an override".
//
// Env stays above the files deliberately. Demoting it means it stops being
// REQUIRED, not that it loses: flipping the order would silently break every
// existing `export LCA_BASE_URL` the moment a .lca/config.json appeared, and
// would make a checked-in project file un-overridable in CI. The honesty is paid
// for in /config, which prints the shadowed file value with its path, and in
// /set, which warns at the moment it matters.
func applyEnvConfig(c *Config, srcs map[string]settingSource) {
	for _, s := range settings {
		if s.Env == "" || s.Kind == kLocator {
			continue
		}
		v, ok := os.LookupEnv(s.Env)
		if !ok || v == "" {
			continue
		}
		if s.Kind == kBool {
			// LCA_SHOW_THINKING and LCA_LOOP have always been "set = on", and a
			// shell that exports them as 1 or true must keep meaning the same.
			if _, named := parseBool(v); !named {
				v = "on"
			}
		}
		if err := applySetting(c, s.Key, v); err != nil {
			warnLine("%s: %v", s.Env, err)
			continue
		}
		mark(srcs, s.Key, SrcEnv, s.Env)
	}
	// Settings with no row of their own: debugging switches and Slurm discovery,
	// which nothing persists because they describe one run.
	c.Raw = os.Getenv("LCA_RAW") != ""
	c.Discover = os.Getenv("LCA_DISCOVER") != ""
	c.Reservation = os.Getenv("LCA_RESERVATION")
	c.DiscoverUser = os.Getenv("LCA_USER")
	c.Scheme = env("LCA_SCHEME", c.Scheme)
	c.Unsafe = os.Getenv("LCA_UNSAFE") != ""
	c.KeepSessions = atoiDefault(os.Getenv("LCA_KEEP_SESSIONS"), c.KeepSessions)
}

// endpointList is the known endpoints: the current BaseURL first, then the rest,
// deduped and trailing-slash-trimmed.
func endpointList(c Config) []string {
	out := []string{c.BaseURL}
	for _, e := range c.Endpoints {
		out = appendUnique(out, strings.TrimRight(e, "/"))
	}
	return out
}

func mark(srcs map[string]settingSource, key string, src Source, path string) {
	if srcs != nil {
		srcs[key] = settingSource{Src: src, Path: path}
	}
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func splitFields(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// stateDir is where run artifacts go (eval separates it from the config dir).
func (c Config) stateDir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	return c.Dir
}
