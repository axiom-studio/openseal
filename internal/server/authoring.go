package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func (s *Server) handleCompileWorkforce(w http.ResponseWriter, r *http.Request) {
	if s.authoring == nil {
		s.respondError(w, http.StatusNotImplemented, "workforce authoring is not configured")
		return
	}
	var request authoring.GenerateRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.authoring.Compile(r.Context(), request)
	if err != nil {
		s.respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handleCreateWorkforceChangeSet(w http.ResponseWriter, r *http.Request) {
	if s.authoringChanges == nil {
		s.respondError(w, http.StatusNotImplemented, "workforce change sets are not configured")
		return
	}
	var request authoring.CreateChangeSetRequest
	if err := decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
		request.IdempotencyKey = key
	}
	result, replayed, err := s.authoringChanges.Create(r.Context(), request)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, authoring.ErrChangeSetIdempotency) {
			status = http.StatusConflict
		}
		s.respondError(w, status, err.Error())
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result)
}

func (s *Server) handleGetWorkforceChangeSet(w http.ResponseWriter, r *http.Request) {
	if s.authoringChanges == nil {
		s.respondError(w, http.StatusNotImplemented, "workforce change sets are not configured")
		return
	}
	scope := capability.ScopeReference{Kind: strings.TrimSpace(r.URL.Query().Get("scopeKind")), ID: strings.TrimSpace(r.URL.Query().Get("scopeId"))}
	result, err := s.authoringChanges.Get(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, authoring.ErrChangeSetNotFound) {
			s.respondError(w, http.StatusNotFound, err.Error())
			return
		}
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}
