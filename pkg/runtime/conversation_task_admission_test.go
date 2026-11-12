package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

type conversationTaskAdmissionTestStore = storetest.ConversationTaskAdmissionStore

func forConversationTaskAdmissionStores(t *testing.T, test func(*testing.T, conversationTaskAdmissionTestStore)) {
	t.Helper()
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		test(t, fixture.store.(conversationTaskAdmissionTestStore))
	})
}

// conversationTaskAdmissionClaim is storetest.ConversationTaskAdmissionClaim
// for the runtime tests that call it by this name.
func conversationTaskAdmissionClaim(now time.Time, reserve int) AgentRunClaim {
	return storetest.ConversationTaskAdmissionClaim(now, reserve)
}

func assertTaskAdmissionBlocked(t *testing.T, decision *AgentRunAdmissionDecision, id string, reason AgentRunAdmissionReason, limit int) {
	t.Helper()
	if decision == nil || decision.Run != nil || decision.Outcome != AgentRunAdmissionBackpressured {
		t.Fatalf("expected backpressure, got %#v", decision)
	}
	for _, block := range decision.Blocks {
		if block.RunID == id && block.Reason == reason && block.Limit == limit {
			return
		}
	}
	t.Fatalf("missing block for %s with %s/%d: %#v", id, reason, limit, decision.Blocks)
}

func TestConversationTaskForegroundReserveLeavesFourthSlotForConversation(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		now := time.Now().UTC().Add(time.Second)
		tasks := make([]*ConversationTaskResult, 4)
		for i := range tasks {
			tasks[i] = storetest.CreateConversationTaskAdmissionTask(t, store, fmt.Sprintf("reserve-%d", i))
		}
		scheduler := agentrun.NewAgentRunScheduler(store, models.WithClock(func() time.Time { return now }))
		request := agentrun.AgentRunClaimRequest{Scope: tasks[0].Task.Scope, Kind: RunKindAgentWork, WorkerID: "worker",
			AssignedAgentID: tasks[0].Task.TargetAgentID, MaxActiveForAgent: 4, ConversationTaskForegroundReserve: 1}
		for i := 0; i < 3; i++ {
			decision, err := scheduler.ClaimNextDecision(t.Context(), request)
			if err != nil || decision.Run == nil || decision.Run.Kind != RunKindAgentWork {
				t.Fatalf("claim %d: %#v %v", i, decision, err)
			}
		}
		decision, err := scheduler.ClaimNextDecision(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, decision, tasks[3].WorkRun.ID, AgentRunAdmissionReasonTaskCapacity, 3)
		inspected, err := scheduler.Inspect(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, inspected, tasks[3].WorkRun.ID, AgentRunAdmissionReasonTaskCapacity, 3)
		foreground := cloneAgentRun(tasks[0].SourceRun)
		foreground.ID, foreground.RootRunID, foreground.Status = "foreground", "foreground", AgentRunStatusQueued
		foreground.Revision, foreground.Output, foreground.CompletedAt = 1, nil, nil
		if err := store.CreateAgentRun(t.Context(), foreground); err != nil {
			t.Fatal(err)
		}
		claim := conversationTaskAdmissionClaim(now, 0)
		claim.Kind = RunKindConversation
		claimed, err := store.ClaimNextAgentRun(t.Context(), claim)
		if err != nil || claimed == nil || claimed.ID != foreground.ID {
			t.Fatalf("foreground fourth slot: %#v %v", claimed, err)
		}
		// The reserve does not bypass the existing ceiling when that slot is held.
		claim.ConversationTaskForegroundReserve = 1
		claim.Kind = RunKindAgentWork
		decision, err = store.ClaimNextAgentRunWithDecision(t.Context(), claim)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, decision, tasks[3].WorkRun.ID, AgentRunAdmissionReasonAgentCapacity, 4)
	})
}

func TestConversationTaskForegroundReserveSerializesConcurrentClaims(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		for i := 0; i < 8; i++ {
			storetest.CreateConversationTaskAdmissionTask(t, store, fmt.Sprintf("concurrent-%d", i))
		}
		var wg sync.WaitGroup
		claimed := make(chan *AgentRun, 8)
		errorsFound := make(chan error, 8)
		now := time.Now().UTC().Add(time.Second)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				claim := conversationTaskAdmissionClaim(now, 1)
				claim.WorkerID = fmt.Sprintf("worker-%d", i)
				run, err := store.ClaimNextAgentRun(t.Context(), claim)
				if err != nil {
					errorsFound <- err
				} else if run != nil {
					claimed <- run
				}
			}(i)
		}
		wg.Wait()
		close(claimed)
		close(errorsFound)
		for err := range errorsFound {
			t.Error(err)
		}
		if len(claimed) != 3 {
			t.Fatalf("concurrent claims = %d, want 3", len(claimed))
		}
	})
}

