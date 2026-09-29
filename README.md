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
[Agents & orchestration](#agents--orchestration--agentsgo-enginego-taskgo).

It runs as an ordinary process under whoever launched it — no setuid, no service
account, no auth layer. Identity, permissions, tickets and the per-file audit
trail come from the process (uid/gid) for free.

## Contents

- [Quick start](#quick-start) — env vars, commands, endpoints, Slurm discovery
- [Models & providers](#models--providers--providersgo-modelsgo-chatgo) — presets, per-family profiles, the JSON config
- [The team — `roles.yaml`](#the-team--rolesyaml-rolesgo-delegatego-verifygo) — roles, [delegation and the verifier](#delegation-and-the-verifier--delegaterole-task-check_cmd), [cross-family review](#cross-family-review), [`fork:`](#fork-true--inherit-the-reads-not-the-transcript), [tiers](#tiers-develop-on-premium-operate-on-cheap), `/role`
- [Through the gateway](#through-the-gateway--gwpolicygo-chatgo) — the cacheable prefix, [failure policy](#gateway-failure-policy), transports, per-model settings
- [Measuring runs](#measuring-runs--the-trace-lca-eval-lca-report) — the trace, `lca eval`, `lca report`
- [The sandbox](#the-sandbox--jailgo-toolsgo) — allowlist, shell mode, the GPU policy
- [Deterministic workflows — `lca run`](#deterministic-workflows--lca-run-workflowgo)
- [Working on other machines: the fleet](#working-on-other-machines-the-fleet--membersgo-remotego)
- [Agents & orchestration](#agents--orchestration--agentsgo-enginego-taskgo) — agents, [`task` vs. `delegate`](#the-task-tool-vs-delegate), skills, commands, permissions
- [Terminal style](#terminal-style--uigo) · [Markdown & math](#markdown--math-rendering--markdowngo-mathgo) · [Keeping the loop moving](#keeping-the-loop-moving--enginego)
- [Design (the four non-trivial parts)](#design-the-four-non-trivial-parts) · [Approval modes](#approval-modes--approvalgo) · [Audit & transcript](#audit--transcript--recordergo)
- [Protocol reference](#protocol-reference) · [What this does not do](#what-this-does-not-do) · [Layout](#layout)

## Quick start

```sh
go build -o lca .

# a team of open models through the gateway (berserk-gw)
export LCA_BASE_URL=http://node:18080/v1
./lca init          # writes .lca/roles.yaml from the gateway's models and your stack
./lca doctor        # checks gateway, roles, real tool calls, sandbox, members, workflows
./lca               # interactive session — type a task; /help lists the commands
./lca run           # list this project's workflows; lca run <name> executes one

# or a single model: a local endpoint or a hosted API
LCA_BASE_URL=http://localhost:8000/v1 LCA_MODEL=my-model ./lca
DEEPSEEK_API_KEY=… ./lca -model deepseek/deepseek-v4-pro
```

`lca init` writes a **minimal working team** — `entry`, `transport`, `apply`,
`defaults`, `sandbox` and the roles `lead`/`coder`/`cheap`. `tiers:`, `review:`,
`members:` and a `reviewer` role are opt-in;
[`examples/roles.yaml`](examples/roles.yaml) is the same file with all of them
filled in, and each section below names the key to add. `lca doctor` then checks
that file end to end and asks **one real tool call of the first model of every
role** (`-all` probes every model in every chain, `-no-probe` skips the calls
and only checks the configuration); `-tier` and `-member` narrow it.

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
carries the run as it happens (the action lines, the answer, the perf line);
with `-check` the verdict and the failing tail go to stderr, and the **exit
status** is the machine-readable result:

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
| `LCA_DIR`      | `~/.lca`                       | Config and state: `audit.jsonl`, `transcripts/`, `traces/`, `runs/`, `worktrees/`, `roles.yaml`, `workflows/` |
| `LCA_CTX_TOKENS` | auto                         | Approx. token budget for the sent transcript before it's compressed. Auto = 75% of the model's context window (from `/models`), or 24k if unknown |
| `LCA_MAX_TOKENS` | unset                        | `max_tokens` per request (0/unset = let the server decide) |
| `LCA_CMD_TIMEOUT` | `120`                       | `run_command` timeout in seconds |
| `LCA_RAW`      | unset                          | If set, stream raw model text (show tool tags) for protocol debugging |
| `LCA_SHOW_THINKING` | unset                     | Start with reasoning expanded to full text (default: collapsed to the live status line; toggle with `/think`) |
| `LCA_LOOP`     | unset                          | Start in autonomous loop mode (toggle with `/loop`) |
| `LCA_INSTRUCTIONS` | unset                      | Path to a project instructions file (overrides the `BEHELIT.md`/`AGENTS.md`/`CLAUDE.md` search) |
| `LCA_KEEP_SESSIONS` | `200`                     | Max transcript files to retain; older ones are pruned at startup (set it high to keep a long history; 0 and junk fall back to the default) |
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
| `LCA_TIER`     | unset                          | Active model tier: every role that declares `tier:` runs that chain (also `-tier`, `lca run -tier`, `lca doctor -tier`, `lca eval -tier a,b`) |
| `LCA_WORKFLOWS`| unset                          | Extra workflow directories (`:`-separated), searched after `.lca/workflows` and `$LCA_DIR/workflows` |
| `LCA_KEEP_RUNS`| `50`                           | Finished `lca run` directories kept under `$LCA_DIR/runs`; unfinished ones are never pruned |
| `LCA_REMOTE`   | unset                          | Work on another machine: `host:/path/to/project` (overrides `remote:` in roles.yaml, and becomes the team's default member) |
| `LCA_MEMBER_PROBE` | `10`                       | Seconds a member's reachability probe may take |
| `LCA_TRACE`    | `$LCA_DIR/traces/<session>.jsonl` | Trace JSONL path |
| `LCA_GW_MAX_WAIT` | `900`                       | Seconds one model call may spend waiting on an overloaded / not-up gateway |
| `LCA_GW_CUT_RETRIES` | `2`                      | Repeats of a turn whose stream was cut after the first byte (0 = none) |
| `LCA_KEEP_WORKTREES` | unset                    | Keep delegate worktrees for inspection |
| `DEEPSEEK_API_KEY`, `MOONSHOT_API_KEY`, `ZAI_API_KEY`/`ZHIPU_API_KEY`, `DASHSCOPE_API_KEY`, `MINIMAX_API_KEY`, `TENCENT_TOKENHUB_API_KEY`, `HUNYUAN_API_KEY`, `OPENROUTER_API_KEY`, … | unset | Keys for the hosted presets (`/providers` shows which are set) |

Default allowlist: `ls, cat, pwd, head, tail, wc, git, go, gofmt, grep, rg,
find, echo, ssh, scp, rsync, bsk` — reaching another machine opens no port, and
what an `ssh` line carries is checked as well (see the sandbox).

Commands (`/help` prints them grouped; `/` opens a menu as you type):

| group | commands |
| --- | --- |
| session | `/help`, `/resume [list\|n]`, `/compact`, `/reset`, `/exit` |
| agents | `/agent [name]`, `/agents`, `/role [name setting value]`, `/delegate <role> <task>`, `/members`, `/run <name> [k=v …]`, `/tasks [id]`, `/todo`, `/skills` |
| turn | `/retry`, `/edit` |
| review | `/diff`, `/undo`, `/context`, `/stats` |
| modes | `/approve [on\|off\|run\|edit]`, `/think [on\|off\|last]`, `/loop`, `/unsafe` |
| model | `/model [name]`, `/endpoint [n\|url]`, `/providers`, `/discover` |

Commands that don't apply to the current setup (hosted presets and Slurm
discovery when a gateway team is configured, `/skills` with no skills,
`/members` on a single-machine team, `/delegate` where the session's machine
can't make a worktree) are left out of the help and the menu; `/help all` shows
everything. Custom
commands from `.lca/commands/` appear in their own group. A line ending in `\`
continues on the next line.

The prompt is a raw-mode line editor with history, `@file` completion and the
usual control keys, and input typed while the agent works is staged for the next
turn — see [Terminal style](#terminal-style--uigo).

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

A model is addressed as `provider/model`. Built-in presets (all OpenAI-compatible)
— the model column is **examples, not a supported-model list**: the name is passed
through, so a version works the day the provider ships it:

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
goes to the local endpoint unchanged. `/model deepseek/deepseek-v4-pro` switches
at runtime; `/providers` lists presets and key status.

Each model **family** has a built-in profile (`models.go`) mirroring the vendors'
published recommendations: context window and max output (for the context budget
and `max_tokens`, capped at 32k), the sampling values `models.go` carries per
family (`/model` prints what a session actually sends), and how reasoning is
handled:

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

Failures are handled per model call by the [gateway failure
policy](#gateway-failure-policy): `Retry-After` is honoured, waiting is capped by
`LCA_GW_MAX_WAIT` (900s), and a 5xx before the first byte gets exactly one
attempt on the next model of the chain. A context-overflow error (recognized
across vendors' phrasings) triggers compaction and one retry. DeepSeek's
`prompt_cache_hit_tokens` counts toward the cache-hit figure in the perf line.

### Config file — `fileconfig.go`

`roles.yaml` describes a team behind **one** gateway; `config.json` is for
everything that is not that — hosted providers with their own keys, markdown
agents, permission rules — which is why there are two files. Optional JSON, read
from `~/.lca/config.json`, then `<root>/.lca/config.json`, then `$LCA_CONFIG`
(later wins per key):

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

## The team — `roles.yaml`, `roles.go`, `delegate.go`, `verify.go`

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

When the file is silent, `apply:` is `verified`, `defaults.verify_attempts` is 2
and `defaults.check_timeout` is 600 seconds; the numbers above are only an
example.

A role is an agent bound to an **ordered model chain**, a reasoning effort, a
prompt, an **exact tool set** and a **context limit** (the budget for trimming
and compaction). Models are gateway names only — an address is rejected — and
are checked against `/v1/models` at startup (unlisted ones are dropped from the
chain with a warning). `-role <name>` or `entry:` picks the session's role;
`/agents` shows each role's tier, chain, effort, context and — on a fleet — its
machine.

### Delegation and the verifier — `delegate(role, task, check_cmd)`

A delegation is deliberately **blind** and deliberately **judged**. Blind: the
subagent gets a self-contained prompt and its own git worktree, never the
caller's transcript — handing one model's reasoning down to another would ship it
as fact — and only `{"status", "diff", "test_tail"}` (plus `review`, below) comes
back. Judged: the verifier runs `check_cmd` in that worktree, and the model's own
"done" never counts. (`delegate` is not `task`; the two are contrasted
[where `task` is introduced](#the-task-tool-vs-delegate).)

Then the mechanics. The subagent is the same loop running another role. Its
worktree is a snapshot of the caller's working tree — uncommitted and new files
included — made through a temporary index, so the user's index and HEAD are never
touched. When the subagent stops, the verifier runs `check_cmd` there (the task's,
else the role's default) in the sandbox. Exit 0 is `passed`; otherwise the failing
output goes back to the subagent (up to `verify_attempts`), then `failed`; with no
check at all the status is `unverified`. With `apply: verified` a passed diff is
applied to the caller's tree (`git apply`, through the `edit` approval); one that
no longer applies is `conflict`. Several delegations in one reply run in parallel,
each in its own worktree. Inside a worktree edits and sandboxed commands run
without prompts; `bsk` still asks.

`-check` gives a one-shot run the same contract:
`lca -role coder -check "go test ./..." "fix the flaky parser test"` exits 0
only if the check passes.

### Cross-family review

A test suite proves a change did what its test says;
it does not notice a change that is wrong in a way nobody wrote a test for, and
the model that wrote the diff is the worst possible judge of that. So a role can
name a second opinion from another model family:

```yaml
defaults:
  review: reviewer           # team-wide: every role that names none
roles:
  coder:
    models: [qwen3-coder-480b-a35b-instruct]
    check_cmd: go test ./...
    review: reviewer         # this one's second opinion, whatever defaults says
  reviewer:
    models: [glm-5.2, kimi-k2.6]              # another family, on purpose
    tools: [list_dir, glob, grep, read_file, run_command]
```

It runs only after the verifier passed **and** the diff is non-empty — a failed
check is the verifier's business and an unchanged tree has nothing to judge —
inside the worktree, as its own session (its own id under the same root, its own
chain, its own trace turns), so the reviewed role's cached prefix is untouched.
The reviewer is read-only by construction: `edit` and `delegate` are denied for
that session, because a reviewer that can edit is grading its own patch. It must
end with a line `VERDICT: approve` or `VERDICT: reject`.

The verdict line has to *start* a line (leading `*`/`_` emphasis is tolerated,
inflections like "rejected" count), lines inside fenced blocks are ignored, and
the last one outside a fence wins — so a `// VERDICT: approve` planted in a diff
hunk, or a reviewer quoting the instruction before concluding, decides nothing.
A reject makes the status `rejected` and **blocks the apply**; `apply: always`
cannot override it, because `rejected` is not one of the statuses it applies. No
parsable verdict is `unreviewed` and **blocks nothing** — the reviewer is asked
exactly once more, and a second opinion that cannot be reached must never turn a
passing change into a failure. The verifier stays the arbiter of correctness.

`review: false` on a `delegate` call and `review: off` on a workflow step skip
it; `review: none` on a role opts out of a team default; `review: <role>` on a
workflow step overrides both. The loader refuses a reviewer with no chain of its
own — a second opinion must never quietly be the reviewed subagent's own model —
and warns when its first model shares the reviewed role's family (that is the
blind spot the review exists to find) or when it is pinned to another member,
since it runs where the worktree is. `reviewer`, `review_model` and
`review_verdict` land on the task's trace record; `/role coder review reviewer`
sets it live.

### `fork: true` — inherit the reads, not the transcript

A delegation starts blank on purpose (above). But when the caller has just read
eight files to write the task, the child re-reading all eight is pure prefill:

```yaml
roles:
  coder:
    fork: true                 # or fork=true on one delegate call
```

The child then inherits the caller's **file reads** — the latest read of each
path (an earlier read is superseded waste), rendered in the text transport's own
`<tool_result …>` spelling so a native and a text child both accept it, and
bounded to half the child's context budget so it arrives with room to work. It
is reads only: no reasoning, no command output, no other tool results. A path
the child's own permission rules would not outright allow (`.env` under the
defaults) is not inherited, nor is one that does not resolve inside its jail, nor
one whose bytes contain the framing itself — a file cannot forge a read of
another file. The child is told what it got (`FORK  6 files inherited from the
caller (~14k tokens, 2 dropped for context)`) and the audit log records
`delegate_fork`. Across machines it is **skipped**, with a note: the caller's
bytes are one machine's truth, and seeding them as the child's would let it edit
a file it has never read on the machine it works on.

### Tiers: develop on premium, operate on cheap

A chain per role is the right unit when the models differ in *kind*; it is the
wrong unit when they differ in *price*, because then every role has to be edited
together. `tiers:` names chains, and a role points at one:

```yaml
tiers:
  premium: [kimi-k2.6, glm-5.2]
  cheap:   [qwen3-30b-a3b-instruct]
roles:
  lead:
    tier: premium            # follows -tier
  coder:
    models: [qwen3-coder-480b-a35b-instruct]   # pinned: not a price choice
```

`-tier cheap` (also `LCA_TIER`, `lca run … -tier cheap`, `lca doctor -tier`,
and `lca eval -tier cheap,premium` for the whole matrix side by side) points
**every role that declares a `tier:`** at that chain for the run. A role that
names `models:` is pinned and no `-tier` moves it — that is how "the implementer
stays strong while the orchestration is rehearsed cheaply" is expressed. A role
carrying both keys in one file is an error, and across files the later one wins
whichever of the two it is, so a project can repoint a role the global team
declared. A
`-tier` that moved no role at all **says so** and names the pinned roles, because
a run that looks like it switched and didn't is worse than one that refused; an
undefined tier refuses to start and lists the ones there are. A tier is a chain,
not a profile: whatever it resolves to still picks up that model's own `models:`
settings. The active tier is on every trace record, in `/agents`, in `lca doctor`
and in a run's `state.json`. `/role <name> tier <t>` switches one live.

### Changing the team live

`roles.yaml` is not the only way in: `/role` lists the team, `/role <name>` shows
one, and `/role <name> <setting> <value>` changes it live —

```
/role                                   # the team: models, effort, context, tools, check
/role coder model <fast-coder>,<fallback>  # chain; names not served by the gateway are flagged
/role coder effort high                 # off | on | low | medium | high | max
/role coder temperature 0.6             # also top_p, context, steps
/role coder check go test ./...         # what the verifier will run
/role coder tier cheap                  # a chain from tiers:
/role coder review reviewer             # a second opinion on a passed diff (none = off)
/role coder fork true                   # start its subagents from the caller's reads
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

## Through the gateway — `gwpolicy.go`, `chat.go`

### A prefix the gateway can cache

The gateway's KV cache pays for a prompt that does not change. So: the system
prompt and the tool schemas are computed once per session and **never mention the
model**, so a fallback, a trim or a compaction cannot move them; every request
carries `x-session-id` (this session — the gateway's cache-affinity key) and
`x-root-session-id` (the whole task tree), with subagents and the compaction
helper on their own ids under the same root; and compaction runs on the `cheap`
role as a separate request that rewrites only the conversation **after** the
prefix.

### Gateway failure policy

Per model call:

| gateway says | recognized by | client does |
| --- | --- | --- |
| not up | 503 + `X-Berserk-State: paused\|drained`, `cold start failed`, `no (healthy) backend`, 404 `unknown model` | next model of the role (sticky for the session); none left → wait `Retry-After` and walk the chain again |
| overloaded | 503 + `X-Berserk-Overload`, 429, bare 503 | wait `Retry-After` on the same model; **no fallback** |
| failed before the first byte | connection refused/reset, 500/502/504 | exactly one attempt on the next model |
| cut after the first byte | stream ends without a finish reason | repeat the turn on the same model, up to `LCA_GW_CUT_RETRIES` (2) |

Waiting is capped by `LCA_GW_MAX_WAIT`. Context overflow and other 4xx go
straight to the loop.

Why the first two rows disagree about falling back: a model that is *not up* will
stay not up for at least a cold start, so the chain is walked; a model that is
*overloaded* is still the right model under load, and falling back would dump that
load — and a cold KV cache — on a model this role was never tuned for.

Why a cut turn is simply repeated: berserk-gw doesn't migrate requests that
carry `tools` (a second replica can't resume a tool call mid-JSON), so the
client has to cover it. A turn is idempotent — tool calls execute only once
they have arrived whole, so a cut turn ran nothing — and the prefix is in the
KV cache, so a repeat costs only the decode. The partial output on screen is
marked void.

### Tool transport

`native` (the default) sends `tools` and uses the model's
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

### Per-model settings from the model card

Vendor recommendations differ per model and are not guessable, so they are
configured rather than invented (the names here are placeholders — use the
gateway's own):

```yaml
models:
  a-thinking-model: {temperature: 1.0, top_p: 0.95, effort: max}
  a-fast-model:     {temperature: 0.3, top_p: 0.95}
  a-picky-model:    {reasoning_replay: off}   # server rejects reasoning_content in history
  some-model:       {transport: text}
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

## Measuring runs — the trace, `lca eval`, `lca report`

### Trace — `trace.go`

One JSONL line per model turn — role, model, transport, active tier, the member
its tools ran on, `usage{prompt_tokens, completion_tokens, cached_tokens}`, TTFT,
duration, finish reason, gateway fallbacks and repeats, every tool call (args,
ok/error, invalid, bytes, ms) and the count of invalid calls (bad JSON, unknown
tool, schema violation, unparsable text tag) — one per verified task (role,
status, check exit, attempts, diff size, files, applied, the reviewer and its
verdict, the member the worktree was on and, when a diff crossed machines, the
member it was applied to) — and one per workflow step. Written to
`$LCA_DIR/traces/<session>.jsonl` (`LCA_TRACE`).

A call that didn't parse also keeps the model's own text: `raw` holds the
arguments as they were emitted and `raw_reply` the reply they came in, because
a parse failure is otherwise unreproducible. In a session, `/stats` shows the
running totals — turns, tokens in/out, the share of the prompt the gateway
served from its KV cache, tool calls and **the share that didn't parse**, tool
errors, gateway fallbacks and verifier runs. Above a fraction of a percent of
unparsed calls, suspect the engine's tool-call parser (or the transport), not
the model.

### Eval — `lca eval`

Built on the trace:

```sh
lca eval [-out dir] [-role r] [-run regexp] [-transport native,text] [-tier cheap,premium] [-keep] [-v] tasks/
```

`-transport native,text` runs every task once per transport (forced for all
roles) and prints them side by side — pass rate, invalid-call rate, turns,
tokens, cache ratio — so "native vs text" is settled with data from your own
fleet. `-tier cheap,premium` is the same matrix over `tiers:`, which is how
"is the cheap chain good enough for this pipeline" gets an answer instead of an
opinion; the tier names are checked against the team before the first task
starts, and the two flags multiply.

A task is `tasks/<name>.yaml` or `tasks/<name>/task.yaml` with `prompt`,
`check_cmd`, and optionally `role`, `repo` (a fixture directory copied into a
fresh git repo; omitted = a worktree of the current repo's HEAD), `setup`,
`timeout`, `verify_attempts` — see
[`examples/tasks/fix-sum`](examples/tasks/fix-sum). Each task runs unattended in
its own workspace; the verifier's result is the score, and turns, tokens,
cache ratio, tool calls/errors, fallbacks and delegations are aggregated from
the task's trace into `results.jsonl`. Exit status is 0 only if every task passed.

### Report — `lca report`

Also built on the trace:

```sh
lca report [<trace.jsonl>|<run-id>|<session-id>] [-out file.html] [-open]
```

One self-contained HTML file, so an hour-long autonomous run can be read after
the fact with no dashboard, no daemon and no network: the page carries its own
CSS, draws its chart as inline SVG, and opens from `file://`. With no argument
it takes the newest trace in `$LCA_DIR/traces`; with no `-out` it writes next to
the trace and prints the path (a run id resolves to *every* trace the run wrote,
because a resumed run writes one per process).

The page shows a summary header (wall clock, tokens in/out, cache hit share,
invalid-call share, tasks passed/failed, fallbacks, unreadable lines), a
per-session activity strip, the session tree (root, subagents, delegations and
the compaction helper) with each turn's role, model, member, tier, transport,
tokens, cached share, TTFT and duration, every tool call with status and timing,
each invalid call with the text the model actually emitted, gateway waits /
switches / repeated turns, verified tasks with check exit and attempts, and any
workflow run with its steps. A trace still
being appended to (the usual case — you run this on a run that is still going) is
read as the prefix it had when the report started, so the chart and the header
cannot end before the rows the page lists.

It never guesses. A field the trace does not carry renders as an **em dash** —
including cost, which nothing in this binary records, so that tile reads `—`
unless some record carried `cost_usd` from elsewhere. Everything that came from a
model, a path or a command line is escaped once on the way in, because a tool
argument is attacker-chosen text in an HTML page. And corrupt or oversized lines
are counted on the page rather than dropped, as are the caps on a long detail
list, so a page missing rows says which and why.

## The sandbox — `jail.go`, `tools.go`

Commands (agents' and verifiers') run without a shell against the
allowlist (`sandbox.allow` in roles.yaml, else `LCA_ALLOW`), so `|`, `>` and
`&&` reach the program as plain arguments. With

```yaml
sandbox:
  allow: [go, git, grep, wc, head, tail, python3]
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

## Deterministic workflows — `lca run`, `workflow.go`

Explore with agents, operate with programs. A model is worth its tokens while
the shape of the work is still unknown; once a pipeline is known — test, plan,
build, format, review, commit — asking a model to remember the order is paying
for judgement you already have, and getting a different pipeline each time.
`lca run` is that pipeline as a file: shell and git steps cost zero tokens, a
model is invoked only at the steps where judgement is actually bought, and the
whole thing is resumable because state is written after every step.

Nothing is installed by `lca init`: a workflow is a file you put in the project.

```sh
mkdir -p .lca/workflows
cp examples/workflows/greenlight.yaml .lca/workflows/   # zero tokens: build, vet, test

lca run                          # the workflows found, with their step counts
lca run greenlight -dry-run      # validate, resolve the team, print the plan
lca run greenlight               # runs on the team `lca init` wrote

lca run -list                    # recent runs, their status and where they stopped
lca run greenlight -resume       # continue its newest unfinished run, if one stopped
lca run <runid> -pause           # stop at the next step boundary (ids come from -list)
```

`lca doctor` binds every workflow on the search path against the team, so run it
after you add one: a role that does not exist or a misspelt `${steps…}` reference
is found by the command you already run, not by the first real run.

The other shipped example, [`harden.yaml`](examples/workflows/harden.yaml), asks
for more than `lca init` writes. A **`reviewer` role**: its fifth step runs as
that role, so doctor and every run refuse the file until one exists, and it needs
`write` in its tools to leave the written review behind. And `review: reviewer` on
`coder`: without it the build step is still delegated and verified, just never
cross-reviewed. Take the `reviewer` block from
[`examples/roles.yaml`](examples/roles.yaml), or `/role new reviewer model <name>`
then `/role save`. It also declares a **required var**, so the flag belongs on the
dry run too:

```sh
lca run harden -dry-run -var task="make Parse reject a trailing comma"
lca run harden -var task="make Parse reject a trailing comma"
```

Workflows are `<name>.yaml` in `.lca/workflows` (the project), then
`$LCA_DIR/workflows` (yours), then every directory in `LCA_WORKFLOWS`. **First
match wins** — the reverse of `roles.yaml`, which merges per field: a workflow is
a whole program, and the repo's copy should beat a personal one of the same name.
`/run <name> [k=v …]` runs one mid-conversation on the same orchestrator and
trace, and hands the lead model a digest of what the program did.

**The file.** Six top-level keys, and `steps:` is a **mapping keyed by step
name**, not a list — file order is execution order, and the names are what the
placeholders refer to:

| key | default | meaning |
| --- | --- | --- |
| `name` | the file's basename | the workflow's name |
| `description` | — | one line, shown by `lca run` and `-dry-run` |
| `role` | `entry:` from roles.yaml | default role for `prompt`/`delegate` steps (`-role` overrides it) |
| `timeout` | none | default per-step timeout, in seconds |
| `vars` | — | `k: v` defaults; a key with no value is **required** and must be passed with `-var k=v` |
| `steps` | required | the ordered mapping of steps |

A step is exactly one action plus what verifies it and what makes it run at all:

| key | default | meaning |
| --- | --- | --- |
| `run` | — | one command line, in the sandbox, **zero tokens** |
| `prompt` | — | one turn of a role in a fresh child session |
| `delegate` | — | a subagent in its own git worktree, judged by the verifier |
| `role` | the file's `role:` | the role for a `prompt`/`delegate` step; refused on a `run` step |
| `check` | — | a command that must exit 0 for the step to be `ok` |
| `retries` | `0` | extra attempts of the whole step (so `retries: 1` = 2 attempts) |
| `timeout` | the file's `timeout:` | seconds; a `run` step with neither falls back to `defaults.check_timeout` |
| `on_fail` | `stop` | `stop` ends the run at this step; `continue` carries on |
| `when` | always runs | a condition; a step with no `when` **runs** |
| `member` | the role's machine (the lead's, on a `run` step) | which machine this step runs on |
| `review` | the role's own | `off`, `on`, or a role name — `delegate` steps only |
| `fork` | the role's own | `true`/`false` — `delegate` steps only |

Anything else is a load error naming the keys that exist, as is a duplicate step
name, a step with two actions or none, `role:` on a `run` step, and `review:` or
`fork:` on a step that is not a delegation. An unquoted ` #` starts a comment
anywhere in a value, so a value that contains one must be quoted:
`run: "git commit -m fix #42"`.

**The three step kinds.**

- `run:` is one command line in the sandbox — the same allowlist, GPU policy and
  deny rules as everything else, on a member's machine too. It is **not**
  approved: it was authored in a file, not chosen by a model, so it is a harness
  command exactly like a `check_cmd` (`-ask` turns the prompts back on). Without
  `sandbox: {shell: true}` the line is **one argv**, so `a && b` would hand `&&`
  to `a` as an argument — `bind` refuses that at load and tells you to use two
  steps. The step is `ok` only when the action **and** its `check` exited 0:
  letting the check override the action would pass `run: go build ./...` while
  the build is broken.
- `prompt:` is one turn of a role in a **fresh child session** — not a reused
  one, so step 5 cannot see step 2's conversation, context cannot grow without
  bound over a long pipeline, and no compaction fires mid-run. Its `check` is the
  verifier's, fed back on a retry (a `timeout:` here bounds the turn and its
  retries together, since the retry loop is the verifier's). With no check the
  step is `ok`: a prompt step is a text-producing node, and the step that acts on
  its output is where a check belongs.
- `delegate:` is the `delegate` tool, unchanged: its own git worktree, the
  verifier, the cross-family review, `apply: verified`, the live subagent tree,
  its own `TaskRecord` in the trace. Only `passed` applied a diff, so only
  `passed` moves the pipeline forward. It needs a `check:` or a role with
  `check_cmd` — without one the result is `unverified` and the diff would never
  be applied — and its role must differ from the lead's. `retries` wrap the whole
  tool while `verify_attempts` works inside it, so the two budgets **multiply**;
  `-dry-run` prints the effective number (`2 × 2 = 4`).

**Placeholders.** `${vars.<k>}` and `${steps.<name>.<field>}`, where field is
`out`, `status` or `exit` (`diff` and `review` on a delegate step as well).
Everything is checked **at load**: a malformed `${…}`, an unknown field, a
reference to a step that does not exist or does not run earlier — all of them
refuse the file before a single token is spent. `out` is the last 60 lines /
4 KB of what the step produced (a command's output, a prompt step's final
answer, a delegation's check tail); `diff` is the full patch, read from the run
directory rather than from `state.json`, because handing a reviewer a truncated
diff is worse than handing it none.

In a `run:` or `check:` line a `${steps…}` value is **shell-quoted** for you —
it is model or command output, i.e. untrusted — and a `${vars.k}` is not, because
it came from the file or your command line and a var like `-count=1 ./...` must
stay several words. Hence the one rule worth memorising: **a `${steps…}`
placeholder may not sit inside quotes.** `run: echo '${steps.plan.out}'` is
refused at load, because your quotes would let the output close them and start a
second command. Write it bare: `run: echo ${steps.plan.out}`.

**`when:`** is an OR of ANDs of string comparisons — `always`, `A == B`,
`A != B`, joined with `&&` and `||`. It is deliberately not an expression
language, and it is parsed at load rather than substituted at run time: a step's
output containing `&&` must not be able to rewrite the condition that gates the
next step. A bare dotted word (`step.build.status`) is rejected as a misspelt
reference instead of quietly comparing two literals and skipping the step on
every run. There is no dependency graph: a later step can only observe an earlier
failure because that step said `on_fail: continue`, and then the author must
guard it.

```yaml
integrate:
  run: git commit -am "verified by the harden workflow"
  when: ${steps.build.status} == ok
```

A rejected delegation already failed its step, so `status` is usually the whole
guard; `${steps.<name>.review}` (`approve`, `reject`, `unreviewed`, or empty when
nothing reviewed it) is there for telling "a reviewer approved this" from "nobody
looked at it".

**State, log, lock, pause.** Every run gets `$LCA_DIR/runs/<workflow>-<session>/`:

- `state.json` — rewritten after **every** step, so a crash, a Ctrl-C or a pause
  leaves a resumable run and never a half-step: the cursor (the next step to
  run), the vars, the lead role, the active tier, each step's status, exit,
  attempts, `out`, timings and token usage, the member each step bound to, and
  every session and trace file that has worked on the run.
- `run.log` — the raw combined output of every step as it is produced,
  ANSI-stripped, with a header per step and per attempt. A run that crashed
  mid-check is still diagnosable. Model steps tee their reply into it too;
  reasoning stays out.
- `steps/<name>.diff` — a delegation's patch, for `${steps.<name>.diff}`.
- `lock` — this process's pid. A second `lca run` on the same run is refused
  (both would execute every remaining step and each would overwrite the other's
  state); a lock whose pid is gone is a crash, not a conflict, and is taken over.
- `pause` — written by `lca run <runid> -pause`. Cooperative on purpose: no
  signal is sent to a run that may be mid-test. The runner stops at the next step
  **boundary** and a resume is what clears it.

`-resume` continues the newest unfinished run of a workflow (or a named run id),
**re-loading the very file the run started from** and re-running the step that
failed. It refuses to continue against different work: a changed file (sha256), a
run belonging to another project directory, a `-var` that contradicts or adds to
the ones the run started with, another lead role, another tier, or a step whose
member moved under it. Each of those would report one green result over two
different teams, trees or machines. Since the failed step is re-run, **a step
with side effects — `git commit`, a migration — belongs last.** Finished runs are
pruned to `LCA_KEEP_RUNS` (50); an unfinished one is someone's resumable work and
is never deleted.

Ctrl-C has three stages: the first stops at the next step boundary (fully
resumable), the second cancels the step in flight — otherwise a long delegation
could only be escaped with a SIGKILL, which loses the state write — and the third
hands the signal back to the runtime.

**Exit status.** `0` — every step `ok` or `skipped`, a paused run included
(nothing failed; the summary and `-list` say where it stopped); `1` — a step
failed (that includes a step whose `on_fail: continue` let the run finish) or the
run was interrupted; `2` — nothing ran: bad flags, no workflow by that name, a
file that does not parse, a reference or role or member that does not resolve, a
command the sandbox refuses, an unreachable member, a lock held by a live
process, or a resume that was refused. Everything a workflow can be wrong about
is meant to be found by `-dry-run`, which resolves the team, the fleet and the
sandbox and prints the
plan — step, kind, role → reviewer, member, attempts, timeout, whether it is
conditional, and its check — without running anything.

Both shipped examples carry comments on why each step is where it is:
[`greenlight.yaml`](examples/workflows/greenlight.yaml) (zero tokens, three
checks and a triage) and [`harden.yaml`](examples/workflows/harden.yaml) (plan,
delegate, review, commit).

## Working on other machines: the fleet — `members.go`, `remote.go`

The code, tests and GPUs usually aren't on your laptop, and they are usually not
all on the *same* other machine either. A **member** is one machine the team
works on: a name, the directory the project lives in there, and the sandbox that
governs what runs there. The unit of execution is a member, not a session, so one
team can span a laptop, a build box and a GPU node. Nothing needs an inbound
port: `ssh` is an ordinary outbound connection using the keys and
`~/.ssh/config` you already have.

**Work meant for a member never runs anywhere else.** Reachability is a gate, not
a fallback: it is checked inside the code that runs the command, it has no
else-branch, and a failure fails the caller — silently running on the wrong
machine is the worst thing this feature could do, so it is made structurally
impossible rather than merely avoided. Success is cached until the transport
itself fails, a failure for 30s (a laptop that joins the VPN mid-session recovers
without a restart), and the error says *which* of the two things failed — ssh, or
the directory — because the fixes have nothing in common.

```yaml
members:
  local:                          # optional, only to scope THIS machine's sandbox
    allow: [go, git, make]
  build-box:
    host: build01                 # ssh target (a ~/.ssh/config alias is fine)
    dir: /srv/work/llmbench       # the project there — absolute, no ~
    ssh: [-o, ConnectTimeout=5]   # extra ssh flags (optional)
    allow: [go, gofmt, git, make] # optional: REPLACES the team's sandbox.allow here
    shell: true                   # optional: this member's shell mode
  gpu-0:
    host: gpu07
    dir: /scratch/llmbench
    allow: [ls, cat, python3, bsk]

defaults:
  member: build-box               # the member roles use when they name none

roles:
  coder:
    member: build-box             # this role's sessions work there
```

`lca init -member build-box=build01:/srv/work/llmbench -member gpu-0=gpu07:/scratch/llmbench`
writes the `members:` block — hosts and directories, not the per-member `allow:`
or `shell:` above. With a single `-member` it also writes `defaults.member`, so
that machine is where the team works; with several it cannot guess which role
belongs where, so **the fleet stays inert until you add `member:` to a role** or
`defaults.member` to the team (`lca doctor` shows each role's machine, so an
inert fleet is visible as `lead local / coder local`). `member:` on a role pins
it; `member:` on a workflow step wins over the role's (a `run` step has no role
but it does have a machine); everything else falls back to `defaults.member`,
then this machine. `/members` lists the
fleet — where each one is, whose sandbox it uses, which roles live there, and
whether it answers right now — and `lca doctor` probes every member: ssh, that
the directory exists, that `git` is installed, that the directory is inside a
repository, that the worktree base is writable, and that **that member's**
allowlisted commands exist there, in one connection per machine. It also refuses
a role whose `check_cmd` that member's allowlist would not run — a verifier the
sandbox rejects fails every delegation to that role, and the message would be
about the sandbox rather than about the change. `lca doctor -member <name>`
narrows it to one machine.

On a member the file tools read and write over ssh, `run_command` and the
verifier execute there, paths stay inside `dir` (absolute paths and `..` are
refused, every path is shell-quoted), and the read-before-edit guard uses remote
mtimes. Two things about the sandbox on a member, neither of them obvious:

- **A member's `allow:` replaces the team's, it does not intersect it.**
  Intersecting would turn a GPU node's `[python3, bsk]` into `[]` on most teams.
  A member that sets neither `allow:` nor `shell:` uses the team's sandbox
  unchanged.
- **A remote command line is checked with shell semantics**, because a shell on
  the far side really will act on it: every segment of `a | b && c` is checked
  against that member's allowlist, quoting respected — not just the first word,
  and never the `ssh` argv we build around it. So `ssh node srun …` is refused
  with the same message a local `srun` gets, and `bsk gw` stays off-limits.

**Delegating across machines.** A delegation to a role on a member gets its
worktree **on that member**: the same sequence as the local one (a temporary
index so the operator's index and HEAD there are untouched, `add -A` so
uncommitted and new files are in the snapshot, no hooks, the worktree under that
machine's own `$LCA_DIR/worktrees` and never inside the project) in one ssh round
trip. The diff comes back as a repository-relative patch and is applied to the
caller's tree, wherever that is — a build box's diff lands on the laptop.

What it deliberately is **not**: a snapshot of *your* tree. Shipping the caller's
dirty tree over ssh would need a shared ancestor (`git bundle`) or copy the
project behind your back (`rsync`), so the base is the **member's own working
tree**, and the subagent says so out loud when it starts rather than papering
over it. Which means the honest limitations:

- The two checkouts share no object store and the patch is text, so there is **no
  3-way merge**: an apply that fails names both checkouts and their commits and
  tells you to line them up or run the role on your own machine.
- The project must sit at the **same path inside its repository** on both
  machines — diff paths are repository-relative. A mismatch is refused up front,
  with the two layouts named, instead of surfacing later as a mysterious conflict.
- `fork: true` is **skipped** across machines (the note says so): inherited
  reads are one machine's bytes.
- A diff of 8 MiB or more is refused rather than truncated — a cut patch cannot
  apply, and a short patch that *does* apply is worse than one that does not.
- Two `lca` processes delegating onto one member can still race git's worktree
  bookkeeping there: worktree creation is serialised inside one process, and the
  lock is per process.

**The old single-machine spelling still works.** `remote: {host, dir, ssh}` (or
`LCA_REMOTE=cab-node:/home/u/llmbench`, or `lca init -remote cab-node:/path`)
means one member named `remote` that every role which names no other one uses.
On that spelling `delegate` is **withheld** — its worktree would be local while
the project is not — so subagents share the remote tree through `task`, or you
move the machine into `members:`, which is a one-line migration and turns
cross-machine worktrees on. `/role save` writes a `remote:` block back as a
`remote:` block; it does not silently rewrite your file into the new form.

**Or don't move the project at all.** Run lca *on* that machine — the binary is
static, needs no glibc and no dependencies, so the agent sits next to the code,
the gateway and `bsk`. Or keep everything local and send only GPU work to the
cluster with `bsk submit` (see the sandbox section): `ssh`, `scp` and `rsync` are
on the default allowlist, so an agent can reach another host by itself when a
task needs it.

## Agents & orchestration — `agents.go`, `engine.go`, `task.go`

The loop is a reusable engine: an **Orchestrator** holds everything shared
(jail, approval gate, audit log, providers, agents, skills, commands), and a
**Session** is one conversation driven by one agent on one model. One process,
many conversations — so the state that must not be duplicated lives in the
Orchestrator, and a subagent cannot end up with a laxer jail, gate or audit trail
than its parent. The REPL owns
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

### The `task` tool vs. `delegate`

`task(agent, …)` asks a helper a question **in the caller's own tree** and returns
its answer — use it to search, read and summarize. `delegate(role, …)` hands a
**change** to a role in its own worktree and returns a verified diff, not prose —
use it when something has to be written and proven. A `task` is never verified; a
[`delegate`](#delegation-and-the-verifier--delegaterole-task-check_cmd) is never
unverified.

The `task` tool: the child gets the agent's system prompt and model, the prompt as
its first message, and runs to completion; its final answer comes back wrapped as
`<task id="t1" agent="explore" state="completed">…`.

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
line), `task` (agent name), `delegate` (role name), `skill`, `todo`, `web` (url),
`doom_loop` (tool name). `*` matches anything, and a trailing ` *` also matches the bare
command (`git status *` covers `git status`). Paths are normalized to
jail-relative before matching. Precedence, lowest first: defaults (reads allow;
edit/run/web ask; `.env` files ask) → built-in agent rules → global
`permission` config → the agent definition's rules → session restrictions. Last
match wins rather than most-specific, because the layers are ordered by who is
closer to the action: a restriction the session imposed (a reviewer's
`delegate: deny`) has to be able to overrule a rule the agent file shipped with. A
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
as you scroll up. An interactive session clears the **visible** screen at startup
so it begins at the top, but stays on the normal screen and leaves the scrollback
intact, so you can scroll up past the banner into earlier output. Set
`LCA_NO_CLEAR` to skip the clear.

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
a legible approximation, not true typesetting — for that, read the run in
`lca report`'s HTML. Rendering is line-buffered (a line is formatted once complete);
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

## Keeping the loop moving — `engine.go`

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

## Design (the four non-trivial parts)

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
   tool tags are re-separated (`normalizeTags` — MiniMax-M3 emits
   `</mm:think><run_command>` — while tags already alone on their line keep their
   exact body), attribute values may be double-
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

3. **Context — `tools.go` + `context.go`.** The repo is never dumped into the
   prompt. The model pulls what it needs via auto-running
   `list_dir`/`grep`/`read_file`. The
   full transcript is kept on disk for audit, but the copy *sent* to the model is
   compressed only when it has to be. **Cache alignment:** while under the token
   budget nothing is rewritten, so each request is a byte-identical extension of
   the last one and the server's KV *prefix cache* hits — which is what the
   [cacheable prefix](#a-prefix-the-gateway-can-cache) is for. The budget itself is
   the model's real context window × 0.75 (from `/models`, overridable with
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

## What this does not do

Each feature section above states its own limits where you are standing — what a
resume re-runs, what `unreviewed` does not block, why there is no 3-way merge
across machines, what a remote worktree is a snapshot of. What is left over,
because it belongs to no one section — stated as what happens, never as what
someone intends to build:

- **No per-member job reservation.** Two roles pinned to one GPU member run there
  at the same time; nothing queues or reserves the machine. The only bound is per
  `lca` process (`LCA_MAX_PARALLEL`, and one slot per top-level delegation), so two
  operators — or two workflows — can oversubscribe a node. Scheduling GPU work
  through `bsk submit` is what stands in for it.
- **No fan-out and no scheduling.** Workflow steps run one at a time in file
  order: no matrix, no `depends_on` graph, no step that runs another workflow
  (parallelism inside a step is what `delegate` gives — several delegations in one
  model reply). And nothing runs `lca run` for you: no daemon, no cron, no queue.
- **No knowledge base and no warm sessions.** Nothing is remembered between
  sessions except the transcript you `-resume` and the trace; there is no index of
  the repository, no learned notes, no pool of pre-warmed gateway sessions to
  start a role from. Every session pays its own first prefill, and cache hits are
  whatever the gateway's KV cache gives a byte-stable prefix.
- **No auth layer, by design.** The process's uid is the identity, the audit log
  is per user, and anyone who can run the binary can do what the sandbox allows.

The verifier is a command, not a judge: `check_cmd` exit 0 is the whole definition
of done, the [review](#cross-family-review) is what stands between that and a
change that passes its test while being wrong, and a review is one model's
opinion. `lca report` never invents a cost for the same reason it never invents a
field.

## Layout

```
main.go       entry point: subcommands (init, doctor, eval, report, run), flags, one-shot, usage
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
input.go/input_*.go one keyboard buffer: paste staging, type-ahead, per-OS reads
rawmode_*.go  per-OS raw terminal mode (termios, no cgo)
width_*.go    terminal size (TIOCGWINSZ), falling back to $COLUMNS
mentions.go   @path completion and <file> attachment
changes.go    per-session change log behind /diff and /undo
diff.go       LCS line diff for /diff
sessions.go   transcript listing, /resume picker, startup pruning
recorder.go   audit.jsonl + transcript writing
discover.go   native Slurm discovery (squeue/scontrol/log/startup-script + probe)
stream.go     prose filter: hide tool tags, line-buffer for markdown rendering
markdown.go   terminal markdown renderer (headings, emphasis, code, lists, math)
math.go       LaTeX-ish → Unicode approximation for inline/display math
prompt.go     system prompt assembly (base, tools, delegation, env, skills, project)
roles.go      roles.yaml: role → model chain, tier, effort, prompt, tools, context, reviewer, member
roles_cmd.go  /role: show and change the team live, /role save back to roles.yaml
gwpolicy.go   gateway failure policy over a role's model chain; session headers
delegate.go   delegate tool: worktree snapshot (local or on a member), review, verified diff, apply
verify.go     verifier loop: check_cmd decides done
members.go    the fleet: members.yaml block, per-member sandbox, which machine a session uses
remote.go     one member's transport: ssh reachability gate, remote file tools, remote commands
workflow.go   lca run: the YAML program, bind-time validation, the runner, state/log/lock/resume
trace.go      JSONL trace: turns, task outcomes and workflow steps
eval.go       lca eval: tasks/ runner scored by the verifier, metrics from the trace
report.go     lca report: the trace as one self-contained HTML file (no JS, no network)
roles_test.go gateway policy, headers, delegate/verifier, compaction, trace, sandbox, eval tests
workflow_test.go lca run: parsing, placeholders and quoting, when/on_fail, state, resume, locks
members_test.go the fleet: routing, per-member sandbox, cross-machine worktrees and diffs
remote_test.go remote transport: reachability, quoting, path confinement
report_test.go lca report: totals, escaping of hostile model text, corrupt lines, no external refs
agent_test.go tests for parser / edit / jail / tokenizer
orchestration_test.go end-to-end loop tests against a fake OpenAI-compatible server
```
