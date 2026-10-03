package runtime

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func claimedForkTaskFixture(t *testing.T) (*MemoryStore, *AgentRun, *ConversationTaskResult) {
	t.Helper()
	store := NewMemoryStore()
	foreground, turn, _, _ := settledWorkerTaskFixture(t, store)
	accepted, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(foreground, turn))
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	root := store.agentRuns[portfolioKey(accepted.WorkRun.Scope, accepted.WorkRun.ID)]
	root.Context["privateBrief"] = "source-only"
	root.Context["release"] = "original"
	root.Plan = map[string]interface{}{"research": "canonical-plan"}
	store.mu.Unlock()
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: accepted.Task.Scope, Kind: RunKindAgentWork, AssignedAgentID: accepted.Task.TargetAgentID,
		WorkerID: "fork-worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != accepted.WorkRun.ID {
		t.Fatalf("claim task root: %#v %v", claimed, err)
	}
	return store, claimed, accepted
}

func taskForkRequest(source *AgentRun, branches ...RunForkBranch) CreateRunForkRequest {
	return CreateRunForkRequest{
		Scope: source.Scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		WorkerID: source.LeaseOwner, ForkID: "task-research",
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast}, Branches: branches,
	}
}

func TestRunForkTaskSameAgentInheritsCanonicalContext(t *testing.T) {
	store, source, accepted := claimedForkTaskFixture(t)
	result, err := NewRunForkCoordinator(store).Create(t.Context(), taskForkRequest(source,
		RunForkBranch{ID: "omitted", Goal: "Research omitted target", Context: map[string]interface{}{"release": "omitted-release"}, Budget: &BudgetPolicy{}},
		RunForkBranch{ID: "same", AssignedAgentID: source.AssignedAgentID, Goal: "Research explicit same target", Context: map[string]interface{}{"release": "same-release"}, Budget: &BudgetPolicy{}},
	))
	if err != nil || result == nil || len(result.Children) != 2 {
		t.Fatalf("create task fork: %#v %v", result, err)
	}
	for _, child := range result.Children {
		if child.ParentRunID != source.ID || child.RootRunID != source.ID || child.Source != RunSourceFork {
			t.Fatalf("task fork lost owned lineage: %#v", child)
		}
		if child.AssignedAgentID == source.AssignedAgentID {
			for _, key := range []string{ConversationTaskContextKey, conversationRunContextConversationID, conversationRunContextTriggerID,
				"threadRootMessageId", runReportingContextRootRunID, runReportingContextMilestones, "privateBrief"} {
				if !reflect.DeepEqual(child.Context[key], source.Context[key]) {
					t.Fatalf("same-agent task fork lost %s: %#v", key, child.Context)
				}
			}
			if !reflect.DeepEqual(child.Plan, source.Plan) || child.Context["release"] == source.Context["release"] {
				t.Fatalf("task fork lost canonical plan or branch arguments: %#v", child)
			}
		} else {
			t.Fatalf("task fork changed its assigned agent: %#v", child)
		}
	}
	for range 2 {
		child, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
			Scope: source.Scope, Kind: RunKindAgentWork, AssignedAgentID: source.AssignedAgentID,
			WorkerID: "task-child-worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
			MaxActiveForAgent: 4, ConversationTaskForegroundReserve: 1,
		})
		if err != nil || child == nil || child.ParentRunID != source.ID || child.Context[ConversationTaskContextKey] != accepted.Task.ID {
			t.Fatalf("verified task child was not admitted: %#v %v", child, err)
		}
	}
}

func TestRunForkTaskRejectsForeignTargetBeforeCommittingDependencies(t *testing.T) {
	store, source, accepted := claimedForkTaskFixture(t)
	req := taskForkRequest(source,
		RunForkBranch{ID: "same", AssignedAgentID: source.AssignedAgentID, Goal: "Research same target", Budget: &BudgetPolicy{}},
		RunForkBranch{ID: "foreign", AssignedAgentID: "other-agent", Goal: "Research foreign target", Context: map[string]interface{}{ConversationTaskContextKey: accepted.Task.ID}, Budget: &BudgetPolicy{}},
	)
	result, err := NewRunForkCoordinator(store).Create(t.Context(), req)
	if !errors.Is(err, ErrInvalidConversationTask) || result != nil {
		t.Fatalf("foreign task fork was accepted: %#v %v", result, err)
	}
	children, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: source.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(children) != 0 {
		t.Fatalf("rejected foreign task fork persisted children: %#v %v", children, err)
	}
	group, err := store.FindRunDependencyGroupByIdempotencyKey(t.Context(), source.Scope, "fork:"+source.ID+":"+req.ForkID)
	if err != nil || group != nil {
		t.Fatalf("rejected foreign task fork persisted dependency group: %#v %v", group, err)
	}
	current, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
	if err != nil || current.Status != AgentRunStatusRunning || current.Revision != source.Revision {
		t.Fatalf("rejected foreign task fork suspended source: %#v %v", current, err)
	}
}

