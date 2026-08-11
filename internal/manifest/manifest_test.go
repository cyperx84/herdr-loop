package manifest

import (
	"strings"
	"testing"
)

// planExample is the manifest sketch from PLAN.md §3, verbatim. It is the
// contract: if this stops parsing, the plan and the parser have drifted.
const planExample = `
[loop]
name            = "impl-review-verify"
max_iterations  = 10
handoff_dir     = ".herdr-loop/handoff"
on_blocked      = "escalate"

[[slot]]
name     = "impl"
kind     = "claude"
worktree = { branch = "loop/impl", base = "main" }
prompt   = "Implement {{task}}. Write your result to $HERDR_LOOP_HANDOFF."

[[slot]]
name     = "review"
kind     = "codex"
worktree = { branch = "loop/review", base = "main" }

[[slot]]
name     = "verify"
kind     = "opencode"
worktree = { branch = "loop/verify", base = "main" }

[[rule]]
when = { op = "all", filters = [
  { op = "eq", field = "slot",   value = "impl" },
  { op = "in", field = "status", values = ["idle", "done"] },
] }
then = { prompt = { slot = "review", text = "Review {{impl.handoff}}. Findings only." } }

[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "clean" }
then = { finish = "converged" }

[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "changes-requested" }
then = { prompt = { slot = "impl", text = "Address {{review.handoff}}" } }

[[rule]]
when = { op = "eq", field = "slot.verify.status", value = "done" }
then = { run = ["cargo", "test"], on_success = { finish = "green" }, on_failure = { prompt = { slot = "impl", text = "Tests fail:\n{{stdout}}" } } }
`

