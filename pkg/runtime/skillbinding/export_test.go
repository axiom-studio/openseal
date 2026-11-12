package skillbinding

import (
	"context"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Test-only access to skill binding internals for the external tests
// (package skillbinding_test), which use skillbindingtest's fixtures and so
// cannot be in-package tests.

type InteractionRequestRefusal = interactionRequestRefusal

func (d *ActionDispatcher) RequestSkillSetup(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun, member string, umbrella, owner models.ObjectiveOwner) (map[string]interface{}, error) {
	return d.requestSkillSetup(ctx, input, run, member, umbrella, owner)
}

func (d *ActionDispatcher) RequestConnection(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun, member string, umbrella, owner models.ObjectiveOwner) (map[string]interface{}, error) {
	return d.requestConnection(ctx, input, run, member, umbrella, owner)
}

func (d *ActionDispatcher) ListSkillSetupRequests(ctx context.Context, run *models.AgentRun, member string, umbrella models.ObjectiveOwner) (map[string]interface{}, error) {
	return d.listSkillSetupRequests(ctx, run, member, umbrella)
}

func (d *ActionDispatcher) FindApps(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun, umbrella models.ObjectiveOwner) (map[string]interface{}, error) {
	return d.findApps(ctx, input, run, umbrella)
}

func (d *ActionDispatcher) Catalog() *skill.Catalog { return d.catalog }

func (d *ActionDispatcher) SetDiscovery(discovery skill.DiscoveryProvider) {
	d.discovery = discovery
}

func SkillActionTarget(ctx context.Context, catalog *skill.Catalog, agents *kernelagent.Registry, run *models.AgentRun, member string, arguments map[string]interface{}) (*skillTargetAgent, error) {
	return skillActionTarget(ctx, catalog, agents, run, member, arguments)
}

func DecodeSkillSetupArguments(arguments map[string]interface{}) (skillSetupArguments, error) {
	return decodeSkillSetupArguments(arguments)
}

func SkillSetupConversationOrigin(ctx context.Context, store models.KernelStore, run *models.AgentRun, member string, umbrella models.ObjectiveOwner) (string, string, error) {
	return skillSetupConversationOrigin(ctx, store, run, member, umbrella)
}

func SkillSetupAction() skill.Action { return skillSetupAction() }

func ServiceKey(value string) string { return serviceKey(value) }

func RefuseInteraction(message string) error { return refuseInteraction(message) }

func InteractionRequestRefusalResult(err error) map[string]interface{} {
	return interactionRequestRefusalResult(err)
}
