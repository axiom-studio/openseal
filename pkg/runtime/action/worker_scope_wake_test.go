package action

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
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
// nothing. It is a cut-down copy of runtime's newTestActionPool, which this
// test cannot see once it lives with the action code.
func newScopeWakeTestPool(t *testing.T, scope models.Scope) *ActionWorkerPool {
	t.Helper()
	pool, err := NewActionWorkerPool(newActionTestStore(t), skill.NewCatalog(), CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "test-secret"}, nil
	}), ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) { return nil, nil }), nil,
		ActionWorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Hour, LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
