package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	herdr "github.com/cyperx84/herdr-api"
)

// --- fakes -----------------------------------------------------------------
//
// The engine is tested against fakes rather than a live server on purpose:
// spawning real agents on an OAuth-only machine is the credential race §4.7
// exists to avoid, and the invariants under test are about what the engine
// decides, not about the wire.

type fakeHerdr struct {
	mu sync.Mutex

	prompts  []promptCall
	keys     []keysCall
	splits   []herdr.PaneSplitParams
	starts   []herdr.AgentStartParams
	notified []herdr.NotificationShowParams

	promptErr error
	startErr  error
	// onSplit runs inside PaneSplit, so a test can observe concurrency at the
	// moment the kind gate admits a spawn.
	onSplit func()
}

type promptCall struct{ target, text string }
type keysCall struct {
	target string
	keys   []string
}

func (f *fakeHerdr) AgentPrompt(_ context.Context, target, text string, _ *herdr.AgentPromptWaitOptions) (herdr.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, promptCall{target, text})
	return herdr.Agent{PaneID: target}, f.promptErr
}

func (f *fakeHerdr) AgentSendKeys(_ context.Context, target string, keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, keysCall{target, keys})
	return nil
}

func (f *fakeHerdr) AgentStart(_ context.Context, p herdr.AgentStartParams) (herdr.AgentStartResult, error) {
	f.mu.Lock()
	f.starts = append(f.starts, p)
	f.mu.Unlock()
	return herdr.AgentStartResult{Agent: herdr.Agent{PaneID: p.PaneID}}, f.startErr
}

func (f *fakeHerdr) PaneSplit(_ context.Context, p herdr.PaneSplitParams) (herdr.Pane, error) {
	f.mu.Lock()
	f.splits = append(f.splits, p)
	n := len(f.splits)
	hook := f.onSplit
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return herdr.Pane{ID: "pane-" + string(rune('0'+n))}, nil
}

func (f *fakeHerdr) NotificationShow(_ context.Context, p herdr.NotificationShowParams) (herdr.NotificationShowResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, p)
	return herdr.NotificationShowResult{Shown: true, Reason: herdr.NotificationShown}, nil
}

func (f *fakeHerdr) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeHerdr) keyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.keys)
}

func (f *fakeHerdr) notifyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.notified)
}

// fakeModel stands in for the reconciled state model. It answers from a plain
// map, which is the point: the engine must read status from the model at fire
// time, so a test can make the model disagree with a transition and assert the
// model wins.
type fakeModel struct {
	mu      sync.Mutex
	status  map[string]herdr.AgentStatus
	targets map[string]string
	blocked map[string]string
}

func newModel() *fakeModel {
	return &fakeModel{
		status:  map[string]herdr.AgentStatus{},
		targets: map[string]string{},
		blocked: map[string]string{},
	}
}

func (m *fakeModel) set(slot string, s herdr.AgentStatus) *fakeModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[slot] = s
	if _, ok := m.targets[slot]; !ok {
		m.targets[slot] = slot + "-target"
	}
	return m
}

func (m *fakeModel) SlotStatus(slot string) (herdr.AgentStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.status[slot]
	return s, ok
}

func (m *fakeModel) SlotTarget(slot string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[slot]
	return t, ok
}

func (m *fakeModel) BlockedPrompt(slot string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.blocked[slot]
	return p, ok
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// baseConfig is a two-slot loop with one rule: when impl settles, prompt
// review. Tests vary one thing at a time from here.
func baseConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Name:          "test-loop",
		MaxIterations: 10,
		HandoffDir:    t.TempDir(),
		Slots: []SlotConfig{
			{Name: "impl", Kind: "claude"},
			{Name: "review", Kind: "codex"},
		},
		Rules: []Rule{{
			Name: "hand impl to review",
			When: Predicate{Op: OpAll, Filters: []Predicate{
				{Op: OpEq, Field: "slot", Value: "impl"},
				{Op: OpIn, Field: "status", Values: []string{"idle", "done"}},
			}},
			Then: Action{Prompt: &PromptAction{Slot: "review", Text: "Review {{impl.handoff}}."}},
		}},
	}
}

