package sqlite

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Store test: the SQLite store's approval page order, seeded with the SQLite
// row fixtures of sqlite_fixture_test.go.

func TestApprovalPaginationUsesIDToBreakTimestampTies(t *testing.T) {
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "local", ID: "review-pagination"}
	now := time.Now().UTC()
	for _, id := range []string{"approval-c", "approval-a", "approval-b"} {
		// The store holds one approval per action call.
		putStoredApproval(t, store, &models.ApprovalCheckpoint{ID: id, Scope: scope, ActionCallID: "call-" + id, Status: models.ApprovalStatusPending, CreatedAt: now})
	}
	for _, newest := range []bool{false, true} {
		for i := 0; i < 20; i++ {
			page, err := store.ListApprovals(t.Context(), models.ApprovalFilter{Scope: scope, Status: []models.ApprovalStatus{models.ApprovalStatusPending}, NewestFirst: newest, Limit: 1, Offset: 1})
			if err != nil || len(page) != 1 || page[0].ID != "approval-b" {
				t.Fatalf("unstable page = %#v, %v", page, err)
			}
			first, err := store.ListApprovals(t.Context(), models.ApprovalFilter{Scope: scope, NewestFirst: newest, Limit: 1})
			want := "approval-a"
			if newest {
				want = "approval-c"
			}
			if err != nil || len(first) != 1 || first[0].ID != want {
				t.Fatalf("wrong first page = %#v, %v", first, err)
			}
		}
	}
}
