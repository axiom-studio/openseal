package modelsteptest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// RenderFixture builds the entries of one session in order.
type RenderFixture struct {
	t       *testing.T
	Session string
	Entries []*models.SessionEntry
}

// NewRenderFixture starts an empty session named session.
func NewRenderFixture(t *testing.T, session string) *RenderFixture {
	return &RenderFixture{t: t, Session: session}
}

func (f *RenderFixture) Add(e *models.SessionEntry) *models.SessionEntry {
	e.Scope = SessionScope
	e.SessionID = f.Session
	e.Sequence = int64(len(f.Entries) + 1)
	e.ID = f.Session + "-" + fmt.Sprint(e.Sequence)
	e.CreatedAt = time.Unix(1_800_000_000+e.Sequence, 0).UTC()
	e.UpdatedAt = e.CreatedAt
	f.Entries = append(f.Entries, e)
	return e
}

func (f *RenderFixture) User(run, text string, parts ...models.ContentPart) *models.SessionEntry {
	content := append([]models.ContentPart{{Type: models.ContentPartText, Text: text}}, parts...)
	return f.Add(&models.SessionEntry{RunID: run, Type: models.SessionEntryUser, User: &models.UserEntry{
		Origin: models.UserEntryChannelMessage, Author: models.SessionAuthor{Type: "user", ID: "u1", DisplayName: "Ana"}, Content: content,
	}})
}

func (f *RenderFixture) Assistant(run, step string, calls ...string) *models.SessionEntry {
	a := AssistantEntry(calls...)
	a.Limits = &models.AssistantModelLimits{ContextTokens: 200000, PromptTokens: 1200}
	return f.Add(&models.SessionEntry{RunID: run, StepID: step, Type: models.SessionEntryAssistant, Assistant: a})
}

func (f *RenderFixture) Tool(run, step, call string, state models.ToolEntryState, parts ...models.ContentPart) *models.SessionEntry {
	tool := &models.ToolEntry{Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), SideEffect: models.ToolSideEffectRead}
	if state.Terminal() {
		status := models.ToolResultSucceeded
		switch state {
		case models.ToolEntryError:
			status = models.ToolResultFailed
		case models.ToolEntryInterrupted:
			status = models.ToolResultInterrupted
		}
		if len(parts) == 0 {
			parts = []models.ContentPart{{Type: models.ContentPartText, Text: "result of " + call}}
		}
		tool.Result = &models.ToolResult{Status: status, IsError: status != models.ToolResultSucceeded, Content: parts}
	}
	return f.Add(&models.SessionEntry{RunID: run, StepID: step, Type: models.SessionEntryTool, State: state, ToolCallID: call, Tool: tool})
}

func (f *RenderFixture) Note(run, text string) *models.SessionEntry {
	return f.Add(&models.SessionEntry{RunID: run, Type: models.SessionEntryNote, Note: &models.NoteEntry{Kind: models.NoteRunContext, Text: text}})
}

func (f *RenderFixture) Compaction(run, firstKept string) *models.SessionEntry {
	return f.Add(&models.SessionEntry{RunID: run, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntry{
		Summary: "Ana asked for the guest list; the lookup found 12 guests.", FirstKeptEntryID: firstKept,
		TokensBefore: 42000, ActionCallIDs: []string{"ac-old-1", "ac-old-2"},
		Provider: "anthropic", Model: "claude-test", Trigger: models.CompactionTriggerThreshold,
	}})
}

func (f *RenderFixture) AgentSession(parent *models.AgentSession, forkSequence int64) *models.AgentSession {
	s := &models.AgentSession{ID: f.Session, Scope: SessionScope, NextSequence: int64(len(f.Entries) + 1)}
	if parent != nil {
		s.ParentSessionID, s.ForkSequence, s.Depth = parent.ID, forkSequence, parent.Depth+1
	}
	return s
}

// Path is the session path of the entries added so far.
func (f *RenderFixture) Path() *models.AgentSessionPath {
	f.t.Helper()
	path, err := models.BuildAgentSessionPath([]*models.AgentSession{f.AgentSession(nil, 0)}, [][]*models.SessionEntry{f.Entries}, nil)
	if err != nil {
		f.t.Fatalf("buildAgentSessionPath: %v", err)
	}
	return path
}

// RenderIDs are the ids of views in order.
func RenderIDs(views []models.SessionEntryView) []string {
	ids := make([]string, len(views))
	for i, v := range views {
		ids[i] = v.ID
	}
	return ids
}

