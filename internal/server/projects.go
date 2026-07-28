package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

var standaloneProjectActor = runtime.ActivityActor{Type: "user", ID: "standalone-operator"}

func (s *Server) projectService(w http.ResponseWriter) (*runtime.ProjectService, bool) {
	store, ok := s.store.(runtime.ProjectStore)
	if !ok {
		s.respondError(w, http.StatusServiceUnavailable, "project capability is unavailable")
		return nil, false
	}
	return runtime.NewProjectService(store, s.store), true
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	service, ok := s.projectService(w)
	if !ok {
		return
	}
	var payload kernelapi.CreateProjectRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(payload.IdempotencyKey)
	}
	project := &runtime.Project{
		ID: strings.TrimSpace(payload.ID), Scope: payload.Scope, Owner: payload.Owner,
		Title: strings.TrimSpace(payload.Title), Purpose: strings.TrimSpace(payload.Purpose), Status: payload.Status,
		AgentRefs: payload.AgentRefs, TeamRefs: payload.TeamRefs, ObjectiveRefs: payload.ObjectiveRefs, RunRefs: payload.RunRefs,
		Milestones: payload.Milestones, Hypotheses: payload.Hypotheses, SourceMonitors: payload.SourceMonitors,
		Deliverables: payload.Deliverables, Budget: payload.Budget, Policy: payload.Policy, Checkpoint: payload.Checkpoint,
	}
	created, event, err := service.Create(r.Context(), runtime.CreateProjectRequest{
		Project: project, IdempotencyKey: key, Actor: standaloneProjectActor, Visibility: runtime.ActivityVisibilityScope,
	})
	if err != nil {
		s.respondProjectError(w, err)
		return
	}
	status := http.StatusCreated
	if event == nil {
		status = http.StatusOK
	}
	s.respondJSON(w, status, created)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	service, ok := s.projectService(w)
	if !ok {
		return
	}
	filter, err := projectFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	projects, err := service.List(r.Context(), filter)
	if err != nil {
		s.respondProjectError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, projects)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	service, ok := s.projectService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, err := service.Get(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondProjectError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, project)
}

func (s *Server) handlePatchProject(w http.ResponseWriter, r *http.Request) {
	service, ok := s.projectService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.UpdateProjectRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, _, err := service.Patch(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), runtime.UpdateProjectRequest{
		ExpectedRevision: payload.ExpectedRevision, Title: payload.Title, Purpose: payload.Purpose, Status: payload.Status,
		AgentRefs: payload.AgentRefs, TeamRefs: payload.TeamRefs, ObjectiveRefs: payload.ObjectiveRefs, RunRefs: payload.RunRefs,
		Milestones: payload.Milestones, Hypotheses: payload.Hypotheses, SourceMonitors: payload.SourceMonitors,
		Deliverables: payload.Deliverables, Budget: payload.Budget, ClearBudget: payload.ClearBudget,
		Policy: payload.Policy, Checkpoint: payload.Checkpoint, Actor: standaloneProjectActor, Visibility: runtime.ActivityVisibilityScope,
	})
	if err != nil {
		s.respondProjectError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, updated)
}

func projectFilterFromQuery(r *http.Request) (runtime.ProjectFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.ProjectFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return runtime.ProjectFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return runtime.ProjectFilter{}, err
	}
	filter := runtime.ProjectFilter{
		Scope: scope, ObjectiveID: strings.TrimSpace(r.URL.Query().Get("objectiveId")), Limit: limit, Offset: offset,
	}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := runtime.ObjectiveOwner{Type: runtime.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return runtime.ProjectFilter{}, err
		}
		filter.Owner = &owner
	}
	for _, value := range queryValues(r, "status") {
		status := runtime.ProjectStatus(value)
		if !isPublicProjectStatus(status) {
			return runtime.ProjectFilter{}, errors.New("invalid project status")
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func isPublicProjectStatus(status runtime.ProjectStatus) bool {
	switch status {
	case runtime.ProjectStatusDraft, runtime.ProjectStatusActive, runtime.ProjectStatusPaused,
		runtime.ProjectStatusCompleted, runtime.ProjectStatusFailed, runtime.ProjectStatusCanceled, runtime.ProjectStatusArchived:
		return true
	default:
		return false
	}
}

func (s *Server) respondProjectError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrProjectNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrProjectConflict), errors.Is(err, runtime.ErrProjectIdempotency):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
