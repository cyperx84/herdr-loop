package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	herdr "github.com/cyperx84/herdr-api"
)

// `herdr-loop probe <kind>` measures one harness and prints the kinds.toml
// stanza describing it.
//
// The capability table has to come from measurement, not belief. Every entry
// in it — whether a harness self-reports its status, how long it needs before
// it can take input, whether it blocks on first run, whether it needs a key
// sequence to stop reinterpreting the task — was learned by starting the thing
// and watching what happened. Hand-measuring does not scale past a handful of
// harnesses and goes stale the moment one updates.
//
// So this runs the same lifecycle the engine runs, records what the harness
// actually did at each step, and emits the config. A harness nobody has
// characterised becomes a supported one by running this against it.
//
// It costs one real agent invocation. Pass --args to pin a cheap model.
type probeResult struct {
	Kind string

	PaneReady     time.Duration // split -> shell at a prompt
	StartOK       bool
	StartErr      string
	StartRetries  int
	StartDuration time.Duration

	InteractiveAt time.Duration // start -> herdr reports it addressable
	Structured    bool          // self-reports status, vs screen-classified

	FirstPromptOK     bool
	FirstPromptErr    string
	SettleNeeded      time.Duration // extra wait before a prompt landed
	PromptToWorking   time.Duration
	TurnCompleted     bool
	TurnDuration      time.Duration
	BlockedOnStart    bool
	BlockedPromptText string
}

func cmdProbe(args []string) error {
	// The kind comes first, then flags. Go's flag package stops at the first
	// non-flag argument, so it has to be peeled off before parsing rather
	// than read back out of the remainder.
	// --all probes every installed kind; otherwise the kind comes first and
	// flags follow, since Go's flag package stops at the first non-flag.
	all := false
	kind := ""
	switch {
	case len(args) > 0 && args[0] == "--all":
		all = true
		args = args[1:]
	case len(args) > 0 && !strings.HasPrefix(args[0], "-"):
		kind = args[0]
		args = args[1:]
	default:
		return errors.New("usage: herdr-loop probe <kind>|--all [--cwd DIR] [--args \"--model sonnet\"]")
	}

	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	cwd := fs.String("cwd", ".", "directory to start the agent in")
	agentArgs := fs.String("args", "", "space-separated args passed to the harness (e.g. \"--model sonnet\")")
	timeout := fs.Duration("timeout", 3*time.Minute, "overall budget for the probe")
	keep := fs.Bool("keep", false, "leave the pane open for inspection")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := herdr.Open()
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := client.CheckProtocol(ctx); err != nil {
		return fmt.Errorf("probe: %w", err)
	}

	abs, err := os.Getwd()
	if err == nil && *cwd == "." {
		*cwd = abs
	}

	if all {
		return probeAll(ctx, client, *cwd, 3*time.Minute)
	}

	res := probeResult{Kind: kind}
	pane, err := runProbe(ctx, client, kind, *cwd, strings.Fields(*agentArgs), &res)
	if pane != "" && !*keep {
		cleanupCtx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer c2()
		_ = client.PaneClose(cleanupCtx, pane)
	}
	if err != nil && !res.StartOK {
		printProbe(res)
		return fmt.Errorf("probe %s: %w", kind, err)
	}
	printProbe(res)
	return nil
}

