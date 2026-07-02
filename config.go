package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Config holds runtime configuration. Everything is resolved from flags/env at
// startup; there is deliberately no config file and no auth layer — identity
// comes from the process (uid/gid of whoever ran the binary).
type Config struct {
	Root        string   // realpath jail root; nothing may be touched outside it
	BaseURL     string   // OpenAI-compatible endpoint, e.g. http://localhost:8000/v1
	Model       string   // model name as served by vLLM/SGLang
	APIKey      string   // optional; most local servers accept any token
	Temperature float64  // low by default for deterministic tool use
	MaxSteps    int      // safety cap on tool-call iterations per user turn
	Allowed     []string // command allowlist (matched against basename of argv[0])
	Dir         string   // where the audit log and session transcripts are written
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
		Root:        env("LCA_ROOT", cwd),
		Dir:         env("LCA_DIR", filepath.Join(home, ".lca")),
		BaseURL:     strings.TrimRight(env("LCA_BASE_URL", "http://localhost:8000/v1"), "/"),
		Model:       env("LCA_MODEL", "local"),
		APIKey:      env("LCA_API_KEY", "sk-noauth"),
		Temperature: 0.2,
		MaxSteps:    25,
		Allowed: []string{
			"ls", "cat", "pwd", "head", "tail", "wc",
			"git", "go", "gofmt", "grep", "rg", "find", "echo",
		},
	}
	if v := os.Getenv("LCA_ALLOW"); v != "" {
		cfg.Allowed = splitFields(v)
	}
	return cfg
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
