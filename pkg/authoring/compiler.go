package authoring

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

const maximumGenerationBytes = 1 << 20

type Compiler struct {
	generator Generator
}

func NewCompiler(generator Generator) (*Compiler, error) {
	if generator == nil {
		return nil, errors.New("workforce authoring generator is required")
	}
	return &Compiler{generator: generator}, nil
}

func (c *Compiler) Compile(ctx context.Context, request GenerateRequest) (*CompileResult, error) {
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.Mode != ModeCreate && request.Mode != ModeAmend {
		return nil, errors.New("authoring mode must be create or amend")
	}
	if request.Prompt == "" {
		return nil, errors.New("authoring prompt is required")
	}
	if request.Mode == ModeAmend && request.Existing == nil {
		return nil, errors.New("amend authoring requires the existing workforce candidate")
	}
	payload, err := c.generator.Generate(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("generate workforce candidate: %w", err)
	}
	if len(payload) == 0 || len(payload) > maximumGenerationBytes {
		return nil, errors.New("generated workforce candidate must be between 1 byte and 1 MiB")
	}
	generated, decodeErr := decodeGenerationResponse(payload)
	if decodeErr != nil {
		repairer, ok := c.generator.(RepairGenerator)
		if !ok {
			return nil, fmt.Errorf("decode workforce candidate: %w", decodeErr)
		}
		payload, err = repairer.Repair(ctx, request, payload, decodeErr)
		if err != nil {
			return nil, fmt.Errorf("repair workforce candidate: %w", err)
		}
		if len(payload) == 0 || len(payload) > maximumGenerationBytes {
			return nil, errors.New("repaired workforce candidate must be between 1 byte and 1 MiB")
		}
		generated, decodeErr = decodeGenerationResponse(payload)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode repaired workforce candidate: %w", decodeErr)
		}
	}
	result := &CompileResult{
		Candidate: generated.Candidate, Assumptions: normalized(generated.Assumptions), Questions: normalized(generated.Questions),
	}
	result.Validation = validateCandidate(&result.Candidate, request.Existing)
	result.MissingRequirements = missingRequirements(&result.Candidate, request.Catalog)
	result.RiskChanges = riskChanges(request.Existing, &result.Candidate)
	result.Diff = workforceDiff(request.Existing, &result.Candidate)
	result.Valid = len(result.Validation) == 0 && len(result.MissingRequirements) == 0 && len(result.Questions) == 0
	return result, nil
}

func decodeGenerationResponse(payload []byte) (GenerationResponse, error) {
	var generated GenerationResponse
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&generated); err != nil {
		return GenerationResponse{}, err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return GenerationResponse{}, err
		}
		return GenerationResponse{}, errors.New("generated workforce candidate must contain one JSON object")
	}
	return generated, nil
}

