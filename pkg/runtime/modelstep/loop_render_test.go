package modelstep_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// ---------------------------------------------------------------------------
// Fixtures

func requireRenderOrder(t *testing.T, views []models.SessionEntryView, want ...string) {
	t.Helper()
	got := modelsteptest.RenderIDs(views)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("render order = %v, want %v", got, want)
	}
	for i, v := range views {
		if i > 0 && v.Type != models.SessionEntryCompaction && views[i-1].Type != models.SessionEntryCompaction && v.Sequence <= views[i-1].Sequence {
			t.Fatalf("view %s sequence %d does not follow %d", v.ID, v.Sequence, views[i-1].Sequence)
		}
	}
}

func onlyText(t *testing.T, parts []models.ContentPart) string {
	t.Helper()
	if len(parts) != 1 || parts[0].Type != models.ContentPartText {
		t.Fatalf("parts = %+v, want one text part", parts)
	}
	return parts[0].Text
}

// Image sources.

func hexDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func photoPart(sum string) models.ContentPart {
	return models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", Detail: "auto", ArtifactID: "photo", ArtifactVersion: "2", SHA256: sum}
}

func shotPart(sum string) models.ContentPart {
	return models.ContentPart{Type: models.ContentPartImage, MediaType: "image/jpeg", Detail: "high", ActionCallID: "ac-shot", SHA256: sum}
}

// ---------------------------------------------------------------------------
// Tests

func TestRenderOpenEntryOfOtherRunIsSynthetic(t *testing.T) {
	f := modelsteptest.NewRenderFixture(t, "lane")
	uA := f.User("run-a", "book the venue")
	aA := f.Assistant("run-a", "step-a", "a1")
	uB := f.User("run-b", "what's the guest count?") // another Run of the lane appends between
	tA := f.Tool("run-a", "step-a", "a1", models.ToolEntryAwaitingApproval)
	aB := f.Assistant("run-b", "step-b", "b1", "b2")
	tB2 := f.Tool("run-b", "step-b", "b2", models.ToolEntryCompleted)
	tB1 := f.Tool("run-b", "step-b", "b1", models.ToolEntryError)
	f.Tool("run-gone", "step-gone", "x1", models.ToolEntryCompleted) // R3: no assistant on the path

	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), modelstep.SessionRenderOptions{})
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	// R1: tool entries follow their assistant in call order; R3: the orphan is dropped.
	requireRenderOrder(t, views, uA.ID, aA.ID, tA.ID, uB.ID, aB.ID, tB1.ID, tB2.ID)

	open := modelsteptest.ViewByID(t, views, tA.ID).Tool
	if open.State != models.ToolEntryCompleted || open.Result.IsError || onlyText(t, open.Result.Content) != modelstep.RenderOpenToolText {
		t.Fatalf("open entry of other Run = %+v, want completed with %q", open, modelstep.RenderOpenToolText)
	}
	if open.CallID != "a1" || open.Name != "lookup" || string(open.Arguments) != `{"q":"x"}` {
		t.Fatalf("open entry identity = %+v", open)
	}
	failed := modelsteptest.ViewByID(t, views, tB1.ID).Tool
	if failed.State != models.ToolEntryError || !failed.Result.IsError || onlyText(t, failed.Result.Content) != "result of b1" {
		t.Fatalf("error entry = %+v", failed)
	}
	if a := modelsteptest.ViewByID(t, views, aB.ID).Assistant; a.Limits != nil {
		t.Fatalf("assistant view keeps Limits: %+v", a.Limits)
	}
	if f.Entries[1].Assistant.Limits == nil {
		t.Fatal("rendering stripped Limits from the stored entry")
	}
	modelsteptest.RequireRenderedStep(t, views)
}