// run drives the engine over a fixed transition list and returns once the list
// is drained, so assertions never race a dispatched action.
func run(t *testing.T, e *Engine, trs ...Transition) Outcome {
	t.Helper()
	ch := make(chan Transition, len(trs))
	for _, tr := range trs {
		ch <- tr
	}
	close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := e.Run(ctx, ch)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out
}

func newEngine(t *testing.T, cfg Config, client Herdr, model Model) *Engine {
	t.Helper()
	e, err := New(cfg, client, model, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// --- contracts -------------------------------------------------------------

// A blocked slot is an approval or question UI herdr recognised. The default
// policy escalates and halts that slot; it must never send keys, because
// answering an approval prompt nobody read is an agent granting itself
// permissions (§4.3).
func TestBlockedEscalatesByDefaultAndNeverAnswers(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusBlocked).set("review", herdr.StatusIdle)
	model.blocked["impl"] = "Allow Bash(rm -rf /)?"

	cfg := baseConfig(t) // OnBlocked unset — must default to escalate
	e := newEngine(t, cfg, client, model)

	run(t, e, Transition{Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusBlocked, At: time.Now()})

	if n := client.keyCount(); n != 0 {
		t.Fatalf("sent keys to a blocked slot %d times, want 0 — escalate must never answer", n)
	}
	if n := client.notifyCount(); n != 1 {
		t.Fatalf("notifications = %d, want 1 — an escalation that reaches nobody is a silent stall", n)
	}
	if !e.Halted("impl") {
		t.Error("blocked slot was not halted; escalate must stop the loop touching it")
	}
	if n := client.promptCount(); n != 0 {
		t.Errorf("prompts = %d, want 0 — a blocked slot must not reach the rules", n)
	}
}

// The escalation carries the facts that make a stall diagnosable rather than a
// bare timeout (§4.11).
func TestEscalationCarriesStructuredReason(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusBlocked)
	model.blocked["impl"] = "Continue?"

	e := newEngine(t, baseConfig(t), client, model)
	out := run(t, e, Transition{
		Slot: "impl", To: herdr.StatusBlocked, At: time.Now().Add(-90 * time.Second),
	})

	if len(out.Escalations) != 1 {
		t.Fatalf("Escalations = %d, want 1", len(out.Escalations))
	}
	esc := out.Escalations[0]
	if esc.Slot != "impl" {
		t.Errorf("Slot = %q, want impl", esc.Slot)
	}
	if esc.Status != herdr.StatusBlocked {
		t.Errorf("Status = %q, want blocked", esc.Status)
	}
	if esc.Prompt != "Continue?" {
		t.Errorf("Prompt = %q, want the observed prompt text", esc.Prompt)
	}
	if esc.Stalled < 90*time.Second {
		t.Errorf("Stalled = %v, want at least the 90s the slot sat blocked", esc.Stalled)
	}
	if esc.Reason == "" {
		t.Error("Reason is empty; an escalation without one is the bare timeout §4.11 forbids")
	}
}

