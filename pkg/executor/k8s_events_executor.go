package executor

import (
	"context"
	"fmt"
)

type K8sEventsExecutor struct {
	k8sClient K8sClient
}

func NewK8sEventsExecutor(k8sClient K8sClient) *K8sEventsExecutor {
	return &K8sEventsExecutor{
		k8sClient: k8sClient,
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

	if e.k8sClient == nil {
		return nil, wrapK8sError("k8s-events", fmt.Errorf("k8s client not configured"))
	}

	events, err := e.k8sClient.ListEvents(ctx, clusterId, namespace, kind, name)
	if err != nil {
		return nil, wrapK8sError("k8s-events", err)
	}
	events = nonNilK8sItems(events)

	return &StepResult{
		Output: map[string]interface{}{
			"items": events,
			"count": len(events),
		},
	}, nil
}
