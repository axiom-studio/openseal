package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/source"
)

func (s *Server) handleRegisterSourcePolicyVersion(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourcePolicyService(w)
	if !ok {
		return
	}
	var request source.RegisterVersionRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	version, err := service.RegisterVersion(r.Context(), request)
	if err != nil {
		s.respondSourcePolicyError(w, err)
		return
	}
	s.respondJSON(w, http.StatusCreated, source.PolicyVersionResult{APIVersion: source.LifecycleAPIVersion, Version: version})
}

func (s *Server) handleListSourcePolicies(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourcePolicyService(w)
	if !ok {
		return
	}
	scope, ok := sourcePolicyScope(w, r, s)
	if !ok {
		return
	}
	items, err := service.ListLifecycles(r.Context(), scope)
	if err != nil {
		s.respondSourcePolicyError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, source.LifecycleList{APIVersion: source.LifecycleAPIVersion, Items: items})
}

func (s *Server) handleGetSourcePolicy(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourcePolicyService(w)
	if !ok {
		return
	}
	scope, ok := sourcePolicyScope(w, r, s)
	if !ok {
		return
	}
	lifecycle, err := service.GetLifecycle(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondSourcePolicyError(w, err)
		return
	}
	version, policyErr := service.GetVersion(r.Context(), scope, lifecycle.PolicyID, lifecycle.ActiveVersion)
	if policyErr != nil {
		s.respondSourcePolicyError(w, policyErr)
		return
	}
	s.respondJSON(w, http.StatusOK, source.LifecycleDetail{APIVersion: source.LifecycleAPIVersion, Lifecycle: lifecycle, Policy: version.Policy})
}

func (s *Server) handleListSourcePolicyVersions(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourcePolicyService(w)
	if !ok {
		return
	}
	scope, ok := sourcePolicyScope(w, r, s)
	if !ok {
		return
	}
	items, err := service.ListVersions(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondSourcePolicyError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, source.PolicyVersionList{APIVersion: source.LifecycleAPIVersion, Items: items})
}

func (s *Server) handleGetSourcePolicyVersion(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourcePolicyService(w)
	if !ok {
		return
	}
	scope, ok := sourcePolicyScope(w, r, s)
	if !ok {
		return
	}
	version, err := service.GetVersion(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(r.PathValue("version")))
	if err != nil {
		s.respondSourcePolicyError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, source.PolicyVersionResult{APIVersion: source.LifecycleAPIVersion, Version: version})
}

func (s *Server) handleActivateSourcePolicy(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourcePolicyService(w)
	if !ok {
		return
	}
	var request source.ActivateRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	pathID := strings.TrimSpace(r.PathValue("id"))
	if request.PolicyID != "" && strings.TrimSpace(request.PolicyID) != pathID {
		s.respondError(w, http.StatusBadRequest, "source policy path and payload ids must match")
		return
	}
	request.PolicyID = pathID
	result, err := service.Activate(r.Context(), request)
	if err != nil {
		s.respondSourcePolicyError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handleRevokeSourcePolicy(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourcePolicyService(w)
	if !ok {
		return
	}
	var request source.RevokeRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	pathID := strings.TrimSpace(r.PathValue("id"))
	if request.PolicyID != "" && strings.TrimSpace(request.PolicyID) != pathID {
		s.respondError(w, http.StatusBadRequest, "source policy path and payload ids must match")
		return
	}
	request.PolicyID = pathID
	result, err := service.Revoke(r.Context(), request)
	if err != nil {
		s.respondSourcePolicyError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handleListSourcePolicyEvents(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourcePolicyService(w)
	if !ok {
		return
	}
	scope, ok := sourcePolicyScope(w, r, s)
	if !ok {
		return
	}
	items, err := service.ListEvents(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondSourcePolicyError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, source.LifecycleEventList{APIVersion: source.LifecycleAPIVersion, Items: items})
}

func (s *Server) sourcePolicyService(w http.ResponseWriter) (*source.LifecycleService, bool) {
	if s.sourcePolicies == nil {
		s.respondError(w, http.StatusNotImplemented, "source policy lifecycle is unavailable")
		return nil, false
	}
	return s.sourcePolicies, true
}

func sourcePolicyScope(w http.ResponseWriter, r *http.Request, s *Server) (capability.ScopeReference, bool) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return capability.ScopeReference{}, false
	}
	return capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, true
}

func (s *Server) respondSourcePolicyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, source.ErrPolicyNotFound), errors.Is(err, source.ErrPolicyVersionNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, source.ErrPolicyRevision), errors.Is(err, source.ErrPolicyVersionExists):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
