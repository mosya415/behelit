package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Skills and commands — the reusable building blocks of workflows (ported from
// opencode's skill/ and command/). Both are plain markdown on disk, so a team
// can version them with the repo.
//
//   - A skill is a SKILL.md (frontmatter name + description) plus any files
//     beside it. Agents see a one-line index in the system prompt and load the
//     full instructions on demand with the skill tool.
//   - A command is a prompt template invoked as /name args. Frontmatter may
//     pin the agent and model, and subtask: true runs it in a child session.

type Skill struct {
	Name        string
	Description string
	Dir         string
	Path        string
}

func skillDirs(root, dir string) []string {
	home, _ := os.UserHomeDir()
	var out []string
	if home != "" {
		out = append(out, filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".config", "opencode", "skills"))
	}
	out = append(out, filepath.Join(dir, "skills"),
		filepath.Join(root, ".claude", "skills"),
		filepath.Join(root, ".opencode", "skill"), filepath.Join(root, ".opencode", "skills"),
		filepath.Join(root, ".lca", "skills"))
	return out
}

// loadSkills discovers SKILL.md files; later directories override earlier
// ones by name (project beats user).
func loadSkills(root, dir string) map[string]*Skill {
	skills := map[string]*Skill{}
	for _, d := range skillDirs(root, dir) {
		filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() && heavyDir[e.Name()] {
				return filepath.SkipDir
			}
			if e.IsDir() || e.Name() != "SKILL.md" {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			meta, _, _ := splitFrontmatter(string(data))
			name := meta.str("name")
			if name == "" {
				name = filepath.Base(filepath.Dir(p))
			}
			skills[name] = &Skill{Name: name, Description: meta.str("description"), Dir: filepath.Dir(p), Path: p}
			return nil
		})
	}
	return skills
}

func init() {
	registerTool(&ToolDef{
		Name: "skill",
		Desc: "Load a skill: specialized instructions (and bundled files) for a kind of task. Load one when the task matches a skill's description in the available skills list.",
		Params: []Param{
			{Name: "name", Type: "string", Desc: "Skill name from the available skills list", Required: true},
		},
		TextDoc:  "Load a skill's full instructions when a task matches its description (runs automatically):\n<skill name=\"skill-name\"/>",
		Parallel: true,
		Run: func(tc *ToolCtx, a Args) string {
			name := a.Str("name")
			sk := tc.S.orch.skills[name]
			if sk == nil {
				return fmt.Sprintf("error: unknown skill %q (available: %s)", name, strings.Join(sortedKeys(tc.S.orch.skills), ", "))
			}
			if msg, ok := tc.Ask("skill", name, "SKILL "+name, ""); !ok {
				return msg
			}
			data, err := os.ReadFile(sk.Path)
			if err != nil {
				return "error: " + err.Error()
			}
			_, body, _ := splitFrontmatter(string(data))
			var files []string
			filepath.WalkDir(sk.Dir, func(p string, e fs.DirEntry, err error) error {
				if err != nil || len(files) >= 10 {
					return filepath.SkipAll
				}
				if !e.IsDir() && p != sk.Path {
					files = append(files, p)
				}
				return nil
			})
			var b strings.Builder
			fmt.Fprintf(&b, "<skill_content name=%q>\n# Skill: %s\n\n%s\n\nBase directory for this skill: %s\n", name, name, body, sk.Dir)
			b.WriteString("Relative paths in this skill are relative to that directory.\n")
			if len(files) > 0 {
				b.WriteString("<skill_files>\n")
				for _, f := range files {
					b.WriteString(f + "\n")
				}
				b.WriteString("</skill_files>\n")
			}
			b.WriteString("</skill_content>")
			tc.S.event("skill", map[string]any{"name": name})
			return b.String()
		},
	})
}

// skillsIndex is the system-prompt block listing skills an agent may load.
func skillsIndex(s *Session) string {
	names := sortedKeys(s.orch.skills)
	var b strings.Builder
	for _, n := range names {
		if Evaluate("skill", n, s.rules()...) == Deny {
			continue
		}
		sk := s.orch.skills[n]
		fmt.Fprintf(&b, "- %s: %s\n", n, strings.TrimSpace(sk.Description))
	}
	if b.Len() == 0 {
		return ""
	}
	return "# Skills\nSkills are specialized instructions for particular kinds of tasks. When a task matches a skill's description, load it with the skill tool before starting.\n" + b.String()
}

type Command struct {
	Name        string
	Description string
	Agent       string
	Model       string
	Subtask     bool
	Template    string
	Source      string
}

// loadCommands discovers command templates; later directories override.
func loadCommands(root, dir string, ps *Providers) (map[string]*Command, []string) {
	home, _ := os.UserHomeDir()
	var dirs []string
	dirs = append(dirs, filepath.Join(dir, "commands"))
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".config", "opencode", "command"), filepath.Join(home, ".claude", "commands"))
	}
	dirs = append(dirs, filepath.Join(root, ".opencode", "command"), filepath.Join(root, ".opencode", "commands"),
		filepath.Join(root, ".claude", "commands"), filepath.Join(root, ".lca", "commands"))
	cmds := map[string]*Command{}
	var warns []string
	for _, d := range dirs {
		filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(d, p)
			name := strings.TrimSuffix(filepath.ToSlash(rel), ".md")
			meta, body, _ := splitFrontmatter(string(data))
			c := &Command{Name: name, Description: meta.str("description"), Agent: meta.str("agent"),
				Subtask: meta.str("subtask") == "true", Template: body, Source: p}
			c.Model = checkModelRef(meta.str("model"), "/"+name, ps, &warns)
			cmds[name] = c
			return nil
		})
	}
	return cmds, warns
}

var (
	rePlaceholder = regexp.MustCompile(`\$(\d+)`)
	reArgToken    = regexp.MustCompile(`"[^"]*"|'[^']*'|[^\s"']+`)
)

// expandTemplate fills $ARGUMENTS and $1..$N (the highest placeholder takes
// the rest of the arguments). With no placeholders, arguments are appended.
func expandTemplate(tpl, args string) string {
	args = strings.TrimSpace(args)
	var toks []string
	for _, t := range reArgToken.FindAllString(args, -1) {
		toks = append(toks, unquote(t))
	}
	maxN := 0
	for _, m := range rePlaceholder.FindAllStringSubmatch(tpl, -1) {
		if n, _ := strconv.Atoi(m[1]); n > maxN {
			maxN = n
		}
	}
	out := rePlaceholder.ReplaceAllStringFunc(tpl, func(m string) string {
		n, _ := strconv.Atoi(m[1:])
		if n < 1 || n > len(toks) {
			return ""
		}
		if n == maxN {
			return strings.Join(toks[n-1:], " ")
		}
		return toks[n-1]
	})
	hasArgs := strings.Contains(tpl, "$ARGUMENTS")
	out = strings.ReplaceAll(out, "$ARGUMENTS", args)
	if maxN == 0 && !hasArgs && args != "" {
		out += "\n\n" + args
	}
	return strings.TrimSpace(out)
}

func sortedCommands(cmds map[string]*Command) []*Command {
	out := make([]*Command, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
