package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- teardown ----------------------------------------------------------
//
// PLAN §4.10: teardown must never destroy uncommitted work. These tests are
// the invariant made executable — a real prior-art incident
// (sean1588/herdr-orchestrator #34) is a force-removed worktree that
// destroyed a completed-but-uncommitted implementation, and this file
// exists to prove that code path is unreachable here.
//
// fakeGit stands in for git so these run with no real filesystem, no real
// git binary, and no real uncommitted change ever created — see fakeGit in
// engine_test.go.

// worktreeSlotConfig is a slot whose cwd is a worktree Teardown may remove —
// as opposed to baseConfig's bare-cwd slots, which Teardown must never touch
// with worktree.remove no matter what git says about them.
func worktreeSlotConfig(name, kind, cwd, workspaceID string) SlotConfig {
	return SlotConfig{Name: name, Kind: kind, CWD: cwd, WorkspaceID: workspaceID, Worktree: true}
}

func teardownConfig(t *testing.T, slots ...SlotConfig) Config {
	t.Helper()
	return Config{
		Name:          "teardown-test",
		MaxIterations: 10,
		HandoffDir:    t.TempDir(),
		Slots:         slots,
	}
}

// A dirty worktree must survive Teardown untouched: no worktree.remove call
// at all, ever — this is the sean1588 incident made unreachable. Everything
// else Teardown owns (the pane, the kind token) is still released, and the
// human finds out through a loud log line and a structured escalation
// naming the path, not by discovering later that the tree is simply gone.
func TestTeardownPreservesDirtyWorktree(t *testing.T) {
	dir := t.TempDir()
	git := newFakeGit()
	git.dirty[dir] = true

	cfg := teardownConfig(t, worktreeSlotConfig("impl", "claude", dir, "ws-impl"))
	cfg.Git = git
	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, newModel())

	ctx := context.Background()
	if err := e.Spawn(ctx, "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if err := e.Teardown(ctx, "impl"); err != nil {
		t.Fatalf("Teardown returned an error for a correctly-preserved dirty tree: %v", err)
	}

	if len(client.removedWorktrees) != 0 {
		t.Fatalf("worktree.remove called on a dirty tree: %v — the invariant this file exists to enforce failed", client.removedWorktrees)
	}
	if len(client.closedPanes) != 1 {
		t.Fatalf("closedPanes = %v, want the pane closed even though the worktree was preserved", client.closedPanes)
	}

	escs := e.takeEscalations()
	if len(escs) != 1 {
		t.Fatalf("escalations = %v, want exactly one for the dirty tree", escs)
	}
	if escs[0].Reason != ReasonWorktreeDirty {
		t.Errorf("Reason = %q, want %q", escs[0].Reason, ReasonWorktreeDirty)
	}
	if escs[0].Path != dir {
		t.Errorf("Path = %q, want %q — a human must be able to find the tree from the escalation alone", escs[0].Path, dir)
	}
	if escs[0].Slot != "impl" {
		t.Errorf("Slot = %q, want %q", escs[0].Slot, "impl")
	}
}

// A clean worktree may be removed, and Teardown is the one caller that may
// do it — but only via worktree.remove(workspace_id, force=false) against
// the slot's own workspace.
func TestTeardownRemovesCleanWorktree(t *testing.T) {
	dir := t.TempDir()
	git := newFakeGit() // clean: no entry for dir

	cfg := teardownConfig(t, worktreeSlotConfig("impl", "claude", dir, "ws-impl"))
	cfg.Git = git
	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, newModel())

	ctx := context.Background()
	if err := e.Spawn(ctx, "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := e.Teardown(ctx, "impl"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	if len(client.removedWorktrees) != 1 {
		t.Fatalf("removedWorktrees = %v, want exactly one worktree.remove call", client.removedWorktrees)
	}
	got := client.removedWorktrees[0]
	if got.workspaceID != "ws-impl" {
		t.Errorf("workspace_id = %q, want %q", got.workspaceID, "ws-impl")
	}
	if got.force {
		t.Error("force = true on a clean-tree removal — §4.10 requires force removal be unreachable from any code path here")
	}
	if len(e.takeEscalations()) != 0 {
		t.Error("a clean removal must not escalate")
	}
}

