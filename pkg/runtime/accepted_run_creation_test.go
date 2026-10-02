package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type acceptedCreationTestPreparer struct {
	calls   int
	id      string
	version string
	err     error
}

func (p *acceptedCreationTestPreparer) PrepareAcceptedRunExecution(_ context.Context, request *CreateAgentRunRequest) error {
	p.calls++
	if p.err != nil {
		return p.err
	}
	request.Context = cloneMap(request.Context)
	if request.Context == nil {
		request.Context = map[string]interface{}{}
	}
	delete(request.Context, AcceptedRunExecutionContextKey)
	if request.Entrypoint == "" {
		return nil
	}
	if id, exists := request.Context["runbookDefinitionId"]; exists && id != p.id {
		return ErrAcceptedRunExecution
	}
	if version, exists := request.Context["runbookDefinitionVersion"]; exists && version != p.version {
		return ErrAcceptedRunExecution
	}
	if method, ok := request.Plan["runbook"].(map[string]interface{}); ok && (method["id"] != p.id || method["version"] != p.version) {
		return ErrAcceptedRunExecution
	}
	request.Context[AcceptedRunExecutionContextKey] = &AcceptedRunExecution{
		Scope: request.Scope, DeploymentID: request.AssignedAgentID, DefinitionID: "immutable-agent", DefinitionVersion: "1",
		RunbookID: p.id, RunbookVersion: p.version, Entrypoint: request.Entrypoint,
		SkillDependencies: []SkillRuntimeReference{{SkillID: "publisher", SkillVersion: "1.0.0", SourceIdentity: "verified::publisher"}},
	}
	return nil
}

func acceptedCreationStores(t *testing.T, test func(*testing.T, KernelStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, NewMemoryStore()) })
	t.Run("sqlite", func(t *testing.T) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "accepted-creation.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		test(t, store)
	})
}

