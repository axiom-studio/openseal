package runtime

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

type ObjectiveCadenceType string

const (
	ObjectiveCadenceInterval ObjectiveCadenceType = "interval"
	ObjectiveCadenceDaily    ObjectiveCadenceType = "daily"
	ObjectiveCadenceWeekly   ObjectiveCadenceType = "weekly"
	ObjectiveCadenceCron     ObjectiveCadenceType = "cron"
)

// ObjectiveCadence is the portable schedule contract for recurring objective
// work. Hosts may wake the reconciler however they choose; schedule semantics
// remain identical in standalone and embedded deployments.
type ObjectiveCadence struct {
	Type              ObjectiveCadenceType  `json:"type"`
	IntervalSeconds   int64                 `json:"intervalSeconds,omitempty"`
	TimeOfDay         string                `json:"timeOfDay,omitempty"`
	DayOfWeek         string                `json:"dayOfWeek,omitempty"`
	CronExpression    string                `json:"cronExpression,omitempty"`
	Timezone          string                `json:"timezone,omitempty"`
	AssignedAgentID   string                `json:"assignedAgentId,omitempty"`
	RunBudget         *BudgetPolicy         `json:"runBudget,omitempty"`
	RunTemplate       *ObjectiveRunTemplate `json:"runTemplate,omitempty"`
	MaximumConcurrent int                   `json:"maximumConcurrent,omitempty"`
}

// ObjectiveRunTemplate carries immutable, credential-free inputs from a
// recurring Objective into every scheduled Run. It lets SRE monitors,
// research listeners, financial controls, and other domains reuse the same
// scheduler without teaching it domain-specific configuration.
type ObjectiveRunTemplate struct {
	Entrypoint         string                         `json:"entrypoint,omitempty"`
	Context            map[string]interface{}         `json:"context,omitempty"`
	Policy             map[string]interface{}         `json:"policy,omitempty"`
	Capability         *ObjectiveCapabilityInvocation `json:"capability,omitempty"`
	EvidenceProjection *ObjectiveEvidenceProjection   `json:"evidenceProjection,omitempty"`
}

// ObjectiveEvidenceProjection bounds the immutable SourceObservation snapshot
// attached to a scheduled, Initiative-owned, non-monitor Run. Zero values use
// conservative kernel defaults. The scheduler always projects Initiative
// evidence unless Disabled is explicit; callers cannot supply snapshot data.
type ObjectiveEvidenceProjection struct {
	Disabled            bool `json:"disabled,omitempty"`
	MaximumObservations int  `json:"maximumObservations,omitempty"`
	MaximumSummaryRunes int  `json:"maximumSummaryRunes,omitempty"`
	MaximumTotalRunes   int  `json:"maximumTotalRunes,omitempty"`
}

func (p *ObjectiveEvidenceProjection) Validate() error {
	if p == nil {
		return nil
	}
	if p.MaximumObservations < 0 || p.MaximumObservations > 99 ||
		p.MaximumSummaryRunes < 0 || p.MaximumSummaryRunes > 4000 ||
		p.MaximumTotalRunes < 0 || p.MaximumTotalRunes > 100000 {
		return errors.New("objective evidence projection bounds are invalid")
	}
	return nil
}

// ObjectiveCapabilityInvocation is an immutable typed action intent attached
// to scheduled work. The assigned Agent still executes it through normal Skill
// authorization, schema, credential, risk, approval, and idempotency policy.
// Credential values are never part of this template.
type ObjectiveCapabilityInvocation struct {
	SkillID      string                 `json:"skillId"`
	SkillVersion string                 `json:"skillVersion"`
	Action       string                 `json:"action"`
	Inputs       map[string]interface{} `json:"inputs,omitempty"`
}

func (i *ObjectiveCapabilityInvocation) Validate() error {
	if i == nil {
		return nil
	}
	if !validOpaqueIdentifier(i.SkillID, 128) || strings.TrimSpace(i.SkillVersion) == "" || len(i.SkillVersion) > 128 ||
		!validOpaqueIdentifier(i.Action, 128) {
		return errors.New("objective cadence capability requires portable Skill id, version, and action")
	}
	if err := validateCredentialFreeContext(i.Inputs); err != nil {
		return fmt.Errorf("objective cadence capability inputs: %w", err)
	}
	return nil
}

func (t *ObjectiveRunTemplate) Validate() error {
	if t == nil {
		return nil
	}
	if len(strings.TrimSpace(t.Entrypoint)) > 128 {
		return errors.New("objective cadence runTemplate entrypoint cannot exceed 128 characters")
	}
	if _, reserved := t.Context["scheduledFor"]; reserved {
		return errors.New("objective cadence runTemplate context cannot override scheduledFor")
	}
	if _, reserved := t.Context["capabilityInvocation"]; reserved {
		return errors.New("objective cadence runTemplate context cannot override capabilityInvocation")
	}
	if _, reserved := t.Context[EvidenceSnapshotContextKey]; reserved {
		return errors.New("objective cadence runTemplate context cannot override evidenceSnapshot")
	}
	if err := validateCredentialFreeContext(t.Context); err != nil {
		return fmt.Errorf("objective cadence runTemplate context: %w", err)
	}
	if err := validateCredentialFreeContext(t.Policy); err != nil {
		return fmt.Errorf("objective cadence runTemplate policy: %w", err)
	}
	if err := t.Capability.Validate(); err != nil {
		return err
	}
	if err := t.EvidenceProjection.Validate(); err != nil {
		return err
	}
	return nil
}