// §4.10's requirement in its own words: "force removal of a dirty worktree
// is not reachable from any code path." This sweeps every worktree.remove
// call recorded across both outcomes and asserts none of them ever carries
// force=true — the dirty case never calls it at all, and the clean case
// calls it with force=false, so the assertion holds by construction, but it
// is pinned here as its own named test so a future change that starts
// passing force=true anywhere gets caught by name, not folded into an
// unrelated test's incidental assertion.
func TestTeardownNeverPassesForceTrue(t *testing.T) {
	dirtyDir, cleanDir := t.TempDir(), t.TempDir()
	git := newFakeGit()
	git.dirty[dirtyDir] = true

	cfg := teardownConfig(t,
		worktreeSlotConfig("dirty", "claude", dirtyDir, "ws-dirty"),
		worktreeSlotConfig("clean", "codex", cleanDir, "ws-clean"),
	)
	cfg.Git = git
	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, newModel())

	ctx := context.Background()
	for _, slot := range []string{"dirty", "clean"} {
		if err := e.Spawn(ctx, slot); err != nil {
			t.Fatalf("Spawn %s: %v", slot, err)
		}
	}
	for _, slot := range []string{"dirty", "clean"} {
		if err := e.Teardown(ctx, slot); err != nil {
			t.Fatalf("Teardown %s: %v", slot, err)
		}
	}

	for _, call := range client.removedWorktrees {
		if call.force {
			t.Errorf("worktree.remove(%q, force=true) — force removal must never be reachable", call.workspaceID)
		}
	}
}

// A slot with no worktree (Worktree: false, the zero value) is never handed
// to git at all, and worktree.remove is never called for it — Teardown must
// not infer "this looks like a worktree" from CWD being non-empty, or it
// would eventually delete a checkout the slot merely happened to run in
// rather than one this loop created.
func TestTeardownSkipsGitCheckWhenNoWorktree(t *testing.T) {
	dir := t.TempDir()
	git := newFakeGit()
	git.dirty[dir] = true // would fail the test if Teardown ever asked

	cfg := teardownConfig(t, SlotConfig{Name: "impl", Kind: "claude", CWD: dir})
	cfg.Git = git
	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, newModel())

	ctx := context.Background()
	if err := e.Spawn(ctx, "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := e.Teardown(ctx, "impl"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	if git.checkedCount() != 0 {
		t.Errorf("git status checked %d time(s) for a slot with no worktree — Worktree must gate the check, not CWD", git.checkedCount())
	}
	if len(client.removedWorktrees) != 0 {
		t.Error("worktree.remove called for a slot that never had a worktree created")
	}
	if len(client.closedPanes) != 1 {
		t.Error("a non-worktree slot must still have its pane closed")
	}
}

// A check that cannot be performed at all (git missing, path gone) must be
// treated identically to a dirty result: "we don't know" is never grounds
// to delete a working tree. It gets its own reason, distinct from
// ReasonWorktreeDirty, so a human reading the escalation goes looking for
// why the check failed rather than for changes to commit.
func TestTeardownEscalatesUnverifiableWorktree(t *testing.T) {
	dir := t.TempDir()
	git := newFakeGit()
	git.err = errors.New("git: not a worktree")

	cfg := teardownConfig(t, worktreeSlotConfig("impl", "claude", dir, "ws-impl"))
	cfg.Git = git
	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, newModel())

	ctx := context.Background()
	if err := e.Spawn(ctx, "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	err := e.Teardown(ctx, "impl")
	if err == nil {
		t.Fatal("Teardown returned nil for a worktree whose status could not be checked")
	}

	if len(client.removedWorktrees) != 0 {
		t.Fatalf("worktree.remove called despite an unverifiable status check: %v", client.removedWorktrees)
	}
	escs := e.takeEscalations()
	if len(escs) != 1 || escs[0].Reason != ReasonWorktreeUnverifiable {
		t.Fatalf("escalations = %+v, want exactly one with reason %q", escs, ReasonWorktreeUnverifiable)
	}
	if escs[0].Path != dir {
		t.Errorf("Path = %q, want %q", escs[0].Path, dir)
	}
}

