package server

import (
	"errors"
	"net/http"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
)

func (s *Server) agentRegistry() (*kernelagent.Registry, error) {
	store, ok := s.store.(kernelagent.Store)
	if !ok {
		return nil, errors.New("first-class Agent control plane is unavailable")
	}
	return kernelagent.NewRegistryWithStore(store), nil
}

func (s *Server) handleGetAgentDeployment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	deployment, err := registry.GetDeployment(r.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	definition, err := registry.GetDefinition(r.Context(), deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.AgentDeploymentCatalogEntry{Deployment: deployment, Definition: definition})
}

func (s *Server) handleListAgentDeployments(w http.ResponseWriter, r *http.Request) {
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	deployments, err := registry.ListDeployments(r.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID})
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	items := make([]kernelapi.AgentDeploymentCatalogEntry, 0, len(deployments))
	for _, deployment := range deployments {
		definition, definitionErr := registry.GetDefinition(r.Context(), deployment.DefinitionID, deployment.ActiveVersion)
		if definitionErr != nil {
			s.respondAgentDeploymentError(w, definitionErr)
			return
		}
		items = append(items, kernelapi.AgentDeploymentCatalogEntry{Deployment: deployment, Definition: definition})
	}
	s.respondJSON(w, http.StatusOK, kernelapi.AgentDeploymentList{Items: items})
}

func (s *Server) handleUpdateAgentDeployment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelapi.UpdateAgentDeploymentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if payload.Deployment == nil || strings.TrimSpace(payload.Deployment.ID) != strings.TrimSpace(r.PathValue("id")) {
		s.respondError(w, http.StatusBadRequest, "agent deployment path and payload ids must match")
		return
	}
	deployment, audit, err := registry.UpdateDeployment(r.Context(), payload.Deployment, payload.ExpectedRevision,
		payload.ActorType, payload.ActorID, payload.Reason)
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.AgentDeploymentUpdateResult{Deployment: deployment, Audit: audit})
}

func (s *Server) respondAgentDeploymentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, kernelagent.ErrDeploymentNotFound), errors.Is(err, kernelagent.ErrDefinitionNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, kernelagent.ErrRevisionConflict):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
