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

	TransportOverride string // force a tool transport for the whole run (eval -transport)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		home = cwd
	}
	cfg := Config{
		Root:         env("LCA_ROOT", cwd),
		Dir:          env("LCA_DIR", filepath.Join(home, ".lca")),
		BaseURL:      strings.TrimRight(env("LCA_BASE_URL", "http://localhost:8000/v1"), "/"),
		Model:        env("LCA_MODEL", "local"),
		APIKey:       env("LCA_API_KEY", "sk-noauth"),
		Temperature:  -1, // unset: send no temperature unless a profile, role or LCA_TEMPERATURE says so
		MaxSteps:     atoiDefault(os.Getenv("LCA_MAX_STEPS"), 50),
		CtxTokens:    atoiDefault(os.Getenv("LCA_CTX_TOKENS"), 0), // 0 = auto: derive from the model's window
		MaxTokens:    atoiDefault(os.Getenv("LCA_MAX_TOKENS"), 0),
		CmdTimeout:   atoiDefault(os.Getenv("LCA_CMD_TIMEOUT"), 120),
		Raw:          os.Getenv("LCA_RAW") != "",
		Discover:     os.Getenv("LCA_DISCOVER") != "",
		Reservation:  os.Getenv("LCA_RESERVATION"),
		DiscoverUser: os.Getenv("LCA_USER"),
		Scheme:       env("LCA_SCHEME", "http"),
		ShowThinking: os.Getenv("LCA_SHOW_THINKING") != "", // default: reasoning collapsed into the live status line
		Loop:         os.Getenv("LCA_LOOP") != "",
		Unsafe:       os.Getenv("LCA_UNSAFE") != "",
		KeepSessions: atoiDefault(os.Getenv("LCA_KEEP_SESSIONS"), 200),
		Tools:        strings.ToLower(os.Getenv("LCA_TOOLS")),
		Thinking:     os.Getenv("LCA_THINKING"),
		Agent:        env("LCA_AGENT", "build"),
		SubagentMax:  atoiDefault(os.Getenv("LCA_SUBAGENT_DEPTH"), 1),
		Tier:         os.Getenv("LCA_TIER"),
		Allowed: []string{
			"ls", "cat", "pwd", "head", "tail", "wc",
			"git", "go", "gofmt", "grep", "rg", "find", "echo",
			// reaching another machine is an outbound connection, no ports opened;
			// what they carry is still checked (jail.go: GPU policy inside ssh)
			"ssh", "scp", "rsync", "bsk",
		},
	}
	if v := os.Getenv("LCA_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			cfg.Temperature = f
		}
	}
	if v := os.Getenv("LCA_ALLOW"); v != "" {
		cfg.Allowed = splitFields(v)
	}

	// Known endpoints: the current BaseURL first, then any from LCA_ENDPOINTS
	// (comma-separated), deduped and trailing-slash-trimmed.
	cfg.Endpoints = []string{cfg.BaseURL}
	for _, e := range splitFields(os.Getenv("LCA_ENDPOINTS")) {
		cfg.Endpoints = appendUnique(cfg.Endpoints, strings.TrimRight(e, "/"))
	}
	return cfg
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
