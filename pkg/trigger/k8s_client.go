package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

type K8sEventClient struct {
	baseURL    string
	httpClient *http.Client
	logger     *zap.SugaredLogger
}

func NewK8sEventClient(baseURL string, logger *zap.SugaredLogger) *K8sEventClient {
	return &K8sEventClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}
}

type K8sResourceIdentifier struct {
	Name             string           `json:"name"`
	Namespace        string           `json:"namespace"`
	GroupVersionKind GroupVersionKind `json:"groupVersionKind"`
}

type GroupVersionKind struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

type K8sRequestBean struct {
	ResourceIdentifier K8sResourceIdentifier `json:"resourceIdentifier"`
}

type ResourceRequestBean struct {
	ClusterId  int             `json:"clusterId"`
	K8sRequest *K8sRequestBean `json:"k8sRequest"`
}

func (c *K8sEventClient) ListEvents(ctx context.Context, clusterId int, namespace string) ([]map[string]interface{}, error) {
	request := &ResourceRequestBean{
		ClusterId: clusterId,
		K8sRequest: &K8sRequestBean{
			ResourceIdentifier: K8sResourceIdentifier{
				Name:      "",
				Namespace: namespace,
				GroupVersionKind: GroupVersionKind{
					Group:   "",
					Version: "v1",
					Kind:    "Event",
				},
			},
		},
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/orchestrator/agent/internal/k8s/events", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call Cortex API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Cortex API error (status %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Events struct {
			Items []map[string]interface{} `json:"items"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Events.Items, nil
}

func (c *K8sEventClient) ListResources(ctx context.Context, clusterId int, namespace, resource, labelSelector, fieldSelector string) ([]map[string]interface{}, error) {
	gvk, err := resourceToGVK(resource)
	if err != nil {
		return nil, err
	}

	request := &ResourceRequestBean{
		ClusterId: clusterId,
		K8sRequest: &K8sRequestBean{
			ResourceIdentifier: K8sResourceIdentifier{
				Name:             "",
				Namespace:        namespace,
				GroupVersionKind: gvk,
			},
		},
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/orchestrator/agent/internal/k8s/resource/list", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call Cortex API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Cortex API error (status %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Resources struct {
			Items []map[string]interface{} `json:"items"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Resources.Items, nil
}

func resourceToGVK(resource string) (GroupVersionKind, error) {
	gvkMap := map[string]GroupVersionKind{
		"pods":                     {Group: "", Version: "v1", Kind: "Pod"},
		"services":                 {Group: "", Version: "v1", Kind: "Service"},
		"configmaps":               {Group: "", Version: "v1", Kind: "ConfigMap"},
		"secrets":                  {Group: "", Version: "v1", Kind: "Secret"},
		"nodes":                    {Group: "", Version: "v1", Kind: "Node"},
		"persistentvolumeclaims":   {Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
		"persistentvolumes":        {Group: "", Version: "v1", Kind: "PersistentVolume"},
		"deployments":              {Group: "apps", Version: "v1", Kind: "Deployment"},
		"statefulsets":             {Group: "apps", Version: "v1", Kind: "StatefulSet"},
		"daemonsets":               {Group: "apps", Version: "v1", Kind: "DaemonSet"},
		"replicasets":              {Group: "apps", Version: "v1", Kind: "ReplicaSet"},
		"jobs":                     {Group: "batch", Version: "v1", Kind: "Job"},
		"cronjobs":                 {Group: "batch", Version: "v1", Kind: "CronJob"},
		"ingresses":                {Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"},
		"networkpolicies":          {Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"},
		"horizontalpodautoscalers": {Group: "autoscaling", Version: "v2", Kind: "HorizontalPodAutoscaler"},
	}

	gvk, ok := gvkMap[resource]
	if !ok {
		return GroupVersionKind{}, fmt.Errorf("unsupported resource type: %s", resource)
	}
	return gvk, nil
}
