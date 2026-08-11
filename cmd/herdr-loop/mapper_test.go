package main

import (
	"strings"
	"testing"

	"github.com/cyperx84/herdr-loop/internal/engine"
	"github.com/cyperx84/herdr-loop/internal/manifest"
)

// noRunManifest is PLAN.md §3's own worked example with its "escape hatch"
// run rule removed, so it exercises everything mapManifest does support:
// worktree slots, an initial prompt, all/eq/in predicates, prompt and finish
// actions.
const noRunManifest = `
[loop]
name           = "impl-review-verify"
max_iterations = 10
handoff_dir    = ".herdr-loop/handoff"
on_blocked     = "escalate"

[[slot]]
name     = "impl"
kind     = "claude"
worktree = { branch = "loop/impl", base = "main" }
prompt   = "Implement {{task}}. Write your result to $HERDR_LOOP_HANDOFF."

[[slot]]
name     = "review"
kind     = "codex"
worktree = { branch = "loop/review", base = "main" }

[[rule]]
when = { op = "all", filters = [
  { op = "eq", field = "slot",   value = "impl" },
  { op = "in", field = "status", values = ["idle", "done"] },
] }
then = { prompt = { slot = "review", text = "Review {{impl.handoff}}. Findings only." } }

[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "clean" }
then = { finish = "converged" }
`

func mustParse(t *testing.T, src string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}
	return m
}

// TestMapManifestSupportedShape asserts the invariants a caller (run,
// validate, doctor) actually relies on, not a frozen snapshot of the output:
// slot/rule counts match the input, worktree slots are collected for
// resolveWorktrees rather than given a bogus cwd, and the initial prompt
// lands on the right slot.
func TestMapManifestSupportedShape(t *testing.T) {
	res, err := mapManifest(mustParse(t, noRunManifest))
	if err != nil {
		t.Fatalf("mapManifest: %v", err)
	}

	if got, want := len(res.Config.Slots), 2; got != want {
		t.Errorf("len(Slots) = %d, want %d", got, want)
	}
	if got, want := len(res.Config.Rules), 2; got != want {
		t.Errorf("len(Rules) = %d, want %d", got, want)
	}
	if got, want := len(res.Worktrees), 2; got != want {
		t.Fatalf("len(Worktrees) = %d, want %d", got, want)
	}
	for _, wt := range res.Worktrees {
		if res.Config.Slots[wt.SlotIndex].CWD != "" {
			t.Errorf("slot %q: worktree slot got a non-empty cwd from mapManifest, want empty until resolveWorktrees runs",
				res.Config.Slots[wt.SlotIndex].Name)
		}
	}

	if got := res.InitialPrompts["impl"]; !strings.Contains(got, "Implement") {
		t.Errorf("InitialPrompts[impl] = %q, want the manifest's prompt text", got)
	}
	if _, has := res.InitialPrompts["review"]; has {
		t.Error("InitialPrompts[review] set, but that slot has no prompt in the manifest")
	}
}

// TestMapManifestRuleNamesAreDeterministic is the property escalations
// depend on (PLAN §4.11: every escalation names the rule that caused it) —
// mapping the same manifest text twice must produce the same names, and two
// different rules in one manifest must never collide.
func TestMapManifestRuleNamesAreDeterministic(t *testing.T) {
	m := mustParse(t, noRunManifest)

	first, err := mapManifest(m)
	if err != nil {
		t.Fatalf("mapManifest (first): %v", err)
	}
	second, err := mapManifest(m)
	if err != nil {
		t.Fatalf("mapManifest (second): %v", err)
	}

	if len(first.Config.Rules) != len(second.Config.Rules) {
		t.Fatalf("rule count differs between calls: %d vs %d", len(first.Config.Rules), len(second.Config.Rules))
	}
	seen := map[string]bool{}
	for i, r := range first.Config.Rules {
		if r.Name != second.Config.Rules[i].Name {
			t.Errorf("rule %d name not deterministic: %q vs %q", i, r.Name, second.Config.Rules[i].Name)
		}
		if r.Name == "" {
			t.Errorf("rule %d has an empty name", i)
		}
		if seen[r.Name] {
			t.Errorf("rule name %q is not unique within one manifest", r.Name)
		}
		seen[r.Name] = true
	}
}

// TestMapManifestRejectsRunAction is the landmine PLAN §3's own example
// walks straight into: its "escape hatch" rule uses a run action, which
// engine.Action has no field for. mapManifest must refuse it loudly rather
// than silently dropping the rule — a manifest author who wrote a run rule
// and had it vanish with no error is worse off than one who gets told it
// isn't supported yet.
func TestMapManifestConvertsRunActionWithBranches(t *testing.T) {
	const src = `
[loop]
name = "gate"
max_iterations = 5

[[slot]]
name = "impl"
kind = "opencode"
cwd = "/tmp/impl"

[[rule]]
when = { op = "eq", field = "slot", value = "impl" }
then = { run = ["go", "test", "./..."], on_success = { finish = "green" }, on_failure = { prompt = { slot = "impl", text = "Tests fail:\n{{stdout}}" } } }
`
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res, err := mapManifest(m)
	if err != nil {
		t.Fatalf("mapManifest: %v", err)
	}
	if len(res.Config.Rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(res.Config.Rules))
	}
	run := res.Config.Rules[0].Then.Run
	if run == nil {
		t.Fatal("run action was not converted")
	}
	if got := strings.Join(run.Argv, " "); got != "go test ./..." {
		t.Errorf("argv = %q", got)
	}
	if run.OnSuccess == nil || run.OnSuccess.Finish == nil || run.OnSuccess.Finish.Reason != "green" {
		t.Errorf("on_success branch lost: %+v", run.OnSuccess)
	}
	if run.OnFailure == nil || run.OnFailure.Prompt == nil || run.OnFailure.Prompt.Slot != "impl" {
		t.Errorf("on_failure branch lost: %+v", run.OnFailure)
	}
	// The failure branch must carry the template through unexpanded — it is
	// expanded at fire time, against the command's actual output.
	if !strings.Contains(run.OnFailure.Prompt.Text, "{{stdout}}") {
		t.Errorf("on_failure text lost its template: %q", run.OnFailure.Prompt.Text)
	}
}