// auto answers only prompts a human wrote out in full. Anything else escalates
// — including a near-miss, which is what a wildcard would have swallowed.
func TestAutoAnswersOnlyExactMatches(t *testing.T) {
	const allowed = "Do you want to proceed?"

	cases := []struct {
		name      string
		prompt    string
		visible   bool
		wantKeys  bool
		wantHalt  bool
		wantNotif bool
	}{
		{name: "exact match answers", prompt: allowed, visible: true, wantKeys: true},
		{name: "trailing whitespace still matches", prompt: allowed + "\n", visible: true, wantKeys: true},
		{name: "prefix of an allowed prompt escalates", prompt: "Do you want to proceed", visible: true, wantHalt: true, wantNotif: true},
		{name: "superstring of an allowed prompt escalates", prompt: allowed + " (y/n)", visible: true, wantHalt: true, wantNotif: true},
		{name: "different prompt escalates", prompt: "Allow write to /etc/hosts?", visible: true, wantHalt: true, wantNotif: true},
		{name: "invisible prompt escalates", prompt: "", visible: false, wantHalt: true, wantNotif: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeHerdr{}
			model := newModel().set("impl", herdr.StatusBlocked)
			if tc.visible {
				model.blocked["impl"] = tc.prompt
			}

			cfg := baseConfig(t)
			cfg.OnBlocked = BlockedAuto
			cfg.BlockedRules = []BlockedRule{{Prompt: allowed, Keys: []string{"y", "Enter"}}}
			e := newEngine(t, cfg, client, model)

			run(t, e, Transition{Slot: "impl", To: herdr.StatusBlocked, At: time.Now()})

			if got := client.keyCount() > 0; got != tc.wantKeys {
				t.Errorf("answered = %v, want %v (prompt %q)", got, tc.wantKeys, tc.prompt)
			}
			if got := e.Halted("impl"); got != tc.wantHalt {
				t.Errorf("halted = %v, want %v", got, tc.wantHalt)
			}
			if got := client.notifyCount() > 0; got != tc.wantNotif {
				t.Errorf("notified = %v, want %v", got, tc.wantNotif)
			}
		})
	}
}

// The auto whitelist is matched literally. Regexp and glob metacharacters carry
// no meaning, so a rule containing them answers only a prompt containing them.
func TestAutoWhitelistHasNoPatternLanguage(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusBlocked)
	model.blocked["impl"] = "Allow Bash(rm -rf /tmp/x)?"

	cfg := baseConfig(t)
	cfg.OnBlocked = BlockedAuto
	cfg.BlockedRules = []BlockedRule{{Prompt: "Allow Bash(.*)?", Keys: []string{"y"}}}
	e := newEngine(t, cfg, client, model)

	run(t, e, Transition{Slot: "impl", To: herdr.StatusBlocked, At: time.Now()})

	if n := client.keyCount(); n != 0 {
		t.Fatalf("a regexp-looking blocked_rule matched a different prompt %d times; the whitelist must be literal", n)
	}
	if !e.Halted("impl") {
		t.Error("unmatched prompt did not halt the slot")
	}
}

// pause halts the whole loop, not just the slot.
func TestBlockedPauseEndsTheLoop(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusBlocked).set("review", herdr.StatusIdle)
	model.blocked["impl"] = "Continue?"

	cfg := baseConfig(t)
	cfg.OnBlocked = BlockedPause
	e := newEngine(t, cfg, client, model)

	// The second transition would satisfy the impl->review rule. It must never
	// be reached: pause ends the loop at the first block.
	out := run(t, e,
		Transition{Slot: "impl", To: herdr.StatusBlocked, At: time.Now()},
		Transition{Slot: "impl", To: herdr.StatusDone, At: time.Now()},
	)

	if out.Reason != ReasonPaused {
		t.Fatalf("Reason = %q, want %q", out.Reason, ReasonPaused)
	}
	if n := client.promptCount(); n != 0 {
		t.Errorf("prompts = %d, want 0 — a paused loop keeps folding", n)
	}
}

