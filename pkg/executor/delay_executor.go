package executor

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// DelayExecutor pauses execution for a specified duration
// Config: {"seconds": 5} or {"milliseconds": 500}
type DelayExecutor struct{}

func (e *DelayExecutor) Type() string {
	return StepTypeDelay
}

func (e *DelayExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("delay step requires config")
	}

	var duration time.Duration

	if seconds, ok := config["seconds"]; ok {
		secs, err := e.toFloat(seconds, resolver)
		if err != nil {
			return nil, fmt.Errorf("invalid seconds value: %w", err)
		}
		duration = time.Duration(secs * float64(time.Second))
	} else if ms, ok := config["milliseconds"]; ok {
		msVal, err := e.toFloat(ms, resolver)
		if err != nil {
			return nil, fmt.Errorf("invalid milliseconds value: %w", err)
		}
		duration = time.Duration(msVal * float64(time.Millisecond))
	} else {
		return nil, fmt.Errorf("delay step requires 'seconds' or 'milliseconds'")
	}

	// Cap at 5 minutes to prevent runaway delays
	maxDuration := 5 * time.Minute
	if duration > maxDuration {
		duration = maxDuration
	}

	select {
	case <-time.After(duration):
		return &StepResult{
			Output: map[string]interface{}{
				"delayed":  true,
				"duration": duration.Milliseconds(),
			},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *DelayExecutor) toFloat(v interface{}, resolver TemplateResolver) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case int:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case string:
		resolved := resolver.ResolveString(val)
		return strconv.ParseFloat(resolved, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float", v)
	}
}
