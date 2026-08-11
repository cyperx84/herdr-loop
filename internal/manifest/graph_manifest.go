package manifest

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cyperx84/herdr-loop/internal/graph"
)

// GraphManifest is the parsed, validated contents of graph.toml — PLAN.md
// §13's altitude above loop.toml, where a node is either a slot or a whole
// nested loop.
//
// The [graph] table embeds Loop rather than restating its fields, and that is
// the design showing through rather than laziness: §13's claim is that a plain
// loop is a graph with one node, so the settings that govern a run — budget,
// handoff directory, blocked policy, shared-cwd opt-out, strict detection —
// are the same settings at both altitudes, validated by the same code. A
// second near-identical struct would be two places to fix one rule.
//
// [[slot]], [[node]], [[edge]] and [[blocked_rule]] are top-level
// array-of-tables, siblings of [graph], exactly as [[slot]] and [[rule]] are
// siblings of [loop] in loop.toml.
type GraphManifest struct {
	Settings     GraphSettings `toml:"graph"`
	Slots        []Slot        `toml:"slot"`
	Nodes        []GraphNode   `toml:"node"`
	Edges        []GraphEdge   `toml:"edge"`
	BlockedRules []BlockedRule `toml:"blocked_rule"`
}

// GraphSettings is the [graph] table: every setting [loop] has, plus the one
// thing only a graph needs.
//
// Loop is embedded rather than copied so the shared settings stay one
// definition validated by one function — TOML decodes an embedded struct's
// fields as if they were declared here, so `name`, `max_iterations`,
// `handoff_dir`, `on_blocked`, `allow_shared_cwd` and `strict` all read from
// [graph] exactly as they read from [loop].
type GraphSettings struct {
	Loop
	// Entry names the node a run starts with. Required unless the graph has
	// exactly one node, where there is nothing to choose; see
	// graph.Graph.Entry for why it is declared instead of inferred from the
	// edges.
	Entry string `toml:"entry"`
}

// GraphNode is one [[node]]: a name plus exactly one reference, either to a
// slot defined in this same file or to a nested loop.toml.
//
// A nested loop is named by path and not loaded here. Parse is a pure function
// of its bytes — no filesystem, matching Parse for loop.toml — so a missing or
// malformed sub-manifest is reported by whatever loads it, not guessed at now.
type GraphNode struct {
	Name string `toml:"name"`
	// Slot names a [[slot]] in this file. One agent, exactly as today.
	Slot string `toml:"slot"`
	// Loop is the path of a nested loop.toml, relative to this manifest.
	Loop string `toml:"loop"`
}

// GraphEdge is one [[edge]]: "when this predicate holds after `from` settles,
// activate `then.activate`".
//
// When is the same Predicate type rules use, deliberately — §13's requirement
// is one predicate contract at both altitudes, so an edge guard is written,
// parsed and validated by exactly the code a rule's `when` is. Omitting `when`
// means unconditional: activate as soon as the source settles.
//
// Timeout is the per-edge cap that can bound a cycle in place of
// graph.max_iterations, mirroring Rule.Timeout: a duration string in
// time.ParseDuration syntax.
type GraphEdge struct {
	From    string          `toml:"from"`
	When    Predicate       `toml:"when"`
	Then    GraphEdgeAction `toml:"then"`
	Timeout string          `toml:"timeout,omitempty"`
}

// GraphEdgeAction is an edge's `then`. Activating a node is the only thing an
// edge does: finishing, prompting and escalating are decisions inside a
// node's own loop, where the rules that make them already live.
type GraphEdgeAction struct {
	Activate string `toml:"activate"`
}

// ParseGraph decodes and validates graph.toml. A non-nil error means the
// graph must not run.
//
// Same posture as Parse: every invariant is enforced here, at parse time. The
// slot rules are literally the loop-altitude ones (validateSlots,
// validateOnBlocked), the predicate rules are literally validatePredicate, and
// the structural and termination rules are graph.Graph.Validate. On success
// the GraphManifest's Graph is known to be valid, so callers need not
// re-validate.
func ParseGraph(data []byte) (*GraphManifest, error) {
	var gm GraphManifest
	if _, err := toml.Decode(string(data), &gm); err != nil {
		return nil, fmt.Errorf("manifest: decode graph: %w", err)
	}
	if err := gm.validate(); err != nil {
		return nil, err
	}
	if err := gm.Graph().Validate(); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &gm, nil
}

