package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
)

// fakeGovernedCards serves card requests by id, without the store's
// validation, for the card checks of governedParker.
type fakeGovernedCards struct {
	setups      map[string]*models.SkillSetupRequest
	credentials map[string]*models.CredentialRequest
}

func newFakeGovernedCards() *fakeGovernedCards {
	return &fakeGovernedCards{setups: map[string]*models.SkillSetupRequest{}, credentials: map[string]*models.CredentialRequest{}}
}

func (f *fakeGovernedCards) GetSkillSetupRequest(_ context.Context, _ models.Scope, id string) (*models.SkillSetupRequest, error) {
	if r := f.setups[id]; r != nil {
		copied := *r
		return &copied, nil
	}
	return nil, nil
}
func (f *fakeGovernedCards) ListSkillSetupRequests(context.Context, models.Scope, string, string) ([]*models.SkillSetupRequest, error) {
	return nil, nil
}
func (f *fakeGovernedCards) SaveSkillSetupRequest(context.Context, *models.SkillSetupRequest, int64) error {
	return nil
}
func (f *fakeGovernedCards) GetCredentialRequest(_ context.Context, _ models.Scope, id string) (*models.CredentialRequest, error) {
	if r := f.credentials[id]; r != nil {
		copied := *r
		return &copied, nil
	}
	return nil, nil
}
func (f *fakeGovernedCards) ListCredentialRequests(context.Context, models.Scope, string, string) ([]*models.CredentialRequest, error) {
	return nil, nil
}
func (f *fakeGovernedCards) SaveCredentialRequest(context.Context, *models.CredentialRequest, int64) error {
	return nil
}

// fakeGovernedActions serves action calls by id; every other ActionStore
// method is unused by these tests.
type fakeGovernedActions struct {
	models.ActionStore
	calls map[string]*models.ActionCall
}

func (f *fakeGovernedActions) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	if call := f.calls[id]; call != nil {
		return call, nil
	}
	return nil, models.ErrActionNotFound
}

var governedCardScope = models.Scope{Kind: "tenant", ID: "cards"}

func governedCardRun() *models.AgentRun {
	return &models.AgentRun{ID: "run-1", Scope: governedCardScope, Status: models.AgentRunStatusRunning}
}

func governedCardCall(id, action string, status models.ActionCallStatus, output map[string]interface{}) *models.ActionCall {
	return &models.ActionCall{
		ID: id, Scope: governedCardScope, RunID: "run-1", TurnID: "step-1", SkillID: models.SkillManagementSkillID, SkillVersion: "1.0.0",
		Action: action, Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop, Status: status, Output: output,
	}
}

func credentialCardCall(id string) *models.ActionCall {
	return governedCardCall(id, agentrun.SkillActionRequestCredential, models.ActionCallStatusSucceeded,
		map[string]interface{}{"credentialRequest": map[string]interface{}{"id": "credential-" + id}})
}

func connectAppCardCall(id string) *models.ActionCall {
	return governedCardCall(id, agentrun.SkillActionRequestConnection, models.ActionCallStatusSucceeded,
		map[string]interface{}{"setupRequest": map[string]interface{}{"id": "setup-" + id}})
}

func connectAccountCardCall(id string) *models.ActionCall {
	call := governedCardCall(id, "send", models.ActionCallStatusFailed,
		map[string]interface{}{"setupRequest": map[string]interface{}{"kind": models.SkillSetupKindConnectAccount, "id": skillbinding.ConnectAccountRequestPrefix + id}})
	call.SkillID, call.FailurePhase, call.Error = "mail", models.ActionFailureBeforeDispatch, "a sign-in is needed"
	return call
}

func humanCardCall(id string) *models.ActionCall {
	call := governedCardCall(id, "open_page", models.ActionCallStatusSucceeded, map[string]interface{}{"requiresHuman": true})
	call.SkillID = "browser"
	return call
}

func (f *fakeGovernedCards) addCredential(call *models.ActionCall, status string) {
	f.credentials["credential-"+call.ID] = &models.CredentialRequest{
		ID: "credential-" + call.ID, Scope: governedCardScope, RunID: call.RunID, ActionCallID: call.ID,
		Kind: models.CredentialRequestKindPaymentCard, Status: status, Revision: 1,
	}
}

func (f *fakeGovernedCards) addConnectApp(call *models.ActionCall, status string) {
	f.setups["setup-"+call.ID] = &models.SkillSetupRequest{
		ID: "setup-" + call.ID, Scope: governedCardScope, RunID: call.RunID, ActionCallID: call.ID,
		Kind: models.SkillSetupKindConnectApp, SkillName: "Linear", Status: status, Revision: 1,
	}
}

