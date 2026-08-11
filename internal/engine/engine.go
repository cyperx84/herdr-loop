// Package engine evaluates a loop's rules against reconciled slot state and
// actuates the results on herdr.
//
// The engine is a reactive fold over state transitions, not over events. The
// distinction is load-bearing: herdr's events.subscribe replays event history
// before it streams live events (PLAN §4.8), so an engine edge-triggered on
// event arrival would spawn agents and send prompts for work that finished
// hours ago. Nothing here touches herdr.Event. The engine consumes Transition
// values a reconciled state model has already agreed against
// agent.list/session.snapshot, and it re-reads that model at fire time, because
// a queued transition can be stale by the time it is drained (§4.9).
//
// Every effect on a slot is gated:
//
//   - a rule only fires off a slot that has settled — never mid-turn (§4.4);
//   - a prompt only lands on a slot that is idle or done, unless that kind's
//     mid-turn injection capability has been measured and enabled (§4.4);
//   - one action per slot at a time, enforced by a per-slot mutex;
//   - spawns queue against a per-kind concurrency limit (§4.7);
//   - the iteration budget is enforced here at runtime, not only by the
//     manifest validator at parse time;
//   - a blocked slot goes to the blocked policy, never to a rule action (§4.3).
//
// Results move through files, never pane output (§4.1). Each slot is spawned
// with HERDR_LOOP_HANDOFF pointing at its own file under Config.HandoffDir; the
// engine reads that file and nothing else to learn what a slot produced.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	herdr "github.com/cyperx84/herdr-api"
	"github.com/cyperx84/herdr-loop/internal/state"
	"gopkg.in/yaml.v3"
)

// HandoffEnv is the environment variable every slot process is spawned with.
// The slot's prompt instructs its agent to write its result to this path, and
// the engine reads that file — pane output is never a result transport (§4.1).
const HandoffEnv = "HERDR_LOOP_HANDOFF"

// DefaultMaxConcurrent bounds how many agents of a kind with no measured
// entry in Config.Kinds may run at once.
//
// One, not more: concurrent processes of an OAuth-authenticated kind race a
// single rotating credential with no lock (§4.7), and an unmeasured kind is
// exactly the case where nobody has established it is safe to fan out.
const DefaultMaxConcurrent = 1

// Herdr is the slice of the herdr socket API the actuator calls.
//
// It is declared here rather than taken as *herdr.Client so the engine can be
// tested against a fake with no live connection. Signatures are copied from
// *herdr.Client verbatim so it satisfies this implicitly.
type Herdr interface {
	AgentPrompt(ctx context.Context, target, text string, wait *herdr.AgentPromptWaitOptions) (herdr.Agent, error)
	AgentSendKeys(ctx context.Context, target string, keys []string) error
	AgentStart(ctx context.Context, params herdr.AgentStartParams) (herdr.AgentStartResult, error)
	PaneSplit(ctx context.Context, params herdr.PaneSplitParams) (herdr.Pane, error)
	NotificationShow(ctx context.Context, params herdr.NotificationShowParams) (herdr.NotificationShowResult, error)
	// PaneSendText writes raw text to a pane. Needed for control sequences
	// herdr's key vocabulary does not cover — S-Tab among them.
	PaneSendText(ctx context.Context, paneID, text string) error
	// AgentGet reads one agent's current record. Used to wait for a
	// just-started agent to become interactive before anything is sent to it.
	AgentGet(ctx context.Context, target string) (herdr.Agent, error)
	// PaneProcessInfo is the corroboration channel required by PLAN §4.9.
	// Re-reading the state model proves nothing: it returns the same
	// inference that is under suspicion. Asking herdr what process is
	// actually in the pane is independent evidence.
	PaneProcessInfo(ctx context.Context, paneID string) (herdr.PaneProcessInfo, error)
}

// Compile-time proof the interface still tracks the real client: if herdr-api
// changes one of these signatures, this fails to build instead of the fake and
// the client silently diverging.
var _ Herdr = (*herdr.Client)(nil)

// Transition is one reconciled change of a slot's agent status.
//
// It carries no event payload on purpose. Producing Transitions is the state
// model's job, and it must not emit any until the replayed prefix has been
// drained and the model agrees with a fresh snapshot (§4.8) — by the time one
// reaches the engine it describes something that actually just happened.
type Transition struct {
	Slot string
	From herdr.AgentStatus
	To   herdr.AgentStatus
	// At is when the model observed the change. Zero is tolerated; it only
	// costs the stall duration reported on an escalation (§4.11).
	At time.Time
}

// Model is the reconciled state the engine reads. It is authoritative: where a
// drained Transition and the Model disagree, the Model wins (§4.9).
type Model interface {
	// SlotStatus reports a slot's current reconciled status. The second
	// return is false for a slot the model has never seen — which is not the
	// same as a slot whose status is unknown, and must not be conflated with
	// it.
	SlotStatus(slot string) (herdr.AgentStatus, bool)
	// SlotTarget reports the herdr agent target (pane id or agent name) to
	// address a slot by. False until the slot's agent exists.
	SlotTarget(slot string) (string, bool)
	// BlockedPrompt reports the prompt text a blocked slot is waiting on, as
	// herdr's own classifier saw it. False when the model cannot see it —
	// which the auto blocked policy treats as unmatched, never as consent.
	BlockedPrompt(slot string) (string, bool)
	// SlotTier reports how herdr learned this slot's status: structured
	// self-reporting, or heuristic screen classification (PLAN §4.6). The
	// distinction is load-bearing — a screen-classified "done" is an
	// inference, not an observation — so it must survive the trip from the
	// state model to the rules rather than being flattened here.
	SlotTier(slot string) (state.Tier, bool)
}

// BlockedPolicy decides what happens when a slot reaches herdr.StatusBlocked —
// meaning herdr recognised an approval or question UI. Blind-firing enter on
// that is an agent granting itself permissions (§4.3), so there is no policy
// here that answers anything the manifest did not name exactly.
type BlockedPolicy string

const (
	// BlockedEscalate notifies a human and halts that slot. Default.
	BlockedEscalate BlockedPolicy = "escalate"
	// BlockedPause notifies and halts the whole loop.
	BlockedPause BlockedPolicy = "pause"
	// BlockedAuto answers prompts matching Config.BlockedRules exactly, and
	// escalates everything else.
	BlockedAuto BlockedPolicy = "auto"
)

// BlockedRule is one exactly-matched approval prompt the auto policy may
// answer.
//
// Prompt is compared for whole-string equality after trimming surrounding
// whitespace. There is deliberately no regexp, glob, or substring form: a
// pattern language here is a way to accidentally consent to a prompt nobody
// read.
type BlockedRule struct {
	Prompt string
	// Keys are sent verbatim via agent.send_keys. There is no default —
	// an unanswerable rule is a config error, not an implicit enter.
	Keys []string
}

// KindConfig is the measured capability of one agent kind. These are runtime
// facts from kinds.toml, never constants compiled in: the same kind differs by
// version and by whether its integration is installed (§4.6).
type KindConfig struct {
	// MaxConcurrent bounds live agents of this kind (§4.7). Zero means
	// DefaultMaxConcurrent.
	MaxConcurrent int
	// MidTurnInjection permits prompting this kind while it is working.
	// Off until measured (§4.4): agent.prompt tracks lifecycle state, not
	// turn boundaries, and where the text lands mid-turn differs per kind.
	MidTurnInjection bool
	// StartupKeys are sent to a freshly started agent of this kind, before
	// its first prompt, to put its UI into a state where work actually
	// happens.
	//
	// This exists because a harness can silently reinterpret the job. Claude
	// Code launched in plan mode turns "implement this" into "write a plan
	// for this": the agent reports working, finishes, and reports done,
	// having changed nothing — a loop gate then fails forever on work that
	// was never attempted, and nothing in the lifecycle says why.
	//
	// Sent through pane.send_text rather than agent.send_keys because the
	// sequences that matter are raw terminal input: herdr rejects S-Tab and
	// BackTab as unsupported keys, while the escape sequence they stand for
	// goes through unchanged.
	StartupKeys []string
	// StartupSettle is how long to wait after StartupKeys before prompting,
	// letting the UI redraw. Zero means DefaultStartupSettle.
	StartupSettle time.Duration
}

