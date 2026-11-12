package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
)

// ---------------------------------------------------------------------------
// Fixtures

type renderFixture struct {
	t       *testing.T
	session string
	entries []*SessionEntry
}

func newRenderFixture(t *testing.T, session string) *renderFixture {
	return &renderFixture{t: t, session: session}
}

func (f *renderFixture) add(e *SessionEntry) *SessionEntry {
	e.Scope = testSessionScope
	e.SessionID = f.session
	e.Sequence = int64(len(f.entries) + 1)
	e.ID = f.session + "-" + fmt.Sprint(e.Sequence)
	e.CreatedAt = time.Unix(1_800_000_000+e.Sequence, 0).UTC()
	e.UpdatedAt = e.CreatedAt
	f.entries = append(f.entries, e)
	return e
}

func (f *renderFixture) user(run, text string, parts ...ContentPart) *SessionEntry {
	content := append([]ContentPart{{Type: ContentPartText, Text: text}}, parts...)
	return f.add(&SessionEntry{RunID: run, Type: SessionEntryUser, User: &UserEntry{
		Origin: UserEntryChannelMessage, Author: SessionAuthor{Type: "user", ID: "u1", DisplayName: "Ana"}, Content: content,
	}})
}

func (f *renderFixture) assistant(run, step string, calls ...string) *SessionEntry {
	a := modelStepTestAssistant(calls...)
	a.Limits = &AssistantModelLimits{ContextTokens: 200000, PromptTokens: 1200}
	return f.add(&SessionEntry{RunID: run, StepID: step, Type: SessionEntryAssistant, Assistant: a})
}

func (f *renderFixture) tool(run, step, call string, state ToolEntryState, parts ...ContentPart) *SessionEntry {
	tool := &ToolEntry{Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), SideEffect: ToolSideEffectRead}
	if state.Terminal() {
		status := ToolResultSucceeded
		switch state {
		case ToolEntryError:
			status = ToolResultFailed
		case ToolEntryInterrupted:
			status = ToolResultInterrupted
		}
		if len(parts) == 0 {
			parts = []ContentPart{{Type: ContentPartText, Text: "result of " + call}}
		}
		tool.Result = &ToolResult{Status: status, IsError: status != ToolResultSucceeded, Content: parts}
	}
	return f.add(&SessionEntry{RunID: run, StepID: step, Type: SessionEntryTool, State: state, ToolCallID: call, Tool: tool})
}

func (f *renderFixture) note(run, text string) *SessionEntry {
	return f.add(&SessionEntry{RunID: run, Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteRunContext, Text: text}})
}

func (f *renderFixture) compaction(run, firstKept string) *SessionEntry {
	return f.add(&SessionEntry{RunID: run, Type: SessionEntryCompaction, Compaction: &CompactionEntry{
		Summary: "Ana asked for the guest list; the lookup found 12 guests.", FirstKeptEntryID: firstKept,
		TokensBefore: 42000, ActionCallIDs: []string{"ac-old-1", "ac-old-2"},
		Provider: "anthropic", Model: "claude-test", Trigger: CompactionTriggerThreshold,
	}})
}

func (f *renderFixture) agentSession(parent *AgentSession, forkSequence int64) *AgentSession {
	s := &AgentSession{ID: f.session, Scope: testSessionScope, NextSequence: int64(len(f.entries) + 1)}
	if parent != nil {
		s.ParentSessionID, s.ForkSequence, s.Depth = parent.ID, forkSequence, parent.Depth+1
	}
	return s
}

func (f *renderFixture) path() *AgentSessionPath {
	f.t.Helper()
	path, err := buildAgentSessionPath([]*AgentSession{f.agentSession(nil, 0)}, [][]*SessionEntry{f.entries}, nil)
	if err != nil {
		f.t.Fatalf("buildAgentSessionPath: %v", err)
	}
	return path
}

func renderIDs(views []SessionEntryView) []string {
	ids := make([]string, len(views))
	for i, v := range views {
		ids[i] = v.ID
	}
	return ids
}

