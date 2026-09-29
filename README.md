# BEHELIT

Approval-first CLI coding agent and **agent orchestrator for open models** —
Qwen, Kimi, GLM, DeepSeek, Hunyuan (hy3) and MiniMax — on their hosted APIs or on
your own vLLM/SGLang endpoints. One static Go binary, stdlib only, no external
dependencies: provider presets, per-model tuning, prompts, agents and every tool
are built in. With only a local endpoint configured, there is zero egress beyond it.

The core ideas of [opencode](https://github.com/sst/opencode) are ported: agents
and subagents (the `task` tool, parallel and background), todo lists, skills,
markdown commands, permission rules, whitespace-tolerant edits, doom-loop
detection, retries with backoff, and anchored-summary compaction. See
[Agents & orchestration](#agents--orchestration).

It runs as an ordinary process under whoever launched it — no setuid, no service
account, no auth layer. Identity, permissions, tickets and the per-file audit
trail come from the process (uid/gid) for free.

## Quick start

```sh
go build -o lca .

# a team of open models through the gateway (berserk-gw)
export LCA_BASE_URL=http://node:18080/v1
./lca init          # writes .lca/roles.yaml from the gateway's models and your stack
./lca doctor        # checks gateway, roles, a real tool call on every model, sandbox
./lca               # interactive session — type a task; /help lists the commands

# or a single model: a local endpoint or a hosted API
LCA_BASE_URL=http://localhost:8000/v1 LCA_MODEL=my-model ./lca
DEEPSEEK_API_KEY=… ./lca -model deepseek/deepseek-v4-pro
```

The directory you start in is the project: every file the agents touch is
inside it (the sandbox), and subagents work in git worktrees of it.

A session opens with a short card — project, role and its model chain, the
team, gateway status, approval mode, config files — then waits for a task.
While agents work you see one line per action (`read sum.go`,
`search "func main"`, `run go test ./...`), subagents as an indented tree with
their verifier result (`✓ go test ./...  1.2s`), and a question whenever
something needs your approval (`y` yes · `n` no · `a` yes to everything this
session). Errors come with the next thing to try.

### One task from the command line

A task on the command line runs once and exits — for scripts and CI. stdout
carries only the answer (notes go to stderr):

```sh
./lca "summarize what pkg/auth does"               # asks before anything with side effects
./lca -y "add a nil check to Parse in x.go"        # -y approves side effects
./lca -check "go test ./..." "fix the flaky test"  # succeeds only if the check passes
```

Without `-y`, side effects still need approval; with no terminal attached
they are refused rather than run unattended.

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
| `LCA_CTX_TOKENS` | auto                         | Approx. token budget for the sent transcript before it's compressed. Auto = 75% of the model's context window (from `/models`), or 24k if unknown |
| `LCA_MAX_TOKENS` | unset                        | `max_tokens` per request (0/unset = let the server decide) |
| `LCA_CMD_TIMEOUT` | `120`                       | `run_command` timeout in seconds |
| `LCA_RAW`      | unset                          | If set, stream raw model text (show tool tags) for protocol debugging |
| `LCA_ORG`      | unset                          | Optional brand shown in the banner's ticket header |
| `LCA_SHOW_THINKING` | unset                     | Start with reasoning expanded to full text (default: collapsed to the live status line; toggle with `/think`) |
| `LCA_LOOP`     | unset                          | Start in autonomous loop mode (toggle with `/loop`) |
| `LCA_INSTRUCTIONS` | unset                      | Path to a project instructions file (overrides the `BEHELIT.md`/`AGENTS.md`/`CLAUDE.md` search) |
| `LCA_KEEP_SESSIONS` | `200`                     | Max transcript files to retain; older ones are pruned at startup (0 disables) |
| `LCA_UNSAFE`   | unset                          | Start with the jail + allowlist off (toggle with `/unsafe`; also `-unsafe`) |
| `LCA_NO_CLEAR` | unset                          | If set, don't clear the screen on interactive startup |
| `LCA_DISCOVER` | unset                          | If set, query `/models` to adopt/validate the model (off = trust the configured name) |
| `LCA_RESERVATION` | unset                       | Slurm reservation to scope `/discover` (e.g. `gigalearn-test`) |
| `LCA_USER`     | unset                          | Slurm user filter for `/discover` (`$me` = you) |
| `LCA_SCHEME`   | `http`                         | Scheme for discovered endpoints |
| `LCA_TOOLS`    | auto                           | Tool transport: `native` (function calling), `text` (tag protocol), `auto` = native for hosted presets, text for the local endpoint |
| `LCA_THINKING` | provider default               | Reasoning: `on`, `off`, or an effort `low`/`medium`/`high`/`max` — translated per provider |
| `LCA_TEMPERATURE` | model card / server default | `temperature` for every request. Unset sends whatever the model's profile prescribes, and nothing at all when it prescribes nothing — a reasoning model run at temperature 0 repeats itself, so no value is invented |
| `LCA_AGENT`    | `build`                        | Primary agent to start with (also `-agent`) |
| `LCA_MAX_STEPS`| `50`                           | Model calls per turn (agents can set their own `steps`) |
| `LCA_SUBAGENT_DEPTH` | `1`                      | How deep subagents may nest (1 = subagents can't delegate) |
| `LCA_MAX_PARALLEL` | `4`                        | Concurrently running top-level subagents |
| `LCA_CONFIG`   | unset                          | Extra JSON config file (after `~/.lca/config.json` and `.lca/config.json`) |
| `LCA_ROLES`    | unset                          | Extra roles file (after `~/.lca/roles.yaml` and `.lca/roles.yaml`) |
| `LCA_REMOTE`   | unset                          | Work on another machine: `host:/path/to/project` (overrides `remote:` in roles.yaml) |
| `LCA_TRACE`    | `$LCA_DIR/traces/<session>.jsonl` | Trace JSONL path |
| `LCA_GW_MAX_WAIT` | `900`                       | Seconds one model call may spend waiting on an overloaded / not-up gateway |
| `LCA_GW_CUT_RETRIES` | `2`                      | Repeats of a turn whose stream was cut after the first byte (0 = none) |
| `LCA_KEEP_WORKTREES` | unset                    | Keep delegate worktrees for inspection |
| `DEEPSEEK_API_KEY`, `MOONSHOT_API_KEY`, `ZAI_API_KEY`/`ZHIPU_API_KEY`, `DASHSCOPE_API_KEY`, `MINIMAX_API_KEY`, `TENCENT_TOKENHUB_API_KEY`, `HUNYUAN_API_KEY`, `OPENROUTER_API_KEY`, … | unset | Keys for the hosted presets (`/providers` shows which are set) |

Default allowlist: `ls, cat, pwd, head, tail, wc, git, go, gofmt, grep, rg, find, echo`.

Commands (`/help` prints them grouped; `/` opens a menu as you type):

| group | commands |
| --- | --- |
| session | `/help`, `/resume [list\|n]`, `/compact`, `/reset`, `/exit` |
| agents | `/agent [name]`, `/agents`, `/role [name setting value]`, `/delegate <role> <task>`, `/tasks [id]`, `/todo`, `/skills` |
| turn | `/retry`, `/edit` |
| review | `/diff`, `/undo`, `/context`, `/stats` |
| modes | `/approve [on\|off\|run\|edit]`, `/think [on\|off\|last]`, `/loop`, `/unsafe` |
| model | `/model [name]`, `/endpoint [n\|url]`, `/providers`, `/discover` |

Commands that don't apply to the current setup (hosted presets and Slurm
discovery when a gateway team is configured, `/skills` with no skills) are
left out of the help and the menu; `/help all` shows everything. Custom
commands from `.lca/commands/` appear in their own group. A line ending in `\`
continues on the next line.

The prompt is a small line editor (`lineedit.go`, `input.go`): arrow keys move
the cursor, ↑/↓ walk history, and the usual control keys work (Ctrl-A/E/U/W,
Ctrl-C to cancel a line, Ctrl-D to exit). It uses raw terminal mode on unix
(macOS included) and falls back to a plain cooked read when stdin is not a
terminal (pipes, one-shot) or on Windows.

**Pasting and typing ahead.** All keyboard input goes through one buffer that
keeps being read *while the agent works*: during a turn the terminal's line
discipline is off (no ~1 KB canonical limit, no echo over the agent's output),
so a large paste survives whole. Whatever arrived meanwhile is handed to the
next prompt as **one staged message**, shown as `[24 lines pasted, 1.5K]` —
press Enter to send it, type to add to it, Ctrl-U to drop it. A short
single-line paste is simply inserted as if typed. This is what keeps a block of
questions from turning into a line-per-turn stampede with text cut mid-word.
An approval question never consumes what you typed before it appeared: that
input is set aside for the next prompt, so a stray `y` can't approve anything.

The input area is fenced by a hairline above and below, and a submitted prompt
is redrawn as a full-width gray-green band — so past turns are easy to pick out
as you scroll up. An interactive session clears the screen and scrollback at startup so it begins
at the top, but stays on the normal screen — so its output remains in the
scrollback and you can scroll back through it. Set `LCA_NO_CLEAR` to skip the
clear.

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

## Models & providers — `providers.go`, `models.go`, `chat.go`

A model is addressed as `provider/model`. Built-in presets (all OpenAI-compatible):

| provider | base URL | models (examples) |
| --- | --- | --- |
| `deepseek` | api.deepseek.com/v1 | deepseek-v4-pro, deepseek-v4-flash, deepseek-chat, deepseek-reasoner |
| `moonshot`, `moonshot-cn` | api.moonshot.ai/v1, api.moonshot.cn/v1 | kimi-k2.6, kimi-k2.5, kimi-k2-thinking |
| `zai`, `zai-coding`, `zhipu` | api.z.ai/api/paas/v4, …/coding/paas/v4, open.bigmodel.cn/api/paas/v4 | glm-5.2, glm-5.1, glm-4.7 |
| `dashscope`, `dashscope-cn` | dashscope(-intl).aliyuncs.com/compatible-mode/v1 | qwen3-coder-plus, qwen3.7-plus, qwen3-max |
| `minimax`, `minimax-cn` | api.minimax.io/v1, api.minimaxi.com/v1 | MiniMax-M3, MiniMax-M2.7 |
| `tencent`, `hunyuan` | tokenhub.tencentmaas.com/v1, api.hunyuan.cloud.tencent.com/v1 | hy3, hunyuan-t1-latest |
| `openrouter`, `siliconflow`, `fireworks`, `together`, `nvidia` | aggregators | any of the above families |

A bare name (or one whose prefix isn't a provider, like `Qwen/Qwen3-Coder-480B`)
goes to the local endpoint as before. `/model deepseek/deepseek-v4-pro` switches
at runtime; `/providers` lists presets and key status.

Each model **family** has a built-in profile (`models.go`) mirroring the vendors'
recommendations as collected by models.dev/opencode: context window and max
output (for the context budget and `max_tokens`, capped at 32k), sampling
(Kimi thinking 1.0/top_p 0.95, GLM-4.6/4.7 1.0, MiniMax 1.0/0.95), and how
reasoning is handled:

- **Reasoning replay.** Interleaved-thinking models need their `reasoning_content`
  sent back with tool-call turns. Kimi and GLM (preserved thinking) replay it for
  every assistant message; DeepSeek and Hunyuan only within the current turn;
  Qwen doesn't; MiniMax keeps `<think>` inline in the content, preserved verbatim.
- **Thinking switch.** `LCA_THINKING` / an agent's `thinking:` becomes the
  provider's dialect: `thinking:{type}` + `reasoning_effort` (DeepSeek V4),
  `thinking:{type}` (Kimi K2.5+), `thinking:{type:"enabled", clear_thinking:false}`
  (Z.ai, on by default), `enable_thinking` (DashScope, on for reasoning models),
  `reasoning_effort` (Tencent hy3), `reasoning:{effort}` (OpenRouter), and
  `chat_template_kwargs` for local vLLM/SGLang.

Requests are retried on 429/5xx/network errors (2s ×2 backoff with jitter, max
30s, honoring `Retry-After`, 5 attempts) as long as nothing has streamed yet. A
context-overflow error (recognized across vendors' phrasings) triggers
compaction and one retry. DeepSeek's `prompt_cache_hit_tokens` counts toward the
cache-hit figure in the perf line.

### Config file — `fileconfig.go`

Optional JSON, read from `~/.lca/config.json`, then `<root>/.lca/config.json`,
then `$LCA_CONFIG` (later wins per key):

```json
{
  "model": "moonshot/kimi-k2.6",
  "thinking": "high",
  "subagent_depth": 1,
  "providers": {
    "gpu2": {"base_url": "http://gpu2:8000/v1", "tools": "native", "dialect": "vllm",
             "models": ["Qwen3-Coder-480B-A35B-Instruct"]}
  },
  "agents": {
    "explore": {"model": "deepseek/deepseek-v4-flash"},
    "general": {"model": "zai/glm-5.2", "steps": 80}
  },
  "permission": {"run": {"*": "ask", "go test *": "allow"}, "web": "deny"}
}
```

A config provider can be a second local vLLM (`gpu2/<model>`), so agents can be
spread across your own GPUs as well as hosted APIs.

### Model selection (local endpoint)

One model per session, sent as the `model` field to the current endpoint. By
default the agent does **not** query the router for a model list — it simply
trusts `LCA_MODEL` (and `/model <name>` to change it). This keeps it decoupled
from whatever `/models` does or doesn't return.

Set `LCA_DISCOVER=1` to opt into discovery: on startup (and on `/endpoint`
switch) the agent queries `GET /v1/models`, adopts the sole served model if the
configured name is absent, or warns if several are served; `/model` then lists
served models with status (`●`, context window, backend). Switches are audited
(`model_change`, and `model_adopt` under discovery).

## Roles through the gateway — `roles.go`, `gwpolicy.go`, `delegate.go`, `verify.go`

For orchestration through the LLM gateway (berserk-gw), point `LCA_BASE_URL`
at it and describe the team in `roles.yaml` (`.lca/roles.yaml`,
`~/.lca/roles.yaml`, or `$LCA_ROLES`; full example in
[`examples/roles.yaml`](examples/roles.yaml)):

```yaml
entry: lead
transport: native
apply: verified            # verified | always | never
defaults: {context: 128000, verify_attempts: 2, check_timeout: 900}
sandbox:
  allow: [go, git, make, pytest, ls, cat, bsk]
roles:
  lead:
    models: [kimi-k2.6, glm-5.2]        # names from the gateway's /v1/models
    effort: high
    context: 200000
    tools: [list_dir, glob, grep, read_file, todowrite, delegate]
    prompt: |
      You lead the task…
  coder:
    models: [qwen3-coder-480b-a35b-instruct, deepseek-v4-pro]
    effort: medium
    tools: [list_dir, glob, grep, read_file, edit, write, run_command]
    check_cmd: go test ./...
    prompt_file: prompts/coder.md
  cheap:
    models: [qwen3-30b-a3b-instruct]
    effort: off
    tools: []
```

A role is an agent bound to an **ordered model chain**, a reasoning effort, a
prompt, an **exact tool set** and a **context limit** (the budget for trimming
and compaction). Models are gateway names only — an address is rejected — and
are checked against `/v1/models` at startup (unlisted ones are dropped from the
chain with a warning). `-role <name>` or `entry:` picks the session's role;
`/agents` shows each role's chain, effort and context.

**`delegate(role, task, check_cmd)`.** The subagent is the same loop running
another role, in its own **git worktree**: a snapshot of the caller's working
tree — uncommitted and new files included — made through a temporary index, so
the user's index and HEAD are never touched. When the subagent stops, the
**verifier** runs `check_cmd` there (the task's, else the role's default) in
the sandbox. Exit 0 is `passed`; otherwise the failing output goes back to the
subagent (up to `verify_attempts`), then `failed`. With no check the status is
`unverified`: the model's own "done" never counts. Only
`{"status", "diff", "test_tail"}` returns to the caller — never the subagent's
transcript. With `apply: verified` a passed diff is applied to the caller's
tree (`git apply`, through the `edit` approval); one that no longer applies is
`conflict`. Several delegations in one reply run in parallel, each in its own
worktree. Inside a worktree edits and sandboxed commands run without prompts;
`bsk` still asks.

`-check` gives a one-shot run the same contract:
`lca -role coder -check "go test ./..." "fix the flaky parser test"` exits 0
only if the check passes.

**Setting the team up by hand.** `roles.yaml` is not the only way in: `/role`
lists the team, `/role <name>` shows one, and `/role <name> <setting> <value>`
changes it live —

```
/role                                   # the team: models, effort, context, tools, check
/role coder model qwen3.8-27b,kimi-k3   # chain; names not served by the gateway are flagged
/role coder effort high                 # off | on | low | medium | high | max
/role coder temperature 0.6             # also top_p, context, steps
/role coder check go test ./...         # what the verifier will run
/role coder tools read_file,edit,run_command
/role new reviewer model glm-5.2        # a role that wasn't in the file
/role save                              # write .lca/roles.yaml (keeps the old one as .bak)
/delegate coder add a regression test for the parser
```

A change to the session's own role takes effect on the next request; `/role
save` serializes the whole team back to YAML, so a team assembled by hand in
one session is the team the next session loads. `/delegate` takes the same path
the model's `delegate` tool does — own worktree, verifier, diff applied on a
pass — and the result is appended to the transcript, so the model knows what
was done.

**Sessions on the wire.** Every request carries `x-session-id` (this session:
the gateway's KV-cache affinity key) and `x-root-session-id` (the whole task
tree). Subagents and the compaction helper get their own session id under the
same root.

**Gateway failure policy** (per model call):

| gateway says | recognized by | client does |
| --- | --- | --- |
| not up | 503 + `X-Berserk-State: paused\|drained`, `cold start failed`, `no (healthy) backend`, 404 `unknown model` | next model of the role (sticky for the session); none left → wait `Retry-After` and walk the chain again |
| overloaded | 503 + `X-Berserk-Overload`, 429, bare 503 | wait `Retry-After` on the same model; **no fallback** |
| failed before the first byte | connection refused/reset, 500/502/504 | exactly one attempt on the next model |
| cut after the first byte | stream ends without a finish reason | repeat the turn on the same model, up to `LCA_GW_CUT_RETRIES` (2) |

Waiting is capped by `LCA_GW_MAX_WAIT`. Context overflow and other 4xx go
straight to the loop.

Why a cut turn is simply repeated: berserk-gw doesn't migrate requests that
carry `tools` (a second replica can't resume a tool call mid-JSON), so the
client has to cover it. A turn is idempotent — tool calls execute only once
they have arrived whole, so a cut turn ran nothing — and the prefix is in the
KV cache, so a repeat costs only the decode. The partial output on screen is
marked void. (The better fix belongs in the gateway: migrate until the first
`tool_calls` delta, and buffer tool-call deltas so the client never sees a
partial call.)

**Tool transport.** `native` (the default) sends `tools` and uses the model's
own tool-call format — the one it was RL-trained on — so the engines behind
the gateway need their tool-call parser enabled (vLLM
`--enable-auto-tool-choice --tool-call-parser <family>`, SGLang
`--tool-call-parser <family>`), and a `--reasoning-parser` to get
`reasoning_content` separately. `text` (the agent's own tag protocol) is a
per-model fallback for a model whose native parser is missing or broken on
your engine:

```yaml
models:
  some-model: {transport: text}
```

**Per-model settings from the model card.** Vendor recommendations differ per
model and are not guessable, so they are configured rather than invented:

```yaml
models:
  kimi-k3:            {temperature: 1.0, top_p: 0.95, effort: max}
  deepseek-v4.1-flash: {temperature: 0.3, top_p: 0.95}
  glm-5:              {reasoning_replay: off}   # server rejects reasoning_content in history
  some-model:         {transport: text}
```

`temperature`/`top_p`/`top_k` and `effort` apply to any request on that model
(a role's own `temperature:` wins); with nothing configured, `models.go`'s
family profile decides, and if that is silent the field is left out of the
request entirely. `/model` prints what a session actually sends. Reasoning
**replay** — feeding the model's own `reasoning_content` back with the tool
results, so its chain of thought survives between tool calls — is on for the
families that want it (`all` = the whole turn's reasoning, `turn` = the last
step's) on both transports; `reasoning_replay: off` is the escape hatch for a
server that 4xx's on it. `lca doctor` probes both: two tool calls in one reply
with nested-JSON arguments, and a history that carries `reasoning_content`.

A session keeps the transport of its role's first model — a fallback never
switches it mid-conversation (the prefix and history format would change);
a role whose chain mixes transports gets a warning. On `text`, a tag that
doesn't parse is reported back to the model as a protocol error (as bad JSON
is on `native`) instead of silently ending the turn.

**Cache-stable prefix.** The system prompt and the tool schemas are computed
once per session and never mention the model, so a fallback, a trim or a
compaction doesn't change them. Compaction runs on the `cheap` role as a
separate request and only rewrites the conversation after the prefix.

**Trace** (`trace.go`). One JSONL line per model turn — role, model,
transport, `usage{prompt_tokens, completion_tokens, cached_tokens}`, TTFT,
duration, finish reason, gateway fallbacks and repeats, every tool call (args,
ok/error, invalid, bytes, ms) and the count of invalid calls (bad JSON, unknown
tool, schema violation, unparsable text tag) — and one per verified task (role, status, check exit, attempts, diff size,
files, applied). Written to `$LCA_DIR/traces/<session>.jsonl` (`LCA_TRACE`).

A call that didn't parse also keeps the model's own text: `raw` holds the
arguments as they were emitted and `raw_reply` the reply they came in, because
a parse failure is otherwise unreproducible. In a session, `/stats` shows the
running totals — turns, tokens in/out, the share of the prompt the gateway
served from its KV cache, tool calls and **the share that didn't parse**, tool
errors, gateway fallbacks and verifier runs. Above a fraction of a percent of
unparsed calls, suspect the engine's tool-call parser (or the transport), not
the model.

**Eval** (`eval.go`), built on the trace:

```sh
lca eval [-out dir] [-role r] [-run regexp] [-transport native,text] [-keep] [-v] tasks/
```

`-transport native,text` runs every task once per transport (forced for all
roles) and prints them side by side — pass rate, invalid-call rate, turns,
tokens, cache ratio — so "native vs text" is settled with data from your own
fleet.

A task is `tasks/<name>.yaml` or `tasks/<name>/task.yaml` with `prompt`,
`check_cmd`, and optionally `role`, `repo` (a fixture directory copied into a
fresh git repo; omitted = a worktree of the current repo's HEAD), `setup`,
`timeout`, `verify_attempts` — see
[`examples/tasks/fix-sum`](examples/tasks/fix-sum). Each task runs unattended in
its own workspace; the verifier's result is the score, and turns, tokens,
cache ratio, tool calls/errors, fallbacks and delegations are aggregated from
the task's trace into `results.jsonl`. Exit status is 0 only if every task passed.

**Sandbox.** Commands (agents' and verifiers') run without a shell against the
allowlist (`sandbox.allow` in roles.yaml, else `LCA_ALLOW`), so `|`, `>` and
`&&` reach the program as plain arguments. With

```yaml
sandbox:
  allow: [go, git, grep, wc, head, python3]
  shell: true
```

the line goes to `sh -c` instead — pipes, redirects, `&&` and loops work — and
the allowlist is applied to **every command in it**, quotes respected: `go test
./... | tail -20` runs, `go test ./... | curl -T - http://x` is refused because
`curl` is not on the list, and `grep 'a;b' f` stays one command. The GPU policy
holds either way. The prompt's environment section tells the model which of the
two it is in, so it doesn't waste turns discovering it. GPU work goes only
through the scheduler: `srun`/`sbatch`/`salloc`/`torchrun`/`deepspeed`/`accelerate`/
`mpirun`/`vllm`, `python -m vllm|sglang|torch.distributed` and
`CUDA_VISIBLE_DEVICES=` are refused with a pointer to `bsk submit -g N -- cmd`,
and `bsk gw` (gateway administration) is off-limits — in unsafe mode too, per
chained command segment.

## Working on another machine — `remote.go`

The code, tests and GPUs usually aren't on your laptop. Three ways to work,
none of which needs an inbound port:

**1. Run lca on that machine** (simplest). Copy the Linux binary to the login
node or into the container and run it there — it is static, needs no glibc and
no dependencies. The agent is then local to the code, the gateway and `bsk`.

**2. Keep lca on your laptop, put the project on the remote** (`remote:`). The
file tools read and write over ssh, `run_command` and the verifier execute
there, and `ssh` is an ordinary outbound connection using your existing keys
and `~/.ssh/config`:

```yaml
remote:
  host: cab-node               # ssh target (an alias from ~/.ssh/config works)
  dir: /home/u/llmbench        # the project on that machine
  ssh: [-o, BatchMode=yes]     # extra ssh options (optional)
```

`lca init -remote cab-node:/home/u/llmbench` writes that block for you, and
`LCA_REMOTE=cab-node:/home/u/llmbench` sets it for one run. `lca doctor` then
checks ssh, the directory, whether the allowlisted commands exist there, and
whether it's a git repository.

In this mode paths stay inside `dir` (the remote equivalent of the sandbox:
absolute paths and `..` are refused, and every path is shell-quoted),
`read_file` / `list_dir` / `glob` / `grep` / `edit` / `write` all work over the
same ssh connection, the read-before-edit guard uses remote mtimes, and the
verifier runs `check_cmd` on the remote. `delegate` is switched off there
because its git worktree is local — subagents share the remote tree through
`task`, or you run lca on that machine (option 1).

**3. Leave the code local and send only GPU work to the cluster** through
`bsk submit` (see the sandbox section). `ssh`, `scp` and `rsync` are on the
default allowlist, so an agent can reach another host by itself when a task
needs it — and what it sends is checked too: `ssh node srun …` is refused with
the same message as a local `srun`, `bsk gw` stays off-limits.

## Agents & orchestration — `agents.go`, `engine.go`, `task.go`

The loop is a reusable engine: an **Orchestrator** holds everything shared
(jail, approval gate, audit log, providers, agents, skills, commands), and a
**Session** is one conversation driven by one agent on one model. The REPL owns
the primary session; subagents are child sessions running the very same loop,
concurrently, each on its own model. Everything a session shows goes through a
`View` (`view.go`), the seam for other front-ends.

**Agents** are named configurations of that loop — prompt, model, sampling,
thinking, step budget, permission rules:

| agent | mode | what it is |
| --- | --- | --- |
| `build` | primary | default: reads, edits, runs, delegates |
| `plan` | primary | read-only; may only write plans under `.lca/plans/` |
| `explore` | subagent | fast read-only search (glob/grep/read/list), no edits, no commands |
| `general` | subagent | multi-step work incl. edits, for parallel units of work |

Switch primary agents with `/agent plan`; `/agents` lists all. Define your own
as markdown in `.lca/agents/<name>.md` (also read from `~/.lca/agents`,
`.opencode/agent(s)`, `~/.config/opencode/agent(s)` and `.claude/agents`, so
existing opencode / Claude Code agents work), or under `"agents"` in config:

```markdown
---
description: Reviews a diff for bugs. Use after non-trivial edits.
mode: subagent            # primary | subagent | all
model: deepseek/deepseek-v4-pro
thinking: high
steps: 30
temperature: 0.3
permission:
  edit: deny
  run:
    "*": deny
    "git diff *": allow
---
You are a meticulous reviewer…
```

A Claude-style `tools: Read, Grep, Glob` list becomes allow rules with
everything else denied. A model that isn't a routable `provider/model` (e.g.
`sonnet`) is ignored with a warning and the agent inherits its caller's model.

**The `task` tool** delegates to a subagent: the child gets the agent's system
prompt and model, the prompt as its first message, and runs to completion; its
final answer comes back wrapped as `<task id="t1" agent="explore" state="completed">…`.

- Several task calls in one reply run **in parallel** (`LCA_MAX_PARALLEL`).
- `background: true` returns at once; the result is delivered to the caller's
  next step, and a caller that finishes its reply waits for outstanding
  background subagents before ending the turn (Ctrl-C stops waiting).
- `task_id` resumes a previous child with its context intact.
- Subagents can't keep a todo list or delegate further unless their agent
  grants it and `subagent_depth` allows; a parent session's deny rules carry down.
- Subagent progress prints as indented lines (`┌ explore·t1 …`, `│ GREP …`,
  `└ ● completed · 12s`); approvals are serialized and labeled with the agent.
  `/tasks` lists child sessions, `/tasks t1` shows one's report; each child's
  transcript is saved to `transcripts/subagents/`.

**Todo lists.** `todowrite` keeps the agent's plan visible (`/todo`), one item
in progress at a time.

**Skills** (`skills.go`) are `SKILL.md` files (frontmatter `name`,
`description`) under `.lca/skills/<name>/` (also `.claude/skills`,
`.opencode/skill(s)`, `~/.claude/skills`, `~/.lca/skills`). Agents see a
one-line index and load the full instructions, plus a list of bundled files,
with the `skill` tool.

**Commands** are prompt templates in `.lca/commands/<name>.md` (also
`.opencode/command(s)`, `.claude/commands`), run as `/name args`. `$ARGUMENTS`
is the raw argument string; `$1…$N` are positional (the highest takes the
rest); frontmatter `agent:` and `model:` pick who runs it, and a subagent (or
`subtask: true`) runs it in a child session whose report the primary agent relays.

```markdown
---
description: review a file for bugs
agent: reviewer
---
Review $1 for correctness bugs. Focus on: $2
```

**Permissions** (`permission.go`). Rules are `{permission, pattern, action}`
with `allow` / `ask` / `deny`; the last match wins. Keys: `read` (read_file,
list_dir, grep, glob → path), `edit` (edit, write → path), `run` (command
line), `task` (agent name), `skill`, `todo`, `web` (url), `doom_loop` (tool
name). `*` matches anything, and a trailing ` *` also matches the bare
command (`git status *` covers `git status`). Paths are normalized to
jail-relative before matching. Precedence, lowest first: defaults (reads allow;
edit/run/web ask; `.env` files ask) → built-in agent rules → global
`permission` config → the agent definition's rules → session restrictions. A
permission denied outright (`"*": deny`) removes its tools from the agent's
tool list. `ask` goes to the approval gate below, so `/approve` and `-y` still
work; the jail stays the hard boundary underneath.

**Safety nets in the loop.** Three identical consecutive tool calls ask
`doom_loop` before continuing (denied → the turn stops). On the last allowed
step the model is told tools are disabled and to summarize. Editing or
overwriting a file requires the agent to have read it this session, and fails
if the file changed on disk since. Dangling tool calls from an interrupted
session are closed with an "interrupted" result before the next request.

## Terminal style — `ui.go`

Monochrome terminal aesthetic: grays for chrome, color used **only** as status
(muted phosphor green/yellow/red), separation by 1px-style hairlines (`─`), and
UPPERCASE labels. Status glyphs are shared everywhere: `●` ready/ok, `◐` partial,
`✕` failed, `·` muted. Casing is applied only to our own chrome (labels, section
titles, status words) — never to data (paths, model ids, commands, file
contents, diffs), which is always shown verbatim. Hairline width follows
the real terminal column count (a read-only `TIOCGWINSZ` ioctl on unix; no cgo,
no deps), falling back to `$COLUMNS` then 80.

The UI is built from a few shared components (`ui.go`): section rules
(`── TEAM  .lca/roles.yaml ──`), label/value rows, aligned tables, `↳` hints
that say what to do next, and paths shown relative to the project. Every
command's output uses them, so screens read as one system.

On screen the model's prose is shown but the tool-call tags are hidden — plain
action lines (`· read sum.go`, `· search "func main" pkg`, the approval
question) stand in for them, each followed by a one-line outcome (`→ 2 lines`,
`→ 3 matches`, `● edited sample.txt (1 replacement)`). Prose streams character-by-character;
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

Reasoning from "thinking" models is, by default, collapsed into a single live
status line that updates in place — a timer-driven braille spinner with the
elapsed time and a running token estimate (`⠋ thinking · 12s · 340 tok`) — so
the reasoning text never scrolls into the output. When the answer begins the
line is finalized (`thought 12s · 340 tok`). `/think` (or `LCA_SHOW_THINKING`)
expands reasoning to show the full dimmed text instead; `/think last` reprints
the last answer's reasoning even when it was collapsed. Reasoning is
display-only — it is not part of the answer returned to the model.

Type `@path` in a prompt to attach a file: the line editor completes `@` against
files in the jail (prefix matches first, heavy dirs like `.git`/`node_modules`
skipped), and at submit the referenced files are injected as `<file>` blocks, so
the model has them immediately without spending a `read_file` round-trip.
Mentions that don't resolve to a real file inside the jail are left as plain
text.

While typing, a status line under the input shows the current model, working
mode (`approve:…`, plus `loop`/`unsafe` when on), and directory.

While the model is streaming, **Ctrl-C aborts the turn** — a scoped SIGINT
handler cancels the request's context and control returns to the prompt (the
partial reply is kept so the transcript stays clean). It's the escape hatch for
a runaway generation or a `/loop` that won't stop.

