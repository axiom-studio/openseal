package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
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

	ctx := context.Background()
	runID, err := s.store.CreateRun(ctx, wf.Name)
	if err != nil {
		s.respondError(w, 500, "failed to create run: "+err.Error())
		return
	}

	go func() {
		nodes := make([]*executor.NodeDefinition, len(wf.Nodes))
		for i, n := range wf.Nodes {
			nodes[i] = &executor.NodeDefinition{
				Id:     n.ID,
				Name:   n.ID,
				Type:   n.Type,
				Config: n.Config,
			}
		}
		connections := make([]*executor.ConnectionDefinition, len(wf.Edges))
		for i, e := range wf.Edges {
			connections[i] = &executor.ConnectionDefinition{
				Id:           "edge-" + strconv.Itoa(i),
				SourceNodeId: e.From,
				TargetNodeId: e.To,
				Label:        e.Condition,
			}
		}

		startNodeID := ""
		if len(nodes) > 0 {
			startNodeID = nodes[0].Id
		}

		execCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		result, err := s.pe.Execute(execCtx, runID, nodes, connections, startNodeID, nil, nil)
		if err != nil {
			s.store.UpdateRunStatus(execCtx, runID, "failed", err)
			return
		}

		for nodeID, nr := range result.NodeResults {
			s.store.UpdateNodeResult(execCtx, runID, nodeID, nr)
		}
		s.store.UpdateRunStatus(execCtx, runID, result.Status, result.Error)
	}()

	s.respondJSON(w, 202, map[string]interface{}{
		"runId":    runID,
		"status":   "pending",
		"workflow": wf.Name,
	})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	runs, err := s.store.ListRuns(ctx, 0)
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

	ctx := context.Background()
	run, err := s.store.GetRun(ctx, id)
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
