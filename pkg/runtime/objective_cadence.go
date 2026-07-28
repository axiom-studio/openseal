package runtime

import (
	"crypto/sha256"
	"encoding/binary"
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
	JitterSeconds     int64                 `json:"jitterSeconds,omitempty"`
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
	if strings.TrimSpace(t.Entrypoint) != "" && t.Capability != nil {
		return errors.New("objective cadence runTemplate must choose either an Agent Runbook entrypoint or a direct Skill capability")
	}
	if _, reserved := t.Context["scheduledFor"]; reserved {
		return errors.New("objective cadence runTemplate context cannot override scheduledFor")
	}
	if _, reserved := t.Context["scheduleWindowStart"]; reserved {
		return errors.New("objective cadence runTemplate context cannot override scheduleWindowStart")
	}
	if _, reserved := t.Context["scheduleWindowEnd"]; reserved {
		return errors.New("objective cadence runTemplate context cannot override scheduleWindowEnd")
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
	if c.JitterSeconds < 0 {
		return errors.New("objective cadence jitterSeconds cannot be negative")
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
		if c.JitterSeconds >= 24*60*60 {
			return errors.New("daily objective cadence jitterSeconds must be less than one day")
		}
	case ObjectiveCadenceWeekly:
		if _, _, err := parseObjectiveClock(c.TimeOfDay); err != nil {
			return err
		}
		if _, err := objectiveWeekday(c.DayOfWeek); err != nil {
			return err
		}
		if c.JitterSeconds >= 7*24*60*60 {
			return errors.New("weekly objective cadence jitterSeconds must be less than one week")
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
	if c.JitterSeconds > 0 {
		if c.Type != ObjectiveCadenceDaily && c.Type != ObjectiveCadenceWeekly {
			return errors.New("objective cadence jitterSeconds is supported only for daily and weekly schedules")
		}
		if strings.TrimSpace(c.Timezone) == "" {
			return errors.New("jittered objective cadence requires an explicit timezone")
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
	return c.NextFor("", from)
}

// NextFor selects the next schedule occurrence. Jitter is derived from the
// stable Objective key and the unjittered local calendar occurrence so every
// replica and restart agrees without storing random worker state.
func (c *ObjectiveCadence) NextFor(objectiveKey string, from time.Time) (time.Time, error) {
	if err := c.Validate(); err != nil {
		return time.Time{}, err
	}
	if c.JitterSeconds > 0 && strings.TrimSpace(objectiveKey) == "" {
		return time.Time{}, errors.New("jittered objective cadence requires a stable objective key")
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
		base := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		next := c.jitterOccurrence(objectiveKey, base)
		if !next.After(from) {
			base = base.AddDate(0, 0, 1)
			next = c.jitterOccurrence(objectiveKey, base)
		}
		return next.UTC(), nil
	case ObjectiveCadenceWeekly:
		hour, minute, _ := parseObjectiveClock(c.TimeOfDay)
		weekday, _ := objectiveWeekday(c.DayOfWeek)
		local := from.In(loc)
		daysSince := (int(local.Weekday()) - int(weekday) + 7) % 7
		base := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc).AddDate(0, 0, -daysSince)
		next := c.jitterOccurrence(objectiveKey, base)
		if !next.After(from) {
			base = base.AddDate(0, 0, 7)
			next = c.jitterOccurrence(objectiveKey, base)
		}
		return next.UTC(), nil
	case ObjectiveCadenceCron:
		schedule, _ := parseObjectiveCron(c.CronExpression)
		return schedule.Next(from.In(loc)).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported objective cadence type %q", c.Type)
	}
}

func (c *ObjectiveCadence) jitterOccurrence(objectiveKey string, base time.Time) time.Time {
	if c.JitterSeconds <= 0 {
		return base
	}
	seed := strings.Join([]string{objectiveKey, string(c.Type), c.TimeOfDay, c.DayOfWeek, c.Timezone, base.UTC().Format(time.RFC3339Nano)}, "\x00")
	digest := sha256.Sum256([]byte(seed))
	offset := binary.BigEndian.Uint64(digest[:8]) % uint64(c.JitterSeconds+1)
	return base.Add(time.Duration(offset) * time.Second)
}

// OccurrenceWindow returns the authorized local-calendar window that produced
// an occurrence. It is used for audit and UI projection; selection remains a
// single instant and does not authorize additional Runs within the window.
func (c *ObjectiveCadence) OccurrenceWindow(occurrence time.Time) (time.Time, time.Time, error) {
	if err := c.Validate(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if c.JitterSeconds == 0 {
		value := occurrence.UTC()
		return value, value, nil
	}
	loc, _ := time.LoadLocation(c.Timezone)
	local := occurrence.In(loc)
	hour, minute, _ := parseObjectiveClock(c.TimeOfDay)
	base := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if c.Type == ObjectiveCadenceWeekly {
		weekday, _ := objectiveWeekday(c.DayOfWeek)
		daysSince := (int(local.Weekday()) - int(weekday) + 7) % 7
		base = base.AddDate(0, 0, -daysSince)
		if base.After(local) {
			base = base.AddDate(0, 0, -7)
		}
	} else if base.After(local) {
		base = base.AddDate(0, 0, -1)
	}
	return base.UTC(), base.Add(time.Duration(c.JitterSeconds) * time.Second).UTC(), nil
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