// The kind concurrency token is released either way — a dirty tree that
// survives teardown must not leave the kind's gate consumed for the rest of
// the run, or a stuck slot silently caps every future spawn of its kind
// (§4.7). Proven the same way TestPerKindMaxConcurrentGatesSpawn proves the
// gate exists: a second slot of the same kind, under max_concurrent=1, must
// be admitted immediately after Teardown, not block.
func TestTeardownReleasesKindTokenRegardlessOfWorktreeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty bool
	}{
		{"dirty tree", true},
		{"clean tree", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			git := newFakeGit()
			if tc.dirty {
				git.dirty[dir] = true
			}

			cfg := teardownConfig(t,
				worktreeSlotConfig("a", "claude", dir, "ws-a"),
				SlotConfig{Name: "b", Kind: "claude"},
			)
			cfg.Git = git
			cfg.Kinds = map[string]KindConfig{"claude": {MaxConcurrent: 1}}
			client := &fakeHerdr{}
			e := newEngine(t, cfg, client, newModel())

			ctx := context.Background()
			if err := e.Spawn(ctx, "a"); err != nil {
				t.Fatalf("Spawn a: %v", err)
			}
			if err := e.Teardown(ctx, "a"); err != nil {
				t.Fatalf("Teardown a: %v", err)
			}

			spawnCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if err := e.Spawn(spawnCtx, "b"); err != nil {
				t.Fatalf("Spawn b: %v — the kind token from slot a was not released by Teardown", err)
			}
		})
	}
}

// Teardown is idempotent: manual teardown and TeardownOnFinish can both
// reach the same slot (an operator tearing one down by hand, then the loop
// finishing normally), and a second call must not double-remove a worktree,
// double-close a pane, or double-escalate a dirty one.
func TestTeardownIsIdempotent(t *testing.T) {
	t.Run("clean worktree removed once", func(t *testing.T) {
		dir := t.TempDir()
		git := newFakeGit()
		cfg := teardownConfig(t, worktreeSlotConfig("impl", "claude", dir, "ws-impl"))
		cfg.Git = git
		client := &fakeHerdr{}
		e := newEngine(t, cfg, client, newModel())

		ctx := context.Background()
		if err := e.Spawn(ctx, "impl"); err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		if err := e.Teardown(ctx, "impl"); err != nil {
			t.Fatalf("first Teardown: %v", err)
		}
		if err := e.Teardown(ctx, "impl"); err != nil {
			t.Fatalf("second Teardown: %v", err)
		}

		if len(client.removedWorktrees) != 1 {
			t.Errorf("removedWorktrees = %v, want exactly one call across two Teardown invocations", client.removedWorktrees)
		}
		if len(client.closedPanes) != 1 {
			t.Errorf("closedPanes = %v, want exactly one call across two Teardown invocations", client.closedPanes)
		}
	})

	t.Run("dirty worktree escalated once", func(t *testing.T) {
		dir := t.TempDir()
		git := newFakeGit()
		git.dirty[dir] = true
		cfg := teardownConfig(t, worktreeSlotConfig("impl", "claude", dir, "ws-impl"))
		cfg.Git = git
		client := &fakeHerdr{}
		e := newEngine(t, cfg, client, newModel())

		ctx := context.Background()
		if err := e.Spawn(ctx, "impl"); err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		if err := e.Teardown(ctx, "impl"); err != nil {
			t.Fatalf("first Teardown: %v", err)
		}
		if err := e.Teardown(ctx, "impl"); err != nil {
			t.Fatalf("second Teardown: %v", err)
		}

		if got := e.EscalationCount(); got != 1 {
			t.Errorf("EscalationCount = %d, want exactly one escalation across two Teardown invocations", got)
		}
		if len(client.removedWorktrees) != 0 {
			t.Error("a dirty worktree must never be removed, idempotent call or not")
		}
	})
}

