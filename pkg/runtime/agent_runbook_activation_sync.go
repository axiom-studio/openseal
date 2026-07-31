package runtime

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
)

// reconcileAgentRunbookActivations advances the operational projection of an
// Agent's embedded Runbook to the exact immutable definition being activated.
// Existing activation identities and deployed Objective identities are kept
// stable; a definition amendment is not allowed to manufacture new deployed
// Objectives behind the workforce review boundary.
func reconcileAgentRunbookActivations(current []*RunbookActivation, definition *kernelagent.AgentDefinition, deployment *kernelagent.AgentDeployment, updatedAt time.Time) ([]*RunbookActivation, error) {
	if definition == nil || deployment == nil {
		return nil, errors.New("Agent definition and deployment are required to synchronize Runbook activations")
	}
	byTrigger := make(map[string]*RunbookActivation, len(current))
	for _, activation := range current {
		if activation == nil || activation.AssignedAgentID != deployment.ID {
			continue
		}
		existing := byTrigger[activation.TriggerID]
		if existing == nil || (existing.Status == RunbookActivationRetired && activation.Status != RunbookActivationRetired) {
			byTrigger[activation.TriggerID] = activation
			continue
		}
		// A replaced schedule intentionally preserves the trigger identity in
		// its lineage. Its retired predecessor is history, not a second live
		// projection of the Agent definition.
		if activation.Status == RunbookActivationRetired {
			continue
		}
		if existing.Status != RunbookActivationRetired {
			return nil, fmt.Errorf("Agent %s has duplicate Runbook activation trigger %s", deployment.ID, activation.TriggerID)
		}
	}

	desiredTriggers := make(map[string]bool)
	result := make([]*RunbookActivation, 0, len(current))
	if definition.Runbook != nil {
		triggerIDs := make([]string, 0, len(definition.Runbook.Triggers))
		for triggerID := range definition.Runbook.Triggers {
			triggerIDs = append(triggerIDs, triggerID)
		}
		sort.Strings(triggerIDs)
		for _, triggerID := range triggerIDs {
			trigger := definition.Runbook.Triggers[triggerID]
			existing := byTrigger[triggerID]
			if existing == nil {
				return nil, fmt.Errorf("Agent amendment cannot add Runbook trigger %s without a reviewed deployed Objective", triggerID)
			}
			if existing.Status == RunbookActivationRetired {
				return nil, fmt.Errorf("Agent amendment cannot reactivate retired Runbook trigger %s", triggerID)
			}
			input, err := materializeRunbookTriggerInput(trigger.Input)
			if err != nil {
				return nil, fmt.Errorf("Runbook trigger %s input: %w", triggerID, err)
			}
			next := cloneRunbookActivation(existing)
			next.DefinitionID = definition.Runbook.ID
			next.DefinitionVersion = definition.Runbook.Version
			next.Trigger = trigger
			next.Trigger.ObjectiveID = existing.ObjectiveID
			next.Input = input
			next.Budget = runbookBudgetPolicyValue(trigger.Budget)
			next.MaximumConcurrent = trigger.MaximumConcurrent
			next.Revision++
			next.UpdatedAt = updatedAt.UTC()
			if !reflect.DeepEqual(existing.Trigger.Schedule, trigger.Schedule) {
				next.NextOccurrenceBase, next.NextRunAt = nil, nil
			}
			if err := next.Validate(); err != nil {
				return nil, fmt.Errorf("synchronize Runbook trigger %s: %w", triggerID, err)
			}
			desiredTriggers[triggerID] = true
			result = append(result, next)
		}
	}

	for _, activation := range current {
		if activation == nil || activation.AssignedAgentID != deployment.ID || desiredTriggers[activation.TriggerID] {
			continue
		}
		if activation.Status == RunbookActivationRetired {
			continue
		}
		next := cloneRunbookActivation(activation)
		next.Status = RunbookActivationRetired
		next.NextOccurrenceBase, next.NextRunAt = nil, nil
		next.Revision++
		next.UpdatedAt = updatedAt.UTC()
		if err := next.Validate(); err != nil {
			return nil, fmt.Errorf("retire Runbook trigger %s: %w", strings.TrimSpace(next.TriggerID), err)
		}
		result = append(result, next)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