// runProbe walks a harness through the engine's own lifecycle, timing each
// step. It returns the pane so the caller can clean up even on failure.
func runProbe(ctx context.Context, c *herdr.Client, kind, cwd string, args []string, res *probeResult) (string, error) {
	label := "probe-" + kind
	created, err := c.TabCreate(ctx, herdr.TabCreateParams{Label: &label, CWD: &cwd})
	if err != nil {
		return "", fmt.Errorf("tab.create: %w", err)
	}
	pane := created.RootPane.ID
	fmt.Printf("probing %s in pane %s\n", kind, pane)

	// 1. How long until the pane is an available shell? agent.start fails
	//    agent_pane_busy before this.
	t0 := time.Now()
	for {
		info, err := c.PaneProcessInfo(ctx, pane)
		if err == nil && info.ShellHoldsForeground() {
			res.PaneReady = time.Since(t0)
			break
		}
		if time.Since(t0) > 20*time.Second {
			res.PaneReady = time.Since(t0)
			break
		}
		if !sleepCtx(ctx, 100*time.Millisecond) {
			return pane, ctx.Err()
		}
	}

	// 2. agent.start, retrying the readiness race the same way the engine does.
	t1 := time.Now()
	for attempt := 1; ; attempt++ {
		_, err = c.AgentStart(ctx, herdr.AgentStartParams{
			PaneID: pane, Name: label, Kind: kind, Args: args,
		})
		if err == nil {
			res.StartOK = true
			res.StartRetries = attempt - 1
			break
		}
		var apiErr *herdr.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "agent_pane_busy" || time.Since(t1) > 20*time.Second {
			res.StartErr = err.Error()
			res.StartDuration = time.Since(t1)
			return pane, fmt.Errorf("agent.start: %w", err)
		}
		if !sleepCtx(ctx, 250*time.Millisecond) {
			return pane, ctx.Err()
		}
	}
	res.StartDuration = time.Since(t1)

	// 3. How long until herdr reports it addressable, and does it self-report
	//    its status or get screen-classified?
	t2 := time.Now()
	for {
		a, err := c.AgentGet(ctx, pane)
		if err == nil {
			res.Structured = a.IntegrationBacked()
			if a.Status == herdr.StatusBlocked {
				res.BlockedOnStart = true
			}
			if a.InteractiveReady && !a.LaunchPending {
				res.InteractiveAt = time.Since(t2)
				break
			}
		}
		if time.Since(t2) > 60*time.Second {
			res.InteractiveAt = time.Since(t2)
			break
		}
		if !sleepCtx(ctx, 250*time.Millisecond) {
			return pane, ctx.Err()
		}
	}

	if res.BlockedOnStart {
		// A harness that blocks before doing any work needs a human once —
		// probing further would mean answering a prompt on their behalf,
		// which is exactly what the loop refuses to do.
		lines := uint32(30)
		snap, _ := c.PaneRead(ctx, herdr.PaneReadParams{
			PaneID: pane, Source: herdr.ReadSourceVisible, Lines: &lines,
		})
		res.BlockedPromptText = firstPromptLine(snap.Text)
		return pane, nil
	}

	// 4. Does a prompt land, and how much settle does it need first?
	//    Escalating waits rather than one guess: the answer IS the config.
	for _, settle := range []time.Duration{0, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if settle > 0 && !sleepCtx(ctx, settle) {
			return pane, ctx.Err()
		}
		t3 := time.Now()
		_, err := c.AgentPrompt(ctx, pane, "Reply with exactly: PROBE_OK", &herdr.AgentPromptWaitOptions{
			Until:     []herdr.AgentStatus{herdr.StatusWorking},
			TimeoutMs: u64(15000),
		})
		if err == nil {
			res.FirstPromptOK = true
			res.PromptToWorking = time.Since(t3)
			break
		}
		res.FirstPromptErr = err.Error()
		res.SettleNeeded += settle
		if !herdr.IsAgentPromptStalled(err) {
			break // a real error, not a readiness race
		}
	}
	if !res.FirstPromptOK {
		return pane, nil
	}

	// 5. Does it finish a turn? That transition out of working is the signal
	//    every rule in the engine keys on.
	t4 := time.Now()
	for {
		a, err := c.AgentGet(ctx, pane)
		if err == nil && a.Status != herdr.StatusWorking && a.Status != herdr.StatusUnknown {
			res.TurnCompleted = true
			res.TurnDuration = time.Since(t4)
			res.Structured = a.IntegrationBacked()
			break
		}
		if time.Since(t4) > 90*time.Second {
			res.TurnDuration = time.Since(t4)
			break
		}
		if !sleepCtx(ctx, 500*time.Millisecond) {
			return pane, ctx.Err()
		}
	}
	return pane, nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func u64(v uint64) *uint64 { return &v }

// firstPromptLine picks the most question-like line out of a blocked pane, so
// the report names what the harness is waiting on.
func firstPromptLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasSuffix(t, "?") && len(t) > 20 {
			return t
		}
	}
	return "(could not read the prompt text)"
}