// Each slot's own cwd is what gets checked — proven with two worktree slots
// at different paths, so a Teardown that accidentally checked the wrong
// slot's directory (or a shared/global path) would fail this rather than
// pass by coincidence.
func TestTeardownChecksEachSlotsOwnCWD(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	git := newFakeGit()

	cfg := teardownConfig(t,
		worktreeSlotConfig("a", "claude", dirA, "ws-a"),
		worktreeSlotConfig("b", "codex", dirB, "ws-b"),
	)
	cfg.Git = git
	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, newModel())

	ctx := context.Background()
	for _, slot := range []string{"a", "b"} {
		if err := e.Spawn(ctx, slot); err != nil {
			t.Fatalf("Spawn %s: %v", slot, err)
		}
	}
	for _, slot := range []string{"a", "b"} {
		if err := e.Teardown(ctx, slot); err != nil {
			t.Fatalf("Teardown %s: %v", slot, err)
		}
	}

	want := map[string]bool{dirA: true, dirB: true}
	got := map[string]bool{}
	for _, d := range git.checked {
		got[d] = true
	}
	for d := range want {
		if !got[d] {
			t.Errorf("git status never checked for %s", d)
		}
	}
	if len(git.checked) != 2 {
		t.Errorf("git status checked %d time(s), want exactly 2 (one per worktree slot)", len(git.checked))
	}
}

// TeardownOnFinish defaults off (Config's zero value), so a loop that
// finishes with no operator opting in leaves every pane and worktree
// exactly as they were — the surprised-inspector contract this option
// exists to protect.
func TestTeardownOnFinishDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	git := newFakeGit()
	git.dirty[dir] = true // would still be true if teardown ran — irrelevant here

	cfg := teardownConfig(t, worktreeSlotConfig("impl", "claude", dir, "ws-impl"))
	cfg.Git = git
	// cfg.TeardownOnFinish left at its zero value: false.
	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, newModel())

	ctx := context.Background()
	if err := e.Spawn(ctx, "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	out := run(t, e) // no transitions: the stream closes immediately

	if out.Reason != ReasonStreamClosed {
		t.Fatalf("Reason = %q, want %q", out.Reason, ReasonStreamClosed)
	}
	if len(client.closedPanes) != 0 {
		t.Errorf("closedPanes = %v, want none — TeardownOnFinish is off", client.closedPanes)
	}
	if len(client.removedWorktrees) != 0 {
		t.Errorf("removedWorktrees = %v, want none — TeardownOnFinish is off", client.removedWorktrees)
	}
	if git.checkedCount() != 0 {
		t.Errorf("git status checked %d time(s) with TeardownOnFinish off", git.checkedCount())
	}
}

