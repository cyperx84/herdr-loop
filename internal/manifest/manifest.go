// Package manifest parses and validates loop.toml — the loop/slot/rule model
// described in PLAN.md §3.
//
// Validation is the point of this package, not an afterthought bolted onto
// parsing. herdr-loop's whole safety case rests on catching config errors
// before a loop starts: a manifest whose rules can retrigger each other
// forever, that puts two agents in one working tree, that references a slot
// which does not exist, or that lets on_blocked = "auto" answer an
// unanticipated prompt must fail Parse, not fail at 3am unattended. Every
// hard rule in PLAN.md §3/§4.2/§4.3 has a corresponding check here.
package manifest

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Manifest is the parsed, validated contents of loop.toml.
//
// [[slot]] and [[rule]] are top-level array-of-tables in the file (siblings
// of [loop], not nested under it — see the sketch in PLAN.md §3), so they
// are fields of Manifest rather than of Loop even though "the loop" is
// conceptually the whole document.
type Manifest struct {
	Loop         Loop          `toml:"loop"`
	Slots        []Slot        `toml:"slot"`
	Rules        []Rule        `toml:"rule"`
	BlockedRules []BlockedRule `toml:"blocked_rule"`
}

// Loop is the [loop] table: settings that apply to the whole run.
type Loop struct {
	Name          string `toml:"name"`
	MaxIterations int    `toml:"max_iterations"`
	HandoffDir    string `toml:"handoff_dir"`
	// OnBlocked selects the policy from PLAN.md §4.3: "escalate" (default,
	// halt the slot and notify), "pause" (halt the whole loop), or "auto"
	// (answer only exact patterns in BlockedRules). Empty means "escalate".
	OnBlocked string `toml:"on_blocked"`
	// AllowSharedCWD opts out of the one-worktree-per-slot rule (PLAN.md
	// §4.2). Off by default: two agents sharing a working tree produces
	// amend-becomes-new-commit and phantom modifications neither agent made.
	AllowSharedCWD bool `toml:"allow_shared_cwd"`
	// Strict refuses to fire any rule against a slot whose status herdr
	// inferred by classifying the rendered screen rather than the agent
	// self-reporting it (PLAN.md §4.6).
	//
	// Off by default, and the reason is worth stating: as of herdr 0.8.0 only
	// pi and opencode self-report, so a strict loop over Claude Code or Codex
	// slots will not fire at all. That is the honest consequence of what is
	// measurable today. Turn it on for loops where acting on a wrong "done"
	// costs more than not acting — anything that merges, publishes, or
	// deletes.
	Strict bool `toml:"strict"`
}

// Worktree gives a slot its own checkout, so its agent never shares a
// working tree with another slot (PLAN.md §4.2).
type Worktree struct {
	Branch string `toml:"branch"`
	Base   string `toml:"base"`
}

// Slot is a named seat for an agent: kind plus where it works.
//
// Every slot sets exactly one of CWD or Worktree — never both (ambiguous
// which one wins) and never neither (an agent needs a working directory).
type Slot struct {
	Name     string    `toml:"name"`
	Kind     string    `toml:"kind"`
	CWD      string    `toml:"cwd"`
	Worktree *Worktree `toml:"worktree"`
	Prompt   string    `toml:"prompt"`
}

// Predicate is a boolean expression over slot/agent state, evaluated by the
// engine on every reconciled model update (never against raw event
// arrival — see PLAN.md §4.8).
//
// It mirrors herdr's own AgentViewFilter DSL (schemas.request.$defs in the
// herdr-api schema): all/any/not compose sub-predicates, eq/in/exists read
// a single field. Reusing that grammar means anyone who has written a herdr
// agent-view filter already knows this one.
type Predicate struct {
	Op string `toml:"op"`
	// Filters holds operands for "all"/"any".
	Filters []Predicate `toml:"filters,omitempty"`
	// Filter holds the single operand for "not".
	Filter *Predicate `toml:"filter,omitempty"`
	// Field is a dotted path such as "review.handoff.verdict", or the
	// literal string "slot" to name a slot directly by Value/Values (both
	// idioms appear in PLAN.md §3's own example).
	Field  string `toml:"field,omitempty"`
	Value  any    `toml:"value,omitempty"`
	Values []any  `toml:"values,omitempty"`
}

