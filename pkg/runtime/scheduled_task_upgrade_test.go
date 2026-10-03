package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func scheduledTaskUpgradeFixture(t *testing.T, store KernelStore, previous, target *skill.Definition) (*AgentRun, *skill.Catalog, *skill.Binding, map[string]interface{}) {
	t.Helper()
	run, _, bound, args := scheduledTaskFixture(t, store)
	catalog := skill.NewCatalogWithStore(store.(skill.CatalogStore))
	for _, definition := range []*skill.Definition{previous, target} {
		if err := catalog.Register(t.Context(), definition); err != nil {
			t.Fatal(err)
		}
	}
	binding := cloneUpgradeBinding(bound.Binding)
	binding.SkillID, binding.SkillVersion, binding.SourceIdentity = previous.ID, previous.Version, skill.DefinitionSourceIdentity(previous)
	binding.AllowedActions = []string{RunbookActionCreateTask, RunbookActionList}
	binding.Credentials = map[string]skill.CredentialReference{"opaque": {Kind: "test", ID: "saved-grant"}}
	binding.ArgumentRestrictions = map[string]map[string]skill.ArgumentRule{RunbookActionCreateTask: {"timezone": {Const: "Asia/Kolkata"}}}
	for revision := int64(1); revision <= 7; revision++ {
		binding.Revision = revision
		if err := catalog.Bind(t.Context(), binding); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := catalog.GetBinding(t.Context(), binding.Scope, binding.DeploymentID, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run, catalog, stored, args
}

func TestScheduledTaskNullableContractUpgradeUsesCanonicalPlanAndApplyAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		for _, previous := range []*skill.Definition{RunbookManagementSkillLegacy130(), RunbookManagementSkillLegacy131()} {
			t.Run(kind+"/"+previous.Version, func(t *testing.T) {
				store := workflowEvidenceStore(t, kind)
				run, catalog, before, args := scheduledTaskUpgradeFixture(t, store, previous, RunbookManagementSkill())
				service := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
				plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: run.Scope, DeploymentID: run.AssignedAgentID, BindingID: before.ID, ToVersion: "1.3.2"})
				if err != nil || plan.ApprovalRequired || len(plan.Findings) != 0 || plan.ExpectedBindingRevision != 7 {
					t.Fatalf("audited nullable contract plan=%#v, %v", plan, err)
				}
				if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Reason: "Missing actor"}); !errors.Is(err, ErrSkillReferenceUpgradeInvalid) {
					t.Fatalf("upgrade accepted missing actor: %v", err)
				}
				foreign := *plan
				foreign.Scope.ID = "foreign"
				if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: &foreign, Actor: ActivityActor{Type: "system", ID: "canonical-skill-reconciler"}, Reason: "Attempt foreign scope"}); err == nil {
					t.Fatal("upgrade accepted foreign scope")
				}
				unreviewed := *plan
				unreviewed.Digest = "sha256:unreviewed"
				if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: &unreviewed, Actor: ActivityActor{Type: "system", ID: "canonical-skill-reconciler"}, Reason: "Attempt unreviewed plan"}); !errors.Is(err, ErrSkillReferenceUpgradeConflict) {
					t.Fatalf("upgrade accepted unreviewed plan: %v", err)
				}
				receipt, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "canonical-skill-reconciler"}, Reason: "Follow the compatible bundled routine contract"})
				if err != nil || receipt.BindingRevision != 8 {
					t.Fatalf("atomic nullable contract upgrade=%#v, %v", receipt, err)
				}
				after, err := catalog.GetBinding(t.Context(), before.Scope, before.DeploymentID, before.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(after.AllowedActions, before.AllowedActions) || after.MaximumRisk != before.MaximumRisk || after.Disabled != before.Disabled ||
					!reflect.DeepEqual(after.Credentials, before.Credentials) || !reflect.DeepEqual(after.Config, before.Config) || !reflect.DeepEqual(after.ArgumentRestrictions, before.ArgumentRestrictions) {
					t.Fatalf("upgrade changed reviewed authority: before=%#v after=%#v", before, after)
				}
				bound, err := catalog.Resolve(t.Context(), before.Scope, before.DeploymentID, before.SkillID, "1.3.2", RunbookActionCreateTask, skill.BindingReference{ID: before.ID, Revision: 8})
				if err != nil {
					t.Fatal(err)
				}
				args["maximumOccurrences"] = nil
				if err := catalog.ValidateInput(t.Context(), bound, args); err != nil {
					t.Fatalf("upgraded binding still rejects ongoing task: %v", err)
				}
				if _, err := catalog.Resolve(t.Context(), before.Scope, before.DeploymentID, before.SkillID, "1.3.2", RunbookActionReplaceSchedule); err == nil {
					t.Fatal("upgrade granted a previously prohibited action")
				}
				unauthorized := cloneMap(args)
				unauthorized["timezone"] = "UTC"
				if err := catalog.ValidateInput(t.Context(), bound, unauthorized); err == nil {
					t.Fatal("upgrade discarded reviewed argument restrictions")
				}
				if _, err := catalog.Resolve(t.Context(), before.Scope, before.DeploymentID, before.SkillID, "1.3.2", RunbookActionCreateTask, skill.BindingReference{ID: before.ID, Revision: 7}); err == nil {
					t.Fatal("upgrade accepted the stale binding revision")
				}
				preserved, err := catalog.GetDefinition(t.Context(), previous.ID, previous.Version)
				if err != nil || !compatibleRunbookOccurrenceLimitUpgrade(preserved, bound.Definition) {
					t.Fatalf("published definitions changed: %#v, %v", preserved, err)
				}
			})
		}
	}
}

