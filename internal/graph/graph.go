// Package graph models a graph of loops — PLAN.md §13's "graphs are this
// engine recursed, not a second plugin".
//
// A node is either a slot (one agent, exactly as today) or a nested loop, and
// edges carry the same all/any/not/eq/in/exists predicate DSL and the same
// file-based handoff that rules already use. One contract, two altitudes. A
// plain loop is a graph with one node, which is why every existing loop.toml
// keeps working untouched: this package adds an altitude, it does not change
// the one below it.
//
// This package models, validates and sequences. It does not execute: nothing
// here spawns an agent, reads a file, or runs a nested loop. A node's
// lifecycle is driven by the caller reporting what the node's underlying loop
// did (Settle/Fail), because only the caller runs it. Executing nested loops
// is a separate change, deliberately — see PLAN.md §13's "not built now".
//
// The package depends on nothing but the standard library, on purpose: the
// parse layer (internal/manifest) builds a Graph, so a dependency on the
// engine here would drag the engine into every manifest parse.
package graph

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Predicate is an edge guard — the same DSL a rule's `when` uses one altitude
// down (PLAN.md §3), which is the point: an author who can gate a rule can
// gate an edge, and the engine that evaluates one can evaluate the other.
//
// It is a third copy of the grammar rather than a shared type, matching how
// manifest.Predicate (the TOML surface) and engine.Predicate (the evaluation
// surface) already relate: each layer owns its own shape and a small
// conversion sits at the seam (cmd/herdr-loop/mapper.go's convertPredicate is
// the precedent). The copy here carries no TOML tags because a Graph is the
// model, never the file.
type Predicate struct {
	Op string
	// Filters holds operands for "all"/"any".
	Filters []Predicate
	// Filter holds the single operand for "not".
	Filter *Predicate
	// Field is a dotted path such as "review.handoff.verdict", or the literal
	// string "slot" to name a graph node directly by Value/Values.
	Field  string
	Value  any
	Values []any
}

// Node is one vertex: either a slot or a nested loop, never both and never
// neither.
//
// Both forms are references, not definitions. Slot names a slot defined
// alongside the graph; Loop is the path of a nested loop.toml. Neither is
// resolved here — this package never touches the filesystem, so whether the
// referenced manifest exists and parses is the executor's problem, checked
// when it loads the file rather than guessed at parse time.
type Node struct {
	Name string
	// Slot names a slot definition that lives with the graph. Empty when this
	// node is a nested loop.
	Slot string
	// Loop is the path to a nested loop.toml, interpreted relative to the
	// graph manifest. Empty when this node is a single slot.
	Loop string
}

// Edge is "when the source node settles and When holds, activate To".
//
// The source is declared rather than inferred, which is the one place this
// layer deliberately differs from rule altitude. A rule has no structural
// edge to declare — manifest.buildEdges has to infer which slots a rule reads
// from its predicate fields — whereas here the edge *is* the structure. So a
// predicate that mentions some third node is read as a guard, not as a second
// trigger, and contributes no edge. That keeps cycle detection exact instead
// of inventing cycles out of predicate text.
type Edge struct {
	From string
	To   string
	// When gates the activation. Nil means unconditional: activate To as soon
	// as From settles. Evaluation belongs to the engine — this package never
	// interprets a predicate, it only carries it.
	When *Predicate
}

// Graph is a set of nodes plus the edges between them.
//
// MaxIterations is the graph-wide activation budget and the usual way a cycle
// is bounded; see Validate for what counts as a bound and Run.Activate for
// what the budget counts at runtime.
type Graph struct {
	Name string
	// Entry is the node a run activates first. It is declared, not inferred
	// from the edges, because the obvious inference — "the node with no
	// incoming edge" — is wrong for the shape PLAN.md §13 is actually about:
	// in impl -> review -> impl, review's kickback gives impl an incoming
	// edge, so a graph with a bounded cycle at its head would be read as
	// having no way to start. Naming the entry also means reordering the node
	// list cannot silently change where a run begins.
	Entry         string
	MaxIterations int
	Nodes         []Node
	Edges         []Edge
}

