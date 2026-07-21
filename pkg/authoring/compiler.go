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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const (
	maximumGenerationBytes        = 1 << 20
	maximumSchemaRepairAttempts   = 2
	maximumPublicSchemaDiagnostic = 256
)

// SchemaGenerationError is a credential-free account of a provider response
// that remained structurally invalid after the bounded strict-schema repair
// budget. It deliberately excludes the provider payload.
type SchemaGenerationError struct {
	RepairAttempts int
	Diagnostic     string
}

func (e *SchemaGenerationError) Error() string {
	if e == nil {
		return "decode repaired workforce candidate"
	}
	return fmt.Sprintf("decode repaired workforce candidate after %d schema repair attempts: %s", e.RepairAttempts, e.Diagnostic)
}

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
	for attempt := 1; decodeErr != nil; attempt++ {
		repairer, ok := c.generator.(RepairGenerator)
		if !ok {
			return nil, fmt.Errorf("decode workforce candidate: %w", decodeErr)
		}
		if attempt > maximumSchemaRepairAttempts {
			return nil, &SchemaGenerationError{RepairAttempts: maximumSchemaRepairAttempts, Diagnostic: publicSchemaDiagnostic(decodeErr)}
		}
		repairRequest := request
		if repairRequest.InvocationKey != "" {
			repairRequest.InvocationKey = fmt.Sprintf("%s:schema:%d", repairRequest.InvocationKey, attempt)
		}
		payload, err = repairer.Repair(ctx, repairRequest, payload, decodeErr)
		if err != nil {
			return nil, fmt.Errorf("repair workforce candidate schema attempt %d: %w", attempt, err)
		}
		if len(payload) == 0 || len(payload) > maximumGenerationBytes {
			return nil, errors.New("repaired workforce candidate must be between 1 byte and 1 MiB")
		}
		generated, decodeErr = decodeGenerationResponse(payload)
	}
	extractedCommitments := extractExplicitPromptCommitments(request.Prompt)
	applyExtractedApprovalCommitments(&generated.Candidate, extractedCommitments)
	commitments, commitmentIssues := effectivePromptCommitments(request.Prompt, generated.Commitments)
	validation := append(validateCandidate(&generated.Candidate, request.Existing), commitmentIssues...)
	validation = append(validation, validatePromptCommitments(commitments, &generated.Candidate)...)
	if err := validateRefinementQuestions(generated.UnresolvedQuestions); err != nil {
		validation = append(validation, issue("unresolvedQuestions", "invalid_refinement_question", err.Error()))
	}
	if err := validateRefinementCatalog(generated.UnresolvedQuestions, request.Catalog); err != nil {
		validation = append(validation, issue("unresolvedQuestions", "invalid_refinement_catalog", err.Error()))
	}
	missing := missingRequirements(&generated.Candidate, request.Catalog)
	// Structural schema repair and deterministic contract repair have separate,
	// bounded budgets. A structurally repaired response must still receive the
	// same one semantic repair opportunity as an initially decodable response.
	if len(validation) > 0 || len(missing) > 0 {
		if repairer, ok := c.generator.(RepairGenerator); ok {
			repairReason := deterministicContractError(validation, missing)
			repairRequest := request
			if repairRequest.InvocationKey != "" {
				repairRequest.InvocationKey += ":contract:1"
			}
			if repaired, repairErr := repairer.Repair(ctx, repairRequest, payload, repairReason); repairErr == nil && len(repaired) > 0 && len(repaired) <= maximumGenerationBytes {
				if candidate, candidateErr := decodeGenerationResponse(repaired); candidateErr == nil {
					generated = candidate
				}
			}
		}
	}
	applyExtractedApprovalCommitments(&generated.Candidate, extractedCommitments)
	commitments, _ = effectivePromptCommitments(request.Prompt, generated.Commitments)
	assumptions := normalized(generated.Assumptions)
	if commitments.Activation == ActivationCommitmentInactive {
		assumptions = normalized(append(assumptions, "Compilation remains inactive; activation requires a separate governed apply operation."))
	}
	result := &CompileResult{
		Candidate: generated.Candidate, Commitments: commitments, Assumptions: assumptions, Questions: normalized(generated.Questions),
		UnresolvedQuestions: append([]RefinementQuestion(nil), generated.UnresolvedQuestions...),
	}
	result.Validation = validateCandidate(&result.Candidate, request.Existing)
	result.Validation = append(result.Validation, validatePromptCommitments(result.Commitments, &result.Candidate)...)
	if err := validateRefinementQuestions(result.UnresolvedQuestions); err != nil {
		result.Validation = append(result.Validation, issue("unresolvedQuestions", "invalid_refinement_question", err.Error()))
	}
	if err := validateRefinementCatalog(result.UnresolvedQuestions, request.Catalog); err != nil {
		result.Validation = append(result.Validation, issue("unresolvedQuestions", "invalid_refinement_catalog", err.Error()))
	}
	result.MissingRequirements = missingRequirements(&result.Candidate, request.Catalog)
	result.RiskChanges = riskChanges(request.Existing, &result.Candidate)
	result.Diff = workforceDiff(request.Existing, &result.Candidate)
	result.Valid = len(result.Validation) == 0 && len(result.MissingRequirements) == 0 && len(result.Questions) == 0 && len(result.UnresolvedQuestions) == 0
	return result, nil
}

