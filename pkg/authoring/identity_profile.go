package authoring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// IdentityProfile is the small, model-authored identity used by ordinary Agent
// creation. It deliberately cannot describe a workflow, capability grant,
// credential, connection, schedule, or runtime resource.
type IdentityProfile struct {
	Name                string                         `json:"name"`
	Purpose             string                         `json:"purpose"`
	Behavior            string                         `json:"behavior"`
	Personality         string                         `json:"personality"`
	OperatingPrinciples []string                       `json:"operatingPrinciples"`
	Clarifications      []IdentityProfileClarification `json:"clarifications"`
}

// Field names identify the identity decision that actually blocks creation;
// selecting a Skill or configuring an account cannot be such a decision.
type IdentityProfileClarification struct {
	Field     string `json:"field"`
	Question  string `json:"question"`
	WhyNeeded string `json:"whyNeeded"`
}

type ProfileGenerator interface {
	GenerateProfile(context.Context, GenerateRequest) (IdentityProfile, error)
}

const identityProfileSystemPrompt = `Create one Agent's identity using submit_agent_profile: a name, a short description of its purpose, behavior, personality, and a few operating principles.
Use the user's request to give this Agent a distinctive purpose and voice. Honor agentName if supplied; otherwise choose a short, purpose-inspired name absent from existingAgentNames. Preserve an existing name unless the user asks to change it.
Describe how the Agent helps when someone talks to it. It should discover and apply relevant Skills during the conversation, guide account connection or configuration when needed, and follow the runtime's approvals. Its purpose is a focus, not a restriction on tools or other useful tasks.
Do not plan workflows, schedules, objectives, teams, channels, integrations, credential setup, or capability requirements. Requested tools, Slack use, and recurring work can inform its identity, but are configured during conversation after creation. Do not claim any connection or automatic task already exists.
Choose sensible reversible identity details. Ask a clarification only if a missing or conflicting identity decision makes a sound profile impossible. Never ask creation-time questions about credentials, tools, destinations, schedules, or work inputs. Usually clarifications is empty. Return only the compact profile.`

// profileCapabilityCatalog retains host policy without carrying an inventory
// or creating account/setup prerequisites for identity creation.
func profileCapabilityCatalog(catalog CapabilityCatalog) CapabilityCatalog {
	result := CapabilityCatalog{}
	if catalog.AuthorityConstraint != nil {
		constraint := *catalog.AuthorityConstraint
		result.AuthorityConstraint = &constraint
	}
	if catalog.HostedExecution != nil {
		hosted := *catalog.HostedExecution
		result.HostedExecution = &hosted
	}
	return result
}

func (c *Compiler) compileIdentityProfile(ctx context.Context, request GenerateRequest, observe CompileProgressObserver) (*CompileResult, error) {
	if request.Existing != nil {
		if err := ValidateProfileCandidate(request.Existing); err != nil {
			return nil, fmt.Errorf("existing Agent profile: %w", err)
		}
	}
	// The identity author receives no workflow planner inputs or inventory.
	request.Form = AuthoringForm{}
	request.CompositionRequirements = nil
	reportCompileProgress(observe, CompilePhaseProviderRequest, 1, 1)
	generator, ok := c.generator.(ProfileGenerator)
	if !ok {
		return nil, errors.New("Agent profile generator is required")
	}
	profile, err := generator.GenerateProfile(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("generate Agent profile: %w", err)
	}
	reportCompileProgress(observe, CompilePhaseCandidateValidate, 1, 1)
	generated, err := compileIdentityProfile(profile, request)
	if err != nil {
		return nil, fmt.Errorf("compile Agent profile: %w", err)
	}
	if err := ValidateWorkforceCandidateSensitiveInput(&generated.Candidate); err != nil {
		return nil, err
	}
	if err := validateProfileQuestions(generated.UnresolvedQuestions); err != nil {
		return nil, err
	}
	result := &CompileResult{
		Candidate: generated.Candidate, Commitments: generated.Commitments,
		UnresolvedQuestions: generated.UnresolvedQuestions,
	}
	result.Validation = validateCandidate(&result.Candidate, request.Existing)
	result.Validation = append(result.Validation, ValidateCandidateAuthorityConstraint(&result.Candidate, request.Catalog.AuthorityConstraint)...)
	if nameIssues := validateSuggestedAgentNames(&result.Candidate, request); len(nameIssues) > 0 {
		return nil, &ContractGenerationError{Diagnostic: publicContractDiagnostic(nameIssues)}
	}
	result.RiskChanges = riskChanges(request.Existing, &result.Candidate)
	result.Diff = workforceDiff(request.Existing, &result.Candidate)
	result.Valid = len(result.Validation) == 0 && len(result.UnresolvedQuestions) == 0
	return result, nil
}

