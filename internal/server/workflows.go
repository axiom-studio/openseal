package server

import (
	"net/http"
)

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	s.muWorkflows.RLock()
	defer s.muWorkflows.RUnlock()

	list := make([]WorkflowEntry, 0, len(s.workflows))
	for _, wf := range s.workflows {
		list = append(list, *wf)
	}
	s.respondJSON(w, 200, list)
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.respondError(w, 400, "workflow id is required")
		return
	}

	s.muWorkflows.RLock()
	defer s.muWorkflows.RUnlock()

	wf, ok := s.workflows[id]
	if !ok {
		s.respondError(w, 404, "workflow not found")
		return
	}
	s.respondJSON(w, 200, wf)
}
