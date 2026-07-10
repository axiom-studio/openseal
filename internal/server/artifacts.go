package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (s *Server) handleRegisterArtifact(w http.ResponseWriter, r *http.Request) {
	catalog, ok := s.artifactCatalog()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "artifact catalog is not available")
		return
	}
	var request runtime.RegisterArtifactRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := catalog.Register(r.Context(), request)
	if err != nil {
		s.respondArtifactError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result)
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	catalog, ok := s.artifactCatalog()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "artifact catalog is not available")
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	version, err := boundedInt64Query(r, "version", 0, 0)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	artifact, err := catalog.Get(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), version)
	if err != nil {
		s.respondArtifactError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, artifact)
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	catalog, ok := s.artifactCatalog()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "artifact catalog is not available")
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	latestOnly := true
	if raw := strings.TrimSpace(r.URL.Query().Get("latestOnly")); raw != "" {
		latestOnly, err = strconv.ParseBool(raw)
		if err != nil {
			s.respondError(w, http.StatusBadRequest, "latestOnly must be true or false")
			return
		}
	}
	filter := runtime.ArtifactFilter{
		Scope: scope, ID: strings.TrimSpace(r.URL.Query().Get("id")),
		Types: queryValues(r, "type"), MediaTypes: queryValues(r, "mediaType"),
		ProducerRunID:     strings.TrimSpace(r.URL.Query().Get("producerRunId")),
		ProducerRequestID: strings.TrimSpace(r.URL.Query().Get("producerRequestId")),
		EvidenceTarget:    strings.TrimSpace(r.URL.Query().Get("evidenceTarget")),
		LatestOnly:        latestOnly, Limit: limit, Offset: offset,
	}
	for _, value := range queryValues(r, "classification") {
		classification := runtime.ArtifactClassification(value)
		switch classification {
		case runtime.ArtifactClassificationPublic, runtime.ArtifactClassificationInternal,
			runtime.ArtifactClassificationConfidential, runtime.ArtifactClassificationRestricted:
			filter.Classifications = append(filter.Classifications, classification)
		default:
			s.respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid artifact classification %q", value))
			return
		}
	}
	artifacts, err := catalog.List(r.Context(), filter)
	if err != nil {
		s.respondArtifactError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, artifacts)
}

func (s *Server) artifactCatalog() (*runtime.ArtifactCatalog, bool) {
	store, ok := s.store.(runtime.ArtifactStore)
	if !ok {
		return nil, false
	}
	return runtime.NewArtifactCatalog(store), true
}

func (s *Server) respondArtifactError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrArtifactNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrArtifactImmutable), errors.Is(err, runtime.ErrArtifactVersionConflict):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrInvalidArtifactRecord), errors.Is(err, runtime.ErrInvalidScope):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("artifact API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "artifact operation failed")
	}
}

func boundedInt64Query(r *http.Request, key string, fallback, min int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < min {
		return 0, fmt.Errorf("%s must be at least %d", key, min)
	}
	return value, nil
}
