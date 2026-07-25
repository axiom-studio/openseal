package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
)

const (
	authorizationTTL  = 10 * time.Minute
	refreshLeaseTTL   = time.Minute
	pkceVerifierBytes = 48
	stateBytes        = 32
)

type Service struct {
	store       Store
	credentials CredentialStore
	providers   map[string]Provider
	now         func() time.Time
	newID       func() string
	random      func(int) (string, error)
}

func NewService(store Store, credentials CredentialStore, providers ...Provider) (*Service, error) {
	if store == nil || credentials == nil {
		return nil, errors.New("OAuth store and encrypted credential store are required")
	}
	registered := make(map[string]Provider, len(providers))
	for _, provider := range providers {
		if provider == nil || !validIdentifier(provider.ID(), 128) {
			return nil, errors.New("OAuth providers require a lowercase stable identifier")
		}
		if _, exists := registered[provider.ID()]; exists {
			return nil, fmt.Errorf("OAuth provider %s is registered more than once", provider.ID())
		}
		registered[provider.ID()] = provider
	}
	return &Service{
		store: store, credentials: credentials, providers: registered,
		now: time.Now, newID: uuid.NewString, random: randomURLToken,
	}, nil
}

func (s *Service) RefreshConnection(ctx context.Context, request RefreshConnectionRequest) (*Connection, error) {
	if !validScope(request.Scope) || !validIdentifier(request.ConnectionID, 512) || request.ExpectedRevision < 1 {
		return nil, ErrInvalid
	}
	now, leaseID := s.now().UTC(), s.newID()
	claimed, err := s.store.ClaimConnectionRefresh(
		ctx, request.Scope, request.ConnectionID, request.ExpectedRevision,
		leaseID, now, now.Add(refreshLeaseTTL),
	)
	if err != nil {
		return nil, err
	}
	if claimed.Status != ConnectionActive && claimed.Status != ConnectionDegraded {
		s.releaseRefreshFailure(ctx, claimed, "connection_inactive")
		return nil, fmt.Errorf("%w: only active or degraded connections can refresh", ErrInvalid)
	}
	provider := s.providers[claimed.Grant.Provider]
	refreshProvider, ok := provider.(RefreshProvider)
	if !ok {
		s.releaseRefreshFailure(ctx, claimed, "refresh_unsupported")
		return nil, ErrUnsupported
	}
	current, err := s.credentials.GetConnection(ctx, claimed.Scope, claimed.CredentialReference)
	if err != nil {
		s.releaseRefreshFailure(ctx, claimed, "credential_unavailable")
		return nil, errors.New("OAuth connection credentials are unavailable")
	}
	requirement := capability.OAuth2Requirement{
		Provider: claimed.Grant.Provider, Subject: claimed.Grant.Subject,
		Scopes: append([]string(nil), claimed.Grant.Scopes...), Resource: claimed.Grant.Resource,
	}
	refreshed, err := refreshProvider.RefreshToken(ctx, ProviderRefreshRequest{Current: current, Requirement: requirement})
	if err != nil {
		s.releaseRefreshFailure(ctx, claimed, "refresh_failed")
		return nil, errors.New("OAuth provider token refresh failed")
	}
	if strings.TrimSpace(refreshed.AccessToken) == "" {
		s.releaseRefreshFailure(ctx, claimed, "missing_access_token")
		return nil, fmt.Errorf("%w: refreshed grant has no access token", ErrInvalid)
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = current.RefreshToken
	}
	if len(refreshed.GrantedScopes) == 0 {
		refreshed.GrantedScopes = append([]string(nil), claimed.Grant.Scopes...)
	}
	if refreshed.Resource == "" {
		refreshed.Resource = claimed.Grant.Resource
	}
	if externalIdentityEmpty(refreshed.ExternalIdentity) {
		refreshed.ExternalIdentity = claimed.ExternalIdentity
	}
	grantSummary, err := normalizeGrant(requirement, refreshed)
	if err != nil {
		s.releaseRefreshFailure(ctx, claimed, "scope_mismatch")
		return nil, err
	}
	externalIdentity, err := normalizeExternalIdentity(refreshed.ExternalIdentity)
	if err != nil {
		s.releaseRefreshFailure(ctx, claimed, "identity_invalid")
		return nil, err
	}
	reference, tokenVersion, err := s.credentials.PutConnection(ctx, CredentialStoreRequest{
		Scope: claimed.Scope, ConnectionID: claimed.ID, Kind: claimed.Kind,
		Provider: claimed.Grant.Provider, Grant: refreshed,
	})
	if err != nil {
		s.releaseRefreshFailure(ctx, claimed, "credential_store_failed")
		return nil, errors.New("store refreshed OAuth connection failed")
	}
	if reference.Kind != claimed.Kind || !validIdentifier(reference.ID, 1024) || tokenVersion <= claimed.TokenVersion {
		s.releaseRefreshFailure(ctx, claimed, "credential_reference_invalid")
		return nil, fmt.Errorf("%w: refreshed credential version is invalid", ErrInvalid)
	}
	updated := cloneConnection(claimed)
	updated.Grant, updated.ExternalIdentity = grantSummary, externalIdentity
	updated.CredentialReference, updated.TokenVersion = reference, tokenVersion
	updated.Status, updated.ExpiresAt, updated.LastRefreshedAt = ConnectionActive, refreshed.ExpiresAt, &now
	updated.LastErrorCode, updated.RefreshLeaseID, updated.RefreshLeaseExpiresAt = "", "", nil
	updated.Revision, updated.UpdatedAt = claimed.Revision+1, now
	if err := s.store.UpsertConnection(ctx, updated, claimed.Revision); err != nil {
		return nil, err
	}
	return cloneConnection(updated), nil
}

