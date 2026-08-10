// Package state maintains the reconciled view of a herdr session that makes
// the replaying event stream safe to act on.
//
// events.subscribe replays event history before it streams live events — herdr
// 0.8.0, undocumented upstream, verified experimentally. Edge-triggering rules
// on event arrival therefore fires them for work that finished hours ago. The
// supervisor never acts on events. It folds them into a Model and acts only on
// the transitions the Model reports:
//
//	subscribe  → Apply the replayed prefix    (nothing is actionable)
//	           → Reconcile against agent.list (authoritative)
//	           → when the model agrees with the list, the stream is LIVE
//	           → from then on Apply reports actionable transitions
//
// Reconcile is periodic, not startup-only. The event stream drops events rather
// than applying backpressure to the server (see herdr.Stream), so a lost event
// has to be self-healing: a scheduled re-snapshot repairs the divergence and
// emits the transition the lost event would have produced. Reconciliation is
// order-independent and idempotent throughout, because herdr guarantees no
// subscriber ordering (PLAN §4.12).
package state

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	herdr "github.com/cyperx84/herdr-api"
)

// DefaultReconcileInterval bounds how long a dropped event can go unrepaired.
// agent.list is a single cheap round-trip, so the interval is chosen for
// recovery latency rather than to conserve calls.
const DefaultReconcileInterval = 30 * time.Second

// DefaultLiveMaxWait bounds how long the model holds out for a quiet interval
// before assuming liveness. A busy session may never fall silent; refusing to
// ever go live would be its own failure. When this expires the handover still
// happens, but Reconciliation.LivenessProven reports false so the caller can
// say plainly that liveness was assumed.
const DefaultLiveMaxWait = 2 * time.Minute

// Tier is how herdr learned an agent's status, which decides how far that
// status can be trusted. It is a per-agent runtime fact read from
// screen_detection_skipped, never a per-kind constant: the same kind reports
// differently by version and by whether its integration is installed.
type Tier string

const (
	// TierUnknown means no authoritative list has covered this pane yet. It is
	// the starting tier for every pane discovered from the event stream: no
	// event payload carries screen_detection_skipped — only agent.list's
	// AgentInfo does — so the tier is genuinely unknowable until the next
	// Reconcile. Treat it as untrusted.
	TierUnknown Tier = "unknown"

	// TierStructured means the agent reports its own state and herdr skips
	// screen classification. High trust.
	TierStructured Tier = "structured"

	// TierScreen means herdr classified the rendered buffer. Heuristic: a
	// loop gating irreversible work on it is unsound.
	TierScreen Tier = "screen"
)

// Trusted reports whether a status at this tier is a structured signal rather
// than a screen-classification heuristic.
func (t Tier) Trusted() bool { return t == TierStructured }

// Source records where a transition came from. It matters to a caller because
// a reconcile-sourced transition is a repair — proof that an event was lost —
// and is worth logging differently from the normal path.
type Source string

const (
	// SourceEvent is a transition folded from a live event.
	SourceEvent Source = "event"
	// SourceReconcile is a transition discovered by re-snapshotting, i.e. one
	// the event stream failed to deliver.
	SourceReconcile Source = "reconcile"
)

// AgentState is the model's view of the agent occupying one pane.
//
// The model tracks panes that have or are reserved for an agent, never plain
// shell panes: agent.list does not return those, so tracking them would make
// every reconcile report a removal that never happened.
type AgentState struct {
	PaneID      string
	WorkspaceID string

	// Kind is the agent kind ("claude", "codex", …). Empty when herdr reports
	// no agent — including a pane reserved by LaunchPending.
	Kind string

	Status herdr.AgentStatus
	Tier   Tier

	// LaunchPending marks a pane reserved for an agent that has not started
	// yet. herdr still reports a status for it, so it must be excluded from
	// Settled explicitly or a rule will prompt a seat with nothing in it.
	LaunchPending bool

	// Revision is the last pane revision folded in. Only pane_created and
	// pane_updated carry one; it exists to reject an out-of-order redelivery,
	// not as a general version.
	Revision uint64

	// Since is when Status last changed, on the model's clock. Rules use it to
	// tell "settled a moment ago" from "stalled for twenty minutes".
	Since time.Time
}

