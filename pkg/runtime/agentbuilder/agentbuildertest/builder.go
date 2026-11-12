package agentbuildertest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// Host is an agentbuilder.Host that records the models it validates and the
// changes it authorizes, and refuses the user/Agent pairs in Forbidden.
type Host struct {
	Validated []string
	// Forbidden lists "user/agentId" pairs AuthorizeAgentChange refuses.
	Forbidden  map[string]bool
	Authorized []string
}

func (h *Host) AuthorizeAgentChange(_ context.Context, _ skill.ScopeReference, actingUserID, deploymentID string) error {
	h.Authorized = append(h.Authorized, actingUserID+"/"+deploymentID)
	if h.Forbidden[actingUserID+"/"+deploymentID] {
		return agentbuilder.ErrAgentChangeForbidden
	}
	return nil
}

func (h *Host) ValidateAgentModel(_ context.Context, _ skill.ScopeReference, model string) error {
	h.Validated = append(h.Validated, model)
	if model == "unknown-model" {
		return errors.New("model unknown-model is not available")
	}
	return nil
}

func (h *Host) PrepareAgentModel(context.Context, agentbuilder.AgentModelRequest) error {
	return nil
}

func (h *Host) AfterAgentCreated(context.Context, agentbuilder.AgentCreated) {}

// Fixture is a builder Run of the Agent finn in tenant-a, with registered
// Agents and a Team, the Agent management and two test skills, and the Sales
// and Home spaces.
type Fixture struct {
	Scope   models.Scope
	Agents  *kernelagent.Registry
	Teams   *kernelteam.Registry
	Catalog *skill.Catalog
	Spaces  *portfolio.SpaceService
	SalesID string
	// HomeID is the default space of the person the fixture's run acts for.
	HomeID    string
	Run       *models.AgentRun
	Host      *Host
	Validator *agentbuilder.ActionValidator
}