// Graph builds the model this manifest describes.
//
// It is the seam between the file and the model, the same seam
// cmd/herdr-loop/mapper.go is between a manifest and the engine: the TOML
// shape and the model shape are allowed to differ, and the conversion is one
// small function rather than a type shared across layers. Call it only on a
// GraphManifest from ParseGraph — it converts, it does not check.
func (gm *GraphManifest) Graph() *graph.Graph {
	g := &graph.Graph{
		Name:          gm.Settings.Name,
		Entry:         gm.entry(),
		MaxIterations: gm.Settings.MaxIterations,
		Nodes:         make([]graph.Node, 0, len(gm.Nodes)),
		Edges:         make([]graph.Edge, 0, len(gm.Edges)),
	}
	for _, n := range gm.Nodes {
		g.Nodes = append(g.Nodes, graph.Node{Name: n.Name, Slot: n.Slot, Loop: n.Loop})
	}
	for _, e := range gm.Edges {
		edge := graph.Edge{From: e.From, To: e.Then.Activate}
		if e.When.Op != "" {
			w := convertGraphPredicate(e.When)
			edge.When = &w
		}
		g.Edges = append(g.Edges, edge)
	}
	return g
}

// convertGraphPredicate copies a manifest predicate onto the graph model's
// own copy of the DSL. Structural, not semantic: the grammar is identical, so
// this exists only because each layer owns its own shape (see
// graph.Predicate's doc comment).
func convertGraphPredicate(p Predicate) graph.Predicate {
	out := graph.Predicate{
		Op:     p.Op,
		Field:  p.Field,
		Value:  p.Value,
		Values: p.Values,
	}
	for _, f := range p.Filters {
		out.Filters = append(out.Filters, convertGraphPredicate(f))
	}
	if p.Filter != nil {
		f := convertGraphPredicate(*p.Filter)
		out.Filter = &f
	}
	return out
}

func (gm *GraphManifest) validate() error {
	// Slot and blocked-policy rules are the loop-altitude ones, run against a
	// synthetic Manifest so the checks are shared rather than restated. Two
	// nodes cannot occupy one working tree for the same reason two slots
	// cannot (PLAN.md §4.2), and on_blocked = "auto" needs the same
	// wildcard-free whitelist here as there (§4.3).
	base := &Manifest{Loop: gm.Settings.Loop, Slots: gm.Slots, BlockedRules: gm.BlockedRules}
	if err := base.validateSlots(); err != nil {
		return err
	}
	if err := base.validateOnBlocked(); err != nil {
		return err
	}

	nodeNames, err := gm.validateNodes()
	if err != nil {
		return err
	}
	return gm.validateEdges(nodeNames)
}

// validateNodes enforces node-name uniqueness, the slot-xor-loop requirement,
// and that every reference resolves.
//
// Two nodes naming the same slot, or the same nested loop.toml, is rejected
// for the §4.2 reason: both would put two agents in one working tree — one
// literally sharing a slot's cwd, the other running one loop's fixed worktree
// branches twice. Looping back into a node is how a graph repeats work;
// declaring it twice is a config error.
func (gm *GraphManifest) validateNodes() (map[string]bool, error) {
	slotNames := gm.slotNameSet()

	names := make(map[string]bool, len(gm.Nodes))
	slotOwner := make(map[string]string, len(gm.Nodes))
	loopOwner := make(map[string]string, len(gm.Nodes))

	for i, n := range gm.Nodes {
		if n.Name == "" {
			return nil, fmt.Errorf("manifest: node %d: name is required", i)
		}
		if names[n.Name] {
			return nil, fmt.Errorf("manifest: node %q: duplicate node name", n.Name)
		}
		names[n.Name] = true

		if (n.Slot == "") == (n.Loop == "") {
			return nil, fmt.Errorf("manifest: node %q: must set exactly one of slot or loop", n.Name)
		}

		if n.Slot != "" {
			if !slotNames[n.Slot] {
				return nil, fmt.Errorf("manifest: node %q: slot %q is not defined", n.Name, n.Slot)
			}
			if owner, dup := slotOwner[n.Slot]; dup {
				return nil, fmt.Errorf("manifest: node %q and node %q both name slot %q", owner, n.Name, n.Slot)
			}
			slotOwner[n.Slot] = n.Name
			continue
		}

		if owner, dup := loopOwner[n.Loop]; dup {
			return nil, fmt.Errorf("manifest: node %q and node %q both name loop %q", owner, n.Name, n.Loop)
		}
		loopOwner[n.Loop] = n.Name
	}
	return names, nil
}

