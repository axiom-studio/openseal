package openseal

import (
	"fmt"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/progression"
	"github.com/axiom-studio/openseal/pkg/runtime"
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

	e.portfolio = runtime.NewPortfolioService(store)
	e.activity = runtime.NewRunActivityService(store, store)
	e.turns = runtime.NewAgentTurnService(store, store)
	e.runQueue = runtime.NewAgentRunScheduler(store)
	e.wake = runtime.NewAgentRunWakeService(store, store)
	if dependencyStore, ok := store.(runtime.DependencyKernelStore); ok {
		e.dependencies = runtime.NewDependencyCoordinator(dependencyStore)
	}
	if conversationStore, ok := store.(runtime.ConversationStore); ok {
		e.conversations = runtime.NewConversationService(conversationStore)
		e.conversationChanges, _ = runtime.NewConversationChangeService(conversationStore, store)
		e.conversationChanges.SetSkillSetupRequestReader(e.reconciledSkillSetupRequests)
		if embedStore, ok := store.(runtime.EmbedStore); ok {
			e.embeds, _ = runtime.NewEmbedSessionService(embedStore, conversationStore)
		}
	}
	if spaceStore, ok := store.(runtime.SpaceStore); ok {
		e.spaces = runtime.NewSpaceService(spaceStore)
	}
	if collaborationStore, ok := store.(runtime.CollaborationKernelStore); ok {
		e.collaboration = runtime.NewCollaborationService(collaborationStore)
		e.collaboration.SetAcceptedRunExecutionPreparer(e)
	}
	if artifactStore, ok := store.(runtime.ArtifactStore); ok {
		e.artifacts = runtime.NewArtifactCatalog(artifactStore).WithPlacement(runtime.NewStoreArtifactPlacement(store))
	}

	if err := e.wireRegistries(); err != nil {
		return err
	}
	if auditStore, ok := store.(runtime.RunbookExecutionAuditStore); ok {
		e.runbookAudits = runtime.NewRunbookExecutionAuditService(auditStore, e.agents)
	}
	if upgradeStore, ok := store.(runtime.SkillReferenceUpgradeStore); ok {
		e.skillReferenceUpgrades = runtime.NewSkillReferenceUpgradeService(upgradeStore, e.skills)
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
func (e *Engine) openOwnedStore() (*runtime.SQLiteStore, error) {
	if e.ownedStore != nil {
		return e.ownedStore, nil
	}
	store, err := runtime.NewInMemorySQLiteStore()
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
