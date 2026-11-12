package collaboration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

// CompleteAgentRequest atomically records the recipient's result and completes
// the child run. It wakes the source immediately unless the snapshotted Team
// policy requires an independent completion review. The
// Principal is the authorized recipient; Actor is the agent or Team member
// that actually performed the completion and is retained in the audit trail.
func (s *CollaborationService) CompleteAgentRequest(ctx context.Context, req CompleteAgentRequestRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil || s.artifacts == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	request, err := s.store.GetAgentRequest(ctx, req.Scope, strings.TrimSpace(req.RequestID))
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	if req.Principal != request.Recipient {
		return nil, ErrAgentRequestUnauthorized
	}
	artifacts, err := s.resolveArtifactReferences(ctx, req.Scope, request.ArtifactRequirements, req.Artifacts)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", models.ErrInvalidArtifact, err)
	}
	if request.Status == models.AgentRequestStatusCompleted || request.Status == models.AgentRequestStatusCompletionReview {
		if request.CompletionKey != strings.TrimSpace(req.CompletionKey) || request.CompletionKey == "" {
			return nil, models.ErrInvalidAgentRequestState
		}
		if !sameAgentRequestCompletion(request, req, artifacts) {
			return nil, models.ErrAgentRequestIdempotency
		}
		source, sourceErr := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if sourceErr != nil {
			return nil, sourceErr
		}
		child, childErr := s.runs.GetAgentRun(ctx, req.Scope, request.ChildRunID)
		if childErr != nil {
			return nil, childErr
		}
		return &AgentRequestResult{Request: request, Source: source, Child: child}, nil
	}
	if request.Revision != req.ExpectedRevision {
		return nil, models.ErrRevisionConflict
	}
	if request.Status != models.AgentRequestStatusAccepted || strings.TrimSpace(request.ChildRunID) == "" {
		return nil, models.ErrInvalidAgentRequestState
	}
	actor := req.Actor
	if strings.TrimSpace(actor.ID) == "" {
		actor = req.Principal
	}
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("actor: %w", err)
	}
	if strings.TrimSpace(req.Summary) == "" || strings.TrimSpace(req.CompletionKey) == "" {
		return nil, errors.New("completion summary and idempotency key are required")
	}
	if len(request.AcceptanceCriteria) > 0 && len(req.AcceptanceEvidence) == 0 {
		return nil, errors.New("acceptance evidence is required")
	}
	if err := models.ValidateCredentialFreeContext(req.AcceptanceEvidence); err != nil {
		return nil, err
	}
	source, err := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	child, err := s.runs.GetAgentRun(ctx, req.Scope, request.ChildRunID)
	if err != nil {
		return nil, err
	}
	if source == nil || child == nil {
		return nil, models.ErrRunNotFound
	}
	if child.ParentRunID != source.ID || child.RootRunID != source.RootRunID || child.Status == models.AgentRunStatusFailed || child.Status == models.AgentRunStatusCanceled {
		return nil, models.ErrInvalidAgentRequestState
	}
	if req.ExpectedChildRevision != child.Revision {
		return nil, models.ErrRevisionConflict
	}
	now := s.now()
	updatedRequest := models.CloneAgentRequest(request)
	requiresReview := updatedRequest.DelegationPolicy != nil && updatedRequest.DelegationPolicy.RequireCompletionReview
	updatedRequest.Status = models.AgentRequestStatusCompleted
	if requiresReview {
		updatedRequest.Status = models.AgentRequestStatusCompletionReview
	}
	updatedRequest.CompletionSummary = strings.TrimSpace(req.Summary)
	updatedRequest.AcceptanceEvidence = maps.Clone(req.AcceptanceEvidence)
	updatedRequest.Artifacts = artifacts
	updatedRequest.CompletionKey = strings.TrimSpace(req.CompletionKey)
	updatedRequest.CompletionSubmittedAt = &now
	updatedRequest.Revision++
	updatedRequest.UpdatedAt = now
	if !requiresReview {
		updatedRequest.CompletedAt = &now
		updatedRequest.ResolvedAt = &now
	}

	sharedChildOutput := maps.Clone(child.Output)
	updatedChild := completedCollaborationChildRun(child, updatedRequest, now)
	var updatedSource *models.AgentRun
	var dependencyResolution *models.RunDependencyResolutionRecord
	if requiresReview {
		updatedSource = models.CloneAgentRun(source)
	} else if updatedRequest.DependencyGroupID != "" {
		edge, edgeErr := s.groupedRequestDependency(ctx, updatedRequest, source, true)
		if edgeErr != nil {
			return nil, edgeErr
		}
		dependencyResolution = &models.RunDependencyResolutionRecord{
			Scope: updatedRequest.Scope, GroupID: updatedRequest.DependencyGroupID, DependencyID: updatedRequest.DependencyID,
			ExpectedDependencyRevision: edge.Revision, State: models.RunDependencyStateSatisfied,
			Result: map[string]interface{}{
				"requestId": updatedRequest.ID, "childRunId": updatedRequest.ChildRunID,
				"summary": updatedRequest.CompletionSummary, "acceptanceEvidence": maps.Clone(updatedRequest.AcceptanceEvidence),
				"output": sharedChildOutput,
			},
			Artifacts: updatedRequest.Artifacts, Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID},
			Visibility: models.ActivityVisibilityTeam, OccurredAt: now,
		}
	} else {
		updatedSource, err = completedCollaborationSourceRun(source, updatedRequest, sharedChildOutput, now)
		if err != nil {
			return nil, err
		}
	}
	eventType := "collaboration.completed"
	if requiresReview {
		eventType = "collaboration.completion_review_requested"
	}
	if request.Kind == models.AgentRequestKindHandoff {
		eventType = "handoff.completed"
		if requiresReview {
			eventType = "handoff.completion_review_requested"
		}
	} else if request.Kind == models.AgentRequestKindEscalation {
		eventType = "escalation.completed"
		if requiresReview {
			eventType = "escalation.completion_review_requested"
		}
	}
	summary := fmt.Sprintf("%s %s completed the work", actor.Type, actor.ID)
	if requiresReview {
		summary = fmt.Sprintf("%s %s submitted completed work for independent review", actor.Type, actor.ID)
	}
	record := models.AgentRequestCompletionRecord{
		Request: updatedRequest, ExpectedRequestRevision: request.Revision,
		ExpectedSourceRevision: source.Revision, DependencyResolution: dependencyResolution,
		ChildRun: updatedChild, ExpectedChildRevision: child.Revision,
		SourceEvent: collaborationCompletionEvent(source, updatedRequest, eventType, summary, actor, now),
		ChildEvent:  collaborationCompletionEvent(child, updatedRequest, eventType, summary, actor, now),
	}
	if !requiresReview {
		record.SourceRun = updatedSource
	}
	events, err := s.store.CompleteAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	if dependencyResolution != nil {
		updatedSource, err = s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if err != nil {
			return nil, err
		}
	}
	return &AgentRequestResult{
		Request: models.CloneAgentRequest(updatedRequest), Source: models.CloneAgentRun(updatedSource), Child: models.CloneAgentRun(updatedChild), Events: events,
	}, nil
}

