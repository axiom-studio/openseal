package runtime

// Event and timer waits (run-loop section 12 §5.9, D6): a tool that opens a
// card (a credential request, a connect app card, a connect account card, a
// human intervention) or asks the person a question parks its chat Run
// waiting_for_event with the tool entry awaiting_event and the claim
// released; the settle goes through the same service the product uses, and
// the resumed Run gets the outcome as the tool result. wait parks a
// background Run on a timer until the timer wake loop wakes it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// waitCredentialRequestOutput is what request_credential returns for the
// request it saved: the request's JSON under credentialRequest.
func waitCredentialRequestOutput(request *CredentialRequest) (map[string]interface{}, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var value map[string]interface{}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return map[string]interface{}{"credentialRequest": value}, nil
}

// waitCardSkills binds the kernel's Skill management Skill (request_credential,
// request_connection) and a Mail Skill whose send uses each person's own
// Google account to finn, next to the fixture Skill.
func waitCardSkills(f *looptest.Fixture) {
	f.T.Helper()
	skillScope := skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}
	mail := &skill.Definition{
		ID: "mail", Version: "1.0.0", Name: "Mail", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://mail.invalid"},
		Actions: map[string]skill.Action{"send_mail": {
			Name: "send_mail", Description: "Send an email", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal,
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{"to": map[string]interface{}{"type": "string"}}, "required": []interface{}{"to"}},
			Credentials: []skill.CredentialRequirement{{Name: "google", Kind: "oauth2", OAuth2: &skill.OAuth2Requirement{Provider: "google", Subject: "user", Scopes: []string{"mail.send"}}}},
			Idempotency: skill.IdempotencySupported,
		}},
	}
	for _, definition := range []*skill.Definition{skillbinding.SkillManagementSkill(), mail} {
		if err := f.Catalog.Register(f.Ctx, definition); err != nil {
			f.T.Fatal(err)
		}
	}
	bindings := []*skill.Binding{
		{ID: "skills-binding", Scope: skillScope, DeploymentID: looptest.FixtureAgent, SkillID: SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion,
			AllowedActions: []string{agentrun.SkillActionRequestCredential, agentrun.SkillActionRequestConnection}, MaximumRisk: skill.RiskLevelExternal, Revision: 1},
		{ID: "mail-binding", Scope: skillScope, DeploymentID: looptest.FixtureAgent, SkillID: "mail", SkillVersion: "1.0.0",
			AllowedActions: []string{"send_mail"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
			Credentials: map[string]skill.CredentialReference{"google": {Kind: "oauth2", ID: skill.PersonCredentialReferenceID}}},
	}
	for _, binding := range bindings {
		if err := f.Catalog.Bind(f.Ctx, binding); err != nil {
			f.T.Fatal(err)
		}
	}
	offered, err := f.Catalog.ListModelActions(f.Ctx, skillScope, looptest.FixtureAgent)
	if err != nil {
		f.T.Fatal(err)
	}
	// Fewer than eight actions keep every action a direct tool (no deferred
	// capability loading): drop the fixture actions these journeys never call.
	f.Offered = f.Offered[:0]
	for _, action := range offered {
		if action.Action != "explode" && action.Action != "list_items" {
			f.Offered = append(f.Offered, action)
		}
	}
	if len(f.Offered) != 6 {
		f.T.Fatalf("offered %d actions, want 6", len(f.Offered))
	}
}

// waitTool is the offered name of a governed action ending in action.
func waitTool(t *testing.T, v looptest.StepView, action string) string {
	t.Helper()
	for _, tool := range v.Request().Tools {
		if strings.HasSuffix(tool.Name, action) && strings.HasPrefix(tool.Name, looptest.GovernedToolPrefix) {
			return tool.Name
		}
	}
	t.Errorf("%s is not offered: %s", action, v.Describe())
	return looptest.GovernedToolPrefix + action
}

