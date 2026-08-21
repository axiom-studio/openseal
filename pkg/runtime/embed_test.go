package runtime

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func testEmbedPublicKey() string { return base64.RawURLEncoding.EncodeToString(make([]byte, 32)) }

func testEmbedPermissions() EmbedPermissionPolicy {
	return EmbedPermissionPolicy{
		AllowTextChat: true, MaximumMessages: 20, SessionTTLSeconds: 3600, MaximumInputBytes: 8192,
		MaximumActionRisk: skill.RiskLevelRead,
		ActionGrants:      []EmbedActionGrant{{BindingID: "crm", Action: "lookup"}},
	}
}

func TestEmbedSessionCreatesDistinctCanonicalAgentConversations(t *testing.T) {
	store := NewMemoryStore()
	service, err := NewEmbedSessionService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "1"}
	installation, err := service.CreateInstallation(t.Context(), CreateEmbedInstallationRequest{
		Scope: scope, DeploymentID: "support-agent", Name: "Support chat", AllowedOrigins: []string{"https://example.com"},
		Identity: EmbedIdentityPolicy{Mode: EmbedIdentityAnonymous}, Permissions: testEmbedPermissions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.OpenSession(t.Context(), OpenEmbedSessionRequest{PublicRoute: installation.PublicRoute, Origin: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.OpenSession(t.Context(), OpenEmbedSessionRequest{PublicRoute: installation.PublicRoute, Origin: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Session.ID == second.Session.ID || first.Session.ConversationID == second.Session.ConversationID || first.Capability == second.Capability {
		t.Fatalf("openings shared identity: first=%#v second=%#v", first.Session, second.Session)
	}
	conversation, err := NewConversationService(store).GetConversation(t.Context(), scope, first.Session.ConversationID)
	if err != nil || conversation.Owner != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "support-agent"}) || conversation.Origin == nil || conversation.Origin.Kind != ConversationReferenceEmbedSession {
		t.Fatalf("canonical conversation = %#v, %v", conversation, err)
	}
	if _, _, err := service.Authenticate(t.Context(), installation.PublicRoute, first.Session.ID, first.Capability, "https://evil.example"); !errors.Is(err, ErrEmbedOriginDenied) {
		t.Fatalf("foreign origin error = %v", err)
	}
	if _, _, err := service.Authenticate(t.Context(), installation.PublicRoute, first.Session.ID, "wrong", "https://example.com"); !errors.Is(err, ErrEmbedSessionUnauthorized) {
		t.Fatalf("wrong capability error = %v", err)
	}
}

func TestSignedEmbedSessionProjectsOnlyVerifiedClaimsIntoActionContext(t *testing.T) {
	store := NewMemoryStore()
	service, _ := NewEmbedSessionService(store, store)
	installation, err := service.CreateInstallation(t.Context(), CreateEmbedInstallationRequest{
		Scope: Scope{Kind: "tenant", ID: "1"}, DeploymentID: "support-agent", Name: "Signed support", AllowedOrigins: []string{"https://app.example.com"},
		Identity:    EmbedIdentityPolicy{Mode: EmbedIdentitySigned, Issuer: "https://identity.example.com", Audience: "support", KeyID: "support-key", PublicKey: testEmbedPublicKey(), RequiredClaims: []string{"userId"}},
		Permissions: testEmbedPermissions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.OpenSession(t.Context(), OpenEmbedSessionRequest{PublicRoute: installation.PublicRoute, Origin: "https://app.example.com"}); !errors.Is(err, ErrEmbedAuthentication) {
		t.Fatalf("unsigned open error = %v", err)
	}
	opened, err := service.OpenSession(t.Context(), OpenEmbedSessionRequest{
		PublicRoute: installation.PublicRoute, Origin: "https://app.example.com", VerifiedClaims: map[string]interface{}{"userId": "customer-42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	context, err := service.ResolveConversationSessionContext(t.Context(), installation.Scope, opened.Session.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	claims := context[sessionContextVerifiedClaimsKey].(map[string]interface{})
	if context[sessionContextIDKey] != opened.Session.ID || claims["userId"] != "customer-42" {
		t.Fatalf("session context = %#v", context)
	}
	authority := context["authority"].(map[string]interface{})
	if !embedAuthorityAllows(authority, "crm", "lookup", skill.RiskLevelRead) || embedAuthorityAllows(authority, "crm", "delete", skill.RiskLevelRead) {
		t.Fatalf("session authority = %#v", authority)
	}
}

func TestSQLiteEmbedSessionCapabilityAndClaimsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embed.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewEmbedSessionService(store, store)
	installation, err := service.CreateInstallation(t.Context(), CreateEmbedInstallationRequest{
		Scope: Scope{Kind: "tenant", ID: "7"}, DeploymentID: "support-agent", Name: "Support", AllowedOrigins: []string{"https://app.example.com"},
		Identity: EmbedIdentityPolicy{Mode: EmbedIdentitySigned, Issuer: "issuer", Audience: "audience", KeyID: "key", PublicKey: testEmbedPublicKey(), RequiredClaims: []string{"userId"}}, Permissions: testEmbedPermissions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := service.OpenSession(t.Context(), OpenEmbedSessionRequest{PublicRoute: installation.PublicRoute, Origin: "https://app.example.com", VerifiedClaims: map[string]interface{}{"userId": "user-7"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service, _ = NewEmbedSessionService(reopened, reopened)
	_, session, err := service.Authenticate(t.Context(), installation.PublicRoute, opened.Session.ID, opened.Capability, "https://app.example.com")
	if err != nil || session.VerifiedClaims["userId"] != "user-7" {
		t.Fatalf("restarted session = %#v, %v", session, err)
	}
}