// SlotConfig is a named seat for an agent.
type SlotConfig struct {
	Name string
	Kind string
	// CWD is the slot's worktree. One worktree per slot is non-negotiable
	// (§4.2); enforcing that is the manifest validator's job, not the
	// engine's.
	CWD         string
	WorkspaceID string
	// Direction is the split axis for this slot's pane. Empty means
	// herdr.SplitRight.
	Direction herdr.SplitDirection
	Args      []string
}

// PredicateOp is one of herdr's own predicate operators, reused so manifest
// authors do not learn a second filter language.
type PredicateOp string

const (
	OpAll    PredicateOp = "all"
	OpAny    PredicateOp = "any"
	OpNot    PredicateOp = "not"
	OpEq     PredicateOp = "eq"
	OpIn     PredicateOp = "in"
	OpExists PredicateOp = "exists"
)

// Predicate is a rule's condition, evaluated against the reconciled model and
// slot handoff files — never against an event.
//
// Field grammar, in resolution order:
//
//	slot                      name of the slot that transitioned
//	status                    its reconciled status
//	kind                      its agent kind
//	iteration                 rule firings consumed so far
//	slot.<name>.status        another slot's reconciled status
//	slot.<name>.kind          another slot's agent kind
//	<name>.handoff            path of a slot's handoff file
//	<name>.handoff.<a>.<b>    a typed field from that file's front-matter
//	<name>                    a manifest variable from Config.Vars
//
// All comparisons are string comparisons; front-matter scalars are rendered to
// their string form first, so verdict = true and verdict = "true" compare
// equal. This keeps the manifest untyped rather than inventing a type system
// for it.
type Predicate struct {
	Op      PredicateOp
	Field   string
	Value   string
	Values  []string
	Filters []Predicate
}

// PromptAction sends text to a slot. The text may reference any predicate
// field as {{field}} — most usefully {{other.handoff}}, the path of another
// slot's result file.
type PromptAction struct {
	Slot string
	Text string
}

// SpawnAction brings a slot up: a pane carrying the slot's handoff environment,
// then an agent in it. Queued against the kind's concurrency limit (§4.7).
type SpawnAction struct {
	Slot string
}

// FinishAction ends the loop with a reason the caller reports verbatim.
type FinishAction struct {
	Reason string
}

// Action is what a rule does. Exactly one field is non-nil; New rejects
// anything else rather than silently picking one.
type Action struct {
	Prompt *PromptAction
	Spawn  *SpawnAction
	Finish *FinishAction
	Run    *RunAction
}

// Rule is one when/then pair. Name is required — it is what an escalation
// reports as the cause, and "which rule did this" is unanswerable without it
// (§4.11).
type Rule struct {
	Name string
	When Predicate
	Then Action
}

// Config is a parsed loop. The manifest package maps loop.toml onto this; the
// engine does no parsing of its own.
type Config struct {
	Name string
	// MaxIterations bounds total rule firings. Must be positive: a loop that
	// can run forever is a config error (§3).
	MaxIterations int
	// Strict refuses to fire any rule against a slot whose status herdr
	// derived by classifying the rendered screen rather than by the agent
	// self-reporting it (PLAN §4.6). Off by default: as of herdr 0.8.0 only
	// pi and opencode self-report, so strict excludes Claude Code and Codex
	// entirely. Turn it on for loops where acting on a wrong "done" is worse
	// than not acting at all.
	Strict bool
	// OnSpawn is called with a slot's name and the pane its agent was just
	// started in, before that fact could reach any reconcile.
	//
	// It exists because the alternative loses the agent's arrival: the first
	// transition lands within a second of the start, while a reconcile is
	// tens of seconds away, so a supervisor that learns the slot-to-pane
	// mapping only from agent.list drops the settled transition it was
	// waiting for and then waits forever, because a settled agent produces no
	// further events. Optional; nil is fine for callers that do not map panes
	// back to slots.
	OnSpawn func(slot, paneID string)
	// RunCWD is where run actions execute when they name no directory of
	// their own. Empty means the supervisor's working directory.
	//
	// Deliberately not a slot's worktree: a gate like "do the tests pass"
	// asks about the integration point, and defaulting it to whichever slot
	// happened to trigger the rule would make the same manifest mean
	// different things depending on which agent finished first.
	RunCWD string
	// HandoffDir holds the per-slot result files.
	HandoffDir   string
	OnBlocked    BlockedPolicy
	BlockedRules []BlockedRule
	Slots        []SlotConfig
	Rules        []Rule
	Kinds        map[string]KindConfig
	// Vars are manifest substitutions such as {{task}}.
	Vars map[string]string
}

// Escalation is the structured reason a slot or loop stopped.
//
// Never a bare timeout: which rule fired, what the slot's last observed status
// was, and how long it sat there are the facts that make a stall diagnosable
// instead of silent (§4.11).
type Escalation struct {
	Slot   string
	Rule   string
	Reason string
	Status herdr.AgentStatus
	// Prompt is the approval text a blocked slot was sitting on, when the
	// model could see it.
	Prompt string
	// Stalled is how long the slot had been in Status. Zero when the
	// transition carried no timestamp.
	Stalled time.Duration
	At      time.Time
	Err     error
}

