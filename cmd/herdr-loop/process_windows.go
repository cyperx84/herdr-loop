//go:build windows

package main

import "os"

// terminationSignals is what run's context listens for on Windows, which has
// no SIGTERM equivalent reachable from the os package — only os.Interrupt is
// portable here (Go maps it to a console control event). processTerminate
// below therefore hard-kills instead of signaling; see its doc comment.
func terminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// processAlive reports whether pid names a live process.
//
// Unlike POSIX, os.FindProcess on Windows actually opens a process handle
// (OpenProcess) rather than always succeeding regardless of whether the pid
// exists, so failure here already means "no such process" — there is no
// Windows equivalent of POSIX's signal-0 existence probe to fall back on.
// Same pid-reuse caveat as the Unix implementation applies.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

// processTerminate ends pid.
//
// Windows has no graceful-shutdown signal reachable from the os package —
// os.Process.Signal only accepts os.Kill here — so this is necessarily a
// hard kill. run's deferred cleanup (halting slots, clearing run state)
// therefore does not execute on this platform when stopped externally; it
// only runs on a clean Run() return. Flagged rather than silently assumed
// away — a real fix needs a Windows-specific IPC or event-based shutdown
// signal, out of scope for this CLI skeleton.
func processTerminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
