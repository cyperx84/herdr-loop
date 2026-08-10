package main

import (
	"context"
	"errors"
	"io"
	"log/slog"

	herdr "github.com/cyperx84/herdr-api"
	"github.com/cyperx84/herdr-loop/internal/engine"
)

// errNoConnection is returned by every noopHerdr method. validate never
// calls one of these methods — engine.New only stores its client and model,
// it does not exercise them — but a stub that errors loudly beats one that
// silently pretends to succeed, in case that assumption ever stops holding.
var errNoConnection = errors.New("herdr-loop: validate: no live connection (validate never dials herdr)")

// noopHerdr and noopModel satisfy engine.Herdr and engine.Model with no
// socket, so `validate` can run engine.New's own construction-time checks —
// rule names, slot references, blocked_rule shape — without a live
// connection. That is the whole point of a separate validate command: catch
// what run would refuse to start with, before anything is running.
type noopHerdr struct{}

func (noopHerdr) AgentPrompt(context.Context, string, string, *herdr.AgentPromptWaitOptions) (herdr.Agent, error) {
	return herdr.Agent{}, errNoConnection
}
func (noopHerdr) AgentSendKeys(context.Context, string, []string) error { return errNoConnection }
func (noopHerdr) AgentStart(context.Context, herdr.AgentStartParams) (herdr.AgentStartResult, error) {
	return herdr.AgentStartResult{}, errNoConnection
}
func (noopHerdr) PaneSplit(context.Context, herdr.PaneSplitParams) (herdr.Pane, error) {
	return herdr.Pane{}, errNoConnection
}
func (noopHerdr) NotificationShow(context.Context, herdr.NotificationShowParams) (herdr.NotificationShowResult, error) {
	return herdr.NotificationShowResult{}, errNoConnection
}

var _ engine.Herdr = noopHerdr{}

type noopModel struct{}

func (noopModel) SlotStatus(string) (herdr.AgentStatus, bool) { return "", false }
func (noopModel) SlotTarget(string) (string, bool)            { return "", false }
func (noopModel) BlockedPrompt(string) (string, bool)         { return "", false }

var _ engine.Model = noopModel{}

// discardLogger is what validate hands engine.New — validate reports success
// or failure on stdout itself; the engine's own structured logging has
// nothing to say about a construction check that never runs a loop.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
