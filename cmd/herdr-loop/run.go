package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	herdr "github.com/cyperx84/herdr-api"
	"github.com/cyperx84/herdr-loop/internal/engine"
	"github.com/cyperx84/herdr-loop/internal/manifest"
	"github.com/cyperx84/herdr-loop/internal/state"
)

// cmdRun is the supervisor entry point: the pane herdr-plugin.toml's
// [[panes]] board keeps alive for the lifetime of one loop (PLAN §2 — herdr
// owns this process's lifecycle, it dies with the server, and it is visible
// and steerable, which is the whole reason it runs as a pane rather than a
// detached [[startup]] daemon).
func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	// Defaults come from HERDR_LOOP_* env vars — herdr-plugin.toml's
	// [[actions]]/[[panes]] entries invoke `run` with no arguments, since
	// herdr's plugin docs document no {{config.x}} command-template
	// placeholder to fill one in with (checked, not assumed: see
	// herdr-plugin.toml's comment on the run action). [config] values reach
	// this process as HERDR_LOOP_<KEY> env vars instead, the same convention
	// herdr-sesh-bro's shipped, working manifest uses for its own prefix.
	// Flags still override the env, so `herdr-loop run` works standalone
	// from a plain shell too.
	kindsPath := fs.String("kinds", envOr("HERDR_LOOP_KINDS_FILE", "kinds.toml"), "kinds.toml path (optional — absent file is not an error)")
	teardown := fs.Bool("teardown", false, "on finish, close each slot's pane and remove the worktree this run created (never one named directly in the manifest, and never one holding uncommitted work)")
	reconcileSecs := fs.Int("reconcile-interval", envOrInt("HERDR_LOOP_RECONCILE_INTERVAL_SECS", int(state.DefaultReconcileInterval/time.Second)), "seconds between authoritative agent.list reconciles")
	logLevel := fs.String("log-level", envOr("HERDR_LOOP_LOG_LEVEL", "info"), "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var manifestPath string
	switch fs.NArg() {
	case 0:
		manifestPath = envOr("HERDR_LOOP_DEFAULT_MANIFEST", "loop.toml")
	case 1:
		manifestPath = fs.Arg(0)
	default:
		return errors.New("usage: herdr-loop run [flags] [manifest.toml]")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)}))

	reconcileEvery := time.Duration(*reconcileSecs) * time.Second
	if reconcileEvery <= 0 {
		reconcileEvery = state.DefaultReconcileInterval
	}

	opts := loopOptions{
		ManifestPath:   manifestPath,
		KindsPath:      *kindsPath,
		ReconcileEvery: reconcileEvery,
		Teardown:       *teardown,
		Log:            log,
		WriteRunState:  true,
	}
	_, err := runLoopFile(ctx, opts)
	return err
}

// loopOptions is everything runLoopFile needs. A struct rather than a long
// parameter list because a graph node supplies most of it from the graph's
// own settings, and positional arguments would make that call unreadable.
type loopOptions struct {
	ManifestPath   string
	KindsPath      string
	ReconcileEvery time.Duration
	Teardown       bool
	Log            *slog.Logger
	// WriteRunState records this loop in the state dir so `status` and `stop`
	// can find it. True for a top-level run; false for a loop running as a
	// graph node, where the graph owns that record and a nested loop
	// overwriting it would make `status` describe the wrong thing.
	WriteRunState bool
}