// §4.4: a rule never acts on a slot mid-turn. The transition claims done; the
// model says the target is working, and the model is authoritative (§4.9).
func TestRuleNeverFiresAgainstAWorkingSlot(t *testing.T) {
	t.Run("working target is not prompted", func(t *testing.T) {
		client := &fakeHerdr{}
		model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusWorking)
		e := newEngine(t, baseConfig(t), client, model)

		out := run(t, e, Transition{Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusDone})

		if n := client.promptCount(); n != 0 {
			t.Fatalf("prompts = %d, want 0 — mid-turn injection is off until measured", n)
		}
		if out.Iterations != 0 {
			t.Errorf("Iterations = %d, want 0 — a rule that cannot act must not spend budget", out.Iterations)
		}
	})

	t.Run("stale done transition loses to a working model", func(t *testing.T) {
		client := &fakeHerdr{}
		// The trigger slot itself has moved on since the transition was queued.
		model := newModel().set("impl", herdr.StatusWorking).set("review", herdr.StatusIdle)
		e := newEngine(t, baseConfig(t), client, model)

		run(t, e, Transition{Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusDone})

		if n := client.promptCount(); n != 0 {
			t.Fatalf("prompts = %d, want 0 — the model is authoritative over a drained transition", n)
		}
	})

	t.Run("unknown is not settled", func(t *testing.T) {
		client := &fakeHerdr{}
		model := newModel().set("impl", herdr.StatusUnknown).set("review", herdr.StatusIdle)
		e := newEngine(t, baseConfig(t), client, model)

		run(t, e, Transition{Slot: "impl", To: herdr.StatusUnknown})

		if n := client.promptCount(); n != 0 {
			t.Fatalf("prompts = %d, want 0 — unknown is not proof of completion (§4.5)", n)
		}
	})
}

// Mid-turn injection is a per-kind capability read from config, never a
// constant: turning it on for a kind changes the gate, and nothing else does.
func TestMidTurnInjectionIsAPerKindCapability(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusWorking)

	cfg := baseConfig(t)
	cfg.Kinds = map[string]KindConfig{"codex": {MidTurnInjection: true}}
	e := newEngine(t, cfg, client, model)

	run(t, e, Transition{Slot: "impl", To: herdr.StatusDone})

	if n := client.promptCount(); n != 1 {
		t.Fatalf("prompts = %d, want 1 once the kind's mid-turn capability is enabled", n)
	}
}

// The budget is enforced at runtime, not only by the manifest validator: once
// firings reach the cap the loop terminates rather than running on.
//
// Five rules match one transition and each targets a different slot, so nothing
// here depends on how fast an action finishes.
func TestIterationBudgetTerminatesTheLoop(t *testing.T) {
	cfg := baseConfig(t)
	cfg.MaxIterations = 3
	cfg.Rules = nil
	model := newModel().set("impl", herdr.StatusDone)
	for _, name := range []string{"r1", "r2", "r3", "r4", "r5"} {
		cfg.Slots = append(cfg.Slots, SlotConfig{Name: name, Kind: "codex"})
		cfg.Rules = append(cfg.Rules, Rule{
			Name: "fan out to " + name,
			When: Predicate{Op: OpEq, Field: "slot", Value: "impl"},
			Then: Action{Prompt: &PromptAction{Slot: name, Text: "go"}},
		})
		model.set(name, herdr.StatusIdle)
	}

	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, model)
	out := run(t, e, Transition{Slot: "impl", To: herdr.StatusDone})

	if out.Reason != ReasonBudgetExhausted {
		t.Fatalf("Reason = %q, want %q", out.Reason, ReasonBudgetExhausted)
	}
	if n := client.promptCount(); n != cfg.MaxIterations {
		t.Errorf("prompts = %d, want %d — the budget bounds actions, not just the exit", n, cfg.MaxIterations)
	}
	if out.Iterations != cfg.MaxIterations+1 {
		t.Errorf("Iterations = %d, want %d — the firing that exceeded the cap is counted",
			out.Iterations, cfg.MaxIterations+1)
	}
}

// The budget is an upper bound on actuation however transitions arrive. This
// asserts the bound rather than an exact count, because per-slot contention
// legitimately reduces how many of a burst's firings actuate.
func TestIterationBudgetBoundsActionsUnderABurst(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)

	cfg := baseConfig(t)
	cfg.MaxIterations = 3
	e := newEngine(t, cfg, client, model)

	trs := make([]Transition, 50)
	for i := range trs {
		trs[i] = Transition{Slot: "impl", To: herdr.StatusDone}
	}
	out := run(t, e, trs...)

	if n := client.promptCount(); n > cfg.MaxIterations {
		t.Errorf("prompts = %d, want at most %d", n, cfg.MaxIterations)
	}
	if out.Iterations > cfg.MaxIterations+1 {
		t.Errorf("Iterations = %d, want the loop to stop one past the cap at the latest", out.Iterations)
	}
}

