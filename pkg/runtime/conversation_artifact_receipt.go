package runtime

import (
	"context"
	"errors"
	"strings"
)

const conversationArtifactReceiptService = "openseal.artifact-delivery"

// postConversationActionArtifacts publishes a kernel receipt, not a model
// answer. It neither resolves the request nor authorizes another model call.
// The service receipt identity prevents scheduler wakes and spoken-reply
// consumers from mistaking an intermediate file for the final answer.
func (r *ConversationRunTurnRunner) postConversationActionArtifacts(ctx context.Context, run *AgentRun, conversation *Conversation, trigger *ChannelMessage) error {
	if r.artifacts == nil || run == nil || conversation == nil || trigger == nil {
		return nil
	}
	if run.Scope != conversation.Scope || run.Owner != conversation.Owner || trigger.Scope != run.Scope || trigger.ConversationID != conversation.ID {
		return ErrInvalidConversation
	}
	last, _ := run.Checkpoint["lastAction"].(map[string]interface{})
	actionID, _ := last["actionCallId"].(string)
	if !validOpaqueIdentifier(strings.TrimSpace(actionID), 256) {
		return nil
	}
	references := []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}
	for _, ref := range conversationActionArtifactReferences(run) {
		artifact, err := r.artifacts.GetArtifact(ctx, run.Scope, ref.ID, ref.Version)
		if errors.Is(err, ErrArtifactNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		// An action may mention input or unrelated artifacts. Publish only exact
		// immutable outputs whose stored provenance proves this run and action.
		if artifact == nil || artifact.Scope != run.Scope || artifact.ID != ref.ID || artifact.Version != ref.Version ||
			artifact.Provenance.RunID != run.ID || artifact.Provenance.ActionID != actionID ||
			artifact.Provenance.Owner == nil || *artifact.Provenance.Owner != run.Owner {
			continue
		}
		references = append(references, ref)
	}
	if len(references) == 1 {
		return nil
	}
	key := "conversation-action-artifacts:" + hashString(run.Scope.Kind+"\x00"+run.Scope.ID+"\x00"+run.ID+"\x00"+actionID)
	for range 3 {
		current, err := r.conversations.GetConversation(ctx, run.Scope, conversation.ID)
		if err != nil {
			return err
		}
		if current.Owner != run.Owner {
			return ErrInvalidConversation
		}
		if r.config.RequireParticipationOptIn && !conversationParticipationAllows(current, trigger) {
			return errConversationParticipationStopped
		}
		_, err = r.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: run.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: conversationArtifactReceiptService},
			Intent: MessageIntentUpdate, Content: "Files saved by the completed action.",
			Audience:         ConversationAudience{Kind: ConversationAudienceChannel},
			ReplyToMessageID: trigger.ID, References: references, IdempotencyKey: key,
		})
		if !errors.Is(err, ErrRevisionConflict) {
			return err
		}
	}
	return ErrRevisionConflict
}
