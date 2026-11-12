// The in-package agent builder tests' own copies of the Team roster
// portfolio, the Slack conversation catalog, the switchable adapter resolver
// and the Postgres test store opener that runtime's tests use.

package agentbuilder

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type rosterPortfolio struct {
	*sqlite.Store
	team *kernelteam.Deployment
}

func (p rosterPortfolio) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if p.team != nil && p.team.ID == id && p.team.Scope == scope {
		return p.team, nil
	}
	return nil, nil
}

type switchableAdapterResolver struct {
	externalconversation.ExternalConversationAdapterResolver
	unavailable bool
}

func (r *switchableAdapterResolver) ResolveConversationAdapter(
	ctx context.Context,
	scope skill.ScopeReference,
	deploymentID, skillID, skillVersion, adapterID string,
	binding ...skill.BindingReference,
) (*skill.BoundConversationAdapter, error) {
	if r.unavailable {
		return nil, errors.New("adapter unavailable")
	}
	return r.ExternalConversationAdapterResolver.ResolveConversationAdapter(ctx, scope, deploymentID, skillID, skillVersion, adapterID, binding...)
}

func (r *switchableAdapterResolver) ResolveConversationAdapterBinding(
	ctx context.Context,
	scope skill.ScopeReference,
	deploymentID, bindingID, adapterID string,
) (*skill.BoundConversationAdapter, error) {
	if r.unavailable {
		return nil, errors.New("adapter unavailable")
	}
	return r.ExternalConversationAdapterResolver.ResolveConversationAdapterBinding(ctx, scope, deploymentID, bindingID, adapterID)
}

func slackConversationCatalog(t *testing.T, ctx context.Context) (*sqlite.Store, *skill.Catalog, models.Scope, models.ExternalConversationAdapterReference) {
	t.Helper()
	store := kerneltest.NewStore(t)
	catalog := skill.NewCatalog()
	definition := skillbindingtest.SlackConversationSkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "tenant", ID: "one"}
	binding := &skill.Binding{
		ID: "slack", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "slack-agent",
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/one/slack"},
		},
		Revision: 1,
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	return store, catalog, scope, models.ExternalConversationAdapterReference{
		SkillID: definition.ID, SkillVersion: definition.Version,
		BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
	}
}

// openPostgresTestStoreOn opens another store on schema s, closed when the
// test ends.
func openPostgresTestStoreOn(t testing.TB, s storetest.PostgresSchema, options ...postgres.Option) *postgres.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := postgres.New(ctx, s.DSN, append([]postgres.Option{postgres.WithSchema(s.Name)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// openPostgresTestStore opens a Postgres store on a fresh schema (see
// storetest.NewPostgresSchema) and returns it with the schema.
func openPostgresTestStore(t testing.TB, prefix string, options ...postgres.Option) (*postgres.Store, storetest.PostgresSchema) {
	t.Helper()
	schema := storetest.NewPostgresSchema(t, prefix)
	return openPostgresTestStoreOn(t, schema, options...), schema
}