// One action per slot at a time. The second rule targeting a busy slot is
// skipped rather than queued, because the in-flight action produces its own
// transition and re-triggers evaluation (§4.12).
func TestPerSlotMutexAllowsOneActionInFlight(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	client := &fakeHerdr{onSplit: func() {
		entered <- struct{}{}
		<-release
	}}
	model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)

	cfg := baseConfig(t)
	cfg.Rules = []Rule{{
		Name: "spawn review",
		When: Predicate{Op: OpEq, Field: "slot", Value: "impl"},
		Then: Action{Spawn: &SpawnAction{Slot: "review"}},
	}}
	e := newEngine(t, cfg, client, model)

	ch := make(chan Transition)
	done := make(chan Outcome, 1)
	go func() {
		out, err := e.Run(context.Background(), ch)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- out
	}()

	ch <- Transition{Slot: "impl", To: herdr.StatusDone}
	<-entered // first spawn is in flight and parked inside PaneSplit
	ch <- Transition{Slot: "impl", To: herdr.StatusDone}
	close(ch)

	// Give the second transition a chance to be mishandled before releasing.
	select {
	case <-entered:
		t.Fatal("two actions in flight for one slot; the per-slot mutex did not hold")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-done

	client.mu.Lock()
	splits := len(client.splits)
	client.mu.Unlock()
	if splits != 1 {
		t.Errorf("pane.split calls = %d, want 1 — the contended action is skipped, not queued", splits)
	}
}

// §4.7: spawns queue against a per-kind limit. The limit counts live agents, so
// a second claude spawn waits until a token is released — it is not rejected.
func TestPerKindMaxConcurrentGatesSpawn(t *testing.T) {
	inSplit := make(chan struct{}, 4)
	hold := make(chan struct{})
	client := &fakeHerdr{onSplit: func() {
		inSplit <- struct{}{}
		<-hold
	}}
	model := newModel().set("a", herdr.StatusIdle).set("b", herdr.StatusIdle)

	cfg := baseConfig(t)
	cfg.Slots = []SlotConfig{{Name: "a", Kind: "claude"}, {Name: "b", Kind: "claude"}}
	cfg.Rules = nil
	cfg.Kinds = map[string]KindConfig{"claude": {MaxConcurrent: 1}}
	e := newEngine(t, cfg, client, model)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 2)
	go func() { errs <- e.Spawn(ctx, "a") }()
	<-inSplit
	go func() { errs <- e.Spawn(ctx, "b") }()

	select {
	case <-inSplit:
		t.Fatal("two claude spawns ran concurrently under max_concurrent = 1")
	case <-time.After(100 * time.Millisecond):
	}

	close(hold)
	if err := <-errs; err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	// The first slot still holds its token; releasing it admits the second.
	e.Release("a")
	select {
	case <-inSplit:
	case <-time.After(2 * time.Second):
		t.Fatal("second spawn never admitted after the first token was released — the gate queues, it does not reject")
	}
	if err := <-errs; err != nil {
		t.Fatalf("second spawn: %v", err)
	}
}

// An unmeasured kind gets the conservative default rather than unlimited
// concurrency — the unmeasured case is exactly where nobody has shown fanning
// out is safe (§4.7).
func TestUnknownKindGetsConservativeConcurrency(t *testing.T) {
	e := newEngine(t, baseConfig(t), &fakeHerdr{}, newModel())
	if got := e.kindConfig("some-kind-nobody-measured").MaxConcurrent; got != DefaultMaxConcurrent {
		t.Errorf("MaxConcurrent = %d, want %d", got, DefaultMaxConcurrent)
	}
	if e.kindConfig("some-kind-nobody-measured").MidTurnInjection {
		t.Error("an unmeasured kind must not permit mid-turn injection")
	}
}

