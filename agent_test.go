package main

import (
	"os"
	"path/filepath"
	"regexp"
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

func TestParseBlocks_GluedTags(t *testing.T) {
	// MiniMax-M3 glues a reasoning-close tag before the open tag and the command
	// before the close tag, all off their own lines.
	text := "reasoning here</mm:think><run_command>\ntail -n 200 logs/x.log</run_command>"
	blocks := ParseBlocks(text)
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Name != "run_command" {
		t.Fatalf("name = %q, want run_command", blocks[0].Name)
	}
	if got := strings.TrimSpace(blocks[0].Body); got != "tail -n 200 logs/x.log" {
		t.Fatalf("body = %q", got)
	}
}

func TestNormalizeTags_KeepsWriteBodyIntact(t *testing.T) {
	// A well-formed write must not gain/lose blank lines around its body.
	text := "<write path=\"x.go\">\npackage main\n\nfunc main() {}\n</write>"
	blocks := ParseBlocks(text)
	if len(blocks) != 1 || blocks[0].Name != "write" {
		t.Fatalf("bad parse: %+v", blocks)
	}
	if blocks[0].Body != "package main\n\nfunc main() {}" {
		t.Fatalf("write body altered: %q", blocks[0].Body)
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
	j, err := NewJail(root, []string{"ls"}, false)
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

func TestJail_Unsafe(t *testing.T) {
	root := t.TempDir()
	j, err := NewJail(root, []string{"ls"}, true) // unsafe
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Resolve("../../etc/passwd"); err != nil {
		t.Fatalf("unsafe jail must allow escape, got %v", err)
	}
	if p, _ := j.Resolve("/etc/hostname"); p != "/etc/hostname" {
		t.Fatalf("unsafe absolute path = %q", p)
	}
	if !j.AllowCommand("rm") {
		t.Fatal("unsafe jail must allow any command")
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
	j, _ := NewJail(root, nil, false)
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

func TestDedupeReads(t *testing.T) {
	rd := func(path, body string) Message {
		return Message{Role: "user", Content: "<tool_result name=\"read_file\" path=\"" + path + "\">\n" + body + "\n</tool_result>"}
	}
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "fix it"},
		rd("a.go", strings.Repeat("x", 4000)), // superseded by the later read of a.go
		{Role: "assistant", Content: "editing"},
		rd("b.go", "small"),
		rd("a.go", strings.Repeat("y", 4000)), // authoritative read of a.go
		{Role: "user", Content: "keep going"},
	}
	// Over budget so compression engages; the two 4k reads (~1k tok each) exceed
	// 1500, and deduping the superseded a.go read alone brings it back under.
	out, trimmed := trimForContext(msgs, 1500)
	if trimmed != 1 {
		t.Fatalf("trimmed = %d, want 1", trimmed)
	}
	if out[2].Content != supersededStub {
		t.Fatalf("older a.go read not collapsed: %q", out[2].Content)
	}
	if !strings.Contains(out[5].Content, "yyyy") {
		t.Fatal("latest a.go read must be kept intact")
	}
	if !strings.Contains(out[4].Content, "small") {
		t.Fatal("b.go read (read once) must be kept")
	}
	if msgs[2].Content == supersededStub {
		t.Fatal("must not mutate the input slice")
	}
}

func TestTrimForContext_CacheAligned(t *testing.T) {
	rd := func(p string) Message {
		return Message{Role: "user", Content: "<tool_result name=\"read_file\" path=\"" + p + "\">\nbody\n</tool_result>"}
	}
	// Two reads of the same file, but comfortably under budget: nothing is
	// rewritten, so the prefix stays byte-identical and the KV cache hits.
	msgs := []Message{{Role: "system", Content: "sys"}, rd("a.go"), {Role: "assistant", Content: "x"}, rd("a.go")}
	out, trimmed := trimForContext(msgs, 1_000_000)
	if trimmed != 0 {
		t.Fatalf("trimmed = %d, want 0 under budget (cache alignment)", trimmed)
	}
	for i := range msgs {
		if out[i].Content != msgs[i].Content {
			t.Fatalf("message %d changed under budget — prefix not byte-identical", i)
		}
	}
}

func TestHeadTail(t *testing.T) {
	s := "HEAD_START\n" + strings.Repeat("filler line\n", 5000) + "TAIL_END\n"
	got := headTail(s, 200)
	if len(got) > 400 {
		t.Fatalf("headTail not bounded: %d bytes", len(got))
	}
	if !strings.Contains(got, "HEAD_START") {
		t.Fatal("head lost")
	}
	if !strings.Contains(got, "TAIL_END") {
		t.Fatal("tail lost (this is the whole point — errors live at the end)")
	}
	if !strings.Contains(got, "elided") {
		t.Fatal("missing elision marker")
	}
	// short input is returned untouched
	if headTail("small", 200) != "small" {
		t.Fatal("short input must pass through")
	}
}

func TestLineDiff(t *testing.T) {
	a := "one\ntwo\nthree\nfour\n"
	b := "one\nTWO\nthree\nfour\nfive\n"
	added, removed, body := lineDiff(a, b)
	if added != 2 { // "TWO" and "five"
		t.Fatalf("added = %d, want 2", added)
	}
	if removed != 1 { // "two"
		t.Fatalf("removed = %d, want 1", removed)
	}
	plain := stripAnsi(body)
	if !strings.Contains(plain, "- two") || !strings.Contains(plain, "+ TWO") || !strings.Contains(plain, "+ five") {
		t.Fatalf("diff body missing changes:\n%s", plain)
	}
}

func TestUndoLast(t *testing.T) {
	resetChanges()
	defer resetChanges()
	dir := t.TempDir()

	// edit of an existing file → undo restores prior bytes
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("original\n"), 0o644)
	before, existed := snapshot(f)
	os.WriteFile(f, []byte("changed\n"), 0o644)
	recordChange("a.txt", f, "edit", before, existed)

	// create of a new file → undo deletes it
	g := filepath.Join(dir, "new.txt")
	nb, ne := snapshot(g)
	os.WriteFile(g, []byte("created\n"), 0o644)
	recordChange("new.txt", g, "write", nb, ne)

	if _, ok := undoLast(); !ok { // undo the create
		t.Fatal("undo create failed")
	}
	if _, err := os.Stat(g); !os.IsNotExist(err) {
		t.Fatal("undo of a create must delete the file")
	}
	if _, ok := undoLast(); !ok { // undo the edit
		t.Fatal("undo edit failed")
	}
	if got, _ := os.ReadFile(f); string(got) != "original\n" {
		t.Fatalf("undo edit didn't restore: %q", got)
	}
	if _, ok := undoLast(); ok {
		t.Fatal("undo on empty log should report nothing to undo")
	}
}

func TestExpandMentions(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "greet.go"), []byte("package main\nfunc greet() {}\n"), 0o644)
	j, _ := NewJail(dir, nil, false)

	blocks, names := expandMentions(j, "please explain @greet.go, thanks")
	if len(names) != 1 || names[0] != "greet.go" {
		t.Fatalf("names = %v, want [greet.go]", names)
	}
	if !strings.Contains(blocks, `<file path="greet.go">`) || !strings.Contains(blocks, "func greet()") {
		t.Fatalf("blocks missing file content: %q", blocks)
	}
	// a mention that isn't a real file is left alone (no injection)
	if _, n := expandMentions(j, "ping @someone about it"); len(n) != 0 {
		t.Fatalf("attached a non-file mention: %v", n)
	}
	// escaping the jail is refused
	if _, n := expandMentions(j, "read @../../etc/passwd"); len(n) != 0 {
		t.Fatalf("attached a path outside the jail: %v", n)
	}
}

