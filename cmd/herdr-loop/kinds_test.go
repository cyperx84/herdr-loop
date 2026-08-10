package main

import (
	"strings"
	"testing"

	"github.com/cyperx84/herdr-loop/internal/engine"
)

// Regression for the F2 review finding. Engine.Spawn holds a kind's
// concurrency token for the agent's lifetime, so a manifest with more slots of
// a kind than its cap allows used to block in acquireKind forever — the
// process hung at startup with no log line after the first successful spawn.
// A config error must never present as a silent hang.
func TestKindCapacityRejectsMoreSlotsThanCap(t *testing.T) {
	slots := []engine.SlotConfig{
		{Name: "impl", Kind: "claude"},
		{Name: "review", Kind: "claude"},
	}

	err := checkKindCapacity(slots, nil) // no kinds.toml: every kind falls to the default of 1
	if err == nil {
		t.Fatal("two claude slots under max_concurrent=1 were accepted — this manifest can never start")
	}
	for _, want := range []string{"claude", "max_concurrent", "kinds.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so the user can act on it; got: %v", want, err)
		}
	}

	// Raising the cap deliberately is the documented way out.
	raised := map[string]engine.KindConfig{"claude": {MaxConcurrent: 2}}
	if err := checkKindCapacity(slots, raised); err != nil {
		t.Errorf("raising max_concurrent to 2 should admit two slots, got: %v", err)
	}
}

// Distinct kinds must not be charged against each other's caps — this is the
// ordinary multi-harness case the tool exists for, and the shipped examples
// all take this path.
func TestKindCapacityCountsPerKind(t *testing.T) {
	slots := []engine.SlotConfig{
		{Name: "impl", Kind: "claude"},
		{Name: "review", Kind: "codex"},
		{Name: "verify", Kind: "opencode"},
	}
	if err := checkKindCapacity(slots, nil); err != nil {
		t.Errorf("three slots of three different kinds are within a per-kind cap of 1, got: %v", err)
	}
}