// PromptAction sends text to a slot's agent.
type PromptAction struct {
	Slot string `toml:"slot"`
	Text string `toml:"text"`
}

// Action is what a rule does once its predicate holds. Exactly one of
// Prompt, Run, Finish, Escalate is set; see validateAction.
//
// Run's OnSuccess/OnFailure are themselves Actions, branching on the
// command's exit code (PLAN.md §3's "escape hatch" rule) — the only place
// nesting is allowed, since a prompt/finish/escalate has no exit code to
// branch on.
type Action struct {
	Prompt    *PromptAction `toml:"prompt,omitempty"`
	Run       []string      `toml:"run,omitempty"`
	OnSuccess *Action       `toml:"on_success,omitempty"`
	OnFailure *Action       `toml:"on_failure,omitempty"`
	Finish    string        `toml:"finish,omitempty"`
	Escalate  bool          `toml:"escalate,omitempty"`
}

// Rule is "when Predicate then Action".
//
// Timeout is the per-rule escape valve PLAN.md §3 allows as an alternative
// to loop.max_iterations for capping a rule cycle: a duration string
// (time.ParseDuration syntax, e.g. "30m"). Optional — most manifests rely
// on the loop-wide max_iterations instead.
type Rule struct {
	When    Predicate `toml:"when"`
	Then    Action    `toml:"then"`
	Timeout string    `toml:"timeout,omitempty"`
}

// BlockedRule is one exact prompt pattern on_blocked = "auto" is permitted
// to answer. PLAN.md §4.3 is explicit: exact patterns only, no wildcards —
// an orchestrator that blind-fires enter on a blocked agent is an agent
// granting itself permissions.
type BlockedRule struct {
	Pattern string `toml:"pattern"`
	Send    string `toml:"send"`
}

// Parse decodes and validates loop.toml. A non-nil error means the manifest
// must not run — every hard invariant in PLAN.md §3/§4.2/§4.3 is enforced
// here, at parse time, rather than left for the engine to discover mid-run.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if _, err := toml.Decode(string(data), &m); err != nil {
		return nil, fmt.Errorf("manifest: decode: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if err := m.validateSlots(); err != nil {
		return err
	}
	if err := m.validateOnBlocked(); err != nil {
		return err
	}

	slotNames := m.slotNameSet()
	for i, r := range m.Rules {
		if err := validatePredicate(r.When, slotNames); err != nil {
			return fmt.Errorf("manifest: rule %d: when: %w", i, err)
		}
		if err := validateAction(r.Then, slotNames); err != nil {
			return fmt.Errorf("manifest: rule %d: then: %w", i, err)
		}
		if r.Timeout != "" {
			if _, err := time.ParseDuration(r.Timeout); err != nil {
				return fmt.Errorf("manifest: rule %d: timeout: %w", i, err)
			}
		}
	}

	return m.validateCycles()
}

