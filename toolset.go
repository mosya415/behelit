package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The tool registry. One definition per tool serves both transports:
//
//   - native: Params become a JSON schema in the request's "tools"; calls come
//     back as tool_calls with JSON arguments.
//   - text:   the tool is a line-anchored tag (protocol.go). Params are tag
//     attributes, except the one named in Body, which is the tag's inner text
//     (a tool with no Body is a void tag: <name …/>).
//
// Tools ask for permission themselves (ToolCtx.Ask), mirroring opencode's
// ctx.ask inside execute, so each can decide what the pattern is and what the
// approval prompt should preview.

type Param struct {
	Name     string
	Type     string // string | integer | boolean | array | object
	Desc     string
	Required bool
	Enum     []string
	Schema   map[string]any // full schema override (arrays of objects)
}

type ToolDef struct {
	Name   string
	Desc   string // native description
	Params []Param
	Body   string // text protocol: which param is the tag body ("" → void tag)
	// RawParams is a JSON Schema handed to us whole, by an owner who is not this
	// program: an MCP server's inputSchema. When it is set, Params is ignored and
	// schema() passes these bytes through (see mcp.go).
	RawParams map[string]any
	TextDoc   string // text protocol usage example(s)
	Parallel  bool   // safe to run concurrently with other Parallel calls
	Run       func(tc *ToolCtx, a Args) string
}

// ToolCtx is what a running tool can reach: its session (jail, approver,
// recorder, orchestrator) and a context canceled on interrupt.
type ToolCtx struct {
	Ctx    context.Context
	S      *Session
	CallID string
	Name   string
	// TaskSession is filled in by runDelegateTool with the subagent's session
	// UID — the Session of the TaskRecord it writes — so a caller that records
	// the call (a workflow step) can be joined to that record.
	TaskSession string
	// Reviewer overrides the role's configured reviewer for this one call. It is
	// a caller-side channel (a workflow step), deliberately not a tool argument:
	// choosing who reviews is registration, not a model's decision.
	Reviewer string
	// Member overrides the role's member for this one call (a workflow step's
	// member:), for the same reason Reviewer is a caller-side channel and not a
	// tool argument: choosing a machine is registration, not a model's decision.
	Member string
	// ForkFrom is the session a `fork: true` delegation inherits context from
	// when it is not the caller itself — a workflow's lead reads nothing, and the
	// reads worth handing down were made by a prompt step's own child session.
	ForkFrom *Session
}

// Args are decoded tool arguments. Text-protocol attributes arrive as strings,
// JSON arguments as their JSON types; the accessors accept both.
type Args map[string]any

func (a Args) Str(k string) string {
	switch v := a[k].(type) {
	case string:
		return v
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func (a Args) Int(k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}
	return 0
}

func (a Args) Bool(k string) bool {
	switch v := a[k].(type) {
	case bool:
		return v
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(v))
		return b
	}
	return false
}

// argBool reads an optional boolean argument: a model that omitted it gets the
// caller's default, and an explicit false is honoured (Args.Bool cannot tell
// "false" from "absent").
func argBool(a Args, k string, def bool) bool {
	if _, ok := a[k]; !ok {
		return def
	}
	return a.Bool(k)
}

var toolRegistry = map[string]*ToolDef{}

func registerTool(t *ToolDef) { toolRegistry[t.Name] = t }

// toolOrder is the stable order tools are described in (prompt + schema), so
// the request prefix stays byte-identical across calls.
var toolOrder = []string{"list_dir", "glob", "grep", "read_file", "edit", "write", "run_command",
	"todowrite", "task", "delegate", "skill", "webfetch"}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// schemaFor renders a tool's JSON schema.
func (t *ToolDef) schema() ToolSchema {
	if t.RawParams != nil {
		// An MCP server owns its own JSON Schema, so we hand it to the model
		// byte-for-byte instead of rebuilding it out of Param: a schema we rebuilt
		// is a schema we could get wrong, and the contract the model has to satisfy
		// is the SERVER's. encoding/json sorts map keys, so the prefix bytes are
		// canonical whatever order the server wrote them in.
		return ToolSchema{Type: "function", Function: ToolSchemaFunc{
			Name: t.Name, Description: t.Desc, Parameters: t.RawParams}}
	}
	props := map[string]any{}
	var req []string
	for _, p := range t.Params {
		var s map[string]any
		if p.Schema != nil {
			s = p.Schema
		} else {
			s = map[string]any{"type": p.Type, "description": p.Desc}
			if len(p.Enum) > 0 {
				s["enum"] = p.Enum
			}
		}
		props[p.Name] = s
		if p.Required {
			req = append(req, p.Name)
		}
	}
	params := map[string]any{"type": "object", "properties": props}
	if len(req) > 0 {
		params["required"] = req
	}
	return ToolSchema{Type: "function", Function: ToolSchemaFunc{Name: t.Name, Description: t.Desc, Parameters: params}}
}