// Validate rejects a graph that must not run.
//
// This is the same discipline internal/manifest applies to loop.toml, one
// altitude up: a config error that would strand or never terminate a run is
// caught before anything starts, not at 3am unattended. It checks structure
// only — node names, references, and termination. Predicate *shape* is the
// parse layer's job (manifest.validatePredicate), because a Graph built in Go
// carries predicates the engine will validate when it evaluates them.
func (g *Graph) Validate() error {
	if len(g.Nodes) == 0 {
		return errors.New("graph: no nodes defined")
	}

	names := make(map[string]bool, len(g.Nodes))
	for i, n := range g.Nodes {
		if n.Name == "" {
			return fmt.Errorf("graph: node %d: name is required", i)
		}
		if names[n.Name] {
			return fmt.Errorf("graph: node %q: duplicate node name", n.Name)
		}
		names[n.Name] = true

		if (n.Slot == "") == (n.Loop == "") {
			return fmt.Errorf("graph: node %q: must set exactly one of slot or loop", n.Name)
		}
	}

	for i, e := range g.Edges {
		if !names[e.From] {
			return fmt.Errorf("graph: edge %d: node %q is not defined", i, e.From)
		}
		if !names[e.To] {
			return fmt.Errorf("graph: edge %d: node %q is not defined", i, e.To)
		}
	}

	if err := g.validateReachable(names); err != nil {
		return err
	}
	return g.validateCycles()
}

// validateReachable requires an entry node, and requires every other node to
// be reachable from it by following edges.
//
// An unreachable node is the graph-altitude twin of a rule that can never
// fire: it parses, it terminates, and the work its author wrote never runs.
// Nothing else in the system would report it — no edge activates the node, so
// there is no failure to escalate, just silence.
func (g *Graph) validateReachable(names map[string]bool) error {
	if g.Entry == "" {
		return errors.New("graph: entry is required: name the node a run starts with")
	}
	if !names[g.Entry] {
		return fmt.Errorf("graph: entry: node %q is not defined", g.Entry)
	}

	adj := make(map[string][]string, len(g.Nodes))
	for _, e := range g.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	seen := map[string]bool{g.Entry: true}
	queue := []string{g.Entry}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, to := range adj[n] {
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}

	var stranded []string
	for _, n := range g.Nodes {
		if !seen[n.Name] {
			stranded = append(stranded, n.Name)
		}
	}
	if len(stranded) > 0 {
		sort.Strings(stranded)
		return fmt.Errorf("graph: node(s) %s cannot be reached from entry %q, so they can never run", strings.Join(stranded, ", "), g.Entry)
	}
	return nil
}

// validateCycles enforces PLAN.md §3's termination invariant at graph
// altitude: a cycle between loops needs a bound for exactly the reasons a
// cycle between slots does, so the same check runs over the node graph.
//
// It finds every strongly-connected component (see tarjanSCCs) and requires
// each real cycle to be covered by graph.max_iterations or by a timeout on
// every edge inside it. An acyclic graph is accepted with no cap at all —
// this is graph analysis, not a proxy check for "is max_iterations set".
func (g *Graph) validateCycles() error {
	adj := make(map[string][]string, len(g.Nodes))
	for _, e := range g.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	nodes := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		nodes = append(nodes, n.Name)
	}
	// Deterministic order so which cycle is reported first (when several are
	// uncapped) does not vary between runs of the same graph.
	sort.Strings(nodes)

	for _, scc := range tarjanSCCs(nodes, adj) {
		inSCC := make(map[string]bool, len(scc))
		for _, n := range scc {
			inSCC[n] = true
		}

		var cycleEdges []Edge
		selfLoop := false
		for _, e := range g.Edges {
			if inSCC[e.From] && inSCC[e.To] {
				cycleEdges = append(cycleEdges, e)
				if e.From == e.To {
					selfLoop = true
				}
			}
		}
		if len(scc) == 1 && !selfLoop {
			continue // a singleton with no self-edge is not a cycle
		}
		if g.MaxIterations > 0 {
			continue // the graph-wide budget covers every cycle in the graph
		}

		sort.Strings(scc)
		return fmt.Errorf("graph: cycle through node(s) %s has no activation cap: set graph.max_iterations",
			strings.Join(scc, " -> "))
	}
	return nil
}

