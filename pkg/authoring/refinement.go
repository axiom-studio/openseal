package authoring

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

var (
	catalogDiagnosticCodePattern      = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	catalogDiagnosticReferencePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,255}$`)
	authorityConstraintVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`)
	credentialBindingKeyPattern       = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

func reconcileRefinement(current ChangeSetRefinement, result *CompileResult) ChangeSetRefinement {
	questions := make([]RefinementQuestion, 0, len(result.UnresolvedQuestions))
	if refinementQuestionsAreActionable(result.UnresolvedQuestions, result.Validation) {
		questions = append(questions, result.UnresolvedQuestions...)
	}
	byID := make(map[string]bool, len(questions))
	merged := make([]RefinementQuestion, 0, len(questions)+len(current.Questions))
	for _, question := range questions {
		if !byID[question.ID] {
			byID[question.ID] = true
			merged = append(merged, question)
		}
	}
	answered := make(map[string]bool, len(current.Answers))
	for _, answer := range current.Answers {
		answered[answer.QuestionID] = true
	}
	for _, question := range current.Questions {
		if answered[question.ID] && !byID[question.ID] {
			byID[question.ID] = true
			merged = append(merged, question)
		}
	}
	return ChangeSetRefinement{Questions: merged, Answers: append([]RefinementAnswerEvent(nil), current.Answers...)}
}

const capabilityNeedQuestionPrefix = "server-skill-choice-"
const capabilitySourceScopeQuestionPrefix = "server-source-scope-"

// CapabilityNeedQuestionID is stable across generation retries and process
// restarts. Hosts may use it to correlate a server-owned capability need with
// the resulting audited refinement answer.
func CapabilityNeedQuestionID(needID string) string {
	return capabilityNeedQuestionPrefix + strings.TrimSpace(needID)
}

// CapabilitySourceScopeQuestionID is stable across retries and lets hosts and
// clients correlate an audited target list without owning authoring state.
func CapabilitySourceScopeQuestionID(needID string) string {
	return capabilitySourceScopeQuestionPrefix + strings.TrimSpace(needID)
}

// synthesizeCapabilityNeedRefinements makes prompt-matched Skill choice a
// deterministic compiler concern instead of relying on the provider to decide
// whether the operator should be asked. Existing provider scope questions are
// gated behind every currently unanswered Skill choice so only one sequential
// decision is actionable at a time.
func synthesizeCapabilityNeedRefinements(generated *GenerationResponse, request GenerateRequest) {
	if generated == nil || len(request.Catalog.CapabilityNeeds) == 0 {
		return
	}
	answered := make(map[string]RefinementProviderAnswerValue)
	if request.Refinement != nil {
		for _, answer := range request.Refinement.Answers {
			answered[strings.TrimSpace(answer.QuestionID)] = answer.Value
		}
	}
	active := make([]RefinementQuestion, 0, len(request.Catalog.CapabilityNeeds))
	activeIDs := make(map[string]bool, len(request.Catalog.CapabilityNeeds))
	for _, need := range request.Catalog.CapabilityNeeds {
		questionID := CapabilityNeedQuestionID(need.ID)
		if _, exists := answered[questionID]; exists || !need.ChoiceRequired && candidateSatisfiesCapabilityNeed(&generated.Candidate, need, request.Catalog) {
			continue
		}
		options := make([]RefinementQuestionOption, 0, len(need.SkillIDs))
		for _, skillID := range need.SkillIDs {
			skill := request.Catalog.Skills[skillID]
			label := strings.TrimSpace(skill.Name)
			if label == "" {
				label = skillID
			}
			options = append(options, RefinementQuestionOption{
				ID: skillID, Label: label, Description: strings.TrimSpace(skill.Description),
				Actions: append([]string(nil), skill.Actions...),
			})
		}
		activeIDs[questionID] = true
		active = append(active, RefinementQuestion{
			ID: questionID, Category: RefinementCategorySkill,
			Prompt: need.Prompt, WhyNeeded: need.WhyNeeded,
			Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
			Answer:   RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Options: options, Minimum: 1, Maximum: 1},
			Provenance: []RefinementQuestionProvenance{{
				Kind: RefinementProvenanceCatalog, Reference: need.ID,
				Evidence: "Prompt-matched Skills were verified by the server-owned capability catalog.",
			}},
			Priority: need.Priority,
		})
	}
	// A server-owned need is the sole authority for Skill-choice questions in
	// this request. Drop provider-authored Skill questions before validation;
	// they may be malformed, alias catalog entries, or duplicate a verified
	// choice under an unstable ID. Non-Skill questions remain provider-owned.
	questions := make([]RefinementQuestion, 0, len(active)+len(generated.UnresolvedQuestions)+len(request.Catalog.CapabilityNeeds))
	questions = append(questions, active...)
	for _, question := range generated.UnresolvedQuestions {
		if activeIDs[question.ID] || question.Category == RefinementCategorySkill {
			continue
		}
		if question.Category == RefinementCategoryScope && capabilityNeedsHaveSourceScope(request.Catalog.CapabilityNeeds) {
			continue
		}
		dependencies := make([]RefinementQuestionDependency, 0, len(question.DependsOn))
		applicable := true
		for _, dependency := range question.DependsOn {
			value, exists := answered[strings.TrimSpace(dependency.QuestionID)]
			if !exists {
				if need := capabilityNeedForQuestionID(request.Catalog.CapabilityNeeds, dependency.QuestionID); need != nil &&
					candidateSatisfiesCapabilityNeed(&generated.Candidate, *need, request.Catalog) {
					continue
				}
				dependencies = append(dependencies, dependency)
				continue
			}
			if !refinementDependencyMatchesAnswer(dependency, value) {
				applicable = false
				break
			}
		}
		if !applicable {
			continue
		}
		question.DependsOn = dependencies
		if question.Category == RefinementCategoryScope || question.Category == RefinementCategoryCredential {
			for _, choice := range active {
				if !refinementHasDependency(question, choice.ID) {
					question.DependsOn = append(question.DependsOn, RefinementQuestionDependency{QuestionID: choice.ID})
				}
			}
		}
		questions = append(questions, question)
	}
	for _, need := range request.Catalog.CapabilityNeeds {
		requirement := need.SourceScope
		if requirement == nil {
			continue
		}
		questionID := CapabilitySourceScopeQuestionID(need.ID)
		if _, exists := answered[questionID]; exists || len(requirement.Targets) > 0 {
			continue
		}
		dependencies := make([]RefinementQuestionDependency, 0, len(questions))
		for _, prerequisite := range questions {
			if prerequisite.Category == RefinementCategorySkill || prerequisite.Category == RefinementCategoryCredential {
				dependencies = append(dependencies, RefinementQuestionDependency{QuestionID: prerequisite.ID})
			}
		}
		questions = append(questions, RefinementQuestion{
			ID: questionID, Category: RefinementCategoryScope,
			Prompt: requirement.Prompt, WhyNeeded: requirement.WhyNeeded,
			Blocking:  []RefinementBlockingScope{RefinementBlocksCandidate},
			Answer:    RefinementAnswerSchema{Kind: RefinementAnswerStringList, Minimum: requirement.Minimum, Maximum: requirement.Maximum},
			DependsOn: dependencies,
			Provenance: []RefinementQuestionProvenance{{
				Kind: RefinementProvenanceCatalog, Reference: need.ID,
				Evidence: "The host matched a monitored-source capability that requires explicit target scope.",
			}},
			Priority: requirement.Priority,
		})
	}
	generated.UnresolvedQuestions = questions
}

