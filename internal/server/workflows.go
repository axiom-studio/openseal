package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/axiom-studio/openseal/pkg/validation"
	"github.com/axiom-studio/openseal/pkg/workflow"
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

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req WorkflowEntry
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.respondError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		s.respondError(w, 400, "workflow name is required")
		return
	}

	// Convert to pkg/workflow.Workflow for HCL generation
	wf := &workflow.Workflow{
		Name: req.Name,
	}
	for _, n := range req.Nodes {
		wf.Nodes = append(wf.Nodes, workflow.Node{
			Type:   n.Type,
			ID:     n.ID,
			Config: n.Config,
		})
	}
	for _, e := range req.Edges {
		wf.Edges = append(wf.Edges, workflow.Edge{
			From:      e.From,
			To:        e.To,
			Condition: e.Condition,
		})
	}

	// Validate before saving
	vwf := &validation.Workflow{
		Name: req.Name,
	}
	for _, n := range req.Nodes {
		vwf.Nodes = append(vwf.Nodes, validation.Node{
			ID:     n.ID,
			Type:   n.Type,
			Config: n.Config,
		})
	}
	for _, e := range req.Edges {
		vwf.Edges = append(vwf.Edges, validation.Edge{
			From:      e.From,
			To:        e.To,
			Condition: e.Condition,
		})
	}
	validator := validation.NewValidator(s.registry)
	result := validator.Validate(vwf)
	if !result.Valid {
		s.respondJSON(w, 422, map[string]interface{}{
			"error":   "validation failed",
			"details": result,
		})
		return
	}

	hclContent := wf.ToHCL()

	// Write to workflows directory
	if s.workflowsDir != "" {
		if err := os.MkdirAll(s.workflowsDir, 0755); err != nil {
			s.respondError(w, 500, "failed to create workflows dir: "+err.Error())
			return
		}
		filename := filepath.Join(s.workflowsDir, req.Name+".hcl")
		if err := os.WriteFile(filename, []byte(hclContent), 0644); err != nil {
			s.respondError(w, 500, "failed to write workflow file: "+err.Error())
			return
		}
	}

	// Update in-memory cache
	s.muWorkflows.Lock()
	s.workflows[req.Name] = &req
	s.muWorkflows.Unlock()

	s.respondJSON(w, 201, map[string]interface{}{
		"name":    req.Name,
		"hcl":     hclContent,
		"message": "workflow created",
	})
}

func (s *Server) handleValidateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req WorkflowEntry
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.respondError(w, 400, "invalid JSON: "+err.Error())
		return
	}

	vwf := &validation.Workflow{
		Name: req.Name,
	}
	for _, n := range req.Nodes {
		vwf.Nodes = append(vwf.Nodes, validation.Node{
			ID:     n.ID,
			Type:   n.Type,
			Config: n.Config,
		})
	}
	for _, e := range req.Edges {
		vwf.Edges = append(vwf.Edges, validation.Edge{
			From:      e.From,
			To:        e.To,
			Condition: e.Condition,
		})
	}
	validator := validation.NewValidator(s.registry)
	result := validator.Validate(vwf)

	status := 200
	if !result.Valid {
		status = 422
	}
	s.respondJSON(w, status, result)
}

func (s *Server) handleGetWorkflowHCL(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.respondError(w, 400, "workflow id is required")
		return
	}

	s.muWorkflows.RLock()
	wf, ok := s.workflows[id]
	s.muWorkflows.RUnlock()
	if !ok {
		s.respondError(w, 404, "workflow not found")
		return
	}

	wfw := &workflow.Workflow{Name: wf.Name}
	for _, n := range wf.Nodes {
		wfw.Nodes = append(wfw.Nodes, workflow.Node{Type: n.Type, ID: n.ID, Config: n.Config})
	}
	for _, e := range wf.Edges {
		wfw.Edges = append(wfw.Edges, workflow.Edge{From: e.From, To: e.To, Condition: e.Condition})
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(200)
	fmt.Fprint(w, wfw.ToHCL())
}