// tarjanSCCs returns every strongly-connected component of the graph
// described by adj, using Tarjan's algorithm (single DFS pass, O(V+E)). Every
// cycle in a directed graph lies within exactly one SCC, so walking SCCs finds
// every cycle without enumerating them.
//
// This mirrors manifest.tarjanSCCs rather than calling it: that copy is
// unexported and its adjacency is typed in rule edges, and manifest imports
// this package, so importing back is a cycle Go will not allow. The algorithm
// and the acceptance rule around it are deliberately identical — if one is
// ever fixed, fix both.
func tarjanSCCs(nodes []string, adj map[string][]string) [][]string {
	t := &tarjanState{
		adj:     adj,
		index:   map[string]int{},
		low:     map[string]int{},
		onStack: map[string]bool{},
	}
	for _, n := range nodes {
		if _, visited := t.index[n]; !visited {
			t.strongConnect(n)
		}
	}
	return t.sccs
}

type tarjanState struct {
	adj     map[string][]string
	index   map[string]int
	low     map[string]int
	onStack map[string]bool
	stack   []string
	counter int
	sccs    [][]string
}

func (t *tarjanState) strongConnect(v string) {
	t.index[v] = t.counter
	t.low[v] = t.counter
	t.counter++
	t.stack = append(t.stack, v)
	t.onStack[v] = true

	for _, w := range t.adj[v] {
		if _, visited := t.index[w]; !visited {
			t.strongConnect(w)
			if t.low[w] < t.low[v] {
				t.low[v] = t.low[w]
			}
		} else if t.onStack[w] {
			if t.index[w] < t.low[v] {
				t.low[v] = t.index[w]
			}
		}
	}

	if t.low[v] != t.index[v] {
		return
	}
	var scc []string
	for {
		n := len(t.stack) - 1
		w := t.stack[n]
		t.stack = t.stack[:n]
		t.onStack[w] = false
		scc = append(scc, w)
		if w == v {
			break
		}
	}
	t.sccs = append(t.sccs, scc)
}

// NodeState is where one node sits in its lifecycle.
type NodeState string

const (
	// NodePending is a node that has not been activated. Every node starts
	// here, the entry node included.
	NodePending NodeState = "pending"

	// NodeRunning is a node whose underlying loop (or slot) is live.
	NodeRunning NodeState = "running"

	// NodeSettled is a node whose loop reached a terminal outcome the caller
	// judged a completion. A settled node can be activated again — that is
	// what an edge back into it means — so this is terminal for one pass, not
	// for the run.
	NodeSettled NodeState = "settled"

	// NodeFailed is a node whose loop ended without completing. Terminal for
	// the run: a failed node is never re-activated, and no edge leaves it,
	// because acting on the output of work that did not finish is exactly the
	// unsound step this engine exists to avoid.
	NodeFailed NodeState = "failed"
)

// ErrBudgetExhausted is returned by Activate once a run has activated
// Graph.MaxIterations nodes. Validate proves a bound exists; this enforces it.
var ErrBudgetExhausted = errors.New("activation budget exhausted")

// NodeStatus is a run's record of one node.
type NodeStatus struct {
	State NodeState
	// Reason is the terminal outcome the caller reported to Settle, verbatim
	// (in practice engine.Outcome.Reason). Empty unless State is NodeSettled.
	Reason string
	// Err is why the node failed. Nil unless State is NodeFailed.
	Err error
	// Activations counts how many times this node has been activated. Greater
	// than one means the graph looped back into it.
	Activations int
}

