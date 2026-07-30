package agent

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

var versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`)
var standingOperationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

type SkillRequirement struct {
	SkillID           string   `json:"skillId"`
	VersionConstraint string   `json:"versionConstraint,omitempty"`
	RequiredActions   []string `json:"requiredActions,omitempty"`
	PromptRequired    bool     `json:"promptRequired,omitempty"`
	Optional          bool     `json:"optional,omitempty"`
}

// StandingActionGrant records reviewed, durable authority for one exact Skill
// action. ExternalOperation and ResourcePrefix narrow stable external writes to
// one semantic operation and destination family; both must be present together.
// Host target policy, binding restrictions, and idempotency remain independently
// authoritative.
type StandingActionGrant struct {
	ID                string `json:"id"`
	SkillID           string `json:"skillId"`
	Action            string `json:"action"`
	ExternalOperation string `json:"externalOperation,omitempty"`
	ResourcePrefix    string `json:"resourcePrefix,omitempty"`
}

// ApprovalDestination selects an external conversation endpoint as a
// notification and decision surface. Provider credentials and identity
// mappings remain on the endpoint, outside the immutable Agent definition.
type ApprovalDestination struct {
	EndpointID string `json:"endpointId"`
}

// ApprovalTimeoutPolicy defines the reviewed fallback for an unanswered
// approval. It is opt-in: omitting it preserves fail-closed expiration.
type ApprovalTimeoutPolicy struct {
	AfterSeconds int64  `json:"afterSeconds" jsonschema:"Seconds a pending approval remains open before applying its reviewed timeout decision."`
	Decision     string `json:"decision" jsonschema:"Reviewed timeout outcome. Use approve only when the user explicitly requested automatic approval; otherwise omit this policy."`
}

type AuthorityPolicy struct {
	MaximumRisk          capability.RiskLevel   `json:"maximumRisk"`
	AllowedSkillIDs      []string               `json:"allowedSkillIds,omitempty"`
	MaxConcurrentRuns    int                    `json:"maxConcurrentRuns"`
	BudgetCeilings       map[string]float64     `json:"budgetCeilings,omitempty"`
	RequireApprovalAt    capability.RiskLevel   `json:"requireApprovalAt,omitempty"`
	StandingGrants       []StandingActionGrant  `json:"standingGrants,omitempty"`
	ApprovalDestinations []ApprovalDestination  `json:"approvalDestinations,omitempty"`
	ApprovalTimeout      *ApprovalTimeoutPolicy `json:"approvalTimeout,omitempty"`
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

type ObjectiveTemplate = workforce.ObjectiveTemplate
type EvaluationCriterion = workforce.EvaluationCriterion
type AmendmentPolicy = workforce.AmendmentPolicy
type DefinitionProvenance = workforce.DefinitionProvenance

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
	Runbook             *runbook.Definition    `json:"runbook,omitempty"`
	Amendments          AmendmentPolicy        `json:"amendments,omitempty"`
	Provenance          DefinitionProvenance   `json:"provenance,omitempty"`
	Digest              string                 `json:"digest,omitempty"`
	CreatedAt           time.Time              `json:"createdAt,omitempty"`
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
	seenGrants := make(map[string]bool, len(d.Authority.StandingGrants))
	for _, grant := range d.Authority.StandingGrants {
		if strings.TrimSpace(grant.ID) == "" || strings.TrimSpace(grant.SkillID) == "" || strings.TrimSpace(grant.Action) == "" || seenGrants[grant.ID] {
			return errors.New("agent standing authority grants require unique ids, skill ids, and actions")
		}
		seenGrants[grant.ID] = true
		if (strings.TrimSpace(grant.ExternalOperation) == "") != (strings.TrimSpace(grant.ResourcePrefix) == "") {
			return errors.New("agent standing authority external operation and resource prefix must be specified together")
		}
		if grant.ExternalOperation != "" {
			parsed, err := url.Parse(strings.TrimSpace(grant.ResourcePrefix))
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !standingOperationPattern.MatchString(strings.TrimSpace(grant.ExternalOperation)) {
				return errors.New("agent standing authority external writes require a bounded operation and credential-free HTTP(S) resource prefix")
			}
		}
		if err := validateNoSecrets(map[string]interface{}{
			"externalOperation": grant.ExternalOperation,
			"resourcePrefix":    grant.ResourcePrefix,
		}, "authority.standingGrants"); err != nil {
			return err
		}
	}
	seenDestinations := make(map[string]bool, len(d.Authority.ApprovalDestinations))
	for _, destination := range d.Authority.ApprovalDestinations {
		id := strings.TrimSpace(destination.EndpointID)
		if id == "" || len(id) > 256 || seenDestinations[id] {
			return errors.New("agent approval destinations require unique portable endpoint ids")
		}
		seenDestinations[id] = true
	}
	if timeout := d.Authority.ApprovalTimeout; timeout != nil {
		if timeout.AfterSeconds <= 0 || timeout.AfterSeconds > int64((30*24*time.Hour)/time.Second) || (timeout.Decision != "expire" && timeout.Decision != "approve") {
			return errors.New("agent approval timeout requires a positive duration and an expire or approve decision")
		}
	}
	if d.Memory.Retention < 0 || d.Memory.MaximumBytes < 0 || d.Escalation.AfterFailures < 0 || d.Escalation.AfterDuration < 0 {
		return errors.New("agent definition memory and escalation limits cannot be negative")
	}
	if d.Amendments.RequiresApproval && len(d.Amendments.ApproverPrincipals) == 0 {
		return errors.New("agent definition amendment approval requires eligible principals")
	}
	if err := validateNoSecrets(d.DomainContext, "domainContext"); err != nil {
		return err
	}
	seenSkills := make(map[string]bool)
	requiredActions := make(map[string]map[string]bool)
	for _, requirement := range d.SkillRequirements {
		if strings.TrimSpace(requirement.SkillID) == "" || seenSkills[requirement.SkillID] {
			return errors.New("agent definition skill requirements require unique skill ids")
		}
		seenSkills[requirement.SkillID] = true
		requiredActions[requirement.SkillID] = make(map[string]bool, len(requirement.RequiredActions))
		for _, action := range requirement.RequiredActions {
			requiredActions[requirement.SkillID][action] = true
		}
	}
	for _, grant := range d.Authority.StandingGrants {
		if !requiredActions[grant.SkillID][grant.Action] {
			return fmt.Errorf("agent standing authority grant %s must reference an exact required Skill action", grant.ID)
		}
	}
	if d.Runbook != nil {
		if diagnostics := runbook.Validate(d.Runbook); len(diagnostics) > 0 {
			return fmt.Errorf("agent definition runbook %s: %s", diagnostics[0].Path, diagnostics[0].Message)
		}
		for stepID, step := range d.Runbook.Steps {
			if step.Action != nil && !seenSkills[step.Action.SkillID] {
				return fmt.Errorf("agent definition runbook step %s uses undeclared Skill %s", stepID, step.Action.SkillID)
			}
		}
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