## Design (the four non-trivial parts)

Some models don't put tags on their own line (MiniMax-M3 emits
`</mm:think><run_command>` and `cmd</run_command>`). `normalizeTags` drops
reasoning tags (`<think>`, `<mm:think>`, …) and re-separates any tool tag glued
to surrounding text onto its own line — leaving tags that are already alone
untouched, so well-formed `<write>`/`<edit>` bodies keep their exact content.
The parser and the on-screen filter share this so both agree.

1. **Tool-call transport — `protocol.go`, `toolset.go`.** Two transports share one
   tool registry. *Native* (default for hosted presets): tools are sent as JSON
   schemas and calls come back as `tool_calls` (streamed deltas assembled by
   index, arguments double-encoding tolerated, common alias names like `bash` or
   `read` mapped). *Text* (default for the local endpoint, or `LCA_TOOLS=text`):
   the model emits line-anchored XML-ish
   tags in plain text; we parse them ourselves instead of trusting each model's
   native `--tool-call-parser`. Tags written by a model on the native transport
   are still honored. Tags must be alone on a line, so code bodies
   containing `<`, `>` or quotes don't break parsing. No native function calling.
   The parser is deliberately tolerant of dirty output: reasoning tags glued to
   tool tags are re-separated (`normalizeTags`), attribute values may be double-
   or single-quoted with loose spacing (`path = 'x.go'`), and the void tools
   (`read_file`/`grep`/`list_dir`, which have no body) parse even when the model
   forgets the self-closing `/` — the display mirrors all of this so what's hidden
   on screen is exactly what executes.

