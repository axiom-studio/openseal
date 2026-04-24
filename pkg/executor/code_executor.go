package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ContextUploader abstracts the storage of execution context
type ContextUploader interface {
	UploadContext(ctx context.Context, data []byte) (string, error)
}

// CodeExecutor runs code in a Kubernetes Job
//
//	Config: {
//	  "language": "python",
//	  "code": "print('Hello')\nresult = {'status': 'ok'}",
//	  "requirements": ["requests", "pandas"],
//	  "timeout": 300,
//	  "resources": {"cpu": "100m", "memory": "256Mi"}
//	}
type CodeExecutor struct {
	k8sClient               kubernetes.Interface
	namespace               string
	runnerImage             string
	serviceAccount          string
	ttlSecondsAfterFinished int32
	contextUploader         ContextUploader
	tokenService            *ExecutionTokenService
	contextStore            *ToolExecutionContextStore
	toolRegistry            *ToolRegistry
	toolProxyBaseURL        string // Base URL for tool proxy API
	sdkConfigMapName        string // Name of the SDK ConfigMap (from Helm)
}

// CodeExecutorConfig holds configuration for the code executor
type CodeExecutorConfig struct {
	Namespace      string
	RunnerImage    string
	ServiceAccount string
	// TTLSecondsAfterFinished controls how long completed/failed Jobs are kept
	// for debugging and pod inspection. Default is 3600 (1 hour).
	TTLSecondsAfterFinished int32
}

func NewCodeExecutor(k8sClient kubernetes.Interface, config *CodeExecutorConfig) *CodeExecutor {
	namespace := "default"
	runnerImage := "python:3.11-slim"
	serviceAccount := "default"
	ttlSeconds := int32(3600) // Default: 1 hour for pod inspection

	if config != nil {
		if config.Namespace != "" {
			namespace = config.Namespace
		}
		if config.RunnerImage != "" {
			runnerImage = config.RunnerImage
		}
		if config.ServiceAccount != "" {
			serviceAccount = config.ServiceAccount
		}
		if config.TTLSecondsAfterFinished > 0 {
			ttlSeconds = config.TTLSecondsAfterFinished
		}
	}

	return &CodeExecutor{
		k8sClient:               k8sClient,
		namespace:               namespace,
		runnerImage:             runnerImage,
		serviceAccount:          serviceAccount,
		ttlSecondsAfterFinished: ttlSeconds,
	}
}

func (e *CodeExecutor) SetContextUploader(uploader ContextUploader) {
	e.contextUploader = uploader
}

func (e *CodeExecutor) SetToolDependencies(tokenService *ExecutionTokenService, contextStore *ToolExecutionContextStore, toolRegistry *ToolRegistry, toolProxyBaseURL string, sdkConfigMapName string) {
	e.tokenService = tokenService
	e.contextStore = contextStore
	e.toolRegistry = toolRegistry
	e.toolProxyBaseURL = toolProxyBaseURL
	e.sdkConfigMapName = sdkConfigMapName

	// Default to standard name if not provided
	if e.sdkConfigMapName == "" {
		e.sdkConfigMapName = "openseal-python-sdk"
	}
}

func (e *CodeExecutor) Type() string {
	return StepTypeCode
}

// extractRunID extracts the run ID from context data
// Handles string, int, and float64 types (test workflows use int, regular runs use string)
func extractRunID(contextData map[string]interface{}) string {
	if run, ok := contextData["run"].(map[string]interface{}); ok {
		if id, ok := run["id"].(string); ok {
			return id
		} else if id, ok := run["id"].(int); ok {
			return fmt.Sprintf("%d", id)
		} else if id, ok := run["id"].(float64); ok {
			return fmt.Sprintf("%.0f", id)
		}
	}
	return ""
}

// buildToolsContext constructs the _tools configuration for Python SDK injection
// Returns nil if no tools are available or token is missing
func (e *CodeExecutor) buildToolsContext(runID string, nodeID string, connectedTools []*ToolDefinition, executionToken string) map[string]interface{} {
	if len(connectedTools) == 0 || executionToken == "" {
		return nil
	}

	// Build full tool proxy URL with execution-specific path
	// Format: {base}/{runId}/nodes/{nodeId}/tools/invoke
	fullURL := fmt.Sprintf("%s/%s/nodes/%s/tools/invoke",
		strings.TrimSuffix(e.toolProxyBaseURL, "/"),
		runID,
		nodeID)

	return map[string]interface{}{
		"api_base": fullURL,
		"token":    executionToken,
		"tools":    e.convertToolsToSDKFormat(connectedTools),
	}
}

