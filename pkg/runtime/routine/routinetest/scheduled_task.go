package routinetest

import (
	"path/filepath"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// NewFileStore is a SQLite kernel store for routine action tests.
func NewFileStore(t *testing.T, kind string) models.KernelStore {
	t.Helper()
	store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "routines.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// ScheduledTask creates a chat Run of the researcher Agent with a user
// request, deploys the Agent and binds the routine management skill (or
// the one definition given). It returns the Run, the catalog, the bound
// create_task action and create_task arguments for a schedule routine.
func ScheduledTask(t *testing.T, store models.KernelStore, runbookDefinitions ...*skill.Definition) (*models.AgentRun, *skill.Catalog, *skill.BoundAction, map[string]interface{}) {
	t.Helper()
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "task-test"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}
	reader := store.(models.ConversationStore)
	service := conversation.NewConversationService(reader)
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	c, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Recurring research", IdempotencyKey: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, conversation.PostChannelMessageRequest{Scope: scope, ConversationID: c.ID, ExpectedRevision: c.Revision, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user"}, Intent: models.MessageIntentQuestion, Content: "Check the public forecast every thirty minutes", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "user-request"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: owner.ID, ActingUser: "user", Goal: "Handle request", Source: models.RunSourceChat, Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: c.ID, models.ConversationRunTriggerIDContextKey: posted.Message.ID}})
	if err != nil {
		t.Fatal(err)
	}
	registryStore, ok := store.(kernelagent.Store)
	if !ok {
		registryStore = kernelagent.NewMemoryStore()
	}
	registry := kernelagent.NewRegistryWithStore(registryStore)
	definition := agentDefinition("1.0.0")
	if _, err := registry.RegisterDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: owner.ID, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "user", "test"); err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalog()
	def := routine.RunbookManagementSkill()
	if len(runbookDefinitions) == 1 {
		def = runbookDefinitions[0]
	}
	if err := catalog.Register(ctx, def); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{ID: "routines", Revision: 1, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: owner.ID, SkillID: def.ID, SkillVersion: def.Version, AllowedActions: []string{action.RunbookActionCreateTask, action.RunbookActionList, action.RunbookActionSetStatus, routine.RunbookActionStart, routine.RunbookActionReplaceSchedule, routine.RunbookActionEventSources, routine.RunbookActionSendTestEvent}, MaximumRisk: skill.RiskLevelWrite}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, binding.Scope, owner.ID, def.ID, def.Version, action.RunbookActionCreateTask)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"title": "Forecast update", "goal": "Use the browser to read the current public forecast and summarize it with its source.",
		"trigger": map[string]interface{}{"schedule": map[string]interface{}{"cron": "0 */30 * * * *", "timezone": "Asia/Kolkata", "maximumOccurrences": float64(2)}},
		"brief":   map[string]interface{}{"summary": "Reads the public forecast every thirty minutes and posts a summary here.", "grants": []interface{}{}}}
	return run, catalog, bound, args
}

func agentDefinition(version string) *kernelagent.AgentDefinition {
	return &kernelagent.AgentDefinition{
		ID: "operator", Version: version, DisplayName: "Operator", Purpose: "Operate", SystemPrompt: "Operate safely.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, MaxConcurrentRuns: 2},
	}
}
