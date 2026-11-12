package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/google/uuid"
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

// putAgentTrialRun stores run in whatever state it is in, bypassing
// transition rules: it creates the Run, or saves it as the next revision of
// the stored one and sets run.Revision to that revision.
func putAgentTrialRun(t *testing.T, store *sqlite.Store, run *models.AgentRun) *models.AgentRun {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil {
		if err := store.CreateAgentRun(ctx, models.CloneAgentRun(run)); err != nil {
			t.Fatalf("create run %s: %v", run.ID, err)
		}
		return run
	}
	run.Revision = current.Revision + 1
	event := &models.ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.updated",
		Summary: "Put agent trial test run", Actor: models.ActivityActor{Type: "test", ID: "agent-trial"},
		Visibility: models.ActivityVisibilityScope, CreatedAt: time.Now().UTC()}
	if _, err := store.UpdateAgentRunWithEvent(ctx, models.CloneAgentRun(run), current.Revision, event, nil); err != nil {
		t.Fatalf("update run %s: %v", run.ID, err)
	}
	return run
}

func mustAgentRun(t *testing.T, store models.PortfolioStore, scope models.Scope, id string) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil || run == nil {
		t.Fatalf("run %s = %#v, %v", id, run, err)
	}
	return run
}

// startAgentTrial dispatches try_agent for the fixture's builder Run.
func startAgentTrial(t *testing.T, f *agentBuilderDispatchFixture, callID string) (*models.ActionCall, *models.AgentRun) {
	t.Helper()
	arguments := f.resolve(t, models.AgentActionTryAgent, agentTrialArguments())
	output, err := f.execute(t, models.AgentActionTryAgent, callID, arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	childID, _ := output["trialRunId"].(string)
	if output["resourceType"] != "agent_trial" || output["status"] != "running" || childID == "" {
		t.Fatalf("try_agent output = %#v", output)
	}
	call := &models.ActionCall{
		ID: callID, Scope: f.scope, RunID: f.run.ID, DeploymentID: "finn", SkillID: models.AgentManagementSkillID,
		SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionTryAgent, Status: models.ActionCallStatusSucceeded,
		Origin: models.ActionOriginLoop, Output: output,
	}
	return call, mustAgentRun(t, f.store, f.scope, childID)
}

func TestAgentTrialStartsChildWithoutDefinition(t *testing.T) {
	f := newAgentBuilderDispatchFixture(t)
	ctx := context.Background()
	before, err := f.agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{Scope: models.ScopeReference(f.scope)})
	if err != nil {
		t.Fatal(err)
	}
	_, child := startAgentTrial(t, f, "trial-call")
	if child.ID != agentTrialRunID(f.run.ID, "trial-call") {
		t.Fatalf("trial child id = %q", child.ID)
	}

	if _, err := f.agents.GetDefinition(ctx, agentBuilderResourceID("agent-definition", f.scope, "trial-call"), agentBuilderDefinitionVersion); !errors.Is(err, kernelagent.ErrDefinitionNotFound) {
		t.Fatalf("try_agent registered a definition: %v", err)
	}
	after, err := f.agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{Scope: models.ScopeReference(f.scope)})
	if err != nil || len(after) != len(before) {
		t.Fatalf("deployments %d -> %d, %v", len(before), len(after), err)
	}
	if child.Kind != models.RunKindAgentWork || child.ParentRunID != f.run.ID || child.AssignedAgentID != "finn" ||
		child.Owner != f.run.Owner || child.Source != models.RunSourceHandoff || child.Status != models.AgentRunStatusQueued ||
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
	if conversationID, _ := child.Context[models.ConversationRunConversationIDContextKey].(string); conversationID != "" {
		t.Fatalf("trial child posts to a chat: %#v", child.Context)
	}

	// A replay of the call finds the same child.
	arguments := f.resolve(t, models.AgentActionTryAgent, agentTrialArguments())
	again, err := f.execute(t, models.AgentActionTryAgent, "trial-call", arguments, agentBuilderApprover)
	if err != nil || again["trialRunId"] != child.ID {
		t.Fatalf("replay = %#v, %v", again, err)
	}
	children, err := f.store.ListAgentRuns(ctx, models.AgentRunFilter{Scope: f.scope, ParentRunID: f.run.ID})
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %d, %v", len(children), err)
	}
}