func compileIdentityProfile(profile IdentityProfile, request GenerateRequest) (GenerationResponse, error) {
	if err := validateIdentityProfile(profile); err != nil {
		return GenerationResponse{}, err
	}
	name := strings.TrimSpace(profile.Name)
	if request.AgentName != "" {
		name = strings.TrimSpace(request.AgentName)
	}
	if err := validateDraftAgentName(name, false); err != nil {
		return GenerationResponse{}, err
	}
	definition := &agent.AgentDefinition{
		ID: "profile", AuthoringKey: "profile", Version: "1.0.0", DisplayName: name,
		Purpose: strings.TrimSpace(profile.Purpose), SystemPrompt: strings.TrimSpace(profile.Behavior),
		Personality: strings.TrimSpace(profile.Personality), OperatingPrinciples: normalized(profile.OperatingPrinciples),
		Authority:  agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		Amendments: workforce.AmendmentPolicy{AgentMayPropose: true},
	}
	if request.Existing != nil {
		if err := ValidateProfileCandidate(request.Existing); err != nil {
			return GenerationResponse{}, err
		}
		previous := request.Existing.Agents[0]
		definition.ID, definition.AuthoringKey = previous.ID, previous.AuthoringKey
		definition.Version = nextAuthoringVersion(previous.Version)
	}
	result := GenerationResponse{
		SchemaVersion: AuthoringResultSchemaVersion,
		Candidate:     WorkforceCandidate{Agents: []*agent.AgentDefinition{definition}, Activation: deterministicAuthoringActivation(request)},
		Authoring:     AuthoringFormSubmission{Version: AuthoringFormVersionV1},
	}
	applyAuthorityConstraint(&result.Candidate, request.Catalog.AuthorityConstraint)
	for _, clarification := range profile.Clarifications {
		result.UnresolvedQuestions = append(result.UnresolvedQuestions, RefinementQuestion{
			ID: "profile-" + clarification.Field, Category: RefinementCategoryOther,
			Prompt: strings.TrimSpace(clarification.Question), WhyNeeded: strings.TrimSpace(clarification.WhyNeeded),
			Blocking:   []RefinementBlockingScope{RefinementBlocksCandidate},
			Answer:     RefinementAnswerSchema{Kind: RefinementAnswerText, Minimum: 1, Maximum: 2048},
			Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt, Evidence: "An identity decision is needed to finish this Agent profile."}},
			Priority:   500,
		})
	}
	return result, ValidateProfileCandidate(&result.Candidate)
}

func validateIdentityProfile(profile IdentityProfile) error {
	for field, value := range map[string]string{"name": profile.Name, "purpose": profile.Purpose, "behavior": profile.Behavior} {
		if strings.TrimSpace(value) == "" || len(value) > 16384 {
			return fmt.Errorf("Agent profile %s must be non-empty and bounded", field)
		}
	}
	if len(profile.Personality) > 16384 || len(profile.OperatingPrinciples) > 16 || len(profile.Clarifications) > 4 {
		return errors.New("Agent profile identity text is too large")
	}
	for _, principle := range profile.OperatingPrinciples {
		if strings.TrimSpace(principle) == "" || len(principle) > 2048 {
			return errors.New("Agent profile principles must be non-empty and bounded")
		}
	}
	seen := map[string]bool{}
	for _, clarification := range profile.Clarifications {
		if !profileIdentityField(clarification.Field) || seen[clarification.Field] ||
			strings.TrimSpace(clarification.Question) == "" || strings.TrimSpace(clarification.WhyNeeded) == "" ||
			len(clarification.Question) > 2048 || len(clarification.WhyNeeded) > 2048 {
			return errors.New("Agent profile clarifications must concern a unique identity field")
		}
		seen[clarification.Field] = true
	}
	return nil
}

