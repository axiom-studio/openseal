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

func (s *Server) handleActivateAgentDefinition(w http.ResponseWriter, r *http.Request) {
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelapi.ActivateAgentDefinitionRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	deployment, activation, err := registry.ActivateDefinition(r.Context(), payload.Scope, strings.TrimSpace(r.PathValue("id")),
		strings.TrimSpace(payload.Version), payload.ExpectedRevision, payload.ActorType, payload.ActorID, payload.Reason)
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.AgentDefinitionActivationResult{Deployment: deployment, Activation: activation})
}

func (s *Server) handleRollbackAgentDefinition(w http.ResponseWriter, r *http.Request) {
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelapi.RollbackAgentDefinitionRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	deployment, activation, err := registry.RollbackDefinition(r.Context(), payload.Scope, strings.TrimSpace(r.PathValue("id")),
		payload.ExpectedRevision, payload.ActorType, payload.ActorID, payload.Reason)
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.AgentDefinitionActivationResult{Deployment: deployment, Activation: activation})
}

func (s *Server) handleListAgentDefinitionActivations(w http.ResponseWriter, r *http.Request) {
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
	deploymentID := strings.TrimSpace(r.PathValue("id"))
	registryScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	if _, err := registry.GetDeployment(r.Context(), registryScope, deploymentID); err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	activations, err := registry.ListActivations(r.Context(), registryScope, deploymentID)
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, activations)
}

func (s *Server) handleProposeAgentDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelagent.ProposeAmendmentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	deploymentID := strings.TrimSpace(r.PathValue("id"))
	if payload.DeploymentID != "" && strings.TrimSpace(payload.DeploymentID) != deploymentID {
		s.respondError(w, http.StatusBadRequest, "agent amendment path and payload deployment ids must match")
		return
	}
	payload.DeploymentID = deploymentID
	amendment, err := registry.ProposeAmendment(r.Context(), payload)
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusCreated, amendment)
}

func (s *Server) handleListAgentDefinitionAmendments(w http.ResponseWriter, r *http.Request) {
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
	amendments, err := registry.ListAmendments(r.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.AgentDefinitionAmendmentList{Items: amendments})
}

func (s *Server) loadAgentDefinitionAmendment(w http.ResponseWriter, r *http.Request, registry *kernelagent.Registry, scope capability.ScopeReference) (*kernelagent.DefinitionAmendment, bool) {
	amendment, err := registry.GetAmendment(r.Context(), scope, strings.TrimSpace(r.PathValue("amendmentId")))
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return nil, false
	}
	if amendment.DeploymentID != strings.TrimSpace(r.PathValue("id")) {
		s.respondError(w, http.StatusNotFound, kernelagent.ErrAmendmentNotFound.Error())
		return nil, false
	}
	return amendment, true
}

func (s *Server) handleGetAgentDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
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
	if amendment, ok := s.loadAgentDefinitionAmendment(w, r, registry, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}); ok {
		s.respondJSON(w, http.StatusOK, amendment)
	}
}

func (s *Server) handleEvaluateAgentDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelagent.SubmitAmendmentEvaluationRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := s.loadAgentDefinitionAmendment(w, r, registry, payload.Scope); !ok {
		return
	}
	payload.AmendmentID = strings.TrimSpace(r.PathValue("amendmentId"))
	amendment, err := registry.SubmitAmendmentEvaluation(r.Context(), payload)
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, amendment)
}

func (s *Server) handleResolveAgentDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelagent.ResolveAmendmentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := s.loadAgentDefinitionAmendment(w, r, registry, payload.Scope); !ok {
		return
	}
	payload.AmendmentID = strings.TrimSpace(r.PathValue("amendmentId"))
	amendment, err := registry.ResolveAmendment(r.Context(), payload)
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, amendment)
}

func (s *Server) handleActivateAgentDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelapi.ActivateAgentDefinitionAmendmentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := s.loadAgentDefinitionAmendment(w, r, registry, payload.Scope); !ok {
		return
	}
	amendment, deployment, activation, err := registry.ActivateAmendment(r.Context(), payload.Scope, strings.TrimSpace(r.PathValue("amendmentId")),
		payload.ExpectedRevision, payload.ActorType, payload.ActorID, payload.Reason)
	if err != nil {
		s.respondAgentDeploymentError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.AgentDefinitionAmendmentActivationResult{Amendment: amendment, Deployment: deployment, Activation: activation})
}

func (s *Server) respondAgentDeploymentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, kernelagent.ErrDeploymentNotFound), errors.Is(err, kernelagent.ErrDefinitionNotFound), errors.Is(err, kernelagent.ErrAmendmentNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, kernelagent.ErrRevisionConflict):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
