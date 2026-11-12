package collaboration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func (s *Service) resolveTeamRequestCandidates(ctx context.Context, request *models.AgentRequest) ([]models.AgentRequestCandidate, error) {
	if s.teams == nil {
		return nil, fmt.Errorf("%w: Team roster assignment is unavailable", models.ErrAgentRequestAssignment)
	}
	scope := capability.ScopeReference{Kind: request.Scope.Kind, ID: request.Scope.ID}
	deployment, err := s.teams.GetTeamDeployment(ctx, scope, request.Recipient.ID)
	if err != nil {
		return nil, err
	}
	if deployment == nil || deployment.Status != kernelteam.DeploymentActive {
		return nil, fmt.Errorf("%w: recipient Team deployment is not active", models.ErrAgentRequestAssignment)
	}
	agents, hasAgentCatalog := s.store.(collaborationAgentStore)
	candidates := make([]models.AgentRequestCandidate, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		if request.SemanticRole != "" && assignment.ID != request.SemanticRole {
			continue
		}
		limit := 1
		if hasAgentCatalog {
			agentDeployment, getErr := agents.GetDeployment(ctx, scope, assignment.AgentDeploymentID)
			if getErr != nil && !errors.Is(getErr, kernelagent.ErrDeploymentNotFound) {
				return nil, getErr
			}
			if agentDeployment == nil {
				if len(assignment.RequiredSkillIDs) > 0 {
					return nil, fmt.Errorf("%w: Agent authority catalog cannot verify member %q", models.ErrAgentRequestAssignment, assignment.ID)
				}
			} else if agentDeployment.RolloutStatus != kernelagent.RolloutActive {
				continue
			} else {
				agentDefinition, getErr := agents.GetDefinition(ctx, agentDeployment.DefinitionID, agentDeployment.ActiveVersion)
				if getErr != nil {
					return nil, getErr
				}
				if agentDefinition == nil || !agentDefinitionProvidesSkills(agentDefinition, assignment.RequiredSkillIDs) {
					continue
				}
				limit = agentDeployment.Capacity.MaxConcurrentRuns
				if agentDeployment.Restrictions.MaxConcurrentRuns != nil && *agentDeployment.Restrictions.MaxConcurrentRuns < limit {
					limit = *agentDeployment.Restrictions.MaxConcurrentRuns
				}
			}
		} else if len(assignment.RequiredSkillIDs) > 0 {
			return nil, fmt.Errorf("%w: Agent authority catalog is required to evaluate member %q", models.ErrAgentRequestAssignment, assignment.ID)
		}
		runs, listErr := s.runs.ListAgentRuns(ctx, models.AgentRunFilter{Scope: request.Scope, AssignedAgentID: assignment.AgentDeploymentID, Limit: 500})
		if listErr != nil {
			return nil, listErr
		}
		active := 0
		for _, run := range runs {
			if !models.IsTerminalAgentRunStatus(run.Status) {
				active++
			}
		}
		available := limit - active
		if available < 0 {
			available = 0
		}
		candidates = append(candidates, models.AgentRequestCandidate{
			AgentDeploymentID: assignment.AgentDeploymentID, RosterAssignmentID: assignment.ID,
			RequiredSkillIDs: append([]string(nil), assignment.RequiredSkillIDs...), CapacityLimit: limit, ActiveRuns: active, AvailableSlots: available,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].AgentDeploymentID < candidates[j].AgentDeploymentID })
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: recipient Team has no eligible Agent for semantic role %q", models.ErrAgentRequestAssignment, request.SemanticRole)
	}
	return candidates, nil
}

func agentDefinitionProvidesSkills(definition *kernelagent.AgentDefinition, required []string) bool {
	if len(required) == 0 {
		return true
	}
	provided := make(map[string]struct{}, len(definition.Authority.AllowedSkillIDs)+len(definition.SkillRequirements))
	for _, id := range definition.Authority.AllowedSkillIDs {
		provided[strings.TrimSpace(id)] = struct{}{}
	}
	for _, requirement := range definition.SkillRequirements {
		provided[strings.TrimSpace(requirement.SkillID)] = struct{}{}
	}
	for _, id := range required {
		if _, ok := provided[strings.TrimSpace(id)]; !ok {
			return false
		}
	}
	return true
}

