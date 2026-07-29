package capability

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	// HostedMinimumChildOutputTokens is the portable protocol floor required
	// for one complete hosted Turn response envelope. A multi-Turn Run must
	// fund this floor for every reviewed Turn.
	HostedMinimumChildOutputTokens int64 = 16384
	// HostedMaximumProviderAttempts includes the initial provider exchange and
	// one bounded schema-repair exchange. Authoring and execution reserve the
	// same complete envelope so an activated Run cannot exhaust itself merely
	// by following the reviewed recovery protocol.
	HostedMaximumProviderAttempts int64 = 2
	// HostedRepairInputReserveTokens covers the bounded validation message
	// appended to a schema-repair request.
	HostedRepairInputReserveTokens int64 = 1024
)

const hostedSkillBindingMetadataReserveTokens int64 = 1024

// HostedSkillModelInputTokenCeiling returns a credential-free conservative
// ceiling for one activated binding of definition. It includes the complete
// private prompt and model-visible action contracts without exposing either.
func HostedSkillModelInputTokenCeiling(definition Definition) (int64, error) {
	type modelAction struct {
		Name                    string                  `json:"name"`
		Description             string                  `json:"description"`
		SkillID                 string                  `json:"skillId"`
		Version                 string                  `json:"version"`
		Action                  string                  `json:"action"`
		InputSchema             map[string]interface{}  `json:"inputSchema"`
		SemanticArguments       map[string]string       `json:"semanticArguments,omitempty"`
		Risk                    RiskLevel               `json:"risk"`
		SideEffect              SideEffect              `json:"sideEffect"`
		ExternalOperationPolicy ExternalOperationPolicy `json:"externalOperationPolicy,omitempty"`
	}
	payload := struct {
		Prompt  *PromptModule `json:"prompt,omitempty"`
		Actions []modelAction `json:"actions,omitempty"`
	}{Prompt: definition.Prompt}
	names := make([]string, 0, len(definition.Actions))
	for name := range definition.Actions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		action := definition.Actions[name]
		payload.Actions = append(payload.Actions, modelAction{
			Name: definition.ID + "." + name, Description: action.Description,
			SkillID: definition.ID, Version: definition.Version, Action: name,
			InputSchema: action.InputSchema, SemanticArguments: action.SemanticArguments,
			Risk: action.Risk, SideEffect: action.SideEffect, ExternalOperationPolicy: action.ExternalOperationPolicy,
		})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal hosted Skill model envelope: %w", err)
	}
	return (int64(len(encoded))+1)/2 + hostedSkillBindingMetadataReserveTokens, nil
}
