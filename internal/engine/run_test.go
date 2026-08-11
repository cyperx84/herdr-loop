package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	herdr "github.com/cyperx84/herdr-api"
)

// The security property, and the reason it matters: run actions carry expanded
// template values, and those values may contain text an agent wrote. If argv
// went through a shell, agent-authored text would be executable. Passing argv
// directly means an expanded value is always exactly one argument, whatever
// characters it contains.
func TestRunDoesNotInterpretArgvThroughAShell(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "pwned")

	// Every one of these is a shell metacharacter sequence. Under sh -c they
	// would create the canary file; passed as argv they are literal text.
	for _, payload := range []string{
		"; touch " + canary,
		"$(touch " + canary + ")",
		"`touch " + canary + "`",
		"&& touch " + canary,
		"| touch " + canary,
	} {
		res := runCommand(context.Background(), RunAction{
			Argv: []string{"echo", payload},
		}, nil)
		if res.Err != nil {
			t.Fatalf("echo failed: %v", res.Err)
		}
		if !strings.Contains(res.Stdout, payload) {
			t.Errorf("payload was not passed through literally: stdout=%q payload=%q", res.Stdout, payload)
		}
		if _, err := os.Stat(canary); err == nil {
			t.Fatalf("SHELL INJECTION: payload %q executed and created %s", payload, canary)
		}
	}
}

// Exit codes are the whole point of a gate: they select the branch.
func TestRunReportsExitCode(t *testing.T) {
	ok := runCommand(context.Background(), RunAction{Argv: []string{"true"}}, nil)
	if !ok.Succeeded() || ok.ExitCode != 0 {
		t.Errorf("true: %+v", ok)
	}
	bad := runCommand(context.Background(), RunAction{Argv: []string{"false"}}, nil)
	if bad.Succeeded() {
		t.Error("false reported success")
	}
	if bad.ExitCode == 0 {
		t.Errorf("false: exit %d, want nonzero", bad.ExitCode)
	}
}

// A command that cannot be started is an environment fault, not a gate result.
// Branching on it would report "the tests failed" when the truth is the test
// runner is not installed.
func TestRunDistinguishesCannotStartFromFailed(t *testing.T) {
	res := runCommand(context.Background(), RunAction{
		Argv: []string{"herdr-loop-no-such-binary-exists"},
	}, nil)
	if res.Err == nil {
		t.Fatal("a missing binary reported no error — it would be mistaken for a failing gate")
	}
	if res.Succeeded() {
		t.Error("a command that could not start reported success")
	}
}

// A hung command must not wedge the loop, and a timeout must be
// distinguishable from a command that chose to exit nonzero.
func TestRunTimesOutAndSaysSo(t *testing.T) {
	start := time.Now()
	res := runCommand(context.Background(), RunAction{
		Argv:    []string{"sleep", "30"},
		Timeout: 200 * time.Millisecond,
	}, nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout did not fire: took %s", elapsed)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false after a deadline kill; a timeout reads as an ordinary failure without it")
	}
	if res.Succeeded() {
		t.Error("a timed-out command reported success")
	}
	if !strings.Contains(res.String(), "timed out") {
		t.Errorf("String() = %q, want it to say the command timed out", res.String())
	}
}

// Output feeds the branch's templates, so a failing gate can hand an agent the
// error it produced. Unbounded output would make that prompt useless and
// expensive, so it is capped from the tail — where the error is.
func TestRunCapturesOutputAndCapsIt(t *testing.T) {
	res := runCommand(context.Background(), RunAction{
		Argv: []string{"sh", "-c", "echo out; echo err >&2"},
	}, nil)
	if !strings.Contains(res.Stdout, "out") || !strings.Contains(res.Stderr, "err") {
		t.Errorf("streams not captured separately: %+v", res)
	}

	long := strings.Repeat("x", MaxRunOutput*2)
	got := tail(long, MaxRunOutput)
	if len(got) > MaxRunOutput+len("[output truncated]\n") {
		t.Errorf("tail returned %d bytes, want <= %d", len(got), MaxRunOutput)
	}
	if !strings.HasPrefix(got, "[output truncated]") {
		t.Error("truncation was silent; a caller cannot tell it is reading a fragment")
	}
	// Short output is returned whole, unmarked.
	if got := tail("short", MaxRunOutput); got != "short" {
		t.Errorf("tail mangled short output: %q", got)
	}
}

// runFacts is what makes {{stdout}} mean "the command that just ran".
func TestRunFactsExposeOutputToTemplates(t *testing.T) {
	f := runFacts(RunResult{ExitCode: 2, Stdout: "hello\n", Stderr: "boom\n", Duration: time.Second})
	if f["stdout"] != "hello" {
		t.Errorf("stdout = %q, want the trailing newline trimmed", f["stdout"])
	}
	if f["stderr"] != "boom" {
		t.Errorf("stderr = %q", f["stderr"])
	}
	if f["exit_code"] != "2" {
		t.Errorf("exit_code = %q", f["exit_code"])
	}
}

// The end-to-end gate: a failing command hands its output to an agent.
func TestRunActionFailureBranchPromptsWithOutput(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)

	cfg := baseConfig(t)
	cfg.Rules = []Rule{{
		Name: "gate",
		When: Predicate{Op: OpEq, Field: "slot", Value: "impl"},
		Then: Action{Run: &RunAction{
			Argv:      []string{"sh", "-c", "echo BUILD_BROKEN >&2; exit 1"},
			OnFailure: &Action{Prompt: &PromptAction{Slot: "review", Text: "fix this: {{stderr}}"}},
		}},
	}}
	e := newEngine(t, cfg, client, model)
	run(t, e, Transition{Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusDone, At: time.Now()})

	client.mu.Lock()
	prompts := append([]promptCall(nil), client.prompts...)
	client.mu.Unlock()

	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1 — the failure branch did not fire", len(prompts))
	}
	if !strings.Contains(prompts[0].text, "BUILD_BROKEN") {
		t.Errorf("prompt did not carry the command's output: %q", prompts[0].text)
	}
}

// A passing gate ends the loop, from a goroutine, long after fire returned.
func TestRunActionSuccessBranchCanFinishTheLoop(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)

	cfg := baseConfig(t)
	cfg.Rules = []Rule{{
		Name: "gate",
		When: Predicate{Op: OpEq, Field: "slot", Value: "impl"},
		Then: Action{Run: &RunAction{
			Argv:      []string{"true"},
			OnSuccess: &Action{Finish: &FinishAction{Reason: "green"}},
		}},
	}}
	e := newEngine(t, cfg, client, model)
	out := run(t, e, Transition{Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusDone, At: time.Now()})

	if out.Reason != "green" {
		t.Errorf("outcome reason = %q, want green — an async branch could not end the loop", out.Reason)
	}
}
