package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// ModelStepProducer is the StepProducer of every model-driven Run: it builds
// one ModelStepRequest per step from the preparer's system and tools plus the
// session path, calls StepHost.ExecuteModelStep and returns the validated
// assistant entry. The loop appends that entry and its tool entries; the
// producer writes only the step's capabilities note (capability_note.go),
// right before it renders the path.
type ModelStepProducer struct {
	host     StepHost
	preparer ModelStepPreparer
	sessions ModelStepProducerStore
	// Media resolves image parts while rendering the path. A zero value renders
	// every image as "[image omitted: <mediaType>]".
	Media ModelStepMedia
	// Now stamps the capabilities note; nil = time.Now.
	Now func() time.Time
	// cache holds the preparer's request between PlanStepBudget and Produce of
	// one step attempt (key stepId:attempt). Produce removes it.
	cache sync.Map
}

// ModelStepMedia are the image sources of the path renderer.
type ModelStepMedia struct {
	Artifacts ArtifactStore
	Content   ArtifactContentStore
	// ImageInput reports whether the model the prepared request resolves to
	// accepts image input (catalog modality). nil = no image input.
	ImageInput func(ctx context.Context, in StepInput, req ModelStepRequest) bool
}

var (
	_ StepProducer      = (*ModelStepProducer)(nil)
	_ StepBudgetPlanner = (*ModelStepProducer)(nil)
)

func NewModelStepProducer(host StepHost, preparer ModelStepPreparer, store ModelStepProducerStore) (*ModelStepProducer, error) {
	if host == nil {
		return nil, errors.New("model step producer requires a step host")
	}
	if preparer == nil {
		return nil, errors.New("model step producer requires a model step preparer")
	}
	if store == nil {
		return nil, errors.New("model step producer requires a session store")
	}
	return &ModelStepProducer{host: host, preparer: preparer, sessions: store}, nil
}

func (p *ModelStepProducer) Kind() StepProducerKind { return StepProducerModel }

// modelCredentialSource is a preparer that knows the model credential its
// steps send.
type modelCredentialSource interface {
	ModelCredential() *capability.CredentialReference
}

// ModelCredential is a copy of the model credential the producer's steps
// send, nil when its preparer names none (the host's default).
func (p *ModelStepProducer) ModelCredential() *capability.CredentialReference {
	source, _ := p.preparer.(modelCredentialSource)
	if source == nil {
		return nil
	}
	return source.ModelCredential()
}

// PlanStepBudget is the step's token reservation on a bounded Run: the
// request's input estimate for every provider attempt plus the host's
// correction message (doubled when compaction is due), against the Run's
// remaining capacity. An unbounded Run plans nothing.
func (p *ModelStepProducer) PlanStepBudget(ctx context.Context, in StepInput) (BudgetUsage, error) {
	if err := checkModelStepInput(in); err != nil {
		return BudgetUsage{}, err
	}
	if in.Run.Budget == nil {
		return BudgetUsage{}, nil
	}
	prepared, err := p.prepare(ctx, in)
	if err != nil {
		return BudgetUsage{}, err
	}
	// The budget is planned before the preparers append this step's context
	// (SPEC §3.1 phases 1 and 3), so the first step of a chat sees an empty
	// lane session. It is planned on the system, tools and protocol reserve;
	// Produce validates the full request.
	req, err := p.render(ctx, in, prepared, 0)
	if err != nil {
		return BudgetUsage{}, err
	}
	if len(req.Entries) > 0 {
		if err := req.Validate(); err != nil {
			return BudgetUsage{}, fmt.Errorf("%w: %w", ErrTurnHostConfiguration, err)
		}
	}
	due := in.Binding != nil && in.Binding.Compactor != nil && in.Binding.Compactor.Due(in)
	input, err := modelStepReservationInput(req, due)
	if err != nil {
		return BudgetUsage{}, err
	}
	budget, err := controlRunBudget(in.Run, in.Step.ID)
	if err != nil {
		return BudgetUsage{}, err
	}
	usage, err := planModelStepReservation(budget.Policy, budget.Remaining, input)
	if err != nil {
		return BudgetUsage{}, err
	}
	p.cache.Store(modelStepCacheKey(in), prepared)
	return usage, nil
}