func TestConversationTaskForegroundReserveAllowsThirdTaskAfterForeground(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		tasks := make([]*ConversationTaskResult, 3)
		for i := range tasks {
			tasks[i] = storetest.CreateConversationTaskAdmissionTask(t, store, fmt.Sprintf("order-%d", i))
		}
		claim := conversationTaskAdmissionClaim(time.Now().Add(time.Second), 1)
		for i := 0; i < 2; i++ {
			run, err := store.ClaimNextAgentRun(t.Context(), claim)
			if err != nil || run == nil {
				t.Fatalf("initial task %d: %#v %v", i, run, err)
			}
		}
		foreground := cloneAgentRun(tasks[0].SourceRun)
		foreground.ID, foreground.RootRunID, foreground.Status = "foreground-first", "foreground-first", AgentRunStatusQueued
		foreground.Revision, foreground.Output, foreground.CompletedAt = 1, nil, nil
		if err := store.CreateAgentRun(t.Context(), foreground); err != nil {
			t.Fatal(err)
		}
		foregroundClaim := claim
		foregroundClaim.Kind, foregroundClaim.ConversationTaskForegroundReserve = RunKindConversation, 0
		if run, err := store.ClaimNextAgentRun(t.Context(), foregroundClaim); err != nil || run == nil || run.ID != foreground.ID {
			t.Fatalf("foreground: %#v %v", run, err)
		}
		run, err := store.ClaimNextAgentRun(t.Context(), claim)
		if err != nil || run == nil || run.ID != tasks[2].WorkRun.ID {
			t.Fatalf("third task with foreground already held: %#v %v", run, err)
		}
	})
}

func TestConversationTaskForegroundReserveCoversPersistedForks(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		task := storetest.CreateConversationTaskAdmissionTask(t, store, "fork-root")
		// A waiting task parent does not count as a live claim. Its verified forks do.
		root := cloneAgentRun(task.WorkRun)
		root.Status, root.Revision = AgentRunStatusWaitingForDependency, root.Revision+1
		updateConversationTaskSQLRun(t, store, root, task.WorkRun.Revision)
		parent := root
		for i := 0; i < 4; i++ {
			child := cloneAgentRun(task.WorkRun)
			child.ID, child.ParentRunID, child.Source = fmt.Sprintf("fork-%d", i), parent.ID, RunSourceFork
			child.ConcurrencyKey, child.Context = "fork:"+child.ID, nil // Ancestry, not a copied hint, is authoritative.
			if i < 3 {
				expires := time.Now().Add(5 * time.Minute)
				child.Status, child.LeaseOwner, child.LeaseExpiresAt = AgentRunStatusRunning, "held-worker", &expires
			}
			if err := store.CreateAgentRun(t.Context(), child); err != nil {
				t.Fatal(err)
			}
			parent = child
		}
		decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), conversationTaskAdmissionClaim(time.Now().Add(time.Second), 1))
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, decision, "fork-3", AgentRunAdmissionReasonTaskCapacity, 3)
	})
}