// Every slot is spawned with HERDR_LOOP_HANDOFF pointing at its own file, and
// that variable can only go in at pane.split time — agent.start has no env
// field in protocol 19.
func TestSpawnInjectsHandoffEnvAtPaneSplit(t *testing.T) {
	client := &fakeHerdr{}
	cfg := baseConfig(t)
	e := newEngine(t, cfg, client, newModel())

	if err := e.Spawn(context.Background(), "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.splits) != 1 || len(client.starts) != 1 {
		t.Fatalf("split/start calls = %d/%d, want 1/1", len(client.splits), len(client.starts))
	}
	got := client.splits[0].Env[HandoffEnv]
	if want := e.HandoffPath("impl"); got != want {
		t.Errorf("%s = %q, want %q", HandoffEnv, got, want)
	}
	if !strings.HasPrefix(got, cfg.HandoffDir) {
		t.Errorf("handoff path %q is outside handoff_dir %q", got, cfg.HandoffDir)
	}
	// agent.start carries no env of its own, so it must land on the pane the
	// split just created or the handoff variable never reaches the agent.
	if client.starts[0].PaneID == "" {
		t.Error("agent.start was not addressed at the pane created by the split")
	}
	if client.starts[0].Kind != "claude" {
		t.Errorf("Kind = %q, want the slot's configured kind", client.starts[0].Kind)
	}
}

// Results come from the handoff FILE. A rule referencing a typed front-matter
// field must fire on what the file says, and must not fire when the file says
// something else — no pane output is involved either way (§4.1).
func TestRulesReadTypedFieldsFromTheHandoffFile(t *testing.T) {
	cases := []struct {
		name     string
		contents string
		want     bool
	}{
		{
			name:     "yaml front-matter",
			contents: "---\nverdict: clean\nfindings: 0\n---\n\nNothing to report.\n",
			want:     true,
		},
		{
			name:     "toml front-matter",
			contents: "+++\nverdict = \"clean\"\n+++\n\nNothing to report.\n",
			want:     true,
		},
		{
			name:     "different verdict does not fire",
			contents: "---\nverdict: changes-requested\n---\n",
			want:     false,
		},
		{
			name:     "no front-matter does not fire",
			contents: "the review went fine, verdict: clean\n",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t)
			cfg.Rules = []Rule{{
				Name: "converged",
				When: Predicate{Op: OpEq, Field: "review.handoff.verdict", Value: "clean"},
				Then: Action{Finish: &FinishAction{Reason: "converged"}},
			}}
			client := &fakeHerdr{}
			model := newModel().set("review", herdr.StatusDone).set("impl", herdr.StatusIdle)
			e := newEngine(t, cfg, client, model)

			writeFile(t, e.HandoffPath("review"), tc.contents)
			out := run(t, e, Transition{Slot: "review", To: herdr.StatusDone})

			fired := out.Reason == "converged"
			if fired != tc.want {
				t.Fatalf("finished = %v (reason %q), want %v", fired, out.Reason, tc.want)
			}
		})
	}
}

// A missing handoff file is the normal state before a slot's first result. It
// must make predicates not hold, not crash the fold.
func TestMissingHandoffMakesPredicatesNotHold(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Rules = []Rule{{
		Name: "converged",
		When: Predicate{Op: OpExists, Field: "review.handoff.verdict"},
		Then: Action{Finish: &FinishAction{Reason: "converged"}},
	}}
	e := newEngine(t, cfg, &fakeHerdr{}, newModel().set("review", herdr.StatusDone))

	out := run(t, e, Transition{Slot: "review", To: herdr.StatusDone})
	if out.Reason != ReasonStreamClosed {
		t.Fatalf("Reason = %q, want the loop to keep folding with no handoff file", out.Reason)
	}
}