// printProbe reports what the harness did, then the config that follows from
// it. The report is the evidence; the stanza is the conclusion.
func printProbe(r probeResult) {
	fmt.Printf("\n── %s ─────────────────────────────────────────\n", r.Kind)

	fmt.Printf("  pane ready          %v\n", round(r.PaneReady))
	if !r.StartOK {
		fmt.Printf("  agent.start         FAILED after %v\n", round(r.StartDuration))
		fmt.Printf("                      %s\n", r.StartErr)
		fmt.Printf("\n  Not usable in a loop until that start succeeds.\n")
		return
	}
	fmt.Printf("  agent.start         ok in %v", round(r.StartDuration))
	if r.StartRetries > 0 {
		fmt.Printf(" (%d retries on pane readiness)", r.StartRetries)
	}
	fmt.Println()
	fmt.Printf("  interactive after   %v\n", round(r.InteractiveAt))

	tier := "screen-classified (heuristic)"
	if r.Structured {
		tier = "self-reported (structured)"
	}
	fmt.Printf("  detection           %s\n", tier)

	if r.BlockedOnStart {
		fmt.Printf("  first run           BLOCKED before doing any work\n")
		fmt.Printf("                      %s\n", r.BlockedPromptText)
		fmt.Printf("\n  A loop will escalate and halt this slot rather than answering.\n")
		fmt.Printf("  Answer it yourself once, in this directory, then re-probe.\n")
		return
	}

	if !r.FirstPromptOK {
		fmt.Printf("  prompt delivery     FAILED\n")
		fmt.Printf("                      %s\n", r.FirstPromptErr)
		fmt.Printf("\n  Not usable in a loop: the engine drives agents by prompting them.\n")
		return
	}
	fmt.Printf("  prompt delivery     ok")
	if r.SettleNeeded > 0 {
		fmt.Printf(" (needed %v of settle first)", round(r.SettleNeeded))
	}
	fmt.Printf(", working after %v\n", round(r.PromptToWorking))

	if r.TurnCompleted {
		fmt.Printf("  turn completed      yes, in %v\n", round(r.TurnDuration))
	} else {
		fmt.Printf("  turn completed      NO — still working after %v\n", round(r.TurnDuration))
		fmt.Printf("                      A loop stays healthy but waits indefinitely on this.\n")
	}

	fmt.Printf("\n  kinds.toml stanza:\n\n")
	fmt.Printf("    [%s]\n", r.Kind)
	if r.SettleNeeded > 0 {
		fmt.Printf("    # Reports itself interactive before it can take input; a prompt\n")
		fmt.Printf("    # sent sooner than this is delivered to nothing.\n")
		fmt.Printf("    startup_settle_ms = %d\n", r.SettleNeeded.Milliseconds())
	}
	fmt.Printf("    max_concurrent    = 1  # raise only if this harness races no shared credential\n")
	if !r.Structured {
		fmt.Printf("    # Screen-classified: a strict loop will refuse this kind, because its\n")
		fmt.Printf("    # status is an inference rather than something the agent reported.\n")
	}
	if !r.TurnCompleted {
		fmt.Printf("    # WARNING: did not finish a turn during the probe. Verify before\n")
		fmt.Printf("    # depending on it — a loop cannot advance past a slot that never settles.\n")
	}
	fmt.Println()
}

func round(d time.Duration) time.Duration { return d.Round(10 * time.Millisecond) }