func requireRenderOrder(t *testing.T, views []SessionEntryView, want ...string) {
	t.Helper()
	got := renderIDs(views)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("render order = %v, want %v", got, want)
	}
	for i, v := range views {
		if i > 0 && v.Type != SessionEntryCompaction && views[i-1].Type != SessionEntryCompaction && v.Sequence <= views[i-1].Sequence {
			t.Fatalf("view %s sequence %d does not follow %d", v.ID, v.Sequence, views[i-1].Sequence)
		}
	}
}

func requireRenderedStep(t *testing.T, views []SessionEntryView) {
	t.Helper()
	req := validModelStepRequest()
	req.Entries = views
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func viewByID(t *testing.T, views []SessionEntryView, id string) SessionEntryView {
	t.Helper()
	for _, v := range views {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no view %s in %v", id, renderIDs(views))
	return SessionEntryView{}
}

func onlyText(t *testing.T, parts []ContentPart) string {
	t.Helper()
	if len(parts) != 1 || parts[0].Type != ContentPartText {
		t.Fatalf("parts = %+v, want one text part", parts)
	}
	return parts[0].Text
}

// Image sources.

type renderArtifacts struct {
	artifacts map[string]*Artifact
	gets      int
}

func (s *renderArtifacts) CreateArtifactVersion(context.Context, *Artifact, int64) error { return nil }
func (s *renderArtifacts) ListArtifacts(context.Context, ArtifactFilter) ([]*Artifact, error) {
	return nil, nil
}
func (s *renderArtifacts) GetArtifact(_ context.Context, scope Scope, id string, version int64) (*Artifact, error) {
	s.gets++
	a, ok := s.artifacts[fmt.Sprintf("%s@%d", id, version)]
	if !ok || a.Scope != scope {
		return nil, artifactcatalog.ErrArtifactNotFound
	}
	return a, nil
}

type renderContent map[string][]byte

func (c renderContent) Put(context.Context, ArtifactContentWrite) (ArtifactStoredContent, error) {
	return ArtifactStoredContent{}, nil
}
func (c renderContent) Open(_ context.Context, _ Scope, ref string) (io.ReadCloser, error) {
	data, ok := c[ref]
	if !ok {
		return nil, artifactcatalog.ErrArtifactNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type renderCalls struct {
	calls map[string]*ActionCall
	gets  int
}

func (s *renderCalls) GetActionCall(_ context.Context, scope Scope, id string) (*ActionCall, error) {
	s.gets++
	call, ok := s.calls[id]
	if !ok || call.Scope != scope {
		return nil, ErrActionNotFound
	}
	return call, nil
}

type renderImageSources struct {
	artifacts *renderArtifacts
	content   renderContent
	calls     *renderCalls
	photo     []byte // artifact "photo" v2
	shot      []byte // governed call "ac-shot"
}

func newRenderImageSources() *renderImageSources {
	photo := []byte("\x89PNG\r\n\x1a\nphoto-bytes")
	shot := []byte("\xff\xd8\xffscreenshot-bytes")
	return &renderImageSources{
		photo: photo, shot: shot,
		artifacts: &renderArtifacts{artifacts: map[string]*Artifact{
			"photo@2": {ID: "photo", Version: 2, Scope: testSessionScope, MediaType: "image/png", ContentRef: "ref-photo",
				SizeBytes: int64(len(photo)), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(photo))},
		}},
		content: renderContent{"ref-photo": photo},
		calls: &renderCalls{calls: map[string]*ActionCall{
			"ac-shot": {ID: "ac-shot", Scope: testSessionScope, Status: ActionCallStatusSucceeded, Output: map[string]interface{}{
				"url": "https://example.test",
				"modelMedia": map[string]interface{}{
					"mediaType": "image/jpeg", "detail": "high", "contentBase64": base64.StdEncoding.EncodeToString(shot),
				},
			}},
		}},
	}
}

func (s *renderImageSources) options(imageInput bool) sessionRenderOptions {
	return sessionRenderOptions{ImageInput: imageInput, Artifacts: s.artifacts, Content: s.content, Actions: s.calls}
}

func hexDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func photoPart(sum string) ContentPart {
	return ContentPart{Type: ContentPartImage, MediaType: "image/png", Detail: "auto", ArtifactID: "photo", ArtifactVersion: "2", SHA256: sum}
}

func shotPart(sum string) ContentPart {
	return ContentPart{Type: ContentPartImage, MediaType: "image/jpeg", Detail: "high", ActionCallID: "ac-shot", SHA256: sum}
}

// ---------------------------------------------------------------------------
// Tests

func TestRenderOpenEntryOfOtherRunIsSynthetic(t *testing.T) {
	f := newRenderFixture(t, "lane")
	uA := f.user("run-a", "book the venue")
	aA := f.assistant("run-a", "step-a", "a1")
	uB := f.user("run-b", "what's the guest count?") // another Run of the lane appends between
	tA := f.tool("run-a", "step-a", "a1", ToolEntryAwaitingApproval)
	aB := f.assistant("run-b", "step-b", "b1", "b2")
	tB2 := f.tool("run-b", "step-b", "b2", ToolEntryCompleted)
	tB1 := f.tool("run-b", "step-b", "b1", ToolEntryError)
	f.tool("run-gone", "step-gone", "x1", ToolEntryCompleted) // R3: no assistant on the path

	views, err := projectSessionPath(context.Background(), f.path(), sessionRenderOptions{})
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	// R1: tool entries follow their assistant in call order; R3: the orphan is dropped.
	requireRenderOrder(t, views, uA.ID, aA.ID, tA.ID, uB.ID, aB.ID, tB1.ID, tB2.ID)

	open := viewByID(t, views, tA.ID).Tool
	if open.State != ToolEntryCompleted || open.Result.IsError || onlyText(t, open.Result.Content) != renderOpenToolText {
		t.Fatalf("open entry of other Run = %+v, want completed with %q", open, renderOpenToolText)
	}
	if open.CallID != "a1" || open.Name != "lookup" || string(open.Arguments) != `{"q":"x"}` {
		t.Fatalf("open entry identity = %+v", open)
	}
	failed := viewByID(t, views, tB1.ID).Tool
	if failed.State != ToolEntryError || !failed.Result.IsError || onlyText(t, failed.Result.Content) != "result of b1" {
		t.Fatalf("error entry = %+v", failed)
	}
	if a := viewByID(t, views, aB.ID).Assistant; a.Limits != nil {
		t.Fatalf("assistant view keeps Limits: %+v", a.Limits)
	}
	if f.entries[1].Assistant.Limits == nil {
		t.Fatal("rendering stripped Limits from the stored entry")
	}
	requireRenderedStep(t, views)
}

func TestRenderMissingToolEntryIsSynthetic(t *testing.T) {
	f := newRenderFixture(t, "lane")
	u := f.user("run-a", "compare two venues")
	a := f.assistant("run-a", "step-a", "c1", "c2")
	t1 := f.tool("run-a", "step-a", "c1", ToolEntryCompleted)
	f.tool("run-a", "step-other", "c2", ToolEntryCompleted)                                                    // same call id, other step: not this assistant's
	f.add(&SessionEntry{RunID: "run-det", Type: SessionEntryTool, State: ToolEntryCompleted, ToolCallID: "c2", // directive entry: no step
		Tool: &ToolEntry{Name: "lookup", Arguments: json.RawMessage(`{}`), SideEffect: ToolSideEffectRead,
			Result: &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartText, Text: "directive"}}}}})

	views, err := projectSessionPath(context.Background(), f.path(), sessionRenderOptions{})
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	if len(views) != 4 || views[0].ID != u.ID || views[1].ID != a.ID || views[2].ID != t1.ID {
		t.Fatalf("render order = %v", renderIDs(views))
	}
	missing := views[3]
	if missing.Type != SessionEntryTool || missing.Tool.CallID != "c2" || missing.Tool.State != ToolEntryCompleted ||
		onlyText(t, missing.Tool.Result.Content) != renderMissingToolText {
		t.Fatalf("missing call view = %+v", missing.Tool)
	}
	requireRenderedStep(t, views)
}

