package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// The one-shot run, and the contract a program on the other end of it can read.
//
// A deterministic wrapper (cron) takes a ticket, makes a worktree, calls lca
// once, unattended, and then does the state transitions itself: commit, push,
// MR, labels, a comment on the ticket. lca's job is to change files in its own
// tree and run checks. That wrapper has exactly one decision to make afterwards
// and it has to make it without a human reading anything:
//
//   - the model did not manage it      → the ticket goes to a person
//   - the infrastructure fell over     → put the ticket back, try again later
//   - lca was called wrong             → alert somebody, do not touch the ticket
//
// "exit 1, see the log" is none of those, which is why the status below is a
// CLOSED set, the exit code is a table, and the whole of stdout under -json is
// one JSON object.

// The statuses. Every one of them is a sentence about the TASK, decided by the
// verifier or by what broke — never by the model's opinion of its own work.
const (
	statusPassed     = "passed"          // the check ran and was green
	statusFailed     = "failed"          // the check ran and stayed red
	statusUnverified = "unverified"      // nothing checked it: no -check was given
	statusBudget     = "budget_exceeded" // out of time, steps or tokens before anything settled
	statusInfra      = "infra_error"     // the gateway, a member or an MCP server is not there
	statusCancelled  = "cancelled"       // SIGINT
	// statusConfig is lca's own answer to the one row of the table that has no
	// status word: lca was called wrong, or its configuration is wrong, and
	// nothing about the task was decided. It never reaches the JSON object,
	// because there is no object to write — see oneShot.
	statusConfig = "config_error"
)

// The exit codes, as the requirements document fixes them. They are the wrapper's
// whole input, so they are a table and not a judgement:
//
//	0    passed            → review / MR
//	1    failed, unverified → agent:needs-human
//	2    usage or config    → alert, do not touch the ticket
//	3    infra_error        → put the ticket back in todo, retry later
//	4    budget_exceeded    → agent:needs-human, marked "did not fit"
//	130  cancelled          → put the ticket back in todo
const (
	exitOK        = 0
	exitFailed    = 1
	exitUsage     = 2
	exitInfra     = 3
	exitBudget    = 4
	exitCancelled = 130
)

// usageErr is a mistake in how lca was CALLED, or in what it was configured
// with: a flag that contradicts another, a prompt file that is not there, a
// check command the sandbox will never run. Retrying it tonight changes nothing,
// so it is never infra_error, and no model failed, so it is never failed.
type usageErr struct{ msg string }

func (e *usageErr) Error() string { return e.msg }

func usageErrf(format string, a ...any) *usageErr {
	return &usageErr{msg: fmt.Sprintf(format, a...)}
}

// infraErr marks something lca DEPENDS ON being unreachable — the machine a
// check runs on, a server a tool talks to. It is the distinction the wrapper
// exists to make: feeding "ssh: connect: host is down" back to a model as "your
// change did not pass" asks it to fix a VPN, and sending that ticket to a human
// wastes the human the same way.
type infraErr struct {
	what string // "member stage", "mcp server jira" — what was not reachable
	err  error
}

func (e *infraErr) Error() string {
	if e.what == "" {
		return e.err.Error()
	}
	return e.what + " is not reachable: " + e.err.Error()
}

func (e *infraErr) Unwrap() error { return e.err }

// errCutStream is the one transport failure the stream reader can only describe
// in prose — no net.Error survives a [DONE] that never came — so chat.go wraps
// it in this sentinel. A reply that stops mid-flight is a proxy or the gateway
// going away, never a model failing a task, and the wrapper has to retry it
// rather than hand the ticket to a person.
var errCutStream = errors.New("the connection to the gateway dropped mid-reply")

// resultTokens is what the run cost. The names are the gateway's own
// (prompt/completion/cached), so a number in a Jira comment can be compared with
// a number in the trace without a glossary.
type resultTokens struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Cached     int `json:"cached"`
}