// TeardownOnFinish, when set, tears every slot down when Run returns on its
// own, and — this is the ordering bug the design review caught — any
// escalation teardown raises (a dirty worktree, here) must appear in the
// Outcome Run actually returns, not merely in the engine's internal log.
// Draining escalations into the Outcome before running teardown would
// silently drop exactly the report §4.10 exists to guarantee a human sees.
func TestTeardownOnFinishTearsDownAllSlotsAndReportsEscalations(t *testing.T) {
	dir := t.TempDir()
	git := newFakeGit()
	git.dirty[dir] = true

	cfg := teardownConfig(t,
		worktreeSlotConfig("dirty", "claude", dir, "ws-dirty"),
		SlotConfig{Name: "plain", Kind: "codex"},
	)
	cfg.Git = git
	cfg.TeardownOnFinish = true
	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, newModel())

	ctx := context.Background()
	for _, slot := range []string{"dirty", "plain"} {
		if err := e.Spawn(ctx, slot); err != nil {
			t.Fatalf("Spawn %s: %v", slot, err)
		}
	}

	out := run(t, e) // no transitions: the stream closes immediately

	if out.Reason != ReasonStreamClosed {
		t.Fatalf("Reason = %q, want %q", out.Reason, ReasonStreamClosed)
	}
	if len(client.removedWorktrees) != 0 {
		t.Errorf("removedWorktrees = %v, want none — the only worktree slot was dirty", client.removedWorktrees)
	}
	if len(client.closedPanes) != 2 {
		t.Errorf("closedPanes = %v, want both slots' panes closed", client.closedPanes)
	}

	var found bool
	for _, esc := range out.Escalations {
		if esc.Slot == "dirty" && esc.Reason == ReasonWorktreeDirty && esc.Path == dir {
			found = true
		}
	}
	if !found {
		t.Fatalf("Outcome.Escalations = %+v, want the dirty-worktree escalation reported in the outcome Run returns, not just logged", out.Escalations)
	}
}

// A canceled run is not a finish: TeardownOnFinish must not fire when Run
// returns because the caller's ctx was canceled rather than because the
// loop actually converged, exhausted its budget, or its stream closed. An
// operator pulling the plug leaves everything up for inspection, same as if
// TeardownOnFinish were off.
func TestTeardownOnFinishSkipsOnCancellation(t *testing.T) {
	dir := t.TempDir()
	git := newFakeGit()

	cfg := teardownConfig(t, worktreeSlotConfig("impl", "claude", dir, "ws-impl"))
	cfg.Git = git
	cfg.TeardownOnFinish = true
	client := &fakeHerdr{}
	e := newEngine(t, cfg, client, newModel())

	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Spawn(ctx, "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	cancel()

	transitions := make(chan Transition)
	out, err := e.Run(ctx, transitions)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if out.Reason != ReasonCanceled {
		t.Fatalf("Reason = %q, want %q", out.Reason, ReasonCanceled)
	}
	if len(client.closedPanes) != 0 || len(client.removedWorktrees) != 0 {
		t.Errorf("teardown ran on a canceled run: closed=%v removed=%v", client.closedPanes, client.removedWorktrees)
	}
}

// New rejects a worktree slot missing the fields Teardown needs to act on it
// — a config error caught at construction is cheaper than one discovered as
// a teardown that silently can neither check nor remove anything.
func TestNewRejectsWorktreeSlotMissingCWDOrWorkspaceID(t *testing.T) {
	for _, sc := range []SlotConfig{
		{Name: "a", Kind: "claude", Worktree: true, WorkspaceID: "ws-a"},
		{Name: "a", Kind: "claude", Worktree: true, CWD: "/tmp/whatever"},
	} {
		cfg := teardownConfig(t, sc)
		if _, err := New(cfg, &fakeHerdr{}, newModel(), quietLogger()); err == nil {
			t.Errorf("New accepted a worktree slot with an empty cwd or workspace_id: %+v", sc)
		}
	}
}

// A worktree.remove failure is reported as an error from Teardown — this is
// a genuine operational failure, not a case where the invariant did its job,
// so it must not be swallowed into a silent success.
func TestTeardownReportsWorktreeRemoveFailure(t *testing.T) {
	dir := t.TempDir()
	git := newFakeGit()
	cfg := teardownConfig(t, worktreeSlotConfig("impl", "claude", dir, "ws-impl"))
	cfg.Git = git
	client := &fakeHerdr{removeErr: errors.New("herdr: worktree.remove: workspace busy")}
	e := newEngine(t, cfg, client, newModel())

	ctx := context.Background()
	if err := e.Spawn(ctx, "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := e.Teardown(ctx, "impl"); err == nil {
		t.Error("Teardown returned nil after worktree.remove failed")
	}
}
