package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/google/uuid"
)

type delegateTestStore interface {
	models.KernelStore
	models.CollaborationKernelStore
	models.AgentSessionStore
}

const delegateWorker = "worker-delegate"

var delegateTargets = []agentrun.HostedAgentTarget{{ID: "specialist", DisplayName: "Specialist", Purpose: "Analyses releases"}}

// delegateFixture is one claimed source Run of Agent "manager" whose session
// holds steps calling control tools.
type delegateFixture struct {
	t       *testing.T
	store   delegateTestStore
	collab  *collaboration.CollaborationService
	exec    *ControlExecutor
	scope   models.Scope
	run     *models.AgentRun
	session *models.AgentSession
	step    *models.AgentTurn
	entry   *models.SessionEntry
}

func newDelegateFixture(t *testing.T, store delegateTestStore, mutate func(*models.CreateAgentRunRequest)) *delegateFixture {
	t.Helper()
	return newDelegateFixtureIn(t, store, models.Scope{Kind: "tenant", ID: "delegate-" + uuid.NewString()[:8]}, mutate)
}

// newDelegateFixtureIn creates and claims a source Run in scope; no other Run
// of scope may be queued.
func newDelegateFixtureIn(t *testing.T, store delegateTestStore, scope models.Scope, mutate func(*models.CreateAgentRunRequest)) *delegateFixture {
	t.Helper()
	req := models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "manager"}, AssignedAgentID: "manager",
		Goal: "Ship the release", Source: models.RunSourceManual,
	}
	if mutate != nil {
		mutate(&req)
	}
	created, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), req)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	collab := collaboration.NewCollaborationService(store)
	f := &delegateFixture{
		t: t, store: store, collab: collab, scope: scope,
		exec: &ControlExecutor{Portfolio: store, Sessions: store, Collaboration: collab},
	}
	f.claim()
	if f.run.ID != created.ID {
		t.Fatalf("claimed %s, want %s", f.run.ID, created.ID)
	}
	opened, err := store.CreateRunAgentSession(t.Context(), models.CreateRunAgentSessionRequest{
		Scope: scope, OwnerRunID: f.run.ID, AgentID: "manager", Lease: f.lease(), Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	f.session = opened.Session
	f.run = f.reload()
	return f
}

func (f *delegateFixture) lease() models.AgentRunLeaseGuard {
	return models.AgentRunLeaseGuard{WorkerID: delegateWorker, Now: time.Now().UTC()}
}

func (f *delegateFixture) reload() *models.AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.t.Context(), f.scope, f.run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// claim claims the source Run (it must be queued).
func (f *delegateFixture) claim() {
	f.t.Helper()
	claimed, err := f.store.ClaimNextAgentRun(f.t.Context(), models.AgentRunClaim{
		Scope: f.scope, WorkerID: delegateWorker, Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil {
		f.t.Fatalf("claim source: %#v %v", claimed, err)
	}
	if f.run != nil && claimed.ID != f.run.ID {
		f.t.Fatalf("claimed %s, want %s", claimed.ID, f.run.ID)
	}
	f.run = claimed
}

// call appends a finished, applied step whose assistant entry calls tool
// with arguments (one pending tool entry).
func (f *delegateFixture) call(tool models.ControlToolName, arguments interface{}) {
	f.t.Helper()
	sequence := f.run.LastAppliedTurn + 1
	if f.step != nil && f.step.Sequence >= sequence {
		sequence = f.step.Sequence + 1
	}
	stepID := fmt.Sprintf("%s-step-%d", f.run.ID, sequence)
	callID := fmt.Sprintf("call-%d", sequence)
	raw, _ := json.Marshal(arguments)
	assistant := &models.SessionEntry{
		ID: models.SessionEntryID(f.session.ID, models.AssistantEntryKey(stepID)), RunID: f.run.ID, StepID: stepID, Step: sequence,
		Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
			Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopToolCalls,
			ToolCalls: []models.ModelToolCall{{ID: callID, Name: string(tool), Arguments: raw}},
		},
	}
	entry := &models.SessionEntry{
		ID: models.SessionEntryID(f.session.ID, models.ToolEntryKey(stepID, callID)), RunID: f.run.ID, StepID: stepID, Step: sequence,
		Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: callID,
		Tool: &models.ToolEntry{Name: string(tool), Arguments: raw, SideEffect: models.ToolSideEffectWrite},
	}
	lease := f.lease()
	appended, err := f.store.AppendAgentSessionEntries(f.t.Context(), models.AppendAgentSessionEntriesRequest{
		Scope: f.scope, SessionID: f.session.ID, RunID: f.run.ID, Lease: &lease,
		Entries: []*models.SessionEntry{assistant, entry}, Now: lease.Now,
	})
	if err != nil {
		f.t.Fatalf("append step: %v", err)
	}
	f.run = f.reload()
	f.run.LastAppliedTurn = sequence
	f.step = &models.AgentTurn{ID: stepID, RunID: f.run.ID, Sequence: sequence, Status: models.AgentTurnStatusCompleted}
	f.entry = appended.Entries[1]
}

func (f *delegateFixture) context() agentrun.ControlContext {
	return NewControlContext(agentrun.StepToolsInput{
		Run: f.run, Step: f.step, Session: f.session, WorkerID: delegateWorker,
		Binding: &agentrun.TurnRunnerBinding{Authority: agentrun.ControlAuthority{AgentID: "manager", DelegateTargets: delegateTargets}},
	}, f.entry)
}

func (f *delegateFixture) settle() (ControlResult, error) {
	result, err := f.exec.Settle(f.t.Context(), f.context())
	if err == nil && result.Entry != nil {
		f.entry = result.Entry
	}
	return result, err
}

func (f *delegateFixture) mustSettle() ControlResult {
	f.t.Helper()
	result, err := f.settle()
	if err != nil {
		f.t.Fatalf("settle: %v", err)
	}
	return result
}

func (f *delegateFixture) delegate() (ControlResult, *models.AgentRequest) {
	f.t.Helper()
	f.call(models.ControlToolDelegate, map[string]interface{}{
		"agentId": "specialist", "goal": "Analyze the release", "context": map[string]interface{}{"release": "2026.07"},
	})
	result := f.mustSettle()
	if result.Kind != ControlParked || result.Park == nil || !result.Park.Committed {
		f.t.Fatalf("delegate did not commit a park: %#v", result)
	}
	return result, f.onlyRequest()
}

func (f *delegateFixture) requests() []*models.AgentRequest {
	f.t.Helper()
	requests, err := f.store.ListAgentRequests(f.t.Context(), models.AgentRequestFilter{Scope: f.scope, SourceRunID: f.run.ID, Limit: 10})
	if err != nil {
		f.t.Fatal(err)
	}
	return requests
}

func (f *delegateFixture) onlyRequest() *models.AgentRequest {
	f.t.Helper()
	requests := f.requests()
	if len(requests) != 1 {
		f.t.Fatalf("requests = %d, want 1", len(requests))
	}
	return requests[0]
}

func (f *delegateFixture) request(id string) *models.AgentRequest {
	f.t.Helper()
	request, err := f.collab.GetAgentRequest(f.t.Context(), f.scope, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return request
}

func (f *delegateFixture) currentEntry() *models.SessionEntry {
	f.t.Helper()
	entry, err := f.store.GetSessionEntry(f.t.Context(), f.scope, f.entry.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return entry
}

// respond is the recipient's decision on request id.
func (f *delegateFixture) respond(id string, decision collaboration.AgentRequestDecision, message string) *collaboration.AgentRequestResult {
	f.t.Helper()
	request := f.request(id)
	result, err := f.collab.RespondAgentRequest(f.t.Context(), collaboration.RespondAgentRequestRequest{
		Scope: f.scope, RequestID: id, ExpectedRevision: request.Revision, Decision: decision,
		Principal: request.Recipient, Message: message,
	})
	if err != nil {
		f.t.Fatalf("respond %s: %v", decision, err)
	}
	return result
}

// finishChild accepts request id and ends its child Run with status/output.
func (f *delegateFixture) finishChild(id string, status models.AgentRunStatus, output map[string]interface{}, failure string) {
	f.t.Helper()
	accepted := f.respond(id, collaboration.AgentRequestDecisionAccept, "On it.")
	activity := runtimeactivity.NewRunActivityService(f.store, f.store)
	running, _, err := activity.TransitionRun(f.t.Context(), f.scope, accepted.Child.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: accepted.Child.Revision, Status: models.AgentRunStatusRunning, Summary: "started",
		Actor: models.ActivityActor{Type: "worker", ID: "child-worker"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	ended, _, err := activity.TransitionRun(f.t.Context(), f.scope, accepted.Child.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: status, Output: output, Error: failure, Summary: "ended",
		Actor: models.ActivityActor{Type: "worker", ID: "child-worker"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.collab.ResolveTerminalAgentRequestChild(f.t.Context(), ended); err != nil {
		f.t.Fatalf("resolve child: %v", err)
	}
}

// resume claims the re-queued source and resumes the current entry.
func (f *delegateFixture) resume() ControlResume {
	f.t.Helper()
	if run := f.reload(); run.Status != models.AgentRunStatusQueued {
		f.t.Fatalf("source = %s, want queued", run.Status)
	}
	f.claim()
	f.entry = f.currentEntry()
	resumed, err := f.exec.Resume(f.t.Context(), f.context())
	if err != nil {
		f.t.Fatalf("resume: %v", err)
	}
	if resumed.Resolved && resumed.Result.Entry != nil {
		f.entry = resumed.Result.Entry
	}
	return resumed
}

func delegateResult(t *testing.T, entry *models.SessionEntry) map[string]interface{} {
	t.Helper()
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil || len(entry.Tool.Result.Content) != 1 {
		t.Fatalf("entry has no result: %#v", entry)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(entry.Tool.Result.Content[0].Value, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestDelegateParksWaitingForAgentDecision(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), nil)
	result, request := f.delegate()
	if request.Recipient != (models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "specialist"}) || request.Status != models.AgentRequestStatusPending ||
		request.AcceptancePolicy != models.AgentRequestAcceptanceRecipientReview || request.Goal != "Analyze the release" ||
		request.SharedContext["release"] != "2026.07" || request.SharedContext[agentrun.DelegationModeContextKey] != "reason" ||
		len(request.ChildCheckpoint) != 0 || request.IdempotencyKey != DelegationIdempotencyKey(f.run.ID, "delegate-"+f.entry.ID) {
		t.Fatalf("request = %#v", request)
	}
	if want := collaboration.StableCollaborationID(f.scope, request.IdempotencyKey, "request"); request.ID != want {
		t.Fatalf("request id %s, want %s", request.ID, want)
	}
	source := f.reload()
	if source.Status != models.AgentRunStatusWaitingForAgent || source.WakeCondition == nil ||
		source.WakeCondition.Type != "agent_request_decision" || source.WakeCondition.Reference != request.ID || source.LeaseOwner != "" {
		t.Fatalf("source = %s %#v", source.Status, source.WakeCondition)
	}
	if result.Park.Status != models.AgentRunStatusWaitingForAgent || result.Park.Wake.Reference != request.ID || result.Run == nil {
		t.Fatalf("park = %#v", result.Park)
	}
	entry := f.currentEntry()
	if entry.State != models.ToolEntryAwaitingChild || entry.Tool.Wait == nil || entry.Tool.Wait.AgentRequestID != request.ID {
		t.Fatalf("entry = %s %#v", entry.State, entry.Tool.Wait)
	}
	meta, err := DecodeControlEntryMeta(entry)
	if err != nil || meta.Kind != models.ControlToolDelegate || meta.RequestID != request.ID {
		t.Fatalf("meta = %#v %v", meta, err)
	}
}

func TestDelegateCompletedResultCarriesAnswerAndArtifacts(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), nil)
	_, request := f.delegate()
	f.finishChild(request.ID, models.AgentRunStatusCompleted, map[string]interface{}{
		"report": "Ship it: no blockers found.", "summary": "Release analysed",
	}, "")
	resumed := f.resume()
	if !resumed.Resolved || resumed.Result.Kind != ControlContinue {
		t.Fatalf("resume = %#v", resumed)
	}
	if f.entry.State != models.ToolEntryCompleted || f.entry.Tool.Result.IsError {
		t.Fatalf("entry = %s %#v", f.entry.State, f.entry.Tool.Result)
	}
	value := delegateResult(t, f.entry)
	artifacts, ok := value["artifacts"].([]interface{})
	if value["status"] != "completed" || value["requestId"] != request.ID || value["agentId"] != "specialist" ||
		value["answer"] != "Ship it: no blockers found." || !ok || len(artifacts) != 0 {
		t.Fatalf("result = %#v", value)
	}
	if strings.Contains(string(f.entry.Tool.Result.Content[0].Value), "checkpoint") {
		t.Fatalf("result leaks child checkpoint: %s", f.entry.Tool.Result.Content[0].Value)
	}
}

func TestDelegateClarificationThenReplyParksAgain(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), nil)
	_, request := f.delegate()
	f.respond(request.ID, collaboration.AgentRequestDecisionRequestClarification, "Which region?")
	resumed := f.resume()
	if !resumed.Resolved || f.entry.State != models.ToolEntryCompleted || f.entry.Tool.Result.IsError {
		t.Fatalf("resume = %#v entry %s", resumed, f.entry.State)
	}
	value := delegateResult(t, f.entry)
	if value["status"] != "clarification_requested" || value["question"] != "Which region?" || value["requestId"] != request.ID {
		t.Fatalf("result = %#v", value)
	}

	f.call(models.ControlToolDelegateReply, map[string]interface{}{"requestId": request.ID, "answer": "EU only."})
	result := f.mustSettle()
	if result.Kind != ControlParked || !result.Park.Committed || result.Park.Status != models.AgentRunStatusWaitingForAgent {
		t.Fatalf("reply = %#v", result)
	}
	answered := f.request(request.ID)
	if answered.Status != models.AgentRequestStatusPending || answered.Response != "EU only." {
		t.Fatalf("request = %s %q", answered.Status, answered.Response)
	}
	if source := f.reload(); source.Status != models.AgentRunStatusWaitingForAgent || source.WakeCondition.Reference != request.ID {
		t.Fatalf("source = %s %#v", source.Status, source.WakeCondition)
	}
	if entry := f.currentEntry(); entry.State != models.ToolEntryAwaitingChild || entry.Tool.Wait.AgentRequestID != request.ID {
		t.Fatalf("reply entry = %s", entry.State)
	}
	if len(f.requests()) != 1 {
		t.Fatal("reply created a second request")
	}

	f.finishChild(request.ID, models.AgentRunStatusCompleted, map[string]interface{}{"summary": "EU analysed"}, "")
	if resumed := f.resume(); !resumed.Resolved || delegateResult(t, f.entry)["answer"] != "EU analysed" {
		t.Fatalf("reply resume = %#v %#v", resumed, f.entry.Tool.Result)
	}
}

func TestDelegateReplyCrashBeforeAnswerAnswersOnResume(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), nil)
	_, request := f.delegate()
	f.respond(request.ID, collaboration.AgentRequestDecisionRequestClarification, "Which region?")
	f.resume()
	f.call(models.ControlToolDelegateReply, map[string]interface{}{"requestId": request.ID, "answer": "EU only."})
	// Crash after the entry moved, before the answer was recorded.
	asked := f.request(request.ID)
	if _, err := f.exec.awaitDelegation(t.Context(), f.context(), asked.ID, asked.Revision); err != nil {
		t.Fatal(err)
	}
	f.entry = f.currentEntry()
	resumed, err := f.exec.Resume(t.Context(), f.context())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Resolved || resumed.Park == nil || !resumed.Park.Committed {
		t.Fatalf("resume = %#v", resumed)
	}
	if answered := f.request(request.ID); answered.Status != models.AgentRequestStatusPending || answered.Response != "EU only." {
		t.Fatalf("request = %s %q", answered.Status, answered.Response)
	}
}

func TestDelegateRejectedIsErrorResult(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), nil)
	_, request := f.delegate()
	f.respond(request.ID, collaboration.AgentRequestDecisionReject, "Out of scope for me.")
	resumed := f.resume()
	if !resumed.Resolved || f.entry.State != models.ToolEntryError || !f.entry.Tool.Result.IsError {
		t.Fatalf("resume = %#v entry %s", resumed, f.entry.State)
	}
	if value := delegateResult(t, f.entry); value["status"] != "rejected" || value["reason"] != "Out of scope for me." {
		t.Fatalf("result = %#v", value)
	}
}

