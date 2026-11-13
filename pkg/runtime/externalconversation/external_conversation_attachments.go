package externalconversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// MaximumExternalConversationAttachmentBytes bounds the files of one external
// message read in (a Slack or Telegram screenshot). They come back in a Skill's
// response, which may be up to skillgrpc.MaxResponseBytes, so 3 MiB of files
// still fit as base64.
const MaximumExternalConversationAttachmentBytes = 3 << 20

// MaximumExternalDeliveryAttachmentBytes bounds the files one reply sends out.
// They travel in the Skill execute request, which Atlas and every Skill's gRPC
// server cap at 4 MiB, so 2 MiB of files fit as base64.
const MaximumExternalDeliveryAttachmentBytes = 2 << 20

type AttachmentContent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Status    string `json:"status,omitempty"`
}

type AttachmentHostRequest struct {
	Endpoint   *models.ExternalConversationEndpoint
	Adapter    *skill.BoundConversationAdapter
	Event      models.NormalizedExternalConversationEvent
	Attachment models.ExternalConversationAttachment
}

type AttachmentHost interface {
	ReadExternalConversationAttachment(context.Context, AttachmentHostRequest) (*AttachmentContent, error)
}

func containsConversationFeature(features []capability.ConversationAdapterFeature, feature capability.ConversationAdapterFeature) bool {
	for _, candidate := range features {
		if candidate == feature {
			return true
		}
	}
	return false
}

func (w *InboxWorker) importAttachments(ctx context.Context, endpoint *models.ExternalConversationEndpoint, conversation *models.Conversation, event models.NormalizedExternalConversationEvent) ([]models.ConversationReference, []string, error) {
	if len(event.Attachments) == 0 {
		return nil, nil, nil
	}
	var refs []models.ConversationReference
	var notices []string
	if w.config.AttachmentHost == nil || w.config.AttachmentContent == nil || w.config.ContextCatalog == nil {
		return nil, []string{"files could not be downloaded"}, nil
	}
	adapter, err := w.config.ContextCatalog.ResolveConversationAdapterBinding(ctx, skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID)
	if err != nil || !externalConversationSnapshotMatchesBinding(endpoint, endpoint.Adapter, adapter) {
		return nil, nil, models.ErrExternalConversationConflict
	}
	if !containsConversationFeature(adapter.Adapter.Features, capability.ConversationFeatureAttachments) {
		return nil, []string{"this connection does not support attachments"}, nil
	}
	store, ok := w.store.(models.ArtifactStore)
	if !ok {
		return nil, nil, errors.New("artifact catalog is unavailable")
	}
	catalog := artifactcatalog.NewCatalog(store, models.WithClock(w.now))
	remaining := MaximumExternalConversationAttachmentBytes
	for _, file := range event.Attachments {
		id := stableExternalConversationID(endpoint.Scope, endpoint.ID, "file", event.ExternalMessageID+":"+file.ID)
		if existing, err := store.GetArtifact(ctx, endpoint.Scope, id, 1); err != nil {
			return nil, nil, err
		} else if existing != nil {
			if existing.Scope != endpoint.Scope || existing.Provenance.Owner == nil || *existing.Provenance.Owner != conversation.Owner {
				return nil, nil, models.ErrExternalConversationConflict
			}
			refs = append(refs, models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: id, Version: 1})
			remaining -= int(existing.SizeBytes)
			continue
		}
		if file.SizeBytes > int64(remaining) {
			notices = append(notices, file.Name+": size limit exceeded")
			continue
		}
		origin := *endpoint
		origin.Address = event.ExternalConversationID
		content, err := w.config.AttachmentHost.ReadExternalConversationAttachment(ctx, AttachmentHostRequest{Endpoint: &origin, Adapter: adapter, Event: event, Attachment: file})
		if err != nil {
			return nil, nil, err
		}
		if content == nil || content.ID != file.ID {
			return nil, nil, errors.New("attachment origin does not match")
		}
		if content.Status != "supplied" {
			status := "unavailable"
			switch content.Status {
			case "missing_scope", "size_limit", "unsupported_format", "unavailable":
				status = content.Status
			}
			notices = append(notices, file.Name+": "+status)
			continue
		}
		if len(content.Data) == 0 || len(content.Data) > remaining {
			notices = append(notices, file.Name+": size limit exceeded")
			continue
		}
		mediaType, _, err := mime.ParseMediaType(content.MediaType)
		if err != nil {
			return nil, nil, errors.New("attachment media type is invalid")
		}
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(content.Data))
		owner := conversation.Owner
		stored, err := w.config.AttachmentContent.Put(ctx, models.ArtifactContentWrite{Scope: endpoint.Scope, MediaType: mediaType, Reader: bytes.NewReader(content.Data), SizeBytes: int64(len(content.Data)), Digest: digest, Owner: &owner})
		if err != nil {
			return nil, nil, err
		}
		_, err = catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: &models.Artifact{ID: id, Version: 1, Scope: endpoint.Scope, Name: file.Name, MediaType: mediaType, ContentRef: stored.ContentRef, Digest: stored.Digest, SizeBytes: stored.SizeBytes, Classification: models.ArtifactClassificationConfidential, CreatedAt: event.OccurredAt,
			Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "service", ID: externalConversationActorID}, Owner: &owner, ConversationID: conversation.ID, SpaceID: conversation.SpaceID}, Metadata: map[string]interface{}{"provider": endpoint.Provider, "providerFileId": file.ID, "externalMessageId": event.ExternalMessageID}}})
		if err != nil {
			return nil, nil, err
		}
		refs = append(refs, models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: id, Version: 1})
		remaining -= len(content.Data)
	}
	return refs, notices, nil
}

