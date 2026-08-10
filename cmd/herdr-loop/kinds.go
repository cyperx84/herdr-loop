package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/BurntSushi/toml"
	"github.com/cyperx84/herdr-loop/internal/engine"
)

// kindsFile is the on-disk shape of kinds.toml (PLAN.md §5): one table per
// agent kind, holding capability facts measured against a real session —
// never guessed, and never compiled in as constants, because the same kind
// differs by version and by whether its integration is installed (§4.6).
//
//	[claude]
//	max_concurrent     = 2
//	mid_turn_injection = false
type kindsFile map[string]struct {
	MaxConcurrent    int  `toml:"max_concurrent"`
	MidTurnInjection bool `toml:"mid_turn_injection"`
}

// loadKinds reads an optional kinds.toml. A missing file is not an error —
// every kind simply falls back to engine.DefaultMaxConcurrent and
// mid-turn-injection off, the conservative defaults §4.4/§4.7 both call for
// on a kind nobody has measured yet.
func loadKinds(path string) (map[string]engine.KindConfig, error) {
	if path == "" {
		return map[string]engine.KindConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]engine.KindConfig{}, nil
		}
		return nil, fmt.Errorf("read kinds file %s: %w", path, err)
	}
	var kf kindsFile
	if _, err := toml.Decode(string(data), &kf); err != nil {
		return nil, fmt.Errorf("parse kinds file %s: %w", path, err)
	}
	out := make(map[string]engine.KindConfig, len(kf))
	for kind, v := range kf {
		out[kind] = engine.KindConfig{
			MaxConcurrent:    v.MaxConcurrent,
			MidTurnInjection: v.MidTurnInjection,
		}
	}
	return out, nil
}

// checkKindCapacity rejects a manifest that cannot ever run.
//
// A loop needs every slot alive at the same time — slots hand work to each
// other, so there is no schedule in which they take turns. If a kind has more
// slots than its concurrency cap allows, the run cannot start.
//
// This must be caught here rather than at spawn time. Engine.Spawn holds a
// kind's token for the agent's lifetime, so the surplus slot blocks in
// acquireKind with no agent left to release one: the process hangs at startup
// until the context expires, and the last thing it logged was a successful
// spawn. A config error that presents as a silent hang is the worst possible
// diagnostic, so it fails loudly, here, with the two ways out spelled out.
func checkKindCapacity(slots []engine.SlotConfig, kinds map[string]engine.KindConfig) error {
	counts := map[string]int{}
	for _, s := range slots {
		counts[s.Kind]++
	}
	names := make([]string, 0, len(counts))
	for kind := range counts {
		names = append(names, kind)
	}
	sort.Strings(names)

	for _, kind := range names {
		n := counts[kind]
		max := engine.DefaultMaxConcurrent
		source := "the conservative default for an unmeasured kind"
		if kc, ok := kinds[kind]; ok && kc.MaxConcurrent > 0 {
			max = kc.MaxConcurrent
			source = "measured in kinds.toml"
		}
		if n > max {
			return fmt.Errorf(
				"%d %q slots but max_concurrent=%d (%s): a loop needs every slot running at once, so this manifest can never start.\n"+
					"  Either reduce the %q slots to %d, or — if you have established that %d concurrent %q agents are safe on this machine — raise it in kinds.toml:\n"+
					"      [%s]\n      max_concurrent = %d\n"+
					"  Raising it is not free: concurrent processes of an OAuth-authenticated kind race a single rotating credential with no lock.",
				n, kind, max, source, kind, max, n, kind, kind, n)
		}
	}
	return nil
}
