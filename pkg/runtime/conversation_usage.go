package runtime

import (
	"errors"
	"math"
)

// Concurrent proposals consume additive tokens and provider time, but elapsed
// turn time is a wall-clock duration. The turn coordinator supplies the complete
// elapsed time; never multiply it by the number of participants.
func addParticipationUsage(total, next TurnUsage) (TurnUsage, error) {
	if err := total.Validate(); err != nil {
		return total, err
	}
	if err := next.Validate(); err != nil {
		return total, err
	}
	result := total
	pairs := []struct {
		target *int
		value  int
	}{
		{&result.InputTokens, next.InputTokens}, {&result.OutputTokens, next.OutputTokens},
		{&result.RepairAttempts, next.RepairAttempts}, {&result.NativeOperations, next.NativeOperations},
	}
	for _, pair := range pairs {
		if *pair.target > math.MaxInt-pair.value {
			return total, errors.New("participation usage exceeds accounting capacity")
		}
		*pair.target += pair.value
	}
	durations := []struct {
		target *int64
		value  int64
	}{
		{&result.ProviderDurationMS, next.ProviderDurationMS}, {&result.ValidationDurationMS, next.ValidationDurationMS},
	}
	for _, pair := range durations {
		if *pair.target > math.MaxInt64-pair.value {
			return total, errors.New("participation duration exceeds accounting capacity")
		}
		*pair.target += pair.value
	}
	result.DurationMS = max(total.DurationMS, next.DurationMS)
	result.Cost += next.Cost
	if err := result.Validate(); err != nil {
		return total, err
	}
	return result, nil
}