func (f *fakeGovernedCards) addConnectAccount(call *models.ActionCall, status string) {
	f.setups[skillbinding.ConnectAccountRequestPrefix+call.ID] = &models.SkillSetupRequest{
		ID: skillbinding.ConnectAccountRequestPrefix + call.ID, Scope: governedCardScope, RunID: call.RunID, ActionCallID: call.ID,
		Kind: models.SkillSetupKindConnectAccount, SkillName: "Gmail", Mode: models.SkillSetupModePerPerson, Status: status, Revision: 1,
	}
}

func cardParker(cards *fakeGovernedCards, calls ...*models.ActionCall) governedParker {
	return cardParkerAt(time.Now, cards, calls...)
}

// cardParkerAt is cardParker on the clock now.
func cardParkerAt(now func() time.Time, cards *fakeGovernedCards, calls ...*models.ActionCall) governedParker {
	actions := &fakeGovernedActions{calls: map[string]*models.ActionCall{}}
	for _, call := range calls {
		actions.calls[call.ID] = call
	}
	return governedParker{actions: actions, setups: cards, credentials: cards, now: now}
}

func requireCardWait(t *testing.T, p governedParker, run *models.AgentRun, call *models.ActionCall, wantType, wantReference, wantSummary string) {
	t.Helper()
	wake, summary, err := p.governedCardWait(context.Background(), run, call)
	if err != nil {
		t.Fatal(err)
	}
	if wantType == "" {
		if wake != nil {
			t.Fatalf("wake = %#v; want none", wake)
		}
		return
	}
	if wake == nil || wake.Type != wantType || wake.Reference != wantReference || summary != wantSummary {
		t.Fatalf("wake = %#v %q; want %s %s %q", wake, summary, wantType, wantReference, wantSummary)
	}
}

func TestCardWaitHumanIntervention(t *testing.T) {
	call := humanCardCall("call-1")
	run := governedCardRun()
	run.HumanInterventions = []models.HumanInterventionRequest{
		{ID: "other", ActionCallID: "call-0", Status: models.HumanInterventionStatusPending, Summary: "other"},
		{ID: "human-1", ActionCallID: call.ID, Status: models.HumanInterventionStatusPending, Summary: "Solve the captcha"},
	}
	p := cardParker(newFakeGovernedCards())
	requireCardWait(t, p, run, call, governedHumanInterventionWakeType, "human-1", "Solve the captcha")

	run.HumanInterventions[1].Status = models.HumanInterventionStatusResolved
	requireCardWait(t, p, run, call, "", "", "")
	run.HumanInterventions[1].Status = models.HumanInterventionStatusPending
	call.Output["requiresHuman"] = false
	requireCardWait(t, p, run, call, "", "", "")
}

func TestCardWaitCredentialRequest(t *testing.T) {
	cards := newFakeGovernedCards()
	call := credentialCardCall("call-1")
	cards.addCredential(call, models.CredentialRequestStatusPending)
	p := cardParker(cards)
	requireCardWait(t, p, governedCardRun(), call, skillbinding.CredentialRequestWakeType, "credential-call-1", "Waiting for the user to save a requested credential")

	cards.credentials["credential-call-1"].Status = models.CredentialRequestStatusResolved
	requireCardWait(t, p, governedCardRun(), call, "", "", "")
}

func TestCardWaitConnectApp(t *testing.T) {
	cards := newFakeGovernedCards()
	call := connectAppCardCall("call-1")
	cards.addConnectApp(call, "pending")
	p := cardParker(cards)
	requireCardWait(t, p, governedCardRun(), call, skillbinding.ConnectAppWakeType, "setup-call-1", "Waiting for the user to connect Linear")

	cards.setups["setup-call-1"].Status = "dismissed"
	requireCardWait(t, p, governedCardRun(), call, "", "", "")
}

func TestCardWaitConnectAccount(t *testing.T) {
	cards := newFakeGovernedCards()
	call := connectAccountCardCall("call-1")
	cards.addConnectAccount(call, "pending")
	p := cardParker(cards)
	requireCardWait(t, p, governedCardRun(), call, models.ConnectAccountWakeType, skillbinding.ConnectAccountRequestPrefix+"call-1", "Waiting for the person asking to connect Gmail")

	cards.setups[skillbinding.ConnectAccountRequestPrefix+"call-1"].Status = "resolved"
	requireCardWait(t, p, governedCardRun(), call, "", "", "")
}