func TestDelegateFailedLeavesNoFinalFailureCheckpoint(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), nil)
	_, request := f.delegate()
	f.finishChild(request.ID, models.AgentRunStatusFailed, nil, "provider exhausted retries")
	source := f.reload()
	if source.Status != models.AgentRunStatusQueued {
		t.Fatalf("source = %s", source.Status)
	}
	for key := range source.Checkpoint {
		if strings.Contains(strings.ToLower(key), "failure") {
			t.Fatalf("source checkpoint carries %s: %#v", key, source.Checkpoint)
		}
	}
	resumed := f.resume()
	if !resumed.Resolved || f.entry.State != models.ToolEntryError {
		t.Fatalf("resume = %#v entry %s", resumed, f.entry.State)
	}
	if value := delegateResult(t, f.entry); value["status"] != "failed" || value["reason"] != "provider exhausted retries" {
		t.Fatalf("result = %#v", value)
	}
}

func TestDelegateFromRunbookOriginIsPreauthorized(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), func(req *models.CreateAgentRunRequest) {
		req.Context = map[string]interface{}{
			"runbookDefinitionId": "hourly-scan", "runbookDefinitionVersion": "2.0.2", "runbookActivationId": "activation-one",
		}
	})
	_, request := f.delegate()
	origin, _ := request.SharedContext["triggerInput"].(map[string]interface{})
	if request.AcceptancePolicy != models.AgentRequestAcceptancePreauthorized || origin["runbookDefinitionId"] != "hourly-scan" ||
		origin["runbookActivationId"] != "activation-one" {
		t.Fatalf("request = %#v", request)
	}
}

