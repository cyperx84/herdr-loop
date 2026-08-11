package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	herdr "github.com/cyperx84/herdr-api"
)

// fakeAgents is the authoritative side of the reconciler. The model is tested
// against it rather than a live herdr connection: the properties under test are
// about ordering and liveness, and a real session cannot be made to replay on
// demand.
type fakeAgents struct {
	list  []herdr.Agent
	err   error
	calls int
}

func (f *fakeAgents) AgentList(context.Context) ([]herdr.Agent, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]herdr.Agent, len(f.list))
	copy(out, f.list)
	return out, nil
}

// fakeClock keeps the reconcile schedule testable without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *fakeClock                   { return &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()} }
func agentKind(s string) *string             { return &s }
func ctxb() context.Context                  { return context.Background() }

// Frames are written as wire JSON, not marshalled structs, so the tests pin the
// field names the server actually sends.
func statusEvent(paneID, kind string, status herdr.AgentStatus) herdr.Event {
	data := fmt.Sprintf(`{"type":"pane_agent_status_changed","pane_id":%q,"workspace_id":"w1","agent_status":%q,"agent":%q}`,
		paneID, status, kind)
	return herdr.Event{Kind: "pane_agent_status_changed", Data: json.RawMessage(data)}
}

func paneCreatedEvent(paneID, kind string, status herdr.AgentStatus, revision uint64) herdr.Event {
	data := fmt.Sprintf(`{"type":"pane_created","pane":{"pane_id":%q,"terminal_id":"t","workspace_id":"w1","tab_id":"w1:t1","focused":false,"agent":%q,"agent_status":%q,"revision":%d}}`,
		paneID, kind, status, revision)
	return herdr.Event{Kind: "pane_created", Data: json.RawMessage(data)}
}

func paneUpdatedEvent(paneID, kind string, status herdr.AgentStatus, revision uint64) herdr.Event {
	ev := paneCreatedEvent(paneID, kind, status, revision)
	ev.Kind = "pane_updated"
	return ev
}

func paneExitedEvent(paneID string) herdr.Event {
	data := fmt.Sprintf(`{"type":"pane_exited","pane_id":%q,"workspace_id":"w1"}`, paneID)
	return herdr.Event{Kind: "pane_exited", Data: json.RawMessage(data)}
}

func agent(paneID, kind string, status herdr.AgentStatus, structured bool) herdr.Agent {
	return herdr.Agent{
		Agent:               agentKind(kind),
		Status:              status,
		PaneID:              paneID,
		WorkspaceID:         "w1",
		ScreenDetectionSkip: structured,
	}
}

