package git

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillDefinitionRegistersHostOnlyFetchAndPush(t *testing.T) {
	d := SkillDefinition()
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if d.ID != "openseal.git" || d.Transport.Kind != "tool" || d.Transport.Endpoint != SkillID || len(d.Actions) != 2 {
		t.Fatalf("definition = %#v", d)
	}
	fetch, push := d.Actions[Fetch], d.Actions[Push]
	if !fetch.HostOnly || fetch.SideEffect != skill.SideEffectRead || fetch.Risk != skill.RiskLevelRead {
		t.Fatalf("fetch = %#v", fetch)
	}
	if !push.HostOnly || push.SideEffect != skill.SideEffectWrite || push.Risk != skill.RiskLevelWrite || push.Idempotency != skill.IdempotencyRequired {
		t.Fatalf("push = %#v", push)
	}
	for _, action := range []skill.Action{fetch, push} {
		if len(action.Credentials) != 1 || action.Credentials[0].Name != CredentialAccess || action.Credentials[0].Kind != mcp.CredentialKind || action.Credentials[0].Optional {
			t.Fatalf("%s credentials = %#v", action.Name, action.Credentials)
		}
	}
}

func TestHostOnlyActionsAreNeverModelActions(t *testing.T) {
	ctx := t.Context()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "git", Revision: 1, Scope: scope, DeploymentID: "agent", SkillID: SkillID, SkillVersion: SkillVersion,
		AllowedActions: []string{Fetch, Push}, MaximumRisk: skill.RiskLevelWrite,
		Credentials: map[string]skill.CredentialReference{CredentialAccess: {Kind: mcp.CredentialKind, ID: "credential://github"}},
	}); err != nil {
		t.Fatal(err)
	}
	actions, err := catalog.ListModelActions(ctx, scope, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("model actions = %#v", actions)
	}
}