func TestParsePlanExample(t *testing.T) {
	m, err := Parse([]byte(planExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Loop.Name != "impl-review-verify" {
		t.Errorf("Loop.Name = %q, want impl-review-verify", m.Loop.Name)
	}
	if m.Loop.MaxIterations != 10 {
		t.Errorf("Loop.MaxIterations = %d, want 10", m.Loop.MaxIterations)
	}
	if len(m.Slots) != 3 {
		t.Fatalf("len(Slots) = %d, want 3", len(m.Slots))
	}
	if len(m.Rules) != 4 {
		t.Fatalf("len(Rules) = %d, want 4", len(m.Rules))
	}
	if m.Rules[0].Then.Prompt.Slot != "review" {
		t.Errorf("rule 0 prompt slot = %q, want review", m.Rules[0].Then.Prompt.Slot)
	}
}

// minimalOK is the smallest manifest that should pass every validator: one
// slot, no rules, so there is no cycle, no shared cwd, and nothing to
// reference. Used as a base that individual tests mutate.
const minimalOKTemplate = `
[loop]
name = "t"
handoff_dir = ".handoff"

[[slot]]
name = "a"
cwd  = "/repo/a"
`

func TestParseMinimal(t *testing.T) {
	if _, err := Parse([]byte(minimalOKTemplate)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string // substring expected in the error
	}{
		{
			name: "duplicate slot name",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[slot]]
name = "a"
cwd  = "/y"
`,
			wantErr: "duplicate slot name",
		},
		{
			name: "slot with neither cwd nor worktree",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
`,
			wantErr: "must set exactly one of cwd or worktree",
		},
		{
			name: "slot with both cwd and worktree",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"
worktree = { branch = "b" }
`,
			wantErr: "must set exactly one of cwd or worktree",
		},
		{
			name: "worktree missing branch",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
worktree = { base = "main" }
`,
			wantErr: "worktree.branch is required",
		},
		{
			name: "shared cwd without allow_shared_cwd",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/repo"

[[slot]]
name = "b"
cwd  = "/repo"
`,
			wantErr: `share cwd "/repo"`,
		},
		{
			name: "on_blocked auto with empty whitelist",
			toml: `
[loop]
name       = "t"
on_blocked = "auto"

[[slot]]
name = "a"
cwd  = "/x"
`,
			wantErr: "non-empty [[blocked_rule]] whitelist",
		},
		{
			name: "on_blocked auto with wildcard pattern",
			toml: `
[loop]
name       = "t"
on_blocked = "auto"

[[slot]]
name = "a"
cwd  = "/x"

[[blocked_rule]]
pattern = "Overwrite *"
send    = "y"
`,
			wantErr: "contains a wildcard",
		},
		{
			name: "on_blocked invalid value",
			toml: `
[loop]
name       = "t"
on_blocked = "yolo"

[[slot]]
name = "a"
cwd  = "/x"
`,
			wantErr: "must be escalate, pause, or auto",
		},
		{
			name: "prompt references undefined slot",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when = { op = "exists", field = "a.status" }
then = { prompt = { slot = "ghost", text = "hi" } }
`,
			wantErr: `slot "ghost" is not defined`,
		},
		{
			name: "predicate references undefined slot via field=slot eq",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when = { op = "eq", field = "slot", value = "ghost" }
then = { finish = "done" }
`,
			wantErr: `slot "ghost" is not defined`,
		},
		{
			name: "predicate references undefined slot via field=slot in",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when = { op = "in", field = "slot", values = ["a", "ghost"] }
then = { finish = "done" }
`,
			wantErr: `slot "ghost" is not defined`,
		},
		{
			name: "unknown predicate op",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when = { op = "maybe", field = "a.status" }
then = { finish = "done" }
`,
			wantErr: `unknown op "maybe"`,
		},
		{
			name: "action with zero variants",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when = { op = "exists", field = "a.status" }
then = {}
`,
			wantErr: "exactly one of prompt/run/finish/escalate, got 0",
		},
		{
			name: "action with two variants",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when = { op = "exists", field = "a.status" }
then = { finish = "done", escalate = true }
`,
			wantErr: "exactly one of prompt/run/finish/escalate, got 2",
		},
		{
			name: "on_success without run",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when = { op = "exists", field = "a.status" }
then = { finish = "done", on_success = { finish = "also-done" } }
`,
			wantErr: "on_success/on_failure only apply to run",
		},
		{
			name: "rule cycle with no cap",
			toml: `
[loop]
name = "t"

[[slot]]
name = "impl"
cwd  = "/impl"

[[slot]]
name = "review"
cwd  = "/review"

[[rule]]
when = { op = "eq", field = "slot", value = "impl" }
then = { prompt = { slot = "review", text = "go" } }

[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "changes-requested" }
then = { prompt = { slot = "impl", text = "go" } }
`,
			wantErr: "no retry cap",
		},
		{
			name: "self-loop with no cap",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when = { op = "eq", field = "a.status", value = "blocked" }
then = { prompt = { slot = "a", text = "retry" } }
`,
			wantErr: "no retry cap",
		},
		{
			name: "bad timeout duration",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when    = { op = "exists", field = "a.status" }
then    = { finish = "done" }
timeout = "not-a-duration"
`,
			wantErr: "timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.toml))
			if err == nil {
				t.Fatalf("Parse: want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Parse error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestCycleAcceptedCases is the acyclic/capped counterpart to the cycle
// rejections above: every case here must parse cleanly. Pairing them in one
// table makes clear the detector distinguishes a real cycle from a manifest
// that merely looks superficially similar (chain, shared target, capped
// cycle).
func TestCycleAcceptedCases(t *testing.T) {
	tests := []struct {
		name string
		toml string
	}{
		{
			name: "acyclic chain needs no cap",
			toml: `
[loop]
name = "t"

[[slot]]
name = "impl"
cwd  = "/impl"

[[slot]]
name = "review"
cwd  = "/review"

[[slot]]
name = "verify"
cwd  = "/verify"

[[rule]]
when = { op = "eq", field = "slot", value = "impl" }
then = { prompt = { slot = "review", text = "go" } }

[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "clean" }
then = { prompt = { slot = "verify", text = "go" } }
`,
		},
		{
			name: "cycle covered by loop.max_iterations",
			toml: `
[loop]
name           = "t"
max_iterations = 5

[[slot]]
name = "impl"
cwd  = "/impl"

[[slot]]
name = "review"
cwd  = "/review"

[[rule]]
when = { op = "eq", field = "slot", value = "impl" }
then = { prompt = { slot = "review", text = "go" } }

[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "changes-requested" }
then = { prompt = { slot = "impl", text = "go" } }
`,
		},
		{
			name: "cycle covered by per-rule timeout on every participating rule",
			toml: `
[loop]
name = "t"

[[slot]]
name = "impl"
cwd  = "/impl"

[[slot]]
name = "review"
cwd  = "/review"

[[rule]]
when    = { op = "eq", field = "slot", value = "impl" }
then    = { prompt = { slot = "review", text = "go" } }
timeout = "1h"

[[rule]]
when    = { op = "eq", field = "review.handoff.verdict", value = "changes-requested" }
then    = { prompt = { slot = "impl", text = "go" } }
timeout = "1h"
`,
		},
		{
			name: "two independent slots, no rules at all",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/a"

[[slot]]
name = "b"
cwd  = "/b"
`,
		},
		{
			name: "shared cwd explicitly allowed",
			toml: `
[loop]
name             = "t"
allow_shared_cwd = true

[[slot]]
name = "a"
cwd  = "/repo"

[[slot]]
name = "b"
cwd  = "/repo"
`,
		},
		{
			name: "escalate action",
			toml: `
[loop]
name = "t"

[[slot]]
name = "a"
cwd  = "/x"

[[rule]]
when = { op = "eq", field = "a.status", value = "blocked" }
then = { escalate = true }
`,
		},
		{
			name: "on_blocked auto with a valid exact whitelist",
			toml: `
[loop]
name       = "t"
on_blocked = "auto"

[[slot]]
name = "a"
cwd  = "/x"

[[blocked_rule]]
pattern = "Overwrite existing file?"
send    = "y"
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.toml)); err != nil {
				t.Fatalf("Parse: unexpected error: %v", err)
			}
		})
	}
}

// TestCycleCoveredByOnlyPartOfCycleTimeout checks the detector is not
// satisfied by a partial cap: if only one of the two rules forming a cycle
// has a timeout, the cycle is still unbounded (the other rule can retrigger
// indefinitely on its own schedule), so this must still fail.
func TestCycleCoveredByOnlyPartOfCycleTimeout(t *testing.T) {
	const toml = `
[loop]
name = "t"

[[slot]]
name = "impl"
cwd  = "/impl"

[[slot]]
name = "review"
cwd  = "/review"

[[rule]]
when    = { op = "eq", field = "slot", value = "impl" }
then    = { prompt = { slot = "review", text = "go" } }
timeout = "1h"

[[rule]]
when = { op = "eq", field = "review.handoff.verdict", value = "changes-requested" }
then = { prompt = { slot = "impl", text = "go" } }
`
	_, err := Parse([]byte(toml))
	if err == nil {
		t.Fatal("Parse: want error, got nil (partial timeout coverage must not satisfy the cap requirement)")
	}
	if !strings.Contains(err.Error(), "no retry cap") {
		t.Fatalf("Parse error = %q, want it to mention the missing retry cap", err.Error())
	}
}

// strict must survive parsing: it is the difference between a loop that
// refuses to act on a heuristic status and one that does not (PLAN §4.6).
func TestParseStrictFlag(t *testing.T) {
	const src = `
[loop]
name = "s"
max_iterations = 3
strict = true

[[slot]]
name = "a"
kind = "opencode"
cwd = "/tmp/a"
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !m.Loop.Strict {
		t.Error("strict = false after parsing strict = true")
	}

	// Absent means off, and that default is deliberate: strict would exclude
	// every screen-classified kind, which today is most of them.
	m2, err := Parse([]byte("[loop]\nname = \"s\"\nmax_iterations = 3\n\n[[slot]]\nname = \"a\"\nkind = \"opencode\"\ncwd = \"/tmp/a\"\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m2.Loop.Strict {
		t.Error("strict defaulted to true; it must be opt-in")
	}
}
