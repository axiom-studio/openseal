package runtime

import (
	"context"
	"strings"
)

// WorkerScopeSource returns the currently active isolation scopes. Embedding
// hosts own tenancy and lifecycle; OpenSeal never guesses or falls back to a
// process-wide scope when discovery fails.
type WorkerScopeSource interface {
	ListWorkerScopes(context.Context) ([]Scope, error)
}

type WorkerScopeSourceFunc func(context.Context) ([]Scope, error)

func (f WorkerScopeSourceFunc) ListWorkerScopes(ctx context.Context) ([]Scope, error) {
	return f(ctx)
}

func workerScopeKey(scope Scope) string {
	return strings.TrimSpace(scope.Kind) + "\x00" + strings.TrimSpace(scope.ID)
}
