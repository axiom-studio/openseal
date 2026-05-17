# Python SDK Integration with OpenSeal Helm Chart

## Overview

The Python SDK for tool injection in code nodes is now fully integrated into the OpenSeal Helm chart. When Sentinel deploys OpenSeal, the SDK is automatically deployed as a ConfigMap.

## What Changed

### Before (Standalone Deployment)
- SDK was a standalone manifest in `openseal/manifests/axiom-python-sdk.yaml`
- Required manual `kubectl apply`
- Not versioned with OpenSeal releases
- Prone to namespace mismatches

### After (Helm Integration)
- SDK is part of the OpenSeal Helm chart
- Deployed automatically when OpenSeal is deployed
- Versioned with OpenSeal releases
- Proper lifecycle management (install/upgrade/uninstall)

## Files Structure

```
openseal/charts/openseal/
├── files/
│   └── axiom_sdk.py                    # Python SDK source code
├── templates/
│   ├── python-sdk-configmap.yaml       # NEW: SDK ConfigMap template
│   ├── configmap.yaml                  # MODIFIED: Added PYTHON_SDK_CONFIGMAP env var
│   └── ...
└── values.yaml                         # MODIFIED: Added pythonSdk configuration
```

## Configuration Options

### values.yaml

```yaml
codeExecutor:
  enabled: true
  image: python:3.11-slim
  namespace: ""  # Uses same namespace as OpenSeal if empty
  
  # Python SDK configuration
  pythonSdk:
    enabled: true                    # Enable/disable SDK deployment
    configMapName: ""                # Override ConfigMap name (default: {release}-python-sdk)
```

### Environment Variables (Auto-set by Helm)

The SDK ConfigMap name is automatically injected into OpenSeal config:

```yaml
# In openseal-config ConfigMap
PYTHON_SDK_CONFIGMAP: "openseal-python-sdk"  # Or custom name from values
```

## Deployment

### Automatic Deployment

When Sentinel deploys OpenSeal using Helm:

```bash
# Sentinel runs:
helm install openseal ./charts/openseal -n <namespace>

# This automatically creates:
# 1. openseal-config ConfigMap (with PYTHON_SDK_CONFIGMAP)
# 2. openseal-python-sdk ConfigMap (with axiom_sdk.py)
# 3. OpenSeal deployment
# 4. All other resources
```

### Verification

```bash
# Check SDK ConfigMap was created
kubectl get configmap -n <namespace> | grep python-sdk

# View SDK contents
kubectl get configmap openseal-python-sdk -n <namespace> -o yaml

# Check OpenSeal config includes SDK reference
kubectl get configmap openseal-config -n <namespace> -o yaml | grep PYTHON_SDK_CONFIGMAP
```

## How It Works

### 1. Helm Rendering

When Helm renders the chart, the `python-sdk-configmap.yaml` template:

```yaml
{{- if and .Values.codeExecutor.enabled .Values.codeExecutor.pythonSdk.enabled }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "openseal.fullname" . }}-python-sdk
  # ...
data:
  axiom_sdk.py: |
{{ .Files.Get "files/axiom_sdk.py" | indent 4 }}
{{- end }}
```

Becomes:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: openseal-python-sdk
  namespace: axiom-agents
data:
  axiom_sdk.py: |
    # Full SDK code here
```

### 2. OpenSeal Configuration

The ConfigMap name is injected into OpenSeal config:

```yaml
# openseal-config ConfigMap
data:
  PYTHON_SDK_CONFIGMAP: "openseal-python-sdk"
```

### 3. Code Executor

OpenSeal reads the environment variable and uses it when creating jobs:

```go
sdkConfigMapName := os.Getenv("PYTHON_SDK_CONFIGMAP")
codeExecutor.SetToolDependencies(..., sdkConfigMapName)