func (e Escalation) String() string {
	s := fmt.Sprintf("slot %s: %s (status %s", e.Slot, e.Reason, e.Status)
	if e.Rule != "" {
		s += ", rule " + e.Rule
	}
	if e.Stalled > 0 {
		s += ", stalled " + e.Stalled.Round(time.Second).String()
	}
	s += ")"
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Why a loop stopped. Rule-supplied finish reasons are reported verbatim, so
// these are namespaced to keep them distinguishable from a manifest's own.
const (
	ReasonBudgetExhausted = "loop:budget-exhausted"
	ReasonPaused          = "loop:paused"
	ReasonStreamClosed    = "loop:stream-closed"
	ReasonCanceled        = "loop:canceled"
)

// Outcome is why Run returned.
type Outcome struct {
	// Reason is a FinishAction's reason verbatim, or one of the loop:
	// constants above.
	Reason string
	// Rule is the rule that finished the loop, empty when the loop ended for
	// a reason no rule caused.
	Rule       string
	Iterations int
	// Escalations is everything that needed a human, in the order it
	// happened. A converged loop with escalations converged despite them.
	Escalations []Escalation
}

// Engine folds transitions into actions.
type Engine struct {
	cfg    Config
	client Herdr
	model  Model
	log    *slog.Logger

	slots map[string]SlotConfig

	mu          sync.Mutex
	iterations  int
	halted      map[string]bool
	escalations []Escalation
	locks       map[string]*sync.Mutex
	// kindGates are counting semaphores, one per kind, each holding a token
	// for the lifetime of a live agent rather than for the duration of a
	// spawn call — the credential race is between running processes, not
	// between the calls that started them (§4.7).
	kindGates map[string]chan struct{}
	held      map[string]string

	actions sync.WaitGroup

	// finish carries a loop-ending outcome from an asynchronous action back to
	// Run. A run action's branch may finish the loop, but it completes in a
	// dispatched goroutine long after fire returned, so it cannot report an
	// outcome up the call stack the way a synchronous rule does. Buffered and
	// written non-blockingly: the first finish wins and later ones are
	// dropped, because a loop ends once.
	finish chan Outcome
}

// New validates cfg and returns an engine bound to a herdr client and a
// reconciled model.
//
// Validation is strict and up front because every check here is one an
// operator would otherwise discover as a misfire against a live agent.
func New(cfg Config, client Herdr, model Model, log *slog.Logger) (*Engine, error) {
	if client == nil || model == nil {
		return nil, errors.New("engine: New: client and model are required")
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxIterations <= 0 {
		return nil, fmt.Errorf("engine: New: max_iterations must be positive, got %d", cfg.MaxIterations)
	}
	if cfg.HandoffDir == "" {
		return nil, errors.New("engine: New: handoff_dir is required — results move through files, not pane output")
	}
	if cfg.OnBlocked == "" {
		cfg.OnBlocked = BlockedEscalate
	}
	switch cfg.OnBlocked {
	case BlockedEscalate, BlockedPause, BlockedAuto:
	default:
		return nil, fmt.Errorf("engine: New: unknown on_blocked policy %q", cfg.OnBlocked)
	}

	slots := make(map[string]SlotConfig, len(cfg.Slots))
	for _, s := range cfg.Slots {
		if s.Name == "" {
			return nil, errors.New("engine: New: slot with no name")
		}
		if _, dup := slots[s.Name]; dup {
			return nil, fmt.Errorf("engine: New: duplicate slot %q", s.Name)
		}
		slots[s.Name] = s
	}

	if cfg.OnBlocked == BlockedAuto && len(cfg.BlockedRules) == 0 {
		return nil, errors.New("engine: New: on_blocked = auto requires blocked_rule entries — auto is not a blanket answer")
	}
	for i, br := range cfg.BlockedRules {
		if strings.TrimSpace(br.Prompt) == "" {
			return nil, fmt.Errorf("engine: New: blocked_rule %d has an empty prompt", i)
		}
		if len(br.Keys) == 0 {
			return nil, fmt.Errorf("engine: New: blocked_rule %d (%q) sends no keys", i, br.Prompt)
		}
	}

	for _, r := range cfg.Rules {
		if r.Name == "" {
			return nil, errors.New("engine: New: rule with no name — escalations report the rule that caused them")
		}
		if err := validatePredicate(r.When); err != nil {
			return nil, fmt.Errorf("engine: New: rule %q: %w", r.Name, err)
		}
		if err := validateAction(r.Then, slots); err != nil {
			return nil, fmt.Errorf("engine: New: rule %q: %w", r.Name, err)
		}
	}

	return &Engine{
		cfg:       cfg,
		client:    client,
		model:     model,
		log:       log,
		slots:     slots,
		halted:    map[string]bool{},
		locks:     map[string]*sync.Mutex{},
		finish:    make(chan Outcome, 1),
		kindGates: map[string]chan struct{}{},
		held:      map[string]string{},
	}, nil
}

func validatePredicate(p Predicate) error {
	switch p.Op {
	case OpAll, OpAny:
		for _, f := range p.Filters {
			if err := validatePredicate(f); err != nil {
				return err
			}
		}
	case OpNot:
		if len(p.Filters) != 1 {
			return fmt.Errorf("predicate %q takes exactly one filter, got %d", p.Op, len(p.Filters))
		}
		return validatePredicate(p.Filters[0])
	case OpEq, OpExists:
		if p.Field == "" {
			return fmt.Errorf("predicate %q needs a field", p.Op)
		}
	case OpIn:
		if p.Field == "" {
			return fmt.Errorf("predicate %q needs a field", p.Op)
		}
		if len(p.Values) == 0 {
			return errors.New(`predicate "in" with no values can never hold`)
		}
	default:
		return fmt.Errorf("unknown predicate op %q", p.Op)
	}
	return nil
}

func validateAction(a Action, slots map[string]SlotConfig) error {
	n := 0
	for _, set := range []bool{a.Prompt != nil, a.Spawn != nil, a.Finish != nil, a.Run != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("action must set exactly one of prompt/spawn/finish/run, got %d", n)
	}
	switch {
	case a.Prompt != nil:
		if _, ok := slots[a.Prompt.Slot]; !ok {
			return fmt.Errorf("prompt targets unknown slot %q", a.Prompt.Slot)
		}
		if a.Prompt.Text == "" {
			return fmt.Errorf("prompt to slot %q has no text", a.Prompt.Slot)
		}
	case a.Spawn != nil:
		if _, ok := slots[a.Spawn.Slot]; !ok {
			return fmt.Errorf("spawn targets unknown slot %q", a.Spawn.Slot)
		}
	case a.Finish != nil:
		if a.Finish.Reason == "" {
			return errors.New("finish has no reason")
		}
	case a.Run != nil:
		if len(a.Run.Argv) == 0 {
			return errors.New("run has no argv")
		}
		// Branches are validated with the same rules, so a branch targeting an
		// unknown slot is caught here rather than at fire time — by which point
		// the command has already run and its result would be discarded.
		for _, br := range []struct {
			name string
			act  *Action
		}{{"on_success", a.Run.OnSuccess}, {"on_failure", a.Run.OnFailure}} {
			if br.act == nil {
				continue
			}
			if br.act.Run != nil {
				return fmt.Errorf("%s: nested run actions are not supported", br.name)
			}
			if err := validateAction(*br.act, slots); err != nil {
				return fmt.Errorf("%s: %w", br.name, err)
			}
		}
	}
	return nil
}

// HandoffPath is where a slot writes its result.
//
// One stable file per slot, not one per iteration: pane.split is the only
// method in protocol 19 that accepts an env map (agent.start has no env field),
// so HERDR_LOOP_HANDOFF is fixed when the slot's pane is created and cannot be
// rotated under a running agent. Iteration history is the caller's to keep by
// archiving the file, not something the engine can change out from under a
// process that already read its environment.
func (e *Engine) HandoffPath(slot string) string {
	return filepath.Join(e.cfg.HandoffDir, slot+".md")
}

// SlotEnv is the environment a slot's pane is created with.
func (e *Engine) SlotEnv(slot string) map[string]string {
	return map[string]string{HandoffEnv: e.HandoffPath(slot)}
}

// Handoff reads a slot's result file.
func (e *Engine) Handoff(slot string) (*Handoff, error) {
	return ParseHandoff(e.HandoffPath(slot))
}

// Run folds transitions into actions until the loop finishes, the budget is
// exhausted, the stream ends, or ctx is canceled.
//
// Transitions are supplied by the caller rather than subscribed to here, so
// that replay draining and snapshot agreement (§4.8) stay the state model's
// concern and cannot be skipped by accident in this package.
func (e *Engine) Run(ctx context.Context, transitions <-chan Transition) (Outcome, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			// The loop is over; in-flight actions go with it.
			cancel()
			e.actions.Wait()
			return e.outcome(ReasonCanceled, ""), ctx.Err()
		case out := <-e.finish:
			// An asynchronous action asked to end the loop (a run action's
			// branch reaching finish). Same teardown as a synchronous finish.
			cancel()
			e.actions.Wait()
			out.Iterations = e.iterationCount()
			out.Escalations = e.takeEscalations()
			return out, nil
		case tr, ok := <-transitions:
			if !ok {
				// Graceful end of stream: let dispatched actions land before
				// reporting, so the outcome describes finished work.
				e.actions.Wait()
				// An action that landed during that wait may have asked to
				// end the loop — a run action's gate passing, say. Its
				// outcome is the real one; reporting stream-closed over it
				// would discard the answer the loop was waiting for.
				select {
				case out := <-e.finish:
					out.Iterations = e.iterationCount()
					out.Escalations = e.takeEscalations()
					return out, nil
				default:
				}
				return e.outcome(ReasonStreamClosed, ""), nil
			}
			out, done := e.step(runCtx, tr)
			if done {
				cancel()
				e.actions.Wait()
				out.Iterations = e.iterationCount()
				out.Escalations = e.takeEscalations()
				return out, nil
			}
		}
	}
}

func (e *Engine) outcome(reason, rule string) Outcome {
	return Outcome{
		Reason:      reason,
		Rule:        rule,
		Iterations:  e.iterationCount(),
		Escalations: e.takeEscalations(),
	}
}

