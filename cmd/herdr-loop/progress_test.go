package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	herdr "github.com/cyperx84/herdr-api"
	"github.com/cyperx84/herdr-loop/internal/state"
)

// Redirect the state dir so tests never touch the real one.
func isolateStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	got, err := stateDir()
	if err != nil {
		t.Fatalf("stateDir: %v", err)
	}
	if got != dir {
		t.Fatalf("stateDir = %q, want the injected %q — this test would otherwise write to the real state dir", got, dir)
	}
	return dir
}

// The snapshot is what makes a running loop watchable rather than a black box
// (PLAN §4c). It must survive a round trip with the fields a watcher needs:
// which slot, how far along, and — critically — whether the status can be
// trusted or was inferred from the screen.
func TestProgressSnapshotRoundTrip(t *testing.T) {
	isolateStateDir(t)

	w, err := newProgressWriter("demo", 10)
	if err != nil {
		t.Fatalf("newProgressWriter: %v", err)
	}
	defer w.Close()

	since := time.Now().Add(-90 * time.Second).UTC()
	w.Update([]slotProgress{
		{Slot: "impl", Kind: "claude", Status: herdr.StatusWorking, Tier: state.TierScreen, Pane: "w1:p1", Since: since},
		{Slot: "review", Kind: "opencode", Status: herdr.StatusIdle, Tier: state.TierStructured, Pane: "w1:p2", Halted: true},
	}, 3, true, 1)

	got, err := readProgress()
	if err != nil {
		t.Fatalf("readProgress: %v", err)
	}
	if got.LoopName != "demo" || got.Iteration != 3 || got.MaxIters != 10 || !got.Live {
		t.Errorf("snapshot header wrong: %+v", got)
	}
	if got.Escalations != 1 {
		t.Errorf("Escalations = %d, want 1", got.Escalations)
	}
	if len(got.Slots) != 2 {
		t.Fatalf("slots = %d, want 2", len(got.Slots))
	}
	if got.Slots[0].Tier != state.TierScreen {
		t.Errorf("tier lost in the round trip (%q) — a watcher cannot judge a 'done' without it", got.Slots[0].Tier)
	}
	if !got.Slots[1].Halted {
		t.Error("halted flag lost; a halted slot that reads as running is the status-lie this surface exists to prevent")
	}
	if got.Slots[0].Since.IsZero() {
		t.Error("Since lost; without it nobody can tell a working slot from a stuck one")
	}
}

// The event log is append-only and one JSON object per line, so it can be
// tailed and parsed by anything. Rewriting or interleaving would break both.
func TestEventLogIsAppendOnlyJSONL(t *testing.T) {
	dir := isolateStateDir(t)

	w, err := newProgressWriter("demo", 5)
	if err != nil {
		t.Fatalf("newProgressWriter: %v", err)
	}
	w.Append(logEntry{Event: "loop_started", Detail: "loop.toml"})
	w.Append(logEntry{Event: "slot_status", Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusDone})
	w.Append(logEntry{Event: "escalation", Slot: "impl", Detail: "prompt template did not resolve"})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := os.Open(filepath.Join(dir, eventsFile))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	var entries []logEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e logEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line is not valid JSON (%q): %v", sc.Text(), err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 — the log must append, never rewrite", len(entries))
	}
	if entries[0].Event != "loop_started" || entries[2].Event != "escalation" {
		t.Errorf("order not preserved: %+v", entries)
	}
	if entries[2].Detail == "" {
		t.Error("escalation logged without a reason — PLAN §4.11 requires the why, not just the fact")
	}
	for i, e := range entries {
		if e.At.IsZero() {
			t.Errorf("entry %d has no timestamp; an ordered history needs one", i)
		}
	}

	// A second writer must extend the history, not truncate it — a loop that
	// restarts should not erase what happened before.
	w2, err := newProgressWriter("demo", 5)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	w2.Append(logEntry{Event: "loop_started", Detail: "restart"})
	w2.Close()

	b, err := os.ReadFile(filepath.Join(dir, eventsFile))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := countLines(b); n != 4 {
		t.Errorf("lines after reopen = %d, want 4 — reopening truncated the history", n)
	}
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// A reader polling progress.json must never catch it half-written. The
// ecosystem sweep found that exact corruption reported eight times over
// against Claude Code's own config file; temp-file-plus-rename avoids it.
func TestProgressSnapshotIsAlwaysCompleteJSON(t *testing.T) {
	isolateStateDir(t)

	w, err := newProgressWriter("demo", 100)
	if err != nil {
		t.Fatalf("newProgressWriter: %v", err)
	}
	defer w.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			w.Update([]slotProgress{{Slot: "impl", Status: herdr.StatusWorking}}, i, true, 0)
		}
	}()

	for i := 0; i < 200; i++ {
		snap, err := readProgress()
		if err != nil {
			if os.IsNotExist(err) {
				continue // not written yet
			}
			t.Fatalf("read %d caught a partial write: %v", i, err)
		}
		if snap.LoopName != "demo" {
			t.Fatalf("read %d saw a torn snapshot: %+v", i, snap)
		}
	}
	<-done
}

// Stale progress must not outlive its loop: a dead run whose progress file
// still reads as live is the same lie one layer down.
func TestClearProgressRemovesBothSurfaces(t *testing.T) {
	dir := isolateStateDir(t)

	w, err := newProgressWriter("demo", 5)
	if err != nil {
		t.Fatalf("newProgressWriter: %v", err)
	}
	w.Append(logEntry{Event: "loop_started"})
	w.Update([]slotProgress{{Slot: "impl"}}, 1, true, 0)
	w.Close()

	if err := clearProgress(); err != nil {
		t.Fatalf("clearProgress: %v", err)
	}
	for _, name := range []string{progressFile, eventsFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived clearProgress (err=%v)", name, err)
		}
	}
	// Clearing an already-clean dir is not an error: status calls it on every
	// stale run it finds.
	if err := clearProgress(); err != nil {
		t.Errorf("clearProgress on a clean dir returned %v, want nil", err)
	}
}

// A nil writer is the "progress surface unavailable" path. Every method must
// tolerate it, because losing observability must never kill a working loop.
func TestNilProgressWriterIsSafe(t *testing.T) {
	var w *progressWriter
	w.Append(logEntry{Event: "slot_status"})
	w.Update(nil, 0, false, 0)
	if err := w.Close(); err != nil {
		t.Errorf("Close on a nil writer = %v, want nil", err)
	}
}
