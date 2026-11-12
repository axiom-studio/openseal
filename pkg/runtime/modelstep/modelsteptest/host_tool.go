package modelsteptest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// FakeHostToolHost answers ExecuteHostTool with scripted replies, one per call.
type FakeHostToolHost struct {
	mu       sync.Mutex
	Requests []models.HostToolRequest
	Replies  []func(context.Context, models.HostToolRequest) (models.HostToolResult, error)
}

func (h *FakeHostToolHost) ExecuteHostTool(ctx context.Context, request models.HostToolRequest) (models.HostToolResult, error) {
	h.mu.Lock()
	h.Requests = append(h.Requests, request)
	index := len(h.Requests) - 1
	h.mu.Unlock()
	if index >= len(h.Replies) {
		return models.HostToolResult{}, errors.New("unexpected host tool call")
	}
	return h.Replies[index](ctx, request)
}

// HostReplyJSON replies with value as one JSON part.
func HostReplyJSON(value interface{}, isError bool) func(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
	return func(_ context.Context, request models.HostToolRequest) (models.HostToolResult, error) {
		encoded, _ := json.Marshal(value)
		return models.HostToolResult{APIVersion: models.HostToolAPIVersion, InvocationID: request.InvocationID,
			Content: []models.ContentPart{{Type: models.ContentPartJSON, Value: encoded}}, IsError: isError}, nil
	}
}

// HostReplyError fails the host call with err.
func HostReplyError(err error) func(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
	return func(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
		return models.HostToolResult{}, err
	}
}

// HostToolSpecFixture is the host tool operation offered in a chat workspace.
func HostToolSpecFixture(t *testing.T, operation string) modelstep.HostToolSpec {
	t.Helper()
	spec, ok := modelstep.LookupHostTool(modelstep.HostToolSpecs(workspace.ChatAuthority("conv_1")), operation)
	if !ok {
		t.Fatalf("no host tool %s", operation)
	}
	return spec
}

// HostToolCallFixture is a leased host tool call of operation in chat conv_1.
func HostToolCallFixture(t *testing.T, operation string, arguments map[string]interface{}) modelstep.HostToolCall {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "42"}
	expires := time.Now().Add(time.Hour)
	return modelstep.HostToolCall{
		Run: &models.AgentRun{ID: "run_1", Scope: scope, Status: models.AgentRunStatusRunning, AssignedAgentID: "agent_1",
			LeaseOwner: "worker_1", LeaseExpiresAt: &expires},
		Step: &models.AgentTurn{ID: "step_1", Scope: scope, RunID: "run_1", Status: models.AgentTurnStatusRunning,
			LeaseOwner: "worker_1", LeaseExpiresAt: &expires},
		Binding: &agentrun.TurnRunnerBinding{DefinitionID: "def_1", DefinitionVersion: "3", ModelActions: []capability.ModelAction{
			{Name: "zeta.send", Description: "Send", Risk: capability.RiskLevel("high"), InputSchema: map[string]interface{}{"type": "object", "required": []interface{}{"to"}}},
			{Name: "alpha.lookup", Description: "Look up", Risk: capability.RiskLevel("low")},
		}},
		SessionID: "ses_1", ToolEntryID: "ent_1", ToolCallID: "toolu_1",
		Spec: HostToolSpecFixture(t, operation), Arguments: arguments,
		Workspace: *workspace.ChatAuthority("conv_1"),
	}
}
