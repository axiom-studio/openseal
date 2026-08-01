package authoring

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

var authoringIntentKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// CompileAuthoringIntent is the trusted form-to-domain boundary. Provider
// answers never survive as runtime structure: every ID, version, policy,
// Runbook edge, pointer, budget, and authority projection below is owned by
// OpenSeal and is subsequently checked by the normal candidate validators.
func CompileAuthoringIntent(intent AuthoringIntent, request GenerateRequest) (GenerationResponse, error) {
	if err := validateAuthoringIntent(intent, request.Catalog); err != nil {
		return GenerationResponse{}, err
	}
	result := GenerationResponse{
		SchemaVersion: AuthoringResultSchemaVersion,
		Candidate:     WorkforceCandidate{Activation: intent.Activation},
		Authoring:     AuthoringFormSubmission{Version: AuthoringFormVersionV1},
		Assumptions:   normalized(intent.Assumptions),
	}
	agents := make(map[string]*agent.AgentDefinition, len(intent.Agents))
	for _, answer := range intent.Agents {
		definition, err := compileAuthoringAgent(answer, request)
		if err != nil {
			return GenerationResponse{}, err
		}
		agents[answer.Key] = definition
		result.Candidate.Agents = append(result.Candidate.Agents, definition)
	}
	if intent.Team != nil {
		definition, assignments, err := compileAuthoringTeam(*intent.Team, agents, request.Catalog)
		if err != nil {
			return GenerationResponse{}, err
		}
		result.Candidate.Team = definition
		result.Candidate.Assignments = assignments
	}
	questions, err := compileAuthoringClarifications(intent.Clarifications)
	if err != nil {
		return GenerationResponse{}, err
	}
	result.UnresolvedQuestions = questions
	return result, nil
}

func validateAuthoringIntent(intent AuthoringIntent, catalog CapabilityCatalog) error {
	if intent.SchemaVersion != AuthoringIntentSchemaVersion {
		return fmt.Errorf("authoring intent schema version must be %s", AuthoringIntentSchemaVersion)
	}
	if !intent.Kind.Valid() || strings.TrimSpace(intent.Name) == "" || strings.TrimSpace(intent.Purpose) == "" {
		return errors.New("authoring intent requires a resource kind, name, and purpose")
	}
	if _, err := EffectiveWorkforceActivationIntent(intent.Activation); err != nil {
		return err
	}
	if len(intent.Agents) == 0 {
		return errors.New("authoring intent requires at least one Agent")
	}
	if intent.Kind == AuthoringResourceAgent && (len(intent.Agents) != 1 || intent.Team != nil) {
		return errors.New("single-Agent intent requires exactly one Agent and no Team")
	}
	if (intent.Kind == AuthoringResourceTeam || intent.Kind == AuthoringResourceWorkforce) && intent.Team == nil {
		return errors.New("Team and Workforce intents require a Team")
	}
	seenAgents := map[string]bool{}
	for _, answer := range intent.Agents {
		if !validAuthoringIntentKey(answer.Key) || seenAgents[answer.Key] || strings.TrimSpace(answer.Name) == "" || strings.TrimSpace(answer.Purpose) == "" || strings.TrimSpace(answer.Behavior) == "" {
			return fmt.Errorf("Agent answer %q requires a unique key, name, purpose, and behavior", answer.Key)
		}
		seenAgents[answer.Key] = true
		seenSkills := map[string]bool{}
		for _, selection := range answer.Skills {
			skill, ok := catalog.Skills[selection.CatalogID]
			if !ok || seenSkills[selection.CatalogID] {
				return fmt.Errorf("Agent %s selects unknown or duplicate catalog Skill %q", answer.Key, selection.CatalogID)
			}
			seenSkills[selection.CatalogID] = true
			for _, action := range selection.Actions {
				if !containsString(skill.Actions, action) {
					return fmt.Errorf("Agent %s selects unknown action %s.%s", answer.Key, selection.CatalogID, action)
				}
			}
		}
		seenObjectives := map[string]bool{}
		for _, objective := range answer.Objectives {
			if !validAuthoringIntentKey(objective.Key) || seenObjectives[objective.Key] || strings.TrimSpace(objective.Title) == "" || strings.TrimSpace(objective.Outcome) == "" || objective.Priority < 0 {
				return fmt.Errorf("Agent %s has an invalid Objective answer %q", answer.Key, objective.Key)
			}
			seenObjectives[objective.Key] = true
		}
		seenOperations := map[string]bool{}
		for _, operation := range answer.Operations {
			if !validAuthoringIntentKey(operation.Key) || seenOperations[operation.Key] || strings.TrimSpace(operation.Name) == "" || strings.TrimSpace(operation.Goal) == "" || !operation.Wake.Valid() || !operation.Approval.Valid() || !seenObjectives[operation.ObjectiveKey] {
				return fmt.Errorf("Agent %s has an invalid operation answer %q", answer.Key, operation.Key)
			}
			seenOperations[operation.Key] = true
			for _, skillID := range operation.SkillCatalogIDs {
				if !seenSkills[skillID] {
					return fmt.Errorf("operation %s references Skill %q not selected by Agent %s", operation.Key, skillID, answer.Key)
				}
			}
			switch operation.Wake {
			case AuthoringWakeSchedule:
				if strings.TrimSpace(operation.Schedule) == "" {
					return fmt.Errorf("scheduled operation %s requires semantic schedule text", operation.Key)
				}
			case AuthoringWakeEvent:
				if strings.TrimSpace(operation.EventType) == "" {
					return fmt.Errorf("event operation %s requires an event type", operation.Key)
				}
			}
		}
	}
	if intent.Team != nil {
		if !validAuthoringIntentKey(intent.Team.Key) || strings.TrimSpace(intent.Team.Name) == "" || strings.TrimSpace(intent.Team.Purpose) == "" || len(intent.Team.Roles) == 0 {
			return errors.New("Team answer requires a key, name, purpose, and roles")
		}
		seenRoles := map[string]bool{}
		for _, role := range intent.Team.Roles {
			if !validAuthoringIntentKey(role.Key) || seenRoles[role.Key] || strings.TrimSpace(role.Name) == "" || strings.TrimSpace(role.Purpose) == "" || len(role.AgentKeys) == 0 {
				return fmt.Errorf("Team has an invalid role answer %q", role.Key)
			}
			seenRoles[role.Key] = true
			for _, key := range role.AgentKeys {
				if !seenAgents[key] {
					return fmt.Errorf("role %s references unknown Agent %q", role.Key, key)
				}
			}
			for _, skillID := range role.SkillCatalogIDs {
				if _, ok := catalog.Skills[skillID]; !ok {
					return fmt.Errorf("role %s references unknown catalog Skill %q", role.Key, skillID)
				}
			}
		}
	}
	return nil
}

