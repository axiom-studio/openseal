package openseal

import (
	"fmt"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/progression"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/sourceartifact"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// wireStore builds every store-backed service from e.store once the options
// have run. New's doc comment states the rule for each service.
func (e *Engine) wireStore() error {
	if e.store == nil {
		store, err := e.openOwnedStore()
		if err != nil {
			return err
		}
		e.store = store
	}
	store := e.store

	e.portfolio = portfolio.NewPortfolioService(store)
	e.activity = activity.NewRunActivityService(store, store)
	e.turns = activity.NewAgentTurnService(store, store)
	e.runQueue = agentrun.NewAgentRunScheduler(store)
	e.wake = agentrun.NewAgentRunWakeService(store, store)
	if dependencyStore, ok := store.(models.DependencyKernelStore); ok {
		e.dependencies = portfolio.NewDependencyCoordinator(dependencyStore)
	}
	if conversationStore, ok := store.(models.ConversationStore); ok {
		e.conversations = conversation.NewConversationService(conversationStore)
		e.conversationChanges, _ = conversation.NewConversationChangeService(conversationStore, store)
		e.conversationChanges.SetSkillSetupRequestReader(e.reconciledSkillSetupRequests)
		if embedStore, ok := store.(models.EmbedStore); ok {
			e.embeds, _ = conversation.NewEmbedSessionService(embedStore, conversationStore)
		}
	}
	if spaceStore, ok := store.(models.SpaceStore); ok {
		e.spaces = portfolio.NewSpaceService(spaceStore)
	}
	if collaborationStore, ok := store.(models.CollaborationKernelStore); ok {
		e.collaboration = collaboration.NewCollaborationService(collaborationStore)
		e.collaboration.SetAcceptedRunExecutionPreparer(e)
	}
	if artifactStore, ok := store.(models.ArtifactStore); ok {
		e.artifacts = artifactcatalog.NewArtifactCatalog(artifactStore).WithPlacement(artifactcatalog.NewStoreArtifactPlacement(store))
	}

	if err := e.wireRegistries(); err != nil {
		return err
	}
	if auditStore, ok := store.(models.RunbookExecutionAuditStore); ok {
		e.runbookAudits = routine.NewRunbookExecutionAuditService(auditStore, e.agents)
	}
	if upgradeStore, ok := store.(models.SkillReferenceUpgradeStore); ok {
		e.skillReferenceUpgrades = skillbinding.NewSkillReferenceUpgradeService(upgradeStore, e.skills)
	}
	if sourceStore, ok := store.(sourceartifact.Store); e.skillSources == nil && ok {
		e.skillSources, _ = sourceartifact.NewService(sourceStore)
	}
	return nil
}

// wireRegistries builds the Agent and Team registries and the Skill catalog.
// Each uses e.store when it implements the registry's store, and otherwise
// the in-memory store, so they are never missing.
func (e *Engine) wireRegistries() error {
	var fallbackAgents *kernelagent.Registry
	inMemoryAgents := func() (*kernelagent.Registry, error) {
		if fallbackAgents == nil {
			store, err := e.openOwnedStore()
			if err != nil {
				return nil, err
			}
			fallbackAgents = kernelagent.NewRegistryWithStore(store)
		}
		return fallbackAgents, nil
	}

	if agentStore, ok := e.store.(kernelagent.Store); ok {
		e.agents = kernelagent.NewRegistryWithStore(agentStore)
	} else {
		agents, err := inMemoryAgents()
		if err != nil {
			return err
		}
		e.agents = agents
	}

	if teamStore, ok := e.store.(kernelteam.Store); ok {
		e.teams = kernelteam.NewRegistryWithStore(teamStore, e.agents)
	} else {
		agents, err := inMemoryAgents()
		if err != nil {
			return err
		}
		e.teams = kernelteam.NewRegistryWithStore(e.ownedStore, agents)
	}
	if e.spaces != nil {
		if err := e.spaces.UseTeams(e.teams); err != nil {
			return err
		}
	}
	e.progression = progression.NewService(e.agents, e.teams)

	if e.skills != nil {
		return nil
	}
	if skillStore, ok := e.store.(skill.CatalogStore); ok {
		e.skills = skill.NewCatalogWithStore(skillStore)
		return nil
	}
	store, err := e.openOwnedStore()
	if err != nil {
		return err
	}
	e.skills = skill.NewCatalogWithStore(store)
	return nil
}

// openOwnedStore returns the in-memory store the Engine owns, opening it on
// first use.
func (e *Engine) openOwnedStore() (*sqlite.Store, error) {
	if e.ownedStore != nil {
		return e.ownedStore, nil
	}
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		return nil, fmt.Errorf("default store: %w", err)
	}
	e.ownedStore = store
	return store, nil
}

func (e *Engine) closeOwnedStore() error {
	if e.ownedStore == nil {
		return nil
	}
	err := e.ownedStore.Close()
	e.ownedStore = nil
	return err
}
