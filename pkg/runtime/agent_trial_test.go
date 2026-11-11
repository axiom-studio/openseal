package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func agentTrialArguments() map[string]interface{} {
	return map[string]interface{}{
		"displayName":         "Inbox Triage",
		"purpose":             "Sort the support inbox every morning.",
		"systemPrompt":        "Read new support mail and sort it by urgency.",
		"personality":         "Friendly",
		"operatingPrinciples": []interface{}{"Flag anything legal."},
		"model":               "gpt-6-luna",
		"message":             "  What would you do with an angry refund mail?  ",
	}
}

// putAgentTrialRun stores run as given, bypassing transition rules.
func putAgentTrialRun(t *testing.T, store *SQLiteStore, run *AgentRun) *AgentRun {
	t.Helper()
	putStoredRun(t, store, cloneAgentRun(run))
	return run
}

func mustAgentRun(t *testing.T, store PortfolioStore, scope Scope, id string) *AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil || run == nil {
		t.Fatalf("run %s = %#v, %v", id, run, err)
	}
	return run
}

// startAgentTrial dispatches try_agent for the fixture's builder Run.
func startAgentTrial(t *testing.T, f *agentBuilderDispatchFixture, callID string) (*ActionCall, *AgentRun) {
	t.Helper()
	arguments := f.resolve(t, AgentActionTryAgent, agentTrialArguments())
	output, err := f.execute(t, AgentActionTryAgent, callID, arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	childID, _ := output["trialRunId"].(string)
	if output["resourceType"] != "agent_trial" || output["status"] != "running" || childID != agentTrialRunID(f.run.ID, callID) {
		t.Fatalf("try_agent output = %#v", output)
	}
	call := &ActionCall{
		ID: callID, Scope: f.scope, RunID: f.run.ID, DeploymentID: "finn", SkillID: AgentManagementSkillID,
		SkillVersion: AgentManagementSkillVersion, Action: AgentActionTryAgent, Status: ActionCallStatusSucceeded,
		Origin: ActionOriginLoop, Output: output,
	}
	return call, mustAgentRun(t, f.store, f.scope, childID)
}

func TestAgentTrialStartsChildWithoutDefinition(t *testing.T) {
	f := newAgentBuilderDispatchFixture(t)
	ctx := context.Background()
	before, err := f.agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{Scope: agentBuilderScope(f.scope)})
	if err != nil {
		t.Fatal(err)
	}
	_, child := startAgentTrial(t, f, "trial-call")

	if _, err := f.agents.GetDefinition(ctx, agentBuilderResourceID("agent-definition", f.scope, "trial-call"), agentBuilderDefinitionVersion); !errors.Is(err, kernelagent.ErrDefinitionNotFound) {
		t.Fatalf("try_agent registered a definition: %v", err)
	}
	after, err := f.agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{Scope: agentBuilderScope(f.scope)})
	if err != nil || len(after) != len(before) {
		t.Fatalf("deployments %d -> %d, %v", len(before), len(after), err)
	}
	if child.Kind != RunKindAgentWork || child.ParentRunID != f.run.ID || child.AssignedAgentID != "finn" ||
		child.Owner != f.run.Owner || child.Source != RunSourceHandoff || child.Status != AgentRunStatusQueued ||
		child.Goal != "What would you do with an angry refund mail?" {
		t.Fatalf("child = %#v", child)
	}
	if child.Budget == nil || child.Budget.MaxTurns != agentTrialMaxTurns || child.Budget.MaxOutputTokens != 0 {
		t.Fatalf("child budget = %#v", child.Budget)
	}
	if child.Context[AgentTrialModelContextKey] != "gpt-6-luna" {
		t.Fatalf("child model = %#v", child.Context)
	}
	trial, ok, err := agentTrialContextForRun(child)
	if err != nil || !ok || trial.ParentRunID != f.run.ID || trial.ActionCallID != "trial-call" ||
		trial.Profile.SystemPrompt != "Read new support mail and sort it by urgency." || trial.Profile.DisplayName != "Inbox Triage" ||
		trial.Profile.Personality != "Friendly" || !slices.Equal(trial.Profile.OperatingPrinciples, []string{"Flag anything legal."}) ||
		trial.Profile.Model != "" {
		t.Fatalf("trial context = %#v ok=%v err=%v", trial, ok, err)
	}
	if conversationID, _ := child.Context[conversationRunContextConversationID].(string); conversationID != "" {
		t.Fatalf("trial child posts to a chat: %#v", child.Context)
	}

	// A replay of the call finds the same child.
	arguments := f.resolve(t, AgentActionTryAgent, agentTrialArguments())
	again, err := f.execute(t, AgentActionTryAgent, "trial-call", arguments, agentBuilderApprover)
	if err != nil || again["trialRunId"] != child.ID {
		t.Fatalf("replay = %#v, %v", again, err)
	}
	children, err := f.store.ListAgentRuns(ctx, AgentRunFilter{Scope: f.scope, ParentRunID: f.run.ID})
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %d, %v", len(children), err)
	}
}

