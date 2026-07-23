package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestServerExposesKernelAPIWithoutBrowserFallback(t *testing.T) {
	server := NewServer(nil, runtime.NewMemoryStore(10), zap.NewNop().Sugar())

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", health.Code, http.StatusOK)
	}

	root := httptest.NewRecorder()
	server.Handler().ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusNotFound {
		t.Fatalf("root status = %d, want %d", root.Code, http.StatusNotFound)
	}
}

func TestServerDoesNotExposeLegacyWorkflowRuntime(t *testing.T) {
	api := NewServer(nil, runtime.NewMemoryStore(10), zap.NewNop().Sugar())
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/workflows"},
		{http.MethodGet, "/api/v1/workflows/legacy"},
		{http.MethodPost, "/api/v1/workflows"},
		{http.MethodPost, "/api/v1/workflows/validate"},
		{http.MethodGet, "/api/v1/workflows/legacy/hcl"},
		{http.MethodPost, "/api/v1/workflows/legacy/run"},
		{http.MethodGet, "/api/v1/runs"},
		{http.MethodGet, "/api/v1/runs/1"},
	} {
		response := httptest.NewRecorder()
		api.Handler().ServeHTTP(response, httptest.NewRequest(route.method, route.path, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want %d", route.method, route.path, response.Code, http.StatusNotFound)
		}
	}

	canonical := httptest.NewRecorder()
	api.Handler().ServeHTTP(canonical, httptest.NewRequest(http.MethodGet, "/api/v1/agent-runs?scopeKind=local&scopeId=default", nil))
	if canonical.Code != http.StatusOK {
		t.Fatalf("canonical Agent Runs status = %d, want %d", canonical.Code, http.StatusOK)
	}
}
