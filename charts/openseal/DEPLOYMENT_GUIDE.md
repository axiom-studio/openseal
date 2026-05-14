# OpenSeal Deployment Guide - Tools for Code Nodes

## Quick Start

The tools feature is **enabled by default** in OpenSeal. When you deploy OpenSeal with Helm, the Python SDK and tool execution services are automatically configured.

### Default Configuration

```yaml
# charts/openseal/values.yaml (defaults)
codeExecutor:
  enabled: true
  pythonSdk:
    enabled: true
    jwtSecret: "change-me-in-production"  # ⚠️ CHANGE IN PRODUCTION!
```

### Production Deployment

**1. Generate a secure JWT secret:**
```bash
# Generate a random 32-character secret
openssl rand -base64 32
# Example output: K7gNU3sdo+OL0wNhqoVWhr3g6s1xYv3+YCKkXWKnFRA=
```

**2. Update values.yaml or use --set:**
```bash
# Option A: Edit values.yaml
codeExecutor:
  pythonSdk:
    jwtSecret: "K7gNU3sdo+OL0wNhqoVWhr3g6s1xYv3+YCKkXWKnFRA="

# Option B: Use --set during install/upgrade
helm upgrade openseal ./charts/openseal \
  --set codeExecutor.pythonSdk.jwtSecret="K7gNU3sdo+OL0wNhqoVWhr3g6s1xYv3+YCKkXWKnFRA="
```

**3. Deploy/Upgrade OpenSeal:**
```bash
# Via Sentinel (automatic)
# Sentinel will deploy OpenSeal with your configured values

# Or manually for testing
helm upgrade --install openseal ./charts/openseal \
  --namespace axiom \
  --create-namespace
```

## What Gets Deployed

When OpenSeal is deployed, the following resources are created:

### ConfigMaps

1. **openseal-config** - OpenSeal runtime configuration
   ```yaml
   TOOL_EXECUTION_JWT_SECRET: "<your-secret>"
   PYTHON_SDK_CONFIGMAP: "openseal-python-sdk"
   ```

2. **openseal-python-sdk** - Python SDK code
   ```yaml
   data:
     axiom_sdk.py: |
       # Full SDK implementation
   ```

### Services Initialized

In OpenSeal runtime (cmd/openseal/main.go):

```go
// Automatically initialized in NewRuntimeWorker:
tokenService := executor.NewExecutionTokenService(cfg.ToolExecutionJWTSecret)
contextStore := executor.NewToolExecutionContextStore()
toolRegistry := executor.GetGlobalToolRegistry()
```

## Verification

### 1. Check ConfigMaps

```bash
# List ConfigMaps
kubectl get configmap -n axiom

# Should show:
# openseal-config
# openseal-python-sdk

# Verify SDK ConfigMap exists
kubectl get configmap openseal-python-sdk -n axiom -o yaml

# Verify OpenSeal config has tool settings
kubectl get configmap openseal-config -n axiom -o yaml | grep -A 2 TOOL_EXECUTION
```

### 2. Check OpenSeal Logs

```bash
# View OpenSeal startup logs
kubectl logs -n axiom deployment/openseal --tail=50

# Look for these log lines:
# "tool execution configured for code nodes"
# "executor registry initialized" hasCodeExecutor=true
```

### 3. Test Tool Execution

Create a simple test agent in Studio:

**Nodes:**
1. Manual Trigger
2. PGVector Search Tool (configure with test data)
3. Code Node with this code:
   ```python
   # Tool is automatically available
   results = vector_search(query="test", limit=5)
   result = {"status": "success", "count": len(results)}
   ```
4. Connect: Trigger → Code, Tool → Code (tools-target)

**Expected Result:**
- Code executes successfully
- `results` contains vector search output
- No import errors or missing function errors

## Configuration Options

### Complete values.yaml Example

