package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestServerExposesKernelAPIWithoutBrowserFallback(t *testing.T) {
	server := NewServer(nil, nil, runtime.NewMemoryStore(10), zap.NewNop().Sugar())

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
