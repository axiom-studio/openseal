package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"go.uber.org/zap"
)

type CortexK8sClient struct {
	baseURL    string
	httpClient *http.Client
	logger     *zap.SugaredLogger
}

func NewCortexK8sClient(baseURL string, logger *zap.SugaredLogger) *CortexK8sClient {
	return &CortexK8sClient{
		baseURL:    baseURL,
		httpClient: &http.Client{},
		logger:     logger,
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

type ResourceResponse struct {
	Manifest map[string]interface{} `json:"manifest"`
}

func (c *CortexK8sClient) GetResource(ctx context.Context, clusterId int, namespace, name, kind string) (map[string]interface{}, error) {
	gvk, err := parseKind(kind)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kind: %w", err)
	}

	request := &ResourceRequestBean{
		ClusterId: clusterId,
		K8sRequest: &K8sRequestBean{
			ResourceIdentifier: K8sResourceIdentifier{
				Name:             name,
				Namespace:        namespace,
				GroupVersionKind: gvk,
			},
		},
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/agent/internal/k8s/resource", c.baseURL)
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
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Cortex API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ManifestResponse ResourceResponse `json:"manifestResponse"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.ManifestResponse.Manifest, nil
}

func (c *CortexK8sClient) ListResources(ctx context.Context, clusterId int, namespace, kind, labelSelector, fieldSelector string) ([]map[string]interface{}, error) {
	gvk, err := parseKind(kind)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kind: %w", err)
	}

	request := &ResourceRequestBean{
		ClusterId: clusterId,
		K8sRequest: &K8sRequestBean{
			ResourceIdentifier: K8sResourceIdentifier{
				Namespace:        namespace,
				GroupVersionKind: gvk,
			},
		},
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/agent/internal/k8s/resource/list", c.baseURL)
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
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Cortex API error (status %d): %s", resp.StatusCode, string(respBody))
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

func (c *CortexK8sClient) DeleteResource(ctx context.Context, clusterId int, namespace, name, kind string) error {
	gvk, err := parseKind(kind)
	if err != nil {
		return fmt.Errorf("failed to parse kind: %w", err)
	}

	request := &ResourceRequestBean{
		ClusterId: clusterId,
		K8sRequest: &K8sRequestBean{
			ResourceIdentifier: K8sResourceIdentifier{
				Name:             name,
				Namespace:        namespace,
				GroupVersionKind: gvk,
			},
		},
	}

	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/agent/internal/k8s/resource/delete", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call Cortex API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Cortex API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *CortexK8sClient) UpdateResource(ctx context.Context, clusterId int, namespace, name, kind string, patch map[string]interface{}) (map[string]interface{}, error) {
	gvk, err := parseKind(kind)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kind: %w", err)
	}

	existingResource, err := c.GetResource(ctx, clusterId, namespace, name, kind)
	if err != nil {
		return nil, fmt.Errorf("failed to get existing resource: %w", err)
	}

	mergedResource := mergeMaps(existingResource, patch)

	manifestBytes, err := json.Marshal(mergedResource)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal merged resource: %w", err)
	}

	request := map[string]interface{}{
		"clusterId": clusterId,
		"k8sRequest": map[string]interface{}{
			"resourceIdentifier": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
				"groupVersionKind": map[string]string{
					"group":   gvk.Group,
					"version": gvk.Version,
					"kind":    gvk.Kind,
				},
			},
			"patch": string(manifestBytes),
		},
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/agent/internal/k8s/resource/update", c.baseURL)
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
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Cortex API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ManifestResponse ResourceResponse `json:"manifestResponse"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.ManifestResponse.Manifest, nil
}

func mergeMaps(target, source map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range target {
		result[k] = v
	}
	for k, v := range source {
		if vMap, ok := v.(map[string]interface{}); ok {
			if targetMap, ok := result[k].(map[string]interface{}); ok {
				result[k] = mergeMaps(targetMap, vMap)
			} else {
				result[k] = vMap
			}
		} else {
			result[k] = v
		}
	}
	return result
}

func (c *CortexK8sClient) GetPodLogs(ctx context.Context, clusterId int, namespace, podName, containerName string, tailLines int, sinceSeconds int) (string, error) {
	request := struct {
		ClusterId     int    `json:"clusterId"`
		Namespace     string `json:"namespace"`
		PodName       string `json:"podName"`
		ContainerName string `json:"containerName,omitempty"`
		TailLines     *int64 `json:"tailLines,omitempty"`
		SinceSeconds  *int64 `json:"sinceSeconds,omitempty"`
	}{
		ClusterId:     clusterId,
		Namespace:     namespace,
		PodName:       podName,
		ContainerName: containerName,
	}

	if tailLines > 0 {
		tl := int64(tailLines)
		request.TailLines = &tl
	}
	if sinceSeconds > 0 {
		ss := int64(sinceSeconds)
		request.SinceSeconds = &ss
	}

	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/agent/internal/k8s/logs", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call Cortex API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Cortex API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(responseBody), nil
}

func (c *CortexK8sClient) ListEvents(ctx context.Context, clusterId int, namespace, resourceKind, resourceName string) ([]map[string]interface{}, error) {
	gvk, err := parseKind(resourceKind)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kind: %w", err)
	}

	request := &ResourceRequestBean{
		ClusterId: clusterId,
		K8sRequest: &K8sRequestBean{
			ResourceIdentifier: K8sResourceIdentifier{
				Name:             resourceName,
				Namespace:        namespace,
				GroupVersionKind: gvk,
			},
		},
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/agent/internal/k8s/events", c.baseURL)
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
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Cortex API error (status %d): %s", resp.StatusCode, string(respBody))
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

func (c *CortexK8sClient) RestartResource(ctx context.Context, clusterId int, namespace, name, kind string) error {
	gvk, err := parseKind(kind)
	if err != nil {
		return fmt.Errorf("failed to parse kind: %w", err)
	}

	request := map[string]interface{}{
		"clusterId": clusterId,
		"resources": []map[string]interface{}{
			{
				"name":      name,
				"namespace": namespace,
				"groupVersionKind": map[string]string{
					"group":   gvk.Group,
					"version": gvk.Version,
					"kind":    gvk.Kind,
				},
			},
		},
	}

	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/agent/internal/k8s/restart", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call Cortex API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Cortex API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func parseKind(kind string) (GroupVersionKind, error) {
	kindMap := map[string]GroupVersionKind{
		"Pod":                     {Group: "", Version: "v1", Kind: "Pod"},
		"Service":                 {Group: "", Version: "v1", Kind: "Service"},
		"ConfigMap":               {Group: "", Version: "v1", Kind: "ConfigMap"},
		"Secret":                  {Group: "", Version: "v1", Kind: "Secret"},
		"PersistentVolumeClaim":   {Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
		"PersistentVolume":        {Group: "", Version: "v1", Kind: "PersistentVolume"},
		"Node":                    {Group: "", Version: "v1", Kind: "Node"},
		"Namespace":               {Group: "", Version: "v1", Kind: "Namespace"},
		"ServiceAccount":          {Group: "", Version: "v1", Kind: "ServiceAccount"},
		"Deployment":              {Group: "apps", Version: "v1", Kind: "Deployment"},
		"StatefulSet":             {Group: "apps", Version: "v1", Kind: "StatefulSet"},
		"DaemonSet":               {Group: "apps", Version: "v1", Kind: "DaemonSet"},
		"ReplicaSet":              {Group: "apps", Version: "v1", Kind: "ReplicaSet"},
		"Job":                     {Group: "batch", Version: "v1", Kind: "Job"},
		"CronJob":                 {Group: "batch", Version: "v1", Kind: "CronJob"},
		"Ingress":                 {Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"},
		"NetworkPolicy":           {Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"},
		"Role":                    {Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
		"RoleBinding":             {Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
		"ClusterRole":             {Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
		"ClusterRoleBinding":      {Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"},
		"HorizontalPodAutoscaler": {Group: "autoscaling", Version: "v2", Kind: "HorizontalPodAutoscaler"},
	}

	gvk, ok := kindMap[kind]
	if !ok {
		return GroupVersionKind{}, fmt.Errorf("unsupported kind: %s", kind)
	}
	return gvk, nil
}