// waitSettleCard is the product's card settle (Engine.continueInteractionRun):
// the settled card's outcome becomes an intervention carrying its action call,
// and the Run waiting on exactly that card is queued.
func waitSettleCard(f *looptest.Fixture, runID, wakeType, requestID, actionCallID, instruction string) {
	f.T.Helper()
	run := f.MustRun(runID)
	if run.Status != AgentRunStatusWaitingForEvent || run.WakeCondition == nil || run.WakeCondition.Type != wakeType || run.WakeCondition.Reference != requestID {
		f.T.Fatalf("run %s = %s %#v, want waiting for %s %s", runID, run.Status, run.WakeCondition, wakeType, requestID)
	}
	actor := ActivityActor{Type: "service", ID: wakeType + "-coordinator"}
	activity := NewRunActivityService(f.Store, f.Store, models.WithClock(f.Clock.Now))
	if _, _, err := activity.TransitionRun(f.Ctx, f.Scope, runID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusQueued, Summary: "Received the card result; continuing",
		EventType: "run.interaction_settled", Actor: actor, CorrelationID: requestID, CausationID: actionCallID,
		Intervention: &AgentRunIntervention{ID: wakeType + "-result:" + requestID, Actor: actor, Instruction: instruction,
			CreatedAt: f.Clock.Now(), ActionCallID: actionCallID},
	}); err != nil {
		f.T.Fatalf("settle card %s: %v", requestID, err)
	}
}

// waitParked asserts the Run parked on wakeType with entry the tool entry
// awaiting the event, the claim released, and returns the entry.
func waitParked(f *looptest.Fixture, runID, tool, wakeType string) *SessionEntry {
	f.T.Helper()
	run := f.MustRun(runID)
	if run.Status != AgentRunStatusWaitingForEvent || run.WakeCondition == nil || run.WakeCondition.Type != wakeType {
		f.T.Fatalf("run %s = %s %#v (%#v), want waiting_for_event on %s; log %s", runID, run.Status, run.WakeCondition, run.FailureExplanation, wakeType, steerShape(f, runID))
	}
	if run.LeaseOwner != "" || run.LeaseExpiresAt != nil {
		f.T.Fatalf("parked run %s still holds the claim: %q %v", runID, run.LeaseOwner, run.LeaseExpiresAt)
	}
	entries := convToolEntries(f, runID, tool)
	if len(entries) != 1 || entries[0].State != ToolEntryAwaitingEvent {
		f.T.Fatalf("%s entries = %#v, want one awaiting_event", tool, entries)
	}
	return entries[0]
}

// waitCompleted is the settled tool entry: completed, with its result text.
func waitCompleted(f *looptest.Fixture, runID, tool string, index int) (*SessionEntry, string) {
	f.T.Helper()
	entries := convToolEntries(f, runID, tool)
	if len(entries) <= index || entries[index].State != ToolEntryCompleted || entries[index].Tool.Result == nil {
		f.T.Fatalf("%s entries = %d, entry %d not completed: %#v", tool, len(entries), index, entries)
	}
	return entries[index], looptest.PartsText(entries[index].Tool.Result.Content)
}

// waitCardRules: step 1 opens the card, the step that sees the card's
// outcome answers with it.
func waitCardRules(t *testing.T, tool string, open func(v looptest.StepView) ModelStepResult) []*looptest.ModelRule {
	return []*looptest.ModelRule{
		{Name: "open card", Times: 1, Reply: open},
		{Name: "answer", Reply: func(v looptest.StepView) ModelStepResult {
			for _, e := range v.ToolResultsSinceUser() {
				if strings.HasSuffix(e.Tool.Name, tool) && e.Tool.State == ToolEntryCompleted {
					return looptest.FinalAnswer(map[string]any{"answer": "Done after the card: " + looptest.PartsText(e.Tool.Result.Content)})
				}
			}
			t.Errorf("answer step without the settled %s: %s", tool, v.Describe())
			return looptest.FinalAnswer(map[string]any{"answer": "?"})
		}},
	}
}

