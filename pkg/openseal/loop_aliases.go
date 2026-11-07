package openseal

import "github.com/axiom-studio/openseal/pkg/runtime"

// Run loop types: the step host contract (one model step, one host tool), the
// step producer contract the model loop and deterministic runners implement,
// and the typed failure a Run ends with.
type (
	StepHost               = runtime.StepHost
	ModelStepRequest       = runtime.ModelStepRequest
	ModelStepResult        = runtime.ModelStepResult
	ModelStepPurpose       = runtime.ModelStepPurpose
	ModelStepSystem        = runtime.ModelStepSystem
	ModelStepSystemSection = runtime.ModelStepSystemSection
	ModelStepTool          = runtime.ModelStepTool
	ModelStepToolGroup     = runtime.ModelStepToolGroup
	ModelStepStream        = runtime.ModelStepStream
	ModelStepModelInfo     = runtime.ModelStepModelInfo
	ModelStepRenderInfo    = runtime.ModelStepRenderInfo
	ModelStepUsage         = runtime.ModelStepUsage
	ModelStepProvider      = runtime.ModelStepProvider
	ModelStopReason        = runtime.ModelStopReason
	ModelToolCall          = runtime.ModelToolCall
	SessionEntryView       = runtime.SessionEntryView
	UserEntryView          = runtime.UserEntryView
	ToolEntryView          = runtime.ToolEntryView
	ToolResultView         = runtime.ToolResultView
	CompactionEntryView    = runtime.CompactionEntryView
	NoteEntryView          = runtime.NoteEntryView
	EntryAuthor            = runtime.EntryAuthor

	StepProducer      = runtime.StepProducer
	StepBudgetPlanner = runtime.StepBudgetPlanner
	StepInput         = runtime.StepInput
	StepMode          = runtime.StepMode
	StepOutput        = runtime.StepOutput
	LoopToolCall      = runtime.LoopToolCall
	ToolCallDirective = runtime.ToolCallDirective
	LoopToolResult    = runtime.LoopToolResult
	LoopFinal         = runtime.LoopFinal
	LoopPark          = runtime.LoopPark
	LoopFailure       = runtime.LoopFailure
	ModelStepProducer = runtime.ModelStepProducer
	ModelStepPreparer = runtime.ModelStepPreparer
	ModelStepMedia    = runtime.ModelStepMedia

	RunFailureExplanation = runtime.RunFailureExplanation
)

const (
	ModelStepAPIVersion = runtime.ModelStepAPIVersion

	ModelStepPurposeStep            = runtime.ModelStepPurposeStep
	ModelStepPurposeSummary         = runtime.ModelStepPurposeSummary
	ModelStepPurposeExplanation     = runtime.ModelStepPurposeExplanation
	ModelStepPurposeAcknowledgement = runtime.ModelStepPurposeAcknowledgement

	ModelStepToolHarness = runtime.ModelStepToolHarness
	ModelStepToolCore    = runtime.ModelStepToolCore
	ModelStepToolLoaded  = runtime.ModelStepToolLoaded

	ModelStepProviderAnthropic = runtime.ModelStepProviderAnthropic
	ModelStepProviderResponses = runtime.ModelStepProviderResponses
	ModelStepProviderChat      = runtime.ModelStepProviderChat

	ModelStopToolCalls = runtime.ModelStopToolCalls
	ModelStopEnd       = runtime.ModelStopEnd
	ModelStopRefusal   = runtime.ModelStopRefusal
	ModelStopMaxTokens = runtime.ModelStopMaxTokens

	StepModeNormal         = runtime.StepModeNormal
	StepModeExplainFailure = runtime.StepModeExplainFailure

	StepFailureHostUnavailable   = runtime.StepFailureHostUnavailable
	StepFailureHostConfiguration = runtime.StepFailureHostConfiguration
	StepFailureContextExceeded   = runtime.StepFailureContextExceeded
	StepFailureProducerFailed    = runtime.StepFailureProducerFailed
	StepFailurePreparationFailed = runtime.StepFailurePreparationFailed

	StepHostFailureUnreachable    = runtime.StepHostFailureUnreachable
	StepHostFailureConnectionLost = runtime.StepHostFailureConnectionLost
	StepHostFailureDraining       = runtime.StepHostFailureDraining
	StepHostFailureProtocol       = runtime.StepHostFailureProtocol
	StepHostFailureAccessDenied   = runtime.StepHostFailureAccessDenied
)

var (
	ErrInvalidModelStepRequest = runtime.ErrInvalidModelStepRequest
	ErrInvalidStepOutput       = runtime.ErrInvalidStepOutput

	NewModelStepProducer    = runtime.NewModelStepProducer
	ValidateModelStepResult = runtime.ValidateModelStepResult
)