// runResult is the whole of stdout under -json: one object, at the end, and
// nothing else. Every field is something the wrapper or the eval harness acts
// on; anything only a human reads lives in the transcript, the trace and the
// HTML report, whose paths are here.
type runResult struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`

	Session string   `json:"session"`
	Role    string   `json:"role"`
	Models  []string `json:"models"`
	Tier    string   `json:"tier,omitempty"`

	Attempts int `json:"attempts"`
	// The three fields that describe the check are all UNCONDITIONAL, and that is
	// the point of them having no omitempty. CheckExit is a POINTER because 0 is
	// the one answer that must not be inventable: a run with no check, or one whose
	// check never got to run, reports null, and a wrapper reading
	// `check_exit == 0` as "green" cannot be lied to by a field that was simply
	// never set. Its two neighbours then have to be present too — a wrapper
	// written against the documented object did `r["check_cmd"]` and got a
	// KeyError on exactly the row that sends the ticket to a human, because that
	// row is the one where omitempty fired.
	CheckCmd  string `json:"check_cmd"`
	CheckExit *int   `json:"check_exit"`
	CheckTail string `json:"check_tail"`
	// CheckLogs are the files holding the FULL output of every check of every
	// attempt, in the order they ran. check_tail above is a selection — on a stand
	// it has to be, because the output is twenty thousand lines — and these are
	// the bytes it was selected from, so the omission markers in the tail ("lines
	// 41-8213 omitted") are line numbers into these files. The wrapper attaches
	// them to the ticket when a human has to look.
	//
	// Always present and never null, for the reason its three neighbours have no
	// omitempty: a wrapper written against the documented object indexes the key,
	// and `[]` is an answer while a KeyError on the row that calls a human is not.
	CheckLogs []string `json:"check_logs"`

	FilesChanged int `json:"files_changed"`
	DiffBytes    int `json:"diff_bytes"`

	Turns        int `json:"turns"`
	ToolCalls    int `json:"tool_calls"`
	ToolErrors   int `json:"tool_errors"`
	InvalidCalls int `json:"invalid_calls"`

	Tokens     resultTokens `json:"tokens"`
	DurationMs int64        `json:"duration_ms"`
	// StartedAt and FinishedAt are the run's two ends in UTC RFC3339, because a
	// duration cannot answer the question a ticket is read with: "the stand went
	// down at 02:14 — had this run finished by then". The wrapper has the clock
	// time it launched us, but not the one we stopped at, and a cron job's own
	// timestamps are the moments around the run rather than its own.
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`

	Transcript string `json:"transcript"`
	Trace      string `json:"trace"`
	// Review is the reviewer's structured verdict, present only on a -diff-base
	// run and absent when one was asked for and could not be read. Absent and not
	// an empty object, on purpose: the wrapper's test is `"review" in result`, and
	// a review that could not be read must not be able to look like one that
	// approved. The reason field says what went wrong, and the status is failed.
	Review *reviewReport `json:"review,omitempty"`

	// Summary is where -summary wrote the short markdown, absent when none was
	// asked for — or when there was nothing to summarise (an infra_error before
	// the first model reply). The wrapper pastes the file's contents into the
	// merge request and the ticket, so it needs the path rather than the text.
	Summary string `json:"summary,omitempty"`

	// Without these three a run before a prompt edit cannot be compared with one
	// after it: the numbers would be from two different programs, two different
	// teams and two different instructions, with nothing on the record to say so.
	// PromptHash is the role's assembled SYSTEM PROMPT — the text this session was
	// actually sent — and it is the one the other two cannot stand in for: an
	// AGENTS.md edit or a tool added to the role's tools: moves neither the binary
	// nor roles.yaml, and each of them rewrites the prompt (see promptFingerprint).
	LCAVersion string `json:"lca_version"`
	RolesHash  string `json:"roles_hash"`
	PromptHash string `json:"prompt_hash"`
}

// exitCode is the table above. machine says the caller passed -json, which is
// the only thing that distinguishes the two statuses that mean "nobody checked"
// from the old contract of `lca "task"`: that was "0 when the model answered",
// nothing verified it then either, and a bare one-shot in a terminal must keep
// meaning what it meant. A broken gateway, a cancellation and a red check are
// reported the same way to everyone.
func (r runResult) exitCode(machine bool) int {
	legacy := !machine && r.CheckCmd == ""
	switch r.Status {
	case statusPassed:
		return exitOK
	case statusInfra:
		return exitInfra
	case statusCancelled:
		return exitCancelled
	case statusConfig:
		return exitUsage
	case statusBudget:
		if legacy {
			return exitOK
		}
		return exitBudget
	case statusUnverified:
		if legacy {
			return exitOK
		}
		return exitFailed
	}
	return exitFailed
}

// resolvePrompt decides what the model is asked to do: the positional arguments,
// a file, or stdin.
//
// A ticket is multi-line markdown with quotes, `$` and backticks in it. In argv
// it has to be escaped by whoever builds the command line, it is visible to every
// process on the machine in `ps`, and at 50 KB it is near the command-line length
// limit. -prompt-file takes it off the command line entirely; "-" reads stdin,
// which also leaves nothing for a question to read.
//
// Both at once is a USAGE ERROR and never a concatenation: a wrapper that passes
// both has a bug, and silently gluing a ticket onto a leftover argument sends the
// model a task nobody wrote.
func resolvePrompt(args []string, file string, stdin io.Reader) (string, error) {
	joined := strings.TrimSpace(strings.Join(args, " "))
	if file == "" {
		return joined, nil
	}
	if joined != "" {
		return "", usageErrf("-prompt-file %s and the task %q are two ways to say the same thing — pass one, not both", file, truncate(joined, 60))
	}
	var data []byte
	var err error
	if file == "-" {
		data, err = readPromptBytes(io.LimitReader(stdin, promptFileMax+1))
	} else {
		data, err = readPromptFile(file)
	}
	if err != nil {
		return "", usageErrf("-prompt-file %s: %v", file, err)
	}
	if len(data) > promptFileMax {
		return "", usageErrf("-prompt-file %s is larger than %d bytes — that is not a ticket, and reading it would be the whole of this run's memory",
			file, promptFileMax)
	}
	// The bytes go to the model EXACTLY as they are on disk — no trimming, no
	// re-encoding, no line-ending translation. A ticket quoted in a transcript has
	// to be the ticket, so that "the model was told X" can be checked rather than
	// believed. Only a prompt with nothing in it is refused, because an empty task
	// would otherwise open an interactive session nobody is sitting at.
	if strings.TrimSpace(string(data)) == "" {
		return "", usageErrf("-prompt-file %s is empty — there is no task in it", file)
	}
	// And the bytes have to BE text. encoding/json — which is every consumer of
	// this string: the request to the gateway, the transcript, the trace — replaces
	// an invalid byte with U+FFFD and reports no error, so a ticket exported in
	// cp1251 reaches the model mangled AND reaches the transcript mangled, which is
	// precisely the pair the "byte for byte, visible in the transcript" criterion
	// is checked against: they agree because both were corrupted the same way.
	// Refusing at the door is the only place an encoding bug in the wrapper can
	// still be fixed cheaply.
	if i := firstInvalidUTF8(data); i >= 0 {
		return "", usageErrf("-prompt-file %s is not valid UTF-8 (first bad byte 0x%02x at offset %d) — convert it, e.g. `iconv -f cp1251 -t utf-8`",
			file, data[i], i)
	}
	return string(data), nil
}

