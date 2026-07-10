package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	internalWorkflow "github.com/axiom-studio/openseal/internal/workflow"
	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

// Server is the HTTP API server for the OpenSeal web GUI.
type Server struct {
	registry     *executor.Registry
	scheduler    *runtime.Scheduler
	store        runtime.KernelStore
	workflowsDir string
	workflows    map[string]*WorkflowEntry
	muWorkflows  sync.RWMutex
	logger       *zap.SugaredLogger
	mux          *http.ServeMux
}

// WorkflowEntry holds a loaded workflow with its source info.
type WorkflowEntry struct {
	Name   string                 `json:"name"`
	Source string                 `json:"source"`
	Nodes  []WorkflowNode         `json:"nodes"`
	Edges  []WorkflowEdge         `json:"edges"`
	Config map[string]interface{} `json:"config,omitempty"`
}

// WorkflowNode is the JSON representation of a node.
type WorkflowNode struct {
	ID     string                 `json:"id"`
	Type   string                 `json:"type"`
	Config map[string]interface{} `json:"config,omitempty"`
}

// WorkflowEdge is the JSON representation of an edge.
type WorkflowEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

// NewServer creates a new API server.
func NewServer(registry *executor.Registry, scheduler *runtime.Scheduler, store runtime.KernelStore, logger *zap.SugaredLogger) *Server {
	return NewServerWithDir(registry, scheduler, store, "", logger)
}

// NewServerWithDir creates a new API server with a workflows directory for persistence.
func NewServerWithDir(registry *executor.Registry, scheduler *runtime.Scheduler, store runtime.KernelStore, workflowsDir string, logger *zap.SugaredLogger) *Server {
	s := &Server{
		registry:     registry,
		scheduler:    scheduler,
		store:        store,
		workflowsDir: workflowsDir,
		workflows:    make(map[string]*WorkflowEntry),
		logger:       logger,
		mux:          http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// SetWorkflows updates the server's workflow cache.
func (s *Server) SetWorkflows(workflows []*internalWorkflow.Workflow) {
	s.muWorkflows.Lock()
	defer s.muWorkflows.Unlock()
	s.workflows = make(map[string]*WorkflowEntry)
	for _, wf := range workflows {
		entry := &WorkflowEntry{
			Name:   wf.Name,
			Source: wf.SourceFile,
			Nodes:  make([]WorkflowNode, len(wf.Nodes)),
			Edges:  make([]WorkflowEdge, len(wf.Edges)),
		}
		for i, n := range wf.Nodes {
			entry.Nodes[i] = WorkflowNode{ID: n.ID, Type: n.Type, Config: n.Config}
		}
		for i, e := range wf.Edges {
			entry.Edges[i] = WorkflowEdge{From: e.From, To: e.To, Condition: e.Condition}
		}
		key := wf.Name
		if _, exists := s.workflows[key]; exists {
			key = wf.SourceFile
		}
		s.workflows[key] = entry
	}
}

// Handler returns the server's HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ListenAndServe starts the server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	s.logger.Infow("starting API server", "addr", addr)
	return http.ListenAndServe(addr, s.mux)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return nil
}

func (s *Server) respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		s.logger.Errorw("failed to encode JSON response", "error", err)
	}
}

func (s *Server) respondError(w http.ResponseWriter, status int, msg string) {
	s.respondJSON(w, status, map[string]string{"error": msg})
}

var _ = fmt.Sprintf
var _ = time.Now
