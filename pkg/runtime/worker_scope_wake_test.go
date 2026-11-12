package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestWorkerPoolScopeWakeDoesNotWakeAnotherTenant(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "mine"}
	pool := newTestActionPool(t, newTestKernelStore(t), skill.NewCatalog(), scope, time.Hour, 1,
		ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) { return nil, nil }))
	pool.WakeScope(Scope{Kind: "tenant", ID: "other"})
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
