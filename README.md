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
| `LCA_BASE_URL` | `http://localhost:8000/v1`     | OpenAI-compatible endpoint (vLLM/SGLang)  |
| `LCA_MODEL`    | `local`                        | Model name as served                      |
| `LCA_API_KEY`  | `sk-noauth`                    | Optional bearer token                     |
| `LCA_ROOT`     | current dir                    | Jail root                                 |
| `LCA_ALLOW`    | see below                      | Comma-separated command allowlist         |
| `LCA_DIR`      | `~/.lca`                       | Audit log + session transcripts location  |
| `LCA_CTX_TOKENS` | `24000`                      | Approx. token budget for the sent transcript |

Default allowlist: `ls, cat, pwd, head, tail, wc, git, go, gofmt, grep, rg, find, echo`.

REPL commands: `/model [name]`, `/approve [on|off|status]`, `/reset`, `/exit`.

### Model selection

There is no router — one model per session, sent as the `model` field to the
single `LCA_BASE_URL` endpoint. On startup the agent queries `GET /v1/models` to
see what the server actually serves and reconciles it with `LCA_MODEL`:

- configured name is served → used as-is
- not served but exactly one model is offered → that one is adopted (the common
  vLLM/SGLang case: one model per endpoint, whose id rarely matches a guess)
- not served and several are offered → a warning; pick one with `/model <name>`
- discovery unavailable (endpoint down or no `/models`) → configured name used as-is

`/model` lists served models with status — a green `●` (served/ready), a cyan
`→` on the current selection, and each model's context window and backend
(`ctx 32768, vllm`) when the server reports `max_model_len` / `owned_by`.
`/model <name>` switches for later turns. The startup banner shows the same
status for the active model. Adoptions and switches are written to the audit log
(`model_adopt` / `model_change`).

## Terminal style — `ui.go`

Monochrome terminal aesthetic: grays for chrome, color used **only** as status
(muted phosphor green/yellow/red), separation by 1px-style hairlines (`─`), and
UPPERCASE labels. Status glyphs are shared everywhere: `●` ready/ok, `◐` partial,
`✕` failed, `·` muted. Casing is applied only to our own chrome (labels, section
titles, status words) — never to data (paths, model ids, commands, file
contents, diffs), which is always shown verbatim. Hairline width follows
`$COLUMNS` (no ioctl/cgo), default 60.

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
`run_command` require an explicit `y` at the prompt.

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
tools.go      read_file, grep, run_command (no-shell exec)
edit.go       strict search/replace + whole-file write
jail.go       realpath jail + command allowlist
approval.go   soft approval gate + session approve-all mode
context.go    transcript trimming to a token budget (prefill control)
ui.go         terminal styling: palette, hairlines, status glyphs, labels
prompt.go     system prompt (kept in sync with protocol.go)
agent_test.go tests for parser / edit / jail / tokenizer
```
