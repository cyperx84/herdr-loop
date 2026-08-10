package main

import (
	"context"
	"encoding/json"
	"sync"

	herdr "github.com/cyperx84/herdr-api"
)

// eventBus fans multiple herdr.Stream connections into one channel.
//
// This exists because of a protocol fact with no workaround: per-pane
// subscriptions (pane.agent_status_changed, pane.output_matched,
// pane.scroll_changed) require pane_id — there is no wildcard form. Following
// a whole session therefore means one global subscription for
// session-wide events (pane.created, pane.agent_detected, …) plus one
// dedicated subscription per pane that turns out to carry an agent, opened
// as panes appear and closed as they leave. Each subscription is its own
// connection (events.subscribe is the one method that keeps a connection
// open at all — herdr-api's own client doc), so this type is what turns N
// connections back into the single ordered stream state.Model.Apply expects
// to be driven from.
type eventBus struct {
	client *herdr.Client
	out    chan herdr.Event

	mu      sync.Mutex
	global  *herdr.Stream
	perPane map[string]*herdr.Stream
}

// newEventBus opens the global subscription and starts fanning it in. The
// subscription types here are exactly the ones state.Model.Apply switches on
// for session-wide events (see internal/state/model.go) — pane.created,
// pane.updated, pane.closed, pane.exited, pane.agent_detected — none of
// which take a pane_id.
func newEventBus(ctx context.Context, client *herdr.Client) (*eventBus, error) {
	global, err := client.Subscribe(ctx,
		herdr.Subscription{Type: "pane.created"},
		herdr.Subscription{Type: "pane.updated"},
		herdr.Subscription{Type: "pane.closed"},
		herdr.Subscription{Type: "pane.exited"},
		herdr.Subscription{Type: "pane.agent_detected"},
	)
	if err != nil {
		return nil, err
	}
	b := &eventBus{
		client:  client,
		out:     make(chan herdr.Event, 256),
		global:  global,
		perPane: make(map[string]*herdr.Stream),
	}
	b.pump(global)
	return b, nil
}

// pump forwards one stream's events onto the shared channel until the stream
// ends (context cancellation, server disconnect, or Close). Every stream —
// global and per-pane alike — gets its own pump goroutine; ordering across
// streams is not guaranteed, which is fine because state.Model.Apply is
// documented order-independent and idempotent (§4.12) precisely so this fan-
// in never has to serialize anything itself.
func (b *eventBus) pump(s *herdr.Stream) {
	go func() {
		for ev := range s.Events() {
			b.out <- ev
		}
	}()
}

// watch opens a pane.agent_status_changed subscription for paneID if one is
// not already open. Idempotent on purpose: pane.created and
// pane.agent_detected can both name the same pane, and a periodic
// state.Model.Reconcile can rediscover a pane the bus already watches.
func (b *eventBus) watch(ctx context.Context, paneID string) {
	if paneID == "" {
		return
	}
	b.mu.Lock()
	if _, already := b.perPane[paneID]; already {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	s, err := b.client.Subscribe(ctx, herdr.Subscription{Type: "pane.agent_status_changed", PaneID: paneID})
	if err != nil {
		// Not fatal: state.Model's periodic Reconcile is the self-healing
		// path for exactly this kind of miss (§4.8) — a status change on
		// this pane will surface as a reconcile-sourced transition instead
		// of an event-sourced one, just later.
		return
	}
	b.mu.Lock()
	b.perPane[paneID] = s
	b.mu.Unlock()
	b.pump(s)
}

// unwatch closes and forgets paneID's per-pane subscription, if any. Called
// once a transition reports the agent gone (Transition.Gone) — there is
// nothing left on that pane worth a dedicated connection for.
func (b *eventBus) unwatch(paneID string) {
	b.mu.Lock()
	s, ok := b.perPane[paneID]
	if ok {
		delete(b.perPane, paneID)
	}
	b.mu.Unlock()
	if ok {
		s.Close()
	}
}

// Events delivers every event from every stream this bus owns, in arrival
// order across streams (not a merge sort across them — see pump's doc).
func (b *eventBus) Events() <-chan herdr.Event { return b.out }

// Close ends every subscription this bus owns. The shared channel is
// deliberately never closed here: pump goroutines exit on their own once
// each stream's connection closes (which Close causes), and closing a
// channel other goroutines might still be sending on is a data race waiting
// to happen. The process exits shortly after Close in every call path this
// binary has, so the channel is reclaimed by process exit, not by a Close
// contract.
func (b *eventBus) Close() {
	b.global.Close()
	b.mu.Lock()
	for _, s := range b.perPane {
		s.Close()
	}
	b.perPane = make(map[string]*herdr.Stream)
	b.mu.Unlock()
}

// paneIDOf extracts a pane id from a raw event without going through
// state.Model.Apply, for the one purpose Apply can't serve: deciding whether
// to open a new per-pane watch even when the event describes a pane with no
// agent yet (Apply's applyPane intentionally ignores those — see its doc —
// so it reports no transition to key a watch off of).
//
// The two decode shapes mirror internal/state/model.go's own Apply switch
// exactly: pane_created/pane_updated nest the pane id under "pane" (a full
// herdr.Pane payload), every other event this bus watches for puts pane_id
// at the top level. Kept in sync with that switch by hand — there is no
// shared decoder to import without exporting one from state, which would be
// scope creep onto a package this plugin does not own.
func paneIDOf(ev herdr.Event) string {
	switch ev.Kind {
	case "pane_created", "pane_updated":
		var d struct {
			Pane struct {
				ID string `json:"pane_id"`
			} `json:"pane"`
		}
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			return d.Pane.ID
		}
	default:
		var d struct {
			PaneID string `json:"pane_id"`
		}
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			return d.PaneID
		}
	}
	return ""
}
