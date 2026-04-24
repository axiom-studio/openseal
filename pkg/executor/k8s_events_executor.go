package executor

import (
	"context"
	"fmt"
)

type K8sEventsExecutor struct {
	cortexClient CortexK8sClient
}

func NewK8sEventsExecutor(cortexClient CortexK8sClient) *K8sEventsExecutor {
	return &K8sEventsExecutor{
		cortexClient: cortexClient,
	}
}

func (e *K8sEventsExecutor) Type() string {
	return NodeTypeK8sEvents
}

func (e *K8sEventsExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, wrapK8sError("k8s-events", fmt.Errorf("config is required"))
	}

	namespace := ""
	if ns, ok := config["namespace"].(string); ok && ns != "" {
		namespace = resolver.ResolveString(ns)
	}

	kind := ""
	name := ""
	if k, ok := config["kind"].(string); ok && k != "" {
		kind = k
		if n, ok := config["name"].(string); ok && n != "" {
			name = resolver.ResolveString(n)
		}
	}

	clusterId := extractClusterId(config)

	// Use Cortex API to list events - works for any cluster Cortex manages
	events, err := e.cortexClient.ListEvents(ctx, clusterId, namespace, kind, name)
	if err != nil {
		return nil, wrapK8sError("k8s-events", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"items": events,
			"count": len(events),
		},
	}, nil
}
