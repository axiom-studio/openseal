package server

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
)

func (s *Server) requireClawHub(w http.ResponseWriter, mutation bool) (*opensealkernel.Engine, bool) {
	if s.clawHub == nil {
		s.respondError(w, http.StatusServiceUnavailable, "ClawHub lifecycle is unavailable")
		return nil, false
	}
	if mutation && !s.clawHubMutations {
		s.respondError(w, http.StatusForbidden, "ClawHub lifecycle mutation requires a trusted local operator")
		return nil, false
	}
	return s.clawHub, true
}

func parseClawHubReference(r *http.Request) (clawhub.SkillReference, error) {
	return clawhub.ParseSkillReference(strings.TrimSpace(r.PathValue("reference")))
}

func (s *Server) handleSearchClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, false)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" || len(query) > 256 {
		s.respondError(w, http.StatusBadRequest, "Skill search query must be 1 to 256 characters")
		return
	}
	result, err := engine.SearchClawHubSkills(r.Context(), clawhub.SearchRequest{Query: query, Limit: 10, NonSuspiciousOnly: true})
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handleInspectClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, false)
	if !ok {
		return
	}
	ref, err := parseClawHubReference(r)
	if err != nil {
		s.respondError(w, 400, "invalid ClawHub reference")
		return
	}
	result, err := engine.InspectClawHubSkill(r.Context(), ref)
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}
func (s *Server) handleListClawHubVersions(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, false)
	if !ok {
		return
	}
	ref, err := parseClawHubReference(r)
	if err != nil {
		s.respondError(w, 400, "invalid ClawHub reference")
		return
	}
	limit := 25
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			s.respondError(w, 400, "limit must be between 1 and 100")
			return
		}
	}
	result, err := engine.ListClawHubSkillVersions(r.Context(), ref, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}
func (s *Server) handleGetClawHubFile(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, false)
	if !ok {
		return
	}
	ref, err := parseClawHubReference(r)
	if err != nil {
		s.respondError(w, 400, "invalid ClawHub reference")
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" || strings.Contains(path, "..") || strings.HasPrefix(path, "/") {
		s.respondError(w, 400, "safe relative path is required")
		return
	}
	content, err := engine.GetClawHubSkillFile(r.Context(), ref, r.URL.Query().Get("version"), r.URL.Query().Get("tag"), path)
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, map[string]interface{}{"path": path, "size": len(content), "contentBase64": base64.StdEncoding.EncodeToString(content)})
}
func (s *Server) handleVerifyClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, false)
	if !ok {
		return
	}
	ref, err := parseClawHubReference(r)
	if err != nil {
		s.respondError(w, 400, "invalid ClawHub reference")
		return
	}
	var request kernelapi.ClawHubVersionRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, 400, "invalid verification request")
		return
	}
	result, err := engine.VerifyClawHubSkill(r.Context(), ref, request.Version, request.Tag)
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}
func (s *Server) handleListInstalledClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, false)
	if !ok {
		return
	}
	result, err := engine.ListInstalledClawHubSkillStates()
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}
func (s *Server) handleInstallClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, true)
	if !ok {
		return
	}
	ref, err := parseClawHubReference(r)
	if err != nil {
		s.respondError(w, 400, "invalid ClawHub reference")
		return
	}
	var request kernelapi.ClawHubVersionRequest
	if err := decodeStrictJSON(r, &request); err != nil || request.Version != "" && request.Tag != "" {
		s.respondError(w, 400, "version and tag are mutually exclusive")
		return
	}
	_, receipt, err := engine.InstallClawHubSkillLifecycle(r.Context(), clawhub.InstallRequest{Reference: ref, Version: request.Version, Tag: request.Tag})
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	status := http.StatusCreated
	if !receipt.Changed {
		status = http.StatusOK
	}
	s.respondJSON(w, status, receipt)
}
func (s *Server) handleVerifyInstalledClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, false)
	if !ok {
		return
	}
	result, err := engine.VerifyInstalledClawHubSkill(r.Context(), r.PathValue("reference"))
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}
func (s *Server) handlePinClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, true)
	if !ok {
		return
	}
	var request kernelapi.ClawHubPinRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, 400, "invalid pin request")
		return
	}
	result, err := engine.PinClawHubSkillLifecycle(r.PathValue("reference"), request.Reason)
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}
func (s *Server) handleUnpinClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, true)
	if !ok {
		return
	}
	result, err := engine.UnpinClawHubSkillLifecycle(r.PathValue("reference"))
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}
func (s *Server) handleUpdateClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, true)
	if !ok {
		return
	}
	_, result, err := engine.UpdateClawHubSkillLifecycle(r.Context(), r.PathValue("reference"))
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}
func (s *Server) handleUpdateAllClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, true)
	if !ok {
		return
	}
	result, err := engine.UpdateAllClawHubSkills(r.Context())
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}
func (s *Server) handleUninstallClawHub(w http.ResponseWriter, r *http.Request) {
	engine, ok := s.requireClawHub(w, true)
	if !ok {
		return
	}
	result, err := engine.UninstallClawHubSkill(r.PathValue("reference"), false)
	if err != nil {
		s.respondClawHubError(w, err)
		return
	}
	s.respondJSON(w, 200, result)
}

func (s *Server) respondClawHubError(w http.ResponseWriter, err error) {
	status, message := http.StatusBadGateway, "ClawHub lifecycle operation failed"
	switch {
	case errors.Is(err, clawhub.ErrNotFound):
		status, message = 404, "ClawHub skill not found"
	case errors.Is(err, clawhub.ErrSkillPinned), errors.Is(err, clawhub.ErrSkillModified), errors.Is(err, clawhub.ErrAmbiguousSkill):
		status, message = 409, "ClawHub lifecycle conflict"
	case errors.Is(err, clawhub.ErrVerificationFailed):
		status, message = 422, "ClawHub verification failed"
	}
	s.respondJSON(w, status, map[string]string{"error": message, "code": string(opensealkernel.ClassifyClawHubLifecycleError(err))})
}
