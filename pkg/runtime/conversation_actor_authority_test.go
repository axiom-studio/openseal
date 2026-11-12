package runtime_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type conversationActorFixture struct {
	store        *sqlite.Store
	service      *runtimeconversation.ConversationService
	scheduler    *runtime.ConversationRunScheduler
	conversation *models.Conversation
}

func newConversationActorFixture(t *testing.T, scope models.Scope, agentID string) *conversationActorFixture {
	t.Helper()
	store := conversationtest.NewStore(t)
	service := runtimeconversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: agentID},
		Title: "Shared organization conversation", IdempotencyKey: "shared-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &conversationActorFixture{store, service, mustConversationRunScheduler(t, store), conversation}
}

func (f *conversationActorFixture) post(t *testing.T, request runtimeconversation.PostChannelMessageRequest) *models.ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	request.Scope = current.Scope
	request.ConversationID = current.ID
	request.ExpectedRevision = current.Revision
	request.Audience = models.ConversationAudience{Kind: models.ConversationAudienceChannel}
	request.RequiresResponse = true
	if request.Content == "" {
		request.Content = "Please continue with this request."
	}
	if request.Intent == "" {
		request.Intent = models.MessageIntentQuestion
	}
	posted, err := f.service.PostChannelMessage(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return posted.Message
}

func (f *conversationActorFixture) schedule(t *testing.T, message *models.ChannelMessage) *models.AgentRun {
	t.Helper()
	result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.conversation.Scope, f.conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule %s = %#v, %v", message.ID, result, err)
	}
	return result.Run
}

