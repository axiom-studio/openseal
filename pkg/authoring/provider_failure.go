package authoring

import (
	"errors"
	"fmt"
)

// ProviderFailureKind is a secret-free classification supplied by the
// authoring provider boundary. The underlying error remains available to
// operators, while durable ChangeSet and Run state persist only the bounded
// public mapping in classifyGenerationFailure.
type ProviderFailureKind string

const (
	ProviderFailureNotConfigured        ProviderFailureKind = "not_configured"
	ProviderFailureConfigurationInvalid ProviderFailureKind = "configuration_invalid"
	ProviderFailureCredentialsRejected  ProviderFailureKind = "credentials_rejected"
	ProviderFailureRequestRejected      ProviderFailureKind = "request_rejected"
	ProviderFailureRateLimited          ProviderFailureKind = "rate_limited"
	ProviderFailureUnavailable          ProviderFailureKind = "unavailable"
)

type ProviderFailure struct {
	Kind ProviderFailureKind
	err  error
}

func NewProviderFailure(kind ProviderFailureKind, err error) *ProviderFailure {
	if err == nil {
		err = errors.New("authoring provider failed")
	}
	return &ProviderFailure{Kind: kind, err: err}
}

func NewProviderHTTPFailure(statusCode int) *ProviderFailure {
	kind := ProviderFailureRequestRejected
	switch {
	case statusCode == 401 || statusCode == 403:
		kind = ProviderFailureCredentialsRejected
	case statusCode == 429:
		kind = ProviderFailureRateLimited
	case statusCode >= 500:
		kind = ProviderFailureUnavailable
	}
	return NewProviderFailure(kind, fmt.Errorf("authoring provider returned HTTP %d", statusCode))
}

func (e *ProviderFailure) Error() string {
	if e == nil || e.err == nil {
		return "authoring provider failed"
	}
	return e.err.Error()
}

func (e *ProviderFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}
