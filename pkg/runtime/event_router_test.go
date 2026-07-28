package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestObjectiveEventRouterMatchesAndCreatesAuditedRunsExactlyOnce(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore() }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "events.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.store(t)
			scope := Scope{Kind: "tenant", ID: "operations"}
			objective := eventObjective(t, store, scope, ObjectiveStatusActive)
			router := NewObjectiveEventRouter(store)
			event := EventEnvelope{
				ID: "event-uid-1", Scope: scope, Type: "kubernetes.warning", Source: "cluster:production",
				Subject: "Deployment/checkout", Severity: "warning", OccurredAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
				Attributes: map[string]interface{}{"namespace": "store", "reason": "BackOff"},
				Payload:    map[string]interface{}{"message": "container restarted", "count": 3},
			}
			first, err := router.Route(ctx, event)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Routes) != 1 || !first.Routes[0].Created || first.Routes[0].Event == nil {
				t.Fatalf("first route = %#v", first)
			}
			run := first.Routes[0].Run
			if run.ObjectiveID != objective.ID || run.Source != RunSourceEvent || run.Kind != RunKindAgentWork || run.AssignedAgentID != "tenant/operations/sre" || run.Entrypoint != "" {
				t.Fatalf("event run identity = %#v", run)
			}
			if run.Context["event"].(map[string]interface{})["id"] != event.ID || run.Context["capabilityInvocation"].(map[string]interface{})["action"] != "list_events" {
				t.Fatalf("event run context = %#v", run.Context)
			}
			if run.Context["operatingMode"] != "evidence-first" || run.Policy["approvalForProduction"] != true {
				t.Fatalf("event run template = context %#v policy %#v", run.Context, run.Policy)
			}

			replay, err := router.Route(ctx, event)
			if err != nil {
				t.Fatal(err)
			}
			if len(replay.Routes) != 1 || replay.Routes[0].Created || replay.Routes[0].Run.ID != run.ID || replay.Routes[0].Event != nil {
				t.Fatalf("event replay = %#v", replay)
			}
			events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
			if err != nil || len(events) != 1 || events[0].EventType != "run.created" || events[0].Actor.Type != "event" {
				t.Fatalf("run activity = %#v, err %v", events, err)
			}

			nonmatch := event
			nonmatch.ID = "event-uid-2"
			nonmatch.Attributes = map[string]interface{}{"namespace": "store", "reason": "Scheduled"}
			ignored, err := router.Route(ctx, nonmatch)
			if err != nil || len(ignored.Routes) != 0 {
				t.Fatalf("nonmatching event = %#v, err %v", ignored, err)
			}
		})
	}
}

func TestObjectiveEventRouteIdentityIncludesSource(t *testing.T) {
	first := eventRouteKey("objective", "rule", "kubernetes:cluster:1", "shared-uid")
	second := eventRouteKey("objective", "rule", "kubernetes:cluster:2", "shared-uid")
	if first == second {
		t.Fatalf("cross-source events collided: %q", first)
	}
	if first != eventRouteKey("objective", "rule", "kubernetes:cluster:1", "shared-uid") {
		t.Fatal("same-source event identity is not stable")
	}
}

