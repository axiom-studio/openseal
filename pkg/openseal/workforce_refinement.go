package openseal

import (
	"context"
	"errors"

	"github.com/axiom-studio/openseal/pkg/authoring"
)

type (
	WorkforceChangeSetRefinement              = authoring.ChangeSetRefinement
	WorkforceRefinementQuestion               = authoring.RefinementQuestion
	WorkforceRefinementQuestionCategory       = authoring.RefinementQuestionCategory
	WorkforceRefinementQuestionOption         = authoring.RefinementQuestionOption
	WorkforceRefinementQuestionDependency     = authoring.RefinementQuestionDependency
	WorkforceRefinementQuestionProvenance     = authoring.RefinementQuestionProvenance
	WorkforceRefinementProvenanceKind         = authoring.RefinementProvenanceKind
	WorkforceRefinementBlockingScope          = authoring.RefinementBlockingScope
	WorkforceRefinementAnswerSchema           = authoring.RefinementAnswerSchema
	WorkforceRefinementAnswerKind             = authoring.RefinementAnswerKind
	WorkforceRefinementAnswerValue            = authoring.RefinementAnswerValue
	WorkforceRefinementAnswerEvent            = authoring.RefinementAnswerEvent
	WorkforceRefinementAnswerSource           = authoring.RefinementAnswerSource
	WorkforceSkillReadiness                   = authoring.SkillReadiness
	WorkforceSkillCompatibility               = authoring.SkillCompatibility
	AnswerWorkforceChangeSetRefinementRequest = authoring.AnswerChangeSetRefinementRequest
)

const (
	WorkforceRefinementCategoryCredential  = authoring.RefinementCategoryCredential
	WorkforceRefinementCategorySkill       = authoring.RefinementCategorySkill
	WorkforceRefinementCategoryScope       = authoring.RefinementCategoryScope
	WorkforceRefinementCategoryPolicy      = authoring.RefinementCategoryPolicy
	WorkforceRefinementCategoryAuthority   = authoring.RefinementCategoryAuthority
	WorkforceRefinementCategoryDestination = authoring.RefinementCategoryDestination
	WorkforceRefinementCategoryBudget      = authoring.RefinementCategoryBudget
	WorkforceRefinementCategoryApproval    = authoring.RefinementCategoryApproval
	WorkforceRefinementCategoryOther       = authoring.RefinementCategoryOther
	WorkforceRefinementAnswerText          = authoring.RefinementAnswerText
	WorkforceRefinementAnswerStringList    = authoring.RefinementAnswerStringList
	WorkforceRefinementAnswerSingleSelect  = authoring.RefinementAnswerSingleSelect
	WorkforceRefinementAnswerMultiSelect   = authoring.RefinementAnswerMultiSelect
	WorkforceRefinementAnswerBoolean       = authoring.RefinementAnswerBoolean
	WorkforceRefinementAnswerCredential    = authoring.RefinementAnswerCredentialReference
	WorkforceRefinementAnswerSkill         = authoring.RefinementAnswerSkillSelection
	WorkforceRefinementBlocksCandidate     = authoring.RefinementBlocksCandidate
	WorkforceRefinementBlocksEvaluation    = authoring.RefinementBlocksEvaluation
	WorkforceRefinementBlocksApply         = authoring.RefinementBlocksApply
	WorkforceRefinementAnswerSourceUser    = authoring.RefinementAnswerSourceUser
	WorkforceRefinementAnswerSourceRuntime = authoring.RefinementAnswerSourceRuntime
	WorkforceSkillReady                    = authoring.SkillReadinessReady
	WorkforceSkillNeedsBinding             = authoring.SkillReadinessNeedsBinding
	WorkforceSkillNeedsInstallation        = authoring.SkillReadinessNeedsInstallation
	WorkforceSkillUnavailable              = authoring.SkillReadinessUnavailable
)

// AnswerWorkforceChangeSetRefinement appends an audited answer and schedules
// the next durable generation attempt. Hosts without the canonical Run service
// fail explicitly so an accepted answer can never strand in evaluating state.
func (e *Engine) AnswerWorkforceChangeSetRefinement(ctx context.Context, request authoring.AnswerChangeSetRefinementRequest) (*authoring.ChangeSet, bool, error) {
	if e == nil || e.authoringRuns == nil {
		return nil, false, errors.New("durable workforce authoring runs are not configured")
	}
	changeSet, _, replayed, err := e.authoringRuns.AnswerRefinement(ctx, request)
	return changeSet, replayed, err
}