func TestAgentTrialRefusesSensitiveMessage(t *testing.T) {
	f := newAgentBuilderDispatchFixture(t)
	arguments := agentTrialArguments()
	arguments["message"] = "Log in with password: hunter2-correct-horse"
	if _, err := f.propose(t, AgentActionTryAgent, arguments); err == nil {
		t.Fatal("sensitive trial message accepted")
	}
}

func TestAgentTrialParksParentUntilDeadline(t *testing.T) {
	f := newAgentBuilderDispatchFixture(t)
	call, child := startAgentTrial(t, f, "trial-call")
	now := child.CreatedAt.Add(time.Minute)
	parker := newGovernedParker(f.store, nil, func() time.Time { return now })

	wake, summary, err := parker.governedCardWait(context.Background(), f.run, call)
	if err != nil {
		t.Fatal(err)
	}
	deadline := child.CreatedAt.Add(agentTrialTimeout)
	if wake == nil || wake.Type != AgentTrialWakeType || wake.Reference != child.ID || wake.WakeAt == nil ||
		!wake.WakeAt.Equal(deadline) || summary != "Trying the draft" {
		t.Fatalf("wake = %#v %q", wake, summary)
	}
	// Past the deadline the Run does not park again: the resume times out.
	now = deadline
	if wake, _, err := parker.governedCardWait(context.Background(), f.run, call); err != nil || wake != nil {
		t.Fatalf("wake past deadline = %#v, %v", wake, err)
	}
	// A finished trial never parks.
	now = child.CreatedAt.Add(time.Minute)
	child.Status = AgentRunStatusCompleted
	putAgentTrialRun(t, f.store.SQLiteStore, child)
	if wake, _, err := parker.governedCardWait(context.Background(), f.run, call); err != nil || wake != nil {
		t.Fatalf("wake after completion = %#v, %v", wake, err)
	}
}