func TestTaskWorkRejectsDelegationBeforeRequestOrGroupCommit(t *testing.T) {
	for _, target := range []string{"agent", "other-agent"} {
		for _, grouped := range []bool{false, true} {
			t.Run(target+map[bool]string{false: "/singular", true: "/grouped"}[grouped], func(t *testing.T) {
				store, source, _ := claimedForkTaskFixture(t)
				service := NewCollaborationService(store)
				requester := CollaborationParty{Type: source.Owner.Type, ID: source.Owner.ID}
				recipient := CollaborationParty{Type: OwnerTypeAgent, ID: target}
				var err error
				if grouped {
					_, err = service.CreateAgentRequestGroup(t.Context(), CreateAgentRequestGroupRequest{
						Scope: source.Scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision, Requester: requester,
						Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast}, IdempotencyKey: "task-delegation-group",
						Requests: []AgentRequestGroupSpec{{Kind: AgentRequestKindRequest, Recipient: recipient, Goal: "Review", BudgetAllocation: &BudgetPolicy{}}},
					})
				} else {
					_, err = service.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
						Scope: source.Scope, SourceRunID: source.ID, Kind: AgentRequestKindRequest, Requester: requester, Recipient: recipient,
						Goal: "Review", BudgetAllocation: &BudgetPolicy{}, IdempotencyKey: "task-delegation",
					})
				}
				if !errors.Is(err, ErrInvalidConversationTask) {
					t.Fatalf("task delegation was accepted: %v", err)
				}
				requests, err := store.ListAgentRequests(t.Context(), AgentRequestFilter{Scope: source.Scope, SourceRunID: source.ID, Limit: 10})
				if err != nil || len(requests) != 0 {
					t.Fatalf("rejected task delegation persisted requests: %#v %v", requests, err)
				}
				group, err := store.FindRunDependencyGroupByIdempotencyKey(t.Context(), source.Scope, "task-delegation-group")
				if err != nil || group != nil {
					t.Fatalf("rejected task delegation persisted dependencies: %#v %v", group, err)
				}
				children, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: source.Scope, ParentRunID: source.ID, Limit: 10})
				current, sourceErr := store.GetAgentRun(t.Context(), source.Scope, source.ID)
				if err != nil || len(children) != 0 || sourceErr != nil || current.Status != AgentRunStatusRunning || current.Revision != source.Revision {
					t.Fatalf("rejected task delegation changed source/children: source=%#v children=%#v errors=%v %v", current, children, err, sourceErr)
				}
			})
		}
	}
}

func TestRunForkTaskRejectsBranchProvenanceReplacement(t *testing.T) {
	store, source, _ := claimedForkTaskFixture(t)
	for _, key := range []string{ConversationTaskContextKey, conversationRunContextConversationID, conversationRunContextTriggerID,
		"threadRootMessageId", runReportingContextRootRunID, runReportingContextMilestones} {
		t.Run(key, func(t *testing.T) {
			_, err := NewRunForkCoordinator(store).Create(t.Context(), taskForkRequest(source,
				RunForkBranch{ID: "same", AssignedAgentID: source.AssignedAgentID, Goal: "Research", Context: map[string]interface{}{key: "forged"}, Budget: &BudgetPolicy{}},
			))
			if !errors.Is(err, ErrInvalidConversationTask) {
				t.Fatalf("task provenance replacement was accepted: %v", err)
			}
		})
	}
	children, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: source.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(children) != 0 {
		t.Fatalf("rejected task fork persisted children: %#v %v", children, err)
	}
}

func TestRunForkTaskRejectsContextOnlySelfRoot(t *testing.T) {
	store := NewMemoryStore()
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: Scope{Kind: "tenant", ID: "forged-task-fork"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "Forged task source", Context: map[string]interface{}{ConversationTaskContextKey: "forged-task"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: source.Scope, WorkerID: "fork-worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim fake root: %#v %v", claimed, err)
	}
	_, err = NewRunForkCoordinator(store).Create(t.Context(), taskForkRequest(claimed,
		RunForkBranch{ID: "same", AssignedAgentID: source.AssignedAgentID, Goal: "Research"},
	))
	if !errors.Is(err, ErrConversationTaskNotFound) {
		t.Fatalf("context-only task hint became authority: %v", err)
	}
	children, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: source.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(children) != 0 {
		t.Fatalf("forged task created children: %#v %v", children, err)
	}
}

func TestRunForkTaskSourceCascadeIsIndependentAndWorkCascadeOwnsChildren(t *testing.T) {
	store, source, accepted := claimedForkTaskFixture(t)
	result, err := NewRunForkCoordinator(store).Create(t.Context(), taskForkRequest(source,
		RunForkBranch{ID: "same", AssignedAgentID: source.AssignedAgentID, Goal: "Research", Budget: &BudgetPolicy{}},
	))
	if err != nil {
		t.Fatal(err)
	}
	commands := NewRunCommandService(store)
	if err := commands.CascadeTerminalRun(t.Context(), accepted.SourceRun); err != nil {
		t.Fatal(err)
	}
	root, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
	if err != nil || root.Status != AgentRunStatusWaitingForDependency || root.ParentRunID != "" || root.RootRunID != root.ID {
		t.Fatalf("source completion canceled independent task: %#v %v", root, err)
	}
	child, err := store.GetAgentRun(t.Context(), source.Scope, result.Children[0].ID)
	if err != nil || child.Status != AgentRunStatusQueued {
		t.Fatalf("source completion canceled task fork: %#v %v", child, err)
	}
	if _, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{
		Scope: source.Scope, RunID: root.ID, ExpectedRevision: root.Revision, Kind: AgentRunCommandCancel,
		Actor: ActivityActor{Type: "user", ID: accepted.Task.AuthenticatedActor.ID}, Summary: "Stop the task",
	}); err != nil {
		t.Fatal(err)
	}
	child, err = store.GetAgentRun(t.Context(), source.Scope, child.ID)
	if err != nil || child.Status != AgentRunStatusCanceled {
		t.Fatalf("task cancellation did not own its fork: %#v %v", child, err)
	}
}
