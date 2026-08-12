package engine

import (
	"context"
	"errors"
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

// A harness can silently reinterpret the job: Claude Code in plan mode turns
// "implement this" into "write a plan for this", reports working, reports
// done, and changes nothing — so a gate fails forever on work never attempted.
// Startup keys exist to prevent that, and must actually reach the pane.
func TestStartupKeysAreSentBeforeTheFirstPrompt(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel()

	cfg := baseConfig(t)
	cfg.Kinds = map[string]KindConfig{
		"claude": {MaxConcurrent: 1, StartupKeys: []string{`\e[Z`}, StartupSettle: time.Millisecond},
	}
	e := newEngine(t, cfg, client, model)

	if err := e.Spawn(context.Background(), "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	client.mu.Lock()
	sent := append([]string(nil), client.sentText...)
	client.mu.Unlock()

	if len(sent) != 1 {
		t.Fatalf("sent %d text sequences, want 1: %q", len(sent), sent)
	}
	// The literal escape byte must reach the pane, not the backslash-e spelling
	// the TOML file carries.
	if sent[0] != "\x1b[Z" {
		t.Errorf("sent %q, want the decoded S-Tab escape \\x1b[Z — herdr rejects the key name, only the raw sequence works", sent[0])
	}
}

// A kind with no measured startup sequence must be left alone, not sent
// something invented.
func TestNoStartupKeysForUnmeasuredKind(t *testing.T) {
	client := &fakeHerdr{}
	e := newEngine(t, baseConfig(t), client, newModel())

	if err := e.Spawn(context.Background(), "impl"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	client.mu.Lock()
	n := len(client.sentText)
	client.mu.Unlock()
	if n != 0 {
		t.Errorf("sent %d sequences to a kind with none configured", n)
	}
}

// Losing a cosmetic keystroke must not fail a spawn: an agent that ignored the
// sequence is no worse off than one never sent it.
func TestStartupKeyFailureDoesNotFailTheSpawn(t *testing.T) {
	client := &fakeHerdr{sendTextErr: errors.New("herdr: pane busy")}
	cfg := baseConfig(t)
	cfg.Kinds = map[string]KindConfig{
		"claude": {StartupKeys: []string{`\e[Z`}, StartupSettle: time.Millisecond},
	}
	e := newEngine(t, cfg, client, newModel())

	if err := e.Spawn(context.Background(), "impl"); err != nil {
		t.Errorf("Spawn failed over an undelivered startup key: %v", err)
	}
}

// The TOML spelling must survive the trip to the wire.
func TestDecodeEscapes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`\e[Z`, "\x1b[Z"},
		{`\x1b[Z`, "\x1b[Z"},
		{`plain`, "plain"},
		{`a\tb`, "a\tb"},
	} {
		if got := decodeEscapes(c.in); got != c.want {
			t.Errorf("decodeEscapes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A freshly spawned slot is idle, and idle satisfies "settled" — so a rule
// keyed on "when this slot has finished" matched a slot that had never
// started. Seen in a two-slot loop: the reviewer's gate ran before the
// implementer was given its task, failed on untouched code, and sent that
// failure back as feedback about work nobody had done.
func TestRulesDoNotFireForASlotThatHasNeverWorked(t *testing.T) {
	client := &fakeHerdr{}
	// Both slots idle, as they are the moment they spawn.
	model := newModel().set("impl", herdr.StatusIdle).set("review", herdr.StatusIdle)
	e := newEngine(t, baseConfig(t), client, model)

	// A transition that is NOT out of working — exactly what spawning
	// produces.
	run(t, e, Transition{Slot: "impl", From: herdr.StatusUnknown, To: herdr.StatusIdle, At: time.Now()})

	if n := client.promptCount(); n != 0 {
		t.Fatalf("prompts = %d, want 0 — a slot that never worked has not finished anything", n)
	}
}

// The same slot, once it has actually completed a turn, must fire normally.
func TestRulesFireAfterATurnCompletes(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)
	e := newEngine(t, baseConfig(t), client, model)

	run(t, e, Transition{Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusDone, At: time.Now()})

	if n := client.promptCount(); n != 1 {
		t.Fatalf("prompts = %d, want 1 — leaving working is a completed turn", n)
	}
}

// A turn is recorded even when the transition that carried it is ignored for
// another reason, so the slot is eligible next time round.
func TestTurnIsRecordedEvenWhenTheTransitionIsIgnored(t *testing.T) {
	client := &fakeHerdr{}
	model := newModel().set("impl", herdr.StatusUnknown).set("review", herdr.StatusIdle)
	e := newEngine(t, baseConfig(t), client, model)

	// working -> unknown: a real turn ended, but unknown is not actionable.
	run(t, e, Transition{Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusUnknown, At: time.Now()})

	if !e.hasWorked("impl") {
		t.Error("turn not recorded because the transition was unactionable; the slot would stay ineligible forever")
	}
}

// herdr declares a prompt stalled when it sees no lifecycle change within its
// own 5s window. That window is shorter than some harnesses take to start, so
// a cold pane reports stalled while the prompt in fact landed — reproduced on
// consecutive runs against Claude Code. Failing the action there aborts work
// that is already underway, and re-sending double-prompts the agent.
func TestStalledPromptIsCorroboratedBeforeFailing(t *testing.T) {
	client := &fakeHerdr{
		promptErr: &herdr.APIError{Code: "agent_prompt_stalled", Message: "no observed state change within 5000 ms"},
		// herdr said stalled, but the agent is in fact working.
		agentGetStatus: herdr.StatusWorking,
	}
	model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)
	e := newEngine(t, baseConfig(t), client, model)

	out := run(t, e, Transition{Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusDone, At: time.Now()})

	if len(out.Escalations) != 0 {
		t.Fatalf("escalated on a false stall: %v — the agent was working, the prompt landed", out.Escalations)
	}
}

// A prompt that genuinely did not land must still fail. The corroboration is
// there to catch herdr's short window, not to paper over a lost prompt.
func TestStalledPromptStillFailsWhenTheAgentNeverStarts(t *testing.T) {
	client := &fakeHerdr{
		promptErr:      &herdr.APIError{Code: "agent_prompt_stalled", Message: "no observed state change within 5000 ms"},
		agentGetStatus: herdr.StatusIdle, // never started
	}
	model := newModel().set("impl", herdr.StatusDone).set("review", herdr.StatusIdle)
	cfg := baseConfig(t)
	e := newEngine(t, cfg, client, model)

	out := run(t, e, Transition{Slot: "impl", From: herdr.StatusWorking, To: herdr.StatusDone, At: time.Now()})

	if len(out.Escalations) == 0 {
		t.Fatal("a prompt that never landed was treated as delivered")
	}
}
