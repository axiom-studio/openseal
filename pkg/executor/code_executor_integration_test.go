//go:build integration

package executor

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// These tests require a running Kubernetes cluster.
// Run with: go test -tags=integration -v ./pkg/agent/executor/ -run TestCodeExecutor

func getK8sClient(t *testing.T) kubernetes.Interface {
	// Try in-cluster config first, then fall back to kubeconfig
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Skipf("Skipping integration test: cannot build k8s config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Skipf("Skipping integration test: cannot create k8s client: %v", err)
	}

	return clientset
}

func TestCodeExecutor_Integration_BasicExecution(t *testing.T) {
	client := getK8sClient(t)

	executor := NewCodeExecutor(client, &CodeExecutorConfig{
		Namespace:      "default",
		RunnerImage:    "python:3.11-slim",
		ServiceAccount: "default",
	})

	// Create a simple resolver with context data
	resolver := &simpleResolver{
		input: map[string]interface{}{
			"bindings": map[string]interface{}{
				"apiKey": "test-key-123",
			},
			"trigger": map[string]interface{}{
				"type": "test",
				"data": map[string]interface{}{
					"message": "Hello from trigger",
				},
			},
			"prev": map[string]interface{}{
				"input": "test-pdf-data",
			},
		},
		nodeOutputs: map[string]interface{}{
			"Webhook": map[string]interface{}{
				"input": "test-pdf-data",
			},
		},
		variables: map[string]interface{}{
			"counter": 42,
		},
		runMetadata: map[string]interface{}{
			"id": 999,
		},
	}

	step := &StepDefinition{
		Name: "test-code-node",
		Type: StepTypeCode,
		Config: map[string]interface{}{
			"language": "python",
			"code": `
# Test that all context variables are accessible
result = {
    "bindings_apiKey": bindings.get("apiKey"),
    "trigger_type": trigger.get("type"),
    "trigger_message": trigger.get("data", {}).get("message"),
    "prev_input": prev.get("input"),
    "nodes_webhook": nodes.get("Webhook", {}).get("input"),
    "vars_counter": vars.get("counter"),
    "run_id": run.get("id"),
}
`,
			"timeout": 60,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	result, err := executor.Execute(ctx, step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.Output

	// Verify all context values are accessible
	checks := []struct {
		key      string
		expected interface{}
	}{
		{"bindings_apiKey", "test-key-123"},
		{"trigger_type", "test"},
		{"trigger_message", "Hello from trigger"},
		{"prev_input", "test-pdf-data"},
		{"nodes_webhook", "test-pdf-data"},
		{"vars_counter", float64(42)}, // JSON numbers are float64
		{"run_id", float64(999)},
	}

	for _, check := range checks {
		if output[check.key] != check.expected {
			t.Errorf("Expected %s=%v, got %v", check.key, check.expected, output[check.key])
		}
	}

	// Intentional bug: wrong expected value to make test fail
	if output["bindings_apiKey"] != "wrong-key-value" {
		t.Errorf("BUG: Expected bindings_apiKey='wrong-key-value', got %v", output["bindings_apiKey"])
	}
}

func TestCodeExecutor_Integration_PrevFromTrigger(t *testing.T) {
	// This test simulates the exact scenario: trigger -> code node
	// where the code node accesses prev["input"]
	client := getK8sClient(t)

	executor := NewCodeExecutor(client, &CodeExecutorConfig{
		Namespace:      "default",
		RunnerImage:    "python:3.11-slim",
		ServiceAccount: "default",
	})

	// Simulate what the pipeline executor would provide:
	// - trigger data comes from webhook
	// - prev is the output of the trigger node (which is trigger.data)
	triggerOutput := map[string]interface{}{
		"input": "JVBERi0xLjQKbase64pdfdata",
	}

	resolver := &simpleResolver{
		input: map[string]interface{}{
			"bindings": map[string]interface{}{},
			"trigger": map[string]interface{}{
				"type": "webhook",
				"data": triggerOutput,
			},
			"prev": triggerOutput, // This is what pipeline executor sets for code node
		},
		nodeOutputs: map[string]interface{}{
			"Webhook": triggerOutput,
		},
		variables:   map[string]interface{}{},
		runMetadata: map[string]interface{}{},
	}

	step := &StepDefinition{
		Name: "test-prev-access",
		Type: StepTypeCode,
		Config: map[string]interface{}{
			"language": "python",
			"code":     `result = {"in": prev["input"]}`,
			"timeout":  60,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	result, err := executor.Execute(ctx, step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.Output

	if output["in"] != "JVBERi0xLjQKbase64pdfdata" {
		t.Errorf("Expected in='JVBERi0xLjQKbase64pdfdata', got %v", output["in"])
	}
}

func TestCodeExecutor_Integration_GetContextData(t *testing.T) {
	// Test that GetContextData returns the correct structure
	resolver := &simpleResolver{
		input: map[string]interface{}{
			"bindings": map[string]interface{}{"key": "value"},
			"trigger":  map[string]interface{}{"type": "test"},
			"prev":     map[string]interface{}{"data": "from-prev"},
		},
		nodeOutputs: map[string]interface{}{
			"Node1": map[string]interface{}{"output": "data1"},
		},
		variables: map[string]interface{}{
			"var1": "value1",
		},
		runMetadata: map[string]interface{}{
			"id": 123,
		},
	}

	ctx := resolver.GetContextData()

	// Verify structure
	bindings := ctx["bindings"].(map[string]interface{})
	if bindings["key"] != "value" {
		t.Errorf("Expected bindings.key='value', got %v", bindings["key"])
	}

	trigger := ctx["trigger"].(map[string]interface{})
	if trigger["type"] != "test" {
		t.Errorf("Expected trigger.type='test', got %v", trigger["type"])
	}

	prev := ctx["prev"].(map[string]interface{})
	if prev["data"] != "from-prev" {
		t.Errorf("Expected prev.data='from-prev', got %v", prev["data"])
	}

	nodes := ctx["nodes"].(map[string]interface{})
	node1 := nodes["Node1"].(map[string]interface{})
	if node1["output"] != "data1" {
		t.Errorf("Expected nodes.Node1.output='data1', got %v", node1["output"])
	}

	vars := ctx["vars"].(map[string]interface{})
	if vars["var1"] != "value1" {
		t.Errorf("Expected vars.var1='value1', got %v", vars["var1"])
	}

	run := ctx["run"].(map[string]interface{})
	if run["id"] != 123 {
		t.Errorf("Expected run.id=123, got %v", run["id"])
	}

	// Verify JSON serialization works
	jsonBytes, err := json.Marshal(ctx)
	if err != nil {
		t.Fatalf("Failed to marshal context: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal context: %v", err)
	}

	t.Logf("Context JSON: %s", string(jsonBytes))
}
