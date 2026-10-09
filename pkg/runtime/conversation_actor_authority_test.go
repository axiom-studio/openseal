package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

type conversationActorFixture struct {
	store        *MemoryStore
	service      *ConversationService
	scheduler    *ConversationRunScheduler
	conversation *Conversation
}

func newConversationActorFixture(t *testing.T, scope Scope, agentID string) *conversationActorFixture {
	t.Helper()
	store := NewMemoryStore()
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID},
		Title: "Shared organization conversation", IdempotencyKey: "shared-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &conversationActorFixture{store, service, mustConversationRunScheduler(t, store), conversation}
}

func (f *conversationActorFixture) post(t *testing.T, request PostChannelMessageRequest) *ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	request.Scope = current.Scope
	request.ConversationID = current.ID
	request.ExpectedRevision = current.Revision
	request.Audience = ConversationAudience{Kind: ConversationAudienceChannel}
	request.RequiresResponse = true
	if request.Content == "" {
		request.Content = "Please continue with this request."
	}
	if request.Intent == "" {
		request.Intent = MessageIntentQuestion
	}
	posted, err := f.service.PostChannelMessage(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return posted.Message
}

func (f *conversationActorFixture) schedule(t *testing.T, message *ChannelMessage) *AgentRun {
	t.Helper()
	result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.conversation.Scope, f.conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule %s = %#v, %v", message.ID, result, err)
	}
	return result.Run
}

