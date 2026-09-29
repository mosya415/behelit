package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Built-in tool definitions. Implementations live in tools.go (fs + commands),
// edit.go (apply layer), glob.go, task.go and skills.go.

func init() {
	registerTool(&ToolDef{
		Name: "list_dir",
		Desc: "List a directory as an indented tree (skips .git; capped at 400 entries). Use it to orient yourself in an unfamiliar tree.",
		Params: []Param{
			{Name: "path", Type: "string", Desc: "Directory relative to the working directory (default \".\")"},
		},
		TextDoc:  "List a directory tree to orient yourself (runs automatically):\n<list_dir path=\"subdir\"/>",
		Parallel: true,
		Run: func(tc *ToolCtx, a Args) string {
			path := a.Str("path")
			if msg, ok := tc.Ask("read", orDot(path), "LIST "+orDot(path), ""); !ok {
				return msg
			}
			if rem := tc.S.remote(); rem != nil {
				return rem.listDir(tc.Ctx, path)
			}
			return listDir(tc.S.jail(), path)
		},
	})

	registerTool(&ToolDef{
		Name: "glob",
		Desc: "Find files by glob pattern (\"**/*.go\", \"src/**/test_*.py\", \"*.{ts,tsx}\"). A pattern without \"/\" matches file names at any depth. Returns up to 100 paths, most recently modified first.",
		Params: []Param{
			{Name: "pattern", Type: "string", Desc: "Glob pattern", Required: true},
			{Name: "path", Type: "string", Desc: "Directory to search in (default: working directory). Omit for the default"},
		},
		TextDoc:  "Find files by glob pattern, newest first (runs automatically):\n<glob pattern=\"**/*_test.go\" path=\"pkg\"/>",
		Parallel: true,
		Run: func(tc *ToolCtx, a Args) string {
			if msg, ok := tc.Ask("read", orDot(a.Str("path")), "GLOB "+a.Str("pattern"), ""); !ok {
				return msg
			}
			if rem := tc.S.remote(); rem != nil {
				return rem.glob(tc.Ctx, a.Str("pattern"), a.Str("path"))
			}
			return globFiles(tc.S.jail(), a.Str("pattern"), a.Str("path"))
		},
	})

	registerTool(&ToolDef{
		Name: "grep",
		Desc: "Search file contents with a regular expression (Go RE2 syntax). Returns file:line:text matches, capped at 200. Filter files with include (e.g. \"*.go\", \"*.{ts,tsx}\").",
		Params: []Param{
			{Name: "pattern", Type: "string", Desc: "Regular expression", Required: true},
			{Name: "path", Type: "string", Desc: "Directory or file to search (default: working directory)"},
			{Name: "include", Type: "string", Desc: "File-name glob to include, e.g. \"*.py\""},
		},
		TextDoc:  "Search the tree with a regexp, optionally filtered by file glob (runs automatically):\n<grep pattern=\"funcName\" path=\"subdir\" include=\"*.go\"/>",
		Parallel: true,
		Run: func(tc *ToolCtx, a Args) string {
			if msg, ok := tc.Ask("read", orDot(a.Str("path")), "GREP "+a.Str("pattern"), ""); !ok {
				return msg
			}
			// Searching reads every file it touches: files a rule doesn't plainly
			// allow (e.g. "*.env" asks, "secrets/*" denies) are skipped.
			rules := tc.S.rules()
			allow := func(rel string) bool { return Evaluate("read", rel, rules...) == Allow }
			if rem := tc.S.remote(); rem != nil {
				return rem.grep(tc.Ctx, a.Str("pattern"), a.Str("path"), a.Str("include"), allow)
			}
			return grepTree(tc.S.jail(), a.Str("pattern"), a.Str("path"), a.Str("include"), allow)
		},
	})

	registerTool(&ToolDef{
		Name: "read_file",
		Desc: "Read a file's raw content (no line-number prefixes, so text can be copied verbatim into an edit). For large files pass offset/limit (1-based line numbers); oversized reads keep head and tail. Read a file before editing it.",
		Params: []Param{
			{Name: "path", Type: "string", Desc: "File path relative to the working directory", Required: true},
			{Name: "offset", Type: "integer", Desc: "First line to read (1-based)"},
			{Name: "limit", Type: "integer", Desc: "Number of lines to read"},
		},
		TextDoc:  "Read a file, optionally a line range (runs automatically):\n<read_file path=\"rel/path.go\"/>\n<read_file path=\"rel/path.go\" lines=\"40-80\"/>",
		Parallel: true,
		Run: func(tc *ToolCtx, a Args) string {
			path := a.Str("path")
			if msg, ok := tc.Ask("read", path, "READ "+path, ""); !ok {
				return msg
			}
			lines := a.Str("lines")
			if off, lim := a.Int("offset"), a.Int("limit"); lines == "" && (off > 0 || lim > 0) {
				if off < 1 {
					off = 1
				}
				if lim <= 0 {
					lim = 1 << 30
				}
				lines = fmt.Sprintf("%d-%d", off, off+lim-1)
			}
			var res string
			if rem := tc.S.remote(); rem != nil {
				res = rem.readFile(tc.Ctx, path, lines)
			} else {
				res = readFile(tc.S.jail(), path, lines)
			}
			if !strings.HasPrefix(res, "error:") {
				tc.S.noteRead(tc.Ctx, path)
			}
			return res
		},
	})

	registerTool(&ToolDef{
		Name: "edit",
		Desc: "Replace text in an existing file. old_string must match the file (exact match first; whitespace/indentation-tolerant fallbacks apply only when they identify a single location). Include enough surrounding lines to be unique, or set replace_all to change every occurrence. Never include line-number prefixes. Needs approval.",
		Params: []Param{
			{Name: "path", Type: "string", Desc: "File path relative to the working directory", Required: true},
			{Name: "old_string", Type: "string", Desc: "Existing text to replace", Required: true},
			{Name: "new_string", Type: "string", Desc: "Replacement text (must differ from old_string)", Required: true},
			{Name: "replace_all", Type: "boolean", Desc: "Replace every occurrence (default false)"},
		},
		TextDoc: `Edit part of a file (needs approval). The search text should match the file
BYTE-FOR-BYTE, including indentation. Include enough lines to be unique
(add replace_all="true" on the edit tag to change every occurrence):
<edit path="rel/path.go">
<search>
old code, exactly as it appears
</search>
<replace>
new code
</replace>
</edit>`,
		Run: runEditTool,
	})

	registerTool(&ToolDef{
		Name: "write",
		Desc: "Create a file or overwrite a small one with the full content (parent directories are created). Prefer edit for changes to existing files; an existing file must have been read first. Needs approval.",
		Params: []Param{
			{Name: "path", Type: "string", Desc: "File path relative to the working directory", Required: true},
			{Name: "content", Type: "string", Desc: "The entire file content", Required: true},
		},
		Body:    "content",
		TextDoc: "Overwrite or create a small file (needs approval):\n<write path=\"rel/path.go\">\n...entire file content...\n</write>",
		Run:     runWriteTool,
	})

	registerTool(&ToolDef{
		Name: "run_command",
		Desc: "Run a command in the working directory: build, test, git, package managers. No shell unless unsafe mode is on (pipes/redirects are inert), argv[0] must be on the allowlist, stdin is closed, output is captured (head+tail kept). Don't use it to read or search files — use read_file/grep/glob. Needs approval.",
		Params: []Param{
			{Name: "command", Type: "string", Desc: "The command line", Required: true},
			{Name: "timeout", Type: "integer", Desc: "Timeout in seconds (default from LCA_CMD_TIMEOUT, max 1800)"},
		},
		Body:    "command",
		TextDoc: "Run an allowlisted command (needs approval; no shell — no pipes/redirects):\n<run_command>\ngo test ./...\n</run_command>",
		Run:     runCommandTool,
	})

	registerTool(&ToolDef{
		Name: "todowrite",
		Desc: `Maintain a structured todo list for the current task; send the WHOLE updated list each time. Use it for work with 3+ steps or when the user gives several tasks. Statuses: pending, in_progress (exactly one at a time), completed (only after the work is actually done and verified), cancelled. Keep items short; preserve user-provided commands verbatim.`,
		Params: []Param{
			{Name: "todos", Type: "array", Desc: "The updated todo list", Required: true, Schema: map[string]any{
				"type": "array", "description": "The full updated todo list",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":  map[string]any{"type": "string", "description": "Brief description of the task"},
						"status":   map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "cancelled"}},
						"priority": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
					},
					"required": []string{"content", "status"},
				},
			}},
		},
		Body: "todos",
		TextDoc: `Track a multi-step task (runs automatically; send the WHOLE list each time,
statuses pending|in_progress|completed|cancelled, one in_progress at a time):
<todowrite>
[{"content": "add parser test", "status": "in_progress"}, {"content": "fix edge case", "status": "pending"}]
</todowrite>`,
		Run: runTodoTool,
	})

	registerTool(&ToolDef{
		Name: "webfetch",
		Desc: "Fetch a URL (http/https) and return its content as plain text (HTML is stripped) or raw. Max 5MB, 30s default timeout. Needs approval.",
		Params: []Param{
			{Name: "url", Type: "string", Desc: "The URL to fetch", Required: true},
			{Name: "format", Type: "string", Desc: "text (default) or raw", Enum: []string{"text", "raw"}},
		},
		TextDoc:  "Fetch a web page as text (needs approval):\n<webfetch url=\"https://example.com/docs\"/>",
		Parallel: true,
		Run:      runWebfetchTool,
	})
}

