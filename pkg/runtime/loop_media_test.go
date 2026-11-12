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
)

// mediaContent serves artifact bytes by content ref; set swaps them.
type mediaContent struct {
	mu    sync.Mutex
	byRef map[string][]byte
}

func (c *mediaContent) Put(context.Context, ArtifactContentWrite) (ArtifactStoredContent, error) {
	return ArtifactStoredContent{}, fmt.Errorf("media content is read-only")
}

func (c *mediaContent) Open(_ context.Context, _ Scope, ref string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.byRef[ref]
	if !ok {
		return nil, ErrArtifactNotFound
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
	*loopFixture
	content *mediaContent
	vision  bool
}

func newMediaFixture(t *testing.T, store KernelStore, vision bool, rules ...*modelRule) *mediaFixture {
	t.Helper()
	f := &mediaFixture{loopFixture: newLoopFixture(t, store, withRules(rules...)), content: &mediaContent{byRef: map[string][]byte{}}, vision: vision}
	// The kit's conversation turn runner, reading attachments from content.
	runner, err := NewConversationRunTurnRunner(f.chats, f.coordinator(), ConversationRunTurnRunnerConfig{
		AttachmentContent: f.content, AgentTurns: TurnRunnerResolverFunc(f.agentBinding),
	}, models.WithClock(f.clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	f.runner = runner
	return f
}

// putArtifact stores version 1 of an artifact whose record says mediaType and
// digest, served with data.
func (f *mediaFixture) putArtifact(id, mediaType string, data []byte, recordDigest string) ConversationReference {
	f.t.Helper()
	artifacts, ok := f.raw.(ArtifactStore)
	if !ok {
		f.t.Fatalf("%T is not an artifact store", f.raw)
	}
	ref := "object-store:" + id
	f.content.set(ref, data)
	artifact := &Artifact{
		ID: id, Version: 1, Scope: f.scope, Name: id + ".png", MediaType: mediaType, ContentRef: ref,
		SizeBytes: int64(len(data)), Digest: recordDigest, Classification: ArtifactClassificationInternal,
		Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "user", ID: loopFixtureUser}}, CreatedAt: f.clock.Now(),
	}
	if err := artifacts.CreateArtifactVersion(f.ctx, artifact, 0); err != nil {
		f.t.Fatalf("store artifact %s: %v", id, err)
	}
	return ConversationReference{Kind: ConversationReferenceArtifact, ID: id, Version: 1}
}

// startChatWithAttachments posts text with references as ana and schedules it.
func (f *mediaFixture) startChatWithAttachments(text string, refs ...ConversationReference) (*Conversation, *AgentRun) {
	f.t.Helper()
	conversation, _, err := f.convs.CreateConversation(f.ctx, CreateConversationRequest{
		Scope: f.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: loopFixtureAgent}, Title: "Media chat",
		IdempotencyKey: "media-chat-" + fmt.Sprint(f.clock.Now().UnixNano()),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	posted, err := f.convs.PostChannelMessage(f.ctx, PostChannelMessageRequest{
		Scope: f.scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: loopFixtureUser}, Intent: MessageIntentQuestion,
		Content: text, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, References: refs,
		IdempotencyKey: "media-post-" + conversation.ID,
	})
	if err != nil {
		f.t.Fatalf("post message: %v", err)
	}
	return conversation, f.chatRun(conversation, posted.Message)
}

// bind gives the claim's model producer the artifact sources and the
// fixture's image modality, as the catalog resolver does from the catalog.
func (f *mediaFixture) bind(binding *TurnRunnerBinding) {
	producer, ok := binding.Producer.(*ModelStepProducer)
	if !ok {
		f.t.Fatalf("producer is %T", binding.Producer)
	}
	vision := f.vision
	producer.Media = ModelStepMedia{
		Artifacts: f.raw.(ArtifactStore), Content: f.content,
		ImageInput: func(context.Context, StepInput, ModelStepRequest) bool { return vision },
	}
}

// drive claims until the Run is terminal or waiting.
func (f *mediaFixture) drive(runID string, maxClaims int) *AgentRun {
	f.t.Helper()
	for i := 0; i < maxClaims; i++ {
		exit, run := f.runClaimWith(f.ctx, runID, f.bind)
		if exit.Err != nil && exit.Reason != RunLoopExitRequeued {
			f.t.Fatalf("claim %d exited %s: %v", i+1, exit.Reason, exit.Err)
		}
		if run.Status != AgentRunStatusQueued {
			return run
		}
		f.clock.Advance(time.Second)
	}
	f.t.Fatalf("run %s not at rest after %d claims", runID, maxClaims)
	return nil
}

// imageParts is every image part of the view's user and tool entries.
func viewImageParts(v stepView) (user, tool []ContentPart) {
	for _, e := range v.Request().Entries {
		switch {
		case e.Type == SessionEntryUser && e.User != nil:
			for _, p := range e.User.Content {
				if p.Type == ContentPartImage {
					user = append(user, p)
				}
			}
		case e.Type == SessionEntryTool && e.Tool != nil:
			for _, p := range e.Tool.Result.Content {
				if p.Type == ContentPartImage {
					tool = append(tool, p)
				}
			}
		}
	}
	return user, tool
}

// viewText is the text of every user and tool entry of the view.
func viewText(v stepView) string {
	var b strings.Builder
	for _, e := range v.Request().Entries {
		switch {
		case e.Type == SessionEntryUser && e.User != nil:
			b.WriteString(kitPartsText(e.User.Content) + "\n")
		case e.Type == SessionEntryTool && e.Tool != nil:
			b.WriteString(kitPartsText(e.Tool.Result.Content) + "\n")
		}
	}
	return b.String()
}

// storedImageParts is every image part of the Run's stored entries.
func storedImageParts(f *loopFixture, runID string) (user, tool []ContentPart) {
	f.t.Helper()
	for _, entry := range f.entries(runID) {
		switch {
		case entry.Type == SessionEntryUser && entry.User != nil:
			for _, p := range entry.User.Content {
				if p.Type == ContentPartImage {
					user = append(user, p)
				}
			}
		case entry.Type == SessionEntryTool && entry.Tool != nil && entry.Tool.Result != nil:
			for _, p := range entry.Tool.Result.Content {
				if p.Type == ContentPartImage {
					tool = append(tool, p)
				}
			}
		}
	}
	return user, tool
}

// requireNoStoredBytes fails when any stored entry of the Run holds encoded.
func requireNoStoredBytes(t *testing.T, f *loopFixture, runID string, encoded ...string) {
	t.Helper()
	raw, err := json.Marshal(f.entries(runID))
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
func screenshotOutput(data []byte) func(ActionDispatchInput) (map[string]interface{}, error) {
	return func(in ActionDispatchInput) (map[string]interface{}, error) {
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
		eachStore(t, "media_attachment", func(t *testing.T, store KernelStore) {
			chart := mediaPNG(t, 3, 2)
			encoded := base64.StdEncoding.EncodeToString(chart)
			f := newMediaFixture(t, store, true, &modelRule{name: "look", when: stepPurpose, reply: func(v stepView) ModelStepResult {
				user, _ := viewImageParts(v)
				if len(user) != 1 || user[0].DataBase64 != encoded || user[0].ArtifactID != "chart" || user[0].ArtifactVersion != "1" || user[0].SHA256 != mediaDigest(chart) {
					t.Errorf("step image parts = %#v", user)
				}
				return say("A blank chart.")
			}})
			ref := f.putArtifact("chart", "image/png", chart, "sha256:"+mediaDigest(chart))
			conversation, run := f.startChatWithAttachments("What is in this chart?", ref)
			done := f.drive(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)

			user, _ := storedImageParts(f.loopFixture, run.ID)
			if len(user) != 1 {
				t.Fatalf("stored image parts = %#v", user)
			}
			if got := user[0]; got.ArtifactID != "chart" || got.ArtifactVersion != "1" || got.ActionCallID != "" || got.SHA256 != mediaDigest(chart) || got.MediaType != "image/png" {
				t.Fatalf("stored image reference = %#v", got)
			}
			requireNoStoredBytes(t, f.loopFixture, run.ID, encoded)
			if replies := f.agentReplies(conversation.ID); !slices.Equal(replies, []string{"A blank chart."}) {
				t.Fatalf("replies = %q", replies)
			}
		})
	})

	t.Run("governed_screenshot", func(t *testing.T) {
		eachStore(t, "media_screenshot", func(t *testing.T, store KernelStore) {
			shot := mediaPNG(t, 4, 4)
			encoded := base64.StdEncoding.EncodeToString(shot)
			f := newMediaFixture(t, store, true,
				&modelRule{name: "answer", when: func(v stepView) bool { _, _, ok := v.ToolResult(kitGovernedTool(v, "lookup")); return ok }, reply: func(v stepView) ModelStepResult {
					_, tool := viewImageParts(v)
					if len(tool) != 1 || tool[0].DataBase64 != encoded || tool[0].ActionCallID == "" || tool[0].ArtifactID != "" || tool[0].SHA256 != mediaDigest(shot) {
						t.Errorf("step tool image parts = %#v", tool)
					}
					if content, _, _ := v.ToolResult(kitGovernedTool(v, "lookup")); strings.Contains(content, "modelMedia") || strings.Contains(content, encoded) {
						t.Errorf("the screenshot is in the tool result text: %q", content)
					}
					return say("The page shows issue 9.")
				}},
				&modelRule{name: "lookup", when: stepPurpose, reply: func(v stepView) ModelStepResult {
					return act("Looking.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "9"}))
				}},
			)
			f.dispatch.on("lookup", screenshotOutput(shot))
			conversation, run := f.startChatWithAttachments("Show me issue 9")
			done := f.drive(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)

			calls := f.calls(run.ID)
			if len(calls) != 1 || calls[0].Status != ActionCallStatusSucceeded {
				t.Fatalf("calls = %#v", calls)
			}
			_, tool := storedImageParts(f.loopFixture, run.ID)
			if len(tool) != 1 || tool[0].ActionCallID != calls[0].ID || tool[0].ArtifactID != "" || tool[0].SHA256 != mediaDigest(shot) || tool[0].Detail != "low" {
				t.Fatalf("stored tool image reference = %#v (call %s)", tool, calls[0].ID)
			}
			requireNoStoredBytes(t, f.loopFixture, run.ID, encoded)
			if replies := f.agentReplies(conversation.ID); !slices.Equal(replies, []string{"The page shows issue 9."}) {
				t.Fatalf("replies = %q", replies)
			}
		})
	})

	t.Run("text_only_model", func(t *testing.T) {
		eachStore(t, "media_text_only", func(t *testing.T, store KernelStore) {
			chart, shot := mediaPNG(t, 3, 2), mediaPNG(t, 4, 4)
			omitted := "[image omitted: image/png]"
			f := newMediaFixture(t, store, false,
				&modelRule{name: "answer", when: func(v stepView) bool { _, _, ok := v.ToolResult(kitGovernedTool(v, "lookup")); return ok }, reply: func(v stepView) ModelStepResult {
					if user, tool := viewImageParts(v); len(user)+len(tool) != 0 {
						t.Errorf("a text-only step got image parts: %#v %#v", user, tool)
					}
					if got := strings.Count(viewText(v), omitted); got != 2 {
						t.Errorf("placeholders = %d, want 2: %s", got, viewText(v))
					}
					return say("I can't see images here.")
				}},
				&modelRule{name: "lookup", when: stepPurpose, reply: func(v stepView) ModelStepResult {
					if user, _ := viewImageParts(v); len(user) != 0 || !strings.Contains(viewText(v), omitted) {
						t.Errorf("a text-only step saw the attachment: %#v / %s", user, viewText(v))
					}
					return act("Looking.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "9"}))
				}},
			)
			f.dispatch.on("lookup", screenshotOutput(shot))
			ref := f.putArtifact("chart", "image/png", chart, "sha256:"+mediaDigest(chart))
			_, run := f.startChatWithAttachments("What is in this chart?", ref)
			done := f.drive(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)
			// The references are stored either way; only the render differs.
			user, tool := storedImageParts(f.loopFixture, run.ID)
			if len(user) != 1 || len(tool) != 1 {
				t.Fatalf("stored image parts = %#v %#v", user, tool)
			}
			requireNoStoredBytes(t, f.loopFixture, run.ID, base64.StdEncoding.EncodeToString(chart), base64.StdEncoding.EncodeToString(shot))
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
		eachStore(t, "media_provenance", func(t *testing.T, store KernelStore) {
			good, other := mediaPNG(t, 3, 2), mediaPNG(t, 5, 5)
			encoded := base64.StdEncoding.EncodeToString(good)
			unavailable := "[image unavailable: image/png]"
			var f *mediaFixture
			f = newMediaFixture(t, store, true,
				&modelRule{name: "after_swap", when: func(v stepView) bool { _, _, ok := v.ToolResult(kitGovernedTool(v, "lookup")); return ok }, reply: func(v stepView) ModelStepResult {
					if user, _ := viewImageParts(v); len(user) != 0 {
						t.Errorf("changed bytes rendered as the pinned image: %#v", user)
					}
					if !strings.Contains(viewText(v), unavailable) {
						t.Errorf("no unavailable placeholder: %s", viewText(v))
					}
					return say("The chart is no longer available.")
				}},
				&modelRule{name: "first", when: stepPurpose, reply: func(v stepView) ModelStepResult {
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
					return act("Checking.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "1"}))
				}},
			)
			refs := []ConversationReference{
				f.putArtifact("good", "image/png", good, "sha256:"+mediaDigest(good)),
				f.putArtifact("tampered", "image/png", good, "sha256:"+mediaDigest(other)),
				f.putArtifact("fake", "image/png", []byte("not an image"), "sha256:"+mediaDigest([]byte("not an image"))),
			}
			_, run := f.startChatWithAttachments("Compare these", refs...)
			done := f.drive(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)

			user, _ := storedImageParts(f.loopFixture, run.ID)
			if len(user) != 1 || user[0].ArtifactID != "good" || user[0].ArtifactVersion != "1" || user[0].SHA256 != mediaDigest(good) {
				t.Fatalf("stored image references = %#v", user)
			}
			requireNoStoredBytes(t, f.loopFixture, run.ID, encoded)
		})
	})

	t.Run("stored_parts", func(t *testing.T) {
		data := mediaPNG(t, 2, 2)
		entry := func(part ContentPart) *SessionEntry {
			return &SessionEntry{
				ID: "entry", Scope: Scope{Kind: "tenant", ID: "loop"}, SessionID: "session", RunID: "run", Type: SessionEntryUser,
				User: &UserEntry{Origin: UserEntryGoal, Author: SessionAuthor{Type: "user", ID: loopFixtureUser},
					Content: []ContentPart{{Type: ContentPartText, Text: "look"}, part}},
				Tokens: 1, CreatedAt: time.Now(),
			}
		}
		image := ContentPart{Type: ContentPartImage, MediaType: "image/png", SHA256: mediaDigest(data)}
		ok := image
		ok.ArtifactID, ok.ArtifactVersion = "chart", "1"
		if err := entry(ok).Validate(); err != nil {
			t.Fatalf("an artifact reference was refused: %v", err)
		}
		for name, change := range map[string]func(*ContentPart){
			"no_source":   func(p *ContentPart) {},
			"two_sources": func(p *ContentPart) { p.ArtifactID, p.ArtifactVersion, p.ActionCallID = "chart", "1", "call-1" },
			"inline_bytes": func(p *ContentPart) {
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