// step is the whole fold: one transition in, at most one loop-ending outcome
// out.
func (e *Engine) step(ctx context.Context, tr Transition) (Outcome, bool) {
	if _, known := e.slots[tr.Slot]; !known {
		// A pane the loop does not own. Reconciliation is session-wide, so
		// this is expected traffic, not an error.
		e.log.Debug("transition for a slot this loop does not own", "slot", tr.Slot)
		return Outcome{}, false
	}
	if e.isHalted(tr.Slot) {
		e.log.Debug("slot is halted, ignoring transition", "slot", tr.Slot)
		return Outcome{}, false
	}

	status, ok := e.model.SlotStatus(tr.Slot)
	if !ok {
		e.log.Warn("model has no state for a slot that transitioned", "slot", tr.Slot)
		return Outcome{}, false
	}
	if status != tr.To {
		// The model is authoritative. A transition can be stale by the time
		// it is drained, and acting on the drained value would be acting on
		// state that no longer holds (§4.9).
		e.log.Debug("transition superseded by the model",
			"slot", tr.Slot, "transition_to", tr.To, "model", status)
	}

	if status == herdr.StatusBlocked {
		// A blocked slot never reaches the rules. herdr classified an
		// approval or question UI; what happens next is policy, not a rule's
		// business (§4.3).
		return e.handleBlocked(ctx, tr, status)
	}
	if !triggerEligible(status) {
		// working and unknown both land here. unknown is not proof of
		// completion (§4.5) and working is mid-turn (§4.4).
		e.log.Debug("slot not settled, no rules evaluated", "slot", tr.Slot, "status", status)
		return Outcome{}, false
	}

	f := e.newFacts(tr.Slot, status)
	for _, r := range e.cfg.Rules {
		if !e.holds(r.When, f) {
			continue
		}
		if out, done := e.fire(ctx, r, f, tr); done {
			return out, true
		}
	}
	return Outcome{}, false
}

// triggerEligible reports whether a slot in this status may trigger rule
// evaluation (§4.4).
//
// Deliberately not herdr.AgentStatus.Settled(): that answers "ready to accept a
// new prompt" for the API's own callers and excludes blocked. This set includes
// blocked because a blocked slot must still reach the policy handler, and the
// two questions are free to diverge.
func triggerEligible(s herdr.AgentStatus) bool {
	return s == herdr.StatusIdle || s == herdr.StatusDone || s == herdr.StatusBlocked
}

// fire executes a rule whose predicate holds. It reports a loop-ending outcome.
func (e *Engine) fire(ctx context.Context, r Rule, f *facts, tr Transition) (Outcome, bool) {
	// Eligibility and the slot lock are both taken before the budget, so a
	// rule that cannot act right now costs nothing. Charging an iteration for
	// a skipped action would let contention alone exhaust the loop.
	var lk *sync.Mutex
	if slot, needsSlot := actionSlot(r.Then); needsSlot {
		if !e.actionable(r, slot) {
			return Outcome{}, false
		}
		var free bool
		if lk, free = e.tryLockSlot(slot); !free {
			// One action per slot. Skip rather than queue: the in-flight
			// action produces its own transition, which re-triggers
			// evaluation, and reconciliation is idempotent anyway (§4.12).
			// A queued action would instead be computed against state that
			// has since moved.
			e.log.Info("action skipped: another action is in flight for this slot",
				"rule", r.Name, "slot", slot)
			return Outcome{}, false
		}
	}

	// §4.9: confirm the inferred status against independent evidence before
	// spending an iteration on it. Placed after the lock so a corroboration
	// probe cannot race a concurrent action, and before the budget so a
	// refused action is free.
	if slot, needsSlot := actionSlot(r.Then); needsSlot && !e.corroborate(ctx, r, slot) {
		if lk != nil {
			lk.Unlock()
		}
		return Outcome{}, false
	}

	// The budget is enforced here as well as by the manifest validator.
	// Parse-time cycle detection proves a manifest cannot loop forever; it
	// does not prove the manifest running now is the one that was validated,
	// and it says nothing about cycles that only close through runtime state.
	n := e.consumeIteration()
	if n > e.cfg.MaxIterations {
		if lk != nil {
			lk.Unlock()
		}
		e.log.Warn("iteration budget exhausted", "rule", r.Name, "max_iterations", e.cfg.MaxIterations)
		return Outcome{Reason: ReasonBudgetExhausted, Rule: r.Name}, true
	}
	e.log.Info("rule fired", "rule", r.Name, "slot", tr.Slot, "iteration", n)

	switch {
	case r.Then.Finish != nil:
		return Outcome{Reason: r.Then.Finish.Reason, Rule: r.Name}, true

	case r.Then.Prompt != nil:
		a := *r.Then.Prompt
		text, err := f.expand(a.Text)
		if err != nil {
			// Sending a prompt with unresolved placeholders in it is worse
			// than sending nothing: the agent acts on a literal {{...}}.
			lk.Unlock()
			// The status reported is the target's, not the trigger's: the
			// escalation names the slot that was about to be prompted.
			targetStatus, _ := e.model.SlotStatus(a.Slot)
			e.escalate(ctx, Escalation{
				Slot: a.Slot, Rule: r.Name, Reason: "prompt template did not resolve",
				Status: targetStatus, Err: err, At: time.Now(),
			})
			return Outcome{}, false
		}
		e.dispatch(ctx, lk, a.Slot, r.Name, func(ctx context.Context) error {
			return e.prompt(ctx, a.Slot, text)
		})

	case r.Then.Spawn != nil:
		slot := r.Then.Spawn.Slot
		e.dispatch(ctx, lk, slot, r.Name, func(ctx context.Context) error {
			return e.Spawn(ctx, slot)
		})

	case r.Then.Run != nil:
		branchSlot, _ := actionSlot(r.Then)
		e.dispatch(ctx, lk, branchSlot, r.Name, func(ctx context.Context) error {
			return e.runAction(ctx, r, f, *r.Then.Run)
		})
	}
	return Outcome{}, false
}

// actionSlot reports which slot an action acts on. finish acts on none.
func actionSlot(a Action) (string, bool) {
	switch {
	case a.Prompt != nil:
		return a.Prompt.Slot, true
	case a.Spawn != nil:
		return a.Spawn.Slot, true
	case a.Run != nil:
		// A run action touches no slot itself, but its branches may. Report
		// the branch's slot so the per-slot lock is taken up front: without
		// it, a long build could finish and prompt a slot that a rule started
		// prompting meanwhile, racing two prompts into one agent.
		if a.Run.OnSuccess != nil {
			if slot, ok := actionSlot(*a.Run.OnSuccess); ok {
				return slot, true
			}
		}
		if a.Run.OnFailure != nil {
			if slot, ok := actionSlot(*a.Run.OnFailure); ok {
				return slot, true
			}
		}
	}
	return "", false
}

// actionable reports whether a rule's action can be carried out on slot right
// now, ignoring contention (which fire handles separately).
func (e *Engine) actionable(r Rule, slot string) bool {
	if e.isHalted(slot) {
		e.log.Info("action skipped: target slot is halted", "rule", r.Name, "slot", slot)
		return false
	}
	if !e.trustEligible(r, slot) {
		return false
	}
	if r.Then.Prompt == nil {
		return true
	}
	status, ok := e.model.SlotStatus(slot)
	if !ok {
		e.log.Info("prompt skipped: no reconciled state for target slot", "rule", r.Name, "slot", slot)
		return false
	}
	if !e.promptEligible(slot, status) {
		e.log.Info("prompt skipped: target slot is not accepting prompts",
			"rule", r.Name, "slot", slot, "status", status)
		return false
	}
	return true
}

// trustEligible applies Config.Strict (PLAN §4.6).
//
// herdr derives some kinds' status by classifying the rendered screen, which
// is an inference rather than an observation. Strict refuses to act on one.
// As of herdr 0.8.0 that means a strict loop over Claude Code or Codex slots
// will not fire — which is the honest consequence of the measurement, not a
// defect here. Slots whose agents self-report (pi, opencode) are unaffected.
//
// A slot with no known tier is refused too: unknown provenance is not
// evidence of structured provenance.
func (e *Engine) trustEligible(r Rule, slot string) bool {
	if !e.cfg.Strict {
		return true
	}
	tier, ok := e.model.SlotTier(slot)
	if !ok {
		e.log.Warn("action refused: strict mode and the slot has no known detection tier",
			"rule", r.Name, "slot", slot)
		return false
	}
	if !tier.Trusted() {
		e.log.Warn("action refused: strict mode and this slot's status is screen-classified, not self-reported",
			"rule", r.Name, "slot", slot, "tier", string(tier))
		return false
	}
	return true
}