func capabilityNeedForQuestionID(needs []CapabilityNeed, questionID string) *CapabilityNeed {
	for index := range needs {
		if CapabilityNeedQuestionID(needs[index].ID) == strings.TrimSpace(questionID) {
			return &needs[index]
		}
	}
	return nil
}

func candidateSatisfiesCapabilityNeed(candidate *WorkforceCandidate, need CapabilityNeed, catalog CapabilityCatalog) bool {
	if candidate == nil {
		return false
	}
	allowed := stringSet(need.SkillIDs)
	for _, endpoint := range candidate.ConversationEndpoints {
		if !allowed[endpoint.SkillID] {
			continue
		}
		skill, exists := catalog.Skills[endpoint.SkillID]
		if !exists || strings.TrimSpace(endpoint.SkillVersion) != strings.TrimSpace(skill.Version) {
			continue
		}
		for _, adapter := range skill.ConversationAdapters {
			if adapter.ID == endpoint.AdapterID &&
				containsConversationMode(adapter.EndpointModes, endpoint.Mode) &&
				containsExactString(adapter.InboundEventTypes, capability.ConversationEventMessageReceived) &&
				containsDeliveryOperation(adapter.Delivery.Operations, capability.ConversationDeliveryMessageSend) {
				return true
			}
		}
	}
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		for _, requirement := range definition.SkillRequirements {
			if !allowed[requirement.SkillID] {
				continue
			}
			available, exists := catalog.Skills[requirement.SkillID]
			if exists && strings.TrimSpace(requirement.VersionConstraint) == strings.TrimSpace(available.Version) {
				return true
			}
		}
	}
	return false
}

func validateAnsweredCapabilityNeeds(candidate *WorkforceCandidate, request GenerateRequest) []ValidationIssue {
	if request.Refinement == nil {
		return nil
	}
	answered := make(map[string]RefinementProviderAnswerValue, len(request.Refinement.Answers))
	for _, answer := range request.Refinement.Answers {
		answered[strings.TrimSpace(answer.QuestionID)] = answer.Value
	}
	issues := make([]ValidationIssue, 0)
	for _, need := range request.Catalog.CapabilityNeeds {
		answer, exists := answered[CapabilityNeedQuestionID(need.ID)]
		if !exists {
			continue
		}
		selected := need
		if len(answer.SkillIDs) > 0 {
			selected.SkillIDs = append([]string(nil), answer.SkillIDs...)
		}
		if !candidateSatisfiesCapabilityNeed(candidate, selected, request.Catalog) {
			issues = append(issues, issue(
				"agents.skillRequirements",
				"capability_need_not_materialized",
				fmt.Sprintf(
					"Answered capability need %s selected exact Skill %s; bind that exact catalog Skill and do not substitute another option",
					need.ID,
					capabilityNeedSkillIdentities(selected.SkillIDs, request.Catalog),
				),
			))
			continue
		}
		// Source capability choices are operational, not descriptive. Merely
		// listing the selected Skill on an Agent while scheduling a different
		// network capability would ignore the operator's audited choice and can
		// bypass the Initiative monitor envelope. Require one exact executable
		// invocation before source-scope materialization proceeds.
		if selected.SourceScope != nil && sourceCapabilityRequiresDurableAction(request) && len(matchingSourceScopeInvocations(candidate, selected, request.Catalog)) == 0 {
			issues = append(issues, issue("objectives.cadence.runTemplate.capability", "capability_need_action_not_materialized", fmt.Sprintf("Answered source capability need %s must be used by an exact durable Objective action", need.ID)))
		}
	}
	return issues
}

func capabilityNeedSkillIdentities(skillIDs []string, catalog CapabilityCatalog) string {
	identities := make([]string, 0, len(skillIDs))
	for _, skillID := range nonEmptyUnique(skillIDs) {
		version := strings.TrimSpace(catalog.Skills[skillID].Version)
		if version == "" {
			identities = append(identities, skillID)
			continue
		}
		identities = append(identities, skillID+"@"+version)
	}
	if len(identities) == 0 {
		return "(none)"
	}
	return strings.Join(identities, ", ")
}

func capabilityNeedsHaveSourceScope(needs []CapabilityNeed) bool {
	for _, need := range needs {
		if need.SourceScope != nil {
			return true
		}
	}
	return false
}

func validateCapabilitySourceScopeFulfillment(candidate *WorkforceCandidate, request GenerateRequest) []ValidationIssue {
	if !sourceCapabilityRequiresDurableAction(request) {
		return nil
	}
	answered := make(map[string]RefinementProviderAnswerValue)
	if request.Refinement != nil {
		answered = make(map[string]RefinementProviderAnswerValue, len(request.Refinement.Answers))
		for _, answer := range request.Refinement.Answers {
			answered[strings.TrimSpace(answer.QuestionID)] = answer.Value
		}
	}
	issues := make([]ValidationIssue, 0)
	for _, need := range request.Catalog.CapabilityNeeds {
		requirement := need.SourceScope
		answer, exists := answered[CapabilitySourceScopeQuestionID(need.ID)]
		if !exists && requirement != nil && len(requirement.Targets) > 0 {
			answer, exists = RefinementProviderAnswerValue{Items: requirement.Targets}, true
		}
		if requirement == nil || len(requirement.MaterializationInputKeys) == 0 || !exists {
			continue
		}
		need = selectedCapabilityNeed(need, answered)
		if !capabilitySourceScopeMaterialized(candidate, need, answer.Items, requirement.MaterializationInputKeys) {
			issues = append(issues, issue("objectives.cadence.runTemplate.capability.inputs", "source_scope_not_materialized", "Every answered source target must be present in a durable capability action input"))
		}
	}
	return issues
}

func capabilitySourceScopeMaterialized(candidate *WorkforceCandidate, need CapabilityNeed, targets, inputKeys []string) bool {
	targets = nonEmptyUnique(targets)
	if candidate == nil || len(targets) == 0 {
		return false
	}
	allowedSkills := stringSet(need.SkillIDs)
	allowedKeys := stringSet(inputKeys)
	found := make(map[string]bool, len(targets))
	for _, candidateInvocation := range candidateObjectiveCapabilityInvocations(candidate) {
		invocation := candidateInvocation.invocation
		skillID, _ := invocation["skillId"].(string)
		if !allowedSkills[strings.TrimSpace(skillID)] {
			continue
		}
		inputs, _ := invocation["inputs"].(map[string]interface{})
		for key, value := range inputs {
			if !allowedKeys[key] {
				continue
			}
			materialized := strings.ToLower(fmt.Sprint(value))
			for _, target := range targets {
				if strings.Contains(materialized, strings.ToLower(target)) {
					found[target] = true
				}
			}
		}
	}
	return len(found) == len(targets)
}