func publicSchemaDiagnostic(err error) string {
	switch value := err.(type) {
	case *json.SyntaxError:
		return fmt.Sprintf("malformed JSON at byte %d", value.Offset)
	case *json.UnmarshalTypeError:
		field := strings.TrimSpace(value.Field)
		if field == "" {
			field = "unknown"
		}
		return truncateSchemaDiagnostic(fmt.Sprintf("field %s expects %s but received %s", field, value.Type, value.Value))
	}
	message := strings.TrimSpace(err.Error())
	const unknownPrefix = "json: unknown field \""
	if strings.HasPrefix(message, unknownPrefix) && strings.HasSuffix(message, "\"") {
		field := strings.TrimSuffix(strings.TrimPrefix(message, unknownPrefix), "\"")
		if validSchemaFieldName(field) {
			return "unknown field " + field
		}
	}
	if strings.Contains(message, "must contain one JSON object") {
		return "provider response must contain one JSON object"
	}
	return "strict JSON schema mismatch"
}

func validSchemaFieldName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func truncateSchemaDiagnostic(value string) string {
	if len(value) <= maximumPublicSchemaDiagnostic {
		return value
	}
	return value[:maximumPublicSchemaDiagnostic]
}

func deterministicContractError(validation []ValidationIssue, missing []MissingRequirement) error {
	payload, _ := json.Marshal(struct {
		Validation          []ValidationIssue    `json:"validation,omitempty"`
		MissingRequirements []MissingRequirement `json:"missingRequirements,omitempty"`
	}{Validation: validation, MissingRequirements: missing})
	return fmt.Errorf("candidate violates the deterministic authoring contract: %s", payload)
}

func decodeGenerationResponse(payload []byte) (GenerationResponse, error) {
	payload = normalizeGeneratedDurations(payload)
	payload = normalizeGeneratedRefinementBlocking(payload)
	payload = normalizeGeneratedRefinementProvenance(payload)
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

// normalizeGeneratedRefinementBlocking accepts a single canonical blocking
// scope as shorthand for the contract's array form. This conversion is
// lossless and deliberately limited to known scope values; unknown strings and
// all other shapes remain untouched so strict decoding and validation fail
// closed.
func normalizeGeneratedRefinementBlocking(payload []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document interface{}
	if err := decoder.Decode(&document); err != nil {
		return payload
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return payload
	}
	root, ok := document.(map[string]interface{})
	if !ok {
		return payload
	}
	questions, ok := root["unresolvedQuestions"].([]interface{})
	if !ok {
		return payload
	}
	changed := false
	for _, rawQuestion := range questions {
		question, ok := rawQuestion.(map[string]interface{})
		if !ok {
			continue
		}
		value, ok := question["blocking"].(string)
		if !ok {
			continue
		}
		scope := RefinementBlockingScope(strings.TrimSpace(value))
		if !validRefinementBlockingScope(scope) {
			continue
		}
		question["blocking"] = []interface{}{string(scope)}
		changed = true
	}
	if !changed {
		return payload
	}
	normalized, err := json.Marshal(document)
	if err != nil {
		return payload
	}
	return normalized
}

func validRefinementBlockingScope(scope RefinementBlockingScope) bool {
	switch scope {
	case RefinementBlocksCandidate, RefinementBlocksEvaluation, RefinementBlocksApply:
		return true
	default:
		return false
	}
}

// normalizeGeneratedRefinementProvenance accepts the unambiguous shorthand
// forms "prompt" and ["prompt", "catalog"] at the one schema location where
// each value maps losslessly to a provenance object with no reference or
// evidence. Providers frequently collapse arrays of single-field objects even
// after schema repair. Unknown strings and object shapes remain untouched so
// the strict decoder and refinement validator continue to fail closed.
func normalizeGeneratedRefinementProvenance(payload []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document interface{}
	if err := decoder.Decode(&document); err != nil {
		return payload
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return payload
	}
	root, ok := document.(map[string]interface{})
	if !ok {
		return payload
	}
	questions, ok := root["unresolvedQuestions"].([]interface{})
	if !ok {
		return payload
	}
	changed := false
	for _, rawQuestion := range questions {
		question, ok := rawQuestion.(map[string]interface{})
		if !ok {
			continue
		}
		normalized, ok := normalizedRefinementProvenanceShorthand(question["provenance"])
		if !ok {
			continue
		}
		question["provenance"] = normalized
		changed = true
	}
	if !changed {
		return payload
	}
	normalized, err := json.Marshal(document)
	if err != nil {
		return payload
	}
	return normalized
}

func normalizedRefinementProvenanceShorthand(raw interface{}) ([]interface{}, bool) {
	values := make([]string, 0, 1)
	switch value := raw.(type) {
	case string:
		values = append(values, value)
	case []interface{}:
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			values = append(values, text)
		}
	default:
		return nil, false
	}
	if len(values) == 0 {
		return nil, false
	}
	result := make([]interface{}, 0, len(values))
	for _, value := range values {
		kind := RefinementProvenanceKind(strings.TrimSpace(value))
		if !validRefinementProvenanceKind(kind) {
			return nil, false
		}
		result = append(result, map[string]interface{}{"kind": string(kind)})
	}
	return result, true
}