func orDot(p string) string {
	if p == "" {
		return "."
	}
	return p
}

func runEditTool(tc *ToolCtx, a Args) string {
	path := a.Str("path")
	oldS, newS := a.Str("old_string"), a.Str("new_string")
	if oldS == newS {
		return "error: no changes to apply: old_string and new_string are identical"
	}
	if rem := tc.S.remote(); rem != nil {
		return runRemoteEdit(tc, rem, a, path, oldS, newS)
	}
	abs, err := tc.S.jail().Resolve(path)
	if err != nil {
		tc.S.event("edit", map[string]any{"path": path, "error": err.Error()})
		return "error: " + err.Error()
	}
	if oldS == "" {
		if _, statErr := os.Stat(abs); statErr == nil {
			return "error: old_string cannot be empty when editing an existing file — provide the exact text to replace, or use write for a full replacement"
		}
		a["content"] = newS
		return runWriteTool(tc, a)
	}
	if msg := tc.S.checkStale(tc.Ctx, path); msg != "" {
		return "error: " + msg
	}
	msg, ok := tc.Ask("edit", path, "EDIT "+path, unifiedPreview(oldS, newS))
	if !ok {
		tc.S.event("edit", map[string]any{"path": path, "approved": false})
		return msg
	}
	before, existed := snapshot(abs)
	res, err := applyEditMode(abs, path, oldS, newS, a.Bool("replace_all"))
	if err != nil {
		tc.S.event("edit", map[string]any{"path": path, "approved": true, "error": err.Error()})
		return "error: " + err.Error()
	}
	if !tc.S.isolated {
		recordChange(path, abs, "edit", before, existed)
	}
	tc.S.noteRead(tc.Ctx, path)
	tc.S.event("edit", map[string]any{"path": path, "approved": true, "result": res})
	return res
}

