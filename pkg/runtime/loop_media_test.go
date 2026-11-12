package runtime

// Run-loop media scenarios (run-loop section 12 §9, tests-T16), replacing the
// hosted runner's media provenance test. Session entries store image
// references only: an attachment names its exact artifact version and digest,
// a governed screenshot names its action call. Each step renders the bytes
// from that source for a vision model, or a text placeholder for a text-only
// model, and nothing in the session ever holds the bytes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

// mediaContent serves artifact bytes by content ref; set swaps them.
type mediaContent struct {
	mu    sync.Mutex
	byRef map[string][]byte
}

func (c *mediaContent) Put(context.Context, models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	return models.ArtifactStoredContent{}, fmt.Errorf("media content is read-only")
}

func (c *mediaContent) Open(_ context.Context, _ models.Scope, ref string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.byRef[ref]
	if !ok {
		return nil, artifactcatalog.ErrArtifactNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (c *mediaContent) set(ref string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byRef[ref] = data
}

// mediaPNG is a valid w×h PNG.
func mediaPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mediaDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// mediaFixture is a loop fixture whose chats read attachments from content.
type mediaFixture struct {
	*looptest.Fixture
	content *mediaContent
	vision  bool
}

func newMediaFixture(t *testing.T, store models.KernelStore, vision bool, rules ...*looptest.ModelRule) *mediaFixture {
	t.Helper()
	f := &mediaFixture{Fixture: looptest.NewFixture(t, store, looptest.WithRules(rules...)), content: &mediaContent{byRef: map[string][]byte{}}, vision: vision}
	// The kit's conversation turn runner, reading attachments from content.
	runner, err := conversationrun.NewTurnRunner(f.Chats, f.Coordinator(), conversationrun.TurnRunnerConfig{
		AttachmentContent: f.content, AgentTurns: runtimeconversation.TurnRunnerResolverFunc(f.AgentBinding),
	}, models.WithClock(f.Clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	f.Runner = runner
	return f
}

// putArtifact stores version 1 of an artifact whose record says mediaType and
// digest, served with data.
func (f *mediaFixture) putArtifact(id, mediaType string, data []byte, recordDigest string) models.ConversationReference {
	f.T.Helper()
	artifacts, ok := f.Raw.(models.ArtifactStore)
	if !ok {
		f.T.Fatalf("%T is not an artifact store", f.Raw)
	}
	ref := "object-store:" + id
	f.content.set(ref, data)
	artifact := &models.Artifact{
		ID: id, Version: 1, Scope: f.Scope, Name: id + ".png", MediaType: mediaType, ContentRef: ref,
		SizeBytes: int64(len(data)), Digest: recordDigest, Classification: models.ArtifactClassificationInternal,
		Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "user", ID: looptest.FixtureUser}}, CreatedAt: f.Clock.Now(),
	}
	if err := artifacts.CreateArtifactVersion(f.Ctx, artifact, 0); err != nil {
		f.T.Fatalf("store artifact %s: %v", id, err)
	}
	return models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: id, Version: 1}
}

// startChatWithAttachments posts text with references as ana and schedules it.
func (f *mediaFixture) startChatWithAttachments(text string, refs ...models.ConversationReference) (*models.Conversation, *models.AgentRun) {
	f.T.Helper()
	conversation, _, err := f.Convs.CreateConversation(f.Ctx, runtimeconversation.CreateConversationRequest{
		Scope: f.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: looptest.FixtureAgent}, Title: "Media chat",
		IdempotencyKey: "media-chat-" + fmt.Sprint(f.Clock.Now().UnixNano()),
	})
	if err != nil {
		f.T.Fatal(err)
	}
	posted, err := f.Convs.PostChannelMessage(f.Ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: f.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: looptest.FixtureUser}, Intent: models.MessageIntentQuestion,
		Content: text, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, References: refs,
		IdempotencyKey: "media-post-" + conversation.ID,
	})
	if err != nil {
		f.T.Fatalf("post message: %v", err)
	}
	return conversation, f.ChatRun(conversation, posted.Message)
}

// bind gives the claim's model producer the artifact sources and the
// fixture's image modality, as the catalog resolver does from the catalog.
func (f *mediaFixture) bind(binding *agentrun.TurnRunnerBinding) {
	producer, ok := binding.Producer.(*modelstep.Producer)
	if !ok {
		f.T.Fatalf("producer is %T", binding.Producer)
	}
	vision := f.vision
	producer.Media = modelstep.Media{
		Artifacts: f.Raw.(models.ArtifactStore), Content: f.content,
		ImageInput: func(context.Context, agentrun.StepInput, models.ModelStepRequest) bool { return vision },
	}
}

