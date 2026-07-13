package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *Server) handleListSkillActions(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(skill.CatalogStore)
	if !ok {
		s.respondError(w, http.StatusServiceUnavailable, "skill action discovery is unavailable")
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	deploymentID := strings.TrimSpace(r.PathValue("deploymentId"))
	if deploymentID == "" || len(deploymentID) > 128 {
		s.respondError(w, http.StatusBadRequest, "agent deployment id is required")
		return
	}
	actions, err := skill.NewCatalogWithStore(store).ListModelActions(r.Context(), skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "skill action discovery failed")
		return
	}
	requiredRoles, err := skillActionSemanticRoles(r.URL.Query()["semanticRole"])
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	sideEffect := strings.TrimSpace(r.URL.Query().Get("sideEffect"))
	if sideEffect != "" && !skillActionSideEffect(sideEffect) {
		s.respondError(w, http.StatusBadRequest, "unsupported skill action side effect")
		return
	}
	filtered := make([]skill.ModelAction, 0, len(actions))
	for _, action := range actions {
		if sideEffect != "" && string(action.SideEffect) != sideEffect {
			continue
		}
		matches := true
		for _, role := range requiredRoles {
			if strings.TrimSpace(action.SemanticArguments[role]) == "" {
				matches = false
				break
			}
		}
		if matches {
			filtered = append(filtered, action)
		}
	}
	sort.Slice(filtered, func(left, right int) bool {
		if filtered[left].Name == filtered[right].Name {
			return filtered[left].BindingID < filtered[right].BindingID
		}
		return filtered[left].Name < filtered[right].Name
	})
	s.respondJSON(w, http.StatusOK, kernelapi.SkillActionList{DeploymentID: deploymentID, Actions: filtered})
}

func skillActionSemanticRoles(values []string) ([]string, error) {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		for _, role := range strings.Split(value, ",") {
			role = strings.TrimSpace(role)
			if role == "" || len(role) > 64 {
				return nil, errors.New("semantic roles must be non-empty and no longer than 64 characters")
			}
			for _, character := range role {
				if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' && character != '-' {
					return nil, errors.New("semantic roles use lowercase letters, numbers, dash, or underscore")
				}
			}
			if !seen[role] {
				seen[role] = true
				result = append(result, role)
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func skillActionSideEffect(value string) bool {
	switch skill.SideEffect(value) {
	case skill.SideEffectNone, skill.SideEffectRead, skill.SideEffectWrite, skill.SideEffectExternal, skill.SideEffectDestructive:
		return true
	default:
		return false
	}
}