func TestRefusedInteractionDoesNotPark(t *testing.T) {
	cards := newFakeGovernedCards()
	call := credentialCardCall("call-1")
	call.Output["refused"] = map[string]interface{}{"message": "No chat to show the card in."}
	cards.addCredential(call, models.CredentialRequestStatusPending)
	requireCardWait(t, cardParker(cards), governedCardRun(), call, "", "", "")
}

func TestAlreadyConnectedAppDoesNotPark(t *testing.T) {
	cards := newFakeGovernedCards()
	call := governedCardCall("call-1", agentrun.SkillActionRequestConnection, models.ActionCallStatusSucceeded, map[string]interface{}{
		"connected": map[string]interface{}{"tools": []interface{}{"linear_search"}, "instruction": "Linear is already connected."},
	})
	requireCardWait(t, cardParker(cards), governedCardRun(), call, "", "", "")
}

func TestPlanParkApprovalBeforeCardFirstInOrder(t *testing.T) {
	ctx := context.Background()
	p := cardParker(newFakeGovernedCards())
	run := governedCardRun()
	card := func(id string) GovernedToolOutcome {
		return governedCardOutcome("tool-"+id, &models.ActionCall{ID: id}, &models.WakeCondition{Type: skillbinding.CredentialRequestWakeType, Reference: "credential-" + id}, "card "+id)
	}
	approval := func(id string) GovernedToolOutcome {
		return governedApprovalOutcome("tool-"+id, &models.ActionCall{ID: id}, &models.ApprovalCheckpoint{ID: "approval-" + id, Summary: "send " + id})
	}
	completed := GovernedToolOutcome{ToolCallID: "tool-done", State: models.ToolEntryCompleted, ActionCallID: "done"}

	plan, err := p.PlanPark(ctx, run, []GovernedToolOutcome{completed, card("c1"), approval("a1"), approval("a2"), card("c2")})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Settled || len(plan.Replaced) != 0 || plan.Park == nil || plan.Park.Status != models.AgentRunStatusWaitingForApproval ||
		plan.Park.Wake.Type != "approval" || plan.Park.Wake.Reference != "approval-a1" || plan.Park.Summary != "Waiting for approval: send a1" {
		t.Fatalf("plan = %#v park %#v", plan, plan.Park)
	}

	plan, err = p.PlanPark(ctx, run, []GovernedToolOutcome{completed, card("c1"), card("c2")})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Park == nil || plan.Park.Status != models.AgentRunStatusWaitingForEvent || plan.Park.Wake.Reference != "credential-c1" || plan.Park.Summary != "card c1" {
		t.Fatalf("card plan = %#v", plan.Park)
	}

	retryAt := time.Now().Add(time.Minute)
	plan, err = p.PlanPark(ctx, run, []GovernedToolOutcome{completed, governedMaintenanceOutcome("tool-m", &models.ActionCall{ID: "m"}, retryAt)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Park == nil || plan.Park.Status != models.AgentRunStatusSleeping || plan.Park.Wake.Type != "timer" || !plan.Park.Wake.WakeAt.Equal(retryAt) {
		t.Fatalf("maintenance plan = %#v", plan.Park)
	}

	plan, err = p.PlanPark(ctx, run, []GovernedToolOutcome{completed})
	if err != nil || plan.Park != nil || plan.Settled {
		t.Fatalf("nothing waits: plan = %#v, %v", plan, err)
	}
}

// withSteeringMessage returns run with an unconsumed conversation message
// delivered at at.
func withSteeringMessage(run *models.AgentRun, messageID string, at time.Time) *models.AgentRun {
	steered := models.CloneAgentRun(run)
	steered.PendingInterventions = append(steered.PendingInterventions,
		models.AgentRunIntervention{ID: "consumed", Instruction: "old", CreatedAt: at.Add(-time.Hour),
			ConversationMessage: &models.ConversationMessageDelivery{MessageID: "old-message"}},
		models.AgentRunIntervention{ID: "card", Instruction: "card result", CreatedAt: at.Add(time.Hour), ActionCallID: "some-call",
			ConversationMessage: &models.ConversationMessageDelivery{MessageID: "card-message"}},
		models.AgentRunIntervention{ID: "message:" + messageID, Instruction: "Actually, stop", CreatedAt: at,
			ConversationMessage: &models.ConversationMessageDelivery{MessageID: messageID}},
	)
	steered.ConsumedInterventions = len(run.PendingInterventions) + 1
	return steered
}

func requireSuperseded(t *testing.T, f *actiontest.LoopStore, outcome GovernedToolOutcome, toolCallID, approvalID, messageID string) {
	t.Helper()
	if outcome.ToolCallID != toolCallID || outcome.State != models.ToolEntryError || outcome.Result == nil || outcome.Result.Status != models.ToolResultSuperseded {
		t.Fatalf("replaced = %#v", outcome)
	}
	stored, err := f.Store.GetApproval(f.Ctx, f.Scope, approvalID)
	if err != nil || stored.Status != models.ApprovalStatusSuperseded || stored.SupersededByMessageID != messageID {
		t.Fatalf("approval %s = %#v, %v", approvalID, stored, err)
	}
}

func TestPlanParkSupersedesHeldApprovalsOnNewMessage(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		firstCall, first := f.ProposeLoopApproval("first", actiontest.LoopApprovalOptions{})
		secondCall, second := f.ProposeLoopApproval("second", actiontest.LoopApprovalOptions{})
		at := f.Now.Add(time.Second)
		p := newGovernedParker(store, f.ApprovalCoordinator(at), func() time.Time { return at })
		before := f.CurrentRun()
		run := withSteeringMessage(before, "message-1", at)
		cardCall := &models.ActionCall{ID: "card"}
		outcomes := []GovernedToolOutcome{
			governedApprovalOutcome("tool-first", firstCall, first),
			governedCardOutcome("tool-card", cardCall, &models.WakeCondition{Type: skillbinding.ConnectAppWakeType, Reference: "setup-card"}, "Waiting for the user to connect Linear"),
			governedApprovalOutcome("tool-second", secondCall, second),
		}
		plan, err := p.PlanPark(f.Ctx, run, outcomes)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Settled || len(plan.Replaced) != 2 {
			t.Fatalf("plan = %#v", plan)
		}
		requireSuperseded(t, f, plan.Replaced[0], "tool-first", first.ID, "message-1")
		requireSuperseded(t, f, plan.Replaced[1], "tool-second", second.ID, "message-1")
		if plan.Park == nil || plan.Park.Wake.Reference != "setup-card" {
			t.Fatalf("park = %#v; want the card", plan.Park)
		}
		after := f.CurrentRun()
		if plan.Run == nil || plan.Run.Revision != after.Revision || after.Revision != before.Revision+2 || after.Status != models.AgentRunStatusRunning {
			t.Fatalf("plan Run = %#v, stored = %#v", plan.Run, after)
		}
	})
}