func acceptedCreationActivation(t *testing.T, store KernelStore, trigger runbook.Trigger) (*RunbookActivation, *acceptedCreationTestPreparer) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "accepted-hooks"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	objective, err := NewPortfolioService(store).CreateObjective(t.Context(), CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Review", Goal: "Review authorized messages", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := NewRunbookActivationService(store).Create(t.Context(), CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "review", DefinitionVersion: "1", TriggerID: "trigger", Trigger: trigger,
		MaximumConcurrent: 10, Input: map[string]interface{}{"project": "preserve-input"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return activation, &acceptedCreationTestPreparer{id: "review", version: "1"}
}

func assertAcceptedCreationPin(t *testing.T, run *AgentRun) {
	t.Helper()
	pin, err := AcceptedRunExecutionForRun(run)
	if err != nil || pin == nil || pin.DefinitionID != "immutable-agent" || pin.DefinitionVersion != "1" || pin.RunbookID != "review" || pin.RunbookVersion != "1" || pin.DeploymentID != "rowan" || len(pin.SkillDependencies) != 1 || pin.SkillDependencies[0].SkillVersion != "1.0.0" {
		t.Fatalf("new accepted Run did not pin the immutable method and exact dependency: %#v, %v", pin, err)
	}
}

func TestAcceptedRunCreationManualPinsOnceAndReplaysBeforeMutableAdmission(t *testing.T) {
	acceptedCreationStores(t, func(t *testing.T, store KernelStore) {
		activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{
			Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"},
		})
		request := StartRunbookActivationRequest{IdempotencyKey: "manual-exact-method"}
		created, err := StartRunbookActivationWithExecutionPreparer(t.Context(), store, activation.Scope, activation.ID, request, preparer)
		if err != nil || created == nil || created.Event == nil || preparer.calls != 1 {
			t.Fatalf("manual creation = %#v, %v; preparations=%d", created, err, preparer.calls)
		}
		assertAcceptedCreationPin(t, created.Run)
		accepted, err := store.GetAgentRun(t.Context(), activation.Scope, created.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		preparer.err = errors.New("deployment active definition changed")
		replayed, err := StartRunbookActivationWithExecutionPreparer(t.Context(), store, activation.Scope, activation.ID, request, preparer)
		if err != nil || replayed == nil || replayed.Event != nil || preparer.calls != 1 || !reflect.DeepEqual(accepted, replayed.Run) {
			t.Fatalf("accepted manual retry consulted mutable admission or changed its pin: %#v, %v; preparations=%d", replayed, err, preparer.calls)
		}
		request.IdempotencyKey = "new-manual-stale-method"
		rejected, err := StartRunbookActivationWithExecutionPreparer(t.Context(), store, activation.Scope, activation.ID, request, preparer)
		if rejected != nil || !errors.Is(err, preparer.err) || preparer.calls != 2 {
			t.Fatalf("new manual occurrence bypassed immutable admission: %#v, %v", rejected, err)
		}
		persisted, err := store.GetAgentRun(t.Context(), activation.Scope, runIDForIdempotencyKey(activation.Scope, request.IdempotencyKey))
		if err != nil || persisted != nil {
			t.Fatalf("rejected manual occurrence was persisted: %#v, %v", persisted, err)
		}
		unchanged, err := store.GetRunbookActivation(t.Context(), activation.Scope, activation.ID)
		if err != nil || !reflect.DeepEqual(activation, unchanged) {
			t.Fatalf("preparing a Run rewrote its reviewed activation: %#v, %v", unchanged, err)
		}
	})
}

type acceptedCreationCursorFailure struct {
	KernelStore
	fail bool
}

func (s *acceptedCreationCursorFailure) UpdateRunbookActivation(ctx context.Context, value *RunbookActivation, revision int64) error {
	if s.fail {
		return errors.New("simulated crash before occurrence cursor commit")
	}
	return s.KernelStore.UpdateRunbookActivation(ctx, value, revision)
}

func TestAcceptedRunCreationSchedulerRetainsPinAcrossOccurrenceDeliveryRetry(t *testing.T) {
	acceptedCreationStores(t, func(t *testing.T, store KernelStore) {
		activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{
			Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"},
		})
		wrapped := &acceptedCreationCursorFailure{KernelStore: store}
		scheduler := NewRunbookScheduler(wrapped)
		scheduler.SetAcceptedRunExecutionPreparer(preparer)
		now := time.Now().UTC()
		scheduler.now = func() time.Time { return now }
		initialized, err := scheduler.ReconcileScope(t.Context(), activation.Scope, 10)
		if err != nil || initialized.Initialized != 1 || preparer.calls != 0 {
			t.Fatalf("schedule initialization prepared execution: %#v, %v", initialized, err)
		}
		cursor, err := store.GetRunbookActivation(t.Context(), activation.Scope, activation.ID)
		if err != nil {
			t.Fatal(err)
		}
		now = cursor.NextRunAt.Add(time.Second)
		key := fmt.Sprintf("runbook-schedule:%s:%s", activation.ID, cursor.NextOccurrenceBase.UTC().Format(time.RFC3339Nano))
		wrapped.fail = true
		created, err := scheduler.ReconcileScope(t.Context(), activation.Scope, 10)
		if err == nil || created.Scheduled != 1 || preparer.calls != 1 {
			t.Fatalf("schedule did not create before cursor failure: %#v, %v", created, err)
		}
		accepted, err := store.GetAgentRun(t.Context(), activation.Scope, runIDForIdempotencyKey(activation.Scope, key))
		if err != nil {
			t.Fatal(err)
		}
		assertAcceptedCreationPin(t, accepted)
		wrapped.fail, preparer.err = false, errors.New("new immutable method is active")
		replayed, err := scheduler.ReconcileScope(t.Context(), activation.Scope, 10)
		if err != nil || replayed.Replayed != 1 || preparer.calls != 1 {
			t.Fatalf("accepted schedule delivery retry changed its method: %#v, %v; preparations=%d", replayed, err, preparer.calls)
		}
		unchanged, err := store.GetAgentRun(t.Context(), activation.Scope, accepted.ID)
		if err != nil || !reflect.DeepEqual(accepted, unchanged) {
			t.Fatalf("schedule replay rewrote accepted continuation state: %#v, %v", unchanged, err)
		}
		cursor, _ = store.GetRunbookActivation(t.Context(), activation.Scope, activation.ID)
		now = cursor.NextRunAt.Add(time.Second)
		rejected, err := scheduler.ReconcileScope(t.Context(), activation.Scope, 10)
		if !errors.Is(err, preparer.err) || rejected.Scheduled != 0 || preparer.calls != 2 {
			t.Fatalf("new scheduled occurrence skipped current authoritative admission: %#v, %v", rejected, err)
		}
	})
}

