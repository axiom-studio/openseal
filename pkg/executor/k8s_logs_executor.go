package executor

import (
	"context"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/module"
)

type K8sLogsExecutor struct {
	k8sClient K8sClient
}

func NewK8sLogsExecutor(k8sClient K8sClient) *K8sLogsExecutor {
	return &K8sLogsExecutor{
		k8sClient: k8sClient,
	}
}

func (e *K8sLogsExecutor) Type() string {
	return NodeTypeK8sLogs
}

func (e *K8sLogsExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, wrapK8sError("k8s-logs", fmt.Errorf("config is required"))
	}

	podNameTemplate, ok := config["podName"].(string)
	if !ok || podNameTemplate == "" {
		return nil, wrapK8sError("k8s-logs", fmt.Errorf("'podName' is required"))
	}
	podName := resolver.ResolveString(podNameTemplate)

	namespace := ""
	if ns, ok := config["namespace"].(string); ok && ns != "" {
		namespace = resolver.ResolveString(ns)
	}
	if namespace == "" {
		namespace = module.GetAgentsNamespace()
	}

	container := ""
	if c, ok := config["container"].(string); ok && c != "" {
		container = resolver.ResolveString(c)
	}

	clusterId := extractClusterId(config)

	tailLines := 0
	if tl, ok := config["tailLines"].(float64); ok && tl > 0 {
		tailLines = int(tl)
	}

	sinceSeconds := 0
	if ss, ok := config["sinceSeconds"].(float64); ok && ss > 0 {
		sinceSeconds = int(ss)
	}

	if e.k8sClient == nil {
		return nil, wrapK8sError("k8s-logs", fmt.Errorf("k8s client not configured"))
	}

	logs, err := e.k8sClient.GetPodLogs(ctx, clusterId, namespace, podName, container, tailLines, sinceSeconds)
	if err != nil {
		return nil, wrapK8sError("k8s-logs", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"logs":      logs,
			"container": container,
			"pod":       podName,
		},
	}, nil
}
