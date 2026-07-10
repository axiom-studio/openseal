package server

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

const maxStandaloneArtifactUpload = 100 << 20

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

func (s *Server) handleUploadArtifactContent(w http.ResponseWriter, r *http.Request) {
	if s.artifactContent == nil {
		s.respondError(w, http.StatusNotImplemented, "artifact content upload is not available")
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxStandaloneArtifactUpload)
	stored, err := s.artifactContent.Put(r.Context(), runtime.ArtifactContentWrite{
		Scope: scope, Reader: r.Body, SizeBytes: r.ContentLength,
		Digest: strings.TrimSpace(r.Header.Get("X-Content-SHA256")), MediaType: strings.TrimSpace(r.Header.Get("Content-Type")),
	})
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.respondError(w, http.StatusRequestEntityTooLarge, "artifact content exceeds 100 MiB")
			return
		}
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.respondJSON(w, http.StatusCreated, stored)
}

func (s *Server) handleDownloadArtifactContent(w http.ResponseWriter, r *http.Request) {
	if s.artifactContent == nil {
		s.respondError(w, http.StatusNotImplemented, "artifact content download is not available")
		return
	}
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
	content, err := s.artifactContent.Open(r.Context(), scope, artifact.ContentRef)
	if err != nil {
		s.respondArtifactError(w, err)
		return
	}
	defer content.Close()
	if artifact.MediaType != "" {
		w.Header().Set("Content-Type", artifact.MediaType)
	}
	w.Header().Set("X-Content-SHA256", artifact.Digest)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": artifact.Name}))
	if artifact.SizeBytes >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.SizeBytes, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, content); err != nil {
		s.logger.Errorw("stream artifact content", "artifactId", artifact.ID, "error", err)
	}
}

func (s *Server) handleResolveArtifactContent(w http.ResponseWriter, r *http.Request) {
	if s.artifactResolver == nil {
		s.respondError(w, http.StatusNotImplemented, "artifact content resolution is not available")
		return
	}
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
	var payload kernelapi.ResolveArtifactContentRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(payload.Actor.Type) == "" || strings.TrimSpace(payload.Actor.ID) == "" || strings.TrimSpace(payload.Purpose) == "" {
		s.respondError(w, http.StatusBadRequest, "resolution actor and purpose are required")
		return
	}
	if payload.TTLSeconds <= 0 || payload.TTLSeconds > 3600 {
		s.respondError(w, http.StatusBadRequest, "ttlSeconds must be between 1 and 3600")
		return
	}
	artifact, err := catalog.Get(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), version)
	if err != nil {
		s.respondArtifactError(w, err)
		return
	}
	resolution, err := s.artifactResolver.Resolve(r.Context(), runtime.ArtifactContentResolutionRequest{
		Scope: scope, ContentRef: artifact.ContentRef, Actor: payload.Actor,
		Purpose: strings.TrimSpace(payload.Purpose), TTL: time.Duration(payload.TTLSeconds) * time.Second,
	})
	if err != nil {
		s.respondArtifactError(w, err)
		return
	}
	if strings.TrimSpace(resolution.URL) == "" || !resolution.ExpiresAt.After(time.Now()) {
		s.respondError(w, http.StatusInternalServerError, "artifact resolver returned an invalid resolution")
		return
	}
	s.respondJSON(w, http.StatusOK, resolution)
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
