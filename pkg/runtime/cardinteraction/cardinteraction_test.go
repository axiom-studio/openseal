package cardinteraction_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/cardinteraction"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

const cardSurface = `{"schema":"` + presentation.Schema + `","title":"Dinner","state":"ready","components":[` +
	`{"id":"pick","type":"choices","summary":"Where to eat","choices":{"chips":[{"id":"thai","label":"Thai"},{"id":"pizza","label":"Pizza"}]}},` +
	`{"id":"check","type":"quiz","summary":"Check","quiz":{"question":"Which is spicy?","options":[{"id":"t","label":"Thai"},{"id":"p","label":"Pizza"}],"answerKey":{"correct":["t"]}}}]}`

type surfaceBytes map[string][]byte

func (b surfaceBytes) Put(context.Context, models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	return models.ArtifactStoredContent{}, errors.New("not used")
}

func (b surfaceBytes) Open(_ context.Context, _ models.Scope, ref string) (io.ReadCloser, error) {
	data, ok := b[ref]
	if !ok {
		return nil, errors.New("no such content")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type cardFixture struct {
	store        *sqlite.Store
	service      *cardinteraction.Service
	conversation *models.Conversation
	card         *models.ChannelMessage
	person       models.ConversationParticipant
}

func newCardFixture(t *testing.T) *cardFixture {
	t.Helper()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "42"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	space := conversationtest.SeedSpace(t, store, scope)
	content := surfaceBytes{"visual-1": []byte(cardSurface)}
	if _, err := artifactcatalog.NewCatalog(store).Register(t.Context(), artifactcatalog.RegisterArtifactRequest{Artifact: &models.Artifact{
		ID: "visual-1", Version: 1, Scope: scope, Name: "Dinner", Type: presentation.ArtifactType, MediaType: presentation.MediaType,
		ContentRef: "visual-1", Digest: models.SHA256Digest([]byte(cardSurface)), SizeBytes: int64(len(cardSurface)),
		Classification: models.ArtifactClassificationInternal, CreatedAt: time.Now().UTC(),
		Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: owner.ID}, Owner: &owner},
	}}); err != nil {
		t.Fatal(err)
	}
	conversations := runtimeconversation.NewService(store)
	conversation, _, err := conversations.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Dinner", SpaceID: space.ID, IdempotencyKey: "conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: owner.ID},
		Intent: models.MessageIntentAnswer, Content: "Where shall we eat?",
		Audience:       models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		References:     []models.ConversationReference{{Kind: models.ConversationReferenceArtifact, ID: "visual-1", Version: 1}},
		IdempotencyKey: "card",
	})
	if err != nil {
		t.Fatal(err)
	}
	person := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "7"}
	people := func(context.Context, *models.Conversation) ([]models.ConversationParticipant, error) {
		return []models.ConversationParticipant{person}, nil
	}
	return &cardFixture{
		store: store, service: cardinteraction.NewService(store, store, content, people),
		conversation: posted.Conversation, card: posted.Message, person: person,
	}
}

func (f *cardFixture) request(componentID, key string, values map[string]interface{}) cardinteraction.Request {
	return cardinteraction.Request{
		Conversation: f.conversation, Card: f.card, SurfaceArtifactID: "visual-1", SurfaceVersion: 1,
		ComponentID: componentID, Values: values, Actor: f.person, IdempotencyKey: key,
	}
}

func (f *cardFixture) answer(t *testing.T, req cardinteraction.Request) (*cardinteraction.Recorded, error) {
	t.Helper()
	prepared, err := f.service.Prepare(t.Context(), req)
	if err != nil {
		return nil, err
	}
	return f.service.Record(t.Context(), prepared)
}

func TestChoiceIsRecordedAndPostedAsThePersonsAnswer(t *testing.T) {
	f := newCardFixture(t)

	recorded, err := f.answer(t, f.request("pick", "click-1", map[string]interface{}{"chipId": "thai"}))
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Replayed || recorded.Interaction.ActorID != "7" || !strings.Contains(recorded.Answer, "Thai") {
		t.Fatalf("recorded = %#v", recorded)
	}
	request := f.service.AnswerRequest(t.Context(), f.conversation, f.card, recorded.Answer, recorded.Interaction)
	commit, err := f.service.PostAnswer(t.Context(), runtimeconversation.NewService(f.store), f.conversation, request)
	if err != nil {
		t.Fatal(err)
	}
	if commit.Message.Sender != f.person || commit.Message.Content != recorded.Answer || !commit.Message.RequiresResponse {
		t.Fatalf("answer = %#v", commit.Message)
	}
}

func TestSingleUseAnswerConflictsOnTheSecondClick(t *testing.T) {
	f := newCardFixture(t)
	if _, err := f.answer(t, f.request("check", "answer-1", map[string]interface{}{"selected": []interface{}{"t"}})); err != nil {
		t.Fatal(err)
	}

	_, err := f.answer(t, f.request("check", "answer-2", map[string]interface{}{"selected": []interface{}{"p"}}))

	var conflict *models.SurfaceInteractionConflictError
	if !errors.As(err, &conflict) || conflict.Existing == nil {
		t.Fatalf("second answer err = %v", err)
	}
}

func TestPrepareRejectsUnknownComponentsAndBadValues(t *testing.T) {
	f := newCardFixture(t)

	_, err := f.service.Prepare(t.Context(), f.request("missing", "k", nil))
	var notFound *cardinteraction.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("missing component err = %v", err)
	}
	_, err = f.service.Prepare(t.Context(), f.request("pick", "k", map[string]interface{}{"chipId": "sushi"}))
	var rejected *cardinteraction.RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("unknown chip err = %v", err)
	}
	stale := f.request("pick", "k", map[string]interface{}{"chipId": "thai"})
	stale.SurfaceVersion = 2
	if _, err := f.service.Prepare(t.Context(), stale); !errors.Is(err, artifactcatalog.ErrArtifactNotFound) {
		t.Fatalf("missing version err = %v", err)
	}
}
