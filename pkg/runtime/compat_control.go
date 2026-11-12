// Re-exports of declarations moved to github.com/axiom-studio/openseal/pkg/runtime/control,
// kept so that importers of github.com/axiom-studio/openseal/pkg/runtime continue to compile.

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

// DelegationIdempotencyKey forwards to control.DelegationIdempotencyKey.
func DelegationIdempotencyKey(runID, stepID string) string {
	return control.DelegationIdempotencyKey(runID, stepID)
}

// StepIDOf forwards to control.StepIDOf.
func StepIDOf(step *models.AgentTurn) string {
	return control.StepIDOf(step)
}

// WithFinalOutputProperties forwards to control.WithFinalOutputProperties.
func WithFinalOutputProperties(tools []models.ModelStepTool, run *models.AgentRun, publisher agentrun.FinalOutputPublisher) []models.ModelStepTool {
	return control.WithFinalOutputProperties(tools, run, publisher)
}

// FinalOutputPublicationFor forwards to control.FinalOutputPublicationFor.
func FinalOutputPublicationFor(run *models.AgentRun, answer agentrun.FinalAnswer, entry *models.SessionEntry, publisher agentrun.FinalOutputPublisher) (*agentrun.FinalOutputPublication, error) {
	return control.FinalOutputPublicationFor(run, answer, entry, publisher)
}

const ActionCallPageSize = control.ActionCallPageSize

type ActionCallLister = control.ActionCallLister

type ForkSpec = control.ForkSpec

// ResultText forwards to control.ResultText.
func ResultText(result *models.ToolResult) string {
	return control.ResultText(result)
}

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

// IsFinalTool forwards to control.IsFinalTool.
func IsFinalTool(name models.ControlToolName) bool {
	return control.IsFinalTool(name)
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

type Handler = control.Handler

type Registry = control.Registry

type Tool = control.Tool

// NewTool forwards to control.NewTool.
func NewTool(name models.ControlToolName, handler control.Handler) control.Tool {
	return control.NewTool(name, handler)
}

// NewRegistry forwards to control.NewRegistry.
func NewRegistry(tools ...control.Tool) control.Registry {
	return control.NewRegistry(tools...)
}

// BuiltinTools forwards to control.BuiltinTools.
func BuiltinTools() []control.Tool {
	return control.BuiltinTools()
}

type ControlExecutor = control.ControlExecutor

var ErrControlEntryState = control.ErrControlEntryState

// WithToolResult forwards to control.WithToolResult.
func WithToolResult(entry *models.SessionEntry, to models.ToolEntryState, result *models.ToolResult) *models.SessionEntry {
	return control.WithToolResult(entry, to, result)
}

const ArgumentsInvalidCode = control.ArgumentsInvalidCode

// CallRefusals forwards to control.CallRefusals.
func CallRefusals(calls []models.ModelToolCall, authority agentrun.ControlAuthority) map[string]*models.ToolResult {
	return control.CallRefusals(calls, authority)
}

// ErrorResult forwards to control.ErrorResult.
func ErrorResult(status models.ToolResultStatus, code, message string, extra map[string]interface{}) *models.ToolResult {
	return control.ErrorResult(status, code, message, extra)
}

const LightpandaBrowserSkillID = control.LightpandaBrowserSkillID

const LiveBrowserActionPrefix = control.LiveBrowserActionPrefix

const LiveBrowserRateLimitedCode = control.LiveBrowserRateLimitedCode

const LiveBrowserRateLimitedError = control.LiveBrowserRateLimitedError

const SkillSetupPendingNote = control.SkillSetupPendingNote

type GovernedToolView = control.GovernedToolView

// LoadGovernedToolViews forwards to control.LoadGovernedToolViews.
func LoadGovernedToolViews(ctx context.Context, store models.ActionStore, scope models.Scope, path *models.AgentSessionPath, runID string) ([]control.GovernedToolView, error) {
	return control.LoadGovernedToolViews(ctx, store, scope, path, runID)
}

// TypedSourceFailure forwards to control.TypedSourceFailure.
func TypedSourceFailure(view control.GovernedToolView) (limited, relevant bool) {
	return control.TypedSourceFailure(view)
}

// RefuseLiveBrowserWhileRateLimited forwards to control.RefuseLiveBrowserWhileRateLimited.
func RefuseLiveBrowserWhileRateLimited(views []control.GovernedToolView, actionName string) (*models.ToolResult, bool) {
	return control.RefuseLiveBrowserWhileRateLimited(views, actionName)
}

// IsBackgroundTaskRun forwards to control.IsBackgroundTaskRun.
func IsBackgroundTaskRun(run *models.AgentRun) bool {
	return control.IsBackgroundTaskRun(run)
}

// PendingSetupRequestID forwards to control.PendingSetupRequestID.
func PendingSetupRequestID(run *models.AgentRun, view control.GovernedToolView, interventions []models.AgentRunIntervention) string {
	return control.PendingSetupRequestID(run, view, interventions)
}

// SkillSetupPendingSourceKey forwards to control.SkillSetupPendingSourceKey.
func SkillSetupPendingSourceKey(runID, requestID string) string {
	return control.SkillSetupPendingSourceKey(runID, requestID)
}

// PendingSkillSetupNote forwards to control.PendingSkillSetupNote.
func PendingSkillSetupNote(run *models.AgentRun, sessionID string, entry *models.SessionEntry, call *models.ActionCall, interventions []models.AgentRunIntervention) *models.SessionEntry {
	return control.PendingSkillSetupNote(run, sessionID, entry, call, interventions)
}
