package skillbinding_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// copied from connect_app_test.go
// connectAppFixture is an Agent whose run sees a Linear connection only once
// connected is set, as the host supplies connected apps per run.
func connectAppFixture(t *testing.T) (context.Context, *sqlite.Store, *skill.Catalog, *skillbinding.SkillBindingActionDispatcher, models.Scope, *atomic.Bool) {
	t.Helper()
	ctx, store, catalog, dispatcher, scope := credentialRequestDispatchFixture(t)
	if err := catalog.Register(ctx, mcp.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{ID: "app-actions", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, AllowedActions: []string{skillbinding.SkillActionFindApps, agentrun.SkillActionRequestConnection}, MaximumRisk: skill.RiskLevelRead}); err != nil {
		t.Fatal(err)
	}
	connected := &atomic.Bool{}
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}}
	catalog.SetHostBindingProvider(func(_ context.Context, skillScope skill.ScopeReference, deploymentID string, _ skill.RunActor) (*skill.HostBindingSet, error) {
		if !connected.Load() || skillScope.ID != scope.ID {
			return nil, nil
		}
		return &skill.HostBindingSet{Fingerprint: "linear@2", Services: map[string][]string{"mcp:c1": {"linear", "Linear"}}, Bindings: []*skill.Binding{{
			ID: "mcp:c1", Revision: 2, Scope: skillScope, DeploymentID: deploymentID, SkillID: mcp.SkillID, SkillVersion: mcp.SkillVersion,
			AllowedActions: []string{mcp.Read}, MaximumRisk: skill.RiskLevelRead, Slug: "linear",
			Config: map[string]interface{}{"endpoint": "https://mcp.linear.app/mcp", "transport": "streamable-http", "auth": map[string]interface{}{"kind": "none"}},
			Tools: []skill.BindingTool{{Name: "list_issues", Description: "List issues", Effect: skill.BindingToolEffectRead, InputSchema: schema,
				Arguments: map[string]interface{}{"tool": "list_issues", "hash": strings.Repeat("0", 64)}}},
		}}}, nil
	})
	apps := &skillbinding.AppCatalogRef{}
	apps.Set(fakeAppCatalog{entries: []skillbinding.AppCatalogEntry{
		{Slug: "linear", Name: "Linear", Summary: "Issue tracking", Official: true, SignIn: models.AppSignInOAuth, LogoSHA256: linearLogo},
		{Slug: "acme/linear-tools", Name: "Linear Tools", SignIn: "weird"},
		{Slug: "neondatabase/mcp-server-neon", Name: "Neon", SignIn: models.AppSignInOAuth},
	}})
	dispatcher.UseAppCatalog(apps)
	return ctx, store, catalog, dispatcher, scope, connected
}

// copied from skill_binding_action_test.go
func createClaimedSkillActionRun(t *testing.T, ctx context.Context, store *sqlite.Store, scope models.Scope, deploymentID, workerID string) *models.AgentRun {
	t.Helper()
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, AssignedAgentID: deploymentID,
		Goal: "Manage Skills", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: workerID, Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return claimed
}

// copied from credential_request_test.go
func credentialRequestDispatchFixture(t *testing.T) (context.Context, *sqlite.Store, *skill.Catalog, *skillbinding.SkillBindingActionDispatcher, models.Scope) {
	t.Helper()
	ctx := context.Background()
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "a"}
	catalog := skillActionCatalog(t, ctx, scope, "agent")
	if err := catalog.Bind(ctx, &skill.Binding{ID: "interaction-actions", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, AllowedActions: []string{agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestCredential}, MaximumRisk: skill.RiskLevelRead}); err != nil {
		t.Fatal(err)
	}
	// The bundled live browser is never in the installable catalog.
	provider := skill.DiscoveryProviderFunc(func(context.Context, skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		return &skill.DiscoveryPage{}, nil
	})
	dispatcher, err := skillbinding.NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, catalog, dispatcher, scope
}