// ReviewAgentRequestCompletion resolves the independent review checkpoint
// required by the snapshotted Team policy. The requester reviews the result;
// the recipient that performed the work cannot approve its own completion.
func (s *CollaborationService) ReviewAgentRequestCompletion(ctx context.Context, req ReviewAgentRequestCompletionRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	request, err := s.store.GetAgentRequest(ctx, req.Scope, strings.TrimSpace(req.RequestID))
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if request.CompletionReviewKey != "" {
		if request.CompletionReviewKey != key || key == "" {
			return nil, models.ErrInvalidAgentRequestState
		}
		source, sourceErr := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if sourceErr != nil {
			return nil, sourceErr
		}
		child, childErr := s.runs.GetAgentRun(ctx, req.Scope, request.ChildRunID)
		if childErr != nil {
			return nil, childErr
		}
		return &AgentRequestResult{Request: request, Source: source, Child: child}, nil
	}
	if request.Revision != req.ExpectedRevision {
		return nil, models.ErrRevisionConflict
	}
	if request.Status != models.AgentRequestStatusCompletionReview || request.DelegationPolicy == nil || !request.DelegationPolicy.RequireCompletionReview {
		return nil, models.ErrInvalidAgentRequestState
	}
	if req.ResolveDisagreement != request.ReviewDisagreement {
		return nil, fmt.Errorf("%w: disagreement resolution must match the durable review state", models.ErrInvalidAgentRequestState)
	}
	if req.Principal != request.Requester || req.Principal == request.Recipient {
		return nil, ErrAgentRequestUnauthorized
	}
	actor := req.Actor
	if strings.TrimSpace(actor.ID) == "" {
		actor = req.Principal
	}
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("actor: %w", err)
	}
	if actor.Type != models.OwnerTypeAgent || actor.ID == request.AssignedAgentID {
		return nil, fmt.Errorf("%w: completed work requires an independent Agent reviewer", ErrAgentRequestUnauthorized)
	}
	for _, decision := range request.CompletionReviews {
		if decision.IdempotencyKey == key && key != "" {
			if decision.Reviewer == actor && decision.Approve == req.Approve && decision.Summary == strings.TrimSpace(req.Summary) {
				return &AgentRequestResult{Request: request}, nil
			}
			return nil, models.ErrAgentRequestIdempotency
		}
		if decision.Reviewer == actor {
			return nil, fmt.Errorf("%w: reviewer already submitted a completion decision", models.ErrInvalidAgentRequestState)
		}
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" || key == "" {
		return nil, errors.New("completion review summary and idempotency key are required")
	}
	source, err := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	child, err := s.runs.GetAgentRun(ctx, req.Scope, request.ChildRunID)
	if err != nil {
		return nil, err
	}
	if source == nil || child == nil || child.Status != models.AgentRunStatusCompleted || child.ParentRunID != source.ID || req.ExpectedChildRevision != child.Revision {
		return nil, models.ErrInvalidAgentRequestState
	}
	now := s.now()
	updatedRequest := models.CloneAgentRequest(request)
	updatedRequest.Revision++
	updatedRequest.UpdatedAt = now
	updatedRequest.CompletionReviews = append(updatedRequest.CompletionReviews, models.AgentRequestCompletionReviewDecision{
		Reviewer: actor, Approve: req.Approve, Summary: summary, IdempotencyKey: key, DecidedAt: now,
	})
	quorum := completionReviewQuorum(updatedRequest.DelegationPolicy)
	approvals, rejections := models.CompletionReviewCounts(updatedRequest.CompletionReviews)
	disagreement := approvals > 0 && rejections > 0 && updatedRequest.DelegationPolicy.EscalateOnDisagreement && !req.ResolveDisagreement
	if disagreement || approvals < quorum && rejections < quorum {
		updatedRequest.ReviewDisagreement = disagreement
		eventType := "collaboration.completion_review_recorded"
		eventSummary := fmt.Sprintf("Agent %s recorded completion review %d of %d", actor.ID, len(updatedRequest.CompletionReviews), quorum)
		if disagreement {
			eventType = "collaboration.completion_review_disagreement"
			eventSummary = fmt.Sprintf("Completion reviewers disagreed after %d decisions; escalation is required", len(updatedRequest.CompletionReviews))
		}
		event := collaborationCompletionEvent(source, updatedRequest, eventType, eventSummary, actor, now)
		persisted, persistErr := s.store.CoordinateAgentRequest(ctx, models.AgentRequestCoordinationRecord{
			Request: updatedRequest, ExpectedRequestRevision: request.Revision, Event: event,
		})
		if persistErr != nil {
			return nil, persistErr
		}
		return &AgentRequestResult{Request: models.CloneAgentRequest(updatedRequest), Source: models.CloneAgentRun(source), Child: models.CloneAgentRun(child), Events: []*models.ActivityEvent{persisted}}, nil
	}
	updatedRequest.CompletionReviewer = &actor
	updatedRequest.CompletionReviewSummary = summary
	updatedRequest.CompletionReviewKey = key
	updatedRequest.ReviewDisagreement = false
	updatedRequest.ReviewedAt = &now
	updatedRequest.ResolvedAt = &now
	updatedChild := models.CloneAgentRun(child)
	updatedChild.Revision++
	updatedChild.UpdatedAt = now
	var updatedSource *models.AgentRun
	var dependencyResolution *models.RunDependencyResolutionRecord
	eventType := "collaboration.completion_review_approved"
	eventSummary := fmt.Sprintf("%s %s approved the completed work", actor.Type, actor.ID)
	dependencyState := models.RunDependencyStateSatisfied
	if req.ResolveDisagreement {
		eventType = "collaboration.completion_review_disagreement_resolved"
		eventSummary = fmt.Sprintf("Agent %s resolved the completion review disagreement", actor.ID)
	}
	if approvals >= quorum {
		updatedRequest.Status = models.AgentRequestStatusCompleted
		updatedRequest.CompletedAt = &now
		updatedChild.Output = collaborationCompletionOutput(updatedChild.Output, updatedRequest, maps.Clone(child.Output))
		if updatedRequest.DependencyGroupID == "" {
			updatedSource, err = completedCollaborationSourceRun(source, updatedRequest, maps.Clone(child.Output), now)
		}
	} else {
		updatedRequest.Status = models.AgentRequestStatusFailed
		updatedRequest.ResolutionReason = summary
		eventType = "collaboration.completion_review_rejected"
		eventSummary = fmt.Sprintf("%s %s rejected the completed work", actor.Type, actor.ID)
		dependencyState = models.RunDependencyStateFailed
		if updatedRequest.DependencyGroupID == "" {
			updatedSource, err = failedCollaborationSourceRun(source, updatedRequest, now)
		}
	}
	if err != nil {
		return nil, err
	}
	if request.Kind == models.AgentRequestKindHandoff {
		eventType = strings.Replace(eventType, "collaboration.", "handoff.", 1)
	} else if request.Kind == models.AgentRequestKindEscalation {
		eventType = strings.Replace(eventType, "collaboration.", "escalation.", 1)
	}
	if updatedRequest.DependencyGroupID != "" {
		edge, edgeErr := s.groupedRequestDependency(ctx, updatedRequest, source, true)
		if edgeErr != nil {
			return nil, edgeErr
		}
		dependencyResolution = &models.RunDependencyResolutionRecord{
			Scope: updatedRequest.Scope, GroupID: updatedRequest.DependencyGroupID, DependencyID: updatedRequest.DependencyID,
			ExpectedDependencyRevision: edge.Revision, State: dependencyState,
			Result: map[string]interface{}{
				"requestId": updatedRequest.ID, "childRunId": updatedRequest.ChildRunID,
				"summary": updatedRequest.CompletionSummary, "review": summary,
				"acceptanceEvidence": maps.Clone(updatedRequest.AcceptanceEvidence), "output": maps.Clone(child.Output),
			},
			Error: updatedRequest.ResolutionReason, Artifacts: updatedRequest.Artifacts,
			Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID}, Visibility: models.ActivityVisibilityTeam, OccurredAt: now,
		}
	}
	record := models.AgentRequestCompletionRecord{
		Request: updatedRequest, ExpectedRequestRevision: request.Revision,
		SourceRun: updatedSource, ExpectedSourceRevision: source.Revision, DependencyResolution: dependencyResolution,
		ChildRun: updatedChild, ExpectedChildRevision: child.Revision,
		SourceEvent: collaborationCompletionEvent(source, updatedRequest, eventType, eventSummary, actor, now),
		ChildEvent:  collaborationCompletionEvent(child, updatedRequest, eventType, eventSummary, actor, now),
	}
	events, err := s.store.CompleteAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	if dependencyResolution != nil {
		updatedSource, err = s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if err != nil {
			return nil, err
		}
	}
	return &AgentRequestResult{Request: models.CloneAgentRequest(updatedRequest), Source: models.CloneAgentRun(updatedSource), Child: models.CloneAgentRun(updatedChild), Events: events}, nil
}