func runWriteTool(tc *ToolCtx, a Args) string {
	path, content := a.Str("path"), a.Str("content")
	if rem := tc.S.remote(); rem != nil {
		return runRemoteWrite(tc, rem, path, content)
	}
	abs, err := tc.S.jail().Resolve(path)
	if err != nil {
		tc.S.event("write", map[string]any{"path": path, "error": err.Error()})
		return "error: " + err.Error()
	}
	action := "overwrite"
	if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
		action = "create"
	} else if msg := tc.S.checkStale(tc.Ctx, path); msg != "" {
		return "error: " + msg
	}
	preview := fmt.Sprintf("  %s %s (%d bytes)", action, path, len(content))
	msg, ok := tc.Ask("edit", path, strings.ToUpper(action)+" "+path, preview)
	if !ok {
		tc.S.event("write", map[string]any{"path": path, "approved": false})
		return msg
	}
	before, existed := snapshot(abs)
	res, err := writeWholeFile(abs, path, content)
	if err != nil {
		tc.S.event("write", map[string]any{"path": path, "approved": true, "error": err.Error()})
		return "error: " + err.Error()
	}
	if !tc.S.isolated {
		recordChange(path, abs, "write", before, existed)
	}
	tc.S.noteRead(tc.Ctx, path)
	tc.S.event("write", map[string]any{"path": path, "approved": true, "bytes": len(content)})
	return res
}

