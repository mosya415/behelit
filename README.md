# Latent Coding Agent

Minimal, approval-first CLI coding agent for an air-gapped, single-user
inference host. One static Go binary, stdlib only (HTTP client + stdlib), no
external dependencies, zero egress beyond the configured LLM endpoint.

It runs as an ordinary process under whoever launched it — no setuid, no service
account, no auth layer. Identity, permissions, tickets and the per-file audit
trail come from the process (uid/gid) for free.

## Build & run

```sh
go build -o lca .
LCA_BASE_URL=http://localhost:8000/v1 LCA_MODEL=my-model ./lca
```

The working directory at launch becomes the jail root — all file access is
confined to it.

### One-shot mode

A prompt on the command line runs a single turn and exits — handy for scripts
and CI inside the enclave. stdout carries only the assistant's answer (model
notes go to stderr):

```sh
./lca "summarize what pkg/auth does"        # read-only, prompts if it needs to write
./lca -y "add a nil check to Parse in x.go" # -y auto-approves side effects
```

Without `-y`, side-effecting actions still gate; if stdin is not attached they
are denied rather than run unattended.

### Configuration (env)

| Var            | Default                        | Meaning                                   |
| -------------- | ------------------------------ | ----------------------------------------- |
| `LCA_BASE_URL` | `http://localhost:8000/v1`     | Current OpenAI-compatible endpoint (vLLM/SGLang) |
| `LCA_ENDPOINTS`| unset                          | Extra endpoints (comma-separated) for `/endpoint` switching |
| `LCA_MODEL`    | `local`                        | Model name as served                      |
| `LCA_API_KEY`  | `sk-noauth`                    | Optional bearer token                     |
| `LCA_ROOT`     | current dir                    | Jail root                                 |
| `LCA_ALLOW`    | see below                      | Comma-separated command allowlist         |
| `LCA_DIR`      | `~/.lca`                       | Audit log + session transcripts location  |
| `LCA_CTX_TOKENS` | `24000`                      | Approx. token budget for the sent transcript |
| `LCA_MAX_TOKENS` | unset                        | `max_tokens` per request (0/unset = let the server decide) |
| `LCA_CMD_TIMEOUT` | `120`                       | `run_command` timeout in seconds |
| `LCA_RAW`      | unset                          | If set, stream raw model text (show tool tags) for protocol debugging |
| `LCA_ORG`      | unset                          | Optional brand shown in the banner's ticket header |
| `LCA_NO_CLEAR` | unset                          | If set, don't clear the screen on interactive startup |
| `LCA_DISCOVER` | unset                          | If set, query `/models` to adopt/validate the model (off = trust the configured name) |
| `LCA_RESERVATION` | unset                       | Slurm reservation to scope `/discover` (e.g. `gigalearn-test`) |
| `LCA_USER`     | unset                          | Slurm user filter for `/discover` (`$me` = you) |
| `LCA_SCHEME`   | `http`                         | Scheme for discovered endpoints |

Default allowlist: `ls, cat, pwd, head, tail, wc, git, go, gofmt, grep, rg, find, echo`.

REPL commands: `/discover`, `/endpoint [n|url]`, `/model [name]`, `/approve [on|off|run|edit|status]`, `/reset`, `/exit`. A line ending in `\` continues on the next line, so long prompts can be typed across several lines.

The prompt is a small line editor (`lineedit.go`): arrow keys move the cursor,
↑/↓ walk history, and the usual control keys work (Ctrl-A/E/U/W, Ctrl-C to
cancel a line, Ctrl-D to exit). Typing `/` pops up a live menu of commands;
Tab completes — and after `/model ` the menu suggests the models learned from
`/discover`. Selecting a discovered model with `/model <name>` also switches to
the endpoint that serves it (so you don't stay pointed at the unreachable
default). It uses raw terminal mode on Linux only and falls back to a plain
cooked read when stdin is not a terminal (pipes, one-shot) or off Linux.

The input area is fenced by a hairline above and below, and a submitted prompt
is redrawn as a full-width gray-green band — so past turns are easy to pick out
as you scroll up. The session starts with the screen and scrollback cleared
(`clearScreen`), so nothing above the banner is reachable.

### Endpoints

Start with several endpoints and switch between them at runtime — useful on a
cluster where nodes come and go:

```sh
LCA_BASE_URL=http://node1:8000/v1 LCA_ENDPOINTS=http://node2:8000/v1,http://node3:8000/v1 ./lca
```

`/endpoint` lists the known endpoints with a concurrent **reachability check** —
a green `●` up (any HTTP response counts, so a model router that has no `/models`
route still reads as up) or `✕` down (transport error) for each, current marked.
`/endpoint <n>` switches by list index and `/endpoint <url>` switches to any
address (added to the list) — handy when a SLURM allocation hands out a fresh
`host:port`. A bare `host:port` gets `http://` prepended; include the base path
(e.g. `.../v1`) the server expects. Switches are audited (`endpoint_change`).