func TestEventRunRequestAllocatesCapabilityOnlyTemplateContext(t *testing.T) {
	event := matchingEvent(Scope{Kind: "tenant", ID: "operations"}, "capability-only")
	rule := ObjectiveEventRule{
		ID: "warnings", EventType: "kubernetes.warning", AssignedAgentID: "agent-live",
		RunTemplate: &ObjectiveRunTemplate{Capability: &ObjectiveCapabilityInvocation{
			SkillID: "openseal.kubernetes", SkillVersion: "1.0.0", Action: "list_events",
			Inputs: map[string]interface{}{"namespace": "operations"},
		}},
	}
	request := eventRunRequest(&Objective{ID: "objective", Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-live"}, Goal: "Investigate"}, rule, event)
	invocation, ok := request.Context["capabilityInvocation"].(map[string]interface{})
	if !ok || invocation["action"] != "list_events" {
		t.Fatalf("capability invocation = %#v", request.Context)
	}
	projected, ok := request.Context["event"].(map[string]interface{})
	if !ok || projected["id"] != event.ID {
		t.Fatalf("event evidence = %#v", request.Context)
	}
	if rule.RunTemplate.Context != nil {
		t.Fatalf("portable template context was mutated: %#v", rule.RunTemplate.Context)
	}
}

func TestObjectiveEventRuleAgentReferenceValidation(t *testing.T) {
	for name, reference := range map[string]string{
		"empty segment": "tenant//sre",
		"extra segment": "tenant/one/team/sre",
		"query":         "tenant/one/sre?admin=true",
		"url":           "https://example.invalid/sre",
		"newline":       "tenant/one/sre\nother",
	} {
		t.Run(name, func(t *testing.T) {
			rule := ObjectiveEventRule{ID: "warning", EventType: "kubernetes.warning", AssignedAgentID: reference}
			if err := rule.Validate(); err == nil || !strings.Contains(err.Error(), "assignedAgentId") {
				t.Fatalf("invalid Agent reference %q accepted: %v", reference, err)
			}
		})
	}
	valid := ObjectiveEventRule{ID: "warning", EventType: "kubernetes.warning", AssignedAgentID: "tenant/one/sre"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("canonical Agent reference rejected: %v", err)
	}
}

func TestObjectiveEventRuleCapabilityRequiresBudgetForBothDurablePhases(t *testing.T) {
	template := &ObjectiveRunTemplate{Capability: &ObjectiveCapabilityInvocation{
		SkillID: "openseal.kubernetes", SkillVersion: "1.0.1", Action: "list_events",
	}}
	for name, budget := range map[string]*BudgetPolicy{
		"one attempt": {MaxAttempts: 1, MaxTurns: 2},
		"one turn":    {MaxAttempts: 2, MaxTurns: 1},
	} {
		t.Run(name, func(t *testing.T) {
			rule := ObjectiveEventRule{ID: "warning", EventType: "kubernetes.warning", RunBudget: budget, RunTemplate: template}
			if err := rule.Validate(); err == nil || !strings.Contains(err.Error(), "at least 2") {
				t.Fatalf("non-completable capability budget accepted: %v", err)
			}
		})
	}
	rule := ObjectiveEventRule{ID: "warning", EventType: "kubernetes.warning", RunBudget: &BudgetPolicy{MaxAttempts: 2, MaxTurns: 2}, RunTemplate: template}
	if err := rule.Validate(); err != nil {
		t.Fatalf("completable capability budget rejected: %v", err)
	}
}

func TestObjectiveEventRouterIsScopeLifecycleAndCredentialSafe(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	router := NewObjectiveEventRouter(store)
	activeScope := Scope{Kind: "tenant", ID: "active"}
	pausedScope := Scope{Kind: "tenant", ID: "paused"}
	eventObjective(t, store, activeScope, ObjectiveStatusActive)
	eventObjective(t, store, pausedScope, ObjectiveStatusPaused)

	paused, err := router.Route(ctx, matchingEvent(pausedScope, "paused-event"))
	if err != nil || len(paused.Routes) != 0 {
		t.Fatalf("paused objective routed event = %#v, err %v", paused, err)
	}
	foreign, err := router.Route(ctx, EventEnvelope{
		ID: "foreign-event", Scope: Scope{Kind: "tenant", ID: "foreign"}, Type: "kubernetes.warning",
		Source: "cluster:production", OccurredAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
		Attributes: map[string]interface{}{"namespace": "store", "reason": "BackOff"},
	})
	if err != nil || len(foreign.Routes) != 0 {
		t.Fatalf("foreign scope routed event = %#v, err %v", foreign, err)
	}
	unsafe := matchingEvent(activeScope, "unsafe-event")
	unsafe.Payload = map[string]interface{}{"apiToken": "resolved-secret"}
	if _, err = router.Route(ctx, unsafe); !errors.Is(err, ErrUnsafeSharedContext) {
		t.Fatalf("unsafe event error = %v", err)
	}

	invalidRules := map[string]interface{}{"version": "1", "rules": []interface{}{map[string]interface{}{
		"id": "unsafe", "eventType": "kubernetes.warning", "attributes": map[string]interface{}{"secret": "value"},
	}}}
	if _, err = NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: activeScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}, Title: "Unsafe", Goal: "Reject", Status: ObjectiveStatusActive, EventRules: invalidRules,
	}); !errors.Is(err, ErrUnsafeSharedContext) {
		t.Fatalf("unsafe rule error = %v", err)
	}
	unknownRules := map[string]interface{}{"version": "1", "rules": []interface{}{map[string]interface{}{
		"id": "unknown", "eventType": "kubernetes.warning", "silentFallback": true,
	}}}
	if _, err = DecodeObjectiveEventRules(unknownRules); !errors.Is(err, ErrInvalidObjectiveEventRules) {
		t.Fatalf("unknown rule field error = %v", err)
	}
}

