package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

// RunActingUserContract checks that store keeps a Run's ActingUser fixed:
// an update that changes or clears it is a revision conflict, and one that
// keeps it goes through. It returns the Run as stored at the end.
func RunActingUserContract(t *testing.T, store models.KernelStore) *models.AgentRun {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "1"}
	run := QueuedAgentRun(scope, "agent", "work", time.Now())
	run.Source, run.ActingUser = models.RunSourceManual, "7"
	if err := store.CreateAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	update := func(actingUser string) error {
		current, err := store.GetAgentRun(ctx, scope, run.ID)
		if err != nil {
			return err
		}
		expected := current.Revision
		current.ActingUser = actingUser
		current.Priority++
		current.Revision++
		current.UpdatedAt = current.UpdatedAt.Add(time.Second)
		event := &models.ActivityEvent{ID: uuid.NewString(), Scope: scope, EventType: "run.updated", Severity: models.ActivitySeverityInfo,
			AgentID: "agent", RunID: run.ID, Actor: models.ActivityActor{Type: "system", ID: "openseal"}, Summary: "Run updated",
			Visibility: models.ActivityVisibilityScope, CreatedAt: current.UpdatedAt}
		_, err = store.UpdateAgentRunWithEvent(ctx, current, expected, event, nil)
		return err
	}
	for _, changed := range []string{"8", ""} {
		if err := update(changed); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("update to ActingUser %q error = %v", changed, err)
		}
	}
	if err := update("7"); err != nil {
		t.Fatalf("update keeping ActingUser: %v", err)
	}
	stored, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil || stored.ActingUser != "7" {
		t.Fatalf("stored run = %#v, %v", stored, err)
	}
	return stored
}