// goLive drives the handover the way the supervisor does: reconcile until the
// model agrees with authoritative state. The bound documents the contract —
// a non-empty session needs one reconcile to adopt and one to agree.
func goLive(t *testing.T, m *Model) {
	t.Helper()
	for i := 0; i < 3 && !m.IsLive(); i++ {
		if _, err := m.Reconcile(ctxb()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if !m.IsLive() {
		t.Fatal("model never went live after three reconciles")
	}
}

// The core contract: the replayed prefix must move the model without moving the
// engine. Every one of these events would fire a rule if edge-triggered.
func TestReplayedBurstProducesNoActionableTransitions(t *testing.T) {
	clk := newClock()
	m := New(&fakeAgents{}, Options{Now: clk.now})

	replay := []herdr.Event{
		paneCreatedEvent("w1:p1", "claude", herdr.StatusWorking, 1),
		statusEvent("w1:p1", "claude", herdr.StatusDone),
		statusEvent("w1:p1", "claude", herdr.StatusIdle),
		paneCreatedEvent("w1:p2", "codex", herdr.StatusWorking, 1),
		statusEvent("w1:p2", "codex", herdr.StatusDone),
	}
	for _, ev := range replay {
		if tr, ok := m.Apply(ev); ok {
			t.Fatalf("replayed %s produced actionable transition %+v", ev.Kind, tr)
		}
	}

	if m.IsLive() {
		t.Error("model went live without reconciling against authoritative state")
	}
	if m.Stats().Transitions != 0 {
		t.Errorf("Transitions = %d during replay, want 0", m.Stats().Transitions)
	}
	// The model must still have absorbed the replay — that is what makes the
	// first reconcile able to agree.
	if a, ok := m.Get("w1:p1"); !ok || a.Status != herdr.StatusIdle {
		t.Errorf("replay was not folded into the model: %+v (found=%v)", a, ok)
	}
}

// The single most dangerous replay artifact: the observed backlog contained
// pane_exited frames for panes that had died long before the process started.
func TestReplayedPaneExitIsNotActionable(t *testing.T) {
	clk := newClock()
	m := New(&fakeAgents{}, Options{Now: clk.now})

	m.Apply(paneCreatedEvent("w1:p9", "claude", herdr.StatusWorking, 1))
	if tr, ok := m.Apply(paneExitedEvent("w1:p9")); ok {
		t.Fatalf("replayed pane_exited produced actionable transition %+v", tr)
	}
	if _, ok := m.Get("w1:p9"); ok {
		t.Error("pane_exited must still be folded: the dead pane should be gone from the model")
	}
}

// After the handover, the same class of event that was inert during replay must
// drive the engine.
func TestLiveEventProducesTransition(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p1", "claude", herdr.StatusWorking, false)}}
	m := New(src, Options{Now: clk.now})

	m.Apply(statusEvent("w1:p1", "claude", herdr.StatusWorking)) // replayed
	goLive(t, m)

	tr, ok := m.Apply(statusEvent("w1:p1", "claude", herdr.StatusDone))
	if !ok {
		t.Fatal("live status change produced no transition")
	}
	if tr.From != herdr.StatusWorking || tr.To != herdr.StatusDone {
		t.Errorf("transition = %s→%s, want working→done", tr.From, tr.To)
	}
	if tr.Source != SourceEvent {
		t.Errorf("Source = %q, want %q", tr.Source, SourceEvent)
	}
	if !tr.BecameSettled() {
		t.Error("working→done must satisfy BecameSettled: it is the handoff predicate")
	}
}

// A dropped event is not directly observable — herdr.Stream discards silently
// when its buffer fills — so the model has to repair itself. This is the
// self-healing contract the whole periodic reconcile exists for.
func TestDivergenceBetweenSnapshotsIsRepairedByNextReconcile(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p1", "claude", herdr.StatusWorking, false)}}
	m := New(src, Options{Now: clk.now})
	goLive(t, m)

	before := m.Stats().LiveDivergences

	// The agent finished; its pane_agent_status_changed was dropped, so no
	// event ever reaches Apply.
	src.list = []herdr.Agent{agent("w1:p1", "claude", herdr.StatusDone, false)}

	rec, err := m.Reconcile(ctxb())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Divergences != 1 {
		t.Fatalf("Divergences = %d, want 1", rec.Divergences)
	}
	if len(rec.Transitions) != 1 {
		t.Fatalf("Transitions = %d, want the repair transition", len(rec.Transitions))
	}
	tr := rec.Transitions[0]
	if tr.Source != SourceReconcile {
		t.Errorf("Source = %q, want %q", tr.Source, SourceReconcile)
	}
	if !tr.BecameSettled() {
		t.Errorf("repair of a lost working→done must still satisfy BecameSettled, got %s→%s", tr.From, tr.To)
	}
	if got := m.Stats().LiveDivergences; got != before+1 {
		t.Errorf("LiveDivergences = %d, want %d — a post-live divergence is the only evidence of a dropped event", got, before+1)
	}

	// Idempotent: reconciling again against unchanged state finds nothing.
	rec2, err := m.Reconcile(ctxb())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec2.Divergences != 0 || len(rec2.Transitions) != 0 {
		t.Errorf("second reconcile diverged again: %+v", rec2)
	}
}

// Divergence before the handover is expected — the replayed prefix is stale
// history — and must not be counted as evidence of a dropped event.
func TestReplayDivergenceIsCountedSeparately(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p1", "claude", herdr.StatusIdle, false)}}
	m := New(src, Options{Now: clk.now})

	m.Apply(statusEvent("w1:p1", "claude", herdr.StatusWorking)) // stale replay
	goLive(t, m)

	s := m.Stats()
	if s.ReplayDivergences == 0 {
		t.Error("stale replay should have diverged from the authoritative list")
	}
	if s.LiveDivergences != 0 {
		t.Errorf("LiveDivergences = %d before the stream went live, want 0", s.LiveDivergences)
	}
}

// An agent that disappears between snapshots is a real event for the engine —
// rules escalate on a slot dying — but it must never look like completion.
func TestVanishedAgentReconcilesToGoneNotSettled(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p1", "claude", herdr.StatusWorking, false)}}
	m := New(src, Options{Now: clk.now})
	goLive(t, m)

	src.list = nil
	rec, err := m.Reconcile(ctxb())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rec.Transitions) != 1 || !rec.Transitions[0].Gone {
		t.Fatalf("want one Gone transition, got %+v", rec.Transitions)
	}
	if rec.Transitions[0].BecameSettled() {
		t.Error("an agent that vanished must never satisfy BecameSettled")
	}
	if _, ok := m.Get("w1:p1"); ok {
		t.Error("vanished agent still modelled")
	}
}

// unknown means "present but unclassifiable" — never proof of completion, in
// any of the three places the model could leak it.
func TestUnknownIsNeverSettled(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p1", "claude", herdr.StatusWorking, false)}}
	m := New(src, Options{Now: clk.now})
	goLive(t, m)

	tr, ok := m.Apply(statusEvent("w1:p1", "claude", herdr.StatusUnknown))
	if !ok {
		t.Fatal("working→unknown is a real transition and must be reported")
	}
	if tr.BecameSettled() {
		t.Error("a transition into unknown must not satisfy BecameSettled")
	}
	a, _ := m.Get("w1:p1")
	if a.Settled() {
		t.Error("an agent at unknown must not report Settled")
	}
	if a.Trusted() {
		t.Error("a screen-classified agent must not report Trusted")
	}
}

// A seat reserved by launch_pending carries a status but has no agent in it.
// Reconciling it away would spuriously report a death; treating it as settled
// would prompt an empty pane.
func TestLaunchPendingIsTrackedButNeverSettled(t *testing.T) {
	clk := newClock()
	pending := herdr.Agent{PaneID: "w1:p3", WorkspaceID: "w1", Status: herdr.StatusIdle, LaunchPending: true}
	src := &fakeAgents{list: []herdr.Agent{pending}}
	m := New(src, Options{Now: clk.now})
	goLive(t, m)

	a, ok := m.Get("w1:p3")
	if !ok {
		t.Fatal("launch-pending seat must be modelled, not skipped")
	}
	if a.Settled() {
		t.Error("a launch-pending seat reports idle but has no agent: it must not be Settled")
	}
	if a.Kind != "" {
		t.Errorf("Kind = %q, want empty for a seat whose agent has not started", a.Kind)
	}

	// It must also stay put across reconciles rather than flapping in and out.
	rec, err := m.Reconcile(ctxb())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Divergences != 0 {
		t.Errorf("launch-pending seat diverged against itself: %+v", rec)
	}
}

// Detection tier is a per-agent runtime fact, not a per-kind constant, and no
// event payload carries it — only agent.list does.
func TestTierIsRuntimeFactAndUnknowableFromEvents(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{
		agent("w1:p1", "claude", herdr.StatusIdle, true),  // integration reports state
		agent("w1:p2", "claude", herdr.StatusIdle, false), // same kind, screen-classified
	}}
	m := New(src, Options{Now: clk.now})
	goLive(t, m)

	p1, _ := m.Get("w1:p1")
	p2, _ := m.Get("w1:p2")
	if p1.Tier != TierStructured || p2.Tier != TierScreen {
		t.Fatalf("tiers = %q/%q, want structured/screen from screen_detection_skipped alone", p1.Tier, p2.Tier)
	}
	if !p1.Trusted() || p2.Trusted() {
		t.Error("Trusted must follow the runtime flag, not the kind")
	}

	// A pane the model only ever saw on the wire has no knowable tier.
	m.Apply(paneCreatedEvent("w1:p7", "codex", herdr.StatusWorking, 1))
	p7, ok := m.Get("w1:p7")
	if !ok {
		t.Fatal("pane_created for an agent pane should be tracked")
	}
	if p7.Tier != TierUnknown {
		t.Errorf("Tier = %q for a pane known only from events, want %q", p7.Tier, TierUnknown)
	}
	if p7.Trusted() {
		t.Error("an unknown tier must never be trusted")
	}
}

