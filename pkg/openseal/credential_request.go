package openseal

import (
	"context"
	"errors"
	"strings"
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

// ListCredentialRequests lists one agent conversation's credential requests.
func (e *Engine) ListCredentialRequests(ctx context.Context, scope runtime.Scope, deploymentID, conversationID string) ([]*CredentialRequest, error) {
	store, err := e.credentialRequestStore()
	if err != nil {
		return nil, err
	}
	conversation, err := e.GetConversation(ctx, scope, conversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil || conversation.Owner.Type != "agent" || conversation.Owner.ID != deploymentID {
		return nil, runtime.ErrConversationNotFound
	}
	return store.ListCredentialRequests(ctx, scope, deploymentID, conversationID)
}

func (e *Engine) GetCredentialRequest(ctx context.Context, scope runtime.Scope, deploymentID, id string) (*CredentialRequest, error) {
	store, err := e.credentialRequestStore()
	if err != nil {
		return nil, err
	}
	r, err := store.GetCredentialRequest(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if r == nil || r.DeploymentID != deploymentID {
		return nil, runtime.ErrInvalidCredentialRequest
	}
	return r, nil
}

// ResolveCredentialRequest records that the user saved the requested
// credential (or dismissed the card) and resumes the Run waiting for it.
// Hosts must authorize the actor and verify the saved credential first; the
// kernel never sees credential values.
func (e *Engine) ResolveCredentialRequest(ctx context.Context, scope runtime.Scope, deploymentID, id string, expected int64, actor string, dismiss bool) (*CredentialRequestResolution, error) {
	store, err := e.credentialRequestStore()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(actor) == "" {
		return nil, runtime.ErrInvalidCredentialRequest
	}
	r, err := e.GetCredentialRequest(ctx, scope, deploymentID, id)
	if err != nil {
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
	continued, err := e.continueCredentialRequestRun(ctx, r)
	if err != nil {
		return nil, err
	}
	return &CredentialRequestResolution{Request: r, Continued: continued}, nil
}

func (e *Engine) continueCredentialRequestRun(ctx context.Context, r *CredentialRequest) (bool, error) {
	for attempt := 0; attempt < 4; attempt++ {
		run, err := e.store.GetAgentRun(ctx, r.Scope, r.RunID)
		if err != nil {
			return false, err
		}
		if run == nil {
			return false, nil
		}
		if !runtime.IsExactCredentialRequestWait(run, r.ID) {
			if run.Status == runtime.AgentRunStatusPaused && run.PausedWakeCondition != nil &&
				run.PausedWakeCondition.Type == runtime.CredentialRequestWakeType && run.PausedWakeCondition.Reference == r.ID {
				// The user paused this work; resuming it later finds the saved
				// credential through the requested action's retry.
				return true, nil
			}
			switch run.Status {
			case runtime.AgentRunStatusCompleted, runtime.AgentRunStatusFailed, runtime.AgentRunStatusCanceled:
				return false, nil
			}
			return true, nil
		}
		actor := runtime.ActivityActor{Type: "service", ID: "credential-request-coordinator"}
		_, _, err = runtime.NewRunActivityService(e.store, e.store).TransitionRun(ctx, r.Scope, run.ID, runtime.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: runtime.AgentRunStatusQueued,
			Summary: "Received the credential request result; continuing", EventType: "run.credential_request_settled",
			Actor: actor, CorrelationID: r.ID, CausationID: r.ActionCallID,
			Payload: map[string]interface{}{"requestId": r.ID, "status": r.Status, "revision": r.Revision},
			Intervention: &runtime.AgentRunIntervention{ID: "credential-request-result:" + r.ID, Actor: actor,
				Instruction: runtime.CredentialRequestResolutionInstruction(r), CreatedAt: r.UpdatedAt},
		})
		if errors.Is(err, runtime.ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return false, err
		}
		e.wakeAgentWorkersForScope(r.Scope)
		return true, nil
	}
	return false, runtime.ErrRevisionConflict
}
