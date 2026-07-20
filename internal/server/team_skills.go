package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func (s *Server) handleListTeamSkillBindings(w http.ResponseWriter, r *http.Request) {
	catalog, scope, teamID, ok := s.teamSkillCatalog(w, r)
	if !ok {
		return
	}
	bindings, err := catalog.ListBindings(r.Context(), scope, teamID)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "Team Skill bindings could not be listed")
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.SkillBindingList{APIVersion: kernelapi.SkillBindingsCapabilityVersion, DeploymentID: teamID, Items: bindings})
}

func (s *Server) handleUpsertTeamSkillBinding(w http.ResponseWriter, r *http.Request) {
	catalog, scope, teamID, ok := s.teamSkillCatalog(w, r)
	if !ok {
		return
	}
	bindingID := strings.TrimSpace(r.PathValue("bindingId"))
	var request skill.UpsertBindingRequest
	if bindingID == "" {
		s.respondError(w, http.StatusBadRequest, "binding id is required")
		return
	}
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Binding == nil {
		s.respondError(w, http.StatusBadRequest, "binding is required")
		return
	}
	if request.Binding.Scope != (skill.ScopeReference{}) && request.Binding.Scope != scope ||
		request.Binding.DeploymentID != "" && request.Binding.DeploymentID != teamID ||
		request.Binding.ID != "" && request.Binding.ID != bindingID {
		s.respondError(w, http.StatusBadRequest, "Team Skill binding identity does not match the request resource")
		return
	}
	request.Binding.Scope, request.Binding.DeploymentID, request.Binding.ID = scope, teamID, bindingID
	value, err := catalog.UpsertBinding(r.Context(), request)
	if err != nil {
		s.respondSkillBindingError(w, err)
		return
	}
	status := http.StatusOK
	if request.ExpectedRevision == 0 {
		status = http.StatusCreated
	}
	s.respondJSON(w, status, kernelapi.SkillBindingMutationResult{APIVersion: kernelapi.SkillBindingsCapabilityVersion, Binding: value})
}

func (s *Server) handleDisableTeamSkillBinding(w http.ResponseWriter, r *http.Request) {
	catalog, scope, teamID, ok := s.teamSkillCatalog(w, r)
	if !ok {
		return
	}
	bindingID := strings.TrimSpace(r.PathValue("bindingId"))
	var request skill.DisableBindingRequest
	if bindingID == "" {
		s.respondError(w, http.StatusBadRequest, "binding id is required")
		return
	}
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Scope != (skill.ScopeReference{}) && request.Scope != scope ||
		request.DeploymentID != "" && request.DeploymentID != teamID ||
		request.BindingID != "" && request.BindingID != bindingID {
		s.respondError(w, http.StatusBadRequest, "Team Skill binding identity does not match the request resource")
		return
	}
	request.Scope, request.DeploymentID, request.BindingID = scope, teamID, bindingID
	value, err := catalog.DisableBinding(r.Context(), request)
	if err != nil {
		s.respondSkillBindingError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.SkillBindingMutationResult{APIVersion: kernelapi.SkillBindingsCapabilityVersion, Binding: value})
}

func (s *Server) teamSkillCatalog(w http.ResponseWriter, r *http.Request) (*skill.Catalog, skill.ScopeReference, string, bool) {
	skillStore, skillsOK := s.store.(skill.CatalogStore)
	teamStore, teamsOK := s.store.(kernelteam.Store)
	if !skillsOK || !teamsOK {
		s.respondError(w, http.StatusServiceUnavailable, "Team Skill binding management is unavailable")
		return nil, skill.ScopeReference{}, "", false
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return nil, skill.ScopeReference{}, "", false
	}
	teamID := strings.TrimSpace(r.PathValue("id"))
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	if teamID == "" {
		s.respondError(w, http.StatusBadRequest, "Team deployment id is required")
		return nil, skill.ScopeReference{}, "", false
	}
	if _, err := teamStore.GetTeamDeployment(r.Context(), skillScope, teamID); err != nil {
		s.respondError(w, http.StatusNotFound, "Team deployment not found")
		return nil, skill.ScopeReference{}, "", false
	}
	return skill.NewCatalogWithStore(skillStore), skillScope, teamID, true
}

func (s *Server) respondSkillBindingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, skill.ErrBindingRevisionConflict):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, skill.ErrBindingNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
