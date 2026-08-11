package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cyperx84/herdr-loop/internal/engine"
	"github.com/cyperx84/herdr-loop/internal/manifest"
)

// defaultMaxIterations bounds a loop the manifest itself left unbounded.
//
// manifest.Parse accepts loop.max_iterations = 0 when the rule graph is
// acyclic (nothing to cap — see manifest.validateCycles), but engine.New
// treats 0 as invalid: every loop it runs carries a budget, no exceptions.
// A manifest reaching this default is by construction acyclic — a cyclic one
// would have been rejected at Parse unless max_iterations or a per-rule
// timeout already covered it — so this rarely binds; it exists to satisfy
// engine.New's invariant, not to silently cap a manifest its author left
// uncapped on purpose.
const defaultMaxIterations = 100

// defaultHandoffDir mirrors the value PLAN.md §3's own manifest sketch uses.
const defaultHandoffDir = ".herdr-loop/handoff"

// worktreeRequest is a slot mapManifest could not give a cwd, because
// manifest.Slot.Worktree names a branch/base pair that only becomes a real
// checkout path once worktree.create runs against a live herdr server.
// Resolving these is run's job (see resolveWorktrees), kept out of
// mapManifest so the mapper stays a pure function testable with no
// connection — worktree.create is the one part of "load this manifest" that
// genuinely needs one, and isolating it to a single call site means validate
// and doctor work against a manifest with worktree slots without dialing
// herdr at all.
type worktreeRequest struct {
	SlotIndex int // index into mapResult.Config.Slots
	Branch    string
	Base      string
}

// mapResult is mapManifest's output: an engine.Config plus the two things the
// engine itself has no field for and that run must handle before or after
// engine.New (see the doc comments on Worktrees and InitialPrompts).
type mapResult struct {
	Config engine.Config

	Worktrees []worktreeRequest

	// InitialPrompts is manifest.Slot.Prompt, keyed by slot name, for slots
	// that set one. engine.SlotConfig carries no prompt field: rule-driven
	// prompts (engine.PromptAction) exist for slot-to-slot handoff after a
	// loop is already running, never for kicking a freshly spawned agent off
	// — PLAN.md §3's own worked example only ever prompts an existing slot.
	// Delivering these is therefore run's bootstrap step, done once per slot
	// the same way Spawn is, not something engine.Config can express.
	InitialPrompts map[string]string
}

// mapManifest translates a parsed, already-validated loop.toml onto the
// engine's Config shape.
//
// manifest and engine independently define the loop/slot/rule model (PLAN.md
// §3) — manifest owns the TOML surface and its own validation (cycle
// detection, cwd/worktree exclusivity, blocked-rule shape), engine owns
// runtime semantics — and the two shapes do not line up field for field. This
// function is the seam. It returns an error for anything manifest.Parse
// accepts that the engine cannot yet execute, rather than silently dropping
// a rule the manifest author wrote in good faith.
func mapManifest(m *manifest.Manifest) (mapResult, error) {
	cfg := engine.Config{
		Name:          m.Loop.Name,
		MaxIterations: m.Loop.MaxIterations,
		HandoffDir:    m.Loop.HandoffDir,
		OnBlocked:     engine.BlockedPolicy(m.Loop.OnBlocked),
		Strict:        m.Loop.Strict,
		Vars:          map[string]string{},
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = defaultMaxIterations
	}
	if cfg.HandoffDir == "" {
		cfg.HandoffDir = defaultHandoffDir
	}

	res := mapResult{InitialPrompts: map[string]string{}}

	for _, s := range m.Slots {
		idx := len(cfg.Slots)
		cfg.Slots = append(cfg.Slots, engine.SlotConfig{
			Name: s.Name,
			Kind: s.Kind,
			CWD:  s.CWD, // left empty for a worktree slot; resolveWorktrees fills it in
		})
		if s.Worktree != nil {
			res.Worktrees = append(res.Worktrees, worktreeRequest{
				SlotIndex: idx,
				Branch:    s.Worktree.Branch,
				Base:      s.Worktree.Base,
			})
		}
		if s.Prompt != "" {
			res.InitialPrompts[s.Name] = s.Prompt
		}
	}

	for _, br := range m.BlockedRules {
		cfg.BlockedRules = append(cfg.BlockedRules, convertBlockedRule(br))
	}

	for i, r := range m.Rules {
		action, err := convertAction(r.Then)
		if err != nil {
			return mapResult{}, fmt.Errorf("mapManifest: rule %d: %w", i, err)
		}
		cfg.Rules = append(cfg.Rules, engine.Rule{
			Name: ruleName(i, r),
			When: convertPredicate(r.When),
			Then: action,
		})
	}

	res.Config = cfg
	return res, nil
}

