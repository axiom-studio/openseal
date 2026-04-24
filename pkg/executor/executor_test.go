package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

type mockExecutor struct {
	stepType string
}

func (m *mockExecutor) Type() string {
	return m.stepType
}

func (m *mockExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	return &StepResult{Output: map[string]interface{}{"mock": true}}, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewEmptyRegistry()
	exec := &mockExecutor{stepType: "test-step"}

	r.Register(exec)

	got, err := r.Get("test-step")
	assert.NoError(t, err)
	assert.Equal(t, exec, got)
}

func TestRegistry_Get_NotFound(t *testing.T) {
	r := NewEmptyRegistry()

	_, err := r.Get("missing-step")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no executor registered for step type: missing-step")
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewEmptyRegistry()
	exec := &mockExecutor{stepType: "test-step"}

	r.Register(exec)
	assert.True(t, r.HasExecutor("test-step"))

	r.Unregister("test-step")
	assert.False(t, r.HasExecutor("test-step"))

	_, err := r.Get("test-step")
	assert.Error(t, err)
}

func TestRegistry_Unregister_NotFound(t *testing.T) {
	r := NewEmptyRegistry()

	r.Unregister("missing-step")
	assert.False(t, r.HasExecutor("missing-step"))
}

func TestRegistry_MultipleExecutors(t *testing.T) {
	r := NewEmptyRegistry()
	exec1 := &mockExecutor{stepType: "step-1"}
	exec2 := &mockExecutor{stepType: "step-2"}

	r.Register(exec1)
	r.Register(exec2)

	assert.True(t, r.HasExecutor("step-1"))
	assert.True(t, r.HasExecutor("step-2"))

	r.Unregister("step-1")
	assert.False(t, r.HasExecutor("step-1"))
	assert.True(t, r.HasExecutor("step-2"))
}

func TestGlobalRegistry(t *testing.T) {
	exec := &mockExecutor{stepType: "global-step"}
	Register("global-step", exec)

	assert.True(t, GlobalRegistry().HasExecutor("global-step"))
}
