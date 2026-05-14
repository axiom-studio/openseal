package server

import (
	"net/http"

	"github.com/axiom-studio/openseal/pkg/executor"
)

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	infos := s.registry.List()
	s.respondJSON(w, 200, infos)
}

func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.respondError(w, 400, "skill id is required")
		return
	}

	meta, err := executor.LoadNodeMetadata()
	if err != nil {
		s.respondError(w, 500, "failed to load skill metadata")
		return
	}

	m, ok := meta[id]
	if !ok {
		s.respondError(w, 404, "skill not found")
		return
	}

	info := executor.ExecutorInfo{
		Type:         m.Name,
		Name:         m.DisplayName,
		Category:     m.Category,
		Description:  m.Description,
		Icon:         m.Icon,
		InputSchema:  m.InputSchema,
		OutputSchema: m.OutputSchema,
	}
	s.respondJSON(w, 200, info)
}
