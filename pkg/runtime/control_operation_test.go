package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/google/uuid"
)

const operationTestWorker = "operation-worker"

var operationTestEngage = agentrun.HostedRunbookOperation{
	DefinitionID: "def-engage", DefinitionVersion: "3", Entrypoint: "engage-now",
	Name: "Engage accounts", Description: "Send the reviewed engagement sequence.",
	InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false, "required": []interface{}{"account"},
		"properties": map[string]interface{}{"account": map[string]interface{}{"type": "string"}},
	},
}

// operationCall is one claimed Run with a run session; addCall records a
// run_operation call in a finished, applied step, ready to settle.
type operationCall struct {
	t         *testing.T
	ctx       context.Context
	store     forkTestStore
	sessions  models.AgentSessionStore
	scope     models.Scope
	run       *models.AgentRun
	session   *models.AgentSession
	step      *models.AgentTurn
	assistant *models.SessionEntry
	entry     *models.SessionEntry
	exec      *ControlExecutor
	authority agentrun.ControlAuthority
}

func newOperationCall(t *testing.T, store forkTestStore, setup func(scope models.Scope, req *models.CreateAgentRunRequest)) *operationCall {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "op-" + uuid.NewString()[:8]}
	req := models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-a"}, AssignedAgentID: "agent-a",
		Goal: "engage the account", Source: models.RunSourceObjective, Context: map[string]interface{}{"projectId": "p1"},
	}
	if setup != nil {
		setup(scope, &req)
	}
	created, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: operationTestWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != created.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	opened, err := store.CreateRunAgentSession(ctx, models.CreateRunAgentSessionRequest{
		Scope: scope, OwnerRunID: claimed.ID, AgentID: claimed.AssignedAgentID,
		Lease: models.AgentRunLeaseGuard{WorkerID: operationTestWorker, Now: now}, Now: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	var reporting models.ConversationStore
	if conversations, ok := store.(models.ConversationStore); ok {
		reporting = conversations
	}
	forks := agentrun.NewRunForkCoordinator(store)
	forks.SetAcceptedRunExecutionPreparer(&acceptedCreationTestPreparer{
		id: operationTestEngage.DefinitionID, version: operationTestEngage.DefinitionVersion,
	})
	return &operationCall{
		t: t, ctx: ctx, store: store, sessions: store, scope: scope, run: claimed, session: opened.Session,
		exec:      &ControlExecutor{Portfolio: store, Sessions: store, Forks: forks, Reporting: reporting},
		authority: agentrun.ControlAuthority{Operations: []agentrun.HostedRunbookOperation{operationTestEngage}},
	}
}

// addCall records step sequence n of the claimed Run calling run_operation
// with arguments, finished and applied.
func (c *operationCall) addCall(n int64, arguments string) {
	c.t.Helper()
	now := time.Now().UTC()
	lease := models.AgentRunLeaseGuard{WorkerID: operationTestWorker, Now: now}
	stepID := fmt.Sprintf("%s-step-%d", c.run.ID, n)
	callID := fmt.Sprintf("call-op-%d", n)
	raw := json.RawMessage(arguments)
	assistant := &models.SessionEntry{
		ID: models.SessionEntryID(c.session.ID, models.AssistantEntryKey(stepID)), RunID: c.run.ID, StepID: stepID, Step: n,
		Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
			Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopToolCalls,
			ToolCalls: []models.ModelToolCall{{ID: callID, Name: string(models.ControlToolRunOperation), Arguments: raw}},
		},
	}
	tool := &models.SessionEntry{
		ID: models.SessionEntryID(c.session.ID, models.ToolEntryKey(stepID, callID)), RunID: c.run.ID, StepID: stepID, Step: n,
		Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: callID,
		Tool: &models.ToolEntry{Name: string(models.ControlToolRunOperation), Arguments: raw, SideEffect: models.ToolSideEffectWrite},
	}
	appended, err := c.sessions.AppendAgentSessionEntries(c.ctx, models.AppendAgentSessionEntriesRequest{
		Scope: c.scope, SessionID: c.session.ID, RunID: c.run.ID, Lease: &lease, Entries: []*models.SessionEntry{assistant, tool}, Now: now,
	})
	if err != nil {
		c.t.Fatalf("append operation step: %v", err)
	}
	c.run = c.getRun(c.run.ID)
	c.run.LastAppliedTurn = n
	c.assistant, c.entry = appended.Entries[0], appended.Entries[1]
	c.step = &models.AgentTurn{ID: stepID, RunID: c.run.ID, Sequence: n, Status: models.AgentTurnStatusCompleted}
}

