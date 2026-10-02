package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestRunbookManagementSkillPreservesPublished130Definition(t *testing.T) {
	// Snapshot of the complete definition before adding subject evidence. This
	// includes every action description and input/output schema, not just version.
	const published130SHA256 = "54b02b77d43c735d4aeb5523cec033241c70df8b5e5ca685d2c1743ba528fd0b"
	legacy := RunbookManagementSkillLegacy130()
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(encoded)); got != published130SHA256 {
		t.Fatalf("published 1.3.0 definition changed: got %s", got)
	}
	current := RunbookManagementSkill()
	if legacy.Version != "1.3.0" || current.Version != "1.3.1" {
		t.Fatalf("versions legacy=%s current=%s", legacy.Version, current.Version)
	}
	catalog := skill.NewCatalog()
	for _, definition := range []*skill.Definition{legacy, current} {
		if err := catalog.Register(t.Context(), definition); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{RunbookActionCreateWorkflow, RunbookActionWorkflowSources} {
			action := definition.Actions[name]
			if !isRunbookAction(&skill.BoundAction{Definition: definition, Action: action}) {
				t.Fatalf("version %s is not dispatched", definition.Version)
			}
			_, hasEvidence := action.InputSchema["properties"].(map[string]interface{})["subjectEvidenceActionCallId"]
			if hasEvidence != (definition.Version == current.Version) {
				t.Fatalf("version %s action %s evidence schema=%v", definition.Version, name, hasEvidence)
			}
		}
	}
	for _, definition := range []*skill.Definition{legacy, current} {
		bound := &skill.BoundAction{Definition: definition, Action: definition.Actions[RunbookActionWorkflowSources], Binding: &skill.Binding{}}
		err := catalog.ValidateInput(t.Context(), bound, map[string]interface{}{"subjectEvidenceActionCallId": "call-1"})
		if (err == nil) != (definition.Version == current.Version) {
			t.Fatalf("version %s new property input error=%v", definition.Version, err)
		}
	}
}

