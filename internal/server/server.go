package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	kernelbundle "github.com/axiom-studio/openseal/pkg/bundle"
	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

// Server exposes the versioned OpenSeal kernel API. Interactive clients
// discover its exact capabilities rather than depending on hidden routes.
type Server struct {
	channelParticipationAuthorizer func(context.Context, *runtime.Conversation) error
	channelMessageDispatcher       func(context.Context, runtime.PostChannelMessageRequest) (*runtime.ChannelMessageCommitResult, error)
	reactionProjector              runtime.MessageReactionProjector

	store                    runtime.KernelStore
	artifactContent          runtime.ArtifactContentStore
	artifactResolver         runtime.ArtifactContentResolver
	actionApprovalAuth       runtime.ApprovalAuthorizer
	logger                   *zap.SugaredLogger
	mux                      *http.ServeMux
	httpServer               *http.Server
	bearerToken              string
	clawHub                  *opensealkernel.Engine
	clawHubMutations         bool
	teamWorkEnabled          bool
	desktopConversationScope *runtime.Scope
	agentRunCreation         func(context.Context, runtime.CreateAgentRunRequest) (*runtime.AgentRunCommandResult, error)
	workforceBundles         kernelbundle.InstallationStore
	workforceBundleActor     string
	workforceBundleTrust     kernelbundle.TrustPolicy
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
	return s.authenticatedHandler()
}

// SetBearerToken protects every API route with a bearer token. An empty token
// preserves the existing embedding and standalone behavior.
// Configure it before creating a handler or serving requests.
func (s *Server) SetBearerToken(token string) {
	s.bearerToken = strings.TrimSpace(token)
}

// healthProbePath is the one route reachable without a bearer token.
//
// A container liveness probe cannot carry a credential. Docker's HEALTHCHECK
// and Compose's healthcheck run a fixed command with no access to the token, so
// gating this route makes an authenticated deployment permanently unhealthy --
// which is what the daemon's own startup warning tells operators to configure.
// docker compose up --wait then fails, depends_on: service_healthy never
// satisfies, and orchestrators restart-loop a container that is working.
//
// The exemption is narrow on purpose: exact path, GET only. handleHealth
// responds {"status":"ok"} and nothing else -- no store contents, no
// configuration, no identifiers -- so it discloses nothing that connecting to
// the port does not already reveal. Every other route, and every other method
// on this path, still requires the token.
const healthProbePath = "/api/v1/health"

func (s *Server) authenticatedHandler() http.Handler {
	token := s.bearerToken
	if token == "" {
		return s.mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == healthProbePath {
			s.mux.ServeHTTP(w, r)
			return
		}
		scheme, provided, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		provided = strings.TrimLeft(provided, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", `Bearer realm="openseal"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		s.mux.ServeHTTP(w, r)
	})
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

// SetActionApprovalAuthorizer enables resolution of durable action approval
// checkpoints. Read-only approval inspection remains available without it.
func (s *Server) SetActionApprovalAuthorizer(authorizer runtime.ApprovalAuthorizer) {
	s.actionApprovalAuth = authorizer
}

// SetAgentRunCreationDispatcher enables Run creation only after the host has
// installed a worker capable of claiming the created work. Inspection and
// lifecycle intervention remain available without a worker.
func (s *Server) SetAgentRunCreationDispatcher(dispatch func(context.Context, runtime.CreateAgentRunRequest) (*runtime.AgentRunCommandResult, error)) {
	s.agentRunCreation = dispatch
}

// SetTeamWorkEnabled advertises team ownership only for a capable host.
func (s *Server) SetTeamWorkEnabled(enabled bool) { s.teamWorkEnabled = enabled }

// SetWorkforceBundleInstallation enables the one-transaction installation
// boundary. The actor is host-authenticated configuration, never client input.
func (s *Server) SetWorkforceBundleInstallation(store kernelbundle.InstallationStore, actorID string, trust ...kernelbundle.TrustPolicy) {
	s.workforceBundles, s.workforceBundleActor = store, actorID
	if len(trust) > 0 {
		s.workforceBundleTrust = trust[0]
	}
}

// ListenAndServe starts the server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(listener)
}

// Serve starts the API on an already-bound listener. Desktop supervisors use
// this to bind port zero safely and learn the selected loopback endpoint.
func (s *Server) Serve(listener net.Listener) error {
	s.logger.Infow("starting API server", "addr", listener.Addr().String())
	s.httpServer = &http.Server{Handler: s.authenticatedHandler()}
	return s.httpServer.Serve(listener)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
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

// SetDesktopConversationScope binds interactive channel creation, updates, and message
// authorship to the authenticated operator of one local desktop workspace.
// Configure before serving requests; standalone channel behavior is unchanged.
func (s *Server) SetDesktopConversationScope(scope runtime.Scope) {
	s.desktopConversationScope = &scope
}

// SetReactionProjector mirrors reactions made through the API to external
// conversations. Configure before serving.
func (s *Server) SetReactionProjector(projector runtime.MessageReactionProjector) {
	s.reactionProjector = projector
}

// SetChannelParticipation enables explicit participation settings and optionally
// connects posting to immediate durable scheduling. Configure before serving.
// The authorizer validates enabling; disabling remains available during outages.
func (s *Server) SetChannelParticipation(authorize func(context.Context, *runtime.Conversation) error, post func(context.Context, runtime.PostChannelMessageRequest) (*runtime.ChannelMessageCommitResult, error)) {
	s.channelParticipationAuthorizer = authorize
	s.channelMessageDispatcher = post
}
