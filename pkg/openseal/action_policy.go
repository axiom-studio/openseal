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
	// Starting an already reviewed Runbook does not widen authority: the
	// activation pins its owner, Objective, definition, policy, budget, and
	// downstream approval requirements. The Runbook validator has proved those
	// facts before policy evaluation, so a conversational start is equivalent
	// to the existing manual Start operation.
	if input.Bound.Definition.ID == runtime.RunbookManagementSkillID && input.Bound.Action.Name == runtime.RunbookActionStart {
		return runtime.ActionPolicyDecision{Disposition: runtime.ActionDispositionAllow, Reason: "start reviewed Runbook activation"}, nil
	}
	// Conversation Run controls are validated against the durable owner and
	// current revision before policy evaluation. They narrow or restore already
	// reviewed work and never grant a new Skill, credential, or external scope.
	if input.Bound.Definition.ID == runtime.RunManagementSkillID {
		return runtime.ActionPolicyDecision{Disposition: runtime.ActionDispositionAllow, Reason: "control owner-scoped Run"}, nil
	}

	deploymentID := strings.TrimSpace(input.Run.AssignedAgentID)
	if deploymentID == "" && input.Run.Owner.Type == runtime.OwnerTypeAgent {
		deploymentID = strings.TrimSpace(input.Run.Owner.ID)
	}
	if deploymentID == "" {
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
	// A shared or Team-owned binding must not inherit an assigned Agent's
	// standing grants. It does, however, still use that Agent's reviewed
	// approval delivery and timeout policy. Returning the default decision here
	// used to discard those destinations and strand otherwise valid approvals
	// inside the platform with no configured delivery edge.
	bindingOwnedByAgent := true
	if bindingOwner := strings.TrimSpace(input.Bound.Binding.DeploymentID); bindingOwner != "" && bindingOwner != deploymentID {
		bindingOwnedByAgent = false
	}

	// Agent behavior amendments are proposals about the durable worker itself.
	// Even when the Agent has broad standing authority for ordinary work, an
	// amendment requested in chat must cross an explicit review checkpoint.
	// This also lets the narrow management capability propose fields outside
	// the Agent's autonomous self-amendment allowlist without silently widening
	// what the Agent may change on its own.
	requiresBehaviorReview := input.Bound.Definition.ID == runtime.AgentManagementSkillID &&
		(input.Bound.Action.Name == runtime.AgentActionAmendBehavior || input.Bound.Action.Name == runtime.AgentActionConfigureChannel)
	if !requiresBehaviorReview && bindingOwnedByAgent {
		if grant := matchingStandingGrant(definition.Authority.StandingGrants, input); grant != nil {
			return runtime.ActionPolicyDecision{Disposition: runtime.ActionDispositionAllow, Reason: "standing authority " + grant.ID}, nil
		}
	}
	decision, err := e.defaultSideEffectPolicy().EvaluateAction(ctx, input)
	if err != nil {
		return runtime.ActionPolicyDecision{}, err
	}
	if requiresBehaviorReview {
		decision.Reason = "Agent behavior amendments require explicit review"
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