func (c *operationCall) context() agentrun.ControlContext {
	return NewControlContext(agentrun.StepToolsInput{
		Run: c.run, Step: c.step, Session: c.session, WorkerID: operationTestWorker,
		Binding: &agentrun.TurnRunnerBinding{Authority: c.authority}, Assistant: c.assistant,
	}, c.entry)
}

func (c *operationCall) settle() ControlResult {
	c.t.Helper()
	result, err := c.exec.Settle(c.ctx, c.context())
	if err != nil {
		c.t.Fatalf("settle run_operation: %v", err)
	}
	if result.Entry != nil {
		c.entry = result.Entry
	}
	return result
}

func (c *operationCall) getRun(id string) *models.AgentRun {
	c.t.Helper()
	run, err := c.store.GetAgentRun(c.ctx, c.scope, id)
	if err != nil {
		c.t.Fatalf("get run %s: %v", id, err)
	}
	return run
}

func (c *operationCall) meta() *ControlEntryMeta {
	c.t.Helper()
	entry, err := c.sessions.GetSessionEntry(c.ctx, c.scope, c.entry.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	meta, err := DecodeControlEntryMeta(entry)
	if err != nil || meta == nil {
		c.t.Fatalf("control meta = %#v, %v", meta, err)
	}
	return meta
}

// finishChild ends the operation child with status and output, then resolves
// its edge as the worker that finished it would.
func (c *operationCall) finishChild(id string, status models.AgentRunStatus, output map[string]interface{}, failure string) {
	c.t.Helper()
	const worker = "operation-child-worker"
	child := c.getRun(id)
	for i := 0; child.Status == models.AgentRunStatusQueued; i++ {
		if i > 10 {
			c.t.Fatalf("child %s was never claimed", id)
		}
		if _, err := c.store.ClaimNextAgentRun(c.ctx, models.AgentRunClaim{
			Scope: c.scope, WorkerID: worker, Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
		}); err != nil {
			c.t.Fatalf("claim child: %v", err)
		}
		child = c.getRun(id)
	}
	done, _, err := activity.NewRunActivityService(c.store, c.store).TransitionRun(c.ctx, c.scope, id, activity.RunTransitionRequest{
		ExpectedRevision: child.Revision, Status: status, LeaseOwner: worker, Output: output, Error: failure,
		Actor: models.ActivityActor{Type: "worker", ID: worker},
	})
	if err != nil {
		c.t.Fatalf("finish child %s: %v", id, err)
	}
	if _, err := c.exec.Forks.CompleteChild(c.ctx, done, models.ActivityActor{Type: "worker", ID: worker}); err != nil {
		c.t.Fatalf("complete child %s: %v", id, err)
	}
}

func (c *operationCall) reclaim() {
	c.t.Helper()
	claimed, err := c.store.ClaimNextAgentRun(c.ctx, models.AgentRunClaim{
		Scope: c.scope, WorkerID: operationTestWorker, Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != c.run.ID {
		c.t.Fatalf("reclaim = %#v, %v", claimed, err)
	}
	c.run = claimed
	entry, err := c.sessions.GetSessionEntry(c.ctx, c.scope, c.entry.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	c.entry = entry
}

func (c *operationCall) resume() ControlResume {
	c.t.Helper()
	resumed, err := c.exec.Resume(c.ctx, c.context())
	if err != nil {
		c.t.Fatalf("resume run_operation: %v", err)
	}
	if resumed.Resolved && resumed.Result.Entry != nil {
		c.entry = resumed.Result.Entry
	}
	return resumed
}

// startOperation settles one call and returns its child id.
func (c *operationCall) startOperation(n int64, arguments string) string {
	c.t.Helper()
	c.addCall(n, arguments)
	result := c.settle()
	if result.Kind != ControlParked || result.Park == nil || !result.Park.Committed ||
		result.Park.Status != models.AgentRunStatusWaitingForDependency {
		c.t.Fatalf("settle = %#v: %s", result, controlResultText(c.entry.Tool.Result))
	}
	meta := c.meta()
	if meta.Kind != models.ControlToolRunOperation || meta.ForkID != "op-"+c.entry.ID || len(meta.ChildRunIDs) != 1 {
		c.t.Fatalf("meta = %#v", meta)
	}
	if c.entry.State != models.ToolEntryAwaitingChild {
		c.t.Fatalf("entry state = %s", c.entry.State)
	}
	return meta.ChildRunIDs[0]
}

func TestRunOperationValidatesInputSchema(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		cases := []struct {
			name, arguments, code string
		}{
			{"missing required", `{"entrypoint":"engage-now","arguments":{}}`, operationInputInvalidCode},
			{"wrong type", `{"entrypoint":"engage-now","arguments":{"account":7}}`, operationInputInvalidCode},
			{"unknown field", `{"entrypoint":"engage-now","arguments":{"account":"a","extra":true}}`, operationInputInvalidCode},
			{"unknown entrypoint", `{"entrypoint":"other","arguments":{"account":"a"}}`, operationNotAuthorizedCode},
			{"no arguments", `{"entrypoint":"engage-now"}`, controlArgumentsInvalidCode},
			{"unknown top-level", `{"entrypoint":"engage-now","arguments":{"account":"a"},"summary":"x"}`, controlArgumentsInvalidCode},
		}
		for i, tc := range cases {
			c := newOperationCall(t, store, nil)
			c.addCall(int64(i+1), tc.arguments)
			result := c.settle()
			if result.Kind != ControlContinue || c.entry.State != models.ToolEntryError {
				t.Fatalf("%s: result = %#v, entry %s", tc.name, result, c.entry.State)
			}
			value, _ := forkResult(t, c.entry)
			if value["error"] != tc.code {
				t.Fatalf("%s: error = %v, want %s", tc.name, value["error"], tc.code)
			}
			children, err := store.ListAgentRuns(c.ctx, models.AgentRunFilter{Scope: c.scope, ParentRunID: c.run.ID})
			if err != nil || len(children) != 0 {
				t.Fatalf("%s: children = %d, %v", tc.name, len(children), err)
			}
		}
	})
}

func TestRunOperationRecursiveInvocationRefused(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		// The Run is itself an invocation of the same definition and version.
		c := newOperationCall(t, store, func(_ models.Scope, req *models.CreateAgentRunRequest) {
			req.Context["runbookDefinitionId"] = operationTestEngage.DefinitionID
			req.Context["runbookDefinitionVersion"] = operationTestEngage.DefinitionVersion
			req.Context["runbookEntrypoint"] = operationTestEngage.Entrypoint
		})
		c.addCall(1, `{"entrypoint":"engage-now","arguments":{"account":"acme"}}`)
		c.settle()
		value, _ := forkResult(t, c.entry)
		if c.entry.State != models.ToolEntryError || value["error"] != operationRecursiveCode {
			t.Fatalf("entry %s result = %v", c.entry.State, value)
		}
		if got := c.getRun(c.run.ID); got.Status != models.AgentRunStatusRunning {
			t.Fatalf("source status = %s", got.Status)
		}
	})
}

