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
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/team"
)

const (
	maximumGenerationBytes        = 1 << 20
	maximumRepairAttempts         = 2
	maximumSchemaRepairAttempts   = maximumRepairAttempts
	maximumContractRepairAttempts = maximumRepairAttempts
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
	form, err := ProjectWorkforceAuthoringForm(request.Catalog, request.Existing)
	if err != nil {
		return nil, fmt.Errorf("project workforce authoring form: %w", err)
	}
	request.Form = form
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
	repairAttempts := 0
	for decodeErr != nil {
		repairer, ok := c.generator.(RepairGenerator)
		if !ok {
			return nil, fmt.Errorf("decode workforce candidate: %w", decodeErr)
		}
		if repairAttempts >= maximumRepairAttempts {
			return nil, &SchemaGenerationError{RepairAttempts: repairAttempts, Diagnostic: publicSchemaDiagnostic(decodeErr)}
		}
		repairAttempts++
		repairRequest := request
		if repairRequest.InvocationKey != "" {
			repairRequest.InvocationKey = fmt.Sprintf("%s:repair:%d", repairRequest.InvocationKey, repairAttempts)
		}
		reportCompileProgress(observe, CompilePhaseSchemaRepair, repairAttempts, maximumRepairAttempts)
		payload, err = repairer.Repair(ctx, repairRequest, payload, decodeErr)
		if err != nil {
			return nil, fmt.Errorf("repair workforce candidate schema attempt %d: %w", repairAttempts, err)
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
	canonicalizeGeneratedRunbookObjectiveReferences(&generated.Candidate)
	materializationIssues := materializeAnsweredCapabilitySourceScopes(&generated.Candidate, request)
	synthesizeCapabilityNeedRefinements(&generated, request)
	scheduleIntentIssues := enforceScheduleIntentAuthority(&generated, request)
	if hasScheduleIntentQuestion(generated.UnresolvedQuestions) {
		materializationIssues = deferScheduleBlockedMaterializationIssues(materializationIssues)
	}
	extractedCommitments := extractExplicitPromptCommitments(request.Prompt)
	validateGenerated := func() (PromptCommitments, []ValidationIssue, []MissingRequirement) {
		formIssues := CompileWorkforceAuthoringForm(&generated.Candidate, request.Form, generated.Authoring)
		materializeDefaultAgentSkillAuthority(&generated.Candidate)
		applyAuthorityConstraint(&generated.Candidate, request.Catalog.AuthorityConstraint)
		applyExtractedApprovalCommitments(&generated.Candidate, extractedCommitments)
		applyExtractedApprovalTimeouts(&generated.Candidate, extractedCommitments)
		normalizeUnboundAmendmentPolicies(&generated.Candidate)
		commitments, commitmentIssues := effectivePromptCommitments(request.Prompt, generated.Commitments)
		applyActivationCommitment(&generated.Candidate, commitments)
		deferInactiveCredentialRefinements(&generated)
		validation := append(validateCandidate(&generated.Candidate, request.Existing), commitmentIssues...)
		validation = append(validation, formIssues...)
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
		validation = append(validation, validateHostedRunbookBudgets(&generated.Candidate, request.Catalog)...)
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
	// Schema and semantic repair share one two-attempt budget. Every repair is
	// revalidated from the raw AuthoringResult before it may replace the proposal.
	contractRepairAttempts := repairAttempts
	if repairer, ok := c.generator.(RepairGenerator); ok {
		repairReason := deterministicContractError(validation, repairableMissing)
		for repairAttempts < maximumRepairAttempts && (len(validation) > 0 || len(repairableMissing) > 0) {
			repairAttempts++
			contractRepairAttempts = repairAttempts
			repairRequest := request
			if repairRequest.InvocationKey != "" {
				repairRequest.InvocationKey = fmt.Sprintf("%s:repair:%d", repairRequest.InvocationKey, repairAttempts)
			}
			reportCompileProgress(observe, CompilePhaseContractRepair, repairAttempts, maximumRepairAttempts)
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
			canonicalizeGeneratedRunbookObjectiveReferences(&generated.Candidate)
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
	normalizeUnboundAmendmentPolicies(&result.Candidate)
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
	result.Validation = append(result.Validation, validateHostedRunbookBudgets(&result.Candidate, request.Catalog)...)
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
	if contractValidation := exhaustedInternalContractValidation(result.Validation); len(contractValidation) > 0 {
		return nil, &ContractGenerationError{
			RepairAttempts: contractRepairAttempts,
			Diagnostic:     publicContractDiagnostic(contractValidation),
		}
	}
	result.MissingRequirements = missingRequirements(&result.Candidate, request.Catalog)
	result.SourcePolicyProposals = sourcePolicyProposals(&result.Candidate, result.MissingRequirements, request.Catalog)
	result.RiskChanges = riskChanges(request.Existing, &result.Candidate)
	result.Diff = workforceDiff(request.Existing, &result.Candidate)
	result.Valid = len(result.Validation) == 0 && len(result.MissingRequirements) == 0 && len(result.UnresolvedQuestions) == 0
	return result, nil
}

// exhaustedInternalContractValidation identifies defects in a provider-built
// canonical object. These are never user-answerable setup gaps and therefore
// must not be persisted as a reviewable "repair plan" after bounded repair.
func exhaustedInternalContractValidation(validation []ValidationIssue) []ValidationIssue {
	result := make([]ValidationIssue, 0)
	for _, issue := range validation {
		if strings.HasPrefix(issue.Code, "runbook_") {
			result = append(result, issue)
		}
	}
	return result
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
	var schemaValidation *AuthoringSchemaValidationError
	if errors.As(err, &schemaValidation) {
		return truncateSchemaDiagnostic(schemaValidation.Error())
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
	if err := validateAuthoringResultDocument(payload); err != nil {
		return GenerationResponse{}, err
	}
	var generated GenerationResponse
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&generated); err != nil {
		return GenerationResponse{}, fmt.Errorf("strict decode authoring result: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return GenerationResponse{}, err
		}
		return GenerationResponse{}, errors.New("generated workforce candidate must contain one JSON object")
	}
	if generated.SchemaVersion != AuthoringResultSchemaVersion {
		return GenerationResponse{}, fmt.Errorf("unsupported authoring result schema version %q", generated.SchemaVersion)
	}
	normalizeGeneratedCredentialReferenceOptions(&generated)
	return generated, nil
}

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
			var runbookError *runbook.ValidationError
			if errors.As(err, &runbookError) {
				for _, diagnostic := range runbookError.Diagnostics {
					code := "runbook_" + strings.NewReplacer(".", "_", "-", "_").Replace(diagnostic.Code)
					issues = append(issues, issue(path+".runbook."+diagnostic.Path, code, diagnostic.Message))
				}
			} else {
				issues = append(issues, issue(path, "invalid_agent", err.Error()))
			}
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
		issues = append(issues, validateProjectBlueprint(candidate, agents)...)
		if existing != nil && existing.Project != nil && (candidate.Project == nil || candidate.Project.ID != existing.Project.ID) {
			issues = append(issues, issue("project.id", "invalid_amendment_identity", "Amended Project must keep its id"))
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
	issues = append(issues, validateProjectBlueprint(candidate, agents)...)
	if existing != nil && existing.Project != nil && (candidate.Project == nil || candidate.Project.ID != existing.Project.ID) {
		issues = append(issues, issue("project.id", "invalid_amendment_identity", "Amended Project must keep its id"))
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
	if candidate.Project != nil {
		for _, monitor := range candidate.Project.SourceMonitors {
			available, ok := catalog.Skills[monitor.SkillID]
			requiredBy := "project:" + candidate.Project.ID + "/monitor:" + monitor.ID
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

func sourceMonitorWithinPolicy(candidate *WorkforceCandidate, monitor ProjectSourceMonitorBlueprint, policy SourcePolicyCapability) bool {
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