func TestAgentTrialRefusesSensitiveMessage(t *testing.T) {
	f := newAgentBuilderDispatchFixture(t)
	arguments := agentTrialArguments()
	arguments["message"] = "Log in with password: hunter2-correct-horse"
	if _, err := f.propose(t, models.AgentActionTryAgent, arguments); err == nil {
		t.Fatal("sensitive trial message accepted")
	}
}

// agentTrialFinalizerFixture is a builder Run waiting on its trial child.
type agentTrialFinalizerFixture struct {
	store  *sqlite.Store
	scope  models.Scope
	parent *models.AgentRun
	child  *models.AgentRun
	call   *models.ActionCall
}

func newAgentTrialFinalizerFixture(t *testing.T, parentStatus models.AgentRunStatus) *agentTrialFinalizerFixture {
	t.Helper()
	f := newAgentBuilderDispatchFixture(t)
	call, child := startAgentTrial(t, f, "trial-call")
	parent := mustAgentRun(t, f.store, f.scope, f.run.ID)
	parent.Status = parentStatus
	if parentStatus == models.AgentRunStatusWaitingForEvent {
		deadline := agentTrialDeadline(child)
		parent.WakeCondition = &models.WakeCondition{Type: agentrun.AgentTrialWakeType, Reference: child.ID, WakeAt: &deadline}
	}
	putAgentTrialRun(t, f.store.Store, parent)
	return &agentTrialFinalizerFixture{store: f.store.Store, scope: f.scope, parent: parent, child: child, call: call}
}

func (f *agentTrialFinalizerFixture) endChild(t *testing.T, status models.AgentRunStatus, output map[string]interface{}, message string) *models.AgentRun {
	t.Helper()
	child := mustAgentRun(t, f.store, f.scope, f.child.ID)
	ended := child.CreatedAt.Add(30 * time.Second)
	child.Status, child.Output, child.Error, child.CompletedAt, child.UpdatedAt = status, output, message, &ended, ended
	putAgentTrialRun(t, f.store, child)
	return child
}

func TestAgentTrialFinalizerHeldParentKeepsStatus(t *testing.T) {
	f := newAgentTrialFinalizerFixture(t, models.AgentRunStatusRunning)
	child := f.endChild(t, models.AgentRunStatusFailed, nil, "model unavailable")
	if err := NewAgentTrialFinalizer(f.store, nil, nil).FinalizeRun(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	parent := mustAgentRun(t, f.store, f.scope, f.parent.ID)
	if parent.Status != models.AgentRunStatusRunning || parent.Revision != f.parent.Revision+1 {
		t.Fatalf("parent = %s rev %d (was %d)", parent.Status, parent.Revision, f.parent.Revision)
	}
	receipt := agentTrialReceiptFor(f.call.ID, child.ID, parent.PendingInterventions)
	if receipt == nil || receipt.Status != AgentTrialStatusFailed || receipt.FailureMessage != "model unavailable" {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestAgentTrialFinalizerIgnoresOtherRuns(t *testing.T) {
	f := newAgentTrialFinalizerFixture(t, models.AgentRunStatusWaitingForEvent)
	// A canceled trial reaches only a parent waiting on it; the parent that
	// canceled it at the deadline already reported timed_out.
	parent := mustAgentRun(t, f.store, f.scope, f.parent.ID)
	parent.Status, parent.WakeCondition = models.AgentRunStatusRunning, nil
	putAgentTrialRun(t, f.store, parent)
	child := f.endChild(t, models.AgentRunStatusCanceled, nil, "")
	finalizer := NewAgentTrialFinalizer(f.store, nil, nil)
	if err := finalizer.FinalizeRun(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	// A Run without a trial context is not touched.
	other := models.CloneAgentRun(parent)
	other.ID, other.Status, other.Context = "other", models.AgentRunStatusCompleted, nil
	if err := finalizer.FinalizeRun(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if after := mustAgentRun(t, f.store, f.scope, f.parent.ID); after.Revision != parent.Revision || len(after.PendingInterventions) != len(parent.PendingInterventions) {
		t.Fatalf("parent changed: %#v", after)
	}
}
