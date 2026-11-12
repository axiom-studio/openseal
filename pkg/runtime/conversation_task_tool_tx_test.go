package runtime

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

type conversationTaskToolTestStore interface {
	conversationTaskSQLTestStore
	models.AgentSessionStore
}

// forConversationTaskToolStores runs test on SQLite and (with
// OPENSEAL_TEST_POSTGRES_DSN) Postgres.
func forConversationTaskToolStores(t *testing.T, test func(*testing.T, conversationTaskToolTestStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "conversation-task-tool.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		test(t, store)
	})
	t.Run("postgres", func(t *testing.T) {
		pgSchema := storetest.NewPostgresSchema(t, "openseal_task_tool_")
		dsn := pgSchema.DSN
		store, err := postgres.NewPostgresStore(t.Context(), dsn, postgres.WithPostgresSchema(pgSchema.Name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = store.Close()
		})
		test(t, store)
	})
}

type conversationTaskToolFixture struct {
	record    models.ConversationTaskCreateRecord
	session   *models.AgentSession
	user      *models.SessionEntry
	assistant *models.SessionEntry
	tool      *models.SessionEntry
}

// conversationTaskToolRecord is the SQL fixture record bound to a
// conversation_start_task tool entry: the source Run has a session holding
// [user, assistant, tool(pending)] for its step 1.
func conversationTaskToolRecord(t *testing.T, store conversationTaskToolTestStore, suffix string) conversationTaskToolFixture {
	t.Helper()
	base := conversationTaskSQLRecord(t, store, suffix)
	task := base.Task
	now := time.Now().UTC()
	lease := models.AgentRunLeaseGuard{WorkerID: base.WorkerID, Now: now}
	opened, err := store.CreateRunAgentSession(t.Context(), models.CreateRunAgentSessionRequest{
		Scope: task.Scope, OwnerRunID: task.SourceRunID, AgentID: task.TargetAgentID, ConversationID: task.ConversationID,
		Lease: lease, Now: now,
	})
	if err != nil {
		t.Fatalf("create source session: %v", err)
	}
	sessionID := opened.Session.ID
	args, _ := json.Marshal(map[string]string{"goal": task.Goal, "acknowledgment": task.Acknowledgment})
	callID := "call-" + suffix
	user := &models.SessionEntry{ID: models.SessionEntryID(sessionID, "message:m-"+suffix), RunID: task.SourceRunID, Type: models.SessionEntryUser,
		SourceKey: "message:m-" + suffix, User: &models.UserEntry{Origin: models.UserEntryChannelMessage, Author: models.SessionAuthor{Type: "user", ID: "requester"},
			Content: []models.ContentPart{storetest.TextPart("Please prepare the result.")}, MessageID: "m-" + suffix}}
	assistant := &models.SessionEntry{ID: models.SessionEntryID(sessionID, models.AssistantEntryKey(task.SourceTurnID)), RunID: task.SourceRunID,
		StepID: task.SourceTurnID, Step: task.SourceTurnNumber, Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
			Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopToolCalls,
			ToolCalls: []models.ModelToolCall{{ID: callID, Name: string(models.ControlToolStartTask), Arguments: args}}}}
	tool := &models.SessionEntry{ID: models.SessionEntryID(sessionID, models.ToolEntryKey(task.SourceTurnID, callID)), RunID: task.SourceRunID,
		StepID: task.SourceTurnID, Step: task.SourceTurnNumber, Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: callID,
		Tool: &models.ToolEntry{Name: string(models.ControlToolStartTask), Arguments: args, SideEffect: models.ToolSideEffectWrite}}
	for _, batch := range [][]*models.SessionEntry{{user}, {assistant, tool}} {
		if _, err := store.AppendAgentSessionEntries(t.Context(), models.AppendAgentSessionEntriesRequest{Scope: task.Scope, SessionID: sessionID,
			RunID: task.SourceRunID, Lease: &lease, Entries: batch, Now: now}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	fixture := conversationTaskToolFixture{session: opened.Session}
	for _, entry := range []**models.SessionEntry{&user, &assistant, &tool} {
		stored, err := store.GetSessionEntry(t.Context(), task.Scope, (*entry).ID)
		if err != nil {
			t.Fatal(err)
		}
		*entry = stored
	}
	fixture.user, fixture.assistant, fixture.tool = user, assistant, tool

	task.SourceToolEntryID = tool.ID
	if task.RequestDigest, err = models.ConversationTaskDigest(task); err != nil {
		t.Fatal(err)
	}
	base.WorkRun.SessionID = models.RunAgentSessionID(task.Scope, task.WorkRunID)
	base.ToolTransition = &models.ToolEntryTransitionRequest{Scope: task.Scope, EntryID: tool.ID, RunID: task.SourceRunID, Lease: lease,
		From: models.ToolEntryPending, To: models.ToolEntryCompleted, Result: models.ConversationTaskStartedToolResult(task), Now: now}
	base.WorkSession = &models.CreateRunAgentSessionTxRequest{Scope: task.Scope, OwnerRunID: task.WorkRunID, AgentID: task.TargetAgentID,
		ConversationID: task.ConversationID, ParentSessionID: sessionID, ForkSequence: assistant.Sequence - 1, Now: now}
	fixture.record = rebaseConversationTaskSource(t, store, base)
	return fixture
}

// rebaseConversationTaskSource rebuilds the completed source of record from
// the stored source Run (its session binding moved the revision).
func rebaseConversationTaskSource(t *testing.T, store conversationTaskToolTestStore, record models.ConversationTaskCreateRecord) models.ConversationTaskCreateRecord {
	t.Helper()
	current, err := store.GetAgentRun(t.Context(), record.Task.Scope, record.Task.SourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	updated := models.CloneAgentRun(current)
	if err := models.AddRunBudgetAllocation(updated, record.WorkRun.ID, record.WorkRun.Budget); err != nil {
		t.Fatal(err)
	}
	updated.Status = models.AgentRunStatusCompleted
	updated.Context[models.RunReportingRootRunIDContextKey] = current.ID
	updated.Context[models.RunReportingMilestonesContextKey] = []interface{}{"completed", "failed"}
	updated.Output = map[string]interface{}{"summary": record.Task.Acknowledgment, "conversationTaskIds": []string{record.Task.ID},
		"conversationTaskWorkRunId": record.WorkRun.ID}
	updated.CompletedAt = record.SourceRun.CompletedAt
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Revision = current.Revision + 1
	record.SourceRun = updated
	record.ExpectedSourceRevision = current.Revision
	if err := models.ValidateConversationTaskCreateRecord(record); err != nil {
		t.Fatalf("invalid tool fixture: %v", err)
	}
	return record
}

// assertConversationTaskNotAdmitted checks that a refused admission wrote
// nothing: no task, no work Run or session, source running, entry pending.
func assertConversationTaskNotAdmitted(t *testing.T, store conversationTaskToolTestStore, fixture conversationTaskToolFixture) {
	t.Helper()
	task := fixture.record.Task
	if got, err := store.GetConversationTask(t.Context(), task.Scope, task.ID); err != nil || got != nil {
		t.Fatalf("task after refusal = %#v, %v", got, err)
	}
	if work, err := store.GetAgentRun(t.Context(), task.Scope, task.WorkRunID); err == nil && work != nil {
		t.Fatalf("work run after refusal = %#v", work)
	}
	if session, err := store.GetAgentSession(t.Context(), task.Scope, models.RunAgentSessionID(task.Scope, task.WorkRunID)); !errors.Is(err, models.ErrAgentSessionNotFound) {
		t.Fatalf("work session after refusal = %#v, %v", session, err)
	}
	source, err := store.GetAgentRun(t.Context(), task.Scope, task.SourceRunID)
	if err != nil || source.Status != models.AgentRunStatusRunning || source.LeaseOwner != fixture.record.WorkerID {
		t.Fatalf("source after refusal = %#v, %v", source, err)
	}
	entry, err := store.GetSessionEntry(t.Context(), task.Scope, fixture.tool.ID)
	if err != nil || entry.Tool.Result != nil || entry.State == models.ToolEntryCompleted || entry.Revision != fixture.tool.Revision {
		t.Fatalf("tool entry after refusal = %#v, %v", entry, err)
	}
}

func TestStartTaskToolEntryCompletedInSameTransaction(t *testing.T) {
	forConversationTaskToolStores(t, func(t *testing.T, store conversationTaskToolTestStore) {
		fixture := conversationTaskToolRecord(t, store, "tool")
		record, task := fixture.record, fixture.record.Task

		created, err := store.CreateConversationTask(t.Context(), record)
		if err != nil || created.Replayed {
			t.Fatalf("create = %#v, %v", created, err)
		}
		entry, err := store.GetSessionEntry(t.Context(), task.Scope, fixture.tool.ID)
		if err != nil {
			t.Fatal(err)
		}
		if entry.State != models.ToolEntryCompleted || !models.SameToolResult(entry.Tool.Result, models.ConversationTaskStartedToolResult(task)) ||
			entry.Revision != fixture.tool.Revision+1 {
			t.Fatalf("tool entry = %#v", entry)
		}
		var result map[string]string
		if err := json.Unmarshal(entry.Tool.Result.Content[0].Value, &result); err != nil || result["status"] != "started" ||
			result["taskId"] != task.ID || result["workRunId"] != task.WorkRunID || result["acknowledgment"] != task.Acknowledgment {
			t.Fatalf("tool result = %s, %v", entry.Tool.Result.Content[0].Value, err)
		}
		stored, err := store.GetConversationTask(t.Context(), task.Scope, task.ID)
		if err != nil || stored == nil || stored.SourceToolEntryID != fixture.tool.ID {
			t.Fatalf("task = %#v, %v", stored, err)
		}

		// The work Run's session forks the chat session just before the
		// step's assistant entry: it sees the user message, not the call.
		work, err := store.GetAgentRun(t.Context(), task.Scope, task.WorkRunID)
		if err != nil || work.SessionID != models.RunAgentSessionID(task.Scope, task.WorkRunID) || work.Revision != 1 {
			t.Fatalf("work run = %#v, %v", work, err)
		}
		session, err := store.GetAgentSession(t.Context(), task.Scope, work.SessionID)
		if err != nil || session.Kind != models.AgentSessionKindRun || session.OwnerRunID != work.ID || session.AgentID != task.TargetAgentID ||
			session.ParentSessionID != fixture.session.ID || session.ForkSequence != fixture.assistant.Sequence-1 ||
			session.ForkEntryID != fixture.user.ID || session.ConversationID != task.ConversationID {
			t.Fatalf("work session = %#v, %v", session, err)
		}
		path, err := store.LoadAgentSessionPath(t.Context(), task.Scope, session.ID)
		if err != nil || len(path.Entries) != 1 || path.Entries[0].ID != fixture.user.ID {
			t.Fatalf("work session path = %#v, %v", path, err)
		}

		// Replay with the same record writes nothing and keeps the entry.
		replayed, err := store.CreateConversationTask(t.Context(), record)
		if err != nil || !replayed.Replayed {
			t.Fatalf("replay = %#v, %v", replayed, err)
		}
		again, err := store.GetSessionEntry(t.Context(), task.Scope, fixture.tool.ID)
		if err != nil || again.Revision != entry.Revision || again.State != models.ToolEntryCompleted {
			t.Fatalf("entry after replay = %#v, %v", again, err)
		}
		// A replay claiming another result is a conflict.
		other := record
		otherTransition := *record.ToolTransition
		otherTransition.Result = models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{"status": "started", "taskId": "other"})
		other.ToolTransition = &otherTransition
		if err := models.CheckConversationTaskToolReplay(other, again); !errors.Is(err, models.ErrConversationTaskConflict) {
			t.Fatalf("replay with other result = %v", err)
		}
	})
}

func TestStartTaskToolEntryRefusalWritesNothing(t *testing.T) {
	forConversationTaskToolStores(t, func(t *testing.T, store conversationTaskToolTestStore) {
		t.Run("entry already running", func(t *testing.T) {
			fixture := conversationTaskToolRecord(t, store, "running")
			lease := fixture.record.ToolTransition.Lease
			moved, err := store.TransitionToolEntry(t.Context(), models.ToolEntryTransitionRequest{Scope: fixture.record.Task.Scope,
				EntryID: fixture.tool.ID, RunID: fixture.tool.RunID, Lease: lease, From: models.ToolEntryPending, To: models.ToolEntryRunning, Now: lease.Now})
			if err != nil {
				t.Fatal(err)
			}
			fixture.tool = moved.Entry
			if _, err := store.CreateConversationTask(t.Context(), fixture.record); !errors.Is(err, models.ErrToolEntryStateConflict) {
				t.Fatalf("create from pending on a running entry = %v", err)
			}
			assertConversationTaskNotAdmitted(t, store, fixture)
			if entry, _ := store.GetSessionEntry(t.Context(), fixture.record.Task.Scope, fixture.tool.ID); entry.State != models.ToolEntryRunning {
				t.Fatalf("entry = %#v", entry)
			}
			// From running is accepted.
			transition := *fixture.record.ToolTransition
			transition.From = models.ToolEntryRunning
			fixture.record.ToolTransition = &transition
			if _, err := store.CreateConversationTask(t.Context(), fixture.record); err != nil {
				t.Fatalf("create from running = %v", err)
			}
		})
		t.Run("fork after the assistant entry", func(t *testing.T) {
			fixture := conversationTaskToolRecord(t, store, "fork")
			session := *fixture.record.WorkSession
			session.ForkSequence = fixture.assistant.Sequence
			fixture.record.WorkSession = &session
			if _, err := store.CreateConversationTask(t.Context(), fixture.record); !errors.Is(err, models.ErrConversationTaskConflict) {
				t.Fatalf("create with fork at the assistant entry = %v", err)
			}
			assertConversationTaskNotAdmitted(t, store, fixture)
		})
		t.Run("entry of another step", func(t *testing.T) {
			fixture := conversationTaskToolRecord(t, store, "step")
			task := *fixture.record.Task
			task.SourceTurnNumber = 2
			fixture.record.Task = &task
			current, _ := store.GetAgentRun(t.Context(), task.Scope, task.SourceRunID)
			current.LastAppliedTurn = 2
			current.UpdatedAt = time.Now().UTC()
			current.Revision++
			updateConversationTaskSQLRun(t, store, current, current.Revision-1)
			var err error
			if task.RequestDigest, err = models.ConversationTaskDigest(&task); err != nil {
				t.Fatal(err)
			}
			fixture.record = rebaseConversationTaskSource(t, store, fixture.record)
			if _, err := store.CreateConversationTask(t.Context(), fixture.record); !errors.Is(err, models.ErrConversationTaskConflict) {
				t.Fatalf("create for another step = %v", err)
			}
			assertConversationTaskNotAdmitted(t, store, fixture)
		})
	})
}

func TestStartTaskCompletesSourceAtomically(t *testing.T) {
	forConversationTaskToolStores(t, func(t *testing.T, store conversationTaskToolTestStore) {
		t.Run("completed with the acknowledgment", func(t *testing.T) {
			fixture := conversationTaskToolRecord(t, store, "source")
			task := fixture.record.Task
			before, err := store.GetAgentRun(t.Context(), task.Scope, task.SourceRunID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateConversationTask(t.Context(), fixture.record); err != nil {
				t.Fatal(err)
			}
			source, err := store.GetAgentRun(t.Context(), task.Scope, task.SourceRunID)
			if err != nil {
				t.Fatal(err)
			}
			if source.Status != models.AgentRunStatusCompleted || source.Output["summary"] != task.Acknowledgment ||
				source.Output["conversationTaskWorkRunId"] != task.WorkRunID || source.LastAppliedTurn != task.SourceTurnNumber ||
				source.LeaseOwner != "" || source.LeaseExpiresAt != nil || source.Revision != before.Revision+1 ||
				source.SessionID != fixture.session.ID {
				t.Fatalf("source = %#v", source)
			}
			entry, err := store.GetSessionEntry(t.Context(), task.Scope, fixture.tool.ID)
			if err != nil || entry.State != models.ToolEntryCompleted {
				t.Fatalf("entry = %#v, %v", entry, err)
			}
		})
		t.Run("steering arrived", func(t *testing.T) {
			fixture := conversationTaskToolRecord(t, store, "steer")
			task := fixture.record.Task
			current, err := store.GetAgentRun(t.Context(), task.Scope, task.SourceRunID)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			current.PendingInterventions = append(current.PendingInterventions, models.AgentRunIntervention{ID: "steer-1",
				Actor: models.ActivityActor{Type: "user", ID: "requester"}, Instruction: "Actually, make it shorter.", CreatedAt: now})
			current.UpdatedAt = now
			current.Revision++
			updateConversationTaskSQLRun(t, store, current, current.Revision-1)
			fixture.record = rebaseConversationTaskSource(t, store, fixture.record)
			if _, err := store.CreateConversationTask(t.Context(), fixture.record); !errors.Is(err, models.ErrSteeringArrived) {
				t.Fatalf("create after steering = %v", err)
			}
			assertConversationTaskNotAdmitted(t, store, fixture)
		})
		t.Run("source moved", func(t *testing.T) {
			fixture := conversationTaskToolRecord(t, store, "moved")
			fixture.record.ExpectedSourceRevision--
			fixture.record.SourceRun.Revision--
			if _, err := store.CreateConversationTask(t.Context(), fixture.record); !errors.Is(err, models.ErrRevisionConflict) {
				t.Fatalf("create with a stale source revision = %v", err)
			}
			assertConversationTaskNotAdmitted(t, store, fixture)
		})
	})
}

func TestConversationTaskToolRecordValidation(t *testing.T) {
	fixture := conversationTaskToolRecord(t, newTestKernelStore(t), "validate")
	mutate := map[string]func(*models.ConversationTaskCreateRecord){
		"transition without entry id": func(r *models.ConversationTaskCreateRecord) {
			task := *r.Task
			task.SourceToolEntryID = ""
			r.Task = &task
		},
		"entry id without transition": func(r *models.ConversationTaskCreateRecord) { r.ToolTransition = nil },
		"no work session":             func(r *models.ConversationTaskCreateRecord) { r.WorkSession = nil },
		"other result": func(r *models.ConversationTaskCreateRecord) {
			transition := *r.ToolTransition
			transition.Result = models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{"status": "started"})
			r.ToolTransition = &transition
		},
		"to error": func(r *models.ConversationTaskCreateRecord) {
			transition := *r.ToolTransition
			transition.To = models.ToolEntryError
			r.ToolTransition = &transition
		},
		"other worker": func(r *models.ConversationTaskCreateRecord) {
			transition := *r.ToolTransition
			transition.Lease.WorkerID = "other-worker"
			r.ToolTransition = &transition
		},
		"session of another parent": func(r *models.ConversationTaskCreateRecord) {
			session := *r.WorkSession
			session.ParentSessionID = "other-session"
			r.WorkSession = &session
		},
		"work run without session id": func(r *models.ConversationTaskCreateRecord) {
			work := models.CloneAgentRun(r.WorkRun)
			work.SessionID = ""
			r.WorkRun = work
		},
	}
	for name, change := range mutate {
		record := fixture.record
		change(&record)
		if err := models.ValidateConversationTaskCreateRecord(record); err == nil {
			t.Errorf("%s: record accepted", name)
		}
	}
}
