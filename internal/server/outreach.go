package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *Server) outreachService(w http.ResponseWriter) (*runtime.OutreachService, runtime.ProjectStore, runtime.SourceMonitorStore, bool) {
	outreach, outreachOK := s.store.(runtime.OutreachStore)
	projects, projectsOK := s.store.(runtime.ProjectStore)
	sources, sourcesOK := s.store.(runtime.SourceMonitorStore)
	actions, actionsOK := s.store.(runtime.OutreachActionReader)
	if !outreachOK || !projectsOK || !sourcesOK || !actionsOK {
		s.respondError(w, http.StatusServiceUnavailable, "outreach capability is unavailable")
		return nil, nil, nil, false
	}
	return runtime.NewOutreachService(outreach, projects, sources, actions), projects, sources, true
}

func (s *Server) handleCreateOutreachThread(w http.ResponseWriter, r *http.Request) {
	service, projects, sources, ok := s.outreachService(w)
	if !ok {
		return
	}
	skillStore, ok := s.store.(skill.CatalogStore)
	if !ok {
		s.respondError(w, http.StatusServiceUnavailable, "outreach drafting requires the canonical Skill catalog")
		return
	}
	var payload kernelapi.CreateOutreachThreadRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	projectID := strings.TrimSpace(r.PathValue("id"))
	if strings.TrimSpace(payload.ProjectID) != projectID {
		s.respondError(w, http.StatusBadRequest, "outreach Project does not match the route")
		return
	}
	project, err := projects.GetProject(r.Context(), payload.Scope, projectID)
	if err != nil || project == nil {
		if err == nil {
			err = runtime.ErrProjectNotFound
		}
		s.respondOutreachError(w, err)
		return
	}
	observation, err := sources.GetSourceObservation(r.Context(), payload.Scope, strings.TrimSpace(payload.SourceObservationID))
	if err != nil || observation == nil {
		if err == nil {
			err = runtime.ErrSourceObservationNotFound
		}
		s.respondOutreachError(w, err)
		return
	}
	if observation.ProjectID != project.ID {
		s.respondError(w, http.StatusBadRequest, "source observation does not belong to the Project")
		return
	}
	var monitor *runtime.SourceMonitorReference
	for index := range project.SourceMonitors {
		if project.SourceMonitors[index].ID == observation.MonitorID {
			monitor = &project.SourceMonitors[index]
			break
		}
	}
	if monitor == nil {
		s.respondError(w, http.StatusConflict, "source observation monitor is no longer part of the Project")
		return
	}
	selected := payload.Message.Capability
	selection := []skill.BindingReference(nil)
	if strings.TrimSpace(selected.BindingID) != "" || selected.BindingRevision != 0 {
		selection = append(selection, skill.BindingReference{ID: strings.TrimSpace(selected.BindingID), Revision: selected.BindingRevision})
	}
	catalog := skill.NewCatalogWithStore(skillStore)
	bound, err := catalog.Resolve(r.Context(), skill.ScopeReference{Kind: payload.Scope.Kind, ID: payload.Scope.ID}, monitor.AssignedAgentID,
		strings.TrimSpace(selected.SkillID), strings.TrimSpace(selected.SkillVersion), strings.TrimSpace(selected.Action), selection...)
	if err != nil {
		s.respondError(w, http.StatusConflict, "selected outreach Skill action is unavailable or stale")
		return
	}
	if bound.Action.SideEffect != skill.SideEffectExternal {
		s.respondError(w, http.StatusBadRequest, "outreach requires an external side-effect Skill action")
		return
	}
	targetArgument := strings.TrimSpace(bound.Action.SemanticArguments["target"])
	bodyArgument := strings.TrimSpace(bound.Action.SemanticArguments["body"])
	if targetArgument == "" || bodyArgument == "" || targetArgument == bodyArgument {
		s.respondError(w, http.StatusBadRequest, "selected outreach Skill action must declare distinct target and body semantic arguments")
		return
	}
	arguments := cloneMap(payload.Message.Capability.Arguments)
	if arguments == nil {
		arguments = map[string]interface{}{}
	}
	arguments[targetArgument] = observation.SourceURI
	arguments[bodyArgument] = strings.TrimSpace(payload.Message.Body)
	if err := catalog.ValidateInput(r.Context(), bound, arguments); err != nil {
		s.respondError(w, http.StatusBadRequest, "outreach Skill action inputs do not satisfy the selected binding")
		return
	}
	thread := &runtime.OutreachThread{
		ID: strings.TrimSpace(payload.ID), Scope: payload.Scope, ProjectID: project.ID,
		SourceObservationID: observation.ID, MonitorID: observation.MonitorID, StableSourceID: observation.StableSourceID,
		TargetURI: observation.SourceURI, Owner: project.Owner, AssignedAgentID: monitor.AssignedAgentID,
		SourcePolicyRef: monitor.SourcePolicyRef, ApprovalPolicyRef: strings.TrimSpace(payload.ApprovalPolicyRef), Identity: payload.Identity,
		Messages: []runtime.OutreachMessage{{
			ID: strings.TrimSpace(payload.Message.ID), Direction: runtime.OutreachMessageOutbound, Intent: payload.Message.Intent,
			Body: strings.TrimSpace(payload.Message.Body), Status: runtime.OutreachMessageDraft,
			Capability: &runtime.OutreachCapability{
				BindingID: bound.Binding.ID, BindingRevision: bound.Binding.Revision,
				SkillID: strings.TrimSpace(selected.SkillID), SkillVersion: strings.TrimSpace(selected.SkillVersion), Action: strings.TrimSpace(selected.Action),
				Arguments: arguments, TargetArgument: targetArgument, BodyArgument: bodyArgument,
			},
		}},
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(payload.IdempotencyKey)
	}
	created, event, err := service.Create(r.Context(), runtime.CreateOutreachThreadRequest{
		Thread: thread, IdempotencyKey: key, Actor: standaloneProjectActor, Visibility: outreachVisibility(project.Owner),
	})
	if err != nil {
		s.respondOutreachError(w, err)
		return
	}
	status := http.StatusCreated
	if event == nil {
		status = http.StatusOK
	}
	s.respondJSON(w, status, created)
}

