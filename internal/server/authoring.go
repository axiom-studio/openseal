package server

import (
	"net/http"

	"github.com/axiom-studio/openseal/pkg/authoring"
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
