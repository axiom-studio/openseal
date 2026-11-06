package workforce

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// DeploymentKind names the registry that owns a deployment ID.
type DeploymentKind string

const (
	DeploymentKindAgent DeploymentKind = "agent"
	DeploymentKindTeam  DeploymentKind = "team"
)

var (
	// ErrDeploymentIDTaken means the ID is already reserved by the other kind.
	ErrDeploymentIDTaken = errors.New("deployment ID is already used by another Agent or Team")
	// ErrDeploymentIDNotReserved means no Agent or Team ever used the ID.
	ErrDeploymentIDNotReserved = errors.New("deployment ID is not reserved")
)

// DeploymentIDReserver keeps Agent and Team deployment IDs in one namespace
// per scope. A reservation is permanent: deleting or archiving a deployment
// keeps it, so an ID never names two umbrellas. Reserving an ID again for the
// same kind succeeds; reserving it for the other kind is ErrDeploymentIDTaken.
type DeploymentIDReserver interface {
	ReserveDeploymentID(ctx context.Context, scope capability.ScopeReference, id string, kind DeploymentKind) error
	DeploymentIDKind(ctx context.Context, scope capability.ScopeReference, id string) (DeploymentKind, error)
}

// ValidDeploymentReservation checks the arguments every reserver accepts.
func ValidDeploymentReservation(scope capability.ScopeReference, id string, kind DeploymentKind) error {
	if strings.TrimSpace(scope.Kind) == "" || strings.TrimSpace(scope.ID) == "" || strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id {
		return errors.New("deployment ID reservation requires a scope and an exact ID")
	}
	if kind != DeploymentKindAgent && kind != DeploymentKindTeam {
		return errors.New("deployment ID reservation kind must be agent or team")
	}
	return nil
}

// MemoryDeploymentIDs is an in-process DeploymentIDReserver.
type MemoryDeploymentIDs struct {
	mu  sync.Mutex
	ids map[string]DeploymentKind
}

func NewMemoryDeploymentIDs() *MemoryDeploymentIDs {
	return &MemoryDeploymentIDs{ids: make(map[string]DeploymentKind)}
}

func memoryDeploymentIDKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}

func (m *MemoryDeploymentIDs) ReserveDeploymentID(_ context.Context, scope capability.ScopeReference, id string, kind DeploymentKind) error {
	if err := ValidDeploymentReservation(scope, id, kind); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryDeploymentIDKey(scope, id)
	if existing, ok := m.ids[key]; ok && existing != kind {
		return ErrDeploymentIDTaken
	}
	m.ids[key] = kind
	return nil
}

func (m *MemoryDeploymentIDs) DeploymentIDKind(_ context.Context, scope capability.ScopeReference, id string) (DeploymentKind, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kind, ok := m.ids[memoryDeploymentIDKey(scope, id)]
	if !ok {
		return "", ErrDeploymentIDNotReserved
	}
	return kind, nil
}
