package server

import (
	"net/http"
	"strconv"

	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (s *Server) handleRunWorkflow(w http.ResponseWriter, r *http.Request) {
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

	entry := runtime.WorkflowEntry{Name: wf.Name}
	entry.Nodes = make([]*executor.NodeDefinition, len(wf.Nodes))
	for i, n := range wf.Nodes {
		entry.Nodes[i] = &executor.NodeDefinition{
			Id:     n.ID,
			Name:   n.ID,
			Type:   n.Type,
			Config: n.Config,
		}
	}
	entry.Connections = make([]*executor.ConnectionDefinition, len(wf.Edges))
	for i, e := range wf.Edges {
		entry.Connections[i] = &executor.ConnectionDefinition{
			Id:           "edge-" + strconv.Itoa(i),
			SourceNodeId: e.From,
			TargetNodeId: e.To,
			Label:        e.Condition,
		}
	}
	if len(entry.Nodes) > 0 {
		entry.StartNodeID = entry.Nodes[0].Id
	}
	runID, err := s.scheduler.Schedule(r.Context(), entry, nil)
	if err != nil {
		s.respondError(w, 500, "failed to schedule run: "+err.Error())
		return
	}

	s.respondJSON(w, 202, map[string]interface{}{
		"runId":    runID,
		"status":   "pending",
		"workflow": wf.Name,
	})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns(r.Context(), 0)
	if err != nil {
		s.respondError(w, 500, "failed to list runs: "+err.Error())
		return
	}
	s.respondJSON(w, 200, runs)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	if idStr == "" {
		s.respondError(w, 400, "run id is required")
		return
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		s.respondError(w, 400, "invalid run id")
		return
	}

	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		s.respondError(w, 500, "failed to get run: "+err.Error())
		return
	}
	if run == nil {
		s.respondError(w, 404, "run not found")
		return
	}
	s.respondJSON(w, 200, run)
}
