package executor

import (
	"fmt"

	"github.com/axiom-studio/openseal/pkg/module"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// resolveGVR resolves a Kubernetes kind to its GroupVersionResource
func resolveGVR(kind string) (schema.GroupVersionResource, error) {
	gvrMap := map[string]schema.GroupVersionResource{
		"Pod":                     {Group: "", Version: "v1", Resource: "pods"},
		"Service":                 {Group: "", Version: "v1", Resource: "services"},
		"ConfigMap":               {Group: "", Version: "v1", Resource: "configmaps"},
		"Secret":                  {Group: "", Version: "v1", Resource: "secrets"},
		"PersistentVolumeClaim":   {Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
		"PersistentVolume":        {Group: "", Version: "v1", Resource: "persistentvolumes"},
		"Node":                    {Group: "", Version: "v1", Resource: "nodes"},
		"Namespace":               {Group: "", Version: "v1", Resource: "namespaces"},
		"ServiceAccount":          {Group: "", Version: "v1", Resource: "serviceaccounts"},
		"Deployment":              {Group: "apps", Version: "v1", Resource: "deployments"},
		"StatefulSet":             {Group: "apps", Version: "v1", Resource: "statefulsets"},
		"DaemonSet":               {Group: "apps", Version: "v1", Resource: "daemonsets"},
		"ReplicaSet":              {Group: "apps", Version: "v1", Resource: "replicasets"},
		"Job":                     {Group: "batch", Version: "v1", Resource: "jobs"},
		"CronJob":                 {Group: "batch", Version: "v1", Resource: "cronjobs"},
		"Ingress":                 {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
		"NetworkPolicy":           {Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
		"Role":                    {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
		"RoleBinding":             {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
		"ClusterRole":             {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
		"ClusterRoleBinding":      {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
		"HorizontalPodAutoscaler": {Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"},
		"Event":                   {Group: "", Version: "v1", Resource: "events"},
	}

	gvr, ok := gvrMap[kind]
	if !ok {
		return schema.GroupVersionResource{}, fmt.Errorf("unsupported kind: %s", kind)
	}
	return gvr, nil
}

// extractNameNamespace extracts name and namespace from config with template resolution
func extractNameNamespace(config map[string]interface{}, resolver TemplateResolver) (name, namespace string, err error) {
	nameTemplate, ok := config["name"].(string)
	if !ok || nameTemplate == "" {
		return "", "", fmt.Errorf("'name' is required")
	}
	name = resolver.ResolveString(nameTemplate)

	namespaceTemplate, ok := config["namespace"].(string)
	if ok && namespaceTemplate != "" {
		namespace = resolver.ResolveString(namespaceTemplate)
	} else {
		namespace = module.GetAgentsNamespace()
	}

	return name, namespace, nil
}

// extractClusterId extracts cluster ID from config (required for multi-cluster support)
// Handles both string and numeric cluster IDs from the UI
func extractClusterId(config map[string]interface{}) int {
	clusterIdRaw, exists := config["clusterId"]
	if !exists {
		return 1 // Default to cluster 1 if not specified
	}

	// Handle numeric cluster ID (float64 from JSON)
	if clusterId, ok := clusterIdRaw.(float64); ok && clusterId > 0 {
		return int(clusterId)
	}

	// Handle string cluster ID (from UI select dropdown)
	if clusterIdStr, ok := clusterIdRaw.(string); ok && clusterIdStr != "" {
		// Try to parse as integer
		var clusterId int
		if _, err := fmt.Sscanf(clusterIdStr, "%d", &clusterId); err == nil && clusterId > 0 {
			return clusterId
		}
	}

	// Default to cluster 1 if parsing fails
	return 1
}

// wrapK8sError wraps a K8s API error with additional context
func wrapK8sError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s failed: %w", operation, err)
}

// extractKind extracts and validates the kind field from config
func extractKind(config map[string]interface{}) (string, error) {
	kind, ok := config["kind"].(string)
	if !ok || kind == "" {
		return "", fmt.Errorf("'kind' is required")
	}
	return kind, nil
}

// createDeleteOptions creates delete options with gracePeriodSeconds if specified
func createDeleteOptions(config map[string]interface{}) *metav1.DeleteOptions {
	opts := &metav1.DeleteOptions{}
	if gracePeriod, ok := config["gracePeriodSeconds"].(float64); ok && gracePeriod >= 0 {
		gracePeriodInt := int64(gracePeriod)
		opts.GracePeriodSeconds = &gracePeriodInt
	}
	return opts
}

// extractLabelSelector extracts label selector from config with template resolution
func extractLabelSelector(config map[string]interface{}, resolver TemplateResolver) string {
	if labelSelector, ok := config["labelSelector"].(string); ok && labelSelector != "" {
		return resolver.ResolveString(labelSelector)
	}
	return ""
}

// extractFieldSelector extracts field selector from config with template resolution
func extractFieldSelector(config map[string]interface{}, resolver TemplateResolver) string {
	if fieldSelector, ok := config["fieldSelector"].(string); ok && fieldSelector != "" {
		return resolver.ResolveString(fieldSelector)
	}
	return ""
}

// extractLimit extracts limit from config with default
func extractLimit(config map[string]interface{}) int64 {
	if limit, ok := config["limit"].(float64); ok && limit > 0 {
		return int64(limit)
	}
	return 100
}