// oncePath is a path flag that may be given exactly once. The second Set is the
// error, and it names both values, because "pass one" is only actionable next to
// the two the wrapper passed.
type oncePath struct {
	flag string
	val  string
	set  bool
}

func (p *oncePath) String() string { return p.val }

func (p *oncePath) Set(v string) error {
	if p.set {
		return fmt.Errorf("%s was given twice (%q, then %q) — pass one", p.flag, p.val, v)
	}
	p.val, p.set = v, true
	return nil
}

// probeWritable establishes NOW that a path the run will write at the end can be
// written, by creating it and removing it again when it was not already there.
// A file that exists is left exactly as it is — the run is about to overwrite it,
// and truncating it here would destroy the previous summary for a run that then
// fails at startup for some other reason.
func probeWritable(path, flag string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return usageErrf("%s %s: %v", flag, path, err)
		}
	}
	if st, err := os.Stat(path); err == nil {
		if st.IsDir() {
			return usageErrf("%s %s: that is a directory", flag, path)
		}
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return usageErrf("%s %s: %v", flag, path, err)
	}
	f.Close()
	os.Remove(path)
	return nil
}

// promptFileMax is generous on purpose: the acceptance criterion is a 50 KB
// ticket and this is four megabytes. It is not a budget, it is a guard — without
// it `-prompt-file /dev/zero` (a path a wrapper can produce by interpolating an
// empty variable) is read into memory until the OOM killer arrives, which in a
// cron job looks like nothing at all.
const promptFileMax = 4 << 20

// readPromptFile reads the ticket, having first established that the path is a
// FILE.
//
// os.ReadFile on a named pipe blocks in open(2) until a writer appears, and this
// read happens before the orchestrator exists, before oneShot's signal handler
// is armed and before any budget: nothing in the program can end it. A wrapper
// feeding the ticket through a fifo whose producer died before opening its end
// holds the worktree and the claimed ticket until somebody looks in the morning
// — the exact hang the "never wait" requirement exists to prevent. A character
// device (/dev/zero) is the same open with the opposite failure.
//
// So the mode is checked first, which also replaces the platform's own "is a
// directory" wording with one lca controls.
func readPromptFile(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file — a directory, pipe, socket or device cannot be a ticket, and reading one can wait forever", modeWord(st.Mode()))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readPromptBytes(io.LimitReader(f, promptFileMax+1))
}

func readPromptBytes(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

// modeWord names what the path turned out to be, because "not a regular file" on
// its own sends the reader to look at the wrong thing.
func modeWord(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "that is a directory"
	case m&os.ModeNamedPipe != 0:
		return "that is a named pipe (fifo)"
	case m&os.ModeSocket != 0:
		return "that is a socket"
	case m&os.ModeDevice != 0:
		return "that is a device"
	case m&os.ModeSymlink != 0:
		return "that is a dangling symlink"
	}
	return "that is not a regular file"
}

// firstInvalidUTF8 is the offset of the first byte that is not part of a valid
// encoding, or -1. The OFFSET and not just "invalid": a 50 KB ticket with one
// bad byte in it is found by seeking to a number, and not by bisecting a file.
func firstInvalidUTF8(b []byte) int {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return -1
}

// checkTrailingFlags refuses a positional argument that looks like a flag.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `lca "do the thing" -json` never sees -json: stdout is not moved, every
// warning, spinner frame and line of the model's prose goes to the caller's JSON
// parser, and the task the model is handed is literally "do the thing -json". A
// wrapper that builds its argv as [lca, ticket, *flags] — or appends -json per
// role to a base command — gets a run whose stdout fails json.loads on the first
// byte and whose ticket was silently rewritten.
//
// -prompt-file together with a positional is already a clean usage error, and
// that asymmetry is what makes this one surprising. After an explicit `--` the
// caller has said they mean it, so the check stands down.
func checkTrailingFlags(args, argv []string) error {
	for _, a := range argv {
		if a == "--" {
			return nil
		}
	}
	for _, a := range args {
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			return usageErrf("flags must come before the task: %q was read as part of it, not as a flag", a)
		}
	}
	return nil
}

