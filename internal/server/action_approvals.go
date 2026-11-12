package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
)

func (s *Server) handleListActionApprovals(w http.ResponseWriter, r *http.Request) {
	filter, err := actionApprovalFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	approvals, err := s.store.ListApprovals(r.Context(), filter)
	if err != nil {
		s.respondActionApprovalError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, approvals)
}

func (s *Server) handleGetActionApproval(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	approval, err := s.store.GetApproval(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondActionApprovalError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, approval)
}

func (s *Server) handleResolveActionApproval(w http.ResponseWriter, r *http.Request) {
	if s.actionApprovalAuth == nil {
		s.respondError(w, http.StatusNotImplemented, "action approval resolution is not configured")
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.ResolveActionApprovalRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if host, ok := s.actionApprovalAuth.(interface {
		ValidateApprovalRequest(models.Scope, models.ApprovalPrincipal) error
	}); ok {
		if err := host.ValidateApprovalRequest(scope, payload.Principal); err != nil {
			s.respondError(w, http.StatusForbidden, err.Error())
			return
		}
	}
	decisionID := strings.TrimSpace(payload.DecisionID)
	if decisionID == "" {
		decisionID = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	result, err := action.NewApprovalCoordinator(s.store, s.store, s.actionApprovalAuth).Resolve(r.Context(), action.ResolveApprovalRequest{
		Scope: scope, ApprovalID: strings.TrimSpace(r.PathValue("id")), ExpectedRevision: payload.ExpectedRevision,
		DecisionID: decisionID, Decision: payload.Decision, Principal: payload.Principal,
		Reason: strings.TrimSpace(payload.Reason), CorrelationID: strings.TrimSpace(r.Header.Get("X-Correlation-ID")),
	})
	if err != nil {
		s.respondActionApprovalError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func actionApprovalFilterFromQuery(r *http.Request) (models.ApprovalFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return models.ApprovalFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return models.ApprovalFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return models.ApprovalFilter{}, err
	}
	order := strings.TrimSpace(r.URL.Query().Get("order"))
	if order != "" && order != "created_asc" && order != "created_desc" {
		return models.ApprovalFilter{}, fmt.Errorf("invalid approval order %q", order)
	}
	filter := models.ApprovalFilter{Scope: scope, RunID: strings.TrimSpace(r.URL.Query().Get("runId")), Limit: limit, Offset: offset}
	filter.NewestFirst = order == "created_desc"

	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := models.ObjectiveOwner{Type: models.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return models.ApprovalFilter{}, err
		}
		filter.Owner = &owner
	}
	for _, value := range queryValues(r, "status") {
		status := models.ApprovalStatus(value)
		if !validActionApprovalStatus(status) {
			return models.ApprovalFilter{}, fmt.Errorf("invalid action approval status %q", value)
		}
		filter.Status = append(filter.Status, status)
	}
	return filter, nil
}

func validActionApprovalStatus(status models.ApprovalStatus) bool {
	switch status {
	case models.ApprovalStatusPending, models.ApprovalStatusApproved, models.ApprovalStatusRejected,
		models.ApprovalStatusExpired, models.ApprovalStatusCanceled, models.ApprovalStatusChangesRequested:
		return true
	default:
		return false
	}
}

func (s *Server) respondActionApprovalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrApprovalNotFound), errors.Is(err, models.ErrRunNotFound), errors.Is(err, models.ErrActionNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, models.ErrRevisionConflict), errors.Is(err, models.ErrApprovalResolved), errors.Is(err, models.ErrInvalidRunTransition):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, models.ErrInvalidScope), errors.Is(err, models.ErrInvalidOwner):
		s.respondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, action.ErrAgentBuilderApprovalForbidden), strings.Contains(err.Error(), "authorize approval"):
		s.respondError(w, http.StatusForbidden, err.Error())
	case strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "invalid"):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("action approval API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "action approval operation failed")
	}
}
