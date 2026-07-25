package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func memorySkillDefinitionKey(id, version, sourceIdentity string) string {
	return id + "\x00" + version + "\x00" + sourceIdentity
}

func memorySkillBindingKey(scope skill.ScopeReference, deploymentID, bindingID string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + deploymentID + "\x00" + bindingID
}

func cloneMemorySkillDefinition(value *skill.Definition) *skill.Definition {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result skill.Definition
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func cloneMemorySkillBinding(value *skill.Binding) *skill.Binding {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result skill.Binding
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func (s *MemoryStore) CreateSkillDefinition(_ context.Context, definition *skill.Definition) error {
	sourceIdentity := skill.DefinitionSourceIdentity(definition)
	key := memorySkillDefinitionKey(definition.ID, definition.Version, sourceIdentity)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.skillDefinitions[key] != nil {
		return skill.ErrDefinitionImmutable
	}
	s.skillDefinitions[key] = cloneMemorySkillDefinition(definition)
	return nil
}

func (s *MemoryStore) ListSkillDefinitionVariants(_ context.Context, id, version string) ([]*skill.Definition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*skill.Definition, 0)
	prefix := id + "\x00" + version + "\x00"
	for key, definition := range s.skillDefinitions {
		if strings.HasPrefix(key, prefix) {
			result = append(result, cloneMemorySkillDefinition(definition))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return skill.DefinitionSourceIdentity(result[i]) < skill.DefinitionSourceIdentity(result[j])
	})
	return result, nil
}

func (s *MemoryStore) SaveSkillBinding(_ context.Context, binding *skill.Binding, expectedRevision int64) error {
	if err := skill.ValidateBindingShape(binding); err != nil {
		return err
	}
	key := memorySkillBindingKey(binding.Scope, binding.DeploymentID, binding.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.skillBindings[key]
	if current == nil && expectedRevision != 0 || current != nil && current.Revision != expectedRevision {
		return skill.ErrBindingRevisionConflict
	}
	s.skillBindings[key] = cloneMemorySkillBinding(binding)
	return nil
}

func (s *MemoryStore) ListSkillBindings(_ context.Context, scope skill.ScopeReference, deploymentID string) ([]*skill.Binding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*skill.Binding, 0)
	for _, binding := range s.skillBindings {
		if binding.Scope == scope && binding.DeploymentID == deploymentID {
			result = append(result, cloneMemorySkillBinding(binding))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