// Prompt text is expanded from the same fields predicates resolve, and an
// unresolved placeholder escalates rather than reaching an agent verbatim.
func TestPromptTemplateExpansion(t *testing.T) {
	t.Run("resolves handoff paths and vars", func(t *testing.T) {
		cfg := baseConfig(t)
		cfg.Vars = map[string]string{"task": "make the tests pass"}
		cfg.Rules[0].Then.Prompt.Text = "{{task}}: review {{impl.handoff}}"
		client := &fakeHerdr{}
		model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)
		e := newEngine(t, cfg, client, model)

		run(t, e, Transition{Slot: "impl", To: herdr.StatusDone})

		client.mu.Lock()
		defer client.mu.Unlock()
		if len(client.prompts) != 1 {
			t.Fatalf("prompts = %d, want 1", len(client.prompts))
		}
		want := "make the tests pass: review " + e.HandoffPath("impl")
		if client.prompts[0].text != want {
			t.Errorf("text = %q, want %q", client.prompts[0].text, want)
		}
		if client.prompts[0].target != "review-target" {
			t.Errorf("target = %q, want the model's target for review", client.prompts[0].target)
		}
	})

	t.Run("unresolved placeholder escalates instead of being sent", func(t *testing.T) {
		cfg := baseConfig(t)
		cfg.Rules[0].Then.Prompt.Text = "review {{nope}}"
		client := &fakeHerdr{}
		model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)
		e := newEngine(t, cfg, client, model)

		out := run(t, e, Transition{Slot: "impl", To: herdr.StatusDone})

		if n := client.promptCount(); n != 0 {
			t.Fatalf("prompts = %d, want 0 — a literal {{nope}} must never reach an agent", n)
		}
		if len(out.Escalations) != 1 {
			t.Fatalf("Escalations = %d, want 1", len(out.Escalations))
		}
	})
}

// A failed actuation escalates with the underlying error attached rather than
// being swallowed — the status-lies failure class (§4.9).
func TestFailedActionEscalatesWithTheCause(t *testing.T) {
	sentinel := errors.New("socket closed")
	client := &fakeHerdr{promptErr: sentinel}
	model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)
	e := newEngine(t, baseConfig(t), client, model)

	out := run(t, e, Transition{Slot: "impl", To: herdr.StatusDone})

	if len(out.Escalations) != 1 {
		t.Fatalf("Escalations = %d, want 1", len(out.Escalations))
	}
	if !errors.Is(out.Escalations[0].Err, sentinel) {
		t.Errorf("Err = %v, want it to wrap %v", out.Escalations[0].Err, sentinel)
	}
	if !strings.Contains(out.Escalations[0].Err.Error(), "agent.prompt") {
		t.Errorf("Err = %q, want it to name the method that failed", out.Escalations[0].Err)
	}
}

// A halted slot stays halted: no later transition brings it back into the fold.
func TestHaltedSlotIsIgnoredAfterwards(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusBlocked).set("review", herdr.StatusIdle)
	model.blocked["impl"] = "Continue?"
	e := newEngine(t, baseConfig(t), client, model)

	ch := make(chan Transition, 2)
	ch <- Transition{Slot: "impl", To: herdr.StatusBlocked}
	close(ch)
	if _, err := e.Run(context.Background(), ch); err != nil {
		t.Fatalf("Run: %v", err)
	}

	model.set("impl", herdr.StatusDone)
	run(t, e, Transition{Slot: "impl", To: herdr.StatusDone})

	if n := client.promptCount(); n != 0 {
		t.Errorf("prompts = %d, want 0 — a halted slot must stay out of the fold", n)
	}
}

