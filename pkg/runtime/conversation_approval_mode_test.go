package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestDefaultActionPolicyApprovalModeMatrix(t *testing.T) {
	classes := []struct {
		name   string
		risk   skill.RiskLevel
		effect skill.SideEffect
	}{
		{"read", skill.RiskLevelRead, skill.SideEffectRead},
		{"write", skill.RiskLevelWrite, skill.SideEffectWrite},
		{"external", skill.RiskLevelExternal, skill.SideEffectExternal},
	}
	want := map[ConversationApprovalMode][3]ActionDisposition{
		"":                         {ActionDispositionAllow, ActionDispositionRequireApproval, ActionDispositionRequireApproval},
		ConversationApprovalManual: {ActionDispositionAllow, ActionDispositionRequireApproval, ActionDispositionRequireApproval},
		ConversationApprovalAuto:   {ActionDispositionAllow, ActionDispositionAllow, ActionDispositionRequireApproval},
		ConversationApprovalSkip:   {ActionDispositionAllow, ActionDispositionAllow, ActionDispositionAllow},
	}
	for mode, expected := range want {
		for index, class := range classes {
			decision, err := NewDefaultActionPolicy().EvaluateAction(context.Background(), ActionPolicyInput{
				ApprovalMode: mode,
				Bound: &skill.BoundAction{
					Definition: &skill.Definition{ID: "work"}, Binding: &skill.Binding{},
					Action: skill.Action{Name: "act", Risk: class.risk, SideEffect: class.effect},
				},
			})
			if err != nil || decision.Disposition != expected[index] {
				t.Errorf("mode %q %s: decision = %#v, error = %v; want %s", mode, class.name, decision, err, expected[index])
			}
		}
	}
}

func TestApprovalModeAutoStillReviewsExternalEffectsAndHigherRisk(t *testing.T) {
	for _, tc := range []struct {
		risk   skill.RiskLevel
		effect skill.SideEffect
	}{
		{skill.RiskLevelWrite, skill.SideEffectExternal},
		{skill.RiskLevelWrite, skill.SideEffectDestructive},
		{skill.RiskLevelProduction, skill.SideEffectWrite},
		{skill.RiskLevelDestructive, skill.SideEffectDestructive},
	} {
		if ApprovalModeAllowsAction(ConversationApprovalAuto, "", tc.risk, tc.effect) {
			t.Errorf("auto allowed %s risk with %s effect", tc.risk, tc.effect)
		}
		if !ApprovalModeAllowsAction(ConversationApprovalSkip, "", tc.risk, tc.effect) {
			t.Errorf("skip required approval for %s risk with %s effect", tc.risk, tc.effect)
		}
	}
}

func TestApprovalModeNeverWaivesAgentBehaviorReview(t *testing.T) {
	definition := AgentManagementSkill()
	for _, name := range []string{AgentActionAmendBehavior, AgentActionConfigureChannel} {
		action := definition.Actions[name]
		if !action.AlwaysReview() {
			t.Fatalf("%s must declare review: always", name)
		}
		decision, err := NewDefaultActionPolicy().EvaluateAction(context.Background(), ActionPolicyInput{
			ApprovalMode: ConversationApprovalSkip,
			Bound:        &skill.BoundAction{Definition: definition, Binding: &skill.Binding{}, Action: action},
		})
		if err != nil || decision.Disposition != ActionDispositionRequireApproval {
			t.Fatalf("%s decision = %#v, error = %v", name, decision, err)
		}
	}
}

func TestAlwaysReviewActionKeepsApprovalInEveryMode(t *testing.T) {
	action := skill.Action{Name: "pay", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways}
	for _, mode := range []ConversationApprovalMode{"", ConversationApprovalManual, ConversationApprovalAuto, ConversationApprovalSkip} {
		if ApprovalModeAllowsAction(mode, action.Review, action.Risk, action.SideEffect) {
			t.Fatalf("mode %q waived an always-reviewed action", mode)
		}
		decision, err := NewDefaultActionPolicy().EvaluateAction(context.Background(), ActionPolicyInput{
			ApprovalMode: mode,
			Bound:        &skill.BoundAction{Definition: &skill.Definition{ID: "skill-live-browser"}, Binding: &skill.Binding{}, Action: action},
		})
		if err != nil || decision.Disposition != ActionDispositionRequireApproval || decision.Reason != "action always requires explicit review" {
			t.Fatalf("mode %q decision = %#v, error = %v", mode, decision, err)
		}
	}
	action.Review = ""
	if !ApprovalModeAllowsAction(ConversationApprovalSkip, action.Review, action.Risk, action.SideEffect) {
		t.Fatal("skip must still allow an ordinary write action")
	}
}