// RequireRenderedStep fails t unless views form a valid model step request.
func RequireRenderedStep(t *testing.T, views []models.SessionEntryView) {
	t.Helper()
	req := ValidModelStepRequest()
	req.Entries = views
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

// ViewByID is the view with id; it fails t when there is none.
func ViewByID(t *testing.T, views []models.SessionEntryView, id string) models.SessionEntryView {
	t.Helper()
	for _, v := range views {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no view %s in %v", id, RenderIDs(views))
	return models.SessionEntryView{}
}

// RenderArtifacts serves artifact versions from memory and counts reads.
type RenderArtifacts struct {
	Artifacts map[string]*models.Artifact
	Gets      int
}

func (s *RenderArtifacts) CreateArtifactVersion(context.Context, *models.Artifact, int64) error {
	return nil
}
func (s *RenderArtifacts) ListArtifacts(context.Context, models.ArtifactFilter) ([]*models.Artifact, error) {
	return nil, nil
}
func (s *RenderArtifacts) GetArtifact(_ context.Context, scope models.Scope, id string, version int64) (*models.Artifact, error) {
	s.Gets++
	a, ok := s.Artifacts[fmt.Sprintf("%s@%d", id, version)]
	if !ok || a.Scope != scope {
		return nil, artifactcatalog.ErrArtifactNotFound
	}
	return a, nil
}

// RenderContent is artifact content by content ref.
type RenderContent map[string][]byte

func (c RenderContent) Put(context.Context, models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	return models.ArtifactStoredContent{}, nil
}
func (c RenderContent) Open(_ context.Context, _ models.Scope, ref string) (io.ReadCloser, error) {
	data, ok := c[ref]
	if !ok {
		return nil, artifactcatalog.ErrArtifactNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// RenderCalls serves action calls from memory and counts reads.
type RenderCalls struct {
	Calls map[string]*models.ActionCall
	Gets  int
}

func (s *RenderCalls) GetActionCall(_ context.Context, scope models.Scope, id string) (*models.ActionCall, error) {
	s.Gets++
	call, ok := s.Calls[id]
	if !ok || call.Scope != scope {
		return nil, models.ErrActionNotFound
	}
	return call, nil
}

// RenderChatFile is one file of RenderChatFiles.
type RenderChatFile struct {
	Data      []byte
	MediaType string
}

// RenderChatFiles serves the files of one Run's chat from memory and counts
// reads; another Run reads no files.
type RenderChatFiles struct {
	RunID string
	Files map[string]RenderChatFile
	Reads int
}

func (s *RenderChatFiles) ReadChatFile(_ context.Context, run *models.AgentRun, path string, limit int64) ([]byte, string, error) {
	s.Reads++
	file, ok := s.Files[path]
	if run == nil || run.ID != s.RunID || !ok || int64(len(file.Data)) > limit {
		return nil, "", nil
	}
	return file.Data, file.MediaType, nil
}

// RenderImageSources holds one image artifact ("photo" v2), one governed
// call with model media ("ac-shot") and one tool image kept in the chat of
// Run "run-a" (ChatShotPath).
type RenderImageSources struct {
	Artifacts *RenderArtifacts
	Content   RenderContent
	Calls     *RenderCalls
	ChatFiles *RenderChatFiles
	Photo     []byte // artifact "photo" v2
	Shot      []byte // governed call "ac-shot"
	ChatShot  []byte // chat file ChatShotPath
}

// ChatShotPath is the chat file NewRenderImageSources keeps.
const ChatShotPath = "tool-output/ac-read/screenshot.png"

// NewRenderImageSources serves the photo artifact and the ac-shot call.
func NewRenderImageSources() *RenderImageSources {
	photo := []byte("\x89PNG\r\n\x1a\nphoto-bytes")
	shot := []byte("\xff\xd8\xffscreenshot-bytes")
	chatShot := []byte("\x89PNG\r\n\x1a\nchat-file-bytes")
	return &RenderImageSources{
		Photo: photo, Shot: shot, ChatShot: chatShot,
		ChatFiles: &RenderChatFiles{RunID: "run-a", Files: map[string]RenderChatFile{
			ChatShotPath: {Data: chatShot, MediaType: "image/png"},
		}},
		Artifacts: &RenderArtifacts{Artifacts: map[string]*models.Artifact{
			"photo@2": {ID: "photo", Version: 2, Scope: SessionScope, MediaType: "image/png", ContentRef: "ref-photo",
				SizeBytes: int64(len(photo)), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(photo))},
		}},
		Content: RenderContent{"ref-photo": photo},
		Calls: &RenderCalls{Calls: map[string]*models.ActionCall{
			"ac-shot": {ID: "ac-shot", Scope: SessionScope, Status: models.ActionCallStatusSucceeded, Output: map[string]interface{}{
				"url": "https://example.test",
				"modelMedia": map[string]interface{}{
					"mediaType": "image/jpeg", "detail": "high", "contentBase64": base64.StdEncoding.EncodeToString(shot),
				},
			}},
		}},
	}
}

// Options are render options reading images from these sources.
func (s *RenderImageSources) Options(imageInput bool) modelstep.SessionRenderOptions {
	return modelstep.SessionRenderOptions{
		ImageInput: imageInput, Artifacts: s.Artifacts, Content: s.Content, Actions: s.Calls,
		ChatFiles: s.ChatFiles, Run: &models.AgentRun{ID: "run-a", Scope: SessionScope},
	}
}
