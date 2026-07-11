package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
)

type WorkforceLifecycleAuthorization struct {
	Actor                        authoring.ChangeSetActor
	EligibleApprovalRequirements []kernelapi.ApprovalRequirementReference
}

// WorkforceLifecycleAuthorizer is the host-owned identity and policy boundary
// for standalone OpenSeal governance. Returning an error denies the operation.
// The API never trusts actor or role authority supplied by a client body.
type WorkforceLifecycleAuthorizer interface {
	AuthorizeWorkforceLifecycle(context.Context, string, *authoring.ChangeSet) (WorkforceLifecycleAuthorization, error)
}

func (s *Server) handleCompileWorkforce(w http.ResponseWriter, r *http.Request) {
	if s.authoring == nil {
		s.respondError(w, http.StatusNotImplemented, "workforce authoring is not configured")
		return
	}
	var request authoring.GenerateRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.authoring.Compile(r.Context(), request)
	if err != nil {
		s.respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handleCreateWorkforceChangeSet(w http.ResponseWriter, r *http.Request) {
	if s.authoringChanges == nil {
		s.respondError(w, http.StatusNotImplemented, "workforce change sets are not configured")
		return
	}
	var request authoring.CreateChangeSetRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
		request.IdempotencyKey = key
	}
	result, replayed, err := s.authoringChanges.Create(r.Context(), request)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, authoring.ErrChangeSetIdempotency) {
			status = http.StatusConflict
		}
		s.respondError(w, status, err.Error())
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result)
}

func (s *Server) handleGetWorkforceChangeSet(w http.ResponseWriter, r *http.Request) {
	if s.authoringChanges == nil {
		s.respondError(w, http.StatusNotImplemented, "workforce change sets are not configured")
		return
	}
	scope := capability.ScopeReference{Kind: strings.TrimSpace(r.URL.Query().Get("scopeKind")), ID: strings.TrimSpace(r.URL.Query().Get("scopeId"))}
	result, err := s.authoringChanges.Get(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, authoring.ErrChangeSetNotFound) {
			s.respondError(w, http.StatusNotFound, err.Error())
			return
		}
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) composeWorkforceLifecycleCapability(r *http.Request, result *kernelapi.Capability) {
	id := strings.TrimSpace(r.URL.Query().Get("changeSetId"))
	if id == "" {
		return
	}
	scope := workforceScopeFromQuery(r)
	changeSet, err := s.authoringChanges.Get(r.Context(), scope, id)
	if err != nil || changeSet == nil {
		return
	}
	result.Context = &kernelapi.CapabilityContext{ChangeSetID: changeSet.ID, Revision: changeSet.Revision}
	if s.workforceAuthority == nil {
		return
	}
	if strings.TrimSpace(changeSet.CandidateDigest) != "" && (changeSet.Status == authoring.ChangeSetReview || changeSet.Status == authoring.ChangeSetEvaluating) {
		if _, err := s.workforceAuthority.AuthorizeWorkforceLifecycle(r.Context(), kernelapi.OperationEvaluate, changeSet); err == nil {
			result.Operations = append(result.Operations, kernelapi.OperationEvaluate)
		}
	}
	if authorization, err := s.workforceAuthority.AuthorizeWorkforceLifecycle(r.Context(), kernelapi.OperationApprove, changeSet); err == nil && changeSet.Status == authoring.ChangeSetAwaitingApproval {
		eligible := activeAuthorizedApprovalRequirements(changeSet, authorization)
		if len(eligible) > 0 {
			result.Operations = append(result.Operations, kernelapi.OperationApprove)
			result.Context.EligibleApprovalRequirements = eligible
		}
	}
	if _, err := s.workforceAuthority.AuthorizeWorkforceLifecycle(r.Context(), kernelapi.OperationApply, changeSet); err == nil && changeSet.Status == authoring.ChangeSetReady {
		if _, ok := s.store.(authoring.AtomicChangeSetStore); ok {
			result.Operations = append(result.Operations, kernelapi.OperationApply)
		}
	}
}

func (s *Server) handleEvaluateWorkforceChangeSet(w http.ResponseWriter, r *http.Request) {
	var request authoring.SubmitChangeSetEvaluationRequest
	if !s.decodeGovernedWorkforceRequest(w, r, &request) {
		return
	}
	changeSet, authorization, ok := s.authorizeWorkforceLifecycle(w, r, kernelapi.OperationEvaluate, request.Scope, request.ChangeSetID)
	if !ok {
		return
	}
	request.Actor = authorization.Actor
	result, replayed, err := s.authoringChanges.SubmitEvaluation(r.Context(), request)
	s.respondWorkforceMutation(w, result, replayed, err)
	_ = changeSet
}

func (s *Server) handleApproveWorkforceChangeSet(w http.ResponseWriter, r *http.Request) {
	var request authoring.ResolveChangeSetApprovalRequest
	if !s.decodeGovernedWorkforceRequest(w, r, &request) {
		return
	}
	changeSet, authorization, ok := s.authorizeWorkforceLifecycle(w, r, kernelapi.OperationApprove, request.Scope, request.ChangeSetID)
	if !ok {
		return
	}
	eligible := false
	for _, reference := range activeAuthorizedApprovalRequirements(changeSet, authorization) {
		if reference.EvaluationID == request.EvaluationID && reference.PolicyID == request.PolicyID && reference.Role == request.Role {
			eligible = true
			break
		}
	}
	if !eligible {
		s.respondError(w, http.StatusForbidden, "approval requirement is not authorized")
		return
	}
	request.Actor = authorization.Actor
	result, replayed, err := s.authoringChanges.ResolveApproval(r.Context(), request)
	s.respondWorkforceMutation(w, result, replayed, err)
}