func TestRunOperationTwiceInOneRunCreatesTwoChildren(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newOperationCall(t, store, nil)
		first := c.startOperation(1, `{"entrypoint":"engage-now","arguments":{"account":"acme"}}`)
		child := c.getRun(first)
		if child.ParentRunID != c.run.ID || child.Entrypoint != "engage-now" || child.Kind != models.RunKindAgentWork ||
			child.Goal != "Run Engage accounts" || child.SessionID != "" {
			t.Fatalf("child = %#v", child)
		}
		invocation, _ := child.Context[agentrun.RunbookInvocationContextKey].(map[string]interface{})
		if child.Context["account"] != "acme" || invocation["summary"] != "Run Engage accounts" ||
			child.Context["runbookDefinitionId"] != "def-engage" || child.Context["runbookDefinitionVersion"] != "3" {
			t.Fatalf("child context = %#v", child.Context)
		}
		if got := c.getRun(c.run.ID); got.Status != models.AgentRunStatusWaitingForDependency {
			t.Fatalf("source status = %s", got.Status)
		}
		c.finishChild(first, models.AgentRunStatusCompleted, map[string]interface{}{"summary": "Engaged acme."}, "")
		c.reclaim()
		if resumed := c.resume(); !resumed.Resolved {
			t.Fatalf("resume = %#v", resumed)
		}
		second := c.startOperation(2, `{"entrypoint":"engage-now","arguments":{"account":"globex"}}`)
		if second == first {
			t.Fatalf("second operation reused child %s", first)
		}
		children, err := store.ListAgentRuns(c.ctx, models.AgentRunFilter{Scope: c.scope, ParentRunID: c.run.ID})
		if err != nil || len(children) != 2 {
			t.Fatalf("children = %d, %v", len(children), err)
		}
	})
}