func (s *Service) RevokeConnection(ctx context.Context, request RevokeConnectionRequest) (*Connection, error) {
	if !validScope(request.Scope) || !validIdentifier(request.ConnectionID, 512) || request.ExpectedRevision < 1 {
		return nil, ErrInvalid
	}
	connection, err := s.store.GetConnection(ctx, request.Scope, request.ConnectionID)
	if err != nil {
		return nil, err
	}
	if connection.Revision != request.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	if connection.Status == ConnectionRevoked {
		return cloneConnection(connection), nil
	}
	current, err := s.credentials.GetConnection(ctx, connection.Scope, connection.CredentialReference)
	if err != nil {
		return nil, errors.New("OAuth connection credentials are unavailable")
	}
	if provider, ok := s.providers[connection.Grant.Provider].(RevocationProvider); ok {
		if err := provider.RevokeToken(ctx, ProviderRevocationRequest{Current: current}); err != nil {
			return nil, errors.New("OAuth provider token revocation failed")
		}
	}
	if err := s.credentials.DeleteConnection(ctx, connection.Scope, connection.CredentialReference); err != nil {
		return nil, errors.New("delete encrypted OAuth connection failed")
	}
	now := s.now().UTC()
	updated := cloneConnection(connection)
	updated.Status, updated.ExpiresAt, updated.RefreshLeaseID, updated.RefreshLeaseExpiresAt = ConnectionRevoked, nil, "", nil
	updated.LastErrorCode, updated.Revision, updated.UpdatedAt = "", connection.Revision+1, now
	if err := s.store.UpsertConnection(ctx, updated, connection.Revision); err != nil {
		return nil, err
	}
	return cloneConnection(updated), nil
}

func (s *Service) releaseRefreshFailure(ctx context.Context, claimed *Connection, code string) {
	if claimed == nil {
		return
	}
	failed := cloneConnection(claimed)
	failed.Status, failed.LastErrorCode = ConnectionDegraded, code
	failed.RefreshLeaseID, failed.RefreshLeaseExpiresAt = "", nil
	failed.Revision, failed.UpdatedAt = claimed.Revision+1, s.now().UTC()
	_ = s.store.UpsertConnection(ctx, failed, claimed.Revision)
}