func TestScheduledTaskNullableContractUpgradeDrainsAcceptedLegacyCalls(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := workflowEvidenceStore(t, kind)
			run, catalog, before, args := scheduledTaskUpgradeFixture(t, store, RunbookManagementSkillLegacy131(), RunbookManagementSkill())
			now := time.Now().Add(time.Second)
			claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, WorkerID: "chat-worker", AssignedAgentID: run.AssignedAgentID, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil {
				t.Fatalf("claim=%#v, %v", claimed, err)
			}
			validator, _ := NewRunbookActionValidator(store.(runbookActionStore))
			coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
				return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
			}), validator)
			coordinator.now = func() time.Time { return now }
			proposal, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "chat-worker", DeploymentID: run.AssignedAgentID, BindingID: before.ID, BindingRevision: before.Revision, SkillID: before.SkillID, SkillVersion: before.SkillVersion, Action: RunbookActionCreateTask, Arguments: args, IdempotencyKey: "legacy-bounded-task", Summary: "Create the approved bounded task"})
			if err != nil {
				t.Fatal(err)
			}
			service := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
			plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: run.Scope, DeploymentID: run.AssignedAgentID, BindingID: before.ID, ToVersion: "1.3.2"})
			if err != nil || plan.ApprovalRequired {
				t.Fatalf("plan=%#v, %v", plan, err)
			}
			apply := func() error {
				_, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "canonical-skill-reconciler"}, Reason: "Follow the compatible routine contract"})
				return err
			}
			if err := apply(); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
				t.Fatalf("upgrade failed to drain accepted legacy task: %v", err)
			}
			dispatcher, err := NewRunbookActionDispatcher(store.(runbookActionStore), nil)
			if err != nil {
				t.Fatal(err)
			}
			worker := NewActionWorker(store, catalog, nil, dispatcher)
			worker.now = func() time.Time { return now.Add(time.Second) }
			result, err := worker.RunOnce(t.Context(), run.Scope, "action-worker", time.Minute)
			if err != nil || result == nil || result.Call.Status != ActionCallStatusSucceeded {
				t.Fatalf("legacy task could not finish: %#v, %v", result, err)
			}
			if err := apply(); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
				t.Fatalf("upgrade invalidated the live legacy receipt: %v", err)
			}
			current, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			activity := NewRunActivityService(store, store)
			started, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: current.Revision, Status: AgentRunStatusRunning, Summary: "Finish the legacy task"})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: started.Revision, Status: AgentRunStatusCompleted, Summary: "Legacy task finished"}); err != nil {
				t.Fatal(err)
			}
			if err := apply(); err != nil {
				t.Fatalf("drained nullable contract upgrade: %v", err)
			}
			call, err := store.GetActionCall(t.Context(), run.Scope, proposal.Call.ID)
			if err != nil || call.SkillVersion != "1.3.1" || call.BindingRevision != 7 || call.Status != ActionCallStatusSucceeded {
				t.Fatalf("legacy accepted-call history changed: %#v, %v", call, err)
			}
		})
	}
}