// TestCardToolParksWaitingForEventAndSettleAppendsOutcome: each card tool
// parks the chat Run on its card, and the card's settle resumes the same Run
// with the outcome as the tool result. The Run is answered once, after the
// card.
func TestCardToolParksWaitingForEventAndSettleAppendsOutcome(t *testing.T) {
	t.Run("request_credential", func(t *testing.T) {
		looptest.EachStore(t, "wait-credential", func(t *testing.T, store KernelStore) {
			f := looptest.NewFixture(t, store)
			waitCardSkills(f)
			f.Dispatch.On(agentrun.SkillActionRequestCredential, func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
				conversationID, _ := in.Run.Context[conversationRunContextConversationID].(string)
				triggerID, _ := in.Run.Context[conversationRunContextTriggerID].(string)
				now := f.Clock.Now()
				request := &CredentialRequest{ID: "credential-request:" + in.Call.ID, Scope: in.Run.Scope, Owner: in.Run.Owner, DeploymentID: in.Run.Owner.ID,
					AgentID: in.Run.AssignedAgentID, ConversationID: conversationID, TriggerMessageID: triggerID, RunID: in.Run.ID, ActionCallID: in.Call.ID,
					Kind: CredentialRequestKindPaymentCard, Reason: "Add your card so I can pay", Status: CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
				if err := f.Raw.(CredentialRequestStore).SaveCredentialRequest(f.Ctx, request, 0); err != nil {
					return nil, err
				}
				return waitCredentialRequestOutput(request)
			})
			f.Model.Add(waitCardRules(t, agentrun.SkillActionRequestCredential, func(v looptest.StepView) ModelStepResult {
				return looptest.Act("I need your card.", looptest.Call(waitTool(t, v, agentrun.SkillActionRequestCredential), map[string]any{"kind": CredentialRequestKindPaymentCard, "reason": "Add your card so I can pay"}))
			})...)
			conversation, run := f.StartChat("Pay the invoice")
			steerToRest(f, run.ID, 4)
			entry := waitParked(f, run.ID, waitTool(t, looptest.StepView{Req: convFirstStep(f, run.ID)}, agentrun.SkillActionRequestCredential), skillbinding.CredentialRequestWakeType)
			requestID := f.MustRun(run.ID).WakeCondition.Reference
			if entry.Tool.Wait == nil || entry.Tool.Wait.EventWaitID != requestID {
				t.Fatalf("entry wait = %#v, want event %s", entry.Tool.Wait, requestID)
			}
			if len(steerAgentMessages(f, conversation.ID)) != 0 {
				t.Fatal("the parked Run posted an answer")
			}

			// The user saves the card.
			credentials := f.Raw.(CredentialRequestStore)
			request, err := credentials.GetCredentialRequest(f.Ctx, f.Scope, requestID)
			if err != nil {
				t.Fatal(err)
			}
			request.Status, request.ResolvedBy, request.Revision, request.UpdatedAt = CredentialRequestStatusResolved, looptest.FixtureUser, request.Revision+1, f.Clock.Now()
			if err := credentials.SaveCredentialRequest(f.Ctx, request, request.Revision-1); err != nil {
				t.Fatal(err)
			}
			waitSettleCard(f, run.ID, skillbinding.CredentialRequestWakeType, requestID, entry.ActionCallID, skillbinding.CredentialRequestResolutionInstruction(request))
			done := convDrive(f, run.ID)
			_, result := waitCompleted(f, done.ID, entry.Tool.Name, 0)
			if !strings.Contains(result, "The user saved or updated their card in the vault.") {
				t.Fatalf("credential outcome = %q", result)
			}
			if answer := convAgentAnswer(f, conversation.ID); !strings.HasPrefix(answer.Content, "Done after the card: ") || len(steerAgentMessages(f, conversation.ID)) != 1 {
				t.Fatalf("answers = %s", looptest.DescribeMessages(steerAgentMessages(f, conversation.ID)))
			}
		})
	})

	t.Run("connect_app", func(t *testing.T) {
		looptest.EachStore(t, "wait-connect-app", func(t *testing.T, store KernelStore) {
			f := looptest.NewFixture(t, store)
			waitCardSkills(f)
			setups := f.Raw.(SkillSetupRequestStore)
			f.Dispatch.On(agentrun.SkillActionRequestConnection, func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
				conversationID, _ := in.Run.Context[conversationRunContextConversationID].(string)
				triggerID, _ := in.Run.Context[conversationRunContextTriggerID].(string)
				now := f.Clock.Now()
				target := ConnectAppTarget{CatalogSlug: "linear", Name: "Linear"}
				request := &SkillSetupRequest{ID: "connect-app:" + in.Call.ID, Scope: in.Run.Scope, Owner: in.Run.Owner, DeploymentID: in.Run.Owner.ID,
					AgentID: in.Run.AssignedAgentID, ConversationID: conversationID, TriggerMessageID: triggerID, RunID: in.Run.ID, ActionCallID: in.Call.ID,
					Kind: SkillSetupKindConnectApp, SkillID: "openseal.mcp", SkillVersion: "1.0.0", SkillName: "Linear", App: &target,
					RequestedFor: in.Run.ActingUser, Reason: "Connect Linear to list your issues", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
				if err := setups.SaveSkillSetupRequest(f.Ctx, request, 0); err != nil {
					return nil, err
				}
				return skillbinding.SkillSetupRequestResult(request)
			})
			f.Model.Add(waitCardRules(t, agentrun.SkillActionRequestConnection, func(v looptest.StepView) ModelStepResult {
				return looptest.Act("Linear needs connecting.", looptest.Call(waitTool(t, v, agentrun.SkillActionRequestConnection), map[string]any{"app": "linear", "reason": "Connect Linear to list your issues"}))
			})...)
			conversation, run := f.StartChat("List my Linear issues")
			steerToRest(f, run.ID, 4)
			entry := waitParked(f, run.ID, waitTool(t, looptest.StepView{Req: convFirstStep(f, run.ID)}, agentrun.SkillActionRequestConnection), skillbinding.ConnectAppWakeType)
			requestID := f.MustRun(run.ID).WakeCondition.Reference

			request, err := setups.GetSkillSetupRequest(f.Ctx, f.Scope, requestID)
			if err != nil {
				t.Fatal(err)
			}
			request.Status, request.ResolvedBy, request.Revision, request.UpdatedAt = "resolved", looptest.FixtureUser, request.Revision+1, f.Clock.Now()
			request.ResolvedBindingID, request.ResolvedBindingRevision = "linear-binding", 1
			if err := setups.SaveSkillSetupRequest(f.Ctx, request, request.Revision-1); err != nil {
				t.Fatal(err)
			}
			waitSettleCard(f, run.ID, skillbinding.ConnectAppWakeType, requestID, entry.ActionCallID, skillbinding.ConnectAppResolutionInstruction(request, []string{"linear_list_issues"}))
			done := convDrive(f, run.ID)
			if _, result := waitCompleted(f, done.ID, entry.Tool.Name, 0); !strings.Contains(result, "The user connected Linear. Its tools are now available: linear_list_issues.") {
				t.Fatalf("connect app outcome = %q", result)
			}
			if len(steerAgentMessages(f, conversation.ID)) != 1 {
				t.Fatalf("answers = %s", looptest.DescribeMessages(steerAgentMessages(f, conversation.ID)))
			}
		})
	})

	t.Run("connect_account", func(t *testing.T) {
		looptest.EachStore(t, "wait-connect-account", func(t *testing.T, store KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithCredentials(action.CredentialResolverFunc(func(context.Context, action.CredentialResolutionRequest) (map[string]string, error) {
				return map[string]string{"google": "token"}, nil
			})))
			waitCardSkills(f)
			connected := false
			f.Executor.SetCredentialAvailabilityChecker(actiontest.AvailabilityFunc(func(_ context.Context, req action.CredentialAvailabilityRequest) (action.CredentialAvailability, error) {
				if connected {
					return action.CredentialAvailability{}, nil
				}
				return action.CredentialAvailability{Gaps: map[string]action.CredentialGap{"google": action.CredentialGapPersonSignIn}}, nil
			}))
			f.Dispatch.On("send_mail", func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{"sent": true}, nil
			})
			sends := 0
			f.Model.Add(
				&looptest.ModelRule{Name: "send", When: func(v looptest.StepView) bool {
					content, _, ok := v.ToolResult(waitTool(t, v, "send_mail"))
					return !ok || strings.Contains(content, "Retry the step that needed it.")
				}, Reply: func(v looptest.StepView) ModelStepResult {
					sends++
					return looptest.Act("Sending.", looptest.ReviewedCall(waitTool(t, v, "send_mail"), map[string]any{"to": "ben@example.com"}, "Email ben"))
				}},
				&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) ModelStepResult {
					return looptest.FinalAnswer(map[string]any{"answer": "Ben has it now."})
				}},
			)
			conversation, run := f.StartChat("Email ben")
			if run.ActingUser != looptest.FixtureUser {
				t.Fatalf("chat Run acts for %q", run.ActingUser)
			}
			steerToRest(f, run.ID, 4)
			tool := waitTool(t, looptest.StepView{Req: convFirstStep(f, run.ID)}, "send_mail")
			entry := waitParked(f, run.ID, tool, ConnectAccountWakeType)
			requestID := f.MustRun(run.ID).WakeCondition.Reference
			if requestID != skillbinding.ConnectAccountRequestPrefix+entry.ActionCallID || f.Dispatch.Count("send_mail") != 0 {
				t.Fatalf("connect account card %s for call %s, dispatches %d", requestID, entry.ActionCallID, f.Dispatch.Count("send_mail"))
			}

			// The person connects the account.
			connected = true
			setups := f.Raw.(SkillSetupRequestStore)
			card, err := setups.GetSkillSetupRequest(f.Ctx, f.Scope, requestID)
			if err != nil || card == nil || card.Kind != SkillSetupKindConnectAccount || card.RequestedFor != looptest.FixtureUser {
				t.Fatalf("card = %#v, %v", card, err)
			}
			// The worker stamps the card from the wall clock.
			card.Status, card.ResolvedBy, card.Revision, card.UpdatedAt = "resolved", looptest.FixtureUser, card.Revision+1, card.CreatedAt.Add(time.Second)
			card.ResolvedBindingID, card.ResolvedBindingRevision = "mail-binding", 1
			if err := setups.SaveSkillSetupRequest(f.Ctx, card, card.Revision-1); err != nil {
				t.Fatal(err)
			}
			waitSettleCard(f, run.ID, ConnectAccountWakeType, requestID, entry.ActionCallID, skillbinding.ConnectAccountResolutionInstruction(card))
			done := convDrive(f, run.ID)
			// The parked call failed before dispatch; its settled entry keeps
			// that failure and carries the card's outcome, and the retry is a
			// new call.
			sendEntries := convToolEntries(f, done.ID, tool)
			if len(sendEntries) != 2 || sendEntries[0].State != ToolEntryError || sendEntries[1].State != ToolEntryCompleted {
				t.Fatalf("send entries = %#v", sendEntries)
			}
			if result := looptest.PartsText(sendEntries[0].Tool.Result.Content); !strings.Contains(result, "The person you act for connected Google. Retry the step that needed it.") {
				t.Fatalf("connect account outcome = %q", result)
			}
			// The model retried the send, which now dispatched once.
			if sends != 2 || f.Dispatch.Count("send_mail") != 1 {
				t.Fatalf("sends = %d, dispatches = %d", sends, f.Dispatch.Count("send_mail"))
			}
			if answer := convAgentAnswer(f, conversation.ID); answer.Content != "Ben has it now." || len(steerAgentMessages(f, conversation.ID)) != 1 {
				t.Fatalf("answers = %s", looptest.DescribeMessages(steerAgentMessages(f, conversation.ID)))
			}
		})
	})

	t.Run("human_intervention", func(t *testing.T) {
		looptest.EachStore(t, "wait-human", func(t *testing.T, store KernelStore) {
			f := looptest.NewFixture(t, store)
			f.Dispatch.On("lookup", func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{"requiresHuman": true, "challenges": []interface{}{"captcha"}}, nil
			})
			f.Model.Add(waitCardRules(t, "lookup", func(v looptest.StepView) ModelStepResult {
				return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "6815"}))
			})...)
			conversation, run := f.StartChat("Look up 6815")
			steerToRest(f, run.ID, 4)
			tool := looptest.GovernedTool(looptest.StepView{Req: convFirstStep(f, run.ID)}, "lookup")
			waitParked(f, run.ID, tool, "human_intervention")
			parked := f.MustRun(run.ID)
			if len(parked.HumanInterventions) != 1 || parked.HumanInterventions[0].Status != HumanInterventionStatusPending ||
				parked.WakeCondition.Reference != parked.HumanInterventions[0].ID {
				t.Fatalf("human interventions = %#v, wake %#v", parked.HumanInterventions, parked.WakeCondition)
			}

			// The person solves the captcha through the run command service.
			commands := agentrun.NewRunCommandService(f.Chats, models.WithClock(f.Clock.Now))
			if _, err := commands.CommandAgentRun(f.Ctx, agentrun.AgentRunCommandRequest{
				Scope: f.Scope, RunID: run.ID, ExpectedRevision: parked.Revision, Kind: agentrun.AgentRunCommandResolveHumanIntervention,
				HumanInterventionID: parked.HumanInterventions[0].ID, Instruction: "Solved the captcha.",
				Actor: ActivityActor{Type: "user", ID: looptest.FixtureUser},
			}); err != nil {
				t.Fatal(err)
			}
			done := convDrive(f, run.ID)
			if _, result := waitCompleted(f, done.ID, tool, 0); !strings.Contains(result, "Solved the captcha.") {
				t.Fatalf("human intervention outcome = %q", result)
			}
			if f.Dispatch.Count("lookup") != 1 || len(steerAgentMessages(f, conversation.ID)) != 1 {
				t.Fatalf("lookups = %d, answers = %s", f.Dispatch.Count("lookup"), looptest.DescribeMessages(steerAgentMessages(f, conversation.ID)))
			}
		})
	})

	t.Run("clarification", func(t *testing.T) {
		looptest.EachStore(t, "wait-ask", func(t *testing.T, store KernelStore) {
			f := looptest.NewFixture(t, store)
			question := "Which environment, staging or production?"
			f.Model.Add(waitCardRules(t, string(ControlToolAskUser), func(v looptest.StepView) ModelStepResult {
				if !v.HasTool(string(ControlToolAskUser)) {
					t.Errorf("ask_user is not offered: %s", v.Describe())
				}
				return looptest.Act("", looptest.Call(string(ControlToolAskUser), map[string]any{"question": question}))
			})...)
			conversation, run := f.StartChat("Deploy the release")
			steerToRest(f, run.ID, 4)
			waitParked(f, run.ID, string(ControlToolAskUser), "user_message")
			asked := convAgentAnswer(f, conversation.ID)
			if asked.Content != question || asked.Intent != MessageIntentQuestion || !asked.RequiresResponse {
				t.Fatalf("question message = %#v", asked)
			}

			// The person answers in the chat; the scheduler resumes the same Run.
			answer := convPost(f, conversation.ID, "staging", func(r *runtimeconversation.PostChannelMessageRequest) {
				r.Intent, r.RequiresResponse = MessageIntentAnswer, true
			})
			resumed := convSchedule(f, conversation.ID, answer)
			if resumed.ID != run.ID || resumed.Status != AgentRunStatusQueued {
				t.Fatalf("answer scheduled %s (%s), want the asking Run %s resumed", resumed.ID, resumed.Status, run.ID)
			}
			// The scheduler's resume stamps AvailableAt from the wall clock.
			if resumed.AvailableAt.After(f.Clock.Now()) {
				f.Clock.Advance(resumed.AvailableAt.Sub(f.Clock.Now()) + time.Millisecond)
			}
			done := convDrive(f, run.ID)
			entry, result := waitCompleted(f, done.ID, string(ControlToolAskUser), 0)
			if !strings.Contains(result, "staging") {
				t.Fatalf("ask_user result = %q", result)
			}
			// The answer is the tool result, not a steering user entry.
			for _, e := range convRunEntries(f, done.ID) {
				if e.Type == SessionEntryUser && e.User != nil && e.User.MessageID == answer.ID && e.Sequence > entry.Sequence {
					t.Fatalf("the answer was also appended as a user entry: %#v", e)
				}
			}
			// The question, the scheduler's acknowledgement of the answer, and
			// the one answer.
			answers := steerAgentMessages(f, conversation.ID)
			if len(answers) != 3 || answers[0].ID != asked.ID || !strings.HasPrefix(answers[2].Content, "Done after the card: ") {
				t.Fatalf("agent messages = %s", looptest.DescribeMessages(answers))
			}
			runs := steerConversationRuns(f, conversation.ID)
			if len(runs) != 1 {
				t.Fatalf("the answer started another Run: %d Runs", len(runs))
			}
		})
	})
}

