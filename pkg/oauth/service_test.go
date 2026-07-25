package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type fixtureProvider struct {
	id           string
	exchanges    int
	refreshes    int
	revokes      int
	grant        TokenGrant
	refreshGrant TokenGrant
}

func (p *fixtureProvider) ID() string { return p.id }

func (p *fixtureProvider) AuthorizationURL(_ context.Context, request ProviderAuthorizationRequest) (string, error) {
	query := url.Values{
		"state": {request.State}, "code_challenge": {request.PKCEChallenge},
		"redirect_uri": {request.RedirectURI}, "scope": {strings.Join(request.Requirement.Scopes, " ")},
	}
	return "https://provider.example/authorize?" + query.Encode(), nil
}

func (p *fixtureProvider) ExchangeCode(_ context.Context, request ProviderCodeExchangeRequest) (TokenGrant, error) {
	p.exchanges++
	if request.Code != "valid-code" || request.PKCEVerifier == "" {
		return TokenGrant{}, errors.New("invalid exchange")
	}
	return p.grant, nil
}

func (p *fixtureProvider) RefreshToken(_ context.Context, request ProviderRefreshRequest) (TokenGrant, error) {
	p.refreshes++
	if request.Current.RefreshToken == "" {
		return TokenGrant{}, errors.New("refresh token missing")
	}
	return p.refreshGrant, nil
}

func (p *fixtureProvider) RevokeToken(_ context.Context, request ProviderRevocationRequest) error {
	p.revokes++
	if request.Current.AccessToken == "" {
		return errors.New("access token missing")
	}
	return nil
}

type memoryCredentialStore struct {
	mu         sync.Mutex
	transient  map[string]string
	connection TokenGrant
	version    int64
	deleted    bool
}

func newMemoryCredentialStore() *memoryCredentialStore {
	return &memoryCredentialStore{transient: make(map[string]string)}
}

func (v *memoryCredentialStore) PutTransient(_ context.Context, _ capability.ScopeReference, value string, _ time.Time) (capability.CredentialReference, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	reference := capability.CredentialReference{Kind: "oauth-transient", ID: "secret-store://transient/one"}
	v.transient[reference.ID] = value
	return reference, nil
}

func (v *memoryCredentialStore) TakeTransient(_ context.Context, _ capability.ScopeReference, reference capability.CredentialReference) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value := v.transient[reference.ID]
	if value == "" {
		return "", errors.New("transient secret not found")
	}
	delete(v.transient, reference.ID)
	return value, nil
}

func (v *memoryCredentialStore) PutConnection(_ context.Context, request CredentialStoreRequest) (capability.CredentialReference, int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.connection = request.Grant
	v.version++
	v.deleted = false
	return capability.CredentialReference{Kind: request.Kind, ID: "connection://tenant/slack/one"}, v.version, nil
}

func (v *memoryCredentialStore) GetConnection(_ context.Context, _ capability.ScopeReference, _ capability.CredentialReference) (TokenGrant, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.deleted || v.connection.AccessToken == "" {
		return TokenGrant{}, errors.New("connection not found")
	}
	return v.connection, nil
}

func (v *memoryCredentialStore) DeleteConnection(_ context.Context, _ capability.ScopeReference, _ capability.CredentialReference) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.connection = TokenGrant{}
	v.deleted = true
	return nil
}

