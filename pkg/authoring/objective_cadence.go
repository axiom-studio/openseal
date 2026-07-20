package authoring

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/workforce"
)

// authoredObjectiveCadence mirrors the portable runtime schedule wire
// contract. Authoring validates this boundary before review so a candidate
// accepted by policy cannot fail later merely because its schedule was shaped
// differently from the executable runtime contract.
type authoredObjectiveCadence struct {
	Type              string                        `json:"type"`
	IntervalSeconds   int64                         `json:"intervalSeconds,omitempty"`
	TimeOfDay         string                        `json:"timeOfDay,omitempty"`
	DayOfWeek         string                        `json:"dayOfWeek,omitempty"`
	Timezone          string                        `json:"timezone,omitempty"`
	AssignedAgentID   string                        `json:"assignedAgentId,omitempty"`
	RunBudget         *authoredObjectiveRunBudget   `json:"runBudget,omitempty"`
	RunTemplate       *authoredObjectiveRunTemplate `json:"runTemplate,omitempty"`
	MaximumConcurrent int                           `json:"maximumConcurrent,omitempty"`
}

type authoredObjectiveRunBudget struct {
	MaxAttempts     int64 `json:"maxAttempts,omitempty"`
	MaxTurns        int64 `json:"maxTurns,omitempty"`
	MaxInputTokens  int64 `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
	MaxTotalTokens  int64 `json:"maxTotalTokens,omitempty"`
	MaxCostMicros   int64 `json:"maxCostMicros,omitempty"`
	MaxDurationMS   int64 `json:"maxDurationMs,omitempty"`
	MaxActions      int64 `json:"maxActions,omitempty"`
	WarningPermille int64 `json:"warningPermille,omitempty"`
}

type authoredObjectiveRunTemplate struct {
	Entrypoint         string                                 `json:"entrypoint,omitempty"`
	Context            map[string]interface{}                 `json:"context,omitempty"`
	Policy             map[string]interface{}                 `json:"policy,omitempty"`
	Capability         *authoredObjectiveCapabilityInvocation `json:"capability,omitempty"`
	EvidenceProjection *authoredObjectiveEvidenceProjection   `json:"evidenceProjection,omitempty"`
}

type authoredObjectiveEvidenceProjection struct {
	Disabled            bool `json:"disabled,omitempty"`
	MaximumObservations int  `json:"maximumObservations,omitempty"`
	MaximumSummaryRunes int  `json:"maximumSummaryRunes,omitempty"`
	MaximumTotalRunes   int  `json:"maximumTotalRunes,omitempty"`
}

type authoredObjectiveCapabilityInvocation struct {
	SkillID      string                 `json:"skillId"`
	SkillVersion string                 `json:"skillVersion"`
	Action       string                 `json:"action"`
	Inputs       map[string]interface{} `json:"inputs,omitempty"`
}

type authoredObjectiveEventRules struct {
	Version string                       `json:"version"`
	Rules   []authoredObjectiveEventRule `json:"rules"`
}

type authoredObjectiveEventRule struct {
	ID              string                        `json:"id"`
	EventType       string                        `json:"eventType"`
	Source          string                        `json:"source,omitempty"`
	Subject         string                        `json:"subject,omitempty"`
	Severities      []string                      `json:"severities,omitempty"`
	Attributes      map[string]interface{}        `json:"attributes,omitempty"`
	AssignedAgentID string                        `json:"assignedAgentId,omitempty"`
	RunBudget       *authoredObjectiveRunBudget   `json:"runBudget,omitempty"`
	RunTemplate     *authoredObjectiveRunTemplate `json:"runTemplate,omitempty"`
}

func validateObjectiveTemplateCadences(path string, templates []workforce.ObjectiveTemplate) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	for index, template := range templates {
		if len(template.Cadence) == 0 {
			continue
		}
		if err := validateAuthoredObjectiveCadence(template.Cadence); err != nil {
			issues = append(issues, issue(fmt.Sprintf("%s[%d].cadence", path, index), "invalid_objective_cadence", err.Error()))
		}
	}
	return issues
}

func validateObjectiveTemplateEventRules(path string, templates []workforce.ObjectiveTemplate) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	for index, template := range templates {
		if len(template.EventRules) == 0 {
			continue
		}
		if err := validateAuthoredObjectiveEventRules(template.EventRules); err != nil {
			issues = append(issues, issue(fmt.Sprintf("%s[%d].eventRules", path, index), "invalid_objective_event_rules", err.Error()))
		}
	}
	return issues
}

func validateAuthoredObjectiveEventRules(value map[string]interface{}) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var rules authoredObjectiveEventRules
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return fmt.Errorf("objective eventRules do not match the executable routing contract: %w", err)
	}
	if rules.Version != "1" || len(rules.Rules) == 0 {
		return errors.New("objective eventRules require version 1 and at least one rule")
	}
	for index, rule := range rules.Rules {
		if strings.TrimSpace(rule.ID) == "" || strings.TrimSpace(rule.EventType) == "" {
			return fmt.Errorf("objective eventRules rule %d requires id and eventType", index)
		}
		if template := rule.RunTemplate; template != nil {
			if len(strings.TrimSpace(template.Entrypoint)) > 128 {
				return fmt.Errorf("objective eventRules rule %d runTemplate entrypoint cannot exceed 128 characters", index)
			}
			if capability := template.Capability; capability != nil {
				if strings.TrimSpace(capability.SkillID) == "" || strings.TrimSpace(capability.SkillVersion) == "" || strings.TrimSpace(capability.Action) == "" {
					return fmt.Errorf("objective eventRules rule %d capability requires Skill id, version, and action", index)
				}
				if rule.RunBudget != nil && rule.RunBudget.MaxAttempts > 0 && rule.RunBudget.MaxAttempts < 2 {
					return errors.New("objective capability run budget requires at least 2 attempts when bounded")
				}
				if rule.RunBudget != nil && rule.RunBudget.MaxTurns > 0 && rule.RunBudget.MaxTurns < 2 {
					return errors.New("objective capability run budget requires at least 2 turns when bounded")
				}
			}
			if err := validateAuthoredEvidenceProjection(template.EvidenceProjection); err != nil {
				return fmt.Errorf("objective eventRules rule %d: %w", index, err)
			}
		}
		hosted := rule.RunTemplate == nil || rule.RunTemplate.Capability == nil
		grounded := hosted && rule.RunTemplate != nil && rule.RunTemplate.EvidenceProjection != nil && !rule.RunTemplate.EvidenceProjection.Disabled
		if err := validateAuthoredObjectiveRunBudget(rule.RunBudget, hosted, grounded); err != nil {
			return fmt.Errorf("objective eventRules rule %d: %w", index, err)
		}
	}
	return nil
}

func validateAuthoredObjectiveCadence(value map[string]interface{}) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var cadence authoredObjectiveCadence
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cadence); err != nil {
		return fmt.Errorf("objective cadence does not match the executable schedule contract: %w", err)
	}
	if cadence.MaximumConcurrent < 0 {
		return errors.New("objective cadence maximumConcurrent cannot be negative")
	}
	switch cadence.Type {
	case "interval":
		if cadence.IntervalSeconds <= 0 {
			return errors.New("interval objective cadence requires positive intervalSeconds")
		}
	case "daily":
		if err := validateAuthoredClock(cadence.TimeOfDay); err != nil {
			return err
		}
	case "weekly":
		if err := validateAuthoredClock(cadence.TimeOfDay); err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(cadence.DayOfWeek)) {
		case "sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday":
		default:
			return errors.New("weekly objective cadence requires a valid dayOfWeek")
		}
	default:
		return fmt.Errorf("unsupported objective cadence type %q", cadence.Type)
	}
	if cadence.Timezone != "" {
		if _, err := time.LoadLocation(cadence.Timezone); err != nil {
			return fmt.Errorf("objective cadence timezone: %w", err)
		}
	}
	if template := cadence.RunTemplate; template != nil {
		if len(strings.TrimSpace(template.Entrypoint)) > 128 {
			return errors.New("objective cadence runTemplate entrypoint cannot exceed 128 characters")
		}
		if capability := template.Capability; capability != nil &&
			(strings.TrimSpace(capability.SkillID) == "" || strings.TrimSpace(capability.SkillVersion) == "" || strings.TrimSpace(capability.Action) == "") {
			return errors.New("objective cadence capability requires Skill id, version, and action")
		}
		if template.Capability != nil && cadence.RunBudget != nil {
			if cadence.RunBudget.MaxAttempts > 0 && cadence.RunBudget.MaxAttempts < 2 {
				return errors.New("objective capability run budget requires at least 2 attempts when bounded")
			}
			if cadence.RunBudget.MaxTurns > 0 && cadence.RunBudget.MaxTurns < 2 {
				return errors.New("objective capability run budget requires at least 2 turns when bounded")
			}
		}
		if err := validateAuthoredEvidenceProjection(template.EvidenceProjection); err != nil {
			return err
		}
	}
	hosted := cadence.RunTemplate == nil || cadence.RunTemplate.Capability == nil
	grounded := hosted && cadence.RunTemplate != nil && cadence.RunTemplate.EvidenceProjection != nil && !cadence.RunTemplate.EvidenceProjection.Disabled
	if err := validateAuthoredObjectiveRunBudget(cadence.RunBudget, hosted, grounded); err != nil {
		return fmt.Errorf("objective cadence: %w", err)
	}
	return nil
}

const (
	minimumHostedObjectiveInputTokens    int64 = 16000
	minimumHostedObjectiveOutputTokens   int64 = 1000
	minimumGroundedObjectiveInputTokens  int64 = 32000
	minimumGroundedObjectiveOutputTokens int64 = 30000
	minimumGroundedObjectiveAttempts     int64 = 5
	minimumGroundedObjectiveTurns        int64 = 4
)

func validateAuthoredObjectiveRunBudget(budget *authoredObjectiveRunBudget, hosted, grounded bool) error {
	if budget == nil {
		return nil
	}
	if budget.MaxAttempts < 0 || budget.MaxTurns < 0 || budget.MaxInputTokens < 0 || budget.MaxOutputTokens < 0 ||
		budget.MaxTotalTokens < 0 || budget.MaxCostMicros < 0 || budget.MaxDurationMS < 0 || budget.MaxActions < 0 {
		return errors.New("runBudget limits cannot be negative")
	}
	if budget.WarningPermille < 0 || budget.WarningPermille > 1000 {
		return errors.New("runBudget warningPermille must be between 0 and 1000")
	}
	if !hosted {
		return nil
	}
	minimumInput := minimumHostedObjectiveInputTokens
	minimumOutput := minimumHostedObjectiveOutputTokens
	if grounded {
		minimumInput = minimumGroundedObjectiveInputTokens
		minimumOutput = minimumGroundedObjectiveOutputTokens
		// A grounded completion has four durable phases in the repair path:
		// draft, semantic review, repair, and re-review. Attempts are Run
		// claims rather than completed Turns, so reserve one additional claim
		// for a transient provider or transport retry.
		if budget.MaxAttempts > 0 && budget.MaxAttempts < minimumGroundedObjectiveAttempts {
			return fmt.Errorf("evidence-grounded hosted runBudget maxAttempts must be zero (unbounded) or at least %d", minimumGroundedObjectiveAttempts)
		}
		if budget.MaxTurns > 0 && budget.MaxTurns < minimumGroundedObjectiveTurns {
			return fmt.Errorf("evidence-grounded hosted runBudget maxTurns must be zero (unbounded) or at least %d", minimumGroundedObjectiveTurns)
		}
	}
	if budget.MaxInputTokens > 0 && budget.MaxInputTokens < minimumInput {
		return fmt.Errorf("hosted runBudget maxInputTokens must be zero (unbounded) or at least %d", minimumInput)
	}
	if budget.MaxOutputTokens > 0 && budget.MaxOutputTokens < minimumOutput {
		return fmt.Errorf("hosted runBudget maxOutputTokens must be zero (unbounded) or at least %d", minimumOutput)
	}
	if budget.MaxTotalTokens > 0 {
		input := budget.MaxInputTokens
		if input == 0 {
			input = minimumInput
		}
		output := budget.MaxOutputTokens
		if output == 0 {
			output = minimumOutput
		}
		if budget.MaxTotalTokens < input+output {
			return fmt.Errorf("hosted runBudget maxTotalTokens must be at least maxInputTokens + maxOutputTokens (%d)", input+output)
		}
	}
	return nil
}

func validateAuthoredEvidenceProjection(projection *authoredObjectiveEvidenceProjection) error {
	if projection == nil {
		return nil
	}
	if projection.MaximumObservations < 0 || projection.MaximumObservations > 99 ||
		projection.MaximumSummaryRunes < 0 || projection.MaximumSummaryRunes > 4000 ||
		projection.MaximumTotalRunes < 0 || projection.MaximumTotalRunes > 100000 {
		return errors.New("objective evidenceProjection bounds are invalid")
	}
	return nil
}

func validateAuthoredClock(value string) error {
	if value == "" {
		value = "09:00"
	}
	var hour, minute int
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return errors.New("objective cadence timeOfDay must be HH:MM")
	}
	return nil
}
