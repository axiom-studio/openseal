package module

import (
	"context"
	"testing"
)

func TestIfExecutor(t *testing.T) {
	exec := NewFuncExecutor("if", ifExecute)
	resolver := newMockResolver()

	tests := []struct {
		name       string
		condition  string
		thenStep   string
		elseStep   string
		wantBranch string
		wantNext   string
	}{
		{
			name:       "condition true",
			condition:  "true",
			thenStep:   "stepA",
			elseStep:   "stepB",
			wantBranch: "then",
			wantNext:   "stepA",
		},
		{
			name:       "condition false",
			condition:  "false",
			thenStep:   "stepA",
			elseStep:   "stepB",
			wantBranch: "else",
			wantNext:   "stepB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{
				Name: "test-if",
				Type: "if",
				Config: map[string]interface{}{
					"condition": tt.condition,
					"then":      tt.thenStep,
					"else":      tt.elseStep,
				},
			}

			result, err := exec.Execute(context.Background(), step, resolver)
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}

			if result.Output["branch"] != tt.wantBranch {
				t.Errorf("Expected branch %q, got %q", tt.wantBranch, result.Output["branch"])
			}
			if result.NextStep != tt.wantNext {
				t.Errorf("Expected next step %q, got %q", tt.wantNext, result.NextStep)
			}
		})
	}
}

func TestSwitchExecutor(t *testing.T) {
	exec := NewFuncExecutor("switch", switchExecute)
	resolver := newMockResolver()

	step := &StepDefinition{
		Name: "test-switch",
		Type: "switch",
		Config: map[string]interface{}{
			"expression": "status",
			"cases": map[string]interface{}{
				"success": "handleSuccess",
				"error":   "handleError",
			},
			"default": "handleDefault",
		},
	}

	// Test matching case
	resolver.strings["status"] = "success"
	result, err := exec.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.NextStep != "handleSuccess" {
		t.Errorf("Expected next step 'handleSuccess', got %q", result.NextStep)
	}

	// Test default case
	resolver.strings["status"] = "unknown"
	result, err = exec.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.NextStep != "handleDefault" {
		t.Errorf("Expected next step 'handleDefault', got %q", result.NextStep)
	}
}

func TestTransformExecutor(t *testing.T) {
	exec := NewFuncExecutor("transform", transformExecute)
	resolver := newMockResolver()

	// Test with map template
	step := &StepDefinition{
		Name: "test-transform",
		Type: "transform",
		Config: map[string]interface{}{
			"template": map[string]interface{}{
				"name":   "test",
				"status": "ok",
			},
		},
	}

	result, err := exec.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Output["name"] != "test" || result.Output["status"] != "ok" {
		t.Errorf("Unexpected output: %v", result.Output)
	}

	// Test with string template
	step.Config["template"] = "hello world"
	result, err = exec.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Output["result"] != "hello world" {
		t.Errorf("Expected result 'hello world', got %v", result.Output["result"])
	}
}

func TestSetExecutor(t *testing.T) {
	exec := NewFuncExecutor("set", setExecute)
	resolver := newMockResolver()

	step := &StepDefinition{
		Name: "test-set",
		Type: "set",
		Config: map[string]interface{}{
			"name":  "myVar",
			"value": "myValue",
		},
	}

	result, err := exec.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Output["name"] != "myVar" {
		t.Errorf("Expected name 'myVar', got %v", result.Output["name"])
	}
	if result.Output["value"] != "myValue" {
		t.Errorf("Expected value 'myValue', got %v", result.Output["value"])
	}
	if resolver.variables["myVar"] != "myValue" {
		t.Errorf("Variable not set in resolver")
	}
}

func TestMergeExecutor(t *testing.T) {
	exec := NewFuncExecutor("merge", mergeExecute)
	resolver := newMockResolver()

	// Test merge strategy
	step := &StepDefinition{
		Name: "test-merge",
		Type: "merge",
		Config: map[string]interface{}{
			"sources": []interface{}{
				map[string]interface{}{"a": 1},
				map[string]interface{}{"b": 2},
			},
			"strategy": "merge",
		},
	}

	result, err := exec.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Output["a"] != 1 || result.Output["b"] != 2 {
		t.Errorf("Merge failed, got: %v", result.Output)
	}

	// Test concat strategy
	step.Config["strategy"] = "concat"
	step.Config["sources"] = []interface{}{"a", "b", "c"}

	result, err = exec.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	arr, ok := result.Output["result"].([]interface{})
	if !ok {
		t.Fatalf("Expected array result, got %T", result.Output["result"])
	}
	if len(arr) != 3 {
		t.Errorf("Expected 3 items, got %d", len(arr))
	}
}

func TestDelayExecutor(t *testing.T) {
	exec := NewFuncExecutor("delay", delayExecute)
	resolver := newMockResolver()

	step := &StepDefinition{
		Name: "test-delay",
		Type: "delay",
		Config: map[string]interface{}{
			"milliseconds": float64(10), // Very short delay for test
		},
	}

	result, err := exec.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Output["delayed"] != true {
		t.Error("Expected delayed=true")
	}
}

func TestDelayExecutorContextCancellation(t *testing.T) {
	exec := NewFuncExecutor("delay", delayExecute)
	resolver := newMockResolver()

	step := &StepDefinition{
		Name: "test-delay",
		Type: "delay",
		Config: map[string]interface{}{
			"seconds": float64(10), // Long delay
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := exec.Execute(ctx, step, resolver)
	if err == nil {
		t.Error("Expected context cancellation error")
	}
}

func TestExecutorErrors(t *testing.T) {
	resolver := newMockResolver()

	tests := []struct {
		name   string
		exec   *FuncExecutor
		config map[string]interface{}
	}{
		{
			name: "if missing condition",
			exec: NewFuncExecutor("if", ifExecute),
			config: map[string]interface{}{
				"then": "stepA",
			},
		},
		{
			name:   "switch missing expression",
			exec:   NewFuncExecutor("switch", switchExecute),
			config: map[string]interface{}{},
		},
		{
			name:   "transform missing template",
			exec:   NewFuncExecutor("transform", transformExecute),
			config: map[string]interface{}{},
		},
		{
			name:   "set missing name",
			exec:   NewFuncExecutor("set", setExecute),
			config: map[string]interface{}{},
		},
		{
			name:   "merge missing sources",
			exec:   NewFuncExecutor("merge", mergeExecute),
			config: map[string]interface{}{},
		},
		{
			name:   "delay missing duration",
			exec:   NewFuncExecutor("delay", delayExecute),
			config: map[string]interface{}{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{
				Name:   "test",
				Type:   tt.exec.Type(),
				Config: tt.config,
			}

			_, err := tt.exec.Execute(context.Background(), step, resolver)
			if err == nil {
				t.Error("Expected error, got nil")
			}
		})
	}
}
