# herdr-loop — design plan

Multi-harness loop orchestration for [herdr](https://herdr.dev). A Go plugin that turns
herdr's pane/agent primitives into declarative, event-driven loops across any mix of
coding agents.

Status: **M0 substantially done** (see §8). Design settled, language settled (Go, §6).
Client lives at `github.com/cyperx84/herdr-api`.
Date: 2026-08-11. Verified against herdr 0.8.0, API protocol 19, schema_version 1.

---

## 1. Verified ground truth

Everything below was checked against the local install, not assumed.

| Fact | Source |
|---|---|
| herdr is Apache-2.0, homepage `herdr.dev`, in homebrew-core, ~22.7k installs/30d | `brew info herdr` |
| Canonical repo **github.com/herdrdev/herdr**, written in Rust, ~27.0k stars, v0.8.0 (2026-08-03) | `gh repo view`, `gh api repos/herdrdev/herdr/releases`, `brew info --json=v2 herdr` |
| Stable cadence ~biweekly; preview channel every few days (`herdr channel set`) | GitHub Releases API |
| **Marketplace auto-indexes any public repo tagged GitHub topic `herdr-plugin`**, refreshed every 30 min | herdr.dev/docs/plugins |
| Docs state plainly: **"This is not a built-in orchestration loop."** Integrations only forward lifecycle state. | herdr.dev/docs/integrations |
| 30+ active third-party plugins exist under topic `herdr-plugin` | `gh search repos topic:herdr-plugin` |
| No Herdr-managed persistent storage API — plugins own their state dir | herdr.dev/docs/plugins |
| Socket API: 90 methods, 25 event types, protocol 19, schema_version 1 | `herdr api schema` (252 KB JSON) |
| Plugin system exists and is first-class: 11 `plugin.*` API methods + full `herdr plugin` CLI group | `herdr api schema`, `herdr plugin` |
| Manifest is `herdr-plugin.toml` with sections `[[build]] [[startup]] [[actions]] [[events]] [[panes]] [[link_handlers]] [config] [default_config]` | herdr.dev/docs/plugins + two installed plugins |
| **Windows is supported** — named pipes instead of Unix sockets, PATHEXT shim resolution | herdr.dev/docs/plugins |
| **Compiled plugins are supported.** "A plugin can be a Bash script, JavaScript app, Lua script, Rust binary, or any other argv command your machine can run." | herdr.dev/docs/plugins |
| **"There is no separate plugin SDK. The entire Herdr CLI is the plugin API."** | herdr.dev/docs/plugins |
| `[[startup]]` hooks run **once after session restore, then exit.** Not supervised daemons. Re-run on live handoff. Failure is logged, does not stop the server. | herdr.dev/docs/plugins |
| `[[build]]` runs on GitHub `plugin install` after user confirmation, **not** on `plugin link` | herdr.dev/docs/plugins |
| Install paths: `herdr plugin install <owner>/<repo>[/subdir]` or `herdr plugin link <path>` | `herdr plugin` |
| Plugin env: `HERDR_SOCKET_PATH`, `HERDR_BIN_PATH`, `HERDR_ENV=1`, `HERDR_PLUGIN_ID`, `HERDR_PLUGIN_ROOT`, `HERDR_PLUGIN_CONFIG_DIR`, `HERDR_PLUGIN_STATE_DIR`, `HERDR_PLUGIN_CONTEXT_JSON`, `HERDR_PLUGIN_ACTION_ID`, `HERDR_PLUGIN_EVENT`, `HERDR_PLUGIN_EVENT_JSON`, `HERDR_PLUGIN_ENTRYPOINT_ID`, `HERDR_WORKSPACE_ID`/`_TAB_ID`/`_PANE_ID` | herdr.dev/docs + `herdr-sesh-bro` source |
| 21 agent kinds recognised: pi, claude, codex, gemini, cursor, devin, agy, cline, omp, mastracode, opencode, copilot, kimi, kiro, droid, amp, grok, hermes, kilo, qodercli, maki | `herdr agent` |
| Agent lifecycle states: `idle`, `working`, `blocked`, `done`, `unknown` | schema `AgentStatus` |
| `agent.prompt` is atomic (text + encoded Enter, honours bracketed paste); returns `agent_prompt_stalled` if no lifecycle change in 5s | `herdr --skill` (0.8.0) |

### Primitives herdr already ships — do not rebuild

```
spawn    worktree.create · pane.split · agent.start
inject   agent.prompt (wait.until[], wait.timeout_ms) · agent.send_keys · pane.send_text
gate     agent.wait (until[]) · events.wait (match_event, timeout_ms) · pane.wait_for_output
observe  agent.list · agent.get · agent.read · session.snapshot · pane.process_info
stream   events.subscribe → pane_agent_status_changed, pane_output_changed, worktree_created, …
notify   notification.show
ui       plugin.pane.open (overlay|popup|split|tab|zoomed)
```

herdr even ships a **predicate DSL** already (`AgentViewFilter`): `all` / `any` / `not` /
`eq` / `in` / `exists` over agent fields. herdr-loop mirrors this shape rather than
inventing a second condition language.

### The gap herdr-loop fills

herdr gives you excellent *single-step* control and zero *multi-step* structure. Missing:

1. No way to declare a **fleet** (N agents, N kinds, N worktrees) as one unit.
2. No **state machine** — no "when A is done, prompt B", no retry, no until-converged.
3. No **result routing** between agents. Pane scraping is lossy (see §4.1).
4. No **blocked policy** — a `blocked` agent just sits there.
5. No **persistence/resume** of an in-flight orchestration.
6. No **cross-harness normalization** — each of the 21 kinds has different prompt,
   approval, and plan-mode semantics.

---

## 2. Architecture

**One Go binary, wrapped as a herdr plugin.** Both, not either — the binary talks the
socket API; the plugin manifest provides distribution, config UI, palette actions,
keybindings, event hooks, and a status pane.

```
                    ┌───────────────────────────────┐
                    │  herdr server (protocol 19)   │
                    │  socket: $HERDR_SOCKET_PATH   │
                    └───────┬───────────────▲───────┘
                 requests   │               │  events.subscribe
                            ▼               │
        ┌──────────────────────────────────────────────┐
        │  herdr-loop supervisor  (runs IN a herdr pane)│
        │  ┌────────────┐  ┌──────────┐  ┌───────────┐ │
        │  │ manifest   │→ │  rule    │→ │ actuator  │ │
        │  │ (loop.toml)│  │ engine   │  │           │ │
        │  └────────────┘  └────▲─────┘  └─────┬─────┘ │
        │                       │              │       │
        │                  ┌────┴──────────────▼────┐  │
        │                  │ state (STATE_DIR/*.json)│ │
        │                  └────────────────────────┘  │
        └───────────────────────┬──────────────────────┘
                                │ spawn / prompt / gate
        ┌───────────┬───────────┼───────────┬───────────┐
        ▼           ▼           ▼           ▼           ▼
    ┌───────┐   ┌───────┐   ┌───────┐   ┌───────┐   ┌───────┐
    │claude │   │ codex │   │opencode│  │  pi   │   │  ...  │
    │worktr.│   │worktr.│   │worktr. │   │worktr.│   │       │
    └───┬───┘   └───┬───┘   └───┬────┘   └───┬───┘   └───────┘
        └───────────┴───────────┴────────────┘
                  handoff dir (files, not scrape)
```

### Why the supervisor lives in a pane, not a `[[startup]]` hook

`[[startup]]` hooks **run once and exit** — verified, not assumed. A long-lived loop
cannot be one. Three options were considered:

| Option | Verdict |
|---|---|
| `[[startup]]` spawns a detached daemon | Rejected. herdr doesn't supervise it; orphans on crash — exactly the `eve-orchestrator` msb-orphan failure mode already recorded. |
| External `herdr-loop run` from any shell | Supported, but invisible in the herdr UI. |
| **Supervisor runs as a herdr pane** (`placement = "split"`) | **Chosen.** herdr owns the lifecycle, it dies with the server, it's visible and steerable, and it's exactly the "visibility + manual/auto steering in one surface" goal. |

`[[startup]]` is still used — for **crash recovery**: on server start it scans
`$HERDR_PLUGIN_STATE_DIR` for loops that were mid-flight, marks them orphaned, and
notifies. It does not resurrect them silently.

### CLI vs raw socket — settled by evidence

Ecosystem convention splits: plugins launched *by* herdr shell out to the CLI via
`$HERDR_BIN_PATH` (both installed examples, `herdr-sesh-bro`); tools running *outside*
herdr talk the raw socket (`collie`, `herdr-remote`).

herdr-loop needs both, and the deciding fact is that **`events.subscribe` and
`events.wait` have no CLI surface** — verified: `herdr events` returns
`unknown command: events`, and `herdr api` exposes only `snapshot` and `schema`. A
push-based, non-polling design is therefore *only* reachable over the raw socket at
`$HERDR_SOCKET_PATH`.

Decision: **raw socket for the supervisor's event stream and hot-path calls**; CLI via
`$HERDR_BIN_PATH` is acceptable for cold one-shot operations where an extra process spawn
is irrelevant. Polling the CLI to fake an event stream is explicitly rejected — it is the
jank-by-construction option.

### Binary shape

```
herdr-loop run <manifest>       # supervisor — long-lived, event-driven (the pane entrypoint)
herdr-loop status [--json]      # one-shot read of live loop state
herdr-loop stop <loop-id>
herdr-loop attach <loop-id>     # TUI board
herdr-loop validate <manifest>  # lint the manifest against live herdr capabilities
herdr-loop doctor               # protocol/version/kind-availability check
```

---

## 3. The loop model

Deliberately **not** a general workflow engine. Three nouns, one verb.

- **slot** — a named seat for an agent: kind + cwd/worktree + lifecycle.
- **rule** — `when <predicate> then <action>`.
- **loop** — a set of slots plus a set of rules, with a termination condition.

The engine is a reactive fold over the event stream, not a poll loop:

```
subscribe(pane_agent_status_changed, pane_output_changed, pane_exited)
for each event:
    update slot state
    for each rule whose predicate now holds:
        execute action (guarded by per-slot mutex + iteration budget)
    if terminate predicate holds: finish
```

Event-driven throughout — no polling. (`events.subscribe` exists precisely for this.)

### Manifest sketch — `loop.toml`

TOML, matching herdr's own config and plugin manifests.

```toml
[loop]
name            = "impl-review-verify"
max_iterations  = 10
handoff_dir     = ".herdr-loop/handoff"      # relative to repo root
on_blocked      = "escalate"                 # escalate | pause | auto (see §4.3)

[[slot]]
name     = "impl"
kind     = "claude"
worktree = { branch = "loop/impl", base = "main" }
prompt   = "Implement {{task}}. Write your result to $HERDR_LOOP_HANDOFF."

[[slot]]
name     = "review"
kind     = "codex"
worktree = { branch = "loop/review", base = "main" }

[[slot]]
name     = "verify"
kind     = "opencode"
worktree = { branch = "loop/verify", base = "main" }

# when impl settles, hand its file to review
[[rule]]
when = { op = "all", filters = [
  { op = "eq", field = "slot",   value = "impl" },
  { op = "in", field = "status", values = ["idle", "done"] },
] }
then = { prompt = { slot = "review", text = "Review {{impl.handoff}}. Findings only." } }

# review found nothing → done
[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "clean" }
then = { finish = "converged" }

# review found something → back to impl, bounded by max_iterations
[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "changes-requested" }
then = { prompt = { slot = "impl", text = "Address {{review.handoff}}" } }

# escape hatch: arbitrary command, branch on exit code
[[rule]]
when = { op = "eq", field = "slot.verify.status", value = "done" }
then = { run = ["cargo", "test"], on_success = { finish = "green" }, on_failure = { prompt = { slot = "impl", text = "Tests fail:\n{{stdout}}" } } }
```

Predicate ops are herdr's own: `all` / `any` / `not` / `eq` / `in` / `exists`.

**Termination is a parse-time invariant, not a soft budget.** `herdr-loop validate`
rejects any manifest containing a rule cycle without a retry cap or timeout — borrowed
from `sean1588/herdr-orchestrator`'s enforced-invariant approach (§10). A loop that can
run forever is a config error, caught before it starts, not a runtime surprise.

**No embedded scripting engine in v1.** The predicate set plus `run`-and-branch-on-exit-code
covers the real cases. Adding rhai/lua/starlark is speculative infrastructure until a
concrete rule can't be expressed. Revisit only with a failing example in hand.

---

## 4. Failure modes as design requirements

These are not caveats to note in a README. They are the requirements.

### 4.1 Handoff is files, never pane scrape

TUI agents (claude, codex, opencode, pi) run on the terminal **alternate screen**. Rows
that scroll off never enter herdr's host scrollback — raising `--lines` cannot recover
them. Confirmed in herdr's own skill docs.

**Contract:** every slot process gets `HERDR_LOOP_HANDOFF=<handoff_dir>/<slot>.<n>.md`.
The slot prompt instructs the agent to write its result there. herdr-loop reads the file.
Pane reads are used only for status/diagnostics, never for result transport.

Optional structured front-matter in the handoff file gives rules typed fields
(`review.handoff.verdict` above).

### 4.2 One worktree per slot, always

Two agents on one working tree produces amend-becomes-new-commit, phantom modifications,
and reflog commits nobody wrote. Non-negotiable: `worktree.create` per slot, or explicit
`cwd` the author owns. `herdr-loop validate` **fails** a manifest where two slots share a
cwd without `allow_shared_cwd = true`.

### 4.3 Blocked policy defaults to escalate

A `blocked` agent means herdr recognised an approval or question UI. An orchestrator that
blind-fires `send_keys enter` on `blocked` is **an agent granting itself permissions**.

- `escalate` (**default**) — `notification.show`, halt that slot, surface in the board.
- `pause` — halt the whole loop.
- `auto` — requires an explicit `[[blocked_rule]]` list of exact prompt patterns to answer.
  No wildcards. No blanket enter. Anything unmatched escalates.

### 4.4 Only prompt settled agents in v1

`agent.prompt --wait` tracks *lifecycle state*, not turn boundaries. Prompting an already
`working` agent may satisfy on the current turn ending, and the text lands in whatever
input widget that harness has — behaviour differs per kind and is **unverified**.

v1: rules only fire against slots in `idle` / `done` / `blocked`. Mid-turn injection is a
per-kind capability flag, off until measured. This is the honest position; measuring all
21 kinds is its own milestone.

### 4.5 `unknown` is not `done`

`unknown` means an agent is present but unclassifiable. Rules must never treat it as
completion. `exists`/`eq` on `unknown` is allowed; it is excluded from any implicit
"settled" set.

### 4.6 Detection trust is two-tier — rules must know which

herdr obtains agent state two different ways, and they are not equally trustworthy:

- **Structured (high trust).** The agent reports its own state; herdr skips screen
  classification entirely. These show `screen_detection_skipped: true` in `agent.list`.
- **Screen-detected (heuristic).** herdr classifies the rendered buffer.

**Measured on a live 7-agent session, not assumed** — and the result corrects an earlier
overstatement in this plan:

| kind | `screen_detection_skipped` | tier |
|---|---|---|
| `pi` | true | structured |
| `opencode` | true | structured |
| `claude` | **false** | screen |
| `codex` | **false** | screen |
| `cmd` | false | screen |

Installing an integration does **not** by itself buy structured lifecycle detection. The
Claude Code integration (`~/.claude/hooks/herdr-agent-state.sh`,
`HERDR_INTEGRATION_VERSION=7`) calls exactly one method — `pane.report_agent_session` —
which reports *session identity*, not status. Claude's `idle`/`working`/`done` still comes
from screen classification.

**Requirement:** the detection tier is a **runtime fact read per agent** from
`screen_detection_skipped`, never static data in `kinds.toml` — the same kind can differ
by version and by whether its integration is installed. `herdr-loop doctor` reports which
slots run on heuristic detection. Loops gating irreversible actions on a heuristic `done`
are unsound: default warn, `strict = true` refuses.

Practical consequence: as of herdr 0.8.0 a `strict` loop over Claude Code slots would
refuse to run. That is the honest state of the world, not a bug in this design.

### 4.7 Concurrency limit per harness kind — credential safety

Independent research surfaced a failure class that hits this design directly: N concurrent
Claude Code processes race a single rotating OAuth token in `~/.claude/.credentials.json`
with no lock or leader election, and `~/.claude.json` corrupts under concurrent writes
(truncation mid-write cascading across sessions). Reported repeatedly through 2026 and
unresolved upstream.

This is not theoretical here — this box runs OAuth-subscription auth with no API-key
fallback, and has already been bitten by a shared-refresh-token race in a different tool.
A loop that fans out five Claude slots is a credential-corruption generator.

**Requirement:** `kinds.toml` carries `max_concurrent` per kind, defaulting to a
conservative value for OAuth-authenticated kinds. Spawn is queued against that limit, not
fired in parallel. `doctor` warns when a manifest exceeds it. herdr-loop does **not**
attempt to broker or serialize the credential refresh itself — that is upstream's bug and
out of scope; the mitigation is not creating the race.

### 4.8 The event stream replays history — reconcile before acting

**Discovered during M0, verified experimentally, undocumented upstream.** Two facts
about the socket API that the docs do not state and that change the engine's design:

1. **Ordinary requests are one-per-connection.** The server writes the response and
   immediately closes; a second request on the same socket fails with a broken pipe.
   `events.subscribe` is the sole exception — it acknowledges with
   `{"type":"subscription_started"}` and then holds the connection open.
2. **`events.subscribe` replays event history before streaming live events.** Two
   connections opened seconds apart received an identical 21-event backlog, including
   `pane_exited` for panes that had died long earlier.

The second is a live hazard for a rule engine: replayed `done` events would fire rules
for work that finished hours ago — spawning agents, sending prompts, running commands,
all on stale history. Naive edge-triggering on this stream is silently, expensively wrong.

**Requirement.** The supervisor does not act on raw events. It maintains a reconciled
state model:

```
subscribe → drain replay into the model (no rules fire)
          → fetch authoritative state (agent.list / session.snapshot)
          → when model agrees with snapshot, mark the stream LIVE
          → only then do rule predicates fire
```

Rules are evaluated against *state transitions in the model*, never against event arrival.
This also makes the engine restart-safe: rebuilding from replay plus a snapshot is exactly
what resume needs anyway (§ M4 orphan recovery).

Reconciliation is **periodic, not startup-only**. The client drops events rather than
applying backpressure to the server when a consumer falls behind, so a dropped event must
be self-healing: a scheduled re-snapshot repairs divergence, and drops are counted and
logged rather than swallowed silently.

Open sub-question: whether the replay buffer is bounded (21 events observed in a session
with far more history suggests a ring buffer) and whether the server offers any
caught-up marker. Neither is documented; the snapshot-agreement approach above does not
depend on the answer.

### 4.9 Trust no inferred state — corroborate before acting

**The single most cross-cutting finding in the ecosystem sweep** (full evidence:
`docs/demand-research.md`). Across nine unrelated repos and teams, the same failure
recurs: an agent, slot, or process is reported fine — `idle`, `working`, `started` — while
actually stalled, blocked, or dead, and nobody finds out until they go looking.

herdr #509 (workflow shows idle while sub-agents still work), #2618/#2591 (OpenCode stuck
blocked), discussions #1635 and #1346 (Pi and OpenCode blocked state reported as
working/idle), vibe-kanban #3227 (session stuck "running" after socket close),
herdr-orchestrator #34 (stalls silently at timeout), agentbox #302 (start reports success
when the bind failed), collie #54/#34 and herdr-remote #17 (input silently lost).