func TestRenderImageRefResolvesBytes(t *testing.T) {
	src := newRenderImageSources()
	f := newRenderFixture(t, "lane")
	u := f.user("run-a", "[Ana · m1] here is the room", photoPart(hexDigest(src.photo)), photoPart(""))
	f.assistant("run-a", "step-a", "c1")
	tool := f.tool("run-a", "step-a", "c1", ToolEntryCompleted,
		ContentPart{Type: ContentPartJSON, Value: json.RawMessage(`{"evidenceRef":"action:ac-shot"}`)}, shotPart(hexDigest(src.shot)))

	views, err := projectSessionPath(context.Background(), f.path(), src.options(true))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	user := viewByID(t, views, u.ID).User
	if user.Author != nil {
		t.Fatalf("channel message author = %+v, want nil (the header carries attribution)", user.Author)
	}
	for _, i := range []int{1, 2} {
		part := user.Content[i]
		if part.Type != ContentPartImage || part.DataBase64 != base64.StdEncoding.EncodeToString(src.photo) || part.SHA256 != hexDigest(src.photo) {
			t.Fatalf("user image %d = %+v", i, part)
		}
	}
	if src.artifacts.gets != 1 {
		t.Fatalf("artifact reads = %d, want 1 (cached per render)", src.artifacts.gets)
	}
	result := viewByID(t, views, tool.ID).Tool.Result.Content
	if len(result) != 2 || result[0].Type != ContentPartJSON || string(result[0].Value) != `{"evidenceRef":"action:ac-shot"}` {
		t.Fatalf("json part did not pass through: %+v", result)
	}
	if shot := result[1]; shot.Type != ContentPartImage || shot.MediaType != "image/jpeg" || shot.Detail != "high" ||
		shot.ActionCallID != "ac-shot" || shot.DataBase64 != base64.StdEncoding.EncodeToString(src.shot) {
		t.Fatalf("screenshot part = %+v", shot)
	}
	if stored := f.entries[0].User.Content[1]; stored.DataBase64 != "" {
		t.Fatal("rendering wrote bytes into the stored entry")
	}
	requireRenderedStep(t, views)
}