var reAnsiTest = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripAnsi(s string) string { return reAnsiTest.ReplaceAllString(s, "") }

func TestLatexToUnicode(t *testing.T) {
	cases := map[string]string{
		`x^2 + y_i`:                  "x² + yᵢ",
		`\frac{a}{b}`:                "a⁄b",
		`\sqrt{x}`:                   "√(x)",
		`\alpha \times \beta`:        "α × β",
		`\sum_{i=1}^{n}`:             "∑ᵢ₌₁ⁿ",
		`E = mc^2`:                   "E = mc²",
		`\pi \approx 3.14`:           "π ≈ 3.14",
		`x^{10}`:                     "x¹⁰",
		`\theta \leq \epsilon`:       "θ ≤ ε",
		`\|\nabla L\| \leq \epsilon`: "‖∇ L‖ ≤ ε",
	}
	for in, want := range cases {
		if got := latexToUnicode(in); got != want {
			t.Errorf("latexToUnicode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderInline(t *testing.T) {
	cases := map[string]string{
		"**bold** and *it* and `code`": "bold and it and code",
		"inline math $x^2$ here":       "inline math x² here",
		"a price $5 or $10 total":      "a price $5 or $10 total", // not math → untouched
		"snake_case stays literal":     "snake_case stays literal",
	}
	for in, want := range cases {
		if got := stripAnsi(renderInline(in)); got != want {
			t.Errorf("renderInline(%q) → %q, want %q", in, got, want)
		}
	}
}

func TestRenderMarkdownLine(t *testing.T) {
	cases := map[string]string{
		"# Heading":  "Heading",
		"## Sub":     "Sub",
		"- item":     "• item",
		"1. first":   "1. first",
		"> quoted":   "▏ quoted",
		"plain text": "plain text",
		"  - nested": "  • nested",
	}
	for in, want := range cases {
		if got := stripAnsi(renderMarkdownLine(in)); got != want {
			t.Errorf("renderMarkdownLine(%q) → %q, want %q", in, got, want)
		}
	}
}

func TestGresGpuCount(t *testing.T) {
	if n := gresGpuCount(map[string]string{"AllocTRES": "cpu=8,mem=64G,gres/gpu=8"}); n != 8 {
		t.Fatalf("AllocTRES gpu = %d, want 8", n)
	}
	if n := gresGpuCount(map[string]string{"TresPerNode": "gres/gpu:4"}); n != 4 {
		t.Fatalf("TresPerNode gpu = %d, want 4", n)
	}
	if n := gpuFromGres("gpu:a100:4"); n != 4 {
		t.Fatalf("gpuFromGres = %d, want 4", n)
	}
}

func TestScanLog(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "job.log")
	os.WriteFile(f, []byte("sglang launching\nINFO: Uvicorn running on http://0.0.0.0:8085 (Press CTRL+C)\n"), 0o644)
	port, engine, _ := scanLog(f)
	if port != 8085 {
		t.Fatalf("port = %d, want 8085", port)
	}
	if engine != "sglang" {
		t.Fatalf("engine = %q, want sglang", engine)
	}
}

func TestLaunchScriptFromSbatch(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "host")
	os.MkdirAll(host, 0o755)
	script := filepath.Join(host, "startup.sh")
	os.WriteFile(script, []byte("#!/bin/bash\nvllm serve --model-path /models/DeepSeek-V4-Flash-FP8 --port 8085 --tp 8\n"), 0o644)

	sbatch := filepath.Join(dir, "job.sbatch")
	os.WriteFile(sbatch, []byte(
		"#!/bin/bash\nsrun --container-mounts=\""+host+":/cont\" bash -c \"/cont/startup.sh\"\n"), 0o644)

	// mount translation: /cont/startup.sh -> <host>/startup.sh
	got := resolveLaunchScript(sbatch)
	if got != script {
		t.Fatalf("resolveLaunchScript = %q, want %q", got, script)
	}
	port, engine, model := parseLaunchScript(got)
	if port != 8085 || engine != "vllm" || model != "DeepSeek-V4-Flash-FP8" {
		t.Fatalf("parseLaunchScript = (%d,%q,%q), want (8085,vllm,DeepSeek-V4-Flash-FP8)", port, engine, model)
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
