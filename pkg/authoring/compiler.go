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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/team"
)

const (
	maximumGenerationBytes        = 1 << 20
	maximumSchemaRepairAttempts   = 2
	maximumContractRepairAttempts = 2
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

// ContractGenerationError is a credential-free account of provider output
// that remained semantically unsafe after bounded deterministic repair. Unlike
// an incomplete but well-formed candidate, an invalid typed refinement cannot
// be persisted because clients cannot answer or faithfully project it.
type ContractGenerationError struct {
	RepairAttempts int
	Diagnostic     string
}

func (e *ContractGenerationError) Error() string {
	if e == nil {
		return "repair workforce candidate contract"
	}
	return fmt.Sprintf("repair workforce candidate contract after %d repair attempts: %s", e.RepairAttempts, e.Diagnostic)
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
	return c.CompileWithProgress(ctx, request, nil)
}

// CompileWithProgress compiles a candidate while reporting bounded,
// credential-free phase changes. Observers must return quickly; compilation
// correctness never depends on observation succeeding.
func (c *Compiler) CompileWithProgress(ctx context.Context, request GenerateRequest, observe CompileProgressObserver) (*CompileResult, error) {
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.Mode != ModeCreate && request.Mode != ModeAmend {
		return nil, errors.New("authoring mode must be create or amend")
	}
	if request.Prompt == "" {
		return nil, errors.New("authoring prompt is required")
	}
	if err := ValidateAuthoringPrompt(request.Prompt); err != nil {
		return nil, err
	}
	if request.Mode == ModeAmend && request.Existing == nil {
		return nil, errors.New("amend authoring requires the existing workforce candidate")
	}
	if err := ValidateCapabilityCatalog(request.Catalog); err != nil {
		return nil, fmt.Errorf("authoring capability catalog: %w", err)
	}
	reportCompileProgress(observe, CompilePhaseCapabilityResolve, 1, 1)
	request.CompositionRequirements = deriveRuntimeCompositionRequirements(request.Prompt, request.Catalog)
	reportCompileProgress(observe, CompilePhaseProviderRequest, 1, 1)
	payload, err := c.generator.Generate(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("generate workforce candidate: %w", err)
	}
	if len(payload) == 0 || len(payload) > maximumGenerationBytes {
		return nil, errors.New("generated workforce candidate must be between 1 byte and 1 MiB")
	}
	reportCompileProgress(observe, CompilePhaseCandidateValidate, 1, 1)
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
		reportCompileProgress(observe, CompilePhaseSchemaRepair, attempt, maximumSchemaRepairAttempts)
		payload, err = repairer.Repair(ctx, repairRequest, payload, decodeErr)
		if err != nil {
			return nil, fmt.Errorf("repair workforce candidate schema attempt %d: %w", attempt, err)
		}
		if len(payload) == 0 || len(payload) > maximumGenerationBytes {
			return nil, errors.New("repaired workforce candidate must be between 1 byte and 1 MiB")
		}
		reportCompileProgress(observe, CompilePhaseCandidateValidate, 1, 1)
		generated, decodeErr = decodeGenerationResponse(payload)
	}
	if err := ValidateWorkforceCandidateSensitiveInput(&generated.Candidate); err != nil {
		return nil, err
	}
	materializationIssues := materializeAnsweredCapabilitySourceScopes(&generated.Candidate, request)
	synthesizeCapabilityNeedRefinements(&generated, request)
	scheduleIntentIssues := enforceScheduleIntentAuthority(&generated, request)
	if hasScheduleIntentQuestion(generated.UnresolvedQuestions) {
		materializationIssues = deferScheduleBlockedMaterializationIssues(materializationIssues)
	}
	extractedCommitments := extractExplicitPromptCommitments(request.Prompt)
	validateGenerated := func() (PromptCommitments, []ValidationIssue, []MissingRequirement) {
		materializeDefaultAgentSkillAuthority(&generated.Candidate)
		applyAuthorityConstraint(&generated.Candidate, request.Catalog.AuthorityConstraint)
		applyExtractedApprovalCommitments(&generated.Candidate, extractedCommitments)
		commitments, commitmentIssues := effectivePromptCommitments(request.Prompt, generated.Commitments)
		applyActivationCommitment(&generated.Candidate, commitments)
		deferInactiveCredentialRefinements(&generated)
		validation := append(validateCandidate(&generated.Candidate, request.Existing), commitmentIssues...)
		validation = append(validation, validateConversationComposition(&generated.Candidate, request)...)
		validation = append(validation, materializationIssues...)
		validation = append(validation, scheduleIntentIssues...)
		validation = append(validation, validateAnsweredCapabilityNeeds(&generated.Candidate, request)...)
		if len(materializationIssues) == 0 && !hasScheduleIntentQuestion(generated.UnresolvedQuestions) {
			validation = append(validation, validateCapabilitySourceScopeFulfillment(&generated.Candidate, request)...)
		}
		validation = append(validation, validateSelectedSkillActionRisks(&generated.Candidate, request.Catalog)...)
		validation = append(validation, validateObjectiveCapabilityInputs(&generated.Candidate, request.Catalog, false)...)
		validation = append(validation, validateRunbookActionContracts(&generated.Candidate, request.Catalog)...)
		validation = append(validation, ValidateCandidateAuthorityConstraint(&generated.Candidate, request.Catalog.AuthorityConstraint)...)
		validation = append(validation, validatePromptCommitments(commitments, &generated.Candidate)...)
		if err := validateRefinementQuestions(generated.UnresolvedQuestions); err != nil {
			validation = append(validation, issue("unresolvedQuestions", "invalid_refinement_question", err.Error()))
		}
		if err := validateRefinementCatalog(generated.UnresolvedQuestions, request.Catalog); err != nil {
			validation = append(validation, issue("unresolvedQuestions", "invalid_refinement_catalog", err.Error()))
		}
		return commitments, validation, missingRequirements(&generated.Candidate, request.Catalog)
	}
	commitments, validation, missing := validateGenerated()
	repairableMissing := providerRepairableMissingRequirements(&generated.Candidate, missing, request)
	// Structural schema repair and deterministic contract repair have separate,
	// bounded budgets. Every semantic repair is revalidated before it can replace
	// the canonical result; a second bounded attempt receives the new diagnostic
	// instead of persisting a still-invalid typed refinement.
	contractRepairAttempts := 0
	if repairer, ok := c.generator.(RepairGenerator); ok {
		repairReason := deterministicContractError(validation, repairableMissing)
		for attempt := 1; attempt <= maximumContractRepairAttempts && (len(validation) > 0 || len(repairableMissing) > 0); attempt++ {
			contractRepairAttempts = attempt
			repairRequest := request
			if repairRequest.InvocationKey != "" {
				repairRequest.InvocationKey = fmt.Sprintf("%s:contract:%d", repairRequest.InvocationKey, attempt)
			}
			reportCompileProgress(observe, CompilePhaseContractRepair, attempt, maximumContractRepairAttempts)
			repaired, repairErr := repairer.Repair(ctx, repairRequest, payload, repairReason)
			if repairErr != nil || len(repaired) == 0 || len(repaired) > maximumGenerationBytes {
				break
			}
			payload = repaired
			reportCompileProgress(observe, CompilePhaseCandidateValidate, 1, 1)
			candidate, candidateErr := decodeGenerationResponse(repaired)
			if candidateErr != nil {
				repairReason = candidateErr
				continue
			}
			generated = candidate
			if err := ValidateWorkforceCandidateSensitiveInput(&generated.Candidate); err != nil {
				return nil, err
			}
			materializationIssues = materializeAnsweredCapabilitySourceScopes(&generated.Candidate, request)
			synthesizeCapabilityNeedRefinements(&generated, request)
			scheduleIntentIssues = enforceScheduleIntentAuthority(&generated, request)
			if hasScheduleIntentQuestion(generated.UnresolvedQuestions) {
				materializationIssues = deferScheduleBlockedMaterializationIssues(materializationIssues)
			}
			commitments, validation, missing = validateGenerated()
			repairableMissing = providerRepairableMissingRequirements(&generated.Candidate, missing, request)
			repairReason = deterministicContractError(validation, repairableMissing)
		}
	}
	assumptions := normalized(generated.Assumptions)
	if commitments.Activation == ActivationCommitmentInactive {
		assumptions = normalized(append(assumptions, "Atomic apply remains inactive by creating non-executing resources; activation requires a separate governed command."))
	}
	result := &CompileResult{
		Candidate: generated.Candidate, Commitments: commitments, Assumptions: assumptions,
		UnresolvedQuestions: append([]RefinementQuestion(nil), generated.UnresolvedQuestions...),
	}
	result.Validation = validateCandidate(&result.Candidate, request.Existing)
	result.Validation = append(result.Validation, validateConversationComposition(&result.Candidate, request)...)
	result.Validation = append(result.Validation, materializationIssues...)
	result.Validation = append(result.Validation, scheduleIntentIssues...)
	result.Validation = append(result.Validation, validateAnsweredCapabilityNeeds(&result.Candidate, request)...)
	if len(materializationIssues) == 0 && !hasScheduleIntentQuestion(result.UnresolvedQuestions) {
		result.Validation = append(result.Validation, validateCapabilitySourceScopeFulfillment(&result.Candidate, request)...)
	}
	result.Validation = append(result.Validation, validateSelectedSkillActionRisks(&result.Candidate, request.Catalog)...)
	result.Validation = append(result.Validation, validateObjectiveCapabilityInputs(&result.Candidate, request.Catalog, false)...)
	result.Validation = append(result.Validation, validateRunbookActionContracts(&result.Candidate, request.Catalog)...)
	result.Validation = append(result.Validation, ValidateCandidateAuthorityConstraint(&result.Candidate, request.Catalog.AuthorityConstraint)...)
	result.Validation = append(result.Validation, validatePromptCommitments(result.Commitments, &result.Candidate)...)
	refinementValidation := make([]ValidationIssue, 0, 2)
	if err := validateRefinementQuestions(result.UnresolvedQuestions); err != nil {
		refinementValidation = append(refinementValidation, issue("unresolvedQuestions", "invalid_refinement_question", err.Error()))
	}
	if err := validateRefinementCatalog(result.UnresolvedQuestions, request.Catalog); err != nil {
		refinementValidation = append(refinementValidation, issue("unresolvedQuestions", "invalid_refinement_catalog", err.Error()))
	}
	if len(refinementValidation) > 0 {
		return nil, &ContractGenerationError{
			RepairAttempts: contractRepairAttempts,
			Diagnostic:     publicContractDiagnostic(refinementValidation),
		}
	}
	result.MissingRequirements = missingRequirements(&result.Candidate, request.Catalog)
	result.SourcePolicyProposals = sourcePolicyProposals(&result.Candidate, result.MissingRequirements, request.Catalog)
	result.RiskChanges = riskChanges(request.Existing, &result.Candidate)
	result.Diff = workforceDiff(request.Existing, &result.Candidate)
	result.Valid = len(result.Validation) == 0 && len(result.MissingRequirements) == 0 && len(result.UnresolvedQuestions) == 0
	return result, nil
}

