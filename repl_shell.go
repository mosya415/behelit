package main

import (
	"context"
	"strings"
)

// The shell subcommands, reachable without leaving the session. They are not
// reimplemented here: each handler calls the same entry point `lca doctor`,
// `lca report` and `lca eval` call, so the two forms cannot drift and the shell
// path keeps working byte for byte. /run and /members were already written this
// way (repl_cmds.go) and are the template.

// inSession runs a subcommand's entry point from the REPL. Type-ahead is
// captured (a two-minute /eval must not scribble the terminal, and what was
// typed meanwhile belongs to the next prompt — exactly what runTurn does), and
// Ctrl-C reaches the callee through the same watchInterrupt every turn uses.
// The terminal is restored by a defer, so even a panic on the way out cannot
// leave it in turn mode: a command that ends the session's terminal is worse
// than a command that fails.
//
// Raw mode exists only inside LineEditor.ReadLine and pick(), each with its own
// defer restore() and a cancel path that returns through it, so a long command
// always runs with the terminal in turn mode (signals on) and there is no
// raw-mode limbo to leave.
func (r *Repl) inSession(fn func(ctx context.Context) int) int {
	r.in.StartCapture()
	defer r.in.StopCapture()
	var code int
	interruptible(r.sess, func(ctx context.Context) { code = fn(ctx) })
	return code
}

func (r *Repl) cmdDoctor(arg string) bool {
	r.inSession(func(ctx context.Context) int { return runDoctor(ctx, r.orch.cfg, strings.Fields(arg)) })
	return false
}

// cmdReport defaults the bare argument to THIS session's trace. Inside a live
// session "the report" means the conversation you are in, not the newest file on
// disk — and resolveTrace already accepts a path.
func (r *Repl) cmdReport(arg string) bool {
	args := strings.Fields(arg)
	bare, _ := splitLeadingName(args)
	if bare == "" && r.orch.tracer != nil && r.orch.tracer.Path != "" {
		args = append([]string{r.orch.tracer.Path}, args...)
	}
	// The shell prints the bare absolute path because a pipe consumes it; inside a
	// session that was a 100-character path with no glyph, no "wrote", and no hint
	// that -open exists, while every other in-session write names its file the same
	// way. Same code, one different line.
	opened := strings.Contains(arg, "-open")
	r.inSession(func(ctx context.Context) int {
		return reportRun(r.orch.cfg, args, func(dst string) {
			okLine("wrote %s", prettyPath(dst, r.orch.jl.Root))
			if !opened {
				hint("/report -open opens it in a browser")
			}
		})
	})
	return false
}

func (r *Repl) cmdEval(arg string) bool {
	args := strings.Fields(arg)
	if len(args) == 0 {
		errLine("usage: /eval [-run <re>] [-tier <a,b>] [-transport native,text] <dir>")
		hint("a task directory holds one task.md (or task.yaml) per case — README has the format")
		return false
	}
	r.inSession(func(ctx context.Context) int { return runEval(ctx, r.orch.cfg, args) })
	return false
}
