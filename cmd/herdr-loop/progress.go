package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	herdr "github.com/cyperx84/herdr-api"
	"github.com/cyperx84/herdr-loop/internal/state"
)

// Per-slot progress and an append-only log, so a running loop is watchable
// instead of a black box until it converges (PLAN §4c, demand item 4).
//
// This is the only finding in the ecosystem sweep where a missing feature was
// named as blocking someone's adoption of herdr itself: an orchestrator
// driving N workers could see free-text status and nothing else, so with five
// or ten agents running it could not tell how far along any of them was.
//
// Two surfaces, because they answer different questions:
//
//   - progress.json — a snapshot: where is every slot right now. Rewritten
//     atomically on every change, so a reader never sees a half-written file.
//   - events.jsonl — an append-only history: what happened, in order. One JSON
//     object per line, so it can be tailed, grepped, or replayed by anything
//     that can read a line.
//
// Both live in the same state dir as run.json and are cleared with it.
const (
	progressFile = "progress.json"
	eventsFile   = "events.jsonl"
)

// slotProgress is one slot's live state.
type slotProgress struct {
	Slot   string            `json:"slot"`
	Kind   string            `json:"kind"`
	Status herdr.AgentStatus `json:"status"`
	// Tier records whether Status was self-reported by the agent or inferred
	// by classifying the screen (PLAN §4.6). A reader deciding whether to
	// trust a "done" needs this as much as the status itself.
	Tier state.Tier `json:"tier"`
	Pane string     `json:"pane,omitempty"`
	// Since is when the slot entered Status — the number that answers "is
	// this one stuck?", which a bare status cannot.
	Since time.Time `json:"since,omitzero"`
	// LastRule is the rule that last acted on this slot.
	LastRule string `json:"last_rule,omitempty"`
	// Halted is set when escalation stopped the loop touching this slot.
	Halted bool `json:"halted,omitempty"`
}

// progressSnapshot is the whole loop's live state.
type progressSnapshot struct {
	LoopName    string         `json:"loop_name"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Iteration   int            `json:"iteration"`
	MaxIters    int            `json:"max_iterations"`
	Live        bool           `json:"live"`
	Slots       []slotProgress `json:"slots"`
	Escalations int            `json:"escalations,omitempty"`
}

// logEntry is one line of events.jsonl.
type logEntry struct {
	At     time.Time         `json:"at"`
	Event  string            `json:"event"`
	Slot   string            `json:"slot,omitempty"`
	From   herdr.AgentStatus `json:"from,omitempty"`
	To     herdr.AgentStatus `json:"to,omitempty"`
	Rule   string            `json:"rule,omitempty"`
	Detail string            `json:"detail,omitempty"`
}

// progressWriter publishes both surfaces. It is safe for concurrent use.
type progressWriter struct {
	dir string

	mu   sync.Mutex
	snap progressSnapshot
	log  *os.File
}

// newProgressWriter opens the append-only log and seeds the snapshot.
//
// A failure here is not fatal to a run: losing observability is bad, but
// killing a loop that is otherwise working because a log file could not be
// opened would be worse. Callers log the error and carry on with a nil writer,
// which every method tolerates.
func newProgressWriter(loopName string, maxIters int) (*progressWriter, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("progress: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, eventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("progress: open event log: %w", err)
	}
	return &progressWriter{
		dir:  dir,
		log:  f,
		snap: progressSnapshot{LoopName: loopName, MaxIters: maxIters},
	}, nil
}

// Update replaces the snapshot's slot table and rewrites progress.json.
func (w *progressWriter) Update(slots []slotProgress, iteration int, live bool, escalations int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.snap.Slots = slots
	w.snap.Iteration = iteration
	w.snap.Live = live
	w.snap.Escalations = escalations
	w.snap.UpdatedAt = time.Now().UTC()
	w.writeSnapshotLocked()
}

// Append adds one line to the event log.
func (w *progressWriter) Append(e logEntry) {
	if w == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// A short write or an error here is deliberately swallowed: the event log
	// is observability, and failing a loop because its log is full would turn
	// a cosmetic problem into an outage.
	_, _ = w.log.Write(append(b, '\n'))
}

// Close flushes and releases the log.
func (w *progressWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.log.Close()
}

// writeSnapshotLocked rewrites progress.json atomically.
//
// Temp file plus rename, not truncate-and-write. A reader polling this file
// must never catch it half-written — that is the concurrent-write corruption
// the ecosystem sweep found reported eight times over against Claude Code's
// own config, and it is trivially avoidable here.
func (w *progressWriter) writeSnapshotLocked() {
	b, err := json.MarshalIndent(w.snap, "", "  ")
	if err != nil {
		return
	}
	final := filepath.Join(w.dir, progressFile)
	tmp, err := os.CreateTemp(w.dir, progressFile+".*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Rename(name, final); err != nil {
		os.Remove(name)
	}
}

// readProgress loads the snapshot a running loop publishes. A missing file is
// reported as os.ErrNotExist so callers can distinguish "no loop running" from
// "could not read".
func readProgress() (progressSnapshot, error) {
	dir, err := stateDir()
	if err != nil {
		return progressSnapshot{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, progressFile))
	if err != nil {
		return progressSnapshot{}, err
	}
	var snap progressSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return progressSnapshot{}, fmt.Errorf("progress: %s is not readable: %w", progressFile, err)
	}
	return snap, nil
}

// clearProgress removes both surfaces. Called wherever run state is cleared,
// so a stale loop does not leave a progress file that reads as live.
func clearProgress() error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	for _, f := range []string{progressFile, eventsFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