```yaml
codeExecutor:
  enabled: true
  image: python:3.11-slim
  namespace: "axiom-agents"  # Where jobs run
  
  pythonSdk:
    enabled: true
    configMapName: ""  # Default: openseal-python-sdk
    jwtSecret: "YOUR-SECURE-SECRET-HERE"
```

### Environment Variables (Auto-set)

These are automatically set in OpenSeal from values.yaml:

| Variable | Default | Description |
|----------|---------|-------------|
| `TOOL_EXECUTION_JWT_SECRET` | `change-me-in-production` | Secret for signing JWT tokens |
| `PYTHON_SDK_CONFIGMAP` | `openseal-python-sdk` | Name of SDK ConfigMap |
| `SENTINEL_API_URL` | `http://sentinel.axiomcd:8090` | Base URL for tool proxy API |

### Computed Values

These are computed at runtime:

- **Tool Proxy URL**: `{SENTINEL_API_URL}/orchestrator/agent/internal/executions/{runId}/nodes/{nodeId}/tools/invoke`
- **ConfigMap Name**: `{release-name}-python-sdk` (e.g., `openseal-python-sdk`)

## Security Considerations

### JWT Secret

**⚠️ CRITICAL:** Change the default JWT secret in production!

**Why:**
- Default secret is publicly known
- Allows unauthorized tool execution if not changed
- Anyone with the default secret could call tools

**Best Practices:**
- Use a cryptographically random secret (32+ characters)
- Store in Kubernetes Secret or external secret manager
- Rotate periodically (requires OpenSeal restart)

**Example with Kubernetes Secret:**

```yaml
# Create secret
apiVersion: v1
kind: Secret
metadata:
  name: openseal-tool-secrets
  namespace: axiom
stringData:
  jwt-secret: "K7gNU3sdo+OL0wNhqoVWhr3g6s1xYv3+YCKkXWKnFRA="

---
# Reference in values.yaml (requires chart modification)
codeExecutor:
  pythonSdk:
    jwtSecretRef:
      name: openseal-tool-secrets
      key: jwt-secret
```

### Token Scope

JWT tokens are automatically scoped to:
- Specific `runID` (workflow execution)
- Specific `nodeID` (code node instance)
- 1-hour expiration

This ensures:
- Code can only call tools connected to its node
- Tokens can't be reused across workflows
- Automatic expiration prevents long-lived tokens

## Troubleshooting

### SDK ConfigMap Not Found

**Symptom:**
```
Error: ConfigMap "openseal-python-sdk" not found
```

**Check:**
```bash
# Verify pythonSdk is enabled
helm get values openseal -n axiom | grep -A 5 pythonSdk

# Should show:
# pythonSdk:
#   enabled: true
```

**Fix:**
```yaml
# In values.yaml
codeExecutor:
  pythonSdk:
    enabled: true
```

### Tool Execution Fails with 401 Unauthorized

**Symptom:**
```python
ToolExecutionError: vector_search HTTP error 401: Invalid token
```

**Causes:**
1. JWT secret mismatch between OpenSeal and code jobs
2. Token expired (job ran > 1 hour)
3. Token validation failing

**Check:**
```bash
# Verify JWT secret is set correctly
kubectl get configmap openseal-config -n axiom -o yaml | grep TOOL_EXECUTION_JWT_SECRET

# Check OpenSeal logs for token validation errors
kubectl logs -n axiom deployment/openseal | grep "Invalid execution token"
```

**Fix:**
- Ensure JWT secret matches in config
- Restart OpenSeal after changing secret
- Check system clock skew (affects token expiration)

### Tool Not Available in Python

**Symptom:**
```python
NameError: name 'vector_search' is not defined
```

**Causes:**
1. Tool node not connected to code node
2. SDK not mounted in job
3. SDK import failed

