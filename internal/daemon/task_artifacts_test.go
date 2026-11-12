package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/artifact"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

var localScope = models.Scope{Kind: "local", ID: "default"}

func filesField(t *testing.T, files ...interface{}) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func localWorkRun() *models.AgentRun {
	return &models.AgentRun{ID: "run-1", Scope: localScope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: "agent", ID: "analyst"}, AssignedAgentID: "analyst"}
}

func newTestPublisher(t *testing.T) (*TaskArtifactPublisher, models.ArtifactContentStore) {
	t.Helper()
	content, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &TaskArtifactPublisher{Scope: localScope, Content: content, Catalog: artifactcatalog.NewArtifactCatalog(kerneltest.NewStore(t))}, content
}

func twoFiles(t *testing.T) map[string]json.RawMessage {
	return map[string]json.RawMessage{generatedFilesProperty: filesField(t,
		generatedFile{Name: "report.md", MediaType: "text/markdown", Text: "# Report"},
		generatedFile{Name: "facts.json", MediaType: "application/json", Text: `{"count":2}`})}
}

func TestGeneratedFilesValidateBeforePublication(t *testing.T) {
	valid := generatedFile{Name: "report.md", MediaType: "text/markdown", Text: "# Report\nUncertainty is explicit."}
	for _, tc := range []struct {
		name  string
		files []interface{}
		ok    bool
	}{
		{"valid", []interface{}{valid}, true},
		{"empty file", []interface{}{generatedFile{Name: "empty.txt", MediaType: "text/plain"}}, true},
		{"duplicate", []interface{}{valid, valid}, false},
		{"directory", []interface{}{generatedFile{Name: "../report.md", MediaType: valid.MediaType}}, false},
		{"unsupported", []interface{}{generatedFile{Name: "app.html", MediaType: "text/html"}}, false},
		{"invalid json", []interface{}{generatedFile{Name: "data.json", MediaType: "application/json", Text: "not json"}}, false},
		{"size", []interface{}{generatedFile{Name: "large.txt", MediaType: "text/plain", Text: strings.Repeat("a", maxGeneratedFileBytes+1)}}, false},
		{"combined size", []interface{}{generatedFile{Name: "a.txt", MediaType: "text/plain", Text: strings.Repeat("a", maxGeneratedFileBytes)}, valid}, false},
		{"too many", make([]interface{}, 9), false},
		{"foreign metadata", []interface{}{map[string]interface{}{"name": "a.txt", "mediaType": "text/plain", "text": "hello", "scope": "other"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := filesField(t, tc.files...)
			if _, err := decodeGeneratedFiles(raw); (err == nil) != tc.ok {
				t.Fatalf("decode: %v", err)
			}
			publisher, _ := newTestPublisher(t)
			if err := publisher.ValidateFinalAnswer(localWorkRun(), map[string]json.RawMessage{generatedFilesProperty: raw}); (err == nil) != tc.ok {
				t.Fatalf("validate: %v", err)
			}
		})
	}
	if _, err := decodeGeneratedFiles(json.RawMessage(`null`)); err == nil {
		t.Fatal("null generatedFiles accepted")
	}
	if _, err := decodeGeneratedFiles(json.RawMessage(`{"name":"a.txt"}`)); err == nil {
		t.Fatal("object generatedFiles accepted")
	}
}

func TestFinalAnswerPropertiesOfferedOnlyForLocalAgentWork(t *testing.T) {
	publisher, _ := newTestPublisher(t)
	props := publisher.FinalAnswerProperties(localWorkRun())
	schema, ok := props[generatedFilesProperty].(map[string]interface{})
	if len(props) != 1 || !ok || schema["type"] != "array" || schema["maxItems"] != maxGeneratedFiles {
		t.Fatalf("properties: %+v", props)
	}
	if description, _ := schema["description"].(string); !strings.Contains(description, "not writes to the user's computer") {
		t.Fatalf("description: %q", description)
	}
	if _, err := json.Marshal(props); err != nil {
		t.Fatal(err)
	}
	foreign := localWorkRun()
	foreign.Scope.ID = "other"
	conversation := localWorkRun()
	conversation.Kind = models.RunKindConversation
	for _, run := range []*models.AgentRun{nil, foreign, conversation} {
		if props := publisher.FinalAnswerProperties(run); props != nil {
			t.Fatalf("offered on %+v: %+v", run, props)
		}
	}
	if (*TaskArtifactPublisher)(nil).FinalAnswerProperties(localWorkRun()) != nil {
		t.Fatal("nil publisher offered properties")
	}
}

func TestValidateFinalAnswerRejectsForgedArtifactRefsAndUnknownFields(t *testing.T) {
	publisher, _ := newTestPublisher(t)
	run := localWorkRun()
	if err := publisher.ValidateFinalAnswer(run, nil); err != nil {
		t.Fatalf("no fields: %v", err)
	}
	err := publisher.ValidateFinalAnswer(run, map[string]json.RawMessage{"artifactRefs": json.RawMessage(`[{"id":"x"}]`)})
	if err == nil || err.Error() != "Artifact references are assigned by the kernel, not the model." {
		t.Fatalf("forged refs: %v", err)
	}
	if err = publisher.ValidateFinalAnswer(run, map[string]json.RawMessage{"attachments": json.RawMessage(`[]`)}); err == nil || err.Error() != "unsupported final answer field" {
		t.Fatalf("unknown field: %v", err)
	}
	conversation := localWorkRun()
	conversation.Kind = models.RunKindConversation
	if err = publisher.ValidateFinalAnswer(conversation, twoFiles(t)); err == nil {
		t.Fatal("generatedFiles accepted on a run that was not offered them")
	}
}

func TestPublishFinalOutputRecordsStepProvenance(t *testing.T) {
	ctx := context.Background()
	publisher, content := newTestPublisher(t)
	run := localWorkRun()
	output, err := publisher.PublishFinalOutput(ctx, agentrun.FinalOutputPublication{Run: run, StepID: "step-2", ToolCallID: "call-1", Fields: twoFiles(t)})
	if err != nil {
		t.Fatal(err)
	}
	refs, ok := output["artifactRefs"].([]map[string]interface{})
	if len(output) != 1 || !ok || len(refs) != 2 || refs[0]["name"] != "report.md" || refs[1]["name"] != "facts.json" || refs[0]["version"] != int64(1) {
		t.Fatalf("output: %+v", output)
	}
	if !strings.HasPrefix(refs[0]["id"].(string), "step-file-") || refs[0]["id"] == refs[1]["id"] {
		t.Fatalf("ids: %+v", refs)
	}
	records, err := publisher.Catalog.List(ctx, models.ArtifactFilter{Scope: localScope, ProducerRunID: run.ID})
	if err != nil || len(records) != 2 {
		t.Fatalf("records: %d %v", len(records), err)
	}
	for _, record := range records {
		if record.Provenance.Producer.ID != "analyst" || record.Provenance.Producer.Type != "agent" || record.Provenance.TurnID != "step-2" || record.Provenance.RunID != run.ID || record.Provenance.Owner == nil || record.Provenance.Owner.ID != "analyst" || record.Classification != models.ArtifactClassificationInternal || record.Version != 1 {
			t.Fatalf("provenance: %+v", record)
		}
		if len(record.Evidence) != 1 || record.Evidence[0].Relation != models.EvidenceRelationOutputOf || record.Evidence[0].TargetKind != models.EvidenceTargetTurn || record.Evidence[0].TargetRef != "step-2" {
			t.Fatalf("evidence: %+v", record.Evidence)
		}
		reader, err := content.Open(ctx, localScope, record.ContentRef)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || int64(len(data)) != record.SizeBytes {
			t.Fatal("missing content", err)
		}
	}
	none, err := publisher.PublishFinalOutput(ctx, agentrun.FinalOutputPublication{Run: run, StepID: "step-2", ToolCallID: "call-1"})
	if err != nil || none != nil {
		t.Fatalf("no fields: %+v %v", none, err)
	}
}

func TestPublishFinalOutputIsIdempotentPerStepCall(t *testing.T) {
	ctx := context.Background()
	publisher, _ := newTestPublisher(t)
	pub := agentrun.FinalOutputPublication{Run: localWorkRun(), StepID: "step-2", ToolCallID: "call-1", Fields: twoFiles(t)}
	first, err := publisher.PublishFinalOutput(ctx, pub)
	if err != nil {
		t.Fatal(err)
	}
	second, err := publisher.PublishFinalOutput(ctx, pub)
	if err != nil {
		t.Fatal("replay:", err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("replay refs differ:\n%s\n%s", a, b)
	}
	records, err := publisher.Catalog.List(ctx, models.ArtifactFilter{Scope: localScope, ProducerRunID: pub.Run.ID})
	if err != nil || len(records) != 2 {
		t.Fatalf("records: %d %v", len(records), err)
	}
	pub.ToolCallID = "call-2"
	other, err := publisher.PublishFinalOutput(ctx, pub)
	if err != nil {
		t.Fatal(err)
	}
	if other["artifactRefs"].([]map[string]interface{})[0]["id"] == first["artifactRefs"].([]map[string]interface{})[0]["id"] {
		t.Fatal("a different tool call reused the artifact identity")
	}
}

type failSecondWrite struct {
	models.ArtifactContentStore
	writes int
}

func (s *failSecondWrite) Put(ctx context.Context, write models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	s.writes++
	if s.writes == 2 {
		return models.ArtifactStoredContent{}, errors.New("simulated disk interruption")
	}
	return s.ArtifactContentStore.Put(ctx, write)
}

func TestPublishFinalOutputResumesAfterPartialContentFailure(t *testing.T) {
	ctx := context.Background()
	publisher, content := newTestPublisher(t)
	publisher.Content = &failSecondWrite{ArtifactContentStore: content}
	pub := agentrun.FinalOutputPublication{Run: localWorkRun(), StepID: "step-2", ToolCallID: "call-1", Fields: twoFiles(t)}
	if _, err := publisher.PublishFinalOutput(ctx, pub); err == nil {
		t.Fatal("expected interrupted publication")
	}
	partial, err := publisher.Catalog.List(ctx, models.ArtifactFilter{Scope: localScope, ProducerRunID: pub.Run.ID})
	if err != nil || len(partial) != 1 {
		t.Fatalf("partial publication: %d %v", len(partial), err)
	}
	output, err := publisher.PublishFinalOutput(ctx, pub)
	if err != nil {
		t.Fatal("resume:", err)
	}
	if refs := output["artifactRefs"].([]map[string]interface{}); len(refs) != 2 || refs[0]["id"] != partial[0].ID {
		t.Fatalf("resumed refs: %+v", refs)
	}
	records, err := publisher.Catalog.List(ctx, models.ArtifactFilter{Scope: localScope, ProducerRunID: pub.Run.ID})
	if err != nil || len(records) != 2 {
		t.Fatalf("records after resume: %d %v", len(records), err)
	}
}

func TestPublishFinalOutputRejectsForeignScope(t *testing.T) {
	ctx := context.Background()
	publisher, _ := newTestPublisher(t)
	foreign := localWorkRun()
	foreign.Scope.ID = "other"
	for _, pub := range []agentrun.FinalOutputPublication{
		{Run: foreign, StepID: "step-2", ToolCallID: "call-1", Fields: twoFiles(t)},
		{Run: localWorkRun(), ToolCallID: "call-1", Fields: twoFiles(t)},
		{Run: localWorkRun(), StepID: "step-2", Fields: twoFiles(t)},
		{StepID: "step-2", ToolCallID: "call-1", Fields: twoFiles(t)},
	} {
		if _, err := publisher.PublishFinalOutput(ctx, pub); err == nil {
			t.Fatalf("publication accepted: %+v", pub)
		}
	}
	records, err := publisher.Catalog.List(ctx, models.ArtifactFilter{Scope: foreign.Scope})
	if err != nil || len(records) != 0 {
		t.Fatalf("foreign records: %d %v", len(records), err)
	}
}
