package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBlocks_ReadAndGrep(t *testing.T) {
	text := `I'll look first.
<read_file path="main.go" lines="1-20"/>
<grep pattern="func main" path="."/>`
	blocks := ParseBlocks(text)
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Name != "read_file" || blocks[0].Attr["path"] != "main.go" || blocks[0].Attr["lines"] != "1-20" {
		t.Fatalf("bad read_file block: %+v", blocks[0])
	}
	if blocks[1].Name != "grep" || blocks[1].Attr["pattern"] != "func main" {
		t.Fatalf("bad grep block: %+v", blocks[1])
	}
}

func TestParseBlocks_EditWithAngleBrackets(t *testing.T) {
	// The body contains '<' and '>' — line anchoring must not choke on it.
	text := `<edit path="x.go">
<search>
if a<b && c>d {
</search>
<replace>
if a < b && c > d {
</replace>
</edit>`
	blocks := ParseBlocks(text)
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].Search != "if a<b && c>d {" {
		t.Fatalf("bad search: %q", blocks[0].Search)
	}
	if blocks[0].Replace != "if a < b && c > d {" {
		t.Fatalf("bad replace: %q", blocks[0].Replace)
	}
}

func TestParseBlocks_NoTagsIsFinalAnswer(t *testing.T) {
	if b := ParseBlocks("All done. The bug was a typo."); len(b) != 0 {
		t.Fatalf("expected 0 blocks, got %d", len(b))
	}
}

func TestApplyEdit_Strict(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("alpha\nbeta\ngamma\n"), 0o644)

	if _, err := applyEdit(f, "a.txt", "beta", "BETA"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("bad result: %q", got)
	}

	if _, err := applyEdit(f, "a.txt", "nope", "x"); err == nil {
		t.Fatal("expected not-found error")
	}

	os.WriteFile(f, []byte("x\nx\n"), 0o644)
	if _, err := applyEdit(f, "a.txt", "x", "y"); err == nil {
		t.Fatal("expected ambiguous-match error")
	}
}

func TestJail_Escape(t *testing.T) {
	root := t.TempDir()
	j, err := NewJail(root, []string{"ls"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Resolve("sub/ok.txt"); err != nil {
		t.Fatalf("in-jail path rejected: %v", err)
	}
	if _, err := j.Resolve("../escape.txt"); err == nil {
		t.Fatal("expected escape to be rejected")
	}
	if _, err := j.Resolve("/etc/passwd"); err == nil {
		t.Fatal("expected absolute escape to be rejected")
	}
}

func TestJail_SymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unsupported")
	}
	j, _ := NewJail(root, nil)
	if _, err := j.Resolve("link/secret"); err == nil {
		t.Fatal("symlink escape should be rejected")
	}
}

func TestTrimForContext(t *testing.T) {
	big := strings.Repeat("x", 4000) // ~1k tokens each
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", Content: "reading"},
		{Role: "user", Content: "<tool_result name=\"read_file\">\n" + big + "\n</tool_result>"},
		{Role: "assistant", Content: "reading more"},
		{Role: "user", Content: "<tool_result name=\"read_file\">\n" + big + "\n</tool_result>"},
		{Role: "assistant", Content: "now editing"},
		{Role: "user", Content: "<tool_result name=\"edit\">ok</tool_result>"},
		{Role: "assistant", Content: "done step"},
		{Role: "user", Content: "keep going"},
	}

	out, trimmed := trimForContext(msgs, 500) // force trimming
	if trimmed == 0 {
		t.Fatal("expected some messages to be collapsed")
	}
	// system prompt and the user's real instruction must survive verbatim
	if out[0].Content != "sys" || out[1].Content != "do the thing" {
		t.Fatal("system/user instruction must not be trimmed")
	}
	// the oldest large tool_result should be the stub
	if out[3].Content != trimStub {
		t.Fatalf("oldest tool_result not collapsed: %q", out[3].Content)
	}
	// the original slice must be untouched (disk transcript stays complete)
	if msgs[3].Content == trimStub {
		t.Fatal("trimForContext must not mutate the input slice")
	}

	// under budget → returned unchanged
	small := []Message{{Role: "user", Content: "hi"}}
	if _, n := trimForContext(small, 24000); n != 0 {
		t.Fatalf("small transcript should not be trimmed, got %d", n)
	}
}

func TestTokenize(t *testing.T) {
	argv, err := tokenize(`git commit -m "a b c"`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"git", "commit", "-m", "a b c"}
	if len(argv) != len(want) {
		t.Fatalf("got %v", argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, argv[i], want[i])
		}
	}
}
