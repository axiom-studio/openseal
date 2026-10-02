//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
)

func TestPostgresAgentChannelActivationIsAtomic(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := "openseal_channel_atomic_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	_, catalog, scope, adapter := externalConversationTestCatalog(t, ctx)
	endpoints := NewExternalConversationEndpointService(store, catalog)
	endpoint, err := endpoints.Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "channel", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"}, DeploymentID: "slack-agent",
		Name: "Slack", Adapter: adapter, Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy:  ExternalConversationPolicy{MessageSelection: ExternalConversationSelectMentions, ReplyMode: ExternalConversationReplyThread, IgnoreBots: true}, Status: ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	agents := kernelagent.NewRegistryWithStore(store)
	base, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "channel-agent", Version: "1.0.0", DisplayName: "Channel Agent", Purpose: "Help with Slack", SystemPrompt: "Work carefully.",
		Authority:  kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 1},
		Amendments: workforce.AmendmentPolicy{AllowedFields: []string{"channels"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	deployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "slack-agent", Scope: agentScope, DefinitionID: base.ID, ActiveVersion: base.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "initial")
	if err != nil {
		t.Fatal(err)
	}
	candidate := cloneAgentDefinitionForAction(base)
	candidate.Version = "2.0.0"
	candidate.Channels = []kernelagent.ChannelRoute{{EndpointID: endpoint.ID, MessageSelection: "all_messages", ReplyMode: "thread", IgnoreBots: true, Purposes: []string{"conversation"}}}
	amendment, err := agents.ProposeAmendment(ctx, kernelagent.ProposeAmendmentRequest{
		Scope: agentScope, DeploymentID: deployment.ID, Candidate: candidate, ProposerType: "user", ProposerID: "operator",
		Rationale: "Configure channel", IdempotencyKey: "first-channel", ExpectedDeploymentRevision: deployment.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := endpoint.Policy
	policy.MessageSelection = ExternalConversationSelectAllMessages
	prepared, err := endpoints.PrepareUpdate(ctx, scope, endpoint.ID, UpdateExternalConversationEndpointRequest{ExpectedRevision: endpoint.Revision, Policy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE FUNCTION `+store.quotedSchema()+`.reject_channel_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'endpoint write rejected'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_channel_update BEFORE UPDATE ON `+store.table("external_conversation_endpoints")+` FOR EACH ROW EXECUTE FUNCTION `+store.quotedSchema()+`.reject_channel_update()`); err != nil {
		t.Fatal(err)
	}
	activate := func() error {
		_, _, _, err := agents.ActivateAmendmentWithCommit(ctx, agentScope, amendment.ID, amendment.Revision, "user", "operator", "reviewed", endpoints.channelActivationCommit(prepared, endpoint.Revision, deployment.Revision))
		return err
	}
	if err := activate(); err == nil || !strings.Contains(err.Error(), "endpoint write rejected") {
		t.Fatalf("write rejection: %v", err)
	}
	unchanged, _ := agents.GetDeployment(ctx, agentScope, deployment.ID)
	versions, _ := agents.ListDefinitionVersions(ctx, base.ID)
	activations, _ := agents.ListActivations(ctx, agentScope, deployment.ID)
	unchangedEndpoint, _ := endpoints.Get(ctx, scope, endpoint.ID)
	unchangedAmendment, _ := agents.GetAmendment(ctx, agentScope, amendment.ID)
	if !equalJSON(unchanged, deployment) || len(versions) != 1 || len(activations) != 1 || !equalJSON(unchangedEndpoint, endpoint) || unchangedAmendment.Status == kernelagent.AmendmentActivated {
		t.Fatalf("failed PostgreSQL transaction left partial changes: deployment=%#v endpoint=%#v amendment=%#v versions=%d activations=%d", unchanged, unchangedEndpoint, unchangedAmendment, len(versions), len(activations))
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_channel_update ON `+store.table("external_conversation_endpoints")); err != nil {
		t.Fatal(err)
	}
	// A concurrent retirement must win even after a prepared, reviewed update.
	retired := ExternalConversationEndpointRetired
	retiredEndpoint, err := endpoints.Update(ctx, scope, endpoint.ID, UpdateExternalConversationEndpointRequest{ExpectedRevision: endpoint.Revision, Status: &retired})
	if err != nil {
		t.Fatal(err)
	}
	if err := activate(); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("retirement race: %v", err)
	}
	unchanged, _ = agents.GetDeployment(ctx, agentScope, deployment.ID)
	unchangedEndpoint, _ = endpoints.Get(ctx, scope, endpoint.ID)
	if !equalJSON(unchanged, deployment) || !equalJSON(unchangedEndpoint, retiredEndpoint) {
		t.Fatal("stale PostgreSQL activation changed Agent or retired endpoint")
	}
}
