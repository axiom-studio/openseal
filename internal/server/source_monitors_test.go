package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestSourceMonitorAPIExposesCheckpointAndEvidenceReadOnly(t *testing.T) {
	store := runtime.NewMemoryStore()
	ctx := context.Background()
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "researcher"}
	portfolio := runtime.NewPortfolioService(store)
	objective, err := portfolio.CreateObjective(ctx, runtime.CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "Monitor", Goal: "Collect evidence", Status: runtime.ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.NewRunbookActivationService(store).Create(ctx, runtime.CreateRunbookActivationRequest{
		ID: "monitor-1-runbook", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "researcher",
		DefinitionID: "research", DefinitionVersion: "1.0.0", TriggerID: "monitor-1",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 * * * * *", Timezone: "UTC"}, Entrypoint: "monitor"},
		Input:   map[string]interface{}{"projectId": "project-1", "sourceMonitorId": "monitor-1"},
		Policy:  map[string]interface{}{"sourcePolicyRef": "public@1"}, Status: runtime.RunbookActivationActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := runtime.NewProjectService(store, store).Create(ctx, runtime.CreateProjectRequest{Project: &runtime.Project{ID: "project-1", Scope: scope, Owner: owner, Title: "Research", Purpose: "Cited evidence", Status: runtime.ProjectStatusActive, ObjectiveRefs: []string{objective.ID}, SourceMonitors: []runtime.SourceMonitorReference{{ID: "monitor-1", ObjectiveID: objective.ID, AssignedAgentID: "researcher", SkillID: "reader", SkillVersion: "1", Action: "read", SourcePolicyRef: "public@1", Deduplication: runtime.SourceMonitorDeduplicateStableSourceAndContent}}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.CreateAgentRun(ctx, runtime.CreateAgentRunRequest{Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: "researcher", Goal: objective.Goal, Source: runtime.RunSourceSchedule, Context: map[string]interface{}{"projectId": project.ID, "sourceMonitorId": "monitor-1"}})
	if err != nil {
		t.Fatal(err)
	}
	service := runtime.NewSourceMonitorService(store, store, store, store)
	_, err = service.AdvanceCheckpoint(ctx, runtime.AdvanceSourceMonitorCheckpointRequest{Scope: scope, ProjectID: project.ID, MonitorID: "monitor-1", Cursor: "cursor-1", RunID: run.ID, AgentID: "researcher", SkillID: "reader", SkillVersion: "1", Action: "read", ActionCallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	api := NewServer(store, zap.NewNop().Sugar())
	checkpoint := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/projects/project-1/source-monitors/monitor-1/checkpoint?scopeKind=tenant&scopeId=one", "", "")
	if checkpoint.Code != http.StatusOK || !strings.Contains(checkpoint.Body.String(), `"cursor":"cursor-1"`) || !strings.Contains(checkpoint.Body.String(), `"observationCount":0`) {
		t.Fatalf("checkpoint=%d %s", checkpoint.Code, checkpoint.Body.String())
	}
	observations := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/projects/project-1/source-monitors/monitor-1/observations?scopeKind=tenant&scopeId=one&limit=20", "", "")
	if observations.Code != http.StatusOK || strings.TrimSpace(observations.Body.String()) != "[]" {
		t.Fatalf("observations=%d %s", observations.Code, observations.Body.String())
	}
	missing := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/projects/project-1/source-monitors/missing/checkpoint?scopeKind=tenant&scopeId=one", "", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing=%d %s", missing.Code, missing.Body.String())
	}
	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), runtimeSourceMonitorsCapabilityID) {
		t.Fatalf("capabilities=%d %s", capabilities.Code, capabilities.Body.String())
	}
}

const runtimeSourceMonitorsCapabilityID = `"id":"source-monitors"`