2. **Applying edits — `edit.go`, `replacers.go`.** `<search>`/`<replace>` (or
   `old_string`/`new_string` natively) is matched exactly first. Only if that
   fails do opencode's tolerant strategies run — line-trimmed, block-anchor
   (Levenshtein ≥ 0.65 on middle lines), whitespace-normalized,
   indentation-flexible, escape-normalized, trimmed-boundary, context-aware —
   and each must land on a single literal location (or `replace_all`). Zero or
   ambiguous matches are reported back so the model regenerates — never a silent
   pick of one of several places; a fallback match is named in the result. CRLF
   files keep CRLF. `<write>` for whole small or new files — it creates missing
   parent directories (jail-checked first). No unified diff (it drifts on
   quantized weights).

When a response is cut off by the generation length limit
(`finish_reason == "length"`) with no usable tool call, the agent continues
automatically (a `auto_continue` audit event) instead of ending the turn and
making you type "continue". And if the model *describes* a change as a diff or
code block instead of emitting an `<edit>`/`<write>` tag (so nothing would be
applied), the agent asks it to redo the change as a real tool call
(`nudge_edit`, bounded so a genuine "show me a diff" answer still ends).
Likewise, if a no-tool reply trails off (ends on a colon/ellipsis) as if it
announced a next step without doing it, the agent nudges it to continue
(`nudge_continue`, bounded). The system prompt also tells the model it runs in
an automatic loop and must emit the next tool call rather than wait.