// New rejects configurations whose failure mode would otherwise be a misfire
// against a live agent.
func TestNewRejectsUnsafeConfig(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"no iteration budget", func(c *Config) { c.MaxIterations = 0 }},
		{"no handoff dir", func(c *Config) { c.HandoffDir = "" }},
		{"unknown blocked policy", func(c *Config) { c.OnBlocked = "yolo" }},
		{"auto with no whitelist", func(c *Config) { c.OnBlocked = BlockedAuto }},
		{"blocked rule that sends nothing", func(c *Config) {
			c.OnBlocked = BlockedAuto
			c.BlockedRules = []BlockedRule{{Prompt: "Continue?"}}
		}},
		{"duplicate slot", func(c *Config) {
			c.Slots = append(c.Slots, SlotConfig{Name: "impl", Kind: "codex"})
		}},
		{"unnamed rule", func(c *Config) { c.Rules[0].Name = "" }},
		{"unknown predicate op", func(c *Config) { c.Rules[0].When = Predicate{Op: "matches", Field: "status"} }},
		{"in with no values", func(c *Config) {
			c.Rules[0].When = Predicate{Op: OpIn, Field: "status"}
		}},
		{"prompt to an unknown slot", func(c *Config) {
			c.Rules[0].Then = Action{Prompt: &PromptAction{Slot: "ghost", Text: "hi"}}
		}},
		{"action with two branches", func(c *Config) {
			c.Rules[0].Then.Finish = &FinishAction{Reason: "x"}
		}},
		{"action with no branch", func(c *Config) { c.Rules[0].Then = Action{} }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t)
			tc.mut(&cfg)
			if _, err := New(cfg, &fakeHerdr{}, newModel(), quietLogger()); err == nil {
				t.Fatal("New accepted a config it must reject")
			}
		})
	}
}

func TestParseHandoff(t *testing.T) {
	t.Run("nested fields resolve by dotted path", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "review.md")
		writeFile(t, p, "---\nreview:\n  verdict: clean\n  findings: 0\n  passed: true\n---\nbody\n")
		h, err := ParseHandoff(p)
		if err != nil {
			t.Fatalf("ParseHandoff: %v", err)
		}
		for field, want := range map[string]string{
			"review.verdict":  "clean",
			"review.findings": "0",
			"review.passed":   "true",
		} {
			got, ok := h.Lookup(field)
			if !ok || got != want {
				t.Errorf("Lookup(%q) = %q,%v want %q,true", field, got, ok, want)
			}
		}
		if _, ok := h.Lookup("review"); ok {
			t.Error("a table is not a scalar and must not resolve")
		}
		if _, ok := h.Lookup("review.missing"); ok {
			t.Error("an absent field must not resolve")
		}
		if h.Body != "body\n" {
			t.Errorf("Body = %q, want the text after the front-matter", h.Body)
		}
	})

	t.Run("CRLF does not hide front-matter", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "win.md")
		writeFile(t, p, "---\r\nverdict: clean\r\n---\r\nbody\r\n")
		h, err := ParseHandoff(p)
		if err != nil {
			t.Fatalf("ParseHandoff: %v", err)
		}
		if v, ok := h.Lookup("verdict"); !ok || v != "clean" {
			t.Errorf("Lookup(verdict) = %q,%v want clean,true", v, ok)
		}
	})

	t.Run("unterminated front-matter is body, not an error", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "open.md")
		writeFile(t, p, "---\nverdict: clean\n")
		h, err := ParseHandoff(p)
		if err != nil {
			t.Fatalf("ParseHandoff: %v", err)
		}
		if len(h.Fields) != 0 {
			t.Errorf("Fields = %v, want none", h.Fields)
		}
	})

	t.Run("malformed front-matter is an error, not silence", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bad.md")
		writeFile(t, p, "---\nverdict: [unclosed\n---\nbody\n")
		if _, err := ParseHandoff(p); err == nil {
			t.Fatal("malformed YAML front-matter parsed without error")
		}
	})

	t.Run("missing file reports fs.ErrNotExist", func(t *testing.T) {
		_, err := ParseHandoff(filepath.Join(t.TempDir(), "nope.md"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("err = %v, want it to wrap fs.ErrNotExist", err)
		}
	})
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