**Check:**
```bash
# 1. Verify tool connection in workflow JSON
# Tool should have targetHandle: "tools-target"

# 2. Check if SDK ConfigMap exists
kubectl get configmap openseal-python-sdk -n axiom

# 3. Inspect a code execution job
kubectl get pods -n axiom-agents
kubectl describe pod <code-job-pod> -n axiom-agents
# Look for volume mount: openseal-python-sdk at /axiom-sdk

# 4. Check job logs for SDK import errors
kubectl logs <code-job-pod> -n axiom-agents
```

**Fix:**
- Reconnect tool node to code node (drag edge to tools-target handle)
- Verify SDK ConfigMap deployed
- Check SDK ConfigMap name matches config

### "Context not found" Errors

**Symptom:**
```
Error: execution context not found for runID=... nodeID=...
```

**Causes:**
1. Execution context expired (> 2 hours old)
2. OpenSeal restarted (contexts stored in memory)
3. RunID/NodeID mismatch

**Check:**
```bash
# Check OpenSeal uptime
kubectl get pods -n axiom

# Check OpenSeal logs
kubectl logs -n axiom deployment/openseal | grep "execution context"
```

**Fix:**
- Retry execution (context should be recreated)
- Contexts are recreated on each execution
- If persistent across restarts needed, file issue for persistent storage

## Monitoring

### Logging

**Structured Logs:**

```bash
# Tool execution started
{"level":"info","msg":"Executing tool","runId":"...","nodeId":"...","toolName":"vector_search"}

# Tool execution completed
{"level":"info","msg":"Tool execution completed","runId":"...","duration_ms":123}

# Tool execution failed
{"level":"error","msg":"Tool execution failed","error":"...","runId":"...","toolName":"..."}
```

## Upgrade Path

### From Previous Versions

If upgrading from a version without tools support:

**1. Update chart:**
```bash
git pull  # Get latest charts
```

**2. Review new values:**
```bash
# Check what's new
diff old-values.yaml charts/openseal/values.yaml
```

**3. Set JWT secret:**
```yaml
# Add to your values.yaml
codeExecutor:
  pythonSdk:
    jwtSecret: "<your-secure-secret>"
```

**4. Upgrade OpenSeal:**
```bash
helm upgrade openseal ./charts/openseal -n axiom
```

**5. Verify:**
```bash
# Check new ConfigMap created
kubectl get configmap openseal-python-sdk -n axiom

# Check OpenSeal logs
kubectl logs -n axiom deployment/openseal | grep "tool execution configured"
```

### Rollback

If issues occur:

```bash
# Rollback to previous release
helm rollback openseal -n axiom

# Or specific revision
helm rollback openseal 1 -n axiom
```

## Best Practices

### Development

✅ **Do:**
- Use default JWT secret for local development
- Test tool connections in Studio before deploying
- Check OpenSeal logs during development

❌ **Don't:**
- Commit JWT secrets to git
- Use production secrets in development

### Production

✅ **Do:**
- Generate strong JWT secrets (32+ characters)
- Store secrets in external secret manager
- Monitor tool execution metrics
- Set up alerts for high error rates
- Rotate JWT secrets periodically

❌ **Don't:**
- Use default JWT secret
- Share JWT secrets across environments
- Disable tool execution without disabling SDK

## Support

For issues or questions:

1. **Check logs:** `kubectl logs -n axiom deployment/openseal`
2. **Verify config:** `kubectl get configmap openseal-config -n axiom -o yaml`
3. **Test manually:** Create simple test workflow in Studio
4. **Review docs:** `/openseal/pkg/agent/executor/TOOLS_FEATURE.md`
5. **File issue:** Include logs, config, and error messages

## Related Documentation

- **Feature Overview**: `openseal/pkg/agent/executor/TOOLS_FEATURE.md`
- **Helm Integration**: `openseal/charts/openseal/PYTHON_SDK_INTEGRATION.md`
- **OpenSeal Chart**: `openseal/charts/openseal/README.md`
- **SDK Reference**: `openseal/pkg/agent/executor/code_runtime/axiom_sdk.py`
