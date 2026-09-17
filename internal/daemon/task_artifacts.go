package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

const maxGeneratedFileBytes = 256 * 1024
const maxGeneratedFiles = 8

// Generated files are model-authored UTF-8 output, never host filesystem paths.
// Scope, identity, provenance, classification and integrity are host-owned.
type generatedFile struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Text      string `json:"text"`
}

func generatedFiles(output map[string]interface{}, status runtime.AgentRunStatus) ([]generatedFile, error) {
	raw, present := output["generatedFiles"]
	if !present {
		return nil, nil
	}
	if status != runtime.AgentRunStatusCompleted {
		return nil, errors.New("generated files require a completed task")
	}
	payload, err := json.Marshal(raw)
	if err != nil || len(payload) > 2<<20 {
		return nil, errors.New("generated files exceed the response limit")
	}
	var files []generatedFile
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&files); err != nil || files == nil || len(files) > maxGeneratedFiles {
		return nil, errors.New("generated files must be a list of at most eight text files")
	}
	total := 0
	names := map[string]bool{}
	for _, file := range files {
		if strings.TrimSpace(file.Name) != file.Name || file.Name == "" || file.Name == "." || file.Name == ".." || len(file.Name) > 180 || strings.ContainsAny(file.Name, "/\\<>:\"|?*") || strings.HasSuffix(file.Name, ".") || strings.ContainsFunc(file.Name, unicode.IsControl) || names[file.Name] {
			return nil, errors.New("generated files require unique portable filenames without directories")
		}
		names[file.Name] = true
		switch file.MediaType {
		case "text/plain", "text/markdown", "text/csv", "application/json":
		default:
			return nil, errors.New("generated files support plain text, Markdown, CSV or JSON")
		}
		total += len(file.Text)
		if !utf8.ValidString(file.Text) || strings.ContainsRune(file.Text, 0) || total > maxGeneratedFileBytes {
			return nil, errors.New("generated file text exceeds the 256 KiB total limit or contains invalid text")
		}
		if file.MediaType == "application/json" && !json.Valid([]byte(file.Text)) {
			return nil, errors.New("generated JSON file is invalid")
		}
	}
	return files, nil
}

func generatedOutputSchema() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"reply": map[string]interface{}{"type": "string"},
		"generatedFiles": map[string]interface{}{"type": "array", "maxItems": maxGeneratedFiles, "items": map[string]interface{}{"type": "object", "additionalProperties": false, "required": []string{"name", "mediaType", "text"}, "properties": map[string]interface{}{
			"name":      map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 180, "description": "A unique filename, without a directory or path."},
			"mediaType": map[string]interface{}{"type": "string", "enum": []string{"text/plain", "text/markdown", "text/csv", "application/json"}},
			"text":      map[string]interface{}{"type": "string", "maxLength": maxGeneratedFileBytes},
		}}},
	}, "additionalProperties": true}
}

type TaskArtifactPublisher struct {
	Scope   runtime.Scope
	Content runtime.ArtifactContentStore
	Catalog *runtime.ArtifactCatalog
}

func (p *TaskArtifactPublisher) PublishTurnOutput(ctx context.Context, run *runtime.AgentRun, turn *runtime.AgentTurn) (map[string]interface{}, error) {
	if run.Scope != p.Scope || turn.Scope != run.Scope || turn.RunID != run.ID || turn.Status != runtime.AgentTurnStatusCompleted {
		return nil, errors.New("artifact output does not match the accepted turn")
	}
	files, err := generatedFiles(turn.RunOutput, turn.NextRunStatus)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return turn.RunOutput, nil
	}
	if p.Content == nil || p.Catalog == nil {
		return nil, errors.New("task artifact storage is unavailable")
	}
	output := map[string]interface{}{}
	for key, value := range turn.RunOutput {
		if key != "generatedFiles" && key != "artifactRefs" {
			output[key] = value
		}
	}
	refs := make([]map[string]interface{}, 0, len(files))
	for index, file := range files {
		saved, err := p.Content.Put(ctx, runtime.ArtifactContentWrite{Scope: run.Scope, MediaType: file.MediaType, Reader: strings.NewReader(file.Text), SizeBytes: int64(len(file.Text))})
		if err != nil {
			return nil, errors.New("could not store generated file content")
		}
		identity, _ := json.Marshal([]interface{}{run.Scope, run.ID, turn.ID, index})
		id := fmt.Sprintf("turn-file-%x", sha256.Sum256(identity))
		owner := run.Owner
		result, err := p.Catalog.Register(ctx, runtime.RegisterArtifactRequest{Artifact: &runtime.Artifact{ID: id, Version: 1, Scope: run.Scope, Name: file.Name, MediaType: file.MediaType, ContentRef: saved.ContentRef, Digest: saved.Digest, SizeBytes: saved.SizeBytes, Classification: runtime.ArtifactClassificationInternal, Provenance: runtime.ArtifactProvenance{Producer: runtime.ActivityActor{Type: "agent", ID: run.AssignedAgentID}, Owner: &owner, RunID: run.ID, TurnID: turn.ID}, Evidence: []runtime.EvidenceLink{{Relation: runtime.EvidenceRelationOutputOf, TargetKind: runtime.EvidenceTargetTurn, TargetRef: turn.ID}}}})
		if err != nil {
			return nil, fmt.Errorf("could not register generated file: %w", err)
		}
		refs = append(refs, map[string]interface{}{"id": result.Artifact.ID, "version": result.Artifact.Version, "name": result.Artifact.Name})
	}
	output["artifactRefs"] = refs
	return output, nil
}
