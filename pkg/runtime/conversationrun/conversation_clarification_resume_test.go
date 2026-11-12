package conversationrun_test

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/control/controltest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestConversationClarificationReplayWithInactiveParent(t *testing.T) {
	for _, status := range []models.AgentRunStatus{models.AgentRunStatusPaused, models.AgentRunStatusCompleted} {
		for _, accepted := range []bool{false, true} {
			name := string(status) + "/fresh"
			if accepted {
				name = string(status) + "/accepted"
			}
			t.Run(name, func(t *testing.T) {
				f := newConversationActorFixture(t, models.Scope{Kind: "organization", ID: "shared"}, "writer")
				a := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-a"}
				trigger := f.post(t, conversation.PostChannelMessageRequest{Sender: a, IdempotencyKey: "request"})
				root, child, question := f.waitingQuestion(t, trigger)
				answer := f.post(t, conversation.PostChannelMessageRequest{Sender: a, Intent: models.MessageIntentAnswer, ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
				if accepted {
					child = f.schedule(t, answer)
					if child.Kind != models.RunKindAgentWork || len(child.PendingInterventions) != 1 {
						t.Fatalf("clarification was not accepted: %#v", child)
					}
				}
				activity := runtimeactivity.NewRunActivityService(f.store, f.store)
				var err error
				root, _, err = activity.TransitionRun(t.Context(), root.Scope, root.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: root.Revision, Status: models.AgentRunStatusRunning})
				if err != nil {
					t.Fatal(err)
				}
				root, _, err = activity.TransitionRun(t.Context(), root.Scope, root.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: root.Revision, Status: status})
				if err != nil {
					t.Fatal(err)
				}
				f.scheduler = conversationruntest.NewScheduler(t, f.store)
				if accepted {
					result, replayed, err := f.scheduler.ScheduleMessage(t.Context(), root.Scope, f.conversation.ID, answer.ID)
					if err != nil || !replayed || result == nil || result.Run.ID != child.ID || len(result.Run.PendingInterventions) != 1 {
						t.Fatalf("accepted answer replay lost original receipt: %#v, %v, %v", result, replayed, err)
					}
				} else {
					result, handled, err := f.scheduler.ResumeConversationAnswer(t.Context(), f.conversation, answer)
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

func TestConversationClarificationTargetsOnlyTheRepliedTask(t *testing.T) {
	f := newClarificationFixture(t, kerneltest.NewStore(t), "targets")
	q := f.question(t)
	other, err := portfolio.NewService(f.store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: f.root.Scope, Kind: models.RunKindAgentWork, Owner: f.root.Owner, AssignedAgentID: "writer", ParentRunID: f.root.ID, Goal: "Draft the outline", Source: models.RunSourceRequest})
	if err != nil {
		t.Fatal(err)
	}
	other, _ = controltest.AskUserParked(t, f.store, controltest.ClaimRun(t, f.store, other, "other-worker"), "other-worker", "Which audience should I use?")
	if _, err := f.scheduler.ReconcileScope(t.Context(), f.root.Scope); err != nil {
		t.Fatal(err)
	}
	answer := f.answer(t, "")
	// More than one waiting task: do not guess what an unthreaded message answers.
	if _, handled, err := f.scheduler.ResumeConversationAnswer(t.Context(), f.conversation, answer); err != nil || handled {
		t.Fatalf("ambiguous reply consumed: %v %v", handled, err)
	}
	answer.ResolvesMessageID = q.ID
	result, handled, err := f.scheduler.ResumeConversationAnswer(t.Context(), f.conversation, answer)
	if err != nil || !handled || result.Run.ID != f.child.ID {
		t.Fatalf("explicit reply = %#v %v %v", result, handled, err)
	}
	unchanged, _ := f.store.GetAgentRun(t.Context(), f.root.Scope, other.ID)
	if unchanged.Status != models.AgentRunStatusWaitingForEvent {
		t.Fatal("unrelated task woke")
	}
}