func TestRunOperationPostsStartedMessagePerChild(t *testing.T) {
	store := newTestKernelStore(t)
	service := runtimeconversation.NewConversationService(store)
	var trigger *models.ChannelMessage
	var conversation *models.Conversation
	c := newOperationCall(t, store, func(scope models.Scope, req *models.CreateAgentRunRequest) {
		seedTestSpace(t, store, scope)
		created, _, err := service.CreateConversation(context.Background(), runtimeconversation.CreateConversationRequest{
			Scope: scope, Owner: req.Owner, Title: "Agent work", IdempotencyKey: "op-channel",
		})
		if err != nil {
			t.Fatal(err)
		}
		conversation = created
		trigger = conversationruntest.PostMessage(t, service, conversation, models.ConversationParticipantUser, models.MessageIntentQuestion, "Engage acme and globex.", "engage")
		req.Kind = models.RunKindConversation
		req.Context[models.ConversationRunConversationIDContextKey] = conversation.ID
		req.Context[models.ConversationRunTriggerIDContextKey] = trigger.ID
	})
	first := c.startOperation(1, `{"entrypoint":"engage-now","arguments":{"account":"acme"}}`)
	invocation, _ := c.getRun(first).Context[agentrun.RunbookInvocationContextKey].(map[string]interface{})
	if source, _ := invocation["sourceMessage"].(map[string]interface{}); source["messageId"] != trigger.ID {
		t.Fatalf("invocation = %#v", invocation)
	}
	c.finishChild(first, models.AgentRunStatusCompleted, map[string]interface{}{"summary": "Engaged acme."}, "")
	c.reclaim()
	c.resume()
	second := c.startOperation(2, `{"entrypoint":"engage-now","arguments":{"account":"globex"}}`)
	// Projecting the same child again (a crash replay) posts nothing more.
	c.exec.projectOperationStart(c.ctx, c.getRun(c.run.ID), "Run Engage accounts", second)

	messages, err := service.ListChannelMessages(c.ctx, models.ChannelMessageFilter{Scope: c.scope, ConversationID: conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	var started []*models.ChannelMessage
	for _, message := range messages {
		if strings.HasPrefix(message.Content, "Started: ") {
			started = append(started, message)
		}
	}
	if len(started) != 2 {
		t.Fatalf("started messages = %d of %#v", len(started), messages)
	}
	for i, want := range []string{first, second} {
		message := started[i]
		if message.Content != "Started: Run Engage accounts" || message.Intent != models.MessageIntentUpdate ||
			message.ReplyToMessageID != trigger.ID || len(message.References) != 1 ||
			message.References[0] != (models.ConversationReference{Kind: models.ConversationReferenceRun, ID: want}) {
			t.Fatalf("started message %d = %#v", i, message)
		}
	}
}

func TestRunOperationResultIsSavedOperationOutput(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newOperationCall(t, store, nil)
		childID := c.startOperation(1, `{"entrypoint":"engage-now","arguments":{"account":"acme"}}`)
		c.finishChild(childID, models.AgentRunStatusCompleted, map[string]interface{}{
			"summary": "Engaged acme.", "receipt": "internal-only",
		}, "")
		c.reclaim()
		resumed := c.resume()
		if !resumed.Resolved || c.entry.State != models.ToolEntryCompleted || c.entry.Tool.Result.IsError {
			t.Fatalf("resume = %#v entry %s", resumed, c.entry.State)
		}
		value, raw := forkResult(t, c.entry)
		if value["status"] != "completed" || value["entrypoint"] != "engage-now" || value["runId"] != childID ||
			value["output"] != "Engaged acme." || strings.Contains(raw, "internal-only") {
			t.Fatalf("result = %s", raw)
		}

		// A failed operation is an is_error result with no output.
		failed := newOperationCall(t, store, nil)
		failedID := failed.startOperation(1, `{"entrypoint":"engage-now","arguments":{"account":"acme"}}`)
		failed.finishChild(failedID, models.AgentRunStatusFailed, map[string]interface{}{"summary": "partial"}, "step 2 failed")
		failed.reclaim()
		failed.resume()
		value, raw = forkResult(t, failed.entry)
		if failed.entry.State != models.ToolEntryError || !failed.entry.Tool.Result.IsError ||
			value["status"] != "failed" || value["output"] != "" || value["error"] != "step 2 failed" {
			t.Fatalf("failed result = %s (entry %s)", raw, failed.entry.State)
		}
		if got := failed.getRun(failed.run.ID); got.Status != models.AgentRunStatusRunning {
			t.Fatalf("source after failed operation = %s", got.Status)
		}
	})
}