// drive claims until the Run is terminal or waiting.
func (f *mediaFixture) drive(runID string, maxClaims int) *models.AgentRun {
	f.T.Helper()
	for i := 0; i < maxClaims; i++ {
		exit, run := f.RunClaimWith(f.Ctx, runID, f.bind)
		if exit.Err != nil && exit.Reason != runloop.ExitRequeued {
			f.T.Fatalf("claim %d exited %s: %v", i+1, exit.Reason, exit.Err)
		}
		if run.Status != models.AgentRunStatusQueued {
			return run
		}
		f.Clock.Advance(time.Second)
	}
	f.T.Fatalf("run %s not at rest after %d claims", runID, maxClaims)
	return nil
}

// imageParts is every image part of the view's user and tool entries.
func viewImageParts(v looptest.StepView) (user, tool []models.ContentPart) {
	for _, e := range v.Request().Entries {
		switch {
		case e.Type == models.SessionEntryUser && e.User != nil:
			for _, p := range e.User.Content {
				if p.Type == models.ContentPartImage {
					user = append(user, p)
				}
			}
		case e.Type == models.SessionEntryTool && e.Tool != nil:
			for _, p := range e.Tool.Result.Content {
				if p.Type == models.ContentPartImage {
					tool = append(tool, p)
				}
			}
		}
	}
	return user, tool
}

// viewText is the text of every user and tool entry of the view.
func viewText(v looptest.StepView) string {
	var b strings.Builder
	for _, e := range v.Request().Entries {
		switch {
		case e.Type == models.SessionEntryUser && e.User != nil:
			b.WriteString(looptest.PartsText(e.User.Content) + "\n")
		case e.Type == models.SessionEntryTool && e.Tool != nil:
			b.WriteString(looptest.PartsText(e.Tool.Result.Content) + "\n")
		}
	}
	return b.String()
}

// storedImageParts is every image part of the Run's stored entries.
func storedImageParts(f *looptest.Fixture, runID string) (user, tool []models.ContentPart) {
	f.T.Helper()
	for _, entry := range f.Entries(runID) {
		switch {
		case entry.Type == models.SessionEntryUser && entry.User != nil:
			for _, p := range entry.User.Content {
				if p.Type == models.ContentPartImage {
					user = append(user, p)
				}
			}
		case entry.Type == models.SessionEntryTool && entry.Tool != nil && entry.Tool.Result != nil:
			for _, p := range entry.Tool.Result.Content {
				if p.Type == models.ContentPartImage {
					tool = append(tool, p)
				}
			}
		}
	}
	return user, tool
}

// requireNoStoredBytes fails when any stored entry of the Run holds encoded.
func requireNoStoredBytes(t *testing.T, f *looptest.Fixture, runID string, encoded ...string) {
	t.Helper()
	raw, err := json.Marshal(f.Entries(runID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "dataBase64") {
		t.Fatal("a stored entry carries dataBase64")
	}
	for _, e := range encoded {
		if strings.Contains(string(raw), e) {
			t.Fatal("image bytes are stored in the session")
		}
	}
}

// screenshotOutput is a lookup result with a screenshot as output.modelMedia.
func screenshotOutput(data []byte) func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
	return func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{
			"id": in.Arguments["id"], "status": "open",
			"modelMedia": map[string]interface{}{"mediaType": "image/png", "detail": "low", "contentBase64": base64.StdEncoding.EncodeToString(data)},
		}, nil
	}
}