func TestPlanParkKeepsApprovalCreatedAfterMessage(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		call, approval := f.ProposeLoopApproval("later", actiontest.LoopApprovalOptions{})
		p := newGovernedParker(store, f.ApprovalCoordinator(f.Now), nil)
		run := withSteeringMessage(f.CurrentRun(), "message-1", f.Now) // not before: same instant
		plan, err := p.PlanPark(f.Ctx, run, []GovernedToolOutcome{governedApprovalOutcome("tool-later", call, approval)})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Replaced) != 0 || plan.Settled || plan.Park == nil || plan.Park.Wake.Reference != approval.ID || plan.Run != nil {
			t.Fatalf("plan = %#v", plan)
		}
	})
}

func awaitingApprovalEntry(toolCallID string, call *models.ActionCall, approvalID string) *models.SessionEntry {
	return &models.SessionEntry{
		Type: models.SessionEntryTool, State: models.ToolEntryAwaitingApproval, ToolCallID: toolCallID, ActionCallID: call.ID,
		Tool: &models.ToolEntry{Name: "repo_push", Wait: &models.ToolEntryWait{ApprovalID: approvalID}},
	}
}

func awaitingEventEntry(toolCallID, actionCallID string, wait models.ToolEntryWait) *models.SessionEntry {
	return &models.SessionEntry{
		Type: models.SessionEntryTool, State: models.ToolEntryAwaitingEvent, ToolCallID: toolCallID, ActionCallID: actionCallID,
		Tool: &models.ToolEntry{Name: "card", Wait: &wait},
	}
}

