package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// The model step fixtures that tests of other features share. They are
// copies of the fixtures in the model step tests and move to a test-support
// package once the model step code has its own package.

// RenderFixture builds the entries of one session in order.
type RenderFixture struct {
	t       *testing.T
	Session string
	Entries []*models.SessionEntry
}

func NewRenderFixture(t *testing.T, session string) *RenderFixture {
	return &RenderFixture{t: t, Session: session}
}

func (f *RenderFixture) Add(e *models.SessionEntry) *models.SessionEntry {
	e.Scope = testSessionScope
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
	a := modelStepTestAssistant(calls...)
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
	s := &models.AgentSession{ID: f.Session, Scope: testSessionScope, NextSequence: int64(len(f.Entries) + 1)}
	if parent != nil {
		s.ParentSessionID, s.ForkSequence, s.Depth = parent.ID, forkSequence, parent.Depth+1
	}
	return s
}

func (f *RenderFixture) Path() *models.AgentSessionPath {
	f.t.Helper()
	path, err := models.BuildAgentSessionPath([]*models.AgentSession{f.AgentSession(nil, 0)}, [][]*models.SessionEntry{f.Entries}, nil)
	if err != nil {
		f.t.Fatalf("buildAgentSessionPath: %v", err)
	}
	return path
}

func RenderIDs(views []models.SessionEntryView) []string {
	ids := make([]string, len(views))
	for i, v := range views {
		ids[i] = v.ID
	}
	return ids
}

func RequireRenderedStep(t *testing.T, views []models.SessionEntryView) {
	t.Helper()
	req := validModelStepRequest()
	req.Entries = views
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

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

// RenderImageSources holds one image artifact ("photo" v2) and one governed
// call with model media ("ac-shot").
type RenderImageSources struct {
	Artifacts *RenderArtifacts
	Content   RenderContent
	Calls     *RenderCalls
	Photo     []byte // artifact "photo" v2
	Shot      []byte // governed call "ac-shot"
}

func NewRenderImageSources() *RenderImageSources {
	photo := []byte("\x89PNG\r\n\x1a\nphoto-bytes")
	shot := []byte("\xff\xd8\xffscreenshot-bytes")
	return &RenderImageSources{
		Photo: photo, Shot: shot,
		Artifacts: &RenderArtifacts{Artifacts: map[string]*models.Artifact{
			"photo@2": {ID: "photo", Version: 2, Scope: testSessionScope, MediaType: "image/png", ContentRef: "ref-photo",
				SizeBytes: int64(len(photo)), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(photo))},
		}},
		Content: RenderContent{"ref-photo": photo},
		Calls: &RenderCalls{Calls: map[string]*models.ActionCall{
			"ac-shot": {ID: "ac-shot", Scope: testSessionScope, Status: models.ActionCallStatusSucceeded, Output: map[string]interface{}{
				"url": "https://example.test",
				"modelMedia": map[string]interface{}{
					"mediaType": "image/jpeg", "detail": "high", "contentBase64": base64.StdEncoding.EncodeToString(shot),
				},
			}},
		}},
	}
}

func (s *RenderImageSources) Options(imageInput bool) sessionRenderOptions {
	return sessionRenderOptions{ImageInput: imageInput, Artifacts: s.Artifacts, Content: s.Content, Actions: s.Calls}
}

// CapabilityCatalogFixture is a deferred catalog: eight Notion-like actions
// bound to one prompt each, four research prompts, and the built-in Skill.
func CapabilityCatalogFixture() CapabilityCatalog {
	catalog := CapabilityCatalog{Scope: models.Scope{Kind: "tenant", ID: "tenant-1"}, AgentID: "agent-1"}
	for i := 0; i < 4; i++ {
		catalog.SkillPrompts = append(catalog.SkillPrompts, models.HostedSkillPrompt{
			SkillID: fmt.Sprintf("research.%d", i), Version: "1.0.0", Reference: fmt.Sprintf("skill-research-%d@1", i),
			Name: fmt.Sprintf("Research %d", i), Description: "Research guidance", Instructions: fmt.Sprintf("Research instructions %d.", i),
		})
	}
	catalog.SkillPrompts = append(catalog.SkillPrompts, models.HostedSkillPrompt{
		SkillID: "notion", Version: "3", Reference: "skill-notion@3", Name: "Notion", Description: "Notion workspace", Instructions: "Use Notion carefully.",
	})
	for i := 0; i < 8; i++ {
		catalog.Actions = append(catalog.Actions, capability.ModelAction{
			Name: fmt.Sprintf("notion.op%d", i), SkillID: "notion", Version: "3", Action: fmt.Sprintf("op%d", i),
			Description: fmt.Sprintf("Notion operation %d", i), InputSchema: map[string]interface{}{"type": "object"},
		})
	}
	catalog.SkillPrompts = append(catalog.SkillPrompts, models.HostedSkillPrompt{
		SkillID: builtin.SkillID, Version: "1", Reference: "builtin-z@1", Name: "Built-in", Instructions: "Built-in instructions.",
	}, models.HostedSkillPrompt{
		SkillID: builtin.SkillID, Version: "1", Reference: "builtin-a@1", Name: "Built-in A", Instructions: "Built-in A instructions.",
	})
	catalog.Actions = append(catalog.Actions, capability.ModelAction{
		Name: "openseal.builtin.web_search", SkillID: builtin.SkillID, Version: "1", Action: "web_search", Description: "Search the web",
	}, capability.ModelAction{
		Name: "openseal.builtin.generate_image", SkillID: builtin.SkillID, Version: "1", Action: "generate_image", Description: "Make an image",
	})
	catalog.HostTools = HostToolSpecs(workspace.ChatAuthority("chat-1"))
	return catalog
}