func TestRenderUnavailableImageBecomesText(t *testing.T) {
	src := newRenderImageSources()
	src.calls.calls["ac-big"] = &ActionCall{ID: "ac-big", Scope: testSessionScope, Output: map[string]interface{}{
		"modelMedia": map[string]interface{}{"mediaType": "image/png",
			"contentBase64": base64.StdEncoding.EncodeToString(make([]byte, maxModelStepMediaBytes+1))},
	}}
	src.artifacts.artifacts["tampered@1"] = &Artifact{ID: "tampered", Version: 1, Scope: testSessionScope, MediaType: "image/png",
		ContentRef: "ref-photo", SizeBytes: int64(len(src.photo)), Digest: "sha256:00"}
	f := newRenderFixture(t, "lane")
	u := f.user("run-a", "pictures",
		photoPart("deadbeef"), // digest mismatch
		ContentPart{Type: ContentPartImage, MediaType: "image/png", ArtifactID: "gone", ArtifactVersion: "1"},
		ContentPart{Type: ContentPartImage, MediaType: "image/png", ArtifactID: "tampered", ArtifactVersion: "1"},
		ContentPart{Type: ContentPartImage, MediaType: "image/png", ActionCallID: "ac-big"},
		ContentPart{Type: ContentPartImage, MediaType: "image/webp", ActionCallID: "ac-missing"},
	)

	views, err := projectSessionPath(context.Background(), f.path(), src.options(true))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	content := viewByID(t, views, u.ID).User.Content
	want := []string{"pictures", renderImageUnavailableText("image/png"), renderImageUnavailableText("image/png"),
		renderImageUnavailableText("image/png"), renderImageUnavailableText("image/png"), renderImageUnavailableText("image/webp")}
	if len(content) != len(want) {
		t.Fatalf("content = %+v", content)
	}
	for i, text := range want {
		if content[i].Type != ContentPartText || content[i].Text != text {
			t.Fatalf("part %d = %+v, want text %q", i, content[i], text)
		}
	}
	requireRenderedStep(t, views)
}

