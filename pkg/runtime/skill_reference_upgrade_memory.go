package runtime

import (
	"context"
)

func (s *MemoryStore) ApplySkillReferenceUpgrade(_ context.Context, application *SkillReferenceUpgradeMutation) error {
	if err := validateSkillReferenceUpgradeMutation(application); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bindingKey := memorySkillBindingKey(application.Binding.Scope, application.Binding.DeploymentID, application.Binding.ID)
	currentBinding := s.skillBindings[bindingKey]
	if currentBinding == nil || currentBinding.Revision != application.Plan.ExpectedBindingRevision ||
		application.Binding.Revision != currentBinding.Revision+1 {
		return ErrSkillReferenceUpgradeConflict
	}
	for _, mutation := range application.Objectives {
		current := s.objectives[portfolioKey(mutation.Value.Scope, mutation.Value.ID)]
		if current == nil || current.Revision != mutation.ExpectedRevision || mutation.Value.Revision != mutation.ExpectedRevision+1 {
			return ErrSkillReferenceUpgradeConflict
		}
	}
	for _, mutation := range application.Initiatives {
		current := s.initiatives[initiativeKey(mutation.Value.Scope, mutation.Value.ID)]
		if current == nil || current.Revision != mutation.ExpectedRevision || mutation.Value.Revision != mutation.ExpectedRevision+1 {
			return ErrSkillReferenceUpgradeConflict
		}
	}
	s.skillBindings[bindingKey] = cloneMemorySkillBinding(application.Binding)
	for _, mutation := range application.Objectives {
		s.objectives[portfolioKey(mutation.Value.Scope, mutation.Value.ID)] = cloneObjective(mutation.Value)
		appendMemoryActivityLocked(s, mutation.Event)
	}
	for _, mutation := range application.Initiatives {
		s.initiatives[initiativeKey(mutation.Value.Scope, mutation.Value.ID)] = cloneInitiative(mutation.Value)
		appendMemoryActivityLocked(s, mutation.Event)
	}
	return nil
}
