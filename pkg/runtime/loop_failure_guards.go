package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/builtin"
)

// The failure-chain guards of the run loop (SPEC §5.4 "Added guards"):
//
//   - error streak: loopErrorStreakLimit consecutive steps whose tool results
//     are all is_error fail the Run with RunFailureCodeErrorStreak;
//   - image provider rejection: once generate_image failed on a binding with
//     ActionErrorCodeImageProviderRejected, later generate_image calls on that
//     binding in the same Run are refused before dispatch.
//
// Both read only durable state (the Run's session entries and action calls),
// so they hold across claims, parks and crashes like the repeat ledger.

// loopErrorStreakLimit is the number of consecutive all-error steps that
// fails the Run.
const loopErrorStreakLimit = 8

// ActionErrorCodeImageProviderRejected is the ErrorCode of a generate_image
// call the selected image provider rejected. The host's image writer returns
// it as a skillerror.ActionError code so the action worker records it.
const ActionErrorCodeImageProviderRejected = "image_provider_rejected"

// ToolGuardImageProviderRejected is the ToolGuardOutcome.Kind of a
// generate_image call refused by the image provider rejection guard.
const ToolGuardImageProviderRejected = "image_provider_rejected"

// imageProviderRejectedRefusalText is the exact model-facing refusal.
const imageProviderRejectedRefusalText = "The image provider rejected this request; ask the user to change the image settings."

// loopErrorStreak counts the trailing consecutive steps of entries (one Run's
// tool and user entries in sequence order) whose tool results are all
// is_error.
//
//   - A step counts when it has at least one terminal tool entry and every
//     terminal one is is_error. Interrupted entries are ignored: a crash is not
//     the agent's failure. A step whose entries are all interrupted is neutral.
//   - A step with any succeeded result, or with a tool entry that is not
//     terminal (still pending or parked), ends the streak.
//   - A user entry (steering or catch-up) ends the streak: new guidance
//     changes what the agent is trying.
func loopErrorStreak(entries []*SessionEntry) int {
	streak := 0
	type stepTally struct {
		terminal, errors int
		open             bool
	}
	var current *stepTally
	currentStep := int64(-1)
	closeStep := func() {
		if current == nil {
			return
		}
		switch {
		case current.open || current.errors < current.terminal:
			streak = 0
		case current.terminal > 0:
			streak++
		}
		current = nil
	}
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		switch entry.Type {
		case SessionEntryUser:
			closeStep()
			currentStep = -1
			streak = 0
		case SessionEntryTool:
			if current == nil || entry.Step != currentStep {
				closeStep()
				current, currentStep = &stepTally{}, entry.Step
			}
			if entry.State == ToolEntryInterrupted {
				continue
			}
			if entry.Tool == nil || entry.Tool.Result == nil || !entry.State.Terminal() {
				current.open = true
				continue
			}
			if entry.Tool.Result.Status == ToolResultInterrupted {
				continue
			}
			current.terminal++
			if entry.Tool.Result.IsError {
				current.errors++
			}
		}
	}
	closeStep()
	return streak
}

// loopErrorStreakFailure loads the Run's tool and user entries and returns
// the error_streak Run failure when its last loopErrorStreakLimit steps all
// ended in errors, or nil. The executor calls it after every tool entry of a
// step is terminal and no other guard failed the step.
func loopErrorStreakFailure(ctx context.Context, store loopSessionEntryLister, scope Scope, sessionID, runID string) (*guardRunFailure, error) {
	if store == nil || sessionID == "" || runID == "" {
		return nil, errors.New("error streak needs a session store, session and Run")
	}
	var entries []*SessionEntry
	after := int64(0)
	for {
		page, err := store.ListAgentSessionEntries(ctx, SessionEntryFilter{
			Scope: scope, SessionID: sessionID, RunID: runID,
			Types:         []SessionEntryType{SessionEntryTool, SessionEntryUser},
			AfterSequence: after, Limit: maxSessionEntryListLimit,
		})
		if err != nil {
			return nil, fmt.Errorf("load error streak: %w", err)
		}
		entries = append(entries, page...)
		if len(page) < maxSessionEntryListLimit {
			break
		}
		next := page[len(page)-1].Sequence
		if next <= after {
			return nil, errors.New("load error streak: session entries are not in sequence order")
		}
		after = next
	}
	if loopErrorStreak(entries) < loopErrorStreakLimit {
		return nil, nil
	}
	failure, _ := guardRunFailureFor(RunFailureCodeErrorStreak)
	return &failure, nil
}

// imageGenerationTarget is the binding a governed generate_image call runs on.
type imageGenerationTarget struct {
	SkillID         string
	Action          string
	BindingID       string
	BindingRevision int64
}

func (t imageGenerationTarget) isImageGeneration() bool {
	return t.SkillID == builtin.SkillID && t.Action == builtin.GenerateImage
}

// priorImageProviderRejection returns this Run's earlier generate_image call
// on target's binding (same binding revision) that the image provider
// rejected, or nil. Rewording the prompt still selects the same provider, so
// the rejection holds for the rest of the Run; a binding revision is a
// configuration change and starts clean. Calls of every origin count.
func priorImageProviderRejection(ctx context.Context, actions loopActionCallLister, scope Scope, runID string, target imageGenerationTarget) (*ActionCall, error) {
	if !target.isImageGeneration() {
		return nil, nil
	}
	if actions == nil || runID == "" {
		return nil, errors.New("image rejection guard needs an action store and Run")
	}
	for offset := 0; ; offset += loopActionCallPageSize {
		page, err := actions.ListActionCalls(ctx, ActionFilter{
			Scope: scope, RunID: runID, Status: []ActionCallStatus{ActionCallStatusFailed},
			Limit: loopActionCallPageSize, Offset: offset,
		})
		if err != nil {
			return nil, fmt.Errorf("load image rejections: %w", err)
		}
		for _, call := range page {
			if call != nil && call.RunID == runID && call.Status == ActionCallStatusFailed &&
				call.ErrorCode == ActionErrorCodeImageProviderRejected &&
				call.SkillID == target.SkillID && call.Action == target.Action &&
				call.BindingID == target.BindingID && call.BindingRevision == target.BindingRevision {
				return call, nil
			}
		}
		if len(page) < loopActionCallPageSize {
			return nil, nil
		}
	}
}

// imageProviderRejectionRefusal is the refused tool result and guard outcome
// for a generate_image call after prior's rejection. The executor transitions
// the pending entry to error with them; nothing is dispatched.
func imageProviderRejectionRefusal(prior *ActionCall) (*ToolResult, *ToolGuardOutcome) {
	value := map[string]interface{}{
		"status": string(ToolResultRefused),
		"code":   ActionErrorCodeImageProviderRejected,
		"error":  imageProviderRejectedRefusalText,
	}
	guard := &ToolGuardOutcome{Kind: ToolGuardImageProviderRejected}
	if prior != nil && prior.ID != "" {
		value["priorEvidenceRef"] = "action-call:" + prior.ID
	}
	encoded, _ := json.Marshal(value)
	return &ToolResult{
		Status: ToolResultRefused, IsError: true,
		Content: []ContentPart{{Type: ContentPartJSON, Value: encoded}},
	}, guard
}
