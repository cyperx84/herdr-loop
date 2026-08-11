package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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

	// Progress is the per-slot view a running loop publishes (PLAN §4c). Nil
	// when no loop is running or it has not published yet — a watcher needs
	// to tell "no progress reported" from "no slots", which an empty slice
	// would conflate.
	Progress *progressSnapshot `json:"progress,omitempty"`
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
	if snap, err := readProgress(); err == nil {
		report.Progress = &snap
	} else if !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "herdr-loop: status: could not read progress:", err)
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
		// The progress file outlived its loop, so it describes a dead run.
		// Leaving it would be exactly the status-lie this command exists to
		// catch, one layer down.
		report.Progress = nil
		if err := clearProgress(); err != nil {
			fmt.Fprintln(os.Stderr, "herdr-loop: status: could not clear stale progress:", err)
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

	p := r.Progress
	if p == nil {
		fmt.Println("no progress published yet")
		return nil
	}

	live := "replay (rules not firing yet)"
	if p.Live {
		live = "live"
	}
	fmt.Printf("iteration %d/%d · %s", p.Iteration, p.MaxIters, live)
	if p.Escalations > 0 {
		fmt.Printf(" · %d escalation(s)", p.Escalations)
	}
	fmt.Println()

	if len(p.Slots) == 0 {
		return nil
	}
	fmt.Printf("\n  %-14s %-10s %-9s %-12s %-8s %s\n", "SLOT", "KIND", "STATUS", "DETECTION", "FOR", "PANE")
	for _, s := range p.Slots {
		age := "-"
		if !s.Since.IsZero() {
			age = time.Since(s.Since).Round(time.Second).String()
		}
		status := string(s.Status)
		if status == "" {
			status = "-"
		}
		if s.Halted {
			status += " (halted)"
		}
		// Detection tier is printed beside the status on purpose: a "done"
		// that herdr inferred from the screen is an inference, and a human
		// deciding whether to trust it needs to see which kind it is.
		tier := string(s.Tier)
		if tier == "" {
			tier = "-"
		}
		kind := s.Kind
		if kind == "" {
			kind = "-"
		}
		fmt.Printf("  %-14s %-10s %-9s %-12s %-8s %s\n", s.Slot, kind, status, tier, age, s.Pane)
	}
	fmt.Printf("\nevent log: %s\n", eventLogPath())
	return nil
}

// eventLogPath reports where the append-only history lives, so the human
// output can point at it rather than making the reader guess.
func eventLogPath() string {
	dir, err := stateDir()
	if err != nil {
		return eventsFile
	}
	return filepath.Join(dir, eventsFile)
}