func refinementDependencyMatchesAnswer(dependency RefinementQuestionDependency, value RefinementProviderAnswerValue) bool {
	if len(dependency.RequiredOptionIDs) == 0 {
		return true
	}
	selected := make(map[string]bool, len(value.OptionIDs)+len(value.SkillIDs))
	for _, id := range value.OptionIDs {
		selected[id] = true
	}
	for _, id := range value.SkillIDs {
		selected[id] = true
	}
	for _, id := range dependency.RequiredOptionIDs {
		if !selected[id] {
			return false
		}
	}
	return true
}

func refinementHasDependency(question RefinementQuestion, questionID string) bool {
	for _, dependency := range question.DependsOn {
		if dependency.QuestionID == questionID {
			return true
		}
	}
	return false
}

func refinementQuestionsAreActionable(questions []RefinementQuestion, validation []ValidationIssue) bool {
	for _, question := range questions {
		if strings.TrimSpace(question.ID) == "" || strings.TrimSpace(question.Prompt) == "" || strings.TrimSpace(question.WhyNeeded) == "" ||
			!validQuestionCategory(question.Category) || !validAnswerKind(question.Answer.Kind) || len(question.Blocking) == 0 ||
			(question.Category == RefinementCategorySkill && question.Answer.Kind != RefinementAnswerSkillSelection) || validateAnswerSchema(question.Answer) != nil {
			return false
		}
	}
	for _, issue := range validation {
		if issue.Path == "unresolvedQuestions" && issue.Code == "invalid_refinement_catalog" {
			return false
		}
	}
	return true
}

// RefinementQuestionCategory identifies the authority that can truthfully
// resolve an authoring gap. It is product-neutral and never implies that a
// model can inspect credentials or installation state by itself.
type RefinementQuestionCategory string

const (
	RefinementCategoryCredential  RefinementQuestionCategory = "credential"
	RefinementCategorySkill       RefinementQuestionCategory = "skill"
	RefinementCategoryScope       RefinementQuestionCategory = "scope"
	RefinementCategoryPolicy      RefinementQuestionCategory = "policy"
	RefinementCategoryAuthority   RefinementQuestionCategory = "authority"
	RefinementCategoryDestination RefinementQuestionCategory = "destination"
	RefinementCategoryBudget      RefinementQuestionCategory = "budget"
	RefinementCategoryApproval    RefinementQuestionCategory = "approval"
	RefinementCategoryOther       RefinementQuestionCategory = "other"
)

type RefinementAnswerKind string

const (
	RefinementAnswerText                RefinementAnswerKind = "text"
	RefinementAnswerStringList          RefinementAnswerKind = "string_list"
	RefinementAnswerSingleSelect        RefinementAnswerKind = "single_select"
	RefinementAnswerMultiSelect         RefinementAnswerKind = "multi_select"
	RefinementAnswerBoolean             RefinementAnswerKind = "boolean"
	RefinementAnswerCredentialReference RefinementAnswerKind = "credential_reference"
	RefinementAnswerSkillSelection      RefinementAnswerKind = "skill_selection"
)

type RefinementBlockingScope string

const (
	RefinementBlocksCandidate  RefinementBlockingScope = "candidate"
	RefinementBlocksEvaluation RefinementBlockingScope = "evaluation"
	RefinementBlocksApply      RefinementBlockingScope = "apply"
)

type RefinementProvenanceKind string

const (
	RefinementProvenancePrompt     RefinementProvenanceKind = "prompt"
	RefinementProvenanceCatalog    RefinementProvenanceKind = "catalog"
	RefinementProvenanceSkill      RefinementProvenanceKind = "skill"
	RefinementProvenanceCredential RefinementProvenanceKind = "credential"
	RefinementProvenancePolicy     RefinementProvenanceKind = "policy"
	RefinementProvenanceRuntime    RefinementProvenanceKind = "runtime"
)

type RefinementQuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Actions is trusted only after validateRefinementCatalog proves every
	// value is an exact action exposed by the selected Skill. Keeping this
	// metadata in the portable contract lets interactive clients explain what
	// a Skill choice enables without copying or re-querying host catalogs.
	Actions []string `json:"actions,omitempty"`
}

type RefinementAnswerSchema struct {
	Kind    RefinementAnswerKind       `json:"kind"`
	Options []RefinementQuestionOption `json:"options,omitempty"`
	Minimum int                        `json:"minimum,omitempty"`
	Maximum int                        `json:"maximum,omitempty"`
	Pattern string                     `json:"pattern,omitempty"`
}

type RefinementQuestionDependency struct {
	QuestionID        string   `json:"questionId"`
	RequiredOptionIDs []string `json:"requiredOptionIds,omitempty"`
}

type RefinementQuestionProvenance struct {
	Kind      RefinementProvenanceKind `json:"kind"`
	Reference string                   `json:"reference,omitempty"`
	Evidence  string                   `json:"evidence,omitempty"`
}

// RefinementQuestion is an immutable, typed gap in a candidate. Priority is a
// positive integer; larger values are presented first after dependencies have
// resolved. AutoResolvable is a declaration only: a trusted host still has to
// submit an audited answer event.
type RefinementQuestion struct {
	ID             string                         `json:"id"`
	Category       RefinementQuestionCategory     `json:"category"`
	Prompt         string                         `json:"prompt"`
	WhyNeeded      string                         `json:"whyNeeded"`
	Blocking       []RefinementBlockingScope      `json:"blocking"`
	Answer         RefinementAnswerSchema         `json:"answer"`
	DependsOn      []RefinementQuestionDependency `json:"dependsOn,omitempty"`
	Provenance     []RefinementQuestionProvenance `json:"provenance,omitempty"`
	Priority       int                            `json:"priority"`
	AutoResolvable bool                           `json:"autoResolvable,omitempty"`
}

type RefinementAnswerSource string

const (
	RefinementAnswerSourceUser    RefinementAnswerSource = "user"
	RefinementAnswerSourceRuntime RefinementAnswerSource = "runtime"
)

// RefinementAnswerValue avoids an untyped JSON bag while supporting the
// portable question kinds. Credential references are opaque identifiers;
// secret values are never part of this contract.
type RefinementAnswerValue struct {
	Text                string                          `json:"text,omitempty"`
	Items               []string                        `json:"items,omitempty"`
	OptionIDs           []string                        `json:"optionIds,omitempty"`
	Boolean             *bool                           `json:"boolean,omitempty"`
	CredentialReference *capability.CredentialReference `json:"credentialReference,omitempty"`
	SkillIDs            []string                        `json:"skillIds,omitempty"`
}

type RefinementAnswerEvent struct {
	ID               string                 `json:"id"`
	IdempotencyKey   string                 `json:"idempotencyKey"`
	RequestDigest    string                 `json:"requestDigest"`
	QuestionID       string                 `json:"questionId"`
	QuestionRevision int64                  `json:"questionRevision"`
	Value            RefinementAnswerValue  `json:"value"`
	Source           RefinementAnswerSource `json:"source"`
	Actor            ChangeSetActor         `json:"actor"`
	AnsweredAt       time.Time              `json:"answeredAt"`
}

type ChangeSetRefinement struct {
	Questions []RefinementQuestion    `json:"questions,omitempty"`
	Answers   []RefinementAnswerEvent `json:"answers,omitempty"`
}