// NewFixture builds the Fixture; withHost gives its validator a Host.
func NewFixture(t *testing.T, withHost bool) *Fixture {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	reference := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	agents := kernelagent.NewRegistry()
	addAgent := func(id, name string, amendments workforce.AmendmentPolicy, status kernelagent.RolloutStatus) {
		t.Helper()
		definition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
			ID: id + "-definition", Version: "1.0.0", DisplayName: name, Purpose: name + " work",
			SystemPrompt: "Work as " + name + ".", Personality: "Calm", OperatingPrinciples: []string{"Be brief."},
			Authority:  kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
			Amendments: amendments,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
			ID: id, Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "operator", "fixture"); err != nil {
			t.Fatal(err)
		}
		if status == kernelagent.RolloutRetired {
			deployment, err := agents.GetDeployment(ctx, reference, id)
			if err != nil {
				t.Fatal(err)
			}
			deployment.RolloutStatus = kernelagent.RolloutRetired
			if _, _, err := agents.UpdateDeployment(ctx, deployment, deployment.Revision, "user", "operator", "retire"); err != nil {
				t.Fatal(err)
			}
		}
	}
	addAgent("finn", "Finn", workforce.AmendmentPolicy{}, kernelagent.RolloutActive)
	addAgent("researcher", "Researcher", workforce.AmendmentPolicy{AgentMayPropose: true}, kernelagent.RolloutActive)
	addAgent("platform", "Platform Helper", workforce.AmendmentPolicy{}, kernelagent.RolloutActive)
	addAgent("old-agent", "Old Agent", workforce.AmendmentPolicy{AgentMayPropose: true}, kernelagent.RolloutRetired)

	teams := kernelteam.NewRegistry(agents)
	teamDefinition, err := teams.RegisterDefinition(ctx, &kernelteam.Definition{
		ID: "ops-team-definition", Version: "1", DisplayName: "Ops  Crew", Purpose: "Run operations",
		Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "ops-team", Scope: reference, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
		Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "researcher-0", AgentDeploymentID: "researcher"}},
	}, "user", "operator", "fixture"); err != nil {
		t.Fatal(err)
	}

	catalog := skill.NewCatalog()
	for _, definition := range []*skill.Definition{
		models.AgentManagementSkill(),
		{
			ID: "acme.notes", Version: "1.0.0", Name: "Notes", Description: "Keep notes",
			Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://notes"},
			Actions: map[string]skill.Action{
				"write_note": {
					Name: "write_note", Description: "Write a note", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
					Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
					InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
				},
			},
		},
		{
			ID: "acme.mail", Version: "2.0.0", Name: "Mail", Description: "Send mail",
			Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://mail"},
			Actions: map[string]skill.Action{
				"send": {
					Name: "send", Description: "Send a message", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal,
					Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
					Credentials: []skill.CredentialRequirement{{Name: "mailbox", Kind: "oauth"}},
					InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
				},
			},
		},
	} {
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	for _, binding := range []*skill.Binding{
		{
			ID: "bundled:agents", Revision: 1, Scope: skillScope, DeploymentID: "finn",
			SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
			AllowedActions: []string{models.AgentActionListAgents, models.AgentActionCreateAgent, models.AgentActionUpdateAgent, models.AgentActionTryAgent},
			MaximumRisk:    skill.RiskLevelWrite,
		},
		{
			ID: "builder:researcher:acme.notes", Revision: 1, Scope: skillScope, DeploymentID: "researcher",
			SkillID: "acme.notes", SkillVersion: "1.0.0", AllowedActions: []string{"write_note"}, MaximumRisk: skill.RiskLevelWrite,
		},
		{
			ID: "bundled:agents", Revision: 1, Scope: skillScope, DeploymentID: "researcher",
			SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
			AllowedActions: []string{models.AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
		},
	} {
		if err := catalog.Bind(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}

	spaces := portfolio.NewSpaceService(kerneltest.NewStore(t))
	salesID, homeID := SeedSpaces(t, spaces, scope)
	fixture := &Fixture{
		Scope: scope, Agents: agents, Teams: teams, Catalog: catalog, Spaces: spaces, SalesID: salesID, HomeID: homeID,
		Run: &models.AgentRun{
			ID: "builder-run", Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"},
			AssignedAgentID: "finn", ActingUser: Approver.ID, Source: models.RunSourceChat,
		},
	}
	var host agentbuilder.Host
	if withHost {
		fixture.Host = &Host{}
		host = fixture.Host
	}
	fixture.Validator, err = agentbuilder.NewActionValidator(agents, teams, catalog, spaces, nil, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// SeedSpaces creates Sales, owned by an operator, and Home, the
// default space of the person the builder run acts for.
func SeedSpaces(t *testing.T, spaces *portfolio.SpaceService, scope models.Scope) (salesID, homeID string) {
	t.Helper()
	ctx := context.Background()
	sales, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Sales", Look: models.DefaultSpaceLook, OwnerID: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Home", Look: models.DefaultSpaceLook, OwnerID: Approver.ID})
	if err != nil || !home.IsDefault {
		t.Fatalf("home = %+v, err = %v", home, err)
	}
	return sales.ID, home.ID
}

func (f *Fixture) Bound(t *testing.T, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.Catalog.Resolve(context.Background(), skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}, "finn",
		models.AgentManagementSkillID, models.AgentManagementSkillVersion, action)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// Propose runs the coordinator's order: resolve, schema check, validate.
func (f *Fixture) Propose(t *testing.T, action string, arguments map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	ctx := context.Background()
	bound := f.Bound(t, action)
	input := agentrun.ActionProposalValidationInput{Run: f.Run, Bound: bound, Arguments: arguments}
	resolved, handled, err := f.Validator.ResolveActionProposalArguments(ctx, input)
	if err != nil {
		return nil, err
	}
	if handled {
		input.Arguments = resolved
	}
	if err := f.Catalog.ValidateInput(ctx, bound, input.Arguments); err != nil {
		t.Fatalf("%s arguments failed the action schema: %v (%#v)", action, err, input.Arguments)
	}
	return f.Validator.ValidateActionProposal(ctx, input)
}

// CreateArguments are create_agent arguments for an inbox triage Agent.
func CreateArguments() map[string]interface{} {
	return map[string]interface{}{
		"displayName":         "  Inbox   Triage ",
		"purpose":             "Sort the support inbox every morning.",
		"systemPrompt":        "Read new support mail and sort it by urgency.",
		"personality":         "Friendly",
		"operatingPrinciples": []interface{}{"Flag anything legal.", " Flag anything legal. "},
		"model":               "gpt-6-luna",
		"skills": []interface{}{map[string]interface{}{
			"skillId": "acme.notes", "skillVersion": "1.0.0", "allowedActions": []interface{}{"write_note"},
			"enablePrompt": false, "maximumRisk": "write",
		}},
		"connections": []interface{}{map[string]interface{}{"skillId": "acme.mail", "skillVersion": "2.0.0"}},
		"rationale":   "The person asked for an inbox helper.",
	}
}

// AssertJSON fails the test unless got equals the JSON object want.
func AssertJSON(t *testing.T, got map[string]interface{}, want string) {
	t.Helper()
	var expected map[string]interface{}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("preview mismatch:\n%s", gotJSON)
	}
}