func TestAuthorizationLifecycleUsesPKCEAndReturnsOpaqueConnection(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	refreshExpiry := now.Add(2 * time.Hour)
	provider := &fixtureProvider{id: "slack", grant: TokenGrant{
		AccessToken: "secret-access-token", RefreshToken: "secret-refresh-token",
		GrantedScopes:    []string{"chat:write", "channels:history"},
		ExternalIdentity: ExternalIdentity{InstallationID: "T123", DisplayName: "Acme"},
	}, refreshGrant: TokenGrant{
		AccessToken: "rotated-access-token", ExpiresAt: &refreshExpiry,
	}}
	store, credentials := NewMemoryStore(), newMemoryCredentialStore()
	service, err := NewService(store, credentials, provider)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	service.newID = func() string { return "session-one" }
	randomValues := []string{"callback-state-secret-with-at-least-thirty-two-bytes", "pkce-verifier-secret"}
	service.random = func(_ int) (string, error) {
		value := randomValues[0]
		randomValues = randomValues[1:]
		return value, nil
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "tenant-1"}
	begin, err := service.BeginAuthorization(context.Background(), BeginAuthorizationRequest{
		Scope: scope, ConnectionID: "slack-primary", Kind: "slack-oauth",
		Owner: Owner{Kind: OwnerTenant, ID: "tenant-1"},
		Requirement: capability.OAuth2Requirement{
			Provider: "slack", Subject: capability.OAuth2SubjectInstallation,
			Scopes: []string{"chat:write", "channels:history"},
		},
		RedirectURI:     "https://studio.example/oauth/callback",
		ResumeReference: "changeset://tenant-1/candidate-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(begin.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	callbackState := "session-one.callback-state-secret-with-at-least-thirty-two-bytes"
	if parsed.Query().Get("state") != callbackState ||
		parsed.Query().Get("code_challenge") != pkceChallenge("pkce-verifier-secret") {
		t.Fatalf("authorization URL does not contain bound state and S256 challenge: %s", begin.AuthorizationURL)
	}
	session, err := store.GetAuthorizationSession(context.Background(), scope, begin.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	encodedSession, _ := json.Marshal(session)
	if strings.Contains(string(encodedSession), "callback-state-secret") ||
		strings.Contains(string(encodedSession), "pkce-verifier-secret") ||
		strings.Contains(string(encodedSession), "secret-store://transient") {
		t.Fatalf("session JSON exposed callback authority: %s", encodedSession)
	}
	result, err := service.CompleteAuthorization(context.Background(), CompleteAuthorizationRequest{
		Scope: scope, SessionID: begin.SessionID, State: callbackState, Code: "valid-code",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ResumeReference != "changeset://tenant-1/candidate-7" ||
		result.Connection.Status != ConnectionActive ||
		result.Connection.CredentialReference.ID != "connection://tenant/slack/one" ||
		result.Connection.Grant.Provider != "slack" ||
		strings.Join(result.Connection.Grant.Scopes, ",") != "channels:history,chat:write" {
		t.Fatalf("completed OAuth result = %#v", result)
	}
	if sessionID, err := SessionIDFromState(callbackState); err != nil || sessionID != begin.SessionID {
		t.Fatalf("callback state session ID = %q, %v", sessionID, err)
	}
	encodedResult, _ := json.Marshal(result)
	for _, secret := range []string{"secret-access-token", "secret-refresh-token", "pkce-verifier-secret", "callback-state-secret"} {
		if strings.Contains(string(encodedResult), secret) {
			t.Fatalf("completed result exposed %s: %s", secret, encodedResult)
		}
	}
	if credentials.connection.AccessToken != "secret-access-token" || credentials.connection.RefreshToken != "secret-refresh-token" {
		t.Fatal("encrypted credential store did not receive provider token material")
	}

	now = now.Add(time.Hour)
	refreshed, err := service.RefreshConnection(context.Background(), RefreshConnectionRequest{
		Scope: scope, ConnectionID: result.Connection.ID, ExpectedRevision: result.Connection.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != ConnectionActive || refreshed.Revision != 3 || refreshed.TokenVersion != 2 ||
		refreshed.LastRefreshedAt == nil || refreshed.ExpiresAt == nil ||
		provider.refreshes != 1 || credentials.connection.AccessToken != "rotated-access-token" ||
		credentials.connection.RefreshToken != "secret-refresh-token" {
		t.Fatalf("refreshed connection = %#v; refreshes=%d; credentials=%#v", refreshed, provider.refreshes, credentials.connection)
	}
	if _, err := service.RefreshConnection(context.Background(), RefreshConnectionRequest{
		Scope: scope, ConnectionID: result.Connection.ID, ExpectedRevision: result.Connection.Revision,
	}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("concurrent stale refresh error = %v", err)
	}
	revoked, err := service.RevokeConnection(context.Background(), RevokeConnectionRequest{
		Scope: scope, ConnectionID: refreshed.ID, ExpectedRevision: refreshed.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Status != ConnectionRevoked || revoked.Revision != 4 || provider.revokes != 1 || !credentials.deleted {
		t.Fatalf("revoked connection = %#v; provider revokes=%d; deleted=%t", revoked, provider.revokes, credentials.deleted)
	}
}

func TestAuthorizationCallbackIsTenantBoundSingleUseAndScopeSafe(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	provider := &fixtureProvider{id: "provider", grant: TokenGrant{
		AccessToken: "token", GrantedScopes: []string{"read"},
	}}
	store, credentials := NewMemoryStore(), newMemoryCredentialStore()
	service, _ := NewService(store, credentials, provider)
	service.now = func() time.Time { return now }
	service.newID = func() string { return "session-one" }
	values := []string{"state-value-with-at-least-thirty-two-bytes", "verifier-value"}
	service.random = func(_ int) (string, error) {
		value := values[0]
		values = values[1:]
		return value, nil
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "tenant-1"}
	begin, err := service.BeginAuthorization(context.Background(), BeginAuthorizationRequest{
		Scope: scope, ConnectionID: "connection-one", Kind: "provider-oauth",
		Owner: Owner{Kind: OwnerTenant, ID: "tenant-1"},
		Requirement: capability.OAuth2Requirement{
			Provider: "provider", Subject: capability.OAuth2SubjectInstallation,
			Scopes: []string{"read", "write"},
		},
		RedirectURI: "https://studio.example/oauth/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	callbackState := "session-one.state-value-with-at-least-thirty-two-bytes"
	if _, err := service.CompleteAuthorization(context.Background(), CompleteAuthorizationRequest{
		Scope:     capability.ScopeReference{Kind: "tenant", ID: "tenant-2"},
		SessionID: begin.SessionID, State: callbackState, Code: "valid-code",
	}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-tenant callback error = %v", err)
	}
	if _, err := service.CompleteAuthorization(context.Background(), CompleteAuthorizationRequest{
		Scope: scope, SessionID: begin.SessionID, State: "wrong-state", Code: "valid-code",
	}); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("wrong-state callback error = %v", err)
	}
	if _, err := service.CompleteAuthorization(context.Background(), CompleteAuthorizationRequest{
		Scope: scope, SessionID: begin.SessionID, State: callbackState, Code: "valid-code",
	}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("under-scoped callback error = %v", err)
	}
	if provider.exchanges != 1 {
		t.Fatalf("provider exchanges = %d", provider.exchanges)
	}
	if _, err := service.CompleteAuthorization(context.Background(), CompleteAuthorizationRequest{
		Scope: scope, SessionID: begin.SessionID, State: callbackState, Code: "valid-code",
	}); !errors.Is(err, ErrSessionConsumed) {
		t.Fatalf("callback replay error = %v", err)
	}
}

func TestTokenGrantJSONNeverSerializesTokenMaterial(t *testing.T) {
	encoded, err := json.Marshal(TokenGrant{
		AccessToken: "access-secret", RefreshToken: "refresh-secret", TokenType: "Bearer",
		GrantedScopes: []string{"read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "Bearer"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("token grant JSON exposed %s: %s", secret, encoded)
		}
	}
}
