# Contributing

Read [PLAN.md](PLAN.md) first. It is the design document, not background reading — every
hard invariant enforced in `internal/manifest` and `internal/engine` traces back to a
numbered section there (most under §4), and a change that contradicts one needs to either
update PLAN.md's reasoning or be wrong.

## Development setup

herdr-loop depends on `github.com/cyperx84/herdr-api` via a `replace` directive in go.mod,
because herdr-api isn't published yet. Clone both as siblings:

```sh
git clone https://github.com/cyperx84/herdr-loop.git
git clone https://github.com/cyperx84/herdr-api.git
cd herdr-loop
go build ./...
go vet ./...
go test ./...
```

If `../herdr-api` isn't there, every build fails at the `replace` resolution — that's
expected, not a bug to work around by vendoring or removing the directive.

## What needs care

- **Validate at parse time, not at runtime.** `internal/manifest`'s whole job is catching a
  config error before a loop starts — a rule cycle with no retry cap, two slots sharing a
  `cwd`, an `on_blocked = "auto"` with a wildcard pattern. If you're adding a new manifest
  field, ask whether a bad value should fail `Parse` rather than fail at 3am unattended.
- **The engine trusts the reconciled model, never a raw event.** `internal/state.Model`
  exists specifically to drain `events.subscribe`'s replayed history and repair dropped
  events before anything is actionable (PLAN.md §4.8). Do not add a code path in
  `internal/engine` that reacts to a `herdr.Event` directly — route everything through a
  `state.Transition`.
- **Every safety requirement in PLAN.md §4 needs a test that would fail if it regressed.**
  `internal/manifest/manifest_test.go`'s cycle-detection cases and
  `internal/engine/engine_test.go`'s blocked-policy/settled-only-prompting cases are the
  pattern — assert the invariant, not a frozen snapshot of current output.
- **`gofmt` and `go vet` clean, always.** Doc comments explain *why* a shape is what it is,
  especially where a protocol quirk (§4.6, §4.8, §4.12) forced it — match the voice already
  in `internal/state/model.go` and `internal/engine/engine.go`.
- **No speculative abstraction.** PLAN.md §3 explicitly cuts an embedded scripting engine
  from v1 "until a concrete rule can't be expressed" — that principle applies broadly.
  Don't add a hook, callback, or extension point with no concrete caller.

## Manifest and engine are not wired together yet

`internal/manifest.Manifest` (what `loop.toml` parses into) and `internal/engine.Config`
(what the rule engine runs) are independent types today, with a real vocabulary gap — the
manifest's `run`/`escalate` actions have no engine implementation, and the engine's `spawn`
action has no manifest syntax. If you're working on that translation layer, say so in the
PR description; it's PLAN.md §8's M2/M3 boundary and worth coordinating on rather than
duplicating.

## Examples must parse

Anything added to `examples/` is expected to pass `internal/manifest.Parse` — that is the
whole point of shipping it as documentation. Before sending a manifest example, confirm it
parses (a throwaway `go run` against `internal/manifest` is enough; there is no `validate`
CLI yet) and, if it demonstrates a rejection (like the uncapped-cycle case), confirm it
actually gets rejected with the error message you're describing.

## License

Contributions are made under the [MIT license](LICENSE) that covers the rest of the
repository.