func validateCandidate(candidate *WorkforceCandidate, existing *WorkforceCandidate) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	agents := make(map[string]*agent.AgentDefinition, len(candidate.Agents))
	for index, definition := range candidate.Agents {
		path := fmt.Sprintf("agents[%d]", index)
		if definition == nil {
			issues = append(issues, issue(path, "required", "Agent definition is required"))
			continue
		}
		if agents[definition.ID] != nil {
			issues = append(issues, issue(path+".id", "duplicate", "Agent definition id must be unique"))
		}
		agents[definition.ID] = definition
		if err := definition.Validate(); err != nil {
			issues = append(issues, issue(path, "invalid_agent", err.Error()))
		}
	}
	if candidate.Team == nil {
		if len(candidate.Agents) == 0 {
			issues = append(issues, issue("workforce", "required", "At least one Agent or Team definition is required"))
		}
		if len(candidate.Assignments) > 0 {
			issues = append(issues, issue("assignments", "team_required", "Assignments require a Team definition"))
		}
		issues = append(issues, validateInitiativeBlueprint(candidate, agents)...)
		if existing != nil && existing.Initiative != nil && (candidate.Initiative == nil || candidate.Initiative.ID != existing.Initiative.ID) {
			issues = append(issues, issue("initiative.id", "invalid_amendment_identity", "Amended Initiative must keep its id"))
		}
		return issues
	}
	if err := candidate.Team.Validate(); err != nil {
		issues = append(issues, issue("team", "invalid_team", err.Error()))
	}
	roles := make(map[string]int, len(candidate.Team.Roles))
	roleDefinitions := make(map[string]map[string]bool, len(candidate.Team.Roles))
	for _, role := range candidate.Team.Roles {
		roleDefinitions[role.ID] = stringSet(role.RequiredDefinitionIDs)
	}
	assignments := make(map[string]bool, len(candidate.Assignments))
	assignedAgents := make(map[string]bool, len(candidate.Assignments))
	for index, assignment := range candidate.Assignments {
		path := fmt.Sprintf("assignments[%d]", index)
		if strings.TrimSpace(assignment.ID) == "" || assignments[assignment.ID] {
			issues = append(issues, issue(path+".id", "invalid_assignment", "Assignment id is required and must be unique"))
		}
		assignments[assignment.ID] = true
		definition := agents[assignment.AgentDefinitionID]
		if definition == nil || assignedAgents[assignment.AgentDefinitionID] {
			issues = append(issues, issue(path+".agentDefinitionId", "invalid_assignment", "Assignment must reference one unique candidate Agent"))
		}
		assignedAgents[assignment.AgentDefinitionID] = true
		declared, ok := roleDefinitions[assignment.RoleID]
		if !ok {
			issues = append(issues, issue(path+".roleId", "unknown_role", "Assignment role is not declared by the Team"))
			continue
		}
		if definition != nil && len(declared) > 0 && !declared[definition.ID] {
			issues = append(issues, issue(path+".agentDefinitionId", "role_mismatch", "Agent definition does not satisfy the role definition constraint"))
		}
		roles[assignment.RoleID]++
	}
	for index, role := range candidate.Team.Roles {
		if roles[role.ID] < role.MinimumMembers || role.MaximumMembers > 0 && roles[role.ID] > role.MaximumMembers {
			issues = append(issues, issue(fmt.Sprintf("team.roles[%d]", index), "role_bounds", "Planned assignments do not satisfy role member bounds"))
		}
	}
	if existing != nil {
		if existing.Team != nil && (candidate.Team.ID != existing.Team.ID || candidate.Team.Version == existing.Team.Version) {
			issues = append(issues, issue("team", "invalid_amendment_identity", "Amended Team must keep its id and use a new version"))
		}
		for _, definition := range candidate.Agents {
			if definition == nil {
				continue
			}
			for _, current := range existing.Agents {
				if current != nil && current.ID == definition.ID && current.Version == definition.Version {
					issues = append(issues, issue("agents."+definition.ID, "invalid_amendment_version", "Amended Agent must use a new version"))
				}
			}
		}
	}
	issues = append(issues, validateInitiativeBlueprint(candidate, agents)...)
	if existing != nil && existing.Initiative != nil && (candidate.Initiative == nil || candidate.Initiative.ID != existing.Initiative.ID) {
		issues = append(issues, issue("initiative.id", "invalid_amendment_identity", "Amended Initiative must keep its id"))
	}
	return issues
}

func missingRequirements(candidate *WorkforceCandidate, catalog CapabilityCatalog) []MissingRequirement {
	missing := make(map[string]MissingRequirement)
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		for _, requirement := range definition.SkillRequirements {
			capability, ok := catalog.Skills[requirement.SkillID]
			if !ok {
				key := "skill:" + requirement.SkillID + ":" + definition.ID
				missing[key] = MissingRequirement{Kind: "skill", ID: requirement.SkillID, RequiredBy: "agent:" + definition.ID}
				continue
			}
			if requirement.PromptRequired && !capability.PromptAvailable {
				key := "prompt:" + requirement.SkillID + ":" + definition.ID
				missing[key] = MissingRequirement{Kind: "prompt", ID: requirement.SkillID, RequiredBy: "agent:" + definition.ID}
			}
			availableActions := stringSet(capability.Actions)
			for _, action := range requirement.RequiredActions {
				if !availableActions[action] {
					key := "action:" + requirement.SkillID + "/" + action + ":" + definition.ID
					missing[key] = MissingRequirement{Kind: "action", ID: requirement.SkillID + "/" + action, RequiredBy: "agent:" + definition.ID}
				}
			}
			for _, credential := range capability.CredentialKinds {
				if !catalog.AvailableCredentials[credential] {
					key := "credential:" + credential + ":" + definition.ID
					missing[key] = MissingRequirement{Kind: "credential", ID: credential, RequiredBy: "agent:" + definition.ID + "/skill:" + requirement.SkillID}
				}
			}
		}
	}
	if candidate.Initiative != nil {
		for _, monitor := range candidate.Initiative.SourceMonitors {
			available, ok := catalog.Skills[monitor.SkillID]
			requiredBy := "initiative:" + candidate.Initiative.ID + "/monitor:" + monitor.ID
			if !ok {
				key := "skill:" + monitor.SkillID + ":" + requiredBy
				missing[key] = MissingRequirement{Kind: "skill", ID: monitor.SkillID, RequiredBy: requiredBy}
				continue
			}
			if available.Version != monitor.SkillVersion {
				key := "version:" + monitor.SkillID + "@" + monitor.SkillVersion + ":" + requiredBy
				missing[key] = MissingRequirement{Kind: "version", ID: monitor.SkillID + "@" + monitor.SkillVersion, RequiredBy: requiredBy}
			}
			if !stringSet(available.Actions)[monitor.Action] {
				key := "action:" + monitor.SkillID + "/" + monitor.Action + ":" + requiredBy
				missing[key] = MissingRequirement{Kind: "action", ID: monitor.SkillID + "/" + monitor.Action, RequiredBy: requiredBy}
			}
			policy, policyAvailable := catalog.SourcePolicies[monitor.SourcePolicyRef]
			if !policyAvailable || policy.Reference != monitor.SourcePolicyRef {
				key := "source_policy:" + monitor.SourcePolicyRef + ":" + requiredBy
				missing[key] = MissingRequirement{Kind: "source_policy", ID: monitor.SourcePolicyRef, RequiredBy: requiredBy}
			}
		}
	}
	result := make([]MissingRequirement, 0, len(missing))
	for _, value := range missing {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind == result[j].Kind {
			if result[i].ID == result[j].ID {
				return result[i].RequiredBy < result[j].RequiredBy
			}
			return result[i].ID < result[j].ID
		}
		return result[i].Kind < result[j].Kind
	})
	return result
}

