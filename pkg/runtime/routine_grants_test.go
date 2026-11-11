package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type routineOwnerAddressesForTest map[string][]string

func (r routineOwnerAddressesForTest) RoutineOwnerAddresses(_ context.Context, _ Scope, owner ConversationParticipant) ([]string, error) {
	if owner.Type != ConversationParticipantUser {
		return nil, errors.New("not a user")
	}
	return r[owner.ID], nil
}

type routineGrantFixtureState struct {
	store     *SQLiteStore
	run       *AgentRun
	catalog   *skill.Catalog
	bound     *skill.BoundAction
	args      map[string]interface{}
	validator *RunbookActionValidator
	owners    routineOwnerAddressesForTest
	// occurrence is the routine occurrence last claimed, still running.
	occurrence *AgentRun
}

func routineGrantFixture(t *testing.T) *routineGrantFixtureState {
	t.Helper()
	store := newTestKernelStore(t)
	run, catalog, bound, args := scheduledTaskFixture(t, store)
	recipients := map[string]interface{}{"type": "string"}
	mail := &skill.Definition{
		ID: "mail", Version: "1.0.0", Name: "Mail", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://mail.invalid"},
		Actions: map[string]skill.Action{
			"send": {Name: "send", Description: "Send an email from your account. Supports attachments.", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationOptional,
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"to": recipients, "cc": recipients, "subject": map[string]interface{}{"type": "string"}, "body": map[string]interface{}{"type": "string"}}}},
			"chat": {Name: "chat", Description: "Post to a chat.", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationOptional,
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"chatId": map[string]interface{}{"type": "string"}, "text": map[string]interface{}{"type": "string"}}}},
			"raw": {Name: "raw", Description: "Send a raw message.", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationOptional,
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"body": map[string]interface{}{"type": "object"}}}},
			"search": {Name: "search", Description: "Search the web.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}},
			"note":   {Name: "note", Description: "Write a note", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden, InputSchema: map[string]interface{}{"type": "object"}},
			"pay": {Name: "pay", Description: "Pay for the checkout", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden,
				InputSchema: map[string]interface{}{"type": "object"}},
		},
	}
	if err := catalog.Register(t.Context(), mail); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "mail-binding", Revision: 1, Scope: bound.Binding.Scope, DeploymentID: run.AssignedAgentID, SkillID: "mail", SkillVersion: "1.0.0", AllowedActions: []string{"send", "chat", "raw", "search", "note", "pay"}, MaximumRisk: skill.RiskLevelExternal}); err != nil {
		t.Fatal(err)
	}
	validator, err := NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	owners := routineOwnerAddressesForTest{"user": {"Kev@Example.com"}}
	validator.SetRoutineGrantCatalog(catalog)
	validator.SetRoutineOwnerAddresses(owners)
	return &routineGrantFixtureState{store: store, run: run, catalog: catalog, bound: bound, args: args, validator: validator, owners: owners}
}

func (f *routineGrantFixtureState) withGrants(grants ...map[string]interface{}) map[string]interface{} {
	args := cloneMap(f.args)
	items := make([]interface{}, 0, len(grants))
	for _, grant := range grants {
		items = append(items, grant)
	}
	args["brief"] = map[string]interface{}{"summary": "Every morning at 8, reads the forecast and emails you a summary.", "deliverTo": "kev@example.com, from your Gmail", "grants": items}
	return args
}

func (f *routineGrantFixtureState) resolve(t *testing.T, args map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	resolved, handled, err := f.validator.ResolveActionProposalArguments(t.Context(), ActionProposalValidationInput{Run: f.run, Bound: f.bound, Arguments: args})
	if !handled {
		t.Fatal("create_task arguments were not resolved")
	}
	return resolved, err
}