type RefinementResolvedAnswer struct {
	QuestionID string                        `json:"questionId"`
	Value      RefinementProviderAnswerValue `json:"value"`
	Source     RefinementAnswerSource        `json:"source"`
}

// RefinementProviderAnswerValue is the deliberately reduced model projection.
// Opaque credential identifiers remain operator-facing and are represented to
// the provider only as a configured kind.
type RefinementProviderAnswerValue struct {
	Text                 string   `json:"text,omitempty"`
	Items                []string `json:"items,omitempty"`
	OptionIDs            []string `json:"optionIds,omitempty"`
	Boolean              *bool    `json:"boolean,omitempty"`
	CredentialConfigured bool     `json:"credentialConfigured,omitempty"`
	CredentialKind       string   `json:"credentialKind,omitempty"`
	SkillIDs             []string `json:"skillIds,omitempty"`
}

// RefinementContext is the credential-free context passed to the authoring
// provider. Answer history remains ordered and auditable on the ChangeSet.
type RefinementContext struct {
	Questions []RefinementQuestion       `json:"questions,omitempty"`
	Answers   []RefinementResolvedAnswer `json:"answers,omitempty"`
}

func providerRefinementContext(value ChangeSetRefinement) *RefinementContext {
	current := make(map[string]RefinementAnswerEvent, len(value.Answers))
	for _, event := range value.Answers {
		current[event.QuestionID] = event
	}
	answers := make([]RefinementResolvedAnswer, 0, len(current))
	for _, question := range value.Questions {
		event, ok := current[question.ID]
		if !ok {
			continue
		}
		projected := RefinementProviderAnswerValue{
			Text: event.Value.Text, Items: append([]string(nil), event.Value.Items...), OptionIDs: append([]string(nil), event.Value.OptionIDs...),
			Boolean: event.Value.Boolean, SkillIDs: append([]string(nil), event.Value.SkillIDs...),
		}
		if event.Value.CredentialReference != nil {
			projected.CredentialConfigured = true
			projected.CredentialKind = event.Value.CredentialReference.Kind
		}
		answers = append(answers, RefinementResolvedAnswer{QuestionID: event.QuestionID, Value: projected, Source: event.Source})
	}
	return &RefinementContext{Questions: append([]RefinementQuestion(nil), value.Questions...), Answers: answers}
}

func (r ChangeSetRefinement) CurrentAnswer(questionID string) *RefinementAnswerEvent {
	for i := len(r.Answers) - 1; i >= 0; i-- {
		if r.Answers[i].QuestionID == questionID {
			answer := r.Answers[i]
			return &answer
		}
	}
	return nil
}

// NextQuestion returns the highest-priority unanswered question whose
// dependencies are satisfied. Stable declaration order breaks ties.
func (r ChangeSetRefinement) NextQuestion() *RefinementQuestion {
	answered := make(map[string]*RefinementAnswerEvent)
	for i := range r.Answers {
		answered[r.Answers[i].QuestionID] = &r.Answers[i]
	}
	candidates := make([]struct {
		index    int
		question RefinementQuestion
	}, 0, len(r.Questions))
	for index, question := range r.Questions {
		if answered[question.ID] != nil || !dependenciesSatisfied(question, answered) {
			continue
		}
		candidates = append(candidates, struct {
			index    int
			question RefinementQuestion
		}{index, question})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].question.Priority == candidates[j].question.Priority {
			return candidates[i].index < candidates[j].index
		}
		return candidates[i].question.Priority > candidates[j].question.Priority
	})
	if len(candidates) == 0 {
		return nil
	}
	question := candidates[0].question
	return &question
}

func dependenciesSatisfied(question RefinementQuestion, answered map[string]*RefinementAnswerEvent) bool {
	for _, dependency := range question.DependsOn {
		answer := answered[dependency.QuestionID]
		if answer == nil {
			return false
		}
		if len(dependency.RequiredOptionIDs) == 0 {
			continue
		}
		selected := make(map[string]bool, len(answer.Value.OptionIDs)+len(answer.Value.SkillIDs))
		for _, id := range answer.Value.OptionIDs {
			selected[id] = true
		}
		for _, id := range answer.Value.SkillIDs {
			selected[id] = true
		}
		for _, required := range dependency.RequiredOptionIDs {
			if !selected[required] {
				return false
			}
		}
	}
	return true
}

func validateRefinementQuestions(questions []RefinementQuestion) error {
	ids := make(map[string]bool, len(questions))
	for i := range questions {
		q := &questions[i]
		q.ID, q.Prompt, q.WhyNeeded = strings.TrimSpace(q.ID), strings.TrimSpace(q.Prompt), strings.TrimSpace(q.WhyNeeded)
		missing := make([]string, 0, 4)
		if q.ID == "" {
			missing = append(missing, "id")
		}
		if q.Prompt == "" {
			missing = append(missing, "prompt")
		}
		if q.WhyNeeded == "" {
			missing = append(missing, "whyNeeded")
		}
		if q.Priority < 1 || q.Priority > 1000 {
			missing = append(missing, "priority (integer 1..1000)")
		}
		if len(missing) > 0 {
			return fmt.Errorf("unresolvedQuestions[%d] missing or invalid required fields: %s", i, strings.Join(missing, ", "))
		}
		if ids[q.ID] {
			return fmt.Errorf("duplicate refinement question %s", q.ID)
		}
		ids[q.ID] = true
		if !validQuestionCategory(q.Category) || !validAnswerKind(q.Answer.Kind) || len(q.Blocking) == 0 {
			return fmt.Errorf("refinement question %s has an invalid category, answer kind, or blocking scope", q.ID)
		}
		if q.Category == RefinementCategorySkill && q.Answer.Kind != RefinementAnswerSkillSelection {
			return fmt.Errorf("refinement question %s has category skill and must use answer kind skill_selection", q.ID)
		}
		if q.Category == RefinementCategoryCredential && q.Answer.Kind != RefinementAnswerCredentialReference {
			return fmt.Errorf("refinement question %s has category credential and must use answer kind credential_reference", q.ID)
		}
		for _, scope := range q.Blocking {
			if scope != RefinementBlocksCandidate && scope != RefinementBlocksEvaluation && scope != RefinementBlocksApply {
				return fmt.Errorf("refinement question %s has an invalid blocking scope", q.ID)
			}
		}
		if err := validateAnswerSchema(q.Answer); err != nil {
			return fmt.Errorf("refinement question %s: %w", q.ID, err)
		}
		if len(q.Provenance) == 0 {
			return fmt.Errorf("refinement question %s requires provenance", q.ID)
		}
		for _, provenance := range q.Provenance {
			if !validProvenanceKind(provenance.Kind) {
				return fmt.Errorf("refinement question %s has invalid provenance", q.ID)
			}
			if provenance.Kind == RefinementProvenanceCredential && strings.TrimSpace(provenance.Reference) != "" {
				return fmt.Errorf("refinement question %s cannot expose an opaque credential reference", q.ID)
			}
		}
	}
	for _, q := range questions {
		for _, dependency := range q.DependsOn {
			if !ids[strings.TrimSpace(dependency.QuestionID)] || dependency.QuestionID == q.ID {
				return fmt.Errorf("refinement question %s has an invalid dependency", q.ID)
			}
		}
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	dependencies := make(map[string][]string, len(questions))
	for _, q := range questions {
		for _, dependency := range q.DependsOn {
			dependencies[q.ID] = append(dependencies[q.ID], dependency.QuestionID)
		}
	}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return false
		}
		if visited[id] {
			return true
		}
		visiting[id] = true
		for _, dependency := range dependencies[id] {
			if !visit(dependency) {
				return false
			}
		}
		visiting[id], visited[id] = false, true
		return true
	}
	for id := range ids {
		if !visit(id) {
			return fmt.Errorf("refinement question %s has a cyclic dependency", id)
		}
	}
	return nil
}

