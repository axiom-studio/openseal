package modelsteptest

import (
	"context"
	"maps"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// FixedActionCalls is an ActionCallReader over a fixed set of calls.
type FixedActionCalls map[string]*models.ActionCall

func (f FixedActionCalls) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	call, ok := f[id]
	if !ok {
		return nil, models.ErrActionNotFound
	}
	cloned := *call
	cloned.Output = maps.Clone(call.Output)
	return &cloned, nil
}

// ProducerToolPath is a run-owned session path holding one terminal tool
// entry for toolCallID.
func ProducerToolPath(runID, toolCallID string, state models.ToolEntryState, actionCallID string) *models.AgentSessionPath {
	status := models.ToolResultSucceeded
	if state != models.ToolEntryCompleted {
		status = models.ToolResultFailed
	}
	return &models.AgentSessionPath{Entries: []*models.SessionEntry{{
		ID: "tool:step-1:" + toolCallID, RunID: runID, StepID: "step-1", Type: models.SessionEntryTool, State: state,
		ToolCallID: toolCallID, ActionCallID: actionCallID,
		Tool: &models.ToolEntry{Name: "tool", Result: &models.ToolResult{Status: status, IsError: status != models.ToolResultSucceeded}},
	}}}
}

// RequireValidDeterministicOutput fails t unless out is valid output of a
// deterministic producer.
func RequireValidDeterministicOutput(t *testing.T, out agentrun.StepOutput) {
	t.Helper()
	if err := modelstep.ValidateStepOutput(models.StepProducerDeterministic, out); err != nil {
		t.Fatalf("invalid deterministic output %#v: %v", out, err)
	}
}
