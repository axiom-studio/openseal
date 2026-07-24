package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/authoring"
	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/source"
	"go.uber.org/zap"
)

// Server exposes the versioned OpenSeal kernel API. Interactive clients
// discover its exact capabilities rather than depending on hidden routes.
type Server struct {
	store                runtime.KernelStore
	artifactContent      runtime.ArtifactContentStore
	artifactResolver     runtime.ArtifactContentResolver
	authoring            *authoring.Compiler
	authoringChanges     *authoring.ChangeSetService
	authoringRuns        *runtime.WorkforceAuthoringRunService
	authoringWorker      *runtime.WorkforceAuthoringWorker
	authoringSkillSearch authoring.SkillSearchProvider
	authoringScope       runtime.Scope
	authoringMu          sync.Mutex
	workforceAuthority   WorkforceLifecycleAuthorizer
	actionApprovalAuth   runtime.ApprovalAuthorizer
	logger               *zap.SugaredLogger
	mux                  *http.ServeMux
	httpServer           *http.Server
	clawHub              *opensealkernel.Engine
	clawHubMutations     bool
	outreachDelivery     func(context.Context, runtime.CreateAgentRunRequest) (*runtime.AgentRunCommandResult, error)
	agentRunCreation     func(context.Context, runtime.CreateAgentRunRequest) (*runtime.AgentRunCommandResult, error)
	sourcePolicies       *source.LifecycleService
}

// SetWorkforceSkillSearchProvider enables verified, paginated Skill discovery
// during authoring. The provider remains the authority for current catalog
// identity and readiness.
func (s *Server) SetWorkforceSkillSearchProvider(provider authoring.SkillSearchProvider) {
	s.authoringSkillSearch = provider
}

// SetClawHubLifecycle enables the canonical registry/install engine. Mutation
// authority is supplied by the host and should only be true at a trusted local
// operator boundary; read operations remain available otherwise.
func (s *Server) SetClawHubLifecycle(engine *opensealkernel.Engine, allowMutations bool) {
	s.clawHub, s.clawHubMutations = engine, allowMutations
}

// NewServer creates a new API server.
func NewServer(store runtime.KernelStore, logger *zap.SugaredLogger) *Server {
	s := &Server{
		store:  store,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// Handler returns the server's HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// SetArtifactContentStore enables streamed artifact upload/download routes.
// Configure it before serving requests.
func (s *Server) SetArtifactContentStore(store runtime.ArtifactContentStore) {
	s.artifactContent = store
}

// SetArtifactContentResolver enables ephemeral authorized content resolution.
// Resolved URLs are returned to the caller and never persisted by Server.
func (s *Server) SetArtifactContentResolver(resolver runtime.ArtifactContentResolver) {
	s.artifactResolver = resolver
}

// SetWorkforceAuthoringCompiler enables non-activating prompt compilation.
func (s *Server) SetWorkforceAuthoringCompiler(compiler *authoring.Compiler) {
	s.authoring = compiler
	s.authoringChanges = nil
	s.authoringRuns = nil
	if store, ok := s.store.(authoring.ChangeSetStore); ok && compiler != nil {
		s.authoringChanges, _ = authoring.NewChangeSetService(compiler, store)
	}
	if store, ok := s.store.(runtime.WorkforceAuthoringRunStore); ok && compiler != nil {
		s.authoringRuns, _ = runtime.NewWorkforceAuthoringRunService(compiler, store)
	}
}

// StartWorkforceAuthoringWorker hosts durable proposal generation for one
// explicit scope. Standalone OpenSeal deliberately defaults to local/default;
// multi-tenant scheduling belongs to an embedding host.
func (s *Server) StartWorkforceAuthoringWorker(ctx context.Context, scope runtime.Scope, workerID string) error {
	s.authoringMu.Lock()
	defer s.authoringMu.Unlock()
	if s.authoringWorker != nil {
		return fmt.Errorf("workforce authoring worker is already started")
	}
	if s.authoringRuns == nil {
		return fmt.Errorf("workforce authoring durable store is not configured")
	}
	worker, err := runtime.NewWorkforceAuthoringWorker(s.authoringRuns, s.logger, runtime.WorkforceAuthoringWorkerConfig{
		Scope: scope, WorkerID: workerID, LeaseDuration: 12 * time.Minute, GenerationTimeout: 10 * time.Minute,
	})
	if err != nil {
		return err
	}
	if err := worker.Start(ctx); err != nil {
		return err
	}
	s.authoringScope, s.authoringWorker = scope, worker
	return nil
}

// SetWorkforceLifecycleAuthorizer enables governed evaluation, approval, and
// Apply operations. With no authorizer these mutations remain unavailable;
// OpenSeal never manufactures a local approver or policy evaluator.
func (s *Server) SetWorkforceLifecycleAuthorizer(authorizer WorkforceLifecycleAuthorizer) {
	s.workforceAuthority = authorizer
}

// SetActionApprovalAuthorizer enables resolution of durable action approval
// checkpoints. Read-only approval inspection remains available without it.
func (s *Server) SetActionApprovalAuthorizer(authorizer runtime.ApprovalAuthorizer) {
	s.actionApprovalAuth = authorizer
}

// SetOutreachDeliveryDispatcher enables the delivery operation only when a
// host has wired a real canonical Agent Run worker. Without it, standalone
// OpenSeal advertises and serves outreach inspection/drafting but never creates
// work that no runtime can execute.
func (s *Server) SetOutreachDeliveryDispatcher(dispatch func(context.Context, runtime.CreateAgentRunRequest) (*runtime.AgentRunCommandResult, error)) {
	s.outreachDelivery = dispatch
}

// SetAgentRunCreationDispatcher enables Run creation only after the host has
// installed a worker capable of claiming the created work. Inspection and
// lifecycle intervention remain available without a worker.
func (s *Server) SetAgentRunCreationDispatcher(dispatch func(context.Context, runtime.CreateAgentRunRequest) (*runtime.AgentRunCommandResult, error)) {
	s.agentRunCreation = dispatch
}

// SetSourcePolicyLifecycle installs the host-selected persistence boundary for
// governed source authority. Without it, routes and capabilities fail closed.
func (s *Server) SetSourcePolicyLifecycle(service *source.LifecycleService) {
	s.sourcePolicies = service
}

// ListenAndServe starts the server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	s.logger.Infow("starting API server", "addr", addr)
	s.httpServer = &http.Server{Addr: addr, Handler: s.mux}
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.authoringMu.Lock()
	worker := s.authoringWorker
	s.authoringWorker = nil
	s.authoringMu.Unlock()
	if worker != nil {
		worker.Stop()
	}
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
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
