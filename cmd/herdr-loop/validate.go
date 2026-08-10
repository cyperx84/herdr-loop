package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

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
