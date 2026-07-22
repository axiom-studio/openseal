package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (s *Server) eventSourceSubscriptions() *runtime.EventSourceSubscriptionService {
	return runtime.NewEventSourceSubscriptionService(s.store, s.store)
}

func (s *Server) handleCreateEventSourceSubscription(w http.ResponseWriter, r *http.Request) {
	var payload kernelapi.CreateEventSourceSubscriptionRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := s.eventSourceSubscriptions().Create(r.Context(), runtime.CreateEventSourceSubscriptionRequest{
		ID: payload.ID, Scope: payload.Scope, Owner: payload.Owner, DisplayName: payload.DisplayName, Description: payload.Description,
		Source: payload.Source, Connector: payload.Connector, Status: payload.Status, EventTypes: payload.EventTypes,
		Parameters: payload.Parameters, PollIntervalSeconds: payload.PollIntervalSeconds,
	})
	if err != nil {
		s.respondEventSourceSubscriptionError(w, err)
		return
	}
	s.respondJSON(w, http.StatusCreated, value)
}

func (s *Server) handleListEventSourceSubscriptions(w http.ResponseWriter, r *http.Request) {
	filter, err := eventSourceSubscriptionFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := s.eventSourceSubscriptions().List(r.Context(), filter)
	if err != nil {
		s.respondEventSourceSubscriptionError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, items)
}

func (s *Server) handleGetEventSourceSubscription(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	detail, err := s.eventSourceSubscriptions().Get(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondEventSourceSubscriptionError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, detail)
}

func (s *Server) handleUpdateEventSourceSubscription(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.UpdateEventSourceSubscriptionRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := s.eventSourceSubscriptions().Update(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), runtime.UpdateEventSourceSubscriptionRequest{
		ExpectedRevision: payload.ExpectedRevision, DisplayName: payload.DisplayName, Description: payload.Description,
		Connector: payload.Connector, Status: payload.Status, EventTypes: payload.EventTypes, Parameters: payload.Parameters,
		ReplaceParameters: payload.ReplaceParameters, PollIntervalSeconds: payload.PollIntervalSeconds,
	})
	if err != nil {
		s.respondEventSourceSubscriptionError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, value)
}

func (s *Server) handleRetireEventSourceSubscription(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.RetireEventSourceSubscriptionRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	retired := runtime.EventSourceSubscriptionRetired
	value, err := s.eventSourceSubscriptions().Update(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), runtime.UpdateEventSourceSubscriptionRequest{ExpectedRevision: payload.ExpectedRevision, Status: &retired})
	if err != nil {
		s.respondEventSourceSubscriptionError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, value)
}

func (s *Server) handleReportEventSourceHealth(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.ReportEventSourceHealthRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	health, err := s.eventSourceSubscriptions().ReportHealth(r.Context(), runtime.ReportEventSourceHealthRequest{
		Scope: scope, SubscriptionID: strings.TrimSpace(r.PathValue("id")), ExpectedHealthRevision: payload.ExpectedHealthRevision,
		ObservedSubscriptionRevision: payload.ObservedSubscriptionRevision, State: payload.State, LastEventAt: payload.LastEventAt,
		ErrorCode: payload.ErrorCode, Summary: payload.Summary,
	})
	if err != nil {
		s.respondEventSourceSubscriptionError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, health)
}

func (s *Server) handleGetEventSourceCheckpoint(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	detail, err := s.eventSourceSubscriptions().Get(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondEventSourceSubscriptionError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, detail.Checkpoint)
}

func (s *Server) handleAdvanceEventSourceCheckpoint(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	detail, err := s.eventSourceSubscriptions().Get(r.Context(), scope, id)
	if err != nil {
		s.respondEventSourceSubscriptionError(w, err)
		return
	}
	var payload kernelapi.AdvanceEventSourceCheckpointRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if detail.Subscription.Status != runtime.EventSourceSubscriptionActive || payload.ObservedSubscriptionRevision <= 0 || detail.Subscription.Revision != payload.ObservedSubscriptionRevision {
		s.respondEventSourceSubscriptionError(w, runtime.ErrEventSourceSubscriptionConflict)
		return
	}
	checkpoint, err := runtime.NewEventSourceCheckpointService(s.store).Advance(r.Context(), runtime.AdvanceEventSourceCheckpointRequest{
		Scope: scope, Source: detail.Subscription.Source, SubscriptionID: id, ExpectedRevision: payload.ExpectedRevision,
		Cursor: payload.Cursor, EventIDs: payload.EventIDs, Watermark: payload.Watermark,
	})
	if err != nil {
		s.respondEventSourceSubscriptionError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, checkpoint)
}

func eventSourceSubscriptionFilterFromQuery(r *http.Request) (runtime.EventSourceSubscriptionFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.EventSourceSubscriptionFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 500)
	if err != nil {
		return runtime.EventSourceSubscriptionFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return runtime.EventSourceSubscriptionFilter{}, err
	}
	filter := runtime.EventSourceSubscriptionFilter{Scope: scope, Limit: limit, Offset: offset, ConnectorKind: runtime.EventSourceConnectorKind(strings.TrimSpace(r.URL.Query().Get("connectorKind")))}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := runtime.ObjectiveOwner{Type: runtime.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return filter, err
		}
		filter.Owner = &owner
	}
	for _, value := range queryValues(r, "status") {
		filter.Statuses = append(filter.Statuses, runtime.EventSourceSubscriptionStatus(value))
	}
	return filter, nil
}

func (s *Server) respondEventSourceSubscriptionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrEventSourceSubscriptionNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrEventSourceSubscriptionConflict), errors.Is(err, runtime.ErrEventSourceCheckpointConflict):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrInvalidEventSourceSubscription), errors.Is(err, runtime.ErrInvalidEventSourceHealth), errors.Is(err, runtime.ErrInvalidEventSourceCheckpoint):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.respondError(w, http.StatusInternalServerError, "event source subscription operation failed")
	}
}