// A gate with no branches is legitimate: the rule exists to stop the loop
// advancing, and nothing further is wanted on either outcome.
func TestMapManifestAcceptsBareRunAction(t *testing.T) {
	const src = `
[loop]
name = "gate"
max_iterations = 5

[[slot]]
name = "impl"
kind = "opencode"
cwd = "/tmp/impl"

[[rule]]
when = { op = "eq", field = "slot", value = "impl" }
then = { run = ["true"] }
`
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res, err := mapManifest(m)
	if err != nil {
		t.Fatalf("mapManifest: %v", err)
	}
	run := res.Config.Rules[0].Then.Run
	if run == nil || len(run.Argv) != 1 || run.Argv[0] != "true" {
		t.Fatalf("bare run action not converted: %+v", run)
	}
	if run.OnSuccess != nil || run.OnFailure != nil {
		t.Error("branches invented where the manifest declared none")
	}
}

func TestMapManifestRejectsEscalateAction(t *testing.T) {
	const withEscalate = noRunManifest + `
[[rule]]
when = { op = "exists", field = "review.handoff" }
then = { escalate = true }
`
	_, err := mapManifest(mustParse(t, withEscalate))
	if err == nil {
		t.Fatal("mapManifest: want an error for an escalate action, got nil")
	}
}

// TestMapManifestDefaultsMaxIterations covers the one place manifest and
// engine disagree on what a zero value means: manifest.Parse accepts
// max_iterations = 0 for an acyclic manifest, engine.New rejects it outright.
func TestMapManifestDefaultsMaxIterations(t *testing.T) {
	const acyclic = `
[loop]
name = "single-shot"

[[slot]]
name = "solo"
kind = "claude"
cwd  = "/tmp/solo"

[[rule]]
when = { op = "eq", field = "status", value = "idle" }
then = { finish = "done" }
`
	res, err := mapManifest(mustParse(t, acyclic))
	if err != nil {
		t.Fatalf("mapManifest: %v", err)
	}
	if res.Config.MaxIterations <= 0 {
		t.Errorf("MaxIterations = %d, want a positive default (engine.New rejects <= 0)", res.Config.MaxIterations)
	}

	// The mapped config must itself satisfy engine.New — that is the actual
	// contract this default exists for, not just "the field is non-zero".
	if _, err := engine.New(res.Config, noopHerdr{}, noopModel{}, discardLogger()); err != nil {
		t.Errorf("engine.New rejected the mapped config: %v", err)
	}
}

// TestConvertPredicateStringifiesScalars covers the one real typing
// difference between the two Predicate shapes: manifest's Value/Values are
// `any` (TOML decodes bool/int/float/string distinctly), engine's are
// string-only. A manifest author writing `value = true` must compare equal
// to a handoff field whose front-matter rendered the same value as text.
func TestConvertPredicateStringifiesScalars(t *testing.T) {
	p := manifest.Predicate{Op: "eq", Field: "x", Value: true}
	got := convertPredicate(p)
	if got.Value != "true" {
		t.Errorf("convertPredicate(bool true).Value = %q, want \"true\"", got.Value)
	}

	p = manifest.Predicate{Op: "in", Field: "x", Values: []any{int64(1), "two", false}}
	got = convertPredicate(p)
	want := []string{"1", "two", "false"}
	if len(got.Values) != len(want) {
		t.Fatalf("len(Values) = %d, want %d", len(got.Values), len(want))
	}
	for i := range want {
		if got.Values[i] != want[i] {
			t.Errorf("Values[%d] = %q, want %q", i, got.Values[i], want[i])
		}
	}
}

// TestConvertPredicateNotUsesSingleFilter covers the shape difference on
// "not": manifest carries a single named Filter, engine expects a
// one-element Filters slice (see engine.validatePredicate's OpNot case).
func TestConvertPredicateNotUsesSingleFilter(t *testing.T) {
	inner := manifest.Predicate{Op: "eq", Field: "status", Value: "blocked"}
	p := manifest.Predicate{Op: "not", Filter: &inner}
	got := convertPredicate(p)
	if len(got.Filters) != 1 {
		t.Fatalf("len(Filters) = %d, want 1", len(got.Filters))
	}
	if got.Filters[0].Field != "status" || got.Filters[0].Value != "blocked" {
		t.Errorf("Filters[0] = %+v, want the inner predicate copied through", got.Filters[0])
	}
}

// TestConvertBlockedRuleSplitsKeys covers the Send-to-Keys convention: a
// space-separated string becomes one agent.send_keys token per field.
func TestConvertBlockedRuleSplitsKeys(t *testing.T) {
	br := manifest.BlockedRule{Pattern: "Overwrite existing file?", Send: "y enter"}
	got := convertBlockedRule(br)
	if got.Prompt != br.Pattern {
		t.Errorf("Prompt = %q, want %q", got.Prompt, br.Pattern)
	}
	want := []string{"y", "enter"}
	if len(got.Keys) != len(want) {
		t.Fatalf("Keys = %v, want %v", got.Keys, want)
	}
	for i := range want {
		if got.Keys[i] != want[i] {
			t.Errorf("Keys[%d] = %q, want %q", i, got.Keys[i], want[i])
		}
	}
}