// oneShot runs a single task and returns the exit code. With -check the
// verifier, not the model, decides success. Under -json the result object goes
// to out (the real stdout) and every human line to stderr; without it, stdout is
// the model's answer exactly as before.
func oneShot(orch *Orchestrator, sess *Session, prompt, check string, machine bool, out *os.File, notes []string) (code int) {
	// A panic must not be the one way out of here that writes nothing. Without
	// this the wrapper gets Go's own exit 2 — which the table defines as "lca was
	// called wrong: alert, do not touch the ticket", so a bug in lca leaves the
	// ticket claimed forever — with an empty stdout that fails json.loads and a
	// goroutine dump in the log it was collecting. infra_error is the honest
	// reading of a crash for the caller: it is not the ticket's fault, and trying
	// again later is the right move. The stack goes to stderr and to the trace,
	// where a crash belongs, and never into the object.
	//
	// It covers this goroutine only — recover cannot reach another one — which is
	// why timedCall has its own (engine.go).
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		code = oneShotPanic(orch, sess, p, debug.Stack(), machine, out)
	}()
	for _, n := range notes {
		// reconcileModel() hands these back pre-coloured for the banner's
		// contValue() rows, so they are stripped on the way to a stderr that may
		// well be a file. The theme already makes a piped run plain; this is the
		// case it cannot see, a terminal stdout with stderr redirected.
		fmt.Fprintln(os.Stderr, stripANSI(n))
	}
	orch.rec.Event("user", map[string]any{"text": prompt, "mode": "one-shot"})
	// A -diff-base run's task is the operator's prompt WITH the diff and the
	// reply contract under it, all of it in the user message so that the request
	// prefix — the system prompt and the tool schemas, which is what the gateway
	// keys its KV cache on — is byte-identical to every other run of this role.
	// Without -diff-base this is the prompt, unchanged to the byte.
	task := prompt
	if orch.review != nil {
		task = orch.review.taskMessage(prompt)
		orch.rec.Event("review_diff", map[string]any{"base": orch.review.base,
			"from": orch.review.from, "diff_bytes": len(orch.review.diff), "files": orch.review.lines.order})
	}
	sess.Msgs = append(sess.Msgs, Message{Role: "user", Content: task})

	// A signal has to end the RUN and not the process: 130 means "put the ticket
	// back", and a wrapper can only be told that if the transcript, the trace and
	// the result object were written on the way out. Until now a one-shot died
	// where it stood — watchInterrupt arms itself only for the interactive
	// primary, which a one-shot is not.
	//
	// All THREE signals, because SIGINT is the only one a person sends and this run
	// has no person: `timeout 3600 lca …`, systemd's TimeoutStopSec, docker stop,
	// k8s and `pkill lca` all send SIGTERM, and a cron or ssh session going away
	// sends SIGHUP. Unhandled, each of them killed the process outright — no
	// object on stdout, an exit code (143, 129) the table does not define, and the
	// stdio MCP children, each in its own process group, left behind. They all mean
	// the same thing to the wrapper ("put the ticket back"), so they share the one
	// cancellation path that already produces status cancelled and exit 130.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// The run's budget arms itself here, and everything the run starts hangs off
	// the context it returns: the check command's process group, a stdio MCP
	// server, a background subagent and its own children. Cancelling it once is
	// what makes "no child left behind" a property of the shape rather than of a
	// list somebody has to keep extending.
	ctx, cancel := orch.budget.start(ctx)
	defer cancel()
	orch.setRunContext(ctx)

	// Unattended and unbounded in every dimension is the one combination nobody
	// can afford: a step ceiling of `unlimited` is how a long task is allowed to
	// finish, not a licence to spend the night on a gateway with nobody watching
	// the GPU queue. In a terminal the person IS the bound — they can read the
	// turns and press Ctrl-C — so this is refused only where nobody can.
	//
	// After budget.start() on purpose: a budget answers for its own ceilings only
	// once it is armed, so asking before this line read every run as uncapped and
	// the guard never fired.
	if sess.maxSteps() == stepsUnlimited && !orch.budget.bounded() {
		// Row 2 of the table: a mistake in the call. Nothing on stdout and no
		// object, because nothing about the task was decided and no reply arrived.
		why := "steps are unlimited and nothing else bounds this run — add -timeout (say 45m) or -max-tokens, " +
			"or give a step count; an unattended run has to have one real ceiling"
		errLine("%s", why)
		orch.rec.Event("usage_error", map[string]any{"why": why})
		return exitUsage
	}

	start := time.Now()
	// One path whether or not there is a check: with none, RunVerifiedAll runs the
	// agent once and reports "unverified", which is the same loop with the same
	// error handling instead of a second copy of it that classified nothing.
	v := sess.RunVerified(ctx, check, orch.verifyAttempts())
	// The review is read BEFORE the summary and before the result object, because
	// its one retry is another model call: the tokens it costs belong inside the
	// total the wrapper is handed, exactly as the summary's own call does. It is
	// skipped when the run never got to a reply — a dead gateway has no opinion
	// about a diff — so that an infra_error stays an infra_error.
	if orch.review != nil && v.Status != "error" && v.Status != statusCancelled {
		orch.finishReview(ctx, sess, &v)
	}
	files, diffBytes := orch.runChangeStats()
	sess.traceTask(prompt, v, check, diffBytes, files, false, orch.reviewOutcomeOf(sess, v), start, "")

	// The summary is written BEFORE the result object is assembled, so the tokens
	// its own closing call cost are inside the total the wrapper is handed. It is
	// one extra request on a prefix the gateway has already cached, and it happens
	// after the verdict, so it can change nothing about the verdict.
	summary := orch.writeSummary(sess, orch.factsOf(sess, v, check, start))
	// And the transcript is saved AFTER it, so the file a reviewer is handed as a
	// Jira attachment contains the closing call that the trace and the audit log
	// both have. Saved before, the two accounts of what the run cost disagreed by
	// exactly that call. saveTranscript rewrites the whole file, so doing it last
	// is all that is needed.
	terr := sess.saveTranscript()
	r := orch.resultOf(sess, v, check, files, diffBytes, start, terr)
	r.Summary = summary
	if check != "" {
		fmt.Fprintf(os.Stderr, "\n%s  %s\n", statusWord(v.Status), faint("%s"+gSep+"%s", check, plural(v.Attempts, "attempt", "attempts")))
	}
	if r.Status != statusPassed && r.Reason != "" {
		fmt.Fprintln(os.Stderr, stripANSI(warn("%s %s", gDown, r.Reason)))
	}
	// Only the CHECK's own output, and only when it ran: for every other
	// non-passing verdict the tail is the error text, which the reason line above
	// has just printed.
	if v.Checked && v.Status != statusPassed && v.Tail != "" {
		fmt.Fprintln(os.Stderr, v.Tail)
	}
	// The verdict in a line a person can read. Without -json there is nowhere
	// else it appears: the object went past as the model's last reply, which is
	// not a thing anybody reads, and with -json it belongs in the object and the
	// object only.
	if orch.review != nil && !machine {
		for _, l := range reviewLines(v.Review, v.ReviewErr) {
			fmt.Fprintln(os.Stderr, l)
		}
	}
	// Nothing about the task was decided AND nothing happened: there is no result
	// to report, the table gives this row no status word, and inventing one would
	// put a value outside the documented set into a field a wrapper switches on.
	//
	// "And nothing happened" is the part that was missing. A config error can also
	// land in the MIDDLE of a run — a gateway that starts refusing the request, a
	// 400 from an engine that dislikes a field — and by then there is a session id,
	// a transcript with the model's edits in it, a trace and a token bill. Throwing
	// that object away left the wrapper unable even to attach the transcript to the
	// ticket it was told to alert a human about. So the object is written whenever
	// the run produced anything; only a failure before the first reply has nothing
	// to say, and that is the row the criterion tested. A reply is the right test
	// for "anything happened": no tool ran and no file changed without one, because
	// a tool call is something a reply asked for.
	if r.Status == statusConfig && sess.stats.Replies == 0 {
		return exitUsage
	}
	if machine {
		// One object, one newline, on the descriptor stdout was before it was
		// handed to the human output. SetEscapeHTML(false) because a ticket quoted
		// back in check_tail is text and not HTML.
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(os.Stderr, "writing the result failed: "+err.Error())
			return exitInfra
		}
	}
	// The legacy "exit 0 when the model answered" rule protects the BARE one-shot:
	// `lca "task"` in a terminal, with no -check and nothing else to verify it. A
	// run that was handed a budget was handed it by something that means to read
	// the code, so exceeding it is 4 there too, with or without -json.
	return r.exitCode(machine || orch.budget.declared())
}