// createRoutine proposes, validates and dispatches create_task with the brief.
func (f *routineGrantFixtureState) createRoutine(t *testing.T, grants ...map[string]interface{}) (*RunbookActivation, map[string]interface{}) {
	t.Helper()
	resolved, err := f.resolve(t, f.withGrants(grants...))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.ValidateInput(t.Context(), f.bound, resolved); err != nil {
		t.Fatalf("resolved brief breaks the create_task contract: %v", err)
	}
	preview, err := f.validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: f.run, Bound: f.bound, Arguments: resolved})
	if err != nil {
		t.Fatal(err)
	}
	result := dispatchTaskForTest(t, f.store, f.run, f.bound, resolved, "create-briefed-task")
	return result["activation"].(*RunbookActivation), preview
}

func TestCreateTaskAlwaysAsksForItsBriefInEveryMode(t *testing.T) {
	f := routineGrantFixture(t)
	if action := RunbookManagementSkill().Actions[RunbookActionCreateTask]; !action.AlwaysReview() {
		t.Fatalf("create_task review = %q, want always", action.Review)
	}
	policy := NewDefaultActionPolicy()
	policy.ApprovalTimeout = ApprovalTimeoutApprove
	definition := RunbookManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: f.bound.Binding, Action: definition.Actions[RunbookActionCreateTask]}
	for _, mode := range []ConversationApprovalMode{"", ConversationApprovalManual, ConversationApprovalAuto, ConversationApprovalSkip} {
		decision, err := policy.EvaluateAction(t.Context(), ActionPolicyInput{Run: f.run, Bound: bound, ApprovalMode: mode, RoutineGrant: &RoutineGrant{Action: "openseal.runbooks.create_task", Label: "x"}})
		if err != nil || decision.Disposition != ActionDispositionRequireApproval || decision.ApprovalTimeout != ApprovalTimeoutExpire {
			t.Fatalf("mode %q create_task decision = %#v, %v", mode, decision, err)
		}
	}
}

func TestRoutineBriefValidation(t *testing.T) {
	f := routineGrantFixture(t)
	for name, args := range map[string]map[string]interface{}{
		"missing brief":        func() map[string]interface{} { a := cloneMap(f.args); delete(a, "brief"); return a }(),
		"unknown action":       f.withGrants(map[string]interface{}{"action": "mail.forward"}),
		"uninstalled skill":    f.withGrants(map[string]interface{}{"action": "bank.transfer"}),
		"always reviewed pay":  f.withGrants(map[string]interface{}{"action": "mail.pay"}),
		"kernel management":    f.withGrants(map[string]interface{}{"action": "openseal.runbooks.set_status"}),
		"owner-only no sender": f.withGrants(map[string]interface{}{"action": "mail.note", "toOwnerOnly": true}),
		"bare action name":     f.withGrants(map[string]interface{}{"action": "send"}),
		"duplicate":            f.withGrants(map[string]interface{}{"action": "mail.send"}, map[string]interface{}{"action": "mail.send"}),
		"unknown field":        f.withGrants(map[string]interface{}{"action": "mail.send", "scope": "everyone"}),
		"markup label":         f.withGrants(map[string]interface{}{"action": "mail.send", "label": "<b>Email</b>"}),
		"long summary": func() map[string]interface{} {
			a := f.withGrants()
			a["brief"].(map[string]interface{})["summary"] = strings.Repeat("a", 301)
			return a
		}(),
	} {
		if _, err := f.resolve(t, args); err == nil {
			t.Fatalf("%s: brief was accepted", name)
		}
	}
	// A host that cannot prove the owner's addresses does not offer owner-only sends.
	f.validator.SetRoutineOwnerAddresses(nil)
	if _, err := f.resolve(t, f.withGrants(map[string]interface{}{"action": "mail.send", "toOwnerOnly": true})); err == nil {
		t.Fatal("owner-only grant offered without an owner address resolver")
	}
	f.validator.SetRoutineOwnerAddresses(f.owners)

	resolved, err := f.resolve(t, f.withGrants(
		map[string]interface{}{"action": "mail.search"},
		map[string]interface{}{"action": "mail.send", "label": "  Email   you the summary ", "toOwnerOnly": true},
		map[string]interface{}{"action": "mail.note", "label": "mail.note"},
	))
	if err != nil {
		t.Fatal(err)
	}
	grants := resolved["brief"].(map[string]interface{})["grants"].([]interface{})
	want := []map[string]interface{}{
		{"action": "mail.search", "label": "Search the web"},
		{"action": "mail.send", "label": "Email you the summary", "toOwnerOnly": true},
		{"action": "mail.note", "label": "Write a note"},
	}
	for index, item := range grants {
		got := item.(map[string]interface{})
		for key, value := range want[index] {
			if got[key] != value {
				t.Fatalf("grant %d = %#v, want %#v", index, got, want[index])
			}
		}
	}
}