// copied from kernel_store_test.go
// newTestKernelStore returns an empty in-memory kernel store that is closed
// when the test ends. runtimetest.NewKernelStore and store/sqlite's test
// opener are its copies for other packages (importing runtimetest here
// would be a cycle); change all three.
func newTestKernelStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// copied from skill_binding_action_test.go
func skillActionCatalog(t *testing.T, ctx context.Context, scope models.Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, skillbinding.SkillManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, redditSkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "skills", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion,
		AllowedActions: []string{skillbinding.SkillActionDiscoverBinding, skillbinding.SkillActionUpsertBinding, skillbinding.SkillActionDisableBinding}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

// copied from host_binding_action_test.go
// hostConnectorCatalog registers a connector Skill whose bindings the host
// supplies per run, as connected apps are: only runs in allowed get the
// linear binding.
func hostConnectorCatalog(t *testing.T, scope models.Scope, allowed map[string]bool) *skill.Catalog {
	t.Helper()
	input := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"tool": map[string]interface{}{"type": "string"}, "hash": map[string]interface{}{"type": "string"},
			"arguments": map[string]interface{}{"type": "object"},
		},
		"required": []interface{}{"tool", "hash", "arguments"},
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), &skill.Definition{
		ID: "openseal.connector", Version: "1.0.0", Name: "Connector", Transport: skill.TransportReference{Kind: "local", Endpoint: "internal://connector"},
		BindingTools: &skill.BindingToolsSpec{ModelPrefix: "mcp", ReadAction: "read", WriteAction: "call", InputArgument: "arguments", FixedArguments: []string{"tool", "hash"}},
		Actions: map[string]skill.Action{
			"read": {Name: "read", Description: "Call a read-only tool", InputSchema: input, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported},
			"call": {Name: "call", Description: "Call a tool", InputSchema: input, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden},
		},
	}); err != nil {
		t.Fatal(err)
	}
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"title": map[string]interface{}{"type": "string"}}}
	catalog.SetHostBindingProvider(func(_ context.Context, skillScope skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		if skillScope.ID != scope.ID || !allowed[actor.RunID] {
			return nil, nil
		}
		return &skill.HostBindingSet{Fingerprint: "linear@1", Services: map[string][]string{"mcp:linear": {"linear", "Linear"}}, Bindings: []*skill.Binding{{
			ID: "mcp:linear", Revision: 1, Scope: skillScope, DeploymentID: deploymentID, SkillID: "openseal.connector", SkillVersion: "1.0.0",
			AllowedActions: []string{"read", "call"}, MaximumRisk: skill.RiskLevelWrite, Slug: "linear",
			Tools: []skill.BindingTool{
				{Name: "create_issue", Description: "Create a Linear issue", Effect: skill.BindingToolEffectWrite, InputSchema: schema, Arguments: map[string]interface{}{"tool": "create_issue", "hash": "linear-create_issue"}},
				{Name: "search", Description: "Search Linear issues", Effect: skill.BindingToolEffectRead, InputSchema: schema, Arguments: map[string]interface{}{"tool": "search", "hash": "linear-search"}},
			},
		}}}, nil
	})
	return catalog
}

// copied from host_binding_action_test.go
func mustDefinition(t *testing.T, catalog *skill.Catalog, id string) *skill.Definition {
	t.Helper()
	definition, err := catalog.GetDefinition(t.Context(), id, "1.0.0")
	if err != nil || definition == nil {
		t.Fatalf("definition %s: %v", id, err)
	}
	return definition
}

