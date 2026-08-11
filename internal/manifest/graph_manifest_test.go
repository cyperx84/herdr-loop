package manifest

import (
	"strings"
	"testing"
)

// graphExample is the shape PLAN.md §13 sketches: two nested loops and one
// bare slot, with review able to kick back to impl. It is the contract — if
// this stops parsing, the plan and the graph parser have drifted.
const graphExample = `
[graph]
name           = "impl-review-ship"
entry          = "impl"
max_iterations = 10
handoff_dir    = ".herdr-loop/handoff"
on_blocked     = "escalate"

[[slot]]
name     = "ship"
kind     = "claude"
worktree = { branch = "graph/ship", base = "main" }

[[node]]
name = "impl"
loop = "loops/impl.toml"

[[node]]
name = "review"
loop = "loops/review.toml"

[[node]]
name = "ship"
slot = "ship"

[[edge]]
from = "impl"
then = { activate = "review" }

[[edge]]
from = "review"
when = { op = "eq", field = "review.handoff.verdict", value = "changes-requested" }
then = { activate = "impl" }

[[edge]]
from = "review"
when = { op = "all", filters = [
  { op = "eq", field = "slot", value = "review" },
  { op = "not", filter = { op = "eq", field = "review.handoff.verdict", value = "changes-requested" } },
] }
then = { activate = "ship" }
`

func TestParseGraphExample(t *testing.T) {
	gm, err := ParseGraph([]byte(graphExample))
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	if gm.Settings.Name != "impl-review-ship" {
		t.Errorf("Settings.Name = %q, want impl-review-ship", gm.Settings.Name)
	}
	if gm.Settings.MaxIterations != 10 {
		t.Errorf("Settings.MaxIterations = %d, want 10", gm.Settings.MaxIterations)
	}
	if gm.Settings.Entry != "impl" {
		t.Errorf("Settings.Entry = %q, want impl", gm.Settings.Entry)
	}
	if len(gm.Nodes) != 3 || len(gm.Edges) != 3 || len(gm.Slots) != 1 {
		t.Fatalf("parsed %d nodes / %d edges / %d slots, want 3/3/1", len(gm.Nodes), len(gm.Edges), len(gm.Slots))
	}

	g := gm.Graph()
	if g.Entry != "impl" {
		t.Errorf("Graph().Entry = %q, want impl", g.Entry)
	}
	if g.MaxIterations != 10 {
		t.Errorf("Graph().MaxIterations = %d, want 10", g.MaxIterations)
	}
	if g.Nodes[2].Slot != "ship" || g.Nodes[2].Loop != "" {
		t.Errorf("node ship = %+v, want a slot node", g.Nodes[2])
	}
	if g.Nodes[0].Loop != "loops/impl.toml" {
		t.Errorf("node impl loop = %q, want loops/impl.toml", g.Nodes[0].Loop)
	}

	// An edge with no `when` is unconditional; the guard must survive
	// conversion for the ones that have it, nested filters included.
	if g.Edges[0].When != nil {
		t.Errorf("edge without when converted to %+v, want nil (unconditional)", g.Edges[0].When)
	}
	if g.Edges[1].When == nil || g.Edges[1].When.Field != "review.handoff.verdict" {
		t.Errorf("edge 1 when = %+v, want the verdict predicate", g.Edges[1].When)
	}
	// "not" is the one operand the conversion has to move rather than copy —
	// a single Filter pointer, not the Filters slice — so a graph edge could
	// silently lose its negation and fire on the case it was written to
	// exclude.
	w := g.Edges[2].When
	if w == nil || w.Op != "all" || len(w.Filters) != 2 {
		t.Fatalf("edge 2 when = %+v, want an all-predicate over two filters", w)
	}
	neg := w.Filters[1]
	if neg.Op != "not" || neg.Filter == nil || neg.Filter.Value != "changes-requested" {
		t.Errorf("edge 2 negated filter = %+v, want not(verdict == changes-requested)", neg)
	}
	if g.Edges[2].To != "ship" {
		t.Errorf("edge 2 target = %q, want ship", g.Edges[2].To)
	}
}

