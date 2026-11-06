package openseal

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// ErrConnectAccountNotSatisfied means the account a connect_account card asks
// for is still not connected for the run's umbrella and person.
var ErrConnectAccountNotSatisfied = errors.New("Connect the account first.")

// ErrInteractionNotAuthorized means the resolver may not settle the in-chat
// request: it is addressed to another person, or needs a manager.
var ErrInteractionNotAuthorized = errors.New("you cannot answer this request")

// InteractionResolver is who settles an in-chat request. UserID is the
// authenticated caller and CanManage its host-verified right to manage the
// request's umbrella. System names a kernel or host reconciliation (for
// example "system:connection-reconciliation") and excludes UserID.
type InteractionResolver struct {
	UserID    string
	CanManage bool
	System    string
}

// actor is the ResolvedBy of a settlement by r.
func (r InteractionResolver) actor() (string, error) {
	user, system := strings.TrimSpace(r.UserID), strings.TrimSpace(r.System)
	if (user == "") == (system == "") || user != r.UserID || system != r.System {
		return "", runtime.ErrInvalidSkillSetup
	}
	if system != "" {
		return system, nil
	}
	return user, nil
}

// authorizeManagerInteraction allows managers and system reconciliation:
// setup, credential, shared and not-yet-enabled app requests.
func authorizeManagerInteraction(resolver InteractionResolver) error {
	if resolver.System != "" || resolver.CanManage {
		return nil
	}
	return ErrInteractionNotAuthorized
}

// authorizePersonInteraction allows the person a per_person request is for to
// resolve it, and that person or a manager to dismiss it.
func authorizePersonInteraction(resolver InteractionResolver, requestedFor string, dismiss bool) error {
	switch {
	case resolver.System != "":
		return nil
	case requestedFor != "" && resolver.UserID == requestedFor:
		return nil
	case dismiss && resolver.CanManage:
		return nil
	}
	return ErrInteractionNotAuthorized
}

// authorizeSkillSetupInteraction applies the resolution authority of a
// connect_account or connect_app request by its mode.
func authorizeSkillSetupInteraction(resolver InteractionResolver, r *runtime.SkillSetupRequest, dismiss bool) error {
	if r.Mode == "per_person" {
		return authorizePersonInteraction(resolver, r.RequestedFor, dismiss)
	}
	return authorizeManagerInteraction(resolver)
}

const connectAccountReconciliationActor = "system:connection-reconciliation"