// copied from skill_binding_target_test.go
func newSkillTargetFixture(t *testing.T) *skillTargetFixture {
	t.Helper()
	f := &skillTargetFixture{ctx: context.Background(), store: newTestKernelStore(t), scope: models.Scope{Kind: "tenant", ID: "tenant-a"}}
	f.skillScope = skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	reference := capability.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	f.agents = kernelagent.NewRegistry()
	for _, agent := range []struct {
		id, name   string
		amendments workforce.AmendmentPolicy
		retired    bool
	}{
		{"finn", "Finn", workforce.AmendmentPolicy{}, false},
		{"writer", "Writer", workforce.AmendmentPolicy{AgentMayPropose: true}, false},
		{"researcher", "Researcher", workforce.AmendmentPolicy{AgentMayPropose: true}, false},
		{"platform", "Platform Helper", workforce.AmendmentPolicy{}, false},
		{"old", "Old Agent", workforce.AmendmentPolicy{AgentMayPropose: true}, true},
	} {
		definition, err := f.agents.RegisterDefinition(f.ctx, &kernelagent.AgentDefinition{
			ID: agent.id + "-definition", Version: "1.0.0", DisplayName: agent.name, Purpose: agent.name + " work",
			SystemPrompt: "Work as " + agent.name + ".", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
			Amendments: agent.amendments,
		})
		if err != nil {
			t.Fatal(err)
		}
		deployment, _, err := f.agents.CreateDeployment(f.ctx, &kernelagent.AgentDeployment{
			ID: agent.id, Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "operator", "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if agent.retired {
			deployment.RolloutStatus = kernelagent.RolloutRetired
			if _, _, err := f.agents.UpdateDeployment(f.ctx, deployment, deployment.Revision, "user", "operator", "retire"); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.catalog = skill.NewCatalog()
	for _, definition := range []*skill.Definition{skillbinding.SkillManagementSkill(), models.AgentManagementSkill(), redditSkillDefinition(), mcp.SkillDefinition()} {
		if err := f.catalog.Register(f.ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range []string{"finn", "writer"} {
		if err := f.catalog.Bind(f.ctx, &skill.Binding{
			ID: "skills", Revision: 1, Scope: f.skillScope, DeploymentID: member, SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion,
			AllowedActions: []string{skillbinding.SkillActionDiscoverBinding, skillbinding.SkillActionUpsertBinding, skillbinding.SkillActionDisableBinding, agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestConnection, skillbinding.SkillActionListSetupRequests},
			MaximumRisk:    skill.RiskLevelWrite,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.catalog.Bind(f.ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: f.skillScope, DeploymentID: "finn", SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
		AllowedActions: []string{models.AgentActionCreateAgent}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	// Writer has openseal.agents too, but no builder action.
	if err := f.catalog.Bind(f.ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: f.skillScope, DeploymentID: "writer", SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
		AllowedActions: []string{models.AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	// Discovery reports readiness per deployment: the reader is bindable for
	// researcher only.
	provider := skill.DiscoveryProviderFunc(func(_ context.Context, request skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		f.discovered = append(f.discovered, request)
		readiness := skill.DiscoveryReadinessUnavailable
		if request.DeploymentID == "researcher" {
			readiness = skill.DiscoveryReadinessBindable
		}
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{
			ID: "reddit.reader", Version: "1.0.0", Name: "Reddit Reader", Readiness: readiness,
			Actions: []skill.DiscoveryAction{{Name: "read", Risk: skill.RiskLevelRead}},
		}}}, nil
	})
	var err error
	if f.validator, err = skillbinding.NewSkillBindingActionValidator(f.catalog, f.store); err != nil {
		t.Fatal(err)
	}
	f.validator.UseAgentRegistry(f.agents)
	if f.dispatcher, err = skillbinding.NewSkillBindingActionDispatcher(f.store, f.catalog, nil, provider); err != nil {
		t.Fatal(err)
	}
	f.dispatcher.UseAgentRegistry(f.agents)
	return f
}

// copied from skill_binding_target_test.go
func readerUpsertArguments(target string) map[string]interface{} {
	arguments := map[string]interface{}{
		"bindingId": "reddit", "expectedRevision": 1, "skillId": "reddit.reader", "skillVersion": "1.0.0",
		"allowedActions": []interface{}{"read"}, "enablePrompt": false, "maximumRisk": "read",
	}
	if target != "" {
		arguments[skillbinding.SkillActionTargetAgentArgument] = target
	}
	return arguments
}

// copied from skill_setup_request_test.go
func setupRequestFixture() *models.SkillSetupRequest {
	now := time.Now().UTC()
	return &models.SkillSetupRequest{ID: "request-1", Scope: models.Scope{Kind: "tenant", ID: "a"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: "run", ActionCallID: "call", Kind: "configure", SkillID: "reddit.reader", SkillVersion: "1.0.0", SkillName: "Reddit", Reason: "Read the requested community", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
}

// copied from skill_binding_action_test.go
func redditSkillDefinition() *skill.Definition {
	return &skill.Definition{
		ID: "reddit.reader", Version: "1.0.0", Name: "Reddit Reader", Description: "Read configured communities.",
		Transport: skill.TransportReference{Kind: "http", Endpoint: "https://reddit.example"},
		BindingConfigSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{"subreddits": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}},
		},
		Prompt: &capability.PromptModule{
			Instructions: "Use Reddit evidence.", UserInvocable: true,
			Credentials: []skill.CredentialRequirement{{Name: "reddit", Kind: "reddit-oauth"}},
		},
		Actions: map[string]skill.Action{
			"read": {
				Name: "read", Description: "Read Reddit posts.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
				Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
				Credentials: []skill.CredentialRequirement{{Name: "reddit", Kind: "reddit-oauth"}},
				InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
			},
		},
	}
}

// copied from kernel_store_test.go
// newTestSQLiteStore opens a SQLite store on a fresh file and the test's own
// connection to that file, both closed when the test ends. Use it instead of
// newTestKernelStore when the test reads or writes rows no store method
// offers.
func newTestSQLiteStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, openTestSQLiteDB(t, path)
}

// copied from connect_app_test.go
type fakeAppCatalog struct {
	entries []skillbinding.AppCatalogEntry
}

// copied from connect_app_test.go
func (c fakeAppCatalog) FindApps(_ context.Context, _ models.Scope, query string, limit int) ([]skillbinding.AppCatalogEntry, error) {
	var result []skillbinding.AppCatalogEntry
	for _, entry := range c.entries {
		if strings.Contains(strings.ToLower(entry.Name), strings.ToLower(query)) {
			result = append(result, entry)
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// copied from connect_app_test.go
func (c fakeAppCatalog) GetApp(_ context.Context, _ models.Scope, slug string) (*skillbinding.AppCatalogEntry, error) {
	for _, entry := range c.entries {
		if entry.Slug == slug {
			copy := entry
			return &copy, nil
		}
	}
	return nil, nil
}

// copied from connect_app_test.go
var linearLogo = strings.Repeat("ab", 32)

// copied from skill_binding_target_test.go
// skillTargetFixture is a builder (finn, with an openseal.agents binding that
// allows create_agent), a plain Agent (writer) and the Agents they might act
// for: researcher (changeable), platform (fixed policy) and old (retired).
type skillTargetFixture struct {
	ctx        context.Context
	store      *sqlite.Store
	scope      models.Scope
	skillScope skill.ScopeReference
	agents     *kernelagent.Registry
	catalog    *skill.Catalog
	validator  *skillbinding.SkillBindingActionValidator
	dispatcher *skillbinding.SkillBindingActionDispatcher
	discovered []skill.DiscoveryRequest
}

// copied from skill_binding_target_test.go
// chatRun is a reply Run of member in its own chat.
func (f *skillTargetFixture) chatRun(t *testing.T, member string) *models.AgentRun {
	t.Helper()
	run, err := portfolio.NewPortfolioService(f.store).CreateAgentRun(f.ctx, models.CreateAgentRunRequest{
		Scope: f.scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: member}, AssignedAgentID: member,
		Goal: "Build an Agent", Source: models.RunSourceChat, ActingUser: "7",
		Context: map[string]interface{}{"conversationId": "chat-" + member, "triggerMessageId": "message-" + member},
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// copied from skill_binding_target_test.go
func (f *skillTargetFixture) bound(t *testing.T, member, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.catalog.Resolve(f.ctx, f.skillScope, member, models.SkillManagementSkillID, skillbinding.SkillManagementSkillVersion, action)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// copied from skill_binding_target_test.go
func (f *skillTargetFixture) dispatch(t *testing.T, run *models.AgentRun, callID, action string, arguments map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	return f.dispatcher.DispatchAction(f.ctx, agentrun.ActionDispatchInput{
		Call:  &models.ActionCall{ID: callID, Scope: f.scope, RunID: run.ID, DeploymentID: run.AssignedAgentID},
		Bound: f.bound(t, run.AssignedAgentID, action), Arguments: arguments,
	})
}

// copied from kernel_store_test.go
// openTestSQLiteDB opens the test's own connection to the SQLite file at
// path, closed when the test ends.
func openTestSQLiteDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