func validRefinementProvenanceKind(kind RefinementProvenanceKind) bool {
	switch kind {
	case RefinementProvenancePrompt, RefinementProvenanceCatalog, RefinementProvenanceSkill,
		RefinementProvenanceCredential, RefinementProvenancePolicy, RefinementProvenanceRuntime:
		return true
	default:
		return false
	}
}

// normalizeGeneratedDurations accepts unambiguous human duration strings only
// at the portable schema's duration fields. Providers commonly emit values such
// as "24h" or "30d" despite an integer nanosecond contract. The canonical
// candidate remains numeric, while every other field still passes through the
// strict decoder unchanged and therefore fails closed on a type mismatch.
func normalizeGeneratedDurations(payload []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document interface{}
	if err := decoder.Decode(&document); err != nil {
		return payload
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return payload
	}
	root, ok := document.(map[string]interface{})
	if !ok {
		return payload
	}
	candidate, _ := root["candidate"].(map[string]interface{})
	agents, _ := candidate["agents"].([]interface{})
	for _, rawAgent := range agents {
		agentDefinition, _ := rawAgent.(map[string]interface{})
		normalizeDurationField(agentDefinition, "memory", "retention")
		normalizeDurationField(agentDefinition, "escalation", "afterDuration")
	}
	teamDefinition, _ := candidate["team"].(map[string]interface{})
	normalizeDurationField(teamDefinition, "sharedContext", "retention")
	normalized, err := json.Marshal(document)
	if err != nil {
		return payload
	}
	return normalized
}

func normalizeDurationField(parent map[string]interface{}, objectKey, fieldKey string) {
	object, _ := parent[objectKey].(map[string]interface{})
	raw, ok := object[fieldKey].(string)
	if !ok {
		return
	}
	duration, err := parseGeneratedDuration(raw)
	if err == nil {
		object[fieldKey] = duration.Nanoseconds()
	}
}

func parseGeneratedDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if duration, err := time.ParseDuration(raw); err == nil {
		return duration, nil
	}
	if len(raw) < 2 {
		return 0, errors.New("invalid duration")
	}
	unit := 24 * time.Hour
	switch raw[len(raw)-1] {
	case 'd':
	case 'w':
		unit *= 7
	default:
		return 0, errors.New("invalid duration")
	}
	count, err := strconv.ParseInt(raw[:len(raw)-1], 10, 64)
	if err != nil || count > int64((1<<63-1)/unit) || count < int64((-1<<63)/unit) {
		return 0, errors.New("invalid duration")
	}
	return time.Duration(count) * unit, nil
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
		issues = append(issues, validateObjectiveTemplateCadences(path+".objectiveTemplates", definition.ObjectiveTemplates)...)
		issues = append(issues, validateObjectiveTemplateEventRules(path+".objectiveTemplates", definition.ObjectiveTemplates)...)
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
	hasSpeakingRole := false
	for _, role := range candidate.Team.Roles {
		if role.ChannelParticipation == "" || role.ChannelParticipation == team.RoleChannelActive {
			hasSpeakingRole = true
			break
		}
	}
	if !hasSpeakingRole {
		issues = append(issues, issue("team.roles", "no_speaking_role", "A prompt-created Team requires at least one role with active channel participation"))
	}
	issues = append(issues, validateObjectiveTemplateCadences("team.objectiveTemplates", candidate.Team.ObjectiveTemplates)...)
	issues = append(issues, validateObjectiveTemplateEventRules("team.objectiveTemplates", candidate.Team.ObjectiveTemplates)...)
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
			if capability.Readiness == SkillReadinessNeedsInstallation || capability.Readiness == SkillReadinessUnavailable {
				kind := "skill_installation"
				if capability.Readiness == SkillReadinessUnavailable {
					kind = "skill_unavailable"
				}
				key := kind + ":" + requirement.SkillID + ":" + definition.ID
				missing[key] = MissingRequirement{Kind: kind, ID: requirement.SkillID, RequiredBy: "agent:" + definition.ID}
			}
			if capability.Readiness == SkillReadinessNeedsBinding {
				key := "skill_binding:" + requirement.SkillID + ":" + definition.ID
				missing[key] = MissingRequirement{Kind: "skill_binding", ID: requirement.SkillID, RequiredBy: "agent:" + definition.ID}
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
		objectives := candidateObjectiveTemplates(candidate)
		for _, monitor := range candidate.Initiative.SourceMonitors {
			available, ok := catalog.Skills[monitor.SkillID]
			requiredBy := "initiative:" + candidate.Initiative.ID + "/monitor:" + monitor.ID
			if !ok {
				key := "skill:" + monitor.SkillID + ":" + requiredBy
				missing[key] = MissingRequirement{Kind: "skill", ID: monitor.SkillID, RequiredBy: requiredBy}
				continue
			}
			if available.Readiness == SkillReadinessNeedsInstallation || available.Readiness == SkillReadinessUnavailable {
				kind := "skill_installation"
				if available.Readiness == SkillReadinessUnavailable {
					kind = "skill_unavailable"
				}
				key := kind + ":" + monitor.SkillID + ":" + requiredBy
				missing[key] = MissingRequirement{Kind: kind, ID: monitor.SkillID, RequiredBy: requiredBy}
			}
			if available.Readiness == SkillReadinessNeedsBinding {
				key := "skill_binding:" + monitor.SkillID + ":" + requiredBy
				missing[key] = MissingRequirement{Kind: "skill_binding", ID: monitor.SkillID, RequiredBy: requiredBy}
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
			} else if !sourceMonitorWithinPolicy(objectives[monitor.ObjectiveRef], policy) {
				key := "source_scope:" + monitor.SourcePolicyRef + ":" + requiredBy
				missing[key] = MissingRequirement{Kind: "source_scope", ID: monitor.SourcePolicyRef, RequiredBy: requiredBy}
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

func sourceMonitorWithinPolicy(template *workforce.ObjectiveTemplate, policy SourcePolicyCapability) bool {
	if template == nil || policy.MaximumItems < 1 {
		return false
	}
	runTemplate, _ := template.Cadence["runTemplate"].(map[string]interface{})
	capability, _ := runTemplate["capability"].(map[string]interface{})
	inputs, _ := capability["inputs"].(map[string]interface{})
	rawURL, _ := inputs["url"].(string)
	maximumItems, ok := jsonInteger(inputs["maxItems"])
	if !ok || maximumItems < 1 || maximumItems > int64(policy.MaximumItems) {
		return false
	}
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || target.Scheme != "https" || target.User != nil || target.Fragment != "" || target.Hostname() == "" || target.Port() != "" && target.Port() != "443" {
		return false
	}
	for _, source := range policy.Sources {
		if !strings.EqualFold(strings.TrimSpace(source.Host), target.Hostname()) {
			continue
		}
		if len(source.PathPrefixes) == 0 {
			return true
		}
		for _, prefix := range source.PathPrefixes {
			if strings.HasPrefix(target.EscapedPath(), strings.TrimSpace(prefix)) {
				return true
			}
		}
	}
	return false
}

func jsonInteger(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		integer := int64(typed)
		return integer, float64(integer) == typed
	case json.Number:
		integer, err := typed.Int64()
		return integer, err == nil
	default:
		return 0, false
	}
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
