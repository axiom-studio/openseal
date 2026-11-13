package externalconversation

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// artifactBytes serves stored artifact bytes by content ref.
type artifactBytes map[string][]byte

func (b artifactBytes) Put(context.Context, models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	return models.ArtifactStoredContent{}, errors.New("not used")
}

func (b artifactBytes) Open(_ context.Context, _ models.Scope, ref string) (io.ReadCloser, error) {
	data, ok := b[ref]
	if !ok {
		return nil, errors.New("no such content")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// saveArtifact registers an artifact of type kind holding data and returns
// the reference a message carries to it.
func saveArtifact(t *testing.T, store models.ArtifactStore, content artifactBytes, scope models.Scope, owner models.ObjectiveOwner, id, kind, mediaType string, data []byte) models.ConversationReference {
	t.Helper()
	content[id] = data
	_, err := artifactcatalog.NewCatalog(store).Register(t.Context(), artifactcatalog.RegisterArtifactRequest{Artifact: &models.Artifact{
		ID: id, Version: 1, Scope: scope, Name: id, Type: kind, MediaType: mediaType,
		ContentRef: id, Digest: models.SHA256Digest(data), SizeBytes: int64(len(data)),
		Classification: models.ArtifactClassificationInternal, CreatedAt: time.Now().UTC(),
		Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: owner.ID}, Owner: &owner},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: id, Version: 1}
}

func saveVisual(t *testing.T, store models.ArtifactStore, content artifactBytes, scope models.Scope, owner models.ObjectiveOwner) models.ConversationReference {
	return saveArtifact(t, store, content, scope, owner, "visual-1", presentation.ArtifactType, presentation.MediaType, []byte(`{"schema":"`+presentation.Schema+`"}`))
}

func saveFile(t *testing.T, store models.ArtifactStore, content artifactBytes, scope models.Scope, owner models.ObjectiveOwner) models.ConversationReference {
	return saveArtifact(t, store, content, scope, owner, "report-1", "file", "text/csv", []byte("a,b\n1,2\n"))
}

// withAdapterFeatures re-registers the endpoint's adapter with features
// added and returns the catalog that resolves it.
func withAdapterFeatures(t *testing.T, catalog *skill.Catalog, endpoint *models.ExternalConversationEndpoint, features ...capability.ConversationAdapterFeature) *skill.Catalog {
	t.Helper()
	bound, err := catalog.ResolveConversationAdapterBinding(t.Context(), skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID)
	if err != nil {
		t.Fatal(err)
	}
	definition := *bound.Definition
	definition.ConversationAdapters = map[string]skill.ConversationAdapter{}
	for id, adapter := range bound.Definition.ConversationAdapters {
		definition.ConversationAdapters[id] = adapter
	}
	adapter := definition.ConversationAdapters[endpoint.Adapter.AdapterID]
	adapter.Features = append(append([]capability.ConversationAdapterFeature(nil), adapter.Features...), features...)
	sort.Slice(adapter.Features, func(i, j int) bool { return adapter.Features[i] < adapter.Features[j] })
	definition.ConversationAdapters[endpoint.Adapter.AdapterID] = adapter
	next := skill.NewCatalog()
	if err := next.Register(t.Context(), &definition); err != nil {
		t.Fatal(err)
	}
	if err := next.Bind(t.Context(), bound.Binding); err != nil {
		t.Fatal(err)
	}
	return next
}

func TestExternalDeliveryNeverSendsAVisualAsAFile(t *testing.T) {
	store, _, endpoint := endpointFixture(t, t.Context(), "slack")
	content := artifactBytes{}
	visual := saveVisual(t, store, content, endpoint.Scope, endpoint.Owner)
	file := saveFile(t, store, content, endpoint.Scope, endpoint.Owner)

	files, visuals, err := externalDeliveryAttachments(t.Context(), store, content, endpoint.Scope, &models.ChannelMessage{References: []models.ConversationReference{visual, file, visual}})
	if err != nil || visuals != 1 || len(files) != 1 || files[0].ID != "report-1:1" || string(files[0].Data) != "a,b\n1,2\n" {
		t.Fatalf("files = %#v, visuals = %d, %v", files, visuals, err)
	}
	files, visuals, err = externalDeliveryAttachments(t.Context(), store, content, endpoint.Scope, &models.ChannelMessage{References: []models.ConversationReference{visual}})
	if err != nil || visuals != 1 || len(files) != 0 {
		t.Fatalf("visual-only files = %#v, visuals = %d, %v", files, visuals, err)
	}
}

// recordingHost delivers every request and keeps it.
type recordingHost struct{ requests []DeliveryHostRequest }

func (h *recordingHost) LookupExternalConversationDelivery(context.Context, DeliveryHostRequest) (*DeliveryAcknowledgement, error) {
	return &DeliveryAcknowledgement{Status: AcknowledgementUnknown}, nil
}

func (h *recordingHost) DeliverExternalConversation(_ context.Context, request DeliveryHostRequest) (*DeliveryHostResult, error) {
	h.requests = append(h.requests, request)
	return &DeliveryHostResult{Outcome: DeliveryOutcomeDelivered, ProviderMessageID: "sent-1"}, nil
}

func TestExternalDeliveryLeavesTheVisualOutOfTheChannel(t *testing.T) {
	for _, test := range []struct {
		name     string
		features []capability.ConversationAdapterFeature
		withFile bool
	}{
		{name: "attachments", features: []capability.ConversationAdapterFeature{capability.ConversationFeatureAttachments}},
		{name: "with a file", features: []capability.ConversationAdapterFeature{capability.ConversationFeatureAttachments}, withFile: true},
		// A declared visuals feature has no delivery yet; it never turns
		// the surface into a file.
		{name: "visuals declared", features: []capability.ConversationAdapterFeature{capability.ConversationFeatureAttachments, capability.ConversationFeatureVisuals}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, catalog, endpoint := endpointFixture(t, t.Context(), "slack")
			catalog = withAdapterFeatures(t, catalog, endpoint, test.features...)
			content := artifactBytes{}
			references := []models.ConversationReference{saveVisual(t, store, content, endpoint.Scope, endpoint.Owner)}
			if test.withFile {
				references = append(references, saveFile(t, store, content, endpoint.Scope, endpoint.Owner))
			}
			conversations := runtimeconversation.NewService(store)
			conversation, _, err := conversations.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "visuals", IdempotencyKey: "visuals"})
			if err != nil {
				t.Fatal(err)
			}
			answer, err := conversations.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
				Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: endpoint.Owner.ID},
				Intent: models.MessageIntentAnswer, Content: "Revenue grew 12% this quarter.",
				Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, References: references, IdempotencyKey: "answer",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewTransportService(store, catalog).Enqueue(t.Context(), EnqueueExternalConversationDeliveryRequest{
				Scope: endpoint.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
				ConversationID: conversation.ID, ChannelMessageID: answer.Message.ID, ExternalThreadID: "171.1",
			}); err != nil {
				t.Fatal(err)
			}
			host := &recordingHost{}
			now := time.Now().UTC().Add(time.Second)
			worker, err := NewDeliveryWorker(store, catalog, host, DeliveryWorkerConfig{WorkerID: "delivery", AttachmentContent: content}, models.WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			delivered, err := worker.ProcessOne(t.Context(), endpoint.Scope)
			if err != nil || delivered == nil || delivered.Status != models.ExternalConversationDeliveryDelivered || len(host.requests) != 1 {
				t.Fatalf("delivery = %#v, %v", delivered, err)
			}
			sent := host.requests[0]
			if sent.Message.Content != "Revenue grew 12% this quarter." {
				t.Fatalf("text = %q", sent.Message.Content)
			}
			if test.withFile {
				if len(sent.Attachments) != 1 || sent.Attachments[0].ID != "report-1:1" || sent.Attachments[0].MediaType != "text/csv" {
					t.Fatalf("attachments = %#v", sent.Attachments)
				}
			} else if len(sent.Attachments) != 0 {
				t.Fatalf("the visual was sent as a file: %#v", sent.Attachments)
			}
			if delivered.Summary != visualsLeftOutSummary {
				t.Fatalf("summary = %q", delivered.Summary)
			}
		})
	}
}

