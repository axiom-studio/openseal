package module

import (
	"context"
	"testing"
)

// mockResolver implements TemplateResolver for testing
type mockResolver struct {
	strings   map[string]string
	outputs   map[string]interface{}
	variables map[string]interface{}
}

func newMockResolver() *mockResolver {
	return &mockResolver{
		strings:   make(map[string]string),
		outputs:   make(map[string]interface{}),
		variables: make(map[string]interface{}),
	}
}

func (m *mockResolver) ResolveString(template string) string {
	if v, ok := m.strings[template]; ok {
		return v
	}
	return template
}

func (m *mockResolver) ResolveMap(input map[string]interface{}) map[string]interface{} {
	return input
}

func (m *mockResolver) EvaluateCondition(condition string) bool {
	return condition == "true"
}

func (m *mockResolver) SetVariable(name string, value interface{}) {
	m.variables[name] = value
}

func (m *mockResolver) GetStepOutput(stepName string) interface{} {
	return m.outputs[stepName]
}

func (m *mockResolver) SetStepOutput(stepName string, output interface{}) {
	m.outputs[stepName] = output
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()

	exec := NewFuncExecutor("test", func(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
		return &StepResult{Output: map[string]interface{}{"ok": true}}, nil
	})

	if err := r.Register(exec); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Should be able to get it
	got, err := r.Get("test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Type() != "test" {
		t.Errorf("Expected type 'test', got %q", got.Type())
	}

	// Should not be able to register duplicate
	if err := r.Register(exec); err == nil {
		t.Error("Expected error registering duplicate, got nil")
	}
}

func TestRegistryHas(t *testing.T) {
	r := NewRegistry()

	if r.Has("nonexistent") {
		t.Error("Has returned true for nonexistent executor")
	}

	r.Register(NewFuncExecutor("exists", func(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
		return &StepResult{}, nil
	}))

	if !r.Has("exists") {
		t.Error("Has returned false for registered executor")
	}
}

func TestRegistryListTypes(t *testing.T) {
	r := NewRegistry()

	r.Register(NewFuncExecutor("alpha", func(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
		return &StepResult{}, nil
	}))
	r.Register(NewFuncExecutor("beta", func(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
		return &StepResult{}, nil
	}))

	types := r.ListTypes()
	if len(types) != 2 {
		t.Errorf("Expected 2 types, got %d", len(types))
	}

	typeMap := make(map[string]bool)
	for _, t := range types {
		typeMap[t] = true
	}
	if !typeMap["alpha"] || !typeMap["beta"] {
		t.Error("ListTypes missing expected types")
	}
}

func TestFuncExecutorExecute(t *testing.T) {
	exec := NewFuncExecutor("mytype", func(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
		msg := step.Config["message"].(string)
		return &StepResult{
			Output: map[string]interface{}{"echo": msg},
		}, nil
	})

	if exec.Type() != "mytype" {
		t.Errorf("Expected type 'mytype', got %q", exec.Type())
	}

	step := &StepDefinition{
		Name:   "test-step",
		Type:   "mytype",
		Config: map[string]interface{}{"message": "hello"},
	}

	result, err := exec.Execute(context.Background(), step, newMockResolver())
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Output["echo"] != "hello" {
		t.Errorf("Expected echo='hello', got %v", result.Output["echo"])
	}
}

func TestDefaultRegistry(t *testing.T) {
	r, err := NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry failed: %v", err)
	}

	expectedTypes := []string{"if", "switch", "transform", "set", "merge", "delay", "http"}
	for _, typ := range expectedTypes {
		if !r.Has(typ) {
			t.Errorf("Default registry missing executor for %q", typ)
		}
	}
}

// Test lifecycle interfaces
type lifecycleExecutor struct {
	initCalled     bool
	shutdownCalled bool
	config         map[string]interface{}
}

func (e *lifecycleExecutor) Type() string { return "lifecycle" }

func (e *lifecycleExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	return &StepResult{Output: map[string]interface{}{"initialized": e.initCalled}}, nil
}

func (e *lifecycleExecutor) Init(config map[string]interface{}) error {
	e.initCalled = true
	e.config = config
	return nil
}

func (e *lifecycleExecutor) Shutdown(ctx context.Context) error {
	e.shutdownCalled = true
	return nil
}

func TestLifecycleExecutor(t *testing.T) {
	r := NewRegistry()
	exec := &lifecycleExecutor{}

	r.Register(exec)

	// Init should be called manually if needed
	exec.Init(map[string]interface{}{"key": "value"})

	if !exec.initCalled {
		t.Error("Init was not called")
	}
	if exec.config["key"] != "value" {
		t.Error("Config not passed correctly")
	}

	// Shutdown all - should call Shutdown on executors that implement it
	r.Shutdown(context.Background())

	if !exec.shutdownCalled {
		t.Error("Shutdown was not called")
	}
}

func TestLifecycleInterfaceCheck(t *testing.T) {
	exec := &lifecycleExecutor{}

	// Verify the executor implements both lifecycle interfaces
	var _ InitializableExecutor = exec
	var _ ShutdownableExecutor = exec
}
