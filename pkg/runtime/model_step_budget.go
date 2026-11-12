package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// StepBudgetPlanner is implemented by producers that know what a step will
// cost before running it (the model producer). The loop adds the binding's
// reservation and sets Turns to 1. Deterministic producers do not implement
// it: their reservation is the floor with Turns 1.
type StepBudgetPlanner interface {
	PlanStepBudget(ctx context.Context, in agentrun.StepInput) (models.BudgetUsage, error)
}

const (
	stepProtocolInputReserveTokens int64 = 6144
	stepMediaReserveTokens         int64 = 4096
	stepMinimumOutputTokens        int64 = 64
	// stepMaximumProviderAttempts includes the initial provider exchange and
	// the host's single JSON correction retry; both are reserved up front.
	stepMaximumProviderAttempts  int64 = capability.HostedMaximumProviderAttempts
	stepRepairInputReserveTokens int64 = capability.HostedRepairInputReserveTokens
)

// EstimateModelStepInputTokens is the provider-neutral input estimate of one
// provider attempt of req: the protocol reserve, two UTF-8 bytes per token of
// the request JSON (model credential and image bytes excluded) and a fixed
// reserve per image part.
func EstimateModelStepInputTokens(req models.ModelStepRequest) (int64, error) {
	estimate := req
	estimate.ModelCredential = nil
	images := int64(0)
	if req.Entries != nil {
		estimate.Entries = make([]models.SessionEntryView, len(req.Entries))
	}
	for i, entry := range req.Entries {
		if entry.User != nil {
			user := *entry.User
			user.Content, images = estimateStripImages(user.Content, images)
			entry.User = &user
		}
		if entry.Tool != nil {
			tool := *entry.Tool
			tool.Result.Content, images = estimateStripImages(tool.Result.Content, images)
			entry.Tool = &tool
		}
		estimate.Entries[i] = entry
	}
	data, err := json.Marshal(estimate)
	if err != nil {
		return 0, fmt.Errorf("estimate model step input: %w", err)
	}
	return stepProtocolInputReserveTokens + estimateStepJSONTokens(data) + images*stepMediaReserveTokens, nil
}

func estimateStripImages(parts []models.ContentPart, images int64) ([]models.ContentPart, int64) {
	out := make([]models.ContentPart, len(parts))
	for i, part := range parts {
		if part.Type == models.ContentPartImage {
			images++
		}
		part.DataBase64 = ""
		out[i] = part
	}
	return out, images
}

// estimateStepJSONTokens counts two UTF-8 bytes per token, conservative for
// the textual prompts, schemas and results a step carries.
func estimateStepJSONTokens(data []byte) int64 {
	return (int64(len(data)) + 1) / 2
}

// modelStepReservationInput is the input to reserve for one model step: every
// provider attempt plus the host's correction message (D12), doubled when
// compaction is due (a summary call may run before the step).
func modelStepReservationInput(req models.ModelStepRequest, compactionDue bool) (int64, error) {
	perAttempt, err := EstimateModelStepInputTokens(req)
	if err != nil {
		return 0, err
	}
	input := perAttempt*stepMaximumProviderAttempts + stepRepairInputReserveTokens
	if compactionDue {
		input *= 2
	}
	return input, nil
}

// planModelStepReservation turns an input estimate into the step's token
// reservation against the Run's remaining capacity (controlRunBudget's
// Remaining for the step). It returns a *BudgetAdmissionError (wrapping
// ErrBudgetExhausted) when the remaining capacity cannot carry the input plus
// the minimum output.
func planModelStepReservation(policy, remaining models.BudgetPolicy, estimatedInput int64) (models.BudgetUsage, error) {
	refuse := func(dimension string, required, left int64, reservation models.BudgetUsage) (models.BudgetUsage, error) {
		return models.BudgetUsage{}, &models.BudgetAdmissionError{Admission: models.BudgetAdmission{
			Reason: models.BudgetAdmissionHostedInput, Dimension: dimension,
			Required: required, Remaining: left, Reservation: reservation,
		}}
	}
	reservation := models.BudgetUsage{}
	if policy.MaxInputTokens > 0 {
		if remaining.MaxInputTokens < estimatedInput {
			return refuse("input_tokens", estimatedInput, remaining.MaxInputTokens, models.BudgetUsage{InputTokens: estimatedInput})
		}
		reservation.InputTokens = estimatedInput
	}
	if policy.MaxTotalTokens > 0 {
		if remaining.MaxTotalTokens < estimatedInput+stepMinimumOutputTokens {
			return refuse("total_tokens", estimatedInput+stepMinimumOutputTokens, remaining.MaxTotalTokens,
				models.BudgetUsage{InputTokens: estimatedInput, OutputTokens: stepMinimumOutputTokens})
		}
		reservation.InputTokens = estimatedInput
		reservation.OutputTokens = remaining.MaxTotalTokens - estimatedInput
	}
	if policy.MaxOutputTokens > 0 {
		if remaining.MaxOutputTokens < stepMinimumOutputTokens {
			return refuse("output_tokens", stepMinimumOutputTokens, remaining.MaxOutputTokens,
				models.BudgetUsage{OutputTokens: stepMinimumOutputTokens})
		}
		if reservation.OutputTokens == 0 || remaining.MaxOutputTokens < reservation.OutputTokens {
			reservation.OutputTokens = remaining.MaxOutputTokens
		}
	}
	return reservation, nil
}
