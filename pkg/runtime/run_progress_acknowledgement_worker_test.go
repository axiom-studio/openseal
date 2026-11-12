package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type runProgressWorkerFixture struct {
	store        *sqlite.Store
	catalog      *skill.Catalog
	endpoint     *models.ExternalConversationEndpoint
	conversation *models.Conversation
	inbound      *models.ChannelMessage
	run          *models.AgentRun
	now          time.Time
}

func newRunProgressWorkerFixture(t *testing.T, provider string, operations []capability.ConversationDeliveryOperation) runProgressWorkerFixture {
	t.Helper()
	return newRunProgressWorkerFixtureOn(t, newTestKernelStore(t), provider, operations)
}

// newRunProgressWorkerFixtureOn builds the fixture in store, so a test can
// pass a store it also holds its own connection to (newTestSQLiteStore).
func newRunProgressWorkerFixtureOn(t *testing.T, store *sqlite.Store, provider string, operations []capability.ConversationDeliveryOperation) runProgressWorkerFixture {
	t.Helper()
	ctx := context.Background()
	catalog, endpoint := externalConversationEndpointFixtureOn(t, ctx, store, provider, operations)
	conversations := runtimeconversation.NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Support thread",
		Origin:         &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "progress-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "speaker"},
		Intent: models.MessageIntentQuestion, Content: "Can you check this?",
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "progress-inbound",
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult, err := agentrun.NewRunCommandService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: endpoint.Scope, Kind: models.RunKindConversation, Owner: endpoint.Owner,
		AssignedAgentID: endpoint.DeploymentID, Goal: "Check the deployment", Source: models.RunSourceChat,
		IdempotencyKey: "progress-run", Actor: models.ActivityActor{Type: "service", ID: "channel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := activity.NewRunActivityService(store, store).TransitionRun(ctx, endpoint.Scope, runResult.Run.ID, activity.RunTransitionRequest{
		ExpectedRevision: runResult.Run.Revision, Status: models.AgentRunStatusRunning,
		Summary: "Inspecting the current deployment", EventType: "run.observed", Actor: models.ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &models.ExternalConversationInboxItem{
		ID: "progress-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: models.NormalizedExternalConversationEvent{
			ID: "progress-event", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "conversation", ExternalThreadID: "thread", ExternalMessageID: "message",
			ExternalParticipantID: "speaker", Text: "Can you check this?", OrderingKey: "conversation:message", OccurredAt: now,
		},
		Status: models.ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
		ConversationID: conversation.ID, ChannelMessageID: inbound.Message.ID, RunID: running.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: now,
	}
	if _, replayed, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil || replayed {
		t.Fatalf("store inbox replayed=%t err=%v", replayed, err)
	}
	return runProgressWorkerFixture{store: store, catalog: catalog, endpoint: endpoint, conversation: conversation, inbound: inbound.Message, run: running, now: now}
}

func TestRunProgressAcknowledgementWorkerUsesNativeProgressCapability(t *testing.T) {
	fixture := newRunProgressWorkerFixture(t, "slack", []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator,
	})
	var rendered agentrun.RunProgressAcknowledgementRequest
	renderer := RunProgressAcknowledgementRendererFunc(func(_ context.Context, request agentrun.RunProgressAcknowledgementRequest) (string, error) {
		rendered = request
		return "Inspecting it, obviously.", nil
	})
	worker, err := NewRunProgressAcknowledgementWorker(fixture.store, fixture.catalog, renderer, RunProgressAcknowledgementWorkerConfig{
		MinimumInterval: time.Nanosecond,
	}, models.WithClock(func() time.Time { return fixture.now.Add(time.Millisecond) }))
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := worker.ProcessScope(context.Background(), fixture.endpoint.Scope)
	if err != nil || len(deliveries) != 1 || deliveries[0].Operation != capability.ConversationDeliveryTypingIndicator ||
		deliveries[0].Parameters["status"] != "Inspecting it, obviously." || deliveries[0].ExternalThreadID != "thread" ||
		deliveries[0].Correlation == nil || !strings.HasPrefix(deliveries[0].Correlation.Phase, "progress-") {
		t.Fatalf("native progress delivery = %#v, %v", deliveries, err)
	}
	if rendered.Snapshot.Goal != "Check the deployment" || rendered.Snapshot.ActivitySummary != "Inspecting the current deployment" ||
		rendered.MaxSentences != 2 || rendered.MaxCharacters != 100 {
		t.Fatalf("renderer request = %#v", rendered)
	}
	messages, _ := fixture.store.ListChannelMessages(context.Background(), models.ChannelMessageFilter{
		Scope: fixture.endpoint.Scope, ConversationID: fixture.conversation.ID, Limit: 10,
	})
	if len(messages) != 1 {
		t.Fatalf("native progress created a chat message: %#v", messages)
	}
	second, err := worker.ProcessScope(context.Background(), fixture.endpoint.Scope)
	if err != nil || len(second) != 0 {
		t.Fatalf("duplicate native progress = %#v, %v", second, err)
	}
}