func TestResolveLoopBindingAgentTrial(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "trial"}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "finn", Scope: skillScope, DefinitionID: "finn-definition", ActiveVersion: "7", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "finn-definition", Version: "7", Purpose: "Build Agents", SystemPrompt: "You are Finn, the builder."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: skillScope, DeploymentID: "finn"},
	}
	store := newTestKernelStore(t)
	parent := putAgentTrialRun(t, store, &AgentRun{
		ID: "parent", Scope: scope, Kind: RunKindConversation, RootRunID: "parent", Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"},
		AssignedAgentID: "finn", Status: AgentRunStatusRunning, Revision: 1, CreatedAt: time.Now().UTC(),
	})
	trial, err := agentTrialContextMap(AgentTrialContext{
		Profile: agentProfileArguments{
			DisplayName: "Inbox Triage", Purpose: "Sort the support inbox.", SystemPrompt: "Sort mail by urgency.",
			Personality: "Friendly", OperatingPrinciples: []string{"Flag anything legal."},
		},
		ParentRunID: parent.ID, ActionCallID: "trial-call",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := claimedBindingRun(t, store, &AgentRun{
		ID: "child", Scope: scope, Kind: RunKindAgentWork, ParentRunID: parent.ID, RootRunID: parent.ID,
		Owner: parent.Owner, AssignedAgentID: "finn", Goal: "Hello there", Source: RunSourceHandoff,
		Context: map[string]interface{}{AgentTrialContextKey: trial, AgentTrialModelContextKey: "gpt-6-luna"},
	})
	config := loopBindingTestConfig(t, store, &progressAckStepHost{})
	config.ChatWorkspaces = bindingTestChatWorkspaces{}
	binding, err := ResolveLoopBinding(t.Context(), catalog, cloneAgentRun(child), config)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.activationDeployments) != 0 {
		t.Fatalf("trial activated Skills: %v", catalog.activationDeployments)
	}
	if binding.DeploymentID != "finn" || binding.DefinitionID != "finn-definition" || binding.DefinitionVersion != "7" ||
		binding.Model != "gpt-6-luna" || len(binding.ModelActions) != 0 || len(binding.RunbookOperations) != 0 ||
		binding.Final != nil || binding.FinalOutput != nil {
		t.Fatalf("binding = %#v", binding)
	}
	authority := binding.Authority
	if authority.finalMode() != FinalModeDefault || authority.CanAskUser || authority.ForkAllowed || authority.CanReportProgress ||
		authority.CanWait || authority.CanReact || authority.TaskStart != nil || len(authority.DelegateTargets) != 0 {
		t.Fatalf("authority = %#v", authority)
	}
	req := prepareBindingStep(t, binding, child)
	if tools := bindingToolNames(req); !slices.Equal(tools, []string{string(ControlToolFinalAnswer)}) {
		t.Fatalf("tools = %v", tools)
	}
	if req.DefinitionID != "finn-definition" || req.DefinitionVersion != "7" || req.Model != "gpt-6-luna" {
		t.Fatalf("request identity = %s@%s model %q", req.DefinitionID, req.DefinitionVersion, req.Model)
	}
	instructions := strings.Join(req.System.AgentInstructions, "\n")
	for _, want := range []string{"Sort mail by urgency.", "Purpose: Sort the support inbox.", "Personality: Friendly", "Flag anything legal."} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions lack %q: %q", want, instructions)
		}
	}
	if strings.Contains(instructions, "You are Finn") || strings.Contains(instructions, "Build Agents") {
		t.Fatalf("instructions carry the builder's persona: %q", instructions)
	}
	entries, err := store.ListAgentSessionEntries(t.Context(), SessionEntryFilter{Scope: scope, SessionID: binding.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].User == nil || len(entries[0].User.Content) != 1 || entries[0].User.Content[0].Text != "Hello there" {
		t.Fatalf("seed = %#v", entries)
	}

	// A trial context on a Run that is not its child is refused.
	stray := cloneAgentRun(child)
	stray.ParentRunID = "someone-else"
	if _, err := ResolveLoopBinding(t.Context(), catalog, stray, config); err == nil {
		t.Fatal("stray trial context accepted")
	}
}

// agentTrialFinalizerFixture is a builder Run waiting on its trial child.
type agentTrialFinalizerFixture struct {
	store  *SQLiteStore
	scope  Scope
	parent *AgentRun
	child  *AgentRun
	call   *ActionCall
}

func newAgentTrialFinalizerFixture(t *testing.T, parentStatus AgentRunStatus) *agentTrialFinalizerFixture {
	t.Helper()
	f := newAgentBuilderDispatchFixture(t)
	call, child := startAgentTrial(t, f, "trial-call")
	parent := mustAgentRun(t, f.store, f.scope, f.run.ID)
	parent.Status = parentStatus
	if parentStatus == AgentRunStatusWaitingForEvent {
		deadline := agentTrialDeadline(child)
		parent.WakeCondition = &WakeCondition{Type: AgentTrialWakeType, Reference: child.ID, WakeAt: &deadline}
	}
	putAgentTrialRun(t, f.store.SQLiteStore, parent)
	return &agentTrialFinalizerFixture{store: f.store.SQLiteStore, scope: f.scope, parent: parent, child: child, call: call}
}

func (f *agentTrialFinalizerFixture) endChild(t *testing.T, status AgentRunStatus, output map[string]interface{}, message string) *AgentRun {
	t.Helper()
	child := mustAgentRun(t, f.store, f.scope, f.child.ID)
	ended := child.CreatedAt.Add(30 * time.Second)
	child.Status, child.Output, child.Error, child.CompletedAt, child.UpdatedAt = status, output, message, &ended, ended
	putAgentTrialRun(t, f.store, child)
	return child
}

