package server

import (
	"io/fs"
	"net/http"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/webui"
)

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/skills", s.handleListSkills)
	s.mux.HandleFunc("GET /api/v1/skills/{id}", s.handleGetSkill)
	s.mux.HandleFunc("GET /api/v1/workflows", s.handleListWorkflows)
	s.mux.HandleFunc("GET /api/v1/workflows/{id}", s.handleGetWorkflow)
	s.mux.HandleFunc("POST /api/v1/workflows", s.handleCreateWorkflow)
	s.mux.HandleFunc("POST /api/v1/workflows/validate", s.handleValidateWorkflow)
	s.mux.HandleFunc("GET /api/v1/workflows/{id}/hcl", s.handleGetWorkflowHCL)
	s.mux.HandleFunc("POST /api/v1/workflows/{id}/run", s.handleRunWorkflow)
	s.mux.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	s.mux.HandleFunc("GET /api/v1/runs/{id}", s.handleGetRun)
	s.mux.HandleFunc("GET /api/v1/capabilities", s.handleCapabilities)
	s.mux.HandleFunc("POST /api/v1/agent-runs", s.handleCreateAgentRun)
	s.mux.HandleFunc("GET /api/v1/agent-runs", s.handleListAgentRuns)
	s.mux.HandleFunc("GET /api/v1/agent-runs/{id}", s.handleGetAgentRun)
	s.mux.HandleFunc("POST /api/v1/agent-runs/{id}/commands", s.handleCommandAgentRun)
	s.mux.HandleFunc("POST /api/v1/artifacts", s.handleRegisterArtifact)
	s.mux.HandleFunc("GET /api/v1/artifacts", s.handleListArtifacts)
	s.mux.HandleFunc("GET /api/v1/artifacts/{id}", s.handleGetArtifact)

	// Serve static frontend files
	dist, err := fs.Sub(webui.Dist, "dist")
	if err == nil {
		s.mux.Handle("/", http.FileServer(http.FS(dist)))
	}
}

func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	capabilities := []kernelapi.Capability{kernelapi.AgentRunsCapability()}
	if _, ok := s.store.(runtime.ArtifactStore); ok {
		capabilities = append(capabilities, kernelapi.ArtifactCapability())
	}
	s.respondJSON(w, http.StatusOK, kernelapi.NewCapabilityDocument(capabilities...))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.respondJSON(w, 200, map[string]string{"status": "ok"})
}