// Settled reports whether this agent is ready to accept a new prompt.
//
// StatusUnknown is excluded by herdr.AgentStatus.Settled — an unclassifiable
// agent has not been shown to be finished — and a launch-pending seat is
// excluded here, because herdr reports a status for a pane whose agent has not
// started.
func (a AgentState) Settled() bool { return !a.LaunchPending && a.Status.Settled() }

// Trusted reports whether this agent's status came from a structured signal.
// A screen-classified status is a heuristic and must be corroborated before it
// gates irreversible work (PLAN §4.9).
func (a AgentState) Trusted() bool { return a.Tier.Trusted() }

// Transition is a change in modelled state. Rules are evaluated against these,
// never against event arrival — that indirection is the whole point of this
// package.
type Transition struct {
	PaneID      string
	WorkspaceID string
	Kind        string

	From herdr.AgentStatus
	To   herdr.AgentStatus

	// Tier is the trust tier at the moment of the transition. TierUnknown
	// means the pane has not been covered by a reconcile yet.
	Tier Tier

	// LaunchPending mirrors AgentState.LaunchPending so a transition can be
	// judged without a second lookup.
	LaunchPending bool

	// Gone reports that the agent left the pane — it exited, the pane closed,
	// or herdr released the detection. To carries the last known status where
	// herdr supplied one.
	Gone bool

	Source Source
	At     time.Time
}

// BecameSettled reports whether this transition took the agent from
// not-ready-for-a-prompt to ready — the predicate the handoff rules fire on.
//
// A transition into StatusUnknown can never satisfy it, an agent that left its
// pane can never satisfy it, and neither can a launch-pending seat.
func (t Transition) BecameSettled() bool {
	return !t.Gone && !t.LaunchPending && !t.From.Settled() && t.To.Settled()
}

// Stats are the model's counters. They are the operational surface: a rising
// LiveDivergences is the only observable evidence that events are being lost.
type Stats struct {
	// EventsFolded counts events that changed or created modelled state.
	EventsFolded uint64
	// EventsIgnored counts events the model does not track, including pane
	// events for panes running no agent.
	EventsIgnored uint64
	// EventsMalformed counts frames whose payload would not decode. A rising
	// count means the server has moved past the pinned protocol.
	EventsMalformed uint64
	// EventsStale counts events rejected by the revision guard as older than
	// state already folded in.
	EventsStale uint64
	// Transitions counts actionable transitions reported to the caller.
	Transitions uint64
	// Reconciles counts completed authoritative fetches.
	Reconciles uint64
	// ReplayDivergences counts disagreements found before the stream went
	// live. These are expected — the replayed prefix is stale history — and
	// are kept separate so they cannot be mistaken for lost events.
	ReplayDivergences uint64
	// LiveDivergences counts disagreements found after the stream went live.
	// herdr.Stream drops events silently when its buffer fills, so a drop is
	// not directly observable; a post-live divergence is its proxy. Each one
	// means an event was lost or mis-modelled and the model was repaired.
	LiveDivergences uint64
}

// Snapshotter is the slice of the herdr client this package needs:
// authoritative agent state on demand.
//
// It is declared here rather than imported so the model can be tested without
// a live herdr connection; *herdr.Client satisfies it implicitly.
//
// agent.list rather than session.snapshot because it is the smallest
// authoritative answer that carries screen_detection_skipped — session.snapshot
// returns the same agent records plus workspaces, tabs and panes this model
// does not model, and is already ~18 KB on a live session.
type Snapshotter interface {
	AgentList(ctx context.Context) ([]herdr.Agent, error)
}

// Options configure a Model. The zero value is usable.
type Options struct {
	// Now supplies the model's clock. nil means time.Now. Tests inject a fake
	// so reconcile scheduling is exercised without sleeping.
	Now func() time.Time

	// ReconcileEvery is how stale the model may get before ReconcileDue
	// reports true. Zero means DefaultReconcileInterval.
	ReconcileEvery time.Duration

	// LiveMaxWait bounds the wait for a quiet interval before liveness is
	// assumed rather than proven. Zero means DefaultLiveMaxWait. Tests set it
	// small; a caller that would rather stall than risk acting on replay can
	// set it very large.
	LiveMaxWait time.Duration
}

