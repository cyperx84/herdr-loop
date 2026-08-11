package graph

import (
	"errors"
	"strings"
	"testing"
)

// linear is impl -> review -> ship: three nodes, no cycle, no cap needed.
func linear() *Graph {
	return &Graph{
		Name:  "linear",
		Entry: "impl",
		Nodes: []Node{
			{Name: "impl", Loop: "loops/impl.toml"},
			{Name: "review", Loop: "loops/review.toml"},
			{Name: "ship", Slot: "ship"},
		},
		Edges: []Edge{
			{From: "impl", To: "review"},
			{From: "review", To: "ship"},
		},
	}
}

// kickback adds review -> impl, the cycle §13 says needs a bound.
func kickback() *Graph {
	g := linear()
	g.Edges = append(g.Edges, Edge{From: "review", To: "impl"})
	return g
}

// A graph with one node and no edges is a plain loop (PLAN.md §13). If this
// stops validating, every existing loop.toml has stopped being expressible as
// a graph, which is the hard requirement of the whole recursion.
func TestValidateAcceptsSingleNodeGraph(t *testing.T) {
	g := &Graph{Entry: "only", Nodes: []Node{{Name: "only", Loop: "loop.toml"}}}
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// An acyclic graph needs no cap at all: the check is graph analysis, not a
// proxy for "is max_iterations set". Demanding a cap here would force every
// straight-line graph to carry a meaningless number.
func TestValidateAcceptsAcyclicGraphWithoutCap(t *testing.T) {
	if err := linear().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// The termination invariant: a cycle between loops needs a bound for exactly
// the reasons a cycle between slots does. Without this a graph runs forever
// unattended, which is the failure PLAN.md §3 makes a parse-time error.
func TestValidateRejectsUnboundedCycle(t *testing.T) {
	err := kickback().Validate()
	if err == nil {
		t.Fatal("Validate accepted a cycle with no cap")
	}
	if !strings.Contains(err.Error(), "impl -> review") {
		t.Errorf("error should name the cycle's nodes, got %v", err)
	}
}

func TestValidateAcceptsCycleBoundedByMaxIterations(t *testing.T) {
	g := kickback()
	g.MaxIterations = 10
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A per-edge timeout bounds a cycle only when every edge in that cycle has
// one: one uncapped edge is enough to keep going round.
// A cycle must be bounded by something that is actually enforced.
//
// An earlier version accepted a per-edge timeout as sufficient. Nothing ever
// consumed it, so a graph could pass validation and activate forever — a
// bound present but never decremented. The only bound this layer enforces is
// graph.max_iterations, checked on every activation, so it is the only one
// validation accepts.
func TestValidateCycleRequiresAnEnforcedBound(t *testing.T) {
	cyclic := func() Graph {
		return Graph{
			Entry: "impl",
			Nodes: []Node{{Name: "impl", Slot: "impl"}, {Name: "review", Slot: "review"}},
			Edges: []Edge{{From: "impl", To: "review"}, {From: "review", To: "impl"}},
		}
	}

	g := cyclic()
	if err := g.Validate(); err == nil {
		t.Fatal("an unbounded cycle was accepted; it can activate forever")
	}

	g = cyclic()
	g.MaxIterations = 10
	if err := g.Validate(); err != nil {
		t.Errorf("max_iterations should bound the cycle, got: %v", err)
	}
}

// The budget is not merely declared — Activate must refuse once it is spent,
// or the bound is the same lie in a different place.
func TestActivationBudgetIsEnforcedAtRuntime(t *testing.T) {
	g := Graph{
		Entry:         "a",
		Nodes:         []Node{{Name: "a", Slot: "a"}, {Name: "b", Slot: "b"}},
		Edges:         []Edge{{From: "a", To: "b"}, {From: "b", To: "a"}},
		MaxIterations: 3,
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	r, err := NewRun(&g)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	activations := 0
	for i := 0; i < 100; i++ {
		name := "a"
		if i%2 == 1 {
			name = "b"
		}
		if err := r.Activate(name); err != nil {
			break
		}
		activations++
		r.Settle(name, "done")
	}
	if activations > g.MaxIterations {
		t.Fatalf("activated %d times against a budget of %d — the cap is not enforced",
			activations, g.MaxIterations)
	}
	if activations == 0 {
		t.Fatal("budget blocked the first activation; the cap is off by one")
	}
}

func TestValidateRejectsUnboundedSelfEdge(t *testing.T) {
	g := &Graph{
		Entry: "seed",
		Nodes: []Node{
			{Name: "seed", Loop: "seed.toml"},
			{Name: "grind", Loop: "grind.toml"},
		},
		Edges: []Edge{
			{From: "seed", To: "grind"},
			{From: "grind", To: "grind"},
		},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("Validate accepted an uncapped self-edge")
	}
	g.MaxIterations = 3
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate rejected a capped self-edge: %v", err)
	}
}

// A node no edge can reach never runs: it parses, it terminates, and the work
// its author wrote is silently skipped. Nothing downstream would report it —
// there is no failure to escalate, just a node that stays pending forever.
func TestValidateRejectsUnreachableNode(t *testing.T) {
	g := linear()
	g.Nodes = append(g.Nodes, Node{Name: "orphan", Loop: "orphan.toml"})
	err := g.Validate()
	if err == nil {
		t.Fatal("Validate accepted a node nothing can reach")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Errorf("error should name the stranded node, got %v", err)
	}
}

// The entry is declared, never inferred. Inferring it as "the node with no
// incoming edge" is wrong for the shape §13 is about: review's kickback gives
// impl an incoming edge, so the canonical impl <-> review graph would read as
// having no way to start.
func TestValidateEntryIsDeclaredNotInferred(t *testing.T) {
	g := kickback()
	g.MaxIterations = 10
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate rejected a graph whose entry sits inside a cycle: %v", err)
	}

	g.Entry = ""
	if err := g.Validate(); err == nil {
		t.Fatal("Validate accepted a graph with no entry")
	}
	g.Entry = "typo"
	err := g.Validate()
	if err == nil {
		t.Fatal("Validate accepted an entry naming no node")
	}
	if !strings.Contains(err.Error(), `"typo" is not defined`) {
		t.Errorf("error = %v, want it to name the undefined entry", err)
	}
}

func TestValidateRejectsMalformedNodes(t *testing.T) {
	tests := []struct {
		name  string
		graph *Graph
		want  string
	}{
		{
			name:  "no nodes",
			graph: &Graph{},
			want:  "no nodes",
		},
		{
			name:  "unnamed node",
			graph: &Graph{Entry: "a", Nodes: []Node{{Loop: "a.toml"}}},
			want:  "name is required",
		},
		{
			name: "duplicate name",
			graph: &Graph{Entry: "a", Nodes: []Node{
				{Name: "a", Loop: "a.toml"},
				{Name: "a", Loop: "b.toml"},
			}},
			want: "duplicate node name",
		},
		{
			name:  "neither slot nor loop",
			graph: &Graph{Entry: "a", Nodes: []Node{{Name: "a"}}},
			want:  "exactly one of slot or loop",
		},
		{
			name:  "both slot and loop",
			graph: &Graph{Entry: "a", Nodes: []Node{{Name: "a", Slot: "s", Loop: "a.toml"}}},
			want:  "exactly one of slot or loop",
		},
		{
			name: "edge to undefined node",
			graph: &Graph{
				Entry: "a",
				Nodes: []Node{{Name: "a", Loop: "a.toml"}},
				Edges: []Edge{{From: "a", To: "typo"}},
			},
			want: `"typo" is not defined`,
		},
		{
			name: "edge from undefined node",
			graph: &Graph{
				Entry: "a",
				Nodes: []Node{{Name: "a", Loop: "a.toml"}},
				Edges: []Edge{{From: "typo", To: "a"}},
			},
			want: `"typo" is not defined`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.graph.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A Run must never exist on a graph that cannot start or cannot terminate: a
// caller who forgot Validate would otherwise sequence one.
func TestNewRunValidates(t *testing.T) {
	if _, err := NewRun(kickback()); err == nil {
		t.Fatal("NewRun accepted an unbounded cycle")
	}
}

// The lifecycle a plain loop takes: one node, no edges, pending -> running ->
// settled, with the finish reason carried through verbatim.
func TestRunSinglenodeLifecycle(t *testing.T) {
	g := &Graph{Entry: "only", Nodes: []Node{{Name: "only", Loop: "loop.toml"}}}
	r, err := NewRun(g)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if s, _ := r.Status("only"); s.State != NodePending {
		t.Fatalf("initial state = %s, want pending", s.State)
	}
	if err := r.Activate("only"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if s, _ := r.Status("only"); s.State != NodeRunning {
		t.Fatalf("state after Activate = %s, want running", s.State)
	}
	if got := r.Running(); len(got) != 1 || got[0] != "only" {
		t.Fatalf("Running() = %v, want [only]", got)
	}
	if err := r.Settle("only", "converged"); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	s, _ := r.Status("only")
	if s.State != NodeSettled || s.Reason != "converged" {
		t.Fatalf("status after Settle = %+v, want settled/converged", s)
	}
	if len(r.Running()) != 0 {
		t.Fatalf("Running() = %v, want none", r.Running())
	}
}

// Re-activating a settled node is what a cycle means. If settled were
// terminal, the bounded cycles Validate deliberately accepts could never
// execute — a validator that permits what the sequencer forbids.
func TestRunReactivatesSettledNodeAcrossCycle(t *testing.T) {
	g := kickback()
	g.MaxIterations = 10
	r, err := NewRun(g)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	mustActivate(t, r, "impl")
	mustSettle(t, r, "impl", "done")
	next, err := r.Next("impl")
	if err != nil {
		t.Fatalf("Next(impl): %v", err)
	}
	if len(next) != 1 || next[0].To != "review" {
		t.Fatalf("Next(impl) = %+v, want one edge to review", next)
	}

	mustActivate(t, r, "review")
	mustSettle(t, r, "review", "changes-requested")
	if err := r.Activate("impl"); err != nil {
		t.Fatalf("re-Activate(impl): %v", err)
	}
	s, _ := r.Status("impl")
	if s.State != NodeRunning {
		t.Fatalf("state after re-activation = %s, want running", s.State)
	}
	if s.Activations != 2 {
		t.Fatalf("impl activations = %d, want 2", s.Activations)
	}
	if s.Reason != "" {
		t.Errorf("re-activation left the previous reason %q in place", s.Reason)
	}
}

// The parse-time bound has to become a runtime fact, or a cyclic graph that
// validated still runs forever.
func TestRunActivationBudgetIsEnforced(t *testing.T) {
	g := kickback()
	g.MaxIterations = 3
	r, err := NewRun(g)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	for i := 0; i < 3; i++ {
		node := "impl"
		if i%2 == 1 {
			node = "review"
		}
		mustActivate(t, r, node)
		mustSettle(t, r, node, "again")
	}
	if r.Activations() != 3 {
		t.Fatalf("Activations() = %d, want 3", r.Activations())
	}

	err = r.Activate("review")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Activate past the budget: err = %v, want ErrBudgetExhausted", err)
	}
}

// A failed node is terminal: nothing restarts it and nothing downstream
// consumes the output of work that never finished (PLAN.md §4.5's "unknown is
// not done", one altitude up).
func TestRunFailedNodeIsTerminal(t *testing.T) {
	r := mustRun(t, linear())
	mustActivate(t, r, "impl")
	if err := r.Fail("impl", errors.New("budget exhausted")); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	s, _ := r.Status("impl")
	if s.State != NodeFailed || s.Err == nil {
		t.Fatalf("status after Fail = %+v, want failed with a cause", s)
	}
	if err := r.Activate("impl"); err == nil {
		t.Fatal("Activate restarted a failed node")
	}
	if _, err := r.Next("impl"); err == nil {
		t.Fatal("Next walked edges out of a failed node")
	}
}

// Edges may only be walked out of a settled node. A node mid-loop has not
// produced a result to hand on, the same guard §4.4 applies to a slot mid-turn.
func TestRunNextRequiresSettledSource(t *testing.T) {
	r := mustRun(t, linear())
	if _, err := r.Next("impl"); err == nil {
		t.Fatal("Next returned edges for a node that never ran")
	}
	mustActivate(t, r, "impl")
	if _, err := r.Next("impl"); err == nil {
		t.Fatal("Next returned edges for a running node")
	}
}

func TestRunRejectsUnknownAndOutOfOrderTransitions(t *testing.T) {
	r := mustRun(t, linear())

	if _, ok := r.Status("nope"); ok {
		t.Fatal("Status reported an undefined node as known")
	}
	if err := r.Activate("nope"); err == nil {
		t.Fatal("Activate accepted an undefined node")
	}
	if err := r.Settle("impl", "done"); err == nil {
		t.Fatal("Settle accepted a node that was never activated")
	}
	if err := r.Fail("impl", errors.New("x")); err == nil {
		t.Fatal("Fail accepted a node that was never activated")
	}
	mustActivate(t, r, "impl")
	if err := r.Activate("impl"); err == nil {
		t.Fatal("Activate accepted a node that is already running")
	}
}

// Running reports in declaration order, so a caller reporting or tearing down
// live nodes sees the same order every time rather than a map\'s.
func TestRunRunningIsDeclarationOrdered(t *testing.T) {
	g := &Graph{
		Entry: "fan",
		Nodes: []Node{
			{Name: "fan", Loop: "fan.toml"},
			{Name: "b", Loop: "b.toml"},
			{Name: "a", Loop: "a.toml"},
		},
		Edges: []Edge{
			{From: "fan", To: "b"},
			{From: "fan", To: "a"},
		},
	}
	r := mustRun(t, g)
	mustActivate(t, r, "fan")
	mustSettle(t, r, "fan", "done")
	mustActivate(t, r, "a")
	mustActivate(t, r, "b")

	running := r.Running()
	if len(running) != 2 || running[0] != "b" || running[1] != "a" {
		t.Fatalf("Running() = %v, want [b a]", running)
	}
}

func mustRun(t *testing.T, g *Graph) *Run {
	t.Helper()
	r, err := NewRun(g)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	return r
}

func mustActivate(t *testing.T, r *Run, name string) {
	t.Helper()
	if err := r.Activate(name); err != nil {
		t.Fatalf("Activate(%s): %v", name, err)
	}
}

func mustSettle(t *testing.T, r *Run, name, reason string) {
	t.Helper()
	if err := r.Settle(name, reason); err != nil {
		t.Fatalf("Settle(%s): %v", name, err)
	}
}

// Edges branch on how a node's loop ended. Same grammar as a rule, different
// vocabulary: a finished node has no live status, only an outcome.
func TestHoldsForOutcome(t *testing.T) {
	eq := func(field, want string) Predicate {
		return Predicate{Op: "eq", Field: field, Value: want}
	}

	if !HoldsForOutcome(eq("outcome", "converged"), "converged") {
		t.Error("eq on a matching outcome must hold")
	}
	if HoldsForOutcome(eq("outcome", "converged"), "loop:budget-exhausted") {
		t.Error("eq held on a different outcome")
	}
	// engine.Outcome calls the field Reason, so somebody reading the loop's
	// own logs reaches for that word. Both names resolve.
	if !HoldsForOutcome(eq("reason", "converged"), "converged") {
		t.Error("reason must be accepted as an alias for outcome")
	}

	in := Predicate{Op: "in", Field: "outcome", Values: []any{"converged", "green"}}
	if !HoldsForOutcome(in, "green") {
		t.Error("in must hold for any listed outcome")
	}
	if HoldsForOutcome(in, "failed") {
		t.Error("in held for an unlisted outcome")
	}

	not := Predicate{Op: "not", Filter: ptr(eq("outcome", "converged"))}
	if !HoldsForOutcome(not, "failed") {
		t.Error("not must invert")
	}

	all := Predicate{Op: "all", Filters: []Predicate{eq("outcome", "converged"), {Op: "exists", Field: "outcome"}}}
	if !HoldsForOutcome(all, "converged") {
		t.Error("all must hold when every operand does")
	}

	// A field this altitude does not expose must not take the edge. An edge
	// that cannot be evaluated is not one to follow, and the failure shows up
	// in the summary as a node that never activated rather than as a crash.
	if HoldsForOutcome(eq("slot.impl.status", "idle"), "converged") {
		t.Error("an unknown field took the edge; only outcome-shaped facts exist here")
	}
	// An empty outcome means the node reported nothing to branch on.
	if HoldsForOutcome(Predicate{Op: "exists", Field: "outcome"}, "") {
		t.Error("exists held on an empty outcome")
	}
}

func ptr(p Predicate) *Predicate { return &p }

// A budget below the number of reachable nodes truncates the run instead of
// bounding a loop, and does it silently: later nodes stay pending and the
// graph reports an ordinary finish. Someone writing max_iterations = 2 on a
// five-node pipeline means "do not loop forever", not "run two of my nodes".
func TestValidateRejectsBudgetSmallerThanTheGraph(t *testing.T) {
	g := Graph{
		Entry: "a",
		Nodes: []Node{{Name: "a", Loop: "a.toml"}, {Name: "b", Loop: "b.toml"}, {Name: "c", Loop: "c.toml"}},
		Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "c"}},
	}

	g.MaxIterations = 2
	err := g.Validate()
	if err == nil {
		t.Fatal("a budget of 2 was accepted for 3 reachable nodes; the run would stop partway through")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error should name the number of nodes to raise the budget to; got: %v", err)
	}

	g.MaxIterations = 3
	if err := g.Validate(); err != nil {
		t.Errorf("a budget equal to the node count must be accepted: %v", err)
	}

	// Unbounded stays legal for an acyclic graph — only cycles require a cap.
	g.MaxIterations = 0
	if err := g.Validate(); err != nil {
		t.Errorf("an acyclic graph needs no budget: %v", err)
	}
}

// Fan-in is the ordinary case, not an exotic one: two edges converge and the
// second activation is declined. A driver must be able to tell that apart from
// a genuine fault, which means a sentinel rather than a formatted string.
func TestDeclinedActivationsAreDistinguishable(t *testing.T) {
	g := Graph{
		Entry: "a",
		Nodes: []Node{{Name: "a", Loop: "a.toml"}, {Name: "b", Loop: "b.toml"}},
		Edges: []Edge{{From: "a", To: "b"}},
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	r, err := NewRun(&g)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if err := r.Activate("a"); err != nil {
		t.Fatalf("first activation: %v", err)
	}
	err = r.Activate("a")
	if !errors.Is(err, ErrNodeAlreadyRunning) {
		t.Errorf("re-activating a running node = %v, want ErrNodeAlreadyRunning so a join can absorb it", err)
	}

	if err := r.Fail("a", errors.New("boom")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	err = r.Activate("a")
	if !errors.Is(err, ErrNodeFailed) {
		t.Errorf("re-activating a failed node = %v, want ErrNodeFailed", err)
	}
}