// A plain loop is a graph with one node (PLAN.md §13). The smallest graph must
// parse with no edges, no cap and no slots — anything more required here would
// mean the recursion does not actually bottom out at today's loop.
func TestParseGraphSingleNode(t *testing.T) {
	gm, err := ParseGraph([]byte(`
[graph]
name = "wrap"

[[node]]
name = "only"
loop = "loop.toml"
`))
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	g := gm.Graph()
	if g.Entry != "only" {
		t.Errorf("Graph().Entry = %q, want the sole node to be the default entry", g.Entry)
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("Graph().Validate: %v", err)
	}
}

// Past one node the entry must be declared. Defaulting to the first [[node]]
// would mean reordering two tables silently moves where a run begins — the
// kind of change a diff makes look harmless.
func TestParseGraphRequiresEntryPastOneNode(t *testing.T) {
	_, err := ParseGraph([]byte(`
[graph]
name = "two"

[[node]]
name = "a"
loop = "a.toml"

[[node]]
name = "b"
loop = "b.toml"

[[edge]]
from = "a"
then = { activate = "b" }
`))
	if err == nil {
		t.Fatal("ParseGraph accepted a two-node graph with no entry")
	}
	if !strings.Contains(err.Error(), "entry is required") {
		t.Errorf("error = %v, want it to ask for an entry", err)
	}
}

// The termination invariant has to reach the file layer, not just the model:
// a cyclic graph.toml with no cap must fail ParseGraph, exactly as a cyclic
// loop.toml fails Parse.
func TestParseGraphRejectsUnboundedCycle(t *testing.T) {
	src := strings.Replace(graphExample, "max_iterations = 10", "", 1)
	_, err := ParseGraph([]byte(src))
	if err == nil {
		t.Fatal("ParseGraph accepted a cycle with no cap")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want it to name the cycle", err)
	}
}

// A per-edge timeout bounds a cycle in place of the graph-wide cap, the same
// escape valve Rule.Timeout is for rules.
// A per-edge timeout is rejected outright rather than quietly ignored.
//
// An earlier version accepted it as a cycle bound and nothing ever enforced
// it, so a graph could pass validation and activate forever. Silently dropping
// the key would leave the same manifest looking bounded to its author.
func TestParseGraphRejectsEdgeTimeout(t *testing.T) {
	const src = `
[graph]
name  = "g"
entry = "impl"

[[node]]
name = "impl"
loop = "impl.toml"

[[node]]
name = "review"
loop = "review.toml"

[[edge]]
from = "impl"
then = { activate = "review" }

[[edge]]
from    = "review"
timeout = "30m"
then    = { activate = "impl" }
`
	_, err := ParseGraph([]byte(src))
	if err == nil {
		t.Fatal("an edge timeout was accepted; nothing enforces it, so the cycle is unbounded")
	}
	if !strings.Contains(err.Error(), "max_iterations") {
		t.Errorf("error must point at the bound that IS enforced; got: %v", err)
	}
}

