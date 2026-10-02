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

- [Quick start](#quick-start) — `lca` → `/setup`
- [Configuration in the session](#configuration-in-the-session--setup-config-set-save) — `/config`, `/set`, `/save`, the `config.json` keys, the API-key rule
- [Driven by a program](#driven-by-a-program---prompt-file--json-oneshotgo) — `-prompt-file`, `-json`, the result object and the exit-code table, [budgets](#budgets-on-one-run---timeout--max-steps--max-tokens-budgetgo), [`-summary`](#the-short-summary---summary-pathmd-summarygo), [the pipeline sandbox profile](#the-pipeline-sandbox-profile--examplespipelinerolesyaml), [the reviewer's per-line verdict](#the-reviewers-per-line-verdict---diff-base-reviewgo), [the next round](#the-next-round-on-the-same-session---session-uid)
- [Non-interactive: CI, scripts and containers](#non-interactive-ci-scripts-and-containers) · [Environment variables](#environment-an-override-one-run-at-a-time)
- [Models & providers](#models--providers--providersgo-modelsgo-chatgo) — presets, per-family profiles, the JSON config
- [The team — `roles.yaml`](#the-team--rolesyaml-rolesgo-delegatego-verifygo) — roles, [delegation and the verifier](#delegation-and-the-verifier--delegaterole-task-check_cmd), [`apply: branch`](#apply-branch--a-delegation-becomes-a-real-branch--branchgo-cleango), [cross-family review](#cross-family-review), [`fork:`](#fork-true--inherit-the-reads-not-the-transcript), [tiers](#tiers-develop-on-premium-operate-on-cheap), `/role`
- [Through the gateway](#through-the-gateway--gwpolicygo-chatgo) — the cacheable prefix, [failure policy](#gateway-failure-policy), transports, per-model settings
- [Measuring runs](#measuring-runs--the-trace-lca-eval-lca-report) — the trace, `lca eval`, `lca report`
- [The sandbox](#the-sandbox--jailgo-toolsgo) — allowlist, shell mode, the GPU policy, [concurrent edits](#concurrent-edits--filelockgo-enginego-editgo)
- [Internal MCP servers](#internal-mcp-servers--mcpgo-mcpclientgo-mcpcmdgo) — the host allowlist, the pinned tool manifest, read vs. write, `/mcp`
- [Deterministic workflows — `lca run`](#deterministic-workflows--lca-run-workflowgo)
- [Working on other machines: the fleet](#working-on-other-machines-the-fleet--membersgo-remotego)
- [Agents & orchestration](#agents--orchestration--agentsgo-enginego-taskgo) — agents, [`task` vs. `delegate`](#the-task-tool-vs-delegate), skills, commands, permissions
- [Terminal style](#terminal-style--uigo) · [Markdown & math](#markdown--math-rendering--markdowngo-mathgo) · [Keeping the loop moving](#keeping-the-loop-moving--enginego)
- [Design (the four non-trivial parts)](#design-the-four-non-trivial-parts) · [Approval modes](#approval-modes--approvalgo) · [Audit & transcript](#audit--transcript--recordergo)
- [Protocol reference](#protocol-reference) · [What this does not do](#what-this-does-not-do) · [Layout](#layout)

## Quick start

```sh
go build -o lca .
./lca            # a session opens; type /setup
```

`/setup` asks for the gateway url, probes it immediately and shows what it
serves — each model with its context window **and where that number came from**
(`262k (server)` is the running deployment's own `max_model_len`; `200k (card)`
is the vendor's model card; `window ?` is nobody published one). If the gateway
answers `401`, it asks for the credential and probes again before going on, so a
keyed gateway is set up from the session like any other. You pick the models with
the arrow keys (type to narrow a long list), give them roles — lead, coder,
reviewer, cheap — and it writes `.lca/roles.yaml` and `.lca/config.json` and
loads them into the session you are already sitting in. **No restart and no
environment variable.** Then `/doctor`, then a task.

Your picks are what gets written. If you accept the optional `tiers:`, it asks
separately whether `lead` and `coder` should *follow* one — a role with a `tier:`
has its own chain replaced by that tier's, and answering `n` (the default) keeps
the models you chose and leaves the tiers in the file for `/tier` to use.

Nothing is written until the last screen, and Ctrl-C at any point leaves the
tree exactly as it was. In a directory with nothing configured, `lca` offers
`/setup` on its own (one line, Enter accepts, `n` goes straight to the prompt).

Everything else is reachable from the same session — `/config` shows every
setting and where it came from, `/set` changes one and keeps it, `/model`,
`/tier`, `/endpoint`, `/role`, `/doctor`, `/report`, `/eval`, `/run`. `/help`
lists them all.

The shell subcommands are unchanged and are what CI uses:

```sh
LCA_BASE_URL=http://node:18080/v1 ./lca init   # write .lca/roles.yaml non-interactively
./lca doctor        # checks gateway, roles, real tool calls, sandbox, members, workflows
./lca run           # list this project's workflows; lca run <name> executes one
DEEPSEEK_API_KEY=… ./lca -model deepseek/deepseek-v4-pro   # a single hosted model
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

### Driven by a program — `-prompt-file`, `-json`, `oneshot.go`

A wrapper that takes a ticket, makes a worktree, calls lca once and then does
the state transitions itself — commit, push, MR, labels, a comment — needs three
things lca gives it here: the task off the command line, one machine-readable
result, and the promise that nothing ever waits for a keystroke.

```sh
./lca -y -role coder -json -prompt-file ticket.md -check ./check.sh   # a ticket from a file
jira-get OPS-412 | ./lca -y -json -prompt-file - -check ./check.sh    # or from stdin
```

`-prompt-file <path>` reads the task from a file, and `-` reads stdin. The bytes
reach the model exactly as they are on disk: a ticket is multi-line markdown with
quotes, `$VAR` and backticks in it, and in argv it has to be escaped by whoever
builds the command line, shows up in `ps` for every user on the machine, and runs
into the command-line length limit. A prompt file **and** a positional task is a
usage error and never a concatenation. An empty one is refused too, rather than
opening a session nobody is sitting at — and so are the four other ways a
wrapper gets this wrong: a second `-prompt-file` (the first one silently lost is
a ticket nobody read), a flag typed *after* the positional task (`lca "task"
-json` — Go stops parsing at the first non-flag, so the flag went into the prompt
and the whole terminal session went to stdout), a path that is not a regular file
(a fifo blocks in `open(2)` before any signal handler or deadline exists), and
bytes that are not valid UTF-8 (every consumer is `encoding/json`, which rewrites
each bad byte as U+FFFD and reports nothing, so the transcript would agree with
the request precisely because both were corrupted).

`-json` writes **exactly one JSON object to stdout and nothing else** — every
human line, including the model's own prose, moves to stderr, and so does colour:

```json
{"status": "passed", "session": "20260102-…", "role": "coder", "models": ["…"],
 "attempts": 2, "check_cmd": "./check.sh", "check_exit": 0, "check_tail": "…",
 "check_logs": ["/path/<session>-1.1.log", "/path/<session>-2.1.log"],
 "files_changed": 3, "diff_bytes": 1840, "turns": 17, "tool_calls": 42,
 "tool_errors": 1, "invalid_calls": 0, "tokens": {"prompt": 0, "completion": 0, "cached": 0},
 "duration_ms": 0, "started_at": "2026-10-03T01:12:04Z", "finished_at": "2026-10-03T01:19:38Z",
 "transcript": "/path", "trace": "/path",
 "lca_version": "sha", "roles_hash": "sha256 of roles.yaml",
 "prompt_hash": "sha256 of the role's system prompt"}
```

`started_at` and `finished_at` are the run's two ends in UTC RFC3339. A duration
cannot answer the question a ticket is read with — "the stand went down at 02:14,
had this run finished by then" — and the wrapper knows when it launched lca but
not when lca stopped.

`check_exit` is `null` when no check ran, so `check_exit == 0` can never be read
as green by accident; `check_cmd` and `check_tail` are always present beside it
(empty when there was no check), because three fields describing one thing that
appear and disappear independently are a `KeyError` on the row that matters.
`check_logs` is the full output of every check of every attempt, one file each,
beside the transcript — `check_tail` is a *selection* out of those bytes (see
[the check that takes minutes](#the-check-that-takes-minutes--checktailgo)), and
these are what a human is given when the selection is not enough. It is `[]` and
never `null`, for the same reason its neighbours have no `omitempty`.
`tokens` is the **run's** spend — every session in it, subagents, the compactor
and the closing summary call included — so it cannot contradict the `reason`
standing next to it. `files_changed` counts what reached the tree, a delegation's
merged diff included. `lca_version`, `roles_hash` (`lca version` prints both) and
`prompt_hash` are what make two runs comparable: a pass rate that moved after
a prompt edit is a different measurement, not a better one, and `prompt_hash` is
the one the other two cannot stand in for — an `AGENTS.md` edit or a tool added
to a role's `tools:` moves neither the binary nor `roles.yaml` and rewrites the
prompt. It is taken over the prompt with the working directory and today's date
normalised out, so two runs of one role in two worktrees agree.

`summary` is there when
`-summary` wrote one, and `review` only on a [`-diff-base`](#the-reviewers-per-line-verdict---diff-base-reviewgo)
run — where its ABSENCE is itself the answer, because a review that could not be
read is `status: failed` and never a silent approve. It can be PRESENT beside a
status of `budget_exceeded`: the reviewer answered and the run then ran out of
clock or tokens, so the verdict is real and the run is not finished — read
`review.verdict` and the status together, which is what the exit table is for.

The **exit code** is the whole of the wrapper's decision, so it is a table:

| exit | status | what it means |
| --- | --- | --- |
| 0 | `passed` | the check ran and was green |
| 1 | `failed`, `unverified` | the model did not manage it — a person should look |
| 2 | — | lca was called wrong, or configured wrong; nothing was decided, so there is no object |
| 3 | `infra_error` | the gateway, a member or an MCP server is not reachable — retry later, leave the ticket alone |
| 4 | `budget_exceeded` | it ran out of time, steps or tokens before anything was settled ([budgets](#budgets-on-one-run---timeout--max-steps--max-tokens-budgetgo)) |
| 130 | `cancelled` | SIGINT, SIGTERM or SIGHUP: the transcript and the object are still written |

The one that matters is 3. "The infrastructure fell over" and "the model did not
manage it" are different tickets, and a run that exits 1 for both makes the
wrapper either wake somebody for a dropped VPN or quietly retry a real failure
for ever. A connection refused, a 5xx or a 429 from the gateway, an overloaded or
drained gateway, a member that does not answer ssh and an MCP server that never
handshook are all 3; a rejected key, a wrong base path and a check the sandbox
will never run are all 2 — and so is a malformed endpoint (`htp://…`, an
out-of-range port), which never reached the network and will not fix itself
overnight. A context overflow that survived the one compaction retry is 4, not 2:
the conversation outgrew the window, which is a thing that happened during the
run rather than a way lca was called.

All three signals a wrapper bounds a job with — `timeout`, systemd, `docker
stop`, `pkill` — share row 130: each one cancels the run, writes the transcript,
the trace and the object, and reaps the children. A crash in lca itself is
reported as `infra_error` with the panic as the reason, so even a bug keeps the
exit code inside the table and leaves something to attach to the ticket.

Nothing in this mode waits for a human. With `-json`, or with stdin not a
terminal, **every** question is an immediate refusal, written to the audit log
and to the transcript with the permission class named — including `mcp_write`,
which `-y` deliberately does not grant (see [Approval modes](#approval-modes--approvalgo)).
`lca eval` and `lca run` are unattended the same way.

#### Budgets on one run — `-timeout`, `-max-steps`, `-max-tokens`, `budget.go`

A role's `steps` and a check's `check_timeout` together bound nothing. An agent
that re-reads the same three files ends every turn inside its step budget, the
verifier hands it the same red check, and the run keeps buying gateway tokens
until somebody looks at the GPU queue in the morning — which a cron job cannot
do. So a run carries three ceilings:

```sh
./lca -y -json -timeout 30m -max-steps 200 -max-tokens 4000000 \
      -prompt-file ticket.md -check ./check.sh
```

| flag | what it bounds |
| --- | --- |
| `-timeout <dur>` | wall clock for the whole run, the check and the summary included |
| `-max-steps <n>` | model requests across the **whole run** — every role, every subagent, every verifier attempt — and a per-session ceiling over each role's own `steps`; a role that asked for less keeps its own |
| `-max-steps unlimited` | no step ceiling at all, over every role's own. A step count is a *proxy* for "stop an agent grinding tokens with nothing to show", and in loop mode a turn spends a step per reply, so 50 is gone before a long task is half done. Unattended it is refused unless `-timeout` or `-max-tokens` is set beside it: one real ceiling has to exist where nobody is watching. `none`, `off` and a bare `0` say the same thing, and so does `steps: unlimited` on a role or `max_steps: unlimited` under `defaults:` |
| `-max-tokens <n>` | prompt + completion across the run, every session in it: subagents, the compactor's request and the closing summary call |

The defaults live in `roles.yaml` under `defaults:` (`timeout:`, `max_steps:`,
`max_tokens:`) and each flag overrides its own independently — stretching the
timeout for one awkward ticket must not silently drop the team's token ceiling.
A bare number for `timeout:` is seconds, like `check_timeout` beside it; a value
that is not a duration is an error, because reading `30 minutes` as "no limit" is
a mistake whose only symptom is the bill.

Exceeding any of them ends the run **gracefully**: the transcript is saved, the
status is `budget_exceeded`, the exit code is 4, and every child the run started
is killed rather than orphaned — the check command's whole process group, a
stdio MCP server, a background subagent and whatever it launched. That last part
is a property of the shape and not of a list: everything a run starts hangs off
one context, so cancelling that context once reaps all of it, and the one-shot
cancels it on every exit path there is.

The clock and the tokens stop different things, on purpose. The clock cancels
everything, because with no time left there is nothing to run a check in. A spent
token budget stops the **model** and lets the verifier finish: a run whose last
reply said "done" and went one token over must still have its check run, or a
finished ticket is reported as "did not fit" and goes to a person for nothing. A
green check outranks a spent budget for the same reason. "Lets the verifier
finish" means exactly one more check: the loop stops there rather than spending
the remaining `verify_attempts` re-running an identical check against a tree the
model can no longer touch, which with a `check_timeout` of half an hour is an
hour of stand builds bought after the run had decided to stop.

All three ceilings are properties of a **run**, armed by the one-shot, by an
`lca eval` task and by `lca run` — and deliberately not by the REPL, so a
`defaults: max_tokens` cannot wedge an interactive session that crossed it over
a long day.

One bound is not lca's: `LCA_GW_MAX_WAIT` (900 seconds by default) is how long a
single model call waits on a gateway answering 503, and a run with no `-timeout`
is bounded only by that, per call. An unattended run should set it low — the
wrapper wants to hear "infra_error" in seconds, not a quarter of an hour per
call — and should carry its own wall-clock kill (`timeout 3600 lca …`) as well,
which lca now handles as a clean row 130 rather than dying where it stands.

#### The short summary — `-summary <path.md>`, `summary.go`

There is a full transcript and an HTML report already, and neither is what a
reviewer opens. `-summary` writes the five hundred words the wrapper pastes into
the merge request and the ticket comment:

```sh
./lca -y -json -summary /tmp/OPS-412.md -prompt-file ticket.md -check ./check.sh
```

Markdown, at most 3 KB, with what changed (one line per file), how it was
verified (the command, its exit, the attempts), what did not work, what it cost
and where the transcript and the trace are. The JSON result carries the path as
`summary`.

The model writes it, in **one** short call after the verdict, on its own system
prompt and with no tools offered — a separate request, so the session's cached
prefix on the gateway does not move. Everything that must be exact is lca's: the
file list, the exit code, the attempt count, the cost and the paths are appended
after the model's prose and counted against the 3 KB first, so a long-winded
summary cannot push the links out. A model asked to recall a token count invents
one.

It is written for a **failed** run too — that is the run that gets read — and for
a cancelled one and a budget_exceeded one. If the gateway goes away between the
verdict and the summary, lca writes the facts itself rather than leaving an empty
file. The only run with no summary is an `infra_error` before the first model
**reply**: there is nothing to summarise and nobody to write it.

Because this file is the one artefact written to be pasted somewhere a wider
audience reads, everything in it is sanitised on the way out: the values of
environment variables whose names match `*TOKEN*`, `*KEY*`, `*SECRET*` or
`*PASSWORD*` become `[redacted]`, and terminal escapes and control bytes are
stripped. The model is handed its own tool results and told to be specific about
what did not work, so a `logs` command that printed an `Authorization` header is
the ordinary path from a live token to a merge request description. The result
object's `check_tail` and `reason` go through the same filter, and so does
[every other artefact a run writes](#secrets-out-of-every-artefact--redactgo).
The 3 KB clamp cuts
at a line boundary and never inside a rune, so the file is always valid UTF-8 —
`open(..., encoding="utf-8")` on it cannot be the thing that fails a run whose
work is already done. A `-summary` path that cannot be written is refused at
startup, with exit 2, before the gateway is touched.

#### The pipeline sandbox profile — `examples/pipeline.roles.yaml`

In the pipeline lca changes files in its own worktree and runs checks. It does
not commit, push, open a merge request or comment on a ticket: those are state
transitions and the wrapper owns them. And on the operator's own machines
production berserk runs under the same unix user as the agent, so `ssh`,
`scancel` and `berserk-ctl.sh` are not dangerous in the abstract — they are the
live gateway.

The profile in `examples/` is that posture, in two files because lca keeps the
two halves of a sandbox in two places, included in one line. Its `permission.run`
block is **fail-closed** — `"*": "deny"` first, then the shapes the work needs —
because `-y`, which is on its own documented command line, turns every `ask` into
an allow before "there is nobody here to ask" is consulted: based on `ask`, the
deny list caught only the spellings it named literally. A command line is also
canonicalised before matching, so `git  push` with two spaces, a tab, or a quoted
subcommand is denied too. `lca doctor -role <name> -y` prints the posture that
flag actually produces. What a pattern list provably cannot catch — a script the
model just wrote, `cargo run` with an edited `main.rs`, a `.git/config` alias — is
written out in the profile itself, next to the enforcement that does hold: a push
token restricted server-side.

```sh
LCA_ROLES=examples/pipeline.roles.yaml LCA_CONFIG=examples/pipeline.config.json \
  ./lca -y -role coder -json -prompt-file ticket.md -check ./check.sh
```

`pipeline.roles.yaml` carries `sandbox.allow` — the hard boundary, which no flag
widens — and the run's budgets. It declares **no roles**, so it collides with
nothing in the team's own file, and `$LCA_ROLES` is read last so its allowlist
wins. `pipeline.config.json` carries the per-command `permission.run` rules,
which apply to every role and need none of them to exist. Allowed: `cargo`, `go`,
`python3`, `bash`, `rg`/`grep`, `git status|diff|log|show` and
`stage/stage.sh check|logs|status`.
Denied: `git commit`, `git push`, `git reset --hard`, `git checkout -- .`,
`git remote`, `git config`, `ssh` except those stage scripts, `curl`/`wget`,
`scancel`, `berserk-ctl.sh` — and `webfetch` is off outright, so the tool is not
in the request prefix at all.

`-y` cannot override any of it: a `deny` returns before trust is consulted, and
the model is told *"denied by a permission rule"* and not to retry — a refusal it
can read and work around, not a dead session. The deny holds on a **member** too,
which it did not before: a remote command is sent as one argument to a login
shell over there, and `*` in a pattern matches `&&`, so `git status && git push`
matched the allow rule whole. Every segment of a line a shell will split is now
evaluated and the most restrictive answer wins.

Read back what including it granted, instead of running an agent to find out:

```sh
LCA_ROLES=examples/pipeline.roles.yaml LCA_CONFIG=examples/pipeline.config.json \
  ./lca doctor -role coder
```

`doctor -role <name>` prints the member, the allowlist, the run's budgets, which
tools are disabled outright, the standing action per permission key, and then
every rule in precedence order with the layer it came from — computed by the same
functions the runtime uses, so a row that says `deny` is the decision a tool call
will get.

#### The reviewer's per-line verdict — `-diff-base`, `review.go`

Inside a delegation a reviewer ends its reply with `VERDICT: approve|reject` and
that is enough: the only consumer is the apply, and the only question is whether
the diff may land. GitLab is a different consumer — it wants a comment on a file
and a line — so the reviewer is also runnable by itself, against a tree the coder
has already finished:

```sh
./lca -y -role reviewer -json -prompt-file review.md -diff-base origin/main
```

The result object then grows one field:

```json
{"review": {"verdict": "request_changes",
            "comments": [{"file": "src/bin/gw.rs", "line": 4891, "severity": "blocker",
                          "body": "this drops the lock before the write"}],
            "summary": "…"}}
```

`verdict` is `approve` or `request_changes`; `severity` is one of `blocker`,
`major`, `minor`, `nit`; `comments` is always a list and never `null`, so a
wrapper that iterates it does not break on the review that found nothing.

**The diff** is the one a merge request shows: the merge **base** of `-diff-base`
and `HEAD`, against a snapshot of the working tree. The merge base, because a
base branch that moved on since the ticket was cut would otherwise put other
people's commits in the review — every comment on them correctly placed, and
posted on somebody else's work. A snapshot of the working tree, taken through a
temporary index that leaves the index, `HEAD` and the uncommitted work exactly as
they were, because at this point in the pipeline the coder's change is usually
**not committed yet** (the wrapper commits after the review) and a file it added
is untracked, which `git diff` does not show at all. It works inside a `git
worktree`, which is where the pipeline always runs it, and a base that is not in
the repository is exit 2 before the gateway is touched — with the `git fetch` that
is usually what is missing named in the message. A tree that does not differ from
its base is exit 2 as well, rather than an approve nobody meant.

**How the object comes back, and why it is not a tool.** The reviewer's final
reply *is* the object, and the instruction asking for it travels in the task's
user message. A `review` tool with the schema in it was the obvious shape and is
the one shape that is not available: the gateway keys its KV cache on the request
prefix — the system prompt plus the tool schemas, computed once per session and
identical for every session of every role — so a tool only the reviewer needs is a
cache miss on the first request of every other role in the team. Nothing above the
user message moves, which is also what lets the next round on the same session
still hit the cache.

**Every `file:line` is checked against the diff**, and that check is the part
that matters. A model reading two thousand lines of diff writes down the number
it can see, which is the line in the file it opened and not a line the hunk
covers; GitLab answers a position that is not in the diff with one 400 for the
whole review, after the pipeline has already decided. So:

| what came back | what happens |
| --- | --- |
| a line inside one of that file's hunks | kept as a comment, with the path spelled as the diff spells it |
| a line outside every hunk, a file not in the diff, a line ≤ 0, an unknown severity, an empty body | **one retry**, naming each problem and the hunks that file actually has — and whatever survives it is moved into `summary` |
| no JSON object, invalid JSON, or a verdict that is neither word | **one retry**, then `status: failed`, exit 1, and **no** `review` field |

Nothing is dropped. A wrong line number is a clerical mistake, not a reason to
discard a finding, so an unplaceable comment is appended to `summary` with the
file and line the reviewer wrote. And an unreadable answer is never a silent
approve: a reviewer whose output could not be read has not approved anything, and
that is the one failure mode in this pipeline that would let a bad change through
unseen. The retry is exactly one — a message on the same session, so the prefix
and the diff above it are already cached — and it is told that it is the last.

Because producing the review is what the run is *for*, a readable review is the
run's `passed` (exit 0) even with no `-check`, which would otherwise have made
every successful review `unverified` and exit 1; the verdict itself is in
`review.verdict`, where the wrapper reads it. A red `-check` beside it still wins
— that is a fact about the tree — and the review is reported anyway, because the
wrapper needs it for the comment it posts. A gateway that goes away is still
`infra_error`: a dropped connection is not a reviewer that failed, and the ticket
goes back in the queue rather than to a person.

What this deliberately does **not** do is take the edit tool away from the
reviewer. The in-delegation reviewer has it denied in-process, because the diff
is about to be applied by that same process and a reviewer grading its own patch
is not a review. Here the tree belongs to the wrapper, and denying a tool would
change the role's tool schemas — which *is* the request prefix, so a review run
and any other run of the same role would key two different caches. That guard
belongs where the other hard boundaries are: the reviewer role's `tools:` list,
or a `permission:` block on it with `edit: deny`, in `roles.yaml` — plus the
[pipeline profile](#the-pipeline-sandbox-profile--examplespipelinerolesyaml). And
`files_changed` is 0 on a review that kept its hands off, which is the wrapper's
own check.

Comment bodies and the summary go through the same filter as `-summary` and
`check_tail`: the values of `*TOKEN*`, `*KEY*`, `*SECRET*` and `*PASSWORD*`
variables become `[redacted]`, and escapes and control bytes are stripped. These
strings are pasted into a public merge request by a program that will not read
them first. The same values are scrubbed out of
[every other artefact the run writes](#secrets-out-of-every-artefact--redactgo),
the transcript included — it goes to Jira as an attachment.

#### The next round on the same session — `-session <uid>`

`-resume` opens the most recent session for a person to type in. A pipeline needs
the other half: one more message to a session that has already run — the
reviewer's comments, a fresh check log — and then exit.

```sh
uid=$(jq -r .session < round1.json)
./lca -y -role coder -json -session "$uid" -prompt-file comments.md -check ./check.sh
```

That continues **that** session: its history is loaded under the current system
prompt, and the round runs as it — the same `x-session-id`, which is what the
gateway keys its KV-cache affinity on, and therefore the same
`x-root-session-id`. The point is cost: a second round on a warm prefix is
cheaper than starting over, and it is also smarter, because the model already
knows what it tried. One id decides all of it — the recorder adopts the uid, so
the transcript is appended to the first round's file (both rounds in the one file
the wrapper attaches to the ticket), the trace appends to the first round's
`.jsonl`, and the result object's `session` and `transcript` name the same
session the wrapper passed in.

The **current** system prompt is kept and the saved one dropped, as `/resume`
does: a saved prompt advertises the tools and the project instructions of the
round that wrote it, and a model told it can call a tool this build no longer has
is being lied to. But the two being identical is this flag's whole cost argument,
so when they differ — a different `-role`, an edited `roles.yaml`, changed project
instructions — the run says so on stderr: the round still works, it just costs
what a fresh session costs.

A uid that names no transcript is exit 2, with the path it looked at named,
because the likeliest way to get there is a wrapper that ran round one under a
different `LCA_DIR`. A uid is also a **file name**, so only the characters the
recorder's own ids are made of are accepted: `-session ../../etc/passwd` is a
wrapper interpolating the wrong variable, and the answer to it is an error and
not a read. `-session` and `-resume` together is exit 2 as well — they are two
different things and a wrapper that passes both would have got the interactive
one.

### Configuration in the session — `/setup`, `/config`, `/set`, `/save`

Configuration lives in the program. You never need an environment variable to
get started; env is an override for environments with no terminal.

| Command | What it does |
| --- | --- |
| `/setup` | the wizard: gateway, models, roles, tiers, check command — writes both files and reloads them live |
| `/setup models` | keep the endpoint and credentials, re-pick the models |
| `/config` | every effective setting, its value, and **which layer and which file** it came from |
| `/config <key>` | one setting: what it means, what it accepts, where it is set, which JSON key holds it |
| `/set <key> <value>` | change it live and write it to `.lca/config.json` |
| `/set -user <key> <value>` | write it to `~/.lca/config.json` instead |
| `/save [<key> …]` | keep what this session changed with `/model`, `/endpoint`, `/tier`, `/approve`, `/think`, `/loop` |

With a team loaded, `/set model` and `/save model` **refuse**: a top-level `model`
key outranks every role's chain at every future start, so the banner and
`/agents` would disagree for ever. They point at `/role <name> model <id>` plus
`/role save` instead, and `-force` does it anyway. `/set -user` warns when the
project file sets the same key and is read after the one it just wrote.

`/set` writes immediately — using `/set` *is* the request to write. The mood
toggles (`/model`, `/endpoint`, `/tier`, `/approve`, `/think`, `/loop`) do not:
they print one faint line naming `/save`, `/config` marks them `session ·
unsaved`, and `/exit` says one line if any are still unwritten. Nothing blocks
to ask, and nothing is silently lost.

**`config.json` keys** (`.lca/config.json` per project, `~/.lca/config.json` for
every project):

| Key | Setting | Accepts |
| --- | --- | --- |
| `base_url` | `endpoint` | a url including the API base path; a missing `/v1` is appended and announced |
| `endpoints` | `endpoints` | a list of urls for `/endpoint` switching |
| `api_key_env` | `api_key_env` | the **NAME** of the variable holding the key |
| `api_key` | `api_key` | the token itself — see below |
| `model` | `model` | a served id, or `provider/model` |
| `tier` | `tier` | a key of `roles.yaml`'s `tiers:` |
| `thinking` | `effort` | `off on low medium high xhigh max` — the level **sent** |
| `tools` | `transport` | `native text auto` |
| `temperature` | `temperature` | `0…2`, or `unset` to send none |
| `ctx_tokens` | `context` | transcript budget (`0` = from the model's window) |
| `max_tokens` · `max_steps` · `cmd_timeout` · `subagent_depth` | `max_tokens` `steps` `cmd_timeout` `subagent_depth` | whole numbers |
| `agent` | `agent` | a primary agent or role name |
| `approve` | `approve` | `off run edit web all` |
| `show_thinking` · `loop` | `show_thinking` `loop` | `on` / `off` |
| `allow` | `allow` | the sandbox allowlist (`roles.yaml`'s `sandbox.allow` overrides it) |
| `providers` · `agents` · `permission` | — | the blocks below; the writer preserves them, and `permission` key **order** (which is precedence) survives byte for byte |

**An API key is never written in plain text without saying so.** `/set api_key
<literal>` refuses and points at `/set api_key_env LCA_API_KEY`, which records
the variable's *name* — `.lca` is inside your repository, and one `git add -A`
publishes a secret. `/set api_key <value> -plaintext` does it anyway, at mode
`0600`, and prints exactly what that exposes — naming `config.json.bak` as well,
because the backup beside it carries the previous key at the same mode. `/config`
always masks the key: `sk-…9f2 (43 chars)`, `set · from $LCA_API_KEY`, `unset ·
$LCA_API_KEY names it, but it is not set in this shell` when the variable was
never exported, and `none · the gateway needs none` for the `sk-noauth`
placeholder. The literal never appears in any command's output — not in
`/config`, not in `/config api_key`, not in the hint that names a file value the
environment shadows — and the audit log records `<redacted>`.

`root` and `dir` are **locators**, not settings: a file that relocated the
directory it was found in would be a paradox, so they come from the environment
and the working directory only and `/config` shows them read-only. `unsafe` is
likewise not a setting — `/unsafe` lifts the sandbox for one session, and a file
that did it for every future run in a directory is a foot-gun.

**Precedence**, last wins: the defaults → `roles.yaml` (for `transport`, the
entry role's chain, its `effort` and its `tier` — `/config` names the team file
for each of them) → `~/.lca/config.json` → `<root>/.lca/config.json` →
`$LCA_CONFIG` → **an `LCA_*` variable that is actually set** → a flag →
something you changed in the session. Env stays above the files on purpose:
demoting it means it stops being *required*, not that it loses — flipping the
order would break every existing `export LCA_BASE_URL` the moment a
`.lca/config.json` appeared, and make a checked-in project file un-overridable
in CI. `/config` pays for that honestly: it prints the shadowed file value with
its path, and `/set` warns, naming the variable, at the moment it matters.

### Non-interactive: CI, scripts and containers

Environment variables are the override for environments with no terminal — you
never need them to get started. Everything below is unchanged:

```sh
LCA_BASE_URL=http://node:18080/v1 ./lca init   # write the team from the gateway's models
./lca doctor                                   # check it end to end
./lca -y "add a nil check to Parse in x.go"    # one task, one exit status
./lca run nightly -var target=pkg/auth         # a deterministic workflow
./lca eval tasks/                              # the evaluation matrix
```

With no terminal on stdin, `/setup` refuses in one line naming `lca init`, the
config file and `lca doctor`, rather than blocking on a question nobody is there
to answer; the pickers fall back to the text output they always printed.

### Environment (an override, one run at a time)

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
  `chat_template_kwargs` for a local vLLM/SGLang endpoint — sourced on both:
  SGLang declares `chat_template_kwargs` and splats it into
  `apply_chat_template`, and it additionally pops a `reasoning_effort` kwarg and
  promotes it to the top level itself, so the one key lca sends reaches the
  template whichever carrier the engine prefers.

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
apply: verified            # verified | always | never | branch
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
    check_timeout: 1800                 # or 30m — this role's check is a stand
    prompt_file: prompts/coder.md
  cheap:
    models: [qwen3-30b-a3b-instruct]
    effort: off
    tools: []
```

When the file is silent, `apply:` is `verified`, `defaults.verify_attempts` is 2
and `defaults.check_timeout` is 600 seconds; the numbers above are only an
example.

`check_timeout` bounds **one** check and is settable on a role as well as under
`defaults:`, as a number of seconds or a duration (`30m`), **up to an hour**.
Both spellings are needed because one role's check is a stand — build, deploy,
`check.sh`, five to fifteen minutes before anything is known — while the rest run
`go test` in seconds, and raising the team default to cover the first gives every
other role an hour to hang in. An hour is a real ceiling and not advice: this
timeout is the only thing that ends a check that stopped answering, and an
unattended run with no bound on it holds its worktree and its ticket until
somebody notices. A value over it, or one that does not parse, is a config error
with exit 2 rather than a silently ignored line that leaves the default in place
and looks obeyed — give the whole *run* longer with `-timeout`, which is where
that belongs.

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

#### The check that takes minutes — `checktail.go`

A stand check is a build, a deploy and then `check.sh`: five to fifteen minutes,
and twenty thousand lines of output with the one line that decides the ticket
somewhere in the middle of it. The last sixty lines — which is what the verifier
used to send back — are the harness's own epilogue, and on a stand that epilogue
is "3 of 412 failed, see above". The model was being told that something was
wrong and shown the one part of the output that does not say what.

So the output is cut three ways instead of one:

- the **full** output of every check of every attempt goes to a file beside the
  transcript (`$LCA_DIR/checks/<session>-<attempt>.<n>.log`, 0600, bounded both by
  `keep_sessions` and by a byte ceiling, oldest session first, because one attempt
  of a stand check was measured at 500 MB), so nothing is lost. The paths are in
  the result object's `check_logs`. A second round on the same session
  (`-session <uid>`) writes `…<session>.r2-<attempt>.<n>.log`, so it cannot
  overwrite the file the first round's result object named.
- what the **model** is sent is *selected* rather than truncated: every line
  matching `error` / `FAIL` / `panicked` / `✗` (case-insensitively) with five
  lines of context around it, plus the last forty. The order is a **priority**
  order and not the order they are listed in: the matching lines first, then the
  end of the log, then the context around each match with the radius shrinking
  while the budget runs out. The line that matched is the one the whole file
  exists to deliver — forty long lines of a JSON deploy record in the epilogue
  must not be able to crowd out the `FAIL` twenty rows above them — and the
  matches are taken from the first forwards, because in a build log the first
  error is the cause and the rest are its consequences. A single line of
  megabytes (a check whose progress output uses only `\r`) is clipped *around*
  its match rather than through the middle.
- what is **omitted is marked**, with the line numbers it stood at — `[… lines
  41-8213 omitted of 20001 …]` — and those numbers are an index into the full log
  file, because the two are cut from the same bytes. A tail that silently skipped
  eight thousand lines would read as a complete log.

Output short enough to arrive whole still does, byte for byte and with no
markers: `go test ./...` on a red package prints forty load-bearing lines, and an
operator reading `check_tail` should see what the command printed. The selection
has a byte ceiling of its own, because this text is appended to a conversation
that goes round the attempt loop again, and the ceiling is real: every omission
marker is charged against it as it is claimed, which is what keeps a log where
every twelfth line matches from spending 3.7x the stated budget on markers. When
it is reached the count of lines left behind is stated rather than dropped — and
it is stated without telling the model to go and read the full log, which under
the default `LCA_DIR` sits outside the sandbox and cannot be read. When the log
*is* inside the jail, the message that goes back names the path and the `grep`
that would find what was cut.

### `apply: branch` — a delegation becomes a real branch — `branch.go`, `clean.go`

One team-wide line in `roles.yaml`:

```yaml
apply: branch
```

Everyone who does not write it keeps today's behaviour to the byte: detached
worktrees, `git apply` of the text patch, the same statuses, the same result
JSON. The switch is **not** per call and the model never chooses it — a tool
parameter would move the request prefix the gateway caches on, and a default
that quietly changed how a verified diff lands would change the outcome of every
existing workflow on upgrade.

With it on, a delegation's work is a branch named
`lca/<role>/<session>-<task>` — `lca/coder/20260930-141233-4412-t3`, the same
ids the transcript, the trace and the HTML report use, so a branch found next
morning traces back to the session that made it. The branch is cut from the
caller's **dirty** tree (as the snapshot already is: a subagent that cannot see
your uncommitted work is working on a different program), the engine commits
once per verifier attempt, and the branch **outlives the call** — including when
the delegation failed, was rejected or conflicted. Today that work evaporates
with the worktree; now you can read it.

`branch` means *verified, on a branch, merged*. Nothing unverified is ever
merged.

**Integration is a real three-way merge in the object database, never a
checkout.** Your tree is usually dirty, and against a dirty tree a text patch
fails as "the patch did not apply" whether the two sides genuinely collide or
merely sit three lines apart — while `git merge` refuses outright and
`git apply --3way` reads the index your dirty tree does not match. So lca
snapshots your current tree as the *ours* side, merges with
`git merge-tree --write-tree` against the snapshot the branch was cut from, and
writes the result as `git diff | git apply --binary` behind the usual per-file
`edit` approval. HEAD, your checked-out branch, your index and the
staged/unstaged split of a half-staged file all come back byte-identical; your
dirty lines and untracked files stay. The clean-tree case is the same code path,
not a second one. Needs git 2.38; an older git is detected up front and falls
back to the patch, saying so.

**A conflict is a conflict.** `status` stays `conflict`, **nothing** is written,
the branch is kept, and the three sides go into `test_tail` as
`git cat-file -p <oid>` commands the calling model can resolve from. Conflict
markers never go into your files — the patch is scanned for them before
anything is written, so a resolution that only *looks* finished is refused
wherever it came from. Then, in order: the calling model; an `integrator` role,
if your team declares one, which gets **one** supervised attempt in its own
merge worktree under the failed delegation's own `check_cmd` (one, literally —
not your `verify_attempts`), its resolution meeting the same `edit` rules a
delegation's own diff meets; and if that attempt does not pass, the run stops,
your screen says so and names the worktree, and both the branch and the merge
worktree are left for you — the worktree holding the markers, the three stages
and a working `git merge --abort`, with the rejected attempt kept readable on
`refs/lca/attempt/…`. Or you, with `/merge <branch>`, which checks the merge
out as a real merge and `/merge --finish`, which writes what you committed
there. Half a verified change is never written: partial integration of the
clean files is deliberately not offered.

Several results in one reply integrate one at a time, each re-snapshotting, so
"the first one moved the tree" is simply the second one's *ours* side; the result
line says `integrated 2nd of 3`. If lca is killed mid-`git apply` your tree
really can be half patched — nothing can fix that — but a journal records it,
`/branches` and `lca clean` tell you, and re-running the integration **is** the
recovery, because an already-integrated branch produces a zero-byte patch.

Reviewing and clearing up:

```
/branches [graph]        the branches lca made, what is integrated, what was interrupted
/merge <branch>          check a conflicted delegation out as a real merge
/merge --finish          write the resolution you committed there into your tree
lca merge …              the same two, outside a session
lca clean                remove the worktrees of dead processes, stale leases, temp files
lca clean --branches     also delete integrated lca/* branches older than a day
lca clean --branches <b> delete that branch even unintegrated — the only way one goes
```

A conflict that reaches you says so **on screen** and names the branch and the
merge worktree, rather than leaving the hand-over inside the model's prompt for
the lead to summarise away.

**How to undo any of it.** Everything lca wrote to your files is unstaged
working-tree change, so `git diff` is the complete list of what landed and
`git log --graph --all --glob='refs/lca/*'` shows each integration commit and
what it merged. `/undo` reverts lca's own writes one at a time, and refuses if
something has touched the file since. Throwing a merged file away is your
`git restore <file>` — lca will not run that for you — and nothing is lost by
doing it, because the branch still holds the work and `lca merge <branch>`
re-does the integration.

Nothing is ever deleted on a timer or on the way out of a session. An
unintegrated branch is deleted only when you name it; an integrated one is kept
for a day, counted from when it **landed** and not from its last commit — an
integration you recover two days later is still a day old to you. And only a
branch lca recorded creating (`refs/lca/made/…`) is ever deleted: the name
`lca/…` is one anybody may use, and a branch of your own pointing into your own
history satisfies every other test there is. A merge worktree you were handed
belongs to a person, not to a process, so `lca clean` leaves it alone and tells
you how to finish it; it goes only once it is a day old, clean, and not
mid-merge.

**Across machines the result is still a text patch.** A member works on a real
branch with real commits *on that machine*, but there is no shared object store
to merge in, so what crosses the ssh link is the patch and the existing
diverged-checkout message. Transferring the objects with `git bundle` is a
measured follow-up, not something half-built here.

**What is never done to your repository**, in one list: no `add`, `commit`,
`stash`, `reset`, `checkout --`, `clean`, `rebase`, `cherry-pick`, `amend`, `gc`
or `reflog expire` in your checkout; never a write to your index or a move of
your HEAD; nothing under `refs/heads` except an `lca/…` branch lca made; no
push, force-push or fetch into your refs; your unrelated dirty files are never
committed to anything that gets merged; no conflict markers in your working
tree; no `git apply` without `git apply --check` first; no `worktree prune`
without `--expire`; and `-c core.hooksPath=/dev/null` and
`-c commit.gpgsign=false` on every `worktree add`, every agent commit and the
resolution merge, because your `pre-commit` hook otherwise runs inside an agent's
scratch copy and your signing config otherwise asks an agent for a passphrase —
and either one failing used to make a verified change quietly vanish. The one
`reset --hard` in the codebase is on lca's own merge worktree, to put the markers
and `git merge --abort` back after an integrator's attempt was rejected.

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
`--tool-call-parser auto` or an explicit registry name, and **no**
`--enable-auto-tool-choice`: SGLang has no such flag and argparse refuses the
launch), and a `--reasoning-parser` — that flag, and only that flag, is what
makes the engine return `reasoning_content` as its own field instead of leaving
the thinking inside `content`. The parser VALUES are not interchangeable between
the two engines (hy3 is `hy_v3` on vLLM and `hunyuan` on SGLang; GLM-5.3's
reasoning parser is `glm47` on vLLM and `glm45` on SGLang), and a value an
engine does not know fails server startup — so `lca doctor` names the flags in
the spelling of whichever engine it resolved, and says so when it cannot resolve
one.

Which engine serves the endpoint is a per-(endpoint, model) fact, because one
gateway url can front both: `engine:` in `config.json` / `LCA_ENGINE` /
`/set engine` names it for the endpoint, `models.<id>.engine:` in `roles.yaml`
names it for one model and outranks everything else, and `/v1/models`'
`owned_by` is read only when it is exactly `sglang` or `vllm`. When nobody has
said, lca sends nothing engine-specific and `lca doctor` says what that means.
It is decided once, before the first request: the gateway keys its KV cache on
the request prefix. `text` (the agent's own tag protocol) is a
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
  a-bare-template:  {template_kwargs: off}    # server 400s on chat_template_kwargs
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
server that 4xx's on it.

`template_kwargs: off` is the other escape hatch, and it is the one to reach for
when a model "cannot print". A chat template is a property of the checkpoint **as
served**: a deployment built without the one the card describes rejects every
`chat_template_kwargs` entry lca sends to steer it, so the model 400s on every
single turn. Turning thinking off does *not* help — for hy3 the off position is
`no_think` and for GLM-5.3 it is `clear_thinking`, both kwargs themselves — so
the switch has to be the model card's. A 400 that names one of our own fields now
says this on screen instead of leaving the operator with "unknown field
enable_thinking". `lca doctor` probes both: two tool calls in one reply
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
lca eval [-out dir] [-role r] [-run regexp] [-transport native,text] [-tier cheap,premium]
         [-repeat N] [-j N] [-keep] [-v] tasks/
lca eval -compare before/results.jsonl after/results.jsonl
```

`-repeat N` runs every task N times, because model variance is large enough that
one run proves nothing: the row that matters is "3 of 5 passed", and five rows
reading passed, failed, passed, passed, failed are not that row. Each repetition
is its own tree and its own line in `results.jsonl` (`repeat` says which run it
was), and a **spread** table at the end counts the passes and the first-attempt
passes per task with the median tokens and time beside them.

`-j N` runs N of them at once. **What that does to the gateway:** each job is a
separate `lca` session with its own `x-session-id`, and each one is sequential in
itself, so `-j N` means exactly N requests in flight and N prefix-cache slots in
use — no more. Nothing is batched and no request is reordered, so the gateway
sees what it would see from N operators working at once; past its own capacity it
queues, and the only thing that changes here is wall clock. It is **1 by
default**, because the right number is a property of your fleet and not of this
program, and it is clamped to the number of jobs. The two git legs that take a
lock on the enclosing repository — making each task's worktree and removing it —
are serialised, so `-j` is safe for tasks without a `repo:` fixture too. `-j > 1`
is **refused** when an `mcp:` block is configured: the MCP tool table is
process-wide, so N orchestrators in one process would send every task's calls
over one task's connection and lose every MCP tool when the first of them
finishes. Such a matrix runs with `-j 1`.

`lca eval -compare a.jsonl b.jsonl` reads finished runs and prints them side by
side, per task and in total: pass rate, passed **on the first attempt** (a task
that needs the verifier to push back twice is a different result from one that
gets it right, and both are `passed`), median tokens and median time. Medians,
because a single run that hit its step ceiling moves a mean by hundreds of
thousands of tokens and tells the reader nothing they can act on — and the cost
medians are taken over the rows that **ran**: a task that died at `workspace:`
reports 0 tokens and 80 ms, and a file holding three of those would otherwise
read as the cheap, fast run. A cell where nothing ran prints `—`, and the number
of rows left out of the medians is stated under the table. A file that is not a
`results.jsonl` — a `trace.jsonl` from the same directory, whose task records
carry `task`, `status`, `attempts` and `duration_ms` too — is refused rather than
scored. Above the table
it prints each run's identity — `lca_version`, `roles_hash`, `prompt_hash` — so a
reader can see whether the difference they are about to explain is between two
different measurements or two runs of the same one. It talks to nothing: no
gateway, no `roles.yaml`, no workspace, because a comparison of two runs from
last month must not need this month's config to load.

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

Every line of `results.jsonl` also carries `lca_version`, `roles_hash` and
`prompt_hash` — the build, the team as it was *loaded*, and a sha256 of the
system prompt that task's role actually ran with. Without the three, two runs
before and after a prompt edit are not comparable, which is the only reason to
run an eval twice. `prompt_hash` is the one the other two cannot stand in for: an
`AGENTS.md` edit, a tool added to a role's `tools:`, or a role switched from
native to text tool calling moves neither the binary nor `roles.yaml`, and each
of them rewrites the prompt. It is taken over the assembled prompt with the two
facts about *this* run removed from it — the working directory and today's date,
which `prompt.go` names in its Environment block — because an eval gives every
task a fresh workspace and a hash of the calendar matches nothing, ever.

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

### Secrets, out of every artefact — `redact.go`

The values of environment variables whose **name** matches `*TOKEN*`, `*KEY*`,
`*SECRET*` or `*PASSWORD*` are `[redacted]` in **everything a run writes**: the
transcript, the subagent transcripts, the audit log, the trace, the full check
logs, the HTML report, the summary, a workflow's `run.log` and `state.json`, and
the result object. The transcript is the
reason the scrub is this wide — it goes to Jira as an attachment, and it holds
every tool result the model saw.

MCP responses were already scrubbed against the tokens the MCP layer itself
resolved. What that cannot see is a token this process never handled: a
`check.sh` that echoes its environment when it fails, a test that prints the
failing request with its `Authorization` header, a deploy script that logs
`curl -H "Authorization: Bearer $BSK_TOKEN"` before dying. The check's output is
not ours and we cannot ask it to behave, so the **environment** is the source of
truth.

Three decisions worth knowing:

- It sits at the **write boundary** of each artefact, on the bytes, not on the
  structs. Scrubbing the records before they are marshalled would mean naming
  every field of every one of them, and the field somebody forgets is the one
  that leaks. It is deliberately *not* at the source: redacting a tool result
  would change what the model is shown, and a run that legitimately prints a
  token to a log it then greps would silently stop working. What a model sees is
  this program's business; what lands in a file somebody attaches to a ticket is
  everybody's.
- It knows every **spelling** of each value. A transcript and a trace record are
  JSON and the report is HTML, and both escape some bytes on the way out: a token
  containing a quote is written `\"` in one and `&#34;` in the other. The raw, the
  JSON-escaped and the HTML-escaped spellings are all searched for, deduplicated —
  which for a token of letters, digits, dashes and underscores is one spelling, so
  a scan costs what it always did. The report is written through a streaming
  variant that holds back the tail of each write, because a token can straddle
  two of them.
- The **value** is asked as well as the name, because the four name patterns are
  substring matches on four very common English words. A value shorter than 6
  bytes is left alone (a `*_KEY=on` would redact every "on" in a build log), and
  so is one that is a filesystem path (`TOKENIZER_PATH`, `GITLAB_TOKEN_FILE`),
  the name of another variable (`BSK_API_KEY_ENV=BSK_GW_TOKEN`), a word of up to
  24 letters (`API_KEY_HEADER=Authorization`, `TOKEN_PREFIX=Bearer`,
  `JIRA_PASSWORD_POLICY=strong`) or a short identifier of up to 12 letters and
  digits (`SSH_KEY_ALGO=ed25519`). Anything with punctuation, a separator or real
  length in it still goes. Without this gate the check's own failure line reached
  the model as `request rejected: missing [redacted] header`, which it cannot act
  on, and it then spent every attempt guessing.
- **stderr is not an artefact.** The live output of a check and of a tool is
  streamed to the terminal as the child produces it, before any buffered copy
  exists to scrub, so lca's own stderr is the one place a credential still appears
  verbatim. It is the operator's live view, deliberately: do not tee it into a
  ticket comment or a CI log. Everything that is a *file* — including a
  workflow's `run.log` and `state.json` — goes through the filter.

**The cost**, since this runs on every write of a file that is rewritten after
every turn, measured rather than assumed:

- With **no credential** in the environment — every development machine, and the
  whole test suite bar three tests — it is one nil check. `redactBytes` returns
  the same slice, so not a byte is copied and the streaming writer is a plain
  pass-through.
- With credentials set, on a 2.1 MB transcript (2 000 messages) and four
  spellings to look for: **3–4 ms per write, 500–700 MB/s** on an M-series
  laptop. For scale, `json.MarshalIndent` on the same transcript is ≈2.8 ms, so
  the scrub roughly **doubles** the cost of writing a transcript — a cost that
  was already linear in the transcript and already paid once per turn, and
  single-digit milliseconds against a turn that takes seconds of gateway time.
  It is one pass per spelling, not one per message or one per secret-by-message,
  which is what keeps it in that range as the session grows.

The measurement is a test (`TestRedactionCostOnALongTranscript`) and prints the
figure, so the numbers here are ones anybody can reproduce.

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

### Concurrent edits — `filelock.go`, `engine.go`, `edit.go`

**The guarantee, in one sentence:** *no write through `edit`, `write`, a
delegation's integration or `/undo` can silently destroy another writer's bytes
— not across tool calls, not across subagents, not across two `lca` processes,
not against your editor; it fails with a message instead.*

How, in five moves:

- **A lease per file**, keyed by the jail-resolved absolute path — never a
  directory and never the tree, because a tree lease turns a parallel reply into
  a queue. It is taken **after** the approval question and held to the rename,
  for single-digit milliseconds. Never across the question: a lease held while a
  human is away would stall every other session, and three subagents asking
  about one file would deadlock. Waiting for one is bounded at two seconds from
  when you asked — not two seconds per waiter ahead of you — and then fails with
  retryable advice: contention degrades to a message, never to a hang.
- **The check runs again under the lease.** The cheap pre-flight before the
  prompt only saves the human a pointless question; the re-check is the
  guarantee. `write` never re-read the file, so a sibling that wrote during the
  prompt used to be erased without a word.
- **The lock files live in the repository** (`<git-common-dir>/lca-locks/`), not
  in `$LCA_DIR`. Two terminals in one project may have different `LCA_DIR`s, and
  a lock neither one can see is not a lock. They never show in `git status` and
  never land in a snapshot. Outside a git repository the fallback is
  `$LCA_DIR/locks`, and the guarantee shrinks to processes that share it. A lock
  whose owner is gone (a `SIGKILL` mid-write) is taken over by the next writer,
  which says so and names the file. The takeover is a single atomic claim, so
  two processes cannot both conclude a stale lock is theirs, and a lock file is
  hard-linked into place complete — an empty one would read back as "no owner"
  and be taken away from the writer who had just been granted it. A lock older
  than a minute is taken over whatever its pid says: pids get reused, and a lock
  nothing could expire made a file unwritable for ever.
- **Writes are temp-file + mode + rename**, so no reader — another agent, your
  editor, a test runner — ever sees half a file, and a `write` meaning "create"
  goes through an exclusive link instead of skipping the check. The mode is
  carried across in full, setuid/setgid/sticky included, and a file you made
  read-only is still refused with the permission error `os.WriteFile` gave —
  `chmod 444` keeps meaning "hands off". The costs are stated plainly: the inode
  changes, so hard links break, a `tail -f` holding the old inode keeps seeing
  the old bytes, extended attributes do not come across, and on a shared
  repository a file a teammate owns comes back owned by `lca` unless the kernel
  lets it be handed back — which is said on screen when it happens, not
  swallowed. The bytes are `fsync`ed before the rename and the directory after
  it, because a rename that is durable before its data is how a crash leaves you
  with an empty file where your old one was. That costs about 8 ms a write on an
  APFS SSD (measured: 130 µs without, 8.2 ms with) — paid once per `edit` or
  `write` tool call and nowhere else, so an agent's whole session pays a fraction
  of a second for never trading your old bytes against a flush that had not
  happened. A process killed between the temp
  file and the rename leaves a
  `.lca-tmp-*` beside the target; it shows up as untracked in `git status` and
  `lca clean` sweeps it (together with a member's, and the lock files under
  `<git-common-dir>/lca-locks/`).
- **The integration takes those leases too**, in both modes: `git apply --check`
  then `git apply` is two passes over the same files, and git has decided the
  patch applies by the time it starts writing. A caller on another machine is
  not leased, because the files are over there and a lock here would guard
  nothing.

**What the model is told to have read** is now per **session** and
content-addressed: mtime *and* size *and* the sha256 of the bytes it was shown.
So a `touch`, or a `gofmt -w` that changed nothing, is no longer a change; bytes
that moved inside one mtime are — including a rival write of exactly the same
length that landed between the read and the stat, which only the hash can see; a `read_file` of a line range does not license a
whole-file `write` (it would drop everything the model never saw); a `task` child
starts with **none** of its parent's reads, exactly as the `task` tool's own
description promises; and after a delegation's diff is merged the caller's
records for those files are **dropped**, because it has seen a diff, not a file.

**Not covered, said out loud:** `run_command` and a verifier's `check_cmd` write
arbitrarily and cannot be leased — we cannot know what a command will write. They
are *accounted for* instead: the session remembers the last one, and when a file
turns out to have moved at or after that moment the message names the command
rather than blaming a stranger for your own `sed -i`.

`/undo` is a writer like any other: it takes the lease and refuses if the file no
longer holds what that change left behind. `/undo force` overrides it.

**On a member** the guard is a compare-and-swap inside the member's own shell:
the hash it must still see is the hash of the bytes the model was shown, so a
same-size rewrite inside `stat`'s whole second is caught by the hash and not
missed by the clock. There is no cross-process lock on a member and none is
invented — a writer on that machine inside the microseconds between the hash and
the `mv` still wins. The temp file it writes through has a unique name per
writer, so two concurrent writers cannot stream into one temp and `mv` a splice
of both into place. If a member's non-interactive shell prints anything to
stdout (an rc file without a `[ -t 1 ]` guard), what `cat` returns is not the
file, so changes to it are refused naming the rc file rather than blaming the
file: re-reading would reproduce the banner, and a `write` would store it.

A file larger than `read_file` returns in one piece can never license a
whole-file `write` — nothing can show the model all of it — so the refusal names
`edit` rather than asking for a read the tool cannot perform.

## Internal MCP servers — `mcp.go`, `mcpclient.go`, `mcpcmd.go`

Your own servers, on your own network: an internal Jira, an internal build
service. No marketplace, no discovery, no `npx`. Two questions decide
everything — **what can this reach** and **what can this change** — and both are
answered in a reviewable file, enforced in one place each, and printed by `/mcp`.

```json
{
  "mcp": {
    "allow_hosts": ["jira.corp.example:443"],
    "allow_cidrs": ["10.0.0.0/8"],
    "stdio": "deny",
    "trust_annotations": false,
    "servers": {
      "jira": {
        "transport": "http",
        "url": "https://jira.corp.example/mcp",
        "headers": {"Authorization": "Bearer ${env:JIRA_MCP_TOKEN}"},
        "ca_file": "/etc/corp/ca-root.pem",
        "timeout": 20,
        "expose":    ["issue_search", "issue_get", "issue_create", "issue_comment_add"],
        "read_only": ["issue_search", "issue_get"]
      }
    }
  },
  "permission": {"mcp_write": {"*": "ask"}}
}
```

Then, once: `/mcp refresh`. Tools appear as `jira__issue_get`, and a role names
them in its `tools:` list like any other tool.

**Nothing is discovered at startup.** The tool schemas come from
`.lca/mcp.lock.json`, written only by `/mcp refresh` — the same house rule
`delegate` already follows: *registration, never a network probe, because the
tool schema is part of the cached prefix and must not depend on whether a host
answered.* So a session opens no connection until the model wants one, `/mcp`
opens none at all, and the request prefix is a function of committed files
rather than of network weather. A server whose tools change mid-session fails
its calls with a message naming `/mcp refresh`, and never changes the prefix.

A `url:` may not carry a username or password: Go would send it as an
`Authorization: Basic` header and `mcp.lock.json` would record it in the clear.
Credentials live in `headers`, as `${env:VAR}` references, and nowhere else.

**Reachability.** Only a host:port literally on `allow_hosts` may be contacted —
no wildcards, `"*"` refused by name, an empty list meaning nothing is reachable.
The resolved address is checked again in the dialer against `allow_cidrs`, with
link-local (the metadata address), multicast and `0.0.0.0` always refused, and
loopback refused unless *that* host:port is itself the loopback entry on the
list. Without `allow_cidrs` the address check catches those cases and not a name
that moved to some other routable address — which is what `allow_cidrs` is for,
and `lca doctor` prints what each host resolves to. Every redirect is refused, including a same-host one, because Go would
forward the `Authorization` header along it. There is no
`insecure` / `skip_verify` knob at any level: point `ca_file` at your internal
CA instead. `allow_hosts` governs MCP only — `webfetch` still reaches any host
with one approval, and `lca doctor` says so out loud.

**Secrets are references, enforced.** Every `headers`/`env` value must be
`"<literal>${env:VAR}"`; a literal is refused at load. The struct that holds it
has no field a token can live in, and everything leaving the layer is scrubbed,
because a 401 body can echo a credential back.

**Writes.** Three permission keys: `mcp` (opening a connection — **ask**, per
server), `mcp_read` (**allow**), `mcp_write` (**ask**). Read vs. write is the
operator's `read_only:`/`write:` lists first, then the server's own
`readOnlyHint` **only** when `trust_annotations` is true (default false — the
server is the component being guarded against), else a write. There are no name
heuristics: a name is not a permission.

`-y`, `a` and `approve: all` do **not** reach `mcp_write`; `/approve mcp-write`
at the terminal is the only standing grant, and a config file cannot persist it.
At an `mcp_write` door `a` is not even offered: it approves that one write and
says so, because a keystroke answering a ticket question must not quietly trust
every later edit and command. A subagent, a delegation, a delegate reviewer and a
workflow `prompt:` step get `mcp_write: deny` unless the role names the key — so
the write schema is not in their prefix at all and injected ticket text has
nothing to aim at.

A role names it in roles.yaml, and a workflow step may grant tools one by one:

```yaml
roles:
  ticket-writer:
    models: [lead-a]
    mode: subagent
    tools: [read_file, jira__issue_get, jira__issue_create]
    permission:
      mcp_write: ask          # without this key its children get deny
```

```yaml
steps:
  file-the-ticket:
    prompt: "open one issue for the failure above"
    role: ticket-writer
    mcp_write: [jira__issue_create]   # this step only, this tool only
```

A step's grant is checked against the registry at load, so a typo fails the run
instead of producing a permission nothing matches.

**Keep it small.** `expose:` is mandatory — there is no "expose everything" —
and a role's `jira__*` wildcard expands to that server's **read-only** tools
only, so it can never hand a role the power to change a ticket. A role's `tools:`
list is a capability and not only a prefix: a tool it does not name is refused at
the call, not merely left unmentioned. Every exposed tool is another schema in the
prefix and another chance for an open model to emit an invalid call, which
`/stats` measures; `lca doctor` prints the schema count per role — in
single-agent mode too — and warns above six MCP schemas.

**Starting from nothing.** `/mcp` is listed before anything is configured and
prints the block to write. Leave `"expose": []` until you know the tool names:
the server loads, registers nothing, and `/mcp refresh` prints what it serves as
a block to paste (every tool a write until you put it in `read_only:`).

**Seeing it.** `/mcp` lists the servers, their transport and endpoint, the
handshake result, the tool count and which tools are in *this* session's prefix
and why not, opening no connections. `/mcp probe` and `lca doctor` are the
explicit ways to contact one; `lca doctor` resolves each allowed host, checks the
token's variable, handshakes, compares `tools/list` against the lock, and names
the fix for each finding. `lca doctor -mcp-refresh` rewrites the lock.

**Recorded.** An MCP call is an ordinary tool call in the trace: arguments
truncated to 300 bytes, the result a byte count. The audit log gets argument
**keys**, the host, the env var's **name** and whether it was approved — never a
value and never the result, because the audit log outlives every transcript.
Ticket text the model asked for does reach the transcript and the inference
gateway; that is what asking a model about a ticket means, and `/mcp` says so.

**stdio** is `"deny"` by default and should stay there on a monitored host. When
allowed, `argv[0]` must be on the sandbox allowlist, there is never a shell, no
shell metacharacter may appear in `args`, `cmd.Env` is built from scratch, the
first spawn asks with the full argv, and the child runs in its own process group.
It is still **not confinement**: `Jail` confines lca's own tools and cannot
confine a program lca started, the check is on `argv[0]` and not on behaviour,
and if `python3` or `sh` is on the allowlist a stdio entry is local code
execution approved once. Run the internal server over HTTP.

Not implemented, and deliberately: `resources/*`, `prompts/*`, sampling,
elicitation, roots. lca advertises `capabilities: {}` and never opens the
server's listening stream, so nothing can arrive unbidden.

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

The longest shape there is, written out so its ceilings are visible, is
[`ticket.yaml`](examples/workflows/ticket.yaml): baseline, read the ticket, build
it on its own branch under a cross-family review, a second written review left in
the repository, merge by an `integrator` role, then the merge request and the
ticket comment through named MCP write tools. Its own `-dry-run` plan:

```
  #  step      kind      role              member  tries      timeout  when         check
  1  baseline  run       —                 local   1          1h0m0s   always       —
  2  ticket    prompt    lead              local   1          15m0s    always       —
  3  build     delegate  coder → reviewer  local   2 × 2 = 4  4h0m0s   always       go test -count=1 ./...
  4  review    prompt    reviewer          local   1          30m0s    always       grep -q '^VERDICT: ship' review.md
  5  merge     prompt    integrator        local   1          30m0s    conditional  —
  6  mr        prompt    integrator        local   1          15m0s    conditional  —
  7  comment   prompt    integrator        local   1          15m0s    always       —
```

**`lca run` has no wall clock of its own, on purpose.** A workflow's units of
time are the `timeout:` lines its author wrote next to a build and a test suite,
so the run is as long as they add up to — six and three quarter hours above — and
a `defaults: timeout:` would cancel a legitimate four-step stand run halfway
through. What bounds it instead is the part that actually runs away: `max_steps`
and `max_tokens` under `defaults:` bound every model in the run, the primary,
every child and the compactor. For long work, remove the step proxy and keep the
real budget — `steps: unlimited` on the role plus a token ceiling.

Across interruptions it is longer still: every step writes `state.json` before
and after itself, the workflow file's sha256 is checked so a resume cannot
straddle an edit of it, and a pid lock keeps two runs off one state, so
`lca run ticket -resume` continues at the step that failed however long ago.

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
refused, every path is shell-quoted), and the read-before-edit guard asks the
member for a hash, not just an mtime — `stat` there has whole-second
granularity, and two writes inside one second are invisible to it. A write to a
member is a **compare-and-swap in that machine's own shell**: it re-checks the
hash of the bytes the change was computed from, refuses if they moved, and lands
through a sibling temp file and `mv`, so an ssh killed mid-transfer leaves a
stray `.lca-tmp` and never a truncated file. There is **no cross-process lock on
a member and none is invented**: another writer on that machine inside the
microseconds between the hash and the `mv` still wins. Two things about the sandbox on a member, neither of them obvious:

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

**You can write to the agent while it is working.** Press Enter and the line is
handed to the model at its next step, labelled so it can tell a correction that
arrived mid-work from the task it was given — "stop, only look at f.txt" lands
while it is still looking. It is never injected mid-request: a request already on
the wire cannot be amended, and appending to the messages under it would move the
prefix the gateway is caching. An unfinished line (no Enter) is still type-ahead
for the next prompt, because half a sentence is not a message, and a slash
command waits for the prompt rather than being sent to the model as prose — it is
held, not lost. The screen says which happened: `queued for <model>`, then `your
message was handed to <model>` when it actually arrives.

`/loop` (or `LCA_LOOP`) turns on **autonomous loop mode**: after any tagless
reply the agent keeps prompting the model to take the next action — unbounded by
the heuristic nudge counter — until the model replies `TASK_DONE`, a budget is
spent, or three replies in a row call no tool at all. Use it to hand off a whole
task and let the agent run it to completion.

Two things to know before you do. A loop spends a step per reply, so pair it
with `-max-steps unlimited` and a real budget (`/set steps unlimited` in a
session, where you are the budget); measured on a 61-turn task, the 50-step cap
reported `budget_exceeded` at turn 50 while the same task with the ceiling
removed finished all 61. And the idle guard is there because the doom-loop
detector cannot see this case: it compares the signatures of recent **tool
calls**, so a model answering prose and calling nothing has no signature to
compare — one such run spent all fifty steps and fifty gateway requests taking
no action of any kind.

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

When there is nobody at the keyboard — stdin is not a terminal, or `-json` was
asked for, or this is `lca eval` / `lca run` — the gate asks nothing at all.
Every question becomes an immediate refusal, with `permission_denied` and the
reason in the audit log and the permission class named in the tool result the
model reads ("`mcp_write` is not granted in an unattended run"). That is not the
same thing as trust: `-y` decides what needs no answer, this decides what happens
to the questions that remain — and `mcp_write`, which `-y` withholds on purpose,
is exactly the door that would otherwise have been left waiting for an answer
nobody is there to type. Every other way into the keyboard refuses before
reading as well: the pickers, the wizard's yes/no and its text fields all answer
"no terminal" without touching stdin.

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
- **`read_only:` is an assertion, not a proof.** lca trusts your list above the
  server's own annotations, so an MCP tool listed there that in fact mutates is
  called with no approval. That is the point of the setting and also its exposure.
  `mcp_read` being allow-by-default likewise means a read produces outbound
  traffic with no prompt once the connection has been approved: injected ticket
  text can drive further reads on that same allowlisted server. Writes are gated,
  results are fenced as untrusted data, and `expose:` keeps the surface small —
  but nothing here stops an agent reading more tickets. There is no certificate
  pinning, so DNS pointing an allowlisted name at another machine inside an
  allowed CIDR, with a valid internal certificate, is not detected.

The verifier is a command, not a judge: `check_cmd` exit 0 is the whole definition
of done, the [review](#cross-family-review) is what stands between that and a
change that passes its test while being wrong, and a review is one model's
opinion. `lca report` never invents a cost for the same reason it never invents a
field.

## Layout

```
main.go       entry point: subcommands (init, doctor, eval, report, run, version), flags, usage
oneshot.go    the one-shot run: -prompt-file, the -json result object, the exit-code table
budget.go     what one run may spend: wall clock, steps and tokens, and the context they share
summary.go    -summary: the short markdown a person reads, written by the model in one call
review.go     -diff-base: the reviewer's per-line verdict, every file:line checked against the diff
redact.go     secrets, escapes and control bytes out of every artefact a run writes
checktail.go  the check that takes minutes: the selected tail, the full log of every attempt
version.go    lca version: the build's sha, the roles.yaml hash, the role prompt's hash
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
edit.go       search/replace apply layer + atomic whole-file write (temp + mode + rename)
filelock.go   per-file write leases in the git common dir, content fingerprints
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
changes.go    per-session change log behind /diff and /undo (leased, staleness-checked)
diff.go       LCS line diff for /diff
sessions.go   transcript listing, /resume picker, startup pruning, -session: the next round on one session
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
mcp.go        internal MCP servers: config + validation, the pinned lock, registration, read vs. write
mcpclient.go  JSON-RPC 2.0 over HTTP (SSE reply shape) and over a child's pipes; the egress chokepoint
mcpcmd.go     /mcp, /mcp probe, /mcp refresh, and doctor's mcp section
trace.go      JSONL trace: turns, task outcomes and workflow steps
eval.go       lca eval: tasks/ runner scored by the verifier, -repeat/-j/-compare, metrics from the trace
report.go     lca report: the trace as one self-contained HTML file (no JS, no network)
roles_test.go gateway policy, headers, delegate/verifier, compaction, trace, sandbox, eval tests
workflow_test.go lca run: parsing, placeholders and quoting, when/on_fail, state, resume, locks
members_test.go the fleet: routing, per-member sandbox, cross-machine worktrees and diffs
remote_test.go remote transport: reachability, quoting, path confinement
filelock_test.go concurrent edits: leases, staleness, atomic writes, the member's CAS
report_test.go lca report: totals, escaping of hostile model text, corrupt lines, no external refs
agent_test.go tests for parser / edit / jail / tokenizer
orchestration_test.go end-to-end loop tests against a fake OpenAI-compatible server
```