func TestReplyToForeignRequestIsRefused(t *testing.T) {
	store := conversationtest.NewStore(t)
	other := newDelegateFixture(t, store, nil)
	_, foreign := other.delegate()
	other.respond(foreign.ID, collaboration.AgentRequestDecisionRequestClarification, "Which region?")
	other.claim() // the other source runs again; it is not queued

	f := newDelegateFixtureIn(t, store, other.scope, nil) // same tenant, another source Run
	f.call(models.ControlToolDelegateReply, map[string]interface{}{"requestId": foreign.ID, "answer": "EU only."})
	result := f.mustSettle()
	if result.Kind != ControlContinue || f.entry.State != models.ToolEntryError {
		t.Fatalf("reply = %#v", result)
	}
	if value := delegateResult(t, f.entry); value["error"] != controlRequestNotAwaitingCode {
		t.Fatalf("result = %#v", value)
	}
	if request := other.request(foreign.ID); request.Status != models.AgentRequestStatusClarificationRequested || request.Response != "Which region?" {
		t.Fatalf("foreign request changed: %s %q", request.Status, request.Response)
	}
	if run := f.reload(); run.Status != models.AgentRunStatusRunning {
		t.Fatalf("source = %s", run.Status)
	}
}

func TestDelegateReplyToUnaskedRequestIsRefused(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), nil)
	_, request := f.delegate()
	f.respond(request.ID, collaboration.AgentRequestDecisionReject, "No.")
	f.resume()
	f.call(models.ControlToolDelegateReply, map[string]interface{}{"requestId": request.ID, "answer": "EU only."})
	f.mustSettle()
	if value := delegateResult(t, f.entry); f.entry.State != models.ToolEntryError || value["error"] != controlRequestNotAwaitingCode ||
		value["message"] != "Request "+request.ID+" is rejected." {
		t.Fatalf("result = %s %#v", f.entry.State, value)
	}
}