func TestRenderMissingToolEntryIsSynthetic(t *testing.T) {
	f := modelsteptest.NewRenderFixture(t, "lane")
	u := f.User("run-a", "compare two venues")
	a := f.Assistant("run-a", "step-a", "c1", "c2")
	t1 := f.Tool("run-a", "step-a", "c1", models.ToolEntryCompleted)
	f.Tool("run-a", "step-other", "c2", models.ToolEntryCompleted)                                                                  // same call id, other step: not this assistant's
	f.Add(&models.SessionEntry{RunID: "run-det", Type: models.SessionEntryTool, State: models.ToolEntryCompleted, ToolCallID: "c2", // directive entry: no step
		Tool: &models.ToolEntry{Name: "lookup", Arguments: json.RawMessage(`{}`), SideEffect: models.ToolSideEffectRead,
			Result: &models.ToolResult{Status: models.ToolResultSucceeded, Content: []models.ContentPart{{Type: models.ContentPartText, Text: "directive"}}}}})

	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), modelstep.SessionRenderOptions{})
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	if len(views) != 4 || views[0].ID != u.ID || views[1].ID != a.ID || views[2].ID != t1.ID {
		t.Fatalf("render order = %v", modelsteptest.RenderIDs(views))
	}
	missing := views[3]
	if missing.Type != models.SessionEntryTool || missing.Tool.CallID != "c2" || missing.Tool.State != models.ToolEntryCompleted ||
		onlyText(t, missing.Tool.Result.Content) != modelstep.RenderMissingToolText {
		t.Fatalf("missing call view = %+v", missing.Tool)
	}
	modelsteptest.RequireRenderedStep(t, views)
}

func TestRenderImageRefResolvesBytes(t *testing.T) {
	src := modelsteptest.NewRenderImageSources()
	f := modelsteptest.NewRenderFixture(t, "lane")
	u := f.User("run-a", "[Ana · m1] here is the room", photoPart(hexDigest(src.Photo)), photoPart(""))
	f.Assistant("run-a", "step-a", "c1")
	tool := f.Tool("run-a", "step-a", "c1", models.ToolEntryCompleted,
		models.ContentPart{Type: models.ContentPartJSON, Value: json.RawMessage(`{"evidenceRef":"action:ac-shot"}`)}, shotPart(hexDigest(src.Shot)))

	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), src.Options(true))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	user := modelsteptest.ViewByID(t, views, u.ID).User
	if user.Author != nil {
		t.Fatalf("channel message author = %+v, want nil (the header carries attribution)", user.Author)
	}
	for _, i := range []int{1, 2} {
		part := user.Content[i]
		if part.Type != models.ContentPartImage || part.DataBase64 != base64.StdEncoding.EncodeToString(src.Photo) || part.SHA256 != hexDigest(src.Photo) {
			t.Fatalf("user image %d = %+v", i, part)
		}
	}
	if src.Artifacts.Gets != 1 {
		t.Fatalf("artifact reads = %d, want 1 (cached per render)", src.Artifacts.Gets)
	}
	result := modelsteptest.ViewByID(t, views, tool.ID).Tool.Result.Content
	if len(result) != 2 || result[0].Type != models.ContentPartJSON || string(result[0].Value) != `{"evidenceRef":"action:ac-shot"}` {
		t.Fatalf("json part did not pass through: %+v", result)
	}
	if shot := result[1]; shot.Type != models.ContentPartImage || shot.MediaType != "image/jpeg" || shot.Detail != "high" ||
		shot.ActionCallID != "ac-shot" || shot.DataBase64 != base64.StdEncoding.EncodeToString(src.Shot) {
		t.Fatalf("screenshot part = %+v", shot)
	}
	if stored := f.Entries[0].User.Content[1]; stored.DataBase64 != "" {
		t.Fatal("rendering wrote bytes into the stored entry")
	}
	modelsteptest.RequireRenderedStep(t, views)
}