// corroborate is the §4.9 check: an inferred status must be confirmed by
// independent evidence before it drives an action.
//
// Re-reading the state model proves nothing — it returns the same inference
// that is under suspicion. Asking herdr which processes hold the pane answers
// a different question with different failure modes, so agreement between the
// two is worth more than either alone. The specific lie this catches is the
// one the ecosystem reports most: an agent that crashed or wedged leaving a
// screen that still classifies as idle or done.
//
// Only applied where it can pay for itself: a self-reporting agent's status is
// not an inference, and a probe failure is not treated as proof of death —
// herdr may simply not be able to read the pane. Returning false halts the
// action; the slot's next transition brings it back.
func (e *Engine) corroborate(ctx context.Context, r Rule, slot string) bool {
	tier, ok := e.model.SlotTier(slot)
	if ok && tier.Trusted() {
		return true // self-reported: not an inference, nothing to corroborate
	}
	target, ok := e.model.SlotTarget(slot)
	if !ok {
		return true // no pane yet — spawn paths have nothing to probe
	}
	info, err := e.client.PaneProcessInfo(ctx, target)
	if err != nil {
		// Inconclusive, not disproven. Log and proceed: refusing to act on a
		// probe outage would stall every loop whenever herdr is busy.
		e.log.Warn("corroboration probe failed; proceeding on the inferred status",
			"rule", r.Name, "slot", slot, "err", err)
		return true
	}
	// A pane whose agent exited falls back to its shell, and the shell is
	// itself a foreground process — so "has any foreground process" stays true
	// for a dead agent and corroborates nothing. The real question is whether
	// something other than the bare shell is running.
	if !info.RunsForegroundCommand() {
		e.log.Warn("action refused: pane has fallen back to its shell, so the agent is gone and the inferred status is stale",
			"rule", r.Name, "slot", slot, "pane", target)
		return false
	}
	return true
}

// promptEligible reports whether a slot may be prompted in this status.
//
// idle and done always; blocked never, because the input would land in an
// approval dialog; unknown never, because unknown is not done (§4.5); working
// only where that kind's mid-turn injection has been measured and turned on
// (§4.4) — agent.prompt tracks lifecycle state, not turn boundaries, so where
// the text ends up mid-turn is a per-kind unknown until someone measures it.
func (e *Engine) promptEligible(slot string, s herdr.AgentStatus) bool {
	if s == herdr.StatusWorking {
		return e.kindConfig(e.slots[slot].Kind).MidTurnInjection
	}
	return s == herdr.StatusIdle || s == herdr.StatusDone
}

// dispatch runs an action on its own goroutine and releases the slot lock the
// caller already holds when it finishes.
//
// Actions run off the fold so a slow one — a spawn queued behind a kind's
// concurrency limit, most of all — cannot stall the engine's reading of
// further transitions.
func (e *Engine) dispatch(ctx context.Context, lk *sync.Mutex, slot, rule string, fn func(context.Context) error) {
	e.actions.Add(1)
	go func() {
		defer e.actions.Done()
		// lk is nil for an action that owns no slot — a run action whose
		// branches only finish the loop, for instance. Not every action
		// contends for an agent, so an absent lock is a normal case rather
		// than a caller error.
		if lk != nil {
			defer lk.Unlock()
		}
		if err := fn(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				e.log.Debug("action canceled", "slot", slot, "rule", rule)
				return
			}
			status, _ := e.model.SlotStatus(slot)
			e.escalate(ctx, Escalation{
				Slot: slot, Rule: rule, Reason: "action failed",
				Status: status, Err: err, At: time.Now(),
			})
		}
	}()
}

// prompt sends text to a slot, re-checking eligibility against the model
// because the slot may have moved between the fold and this goroutine.
func (e *Engine) prompt(ctx context.Context, slot, text string) error {
	status, ok := e.model.SlotStatus(slot)
	if !ok {
		return fmt.Errorf("engine: prompt %s: no reconciled state", slot)
	}
	if !e.promptEligible(slot, status) {
		e.log.Info("prompt abandoned: slot moved before the prompt was sent", "slot", slot, "status", status)
		return nil
	}
	target, ok := e.model.SlotTarget(slot)
	if !ok {
		return fmt.Errorf("engine: prompt %s: slot has no agent target", slot)
	}
	// wait is nil on purpose. Blocking on agent.prompt --wait would make the
	// fold synchronous with an agent's turn; the settle is observed as the
	// next transition instead, which is the whole point of an event-driven
	// engine.
	if err := e.sendPrompt(ctx, target, text); err != nil {
		if herdr.IsAgentPromptStalled(err) {
			// herdr could not observe a lifecycle change within 5s of
			// delivery — it does not know the prompt was received. Surfacing
			// that is the point; silently assuming it landed is the
			// status-lies failure (§4.9).
			return fmt.Errorf("engine: agent.prompt %s: herdr saw no lifecycle change, delivery unconfirmed: %w", slot, err)
		}
		return fmt.Errorf("engine: agent.prompt %s: %w", slot, err)
	}
	return nil
}

// SendPrompt delivers a prompt to an agent target, retrying while herdr
// reports it is not yet addressable.
//
// Exported for the supervisor's bootstrap path: a slot's first prompt is
// delivered outside the rule engine, but it hits the same not-yet-registered
// window and must handle it the same way.
func (e *Engine) SendPrompt(ctx context.Context, target, text string) error {
	return e.sendPrompt(ctx, target, text)
}