func validProvenanceKind(kind RefinementProvenanceKind) bool {
	switch kind {
	case RefinementProvenancePrompt, RefinementProvenanceCatalog, RefinementProvenanceSkill, RefinementProvenanceCredential, RefinementProvenancePolicy, RefinementProvenanceRuntime:
		return true
	default:
		return false
	}
}

func validateRefinementCatalog(questions []RefinementQuestion, catalog CapabilityCatalog) error {
	if err := ValidateCapabilityCatalog(catalog); err != nil {
		return err
	}
	for _, question := range questions {
		if question.Answer.Kind != RefinementAnswerSkillSelection {
			continue
		}
		for _, option := range question.Answer.Options {
			skill, ok := catalog.Skills[option.ID]
			if !ok {
				return refinementCatalogOptionError(question.ID, option.ID, catalog.Skills)
			}
			if skill.Readiness == SkillReadinessUnavailable {
				return fmt.Errorf("refinement question %s presents unavailable Skill %s", question.ID, option.ID)
			}
			allowedActions := stringSet(skill.Actions)
			for _, action := range option.Actions {
				if !allowedActions[action] {
					return fmt.Errorf("refinement question %s presents action %s not exposed by Skill %s", question.ID, action, option.ID)
				}
			}
			for _, evidence := range skill.Compatibility {
				if !evidence.Compatible && !refinementLifecycleGap(skill.Readiness, evidence.Requirement) {
					return fmt.Errorf("refinement question %s presents incompatibility-proven Skill %s (%s)", question.ID, option.ID, evidence.Requirement)
				}
			}
			if skill.Readiness == SkillReadinessNeedsInstallation {
				if strings.TrimSpace(skill.Version) == "" || strings.TrimSpace(skill.SourceIdentity) == "" {
					return fmt.Errorf("refinement question %s presents installable Skill %s without exact version and source identity", question.ID, option.ID)
				}
				verified := false
				for _, evidence := range skill.Compatibility {
					if evidence.Compatible && strings.TrimSpace(evidence.Reference) != "" {
						verified = true
						break
					}
				}
				if !verified {
					return fmt.Errorf("refinement question %s presents installable Skill %s without referenced positive compatibility evidence", question.ID, option.ID)
				}
			}
		}
	}
	return nil
}

