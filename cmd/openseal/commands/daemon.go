package commands

import (
	"context"
	"encoding/json"
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

	"github.com/axiom-studio/openseal/internal/daemon"
	"github.com/axiom-studio/openseal/internal/server"
	"github.com/axiom-studio/openseal/pkg/authoring"
	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/outreach"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	"go.uber.org/zap"
)

func daemonCmd(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	configPath := fs.String("config", "daemon.yaml", "path to daemon config file")
	contextPath := fs.String("context", "context.yaml", "path to optional standalone Vault and provider context")
	authoringScope := fs.String("scope", "local:default", "standalone authoring scope as kind:id")
	standaloneOperator := fs.Bool("standalone-operator", false, "allow the loopback TUI to retry failed generation as local-operator")
	help := fs.Bool("help", false, "print help for daemon")
	fs.Parse(args)

	if *help {
		fmt.Println(`Usage: openseal daemon [options]

Start the durable OpenSeal agent kernel and versioned API.

Options:
  --config <path>   Path to daemon config file (default: daemon.yaml)
  --context <path>  Optional local Vault/provider context (default: context.yaml)
  --scope <kind:id> Durable standalone authoring scope (default: local:default)
  --standalone-operator Allow governed generation retry when API binds to loopback
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

	sugar.Infow("daemon config loaded",
		"logLevel", cfg.LogLevel,
		"apiAddr", cfg.API.ListenAddr,
	)

	if !isLoopbackListenAddress(cfg.API.ListenAddr) {
		sugar.Warnw("OpenSeal API is exposed on non-loopback interface",
			"apiAddr", cfg.API.ListenAddr,
			"warning", "OpenSeal has no built-in API authentication. Expose it only through an authorization-aware reverse proxy or local network boundary.",
		)
	}

	scope, err := parseDaemonScope(*authoringScope)
	if err != nil {
		sugar.Fatal(err)
	}
	if *standaloneOperator && !isLoopbackListenAddress(cfg.API.ListenAddr) {
		sugar.Fatal("--standalone-operator requires the API listen address to be loopback")
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

	policyCatalog, err := daemon.NewSourcePolicyCatalog(cfg.SourcePolicies)
	if err != nil {
		sugar.Fatalf("configure source policies: %v", err)
	}
	skillsDir := strings.TrimSpace(os.Getenv("OPENSEAL_SKILLS_DIR"))
	if skillsDir == "" {
		skillsDir = filepath.Join(filepath.Dir(*configPath), "skills")
	}
	engineOptions := []opensealkernel.Option{
		opensealkernel.WithPersistentStore(store),
		opensealkernel.WithWorkerConcurrencyLimit(8),
		opensealkernel.WithSkillManagementActions(),
		opensealkernel.WithAgentManagementActions(),
		opensealkernel.WithTeamManagementActions(),
		opensealkernel.WithClawHubRegistrySkillsDirectory(clawhub.RegistryURL, clawhub.NewClawHubClient(""), skillsDir, skillsDir),
		opensealkernel.WithClawHubSourceArtifactScope(skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}),
	}
	if *standaloneOperator {
		engineOptions = append(engineOptions,
			opensealkernel.WithActionPolicy(&runtime.DefaultActionPolicy{Approvers: []runtime.ApprovalPrincipal{{Type: "user", ID: "local"}}}),
			opensealkernel.WithApprovalAuthorizer(runtime.EligibleApprovalAuthorizer{}),
		)
	}
	workerScopes, err := policyCatalog.ListWorkerScopes(ctx)
	if err != nil {
		sugar.Fatalf("resolve source-policy worker scopes: %v", err)
	}
	credentialScopes, err := standaloneContext.ListWorkerScopes(ctx)
	if err != nil {
		sugar.Fatalf("resolve standalone context worker scopes: %v", err)
	}
	workerScopes = mergeDaemonWorkerScopes(workerScopes, credentialScopes)
	workerScopeSource := runtime.WorkerScopeSourceFunc(func(context.Context) ([]runtime.Scope, error) {
		return append([]runtime.Scope(nil), workerScopes...), nil
	})
	var kernel *opensealkernel.Engine
	if len(workerScopes) > 0 {
		turnResolver := runtime.TurnRunnerResolverFunc(func(resolveCtx context.Context, run *runtime.AgentRun) (*runtime.TurnRunnerBinding, error) {
			if kernel == nil {
				return nil, runtime.ErrTurnHostUnavailable
			}
			return runtime.ResolveCatalogTurnRunner(resolveCtx, kernel, run, runtime.CatalogTurnResolverConfig{})
		})
		authorizer := outreach.InvocationAuthorizerFunc(func(authorizeCtx context.Context, invocation runtime.ToolInvocation) (*outreach.InvocationAuthorization, error) {
			if kernel == nil {
				return nil, runtime.ErrTurnHostUnavailable
			}
			canonical, authorizeErr := outreach.NewCanonicalInvocationAuthorizer(kernel, policyCatalog)
			if authorizeErr != nil {
				return nil, authorizeErr
			}
			return canonical.AuthorizeOutreachInvocation(authorizeCtx, invocation)
		})
		invoker, invokerErr := outreach.NewWebhookInvoker(authorizer)
		if invokerErr != nil {
			sugar.Fatalf("configure governed outreach transport: %v", invokerErr)
		}
		dispatcher, dispatcherErr := runtime.NewToolActionDispatcher(invoker)
		if dispatcherErr != nil {
			sugar.Fatalf("configure governed action dispatcher: %v", dispatcherErr)
		}
		engineOptions = append(engineOptions,
			opensealkernel.WithDynamicAgentRunWorkers(runtime.DynamicAgentRunWorkerConfig{Kind: runtime.RunKindAgentWork, Concurrency: 2, MaxTurnsPerClaim: 1, WorkerIDPrefix: "standalone-agent"}, workerScopeSource, turnResolver),
			opensealkernel.WithDynamicActionWorkers(runtime.DynamicActionWorkerConfig{Concurrency: 2, WorkerIDPrefix: "standalone-action"}, workerScopeSource, standaloneContext, dispatcher),
		)
	}
	kernel, err = opensealkernel.New(engineOptions...)
	if err != nil {
		sugar.Fatalf("configure OpenSeal kernel: %v", err)
	}
	if err := ensureCanonicalSkill(ctx, kernel, outreach.SkillDefinition()); err != nil {
		sugar.Fatalf("register governed outreach Skill: %v", err)
	}
	kernel.Start(ctx)

	// Versioned kernel API for the TUI and embedding integrations.
	apiServer := server.NewServer(store, sugar)
	apiServer.SetClawHubLifecycle(kernel, *standaloneOperator)
	apiServer.SetWorkforceCredentialBindings(standaloneContext.CredentialChoices(scope))
	if *standaloneOperator {
		apiServer.SetActionApprovalAuthorizer(runtime.EligibleApprovalAuthorizer{})
		if len(workerScopes) > 0 {
			apiServer.SetOutreachDeliveryDispatcher(func(dispatchCtx context.Context, request runtime.CreateAgentRunRequest) (*runtime.AgentRunCommandResult, error) {
				if !policyCatalog.OutreachEnabled(request.Scope) {
					return nil, fmt.Errorf("governed outreach workers are not configured for scope %s:%s", request.Scope.Kind, request.Scope.ID)
				}
				result, dispatchErr := kernel.CreateAgentRunCommand(dispatchCtx, request)
				if dispatchErr == nil {
					kernel.WakeAgentWorkers()
				}
				return result, dispatchErr
			})
		}
	}
	contentStore, contentPath, err := daemon.OpenArtifactContentStore(cfg.Storage, filepath.Dir(*configPath))
	if err != nil {
		sugar.Fatalf("failed to open artifact content store: %v", err)
	}
	apiServer.SetArtifactContentStore(contentStore)
	sugar.Infow("artifact content store opened", "path", contentPath)
	endpoint := strings.TrimSpace(os.Getenv("OPENSEAL_LLM_BASE_URL"))
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("OPENSEAL_LLM_MODEL"))
	if standaloneContext.Authoring != nil {
		endpoint, model = standaloneContext.Authoring.BaseURL, standaloneContext.Authoring.Model
		apiKey, err = standaloneContext.ResolveReference(scope, standaloneContext.Authoring.Credential)
		if err != nil {
			sugar.Fatalf("resolve standalone authoring credential: %v", err)
		}
	}
	configured := 0
	for _, value := range []string{endpoint, apiKey, model} {
		if value != "" {
			configured++
		}
	}
	if configured != 0 && configured != 3 {
		sugar.Fatal("workforce authoring requires OPENSEAL_LLM_BASE_URL, OPENAI_API_KEY, and OPENSEAL_LLM_MODEL together")
	}
	if configured == 3 {
		generator, generatorErr := authoring.NewOpenAICompatibleGenerator(endpoint, apiKey, model, nil)
		if generatorErr != nil {
			sugar.Fatalf("configure workforce authoring: %v", generatorErr)
		}
		compiler, compilerErr := authoring.NewCompiler(generator)
		if compilerErr != nil {
			sugar.Fatalf("configure workforce authoring compiler: %v", compilerErr)
		}
		apiServer.SetWorkforceAuthoringCompiler(compiler)
		if workerErr := apiServer.StartWorkforceAuthoringWorker(ctx, scope, ""); workerErr != nil {
			sugar.Fatalf("start workforce authoring worker: %v", workerErr)
		}
		if *standaloneOperator {
			apiServer.SetWorkforceLifecycleAuthorizer(server.StandaloneRetryAuthorizer{ActorID: "local-operator"})
		} else {
			sugar.Warn("generation retry is disabled; use --standalone-operator with a loopback API address or configure an embedding-host lifecycle authority")
		}
		sugar.Infow("workforce authoring enabled", "model", model, "scope", scope.Kind+":"+scope.ID)
	}

	go func() {
		if err := apiServer.ListenAndServe(cfg.API.ListenAddr); err != nil && err != http.ErrServerClosed {
			sugar.Errorw("API server error", "error", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	sugar.Infow("shutting down", "signal", sig)

	kernel.Stop()
	apiServer.Shutdown(ctx)
}

func mergeDaemonWorkerScopes(groups ...[]runtime.Scope) []runtime.Scope {
	seen := map[string]runtime.Scope{}
	for _, group := range groups {
		for _, scope := range group {
			seen[scope.Kind+"\x00"+scope.ID] = scope
		}
	}
	result := make([]runtime.Scope, 0, len(seen))
	for _, scope := range seen {
		result = append(result, scope)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Kind+"\x00"+result[i].ID < result[j].Kind+"\x00"+result[j].ID
	})
	return result
}

func parseDaemonScope(value string) (runtime.Scope, error) {
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) != 2 {
		return runtime.Scope{}, fmt.Errorf("--scope must use kind:id format")
	}
	scope := runtime.Scope{Kind: strings.TrimSpace(parts[0]), ID: strings.TrimSpace(parts[1])}
	if err := scope.Validate(); err != nil {
		return runtime.Scope{}, fmt.Errorf("--scope: %w", err)
	}
	return scope, nil
}

func ensureCanonicalSkill(ctx context.Context, kernel *opensealkernel.Engine, definition *skill.Definition) error {
	if kernel == nil || definition == nil {
		return fmt.Errorf("kernel and canonical Skill definition are required")
	}
	existing, err := kernel.GetSkillDefinition(ctx, definition.ID, definition.Version)
	if err != nil {
		return err
	}
	if existing == nil {
		return kernel.RegisterSkill(ctx, definition)
	}
	existingJSON, existingErr := json.Marshal(existing)
	definitionJSON, definitionErr := json.Marshal(definition)
	if existingErr != nil || definitionErr != nil || string(existingJSON) != string(definitionJSON) {
		return fmt.Errorf("persisted Skill %s@%s does not match the canonical definition", definition.ID, definition.Version)
	}
	return nil
}

func isLoopbackListenAddress(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
