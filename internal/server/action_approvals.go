package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
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
	decisionID := strings.TrimSpace(payload.DecisionID)
	if decisionID == "" {
		decisionID = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	result, err := runtime.NewApprovalCoordinator(s.store, s.store, s.actionApprovalAuth).Resolve(r.Context(), runtime.ResolveApprovalRequest{
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

func actionApprovalFilterFromQuery(r *http.Request) (runtime.ApprovalFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.ApprovalFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return runtime.ApprovalFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return runtime.ApprovalFilter{}, err
	}
	filter := runtime.ApprovalFilter{Scope: scope, RunID: strings.TrimSpace(r.URL.Query().Get("runId")), Limit: limit, Offset: offset}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := runtime.ObjectiveOwner{Type: runtime.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return runtime.ApprovalFilter{}, err
		}
		filter.Owner = &owner
	}
	for _, value := range queryValues(r, "status") {
		status := runtime.ApprovalStatus(value)
		if !validActionApprovalStatus(status) {
			return runtime.ApprovalFilter{}, fmt.Errorf("invalid action approval status %q", value)
		}
		filter.Status = append(filter.Status, status)
	}
	return filter, nil
}

func validActionApprovalStatus(status runtime.ApprovalStatus) bool {
	switch status {
	case runtime.ApprovalStatusPending, runtime.ApprovalStatusApproved, runtime.ApprovalStatusRejected,
		runtime.ApprovalStatusExpired, runtime.ApprovalStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *Server) respondActionApprovalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrApprovalNotFound), errors.Is(err, runtime.ErrRunNotFound), errors.Is(err, runtime.ErrActionNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrRevisionConflict), errors.Is(err, runtime.ErrApprovalResolved), errors.Is(err, runtime.ErrInvalidRunTransition):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrInvalidScope), errors.Is(err, runtime.ErrInvalidOwner):
		s.respondError(w, http.StatusBadRequest, err.Error())
	case strings.Contains(err.Error(), "authorize approval"):
		s.respondError(w, http.StatusForbidden, err.Error())
	case strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "invalid"):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("action approval API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "action approval operation failed")
	}
}
