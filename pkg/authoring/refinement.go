package authoring

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func reconcileRefinement(current ChangeSetRefinement, result *CompileResult) ChangeSetRefinement {
	questions := append([]RefinementQuestion(nil), result.UnresolvedQuestions...)
	for _, prompt := range result.Questions {
		prompt = strings.TrimSpace(prompt)
		if prompt == "" {
			continue
		}
		sum := sha256.Sum256([]byte(prompt))
		questions = append(questions, RefinementQuestion{
			ID: "legacy-" + hex.EncodeToString(sum[:8]), Category: RefinementCategoryOther,
			Prompt: prompt, WhyNeeded: "Additional information is required to complete the workforce candidate.",
			Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerText}, Priority: 1,
			Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}},
		})
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
		if q.ID == "" || q.Prompt == "" || q.WhyNeeded == "" || q.Priority < 1 || q.Priority > 1000 {
			return errors.New("refinement questions require id, prompt, why-needed, and positive priority")
		}
		if ids[q.ID] {
			return fmt.Errorf("duplicate refinement question %s", q.ID)
		}
		ids[q.ID] = true
		if !validQuestionCategory(q.Category) || !validAnswerKind(q.Answer.Kind) || len(q.Blocking) == 0 {
			return fmt.Errorf("refinement question %s has an invalid category, answer kind, or blocking scope", q.ID)
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
	for id, skill := range catalog.Skills {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(skill.ID) == "" || id != skill.ID {
			return errors.New("Skill catalog keys must match non-empty Skill ids")
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
	}
	for _, question := range questions {
		if question.Answer.Kind != RefinementAnswerSkillSelection {
			continue
		}
		for _, option := range question.Answer.Options {
			skill, ok := catalog.Skills[option.ID]
			if !ok {
				return fmt.Errorf("refinement question %s references Skill %s outside the authorized catalog", question.ID, option.ID)
			}
			if skill.Readiness == SkillReadinessUnavailable {
				return fmt.Errorf("refinement question %s presents unavailable Skill %s", question.ID, option.ID)
			}
		}
	}
	return nil
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