// herdr guarantees no subscriber ordering, so the fold must survive duplication
// and out-of-order redelivery without inventing transitions.
func TestFoldIsIdempotentAndRejectsStaleRevisions(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p1", "claude", herdr.StatusWorking, false)}}
	m := New(src, Options{Now: clk.now})
	goLive(t, m)

	if _, ok := m.Apply(statusEvent("w1:p1", "claude", herdr.StatusWorking)); ok {
		t.Error("re-delivering the status the model already holds must not be a transition")
	}
	if _, ok := m.Apply(paneUpdatedEvent("w1:p1", "claude", herdr.StatusDone, 5)); !ok {
		t.Fatal("a newer pane_updated should transition")
	}
	if _, ok := m.Apply(paneUpdatedEvent("w1:p1", "claude", herdr.StatusWorking, 2)); ok {
		t.Error("a pane_updated older than state already folded must be rejected, not replayed backwards")
	}
	if a, _ := m.Get("w1:p1"); a.Status != herdr.StatusDone {
		t.Errorf("Status = %q after a stale redelivery, want done", a.Status)
	}
	if m.Stats().EventsStale == 0 {
		t.Error("the rejected redelivery must be counted, not swallowed")
	}
}

// Panes running no agent are not the model's business: tracking one would make
// the next reconcile report a removal that never happened.
func TestPaneWithoutAgentIsNotTracked(t *testing.T) {
	clk := newClock()
	m := New(&fakeAgents{}, Options{Now: clk.now})

	data := `{"type":"pane_created","pane":{"pane_id":"w1:p4","terminal_id":"t","workspace_id":"w1","tab_id":"w1:t1","focused":false,"agent":null,"agent_status":"unknown","revision":1}}`
	m.Apply(herdr.Event{Kind: "pane_created", Data: json.RawMessage(data)})

	if _, ok := m.Get("w1:p4"); ok {
		t.Error("a shell pane must not enter the model — agent.list never returns it")
	}
}