func TestApprovedBriefGrantsArePersistedOnTheRoutine(t *testing.T) {
	f := routineGrantFixture(t)
	activation, preview := f.createRoutine(t, map[string]interface{}{"action": "mail.search"}, map[string]interface{}{"action": "mail.send", "label": "Email you the summary", "toOwnerOnly": true})
	brief, _ := preview["brief"].(map[string]interface{})
	if preview["operation"] != RunbookActionCreateTask || brief["summary"] != "Every morning at 8, reads the forecast and emails you a summary." || brief["deliverTo"] != "kev@example.com, from your Gmail" || len(brief["grants"].([]interface{})) != 2 {
		t.Fatalf("approval preview = %#v", preview)
	}
	want := []RoutineGrant{{Action: "mail.search", Label: "Search the web"}, {Action: "mail.send", Label: "Email you the summary", ToOwnerOnly: true}}
	stored, err := f.store.GetRunbookActivation(t.Context(), f.run.Scope, activation.ID)
	if err != nil || stored.Task == nil || !reflect.DeepEqual(stored.Task.Grants, want) {
		t.Fatalf("persisted grants = %#v, %v", stored.Task, err)
	}
	// Dispatch reads only the approved arguments: a grant the brief lacks
	// (here, missing labels never filled by the catalog) cannot be stored.
	unresolved := f.withGrants(map[string]interface{}{"action": "mail.note"})
	d, _ := NewRunbookActionDispatcher(f.store, nil)
	if _, err := d.DispatchAction(t.Context(), ActionDispatchInput{Run: f.run, Bound: f.bound, Call: &ActionCall{ID: "unapproved", Scope: f.run.Scope, RunID: f.run.ID, DeploymentID: f.run.AssignedAgentID, IdempotencyKey: "unapproved"}, Arguments: unresolved}); err == nil {
		t.Fatal("dispatch stored a grant without an approved label")
	}
}

// routineOccurrence creates and claims a scheduled occurrence of activation.
// The previous occurrence completes first: a loop call leaves its Run
// running, and the routine's objective admits one running occurrence.
func (f *routineGrantFixtureState) routineOccurrence(t *testing.T, activation *RunbookActivation, key string) *AgentRun {
	t.Helper()
	now := time.Now().UTC()
	f.completeOccurrence(t, now)
	run, err := NewPortfolioService(f.store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: activation.Scope, ObjectiveID: activation.ObjectiveID, Owner: activation.Owner, AssignedAgentID: activation.AssignedAgentID,
		Goal: activation.Task.Goal, Source: RunSourceSchedule, IdempotencyKey: key,
		Context: map[string]interface{}{
			conversationRunContextConversationID: activation.Task.ConversationID, conversationRunContextTriggerID: activation.Task.SourceMessageID,
			scheduledTaskContextKey: true, "runbookActivationId": activation.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 10; attempt++ {
		claimed, err := f.store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: activation.Scope, WorkerID: "worker", Now: now.Add(time.Second), LeaseDuration: time.Hour, AgingInterval: time.Minute})
		if err != nil || claimed == nil {
			t.Fatalf("claim: %#v %v", claimed, err)
		}
		if claimed.ID == run.ID {
			f.occurrence = claimed
			return claimed
		}
	}
	t.Fatal("routine occurrence was not claimed")
	return nil
}