`/loop` (or `LCA_LOOP`) turns on **autonomous loop mode**: after any tagless
reply the agent keeps prompting the model to take the next action — unbounded by
the heuristic nudge counter — until the model replies `TASK_DONE` or the step cap
is hit. Use it to hand off a whole task and let the agent run it to completion.

3. **Context — `tools.go` + `context.go`.** The repo is never dumped into the
   prompt. The model pulls what it needs via auto-running
   `list_dir`/`grep`/`read_file`. The
   full transcript is kept on disk for audit, but the copy *sent* to the model is
   compressed only when it has to be. **Cache alignment:** while under the token
   budget nothing is rewritten, so each request is a byte-identical extension of
   the last one and the server's KV *prefix cache* hits — near-free prefill on a
   local GPU, where the payoff is latency, not dollars. The budget itself is the
   model's real context window × 0.75 (from `/models`, overridable with
   `LCA_CTX_TOKENS`), so compression only kicks in near the true limit. Once over
   budget, two passes run: superseded reads are deduped (a `read_file` for a path
   read again later is collapsed — the big win in loops that re-read after
   editing), then the oldest remaining `tool_result` outputs are collapsed to a
   stub, while user instructions and the assistant's reasoning are preserved.
   Oversized reads and command output are kept **head+tail** (with an elision
   marker), since errors live at the end. After each streamed step a dim perf
   line reports prompt tokens and the **KV cache hit rate**, completion tokens,
   decode throughput, and time-to-first-token; `/context` shows the live budget,
   cache-alignment state, and the biggest outputs.

   A **project instructions file** in the root (`BEHELIT.md`, `AGENTS.md`,
   `CLAUDE.md`, or `.lca/instructions.md`, first found; or `LCA_INSTRUCTIONS`) is
   appended to the system prompt, so a repo can teach the agent its build/test
   commands, style, and do-nots. For long sessions, `/compact` replaces the
   transcript with an LLM-generated brief (goal, decisions, files changed,
   commands run, open tasks), reclaiming context while keeping the thread of
   work; the system prompt and the `/undo` backups survive it.

   Every turn rewrites the session transcript to `transcripts/<id>.json`, so a
   closed or crashed session can be picked up later: `/resume` reloads the most
   recent one, `/resume list` shows a dated picker (turn count + preview), and
   `/resume <n>` picks one; `-resume` does the same at startup. The current
   system prompt is kept (protocol/project instructions may have changed) and the
   saved conversation appended.

   `/retry` drops the last exchange and re-runs the last user turn — handy after
   a `/model` switch or just to regenerate. `/edit` pulls the last user message
   back into the prompt (pre-filled) to amend and resend, dropping the old
   exchange.

