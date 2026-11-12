// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/modelstep. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/modelstep, kept so that importers of
// pkg/runtime continue to compile.

package runtime

import (
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/workspace"
	"go.uber.org/zap"
)

type CapabilityInvocationStepProducer = modelstep.CapabilityInvocationStepProducer

// NewCapabilityInvocationStepProducer forwards to modelstep.NewCapabilityInvocationStepProducer.
func NewCapabilityInvocationStepProducer(actions []capability.ModelAction, calls models.ActionCallReader) (*modelstep.CapabilityInvocationStepProducer, error) {
	return modelstep.NewCapabilityInvocationStepProducer(actions, calls)
}

type LoopToolResult = modelstep.LoopToolResult

// CapabilityNoteSourceKey forwards to modelstep.CapabilityNoteSourceKey.
func CapabilityNoteSourceKey(stepID, compactionID, digest string) string {
	return modelstep.CapabilityNoteSourceKey(stepID, compactionID, digest)
}

const MaxCapabilitySearchesPerRun = modelstep.MaxCapabilitySearchesPerRun

// SearchCapabilitiesSpec forwards to modelstep.SearchCapabilitiesSpec.
func SearchCapabilitiesSpec() models.ModelStepTool {
	return modelstep.SearchCapabilitiesSpec()
}

// ExecuteSearchCapabilities forwards to modelstep.ExecuteSearchCapabilities.
func ExecuteSearchCapabilities(set *modelstep.CapabilitySet, arguments map[string]interface{}, searchesThisRun int) (content json.RawMessage, isError bool) {
	return modelstep.ExecuteSearchCapabilities(set, arguments, searchesThisRun)
}

// CapabilitySearchesThisRun forwards to modelstep.CapabilitySearchesThisRun.
func CapabilitySearchesThisRun(path []*models.SessionEntry, runID string) int {
	return modelstep.CapabilitySearchesThisRun(path, runID)
}

const LoadCapabilitiesTool = modelstep.LoadCapabilitiesTool

const SearchCapabilitiesTool = modelstep.SearchCapabilitiesTool

type CapabilityCatalog = modelstep.CapabilityCatalog

type CapabilitySetConfig = modelstep.CapabilitySetConfig

type CapabilitySet = modelstep.CapabilitySet

// NewCapabilitySet forwards to modelstep.NewCapabilitySet.
func NewCapabilitySet(catalog modelstep.CapabilityCatalog, loadedIDs []string, config modelstep.CapabilitySetConfig) *modelstep.CapabilitySet {
	return modelstep.NewCapabilitySet(catalog, loadedIDs, config)
}

// NewPathCapabilitySet forwards to modelstep.NewPathCapabilitySet.
func NewPathCapabilitySet(catalog modelstep.CapabilityCatalog, path *models.AgentSessionPath, config modelstep.CapabilitySetConfig) *modelstep.CapabilitySet {
	return modelstep.NewPathCapabilitySet(catalog, path, config)
}

type SurfacedLoad = modelstep.SurfacedLoad

// SurfacedOnPath forwards to modelstep.SurfacedOnPath.
func SurfacedOnPath(path *models.AgentSessionPath) []modelstep.SurfacedLoad {
	return modelstep.SurfacedOnPath(path)
}

// PathCarriedCapabilityIDs forwards to modelstep.PathCarriedCapabilityIDs.
func PathCarriedCapabilityIDs(path *models.AgentSessionPath) []string {
	return modelstep.PathCarriedCapabilityIDs(path)
}

// PathLoadedCapabilityIDs forwards to modelstep.PathLoadedCapabilityIDs.
func PathLoadedCapabilityIDs(path *models.AgentSessionPath) []string {
	return modelstep.PathLoadedCapabilityIDs(path)
}

// LoadCapabilitiesSpec forwards to modelstep.LoadCapabilitiesSpec.
func LoadCapabilitiesSpec() models.ModelStepTool {
	return modelstep.LoadCapabilitiesSpec()
}

// ExecuteLoadCapabilities forwards to modelstep.ExecuteLoadCapabilities.
func ExecuteLoadCapabilities(set *modelstep.CapabilitySet, arguments map[string]interface{}, toolName func(id string) string) (content json.RawMessage, isError bool, loaded []string) {
	return modelstep.ExecuteLoadCapabilities(set, arguments, toolName)
}

const ControlBudgetErrorCode = modelstep.ControlBudgetErrorCode

type ControlBudgetInsufficientError = modelstep.ControlBudgetInsufficientError

type GovernedToolInput = modelstep.GovernedToolInput

// ParseGovernedToolInput forwards to modelstep.ParseGovernedToolInput.
func ParseGovernedToolInput(action capability.ModelAction, raw json.RawMessage, directive *modelstep.GovernedInputDirective) (modelstep.GovernedToolInput, error) {
	return modelstep.ParseGovernedToolInput(action, raw, directive)
}

const GovernedReviewSummaryField = modelstep.GovernedReviewSummaryField

// GovernedToolReviewed forwards to modelstep.GovernedToolReviewed.
func GovernedToolReviewed(action capability.ModelAction) bool {
	return modelstep.GovernedToolReviewed(action)
}

