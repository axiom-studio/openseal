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
	Entrypoint string                                 `json:"entrypoint,omitempty"`
	Context    map[string]interface{}                 `json:"context,omitempty"`
	Policy     map[string]interface{}                 `json:"policy,omitempty"`
	Capability *authoredObjectiveCapabilityInvocation `json:"capability,omitempty"`
}

type authoredObjectiveCapabilityInvocation struct {
	SkillID      string                 `json:"skillId"`
	SkillVersion string                 `json:"skillVersion"`
	Action       string                 `json:"action"`
	Inputs       map[string]interface{} `json:"inputs,omitempty"`
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
	if budget := cadence.RunBudget; budget != nil {
		if budget.MaxAttempts < 0 || budget.MaxTurns < 0 || budget.MaxInputTokens < 0 || budget.MaxOutputTokens < 0 ||
			budget.MaxTotalTokens < 0 || budget.MaxCostMicros < 0 || budget.MaxDurationMS < 0 || budget.MaxActions < 0 {
			return errors.New("objective cadence runBudget limits cannot be negative")
		}
		if budget.WarningPermille < 0 || budget.WarningPermille > 1000 {
			return errors.New("objective cadence runBudget warningPermille must be between 0 and 1000")
		}
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