func profileIdentityField(field string) bool {
	switch field {
	case "name", "purpose", "behavior", "personality":
		return true
	}
	return false
}

func validateProfileQuestions(questions []RefinementQuestion) error {
	for _, question := range questions {
		if !strings.HasPrefix(question.ID, "profile-") || !profileIdentityField(strings.TrimPrefix(question.ID, "profile-")) ||
			question.Category != RefinementCategoryOther || question.Answer.Kind != RefinementAnswerText || question.AutoResolvable ||
			len(question.Answer.Options) > 0 || len(question.DependsOn) > 0 {
			return errors.New("Agent profile creation accepts only identity clarification questions")
		}
	}
	return validateRefinementQuestions(questions)
}

func profileFromAgent(definition *agent.AgentDefinition) IdentityProfile {
	return IdentityProfile{Name: definition.DisplayName, Purpose: definition.Purpose, Behavior: definition.SystemPrompt,
		Personality: definition.Personality, OperatingPrinciples: append([]string(nil), definition.OperatingPrinciples...)}
}

func profileFromAuthoringIntent(intent AuthoringIntent) (IdentityProfile, error) {
	if intent.Kind != AuthoringResourceAgent || len(intent.Agents) != 1 || intent.Team != nil || len(intent.Conversations) > 0 || len(intent.Clarifications) > 0 {
		return IdentityProfile{}, errors.New("Agent profile creation requires exactly one identity and no workflow or setup answers")
	}
	answer := intent.Agents[0]
	if len(answer.Skills) > 0 || len(answer.Objectives) > 0 || len(answer.Operations) > 0 {
		return IdentityProfile{}, errors.New("Agent profile creation cannot select Skills, objectives, or operations")
	}
	return IdentityProfile{Name: answer.Name, Purpose: answer.Purpose, Behavior: answer.Behavior,
		Personality: answer.Personality, OperatingPrinciples: answer.OperatingPrinciples}, nil
}

// ValidateProfileCandidate is the authoritative content boundary, including
// legacy raw generators and hosts restoring durable authoring state.
func ValidateProfileCandidate(candidate *WorkforceCandidate) error {
	if candidate == nil || len(candidate.Agents) != 1 || candidate.Agents[0] == nil {
		return errors.New("Agent profile creation requires exactly one Agent")
	}
	if candidate.Team != nil || len(candidate.Assignments) > 0 || candidate.Project != nil || len(candidate.ConversationEndpoints) > 0 {
		return errors.New("Agent profile creation cannot include teams, assignments, projects, or endpoints")
	}
	definition := candidate.Agents[0]
	if len(definition.SkillRequirements) > 0 || len(definition.ObjectiveTemplates) > 0 || definition.Runbook != nil ||
		len(definition.Channels) > 0 || len(definition.Authority.AllowedSkillIDs) > 0 || len(definition.Authority.StandingGrants) > 0 ||
		len(definition.Authority.ApprovalDestinations) > 0 || definition.Authority.ApprovalTimeout != nil ||
		len(definition.DomainContext) > 0 || len(definition.Evaluations) > 0 {
		return errors.New("Agent profile creation cannot include Skills, work plans, channels, or external authority")
	}
	return nil
}

// ValidateProfilePlacement permits normal reviewed Agent placement and rejects
// external integration acquisition/configuration even when there is no Skill
// requirement that would otherwise consume it.
func ValidateProfilePlacement(placement ChangeSetPlacement) error {
	if placement.TeamDeploymentID != "" || placement.TeamExpectedRevision != 0 || placement.ProjectID != "" || placement.ProjectExpectedRevision != 0 ||
		len(placement.CredentialReferences) > 0 || len(placement.BindingConfigs) > 0 || len(placement.SkillSourceIdentities) > 0 ||
		len(placement.SkillSourceVersions) > 0 || len(placement.SkillRuntimeIdentities) > 0 || len(placement.PlannedSkillInstallations) > 0 ||
		len(placement.Objectives) > 0 || len(placement.ConversationEndpoints) > 0 {
		return errors.New("Agent profile creation cannot configure credentials, Skills, objectives, or external channels")
	}
	return nil
}

func profileOnlyChangeSet(changeSet *ChangeSet) bool {
	return changeSet != nil && (changeSet.ProfileOnly || (changeSet.Generation != nil && changeSet.Generation.Request.ProfileOnly))
}

