package openseal

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

// Run loop types: the step host contract (one model step, one host tool), the
// step producer contract the model loop and deterministic runners implement,
// and the typed failure a Run ends with.
type (
	StepHost               = agentrun.StepHost
	ModelStepRequest       = models.ModelStepRequest
	ModelStepResult        = models.ModelStepResult
	ModelStepPurpose       = models.ModelStepPurpose
	ModelStepSystem        = models.ModelStepSystem
	ModelStepSystemSection = models.ModelStepSystemSection
	ModelStepTool          = models.ModelStepTool
	ModelStepToolGroup     = models.ModelStepToolGroup
	ModelStepStream        = models.ModelStepStream
	ModelStepModelInfo     = models.ModelStepModelInfo
	ModelStepRenderInfo    = models.ModelStepRenderInfo
	ModelStepUsage         = models.ModelStepUsage
	ModelStepProvider      = models.ModelStepProvider
	ModelStopReason        = models.ModelStopReason
	ModelToolCall          = models.ModelToolCall
	SessionEntryView       = models.SessionEntryView
	UserEntryView          = models.UserEntryView
	ToolEntryView          = models.ToolEntryView
	ToolResultView         = models.ToolResultView
	CompactionEntryView    = models.CompactionEntryView
	NoteEntryView          = models.NoteEntryView
	EntryAuthor            = models.EntryAuthor

	StepProducer      = agentrun.StepProducer
	StepBudgetPlanner = modelstep.StepBudgetPlanner
	StepInput         = agentrun.StepInput
	StepMode          = agentrun.StepMode
	StepOutput        = agentrun.StepOutput
	LoopToolCall      = agentrun.LoopToolCall
	ToolCallDirective = agentrun.ToolCallDirective
	LoopToolResult    = modelstep.LoopToolResult
	LoopFinal         = agentrun.LoopFinal
	LoopPark          = agentrun.LoopPark
	LoopFailure       = agentrun.LoopFailure
	ModelStepProducer = modelstep.ModelStepProducer
	ModelStepPreparer = agentrun.ModelStepPreparer
	ModelStepMedia    = modelstep.ModelStepMedia

	RunFailureExplanation = models.RunFailureExplanation
)

const (
	ModelStepAPIVersion = models.ModelStepAPIVersion

	ModelStepPurposeStep            = models.ModelStepPurposeStep
	ModelStepPurposeSummary         = models.ModelStepPurposeSummary
	ModelStepPurposeExplanation     = models.ModelStepPurposeExplanation
	ModelStepPurposeAcknowledgement = models.ModelStepPurposeAcknowledgement

	ModelStepToolHarness = models.ModelStepToolHarness
	ModelStepToolCore    = models.ModelStepToolCore
	ModelStepToolLoaded  = models.ModelStepToolLoaded

	ModelStepProviderAnthropic = models.ModelStepProviderAnthropic
	ModelStepProviderResponses = models.ModelStepProviderResponses
	ModelStepProviderChat      = models.ModelStepProviderChat

	ModelStopToolCalls = models.ModelStopToolCalls
	ModelStopEnd       = models.ModelStopEnd
	ModelStopRefusal   = models.ModelStopRefusal
	ModelStopMaxTokens = models.ModelStopMaxTokens

	StepModeNormal         = agentrun.StepModeNormal
	StepModeExplainFailure = agentrun.StepModeExplainFailure

	StepFailureHostUnavailable   = runloop.StepFailureHostUnavailable
	StepFailureHostConfiguration = runloop.StepFailureHostConfiguration
	StepFailureContextExceeded   = runloop.StepFailureContextExceeded
	StepFailureProducerFailed    = runloop.StepFailureProducerFailed
	StepFailurePreparationFailed = runloop.StepFailurePreparationFailed

	StepHostFailureUnreachable    = modelstep.StepHostFailureUnreachable
	StepHostFailureConnectionLost = modelstep.StepHostFailureConnectionLost
	StepHostFailureDraining       = modelstep.StepHostFailureDraining
	StepHostFailureProtocol       = modelstep.StepHostFailureProtocol
	StepHostFailureAccessDenied   = modelstep.StepHostFailureAccessDenied
)

var (
	ErrInvalidModelStepRequest = models.ErrInvalidModelStepRequest
	ErrInvalidStepOutput       = agentrun.ErrInvalidStepOutput

	NewModelStepProducer    = modelstep.NewModelStepProducer
	ValidateModelStepResult = modelstep.ValidateModelStepResult
)