// validateEdges checks predicate shape and the timeout, and leaves the
// endpoint and termination checks to graph.Graph.Validate so there is one
// implementation of each.
//
// Edge predicates go through validatePredicate with the node names as the
// known set, which makes the strict `{ field = "slot", value = "x" }` idiom
// resolve against nodes here. Reusing that field name rather than adding a
// parallel `field = "node"` is §13's "one contract, two altitudes" taken
// literally: the DSL keeps one shape and the surrounding altitude decides
// what a name refers to. A misspelled reference is still caught, which is the
// point of checking it at all.
func (gm *GraphManifest) validateEdges(nodeNames map[string]bool) error {
	for i, e := range gm.Edges {
		if e.From == "" {
			return fmt.Errorf("manifest: edge %d: from is required", i)
		}
		if e.Then.Activate == "" {
			return fmt.Errorf("manifest: edge %d: then.activate is required", i)
		}
		switch {
		case e.When.Op != "":
			if err := validatePredicate(e.When, nodeNames); err != nil {
				return fmt.Errorf("manifest: edge %d: when: %w", i, err)
			}
		case predicateWritten(e.When):
			// A `when` with operands but no op is a typo, not an
			// unconditional edge. Only a wholly absent `when` means
			// unconditional — silently treating a half-written guard as
			// "always" would fire an edge its author meant to gate.
			return fmt.Errorf("manifest: edge %d: when: op is required", i)
		}
		if e.Timeout != "" {
			// Rejected rather than ignored. An earlier version accepted a
			// per-edge timeout as a cycle bound and nothing ever enforced it,
			// so a graph could pass validation and activate forever. A knob
			// that does nothing is worse than an absent one — bound cycles
			// with graph.max_iterations, which is enforced on every
			// activation.
			return fmt.Errorf("manifest: graph: edge %d (%s): timeout is not supported; bound the cycle with graph.max_iterations", i, e.From)
		}
		if false {
			if _, err := time.ParseDuration(e.Timeout); err != nil {
				return fmt.Errorf("manifest: edge %d: timeout: %w", i, err)
			}
		}
	}
	return nil
}

// predicateWritten reports whether a predicate carries any operand, i.e.
// whether the author wrote a `when` table at all. Used only to tell an absent
// guard from a malformed one.
func predicateWritten(p Predicate) bool {
	return p.Field != "" || p.Value != nil || len(p.Values) > 0 || len(p.Filters) > 0 || p.Filter != nil
}

// entry resolves which node a run starts with.
//
// A one-node graph — a plain loop wrapped as a graph, PLAN.md §13's own
// bottom case — needs no entry key, because there is only one answer and
// requiring it would be noise on the simplest possible file. Anything larger
// must say, and graph.Graph.Validate rejects an empty entry: defaulting to
// the first declared node would mean reordering two [[node]] tables silently
// moves where the run begins.
func (gm *GraphManifest) entry() string {
	if gm.Settings.Entry == "" && len(gm.Nodes) == 1 {
		return gm.Nodes[0].Name
	}
	return gm.Settings.Entry
}

func (gm *GraphManifest) slotNameSet() map[string]bool {
	set := make(map[string]bool, len(gm.Slots))
	for _, s := range gm.Slots {
		set[s.Name] = true
	}
	return set
}
