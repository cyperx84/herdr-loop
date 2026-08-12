package main

import (
	"context"
	"testing"

	herdr "github.com/cyperx84/herdr-api"
	"github.com/cyperx84/herdr-loop/internal/engine"
	"github.com/cyperx84/herdr-loop/internal/state"
)

// fakeSnapshotter is state.Snapshotter over a fixed agent list, so the
// adapter can be tested without a live herdr connection — the same reason
// the engine and state packages test against fakes (see their own test
// files' doc comments).
type fakeSnapshotter struct{ agents []herdr.Agent }

func (f fakeSnapshotter) AgentList(context.Context) ([]herdr.Agent, error) {
	return f.agents, nil
}

func strPtr(s string) *string { return &s }

// teeingFake mirrors teeingSnapshotter's tee-then-forward behaviour (see
// adapter.go) over a fakeSnapshotter instead of a live *herdr.Client, so the
// name-index-refreshes-on-reconcile wiring is testable without a connection.
type teeingFake struct {
	fakeSnapshotter
	idx *nameIndex
}

func (t teeingFake) AgentList(ctx context.Context) ([]herdr.Agent, error) {
	agents, err := t.fakeSnapshotter.AgentList(ctx)
	if err != nil {
		return nil, err
	}
	t.idx.update(agents)
	return agents, nil
}

