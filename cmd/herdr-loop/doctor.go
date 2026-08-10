package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	herdr "github.com/cyperx84/herdr-api"
	"github.com/cyperx84/herdr-loop/internal/engine"
	"github.com/cyperx84/herdr-loop/internal/manifest"
)

// cmdDoctor checks the three things PLAN.md treats as facts that must be
// read at runtime, never assumed: protocol compatibility (§4.13), which live
// agents are on heuristic screen-detection rather than structured self-
// reporting (§4.6), and whether a manifest's slot counts fit each kind's
// measured concurrency cap (§4.7).
func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "also check this manifest's slot counts against kind concurrency caps")
	kindsPath := fs.String("kinds", envOr("HERDR_LOOP_KINDS_FILE", "kinds.toml"), "kinds.toml path (optional — absent file is not an error)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The connection-requiring checks (1, 2) and the manifest-only check (3)
	// are independent: a manifest can be linted with no herdr connection at
	// all (that is the whole reason validate exists as a separate command),
	// so a failure to connect here must not skip the manifest check below —
	// only the two checks that genuinely need a socket.
	client, openErr := herdr.Open()
	if openErr != nil {
		fmt.Printf("protocol:    FAIL   %v\n", openErr)
		fmt.Println("detection:   SKIP   no connection")
	} else {
		checkProtocol(ctx, client)
		checkDetection(ctx, client)
	}

	// 3. Manifest concurrency check (§4.7), optional — only runs when a
	// manifest was named, and needs no connection at all.
	if *manifestPath == "" {
		return nil
	}
	kinds, err := loadKinds(*kindsPath)
	if err != nil {
		return fmt.Errorf("doctor: %w", err)
	}
	data, err := os.ReadFile(*manifestPath)
	if err != nil {
		return fmt.Errorf("doctor: %w", err)
	}
	m, err := manifest.Parse(data)
	if err != nil {
		return fmt.Errorf("doctor: %s: %w", *manifestPath, err)
	}
	fmt.Printf("concurrency: checking %s against %s\n", *manifestPath, kindsSource(*kindsPath))
	reportConcurrency(m, kinds)
	return nil
}

// checkProtocol is doctor's check 1 (§4.13): whether this binary and the
// live server speak the same protocol, the fact everything else here would
// be misleading without.
func checkProtocol(ctx context.Context, client *herdr.Client) {
	ping, err := client.Ping(ctx)
	switch {
	case err != nil:
		fmt.Printf("protocol:    FAIL   could not reach herdr: %v\n", err)
	case ping.Protocol != herdr.Protocol:
		fmt.Printf("protocol:    MISMATCH   server speaks %d (herdr %s), this binary targets %d\n",
			ping.Protocol, ping.Version, herdr.Protocol)
	default:
		fmt.Printf("protocol:    OK   %d (herdr %s)\n", ping.Protocol, ping.Version)
	}
}

// checkDetection is doctor's check 2 (§4.6): which live agents are on
// heuristic screen-detection rather than structured self-reporting — a
// runtime fact read per agent from screen_detection_skipped, never a
// per-kind constant, because the same kind differs by version and by
// whether its integration is installed.
func checkDetection(ctx context.Context, client *herdr.Client) {
	agents, err := client.AgentList(ctx)
	if err != nil {
		fmt.Printf("detection:   FAIL   agent.list: %v\n", err)
		return
	}
	if len(agents) == 0 {
		fmt.Println("detection:   (no live agents to check — every kind is unmeasured until one runs)")
		return
	}
	heuristic := 0
	for _, a := range agents {
		kind := ""
		if a.Agent != nil {
			kind = *a.Agent
		}
		tier := "structured"
		if !a.IntegrationBacked() {
			tier = "screen (heuristic)"
			heuristic++
		}
		fmt.Printf("  pane %-14s kind=%-10s detection=%s\n", a.PaneID, kind, tier)
	}
	fmt.Printf("detection:   %d/%d live agent(s) on heuristic (screen-classified) detection\n", heuristic, len(agents))
}

func kindsSource(path string) string {
	if _, err := os.Stat(path); err != nil {
		return "engine defaults (no " + path + ")"
	}
	return path
}

// reportConcurrency warns for every kind whose manifest slot count exceeds
// its measured (or default) cap, per PLAN §4.7: N concurrent OAuth-
// authenticated processes race a single rotating credential with no lock, so
// a manifest that would spawn more than the cap allows is exactly the
// failure mode this check exists to catch before it starts.
func reportConcurrency(m *manifest.Manifest, kinds map[string]engine.KindConfig) {
	counts := map[string]int{}
	for _, s := range m.Slots {
		counts[s.Kind]++
	}
	for kind, n := range counts {
		max := engine.DefaultMaxConcurrent
		source := "default"
		if kc, ok := kinds[kind]; ok && kc.MaxConcurrent > 0 {
			max = kc.MaxConcurrent
			source = "measured"
		}
		if n > max {
			// Not a warning and not a queue: run and validate both refuse
			// this manifest (checkKindCapacity). A loop needs every slot
			// alive at once, so surplus slots have no later turn to take.
			fmt.Printf("  %-10s FAIL   %d slot(s) exceed max_concurrent=%d (%s) — run will refuse this manifest; reduce the slots or raise max_concurrent in kinds.toml\n",
				kind, n, max, source)
		} else {
			fmt.Printf("  %-10s OK     %d slot(s) <= max_concurrent=%d (%s)\n", kind, n, max, source)
		}
	}
}
