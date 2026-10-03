package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LCA_SKILLS is the shared knowledge base: one repository of skills that several
// projects read, so a team's hard-won "how we deploy to the stand" stops being
// retyped into every project's own .lca.
func TestLCASkillsIsReadLikeAPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	sep := string(os.PathListSeparator)

	t.Setenv("LCA_SKILLS", "")
	if got := envSkillDirs(); got != nil {
		t.Fatalf("unset means no extra directories, got %v", got)
	}

	t.Setenv("LCA_SKILLS", "/team/skills"+sep+"~/mine"+sep+sep+"  ")
	got := envSkillDirs()
	want := []string{"/team/skills", filepath.Join(home, "mine")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v — a `~` is expanded because this is typed into a shell profile, and an empty element is skipped rather than read as the current directory", got, want)
	}

	// A relative path becomes absolute, so the index does not move with the cwd.
	t.Setenv("LCA_SKILLS", "rel/skills")
	if g := envSkillDirs(); len(g) != 1 || !filepath.IsAbs(g[0]) {
		t.Fatalf("a relative path must be resolved once, got %v", g)
	}
}

// The order is the whole point: a shared skill is a DEFAULT, not a law, so a
// project that ships its own version of a name wins — while the shared
// repository, named deliberately by the operator, outranks whatever happens to
// be in ~/.claude.
func TestAProjectSkillBeatsASharedOneAndASharedOneBeatsHome(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	shared := filepath.Join(base, "shared")
	root := filepath.Join(base, "proj")
	dir := filepath.Join(root, ".lca")

	write := func(at, name, desc string) {
		t.Helper()
		d := filepath.Join(at, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: " + desc + "\n---\nbody\n"
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude", "skills"), "deploy", "from home")
	write(shared, "deploy", "from the shared repo")
	write(shared, "runbook", "only in the shared repo")
	write(filepath.Join(dir, "skills"), "deploy", "from the project")

	t.Setenv("HOME", home)
	t.Setenv("LCA_SKILLS", shared)
	got := loadSkills(root, dir)

	if sk := got["deploy"]; sk == nil || sk.Description != "from the project" {
		t.Fatalf("the project's own version must win: %+v", sk)
	}
	if sk := got["runbook"]; sk == nil || sk.Description != "only in the shared repo" {
		t.Fatalf("a shared skill with no local rival must be available: %+v", sk)
	}

	// Without the project's copy, the shared one takes over from home's.
	if err := os.RemoveAll(filepath.Join(dir, "skills")); err != nil {
		t.Fatal(err)
	}
	if sk := loadSkills(root, dir)["deploy"]; sk == nil || sk.Description != "from the shared repo" {
		t.Fatalf("the shared repo outranks ~/.claude: %+v", sk)
	}
}
