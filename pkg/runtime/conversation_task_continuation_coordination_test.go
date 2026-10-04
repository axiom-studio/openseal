package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConversationCoordinatorContinuationReusesProposalsAcrossHandoff(t *testing.T) {
	for _, scenario := range []string{"during proposals", "before coordination", "settings changed"} {
		t.Run(scenario, func(t *testing.T) {
			store := NewMemoryStore()
			service := NewConversationService(store)
			scope := Scope{Kind: "tenant", ID: "team-continuation"}
			conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Release review", IdempotencyKey: "team-continuation"})
			if err != nil {
				t.Fatal(err)
			}
			trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Is the release ready?", "team-continuation-trigger")
			result, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, trigger.ID)
			if err != nil || result == nil {
				t.Fatalf("schedule: %#v %v", result, err)
			}
			run := backdateContinuationLifecycleRun(t, store, result.Run, time.Now().Add(-ConversationTaskForegroundTimeout-time.Second))
			conversation, err = service.GetConversation(t.Context(), scope, conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := cloneConversation(conversation)
			promote := func(ctx context.Context) error {
				accepted, err := store.PromoteConversationTask(ctx, ConversationTaskPromotionRequest{Scope: scope, RunID: run.ID, Now: time.Now()})
				if err == nil && accepted == nil {
					return errors.New("handoff did not commit")
				}
				return err
			}
			if scenario == "before coordination" {
				if err := promote(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			coordinator, err := NewConversationCoordinator(service, ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
				return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer-a"}, SemanticRoles: []string{"reviewer"}}, {Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer-b"}, SemanticRoles: []string{"operator"}}}, nil
			}), ParticipationProposalProviderFunc(func(ctx context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
				calls++
				if calls == 1 && scenario != "before coordination" {
					if err := promote(ctx); err != nil {
						return ParticipationProposal{}, err
					}
					if scenario == "settings changed" {
						current, err := service.GetConversation(ctx, scope, conversation.ID)
						if err != nil {
							return ParticipationProposal{}, err
						}
						title := "Changed release policy"
						if _, err := service.UpdateConversation(ctx, UpdateConversationRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision, Title: &title}); err != nil {
							return ParticipationProposal{}, err
						}
					}
				}
				return ParticipationProposal{WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "The release has been reviewed by " + input.Participant.ID, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: trigger.ID, Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true}}, nil
			}), ConversationCoordinatorConfig{MaximumParticipants: 2, MaximumConcurrency: 1, RecentMessageLimit: 20, ProposalTimeout: time.Second, PresenceTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			round, err := coordinator.Coordinate(t.Context(), ConversationCoordinationRequest{conversationSnapshot: before, Scope: scope, ConversationID: conversation.ID, ExpectedRevision: before.Revision, TriggerMessageID: trigger.ID, MessageReferences: []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}, MaximumConcurrency: 1, IdempotencyKey: "team-continuation-round"})
			if scenario == "settings changed" {
				if !errors.Is(err, ErrRevisionConflict) || calls != 1 || round != nil {
					t.Fatalf("configuration drift was accepted: %#v calls=%d err=%v", round, calls, err)
				}
				return
			}
			if err != nil || round == nil || calls != 2 || len(round.Messages) == 0 {
				t.Fatalf("handoff discarded or reran participant proposals: %#v calls=%d err=%v", round, calls, err)
			}
			task, err := store.FindConversationTaskByWorkRunID(t.Context(), scope, run.ID)
			if err != nil || task == nil {
				t.Fatalf("Task: %#v %v", task, err)
			}
			for _, message := range round.Messages {
				found := false
				for _, ref := range message.References {
					if ref.Kind == ConversationReferenceTask && ref.ID == task.ID {
						found = true
					}
				}
				if !found {
					t.Fatalf("Team final response lacks canonical Task reference: %#v", message)
				}
			}
		})
	}
}