func compileAuthoringAgent(answer AuthoringAgentIntent, request GenerateRequest) (*agent.AgentDefinition, error) {
	definition := &agent.AgentDefinition{
		ID: answer.Key, Version: "1.0.0", DisplayName: strings.TrimSpace(answer.Name),
		Purpose: strings.TrimSpace(answer.Purpose), SystemPrompt: strings.TrimSpace(answer.Behavior),
		Personality: strings.TrimSpace(answer.Personality), OperatingPrinciples: normalized(answer.OperatingPrinciples),
		Authority:  agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		Amendments: workforce.AmendmentPolicy{AgentMayPropose: true},
	}
	selectedSkills := map[string]AuthoringSkillIntent{}
	for _, selection := range answer.Skills {
		selectedSkills[selection.CatalogID] = selection
		skill := request.Catalog.Skills[selection.CatalogID]
		actions := append([]string(nil), selection.Actions...)
		if len(actions) == 0 && selection.Required {
			actions = append(actions, skill.Actions...)
		}
		sort.Strings(actions)
		definition.SkillRequirements = append(definition.SkillRequirements, agent.SkillRequirement{
			SkillID: selection.CatalogID, VersionConstraint: skill.Version,
			RequiredActions: actions, PromptRequired: skill.PromptAvailable,
		})
		definition.Authority.AllowedSkillIDs = append(definition.Authority.AllowedSkillIDs, selection.CatalogID)
		definition.Authority.MaximumRisk = maximumAuthoringRisk(definition.Authority.MaximumRisk, selectedSkillRisk(skill, actions))
	}
	sort.Strings(definition.Authority.AllowedSkillIDs)
	for _, objective := range answer.Objectives {
		definition.ObjectiveTemplates = append(definition.ObjectiveTemplates, workforce.ObjectiveTemplate{
			ID: objective.Key, Title: strings.TrimSpace(objective.Title), Goal: strings.TrimSpace(objective.Outcome), Priority: objective.Priority,
			SuccessCriteria: stringListObject(objective.SuccessCriteria), Constraints: stringListObject(objective.Constraints),
		})
	}
	if len(answer.Operations) > 0 {
		runbookDefinition, err := compileAuthoringRunbook(answer, definition, selectedSkills, request)
		if err != nil {
			return nil, err
		}
		definition.Runbook = runbookDefinition
	}
	return definition, nil
}

