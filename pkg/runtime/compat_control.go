// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/control. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/control, kept so that importers of
// pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

type ControlAuthorityInput = control.ControlAuthorityInput

// ResolveControlAuthority forwards to control.ResolveControlAuthority.
func ResolveControlAuthority(ctx context.Context, in control.ControlAuthorityInput) (agentrun.ControlAuthority, error) {
	return control.ResolveControlAuthority(ctx, in)
}

type DelegationSpec = control.DelegationSpec

// WithFinalOutputProperties forwards to control.WithFinalOutputProperties.
func WithFinalOutputProperties(tools []models.ModelStepTool, run *models.AgentRun, publisher agentrun.FinalOutputPublisher) []models.ModelStepTool {
	return control.WithFinalOutputProperties(tools, run, publisher)
}

// FinalOutputPublicationFor forwards to control.FinalOutputPublicationFor.
func FinalOutputPublicationFor(run *models.AgentRun, answer agentrun.FinalAnswer, entry *models.SessionEntry, publisher agentrun.FinalOutputPublisher) (*agentrun.FinalOutputPublication, error) {
	return control.FinalOutputPublicationFor(run, answer, entry, publisher)
}

type ForkSpec = control.ForkSpec

// RunReactions forwards to control.RunReactions.
func RunReactions(ctx context.Context, sessions models.AgentSessionStore, run *models.AgentRun) ([]runtimeconversation.TurnReaction, error) {
	return control.RunReactions(ctx, sessions, run)
}

type ControlPhase = control.ControlPhase

const ControlPhaseInline = control.ControlPhaseInline

const ControlPhaseSettle = control.ControlPhaseSettle

// IsControlTool forwards to control.IsControlTool.
func IsControlTool(name string) bool {
	return control.IsControlTool(name)
}

// ControlToolPhase forwards to control.ControlToolPhase.
func ControlToolPhase(name string) (control.ControlPhase, bool) {
	return control.ControlToolPhase(name)
}

type ControlToolSpec = control.ControlToolSpec

const ControlOriginModel = control.ControlOriginModel

const ControlOriginKernel = control.ControlOriginKernel

// NewControlContext forwards to control.NewControlContext.
func NewControlContext(in agentrun.StepToolsInput, entry *models.SessionEntry) agentrun.ControlContext {
	return control.NewControlContext(in, entry)
}

type ControlEntryMeta = control.ControlEntryMeta

// DecodeControlEntryMeta forwards to control.DecodeControlEntryMeta.
func DecodeControlEntryMeta(entry *models.SessionEntry) (*control.ControlEntryMeta, error) {
	return control.DecodeControlEntryMeta(entry)
}

type ControlOutcomeKind = control.ControlOutcomeKind

const ControlContinue = control.ControlContinue

const ControlParked = control.ControlParked

const ControlFinal = control.ControlFinal

const ControlEnded = control.ControlEnded

const ControlFailed = control.ControlFailed

type ControlResult = control.ControlResult

type ControlResume = control.ControlResume

type ControlExecutor = control.ControlExecutor

var ErrControlEntryState = control.ErrControlEntryState

// SkillSetupPendingSourceKey forwards to control.SkillSetupPendingSourceKey.
func SkillSetupPendingSourceKey(runID, requestID string) string {
	return control.SkillSetupPendingSourceKey(runID, requestID)
}
