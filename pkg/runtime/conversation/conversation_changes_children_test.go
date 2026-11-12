package conversation

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type batchedConversationRunsStore struct {
	models.PortfolioStore
	roots   []*models.AgentRun
	batches [][]string
}

func (s *batchedConversationRunsStore) ListAgentRuns(_ context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	if filter.Kind == models.RunKindConversation {
		roots := s.roots[min(max(filter.Offset, 0), len(s.roots)):]
		if filter.Limit > 0 && filter.Limit < len(roots) {
			roots = roots[:filter.Limit]
		}
		return roots, nil
	}
	s.batches = append(s.batches, append([]string(nil), filter.RootRunIDs...))
	return nil, nil
}

func TestConversationChangeBatchesHistoricalRootLookups(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "batch"}
	store := &batchedConversationRunsStore{}
	for i := 0; i < 501; i++ {
		store.roots = append(store.roots, &models.AgentRun{ID: fmt.Sprintf("root-%03d", i), Scope: scope, ConcurrencyKey: "chat", Status: models.AgentRunStatusCompleted})
	}
	service := &ConversationChangeService{portfolio: store}
	if _, err := service.listConversationRuns(t.Context(), &models.Conversation{ID: "chat", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	if len(store.batches) != 2 || len(store.batches[0]) != 500 || len(store.batches[1]) != 1 {
		t.Fatalf("expected 500+1 root batches, got %v", store.batches)
	}
	seen := map[string]bool{}
	for _, batch := range store.batches {
		for _, id := range batch {
			if seen[id] {
				t.Fatalf("duplicate root %s", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 501 {
		t.Fatalf("lost roots: %d", len(seen))
	}
}

func TestConversationChangeProjectionIncludesScopedDelegatedWork(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store models.KernelStore = newConversationTestStore(t)
			if backend == "sqlite" {
				db, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "children.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store = db
			}
			scope := models.Scope{Kind: "tenant", ID: "one"}
			owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "writer"}
			seedConversationTestSpace(t, store.(models.SpaceStore), scope)
			conversation, _, err := NewConversationService(store.(models.ConversationStore)).CreateConversation(t.Context(), CreateConversationRequest{
				Scope: scope, Owner: owner, Title: "Draft report", IdempotencyKey: "report-chat",
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			root := &models.AgentRun{ID: "root", Scope: scope, Kind: models.RunKindConversation, Owner: owner, RootRunID: "root", ConcurrencyKey: "chat", Status: models.AgentRunStatusCompleted, Revision: 1, CreatedAt: now, UpdatedAt: now}
			child := &models.AgentRun{ID: "child", Scope: scope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}, RootRunID: "root", ParentRunID: "root", Status: models.AgentRunStatusRunning, Revision: 1, CreatedAt: now, UpdatedAt: now}
			foreign := *child
			foreign.ID = "foreign"
			foreign.Scope.ID = "other"
			unrelated := *child
			unrelated.ID = "unrelated"
			unrelated.RootRunID = "different-root"
			for _, run := range []*models.AgentRun{root, child, &foreign, &unrelated} {
				if run.ID == root.ID {
					run.ConcurrencyKey = conversation.ID
				}
				run.Goal = "Prepare a report"
				run.Source = models.RunSourceChat
				run.AvailableAt = now
				run.QueueEnteredAt = now
				if err := store.CreateAgentRun(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}
			for _, check := range []struct {
				roots []string
				exact string
				want  int
			}{
				{[]string{"root"}, "", 1},
				{[]string{"root", "different-root"}, "", 2},
				{[]string{"missing"}, "", 0},
				{[]string{"root"}, "different-root", 0},
			} {
				got, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, Kind: models.RunKindAgentWork, RootRunIDs: check.roots, RootRunID: check.exact})
				if err != nil || len(got) != check.want {
					t.Fatalf("root batch %v exact %q: got %d, want %d, error %v", check.roots, check.exact, len(got), check.want, err)
				}
			}
			service, err := NewConversationChangeService(store.(models.ConversationStore), store)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := service.listConversationRuns(t.Context(), conversation)
			if err != nil {
				t.Fatal(err)
			}
			if len(projection) != 2 || projection[0].ID != "child" || projection[1].ID != "root" {
				t.Fatalf("wrong projection: %#v", projection)
			}
			before, err := conversationRunProjectionDigest(projection)
			if err != nil {
				t.Fatal(err)
			}
			initial, err := service.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID})
			if err != nil {
				t.Fatal(err)
			}
			unchanged, err := service.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor})
			if err != nil {
				t.Fatal(err)
			}
			if unchanged.HasChanges || unchanged.RunsChanged || len(unchanged.Runs) != 0 || unchanged.Cursor != initial.Cursor {
				t.Fatal("unchanged tree was resent")
			}
			_, _, err = activity.NewRunActivityService(store, store).TransitionRun(t.Context(), scope, child.ID, activity.RunTransitionRequest{ExpectedRevision: 1, Status: models.AgentRunStatusFailed, Error: "task failed"})
			if err != nil {
				t.Fatal(err)
			}
			projection, err = service.listConversationRuns(t.Context(), conversation)
			if err != nil {
				t.Fatal(err)
			}
			after, err := conversationRunProjectionDigest(projection)
			if err != nil {
				t.Fatal(err)
			}
			if before == after || projection[0].Status != models.AgentRunStatusFailed {
				t.Fatal("child failure did not change the projection")
			}
			changed, err := service.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor})
			if err != nil {
				t.Fatal(err)
			}
			if !changed.HasChanges || !changed.RunsChanged || changed.Cursor == initial.Cursor || len(changed.Runs) != 2 || changed.Runs[0].Status != models.AgentRunStatusFailed {
				t.Fatal("child failure did not advance the public change feed")
			}
		})
	}
}
