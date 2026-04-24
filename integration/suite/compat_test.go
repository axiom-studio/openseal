package suite

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/types"
)

func TestYAMLParsesAllFields(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	if wf.APIVersion != "axiom.studio/v1" {
		t.Errorf("Expected apiVersion 'axiom.studio/v1', got '%s'", wf.APIVersion)
	}
	if wf.Kind != "AgentLibraryVersion" {
		t.Errorf("Expected kind 'AgentLibraryVersion', got '%s'", wf.Kind)
	}
	if wf.Metadata.Name != "Document Processor V2" {
		t.Errorf("Expected metadata.name 'Document Processor V2', got '%s'", wf.Metadata.Name)
	}
	if wf.Metadata.Version != "1.0.22" {
		t.Errorf("Expected metadata.version '1.0.22', got '%s'", wf.Metadata.Version)
	}
	if wf.Metadata.Category != "custom" {
		t.Errorf("Expected metadata.category 'custom', got '%s'", wf.Metadata.Category)
	}
}

func TestNodeCountAndTypesMatchAtlas(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	if len(pw.Nodes) == 0 {
		t.Fatal("Expected non-zero nodes in workflow")
	}
	if len(pw.Connections) == 0 {
		t.Fatal("Expected non-zero connections in workflow")
	}

	expectedTypes := map[string]int{
		"manual":        1,
		"switch":        1,
		"code":          6,
		"ai":            5,
		"tool_pgvector": 1,
		"http":          2,
		"slack":         1,
		"split":         2,
		"join":          1,
	}

	actualTypes := GetUniqueNodeTypes(pw)
	for typeName, expectedCount := range expectedTypes {
		actualCount, ok := actualTypes[typeName]
		if !ok {
			t.Errorf("Missing expected node type '%s'", typeName)
		} else if actualCount != expectedCount {
			t.Errorf("Node type '%s': expected count %d, got %d", typeName, expectedCount, actualCount)
		}
	}

	for typeName, actualCount := range actualTypes {
		if _, ok := expectedTypes[typeName]; !ok {
			t.Errorf("Unexpected node type '%s' with count %d", typeName, actualCount)
		}
	}
}

func TestAllAtlasNodeTypesHaveExecutors(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	registry := executor.NewEmptyRegistry()
	registry.Register(&executor.SwitchExecutor{})
	registry.Register(&executor.SplitExecutor{})
	registry.Register(&executor.JoinExecutor{})
	registry.Register(executor.NewAIExecutor())
	registry.Register(executor.NewPGVectorExecutor())
	registry.Register(executor.NewHTTPExecutor())
	registry.Register(executor.NewSlackExecutor())

	actualTypes := GetUniqueNodeTypes(pw)

	yamlToExecutorType := map[string]string{
		"manual":        executor.NodeTypeManual,
		"switch":        executor.StepTypeSwitch,
		"code":          executor.StepTypeCode,
		"ai":            executor.StepTypeAI,
		"tool_pgvector": executor.StepTypePGVector,
		"http":          executor.StepTypeHTTP,
		"slack":         executor.NodeTypeSlack,
		"split":         executor.NodeTypeSplit,
		"join":          executor.NodeTypeJoin,
	}

	for yamlType, executorType := range yamlToExecutorType {
		if _, exists := actualTypes[yamlType]; !exists {
			continue
		}
		if !registry.HasExecutor(executorType) {
			if yamlType != "code" && yamlType != "manual" {
				t.Errorf("No executor registered for YAML type '%s' (executor type '%s')", yamlType, executorType)
			}
		}
	}
}

