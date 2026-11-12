package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

func TestAgentRunReplayActingUser(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	service := agentrun.NewRunCommandService(store)
	scope := models.Scope{Kind: "tenant", ID: "1"}
	request := func(key, actingUser string) models.CreateAgentRunRequest {
		return models.CreateAgentRunRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
			Goal: "work", Source: models.RunSourceManual, IdempotencyKey: key, ActingUser: actingUser}
	}
	created, err := service.CreateAgentRun(ctx, request("stamped", "7"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Run.ActingUser != "7" {
		t.Fatalf("ActingUser = %q", created.Run.ActingUser)
	}
	if replayed, err := service.CreateAgentRun(ctx, request("stamped", "7")); err != nil || replayed.Run.ID != created.Run.ID {
		t.Fatalf("exact replay = %#v, %v", replayed, err)
	}
	if _, err := service.CreateAgentRun(ctx, request("stamped", "8")); !errors.Is(err, models.ErrRunIdempotency) {
		t.Fatalf("other acting user replay error = %v", err)
	}
	if _, err := service.CreateAgentRun(ctx, request("stamped", "")); !errors.Is(err, models.ErrRunIdempotency) {
		t.Fatalf("missing acting user replay error = %v", err)
	}
	// A run stored before ActingUser existed replays for any caller.
	legacy, err := service.CreateAgentRun(ctx, request("legacy", ""))
	if err != nil {
		t.Fatal(err)
	}
	if replayed, err := service.CreateAgentRun(ctx, request("legacy", "7")); err != nil || replayed.Run.ID != legacy.Run.ID || replayed.Run.ActingUser != "" {
		t.Fatalf("replay with empty stored ActingUser = %#v, %v", replayed, err)
	}
}