// factsOf is what the summary is written from: the same numbers the result
// object carries, plus the per-file account the JSON has no room for. It is
// built from the session and the verdict and invents nothing.
func (o *Orchestrator) factsOf(s *Session, v Verdict, check string, start time.Time) summaryFacts {
	prompt, completion, _ := o.budget.spentTokens()
	f := summaryFacts{
		role: s.agent.Name, check: check, attempts: v.Attempts,
		checked: v.Checked, exit: v.Exit, tail: v.Tail,
		files: sessionFileEdits(),
		turns: s.stats.Turns, tools: s.stats.ToolCalls, errs: s.stats.ToolErrors,
		// The RUN's tokens, not this session's: the footer is what goes in the
		// ticket, and a lead that delegated four coders spent their tokens on this
		// one ticket. See resultOf.
		promptTok: prompt, outputTok: completion,
		elapsed:    time.Since(start),
		transcript: o.rec.SessionPath(), trace: o.tracer.Path,
	}
	f.status, f.reason = o.statusOf(s, v, check)
	return f
}

// oneShotPanic turns a crash into the one thing the wrapper can act on. It is
// deliberately small and defensive: everything it touches may be half-built,
// which is why it reports through plain fmt and a bare encoder rather than
// through the view or the panels.
func oneShotPanic(orch *Orchestrator, sess *Session, p any, stack []byte, machine bool, out *os.File) int {
	reason := fmt.Sprintf("lca crashed: %v", p)
	fmt.Fprintln(os.Stderr, reason)
	fmt.Fprintln(os.Stderr, string(stack))
	if orch != nil {
		orch.rec.Event("panic", map[string]any{"err": fmt.Sprint(p), "stack": string(stack)})
	}
	if sess != nil {
		sess.saveTranscript()
	}
	if !machine {
		return exitInfra
	}
	r := runResult{Status: statusInfra, Reason: reason, LCAVersion: lcaVersion(), CheckLogs: []string{}}
	if sess != nil {
		r.Session, r.Role, r.Models = sess.UID, sess.agent.Name, sess.modelChain()
		r.Turns, r.ToolCalls = sess.stats.Turns, sess.stats.ToolCalls
		r.PromptHash = promptFingerprint(sess)
	}
	if orch != nil {
		r.Transcript, r.Trace = orch.rec.SessionPath(), orch.tracer.Path
		r.RolesHash = rolesHash(orch.roles)
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	enc.Encode(r)
	return exitInfra
}

// runChangeStats is "did lca change the tree", which is the only question the
// wrapper asks of files_changed — it decides from it whether there is anything
// to commit, and puts the number in the merge request.
//
// sessionChangeStats alone answers it only for a run with no delegation in it.
// The global change log records a change only for a session that is not isolated
// (builtin_tools.go), because that log drives /undo on the CALLER's tree and a
// subagent's worktree is not it; and the merge that brings a verified delegation
// home is a git operation, so it never enters the log at all. Delegation is the
// headline feature and `lead` is the default entry role, so for the commonest
// shape of run this field read zero for a run that had just rewritten three
// files.
func (o *Orchestrator) runChangeStats() (files, diffBytes int) {
	own, diffBytes := sessionChangedPaths()
	paths := map[string]bool{}
	for _, p := range own {
		paths[p] = true
	}
	// The union and not the sum: a file the caller edited AND a delegation then
	// merged into is one changed file, and "4 files" in a merge request that
	// touched three is the same kind of wrong as "0 files".
	applied, appliedBytes := o.appliedStats()
	for _, p := range applied {
		paths[p] = true
	}
	return len(paths), diffBytes + appliedBytes
}

// noteApplied records what a delegation's integration put into the caller's
// tree. delegate.go knows it at the apply site — it prints "applied N files"
// from the same numbers — and it is the only place that can, because the merge
// is a git operation and never touches the tool-level change log.
func (o *Orchestrator) noteApplied(root string, files []string, diffBytes int) {
	if o == nil || len(files) == 0 {
		return
	}
	o.appliedMu.Lock()
	defer o.appliedMu.Unlock()
	if o.appliedFiles == nil {
		o.appliedFiles = map[string]bool{}
	}
	for _, f := range files {
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, f)
		}
		o.appliedFiles[filepath.Clean(p)] = true
	}
	o.appliedBytes += diffBytes
}

