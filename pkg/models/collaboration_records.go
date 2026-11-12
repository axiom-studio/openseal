package models

import (
	"errors"
	"fmt"
	"strings"
)

func ValidateAgentRequestCreateRecord(record AgentRequestCreateRecord) error {
	if record.Request == nil || record.Event == nil {
		return errors.New("agent request and source event are required")
	}
	if err := record.Request.Validate(); err != nil {
		return err
	}
	if err := record.Event.Validate(); err != nil {
		return err
	}
	if record.Request.Scope != record.Event.Scope || record.Request.SourceRunID != record.Event.RunID {
		return ErrInvalidScope
	}
	if record.SourceRun != nil {
		if err := record.SourceRun.Validate(); err != nil {
			return err
		}
		if record.SourceRun.Scope != record.Request.Scope || record.SourceRun.ID != record.Request.SourceRunID ||
			record.SourceRun.Revision != record.ExpectedSourceRevision+1 {
			return ErrInvalidScope
		}
	}
	if record.RequireInterventionCount != nil && *record.RequireInterventionCount < 0 {
		return errors.New("required intervention count must not be negative")
	}
	if transition := record.ToolTransition; transition != nil {
		if transition.Scope != record.Request.Scope || transition.RunID != record.Request.SourceRunID {
			return fmt.Errorf("%w: tool transition must belong to the request's source Run", ErrInvalidToolEntryTransition)
		}
		if strings.TrimSpace(transition.Lease.WorkerID) == "" {
			return fmt.Errorf("%w: tool transition needs the source Run lease", ErrInvalidToolEntryTransition)
		}
		if err := ValidateToolEntryTransitionRequest(*transition); err != nil {
			return err
		}
		if err := ValidateSessionEvents(transition.Scope, transition.RunID, transition.Events, ErrInvalidToolEntryTransition); err != nil {
			return err
		}
	}
	if (record.Request.DependencyGroupID == "") != (record.SourceRun != nil) {
		return errors.New("singular AgentRequest creation requires exactly one source Run wait transition")
	}
	if (strings.TrimSpace(record.DelegationTeamID) == "") != (record.MaximumConcurrent == 0) || record.MaximumConcurrent < 0 {
		return errors.New("delegation concurrency reservation requires a Team and positive maximum")
	}
	if record.MaximumConcurrent > 0 && (record.Request.DelegationPolicy == nil || record.Request.DelegationPolicy.TeamDeploymentID != record.DelegationTeamID ||
		record.Request.DelegationPolicy.MaximumConcurrent != record.MaximumConcurrent) {
		return errors.New("delegation concurrency reservation must match the request policy snapshot")
	}
	return nil
}

func ValidateAgentRequestCoordinationRecord(record AgentRequestCoordinationRecord) error {
	if record.Request == nil || record.Event == nil || record.ExpectedRequestRevision < 1 {
		return errors.New("agent request coordination requires request, expected revision, and activity event")
	}
	if err := record.Request.Validate(); err != nil {
		return err
	}
	if err := record.Event.Validate(); err != nil {
		return err
	}
	if record.Request.Revision != record.ExpectedRequestRevision+1 || record.Event.Scope != record.Request.Scope ||
		record.Event.RunID != record.Request.SourceRunID {
		return ErrInvalidScope
	}
	return nil
}

func ValidateAgentRequestResponseRecord(record AgentRequestResponseRecord) error {
	if record.Request == nil || record.SourceEvent == nil {
		return errors.New("agent request response and source event are required")
	}
	if err := record.Request.Validate(); err != nil {
		return err
	}
	if err := record.SourceEvent.Validate(); err != nil {
		return err
	}
	if record.SourceRun != nil {
		if err := record.SourceRun.Validate(); err != nil {
			return err
		}
	}
	if record.ChildRun != nil {
		if record.ChildEvent == nil {
			return errors.New("new accepted child run requires a child event")
		}
		if err := record.ChildRun.Validate(); err != nil {
			return err
		}
	}
	if record.ChildEvent != nil {
		if err := record.ChildEvent.Validate(); err != nil {
			return err
		}
	}
	if record.DependencyResolution != nil {
		if err := ValidateRunDependencyResolutionRecord(*record.DependencyResolution); err != nil {
			return err
		}
		if record.DependencyResolution.Scope != record.Request.Scope ||
			record.DependencyResolution.GroupID != record.Request.DependencyGroupID ||
			record.DependencyResolution.DependencyID != record.Request.DependencyID {
			return ErrInvalidScope
		}
	}
	return nil
}

func ValidateAgentRequestCompletionRecord(record AgentRequestCompletionRecord) error {
	if record.Request == nil || record.ChildRun == nil || record.SourceEvent == nil || record.ChildEvent == nil {
		return errors.New("agent request completion requires request, source run, child run, and both events")
	}
	reviewPending := record.Request != nil && record.Request.Status == AgentRequestStatusCompletionReview
	if reviewPending {
		if record.SourceRun != nil || record.DependencyResolution != nil {
			return errors.New("completion awaiting review cannot wake its source")
		}
	} else if (record.SourceRun == nil) == (record.DependencyResolution == nil) {
		return errors.New("agent request completion requires exactly one direct source update or dependency resolution")
	}
	if err := record.Request.Validate(); err != nil {
		return err
	}
	if record.Request.Status != AgentRequestStatusCompleted && record.Request.Status != AgentRequestStatusCompletionReview && record.Request.Status != AgentRequestStatusFailed {
		return ErrInvalidAgentRequestState
	}
	for _, run := range []*AgentRun{record.SourceRun, record.ChildRun} {
		if run == nil {
			continue
		}
		if err := run.Validate(); err != nil {
			return err
		}
	}
	if record.DependencyResolution != nil {
		if err := ValidateRunDependencyResolutionRecord(*record.DependencyResolution); err != nil {
			return err
		}
		if record.DependencyResolution.Scope != record.Request.Scope ||
			record.DependencyResolution.GroupID != record.Request.DependencyGroupID ||
			record.DependencyResolution.DependencyID != record.Request.DependencyID {
			return ErrInvalidScope
		}
	}
	for _, event := range []*ActivityEvent{record.SourceEvent, record.ChildEvent} {
		if err := event.Validate(); err != nil {
			return err
		}
	}
	return nil
}
