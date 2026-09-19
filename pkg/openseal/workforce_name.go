package openseal

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/authoring"
)

func (e *Engine) RenameWorkforceDraftAgent(ctx context.Context, scope SkillScope, id string, revision int64, name string, actor WorkforceChangeSetActor) (*authoring.ChangeSet, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, errors.New("workforce authoring is unavailable")
	}
	return e.authoringChanges.RenameAgent(ctx, scope, id, revision, name, actor)
}

func WorkforceDraftAgentNameEditable(current *authoring.ChangeSet) bool {
	return authoring.DraftAgentNameEditable(current)
}