func (o *Orchestrator) appliedStats() (paths []string, diffBytes int) {
	if o == nil {
		return nil, 0
	}
	o.appliedMu.Lock()
	defer o.appliedMu.Unlock()
	for p := range o.appliedFiles {
		paths = append(paths, p)
	}
	return paths, o.appliedBytes
}

// resultOf assembles the object from what the session and the verdict already
// know. It invents nothing: every number here is one lca counted while it ran.
func (o *Orchestrator) resultOf(s *Session, v Verdict, check string, files, diffBytes int, start time.Time, terr error) runResult {
	// The RUN's tokens and not this session's. The budget counts every session —
	// the primary, the compactor, every subagent, the closing summary call — which
	// is what the ceiling is written against; s.stats counts one. Reading s.stats
	// here put a `reason` of "the run's token budget is spent: 1.1k of 900" next
	// to a `tokens` of 220 in the same object, and an eval comparing two prompts
	// where one delegates compared a number that was wrong by whatever the
	// subagents spent.
	prompt, completion, cached := o.budget.spentTokens()
	r := runResult{
		Session: s.UID, Role: s.agent.Name, Models: s.modelChain(), Tier: o.activeTier(),
		// The command line goes through the same filter as everything else in this
		// object. It is the operator's own text and not the check's, but the shell
		// expands $BSK_TOKEN before lca is started, so `-check "curl -H \"Authorization:
		// Bearer $BSK_TOKEN\" …"` arrives here as a live credential — and `reason`, on
		// the line below, quotes this same string and has been scrubbed since the day
		// it was written.
		Attempts: v.Attempts, CheckCmd: forPublication(check),
		FilesChanged: files, DiffBytes: diffBytes,
		Turns: s.stats.Turns, ToolCalls: s.stats.ToolCalls, ToolErrors: s.stats.ToolErrors,
		InvalidCalls: s.stats.InvalidCalls,
		Tokens:       resultTokens{Prompt: prompt, Completion: completion, Cached: cached},
		DurationMs:   time.Since(start).Milliseconds(),
		StartedAt:    traceTS(start),
		FinishedAt:   traceTS(time.Now()),
		Transcript:   o.rec.SessionPath(), Trace: o.tracer.Path,
		LCAVersion: lcaVersion(), RolesHash: rolesHash(o.roles), PromptHash: promptFingerprint(s),
		// Never nil: see the field. A run with no check and a run whose check
		// printed nothing both report [], which is the truth in both cases.
		CheckLogs: append([]string{}, v.CheckLogs...),
	}
	if v.Checked {
		exit := v.Exit
		r.CheckExit = &exit
		// The check's output is not ours: a check.sh that echoes its environment on
		// failure, or a test that prints the failing request with its Authorization
		// header, puts a live token here — and this field is the one the wrapper
		// pastes into a public merge request and a ticket comment.
		r.CheckTail = forPublication(v.Tail)
	}
	// The object a -diff-base run exists to produce. Nil unless one was asked for
	// AND could be read, which is what keeps "the reviewer's output was garbage"
	// from reaching the wrapper as a verdict (see statusOf). Its bodies and its
	// summary went through forPublication in review.go, where every path into them
	// passes: this field is pasted straight into merge-request comments.
	r.Review = v.Review
	r.Status, r.Reason = o.statusOf(s, v, check)
	r.Reason = forPublication(r.Reason)
	// A transcript the disk refused to hold must not be ASSERTED. os.WriteFile
	// truncates before it writes, so ENOSPC or a read-only state directory
	// destroys the previous good transcript and leaves nothing in its place —
	// and the object went on naming the path, so the wrapper attached a 0-byte
	// file to the ticket with nothing anywhere saying the only human-readable
	// record of the run was lost. The status stays what it was: it is a fact about
	// the check, and the check really did pass.
	if terr != nil {
		r.Transcript = ""
		r.Reason = strings.TrimSpace(r.Reason + " (the transcript could not be written: " + terr.Error() + ")")
	}
	return r
}

