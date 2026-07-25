// Package oauth defines the portable OAuth 2 connection lifecycle used by
// Skills. Provider adapters perform protocol-specific requests while hosts own
// tenant policy, callback routing, encrypted credential storage, and audit.
package oauth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const APIVersion = "openseal.oauth-connection/v1"

var (
	ErrInvalid            = errors.New("OAuth connection request is invalid")
	ErrSessionNotFound    = errors.New("OAuth authorization session not found")
	ErrSessionConsumed    = errors.New("OAuth authorization session was already consumed")
	ErrSessionExpired     = errors.New("OAuth authorization session expired")
	ErrStateMismatch      = errors.New("OAuth callback state does not match")
	ErrScopeMismatch      = errors.New("OAuth grant does not cover the requested authorization")
	ErrRevisionConflict   = errors.New("OAuth connection revision conflict")
	ErrConnectionNotFound = errors.New("OAuth connection not found")
)

type ConnectionStatus string

const (
	ConnectionPending  ConnectionStatus = "pending"
	ConnectionActive   ConnectionStatus = "active"
	ConnectionDegraded ConnectionStatus = "degraded"
	ConnectionRevoked  ConnectionStatus = "revoked"
)

type SessionStatus string

const (
	SessionPending    SessionStatus = "pending"
	SessionExchanging SessionStatus = "exchanging"
	SessionCompleted  SessionStatus = "completed"
	SessionDenied     SessionStatus = "denied"
	SessionFailed     SessionStatus = "failed"
)

type OwnerKind string

const (
	OwnerTenant OwnerKind = "tenant"
	OwnerUser   OwnerKind = "user"
)

// Owner identifies who may bind a connection. It does not identify an Agent:
// Agent and Team bindings receive only the opaque CredentialReference.
type Owner struct {
	Kind OwnerKind `json:"kind"`
	ID   string    `json:"id"`
}

// ExternalIdentity contains operator-visible, non-secret installation facts.
// It must never be projected into model catalogs.
type ExternalIdentity struct {
	InstallationID string            `json:"installationId,omitempty"`
	AccountID      string            `json:"accountId,omitempty"`
	DisplayName    string            `json:"displayName,omitempty"`
	Attributes     map[string]string `json:"attributes,omitempty"`
}

// Connection is the secret-free durable view of an encrypted host grant.
type Connection struct {
	APIVersion          string                         `json:"apiVersion"`
	Scope               capability.ScopeReference      `json:"scope"`
	ID                  string                         `json:"id"`
	Kind                string                         `json:"kind"`
	Owner               Owner                          `json:"owner"`
	Grant               capability.OAuth2GrantSummary  `json:"grant"`
	ExternalIdentity    ExternalIdentity               `json:"externalIdentity,omitempty"`
	CredentialReference capability.CredentialReference `json:"credentialReference"`
	Status              ConnectionStatus               `json:"status"`
	TokenVersion        int64                          `json:"tokenVersion"`
	ExpiresAt           *time.Time                     `json:"expiresAt,omitempty"`
	LastRefreshedAt     *time.Time                     `json:"lastRefreshedAt,omitempty"`
	LastErrorCode       string                         `json:"lastErrorCode,omitempty"`
	Revision            int64                          `json:"revision"`
	CreatedAt           time.Time                      `json:"createdAt"`
	UpdatedAt           time.Time                      `json:"updatedAt"`
}

// AuthorizationSession is durable callback authority. StateDigest and
// PKCEVerifierReference are persistence fields but are deliberately omitted
// from JSON and operator/model projections.
type AuthorizationSession struct {
	APIVersion            string                         `json:"apiVersion"`
	Scope                 capability.ScopeReference      `json:"scope"`
	ID                    string                         `json:"id"`
	ConnectionID          string                         `json:"connectionId"`
	Kind                  string                         `json:"kind"`
	Owner                 Owner                          `json:"owner"`
	Requirement           capability.OAuth2Requirement   `json:"requirement"`
	RedirectURI           string                         `json:"redirectUri"`
	ResumeReference       string                         `json:"resumeReference,omitempty"`
	Status                SessionStatus                  `json:"status"`
	ExpiresAt             time.Time                      `json:"expiresAt"`
	ConsumedAt            *time.Time                     `json:"consumedAt,omitempty"`
	FailureCode           string                         `json:"failureCode,omitempty"`
	Revision              int64                          `json:"revision"`
	CreatedAt             time.Time                      `json:"createdAt"`
	UpdatedAt             time.Time                      `json:"updatedAt"`
	StateDigest           string                         `json:"-"`
	PKCEVerifierReference capability.CredentialReference `json:"-"`
}

type BeginAuthorizationRequest struct {
	Scope           capability.ScopeReference
	ConnectionID    string
	Kind            string
	Owner           Owner
	Requirement     capability.OAuth2Requirement
	RedirectURI     string
	ResumeReference string
}

