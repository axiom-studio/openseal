package commands

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/axiom-studio/openseal/internal/daemon"
	"github.com/axiom-studio/openseal/internal/server"
	"github.com/axiom-studio/openseal/pkg/models"
	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"go.uber.org/zap"
)

func daemonCmd(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	configPath := fs.String("config", "daemon.yaml", "path to daemon config file")
	contextPath := fs.String("context", "context.yaml", "path to optional standalone Vault and provider context")
	daemonScope := fs.String("scope", "local:default", "durable daemon scope as kind:id")
	desktopOperator := fs.Bool("desktop-operator", false, "enable governed local desktop review and installation")
	standaloneOperator := fs.Bool("standalone-operator", false, "let the loopback TUI act as the local operator")
	listenAddr := fs.String("listen", "", "override the configured API listen address (use 127.0.0.1:0 for an ephemeral port)")
	help := fs.Bool("help", false, "print help for daemon")
	fs.Parse(args)

	if *help {
		fmt.Println(`Usage: openseal daemon [options]

Start the durable OpenSeal agent kernel and versioned API.

Options:
  --config <path>   Path to daemon config file (default: daemon.yaml)
  --context <path>  Optional local Vault/provider context (default: context.yaml)
  --scope <kind:id> Durable daemon scope (default: local:default)
  --listen <addr>   Override the configured API address; port 0 selects a free port
  --standalone-operator Let the loopback TUI act as the local operator
  --desktop-operator Enable local desktop lifecycle authority (loopback and API token required)
  --help            Print this help message`)
		return
	}

	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	sugar := logger.Sugar()
	defer logger.Sync()

	configMissing := false
	if _, statErr := os.Stat(*configPath); os.IsNotExist(statErr) {
		configMissing = true
	}

	cfg, err := daemon.LoadDaemonConfig(*configPath)
	if err != nil {
		sugar.Fatalf("failed to load config: %v", err)
	}

	if configMissing {
		sugar.Infow("auto-created default config", "path", *configPath)
	}
	if strings.TrimSpace(*listenAddr) != "" {
		cfg.API.ListenAddr = strings.TrimSpace(*listenAddr)
	}

	sugar.Infow("daemon config loaded",
		"logLevel", cfg.LogLevel,
		"apiAddr", cfg.API.ListenAddr,
	)

	if !isLoopbackListenAddress(cfg.API.ListenAddr) {
		// The warning has to distinguish the two states. Saying OpenSeal has no
		// authentication is false when OPENSEAL_API_TOKEN is set, and an
		// operator who has correctly armed the token should not be told
		// otherwise on every start.
		warning := "OPENSEAL_API_TOKEN is set, so routes require a bearer token. There is still no authorization model: any holder of the token can call every route. Prefer an authorization-aware reverse proxy."
		if strings.TrimSpace(os.Getenv("OPENSEAL_API_TOKEN")) == "" {
			warning = "OPENSEAL_API_TOKEN is not set, so every route is reachable without a credential. Set it, or expose this only through an authorization-aware reverse proxy or local network boundary."
		}
		sugar.Warnw("OpenSeal API is exposed on non-loopback interface",
			"apiAddr", cfg.API.ListenAddr,
			"warning", warning,
		)
	}

	scope, err := parseDaemonScope(*daemonScope)
	if err != nil {
		sugar.Fatal(err)
	}
	if *standaloneOperator && !isLoopbackListenAddress(cfg.API.ListenAddr) {
		sugar.Fatal("--standalone-operator requires the API listen address to be loopback")
	}
	if *desktopOperator {
		if err := validateDesktopOperator(cfg.API.ListenAddr, os.Getenv("OPENSEAL_API_TOKEN"), scope); err != nil {
			sugar.Fatal(err)
		}
	}
	standaloneContext, err := daemon.LoadStandaloneContext(*contextPath)
	if err != nil {
		sugar.Fatalf("load standalone context: %v", err)
	}

	// One durable store backs the canonical Agent and Team kernel. Interactive
	// clients never own authoritative state.
	store, storagePath, err := daemon.OpenKernelStore(cfg.Storage, filepath.Dir(*configPath))
	if err != nil {
		sugar.Fatalf("failed to open kernel store: %v", err)
	}
	defer store.Close()
	sugar.Infow("durable kernel store opened", "driver", cfg.Storage.Driver, "path", storagePath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Converts the change-set drafts migration 76 kept into builder chats at
	// the first start (D12), seeding the builder where a draft needs it.
	cutoverHost := &daemon.BuilderCutoverHost{}
	engineOptions := []opensealkernel.Option{
		opensealkernel.WithPersistentStore(store),
		opensealkernel.WithAgentBuilderCutoverHost(cutoverHost),
		opensealkernel.WithWorkerConcurrencyLimit(8),
		opensealkernel.WithSkillManagementActions(),
		opensealkernel.WithAgentManagementActions(),
		opensealkernel.WithTeamManagementActions(),
	}
	if *standaloneOperator {
		engineOptions = append(engineOptions,
			opensealkernel.WithActionPolicy(&action.DefaultActionPolicy{Approvers: []models.ApprovalPrincipal{{Type: "user", ID: "local"}}}),
			opensealkernel.WithApprovalAuthorizer(action.EligibleApprovalAuthorizer{}),
		)
	}
	endpoint := strings.TrimSpace(os.Getenv("OPENSEAL_LLM_BASE_URL"))
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("OPENSEAL_LLM_MODEL"))
	if standaloneContext.Authoring != nil {
		endpoint, model = standaloneContext.Authoring.BaseURL, standaloneContext.Authoring.Model
		apiKey, err = standaloneContext.ResolveReference(scope, standaloneContext.Authoring.Credential)
		if err != nil {
			sugar.Fatalf("resolve the model provider credential: %v", err)
		}
	}
	configured := 0
	for _, value := range []string{endpoint, apiKey, model} {
		if value != "" {
			configured++
		}
	}
	if configured != 0 && configured != 3 {
		sugar.Fatal("the model provider requires OPENSEAL_LLM_BASE_URL, OPENAI_API_KEY, and OPENSEAL_LLM_MODEL together")
	}
	contentStore, contentPath, err := daemon.OpenArtifactContentStore(cfg.Storage, filepath.Dir(*configPath))
	if err != nil {
		sugar.Fatalf("failed to open artifact content store: %v", err)
	}
	artifactPublisher := &daemon.TaskArtifactPublisher{Scope: scope, Content: contentStore, Catalog: artifactcatalog.NewCatalog(store)}
	contextTokens, err := standaloneContext.ContextTokens(os.Getenv("OPENSEAL_LLM_CONTEXT_TOKENS"))
	if err != nil {
		sugar.Fatal(err)
	}
	var stepHost *daemon.ProviderStepHost
	if *desktopOperator && configured == 3 {
		stepHost, err = daemon.NewProviderStepHost(daemon.OpenAIProviderConfig{Endpoint: endpoint, Key: apiKey, Model: model, ContextWindowTokens: contextTokens})
		if err != nil {
			sugar.Fatalf("configure desktop task provider: %v", err)
		}
	}
	workerScopes, err := standaloneContext.ListWorkerScopes(ctx)
	if err != nil {
		sugar.Fatalf("resolve standalone context worker scopes: %v", err)
	}
	if stepHost != nil {
		workerScopes = mergeDaemonWorkerScopes(workerScopes, []models.Scope{scope})
	}
	workerScopeSource := workqueue.WorkerScopeSourceFunc(func(context.Context) ([]models.Scope, error) {
		return append([]models.Scope(nil), workerScopes...), nil
	})
	var kernel *opensealkernel.Engine
	resolver := &daemon.DesktopTurnResolver{Scope: scope, Host: stepHost, Publisher: artifactPublisher}
	var turnResolver conversation.TurnRunnerResolver
	if len(workerScopes) > 0 {
		turnResolver = resolver
		engineOptions = append(engineOptions,
			opensealkernel.WithDynamicAgentRunWorkers(runworker.DynamicAgentRunWorkerConfig{Kind: models.RunKindAgentWork, Concurrency: 2, WorkerIDPrefix: "standalone-agent"}, workerScopeSource, turnResolver),
		)
	}
	if stepHost != nil {
		participants := conversation.ParticipantSourceFunc(func(ctx context.Context, query conversation.ParticipantQuery) ([]conversation.ParticipantBinding, error) {
			if kernel == nil {
				return nil, conversation.ErrConversationCoordinationUnavailable
			}
			source, err := daemon.NewDesktopConversationParticipants(kernel, scope, 8)
			if err != nil {
				return nil, err
			}
			return source.ResolveConversationParticipants(ctx, query)
		})
		router, err := daemon.NewDesktopConversationRouter(stepHost)
		if err != nil {
			sugar.Fatalf("configure desktop channel routing: %v", err)
		}
		config := conversation.DefaultConversationCoordinatorConfig()
		config.MaximumParticipants = 8
		localScopes := workqueue.WorkerScopeSourceFunc(func(context.Context) ([]models.Scope, error) { return []models.Scope{scope}, nil })
		engineOptions = append(engineOptions,
			opensealkernel.WithConversationCoordinator(participants, router, config),
			opensealkernel.WithDynamicConversationRuns(opensealkernel.ConversationRunConfig{
				Scheduler: conversationrun.SchedulerConfig{RequireParticipationOptIn: true, Budget: daemon.DesktopConversationBudget()},
				// Each routed member answers in its own agent_work Run through
				// the same Agent turn as a 1:1 chat.
				Runner:     conversationrun.TurnRunnerConfig{RequireParticipationOptIn: true, AgentTurns: turnResolver},
				Workers:    runworker.DynamicAgentRunWorkerConfig{Kind: models.RunKindConversation, Concurrency: 2, WorkerIDPrefix: "desktop-channel"},
				Reconciler: conversationrun.ReconcilerConfig{Interval: 5 * time.Second},
			}, localScopes),
		)
	}
	kernel, err = opensealkernel.New(engineOptions...)
	if err != nil {
		sugar.Fatalf("configure OpenSeal kernel: %v", err)
	}
	cutoverHost.Bind(kernel)
	if err := resolver.Bind(kernel, contentStore, sugar); err != nil {
		sugar.Fatalf("configure the run loop: %v", err)
	}
	// Event routines: no webhooks and no Google sources on the desktop.
	daemon.ConfigureRoutineEvents(kernel)
	// The kernel creates no space and the daemon has no space API, so the
	// local operator's first space, their default, is created here, before the
	// kernel starts work that opens chats for them.
	if _, err := kernel.DefaultSpace(ctx, scope, "local-operator"); errors.Is(err, models.ErrNoDefaultSpace) {
		_, err = kernel.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Home", Look: models.DefaultSpaceLook, OwnerID: "local-operator"})
		if err != nil {
			sugar.Fatalf("create the local operator's space: %v", err)
		}
	} else if err != nil {
		sugar.Fatalf("read the local operator's space: %v", err)
	}
	if err := kernel.Start(ctx); err != nil {
		sugar.Fatalf("start OpenSeal kernel: %v", err)
	}
	// The daemon is the only process, so its dispatcher always leads.
	stopRoutineEvents := daemon.StartRoutineEventDispatcher(ctx, kernel)
	if err := daemon.SeedBuilderAgent(ctx, kernel, scope); err != nil {
		sugar.Fatalf("seed the builder agent: %v", err)
	}

	// Versioned kernel API for the TUI and embedding integrations.
	apiServer := server.NewServer(store, sugar)
	apiServer.SetBearerToken(os.Getenv("OPENSEAL_API_TOKEN"))
	if stepHost != nil {
		apiServer.SetTeamWorkEnabled(true)
		apiServer.SetAgentRunCreationDispatcher(func(ctx context.Context, request models.CreateAgentRunRequest) (*agentrun.CommandResult, error) {
			canonical, err := daemon.DesktopWorkRequest(scope, request)
			if err != nil {
				return nil, err
			}
			if saved, err := agentrun.NewRunCommandService(store).FindCreatedAgentRun(ctx, canonical); err != nil || saved != nil {
				return saved, err
			}
			canonical, err = daemon.ResolveDesktopWork(ctx, kernel, scope, request)
			if err != nil {
				return nil, err
			}
			result, err := kernel.CreateAgentRunCommand(ctx, canonical)
			if err == nil {
				kernel.WakeAgentWorkers()
			}
			return result, err
		})
	}

	if *desktopOperator {
		apiServer.SetActionApprovalAuthorizer(server.DesktopApprovalAuthorizer{Scope: scope})
		apiServer.SetDesktopConversationScope(scope)
		authorize := func(ctx context.Context, channel *models.Conversation) error {
			if stepHost == nil {
				return fmt.Errorf("configure a model provider in Settings before enabling team replies")
			}
			source, err := daemon.NewDesktopConversationParticipants(kernel, scope, 8)
			if err != nil {
				return err
			}
			_, err = source.ResolveConversationParticipants(ctx, conversation.ParticipantQuery{Conversation: channel})
			return err
		}
		var post func(context.Context, conversation.PostChannelMessageRequest) (*models.ChannelMessageCommitResult, error)
		if stepHost != nil {
			post = kernel.PostChannelMessage
		}
		apiServer.SetChannelParticipation(authorize, post)
	}
	apiServer.SetReactionProjector(kernel.ReactionProjector())
	if *standaloneOperator {
		if !*desktopOperator {
			apiServer.SetActionApprovalAuthorizer(action.EligibleApprovalAuthorizer{})
		}
	}
	apiServer.SetArtifactContentStore(contentStore)
	sugar.Infow("artifact content store opened", "path", contentPath)
	listener, err := net.Listen("tcp", cfg.API.ListenAddr)
	if err != nil {
		sugar.Fatalf("bind API server: %v", err)
	}
	ready := struct {
		Type     string `json:"type"`
		Endpoint string `json:"endpoint"`
	}{Type: "ready", Endpoint: "http://" + listener.Addr().String()}
	readyJSON, err := json.Marshal(ready)
	if err != nil {
		sugar.Fatalf("encode API readiness: %v", err)
	}
	fmt.Printf("OPENSEAL_DAEMON %s\n", readyJSON)

	go func() {
		if err := apiServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			sugar.Errorw("API server error", "error", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	sugar.Infow("shutting down", "signal", sig)

	stopRoutineEvents()
	kernel.Close()
	apiServer.Shutdown(ctx)
}

func mergeDaemonWorkerScopes(groups ...[]models.Scope) []models.Scope {
	seen := map[string]models.Scope{}
	for _, group := range groups {
		for _, scope := range group {
			seen[scope.Kind+"\x00"+scope.ID] = scope
		}
	}
	result := make([]models.Scope, 0, len(seen))
	for _, scope := range seen {
		result = append(result, scope)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Kind+"\x00"+result[i].ID < result[j].Kind+"\x00"+result[j].ID
	})
	return result
}

func parseDaemonScope(value string) (models.Scope, error) {
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) != 2 {
		return models.Scope{}, fmt.Errorf("--scope must use kind:id format")
	}
	scope := models.Scope{Kind: strings.TrimSpace(parts[0]), ID: strings.TrimSpace(parts[1])}
	if err := scope.Validate(); err != nil {
		return models.Scope{}, fmt.Errorf("--scope: %w", err)
	}
	return scope, nil
}

func isLoopbackListenAddress(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// Desktop lifecycle authority is an explicit local-owner mode, never a fallback
// for a publicly reachable or unauthenticated API.
func validateDesktopOperator(listen, token string, scope models.Scope) error {
	if !isLoopbackListenAddress(listen) {
		return fmt.Errorf("--desktop-operator requires a loopback API listen address")
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("--desktop-operator requires OPENSEAL_API_TOKEN")
	}
	if scope.Kind != "local" || strings.TrimSpace(scope.ID) == "" {
		return fmt.Errorf("--desktop-operator requires a local workspace scope")
	}
	return nil
}