func runCommandTool(tc *ToolCtx, a Args) string {
	cmd := strings.TrimSpace(a.Str("command"))
	if cmd == "" {
		return "error: empty command"
	}
	msg, ok := tc.Ask("run", cmd, "RUN", "  $ "+cmd)
	if !ok {
		tc.S.event("run_command", map[string]any{"cmd": cmd, "approved": false})
		return msg
	}
	timeout := cmdTimeout
	if t := a.Int("timeout"); t > 0 {
		timeout = time.Duration(min(t, 1800)) * time.Second
	}
	var res string
	if rem := tc.S.remote(); rem != nil {
		out, exit := rem.run(tc.Ctx, cmd, timeout, nil, tc.S.view.Live())
		res = remoteCmdResult(out, exit, timeout)
	} else {
		res = runCommand(tc.Ctx, tc.S.jail(), cmd, timeout, tc.S.view.Live())
	}
	tc.S.event("run_command", map[string]any{"cmd": cmd, "approved": true, "result": summarize(res)})
	return res
}

// Todo is one entry of a session's todo list (todowrite).
type Todo struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
}

func runTodoTool(tc *ToolCtx, a Args) string {
	if msg, ok := tc.Ask("todo", "*", "TODO", ""); !ok {
		return msg
	}
	raw, _ := json.Marshal(a["todos"])
	var todos []Todo
	if err := json.Unmarshal(raw, &todos); err != nil {
		return "error: todos must be an array of {content, status, priority}: " + err.Error()
	}
	for i := range todos {
		switch todos[i].Status {
		case "pending", "in_progress", "completed", "cancelled":
		default:
			todos[i].Status = "pending"
		}
	}
	tc.S.setTodos(todos)
	tc.S.event("todowrite", map[string]any{"count": len(todos)})
	out, _ := json.MarshalIndent(todos, "", "  ")
	return string(out)
}

const maxFetchBytes = 5 << 20

func runWebfetchTool(tc *ToolCtx, a Args) string {
	url := strings.TrimSpace(a.Str("url"))
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "error: url must start with http:// or https://"
	}
	if msg, ok := tc.Ask("web", url, "FETCH "+url, ""); !ok {
		return msg
	}
	req, err := http.NewRequestWithContext(tc.Ctx, http.MethodGet, url, nil)
	if err != nil {
		return "error: " + err.Error()
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; lca-agent)")
	req.Header.Set("Accept", "text/markdown;q=1.0, text/plain;q=0.9, text/html;q=0.8, */*;q=0.1")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return "error: " + err.Error()
	}
	if len(body) > maxFetchBytes {
		return "error: response too large (exceeds 5MB)"
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Sprintf("error: HTTP %d from %s", resp.StatusCode, url)
	}
	text := string(body)
	ct := resp.Header.Get("Content-Type")
	if a.Str("format") != "raw" && strings.Contains(ct, "html") {
		text = htmlToText(text)
	}
	tc.S.event("webfetch", map[string]any{"url": url, "bytes": len(body)})
	return fmt.Sprintf("%s (%s):\n%s", url, ct, headTail(text, maxToolOutput))
}