func TestNewConversationsDefaultToAutoApprovalMode(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	conversation := approvalModeConversation(t, store)
	if conversation.ApprovalMode != ConversationApprovalAuto {
		t.Fatalf("new conversation approval mode = %q", conversation.ApprovalMode)
	}
	legacy, err := decodeConversation(`{"id":"legacy","scope":{"kind":"tenant","id":"one"},"owner":{"type":"agent","id":"a"},"title":"t","status":"active","revision":1}`)
	if err != nil || legacy.ApprovalMode != ConversationApprovalAuto {
		t.Fatalf("legacy conversation approval mode = %#v, %v", legacy, err)
	}
	invalid := cloneConversation(conversation)
	invalid.ApprovalMode = "sometimes"
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("invalid approval mode error = %v", err)
	}
	if _, err := NewConversationApprovalModeService(store).SetApprovalMode(ctx, SetConversationApprovalModeRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: "sometimes", Actor: approvalModeActor,
	}); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("invalid approval mode update error = %v", err)
	}
}

func TestSetApprovalModeIsRevisionCheckedAndRecordsActivity(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	conversation := approvalModeConversation(t, store)
	service := NewConversationApprovalModeService(store)
	if _, err := service.SetApprovalMode(ctx, SetConversationApprovalModeRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision + 1,
		Mode: ConversationApprovalManual, Actor: approvalModeActor,
	}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	result, err := service.SetApprovalMode(ctx, SetConversationApprovalModeRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Mode: ConversationApprovalManual, Actor: approvalModeActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Conversation.ApprovalMode != ConversationApprovalManual || result.Conversation.Revision != conversation.Revision+1 {
		t.Fatalf("approval mode update = %#v", result)
	}
	if result.Event == nil || result.Event.EventType != ConversationApprovalModeChangedEvent || result.Event.Actor != approvalModeActor ||
		result.Event.Payload["from"] != "auto" || result.Event.Payload["to"] != "manual" || result.Event.AgentID != "assistant" {
		t.Fatalf("approval mode event = %#v", result.Event)
	}
	events, err := store.ListActivity(ctx, ActivityFilter{Scope: conversation.Scope, EventTypes: []string{ConversationApprovalModeChangedEvent}, Descending: true})
	if err != nil || len(events) != 1 || events[0].ID != result.Event.ID {
		t.Fatalf("approval mode activity = %#v, %v", events, err)
	}
	stored, err := store.GetConversation(ctx, conversation.Scope, conversation.ID)
	if err != nil || stored.ApprovalMode != ConversationApprovalManual {
		t.Fatalf("stored conversation = %#v, %v", stored, err)
	}
	// Setting the current mode again is a no-op that keeps the revision.
	again, err := service.SetApprovalMode(ctx, SetConversationApprovalModeRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: stored.Revision,
		Mode: ConversationApprovalManual, Actor: approvalModeActor,
	})
	if err != nil || again.Changed || again.Event != nil || again.Conversation.Revision != stored.Revision {
		t.Fatalf("repeat approval mode update = %#v, %v", again, err)
	}
}

