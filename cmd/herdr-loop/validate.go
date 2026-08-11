package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/cyperx84/herdr-loop/internal/engine"
	"github.com/cyperx84/herdr-loop/internal/manifest"
)

// cmdValidate lints a manifest with no herdr connection at all — the whole
// point of a separate command, so a manifest can be checked in CI or before
// `herdr plugin install` ever runs. It runs both layers of validation a
// manifest can fail:
//
//  1. manifest.Parse's own rules (PLAN §3/§4.2/§4.3): slot uniqueness,
//     cwd-xor-worktree, shared-cwd, on_blocked shape, and the rule-cycle
//     termination invariant.
//  2. engine.New's construction-time checks (rule names, slot references,
//     blocked_rule shape) — reached via mapManifest + a pair of no-op
//     Herdr/Model stand-ins (fakes.go), because engine.New only stores its
//     client and model, it never calls a method on them during
//     construction, so a stub that just satisfies the interfaces is enough
//     to run the checks with nothing live.
func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	kindsPath := fs.String("kinds", envOr("HERDR_LOOP_KINDS_FILE", "kinds.toml"), "kinds.toml path (optional — absent file is not an error)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: herdr-loop validate <manifest.toml>")
	}
	path := fs.Arg(0)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	// A graph manifest routed through the loop parser parses to nothing and
	// reports "OK — 0 slot(s), 0 rule(s)", which is a lie by omission: the
	// file is not a valid loop, it is a different kind of document. Detect it
	// and validate it as what it is.
	if isGraphManifest(data) {
		return validateGraph(path, data)
	}

	m, err := manifest.Parse(data)
	if err != nil {
		return fmt.Errorf("validate: %s: %w", path, err)
	}

	res, err := mapManifest(m)
	if err != nil {
		return fmt.Errorf("validate: %s: %w", path, err)
	}

	if _, err := engine.New(res.Config, noopHerdr{}, noopModel{}, discardLogger()); err != nil {
		return fmt.Errorf("validate: %s: %w", path, err)
	}

	// Same kinds.toml the run would load, so validate refuses exactly what run
	// refuses rather than passing a manifest that cannot start.
	kinds, err := loadKinds(*kindsPath)
	if err != nil {
		return fmt.Errorf("validate: %s: %w", path, err)
	}
	if err := checkKindCapacity(res.Config.Slots, kinds); err != nil {
		return fmt.Errorf("validate: %s: %w", path, err)
	}

	fmt.Printf("%s: OK — %d slot(s), %d rule(s)", path, len(res.Config.Slots), len(res.Config.Rules))
	if n := len(res.Worktrees); n > 0 {
		fmt.Printf(", %d worktree slot(s)", n)
	}
	fmt.Println()
	return nil
}

// isGraphManifest reports whether a file is a graph rather than a loop.
//
// Keyed on the [graph] table, which a loop manifest never has and a graph
// manifest always does.
func isGraphManifest(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if t == "[graph]" {
			return true
		}
		if t == "[loop]" {
			return false
		}
	}
	return false
}

// validateGraph checks a graph manifest and reports its shape.
func validateGraph(path string, data []byte) error {
	gm, err := manifest.ParseGraph(data)
	if err != nil {
		return fmt.Errorf("validate: %s: %w", path, err)
	}
	g := gm.Graph()
	if err := g.Validate(); err != nil {
		return fmt.Errorf("validate: %s: %w", path, err)
	}

	slots, loops := 0, 0
	for _, n := range g.Nodes {
		if n.Loop != "" {
			loops++
		} else {
			slots++
		}
	}
	fmt.Printf("%s: OK — graph, %d node(s) (%d loop, %d slot), %d edge(s), entry %q\n",
		path, len(g.Nodes), loops, slots, len(g.Edges), g.Entry)
	fmt.Printf("  run it: herdr-loop graph %s\n", path)
	return nil
}