func (s *Service) BeginAuthorization(ctx context.Context, request BeginAuthorizationRequest) (*BeginAuthorizationResult, error) {
	requirement, err := capability.NormalizeOAuth2Requirement(&request.Requirement)
	if err != nil || requirement == nil || !validScope(request.Scope) ||
		!validIdentifier(request.ConnectionID, 512) || !validIdentifier(request.Kind, 128) ||
		!validOwner(request.Owner) || !validRedirectURI(request.RedirectURI) ||
		len(request.ResumeReference) > 1024 {
		return nil, fmt.Errorf("%w: scope, connection, owner, authorization, and redirect are required", ErrInvalid)
	}
	provider := s.providers[requirement.Provider]
	if provider == nil {
		return nil, fmt.Errorf("%w: OAuth provider %s is not registered", ErrInvalid, requirement.Provider)
	}
	sessionID := s.newID()
	if !validIdentifier(sessionID, 512) {
		return nil, fmt.Errorf("%w: generated OAuth session identity is invalid", ErrInvalid)
	}
	stateNonce, err := s.random(stateBytes)
	if err != nil {
		return nil, err
	}
	state := sessionID + "." + stateNonce
	verifier, err := s.random(pkceVerifierBytes)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	expiresAt := now.Add(authorizationTTL)
	verifierReference, err := s.credentials.PutTransient(ctx, request.Scope, verifier, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("store OAuth PKCE verifier: %w", err)
	}
	session := &AuthorizationSession{
		APIVersion: APIVersion, Scope: request.Scope, ID: sessionID,
		ConnectionID: strings.TrimSpace(request.ConnectionID), Kind: strings.TrimSpace(request.Kind),
		Owner: request.Owner, Requirement: *requirement, RedirectURI: strings.TrimSpace(request.RedirectURI),
		ResumeReference: strings.TrimSpace(request.ResumeReference), Status: SessionPending,
		ExpiresAt: expiresAt, Revision: 1, CreatedAt: now, UpdatedAt: now,
		StateDigest: digest(state), PKCEVerifierReference: verifierReference,
	}
	if err := s.store.CreateAuthorizationSession(ctx, session); err != nil {
		return nil, err
	}
	authorizationURL, err := provider.AuthorizationURL(ctx, ProviderAuthorizationRequest{
		SessionID: session.ID, State: state, PKCEChallenge: pkceChallenge(verifier),
		RedirectURI: session.RedirectURI, Requirement: session.Requirement,
	})
	if err != nil || !validAuthorizationURL(authorizationURL) {
		s.failSession(ctx, session, "authorization_url")
		if err != nil {
			return nil, fmt.Errorf("build OAuth authorization URL: %w", err)
		}
		return nil, fmt.Errorf("%w: provider returned an invalid authorization URL", ErrInvalid)
	}
	return &BeginAuthorizationResult{
		APIVersion: APIVersion, SessionID: session.ID, ConnectionID: session.ConnectionID,
		AuthorizationURL: authorizationURL, ExpiresAt: expiresAt,
	}, nil
}

