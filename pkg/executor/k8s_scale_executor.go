package executor

import (
	"context"
	"fmt"
)

type K8sScaleExecutor struct {
	cortexClient CortexK8sClient
}

func NewK8sScaleExecutor(cortexClient CortexK8sClient) *K8sScaleExecutor {
	return &K8sScaleExecutor{
		cortexClient: cortexClient,
	}
}

func (e *K8sScaleExecutor) Type() string {
	return NodeTypeK8sScale
}

func (e *K8sScaleExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, wrapK8sError("k8s-scale", fmt.Errorf("config is required"))
	}

	kind, err := extractKind(config)
	if err != nil {
		return nil, wrapK8sError("k8s-scale", err)
	}

	if kind != "Deployment" && kind != "StatefulSet" && kind != "ReplicaSet" {
		return nil, wrapK8sError("k8s-scale", fmt.Errorf("kind must be Deployment, StatefulSet, or ReplicaSet"))
	}

	name, namespace, err := extractNameNamespace(config, resolver)
	if err != nil {
		return nil, wrapK8sError("k8s-scale", err)
	}

	replicas, ok := config["replicas"].(float64)
	if !ok {
		return nil, wrapK8sError("k8s-scale", fmt.Errorf("'replicas' is required"))
	}

	clusterId := extractClusterId(config)

	// Build patch to scale replicas
	patchMap := map[string]interface{}{
		"spec": map[string]interface{}{
			"replicas": int(replicas),
		},
	}

	// Use Cortex API to update resource - works for any cluster Cortex manages
	_, err = e.cortexClient.UpdateResource(ctx, clusterId, namespace, name, kind, patchMap)
	if err != nil {
		return nil, wrapK8sError("k8s-scale", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"success":  true,
			"message":  fmt.Sprintf("Scaled to %d replicas", int(replicas)),
			"name":     name,
			"kind":     kind,
			"replicas": int(replicas),
		},
	}, nil
}