func TestLooseningApprovalModeApprovesAllowedPendingApprovals(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	conversation := approvalModeConversation(t, store)
	service := NewConversationApprovalModeService(store)
	conversation = setApprovalModeForTest(t, service, conversation, ConversationApprovalManual).Conversation
	catalog := approvalModeCatalog(t)
	coordinator := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy())

	writeApproval := proposeForApprovalMode(t, store, coordinator, conversation, "note")
	externalApproval := proposeForApprovalMode(t, store, coordinator, conversation, "send")
	other := approvalModeConversationWithKey(t, store, "other")
	setApprovalModeForTest(t, service, other, ConversationApprovalManual)
	otherApproval := proposeForApprovalModeInConversation(t, store, coordinator, other.ID, "note", "other-run")

	result := setApprovalModeForTest(t, service, conversation, ConversationApprovalAuto)
	if len(result.ApprovedApprovalIDs) != 1 || result.ApprovedApprovalIDs[0] != writeApproval.ID || result.ApprovalSweepIncomplete {
		t.Fatalf("auto sweep = %#v", result)
	}
	approved, _ := store.GetApproval(ctx, conversation.Scope, writeApproval.ID)
	if approved.Status != ApprovalStatusApproved || approved.DecisionBy == nil || *approved.DecisionBy != ApprovalModePrincipal || approved.DecisionReason != "approval mode auto" {
		t.Fatalf("write approval = %#v", approved)
	}
	call, _ := store.GetActionCall(ctx, conversation.Scope, approved.ActionCallID)
	run, _ := store.GetAgentRun(ctx, conversation.Scope, approved.RunID)
	if call.Status != ActionCallStatusReady || run.Status != AgentRunStatusWaitingForDependency {
		t.Fatalf("approved action did not resume: call=%s run=%s", call.Status, run.Status)
	}
	if pending, _ := store.GetApproval(ctx, conversation.Scope, externalApproval.ID); pending.Status != ApprovalStatusPending {
		t.Fatalf("auto approved an external action: %#v", pending)
	}
	if pending, _ := store.GetApproval(ctx, conversation.Scope, otherApproval.ID); pending.Status != ApprovalStatusPending {
		t.Fatalf("another conversation's approval was resolved: %#v", pending)
	}

	// Re-sending the same mode re-runs the sweep idempotently.
	repeat := setApprovalModeForTest(t, service, result.Conversation, ConversationApprovalAuto)
	if repeat.Changed || len(repeat.ApprovedApprovalIDs) != 0 {
		t.Fatalf("repeat sweep = %#v", repeat)
	}
	// A second sweep racing on the same approval converges on one decision.
	again, err := service.approvals.ResolveByApprovalMode(ctx, conversation.Scope, writeApproval.ID, writeApproval.Revision, ConversationApprovalAuto, "")
	if err != nil || again.Resolved {
		t.Fatalf("racing sweep = %#v, %v", again, err)
	}

	skipped := setApprovalModeForTest(t, service, repeat.Conversation, ConversationApprovalSkip)
	if len(skipped.ApprovedApprovalIDs) != 1 || skipped.ApprovedApprovalIDs[0] != externalApproval.ID {
		t.Fatalf("skip sweep = %#v", skipped)
	}
	if resolved, _ := store.GetApproval(ctx, conversation.Scope, externalApproval.ID); resolved.Status != ApprovalStatusApproved || resolved.DecisionReason != "approval mode skip" {
		t.Fatalf("external approval = %#v", resolved)
	}
}

func TestTighteningApprovalModeLeavesPendingApprovals(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	conversation := approvalModeConversation(t, store)
	service := NewConversationApprovalModeService(store)
	coordinator := NewActionCoordinator(store, store, approvalModeCatalog(t), NewDefaultActionPolicy())
	// Under auto, the write proceeds and the external action waits for review.
	allowed := proposeResultForApprovalMode(t, store, coordinator, conversation.ID, "note", "allowed-run")
	if allowed.Approval != nil || allowed.Call.Status != ActionCallStatusReady {
		t.Fatalf("auto write proposal = %#v", allowed)
	}
	external := proposeForApprovalMode(t, store, coordinator, conversation, "send")
	result := setApprovalModeForTest(t, service, conversation, ConversationApprovalManual)
	if !result.Changed || len(result.ApprovedApprovalIDs) != 0 {
		t.Fatalf("tightening result = %#v", result)
	}
	if pending, _ := store.GetApproval(ctx, conversation.Scope, external.ID); pending.Status != ApprovalStatusPending || pending.Revision != external.Revision {
		t.Fatalf("tightening touched a pending approval: %#v", pending)
	}
	// Manual now applies to new proposals from this conversation.
	if manual := proposeForApprovalMode(t, store, coordinator, result.Conversation, "note"); manual.Status != ApprovalStatusPending {
		t.Fatalf("manual write proposal = %#v", manual)
	}
}

