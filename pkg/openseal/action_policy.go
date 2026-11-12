package openseal

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
)

func (e *Engine) evaluateAgentActionAuthority(ctx context.Context, input action.PolicyInput) (action.PolicyDecision, error) {
	if input.Run == nil || input.Bound == nil || input.Bound.Definition == nil || input.Bound.Binding == nil {
		return action.PolicyDecision{}, fmt.Errorf("run and bound action are required")
	}
	if input.Bound.Action.Risk == capability.RiskLevelRead &&
		(input.Bound.Action.SideEffect == capability.SideEffectNone || input.Bound.Action.SideEffect == capability.SideEffectRead) {
		return action.PolicyDecision{Disposition: action.DispositionAllow, Reason: "read-only action"}, nil
	}
	// These host-owned actions create bounded conversation artifacts. Public
	// app publication and all unrelated writes still follow approval policy.
	if input.Bound.Definition.ID == builtin.SkillID && input.Bound.Definition.Version == builtin.SkillVersion &&
		(input.Bound.Action.Name == builtin.GenerateImage || input.Bound.Action.Name == builtin.Present || input.Bound.Action.Name == builtin.SaveApp || input.Bound.Action.Name == builtin.SaveAppData) ||
		input.Bound.Definition.ID == presentation.SkillID && input.Bound.Definition.Version == presentation.SkillVersion &&
			input.Bound.Action.Name == presentation.UpdateVisual {
		return action.PolicyDecision{Disposition: action.DispositionAllow, Reason: "create bounded conversation artifact"}, nil
	}
	// Remembering the asking user's own picture style changes only how their
	// later images look.
	if input.Bound.Definition.ID == builtin.SkillID && input.Bound.Definition.Version == builtin.SkillVersion && input.Bound.Action.Name == builtin.SetImageStyle {
		return action.PolicyDecision{Disposition: action.DispositionAllow, Reason: "remember the user's picture style"}, nil
	}
	// Starting an already reviewed Runbook does not widen authority: the
	// activation pins its owner, Objective, definition, policy, budget, and
	// downstream approval requirements. The Runbook validator has proved those
	// facts before policy evaluation, so a conversational start is equivalent
	// to the existing manual Start operation.
	if input.Bound.Definition.ID == models.RunbookManagementSkillID && input.Bound.Action.Name == routine.RunbookActionStart {
		return action.PolicyDecision{Disposition: action.DispositionAllow, Reason: "start reviewed Runbook activation"}, nil
	}
	// Conversation Run controls are validated against the durable owner and
	// current revision before policy evaluation. They narrow or restore already
	// reviewed work and never grant a new Skill, credential, or external scope.
	if input.Bound.Definition.ID == models.RunManagementSkillID {
		return action.PolicyDecision{Disposition: action.DispositionAllow, Reason: "control owner-scoped Run"}, nil
	}

	deploymentID := strings.TrimSpace(input.Run.AssignedAgentID)
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

	// A manifest-declared review: always action (for example an Agent behavior
	// amendment or a payment) crosses an explicit review checkpoint even when
	// the Agent has broad standing authority or a permissive threshold.
	alwaysReview := input.Bound.Action.AlwaysReview()
	if !alwaysReview && bindingOwnedByAgent {
		if grant := matchingStandingGrant(definition.Authority.StandingGrants, input); grant != nil {
			return action.PolicyDecision{Disposition: action.DispositionAllow, Reason: "standing authority " + grant.ID}, nil
		}
	}
	// RequireApprovalAt is the Agent's reviewed risk boundary. Actions below
	// that boundary may proceed without a checkpoint; actions at or above it
	// are still handled by the default side-effect policy below. An omitted or
	// invalid threshold retains the fail-closed default behavior.
	if !alwaysReview && actionRiskBelowApprovalThreshold(input.Bound.Action.Risk, definition.Authority.RequireApprovalAt) {
		return action.PolicyDecision{
			Disposition: action.DispositionAllow,
			Reason:      fmt.Sprintf("%s risk is below the Agent approval threshold %s", input.Bound.Action.Risk, definition.Authority.RequireApprovalAt),
		}, nil
	}
	decision, err := e.defaultSideEffectPolicy().EvaluateAction(ctx, input)
	if err != nil {
		return action.PolicyDecision{}, err
	}
	for _, destination := range definition.Authority.ApprovalDestinations {
		decision.ApprovalDestinations = append(decision.ApprovalDestinations, models.ApprovalDestination{EndpointID: destination.EndpointID})
	}
	if timeout := definition.Authority.ApprovalTimeout; timeout != nil {
		decision.ApprovalTTL = time.Duration(timeout.AfterSeconds) * time.Second
		decision.ApprovalTimeout = models.ApprovalTimeoutDecision(timeout.Decision)
	}
	return decision, nil
}

func actionRiskBelowApprovalThreshold(action, threshold capability.RiskLevel) bool {
	return action.Rank() >= 0 && threshold.Rank() >= 0 && action.Rank() < threshold.Rank()
}

func (e *Engine) defaultSideEffectPolicy() action.PolicyEvaluator {
	return action.NewDefaultActionPolicy()
}

func matchingStandingGrant(grants []agent.StandingActionGrant, input action.PolicyInput) *agent.StandingActionGrant {
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
