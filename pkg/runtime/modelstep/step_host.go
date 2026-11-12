package modelstep

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// maxModelStepResultTextBytes caps the assistant text of one model step.
const maxModelStepResultTextBytes = 256 << 10

func invalidModelStepResult(format string, args ...interface{}) error {
	return fmt.Errorf("%w: model step result: %s", ErrTurnHostConfiguration, fmt.Sprintf(format, args...))
}

// ValidateModelStepResult checks what a StepHost returned for req before the
// kernel appends it. Every violation wraps ErrTurnHostConfiguration.
//
// It does not check tool names against req.Tools, nor argument size: an
// unknown tool, arguments the host flagged in ArgumentsError and oversize
// arguments become error tool entries so the model can correct itself.
func ValidateModelStepResult(req models.ModelStepRequest, res models.ModelStepResult) error {
	if res.APIVersion != models.ModelStepAPIVersion {
		return invalidModelStepResult("apiVersion %q, want %q", res.APIVersion, models.ModelStepAPIVersion)
	}
	if res.StepID != req.StepID {
		return invalidModelStepResult("stepId %q does not match the request's %q", res.StepID, req.StepID)
	}
	a := res.Assistant
	if a.Limits != nil {
		return invalidModelStepResult("assistant limits are kernel-filled")
	}
	if err := checkProducedAssistant(&a); err != nil {
		return invalidModelStepResult("%v", err)
	}
	if res.Model.Provider != a.Provider {
		return invalidModelStepResult("model provider %q does not match the assistant's %q", res.Model.Provider, a.Provider)
	}
	if strings.TrimSpace(res.Model.Model) == "" {
		return invalidModelStepResult("model info has no model")
	}
	if res.Model.ContextTokens < 0 || res.Model.InputLimitTokens < 0 || res.Model.MaxOutputTokens < 0 ||
		res.Render.EstimatedInputTokens < 0 || res.Render.ReasoningCutoffSequence < 0 {
		return invalidModelStepResult("model limits and render info cannot be negative")
	}
	if len(a.ToolCalls) == 0 && res.Narration != "" {
		return invalidModelStepResult("narration without tool calls")
	}
	switch {
	case req.Purpose == models.ModelStepPurposeSummary:
		if len(a.ToolCalls) > 0 {
			return invalidModelStepResult("summary step returned tool calls")
		}
		if strings.TrimSpace(a.Text) == "" {
			return invalidModelStepResult("summary step returned no summary text")
		}
	case req.Purpose.ToolFree():
		if len(a.ToolCalls) > 0 {
			return invalidModelStepResult("%s step returned tool calls", req.Purpose)
		}
	}
	return nil
}

// checkProducedAssistant is the shape check shared by ValidateModelStepResult
// and validateStepOutput. Unlike AssistantEntry.validate it does not cap
// argument size, because oversize arguments become an error tool entry.
func checkProducedAssistant(a *models.AssistantEntry) error {
	if !a.Provider.Valid() {
		return fmt.Errorf("unknown assistant provider %q", a.Provider)
	}
	if strings.TrimSpace(a.Model) == "" {
		return errors.New("assistant entry has no model")
	}
	if !a.StopReason.Valid() {
		return fmt.Errorf("unknown stop reason %q", a.StopReason)
	}
	if len(a.Text) > maxModelStepResultTextBytes {
		return fmt.Errorf("assistant text is %d bytes (max %d)", len(a.Text), maxModelStepResultTextBytes)
	}
	u := a.Usage
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.ReasoningTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 ||
		u.DurationMS < 0 || u.ProviderDurationMS < 0 || u.FirstOutputMS < 0 || u.Attempts < 0 ||
		u.Cost < 0 || math.IsNaN(u.Cost) || math.IsInf(u.Cost, 0) {
		return errors.New("usage cannot be negative, NaN, or infinite")
	}
	seen := make(map[string]bool, len(a.ToolCalls))
	for i, call := range a.ToolCalls {
		if strings.TrimSpace(call.ID) == "" {
			return fmt.Errorf("tool call %d has no id", i)
		}
		if seen[call.ID] {
			return fmt.Errorf("duplicate tool call id %q", call.ID)
		}
		seen[call.ID] = true
		if strings.TrimSpace(call.Name) == "" {
			return fmt.Errorf("tool call %q has no name", call.ID)
		}
		if !isModelToolArgumentsObject(call.Arguments) {
			return fmt.Errorf("tool call %q arguments are not a JSON object", call.ID)
		}
	}
	if a.StopReason == models.ModelStopToolCalls && len(a.ToolCalls) == 0 {
		return fmt.Errorf("stop reason %q with no tool calls", a.StopReason)
	}
	if a.Native != nil {
		for i, raw := range a.Native.Anthropic {
			if !json.Valid(raw) {
				return fmt.Errorf("native anthropic block %d is not JSON", i)
			}
		}
		for i, raw := range a.Native.Responses {
			if !json.Valid(raw) {
				return fmt.Errorf("native responses item %d is not JSON", i)
			}
		}
	}
	return nil
}

func isModelToolArgumentsObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

var (
	ErrTurnHostUnavailable   = errors.New("turn host is temporarily unavailable")
	ErrTurnHostConfiguration = errors.New("turn host configuration is invalid")
)

// TurnHostFailure is a safe, stable failure a StepHost returns for a model
// step. Retryable marks a transient host or provider outage, which the loop
// requeues (host_unavailable); a terminal failure ends the step. Message
// must never contain provider credentials or a raw provider response.
type TurnHostFailure struct {
	Code      string
	Message   string
	Retryable bool
	// ContextPressure is set with TurnHostContextExceeded when the step's
	// input does not fit the model's context. The host made no call, so the
	// loop compacts the session and sends the step once more.
	ContextPressure *HostedContextPressure
}

// TurnHostContextExceeded is the TurnHostFailure code for a step whose input
// does not fit the model's context.
const TurnHostContextExceeded = "model_context_exceeded"

// HostedContextPressure compares a host's estimate of a step's model input
// with the input the model accepts, both in tokens.
type HostedContextPressure struct {
	EstimatedTokens int64 `json:"estimatedTokens"`
	LimitTokens     int64 `json:"limitTokens"`
}

func (e *TurnHostFailure) Error() string {
	if e == nil || strings.TrimSpace(e.Message) == "" {
		return "hosted agent turn failed"
	}
	return strings.TrimSpace(e.Message)
}

func (e *TurnHostFailure) Is(target error) bool {
	return e != nil && e.Retryable && target == ErrTurnHostUnavailable
}

func NewTurnHostFailure(code, message string, retryable bool) error {
	return &TurnHostFailure{Code: strings.TrimSpace(code), Message: strings.TrimSpace(message), Retryable: retryable}
}

type retryableTurnHostError struct{ cause error }

func (e retryableTurnHostError) Error() string        { return ErrTurnHostUnavailable.Error() }
func (e retryableTurnHostError) Unwrap() error        { return e.cause }
func (e retryableTurnHostError) Is(target error) bool { return target == ErrTurnHostUnavailable }

// HostedRunBudget is the capacity visible to one step of a bounded Run.
// Policy distinguishes an unbounded zero from an exhausted zero; Remaining
// has already accounted for committed usage, live reservations and durable
// child allocations.
type HostedRunBudget struct {
	Policy          models.BudgetPolicy `json:"policy"`
	CommittedUsage  models.BudgetUsage  `json:"committedUsage,omitempty"`
	EffectiveUsage  models.BudgetUsage  `json:"effectiveUsage,omitempty"`
	Allocated       models.BudgetPolicy `json:"allocated,omitempty"`
	TurnReservation models.BudgetUsage  `json:"turnReservation,omitempty"`
	Remaining       models.BudgetPolicy `json:"remaining"`
	// MinimumChild is the floor for one delegated, forked or task child. A
	// positive value applies only when the parent dimension is bounded.
	MinimumChild models.BudgetPolicy `json:"minimumChild,omitempty"`
}

func remainingBudgetDimension(limit, used, allocated int64) int64 {
	if limit == 0 {
		return 0
	}
	remaining := limit - used - allocated
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ResolveHostedAgentTarget resolves reference to the canonical ID of one
// eligible Agent.
func ResolveHostedAgentTarget(reference string, eligible []agentrun.HostedAgentTarget) (string, error) {
	reference = strings.TrimSpace(reference)
	for _, target := range eligible {
		if reference == target.ID {
			return target.ID, nil
		}
	}
	matches := make([]string, 0, 1)
	for _, target := range eligible {
		if strings.EqualFold(reference, strings.TrimSpace(target.DisplayName)) {
			matches = append(matches, target.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("Agent name %q is ambiguous; use canonical ID %s", reference, strings.Join(matches, " or "))
	}
	ids := make([]string, 0, len(eligible))
	for _, target := range eligible {
		ids = append(ids, target.ID)
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("Agent %q is not eligible; no delegation targets are available", reference)
	}
	return "", fmt.Errorf("Agent %q is not eligible; use canonical ID %s", reference, strings.Join(ids, ", "))
}

// CloneHostedCredentialReference copies reference.
func CloneHostedCredentialReference(reference *capability.CredentialReference) *capability.CredentialReference {
	if reference == nil {
		return nil
	}
	cloned := *reference
	return &cloned
}

func cloneHostedModelActions(values []capability.ModelAction) []capability.ModelAction {
	if values == nil {
		return nil
	}
	result := make([]capability.ModelAction, len(values))
	for index, value := range values {
		result[index] = value
		result[index].InputSchema = maps.Clone(value.InputSchema)
		if value.BindingTool != nil {
			tool := *value.BindingTool
			tool.Arguments = maps.Clone(value.BindingTool.Arguments)
			result[index].BindingTool = &tool
		}
		if value.SemanticArguments != nil {
			result[index].SemanticArguments = make(map[string]string, len(value.SemanticArguments))
			for role, argument := range value.SemanticArguments {
				result[index].SemanticArguments[role] = argument
			}
		}
	}
	return result
}
