package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	tea "github.com/charmbracelet/bubbletea"
)

type fakeKernelClient struct {
	document       kernelapi.CapabilityDocument
	runs           []*runtime.AgentRun
	createErrors   []error
	createKeys     []string
	createRequests []kernelapi.CreateAgentRunRequest
	commands       []kernelapi.AgentRunCommandRequest
	artifacts      []*runtime.Artifact
	downloadBody   string
	downloadCalls  int
}

func (f *fakeKernelClient) Capabilities(context.Context) (kernelapi.CapabilityDocument, error) {
	return f.document, nil
}

func (f *fakeKernelClient) CreateAgentRun(_ context.Context, request kernelapi.CreateAgentRunRequest, key string) (*runtime.AgentRunCommandResult, error) {
	f.createKeys = append(f.createKeys, key)
	f.createRequests = append(f.createRequests, request)
	if len(f.createErrors) > 0 {
		err := f.createErrors[0]
		f.createErrors = f.createErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	run := testRun("created", runtime.AgentRunStatusQueued, 1)
	f.runs = append([]*runtime.AgentRun{run}, f.runs...)
	return &runtime.AgentRunCommandResult{Run: run}, nil
}

func (f *fakeKernelClient) ListAgentRuns(context.Context, runtime.AgentRunFilter) ([]*runtime.AgentRun, error) {
	return f.runs, nil
}

func (f *fakeKernelClient) GetAgentRun(context.Context, runtime.Scope, string) (*runtime.AgentRun, error) {
	if len(f.runs) == 0 {
		return nil, runtime.ErrRunNotFound
	}
	return f.runs[0], nil
}

func (f *fakeKernelClient) CommandAgentRun(_ context.Context, _ runtime.Scope, id string, request kernelapi.AgentRunCommandRequest) (*runtime.AgentRunCommandResult, error) {
	f.commands = append(f.commands, request)
	status := runtime.AgentRunStatusPaused
	if request.Kind == runtime.AgentRunCommandResume {
		status = runtime.AgentRunStatusQueued
	} else if request.Kind == runtime.AgentRunCommandCancel {
		status = runtime.AgentRunStatusCanceled
	}
	run := testRun(id, status, request.ExpectedRevision+1)
	return &runtime.AgentRunCommandResult{Run: run}, nil
}

func (f *fakeKernelClient) RegisterArtifact(context.Context, runtime.RegisterArtifactRequest) (*runtime.ArtifactRegistrationResult, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) GetArtifact(_ context.Context, _ runtime.Scope, id string, version int64) (*runtime.Artifact, error) {
	for _, artifact := range f.artifacts {
		if artifact.ID == id && (version == 0 || artifact.Version == version) {
			return artifact, nil
		}
	}
	return nil, runtime.ErrArtifactNotFound
}

func (f *fakeKernelClient) ListArtifacts(context.Context, runtime.ArtifactFilter) ([]*runtime.Artifact, error) {
	return f.artifacts, nil
}

func (f *fakeKernelClient) UploadArtifactContent(context.Context, runtime.Scope, string, string, int64, io.Reader) (*runtime.ArtifactStoredContent, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) DownloadArtifactContent(context.Context, runtime.Scope, string, int64) (*client.ArtifactDownload, error) {
	f.downloadCalls++
	return &client.ArtifactDownload{
		Body:   io.NopCloser(strings.NewReader(f.downloadBody)),
		Digest: testDigest(f.downloadBody), SizeBytes: int64(len(f.downloadBody)),
	}, nil
}

func (f *fakeKernelClient) ResolveArtifactContent(context.Context, runtime.Scope, string, int64, kernelapi.ResolveArtifactContentRequest) (*runtime.ArtifactContentResolution, error) {
	return nil, errors.New("not implemented by test client")
}

func TestModelDiscoversCapabilitiesBeforeRenderingActions(t *testing.T) {
	limited := kernelapi.Capability{
		ID: kernelapi.AgentRunsCapabilityID, Version: kernelapi.AgentRunsCapabilityVersion,
		Available: true, Operations: []string{kernelapi.OperationCreate, kernelapi.OperationList},
	}
	fake := &fakeKernelClient{
		document: kernelapi.CapabilityDocument{Version: kernelapi.Version, Capabilities: []kernelapi.Capability{limited}},
		runs:     []*runtime.AgentRun{testRun("run-1", runtime.AgentRunStatusQueued, 1)},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	view := model.View()
	if !strings.Contains(view, "Current work") || !strings.Contains(view, "First durable outcome") {
		t.Fatalf("work view missing canonical run:\n%s", view)
	}
	for _, action := range []string{"p pause", "g guide", "x stop"} {
		if strings.Contains(view, action) {
			t.Fatalf("unadvertised action %q was rendered:\n%s", action, view)
		}
	}
}

func TestCreateRetryPreservesIdempotencyAndPrompt(t *testing.T) {
	fake := &fakeKernelClient{
		document:     kernelapi.Capabilities(),
		createErrors: []error{errors.New("temporary disconnect"), nil},
	}
	model := newTestModel(t, fake)
	model.ready = true
	model.runCapability = kernelapi.AgentRunsCapability()
	model.editor.SetValue("Monitor Kubernetes events and remediate safely")

	applyCommand(t, model, model.submitRun())
	if model.editor.Value() == "" || model.pendingKey == "" {
		t.Fatal("failed creation did not preserve prompt and idempotency key")
	}
	applyCommand(t, model, model.submitRun())
	if len(fake.createKeys) != 2 || fake.createKeys[0] == "" || fake.createKeys[0] != fake.createKeys[1] {
		t.Fatalf("idempotency keys = %#v", fake.createKeys)
	}
	if model.editor.Value() != "" || model.pendingKey != "" {
		t.Fatal("successful creation did not clear composer state")
	}
	if fake.createRequests[0].AssignedAgentID != "operator" || fake.createRequests[0].Source != runtime.RunSourceManual {
		t.Fatalf("create request = %#v", fake.createRequests[0])
	}
}

func TestLifecycleCommandUsesSelectedRevisionAndAdvertisedOperation(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.Capabilities()}
	model := newTestModel(t, fake)
	model.ready = true
	model.runCapability = kernelapi.AgentRunsCapability()
	model.runs = []*runtime.AgentRun{testRun("run-7", runtime.AgentRunStatusRunning, 7)}
	model.selectedID = "run-7"

	applyCommand(t, model, model.pauseOrResume())
	if len(fake.commands) != 1 {
		t.Fatalf("commands = %#v", fake.commands)
	}
	command := fake.commands[0]
	if command.Kind != runtime.AgentRunCommandPause || command.ExpectedRevision != 7 {
		t.Fatalf("command = %#v", command)
	}

	model.runCapability.Operations = []string{kernelapi.OperationList}
	if command := model.commandSelected(runtime.AgentRunCommandCancel, ""); command != nil {
		t.Fatal("TUI dispatched an unadvertised cancel operation")
	}
}

func TestContractMismatchFailsClosed(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.CapabilityDocument{Version: "99"}}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.ready || !strings.Contains(model.View(), "Capability unavailable") {
		t.Fatalf("mismatch did not fail closed:\n%s", model.View())
	}
}

