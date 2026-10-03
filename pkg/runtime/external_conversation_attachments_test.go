package runtime

import (
	"bytes"
	"context"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"io"
	"sort"
	"testing"
	"time"
)

type externalFileContentStore struct {
	data  []byte
	scope Scope
	opens int
}

func (s *externalFileContentStore) Put(_ context.Context, req ArtifactContentWrite) (ArtifactStoredContent, error) {
	s.data, _ = io.ReadAll(req.Reader)
	s.scope = req.Scope
	return ArtifactStoredContent{ContentRef: "opaque-file", SizeBytes: int64(len(s.data)), Digest: conversationTestDigest(string(s.data))}, nil
}
func (s *externalFileContentStore) Open(_ context.Context, scope Scope, _ string) (io.ReadCloser, error) {
	s.scope = scope
	s.opens++
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

type externalFileHost struct {
	calls  int
	origin string
}

func (h *externalFileHost) ReadExternalConversationAttachment(_ context.Context, req ExternalConversationAttachmentHostRequest) (*ExternalConversationAttachmentContent, error) {
	h.calls++
	h.origin = req.Endpoint.Address
	return &ExternalConversationAttachmentContent{ID: req.Attachment.ID, Name: req.Attachment.Name, MediaType: "text/plain", Data: []byte("real contents"), Status: "supplied"}, nil
}
func fileConversationFixture(t *testing.T) (*MemoryStore, *skill.Catalog, *ExternalConversationEndpoint) {
	t.Helper()
	store, original, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
	bound, err := original.ResolveConversationAdapterBinding(t.Context(), skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID)
	if err != nil {
		t.Fatal(err)
	}
	definition := slackConversationSkillDefinition()
	definition.ID = "slack"
	definition.Name = "slack"
	adapter := definition.ConversationAdapters["conversations"]
	adapter.Features = append(adapter.Features, capability.ConversationFeatureAttachments)
	sort.Slice(adapter.Features, func(i, j int) bool { return adapter.Features[i] < adapter.Features[j] })
	definition.ConversationAdapters["conversations"] = adapter
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), bound.Binding); err != nil {
		t.Fatal(err)
	}
	return store, catalog, endpoint
}
func TestExternalConversationImportsFileOnceAndScopesArtifact(t *testing.T) {
	store, catalog, endpoint := fileConversationFixture(t)
	content := &externalFileContentStore{}
	host := &externalFileHost{}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "files", IdempotencyKey: "files"})
	if err != nil {
		t.Fatal(err)
	}
	worker := &ExternalConversationInboxWorker{store: store, config: ExternalConversationInboxWorkerConfig{ContextCatalog: catalog, AttachmentHost: host, AttachmentContent: content}}
	event := NormalizedExternalConversationEvent{ID: "event1", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "C-origin", ExternalThreadID: "171.1", ExternalMessageID: "171.2", ExternalParticipantID: "U1", OccurredAt: time.Now().UTC(), OrderingKey: "171.2", Attachments: []ExternalConversationAttachment{{ID: "F1", Name: "note.txt", MediaType: "text/plain", SizeBytes: 13}}}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	refs, notices, err := worker.importAttachments(t.Context(), endpoint, conversation, event)
	if err != nil || len(refs) != 1 || len(notices) != 0 || host.origin != "C-origin" {
		t.Fatalf("import=%#v %#v %v", refs, notices, err)
	}
	artifact, err := store.GetArtifact(t.Context(), endpoint.Scope, refs[0].ID, 1)
	if err != nil || artifact == nil || artifact.Provenance.Owner == nil || *artifact.Provenance.Owner != conversation.Owner || content.scope != endpoint.Scope {
		t.Fatal("artifact authority lost")
	}
	if _, _, err := worker.importAttachments(t.Context(), endpoint, conversation, event); err != nil || host.calls != 1 {
		t.Fatal("replay downloaded duplicate file")
	}
	reply := &ChannelMessage{References: refs}
	files, err := externalDeliveryAttachments(t.Context(), store, content, endpoint.Scope, reply)
	if err != nil || len(files) != 1 || string(files[0].Data) != "real contents" {
		t.Fatalf("files=%#v %v", files, err)
	}
	before := content.opens
	if _, err := externalDeliveryAttachments(t.Context(), store, content, Scope{Kind: "tenant", ID: "other"}, reply); err == nil || content.opens != before {
		t.Fatal("cross tenant file was opened")
	}
	content.data = []byte("tampered")
	if _, err := externalDeliveryAttachments(t.Context(), store, content, endpoint.Scope, reply); err == nil {
		t.Fatal("tampered artifact sent")
	}
}
func TestExternalConversationFileDescriptorsRejectDuplicatesAndInvalidMetadata(t *testing.T) {
	for _, files := range [][]ExternalConversationAttachment{{{ID: "F1", Name: "ok"}, {ID: "F1", Name: "duplicate"}}, {{ID: "F1", Name: "bad\nname"}}, {{ID: "F1", Name: "ok", SizeBytes: -1}}, {{ID: "F1", Name: "ok", MediaType: "bad mime"}}} {
		if validateExternalAttachments(files) == nil {
			t.Fatalf("accepted invalid descriptors %#v", files)
		}
	}
}

type checkpointConversationHost struct{ calls int }

func (h *checkpointConversationHost) LookupExternalConversationDelivery(context.Context, ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryAcknowledgement, error) {
	return &ExternalConversationDeliveryAcknowledgement{Status: ExternalConversationAcknowledgementNotFound}, nil
}
func (h *checkpointConversationHost) DeliverExternalConversation(_ context.Context, req ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryHostResult, error) {
	h.calls++
	if h.calls == 1 {
		return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeRetry, ErrorCode: "file_upload_ready", Progress: map[string]interface{}{"providerFileId": "F-uploaded"}}, nil
	}
	if req.Delivery.Progress["providerFileId"] != "F-uploaded" {
		return nil, ErrExternalConversationConflict
	}
	return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: "file:F-uploaded"}, nil
}
func TestExternalConversationDeliveryPersistsCheckpointAcrossWorkerRestart(t *testing.T) {
	store, catalog, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "files", IdempotencyKey: "checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := conversations.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.Owner.ID}, Intent: MessageIntentAnswer, Content: "Here is your file.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "reply"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewExternalConversationTransportService(store, catalog).Enqueue(t.Context(), EnqueueExternalConversationDeliveryRequest{Scope: endpoint.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend, ConversationID: conversation.ID, ChannelMessageID: reply.Message.ID, ExternalThreadID: "171.1", MaximumAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	host := &checkpointConversationHost{}
	config := ExternalConversationDeliveryWorkerConfig{WorkerID: "worker-one", BaseRetry: time.Nanosecond, MaximumRetry: time.Second}
	worker, err := NewExternalConversationDeliveryWorker(store, catalog, host, config)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	worker.now = func() time.Time { return now }
	result, err := worker.ProcessOne(t.Context(), endpoint.Scope)
	if err != nil || result == nil || result.Status != ExternalConversationDeliveryRetry || result.Progress["providerFileId"] != "F-uploaded" {
		t.Fatalf("checkpoint=%#v %v", result, err)
	}
	config.WorkerID = "worker-restarted"
	worker, err = NewExternalConversationDeliveryWorker(store, catalog, host, config)
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now.Add(time.Second) }
	result, err = worker.ProcessOne(t.Context(), endpoint.Scope)
	if err != nil || result == nil || result.Status != ExternalConversationDeliveryDelivered || host.calls != 2 {
		t.Fatalf("restart=%#v %v", result, err)
	}
}