// deferInactiveCredentialRefinements keeps creation and activation as separate
// governed boundaries. An explicitly inactive workforce cannot execute a
// credentialed Skill, so selecting an execution credential is setup for the
// later activation ChangeSet rather than information required to create the
// inert resources. PrepareActivation recomputes the exact named credential
// slots from the then-current catalog and fails closed until they are placed.
func deferInactiveCredentialRefinements(generated *GenerationResponse) {
	if generated == nil || generated.Candidate.Activation != WorkforceActivationInactive {
		return
	}
	filtered := generated.UnresolvedQuestions[:0]
	for _, question := range generated.UnresolvedQuestions {
		if question.Category == RefinementCategoryCredential ||
			question.Answer.Kind == RefinementAnswerCredentialReference {
			continue
		}
		filtered = append(filtered, question)
	}
	generated.UnresolvedQuestions = filtered
}

// materializeDefaultAgentSkillAuthority gives an omitted allowlist the only
// safe useful meaning available from the typed definition: authorize the
// Agent's exact non-optional Skill requirements. An explicitly supplied empty
// or narrower list remains an intentional restriction and is validated below
// instead of being silently widened.
func materializeDefaultAgentSkillAuthority(candidate *WorkforceCandidate) {
	if candidate == nil {
		return
	}
	for _, definition := range candidate.Agents {
		if definition == nil || definition.Authority.AllowedSkillIDs != nil {
			continue
		}
		for _, requirement := range definition.SkillRequirements {
			if !requirement.Optional {
				definition.Authority.AllowedSkillIDs = append(definition.Authority.AllowedSkillIDs, requirement.SkillID)
			}
		}
		definition.Authority.AllowedSkillIDs = normalized(definition.Authority.AllowedSkillIDs)
	}
}

