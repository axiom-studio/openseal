package openseal

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type (
	AgentBuilderCutoverHost  = runtime.AgentBuilderCutoverHost
	AgentBuilderCutoverDraft = runtime.AgentBuilderCutoverDraft
)

// AgentBuilderCutoverReport counts what one cutover pass did.
type AgentBuilderCutoverReport struct {
	// Converted drafts became builder conversations and left the table.
	Converted int
	// Kept drafts stay for the next Engine.Start.
	Kept int
	// Dropped reports that this pass dropped the empty cutover table and
	// called AfterAgentBuilderCutover.
	Dropped bool
}

// WithAgentBuilderCutoverHost lets Engine.Start convert the drafts migration
// 76 kept (D12). Without it the drafts stay in place.
func WithAgentBuilderCutoverHost(host AgentBuilderCutoverHost) Option {
	return func(e *Engine) error {
		if host == nil {
			return errors.New("agent builder cutover host is required")
		}
		e.agentBuilderCutoverHost = host
		return nil
	}
}

// AgentBuilderCutover turns each change-set draft migration 76 kept into a
// builder conversation (D12, spec §3.6). Engine.Start is its only caller.
// Per draft: the builder is provisioned through the cutover host when it is
// missing, the draft's billing model is read, the conversation is started
// with the typed request as content and name, model and drafted profile as
// builder parameters (D7), and the draft is deleted. A draft that cannot be
// converted is kept for the next start, never dropped. Once no draft is
// left the table is dropped and AfterAgentBuilderCutover runs, once.
//
// Store errors are returned and Start fails closed; a draft's own failure
// is only logged.
func (e *Engine) AgentBuilderCutover(ctx context.Context) (AgentBuilderCutoverReport, error) {
	var report AgentBuilderCutoverReport
	if e == nil || e.store == nil {
		return report, errors.New("agent builder cutover: engine store is not configured")
	}
	store, ok := e.store.(runtime.AgentBuilderCutoverStore)
	if !ok {
		return report, nil
	}
	drafts, err := store.ListAgentBuilderCutoverDrafts(ctx)
	if err != nil {
		return report, fmt.Errorf("agent builder cutover: %w", err)
	}
	host := e.agentBuilderCutoverHost
	if host == nil {
		if len(drafts) > 0 {
			report.Kept = len(drafts)
			e.logCutoverWarning("agent builder cutover: no cutover host; drafts kept", "drafts", len(drafts))
		}
		return report, nil
	}
	for _, draft := range drafts {
		if err := e.convertAgentBuilderCutoverDraft(ctx, host, draft); err != nil {
			report.Kept++
			e.logCutoverWarning("agent builder cutover: draft kept", "scopeKind", draft.Scope.Kind, "scopeId", draft.Scope.ID, "draft", draft.ID, "error", err)
			continue
		}
		if err := store.DeleteAgentBuilderCutoverDraft(ctx, draft.Scope, draft.ID); err != nil {
			return report, fmt.Errorf("agent builder cutover: delete draft %s: %w", draft.ID, err)
		}
		report.Converted++
	}
	if report.Kept > 0 {
		return report, nil
	}
	dropped, err := store.DropAgentBuilderCutoverTable(ctx)
	if err != nil {
		return report, fmt.Errorf("agent builder cutover: %w", err)
	}
	if !dropped {
		return report, nil
	}
	report.Dropped = true
	// The table is gone, so this runs once; its failure is the host's to
	// repair and must not stop the kernel.
	if err := host.AfterAgentBuilderCutover(ctx); err != nil {
		e.logCutoverWarning("agent builder cutover: after-cutover host step failed", "error", err)
	}
	return report, nil
}

func (e *Engine) convertAgentBuilderCutoverDraft(ctx context.Context, host AgentBuilderCutoverHost, draft AgentBuilderCutoverDraft) error {
	if !draft.Readable {
		return errors.New("the draft payload is unreadable")
	}
	actor := strings.TrimSpace(draft.ActorID)
	if actor == "" {
		return errors.New("the draft has no actor")
	}
	present, err := e.agentBuilderPresent(ctx, draft.Scope)
	if err != nil {
		return err
	}
	if !present {
		if err := host.EnsureBuilderAgent(ctx, draft.Scope, actor); err != nil {
			return fmt.Errorf("ensure builder agent: %w", err)
		}
		if present, err = e.agentBuilderPresent(ctx, draft.Scope); err != nil {
			return err
		} else if !present {
			return errors.New("the builder agent is still missing after EnsureBuilderAgent")
		}
	}
	// Read before AfterAgentBuilderCutover lets the host delete its rows.
	model, err := host.DraftModel(ctx, draft.Scope, draft.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("draft model: %w", err)
	}
	spaceID, err := e.agentBuilderCutoverSpace(ctx, draft)
	if err != nil {
		return err
	}
	if _, err := e.StartAgentBuilderConversation(ctx, draft.StartRequest(model, spaceID)); err != nil {
		return fmt.Errorf("start builder conversation: %w", err)
	}
	return nil
}

// agentBuilderPresent reports a non-retired builder deployment in scope.
func (e *Engine) agentBuilderPresent(ctx context.Context, scope Scope) (bool, error) {
	if e.agents == nil {
		return false, errors.New("agent registry is not configured")
	}
	deployment, err := e.agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, AgentBuilderDeploymentID)
	if errors.Is(err, kernelagent.ErrDeploymentNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return deployment.RolloutStatus != kernelagent.RolloutRetired, nil
}

// agentBuilderCutoverSpace is the space of the first existing deployment the
// draft's placement names, else "" (the builder's space or Personal).
func (e *Engine) agentBuilderCutoverSpace(ctx context.Context, draft AgentBuilderCutoverDraft) (string, error) {
	if e.spaces == nil || e.agents == nil || len(draft.Placement.AgentDeploymentIDs) == 0 {
		return "", nil
	}
	ids := make([]string, 0, len(draft.Placement.AgentDeploymentIDs))
	for _, id := range draft.Placement.AgentDeploymentIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	scopeRef := capability.ScopeReference{Kind: draft.Scope.Kind, ID: draft.Scope.ID}
	var spaces []*runtime.Space
	for _, id := range ids {
		if _, err := e.agents.GetDeployment(ctx, scopeRef, id); errors.Is(err, kernelagent.ErrDeploymentNotFound) {
			continue
		} else if err != nil {
			return "", err
		}
		if spaces == nil {
			listed, err := e.spaces.ListSpaces(ctx, draft.Scope)
			if err != nil {
				return "", err
			}
			spaces = listed
		}
		for _, space := range spaces {
			members, err := e.spaces.ListSpaceMembers(ctx, draft.Scope, space.ID)
			if err != nil {
				return "", err
			}
			for _, member := range members {
				if member.Type == runtime.SpaceMemberAgent && member.ID == id {
					return space.ID, nil
				}
			}
		}
	}
	return "", nil
}

func (e *Engine) logCutoverWarning(message string, fields ...interface{}) {
	if e.logger != nil {
		e.logger.Warnw(message, fields...)
	}
}
