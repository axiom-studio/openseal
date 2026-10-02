package openseal

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestEngineAcceptedRunExecutionPinsDefinitionAndReplaysOriginalIntent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, explicit := range []bool{false, true} {
			name := backend + "/derived"
			if explicit {
				name = backend + "/explicit"
			}
			t.Run(name, func(t *testing.T) {
				var store runtime.KernelStore = runtime.NewMemoryStore()
				if backend == "sqlite" {
					sqlite, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "accepted-engine.db"))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = sqlite.Close() })
					store = sqlite
				}
				engine, err := New(WithStore(store))
				if err != nil {
					t.Fatal(err)
				}
				scope := Scope{Kind: "tenant", ID: "accepted-engine"}
				definition := &AgentDefinition{
					ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate safely", SystemPrompt: "Operate safely",
					Authority: AgentAuthorityPolicy{MaximumRisk: SkillRiskRead, MaxConcurrentRuns: 1},
					Runbook:   &RunbookDefinition{APIVersion: RunbookAPIVersion, ID: "operator-method", Version: "1", Name: "Original method", Entrypoints: map[string]string{"manual": "done"}, Steps: map[string]RunbookStep{"done": {Kind: RunbookStepEnd, End: &RunbookEndStep{}}}},
				}
				if _, err := engine.RegisterAgentDefinition(t.Context(), definition); err != nil {
					t.Fatal(err)
				}
				deployment, _, err := engine.CreateAgentDeployment(t.Context(), &AgentDeployment{ID: "operator", Scope: SkillScope(scope), DefinitionID: "operator", ActiveVersion: "1", RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "owner", "Create operator")
				if err != nil {
					t.Fatal(err)
				}
				for _, partial := range []map[string]interface{}{{"runbookDefinitionId": "operator-method"}, {"runbookDefinitionVersion": "1"}} {
					request := CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "operator"}, AssignedAgentID: "operator", Entrypoint: "manual", Goal: "Incomplete method identity", Source: RunSourceManual, Context: partial}
					if _, err := engine.CreateAgentRunCommand(t.Context(), request); !errors.Is(err, runtime.ErrAcceptedRunExecution) {
						t.Fatalf("partial caller method identity was accepted with a non-replayable intent: %v", err)
					}
				}
				request := CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "operator"}, AssignedAgentID: "operator", Entrypoint: "manual", Goal: "Original requested work", Source: RunSourceManual, IdempotencyKey: "exact-delivery", Context: map[string]interface{}{"input": "preserved", runtime.AcceptedRunExecutionContextKey: "forged server snapshot"}}
				if explicit {
					request.Context["runbookDefinitionId"], request.Context["runbookDefinitionVersion"] = "operator-method", "1"
					request.Plan = map[string]interface{}{"runbook": map[string]interface{}{"id": "operator-method", "version": "1"}}
				}
				created, err := engine.CreateAgentRunCommand(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				pin, err := runtime.AcceptedRunExecutionForRun(created.Run)
				if err != nil || pin == nil || pin.DefinitionID != "operator" || pin.DefinitionVersion != "1" || pin.RunbookVersion != "1" || pin.DerivedRunbookIdentity == explicit {
					t.Fatalf("request did not receive authoritative immutable identity: %#v %v", pin, err)
				}
				if request.Context[runtime.AcceptedRunExecutionContextKey] != "forged server snapshot" || created.Run.Context["input"] != "preserved" {
					t.Fatal("preparation mutated caller inputs or retained forged identity")
				}
				before, err := engine.GetAgentRun(t.Context(), scope, created.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				latest := *definition
				latest.Version = "2"
				latest.Runbook = &RunbookDefinition{APIVersion: RunbookAPIVersion, ID: "different-method", Version: "2", Name: "Latest method", Entrypoints: map[string]string{"different": "done"}, Steps: map[string]RunbookStep{"done": {Kind: RunbookStepEnd, End: &RunbookEndStep{}}}}
				if _, err := engine.RegisterAgentDefinition(t.Context(), &latest); err != nil {
					t.Fatal(err)
				}
				if _, _, err := engine.ActivateAgentDefinition(t.Context(), SkillScope(scope), deployment.ID, "2", deployment.Revision, "user", "owner", "Adopt latest method"); err != nil {
					t.Fatal(err)
				}
				replay, err := engine.CreateAgentRunCommand(t.Context(), request)
				if err != nil || replay == nil || replay.Event != nil || !reflect.DeepEqual(replay.Run, before) {
					t.Fatalf("accepted retry consulted changed deployment or rewrote history: %#v %v", replay, err)
				}
				changed := request
				changed.Context = map[string]interface{}{"input": "different"}
				if explicit {
					changed.Context["runbookDefinitionId"], changed.Context["runbookDefinitionVersion"] = "operator-method", "1"
				}
				if _, err := engine.CreateAgentRunCommand(t.Context(), changed); !errors.Is(err, ErrRunIdempotency) {
					t.Fatalf("changed meaningful inputs bypassed canonical fingerprint: %v", err)
				}
				changed = request
				changed.IdempotencyKey = "new-delivery"
				if _, err := engine.CreateAgentRunCommand(t.Context(), changed); err == nil {
					t.Fatal("new work silently repinned a stale entrypoint to current definition")
				}
				runs, err := engine.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope})
				if err != nil || len(runs) != 1 {
					t.Fatalf("rejected/replayed delivery created extra work: %#v %v", runs, err)
				}
			})
		}
	}
}