func validateAgentSkillAuthority(path string, definition *agent.AgentDefinition) []ValidationIssue {
	if definition == nil {
		return nil
	}
	allowed := stringSet(definition.Authority.AllowedSkillIDs)
	issues := make([]ValidationIssue, 0)
	for index, requirement := range definition.SkillRequirements {
		if requirement.Optional || allowed[requirement.SkillID] {
			continue
		}
		issues = append(issues, issue(
			fmt.Sprintf("%s.skillRequirements[%d].skillId", path, index),
			"required_skill_not_authorized",
			fmt.Sprintf("Required Skill %s must be present in authority.allowedSkillIds", requirement.SkillID),
		))
	}
	return issues
}

// validateSelectedSkillActionRisks keeps an Agent's authority aligned with the
// exact catalog actions it selected. Skill MaximumRisk describes the broadest
// operation the Skill exposes and must not force an Agent to receive unrelated
// destructive authority; ActionRisks provides the narrower deterministic fact
// needed for contract repair before policy review and binding.
func validateSelectedSkillActionRisks(candidate *WorkforceCandidate, catalog CapabilityCatalog) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	issues := make([]ValidationIssue, 0)
	for agentIndex, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		authorityRank := riskRank(definition.Authority.MaximumRisk)
		for requirementIndex, requirement := range definition.SkillRequirements {
			skill, exists := catalog.Skills[requirement.SkillID]
			if !exists {
				continue
			}
			for _, action := range requirement.RequiredActions {
				actionRisk, known := skill.ActionRisks[action]
				if !known || riskRank(actionRisk) <= authorityRank {
					continue
				}
				issues = append(issues, issue(
					fmt.Sprintf("agents[%d].authority.maximumRisk", agentIndex),
					"skill_action_risk_exceeded",
					fmt.Sprintf("Skill requirement %d action %s requires %s risk, but Agent %s permits at most %s; widen Agent authority to %s and preserve the configured approval threshold", requirementIndex, action, actionRisk, definition.ID, definition.Authority.MaximumRisk, actionRisk),
				))
			}
		}
	}
	return issues
}

func reportCompileProgress(observe CompileProgressObserver, phase CompilePhase, attempt, maximumAttempts int) {
	if observe == nil {
		return
	}
	observe(CompileProgress{Phase: phase, Attempt: attempt, MaximumAttempts: maximumAttempts})
}

// applyAuthorityConstraint narrows only the approval threshold that can be
// derived without interpretation from a validated host projection. It never
// creates Agents, increases authority, or silently lowers maximum risk.
func applyAuthorityConstraint(candidate *WorkforceCandidate, constraint *AuthorityConstraint) {
	if candidate == nil || constraint == nil || riskRank(constraint.RequireApprovalAt) < 0 {
		return
	}
	requiredRank := riskRank(constraint.RequireApprovalAt)
	for _, definition := range candidate.Agents {
		if definition == nil || riskRank(definition.Authority.MaximumRisk) < requiredRank {
			continue
		}
		current := definition.Authority.RequireApprovalAt
		if current == "" || riskRank(current) > requiredRank {
			definition.Authority.RequireApprovalAt = constraint.RequireApprovalAt
		}
	}
}

