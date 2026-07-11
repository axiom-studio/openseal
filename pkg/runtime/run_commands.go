package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AgentRunCommandKind string

const (
	AgentRunCommandPause     AgentRunCommandKind = "pause"
	AgentRunCommandResume    AgentRunCommandKind = "resume"
	AgentRunCommandCancel    AgentRunCommandKind = "cancel"
	AgentRunCommandIntervene AgentRunCommandKind = "intervene"
)

type AgentRunCommandRequest struct {
	Scope            Scope
	RunID            string
	ExpectedRevision int64
	Kind             AgentRunCommandKind
	Actor            ActivityActor
	Summary          string
	Instruction      string
	Visibility       ActivityVisibility
}

type AgentRunCommandResult struct {
	Run   *AgentRun      `json:"run"`
	Event *ActivityEvent `json:"event,omitempty"`
}

type RunCommandStore interface {
	PortfolioStore
	RunActivityStore
}

type RunCommandService struct {
	store RunCommandStore
	now   func() time.Time
}

func NewRunCommandService(store RunCommandStore) *RunCommandService {
	return &RunCommandService{store: store, now: time.Now}
}

// CreateAgentRun creates the canonical durable run and its first audit event
// atomically. A supplied idempotency key is scope-bound, stored only as a hash,
// and conflicts if replayed with different work.
func (s *RunCommandService) CreateAgentRun(ctx context.Context, req CreateAgentRunRequest) (*AgentRunCommandResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("run command store is not configured")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if len(key) > 256 {
		return nil, fmt.Errorf("%w: idempotency key cannot exceed 256 characters", ErrInvalidAgentRun)
	}
	fingerprint, err := runCreationFingerprint(req)
	if err != nil {
		return nil, err
	}
	runID := uuid.NewString()
	keyHash := ""
	if key != "" {
		keyHash = hashString(key)
		runID = runIDForIdempotencyKey(req.Scope, key)
		current, err := s.store.GetAgentRun(ctx, req.Scope, runID)
		if err != nil {
			return nil, err
		}
		if current != nil {
			return replayedRun(current, fingerprint)
		}
	}
	now := s.now()
	run, err := buildAgentRun(ctx, s.store, req, runID, now)
	if err != nil {
		return nil, err
	}
	run.IdempotencyKeyHash = keyHash
	run.CreationFingerprint = fingerprint
	actor := req.Actor
	if strings.TrimSpace(actor.Type) == "" {
		actor = ActivityActor{Type: "system", ID: "openseal"}
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.created", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID,
		ParentRunID: run.ParentRunID, TeamID: teamIDForRun(run), Actor: actor,
		Summary: "Run created", Visibility: visibility, CreatedAt: now,
		Payload: map[string]interface{}{"status": run.Status, "source": run.Source, "owner": run.Owner},
	}
	persisted, err := s.store.CreateAgentRunWithEvent(ctx, run, event)
	if err != nil {
		if key != "" {
			current, loadErr := s.store.GetAgentRun(ctx, req.Scope, runID)
			if loadErr == nil && current != nil {
				return replayedRun(current, fingerprint)
			}
		}
		return nil, err
	}
	return &AgentRunCommandResult{Run: run, Event: persisted}, nil
}

func runIDForIdempotencyKey(scope Scope, key string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(scope.Kind+"\x00"+scope.ID+"\x00"+hashString(strings.TrimSpace(key)))).String()
}

func replayedRun(current *AgentRun, fingerprint string) (*AgentRunCommandResult, error) {
	if current.CreationFingerprint != fingerprint {
		return nil, ErrRunIdempotency
	}
	return &AgentRunCommandResult{Run: current}, nil
}

func (s *RunCommandService) CommandAgentRun(ctx context.Context, req AgentRunCommandRequest) (*AgentRunCommandResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("run command store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return nil, fmt.Errorf("%w: run id is required", ErrInvalidRunCommand)
	}
	if req.ExpectedRevision <= 0 {
		return nil, fmt.Errorf("%w: expected revision must be positive", ErrInvalidRunCommand)
	}
	current, err := s.store.GetAgentRun(ctx, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrRunNotFound
	}
	if current.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	transition, err := commandTransition(current, req, s.now())
	if err != nil {
		return nil, err
	}
	activity := NewRunActivityService(s.store, s.store)
	run, event, err := activity.TransitionRun(ctx, req.Scope, req.RunID, transition)
	if err != nil {
		return nil, err
	}
	return &AgentRunCommandResult{Run: run, Event: event}, nil
}