// ValidateCapabilityCatalog rejects malformed host projections before they are
// persisted or placed in model context.
func ValidateCapabilityCatalog(catalog CapabilityCatalog) error {
	if err := ValidateRuntimeCompositionCapability(catalog.RuntimeComposition); err != nil {
		return fmt.Errorf("runtime composition capability: %w", err)
	}
	if constraint := catalog.AuthorityConstraint; constraint != nil {
		if constraint.ID != strings.TrimSpace(constraint.ID) || constraint.Version != strings.TrimSpace(constraint.Version) ||
			!catalogDiagnosticReferencePattern.MatchString(constraint.ID) || !authorityConstraintVersionPattern.MatchString(constraint.Version) {
			return errors.New("authority constraint requires a valid id and version")
		}
		if constraint.MaximumRisk != "" && riskRank(constraint.MaximumRisk) < 0 {
			return errors.New("authority constraint maximum risk is invalid")
		}
		if constraint.RequireApprovalAt != "" && riskRank(constraint.RequireApprovalAt) < 0 {
			return errors.New("authority constraint approval risk is invalid")
		}
		if constraint.MaximumRisk != "" && constraint.RequireApprovalAt != "" && riskRank(constraint.RequireApprovalAt) > riskRank(constraint.MaximumRisk) {
			return errors.New("authority constraint approval risk cannot exceed maximum risk")
		}
	}
	for id, skill := range catalog.Skills {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(skill.ID) == "" || id != skill.ID {
			return errors.New("Skill catalog keys must match non-empty Skill ids")
		}
		if skill.MaximumRisk != "" && riskRank(skill.MaximumRisk) < 0 {
			return fmt.Errorf("Skill %s maximum risk is invalid", id)
		}
		actions := stringSet(skill.Actions)
		for action, risk := range skill.ActionRisks {
			if action != strings.TrimSpace(action) || action == "" || !actions[action] {
				return fmt.Errorf("Skill %s action risk references an undeclared action", id)
			}
			if riskRank(risk) < 0 {
				return fmt.Errorf("Skill %s action %s risk is invalid", id, action)
			}
			if skill.MaximumRisk != "" && riskRank(risk) > riskRank(skill.MaximumRisk) {
				return fmt.Errorf("Skill %s action %s risk exceeds the Skill maximum", id, action)
			}
		}
		if len(strings.TrimSpace(skill.SourceIdentity)) > 1024 {
			return fmt.Errorf("Skill %s source identity is too long", id)
		}
		if skill.RuntimeIdentity != nil {
			identity := skill.RuntimeIdentity.Normalized()
			if !identity.Valid() || identity != *skill.RuntimeIdentity {
				return fmt.Errorf("Skill %s runtime identity is invalid or non-canonical", id)
			}
			if source := strings.TrimSpace(skill.SourceIdentity); source != "" && identity.SourceIdentity != source {
				return fmt.Errorf("Skill %s runtime identity conflicts with its source identity", id)
			}
		}
		switch skill.Readiness {
		case "", SkillReadinessReady, SkillReadinessNeedsBinding, SkillReadinessNeedsInstallation, SkillReadinessUnavailable:
		default:
			return fmt.Errorf("Skill %s has invalid readiness", id)
		}
		for _, evidence := range skill.Compatibility {
			if strings.TrimSpace(evidence.Requirement) == "" || strings.TrimSpace(evidence.Evidence) == "" {
				return fmt.Errorf("Skill %s compatibility requires a requirement and evidence", id)
			}
		}
		type credentialContract struct {
			kind    string
			oauth2  *capability.OAuth2Requirement
			actions map[string]bool
			all     bool
		}
		credentialNames := make(map[string]credentialContract, len(skill.Credentials))
		for index, credential := range skill.Credentials {
			if credential.Name != strings.TrimSpace(credential.Name) || credential.Kind != strings.TrimSpace(credential.Kind) ||
				credential.Name == "" || credential.Kind == "" || len(credential.Name) > 128 || len(credential.Kind) > 128 {
				return fmt.Errorf("Skill %s credential %d is invalid", id, index)
			}
			normalized, err := capability.NormalizeOAuth2Requirement(credential.OAuth2)
			if err != nil || !reflect.DeepEqual(normalized, credential.OAuth2) {
				return fmt.Errorf("Skill %s credential %d OAuth 2 requirement is invalid or non-canonical", id, index)
			}
			selectedActions := make(map[string]bool, len(credential.Actions))
			for _, action := range credential.Actions {
				if action != strings.TrimSpace(action) || !actions[action] || selectedActions[action] {
					return fmt.Errorf("Skill %s credential %d references an undeclared action", id, index)
				}
				selectedActions[action] = true
			}
			if existing, ok := credentialNames[credential.Name]; ok {
				if existing.kind != credential.Kind || !sameOAuth2Authorization(existing.oauth2, credential.OAuth2) ||
					existing.all || len(selectedActions) == 0 {
					return fmt.Errorf("Skill %s credential %s has conflicting action-scoped declarations", id, credential.Name)
				}
				for action := range selectedActions {
					if existing.actions[action] {
						return fmt.Errorf("Skill %s credential %s repeats action %s", id, credential.Name, action)
					}
					existing.actions[action] = true
				}
				credentialNames[credential.Name] = existing
			} else {
				credentialNames[credential.Name] = credentialContract{
					kind: credential.Kind, oauth2: credential.OAuth2,
					actions: selectedActions, all: len(selectedActions) == 0,
				}
			}
		}
		adapterIDs := make(map[string]bool, len(skill.ConversationAdapters))
		for index, adapter := range skill.ConversationAdapters {
			if adapter.ID == "" || adapter.ID != strings.TrimSpace(adapter.ID) || len(adapter.ID) > 128 || adapterIDs[adapter.ID] {
				return fmt.Errorf("Skill %s conversation adapter %d has an invalid or duplicate id", id, index)
			}
			normalized, err := capability.NormalizeConversationAdapter(capability.ConversationAdapter{
				ProtocolVersion: adapter.ProtocolVersion,
				Name:            adapter.ID, Description: adapter.ID, Provider: adapter.Provider,
				EndpointModes: adapter.EndpointModes, InboundEventTypes: adapter.InboundEventTypes, Features: adapter.Features,
				Delivery: adapter.Delivery,
				Transport: capability.ConversationAdapterTransport{
					Kind: "authoring", IngressEndpoint: "authoring", DeliveryEndpoint: "authoring",
				},
			})
			if err != nil || normalized.Provider != adapter.Provider ||
				!reflect.DeepEqual(normalized.EndpointModes, adapter.EndpointModes) ||
				!reflect.DeepEqual(normalized.InboundEventTypes, adapter.InboundEventTypes) ||
				!reflect.DeepEqual(normalized.Features, adapter.Features) {
				return fmt.Errorf("Skill %s conversation adapter %d is invalid or non-canonical", id, index)
			}
			endpointModes := make(map[capability.ConversationEndpointMode]bool, len(adapter.EndpointModes))
			for _, mode := range adapter.EndpointModes {
				endpointModes[mode] = true
			}
			seenDiscoverableModes := make(map[capability.ConversationEndpointMode]bool, len(adapter.DiscoverableDestinationModes))
			for modeIndex, mode := range adapter.DiscoverableDestinationModes {
				if !endpointModes[mode] || seenDiscoverableModes[mode] {
					return fmt.Errorf("Skill %s conversation adapter %d has invalid discoverable destination modes", id, index)
				}
				if modeIndex > 0 && adapter.DiscoverableDestinationModes[modeIndex-1] > mode {
					return fmt.Errorf("Skill %s conversation adapter %d discoverable destination modes are not canonical", id, index)
				}
				seenDiscoverableModes[mode] = true
			}
			seenAdapterCredentials := make(map[string]bool, len(adapter.Credentials))
			for credentialIndex, credential := range adapter.Credentials {
				if credential.Name == "" || credential.Name != strings.TrimSpace(credential.Name) ||
					credential.Kind == "" || credential.Kind != strings.TrimSpace(credential.Kind) ||
					len(credential.Name) > 128 || len(credential.Kind) > 128 || seenAdapterCredentials[credential.Name] {
					return fmt.Errorf("Skill %s conversation adapter %d credential %d is invalid", id, index, credentialIndex)
				}
				normalizedOAuth2, oauthErr := capability.NormalizeOAuth2Requirement(credential.OAuth2)
				if oauthErr != nil || !reflect.DeepEqual(normalizedOAuth2, credential.OAuth2) {
					return fmt.Errorf("Skill %s conversation adapter %d credential %d OAuth 2 requirement is invalid or non-canonical", id, index, credentialIndex)
				}
				seenAdapterCredentials[credential.Name] = true
			}
			adapterIDs[adapter.ID] = true
		}
	}
	for bindingKey, grants := range catalog.AvailableCredentialGrants {
		if bindingKey != strings.TrimSpace(bindingKey) || bindingKey == "" || len(bindingKey) > 128 || len(grants) > 32 {
			return fmt.Errorf("available OAuth 2 grants for credential %q are invalid", bindingKey)
		}
		seen := make(map[string]bool, len(grants))
		for index := range grants {
			normalized, err := capability.NormalizeOAuth2GrantSummary(&grants[index])
			if err != nil || !reflect.DeepEqual(normalized, &grants[index]) {
				return fmt.Errorf("available OAuth 2 grant %d for credential %s is invalid or non-canonical", index, bindingKey)
			}
			encoded, _ := json.Marshal(normalized)
			if seen[string(encoded)] {
				return fmt.Errorf("available OAuth 2 grants for credential %s contain a duplicate", bindingKey)
			}
			seen[string(encoded)] = true
		}
	}
	credentialKeys := make(map[string]bool, len(catalog.AgentCredentialRequirements))
	for index, requirement := range catalog.AgentCredentialRequirements {
		key := strings.TrimSpace(requirement.BindingKey)
		name := strings.TrimSpace(requirement.DisplayName)
		prompt := strings.TrimSpace(requirement.Prompt)
		if key != requirement.BindingKey || name != requirement.DisplayName || prompt != requirement.Prompt ||
			!credentialBindingKeyPattern.MatchString(key) || name == "" || prompt == "" ||
			len(name) > 200 || len(prompt) > 1024 || strings.ContainsAny(name+prompt, "\r\n\t") ||
			credentialKeys[key] {
			return fmt.Errorf("Agent credential requirement %d is invalid", index)
		}
		credentialKeys[key] = true
	}
	if len(catalog.CapabilityNeeds) > MaximumCapabilityNeeds {
		return fmt.Errorf("capability catalog has more than %d capability needs", MaximumCapabilityNeeds)
	}
	needIDs := make(map[string]bool, len(catalog.CapabilityNeeds))
	for index, need := range catalog.CapabilityNeeds {
		need.ID, need.Prompt, need.WhyNeeded = strings.TrimSpace(need.ID), strings.TrimSpace(need.Prompt), strings.TrimSpace(need.WhyNeeded)
		if !catalogDiagnosticReferencePattern.MatchString(need.ID) || need.Prompt == "" || need.WhyNeeded == "" ||
			len(need.Prompt) > 1024 || len(need.WhyNeeded) > 1024 || strings.ContainsAny(need.Prompt+need.WhyNeeded, "\r\n\t") ||
			need.Priority < 1 || need.Priority > 1000 {
			return fmt.Errorf("capability catalog need %d is invalid", index)
		}
		if scope := need.SourceScope; scope != nil {
			scope.Prompt, scope.WhyNeeded = strings.TrimSpace(scope.Prompt), strings.TrimSpace(scope.WhyNeeded)
			if scope.Prompt == "" || scope.WhyNeeded == "" || len(scope.Prompt) > 1024 || len(scope.WhyNeeded) > 1024 ||
				strings.ContainsAny(scope.Prompt+scope.WhyNeeded, "\r\n\t") || scope.Minimum < 1 || scope.Maximum < scope.Minimum ||
				scope.Maximum > 100 || scope.Priority < 1 || scope.Priority > 1000 {
				return fmt.Errorf("capability catalog need %d source scope is invalid", index)
			}
			seenInputKeys := make(map[string]bool, len(scope.MaterializationInputKeys))
			seenTargets := make(map[string]bool, len(scope.Targets))
			for _, target := range scope.Targets {
				if target != strings.TrimSpace(target) || target == "" || len(target) > 2048 || strings.ContainsAny(target, "\r\n\t") || seenTargets[target] {
					return fmt.Errorf("capability catalog need %d source scope has invalid targets", index)
				}
				seenTargets[target] = true
			}
			if len(scope.Targets) > 0 && (len(scope.Targets) < scope.Minimum || len(scope.Targets) > scope.Maximum) {
				return fmt.Errorf("capability catalog need %d source scope targets are outside its bounds", index)
			}
			for _, key := range scope.MaterializationInputKeys {
				if key != strings.TrimSpace(key) || !catalogDiagnosticCodePattern.MatchString(key) || seenInputKeys[key] {
					return fmt.Errorf("capability catalog need %d source scope has invalid materialization input keys", index)
				}
				seenInputKeys[key] = true
			}
		}
		if proposal := need.SourcePolicyProposal; proposal != nil {
			proposal.Reason = strings.TrimSpace(proposal.Reason)
			if proposal.Reason == "" || len(proposal.Reason) > 1024 || strings.ContainsAny(proposal.Reason, "\r\n\t") {
				return fmt.Errorf("capability catalog need %d source policy proposal reason is invalid", index)
			}
			if err := proposal.Policy.Validate(); err != nil {
				return fmt.Errorf("capability catalog need %d source policy proposal is invalid: %w", index, err)
			}
			if !proposal.Policy.Enabled {
				return fmt.Errorf("capability catalog need %d source policy proposal must describe an enabled immutable version", index)
			}
			for _, policySource := range proposal.Policy.Sources {
				if len(policySource.PathPrefixes) == 0 || len(policySource.Methods) == 0 {
					return fmt.Errorf("capability catalog need %d source policy proposal requires explicit paths and methods", index)
				}
			}
			allowedSkills := stringSet(need.SkillIDs)
			seenProposalSkills := make(map[string]bool, len(proposal.SkillIDs))
			if len(proposal.SkillIDs) == 0 {
				return fmt.Errorf("capability catalog need %d source policy proposal requires at least one applicable Skill", index)
			}
			for _, skillID := range proposal.SkillIDs {
				if skillID != strings.TrimSpace(skillID) || !allowedSkills[skillID] || seenProposalSkills[skillID] {
					return fmt.Errorf("capability catalog need %d source policy proposal has an invalid applicable Skill", index)
				}
				seenProposalSkills[skillID] = true
			}
		}
		if needIDs[need.ID] {
			return fmt.Errorf("capability catalog need %d duplicates id %s", index, need.ID)
		}
		needIDs[need.ID] = true
		if len(need.SkillIDs) == 0 || len(need.SkillIDs) > MaximumCapabilityNeedSkillChoices {
			return fmt.Errorf("capability catalog need %s must contain between 1 and %d Skill choices", need.ID, MaximumCapabilityNeedSkillChoices)
		}
		seenSkills := make(map[string]bool, len(need.SkillIDs))
		for _, skillID := range need.SkillIDs {
			if skillID != strings.TrimSpace(skillID) || skillID == "" || seenSkills[skillID] {
				return fmt.Errorf("capability catalog need %s contains an invalid or duplicate Skill id", need.ID)
			}
			seenSkills[skillID] = true
			skill, exists := catalog.Skills[skillID]
			if !exists || !refinementSkillChoiceViable(skill) {
				return fmt.Errorf("capability catalog need %s references unavailable or incompatible Skill %s", need.ID, skillID)
			}
		}
	}
	if len(catalog.Diagnostics) > MaximumCatalogDiagnostics {
		return fmt.Errorf("capability catalog has more than %d diagnostics", MaximumCatalogDiagnostics)
	}
	seenDiagnostics := make(map[string]bool, len(catalog.Diagnostics))
	for index, diagnostic := range catalog.Diagnostics {
		code, message, reference := strings.TrimSpace(diagnostic.Code), strings.TrimSpace(diagnostic.Message), strings.TrimSpace(diagnostic.Reference)
		if code != diagnostic.Code || message != diagnostic.Message || reference != diagnostic.Reference ||
			!catalogDiagnosticCodePattern.MatchString(code) || message == "" || len(message) > 1024 || strings.ContainsAny(message, "\r\n\t") ||
			(reference != "" && !catalogDiagnosticReferencePattern.MatchString(reference)) {
			return fmt.Errorf("capability catalog diagnostic %d is invalid", index)
		}
		identity := code + "\x00" + reference
		if seenDiagnostics[identity] {
			return fmt.Errorf("capability catalog diagnostic %d duplicates code and reference", index)
		}
		seenDiagnostics[identity] = true
	}
	return nil
}