func TestRenderNonVisionReplacesImage(t *testing.T) {
	src := newRenderImageSources()
	f := newRenderFixture(t, "lane")
	u := f.user("run-a", "look", photoPart(hexDigest(src.photo)))
	f.assistant("run-a", "step-a", "c1")
	tool := f.tool("run-a", "step-a", "c1", ToolEntryCompleted, ContentPart{Type: ContentPartText, Text: "opened"}, shotPart(""))

	views, err := projectSessionPath(context.Background(), f.path(), src.options(false))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	user := viewByID(t, views, u.ID).User.Content
	if len(user) != 2 || user[1].Type != ContentPartText || user[1].Text != "[image omitted: image/png]" {
		t.Fatalf("user content = %+v", user)
	}
	result := viewByID(t, views, tool.ID).Tool.Result.Content
	if len(result) != 2 || result[1].Type != ContentPartText || result[1].Text != "[image omitted: image/jpeg]" {
		t.Fatalf("tool content = %+v", result)
	}
	if src.artifacts.gets != 0 || src.calls.gets != 0 {
		t.Fatalf("non-vision render read image sources (artifacts %d, calls %d)", src.artifacts.gets, src.calls.gets)
	}
	requireRenderedStep(t, views)
}

func TestRenderCompactionUsesSummaryText(t *testing.T) {
	f := newRenderFixture(t, "lane")
	f.user("run-a", "old request")
	f.assistant("run-a", "step-1", "c1")
	f.tool("run-a", "step-1", "c1", ToolEntryCompleted)
	kept := f.user("run-a", "recent request")
	keptA := f.assistant("run-a", "step-2", "c2")
	keptT := f.tool("run-a", "step-2", "c2", ToolEntryCompleted)
	compaction := f.compaction("run-a", kept.ID)
	after := f.note("run-a", "Run context")
	afterA := f.assistant("run-a", "step-3")

	path := f.path()
	views, err := projectSessionPath(context.Background(), path, sessionRenderOptions{})
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	requireRenderOrder(t, views, compaction.ID, kept.ID, keptA.ID, keptT.ID, after.ID, afterA.ID)
	view := views[0].Compaction
	if view.Summary != CompactionSummaryText(compaction.Compaction) || view.FirstKeptEntryID != kept.ID || view.TokensBefore != 42000 {
		t.Fatalf("compaction view = %+v", view)
	}
	if !strings.Contains(view.Summary, compactionSummaryPreamble) || !strings.Contains(view.Summary, "ac-old-1, ac-old-2") {
		t.Fatalf("summary text = %q", view.Summary)
	}
	// Kept tail numbers before the compaction, later entries after it.
	if views[1].Sequence != 1 || views[3].Sequence != 3 || views[0].Sequence != 4 || views[4].Sequence != 5 {
		t.Fatalf("sequences = %d %d %d %d", views[0].Sequence, views[1].Sequence, views[3].Sequence, views[4].Sequence)
	}
	if note := views[4].Note; note.Kind != string(NoteRunContext) || note.Text != "Run context" {
		t.Fatalf("note view = %+v", note)
	}
	requireRenderedStep(t, views)

	// Keep nothing: the compaction alone precedes the later entries.
	f2 := newRenderFixture(t, "lane2")
	f2.user("run-a", "old")
	c2 := f2.compaction("run-a", "")
	u2 := f2.user("run-a", "next")
	views, err = projectSessionPath(context.Background(), f2.path(), sessionRenderOptions{})
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	requireRenderOrder(t, views, c2.ID, u2.ID)
	if views[0].Sequence != 1 || views[1].Sequence != 2 || views[0].Compaction.FirstKeptEntryID != "" {
		t.Fatalf("keep-nothing views = %+v", views)
	}
	requireRenderedStep(t, views)
}

