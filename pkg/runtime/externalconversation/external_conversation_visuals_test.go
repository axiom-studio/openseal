package externalconversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
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

// visualSurface is a stored visual whose quiz carries an answer key.
const visualSurface = `{"schema":"` + presentation.Schema + `","title":"Quarter review","state":"ready","components":[` +
	`{"id":"intro","type":"text","text":"Revenue grew."},` +
	`{"id":"check","type":"quiz","summary":"Check","quiz":{"question":"Which region grew?","options":[{"id":"e","label":"East"},{"id":"w","label":"West"}],"answerKey":{"correct":["e"],"explanation":"East grew 12%."}}}]}`

func saveVisual(t *testing.T, store models.ArtifactStore, content artifactBytes, scope models.Scope, owner models.ObjectiveOwner) models.ConversationReference {
	return saveArtifact(t, store, content, scope, owner, "visual-1", presentation.ArtifactType, presentation.MediaType, []byte(visualSurface))
}

func saveFile(t *testing.T, store models.ArtifactStore, content artifactBytes, scope models.Scope, owner models.ObjectiveOwner) models.ConversationReference {
	return saveArtifact(t, store, content, scope, owner, "report-1", "file", "text/csv", []byte("a,b\n1,2\n"))
}

// withAdapterFeatures re-registers the endpoint's adapter with features
// added and returns the catalog that resolves it.
func withAdapterFeatures(t *testing.T, catalog *skill.Catalog, endpoint *models.ExternalConversationEndpoint, features ...capability.ConversationAdapterFeature) *skill.Catalog {
	t.Helper()
	return withAdapterChange(t, catalog, endpoint, func(adapter *skill.ConversationAdapter) {
		adapter.Features = append(append([]capability.ConversationAdapterFeature(nil), adapter.Features...), features...)
		sort.Slice(adapter.Features, func(i, j int) bool { return adapter.Features[i] < adapter.Features[j] })
	})
}

// withAdapterChange re-registers the endpoint's adapter as change leaves it
// and returns the catalog that resolves it.
func withAdapterChange(t *testing.T, catalog *skill.Catalog, endpoint *models.ExternalConversationEndpoint, change func(*skill.ConversationAdapter)) *skill.Catalog {
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
	change(&adapter)
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

	files, visuals, err := externalDeliveryFiles(t.Context(), store, endpoint.Scope, &models.ChannelMessage{References: []models.ConversationReference{visual, file, visual}})
	if err != nil || len(visuals) != 1 || visuals[0].ID != "visual-1" || len(files) != 1 {
		t.Fatalf("files = %#v, visuals = %#v, %v", files, visuals, err)
	}
	attachments, err := externalDeliveryAttachments(t.Context(), content, endpoint.Scope, files)
	if err != nil || len(attachments) != 1 || attachments[0].ID != "report-1:1" || string(attachments[0].Data) != "a,b\n1,2\n" {
		t.Fatalf("attachments = %#v, %v", attachments, err)
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

// deliverAnswer posts an answer carrying references, delivers it once to
// an adapter with features and returns the delivery and the host request.
func deliverAnswer(t *testing.T, features []capability.ConversationAdapterFeature, refs func(*testing.T, models.ArtifactStore, artifactBytes, *models.ExternalConversationEndpoint) []models.ConversationReference) (*models.ExternalConversationDelivery, DeliveryHostRequest) {
	t.Helper()
	store, catalog, endpoint := endpointFixture(t, t.Context(), "slack")
	catalog = withAdapterFeatures(t, catalog, endpoint, features...)
	content := artifactBytes{}
	conversations := runtimeconversation.NewService(store)
	conversation, _, err := conversations.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "visuals", IdempotencyKey: "visuals"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := conversations.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: endpoint.Owner.ID},
		// The placeholder marks where the visual stands in the chat; a
		// channel gets the text without it.
		Intent: models.MessageIntentAnswer, Content: "Revenue grew 12% this quarter.\n\n::visual{#revenue}",
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, References: refs(t, store, content, endpoint), IdempotencyKey: "answer",
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
	if host.requests[0].Message.Content != "Revenue grew 12% this quarter." {
		t.Fatalf("text = %q", host.requests[0].Message.Content)
	}
	return delivered, host.requests[0]
}

func visualAndMaybeFile(withFile bool) func(*testing.T, models.ArtifactStore, artifactBytes, *models.ExternalConversationEndpoint) []models.ConversationReference {
	return func(t *testing.T, store models.ArtifactStore, content artifactBytes, endpoint *models.ExternalConversationEndpoint) []models.ConversationReference {
		refs := []models.ConversationReference{saveVisual(t, store, content, endpoint.Scope, endpoint.Owner)}
		if withFile {
			refs = append(refs, saveFile(t, store, content, endpoint.Scope, endpoint.Owner))
		}
		return refs
	}
}

func TestExternalDeliveryLeavesTheVisualOutOfTheChannel(t *testing.T) {
	for _, withFile := range []bool{false, true} {
		delivered, sent := deliverAnswer(t, []capability.ConversationAdapterFeature{capability.ConversationFeatureAttachments}, visualAndMaybeFile(withFile))
		if withFile {
			if len(sent.Attachments) != 1 || sent.Attachments[0].ID != "report-1:1" || sent.Attachments[0].MediaType != "text/csv" {
				t.Fatalf("attachments = %#v", sent.Attachments)
			}
		} else if len(sent.Attachments) != 0 {
			t.Fatalf("the visual was sent as a file: %#v", sent.Attachments)
		}
		if len(sent.Visuals) != 0 || delivered.Summary != visualsLeftOutSummary {
			t.Fatalf("visuals = %#v, summary = %q", sent.Visuals, delivered.Summary)
		}
	}
}

func TestExternalDeliveryShowsTheVisualOnAVisualsChannel(t *testing.T) {
	for _, test := range []struct {
		name     string
		features []capability.ConversationAdapterFeature
		withFile bool
	}{
		{name: "visuals only", features: []capability.ConversationAdapterFeature{capability.ConversationFeatureVisuals}, withFile: true},
		{name: "with a file", features: []capability.ConversationAdapterFeature{capability.ConversationFeatureAttachments, capability.ConversationFeatureVisuals}, withFile: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			delivered, sent := deliverAnswer(t, test.features, visualAndMaybeFile(test.withFile))
			if delivered.Summary != "" || len(sent.Visuals) != 1 {
				t.Fatalf("visuals = %#v, summary = %q", sent.Visuals, delivered.Summary)
			}
			visual := sent.Visuals[0]
			if visual.ArtifactID != "visual-1" || visual.Version != 1 || visual.Surface.Title != "Quarter review" || len(visual.Surface.Components) != 2 {
				t.Fatalf("visual = %#v", visual)
			}
			if presentation.HasAnswerKeys(visual.Surface) {
				t.Fatal("the quiz answer key reached the channel")
			}
			if visual.FallbackText != presentation.FallbackText(visual.Surface) || !strings.Contains(visual.FallbackText, "- East") || strings.Contains(visual.FallbackText, "12%") {
				t.Fatalf("fallback = %q", visual.FallbackText)
			}
			encoded, err := json.Marshal(sent)
			if err != nil || !bytes.Contains(encoded, []byte(`"visuals":[{"artifactId":"visual-1","version":1,"surface":{"schema":`)) || bytes.Contains(encoded, []byte("answerKey")) {
				t.Fatalf("wire = %s, %v", encoded, err)
			}
			// Files stay files; only an adapter with attachments receives them.
			wantFiles := 0
			if containsConversationFeature(test.features, capability.ConversationFeatureAttachments) {
				wantFiles = 1
			}
			if len(sent.Attachments) != wantFiles {
				t.Fatalf("attachments = %#v", sent.Attachments)
			}
		})
	}
}

