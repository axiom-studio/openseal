package executor

import (
	"context"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/module"
	"k8s.io/client-go/kubernetes"
)

// Node type constants
const (
	// Trigger types
	NodeTypeWebhook  = "webhook"
	NodeTypeCron     = "cron"
	NodeTypeManual   = "manual"
	NodeTypeK8sEvent = "k8s-event"
	NodeTypeK8sWatch = "k8s-watch"

	// Control flow types
	NodeTypeIf     = "if"
	NodeTypeSwitch = "switch"
	NodeTypeDelay  = "delay"

	// Action types
	NodeTypeHTTP     = "http"
	NodeTypeCode     = "code"
	NodeTypeAI       = "ai"
	NodeTypePGVector = "pgvector"

	// K8s action types
	NodeTypeK8sGet     = "k8s-get"
	NodeTypeK8sList    = "k8s-list"
	NodeTypeK8sLogs    = "k8s-logs"
	NodeTypeK8sEvents  = "k8s-events"
	NodeTypeK8sRestart = "k8s-restart"
	NodeTypeK8sScale   = "k8s-scale"
	NodeTypeK8sPatch   = "k8s-patch"
	NodeTypeK8sDelete       = "k8s-delete"
	NodeTypeSkillContainer  = "skill-container"

	// Data types
	NodeTypeTransform = "transform"
	NodeTypeSet       = "set"
	NodeTypeMerge     = "merge"
	NodeTypeFilter    = "filter"
)

// Step type constants (legacy aliases)
const (
	StepTypeIf        = "if"
	StepTypeSwitch    = "switch"
	StepTypeTransform = "transform"
	StepTypeSet       = "set"
	StepTypeMerge     = "merge"
	StepTypeDelay     = "delay"
	StepTypeCode      = "code"
	StepTypeHTTP      = "http"
	StepTypeAI        = "ai"
)

// Registry holds all registered step executors
type Registry struct {
	executors map[string]StepExecutor
}

// NewRegistry creates a new executor registry
// DEPRECATED: Use NewEmptyRegistry() and load skills via skill.PluginLoader instead
// For backward compatibility, this still auto-registers all executors
// For K8s executors, pass a K8sClient to enable K8s operations.
// OpenSeal provides a direct-K8s implementation; the platform injects a remote proxy.
func NewRegistry(k8sClient K8sClient) *Registry {
	r := &Registry{
		executors: make(map[string]StepExecutor),
	}

	// Register in-process executors
	r.Register(&IfExecutor{})
	r.Register(&SwitchExecutor{})
	r.Register(&TransformExecutor{})
	r.Register(&SetExecutor{})
	r.Register(&MergeExecutor{})
	r.Register(&DelayExecutor{})
	r.Register(NewFilterExecutor())
	r.Register(NewSortExecutor())
	r.Register(NewAggregateExecutor())
	r.Register(NewSplitExecutor())
	r.Register(NewJoinExecutor())
	r.Register(NewLoopExecutor())
	r.Register(NewWebhookResponseExecutor())

	// Register communication executors
	r.Register(NewSlackExecutor())
	r.Register(NewDiscordExecutor())
	r.Register(NewTeamsExecutor())
	r.Register(NewEmailExecutor())

	// Register HTTP executor
	r.Register(NewHTTPExecutor())

	// Register AI executor
	r.Register(NewAIExecutor())

	// Register PGVector executor
	r.Register(NewPGVectorExecutor())

	// Register K8s executors when a K8sClient is provided
	// OpenSeal provides a direct-K8s implementation for standalone mode.
	// The platform injects a remote proxy that delegates to the upstream API.
	if k8sClient != nil {
		r.Register(NewK8sGetExecutor(k8sClient))
		r.Register(NewK8sListExecutor(k8sClient))
		r.Register(NewK8sLogsExecutor(k8sClient))
		r.Register(NewK8sEventsExecutor(k8sClient))
		r.Register(NewK8sRestartExecutor(k8sClient))
		r.Register(NewK8sScaleExecutor(k8sClient))
		r.Register(NewK8sPatchExecutor(k8sClient))
		r.Register(NewK8sDeleteExecutor(k8sClient))
	}

	// Register code executor if k8s is available
	if k8sClient, err := module.GetK8sClient(); err == nil && k8sClient != nil {
		r.RegisterCodeExecutor(k8sClient, &CodeExecutorConfig{
			Namespace:               module.GetAgentsNamespace(),
			RunnerImage:             module.GetCodeExecutorImage(),
			ServiceAccount:          "default",
			TTLSecondsAfterFinished: module.GetJobTTLSecondsAfterFinished(),
		})

		// Register openclaw executor
		r.Register(NewOpenClawExecutor(k8sClient, module.GetAgentsNamespace(), ""))
	}

	return r
}

// NewEmptyRegistry creates a new executor registry with no executors registered
// Use this when loading executors from skills via skill.PluginLoader
func NewEmptyRegistry() *Registry {
	return &Registry{
		executors: make(map[string]StepExecutor),
	}
}

// RegisterCodeExecutor adds the code executor with K8s client
func (r *Registry) RegisterCodeExecutor(k8sClient interface{}, config *CodeExecutorConfig) {
	// Import kubernetes client at runtime to avoid import cycle
	// The caller should pass a kubernetes.Interface
	if k8sClient == nil {
		return
	}

	// Use type assertion - the caller must pass kubernetes.Interface
	// This is done this way to avoid importing kubernetes in the base registry
	r.executors[StepTypeCode] = &codeExecutorWrapper{
		k8sClient: k8sClient,
		config:    config,
	}
}