// ResolveConnectAccountRequest settles a connect_account card. Resolving
// needs evidence: the card's binding is current and enabled, and the host's
// credential check reports no gap for its slots. Dismissing needs none.
// Either way the waiting run (and every sibling card's run) resumes.
func (e *Engine) ResolveConnectAccountRequest(ctx context.Context, scope runtime.Scope, owner ObjectiveOwner, id string, expected int64, resolver InteractionResolver, dismiss bool) (*runtime.SkillSetupRequest, error) {
	actor, err := resolver.actor()
	if err != nil {
		return nil, err
	}
	r, err := e.GetSkillSetupRequest(ctx, scope, owner, id)
	if err != nil {
		return nil, err
	}
	if r.Kind != runtime.SkillSetupKindConnectAccount {
		return nil, runtime.ErrInvalidSkillSetup
	}
	if err := authorizeSkillSetupInteraction(resolver, r, dismiss); err != nil {
		return nil, err
	}
	want := "resolved"
	if dismiss {
		want = "dismissed"
	}
	if r.Status == "pending" {
		if r.Revision != expected {
			return nil, runtime.ErrSkillSetupConflict
		}
		if !dismiss {
			binding, satisfied, checkErr := e.connectAccountSatisfied(ctx, r)
			if checkErr != nil {
				return nil, checkErr
			}
			if !satisfied {
				return nil, ErrConnectAccountNotSatisfied
			}
			r.ResolvedBindingID, r.ResolvedBindingRevision = binding.ID, binding.Revision
		}
		r.Status, r.ResolvedBy = want, actor
		r.Revision++
		r.UpdatedAt = time.Now().UTC()
		if err := e.store.SaveSkillSetupRequest(ctx, r, expected); err != nil {
			return nil, err
		}
	} else if r.Status != want {
		return nil, runtime.ErrSkillSetupConflict
	}
	e.settleSkillSetupSiblings(ctx, r, e.continueConnectAccountRun)
	if _, err := e.continueConnectAccountRun(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (e *Engine) continueConnectAccountRun(ctx context.Context, r *runtime.SkillSetupRequest) (bool, error) {
	return e.continueInteractionRun(ctx, interactionContinuation{
		scope: r.Scope, owner: r.Owner, agentID: r.AgentID, runID: r.RunID, requestID: r.ID, actionCallID: r.ActionCallID, status: r.Status, revision: r.Revision,
		updatedAt: r.UpdatedAt, wakeType: runtime.ConnectAccountWakeType, coordinator: "connect-account-coordinator",
		eventType: "run.connection_request_settled", summary: "Received the account connection result; continuing",
		interventionPrefix: "connect-account-result:", instruction: runtime.ConnectAccountResolutionInstruction(r),
		requestedFor: r.RequestedFor,
	})
}

// connectAccountSatisfied checks a card's evidence against the current
// binding: the person a per_person card is for (or the run's person for a
// shared one) must have every slot available now. A binding whose mode
// changed is evaluated as it is now.
func (e *Engine) connectAccountSatisfied(ctx context.Context, r *runtime.SkillSetupRequest) (*skill.Binding, bool, error) {
	if e.credentialAvailability == nil {
		return nil, false, errors.New("account connections cannot be checked on this host")
	}
	actingUser := r.RequestedFor
	if actingUser == "" {
		run, err := e.store.GetAgentRun(ctx, r.Scope, r.RunID)
		if err != nil {
			return nil, false, err
		}
		if run != nil {
			actingUser = run.ActingUser
		}
	}
	scope := skill.ScopeReference{Kind: r.Scope.Kind, ID: r.Scope.ID}
	var binding *skill.Binding
	if strings.HasPrefix(r.BindingID, "mcp:") {
		set, err := e.skills.HostBindings(ctx, scope, r.Owner.ID, skill.RunActor{RunID: r.RunID, AgentID: r.AgentID, Umbrella: runtime.SkillUmbrella(r.Owner), ActingUser: actingUser})
		if err != nil {
			return nil, false, err
		}
		for _, candidate := range set.Bindings {
			if candidate != nil && candidate.ID == r.BindingID {
				binding = candidate
				break
			}
		}
	} else {
		current, err := e.skills.GetBinding(ctx, scope, r.Owner.ID, r.BindingID)
		if err != nil {
			return nil, false, err
		}
		binding = current
	}
	if binding == nil || binding.Disabled || binding.DeploymentID != r.Owner.ID || binding.Revision < 1 {
		return nil, false, nil
	}
	references := make(map[string]skill.CredentialReference, len(r.Slots))
	requirements := make(map[string]skill.CredentialRequirement, len(r.Slots))
	for _, slot := range r.Slots {
		reference, ok := binding.Credentials[slot.Name]
		if !ok {
			return nil, false, nil
		}
		references[slot.Name], requirements[slot.Name] = reference, slot.Requirement
	}
	availability, err := e.credentialAvailability.CheckCredentialAvailability(ctx, runtime.CredentialAvailabilityRequest{
		Scope: r.Scope, Umbrella: r.Owner, ActingUser: actingUser, DeploymentID: r.Owner.ID, BindingID: binding.ID,
		References: references, Requirements: requirements,
	})
	if err != nil {
		return nil, false, err
	}
	return binding, len(availability.Gaps) == 0, nil
}

// ConnectAccountReconcileFilter selects pending connect_account cards by the
// person they are for and/or their umbrella. An empty filter selects all.
type ConnectAccountReconcileFilter struct {
	RequestedFor string
	Umbrella     *ObjectiveOwner
}

// ReconcileConnectAccountRequests resolves the pending connect_account cards
// the filter selects whose accounts are now available, for example right
// after a person signed in or a binding changed. It returns the resolved
// cards; an unsatisfied card stays for the next reconciliation.
func (e *Engine) ReconcileConnectAccountRequests(ctx context.Context, scope runtime.Scope, filter ConnectAccountReconcileFilter) ([]*runtime.SkillSetupRequest, error) {
	if e == nil || e.credentialAvailability == nil {
		return nil, nil
	}
	matches, err := e.pendingSkillSetupRequests(ctx, scope, func(r *runtime.SkillSetupRequest) bool {
		return r.Kind == runtime.SkillSetupKindConnectAccount &&
			(filter.RequestedFor == "" || r.RequestedFor == filter.RequestedFor) &&
			(filter.Umbrella == nil || r.Owner == *filter.Umbrella)
	})
	if err != nil {
		return nil, err
	}
	var resolved []*runtime.SkillSetupRequest
	var failures []error
	for _, request := range matches {
		result, err := e.ResolveConnectAccountRequest(ctx, scope, request.Owner, request.ID, request.Revision, InteractionResolver{System: connectAccountReconciliationActor}, false)
		switch {
		case err == nil:
			resolved = append(resolved, result)
		case errors.Is(err, ErrConnectAccountNotSatisfied), errors.Is(err, runtime.ErrSkillSetupConflict):
		default:
			failures = append(failures, err)
		}
	}
	return resolved, errors.Join(failures...)
}

// pendingSkillSetupRequests lists a workspace's pending setup-store requests
// that match keep.
func (e *Engine) pendingSkillSetupRequests(ctx context.Context, scope runtime.Scope, keep func(*runtime.SkillSetupRequest) bool) ([]*runtime.SkillSetupRequest, error) {
	pending, ok := e.store.(interface {
		ListPendingSkillSetupRequests(context.Context, runtime.Scope, int, int) ([]*runtime.SkillSetupRequest, error)
	})
	if !ok || scope.Validate() != nil {
		return nil, nil
	}
	const pageSize, maximumPages = 100, 20
	var matches []*runtime.SkillSetupRequest
	for page := 0; page < maximumPages; page++ {
		requests, err := pending.ListPendingSkillSetupRequests(ctx, scope, pageSize, page*pageSize)
		if err != nil {
			return nil, err
		}
		for _, request := range requests {
			if request != nil && keep(request) {
				matches = append(matches, request)
			}
		}
		if len(requests) < pageSize {
			break
		}
	}
	return matches, nil
}

// reconcileConnectAccountRequestsForUmbrella runs after a binding of owner
// changed. Failures leave the cards for the next reconciliation.
func (e *Engine) reconcileConnectAccountRequestsForUmbrella(ctx context.Context, scope skill.ScopeReference, owner ObjectiveOwner) {
	_, _ = e.ReconcileConnectAccountRequests(ctx, runtime.Scope{Kind: scope.Kind, ID: scope.ID}, ConnectAccountReconcileFilter{Umbrella: &owner})
}
