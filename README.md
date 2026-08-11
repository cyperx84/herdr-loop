# herdr-loop

Multi-harness loop orchestration for [herdr](https://herdr.dev) — a `loop.toml` manifest
that turns herdr's pane/agent primitives into declarative, event-driven fleets across any
mix of coding agents (claude, codex, opencode, pi, and 17 others herdr recognises).

> Full design rationale, evidence, and open questions live in [PLAN.md](PLAN.md) — read it
> before filing an issue that questions a decision documented there.

## Status: pre-alpha, not yet runnable end to end

Read this before anything else below, because most of what this README describes is the
*target* shape, not something you can `herdr plugin install` today.

**Built and tested:**
- `internal/manifest` — `loop.toml` parsing and validation: slot/predicate/action shape
  checking, the one-worktree-per-slot rule, the `on_blocked = "auto"` exact-pattern
  whitelist, and parse-time rule-cycle detection (Tarjan's algorithm over the slot
  data-flow graph — a manifest with an uncapped retrigger loop fails to parse).
- `internal/state` — a reconciled agent-state model that drains `events.subscribe`'s
  replayed history before anything is treated as live, then repairs itself against
  `agent.list` on a schedule so a dropped event self-heals instead of going unnoticed.
- `internal/engine` — the rule fold: evaluates predicates against reconciled state (never
  raw events), gates every effect behind settled-only prompting, a per-slot mutex, a
  per-kind concurrency limit, and the blocked-agent policy; reads/writes handoff files with
  YAML/TOML front-matter.

All three have real test suites (`go test ./...` is green, gofmt/vet clean) and are wired
against `github.com/cyperx84/herdr-api`, not mocked out.

**Not built yet:**
- **No `cmd/herdr-loop` binary.** The package has no `func main()` yet, so it doesn't
  build into an executable — none of `herdr-loop run|status|stop|attach|validate|doctor`
  work. You cannot currently run a loop, only parse and unit-test its manifest. (This
  package is under active development; check `go build ./...` for the current state
  rather than trusting this line indefinitely.)
- **`herdr-plugin.toml` describes the intended plugin surface but cannot install.** The
  manifest is in the repo (matching PLAN.md §7's build/actions/panes/config design), but
  its `[[build]]` step compiles `./cmd/herdr-loop`, which has no source yet — so
  `herdr plugin install cyperx84/herdr-loop`, shown below as the target install command,
  does not work until that lands.
- **The manifest and engine packages are not wired together.** `internal/manifest.Manifest`
  (what `loop.toml` parses into) and `internal/engine.Config` (what the rule engine
  actually runs) are separate types with a real vocabulary gap: the manifest's `run` action
  (arbitrary command, branch on exit code) and `escalate` action have no engine-side
  implementation yet, and the engine's `spawn` action has no `loop.toml` syntax to reach it
  from. Closing that gap is the next milestone (PLAN.md §8, M2/M3).
- **`kinds.toml`** (per-kind capability data — settled states, mid-turn injection,
  concurrency caps) does not exist as a file; the engine's `KindConfig` type exists and is
  read at runtime, but nothing populates it from disk yet.

The examples and the worked example below describe exactly what `internal/manifest.Parse`
accepts today — every example in this repo is checked against the real parser, not
hand-waved. What they do *not* claim is that you can run one.

## The problem

herdr gives you excellent single-step control over a coding agent in a pane — spawn it,
prompt it, wait for it to settle, read what it did. It gives you nothing for a *fleet*:
no way to say "when the implementer is done, hand its result to a reviewer," no retry, no
until-converged, no cross-agent result routing, and no policy for what happens when an
agent hits an approval prompt unattended. herdr's own docs are explicit: *"This is not a
built-in orchestration loop."*

herdr-loop is the layer above that: a small, TOML-declared state machine — **slot** (a
named seat for an agent), **rule** (`when <predicate> then <action>`), **loop** (slots +
rules + a termination condition) — that reacts to herdr's push event stream instead of
polling, and treats agent output as untrusted until corroborated against authoritative
state.

## Install

Once the plugin wrapper exists (see Status above):

```sh
herdr plugin install cyperx84/herdr-loop
```

Until then, the only way to exercise this repo is `go test ./...` and reading a manifest
through `internal/manifest.Parse` yourself — there is no installable artifact.

## A worked example: two-slot review loop

The full commented file is [`examples/simple-review-loop.toml`](examples/simple-review-loop.toml).

```toml
[loop]
name        = "two-slot-review"
handoff_dir = ".herdr-loop/handoff"

[[slot]]
name     = "impl"
kind     = "claude"
worktree = { branch = "loop/impl", base = "main" }
prompt   = "Implement {{task}}. Write your result to $HERDR_LOOP_HANDOFF."

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

Reading it top to bottom:

- **`[loop]`** — `handoff_dir` is where every slot's result file lands, relative to the
  repo root. `on_blocked` is omitted, so it defaults to `escalate` (see Safety model
  below). `max_iterations` is also omitted — legal here because this manifest has no rule
  cycle to bound (impl only ever triggers review; nothing sends work back).
- **`[[slot]]`** — `impl` and `review` each get their own git worktree
  (`worktree.branch`/`worktree.base`), never a shared `cwd`. That is enforced at parse
  time, not left to convention: two slots naming the same `cwd` fail validation unless the
  manifest sets `loop.allow_shared_cwd = true`.
- **First `[[rule]]`** — predicates reuse herdr's own `AgentViewFilter` grammar
  (`all`/`any`/`not`/`eq`/`in`/`exists`) rather than inventing a second condition language.
  This one fires once `impl`'s reconciled status is `idle` or `done` — deliberately not
  `working`: prompting a mid-turn agent is a per-kind unmeasured capability, off by default
  (see Limitations). The action sends `review` a prompt referencing
  `{{impl.handoff}}` — the path to `impl`'s result file, not its contents; the review agent
  reads the file itself.
- **Second `[[rule]]`** — `review.handoff.verdict` reads a typed field out of `review`'s
  handoff front-matter (a `verdict: clean` line under a `---`/`+++` block at the top of the
  file it wrote). If it matches, the loop finishes with reason `"converged"`. If `review`
  never writes a `verdict` field, this rule simply never holds — no error, no accidental
  finish.

[`examples/converge-until-clean.toml`](examples/converge-until-clean.toml) extends this to
a closed loop: `review` sending `changes-requested` prompts `impl` again, which makes
`impl <-> review` a genuine cycle — so that manifest *must* set `loop.max_iterations`, or
`internal/manifest.Parse` rejects it outright. It also shows the `run` escape hatch
(arbitrary command, branch on exit code via `on_success`/`on_failure`) gating the finish
line behind a mechanical check rather than trusting the review verdict alone.

## Safety model

These aren't caveats bolted onto a README — they're requirements the manifest validator
and the engine enforce, not conventions you're trusted to follow. Full evidence and the
incidents that motivated each one are in PLAN.md §4.

**Escalate by default.** `blocked` means herdr recognised an approval or question UI. An
orchestrator that blind-fires "enter" on that is an agent granting itself permission. Three
policies, `escalate` being the default:

| `on_blocked` | Behaviour |
|---|---|
| `escalate` (default) | Notify, halt that slot, keep going otherwise. |
| `pause` | Notify, halt the whole loop. |
| `auto` | Answer only prompts in an explicit `[[blocked_rule]]` list, matched by **exact string**, whitespace-trimmed — no wildcards, no regex, no substring. Anything unmatched still escalates. `on_blocked = "auto"` with an empty or wildcard-containing `blocked_rule` list is a parse error, not a silent no-op. |

**Worktree per slot, always.** Two agents committing in one working tree produces
amend-becomes-new-commit, phantom modifications, and reflog entries nobody wrote — a
failure class this machine's own history has hit before. `internal/manifest` rejects two
slots sharing a `cwd` unless the manifest explicitly opts out with
`loop.allow_shared_cwd = true`.

**Detection tier is a runtime fact, not a config constant.** herdr learns an agent's
lifecycle status two different ways: some kinds (`pi`, `opencode`, measured on herdr 0.8.0)
self-report structurally; others (`claude`, `codex`) are classified from the rendered
screen — a heuristic. `internal/state.Tier` reads this per agent from
`screen_detection_skipped` on every reconcile, never assumes it from the kind name, because
the same kind can differ by version and by whether an integration hook is installed. A
screen-classified `done` is not treated as equivalent grounds for irreversible work as a
structured one.

**The event stream replays history — nothing acts on it until reconciled.**
`events.subscribe` replays event backlog (including exit events for panes that died hours
ago) before it streams anything live — undocumented upstream, verified experimentally.
`internal/state.Model` drains that replay silently and only starts reporting actionable
transitions once a fresh `agent.list` agrees with what it reconstructed. The rule engine
never sees a raw event, only a transition the model has already corroborated.

**Only settled agents get prompted.** `agent.prompt` tracks lifecycle state, not turn
boundaries — where text lands on an already-`working` agent is an unmeasured, per-kind
unknown. Rules fire only against `idle`/`done`/`blocked` slots; mid-turn injection is a
capability flag (`KindConfig.MidTurnInjection`), off until someone measures it per kind.

**`unknown` is never treated as done.** An unclassifiable agent has not been shown to be
finished; it's excluded from every implicit "settled" set.

**Per-kind concurrency limits guard OAuth credentials.** N concurrent agents of an
OAuth-authenticated kind can race a single rotating token with no lock. The engine queues
spawns behind a per-kind counting semaphore (`DefaultMaxConcurrent = 1` for anything
unmeasured) rather than firing them in parallel. herdr-loop does not attempt to broker or
serialize the credential refresh itself — that's upstream's bug and out of scope; the
mitigation here is simply not creating the race.

**Every escalation carries a structured reason** — which rule fired (or didn't), the slot's
last observed status, how long it had been stalled, and the approval text if the model
could see it. Never a bare timeout.

## Limitations, stated plainly

- **Not runnable yet.** See Status above — there is no `cmd/herdr-loop` source, the plugin
  manifest's `[[build]]` step has nothing to build, and the manifest/engine wiring is
  incomplete. This README documents the design and the pieces that exist, not a working
  tool.
- **Not a general workflow engine, on purpose.** Three nouns (slot/rule/loop), the
  predicate set herdr already ships, and `run`-with-exit-code-branching. No embedded
  scripting language (rhai/lua/starlark) — adding one is explicitly deferred until a
  concrete rule can't be expressed without it.
- **External gates poll; agent lifecycle doesn't.** herdr pushes agent status changes over
  `events.subscribe`. It has no event for "PR merged," "CI green," or "file appeared" —
  those need polling on a declared interval. Do not read "event-driven" as "zero polling
  anywhere."
- **Mid-turn prompt injection is unmeasured for every kind.** Until a kind's behaviour here
  is actually measured and its `KindConfig.MidTurnInjection` flag set, rules only act on
  settled agents.
- **Windows support is unverified.** `GOOS=windows GOARCH=amd64 go build ./...` passes for
  both this repo and `herdr-api`, so the internal packages cross-compile, but there is no
  binary to run and nobody has exercised either against a real Windows herdr install.
- **`kinds.toml` does not exist.** Per-kind capability data (concurrency caps, settled
  states, mid-turn injection) is read from `engine.Config.Kinds` at runtime; nothing
  currently populates that from a file, so every kind runs on the conservative defaults.

## Development

```sh
git clone https://github.com/cyperx84/herdr-loop.git
git clone https://github.com/cyperx84/herdr-api.git   # sibling checkout — see go.mod
cd herdr-loop
go build ./...
go vet ./...
go test ./...
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the sibling-checkout requirement and what each
package's tests actually need to cover.



## Harness support

Four harnesses are measured and exercised in real loops. Probe results against
herdr 0.8.0:

| kind | detection | interactive | first turn | notes |
|---|---|---|---|---|
| `pi` | structured | 3.13s | 0.50s | fastest; best default for a reviewer slot |
| `opencode` | structured | 3.12s | 33.54s | slow turns; keyless on free models |
| `claude` | screen | 3.02s | 2.51s | needs plan-mode disarm, or it plans instead of building |
| `codex` | screen | 3.02s | 1.00s | asks to trust a new directory on first run |

**Structured** means the agent reports its own lifecycle state. **Screen** means
herdr infers it from the rendered terminal — a heuristic, and one `strict` mode
refuses to act on. As of herdr 0.8.0 a strict loop therefore runs on `pi` and
`opencode` only.

Every other kind herdr recognises should work and inherits conservative
defaults, but none has been probed. Run `herdr-loop probe <kind>` and add what
it prints.

## Measuring a harness

Loop orchestration lives or dies on per-harness behaviour: whether an agent
self-reports its status or gets classified from the screen, how long before it
can take a prompt, whether it blocks on first run, whether it finishes a turn
at all. None of that is documented anywhere, and it differs per harness and per
version.

So measure it rather than believe it:

```sh
herdr-loop probe pi                     # one harness
herdr-loop probe claude --args="--model sonnet"
herdr-loop probe --all                  # every kind installed here
```

Each probe runs the harness through the same lifecycle the engine does —
create the pane, wait for its shell, start the agent, wait for it to become
addressable, prompt it, watch for the turn to finish — and prints both what
happened and the `kinds.toml` stanza that follows from it. `--all` adds a
compatibility matrix across every kind installed on the machine.

It costs one real agent invocation per kind, so pin a cheap model with
`--args` on anything that bills.

This exists because hand-measured capability tables are wrong in ways nobody
notices. One entry in this repo's own `kinds.toml` claimed opencode needed a
six-second settle; the probe showed it needs none, and that the stalls behind
that entry were caused by something else entirely.

## License

[MIT](LICENSE)