func TestRunProgressAcknowledgementWorkerFallsBackToText(t *testing.T) {
	fixture := newRunProgressWorkerFixture(t, "webchat", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend})
	renderer := RunProgressAcknowledgementRendererFunc(func(context.Context, agentrun.RunProgressAcknowledgementRequest) (string, error) {
		return "I am checking now.", nil
	})
	worker, err := NewRunProgressAcknowledgementWorker(fixture.store, fixture.catalog, renderer, RunProgressAcknowledgementWorkerConfig{
		MinimumRunAge: time.Nanosecond, MinimumInterval: time.Nanosecond,
	}, models.WithClock(func() time.Time { return fixture.now.Add(time.Minute) }))
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := worker.ProcessScope(context.Background(), fixture.endpoint.Scope)
	if err != nil || len(deliveries) != 1 || deliveries[0].Operation != capability.ConversationDeliveryMessageSend {
		t.Fatalf("text fallback delivery = %#v, %v", deliveries, err)
	}
	messages, _ := fixture.store.ListChannelMessages(context.Background(), models.ChannelMessageFilter{
		Scope: fixture.endpoint.Scope, ConversationID: fixture.conversation.ID, Limit: 10,
	})
	if len(messages) != 2 || messages[1].Intent != models.MessageIntentAcknowledgment || messages[1].Content != "I am checking now." {
		t.Fatalf("text fallback messages = %#v", messages)
	}
}

func TestRenderedRunProgressAcknowledgementIsBounded(t *testing.T) {
	for _, test := range []struct {
		value string
		ok    bool
	}{
		{value: "Checking it now.", ok: true},
		{value: "Checking it now. Patience, please.", ok: true},
		{value: "One. Two. Three.", ok: false},
		{value: strings.Repeat("a", 101), ok: false},
		{value: "line one\nline two", ok: false},
	} {
		_, err := validateRenderedRunProgressAcknowledgement(test.value, 2, 100)
		if (err == nil) != test.ok {
			t.Fatalf("value %q: err=%v", test.value, err)
		}
	}
}

type progressAckStepHost struct {
	requests []models.ModelStepRequest
	reply    func(models.ModelStepRequest) models.ModelStepResult
}

func (h *progressAckStepHost) ExecuteModelStep(_ context.Context, req models.ModelStepRequest) (models.ModelStepResult, error) {
	h.requests = append(h.requests, req)
	return h.reply(req), nil
}

func (h *progressAckStepHost) ExecuteHostTool(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
	return models.HostToolResult{}, models.ErrHostToolUnsupported
}

func progressAckResult(req models.ModelStepRequest, text string, calls ...models.ModelToolCall) models.ModelStepResult {
	stop := models.ModelStopEnd
	if len(calls) > 0 {
		stop = models.ModelStopToolCalls
	}
	return models.ModelStepResult{
		APIVersion: models.ModelStepAPIVersion, StepID: req.StepID,
		Assistant: models.AssistantEntry{
			Provider: models.ModelStepProviderAnthropic, Model: "test-model", SystemDigest: "sys", ToolsDigest: "tools",
			Text: text, ToolCalls: calls, StopReason: stop,
		},
		Model: models.ModelStepModelInfo{Provider: models.ModelStepProviderAnthropic, Model: "test-model"},
	}
}

func progressAckRequest() agentrun.RunProgressAcknowledgementRequest {
	return agentrun.RunProgressAcknowledgementRequest{
		Snapshot: agentrun.RunProgressSnapshot{
			RunID: "run-1", Scope: models.Scope{Kind: "workspace", ID: "workspace-1"}, AgentID: "agent-1",
			Goal: "Check the deployment", Status: models.AgentRunStatusRunning,
			ActivityID: "event-1", ActivityType: "run.observed", ActivitySummary: "Inspecting the current deployment",
		},
		MaxSentences: 2, MaxCharacters: 100,
	}
}