func (s *Service) CompleteAuthorization(ctx context.Context, request CompleteAuthorizationRequest) (*CompleteAuthorizationResult, error) {
	if !validScope(request.Scope) || !validIdentifier(request.SessionID, 512) {
		return nil, ErrInvalid
	}
	session, err := s.store.GetAuthorizationSession(ctx, request.Scope, request.SessionID)
	if err != nil {
		return nil, err
	}
	if session.Status != SessionPending {
		return nil, ErrSessionConsumed
	}
	now := s.now().UTC()
	if !now.Before(session.ExpiresAt) {
		s.failSession(ctx, session, "expired")
		return nil, ErrSessionExpired
	}
	if subtleDigestMismatch(session.StateDigest, request.State) {
		return nil, ErrStateMismatch
	}
	if strings.TrimSpace(request.ErrorCode) != "" {
		session.Status, session.FailureCode, session.ConsumedAt = SessionDenied, safeFailureCode(request.ErrorCode), &now
		session.UpdatedAt, session.Revision = now, session.Revision+1
		if err := s.store.UpdateAuthorizationSession(ctx, session, session.Revision-1); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("OAuth consent was denied: %s", session.FailureCode)
	}
	if strings.TrimSpace(request.Code) == "" {
		return nil, fmt.Errorf("%w: OAuth authorization code is required", ErrInvalid)
	}

	expectedRevision := session.Revision
	session.Status, session.ConsumedAt, session.UpdatedAt = SessionExchanging, &now, now
	session.Revision++
	if err := s.store.UpdateAuthorizationSession(ctx, session, expectedRevision); err != nil {
		return nil, err
	}
	verifier, err := s.credentials.TakeTransient(ctx, session.Scope, session.PKCEVerifierReference)
	if err != nil {
		s.failSession(ctx, session, "pkce_unavailable")
		return nil, fmt.Errorf("resolve OAuth PKCE verifier: %w", err)
	}
	provider := s.providers[session.Requirement.Provider]
	if provider == nil {
		s.failSession(ctx, session, "provider_unavailable")
		return nil, fmt.Errorf("%w: OAuth provider is no longer registered", ErrInvalid)
	}
	grant, err := provider.ExchangeCode(ctx, ProviderCodeExchangeRequest{
		Code: strings.TrimSpace(request.Code), PKCEVerifier: verifier,
		RedirectURI: session.RedirectURI, Requirement: session.Requirement,
	})
	if err != nil {
		s.failSession(ctx, session, "exchange_failed")
		return nil, fmt.Errorf("exchange OAuth authorization code: %w", err)
	}
	if strings.TrimSpace(grant.AccessToken) == "" {
		s.failSession(ctx, session, "missing_access_token")
		return nil, fmt.Errorf("%w: provider returned no access token", ErrInvalid)
	}
	grantSummary, err := normalizeGrant(session.Requirement, grant)
	if err != nil {
		s.failSession(ctx, session, "scope_mismatch")
		return nil, err
	}
	externalIdentity, err := normalizeExternalIdentity(grant.ExternalIdentity)
	if err != nil {
		s.failSession(ctx, session, "identity_invalid")
		return nil, err
	}
	reference, tokenVersion, err := s.credentials.PutConnection(ctx, CredentialStoreRequest{
		Scope: session.Scope, ConnectionID: session.ConnectionID, Kind: session.Kind,
		Provider: session.Requirement.Provider, Grant: grant,
	})
	if err != nil {
		s.failSession(ctx, session, "credential_store_failed")
		return nil, fmt.Errorf("store encrypted OAuth connection: %w", err)
	}
	if reference.Kind != session.Kind || !validIdentifier(reference.ID, 1024) || tokenVersion < 1 {
		s.failSession(ctx, session, "credential_reference_invalid")
		return nil, fmt.Errorf("%w: credential store returned an invalid reference", ErrInvalid)
	}
	current, getErr := s.store.GetConnection(ctx, session.Scope, session.ConnectionID)
	expectedConnectionRevision := int64(0)
	createdAt := now
	if getErr == nil {
		expectedConnectionRevision, createdAt = current.Revision, current.CreatedAt
	} else if !errors.Is(getErr, ErrConnectionNotFound) {
		return nil, getErr
	}
	connection := &Connection{
		APIVersion: APIVersion, Scope: session.Scope, ID: session.ConnectionID, Kind: session.Kind,
		Owner: session.Owner, Grant: grantSummary, ExternalIdentity: externalIdentity,
		CredentialReference: reference, Status: ConnectionActive, TokenVersion: tokenVersion,
		ExpiresAt: grant.ExpiresAt, Revision: expectedConnectionRevision + 1,
		CreatedAt: createdAt, UpdatedAt: now,
	}
	if err := s.store.UpsertConnection(ctx, connection, expectedConnectionRevision); err != nil {
		return nil, err
	}
	session.Status, session.FailureCode, session.UpdatedAt = SessionCompleted, "", now
	expectedRevision = session.Revision
	session.Revision++
	if err := s.store.UpdateAuthorizationSession(ctx, session, expectedRevision); err != nil {
		return nil, err
	}
	return &CompleteAuthorizationResult{
		APIVersion: APIVersion, Connection: cloneConnection(connection),
		ResumeReference: session.ResumeReference,
	}, nil
}

