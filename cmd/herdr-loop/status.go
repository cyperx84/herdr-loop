package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

// statusReport is cmdStatus's output shape, shared between the human and
// --json forms so they can never drift apart.
type statusReport struct {
	Running bool `json:"running"`
	// Stale is true when a run.json was found but its pid is dead — the
	// status-lies failure PLAN §4.9 warns about, applied to this plugin's own
	// bookkeeping: a pid file is exactly the kind of "reported fine" state
	// that must be corroborated, not trusted, before being reported as
	// running.
	Stale        bool      `json:"stale,omitempty"`
	LoopName     string    `json:"loop_name,omitempty"`
	ManifestPath string    `json:"manifest_path,omitempty"`
	PID          int       `json:"pid,omitempty"`
	StartedAt    time.Time `json:"started_at,omitzero"`
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rs, err := readRunState()
	if err != nil {
		if os.IsNotExist(err) {
			return printStatus(*asJSON, statusReport{})
		}
		return fmt.Errorf("status: %w", err)
	}

	report := statusReport{
		Running:      true,
		LoopName:     rs.LoopName,
		ManifestPath: rs.ManifestPath,
		PID:          rs.PID,
		StartedAt:    rs.StartedAt,
	}
	if !processAlive(rs.PID) {
		// Corroborate before reporting: the recorded pid is dead, so this is
		// not a running loop no matter what the file says. Clear it so the
		// next run/status starts from a clean slate rather than re-deriving
		// the same stale answer every time.
		report.Running = false
		report.Stale = true
		if err := clearRunState(); err != nil {
			fmt.Fprintln(os.Stderr, "herdr-loop: status: could not clear stale run state:", err)
		}
	}
	return printStatus(*asJSON, report)
}

func printStatus(asJSON bool, r statusReport) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	if !r.Running {
		if r.Stale {
			fmt.Println("not running (stale pid file cleared)")
		} else {
			fmt.Println("not running")
		}
		return nil
	}
	fmt.Printf("running: loop=%q manifest=%s pid=%d started=%s\n",
		r.LoopName, r.ManifestPath, r.PID, r.StartedAt.Format(time.RFC3339))
	return nil
}
