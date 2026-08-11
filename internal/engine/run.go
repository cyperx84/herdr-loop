package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultRunTimeout bounds a run action that never returns.
//
// A rule engine that blocks forever on a hung command is worse than one that
// gives up: the loop stops making progress with no signal, which is the
// silent-stall failure this project exists to avoid. Manifests can raise it
// per action; they cannot disable it.
const DefaultRunTimeout = 10 * time.Minute

// MaxRunOutput caps how much of a command's output is captured.
//
// Output becomes template data for the branch action, and a prompt built from
// an unbounded build log would be both useless and expensive. The tail is kept
// rather than the head: the end of a failing command's output is where the
// error is.
const MaxRunOutput = 64 * 1024

// RunAction shells out and branches on the exit code.
//
// This is the escape hatch from PLAN.md §3: predicates answer questions about
// agent state, and a mechanical gate — does it build, do the tests pass — is a
// question no predicate over agent status can answer.
//
// # No shell
//
// Argv is executed directly, never through sh -c. This is a security boundary,
// not a style choice: run actions carry expanded template values (a slot's
// handoff text, a command's previous output), and any of those may contain
// text an agent wrote. Passing them through a shell would make agent-authored
// text executable. Executing argv directly means an expanded value is always
// exactly one argument, whatever characters it contains.
//
// Shell features are therefore unavailable by design — no pipes, globs, or
// redirection. A manifest wanting them must call a script that has them.
type RunAction struct {
	// Argv is the command and its arguments. Argv[0] is resolved on PATH.
	Argv []string
	// CWD is where the command runs. Empty means the supervisor's directory.
	CWD string
	// Timeout overrides DefaultRunTimeout. Zero means the default.
	Timeout time.Duration
	// Env adds to the inherited environment.
	Env map[string]string
	// OnSuccess fires when the command exits 0, OnFailure otherwise. Either
	// may be nil, which makes that branch a no-op — a run action used purely
	// as a gate is legitimate.
	OnSuccess *Action
	OnFailure *Action
}

// RunResult is what a finished command reports to its branch action.
type RunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	// TimedOut distinguishes a command killed by the deadline from one that
	// chose to exit nonzero. They look identical in ExitCode and mean very
	// different things.
	TimedOut bool
	// Err is set when the command could not be run at all — Argv[0] not found,
	// CWD missing, permission denied. Distinct from a command that ran and
	// failed, which is an ordinary result reported through ExitCode.
	Err error
}

// Succeeded reports whether the OnSuccess branch should fire.
//
// Only a clean exit 0 counts. A timeout is not success even if the process
// happened to be killed at the right moment, and a command that could not be
// started is not success either.
func (r RunResult) Succeeded() bool { return r.Err == nil && !r.TimedOut && r.ExitCode == 0 }

// String renders the result for an escalation or log line.
func (r RunResult) String() string {
	switch {
	case r.Err != nil:
		return fmt.Sprintf("could not run: %v", r.Err)
	case r.TimedOut:
		return fmt.Sprintf("timed out after %s", r.Duration.Round(time.Millisecond))
	default:
		return fmt.Sprintf("exit %d in %s", r.ExitCode, r.Duration.Round(time.Millisecond))
	}
}

// runCommand executes a RunAction and reports what happened.
//
// It never returns an error: a command that fails is a result to branch on,
// not an error to propagate. The only errors are recorded in RunResult.Err,
// which the caller treats as failure.
func runCommand(ctx context.Context, a RunAction, extraEnv map[string]string) RunResult {
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// exec.CommandContext, not sh -c. See RunAction's doc: expanded template
	// values may contain agent-authored text, and a shell would make it
	// executable.
	cmd := exec.CommandContext(ctx, a.Argv[0], a.Argv[1:]...)
	cmd.Dir = a.CWD

	env := os.Environ()
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	for k, v := range a.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	res := RunResult{
		Stdout:   tail(stdout.String(), MaxRunOutput),
		Stderr:   tail(stderr.String(), MaxRunOutput),
		Duration: elapsed,
	}

	// A deadline kill surfaces as an ExitError, so check the context rather
	// than trying to recognise the signal.
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res
	}

	switch e := err.(type) {
	case nil:
		res.ExitCode = 0
	case *exec.ExitError:
		res.ExitCode = e.ExitCode()
	default:
		// Could not start: binary missing, cwd gone, not executable.
		res.Err = err
		res.ExitCode = -1
	}
	return res
}

