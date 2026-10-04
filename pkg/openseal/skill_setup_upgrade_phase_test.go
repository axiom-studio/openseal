package openseal

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestSkillSetupUpgradePhaseRequiresCanonicalDrainThenSeparateSave(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			var store interface {
				runtime.KernelStore
				runtime.SkillSetupRequestStore
			} = runtime.NewMemoryStore()
			path := filepath.Join(t.TempDir(), "setup.db")
			if kind == "sqlite" {
				sqlite, err := runtime.NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				store = sqlite
				t.Cleanup(func() { _ = sqlite.Close() })
			}
			engine, err := New(WithStore(store))
			if err != nil {
				t.Fatal(err)
			}
			for _, version := range []string{"1.0.0", "1.1.0"} {
				if err := engine.RegisterSkill(ctx, &SkillDefinition{ID: "connector", Version: version, Name: "Connector", Transport: SkillTransportReference{Kind: "local"},
					Actions: map[string]SkillAction{"read": {Name: "read", Description: "Read the connected service", Risk: SkillRiskRead, SideEffect: SkillSideEffectRead, Idempotency: SkillIdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}}}); err != nil {
					t.Fatal(err)
				}
			}
			scope := Scope{Kind: "tenant", ID: "one"}
			skillScope := SkillScope{Kind: scope.Kind, ID: scope.ID}
			binding, err := engine.UpsertSkillBinding(ctx, UpsertSkillBindingRequest{Binding: &SkillBinding{ID: "account", Scope: skillScope, DeploymentID: "agent", SkillID: "connector", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: SkillRiskRead}, Actor: SkillBindingActor{Type: "user", ID: "operator"}, Reason: "Connect account"})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := engine.CreateConversation(ctx, CreateConversationRequest{ID: "chat", Scope: scope, Owner: ObjectiveOwner{Type: "agent", ID: "agent"}, Title: "Setup", IdempotencyKey: "setup"}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			request := &SkillSetupRequest{ID: "setup", Scope: scope, DeploymentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: "requesting-run", ActionCallID: "setup-call",
				Kind: "configure", SkillID: "connector", SkillVersion: "1.1.0", SkillName: "Connector", BindingID: binding.ID, BindingRevision: binding.Revision,
				Phase: SkillSetupPhaseBindingUpgrade, BindingVersion: "1.0.0", RequiredActions: []string{"read"}, Reason: "Connect the current service", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
			if err := store.SaveSkillSetupRequest(ctx, request, 0); err != nil {
				t.Fatal(err)
			}
			objective, err := engine.CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: ObjectiveOwner{Type: "agent", ID: "agent"}, Title: "Old work", Goal: "Finish accepted work", Status: ObjectiveStatusActive, Priority: 1, Actor: ActivityActor{Type: "user", ID: "operator"}, Visibility: ActivityVisibilityScope})
			if err != nil {
				t.Fatal(err)
			}
			active, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindAgentWork, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: "agent", Goal: "Accepted old-version work", Source: RunSourceManual,
				Context: map[string]interface{}{"capabilityInvocation": map[string]interface{}{"skillId": "connector", "skillVersion": "1.0.0", "action": "read"}}, Actor: ActivityActor{Type: "user", ID: "operator"}, Visibility: ActivityVisibilityScope})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := engine.PlanSkillReferenceUpgrade(ctx, PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: binding.ID, ToVersion: "1.1.0"})
			if err != nil {
				t.Fatal(err)
			}
			apply := ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "Adopt the reviewed current version"}
			if _, err := engine.ApplySkillReferenceUpgrade(ctx, apply); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
				t.Fatalf("old accepted work was bypassed: %v", err)
			}
			before, _ := engine.GetSkillBinding(ctx, skillScope, "agent", binding.ID)
			if !reflect.DeepEqual(before, binding) {
				t.Fatal("blocked upgrade changed the account")
			}
			if _, err := engine.CommandAgentRun(ctx, AgentRunCommandRequest{Scope: scope, RunID: active.ID, ExpectedRevision: active.Revision, Kind: AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: "operator"}, Summary: "Finish old work before upgrade", Visibility: ActivityVisibilityScope}); err != nil {
				t.Fatal(err)
			}
			receipt, err := engine.ApplySkillReferenceUpgrade(ctx, apply)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "sqlite" {
				// A restarted catalog must load the persisted canonical provenance.
				reopened, err := runtime.NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reopened.Close() })
				store = reopened
				engine, err = New(WithStore(store))
				if err != nil {
					t.Fatal(err)
				}
			}
			current, err := engine.GetSkillBinding(ctx, skillScope, "agent", binding.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Revision != receipt.BindingRevision || current.Lifecycle[len(current.Lifecycle)-1].SkillUpgrade == nil || current.Lifecycle[len(current.Lifecycle)-1].SkillUpgrade.PlanDigest != plan.Digest {
				t.Fatal("canonical upgrade proof did not persist")
			}
			requests, err := engine.ListSkillSetupRequests(ctx, scope, "agent", "chat")
			if err != nil || len(requests) != 1 || requests[0].Status != "pending" || requests[0].Phase != SkillSetupPhaseBindingUpgrade {
				t.Fatalf("upgrade alone resolved setup: %#v %v", requests, err)
			}
			if _, err := engine.ResolveSkillSetupRequest(ctx, scope, "agent", request.ID, 1, binding.ID, "operator", false); err == nil {
				t.Fatal("explicit resolve bypassed upgrade phase")
			}
			rebased, err := RebaseSkillSetupRequestAfterBindingUpgrade(requests[0], 1, current)
			if err != nil {
				t.Fatalf("rebase: %v request=%#v binding=%#v lifecycle=%#v", err, requests[0], current, current.Lifecycle)
			}
			if err := store.SaveSkillSetupRequest(ctx, rebased, 1); err != nil {
				t.Fatalf("rebase: %v request=%#v binding=%#v lifecycle=%#v", err, requests[0], current, current.Lifecycle)
			}
			if err := store.SaveSkillSetupRequest(ctx, rebased, 1); !errors.Is(err, ErrSkillSetupConflict) {
				t.Fatalf("stale rebase did not conflict: %v", err)
			}
			requests, err = engine.ListSkillSetupRequests(ctx, scope, "agent", "chat")
			if err != nil || requests[0].Status != "pending" {
				t.Fatalf("rebase itself resolved setup: %#v %v", requests, err)
			}
			if _, err := engine.ResolveSkillSetupRequest(ctx, scope, "agent", request.ID, rebased.Revision, binding.ID, "operator", false); err == nil {
				t.Fatal("upgrade revision counted as actual configuration")
			}
			saved, err := engine.UpsertSkillBinding(ctx, UpsertSkillBindingRequest{Binding: current, ExpectedRevision: current.Revision, Actor: SkillBindingActor{Type: "user", ID: "operator"}, Reason: "Save the reviewed account configuration"})
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := engine.ResolveSkillSetupRequest(ctx, scope, "agent", request.ID, rebased.Revision, binding.ID, "operator", false)
			if err != nil || resolved.Status != "resolved" || resolved.ResolvedBindingRevision != saved.Revision {
				t.Fatalf("actual save did not resolve: %#v %v", resolved, err)
			}
		})
	}
}
