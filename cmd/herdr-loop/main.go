// Command herdr-loop is the supervisor binary herdr-plugin.toml wraps: the
// same executable serves as both a `herdr plugin action` target and the
// long-lived process a [[panes]] board entry keeps running for the
// lifetime of one loop (PLAN.md §2).
//
// # Why stdlib flag, not cobra
//
// PLAN.md §6's stack table names spf13/cobra as the anticipated CLI choice,
// but the task that wrote this binary explicitly leaves the call to
// whoever implements it. Five flat subcommands (run/status/stop/validate/
// doctor), no nested groups, and a handful of flags apiece do not earn a
// dependency whose main payoff — nested command trees, shell-completion
// generation, a rich help renderer — none of this surface needs. flag plus
// a manual switch is the whole implementation below.
//
// Revisit at M5: PLAN.md's binary shape adds `attach` (a bubbletea TUI) and
// a real flag surface (watch intervals, board filters) that cobra's flag
// composition and command grouping would start to pay for. Until then this
// stays stdlib.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
)

const usage = `usage: herdr-loop <command> [args]

commands:
  run [flags] <manifest.toml>   start the supervisor for a loop (long-lived)
  status [--json]               report the running loop, if any
  stop                          stop the running loop
  validate <manifest.toml>      parse and validate a manifest, no connection needed
  doctor [flags]                check protocol compatibility and detection tiers

run "herdr-loop <command> -h" for a command's own flags.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "herdr-loop:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return errors.New("no command given")
	}

	// terminationSignals is platform-split (process_unix.go /
	// process_windows.go): Unix listens for SIGTERM too, since stop's
	// processTerminate sends it and run needs to catch it to unwind cleanly
	// (halt slots, clear run state) rather than die uncaught.
	ctx, stop := signal.NotifyContext(context.Background(), terminationSignals()...)
	defer stop()

	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "stop":
		return cmdStop(args[1:])
	case "validate":
		return cmdValidate(args[1:])
	case "doctor":
		return cmdDoctor(ctx, args[1:])
	case "probe":
		return cmdProbe(args[1:])
	case "graph":
		return cmdGraph(ctx, args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() { fmt.Fprint(os.Stderr, usage) }

// envOr and envOrInt read a herdr-injected HERDR_LOOP_<KEY> config env var
// (see herdr-plugin.toml's [config] comment for the naming convention),
// falling back to def when unset or, for envOrInt, unparsable — an
// unparsable override should not crash the loop, it should fall back the
// same as an absent one.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