// Model is the reconciled per-pane agent state.
//
// Apply and Reconcile are expected to be driven from one goroutine — the
// supervisor's event loop — with the read accessors called from elsewhere (the
// status command, the board). It is safe for concurrent use regardless.
type Model struct {
	src   Snapshotter
	now   func() time.Time
	every time.Duration

	// liveMaxWait bounds how long the model will hold out for a quiet
	// interval before assuming liveness. A genuinely busy session may never
	// fall silent, and refusing to ever go live is its own failure.
	liveMaxWait time.Duration

	mu            sync.RWMutex
	agents        map[string]*AgentState
	live          bool
	lastReconcile time.Time
	// firstReconcile anchors liveMaxWait. Zero until the first reconcile.
	firstReconcile time.Time
	// eventsAtLastReconcile snapshots the total event count observed at the
	// end of the previous pre-live reconcile. Liveness requires this to be
	// unchanged across an interval: agreement with agent.list says nothing
	// about whether replay has finished arriving, but silence does.
	eventsAtLastReconcile uint64
	stats                 Stats
}

// New returns a Model that reconciles against src, which must not be nil.
//
// The model starts in replay: nothing is actionable until Reconcile reports
// agreement with authoritative state.
func New(src Snapshotter, opts Options) *Model {
	m := &Model{
		src:         src,
		now:         opts.Now,
		every:       opts.ReconcileEvery,
		liveMaxWait: opts.LiveMaxWait,
		agents:      make(map[string]*AgentState),
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.every <= 0 {
		m.every = DefaultReconcileInterval
	}
	if m.liveMaxWait <= 0 {
		m.liveMaxWait = DefaultLiveMaxWait
	}
	return m
}

// IsLive reports whether the replayed prefix has been drained and the model has
// been corroborated against authoritative state. Rule predicates may only fire
// once it is true.
func (m *Model) IsLive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.live
}

// ReconcileDue reports whether the model is stale enough to re-snapshot. It is
// true before the first reconcile, so a caller can drive both the initial
// handover and the periodic repair from one branch.
func (m *Model) ReconcileDue() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.now().Sub(m.lastReconcile) >= m.every
}

// Get returns the modelled state of one pane's agent.
func (m *Model) Get(paneID string) (AgentState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.agents[paneID]
	if !ok {
		return AgentState{}, false
	}
	return *a, true
}

// Agents returns every modelled agent, ordered by pane id so status output and
// logs are stable across calls.
func (m *Model) Agents() []AgentState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AgentState, 0, len(m.agents))
	for _, a := range m.agents {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PaneID < out[j].PaneID })
	return out
}

// Stats returns a copy of the model's counters.
func (m *Model) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stats
}

