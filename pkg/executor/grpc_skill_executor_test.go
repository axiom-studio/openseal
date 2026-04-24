package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

type mockStepExecutor struct {
	stepType string
}

func (m *mockStepExecutor) Type() string {
	return m.stepType
}

func (m *mockStepExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	return nil, nil
}

func TestGRPCSkillRegistry_UnregisterSkill(t *testing.T) {
	execRegistry := NewEmptyRegistry()
	grpcRegistry := NewGRPCSkillRegistry(execRegistry)

	exec := &mockStepExecutor{stepType: "test-node"}
	execRegistry.Register(exec)
	assert.True(t, execRegistry.HasExecutor("test-node"))

	err := grpcRegistry.UnregisterSkill("skill-1")
	assert.NoError(t, err)

	assert.True(t, execRegistry.HasExecutor("test-node"))
}

func TestGRPCSkillRegistry_ListSkillsEmpty(t *testing.T) {
	execRegistry := NewEmptyRegistry()
	grpcRegistry := NewGRPCSkillRegistry(execRegistry)

	skills := grpcRegistry.ListSkills()
	assert.Empty(t, skills)

	types := grpcRegistry.ListTypes()
	assert.Empty(t, types)
}