func TestRunConversationIDFallsBackToRootRun(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "one"}
	root := &AgentRun{ID: "root", Scope: scope, RootRunID: "root", Context: map[string]interface{}{"conversationId": "chat"}}
	child := &AgentRun{ID: "child", Scope: scope, RootRunID: "root", ParentRunID: "root", Context: map[string]interface{}{}}
	store.agentRuns[portfolioKey(scope, root.ID)] = root
	if id, err := RunConversationID(ctx, store, child); err != nil || id != "chat" {
		t.Fatalf("child conversation = %q, %v", id, err)
	}
	own := &AgentRun{ID: "own", Scope: scope, RootRunID: "root", Context: map[string]interface{}{"conversationId": "direct"}}
	if id, err := RunConversationID(ctx, store, own); err != nil || id != "direct" {
		t.Fatalf("own conversation = %q, %v", id, err)
	}
	orphan := &AgentRun{ID: "orphan", Scope: scope, RootRunID: "missing"}
	if id, err := RunConversationID(ctx, store, orphan); err != nil || id != "" {
		t.Fatalf("orphan conversation = %q, %v", id, err)
	}
	// A Run acting for no stored conversation has no mode (manual behavior).
	if id, mode, err := RunConversationApprovalMode(ctx, store, store, child); err != nil || id != "chat" || mode != "" {
		t.Fatalf("missing conversation mode = %q %q %v", id, mode, err)
	}
	conversation := approvalModeConversation(t, store)
	root.Context["conversationId"] = conversation.ID
	if _, mode, err := RunConversationApprovalMode(ctx, store, store, child); err != nil || mode != ConversationApprovalAuto {
		t.Fatalf("root conversation mode = %q %v", mode, err)
	}
}

func TestActionCoordinatorPassesConversationApprovalModeToPolicy(t *testing.T) {
	store := NewMemoryStore()
	conversation := approvalModeConversation(t, store)
	var seen ActionPolicyInput
	coordinator := NewActionCoordinator(store, store, approvalModeCatalog(t), ActionPolicyEvaluatorFunc(func(_ context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
		seen = input
		return ActionPolicyDecision{Disposition: ActionDispositionAllow, Reason: "test"}, nil
	}))
	proposeResultForApprovalMode(t, store, coordinator, conversation.ID, "note", "seen-run")
	if seen.ConversationID != conversation.ID || seen.ApprovalMode != ConversationApprovalAuto {
		t.Fatalf("policy input conversation = %q mode = %q", seen.ConversationID, seen.ApprovalMode)
	}
}

var approvalModeActor = ActivityActor{Type: "user", ID: "user-7"}

func approvalModeConversation(t *testing.T, store *MemoryStore) *Conversation {
	return approvalModeConversationWithKey(t, store, "chat")
}

