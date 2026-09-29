package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileConfig is the optional JSON config, read from $LCA_DIR/config.json (user)
// and then <root>/.lca/config.json (project; later wins per key). Env still
// configures the local endpoint; the file is where multi-model orchestration
// lives — extra providers, per-agent models, and permission rules.
//
//	{
//	  "model": "moonshot/kimi-k2.6",
//	  "thinking": "high",
//	  "subagent_depth": 2,
//	  "providers": {
//	    "gpu2": {"base_url": "http://gpu2:8000/v1", "tools": "native", "dialect": "vllm",
//	             "models": ["Qwen3-Coder-480B"]}
//	  },
//	  "agents": {
//	    "explore": {"model": "deepseek/deepseek-v4-flash", "steps": 30},
//	    "reviewer": {"description": "Reviews diffs", "mode": "subagent",
//	                 "prompt": "You review…", "permission": {"edit": "deny"}}
//	  },
//	  "permission": {"run": {"*": "ask", "go test *": "allow"}, "web": "deny"}
//	}
type FileConfig struct {
	Model         string                    `json:"model"`
	Thinking      string                    `json:"thinking"`
	APIKeyEnv     string                    `json:"api_key_env"` // the NAME of the variable holding the key
	SubagentDepth int                       `json:"subagent_depth"`
	Providers     map[string]ProviderConfig `json:"providers"`
	Agents        map[string]AgentConfig    `json:"agents"`
	Permission    PermissionConfig          `json:"permission"`
	Sources       []string                  `json:"-"`

	// The scalars are carried as the TEXT that was in the file, keyed by JSON
	// name, and validated once by applySetting (settings.go) — the same code /set
	// and the env layer go through, so a file cannot smuggle in a value a command
	// would have refused. A typed struct would need every setting spelled twice,
	// and absent would be indistinguishable from zero.
	Raw map[string]string `json:"-"`
	// Which file last set each key, so /config can name the file instead of
	// saying "a config file". Filled as loadFileConfig merges.
	From map[string]string `json:"-"`
}

type ProviderConfig struct {
	Name      string            `json:"name"`
	BaseURL   string            `json:"base_url"`
	APIKey    string            `json:"api_key"`
	APIKeyEnv string            `json:"api_key_env"`
	Dialect   string            `json:"dialect"`
	Tools     string            `json:"tools"`
	Models    []string          `json:"models"`
	Headers   map[string]string `json:"headers"`
	Extra     map[string]any    `json:"extra"`
}

func (pc ProviderConfig) apply(p *Provider) {
	if pc.Name != "" {
		p.Name = pc.Name
	}
	if pc.BaseURL != "" {
		p.BaseURL = strings.TrimRight(pc.BaseURL, "/")
	}
	if pc.APIKey != "" {
		p.Key = os.ExpandEnv(pc.APIKey)
	}
	if pc.APIKeyEnv != "" {
		p.KeyEnv = append([]string{pc.APIKeyEnv}, p.KeyEnv...)
	}
	if pc.Dialect != "" {
		p.Dialect = pc.Dialect
	}
	if pc.Tools != "" {
		p.Transport = pc.Tools
	}
	if len(pc.Models) > 0 {
		p.Models = pc.Models
	}
	if pc.Headers != nil {
		p.Headers = pc.Headers
	}
	if pc.Extra != nil {
		p.Extra = pc.Extra
	}
}

// AgentConfig mirrors the markdown agent frontmatter (agents.go).
type AgentConfig struct {
	Description string           `json:"description"`
	Mode        string           `json:"mode"`
	Model       string           `json:"model"`
	Prompt      string           `json:"prompt"`
	Temperature *float64         `json:"temperature"`
	TopP        *float64         `json:"top_p"`
	Thinking    string           `json:"thinking"`
	Steps       int              `json:"steps"`
	Hidden      bool             `json:"hidden"`
	Disable     bool             `json:"disable"`
	Permission  PermissionConfig `json:"permission"`
}

// PermissionConfig decodes the permission object while preserving key order —
// order is precedence (last match wins), which a Go map would lose. Accepts:
//
//	"allow"                                   → {"*": "allow"}
//	{"edit": "ask", "run": {"*": "ask", "git status *": "allow"}}
type PermissionConfig Ruleset

