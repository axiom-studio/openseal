package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testWorkflowHCL = `
workflow "test_workflow" {
  description = "A test workflow"
  
  node "http_request" "fetch_data" {
    method = "GET"
    url = "https://api.example.com/data"
    headers = {
      "Authorization" = "Bearer ${env.API_TOKEN}"
    }
  }
  
  node "ai" "process_data" {
    model = "gpt-4"
    prompt = "Process this data: ${fetch_data.body}"
    temperature = 0.7
  }
  
  edge "fetch_data" "process_data" {
    condition = "fetch_data.status == 200"
  }
}
`

func TestParseWorkflow(t *testing.T) {
	wf, err := ParseWorkflow([]byte(testWorkflowHCL), "test.hcl")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wf.Name != "test_workflow" {
		t.Fatalf("expected name 'test_workflow', got %q", wf.Name)
	}

	if wf.Description != "A test workflow" {
		t.Fatalf("expected description 'A test workflow', got %q", wf.Description)
	}

	if len(wf.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(wf.Nodes))
	}

	if len(wf.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(wf.Edges))
	}
}

func TestParseWorkflowNodes(t *testing.T) {
	wf, err := ParseWorkflow([]byte(testWorkflowHCL), "test.hcl")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	fetchNode := findNode(wf, "fetch_data")
	if fetchNode == nil {
		t.Fatal("node 'fetch_data' not found")
	}
	if fetchNode.Type != "http_request" {
		t.Fatalf("expected type 'http_request', got %q", fetchNode.Type)
	}
	if len(fetchNode.Config) < 2 {
		t.Fatalf("expected at least 2 config keys, got %d", len(fetchNode.Config))
	}

	aiNode := findNode(wf, "process_data")
	if aiNode == nil {
		t.Fatal("node 'process_data' not found")
	}
	if aiNode.Model() != "gpt-4" {
		t.Fatalf("expected model 'gpt-4', got %q", aiNode.Model())
	}
}

func TestParseWorkflowEdges(t *testing.T) {
	wf, err := ParseWorkflow([]byte(testWorkflowHCL), "test.hcl")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	edge := wf.Edges[0]
	if edge.From != "fetch_data" {
		t.Fatalf("expected from 'fetch_data', got %q", edge.From)
	}
	if edge.To != "process_data" {
		t.Fatalf("expected to 'process_data', got %q", edge.To)
	}
	if edge.Condition != "fetch_data.status == 200" {
		t.Fatalf("expected condition, got %q", edge.Condition)
	}
}

func TestParseWorkflowEmpty(t *testing.T) {
	_, err := ParseWorkflow([]byte(""), "empty.hcl")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParseWorkflowMultiple(t *testing.T) {
	hcl := `
workflow "a" {}
workflow "b" {}
`
	_, err := ParseWorkflow([]byte(hcl), "multi.hcl")
	if err == nil {
		t.Fatal("expected error for multiple workflow blocks")
	}
}

func TestLoadWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.hcl")
	if err := os.WriteFile(path, []byte(testWorkflowHCL), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	if wf.Name != "test_workflow" {
		t.Fatalf("expected name 'test_workflow', got %q", wf.Name)
	}
	if wf.SourceFile == "" {
		t.Fatal("expected SourceFile to be set")
	}
}

func TestLoadWorkflowDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.hcl"), []byte(testWorkflowHCL), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.hcl"), []byte(testWorkflowHCL), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("not a workflow"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	workflows, err := LoadWorkflowDir(dir)
	if err != nil {
		t.Fatalf("failed to load dir: %v", err)
	}
	if len(workflows) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(workflows))
	}
}

func TestContainsInterpolation(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"${env.API_TOKEN}", true},
		{"${fetch_data.body}", true},
		{"${trigger.input}", true},
		{"no interpolation here", false},
		{"$not_interp", false},
		{"${nested:${inner}}", true},
	}

	for _, tt := range tests {
		if got := ContainsInterpolation(tt.input); got != tt.expected {
			t.Errorf("ContainsInterpolation(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

func TestInterpolationVars(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"${env.API_TOKEN}", []string{"env.API_TOKEN"}},
		{"${fetch_data.body}", []string{"fetch_data.body"}},
		{"prefix ${a} and ${b} suffix", []string{"a", "b"}},
		{"no vars", nil},
	}

	for _, tt := range tests {
		got := InterpolationVars(tt.input)
		if len(got) != len(tt.expected) {
			t.Errorf("InterpolationVars(%q) = %v, want %v", tt.input, got, tt.expected)
			continue
		}
		for i, v := range got {
			if v != tt.expected[i] {
				t.Errorf("InterpolationVars(%q)[%d] = %q, want %q", tt.input, i, v, tt.expected[i])
			}
		}
	}
}

func TestDuplicateNodeID(t *testing.T) {
	hcl := `
workflow "dup" {
  node "http_request" "same" {
    url = "https://example.com"
  }
  node "ai" "same" {
    model = "gpt-4"
  }
}
`
	_, err := ParseWorkflow([]byte(hcl), "dup.hcl")
	if err == nil {
		t.Fatal("expected error for duplicate node ID")
	}
	if !strings.Contains(err.Error(), "duplicate node id") {
		t.Fatalf("expected duplicate node error, got: %v", err)
	}
}

func findNode(wf *Workflow, id string) *Node {
	for i := range wf.Nodes {
		if wf.Nodes[i].ID == id {
			return &wf.Nodes[i]
		}
	}
	return nil
}

func (n *Node) Model() string {
	if v, ok := n.Config["model"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