func TestExternalDeliveryLeavesOutAVisualWithCredentialLikeFields(t *testing.T) {
	surface := `{"schema":"` + presentation.Schema + `","title":"Keys","state":"ready","components":[{"id":"t","type":"table","summary":"Keys",` +
		`"table":{"columns":[{"key":"api_key","label":"Key","kind":"text"}],"rows":[{"api_key":"masked"}]}}]}`
	delivered, sent := deliverAnswer(t, []capability.ConversationAdapterFeature{capability.ConversationFeatureVisuals},
		func(t *testing.T, store models.ArtifactStore, content artifactBytes, endpoint *models.ExternalConversationEndpoint) []models.ConversationReference {
			return []models.ConversationReference{saveArtifact(t, store, content, endpoint.Scope, endpoint.Owner, "visual-keys", presentation.ArtifactType, presentation.MediaType, []byte(surface))}
		})
	if len(sent.Visuals) != 0 || delivered.Summary != visualsUnsafeSummary {
		t.Fatalf("visuals = %#v, summary = %q", sent.Visuals, delivered.Summary)
	}
}

// newAnswerStreamFixtureWithFiles is newAnswerStreamFixture for an adapter
// that declares attachments and reads stored artifact bytes.
func newAnswerStreamFixtureWithFiles(t *testing.T, content artifactBytes, operations ...capability.ConversationDeliveryOperation) *answerStreamFixture {
	return newAnswerStreamFixtureWithFeatures(t, content, []capability.ConversationAdapterFeature{capability.ConversationFeatureAttachments}, operations...)
}

func newAnswerStreamFixtureWithFeatures(t *testing.T, content artifactBytes, features []capability.ConversationAdapterFeature, operations ...capability.ConversationDeliveryOperation) *answerStreamFixture {
	t.Helper()
	f := newAnswerStreamFixture(t, operations...)
	f.catalog = withAdapterFeatures(t, f.catalog, f.endpoint, features...)
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
	if len(f.host.messages) != 1 || f.host.messages["chat-1"] != streamedOpening+" timed out." || len(f.host.requests[1].Attachments) != 0 || len(f.host.requests[1].Visuals) != 0 {
		t.Fatalf("chat = %#v", f.host.messages)
	}
}

func TestExternalAnswerStreamFinishesInPlaceWithTheVisual(t *testing.T) {
	content := artifactBytes{}
	f := newAnswerStreamFixtureWithFeatures(t, content, []capability.ConversationAdapterFeature{capability.ConversationFeatureAttachments, capability.ConversationFeatureVisuals},
		capability.ConversationDeliveryMessageDelete, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate)
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
	// The preview carries no visual; the finishing edit adds it in place.
	if len(f.host.requests[0].Visuals) != 0 || len(f.host.requests[1].Visuals) != 1 || f.host.requests[1].Visuals[0].ArtifactID != "visual-1" {
		t.Fatalf("visuals = %#v / %#v", f.host.requests[0].Visuals, f.host.requests[1].Visuals)
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
