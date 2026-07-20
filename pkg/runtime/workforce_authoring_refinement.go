package runtime

import (
	"context"
	"strings"

	"github.com/axiom-studio/openseal/pkg/authoring"
)

// AnswerRefinement persists the answer before scheduling generation. The
// canonical Run is idempotent for the refinement attempt, and RecoverPending
// closes the crash window between these two durable writes.
func (s *WorkforceAuthoringRunService) AnswerRefinement(ctx context.Context, request authoring.AnswerChangeSetRefinementRequest) (*authoring.ChangeSet, *AgentRun, bool, error) {
	changeSet, replayed, err := s.changeSets.AnswerRefinement(ctx, request)
	if err != nil {
		return nil, nil, false, err
	}
	if replayed && changeSet.Status != authoring.ChangeSetEvaluating {
		var run *AgentRun
		if changeSet.Generation != nil && strings.TrimSpace(changeSet.Generation.RunID) != "" {
			run, _ = s.store.GetAgentRun(ctx, Scope{Kind: changeSet.Scope.Kind, ID: changeSet.Scope.ID}, changeSet.Generation.RunID)
		}
		return changeSet, run, true, nil
	}
	run, err := s.Enqueue(ctx, changeSet)
	if err != nil {
		return changeSet, nil, replayed, err
	}
	changeSet, err = s.changeSets.Get(ctx, changeSet.Scope, changeSet.ID)
	return changeSet, run, replayed, err
}