// A Skill may return a screenshot over the hosted 1 MiB rule; the model gets
// it scaled like any other image, not "image unavailable".
func TestRenderScalesALargeActionImage(t *testing.T) {
	src := modelsteptest.NewRenderImageSources()
	big := image.NewNRGBA(image.Rect(0, 0, 2000, 1000))
	random := rand.New(rand.NewPCG(3, 4))
	for i := range big.Pix {
		big.Pix[i] = uint8(random.IntN(256))
	}
	for i := 3; i < len(big.Pix); i += 4 {
		big.Pix[i] = 0xff
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, big, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() <= models.MaxModelStepMediaBytes || encoded.Len() > models.MaxModelImageSourceBytes {
		t.Fatalf("fixture is %d bytes; want over 1 MiB and within the source limit", encoded.Len())
	}
	src.Calls.Calls["ac-shot"].Output["modelMedia"] = map[string]interface{}{"mediaType": "image/jpeg", "contentBase64": base64.StdEncoding.EncodeToString(encoded.Bytes())}
	f := modelsteptest.NewRenderFixture(t, "lane")
	f.User("run-a", "check this")
	f.Assistant("run-a", "step-a", "c1")
	tool := f.Tool("run-a", "step-a", "c1", models.ToolEntryCompleted, shotPart(hexDigest(encoded.Bytes())))
	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), src.Options(true))
	if err != nil {
		t.Fatal(err)
	}
	shot := modelsteptest.ViewByID(t, views, tool.ID).Tool.Result.Content[0]
	data, err := base64.StdEncoding.DecodeString(shot.DataBase64)
	if shot.Type != models.ContentPartImage || err != nil || len(data) > models.MaxModelStepMediaBytes {
		t.Fatalf("large screenshot = %s, %d bytes, %v", shot.Type, len(data), err)
	}
	if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || config.Width != modelstep.MaxModelImageEdge {
		t.Fatalf("scaled to %d wide (%v), want %d", config.Width, err, modelstep.MaxModelImageEdge)
	}
	modelsteptest.RequireRenderedStep(t, views)
}

func TestRenderUnavailableImageBecomesText(t *testing.T) {
	src := modelsteptest.NewRenderImageSources()
	src.Calls.Calls["ac-big"] = &models.ActionCall{ID: "ac-big", Scope: modelsteptest.SessionScope, Output: map[string]interface{}{
		"modelMedia": map[string]interface{}{"mediaType": "image/png",
			"contentBase64": base64.StdEncoding.EncodeToString(make([]byte, models.MaxModelStepMediaBytes+1))},
	}}
	src.Artifacts.Artifacts["tampered@1"] = &models.Artifact{ID: "tampered", Version: 1, Scope: modelsteptest.SessionScope, MediaType: "image/png",
		ContentRef: "ref-photo", SizeBytes: int64(len(src.Photo)), Digest: "sha256:00"}
	f := modelsteptest.NewRenderFixture(t, "lane")
	u := f.User("run-a", "pictures",
		photoPart("deadbeef"), // digest mismatch
		models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", ArtifactID: "gone", ArtifactVersion: "1"},
		models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", ArtifactID: "tampered", ArtifactVersion: "1"},
		models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", ActionCallID: "ac-big"},
		models.ContentPart{Type: models.ContentPartImage, MediaType: "image/webp", ActionCallID: "ac-missing"},
	)

	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), src.Options(true))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	content := modelsteptest.ViewByID(t, views, u.ID).User.Content
	want := []string{"pictures", modelstep.RenderImageUnavailableText("image/png"), modelstep.RenderImageUnavailableText("image/png"),
		modelstep.RenderImageUnavailableText("image/png"), modelstep.RenderImageUnavailableText("image/png"), modelstep.RenderImageUnavailableText("image/webp")}
	if len(content) != len(want) {
		t.Fatalf("content = %+v", content)
	}
	for i, text := range want {
		if content[i].Type != models.ContentPartText || content[i].Text != text {
			t.Fatalf("part %d = %+v, want text %q", i, content[i], text)
		}
	}
	modelsteptest.RequireRenderedStep(t, views)
}

func chatFilePart(path string, size int) models.ContentPart {
	return models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", Path: path, SizeBytes: int64(size)}
}

