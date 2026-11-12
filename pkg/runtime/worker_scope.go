package runtime

import (
	"context"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

// WorkerScopeSource returns the currently active isolation scopes. Embedding
// hosts own tenancy and lifecycle; OpenSeal never guesses or falls back to a
// process-wide scope when discovery fails.
type WorkerScopeSource interface {
	ListWorkerScopes(context.Context) ([]models.Scope, error)
}

type WorkerScopeSourceFunc func(context.Context) ([]models.Scope, error)

func (f WorkerScopeSourceFunc) ListWorkerScopes(ctx context.Context) ([]models.Scope, error) {
	return f(ctx)
}

func workerScopeKey(scope models.Scope) string {
	return strings.TrimSpace(scope.Kind) + "\x00" + strings.TrimSpace(scope.ID)
}