func sameOAuth2Authorization(left, right *capability.OAuth2Requirement) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Provider == right.Provider && left.Subject == right.Subject && left.Resource == right.Resource
}

func refinementSkillChoiceViable(skill SkillCapability) bool {
	if skill.Readiness == SkillReadinessUnavailable {
		return false
	}
	for _, evidence := range skill.Compatibility {
		if !evidence.Compatible && !refinementLifecycleGap(skill.Readiness, evidence.Requirement) {
			return false
		}
	}
	return true
}

// Lifecycle gaps are not compatibility failures: they are the explicit work a
// refinement is proposing. Keep this allowlist narrow so action, platform,
// source, compilation, and other semantic incompatibilities still fail closed.
func refinementLifecycleGap(readiness SkillReadiness, requirement string) bool {
	requirement = strings.TrimSpace(requirement)
	if requirement == "installation" {
		return readiness == SkillReadinessNeedsInstallation
	}
	if requirement == "binding_configuration" || strings.HasPrefix(requirement, "credential:") {
		return readiness == SkillReadinessNeedsBinding || readiness == SkillReadinessNeedsInstallation
	}
	return false
}

func refinementCatalogOptionError(questionID, optionID string, skills map[string]SkillCapability) error {
	aliases := make([]string, 0, 1)
	for id := range skills {
		if strings.HasSuffix(id, "."+optionID) || strings.HasSuffix(id, "/"+optionID) {
			aliases = append(aliases, id)
		}
	}
	sort.Strings(aliases)
	if len(aliases) == 1 {
		return fmt.Errorf("refinement question %s uses non-canonical Skill option id %q; use the exact authorized catalog key %q (aliases are not accepted)", questionID, optionID, aliases[0])
	}
	authorized := make([]string, 0, len(skills))
	for id := range skills {
		authorized = append(authorized, id)
	}
	sort.Strings(authorized)
	if len(authorized) > 8 {
		authorized = authorized[:8]
	}
	if len(authorized) == 0 {
		return fmt.Errorf("refinement question %s references Skill option id %q, but the authorized catalog is empty", questionID, optionID)
	}
	return fmt.Errorf("refinement question %s references unknown Skill option id %q; use an exact authorized catalog key (available keys include %s)", questionID, optionID, strings.Join(authorized, ", "))
}