func (f *conversationActorFixture) waitingQuestion(t *testing.T, trigger *models.ChannelMessage) (*models.AgentRun, *models.AgentRun, *models.ChannelMessage) {
	t.Helper()
	root := f.schedule(t, trigger)
	activities := activity.NewRunActivityService(f.store, f.store)
	var err error
	root, _, err = activities.TransitionRun(t.Context(), root.Scope, root.ID, activity.RunTransitionRequest{ExpectedRevision: root.Revision, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	root, _, err = activities.TransitionRun(t.Context(), root.Scope, root.ID, activity.RunTransitionRequest{ExpectedRevision: root.Revision, Status: models.AgentRunStatusWaitingForDependency, WakeCondition: &models.WakeCondition{Type: "run_dependencies", Reference: "draft"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := portfolio.NewPortfolioService(f.store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: root.Scope, Kind: models.RunKindAgentWork, Owner: root.Owner, AssignedAgentID: root.Owner.ID,
		ParentRunID: root.ID, Goal: "Draft the requested report", Source: models.RunSourceRequest,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, _ = askUserParked(t, f.store, claimAskUserRun(t, f.store, child, "actor-worker"), "actor-worker", "Who is the report for?")
	if _, err = f.scheduler.ReconcileScope(t.Context(), root.Scope); err != nil {
		t.Fatal(err)
	}
	question, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), root.Scope, f.conversation.ID, clarificationQuestionKeyOf(child))
	if err != nil || question == nil {
		t.Fatalf("projected question = %#v, %v", question, err)
	}
	return root, child, question
}

func (f *conversationActorFixture) assertRunUnchanged(t *testing.T, before *models.AgentRun) {
	t.Helper()
	after, err := f.store.GetAgentRun(t.Context(), before.Scope, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Store cloning can normalize numbers in Context from int64 to float64.
	// Compare the complete durable representation rather than Go-only types.
	want, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Fatalf("another member changed run %s: before=%#v after=%#v err=%v", before.ID, before, after, err)
	}
}

func TestConversationClarificationOnlyAcceptsInitiatingUsersAnswer(t *testing.T) {
	for _, target := range []string{"implicit", "reply", "resolves"} {
		t.Run(target, func(t *testing.T) {
			f := newConversationActorFixture(t, models.Scope{Kind: "organization", ID: "shared"}, "writer")
			a := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-a"}
			b := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-b"}
			trigger := f.post(t, runtimeconversation.PostChannelMessageRequest{Sender: a, IdempotencyKey: "request-a"})
			root, child, question := f.waitingQuestion(t, trigger)
			request := runtimeconversation.PostChannelMessageRequest{Sender: b, Intent: models.MessageIntentAnswer, Content: "Use the finance team as the audience.", IdempotencyKey: "answer-b"}
			switch target {
			case "reply":
				request.ReplyToMessageID = question.ID
			case "resolves":
				request.ResolvesMessageID = question.ID
			}
			answerB := f.post(t, request)
			runB := f.schedule(t, answerB)
			if runB.ID == root.ID || runB.ID == child.ID || runB.Kind != models.RunKindConversation || runB.Context[models.ConversationRunTriggerIDContextKey] != answerB.ID {
				t.Fatalf("member B did not receive a distinct run for their own message: %#v", runB)
			}
			f.assertRunUnchanged(t, root)
			f.assertRunUnchanged(t, child)
			if receipt, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), root.Scope, f.conversation.ID, "conversation-answer-received:"+answerB.ID); err != nil || receipt != nil {
				t.Fatalf("other member's answer was acknowledged as accepted: %#v, %v", receipt, err)
			}
			f.scheduler = mustConversationRunScheduler(t, f.store)
			replayed, replay, err := f.scheduler.ScheduleMessage(t.Context(), root.Scope, f.conversation.ID, answerB.ID)
			if err != nil || !replay || replayed == nil || replayed.Run.ID != runB.ID {
				t.Fatalf("member B replay = %#v, %v, %v", replayed, replay, err)
			}
			f.assertRunUnchanged(t, root)
			f.assertRunUnchanged(t, child)

			answerA := f.post(t, runtimeconversation.PostChannelMessageRequest{Sender: a, Intent: models.MessageIntentAnswer, ResolvesMessageID: question.ID, Content: "Write for general readers.", IdempotencyKey: "answer-a"})
			resumed := f.schedule(t, answerA)
			if resumed.ID != child.ID || resumed.Status != models.AgentRunStatusQueued || resumed.WakeCondition != nil || len(resumed.PendingInterventions) != 1 || resumed.PendingInterventions[0].Actor.ID != a.ID {
				t.Fatalf("original member's clarification did not resume exactly once: %#v", resumed)
			}
			f.scheduler = mustConversationRunScheduler(t, f.store)
			replayed, replay, err = f.scheduler.ScheduleMessage(t.Context(), root.Scope, f.conversation.ID, answerA.ID)
			if err != nil || !replay || replayed == nil || replayed.Run.ID != child.ID || len(replayed.Run.PendingInterventions) != 1 {
				t.Fatalf("original member's clarification replay = %#v, %v, %v", replayed, replay, err)
			}
			f.assertRunUnchanged(t, runB)
		})
	}
}

func TestConversationFollowUpsSeparateMembersIncludingDelayedScheduling(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "in-order"
		if reverse {
			name = "reverse-order"
		}
		t.Run(name, func(t *testing.T) {
			f := newConversationActorFixture(t, models.Scope{Kind: "organization", ID: "shared"}, "writer")
			a := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-a"}
			b := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-b"}
			requests := []runtimeconversation.PostChannelMessageRequest{
				{Sender: a, IdempotencyKey: "a-first"},
				{Sender: b, IdempotencyKey: "b-first"},
				{Sender: a, IdempotencyKey: "a-second"},
			}
			messages := make([]*models.ChannelMessage, len(requests))
			runs := make([]*models.AgentRun, len(requests))
			for i, request := range requests {
				messages[i] = f.post(t, request)
				if !reverse {
					runs[i] = f.schedule(t, messages[i])
					if i == 1 {
						f.assertRunUnchanged(t, runs[0])
					}
				}
			}
			if reverse {
				for i := len(messages) - 1; i >= 0; i-- {
					runs[i] = f.schedule(t, messages[i])
				}
			}
			// Member A's two messages share one Run; member B keeps their own.
			if runs[0].ID != runs[2].ID || runs[1].ID == runs[0].ID {
				t.Fatalf("runs = %s, %s, %s", runs[0].ID, runs[1].ID, runs[2].ID)
			}
			memberA, err := f.store.GetAgentRun(t.Context(), f.conversation.Scope, runs[0].ID)
			if err != nil || memberA.Status != models.AgentRunStatusQueued || len(models.ConversationRunFollowUps(memberA)) != 1 {
				t.Fatalf("member A Run = %#v, %v", memberA, err)
			}
			first, second := messages[0], messages[2]
			if reverse {
				first, second = second, first
			}
			if memberA.Context[models.ConversationRunTriggerIDContextKey] != first.ID || !runHasFollowUp(memberA, second.ID) {
				t.Fatalf("member A Run trigger/follow-up = %#v", memberA)
			}
			memberB, err := f.store.GetAgentRun(t.Context(), f.conversation.Scope, runs[1].ID)
			if err != nil || memberB.Status != models.AgentRunStatusQueued || len(memberB.PendingInterventions) != 0 || memberB.Context[models.ConversationRunTriggerIDContextKey] != messages[1].ID {
				t.Fatalf("member B Run = %#v, %v", memberB, err)
			}
			// Replaying any message changes neither member's Run.
			f.scheduler = mustConversationRunScheduler(t, f.store)
			for _, message := range messages {
				f.schedule(t, message)
			}
			f.assertRunUnchanged(t, memberA)
			f.assertRunUnchanged(t, memberB)
		})
	}
}

func TestOtherConversationMemberCannotCancelPendingApproval(t *testing.T) {
	catalog, scope := cancellationActionCatalog(t)
	f := newConversationActorFixture(t, scope, "research-agent")
	a := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-a"}
	trigger := f.post(t, runtimeconversation.PostChannelMessageRequest{Sender: a, IdempotencyKey: "request-a"})
	root := f.schedule(t, trigger)
	// The run loop claims member A's Run; one of its steps proposes the
	// reviewed action and the step-end park waits on its approval.
	loop := actiontest.NewLoopProposalFixture(t, f.store, root, "worker", time.Now().UTC())
	coordinator := action.NewActionCoordinator(f.store, f.store, catalog, action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionRequireApproval, Reason: "review required", ApprovalTTL: time.Hour, EligibleApprovers: []models.ApprovalPrincipal{{Type: "role", ID: "reviewer"}}}, nil
	}), nil)
	proposal, err := coordinator.Propose(t.Context(), loop.Request(action.ProposeActionRequest{
		DeploymentID: "research-agent", SkillID: "forum", SkillVersion: "1.0.0",
		Action: "reply", Arguments: map[string]interface{}{"body": "reviewed report"}, Summary: "Send reviewed report",
	}, "reviewed-action"))
	if err != nil || proposal == nil || proposal.Approval == nil || proposal.Call.Status != models.ActionCallStatusWaitingApproval {
		t.Fatalf("pending approval = %#v, %v", proposal, err)
	}
	loop.ParkOnApproval(proposal.Approval.ID)
	parked := loop.Run
	messageB := f.post(t, runtimeconversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-b"}, IdempotencyKey: "request-b"})
	runB := f.schedule(t, messageB)
	if runB.ID == root.ID || runB.Context[models.ConversationRunTriggerIDContextKey] != messageB.ID {
		t.Fatalf("member B inherited member A's run: %#v", runB)
	}
	f.assertRunUnchanged(t, parked)
	approval, err := f.store.GetApproval(t.Context(), scope, proposal.Approval.ID)
	if err != nil || !reflect.DeepEqual(approval, proposal.Approval) {
		t.Fatalf("member B changed pending approval: %#v, %v", approval, err)
	}
	call, err := f.store.GetActionCall(t.Context(), scope, proposal.Call.ID)
	if err != nil || !reflect.DeepEqual(call, proposal.Call) {
		t.Fatalf("member B changed pending action: %#v, %v", call, err)
	}

	// The requesting member's own message replaces the approval: the action
	// is not carried out and their Run resumes with the message.
	messageA := f.post(t, runtimeconversation.PostChannelMessageRequest{Sender: a, IdempotencyKey: "follow-up-a"})
	delivered := f.schedule(t, messageA)
	if delivered.ID != root.ID || delivered.Status != models.AgentRunStatusQueued || delivered.WakeCondition != nil ||
		!runHasFollowUp(delivered, messageA.ID) {
		t.Fatalf("follow-up did not resume the waiting Run: %#v", delivered)
	}
	approval, err = f.store.GetApproval(t.Context(), scope, proposal.Approval.ID)
	if err != nil || approval.Status != models.ApprovalStatusSuperseded {
		t.Fatalf("follow-up did not replace the pending approval: %#v, %v", approval, err)
	}
	f.assertRunUnchanged(t, runB)
}