// tail keeps the last n bytes, which is where a failing command's error is.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	// Do not start mid-line; a truncated first line reads as corruption.
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return "[output truncated]\n" + cut
}

// runFacts exposes a finished command to its branch action's templates.
//
// Named so a manifest reads plainly: {{stdout}}, {{stderr}}, {{exit_code}}.
func runFacts(r RunResult) map[string]string {
	return map[string]string{
		"stdout":    strings.TrimRight(r.Stdout, "\n"),
		"stderr":    strings.TrimRight(r.Stderr, "\n"),
		"exit_code": strconv.Itoa(r.ExitCode),
		"duration":  r.Duration.Round(time.Millisecond).String(),
	}
}

// runAction executes a run action and fires whichever branch its exit code
// selects.
//
// The command's output is expanded into the branch's templates, so a failing
// gate can hand an agent the error it produced — which is the whole point of
// having a mechanical gate in a loop of language models.
func (e *Engine) runAction(ctx context.Context, r Rule, f *facts, a RunAction) error {
	// Expand templates in argv before running. Values land as whole arguments,
	// never reparsed — see RunAction's no-shell note.
	expanded := make([]string, 0, len(a.Argv))
	for i, arg := range a.Argv {
		v, err := f.expand(arg)
		if err != nil {
			return fmt.Errorf("run: argv[%d]: %w", i, err)
		}
		expanded = append(expanded, v)
	}
	a.Argv = expanded

	if a.CWD == "" {
		a.CWD = e.cfg.RunCWD
	}

	e.log.Info("run action starting", "rule", r.Name, "argv", a.Argv, "cwd", a.CWD)
	res := runCommand(ctx, a, e.SlotEnv(f.slot))
	e.log.Info("run action finished", "rule", r.Name, "result", res.String())

	// A command that could not be started is a manifest or environment fault,
	// not a gate result: branching on it would report "the tests failed" when
	// the truth is the test runner is not installed. Escalate with the reason
	// (§4.11) rather than firing OnFailure.
	if res.Err != nil {
		return fmt.Errorf("run %v: %w", a.Argv, res.Err)
	}

	branch := a.OnFailure
	which := "on_failure"
	if res.Succeeded() {
		branch = a.OnSuccess
		which = "on_success"
	}
	if branch == nil {
		// A gate with no branch on this side is legitimate: the rule exists to
		// stop the loop advancing, and nothing further is wanted.
		e.log.Info("run action has no branch for this outcome", "rule", r.Name, "outcome", which)
		return nil
	}

	// The branch sees the command's output as template fields.
	bf := f.withOverlay(runFacts(res))
	return e.fireBranch(ctx, r, bf, *branch, which)
}

// fireBranch carries out a run action's chosen branch.
//
// Deliberately narrow: only the terminal action variants are reachable here.
// Nested run actions are rejected by the manifest validator, and the branch
// runs inside the parent's already-held slot lock and already-charged
// iteration, so re-entering the full fire path would double-charge the budget
// and deadlock on the lock it already owns.
func (e *Engine) fireBranch(ctx context.Context, r Rule, f *facts, a Action, which string) error {
	switch {
	case a.Finish != nil:
		e.requestFinish(Outcome{Reason: a.Finish.Reason, Rule: r.Name})
		return nil

	case a.Prompt != nil:
		text, err := f.expand(a.Prompt.Text)
		if err != nil {
			return fmt.Errorf("%s prompt: %w", which, err)
		}
		return e.prompt(ctx, a.Prompt.Slot, text)

	case a.Spawn != nil:
		return e.Spawn(ctx, a.Spawn.Slot)

	case a.Run != nil:
		// Unreachable via a validated manifest; explicit so a future manifest
		// change cannot turn this into unbounded recursion silently.
		return fmt.Errorf("%s: nested run actions are not supported", which)
	}
	return nil
}

// requestFinish asks Run to end the loop from a dispatched action.
//
// Non-blocking on a buffered channel: the first request wins, later ones are
// dropped. A loop ends once, and an action racing to report a second outcome
// must not block a goroutine that Run is waiting on — that would deadlock the
// teardown it is trying to trigger.
func (e *Engine) requestFinish(out Outcome) {
	select {
	case e.finish <- out:
	default:
		e.log.Debug("finish already requested; ignoring", "reason", out.Reason, "rule", out.Rule)
	}
}
