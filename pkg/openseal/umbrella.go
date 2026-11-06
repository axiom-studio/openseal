package openseal

import (
	"context"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// ResolveUmbrella returns the Agent or Team that owns deployment id. Agent
// and Team IDs share one namespace, so the answer is never ambiguous.
func (e *Engine) ResolveUmbrella(ctx context.Context, scope skill.ScopeReference, id string) (ObjectiveOwner, error) {
	if e == nil || e.agents == nil || e.agents.DeploymentIDs() == nil {
		return ObjectiveOwner{}, errors.New("deployment ID namespace is not configured")
	}
	kind, err := e.agents.DeploymentIDs().DeploymentIDKind(ctx, scope, id)
	if err != nil {
		return ObjectiveOwner{}, err
	}
	switch kind {
	case workforce.DeploymentKindAgent:
		return ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: id}, nil
	case workforce.DeploymentKindTeam:
		return ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: id}, nil
	}
	return ObjectiveOwner{}, fmt.Errorf("deployment ID %s has unknown kind %q", id, kind)
}