// Apply folds one event into the model and reports whether it produced a
// transition a rule may act on.
//
// The returned bool is true only when both halves hold:
//
//   - the stream is live. During the replayed prefix Apply folds silently by
//     construction — that is what makes replay safe to consume, and it applies
//     to a replayed pane_exited exactly as it does to a replayed status
//     change.
//   - the event actually changed modelled state. A redelivery or a duplicate
//     is a no-op, because herdr guarantees no subscriber ordering and the
//     reconciler must be idempotent.
//
// The returned Transition is meaningful only when the bool is true.
func (m *Model) Apply(ev herdr.Event) (Transition, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch ev.Kind {
	case "pane_created", "pane_updated":
		var d struct {
			Pane herdr.Pane `json:"pane"`
		}
		if !m.decode(ev, &d) {
			return Transition{}, false
		}
		return m.applyPane(d.Pane)

	case "pane_agent_status_changed":
		var d herdr.AgentStatusChanged
		if !m.decode(ev, &d) {
			return Transition{}, false
		}
		kind := ""
		if d.Agent != nil {
			kind = *d.Agent
		}
		return m.setStatus(d.PaneID, d.WorkspaceID, kind, d.Status, SourceEvent)

	case "pane_agent_detected":
		var d struct {
			PaneID      string             `json:"pane_id"`
			WorkspaceID string             `json:"workspace_id"`
			Agent       *string            `json:"agent"`
			Released    bool               `json:"released"`
			FinalStatus *herdr.AgentStatus `json:"final_status"`
		}
		if !m.decode(ev, &d) {
			return Transition{}, false
		}
		if d.Released {
			// Schema-derived, not experimentally verified: released reports
			// the agent leaving the pane and final_status its last known
			// state. The pane survives, the agent does not.
			final := herdr.StatusUnknown
			if d.FinalStatus != nil {
				final = *d.FinalStatus
			}
			return m.detach(d.PaneID, final, SourceEvent)
		}
		// Detection carries no status, so registering a newly seen agent is
		// not itself a transition — rules gate on status, and the pane's tier
		// stays unknown until a reconcile covers it.
		kind := ""
		if d.Agent != nil {
			kind = *d.Agent
		}
		if _, created := m.track(d.PaneID, d.WorkspaceID, kind); created {
			m.stats.EventsFolded++
		} else {
			m.stats.EventsIgnored++
		}
		return Transition{}, false

	case "pane_exited", "pane_closed":
		var d struct {
			PaneID string `json:"pane_id"`
		}
		if !m.decode(ev, &d) {
			return Transition{}, false
		}
		return m.detach(d.PaneID, herdr.StatusUnknown, SourceEvent)

	default:
		m.stats.EventsIgnored++
		return Transition{}, false
	}
}

// Reconciliation reports what one Reconcile found.
type Reconciliation struct {
	// Divergences is how many panes disagreed with authoritative state before
	// it was adopted.
	Divergences int

	// Transitions are the changes adoption made to an already-live model — the
	// repair path for events the stream dropped. Always empty before the
	// stream goes live: nothing reconstructed from replay is actionable.
	// Ordered by pane id, because removals are found by map iteration.
	Transitions []Transition

	// WentLive is true on the reconcile that handed the stream over to the
	// rule engine.
	WentLive bool

	// LivenessProven distinguishes the two ways WentLive can happen. True
	// means replay was observed to stop: authoritative state agreed AND no
	// event arrived for a whole reconcile interval. False means liveMaxWait
	// expired on a session that never fell quiet, so liveness was assumed
	// rather than demonstrated — callers should say so in their logs, because
	// a stale replayed event may still be treated as actionable.
	LivenessProven bool
}