func TestConversationTaskForegroundReserveRejectsForgedAuthority(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		for _, scenario := range []string{"actor", "source", "hint", "cross-agent", "cross-owner", "missing-parent", "cycle"} {
			t.Run(scenario, func(t *testing.T) {
				record := storetest.ConversationTaskAdmissionRecord(t, store, "forged-"+scenario)
				if scenario == "actor" {
					record.Task.AuthenticatedActor.ID = "other-human"
					record.Task.RequestDigest, _ = conversationTaskDigest(record.Task)
				}
				result, err := store.CreateConversationTask(t.Context(), record)
				if err != nil {
					t.Fatal(err)
				}
				candidate := cloneAgentRun(result.WorkRun)
				if scenario == "source" {
					candidate.Context[conversationRunContextTriggerID] = "other-source"
					candidate.Revision++
					updateConversationTaskSQLRun(t, store, candidate, result.WorkRun.Revision)
				} else if scenario != "actor" {
					root := cloneAgentRun(result.WorkRun)
					root.Status, root.Revision = AgentRunStatusWaitingForDependency, root.Revision+1
					updateConversationTaskSQLRun(t, store, root, result.WorkRun.Revision)
					candidate.ID, candidate.ParentRunID, candidate.Source = "candidate-"+scenario, root.ID, RunSourceFork
					candidate.ConcurrencyKey = "fork:" + candidate.ID
					switch scenario {
					case "hint":
						candidate.RootRunID, candidate.ParentRunID = candidate.ID, ""
					case "cross-agent":
						candidate.AssignedAgentID = "other-agent"
					case "cross-owner":
						candidate.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: "other-agent"}
					case "missing-parent":
						candidate.ParentRunID = "missing"
					case "cycle":
						candidate.ParentRunID = candidate.ID
					}
					if err := store.CreateAgentRun(t.Context(), candidate); err != nil {
						t.Fatal(err)
					}
				}
				claim := conversationTaskAdmissionClaim(time.Now().Add(time.Second), 1)
				claim.AssignedAgentID = candidate.AssignedAgentID
				decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), claim)
				if err != nil {
					t.Fatal(err)
				}
				assertTaskAdmissionBlocked(t, decision, candidate.ID, AgentRunAdmissionReasonTaskProvenance, 0)
			})
		}
	})
}

func TestConversationTaskForegroundReservePreservesOrdinaryAndDefaultPolicy(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		for i := 0; i < 4; i++ {
			task := storetest.CreateConversationTaskAdmissionTask(t, store, fmt.Sprintf("default-%d", i))
			claim := conversationTaskAdmissionClaim(time.Now().Add(time.Second), 0)
			run, err := store.ClaimNextAgentRun(t.Context(), claim)
			if err != nil || run == nil || run.ID != task.WorkRun.ID {
				t.Fatalf("zero reserve claim %d: %#v %v", i, run, err)
			}
		}
	})
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		for i := 0; i < 5; i++ {
			now := time.Now().Add(-time.Minute)
			run := &AgentRun{ID: fmt.Sprintf("ordinary-%d", i), RootRunID: fmt.Sprintf("ordinary-%d", i),
				Scope: Scope{Kind: "tenant", ID: "task-sql"}, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "task-agent"},
				AssignedAgentID: "task-agent", Source: RunSourceSchedule, Goal: "Ordinary scheduled work", Status: AgentRunStatusQueued,
				AvailableAt: now, QueueEnteredAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
			if err := store.CreateAgentRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
		}
		claim := conversationTaskAdmissionClaim(time.Now(), 1)
		for i := 0; i < 4; i++ {
			run, err := store.ClaimNextAgentRun(t.Context(), claim)
			if err != nil || run == nil {
				t.Fatalf("ordinary claim %d: %#v %v", i, run, err)
			}
		}
		decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), claim)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, decision, "ordinary-4", AgentRunAdmissionReasonAgentCapacity, 4)
	})
}

func TestConversationTaskForegroundReserveZeroDoesNotReadTaskProof(t *testing.T) {
	claim := conversationTaskAdmissionClaim(time.Now(), 0)
	lookup := conversationTaskAdmissionLookup{Task: func(context.Context, Scope, string) (*ConversationTask, error) { panic("zero reserve read task proof") }}
	if err := prepareConversationTaskAdmission(t.Context(), &claim, []*AgentRun{{ID: "plain", Scope: claim.Scope, Status: AgentRunStatusQueued}}, lookup); err != nil {
		t.Fatal(err)
	}
	for _, reserve := range []int{-1, 4, 5} {
		claim.ConversationTaskForegroundReserve = reserve
		if err := claim.Validate(); err == nil {
			t.Fatalf("invalid reserve %d accepted", reserve)
		}
		worker := AgentRunWorkerConfig{Scope: claim.Scope, MaxActiveForAgent: 4, ConversationTaskForegroundReserve: reserve}
		if err := worker.applyDefaults(); err == nil {
			t.Fatalf("worker invalid reserve %d accepted", reserve)
		}
		dynamic := DynamicAgentRunWorkerConfig{MaxActiveForAgent: 4, ConversationTaskForegroundReserve: reserve}
		if err := dynamic.applyDefaults(); err == nil {
			t.Fatalf("supervisor invalid reserve %d accepted", reserve)
		}
	}
}
