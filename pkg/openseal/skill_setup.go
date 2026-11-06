package openseal

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"slices"
	"time"
)

type SkillSetupRequest = runtime.SkillSetupRequest
type SkillSetupPhase = runtime.SkillSetupPhase
type SkillSetupValidationError = runtime.SkillSetupValidationError

const SkillSetupPhaseConfiguration = runtime.SkillSetupPhaseConfiguration
const SkillSetupPhaseBindingUpgrade = runtime.SkillSetupPhaseBindingUpgrade

type SkillSetupResolutionReceipt = runtime.SkillSetupResolutionReceipt

const SkillActionRequestSetup = runtime.SkillActionRequestSetup
const SkillActionListSetupRequests = runtime.SkillActionListSetupRequests
const ConversationTaskSkillSetupWakeType = runtime.ConversationTaskSkillSetupWakeType

var ErrSkillSetupConflict = runtime.ErrSkillSetupConflict
var ErrInvalidSkillSetup = runtime.ErrInvalidSkillSetup

// RebaseSkillSetupRequestAfterBindingUpgrade verifies durable migration proof
// without completing the separate configuration or reauthorization step.
func RebaseSkillSetupRequestAfterBindingUpgrade(request *SkillSetupRequest, expected int64, binding *skill.Binding) (*SkillSetupRequest, error) {
	return runtime.RebaseSkillSetupRequestAfterBindingUpgrade(request, expected, binding)
}

// ReconcileSkillSetupTask delivers a saved setup result to the exact background
// task. A handled result must not also schedule a foreground conversation reply.
func (e *Engine) ReconcileSkillSetupTask(ctx context.Context, scope runtime.Scope, requestID string) (*runtime.AgentRunCommandResult, bool, error) {
	if e == nil || e.conversationRunScheduler == nil {
		return nil, false, errors.New("conversation run scheduling is unavailable")
	}
	result, handled, err := e.conversationRunScheduler.ReconcileSkillSetupTask(ctx, scope, requestID)
	if err == nil && result != nil && result.Event != nil {
		e.wakeAgentWorkersForScope(scope)
	}
	return result, handled, err
}

// ListSkillSetupRequests lists a conversation's setup requests. owner must
// own the conversation: every request there is saved to it.
func (e *Engine) ListSkillSetupRequests(ctx context.Context, scope runtime.Scope, owner ObjectiveOwner, conversationID string) ([]*runtime.SkillSetupRequest, error) {
	if err := e.conversationOwnedBy(ctx, scope, owner, conversationID); err != nil {
		return nil, err
	}
	return e.reconciledSkillSetupRequests(ctx, scope, owner.ID, conversationID)
}

