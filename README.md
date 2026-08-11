# herdr-loop

[![CI](https://github.com/cyperx84/herdr-loop/actions/workflows/ci.yml/badge.svg)](https://github.com/cyperx84/herdr-loop/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![herdr-plugin](https://img.shields.io/badge/herdr--plugin-topic-6f42c1)](https://github.com/topics/herdr-plugin)

Declarative, event-driven multi-agent loops for [herdr](https://herdr.dev) — a `loop.toml`
file that turns herdr's single-agent pane primitives into a fleet: implement, review, gate,
retry, converge, across any mix of `claude`, `codex`, `opencode`, `pi`, and 17 other
harnesses herdr recognises.

```sh
herdr plugin install cyperx84/herdr-loop
```

One verified run, two vendors, one loop: **2 iterations, 0 escalations, reason
`"converged"`** — Claude Code wrote the fix, pi reviewed it, `go test` had the final word.
Full trace in [Recorded run](#recorded-run).

herdr gives you excellent control over one agent in one pane: spawn it, prompt it, wait for
it to settle, read what it did. It gives you nothing above that — no way to say "when the
implementer is done, hand its result to a reviewer," no retry, no until-converged, no
cross-agent result routing, no policy for an agent that hits an approval prompt unattended.
herdr's own docs are explicit: *"This is not a built-in orchestration loop."* herdr-loop is
that layer — a small TOML state machine reacting to herdr's push event stream — and it has
run real loops to convergence against real agents, not just parsed manifests in a test.

## Install

```sh
herdr plugin install cyperx84/herdr-loop
```

This has been taken seriously, not just written down — two install-time-only bugs surfaced
around it, neither of which `go build`, `go test`, or manifest validation would ever have
caught: one found by actually installing it (`5f1bdc1` — the status pane declared a `height`
on `split` placement, which herdr rejects the whole plugin manifest for at install time), one
caught by reading a sibling plugin's identical bug before the first install attempt
(`a8415de` — every action resolved `bin/herdr-loop` through `PATH` rather than the plugin
root, so nothing would have launched for anyone who installed it). Both are fixed on `main`,
but a clean install after both fixes hasn't been separately re-confirmed. `go.mod`'s
dependency on `herdr-api` also used to be a local `replace`
directive that only resolved on one machine — it now pulls the tagged
`github.com/cyperx84/herdr-api@v0.1.0` like any other module, closing the other reason a
fresh install would have failed.

One real gap: **herdr-loop itself has no tagged release yet.** `[[build]]`'s prebuilt-binary
fallback (for a machine with no Go toolchain) has placeholder checksums and refuses with "no
release published yet" until one exists — see `scripts/build.sh`. Until then, `herdr plugin
install` needs a local Go toolchain to build from source.

If you'd rather skip the plugin wrapper entirely, the binary runs standalone:

```sh
git clone https://github.com/cyperx84/herdr-loop.git && cd herdr-loop
go build -o herdr-loop ./cmd/herdr-loop
./herdr-loop validate examples/mixed-harness-converge.toml   # no herdr connection needed
./herdr-loop run examples/mixed-harness-converge.toml        # needs a running herdr session
```

## A real loop.toml

The full commented file is
[`examples/mixed-harness-converge.toml`](examples/mixed-harness-converge.toml), and it is
the loop the [recorded run](#recorded-run) below came from — every field here is exercised,
not aspirational.

```toml
[loop]
name             = "mixed-converge"
max_iterations   = 8
handoff_dir      = ".herdr-loop/handoff"
on_blocked       = "escalate"
allow_shared_cwd = true   # impl writes, review only reads

[[slot]]
name = "impl"
kind = "claude"
cwd  = "."   # set to your repo
args = ["--model", "sonnet"]
prompt = """
Reverse() in str.go is wrong for multi-byte characters. Fix it so every case in
str_test.go passes. Edit str.go directly. Reply DONE when finished.
"""

[[slot]]
name = "review"
kind = "pi"
cwd  = "."   # set to your repo

[[rule]]
when = { op = "all", filters = [
  { op = "eq", field = "slot",   value = "impl" },
  { op = "in", field = "status", values = ["idle", "done"] },
] }
then = { prompt = { slot = "review", text = "Read str.go. If Reverse handles multi-byte runes correctly, reply exactly CLEAN. Otherwise name the one problem in a single line." } }

[[rule]]
when = { op = "all", filters = [
  { op = "eq", field = "slot",   value = "review" },
  { op = "in", field = "status", values = ["idle", "done"] },
] }
then = { run = ["go", "test", "./..."], on_success = { finish = "converged" }, on_failure = { prompt = { slot = "impl", text = "Tests still fail. Fix str.go.\n\n{{stdout}}" } } }
```

Reading it top to bottom:

- **`[loop]`** — `max_iterations = 8` bounds the retrigger cycle below (impl and review can
  hand work back and forth; a rule cycle with no cap fails to parse at all). `handoff_dir`
  is where each slot could write a result file, relative to the repo root. `on_blocked =
  "escalate"` is actually the default — written explicitly here so the behaviour is visible
  in the file. `allow_shared_cwd = true` is the interesting one: both slots use a plain `cwd`
  instead of a worktree, which `internal/manifest` rejects by default (see
  [Safety model](#safety-model)) because two agents in one working tree produces phantom
  commits. It's allowed here on purpose — `review` never writes, so there is no writer
  collision to guard against, and the manifest has to say so explicitly rather than getting
  it for free.
- **`impl` slot** — `kind = "claude"`, `args = ["--model", "sonnet"]` pins a cheap model:
  every iteration of a converge loop is a real invocation, and the model is the biggest lever
  on what that costs. The prompt is literal task text, not a `{{task}}` template — manifest
  variables (`engine.Config.Vars`) exist in the engine but nothing populates one from
  `loop.toml` or a CLI flag yet, so a real loop writes the task directly into the prompt, as
  this one does.
- **`review` slot** — `kind = "pi"`, no model pin: pi is the cheapest and fastest harness
  measured (see [Harness support](#harness-support)), so this slot doesn't need one.
- **First `[[rule]]`** — predicates reuse herdr's own `AgentViewFilter` grammar
  (`all`/`any`/`not`/`eq`/`in`/`exists`), not a second condition language. It fires once
  `impl` has settled to `idle` or `done` — deliberately not `working`: prompting a mid-turn
  agent is unmeasured per kind and off by default. The action hands `review` a literal
  instruction to read `str.go` and answer `CLEAN` or name the one problem — no handoff-file
  indirection needed here because the instruction fits in the prompt.
- **Second `[[rule]]`** — fires once `review` settles, and runs `go test ./...` directly
  (never through a shell, so nothing an agent wrote can become executable). `on_success`
  finishes the loop with reason `"converged"`; `on_failure` reprompts `impl` with the
  captured `{{stdout}}`, closing the retrigger cycle `max_iterations` above exists to bound.
  A test run gets the final word over either agent's opinion that the work is done.

## Recorded run

Verified end to end against herdr 0.8.0. Claude Code and pi are two different vendors on two
different panes, coordinated through one loop, with a mechanical gate — not either agent's
self-report — deciding when to stop:

```
impl   idle -> working      kickoff prompt
impl   working -> idle      turn finished  (screen-classified)
review idle -> working      <- the handoff, cross-vendor
review working -> idle      turn finished  (structured)
gate   go test ./...        exit 0 -> converged
```

**2 iterations, 0 escalations, reason `"converged"`**, and the diff `impl` produced was the
correct fix. Nothing here is a mock or a fake client — same `herdr-api` transport, real
panes, a real `go test` subprocess.

## Graphs

A node is either a slot (one agent) or a nested loop (a whole converging fleet); edges
branch on how the previous node finished, using the same predicate grammar rules already
use. This is not a sketch — `herdr-loop graph` executes it, sequentially, one node's loop
running to completion before the next is activated.

[`examples/graph-fix-then-verify.toml`](examples/graph-fix-then-verify.toml), verified end
to end:

```toml
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

# Only run the verify loop if the fix loop actually converged.
[[edge]]
from = "fix"
when = { op = "eq", field = "outcome", value = "converged" }
then = { activate = "verify" }
```

```sh
herdr-loop graph --kinds kinds.toml graph.toml
```

Recorded run: `fix` spawned pi, the agent fixed a real bug, the `go test` gate passed, the
node settled `"converged"`. The edge's `outcome == "converged"` predicate held, so `verify`
activated: it spawned pi again, the `go vet` gate passed, the node settled `"clean"`. Two
activations of a budget of six.

`fix.toml` and `verify.toml` are ordinary `loop.toml` files — nothing about a loop manifest
knows it's running inside a graph, which is the point: a plain loop is a graph with one
node, so every existing manifest composes without being rewritten. They aren't checked into
`examples/` (same convention as `cwd = "."   # set to your repo` above — point them at your
own fix-and-gate loop).

Two honest limits on this today: nodes run **one at a time**, never concurrently — two loops
running at once would share the machine's per-kind concurrency budget with no broker between
two separate engine instances to enforce it, so sequential is the correct thing to ship
first, not a shortcut. And a node that names a bare `slot` instead of a `loop` fails
explicitly (`only loop nodes can execute today`) rather than silently doing nothing — there's
no engine instance yet to host a single agent outside a loop.

## Harness support

Four harnesses are measured and exercised in real loops. Probe results against herdr 0.8.0:

| kind | detection | interactive | first turn | notes |
|---|---|---|---|---|
| `pi` | structured | 3.13s | 0.50s | fastest; best default for a reviewer slot |
| `opencode` | structured | 3.12s | 33.54s | slow turns; keyless on free models |
| `claude` | screen | 3.02s | 2.51s | needs plan-mode disarm, or it plans instead of building |
| `codex` | screen | 3.02s | 1.00s | asks to trust a new directory on first run |

**Structured** means the agent reports its own lifecycle state. **Screen** means herdr infers
it from the rendered terminal — a heuristic, and one `strict` mode refuses to act on. As of
herdr 0.8.0 a strict loop therefore runs on `pi` and `opencode` only.

Every other kind herdr recognises (`gemini`, `cursor`, `amp`, `droid`, `kimi`, `grok`,
`hermes`, `copilot`, `kilo`, `qodercli`, `maki`, `cline`, `agy`, `omp`, `devin`, `kiro`,
`mastracode`) should work and inherits conservative defaults, but none has been probed. Run
`herdr-loop probe <kind>` and add what it prints.

## Measuring a harness

Loop orchestration lives or dies on per-harness behaviour: whether an agent self-reports its
status or gets classified from the screen, how long before it can take a prompt, whether it
blocks on first run, whether it finishes a turn at all. None of that is documented anywhere,
and it differs per harness and per version. So measure it rather than believe it:

```sh
herdr-loop probe pi                          # one harness
herdr-loop probe claude --args "--model sonnet"
herdr-loop probe --all                       # every kind installed here
```

Each probe runs the harness through the same lifecycle the engine does — create the pane,
wait for its shell, start the agent, wait for it to become addressable, prompt it, watch for
the turn to finish — and prints both what happened and the `kinds.toml` stanza that follows
from it. `--all` adds a compatibility matrix across every kind installed on the machine. It
costs one real agent invocation per kind, so pin a cheap model with `--args` on anything that
bills.

This exists because hand-measured capability tables are wrong in ways nobody notices. One
entry in this repo's own `kinds.toml` claimed opencode needed a six-second settle; the probe
showed it needs none, and that the stalls behind that entry were caused by something else
entirely (a squeezed split pane, fixed separately by giving each slot its own tab).

## Safety model

Requirements the manifest validator and the engine enforce, not conventions you're trusted
to follow. Full evidence and the incidents that motivated each one are in PLAN.md §4.

**Escalate by default.** `blocked` means herdr recognised an approval or question UI. An
orchestrator that blind-fires "enter" on that is an agent granting itself permission. Three
policies, `escalate` being the default:

| `on_blocked` | Behaviour |
|---|---|
| `escalate` (default) | Notify, halt that slot, keep going otherwise. |
| `pause` | Notify, halt the whole loop. |
| `auto` | Answer only prompts in an explicit `[[blocked_rule]]` list, matched by **exact string**, whitespace-trimmed — no wildcards, no regex, no substring. Anything unmatched still escalates. `on_blocked = "auto"` with an empty or wildcard-containing `blocked_rule` list is a parse error, not a silent no-op. |

**Worktree per slot, by default.** Two agents committing in one working tree produces
amend-becomes-new-commit, phantom modifications, and reflog entries nobody wrote.
`internal/manifest` rejects two slots sharing a `cwd` unless the manifest explicitly opts in
with `loop.allow_shared_cwd = true` — the escape hatch the worked example above uses, because
one of its two slots never writes.

**Teardown never removes uncommitted work.** `herdr-loop run --teardown` (and `herdr-loop
graph --teardown`) close each slot's pane and remove the worktree that run created when the
loop finishes — but only a worktree this run owns, never one named directly by `cwd` in the
manifest, and never one `git` reports as dirty. A dirty tree is preserved and escalated
instead of removed, an invariant `internal/engine/teardown_test.go` pins directly: no code
path can force-remove a tree with uncommitted changes. Teardown is opt-in and off by default
— a run with no `--teardown` flag leaves every worktree in place regardless of outcome.

**Detection tier is a runtime fact, not a config constant.** herdr learns an agent's
lifecycle status two different ways: some kinds (`pi`, `opencode`, measured on herdr 0.8.0)
self-report structurally; others (`claude`, `codex`) are classified from the rendered screen
— a heuristic. `internal/state.Tier` reads this per agent from `screen_detection_skipped` on
every reconcile, never assumes it from the kind name. A screen-classified `done` is not
treated as equivalent grounds for irreversible work as a structured one, and `loop.strict`
refuses to fire any rule against a screen-classified slot at all.

**The event stream replays history — nothing acts on it until reconciled.**
`events.subscribe` replays event backlog (including exit events for panes that died hours
ago) before it streams anything live — undocumented upstream, verified experimentally.
`internal/state.Model` drains that replay silently and only starts reporting actionable
transitions once a fresh `agent.list` agrees with what it reconstructed. The rule engine
never sees a raw event, only a transition the model has already corroborated. A related trap
the client also had to close: a per-pane subscription echoes the dotted
`pane.agent_status_changed`, absent from the schema, while a global subscription delivers the
underscored `pane_agent_detected` — filtering on the documented spelling alone silently
discarded every per-pane status event with the drop counters reading zero.

**Only settled agents get prompted.** `agent.prompt` tracks lifecycle state, not turn
boundaries — where text lands on an already-`working` agent is an unmeasured, per-kind
unknown. Rules fire only against slots that have completed a transition out of `working`;
mid-turn injection is a capability flag (`KindConfig.MidTurnInjection`), off until someone
measures it per kind.

**`unknown` is never treated as done.** An unclassifiable agent has not been shown to be
finished; it's excluded from every implicit "settled" set.

**Per-kind concurrency limits guard OAuth credentials.** N concurrent agents of an
OAuth-authenticated kind can race a single rotating token with no lock. The engine queues
spawns behind a per-kind counting semaphore (`max_concurrent = 1` for anything unmeasured)
rather than firing them in parallel.

**Every escalation carries a structured reason** — which rule fired (or didn't), the slot's
last observed status, how long it had been stalled, and the approval text if the model could
see it. Never a bare timeout.

## Limitations, stated plainly

- **Graph nodes run sequentially, never concurrently**, and a node that names a bare `slot`
  instead of a nested `loop` fails explicitly rather than executing anything — see
  [Graphs](#graphs).
- **`strict` mode is honest but near-unusable as of herdr 0.8.0.** Only `pi` and `opencode`
  self-report, so a strict loop over Claude Code or Codex slots refuses to fire at all. The
  right fix is upstream — structured status reporting in those integrations — not a
  relaxation here.
- **Mid-turn prompt injection is unmeasured for every kind.** Rules only act on settled
  agents until a kind's `KindConfig.MidTurnInjection` is actually measured and set.
- **Manifest variables aren't wired.** `engine.Config.Vars` exists and templates can read it,
  but nothing populates one from `loop.toml` or a flag yet — write literal prompt text, as
  the worked example does, not `{{task}}`.
- **17 of 21 recognised kinds are unprobed** and run on conservative defaults. `herdr-loop
  probe <kind>` is how that changes.
- **External gates poll; agent lifecycle doesn't.** herdr pushes agent status changes over
  `events.subscribe`. It has no event for "PR merged," "CI green," or "file appeared" — those
  need polling on a declared interval. "Event-driven" does not mean zero polling anywhere.
- **Windows cross-compiles, unverified against a real install.** `GOOS=windows GOARCH=amd64
  go build ./...` passes in CI for both this repo and `herdr-api`, but nobody has run the
  binary against a real Windows herdr install.
- **No `attach` TUI.** `status` is a one-shot read of `progress.json`, not a live view.
- **Not a general workflow engine, on purpose.** Three nouns (slot/rule/loop, or
  node/edge/graph one level up), the predicate set herdr already ships, and
  `run`-with-exit-code-branching. No embedded scripting language — adding one is explicitly
  deferred until a concrete rule can't be expressed without it.

## Links

- [PLAN.md](PLAN.md) — design rationale, the evidence behind every safety requirement above,
  and open questions.
- [CHANGELOG.md](CHANGELOG.md) — what shipped, what broke and got fixed, and the current
  known-gaps list in the project's own words.
- [CONTRIBUTING.md](CONTRIBUTING.md) — what needs care before changing `internal/manifest` or
  `internal/engine`.
- [herdr-api](https://github.com/cyperx84/herdr-api) — the Go client this repo is built on,
  and the source of the two undocumented protocol facts this README relies on.

## Development

```sh
git clone https://github.com/cyperx84/herdr-loop.git
cd herdr-loop
go build ./...
go vet ./...
go test -race ./...
```

CI runs `gofmt`, `go vet`, `go test -race`, validates every file in `examples/` against the
built binary's own `validate` command, and cross-compiles to darwin/arm64, darwin/amd64,
linux/amd64, linux/arm64, and windows/amd64 (`.github/workflows/ci.yml`).

## License

[MIT](LICENSE)