func TestAcceptedRunCreationEventPinsAndRejectsNewMethodDrift(t *testing.T) {
	acceptedCreationStores(t, func(t *testing.T, store KernelStore) {
		activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{Kind: runbook.TriggerEvent, EventType: "message.received", Entrypoint: "review"})
		router := NewRunbookEventRouter(store)
		router.SetAcceptedRunExecutionPreparer(preparer)
		event := EventEnvelope{ID: "message-one", Scope: activation.Scope, Type: "message.received", Source: "verified-connector", Subject: "thread", OccurredAt: time.Now().UTC(), Payload: map[string]interface{}{"text": "keep original input"}}
		created, err := router.Route(t.Context(), event)
		if err != nil || len(created.Routes) != 1 || !created.Routes[0].Created || preparer.calls != 1 {
			t.Fatalf("event creation = %#v, %v", created, err)
		}
		assertAcceptedCreationPin(t, created.Routes[0].Run)
		accepted, err := store.GetAgentRun(t.Context(), activation.Scope, created.Routes[0].Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		preparer.version = "2"
		replayed, err := router.Route(t.Context(), event)
		if err != nil || len(replayed.Routes) != 1 || replayed.Routes[0].Created || preparer.calls != 1 || !reflect.DeepEqual(accepted, replayed.Routes[0].Run) {
			t.Fatalf("event retry repinned an accepted method: %#v, %v", replayed, err)
		}
		event.ID = "message-two"
		if rejected, err := router.Route(t.Context(), event); !errors.Is(err, ErrAcceptedRunExecution) || rejected != nil || preparer.calls != 2 {
			t.Fatalf("new event accepted a different method than its activation: %#v, %v", rejected, err)
		}
	})
}

func TestAcceptedRunCreationReplayRejectsChangedWorkBeforePreparation(t *testing.T) {
	store := NewMemoryStore()
	activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{Kind: runbook.TriggerEvent, EventType: "message.received", Entrypoint: "review"})
	request := CreateAgentRunRequest{Scope: activation.Scope, ObjectiveID: activation.ObjectiveID, Owner: activation.Owner, AssignedAgentID: activation.AssignedAgentID, Entrypoint: "review", Goal: "Original work", Source: RunSourceManual, IdempotencyKey: "request-conflict", Context: map[string]interface{}{"runbookDefinitionId": "review", "runbookDefinitionVersion": "1"}}
	commands := NewRunCommandService(store)
	created, err := createAgentRunWithAcceptedExecution(t.Context(), commands, request, preparer)
	if err != nil {
		t.Fatal(err)
	}
	assertAcceptedCreationPin(t, created.Run)
	if _, injected := request.Context[AcceptedRunExecutionContextKey]; injected {
		t.Fatal("preparation changed the caller's original context")
	}
	request.Goal = "Different work"
	preparer.err = errors.New("mutable admission must not run")
	conflict, err := createAgentRunWithAcceptedExecution(t.Context(), commands, request, preparer)
	if conflict != nil || !errors.Is(err, ErrRunIdempotency) || preparer.calls != 1 {
		t.Fatalf("changed retry bypassed canonical fingerprint or reached admission: %#v, %v", conflict, err)
	}
}