// Reconcile fetches authoritative agent state and adopts it, reporting what
// diverged.
//
// Authoritative state always wins: the model cannot tell "I am ahead of the
// snapshot" from "I missed an event", so it takes the server's answer and
// reports the difference. The stream goes live on the first reconcile that
// finds zero divergences *before* adoption — after that the model equals the
// server's answer, so agreement at the next reconcile only requires that the
// events since were folded correctly. A model built from a non-empty session
// therefore needs at least two reconciles to go live: one to adopt, one to
// agree. Call it in a loop until WentLive rather than waiting a full interval.
//
// Call it from the goroutine that calls Apply, after draining what the event
// channel already holds — comparing against a snapshot while known events sit
// unapplied manufactures divergence.
func (m *Model) Reconcile(ctx context.Context) (Reconciliation, error) {
	// Fetched outside the lock: this is a socket round-trip and the status
	// surface reads the model concurrently.
	agents, err := m.src.AgentList(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("state: reconcile agent.list: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	m.lastReconcile = now
	if m.firstReconcile.IsZero() {
		m.firstReconcile = now
	}
	m.stats.Reconciles++

	var rec Reconciliation
	present := make(map[string]struct{}, len(agents))

	for _, a := range agents {
		want := authoritative(a, now)
		present[want.PaneID] = struct{}{}

		cur, tracked := m.agents[want.PaneID]
		if tracked && agrees(*cur, want) {
			// Tier and Revision refresh unconditionally and are never
			// divergence-bearing: no event payload carries
			// screen_detection_skipped, so the model cannot be wrong about a
			// tier it had no way to learn.
			cur.Tier, cur.Revision, cur.WorkspaceID = want.Tier, want.Revision, want.WorkspaceID
			continue
		}

		rec.Divergences++
		from := herdr.StatusUnknown
		if tracked {
			from = cur.Status
			if cur.Status == want.Status {
				want.Since = cur.Since // adopt the record, keep the age
			}
		}
		adopted := want
		m.agents[want.PaneID] = &adopted

		if m.live && from != want.Status {
			rec.Transitions = append(rec.Transitions, transitionOf(adopted, from, want.Status, false, SourceReconcile, now))
		}
	}

	for id, cur := range m.agents {
		if _, ok := present[id]; ok {
			continue
		}
		// agent.list returns launch-pending seats, so an absent pane really is
		// an agent that went away rather than one that has not started.
		rec.Divergences++
		delete(m.agents, id)
		if m.live {
			rec.Transitions = append(rec.Transitions, transitionOf(*cur, cur.Status, herdr.StatusUnknown, true, SourceReconcile, now))
		}
	}

	sort.Slice(rec.Transitions, func(i, j int) bool { return rec.Transitions[i].PaneID < rec.Transitions[j].PaneID })

	if m.live {
		m.stats.LiveDivergences += uint64(rec.Divergences)
		m.stats.Transitions += uint64(len(rec.Transitions))
		return rec, nil
	}

	m.stats.ReplayDivergences += uint64(rec.Divergences)

	// Going live is a claim that the replayed prefix has finished arriving.
	// Agreement with agent.list cannot support that claim: replay is
	// chronological and ends at current state, so a PARTIAL prefix agrees
	// whenever the intermediate status it left behind happens to equal the
	// current one — common when a status oscillates working/idle. Going live
	// there makes the remaining stale events actionable, which is the exact
	// failure this model exists to prevent.
	//
	// Quiescence is the evidence that does support it: no event arrived for a
	// whole reconcile interval. Requiring two reconciles also stops an empty
	// session going live on the first pass with the entire backlog unread.
	events := m.stats.EventsFolded + m.stats.EventsIgnored
	quiet := m.stats.Reconciles >= 2 && events == m.eventsAtLastReconcile
	m.eventsAtLastReconcile = events

	switch {
	case rec.Divergences == 0 && quiet:
		m.live = true
		rec.WentLive = true
		rec.LivenessProven = true
	case !m.firstReconcile.IsZero() && now.Sub(m.firstReconcile) >= m.liveMaxWait:
		// A busy session may never fall silent. Proceed, but tell the truth
		// about it: liveness is assumed, not proven.
		m.live = true
		rec.WentLive = true
		rec.LivenessProven = false
	}
	return rec, nil
}

// decode reports whether an event payload parsed. A frame the model cannot
// read is counted rather than returned as an error: Apply sits in the event
// loop's hot path and one bad frame is no reason to stop consuming. Watch
// Stats.EventsMalformed instead — it rising is a protocol-drift signal.
func (m *Model) decode(ev herdr.Event, out any) bool {
	if err := json.Unmarshal(ev.Data, out); err != nil {
		m.stats.EventsMalformed++
		return false
	}
	return true
}

// applyPane folds a PaneInfo-bearing event.
func (m *Model) applyPane(p herdr.Pane) (Transition, bool) {
	cur, tracked := m.agents[p.ID]

	kind := ""
	if p.Agent != nil {
		kind = *p.Agent
	}
	if kind == "" && !tracked {
		// A pane running no agent is not this model's business: tracking it
		// would make the next reconcile see a pane agent.list never returns
		// and report a removal that did not happen. An agent field that turns
		// nil on a pane already tracked is left alone — whether that means
		// departure is unverified, and the next reconcile settles it either
		// way.
		m.stats.EventsIgnored++
		return Transition{}, false
	}

	// pane_created and pane_updated are the only events carrying a revision,
	// so they are the only ones that can be ordered at all, and herdr
	// guarantees no subscriber ordering. Known hole, left unfixed because the
	// wire cannot close it: pane_agent_status_changed carries no revision, so
	// a status change followed by a pane_updated whose revision predates it
	// still reverts the status. The periodic reconcile is that repair.
	if tracked && p.Revision < cur.Revision {
		m.stats.EventsStale++
		return Transition{}, false
	}

	t, ok := m.setStatus(p.ID, p.WorkspaceID, kind, p.AgentStatus, SourceEvent)
	if a := m.agents[p.ID]; a != nil {
		a.Revision = p.Revision
	}
	return t, ok
}

// track registers a pane as carrying an agent without asserting a status, and
// reports whether the entry was created by this call.
func (m *Model) track(paneID, workspaceID, kind string) (*AgentState, bool) {
	a, ok := m.agents[paneID]
	if !ok {
		a = &AgentState{
			PaneID: paneID,
			Status: herdr.StatusUnknown,
			Tier:   TierUnknown,
			Since:  m.now(),
		}
		m.agents[paneID] = a
	}
	if workspaceID != "" {
		a.WorkspaceID = workspaceID
	}
	if kind != "" {
		a.Kind = kind
	}
	return a, !ok
}

// setStatus folds a status observation, reporting a transition only when the
// status actually moved.
func (m *Model) setStatus(paneID, workspaceID, kind string, to herdr.AgentStatus, src Source) (Transition, bool) {
	if paneID == "" {
		m.stats.EventsIgnored++
		return Transition{}, false
	}
	a, created := m.track(paneID, workspaceID, kind)

	from := a.Status
	if from == to {
		if created {
			m.stats.EventsFolded++
		} else {
			m.stats.EventsIgnored++
		}
		return Transition{}, false // redelivery or duplicate
	}
	a.Status = to
	a.Since = m.now()
	m.stats.EventsFolded++

	return m.report(transitionOf(*a, from, to, false, src, a.Since))
}

// detach folds an agent leaving its pane.
func (m *Model) detach(paneID string, final herdr.AgentStatus, src Source) (Transition, bool) {
	a, ok := m.agents[paneID]
	if !ok {
		// Untracked pane: nothing left to report. This is the common shape of
		// a replayed pane_exited for a pane that died long before the process
		// started.
		m.stats.EventsIgnored++
		return Transition{}, false
	}
	delete(m.agents, paneID)
	m.stats.EventsFolded++
	return m.report(transitionOf(*a, a.Status, final, true, src, m.now()))
}

// report gates a transition on liveness. A transition reconstructed from the
// replayed prefix describes state the session left behind, possibly hours ago;
// acting on it spawns agents and runs commands against stale history.
func (m *Model) report(t Transition) (Transition, bool) {
	if !m.live {
		return Transition{}, false
	}
	m.stats.Transitions++
	return t, true
}

func transitionOf(a AgentState, from, to herdr.AgentStatus, gone bool, src Source, at time.Time) Transition {
	return Transition{
		PaneID:        a.PaneID,
		WorkspaceID:   a.WorkspaceID,
		Kind:          a.Kind,
		From:          from,
		To:            to,
		Tier:          a.Tier,
		LaunchPending: a.LaunchPending,
		Gone:          gone,
		Source:        src,
		At:            at,
	}
}

// authoritative converts one agent.list record into modelled state. The tier
// is read from the record's own screen_detection_skipped — this is the only
// place a tier can be learned, and the only correct way to learn one.
func authoritative(a herdr.Agent, now time.Time) AgentState {
	kind := ""
	if a.Agent != nil {
		kind = *a.Agent
	}
	tier := TierScreen
	if a.ScreenDetectionSkip {
		tier = TierStructured
	}
	return AgentState{
		PaneID:        a.PaneID,
		WorkspaceID:   a.WorkspaceID,
		Kind:          kind,
		Status:        a.Status,
		Tier:          tier,
		LaunchPending: a.LaunchPending,
		Revision:      a.Revision,
		Since:         now,
	}
}

// agrees compares only what the event stream can also carry. Tier and Revision
// are excluded deliberately: the model has no way to learn a tier from events,
// so disagreeing about one is not evidence of a lost event.
func agrees(cur, want AgentState) bool {
	return cur.Status == want.Status &&
		cur.Kind == want.Kind &&
		cur.LaunchPending == want.LaunchPending
}
