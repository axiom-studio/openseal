package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type acceptedCreationTestPreparer struct {
	calls   int
	id      string
	version string
	err     error
}

func (p *acceptedCreationTestPreparer) PrepareAcceptedRunExecution(_ context.Context, request *models.CreateAgentRunRequest) error {
	p.calls++
	if p.err != nil {
		return p.err
	}
	request.Context = maps.Clone(request.Context)
	if request.Context == nil {
		request.Context = map[string]interface{}{}
	}
	delete(request.Context, models.AcceptedRunExecutionContextKey)
	if request.Entrypoint == "" {
		return nil
	}
	if id, exists := request.Context["runbookDefinitionId"]; exists && id != p.id {
		return models.ErrAcceptedRunExecution
	}
	if version, exists := request.Context["runbookDefinitionVersion"]; exists && version != p.version {
		return models.ErrAcceptedRunExecution
	}
	if method, ok := request.Plan["runbook"].(map[string]interface{}); ok && (method["id"] != p.id || method["version"] != p.version) {
		return models.ErrAcceptedRunExecution
	}
	request.Context[models.AcceptedRunExecutionContextKey] = &models.AcceptedRunExecution{
		Scope: request.Scope, DeploymentID: request.AssignedAgentID, DefinitionID: "immutable-agent", DefinitionVersion: "1",
		RunbookID: p.id, RunbookVersion: p.version, Entrypoint: request.Entrypoint,
		SkillDependencies: []models.SkillRuntimeReference{{SkillID: "publisher", SkillVersion: "1.0.0", SourceIdentity: "verified::publisher"}},
	}
	return nil
}