// ResolveTerminalAgentRequestChild closes the collaboration lifecycle from an
// authoritative terminal child Run. Artifact-bearing requests remain accepted
// until their required immutable artifacts are explicitly supplied.
func (s *CollaborationService) ResolveTerminalAgentRequestChild(ctx context.Context, child *models.AgentRun) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil || child == nil {
		return nil, errors.New("collaboration service and child run are required")
	}
	collaboration, ok := child.Context["collaboration"].(map[string]interface{})
	if !ok {
		return nil, nil
	}
	requestID, _ := collaboration["requestId"].(string)
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, nil
	}
	request, err := s.GetAgentRequest(ctx, child.Scope, requestID)
	if err != nil {
		return nil, err
	}
	if request.ChildRunID != child.ID {
		return nil, models.ErrInvalidAgentRequestState
	}
	if request.Status == models.AgentRequestStatusCompletionReview || request.Status == models.AgentRequestStatusCompleted || request.Status == models.AgentRequestStatusFailed || request.Status == models.AgentRequestStatusCanceled {
		return &AgentRequestResult{Request: request, Child: child}, nil
	}
	if request.Status != models.AgentRequestStatusAccepted {
		return nil, models.ErrInvalidAgentRequestState
	}
	if child.Status == models.AgentRunStatusCompleted {
		if len(request.ArtifactRequirements) > 0 {
			artifacts, present, err := terminalChildArtifactReferences(child.Output)
			if err != nil {
				return nil, err
			}
			if !present {
				return &AgentRequestResult{Request: request, Child: child}, nil
			}
			evidence := map[string]interface{}{"runOutput": maps.Clone(child.Output)}
			return s.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
				Scope: child.Scope, RequestID: request.ID, ExpectedRevision: request.Revision, ExpectedChildRevision: child.Revision,
				Principal: request.Recipient, Actor: request.Recipient, Summary: terminalChildSummary(child.Output),
				AcceptanceEvidence: evidence, Artifacts: artifacts, CompletionKey: "terminal-child:" + child.ID,
			})
		}
		evidence := map[string]interface{}{}
		if len(request.AcceptanceCriteria) > 0 {
			evidence["runOutput"] = maps.Clone(child.Output)
			if len(child.Output) == 0 {
				evidence["runStatus"] = string(child.Status)
			}
		}
		return s.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
			Scope: child.Scope, RequestID: request.ID, ExpectedRevision: request.Revision, ExpectedChildRevision: child.Revision,
			Principal: request.Recipient, Actor: request.Recipient, Summary: terminalChildSummary(child.Output),
			AcceptanceEvidence: evidence, CompletionKey: "terminal-child:" + child.ID,
		})
	}
	if child.Status != models.AgentRunStatusFailed && child.Status != models.AgentRunStatusCanceled {
		return nil, models.ErrInvalidAgentRequestState
	}
	now := s.now()
	updated := models.CloneAgentRequest(request)
	updated.Status = models.AgentRequestStatusFailed
	if child.Status == models.AgentRunStatusCanceled {
		updated.Status = models.AgentRequestStatusCanceled
	}
	updated.ResolutionReason = strings.TrimSpace(child.Error)
	if updated.ResolutionReason == "" {
		updated.ResolutionReason = "delegated child run " + string(child.Status)
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.ResolvedAt = &now
	record := models.AgentRequestResponseRecord{Request: updated, ExpectedRequestRevision: request.Revision}
	source, err := s.runs.GetAgentRun(ctx, child.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	if request.DependencyGroupID != "" {
		edge, edgeErr := s.groupedRequestDependency(ctx, request, source, true)
		if edgeErr != nil {
			return nil, edgeErr
		}
		state := models.RunDependencyStateFailed
		if child.Status == models.AgentRunStatusCanceled {
			state = models.RunDependencyStateCanceled
		}
		record.DependencyResolution = &models.RunDependencyResolutionRecord{
			Scope: request.Scope, GroupID: request.DependencyGroupID, DependencyID: request.DependencyID,
			ExpectedDependencyRevision: edge.Revision, State: state, Error: updated.ResolutionReason,
			Actor:      models.ActivityActor{Type: string(request.Recipient.Type), ID: request.Recipient.ID},
			Visibility: models.ActivityVisibilityTeam, OccurredAt: now,
		}
	} else {
		record.SourceRun, err = failedCollaborationSourceRun(source, updated, now)
		if err != nil {
			return nil, err
		}
		record.ExpectedSourceRevision = source.Revision
	}
	eventType := "collaboration.failed"
	if request.Kind == models.AgentRequestKindHandoff {
		eventType = "handoff.failed"
	} else if request.Kind == models.AgentRequestKindEscalation {
		eventType = "escalation.failed"
	}
	if child.Status == models.AgentRunStatusCanceled {
		eventType = "collaboration.canceled"
		if request.Kind == models.AgentRequestKindHandoff {
			eventType = "handoff.canceled"
		} else if request.Kind == models.AgentRequestKindEscalation {
			eventType = "escalation.canceled"
		}
	}
	summary := fmt.Sprintf("%s %s ended delegated work: %s", request.Recipient.Type, request.Recipient.ID, updated.ResolutionReason)
	record.SourceEvent = collaborationEvent(source, updated, eventType, summary, request.Recipient, now)
	record.ChildEvent = collaborationEvent(child, updated, eventType, summary, request.Recipient, now)
	events, err := s.store.RespondAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	if record.DependencyResolution != nil {
		record.SourceRun, err = s.runs.GetAgentRun(ctx, child.Scope, request.SourceRunID)
		if err != nil {
			return nil, err
		}
	}
	return &AgentRequestResult{Request: updated, Source: record.SourceRun, Child: child, Events: events}, nil
}

func terminalChildArtifactReferences(output map[string]interface{}) ([]models.ArtifactReference, bool, error) {
	if len(output) == 0 {
		return nil, false, nil
	}
	raw, present := output["artifactRefs"]
	if !present {
		raw, present = output["artifacts"]
	}
	if !present {
		return nil, false, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, true, fmt.Errorf("encode terminal child artifact references: %w", err)
	}
	var references []models.ArtifactReference
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&references); err != nil {
		return nil, true, fmt.Errorf("decode terminal child artifact references: %w", err)
	}
	if len(references) == 0 {
		return nil, true, errors.New("terminal child artifact references cannot be empty")
	}
	return references, true, nil
}