func commandTransition(current *AgentRun, req AgentRunCommandRequest, now time.Time) (RunTransitionRequest, error) {
	transition := RunTransitionRequest{
		ExpectedRevision: req.ExpectedRevision, Actor: req.Actor, Visibility: req.Visibility,
		Summary: strings.TrimSpace(req.Summary), OccurredAt: &now,
	}
	switch req.Kind {
	case AgentRunCommandPause:
		if current.Status == AgentRunStatusPaused || isTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot pause %s run", ErrInvalidRunTransition, current.Status)
		}
		transition.Status = AgentRunStatusPaused
		transition.EventType = "run.paused"
		if transition.Summary == "" {
			transition.Summary = "Run paused"
		}
	case AgentRunCommandResume:
		if current.Status != AgentRunStatusPaused {
			return transition, fmt.Errorf("%w: cannot resume %s run", ErrInvalidRunTransition, current.Status)
		}
		transition.Status = resumedRunStatus(current.PausedFrom)
		if isWaitingRunStatus(transition.Status) {
			transition.WakeCondition = current.PausedWakeCondition
			if transition.WakeCondition == nil {
				return transition, fmt.Errorf("%w: paused waiting run has no wake condition", ErrInvalidRunTransition)
			}
		}
		transition.EventType = "run.resumed"
		if transition.Summary == "" {
			transition.Summary = "Run resumed"
		}
	case AgentRunCommandCancel:
		if isTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot cancel %s run", ErrInvalidRunTransition, current.Status)
		}
		transition.Status = AgentRunStatusCanceled
		transition.EventType = "run.canceled"
		if transition.Summary == "" {
			transition.Summary = "Run canceled"
		}
	case AgentRunCommandIntervene:
		instruction := strings.TrimSpace(req.Instruction)
		if instruction == "" {
			return transition, fmt.Errorf("%w: intervention instruction is required", ErrInvalidRunCommand)
		}
		if isTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot intervene on %s run", ErrInvalidRunTransition, current.Status)
		}
		transition.Status = current.Status
		transition.WakeCondition = current.WakeCondition
		transition.EventType = "run.intervened"
		transition.Intervention = &AgentRunIntervention{ID: uuid.NewString(), Actor: req.Actor, Instruction: instruction, CreatedAt: now}
		transition.Payload = map[string]interface{}{"interventionId": transition.Intervention.ID, "instruction": instruction}
		if transition.Summary == "" {
			transition.Summary = "Operator steered the run"
		}
	default:
		return transition, fmt.Errorf("%w: kind must be pause, resume, cancel, or intervene", ErrInvalidRunCommand)
	}
	return transition, nil
}

func resumedRunStatus(previous AgentRunStatus) AgentRunStatus {
	if isWaitingRunStatus(previous) {
		return previous
	}
	return AgentRunStatusQueued
}

func runCreationFingerprint(req CreateAgentRunRequest) (string, error) {
	payload := struct {
		Scope           Scope                  `json:"scope"`
		Kind            RunKind                `json:"kind"`
		ObjectiveID     string                 `json:"objectiveId,omitempty"`
		ParentRunID     string                 `json:"parentRunId,omitempty"`
		Owner           ObjectiveOwner         `json:"owner"`
		AssignedAgentID string                 `json:"assignedAgentId,omitempty"`
		ConcurrencyKey  string                 `json:"concurrencyKey,omitempty"`
		Goal            string                 `json:"goal"`
		Source          RunSource              `json:"source"`
		Priority        int                    `json:"priority"`
		Deadline        *time.Time             `json:"deadline,omitempty"`
		AvailableAt     *time.Time             `json:"availableAt,omitempty"`
		Context         map[string]interface{} `json:"context,omitempty"`
		Plan            map[string]interface{} `json:"plan,omitempty"`
		Checkpoint      map[string]interface{} `json:"checkpoint,omitempty"`
		WakeCondition   *WakeCondition         `json:"wakeCondition,omitempty"`
		Budget          map[string]interface{} `json:"budget,omitempty"`
		BudgetPolicy    *BudgetPolicy          `json:"budgetPolicy,omitempty"`
		Policy          map[string]interface{} `json:"policy,omitempty"`
	}{
		Scope: req.Scope, Kind: normalizeRunKind(req.Kind), ObjectiveID: req.ObjectiveID,
		ParentRunID: req.ParentRunID, Owner: req.Owner, AssignedAgentID: req.AssignedAgentID,
		ConcurrencyKey: strings.TrimSpace(req.ConcurrencyKey),
		Goal:           req.Goal, Source: req.Source, Priority: req.Priority, Deadline: req.Deadline,
		AvailableAt: req.AvailableAt, Context: req.Context, Plan: req.Plan, Checkpoint: req.Checkpoint,
		WakeCondition: req.WakeCondition, Budget: req.Budget, BudgetPolicy: req.BudgetPolicy, Policy: req.Policy,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode run creation fingerprint: %w", err)
	}
	return hashBytes(encoded), nil
}

func hashString(value string) string { return hashBytes([]byte(value)) }

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}