// runLoopFile runs one loop manifest to completion and reports its outcome.
//
// Extracted from cmdRun so a graph node can run a loop and branch on what it
// returned. The outcome is the value a graph edge's predicate reads — without
// it a node could only report "finished", which is not enough to decide what
// happens next.
func runLoopFile(ctx context.Context, opts loopOptions) (engine.Outcome, error) {
	manifestPath := opts.ManifestPath
	log := opts.Log
	reconcileEvery := opts.ReconcileEvery
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}
	m, err := manifest.Parse(data)
	if err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %s: %w", manifestPath, err)
	}
	res, err := mapManifest(m)
	if err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %s: %w", manifestPath, err)
	}

	client, err := herdr.Open()
	if err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}
	// Pinned protocol check (PLAN §4.13): refuse to run against a server this
	// client was not built for, with a clear message, rather than sending it
	// requests it may not understand.
	if err := client.CheckProtocol(ctx); err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}

	if err := resolveWorktrees(ctx, client, &res); err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}

	kinds, err := loadKinds(opts.KindsPath)
	if err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}
	res.Config.Kinds = kinds
	res.Config.TeardownOnFinish = opts.Teardown
	if err := checkKindCapacity(res.Config.Slots, kinds); err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}
	if opts.Teardown {
		// Say what it will touch before it runs. Teardown is the one
		// irreversible thing here, and a run that quietly removes checkouts
		// is a bad surprise even when it is what was asked for.
		var owned []string
		for _, sc := range res.Config.Slots {
			if sc.Worktree {
				owned = append(owned, fmt.Sprintf("%s (%s)", sc.Name, sc.CWD))
			}
		}
		if len(owned) == 0 {
			log.Info("teardown requested, but no slot has a worktree this run created; nothing will be removed")
		} else {
			log.Info("teardown on finish", "worktrees", strings.Join(owned, ", "),
				"note", "a worktree holding uncommitted work is preserved and escalated, never removed")
		}
	}

	rs := runState{
		LoopName:     res.Config.Name,
		ManifestPath: manifestPath,
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC(),
	}
	if err := writeRunState(rs); err != nil {
		// Not fatal to the loop itself — only to status/stop's ability to
		// find it. Warn loudly rather than silently degrading to
		// unsupervisable.
		log.Warn("could not persist run state; status/stop will not see this loop", "err", err)
	}
	defer func() {
		if err := clearRunState(); err != nil {
			log.Warn("could not clear run state", "err", err)
		}
	}()

	idx := newNameIndex(res.Config.Slots)
	sm := state.New(teeingSnapshotter{client: client, idx: idx}, state.Options{ReconcileEvery: reconcileEvery})

	bus, err := newEventBus(ctx, client)
	if err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}
	defer bus.Close()

	if err := goLive(ctx, sm, bus); err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}
	log.Info("state model live", "reconciles", sm.Stats().Reconciles)

	res.Config.OnSpawn = func(slot, paneID string) {
		idx.set(slot, paneID)
		log.Debug("slot mapped to pane", "slot", slot, "pane", paneID)
	}
	eng, err := engine.New(res.Config, client, modelAdapter{state: sm, idx: idx}, log)
	if err != nil {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}

	// Observability, not correctness: a loop that cannot publish progress
	// still runs. Losing the surface is bad; refusing to work because a file
	// would not open would be worse.
	prog, err := newProgressWriter(res.Config.Name, res.Config.MaxIterations)
	if err != nil {
		log.Warn("progress surface unavailable; the loop will run unwatchable", "err", err)
	}
	defer prog.Close()
	prog.Append(logEntry{Event: "loop_started", Detail: manifestPath})

	spawnAll(ctx, eng, res.Config.Slots, log)

	transitions := make(chan engine.Transition, 64)
	go feed(ctx, sm, bus, idx, eng, client, reconcileEvery, res.InitialPrompts, transitions, prog, log)

	outcome, err := eng.Run(ctx, transitions)
	if err != nil && !errors.Is(err, context.Canceled) {
		return engine.Outcome{}, fmt.Errorf("run: %w", err)
	}
	log.Info("loop finished", "reason", outcome.Reason, "rule", outcome.Rule,
		"iterations", outcome.Iterations, "escalations", len(outcome.Escalations))
	prog.Append(logEntry{
		Event: "loop_finished", Rule: outcome.Rule,
		Detail: fmt.Sprintf("%s (%d iteration(s), %d escalation(s))",
			outcome.Reason, outcome.Iterations, len(outcome.Escalations)),
	})
	for _, esc := range outcome.Escalations {
		log.Warn("escalation", "detail", esc.String())
		// §4.11: the escalation carries its reason into the log too, so the
		// history says why a slot stopped, not merely that it did.
		prog.Append(logEntry{
			At: esc.At, Event: "escalation", Slot: esc.Slot, Rule: esc.Rule,
			To: esc.Status, Detail: esc.Reason,
		})
	}
	return outcome, nil
}

