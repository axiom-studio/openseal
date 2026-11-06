package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

var _ workforce.DeploymentIDReserver = (*MemoryStore)(nil)

func (s *MemoryStore) ReserveDeploymentID(ctx context.Context, scope capability.ScopeReference, id string, kind workforce.DeploymentKind) error {
	return s.deploymentIDs.ReserveDeploymentID(ctx, scope, id, kind)
}

func (s *MemoryStore) DeploymentIDKind(ctx context.Context, scope capability.ScopeReference, id string) (workforce.DeploymentKind, error) {
	return s.deploymentIDs.DeploymentIDKind(ctx, scope, id)
}
