package executor

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/types"
)

// mockK8sClient implements K8sClient for testing.
type mockK8sClient struct{}

func (m *mockK8sClient) GetResource(ctx context.Context, clusterId int, namespace, name, kind string) (map[string]interface{}, error) {
	return nil, nil
}
func (m *mockK8sClient) ListResources(ctx context.Context, clusterId int, namespace, kind, labelSelector, fieldSelector string) ([]map[string]interface{}, error) {
	return nil, nil
}
func (m *mockK8sClient) DeleteResource(ctx context.Context, clusterId int, namespace, name, kind string) error {
	return nil
}
func (m *mockK8sClient) UpdateResource(ctx context.Context, clusterId int, namespace, name, kind string, patch map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}
func (m *mockK8sClient) GetPodLogs(ctx context.Context, clusterId int, namespace, podName, containerName string, tailLines int, sinceSeconds int) (string, error) {
	return "", nil
}
func (m *mockK8sClient) ListEvents(ctx context.Context, clusterId int, namespace, resourceKind, resourceName string) ([]map[string]interface{}, error) {
	return nil, nil
}
func (m *mockK8sClient) RestartResource(ctx context.Context, clusterId int, namespace, name, kind string) error {
	return nil
}

// TestK8sClientInterfaceExists verifies the generic K8sClient interface is
// defined in this package.
func TestK8sClientInterfaceExists(t *testing.T) {
	// Compile-time check: ensure mockK8sClient implements K8sClient.
	var _ K8sClient = (*mockK8sClient)(nil)

	// Verify each executor constructor accepts the generic interface.
	_ = NewK8sGetExecutor(nil)
	_ = NewK8sListExecutor(nil)
	_ = NewK8sLogsExecutor(nil)
	_ = NewK8sEventsExecutor(nil)
	_ = NewK8sRestartExecutor(nil)
	_ = NewK8sScaleExecutor(nil)
	_ = NewK8sPatchExecutor(nil)
	_ = NewK8sDeleteExecutor(nil)
}

// TestK8sClientNilSafety verifies all K8s executors return a clean error
// when the K8sClient is nil rather than panicking.
func TestK8sClientNilSafety(t *testing.T) {
	resolver := &k8sTestResolver{}
	ctx := context.Background()

	cases := []struct {
		name     string
		executor StepExecutor
		config   map[string]interface{}
	}{
		{
			name:     "k8s-get",
			executor: NewK8sGetExecutor(nil),
			config:   map[string]interface{}{"kind": "Pod", "name": "test", "namespace": "default"},
		},
		{
			name:     "k8s-list",
			executor: NewK8sListExecutor(nil),
			config:   map[string]interface{}{"kind": "Pod", "namespace": "default"},
		},
		{
			name:     "k8s-logs",
			executor: NewK8sLogsExecutor(nil),
			config:   map[string]interface{}{"podName": "test", "namespace": "default"},
		},
		{
			name:     "k8s-events",
			executor: NewK8sEventsExecutor(nil),
			config:   map[string]interface{}{"namespace": "default"},
		},
		{
			name:     "k8s-restart",
			executor: NewK8sRestartExecutor(nil),
			config:   map[string]interface{}{"kind": "Deployment", "name": "test", "namespace": "default"},
		},
		{
			name:     "k8s-scale",
			executor: NewK8sScaleExecutor(nil),
			config:   map[string]interface{}{"kind": "Deployment", "name": "test", "namespace": "default", "replicas": float64(3)},
		},
		{
			name:     "k8s-patch",
			executor: NewK8sPatchExecutor(nil),
			config:   map[string]interface{}{"kind": "Deployment", "name": "test", "namespace": "default", "patch": map[string]interface{}{"foo": "bar"}},
		},
		{
			name:     "k8s-delete",
			executor: NewK8sDeleteExecutor(nil),
			config:   map[string]interface{}{"kind": "Pod", "name": "test", "namespace": "default"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.executor.Execute(ctx, &StepDefinition{Config: tc.config}, resolver)
			if err == nil {
				t.Fatalf("expected error when k8sClient is nil, got nil")
			}
			if !strings.Contains(err.Error(), "k8s client not configured") {
				t.Fatalf("expected 'k8s client not configured' error, got: %v", err)
			}
		})
	}
}

// TestInvokeAgentRemoved verifies the invoke_agent node type is no longer
// registered in the default executor registry.
func TestInvokeAgentRemoved(t *testing.T) {
	reg := NewRegistry(nil)
	if reg.HasExecutor("invoke_agent") {
		t.Fatal("invoke_agent executor should not be registered in OpenSeal")
	}
}

// TestAgentOrchestratorInTypes verifies the unified AgentOrchestrator
// interface lives in pkg/types and includes the full method set.
func TestAgentOrchestratorInTypes(t *testing.T) {
	// Compile-time check: types.AgentOrchestrator must have at least the
	// methods that pkg/runtime/tools.go depends on.
	var _ types.AgentOrchestrator = (*stubOrchestrator)(nil)
}

// TestFileStoreInTypes verifies FileStore is defined in pkg/types with the
// methods needed by the runtime (Store, Get, GetReader).
func TestFileStoreInTypes(t *testing.T) {
	var _ types.FileStore = (*stubFileStore)(nil)
}

type stubOrchestrator struct{}

func (s *stubOrchestrator) TriggerAgent(ctx context.Context, req *types.TriggerAgentRequest) (*types.AgentRunBean, error) {
	return nil, nil
}
func (s *stubOrchestrator) ExecutePipeline(ctx context.Context, runId int) error { return nil }
func (s *stubOrchestrator) GetRun(runId int) (*types.AgentRunBean, error)        { return nil, nil }
func (s *stubOrchestrator) GetRunsByInstance(instanceId int, limit int) ([]*types.AgentRunListBean, error) {
	return nil, nil
}
func (s *stubOrchestrator) TestWorkflow(ctx context.Context, req *types.TestWorkflowRequest) (*types.TestWorkflowResponse, error) {
	return nil, nil
}
func (s *stubOrchestrator) TestWorkflowWithCallback(ctx context.Context, req *types.TestWorkflowRequest, onNodeUpdate types.NodeUpdateCallback) (*types.TestWorkflowResponse, error) {
	return nil, nil
}
func (s *stubOrchestrator) WaitForRunCompletion(ctx context.Context, runId int, timeout time.Duration) (*types.AgentRunBean, error) {
	return nil, nil
}
func (s *stubOrchestrator) NotifyRunCompletion(runId int, status string) {}
func (s *stubOrchestrator) GetFileStore() types.FileStore                { return nil }

type stubFileStore struct{}

func (s *stubFileStore) Store(data []byte, filename string, mimeType string) (*types.StoredFile, error) {
	return nil, nil
}
func (s *stubFileStore) Get(fileId string) (*types.StoredFile, error)   { return nil, nil }
func (s *stubFileStore) GetReader(fileId string) (io.ReadCloser, error) { return nil, nil }