4. **Scope as defense-in-depth — `jail.go` + `approval.go`.** A realpath jail
   (symlink-resolved, prefix-checked) confines every path to the root, and
   `run_command` execs argv directly with **no shell** against an allowlist
   (pipes/redirects are inert). Approval is the soft gate (intent); the jail is
   the hard gate (reach). Both are required.

By default reads, searches, todos, skills and subagents run automatically;
`edit`/`write`/`run_command`/`webfetch` require an explicit `y` at the prompt
(adjustable with permission rules — see Agents & orchestration).

Every applied `edit`/`write` is tracked: the file's prior bytes are snapshotted
in memory so you can review and revert the agent's work. `/diff` shows a colored,
context-collapsed line diff (LCS-based, `diff.go`) of every file changed this
session — first-touch state vs. what's on disk now, with `+`/`-` counts and
`(new)`/`(deleted)` tags. `/undo` reverts the most recent change (restoring the
prior bytes, or deleting a file the agent created); repeat it to walk back
further. Display-only — the apply layer still uses strict verbatim
search/replace, and the model is never shown or asked to produce a diff.

**Unsafe mode** (`/unsafe`, `LCA_UNSAFE`, or `-unsafe`) turns the hard boundary
OFF: paths are no longer confined to the root (any file), the command allowlist
is bypassed and commands run through a shell (any command, pipes/redirects). It
is loudly flagged in the banner and audited (`unsafe_mode`). The approval gate is
independent — combine with `/approve on` (or `-y`) and `/loop` for a fully
autonomous, unrestricted agent. Off by default. A running command streams
its output live behind a dim `│` gutter, its stdin is the null device (so it
can't hang waiting for input), it is bounded by `LCA_CMD_TIMEOUT`, and Ctrl-C
interrupts just that command — not the agent.


### Approval modes — `approval.go`

At each gate the action is shown (a diff for edits, the command line for
runs) with the question `allow?  y yes · n no · a yes to everything this session`:

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

Native tools: `list_dir(path)`, `glob(pattern, path)`, `grep(pattern, path,
include)`, `read_file(path, offset, limit)`, `edit(path, old_string, new_string,
replace_all)`, `write(path, content)`, `run_command(command, timeout)`,
`todowrite(todos)`, `task(description, prompt, agent, task_id, background)`,
`skill(name)`, `webfetch(url, format)`. Text-protocol equivalents:

```
<read_file path="rel/path.go"/>
<read_file path="rel/path.go" lines="40-80"/>
<list_dir path="subdir"/>
<glob pattern="**/*_test.go" path="pkg"/>
<grep pattern="regexp" path="subdir" include="*.go"/>
<skill name="go-style"/>
<webfetch url="https://example.com"/>
<todowrite>
[{"content": "step one", "status": "in_progress"}]
</todowrite>
<task agent="explore" description="find auth">
Where is auth middleware defined? Return paths with line numbers.
</task>
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
main.go       entry point: subcommands (init, doctor, eval), flags, one-shot, usage
repl.go       interactive session: command registry (help, menu, dispatch), banner card, all /commands
repl_cmds.go  custom markdown commands
setup_cmds.go lca init (roles.yaml from the gateway + stack) and lca doctor (end-to-end checks)
hints.go      error → "what to try next" hints, short error text for the screen
format.go     shared formatting helpers (tokens, durations, perf line, outcomes)
procgroup_*.go kill a command's whole process group on timeout
engine.go     Orchestrator + Session: the agent loop, tool dispatch, permissions ask
task.go       task tool: subagent sessions (parallel, background, resume)
agents.go     built-in agents + markdown/config agent loading
skills.go     skills (SKILL.md + skill tool) and command templates
view.go       View interface: terminal primary view, indented subagent view
compaction.go anchored-summary compaction
config.go     env-based configuration
fileconfig.go optional JSON config (providers, agents, ordered permission rules)
providers.go  hosted provider presets, provider/model refs, per-ref clients
models.go     per-family model profiles + thinking-parameter dialects
chat.go       messages, streaming chat with native tool calls, retry, overflow detection
llm.go        local endpoint client: endpoints, /models probing
permission.go permission rules (last match wins, wildcards)
toolset.go    tool registry: schemas, argument decoding/validation, per-agent tool list
builtin_tools.go list_dir, glob, grep, read_file, edit, write, run_command, todowrite, webfetch
glob.go       glob matching, file search, HTML → text
frontmatter.go minimal YAML frontmatter parser
protocol.go   line-anchored tag parser (text tool-call transport)
tools.go      fs helpers, run_command (no-shell exec, live output, stdin=EOF)
edit.go       search/replace apply layer + whole-file write
replacers.go  opencode's tolerant match strategies for edit
jail.go       realpath jail + command allowlist
approval.go   soft approval gate + session approve-all mode
context.go    transcript trimming to a token budget (prefill control)
ui.go         terminal styling: palette, hairlines, status glyphs, labels
lineedit.go   raw-mode line editor: history, cursor keys, /command menu
discover.go   native Slurm discovery (squeue/scontrol/log/startup-script + probe)
stream.go     prose filter: hide tool tags, line-buffer for markdown rendering
markdown.go   terminal markdown renderer (headings, emphasis, code, lists, math)
math.go       LaTeX-ish → Unicode approximation for inline/display math
prompt.go     system prompt assembly (base, tools, delegation, env, skills, project)
roles.go      roles.yaml: role → model chain, effort, prompt, tools, context; gateway validation
gwpolicy.go   gateway failure policy over a role's model chain; session headers
delegate.go   delegate tool: worktree snapshot, verified diff, apply
verify.go     verifier loop: check_cmd decides done
trace.go      JSONL trace: turns and task outcomes
eval.go       lca eval: tasks/ runner scored by the verifier, metrics from the trace
roles_test.go gateway policy, headers, delegate/verifier, compaction, trace, sandbox, eval tests
agent_test.go tests for parser / edit / jail / tokenizer
orchestration_test.go end-to-end loop tests against a fake OpenAI-compatible server
```