// ── the same edit/write tools, on a remote project ──────────────────────────

func runRemoteEdit(tc *ToolCtx, rem *Remote, a Args, path, oldS, newS string) string {
	rel, err := rem.relPath(path)
	if err != nil {
		return "error: " + err.Error()
	}
	content, errs := rem.read(tc.Ctx, rel)
	if errs != "" {
		if oldS == "" { // a new file
			a["content"] = newS
			return runRemoteWrite(tc, rem, path, newS)
		}
		return errs
	}
	if oldS == "" {
		return "error: old_string cannot be empty when editing an existing file — provide the exact text to replace, or use write for a full replacement"
	}
	if msg := tc.S.checkStale(tc.Ctx, path); msg != "" {
		return "error: " + msg
	}
	updated, strategy, err := fuzzyReplace(content, oldS, newS, a.Bool("replace_all"))
	if err != nil {
		tc.S.event("edit", map[string]any{"path": path, "remote": rem.Label(), "error": err.Error()})
		return "error: " + path + ": " + err.Error()
	}
	msg, ok := tc.Ask("edit", path, "EDIT "+path, unifiedPreview(oldS, newS))
	if !ok {
		tc.S.event("edit", map[string]any{"path": path, "remote": rem.Label(), "approved": false})
		return msg
	}
	if errs := rem.write(tc.Ctx, rel, updated); errs != "" {
		return errs
	}
	tc.S.noteRead(tc.Ctx, path)
	tc.S.event("edit", map[string]any{"path": path, "remote": rem.Label(), "approved": true, "strategy": strategy})
	res := fmt.Sprintf("edited %s on %s (1 replacement)", path, rem.Where())
	if strategy != "exact" {
		res += " — matched via " + strategy + " fallback; re-read before further edits nearby"
	}
	return res
}

func runRemoteWrite(tc *ToolCtx, rem *Remote, path, content string) string {
	rel, err := rem.relPath(path)
	if err != nil {
		return "error: " + err.Error()
	}
	action := "create"
	if _, exists := rem.stat(tc.Ctx, rel); exists {
		action = "overwrite"
		if msg := tc.S.checkStale(tc.Ctx, path); msg != "" {
			return "error: " + msg
		}
	}
	preview := fmt.Sprintf("   %s %s on %s (%d bytes)", action, path, rem.Where(), len(content))
	msg, ok := tc.Ask("edit", path, strings.ToUpper(action)+" "+path, preview)
	if !ok {
		tc.S.event("write", map[string]any{"path": path, "remote": rem.Label(), "approved": false})
		return msg
	}
	if errs := rem.write(tc.Ctx, rel, content); errs != "" {
		return errs
	}
	tc.S.noteRead(tc.Ctx, path)
	tc.S.event("write", map[string]any{"path": path, "remote": rem.Label(), "approved": true, "bytes": len(content)})
	return fmt.Sprintf("wrote %s on %s (%d bytes)", path, rem.Where(), len(content))
}

// remoteCmdResult shapes a remote command's outcome like the local one.
func remoteCmdResult(out string, exit int, timeout time.Duration) string {
	res := headTail(out, maxCmdOutput)
	switch {
	case exit == 0:
		if strings.TrimSpace(res) == "" {
			return "(no output, exit 0)"
		}
		return res
	case exit < 0:
		return res
	default:
		return res + fmt.Sprintf("\n(exit status %d)", exit)
	}
}