func (s *Service) SubmitAgentRequestBid(ctx context.Context, req SubmitAgentRequestBidRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	request, err := s.store.GetAgentRequest(ctx, req.Scope, strings.TrimSpace(req.RequestID))
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	if request.Revision != req.ExpectedRevision || request.Status != models.AgentRequestStatusPending || request.Recipient.Type != models.OwnerTypeTeam {
		return nil, models.ErrInvalidAgentRequestState
	}
	agentID := strings.TrimSpace(req.AgentDeploymentID)
	key := strings.TrimSpace(req.IdempotencyKey)
	if agentID == "" || key == "" || req.Decision != models.AgentRequestBidAccept && req.Decision != models.AgentRequestBidDecline {
		return nil, errors.New("Team work bid requires an eligible Agent, accept or decline decision, and idempotency key")
	}
	eligible := false
	for _, candidate := range request.Candidates {
		if candidate.AgentDeploymentID == agentID {
			eligible = true
			break
		}
	}
	if !eligible {
		return nil, fmt.Errorf("%w: bidding Agent is not an eligible Team member", models.ErrAgentRequestAssignment)
	}
	for _, bid := range request.Bids {
		if bid.AgentDeploymentID == agentID {
			if bid.IdempotencyKey == key && bid.Decision == req.Decision && bid.Reason == strings.TrimSpace(req.Reason) {
				return &AgentRequestResult{Request: request}, nil
			}
			return nil, models.ErrAgentRequestIdempotency
		}
	}
	now := s.now()
	updated := models.CloneAgentRequest(request)
	updated.Bids = append(updated.Bids, models.AgentRequestBid{AgentDeploymentID: agentID, Decision: req.Decision, Reason: strings.TrimSpace(req.Reason), IdempotencyKey: key, SubmittedAt: now})
	updated.Revision++
	updated.UpdatedAt = now
	source, err := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	event := collaborationEvent(source, updated, "collaboration.work_bid_submitted", fmt.Sprintf("Agent %s %sed the Team work offer", agentID, req.Decision), models.CollaborationParty{Type: models.OwnerTypeAgent, ID: agentID}, now)
	persisted, err := s.store.CoordinateAgentRequest(ctx, models.AgentRequestCoordinationRecord{Request: updated, ExpectedRequestRevision: request.Revision, Event: event})
	if err != nil {
		return nil, err
	}
	return &AgentRequestResult{Request: updated, Source: source, Events: []*models.ActivityEvent{persisted}}, nil
}

func (s *Service) resolveRequestAssignment(ctx context.Context, request *models.AgentRequest, assignedAgentID string) (string, error) {
	assignedAgentID = strings.TrimSpace(assignedAgentID)
	if request.Recipient.Type == models.OwnerTypeAgent {
		if assignedAgentID != "" && assignedAgentID != request.Recipient.ID {
			return "", fmt.Errorf("%w: assigned Agent does not match the recipient", models.ErrAgentRequestAssignment)
		}
		return request.Recipient.ID, nil
	}
	if s.teams == nil {
		return "", fmt.Errorf("%w: Team roster assignment is unavailable", models.ErrAgentRequestAssignment)
	}
	deployment, err := s.teams.GetTeamDeployment(ctx, capability.ScopeReference{Kind: request.Scope.Kind, ID: request.Scope.ID}, request.Recipient.ID)
	if err != nil {
		return "", err
	}
	if deployment == nil || deployment.Status != kernelteam.DeploymentActive {
		return "", fmt.Errorf("%w: recipient Team deployment is not active", models.ErrAgentRequestAssignment)
	}
	eligible := make([]kernelteam.RosterAssignment, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		if request.SemanticRole == "" || assignment.ID == request.SemanticRole {
			eligible = append(eligible, assignment)
		}
	}
	if assignedAgentID == "" {
		if len(eligible) == 0 {
			return "", fmt.Errorf("%w: recipient Team has no Agent for semantic role %q", models.ErrAgentRequestAssignment, request.SemanticRole)
		}
		accepted := acceptedTeamRequestCandidates(request)
		if len(accepted) > 0 {
			return accepted[0].AgentDeploymentID, nil
		}
		if len(eligible) == 1 {
			return eligible[0].AgentDeploymentID, nil
		}
		return "", fmt.Errorf("%w: Team request acceptance requires an explicit assigned Agent or accepted bid because %d roster members are eligible", models.ErrAgentRequestAssignment, len(eligible))
	}
	for _, assignment := range eligible {
		if assignment.AgentDeploymentID == assignedAgentID {
			return assignedAgentID, nil
		}
	}
	return "", fmt.Errorf("%w: assigned Agent is not an eligible member of the recipient Team", models.ErrAgentRequestAssignment)
}

func acceptedTeamRequestCandidates(request *models.AgentRequest) []models.AgentRequestCandidate {
	accepted := make(map[string]struct{})
	for _, bid := range request.Bids {
		if bid.Decision == models.AgentRequestBidAccept {
			accepted[bid.AgentDeploymentID] = struct{}{}
		}
	}
	result := make([]models.AgentRequestCandidate, 0, len(accepted))
	for _, candidate := range request.Candidates {
		if _, ok := accepted[candidate.AgentDeploymentID]; ok && candidate.AvailableSlots > 0 {
			result = append(result, candidate)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].AvailableSlots != result[j].AvailableSlots {
			return result[i].AvailableSlots > result[j].AvailableSlots
		}
		if result[i].ActiveRuns != result[j].ActiveRuns {
			return result[i].ActiveRuns < result[j].ActiveRuns
		}
		return result[i].AgentDeploymentID < result[j].AgentDeploymentID
	})
	return result
}

func teamAssignmentReason(request *models.AgentRequest, selected string) string {
	accepted := acceptedTeamRequestCandidates(request)
	for index, candidate := range accepted {
		if candidate.AgentDeploymentID == selected {
			return fmt.Sprintf("selected accepted bid rank %d by available capacity (%d slots, %d active Runs)", index+1, candidate.AvailableSlots, candidate.ActiveRuns)
		}
	}
	if len(request.Candidates) == 1 {
		return "selected the only eligible roster member under the reviewed Team role policy"
	}
	return "selected explicitly from the eligible roster under the reviewed Team role policy"
}