// validateSlots enforces slot-name uniqueness, the cwd-xor-worktree
// requirement, and the shared-cwd rule (PLAN.md §4.2).
func (m *Manifest) validateSlots() error {
	seen := make(map[string]bool, len(m.Slots))
	cwdOwner := make(map[string]string, len(m.Slots)) // cwd -> first slot that claimed it

	for i, s := range m.Slots {
		if s.Name == "" {
			return fmt.Errorf("manifest: slot %d: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("manifest: slot %q: duplicate slot name", s.Name)
		}
		seen[s.Name] = true

		hasCWD := s.CWD != ""
		hasWorktree := s.Worktree != nil
		if hasCWD == hasWorktree {
			return fmt.Errorf("manifest: slot %q: must set exactly one of cwd or worktree", s.Name)
		}
		if hasWorktree && s.Worktree.Branch == "" {
			return fmt.Errorf("manifest: slot %q: worktree.branch is required", s.Name)
		}

		if hasCWD {
			if owner, dup := cwdOwner[s.CWD]; dup && !m.Loop.AllowSharedCWD {
				return fmt.Errorf("manifest: slot %q and slot %q share cwd %q; set loop.allow_shared_cwd to allow this", owner, s.Name, s.CWD)
			}
			cwdOwner[s.CWD] = s.Name
		}
	}
	return nil
}

// validateOnBlocked enforces PLAN.md §4.3: auto requires a non-empty,
// wildcard-free whitelist. escalate/pause need nothing extra.
func (m *Manifest) validateOnBlocked() error {
	switch m.Loop.OnBlocked {
	case "", "escalate", "pause":
		return nil
	case "auto":
		if len(m.BlockedRules) == 0 {
			return errors.New(`manifest: on_blocked = "auto" requires a non-empty [[blocked_rule]] whitelist`)
		}
		for i, br := range m.BlockedRules {
			if br.Pattern == "" {
				return fmt.Errorf("manifest: blocked_rule %d: pattern is required", i)
			}
			// "*" only: prompt text legitimately ends in "?" ("Overwrite
			// existing file?"), so treating "?" as a wildcard would reject
			// the exact patterns this whitelist exists to allow.
			if strings.Contains(br.Pattern, "*") {
				return fmt.Errorf("manifest: blocked_rule %d: pattern %q contains a wildcard; on_blocked=auto allows exact patterns only", i, br.Pattern)
			}
		}
		return nil
	default:
		return fmt.Errorf("manifest: loop.on_blocked = %q: must be escalate, pause, or auto", m.Loop.OnBlocked)
	}
}

func (m *Manifest) slotNameSet() map[string]bool {
	set := make(map[string]bool, len(m.Slots))
	for _, s := range m.Slots {
		set[s.Name] = true
	}
	return set
}

// validatePredicate checks op/field shape and, for the field=="slot" idiom,
// that any slot name it names by value actually exists.
//
// That idiom — { op = "eq", field = "slot", value = "impl" } — is the only
// unambiguous slot reference a predicate can make: dotted fields like
// "status" or "review.handoff.verdict" mix real slot names with arbitrary
// data-path segments (PLAN.md §3's own sketch does this three different
// ways — see predicateSlots), so rejecting an unrecognised first segment
// there would misfire on a legitimate non-slot field. A typo'd field="slot"
// value, by contrast, can only ever mean a slot, so it is checked strictly:
// a rule that can never fire because it names a slot that doesn't exist is
// exactly the class of silent config error this package exists to catch.
func validatePredicate(p Predicate, slotNames map[string]bool) error {
	switch p.Op {
	case "all", "any":
		if len(p.Filters) == 0 {
			return fmt.Errorf("predicate op %q requires a non-empty filters list", p.Op)
		}
		for _, f := range p.Filters {
			if err := validatePredicate(f, slotNames); err != nil {
				return err
			}
		}
		return nil
	case "not":
		if p.Filter == nil {
			return errors.New(`predicate op "not" requires filter`)
		}
		return validatePredicate(*p.Filter, slotNames)
	case "eq":
		if p.Field == "" {
			return errors.New(`predicate op "eq" requires field`)
		}
		if p.Field == "slot" {
			if s, ok := p.Value.(string); ok && !slotNames[s] {
				return fmt.Errorf("predicate field \"slot\": slot %q is not defined", s)
			}
		}
		return nil
	case "in":
		if p.Field == "" || len(p.Values) == 0 {
			return errors.New(`predicate op "in" requires field and a non-empty values list`)
		}
		if p.Field == "slot" {
			for _, v := range p.Values {
				if s, ok := v.(string); ok && !slotNames[s] {
					return fmt.Errorf("predicate field \"slot\": slot %q is not defined", s)
				}
			}
		}
		return nil
	case "exists":
		if p.Field == "" {
			return errors.New(`predicate op "exists" requires field`)
		}
		return nil
	default:
		return fmt.Errorf("predicate: unknown op %q (want all/any/not/eq/in/exists)", p.Op)
	}
}

// validateAction enforces the exactly-one-variant rule, that on_success /
// on_failure only apply to run, and that every slot an action names
// (including through on_success/on_failure) actually exists.
func validateAction(a Action, slotNames map[string]bool) error {
	variants := 0
	if a.Prompt != nil {
		variants++
	}
	if a.Run != nil {
		variants++
	}
	if a.Finish != "" {
		variants++
	}
	if a.Escalate {
		variants++
	}
	if variants != 1 {
		return fmt.Errorf("action must set exactly one of prompt/run/finish/escalate, got %d", variants)
	}

	if a.Prompt != nil {
		if !slotNames[a.Prompt.Slot] {
			return fmt.Errorf("action prompt: slot %q is not defined", a.Prompt.Slot)
		}
		if a.Prompt.Text == "" {
			return errors.New("action prompt: text is required")
		}
	}

	if a.Run == nil {
		if a.OnSuccess != nil || a.OnFailure != nil {
			return errors.New("action: on_success/on_failure only apply to run")
		}
		return nil
	}

	if len(a.Run) == 0 {
		return errors.New("action run: argv must not be empty")
	}
	if a.OnSuccess != nil {
		if err := validateAction(*a.OnSuccess, slotNames); err != nil {
			return fmt.Errorf("on_success: %w", err)
		}
	}
	if a.OnFailure != nil {
		if err := validateAction(*a.OnFailure, slotNames); err != nil {
			return fmt.Errorf("on_failure: %w", err)
		}
	}
	return nil
}

// ruleEdge is one data-flow hop the cycle detector walks: rule i's predicate
// reads slot From's state, and rule i's action can prompt slot To.
type ruleEdge struct {
	from, to string
	rule     int
}

// buildEdges turns every rule into zero or more ruleEdges. A rule with no
// slot-scoped predicate field (e.g. a bare exists check unrelated to any
// slot) or a non-prompt action contributes no edges — it cannot participate
// in a retrigger cycle.
func (m *Manifest) buildEdges() []ruleEdge {
	slotNames := m.slotNameSet()
	var edges []ruleEdge
	for i, r := range m.Rules {
		triggers := predicateSlots(r.When, slotNames)
		if len(triggers) == 0 {
			continue
		}
		targets := actionTargets(r.Then, slotNames)
		for src := range triggers {
			for dst := range targets {
				edges = append(edges, ruleEdge{from: src, to: dst, rule: i})
			}
		}
	}
	return edges
}

// predicateSlots collects every defined slot name a predicate reads from.
//
// PLAN.md §3's own example addresses slots two different ways —
// { field = "slot", value = "impl" } and { field = "review.handoff.verdict" }
// (and even { field = "slot.verify.status" }, mixing both in one field) — so
// rather than lock in one positional convention, this treats any dot-segment
// of Field that matches a known slot name as a reference to it, plus the
// direct-value idiom when Field is literally "slot". That reading is correct
// for all three forms in the plan without guessing which one is canonical.
func predicateSlots(p Predicate, known map[string]bool) map[string]bool {
	out := map[string]bool{}
	collectPredicateSlots(p, known, out)
	return out
}

func collectPredicateSlots(p Predicate, known map[string]bool, out map[string]bool) {
	switch p.Op {
	case "all", "any":
		for _, f := range p.Filters {
			collectPredicateSlots(f, known, out)
		}
	case "not":
		if p.Filter != nil {
			collectPredicateSlots(*p.Filter, known, out)
		}
	case "eq", "in", "exists":
		for _, seg := range strings.Split(p.Field, ".") {
			if known[seg] {
				out[seg] = true
			}
		}
		if p.Field == "slot" {
			if s, ok := p.Value.(string); ok && known[s] {
				out[s] = true
			}
			for _, v := range p.Values {
				if s, ok := v.(string); ok && known[s] {
					out[s] = true
				}
			}
		}
	}
}

// actionTargets collects every slot an action can prompt, recursing through
// on_success/on_failure so a run rule's branches count too.
func actionTargets(a Action, known map[string]bool) map[string]bool {
	out := map[string]bool{}
	collectActionTargets(a, known, out)
	return out
}

func collectActionTargets(a Action, known map[string]bool, out map[string]bool) {
	if a.Prompt != nil && known[a.Prompt.Slot] {
		out[a.Prompt.Slot] = true
	}
	if a.OnSuccess != nil {
		collectActionTargets(*a.OnSuccess, known, out)
	}
	if a.OnFailure != nil {
		collectActionTargets(*a.OnFailure, known, out)
	}
}

// validateCycles is the enforcement of PLAN.md §3's termination invariant:
// "herdr-loop validate rejects any manifest containing a rule cycle without
// a retry cap or timeout." It runs Tarjan's algorithm over the slot
// data-flow graph (buildEdges) to find every strongly-connected component —
// every genuine cycle, not just an isolated pair — and requires each one to
// be covered by loop.max_iterations or a timeout on every rule that
// contributes an edge inside it.
//
// This is real graph analysis, not a proxy check for "does max_iterations
// exist somewhere in the file": an acyclic manifest is accepted with no cap
// at all, and max_iterations is loop-wide, so setting it once covers every
// cycle the graph contains. A cycle is only rejected when neither that
// global cap nor a per-rule timeout actually applies to it.
func (m *Manifest) validateCycles() error {
	edges := m.buildEdges()
	if len(edges) == 0 {
		return nil
	}

	adj := map[string][]ruleEdge{}
	nodeSet := map[string]bool{}
	for _, e := range edges {
		adj[e.from] = append(adj[e.from], e)
		nodeSet[e.from] = true
		nodeSet[e.to] = true
	}
	nodes := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	// Deterministic order so which cycle is reported first (if several are
	// uncapped) doesn't vary between runs of the same manifest.
	sort.Strings(nodes)

	for _, scc := range tarjanSCCs(nodes, adj) {
		inSCC := make(map[string]bool, len(scc))
		for _, s := range scc {
			inSCC[s] = true
		}

		var ruleIdx []int
		selfLoop := false
		for _, e := range edges {
			if inSCC[e.from] && inSCC[e.to] {
				ruleIdx = append(ruleIdx, e.rule)
				if e.from == e.to {
					selfLoop = true
				}
			}
		}
		if len(scc) == 1 && !selfLoop {
			continue // a singleton with no self-loop is not a cycle
		}
		if m.Loop.MaxIterations > 0 {
			continue // the global retry cap covers every cycle in the manifest
		}
		if rulesAllTimedOut(m.Rules, ruleIdx) {
			continue // every rule in this specific cycle has its own cap
		}

		sort.Strings(scc)
		return fmt.Errorf("manifest: rule cycle through slot(s) %s has no retry cap (loop.max_iterations) or timeout on every participating rule",
			strings.Join(scc, " -> "))
	}
	return nil
}

func rulesAllTimedOut(rules []Rule, idx []int) bool {
	if len(idx) == 0 {
		return false
	}
	for _, i := range idx {
		if rules[i].Timeout == "" {
			return false
		}
		if _, err := time.ParseDuration(rules[i].Timeout); err != nil {
			return false // already reported by validate's own timeout check
		}
	}
	return true
}

// tarjanSCCs returns every strongly-connected component of the graph
// described by adj, using Tarjan's algorithm (single DFS pass, O(V+E)).
// Every cycle in a directed graph is contained within exactly one SCC, so
// walking SCCs finds every cycle without enumerating them individually.
func tarjanSCCs(nodes []string, adj map[string][]ruleEdge) [][]string {
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
	adj     map[string][]ruleEdge
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

	for _, e := range t.adj[v] {
		w := e.to
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