func TestConversationClarificationUsesOnlyCanonicalVoiceInitiator(t *testing.T) {
	for _, test := range []struct {
		name        string
		serviceID   string
		response    string
		initiator   bool
		wantResumed bool
	}{
		{name: "voice initiator", serviceID: models.VoiceCallCoordinatorParticipantID, response: "spoken", initiator: true, wantResumed: true},
		{name: "unrelated service", serviceID: "other-service", response: "spoken", initiator: true},
		{name: "non voice event", serviceID: models.VoiceCallCoordinatorParticipantID, initiator: true},
		{name: "missing initiator", serviceID: models.VoiceCallCoordinatorParticipantID, response: "spoken"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConversationActorFixture(t, models.Scope{Kind: "organization", ID: "shared-voice"}, "writer")
			a := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-a"}
			request := runtimeconversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantService, ID: test.serviceID}, Intent: models.MessageIntentUpdate, ResponseMode: test.response, IdempotencyKey: "call-start"}
			if test.initiator {
				request.Initiator = &a
			}
			trigger := f.post(t, request)
			root, child, question := f.waitingQuestion(t, trigger)
			answerB := f.post(t, runtimeconversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-b"}, Intent: models.MessageIntentAnswer, ResolvesMessageID: question.ID, IdempotencyKey: "answer-b"})
			f.schedule(t, answerB)
			f.assertRunUnchanged(t, root)
			f.assertRunUnchanged(t, child)
			answerA := f.post(t, runtimeconversation.PostChannelMessageRequest{Sender: a, Intent: models.MessageIntentAnswer, ResolvesMessageID: question.ID, IdempotencyKey: "answer-a"})
			result := f.schedule(t, answerA)
			if test.wantResumed {
				if result.ID != child.ID || result.Status != models.AgentRunStatusQueued || len(result.PendingInterventions) != 1 || result.PendingInterventions[0].Actor.ID != a.ID {
					t.Fatalf("verified voice initiator failed to resume clarification: %#v", result)
				}
			} else {
				if result.ID == child.ID || result.ID == root.ID || result.Context[models.ConversationRunTriggerIDContextKey] != answerA.ID {
					t.Fatalf("untrusted service origin conferred continuation authority: %#v", result)
				}
				f.assertRunUnchanged(t, root)
				f.assertRunUnchanged(t, child)
			}
		})
	}
}