// TestLoopMediaProvenanceOnEntries: an attached image is stored as a
// reference to its exact artifact version and digest, and a governed
// screenshot as a reference to its action call. A vision model's steps get
// the bytes of exactly those sources; a text-only model's steps get the
// "[image omitted]" placeholder. The session never stores the bytes.
func TestLoopMediaProvenanceOnEntries(t *testing.T) {
	t.Run("attachment", func(t *testing.T) {
		looptest.EachStore(t, "media_attachment", func(t *testing.T, store models.KernelStore) {
			chart := mediaPNG(t, 3, 2)
			encoded := base64.StdEncoding.EncodeToString(chart)
			f := newMediaFixture(t, store, true, &looptest.ModelRule{Name: "look", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
				user, _ := viewImageParts(v)
				if len(user) != 1 || user[0].DataBase64 != encoded || user[0].ArtifactID != "chart" || user[0].ArtifactVersion != "1" || user[0].SHA256 != mediaDigest(chart) {
					t.Errorf("step image parts = %#v", user)
				}
				return looptest.Say("A blank chart.")
			}})
			ref := f.putArtifact("chart", "image/png", chart, "sha256:"+mediaDigest(chart))
			conversation, run := f.startChatWithAttachments("What is in this chart?", ref)
			done := f.drive(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)

			user, _ := storedImageParts(f.Fixture, run.ID)
			if len(user) != 1 {
				t.Fatalf("stored image parts = %#v", user)
			}
			if got := user[0]; got.ArtifactID != "chart" || got.ArtifactVersion != "1" || got.ActionCallID != "" || got.SHA256 != mediaDigest(chart) || got.MediaType != "image/png" {
				t.Fatalf("stored image reference = %#v", got)
			}
			requireNoStoredBytes(t, f.Fixture, run.ID, encoded)
			if replies := f.AgentReplies(conversation.ID); !slices.Equal(replies, []string{"A blank chart."}) {
				t.Fatalf("replies = %q", replies)
			}
		})
	})

	t.Run("governed_screenshot", func(t *testing.T) {
		looptest.EachStore(t, "media_screenshot", func(t *testing.T, store models.KernelStore) {
			shot := mediaPNG(t, 4, 4)
			encoded := base64.StdEncoding.EncodeToString(shot)
			f := newMediaFixture(t, store, true,
				&looptest.ModelRule{Name: "answer", When: func(v looptest.StepView) bool {
					_, _, ok := v.ToolResult(looptest.GovernedTool(v, "lookup"))
					return ok
				}, Reply: func(v looptest.StepView) models.ModelStepResult {
					_, tool := viewImageParts(v)
					if len(tool) != 1 || tool[0].DataBase64 != encoded || tool[0].ActionCallID == "" || tool[0].ArtifactID != "" || tool[0].SHA256 != mediaDigest(shot) {
						t.Errorf("step tool image parts = %#v", tool)
					}
					if content, _, _ := v.ToolResult(looptest.GovernedTool(v, "lookup")); strings.Contains(content, "modelMedia") || strings.Contains(content, encoded) {
						t.Errorf("the screenshot is in the tool result text: %q", content)
					}
					return looptest.Say("The page shows issue 9.")
				}},
				&looptest.ModelRule{Name: "lookup", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Looking.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "9"}))
				}},
			)
			f.Dispatch.On("lookup", screenshotOutput(shot))
			conversation, run := f.startChatWithAttachments("Show me issue 9")
			done := f.drive(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)

			calls := f.Calls(run.ID)
			if len(calls) != 1 || calls[0].Status != models.ActionCallStatusSucceeded {
				t.Fatalf("calls = %#v", calls)
			}
			_, tool := storedImageParts(f.Fixture, run.ID)
			if len(tool) != 1 || tool[0].ActionCallID != calls[0].ID || tool[0].ArtifactID != "" || tool[0].SHA256 != mediaDigest(shot) || tool[0].Detail != "low" {
				t.Fatalf("stored tool image reference = %#v (call %s)", tool, calls[0].ID)
			}
			requireNoStoredBytes(t, f.Fixture, run.ID, encoded)
			if replies := f.AgentReplies(conversation.ID); !slices.Equal(replies, []string{"The page shows issue 9."}) {
				t.Fatalf("replies = %q", replies)
			}
		})
	})

	t.Run("text_only_model", func(t *testing.T) {
		looptest.EachStore(t, "media_text_only", func(t *testing.T, store models.KernelStore) {
			chart, shot := mediaPNG(t, 3, 2), mediaPNG(t, 4, 4)
			omitted := "[image omitted: image/png]"
			f := newMediaFixture(t, store, false,
				&looptest.ModelRule{Name: "answer", When: func(v looptest.StepView) bool {
					_, _, ok := v.ToolResult(looptest.GovernedTool(v, "lookup"))
					return ok
				}, Reply: func(v looptest.StepView) models.ModelStepResult {
					if user, tool := viewImageParts(v); len(user)+len(tool) != 0 {
						t.Errorf("a text-only step got image parts: %#v %#v", user, tool)
					}
					if got := strings.Count(viewText(v), omitted); got != 2 {
						t.Errorf("placeholders = %d, want 2: %s", got, viewText(v))
					}
					return looptest.Say("I can't see images here.")
				}},
				&looptest.ModelRule{Name: "lookup", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
					if user, _ := viewImageParts(v); len(user) != 0 || !strings.Contains(viewText(v), omitted) {
						t.Errorf("a text-only step saw the attachment: %#v / %s", user, viewText(v))
					}
					return looptest.Act("Looking.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "9"}))
				}},
			)
			f.Dispatch.On("lookup", screenshotOutput(shot))
			ref := f.putArtifact("chart", "image/png", chart, "sha256:"+mediaDigest(chart))
			_, run := f.startChatWithAttachments("What is in this chart?", ref)
			done := f.drive(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)
			// The references are stored either way; only the render differs.
			user, tool := storedImageParts(f.Fixture, run.ID)
			if len(user) != 1 || len(tool) != 1 {
				t.Fatalf("stored image parts = %#v %#v", user, tool)
			}
			requireNoStoredBytes(t, f.Fixture, run.ID, base64.StdEncoding.EncodeToString(chart), base64.StdEncoding.EncodeToString(shot))
		})
	})
}