func riskChanges(existing *WorkforceCandidate, candidate *WorkforceCandidate) []RiskChange {
	if existing == nil {
		return nil
	}
	currentAgents := make(map[string]*agent.AgentDefinition, len(existing.Agents))
	for _, definition := range existing.Agents {
		if definition != nil {
			currentAgents[definition.ID] = definition
		}
	}
	changes := make([]RiskChange, 0)
	for _, definition := range candidate.Agents {
		current := currentAgents[definition.ID]
		if current != nil && current.Authority.MaximumRisk != definition.Authority.MaximumRisk {
			changes = append(changes, RiskChange{Path: "agents." + definition.ID + ".authority.maximumRisk", Before: string(current.Authority.MaximumRisk), After: string(definition.Authority.MaximumRisk), Widening: riskRank(definition.Authority.MaximumRisk) > riskRank(current.Authority.MaximumRisk)})
		}
	}
	if existing.Team != nil && candidate.Team != nil && existing.Team.Approvals.MaximumRisk != candidate.Team.Approvals.MaximumRisk {
		changes = append(changes, RiskChange{Path: "team.approvals.maximumRisk", Before: string(existing.Team.Approvals.MaximumRisk), After: string(candidate.Team.Approvals.MaximumRisk), Widening: riskRank(candidate.Team.Approvals.MaximumRisk) > riskRank(existing.Team.Approvals.MaximumRisk)})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

func workforceDiff(existing *WorkforceCandidate, candidate *WorkforceCandidate) []FieldDiff {
	if existing == nil {
		return []FieldDiff{{Path: "workforce", AfterDigest: digest(candidate)}}
	}
	before, after := map[string]interface{}{}, map[string]interface{}{}
	encoded, _ := json.Marshal(existing)
	_ = json.Unmarshal(encoded, &before)
	encoded, _ = json.Marshal(candidate)
	_ = json.Unmarshal(encoded, &after)
	fields := map[string]bool{}
	for field := range before {
		fields[field] = true
	}
	for field := range after {
		fields[field] = true
	}
	paths := make([]string, 0, len(fields))
	for field := range fields {
		paths = append(paths, field)
	}
	sort.Strings(paths)
	result := make([]FieldDiff, 0, len(paths))
	for _, path := range paths {
		beforeDigest, afterDigest := digest(before[path]), digest(after[path])
		if beforeDigest != afterDigest {
			result = append(result, FieldDiff{Path: path, BeforeDigest: beforeDigest, AfterDigest: afterDigest})
		}
	}
	return result
}

func issue(path, code, message string) ValidationIssue {
	return ValidationIssue{Path: path, Code: code, Message: message}
}

func normalized(values []string) []string {
	set := stringSet(values)
	result := make([]string, 0, len(set))
	for value := range set {
		if value != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result[value] = true
		}
	}
	return result
}

func digest(value interface{}) string {
	encoded, _ := json.Marshal(value)
	valueDigest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(valueDigest[:])
}

func riskRank(value capability.RiskLevel) int {
	switch value {
	case capability.RiskLevelRead:
		return 0
	case capability.RiskLevelWrite:
		return 1
	case capability.RiskLevelExternal:
		return 2
	case capability.RiskLevelProduction:
		return 3
	case capability.RiskLevelDestructive:
		return 4
	default:
		return -1
	}
}
