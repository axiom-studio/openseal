package collaboration

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func (s *CollaborationService) RespondAgentRequest(ctx context.Context, req RespondAgentRequestRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	request, err := s.store.GetAgentRequest(ctx, req.Scope, req.RequestID)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	if request.Revision != req.ExpectedRevision {
		return nil, models.ErrRevisionConflict
	}
	if request.Status != models.AgentRequestStatusPending && request.Status != models.AgentRequestStatusClarificationRequested {
		return nil, models.ErrInvalidAgentRequestState
	}
	if req.Decision == AgentRequestDecisionProvideClarification {
		if req.Principal != request.Requester {
			return nil, ErrAgentRequestUnauthorized
		}
	} else if req.Principal != request.Recipient {
		return nil, ErrAgentRequestUnauthorized
	}
	source, err := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	var groupedDependency *models.RunDependency
	if request.DependencyGroupID != "" {
		groupedDependency, err = s.groupedRequestDependency(ctx, request, source, false)
		if err != nil {
			return nil, err
		}
	}
	now := s.now()
	actor := req.Actor
	if strings.TrimSpace(actor.ID) == "" {
		actor = req.Principal
	}
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("actor: %w", err)
	}
	if err := s.validateDecisionRunAuthority(ctx, request, actor, req.DecisionRunID); err != nil {
		return nil, err
	}
	updated := models.CloneAgentRequest(request)
	updated.Revision++
	updated.UpdatedAt = now
	updated.Response = strings.TrimSpace(req.Message)
	record := models.AgentRequestResponseRecord{Request: updated, ExpectedRequestRevision: request.Revision}
	eventType := ""
	summary := ""
	switch req.Decision {
	case AgentRequestDecisionProvideClarification:
		if request.Status != models.AgentRequestStatusClarificationRequested || strings.TrimSpace(req.Message) == "" {
			return nil, errors.New("clarification response is required for a request awaiting clarification")
		}
		updated.Status = models.AgentRequestStatusPending
		eventType = "collaboration.clarification_provided"
		summary = fmt.Sprintf("%s %s provided clarification", actor.Type, actor.ID)
		if groupedDependency == nil {
			record.SourceRun, err = sourceAwaitingAgentRequestDecision(source, updated, now)
			if err != nil {
				return nil, err
			}
			record.ExpectedSourceRevision = source.Revision
		}
	case AgentRequestDecisionRequestClarification:
		if request.Status != models.AgentRequestStatusPending {
			return nil, models.ErrInvalidAgentRequestState
		}
		if strings.TrimSpace(req.Message) == "" {
			return nil, errors.New("clarification question is required")
		}
		updated.Status = models.AgentRequestStatusClarificationRequested
		updated.Clarification = strings.TrimSpace(req.Message)
		eventType = "collaboration.clarification_requested"
		summary = fmt.Sprintf("%s %s requested clarification", actor.Type, actor.ID)
		if groupedDependency == nil {
			record.SourceRun, err = sourceResumingAfterAgentRequestDecision(source, updated, now)
			if err != nil {
				return nil, err
			}
			record.ExpectedSourceRevision = source.Revision
		}
	case AgentRequestDecisionReject:
		updated.Status = models.AgentRequestStatusRejected
		updated.ResolvedAt = &now
		eventType = "collaboration.rejected"
		summary = fmt.Sprintf("%s %s rejected the request", actor.Type, actor.ID)
		if groupedDependency != nil {
			reason := strings.TrimSpace(req.Message)
			if reason == "" {
				reason = "recipient rejected the request"
			}
			record.DependencyResolution = &models.RunDependencyResolutionRecord{
				Scope: updated.Scope, GroupID: updated.DependencyGroupID, DependencyID: updated.DependencyID,
				ExpectedDependencyRevision: groupedDependency.Revision, State: models.RunDependencyStateFailed, Error: reason,
				Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID}, Visibility: models.ActivityVisibilityTeam, OccurredAt: now,
			}
		} else {
			record.SourceRun, err = sourceResumingAfterAgentRequestDecision(source, updated, now)
			if err != nil {
				return nil, err
			}
			record.ExpectedSourceRevision = source.Revision
		}
	case AgentRequestDecisionAccept:
		if groupedDependency == nil {
			if !sourceWaitsForAgentRequestDecision(source, request.ID) {
				if err := validateAgentRequestSource(source); err != nil {
					return nil, err
				}
			}
		}
		assignedAgentID, assignmentErr := s.resolveRequestAssignment(ctx, request, req.AssignedAgentID)
		if assignmentErr != nil {
			return nil, assignmentErr
		}
		if err := s.verifyChatAssignment(ctx, source, assignedAgentID); err != nil {
			return nil, err
		}
		updated.Status = models.AgentRequestStatusAccepted
		updated.AcceptedAt = &now
		updated.AssignedAgentID = assignedAgentID
		if request.Recipient.Type == models.OwnerTypeTeam {
			updated.AssignmentDecision = &models.AgentRequestAssignmentDecision{
				AgentDeploymentID: assignedAgentID,
				Reason:            teamAssignmentReason(request, assignedAgentID),
				DecidedAt:         now,
			}
		}
		child := buildCollaborationChildRun(source, updated, now, s.newID())
		task, err := agentrun.PersistedConversationTaskSchedulingOrigin(ctx, s.runs, source, s.store.GetAgentRequest)
		if err != nil {
			return nil, err
		}
		if task != nil && task.Mode == models.ConversationTaskModeContinuation {
			child.Kind = models.RunKindAgentWork
		}
		entrypoint, err := agentrun.RequestedAcceptedChildEntrypoint(updated.SharedContext)
		if err != nil {
			return nil, err
		}
		childRequest := models.CreateAgentRunRequest{
			Kind: child.Kind, Scope: child.Scope, ObjectiveID: child.ObjectiveID, ParentRunID: child.ParentRunID,
			Owner: child.Owner, AssignedAgentID: child.AssignedAgentID, ActingUser: child.ActingUser, Entrypoint: entrypoint,
			ConcurrencyKey: child.ConcurrencyKey, ResourceRequirements: portfolio.CloneResourceQuantities(child.ResourceRequirements),
			Goal: child.Goal, Source: child.Source, Priority: child.Priority, Deadline: child.Deadline,
			Context: child.Context, Plan: child.Plan, Checkpoint: child.Checkpoint, WakeCondition: child.WakeCondition,
			Budget: child.Budget, Policy: child.Policy,
		}
		inherit := entrypoint != "" && child.AssignedAgentID == source.AssignedAgentID && entrypoint == source.Entrypoint
		if err := agentrun.PrepareAcceptedChildRun(ctx, source, &childRequest, s.executionPreparer, inherit); err != nil {
			return nil, err
		}
		child.Entrypoint, child.Context, child.Plan = childRequest.Entrypoint, childRequest.Context, childRequest.Plan
		if child.Entrypoint != "" {
			child.Kind = models.RunKindAgentWork
		}
		if err := child.Validate(); err != nil {
			return nil, err
		}
		updated.ChildRunID = child.ID
		record.ChildRun = child
		record.SourceRun, err = acceptedSourceRun(source, updated, now)
		if err != nil {
			return nil, err
		}
		record.ExpectedSourceRevision = source.Revision
		if groupedDependency != nil {
			record.SourceRun.Status = source.Status
			record.SourceRun.WakeCondition = models.CloneWakeCondition(source.WakeCondition)
		}
		eventType = "collaboration.accepted"
		if updated.Kind == models.AgentRequestKindHandoff {
			eventType = "handoff.accepted"
		} else if updated.Kind == models.AgentRequestKindEscalation {
			eventType = "escalation.accepted"
		}
		summary = fmt.Sprintf("%s %s accepted the work", actor.Type, actor.ID)
		record.ChildEvent = collaborationEvent(child, updated, eventType, summary, actor, now)
	default:
		return nil, errors.New("agent request decision is invalid")
	}
	record.SourceEvent = collaborationEvent(source, updated, eventType, summary, actor, now)
	linkAgentRequestDecisionRun(record.SourceEvent, req.DecisionRunID)
	linkAgentRequestDecisionRun(record.ChildEvent, req.DecisionRunID)
	events, err := s.store.RespondAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	if record.DependencyResolution != nil {
		source, err = s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if err != nil {
			return nil, err
		}
	} else if record.SourceRun != nil {
		source = record.SourceRun
	}
	return &AgentRequestResult{Request: models.CloneAgentRequest(updated), Source: models.CloneAgentRun(source), Child: models.CloneAgentRun(record.ChildRun), Events: events}, nil
}

