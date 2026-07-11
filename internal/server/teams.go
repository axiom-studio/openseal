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

func (s *Server) respondTeamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, kernelteam.ErrDefinitionNotFound), errors.Is(err, kernelteam.ErrDeploymentNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, kernelteam.ErrRevisionConflict):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
