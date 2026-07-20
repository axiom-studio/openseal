package server

import (
	"errors"
	"net/http"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func (s *Server) teamRegistry() (*kernelteam.Registry, error) {
	agentStore, agentsOK := s.store.(kernelagent.Store)
	teamStore, teamsOK := s.store.(kernelteam.Store)
	if !agentsOK || !teamsOK {
		return nil, errors.New("first-class Team control plane is unavailable")
	}
	agents := kernelagent.NewRegistryWithStore(agentStore)
	return kernelteam.NewRegistryWithStore(teamStore, agents), nil
}

func (s *Server) handleRegisterTeamDefinition(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelteam.Definition
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	definition, err := registry.RegisterDefinition(r.Context(), &payload)
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusCreated, definition)
}

func (s *Server) handleGetTeamDefinition(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	version := strings.TrimSpace(r.URL.Query().Get("version"))
	if version == "" {
		definitions, err := registry.ListDefinitionVersions(r.Context(), id)
		if err != nil {
			s.respondTeamError(w, err)
			return
		}
		s.respondJSON(w, http.StatusOK, definitions)
		return
	}
	definition, err := registry.GetDefinition(r.Context(), id, version)
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, definition)
}

func (s *Server) handleCreateTeamDeployment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelapi.CreateTeamDeploymentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	deployment, activation, err := registry.CreateDeployment(r.Context(), payload.Deployment, payload.ActorType, payload.ActorID, payload.Reason)
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusCreated, kernelapi.TeamDeploymentResult{Deployment: deployment, Activation: activation})
}

func (s *Server) handleGetTeamDeployment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
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
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, deployment)
}

func (s *Server) handleListTeamDeployments(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
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
		s.respondTeamError(w, err)
		return
	}
	items := make([]kernelapi.TeamDeploymentCatalogEntry, 0, len(deployments))
	for _, deployment := range deployments {
		definition, definitionErr := registry.GetDefinition(r.Context(), deployment.DefinitionID, deployment.ActiveVersion)
		if definitionErr != nil {
			s.respondTeamError(w, definitionErr)
			return
		}
		items = append(items, kernelapi.TeamDeploymentCatalogEntry{Deployment: deployment, Definition: definition})
	}
	s.respondJSON(w, http.StatusOK, kernelapi.TeamDeploymentList{Items: items})
}

func (s *Server) handleUpdateTeamDeployment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelapi.UpdateTeamDeploymentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if payload.Deployment == nil || strings.TrimSpace(payload.Deployment.ID) != strings.TrimSpace(r.PathValue("id")) {
		s.respondError(w, http.StatusBadRequest, "team deployment path and payload ids must match")
		return
	}
	deployment, activation, err := registry.UpdateDeployment(r.Context(), payload.Deployment, payload.ExpectedRevision,
		payload.ActorType, payload.ActorID, payload.Reason)
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.TeamDeploymentResult{Deployment: deployment, Activation: activation})
}

func (s *Server) handleActivateTeamDefinition(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelapi.ActivateTeamDefinitionRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	deployment, activation, err := registry.ActivateDefinition(r.Context(), payload.Scope, strings.TrimSpace(r.PathValue("id")), payload.Version,
		payload.ExpectedRevision, payload.ActorType, payload.ActorID, payload.Reason)
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.TeamDeploymentResult{Deployment: deployment, Activation: activation})
}

func (s *Server) handleListTeamDefinitionActivations(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	activations, err := registry.ListActivations(r.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, activations)
}

func (s *Server) handleProposeTeamDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelteam.ProposeAmendmentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	deploymentID := strings.TrimSpace(r.PathValue("id"))
	if strings.TrimSpace(payload.DeploymentID) != deploymentID {
		s.respondError(w, http.StatusBadRequest, "team deployment path and amendment payload ids must match")
		return
	}
	amendment, err := registry.ProposeAmendment(r.Context(), payload)
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusCreated, amendment)
}

func (s *Server) handleListTeamDefinitionAmendments(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
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
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.TeamDefinitionAmendmentList{Items: amendments})
}

func (s *Server) handleGetTeamDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	amendment, err := registry.GetAmendment(r.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, strings.TrimSpace(r.PathValue("amendmentId")))
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	if amendment.DeploymentID != strings.TrimSpace(r.PathValue("id")) {
		s.respondTeamError(w, kernelteam.ErrAmendmentNotFound)
		return
	}
	s.respondJSON(w, http.StatusOK, amendment)
}

func (s *Server) handleEvaluateTeamDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelteam.SubmitAmendmentEvaluationRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !teamAmendmentPathMatches(r, payload.AmendmentID) {
		s.respondError(w, http.StatusBadRequest, "team amendment path and payload ids must match")
		return
	}
	current, err := registry.GetAmendment(r.Context(), payload.Scope, payload.AmendmentID)
	if err != nil || current.DeploymentID != strings.TrimSpace(r.PathValue("id")) {
		s.respondTeamError(w, kernelteam.ErrAmendmentNotFound)
		return
	}
	amendment, err := registry.SubmitAmendmentEvaluation(r.Context(), payload)
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, amendment)
}

func (s *Server) handleResolveTeamDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelteam.ResolveAmendmentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !teamAmendmentPathMatches(r, payload.AmendmentID) {
		s.respondError(w, http.StatusBadRequest, "team amendment path and payload ids must match")
		return
	}
	current, err := registry.GetAmendment(r.Context(), payload.Scope, payload.AmendmentID)
	if err != nil || current.DeploymentID != strings.TrimSpace(r.PathValue("id")) {
		s.respondTeamError(w, kernelteam.ErrAmendmentNotFound)
		return
	}
	amendment, err := registry.ResolveAmendment(r.Context(), payload)
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, amendment)
}

func (s *Server) handleActivateTeamDefinitionAmendment(w http.ResponseWriter, r *http.Request) {
	registry, err := s.teamRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	var payload kernelapi.ActivateTeamDefinitionAmendmentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	amendmentID := strings.TrimSpace(r.PathValue("amendmentId"))
	current, err := registry.GetAmendment(r.Context(), payload.Scope, amendmentID)
	if err != nil || current.DeploymentID != strings.TrimSpace(r.PathValue("id")) {
		s.respondTeamError(w, kernelteam.ErrAmendmentNotFound)
		return
	}
	amendment, deployment, activation, err := registry.ActivateAmendment(r.Context(), payload.Scope, amendmentID, payload.ExpectedRevision, payload.ActorType, payload.ActorID, payload.Reason)
	if err != nil {
		s.respondTeamError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.TeamDefinitionAmendmentActivationResult{Amendment: amendment, Deployment: deployment, Activation: activation})
}

func teamAmendmentPathMatches(r *http.Request, amendmentID string) bool {
	return strings.TrimSpace(amendmentID) != "" && strings.TrimSpace(amendmentID) == strings.TrimSpace(r.PathValue("amendmentId"))
}

func (s *Server) respondTeamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, kernelteam.ErrDefinitionNotFound), errors.Is(err, kernelteam.ErrDeploymentNotFound), errors.Is(err, kernelteam.ErrAmendmentNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, kernelteam.ErrRevisionConflict):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