func TestObjectiveEventRouterConcurrentDeliveryAndSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "restart"}
	eventObjective(t, store, scope, ObjectiveStatusActive)
	event := matchingEvent(scope, "stable-source-event")
	router := NewObjectiveEventRouter(store)

	const contenders = 16
	var wg sync.WaitGroup
	created := make(chan bool, contenders)
	errs := make(chan error, contenders)
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, routeErr := router.Route(ctx, event)
			if routeErr != nil {
				errs <- routeErr
				return
			}
			created <- result.Routes[0].Created
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	for routeErr := range errs {
		if routeErr != nil {
			t.Fatal(routeErr)
		}
	}
	createdCount := 0
	for value := range created {
		if value {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created routes = %d, want 1", createdCount)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := NewObjectiveEventRouter(reopened).Route(ctx, event)
	if err != nil || len(replay.Routes) != 1 || replay.Routes[0].Created {
		t.Fatalf("restart replay = %#v, err %v", replay, err)
	}
	runs, err := reopened.ListAgentRuns(ctx, AgentRunFilter{Scope: scope})
	if err != nil || len(runs) != 1 {
		t.Fatalf("restart runs = %#v, err %v", runs, err)
	}
}

func eventObjective(t *testing.T, store KernelStore, scope Scope, status ObjectiveStatus) *Objective {
	t.Helper()
	rules := ObjectiveEventRules{Version: "1", Rules: []ObjectiveEventRule{{
		ID: "kubernetes-backoff", EventType: "kubernetes.warning", Source: "cluster:production",
		Severities: []string{"warning"}, Attributes: map[string]interface{}{"namespace": "store", "reason": "BackOff"},
		AssignedAgentID: "tenant/operations/sre",
		RunTemplate: &ObjectiveRunTemplate{
			Context: map[string]interface{}{"operatingMode": "evidence-first"},
			Policy:  map[string]interface{}{"approvalForProduction": true}, Capability: &ObjectiveCapabilityInvocation{
				SkillID: "openseal.kubernetes", SkillVersion: "1.0.0", Action: "list_events", Inputs: map[string]interface{}{"namespace": "store"},
			},
		},
	}}}
	encoded, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	var eventRules map[string]interface{}
	if err := json.Unmarshal(encoded, &eventRules); err != nil {
		t.Fatal(err)
	}
	objective, err := NewPortfolioService(store).CreateObjective(context.Background(), CreateObjectiveRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre"}, Title: "Production reliability",
		Goal: "Investigate Kubernetes warnings and remediate safely", Status: status, Priority: 10, EventRules: eventRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	return objective
}

func matchingEvent(scope Scope, id string) EventEnvelope {
	return EventEnvelope{
		ID: id, Scope: scope, Type: "kubernetes.warning", Source: "cluster:production", Severity: "warning",
		OccurredAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
		Attributes: map[string]interface{}{"namespace": "store", "reason": "BackOff"},
	}
}
