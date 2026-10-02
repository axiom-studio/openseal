package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillReferenceUpgradeEnumeratesProjectsBeyondFirstPageAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(SkillReferenceUpgradeStore)
		catalog, binding, last := seedSkillUpgradeProjectPagination(t, store, false)
		recording := &skillUpgradeProjectPageRecorder{SkillReferenceUpgradeStore: store}
		service := NewSkillReferenceUpgradeService(recording, catalog)
		plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{
			Scope: last.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ToVersion: "1.1.0",
		})
		if err != nil {
			t.Fatal(err)
		}
		if plan.ApprovalRequired || len(plan.Projects) != 1 || plan.Projects[0].ID != last.ID ||
			plan.Projects[0].ExpectedRevision != last.Revision || !reflect.DeepEqual(plan.Projects[0].MonitorIDs, []string{"last-page-monitor"}) {
			t.Fatalf("last-page monitor missing from complete compatible plan: %#v", plan)
		}
		wantPages := []ProjectFilter{{Scope: last.Scope, Limit: 500}, {Scope: last.Scope, Limit: 500, Offset: 500}}
		if !reflect.DeepEqual(recording.pages, wantPages) {
			t.Fatalf("canonical enumeration did not use bounded pages: got=%#v want=%#v", recording.pages, wantPages)
		}
		if len(recording.ids[0]) != 500 || recording.ids[0][0] != "project-0000" || recording.ids[0][499] != "project-0499" ||
			!reflect.DeepEqual(recording.ids[1], []string{last.ID}) {
			t.Fatalf("timestamp ties did not put the affected Project beyond the first page: %#v", recording.ids)
		}
		receipt, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{
			Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Adopt compatible source observer",
		})
		if err != nil || receipt.BindingRevision != binding.Revision+1 || len(receipt.ActivityIDs) != 1 {
			t.Fatalf("apply complete reference plan: receipt=%#v err=%v", receipt, err)
		}
		updatedBinding, err := catalog.GetBinding(t.Context(), binding.Scope, binding.DeploymentID, binding.ID)
		if err != nil || updatedBinding.SkillVersion != "1.1.0" || updatedBinding.Revision != receipt.BindingRevision {
			t.Fatalf("binding did not move with last-page monitor: %#v err=%v", updatedBinding, err)
		}
		updated, err := store.GetProject(t.Context(), last.Scope, last.ID)
		if err != nil || updated.Revision != last.Revision+1 || len(updated.SourceMonitors) != 1 || updated.SourceMonitors[0].SkillVersion != "1.1.0" {
			t.Fatalf("last-page Project did not upgrade atomically: %#v err=%v", updated, err)
		}
		for _, id := range []string{"project-0000", "project-0499"} {
			unchanged, err := store.GetProject(t.Context(), last.Scope, id)
			if err != nil || unchanged.Revision != 1 || len(unchanged.SourceMonitors) != 0 {
				t.Fatalf("unrelated first-page Project changed: %#v err=%v", unchanged, err)
			}
		}
	})
}

func TestSkillReferenceUpgradeReviewsMonitorOnlyContractsBeyondFirstPageAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(SkillReferenceUpgradeStore)
		catalog, binding, last := seedSkillUpgradeProjectPagination(t, store, true)
		if !reflect.DeepEqual(binding.AllowedActions, []string{"read"}) {
			t.Fatalf("fixture must discover monitor contract through references, not binding actions: %#v", binding)
		}
		service := NewSkillReferenceUpgradeService(store, catalog)
		plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{
			Scope: last.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ToVersion: "1.1.0",
		})
		if err != nil || !plan.ApprovalRequired || len(plan.Projects) != 1 || plan.Projects[0].ID != last.ID {
			t.Fatalf("last-page referenced action bypassed review: plan=%#v err=%v", plan, err)
		}
		findings := make(map[string]bool)
		for _, finding := range plan.Findings {
			if strings.Contains(finding.Message, "monitor") {
				findings[finding.Code] = true
			}
		}
		if !findings["risk_increased"] || !findings["action_contract_changed"] {
			t.Fatalf("referenced-only monitor risk/schema changes were skipped: %#v", plan.Findings)
		}
		if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{
			Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Unreviewed change",
		}); !errors.Is(err, ErrSkillReferenceUpgradeApproval) {
			t.Fatalf("monitor-only contract change applied without review: %v", err)
		}
		currentBinding, err := catalog.GetBinding(t.Context(), binding.Scope, binding.DeploymentID, binding.ID)
		if err != nil || currentBinding.SkillVersion != "1.0.0" || currentBinding.Revision != binding.Revision {
			t.Fatalf("unreviewed plan changed binding: %#v err=%v", currentBinding, err)
		}
		currentProject, err := store.GetProject(t.Context(), last.Scope, last.ID)
		if err != nil || currentProject.Revision != last.Revision || currentProject.SourceMonitors[0].SkillVersion != "1.0.0" {
			t.Fatalf("unreviewed plan changed last-page Project: %#v err=%v", currentProject, err)
		}
	})
}