func TestAgentTrialFinalizerDeliversReceiptAndQueuesParent(t *testing.T) {
	f := newAgentTrialFinalizerFixture(t, AgentRunStatusWaitingForEvent)
	child := f.endChild(t, AgentRunStatusCompleted, map[string]interface{}{finalOutputReportKey: "I would apologise and refund."}, "")
	var woken []Scope
	nextCalls := 0
	next := RunTerminalFinalizerFunc(func(context.Context, *AgentRun) error { nextCalls++; return nil })
	finalizer := NewAgentTrialFinalizer(f.store, next, func(scope Scope) { woken = append(woken, scope) })
	for range 2 {
		if err := finalizer.FinalizeRun(context.Background(), child); err != nil {
			t.Fatal(err)
		}
	}
	parent := mustAgentRun(t, f.store, f.scope, f.parent.ID)
	if parent.Status != AgentRunStatusQueued || parent.WakeCondition != nil {
		t.Fatalf("parent = %s %#v", parent.Status, parent.WakeCondition)
	}
	receipts := 0
	for _, intervention := range parent.PendingInterventions {
		if intervention.AgentTrial != nil {
			receipts++
			if intervention.ActionCallID != f.call.ID || *intervention.AgentTrial != (AgentTrialReceipt{
				RunID: child.ID, Status: AgentTrialStatusCompleted, Answer: "I would apologise and refund.",
			}) {
				t.Fatalf("intervention = %#v receipt=%#v", intervention, intervention.AgentTrial)
			}
		}
	}
	if receipts != 1 || len(woken) != 1 || woken[0] != f.scope || nextCalls != 2 {
		t.Fatalf("receipts=%d woken=%v next=%d", receipts, woken, nextCalls)
	}

	// The resume renders the receipt as the tool result.
	outcome := settleAgentTrialCall(t, f, parent, parent.PendingInterventions, time.Now())
	output := agentTrialResultOutput(t, outcome)
	if outcome.State != ToolEntryCompleted || output["status"] != AgentTrialStatusCompleted ||
		output["reply"] != "I would apologise and refund." || output["trialRunId"] != child.ID {
		t.Fatalf("outcome = %#v output=%#v", outcome, output)
	}
}

