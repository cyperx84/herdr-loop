//go:build !windows

package main

import (
	"os"
	"syscall"
)

// terminationSignals is what run's context listens for. SIGTERM is included
// here because processTerminate below sends it; Interrupt (SIGINT) covers a
// user hitting Ctrl-C in the pane directly. This lives behind a build tag
// because syscall.SIGTERM is not defined on Windows — see
// process_windows.go for that platform's answer.
func terminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

// processAlive reports whether pid names a live, signalable process.
//
// Signal 0 sends nothing — POSIX defines it as existence/permission check
// only — which is the standard way to answer "is this pid still there"
// without side effects. A pid table wraps, so a stale run.json naming a
// long-dead pid that has since been reassigned is a known, accepted
// imprecision (status treats it as a stale-file repair either way, see
// cmdStatus).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// processTerminate asks pid to shut down.
//
// SIGTERM, not SIGKILL: run's own shutdown path (context cancellation,
// e.Actions.Wait(), clearRunState) only runs if the process gets to catch the
// signal and unwind — see cmd/herdr-loop's run() installing
// signal.NotifyContext(os.Interrupt) for exactly this.
func processTerminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
