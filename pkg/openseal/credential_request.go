package openseal

import (
	"context"
	"errors"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

type CredentialRequest = runtime.CredentialRequest

const SkillActionRequestCredential = runtime.SkillActionRequestCredential
const CredentialRequestWakeType = runtime.CredentialRequestWakeType

const (
	CredentialRequestKindWebsiteLogin = runtime.CredentialRequestKindWebsiteLogin
	CredentialRequestKindPaymentCard  = runtime.CredentialRequestKindPaymentCard
	CredentialRequestStatusPending    = runtime.CredentialRequestStatusPending
	CredentialRequestStatusResolved   = runtime.CredentialRequestStatusResolved
	CredentialRequestStatusDismissed  = runtime.CredentialRequestStatusDismissed
)

var ErrCredentialRequestConflict = runtime.ErrCredentialRequestConflict
var ErrInvalidCredentialRequest = runtime.ErrInvalidCredentialRequest

// CredentialRequestResolution is the saved request and what happened to the
// Run that asked for it.
type CredentialRequestResolution struct {
	Request *CredentialRequest `json:"request"`
	// Continued is true when the requesting Run resumed, or is still active
	// and will see the saved credential. False means the Run already ended;
	// the host should start a new reply for the conversation.
	Continued bool `json:"continued"`
}

func (e *Engine) credentialRequestStore() (runtime.CredentialRequestStore, error) {
	if e == nil || e.store == nil {
		return nil, errors.New("credential requests are unavailable")
	}
	return e.store, nil
}

// ListCredentialRequests lists a conversation's credential requests. owner
// must own the conversation: every request there is saved to it.
func (e *Engine) ListCredentialRequests(ctx context.Context, scope runtime.Scope, owner ObjectiveOwner, conversationID string) ([]*CredentialRequest, error) {
	store, err := e.credentialRequestStore()
	if err != nil {
		return nil, err
	}
	if err := e.conversationOwnedBy(ctx, scope, owner, conversationID); err != nil {
		return nil, err
	}
	return store.ListCredentialRequests(ctx, scope, owner.ID, conversationID)
}

// GetCredentialRequest returns request id when it is saved to owner.
func (e *Engine) GetCredentialRequest(ctx context.Context, scope runtime.Scope, owner ObjectiveOwner, id string) (*CredentialRequest, error) {
	store, err := e.credentialRequestStore()
	if err != nil {
		return nil, err
	}
	r, err := store.GetCredentialRequest(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if r == nil || r.Owner != owner || r.DeploymentID != owner.ID {
		return nil, runtime.ErrInvalidCredentialRequest
	}
	return r, nil
}

// ResolveCredentialRequest records that the user saved the requested
// credential (or dismissed the card) and resumes the Run waiting for it.
// Only a manager of the request's umbrella (or a system reconciliation)
// settles it. Hosts verify the saved credential first; the kernel never sees
// credential values. Settling it settles the same request of other runs in
// the conversation.
func (e *Engine) ResolveCredentialRequest(ctx context.Context, scope runtime.Scope, owner ObjectiveOwner, id string, expected int64, resolver InteractionResolver, dismiss bool) (*CredentialRequestResolution, error) {
	store, err := e.credentialRequestStore()
	if err != nil {
		return nil, err
	}
	actor, err := resolver.actor()
	if err != nil {
		return nil, runtime.ErrInvalidCredentialRequest
	}
	r, err := e.GetCredentialRequest(ctx, scope, owner, id)
	if err != nil {
		return nil, err
	}
	if err := authorizeManagerInteraction(resolver); err != nil {
		return nil, err
	}
	want := runtime.CredentialRequestStatusResolved
	if dismiss {
		want = runtime.CredentialRequestStatusDismissed
	}
	if r.Status == runtime.CredentialRequestStatusPending {
		if r.Revision != expected {
			return nil, runtime.ErrCredentialRequestConflict
		}
		r.Status, r.ResolvedBy = want, actor
		r.Revision++
		r.UpdatedAt = time.Now().UTC()
		if err := store.SaveCredentialRequest(ctx, r, expected); err != nil {
			return nil, err
		}
	} else if r.Status != want {
		return nil, runtime.ErrCredentialRequestConflict
	}
	e.settleCredentialRequestSiblings(ctx, r)
	continued, err := e.continueCredentialRequestRun(ctx, r)
	if err != nil {
		return nil, err
	}
	return &CredentialRequestResolution{Request: r, Continued: continued}, nil
}

func (e *Engine) continueCredentialRequestRun(ctx context.Context, r *CredentialRequest) (bool, error) {
	return e.continueInteractionRun(ctx, interactionContinuation{
		scope: r.Scope, owner: r.Owner, agentID: r.AgentID, runID: r.RunID, requestID: r.ID, actionCallID: r.ActionCallID, status: r.Status, revision: r.Revision,
		updatedAt: r.UpdatedAt, wakeType: runtime.CredentialRequestWakeType, coordinator: "credential-request-coordinator",
		eventType: "run.credential_request_settled", summary: "Received the credential request result; continuing",
		interventionPrefix: "credential-request-result:", instruction: runtime.CredentialRequestResolutionInstruction(r),
	})
}

// interactionContinuation is one settled in-chat request whose Run may be
// waiting for it.
type interactionContinuation struct {
	scope runtime.Scope
	// owner and agentID are the umbrella and member that asked; the Run
	// resumes only while it still acts as both.
	owner                                                  runtime.ObjectiveOwner
	agentID                                                string
	runID, requestID, actionCallID, status                 string
	revision                                               int64
	updatedAt                                              time.Time
	wakeType, coordinator, eventType, summary, instruction string
	interventionPrefix                                     string
	// requestedFor is the person the request was for; the run resumes only
	// while it still acts for that person. Empty for manager requests.
	requestedFor string
}

// continueInteractionRun hands a settled in-chat request to the Run that
// asked for it, under the Run's revision CAS. A Run parked on the request is
// queued with the result for the requesting tool call. A Run that is still
// held (its step has not parked yet) keeps its status, and the appended result
// bumps its revision, so the step-end park CAS fails and re-plans instead of
// waiting on a settled request. It reports false when the Run already ended,
// or no longer acts for the umbrella and member that asked, so the host starts
// a new reply instead.
func (e *Engine) continueInteractionRun(ctx context.Context, c interactionContinuation) (bool, error) {
	interventionID := c.interventionPrefix + c.requestID
	for attempt := 0; attempt < 4; attempt++ {
		run, err := e.store.GetAgentRun(ctx, c.scope, c.runID)
		if err != nil {
			return false, err
		}
		if run == nil {
			return false, nil
		}
		switch run.Status {
		case runtime.AgentRunStatusCompleted, runtime.AgentRunStatusFailed, runtime.AgentRunStatusCanceled:
			return false, nil
		}
		if c.requestedFor != "" && run.ActingUser != c.requestedFor {
			return false, nil
		}
		resumable, err := runtime.InteractionRunResumable(ctx, e, run, c.owner, c.agentID)
		if err != nil || !resumable {
			return false, err
		}
		waiting := run.Status == runtime.AgentRunStatusWaitingForEvent && run.WakeCondition != nil &&
			run.WakeCondition.Type == c.wakeType && run.WakeCondition.Reference == c.requestID
		if !waiting && interactionDelivered(run, interventionID) {
			return true, nil
		}
		status, wake := runtime.AgentRunStatusQueued, (*runtime.WakeCondition)(nil)
		if !waiting {
			status, wake = run.Status, run.WakeCondition
		}
		actor := runtime.ActivityActor{Type: "service", ID: c.coordinator}
		_, _, err = runtime.NewRunActivityService(e.store, e.store).TransitionRun(ctx, c.scope, run.ID, runtime.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: status, WakeCondition: wake,
			Summary: c.summary, EventType: c.eventType,
			Actor: actor, CorrelationID: c.requestID, CausationID: c.actionCallID,
			Payload: map[string]interface{}{"requestId": c.requestID, "status": c.status, "revision": c.revision},
			Intervention: &runtime.AgentRunIntervention{ID: interventionID, Actor: actor,
				Instruction: c.instruction, CreatedAt: c.updatedAt, ActionCallID: c.actionCallID},
		})
		if errors.Is(err, runtime.ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return false, err
		}
		if waiting {
			e.wakeAgentWorkersForScope(c.scope)
		}
		return true, nil
	}
	return false, runtime.ErrRevisionConflict
}

// interactionDelivered reports whether the Run already holds this request's
// settled result, so a replayed settle does not append it twice.
func interactionDelivered(run *runtime.AgentRun, interventionID string) bool {
	for _, intervention := range run.PendingInterventions {
		if intervention.ID == interventionID {
			return true
		}
	}
	return false
}
