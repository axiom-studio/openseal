package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestObjectiveScheduleReconciliationProjectsCanonicalRuns(t *testing.T) {
	ctx := t.Context()
	store := runtime.NewMemoryStore()
	due := time.Now().UTC().Add(-time.Minute)
	objective, err := runtime.NewPortfolioService(store).CreateObjective(ctx, runtime.CreateObjectiveRequest{
		Scope: runtime.Scope{Kind: "tenant", ID: "operations"},
		Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "sre"},
		Title: "Production reliability", Goal: "Inspect production health", Status: runtime.ObjectiveStatusActive,
		Cadence: &runtime.ObjectiveCadence{
			Type: runtime.ObjectiveCadenceInterval, IntervalSeconds: 300, AssignedAgentID: "sre",
		},
		NextEvaluationAt: &due, IdempotencyKey: "production-reliability",
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewServer(store, zap.NewNop().Sugar())
	body := `{"scope":{"kind":"tenant","id":"operations"},"limit":10}`
	response := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/objective-schedules/reconciliations", body, "")
	if response.Code != http.StatusOK {
		t.Fatalf("reconcile = %d %s", response.Code, response.Body.String())
	}
	var reconciliation kernelapi.ObjectiveScheduleReconciliation
	if err := json.NewDecoder(response.Body).Decode(&reconciliation); err != nil {
		t.Fatal(err)
	}
	if reconciliation.Scope != objective.Scope || reconciliation.ReconciledAt.IsZero() || reconciliation.Result == nil || reconciliation.Result.Scheduled != 1 || reconciliation.Result.Examined != 1 {
		t.Fatalf("reconciliation = %#v", reconciliation)
	}
	runs, err := store.ListAgentRuns(ctx, runtime.AgentRunFilter{Scope: objective.Scope, ObjectiveID: objective.ID})
	if err != nil || len(runs) != 1 || runs[0].Source != runtime.RunSourceSchedule || runs[0].AssignedAgentID != "sre" {
		t.Fatalf("runs = %#v, err = %v", runs, err)
	}

	replay := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/objective-schedules/reconciliations", body, "")
	if replay.Code != http.StatusOK {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body.String())
	}
	if err := json.NewDecoder(replay.Body).Decode(&reconciliation); err != nil {
		t.Fatal(err)
	}
	if reconciliation.Result == nil || reconciliation.Result.Scheduled != 0 {
		t.Fatalf("replay reconciliation = %#v", reconciliation)
	}

	for name, invalidBody := range map[string]string{
		"protocol drift": `{"scope":{"kind":"tenant","id":"operations"},"executeDirectly":true}`,
		"invalid limit":  `{"scope":{"kind":"tenant","id":"operations"},"limit":501}`,
	} {
		t.Run(name, func(t *testing.T) {
			invalid := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/objective-schedules/reconciliations", invalidBody, "")
			if invalid.Code != http.StatusBadRequest {
				t.Fatalf("invalid = %d %s", invalid.Code, invalid.Body.String())
			}
		})
	}

	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	var document kernelapi.CapabilityDocument
	if err := json.NewDecoder(capabilities.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.ObjectiveSchedulesCapabilityID, kernelapi.ObjectiveSchedulesCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationReconcile) {
		t.Fatalf("schedule capability = %#v", document)
	}
}