func approvalModeConversationWithKey(t *testing.T, store *MemoryStore, key string) *Conversation {
	t.Helper()
	conversation, _, err := NewConversationService(store).CreateConversation(context.Background(), CreateConversationRequest{
		Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "assistant"}, Title: "Chat " + key, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func setApprovalModeForTest(t *testing.T, service *ConversationApprovalModeService, conversation *Conversation, mode ConversationApprovalMode) *SetConversationApprovalModeResult {
	t.Helper()
	result, err := service.SetApprovalMode(context.Background(), SetConversationApprovalModeRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: mode, Actor: approvalModeActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func approvalModeCatalog(t *testing.T) *skill.Catalog {
	t.Helper()
	ctx := context.Background()
	catalog := skill.NewCatalog()
	object := map[string]interface{}{"type": "object"}
	definition := &skill.Definition{
		ID: "workspace", Version: "1.0.0", Name: "Workspace", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://workspace.invalid"},
		Actions: map[string]skill.Action{
			"note": {Name: "note", Description: "Write a note", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, InputSchema: object, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden},
			"send": {Name: "send", Description: "Send a message", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, InputSchema: object, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationOptional},
			"pay": {Name: "pay", Description: "Pay for the checkout", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways,
				Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden, InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"amount": map[string]interface{}{"type": "string"}, "currency": map[string]interface{}{"type": "string"},
						"merchant": map[string]interface{}{"type": "string"}, "cardToken": map[string]interface{}{"type": "string", "x-sensitive": true},
					},
				}},
		},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "workspace-binding", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "assistant",
		SkillID: "workspace", SkillVersion: "1.0.0", AllowedActions: []string{"note", "send", "pay"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

var approvalModeRunCounter int

func proposeForApprovalMode(t *testing.T, store *MemoryStore, coordinator *ActionCoordinator, conversation *Conversation, action string) *ApprovalCheckpoint {
	t.Helper()
	approvalModeRunCounter++
	return proposeForApprovalModeInConversation(t, store, coordinator, conversation.ID, action, fmt.Sprintf("run-%s-%d", action, approvalModeRunCounter))
}

func proposeForApprovalModeInConversation(t *testing.T, store *MemoryStore, coordinator *ActionCoordinator, conversationID, action, key string) *ApprovalCheckpoint {
	t.Helper()
	result := proposeResultForApprovalMode(t, store, coordinator, conversationID, action, key)
	if result.Approval == nil {
		t.Fatalf("%s proposal did not wait for approval: %#v", action, result.Call)
	}
	return result.Approval
}

func proposeResultForApprovalMode(t *testing.T, store *MemoryStore, coordinator *ActionCoordinator, conversationID, action, key string) *ActionProposalResult {
	t.Helper()
	result, err := proposeWithArgumentsForApprovalMode(t, store, coordinator, conversationID, action, key, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func proposeWithArgumentsForApprovalMode(t *testing.T, store *MemoryStore, coordinator *ActionCoordinator, conversationID, action, key string, arguments map[string]interface{}) (*ActionProposalResult, error) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	now := time.Now().UTC()
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "assistant"}, AssignedAgentID: "assistant", Goal: key, Source: RunSourceObjective,
		Context: map[string]interface{}{"conversationId": conversationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("run was not claimed: %#v %v", claimed, err)
	}
	result, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "assistant",
		SkillID: "workspace", SkillVersion: "1.0.0", Action: action, Arguments: arguments, IdempotencyKey: key,
	})
	return result, err
}

func TestSQLiteApprovalModeBackfillsLegacyConversationsAndCommitsEvent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "approval-mode.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "one"}
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "assistant"}, Title: "Chat", IdempotencyKey: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE conversations SET payload = json_remove(payload, '$.approvalMode')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var stored string
	if err := store.db.QueryRow(`SELECT json_extract(payload, '$.approvalMode') FROM conversations`).Scan(&stored); err != nil || stored != "auto" {
		t.Fatalf("backfilled approval mode = %q, %v", stored, err)
	}
	result, err := NewConversationApprovalModeService(store).SetApprovalMode(ctx, SetConversationApprovalModeRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: ConversationApprovalSkip, Actor: approvalModeActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.GetConversation(ctx, scope, conversation.ID)
	if err != nil || reloaded.ApprovalMode != ConversationApprovalSkip || reloaded.Revision != conversation.Revision+1 {
		t.Fatalf("reloaded conversation = %#v, %v", reloaded, err)
	}
	events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: "conversation:" + conversation.ID})
	if err != nil || len(events) != 1 || events[0].ID != result.Event.ID || events[0].Payload["to"] != "skip" {
		t.Fatalf("approval mode events = %#v, %v", events, err)
	}
	if _, err := NewConversationApprovalModeService(store).SetApprovalMode(ctx, SetConversationApprovalModeRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: ConversationApprovalManual, Actor: approvalModeActor,
	}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
}

func TestApprovalModeChangeAppearsInConversationChangeFeed(t *testing.T) {
	ctx := context.Background()
	for name, store := range map[string]interface {
		ConversationApprovalModeKernelStore
		RunActivityStore
	}{"memory": NewMemoryStore(), "sqlite": mustSQLiteApprovalModeStore(t)} {
		t.Run(name, func(t *testing.T) {
			conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
				Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "assistant"}, Title: "Chat", IdempotencyKey: "chat",
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := NewConversationApprovalModeService(store).SetApprovalMode(ctx, SetConversationApprovalModeRequest{
				Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: ConversationApprovalSkip, Actor: approvalModeActor,
			})
			if err != nil {
				t.Fatal(err)
			}
			changes, err := NewConversationChangeService(store, store)
			if err != nil {
				t.Fatal(err)
			}
			set, err := changes.ListChanges(ctx, ConversationChangeRequest{Scope: conversation.Scope, ConversationID: conversation.ID})
			if err != nil {
				t.Fatal(err)
			}
			if set.Conversation.ApprovalMode != ConversationApprovalSkip || len(set.Activity) != 1 || set.Activity[0].ID != result.Event.ID ||
				set.Activity[0].EventType != ConversationApprovalModeChangedEvent || set.Activity[0].Payload["from"] != "auto" || set.Activity[0].Payload["to"] != "skip" {
				t.Fatalf("change set = %#v", set)
			}
		})
	}
}

func mustSQLiteApprovalModeStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "changes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestApprovalWithoutReviewContextIsNamedByActionIntent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	conversation := approvalModeConversation(t, store)
	coordinator := NewActionCoordinator(store, store, approvalModeCatalog(t), NewDefaultActionPolicy())
	scope := conversation.Scope
	now := time.Now().UTC()
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "assistant"}, AssignedAgentID: "assistant", Goal: "search", Source: RunSourceObjective,
		Context: map[string]interface{}{"conversationId": conversation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "assistant", SkillID: "workspace", SkillVersion: "1.0.0", Action: "send",
		Arguments: map[string]interface{}{"intent": "Click 'Search' on google.com", "url": "https://www.google.com/"}, IdempotencyKey: "search",
	})
	if err != nil || result.Approval == nil {
		t.Fatalf("proposal = %#v, %v", result, err)
	}
	review, _ := result.Approval.ProposedAction["reviewContext"].(map[string]interface{})
	if review["summary"] != "Click 'Search' on google.com" || review["target"] != "https://www.google.com/" {
		t.Fatalf("review context = %#v", result.Approval.ProposedAction)
	}
}

func setFixtureApprovalMode(t *testing.T, store *MemoryStore, conversation *Conversation, mode ConversationApprovalMode) {
	t.Helper()
	current, err := store.GetConversation(t.Context(), conversation.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewConversationApprovalModeService(store).SetApprovalMode(t.Context(), SetConversationApprovalModeRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision, Mode: mode, Actor: approvalModeActor,
	}); err != nil {
		t.Fatal(err)
	}
}

func assertHostedApprovalModeHint(t *testing.T, input TurnExecutionContext, agentID string, want ConversationApprovalMode) {
	t.Helper()
	runner, err := NewHostedTurnRunner(&recordingTurnHost{}, HostedTurnRunnerConfig{AgentID: agentID, DefinitionID: "browser", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := runner.buildRequest(input)
	if err != nil || request.ConversationApprovalMode != want {
		t.Fatalf("hosted approval mode = %q want %q err=%v", request.ConversationApprovalMode, want, err)
	}
	request.ConversationApprovalMode = ConversationApprovalSkip
	raw, err := MarshalHostedTurnModelInput(request)
	var modelInput map[string]interface{}
	if err != nil || json.Unmarshal(raw, &modelInput) != nil || modelInput["conversationApprovalMode"] != nil {
		t.Fatalf("host-only approval mode leaked into model input: %s %v", raw, err)
	}
}

func TestHostedTurnRequestCarriesConversationApprovalMode(t *testing.T) {
	store := NewMemoryStore()
	f := newForegroundClarificationFixture(t, store, false)
	setFixtureApprovalMode(t, store, f.conversation, ConversationApprovalSkip)
	wrapper := conversationWorkTurnRunner{runs: store, conversations: store}

	input, err := wrapper.input(t.Context(), TurnExecutionContext{Run: f.run, Turn: &AgentTurn{ID: "skip-turn"}, approvalMode: ConversationApprovalManual})
	if err != nil {
		t.Fatal(err)
	}
	assertHostedApprovalModeHint(t, input, f.run.AssignedAgentID, ConversationApprovalSkip)

	setFixtureApprovalMode(t, store, f.conversation, ConversationApprovalManual)
	input, err = wrapper.input(t.Context(), TurnExecutionContext{Run: f.run, Turn: &AgentTurn{ID: "manual-turn"}})
	if err != nil {
		t.Fatal(err)
	}
	assertHostedApprovalModeHint(t, input, f.run.AssignedAgentID, ConversationApprovalManual)

	// A Run that acts for no conversation carries no mode, even when a stale
	// value arrives on the execution context.
	detached := &AgentRun{ID: "detached", Kind: RunKindAgentWork, Scope: f.run.Scope, AssignedAgentID: "browser-agent",
		Context: map[string]interface{}{"conversationApprovalMode": "skip"}}
	input, err = wrapper.input(t.Context(), TurnExecutionContext{Run: detached, Turn: &AgentTurn{ID: "detached-turn"}, approvalMode: ConversationApprovalSkip})
	if err != nil {
		t.Fatal(err)
	}
	assertHostedApprovalModeHint(t, input, "browser-agent", "")
}

func TestForegroundConversationTurnCarriesApprovalMode(t *testing.T) {
	store := NewMemoryStore()
	f := newForegroundClarificationFixture(t, store, false)
	setFixtureApprovalMode(t, store, f.conversation, ConversationApprovalSkip)
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: f.run.Scope, Kind: RunKindConversation, WorkerID: "foreground-worker",
		Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != f.run.ID {
		t.Fatalf("claim foreground: %#v %v", claimed, err)
	}
	var captured *TurnExecutionContext
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: f.run.AssignedAgentID, DefinitionID: "browser", DefinitionVersion: "1",
			Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
				captured = &input
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Done.", RunOutput: map[string]interface{}{"summary": "Done."}}, nil
			})}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, f.service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: f.run.Scope, RunID: f.run.ID, WorkerID: "foreground-worker", DefinitionID: "browser", DefinitionVersion: "1",
	}, runner); err != nil {
		t.Fatal(err)
	}
	if captured == nil {
		t.Fatal("foreground adapter did not invoke the hosted runner")
	}
	assertHostedApprovalModeHint(t, *captured, captured.Run.AssignedAgentID, ConversationApprovalSkip)
}

