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

### Configuration (env)

| Var            | Default                        | Meaning                                   |
| -------------- | ------------------------------ | ----------------------------------------- |
| `LCA_BASE_URL` | `http://localhost:8000/v1`     | OpenAI-compatible endpoint (vLLM/SGLang)  |
| `LCA_MODEL`    | `local`                        | Model name as served                      |
| `LCA_API_KEY`  | `sk-noauth`                    | Optional bearer token                     |
| `LCA_ROOT`     | current dir                    | Jail root                                 |
| `LCA_ALLOW`    | see below                      | Comma-separated command allowlist         |

Default allowlist: `ls, cat, pwd, head, tail, wc, git, go, gofmt, grep, rg, find, echo`.

REPL commands: `/reset` (clear transcript), `/exit`.

## Design (the four non-trivial parts)

1. **Tool-call transport — `protocol.go`.** The model emits line-anchored XML-ish
   tags in plain text; we parse them ourselves instead of trusting each model's
   native `--tool-call-parser`. Tags must be alone on a line, so code bodies
   containing `<`, `>` or quotes don't break parsing. No native function calling.

2. **Applying edits — `edit.go`.** Strict, verbatim `<search>`/`<replace>`. Zero
   matches or more than one match is reported back to the model as an error so it
   regenerates — never a silent fuzzy pick. `<write>` for whole small files. No
   unified diff (it drifts on quantized weights).

3. **Context — `tools.go` + `main.go`.** The repo is never dumped into the
   prompt. The model pulls what it needs via auto-running `read_file`/`grep`; a
   running message transcript is kept to avoid prefill blow-up.

4. **Scope as defense-in-depth — `jail.go` + `approval.go`.** A realpath jail
   (symlink-resolved, prefix-checked) confines every path to the root, and
   `run_command` execs argv directly with **no shell** against an allowlist
   (pipes/redirects are inert). Approval is the soft gate (intent); the jail is
   the hard gate (reach). Both are required.

`read_file`/`grep` run automatically (no side effects). `edit`/`write`/
`run_command` require an explicit `y` at the prompt.

## Protocol reference

```
<read_file path="rel/path.go"/>
<read_file path="rel/path.go" lines="40-80"/>
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
approval.go   soft approval gate
prompt.go     system prompt (kept in sync with protocol.go)
agent_test.go tests for parser / edit / jail / tokenizer
```
