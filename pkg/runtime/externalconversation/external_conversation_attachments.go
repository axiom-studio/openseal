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
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Bound the wire payload below the default four MiB gRPC envelope limit.
const MaximumExternalConversationAttachmentBytes = 2 << 20

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

func externalDeliveryAttachments(ctx context.Context, store models.ExternalConversationStore, content models.ArtifactContentStore, scope models.Scope, message *models.ChannelMessage) ([]AttachmentContent, error) {
	var result []AttachmentContent
	seen := map[string]bool{}
	catalog, ok := store.(models.ArtifactStore)
	remaining := MaximumExternalConversationAttachmentBytes
	for _, ref := range message.References {
		if ref.Kind != models.ConversationReferenceArtifact {
			continue
		}
		key := fmt.Sprintf("%s:%d", ref.ID, ref.Version)
		if seen[key] {
			continue
		}
		seen[key] = true
		if !ok || content == nil || len(result) >= 8 {
			return nil, errors.New("attachment delivery is unavailable")
		}
		artifact, err := catalog.GetArtifact(ctx, scope, ref.ID, ref.Version)
		if err != nil || artifact == nil || artifact.Scope != scope || artifact.SizeBytes < 0 || artifact.SizeBytes > int64(remaining) {
			return nil, errors.New("attachment delivery exceeds limits or is unavailable")
		}
		reader, err := content.Open(ctx, scope, artifact.ContentRef)
		if err != nil {
			return nil, errors.New("attachment bytes are unavailable")
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, int64(remaining)+1))
		reader.Close()
		if readErr != nil || int64(len(data)) != artifact.SizeBytes || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != artifact.Digest {
			return nil, errors.New("attachment integrity check failed")
		}
		result = append(result, AttachmentContent{ID: key, Name: artifact.Name, MediaType: artifact.MediaType, Data: data})
		remaining -= len(data)
	}
	return result, nil
}