func TestAcceptedRunCreationDiscardsUntrustedPinBeforeCanonicalReplay(t *testing.T) {
	store := NewMemoryStore()
	activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{Kind: runbook.TriggerEvent, EventType: "message.received", Entrypoint: "review"})
	request := CreateAgentRunRequest{
		Scope: activation.Scope, ObjectiveID: activation.ObjectiveID, Owner: activation.Owner, AssignedAgentID: "rowan", Entrypoint: "review", Goal: "Original work", Source: RunSourceManual, IdempotencyKey: "untrusted-pin",
		Context: map[string]interface{}{"runbookDefinitionId": "review", "runbookDefinitionVersion": "1", "input": "preserved", AcceptedRunExecutionContextKey: "caller-supplied forged pin"},
	}
	commands := NewRunCommandService(store)
	created, err := createAgentRunWithAcceptedExecution(t.Context(), commands, request, preparer)
	if err != nil || created == nil || preparer.calls != 1 {
		t.Fatalf("untrusted pin affected authoritative creation: %#v, %v", created, err)
	}
	assertAcceptedCreationPin(t, created.Run)
	if created.Run.Context["input"] != "preserved" || request.Context[AcceptedRunExecutionContextKey] != "caller-supplied forged pin" {
		t.Fatal("server sanitization rewrote ordinary inputs or the caller's request")
	}
	preparer.err = errors.New("mutable admission must not run on replay")
	replayed, err := createAgentRunWithAcceptedExecution(t.Context(), commands, request, preparer)
	if err != nil || replayed == nil || replayed.Event != nil || replayed.Run.ID != created.Run.ID || preparer.calls != 1 {
		t.Fatalf("untrusted pin affected canonical replay: %#v, %v", replayed, err)
	}
}

func TestAcceptedRunCreationActionDispatcherPreparesNewWorkAndReplays(t *testing.T) {
	store := NewMemoryStore()
	activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}})
	conversation, _, err := NewConversationService(store).CreateConversation(t.Context(), CreateConversationRequest{
		Scope: activation.Scope, Owner: activation.Owner, Title: "Review",
		Origin: &ConversationReference{Kind: ConversationReferenceObjective, ID: activation.ObjectiveID, Version: 1}, IdempotencyKey: "review-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: activation.Scope, Owner: activation.Owner, AssignedAgentID: "rowan", Kind: RunKindConversation, Goal: "Run reviewed work", Source: RunSourceChat,
		Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewRunbookActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.SetAcceptedRunExecutionPreparer(preparer)
	definition := RunbookManagementSkill()
	input := ActionDispatchInput{
		Run: parent, Call: &ActionCall{ID: "approved-start", Scope: activation.Scope, RunID: parent.ID, DeploymentID: "rowan"},
		Bound:     &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: "rowan"}, Action: definition.Actions[RunbookActionStart]},
		Arguments: map[string]interface{}{"activationId": activation.ID, "reason": "User requested immediate review"},
	}
	created, err := dispatcher.DispatchAction(t.Context(), input)
	if err != nil || preparer.calls != 1 {
		t.Fatalf("approved Runbook action did not use accepted preparation: %#v, %v", created, err)
	}
	assertAcceptedCreationPin(t, created["run"].(*AgentRun))
	preparer.err = errors.New("new definition is active")
	replayed, err := dispatcher.DispatchAction(t.Context(), input)
	if err != nil || replayed["replayed"] != true || preparer.calls != 1 {
		t.Fatalf("accepted action delivery consulted mutable definition: %#v, %v", replayed, err)
	}
	input.Call.ID = "new-approved-start"
	if _, err := dispatcher.DispatchAction(t.Context(), input); !errors.Is(err, preparer.err) || preparer.calls != 2 {
		t.Fatalf("new action delivery bypassed authoritative admission: %v", err)
	}
}