func (e *CodeExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("code step requires config")
	}

	// Get language (default: python)
	language := "python"
	if l, ok := config["language"].(string); ok {
		language = l
	}

	// Only Python supported for now
	if language != "python" {
		return nil, fmt.Errorf("unsupported language: %s (only python is supported)", language)
	}

	// Get code (required)
	codeTemplate, ok := config["code"].(string)
	if !ok || codeTemplate == "" {
		return nil, fmt.Errorf("code step requires 'code'")
	}
	code := resolver.ResolveString(codeTemplate)

	// Get timeout (default: 5 minutes)
	timeout := 300
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeout = int(t)
	}

	// Get requirements
	var requirements []string
	if reqs, ok := config["requirements"].([]interface{}); ok {
		for _, r := range reqs {
			if s, ok := r.(string); ok {
				requirements = append(requirements, s)
			}
		}
	}

	// Build context data for the code to access
	var contextData map[string]interface{}
	if cp, ok := resolver.(ContextProvider); ok {
		contextData = cp.GetContextData()
	} else {
		contextData = make(map[string]interface{})
	}

	// Ensure all context values are properly initialized
	if contextData["bindings"] == nil {
		contextData["bindings"] = make(map[string]interface{})
	}
	if contextData["trigger"] == nil {
		contextData["trigger"] = make(map[string]interface{})
	}
	if contextData["prev"] == nil {
		contextData["prev"] = make(map[string]interface{})
	}
	if contextData["nodes"] == nil {
		contextData["nodes"] = make(map[string]interface{})
	}
	if contextData["vars"] == nil {
		contextData["vars"] = make(map[string]interface{})
	}
	if contextData["run"] == nil {
		contextData["run"] = make(map[string]interface{})
	}

	// Discover connected tools from the graph
	connectedTools, executionToken := e.discoverAndSetupTools(ctx, step, resolver, contextData)

	// Extract runID and build tools context if tools are available
	runID := extractRunID(contextData)
	if toolsContext := e.buildToolsContext(runID, step.Id, connectedTools, executionToken); toolsContext != nil {
		contextData["_tools"] = toolsContext
		fmt.Printf("DEBUG: Added %d tools to context for node %s with URL=%s\n",
			len(connectedTools), step.Id, toolsContext["api_base"])
	} else {
		fmt.Printf("DEBUG: No tools added to context for node %s (tools=%d, hasToken=%v)\n",
			step.Id, len(connectedTools), executionToken != "")
	}

	contextJSON, err := json.Marshal(contextData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal context data: %w", err)
	}

	// Generate unique job name (must be lowercase RFC 1123)
	safeName := sanitizeK8sName(step.Name)
	if len(safeName) > 10 {
		safeName = safeName[:10]
	}
	safeId := sanitizeK8sName(step.Id)
	if len(safeId) > 8 {
		safeId = safeId[len(safeId)-8:]
	}

	// Use UnixNano to avoid collisions in parallel execution
	jobName := fmt.Sprintf("code-%s-%s-%d", safeName, safeId, time.Now().UnixNano())
	if len(jobName) > 63 {
		jobName = jobName[:63]
	}

	// Upload context if uploader is available to avoid ARG_MAX limits
	var contextURL string
	if e.contextUploader != nil {
		url, err := e.contextUploader.UploadContext(ctx, contextJSON)
		if err == nil {
			contextURL = url
			// If upload successful, clear the JSON from env var to save space
			contextJSON = []byte("{}")
		} else {
			// Log error but continue with env var fallback
			// In a real logger we'd log this, here we rely on the fact that
			// contextJSON still holds the data
			fmt.Printf("Warning: context upload failed: %v\n", err)
		}
	}

	// Build job with context passed via env var
	// Files are stored separately and accessed via URL (see fileBaseURL in context)
	job := e.buildJob(jobName, code, requirements, timeout, string(contextJSON), contextURL)

	// Submit Job to Kubernetes
	createdJob, err := e.k8sClient.BatchV1().Jobs(e.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	// Wait for Job completion
	output, err := e.waitForJob(ctx, createdJob.Name, time.Duration(timeout)*time.Second)
	if err != nil {
		// Don't delete job on error - keep for debugging, TTL will clean up
		return nil, err
	}

	// Job completed successfully - let TTL handle cleanup for pod inspection
	return &StepResult{
		Output: output,
	}, nil
}