// TestLoopArtifactMediaRequiresUnambiguousProvenance: an attachment becomes
// image context only when its exact artifact version passes the integrity
// checks, and every later step re-checks the bytes against that version: a
// bad attachment is a status element, and bytes that changed under the
// reference render as "[image unavailable]". A stored image part must name
// exactly one source and never carry bytes.
func TestLoopArtifactMediaRequiresUnambiguousProvenance(t *testing.T) {
	t.Run("attachments", func(t *testing.T) {
		looptest.EachStore(t, "media_provenance", func(t *testing.T, store models.KernelStore) {
			good, other := mediaPNG(t, 3, 2), mediaPNG(t, 5, 5)
			encoded := base64.StdEncoding.EncodeToString(good)
			unavailable := "[image unavailable: image/png]"
			var f *mediaFixture
			f = newMediaFixture(t, store, true,
				&looptest.ModelRule{Name: "after_swap", When: func(v looptest.StepView) bool {
					_, _, ok := v.ToolResult(looptest.GovernedTool(v, "lookup"))
					return ok
				}, Reply: func(v looptest.StepView) models.ModelStepResult {
					if user, _ := viewImageParts(v); len(user) != 0 {
						t.Errorf("changed bytes rendered as the pinned image: %#v", user)
					}
					if !strings.Contains(viewText(v), unavailable) {
						t.Errorf("no unavailable placeholder: %s", viewText(v))
					}
					return looptest.Say("The chart is no longer available.")
				}},
				&looptest.ModelRule{Name: "first", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
					user, _ := viewImageParts(v)
					if len(user) != 1 || user[0].ArtifactID != "good" || user[0].DataBase64 != encoded {
						t.Errorf("first step image parts = %#v", user)
					}
					text := viewText(v)
					for _, want := range []string{`id="tampered" version="1" name="tampered.png" status="integrity_failed"/>`, `id="fake" version="1" name="fake.png" status="unsupported_encoding"/>`} {
						if !strings.Contains(text, want) {
							t.Errorf("missing %s in %s", want, text)
						}
					}
					// The bytes behind the pinned version change before the next step.
					f.content.set("object-store:good", other)
					return looptest.Act("Checking.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "1"}))
				}},
			)
			refs := []models.ConversationReference{
				f.putArtifact("good", "image/png", good, "sha256:"+mediaDigest(good)),
				f.putArtifact("tampered", "image/png", good, "sha256:"+mediaDigest(other)),
				f.putArtifact("fake", "image/png", []byte("not an image"), "sha256:"+mediaDigest([]byte("not an image"))),
			}
			_, run := f.startChatWithAttachments("Compare these", refs...)
			done := f.drive(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)

			user, _ := storedImageParts(f.Fixture, run.ID)
			if len(user) != 1 || user[0].ArtifactID != "good" || user[0].ArtifactVersion != "1" || user[0].SHA256 != mediaDigest(good) {
				t.Fatalf("stored image references = %#v", user)
			}
			requireNoStoredBytes(t, f.Fixture, run.ID, encoded)
		})
	})

	t.Run("stored_parts", func(t *testing.T) {
		data := mediaPNG(t, 2, 2)
		entry := func(part models.ContentPart) *models.SessionEntry {
			return &models.SessionEntry{
				ID: "entry", Scope: models.Scope{Kind: "tenant", ID: "loop"}, SessionID: "session", RunID: "run", Type: models.SessionEntryUser,
				User: &models.UserEntry{Origin: models.UserEntryGoal, Author: models.SessionAuthor{Type: "user", ID: looptest.FixtureUser},
					Content: []models.ContentPart{{Type: models.ContentPartText, Text: "look"}, part}},
				Tokens: 1, CreatedAt: time.Now(),
			}
		}
		image := models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", SHA256: mediaDigest(data)}
		ok := image
		ok.ArtifactID, ok.ArtifactVersion = "chart", "1"
		if err := entry(ok).Validate(); err != nil {
			t.Fatalf("an artifact reference was refused: %v", err)
		}
		for name, change := range map[string]func(*models.ContentPart){
			"no_source":   func(p *models.ContentPart) {},
			"two_sources": func(p *models.ContentPart) { p.ArtifactID, p.ArtifactVersion, p.ActionCallID = "chart", "1", "call-1" },
			"inline_bytes": func(p *models.ContentPart) {
				p.ArtifactID, p.ArtifactVersion, p.DataBase64 = "chart", "1", base64.StdEncoding.EncodeToString(data)
			},
		} {
			part := image
			change(&part)
			if err := entry(part).Validate(); err == nil {
				t.Errorf("%s: stored image part %#v was accepted", name, part)
			}
		}
	})
}
