package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

type terminalCloseTestStore interface {
	models.RunCommandStore
	models.AgentTurnStore
	models.AgentRunScheduleStore
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
			store, err := sqlite.New(filepath.Join(t.TempDir(), "terminal-close.db"))
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
func claimedTerminalCloseRun(t *testing.T, store terminalCloseTestStore, key string) (*agentrun.RunCommandService, *models.AgentRun) {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "close"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	service := agentrun.NewRunCommandService(store)
	created, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, ConcurrencyKey: "agent-1:" + key, Goal: "Do the work",
		Source: models.RunSourceManual, IdempotencyKey: key, Actor: models.ActivityActor{Type: "user", ID: "7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: scope, WorkerID: "loop-worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != created.Run.ID || claimed.Status != models.AgentRunStatusRunning {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return service, claimed
}

func cancelTerminalCloseRun(t *testing.T, store terminalCloseTestStore, service *agentrun.RunCommandService, runID string) *models.AgentRun {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, models.Scope{Kind: "tenant", ID: "close"}, runID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{
		Scope: current.Scope, RunID: current.ID, ExpectedRevision: current.Revision,
		Kind: agentrun.AgentRunCommandCancel, Actor: models.ActivityActor{Type: "user", ID: "7"},
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if result.Run.Status != models.AgentRunStatusCanceled {
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
				turns := activity.NewAgentTurnService(store, store)
				// The step the loop is running, under the Run's lease owner.
				live, err := turns.BeginTurn(ctx, activity.BeginAgentTurnRequest{
					Scope: run.Scope, RunID: run.ID, Producer: models.StepProducerModel, WorkerID: "loop-worker", LeaseDuration: time.Minute,
				})
				if err != nil {
					t.Fatal(err)
				}
				cancelTerminalCloseRun(t, store, service, run.ID)
				assertStepEndedByRun(t, store, run.Scope, live.ID)
				// The loop's own finish of the step now fails instead of
				// overwriting the close.
				if _, err := turns.FinishTurn(ctx, run.Scope, live.ID, activity.FinishAgentTurnRequest{
					ExpectedRevision: live.Revision, Status: models.AgentTurnStatusCompleted, WorkerID: "loop-worker",
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
				turns := activity.NewAgentTurnService(store, store)
				// A step released by a lost worker: running, but without a lease.
				released, err := turns.BeginTurn(ctx, activity.BeginAgentTurnRequest{
					Scope: run.Scope, RunID: run.ID, Producer: models.StepProducerModel, WorkerID: "lost-worker", LeaseDuration: time.Minute,
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

func assertStepEndedByRun(t *testing.T, store terminalCloseTestStore, scope models.Scope, stepID string) {
	t.Helper()
	step, err := store.GetAgentTurn(context.Background(), scope, stepID)
	if err != nil {
		t.Fatal(err)
	}
	if step.Status != models.AgentTurnStatusCanceled || step.FailureCode != agentrun.StepFailureRunEnded || step.CompletedAt == nil ||
		step.LeaseOwner != "" || step.LeaseExpiresAt != nil || step.Error == "" {
		t.Fatalf("step %s after cancel = %#v", stepID, step)
	}
}

func TestTerminalRunClosesOpenToolEntries(t *testing.T) {
	for _, fixture := range terminalCloseStores() {
		t.Run(fixture.name, func(t *testing.T) {
			store := fixture.store(t)
			if _, ok := store.(models.AgentSessionStore); !ok {
				t.Skip("this store has no session log yet (SQLite: store-5)")
			}
			t.Run("cancel", func(t *testing.T) {
				store := fixture.store(t)
				service, run := claimedTerminalCloseRun(t, store, "cancel-tools")
				open, done := seedTerminalCloseToolEntries(t, store, run)
				cancelTerminalCloseRun(t, store, service, run.ID)
				assertTerminalCloseToolEntries(t, store.(models.AgentSessionStore), run.Scope, open, done, "Not run: the request was canceled")
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
				failed, _, err := activity.NewRunActivityService(store, store).TransitionRun(ctx, run.Scope, run.ID, activity.RunTransitionRequest{
					ExpectedRevision: current.Revision, Status: models.AgentRunStatusFailed, Summary: "Run failed",
					Error: "repeat limit", LeaseOwner: "loop-worker", Actor: models.ActivityActor{Type: "runtime", ID: "loop"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := service.CascadeTerminalRun(ctx, failed); err != nil {
					t.Fatal(err)
				}
				assertTerminalCloseToolEntries(t, store.(models.AgentSessionStore), run.Scope, open, done, "Not run: the request was failed")
			})
			t.Run("lease-less close needs a terminal run", func(t *testing.T) {
				store := fixture.store(t)
				_, run := claimedTerminalCloseRun(t, store, "running-tools")
				open, _ := seedTerminalCloseToolEntries(t, store, run)
				sessions := store.(models.AgentSessionStore)
				entry, err := sessions.GetSessionEntry(context.Background(), run.Scope, open[0])
				if err != nil {
					t.Fatal(err)
				}
				_, err = sessions.TransitionToolEntry(context.Background(), models.ToolEntryTransitionRequest{
					Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, From: entry.State, To: models.ToolEntryError,
					Result: &models.ToolResult{
						Status: models.ToolResultCanceled, IsError: true,
						Content: []models.ContentPart{{Type: models.ContentPartText, Text: "Not run: the request was canceled"}},
					},
				})
				if !errors.Is(err, models.ErrLeaseLost) {
					t.Fatalf("lease-less close of a running run = %v, want ErrLeaseLost", err)
				}
				_, err = sessions.TransitionToolEntry(context.Background(), models.ToolEntryTransitionRequest{
					Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, From: entry.State, To: models.ToolEntryRunning,
				})
				if !errors.Is(err, models.ErrInvalidToolEntryTransition) {
					t.Fatalf("lease-less start = %v, want ErrInvalidToolEntryTransition", err)
				}
			})
		})
	}
}

// seedTerminalCloseToolEntries opens the Run's session and appends one step
// with three tool calls: pending, awaiting approval, and completed. It
// returns the open and the completed entry ids.
func seedTerminalCloseToolEntries(t *testing.T, store terminalCloseTestStore, run *models.AgentRun) ([]string, string) {
	t.Helper()
	ctx := context.Background()
	sessions := store.(models.AgentSessionStore)
	now := time.Now()
	lease := models.AgentRunLeaseGuard{WorkerID: "loop-worker", Now: now}
	opened, err := sessions.CreateRunAgentSession(ctx, models.CreateRunAgentSessionRequest{
		Scope: run.Scope, OwnerRunID: run.ID, AgentID: run.AssignedAgentID, Lease: lease, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := activity.NewAgentTurnService(store, store).BeginTurn(ctx, activity.BeginAgentTurnRequest{
		Scope: run.Scope, RunID: run.ID, SessionID: opened.Session.ID, Producer: models.StepProducerModel,
		WorkerID: "loop-worker", LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	prefix := run.ID + ":"
	assistant := storetest.AssistantSessionEntry(prefix+"assistant", step.ID, "call-1", "call-2", "call-3")
	pending := storetest.ToolSessionEntry(prefix+"tool-1", step.ID, "call-1", models.ToolEntryPending)
	awaiting := storetest.ToolSessionEntry(prefix+"tool-2", step.ID, "call-2", models.ToolEntryPending)
	completed := storetest.ToolSessionEntry(prefix+"tool-3", step.ID, "call-3", models.ToolEntryCompleted)
	if _, err := sessions.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: opened.Session.ID, RunID: run.ID, Lease: &lease, Now: now,
		Entries: []*models.SessionEntry{assistant, pending, awaiting, completed},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.TransitionToolEntry(ctx, models.ToolEntryTransitionRequest{
		Scope: run.Scope, EntryID: awaiting.ID, RunID: run.ID, Lease: lease, Now: now,
		From: models.ToolEntryPending, To: models.ToolEntryAwaitingApproval, Wait: &models.ToolEntryWait{ApprovalID: "approval-1"},
	}); err != nil {
		t.Fatal(err)
	}
	return []string{pending.ID, awaiting.ID}, completed.ID
}

func assertTerminalCloseToolEntries(t *testing.T, sessions models.AgentSessionStore, scope models.Scope, open []string, done, text string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range open {
		entry, err := sessions.GetSessionEntry(ctx, scope, id)
		if err != nil {
			t.Fatal(err)
		}
		result := entry.Tool.Result
		if entry.State != models.ToolEntryError || result == nil || !result.IsError || result.Status != models.ToolResultCanceled ||
			len(result.Content) != 1 || result.Content[0].Text != text || entry.Tool.Wait != nil || entry.Tool.CompletedAt == nil {
			t.Fatalf("entry %s after the run ended = state %s, tool %#v", id, entry.State, entry.Tool)
		}
	}
	entry, err := sessions.GetSessionEntry(ctx, scope, done)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != models.ToolEntryCompleted || entry.Revision != 1 || entry.Tool.Result.Content[0].Text != "done" {
		t.Fatalf("a finished entry was rewritten: %#v", entry)
	}
	left, err := sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{Scope: scope, RunID: entry.RunID, States: models.OpenToolEntryStateValues()})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d tool entries are still open", len(left))
	}
}