func TestAlwaysReviewActionApprovalShowsInputsAndSurvivesSkip(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	conversation := approvalModeConversation(t, store)
	service := NewConversationApprovalModeService(store)
	conversation = setApprovalModeForTest(t, service, conversation, ConversationApprovalSkip).Conversation
	policy := NewDefaultActionPolicy()
	policy.ApprovalTimeout = ApprovalTimeoutApprove
	coordinator := NewActionCoordinator(store, store, approvalModeCatalog(t), policy)

	arguments := map[string]interface{}{"amount": "1499.00", "currency": "INR", "merchant": "https://www.amazon.in", "cardToken": "tok-secret"}
	result, err := proposeWithArgumentsForApprovalMode(t, store, coordinator, conversation.ID, "pay", "pay-run", arguments)
	if err != nil || result.Approval == nil {
		t.Fatalf("skip mode waived an always-reviewed action: %#v %v", result, err)
	}
	approval := result.Approval
	if approval.TimeoutDecision != ApprovalTimeoutExpire || result.Call.Review != skill.ActionReviewAlways {
		t.Fatalf("always-reviewed approval may auto-approve: timeout=%q review=%q", approval.TimeoutDecision, result.Call.Review)
	}
	shown, _ := approval.ProposedAction["arguments"].(map[string]interface{})
	if approval.ProposedAction["review"] != "always" || shown["amount"] != "1499.00" || shown["currency"] != "INR" ||
		shown["merchant"] != "https://www.amazon.in" || shown["cardToken"] != "[REDACTED]" {
		t.Fatalf("approval card inputs = %#v", approval.ProposedAction)
	}

	// Re-entering skip re-runs the sweep, which must leave the checkpoint pending.
	conversation = setApprovalModeForTest(t, service, conversation, ConversationApprovalManual).Conversation
	swept := setApprovalModeForTest(t, service, conversation, ConversationApprovalSkip)
	if len(swept.ApprovedApprovalIDs) != 0 {
		t.Fatalf("skip sweep approved an always-reviewed action: %#v", swept.ApprovedApprovalIDs)
	}
	if pending, _ := store.GetApproval(ctx, conversation.Scope, approval.ID); pending.Status != ApprovalStatusPending {
		t.Fatalf("always-reviewed approval = %#v", pending)
	}

	permissive := NewActionCoordinator(store, store, approvalModeCatalog(t), ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow, Reason: "test"}, nil
	}))
	if _, err := proposeWithArgumentsForApprovalMode(t, store, permissive, conversation.ID, "pay", "pay-allowed", arguments); err == nil {
		t.Fatal("a policy allowed an always-reviewed action without review")
	}
}