// ValidateCandidateAuthorityConstraint rechecks a generated candidate against
// a current host authority projection. Hosts use the same deterministic
// contract at compilation, readiness evaluation, and final mutation time.
func ValidateCandidateAuthorityConstraint(candidate *WorkforceCandidate, constraint *AuthorityConstraint) []ValidationIssue {
	if candidate == nil || constraint == nil {
		return nil
	}
	maximumRank := riskRank(constraint.MaximumRisk)
	approvalRank := riskRank(constraint.RequireApprovalAt)
	issues := make([]ValidationIssue, 0)
	for index, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		agentMaximumRank := riskRank(definition.Authority.MaximumRisk)
		if maximumRank >= 0 && agentMaximumRank > maximumRank {
			issues = append(issues, issue(
				fmt.Sprintf("agents[%d].authority.maximumRisk", index),
				"authority_maximum_risk_exceeded",
				fmt.Sprintf("Host authority constraint %s@%s permits maximum risk %s", constraint.ID, constraint.Version, constraint.MaximumRisk),
			))
		}
		agentApprovalRank := riskRank(definition.Authority.RequireApprovalAt)
		if approvalRank >= 0 && agentMaximumRank >= approvalRank && (agentApprovalRank < 0 || agentApprovalRank > approvalRank) {
			issues = append(issues, issue(
				fmt.Sprintf("agents[%d].authority.requireApprovalAt", index),
				"authority_approval_threshold_exceeded",
				fmt.Sprintf("Host authority constraint %s@%s requires approval at %s risk or earlier", constraint.ID, constraint.Version, constraint.RequireApprovalAt),
			))
		}
	}
	return issues
}

func publicContractDiagnostic(validation []ValidationIssue) string {
	if len(validation) == 0 {
		return "deterministic contract mismatch"
	}
	first := validation[0]
	diagnostic := strings.TrimSpace(first.Code)
	if message := strings.TrimSpace(first.Message); message != "" {
		if diagnostic != "" {
			diagnostic += ": "
		}
		diagnostic += message
	}
	if diagnostic == "" {
		diagnostic = "deterministic contract mismatch"
	}
	return truncateSchemaDiagnostic(diagnostic)
}

func publicSchemaDiagnostic(err error) string {
	var contextual *strictJSONSchemaError
	if errors.As(err, &contextual) {
		return truncateSchemaDiagnostic(contextual.diagnostic)
	}
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
	payload = normalizeGeneratedResponseMetadataPlacement(payload)
	payload = normalizeGeneratedDefinitionVersions(payload)
	payload = normalizeGeneratedDurations(payload)
	payload = normalizeGeneratedDefinitionProvenance(payload)
	payload = normalizeGeneratedRefinementBlocking(payload)
	payload = normalizeGeneratedRefinementProvenance(payload)
	payload = normalizeGeneratedRefinementDependencies(payload)
	payload = normalizeGeneratedRunbookValues(payload)
	var generated GenerationResponse
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&generated); err != nil {
		return GenerationResponse{}, contextualizeStrictJSONError(payload, err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return GenerationResponse{}, err
		}
		return GenerationResponse{}, errors.New("generated workforce candidate must contain one JSON object")
	}
	normalizeGeneratedCredentialReferenceOptions(&generated)
	return generated, nil
}

