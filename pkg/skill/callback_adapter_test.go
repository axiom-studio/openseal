package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestCallbackAdapterRequiresExactEnabledBinding(t *testing.T) {
	ctx := context.Background()
	catalog := NewCatalog()
	definition := callbackAdapterDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &Binding{
		ID: "slack-callback", Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent-one",
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledCallbackAdapters: []string{"interactions"}, MaximumRisk: RiskLevelRead, Revision: 1,
		Credentials: map[string]CredentialReference{
			"signing_secret": {Kind: "slack_signing_secret", ID: "credential-store-callback"},
		},
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.ResolveCallbackAdapter(
		ctx, binding.Scope, binding.DeploymentID, binding.SkillID, binding.SkillVersion, "interactions",
		BindingReference{ID: binding.ID, Revision: binding.Revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Adapter.Provider != "slack" || resolved.Binding.Credentials["signing_secret"].ID != "credential-store-callback" {
		t.Fatalf("resolved callback adapter = %#v", resolved)
	}
	if _, err := catalog.ResolveCallbackAdapter(
		ctx, binding.Scope, binding.DeploymentID, binding.SkillID, binding.SkillVersion, "interactions",
		BindingReference{ID: binding.ID, Revision: 2},
	); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale binding error = %v", err)
	}
}

func TestCallbackAdapterRejectsMissingVerifierCredential(t *testing.T) {
	ctx := context.Background()
	catalog := NewCatalog()
	definition := callbackAdapterDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	err := catalog.Bind(ctx, &Binding{
		ID: "slack-callback", Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent-one",
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledCallbackAdapters: []string{"interactions"}, MaximumRisk: RiskLevelRead, Revision: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "signing_secret") {
		t.Fatalf("missing credential error = %v", err)
	}
}

func TestCallbackAdapterRequiresCanonicalEventOrdering(t *testing.T) {
	definition := callbackAdapterDefinition()
	adapter := definition.CallbackAdapters["interactions"]
	adapter.EventTypes = []string{"source.changed", "approval.decided"}
	definition.CallbackAdapters["interactions"] = adapter
	err := NewCatalog().Register(context.Background(), definition)
	if err == nil || !strings.Contains(err.Error(), "canonical ordering") {
		t.Fatalf("non-canonical adapter error = %v", err)
	}
}

func TestCallbackAdapterAcceptsIsolatedWebSocketConnectionCredentials(t *testing.T) {
	definition := callbackAdapterDefinition()
	adapter := definition.CallbackAdapters["interactions"]
	adapter.Credentials = []capability.CredentialRequirement{
		{Name: "app_token", Kind: "slack_app_token"},
		{Name: "signing_secret", Kind: "slack_signing_secret"},
	}
	adapter.Transport.Connection = &capability.CallbackAdapterConnectionTransport{
		Kind: "websocket", Endpoint: "slack.callback.socket_mode",
		Credentials: []string{"app_token", "signing_secret"}, SharedByCredential: "app_token",
	}
	definition.CallbackAdapters["interactions"] = adapter
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	binding := &Binding{
		ID: "slack-callback", Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent-one",
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledCallbackAdapters: []string{"interactions"}, MaximumRisk: RiskLevelRead, Revision: 1,
		Credentials: map[string]CredentialReference{
			"app_token":      {Kind: "slack_app_token", ID: "vault://slack.app_token"},
			"signing_secret": {Kind: "slack_signing_secret", ID: "vault://slack.signing_secret"},
		},
	}
	if err := catalog.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.ResolveCallbackAdapterBinding(context.Background(), binding.Scope, binding.DeploymentID, binding.ID, "interactions")
	if err != nil {
		t.Fatal(err)
	}
	connection := resolved.Adapter.Transport.Connection
	if connection == nil || connection.Kind != "websocket" || connection.Endpoint != "slack.callback.socket_mode" ||
		strings.Join(connection.Credentials, ",") != "app_token,signing_secret" || connection.SharedByCredential != "app_token" {
		t.Fatalf("connection transport = %#v", connection)
	}
}

func TestCallbackAdapterRejectsUnprojectedSharedConnectionCredential(t *testing.T) {
	definition := callbackAdapterDefinition()
	adapter := definition.CallbackAdapters["interactions"]
	adapter.Credentials = []capability.CredentialRequirement{
		{Name: "app_token", Kind: "slack_app_token"},
		{Name: "signing_secret", Kind: "slack_signing_secret"},
	}
	adapter.Transport.Connection = &capability.CallbackAdapterConnectionTransport{
		Kind: "websocket", Endpoint: "slack.callback.socket_mode",
		Credentials: []string{"signing_secret"}, SharedByCredential: "app_token",
	}
	definition.CallbackAdapters["interactions"] = adapter
	err := NewCatalog().Register(context.Background(), definition)
	if err == nil || !strings.Contains(err.Error(), "shared connection credential") {
		t.Fatalf("shared connection credential error = %v", err)
	}
}

func TestCallbackAdapterRejectsUnusedConnectionCredential(t *testing.T) {
	definition := callbackAdapterDefinition()
	adapter := definition.CallbackAdapters["interactions"]
	adapter.Credentials = []capability.CredentialRequirement{
		{Name: "app_token", Kind: "slack_app_token"},
		{Name: "signing_secret", Kind: "slack_signing_secret"},
	}
	definition.CallbackAdapters["interactions"] = adapter
	err := NewCatalog().Register(context.Background(), definition)
	if err == nil || !strings.Contains(err.Error(), "ingress or connection use") {
		t.Fatalf("unused connection credential error = %v", err)
	}
}

func callbackAdapterDefinition() *Definition {
	return &Definition{
		ID: "skill-slack", Version: "2.2.0", Name: "Slack",
		Actions: map[string]Action{},
		CallbackAdapters: map[string]CallbackAdapter{
			"interactions": {
				ProtocolVersion: CallbackAdapterProtocolV1, Name: "Slack interactions",
				Description: "Verify signed Slack interactions and emit portable events.", Provider: "slack",
				EventTypes:  []string{"approval.decided", "source.changed"},
				Credentials: []capability.CredentialRequirement{{Name: "signing_secret", Kind: "slack_signing_secret"}},
				Transport: CallbackAdapterTransport{
					Kind: "http", IngressEndpoint: "/v1/callbacks/slack",
					IngressCredentials: []string{"signing_secret"},
				},
			},
		},
	}
}