func TestSkillReferenceUpgradeIndexedPlanIncludesAllMatchingProjectsAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(SkillReferenceUpgradeStore)
		if _, ok := store.(ProjectSkillReferenceStore); !ok {
			t.Fatal("canonical store must expose indexed Project references")
		}
		catalog, binding, last := seedSkillUpgradeProjectPaginationMode(t, store, false, true)
		first, err := store.GetProject(t.Context(), last.Scope, "project-0000")
		if err != nil {
			t.Fatal(err)
		}
		secondMonitor := first.SourceMonitors[0]
		secondMonitor.ID = "secondary-source"
		secondMonitor.Action = "read"
		first.SourceMonitors = append(first.SourceMonitors, secondMonitor)
		first.Revision++
		if err := store.UpdateProject(t.Context(), first, first.Revision-1); err != nil {
			t.Fatal(err)
		}
		// Use the concrete canonical store so planning takes its optional indexed
		// path. Owner and assigned-Agent predicates both match every Project;
		// multiple monitors also must not duplicate a Project impact.
		plan, err := NewSkillReferenceUpgradeService(store, catalog).Plan(t.Context(), PlanSkillReferenceUpgradeRequest{
			Scope: last.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ToVersion: "1.1.0",
		})
		if err != nil || plan.ApprovalRequired || len(plan.Projects) != 501 {
			t.Fatalf("indexed matching references were truncated or duplicated: plan=%#v err=%v", plan, err)
		}
		for index, impact := range plan.Projects {
			wantID := fmt.Sprintf("project-%04d", index)
			wantMonitors := []string{"last-page-monitor"}
			wantRevision := int64(1)
			if index == 0 {
				wantMonitors = append(wantMonitors, "secondary-source")
				wantRevision = first.Revision
			}
			if impact.ID != wantID || impact.ExpectedRevision != wantRevision || !reflect.DeepEqual(impact.MonitorIDs, wantMonitors) {
				t.Fatalf("indexed impact %d lost exact reference identity: %#v", index, impact)
			}
		}
	})
}

func seedSkillUpgradeProjectPagination(t *testing.T, store SkillReferenceUpgradeStore, changedMonitor bool) (*skill.Catalog, *skill.Binding, *Project) {
	t.Helper()
	return seedSkillUpgradeProjectPaginationMode(t, store, changedMonitor, false)
}

func seedSkillUpgradeProjectPaginationMode(t *testing.T, store SkillReferenceUpgradeStore, changedMonitor, allMatching bool) (*skill.Catalog, *skill.Binding, *Project) {
	t.Helper()
	ctx := t.Context()
	catalog := skill.NewCatalogWithStore(store)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		definition := &skill.Definition{
			ID: "project-observer", Version: version, Name: "Project observer",
			Transport: skill.TransportReference{Kind: "tool", Endpoint: "project-observer"},
			Actions:   make(map[string]skill.Action),
		}
		for _, name := range []string{"read", "monitor"} {
			definition.Actions[name] = skill.Action{
				Name: name, Description: "Observe authorized source information", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
				InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
			}
		}
		if changedMonitor && version == "1.1.0" {
			monitor := definition.Actions["monitor"]
			monitor.Risk = skill.RiskLevelWrite
			monitor.InputSchema = map[string]interface{}{
				"type": "object", "required": []interface{}{"query"},
				"properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}},
			}
			definition.Actions["monitor"] = monitor
		}
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	scope := Scope{Kind: "tenant", ID: "paged-reference-upgrade"}
	allowed := []string{"read", "monitor"}
	if changedMonitor {
		allowed = []string{"read"}
	}
	binding := &skill.Binding{
		ID: "observer", Scope: skill.ScopeReference(scope), DeploymentID: "researcher", SkillID: "project-observer",
		SkillVersion: "1.0.0", AllowedActions: allowed, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: binding.DeploymentID},
		Title: "Monitor source", Goal: "Observe authorized sources", Status: ObjectiveStatusActive, Priority: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var last *Project
	for index := 0; index < 501; index++ {
		project := &Project{
			ID: fmt.Sprintf("project-%04d", index), Scope: scope, Owner: objective.Owner,
			Title: "Source research", Purpose: "Coordinate research", Status: ProjectStatusActive, Revision: 1,
			CreatedAt: eventWaitContractEpoch, UpdatedAt: eventWaitContractEpoch,
		}
		if index == 500 || allMatching {
			project.ObjectiveRefs = []string{objective.ID}
			project.SourceMonitors = []SourceMonitorReference{{
				ID: "last-page-monitor", ObjectiveID: objective.ID, AssignedAgentID: binding.DeploymentID,
				SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, Action: "monitor", SourcePolicyRef: "approved-sources",
				Deduplication: SourceMonitorDeduplicateStableSourceAndContent,
			}}
		}
		if index == 500 {
			last = project
		}
		if err := store.CreateProject(ctx, project); err != nil {
			t.Fatalf("create %s: %v", project.ID, err)
		}
	}
	return catalog, binding, last
}