// Run is the lifecycle state of one pass over a Graph.
//
// It sequences; it does not execute and it does not evaluate. The caller runs
// each activated node's loop, evaluates each candidate edge's predicate with
// the same evaluator the engine uses for rules, and reports back:
//
//	run.Activate(g.Entry)
//	… caller runs the activated node's loop …
//	run.Settle(n, outcome.Reason)      // or run.Fail(n, err)
//	for _, e := range must(run.Next(n)) {
//	    if e.When == nil || eval(*e.When) { run.Activate(e.To) }
//	}
//
// Not safe for concurrent use. Drive it from the one goroutine that owns the
// reconciled state model, for the reason PLAN.md §4.8 gives: two owners of one
// session's state means two divergent models and no way to decide which is
// right.
type Run struct {
	graph       *Graph
	status      map[string]*NodeStatus
	order       []string
	activations int
}

// NewRun starts a pass over g. It validates first so a Run can never exist on
// a graph whose invariants do not hold — a caller that skipped Validate would
// otherwise sequence a graph that cannot start or cannot terminate.
func NewRun(g *Graph) (*Run, error) {
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("graph: new run: %w", err)
	}
	r := &Run{
		graph:  g,
		status: make(map[string]*NodeStatus, len(g.Nodes)),
		order:  make([]string, 0, len(g.Nodes)),
	}
	for _, n := range g.Nodes {
		r.status[n.Name] = &NodeStatus{State: NodePending}
		r.order = append(r.order, n.Name)
	}
	return r, nil
}

// Status returns the run's record of one node. The bool reports whether the
// node exists in the graph, distinguishing "never activated" from "not a
// node" — a typo'd node name must not read as a pending one.
func (r *Run) Status(name string) (NodeStatus, bool) {
	s, ok := r.status[name]
	if !ok {
		return NodeStatus{}, false
	}
	return *s, true
}

// Running returns the nodes currently live, in declaration order. Empty means
// the run cannot progress until the caller activates something — which after
// a settle with no satisfied outgoing edge means the run is over.
func (r *Run) Running() []string {
	var out []string
	for _, name := range r.order {
		if r.status[name].State == NodeRunning {
			out = append(out, name)
		}
	}
	return out
}

// Activations is how many node activations this run has spent against
// Graph.MaxIterations.
func (r *Run) Activations() int { return r.activations }

// Activate moves a node to running.
//
// Pending and settled are both legal sources. Re-activating a settled node is
// not a special case — it is what a cycle *is*: `impl -> review -> impl` means
// review's kickback activates a node that already settled once. Refusing it
// would make the bounded cycles Validate accepts impossible to execute.
//
// Every activation counts against Graph.MaxIterations, including the first
// activation of a root, which is what turns the parse-time bound into a
// runtime fact. A budget below the node count therefore stops even a
// straight-line graph early; set it above the number of nodes. Zero means
// unbounded, which Validate only accepts for an acyclic graph — where the
// activation count is bounded by the node count anyway.
func (r *Run) Activate(name string) error {
	s, ok := r.status[name]
	if !ok {
		return fmt.Errorf("graph: activate: node %q is not defined", name)
	}
	switch s.State {
	case NodeRunning:
		return fmt.Errorf("graph: activate %q: node is already running", name)
	case NodeFailed:
		return fmt.Errorf("graph: activate %q: node failed and is not restartable", name)
	}
	if r.graph.MaxIterations > 0 && r.activations >= r.graph.MaxIterations {
		return fmt.Errorf("graph: activate %q: %w (max_iterations = %d)", name, ErrBudgetExhausted, r.graph.MaxIterations)
	}

	s.State = NodeRunning
	s.Reason = ""
	s.Err = nil
	s.Activations++
	r.activations++
	return nil
}

// Settle records that a running node's loop reached a terminal outcome that
// counts as completion, carrying the reason verbatim (engine.Outcome.Reason).
//
// Which terminal outcomes count is the caller's call, not this package's, and
// deliberately: only the caller knows where the outcome came from. The rule of
// thumb is that a loop that ended because one of its own rules said finish has
// settled, while a loop that ran out of budget, was paused, lost its event
// stream, or returned an error has failed — those end a loop without
// converging it, and PLAN.md §4.5's "unknown is not done" is the same point at
// slot altitude.
func (r *Run) Settle(name, reason string) error {
	s, ok := r.status[name]
	if !ok {
		return fmt.Errorf("graph: settle: node %q is not defined", name)
	}
	if s.State != NodeRunning {
		return fmt.Errorf("graph: settle %q: node is %s, not running", name, s.State)
	}
	s.State = NodeSettled
	s.Reason = reason
	return nil
}