// A tool image kept as a chat file resolves from the rendering Run's chat;
// one that is missing, of another size or of another media type renders as
// unavailable, as does any chat file without a chat file reader.
func TestRenderChatFileImage(t *testing.T) {
	src := modelsteptest.NewRenderImageSources()
	src.ChatFiles.Files["tool-output/ac-read/renamed.png"] = modelsteptest.RenderChatFile{Data: src.ChatShot, MediaType: "image/jpeg"}
	f := modelsteptest.NewRenderFixture(t, "lane")
	f.User("run-a", "read the screenshot")
	f.Assistant("run-a", "step-a", "c1")
	tool := f.Tool("run-a", "step-a", "c1", models.ToolEntryCompleted,
		models.ContentPart{Type: models.ContentPartJSON, Value: json.RawMessage(`{"evidenceRef":"action-call:ac-read"}`)},
		chatFilePart(modelsteptest.ChatShotPath, len(src.ChatShot)),
		chatFilePart(modelsteptest.ChatShotPath, len(src.ChatShot)+1),
		chatFilePart("tool-output/ac-read/gone.png", 10),
		chatFilePart("tool-output/ac-read/renamed.png", len(src.ChatShot)),
	)

	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), src.Options(true))
	if err != nil {
		t.Fatal(err)
	}
	result := modelsteptest.ViewByID(t, views, tool.ID).Tool.Result.Content
	if len(result) != 5 {
		t.Fatalf("content = %+v", result)
	}
	if shot := result[1]; shot.Type != models.ContentPartImage || shot.Path != modelsteptest.ChatShotPath ||
		shot.DataBase64 != base64.StdEncoding.EncodeToString(src.ChatShot) || shot.SHA256 != hexDigest(src.ChatShot) {
		t.Fatalf("chat file image = %+v", shot)
	}
	unavailable := modelstep.RenderImageUnavailableText("image/png")
	for i := 2; i < 5; i++ {
		if result[i].Type != models.ContentPartText || result[i].Text != unavailable {
			t.Fatalf("part %d = %+v, want %q", i, result[i], unavailable)
		}
	}
	if src.ChatFiles.Reads != 3 {
		t.Fatalf("chat file reads = %d, want 3 (cached per path)", src.ChatFiles.Reads)
	}
	if stored := f.Entries[2].Tool.Result.Content[1]; stored.DataBase64 != "" {
		t.Fatal("rendering wrote bytes into the stored entry")
	}
	modelsteptest.RequireRenderedStep(t, views)

	without := src.Options(true)
	without.ChatFiles = nil
	views, err = modelstep.ProjectSessionPath(context.Background(), f.Path(), without)
	if err != nil {
		t.Fatal(err)
	}
	if got := modelsteptest.ViewByID(t, views, tool.ID).Tool.Result.Content[1]; got.Type != models.ContentPartText || got.Text != unavailable {
		t.Fatalf("without a reader = %+v", got)
	}

	views, err = modelstep.ProjectSessionPath(context.Background(), f.Path(), src.Options(false))
	if err != nil {
		t.Fatal(err)
	}
	if got := modelsteptest.ViewByID(t, views, tool.ID).Tool.Result.Content[1]; got.Type != models.ContentPartText || got.Text != "[image omitted: image/png]" {
		t.Fatalf("non-vision = %+v", got)
	}
}

// A large tool image kept as a chat file is scaled like every other image.
func TestRenderScalesALargeChatFileImage(t *testing.T) {
	src := modelsteptest.NewRenderImageSources()
	big := image.NewNRGBA(image.Rect(0, 0, 2000, 1000))
	random := rand.New(rand.NewPCG(5, 6))
	for i := range big.Pix {
		big.Pix[i] = uint8(random.IntN(256))
	}
	for i := 3; i < len(big.Pix); i += 4 {
		big.Pix[i] = 0xff
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, big, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() <= models.MaxModelStepMediaBytes || encoded.Len() > models.MaxModelImageSourceBytes {
		t.Fatalf("fixture is %d bytes; want over 1 MiB and within the source limit", encoded.Len())
	}
	path := "tool-output/ac-read/photo.jpg"
	src.ChatFiles.Files[path] = modelsteptest.RenderChatFile{Data: encoded.Bytes(), MediaType: "image/jpeg"}
	f := modelsteptest.NewRenderFixture(t, "lane")
	f.User("run-a", "check this")
	f.Assistant("run-a", "step-a", "c1")
	tool := f.Tool("run-a", "step-a", "c1", models.ToolEntryCompleted,
		models.ContentPart{Type: models.ContentPartImage, MediaType: "image/jpeg", Path: path, SizeBytes: int64(encoded.Len())})
	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), src.Options(true))
	if err != nil {
		t.Fatal(err)
	}
	shot := modelsteptest.ViewByID(t, views, tool.ID).Tool.Result.Content[0]
	data, err := base64.StdEncoding.DecodeString(shot.DataBase64)
	if shot.Type != models.ContentPartImage || err != nil || len(data) > models.MaxModelStepMediaBytes {
		t.Fatalf("large chat file image = %s, %d bytes, %v", shot.Type, len(data), err)
	}
	if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || config.Width != modelstep.MaxModelImageEdge {
		t.Fatalf("scaled to %d wide (%v), want %d", config.Width, err, modelstep.MaxModelImageEdge)
	}
	modelsteptest.RequireRenderedStep(t, views)
}