type acceptedCreationExternalResolver struct {
	resolved *ResolvedExternalConversationRunbook
	calls    int
	err      error
}

func (r *acceptedCreationExternalResolver) ResolveExternalConversationRunbook(context.Context, Scope, *ExternalConversationEndpoint, ExternalConversationHandler) (*ResolvedExternalConversationRunbook, error) {
	r.calls++
	return r.resolved, r.err
}

func TestAcceptedRunCreationExternalDispatcherPinsExactPlanBeforePublication(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "accepted-hooks"}
	handler := ExternalConversationHandler{Kind: ExternalConversationHandlerRunbook, ID: "review", Version: "1", Trigger: "on-message", AssignedAgentID: "rowan"}
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "review", Version: "1", Name: "Review",
		Entrypoints: map[string]string{"review": "done"},
		Triggers:    map[string]runbook.Trigger{"on-message": {Kind: runbook.TriggerEvent, EventType: externalConversationEventType, Entrypoint: "review", ObjectiveID: "agent:rowan:review"}},
		Steps:       map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
	}
	resolver := &acceptedCreationExternalResolver{
		resolved: &ResolvedExternalConversationRunbook{Definition: definition, AssignedAgentID: "rowan"},
	}
	dispatcher := NewExternalConversationRunbookEventDispatcher(store, resolver)
	preparer := &acceptedCreationTestPreparer{id: "review", version: "1"}
	dispatcher.SetAcceptedRunExecutionPreparer(preparer)
	request := ExternalConversationDispatchRequest{
		Endpoint:     &ExternalConversationEndpoint{ID: "endpoint", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}, Handler: handler},
		Conversation: &Conversation{ID: "conversation"}, Message: &ChannelMessage{ID: "message"}, IdempotencyKey: "external-review-one",
		Event: EventEnvelope{ID: "event", Scope: scope, Type: externalConversationEventType, Source: "conversation-adapter:verified", Subject: "conversation", OccurredAt: time.Now().UTC()},
	}
	created, err := dispatcher.DispatchExternalConversationRunbook(t.Context(), handler, request)
	if err != nil || created == nil || preparer.calls != 1 || resolver.calls != 1 {
		t.Fatalf("external event did not pin its exact method: %#v, %v", created, err)
	}
	run, err := store.GetAgentRun(t.Context(), scope, created.RunID)
	if err != nil {
		t.Fatal(err)
	}
	assertAcceptedCreationPin(t, run)
	preparer.version = "2"
	resolver.err = errors.New("active deployment now contains a different method")
	replayed, err := dispatcher.DispatchExternalConversationRunbook(t.Context(), handler, request)
	if err != nil || replayed == nil || replayed.RunID != created.RunID || preparer.calls != 1 || resolver.calls != 1 {
		t.Fatalf("external retry repinned accepted work: %#v, %v", replayed, err)
	}
	request.Event.Payload = map[string]interface{}{"changed": "different caller work"}
	if changed, err := dispatcher.DispatchExternalConversationRunbook(t.Context(), handler, request); !errors.Is(err, ErrRunIdempotency) || changed != nil || resolver.calls != 1 {
		t.Fatalf("external replay accepted changed event or consulted mutable resolver: %#v, %v", changed, err)
	}
	request.Event.Payload = nil
	resolver.err = nil
	request.IdempotencyKey = "external-review-two"
	if rejected, err := dispatcher.DispatchExternalConversationRunbook(t.Context(), handler, request); !errors.Is(err, ErrAcceptedRunExecution) || rejected != nil || preparer.calls != 2 || resolver.calls != 2 {
		t.Fatalf("external new work ignored stale Plan method identity: %#v, %v", rejected, err)
	}
}
