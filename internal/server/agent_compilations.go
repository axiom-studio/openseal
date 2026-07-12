package server

import (
	"net/http"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func (s *Server) handleListAgentDefinitionCompilations(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(kernelagent.Store)
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "Agent definition compilation records are unavailable")
		return
	}
	scope := capability.ScopeReference{Kind: strings.TrimSpace(r.URL.Query().Get("scopeKind")), ID: strings.TrimSpace(r.URL.Query().Get("scopeId"))}
	deploymentID := strings.TrimSpace(r.PathValue("id"))
	if scope.Kind == "" || scope.ID == "" || deploymentID == "" {
		s.respondError(w, http.StatusBadRequest, "scopeKind, scopeId, and deployment ID are required")
		return
	}
	if _, err := store.GetDeployment(r.Context(), scope, deploymentID); err != nil {
		s.respondError(w, http.StatusNotFound, "Agent deployment was not found in this scope")
		return
	}
	values, err := store.ListCompilations(r.Context(), scope, deploymentID)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, values)
}
