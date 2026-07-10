package agent

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

var versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`)

type SkillRequirement struct {
	SkillID           string   `json:"skillId"`
	VersionConstraint string   `json:"versionConstraint,omitempty"`
	RequiredActions   []string `json:"requiredActions,omitempty"`
	PromptRequired    bool     `json:"promptRequired,omitempty"`
	Optional          bool     `json:"optional,omitempty"`
}

type AuthorityPolicy struct {
	MaximumRisk       capability.RiskLevel `json:"maximumRisk"`
	AllowedSkillIDs   []string             `json:"allowedSkillIds,omitempty"`
	MaxConcurrentRuns int                  `json:"maxConcurrentRuns"`
	BudgetCeilings    map[string]float64   `json:"budgetCeilings,omitempty"`
	RequireApprovalAt capability.RiskLevel `json:"requireApprovalAt,omitempty"`
}

type MemoryPolicy struct {
	Retention        time.Duration `json:"retention,omitempty"`
	MaximumBytes     int64         `json:"maximumBytes,omitempty"`
	AllowSharedRead  bool          `json:"allowSharedRead,omitempty"`
	AllowSharedWrite bool          `json:"allowSharedWrite,omitempty"`
}

type EscalationPolicy struct {
	AfterFailures int           `json:"afterFailures,omitempty"`
	AfterDuration time.Duration `json:"afterDuration,omitempty"`
	Recipient     string        `json:"recipient,omitempty"`
}

type ObjectiveTemplate struct {
	ID              string                 `json:"id"`
	Title           string                 `json:"title"`
	Goal            string                 `json:"goal"`
	Priority        int                    `json:"priority,omitempty"`
	Cadence         map[string]interface{} `json:"cadence,omitempty"`
	EventRules      map[string]interface{} `json:"eventRules,omitempty"`
	SuccessCriteria map[string]interface{} `json:"successCriteria,omitempty"`
	Constraints     map[string]interface{} `json:"constraints,omitempty"`
}

type EvaluationCriterion struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Weight      float64 `json:"weight,omitempty"`
	Required    bool    `json:"required,omitempty"`
}

type AmendmentPolicy struct {
	AgentMayPropose   bool                 `json:"agentMayPropose,omitempty"`
	AllowedFields     []string             `json:"allowedFields,omitempty"`
	RequiresApproval  bool                 `json:"requiresApproval,omitempty"`
	AutoActivateSafe  bool                 `json:"autoActivateSafe,omitempty"`
	MaximumRiskChange capability.RiskLevel `json:"maximumRiskChange,omitempty"`
}

type DefinitionProvenance struct {
	Source      string `json:"source,omitempty"`
	Reference   string `json:"reference,omitempty"`
	CreatedBy   string `json:"createdBy,omitempty"`
	DerivedFrom string `json:"derivedFrom,omitempty"`
}

// AgentDefinition is immutable behavior. It deliberately excludes credentials,
// placement, health, active runs, and every other tenant-local mutable value.
type AgentDefinition struct {
	ID                  string                 `json:"id"`
	Version             string                 `json:"version"`
	DisplayName         string                 `json:"displayName"`
	Purpose             string                 `json:"purpose"`
	SystemPrompt        string                 `json:"systemPrompt"`
	Personality         string                 `json:"personality,omitempty"`
	OperatingPrinciples []string               `json:"operatingPrinciples,omitempty"`
	DomainContext       map[string]interface{} `json:"domainContext,omitempty"`
	SkillRequirements   []SkillRequirement     `json:"skillRequirements,omitempty"`
	Authority           AuthorityPolicy        `json:"authority"`
	Memory              MemoryPolicy           `json:"memory,omitempty"`
	Escalation          EscalationPolicy       `json:"escalation,omitempty"`
	ObjectiveTemplates  []ObjectiveTemplate    `json:"objectiveTemplates,omitempty"`
	Evaluations         []EvaluationCriterion  `json:"evaluations,omitempty"`
	Amendments          AmendmentPolicy        `json:"amendments,omitempty"`
	Provenance          DefinitionProvenance   `json:"provenance,omitempty"`
	Digest              string                 `json:"digest"`
	CreatedAt           time.Time              `json:"createdAt"`
}

func (d *AgentDefinition) Validate() error {
	if d == nil {
		return errors.New("agent definition is required")
	}
	if strings.TrimSpace(d.ID) == "" || !versionPattern.MatchString(strings.TrimSpace(d.Version)) || strings.TrimSpace(d.DisplayName) == "" || strings.TrimSpace(d.Purpose) == "" || strings.TrimSpace(d.SystemPrompt) == "" {
		return errors.New("agent definition id, valid version, display name, purpose, and system prompt are required")
	}
	if !validRisk(d.Authority.MaximumRisk) || d.Authority.MaxConcurrentRuns < 1 {
		return errors.New("agent definition authority requires maximum risk and positive concurrency")
	}
	if d.Authority.RequireApprovalAt != "" && !validRisk(d.Authority.RequireApprovalAt) {
		return errors.New("agent definition approval risk is invalid")
	}
	if d.Memory.Retention < 0 || d.Memory.MaximumBytes < 0 || d.Escalation.AfterFailures < 0 || d.Escalation.AfterDuration < 0 {
		return errors.New("agent definition memory and escalation limits cannot be negative")
	}
	if err := validateNoSecrets(d.DomainContext, "domainContext"); err != nil {
		return err
	}
	seenSkills := make(map[string]bool)
	for _, requirement := range d.SkillRequirements {
		if strings.TrimSpace(requirement.SkillID) == "" || seenSkills[requirement.SkillID] {
			return errors.New("agent definition skill requirements require unique skill ids")
		}
		seenSkills[requirement.SkillID] = true
	}
	if !isSubset(d.Authority.AllowedSkillIDs, mapKeys(seenSkills)) {
		return errors.New("authority allowed skills must be declared skill requirements")
	}
	seenTemplates := make(map[string]bool)
	for _, template := range d.ObjectiveTemplates {
		if strings.TrimSpace(template.ID) == "" || strings.TrimSpace(template.Title) == "" || strings.TrimSpace(template.Goal) == "" || template.Priority < 0 || seenTemplates[template.ID] {
			return errors.New("objective templates require unique ids, title, goal, and non-negative priority")
		}
		for field, value := range map[string]interface{}{
			"cadence": template.Cadence, "eventRules": template.EventRules,
			"successCriteria": template.SuccessCriteria, "constraints": template.Constraints,
		} {
			if err := validateNoSecrets(value, "objectiveTemplates."+template.ID+"."+field); err != nil {
				return err
			}
		}
		seenTemplates[template.ID] = true
	}
	for name, ceiling := range d.Authority.BudgetCeilings {
		if strings.TrimSpace(name) == "" || ceiling < 0 {
			return errors.New("authority budget ceilings require names and non-negative values")
		}
	}
	return nil
}

func validRisk(value capability.RiskLevel) bool {
	switch value {
	case capability.RiskLevelRead, capability.RiskLevelWrite, capability.RiskLevelExternal, capability.RiskLevelProduction, capability.RiskLevelDestructive:
		return true
	default:
		return false
	}
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

func validateNoSecrets(value interface{}, path string) error {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			if normalized == "token" || strings.HasSuffix(normalized, "apikey") || strings.HasSuffix(normalized, "password") || strings.HasSuffix(normalized, "secret") || strings.HasSuffix(normalized, "credential") || strings.HasSuffix(normalized, "credentialid") || strings.HasSuffix(normalized, "accesstoken") || strings.HasSuffix(normalized, "refreshtoken") {
				return fmt.Errorf("agent definition %s.%s cannot contain credentials", path, key)
			}
			if err := validateNoSecrets(child, path+"."+key); err != nil {
				return err
			}
		}
	case []interface{}:
		for index, child := range typed {
			if err := validateNoSecrets(child, fmt.Sprintf("%s.%d", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizedStrings(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func isSubset(values, allowed []string) bool {
	set := make(map[string]bool, len(allowed))
	for _, value := range allowed {
		set[value] = true
	}
	for _, value := range values {
		if !set[value] {
			return false
		}
	}
	return true
}

func mapKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}