// statusOf turns the verifier's verdict into the closed set above.
//
// The order is the point. A green check outranks everything that happened on the
// way to it. A cancellation and a broken dependency outrank the step cap,
// because "retry later" and "put it back" are recoverable and "did not fit" is
// not. Only then does a red check mean the model did not manage it.
func (o *Orchestrator) statusOf(s *Session, v Verdict, check string) (status, reason string) {
	// A spent budget outranks the shape of the stop it caused. The budget ends a
	// run by cancelling its context, so by the time the verifier reports, every
	// symptom reads as an interrupt — and "the operator pressed Ctrl-C" sends the
	// ticket back to todo while "it did not fit" sends it to a person. Only a green
	// check outranks this, below: if the work was done and verified before the
	// clock ran out, it was done.
	if why := o.budget.tripped(); why != "" && v.Status != statusPassed {
		return statusBudget, why
	}
	// A review that was ASKED FOR decides this run, because producing it is what
	// the run was for.
	//
	// Unreadable is `failed`, and it outranks a green check: a reviewer whose
	// output could not be read has not approved anything, and reporting exit 0 on
	// a run with no `review` field in its object is the one failure mode that
	// lets a bad change through unseen. Readable is `passed` — the review IS the
	// verification, and a reviewer run normally has no -check, which would
	// otherwise have made every successful review `unverified` and exit 1. A red
	// -check beside it still wins: that is a fact about the tree.
	//
	// A run that never reached a reply is left to the ordinary classification
	// below: a dead gateway is infra_error, not a failed review.
	if o.review != nil && v.Status != "error" && v.Status != statusCancelled {
		switch {
		case v.Review == nil:
			return statusFailed, "the reviewer's verdict could not be read, so nothing reviewed this change: " + v.ReviewErr
		case v.Status != statusFailed:
			return statusPassed, ""
		}
	}
	switch v.Status {
	case statusPassed:
		return statusPassed, ""
	case statusCancelled:
		return statusCancelled, "interrupted — nothing here says the task is done"
	case "error":
		st, why := classifyRunErr(firstErr(v.Err, errors.New(v.Tail)))
		return st, why
	}
	// An MCP server that never answered is infrastructure, whatever the check then
	// said: the model worked without a tool it was given, so the red check is not
	// evidence about the change. A green check still wins above — the model got
	// there anyway.
	if down := o.mcp.unreachable(); len(down) > 0 {
		return statusInfra, "could not reach " + strings.Join(down, "; ")
	}
	if s.stats.StepCaps > 0 {
		return statusBudget, fmt.Sprintf("the %d-step cap ended the turn before anything settled it", s.maxSteps())
	}
	if v.Status == statusUnverified {
		return statusUnverified, "nothing verified this: no -check was given, and the model's own word does not count"
	}
	return statusFailed, fmt.Sprintf("`%s` still failed (exit %d) after %s", check, v.Exit,
		plural(v.Attempts, "attempt", "attempts"))
}

// classifyRunErr decides, from an error alone, which of the three things the
// wrapper does next. Being wrong here is expensive in both directions: calling a
// dead gateway "failed" sends a human to read a transcript with no change in it,
// and calling a bad API key "infra" puts the ticket back in the queue forever.
func classifyRunErr(err error) (status, reason string) {
	if err == nil {
		return statusFailed, ""
	}
	var ue *usageErr
	if errors.As(err, &ue) {
		return statusConfig, ue.Error()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return statusCancelled, "interrupted — nothing here says the task is done"
	case errors.Is(err, context.DeadlineExceeded):
		return statusBudget, "the run's deadline passed: " + err.Error()
	}
	if why := infraReason(err); why != "" {
		return statusInfra, why
	}
	// A context overflow that survived the one compaction retry is not "lca was
	// called wrong": the conversation outgrew the window, which is a thing that
	// HAPPENED during the run — a 50 KB ticket plus a few large file reads is the
	// ordinary way there. The table's row 2 tells the wrapper to alert a human and
	// NOT touch the ticket, so reporting it there left the ticket claimed in
	// in-progress forever; "did not fit" is the answer the operator wants, and it
	// is the row the step and token ceilings already use.
	if isContextOverflow(err) {
		return statusBudget, "the conversation outgrew the model's context window, and compacting it once was not enough: " + err.Error()
	}
	var ae *APIError
	if errors.As(err, &ae) {
		// A 4xx that is not a "come back later" is OUR request or OUR credentials:
		// the wrong base path, a key the gateway rejects, a model it does not serve.
		// None of that improves overnight, and none of it is a ticket for a person.
		return statusConfig, err.Error()
	}
	// A request that never left this process: a typo'd scheme, an out-of-range
	// port. infraReason declined it above for the reason given there, and it must
	// not fall through to "the model did not manage it" either — nothing was asked
	// of any model. It is the same row as a key the gateway rejects.
	if why := unsendableReason(err); why != "" {
		return statusConfig, why
	}
	return statusFailed, err.Error()
}

// unsendableReason names a request the HTTP client refused to send, and "" for
// anything else. It is the complement of infraReason's transport group: a
// *url.Error with no *net.OpError, *net.DNSError or syscall.Errno under it never
// reached the network, and a *net.AddrError is a syntax error in the address.
func unsendableReason(err error) string {
	var aerr *net.AddrError
	if errors.As(err, &aerr) {
		return "the endpoint's address is not valid: " + err.Error()
	}
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		return ""
	}
	var oerr *net.OpError
	var derr *net.DNSError
	var serr syscall.Errno
	if errors.As(err, &oerr) || errors.As(err, &derr) || errors.As(err, &serr) {
		return ""
	}
	return "the endpoint could not be called at all: " + err.Error()
}

