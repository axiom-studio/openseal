package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

const (
	answerVisualNotShownEvent   = "turn.visual_not_shown"
	answerVisualNotShownSummary = "The agent's visual was not shown: a visual can only go with a reply in a chat."
)

// errInvalidAnswerVisual marks a visual rejected by validation, as opposed
// to a storage failure.
var errInvalidAnswerVisual = errors.New("invalid answer visual")

// answerVisualArtifactID is stable per Run, trigger and (for Team members
// answering in one round) participant, so a replayed answer references the
// visual stored by its first attempt.
func answerVisualArtifactID(runID, triggerID, participantID string) string {
	key := runID + "\x00" + triggerID
	if participantID != "" {
		key += "\x00" + participantID
	}
	return "visual-" + strings.TrimPrefix(hashString(key), "sha256:")[:40]
}

// teamAnswerVisual returns the hook a Team participation round uses to keep
// the visual a member attaches to its answer. Team members' visuals arrive
// on participation proposals rather than through HostedTurnRunner, so the
// host checks run here.
func (r *ConversationRunTurnRunner) teamAnswerVisual(run *AgentRun, triggerID string) func(context.Context, ConversationParticipant, presentation.Visual) (ConversationReference, error) {
	return func(ctx context.Context, participant ConversationParticipant, visual presentation.Visual) (ConversationReference, error) {
		var checks presentation.ValidateOptions
		if r.config.VisualChecks != nil {
			// The member publishes, so an approval binding must name an
			// action that member proposed.
			member := cloneAgentRun(run)
			member.AssignedAgentID = participant.ID
			checks = r.config.VisualChecks(member)
		}
		if err := validateHostedTurnVisual(ctx, &visual, AgentRunStatusCompleted, false, checks); err != nil {
			return ConversationReference{}, fmt.Errorf("%w: %w", errInvalidAnswerVisual, err)
		}
		return r.storeAnswerVisual(ctx, run, participant.ID, answerVisualArtifactID(run.ID, triggerID, participant.ID), visual)
	}
}

// storeAnswerVisual stores the visual that rides on an Agent's answer as an
// interactive surface artifact (version 1) with the given ID and returns the
// reference the answer message carries. No receipt is posted: the answer is
// the delivery. One-to-one and Team answers share it.
func (r *ConversationRunTurnRunner) storeAnswerVisual(ctx context.Context, run *AgentRun, participantID, id string, visual presentation.Visual) (ConversationReference, error) {
	if r.artifacts == nil || r.config.AttachmentContent == nil {
		return ConversationReference{}, errors.New("answer visuals require artifact and content storage")
	}
	reference := ConversationReference{Kind: ConversationReferenceArtifact, ID: id, Version: 1}
	existing, err := r.artifacts.GetArtifact(ctx, run.Scope, id, 1)
	if err != nil && !errors.Is(err, ErrArtifactNotFound) {
		return ConversationReference{}, err
	}
	if existing != nil {
		if existing.Type != presentation.ArtifactType || existing.Provenance.RunID != run.ID {
			return ConversationReference{}, ErrArtifactImmutable
		}
		return reference, nil
	}
	surface := visual.Surface(presentation.StateReady)
	content, err := presentation.Encode(surface)
	if err != nil {
		return ConversationReference{}, err
	}
	owner := run.Owner
	stored, err := r.config.AttachmentContent.Put(ctx, ArtifactContentWrite{
		Scope: run.Scope, MediaType: presentation.MediaType, Reader: bytes.NewReader(content), SizeBytes: int64(len(content)),
		Digest: hashBytes(content), Owner: &owner,
	})
	if err != nil {
		return ConversationReference{}, err
	}
	// The name is what a person sees if the card is ever listed: its title.
	name := strings.TrimSpace(surface.Title)
	if name == "" {
		name = "Card"
	}
	for len(name) > 480 {
		runes := []rune(name)
		name = string(runes[:len(runes)-1])
	}
	componentTypes := make([]interface{}, 0, len(surface.Components))
	for _, component := range surface.Components {
		componentTypes = append(componentTypes, component.Type)
	}
	_, err = NewArtifactCatalog(r.artifacts).Register(ctx, RegisterArtifactRequest{Artifact: &Artifact{
		ID: id, Version: 1, Scope: run.Scope, Name: name,
		Type: presentation.ArtifactType, MediaType: presentation.MediaType,
		ContentRef: stored.ContentRef, Digest: stored.Digest, SizeBytes: stored.SizeBytes, Classification: ArtifactClassificationInternal,
		MetadataSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"schema", "state", "componentTypes"},
			"properties": map[string]interface{}{
				"schema":         map[string]interface{}{"const": presentation.Schema},
				"state":          map[string]interface{}{"enum": []interface{}{string(presentation.StateStreaming), string(presentation.StateReady)}},
				"componentTypes": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": presentation.MaxComponents, "items": map[string]interface{}{"type": "string"}},
			},
		},
		Metadata: map[string]interface{}{"schema": presentation.Schema, "state": string(surface.State), "componentTypes": componentTypes},
		Provenance: ArtifactProvenance{
			Producer: ActivityActor{Type: "agent", ID: participantID}, Owner: &owner, RunID: run.ID, ObjectiveID: run.ObjectiveID,
		},
	}})
	if err != nil {
		return ConversationReference{}, err
	}
	return reference, nil
}