// Produce runs the step's model call. Host failures return unchanged (the
// loop maps *TurnHostFailure codes, including TurnHostContextExceeded); a
// result that does not answer the request wraps ErrTurnHostConfiguration.
func (p *ModelStepProducer) Produce(ctx context.Context, in StepInput) (StepOutput, error) {
	if err := checkModelStepInput(in); err != nil {
		return StepOutput{}, err
	}
	var prepared ModelStepRequest
	if cached, ok := p.cache.LoadAndDelete(modelStepCacheKey(in)); ok {
		prepared = cached.(ModelStepRequest)
	} else {
		var err error
		if prepared, err = p.prepare(ctx, in); err != nil {
			return StepOutput{}, err
		}
	}
	if err := p.ensureCapabilityNote(ctx, in); err != nil {
		return StepOutput{}, err
	}
	req, err := p.request(ctx, in, prepared, modelStepReservedOutput(in))
	if err != nil {
		return StepOutput{}, err
	}
	result, err := p.host.ExecuteModelStep(ctx, req)
	if err != nil {
		return StepOutput{}, err
	}
	if err := ValidateModelStepResult(req, result); err != nil {
		return StepOutput{}, err
	}
	assistant := result.Assistant
	assistant.Limits = modelStepLimits(result)
	return StepOutput{
		ModelProvider: string(result.Model.Provider),
		Model:         result.Model.Model,
		Usage:         turnUsageFromModelStep(assistant.Usage),
		Assistant:     &assistant,
	}, nil
}

// ensureCapabilityNote appends the capabilities note the step's current path
// needs, after the step's context and any compaction, so the request rendered
// next shows the current listing. A preparer without a catalog has none.
func (p *ModelStepProducer) ensureCapabilityNote(ctx context.Context, in StepInput) error {
	source, ok := p.preparer.(capabilityListingSource)
	if !ok || in.Mode != StepModeNormal {
		return nil
	}
	path, err := p.sessions.LoadAgentSessionPath(ctx, in.Run.Scope, in.Session.ID)
	if err != nil {
		return fmt.Errorf("load session path: %w", err)
	}
	in.Path = path
	_, err = ensureCapabilityNote(ctx, p.sessions, in, path, source.CapabilityListing(in), p.now())
	return err
}

func (p *ModelStepProducer) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func checkModelStepInput(in StepInput) error {
	if in.Run == nil || in.Step == nil {
		return fmt.Errorf("%w: model step needs a Run and a step", ErrTurnHostConfiguration)
	}
	if in.Session == nil || strings.TrimSpace(in.Session.ID) == "" {
		return fmt.Errorf("%w: model step needs an agent session", ErrTurnHostConfiguration)
	}
	if !in.Mode.Valid() {
		return fmt.Errorf("%w: unknown step mode %q", ErrTurnHostConfiguration, in.Mode)
	}
	if in.Attempt < 0 {
		return fmt.Errorf("%w: negative step attempt", ErrTurnHostConfiguration)
	}
	return nil
}

func modelStepCacheKey(in StepInput) string {
	return in.Step.ID + ":" + strconv.Itoa(in.Attempt) + ":" + string(in.Mode)
}

func (p *ModelStepProducer) prepare(ctx context.Context, in StepInput) (ModelStepRequest, error) {
	req, err := p.preparer.PrepareModelStep(ctx, in)
	if err != nil {
		return ModelStepRequest{}, fmt.Errorf("prepare model step: %w", err)
	}
	return req, nil
}

// request completes the preparer's request for this step: identity, purpose,
// the freshly loaded session path (after this step's context append and
// compaction), the output cap and the preview flag. maxOutput > 0 caps
// MaxOutputTokens at the step's reserved output.
func (p *ModelStepProducer) request(ctx context.Context, in StepInput, prepared ModelStepRequest, maxOutput int64) (ModelStepRequest, error) {
	req, err := p.render(ctx, in, prepared, maxOutput)
	if err != nil {
		return ModelStepRequest{}, err
	}
	if err := req.Validate(); err != nil {
		return ModelStepRequest{}, fmt.Errorf("%w: %w", ErrTurnHostConfiguration, err)
	}
	return req, nil
}

