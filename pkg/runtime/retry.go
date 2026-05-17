package runtime

import "time"

// RetryPolicy defines how failed runs should be retried.
type RetryPolicy struct {
	MaxRetries  int           `json:"maxRetries"`
	BaseDelay   time.Duration `json:"baseDelay"`
	MaxDelay    time.Duration `json:"maxDelay"`
	Multiplier  float64       `json:"multiplier"`
	Retryable   func(error) bool
}

// DefaultRetryPolicy returns a sensible default retry configuration.
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		MaxRetries: 3,
		BaseDelay:  1 * time.Second,
		MaxDelay:   30 * time.Second,
		Multiplier: 2.0,
		Retryable:  func(err error) bool { return err != nil },
	}
}

// NextDelay computes the backoff delay for a given retry count.
func (p *RetryPolicy) NextDelay(retryCount int) time.Duration {
	if retryCount >= p.MaxRetries {
		return 0
	}
	delay := p.BaseDelay
	for i := 0; i < retryCount; i++ {
		delay = time.Duration(float64(delay) * p.Multiplier)
	}
	if delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	return delay
}

// ShouldRetry returns true if the run should be retried.
func (p *RetryPolicy) ShouldRetry(retryCount int, err error) bool {
	if retryCount >= p.MaxRetries {
		return false
	}
	if p.Retryable == nil {
		return err != nil
	}
	return p.Retryable(err)
}