func TestArtifactEvidenceAndVerifiedDownloadAreCapabilityGated(t *testing.T) {
	body := "cited market research"
	confidence := 0.91
	artifact := &runtime.Artifact{
		ID: "research-report", Version: 2, Name: "OpenClaw research.pdf", Type: "report",
		MediaType: "application/pdf", Digest: testDigest(body), SizeBytes: int64(len(body)),
		Classification: runtime.ArtifactClassificationInternal, CreatedAt: time.Now(),
		Provenance: runtime.ArtifactProvenance{
			Producer: runtime.ActivityActor{Type: "agent", ID: "researcher"}, RunID: "run-research",
		},
		Evidence: []runtime.EvidenceLink{{
			Relation: runtime.EvidenceRelationCites, TargetKind: runtime.EvidenceTargetExternalSource,
			TargetRef: "https://example.com/source", Summary: "Primary source", Confidence: &confidence,
		}},
	}
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(
			kernelapi.AgentRunsCapability(), kernelapi.ArtifactCapability(kernelapi.OperationDownload),
		),
		artifacts: []*runtime.Artifact{artifact}, downloadBody: body,
	}
	model := newTestModel(t, fake)
	model.config.DownloadDir = t.TempDir()
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionArtifacts
	model.focusPanelList()

	view := model.View()
	for _, expected := range []string{"Artifacts & evidence", "OpenClaw research.pdf", "1 evidence link(s)", "d download"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("artifact view missing %q:\n%s", expected, view)
		}
	}
	model.artifactExpanded = true
	if view = model.View(); !strings.Contains(view, "Primary source") || !strings.Contains(view, "91%") {
		t.Fatalf("expanded evidence missing provenance details:\n%s", view)
	}

	applyCommand(t, model, model.downloadSelectedArtifact())
	entries, err := os.ReadDir(model.config.DownloadDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("download entries = %v, err = %v", entries, err)
	}
	downloaded, err := os.ReadFile(model.config.DownloadDir + "/" + entries[0].Name())
	if err != nil || string(downloaded) != body || fake.downloadCalls != 1 {
		t.Fatalf("downloaded = %q, calls = %d, err = %v", downloaded, fake.downloadCalls, err)
	}

	model.artifactCapability = kernelapi.ArtifactCapability()
	if command := model.downloadSelectedArtifact(); command != nil {
		t.Fatal("TUI dispatched an unadvertised artifact download")
	}
	if strings.Contains(model.View(), "d download") {
		t.Fatal("TUI rendered an unadvertised artifact download")
	}
}

func newTestModel(t *testing.T, fake *fakeKernelClient) *Model {
	t.Helper()
	config := DefaultConfig()
	config.PollInterval = -1
	model, err := NewModel(context.Background(), fake, config)
	if err != nil {
		t.Fatal(err)
	}
	model.width, model.height = 120, 36
	return model
}

func applyCommand(t *testing.T, model *Model, command tea.Cmd) {
	t.Helper()
	if command == nil {
		t.Fatal("expected command")
	}
	applyCommandRecursive(t, model, command)
}

func applyCommandRecursive(t *testing.T, model *Model, command tea.Cmd) {
	t.Helper()
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		for _, child := range batch {
			if child != nil {
				applyCommandRecursive(t, model, child)
			}
		}
		return
	}
	updated, followup := model.Update(message)
	if updated != model {
		t.Fatal("model identity changed unexpectedly")
	}
	if followup != nil {
		applyCommandRecursive(t, model, followup)
	}
}

func testRun(id string, status runtime.AgentRunStatus, revision int64) *runtime.AgentRun {
	return &runtime.AgentRun{
		ID: id, Scope: runtime.Scope{Kind: "local", ID: "default"},
		Owner:           runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"},
		AssignedAgentID: "operator", Goal: "First durable outcome", Source: runtime.RunSourceManual,
		Status: status, Revision: revision, UpdatedAt: time.Now(),
	}
}

func testDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