func (c *ObjectiveCadence) Validate() error {
	if c == nil {
		return nil
	}
	if c.RunBudget != nil {
		if err := c.RunBudget.Validate(); err != nil {
			return fmt.Errorf("objective cadence run budget: %w", err)
		}
	}
	if err := c.RunTemplate.Validate(); err != nil {
		return err
	}
	if err := validateObjectiveCapabilityRunBudget(c.RunTemplate, c.RunBudget); err != nil {
		return err
	}
	if c.MaximumConcurrent < 0 {
		return errors.New("objective cadence maximum concurrency cannot be negative")
	}
	switch c.Type {
	case ObjectiveCadenceInterval:
		if c.IntervalSeconds <= 0 {
			return errors.New("interval objective cadence requires positive intervalSeconds")
		}
	case ObjectiveCadenceDaily:
		if _, _, err := parseObjectiveClock(c.TimeOfDay); err != nil {
			return err
		}
	case ObjectiveCadenceWeekly:
		if _, _, err := parseObjectiveClock(c.TimeOfDay); err != nil {
			return err
		}
		if _, err := objectiveWeekday(c.DayOfWeek); err != nil {
			return err
		}
	case ObjectiveCadenceCron:
		if _, err := parseObjectiveCron(c.CronExpression); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported objective cadence type %q", c.Type)
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("objective cadence timezone: %w", err)
		}
	}
	return nil
}

// A governed capability invocation is deliberately a two-phase durable
// lifecycle: one Turn proposes the ActionCall and a later Turn consumes its
// persisted result. A bounded template must reserve capacity for both phases.
func validateObjectiveCapabilityRunBudget(template *ObjectiveRunTemplate, budget *BudgetPolicy) error {
	if template == nil || template.Capability == nil || budget == nil {
		return nil
	}
	if budget.MaxAttempts > 0 && budget.MaxAttempts < 2 {
		return errors.New("objective capability run budget requires at least 2 attempts when bounded")
	}
	if budget.MaxTurns > 0 && budget.MaxTurns < 2 {
		return errors.New("objective capability run budget requires at least 2 turns when bounded")
	}
	return nil
}

func (c *ObjectiveCadence) Next(from time.Time) (time.Time, error) {
	if err := c.Validate(); err != nil {
		return time.Time{}, err
	}
	loc := time.UTC
	if c.Timezone != "" {
		loc, _ = time.LoadLocation(c.Timezone)
	}
	switch c.Type {
	case ObjectiveCadenceInterval:
		return from.Add(time.Duration(c.IntervalSeconds) * time.Second).UTC(), nil
	case ObjectiveCadenceDaily:
		hour, minute, _ := parseObjectiveClock(c.TimeOfDay)
		local := from.In(loc)
		next := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		if !next.After(local) {
			next = next.AddDate(0, 0, 1)
		}
		return next.UTC(), nil
	case ObjectiveCadenceWeekly:
		hour, minute, _ := parseObjectiveClock(c.TimeOfDay)
		weekday, _ := objectiveWeekday(c.DayOfWeek)
		local := from.In(loc)
		days := (int(weekday) - int(local.Weekday()) + 7) % 7
		next := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc).AddDate(0, 0, days)
		if !next.After(local) {
			next = next.AddDate(0, 0, 7)
		}
		return next.UTC(), nil
	case ObjectiveCadenceCron:
		schedule, _ := parseObjectiveCron(c.CronExpression)
		return schedule.Next(from.In(loc)).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported objective cadence type %q", c.Type)
	}
}

func parseObjectiveCron(value string) (cron.Schedule, error) {
	expression := strings.TrimSpace(value)
	if expression == "" {
		return nil, errors.New("cron objective cadence requires cronExpression")
	}
	if len(expression) > 128 {
		return nil, errors.New("objective cadence cronExpression cannot exceed 128 characters")
	}
	// OpenSeal uses one explicit portable dialect: second, minute, hour,
	// day-of-month, month, day-of-week. Timezone is a typed cadence field,
	// never an implementation-specific CRON_TZ prefix.
	parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("objective cadence cronExpression must contain six valid fields: %w", err)
	}
	return schedule, nil
}

func parseObjectiveClock(value string) (int, int, error) {
	if value == "" {
		value = "09:00"
	}
	var hour, minute int
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("objective cadence timeOfDay must be HH:MM")
	}
	return hour, minute, nil
}

func objectiveWeekday(value string) (time.Weekday, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sunday":
		return time.Sunday, nil
	case "monday":
		return time.Monday, nil
	case "tuesday":
		return time.Tuesday, nil
	case "wednesday":
		return time.Wednesday, nil
	case "thursday":
		return time.Thursday, nil
	case "friday":
		return time.Friday, nil
	case "saturday":
		return time.Saturday, nil
	default:
		return 0, errors.New("weekly objective cadence requires a valid dayOfWeek")
	}
}