func (s *CollaborationService) validateDecisionRunAuthority(ctx context.Context, request *models.AgentRequest, actor models.CollaborationParty, decisionRunID string) error {
	decisionRunID = strings.TrimSpace(decisionRunID)
	if decisionRunID == "" {
		return nil
	}
	run, err := s.runs.GetAgentRun(ctx, request.Scope, decisionRunID)
	if err != nil {
		return err
	}
	if run == nil || run.Source != models.RunSourceRequestDecision || run.Status != models.AgentRunStatusCompleted ||
		run.AssignedAgentID != actor.ID || actor.Type != models.OwnerTypeAgent ||
		run.Owner != (models.ObjectiveOwner{Type: request.Recipient.Type, ID: request.Recipient.ID}) {
		return ErrAgentRequestUnauthorized
	}
	inbox, _ := run.Context[agentrun.AgentRequestInboxContextKey].(map[string]interface{})
	inboxRequestID, _ := inbox["requestId"].(string)
	if strings.TrimSpace(inboxRequestID) != request.ID {
		return ErrAgentRequestUnauthorized
	}
	revision, revisionErr := positiveInt64(inbox["requestRevision"])
	if revisionErr != nil || revision != request.Revision {
		return ErrAgentRequestUnauthorized
	}
	return nil
}