func acceptedCreationStores(t *testing.T, test func(*testing.T, models.KernelStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.New(filepath.Join(t.TempDir(), "accepted-creation.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		test(t, store)
	})
}

func acceptedCreationActivation(t *testing.T, store models.KernelStore, trigger runbook.Trigger) (*models.RunbookActivation, *acceptedCreationTestPreparer) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "accepted-hooks"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	objective, err := portfolio.NewPortfolioService(store).CreateObjective(t.Context(), portfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Review", Goal: "Review authorized messages", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := routine.NewRunbookActivationService(store).Create(t.Context(), routine.CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "review", DefinitionVersion: "1", TriggerID: "trigger", Trigger: trigger,
		MaximumConcurrent: 10, Input: map[string]interface{}{"project": "preserve-input"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return activation, &acceptedCreationTestPreparer{id: "review", version: "1"}
}

func assertAcceptedCreationPin(t *testing.T, run *models.AgentRun) {
	t.Helper()
	pin, err := models.AcceptedRunExecutionForRun(run)
	if err != nil || pin == nil || pin.DefinitionID != "immutable-agent" || pin.DefinitionVersion != "1" || pin.RunbookID != "review" || pin.RunbookVersion != "1" || pin.DeploymentID != "rowan" || len(pin.SkillDependencies) != 1 || pin.SkillDependencies[0].SkillVersion != "1.0.0" {
		t.Fatalf("new accepted Run did not pin the immutable method and exact dependency: %#v, %v", pin, err)
	}
}

func TestAcceptedRunCreationManualPinsOnceAndReplaysBeforeMutableAdmission(t *testing.T) {
	acceptedCreationStores(t, func(t *testing.T, store models.KernelStore) {
		activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{
			Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"},
		})
		request := routine.StartRunbookActivationRequest{IdempotencyKey: "manual-exact-method"}
		created, err := routine.StartRunbookActivationWithExecutionPreparer(t.Context(), store, activation.Scope, activation.ID, request, preparer)
		if err != nil || created == nil || created.Event == nil || preparer.calls != 1 {
			t.Fatalf("manual creation = %#v, %v; preparations=%d", created, err, preparer.calls)
		}
		assertAcceptedCreationPin(t, created.Run)
		accepted, err := store.GetAgentRun(t.Context(), activation.Scope, created.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		preparer.err = errors.New("deployment active definition changed")
		replayed, err := routine.StartRunbookActivationWithExecutionPreparer(t.Context(), store, activation.Scope, activation.ID, request, preparer)
		if err != nil || replayed == nil || replayed.Event != nil || preparer.calls != 1 || !reflect.DeepEqual(accepted, replayed.Run) {
			t.Fatalf("accepted manual retry consulted mutable admission or changed its pin: %#v, %v; preparations=%d", replayed, err, preparer.calls)
		}
		request.IdempotencyKey = "new-manual-stale-method"
		rejected, err := routine.StartRunbookActivationWithExecutionPreparer(t.Context(), store, activation.Scope, activation.ID, request, preparer)
		if rejected != nil || !errors.Is(err, preparer.err) || preparer.calls != 2 {
			t.Fatalf("new manual occurrence bypassed immutable admission: %#v, %v", rejected, err)
		}
		persisted, err := store.GetAgentRun(t.Context(), activation.Scope, agentrun.RunIDForIdempotencyKey(activation.Scope, request.IdempotencyKey))
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
	models.KernelStore
	fail bool
}

func (s *acceptedCreationCursorFailure) UpdateRunbookActivation(ctx context.Context, value *models.RunbookActivation, revision int64) error {
	if s.fail {
		return errors.New("simulated crash before occurrence cursor commit")
	}
	return s.KernelStore.UpdateRunbookActivation(ctx, value, revision)
}

func TestAcceptedRunCreationSchedulerRetainsPinAcrossOccurrenceDeliveryRetry(t *testing.T) {
	acceptedCreationStores(t, func(t *testing.T, store models.KernelStore) {
		activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{
			Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"},
		})
		wrapped := &acceptedCreationCursorFailure{KernelStore: store}
		now := time.Now().UTC()
		scheduler := routine.NewRunbookScheduler(wrapped, models.WithClock(func() time.Time { return now }))
		scheduler.SetAcceptedRunExecutionPreparer(preparer)
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
		accepted, err := store.GetAgentRun(t.Context(), activation.Scope, agentrun.RunIDForIdempotencyKey(activation.Scope, key))
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

func TestAcceptedRunCreationReplayRejectsChangedWorkBeforePreparation(t *testing.T) {
	store := kerneltest.NewStore(t)
	activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}})
	request := models.CreateAgentRunRequest{Scope: activation.Scope, ObjectiveID: activation.ObjectiveID, Owner: activation.Owner, AssignedAgentID: activation.AssignedAgentID, Entrypoint: "review", Goal: "Original work", Source: models.RunSourceManual, IdempotencyKey: "request-conflict", Context: map[string]interface{}{"runbookDefinitionId": "review", "runbookDefinitionVersion": "1"}}
	commands := agentrun.NewRunCommandService(store)
	created, err := agentrun.CreateAgentRunWithAcceptedExecution(t.Context(), commands, request, preparer)
	if err != nil {
		t.Fatal(err)
	}
	assertAcceptedCreationPin(t, created.Run)
	if _, injected := request.Context[models.AcceptedRunExecutionContextKey]; injected {
		t.Fatal("preparation changed the caller's original context")
	}
	request.Goal = "Different work"
	preparer.err = errors.New("mutable admission must not run")
	conflict, err := agentrun.CreateAgentRunWithAcceptedExecution(t.Context(), commands, request, preparer)
	if conflict != nil || !errors.Is(err, models.ErrRunIdempotency) || preparer.calls != 1 {
		t.Fatalf("changed retry bypassed canonical fingerprint or reached admission: %#v, %v", conflict, err)
	}
}

func TestAcceptedRunCreationDiscardsUntrustedPinBeforeCanonicalReplay(t *testing.T) {
	store := kerneltest.NewStore(t)
	activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}})
	request := models.CreateAgentRunRequest{
		Scope: activation.Scope, ObjectiveID: activation.ObjectiveID, Owner: activation.Owner, AssignedAgentID: "rowan", Entrypoint: "review", Goal: "Original work", Source: models.RunSourceManual, IdempotencyKey: "untrusted-pin",
		Context: map[string]interface{}{"runbookDefinitionId": "review", "runbookDefinitionVersion": "1", "input": "preserved", models.AcceptedRunExecutionContextKey: "caller-supplied forged pin"},
	}
	commands := agentrun.NewRunCommandService(store)
	created, err := agentrun.CreateAgentRunWithAcceptedExecution(t.Context(), commands, request, preparer)
	if err != nil || created == nil || preparer.calls != 1 {
		t.Fatalf("untrusted pin affected authoritative creation: %#v, %v", created, err)
	}
	assertAcceptedCreationPin(t, created.Run)
	if created.Run.Context["input"] != "preserved" || request.Context[models.AcceptedRunExecutionContextKey] != "caller-supplied forged pin" {
		t.Fatal("server sanitization rewrote ordinary inputs or the caller's request")
	}
	preparer.err = errors.New("mutable admission must not run on replay")
	replayed, err := agentrun.CreateAgentRunWithAcceptedExecution(t.Context(), commands, request, preparer)
	if err != nil || replayed == nil || replayed.Event != nil || replayed.Run.ID != created.Run.ID || preparer.calls != 1 {
		t.Fatalf("untrusted pin affected canonical replay: %#v, %v", replayed, err)
	}
}

func TestAcceptedRunCreationActionDispatcherPreparesNewWorkAndReplays(t *testing.T) {
	store := kerneltest.NewStore(t)
	activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}})
	seedTestSpace(t, store, activation.Scope)
	conversation, _, err := conversation.NewConversationService(store).CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: activation.Scope, Owner: activation.Owner, Title: "Review",
		Origin: &models.ConversationReference{Kind: models.ConversationReferenceObjective, ID: activation.ObjectiveID, Version: 1}, IdempotencyKey: "review-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: activation.Scope, Owner: activation.Owner, AssignedAgentID: "rowan", Kind: models.RunKindConversation, Goal: "Run reviewed work", Source: models.RunSourceChat,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := routine.NewRunbookActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.SetAcceptedRunExecutionPreparer(preparer)
	definition := routine.RunbookManagementSkill()
	input := agentrun.ActionDispatchInput{
		Run: parent, Call: &models.ActionCall{ID: "approved-start", Scope: activation.Scope, RunID: parent.ID, DeploymentID: "rowan"},
		Bound:     &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: "rowan"}, Action: definition.Actions[routine.RunbookActionStart]},
		Arguments: map[string]interface{}{"activationId": activation.ID, "reason": "User requested immediate review"},
	}
	created, err := dispatcher.DispatchAction(t.Context(), input)
	if err != nil || preparer.calls != 1 {
		t.Fatalf("approved Runbook action did not use accepted preparation: %#v, %v", created, err)
	}
	assertAcceptedCreationPin(t, created["run"].(*models.AgentRun))
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