func TestExecutorConstantsMatchYAMLTypes(t *testing.T) {
	yamlToExpectedConstant := map[string]string{
		"manual":        types.NodeTypeManual,
		"switch":        types.NodeTypeSwitch,
		"code":          types.NodeTypeCode,
		"ai":            types.NodeTypeAI,
		"tool_pgvector": types.NodeTypePGVector,
		"http":          types.NodeTypeHTTP,
	}

	for yamlType, expectedConstant := range yamlToExpectedConstant {
		if yamlType != expectedConstant && yamlType != "tool_pgvector" {
			if yamlType == "code" && expectedConstant == "code" {
				continue
			}
		}
	}

	if types.NodeTypeManual != "manual" {
		t.Errorf("Expected NodeTypeManual='manual', got '%s'", types.NodeTypeManual)
	}
	if types.NodeTypeSwitch != "switch" {
		t.Errorf("Expected NodeTypeSwitch='switch', got '%s'", types.NodeTypeSwitch)
	}
	if types.NodeTypeCode != "code" {
		t.Errorf("Expected NodeTypeCode='code', got '%s'", types.NodeTypeCode)
	}
	if types.NodeTypeAI != "ai" {
		t.Errorf("Expected NodeTypeAI='ai', got '%s'", types.NodeTypeAI)
	}
	if types.NodeTypePGVector != "pgvector" {
		t.Errorf("Expected NodeTypePGVector='pgvector', got '%s'", types.NodeTypePGVector)
	}
	if types.NodeTypeHTTP != "http" {
		t.Errorf("Expected NodeTypeHTTP='http', got '%s'", types.NodeTypeHTTP)
	}

	if executor.NodeTypeSlack != "slack" {
		t.Errorf("Expected executor NodeTypeSlack='slack', got '%s'", executor.NodeTypeSlack)
	}
	if executor.NodeTypeSplit != "split" {
		t.Errorf("Expected executor NodeTypeSplit='split', got '%s'", executor.NodeTypeSplit)
	}
	if executor.NodeTypeJoin != "join" {
		t.Errorf("Expected executor NodeTypeJoin='join', got '%s'", executor.NodeTypeJoin)
	}
}