func TestScheduledTaskNullableUpgradeRejectsUnreviewedDescriptors(t *testing.T) {
	for _, change := range []string{"unknown-version", "external-id", "external-source", "old-tamper", "metadata", "wider-limit", "other-nullable-field", "local-reference", "other-action", "output", "credentials", "risk", "storage", "side-effect", "idempotency", "external-policy", "transport"} {
		t.Run(change, func(t *testing.T) {
			previous, target := RunbookManagementSkillLegacy131(), RunbookManagementSkill()
			action := target.Actions[RunbookActionCreateTask]
			properties := action.InputSchema["properties"].(map[string]interface{})
			switch change {
			case "unknown-version":
				previous.Version = "1.3.9"
			case "external-id":
				previous.ID, target.ID = "external.runbooks", "external.runbooks"
			case "external-source":
				previous.Source = &skill.SourceProvenance{Identity: "native::untrusted", Format: "test"}
				target.Source = &skill.SourceProvenance{Identity: "native::untrusted", Format: "test"}
			case "old-tamper":
				previous.Actions[RunbookActionCreateTask].InputSchema["properties"].(map[string]interface{})["maximumOccurrences"].(map[string]interface{})["maximum"] = 2000000
			case "metadata":
				target.Description = "Unreviewed replacement descriptor"
			case "wider-limit":
				properties["maximumOccurrences"].(map[string]interface{})["type"] = []interface{}{"integer", "null", "string"}
			case "other-nullable-field":
				properties["title"].(map[string]interface{})["type"] = []interface{}{"string", "null"}
			case "local-reference":
				action.InputSchema["$defs"] = map[string]interface{}{"limit": properties["maximumOccurrences"]}
				properties["maximumOccurrences"] = map[string]interface{}{"$ref": "#/$defs/limit"}
			case "other-action":
				target.Actions[RunbookActionList].OutputSchema["description"] = "Changed other action contract"
			case "output":
				action.OutputSchema["description"] = "Changed output contract"
			case "credentials":
				action.Credentials = []skill.CredentialRequirement{{Name: "extra", Kind: "api_key"}}
			case "risk":
				action.Risk = skill.RiskLevelExternal
			case "storage":
				target.Requirements.Storage = []skill.StorageRequirement{{Name: "unreviewed", MountPath: "/var/lib/unreviewed", Durability: skill.StorageDurabilityPersistent, MinimumCapacity: "1Gi", Retention: skill.StorageRetentionRetain}}
			case "side-effect":
				action.SideEffect = skill.SideEffectExternal
			case "idempotency":
				action.Idempotency = skill.IdempotencySupported
			case "external-policy":
				action.ExternalOperationPolicy = skill.ExternalOperationOptional
			case "transport":
				target.Transport.Endpoint = "kernel://unreviewed"
			}
			target.Actions[RunbookActionCreateTask] = action
			if compatibleRunbookOccurrenceLimitUpgrade(previous, target) {
				t.Fatal("unreviewed descriptor received the builtin compatibility exception")
			}
			store := NewMemoryStore()
			run, catalog, before, _ := scheduledTaskUpgradeFixture(t, store, previous, target)
			service := NewSkillReferenceUpgradeService(store, catalog)
			plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: run.Scope, DeploymentID: run.AssignedAgentID, BindingID: before.ID, ToVersion: target.Version, ToSourceIdentity: skill.DefinitionSourceIdentity(target)})
			if err == nil {
				if !plan.ApprovalRequired {
					t.Fatalf("unreviewed descriptor bypassed policy: %#v", plan)
				}
				if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "canonical-skill-reconciler"}, Reason: "Attempt unreviewed upgrade"}); !errors.Is(err, ErrSkillReferenceUpgradeApproval) {
					t.Fatalf("unreviewed descriptor applied: %v", err)
				}
			}
			after, err := catalog.GetBinding(t.Context(), before.Scope, before.DeploymentID, before.ID)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("denied upgrade changed authority: %#v, %v", after, err)
			}
		})
	}
}
