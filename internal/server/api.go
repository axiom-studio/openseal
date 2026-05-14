package server

import (
	"io/fs"
	"net/http"

	"github.com/axiom-studio/openseal/pkg/webui"
)

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/skills", s.handleListSkills)
	s.mux.HandleFunc("GET /api/v1/skills/{id}", s.handleGetSkill)
	s.mux.HandleFunc("GET /api/v1/workflows", s.handleListWorkflows)
	s.mux.HandleFunc("GET /api/v1/workflows/{id}", s.handleGetWorkflow)
	s.mux.HandleFunc("POST /api/v1/workflows/{id}/run", s.handleRunWorkflow)
	s.mux.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	s.mux.HandleFunc("GET /api/v1/runs/{id}", s.handleGetRun)

	// Serve static frontend files
	dist, err := fs.Sub(webui.Dist, "dist")
	if err == nil {
		s.mux.Handle("/", http.FileServer(http.FS(dist)))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.respondJSON(w, 200, map[string]string{"status": "ok"})
}