// released reports the agent leaving a pane that survives it. Its final_status
// is the last thing herdr knew, not a completion the engine may act on.
func TestAgentReleaseIsGoneNotSettled(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p1", "claude", herdr.StatusWorking, false)}}
	m := New(src, Options{Now: clk.now})
	goLive(t, m)

	data := `{"type":"pane_agent_detected","pane_id":"w1:p1","workspace_id":"w1","agent":"claude","released":true,"final_status":"done"}`
	tr, ok := m.Apply(herdr.Event{Kind: "pane_agent_detected", Data: json.RawMessage(data)})
	if !ok {
		t.Fatal("a release on a tracked pane is a transition")
	}
	if !tr.Gone {
		t.Error("release must be reported as Gone")
	}
	if tr.BecameSettled() {
		t.Error("a released agent must not satisfy BecameSettled even with final_status done")
	}
	if tr.To != herdr.StatusDone {
		t.Errorf("To = %q, want the reported final_status", tr.To)
	}
}

// Reconcile scheduling is a model property, so it is exercised on an injected
// clock rather than by sleeping.
func TestReconcileDueFollowsInjectedClock(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{}
	m := New(src, Options{Now: clk.now, ReconcileEvery: time.Minute})

	if !m.ReconcileDue() {
		t.Error("a model that has never reconciled is due immediately")
	}
	if _, err := m.Reconcile(ctxb()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if m.ReconcileDue() {
		t.Error("due again with no time elapsed")
	}

	clk.advance(59 * time.Second)
	if m.ReconcileDue() {
		t.Error("due before the interval elapsed")
	}
	clk.advance(time.Second)
	if !m.ReconcileDue() {
		t.Error("not due after the interval elapsed — a dropped event would go unrepaired forever")
	}
}

// A failing snapshot must leave the model as it was and name the call that
// failed, so a supervisor log says which round-trip broke.
func TestReconcileErrorWrapsAndLeavesModelIntact(t *testing.T) {
	clk := newClock()
	sentinel := errors.New("dial refused")
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p1", "claude", herdr.StatusWorking, false)}}
	m := New(src, Options{Now: clk.now})
	goLive(t, m)

	src.err = sentinel
	if _, err := m.Reconcile(ctxb()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the transport error", err)
	}
	if _, ok := m.Get("w1:p1"); !ok {
		t.Error("a failed reconcile must not discard modelled state")
	}
	if !m.IsLive() {
		t.Error("a failed reconcile must not demote a live stream")
	}
}

// Unrelated event kinds are counted and dropped rather than reaching the fold.
func TestUnrelatedEventsAreIgnored(t *testing.T) {
	clk := newClock()
	m := New(&fakeAgents{}, Options{Now: clk.now})
	if _, ok := m.Apply(herdr.Event{Kind: "workspace_focused", Data: json.RawMessage(`{"type":"workspace_focused"}`)}); ok {
		t.Error("workspace_focused is not an agent transition")
	}
	if m.Stats().EventsIgnored != 1 {
		t.Errorf("EventsIgnored = %d, want 1", m.Stats().EventsIgnored)
	}
}

// A frame that will not decode is counted, not fatal: one bad frame is no
// reason for the supervisor to stop consuming the stream.
func TestMalformedFrameIsCountedNotFatal(t *testing.T) {
	clk := newClock()
	m := New(&fakeAgents{}, Options{Now: clk.now})
	if _, ok := m.Apply(herdr.Event{Kind: "pane_agent_status_changed", Data: json.RawMessage(`{"pane_id":42}`)}); ok {
		t.Error("a malformed frame must not produce a transition")
	}
	if m.Stats().EventsMalformed != 1 {
		t.Errorf("EventsMalformed = %d, want 1", m.Stats().EventsMalformed)
	}
	// The model still works afterwards.
	goLive(t, m)
}

// Regression for the F1 review finding: agreement with authoritative state is
// not evidence that replay has finished arriving.
//
// An empty session agrees with an empty agent.list trivially, on the very first
// reconcile, microseconds after Subscribe and with the entire replayed backlog
// still unread. Going live there makes every replayed event actionable.
func TestEmptySessionDoesNotGoLiveOnFirstReconcile(t *testing.T) {
	clk := newClock()
	m := New(&fakeAgents{}, Options{Now: clk.now})

	rec, err := m.Reconcile(ctxb())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Divergences != 0 {
		t.Fatalf("Divergences = %d, want 0 — the premise of this test is that an empty session agrees trivially", rec.Divergences)
	}
	if rec.WentLive || m.IsLive() {
		t.Fatal("model went live on its first reconcile: trivial agreement was mistaken for a drained replay")
	}

	// A replayed event arriving after that first agreement must still be inert.
	if tr, ok := m.Apply(paneCreatedEvent("w1:pdead", "claude", herdr.StatusDone, 1)); ok {
		t.Errorf("replayed event became actionable after a trivially-agreeing first reconcile: %+v", tr)
	}
}

// Regression for the F1 review finding, the damaging variant: replay is
// chronological and ends at current state, so a PARTIAL prefix agrees whenever
// the intermediate status it leaves behind happens to equal the current one.
// With a status oscillating working/idle that coincidence is ordinary.
//
// The model must stay in replay for as long as events keep arriving, however
// often authoritative state happens to agree.
func TestPartialReplayAgreementDoesNotGoLiveWhileEventsStillArrive(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p5", "claude", herdr.StatusWorking, false)}}
	m := New(src, Options{Now: clk.now})

	// A prefix of replay that lands on exactly the authoritative status.
	m.Apply(statusEvent("w1:p5", "claude", herdr.StatusWorking))

	rec, err := m.Reconcile(ctxb())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Divergences != 0 {
		t.Fatalf("Divergences = %d, want 0 — this test requires the coincidental agreement", rec.Divergences)
	}
	if m.IsLive() {
		t.Fatal("model went live mid-replay on coincidental agreement — the remaining backlog would be actionable")
	}

	// More replay keeps arriving between reconciles. Each reconcile still sees
	// agreement once it adopts, but the stream is demonstrably not quiet.
	for i := 0; i < 3; i++ {
		m.Apply(statusEvent("w1:p5", "claude", herdr.StatusWorking))
		clk.advance(DefaultReconcileInterval)
		if _, err := m.Reconcile(ctxb()); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if m.IsLive() {
			t.Fatalf("model went live after reconcile %d while events were still arriving", i)
		}
	}

	// Once the stream falls silent for a whole interval, handover is justified.
	clk.advance(DefaultReconcileInterval)
	rec, err = m.Reconcile(ctxb())
	if err != nil {
		t.Fatalf("final reconcile: %v", err)
	}
	if !rec.WentLive || !m.IsLive() {
		t.Fatal("model never went live after the event stream fell quiet")
	}
	if !rec.LivenessProven {
		t.Error("LivenessProven = false, want true: replay was observed to stop, not assumed")
	}
}

// A session that never falls quiet must not stall forever — but the handover
// has to admit it was assumed rather than demonstrated, because a stale
// replayed event may still be treated as actionable.
func TestBusySessionGoesLiveOnMaxWaitAndSaysLivenessWasAssumed(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p1", "claude", herdr.StatusWorking, false)}}
	m := New(src, Options{Now: clk.now, ReconcileEvery: time.Second, LiveMaxWait: 10 * time.Second})

	var got Reconciliation
	for i := 0; i < 20 && !m.IsLive(); i++ {
		// Never quiet: an event lands before every reconcile.
		m.Apply(statusEvent("w1:p1", "claude", herdr.StatusWorking))
		clk.advance(2 * time.Second)
		rec, err := m.Reconcile(ctxb())
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		got = rec
	}
	if !m.IsLive() {
		t.Fatal("busy session never went live: the max-wait escape hatch did not fire")
	}
	if !got.WentLive {
		t.Fatal("WentLive was not reported on the reconcile that handed over")
	}
	if got.LivenessProven {
		t.Error("LivenessProven = true, but the stream never fell quiet — this must report an assumption, not a proof")
	}
}

// The wire delivers per-pane status events under a dotted name the schema does
// not list. herdr-api normalizes it, but the model is where the cost of a
// mismatch lands, so pin the contract here too: this frame must produce a
// transition, not be silently ignored.
func TestNormalizedPerPaneStatusFrameIsFolded(t *testing.T) {
	clk := newClock()
	src := &fakeAgents{list: []herdr.Agent{agent("w1:p5", "opencode", herdr.StatusIdle, true)}}
	m := New(src, Options{Now: clk.now})

	// Drain replay and reach live.
	for i := 0; i < 4 && !m.IsLive(); i++ {
		clk.advance(DefaultReconcileInterval)
		if _, err := m.Reconcile(ctxb()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if !m.IsLive() {
		t.Fatal("model never went live")
	}

	before := m.Stats().EventsIgnored

	// idle -> working -> done, the real cycle a prompt drives. Only the second
	// leg "becomes" settled: idle was already settled, so a transition out of
	// it is not a completion signal.
	if _, ok := m.Apply(statusEvent("w1:p5", "opencode", herdr.StatusWorking)); !ok {
		t.Fatal("working transition was not actionable — the event kind was not recognised")
	}
	tr, ok := m.Apply(statusEvent("w1:p5", "opencode", herdr.StatusDone))
	if !ok {
		t.Fatal("a live status change produced no actionable transition — the kind was not recognised")
	}
	if tr.From != herdr.StatusWorking || tr.To != herdr.StatusDone {
		t.Errorf("transition = %+v, want working -> done", tr)
	}
	if !tr.BecameSettled() {
		t.Error("working -> done did not report BecameSettled; this is the edge that releases the next rule")
	}
	if tr.Tier != TierStructured {
		t.Errorf("tier = %q, want structured — opencode self-reports", tr.Tier)
	}
	if got := m.Stats().EventsIgnored; got != before {
		t.Errorf("EventsIgnored rose to %d — the frame was dropped rather than folded", got)
	}
}
