package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestConversationArtifactReceiptSurvivesModelFailure(t *testing.T) {
	for _, modelFails := range []bool{true, false} {
		t.Run(map[bool]string{true: "failure", false: "success"}[modelFails], func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryStore()
			service := NewConversationService(store)
			scope := Scope{Kind: "tenant", ID: "artifact-receipt"}
			conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}, Title: "Files", IdempotencyKey: "files",
			})
			if err != nil {
				t.Fatal(err)
			}
			trigger := receiptTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Create a PDF and explain it.", "create")
			scheduled, _, err := receiptTestScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
			if err != nil {
				t.Fatal(err)
			}
			run := cloneAgentRun(scheduled.Run)
			artifact := receiptTestArtifact("completed-pdf", 1)
			artifact.Scope = scope
			artifact.Provenance.Owner = &run.Owner
			artifact.Provenance.RunID = run.ID
			artifact.Provenance.ActionID = "create-pdf-action"
			if err := store.CreateArtifactVersion(ctx, artifact, 0); err != nil {
				t.Fatal(err)
			}
			run.Checkpoint = map[string]interface{}{"lastAction": map[string]interface{}{
				"actionCallId": artifact.Provenance.ActionID, "status": ActionCallStatusSucceeded,
				"result": map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": artifact.ID, "version": artifact.Version}}},
			}}
			modelErr := errors.New("model authorization denied")
			calls := 0
			resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
				return &TurnRunnerBinding{DeploymentID: "agent-42", DefinitionID: "definition", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
					calls++
					messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
					if err != nil || len(messages) < 2 || messages[1].Sender.Type != ConversationParticipantService {
						t.Fatalf("receipt must exist before model call: %#v, %v", messages, err)
					}
					if modelFails {
						return nil, modelErr
					}
					return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Drafted reply", "report": "Your PDF is ready."}}, nil
				})}, nil
			})
			runner, err := NewConversationRunTurnRunner(store, receiptTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
			if err != nil {
				t.Fatal(err)
			}
			binding, err := runner.ResolveTurnRunner(ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: run})
				if modelFails && (!errors.Is(err, modelErr) || outcome != nil) {
					t.Fatalf("failure must remain failure: %#v %v", outcome, err)
				}
				if !modelFails && (err != nil || outcome.NextRunStatus != AgentRunStatusCompleted) {
					t.Fatalf("success: %#v %v", outcome, err)
				}
			}
			messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
			want := 3
			if modelFails {
				want = 2
			}
			if err != nil || len(messages) != want || calls != 2 {
				t.Fatalf("duplicate receipt or lost continuation: messages=%d calls=%d err=%v", len(messages), calls, err)
			}
			if !modelFails && messages[2].Content != "Your PDF is ready." {
				t.Fatalf("complete report was replaced by its status summary: %#v", messages[2])
			}
			receipt := messages[1]
			if receipt.ResolvesMessageID != "" || receipt.RequiresResponse || receipt.Intent != MessageIntentUpdate || conversationMessageStartsRun(conversation, receipt) {
				t.Fatalf("receipt must not finish or restart work: %#v", receipt)
			}
			if receipt.ReplyToMessageID != trigger.ID || len(receipt.References) != 2 || receipt.References[1] != (ConversationReference{Kind: ConversationReferenceArtifact, ID: artifact.ID, Version: 1}) {
				t.Fatalf("receipt provenance: %#v", receipt)
			}
		})
	}
}

func TestConversationArtifactReceiptRequiresExactProvenance(t *testing.T) {
	for _, mismatch := range []string{"tenant", "owner", "run", "action", "version", "failed", "missing-action-id"} {
		t.Run(mismatch, func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryStore()
			service := NewConversationService(store)
			scope := Scope{Kind: "tenant", ID: "one"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
			conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Files", IdempotencyKey: "files"})
			if err != nil {
				t.Fatal(err)
			}
			trigger := receiptTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "PDF please", "trigger")
			artifact := receiptTestArtifact("file", 1)
			artifact.Provenance = ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "agent"}, Owner: &owner, RunID: "run", ActionID: "action"}
			last := map[string]interface{}{"actionCallId": "action", "status": ActionCallStatusSucceeded, "result": map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": "file", "version": 1}}}}
			switch mismatch {
			case "tenant":
				artifact.Scope.ID = "other"
			case "owner":
				artifact.Provenance.Owner = &ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}
			case "run":
				artifact.Provenance.RunID = "other"
			case "action":
				artifact.Provenance.ActionID = "other"
			case "version":
				last["result"] = map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": "file", "version": 2}}}
			case "failed":
				last["status"] = ActionCallStatusFailed
			case "missing-action-id":
				delete(last, "actionCallId")
			}
			if err := store.CreateArtifactVersion(ctx, artifact, 0); err != nil {
				t.Fatal(err)
			}
			runner := &ConversationRunTurnRunner{conversations: service, artifacts: store}
			run := &AgentRun{ID: "run", Scope: scope, Owner: owner, Checkpoint: map[string]interface{}{"lastAction": last}}
			if err := runner.postConversationActionArtifacts(ctx, run, conversation, trigger); err != nil {
				t.Fatal(err)
			}
			messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
			if err != nil || len(messages) != 1 {
				t.Fatalf("unproven artifact published: %#v %v", messages, err)
			}
		})
	}
}

func receiptTestScheduler(t *testing.T, store *MemoryStore) *ConversationRunScheduler {
	t.Helper()
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

func receiptTestCoordinator(t *testing.T, service *ConversationService) *ConversationCoordinator {
	t.Helper()
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{
			Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"}, SemanticRoles: []string{"developer"},
		}}, nil
	})
	proposals := ParticipationProposalProviderFunc(func(context.Context, ParticipationProposalContext) (ParticipationProposal, error) {
		return ParticipationProposal{
			WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Telemetry confirms all three replicas passed the smoke checks.",
			Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			Signals:  ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true},
		}, nil
	})
	coordinator, err := NewConversationCoordinator(service, participants, proposals, DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func receiptTestMessage(
	t *testing.T,
	service *ConversationService,
	conversation *Conversation,
	senderType ConversationParticipantType,
	intent ConversationMessageIntent,
	content string,
	key string,
) *ChannelMessage {
	t.Helper()
	result, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: senderType, ID: "sender-" + string(senderType)},
		Intent: intent, Content: content, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

func receiptTestArtifact(id string, version int64) *Artifact {
	return &Artifact{ID: id, Version: version, Scope: Scope{Kind: "tenant", ID: "one"}, Name: "file.pdf",
		Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "agent-42"}},
		ContentRef: "object-store:" + id, Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10, Classification: ArtifactClassificationInternal}
}
