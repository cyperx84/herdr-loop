# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - Unreleased

Pre-alpha. Verified against herdr 0.8.0, API protocol 19, schema_version 1. The engine now
drives a real loop to convergence end to end when the built `herdr-loop` binary is invoked
directly — see the harness table this entry adds to README.md. Installing it as a herdr
plugin (`herdr plugin install` / `plugin link`) has not been exercised; see the README's
Status section for the exact boundary between what runs and what is still wiring.

### Added

**Manifest, state model, engine — the foundation**
- `internal/manifest`: `loop.toml` parsing (`[loop]`/`[[slot]]`/`[[rule]]`/`[[blocked_rule]]`)
  and validation — slot-name uniqueness, exactly-one-of cwd/worktree, the shared-cwd
  rejection (`loop.allow_shared_cwd` to opt out), `on_blocked = "auto"` requiring a
  non-empty, wildcard-free `[[blocked_rule]]` whitelist, predicate and action shape
  checking reusing herdr's own `all`/`any`/`not`/`eq`/`in`/`exists` grammar.
- Parse-time rule-cycle detection: Tarjan's algorithm over the slot data-flow graph rejects
  any manifest containing a retrigger cycle with no `loop.max_iterations` and no per-rule
  timeout covering every rule in that cycle.
- `internal/state`: a reconciled agent-state model (`Model`) that drains
  `events.subscribe`'s replayed history before treating anything as live, then repairs
  itself against `agent.list` on a scheduled interval so a dropped event self-heals instead
  of going silently missed. Tracks detection tier (`structured`/`screen`/`unknown`) per
  agent, read from `screen_detection_skipped` on every reconcile — never assumed from kind.
- `internal/engine`: the rule fold — evaluates predicates against reconciled transitions
  (never raw events), gates prompting behind settled-only eligibility, enforces one action
  per slot via a per-slot mutex, queues spawns behind a per-kind concurrency semaphore
  (`DefaultMaxConcurrent = 1`), and applies the blocked-agent policy
  (`escalate`/`pause`/`auto`) with exact-match-only prompt answering.
- Handoff file support: `HERDR_LOOP_HANDOFF` env var per slot, YAML (`---`) or TOML (`+++`)
  front-matter parsing, dotted-path lookup for rule predicates and `{{field}}` template
  expansion.
- Structured `Escalation` type: every stall or failure records which rule fired (if any),
  the slot's last observed status, how long it had been stalled, and the approval prompt
  text when visible — routed through `notification.show`.
- `docs/demand-research.md`: the ecosystem evidence sweep behind PLAN.md §4c's adopted
  requirements.

**`cmd/herdr-loop` — the binary, and the loop actually running**
- The binary itself: `run`, `status`, `stop`, `validate`, `doctor`, and `probe` subcommands
  (`main.go`). `mapper.go` maps a parsed `internal/manifest.Manifest` onto a runnable
  `internal/engine.Config` — closing the gap the previous entry in this file left open, where
  the manifest's `run`/`spawn` actions had no engine-side implementation to reach.
- `run` actions: arbitrary argv, executed directly and never through a shell — a deliberate
  security boundary, since a run action's expanded `{{...}}` template values may contain text
  an agent wrote, and a shell would make that text executable. Branches on exit code via
  `on_success`/`on_failure`, with `{{stdout}}`/`{{stderr}}`/`{{exit_code}}`/`{{duration}}`
  available to the branch (output capped at 64 KB from the tail, marked when truncated). A
  nonzero exit, a timeout, and a command that could not start at all are kept as three
  distinct outcomes — the third escalates rather than silently branching as if the gate had
  simply failed.
- Startup keys: a kind may declare `startup_keys`/`startup_settle_ms` in `kinds.toml`, sent
  once via `pane.send_text` after the agent is confirmed interactive and before its first
  prompt. Exists because Claude Code launched in plan mode silently reinterprets "implement
  this" as "write a plan for this" — the agent reports working, finishes, reports done, and
  has changed nothing, and nothing in the lifecycle says why.
- One tab per slot by default (`placement = "tab"`) instead of splitting the supervisor's
  pane — splitting does not survive a fleet, since each extra slot halves a dimension and a
  coding agent's TUI degrades badly once narrow. `placement = "split"` is still available per
  slot. Slots can also pass per-slot `args` through to the harness's own command line
  (`args = ["--model", "sonnet"]`), the main lever on what a converge loop costs.
- Turn gating: a rule now only evaluates once a slot has completed a transition *out of*
  `working`, not merely reached `idle`. A freshly spawned slot is `idle` too, so without this
  a reviewer's gate could fire before the implementer had ever been given its task.
- `Config.Strict` (`loop.strict`) refusing to fire any rule against a screen-classified slot,
  and a real `pane.process_info` corroboration probe run before spending an iteration on an
  inferred status (PLAN.md §4.9) — both had been described in comments as enforced before
  this closed the gap between the description and the code.
- Per-slot progress and an append-only event log: `progress.json` (a snapshot of where every
  slot is right now, rewritten atomically) and `events.jsonl` (append-only, one JSON object
  per line, reopened rather than truncated on restart). Each slot reports its detection tier
  and how long it has been in its current status alongside the status itself. `status`
  renders both and points at the log; a stale `progress.json` beside a dead pid file is
  cleared rather than reported as live.