func compileAuthoringRunbook(answer AuthoringAgentIntent, definition *agent.AgentDefinition, selected map[string]AuthoringSkillIntent, request GenerateRequest) (*runbook.Definition, error) {
	result := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: answer.Key + "-operations", Version: "1.0.0",
		Name: answer.Name + " operations", Description: "Compiler-owned execution methods for " + answer.Purpose,
		Entrypoints: map[string]string{}, Interfaces: map[string]runbook.Interface{}, Triggers: map[string]runbook.Trigger{}, Steps: map[string]runbook.Step{},
	}
	for _, operation := range answer.Operations {
		stepID, endID := operation.Key, operation.Key+"-done"
		result.Entrypoints[operation.Key] = stepID
		result.Interfaces[operation.Key] = runbook.Interface{
			Description:  strings.TrimSpace(operation.Goal),
			InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false},
			OutputSchema: map[string]interface{}{"type": "object"},
		}
		encodedAgent, _ := json.Marshal(definition.ID)
		encodedGoal, _ := json.Marshal(strings.TrimSpace(operation.Goal))
		context := map[string]runbook.Value{}
		if len(operation.SkillCatalogIDs) > 0 {
			encodedSkills, _ := json.Marshal(operation.SkillCatalogIDs)
			context["authorizedSkillCatalogIds"] = runbook.Value{Literal: encodedSkills}
		}
		result.Steps[stepID] = runbook.Step{Kind: runbook.StepDelegate, Name: operation.Name, Delegate: &runbook.DelegateStep{
			AgentID: runbook.Value{Literal: encodedAgent}, Goal: runbook.Value{Literal: encodedGoal}, Context: context,
			Mode: runbook.DelegateReason, ResultPath: runbook.JSONPointer("/results/" + operation.Key), Next: endID,
		}}
		result.Steps[endID] = runbook.Step{Kind: runbook.StepEnd, Name: "Complete " + operation.Name, End: &runbook.EndStep{
			Outputs: map[string]runbook.Value{"result": {Ref: runbook.JSONPointer("/results/" + operation.Key)}},
		}}
		trigger := runbook.Trigger{Entrypoint: operation.Key, ObjectiveID: "agent:" + definition.ID + ":" + operation.ObjectiveKey, MaximumConcurrent: 1}
		switch operation.Wake {
		case AuthoringWakeSchedule:
			parsed := parseScheduleIntent(operation.Schedule)
			if parsed.kind != scheduleIntentExact {
				return nil, fmt.Errorf("operation %s schedule is incomplete and requires clarification", operation.Key)
			}
			schedule, err := scheduleForIntent(parsed)
			if err != nil {
				return nil, fmt.Errorf("compile operation %s schedule: %w", operation.Key, err)
			}
			trigger.Kind, trigger.Schedule = runbook.TriggerSchedule, schedule
		case AuthoringWakeEvent:
			trigger.Kind, trigger.EventType = runbook.TriggerEvent, strings.TrimSpace(operation.EventType)
		}
		if operation.ReportProgress {
			trigger.Reporting = &runbook.ReportingPolicy{
				Channel: authoringPortableKey(operation.Key + "-work"), Title: operation.Name,
				Milestones: []runbook.ReportingMilestone{runbook.ReportingStarted, runbook.ReportingApprovalRequired, runbook.ReportingCompleted, runbook.ReportingFailed},
			}
		}
		if operation.Wake != AuthoringWakeOnDemand {
			result.Triggers[operation.Key] = trigger
		}
		if operation.Approval == AuthoringApprovalRequired {
			definition.Authority.RequireApprovalAt = capability.RiskLevelRead
		}
		if operation.Approval == AuthoringApprovalStanding {
			// Standing authority requires exact operation and resource boundaries.
			// The semantic answer deliberately cannot invent those grants.
			definition.OperatingPrinciples = normalized(append(definition.OperatingPrinciples, "Use standing authority only after an exact governed grant has been reviewed and materialized."))
		}
		_ = selected
		_ = request
	}
	return result, nil
}

