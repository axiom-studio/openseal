package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	kernelbundle "github.com/axiom-studio/openseal/pkg/bundle"
	"github.com/axiom-studio/openseal/pkg/capability"
)

type compareWorkforceBundlesRequest struct {
	From *kernelbundle.Bundle `json:"from"`
	To   *kernelbundle.Bundle `json:"to"`
}

type previewWorkforceBundleInstallationRequest struct {
	Bundle    *kernelbundle.Bundle   `json:"bundle"`
	Placement kernelbundle.Placement `json:"placement"`
}

type planWorkforceBundleUpgradeRequest struct {
	Current *kernelbundle.Bundle `json:"current"`
	Target  *kernelbundle.Bundle `json:"target"`
}

type installWorkforceBundleRequest struct {
	Bundle    *kernelbundle.Bundle      `json:"bundle"`
	Scope     capability.ScopeReference `json:"scope"`
	Placement kernelbundle.Placement    `json:"placement"`
	Reason    string                    `json:"reason"`
}

func (s *Server) handleValidateWorkforceBundle(w http.ResponseWriter, r *http.Request) {
	var bundle kernelbundle.Bundle
	if !s.decodeWorkforceBundleRequest(w, r, &bundle) {
		return
	}
	if _, err := kernelbundle.Verify(&bundle, kernelbundle.TrustPolicy{}); err != nil {
		s.respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, map[string]interface{}{"valid": true, "digest": bundle.Digest})
}

func (s *Server) handleInspectWorkforceBundle(w http.ResponseWriter, r *http.Request) {
	var bundle kernelbundle.Bundle
	if !s.decodeWorkforceBundleRequest(w, r, &bundle) {
		return
	}
	result, err := kernelbundle.Inspect(&bundle)
	if err != nil {
		s.respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handleCompareWorkforceBundles(w http.ResponseWriter, r *http.Request) {
	var request compareWorkforceBundlesRequest
	if !s.decodeWorkforceBundleRequest(w, r, &request) {
		return
	}
	result, err := kernelbundle.Compare(request.From, request.To)
	if err != nil {
		s.respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handlePreviewWorkforceBundleInstallation(w http.ResponseWriter, r *http.Request) {
	var request previewWorkforceBundleInstallationRequest
	if !s.decodeWorkforceBundleRequest(w, r, &request) {
		return
	}
	result, err := kernelbundle.PreviewInstallation(request.Bundle, request.Placement)
	if err != nil {
		s.respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handlePlanWorkforceBundleUpgrade(w http.ResponseWriter, r *http.Request) {
	var request planWorkforceBundleUpgradeRequest
	if !s.decodeWorkforceBundleRequest(w, r, &request) {
		return
	}
	result, err := kernelbundle.PlanUpgrade(request.Current, request.Target)
	if err != nil {
		s.respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handleInstallWorkforceBundle(w http.ResponseWriter, r *http.Request) {
	actorID := strings.TrimSpace(s.workforceBundleActor)
	if s.workforceBundles == nil || actorID == "" {
		s.respondError(w, http.StatusNotImplemented, "workforce bundle installation is not configured")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		s.respondError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	var request installWorkforceBundleRequest
	if !s.decodeWorkforceBundleRequest(w, r, &request) {
		return
	}
	result, err := kernelbundle.Install(r.Context(), s.workforceBundles, kernelbundle.InstallationRequest{
		Bundle: request.Bundle, TrustPolicy: s.workforceBundleTrust, Scope: request.Scope, Placement: request.Placement,
		ActorType: "user", ActorID: actorID, Reason: request.Reason, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.respondJSON(w, http.StatusCreated, result)
}

func (s *Server) decodeWorkforceBundleRequest(w http.ResponseWriter, r *http.Request, value interface{}) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid workforce bundle request: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		s.respondError(w, http.StatusBadRequest, "workforce bundle request must contain one JSON document")
		return false
	}
	return true
}
