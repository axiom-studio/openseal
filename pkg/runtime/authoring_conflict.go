package runtime

import (
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/authoring"
)

func workforceAgentDeploymentIdentityConflict(deploymentID string) error {
	return fmt.Errorf(
		"%w: Agent deployment identity %q already exists; use a different identity or amend the existing Agent",
		authoring.ErrChangeSetPlacementConflict,
		strings.TrimSpace(deploymentID),
	)
}