func (pc *PermissionConfig) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	var s string
	if json.Unmarshal(data, &s) == nil {
		a, ok := validAction(s)
		if !ok {
			return fmt.Errorf("permission: bad action %q", s)
		}
		*pc = PermissionConfig{{"*", "*", a}}
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	keys, vals, err := orderedObject(dec)
	if err != nil {
		return fmt.Errorf("permission: %w", err)
	}
	var rs Ruleset
	for i, key := range keys {
		var action string
		if json.Unmarshal(vals[i], &action) == nil {
			a, ok := validAction(action)
			if !ok {
				return fmt.Errorf("permission %s: bad action %q", key, action)
			}
			rs = append(rs, Rule{key, "*", a})
			continue
		}
		pk, pv, err := orderedObject(json.NewDecoder(bytes.NewReader(vals[i])))
		if err != nil {
			return fmt.Errorf("permission %s: %w", key, err)
		}
		for j, pat := range pk {
			if err := json.Unmarshal(pv[j], &action); err != nil {
				return fmt.Errorf("permission %s.%s: %w", key, pat, err)
			}
			a, ok := validAction(action)
			if !ok {
				return fmt.Errorf("permission %s.%s: bad action %q", key, pat, action)
			}
			rs = append(rs, Rule{key, expandHome(pat), a})
		}
	}
	*pc = PermissionConfig(rs)
	return nil
}

// orderedObject reads one JSON object, returning its keys and raw values in order.
func orderedObject(dec *json.Decoder) ([]string, []json.RawMessage, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("expected an object")
	}
	var keys []string
	var vals []json.RawMessage
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k, _ := kt.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		keys = append(keys, k)
		vals = append(vals, raw)
	}
	_, err = dec.Token() // closing }
	return keys, vals, err
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// loadFileConfig merges the user and project config files. Missing files are
// fine; a malformed one is an error (silently ignoring a typo'd permission rule
// would be worse than refusing to start).
func loadFileConfig(cfg Config) (*FileConfig, error) {
	out := &FileConfig{Providers: map[string]ProviderConfig{}, Agents: map[string]AgentConfig{}}
	paths := []string{filepath.Join(cfg.Dir, "config.json"), filepath.Join(cfg.Root, ".lca", "config.json")}
	if p := os.Getenv("LCA_CONFIG"); p != "" {
		paths = append(paths, p)
	}
	out.Raw, out.From = map[string]string{}, map[string]string{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var fc FileConfig
		if err := json.Unmarshal(data, &fc); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		// Decoded twice on purpose: once into the struct for the blocks, once into
		// raw keys to learn which scalars were PRESENT. Absent and zero must not be
		// confused — "max_tokens": 0 is an operator saying "let the server decide",
		// and a missing key is them saying nothing.
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(data, &keys); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for k, raw := range keys {
			s := findSetting(k)
			if s == nil || s.JSON != k {
				continue // a block (providers, agents, permission) or a key we do not own
			}
			txt, err := scalarText(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", p, k, err)
			}
			out.Raw[k], out.From[k] = txt, p
		}
		out.Sources = append(out.Sources, p)
		if fc.Model != "" {
			out.Model = fc.Model
		}
		if fc.Thinking != "" {
			out.Thinking = fc.Thinking
		}
		if fc.APIKeyEnv != "" {
			out.APIKeyEnv = fc.APIKeyEnv
		}
		if fc.SubagentDepth > 0 {
			out.SubagentDepth = fc.SubagentDepth
		}
		for k, v := range fc.Providers {
			out.Providers[k] = v
		}
		for k, v := range fc.Agents {
			out.Agents[k] = v
		}
		out.Permission = append(out.Permission, fc.Permission...)
	}
	return out, nil
}

// scalarText renders one config.json value as the text applySetting takes: a
// string as itself, a number and a bool in their own spelling, and a list as the
// comma form the env variables already use. Anything else is a file the operator
// meant differently, and saying so beats guessing.
func scalarText(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		if t {
			return "on", nil
		}
		return "off", nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case []any:
		var parts []string
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return "", fmt.Errorf("a list of names, got %v", e)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ","), nil
	case nil:
		return "", nil
	}
	return "", fmt.Errorf("expected a name, a number, a flag or a list")
}
