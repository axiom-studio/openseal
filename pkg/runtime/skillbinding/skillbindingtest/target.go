package skillbindingtest

import (
	"context"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// TargetFixture is a builder (finn, with an openseal.agents binding that
// allows create_agent), a plain Agent (writer) and the Agents they might act
// for: researcher (changeable), platform (fixed policy) and old (retired).
type TargetFixture struct {
	Ctx        context.Context
	Store      *sqlite.Store
	Scope      models.Scope
	SkillScope skill.ScopeReference
	Agents     *kernelagent.Registry
	Catalog    *skill.Catalog
	Validator  *skillbinding.ActionValidator
	Dispatcher *skillbinding.ActionDispatcher
	Discovered []skill.DiscoveryRequest
}

// NewTargetFixture builds the Agents, catalog, validator and dispatcher of a
// TargetFixture on a fresh store.
func NewTargetFixture(t *testing.T) *TargetFixture {
	t.Helper()
	f := &TargetFixture{Ctx: context.Background(), Store: kerneltest.NewStore(t), Scope: models.Scope{Kind: "tenant", ID: "tenant-a"}}
	f.SkillScope = skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}
	reference := capability.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}
	f.Agents = kernelagent.NewRegistry()
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
		definition, err := f.Agents.RegisterDefinition(f.Ctx, &kernelagent.AgentDefinition{
			ID: agent.id + "-definition", Version: "1.0.0", DisplayName: agent.name, Purpose: agent.name + " work",
			SystemPrompt: "Work as " + agent.name + ".", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
			Amendments: agent.amendments,
		})
		if err != nil {
			t.Fatal(err)
		}
		deployment, _, err := f.Agents.CreateDeployment(f.Ctx, &kernelagent.AgentDeployment{
			ID: agent.id, Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "operator", "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if agent.retired {
			deployment.RolloutStatus = kernelagent.RolloutRetired
			if _, _, err := f.Agents.UpdateDeployment(f.Ctx, deployment, deployment.Revision, "user", "operator", "retire"); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.Catalog = skill.NewCatalog()
	for _, definition := range []*skill.Definition{skillbinding.SkillManagementSkill(), models.AgentManagementSkill(), RedditSkillDefinition(), mcp.SkillDefinition()} {
		if err := f.Catalog.Register(f.Ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range []string{"finn", "writer"} {
		if err := f.Catalog.Bind(f.Ctx, &skill.Binding{
			ID: "skills", Revision: 1, Scope: f.SkillScope, DeploymentID: member, SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion,
			AllowedActions: []string{skillbinding.SkillActionDiscoverBinding, skillbinding.SkillActionUpsertBinding, skillbinding.SkillActionDisableBinding, agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestConnection, skillbinding.SkillActionListSetupRequests},
			MaximumRisk:    skill.RiskLevelWrite,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Catalog.Bind(f.Ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: f.SkillScope, DeploymentID: "finn", SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
		AllowedActions: []string{models.AgentActionCreateAgent}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	// Writer has openseal.agents too, but no builder action.
	if err := f.Catalog.Bind(f.Ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: f.SkillScope, DeploymentID: "writer", SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
		AllowedActions: []string{models.AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	// Discovery reports readiness per deployment: the reader is bindable for
	// researcher only.
	provider := skill.DiscoveryProviderFunc(func(_ context.Context, request skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		f.Discovered = append(f.Discovered, request)
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
	if f.Validator, err = skillbinding.NewActionValidator(f.Catalog, f.Store); err != nil {
		t.Fatal(err)
	}
	f.Validator.UseAgentRegistry(f.Agents)
	if f.Dispatcher, err = skillbinding.NewActionDispatcher(f.Store, f.Catalog, nil, provider); err != nil {
		t.Fatal(err)
	}
	f.Dispatcher.UseAgentRegistry(f.Agents)
	return f
}

// ChatRun is a reply Run of member in its own chat.
func (f *TargetFixture) ChatRun(t *testing.T, member string) *models.AgentRun {
	t.Helper()
	run, err := portfolio.NewService(f.Store).CreateAgentRun(f.Ctx, models.CreateAgentRunRequest{
		Scope: f.Scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: member}, AssignedAgentID: member,
		Goal: "Build an Agent", Source: models.RunSourceChat, ActingUser: "7",
		Context: map[string]interface{}{"conversationId": "chat-" + member, "triggerMessageId": "message-" + member},
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// Bound resolves member's management Skill binding for action.
func (f *TargetFixture) Bound(t *testing.T, member, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.Catalog.Resolve(f.Ctx, f.SkillScope, member, models.SkillManagementSkillID, skillbinding.SkillManagementSkillVersion, action)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// Dispatch runs action for run through the fixture's dispatcher.
func (f *TargetFixture) Dispatch(t *testing.T, run *models.AgentRun, callID, action string, arguments map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	return f.Dispatcher.DispatchAction(f.Ctx, agentrun.ActionDispatchInput{
		Call:  &models.ActionCall{ID: callID, Scope: f.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID},
		Bound: f.Bound(t, run.AssignedAgentID, action), Arguments: arguments,
	})
}

// ReaderUpsertArguments are the arguments of an upsert of the Reddit reader
// binding, for the Agent target when target is set.
func ReaderUpsertArguments(target string) map[string]interface{} {
	arguments := map[string]interface{}{
		"bindingId": "reddit", "expectedRevision": 1, "skillId": "reddit.reader", "skillVersion": "1.0.0",
		"allowedActions": []interface{}{"read"}, "enablePrompt": false, "maximumRisk": "read",
	}
	if target != "" {
		arguments[skillbinding.SkillActionTargetAgentArgument] = target
	}
	return arguments
}
