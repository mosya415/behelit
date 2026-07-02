package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"time"
)

// Endpoint discovery via an external command (LCA_DISCOVER_CMD), e.g. the
// SLURM-aware `modelstat` tool. We deliberately do NOT reimplement the cluster
// discovery (squeue → scontrol → port-in-startup-script → node→IP → HTTP probe)
// — that is a moving target (nodes requeue/preempt, script names lie) and it
// already lives in modelstat. lca just runs it and consumes its JSON, staying
// decoupled from both the model router and the scheduler.
//
// modelstat --json emits: {"models":[{endpoint,health,model,served_names,
// engine,max_model_len,node,gpu_count,...}], "warnings":[...]}. `endpoint` is a
// bare "host:port" (no scheme, no path); the OpenAI base URL is http://…/v1.

type msModel struct {
	Endpoint    string   `json:"endpoint"` // "host:port"
	Health      string   `json:"health"`   // up | unhealthy | down | unknown
	Model       string   `json:"model"`    // served id / path
	ServedNames []string `json:"served_names"`
	Engine      string   `json:"engine"`
	MaxModelLen int      `json:"max_model_len"`
	Node        string   `json:"node"`
	GpuCount    int      `json:"gpu_count"`
	JobID       string   `json:"job_id"`
}

type msResult struct {
	Models   []msModel `json:"models"`
	Warnings []string  `json:"warnings"`
}

// runDiscovery executes the configured discovery command and parses its JSON.
// The command is operator-configured (env), not model-driven, so it runs via the
// shell (to allow pipelines/module invocation) and bypasses the tool jail.
func runDiscovery(cmdline string) (msResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "sh", "-c", cmdline).Output()
	if err != nil {
		return msResult{}, fmt.Errorf("discovery command failed: %w", err)
	}
	// Tolerate leading noise: parse from the first '{'.
	s := string(out)
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	var res msResult
	if err := json.Unmarshal([]byte(s), &res); err != nil {
		return msResult{}, fmt.Errorf("discovery output was not the expected JSON: %w", err)
	}
	return res, nil
}

// baseURL turns a modelstat endpoint ("host:port") into an OpenAI base URL.
func (m msModel) baseURL() string {
	e := strings.TrimRight(m.Endpoint, "/")
	if !strings.Contains(e, "://") {
		e = "http://" + e
	}
	if !strings.HasSuffix(e, "/v1") {
		e += "/v1"
	}
	return e
}

// modelName is what to send in the chat `model` field: the served name if the
// server was launched with one, else the served id/path as-is.
func (m msModel) modelName() string {
	if len(m.ServedNames) > 0 && m.ServedNames[0] != "" {
		return m.ServedNames[0]
	}
	return m.Model
}

// display is a short human label for the model (basename of a path).
func (m msModel) display() string {
	if len(m.ServedNames) > 0 && m.ServedNames[0] != "" {
		return m.ServedNames[0]
	}
	if m.Model == "" {
		return "(unknown)"
	}
	return path.Base(m.Model)
}

// healthGlyph maps modelstat's health string to our shared status glyphs.
func healthGlyph(h string) string {
	switch h {
	case "up":
		return cGreen + gUp + cReset
	case "unhealthy":
		return cYellow + gPartial + cReset
	case "down":
		return cRed + gDown + cReset
	default:
		return cFaint + gNone + cReset
	}
}