func TestProgressAckIsSessionlessToolFreeModelStep(t *testing.T) {
	host := &progressAckStepHost{reply: func(req models.ModelStepRequest) models.ModelStepResult {
		return progressAckResult(req, "  Looking at the deployment now.  ")
	}}
	renderer := &modelStepProgressAcknowledgementRenderer{
		host: host, agentID: "agent-1", definitionID: "def", definitionVersion: "1",
		system: []string{"You are cheerful.", runProgressAcknowledgementSystemInstruction},
	}
	text, err := renderer.RenderRunProgressAcknowledgement(context.Background(), progressAckRequest())
	if err != nil {
		t.Fatal(err)
	}
	if text != "Looking at the deployment now." {
		t.Fatalf("text = %q", text)
	}
	if len(host.requests) != 1 {
		t.Fatalf("requests = %d", len(host.requests))
	}
	req := host.requests[0]
	if req.SessionID != "" || req.Tools != nil || req.Purpose != models.ModelStepPurposeAcknowledgement {
		t.Fatalf("request is not a sessionless tool-free acknowledgement: session=%q tools=%v purpose=%q", req.SessionID, req.Tools, req.Purpose)
	}
	if req.Stream != (models.ModelStepStream{}) {
		t.Fatalf("acknowledgement must not stream: %+v", req.Stream)
	}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	if req.RunID != "run-1" || req.AgentID != "agent-1" || req.DefinitionID != "def" || req.DefinitionVersion != "1" {
		t.Fatalf("identity = %+v", req)
	}
	wantStep := "progress-ack-" + models.SHA256DigestString("run-1\x00event-1\x00" + string(models.AgentRunStatusRunning))[:16]
	if req.StepID != wantStep {
		t.Fatalf("step id = %q, want %q", req.StepID, wantStep)
	}
	if got := req.System.AgentInstructions; len(got) != 2 || got[1] != runProgressAcknowledgementSystemInstruction {
		t.Fatalf("system = %v", got)
	}
	if len(req.Entries) != 1 || req.Entries[0].Type != models.SessionEntryUser {
		t.Fatalf("entries = %+v", req.Entries)
	}
	var facts map[string]interface{}
	if err := json.Unmarshal([]byte(req.Entries[0].User.Content[0].Text), &facts); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 4 || facts["goal"] != "Check the deployment" || facts["status"] != "running" ||
		facts["activityType"] != "run.observed" || facts["activitySummary"] != "Inspecting the current deployment" {
		t.Fatalf("facts = %v", facts)
	}
}

func TestProgressAckRejectsToolCallsAndOverlongText(t *testing.T) {
	for name, reply := range map[string]func(models.ModelStepRequest) models.ModelStepResult{
		"tool call": func(req models.ModelStepRequest) models.ModelStepResult {
			return progressAckResult(req, "On it.", models.ModelToolCall{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)})
		},
		"overlong": func(req models.ModelStepRequest) models.ModelStepResult {
			return progressAckResult(req, strings.Repeat("a", 101))
		},
		"too many sentences": func(req models.ModelStepRequest) models.ModelStepResult {
			return progressAckResult(req, "One. Two. Three.")
		},
		"empty": func(req models.ModelStepRequest) models.ModelStepResult {
			return progressAckResult(req, "  ")
		},
	} {
		t.Run(name, func(t *testing.T) {
			renderer := &modelStepProgressAcknowledgementRenderer{host: &progressAckStepHost{reply: reply}, agentID: "agent-1"}
			text, err := renderer.RenderRunProgressAcknowledgement(context.Background(), progressAckRequest())
			if err == nil {
				t.Fatalf("accepted %q", text)
			}
			if name == "tool call" && err.Error() != "Agent returned an actionable progress acknowledgement" {
				t.Fatalf("err = %v", err)
			}
		})
	}
	other := progressAckRequest()
	other.Snapshot.AgentID = "agent-2"
	host := &progressAckStepHost{reply: func(req models.ModelStepRequest) models.ModelStepResult { return progressAckResult(req, "Fine.") }}
	if _, err := (&modelStepProgressAcknowledgementRenderer{host: host, agentID: "agent-1"}).RenderRunProgressAcknowledgement(context.Background(), other); err == nil || len(host.requests) != 0 {
		t.Fatalf("rendered for another Agent: err=%v requests=%d", err, len(host.requests))
	}
}