func (e *CodeExecutor) buildJob(name, code string, requirements []string, timeout int, contextJSON string, contextURL string) *batchv1.Job {
	// Build pip install command if requirements exist (fully silenced)
	pipInstall := ""
	if len(requirements) > 0 {
		pipInstall = fmt.Sprintf("pip install --quiet --disable-pip-version-check --no-warn-script-location %s >/dev/null 2>&1 && ", joinRequirements(requirements))
	}

	// Wrap code to capture output as JSON with context injection
	wrappedCode := fmt.Sprintf(`
import json
import os
import sys
import urllib.request
import time

# Parse context
_ctx = {}
_ctx_url = os.environ.get('AGENT_CONTEXT_URL')

if _ctx_url:
    try:
        # Retry logic for context fetching
        for i in range(3):
            try:
                with urllib.request.urlopen(_ctx_url, timeout=30) as response:
                    _ctx_raw = response.read().decode('utf-8')
                    _ctx = json.loads(_ctx_raw)
                    break
            except Exception as e:
                if i == 2: raise e
                time.sleep(1)
    except Exception as e:
        print(f"---AGENT_OUTPUT_START---")
        print(json.dumps({"success": False, "error": f"Failed to load context: {e}"}))
        sys.exit(1)
else:
    # Fallback to env var
    _ctx_raw = os.environ.get('AGENT_CONTEXT', '{}')
    try:
        _ctx = json.loads(_ctx_raw)
    except:
        pass

# Context variables available to user code
# Use 'or {}' to handle None values (JSON null becomes Python None)
bindings = _ctx.get('bindings') or {}
trigger = _ctx.get('trigger') or {}
prev = _ctx.get('prev') or {}
nodes = _ctx.get('nodes') or {}
vars = _ctx.get('vars') or {}
run = _ctx.get('run') or {}
self = _ctx.get('self') or {}
elapsed_ms = _ctx.get('elapsed_ms') or 0

def get_node(name, default=None):
    return nodes.get(name, default)

def get_node_duration(name):
    """Get the execution duration of a specific node in milliseconds.
    
    Args:
        name: Node name (e.g., 'http-call', 'transform')
    
    Returns:
        int: Duration in milliseconds, or 0 if node hasn't completed
    
    Example:
        duration = get_node_duration('http-call')
        print(f"HTTP call took {duration}ms")
    """
    node_meta = run.get('nodes', {}).get(name, {})
    return node_meta.get('duration', 0)

def get_cumulative_duration():
    """Get the total execution time of all completed nodes.
    
    Returns:
        int: Total duration in milliseconds
    
    Example:
        total = get_cumulative_duration()
        print(f"Total workflow time: {total}ms")
    """
    return sum(n.get('duration', 0) for n in run.get('nodes', {}).values())

def get_file(file_obj):
    """Fetch file content from a file object.

    Args:
        file_obj: A file object dict with '_type': 'file' and 'url' key,
                  e.g., trigger['document'] or nodes['upload']['output']['file']

    Returns:
        bytes: The file content

    Example:
        content = get_file(trigger['document'])
        # or for text files:
        text = get_file(trigger['document']).decode('utf-8')
    """
    if not file_obj or not isinstance(file_obj, dict):
        raise ValueError("Invalid file object")
    if file_obj.get('_type') != 'file':
        raise ValueError("Not a file object (missing _type: 'file')")
    url = file_obj.get('url')
    if not url:
        raise ValueError("File object has no URL")
    
    # Create request with headers to avoid 403 errors from CDNs/servers
    req = urllib.request.Request(
        url,
        headers={
            'User-Agent': 'Axiom-Agent-Runtime/1.0',
            'Accept': '*/*'
        }
    )
    with urllib.request.urlopen(req, timeout=60) as response:
        return response.read()

# Streaming support - emit partial outputs to downstream nodes
def emit(data, progress=None):
    """Emit a partial output for streaming to downstream nodes.

    This sends data to the next node in the pipeline without waiting for
    this node to complete. The downstream node can start processing
    immediately.

    Args:
        data: The data to emit (will be JSON serialized)
        progress: Optional progress value (0.0 to 1.0)

    Example:
        # Parse CSV and send each row to the next node for processing
        for i, row in enumerate(csv_reader):
            emit({"row": row}, progress=i/total_rows)
        result = {"total_processed": total_rows}

    Note:
        If the downstream node's buffer is full, this call will block
        until there is space (backpressure). This prevents overwhelming
        slower downstream nodes.
    """
    payload = {"data": data}
    if progress is not None:
        payload["progress"] = float(progress)

    # Check if streaming endpoint is configured
    emit_url = run.get('emitUrl')
    run_id = run.get('id')
    node_id = run.get('currentNodeId')

    if emit_url and run_id is not None and node_id:
        # POST to streaming endpoint (blocks until downstream is ready - backpressure)
        emit_payload = {
            "runId": run_id,
            "nodeId": node_id,
            "data": data,
            "progress": progress if progress is not None else 0
        }
        try:
            # Try to serialize to JSON first to catch NaN/invalid data early
            try:
                payload_json = json.dumps(emit_payload)
            except (ValueError, TypeError) as json_err:
                raise RuntimeError(f"emit() data is not JSON serializable: {json_err}. Common issues: NaN values (use .fillna(None) or .replace({{np.nan: None}})), datetime objects (convert to strings), or custom objects.")
            
            req = urllib.request.Request(
                emit_url,
                data=payload_json.encode('utf-8'),
                headers={'Content-Type': 'application/json'},
                method='POST'
            )
            with urllib.request.urlopen(req, timeout=300) as response:
                # Response indicates if item was queued
                pass
        except urllib.error.HTTPError as e:
            error_body = e.read().decode('utf-8') if e.fp else str(e)
            error_msg = f"emit() failed with HTTP {e.code}: {error_body}"
            print(error_msg, file=sys.stderr)
            raise RuntimeError(error_msg)
        except Exception as e:
            error_msg = f"emit() failed: {str(e)}"
            print(error_msg, file=sys.stderr)
            raise RuntimeError(error_msg)
    else:
        # Fall back to stdout marker (for backward compatibility / UI streaming)
        print("---AGENT_STREAM---" + json.dumps(payload), flush=True)

# Inject tool functions if tools are available
_tools_config = _ctx.get('_tools')
if _tools_config:
    # Add SDK directory to path if it exists
    _sdk_path = '/axiom-sdk'
    if os.path.exists(_sdk_path) and _sdk_path not in sys.path:
        sys.path.insert(0, _sdk_path)
    
    try:
        from axiom_sdk import inject_tools
        # Note: inject_tools will be called after _exec_globals is created
    except ImportError as e:
        print(f"Warning: Failed to import Axiom SDK: {e}", file=sys.stderr)
        _tools_config = None  # Disable tools if SDK not available

# Create execution context with all variables accessible
_exec_globals = {
    'bindings': bindings,
    'trigger': trigger,
    'prev': prev,
    'nodes': nodes,
    'vars': vars,
    'run': run,
    'self': self,
    'elapsed_ms': elapsed_ms,
    'get_node': get_node,
    'get_node_duration': get_node_duration,
    'get_cumulative_duration': get_cumulative_duration,
    'get_file': get_file,
    'emit': emit,
    'result': None,
    'json': json,
    'os': os,
    'sys': sys,
    '__builtins__': __builtins__,
}

# Inject tool functions into global namespace
if _tools_config:
    try:
        inject_tools(_exec_globals, _tools_config)
    except Exception as e:
        print(f"Warning: Failed to inject tools: {e}", file=sys.stderr)

try:
    exec('''%s''', _exec_globals)
    result = _exec_globals.get('result')
    _output = result if result is not None else {"status": "completed"}
    if not isinstance(_output, dict):
        _output = {"result": _output}
    
    def clean_nan(obj):
        if isinstance(obj, float):
            if obj != obj:  # NaN check (NaN != NaN in Python)
                return None
            return obj
        elif isinstance(obj, dict):
            return {k: clean_nan(v) for k, v in obj.items()}
        elif isinstance(obj, list):
            return [clean_nan(item) for item in obj]
        return obj
    
    _output = clean_nan(_output)
    print("---AGENT_OUTPUT_START---")
    print(json.dumps({"success": True, "output": _output}))
except Exception as e:
    import traceback
    print("---AGENT_OUTPUT_START---")
    print(json.dumps({"success": False, "error": str(e), "traceback": traceback.format_exc()}))
    sys.exit(1)
`, escapeForPython(code))

	command := fmt.Sprintf("%spython -c '%s'", pipInstall, escapeForShell(wrappedCode))

	backoffLimit := int32(0)
	ttlSeconds := e.ttlSecondsAfterFinished
	activeDeadlineSeconds := int64(timeout)

	envVars := []corev1.EnvVar{
		{Name: "AGENT_CONTEXT", Value: contextJSON},
	}
	if contextURL != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "AGENT_CONTEXT_URL", Value: contextURL})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: e.namespace,
			Labels: map[string]string{
				"app":        "agent-code-runner",
				"managed-by": "agent-orchestrator",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSeconds,
			ActiveDeadlineSeconds:   &activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: e.serviceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "code-runner",
							Image:   e.runnerImage,
							Command: []string{"/bin/sh", "-c", command},
							Env:     envVars,
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    mustParseQuantity("500m"),
									corev1.ResourceMemory: mustParseQuantity("512Mi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    mustParseQuantity("100m"),
									corev1.ResourceMemory: mustParseQuantity("128Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "axiom-sdk",
									MountPath: "/axiom-sdk",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "axiom-sdk",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: e.sdkConfigMapName,
									},
									Optional: boolPtr(true), // Don't fail if ConfigMap doesn't exist
								},
							},
						},
					},
				},
			},
		},
	}
}