// infraReason names what makes an error somebody else's machine rather than a
// failed change, and "" when it does not.
func infraReason(err error) string {
	var ie *infraErr
	if errors.As(err, &ie) {
		return ie.Error()
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Overload:
			return "the gateway is at capacity (X-Berserk-Overload): " + ae.Error()
		case ae.State != "":
			return "the gateway is " + ae.State + " (X-Berserk-State): " + ae.Error()
		// 408/425/429 are "ask again"; 5xx is the gateway or the engine behind it.
		// Everything else 4xx is a config mistake and is handled by the caller.
		case ae.Status == 408, ae.Status == 425, ae.Status == 429, ae.Status >= 500:
			return "the gateway answered " + ae.Error()
		}
		return ""
	}
	if errors.Is(err, errCutStream) {
		return err.Error()
	}
	// The transport, matched on TYPE and not on message text so that a Go release
	// rewording a dial error does not move a ticket from one queue to the other.
	//
	// The order is the whole content of this block. A *url.Error is returned for
	// everything http.Client.Do refuses, INCLUDING the requests it refuses to
	// send: `htp://…` (unsupported protocol scheme) and `:99999` (invalid port)
	// come back before a packet leaves, and a typo in LCA_BASE_URL or in
	// config.json's base_url is the single most likely mistake in this pipeline.
	// Calling those "not reachable" is exit 3, which returns the ticket to todo
	// and retries it every tick forever, while the right answer is row 2: alert
	// somebody and leave the ticket alone. So a syntax error in the address, and a
	// url.Error with no transport failure underneath it, fall through to "" and
	// classifyRunErr's statusConfig — the same way a non-retryable 4xx already
	// does.
	//
	// "no such host" is deliberately NOT in that group: a DNS failure is genuinely
	// ambiguous (a VPN may be down), and infra is the right guess there.
	var aerr *net.AddrError
	if errors.As(err, &aerr) {
		return ""
	}
	var oerr *net.OpError
	var derr *net.DNSError
	var serr syscall.Errno
	switch {
	case errors.As(err, &oerr), errors.As(err, &derr), errors.As(err, &serr):
		return "the endpoint is not reachable: " + err.Error()
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "the connection dropped: " + err.Error()
	}
	return ""
}

// firstErr is "the real error, or the prose we have instead": the verifier
// records some failures as text on the verdict and nothing else, and a
// classification that only ever saw nil would call them all failures.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil && e.Error() != "" {
			return e
		}
	}
	return nil
}

// modelChain is the models this session could have used, in order: the role's
// chain, or the one model it was pinned to. Plural because a fallback down the
// chain is normal and "which model did this" has more than one honest answer.
func (s *Session) modelChain() []string {
	if len(s.models) > 0 {
		return append([]string(nil), s.models...)
	}
	return []string{s.client.Ref()}
}

// sessionChangeStats counts what the model changed through its OWN tools: the
// distinct files, and the size of the unified diff between the bytes each file
// held when this run first touched it and what is on disk now.
//
// It is deliberately not `git diff`. The wrapper commits, so git is its truth
// and it already has it; a tree with somebody else's uncommitted work in it
// would make git's numbers say things about lca's run that are not true. These
// two are lca's own account of its work.
func sessionChangeStats() (files, diffBytes int) {
	paths, diffBytes := sessionChangedPaths()
	return len(paths), diffBytes
}

// sessionChangedPaths is sessionChangeStats with the paths kept, so a caller
// that has a second source of changed files can take the union rather than the
// sum (see Orchestrator.runChangeStats).
func sessionChangedPaths() (paths []string, diffBytes int) {
	changeMu.Lock()
	defer changeMu.Unlock()
	type orig struct {
		before  []byte
		existed bool
	}
	firsts := map[string]*orig{}
	var order []string
	for _, c := range changeLog {
		if _, ok := firsts[c.abs]; !ok {
			firsts[c.abs] = &orig{c.before, c.existed}
			order = append(order, c.abs)
		}
	}
	for _, abs := range order {
		o := firsts[abs]
		cur, _ := os.ReadFile(abs)
		was := ""
		if o.existed {
			was = string(o.before)
		}
		changed, n := diffSize(was, string(cur))
		if !changed {
			continue // touched and left exactly as it was: not a change
		}
		paths = append(paths, filepath.Clean(abs))
		diffBytes += n
	}
	return paths, diffBytes
}

// diffSize measures one file's change the way a patch would: only the lines
// that differ, each with its one-character marker and its newline.
//
// lineDiff's rendered output is deliberately not used. That is a TERMINAL
// rendering — it keeps three lines of context either side and wraps every line
// in colour escapes — so diff_bytes taken from it would have depended on the
// theme, and a wrapper comparing two runs' numbers would have been comparing two
// renderings.
func diffSize(was, now string) (changed bool, bytes int) {
	a, b := splitLines(was), splitLines(now)
	// The same ceiling lineDiff uses: above it the LCS walk is O(n·m) over a
	// generated file nobody will read either. Both sides' own size is the honest
	// answer there, and the "did it change" question is still answered exactly.
	if len(a) > 5000 || len(b) > 5000 {
		return was != now, len(was) + len(now)
	}
	for _, o := range diffOps(a, b) {
		if o.kind == ' ' {
			continue
		}
		changed = true
		bytes += len(o.text) + 2
	}
	return changed, bytes
}