// normalizeGeneratedResponseMetadataPlacement lifts exact response-level
// metadata when a provider has placed it under candidate. This is a lossless
// structural correction for the three known fields and their exact JSON
// shapes. Existing response-level values, invalid shapes, a non-object
// candidate, and every other unknown candidate field remain untouched and are
// rejected by strict decoding.
func normalizeGeneratedResponseMetadataPlacement(payload []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document map[string]interface{}
	if err := decoder.Decode(&document); err != nil {
		return payload
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return payload
	}
	candidate, ok := document["candidate"].(map[string]interface{})
	if !ok {
		return payload
	}
	validShape := map[string]func(interface{}) bool{
		"commitments": func(value interface{}) bool {
			_, valid := value.(map[string]interface{})
			return valid
		},
		"assumptions": func(value interface{}) bool {
			_, valid := value.([]interface{})
			return valid
		},
		"unresolvedQuestions": func(value interface{}) bool {
			_, valid := value.([]interface{})
			return valid
		},
	}
	changed := false
	for _, field := range []string{"commitments", "assumptions", "unresolvedQuestions"} {
		if _, exists := document[field]; exists {
			continue
		}
		value, exists := candidate[field]
		if !exists || !validShape[field](value) {
			continue
		}
		delete(candidate, field)
		document[field] = value
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

// normalizeGeneratedDefinitionProvenance accepts the common provider alias
// provenance.kind only where the portable Agent/Team definition contract uses
// provenance.source. The alias is lossless because Source is descriptive
// provenance rather than authority. Existing source, unknown siblings,
// whitespace changes, and non-string values remain untouched for strict JSON
// decoding to reject.
func normalizeGeneratedDefinitionProvenance(payload []byte) []byte {
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
	candidate, ok := root["candidate"].(map[string]interface{})
	if !ok {
		return payload
	}
	changed := false
	if agents, ok := candidate["agents"].([]interface{}); ok {
		for _, rawAgent := range agents {
			agent, ok := rawAgent.(map[string]interface{})
			if ok && normalizeDefinitionProvenanceKindAlias(agent["provenance"]) {
				changed = true
			}
		}
	}
	if team, ok := candidate["team"].(map[string]interface{}); ok && normalizeDefinitionProvenanceKindAlias(team["provenance"]) {
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

func normalizeDefinitionProvenanceKindAlias(raw interface{}) bool {
	provenance, ok := raw.(map[string]interface{})
	if !ok {
		return false
	}
	if _, exists := provenance["source"]; exists {
		return false
	}
	kind, ok := provenance["kind"].(string)
	if !ok || kind == "" || kind != strings.TrimSpace(kind) {
		return false
	}
	for key := range provenance {
		switch key {
		case "kind", "reference", "createdBy", "derivedFrom":
		default:
			return false
		}
	}
	delete(provenance, "kind")
	provenance["source"] = kind
	return true
}

// normalizeGeneratedRunbookValues canonicalizes unambiguous scalar shorthand
// only at fields whose declared portable type is runbook.Value. A JSON Pointer
// string becomes a ref and every other primitive becomes a literal. Objects
// remain untouched so misspelled Value fields still fail strict decoding.
func normalizeGeneratedRunbookValues(payload []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document interface{}
	if err := decoder.Decode(&document); err != nil {
		return payload
	}
	normalized, changed := normalizeRunbookValuesAtType(document, reflect.TypeOf(GenerationResponse{}), 0)
	if !changed {
		return payload
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return payload
	}
	return encoded
}

func normalizeRunbookValuesAtType(value interface{}, expected reflect.Type, depth int) (interface{}, bool) {
	if expected == nil || depth > 64 {
		return value, false
	}
	for expected.Kind() == reflect.Pointer {
		expected = expected.Elem()
	}
	if expected == reflect.TypeOf(runbook.Value{}) {
		if _, isObject := value.(map[string]interface{}); isObject {
			return value, false
		}
		if reference, ok := value.(string); ok && generatedRunbookReferenceShorthand(reference) {
			return map[string]interface{}{"ref": reference}, true
		}
		return map[string]interface{}{"literal": value}, true
	}
	switch expected.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]interface{})
		if !ok {
			return value, false
		}
		fields := jsonStructFields(expected)
		changed := false
		for name, child := range object {
			childType, known := fields[name]
			if !known {
				continue
			}
			normalized, childChanged := normalizeRunbookValuesAtType(child, childType, depth+1)
			if childChanged {
				object[name] = normalized
				changed = true
			}
		}
		return object, changed
	case reflect.Slice, reflect.Array:
		items, ok := value.([]interface{})
		if !ok {
			return value, false
		}
		changed := false
		for index, child := range items {
			normalized, childChanged := normalizeRunbookValuesAtType(child, expected.Elem(), depth+1)
			if childChanged {
				items[index] = normalized
				changed = true
			}
		}
		return items, changed
	case reflect.Map:
		object, ok := value.(map[string]interface{})
		if !ok || expected.Key().Kind() != reflect.String {
			return value, false
		}
		changed := false
		for name, child := range object {
			normalized, childChanged := normalizeRunbookValuesAtType(child, expected.Elem(), depth+1)
			if childChanged {
				object[name] = normalized
				changed = true
			}
		}
		return object, changed
	default:
		return value, false
	}
}

func generatedRunbookReferenceShorthand(value string) bool {
	for _, root := range []string{"/input", "/results", "/context"} {
		if value == root || strings.HasPrefix(value, root+"/") {
			return true
		}
	}
	return false
}

// strictJSONSchemaError preserves the decoder failure for errors.Is/As while
// giving the bounded repair provider an exact, value-free location. Go's JSON
// decoder reports only an unknown field name, which is ambiguous in a deeply
// nested workforce document (for example, "id" is valid in many objects but
// not in Agent skill requirements).
type strictJSONSchemaError struct {
	cause      error
	diagnostic string
}

func (e *strictJSONSchemaError) Error() string { return e.diagnostic }
func (e *strictJSONSchemaError) Unwrap() error { return e.cause }

type unknownJSONFieldLocation struct {
	path       string
	allowed    []string
	correction string
}

func contextualizeStrictJSONError(payload []byte, decodeErr error) error {
	if decodeErr == nil {
		return nil
	}
	var typeError *json.UnmarshalTypeError
	if errors.As(decodeErr, &typeError) && typeError.Type == reflect.TypeOf(runbook.Value{}) {
		field := strings.TrimSpace(typeError.Field)
		if field == "" {
			field = "unknown"
		}
		diagnostic := fmt.Sprintf(
			"field %s expects a Runbook Value object, not %s; use exactly one of {\"ref\":\"<JSON Pointer>\"}, {\"literal\":<JSON value>}, or {\"template\":[{\"text\":\"...\"} or {\"ref\":\"<JSON Pointer>\"}]}",
			field, typeError.Value,
		)
		return &strictJSONSchemaError{cause: decodeErr, diagnostic: diagnostic}
	}
	message := strings.TrimSpace(decodeErr.Error())
	const unknownPrefix = "json: unknown field \""
	if !strings.HasPrefix(message, unknownPrefix) || !strings.HasSuffix(message, "\"") {
		return decodeErr
	}
	field := strings.TrimSuffix(strings.TrimPrefix(message, unknownPrefix), "\"")
	if !validSchemaFieldName(field) {
		return decodeErr
	}
	locations := locateUnknownJSONFields(payload, field, reflect.TypeOf(GenerationResponse{}))
	if len(locations) == 0 {
		return decodeErr
	}
	sort.Slice(locations, func(i, j int) bool { return locations[i].path < locations[j].path })
	parts := make([]string, 0, len(locations))
	for _, location := range locations {
		part := location.path
		if len(location.allowed) > 0 {
			part += " (allowed: " + strings.Join(location.allowed, ", ") + ")"
		}
		if location.correction != "" {
			part += "; move to " + location.correction
		}
		parts = append(parts, part)
	}
	diagnostic := "unknown field " + field + " at " + strings.Join(parts, "; ")
	return &strictJSONSchemaError{cause: decodeErr, diagnostic: diagnostic}
}

// locateUnknownJSONFields walks only statically typed JSON values. Map keys are
// permitted by the portable contract, but map values may still have a declared
// schema (for example, named runbook steps). Interface values remain opaque. It
// returns paths and field names, never values.
func locateUnknownJSONFields(payload []byte, field string, rootType reflect.Type) []unknownJSONFieldLocation {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document interface{}
	if err := decoder.Decode(&document); err != nil {
		return nil
	}
	locations := make([]unknownJSONFieldLocation, 0, 2)
	var walk func(interface{}, reflect.Type, string, int)
	walk = func(value interface{}, expected reflect.Type, path string, depth int) {
		if depth > 64 || len(locations) >= 8 || expected == nil {
			return
		}
		for expected.Kind() == reflect.Pointer {
			expected = expected.Elem()
		}
		switch expected.Kind() {
		case reflect.Struct:
			object, ok := value.(map[string]interface{})
			if !ok {
				return
			}
			fields := jsonStructFields(expected)
			allowed := make([]string, 0, len(fields))
			for name := range fields {
				allowed = append(allowed, name)
			}
			sort.Strings(allowed)
			for name, child := range object {
				childType, known := fields[name]
				childPath := name
				if path != "" {
					childPath = path + "." + name
				}
				if !known {
					if name == field {
						locations = append(locations, unknownJSONFieldLocation{
							path:       childPath,
							allowed:    allowed,
							correction: strictJSONFieldCorrection(expected, object, path, name),
						})
					}
					continue
				}
				walk(child, childType, childPath, depth+1)
			}
		case reflect.Slice, reflect.Array:
			items, ok := value.([]interface{})
			if !ok {
				return
			}
			for index, child := range items {
				walk(child, expected.Elem(), fmt.Sprintf("%s[%d]", path, index), depth+1)
			}
		case reflect.Map:
			object, ok := value.(map[string]interface{})
			if !ok || expected.Key().Kind() != reflect.String {
				return
			}
			for name, child := range object {
				childPath := name
				if path != "" {
					childPath = path + "." + name
				}
				walk(child, expected.Elem(), childPath, depth+1)
			}
		case reflect.Interface:
			// Arbitrary values are part of this field's declared schema.
			return
		}
	}
	walk(document, rootType, "", 0)
	return locations
}

func strictJSONFieldCorrection(expected reflect.Type, object map[string]interface{}, path, field string) string {
	if expected != reflect.TypeOf(runbook.Step{}) {
		return ""
	}
	kindValue, ok := object["kind"].(string)
	if !ok {
		return ""
	}
	payloadField, ok := runbook.StepPayloadFieldForJSONField(runbook.StepKind(kindValue), field)
	if !ok {
		return ""
	}
	if path == "" {
		return payloadField + "." + field
	}
	return path + "." + payloadField + "." + field
}

func jsonStructFields(structType reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, structType.NumField())
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if field.PkgPath != "" { // unexported
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}

// normalizeGeneratedDefinitionVersions accepts JSON numbers only at the two
// immutable definition-version fields authored by the provider. A JSON number
// has one lossless textual representation under UseNumber, while the runtime
// contract deliberately models versions as opaque strings. All other scalar
// mismatches remain untouched and fail strict decoding.
func normalizeGeneratedDefinitionVersions(payload []byte) []byte {
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
	candidate, ok := root["candidate"].(map[string]interface{})
	if !ok {
		return payload
	}
	changed := false
	if agents, ok := candidate["agents"].([]interface{}); ok {
		for _, rawAgent := range agents {
			agent, ok := rawAgent.(map[string]interface{})
			if !ok {
				continue
			}
			if version, ok := agent["version"].(json.Number); ok {
				agent["version"] = version.String()
				changed = true
			}
		}
	}
	if team, ok := candidate["team"].(map[string]interface{}); ok {
		if version, ok := team["version"].(json.Number); ok {
			team["version"] = version.String()
			changed = true
		}
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

// normalizeGeneratedCredentialReferenceOptions removes provider-authored
// credential choices at the strict decode boundary. A model can identify the
// credential kind that work requires, but only the authorized host may resolve
// that requirement to an opaque credential reference. Retaining option IDs here would
// let untrusted output invent or disclose credential identities.
func normalizeGeneratedCredentialReferenceOptions(generated *GenerationResponse) {
	if generated == nil {
		return
	}
	for index := range generated.UnresolvedQuestions {
		answer := &generated.UnresolvedQuestions[index].Answer
		if answer.Kind == RefinementAnswerCredentialReference {
			answer.Options = nil
		}
	}
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

// normalizeGeneratedRefinementProvenance accepts unambiguous provider aliases
// only at the refinement provenance boundary. The shorthand forms "prompt"
// and ["prompt", "catalog"] map to objects without reference or evidence. A
// credential question with the exact credential_reference answer contract may
// omit provenance because its category already determines the only safe,
// opaque provenance kind; no credential identity is inferred or exposed. An
// object may use "type" instead of canonical "kind" only when it contains no
// other fields beyond reference and evidence and names a known provenance
// kind. Unknown or ambiguous shapes remain untouched so strict decoding and
// refinement validation continue to fail closed.
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
		if _, present := question["provenance"]; !present && generatedCredentialReferenceQuestion(question) {
			question["provenance"] = []interface{}{map[string]interface{}{"kind": string(RefinementProvenanceCredential)}}
			changed = true
			continue
		}
		normalized, ok := normalizedRefinementProvenanceShorthand(question["provenance"])
		if ok {
			question["provenance"] = normalized
			changed = true
			continue
		}
		provenance, ok := question["provenance"].([]interface{})
		if !ok {
			continue
		}
		for _, rawEntry := range provenance {
			entry, ok := rawEntry.(map[string]interface{})
			if !ok {
				continue
			}
			entryChanged := false
			if normalizeRefinementProvenanceSourceAlias(entry) || normalizeRefinementProvenanceTypeAlias(entry) {
				entryChanged = true
			}
			if normalizeRefinementProvenanceValueAlias(entry) {
				entryChanged = true
			}
			if entryChanged {
				changed = true
			}
		}
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

func generatedCredentialReferenceQuestion(question map[string]interface{}) bool {
	category, ok := question["category"].(string)
	if !ok || category != string(RefinementCategoryCredential) {
		return false
	}
	answer, ok := question["answer"].(map[string]interface{})
	if !ok {
		return false
	}
	kind, ok := answer["kind"].(string)
	return ok && kind == string(RefinementAnswerCredentialReference)
}

// normalizeRefinementProvenanceSourceAlias handles the exact live provider
// shape {"source":"prompt"}. Unlike the older type alias, source is accepted
// only as a single-field object: reference/evidence or any other sibling would
// make the provider's intended semantics ambiguous and must remain a strict
// unknown-field failure.
func normalizeRefinementProvenanceSourceAlias(entry map[string]interface{}) bool {
	if len(entry) != 1 {
		return false
	}
	if _, hasKind := entry["kind"]; hasKind {
		return false
	}
	rawSource, hasSource := entry["source"]
	if !hasSource {
		return false
	}
	source, ok := rawSource.(string)
	if !ok || source != strings.TrimSpace(source) {
		return false
	}
	kind := RefinementProvenanceKind(source)
	if !validRefinementProvenanceKind(kind) {
		return false
	}
	delete(entry, "source")
	entry["kind"] = string(kind)
	return true
}

func normalizeRefinementProvenanceTypeAlias(entry map[string]interface{}) bool {
	if _, hasKind := entry["kind"]; hasKind {
		return false
	}
	rawType, hasType := entry["type"]
	if !hasType || len(entry) > 3 {
		return false
	}
	for key := range entry {
		switch key {
		case "type", "reference", "evidence":
		default:
			return false
		}
	}
	typeName, ok := rawType.(string)
	if !ok || typeName != strings.TrimSpace(typeName) {
		return false
	}
	kind := RefinementProvenanceKind(typeName)
	if !validRefinementProvenanceKind(kind) {
		return false
	}
	delete(entry, "type")
	entry["kind"] = string(kind)
	return true
}

// normalizeRefinementProvenanceValueAlias accepts the provider's common
// `value` spelling for the portable `reference` field only after a known,
// non-credential provenance kind is present. Credential provenance never
// accepts model-supplied references because they could contain an opaque host
// binding identifier.
func normalizeRefinementProvenanceValueAlias(entry map[string]interface{}) bool {
	if _, hasReference := entry["reference"]; hasReference {
		return false
	}
	kindText, ok := entry["kind"].(string)
	if !ok {
		return false
	}
	kind := RefinementProvenanceKind(kindText)
	if !validRefinementProvenanceKind(kind) || kind == RefinementProvenanceCredential {
		return false
	}
	value, ok := entry["value"].(string)
	if !ok || value == "" || value != strings.TrimSpace(value) {
		return false
	}
	for key := range entry {
		switch key {
		case "kind", "value", "evidence":
		default:
			return false
		}
	}
	delete(entry, "value")
	entry["reference"] = value
	return true
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

// normalizeGeneratedRefinementDependencies accepts the provider shorthand
// dependsOn:["scope"] only at the typed dependency-array boundary. Each
// non-empty string maps losslessly to {"questionId":"scope"}. Dependency
// existence, option constraints, and cycle checks remain authoritative in the
// refinement validator; unknown and mixed invalid shapes still fail closed.
func normalizeGeneratedRefinementDependencies(payload []byte) []byte {
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
		dependencies, ok := question["dependsOn"].([]interface{})
		if !ok {
			continue
		}
		for index, rawDependency := range dependencies {
			questionID, ok := rawDependency.(string)
			if !ok || strings.TrimSpace(questionID) == "" || questionID != strings.TrimSpace(questionID) {
				continue
			}
			dependencies[index] = map[string]interface{}{"questionId": questionID}
			changed = true
		}
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
	if _, err := EffectiveWorkforceActivationIntent(candidate.Activation); err != nil {
		issues = append(issues, issue("activation", "invalid_activation_intent", err.Error()))
	}
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
		issues = append(issues, validateAgentSkillAuthority(path, definition)...)
	}
	issues = append(issues, validateObjectiveRunbookEntrypoints(candidate, agents)...)
	issues = append(issues, validateSourceActionProjection(candidate)...)
	issues = append(issues, validateConversationEndpointBlueprints(candidate)...)
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
		for grantIndex, grant := range role.SkillGrants {
			if grant.RuntimeIdentity != nil {
				issues = append(issues, issue(fmt.Sprintf("team.roles[%d].skillGrants[%d].runtimeIdentity", index, grantIndex), "server_owned", "Skill runtime identity is resolved by authorized placement, not generated by the model"))
			}
		}
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
			if candidate.Activation != WorkforceActivationInactive && capability.Readiness == SkillReadinessNeedsBinding {
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
			if candidate.Activation != WorkforceActivationInactive {
				for _, binding := range requiredSkillCredentialBindings(capability, requirement.RequiredActions) {
					if !credentialRequirementAvailable(catalog, binding) {
						key := "credential:" + binding.Key + ":" + definition.ID
						missing[key] = MissingRequirement{
							Kind: "credential", ID: binding.Key,
							RequiredBy: "agent:" + definition.ID + "/skill:" + requirement.SkillID,
							OAuth2:     binding.OAuth2,
						}
					}
				}
			}
		}
	}
	for _, endpoint := range candidate.ConversationEndpoints {
		requiredBy := "conversation_endpoint:" + endpoint.ID
		skillCapability, ok := catalog.Skills[endpoint.SkillID]
		if !ok {
			key := "skill:" + endpoint.SkillID + ":" + requiredBy
			missing[key] = MissingRequirement{Kind: "skill", ID: endpoint.SkillID, RequiredBy: requiredBy}
			continue
		}
		if skillCapability.Version != endpoint.SkillVersion {
			key := "version:" + endpoint.SkillID + "@" + endpoint.SkillVersion + ":" + requiredBy
			missing[key] = MissingRequirement{
				Kind: "version", ID: endpoint.SkillID + "@" + endpoint.SkillVersion, RequiredBy: requiredBy,
			}
			continue
		}
		var adapter *ConversationAdapterCapability
		for index := range skillCapability.ConversationAdapters {
			if skillCapability.ConversationAdapters[index].ID == endpoint.AdapterID {
				adapter = &skillCapability.ConversationAdapters[index]
				break
			}
		}
		if adapter == nil {
			key := "conversation_adapter:" + endpoint.SkillID + "/" + endpoint.AdapterID + ":" + requiredBy
			missing[key] = MissingRequirement{
				Kind: "conversation_adapter", ID: endpoint.SkillID + "/" + endpoint.AdapterID, RequiredBy: requiredBy,
			}
			continue
		}
		if skillCapability.Readiness == SkillReadinessNeedsInstallation || skillCapability.Readiness == SkillReadinessUnavailable {
			kind := "skill_installation"
			if skillCapability.Readiness == SkillReadinessUnavailable {
				kind = "skill_unavailable"
			}
			key := kind + ":" + endpoint.SkillID + ":" + requiredBy
			missing[key] = MissingRequirement{Kind: kind, ID: endpoint.SkillID, RequiredBy: requiredBy}
		}
		if candidate.Activation != WorkforceActivationInactive && skillCapability.Readiness == SkillReadinessNeedsBinding {
			key := "skill_binding:" + endpoint.SkillID + ":" + requiredBy
			missing[key] = MissingRequirement{Kind: "skill_binding", ID: endpoint.SkillID, RequiredBy: requiredBy}
		}
		if candidate.Activation != WorkforceActivationInactive {
			for _, credential := range adapter.Credentials {
				if credential.Optional {
					continue
				}
				binding := skillCredentialBinding{Key: credential.Name, Kind: credential.Kind, OAuth2: credential.OAuth2}
				if credentialRequirementAvailable(catalog, binding) {
					continue
				}
				key := "credential:" + binding.Key + ":" + requiredBy
				missing[key] = MissingRequirement{
					Kind: "credential", ID: binding.Key, RequiredBy: requiredBy + "/skill:" + endpoint.SkillID,
					OAuth2: binding.OAuth2,
				}
			}
		}
	}
	if candidate.Team != nil {
		for _, role := range candidate.Team.Roles {
			for _, grant := range role.SkillGrants {
				catalogID := strings.TrimSpace(grant.CatalogID)
				if catalogID == "" {
					catalogID = strings.TrimSpace(grant.SkillID)
				}
				available, ok := catalog.Skills[catalogID]
				requiredBy := "team:" + candidate.Team.ID + "/role:" + role.ID
				if !ok {
					key := "skill:" + catalogID + ":" + requiredBy
					missing[key] = MissingRequirement{Kind: "skill", ID: catalogID, RequiredBy: requiredBy}
					continue
				}
				if strings.TrimSpace(grant.SkillVersion) != strings.TrimSpace(available.Version) {
					key := "version:" + catalogID + "@" + grant.SkillVersion + ":" + requiredBy
					missing[key] = MissingRequirement{Kind: "version", ID: catalogID + "@" + grant.SkillVersion, RequiredBy: requiredBy}
				}
				actions := stringSet(available.Actions)
				for _, action := range grant.AllowedActions {
					if !actions[action] {
						key := "action:" + catalogID + "/" + action + ":" + requiredBy
						missing[key] = MissingRequirement{Kind: "action", ID: catalogID + "/" + action, RequiredBy: requiredBy}
					}
				}
				if grant.EnablePrompt && !available.PromptAvailable {
					key := "prompt:" + catalogID + ":" + requiredBy
					missing[key] = MissingRequirement{Kind: "prompt", ID: catalogID, RequiredBy: requiredBy}
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
			} else if !sourceMonitorWithinPolicy(candidate, monitor, policy) {
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

func credentialRequirementAvailable(catalog CapabilityCatalog, requirement skillCredentialBinding) bool {
	if requirement.OAuth2 == nil {
		return catalog.AvailableCredentials[requirement.Key]
	}
	for index := range catalog.AvailableCredentialGrants[requirement.Key] {
		grant := &catalog.AvailableCredentialGrants[requirement.Key][index]
		if capability.OAuth2GrantSatisfies(requirement.OAuth2, grant) {
			return true
		}
	}
	return false
}

func sourceMonitorWithinPolicy(candidate *WorkforceCandidate, monitor InitiativeSourceMonitorBlueprint, policy SourcePolicyCapability) bool {
	if candidate == nil || policy.MaximumItems < 1 {
		return false
	}
	var rawURL string
	var maximumItems int64
	found := false
	for _, invocation := range candidateObjectiveCapabilityInvocations(candidate) {
		if invocation.action == nil || invocation.objectiveRef != monitor.ObjectiveRef || invocation.agentID != monitor.AssignedAgentDefinitionID ||
			invocation.action.SkillID != monitor.SkillID || invocation.action.SkillVersion != monitor.SkillVersion || invocation.action.Action != monitor.Action {
			continue
		}
		_ = json.Unmarshal(invocation.action.Arguments["url"].Literal, &rawURL)
		_ = json.Unmarshal(invocation.action.Arguments["maxItems"].Literal, &maximumItems)
		found = true
		break
	}
	if !found {
		return false
	}
	ok := maximumItems > 0
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