// In job spec:
Volumes: []corev1.Volume{
    {
        Name: "axiom-sdk",
        VolumeSource: corev1.VolumeSource{
            ConfigMap: &corev1.ConfigMapVolumeSource{
                LocalObjectReference: corev1.LocalObjectReference{
                    Name: e.sdkConfigMapName,  // "openseal-python-sdk"
                },
            },
        },
    },
}
```

## Namespace Handling

The SDK ConfigMap is created in the **code executor namespace**:

```yaml
namespace: {{ .Values.codeExecutor.namespace | default .Release.Namespace }}
```

This ensures:
- Jobs can access the ConfigMap (same namespace)
- No cross-namespace mount issues
- Proper isolation

## Customization

### Custom ConfigMap Name

If you want to use a different name:

```yaml
# values.yaml
codeExecutor:
  pythonSdk:
    enabled: true
    configMapName: "my-custom-sdk"
```

### Disable SDK Deployment

If you want to manage the SDK separately:

```yaml
# values.yaml
codeExecutor:
  pythonSdk:
    enabled: false
```

## Upgrading

### From Standalone to Helm-Managed

If you previously deployed the standalone ConfigMap:

```bash
# 1. Remove old ConfigMap
kubectl delete configmap axiom-python-sdk -n <namespace>

# 2. Deploy OpenSeal (SDK included)
# Sentinel deploys with Helm automatically

# 3. Verify new ConfigMap
kubectl get configmap openseal-python-sdk -n <namespace>
```

### Updating SDK Code

To update the SDK:

1. Modify `charts/openseal/files/axiom_sdk.py`
2. Commit changes
3. Sentinel redeploys OpenSeal on next update
4. SDK ConfigMap is automatically updated

## Troubleshooting

### SDK ConfigMap Not Created

**Check:**
```bash
# Verify pythonSdk is enabled in values
helm get values openseal -n <namespace>

# Check template rendering
helm template openseal ./charts/openseal | grep -A 20 python-sdk
```

**Fix:**
```yaml
# In values.yaml or override
codeExecutor:
  pythonSdk:
    enabled: true
```

### Jobs Can't Mount SDK

**Check:**
```bash
# Verify ConfigMap exists in same namespace as jobs
kubectl get configmap -n <job-namespace>

# Check job pod events
kubectl describe pod <job-pod> -n <job-namespace>
```

**Fix:**
- Ensure `codeExecutor.namespace` matches job namespace
- Verify ConfigMap name in OpenSeal config matches actual ConfigMap

### SDK Version Mismatch

**Check:**
```bash
# Compare SDK in ConfigMap vs. chart files
diff <(kubectl get configmap openseal-python-sdk -o jsonpath='{.data.axiom_sdk\.py}') \
     charts/openseal/files/axiom_sdk.py
```

**Fix:**
```bash
# Upgrade OpenSeal deployment
helm upgrade openseal ./charts/openseal -n <namespace>
```

## Benefits

✅ **Automatic Deployment** - No manual kubectl commands needed
✅ **Version Control** - SDK version tied to OpenSeal chart version
✅ **Lifecycle Management** - Helm handles install/upgrade/uninstall
✅ **Namespace Safety** - Always deployed in correct namespace
✅ **Rollback Support** - `helm rollback` includes SDK
✅ **Consistency** - Same SDK version across all nodes in cluster

## Migration Checklist

For existing deployments:

- [ ] Remove standalone `openseal/manifests/axiom-python-sdk.yaml`
- [ ] Update OpenSeal chart with new files/templates
- [ ] Ensure `codeExecutor.pythonSdk.enabled: true` in values
- [ ] Delete old standalone ConfigMap
- [ ] Deploy/upgrade OpenSeal via Helm
- [ ] Verify new ConfigMap created
- [ ] Test code execution with tools
- [ ] Update documentation/runbooks

## Related Documentation

- Main feature docs: `openseal/pkg/agent/executor/TOOLS_FEATURE.md`
- OpenSeal chart: `openseal/charts/openseal/`
- SDK source: `openseal/charts/openseal/files/axiom_sdk.py`
- SDK tests: `openseal/pkg/agent/executor/code_runtime/test_axiom_sdk.py`