// render builds the step's request from the prepared one and the session
// path, without validating it.
func (p *ModelStepProducer) render(ctx context.Context, in StepInput, prepared ModelStepRequest, maxOutput int64) (ModelStepRequest, error) {
	req := prepared
	req.APIVersion = ModelStepAPIVersion
	req.Scope = in.Run.Scope
	req.RunID = in.Run.ID
	req.StepID = in.Step.ID
	req.Attempt = in.Attempt
	req.SessionID = in.Session.ID
	req.SummaryInstruction = ""
	if strings.TrimSpace(req.AgentID) == "" {
		req.AgentID = in.Run.AssignedAgentID
	}
	if in.Binding != nil {
		if req.DefinitionID == "" {
			req.DefinitionID = in.Binding.DefinitionID
		}
		if req.DefinitionVersion == "" {
			req.DefinitionVersion = in.Binding.DefinitionVersion
		}
	}
	req.Purpose = ModelStepPurposeStep
	if in.Mode == StepModeExplainFailure {
		req.Purpose = ModelStepPurposeExplanation
		req.Tools = nil
		req.Stream.FinalAnswerTool = ""
	}
	req.Stream.AnswerPreview = HasTurnAnswerPreview(ctx)
	if maxOutput > 0 && (req.MaxOutputTokens == 0 || maxOutput < req.MaxOutputTokens) {
		req.MaxOutputTokens = maxOutput
	}

	path, err := p.sessions.LoadAgentSessionPath(ctx, in.Run.Scope, in.Session.ID)
	if err != nil {
		return ModelStepRequest{}, fmt.Errorf("load session path: %w", err)
	}
	opts := sessionRenderOptions{Artifacts: p.Media.Artifacts, Content: p.Media.Content, Actions: p.sessions}
	if p.Media.ImageInput != nil {
		opts.ImageInput = p.Media.ImageInput(ctx, in, req)
	}
	entries, err := projectSessionPath(ctx, path, opts)
	if err != nil {
		return ModelStepRequest{}, fmt.Errorf("render session path: %w", err)
	}
	req.Entries = entries
	if len(req.Tools) > 0 {
		req.Tools = applySurfaces(req.Tools, path, req.Entries)
	}
	return req, nil
}

// modelStepReservedOutput is the output reserved for the step on a bounded
// Run (0 = no Run limit).
func modelStepReservedOutput(in StepInput) int64 {
	if in.Run.Budget == nil {
		return 0
	}
	reservation, ok := in.Run.BudgetReservations[in.Step.ID]
	if !ok || reservation.Usage.OutputTokens <= 0 {
		return 0
	}
	return reservation.Usage.OutputTokens
}

// modelStepLimits are the kernel-filled limits of the assistant entry. The
// prompt size is the provider's input count when the host made one attempt;
// across attempts InputTokens is a sum, so the render estimate is used.
func modelStepLimits(result ModelStepResult) *AssistantModelLimits {
	prompt := result.Render.EstimatedInputTokens
	if result.Assistant.Usage.Attempts == 1 && result.Assistant.Usage.InputTokens > 0 {
		prompt = result.Assistant.Usage.InputTokens
	}
	limits := &AssistantModelLimits{
		ContextTokens:    result.Model.ContextTokens,
		InputLimitTokens: result.Model.InputLimitTokens,
		PromptTokens:     prompt,
	}
	if *limits == (AssistantModelLimits{}) {
		return nil
	}
	return limits
}

func turnUsageFromModelStep(usage ModelStepUsage) TurnUsage {
	repairs := 0
	if usage.Attempts > 1 {
		repairs = usage.Attempts - 1
	}
	return TurnUsage{
		InputTokens: int(usage.InputTokens), OutputTokens: int(usage.OutputTokens),
		CacheReadTokens: int(usage.CacheReadTokens), CacheWriteTokens: int(usage.CacheWriteTokens),
		Cost: usage.Cost, DurationMS: usage.DurationMS, ProviderDurationMS: usage.ProviderDurationMS,
		RepairAttempts: repairs,
	}
}