func compileAuthoringTeam(answer AuthoringTeamIntent, agents map[string]*agent.AgentDefinition, catalog CapabilityCatalog) (*team.Definition, []Assignment, error) {
	definition := &team.Definition{
		ID: answer.Key, Version: "1.0.0", DisplayName: strings.TrimSpace(answer.Name), Purpose: strings.TrimSpace(answer.Purpose),
		OperatingPrinciples: normalized(answer.OperatingPrinciples),
		Coordination:        team.CoordinationPolicy{MaximumSpeakersPerRound: maxInt(1, len(answer.Roles)), QuietByDefault: true, RequireRoleRelevance: true, SuppressDuplicateContent: true},
		Delegation:          team.DelegationPolicy{MaximumDepth: 4, MaximumConcurrent: maxInt(1, len(agents)), AllowPeerDelegation: true, RequireAcceptance: true, RequireCompletionReview: true},
		Approvals:           team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
		Amendments:          workforce.AmendmentPolicy{AgentMayPropose: true},
	}
	for _, objective := range answer.Objectives {
		definition.ObjectiveTemplates = append(definition.ObjectiveTemplates, workforce.ObjectiveTemplate{
			ID: objective.Key, Title: objective.Title, Goal: objective.Outcome, Priority: objective.Priority,
			SuccessCriteria: stringListObject(objective.SuccessCriteria), Constraints: stringListObject(objective.Constraints),
		})
	}
	assignments := make([]Assignment, 0)
	for _, role := range answer.Roles {
		slot := team.RoleSlot{
			ID: role.Key, DisplayName: role.Name, Purpose: role.Purpose,
			MinimumMembers: len(role.AgentKeys), MaximumMembers: len(role.AgentKeys),
			ChannelParticipation: team.RoleChannelObserveOnly,
		}
		if role.CanSpeakInChannels {
			slot.ChannelParticipation = team.RoleChannelActive
		}
		for _, key := range role.AgentKeys {
			slot.RequiredDefinitionIDs = append(slot.RequiredDefinitionIDs, agents[key].ID)
			assignments = append(assignments, Assignment{ID: role.Key + "-" + key, RoleID: role.Key, AgentDefinitionID: agents[key].ID, DisplayName: agents[key].DisplayName})
		}
		for _, catalogID := range role.SkillCatalogIDs {
			skill := catalog.Skills[catalogID]
			slot.RequiredSkillIDs = append(slot.RequiredSkillIDs, catalogID)
			slot.SkillGrants = append(slot.SkillGrants, team.RoleSkillGrant{
				SkillID: skill.ID, SkillVersion: skill.Version, CatalogID: catalogID, RuntimeIdentity: skill.RuntimeIdentity,
				AllowedActions: append([]string(nil), skill.Actions...), EnablePrompt: skill.PromptAvailable, MaximumRisk: skill.MaximumRisk,
			})
			definition.Approvals.MaximumRisk = maximumAuthoringRisk(definition.Approvals.MaximumRisk, skill.MaximumRisk)
		}
		definition.Roles = append(definition.Roles, slot)
	}
	return definition, assignments, nil
}

func compileAuthoringClarifications(values []AuthoringClarification) ([]RefinementQuestion, error) {
	result := make([]RefinementQuestion, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if !validAuthoringIntentKey(value.Key) || seen[value.Key] || strings.TrimSpace(value.Question) == "" || strings.TrimSpace(value.WhyNeeded) == "" {
			return nil, fmt.Errorf("invalid semantic clarification %q", value.Key)
		}
		seen[value.Key] = true
		answer := RefinementAnswerSchema{Kind: RefinementAnswerText, Minimum: 1, Maximum: 2048}
		if len(value.Choices) > 0 {
			answer.Kind, answer.Maximum = RefinementAnswerSingleSelect, 1
			for index, choice := range normalized(value.Choices) {
				answer.Options = append(answer.Options, RefinementQuestionOption{ID: fmt.Sprintf("choice-%d", index+1), Label: choice})
			}
		}
		result = append(result, RefinementQuestion{
			ID: "intent-" + value.Key, Category: RefinementCategoryOther, Prompt: strings.TrimSpace(value.Question), WhyNeeded: strings.TrimSpace(value.WhyNeeded),
			Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: answer,
			Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt, Evidence: "The semantic planner identified an unresolved user decision."}}, Priority: 500,
		})
	}
	return result, nil
}

func selectedSkillRisk(skill SkillCapability, actions []string) capability.RiskLevel {
	risk := skill.MaximumRisk
	if risk == "" {
		risk = capability.RiskLevelRead
	}
	if len(actions) == 0 {
		return risk
	}
	risk = capability.RiskLevelRead
	for _, action := range actions {
		risk = maximumAuthoringRisk(risk, skill.ActionRisks[action])
	}
	return risk
}

func maximumAuthoringRisk(left, right capability.RiskLevel) capability.RiskLevel {
	if riskRank(right) > riskRank(left) {
		return right
	}
	if left == "" {
		return capability.RiskLevelRead
	}
	return left
}

func stringListObject(values []string) map[string]interface{} {
	values = normalized(values)
	if len(values) == 0 {
		return nil
	}
	return map[string]interface{}{"items": values}
}

func validAuthoringIntentKey(value string) bool {
	return authoringIntentKeyPattern.MatchString(strings.TrimSpace(value))
}

func authoringPortableKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(value, "-")
	value = strings.Trim(value, "-_")
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		value = "work-" + value
	}
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