// reconciledSkillSetupRequests lists one conversation's requests after
// resolving any pending request that a saved binding already satisfies.
// A validated binding saved in Settings or an OAuth callback is the same
// evidence as one saved in the chat form. Reconcile from canonical state so
// closing a tab or losing a completion response cannot strand the request.
func (e *Engine) reconciledSkillSetupRequests(ctx context.Context, scope runtime.Scope, deploymentID, conversationID string) ([]*runtime.SkillSetupRequest, error) {
	requests, err := e.store.ListSkillSetupRequests(ctx, scope, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(requests, skillSetupReconcilable) {
		return requests, nil
	}
	bindings, err := e.skills.ListBindings(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID)
	if err != nil {
		return nil, err
	}
	for index, request := range requests {
		if !skillSetupReconcilable(request) {
			continue
		}
		for _, binding := range bindings {
			if !skillSetupBindingCandidate(request, binding) {
				continue
			}
			resolved, resolveErr := e.ResolveSkillSetupRequest(ctx, scope, request.Owner, request.ID, request.Revision, binding.ID, InteractionResolver{System: skillSetupReconciliationActor}, false)
			if resolveErr == nil {
				requests[index] = resolved
				break
			}
			if errors.Is(resolveErr, runtime.ErrSkillSetupConflict) {
				latest, readErr := e.store.GetSkillSetupRequest(ctx, scope, request.ID)
				if readErr != nil {
					return nil, readErr
				}
				if latest != nil {
					requests[index] = latest
				}
				break
			}
		}
	}
	return requests, nil
}

const skillSetupReconciliationActor = "system:binding-reconciliation"

func skillSetupReconcilable(request *runtime.SkillSetupRequest) bool {
	return request != nil && request.Status == "pending" && request.Phase != runtime.SkillSetupPhaseBindingUpgrade &&
		request.Kind != runtime.SkillSetupKindConnectApp && request.Kind != runtime.SkillSetupKindConnectAccount
}

// skillSetupBindingCandidate reports whether a binding is a save made for
// this request. ResolveSkillSetupRequest still checks the requested access.
func skillSetupBindingCandidate(request *runtime.SkillSetupRequest, binding *skill.Binding) bool {
	if binding == nil || binding.Disabled || binding.SkillID != request.SkillID || binding.SkillVersion != request.SkillVersion ||
		binding.SourceIdentity != request.SourceIdentity || binding.Revision <= request.BindingRevision ||
		(request.BindingID != "" && binding.ID != request.BindingID) {
		return false
	}
	return request.BindingID != "" || !binding.CreatedAt.Before(request.CreatedAt)
}

// reconcileSkillSetupRequestsForBinding resolves the pending setup requests
// of the binding's umbrella that a just-saved binding completes, wherever it
// was saved.
// The saved binding is authoritative; failures leave the read-side
// reconciliation to retry.
func (e *Engine) reconcileSkillSetupRequestsForBinding(ctx context.Context, binding *skill.Binding) {
	pending, ok := e.store.(interface {
		ListPendingSkillSetupRequests(context.Context, runtime.Scope, int, int) ([]*runtime.SkillSetupRequest, error)
	})
	if !ok || binding == nil || binding.Disabled {
		return
	}
	scope := runtime.Scope{Kind: binding.Scope.Kind, ID: binding.Scope.ID}
	if scope.Validate() != nil {
		return
	}
	const pageSize, maximumPages = 100, 20
	var matches []*runtime.SkillSetupRequest
	for page := 0; page < maximumPages; page++ {
		requests, err := pending.ListPendingSkillSetupRequests(ctx, scope, pageSize, page*pageSize)
		if err != nil {
			return
		}
		for _, request := range requests {
			if request.Owner.ID == binding.DeploymentID && skillSetupReconcilable(request) && skillSetupBindingCandidate(request, binding) {
				matches = append(matches, request)
			}
		}
		if len(requests) < pageSize {
			break
		}
	}
	for _, request := range matches {
		resolved, err := e.ResolveSkillSetupRequest(ctx, scope, request.Owner, request.ID, request.Revision, binding.ID, InteractionResolver{System: skillSetupReconciliationActor}, false)
		if err == nil && resolved.Status == "resolved" {
			_, _, _ = e.ReconcileSkillSetupTask(ctx, scope, resolved.ID)
		}
	}
}

// GetSkillSetupRequest returns request id when it is saved to owner.
func (e *Engine) GetSkillSetupRequest(ctx context.Context, scope runtime.Scope, owner ObjectiveOwner, id string) (*runtime.SkillSetupRequest, error) {
	r, err := e.store.GetSkillSetupRequest(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if r == nil || r.Owner != owner || r.DeploymentID != owner.ID {
		return nil, runtime.ErrInvalidSkillSetup
	}
	return r, nil
}

// ResolveSkillSetupRequest accepts evidence of a successful, separately
// authorized binding write. A click or an OAuth popup closing is not completion.
// A setup card is settled by a manager of its umbrella (or a system
// reconciliation); connect cards follow their own authority. Hosts authorize
// the exact target before calling this method.
func (e *Engine) ResolveSkillSetupRequest(ctx context.Context, scope runtime.Scope, owner ObjectiveOwner, id string, expected int64, bindingID string, resolver InteractionResolver, dismiss bool) (*runtime.SkillSetupRequest, error) {
	r, err := e.GetSkillSetupRequest(ctx, scope, owner, id)
	if err != nil {
		return nil, err
	}
	actor, err := resolver.actor()
	if err != nil {
		return nil, err
	}
	// A connect card is completed by a connection, not a saved binding.
	switch r.Kind {
	case runtime.SkillSetupKindConnectApp:
		resolution, resolveErr := e.ResolveConnectAppRequest(ctx, scope, owner, id, expected, resolver, dismiss)
		if resolveErr != nil {
			return nil, resolveErr
		}
		return resolution.Request, nil
	case runtime.SkillSetupKindConnectAccount:
		return e.ResolveConnectAccountRequest(ctx, scope, owner, id, expected, resolver, dismiss)
	}
	if err := authorizeManagerInteraction(resolver); err != nil {
		return nil, err
	}
	if r.Status != "pending" {
		if (dismiss && r.Status == "dismissed") || (!dismiss && r.Status == "resolved" && r.ResolvedBindingID == bindingID) {
			return r, nil
		}
		return nil, runtime.ErrSkillSetupConflict
	}
	if r.Revision != expected {
		return nil, runtime.ErrSkillSetupConflict
	}
	if dismiss {
		r.Status = "dismissed"
	} else {
		if r.Phase == runtime.SkillSetupPhaseBindingUpgrade {
			return nil, errors.New("the requested Skill binding upgrade must be completed before saving configuration")
		}
		binding, err := e.skills.GetBinding(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, bindingID)
		if err != nil {
			return nil, err
		}
		if binding == nil || binding.Disabled || binding.SkillID != r.SkillID || binding.SkillVersion != r.SkillVersion || binding.SourceIdentity != r.SourceIdentity || (r.BindingID != "" && r.BindingID != binding.ID) || binding.Revision <= r.BindingRevision {
			return nil, errors.New("the requested Skill configuration has not been saved")
		}
		for _, action := range r.RequiredActions {
			found := false
			for _, allowed := range binding.AllowedActions {
				if allowed == action {
					found = true
					break
				}
			}
			if !found {
				return nil, errors.New("requested Skill actions have not been enabled")
			}
		}
		if r.EnablePrompt && !binding.EnablePrompt {
			return nil, errors.New("requested Skill instructions have not been enabled")
		}
		r.Status = "resolved"
		r.ResolvedBindingID = binding.ID
		r.ResolvedBindingRevision = binding.Revision
	}
	r.ResolvedBy = actor
	r.Revision++
	r.UpdatedAt = time.Now().UTC()
	if err = e.store.SaveSkillSetupRequest(ctx, r, expected); err != nil {
		return nil, err
	}
	return r, nil
}