func (e *CodeExecutor) waitForJob(ctx context.Context, jobName string, timeout time.Duration) (map[string]interface{}, error) {
	deadline := time.Now().Add(timeout)
	pollInterval := 2 * time.Second

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		job, err := e.k8sClient.BatchV1().Jobs(e.namespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get job status: %w", err)
		}

		// Check if completed
		if job.Status.Succeeded > 0 {
			// Get logs from pod
			return e.getJobOutput(ctx, jobName)
		}

		// Check if failed
		if job.Status.Failed > 0 {
			logs, _ := e.getJobLogs(ctx, jobName)
			return nil, fmt.Errorf("job failed: %s", logs)
		}
	}

	return nil, fmt.Errorf("job timed out after %v", timeout)
}

const outputDelimiter = "---AGENT_OUTPUT_START---"

func (e *CodeExecutor) getJobOutput(ctx context.Context, jobName string) (map[string]interface{}, error) {
	logs, err := e.getJobLogs(ctx, jobName)
	if err != nil {
		return nil, err
	}

	// Split logs by delimiter to separate system output from app output
	systemLogs, appOutput := splitByDelimiter(logs)

	// Find the JSON line in app output
	jsonLine := findLastJSONLine(appOutput)
	if jsonLine == "" {
		result := map[string]interface{}{
			"raw":    logs,
			"status": "completed",
		}
		if systemLogs != "" {
			result["system_logs"] = systemLogs
		}
		return result, nil
	}

	// Parse JSON output
	var output map[string]interface{}
	if err := json.Unmarshal([]byte(jsonLine), &output); err != nil {
		result := map[string]interface{}{
			"raw":    logs,
			"status": "completed",
		}
		if systemLogs != "" {
			result["system_logs"] = systemLogs
		}
		return result, nil
	}

	// Extract the actual output
	if success, ok := output["success"].(bool); ok && success {
		if result, ok := output["output"]; ok {
			if resultMap, ok := result.(map[string]interface{}); ok {
				if systemLogs != "" {
					resultMap["system_logs"] = systemLogs
				}
				return resultMap, nil
			}
			res := map[string]interface{}{"result": result}
			if systemLogs != "" {
				res["system_logs"] = systemLogs
			}
			return res, nil
		}
	}

	if systemLogs != "" {
		output["system_logs"] = systemLogs
	}
	return output, nil
}

