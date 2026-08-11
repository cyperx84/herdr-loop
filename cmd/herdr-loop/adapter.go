package main

import (
	"context"
	"sort"
	"sync"

	herdr "github.com/cyperx84/herdr-api"
	"github.com/cyperx84/herdr-loop/internal/engine"
	"github.com/cyperx84/herdr-loop/internal/state"
)

// nameIndex maps a slot name to the pane id herdr assigned the agent spawned
// for it.
//
// engine.Model is keyed by slot name (PLAN's noun); state.Model is keyed by
// pane id (the wire's noun) and deliberately carries no name field — coupling
// that package to this plugin's supervisor-only concept of a "slot" would
// make it reusable by nothing else. herdr itself is the thing that already
// ties the two together: engine.Spawn passes AgentStartParams.Name = slot,
// and agent.list echoes that name straight back on every Agent record. This
// index is just a cache of that echo, so it is this package's job — the
// bridge between the two other packages — rather than either of theirs.
type nameIndex struct {
	mu     sync.RWMutex
	byName map[string]string // slot name -> pane id
}

func newNameIndex() *nameIndex {
	return &nameIndex{byName: make(map[string]string)}
}

// slots lists every slot name the index knows, sorted so progress output is
// stable between reads rather than reordering on every map iteration.
func (n *nameIndex) slots() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]string, 0, len(n.byName))
	for name := range n.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// update folds the Name/PaneID of every agent.list record into the index. It
// is safe to call with an empty or partial list — a slot not yet spawned
// simply has no entry, which paneFor's ok=false already handles.
func (n *nameIndex) update(agents []herdr.Agent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, a := range agents {
		if a.Name != "" && a.PaneID != "" {
			n.byName[a.Name] = a.PaneID
		}
	}
}

// paneFor reports the pane id for a slot name, if the index has learned one.
func (n *nameIndex) paneFor(slot string) (string, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	p, ok := n.byName[slot]
	return p, ok
}

// slotFor reports the slot name for a pane id, the reverse of paneFor.
//
// Linear in the number of slots, which is fine: a herdr-loop fleet is a
// handful of agents (§4.7's concurrency caps see to that), never the
// thousands where an O(n) reverse lookup would matter.
func (n *nameIndex) slotFor(paneID string) (string, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for name, p := range n.byName {
		if p == paneID {
			return name, true
		}
	}
	return "", false
}

// teeingSnapshotter satisfies state.Snapshotter by forwarding AgentList to a
// real client while also feeding every answer's Name field into idx.
//
// state.Model already calls AgentList on its own reconcile schedule (§4.8) —
// teeing that call is how the slot index stays fresh with zero extra socket
// round-trips, rather than this package polling agent.list on a second timer
// that would race the model's own view of the same data.
type teeingSnapshotter struct {
	client *herdr.Client
	idx    *nameIndex
}

func (t teeingSnapshotter) AgentList(ctx context.Context) ([]herdr.Agent, error) {
	agents, err := t.client.AgentList(ctx)
	if err != nil {
		return nil, err
	}
	t.idx.update(agents)
	return agents, nil
}

// modelAdapter satisfies engine.Model over a reconciled state.Model, using
// idx to translate the engine's slot-name keys into the pane ids state.Model
// actually tracks.
type modelAdapter struct {
	state *state.Model
	idx   *nameIndex
}

var _ engine.Model = modelAdapter{}

// SlotStatus reports a slot's reconciled status. False whenever either half
// of the lookup is unresolved — the slot has no known pane yet, or the model
// has not (or no longer) tracked that pane — which engine.Model's own doc
// requires: "not the same as a slot whose status is unknown."
func (a modelAdapter) SlotStatus(slot string) (herdr.AgentStatus, bool) {
	pane, ok := a.idx.paneFor(slot)
	if !ok {
		return "", false
	}
	ag, ok := a.state.Get(pane)
	if !ok {
		return "", false
	}
	return ag.Status, true
}

// SlotTarget reports the pane id to address a slot by. herdr's own target
// resolution (agent.prompt, agent.send_keys) accepts a pane id directly, so
// no further translation is needed once the index has one.
func (a modelAdapter) SlotTarget(slot string) (string, bool) {
	return a.idx.paneFor(slot)
}

// BlockedPrompt always reports false — herdr's socket API has no method that
// returns the literal text of an approval/question prompt a blocked agent is
// sitting on. pane.read only returns terminal output, which §4.1 already
// rules out as this plugin's result-transport; using it here to drive policy
// would be exactly the brittle pane-scraping the whole design exists to
// avoid (PLAN §4b). engine.Model's own doc names false as the safe answer for
// this case — the auto blocked policy treats it as "unmatched," never as
// consent — so every blocked slot escalates until a verified way to read the
// prompt text exists.
func (a modelAdapter) BlockedPrompt(slot string) (string, bool) {
	return "", false
}

// SlotTier reports how herdr learned this slot's status. False when the slot
// has no known pane yet or the model no longer tracks it — the same
// unresolved-lookup rule SlotStatus follows, so a caller can never mistake
// "not tracked" for "screen-classified".
func (a modelAdapter) SlotTier(slot string) (state.Tier, bool) {
	pane, ok := a.idx.paneFor(slot)
	if !ok {
		return "", false
	}
	ag, ok := a.state.Get(pane)
	if !ok {
		return "", false
	}
	return ag.Tier, true
}