type skillUpgradeProjectPageRecorder struct {
	SkillReferenceUpgradeStore
	pages []ProjectFilter
	ids   [][]string
}

func (s *skillUpgradeProjectPageRecorder) ListProjects(ctx context.Context, filter ProjectFilter) ([]*Project, error) {
	s.pages = append(s.pages, filter)
	rows, err := s.SkillReferenceUpgradeStore.ListProjects(ctx, filter)
	ids := make([]string, len(rows))
	for index, project := range rows {
		ids[index] = project.ID
	}
	s.ids = append(s.ids, ids)
	return rows, err
}

type skillUpgradeProjectPageStub struct {
	ProjectStore
	list func(context.Context, ProjectFilter) ([]*Project, error)
}

func (s skillUpgradeProjectPageStub) ListProjects(ctx context.Context, filter ProjectFilter) ([]*Project, error) {
	return s.list(ctx, filter)
}

func TestSkillReferenceUpgradeProjectEnumerationRejectsInvalidPages(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "page-validation"}
	page := func(count int) []*Project {
		rows := make([]*Project, count)
		for index := range rows {
			rows[index] = &Project{ID: fmt.Sprintf("project-%04d", index), Scope: scope}
		}
		return rows
	}
	for _, test := range []struct {
		name      string
		first     []*Project
		second    []*Project
		wantError error
	}{
		{name: "over-limit page", first: page(501), wantError: ErrSkillReferenceUpgradeInvalid},
		{name: "foreign scope", first: []*Project{{ID: "foreign", Scope: Scope{Kind: scope.Kind, ID: "other"}}}, wantError: ErrSkillReferenceUpgradeConflict},
		{name: "nil row", first: []*Project{nil}, wantError: ErrSkillReferenceUpgradeConflict},
		{name: "missing identity", first: []*Project{{Scope: scope}}, wantError: ErrSkillReferenceUpgradeConflict},
		{name: "duplicate within page", first: []*Project{{ID: "same", Scope: scope}, {ID: "same", Scope: scope}}, wantError: ErrSkillReferenceUpgradeConflict},
		{name: "duplicate across pages", first: page(500), second: []*Project{{ID: "project-0000", Scope: scope}}, wantError: ErrSkillReferenceUpgradeConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			store := skillUpgradeProjectPageStub{list: func(_ context.Context, filter ProjectFilter) ([]*Project, error) {
				calls++
				if filter.Scope != scope || filter.Limit != 500 || filter.Offset != (calls-1)*500 {
					t.Fatalf("wrong bounded scope/page: %#v", filter)
				}
				if calls == 1 {
					return test.first, nil
				}
				if calls > 2 {
					t.Fatal("invalid page should terminate enumeration")
				}
				return test.second, nil
			}}
			rows, err := listUpgradeProjects(t.Context(), store, scope)
			if !errors.Is(err, test.wantError) || rows != nil {
				t.Fatalf("invalid page produced a partial canonical plan: rows=%#v err=%v", rows, err)
			}
		})
	}
}

func TestSkillReferenceUpgradeProjectEnumerationHonorsCancellationAndStoreFailure(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "page-cancellation"}
	for _, cancelAfterPage := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel-after-page-%t", cancelAfterPage), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if !cancelAfterPage {
				cancel()
			}
			calls := 0
			store := skillUpgradeProjectPageStub{list: func(context.Context, ProjectFilter) ([]*Project, error) {
				calls++
				rows := make([]*Project, 500)
				for index := range rows {
					rows[index] = &Project{ID: fmt.Sprintf("project-%04d", index), Scope: scope}
				}
				cancel()
				return rows, nil
			}}
			rows, err := listUpgradeProjects(ctx, store, scope)
			wantCalls := 0
			if cancelAfterPage {
				wantCalls = 1
			}
			if !errors.Is(err, context.Canceled) || rows != nil || calls != wantCalls {
				t.Fatalf("canceled canonical enumeration continued: rows=%#v err=%v calls=%d", rows, err, calls)
			}
		})
	}
	storeFailure := errors.New("Project store unavailable")
	store := skillUpgradeProjectPageStub{list: func(context.Context, ProjectFilter) ([]*Project, error) {
		return nil, storeFailure
	}}
	if rows, err := listUpgradeProjects(t.Context(), store, scope); !errors.Is(err, storeFailure) || rows != nil {
		t.Fatalf("store failure was masked: rows=%#v err=%v", rows, err)
	}
	store = skillUpgradeProjectPageStub{list: func(_ context.Context, filter ProjectFilter) ([]*Project, error) {
		if filter.Offset != 0 {
			return nil, storeFailure
		}
		rows := make([]*Project, 500)
		for index := range rows {
			rows[index] = &Project{ID: fmt.Sprintf("project-%04d", index), Scope: scope}
		}
		return rows, nil
	}}
	if rows, err := listUpgradeProjects(t.Context(), store, scope); !errors.Is(err, storeFailure) || rows != nil {
		t.Fatalf("later-page failure exposed a partial plan: rows=%#v err=%v", rows, err)
	}
}