func TestRenderNonVisionReplacesImage(t *testing.T) {
	src := modelsteptest.NewRenderImageSources()
	f := modelsteptest.NewRenderFixture(t, "lane")
	u := f.User("run-a", "look", photoPart(hexDigest(src.Photo)))
	f.Assistant("run-a", "step-a", "c1")
	tool := f.Tool("run-a", "step-a", "c1", models.ToolEntryCompleted, models.ContentPart{Type: models.ContentPartText, Text: "opened"}, shotPart(""))

	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), src.Options(false))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	user := modelsteptest.ViewByID(t, views, u.ID).User.Content
	if len(user) != 2 || user[1].Type != models.ContentPartText || user[1].Text != "[image omitted: image/png]" {
		t.Fatalf("user content = %+v", user)
	}
	result := modelsteptest.ViewByID(t, views, tool.ID).Tool.Result.Content
	if len(result) != 2 || result[1].Type != models.ContentPartText || result[1].Text != "[image omitted: image/jpeg]" {
		t.Fatalf("tool content = %+v", result)
	}
	if src.Artifacts.Gets != 0 || src.Calls.Gets != 0 {
		t.Fatalf("non-vision render read image sources (artifacts %d, calls %d)", src.Artifacts.Gets, src.Calls.Gets)
	}
	modelsteptest.RequireRenderedStep(t, views)
}

func TestRenderCompactionUsesSummaryText(t *testing.T) {
	f := modelsteptest.NewRenderFixture(t, "lane")
	f.User("run-a", "old request")
	f.Assistant("run-a", "step-1", "c1")
	f.Tool("run-a", "step-1", "c1", models.ToolEntryCompleted)
	kept := f.User("run-a", "recent request")
	keptA := f.Assistant("run-a", "step-2", "c2")
	keptT := f.Tool("run-a", "step-2", "c2", models.ToolEntryCompleted)
	compaction := f.Compaction("run-a", kept.ID)
	after := f.Note("run-a", "Run context")
	afterA := f.Assistant("run-a", "step-3")

	path := f.Path()
	views, err := modelstep.ProjectSessionPath(context.Background(), path, modelstep.SessionRenderOptions{})
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	requireRenderOrder(t, views, compaction.ID, kept.ID, keptA.ID, keptT.ID, after.ID, afterA.ID)
	view := views[0].Compaction
	if view.Summary != modelstep.CompactionSummaryText(compaction.Compaction) || view.FirstKeptEntryID != kept.ID || view.TokensBefore != 42000 {
		t.Fatalf("compaction view = %+v", view)
	}
	if !strings.Contains(view.Summary, modelstep.CompactionSummaryPreamble) || !strings.Contains(view.Summary, "ac-old-1, ac-old-2") {
		t.Fatalf("summary text = %q", view.Summary)
	}
	// Kept tail numbers before the compaction, later entries after it.
	if views[1].Sequence != 1 || views[3].Sequence != 3 || views[0].Sequence != 4 || views[4].Sequence != 5 {
		t.Fatalf("sequences = %d %d %d %d", views[0].Sequence, views[1].Sequence, views[3].Sequence, views[4].Sequence)
	}
	if note := views[4].Note; note.Kind != string(models.NoteRunContext) || note.Text != "Run context" {
		t.Fatalf("note view = %+v", note)
	}
	modelsteptest.RequireRenderedStep(t, views)

	// Keep nothing: the compaction alone precedes the later entries.
	f2 := modelsteptest.NewRenderFixture(t, "lane2")
	f2.User("run-a", "old")
	c2 := f2.Compaction("run-a", "")
	u2 := f2.User("run-a", "next")
	views, err = modelstep.ProjectSessionPath(context.Background(), f2.Path(), modelstep.SessionRenderOptions{})
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	requireRenderOrder(t, views, c2.ID, u2.ID)
	if views[0].Sequence != 1 || views[1].Sequence != 2 || views[0].Compaction.FirstKeptEntryID != "" {
		t.Fatalf("keep-nothing views = %+v", views)
	}
	modelsteptest.RequireRenderedStep(t, views)
}