// completeOccurrence ends the occurrence last claimed, as its loop would.
func (f *routineGrantFixtureState) completeOccurrence(t *testing.T, now time.Time) {
	t.Helper()
	if f.occurrence == nil {
		return
	}
	current, err := f.store.GetAgentRun(t.Context(), f.occurrence.Scope, f.occurrence.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.occurrence = nil
	if current.Status != AgentRunStatusRunning {
		return
	}
	completed := cloneAgentRun(current)
	completed.Status, completed.LeaseOwner, completed.LeaseExpiresAt = AgentRunStatusCompleted, "", nil
	completed.Revision++
	completed.UpdatedAt = now
	event := &ActivityEvent{
		ID: "complete-" + current.ID, Scope: current.Scope, EventType: "run.updated", Severity: ActivitySeverityInfo,
		AgentID: current.AssignedAgentID, RunID: current.ID, Actor: ActivityActor{Type: "worker", ID: "worker"},
		Summary: "Routine occurrence completed", Visibility: ActivityVisibilityScope, CreatedAt: now,
	}
	if _, err := f.store.UpdateAgentRunWithEvent(t.Context(), completed, current.Revision, event, nil); err != nil {
		t.Fatalf("complete occurrence %s: %v", current.ID, err)
	}
}

// reviewEverything sets the routine's chat to manual approval.
func (f *routineGrantFixtureState) reviewEverything(t *testing.T, activation *RunbookActivation) {
	t.Helper()
	conversation, err := f.store.GetConversation(t.Context(), f.run.Scope, activation.Task.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewConversationApprovalModeService(f.store).SetApprovalMode(t.Context(), SetConversationApprovalModeRequest{Scope: f.run.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: ConversationApprovalManual, Actor: ActivityActor{Type: "user", ID: "user"}}); err != nil {
		t.Fatal(err)
	}
}

func (f *routineGrantFixtureState) propose(t *testing.T, coordinator *ActionCoordinator, run *AgentRun, action, key string, arguments map[string]interface{}) *ActionProposalResult {
	t.Helper()
	result, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: run.Scope, RunID: run.ID, TurnID: "step-1", WorkerID: "worker", Origin: ActionOriginLoop, Execution: ActionExecutionLoop, Summary: "Run the routine's step.", DeploymentID: run.AssignedAgentID, SkillID: "mail", SkillVersion: "1.0.0", Action: action, Arguments: arguments, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return result
}

func TestRoutineRunsPerformGrantedActionsWithoutAsking(t *testing.T) {
	f := routineGrantFixture(t)
	activation, _ := f.createRoutine(t, map[string]interface{}{"action": "mail.send", "label": "Email you the summary", "toOwnerOnly": true})
	// Manual review in the chat does not stop what the brief approved.
	f.reviewEverything(t, activation)
	coordinator := NewActionCoordinator(f.store, f.store, f.catalog, NewDefaultActionPolicy())
	coordinator.SetRoutineOwnerAddresses(f.owners)

	cases := []struct {
		name, action string
		arguments    map[string]interface{}
		asks         bool
	}{
		{"owner only", "send", map[string]interface{}{"to": "kev@example.com", "subject": "Forecast"}, false},
		{"owner with name and cc", "send", map[string]interface{}{"to": "Kev <KEV@example.com>", "cc": "kev@example.com"}, false},
		{"someone else", "send", map[string]interface{}{"to": "kev@example.com, eve@example.com"}, true},
		{"lookalike", "send", map[string]interface{}{"to": "kev@example.com.evil"}, true},
		{"no recipient", "send", map[string]interface{}{"subject": "Forecast"}, true},
		{"not granted", "note", map[string]interface{}{}, true},
		{"always reviewed", "pay", map[string]interface{}{}, true},
	}
	for index, tc := range cases {
		run := f.routineOccurrence(t, activation, "occurrence-"+tc.name)
		result := f.propose(t, coordinator, run, tc.action, "call-"+string(rune('a'+index)), tc.arguments)
		if asks := result.Approval != nil; asks != tc.asks {
			t.Fatalf("%s: asks=%v want %v (call %#v)", tc.name, asks, tc.asks, result.Call)
		}
	}

	// The same send from the chat itself, not a routine run, still asks.
	chat := f.run
	chat.Context = cloneMap(chat.Context)
	if grant, err := resolveRoutineGrant(t.Context(), f.store, f.store, f.owners, chat, &skill.BoundAction{Definition: &skill.Definition{ID: "mail"}, Action: skill.Action{Name: "send"}}, map[string]interface{}{"to": "kev@example.com"}); err != nil || grant != nil {
		t.Fatalf("chat run used a routine grant: %#v %v", grant, err)
	}
	// Without an owner address resolver, owner-only grants never apply.
	if grant, err := resolveRoutineGrant(t.Context(), f.store, f.store, nil, f.routineOccurrence(t, activation, "no-resolver"), &skill.BoundAction{Definition: &skill.Definition{ID: "mail"}, Action: skill.Action{Name: "send"}}, map[string]interface{}{"to": "kev@example.com"}); err != nil || grant != nil {
		t.Fatalf("owner-only grant applied without owner addresses: %#v %v", grant, err)
	}
}

func TestRoutineGrantWithoutOwnerLimitAndRevoke(t *testing.T) {
	f := routineGrantFixture(t)
	activation, _ := f.createRoutine(t, map[string]interface{}{"action": "mail.note"})
	f.reviewEverything(t, activation)
	coordinator := NewActionCoordinator(f.store, f.store, f.catalog, NewDefaultActionPolicy())
	run := f.routineOccurrence(t, activation, "before-revoke")
	if result := f.propose(t, coordinator, run, "note", "note-before", map[string]interface{}{}); result.Approval != nil {
		t.Fatal("granted note asked")
	}

	service := NewRunbookActivationService(f.store)
	if _, err := service.RevokeGrants(t.Context(), f.run.Scope, activation.ID, RevokeRoutineGrantsRequest{Requester: ConversationParticipant{Type: ConversationParticipantUser, ID: "someone-else"}}); !errors.Is(err, ErrRoutineGrantsNotOwner) {
		t.Fatalf("another user revoked: %v", err)
	}
	owner := RevokeRoutineGrantsRequest{Requester: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}}
	revoked, err := service.RevokeGrants(t.Context(), f.run.Scope, activation.ID, owner)
	if err != nil || revoked.Task.Grants != nil || revoked.Revision != activation.Revision+1 {
		t.Fatalf("revoke = %#v, %v", revoked, err)
	}
	again, err := service.RevokeGrants(t.Context(), f.run.Scope, activation.ID, owner)
	if err != nil || again.Revision != revoked.Revision {
		t.Fatalf("revoke is not idempotent: %#v, %v", again, err)
	}
	if _, err := service.RevokeGrants(t.Context(), f.run.Scope, "missing", owner); !errors.Is(err, ErrRunbookActivationNotFound) {
		t.Fatalf("missing routine: %v", err)
	}
	run = f.routineOccurrence(t, activation, "after-revoke")
	if result := f.propose(t, coordinator, run, "note", "note-after", map[string]interface{}{}); result.Approval == nil {
		t.Fatal("revoked grant still ran without asking")
	}
}

func TestRoutineRecipientsOwned(t *testing.T) {
	owned := []string{"kev@example.com"}
	for name, tc := range map[string]struct {
		arguments map[string]interface{}
		want      bool
	}{
		"to string":         {map[string]interface{}{"to": "kev@example.com"}, true},
		"array":             {map[string]interface{}{"recipients": []interface{}{"kev@example.com"}}, true},
		"object recipients": {map[string]interface{}{"to": []interface{}{map[string]interface{}{"email": "kev@example.com", "name": "Kev"}}}, true},
		"nested other":      {map[string]interface{}{"message": map[string]interface{}{"to": "kev@example.com", "bcc": "eve@example.com"}}, false},
		"unknown subfield":  {map[string]interface{}{"to": map[string]interface{}{"id": "u123"}}, false},
		"non string":        {map[string]interface{}{"to": 7}, false},
		"invalid":           {map[string]interface{}{"to": "not an address"}, false},
		"none":              {map[string]interface{}{"body": "hi"}, false},
	} {
		if got := routineRecipientsOwned(tc.arguments, owned); got != tc.want {
			t.Fatalf("%s: got %v", name, got)
		}
	}
}