func TestReplanParkSupersedesHeldApprovalOnNewMessage(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		call, approval := f.ProposeLoopApproval("held", actiontest.LoopApprovalOptions{})
		at := f.Now.Add(time.Second)
		p := newGovernedParker(store, f.ApprovalCoordinator(at), func() time.Time { return at })
		// A host-tool entry with no action call is not the governed executor's.
		other := &models.SessionEntry{Type: models.SessionEntryTool, State: models.ToolEntryAwaitingEvent, ToolCallID: "tool-host", Tool: &models.ToolEntry{Name: "host", Wait: &models.ToolEntryWait{EventWaitID: "x"}}}
		run := withSteeringMessage(f.CurrentRun(), "message-1", at)
		plan, err := p.ReplanPark(f.Ctx, run, []*models.SessionEntry{other, awaitingApprovalEntry("tool-held", call, approval.ID)})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Settled || plan.Park != nil || len(plan.Replaced) != 1 || plan.Run == nil {
			t.Fatalf("plan = %#v", plan)
		}
		requireSuperseded(t, f, plan.Replaced[0], "tool-held", approval.ID, "message-1")
		stored, err := store.GetActionCall(f.Ctx, f.Scope, call.ID)
		if err != nil || stored.Status != models.ActionCallStatusDenied {
			t.Fatalf("call = %#v, %v", stored, err)
		}
	})
}

func TestReplanParkNilWhenApprovalDecided(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		firstCall, first := f.ProposeLoopApproval("first", actiontest.LoopApprovalOptions{})
		secondCall, second := f.ProposeLoopApproval("second", actiontest.LoopApprovalOptions{})
		at := f.Now.Add(time.Second)
		coordinator := f.ApprovalCoordinator(at)
		p := newGovernedParker(store, coordinator, func() time.Time { return at })
		entries := []*models.SessionEntry{awaitingApprovalEntry("tool-first", firstCall, first.ID), awaitingApprovalEntry("tool-second", secondCall, second.ID)}

		plan, err := p.ReplanPark(f.Ctx, f.CurrentRun(), entries)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Settled || plan.Park == nil || plan.Park.Wake.Reference != first.ID || plan.Park.Summary != "Waiting for approval: "+first.Summary {
			t.Fatalf("pending plan = %#v", plan)
		}

		if _, err := f.Decide(coordinator, second, models.ApprovalDecisionApprove, ""); err != nil {
			t.Fatal(err)
		}
		plan, err = p.ReplanPark(f.Ctx, f.CurrentRun(), entries)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Settled || plan.Park != nil || len(plan.Replaced) != 0 {
			t.Fatalf("decided plan = %#v", plan)
		}
	})
}

func TestReplanParkNilWhenCardSettled(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	type card struct {
		name   string
		call   *models.ActionCall
		add    func(*fakeGovernedCards, *models.ActionCall, string)
		settle func(*fakeGovernedCards, *models.ActionCall, *models.AgentRun)
		wake   string
	}
	cards := []card{
		{name: "credential request", call: credentialCardCall("call-1"), add: (*fakeGovernedCards).addCredential, wake: skillbinding.CredentialRequestWakeType,
			settle: func(f *fakeGovernedCards, c *models.ActionCall, _ *models.AgentRun) {
				f.credentials["credential-"+c.ID].Status = models.CredentialRequestStatusResolved
			}},
		{name: "connect app", call: connectAppCardCall("call-1"), add: (*fakeGovernedCards).addConnectApp, wake: skillbinding.ConnectAppWakeType,
			settle: func(f *fakeGovernedCards, c *models.ActionCall, _ *models.AgentRun) {
				f.setups["setup-"+c.ID].Status = "resolved"
			}},
		{name: "connect account", call: connectAccountCardCall("call-1"), add: (*fakeGovernedCards).addConnectAccount, wake: models.ConnectAccountWakeType,
			settle: func(f *fakeGovernedCards, c *models.ActionCall, _ *models.AgentRun) {
				f.setups[skillbinding.ConnectAccountRequestPrefix+c.ID].Status = "dismissed"
			}},
		{name: "human intervention", call: humanCardCall("call-1"), wake: governedHumanInterventionWakeType,
			add: func(*fakeGovernedCards, *models.ActionCall, string) {},
			settle: func(_ *fakeGovernedCards, _ *models.ActionCall, run *models.AgentRun) {
				run.HumanInterventions[0].Status = models.HumanInterventionStatusResolved
			}},
	}
	for _, c := range cards {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeGovernedCards()
			c.add(fake, c.call, "pending")
			run := governedCardRun()
			run.HumanInterventions = []models.HumanInterventionRequest{{ID: "human-1", ActionCallID: c.call.ID, Status: models.HumanInterventionStatusPending, Summary: "Solve it"}}
			p := cardParker(fake, c.call)
			wake, _, _ := p.governedCardWait(ctx, run, c.call)
			if wake == nil {
				t.Fatal("card is not pending")
			}
			entries := []*models.SessionEntry{awaitingEventEntry("tool-1", c.call.ID, models.ToolEntryWait{EventWaitID: wake.Reference})}
			plan, err := p.ReplanPark(ctx, run, entries)
			if err != nil || plan.Settled || plan.Park == nil || plan.Park.Status != models.AgentRunStatusWaitingForEvent || plan.Park.Wake.Type != c.wake {
				t.Fatalf("pending plan = %#v, %v", plan, err)
			}
			c.settle(fake, c.call, run)
			plan, err = p.ReplanPark(ctx, run, entries)
			if err != nil || !plan.Settled || plan.Park != nil {
				t.Fatalf("settled plan = %#v, %v", plan, err)
			}
		})
	}

	t.Run("skill maintenance", func(t *testing.T) {
		clock := now
		p := cardParkerAt(func() time.Time { return clock }, newFakeGovernedCards())
		retryAt := now.Add(time.Minute)
		entries := []*models.SessionEntry{awaitingEventEntry("tool-1", "call-1", models.ToolEntryWait{Reason: governedMaintenanceWaitReason, RetryAt: &retryAt})}
		plan, err := p.ReplanPark(ctx, governedCardRun(), entries)
		if err != nil || plan.Settled || plan.Park == nil || plan.Park.Status != models.AgentRunStatusSleeping || !plan.Park.Wake.WakeAt.Equal(retryAt) {
			t.Fatalf("maintenance plan = %#v, %v", plan, err)
		}
		clock = retryAt
		plan, err = p.ReplanPark(ctx, governedCardRun(), entries)
		if err != nil || !plan.Settled || plan.Park != nil {
			t.Fatalf("due maintenance plan = %#v, %v", plan, err)
		}
	})
}

