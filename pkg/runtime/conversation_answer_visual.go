package runtime

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

// answerVisualArtifactID is stable per Run and trigger, so a replayed answer
// references the visual stored by its first attempt.
func answerVisualArtifactID(runID, triggerID string) string {
	return "visual-" + strings.TrimPrefix(hashString(runID+"\x00"+triggerID), "sha256:")[:40]
}

// storeAnswerVisual stores the visual that rides on an Agent's answer as an
// interactive surface artifact (version 1) and returns the reference the
// answer message carries. No receipt is posted: the answer is the delivery.
func (r *ConversationRunTurnRunner) storeAnswerVisual(ctx context.Context, run *AgentRun, participantID string, trigger *ChannelMessage, visual presentation.Visual) (ConversationReference, error) {
	if r.artifacts == nil || r.config.AttachmentContent == nil {
		return ConversationReference{}, errors.New("answer visuals require artifact and content storage")
	}
	id := answerVisualArtifactID(run.ID, trigger.ID)
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
	name := strings.TrimSpace(surface.Title)
	for len(name) > 480 {
		runes := []rune(name)
		name = string(runes[:len(runes)-1])
	}
	componentTypes := make([]interface{}, 0, len(surface.Components))
	for _, component := range surface.Components {
		componentTypes = append(componentTypes, component.Type)
	}
	_, err = NewArtifactCatalog(r.artifacts).Register(ctx, RegisterArtifactRequest{Artifact: &Artifact{
		ID: id, Version: 1, Scope: run.Scope, Name: name + ".surface.json",
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
