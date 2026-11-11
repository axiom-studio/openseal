package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

// TestDaemonDispatcherStartsEventRoutineRuns wires event routines as the
// daemon command does (sources before Start, the dispatcher loop after) and
// checks that an event offered to the kernel starts the routine's Run with
// no explicit dispatch pass.
func TestDaemonDispatcherStartsEventRoutineRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	kernel, err := opensealkernel.New(opensealkernel.WithPersistentStore(store))
	if err != nil {
		t.Fatal(err)
	}
	ConfigureRoutineEvents(kernel)
	if err := kernel.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kernel.Stop)
	stop := StartRoutineEventDispatcher(ctx, kernel)
	t.Cleanup(stop)

	seedLocalOperatorSpace(t, store)
	const agentID = "triage"
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: agentID}
	definition, err := kernel.RegisterAgentDefinition(ctx, &opensealkernel.AgentDefinition{
		ID: agentID, Version: "1", DisplayName: "Triage", Purpose: "Triage new issues", SystemPrompt: "Triage the issue.",
		Authority: opensealkernel.AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := kernel.CreateAgentDeployment(ctx, &opensealkernel.AgentDeployment{
		ID: agentID, Scope: capability.ScopeReference{Kind: localScope.Kind, ID: localScope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: opensealkernel.AgentRolloutActive, Environment: "local", Capacity: opensealkernel.AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "local-operator", "daemon routine test"); err != nil {
		t.Fatal(err)
	}
	// A GitHub registration in the daemon's own store is a source the daemon
	// has, so the dispatcher's source check keeps the routine active.
	now := time.Now().UTC()
	if err := store.CreateCallbackRegistration(ctx, &runtime.CallbackRegistration{
		ID: "reg-api", IngressRoute: "route-reg-api", Scope: localScope, Owner: owner, DeploymentID: agentID, Name: "acme/api", Provider: "github",
		Adapter:       runtime.CallbackAdapterReference{SkillID: "github", SkillVersion: "1.0.0", BindingID: "github-binding", BindingRevision: 1, AdapterID: "callbacks"},
		Subscriptions: []runtime.CallbackSubscription{{EventType: "github.issues.opened", Consumer: "routines"}},
		Status:        runtime.CallbackRegistrationActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Lifecycle: []runtime.CallbackRegistrationLifecycleEntry{{Revision: 1, Action: runtime.CallbackRegistrationCreated, Actor: runtime.ActivityActor{Type: "user", ID: "local"}, Reason: "test", At: now}},
	}); err != nil {
		t.Fatal(err)
	}
	conversation, _, err := kernel.CreateConversation(ctx, opensealkernel.CreateConversationRequest{Scope: localScope, Owner: owner, Title: "Issues", IdempotencyKey: "chat:issues"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := kernel.PostChannelMessage(ctx, opensealkernel.PostChannelMessageRequest{
		Scope: localScope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: runtime.ConversationParticipant{Type: opensealkernel.ConversationParticipantUser, ID: "local"}, Intent: opensealkernel.MessageIntentQuestion,
		Content: "Triage every new issue", Audience: runtime.ConversationAudience{Kind: opensealkernel.ConversationAudienceChannel},
		IdempotencyKey: "request:issues",
	})
	if err != nil {
		t.Fatal(err)
	}
	objective, err := kernel.CreateObjective(ctx, opensealkernel.CreateObjectiveRequest{
		Scope: localScope, Owner: owner, Title: "New issues", Goal: "Triage new issues", Status: runtime.ObjectiveStatusActive,
		ExecutionPolicy: &opensealkernel.ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceRef := runtime.RoutineCallbackSourceRef("reg-api")
	activation, err := kernel.CreateRunbookActivation(ctx, opensealkernel.CreateRunbookActivationRequest{
		Scope: localScope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: agentID, TriggerID: "event", MaximumConcurrent: 1,
		Trigger: runbook.Trigger{Kind: runbook.TriggerEvent}, IdempotencyKey: "create-task:issues",
		Task: &opensealkernel.ScheduledAgentTask{
			Goal: "Triage the new issue.", ConversationID: conversation.ID, SourceMessageID: posted.Message.ID, CreatedBy: "local",
			Event: &opensealkernel.RoutineEventTrigger{Kind: opensealkernel.RoutineEventGitHub, SourceRef: sourceRef, SourceLabel: "acme/api (GitHub)", MaxPerHour: 20},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	offer, err := kernel.OfferRoutineEvent(ctx, &opensealkernel.RoutineEvent{
		Scope: localScope, ID: "github:delivery-1", Kind: opensealkernel.RoutineEventGitHub, SourceRef: sourceRef,
		OccurredAt: now, Summary: "Issue #12 opened in acme/api",
	})
	if err != nil || offer != (opensealkernel.RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("offer = %#v err=%v", offer, err)
	}
	for {
		runs, err := kernel.ListAgentRuns(ctx, runtime.AgentRunFilter{Scope: localScope, ObjectiveID: activation.ObjectiveID})
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 {
			if runs[0].Source != runtime.RunSourceEvent || runs[0].Context[opensealkernel.RoutineEventHeaderContextKey] != "Issue #12 opened in acme/api" {
				t.Fatalf("occurrence run = %#v", runs[0])
			}
			break
		}
		if len(runs) > 1 {
			t.Fatalf("runs = %d", len(runs))
		}
		select {
		case <-ctx.Done():
			t.Fatal("the dispatcher never started the routine's Run")
		case <-time.After(10 * time.Millisecond):
		}
	}
	current, err := kernel.GetRunbookActivation(ctx, localScope, activation.ID)
	if err != nil || current.Status != runtime.RunbookActivationActive {
		t.Fatalf("routine after the pass = %#v err=%v", current, err)
	}
}