func TestSettledTextFromInterventionElseRecord(t *testing.T) {
	ctx := context.Background()
	fake := newFakeGovernedCards()
	p := cardParker(fake)
	run := governedCardRun()

	credential := credentialCardCall("credential-call")
	fake.addCredential(credential, models.CredentialRequestStatusPending)
	settled := []models.AgentRunIntervention{
		{ID: "credential-request-result:old", ActionCallID: credential.ID, Instruction: "older"},
		{ID: "other", ActionCallID: "other-call", Instruction: "not this call"},
		{ID: "credential-request-result:new", ActionCallID: credential.ID, Instruction: "The user saved their card."},
	}
	if got := p.settledText(ctx, run, credential, settled); got != "The user saved their card." {
		t.Fatalf("from intervention = %q", got)
	}
	if got := p.settledText(ctx, run, credential, nil); got != "" {
		t.Fatalf("pending request = %q; want none", got)
	}
	fake.credentials["credential-"+credential.ID].Status = models.CredentialRequestStatusDismissed
	if got, want := p.settledText(ctx, run, credential, nil), skillbinding.CredentialRequestResolutionInstruction(fake.credentials["credential-"+credential.ID]); got != want {
		t.Fatalf("credential record = %q; want %q", got, want)
	}

	app := connectAppCardCall("app-call")
	fake.addConnectApp(app, "resolved")
	if got := p.settledText(ctx, run, app, nil); got != "The user connected Linear. Continue the user's original request with them." {
		t.Fatalf("connect app record = %q", got)
	}

	account := connectAccountCardCall("account-call")
	fake.addConnectAccount(account, "resolved")
	if got := p.settledText(ctx, run, account, nil); got != "The person you act for connected Gmail. Retry the step that needed it." {
		t.Fatalf("connect account record = %q", got)
	}

	human := humanCardCall("human-call")
	run.HumanInterventions = []models.HumanInterventionRequest{{ID: "human-1", ActionCallID: human.ID, Status: models.HumanInterventionStatusResolved, Resolution: "I solved the captcha."}}
	if got := p.settledText(ctx, run, human, nil); got != "I solved the captcha." {
		t.Fatalf("human record = %q", got)
	}

	plain := governedCardCall("plain", "read", models.ActionCallStatusSucceeded, map[string]interface{}{"ok": true})
	plain.SkillID = "repo"
	if got := p.settledText(ctx, run, plain, nil); got != "" {
		t.Fatalf("plain call = %q", got)
	}
}