// externalDeliveryFiles returns the artifacts message carries that an
// adapter receives as files, in reference order, and how many visuals it
// left out. A visual (a presentation surface) is never a file: its bytes are
// the surface contract, which a chat would show as a raw JSON snippet. A
// channel gets the answer without it, with no text in its place. Declaring
// capability.ConversationFeatureVisuals does not change this: a visual a
// provider can show will travel in its own DeliveryHostRequest field, which
// does not exist yet. catalog is nil when the store keeps no artifacts.
func externalDeliveryFiles(ctx context.Context, catalog models.ArtifactStore, scope models.Scope, message *models.ChannelMessage) ([]*models.Artifact, int, error) {
	var files []*models.Artifact
	visuals := 0
	seen := map[string]bool{}
	for _, ref := range message.References {
		if ref.Kind != models.ConversationReferenceArtifact {
			continue
		}
		key := fmt.Sprintf("%s:%d", ref.ID, ref.Version)
		if seen[key] {
			continue
		}
		seen[key] = true
		if catalog == nil {
			return nil, 0, errors.New("attachment delivery is unavailable")
		}
		artifact, err := catalog.GetArtifact(ctx, scope, ref.ID, ref.Version)
		if err != nil || artifact == nil || artifact.Scope != scope {
			return nil, 0, errors.New("attachment is unavailable")
		}
		if artifact.Type == presentation.ArtifactType {
			visuals++
			continue
		}
		files = append(files, artifact)
	}
	return files, visuals, nil
}

// externalDeliveryAttachments reads the bytes of message's files
// (externalDeliveryFiles) and reports how many visuals it left out.
func externalDeliveryAttachments(ctx context.Context, store models.ExternalConversationStore, content models.ArtifactContentStore, scope models.Scope, message *models.ChannelMessage) ([]AttachmentContent, int, error) {
	catalog, _ := store.(models.ArtifactStore)
	files, visuals, err := externalDeliveryFiles(ctx, catalog, scope, message)
	if err != nil {
		return nil, 0, err
	}
	if len(files) == 0 {
		return nil, visuals, nil
	}
	if content == nil || len(files) > 8 {
		return nil, 0, errors.New("attachment delivery is unavailable")
	}
	result := make([]AttachmentContent, 0, len(files))
	remaining := MaximumExternalDeliveryAttachmentBytes
	for _, artifact := range files {
		if artifact.SizeBytes < 0 || artifact.SizeBytes > int64(remaining) {
			return nil, 0, errors.New("attachment delivery exceeds limits")
		}
		reader, err := content.Open(ctx, scope, artifact.ContentRef)
		if err != nil {
			return nil, 0, errors.New("attachment bytes are unavailable")
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, int64(remaining)+1))
		reader.Close()
		if readErr != nil || int64(len(data)) != artifact.SizeBytes || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != artifact.Digest {
			return nil, 0, errors.New("attachment integrity check failed")
		}
		result = append(result, AttachmentContent{ID: fmt.Sprintf("%s:%d", artifact.ID, artifact.Version), Name: artifact.Name, MediaType: artifact.MediaType, Data: data})
		remaining -= len(data)
	}
	return result, visuals, nil
}