// waitInteractive blocks until herdr reports the agent in a pane is past its
// launch-pending seat and ready for input.
func (e *Engine) waitInteractive(ctx context.Context, paneID string) error {
	deadline := time.Now().Add(AgentReadyTimeout)
	for {
		a, err := e.client.AgentGet(ctx, paneID)
		if err == nil && a.InteractiveReady && !a.LaunchPending {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("agent.get %s: %w", paneID, err)
			}
			return fmt.Errorf("agent in pane %s not interactive within %s", paneID, AgentReadyTimeout)
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// DefaultStartupSettle gives a harness's UI time to redraw after startup keys
// before the first prompt is delivered.
const DefaultStartupSettle = 400 * time.Millisecond

// applyStartupKeys puts a freshly started agent into a workable state.
//
// Failures are logged, never fatal: a harness that ignores the sequence is no
// worse off than one that was never sent it, and refusing to run the loop over
// a cosmetic keystroke would be the wrong trade.
func (e *Engine) applyStartupKeys(ctx context.Context, slot, paneID string) {
	kc := e.kindConfig(e.slots[slot].Kind)
	if len(kc.StartupKeys) == 0 {
		return
	}
	// Wait for the harness to actually be interactive first. agent.start
	// returns a launch-pending seat, so sending immediately writes into a
	// terminal whose TUI has not started — the bytes are swallowed and the
	// mode they were meant to change stays on. Observed exactly that with
	// Claude Code: the sequence went out, plan mode remained, and the agent
	// produced a plan and an approval dialog instead of an edit.
	if err := e.waitInteractive(ctx, paneID); err != nil {
		e.log.Warn("startup keys skipped: agent never became interactive",
			"slot", slot, "pane", paneID, "err", err)
		return
	}
	for _, seq := range kc.StartupKeys {
		if err := e.client.PaneSendText(ctx, paneID, decodeEscapes(seq)); err != nil {
			e.log.Warn("startup key not delivered", "slot", slot, "pane", paneID, "err", err)
			return
		}
	}
	settle := kc.StartupSettle
	if settle <= 0 {
		settle = DefaultStartupSettle
	}
	select {
	case <-time.After(settle):
	case <-ctx.Done():
	}
	e.log.Debug("startup keys applied", "slot", slot, "pane", paneID, "count", len(kc.StartupKeys))
}

// decodeEscapes expands the backslash escapes a TOML manifest can carry, so a
// kinds file can spell a control sequence readably as "\e[Z" rather than
// embedding a literal escape byte no editor will show.
func decodeEscapes(s string) string {
	return escapeDecoder.Replace(s)
}

// escapeDecoder is built once: the set is fixed and small.
var escapeDecoder = strings.NewReplacer(
	"\\e", "\x1b",
	"\\x1b", "\x1b",
	"\\r", "\r",
	"\\n", "\n",
	"\\t", "\t",
)

// AgentReadyTimeout bounds the wait for a just-started agent to become
// addressable by name.
const AgentReadyTimeout = 30 * time.Second

// PromptDeliveryTimeout bounds the confirmation that a prompt actually
// started an agent working. Short: this confirms delivery, it does not wait
// for the answer.
const PromptDeliveryTimeout = 15 * time.Second

func promptDeliveryTimeoutMs() *uint64 {
	ms := uint64(PromptDeliveryTimeout / time.Millisecond)
	return &ms
}

// sendPrompt delivers a prompt, retrying while herdr reports the agent is not
// yet an active named agent.
//
// herdr has a window in which an agent already reports idle but is not yet
// addressable: agent.start returns a launch-pending seat, and the first status
// event arrives before registration completes. Prompting inside that window
// fails agent_not_ready — reliably, on the very first prompt of every run,
// since that is exactly when the loop reacts to the agent settling.
//
// Retrying is the honest fix rather than sleeping a guessed interval: the
// server knows when it is ready and says so, and only that one error code is
// retried. Everything else, agent_prompt_stalled included, returns at once —
// a stall means delivery is unconfirmed, which is a report to make, not a
// condition to wait out.
func (e *Engine) sendPrompt(ctx context.Context, target, text string) error {
	deadline := time.Now().Add(AgentReadyTimeout)
	for attempt := 1; ; attempt++ {
		// A wait is requested, but only far enough to confirm delivery — not
		// to sit through the agent's turn.
		//
		// Passing nil looks right for an event-driven engine and is a trap:
		// herdr only runs its delivery check when a wait is asked for, so
		// agent.prompt returns success whether or not the text ever reached
		// the agent. Observed end to end — a prompt reported delivered, the
		// agent stayed idle, and the loop waited forever on a turn that never
		// began. That is the status-lie failure (§4.9) reaching the one
		// operation the whole loop depends on.
		//
		// until=working with a short timeout asks herdr to confirm the agent
		// actually started, then returns; the completion is still observed as
		// the next transition, which is what keeps the fold asynchronous.
		_, err := e.client.AgentPrompt(ctx, target, text, &herdr.AgentPromptWaitOptions{
			Until:     []herdr.AgentStatus{herdr.StatusWorking},
			TimeoutMs: promptDeliveryTimeoutMs(),
		})
		if err == nil {
			if attempt > 1 {
				e.log.Debug("prompt delivered after retry", "target", target, "attempts", attempt)
			}
			return nil
		}
		var apiErr *herdr.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "agent_not_ready" {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agent %s never became addressable within %s: %w", target, AgentReadyTimeout, err)
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Spawn brings a slot up: a pane carrying HERDR_LOOP_HANDOFF, then an agent in
// it. Exported because the supervisor spawns the initial slots the same way a
// rule does.
//
// It blocks until the kind's concurrency limit admits it (§4.7). agent.start
// accepts no env, so the handoff variable has to go in at pane.split time and
// the pane must exist first regardless.
func (e *Engine) Spawn(ctx context.Context, slot string) error {
	sc, ok := e.slots[slot]
	if !ok {
		return fmt.Errorf("engine: spawn: unknown slot %q", slot)
	}
	if err := e.acquireKind(ctx, slot, sc.Kind); err != nil {
		return fmt.Errorf("engine: spawn %s: %w", slot, err)
	}
	release := true
	defer func() {
		if release {
			e.releaseKind(slot)
		}
	}()

	dir := sc.Direction
	if dir == "" {
		dir = herdr.SplitRight
	}
	pane, err := e.client.PaneSplit(ctx, herdr.PaneSplitParams{
		Direction:   dir,
		WorkspaceID: sc.WorkspaceID,
		CWD:         sc.CWD,
		Env:         e.SlotEnv(slot),
	})
	if err != nil {
		return fmt.Errorf("engine: pane.split %s: %w", slot, err)
	}
	// A freshly split pane is not immediately an available shell: its shell
	// is still starting, and agent.start requires the shell itself to be in
	// the foreground at an interactive prompt. Calling straight through
	// fails with agent_pane_busy — observed against herdr 0.8.0, not
	// theorised, and it is the ordinary case rather than a race that
	// sometimes bites.
	if err := e.waitForShell(ctx, pane.ID); err != nil {
		return fmt.Errorf("engine: %s: %w", slot, err)
	}
	if err := e.startAgent(ctx, slot, sc, pane.ID); err != nil {
		return err
	}
	// Before anything is asked of it: put the harness into a state where the
	// answer will be work rather than a description of work.
	e.applyStartupKeys(ctx, slot, pane.ID)

	// Publish the mapping before returning, so a transition arriving in the
	// next moment can be attributed to this slot.
	if e.cfg.OnSpawn != nil {
		e.cfg.OnSpawn(slot, pane.ID)
	}
	release = false
	e.log.Info("slot spawned", "slot", slot, "kind", sc.Kind, "pane", pane.ID,
		HandoffEnv, e.HandoffPath(slot))
	return nil
}

// startAgent starts a slot's agent, retrying while herdr reports the pane is
// not yet an available shell.
//
// waitForShell narrows the window but cannot close it: it asks the kernel
// which process group holds the terminal, while herdr applies its own
// readiness test, and a shell can satisfy the first before the second. The
// result is a race that shows up as agent_pane_busy on a pane that looked
// ready — observed intermittently against herdr 0.8.0, succeeding and failing
// across otherwise identical runs.
//
// So the server is treated as the authority rather than predicted: ask, and
// retry while it says not yet. Only agent_pane_busy is retried — every other
// error is real and returned immediately, because retrying a missing binary or
// a bad kind just delays the report.
func (e *Engine) startAgent(ctx context.Context, slot string, sc SlotConfig, paneID string) error {
	deadline := time.Now().Add(ShellReadyTimeout)
	for attempt := 1; ; attempt++ {
		_, err := e.client.AgentStart(ctx, herdr.AgentStartParams{
			PaneID: paneID,
			Name:   slot,
			Kind:   sc.Kind,
			Args:   sc.Args,
		})
		if err == nil {
			if attempt > 1 {
				e.log.Debug("agent started after retry", "slot", slot, "attempts", attempt)
			}
			return nil
		}
		var apiErr *herdr.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "agent_pane_busy" {
			return fmt.Errorf("engine: agent.start %s: %w", slot, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("engine: agent.start %s: pane %s never became an available shell within %s: %w",
				slot, paneID, ShellReadyTimeout, err)
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ShellReadyTimeout bounds the wait for a freshly split pane's shell to reach
// its prompt. Generous: a cold shell with a heavy rc file is slow, and the
// cost of waiting too long is a slower spawn, while the cost of not waiting
// long enough is a failed run.
const ShellReadyTimeout = 15 * time.Second

// waitForShell blocks until a pane is an available shell, or the timeout
// expires.
//
// "Available" means the shell itself holds the foreground — no command,
// editor, or agent running — which is what agent.start requires. An empty
// foreground process list is exactly that condition, so pane.process_info
// answers the question directly rather than by guessing at a fixed sleep.
//
// A probe error is not fatal: if herdr cannot tell us, fall through and let
// agent.start be the authority. Its own error is clearer than one invented
// here.
func (e *Engine) waitForShell(ctx context.Context, paneID string) error {
	deadline := time.Now().Add(ShellReadyTimeout)
	for attempt := 0; ; attempt++ {
		info, err := e.client.PaneProcessInfo(ctx, paneID)
		if err != nil {
			e.log.Debug("shell readiness probe failed; proceeding to agent.start",
				"pane", paneID, "err", err)
			return nil
		}
		// The shell holding the foreground IS the available-shell condition.
		// Not "no foreground process": herdr lists the shell itself among
		// them, so that check never becomes true and every spawn times out.
		if info.ShellHoldsForeground() {
			if attempt > 0 {
				e.log.Debug("pane shell ready", "pane", paneID, "attempts", attempt+1)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pane %s never reached an interactive shell prompt within %s (foreground process %q still running)",
				paneID, ShellReadyTimeout, info.ForegroundProcesses[0].Name)
		}
		select {
		case <-time.After(150 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Release gives a slot's concurrency token back. Callers tearing a slot down
// must call it, or the kind's limit stays consumed for the rest of the run.
func (e *Engine) Release(slot string) { e.releaseKind(slot) }

func (e *Engine) kindConfig(kind string) KindConfig {
	kc := e.cfg.Kinds[kind]
	if kc.MaxConcurrent <= 0 {
		kc.MaxConcurrent = DefaultMaxConcurrent
	}
	return kc
}

func (e *Engine) acquireKind(ctx context.Context, slot, kind string) error {
	e.mu.Lock()
	if held, ok := e.held[slot]; ok {
		e.mu.Unlock()
		return fmt.Errorf("slot already holds a concurrency token for kind %q — release it before respawning", held)
	}
	gate, ok := e.kindGates[kind]
	if !ok {
		gate = make(chan struct{}, e.kindConfig(kind).MaxConcurrent)
		e.kindGates[kind] = gate
	}
	e.mu.Unlock()

	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	e.held[slot] = kind
	e.mu.Unlock()
	return nil
}

func (e *Engine) releaseKind(slot string) {
	e.mu.Lock()
	kind, ok := e.held[slot]
	if ok {
		delete(e.held, slot)
	}
	gate := e.kindGates[kind]
	e.mu.Unlock()
	if !ok || gate == nil {
		return
	}
	select {
	case <-gate:
	default:
	}
}

// handleBlocked applies the blocked policy (§4.3).
func (e *Engine) handleBlocked(ctx context.Context, tr Transition, status herdr.AgentStatus) (Outcome, bool) {
	prompt, seen := e.model.BlockedPrompt(tr.Slot)
	esc := Escalation{
		Slot:    tr.Slot,
		Status:  status,
		Prompt:  prompt,
		Stalled: stalledFor(tr),
		At:      time.Now(),
	}

	switch e.cfg.OnBlocked {
	case BlockedPause:
		esc.Reason = "slot blocked, loop paused"
		e.escalate(ctx, esc)
		e.halt(tr.Slot)
		return Outcome{Reason: ReasonPaused}, true

	case BlockedAuto:
		if !seen {
			// No visible prompt means nothing to match against. Not seeing a
			// prompt is not permission to answer it.
			esc.Reason = "slot blocked, prompt text not visible to the model"
			e.escalate(ctx, esc)
			e.halt(tr.Slot)
			return Outcome{}, false
		}
		br, matched := e.matchBlocked(prompt)
		if !matched {
			esc.Reason = "slot blocked on a prompt no blocked_rule matches"
			e.escalate(ctx, esc)
			e.halt(tr.Slot)
			return Outcome{}, false
		}
		target, ok := e.model.SlotTarget(tr.Slot)
		if !ok {
			esc.Reason = "slot blocked, no agent target to answer on"
			e.escalate(ctx, esc)
			e.halt(tr.Slot)
			return Outcome{}, false
		}
		lk, free := e.tryLockSlot(tr.Slot)
		if !free {
			// Something is already acting on this slot; answering an approval
			// dialog concurrently with it is exactly the race the per-slot
			// lock exists to prevent.
			esc.Reason = "slot blocked while another action was in flight"
			e.escalate(ctx, esc)
			e.halt(tr.Slot)
			return Outcome{}, false
		}
		defer lk.Unlock()
		e.log.Info("answering blocked prompt from an exact blocked_rule match",
			"slot", tr.Slot, "prompt", br.Prompt, "keys", br.Keys)
		if err := e.client.AgentSendKeys(ctx, target, br.Keys); err != nil {
			esc.Reason = "answering a matched blocked prompt failed"
			esc.Err = fmt.Errorf("engine: agent.send_keys %s: %w", tr.Slot, err)
			e.escalate(ctx, esc)
			e.halt(tr.Slot)
		}
		return Outcome{}, false

	default: // BlockedEscalate
		esc.Reason = "slot blocked, escalated"
		e.escalate(ctx, esc)
		e.halt(tr.Slot)
		return Outcome{}, false
	}
}

// matchBlocked compares whole strings, after trimming surrounding whitespace
// on both sides so a trailing newline in a captured prompt is not a mismatch.
//
// No regexp, no glob, no substring: the only prompt this loop may answer is one
// a human wrote out in full in the manifest.
func (e *Engine) matchBlocked(prompt string) (BlockedRule, bool) {
	want := strings.TrimSpace(prompt)
	for _, br := range e.cfg.BlockedRules {
		if strings.TrimSpace(br.Prompt) == want {
			return br, true
		}
	}
	return BlockedRule{}, false
}

func stalledFor(tr Transition) time.Duration {
	if tr.At.IsZero() {
		return 0
	}
	return time.Since(tr.At)
}

// escalate records a structured reason, surfaces it to the human, and logs it.
// A failed notification is logged and nothing more — escalation must not be
// able to escalate.
func (e *Engine) escalate(ctx context.Context, esc Escalation) {
	if esc.At.IsZero() {
		esc.At = time.Now()
	}
	e.mu.Lock()
	e.escalations = append(e.escalations, esc)
	e.mu.Unlock()

	e.log.Warn("escalation", "slot", esc.Slot, "rule", esc.Rule, "reason", esc.Reason,
		"status", esc.Status, "stalled", esc.Stalled, "err", esc.Err)

	body := esc.String()
	res, err := e.client.NotificationShow(ctx, herdr.NotificationShowParams{
		Title: "herdr-loop: " + e.cfg.Name,
		Body:  &body,
		Sound: herdr.NotificationSoundRequest,
	})
	switch {
	case err != nil:
		e.log.Error("escalation notification failed", "slot", esc.Slot, "err", err)
	case !res.Shown:
		// herdr suppresses notifications for several reasons; the log is the
		// backstop so an escalation is never invisible.
		e.log.Warn("escalation notification suppressed", "slot", esc.Slot, "reason", res.Reason)
	}
}

// halt stops the loop touching a slot. It does not kill anything: a halted slot
// keeps whatever work it has, because teardown must never destroy uncommitted
// work (§4.10).
func (e *Engine) halt(slot string) {
	e.mu.Lock()
	e.halted[slot] = true
	e.mu.Unlock()
}

// Halted reports whether a slot has been halted by policy.
func (e *Engine) Halted(slot string) bool { return e.isHalted(slot) }

// Iterations reports how much of the budget has been spent. Exported for the
// progress surface: "iteration 7 of 10" is the number that tells a watcher
// whether a loop is converging or about to hit its cap.
func (e *Engine) Iterations() int { return e.iterationCount() }

// EscalationCount reports how many escalations have been raised so far.
func (e *Engine) EscalationCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.escalations)
}

func (e *Engine) isHalted(slot string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.halted[slot]
}

// TryLockSlot exposes the per-slot mutex every rule action takes.
//
// It exists for the supervisor's bootstrap path: a slot's initial prompt is
// delivered outside the rule engine (it is a kickoff, not a handoff), but it
// still talks to the same agent's input widget and so must not run
// concurrently with a rule-driven prompt for that slot.
//
// Returns the mutex and whether it was acquired. The caller unlocks it. A
// false return means the slot is busy — do not act, the next settled
// transition will bring the caller back.
func (e *Engine) TryLockSlot(slot string) (*sync.Mutex, bool) { return e.tryLockSlot(slot) }

func (e *Engine) tryLockSlot(slot string) (*sync.Mutex, bool) {
	e.mu.Lock()
	lk, ok := e.locks[slot]
	if !ok {
		lk = &sync.Mutex{}
		e.locks[slot] = lk
	}
	e.mu.Unlock()
	return lk, lk.TryLock()
}

func (e *Engine) consumeIteration() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.iterations++
	return e.iterations
}

func (e *Engine) iterationCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.iterations
}

func (e *Engine) takeEscalations() []Escalation {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.escalations) == 0 {
		return nil
	}
	out := make([]Escalation, len(e.escalations))
	copy(out, e.escalations)
	return out
}

// facts is the read-only view one step evaluates against. Handoff files are
// read at most once per step so every predicate in a step sees the same file
// contents — a rule that fires on a verdict must not be racing a rewrite of the
// file it fired on.
type facts struct {
	e        *Engine
	slot     string
	status   herdr.AgentStatus
	handoffs map[string]*Handoff
	// overlay holds fields contributed by the action in progress — a finished
	// run action's stdout, stderr and exit_code. Checked before the general
	// resolvers so {{stdout}} means "the command that just ran" rather than
	// resolving to nothing.
	overlay map[string]string
}

func (e *Engine) newFacts(slot string, status herdr.AgentStatus) *facts {
	return &facts{e: e, slot: slot, status: status, handoffs: map[string]*Handoff{}}
}

// withOverlay returns a copy carrying extra template fields. A copy, not a
// mutation: the branch action's view must not leak back into the rule's.
func (f *facts) withOverlay(extra map[string]string) *facts {
	cp := *f
	cp.overlay = extra
	return &cp
}

func (e *Engine) holds(p Predicate, f *facts) bool {
	switch p.Op {
	case OpAll:
		for _, sub := range p.Filters {
			if !e.holds(sub, f) {
				return false
			}
		}
		return true
	case OpAny:
		for _, sub := range p.Filters {
			if e.holds(sub, f) {
				return true
			}
		}
		return false
	case OpNot:
		return !e.holds(p.Filters[0], f)
	case OpEq:
		v, ok := f.resolve(p.Field)
		return ok && v == p.Value
	case OpIn:
		v, ok := f.resolve(p.Field)
		if !ok {
			return false
		}
		for _, want := range p.Values {
			if v == want {
				return true
			}
		}
		return false
	case OpExists:
		v, ok := f.resolve(p.Field)
		return ok && v != ""
	}
	return false
}

// resolve maps a field name to a string. See Predicate for the grammar.
//
// An unresolvable field is false rather than an error: a handoff file that does
// not exist yet is the normal state before a slot's first result, and every
// operator treats false as "does not hold", which is the safe direction.
func (f *facts) resolve(field string) (string, bool) {
	if v, ok := f.overlay[field]; ok {
		return v, true
	}
	switch field {
	case "slot":
		return f.slot, true
	case "status":
		return string(f.status), true
	case "kind":
		return f.e.slots[f.slot].Kind, true
	case "tier":
		t, ok := f.e.model.SlotTier(f.slot)
		if !ok {
			return "", false
		}
		return string(t), true
	case "iteration":
		return strconv.Itoa(f.e.iterationCount()), true
	}

	head, rest, nested := strings.Cut(field, ".")
	if !nested {
		v, ok := f.e.cfg.Vars[field]
		return v, ok
	}

	if head == "slot" {
		name, attr, ok := strings.Cut(rest, ".")
		if !ok {
			return "", false
		}
		switch attr {
		case "status":
			s, ok := f.e.model.SlotStatus(name)
			return string(s), ok
		case "kind":
			sc, ok := f.e.slots[name]
			return sc.Kind, ok
		}
		return "", false
	}

	section, path, hasPath := strings.Cut(rest, ".")
	if section != "handoff" {
		return "", false
	}
	if _, known := f.e.slots[head]; !known {
		return "", false
	}
	if !hasPath {
		// The bare path resolves whether or not the file exists yet — telling
		// an agent where to write its result is the main use of it, and that
		// necessarily happens before anything is there.
		return f.e.HandoffPath(head), true
	}
	h, ok := f.handoff(head)
	if !ok {
		return "", false
	}
	return h.Lookup(path)
}

func (f *facts) handoff(slot string) (*Handoff, bool) {
	if h, cached := f.handoffs[slot]; cached {
		return h, h != nil
	}
	h, err := f.e.Handoff(slot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Expected before a slot has produced anything.
			f.e.log.Debug("no handoff file yet", "slot", slot, "path", f.e.HandoffPath(slot))
		} else {
			// Malformed front-matter is not expected and would otherwise make
			// every rule referencing it silently stop holding.
			f.e.log.Warn("handoff file unreadable", "slot", slot, "path", f.e.HandoffPath(slot), "err", err)
		}
		f.handoffs[slot] = nil
		return nil, false
	}
	f.handoffs[slot] = h
	return h, true
}

var placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.\-]+)\s*\}\}`)

// expand substitutes {{field}} using the same resolver the predicates use, so a
// manifest never has two ways to name the same thing.
//
// An unresolved placeholder is an error, not a passthrough: sending an agent a
// literal {{review.handoff}} is worse than sending nothing.
func (f *facts) expand(tmpl string) (string, error) {
	var missing []string
	out := placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		field := placeholder.FindStringSubmatch(m)[1]
		v, ok := f.resolve(field)
		if !ok {
			missing = append(missing, field)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unresolved template fields: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Handoff is a slot's result file: optional front-matter, then prose.
//
// Front-matter is what lets a rule say review.handoff.verdict instead of
// grepping a body an agent wrote freehand.
type Handoff struct {
	Path string
	// Fields is the decoded front-matter, empty when the file has none.
	Fields map[string]any
	Body   string
}

// ParseHandoff reads a result file, decoding YAML front-matter delimited by
// --- or TOML front-matter delimited by +++.
//
// The delimiter picks the format; there is no sniffing, because a file that
// parses as both under different rules is a footgun. A file with no
// front-matter is valid and yields an empty Fields.
func ParseHandoff(path string) (*Handoff, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("engine: read handoff %s: %w", path, err)
	}
	// Agents run on every platform herdr does; CRLF must not change whether
	// front-matter is recognised.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")

	h := &Handoff{Path: path, Fields: map[string]any{}}
	delim, front, body, ok := splitFrontMatter(text)
	if !ok {
		h.Body = text
		return h, nil
	}
	h.Body = body

	switch delim {
	case "---":
		if err := yaml.Unmarshal([]byte(front), &h.Fields); err != nil {
			return nil, fmt.Errorf("engine: handoff %s: decode YAML front-matter: %w", path, err)
		}
	case "+++":
		if _, err := toml.Decode(front, &h.Fields); err != nil {
			return nil, fmt.Errorf("engine: handoff %s: decode TOML front-matter: %w", path, err)
		}
	}
	if h.Fields == nil {
		h.Fields = map[string]any{}
	}
	return h, nil
}

// splitFrontMatter recognises front-matter only at byte zero, the way every
// other tool that reads it does.
func splitFrontMatter(text string) (delim, front, body string, ok bool) {
	for _, d := range []string{"---", "+++"} {
		open := d + "\n"
		if !strings.HasPrefix(text, open) {
			continue
		}
		rest := text[len(open):]
		closer := "\n" + d
		i := strings.Index(rest, closer)
		if i < 0 {
			// An unterminated opener is not front-matter; treat the whole
			// file as body rather than swallowing it.
			return "", "", "", false
		}
		body = strings.TrimPrefix(rest[i+len(closer):], "\n")
		return d, rest[:i], body, true
	}
	return "", "", "", false
}

// Lookup resolves a dotted path into the front-matter and renders the value as
// a string. Non-scalar values (a nested table, a list) report false — a rule
// compares strings, and stringifying a map would compare formatting.
func (h *Handoff) Lookup(path string) (string, bool) {
	if h == nil {
		return "", false
	}
	var cur any = h.Fields
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[seg]
		if !ok {
			return "", false
		}
	}
	return scalarString(cur)
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(t), true
	case time.Time:
		return t.Format(time.RFC3339), true
	}
	return "", false
}
