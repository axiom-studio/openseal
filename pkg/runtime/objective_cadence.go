package runtime

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type ObjectiveCadenceType string

const (
	ObjectiveCadenceInterval ObjectiveCadenceType = "interval"
	ObjectiveCadenceDaily    ObjectiveCadenceType = "daily"
	ObjectiveCadenceWeekly   ObjectiveCadenceType = "weekly"
)

// ObjectiveCadence is the portable schedule contract for recurring objective
// work. Hosts may wake the reconciler however they choose; schedule semantics
// remain identical in standalone and embedded deployments.
type ObjectiveCadence struct {
	Type              ObjectiveCadenceType `json:"type"`
	IntervalSeconds   int64                `json:"intervalSeconds,omitempty"`
	TimeOfDay         string               `json:"timeOfDay,omitempty"`
	DayOfWeek         string               `json:"dayOfWeek,omitempty"`
	Timezone          string               `json:"timezone,omitempty"`
	AssignedAgentID   string               `json:"assignedAgentId,omitempty"`
	RunBudget         *BudgetPolicy        `json:"runBudget,omitempty"`
	MaximumConcurrent int                  `json:"maximumConcurrent,omitempty"`
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
	default:
		return time.Time{}, fmt.Errorf("unsupported objective cadence type %q", c.Type)
	}
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