// goLive drains the replayed event prefix and reconciles until state.Model
// reports live (PLAN §4.8). Nothing downstream is actionable before this —
// state.Model.Apply itself reports no transitions while !live — so run must
// not start spawning or feeding the engine until it returns.
//
// Reconcile's own doc says a non-empty session needs at least two passes
// (one to adopt the snapshot, one to confirm nothing moved since), which is
// why this loops rather than reconciling once and trusting it.
func goLive(ctx context.Context, sm *state.Model, bus *eventBus) error {
	for {
		// Drain whatever the bus has already buffered, without blocking,
		// before reconciling — comparing against a snapshot while known
		// events sit unapplied would manufacture a divergence that was
		// never really there (Reconcile's own doc, PLAN §4.8).
	drain:
		for {
			select {
			case ev, ok := <-bus.Events():
				if !ok {
					return errors.New("event stream closed before the model went live")
				}
				if ev.Kind == "pane_created" || ev.Kind == "pane_agent_detected" {
					if p := paneIDOf(ev); p != "" {
						bus.watch(ctx, p)
					}
				}
				sm.Apply(ev) // replayed prefix: no transition is actionable yet
			case <-ctx.Done():
				return ctx.Err()
			default:
				break drain
			}
		}

		rec, err := sm.Reconcile(ctx)
		if err != nil {
			return fmt.Errorf("initial reconcile: %w", err)
		}
		if rec.WentLive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// spawnAll brings every configured slot up in parallel. engine.Spawn queues
// each call against its own kind's concurrency gate (§4.7) internally, so
// this does not need its own throttle — the fan-out here just lets
// independent kinds start concurrently instead of waiting on each other.
func spawnAll(ctx context.Context, eng *engine.Engine, slots []engine.SlotConfig, log *slog.Logger) {
	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Add(1)
		go func(slot string) {
			defer wg.Done()
			if err := eng.Spawn(ctx, slot); err != nil {
				log.Error("spawn failed", "slot", slot, "err", err)
			}
		}(s.Name)
	}
	wg.Wait()
}

// resolveWorktrees materializes a checkout for every slot the manifest gave
// a worktree instead of a bare cwd, filling in the SlotConfig fields
// engine.Spawn needs before engine.New ever sees them.
//
// This is the one part of "load this manifest" that genuinely requires a
// live connection, which is why it is isolated here rather than folded into
// mapManifest: validate and doctor call mapManifest against a manifest with
// worktree slots and never reach this function, so they still work with no
// herdr connection at all (PLAN §4.2 requires one worktree per slot; this is
// where that requirement is actually satisfied for a live run).
func resolveWorktrees(ctx context.Context, client *herdr.Client, res *mapResult) error {
	for _, req := range res.Worktrees {
		branch := req.Branch
		params := herdr.WorktreeCreateParams{Branch: &branch}
		if req.Base != "" {
			base := req.Base
			params.Base = &base
		}
		created, err := client.WorktreeCreate(ctx, params)
		if err != nil {
			return fmt.Errorf("worktree.create for slot %q (branch %s): %w",
				res.Config.Slots[req.SlotIndex].Name, branch, err)
		}
		res.Config.Slots[req.SlotIndex].CWD = created.Worktree.Path
		res.Config.Slots[req.SlotIndex].WorkspaceID = created.Workspace.ID
		// Marks this slot's checkout as ours to remove. Set ONLY here, on the
		// path that just created the worktree, and never for a slot given a
		// bare cwd in the manifest: that directory is the user's own repo and
		// teardown must never be able to reach it. Both fields come from this
		// one `created` result, so the tree teardown checks for uncommitted
		// work is necessarily the same tree it would remove.
		res.Config.Slots[req.SlotIndex].Worktree = true
		// created.RootPane is the new workspace's own first pane, but
		// engine.Spawn always opens a second pane via pane.split rather than
		// accepting a caller-supplied one (see engine.go's Spawn) — so this
		// root pane is left idle. Known imperfection, not a silent drop:
		// letting Spawn reuse it belongs to the engine package, which this
		// CLI does not own.
	}
	return nil
}

// feed is the supervisor's single event-loop goroutine. state.Model requires
// Apply and Reconcile to be driven from one goroutine (its own package doc),
// so this is the only place either is called once run is past goLive.
func feed(
	ctx context.Context,
	sm *state.Model,
	bus *eventBus,
	idx *nameIndex,
	eng *engine.Engine,
	client *herdr.Client,
	reconcileEvery time.Duration,
	initialPrompts map[string]string,
	out chan<- engine.Transition,
	prog *progressWriter,
	log *slog.Logger,
) {
	defer close(out)
	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()

	handle := func(tr state.Transition) {
		// Every transition is logged before any decision about it. A
		// supervisor that silently discards the signal it exists to act on is
		// unfalsifiable from the outside — the failure mode this whole project
		// is about (§4.9), applied to itself.
		log.Debug("transition", "pane", tr.PaneID, "from", tr.From, "to", tr.To,
			"source", tr.Source, "gone", tr.Gone, "settled", tr.BecameSettled())
		if tr.Gone {
			bus.unwatch(tr.PaneID)
		}
		slot, known := idx.slotFor(tr.PaneID)
		if known && tr.Gone {
			// Return the kind's concurrency token. Spawn deliberately holds it
			// for the agent's lifetime, so without this every kind leaks a
			// permanent token and a second slot of the same kind blocks in
			// acquireKind until its context expires — with the default cap of
			// one, that hangs spawnAll's WaitGroup and the run never starts.
			// This is the exact point the model has established the agent left.
			eng.Release(slot)
		}
		if !known {
			// A pane this loop does not own — reconciliation is session-
			// wide, so seeing one is expected traffic, not an error.
			return
		}
		if tr.BecameSettled() {
			if text, pending := initialPrompts[slot]; pending {
				// Bootstrap, not a rule action: delivering a slot's initial
				// prompt happens exactly once, the same way Spawn does (see
				// mapResult.InitialPrompts' doc). This transition is
				// consumed here, not forwarded — the engine's rules are for
				// handoff between already-running slots, and treating "just
				// spawned, about to receive its kickoff prompt" as a
				// settled-and-idle slot would let a handoff rule fire on it
				// before it has done any work.
				//
				// Bypassing the rule engine does NOT license bypassing the
				// per-slot lock: this prompt reaches the same agent's input
				// widget as any rule-driven one, and AgentPrompt is a blocking
				// round-trip. Without the lock a reconcile-sourced transition
				// could fire a rule for this slot mid-delivery and race two
				// prompts into one agent — exactly what the lock exists to
				// stop. If the slot is busy, leave the prompt pending; the
				// next settled transition brings us back.
				lk, free := eng.TryLockSlot(slot)
				if !free {
					log.Debug("initial prompt deferred: slot busy", "slot", slot)
					return
				}
				delete(initialPrompts, slot)
				go func() {
					defer lk.Unlock()
					deliverInitialPrompt(ctx, eng, tr.PaneID, slot, text, log)
				}()
				// Deliberately NOT marking a turn here. Delivering the prompt
				// is not the turn — the work it causes is, and that shows up
				// as the agent leaving `working`, which the engine records on
				// its own.
				//
				// Marking it here made the slot rule-eligible immediately, so
				// the first settled transition after delivery fired the rules
				// before the agent had done anything. Observed as a gate
				// running four times in three seconds against untouched code,
				// burning the iteration budget while the agent was still
				// reading its task.
				return
			}
		}
		prog.Append(logEntry{
			At: tr.At, Event: "slot_status", Slot: slot,
			From: tr.From, To: tr.To, Detail: string(tr.Tier),
		})
		publishProgress(sm, idx, eng, prog)

		select {
		case out <- engine.Transition{Slot: slot, From: tr.From, To: tr.To, At: tr.At}:
		case <-ctx.Done():
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-bus.Events():
			if !ok {
				return
			}
			if ev.Kind == "pane_created" || ev.Kind == "pane_agent_detected" {
				if p := paneIDOf(ev); p != "" {
					bus.watch(ctx, p)
				}
			}
			if tr, ok := sm.Apply(ev); ok {
				handle(tr)
			}
		case <-ticker.C:
			rec, err := sm.Reconcile(ctx)
			if err != nil {
				log.Warn("reconcile failed", "err", err)
				continue
			}
			for _, tr := range rec.Transitions {
				handle(tr)
			}
		}
	}
}

// deliverInitialPrompt sends a slot's [[slot]].prompt exactly once, the
// first time that slot settles after being spawned. See feed's handle
// closure for why this bypasses the rule engine entirely.
func deliverInitialPrompt(ctx context.Context, eng *engine.Engine, target, slot, text string, log *slog.Logger) {
	// Through the engine, not the client directly: a just-spawned agent is
	// not addressable for a moment after it first reports idle, and this is
	// the prompt that always lands in that window.
	// SendPrompt does not corroborate a stalled report the way the rule path
	// does, so do it here too: the bootstrap prompt is the one most likely to
	// hit it, because a slot's first prompt lands on the coldest possible pane.
	if err := eng.SendPrompt(ctx, target, text); err != nil {
		if eng.PromptLanded(ctx, target) {
			// Not marking a turn: the agent is working, and leaving working
			// is what records the turn. Marking it here would make the slot
			// rule-eligible before it has done anything — the same mistake
			// that once had a gate firing four times in three seconds.
			log.Info("initial prompt reported stalled but the agent started working; treating as delivered", "slot", slot)
			return
		}
		log.Error("initial prompt failed", "slot", slot, "err", err)
		return
	}
	log.Info("initial prompt delivered", "slot", slot)
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// publishProgress rebuilds the per-slot snapshot from the authoritative model.
//
// Derived rather than accumulated: the model is the source of truth for slot
// state (PLAN §4.9), so recomputing from it cannot drift the way a separately
// maintained counter would.
func publishProgress(sm *state.Model, idx *nameIndex, eng *engine.Engine, prog *progressWriter) {
	if prog == nil {
		return
	}
	names := idx.slots()
	slots := make([]slotProgress, 0, len(names))
	for _, name := range names {
		sp := slotProgress{Slot: name, Halted: eng.Halted(name)}
		pane, ok := idx.paneFor(name)
		if !ok {
			slots = append(slots, sp)
			continue
		}
		sp.Pane = pane
		if ag, ok := sm.Get(pane); ok {
			sp.Status = ag.Status
			sp.Tier = ag.Tier
			sp.Since = ag.Since
			sp.Kind = ag.Kind
		}
		slots = append(slots, sp)
	}
	prog.Update(slots, eng.Iterations(), sm.IsLive(), eng.EscalationCount())
}
