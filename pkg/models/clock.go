package models

import "time"

// Clock reports the current time. Runtime services read time only through
// the Clock they were built with, so the wiring can give the whole engine
// one source of time and a test can replace it with a fake one.
type Clock func() time.Time

// ServiceOption configures a runtime service when it is built. Every
// service constructor that reads time accepts these options.
type ServiceOption func(*serviceOptions)

type serviceOptions struct {
	clock Clock
}

// WithClock makes a service read time from clock instead of time.Now. A
// service that builds other services passes the same clock on to them. A nil
// clock keeps time.Now.
func WithClock(clock Clock) ServiceOption {
	return func(o *serviceOptions) { o.clock = clock }
}

// ClockFrom returns the clock that opts select, or time.Now when none does.
func ClockFrom(opts []ServiceOption) Clock {
	var resolved serviceOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&resolved)
		}
	}
	if resolved.clock == nil {
		return time.Now
	}
	return resolved.clock
}
