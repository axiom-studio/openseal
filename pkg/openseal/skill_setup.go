package openseal

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"strings"
	"time"
)

type SkillSetupRequest = runtime.SkillSetupRequest

const SkillActionRequestSetup = runtime.SkillActionRequestSetup
const SkillActionListSetupRequests = runtime.SkillActionListSetupRequests

var ErrSkillSetupConflict = runtime.ErrSkillSetupConflict
var ErrInvalidSkillSetup = runtime.ErrInvalidSkillSetup

func (e *Engine) ListSkillSetupRequests(ctx context.Context, scope runtime.Scope, deploymentID, conversationID string) ([]*runtime.SkillSetupRequest, error) {
	conversation, err := e.GetConversation(ctx, scope, conversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil || conversation.Owner.Type != "agent" || conversation.Owner.ID != deploymentID {
		return nil, runtime.ErrConversationNotFound
	}
	requests, err := e.store.ListSkillSetupRequests(ctx, scope, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	bindings, err := e.skills.ListBindings(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID)
	if err != nil {
		return nil, err
	}
	// A validated binding saved in Settings or an OAuth callback is the same
	// evidence as one saved in the chat form. Reconcile from canonical state so
	// closing a tab or losing a completion response cannot strand the request.
	for index, request := range requests {
		if request.Status != "pending" {
			continue
		}
		for _, binding := range bindings {
			if binding.Disabled || binding.SkillID != request.SkillID || binding.SkillVersion != request.SkillVersion || binding.SourceIdentity != request.SourceIdentity || binding.Revision <= request.BindingRevision || (request.BindingID != "" && binding.ID != request.BindingID) {
				continue
			}
			if request.BindingID == "" && binding.CreatedAt.Before(request.CreatedAt) {
				continue
			}
			resolved, resolveErr := e.ResolveSkillSetupRequest(ctx, scope, deploymentID, request.ID, request.Revision, binding.ID, "system:binding-reconciliation", false)
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
func (e *Engine) GetSkillSetupRequest(ctx context.Context, scope runtime.Scope, deploymentID, id string) (*runtime.SkillSetupRequest, error) {
	r, err := e.store.GetSkillSetupRequest(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if r == nil || r.DeploymentID != deploymentID {
		return nil, runtime.ErrInvalidSkillSetup
	}
	return r, nil
}

// ResolveSkillSetupRequest accepts evidence of a successful, separately
// authorized binding write. A click or an OAuth popup closing is not completion.
// Hosts must authorize the actor and the exact target before calling this method.
func (e *Engine) ResolveSkillSetupRequest(ctx context.Context, scope runtime.Scope, deploymentID, id string, expected int64, bindingID, actor string, dismiss bool) (*runtime.SkillSetupRequest, error) {
	r, err := e.GetSkillSetupRequest(ctx, scope, deploymentID, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(actor) == "" {
		return nil, runtime.ErrInvalidSkillSetup
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
		binding, err := e.skills.GetBinding(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID, bindingID)
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