// splitByDelimiter splits logs into system output (before delimiter) and app output (after)
func splitByDelimiter(logs string) (systemLogs, appOutput string) {
	parts := strings.SplitN(logs, outputDelimiter, 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return "", logs
}

// findLastJSONLine finds JSON in the output, handling embedded newlines
func findLastJSONLine(logs string) string {
	logs = strings.TrimSpace(logs)

	// Try to find a complete JSON object by counting braces
	if !strings.HasPrefix(logs, "{") {
		return ""
	}

	// Find the end of the JSON by matching braces
	depth := 0
	inString := false
	escape := false

	for i, ch := range logs {
		if escape {
			escape = false
			continue
		}

		switch ch {
		case '\\':
			if inString {
				escape = true
			}
		case '"':
			inString = !inString
		case '{':
			if !inString {
				depth++
			}
		case '}':
			if !inString {
				depth--
				if depth == 0 {
					// Found complete JSON
					return logs[:i+1]
				}
			}
		}
	}

	// Fallback: try the old line-by-line approach
	lines := strings.Split(logs, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			return line
		}
	}
	return ""
}

func (e *CodeExecutor) getJobLogs(ctx context.Context, jobName string) (string, error) {
	// List pods for this job
	pods, err := e.k8sClient.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})
	if err != nil || len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found for job")
	}

	// Get logs from the first pod
	podName := pods.Items[0].Name
	req := e.k8sClient.CoreV1().Pods(e.namespace).GetLogs(podName, &corev1.PodLogOptions{})
	logs, err := req.Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("failed to get logs: %w", err)
	}

	return string(logs), nil
}

