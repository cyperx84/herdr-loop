package main

import (
	"flag"
	"fmt"
	"os"
)

// cmdStop signals the running loop's supervisor process to shut down.
//
// It does not itself tear down slots or worktrees — that is run's own
// shutdown path (context cancellation flowing into engine.Run, which waits
// for in-flight actions before returning) on platforms where processTerminate
// can ask nicely. See process_unix.go / process_windows.go for why that
// differs by platform.
func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	rs, err := readRunState()
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no loop is running")
			return nil
		}
		return fmt.Errorf("stop: %w", err)
	}

	if !processAlive(rs.PID) {
		fmt.Println("recorded loop is already gone; clearing stale state")
		return clearRunState()
	}

	if err := processTerminate(rs.PID); err != nil {
		return fmt.Errorf("stop: terminate pid %d: %w", rs.PID, err)
	}
	fmt.Printf("sent stop signal to loop %q (pid %d)\n", rs.LoopName, rs.PID)
	// run's own shutdown clears the state file; stop deliberately does not
	// also delete it here; a run process that fails to unwind cleanly
	// (Windows' hard-kill path, or a crash) leaves it for the next `status`
	// to find and repair via the stale-pid check instead.
	return nil
}
