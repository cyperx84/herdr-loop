package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cyperx84/herdr-loop/internal/engine"
	"github.com/cyperx84/herdr-loop/internal/graph"
	"github.com/cyperx84/herdr-loop/internal/manifest"
	"github.com/cyperx84/herdr-loop/internal/state"
)

// `herdr-loop graph <graph.toml>` runs a graph: each node is a whole loop, and
// edges decide which loop runs next based on how the last one ended.
//
// The graph layer already modelled and validated this. What it could not do
// was execute — a node could be activated and settled by a caller, but nothing
// ran the loop behind it. This is that caller.
//
// Nodes run one at a time. Concurrent nodes are a real want and a separate
// change: two loops running at once share the machine's per-kind concurrency
// budget, and honouring that across independent engine instances needs a
// broker neither of them has. Sequential is the honest thing to ship first.
func cmdGraph(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	kindsPath := fs.String("kinds", envOr("HERDR_LOOP_KINDS_FILE", "kinds.toml"), "kinds.toml path")
	teardown := fs.Bool("teardown", false, "tear each node's loop down as it finishes (never a directory named in a manifest, never one holding uncommitted work)")
	reconcileSecs := fs.Int("reconcile-interval", envOrInt("HERDR_LOOP_RECONCILE_INTERVAL_SECS", int(state.DefaultReconcileInterval/time.Second)), "seconds between authoritative agent.list reconciles")
	logLevel := fs.String("log-level", envOr("HERDR_LOOP_LOG_LEVEL", "info"), "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: herdr-loop graph [flags] <graph.toml>")
	}
	graphPath := fs.Arg(0)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)}))

	data, err := os.ReadFile(graphPath)
	if err != nil {
		return fmt.Errorf("graph: %w", err)
	}
	gm, err := manifest.ParseGraph(data)
	if err != nil {
		return fmt.Errorf("graph: %s: %w", graphPath, err)
	}
	g := gm.Graph()
	if err := g.Validate(); err != nil {
		return fmt.Errorf("graph: %s: %w", graphPath, err)
	}
	run, err := graph.NewRun(g)
	if err != nil {
		return fmt.Errorf("graph: %s: %w", graphPath, err)
	}

	reconcileEvery := time.Duration(*reconcileSecs) * time.Second
	if reconcileEvery <= 0 {
		reconcileEvery = state.DefaultReconcileInterval
	}

	// A node's loop path is relative to the graph manifest, so somebody moving
	// the pair together does not have to rewrite every node.
	base := filepath.Dir(graphPath)

	log.Info("graph started", "name", g.Name, "nodes", len(g.Nodes), "entry", g.Entry,
		"max_iterations", g.MaxIterations)

	// Sequential worklist. The graph's own activation budget is what stops a
	// cycle here — it is checked inside Activate, so this loop cannot outrun
	// it however the edges are arranged.
	queue := []string{g.Entry}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]

		node, ok := g.Node(name)
		if !ok {
			return fmt.Errorf("graph: node %q is not defined", name)
		}
		if err := run.Activate(name); err != nil {
			if errors.Is(err, graph.ErrBudgetExhausted) {
				log.Warn("graph stopped: activation budget exhausted",
					"node", name, "max_iterations", g.MaxIterations,
					"note", "raise graph.max_iterations if the work genuinely needs more rounds")
				break
			}
			// Already running, or failed and not restartable. Neither is
			// fatal to the graph — skip and let the rest proceed.
			log.Info("node not activated", "node", name, "err", err)
			continue
		}

		outcome, err := runNode(ctx, node, base, loopOptions{
			KindsPath:      *kindsPath,
			ReconcileEvery: reconcileEvery,
			Teardown:       *teardown,
			Log:            log.With("node", name),
			// The graph owns the run-state record. A nested loop writing its
			// own would make `herdr-loop status` describe whichever node
			// happened to start last rather than the graph.
			WriteRunState: false,
		})
		if err != nil {
			// A node that could not run is a failed node, not a crashed
			// graph: other branches may still be worth running, and stopping
			// everything would discard work that already succeeded.
			log.Error("node failed", "node", name, "err", err)
			_ = run.Fail(name, err)
			continue
		}

		log.Info("node settled", "node", name, "reason", outcome.Reason,
			"iterations", outcome.Iterations, "escalations", len(outcome.Escalations))
		if err := run.Settle(name, outcome.Reason); err != nil {
			return fmt.Errorf("graph: %w", err)
		}

		next, err := run.Next(name)
		if err != nil {
			return fmt.Errorf("graph: %w", err)
		}
		for _, e := range next {
			if !edgeHolds(e, outcome) {
				continue
			}
			log.Info("edge taken", "from", name, "to", e.To, "outcome", outcome.Reason)
			queue = append(queue, e.To)
		}
	}

	printGraphSummary(g, run)
	return nil
}

// runNode runs whatever a node stands for and reports the outcome.
//
// A slot node has no loop file of its own, so it cannot run yet: the graph
// manifest declares slots but there is no engine instance to host a bare slot
// outside a loop. Reported as an explicit failure rather than silently
// settling, because a node that quietly did nothing would look converged.
func runNode(ctx context.Context, node graph.Node, base string, opts loopOptions) (engine.Outcome, error) {
	if node.Loop == "" {
		return engine.Outcome{}, fmt.Errorf("node %q is a bare slot; only loop nodes can execute today", node.Name)
	}
	path := node.Loop
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	opts.ManifestPath = path
	return runLoopFile(ctx, opts)
}

// edgeHolds evaluates an edge's predicate against the outcome the node's loop
// reported.
//
// An edge with no predicate always holds — the unconditional "then do this
// next", which is the common case and should not require ceremony.
func edgeHolds(e graph.Edge, out engine.Outcome) bool {
	if e.When == nil {
		return true
	}
	return graph.HoldsForOutcome(*e.When, out.Reason)
}

// printGraphSummary reports where every node ended up. A graph that stopped
// early is far more legible as a table than as a scroll of log lines.
func printGraphSummary(g *graph.Graph, run *graph.Run) {
	fmt.Printf("\n═══ graph %q ═══════════════════════════════\n\n", g.Name)
	fmt.Printf("  %-16s %-10s %-14s %s\n", "NODE", "STATE", "ACTIVATIONS", "OUTCOME")
	for _, n := range g.Nodes {
		s, ok := run.Status(n.Name)
		if !ok {
			continue
		}
		detail := s.Reason
		if s.Err != nil {
			detail = s.Err.Error()
		}
		fmt.Printf("  %-16s %-10s %-14d %s\n", n.Name, s.State, s.Activations, detail)
	}
	fmt.Printf("\n  %d activation(s) of a budget of %d\n\n", run.Activations(), g.MaxIterations)
}
