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

func TestRegistryRequiresSkillIdentityForSharedNodeType(t *testing.T) {
	r := NewRegistry()
	first := &Client{skillID: "first"}
	second := &Client{skillID: "second"}
	r.clients[first.skillID] = first
	r.clients[second.skillID] = second
	r.typesBySkill[first.skillID] = map[string]struct{}{"openclaw": {}}
	r.typesBySkill[second.skillID] = map[string]struct{}{"openclaw": {}, "unique": {}}
	r.skillsByType["openclaw"] = map[string]struct{}{first.skillID: {}, second.skillID: {}}
	r.skillsByType["unique"] = map[string]struct{}{second.skillID: {}}

	assert.Nil(t, r.GetClientForType("openclaw"), "ambiguous endpoint must fail closed")
	assert.Same(t, first, r.GetClientForSkillType("first", "openclaw"))
	assert.Same(t, second, r.GetClientForSkillType("second", "openclaw"))
	assert.Same(t, second, r.GetClientForType("unique"))
	assert.Equal(t, []string{"openclaw", "unique"}, r.GetSkillTypes("second"))

	require.NoError(t, r.Unregister("first"))
	assert.Same(t, second, r.GetClientForType("openclaw"))
}

func TestRegistryKeepsSameReportedSkillUnderIndependentAuthorityKeys(t *testing.T) {
	r := NewRegistry()
	firstKey := "tenant:1\x00skill:ideogram"
	secondKey := "tenant:3\x00skill:ideogram"
	first := &Client{skillID: "ideogram"}
	second := &Client{skillID: "ideogram"}
	r.clients[firstKey] = first
	r.clients[secondKey] = second
	r.typesBySkill[firstKey] = map[string]struct{}{"generate": {}}
	r.typesBySkill[secondKey] = map[string]struct{}{"generate": {}}
	r.skillsByType["generate"] = map[string]struct{}{firstKey: {}, secondKey: {}}

	assert.Same(t, first, r.GetClientForSkillType(firstKey, "generate"))
	assert.Same(t, second, r.GetClientForSkillType(secondKey, "generate"))
	assert.Nil(t, r.GetClientForSkillType("ideogram", "generate"), "reported Skill ID must not bypass scoped authority")
	assert.Nil(t, r.GetClientForType("generate"), "cross-authority ambiguity must fail closed")

	require.NoError(t, r.Unregister(firstKey))
	assert.Same(t, second, r.GetClientForSkillType(secondKey, "generate"))
}