func TestDelegateUnknownAgentIsRefused(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), nil)
	f.call(models.ControlToolDelegate, map[string]interface{}{"agentId": "stranger", "goal": "Do it"})
	f.mustSettle()
	if value := delegateResult(t, f.entry); f.entry.State != models.ToolEntryError || value["error"] != controlDelegationRejectedCode {
		t.Fatalf("result = %s %#v", f.entry.State, value)
	}
	if len(f.requests()) != 0 {
		t.Fatal("request was created")
	}
}

func TestDelegateNewMessageRefusesWithoutRequest(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), nil)
	f.call(models.ControlToolDelegate, map[string]interface{}{"agentId": "specialist", "goal": "Analyze the release"})
	// A message arrives after the loop read the Run.
	current := f.reload()
	next := models.CloneAgentRun(current)
	next.PendingInterventions = append(next.PendingInterventions, models.AgentRunIntervention{
		ID: "i1", Actor: models.ActivityActor{Type: "user", ID: "u1"}, Instruction: "use the EU numbers", CreatedAt: time.Now().UTC(),
	})
	next.Revision++
	if _, err := f.store.UpdateAgentRunWithEvent(t.Context(), next, current.Revision, &models.ActivityEvent{
		ID: uuid.NewString(), Scope: f.scope, EventType: "run.intervention_added", Severity: models.ActivitySeverityInfo,
		RunID: current.ID, Actor: models.ActivityActor{Type: "user", ID: "u1"}, Summary: "guidance", Visibility: models.ActivityVisibilityScope,
		CreatedAt: time.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	result := f.mustSettle()
	if result.Kind != ControlContinue || f.entry.State != models.ToolEntryError || delegateResult(t, f.entry)["error"] != controlNewMessageCode {
		t.Fatalf("result = %#v entry %s", result, f.entry.State)
	}
	if len(f.requests()) != 0 {
		t.Fatal("request was created")
	}
	if run := f.reload(); run.Status != models.AgentRunStatusRunning {
		t.Fatalf("source = %s", run.Status)
	}
}

func TestDelegateBoundedSourceFundsMinimumChild(t *testing.T) {
	const childMinimumTurns = 8 // the Turns every child is funded with at least
	f := newDelegateFixture(t, conversationtest.NewStore(t), func(req *models.CreateAgentRunRequest) {
		req.Budget = &models.BudgetPolicy{MaxTurns: 64}
	})
	_, request := f.delegate()
	if request.BudgetAllocation == nil || request.BudgetAllocation.MaxTurns != childMinimumTurns {
		t.Fatalf("budget = %#v", request.BudgetAllocation)
	}
}

func TestDelegateInsufficientBudgetIsToolError(t *testing.T) {
	f := newDelegateFixture(t, conversationtest.NewStore(t), func(req *models.CreateAgentRunRequest) {
		req.Budget = &models.BudgetPolicy{MaxTurns: 4}
	})
	f.call(models.ControlToolDelegate, map[string]interface{}{"agentId": "specialist", "goal": "Analyze the release"})
	f.mustSettle()
	if value := delegateResult(t, f.entry); f.entry.State != models.ToolEntryError || value["error"] != modelstep.ControlBudgetErrorCode {
		t.Fatalf("result = %s %#v", f.entry.State, value)
	}
	if len(f.requests()) != 0 {
		t.Fatal("request was created")
	}
}

// crashingCreateStore fails the first AgentRequest create before (or, with
// afterCommit, after) it commits, like a process crash around the write.
type crashingCreateStore struct {
	delegateTestStore
	afterCommit bool
	crashed     bool
}

var errDelegateCrash = errors.New("simulated crash")

func (s *crashingCreateStore) CreateAgentRequest(ctx context.Context, record models.AgentRequestCreateRecord) (*models.ActivityEvent, error) {
	if s.crashed {
		return s.delegateTestStore.CreateAgentRequest(ctx, record)
	}
	s.crashed = true
	if s.afterCommit {
		if _, err := s.delegateTestStore.CreateAgentRequest(ctx, record); err != nil {
			return nil, err
		}
	}
	return nil, errDelegateCrash
}

func TestDelegateCrashBeforeRequestResettlesOnce(t *testing.T) {
	backends := []struct {
		name string
		open func(t *testing.T) delegateTestStore
	}{
		{"Memory", func(*testing.T) delegateTestStore { return conversationtest.NewStore(t) }},
		{"SQLite", func(t *testing.T) delegateTestStore { return openControlSessionStore(t) }},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Run("BeforeCommit", func(t *testing.T) {
				store := &crashingCreateStore{delegateTestStore: backend.open(t)}
				f := newDelegateFixture(t, store, nil)
				f.call(models.ControlToolDelegate, map[string]interface{}{"agentId": "specialist", "goal": "Analyze the release"})
				if _, err := f.settle(); !errors.Is(err, errDelegateCrash) {
					t.Fatalf("first settle = %v, want the crash", err)
				}
				if entry := f.currentEntry(); entry.State != models.ToolEntryPending {
					t.Fatalf("entry after crash = %s", entry.State)
				}
				if len(f.requests()) != 0 {
					t.Fatal("a request survived the crash")
				}
				// The next claim re-settles the same pending entry.
				f.run = f.reload()
				f.run.LastAppliedTurn = f.step.Sequence
				f.entry = f.currentEntry()
				result := f.mustSettle()
				if result.Kind != ControlParked || !result.Park.Committed {
					t.Fatalf("re-settle = %#v", result)
				}
				request := f.onlyRequest()
				if entry := f.currentEntry(); entry.State != models.ToolEntryAwaitingChild || entry.Tool.Wait.AgentRequestID != request.ID {
					t.Fatalf("entry = %s", entry.State)
				}
				if source := f.reload(); source.Status != models.AgentRunStatusWaitingForAgent || source.WakeCondition.Reference != request.ID {
					t.Fatalf("source = %s", source.Status)
				}
			})
			t.Run("AfterCommit", func(t *testing.T) {
				store := &crashingCreateStore{delegateTestStore: backend.open(t), afterCommit: true}
				f := newDelegateFixture(t, store, nil)
				f.call(models.ControlToolDelegate, map[string]interface{}{"agentId": "specialist", "goal": "Analyze the release"})
				if _, err := f.settle(); !errors.Is(err, errDelegateCrash) {
					t.Fatalf("first settle = %v, want the crash", err)
				}
				request := f.onlyRequest()
				if entry := f.currentEntry(); entry.State != models.ToolEntryAwaitingChild {
					t.Fatalf("entry = %s", entry.State)
				}
				// The recipient rejects; the resumed claim resumes (no re-settle).
				f.respond(request.ID, collaboration.AgentRequestDecisionReject, "No capacity.")
				if resumed := f.resume(); !resumed.Resolved || f.entry.State != models.ToolEntryError {
					t.Fatalf("resume = %#v entry %s", resumed, f.entry.State)
				}
				if len(f.requests()) != 1 {
					t.Fatal("resume created another request")
				}
			})
		})
	}
}

func TestDelegatedRunbookOriginPreservesImmutableIdentity(t *testing.T) {
	origin := delegatedRunbookOrigin(map[string]interface{}{
		"runbookDefinitionId": "hourly-scan", "runbookDefinitionVersion": "2.0.2",
		"runbookEntrypoint": "perform-hourly", "runbookActivationId": "activation-one", "runbookTriggerId": "hourly",
	})
	if origin["runbookDefinitionId"] != "hourly-scan" || origin["runbookDefinitionVersion"] != "2.0.2" ||
		origin["runbookEntrypoint"] != "perform-hourly" || origin["runbookActivationId"] != "activation-one" ||
		origin["runbookTriggerId"] != "hourly" {
		t.Fatalf("delegated Runbook origin = %#v", origin)
	}
	if delegatedRunbookOrigin(map[string]interface{}{"runbookDefinitionId": "hourly-scan"}) != nil {
		t.Fatal("incomplete Runbook origin was propagated")
	}
}
