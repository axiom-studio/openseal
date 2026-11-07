package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const conversationArtifactReceiptService = "openseal.artifact-delivery"

// runActionArtifactReferences returns the artifact references of run's
// succeeded governed tool calls, in entry order, deduplicated by (id,
// version). They ride on the Run's final answer. A Run with no session log
// (or stores that cannot read it) has none.
func runActionArtifactReferences(ctx context.Context, sessions loopSessionEntryLister, actions actionCallReader, run *AgentRun) ([]ConversationReference, error) {
	if sessions == nil || actions == nil || run == nil || run.ID == "" {
		return nil, nil
	}
	entries, err := sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, States: []ToolEntryState{ToolEntryCompleted}, Limit: maxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	var references []ConversationReference
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if entry == nil || entry.ActionCallID == "" {
			continue
		}
		call, err := actions.GetActionCall(ctx, run.Scope, entry.ActionCallID)
		if errors.Is(err, ErrActionNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if call == nil || call.RunID != run.ID || call.Status != ActionCallStatusSucceeded {
			continue
		}
		for _, ref := range actionCallArtifactReferences(call) {
			key := fmt.Sprintf("%s\x00%d", ref.ID, ref.Version)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			references = append(references, ref)
		}
	}
	return references, nil
}

// actionCallArtifactReferences parses call.Output["artifactRefs"]: valid
// opaque ids of at most 256 bytes with a version of at least 1; anything
// else is skipped.
func actionCallArtifactReferences(call *ActionCall) []ConversationReference {
	if call == nil || call.Output == nil {
		return nil
	}
	raw, ok := call.Output["artifactRefs"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var artifacts []struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if json.Unmarshal(encoded, &artifacts) != nil {
		return nil
	}
	references := make([]ConversationReference, 0, len(artifacts))
	for _, artifact := range artifacts {
		id := strings.TrimSpace(artifact.ID)
		if !validOpaqueIdentifier(id, 256) || artifact.Version < 1 {
			continue
		}
		references = append(references, ConversationReference{Kind: ConversationReferenceArtifact, ID: id, Version: artifact.Version})
	}
	return references
}

// postToolArtifactReceipt publishes the receipt for one completed governed
// tool entry (the AfterTool hook): the files that call saved, as soon as it
// saved them. It is a kernel receipt, not a model answer: it neither resolves
// the request nor starts work, and its service sender keeps scheduler wakes
// and spoken-reply consumers from taking it for the final answer. Only exact
// immutable outputs whose stored provenance proves this Run and call are
// published.
func (r *ConversationRunTurnRunner) postToolArtifactReceipt(ctx context.Context, actions actionCallReader, run *AgentRun, conversation *Conversation, trigger *ChannelMessage, entry *SessionEntry) error {
	if r.artifacts == nil || actions == nil || run == nil || conversation == nil || trigger == nil || entry == nil {
		return nil
	}
	if run.Scope != conversation.Scope || run.Owner != conversation.Owner || trigger.Scope != run.Scope || trigger.ConversationID != conversation.ID {
		return ErrInvalidConversation
	}
	actionID := strings.TrimSpace(entry.ActionCallID)
	if entry.Type != SessionEntryTool || entry.State != ToolEntryCompleted || entry.RunID != run.ID || !validOpaqueIdentifier(actionID, 256) {
		return nil
	}
	call, err := actions.GetActionCall(ctx, run.Scope, actionID)
	if errors.Is(err, ErrActionNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if call == nil || call.RunID != run.ID || call.Status != ActionCallStatusSucceeded {
		return nil
	}
	references := []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}
	for _, ref := range actionCallArtifactReferences(call) {
		artifact, err := r.artifacts.GetArtifact(ctx, run.Scope, ref.ID, ref.Version)
		if errors.Is(err, ErrArtifactNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		// An action may mention input or unrelated artifacts.
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