func TestRenderedViewPassesModelStepValidate(t *testing.T) {
	src := newRenderImageSources()
	// Parent chat session, forked into a child whose sequences restart at 1.
	parent := newRenderFixture(t, "chat")
	parent.user("run-a", "plan the launch", photoPart(""))
	parent.assistant("run-a", "step-1", "c1")
	parent.tool("run-a", "step-1", "c1", ToolEntryCompleted, ContentPart{Type: ContentPartText, Text: "found"}, shotPart(""))
	keep := parent.user("run-a", "now the guest list")
	parent.assistant("run-a", "step-2", "c2", "c3")
	parent.user("run-b", "interleaved message")
	parent.tool("run-a", "step-2", "c3", ToolEntryInterrupted)
	parent.tool("run-a", "step-2", "c2", ToolEntryRunning)
	parentSession := parent.agentSession(nil, 0)

	child := newRenderFixture(t, "task")
	child.add(&SessionEntry{RunID: "run-t", Type: SessionEntryUser, User: &UserEntry{
		Origin: UserEntryIntervention, InterventionID: "i1", Author: SessionAuthor{Type: "user", ID: "u1", DisplayName: "Ana"},
		Content: []ContentPart{{Type: ContentPartText, Text: "also invite Kim"}},
	}})
	child.compaction("run-t", keep.ID)
	child.assistant("run-t", "step-t1", "t1")
	child.tool("run-t", "step-t1", "t1", ToolEntryCompleted)
	child.add(&SessionEntry{RunID: "run-t", Type: SessionEntryModelChange, ModelChange: &ModelChangeEntry{Provider: "openai-responses", Model: "gpt-test"}})
	childSession := child.agentSession(parentSession, int64(len(parent.entries)))

	path, err := buildAgentSessionPath([]*AgentSession{parentSession, childSession}, [][]*SessionEntry{parent.entries, child.entries}, nil)
	if err != nil {
		t.Fatalf("buildAgentSessionPath: %v", err)
	}
	views, err := projectSessionPath(context.Background(), path, src.options(true))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	if views[0].Type != SessionEntryCompaction || views[0].Compaction.FirstKeptEntryID != keep.ID {
		t.Fatalf("first view = %+v", views[0])
	}
	intervention := viewByID(t, views, child.entries[0].ID).User
	if intervention.Author == nil || intervention.Author.Kind != EntryAuthorPerson || intervention.Author.Label != "Ana" {
		t.Fatalf("intervention author = %+v", intervention.Author)
	}
	running := viewByID(t, views, parent.entries[7].ID).Tool
	if running.CallID != "c2" || onlyText(t, running.Result.Content) != renderOpenToolText {
		t.Fatalf("running entry = %+v", running)
	}
	requireRenderedStep(t, views)

	// The same path without image input also validates.
	views, err = projectSessionPath(context.Background(), path, src.options(false))
	if err != nil {
		t.Fatalf("projectSessionPath without images: %v", err)
	}
	requireRenderedStep(t, views)

	// The uncompacted parent alone, images included.
	views, err = projectSessionPath(context.Background(), parent.path(), src.options(true))
	if err != nil {
		t.Fatalf("projectSessionPath parent: %v", err)
	}
	if got := views[0].User.Content[1].DataBase64; got != base64.StdEncoding.EncodeToString(src.photo) {
		t.Fatalf("parent image not resolved: %q", got)
	}
	requireRenderedStep(t, views)
}
