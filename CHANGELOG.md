# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - Unreleased

Pre-alpha. Not yet installable or runnable — see the README's Status section for exactly
what exists. Verified against herdr 0.8.0, API protocol 19, schema_version 1.

### Added
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
  (never raw events), gates prompting behind settled-only eligibility (with a per-kind,
  currently-unmeasured `MidTurnInjection` override point), enforces one action per slot via
  a per-slot mutex, queues spawns behind a per-kind concurrency semaphore
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

### Known gaps
- No `cmd/` binary: none of `run`/`status`/`stop`/`attach`/`validate`/`doctor` exist as
  code.
- `internal/manifest.Manifest` and `internal/engine.Config` are not wired together; the
  manifest's `run`/`escalate` actions and the engine's `spawn` action don't reach each
  other yet.
- No `kinds.toml` — per-kind capability data is read from `engine.Config.Kinds` at runtime
  but nothing populates it from disk.