func (e *CodeExecutor) deleteJob(ctx context.Context, jobName string) {
	propagation := metav1.DeletePropagationBackground
	_ = e.k8sClient.BatchV1().Jobs(e.namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
}

// Helper functions

func joinRequirements(reqs []string) string {
	result := ""
	for i, r := range reqs {
		if i > 0 {
			result += " "
		}
		result += r
	}
	return result
}

func escapeForPython(s string) string {
	// Escape single quotes and backslashes
	result := ""
	for _, c := range s {
		if c == '\'' {
			result += "\\'"
		} else if c == '\\' {
			result += "\\\\"
		} else {
			result += string(c)
		}
	}
	return result
}

func escapeForShell(s string) string {
	// Escape single quotes for shell
	result := ""
	for _, c := range s {
		if c == '\'' {
			result += "'\"'\"'"
		} else {
			result += string(c)
		}
	}
	return result
}

func mustParseQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

// sanitizeK8sName converts a name to lowercase RFC 1123 compatible format
func sanitizeK8sName(name string) string {
	result := strings.ToLower(name)
	var sb strings.Builder
	for _, c := range result {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			sb.WriteRune(c)
		} else {
			sb.WriteRune('-')
		}
	}
	result = strings.Trim(sb.String(), "-")
	if result == "" {
		result = "code"
	}
	return result
}

// Streaming marker for parsing emit() calls
const streamMarker = "---AGENT_STREAM---"

// SupportsStreaming returns true if the code executor can stream with the given config
// This implements StreamingExecutor for UI streaming updates
func (e *CodeExecutor) SupportsStreaming(config map[string]interface{}) bool {
	// Check if streaming is explicitly disabled
	if stream, ok := config["stream"].(bool); ok && !stream {
		return false
	}
	// Code executor supports streaming when we have a k8s client
	return e.k8sClient != nil
}

// SupportsStreamingOutput returns true if this executor can produce streaming output
// This implements StreamProducer for inter-node streaming
func (e *CodeExecutor) SupportsStreamingOutput(config map[string]interface{}) bool {
	// Code nodes can always produce streaming output via emit()
	// emit() works via HTTP POST and doesn't require k8s client
	return true
}

// ExecuteStreaming runs the code step with streaming, calling onStream for each emit() call
func (e *CodeExecutor) ExecuteStreaming(ctx context.Context, step *StepDefinition, resolver TemplateResolver, onStream StreamCallback) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("code step requires config")
	}

	// Get language (default: python)
	language := "python"
	if l, ok := config["language"].(string); ok {
		language = l
	}

	// Only Python supported for now
	if language != "python" {
		return nil, fmt.Errorf("unsupported language: %s (only python is supported)", language)
	}

	// Get code (required)
	codeTemplate, ok := config["code"].(string)
	if !ok || codeTemplate == "" {
		return nil, fmt.Errorf("code step requires 'code'")
	}
	code := resolver.ResolveString(codeTemplate)

	// Get timeout (default: 5 minutes)
	timeout := 300
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeout = int(t)
	}

	// Get requirements
	var requirements []string
	if reqs, ok := config["requirements"].([]interface{}); ok {
		for _, r := range reqs {
			if s, ok := r.(string); ok {
				requirements = append(requirements, s)
			}
		}
	}

	// Build context data for the code to access
	var contextData map[string]interface{}
	if cp, ok := resolver.(ContextProvider); ok {
		contextData = cp.GetContextData()
	} else {
		contextData = make(map[string]interface{})
	}

	// Ensure all context values are properly initialized
	if contextData["bindings"] == nil {
		contextData["bindings"] = make(map[string]interface{})
	}
	if contextData["trigger"] == nil {
		contextData["trigger"] = make(map[string]interface{})
	}
	if contextData["prev"] == nil {
		contextData["prev"] = make(map[string]interface{})
	}
	if contextData["nodes"] == nil {
		contextData["nodes"] = make(map[string]interface{})
	}
	if contextData["vars"] == nil {
		contextData["vars"] = make(map[string]interface{})
	}
	if contextData["run"] == nil {
		contextData["run"] = make(map[string]interface{})
	}

	// Discover connected tools from the graph
	connectedTools, executionToken := e.discoverAndSetupTools(ctx, step, resolver, contextData)

	// Extract runID and build tools context if tools are available
	runID := extractRunID(contextData)
	if toolsContext := e.buildToolsContext(runID, step.Id, connectedTools, executionToken); toolsContext != nil {
		contextData["_tools"] = toolsContext
		fmt.Printf("DEBUG: ExecuteStreaming added %d tools to context for node %s with URL=%s\n",
			len(connectedTools), step.Id, toolsContext["api_base"])
	} else {
		fmt.Printf("DEBUG: ExecuteStreaming no tools added to context for node %s (tools=%d, hasToken=%v)\n",
			step.Id, len(connectedTools), executionToken != "")
	}

	contextJSON, err := json.Marshal(contextData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal context data: %w", err)
	}

	// Generate unique job name
	safeName := sanitizeK8sName(step.Name)
	jobName := fmt.Sprintf("agent-code-%s-%d", safeName, time.Now().Unix())
	if len(jobName) > 63 {
		jobName = jobName[:63]
	}

	// Upload context if uploader is available
	var contextURL string
	if e.contextUploader != nil {
		url, err := e.contextUploader.UploadContext(ctx, contextJSON)
		if err == nil {
			contextURL = url
			contextJSON = []byte("{}")
		}
	}

	// Build and submit job
	job := e.buildJob(jobName, code, requirements, timeout, string(contextJSON), contextURL)
	createdJob, err := e.k8sClient.BatchV1().Jobs(e.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	// Wait for job with streaming
	output, err := e.waitForJobWithStreaming(ctx, createdJob.Name, time.Duration(timeout)*time.Second, onStream)
	if err != nil {
		// Don't delete job on error - keep for debugging, TTL will clean up
		return nil, err
	}

	// Job completed successfully - let TTL handle cleanup for pod inspection
	return &StepResult{
		Output: output,
	}, nil
}