- `herdr-loop probe <kind>` / `probe --all`: runs one harness (or every kind installed) through
  the same lifecycle the engine runs — pane, shell, `agent.start`, addressable, prompt, turn
  end — timing each step and printing both the evidence and the `kinds.toml` stanza it
  implies. A kind with no binary installed is reported as absent, not as a failure.
- `kinds.toml`: measured, not assumed, capability data for four harnesses (see README.md's
  Harness support table). The probe immediately earned its keep by disproving a hand-written
  entry in an earlier draft of this file — opencode did not need the six-second settle it
  claimed; the real cause of the stalls it was covering for was slots sharing a squeezed
  split pane, fixed separately by tab-per-slot.

### Fixed
- Six defects found only by running a real loop against a real herdr session — invisible to
  unit tests, fakes, and adversarial review: shell readiness misjudged as "no foreground
  process" when herdr lists the pane's own shell among them; an `agent.start` race now
  retried only on `agent_pane_busy`; a name index that adopted agents this loop never
  started; a slot-to-pane mapping learned only from `agent.list`, tens of seconds later than
  the agent's own first transition; an unretried `agent_not_ready` window that failed the
  first prompt of every run; and `agent.prompt` called with no wait options, which reports
  delivery success whether or not the text ever reached the agent.
- The same status event arrives spelled two different ways: a per-pane subscription echoes
  the dotted `pane.agent_status_changed` (absent from the schema), while a global subscription
  delivers the underscored `pane_agent_detected`. Filtering on the documented spelling alone
  silently discarded every per-pane status event while the stream still looked healthy and
  drop counters read zero. Normalized at the `herdr-api` boundary and pinned by a test driving
  idle → working → done through the model.
- A freshly split pane is not immediately an available shell — `Spawn` now waits for
  `pane.process_info` to report the shell itself holding the foreground before calling
  `agent.start`, instead of failing `agent_pane_busy` on effectively every spawn.
- Two bugs surfaced while wiring up run actions: `dispatch` unconditionally unlocked a slot
  lock that is `nil` for an action owning no slot, and a loop-ending `finish` requested by an
  action that landed during the stream-close drain was discarded in favour of reporting
  `stream-closed` over the real outcome.

### Docs
- PLAN.md §13: the graphs-as-recursion direction. A node is either a slot or a nested loop;
  edges reuse the same predicate DSL and the same file-based handoff contract rules already
  use; a plain loop is a graph with one node. Recorded why this stays one plugin rather than
  two: every scarce resource it manages — per-kind concurrency tokens, the reconciled state
  model, the event subscription, worktree ownership — is global to the machine and has
  exactly one correct owner.
- README.md "Graphs" section, describing the same shape for readers who don't start at
  PLAN.md, and marked plainly as not yet runnable: `internal/graph` and
  `internal/manifest.ParseGraph` parse, validate and sequence a `graph.toml` and are tested,
  but nothing in `cmd/herdr-loop` calls either one, and nothing executes a nested loop.
- `examples/graph-two-loops.toml`: a commented, non-executing sketch of two composed loops
  (implement-and-review feeding integrate-and-verify) in the shape PLAN.md §13 describes. Its
  inline `[node.loop.slot]` syntax is illustrative and does not match `ParseGraph`'s real,
  tested grammar (`internal/manifest/graph_manifest_test.go`) — the two were written in
  parallel and diverged; the README's own graph.toml sketch uses the real grammar instead.
- Measured capability table for `pi`, `opencode`, `claude`, `codex` in README.md's "Harness
  support" section, generated from `herdr-loop probe` runs against herdr 0.8.0.

### Known gaps
- **Graph composition parses and validates but does not execute.** `internal/graph` and
  `internal/manifest.ParseGraph` are real and tested (PLAN.md §13), but no command in
  `cmd/herdr-loop` calls `ParseGraph`, and nothing spawns a nested loop or reports its outcome
  onto an edge. A loop — one flat set of slots and rules — is still the top of what can
  actually run end to end; see README.md's Graphs section for exactly where the line sits.
- **No `attach` TUI board** (PLAN.md M5). `status` is a one-shot read of `progress.json`, not
  a live view.
- **`engine.Teardown` exists, is tested, and enforces §4.10 — but `cmd/herdr-loop` never calls
  it.** `Config.TeardownOnFinish` and the check-before-`worktree.remove` invariant are real: a
  dirty tree cannot be force-removed from any code path, proven by
  `internal/engine/teardown_test.go`. What's still missing is the caller — neither `run`'s own
  finish path nor `stop` sets `TeardownOnFinish` or invokes `Teardown`, so today a real
  invocation of the binary leaves every worktree in place regardless of what git says about
  it. Wiring that in is the remaining gap, not the invariant itself.
- **`Strict` mode is honest but near-unusable as of herdr 0.8.0.** Only `pi` and `opencode`
  self-report, so a strict loop over Claude Code or Codex slots refuses to fire at all. The
  right fix is upstream — structured status reporting in those integrations — not a relaxation
  here.
- **Installing this as a herdr plugin is unverified.** Only direct invocation of the built
  binary (`go build ./cmd/herdr-loop`, then `./herdr-loop run …`) has driven a loop end to
  end; `herdr plugin install` / `plugin link` have not been exercised against this repo.
- Every kind beyond the four in the harness table works on conservative defaults but is
  unmeasured — `herdr-loop probe <kind>` is how that changes.