// decodeArgs parses a native call's JSON arguments, tolerating a few common
// model mistakes: empty string, and arguments double-encoded as a JSON string.
func decodeArgs(raw string) (Args, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Args{}, nil
	}
	var a Args
	if err := json.Unmarshal([]byte(raw), &a); err == nil {
		return a, nil
	}
	var inner string
	if json.Unmarshal([]byte(raw), &inner) == nil {
		if err := json.Unmarshal([]byte(inner), &a); err == nil {
			return a, nil
		}
	}
	return nil, fmt.Errorf("arguments are not valid JSON: %s", truncate(raw, 200))
}

// validate checks required params and simple types, producing the message the
// model sees (opencode: "The X tool was called with invalid arguments…").
func (t *ToolDef) validate(a Args) error {
	var problems []string
	for _, p := range t.Params {
		v, ok := a[p.Name]
		if !ok || v == nil {
			if p.Required {
				problems = append(problems, "missing required "+p.Name)
			}
			continue
		}
		if p.Type == "array" {
			if _, ok := v.([]any); !ok {
				if s, isStr := v.(string); isStr { // text protocol / stringified JSON
					var arr []any
					if json.Unmarshal([]byte(s), &arr) == nil {
						a[p.Name] = arr
						continue
					}
				}
				problems = append(problems, p.Name+" must be an array")
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the %s tool was called with invalid arguments: %s. Rewrite the call so it satisfies the schema", t.Name, strings.Join(problems, "; "))
	}
	return nil
}

// resolveToolName maps a model-emitted name to a registered tool: exact, then
// case-insensitive, then a few aliases open models commonly use.
func resolveToolName(name string) *ToolDef {
	if t := toolRegistry[name]; t != nil {
		return t
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	if t := toolRegistry[lower]; t != nil {
		return t
	}
	aliases := map[string]string{
		"read": "read_file", "cat": "read_file", "view": "read_file", "open_file": "read_file",
		"ls": "list_dir", "list": "list_dir", "bash": "run_command", "shell": "run_command", "exec": "run_command",
		"write_file": "write", "create_file": "write", "edit_file": "edit", "str_replace": "edit",
		"search": "grep", "find_files": "glob", "todo_write": "todowrite", "fetch": "webfetch", "agent": "task",
	}
	if n, ok := aliases[lower]; ok {
		return toolRegistry[n]
	}
	return nil
}

// toolsFor returns the tool definitions an agent may use, in stable order: a
// tool whose permission is denied outright is not offered at all, and task is
// withheld once the subagent depth limit is reached.
func toolsFor(s *Session) []*ToolDef {
	var out []*ToolDef
	for _, name := range toolOrder {
		t := toolRegistry[name]
		if t == nil {
			continue
		}
		if s.agent.ToolsSet && !contains(s.agent.Tools, name) {
			continue // a role's tool set is exact
		}
		if Disabled(permissionOf(name), s.rules()...) {
			continue
		}
		switch name {
		case "task":
			if s.depth >= s.orch.subagentDepth() || len(s.orch.subagentsFor(s)) == 0 {
				continue
			}
		case "skill":
			if len(s.orch.skills) == 0 {
				continue
			}
		case "delegate":
			// A delegation needs a git worktree. On a members: fleet it gets one ON
			// that member; on the old single-remote spelling the worktree would be
			// local while the project is not, so the subagents share the tree
			// instead (task), or lca runs on that machine. canDelegate is
			// registration, never a network probe: the tool schema is part of the
			// cached prefix and must not depend on whether a host answered.
			if !s.orch.canDelegate(s.memberOf()) || s.depth >= s.orch.subagentDepth() || len(s.orch.roleList(s.agent.Name)) == 0 {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// textToolDocs renders the text-protocol reference for the given tools.
func textToolDocs(tools []*ToolDef) string {
	var b strings.Builder
	for _, t := range tools {
		b.WriteString(t.TextDoc)
		b.WriteString("\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