// codeExecutorWrapper wraps the code executor to defer K8s client usage
type codeExecutorWrapper struct {
	k8sClient        interface{}
	config           *CodeExecutorConfig
	executor         *CodeExecutor
	contextUploader  ContextUploader
	tokenService     *ExecutionTokenService
	contextStore     *ToolExecutionContextStore
	toolRegistry     *ToolRegistry
	toolProxyBaseURL string
	sdkConfigMapName string
}

func (w *codeExecutorWrapper) Type() string {
	return StepTypeCode
}

func (w *codeExecutorWrapper) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	if err := w.ensureExecutor(); err != nil {
		return nil, err
	}
	return w.executor.Execute(ctx, step, resolver)
}

func (w *codeExecutorWrapper) ensureExecutor() error {
	if w.executor == nil {
		k8sClient, ok := w.k8sClient.(kubernetes.Interface)
		if !ok {
			return fmt.Errorf("code executor requires kubernetes.Interface client")
		}
		w.executor = NewCodeExecutor(k8sClient, w.config)
		if w.contextUploader != nil {
			w.executor.SetContextUploader(w.contextUploader)
		}
		// Set tool dependencies if configured
		if w.tokenService != nil && w.contextStore != nil && w.toolRegistry != nil {
			w.executor.SetToolDependencies(w.tokenService, w.contextStore, w.toolRegistry, w.toolProxyBaseURL, w.sdkConfigMapName)
		}
	}
	return nil
}

func (w *codeExecutorWrapper) SupportsStreaming(config map[string]interface{}) bool {
	if err := w.ensureExecutor(); err != nil {
		return false
	}
	return w.executor.SupportsStreaming(config)
}

func (w *codeExecutorWrapper) ExecuteStreaming(ctx context.Context, step *StepDefinition, resolver TemplateResolver, onStream StreamCallback) (*StepResult, error) {
	if err := w.ensureExecutor(); err != nil {
		return nil, err
	}
	return w.executor.ExecuteStreaming(ctx, step, resolver, onStream)
}

func (w *codeExecutorWrapper) SupportsStreamingOutput(config map[string]interface{}) bool {
	if err := w.ensureExecutor(); err != nil {
		return false
	}
	return w.executor.SupportsStreamingOutput(config)
}

// SetContextUploader sets the context uploader for the code executor
func (r *Registry) SetContextUploader(uploader ContextUploader) {
	if executor, ok := r.executors[StepTypeCode]; ok {
		if wrapper, ok := executor.(*codeExecutorWrapper); ok {
			wrapper.contextUploader = uploader
			// If already instantiated, set it directly
			if wrapper.executor != nil {
				wrapper.executor.SetContextUploader(uploader)
			}
		}
	}
}

// SetToolDependencies sets the tool dependencies for the code executor
func (r *Registry) SetToolDependencies(tokenService *ExecutionTokenService, contextStore *ToolExecutionContextStore, toolRegistry *ToolRegistry, toolProxyBaseURL string, sdkConfigMapName string) {
	if executor, ok := r.executors[StepTypeCode]; ok {
		if wrapper, ok := executor.(*codeExecutorWrapper); ok {
			// Store dependencies in wrapper for lazy initialization
			wrapper.tokenService = tokenService
			wrapper.contextStore = contextStore
			wrapper.toolRegistry = toolRegistry
			wrapper.toolProxyBaseURL = toolProxyBaseURL
			wrapper.sdkConfigMapName = sdkConfigMapName

			// If already instantiated, set it directly
			if wrapper.executor != nil {
				wrapper.executor.SetToolDependencies(tokenService, contextStore, toolRegistry, toolProxyBaseURL, sdkConfigMapName)
			}
		}
	}
}

// globalRegistry is a package-level registry for self-registering executors
var globalRegistry = NewEmptyRegistry()

// Register adds an executor to the global registry by type name.
// Used by self-registering executors in their init() functions.
func Register(name string, executor StepExecutor) {
	globalRegistry.executors[name] = executor
}

// GlobalRegistry returns the package-level registry containing all self-registered executors.
func GlobalRegistry() *Registry {
	return globalRegistry
}

// Register adds an executor to the registry
func (r *Registry) Register(executor StepExecutor) {
	r.executors[executor.Type()] = executor
}

func (r *Registry) Unregister(stepType string) {
	delete(r.executors, stepType)
}

// Get returns the executor for a given step type
func (r *Registry) Get(stepType string) (StepExecutor, error) {
	executor, ok := r.executors[stepType]
	if !ok {
		return nil, fmt.Errorf("no executor registered for step type: %s", stepType)
	}
	return executor, nil
}

// HasExecutor checks if an executor exists for the given type
func (r *Registry) HasExecutor(stepType string) bool {
	_, ok := r.executors[stepType]
	return ok
}

// IsInProcess returns true if the step type is executed in-process (not dispatched)
func (r *Registry) IsInProcess(stepType string) bool {
	switch stepType {
	case StepTypeIf, StepTypeSwitch, StepTypeTransform,
		StepTypeSet, StepTypeMerge, StepTypeDelay, NodeTypeFilter, NodeTypeSort, NodeTypeAggregate,
		NodeTypeSplit, NodeTypeJoin, NodeTypeLoop:
		return true
	default:
		return false
	}
}