func completedCollaborationChildRun(child *models.AgentRun, request *models.AgentRequest, now time.Time) *models.AgentRun {
	updated := models.CloneAgentRun(child)
	sharedOutput := maps.Clone(updated.Output)
	updated.Status = models.AgentRunStatusCompleted
	updated.Revision++
	updated.UpdatedAt = now
	updated.CompletedAt = &now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.WakeCondition = nil
	updated.Output = collaborationCompletionOutput(updated.Output, request, sharedOutput)
	return updated
}

func completedCollaborationSourceRun(source *models.AgentRun, request *models.AgentRequest, childOutput map[string]interface{}, now time.Time) (*models.AgentRun, error) {
	updated := models.CloneAgentRun(source)
	if updated.Status == models.AgentRunStatusWaitingForDependency && updated.WakeCondition != nil &&
		updated.WakeCondition.Type == "agent_request" && updated.WakeCondition.Reference == request.ID {
		updated.WakeCondition = nil
		updated.LastWakeSignalID = "agent_request:" + request.ID + ":" + fmt.Sprint(request.Revision)
		if request.Kind == models.AgentRequestKindHandoff {
			updated.Status = models.AgentRunStatusCompleted
			updated.CompletedAt = &now
		} else {
			updated.Status = models.AgentRunStatusQueued
			updated.AvailableAt = now
			updated.QueueEnteredAt = now
		}
	} else if updated.Status != models.AgentRunStatusCompleted {
		return nil, fmt.Errorf("%w: source run is not waiting on request %s", models.ErrInvalidAgentRequestState, request.ID)
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Output = collaborationCompletionOutput(updated.Output, request, childOutput)
	return updated, nil
}

func failedCollaborationSourceRun(source *models.AgentRun, request *models.AgentRequest, now time.Time) (*models.AgentRun, error) {
	updated := models.CloneAgentRun(source)
	if updated.Status == models.AgentRunStatusWaitingForDependency && updated.WakeCondition != nil &&
		updated.WakeCondition.Type == "agent_request" && updated.WakeCondition.Reference == request.ID {
		updated.Status = models.AgentRunStatusQueued
		updated.AvailableAt = now
		updated.QueueEnteredAt = now
		updated.WakeCondition = nil
		updated.LastWakeSignalID = "agent_request:" + request.ID + ":" + fmt.Sprint(request.Revision)
	} else if updated.Status != models.AgentRunStatusCompleted {
		return nil, fmt.Errorf("%w: source run is not waiting on request %s", models.ErrInvalidAgentRequestState, request.ID)
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Output = collaborationResolutionOutput(updated.Output, request)
	return updated, nil
}

func collaborationCompletionOutput(existing map[string]interface{}, request *models.AgentRequest, childOutput map[string]interface{}) map[string]interface{} {
	result := maps.Clone(existing)
	if result == nil {
		result = make(map[string]interface{})
	}
	results := make(map[string]interface{})
	if current, ok := result["collaborationResults"].(map[string]interface{}); ok {
		for key, value := range current {
			results[key] = value
		}
	}
	results[request.ID] = map[string]interface{}{
		"requestId": request.ID, "kind": request.Kind, "childRunId": request.ChildRunID,
		"status":  request.Status,
		"summary": request.CompletionSummary, "acceptanceEvidence": maps.Clone(request.AcceptanceEvidence),
		"artifacts": models.CloneArtifactReferences(request.Artifacts), "output": maps.Clone(childOutput),
	}
	result["collaborationResults"] = results
	return result
}

func collaborationResolutionOutput(existing map[string]interface{}, request *models.AgentRequest) map[string]interface{} {
	result := maps.Clone(existing)
	if result == nil {
		result = make(map[string]interface{})
	}
	results := make(map[string]interface{})
	if current, ok := result["collaborationResults"].(map[string]interface{}); ok {
		for key, value := range current {
			results[key] = value
		}
	}
	results[request.ID] = map[string]interface{}{
		"requestId": request.ID, "kind": request.Kind, "childRunId": request.ChildRunID,
		"status": request.Status, "reason": request.ResolutionReason,
	}
	result["collaborationResults"] = results
	return result
}

func terminalChildSummary(output map[string]interface{}) string {
	preferred := []string{"summary", "response", "result", "outcome", "followUp"}
	for _, key := range preferred {
		if value, ok := output[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	keys := make([]string, 0, len(output))
	for key := range output {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if value, ok := output[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "Completed delegated work"
}

func collaborationEvent(run *models.AgentRun, request *models.AgentRequest, eventType, summary string, actor models.CollaborationParty, now time.Time) *models.ActivityEvent {
	teamID := models.TeamIDForRun(run)
	if teamID == "" && request.Recipient.Type == models.OwnerTypeTeam {
		teamID = request.Recipient.ID
	}
	return &models.ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: eventType, Severity: models.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID,
		TeamID: teamID, ConversationRefs: append([]string(nil), request.ConversationRefs...),
		Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID}, Summary: summary, Visibility: models.ActivityVisibilityTeam,
		CorrelationID: request.ID, CausationID: request.SourceRunID, CreatedAt: now,
		Payload: map[string]interface{}{
			"requestId": request.ID, "kind": request.Kind, "status": request.Status, "recipient": request.Recipient,
			"semanticRole": request.SemanticRole, "acceptancePolicy": request.AcceptancePolicy,
			"assignedAgentId": request.AssignedAgentID, "childRunId": request.ChildRunID,
		},
	}
}

func collaborationCompletionEvent(run *models.AgentRun, request *models.AgentRequest, eventType, summary string, actor models.CollaborationParty, now time.Time) *models.ActivityEvent {
	event := collaborationEvent(run, request, eventType, summary, actor, now)
	artifacts := make([]map[string]interface{}, 0, len(request.Artifacts))
	for _, artifact := range request.Artifacts {
		artifacts = append(artifacts, map[string]interface{}{
			"id": artifact.ID, "version": artifact.Version, "requirementName": artifact.RequirementName, "name": artifact.Name,
			"type": artifact.Type, "mediaType": artifact.MediaType, "digest": artifact.Digest, "sizeBytes": artifact.SizeBytes,
		})
	}
	evidenceKeys := make([]string, 0, len(request.AcceptanceEvidence))
	for key := range request.AcceptanceEvidence {
		evidenceKeys = append(evidenceKeys, key)
	}
	sort.Strings(evidenceKeys)
	event.Payload["completionSummary"] = request.CompletionSummary
	event.Payload["artifacts"] = artifacts
	event.Payload["acceptanceEvidenceKeys"] = evidenceKeys
	return event
}