func inheritProfileMode(request *CreateChangeSetRequest, parent *ChangeSet) error {
	if err := ValidateProfileChangeSet(parent); err != nil {
		return err
	}
	if request.ProfileOnly && !profileOnlyChangeSet(parent) {
		return errors.New("start a new Agent profile instead of revising a workflow proposal")
	}
	request.ProfileOnly = request.ProfileOnly || profileOnlyChangeSet(parent)
	return nil
}

func normalizeProfileCreateRequest(request *CreateChangeSetRequest) error {
	if !request.ProfileOnly {
		return nil
	}
	request.Catalog = profileCapabilityCatalog(request.Catalog)
	return ValidateProfilePlacement(request.Placement)
}

func finalizeProfileResult(result *CompileResult, request GenerateRequest, placement ChangeSetPlacement) error {
	if err := ValidateProfileCandidate(&result.Candidate); err != nil {
		return err
	}
	if err := ValidateProfilePlacement(placement); err != nil {
		return err
	}
	if err := validateProfileQuestions(result.UnresolvedQuestions); err != nil {
		return err
	}
	result.Validation = validateCandidate(&result.Candidate, request.Existing)
	result.Validation = append(result.Validation, ValidateCandidateAuthorityConstraint(&result.Candidate, request.Catalog.AuthorityConstraint)...)
	result.MissingRequirements = nil
	result.RiskChanges = riskChanges(request.Existing, &result.Candidate)
	result.Diff = workforceDiff(request.Existing, &result.Candidate)
	result.Valid = len(result.Validation) == 0 && len(result.UnresolvedQuestions) == 0
	return nil
}

// ValidateProfileChangeSet fences persisted generation, review and apply. A
// profile flag is durable authority and cannot be bypassed by a child request.
func ValidateProfileChangeSet(changeSet *ChangeSet) error {
	if !profileOnlyChangeSet(changeSet) {
		return nil
	}
	if !changeSet.ProfileOnly || (changeSet.Generation != nil && !changeSet.Generation.Request.ProfileOnly) {
		return errors.New("Agent profile generation mode is inconsistent")
	}
	if err := ValidateProfilePlacement(changeSet.Placement); err != nil {
		return err
	}
	if len(changeSet.RequiredCredentials) > 0 || len(changeSet.RequiredCredentialBindings) > 0 || len(changeSet.Result.MissingRequirements) > 0 {
		return errors.New("Agent profile creation cannot carry capability or account setup requirements")
	}
	if err := validateProfileCatalog(changeSet.Catalog); err != nil {
		return err
	}
	if changeSet.Generation != nil {
		request := changeSet.Generation.Request
		if request.Existing != nil {
			if err := ValidateProfileCandidate(request.Existing); err != nil {
				return err
			}
		}
		if request.CompositionRequirements != nil || len(request.Form.Fields) > 0 || len(request.Form.Values) > 0 {
			return errors.New("Agent profile creation cannot carry workflow planning inputs")
		}
		if err := validateProfileCatalog(request.Catalog); err != nil {
			return err
		}
	}
	if len(changeSet.Result.Candidate.Agents) > 0 || changeSet.Result.Candidate.Team != nil || changeSet.CandidateDigest != "" {
		if err := ValidateProfileCandidate(&changeSet.Result.Candidate); err != nil {
			return err
		}
	}
	if err := validateProfileQuestions(changeSet.Result.UnresolvedQuestions); err != nil {
		return err
	}
	return validateProfileQuestions(changeSet.Refinement.Questions)
}

func validateProfileCatalog(catalog CapabilityCatalog) error {
	full, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	minimal, err := json.Marshal(profileCapabilityCatalog(catalog))
	if err != nil {
		return err
	}
	if !bytes.Equal(full, minimal) {
		return errors.New("Agent profile catalog cannot carry capability or account inventory")
	}
	return nil
}

func decodeIdentityProfile(payload []byte) (IdentityProfile, error) {
	if len(payload) == 0 || len(payload) > maximumGenerationBytes {
		return IdentityProfile{}, errors.New("generated Agent profile must be between 1 byte and 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var result IdentityProfile
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return IdentityProfile{}, errors.New("Agent profile must contain exactly one JSON object")
	}
	return result, validateIdentityProfile(result)
}