func TestRunOperationResumeWhileChildRunsReparks(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newOperationCall(t, store, nil)
		c.startOperation(1, `{"entrypoint":"engage-now","arguments":{"account":"acme"}}`)
		// Force the source back to running under the lease to resume early,
		// as a claim of a spuriously woken Run would.
		source := c.getRun(c.run.ID)
		if source.Status != models.AgentRunStatusWaitingForDependency {
			t.Fatalf("source = %s", source.Status)
		}
		c.run = source
		c.run.Status = models.AgentRunStatusRunning
		c.run.LeaseOwner = operationTestWorker
		resumed, err := c.exec.Resume(c.ctx, c.context())
		if err != nil {
			t.Fatal(err)
		}
		if resumed.Resolved || resumed.Park == nil || resumed.Park.Status != models.AgentRunStatusWaitingForDependency ||
			resumed.Park.Wake == nil || resumed.Park.Wake.Reference != c.meta().GroupID {
			t.Fatalf("resume = %#v", resumed)
		}
	})
}

func TestRunOperationsListedInCapabilitiesNoteInOrder(t *testing.T) {
	second := agentrun.HostedRunbookOperation{Entrypoint: "report", Name: "Weekly report", Description: "Summarize the week"}
	authority := agentrun.ControlAuthority{Operations: []agentrun.HostedRunbookOperation{operationTestEngage, second}}
	specs := builtinControlSpecs(authority)
	var spec *ControlToolSpec
	for i := range specs {
		if specs[i].Name == models.ControlToolRunOperation {
			spec = &specs[i]
		}
	}
	if spec == nil {
		t.Fatalf("run_operation not offered: %v", specNames(specs))
	}
	engage := `- engage-now — Engage accounts: Send the reviewed engagement sequence. Input schema: {"additionalProperties":false,"properties":{"account":{"type":"string"}},"required":["account"],"type":"object"}`
	report := `- report — Weekly report: Summarize the week. Input schema: {"type":"object"}`
	if listed := modelsteptest.CapabilityNoteLines(modelstep.ControlTargetItems(authority)); !strings.Contains(listed, engage+"\n"+report) {
		t.Fatalf("note lines = %s", listed)
	}
	entrypoint := spec.InputSchema["properties"].(map[string]interface{})["entrypoint"].(map[string]interface{})
	if _, enum := entrypoint["enum"]; enum || strings.Contains(spec.Description, "engage-now") {
		t.Fatalf("run_operation lists its operations: %v %s", entrypoint, spec.Description)
	}
	for _, s := range builtinControlSpecs(agentrun.ControlAuthority{}) {
		if s.Name == models.ControlToolRunOperation {
			t.Fatal("run_operation offered with no operations")
		}
	}
}