// waitForJobWithStreaming waits for a job to complete while streaming emit() outputs
func (e *CodeExecutor) waitForJobWithStreaming(ctx context.Context, jobName string, timeout time.Duration, onStream StreamCallback) (map[string]interface{}, error) {
	deadline := time.Now().Add(timeout)
	pollInterval := 500 * time.Millisecond // Poll frequently for streaming
	var lastLogOffset int
	firstPoll := true

	for time.Now().Before(deadline) {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Wait before polling (skip on first iteration for faster response)
		if !firstPoll {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(pollInterval):
			}
		}
		firstPoll = false

		job, err := e.k8sClient.BatchV1().Jobs(e.namespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get job status: %w", err)
		}

		// Try to get current logs and parse stream markers
		if onStream != nil {
			logs, err := e.getJobLogs(ctx, jobName)
			if err == nil && len(logs) > lastLogOffset {
				// Parse new log content for stream markers
				newContent := logs[lastLogOffset:]
				lastLogOffset = len(logs)
				e.parseAndEmitStreamUpdates(newContent, onStream)
			}
		}

		// Check if completed
		if job.Status.Succeeded > 0 {
			// Final log poll to catch any remaining stream markers
			if onStream != nil {
				logs, err := e.getJobLogs(ctx, jobName)
				if err == nil && len(logs) > lastLogOffset {
					newContent := logs[lastLogOffset:]
					e.parseAndEmitStreamUpdates(newContent, onStream)
				}
			}
			return e.getJobOutput(ctx, jobName)
		}

		// Check if failed
		if job.Status.Failed > 0 {
			logs, _ := e.getJobLogs(ctx, jobName)
			return nil, fmt.Errorf("job failed: %s", logs)
		}
	}

	return nil, fmt.Errorf("job timed out after %v", timeout)
}

// parseAndEmitStreamUpdates parses log content for stream markers and emits updates
func (e *CodeExecutor) parseAndEmitStreamUpdates(content string, onStream StreamCallback) {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, streamMarker) {
			jsonData := strings.TrimPrefix(line, streamMarker)
			var payload struct {
				Data     interface{} `json:"data"`
				Progress float64     `json:"progress"`
			}
			if err := json.Unmarshal([]byte(jsonData), &payload); err == nil {
				onStream(&StreamUpdate{
					Partial:  payload.Data,
					Progress: payload.Progress,
				})
			}
		}
	}
}

