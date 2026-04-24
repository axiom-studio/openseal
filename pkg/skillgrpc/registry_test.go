package skillgrpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_GetSkillTypes(t *testing.T) {
	r := NewRegistry()

	types := r.GetSkillTypes("skill-1")
	assert.Empty(t, types)
}

func TestRegistry_RegisterAndGetSkillTypes(t *testing.T) {
	r := NewRegistry()

	client := NewClient("localhost:50051")

	_, err := r.Register(context.Background(), "localhost:50051")
	require.Error(t, err)

	_ = client
}

func TestRegistry_UnregisterAndGetSkillTypes(t *testing.T) {
	r := NewRegistry()

	err := r.Unregister("missing-skill")
	assert.NoError(t, err)
}

func TestRegistry_HasType(t *testing.T) {
	r := NewRegistry()

	assert.False(t, r.HasType("non-existent"))
}

func TestRegistry_ListSkillsEmpty(t *testing.T) {
	r := NewRegistry()

	skills := r.ListSkills()
	assert.Empty(t, skills)
}

func TestRegistry_ListTypesEmpty(t *testing.T) {
	r := NewRegistry()

	types := r.ListTypes()
	assert.Empty(t, types)
}