func validQuestionCategory(category RefinementQuestionCategory) bool {
	switch category {
	case RefinementCategoryCredential, RefinementCategorySkill, RefinementCategoryScope, RefinementCategoryPolicy,
		RefinementCategoryAuthority, RefinementCategoryDestination, RefinementCategoryBudget, RefinementCategoryApproval, RefinementCategoryOther:
		return true
	default:
		return false
	}
}

func validAnswerKind(kind RefinementAnswerKind) bool {
	switch kind {
	case RefinementAnswerText, RefinementAnswerStringList, RefinementAnswerSingleSelect, RefinementAnswerMultiSelect,
		RefinementAnswerBoolean, RefinementAnswerCredentialReference, RefinementAnswerSkillSelection:
		return true
	default:
		return false
	}
}

func validateAnswerSchema(schema RefinementAnswerSchema) error {
	if schema.Minimum < 0 || schema.Maximum < 0 || schema.Maximum > 0 && schema.Maximum < schema.Minimum {
		return errors.New("answer cardinality is invalid")
	}
	if schema.Kind != RefinementAnswerSingleSelect && schema.Kind != RefinementAnswerMultiSelect && schema.Kind != RefinementAnswerSkillSelection && len(schema.Options) > 0 {
		return errors.New("only select and Skill-selection answers may declare options")
	}
	if (schema.Kind == RefinementAnswerSingleSelect || schema.Kind == RefinementAnswerMultiSelect) && len(schema.Options) == 0 {
		return errors.New("select answers require options")
	}
	if schema.Pattern != "" {
		if _, err := regexp.Compile(schema.Pattern); err != nil {
			return errors.New("answer pattern is invalid")
		}
	}
	seen := make(map[string]bool, len(schema.Options))
	for _, option := range schema.Options {
		if strings.TrimSpace(option.ID) == "" || strings.TrimSpace(option.Label) == "" || seen[option.ID] {
			return errors.New("answer options require unique ids and labels")
		}
		if schema.Kind != RefinementAnswerSkillSelection && len(option.Actions) > 0 {
			return errors.New("only Skill-selection options may declare actions")
		}
		if len(nonEmptyUnique(option.Actions)) != len(option.Actions) {
			return errors.New("Skill-selection option actions must be non-empty and unique")
		}
		seen[option.ID] = true
	}
	return nil
}

func validateRefinementAnswer(question RefinementQuestion, value RefinementAnswerValue) error {
	populated := 0
	if strings.TrimSpace(value.Text) != "" {
		populated++
	}
	if len(nonEmptyUnique(value.Items)) > 0 {
		populated++
	}
	if len(nonEmptyUnique(value.OptionIDs)) > 0 {
		populated++
	}
	if value.Boolean != nil {
		populated++
	}
	if value.CredentialReference != nil {
		populated++
	}
	if len(nonEmptyUnique(value.SkillIDs)) > 0 {
		populated++
	}
	if populated != 1 {
		return errors.New("refinement answer must populate exactly the field selected by its schema")
	}
	count := 0
	switch question.Answer.Kind {
	case RefinementAnswerText:
		if strings.TrimSpace(value.Text) == "" {
			return errors.New("text answer is required")
		}
		if question.Answer.Pattern != "" && !regexp.MustCompile(question.Answer.Pattern).MatchString(value.Text) {
			return errors.New("text answer does not match the required pattern")
		}
		count = 1
	case RefinementAnswerStringList:
		value.Items = nonEmptyUnique(value.Items)
		count = len(value.Items)
	case RefinementAnswerSingleSelect, RefinementAnswerMultiSelect:
		selected := nonEmptyUnique(value.OptionIDs)
		count = len(selected)
		if question.Answer.Kind == RefinementAnswerSingleSelect && count != 1 {
			return errors.New("single-select answer requires exactly one option")
		}
		options := make(map[string]bool, len(question.Answer.Options))
		for _, option := range question.Answer.Options {
			options[option.ID] = true
		}
		for _, id := range selected {
			if !options[id] {
				return fmt.Errorf("unknown answer option %s", id)
			}
		}
	case RefinementAnswerBoolean:
		if value.Boolean == nil {
			return errors.New("boolean answer is required")
		}
		count = 1
	case RefinementAnswerCredentialReference:
		if value.CredentialReference == nil || strings.TrimSpace(value.CredentialReference.Kind) == "" || strings.TrimSpace(value.CredentialReference.ID) == "" {
			return errors.New("opaque credential reference is required")
		}
		count = 1
	case RefinementAnswerSkillSelection:
		value.SkillIDs = nonEmptyUnique(value.SkillIDs)
		count = len(value.SkillIDs)
		if len(question.Answer.Options) > 0 {
			options := make(map[string]bool, len(question.Answer.Options))
			for _, option := range question.Answer.Options {
				options[option.ID] = true
			}
			for _, id := range value.SkillIDs {
				if !options[id] {
					return fmt.Errorf("unknown Skill option %s", id)
				}
			}
		}
	default:
		return errors.New("unsupported refinement answer kind")
	}
	if question.Answer.Minimum > 0 && count < question.Answer.Minimum {
		return errors.New("answer has too few values")
	}
	if question.Answer.Maximum > 0 && count > question.Answer.Maximum {
		return errors.New("answer has too many values")
	}
	return nil
}

// unansweredRefinementQuestions makes durable answers authoritative even when
// a provider repeats a stable question during refinement. A question is
// suppressed only while its stored answer still validates against the current
// schema; a materially changed question remains visible for review.
func unansweredRefinementQuestions(questions []RefinementQuestion, refinement ChangeSetRefinement) []RefinementQuestion {
	result := make([]RefinementQuestion, 0, len(questions))
	for _, question := range questions {
		answer := refinement.CurrentAnswer(question.ID)
		if answer != nil && validateRefinementAnswer(question, answer.Value) == nil {
			continue
		}
		result = append(result, question)
	}
	return result
}

func normalizeRefinementAnswerValue(value RefinementAnswerValue) RefinementAnswerValue {
	value.Text = strings.TrimSpace(value.Text)
	value.Items = nonEmptyUnique(value.Items)
	value.OptionIDs = nonEmptyUnique(value.OptionIDs)
	if value.CredentialReference != nil {
		value.CredentialReference.Kind = strings.TrimSpace(value.CredentialReference.Kind)
		value.CredentialReference.ID = strings.TrimSpace(value.CredentialReference.ID)
	}
	value.SkillIDs = nonEmptyUnique(value.SkillIDs)
	return value
}

func nonEmptyUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func cloneRefinement(value ChangeSetRefinement) ChangeSetRefinement {
	payload, _ := json.Marshal(value)
	var clone ChangeSetRefinement
	_ = json.Unmarshal(payload, &clone)
	return clone
}