### Slurm discovery — `discover.go`

On a scheduler-driven cluster, endpoints move (requeue/preemption) and the port
lives in the container startup script, not a router. `/discover` finds the live
models itself, natively (no external tool):

1. `squeue --states=RUNNING` (scoped by `LCA_RESERVATION` / `LCA_USER`) → jobs + nodes
2. `scontrol show job <id>` → serving node (`BatchHost`), GPU count, log paths, sbatch `Command`
3. read the job log on shared NFS for the bound port (`Uvicorn running on http://…:PORT`)
4. if the log lacks it, read the container startup script the sbatch runs,
   translating the `--container-mounts` path back to its host (NFS) path, and grep `--port`
5. confirm health + model over HTTP (`/health`, `/v1/models`)

It needs only `squeue`/`scontrol` + NFS reads + HTTP from the login/dev node — no
compute-node access. The picker shows per-model health (`● up`, `◐ unhealthy`,
`✕ down`), engine, context window and GPU count. Each becomes
`http://node:port/v1` and the served model name is remembered, so `/endpoint <n>`
switches endpoint **and** selects that endpoint's model together. Nothing is
cached — re-run `/discover` whenever addresses may have changed.

```sh
LCA_RESERVATION=gigalearn-test ./lca      # then: /discover
```

### Model selection

One model per session, sent as the `model` field to the current endpoint. By
default the agent does **not** query the router for a model list — it simply
trusts `LCA_MODEL` (and `/model <name>` to change it). This keeps it decoupled
from whatever `/models` does or doesn't return.

Set `LCA_DISCOVER=1` to opt into discovery: on startup (and on `/endpoint`
switch) the agent queries `GET /v1/models`, adopts the sole served model if the
configured name is absent, or warns if several are served; `/model` then lists
served models with status (`●`, context window, backend). Switches are audited
(`model_change`, and `model_adopt` under discovery).

## Terminal style — `ui.go`

Monochrome terminal aesthetic: grays for chrome, color used **only** as status
(muted phosphor green/yellow/red), separation by 1px-style hairlines (`─`), and
UPPERCASE labels. Status glyphs are shared everywhere: `●` ready/ok, `◐` partial,
`✕` failed, `·` muted. Casing is applied only to our own chrome (labels, section
titles, status words) — never to data (paths, model ids, commands, file
contents, diffs), which is always shown verbatim. Hairline width follows
the real terminal column count (a read-only `TIOCGWINSZ` ioctl on unix; no cgo,
no deps), falling back to `$COLUMNS` then 80.

On screen the model's prose is shown but the tool-call tags are hidden — the
clean action markers (`· READ_FILE …`, `● APPROVAL REQUIRED …`) stand in for
them, each followed by a one-line outcome (`→ 2 lines`, `→ 3 matches`,
`● edited sample.txt (1 replacement)`). Prose streams character-by-character;
only a line that could be a tag (it opens with `<`) is briefly buffered to
end-of-line to decide. The hide/show decision reuses the same tag matcher as the
parser, so what the screen hides is exactly what will execute. The full text
(tags included) is still kept in the transcript and fed back to the model;
`LCA_RAW=1` streams it verbatim for debugging a model's protocol adherence.

## Markdown & math rendering — `markdown.go`, `math.go`

