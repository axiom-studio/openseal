// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/runworker. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/runworker, kept so that importers of
// pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"go.uber.org/zap"
)

type ActionProposalObserverFunc = runworker.ActionProposalObserverFunc

type AgentRunWorkerConfig = runworker.AgentRunWorkerConfig

type AgentRunWorkerPool = runworker.AgentRunWorkerPool

// NewAgentRunWorkerPool forwards to runworker.NewAgentRunWorkerPool.
func NewAgentRunWorkerPool(store models.KernelStore, resolver conversation.TurnRunnerResolver, logger *zap.SugaredLogger, config runworker.AgentRunWorkerConfig, opts ...models.ServiceOption) (*runworker.AgentRunWorkerPool, error) {
	return runworker.NewAgentRunWorkerPool(store, resolver, logger, config, opts...)
}

type DynamicAgentRunWorkerConfig = runworker.DynamicAgentRunWorkerConfig

type AgentRunWorkerSupervisor = runworker.AgentRunWorkerSupervisor

// NewAgentRunWorkerSupervisor forwards to runworker.NewAgentRunWorkerSupervisor.
func NewAgentRunWorkerSupervisor(store models.KernelStore, resolver conversation.TurnRunnerResolver, source workqueue.WorkerScopeSource, logger *zap.SugaredLogger, config runworker.DynamicAgentRunWorkerConfig) (*runworker.AgentRunWorkerSupervisor, error) {
	return runworker.NewAgentRunWorkerSupervisor(store, resolver, source, logger, config)
}

type LoopBindingConfig = runworker.LoopBindingConfig

type SkillHostCapabilityResolver = runworker.SkillHostCapabilityResolver

type SkillHostCapabilityResolverFunc = runworker.SkillHostCapabilityResolverFunc

// ResolveLoopBinding forwards to runworker.ResolveLoopBinding.
func ResolveLoopBinding(ctx context.Context, catalog conversationrun.AgentTurnCatalog, run *models.AgentRun, config runworker.LoopBindingConfig) (*agentrun.TurnRunnerBinding, error) {
	return runworker.ResolveLoopBinding(ctx, catalog, run, config)
}