func (s *Server) handleListOutreachThreads(w http.ResponseWriter, r *http.Request) {
	service, projects, _, ok := s.outreachService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	projectID := strings.TrimSpace(r.PathValue("id"))
	if project, getErr := projects.GetProject(r.Context(), scope, projectID); getErr != nil || project == nil {
		if getErr == nil {
			getErr = runtime.ErrProjectNotFound
		}
		s.respondOutreachError(w, getErr)
		return
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter := runtime.OutreachThreadFilter{Scope: scope, ProjectID: projectID, SourceObservationID: strings.TrimSpace(r.URL.Query().Get("sourceObservationId")), Limit: limit, Offset: offset}
	for _, raw := range queryValues(r, "status") {
		status := runtime.OutreachThreadStatus(raw)
		if status != runtime.OutreachThreadOpen && status != runtime.OutreachThreadClosed && status != runtime.OutreachThreadCanceled {
			s.respondError(w, http.StatusBadRequest, "invalid outreach status")
			return
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	threads, err := service.List(r.Context(), filter)
	if err != nil {
		s.respondOutreachError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, threads)
}

func (s *Server) handleGetOutreachThread(w http.ResponseWriter, r *http.Request) {
	service, _, _, ok := s.outreachService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	thread, err := service.Get(r.Context(), scope, strings.TrimSpace(r.PathValue("threadId")))
	if err != nil {
		s.respondOutreachError(w, err)
		return
	}
	if thread.ProjectID != strings.TrimSpace(r.PathValue("id")) {
		s.respondOutreachError(w, runtime.ErrOutreachThreadNotFound)
		return
	}
	s.respondJSON(w, http.StatusOK, thread)
}

func (s *Server) handleDeliverOutreachMessage(w http.ResponseWriter, r *http.Request) {
	if s.outreachDelivery == nil {
		s.respondError(w, http.StatusServiceUnavailable, "outreach delivery runtime is unavailable")
		return
	}
	service, projects, _, ok := s.outreachService(w)
	if !ok {
		return
	}
	var payload kernelapi.DeliverOutreachMessageRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	projectID := strings.TrimSpace(r.PathValue("id"))
	project, err := projects.GetProject(r.Context(), payload.Scope, projectID)
	if err != nil || project == nil {
		if err == nil {
			err = runtime.ErrProjectNotFound
		}
		s.respondOutreachError(w, err)
		return
	}
	thread, err := service.Get(r.Context(), payload.Scope, strings.TrimSpace(r.PathValue("threadId")))
	if err != nil || thread.ProjectID != project.ID {
		if err == nil {
			err = runtime.ErrOutreachThreadNotFound
		}
		s.respondOutreachError(w, err)
		return
	}
	messageID := strings.TrimSpace(r.PathValue("messageId"))
	var message *runtime.OutreachMessage
	for index := range thread.Messages {
		if thread.Messages[index].ID == messageID {
			message = &thread.Messages[index]
			break
		}
	}
	if message == nil || message.Direction != runtime.OutreachMessageOutbound || message.Status != runtime.OutreachMessageDraft {
		s.respondError(w, http.StatusConflict, "only an outbound outreach draft can be delivered")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(payload.IdempotencyKey)
	}
	if key == "" {
		s.respondError(w, http.StatusBadRequest, "Idempotency-Key is required for outbound delivery")
		return
	}
	result, err := s.outreachDelivery(r.Context(), runtime.CreateAgentRunRequest{
		Scope: payload.Scope, Kind: runtime.RunKindAgentWork, Owner: thread.Owner, AssignedAgentID: thread.AssignedAgentID,
		ConcurrencyKey: "outreach:" + thread.ID + ":" + message.ID,
		Goal:           "Deliver reviewed outreach message for Project " + project.Title, Source: runtime.RunSourceRequest, Priority: payload.Priority,
		Context: map[string]interface{}{runtime.OutreachInvocationContextKey: map[string]interface{}{"threadId": thread.ID, "messageId": message.ID}},
		Budget:  payload.Budget, Policy: map[string]interface{}{"outreachApprovalPolicyRef": thread.ApprovalPolicyRef}, IdempotencyKey: key,
		Actor: standaloneProjectActor, Visibility: outreachVisibility(thread.Owner),
	})
	if err != nil {
		s.respondAgentRunError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Event == nil {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result.Run)
}

func outreachVisibility(owner runtime.ObjectiveOwner) runtime.ActivityVisibility {
	if owner.Type == runtime.OwnerTypeTeam {
		return runtime.ActivityVisibilityTeam
	}
	return runtime.ActivityVisibilityScope
}

func (s *Server) respondOutreachError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrOutreachThreadNotFound), errors.Is(err, runtime.ErrSourceObservationNotFound), errors.Is(err, runtime.ErrProjectNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrOutreachThreadConflict), errors.Is(err, runtime.ErrOutreachThreadIdempotency):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrInvalidOutreachThread), errors.Is(err, runtime.ErrInvalidSourceObservation), errors.Is(err, runtime.ErrInvalidScope), errors.Is(err, runtime.ErrInvalidOwner):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("outreach API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "outreach operation failed")
	}
}

func cloneMap(input map[string]interface{}) map[string]interface{} {
	if input == nil {
		return nil
	}
	result := make(map[string]interface{}, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