func TestRenderedViewPassesModelStepValidate(t *testing.T) {
	src := modelsteptest.NewRenderImageSources()
	// Parent chat session, forked into a child whose sequences restart at 1.
	parent := modelsteptest.NewRenderFixture(t, "chat")
	parent.User("run-a", "plan the launch", photoPart(""))
	parent.Assistant("run-a", "step-1", "c1")
	parent.Tool("run-a", "step-1", "c1", models.ToolEntryCompleted, models.ContentPart{Type: models.ContentPartText, Text: "found"}, shotPart(""))
	keep := parent.User("run-a", "now the guest list")
	parent.Assistant("run-a", "step-2", "c2", "c3")
	parent.User("run-b", "interleaved message")
	parent.Tool("run-a", "step-2", "c3", models.ToolEntryInterrupted)
	parent.Tool("run-a", "step-2", "c2", models.ToolEntryRunning)
	parentSession := parent.AgentSession(nil, 0)

	child := modelsteptest.NewRenderFixture(t, "task")
	child.Add(&models.SessionEntry{RunID: "run-t", Type: models.SessionEntryUser, User: &models.UserEntry{
		Origin: models.UserEntryIntervention, InterventionID: "i1", Author: models.SessionAuthor{Type: "user", ID: "u1", DisplayName: "Ana"},
		Content: []models.ContentPart{{Type: models.ContentPartText, Text: "also invite Kim"}},
	}})
	child.Compaction("run-t", keep.ID)
	child.Assistant("run-t", "step-t1", "t1")
	child.Tool("run-t", "step-t1", "t1", models.ToolEntryCompleted)
	child.Add(&models.SessionEntry{RunID: "run-t", Type: models.SessionEntryModelChange, ModelChange: &models.ModelChangeEntry{Provider: "openai-responses", Model: "gpt-test"}})
	childSession := child.AgentSession(parentSession, int64(len(parent.Entries)))

	path, err := models.BuildAgentSessionPath([]*models.AgentSession{parentSession, childSession}, [][]*models.SessionEntry{parent.Entries, child.Entries}, nil)
	if err != nil {
		t.Fatalf("buildAgentSessionPath: %v", err)
	}
	views, err := modelstep.ProjectSessionPath(context.Background(), path, src.Options(true))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	if views[0].Type != models.SessionEntryCompaction || views[0].Compaction.FirstKeptEntryID != keep.ID {
		t.Fatalf("first view = %+v", views[0])
	}
	intervention := modelsteptest.ViewByID(t, views, child.Entries[0].ID).User
	if intervention.Author == nil || intervention.Author.Kind != models.EntryAuthorPerson || intervention.Author.Label != "Ana" {
		t.Fatalf("intervention author = %+v", intervention.Author)
	}
	running := modelsteptest.ViewByID(t, views, parent.Entries[7].ID).Tool
	if running.CallID != "c2" || onlyText(t, running.Result.Content) != modelstep.RenderOpenToolText {
		t.Fatalf("running entry = %+v", running)
	}
	modelsteptest.RequireRenderedStep(t, views)

	// The same path without image input also validates.
	views, err = modelstep.ProjectSessionPath(context.Background(), path, src.Options(false))
	if err != nil {
		t.Fatalf("projectSessionPath without images: %v", err)
	}
	modelsteptest.RequireRenderedStep(t, views)

	// The uncompacted parent alone, images included.
	views, err = modelstep.ProjectSessionPath(context.Background(), parent.Path(), src.Options(true))
	if err != nil {
		t.Fatalf("projectSessionPath parent: %v", err)
	}
	if got := views[0].User.Content[1].DataBase64; got != base64.StdEncoding.EncodeToString(src.Photo) {
		t.Fatalf("parent image not resolved: %q", got)
	}
	modelsteptest.RequireRenderedStep(t, views)
}
