package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

type createAgentRunPayload struct {
	Scope           runtime.Scope              `json:"scope"`
	ObjectiveID     string                     `json:"objectiveId,omitempty"`
	ParentRunID     string                     `json:"parentRunId,omitempty"`
	Owner           runtime.ObjectiveOwner     `json:"owner"`
	AssignedAgentID string                     `json:"assignedAgentId,omitempty"`
	Goal            string                     `json:"goal"`
	Source          runtime.RunSource          `json:"source"`
	Priority        int                        `json:"priority,omitempty"`
	Deadline        *time.Time                 `json:"deadline,omitempty"`
	AvailableAt     *time.Time                 `json:"availableAt,omitempty"`
	Context         map[string]interface{}     `json:"context,omitempty"`
	Plan            map[string]interface{}     `json:"plan,omitempty"`
	Checkpoint      map[string]interface{}     `json:"checkpoint,omitempty"`
	WakeCondition   *runtime.WakeCondition     `json:"wakeCondition,omitempty"`
	Budget          map[string]interface{}     `json:"budget,omitempty"`
	Policy          map[string]interface{}     `json:"policy,omitempty"`
	IdempotencyKey  string                     `json:"idempotencyKey,omitempty"`
	Actor           runtime.ActivityActor      `json:"actor,omitempty"`
	Visibility      runtime.ActivityVisibility `json:"visibility,omitempty"`
}

type agentRunCommandPayload struct {
	ExpectedRevision int64                       `json:"expectedRevision"`
	Kind             runtime.AgentRunCommandKind `json:"kind"`
	Actor            runtime.ActivityActor       `json:"actor,omitempty"`
	Summary          string                      `json:"summary,omitempty"`
	Instruction      string                      `json:"instruction,omitempty"`
	Visibility       runtime.ActivityVisibility  `json:"visibility,omitempty"`
}

func (s *Server) handleCreateAgentRun(w http.ResponseWriter, r *http.Request) {
	var payload createAgentRunPayload
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	result, err := runtime.NewRunCommandService(s.store).CreateAgentRun(r.Context(), runtime.CreateAgentRunRequest{
		Scope: payload.Scope, ObjectiveID: strings.TrimSpace(payload.ObjectiveID), ParentRunID: strings.TrimSpace(payload.ParentRunID),
		Owner: payload.Owner, AssignedAgentID: strings.TrimSpace(payload.AssignedAgentID), Goal: strings.TrimSpace(payload.Goal),
		Source: payload.Source, Priority: payload.Priority, Deadline: payload.Deadline, AvailableAt: payload.AvailableAt,
		Context: payload.Context, Plan: payload.Plan, Checkpoint: payload.Checkpoint, WakeCondition: payload.WakeCondition,
		Budget: payload.Budget, Policy: payload.Policy, IdempotencyKey: idempotencyKey, Actor: payload.Actor, Visibility: payload.Visibility,
	})
	if err != nil {
		s.respondAgentRunError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Event == nil {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result)
}

func (s *Server) handleListAgentRuns(w http.ResponseWriter, r *http.Request) {
	filter, err := agentRunFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	runs, err := runtime.NewPortfolioService(s.store).ListAgentRuns(r.Context(), filter)
	if err != nil {
		s.respondAgentRunError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, runs)
}

func (s *Server) handleGetAgentRun(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	run, err := runtime.NewPortfolioService(s.store).GetAgentRun(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondAgentRunError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, run)
}

func (s *Server) handleCommandAgentRun(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload agentRunCommandPayload
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := runtime.NewRunCommandService(s.store).CommandAgentRun(r.Context(), runtime.AgentRunCommandRequest{
		Scope: scope, RunID: strings.TrimSpace(r.PathValue("id")), ExpectedRevision: payload.ExpectedRevision,
		Kind: payload.Kind, Actor: payload.Actor, Summary: payload.Summary, Instruction: payload.Instruction, Visibility: payload.Visibility,
	})
	if err != nil {
		s.respondAgentRunError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func agentRunFilterFromQuery(r *http.Request) (runtime.AgentRunFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.AgentRunFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return runtime.AgentRunFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return runtime.AgentRunFilter{}, err
	}
	filter := runtime.AgentRunFilter{
		Scope: scope, ObjectiveID: strings.TrimSpace(r.URL.Query().Get("objectiveId")), ParentRunID: strings.TrimSpace(r.URL.Query().Get("parentRunId")),
		RootRunID: strings.TrimSpace(r.URL.Query().Get("rootRunId")), AssignedAgentID: strings.TrimSpace(r.URL.Query().Get("assignedAgentId")),
		Limit: limit, Offset: offset,
	}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := runtime.ObjectiveOwner{Type: runtime.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return runtime.AgentRunFilter{}, err
		}
		filter.Owner = &owner
	}
	for _, value := range queryValues(r, "status") {
		status := runtime.AgentRunStatus(value)
		if !validAgentRunStatus(status) {
			return runtime.AgentRunFilter{}, fmt.Errorf("invalid run status %q", value)
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func scopeFromQuery(r *http.Request) (runtime.Scope, error) {
	scope := runtime.Scope{Kind: strings.TrimSpace(r.URL.Query().Get("scopeKind")), ID: strings.TrimSpace(r.URL.Query().Get("scopeId"))}
	if err := scope.Validate(); err != nil {
		return runtime.Scope{}, err
	}
	return scope, nil
}

func queryValues(r *http.Request, key string) []string {
	result := make([]string, 0)
	for _, raw := range r.URL.Query()[key] {
		for _, value := range strings.Split(raw, ",") {
			if value = strings.TrimSpace(value); value != "" {
				result = append(result, value)
			}
		}
	}
	return result
}

func boundedIntQuery(r *http.Request, key string, fallback, min, max int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return value, nil
}

func validAgentRunStatus(status runtime.AgentRunStatus) bool {
	switch status {
	case runtime.AgentRunStatusQueued, runtime.AgentRunStatusPlanning, runtime.AgentRunStatusRunning, runtime.AgentRunStatusPaused,
		runtime.AgentRunStatusSleeping, runtime.AgentRunStatusWaitingForDependency, runtime.AgentRunStatusWaitingForAgent,
		runtime.AgentRunStatusWaitingForApproval, runtime.AgentRunStatusWaitingForEvent, runtime.AgentRunStatusCompleted,
		runtime.AgentRunStatusFailed, runtime.AgentRunStatusCanceled:
		return true
	default:
		return false
	}
}

func decodeStrictJSON(r *http.Request, target interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("request body must contain one JSON object")
		}
		return err
	}
	return nil
}

func (s *Server) respondAgentRunError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrRunNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrRevisionConflict), errors.Is(err, runtime.ErrRunIdempotency), errors.Is(err, runtime.ErrInvalidRunTransition):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrInvalidScope), errors.Is(err, runtime.ErrInvalidOwner), errors.Is(err, runtime.ErrObjectiveNotFound):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
