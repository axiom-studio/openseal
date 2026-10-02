package runtime

import (
	"context"
	"errors"
	"time"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func (s *MemoryStore) ApplySkillReferenceUpgrade(ctx context.Context, application *SkillReferenceUpgradeMutation) error {
	if err := validateSkillReferenceUpgradeMutation(application); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSkillUpgradeMaintenance(ctx, s.memorySkillRuntimeMaintenanceActiveLocked(application.Plan.Scope, application.Plan.From.ID), application, time.Now().UTC()); err != nil {
		return err
	}
	bindingKey := memorySkillBindingKey(application.Binding.Scope, application.Binding.DeploymentID, application.Binding.ID)
	currentBinding := s.skillBindings[bindingKey]
	if currentBinding == nil || currentBinding.Disabled || currentBinding.Revision != application.Plan.ExpectedBindingRevision ||
		application.Binding.Revision != currentBinding.Revision+1 || currentBinding.SkillID != application.Plan.From.ID ||
		currentBinding.SkillVersion != application.Plan.From.Version || currentBinding.SourceIdentity != application.Plan.From.SourceIdentity {
		return ErrSkillReferenceUpgradeConflict
	}
	if s.hasMemorySkillRuntimeUsageLocked(skillBindingUpgradeUsageFilter(application)) {
		return ErrSkillReferenceUpgradeBusy
	}
	if err := s.validateProjectSkillUpgradeReferencesLocked(application); err != nil {
		return err
	}
	for _, mutation := range application.Objectives {
		current := s.objectives[portfolioKey(mutation.Value.Scope, mutation.Value.ID)]
		if current == nil || current.Revision != mutation.ExpectedRevision || mutation.Value.Revision != mutation.ExpectedRevision+1 {
			return ErrSkillReferenceUpgradeConflict
		}
	}
	for _, mutation := range application.Projects {
		current := s.projects[projectKey(mutation.Value.Scope, mutation.Value.ID)]
		if current == nil || current.Revision != mutation.ExpectedRevision || mutation.Value.Revision != mutation.ExpectedRevision+1 {
			return ErrSkillReferenceUpgradeConflict
		}
	}
	for _, mutation := range application.ConversationEndpoints {
		current := s.externalEndpoints[externalConversationEndpointKey(mutation.Value.Scope, mutation.Value.ID)]
		if current == nil || current.Revision != mutation.ExpectedRevision || mutation.Value.Revision != mutation.ExpectedRevision+1 {
			return ErrSkillReferenceUpgradeConflict
		}
	}
	for _, mutation := range application.CallbackRegistrations {
		current := s.callbackRegistrations[callbackRegistrationKey(mutation.Value.Scope, mutation.Value.ID)]
		if current == nil || current.Revision != mutation.ExpectedRevision || mutation.Value.Revision != mutation.ExpectedRevision+1 {
			return ErrSkillReferenceUpgradeConflict
		}
	}
	apply := func() error {
		// Acquiring the Team lock may wait past an owner's lease. Recheck the
		// clock after every authority lock and before making any record visible.
		if err := validateSkillUpgradeMaintenance(ctx, s.memorySkillRuntimeMaintenanceActiveLocked(application.Plan.Scope, application.Plan.From.ID), application, time.Now().UTC()); err != nil {
			return err
		}
		s.skillBindings[bindingKey] = cloneMemorySkillBinding(application.Binding)
		for _, mutation := range application.Objectives {
			s.objectives[portfolioKey(mutation.Value.Scope, mutation.Value.ID)] = cloneObjective(mutation.Value)
			appendMemoryActivityLocked(s, mutation.Event)
		}
		for _, mutation := range application.Projects {
			s.saveProjectSkillReferencesLocked(mutation.Value)
			appendMemoryActivityLocked(s, mutation.Event)
		}
		for _, mutation := range application.ConversationEndpoints {
			s.externalEndpoints[externalConversationEndpointKey(mutation.Value.Scope, mutation.Value.ID)] = cloneExternalConversationEndpoint(mutation.Value)
		}
		for _, mutation := range application.CallbackRegistrations {
			s.callbackRegistrations[callbackRegistrationKey(mutation.Value.Scope, mutation.Value.ID)] = cloneCallbackRegistration(mutation.Value)
		}
		return nil
	}
	if mutation := application.TeamAuthority; mutation != nil {
		err := s.MemoryStore.CompareAndApplyDefinitionUpgrade(ctx, mutation.PreviousDeployment, mutation.PreviousDefinition,
			mutation.Definition, mutation.Deployment, mutation.Activation, apply)
		if errors.Is(err, kernelteam.ErrRevisionConflict) {
			return ErrSkillReferenceUpgradeConflict
		}
		return err
	}
	return apply()
}