type BeginAuthorizationResult struct {
	APIVersion       string    `json:"apiVersion"`
	SessionID        string    `json:"sessionId"`
	ConnectionID     string    `json:"connectionId"`
	AuthorizationURL string    `json:"authorizationUrl"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

type CompleteAuthorizationRequest struct {
	Scope        capability.ScopeReference
	SessionID    string
	State        string
	Code         string
	ErrorCode    string
	ErrorMessage string
}

type CompleteAuthorizationResult struct {
	APIVersion      string      `json:"apiVersion"`
	Connection      *Connection `json:"connection,omitempty"`
	ResumeReference string      `json:"resumeReference,omitempty"`
}

type ProviderAuthorizationRequest struct {
	SessionID     string
	State         string
	PKCEChallenge string
	RedirectURI   string
	Requirement   capability.OAuth2Requirement
}

type ProviderCodeExchangeRequest struct {
	Code         string
	PKCEVerifier string
	RedirectURI  string
	Requirement  capability.OAuth2Requirement
}

// TokenGrant is passed directly from a provider adapter to the host credential
// store.
// Token fields cannot be JSON serialized.
type TokenGrant struct {
	AccessToken      string           `json:"-"`
	RefreshToken     string           `json:"-"`
	TokenType        string           `json:"-"`
	GrantedScopes    []string         `json:"grantedScopes"`
	Resource         string           `json:"resource,omitempty"`
	ExpiresAt        *time.Time       `json:"expiresAt,omitempty"`
	ExternalIdentity ExternalIdentity `json:"externalIdentity,omitempty"`
}

type Provider interface {
	ID() string
	AuthorizationURL(context.Context, ProviderAuthorizationRequest) (string, error)
	ExchangeCode(context.Context, ProviderCodeExchangeRequest) (TokenGrant, error)
}

type CredentialStoreRequest struct {
	Scope        capability.ScopeReference
	ConnectionID string
	Kind         string
	Provider     string
	Grant        TokenGrant
}

// CredentialStore stores transient PKCE verifiers and connection tokens.
// Implementations must encrypt material and return opaque references only.
type CredentialStore interface {
	PutTransient(context.Context, capability.ScopeReference, string, time.Time) (capability.CredentialReference, error)
	TakeTransient(context.Context, capability.ScopeReference, capability.CredentialReference) (string, error)
	PutConnection(context.Context, CredentialStoreRequest) (capability.CredentialReference, int64, error)
}

type Store interface {
	CreateAuthorizationSession(context.Context, *AuthorizationSession) error
	GetAuthorizationSession(context.Context, capability.ScopeReference, string) (*AuthorizationSession, error)
	UpdateAuthorizationSession(context.Context, *AuthorizationSession, int64) error
	GetConnection(context.Context, capability.ScopeReference, string) (*Connection, error)
	UpsertConnection(context.Context, *Connection, int64) error
}

func normalizeGrant(requirement capability.OAuth2Requirement, grant TokenGrant) (capability.OAuth2GrantSummary, error) {
	summary, err := capability.NormalizeOAuth2GrantSummary(&capability.OAuth2GrantSummary{
		Provider: requirement.Provider, Subject: requirement.Subject,
		Scopes: append([]string(nil), grant.GrantedScopes...), Resource: grant.Resource,
	})
	if err != nil {
		return capability.OAuth2GrantSummary{}, fmt.Errorf("%w: provider grant is invalid: %v", ErrInvalid, err)
	}
	if !capability.OAuth2GrantSatisfies(&requirement, summary) {
		return capability.OAuth2GrantSummary{}, ErrScopeMismatch
	}
	return *summary, nil
}

func normalizeExternalIdentity(value ExternalIdentity) (ExternalIdentity, error) {
	result := ExternalIdentity{
		InstallationID: strings.TrimSpace(value.InstallationID),
		AccountID:      strings.TrimSpace(value.AccountID),
		DisplayName:    strings.TrimSpace(value.DisplayName),
	}
	if len(result.InstallationID) > 512 || len(result.AccountID) > 512 || len(result.DisplayName) > 512 || len(value.Attributes) > 64 {
		return ExternalIdentity{}, fmt.Errorf("%w: external identity is too large", ErrInvalid)
	}
	keys := make([]string, 0, len(value.Attributes))
	for key := range value.Attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		result.Attributes = make(map[string]string, len(keys))
	}
	for _, key := range keys {
		normalizedKey, normalizedValue := strings.TrimSpace(key), strings.TrimSpace(value.Attributes[key])
		if normalizedKey == "" || len(normalizedKey) > 128 || len(normalizedValue) > 512 {
			return ExternalIdentity{}, fmt.Errorf("%w: external identity attribute is invalid", ErrInvalid)
		}
		result.Attributes[normalizedKey] = normalizedValue
	}
	return result, nil
}