// discoverAndSetupTools discovers connected tools from the graph and sets up execution context
// Returns the connected tools and execution token if successful
func (e *CodeExecutor) discoverAndSetupTools(ctx context.Context, step *StepDefinition, resolver TemplateResolver, contextData map[string]interface{}) ([]*ToolDefinition, string) {
	var connectedTools []*ToolDefinition
	var executionToken string

	gp, ok := resolver.(GraphProvider)
	if !ok {
		fmt.Printf("DEBUG: Resolver does not implement GraphProvider for node %s\n", step.Id)
		return nil, ""
	}

	graph := gp.GetGraph()
	if graph == nil || step.Id == "" {
		fmt.Printf("DEBUG: No graph or empty step ID for node %s\n", step.Id)
		return nil, ""
	}

	toolNodes := graph.GetConnectedTools(step.Id)
	fmt.Printf("DEBUG: Code executor for node %s (name=%s) found %d tool nodes\n", step.Id, step.Name, len(toolNodes))

	if len(toolNodes) == 0 {
		return nil, ""
	}

	// Build tool definitions from nodes
	for _, toolNode := range toolNodes {
		fmt.Printf("DEBUG: Processing tool node: type=%s, id=%s, name=%s\n", toolNode.Type, toolNode.Id, toolNode.Name)
		toolDef := e.buildToolDefinitionFromNode(toolNode, resolver)
		if toolDef != nil {
			fmt.Printf("DEBUG: Built tool definition: name=%s, type=%s\n", toolDef.Name, toolDef.Config["type"])
			connectedTools = append(connectedTools, toolDef)
		} else {
			fmt.Printf("DEBUG: Failed to build tool definition for node %s\n", toolNode.Id)
		}
	}

	// Generate execution token if tools are present and dependencies are configured
	if len(connectedTools) == 0 || e.tokenService == nil || e.contextStore == nil {
		fmt.Printf("DEBUG: Skipping token generation (tools=%d, hasTokenService=%v, hasContextStore=%v)\n",
			len(connectedTools), e.tokenService != nil, e.contextStore != nil)
		return connectedTools, ""
	}

	// Extract runID from context (handles string, int, and float64 types)
	runID := extractRunID(contextData)
	if runID == "" {
		fmt.Printf("DEBUG: No valid runID found in context for node %s\n", step.Id)
		return connectedTools, ""
	}

	// Generate token
	token, err := e.tokenService.GenerateToken(runID, step.Id)
	if err != nil {
		fmt.Printf("DEBUG: Failed to generate token for node %s: %v\n", step.Id, err)
		return connectedTools, ""
	}

	executionToken = token

	// Store execution context
	execCtx := &ToolExecutionContext{
		RunID:            runID,
		NodeID:           step.Id,
		Tools:            connectedTools,
		ToolRegistry:     e.toolRegistry,
		TemplateResolver: resolver,
	}
	_ = e.contextStore.Store(execCtx)

	fmt.Printf("DEBUG: Successfully set up %d tools with token for node %s\n", len(connectedTools), step.Id)
	return connectedTools, executionToken
}

// buildToolDefinitionFromNode creates a ToolDefinition from a tool node's config
// This is similar to AI executor's method but adapted for code executor needs
func (e *CodeExecutor) buildToolDefinitionFromNode(node *NodeDefinition, resolver TemplateResolver) *ToolDefinition {
	if node == nil || node.Config == nil {
		return nil
	}
	config := node.Config

	toolName, _ := config["toolName"].(string)
	if toolName == "" {
		toolName = "tool"
	}

	toolDesc, _ := config["toolDescription"].(string)

	// Determine tool type based on node type
	nodeType := node.Type
	toolType := strings.TrimPrefix(nodeType, "tool_") // "tool_pgvector" -> "pgvector"

	// Build tool config from node config
	toolConfig := map[string]interface{}{
		"type": toolType,
	}

	// Copy relevant config fields (excluding name and description)
	for k, v := range config {
		if k != "toolName" && k != "toolDescription" {
			toolConfig[k] = v
		}
	}

	// Map pgvector to vector_search for the tool registry
	if toolType == "pgvector" {
		toolConfig["type"] = "vector_search"
	}

	return &ToolDefinition{
		Name:        toolName,
		Description: toolDesc,
		Config:      toolConfig,
	}
}

// convertToolsToSDKFormat converts ToolDefinitions to a format suitable for the Python SDK
// Returns an array of tool info with name and description for SDK injection
func (e *CodeExecutor) convertToolsToSDKFormat(tools []*ToolDefinition) []map[string]interface{} {
	result := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		result[i] = map[string]interface{}{
			"name":        tool.Name,
			"description": tool.Description,
		}
	}
	return result
}

// boolPtr returns a pointer to a bool value
func boolPtr(b bool) *bool {
	return &b
}