The model's prose is rendered as Markdown in the monochrome style: headings and
`**bold**` in bold, `*italic*` in italic, `` `inline code` `` in a muted teal
(no box), `-`/`1.` lists with a `•` bullet, `>` quotes with a faint bar, fenced
` ``` ` code blocks behind a `│` gutter, and pipe tables rendered as aligned
columns with a hairline under the header (emoji width is accounted for).
Emphasis is limited to `*`/`**` (underscores stay literal, since they are common
in code identifiers).

Inline math in `$…$` / `$$…$$` is approximated in Unicode (`math.go`): Greek
letters, operators, super/subscripts, roots and simple fractions —
`$\theta_{t+1} = \theta_t - \alpha \nabla L$` renders as
`θₜ₊₁ = θₜ - α ∇ L`. A `$…$` span is only treated as math if it carries a TeX
marker, so `$5 and $10` is left alone. A terminal can't typeset math, so this is
a legible approximation, not true typesetting — for that, use the (not-yet-built)
HTML report. Rendering is line-buffered (a line is formatted once complete);
`LCA_RAW=1` disables all of it and streams verbatim.

Reasoning from "thinking" models is rendered dimmed, set apart from the answer:
a `<think>…</think>` block in the content has its tags dropped and its lines
shown faint, and a separate `reasoning_content` stream field (vLLM/SGLang
reasoning parsers) is shown the same way. Reasoning is display-only — it is not
part of the answer returned to the model.

## Design (the four non-trivial parts)

1. **Tool-call transport — `protocol.go`.** The model emits line-anchored XML-ish
   tags in plain text; we parse them ourselves instead of trusting each model's
   native `--tool-call-parser`. Tags must be alone on a line, so code bodies
   containing `<`, `>` or quotes don't break parsing. No native function calling.

2. **Applying edits — `edit.go`.** Strict, verbatim `<search>`/`<replace>`. Zero
   matches or more than one match is reported back to the model as an error so it
   regenerates — never a silent fuzzy pick. `<write>` for whole small or new
   files — it creates missing parent directories (jail-checked first, so new
   dirs stay inside the root). No
   unified diff (it drifts on quantized weights).

When a response is cut off by the generation length limit
(`finish_reason == "length"`) with no usable tool call, the agent continues
automatically (a `auto_continue` audit event) instead of ending the turn and
making you type "continue". And if the model *describes* a change as a diff or
code block instead of emitting an `<edit>`/`<write>` tag (so nothing would be
applied), the agent asks it to redo the change as a real tool call
(`nudge_edit`, bounded so a genuine "show me a diff" answer still ends).

3. **Context — `tools.go` + `context.go`.** The repo is never dumped into the
   prompt. The model pulls what it needs via auto-running
   `list_dir`/`grep`/`read_file`. The
   full transcript is kept on disk for audit, but the copy *sent* to the model is
   trimmed to an approximate token budget (`LCA_CTX_TOKENS`): the oldest
   `tool_result` outputs are collapsed to a stub first (the model already
   extracted what it needed), while user instructions and the assistant's own
   reasoning are preserved. Keeps prefill bounded across long sessions.

4. **Scope as defense-in-depth — `jail.go` + `approval.go`.** A realpath jail
   (symlink-resolved, prefix-checked) confines every path to the root, and
   `run_command` execs argv directly with **no shell** against an allowlist
   (pipes/redirects are inert). Approval is the soft gate (intent); the jail is
   the hard gate (reach). Both are required.

`read_file`/`grep` run automatically (no side effects). `edit`/`write`/
`run_command` require an explicit `y` at the prompt. A running command streams
its output live behind a dim `│` gutter, its stdin is the null device (so it
can't hang waiting for input), it is bounded by `LCA_CMD_TIMEOUT`, and Ctrl-C
interrupts just that command — not the agent.

### Approval modes — `approval.go`

At each gate the prompt is `[y/N/a=all]`:

- `y` — approve just this action
- `n` / Enter — deny (the model is told and can adjust)
- `a` — approve this and auto-approve every later action **this session**

Trust can also be granted per **class** from the REPL, so you can auto-run
allowlisted commands while still confirming every file change (or vice versa):

- `/approve run` — auto-approve `run_command`
- `/approve edit` — auto-approve file mutations (`edit`, `write`)
- `/approve on` — auto-approve everything; `/approve off` — back to prompting
- `/approve status` — show the current posture

The audit log records `auto: true` on every action granted by a trusted class,
so an interactive `y` and an auto-approval are always distinguishable after the
fact. `-y` on the command line trusts all classes for a one-shot run.

## Audit & transcript — `recorder.go`

Both artifacts live under `$LCA_DIR` (default `~/.lca`), created `0600` under the
invoking user's own uid — the audit trail is per-user for free, no extra plumbing.

- `audit.jsonl` — append-only, one JSON event per line (timestamp, uid, user,
  pid, session, kind + fields). Records every tool call, whether it was approved,
  and the outcome. Never rewritten.
- `transcripts/<session>.json` — the full running message list, rewritten after
  every turn so a crashed or killed session stays reviewable.

Replies stream token-by-token over SSE; streaming is a UX layer only — the
tool-call parser sees the fully assembled text, so it cannot affect correctness.

## Protocol reference

```
<read_file path="rel/path.go"/>
<read_file path="rel/path.go" lines="40-80"/>
<list_dir path="subdir"/>
<grep pattern="regexp" path="subdir"/>
<run_command>
go test ./...
</run_command>
<write path="rel/new.go">
...full content...
</write>
<edit path="rel/path.go">
<search>
exact existing text
</search>
<replace>
new text
</replace>
</edit>
```

## Layout

```
main.go       REPL + agentic loop + tool dispatch
config.go     env-based configuration
llm.go        OpenAI-compatible chat-completions client
protocol.go   line-anchored tag parser (tool-call transport)
tools.go      read_file, grep, run_command (no-shell exec, live output, stdin=EOF)
edit.go       strict search/replace + whole-file write
jail.go       realpath jail + command allowlist
approval.go   soft approval gate + session approve-all mode
context.go    transcript trimming to a token budget (prefill control)
ui.go         terminal styling: palette, hairlines, status glyphs, labels
lineedit.go   raw-mode line editor: history, cursor keys, /command menu
discover.go   native Slurm discovery (squeue/scontrol/log/startup-script + probe)
stream.go     prose filter: hide tool tags, line-buffer for markdown rendering
markdown.go   terminal markdown renderer (headings, emphasis, code, lists, math)
math.go       LaTeX-ish → Unicode approximation for inline/display math
prompt.go     system prompt (kept in sync with protocol.go)
agent_test.go tests for parser / edit / jail / tokenizer
```