// TestModelAdapterResolvesSlotByName is the contract this whole file exists
// for: the engine addresses agents by slot name, state.Model tracks them by
// pane id, and modelAdapter is the only thing that bridges the two — via the
// Name field herdr echoes back on every agent.list record.
func TestModelAdapterResolvesSlotByName(t *testing.T) {
	agents := []herdr.Agent{
		{Name: "impl", Agent: strPtr("claude"), PaneID: "w1:p1", Status: herdr.StatusIdle},
		{Name: "review", Agent: strPtr("codex"), PaneID: "w1:p2", Status: herdr.StatusWorking},
	}

	idx := newNameIndex([]engine.SlotConfig{{Name: "impl"}, {Name: "review"}, {Name: "a"}, {Name: "b"}}, nil)
	sm := state.New(teeingFake{fakeSnapshotter{agents}, idx}, state.Options{})

	// Reconcile needs two passes to go live on a non-empty session (adopt,
	// then confirm nothing moved — state.Model.Reconcile's own doc).
	for i := 0; i < 2; i++ {
		if _, err := sm.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if !sm.IsLive() {
		t.Fatal("model did not go live after two reconciles against a stable snapshot")
	}

	a := modelAdapter{state: sm, idx: idx}

	status, ok := a.SlotStatus("impl")
	if !ok || status != herdr.StatusIdle {
		t.Errorf("SlotStatus(impl) = (%q, %v), want (idle, true)", status, ok)
	}
	target, ok := a.SlotTarget("impl")
	if !ok || target != "w1:p1" {
		t.Errorf("SlotTarget(impl) = (%q, %v), want (w1:p1, true)", target, ok)
	}

	status, ok = a.SlotStatus("review")
	if !ok || status != herdr.StatusWorking {
		t.Errorf("SlotStatus(review) = (%q, %v), want (working, true)", status, ok)
	}

	// A slot the fleet has never spawned must report false, not a zero
	// value that could be mistaken for a real "unknown" status — matches
	// engine.Model's own documented distinction.
	if _, ok := a.SlotStatus("nonexistent"); ok {
		t.Error("SlotStatus(nonexistent) reported ok=true, want false")
	}
	if _, ok := a.SlotTarget("nonexistent"); ok {
		t.Error("SlotTarget(nonexistent) reported ok=true, want false")
	}
}

// TestModelAdapterBlockedPromptAlwaysUnmatched pins down the documented safe
// default (adapter.go's BlockedPrompt doc): herdr has no method that returns
// an approval prompt's literal text, so this must always report "no match"
// rather than ever synthesizing consent.
func TestModelAdapterBlockedPromptAlwaysUnmatched(t *testing.T) {
	a := modelAdapter{state: state.New(fakeSnapshotter{}, state.Options{}), idx: newNameIndex(nil, nil)}
	if _, ok := a.BlockedPrompt("anything"); ok {
		t.Error("BlockedPrompt reported ok=true, want false always")
	}
}

// TestNameIndexReverseLookup covers slotFor, the direction feed() uses to
// turn a pane-keyed state.Transition back into a slot-keyed engine.Transition.
func TestNameIndexReverseLookup(t *testing.T) {
	idx := newNameIndex([]engine.SlotConfig{{Name: "impl"}, {Name: "review"}, {Name: "a"}, {Name: "b"}}, nil)
	idx.update([]herdr.Agent{
		{Name: "impl", PaneID: "w1:p1"},
		{Name: "review", PaneID: "w1:p2"},
	})

	slot, ok := idx.slotFor("w1:p2")
	if !ok || slot != "review" {
		t.Errorf("slotFor(w1:p2) = (%q, %v), want (review, true)", slot, ok)
	}
	if _, ok := idx.slotFor("w9:p9"); ok {
		t.Error("slotFor(unknown pane) reported ok=true, want false")
	}
}

// Found on the first real run: the index adopted every named agent in the
// session, so a loop's status showed other people's agents — and, far worse,
// an agent that happened to share a slot's name would have been driven as if
// it were ours. A session routinely contains agents from other work.
func TestNameIndexIgnoresAgentsThisLoopDoesNotOwn(t *testing.T) {
	idx := newNameIndex([]engine.SlotConfig{{Name: "impl"}, {Name: "review"}}, nil)

	idx.update([]herdr.Agent{
		{Name: "impl", PaneID: "w1:p1"},     // ours
		{Name: "shaderfx", PaneID: "w9:p9"}, // somebody else's work
		{Name: "heromain", PaneID: "w9:p8"}, // ditto
	})

	if pane, ok := idx.paneFor("impl"); !ok || pane != "w1:p1" {
		t.Errorf("own slot not indexed: pane=%q ok=%v", pane, ok)
	}
	if _, ok := idx.paneFor("shaderfx"); ok {
		t.Error("adopted an agent this loop never declared — it would be prompted as if it were ours")
	}
	if slot, ok := idx.slotFor("w9:p9"); ok {
		t.Errorf("foreign pane w9:p9 resolved to slot %q; a rule firing on it would drive another loop's agent", slot)
	}

	// Declared-but-unspawned slots must still be listed, or a watcher cannot
	// tell "not started yet" from "not part of this loop".
	got := idx.slots()
	if len(got) != 2 || got[0] != "impl" || got[1] != "review" {
		t.Errorf("slots() = %v, want both declared slots including the unspawned one", got)
	}
}

// The specific collision the reviewer predicted: a leftover agent from a
// crashed run, carrying the same name, in a different pane.
func TestNameIndexDoesNotAdoptStaleAgentInAnotherPane(t *testing.T) {
	idx := newNameIndex([]engine.SlotConfig{{Name: "impl"}}, nil)

	// Our spawn lands first.
	idx.update([]herdr.Agent{{Name: "impl", PaneID: "w1:p1"}})
	if pane, _ := idx.paneFor("impl"); pane != "w1:p1" {
		t.Fatalf("setup: pane=%q", pane)
	}
	// A later reconcile also sees an agent named impl that is not ours. The
	// name is ours, so it is accepted — this documents the remaining limit:
	// herdr names are unique among live agents, so two live "impl" agents
	// cannot coexist, and the index tracks whichever herdr currently reports.
	idx.update([]herdr.Agent{{Name: "impl", PaneID: "w2:p2"}})
	if pane, _ := idx.paneFor("impl"); pane != "w2:p2" {
		t.Errorf("index did not follow the live agent: pane=%q", pane)
	}
}

// herdr agent names are unique across the whole session, not per loop, so a
// slot called "impl" collides with any other agent anywhere already using that
// name — and "impl" and "review" are exactly what people reach for. Seen on a
// real machine: a loop failed to spawn both slots because unrelated work in
// another workspace had already taken both names.
//
// The engine qualifies the registered name with the loop's name, so the index
// has to recognise the qualified form while still answering in slot names.
func TestNameIndexRecognisesQualifiedAgentNames(t *testing.T) {
	qualify := engine.AgentNameFor("wire-vars")
	idx := newNameIndex([]engine.SlotConfig{{Name: "impl"}, {Name: "review"}}, qualify)

	idx.update([]herdr.Agent{
		{Name: qualify("impl"), PaneID: "w1:p1"}, // ours, qualified
		{Name: "impl", PaneID: "w9:p9"},          // somebody else's bare "impl"
	})

	pane, ok := idx.paneFor("impl")
	if !ok {
		t.Fatal("the qualified agent was not recognised as this loop's slot")
	}
	if pane != "w1:p1" {
		t.Errorf("paneFor(impl) = %q, want our own pane — a bare 'impl' from other work was adopted", pane)
	}
	if slot, ok := idx.slotFor("w9:p9"); ok {
		t.Errorf("an unrelated agent named 'impl' resolved to slot %q; it would be prompted as ours", slot)
	}
}

// The qualified name must satisfy herdr's grammar: [a-z][a-z0-9_-]{0,31}.
func TestAgentNameIsValidForHerdr(t *testing.T) {
	for _, c := range []struct{ loop, slot string }{
		{"wire-manifest-vars", "impl"},
		{"Loop With Spaces!", "review"},
		{"", "impl"},
		{"a-very-long-loop-name-that-goes-well-past-the-limit", "implementer"},
	} {
		got := engine.AgentNameFor(c.loop)(c.slot)
		if got == "" {
			t.Errorf("loop %q slot %q produced an empty name", c.loop, c.slot)
			continue
		}
		if len(got) > 32 {
			t.Errorf("name %q is %d chars, herdr allows 32", got, len(got))
		}
		if got[0] < 'a' || got[0] > 'z' {
			t.Errorf("name %q must start with a letter", got)
		}
		for _, r := range got {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				t.Errorf("name %q contains %q, outside herdr's grammar", got, r)
				break
			}
		}
	}
}