func (f *conversationActorFixture) waitingQuestion(t *testing.T, trigger *ChannelMessage) (*AgentRun, *AgentRun, *ChannelMessage) {
	t.Helper()
	root := f.schedule(t, trigger)
	activity := NewRunActivityService(f.store, f.store)
	var err error
	root, _, err = activity.TransitionRun(t.Context(), root.Scope, root.ID, RunTransitionRequest{ExpectedRevision: root.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	root, _, err = activity.TransitionRun(t.Context(), root.Scope, root.ID, RunTransitionRequest{ExpectedRevision: root.Revision, Status: AgentRunStatusWaitingForDependency, WakeCondition: &WakeCondition{Type: "run_dependencies", Reference: "draft"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := NewPortfolioService(f.store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: root.Scope, Kind: RunKindAgentWork, Owner: root.Owner, AssignedAgentID: root.Owner.ID,
		ParentRunID: root.ID, Goal: "Draft the requested report", Source: RunSourceRequest,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err = activity.TransitionRun(t.Context(), child.Scope, child.ID, RunTransitionRequest{ExpectedRevision: child.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err = activity.TransitionRun(t.Context(), child.Scope, child.ID, RunTransitionRequest{
		ExpectedRevision: child.Revision, Status: AgentRunStatusWaitingForEvent,
		WakeCondition: &WakeCondition{Type: "user_message", Reference: "user:next_message"},
		Output:        map[string]interface{}{"summary": "Who is the report for?"}, AppliedTurn: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.scheduler.ReconcileScope(t.Context(), root.Scope); err != nil {
		t.Fatal(err)
	}
	question, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), root.Scope, f.conversation.ID, clarificationQuestionKey(child))
	if err != nil || question == nil {
		t.Fatalf("projected question = %#v, %v", question, err)
	}
	return root, child, question
}

func (f *conversationActorFixture) assertRunUnchanged(t *testing.T, before *AgentRun) {
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
			f := newConversationActorFixture(t, Scope{Kind: "organization", ID: "shared"}, "writer")
			a := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-a"}
			b := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-b"}
			trigger := f.post(t, PostChannelMessageRequest{Sender: a, IdempotencyKey: "request-a"})
			root, child, question := f.waitingQuestion(t, trigger)
			request := PostChannelMessageRequest{Sender: b, Intent: MessageIntentAnswer, Content: "Use the finance team as the audience.", IdempotencyKey: "answer-b"}
			switch target {
			case "reply":
				request.ReplyToMessageID = question.ID
			case "resolves":
				request.ResolvesMessageID = question.ID
			}
			answerB := f.post(t, request)
			runB := f.schedule(t, answerB)
			if runB.ID == root.ID || runB.ID == child.ID || runB.Kind != RunKindConversation || runB.Context[conversationRunContextTriggerID] != answerB.ID {
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

			answerA := f.post(t, PostChannelMessageRequest{Sender: a, Intent: MessageIntentAnswer, ResolvesMessageID: question.ID, Content: "Write for general readers.", IdempotencyKey: "answer-a"})
			resumed := f.schedule(t, answerA)
			if resumed.ID != child.ID || resumed.Status != AgentRunStatusQueued || resumed.WakeCondition != nil || len(resumed.PendingInterventions) != 1 || resumed.PendingInterventions[0].Actor.ID != a.ID {
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

func TestConversationClarificationReplayWithInactiveParent(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusPaused, AgentRunStatusCompleted} {
		for _, accepted := range []bool{false, true} {
			name := string(status) + "/fresh"
			if accepted {
				name = string(status) + "/accepted"
			}
			t.Run(name, func(t *testing.T) {
				f := newConversationActorFixture(t, Scope{Kind: "organization", ID: "shared"}, "writer")
				a := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-a"}
				trigger := f.post(t, PostChannelMessageRequest{Sender: a, IdempotencyKey: "request"})
				root, child, question := f.waitingQuestion(t, trigger)
				answer := f.post(t, PostChannelMessageRequest{Sender: a, Intent: MessageIntentAnswer, ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
				if accepted {
					child = f.schedule(t, answer)
					if child.Kind != RunKindAgentWork || len(child.PendingInterventions) != 1 {
						t.Fatalf("clarification was not accepted: %#v", child)
					}
				}
				activity := NewRunActivityService(f.store, f.store)
				var err error
				root, _, err = activity.TransitionRun(t.Context(), root.Scope, root.ID, RunTransitionRequest{ExpectedRevision: root.Revision, Status: AgentRunStatusRunning})
				if err != nil {
					t.Fatal(err)
				}
				root, _, err = activity.TransitionRun(t.Context(), root.Scope, root.ID, RunTransitionRequest{ExpectedRevision: root.Revision, Status: status})
				if err != nil {
					t.Fatal(err)
				}
				f.scheduler = mustConversationRunScheduler(t, f.store)
				if accepted {
					result, replayed, err := f.scheduler.ScheduleMessage(t.Context(), root.Scope, f.conversation.ID, answer.ID)
					if err != nil || !replayed || result == nil || result.Run.ID != child.ID || len(result.Run.PendingInterventions) != 1 {
						t.Fatalf("accepted answer replay lost original receipt: %#v, %v, %v", result, replayed, err)
					}
				} else {
					result, handled, err := f.scheduler.resumeConversationAnswer(t.Context(), f.conversation, answer)
					if err != nil || handled || result != nil {
						t.Fatalf("fresh answer resumed inactive parent's work: %#v, %v, %v", result, handled, err)
					}
				}
				f.assertRunUnchanged(t, root)
				f.assertRunUnchanged(t, child)
			})
		}
	}
}

func TestConversationFollowUpsSeparateMembersIncludingDelayedScheduling(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "in-order"
		if reverse {
			name = "reverse-order"
		}
		t.Run(name, func(t *testing.T) {
			f := newConversationActorFixture(t, Scope{Kind: "organization", ID: "shared"}, "writer")
			a := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-a"}
			b := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-b"}
			requests := []PostChannelMessageRequest{
				{Sender: a, IdempotencyKey: "a-first"},
				{Sender: b, IdempotencyKey: "b-first"},
				{Sender: a, IdempotencyKey: "a-second"},
			}
			messages := make([]*ChannelMessage, len(requests))
			runs := make([]*AgentRun, len(requests))
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
			if err != nil || memberA.Status != AgentRunStatusQueued || len(conversationRunFollowUps(memberA)) != 1 {
				t.Fatalf("member A Run = %#v, %v", memberA, err)
			}
			first, second := messages[0], messages[2]
			if reverse {
				first, second = second, first
			}
			if memberA.Context[conversationRunContextTriggerID] != first.ID || !conversationRunHasFollowUp(memberA, second.ID) {
				t.Fatalf("member A Run trigger/follow-up = %#v", memberA)
			}
			memberB, err := f.store.GetAgentRun(t.Context(), f.conversation.Scope, runs[1].ID)
			if err != nil || memberB.Status != AgentRunStatusQueued || len(memberB.PendingInterventions) != 0 || memberB.Context[conversationRunContextTriggerID] != messages[1].ID {
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
	a := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-a"}
	trigger := f.post(t, PostChannelMessageRequest{Sender: a, IdempotencyKey: "request-a"})
	root := f.schedule(t, trigger)
	running, _, err := NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), scope, root.ID, RunTransitionRequest{ExpectedRevision: root.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewActionCoordinator(f.store, f.store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, Reason: "review required", ApprovalTTL: time.Hour, EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "reviewer"}}}, nil
	}))
	proposal, err := coordinator.Propose(t.Context(), ProposeActionRequest{
		Scope: scope, RunID: running.ID, DeploymentID: "research-agent", SkillID: "outreach", SkillVersion: "1.0.0",
		Action: "reply", Arguments: map[string]interface{}{"body": "reviewed report"}, IdempotencyKey: "reviewed-action", Summary: "Send reviewed report",
	})
	if err != nil || proposal == nil || proposal.Approval == nil || proposal.Run.Status != AgentRunStatusWaitingForApproval {
		t.Fatalf("pending approval = %#v, %v", proposal, err)
	}
	messageB := f.post(t, PostChannelMessageRequest{Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "member-b"}, IdempotencyKey: "request-b"})
	runB := f.schedule(t, messageB)
	if runB.ID == root.ID || runB.Context[conversationRunContextTriggerID] != messageB.ID {
		t.Fatalf("member B inherited member A's run: %#v", runB)
	}
	f.assertRunUnchanged(t, proposal.Run)
	approval, err := f.store.GetApproval(t.Context(), scope, proposal.Approval.ID)
	if err != nil || !reflect.DeepEqual(approval, proposal.Approval) {
		t.Fatalf("member B changed pending approval: %#v, %v", approval, err)
	}
	call, err := f.store.GetActionCall(t.Context(), scope, proposal.Call.ID)
	if err != nil || !reflect.DeepEqual(call, proposal.Call) {
		t.Fatalf("member B changed pending action: %#v, %v", call, err)
	}

	// The requesting member's new message is delivered to their waiting Run.
	// The approval stays pending; the Run sees the message once it resumes.
	messageA := f.post(t, PostChannelMessageRequest{Sender: a, IdempotencyKey: "follow-up-a"})
	delivered := f.schedule(t, messageA)
	if delivered.ID != root.ID || delivered.Status != AgentRunStatusWaitingForApproval || delivered.WakeCondition == nil ||
		!conversationRunHasFollowUp(delivered, messageA.ID) {
		t.Fatalf("follow-up was not delivered to the waiting Run: %#v", delivered)
	}
	approval, err = f.store.GetApproval(t.Context(), scope, proposal.Approval.ID)
	if err != nil || approval.Status != ApprovalStatusPending {
		t.Fatalf("follow-up changed the pending approval: %#v, %v", approval, err)
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
		{name: "voice initiator", serviceID: VoiceCallCoordinatorParticipantID, response: "spoken", initiator: true, wantResumed: true},
		{name: "unrelated service", serviceID: "other-service", response: "spoken", initiator: true},
		{name: "non voice event", serviceID: VoiceCallCoordinatorParticipantID, initiator: true},
		{name: "missing initiator", serviceID: VoiceCallCoordinatorParticipantID, response: "spoken"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConversationActorFixture(t, Scope{Kind: "organization", ID: "shared-voice"}, "writer")
			a := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-a"}
			request := PostChannelMessageRequest{Sender: ConversationParticipant{Type: ConversationParticipantService, ID: test.serviceID}, Intent: MessageIntentUpdate, ResponseMode: test.response, IdempotencyKey: "call-start"}
			if test.initiator {
				request.Initiator = &a
			}
			trigger := f.post(t, request)
			root, child, question := f.waitingQuestion(t, trigger)
			answerB := f.post(t, PostChannelMessageRequest{Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "member-b"}, Intent: MessageIntentAnswer, ResolvesMessageID: question.ID, IdempotencyKey: "answer-b"})
			f.schedule(t, answerB)
			f.assertRunUnchanged(t, root)
			f.assertRunUnchanged(t, child)
			answerA := f.post(t, PostChannelMessageRequest{Sender: a, Intent: MessageIntentAnswer, ResolvesMessageID: question.ID, IdempotencyKey: "answer-a"})
			result := f.schedule(t, answerA)
			if test.wantResumed {
				if result.ID != child.ID || result.Status != AgentRunStatusQueued || len(result.PendingInterventions) != 1 || result.PendingInterventions[0].Actor.ID != a.ID {
					t.Fatalf("verified voice initiator failed to resume clarification: %#v", result)
				}
			} else {
				if result.ID == child.ID || result.ID == root.ID || result.Context[conversationRunContextTriggerID] != answerA.ID {
					t.Fatalf("untrusted service origin conferred continuation authority: %#v", result)
				}
				f.assertRunUnchanged(t, root)
				f.assertRunUnchanged(t, child)
			}
		})
	}
}

func TestConversationFollowUpUsesVoiceInitiator(t *testing.T) {
	f := newConversationActorFixture(t, Scope{Kind: "organization", ID: "shared-voice"}, "writer")
	a := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-a"}
	trigger := f.post(t, PostChannelMessageRequest{
		Sender: ConversationParticipant{Type: ConversationParticipantService, ID: VoiceCallCoordinatorParticipantID}, Initiator: &a,
		Intent: MessageIntentUpdate, ResponseMode: "spoken", IdempotencyKey: "call-start",
	})
	voice := f.schedule(t, trigger)
	messageB := f.post(t, PostChannelMessageRequest{Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "member-b"}, IdempotencyKey: "request-b"})
	runB := f.schedule(t, messageB)
	f.assertRunUnchanged(t, voice)
	messageA := f.post(t, PostChannelMessageRequest{Sender: a, IdempotencyKey: "request-a"})
	f.schedule(t, messageA)
	current, err := f.store.GetAgentRun(t.Context(), voice.Scope, voice.ID)
	if err != nil || current.Status != AgentRunStatusQueued || !conversationRunHasFollowUp(current, messageA.ID) {
		t.Fatalf("call initiator's message was not delivered to their call Run: %#v, %v", current, err)
	}
	f.assertRunUnchanged(t, runB)
}

func TestConversationInitiatingUserRejectsUnboundOrMalformedOrigin(t *testing.T) {
	conversation := &Conversation{ID: "shared", Scope: Scope{Kind: "organization", ID: "organization-a"}}
	a := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-a"}
	voice := &ChannelMessage{
		Scope: conversation.Scope, ConversationID: conversation.ID,
		Sender: ConversationParticipant{Type: ConversationParticipantService, ID: VoiceCallCoordinatorParticipantID}, Initiator: &a,
		Intent: MessageIntentUpdate, ResponseMode: "spoken", RequiresResponse: true,
	}
	for _, test := range []struct {
		name   string
		change func(*ChannelMessage)
		valid  bool
	}{
		{name: "verified voice", change: func(*ChannelMessage) {}, valid: true},
		{name: "human sender", change: func(m *ChannelMessage) { m.Sender = a; m.Initiator = nil }, valid: true},
		{name: "foreign scope", change: func(m *ChannelMessage) { m.Scope.ID = "organization-b" }},
		{name: "foreign conversation", change: func(m *ChannelMessage) { m.ConversationID = "other-conversation" }},
		{name: "missing initiator", change: func(m *ChannelMessage) { m.Initiator = nil }},
		{name: "empty initiator identity", change: func(m *ChannelMessage) { m.Initiator.ID = "" }},
		{name: "invalid initiator identity", change: func(m *ChannelMessage) { m.Initiator.ID = "member/a" }},
		{name: "nonhuman initiator", change: func(m *ChannelMessage) { m.Initiator.Type = ConversationParticipantAgent }},
		{name: "wrong service", change: func(m *ChannelMessage) { m.Sender.ID = "other-service" }},
		{name: "wrong intent", change: func(m *ChannelMessage) { m.Intent = MessageIntentQuestion }},
		{name: "no response requested", change: func(m *ChannelMessage) { m.RequiresResponse = false }},
		{name: "nonspoken event", change: func(m *ChannelMessage) { m.ResponseMode = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := cloneChannelMessage(voice)
			test.change(message)
			actor, valid := conversationMessageInitiatingUser(conversation, message)
			if valid != test.valid || valid && actor != a {
				t.Fatalf("origin actor = %#v, valid=%v; want valid=%v", actor, valid, test.valid)
			}
		})
	}
}