func (s *Server) handleApplyWorkforceChangeSet(w http.ResponseWriter, r *http.Request) {
	var request authoring.ApplyChangeSetRequest
	if !s.decodeGovernedWorkforceRequest(w, r, &request) {
		return
	}
	_, authorization, ok := s.authorizeWorkforceLifecycle(w, r, kernelapi.OperationApply, request.Scope, request.ChangeSetID)
	if !ok {
		return
	}
	request.Actor = authorization.Actor
	result, replayed, err := s.authoringChanges.Apply(r.Context(), request)
	s.respondWorkforceMutation(w, result, replayed, err)
}

func (s *Server) decodeGovernedWorkforceRequest(w http.ResponseWriter, r *http.Request, value interface{}) bool {
	if s.authoringChanges == nil {
		s.respondError(w, http.StatusNotImplemented, "workforce change sets are not configured")
		return false
	}
	if err := decodeStrictJSON(r, value); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return false
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		s.respondError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return false
	}
	switch request := value.(type) {
	case *authoring.SubmitChangeSetEvaluationRequest:
		request.IdempotencyKey = key
	case *authoring.ResolveChangeSetApprovalRequest:
		request.IdempotencyKey = key
	case *authoring.ApplyChangeSetRequest:
		request.IdempotencyKey = key
	}
	return true
}

func activeAuthorizedApprovalRequirements(changeSet *authoring.ChangeSet, authorization WorkforceLifecycleAuthorization) []kernelapi.ApprovalRequirementReference {
	if changeSet == nil || changeSet.Status != authoring.ChangeSetAwaitingApproval {
		return nil
	}
	for i := len(changeSet.Evaluations) - 1; i >= 0; i-- {
		evaluation := changeSet.Evaluations[i]
		if !evaluation.Allowed || len(evaluation.ApprovalRequirements) == 0 {
			continue
		}
		result := make([]kernelapi.ApprovalRequirementReference, 0, len(authorization.EligibleApprovalRequirements))
		seen := make(map[string]bool, len(authorization.EligibleApprovalRequirements))
		for _, authorized := range authorization.EligibleApprovalRequirements {
			if authorized.EvaluationID != evaluation.ID {
				continue
			}
			active := false
			for _, requirement := range evaluation.ApprovalRequirements {
				if requirement.PolicyID == authorized.PolicyID && requirement.Role == authorized.Role {
					active = true
					break
				}
			}
			if !active {
				continue
			}
			alreadyDecided := false
			for _, decision := range changeSet.ApprovalDecisions {
				if decision.EvaluationID == authorized.EvaluationID && decision.PolicyID == authorized.PolicyID && decision.Role == authorized.Role && decision.Actor == authorization.Actor {
					alreadyDecided = true
					break
				}
			}
			key := authorized.EvaluationID + "\x00" + authorized.PolicyID + "\x00" + authorized.Role
			if !alreadyDecided && !seen[key] {
				result = append(result, authorized)
				seen[key] = true
			}
		}
		return result
	}
	return nil
}

func (s *Server) authorizeWorkforceLifecycle(w http.ResponseWriter, r *http.Request, operation string, scope capability.ScopeReference, id string) (*authoring.ChangeSet, WorkforceLifecycleAuthorization, bool) {
	if strings.TrimSpace(id) == "" || id != strings.TrimSpace(r.PathValue("id")) {
		s.respondError(w, http.StatusBadRequest, "changeSetId must match the route")
		return nil, WorkforceLifecycleAuthorization{}, false
	}
	changeSet, err := s.authoringChanges.Get(r.Context(), scope, id)
	if err != nil {
		s.respondWorkforceMutation(w, nil, false, err)
		return nil, WorkforceLifecycleAuthorization{}, false
	}
	if s.workforceAuthority == nil {
		s.respondError(w, http.StatusForbidden, "workforce lifecycle authority is not configured")
		return nil, WorkforceLifecycleAuthorization{}, false
	}
	authorization, err := s.workforceAuthority.AuthorizeWorkforceLifecycle(r.Context(), operation, changeSet)
	if err != nil || strings.TrimSpace(authorization.Actor.Type) == "" || strings.TrimSpace(authorization.Actor.ID) == "" {
		s.respondError(w, http.StatusForbidden, "workforce lifecycle operation is not authorized")
		return nil, WorkforceLifecycleAuthorization{}, false
	}
	return changeSet, authorization, true
}

func (s *Server) respondWorkforceMutation(w http.ResponseWriter, result *authoring.ChangeSet, replayed bool, err error) {
	if err != nil {
		status := http.StatusUnprocessableEntity
		switch {
		case errors.Is(err, authoring.ErrChangeSetNotFound):
			status = http.StatusNotFound
		case errors.Is(err, authoring.ErrChangeSetIdempotency), errors.Is(err, authoring.ErrChangeSetRevision), errors.Is(err, authoring.ErrChangeSetTransition):
			status = http.StatusConflict
		}
		s.respondError(w, status, err.Error())
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result)
}

func workforceScopeFromQuery(r *http.Request) capability.ScopeReference {
	return capability.ScopeReference{Kind: strings.TrimSpace(r.URL.Query().Get("scopeKind")), ID: strings.TrimSpace(r.URL.Query().Get("scopeId"))}
}
