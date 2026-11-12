package action

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestWorkerPoolScopeWakeDoesNotWakeAnotherTenant(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "mine"}
	pool := newScopeWakeTestPool(t, scope)
	pool.WakeScope(models.Scope{Kind: "tenant", ID: "other"})
	select {
	case <-pool.wake:
		t.Fatal("scope-specific wake crossed the tenant boundary")
	default:
	}
	pool.WakeScope(scope)
	select {
	case <-pool.wake:
	default:
		t.Fatal("scope-specific wake missed its own tenant")
	}
}

// newScopeWakeTestPool is an idle action pool for scope that dispatches
// nothing. It is a cut-down copy of actiontest.NewPool, which an in-package
// test cannot import (actiontest imports action).
func newScopeWakeTestPool(t *testing.T, scope models.Scope) *WorkerPool {
	t.Helper()
	pool, err := NewWorkerPool(kerneltest.NewStore(t), skill.NewCatalog(), CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "test-secret"}, nil
	}), DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) { return nil, nil }), nil,
		WorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Hour, LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
