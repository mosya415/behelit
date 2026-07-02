package main

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"strings"
)

func main() {
	cfg := loadConfig()

	jail, err := NewJail(cfg.Root, cfg.Allowed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "jail init failed:", err)
		os.Exit(1)
	}
	rec, err := NewRecorder(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "recorder init failed:", err)
		os.Exit(1)
	}
	defer rec.Close()

	client := NewClient(cfg)
	in := bufio.NewReader(os.Stdin)

	banner(cfg, jail, rec)
	rec.Event("session_start", map[string]any{
		"root": jail.Root, "model": cfg.Model, "endpoint": cfg.BaseURL,
	})
	defer rec.Event("session_end", nil)

	msgs := []Message{{Role: "system", Content: systemPrompt(jail)}}

	for {
		fmt.Print("\n\033[36m›\033[0m ")
		line, err := in.ReadString('\n')
		if err != nil { // EOF (Ctrl-D)
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return
		case "/reset":
			msgs = msgs[:1]
			rec.Event("reset", nil)
			fmt.Println("(transcript cleared)")
			continue
		}

		msgs = append(msgs, Message{Role: "user", Content: line})
		rec.Event("user", map[string]any{"text": line})
		runTurn(client, jail, in, rec, &msgs, cfg.MaxSteps)
		rec.Transcript(msgs)
	}
}

// runTurn drives the agentic loop for one user message: stream the model, execute
// any tool blocks, feed results back, repeat until the model stops emitting
// tools (a final answer) or we hit the step cap.
func runTurn(client *Client, jail *Jail, in *bufio.Reader, rec *Recorder, msgs *[]Message, maxSteps int) {
	for step := 0; step < maxSteps; step++ {
		printed := false
		onDelta := func(s string) {
			if !printed {
				fmt.Print("\n\033[32m●\033[0m ")
				printed = true
			}
			fmt.Print(s)
		}
		reply, err := client.CompleteStream(*msgs, onDelta)
		if printed {
			fmt.Println()
		}
		if err != nil {
			fmt.Println("\033[31mendpoint error:\033[0m", err)
			rec.Event("error", map[string]any{"err": err.Error()})
			return
		}
		*msgs = append(*msgs, Message{Role: "assistant", Content: reply})

		blocks := ParseBlocks(reply)
		if len(blocks) == 0 {
			return // final answer
		}

		results := executeBlocks(jail, in, rec, blocks)
		*msgs = append(*msgs, Message{Role: "user", Content: results})
		rec.Transcript(*msgs)
	}
	fmt.Printf("\033[33m(stopped: hit %d-step cap)\033[0m\n", maxSteps)
	rec.Event("step_cap", map[string]any{"steps": maxSteps})
}

func executeBlocks(jail *Jail, in *bufio.Reader, rec *Recorder, blocks []Block) string {
	var out strings.Builder
	for _, b := range blocks {
		var res string
		switch b.Name {
		case "read_file":
			fmt.Printf("\033[90m read_file %s\033[0m\n", b.Attr["path"])
			res = readFile(jail, b.Attr["path"], b.Attr["lines"])
			rec.Event("read_file", map[string]any{"path": b.Attr["path"], "result": summarize(res)})
		case "grep":
			fmt.Printf("\033[90m grep %q %s\033[0m\n", b.Attr["pattern"], b.Attr["path"])
			res = grepTree(jail, b.Attr["pattern"], b.Attr["path"])
			rec.Event("grep", map[string]any{"pattern": b.Attr["pattern"], "path": b.Attr["path"], "result": summarize(res)})
		case "edit":
			res = gatedEdit(jail, in, rec, b)
		case "write":
			res = gatedWrite(jail, in, rec, b)
		case "run_command":
			res = gatedRun(jail, in, rec, b)
		default:
			res = "error: unknown tool " + b.Name
		}
		fmt.Fprintf(&out, "<tool_result name=\"%s\" path=\"%s\">\n%s\n</tool_result>\n",
			b.Name, b.Attr["path"], res)
	}
	return out.String()
}

func gatedEdit(jail *Jail, in *bufio.Reader, rec *Recorder, b Block) string {
	abs, err := jail.Resolve(b.Attr["path"])
	if err != nil {
		rec.Event("edit", map[string]any{"path": b.Attr["path"], "error": err.Error()})
		return "error: " + err.Error()
	}
	approved := askApproval(in, "edit "+b.Attr["path"], unifiedPreview(b.Search, b.Replace))
	if !approved {
		rec.Event("edit", map[string]any{"path": b.Attr["path"], "approved": false})
		return "user denied this edit"
	}
	res, err := applyEdit(abs, b.Search, b.Replace)
	if err != nil {
		rec.Event("edit", map[string]any{"path": b.Attr["path"], "approved": true, "error": err.Error()})
		return "error: " + err.Error()
	}
	rec.Event("edit", map[string]any{"path": b.Attr["path"], "approved": true, "result": res})
	return res
}

func gatedWrite(jail *Jail, in *bufio.Reader, rec *Recorder, b Block) string {
	abs, err := jail.Resolve(b.Attr["path"])
	if err != nil {
		rec.Event("write", map[string]any{"path": b.Attr["path"], "error": err.Error()})
		return "error: " + err.Error()
	}
	preview := fmt.Sprintf("  write %d bytes to %s", len(b.Body), b.Attr["path"])
	if !askApproval(in, "write "+b.Attr["path"], preview) {
		rec.Event("write", map[string]any{"path": b.Attr["path"], "approved": false})
		return "user denied this write"
	}
	res, err := writeWholeFile(abs, b.Body)
	if err != nil {
		rec.Event("write", map[string]any{"path": b.Attr["path"], "approved": true, "error": err.Error()})
		return "error: " + err.Error()
	}
	rec.Event("write", map[string]any{"path": b.Attr["path"], "approved": true, "bytes": len(b.Body)})
	return res
}

func gatedRun(jail *Jail, in *bufio.Reader, rec *Recorder, b Block) string {
	cmd := strings.TrimSpace(b.Body)
	if !askApproval(in, "run", "  $ "+cmd) {
		rec.Event("run_command", map[string]any{"cmd": cmd, "approved": false})
		return "user denied this command"
	}
	res := runCommand(jail, cmd)
	rec.Event("run_command", map[string]any{"cmd": cmd, "approved": true, "result": summarize(res)})
	return res
}

func banner(cfg Config, jail *Jail, rec *Recorder) {
	who := "?"
	if u, err := user.Current(); err == nil {
		who = fmt.Sprintf("%s (uid %s)", u.Username, u.Uid)
	}
	fmt.Println("\033[1mLatent Coding Agent\033[0m")
	fmt.Printf("  user:  %s\n", who)
	fmt.Printf("  jail:  %s\n", jail.Root)
	fmt.Printf("  model: %s @ %s\n", cfg.Model, cfg.BaseURL)
	fmt.Printf("  audit: %s\n", cfg.Dir)
	fmt.Printf("  log:   %s\n", rec.SessionPath())
	fmt.Println("  commands: /reset  /exit")
}
