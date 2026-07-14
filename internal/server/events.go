package server

import (
	"errors"
	"net/http"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (s *Server) handleRouteEvent(w http.ResponseWriter, r *http.Request) {
	var event runtime.EventEnvelope
	if err := decodeStrictJSON(r, &event); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := runtime.NewObjectiveEventRouter(s.store).Route(r.Context(), event)
	if err != nil {
		s.respondEventRouteError(w, err)
		return
	}
	status := http.StatusOK
	for _, route := range result.Routes {
		if route.Created {
			status = http.StatusCreated
			break
		}
	}
	s.respondJSON(w, status, result)
}

func (s *Server) respondEventRouteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrRunIdempotency):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