func (s *Service) failSession(ctx context.Context, session *AuthorizationSession, code string) {
	if session == nil || session.Status == SessionCompleted || session.Status == SessionDenied || session.Status == SessionFailed {
		return
	}
	expected := session.Revision
	now := s.now().UTC()
	session.Status, session.FailureCode, session.UpdatedAt = SessionFailed, code, now
	session.Revision++
	_ = s.store.UpdateAuthorizationSession(ctx, session, expected)
}

func randomURLToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate OAuth random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func subtleDigestMismatch(expectedDigest, value string) bool {
	actual := digest(strings.TrimSpace(value))
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expectedDigest)) != 1
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SessionIDFromState extracts the non-secret durable routing identity from an
// OAuth callback state value. The complete state remains high entropy, hashed
// at rest, tenant-bound, expiring, and single-use.
func SessionIDFromState(state string) (string, error) {
	state = strings.TrimSpace(state)
	separator := strings.IndexByte(state, '.')
	if separator <= 0 || separator == len(state)-1 {
		return "", ErrStateMismatch
	}
	sessionID, nonce := state[:separator], state[separator+1:]
	if !validIdentifier(sessionID, 512) || len(nonce) < 32 {
		return "", ErrStateMismatch
	}
	return sessionID, nil
}

func validOwner(owner Owner) bool {
	return (owner.Kind == OwnerTenant || owner.Kind == OwnerUser) && validIdentifier(owner.ID, 512)
}

func validScope(scope capability.ScopeReference) bool {
	return validIdentifier(scope.Kind, 64) && validIdentifier(scope.ID, 512)
}

func validIdentifier(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximum {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			index > 0 && strings.ContainsRune("._:/-", character) {
			continue
		}
		return false
	}
	return true
}

func validRedirectURI(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.Fragment == "" &&
		(parsed.Scheme == "https" || parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1"))
}

func validAuthorizationURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.Scheme == "https"
}

func safeFailureCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if validIdentifier(value, 128) {
		return value
	}
	return "provider_error"
}

func cloneConnection(value *Connection) *Connection {
	if value == nil {
		return nil
	}
	result := *value
	if value.ExpiresAt != nil {
		expiresAt := *value.ExpiresAt
		result.ExpiresAt = &expiresAt
	}
	if value.LastRefreshedAt != nil {
		lastRefreshedAt := *value.LastRefreshedAt
		result.LastRefreshedAt = &lastRefreshedAt
	}
	if value.RefreshLeaseExpiresAt != nil {
		refreshLeaseExpiresAt := *value.RefreshLeaseExpiresAt
		result.RefreshLeaseExpiresAt = &refreshLeaseExpiresAt
	}
	result.Grant.Scopes = append([]string(nil), value.Grant.Scopes...)
	if value.ExternalIdentity.Attributes != nil {
		result.ExternalIdentity.Attributes = make(map[string]string, len(value.ExternalIdentity.Attributes))
		for key, item := range value.ExternalIdentity.Attributes {
			result.ExternalIdentity.Attributes[key] = item
		}
	}
	return &result
}

func externalIdentityEmpty(value ExternalIdentity) bool {
	return value.InstallationID == "" && value.AccountID == "" && value.DisplayName == "" && len(value.Attributes) == 0
}