// probeAll walks every agent kind this herdr knows about.
//
// This is what makes the plugin usable by people who did not measure these
// harnesses themselves. A compatibility matrix built by running each harness
// beats one built from anybody's recollection, and it can be regenerated when
// a harness updates rather than slowly going stale.
//
// Kinds whose binary is not installed are reported as such and skipped rather
// than counted as failures: "not present here" and "does not work" are
// different answers and a matrix that conflates them is worse than none.
func probeAll(ctx context.Context, c *herdr.Client, cwd string, budget time.Duration) error {
	kinds, err := installedKinds(ctx, c)
	if err != nil {
		return err
	}
	if len(kinds) == 0 {
		return errors.New("probe: herdr reported no agent kinds")
	}

	fmt.Printf("probing %d installed kind(s): %s\n", len(kinds), strings.Join(kinds, " "))
	fmt.Printf("one real agent invocation each — this costs quota on any harness that bills\n\n")

	results := make([]probeResult, 0, len(kinds))
	for _, kind := range kinds {
		res := probeResult{Kind: kind}
		kctx, cancel := context.WithTimeout(ctx, budget)
		pane, err := runProbe(kctx, c, kind, cwd, nil, &res)
		cancel()
		if pane != "" {
			cctx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
			_ = c.PaneClose(cctx, pane)
			c2()
		}
		if err != nil && res.StartErr == "" {
			res.StartErr = err.Error()
		}
		results = append(results, res)
		printProbe(res)
	}

	printMatrix(results)
	return nil
}

// installedKinds asks herdr which kinds it recognises, then keeps the ones
// whose command actually resolves on PATH.
func installedKinds(ctx context.Context, c *herdr.Client) ([]string, error) {
	manifests, err := c.ServerAgentManifests(ctx)
	if err != nil {
		return nil, fmt.Errorf("server.agent_manifests: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range manifests.Manifests {
		if m.Agent == "" || seen[m.Agent] {
			continue
		}
		seen[m.Agent] = true
		if _, err := exec.LookPath(m.Agent); err != nil {
			fmt.Printf("  skip %-12s not installed on this machine\n", m.Agent)
			continue
		}
		out = append(out, m.Agent)
	}
	sort.Strings(out)
	return out, nil
}

// printMatrix is the community-facing summary: one row per harness, the four
// facts that decide whether it can be looped, and nothing else.
func printMatrix(rs []probeResult) {
	fmt.Printf("\n═══ harness compatibility ═══════════════════════════════════\n\n")
	fmt.Printf("  %-12s %-8s %-12s %-9s %-8s %s\n", "KIND", "STARTS", "DETECTION", "PROMPTS", "TURNS", "NOTE")
	for _, r := range rs {
		starts, detection, prompts, turns, note := "no", "-", "-", "-", ""
		if r.StartOK {
			starts = "yes"
			detection = "screen"
			if r.Structured {
				detection = "structured"
			}
			switch {
			case r.BlockedOnStart:
				prompts, note = "blocked", "needs a human to trust the directory once"
			case r.FirstPromptOK:
				prompts = "yes"
				if r.SettleNeeded > 0 {
					note = fmt.Sprintf("needs %v settle", round(r.SettleNeeded))
				}
				if r.TurnCompleted {
					turns = round(r.TurnDuration).String()
				} else {
					turns, note = "never", "did not finish a turn — unusable in a loop"
				}
			default:
				prompts, note = "no", "prompt never landed — unusable in a loop"
			}
		} else {
			note = "agent.start failed"
		}
		fmt.Printf("  %-12s %-8s %-12s %-9s %-8s %s\n", r.Kind, starts, detection, prompts, turns, note)
	}
	fmt.Printf("\n  structured detection means the agent reports its own state; screen means\n")
	fmt.Printf("  herdr infers it from the rendered terminal, which is a heuristic a strict\n")
	fmt.Printf("  loop refuses to act on.\n\n")
}
