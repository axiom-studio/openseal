package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// handleListActivity exposes only the canonical selector-bounded projection.
// The underlying append store is never an interactive mutation surface.
func (s *Server) handleListActivity(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := 25
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			s.respondError(w, http.StatusBadRequest, "activity limit must be an integer")
			return
		}
	}
	includeDetails := false
	if raw := strings.TrimSpace(r.URL.Query().Get("includeDetails")); raw != "" {
		includeDetails, err = strconv.ParseBool(raw)
		if err != nil {
			s.respondError(w, http.StatusBadRequest, "activity includeDetails must be true or false")
			return
		}
	}
	request := runtime.ActivityFeedRequest{
		Scope: scope, RunID: strings.TrimSpace(r.URL.Query().Get("runId")), AgentID: strings.TrimSpace(r.URL.Query().Get("agentId")),
		ObjectiveID: strings.TrimSpace(r.URL.Query().Get("objectiveId")), ProjectID: strings.TrimSpace(r.URL.Query().Get("projectId")), TeamID: strings.TrimSpace(r.URL.Query().Get("teamId")),
		EventTypes: cleanQueryValues(r.URL.Query()["eventType"]), Cursor: strings.TrimSpace(r.URL.Query().Get("cursor")),
		Limit: limit, IncludeDetails: includeDetails,
	}
	request.Severities, err = activitySeverities(r.URL.Query()["severity"])
	if err == nil {
		request.Visibilities, err = activityVisibilities(r.URL.Query()["visibility"])
	}
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := runtime.NewRunActivityService(s.store, s.store).ListActivityFeed(r.Context(), request)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, page)
}

func activitySeverities(values []string) ([]runtime.ActivitySeverity, error) {
	result := make([]runtime.ActivitySeverity, 0, len(values))
	for _, value := range cleanQueryValues(values) {
		severity := runtime.ActivitySeverity(value)
		switch severity {
		case runtime.ActivitySeverityDebug, runtime.ActivitySeverityInfo, runtime.ActivitySeverityWarning, runtime.ActivitySeverityError:
			result = append(result, severity)
		default:
			return nil, errors.New("activity severity is invalid")
		}
	}
	return result, nil
}

func activityVisibilities(values []string) ([]runtime.ActivityVisibility, error) {
	result := make([]runtime.ActivityVisibility, 0, len(values))
	for _, value := range cleanQueryValues(values) {
		visibility := runtime.ActivityVisibility(value)
		switch visibility {
		case runtime.ActivityVisibilityPrivate, runtime.ActivityVisibilityTeam, runtime.ActivityVisibilityScope:
			result = append(result, visibility)
		default:
			return nil, errors.New("activity visibility is invalid")
		}
	}
	return result, nil
}

func cleanQueryValues(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}
