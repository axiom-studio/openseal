package suite

import (
	"testing"
)

func TestCodeNodesHavePythonLanguage(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	codeNodes := GetNodesByType(pw, "code")
	if len(codeNodes) == 0 {
		t.Fatal("Expected code nodes in Document Processor V2")
	}

	for _, node := range codeNodes {
		lang, ok := node.Config["language"].(string)
		if !ok {
			t.Errorf("Code node '%s' (name='%s'): missing 'language' config", node.Id, node.Name)
			continue
		}
		if lang != "python" {
			t.Errorf("Code node '%s' (name='%s'): expected language='python', got '%s'", node.Id, node.Name, lang)
		}
	}
}

func TestCodeNodesHaveCodeConfig(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	codeNodes := GetNodesByType(pw, "code")
	expectedCodeNodes := map[string]string{
		"docx-to-html":                           "mammoth",
		"msg-to-html":                             "extract-msg",
		"pdf-to-html-xfa-format-support":         "pymupdf",
		"html-doc":                                "",
		"html-to-md":                              "markdownify",
		"code-output-json-partial":                "",
	}

	for _, node := range codeNodes {
		code, ok := node.Config["code"].(string)
		if !ok {
			t.Errorf("Code node '%s' (name='%s'): missing 'code' config", node.Id, node.Name)
			continue
		}
		if len(code) == 0 {
			t.Errorf("Code node '%s' (name='%s'): empty code string", node.Id, node.Name)
		}

		expectedPkg, hasExpectedPkg := expectedCodeNodes[node.Name]
		if hasExpectedPkg && expectedPkg != "" {
			reqs, ok := node.Config["requirements"].([]interface{})
			if !ok {
				t.Errorf("Code node '%s' (name='%s'): missing 'requirements' config", node.Id, node.Name)
				continue
			}

			found := false
			for _, r := range reqs {
				if s, ok := r.(string); ok && s == expectedPkg {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Code node '%s' (name='%s'): expected requirement '%s' not found", node.Id, node.Name, expectedPkg)
			}
		}
	}
}

func TestAINodesHaveModelConfig(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	aiNodes := GetNodesByType(pw, "ai")
	if len(aiNodes) == 0 {
		t.Fatal("Expected AI nodes in Document Processor V2")
	}

	expectedProviders := map[string]string{
		"google-gemini":   "gemini",
		"Validator":       "openai",
		"Extractor":       "openai",
		"Reconciliation":  "openai",
		"LLM-gateway":     "openai-compatible",
	}

	for _, node := range aiNodes {
		provider, ok := node.Config["provider"].(string)
		if !ok {
			t.Errorf("AI node '%s' (name='%s'): missing 'provider' config", node.Id, node.Name)
			continue
		}

		if expectedProvider, exists := expectedProviders[node.Name]; exists {
			if provider != expectedProvider {
				t.Errorf("AI node '%s' (name='%s'): expected provider='%s', got '%s'", node.Id, node.Name, expectedProvider, provider)
			}
		}

		model, ok := node.Config["model"].(string)
		if !ok || model == "" {
			t.Errorf("AI node '%s' (name='%s'): missing 'model' config", node.Id, node.Name)
		}

		prompt, ok := node.Config["prompt"].(string)
		if !ok || prompt == "" {
			t.Errorf("AI node '%s' (name='%s'): missing 'prompt' config", node.Id, node.Name)
		}

		systemPrompt, _ := node.Config["systemPrompt"].(string)
		_ = systemPrompt
	}
}

func TestPGVectorNodeConfig(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	pgvectorNodes := GetNodesByType(pw, "tool_pgvector")
	if len(pgvectorNodes) != 1 {
		t.Fatalf("Expected 1 tool_pgvector node, got %d", len(pgvectorNodes))
	}

	node := pgvectorNodes[0]
	config := node.Config

	requiredFields := []string{"connectionString", "embeddingModel", "embeddingProvider", "table", "topK", "searchMode"}
	for _, field := range requiredFields {
		if _, exists := config[field]; !exists {
			t.Errorf("PGVector node missing required config field '%s'", field)
		}
	}

	searchMode, ok := config["searchMode"].(string)
	if !ok {
		t.Error("PGVector node 'searchMode' is not a string")
	} else if searchMode != "advanced" {
		t.Errorf("PGVector node: expected searchMode='advanced', got '%s'", searchMode)
	}

	topK, ok := config["topK"]
	if !ok {
		t.Error("PGVector node missing 'topK' config")
	} else {
		switch v := topK.(type) {
		case int:
			if v != 5 {
				t.Errorf("PGVector node: expected topK=5, got %d", v)
			}
		case float64:
			if int(v) != 5 {
				t.Errorf("PGVector node: expected topK=5, got %d", int(v))
			}
		default:
			t.Errorf("PGVector node 'topK' is not a number, got %T", topK)
		}
	}

	toolName, ok := config["toolName"].(string)
	if !ok || toolName == "" {
		t.Error("PGVector node missing 'toolName' config")
	}

	vectorColumns, ok := config["vectorColumns"].(string)
	if !ok || vectorColumns == "" {
		t.Error("PGVector node missing 'vectorColumns' config - required for weighted search")
	}
}

func TestHTTPNodeConfig(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	httpNodes := GetNodesByType(pw, "http")
	if len(httpNodes) < 2 {
		t.Fatalf("Expected at least 2 HTTP nodes, got %d", len(httpNodes))
	}

	httpNodeNames := make(map[string]bool)
	for _, node := range httpNodes {
		httpNodeNames[node.Name] = true

		hasURL := false
		if _, ok := node.Config["url"]; ok {
			hasURL = true
		} else if body, ok := node.Config["body"]; ok {
			if bodyStr, ok := body.(string); ok && bodyStr != "" {
				hasURL = true
			} else if bodyMap, ok := body.(map[string]interface{}); ok {
				if _, ok := bodyMap["url"]; ok {
					hasURL = true
				}
			}
		}
		if !hasURL {
			t.Errorf("HTTP node '%s': no 'url' config found (top-level, in body string, or in body map)", node.Name)
		}
	}

	if !httpNodeNames["http-upload-document"] {
		t.Error("Expected HTTP node named 'http-upload-document'")
	}
	if !httpNodeNames["http-upload-metadata"] {
		t.Error("Expected HTTP node named 'http-upload-metadata'")
	}
}

func TestSlackNodeConfig(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	slackNodes := GetNodesByType(pw, "slack")
	if len(slackNodes) != 1 {
		t.Fatalf("Expected 1 slack node, got %d", len(slackNodes))
	}

	node := slackNodes[0]

	if node.Name != "Slack-1" {
		t.Errorf("Slack node: expected name='Slack-1', got '%s'", node.Name)
	}

	webhookUrl, ok := node.Config["webhookUrl"].(string)
	if !ok || webhookUrl == "" {
		t.Error("Slack node missing 'webhookUrl' config")
	}

	channel, ok := node.Config["channel"].(string)
	if !ok || channel == "" {
		t.Error("Slack node missing 'channel' config")
	}
}

func TestSplitJoinNodesConfig(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	splitNodes := GetNodesByType(pw, "split")
	if len(splitNodes) != 2 {
		t.Fatalf("Expected 2 split nodes, got %d", len(splitNodes))
	}

	splitNames := make(map[string]bool)
	for _, node := range splitNodes {
		splitNames[node.Name] = true
	}
	if !splitNames["multi-llm-splitter"] {
		t.Error("Expected split node named 'multi-llm-splitter'")
	}
	if !splitNames["Split-2"] {
		t.Error("Expected split node named 'Split-2'")
	}

	for _, node := range splitNodes {
		if node.Name == "multi-llm-splitter" {
			execMode, _ := node.Config["executionMode"].(string)
			if execMode != "continue" {
				t.Errorf("multi-llm-splitter: expected executionMode='continue', got '%s'", execMode)
			}
		}
	}

	joinNodes := GetNodesByType(pw, "join")
	if len(joinNodes) != 1 {
		t.Fatalf("Expected 1 join node, got %d", len(joinNodes))
	}

	if joinNodes[0].Name != "Join-1" {
		t.Errorf("Join node: expected name='Join-1', got '%s'", joinNodes[0].Name)
	}

	joinMode, _ := joinNodes[0].Config["mode"].(string)
	if joinMode != "stream" {
		t.Errorf("Join node: expected mode='stream', got '%s'", joinMode)
	}

	splitConns := GetConnectionsFrom(pw, splitNodes[0].Id)
	if len(splitConns) == 0 {
		t.Errorf("Split node '%s' has no outgoing connections", splitNodes[0].Id)
	}

	joinConns := GetConnectionsTo(pw, joinNodes[0].Id)
	if len(joinConns) < 2 {
		t.Errorf("Join node '%s' should have at least 2 incoming connections, got %d", joinNodes[0].Id, len(joinConns))
	}
}

func TestManualNodeIsEntryPoint(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	manualNodes := GetNodesByType(pw, "manual")
	if len(manualNodes) != 1 {
		t.Fatalf("Expected 1 manual node, got %d", len(manualNodes))
	}

	manualNode := manualNodes[0]
	if manualNode.Name != "File-Upload" {
		t.Errorf("Manual node: expected name='File-Upload', got '%s'", manualNode.Name)
	}

	incomingConnCount := len(GetConnectionsTo(pw, manualNode.Id))
	if incomingConnCount != 0 {
		t.Errorf("Manual entry point should have 0 incoming connections, got %d", incomingConnCount)
	}

	if len(pw.Graph.StartNodes) != 1 {
		t.Errorf("Expected 1 graph start node, got %d", len(pw.Graph.StartNodes))
	}
	if len(pw.Graph.StartNodes) > 0 && pw.Graph.StartNodes[0] != manualNode.Id {
		t.Errorf("Graph start node should be manual node '%s', got '%s'", manualNode.Id, pw.Graph.StartNodes[0])
	}
}

func TestWorkflowDAGNoCycles(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	_, err = pw.Graph.TopologicalSort()
	if err != nil {
		t.Errorf("Workflow graph has cycles or is invalid: %v", err)
	}
}

func TestSwitchNodeConfigStructure(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	switchNodes := GetNodesByType(pw, "switch")
	if len(switchNodes) != 1 {
		t.Fatalf("Expected 1 switch node, got %d", len(switchNodes))
	}

	switchNode := switchNodes[0]

	if switchNode.Name != "Switch-2" {
		t.Errorf("Switch node: expected name='Switch-2', got '%s'", switchNode.Name)
	}

	config := switchNode.Config

	if _, ok := config["expression"]; !ok {
		t.Error("Switch node missing 'expression' config")
	}

	if _, ok := config["cases"]; !ok {
		t.Error("Switch node missing 'cases' config")
	}

	if _, ok := config["default"]; !ok {
		t.Error("Switch node missing 'default' config")
	}
}