func linkAgentRequestDecisionRun(event *models.ActivityEvent, decisionRunID string) {
	if event == nil || strings.TrimSpace(decisionRunID) == "" {
		return
	}
	event.CausationID = strings.TrimSpace(decisionRunID)
	if event.Payload == nil {
		event.Payload = make(map[string]interface{})
	}
	event.Payload["decisionRunId"] = strings.TrimSpace(decisionRunID)
}

func sourceWaitsForAgentRequestDecision(source *models.AgentRun, requestID string) bool {
	return source != nil && source.Status == models.AgentRunStatusWaitingForAgent && source.WakeCondition != nil &&
		source.WakeCondition.Type == "agent_request_decision" &&
		source.WakeCondition.Reference == strings.TrimSpace(requestID)
}

func sourceAwaitingAgentRequestDecision(source *models.AgentRun, request *models.AgentRequest, now time.Time) (*models.AgentRun, error) {
	if source == nil || request == nil {
		return nil, models.ErrRunNotFound
	}
	if sourceWaitsForAgentRequestDecision(source, request.ID) {
		return models.CloneAgentRun(source), nil
	}
	if !CanStartAgentRequestFromRun(source) {
		return nil, fmt.Errorf("%w: source run cannot await request decision from %s", models.ErrInvalidAgentRequestState, source.Status)
	}
	updated := models.CloneAgentRun(source)
	updated.Status = models.AgentRunStatusWaitingForAgent
	updated.WakeCondition = &models.WakeCondition{Type: "agent_request_decision", Reference: request.ID}
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	return updated, updated.Validate()
}

func sourceResumingAfterAgentRequestDecision(source *models.AgentRun, request *models.AgentRequest, now time.Time) (*models.AgentRun, error) {
	if source == nil || request == nil {
		return nil, models.ErrRunNotFound
	}
	if !sourceWaitsForAgentRequestDecision(source, request.ID) {
		return nil, fmt.Errorf("%w: source run is not awaiting request %s", models.ErrInvalidAgentRequestState, request.ID)
	}
	updated := models.CloneAgentRun(source)
	updated.Status = models.AgentRunStatusQueued
	updated.WakeCondition = nil
	updated.Revision++
	updated.UpdatedAt = now
	updated.AvailableAt = now
	updated.QueueEnteredAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	if updated.Output == nil {
		updated.Output = make(map[string]interface{})
	}
	results, _ := updated.Output["collaborationResults"].(map[string]interface{})
	results = maps.Clone(results)
	if results == nil {
		results = make(map[string]interface{})
	}
	results[request.ID] = map[string]interface{}{
		"requestId": request.ID, "status": request.Status, "message": request.Response,
		"clarification": request.Clarification, "recipient": request.Recipient,
	}
	updated.Output["collaborationResults"] = results
	return updated, updated.Validate()
}

func acceptedSourceRun(source *models.AgentRun, request *models.AgentRequest, now time.Time) (*models.AgentRun, error) {
	updated := models.CloneAgentRun(source)
	if err := models.AddRunBudgetAllocation(updated, request.ID, request.BudgetAllocation); err != nil {
		return nil, err
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	requiresCompletionReview := request.DelegationPolicy != nil && request.DelegationPolicy.RequireCompletionReview
	if request.Kind == models.AgentRequestKindHandoff && !requiresCompletionReview {
		updated.Status = models.AgentRunStatusCompleted
		updated.WakeCondition = nil
		updated.Output = map[string]interface{}{"handoffRequestId": request.ID, "childRunId": request.ChildRunID}
		updated.CompletedAt = &now
	} else {
		updated.Status = models.AgentRunStatusWaitingForDependency
		updated.WakeCondition = &models.WakeCondition{Type: "agent_request", Reference: request.ID}
	}
	return updated, nil
}
