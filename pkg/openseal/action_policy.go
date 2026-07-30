package openseal

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (e *Engine) evaluateAgentActionAuthority(ctx context.Context, input runtime.ActionPolicyInput) (runtime.ActionPolicyDecision, error) {
	if input.Run == nil || input.Bound == nil || input.Bound.Definition == nil || input.Bound.Binding == nil {
		return runtime.ActionPolicyDecision{}, fmt.Errorf("run and bound action are required")
	}
	if input.Bound.Action.Risk == capability.RiskLevelRead &&
		(input.Bound.Action.SideEffect == capability.SideEffectNone || input.Bound.Action.SideEffect == capability.SideEffectRead) {
		return runtime.ActionPolicyDecision{Disposition: runtime.ActionDispositionAllow, Reason: "read-only action"}, nil
	}

	deploymentID := strings.TrimSpace(input.Run.AssignedAgentID)
	if deploymentID == "" && input.Run.Owner.Type == runtime.OwnerTypeAgent {
		deploymentID = strings.TrimSpace(input.Run.Owner.ID)
	}
	if deploymentID == "" {
		return e.defaultSideEffectPolicy().EvaluateAction(ctx, input)
	}
	// Team-owned bindings are governed by Team role grants and approval policy;
	// an assigned Agent's personal standing grants cannot widen that authority.
	if bindingOwner := strings.TrimSpace(input.Bound.Binding.DeploymentID); bindingOwner != "" && bindingOwner != deploymentID {
		return e.defaultSideEffectPolicy().EvaluateAction(ctx, input)
	}
	scope := capability.ScopeReference{Kind: input.Run.Scope.Kind, ID: input.Run.Scope.ID}
	deployment, err := e.agents.GetDeployment(ctx, scope, deploymentID)
	if err != nil || deployment == nil {
		return e.defaultSideEffectPolicy().EvaluateAction(ctx, input)
	}
	definition, err := e.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition == nil {
		return e.defaultSideEffectPolicy().EvaluateAction(ctx, input)
	}

	if grant := matchingStandingGrant(definition.Authority.StandingGrants, input); grant != nil {
		return runtime.ActionPolicyDecision{Disposition: runtime.ActionDispositionAllow, Reason: "standing authority " + grant.ID}, nil
	}
	decision, err := e.defaultSideEffectPolicy().EvaluateAction(ctx, input)
	if err != nil {
		return runtime.ActionPolicyDecision{}, err
	}
	for _, destination := range definition.Authority.ApprovalDestinations {
		decision.ApprovalDestinations = append(decision.ApprovalDestinations, runtime.ApprovalDestination{EndpointID: destination.EndpointID})
	}
	if timeout := definition.Authority.ApprovalTimeout; timeout != nil {
		decision.ApprovalTTL = time.Duration(timeout.AfterSeconds) * time.Second
		decision.ApprovalTimeout = runtime.ApprovalTimeoutDecision(timeout.Decision)
	}
	return decision, nil
}

func (e *Engine) defaultSideEffectPolicy() runtime.ActionPolicyEvaluator {
	return runtime.NewDefaultActionPolicy()
}

func matchingStandingGrant(grants []agent.StandingActionGrant, input runtime.ActionPolicyInput) *agent.StandingActionGrant {
	for index := range grants {
		grant := &grants[index]
		if grant.SkillID != input.Bound.Definition.ID || grant.Action != input.Bound.Action.Name {
			continue
		}
		if grant.ExternalOperation == "" {
			return grant
		}
		if input.ExternalOperation != nil && strings.EqualFold(strings.TrimSpace(grant.ExternalOperation), strings.TrimSpace(input.ExternalOperation.Operation)) &&
			resourceWithinPrefix(input.ExternalOperation.Resource, grant.ResourcePrefix) {
			return grant
		}
	}
	return nil
}

func resourceWithinPrefix(resource, prefix string) bool {
	candidate, candidateErr := url.Parse(strings.TrimSpace(resource))
	allowed, allowedErr := url.Parse(strings.TrimSpace(prefix))
	if candidateErr != nil || allowedErr != nil || candidate.User != nil || allowed.User != nil ||
		(candidate.Scheme != "http" && candidate.Scheme != "https") ||
		!strings.EqualFold(candidate.Scheme, allowed.Scheme) || !strings.EqualFold(candidate.Host, allowed.Host) {
		return false
	}
	allowedPath := allowed.EscapedPath()
	if allowedPath == "" {
		allowedPath = "/"
	}
	candidatePath := candidate.EscapedPath()
	if candidatePath == "" {
		candidatePath = "/"
	}
	if !strings.HasPrefix(candidatePath, allowedPath) {
		return false
	}
	return strings.HasSuffix(allowedPath, "/") || len(candidatePath) == len(allowedPath) || candidatePath[len(allowedPath)] == '/'
}
