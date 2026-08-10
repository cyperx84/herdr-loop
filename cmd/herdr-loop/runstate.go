package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// runState is what `run` persists so `status` and `stop` can find a live
// loop without a connection to it. herdr's socket API has no concept of a
// herdr-loop instance to query — this plugin's loop state is not herdr's
// business (PLAN §1: "no Herdr-managed persistent storage API — plugins own
// their state dir") — so this file is the entire persistence layer: one
// loop, one file, written atomically so a reader never observes a
// half-written one.
type runState struct {
	LoopName     string    `json:"loop_name"`
	ManifestPath string    `json:"manifest_path"`
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
}

// stateDir resolves where herdr-loop persists run state.
//
// Inside a herdr pane, HERDR_PLUGIN_STATE_DIR is herdr's own answer to
// "where does this plugin keep files across restarts" (PLAN §1's env var
// table). Outside one — running `herdr-loop status` from a plain shell to
// check on a pane-hosted loop is an explicit design goal, PLAN §2's CLI-vs-
// socket split — fall back to a user state dir so the command still works.
func stateDir() (string, error) {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(base, "herdr-loop"), nil
}

func runStatePath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "run.json"), nil
}

// writeRunState installs rs atomically: encode to a temp file in the same
// directory, then rename over the target. A reader (status, stop) never sees
// a partially written file, only the old one or the new one.
func writeRunState(rs runState) error {
	path, err := runStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	data, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write run state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install run state: %w", err)
	}
	return nil
}

// readRunState returns the persisted state. The error satisfies os.IsNotExist
// when no loop has ever run (or the last one cleaned up after itself) —
// callers branch on that rather than treating it as failure.
func readRunState() (runState, error) {
	path, err := runStatePath()
	if err != nil {
		return runState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return runState{}, err
	}
	var rs runState
	if err := json.Unmarshal(data, &rs); err != nil {
		return runState{}, fmt.Errorf("decode run state %s: %w", path, err)
	}
	return rs, nil
}

// clearRunState removes the persisted state. Removing an already-absent file
// is not an error — both run's normal shutdown and status's stale-pid repair
// call this, and either may race the other losing harmlessly.
func clearRunState() error {
	path, err := runStatePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove run state: %w", err)
	}
	return nil
}
