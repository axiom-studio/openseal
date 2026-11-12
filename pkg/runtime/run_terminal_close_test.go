package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

type terminalCloseTestStore interface {
	RunCommandStore
	AgentTurnStore
	AgentRunScheduleStore
}

func terminalCloseStores() []struct {
	name  string
	store func(*testing.T) terminalCloseTestStore
} {
	return []struct {
		name  string
		store func(*testing.T) terminalCloseTestStore
	}{
		{name: "sqlite", store: func(t *testing.T) terminalCloseTestStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "terminal-close.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
}

// claimedTerminalCloseRun creates a Run and claims it as worker "loop-worker",
// so it is running under a live lease.
func claimedTerminalCloseRun(t *testing.T, store terminalCloseTestStore, key string) (*RunCommandService, *AgentRun) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "close"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
	service := NewRunCommandService(store)
	created, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, ConcurrencyKey: "agent-1:" + key, Goal: "Do the work",
		Source: RunSourceManual, IdempotencyKey: key, Actor: ActivityActor{Type: "user", ID: "7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, WorkerID: "loop-worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != created.Run.ID || claimed.Status != AgentRunStatusRunning {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return service, claimed
}

func cancelTerminalCloseRun(t *testing.T, store terminalCloseTestStore, service *RunCommandService, runID string) *AgentRun {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, Scope{Kind: "tenant", ID: "close"}, runID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: current.Scope, RunID: current.ID, ExpectedRevision: current.Revision,
		Kind: AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: "7"},
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if result.Run.Status != AgentRunStatusCanceled {
		t.Fatalf("canceled run = %#v", result.Run)
	}
	return result.Run
}

func TestCancelFinishesRunningStep(t *testing.T) {
	for _, fixture := range terminalCloseStores() {
		t.Run(fixture.name, func(t *testing.T) {
			t.Run("live step", func(t *testing.T) {
				ctx := context.Background()
				store := fixture.store(t)
				service, run := claimedTerminalCloseRun(t, store, "cancel-live-step")
				turns := NewAgentTurnService(store, store)
				// The step the loop is running, under the Run's lease owner.
				live, err := turns.BeginTurn(ctx, BeginAgentTurnRequest{
					Scope: run.Scope, RunID: run.ID, Producer: StepProducerModel, WorkerID: "loop-worker", LeaseDuration: time.Minute,
				})
				if err != nil {
					t.Fatal(err)
				}
				cancelTerminalCloseRun(t, store, service, run.ID)
				assertStepEndedByRun(t, store, run.Scope, live.ID)
				// The loop's own finish of the step now fails instead of
				// overwriting the close.
				if _, err := turns.FinishTurn(ctx, run.Scope, live.ID, FinishAgentTurnRequest{
					ExpectedRevision: live.Revision, Status: AgentTurnStatusCompleted, WorkerID: "loop-worker",
				}); err == nil {
					t.Fatal("the loop finished a step the cancel had already closed")
				}
				// Cascading again is a no-op.
				current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := service.CascadeTerminalRun(ctx, current); err != nil {
					t.Fatalf("second cascade: %v", err)
				}
				again, _ := store.GetAgentTurn(ctx, run.Scope, live.ID)
				if again.Revision != live.Revision+1 {
					t.Fatalf("second cascade rewrote the step: revision %d", again.Revision)
				}
			})
			t.Run("released step", func(t *testing.T) {
				ctx := context.Background()
				store := fixture.store(t)
				service, run := claimedTerminalCloseRun(t, store, "cancel-released-step")
				turns := NewAgentTurnService(store, store)
				// A step released by a lost worker: running, but without a lease.
				released, err := turns.BeginTurn(ctx, BeginAgentTurnRequest{
					Scope: run.Scope, RunID: run.ID, Producer: StepProducerModel, WorkerID: "lost-worker", LeaseDuration: time.Minute,
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := turns.ReleaseTurn(ctx, run.Scope, released.ID, released.Revision, "lost-worker"); err != nil {
					t.Fatal(err)
				}
				cancelTerminalCloseRun(t, store, service, run.ID)
				assertStepEndedByRun(t, store, run.Scope, released.ID)
			})
		})
	}
}

func assertStepEndedByRun(t *testing.T, store terminalCloseTestStore, scope Scope, stepID string) {
	t.Helper()
	step, err := store.GetAgentTurn(context.Background(), scope, stepID)
	if err != nil {
		t.Fatal(err)
	}
	if step.Status != AgentTurnStatusCanceled || step.FailureCode != StepFailureRunEnded || step.CompletedAt == nil ||
		step.LeaseOwner != "" || step.LeaseExpiresAt != nil || step.Error == "" {
		t.Fatalf("step %s after cancel = %#v", stepID, step)
	}
}

func TestTerminalRunClosesOpenToolEntries(t *testing.T) {
	for _, fixture := range terminalCloseStores() {
		t.Run(fixture.name, func(t *testing.T) {
			store := fixture.store(t)
			if _, ok := store.(AgentSessionStore); !ok {
				t.Skip("this store has no session log yet (SQLite: store-5)")
			}
			t.Run("cancel", func(t *testing.T) {
				store := fixture.store(t)
				service, run := claimedTerminalCloseRun(t, store, "cancel-tools")
				open, done := seedTerminalCloseToolEntries(t, store, run)
				cancelTerminalCloseRun(t, store, service, run.ID)
				assertTerminalCloseToolEntries(t, store.(AgentSessionStore), run.Scope, open, done, "Not run: the request was canceled")
			})
			t.Run("loop terminal exit", func(t *testing.T) {
				ctx := context.Background()
				store := fixture.store(t)
				service, run := claimedTerminalCloseRun(t, store, "fail-tools")
				open, done := seedTerminalCloseToolEntries(t, store, run)
				current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				// The loop fails the Run under its lease, then the worker's
				// terminal finalization cascades.
				failed, _, err := NewRunActivityService(store, store).TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
					ExpectedRevision: current.Revision, Status: AgentRunStatusFailed, Summary: "Run failed",
					Error: "repeat limit", LeaseOwner: "loop-worker", Actor: ActivityActor{Type: "runtime", ID: "loop"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := service.CascadeTerminalRun(ctx, failed); err != nil {
					t.Fatal(err)
				}
				assertTerminalCloseToolEntries(t, store.(AgentSessionStore), run.Scope, open, done, "Not run: the request was failed")
			})
			t.Run("lease-less close needs a terminal run", func(t *testing.T) {
				store := fixture.store(t)
				_, run := claimedTerminalCloseRun(t, store, "running-tools")
				open, _ := seedTerminalCloseToolEntries(t, store, run)
				sessions := store.(AgentSessionStore)
				entry, err := sessions.GetSessionEntry(context.Background(), run.Scope, open[0])
				if err != nil {
					t.Fatal(err)
				}
				_, err = sessions.TransitionToolEntry(context.Background(), ToolEntryTransitionRequest{
					Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, From: entry.State, To: ToolEntryError,
					Result: terminalRunToolResult(AgentRunStatusCanceled),
				})
				if !errors.Is(err, ErrLeaseLost) {
					t.Fatalf("lease-less close of a running run = %v, want ErrLeaseLost", err)
				}
				_, err = sessions.TransitionToolEntry(context.Background(), ToolEntryTransitionRequest{
					Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, From: entry.State, To: ToolEntryRunning,
				})
				if !errors.Is(err, ErrInvalidToolEntryTransition) {
					t.Fatalf("lease-less start = %v, want ErrInvalidToolEntryTransition", err)
				}
			})
		})
	}
}

// seedTerminalCloseToolEntries opens the Run's session and appends one step
// with three tool calls: pending, awaiting approval, and completed. It
// returns the open and the completed entry ids.
func seedTerminalCloseToolEntries(t *testing.T, store terminalCloseTestStore, run *AgentRun) ([]string, string) {
	t.Helper()
	ctx := context.Background()
	sessions := store.(AgentSessionStore)
	now := time.Now()
	lease := AgentRunLeaseGuard{WorkerID: "loop-worker", Now: now}
	opened, err := sessions.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{
		Scope: run.Scope, OwnerRunID: run.ID, AgentID: run.AssignedAgentID, Lease: lease, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := NewAgentTurnService(store, store).BeginTurn(ctx, BeginAgentTurnRequest{
		Scope: run.Scope, RunID: run.ID, SessionID: opened.Session.ID, Producer: StepProducerModel,
		WorkerID: "loop-worker", LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	prefix := run.ID + ":"
	assistant := storetest.AssistantSessionEntry(prefix+"assistant", step.ID, "call-1", "call-2", "call-3")
	pending := storetest.ToolSessionEntry(prefix+"tool-1", step.ID, "call-1", ToolEntryPending)
	awaiting := storetest.ToolSessionEntry(prefix+"tool-2", step.ID, "call-2", ToolEntryPending)
	completed := storetest.ToolSessionEntry(prefix+"tool-3", step.ID, "call-3", ToolEntryCompleted)
	if _, err := sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: opened.Session.ID, RunID: run.ID, Lease: &lease, Now: now,
		Entries: []*SessionEntry{assistant, pending, awaiting, completed},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.TransitionToolEntry(ctx, ToolEntryTransitionRequest{
		Scope: run.Scope, EntryID: awaiting.ID, RunID: run.ID, Lease: lease, Now: now,
		From: ToolEntryPending, To: ToolEntryAwaitingApproval, Wait: &ToolEntryWait{ApprovalID: "approval-1"},
	}); err != nil {
		t.Fatal(err)
	}
	return []string{pending.ID, awaiting.ID}, completed.ID
}

func assertTerminalCloseToolEntries(t *testing.T, sessions AgentSessionStore, scope Scope, open []string, done, text string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range open {
		entry, err := sessions.GetSessionEntry(ctx, scope, id)
		if err != nil {
			t.Fatal(err)
		}
		result := entry.Tool.Result
		if entry.State != ToolEntryError || result == nil || !result.IsError || result.Status != ToolResultCanceled ||
			len(result.Content) != 1 || result.Content[0].Text != text || entry.Tool.Wait != nil || entry.Tool.CompletedAt == nil {
			t.Fatalf("entry %s after the run ended = state %s, tool %#v", id, entry.State, entry.Tool)
		}
	}
	entry, err := sessions.GetSessionEntry(ctx, scope, done)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != ToolEntryCompleted || entry.Revision != 1 || entry.Tool.Result.Content[0].Text != "done" {
		t.Fatalf("a finished entry was rewritten: %#v", entry)
	}
	left, err := sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{Scope: scope, RunID: entry.RunID, States: OpenToolEntryStates})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d tool entries are still open", len(left))
	}
}