func TestParseGraphRejections(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "node names an undefined slot",
			src: `
[graph]
name = "g"
[[node]]
name = "ship"
slot = "nope"
`,
			want: `slot "nope" is not defined`,
		},
		{
			name: "node sets both slot and loop",
			src: `
[graph]
name = "g"
[[slot]]
name = "s"
cwd  = "/repo"
[[node]]
name = "n"
slot = "s"
loop = "l.toml"
`,
			want: "exactly one of slot or loop",
		},
		{
			name: "duplicate node name",
			src: `
[graph]
name = "g"
[[node]]
name = "n"
loop = "a.toml"
[[node]]
name = "n"
loop = "b.toml"
`,
			want: "duplicate node name",
		},
		{
			name: "two nodes on one slot",
			src: `
[graph]
name = "g"
[[slot]]
name = "s"
cwd  = "/repo"
[[node]]
name = "a"
slot = "s"
[[node]]
name = "b"
slot = "s"
`,
			want: `both name slot "s"`,
		},
		{
			name: "two nodes on one nested loop",
			src: `
[graph]
name = "g"
[[node]]
name = "a"
loop = "impl.toml"
[[node]]
name = "b"
loop = "impl.toml"
`,
			want: `both name loop "impl.toml"`,
		},
		{
			name: "edge activates an undefined node",
			src: `
[graph]
name  = "g"
entry = "a"
[[node]]
name = "a"
loop = "a.toml"
[[edge]]
from = "a"
then = { activate = "ghost" }
`,
			want: `"ghost" is not defined`,
		},
		{
			name: "edge with no from",
			src: `
[graph]
name = "g"
[[node]]
name = "a"
loop = "a.toml"
[[edge]]
then = { activate = "a" }
`,
			want: "from is required",
		},
		{
			name: "edge with no activate target",
			src: `
[graph]
name = "g"
[[node]]
name = "a"
loop = "a.toml"
[[edge]]
from = "a"
when = { op = "exists", field = "a.handoff" }
`,
			want: "then.activate is required",
		},
		{
			name: "edge predicate with an unknown op",
			src: `
[graph]
name = "g"
[[node]]
name = "a"
loop = "a.toml"
[[node]]
name = "b"
loop = "b.toml"
[[edge]]
from = "a"
when = { op = "maybe", field = "x" }
then = { activate = "b" }
`,
			want: "unknown op",
		},
		{
			name: "edge predicate names an undefined node",
			src: `
[graph]
name = "g"
[[node]]
name = "a"
loop = "a.toml"
[[node]]
name = "b"
loop = "b.toml"
[[edge]]
from = "a"
when = { op = "eq", field = "slot", value = "typo" }
then = { activate = "b" }
`,
			want: `"typo" is not defined`,
		},
		{
			name: "half-written when",
			src: `
[graph]
name = "g"
[[node]]
name = "a"
loop = "a.toml"
[[node]]
name = "b"
loop = "b.toml"
[[edge]]
from = "a"
when = { field = "a.handoff.verdict", value = "clean" }
then = { activate = "b" }
`,
			want: "op is required",
		},
		{
			name: "unparsable edge timeout",
			src: `
[graph]
name = "g"
[[node]]
name = "a"
loop = "a.toml"
[[node]]
name = "b"
loop = "b.toml"
[[edge]]
from    = "a"
timeout = "soon"
then    = { activate = "b" }
`,
			want: "timeout",
		},
		{
			name: "slots sharing a cwd",
			src: `
[graph]
name = "g"
[[slot]]
name = "a"
cwd  = "/repo"
[[slot]]
name = "b"
cwd  = "/repo"
[[node]]
name = "na"
slot = "a"
[[node]]
name = "nb"
slot = "b"
`,
			want: "share cwd",
		},
		{
			name: "auto blocked policy with no whitelist",
			src: `
[graph]
name       = "g"
on_blocked = "auto"
[[node]]
name = "a"
loop = "a.toml"
`,
			want: "requires a non-empty",
		},
		{
			name: "no nodes at all",
			src: `
[graph]
name = "g"
`,
			want: "no nodes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseGraph([]byte(tt.src))
			if err == nil {
				t.Fatalf("ParseGraph accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
			if !strings.HasPrefix(err.Error(), "manifest: ") {
				t.Errorf("error = %v, want the package's manifest: voice", err)
			}
		})
	}
}

// The two altitudes stay separate parsers: a graph.toml is not a loop.toml,
// and ParseGraph says so plainly rather than running a loop with no nodes.
// The reverse direction — every existing loop.toml still parsing unchanged —
// is what the rest of this package\'s tests already hold down; nothing here
// touches Parse.
func TestParseGraphRejectsLoopManifest(t *testing.T) {
	if _, err := ParseGraph([]byte(planExample)); err == nil {
		t.Fatal("ParseGraph accepted a loop.toml as a graph.toml")
	}
}