func TestAgentTrialFinalizerHeldParentKeepsStatus(t *testing.T) {
	f := newAgentTrialFinalizerFixture(t, AgentRunStatusRunning)
	child := f.endChild(t, AgentRunStatusFailed, nil, "model unavailable")
	if err := NewAgentTrialFinalizer(f.store, nil, nil).FinalizeRun(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	parent := mustAgentRun(t, f.store, f.scope, f.parent.ID)
	if parent.Status != AgentRunStatusRunning || parent.Revision != f.parent.Revision+1 {
		t.Fatalf("parent = %s rev %d (was %d)", parent.Status, parent.Revision, f.parent.Revision)
	}
	receipt := agentTrialReceiptFor(f.call.ID, child.ID, parent.PendingInterventions)
	if receipt == nil || receipt.Status != AgentTrialStatusFailed || receipt.FailureMessage != "model unavailable" {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestAgentTrialFinalizerIgnoresOtherRuns(t *testing.T) {
	f := newAgentTrialFinalizerFixture(t, AgentRunStatusWaitingForEvent)
	// A canceled trial reaches only a parent waiting on it; the parent that
	// canceled it at the deadline already reported timed_out.
	parent := mustAgentRun(t, f.store, f.scope, f.parent.ID)
	parent.Status, parent.WakeCondition = AgentRunStatusRunning, nil
	putAgentTrialRun(t, f.store, parent)
	child := f.endChild(t, AgentRunStatusCanceled, nil, "")
	finalizer := NewAgentTrialFinalizer(f.store, nil, nil)
	if err := finalizer.FinalizeRun(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	// A Run without a trial context is not touched.
	other := cloneAgentRun(parent)
	other.ID, other.Status, other.Context = "other", AgentRunStatusCompleted, nil
	if err := finalizer.FinalizeRun(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if after := mustAgentRun(t, f.store, f.scope, f.parent.ID); after.Revision != parent.Revision || len(after.PendingInterventions) != len(parent.PendingInterventions) {
		t.Fatalf("parent changed: %#v", after)
	}
}

func TestAgentTrialResumeRendersFailure(t *testing.T) {
	f := newAgentTrialFinalizerFixture(t, AgentRunStatusRunning)
	child := f.endChild(t, AgentRunStatusFailed, nil, "")
	child.FailureExplanation = &RunFailureExplanation{Kind: RunFailureKindExecution, Code: "model", Message: "The model is unavailable."}
	putAgentTrialRun(t, f.store, child)
	// No receipt (the finalizer missed it): the resume reads the child.
	outcome := settleAgentTrialCall(t, f, f.parent, nil, time.Now())
	output := agentTrialResultOutput(t, outcome)
	if output["status"] != AgentTrialStatusFailed || output["error"] != "The model is unavailable." || output["trialRunId"] != child.ID {
		t.Fatalf("output = %#v", output)
	}
}

func TestAgentTrialResumeReadsMissedChild(t *testing.T) {
	f := newAgentTrialFinalizerFixture(t, AgentRunStatusRunning)
	f.endChild(t, AgentRunStatusCompleted, map[string]interface{}{finalOutputReportKey: strings.Repeat("a", agentTrialReplyBytes+100)}, "")
	outcome := settleAgentTrialCall(t, f, f.parent, nil, time.Now())
	output := agentTrialResultOutput(t, outcome)
	reply, _ := output["reply"].(string)
	if output["status"] != AgentTrialStatusCompleted || len(reply) > agentTrialReplyBytes || !strings.Contains(reply, "truncated") {
		t.Fatalf("output status=%v reply=%d bytes", output["status"], len(reply))
	}
}

func TestAgentTrialTimerWakeCancelsRunningChild(t *testing.T) {
	f := newAgentTrialFinalizerFixture(t, AgentRunStatusWaitingForEvent)
	ctx := context.Background()
	deadline := agentTrialDeadline(f.child)

	// The timer wakes the waiting parent only at the deadline.
	wakes := NewAgentRunWakeService(f.store, f.store)
	early, err := wakes.WakeDueTimers(ctx, f.scope, deadline.Add(-time.Second))
	if err != nil || len(early.Runs) != 0 || early.NextWakeAt == nil || !early.NextWakeAt.Equal(deadline) {
		t.Fatalf("early wake = %#v, %v", early, err)
	}
	due, err := wakes.WakeDueTimers(ctx, f.scope, deadline.Add(time.Second))
	if err != nil || len(due.Runs) != 1 || due.Runs[0].Run.ID != f.parent.ID || due.Runs[0].Run.Status != AgentRunStatusQueued {
		t.Fatalf("due wake = %#v, %v", due, err)
	}

	// The resume cancels the child that is still running and times out.
	parent := mustAgentRun(t, f.store, f.scope, f.parent.ID)
	outcome := settleAgentTrialCall(t, f, parent, nil, deadline.Add(time.Second))
	output := agentTrialResultOutput(t, outcome)
	if outcome.State != ToolEntryCompleted || output["status"] != AgentTrialStatusTimedOut || output["trialRunId"] != f.child.ID {
		t.Fatalf("outcome = %#v output=%#v", outcome, output)
	}
	if child := mustAgentRun(t, f.store, f.scope, f.child.ID); child.Status != AgentRunStatusCanceled {
		t.Fatalf("child = %s", child.Status)
	}
}

// settleAgentTrialCall settles the fixture's try_agent call for parent at
// now, as the loop's resume does.
func settleAgentTrialCall(t *testing.T, f *agentTrialFinalizerFixture, parent *AgentRun, settled []AgentRunIntervention, now time.Time) GovernedToolOutcome {
	t.Helper()
	clock := func() time.Time { return now }
	executor := &GovernedToolExecutor{governedParker: newGovernedParker(f.store, nil, clock), store: f.store, now: clock}
	call := GovernedToolCall{Run: parent, ToolCallID: "tool-1", chain: &governedRunChain{known: parent}}
	outcome, err := executor.settle(context.Background(), call, f.call, settled, false)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Park != nil {
		t.Fatalf("settled trial parked: %#v", outcome.Park)
	}
	return outcome
}

func agentTrialResultOutput(t *testing.T, outcome GovernedToolOutcome) map[string]interface{} {
	t.Helper()
	if outcome.Result == nil || len(outcome.Result.Content) == 0 {
		t.Fatalf("outcome has no result: %#v", outcome)
	}
	var value struct {
		Output map[string]interface{} `json:"output"`
	}
	if err := json.Unmarshal(outcome.Result.Content[0].Value, &value); err != nil {
		t.Fatal(err)
	}
	return value.Output
}