Nine independent codebases converging means this is **structural to inferring agent state
from CLI output**, not a bug in any one implementation. §4.6 already establishes that
`claude` and `codex` states are screen-classified heuristics — this is what that costs in
practice.

**Design principle, stated rather than left implicit:** every inferred state is
*unverified* until corroborated. Before acting on a transition that triggers irreversible
work, the engine corroborates: the process is actually alive (`pane.process_info`), and
the predicate that would explain the state actually holds. A heuristic `idle` alone is
never sufficient grounds to tear down, merge, or commit.

### 4.10 Teardown never destroys uncommitted work

Backed by a real documented data-loss incident in the closest prior art:
`sean1588/herdr-orchestrator` #34, the maintainer's own dogfood report — *"no-PR escalation
`--force`-removes the worktree, discarding uncommitted work… destroyed a
completed-but-uncommitted implementation."* Same design shape as ours: worktree per task,
timeout-based escalation.

§4.3 covers approval *bypass*; this is a distinct invariant it does not cover.

**Requirement:** every escalation, timeout, and teardown path commits or stashes before
any worktree removal. `--force` removal of a dirty worktree is not reachable from any
code path. Small guard, non-negotiable for v1 — the failure mode is proven, not
hypothetical.

### 4.11 Every escalation carries a structured reason