func TestRunbookManagementSkillExecutesQueued130WorkflowAfterHostUpgradeAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		for _, subject := range []string{"D-provider-room", "U-recipient"} {
			t.Run(kind+"/"+subject, func(t *testing.T) {
				store := workflowEvidenceStore(t, kind)
				run, catalog, bound, args := workflowActionDefinitionFixture(t, store, RunbookManagementSkillLegacy130(), true)
				originalBinding := *bound.Binding
				now := time.Now().UTC().Add(time.Second)
				coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
					return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
				}))
				coordinator.now = func() time.Time { return now }
				claim := func() {
					t.Helper()
					claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, WorkerID: "legacy-chat-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
					if err != nil || claimed == nil || claimed.ID != run.ID {
						t.Fatalf("claim originating conversation=%#v error=%v", claimed, err)
					}
					run = claimed
				}
				propose := func(bindingID string, revision int64, skillID, version, action string, arguments map[string]interface{}) *ActionProposalResult {
					t.Helper()
					proposal, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "legacy-chat-worker", DeploymentID: run.AssignedAgentID,
						BindingID: bindingID, BindingRevision: revision, SkillID: skillID, SkillVersion: version, Action: action, Arguments: arguments, IdempotencyKey: action, Summary: "Perform requested legacy action"})
					if err != nil {
						t.Fatal(err)
					}
					return proposal
				}
				providerCalls := 0
				dispatcher, err := NewRunbookActionDispatcher(store.(runbookActionStore), ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
					if input.Call.Action != "send_message" || input.Arguments["participant"] != "U-recipient" {
						t.Fatalf("unexpected provider action=%#v", input.Call)
					}
					providerCalls++
					return map[string]interface{}{"receipt": map[string]interface{}{"conversationId": "D-provider-room"}}, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				dispatcher.SetWorkflowCatalog(catalog)
				worker := NewActionWorker(store, catalog, nil, dispatcher)
				worker.now = func() time.Time { return now }
				execute := func() *ActionExecutionResult {
					t.Helper()
					now = now.Add(time.Second)
					result, err := worker.RunOnce(t.Context(), run.Scope, "legacy-action-worker", time.Minute)
					if err != nil || result == nil {
						t.Fatalf("execute queued action=%#v error=%v", result, err)
					}
					if result.Run != nil {
						run = result.Run
					}
					return result
				}
				claim()
				proof := propose("event-account", 1, "test-events", "1.0.0", "send_message", map[string]interface{}{"participant": "U-recipient"})
				if result := execute(); result.Call.Status != ActionCallStatusSucceeded || providerCalls != 1 {
					t.Fatalf("provider receipt=%#v calls=%d", result.Call, providerCalls)
				}
				// Discovery becomes lastAction; the receipt remains in protected
				// canonical history for the old create_workflow input contract.
				claim()
				propose(bound.Binding.ID, bound.Binding.Revision, bound.Definition.ID, "1.3.0", RunbookActionWorkflowSources, map[string]interface{}{})
				if result := execute(); result.Call.Status != ActionCallStatusSucceeded {
					t.Fatalf("legacy discovery=%#v", result.Call)
				}
				claim()
				args["subject"] = subject
				args["attributes"] = map[string]interface{}{"externalParticipantId": "U-recipient"}
				if _, exists := args["subjectEvidenceActionCallId"]; exists {
					t.Fatal("legacy queued call acquired a new-schema property")
				}
				queued := propose(bound.Binding.ID, bound.Binding.Revision, bound.Definition.ID, "1.3.0", RunbookActionCreateWorkflow, args)
				// Installing the host's current definition leaves the published
				// 1.3.0 binding/version/revision and queued action pins intact.
				if err := catalog.Register(t.Context(), RunbookManagementSkill()); err != nil {
					t.Fatal(err)
				}
				current, err := catalog.Resolve(t.Context(), bound.Binding.Scope, run.AssignedAgentID, bound.Definition.ID, "1.3.0", RunbookActionCreateWorkflow, skill.BindingReference{ID: originalBinding.ID, Revision: originalBinding.Revision})
				if err != nil || !reflect.DeepEqual(*current.Binding, originalBinding) || queued.Call.SkillVersion != "1.3.0" || queued.Call.BindingRevision != originalBinding.Revision {
					t.Fatalf("host upgrade changed queued binding: %#v error=%v", current, err)
				}
				points := pointWorkflowEvidenceStore(store)
				dispatcher, err = NewRunbookActionDispatcher(points, nil)
				if err != nil {
					t.Fatal(err)
				}
				dispatcher.SetWorkflowCatalog(catalog)
				worker = NewActionWorker(points, catalog, nil, dispatcher)
				worker.now = func() time.Time { return now }
				result := execute()
				for result.Call.Status == ActionCallStatusReady {
					now = result.Call.AvailableAt
					result = execute()
				}
				want := ActionCallStatusSucceeded
				if subject == "U-recipient" {
					want = ActionCallStatusFailed
				}
				validReads := len(points.lookups) == result.Call.Attempt
				for _, id := range points.lookups {
					validReads = validReads && id == proof.Call.ID
				}
				if result.Call.Status != want || result.Call.ID != queued.Call.ID || providerCalls != 1 || !validReads || points.scans != 0 {
					t.Fatalf("queued legacy result=%#v reads=%v scans=%d providercalls=%d", result.Call, points.lookups, points.scans, providerCalls)
				}
				values, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: run.Scope, Kind: RunKindAgentWork})
				wantWorkflows := 0
				if subject == "D-provider-room" {
					wantWorkflows = 1
				}
				if err != nil || len(values) != wantWorkflows {
					t.Fatalf("legacy workflows=%#v error=%v", values, err)
				}
			})
		}
	}
}

func TestRunbookManagementSkillDispatchesQueued130Workflow(t *testing.T) {
	store := NewMemoryStore()
	legacy := RunbookManagementSkillLegacy130()
	run, catalog, bound, args := workflowActionDefinitionFixture(t, store, legacy, false)
	if err := catalog.ValidateInput(t.Context(), bound, args); err != nil {
		t.Fatalf("queued legacy action input: %v", err)
	}
	result, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "queued-before-upgrade")
	if err != nil || result["workflowId"] == "" {
		t.Fatalf("queued legacy dispatch=%#v error=%v", result, err)
	}
}