// ruleName synthesizes the identifier engine.Rule requires but manifest.Rule
// has no field for. Deterministic and index-based, so the same manifest text
// always produces the same name — escalations (PLAN §4.11) report this, and
// a name that moved between two runs of the same manifest would make an
// escalation log misleading to read back.
func ruleName(i int, r manifest.Rule) string {
	return fmt.Sprintf("rule-%d-%s", i, actionKind(r.Then))
}

func actionKind(a manifest.Action) string {
	switch {
	case a.Prompt != nil:
		return "prompt-" + a.Prompt.Slot
	case a.Finish != "":
		return "finish"
	case a.Run != nil:
		return "run"
	case a.Escalate:
		return "escalate"
	default:
		return "unset"
	}
}

// convertAction maps a manifest action onto engine.Action, rejecting the two
// variants the engine has no way to execute.
//
// manifest.Action supports run (arbitrary command, branch on exit code) and
// escalate; engine.Action supports only Prompt, Spawn, and Finish (see
// engine.go's Action doc — Spawn has no manifest-level surface either, it is
// only ever used by run's own initial-slot bootstrap). A manifest using
// run/escalate parses and validates cleanly against manifest.Parse's own
// rules, because that package validates TOML shape, not engine capability.
// Refusing here — loudly, at validate time, before anything runs — is
// better than the alternative of quietly never firing that rule.
func convertAction(a manifest.Action) (engine.Action, error) {
	switch {
	case a.Prompt != nil:
		return engine.Action{Prompt: &engine.PromptAction{
			Slot: a.Prompt.Slot,
			Text: a.Prompt.Text,
		}}, nil
	case a.Finish != "":
		return engine.Action{Finish: &engine.FinishAction{Reason: a.Finish}}, nil
	case a.Run != nil:
		run := &engine.RunAction{Argv: a.Run}
		if a.OnSuccess != nil {
			branch, err := convertAction(*a.OnSuccess)
			if err != nil {
				return engine.Action{}, fmt.Errorf("on_success: %w", err)
			}
			run.OnSuccess = &branch
		}
		if a.OnFailure != nil {
			branch, err := convertAction(*a.OnFailure)
			if err != nil {
				return engine.Action{}, fmt.Errorf("on_failure: %w", err)
			}
			run.OnFailure = &branch
		}
		return engine.Action{Run: run}, nil
	case a.Escalate:
		return engine.Action{}, errors.New(
			"action escalate: the engine has no escalate action yet (tracked as an M2 gap) — remove this rule or wait for engine support")
	default:
		return engine.Action{}, errors.New("action sets none of prompt/run/finish/escalate")
	}
}

// convertPredicate maps a manifest predicate onto engine.Predicate.
//
// The two grammars agree on op names and on the field="slot" idiom (both
// resolve it against "the slot this predicate is being evaluated for" — see
// engine's facts.resolve and manifest's own §3 example), so this is a
// structural copy, not a semantic translation. The one real difference is
// typing: manifest.Predicate.Value/Values are `any` (TOML strings, integers,
// floats, booleans all decode as themselves), engine.Predicate compares
// strings only. scalarString renders each to its string form so a manifest
// author can write `value = true` and have it compare equal to an agent's
// `true` string front-matter field, matching engine's own documented
// all-comparisons-are-string-comparisons rule.
func convertPredicate(p manifest.Predicate) engine.Predicate {
	out := engine.Predicate{
		Op:    engine.PredicateOp(p.Op),
		Field: p.Field,
	}
	if p.Value != nil {
		out.Value = scalarString(p.Value)
	}
	for _, v := range p.Values {
		out.Values = append(out.Values, scalarString(v))
	}
	for _, f := range p.Filters {
		out.Filters = append(out.Filters, convertPredicate(f))
	}
	if p.Filter != nil {
		// manifest's "not" takes a single named Filter field; engine's takes
		// a one-element Filters slice (see engine.validatePredicate's OpNot
		// case). Same grammar, different Go shape.
		out.Filters = append(out.Filters, convertPredicate(*p.Filter))
	}
	return out
}

func scalarString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// convertBlockedRule maps one manifest [[blocked_rule]] onto engine.BlockedRule.
//
// manifest.BlockedRule.Send is a single TOML string; engine.BlockedRule.Keys
// is []string, sent verbatim to agent.send_keys (herdr's PaneSendKeys takes
// one key token per slice element — "enter", "y", "tab", …). Splitting Send
// on whitespace lets a manifest author write `send = "y enter"` for a
// two-key answer while `send = "enter"` still produces the common
// single-key case. This is a manifest-authoring convention this package
// defines, not something manifest.go's own validation enforces (it only
// checks Send/Pattern are non-empty and wildcard-free) — documented here
// because this is the one place the convention takes effect.
func convertBlockedRule(br manifest.BlockedRule) engine.BlockedRule {
	return engine.BlockedRule{
		Prompt: br.Pattern,
		Keys:   strings.Fields(br.Send),
	}
}