Same source thread: *"the tool tends to stall silently at a timeout instead of saying
why."* Corroborated by four independent reports of the status-lies class (§4.9).

**Requirement:** escalation events carry which predicate failed, which rule fired, how
long the slot was stalled, and the last observed state — never a bare timeout. Cheap to
build in now, expensive once the event schema is frozen. v1.

### 4.12 Reconciliation must be order-independent and idempotent

`persiyanov/herdr-reviewr` #5: two plugins both subscribed to `worktree.created` and
*"herdr runs the handlers in no guaranteed order."*

herdr's event bus offers no subscriber-ordering guarantee. Combined with the replay
behaviour in §4.8 and the drop-on-full-buffer policy, the reconciler must be correct under
arbitrary ordering, duplication, and loss. This is a constraint to design against from day
one, not a property to discover in production.

### 4.13 Protocol pinning

herdr-loop checks `schema_version` + `protocol` at startup like the client does, and
refuses to run against an incompatible server with a clear message rather than sending
malformed requests. Pin: schema_version 1, protocol 19.

---

## 4b. Positioning — why this is buildable at all

A survey of the terminal multi-agent orchestration field (claude-squad, uzi, Crystal,
Conductor, vibe-kanban, container-use, claude-swarm, claude-flow, bosun, ccswarm, cccc,
sculptor, CAO, Tmux-Orchestrator, plus Anthropic's own Agent Teams) found three distinct
control-loop architectures:

1. **PTY/pane-scraping** — `capture-pane`, hash-diff for "changed", hardcoded substring
   match for approval prompts, blind Enter to accept. Trivially multi-vendor, brittle by
   construction: breaks silently whenever an upstream CLI changes its prompt text.
2. **Structured protocol** — consume the vendor's own machine-readable stream or hooks.
   Robust, but single-vendor by construction, which is why these tools support the fewest
   backends.
3. **Message ledger** — ACK-required messages and read cursors instead of inferring state
   from terminal bytes. Structurally soundest, least adopted.

The field's stated #1 gap is *"no cross-vendor agent-lifecycle protocol — every tool
reimplements its own idle/running/waiting classifier tuned to one backend's prompt
strings."*

**herdr is the closest thing that exists to that protocol** — with an important
qualification measured in §4.6, not assumed. herdr normalizes 21 agent kinds into one
five-state lifecycle behind a single API. *How* it derives that state varies per kind:
structured self-reporting for some (`pi`, `opencode`), screen classification for others
(`claude`, `codex`). So herdr has solved the **normalization and breadth** problem
industry-wide, and the **robustness** problem only partially.

That is still the whole reason herdr-loop is worth building. Every other orchestrator
spends its complexity budget re-solving completion detection per backend — the brittle
part — and none of them normalize across vendors. On herdr the detection layer exists,
is maintained upstream, and improves for every consumer whenever an integration gains
structured reporting. The budget therefore goes to the layer above: fleets, rules,
handoff, policy.

**herdr-loop is a policy engine, explicitly not a detector.** If it ever starts scraping
panes to infer state, the design has failed — improving detection is upstream's job, and
contributing there is the right move if a kind's detection is weak.

Two further gaps this plan targets deliberately:

- *"Approval gates are routed around, not solved"* — the industry answer is disabling
  permission prompts wholesale, trading a detection problem for a safety regression. No
  surveyed tool treats approval gates as first-class default-safe. §4.3 does.
- *"No credential/session broker"* — §4.7 doesn't fix it, but refuses to trigger it.

Known-declining prior art, so it isn't cited as live: Crystal deprecated 2026-02
(successor Nimbalyst), vibe-kanban sunsetting 2026-04, uzi ~14 months stale,
`parruda/claude-swarm` repo gone, Claude Flow's Rust/consensus claims unsupported by its
implementation. Best-executed scraping-tier detector to learn from: `bosun` (Rust) —
screen-*region*-targeted detection with debounce rather than full-pane regex.

## 4c. Demand evidence — what users actually asked for

Full mining report with every URL and quote: `docs/demand-research.md`. Method per the
standing plugin doctrine: mine open *and* closed issues across upstream and the ecosystem
for unmet demand, then build the findings in.

**Methodology finding worth keeping:** herdrdev/herdr's issue bot triages *all* feature
requests out of Issues into **Discussions** (863 of them). Anyone mining only
`gh issue list` on herdr core structurally misses the entire demand signal.

**Validated by evidence — keep as designed:**

- *File-based handoff* (§4.1). herdr discussion #2401 and issue #2306, plus collie #54/#34
  and herdr-remote #17, all describe the same failure: text typed into a live pane is
  silently dropped. Our handoff avoids that class by never typing into panes for results.
  Verify in v1 that no race-prone fallback path exists.
- *Parse-time loop termination* (§3). Correct, but **not a differentiator** — bermuda,
  herdr-factory and herdr-orchestrator independently converged on bounded-loop-with-cap.
  Table stakes. Do not lead with it.

**Reframed for honesty:**

- *Per-kind concurrency caps* (§4.7). Real and well-evidenced — claude-code #29217 has 8+
  independent reporters and a third-party fix tool — but **zero herdr-ecosystem users have
  asked for it** (explicitly searched, no hits). Present it as defensive design against a
  documented unfixed upstream bug, never as a user request. Overclaiming demand is the
  fastest way to lose trust.

**New requirements adopted from demand (were not planned):**

| # | Requirement | Where | Timing |
|---|---|---|---|
| 1 | Structured reason on every escalation | §4.11 | v1 |
| 2 | Teardown never destroys uncommitted work | §4.10 | v1 |
| 3 | Order-independent, idempotent reconciliation | §4.12 | v1 |
| 4 | Per-slot progress + append-only log surfaced to a controller | below | v1 |
| 5 | Escalation → OS notification hook | below | v1 |

**4 — Per-slot progress and log surface.** herdr discussion #713, two distinct commenters,
one stating this is *what is holding them back from switching their orchestrator to
herdr* — the only finding in the sweep where a missing feature blocks herdr adoption
outright. Four people describe the same shape: watching N parallel workers from one
control point with no way to tell how far along each is. Requirement: each slot publishes
structured progress and an append-only log; the supervisor surfaces both live. This is
what makes herdr-loop watchable mid-run instead of a black box until convergence — and it
directly serves the "visibility and steering in one surface" goal.

**5 — Escalation notification hook.** claude-code #70591 (multiple commenters: *"as the
number of concurrent agents increases, terminal switching becomes a significant source of
friction"*) and herdr discussion #1970, where a third-party plugin already answers a
blocked agent's prompt from a macOS notification. The plumbing exists; piggyback
`notification.show` rather than inventing a channel. Without this, §4.3's escalations only
reach a per-slot log and the original pain recurs inside herdr-loop.

**Not covered by the sweep**, flagged rather than assumed: `sarmientoF/herdr-pr-loop`,
`SecretAardvark/pi-overseer`, `machine-machine/herdr-factory-loop-skill`.

## 5. Cross-harness capability table

21 kinds, all different. Encode as **data, not code** — a `kinds.toml` shipped with the
plugin and overridable per user:

```toml
[claude]
settled_states      = ["idle", "done"]
mid_turn_injection  = "unverified"
plan_mode_disarm    = "[Z"      # raw shift-tab; S-Tab/BackTab are rejected by herdr
quota_pattern       = '(\d+)%'        # status-line quota read before dispatch

detection           = "integration"   # §4.6 — hook-reported, high trust
max_concurrent      = 2               # §4.7 — OAuth token race

[codex]
settled_states      = ["idle", "done"]
mid_turn_injection  = "unverified"
detection           = "integration"
max_concurrent      = 2

[some-kind-with-no-integration]
detection           = "screen"        # heuristic — `doctor` warns, `strict` refuses
```

Fields are looked up at runtime, never hardcoded in Rust. Unknown kinds get conservative
defaults (settled-only, escalate-on-blocked) and still work.

---

## 6. Go stack

**Language: Go.** Decided 2026-08-11, over Rust.

Rationale, in order of weight:

1. **Cross-compilation.** All-platform support is a stated requirement.
   `GOOS=windows GOARCH=amd64 go build` produces a Windows binary from this Mac with no
   toolchain wrangling. This compounds with §7: the proven bootstrap pattern gives Windows
   **no download fallback** (no POSIX `sh`), so Windows users compile from source —
   requiring a Go toolchain is a far smaller ask than requiring Rust, and builds in
   seconds rather than minutes.
2. **Precedent.** The highest-adoption compiled herdr plugin, `cloudmanic/herdr-plus`
   (220★), is Go — so the §7 bootstrap script is copied with zero adaptation. So is
   `sean1588/herdr-orchestrator`.
3. **Workload fit.** JSON over a socket, subprocess management, a state machine, a TUI —
   all IO-bound. Goroutines + channels map directly onto "one event stream fanning out to
   N agent slots." None of Rust's advantages are load-bearing here.
4. **Iteration speed** on a tool under daily development.

Rust's real counter-argument, recorded so the decision is auditable: herdr itself is Rust
and has no published SDK crate, so a `herdr-api` crate would be a first and could attract
upstream or ecosystem pickup. Rejected because the actual plugin ecosystem is mostly
TypeScript, Shell, and Go — a Rust crate would serve few of them.

| Concern | Choice | Note |
|---|---|---|
| IPC | `net.Dial("unix", …)` / `github.com/Microsoft/go-winio` on Windows | herdr uses UDS on unix, **named pipes on Windows** — a small platform-split transport, `_unix.go` / `_windows.go` build tags |
| Concurrency | goroutines + channels; `context.Context` for cancellation | one reader goroutine on the event stream, one per slot |
| CLI | `spf13/cobra` | conventional subcommand tree; `urfave/cli` is the lighter alternative |
| Manifest | TOML via `BurntSushi/toml` | matches herdr's config and plugin manifests |
| Wire types | hand-written structs for the ~20 methods actually used | 90 methods exist; generating all of them from the 252 KB schema is waste. Schema stays the reference, checked in for diffing across herdr releases |
| Logging | `log/slog` (stdlib), single-line structured output | no dependency; automated jobs need deterministic parseable status |
| TUI (status board) | `charmbracelet/bubbletea` + `lipgloss` | only needed at M5 |
| Dist | `[[build]]` + prebuilt release binaries | §7 — pattern proven, `go build` in place of `cargo build` |

Scripting engine: **cut from v1** (§3).

**Fuzzy matching note (affects the sesh-bro port, not herdr-loop):** Go has no equivalent
of Rust's `nucleo`. That makes shelling out to `fzf` the right call for the picker rather
than a native reimplementation — which is also the choice that preserves the existing
muscle memory and preview behaviour.

---

## 7. Distribution — the compiled-plugin bootstrap

A compiled plugin has a bootstrap question: `[[build]]` runs on GitHub install, but a
toolchain may not be present on the user's machine.

**SOLVED** — there is an established ecosystem pattern with the highest-adoption
precedent. `cloudmanic/herdr-plus` (220★, Go) does exactly this:

```toml
[[build]]
platforms = ["linux", "macos"]
command = ["sh", "scripts/build.sh"]

[[build]]
platforms = ["windows"]
command = ["go", "build", "-o", "bin/herdr-plus.exe", "."]
```

with `scripts/build.sh`:

```sh
if command -v go >/dev/null 2>&1; then
    exec go build -o bin/herdr-plus .
fi
echo "no Go toolchain found — downloading the latest prebuilt binary…" >&2
INSTALL_DIR="$(pwd)/bin" sh install.sh
```

**Adopt verbatim** — herdr-loop is Go, so this is copied with no adaptation at all: prefer a local
toolchain build (exact source match), fall back to a SHA256-verified prebuilt from GitHub
Releases when the toolchain is absent. Checksum verification is mandatory, not optional —
`lachieh/vfox-herdr` independently uses the same verified-download approach.

Windows caveat inherited from the precedent: the download fallback is a POSIX `sh`
script, so Windows gets the direct-compile branch and requires a Rust toolchain on PATH.
Acceptable — requiring a Go toolchain is cheap, and this was a deciding factor in §6. A PowerShell fallback is a later refinement.

Alternative precedent, rejected: `aorumbayev/herdr-workflows` always compiles at install
(Bun single-executable) with no prebuilt fallback — simpler, but excludes anyone without
the toolchain. `madarco/agentbox-herdr-plugin` doesn't self-bootstrap at all and just
errors with manual install instructions — worse UX.

**Marketplace listing is free**: tag the public repo with GitHub topic `herdr-plugin` and
the marketplace indexes it within 30 minutes. No submission process. So distribution is
`herdr plugin install cyperx84/herdr-loop` on day one — the only unsolved part is how the
binary materialises (A/B/C above).

---

## 8. Milestones

- **M0 — spike. Substantially done (2026-08-11), one gap.**
  `github.com/cyperx84/herdr-api` — transport with `_unix.go`/`_windows.go` split, request
  client, event `Stream`, typed agent model, `cmd/herdr-api-spike`.
  - ✅ Connects, verifies protocol 19, enumerates a real 7-agent session with per-agent
    detection tier.
  - ✅ Cross-compiles: `GOOS=windows GOARCH=amd64` and `GOOS=linux GOARCH=amd64` both
    build clean — the premise Go was chosen on, now verified rather than assumed.
  - ✅ Live post-replay delivery proven: replay burst 08:03:53–08:03:57, then 44s of
    silence, then a `pane_created` for a pane created at 08:04:41. Unambiguous.
  - ✅ Decoder contract tests against a real captured frame, including nullable-field
    and `Settled()`-excludes-`unknown` invariants. `go test` green.
  - ✅ Surfaced two undocumented protocol facts (§4.8) that would have broken the engine,
    and corrected an overstated detection claim (§4.6).
  - ❌ **Gap: no live `pane_agent_status_changed` has been observed end to end.** None of
    the 7 live agents changed state during the observation window, and manufacturing one
    means starting an extra agent — which on this OAuth-only box is exactly the
    credential race §4.7 exists to avoid. The decoder is contract-tested against a real
    payload, but the wire path for the single signal the rule engine gates on is
    unproven. **Close this first at M1**, when spawning an agent is the milestone's own
    work and a status transition comes for free.
- **M1 — spawn/gate/collect.** `worktree.create` → `pane.split` → `agent.start` →
  `agent.prompt` → wait on event → read handoff file. One slot, hardcoded.
- **M2 — manifest + rule engine.** `loop.toml` parsing, predicate evaluation, the reactive
  fold. Multi-slot. State persisted to `$HERDR_PLUGIN_STATE_DIR`.
- **M3 — plugin wrapper.** `herdr-plugin.toml` with actions, pane entrypoint, `[[events]]`
  hooks, `[config]`. Installable via `herdr plugin link`.
- **M4 — safety.** Blocked policy, worktree-collision validation, protocol pinning,
  orphan recovery on `[[startup]]`, `doctor`.
- **M5 — board + distribution.** Status pane TUI, `kinds.toml` capability table populated
  by actual measurement, prebuilt release pipeline, Windows verification.

**Worked example for M2:** the live `w48` workspace — claude + pi + opencode + codex
running in parallel worktrees on hero-phases. That's the exact target use case and it is
already running, so it's a real test, not a synthetic one.

---

## 9. Open questions

- ~~**O1 — binary plugin convention.**~~ **CLOSED.** Established pattern exists; see §7.
  Copy `cloudmanic/herdr-plus` (220★): `[[build]]` → `sh scripts/build.sh` → local
  toolchain build, else SHA256-verified prebuilt download from GitHub Releases.
- **O2 — `[[events]]` completeness.** Docs show `on = "worktree.created"` with one example
  and no full event-name list, no filter/debounce/concurrency semantics. Need the real
  list — likely mirrors the 25 API event types, but unverified.
- **O3 — API stability.** *Partly answered.* No formal stability declaration or
  deprecation policy exists; herdr is pre-1.0. What it does give: a protocol version
  (19), an introspectable schema (`herdr api schema`), a `min_herdr_version` gate in
  plugin manifests, and docs advising a `ping`/`status` compat check plus graceful
  handling of unknown fields. Treat as versioned-but-evolving: pin protocol, tolerate
  unknown fields, gate on `min_herdr_version`. Still open: is there a breaking-change
  policy at all?
- **O4 — Windows reality check.** Docs say Windows is supported; no brew bottle proves it
  and both extant plugins declare `platforms = ["linux","macos"]`. Needs an actual test.
- **O5 — mid-turn injection semantics per kind.** Measured, not assumed. Blocks §4.4.
- **O6 — `typify`** against a 252 KB draft-2020-12 schema: does it hold up, or is
  hand-writing the ~20 types we actually use faster?

---

## 10. Prior art — and an open challenge to this whole plan

The herdr ecosystem is not empty. 30+ actively-maintained plugins under topic
`herdr-plugin`, several adjacent to this one:

| Repo | ★ | What it claims |
|---|---|---|
| `vekexasia/pi-extensible-workflows` | 168 | "Deterministic multi-agent workflow orchestration" |
| `madarco/agentbox` + `agentbox-herdr-plugin` | 340 / 26 | Parallel sandboxed multi-agent runner |
| `persiyanov/herdr-reviewr` | 382 | Diff review sidebar |
| `AltanS/collie` | 336 | Mobile PWA control (raw socket API consumer) |
| `dcolinmorgan/herdr-remote` | 217 | Remote relay (raw socket API consumer) |
| `yigitkonur/awesome-herdr` | 119 | Curated ecosystem index |

### Premise check — verdict: ADJACENT

A dedicated sweep read the actual source and manifests of every orchestration-shaped
plugin in the ecosystem. Result: a real cluster of overlapping partial attempts, none
occupying the target slot.

| Project | ★ | Declarative | Multi-harness | Real loops | herdr plugin | Push events |
|---|---|---|---|---|---|---|
| `aorumbayev/herdr-workflows` | 7 | ✅ YAML | ✅ profile discovery on PATH | ❌ **none** | ✅ | ❌ |
| `sean1588/herdr-orchestrator` | 4 | ✅ YAML state graph | ~ architecturally, Claude-only in practice | ✅ bounded | ❌ standalone Go daemon | ❌ polls |
| `firegnu/herdr-loop-lab` | 1 | ❌ shell scripts | ~ claude+codex only | ✅ nested convergence | ❌ | ❌ |
| `razajamil/herdr-factory` | 3 | ✅ YAML belts | ❌ Claude only | ~ `max_bounces` | ~ layout events only | ❌ daemon |
| `vekexasia/pi-extensible-workflows` | 168 | ❌ imperative JS | ❌ Pi only | ~ hand-written `for` loops | ~ display only | — |
| `madarco/agentbox` | 340 | ✅ but for VM provisioning | ✅ | ❌ | ✅ | — |

The cleanest single data point: `herdr-workflows` describes itself as a **"Linear YAML
workflow runner for herdr panes"** — declarative and genuinely multi-harness, with no
while/until/retry primitive anywhere in its step vocabulary. That is direct evidence that
*declarative + multi-harness* and *loop-capable* are currently **disjoint sets** in this
ecosystem.

**Nothing combines: declarative manifest + genuine multi-harness + first-class
loop/converge semantics + real herdr-plugin packaging + event-driven detection.** That
combination is the open slot. Plan confirmed; proceed.

### What to beat, concretely

- **`herdr-workflows`** — match its agent-profile discovery and native-compiled
  distribution quality, then add the loop primitive it admittedly lacks.
- **`sean1588/herdr-orchestrator`** — beat on being an *actual plugin* rather than a
  bolt-on daemon, on generality past its issue→PR-shaped default topology, and on
  demonstrated (not merely architectural) multi-harness support. It also ships no LICENSE
  file, so **do not copy code from it** — design ideas only.
- **`herdr-loop-lab`** — beat on declarative config over shell, harness breadth, and
  actually being packaged and maintained.

### Ideas worth stealing (attribution, not code)

- **`sean1588/herdr-orchestrator`'s enforced safety invariants**, validated at parse
  time — notably *"loops terminate: every cycle has a retry cap or timeout."* That is
  strictly better than §3's `max_iterations` as a soft budget. Adopt: `validate` rejects
  any rule cycle lacking a bound. Its open issue #37 also admits permission-prompt wedges
  aren't well surfaced — independent confirmation that §4.3 is a real gap, not a
  nice-to-have.
- **`herdr-workflows`' agent profile discovery** (`init` scans PATH) — better than
  hardcoding kinds; feeds §5's `kinds.toml`.
- **`herdr-loop-lab`'s gate/judge split** — a mechanical gate (exit code) *and* a
  cross-model adversarial judge scoring unmet acceptance criteria. Stronger than a bare
  exit-code check. Candidate for a v2 rule action.

### Honest limit on the event-driven claim

`sean1588/herdr-orchestrator` polls, and its stated reason is that *"`status.changed` has
no push source."* That is right for the half it's describing. Precisely:

- **Agent lifecycle** — push. `events.subscribe` delivers `pane_agent_status_changed`.
  This is where herdr-loop is genuinely event-driven and the competition is not.
- **External world state** — no push. "PR merged", "CI green", "file appeared" have no
  herdr event. Those gates need polling or a webhook, and herdr-loop will poll them on an
  explicit per-gate interval.

Do not market this as "no polling." The accurate claim: *agent state is push-driven;
external gates poll on a declared interval.*

Separately in flight: the terminal multi-agent orchestration state of the art outside
herdr (claude-squad, uzi, Crystal, Conductor, vibe-kanban, container-use, claude-swarm,
claude-flow, tmux orchestrators) — for design patterns and known failure modes.

---

## 12. Known gaps — honest status as of 2026-08-11

Both repos are green: `gofmt`, `go vet`, `go test -race`, and cross-compilation to
windows/amd64, linux/amd64, linux/arm64 and darwin/arm64. An adversarial review found
seven defects; all seven are now closed, each with a regression test that fails against
the old behaviour.

**Closed since the review:**

- F1 going live mid-replay, F2 the never-released concurrency token, F4 the bootstrap
  prompt bypassing the per-slot lock, F5 corrupted drop metrics (fixed with F1),
  F6 the unconstructible `pane.output_matched` subscription, F7 a shipped example the
  binary rejected.
- F3 detection tier now reaches the rules: `Model.SlotTier`, a `tier` predicate field,
  and `loop.strict` refusing to fire against a screen-classified slot.
- §4.9 corroboration is now real, not described: the engine probes `pane.process_info`
  before spending an iteration and refuses to act on a pane holding no foreground
  process. A self-reporting agent is not probed — its status is observed, not inferred —
  and a probe that errors is inconclusive rather than proof of death.
- §4c demand items 1, 2 and 5: structured escalation reasons, the per-slot progress and
  append-only log surface, and escalation reaching an OS notification.

**Still open, tracked not hidden:**

1. **`run` actions are designed but not executed.** `validate` refuses them loudly rather
   than letting a rule silently never fire, which is the right failure, but the escape
   hatch promised in §3 is absent. This is the largest remaining functional gap.
2. **Teardown safety (§4.10) is vacuously satisfied.** `WorktreeRemove` exists in
   herdr-api but has no caller in herdr-loop and `halt` kills nothing, so nothing can
   destroy uncommitted work yet. The invariant must be built *with* the first teardown
   path, not after it — the prior-art incident it comes from was exactly that ordering
   mistake.
3. ~~**No live `pane_agent_status_changed` observed end to end.**~~ **CLOSED, and it
   found a bug that would have stopped the engine ever working.** Driving a real
   `opencode` agent through idle → working → done revealed that the server spells the
   same event two ways: a *global* subscription delivers the underscored
   `pane_agent_detected` from the schema's `EventKind`, while a *per-pane* subscription
   echoes back the dotted subscription type — `pane.agent_status_changed`, which appears
   nowhere in the schema and carries no `type` field. Filtering on the documented
   spelling, which both this plugin and herdr-api did, silently discarded every status
   event while global events kept arriving: the stream looked healthy, the drop counters
   read zero, and no rule could ever fire. Now normalized at the herdr-api boundary and
   pinned by tests in both repos.

   Two smaller findings from the same session, both also fixed: a freshly split pane is
   not an available shell, so `agent.start` against one fails `agent_pane_busy` —
   `Spawn` now waits for the shell to reach its prompt using `pane.process_info`. And
   `agent.start` returns a launch-pending seat rather than blocking until the agent is
   interactive, so callers must poll rather than assume.
4. **`strict` is honest but currently near-unusable.** As of herdr 0.8.0 only `pi` and
   `opencode` self-report, so a strict loop over Claude Code or Codex slots will not fire
   at all. The right fix is upstream — contributing structured status reporting to those
   integrations — not weakening the check here.

**Verified genuinely enforced in code**, not merely described — confirmed by adversarial
review reading the implementation: escalate-by-default with whole-string prompt matching
and no implicit enter; settled-state-only prompting; `unknown` excluded from settled;
one-worktree-per-slot; parse-time cycle detection (a real Tarjan SCC pass) backed by a
runtime iteration budget; SHA256 verification in the build bootstrap that fails closed and
could not be bypassed under test.

---

## 13. Direction: graphs are this engine recursed, not a second plugin

Loops are not the top of the model. A loop converges one fleet of agents on one piece of
work; real work is a **graph of loops** — compose any harnesses herdr can run into a
larger structure where each node is itself a converging fleet. This section records how
that lands, and why it is not a separate plugin, so the reasoning is not rediscovered.

### The loop is already a graph

§3's model is `slots + rules + predicates`, and `validate` runs a Tarjan SCC pass over
the slot data-flow graph to prove every cycle is bounded. A graph of loops is the same
shape one level up: nodes, edges, predicates. The right move is therefore to generalize
the *node* — a node is either a **slot** (one agent) or a **nested loop** — rather than to
build a second engine that reimplements evaluation, reconciliation, and policy.

```
graph.toml          nodes + edges + predicates
   ├─ node "impl"   → loop.toml   (slots + rules)   converges internally
   ├─ node "review" → loop.toml
   └─ node "ship"   → slot         a single agent
        edges carry the same predicate DSL and the same handoff contract
```

A plain loop is a graph with one node, so today's manifests keep working unchanged.

### Why not a second plugin

Not a taste call. Every scarce resource this engine manages is **global to the machine**,
and splitting the engine across two processes breaks each one:

| Resource | What splitting it costs |
|---|---|
| Per-kind concurrency tokens (§4.7) | The credential race is machine-wide. Two processes each enforcing "max 2 claude" yields four concurrent agents racing one rotating OAuth token — the exact failure the cap exists to prevent. |
| The reconciled state model (§4.8) | `state.Model` requires `Apply`/`Reconcile` be driven from one goroutine. Two owners means two divergent models of one session, and no way to decide which is right. |
| Event subscriptions (§4.12) | herdr's bus has no subscriber-ordering guarantee — observed via `herdr-reviewr` #5, two plugins racing on `worktree.created`. A second subscriber to the same panes reproduces that bug by construction, and doubles the replay each connection must drain. |
| Worktrees (§4.2) | One owner per working tree. Two planners allocating worktrees cannot see each other's claims. |

**herdr also offers no channel to bridge them.** `plugin.action.invoke` takes
`{plugin_id, action_id, context}` where context is a fixed workspace/pane/selection
struct. It fires an action; it does not carry a payload and does not return data. There is
no plugin-to-plugin data surface at all. A graph plugin driving a loop plugin would have
to shell out or reimplement the engine — the duplication argument again, with worse
ergonomics and a coordination problem in the middle.

One supervisor, one state model, one concurrency budget, one event stream.

### Not built now — deliberately

`herdr-loop` has not run end to end once (§12). Building a composition layer above a thing
that has never executed is speculative infrastructure, and the footprint ladder says the
bar for new surface is high. Adding it now would also mean designing edges against a rule
engine whose real-world behaviour is still unobserved.

What is worth doing now, at near-zero cost, so the recursion is free later:

1. **Do not harden "node == one agent."** Keep the manifest shape able to admit a nested
   loop where a slot sits today, rather than baking the assumption into types and
   validation.
2. **Keep the predicate DSL and handoff contract identical at both levels.** Edges between
   nodes should evaluate the same `all`/`any`/`not`/`eq`/`in`/`exists` predicates over the
   same file-based handoff that rules already use. One contract, two altitudes.
3. **Keep termination validation altitude-agnostic.** The SCC cycle check should be able to
   run over a node graph without change; a cycle between loops needs a bound for exactly
   the reasons a cycle between slots does.

Revisit once a real loop has run and the failure modes of the rule engine are observed
rather than predicted. The trigger for building this is a concrete piece of work that
genuinely needs two loops composed — not the fact that it would be elegant.