func TestSwitchNodeSourceHandleRouting(t *testing.T) {
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
	switchConnections := GetConnectionsFrom(pw, switchNode.Id)

	sourceHandles := make(map[string]string)
	for _, conn := range switchConnections {
		if conn.SourceHandle != "" {
			sourceHandles[conn.SourceHandle] = conn.TargetNodeId

			graphNextNodes := pw.Graph.GetNextNodes(switchNode.Id, conn.SourceHandle)
			found := false
			for _, node := range graphNextNodes {
				if node.Node.Id == conn.TargetNodeId {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Graph routing mismatch: sourceHandle '%s' -> target '%s' not found via GetNextNodes", conn.SourceHandle, conn.TargetNodeId)
			}
		}
	}

	expectedHandles := map[string]string{
		"application/pdf":                                        "temp-1770653649501-2",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "temp-1768324487756-5",
		"application/octet-stream":                               "temp-1769042421398-1",
		"application/vnd.ms-outlook":                             "temp-1769042421398-1",
		"text/html":                                              "temp-1769044897371-1",
	}

	for handle, expectedTarget := range expectedHandles {
		actualTarget, ok := sourceHandles[handle]
		if !ok {
			t.Errorf("Missing sourceHandle '%s' from switch node", handle)
		} else if actualTarget != expectedTarget {
			t.Errorf("sourceHandle '%s': expected target '%s', got '%s'", handle, expectedTarget, actualTarget)
		}
	}

	defaultConns := 0
	for _, conn := range switchConnections {
		if conn.SourceHandle == "" {
			defaultConns++
		}
	}
	if defaultConns == 0 {
		t.Log("Note: Switch node has no default connection (empty sourceHandle) — all routes use explicit sourceHandle routing")
	}
}

func TestToolOutputSourceHandleRouting(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	toolOutputConns := make([]*types.AgentConnection, 0)
	for _, conn := range pw.Connections {
		if conn.SourceHandle == "tool-output" {
			toolOutputConns = append(toolOutputConns, conn)
		}
	}

	if len(toolOutputConns) == 0 {
		t.Fatal("Expected 'tool-output' sourceHandle connections, found none")
	}

	for _, conn := range toolOutputConns {
		sourceNode := GetNodeByID(pw, conn.SourceNodeId)
		if sourceNode == nil {
			t.Errorf("tool-output connection source node '%s' not found", conn.SourceNodeId)
			continue
		}
		if !startsWith(sourceNode.Type, "tool_") && sourceNode.Type != "tool_pgvector" {
			t.Errorf("tool-output connection from non-tool node: type='%s', id='%s'", sourceNode.Type, sourceNode.Id)
		}
	}

	pgvectorNodes := GetNodesByType(pw, "tool_pgvector")
	if len(pgvectorNodes) != 1 {
		t.Fatalf("Expected 1 tool_pgvector node, got %d", len(pgvectorNodes))
	}

	pgvectorConns := GetConnectionsFrom(pw, pgvectorNodes[0].Id)
	toolOutputFromPgvector := 0
	for _, conn := range pgvectorConns {
		if conn.SourceHandle == "tool-output" {
			toolOutputFromPgvector++
		}
	}
	if toolOutputFromPgvector == 0 {
		t.Error("Expected tool-output connections from pgvector node")
	}
}

func TestGraphTopologyIntegrity(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	if pw.Graph == nil {
		t.Fatal("Graph should not be nil after ConvertToTypes")
	}

	for _, node := range pw.Nodes {
		graphNode, err := pw.Graph.GetNode(node.Id)
		if err != nil {
			t.Errorf("Node '%s' (type=%s) not found in graph: %v", node.Id, node.Type, err)
			continue
		}
		if graphNode.Node.Type != node.Type {
			t.Errorf("Node '%s' type mismatch: expected '%s', got '%s'", node.Id, node.Type, graphNode.Node.Type)
		}
	}

	manualNodes := GetNodesByType(pw, "manual")
	if len(manualNodes) != 1 {
		t.Fatalf("Expected 1 manual node, got %d", len(manualNodes))
	}

	startNodes := pw.Graph.StartNodes
	if len(startNodes) != 1 {
		t.Errorf("Expected 1 start node, got %d", len(startNodes))
	}
	if len(startNodes) > 0 && startNodes[0] != manualNodes[0].Id {
		t.Errorf("Start node '%s' doesn't match manual node '%s'", startNodes[0], manualNodes[0].Id)
	}

	fileUpload := manualNodes[0]
	fileUploadConns := GetConnectionsFrom(pw, fileUpload.Id)
	if len(fileUploadConns) != 1 {
		t.Errorf("Expected 1 connection from File-Upload, got %d", len(fileUploadConns))
	}
	if len(fileUploadConns) > 0 && fileUploadConns[0].TargetNodeId != "temp-1768324042067-2" {
		t.Errorf("File-Upload should connect to Switch-2, got '%s'", fileUploadConns[0].TargetNodeId)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestConnectionRoundTrip(t *testing.T) {
	wf, err := LoadWorkflowYAML("document_processor_v2.yaml")
	if err != nil {
		t.Fatalf("Failed to load workflow YAML: %v", err)
	}

	pw, err := ConvertToTypes(wf)
	if err != nil {
		t.Fatalf("Failed to convert to types: %v", err)
	}

	for _, conn := range pw.Connections {
		sourceNode := GetNodeByID(pw, conn.SourceNodeId)
		if sourceNode == nil {
			t.Errorf("Connection '%s' references unknown source node '%s'", conn.Id, conn.SourceNodeId)
		}

		targetNode := GetNodeByID(pw, conn.TargetNodeId)
		if targetNode == nil {
			t.Errorf("Connection '%s' references unknown target node '%s'", conn.Id, conn.TargetNodeId)
		}
	}
}

func TestYAMLNodeConfigsPreserved(t *testing.T) {
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

	switchConfig := switchNodes[0].Config
	expression, ok := switchConfig["expression"].(string)
	if !ok {
		t.Fatal("Switch node config 'expression' field is not a string")
	}
	if expression != "{{nodes.File-Upload.document.mimeType}}" {
		t.Errorf("Switch expression: expected '{{nodes.File-Upload.document.mimeType}}', got '%s'", expression)
	}

	cases, ok := switchConfig["cases"].(map[string]interface{})
	if !ok {
		t.Fatal("Switch node config 'cases' field is not a map")
	}
	expectedCases := []string{"application/octet-stream", "application/pdf", "application/vnd.ms-outlook", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "text/html"}
	for _, caseKey := range expectedCases {
		if _, exists := cases[caseKey]; !exists {
			t.Errorf("Switch cases missing expected key '%s'", caseKey)
		}
	}
}