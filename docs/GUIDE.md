# Building a loop

This is the page after the README: how to actually write a `loop.toml`, what every field
does, and what breaks in practice. Everything here is checked against `internal/manifest`,
`internal/engine`, and `cmd/herdr-loop` as they exist on `main` — not the target shape. Where
a capability doesn't exist yet, this says so instead of describing it as if it did.

Read the README first for the pitch and the [recorded run](../README.md#recorded-run). Read
[PLAN.md](../PLAN.md) for *why* each safety rule exists — the incident it traces back to.
This document is *how*.

## Contents

- [The mental model](#the-mental-model)
- [The predicate DSL](#the-predicate-dsl)
- [Actions](#actions)
- [Handoff](#handoff)
- [Termination](#termination)
- [kinds.toml](#kindstoml)
- [Graphs](#graphs)
- [Troubleshooting](#troubleshooting)

## The mental model

Three nouns, one verb, borrowed straight from PLAN.md §3:

- **slot** — a named seat for one agent: a `kind` (`claude`, `codex`, `opencode`, `pi`, …), a
  working directory, and optionally a bootstrap prompt.
- **rule** — `when <predicate> then <action>`. The whole engine is a fold over these: every
  time a slot settles, every rule is checked against the current state, and every one whose
  predicate holds fires.
- **loop** — a set of slots plus a set of rules plus a termination condition
  (`max_iterations`). That's the whole manifest.

```toml
[loop]
name = "two-slot-review"

[[slot]]
name     = "impl"
kind     = "claude"
worktree = { branch = "loop/impl", base = "main" }

[[slot]]
name     = "review"
kind     = "codex"
worktree = { branch = "loop/review", base = "main" }

[[rule]]
when = { op = "all", filters = [
  { op = "eq", field = "slot",   value = "impl" },
  { op = "in", field = "status", values = ["idle", "done"] },
] }
then = { prompt = { slot = "review", text = "Review {{impl.handoff}}. Findings only." } }

[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "clean" }
then = { finish = "converged" }
```

This is the shape of every manifest in `examples/`. Two ideas do the actual work:

**Gates.** A rule only ever evaluates against a slot that has *settled* — reached `idle`,
`done`, or `blocked` — never against one still `working`. More than that: a slot must have
completed at least one full turn (a transition *out of* `working`) before any rule fires for
it at all. A freshly spawned slot reports `idle` the instant it exists, which is
indistinguishable from "just finished its work" unless the engine tracks the difference
itself — see [Troubleshooting](#a-gate-fires-before-the-agent-worked) for the bug this fixed.

**Handoff.** Slots don't read each other's terminal output — TUI agents run on the terminal's
alternate screen, and rows that scroll off never enter herdr's scrollback, so pane-scraping
result transport is lossy by construction. Every slot instead gets `$HERDR_LOOP_HANDOFF`, a
path it writes its result to. A rule references another slot's result as
`{{other.handoff}}` (the *path*, so the receiving agent reads the file itself) or, if the
receiver wrote structured front-matter, a typed field like `review.handoff.verdict`.

The engine never runs on raw events either. `events.subscribe` replays event history —
including exit events for panes that died hours ago — before it streams anything live.
`internal/state.Model` drains that replay silently, waits for a fresh `agent.list` to agree
with what it reconstructed, and only then starts reporting transitions the rule engine can
act on. A rule's `when` is evaluated against *that* reconciled state, never against an event
as it arrives.

## The predicate DSL

Not invented for this project — it mirrors herdr's own `AgentViewFilter` grammar (`all` /
`any` / `not` / `eq` / `in` / `exists`), so anyone who has written a herdr agent-view filter
already knows this one. `internal/manifest.validatePredicate` enforces the shape below at
parse time; `internal/engine`'s `holds`/`resolve` evaluate it at runtime. Both are the
authority — this table is copied from `facts.resolve`'s field switch, not from memory.

| op | TOML shape | holds when |
|---|---|---|
| `all` | `{ op = "all", filters = [...] }` | every sub-predicate holds |
| `any` | `{ op = "any", filters = [...] }` | at least one sub-predicate holds |
| `not` | `{ op = "not", filter = {...} }` | the (singular) sub-predicate does not hold — note `filter`, not `filters` |
| `eq` | `{ op = "eq", field = "...", value = "..." }` | the field resolves and equals `value` |
| `in` | `{ op = "in", field = "...", values = [...] }` | the field resolves and equals one of `values` |
| `exists` | `{ op = "exists", field = "..." }` | the field resolves to a non-empty string |

All comparisons are string comparisons — front-matter scalars are rendered to their string
form first, so `value = true` in TOML compares equal to a handoff file's `verdict: true`. This
keeps the manifest untyped rather than inventing a second type system on top of TOML's.

**An unresolvable field is `false`, never an error.** A handoff file that doesn't exist yet is
the normal state before a slot's first result — every operator treats "can't resolve" as "does
not hold," the safe direction. The flip side: a typo'd dotted field
(`reviw.handoff.verdict`) parses cleanly and simply never holds, silently. `validatePredicate`
only strict-checks one idiom — `{ field = "slot", value = "impl" }`, where a misspelled slot
name fails to parse — because that's the only field shape where "this must name a slot" is
unambiguous. Everything else is free-form dotted-path text the validator cannot know is wrong.

### Every field a predicate can read

This is `facts.resolve`'s switch, in resolution order, straight from `internal/engine/engine.go`:

| field | resolves to |
|---|---|
| `slot` | the name of the slot whose transition triggered this evaluation |
| `status` | that slot's reconciled status: `idle` / `working` / `blocked` / `done` / `unknown` |
| `kind` | that slot's agent kind (`claude`, `codex`, …) |
| `tier` | that slot's detection tier: `structured` (self-reported) or `screen` (heuristically classified) |
| `iteration` | the loop's iteration counter so far (see [Termination](#termination) for what "iteration" means) |
| `slot.<name>.status` | another named slot's reconciled status |
| `slot.<name>.kind` | another named slot's agent kind |
| `<name>.handoff` | the *path* to slot `<name>`'s handoff file — resolves whether or not the file exists yet, because telling an agent where to write is the point |
| `<name>.handoff.<a>.<b>` | a dotted-path lookup into that file's front-matter, once it exists |
| any other bare `<name>` | a manifest variable from `Config.Vars` |

Two things worth flagging honestly:

- **Manifest variables aren't wired.** `Config.Vars` exists in the engine and templates can
  read it, but nothing in `mapManifest` or the CLI populates one from `loop.toml` or a flag
  yet — `mapManifest` sets `Vars: map[string]string{}` and stops. A `{{task}}` placeholder in
  a manifest resolves to nothing today; every real example writes the task as literal prose
  instead (see `examples/mixed-harness-converge.toml`).
- Inside a `run` action's `on_success`/`on_failure` branch, four more fields are available —
  `stdout`, `stderr`, `exit_code`, `duration` — the finished command's result. They're checked
  before the general resolvers, so `{{stdout}}` inside a branch always means "the command that
  just ran," never something else that happened to be named `stdout`.

`{{field}}` in a prompt or a `run` argument expands through this same resolver
(`facts.expand`). An unresolved placeholder is an error, not a passthrough — sending an agent
the literal text `{{review.handoff}}` is worse than sending nothing, so the action is refused
and escalated instead. **This applies only to rule-fired prompts.** A slot's own bootstrap
`prompt` (its `[[slot]]` entry's `prompt` field, delivered once at spawn) is sent through
`Engine.SendPrompt` directly, with no template expansion — see the manifest-variables note
above for why that matters in practice.

## Actions

A rule's `then` sets exactly one action. `internal/manifest.validateAction` enforces this —
zero or two variants set is a parse error. What the manifest surface actually offers is
narrower than the engine's internal `Action` type, and that gap is worth being precise about.

### prompt

```toml
then = { prompt = { slot = "review", text = "Review {{impl.handoff}}. Findings only." } }
```

Sends `text` (after `{{field}}` expansion) to the named slot's agent, via herdr's
`agent.prompt`. Skipped — logged, not escalated, no iteration consumed — if the target isn't
in a state that accepts a prompt (`idle`/`done` always; `working` only if that kind's
`KindConfig.MidTurnInjection` is measured and set, which is true for none of the four measured
kinds today; `blocked` and `unknown` never). **This is not queued or retried.** A skipped
prompt is simply gone; the same rule fires again only if the *triggering* slot transitions a
second time — see [Troubleshooting](#a-rule-fires-but-the-target-is-never-prompted).

### spawn — bootstrap only, no manifest syntax

There is no `spawn` action a rule can write in `loop.toml`. Every slot declared in `[[slot]]`
is spawned automatically, once, when `herdr-loop run` starts the loop — a pane is opened, the
agent is started in it, and (if the slot set a `prompt`) that text is delivered the moment the
agent first reports settled. That's `spawnAll` and the initial-prompt bootstrap in
`cmd/herdr-loop/run.go`, not a rule firing.

The engine's internal `Action` type has a `Spawn` field (`engine.Action.Spawn`), and
`fireBranch` — the function that carries out a `run` action's chosen branch — does have a case
for it. But no path from a parsed manifest can ever produce one: `convertAction` in
`cmd/herdr-loop/mapper.go` only emits `Prompt`/`Finish`/`Run` from TOML, on the top-level
action *and* on `on_success`/`on_failure` branches alike, since branches are converted through
the same function. `mapManifest`'s own comment says this outright: spawn "has no
manifest-level surface either, it is only ever used by run's own initial-slot bootstrap." If
you need a slot that only comes up conditionally partway through a loop, that's not reachable
from a manifest today — the `Spawn` field exists for the engine's own internal use, not as a
TOML-reachable capability.

### run — the mechanical gate

```toml
then = { run = ["go", "test", "./..."], on_success = { finish = "converged" }, on_failure = { prompt = { slot = "impl", text = "Tests fail:\n{{stdout}}" } } }
```

`run` is the escape hatch: predicates answer questions about *agent* state, and "do the tests
pass" is a question no predicate over agent status can answer. `argv` is the command and its
arguments — `argv[0]` resolved on `PATH` — each element expanded through `{{field}}` first.

**Argv is executed directly via `exec.CommandContext`, never through a shell.** This is a
security boundary, not a missing feature. A `run` action's expanded values can contain text an
agent wrote — a slot's handoff body, a previous command's captured stdout — and running that
through `sh -c` would make agent-authored text executable: an agent that writes `; rm -rf .`
into a handoff field its prompt never asked it to sanitize would get that text run as shell
syntax the instant a rule interpolated it into a command line. Executing argv directly closes
that off entirely — an expanded `{{...}}` value is always exactly one argument, whatever
characters it contains, because there's no shell there to reinterpret it. The cost is that
pipes, globs, and redirection aren't available; a manifest wanting them calls a script that
has them.

What the manifest surface controls, and what it doesn't:

- **`argv` only.** `manifest.Action.Run` is `[]string` — nothing else. `on_success` and
  `on_failure` are themselves actions (the only place nesting is allowed — a
  `prompt`/`finish` has no exit code to branch on, and a nested `run` inside a branch is
  rejected explicitly rather than silently becoming unbounded recursion).
- **No manifest-level `cwd`, `timeout`, or `env`.** The engine's `RunAction` struct has fields
  for all three, but nothing in `internal/manifest` or `cmd/herdr-loop/mapper.go` exposes them
  from TOML. In practice: the command always runs in the directory `herdr-loop run` (or
  `graph`) was launched from — deliberately not any slot's worktree, so the same manifest
  means the same thing regardless of which slot happened to trigger the rule — and it always
  times out at `engine.DefaultRunTimeout` (10 minutes), with no way to raise or lower it from
  `loop.toml` today.
- **Output is capped and templated back in.** Up to 64 KB of stdout/stderr is kept (the tail,
  not the head — that's where a failing command's error is), available in the branch as
  `{{stdout}}`/`{{stderr}}`/`{{exit_code}}`/`{{duration}}`.
- **Three outcomes, not two.** A nonzero exit fires `on_failure`. A timeout also fires
  `on_failure` (`{{exit_code}}` reads `-1`, distinguishable from a genuine `-1` exit only by
  knowing the command hit its deadline). A command that could not start at all — binary
  missing, `cwd` gone — is neither: it escalates the rule instead of firing either branch,
  because "the test runner isn't installed" is a different problem than "the tests failed,"
  and branching on it would report the wrong one.

### finish

```toml
then = { finish = "converged" }
```

Ends the loop. The string is reported back verbatim as `Outcome.Reason` — this is what an
edge in a graph reads (see [Graphs](#graphs)), and what shows up in `progress.json` and the
event log. Reasons the engine itself can produce, namespaced so they're never confused with a
manifest's own: `loop:budget-exhausted`, `loop:paused`, `loop:stream-closed`,
`loop:teardown-worktree-dirty`.

### escalate — parses, does not run

`{ escalate = true }` is valid TOML and passes `manifest.Parse`, but `mapManifest` refuses to
convert it — a manifest containing one fails `herdr-loop validate` with an explicit error
naming it a tracked M2 gap, rather than silently never firing. Don't reach for it; use
`on_blocked` (below) for the case it would otherwise cover.

## Handoff

Every slot's process gets `HERDR_LOOP_HANDOFF=<handoff_dir>/<slot>.md` in its environment
(`Engine.SlotEnv`, `Engine.HandoffPath`) — one file per slot, not one per iteration. (PLAN.md
§3's own sketch shows a per-iteration `<slot>.<n>.md` path; the code that shipped is simpler
than that sketch: a retriggered slot overwrites its own previous handoff file rather than
appending a new one, which is also why `facts` caches a handoff read for the duration of one
evaluation step — a rule must not see a rewrite mid-check.) The slot's prompt tells the agent
to write its result there:

```toml
prompt = "Implement the fix for Reverse() in str.go. Write your result to $HERDR_LOOP_HANDOFF."
```

`$HERDR_LOOP_HANDOFF` is a real environment variable the agent's own shell exposes, not a
`{{field}}` template — it is never expanded by the engine, because the agent process reads it
itself. The task description before it, by contrast, has to be literal prose today: see
[The predicate DSL](#the-predicate-dsl) for why a `{{task}}` placeholder in a bootstrap prompt
like this one would not expand.

herdr-loop reads that file, never the pane. The reason is structural, not a preference:
claude, codex, opencode, and pi all run their TUI on the terminal's *alternate screen* buffer.
Anything that scrolls off never enters herdr's own scrollback — raising `--lines` on a read
can't recover it, because it was never captured in the first place. A result transport that
depends on reading pane output is lossy for exactly the output it needs, on every harness this
project targets. A file has no such ceiling.

The file is optionally front-matter — YAML (`---`) or TOML (`+++`) delimited, decided by which
delimiter appears, no sniffing — followed by prose:

```markdown
---
verdict: clean
---
No issues found in str.go.
```

A rule reads the typed field as `review.handoff.verdict`, or the whole file's path as
`review.handoff`. If the agent's prompt never asks for a `verdict` field, the rule
referencing it simply never holds — no parse error, no accidental match, no accidental
`finish`. Handoff files are read at most once per rule-evaluation step, so every predicate
checked in that step sees the same file contents — a rule that fires on a verdict never races
a concurrent rewrite of the file it fired on.

## Termination

**`max_iterations` bounds total rule *firings*, not rounds.** Every time a rule's action
actually dispatches, `consumeIteration` increments a single loop-wide counter; once it exceeds
`loop.max_iterations` the loop stops with reason `loop:budget-exhausted`. In
`examples/converge-until-clean.toml`'s cycle (`impl → review → impl`), one full round through
both directions consumes *two* iterations, one per rule that fires — `max_iterations = 8`
there is roughly four rounds, not eight.

This budget is enforced twice, and it's worth knowing why both exist. `internal/manifest`
proves at **parse time** that any manifest containing a rule cycle — a set of rules whose
predicates and actions can retrigger each other — is covered by either `loop.max_iterations`
or a `timeout` on every rule participating in that specific cycle. It does this with a real
graph algorithm, Tarjan's algorithm for strongly-connected components over the slot data-flow
graph each rule implies (which slots a rule's `when` reads, which slot its `then` can prompt),
not a proxy check for "does `max_iterations` appear somewhere in the file." A manifest with no
cycle needs no cap at all — `examples/simple-review-loop.toml` sends `impl → review` and
nothing sends work back, so it validates with `loop.max_iterations` unset.

```
$ herdr-loop validate examples/converge-until-clean.toml
examples/converge-until-clean.toml: OK — 3 slot(s), 3 rule(s), 2 worktree slot(s)
```

Delete `max_iterations` from that file and re-validate:

```
manifest: rule cycle through slot(s) impl -> review has no retry cap (loop.max_iterations) or timeout on every participating rule
```

The **second** enforcement is at runtime, inside `Engine.fire`, and it exists because
parse-time proof only proves the manifest *as written* terminates — it says nothing about a
cycle that only closes through state the engine discovers while running (a handoff field
whose value only becomes a retrigger target once an agent actually writes it, for instance).
The runtime check is what actually stops a live loop; the parse-time check is what stops a
config error from ever reaching a live session in the first place.

Contention doesn't cost budget. Eligibility, the per-slot lock, and the §4.9 corroboration
probe are all checked *before* `consumeIteration` — a rule that can't act right now (slot
busy, corroboration failed) is skipped for free, so lock contention alone can't exhaust a
loop's iteration budget.

## kinds.toml

Per-kind capability data — measured, not assumed. The file's own header states the discipline
plainly: every entry came from running `herdr-loop probe <kind>`, and it should be re-run
after a harness updates rather than trusting a stale entry. A kind with no entry still works —
it inherits `max_concurrent = 1` and no startup sequence, the conservative default for a kind
nobody has measured.

```toml
[claude]
startup_keys      = ["\\e[Z"]
startup_settle_ms = 500
max_concurrent    = 1

[pi]
max_concurrent = 1

[opencode]
max_concurrent = 3

[codex]
max_concurrent = 1
```

Fields, from `cmd/herdr-loop/kinds.go`'s `kindsFile` struct:

| field | meaning |
|---|---|
| `max_concurrent` | how many agents of this kind may be spawned at once, enforced by a counting semaphore the engine queues `Spawn` calls behind. Exists specifically because N concurrent processes of an OAuth-authenticated kind (Claude Code, Codex) race a single rotating credential with no lock — an unresolved upstream bug, not something herdr-loop tries to fix; the cap just avoids creating the race. |
| `mid_turn_injection` | whether prompting this kind while it's still `working` is known to land somewhere sensible. Off (`false`) for every kind today — no harness has been measured for this. |
| `startup_keys` | a raw key sequence sent once via `pane.send_text` after the agent is confirmed interactive, before its first prompt. Claude Code's entry (`"\\e[Z"`, a raw Shift-Tab escape) exists because launching in plan mode silently reinterprets "implement this" as "write a plan for this" — the agent reports `working`, finishes, reports `done`, and has changed nothing, with nothing in the lifecycle explaining why. Sent as a raw escape sequence rather than a key name because herdr rejects both `S-Tab` and `BackTab` as unsupported keys, while the byte sequence they stand for passes through `pane.send_text` untouched. |
| `startup_settle_ms` | how long to wait after `startup_keys` before the first prompt, for a kind whose TUI needs a moment after a key sequence lands. |

`checkKindCapacity` (run by both `validate` and `run`, before anything spawns) rejects a
manifest outright if it declares more slots of a kind than that kind's `max_concurrent`
allows — because `Spawn` holds a kind's token for the agent's whole lifetime, a surplus slot
would otherwise block forever waiting for a token nothing releases, and the process would just
hang after logging a successful spawn for everything else. Caught loudly at validate time
instead.

### Measuring a harness with probe

```sh
herdr-loop probe pi                              # one harness
herdr-loop probe claude --args "--model sonnet"  # pin a cheap model — probing bills real usage
herdr-loop probe --all                           # every kind installed on this machine
```

`probe` runs one harness through the exact lifecycle the engine runs against a real one —
split a pane, wait for its shell, `agent.start`, wait for it to be addressable, send a prompt,
watch the turn finish — timing every step, and prints both what happened and the `kinds.toml`
stanza that follows from it. A kind with no binary installed is reported as absent, not as a
failure. It costs one real agent invocation per kind measured.

This exists because a hand-written capability table goes stale silently. This repo's own
earlier `kinds.toml` claimed opencode needed a six-second settle before its first prompt; the
probe disproved it — opencode needed none, and the stalls that entry was covering for were
actually caused by two agents sharing a squeezed split pane, fixed separately by giving every
slot its own tab (`placement = "tab"`, the default).

Measured so far, against herdr 0.8.0 (from `kinds.toml`'s own header):

| kind | detection | interactive | first turn | notes |
|---|---|---|---|---|
| `pi` | structured | 3.13s | 0.50s | fastest; best default for a reviewer slot |
| `opencode` | structured | 3.12s | 33.54s | slow turns; no settle needed |
| `claude` | screen | 3.02s | 2.51s | needs the plan-mode disarm above |
| `codex` | screen | 3.02s | 1.00s | 1 retry on pane readiness; asks to trust a new directory on first run |

"Structured" means the agent reports its own lifecycle state — `screen_detection_skipped:
true` on every `agent.list` reconcile. "Screen" means herdr infers status by classifying the
rendered terminal, a heuristic `loop.strict` refuses to act on. As of herdr 0.8.0 a strict
loop therefore runs on `pi` and `opencode` only. The other 17 kinds herdr recognises
(`gemini`, `cursor`, `amp`, `droid`, `kimi`, `grok`, `hermes`, `copilot`, `kilo`, `qodercli`,
`maki`, `cline`, `agy`, `omp`, `devin`, `kiro`, `mastracode`) should work on the conservative
defaults but are unmeasured — `probe` is how that changes.

## Graphs

`herdr-loop graph <graph.toml>` executes a graph of loops — this is real, not a sketch. A
**node** is either a single `slot` or a whole nested `loop.toml`; an **edge** decides which
node activates next, based on how the previous one's loop ended.

```toml
# examples/graph-fix-then-verify.toml — verified end to end on herdr 0.8.0
[graph]
name           = "fix-then-verify"
entry          = "fix"
max_iterations = 6

[[node]]
name = "fix"
loop = "fix.toml"

[[node]]
name = "verify"
loop = "verify.toml"

[[edge]]
from = "fix"
when = { op = "eq", field = "outcome", value = "converged" }
then = { activate = "verify" }
```

```sh
herdr-loop graph --kinds kinds.toml examples/graph-fix-then-verify.toml
```

Recorded run: `fix` spawned pi, the agent fixed a real bug, the `go test` gate inside `fix.toml`
passed, the node settled with reason `"converged"`. The edge's `eq field=outcome value=converged`
predicate held, so `verify` activated: it spawned pi again, `go vet` passed, the node settled
`"clean"`. Two activations of a budget of six.

Node manifests are ordinary `loop.toml` files — nothing about `fix.toml` knows it's running
inside a graph. That's the design, not an accident: **a plain loop is a graph with one node**,
so every `loop.toml` in this repo already composes without being rewritten.

### The edge vocabulary is narrower than a rule's

An edge's `when` reuses the exact same predicate grammar a rule's `when` does — `all` / `any`
/ `not` / `eq` / `in` / `exists` — but the field it can read is smaller: `outcome` (or its
alias `reason`), and nothing else. `HoldsForOutcome` in `internal/graph/graph.go` only knows
that one field; a from-node's `outcome` holds the `finish` reason its loop reported verbatim —
`"converged"`, `"clean"`, or `loop:budget-exhausted` if it ran out of budget rather than
finishing on purpose. An edge cannot read a handoff field or a slot's live status the way a
rule can — by the time an edge is evaluated, the node's loop has already ended, so there's no
live agent state left to read, only how it ended. An edge whose `when` mentions any other
field is not an error; `HoldsForOutcome` returns `false` for an unresolvable field, the same
"unresolvable is false" rule a rule's predicate follows, so a graph author needs the same
attentiveness described in [The predicate DSL](#the-predicate-dsl) above.

An edge with no `when` at all is unconditional — activate `then.activate` as soon as `from`
settles. A `when` table with fields but no `op` is rejected as a parse error rather than
silently treated as unconditional; only a wholly absent `when` means "always."

### Termination, one altitude up

The same Tarjan cycle check `internal/manifest` runs over a loop's rules, `internal/graph.Graph.Validate`
runs over a graph's nodes and edges: any cycle between nodes needs `graph.max_iterations` set,
checked before the graph is allowed to run. A per-edge `timeout` key is explicitly *rejected*
at parse time, not silently ignored — an earlier draft accepted one and nothing ever enforced
it, so a graph could pass validation and then activate forever. `graph.max_iterations` is
checked inside every `Activate` call, so it's the one thing that actually bounds a run.

A **budget-exhausted node settles, it does not fail** — `run.Activate` returning
`ErrBudgetExhausted` stops the queue from growing further, and the graph run just ends there;
it does not mark the in-flight node `NodeFailed`. An edge keyed on `outcome == "converged"`
simply never fires in that case, because the node it was watching never got the chance to
report that reason — not because anything broke.

### What doesn't run yet

- **Nodes run one at a time, never concurrently.** Two loops running simultaneously would
  share the machine's per-kind concurrency budget with no broker between two separate engine
  instances to enforce it — sequential is the correct thing to ship first, not a shortcut
  taken for lack of time.
- **A node that names a bare `slot` instead of a `loop` fails explicitly when activated**
  (`node "x" is a bare slot; only loop nodes can execute today`), rather than silently doing
  nothing. There's no engine instance yet to host a single agent outside a loop, so the graph
  parses and validates a slot-node fine but cannot run it.
- **`--teardown` on `herdr-loop graph`** tears down each node's loop as it finishes, subject to
  the same never-remove-a-dirty-tree invariant `herdr-loop run --teardown` enforces. Off by
  default.

## Troubleshooting

Every scenario below is a real failure this project hit while getting a loop to run against a
live herdr session — see `CHANGELOG.md`'s Fixed section for the fuller list. The pattern
worth noticing: none of these were caught by unit tests, mocks, or manifest validation. They
only showed up running a real loop against real agents.

### An agent never leaves `working`

There is no timeout for this today, and that's worth knowing rather than discovering. A rule
only evaluates once a slot has completed a transition *out of* `StatusWorking`
(`Engine.hasWorked`/`noteTurn`) — that's deliberate (see the next entry) — but the flip side is
that a slot stuck in `working` forever produces no transition, so no rule ever evaluates for
it, and nothing escalates on its own. §4.9's corroboration probe (`pane.process_info`, checking
whether the pane has fallen back to its bare shell) only runs as part of firing a rule that
targets that slot — if nothing ever fires for a stuck slot, corroboration never runs either.
Practically: watch `progress.json` or the event log for a slot's "time in current status," and
if a rule depends on that slot ever settling, add a bound elsewhere (a wall-clock check outside
the loop, or simply don't build a manifest with a single point of failure and no external
watchdog).

### A gate fires before the agent worked

This was a real bug (`b46c00b`), not a hypothetical. Status alone can't distinguish "just
finished a turn" from "just spawned and hasn't done anything yet" — both are `idle`. A
two-slot loop with a reviewer gate keyed on `{ field = "slot", value = "impl" }` +
`status in [idle, done]` fired the instant `impl` was spawned, before it had been given its
task, sent the untouched worktree to review, and got back feedback about work nobody had done.

The fix is in `Engine.step`: a completed turn is defined as a transition *out of*
`StatusWorking`, tracked per slot (`e.worked[slot]`), and no rule evaluates for a slot until
that's true at least once. If you see a rule fire suspiciously early — before an agent has had
time to do anything — this is the mechanism to check; it should already prevent it, but a slot
spawned with `hasWorked` set some other way (a bootstrap prompt is *not* itself a turn — the
engine deliberately does not mark one on prompt delivery, only on the agent actually leaving
`working`) is worth confirming against the event log.

### A rule fires but the target is never prompted

A rule's `when` reads the *triggering* slot's state (`{ field = "slot", value = "impl" }`), but
its `then = { prompt = { slot = "review", ... } }` acts on a different slot — and that target
slot's own eligibility is checked separately, in `actionable`, before the prompt is sent. If
`review` isn't `idle`/`done` (or `working` with mid-turn injection on, which is true for no
kind today) at that exact moment — still mid-turn from something else, `blocked`, or
`unknown` — the action is skipped: logged ("prompt skipped: target slot is not accepting
prompts"), no escalation, no iteration consumed.

**This is not queued.** There's no per-slot pending-prompt retry the way the bootstrap prompt
has (`initialPrompts` explicitly re-attempts on the next settled transition — see
[Actions: spawn](#spawn--bootstrap-only-no-manifest-syntax)). A rule-fired prompt that gets
skipped this way only fires again if the *triggering* slot (`impl`) transitions a second
time — `review` finishing whatever it was doing does not, by itself, re-evaluate a rule keyed
on `impl`. If you see a handoff that should have happened but didn't, and the log shows
"prompt skipped," the fix is on the manifest side: make the target's readiness part of the
rule (`op = "all"` combining `impl`'s status with `slot.review.status in [idle, done]`), not
an assumption.

### A prompt stalls

`agent.prompt` is atomic (text plus an encoded Enter) but reports delivery success whether or
not the text actually reached the agent — herdr returns `agent_prompt_stalled` if no lifecycle
change happens within 5 seconds of a prompt landing. Two real causes this project hit:

- **Prompted too early.** A freshly split pane is not immediately an available shell — an
  earlier version of `Spawn` called `agent.start` the instant the pane existed, and it failed
  `agent_pane_busy` on effectively every spawn, because herdr's shell was still the pane's
  foreground process. `Spawn` now waits for `pane.process_info` to report the shell itself
  holding the foreground first.
- **Prompted into a TUI that hasn't finished painting.** herdr's readiness signal is about the
  *process* being interactive, not about whether a text UI has rendered enough to accept
  input. A prompt delivered into that gap lands on nothing. This is what `startup_settle_ms` in
  `kinds.toml` exists to absorb, per kind, once measured — see [kinds.toml](#kindstoml).

### A blocked agent

`blocked` means herdr recognised an approval or question UI on the agent's screen — a
permission prompt, a yes/no question, anything waiting on a human. `on_blocked` decides what
happens, and the default is deliberately conservative:

| `on_blocked` | behaviour |
|---|---|
| `escalate` (default) | notify (`notification.show`), halt that one slot, keep the rest of the loop going |
| `pause` | notify, halt the whole loop |
| `auto` | answer only prompts matching an explicit `[[blocked_rule]]` whitelist, by exact string (whitespace-trimmed, no wildcards, no regex); anything unmatched still escalates |

An orchestrator that blind-fires "enter" on a blocked agent is an agent granting itself
permission over something it was explicitly asked to confirm — this is why `escalate` is the
default and why `auto` requires an exact-match whitelist rather than a pattern. `on_blocked =
"auto"` with an empty or wildcard-containing `[[blocked_rule]]` list is a parse error, not a
silent no-op — `internal/manifest.validateOnBlocked` refuses to accept it.

### A plan-mode harness plans instead of building

Claude Code specifically. Launched fresh, it can start in plan mode, where "implement this"
gets silently reinterpreted as "write a plan for this" — the agent reports `working`, finishes,
reports `done`, and has changed nothing. Nothing in the lifecycle explains why; the loop's gate
just fails forever on work that was never attempted, against code that looks untouched.

The fix is `kinds.toml`'s `startup_keys = ["\\e[Z"]` for `claude` — a raw Shift-Tab escape sent
once via `pane.send_text` after the agent is confirmed interactive, cycling plan mode off
before the first real prompt ever lands. Sent as a raw escape sequence because herdr rejects
both `S-Tab` and `BackTab` as key names, while the byte sequence they represent passes through
untouched. If a Claude Code slot's first turn looks suspicious — reports done fast, touches
nothing — check whether `kinds.toml` is actually being loaded (`--kinds` flag, or
`HERDR_LOOP_KINDS_FILE`) before assuming the model itself is the problem.

### Codex asks to trust a new directory

The first time `codex` starts in a directory it hasn't seen, it asks whether it trusts the
contents — a genuine prompt-injection warning, not a UI quirk. That lands as `blocked`. The
loop escalates and halts the slot rather than answering it, on purpose: pressing enter there
would be the loop granting an agent trust over contents it was explicitly warned about, exactly
the class of thing `on_blocked` exists to refuse by default. Answer it yourself, once,
interactively, in that directory, before putting a codex slot in a loop that runs unattended.

### opencode is just slow

Not a bug — measured at ~34 seconds for a first turn in the probe table above, and minutes
against a busy free-tier endpoint in practice. A loop stays healthy through this; it waits
correctly and doesn't misclassify a slow turn as a stall. What it costs you is wall-clock time
if opencode is on the critical path of something you're watching interactively — put it on a
reviewer slot (cheap, not time-sensitive) rather than the slot you're waiting on.

### A manifest hangs at startup with no error

If a kind's slot count exceeds its `max_concurrent` in `kinds.toml`, the run doesn't fail — it
hangs. `Spawn` holds a kind's concurrency token for the agent's entire lifetime, so a surplus
slot blocks in `acquireKind` waiting for a token nothing will ever release, and the last thing
logged is a successful spawn of everything else. `checkKindCapacity` catches this at `validate`
time specifically because the runtime failure mode is a silent hang — always run
`herdr-loop validate` (or `doctor --manifest`) before `run` on a new manifest, not after.

### Nothing about `{{task}}` resolves

Covered in [The predicate DSL](#the-predicate-dsl): manifest variables (`Config.Vars`) exist in
the engine but nothing populates one from `loop.toml` or a CLI flag today. Write the task as
literal prose in a slot's `prompt`, the way every real example in `examples/` does, not as a
`{{task}}` placeholder — it will either fail to expand (in a rule-fired prompt, which errors
loudly) or be sent completely literally (in a slot's bootstrap prompt, which does not expand
templates at all, so `{{task}}` reaches the agent as those seven literal characters instead of
being caught anywhere).
