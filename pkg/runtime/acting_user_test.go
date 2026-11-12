package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestAgentRunReplayActingUser(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	service := NewRunCommandService(store)
	scope := Scope{Kind: "tenant", ID: "1"}
	request := func(key, actingUser string) CreateAgentRunRequest {
		return CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
			Goal: "work", Source: RunSourceManual, IdempotencyKey: key, ActingUser: actingUser}
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
	if _, err := service.CreateAgentRun(ctx, request("stamped", "8")); !errors.Is(err, ErrRunIdempotency) {
		t.Fatalf("other acting user replay error = %v", err)
	}
	if _, err := service.CreateAgentRun(ctx, request("stamped", "")); !errors.Is(err, ErrRunIdempotency) {
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
