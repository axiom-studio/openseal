package oauth

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestReauthorizingVerifiedAccountReusesConnectionAndRefreshToken(t *testing.T) {
	ctx := context.Background()
	store, credentials := NewMemoryStore(), newMemoryCredentialStore()
	provider := &fixtureProvider{id: "mail", grant: TokenGrant{AccessToken: "first", RefreshToken: "refresh", GrantedScopes: []string{"read"}, ExternalIdentity: ExternalIdentity{AccountID: "verified-subject", DisplayName: "first@example.test"}}}
	service, err := NewService(store, credentials, provider)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "42"}
	request := BeginAuthorizationRequest{Scope: scope, ConnectionID: "random-attempt-one", Kind: "mail_token", Owner: Owner{Kind: OwnerTenant, ID: "42"}, Requirement: capability.OAuth2Requirement{Provider: "mail", Subject: capability.OAuth2SubjectUser, Scopes: []string{"read"}}, RedirectURI: "https://studio.example/oauth/callback"}
	complete := func(request BeginAuthorizationRequest) *Connection {
		t.Helper()
		begin, err := service.BeginAuthorization(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(begin.AuthorizationURL)
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.CompleteAuthorization(ctx, CompleteAuthorizationRequest{Scope: scope, SessionID: begin.SessionID, State: u.Query().Get("state"), Code: "valid-code"})
		if err != nil {
			t.Fatal(err)
		}
		return result.Connection
	}
	first := complete(request)
	provider.grant.AccessToken, provider.grant.RefreshToken = "second", ""
	provider.grant.ExternalIdentity.DisplayName = "renamed@example.test"
	request.ConnectionID = "random-attempt-two"
	second := complete(request)
	if second.ID != first.ID || second.CredentialReference != first.CredentialReference || second.Revision != first.Revision+1 {
		t.Fatalf("reauthorization replaced account identity: first=%#v second=%#v", first, second)
	}
	if credentials.connection.RefreshToken != "refresh" {
		t.Fatal("reauthorization lost the saved refresh token")
	}
	connections, err := store.ListConnections(ctx, scope)
	if err != nil || len(connections) != 1 {
		t.Fatalf("expected one saved account, got %#v, %v", connections, err)
	}
	// A different verified subject cannot overwrite credentials through an
	// explicit reconnect, even when the displayed email is identical.
	provider.grant.ExternalIdentity.AccountID = "another-subject"
	request.ConnectionID = first.ID
	begin, err := service.BeginAuthorization(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(begin.AuthorizationURL)
	version := credentials.version
	if _, err := service.CompleteAuthorization(ctx, CompleteAuthorizationRequest{Scope: scope, SessionID: begin.SessionID, State: u.Query().Get("state"), Code: "valid-code"}); err == nil {
		t.Fatal("different account overwrote reconnect")
	}
	if credentials.version != version {
		t.Fatal("account identity validation occurred after credential mutation")
	}
}

func TestVerifiedAccountDeduplicationPreservesAuthorizationDomains(t *testing.T) {
	base := &Connection{Scope: capability.ScopeReference{Kind: "tenant", ID: "42"}, Owner: Owner{Kind: OwnerTenant, ID: "42"}, Kind: "token", Grant: capability.OAuth2GrantSummary{Provider: "mail", Subject: capability.OAuth2SubjectUser}, ExternalIdentity: ExternalIdentity{AccountID: "subject", DisplayName: "a@example.test"}}
	for name, mutate := range map[string]func(*Connection){
		"tenant":       func(c *Connection) { c.Scope.ID = "43" },
		"owner":        func(c *Connection) { c.Owner.ID = "43" },
		"provider":     func(c *Connection) { c.Grant.Provider = "other" },
		"kind":         func(c *Connection) { c.Kind = "other" },
		"resource":     func(c *Connection) { c.Grant.Resource = "other" },
		"subject":      func(c *Connection) { c.ExternalIdentity.AccountID = "other" },
		"unverified":   func(c *Connection) { c.ExternalIdentity.AccountID = "" },
		"installation": func(c *Connection) { c.Grant.Subject = capability.OAuth2SubjectInstallation },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneConnection(base)
			mutate(candidate)
			if SameAccountConnection(base, candidate) {
				t.Fatal("different authorization domain merged")
			}
		})
	}
	candidate := cloneConnection(base)
	candidate.Grant.Scopes = []string{"different"}
	candidate.ExternalIdentity.DisplayName = "b@example.test"
	if !SameAccountConnection(base, candidate) {
		t.Fatal("mutable scopes and email must not replace account identity")
	}
}

func TestAccountUpsertSupersedesLegacyDuplicatesAtomically(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	base := &Connection{ID: "legacy", Scope: capability.ScopeReference{Kind: "tenant", ID: "42"}, Owner: Owner{Kind: OwnerTenant, ID: "42"}, Kind: "token", Grant: capability.OAuth2GrantSummary{Provider: "mail", Subject: capability.OAuth2SubjectUser}, ExternalIdentity: ExternalIdentity{AccountID: "subject"}, Status: ConnectionActive, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	store.connections[oauthKey(base.Scope, base.ID)] = cloneConnection(base)
	duplicate := cloneConnection(base)
	duplicate.ID = "duplicate"
	store.connections[oauthKey(base.Scope, duplicate.ID)] = duplicate
	other := cloneConnection(base)
	other.ID = "other"
	other.ExternalIdentity.AccountID = "other"
	store.connections[oauthKey(base.Scope, other.ID)] = other
	updated := cloneConnection(base)
	updated.Revision++
	if err := store.UpsertConnection(ctx, updated, 99); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal("expected revision conflict")
	}
	if store.connections[oauthKey(base.Scope, duplicate.ID)].Status != ConnectionActive {
		t.Fatal("failed upsert superseded another account")
	}
	if err := store.UpsertConnection(ctx, updated, 1); err != nil {
		t.Fatal(err)
	}
	if store.connections[oauthKey(base.Scope, duplicate.ID)].Status != ConnectionRevoked || store.connections[oauthKey(base.Scope, other.ID)].Status != ConnectionActive {
		t.Fatal("upsert did not preserve exactly one active record for the verified identity")
	}
}