func TestConversationFollowUpUsesVoiceInitiator(t *testing.T) {
	f := newConversationActorFixture(t, models.Scope{Kind: "organization", ID: "shared-voice"}, "writer")
	a := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-a"}
	trigger := f.post(t, runtimeconversation.PostChannelMessageRequest{
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantService, ID: models.VoiceCallCoordinatorParticipantID}, Initiator: &a,
		Intent: models.MessageIntentUpdate, ResponseMode: "spoken", IdempotencyKey: "call-start",
	})
	voice := f.schedule(t, trigger)
	messageB := f.post(t, runtimeconversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-b"}, IdempotencyKey: "request-b"})
	runB := f.schedule(t, messageB)
	f.assertRunUnchanged(t, voice)
	messageA := f.post(t, runtimeconversation.PostChannelMessageRequest{Sender: a, IdempotencyKey: "request-a"})
	f.schedule(t, messageA)
	current, err := f.store.GetAgentRun(t.Context(), voice.Scope, voice.ID)
	if err != nil || current.Status != models.AgentRunStatusQueued || !runHasFollowUp(current, messageA.ID) {
		t.Fatalf("call initiator's message was not delivered to their call Run: %#v, %v", current, err)
	}
	f.assertRunUnchanged(t, runB)
}

func TestConversationInitiatingUserRejectsUnboundOrMalformedOrigin(t *testing.T) {
	conversation := &models.Conversation{ID: "shared", Scope: models.Scope{Kind: "organization", ID: "organization-a"}}
	a := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-a"}
	voice := &models.ChannelMessage{
		Scope: conversation.Scope, ConversationID: conversation.ID,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantService, ID: models.VoiceCallCoordinatorParticipantID}, Initiator: &a,
		Intent: models.MessageIntentUpdate, ResponseMode: "spoken", RequiresResponse: true,
	}
	for _, test := range []struct {
		name   string
		change func(*models.ChannelMessage)
		valid  bool
	}{
		{name: "verified voice", change: func(*models.ChannelMessage) {}, valid: true},
		{name: "human sender", change: func(m *models.ChannelMessage) { m.Sender = a; m.Initiator = nil }, valid: true},
		{name: "foreign scope", change: func(m *models.ChannelMessage) { m.Scope.ID = "organization-b" }},
		{name: "foreign conversation", change: func(m *models.ChannelMessage) { m.ConversationID = "other-conversation" }},
		{name: "missing initiator", change: func(m *models.ChannelMessage) { m.Initiator = nil }},
		{name: "empty initiator identity", change: func(m *models.ChannelMessage) { m.Initiator.ID = "" }},
		{name: "invalid initiator identity", change: func(m *models.ChannelMessage) { m.Initiator.ID = "member/a" }},
		{name: "nonhuman initiator", change: func(m *models.ChannelMessage) { m.Initiator.Type = models.ConversationParticipantAgent }},
		{name: "wrong service", change: func(m *models.ChannelMessage) { m.Sender.ID = "other-service" }},
		{name: "wrong intent", change: func(m *models.ChannelMessage) { m.Intent = models.MessageIntentQuestion }},
		{name: "no response requested", change: func(m *models.ChannelMessage) { m.RequiresResponse = false }},
		{name: "nonspoken event", change: func(m *models.ChannelMessage) { m.ResponseMode = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := models.CloneChannelMessage(voice)
			test.change(message)
			actor, valid := runtimeconversation.ConversationMessageInitiatingUser(conversation, message)
			if valid != test.valid || valid && actor != a {
				t.Fatalf("origin actor = %#v, valid=%v; want valid=%v", actor, valid, test.valid)
			}
		})
	}
}