// Fail records that a running node's loop ended without completing. The node
// is terminal from here: Activate refuses it and Next returns nothing for it,
// so nothing downstream consumes the output of work that did not finish.
func (r *Run) Fail(name string, cause error) error {
	s, ok := r.status[name]
	if !ok {
		return fmt.Errorf("graph: fail: node %q is not defined", name)
	}
	if s.State != NodeRunning {
		return fmt.Errorf("graph: fail %q: node is %s, not running", name, s.State)
	}
	s.State = NodeFailed
	s.Err = cause
	return nil
}

// Next returns the edges leaving a settled node, in declaration order: the
// activations the caller should now consider. The caller evaluates each edge's
// When (nil means unconditional) and activates the ones that hold.
//
// It requires the node to be settled rather than merely known, so a caller
// cannot walk edges out of a node that is still running or that failed. That
// is the same guard PLAN.md §4.4 applies one altitude down — a slot mid-turn
// has not produced a result to hand on, and neither has a node mid-loop.
func (r *Run) Next(name string) ([]Edge, error) {
	s, ok := r.status[name]
	if !ok {
		return nil, fmt.Errorf("graph: next: node %q is not defined", name)
	}
	if s.State != NodeSettled {
		return nil, fmt.Errorf("graph: next %q: node is %s, not settled", name, s.State)
	}
	var out []Edge
	for _, e := range r.graph.Edges {
		if e.From == name {
			out = append(out, e)
		}
	}
	return out, nil
}

// Node returns a node by name.
func (g *Graph) Node(name string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// HoldsForOutcome evaluates an edge predicate against the outcome a node's
// loop reported.
//
// The grammar is the loop's own — all/any/not/eq/in/exists — so anyone who has
// written a rule already knows how to write an edge. What differs is the
// vocabulary: at rule altitude a predicate reads a slot's live status, while
// here the node has already finished and the only fact worth branching on is
// how it ended. "outcome" is therefore the field an edge reads, and it holds
// engine.Outcome.Reason verbatim: "converged", "loop:budget-exhausted", or
// whatever a finish action named.
//
// An unknown field is false rather than an error. An edge that cannot be
// evaluated must not be taken, and a graph that refused to run because one
// edge mentioned a field this altitude does not expose would be worse than one
// that simply does not follow it — the failure is visible either way, in the
// summary, as a node that never activated.
func HoldsForOutcome(p Predicate, outcome string) bool {
	switch p.Op {
	case "all":
		for _, sub := range p.Filters {
			if !HoldsForOutcome(sub, outcome) {
				return false
			}
		}
		return true
	case "any":
		for _, sub := range p.Filters {
			if HoldsForOutcome(sub, outcome) {
				return true
			}
		}
		return false
	case "not":
		if p.Filter == nil {
			return false
		}
		return !HoldsForOutcome(*p.Filter, outcome)
	case "eq":
		v, ok := outcomeField(p.Field, outcome)
		return ok && fmt.Sprint(p.Value) == v
	case "in":
		v, ok := outcomeField(p.Field, outcome)
		if !ok {
			return false
		}
		for _, want := range p.Values {
			if fmt.Sprint(want) == v {
				return true
			}
		}
		return false
	case "exists":
		_, ok := outcomeField(p.Field, outcome)
		return ok
	}
	return false
}

// outcomeField resolves the fields an edge may read about a finished node.
func outcomeField(field, outcome string) (string, bool) {
	switch field {
	case "outcome", "reason":
		// "reason" is accepted because engine.Outcome calls it that, and
		// somebody reading the loop's own logs will reach for the word they
		// saw there.
		return outcome, outcome != ""
	}
	return "", false
}