// GovernedToolNeedsReviewSummary forwards to modelstep.GovernedToolNeedsReviewSummary.
func GovernedToolNeedsReviewSummary(action capability.ModelAction) bool {
	return modelstep.GovernedToolNeedsReviewSummary(action)
}

// GovernedToolInputSchema forwards to modelstep.GovernedToolInputSchema.
func GovernedToolInputSchema(action capability.ModelAction) map[string]interface{} {
	return modelstep.GovernedToolInputSchema(action)
}

const HostToolInterruptedMessage = modelstep.HostToolInterruptedMessage

type HostToolCall = modelstep.HostToolCall

type HostToolOutcome = modelstep.HostToolOutcome

type HostToolDispatcher = modelstep.HostToolDispatcher

// NewHostToolDispatcher forwards to modelstep.NewHostToolDispatcher.
func NewHostToolDispatcher(host models.HostToolExecutor, actions *action.ActionCoordinator, logger *zap.SugaredLogger) *modelstep.HostToolDispatcher {
	return modelstep.NewHostToolDispatcher(host, actions, logger)
}

// HostToolResumeState forwards to modelstep.HostToolResumeState.
func HostToolResumeState(spec modelstep.HostToolSpec) (models.ToolEntryState, *models.ToolResult) {
	return modelstep.HostToolResumeState(spec)
}

type ChatWorkspaces = modelstep.ChatWorkspaces

type HostToolSpec = modelstep.HostToolSpec

// HostToolSpecs forwards to modelstep.HostToolSpecs.
func HostToolSpecs(authority *workspace.Authority) []modelstep.HostToolSpec {
	return modelstep.HostToolSpecs(authority)
}

// LookupHostTool forwards to modelstep.LookupHostTool.
func LookupHostTool(specs []modelstep.HostToolSpec, name string) (modelstep.HostToolSpec, bool) {
	return modelstep.LookupHostTool(specs, name)
}

type LoopCompactor = modelstep.LoopCompactor

type LoopFailureError = modelstep.LoopFailureError

type ModelWindow = modelstep.ModelWindow

// CompactionSummaryText forwards to modelstep.CompactionSummaryText.
func CompactionSummaryText(c *models.CompactionEntry) string {
	return modelstep.CompactionSummaryText(c)
}

type PathEstimate = modelstep.PathEstimate

const RunFailureKindRepeatLimit = modelstep.RunFailureKindRepeatLimit

const RunFailureKindContextOverflow = modelstep.RunFailureKindContextOverflow

const RunFailureCodeErrorStreak = modelstep.RunFailureCodeErrorStreak

const RunFailureCodeCompactionFailed = modelstep.RunFailureCodeCompactionFailed

type StepBudgetPlanner = modelstep.StepBudgetPlanner

// EstimateModelStepInputTokens forwards to modelstep.EstimateModelStepInputTokens.
func EstimateModelStepInputTokens(req models.ModelStepRequest) (int64, error) {
	return modelstep.EstimateModelStepInputTokens(req)
}

type ModelStepProducer = modelstep.ModelStepProducer

type ModelStepMedia = modelstep.ModelStepMedia

// NewModelStepProducer forwards to modelstep.NewModelStepProducer.
func NewModelStepProducer(host agentrun.StepHost, preparer agentrun.ModelStepPreparer, store models.ModelStepProducerStore) (*modelstep.ModelStepProducer, error) {
	return modelstep.NewModelStepProducer(host, preparer, store)
}

// ValidateModelStepResult forwards to modelstep.ValidateModelStepResult.
func ValidateModelStepResult(req models.ModelStepRequest, res models.ModelStepResult) error {
	return modelstep.ValidateModelStepResult(req, res)
}

var ErrTurnHostUnavailable = modelstep.ErrTurnHostUnavailable

var ErrTurnHostConfiguration = modelstep.ErrTurnHostConfiguration

type TurnHostFailure = modelstep.TurnHostFailure

const TurnHostContextExceeded = modelstep.TurnHostContextExceeded

type HostedContextPressure = modelstep.HostedContextPressure

// NewTurnHostFailure forwards to modelstep.NewTurnHostFailure.
func NewTurnHostFailure(code, message string, retryable bool) error {
	return modelstep.NewTurnHostFailure(code, message, retryable)
}

type HostedRunBudget = modelstep.HostedRunBudget

const StepHostFailureUnreachable = modelstep.StepHostFailureUnreachable

const StepHostFailureConnectionLost = modelstep.StepHostFailureConnectionLost

const StepHostFailureDraining = modelstep.StepHostFailureDraining

const StepHostFailureProtocol = modelstep.StepHostFailureProtocol

const StepHostFailureAccessDenied = modelstep.StepHostFailureAccessDenied

type ToolInvoker = modelstep.ToolInvoker

type ToolInvokerFunc = modelstep.ToolInvokerFunc

type ToolActionDispatcher = modelstep.ToolActionDispatcher

// NewToolActionDispatcher forwards to modelstep.NewToolActionDispatcher.
func NewToolActionDispatcher(invoker modelstep.ToolInvoker) (*modelstep.ToolActionDispatcher, error) {
	return modelstep.NewToolActionDispatcher(invoker)
}