// TestWaitToolParksUntilTimer: wait parks a background Run sleeping on a
// timer with the tool entry awaiting the event; when the clock passes the
// timer, the timer wake loop queues it, and the resumed Run completes the
// entry as elapsed and goes on.
func TestWaitToolParksUntilTimer(t *testing.T) {
	looptest.EachStore(t, "wait-timer", func(t *testing.T, store KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "wait", Times: 1, Reply: func(v looptest.StepView) ModelStepResult {
				if !v.HasTool(string(ControlToolWait)) {
					t.Errorf("wait is not offered to background work: %s", v.Describe())
				}
				return looptest.Act("Waiting for the build.", looptest.Call(string(ControlToolWait), map[string]any{"seconds": 600}))
			}},
			&looptest.ModelRule{Name: "after", Reply: func(v looptest.StepView) ModelStepResult {
				content, _, ok := v.ToolResult(string(ControlToolWait))
				if !ok {
					t.Errorf("resumed step without the wait result: %s", v.Describe())
				}
				return looptest.Say("The build had time to finish: " + content)
			}},
		))
		run := f.StartRun(RunKindAgentWork, "Check the nightly build after it finishes")
		started := f.Clock.Now()
		parked := f.DriveToRest(run.ID, 4)
		if parked.Status != AgentRunStatusSleeping || parked.WakeCondition == nil || parked.WakeCondition.Type != "timer" ||
			parked.WakeCondition.WakeAt == nil || !parked.WakeCondition.WakeAt.Equal(started.Add(600*time.Second)) {
			t.Fatalf("parked = %s %#v", parked.Status, parked.WakeCondition)
		}
		if parked.LeaseOwner != "" {
			t.Fatalf("sleeping Run holds the claim: %q", parked.LeaseOwner)
		}
		waits := convToolEntries(f, run.ID, string(ControlToolWait))
		if len(waits) != 1 || waits[0].State != ToolEntryAwaitingEvent || parked.WakeCondition.Reference != waits[0].ID {
			t.Fatalf("wait entries = %#v, wake %#v", waits, parked.WakeCondition)
		}

		wake := agentrun.NewAgentRunWakeService(f.Store, f.Store, models.WithClock(f.Clock.Now))
		// Before the timer nothing wakes.
		f.Clock.Advance(599 * time.Second)
		if result, err := wake.WakeDueTimers(f.Ctx, f.Scope, f.Clock.Now()); err != nil || len(result.Runs) != 0 {
			t.Fatalf("early wake = %#v, %v", result, err)
		}
		f.Clock.Advance(2 * time.Second)
		result, err := wake.WakeDueTimers(f.Ctx, f.Scope, f.Clock.Now())
		if err != nil || len(result.Runs) != 1 || result.Runs[0].Run.ID != run.ID || result.Runs[0].Run.Status != AgentRunStatusQueued {
			t.Fatalf("timer wake = %#v, %v", result, err)
		}
		done := f.DriveToRest(run.ID, 4)
		if done.Status != AgentRunStatusCompleted {
			t.Fatalf("resumed Run = %s (%#v)", done.Status, done.FailureExplanation)
		}
		_, content := waitCompleted(f, run.ID, string(ControlToolWait), 0)
		var value map[string]interface{}
		if err := json.Unmarshal([]byte(content), &value); err != nil || value["status"] != "elapsed" || fmt.Sprint(value["waitedSeconds"]) != "601" {
			t.Fatalf("wait result = %q (%v)", content, err)
		}
		if summary, _ := done.Output["summary"].(string); !strings.Contains(summary, "The build had time to finish") {
			t.Fatalf("output = %#v", done.Output)
		}
	})
}