// CapabilityNoteLines is the text of a capability listing, one line per item.
func CapabilityNoteLines(items []capabilityNoteItem) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString("\n" + item.Text)
	}
	return b.String()
}

// FakeHostToolHost answers ExecuteHostTool with scripted replies, one per call.
type FakeHostToolHost struct {
	mu       sync.Mutex
	Requests []models.HostToolRequest
	Replies  []func(context.Context, models.HostToolRequest) (models.HostToolResult, error)
}

func (h *FakeHostToolHost) ExecuteHostTool(ctx context.Context, request models.HostToolRequest) (models.HostToolResult, error) {
	h.mu.Lock()
	h.Requests = append(h.Requests, request)
	index := len(h.Requests) - 1
	h.mu.Unlock()
	if index >= len(h.Replies) {
		return models.HostToolResult{}, errors.New("unexpected host tool call")
	}
	return h.Replies[index](ctx, request)
}

func HostReplyJSON(value interface{}, isError bool) func(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
	return func(_ context.Context, request models.HostToolRequest) (models.HostToolResult, error) {
		encoded, _ := json.Marshal(value)
		return models.HostToolResult{APIVersion: models.HostToolAPIVersion, InvocationID: request.InvocationID,
			Content: []models.ContentPart{{Type: models.ContentPartJSON, Value: encoded}}, IsError: isError}, nil
	}
}

func HostReplyError(err error) func(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
	return func(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
		return models.HostToolResult{}, err
	}
}

// HostToolSpecFixture is the host tool operation offered in a chat workspace.
func HostToolSpecFixture(t *testing.T, operation string) HostToolSpec {
	t.Helper()
	spec, ok := LookupHostTool(HostToolSpecs(workspace.ChatAuthority("conv_1")), operation)
	if !ok {
		t.Fatalf("no host tool %s", operation)
	}
	return spec
}

// HostToolCallFixture is a leased host tool call of operation in chat conv_1.
func HostToolCallFixture(t *testing.T, operation string, arguments map[string]interface{}) HostToolCall {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "42"}
	expires := time.Now().Add(time.Hour)
	return HostToolCall{
		Run: &models.AgentRun{ID: "run_1", Scope: scope, Status: models.AgentRunStatusRunning, AssignedAgentID: "agent_1",
			LeaseOwner: "worker_1", LeaseExpiresAt: &expires},
		Step: &models.AgentTurn{ID: "step_1", Scope: scope, RunID: "run_1", Status: models.AgentTurnStatusRunning,
			LeaseOwner: "worker_1", LeaseExpiresAt: &expires},
		Binding: &agentrun.TurnRunnerBinding{DefinitionID: "def_1", DefinitionVersion: "3", ModelActions: []capability.ModelAction{
			{Name: "zeta.send", Description: "Send", Risk: capability.RiskLevel("high"), InputSchema: map[string]interface{}{"type": "object", "required": []interface{}{"to"}}},
			{Name: "alpha.lookup", Description: "Look up", Risk: capability.RiskLevel("low")},
		}},
		SessionID: "ses_1", ToolEntryID: "ent_1", ToolCallID: "toolu_1",
		Spec: HostToolSpecFixture(t, operation), Arguments: arguments,
		Workspace: *workspace.ChatAuthority("conv_1"),
	}
}

// FixedActionCalls is an ActionCallReader over a fixed set of calls.
type FixedActionCalls map[string]*models.ActionCall

func (f FixedActionCalls) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	call, ok := f[id]
	if !ok {
		return nil, models.ErrActionNotFound
	}
	cloned := *call
	cloned.Output = maps.Clone(call.Output)
	return &cloned, nil
}

// ProducerToolPath is a run-owned session path holding one terminal tool
// entry for toolCallID.
func ProducerToolPath(runID, toolCallID string, state models.ToolEntryState, actionCallID string) *models.AgentSessionPath {
	status := models.ToolResultSucceeded
	if state != models.ToolEntryCompleted {
		status = models.ToolResultFailed
	}
	return &models.AgentSessionPath{Entries: []*models.SessionEntry{{
		ID: "tool:step-1:" + toolCallID, RunID: runID, StepID: "step-1", Type: models.SessionEntryTool, State: state,
		ToolCallID: toolCallID, ActionCallID: actionCallID,
		Tool: &models.ToolEntry{Name: "tool", Result: &models.ToolResult{Status: status, IsError: status != models.ToolResultSucceeded}},
	}}}
}

func RequireValidDeterministicOutput(t *testing.T, out agentrun.StepOutput) {
	t.Helper()
	if err := validateStepOutput(models.StepProducerDeterministic, out); err != nil {
		t.Fatalf("invalid deterministic output %#v: %v", out, err)
	}
}