// newAnswerStreamFixtureWithFiles is newAnswerStreamFixture for an adapter
// that declares attachments and reads stored artifact bytes.
func newAnswerStreamFixtureWithFiles(t *testing.T, content artifactBytes, operations ...capability.ConversationDeliveryOperation) *answerStreamFixture {
	t.Helper()
	f := newAnswerStreamFixture(t, operations...)
	f.catalog = withAdapterFeatures(t, f.catalog, f.endpoint, capability.ConversationFeatureAttachments)
	var err error
	if f.replies, err = NewReplyWorker(f.store, f.catalog, f.clockOption()); err != nil {
		t.Fatal(err)
	}
	if f.delivery, err = NewDeliveryWorker(f.store, f.catalog, f.host, DeliveryWorkerConfig{WorkerID: "delivery", AttachmentContent: content}, f.clockOption()); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExternalAnswerStreamFinishesInPlaceWhenOnlyAVisualIsLeftOut(t *testing.T) {
	content := artifactBytes{}
	f := newAnswerStreamFixtureWithFiles(t, content, capability.ConversationDeliveryMessageDelete, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate)
	f.files = []models.ConversationReference{saveVisual(t, f.store, content, f.endpoint.Scope, f.endpoint.Owner)}
	f.preview(streamedOpening, false)
	f.stream()
	f.deliverAll()
	f.finish(models.AgentRunStatusCompleted, streamedOpening+" timed out.")
	f.reply()
	f.deliverAll()
	if got := f.operations(); len(got) != 2 || got[1] != capability.ConversationDeliveryMessageUpdate {
		t.Fatalf("operations = %v", got)
	}
	if len(f.host.messages) != 1 || f.host.messages["chat-1"] != streamedOpening+" timed out." || len(f.host.requests[1].Attachments) != 0 {
		t.Fatalf("chat = %#v", f.host.messages)
	}
}

func TestExternalAnswerStreamRepostsAnAnswerWithAFile(t *testing.T) {
	content := artifactBytes{}
	f := newAnswerStreamFixtureWithFiles(t, content, capability.ConversationDeliveryMessageDelete, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate)
	f.files = []models.ConversationReference{
		saveVisual(t, f.store, content, f.endpoint.Scope, f.endpoint.Owner),
		saveFile(t, f.store, content, f.endpoint.Scope, f.endpoint.Owner),
	}
	f.preview(streamedOpening, false)
	f.stream()
	f.deliverAll()
	f.finish(models.AgentRunStatusCompleted, streamedOpening+" timed out.")
	f.reply()
	f.deliverAll()
	f.reply()
	f.deliverAll()
	got := f.operations()
	if len(got) != 3 || got[1] != capability.ConversationDeliveryMessageDelete || got[2] != capability.ConversationDeliveryMessageSend {
		t.Fatalf("operations = %v", got)
	}
	if attachments := f.host.requests[2].Attachments; len(attachments) != 1 || attachments[0].ID != "report-1:1" {
		t.Fatalf("attachments = %#v", attachments)
	}
}
