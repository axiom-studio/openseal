package action_test

import (
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// The durable fence admits a host binding only without a stored row of the
// same identity, and only while the call matches it exactly.
func TestActionProposalFenceChecksHostBindings(t *testing.T) {
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "one"}
	claimed := claimedOwnedActionRun(t, store, scope, time.Now().UTC(), "worker", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"})
	catalog := skillbindingtest.HostConnectorCatalog(t, scope, map[string]bool{claimed.ID: true})
	bound, err := catalog.Resolve(skill.WithRunActor(t.Context(), skill.RunActor{RunID: claimed.ID, AgentID: "release-agent", Umbrella: skill.Umbrella{Kind: skill.UmbrellaAgent, ID: "release-agent"}}),
		skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "release-agent", "openseal.connector", "1.0.0", "read", skill.BindingReference{ID: "mcp:linear", Revision: 1})
	if err != nil || !bound.Host {
		t.Fatalf("resolve = %#v, %v", bound, err)
	}
	call := &models.ActionCall{Scope: scope, DeploymentID: "release-agent", BindingID: "mcp:linear", BindingRevision: 1, SkillID: "openseal.connector", SkillVersion: "1.0.0"}
	if err := models.ValidateActionSubmissionBinding(call, action.HostActionBinding(bound)); err != nil {
		t.Fatalf("matching host binding refused: %v", err)
	}
	call.BindingRevision = 2
	if err := models.ValidateActionSubmissionBinding(call, action.HostActionBinding(bound)); !errors.Is(err, skill.ErrBindingUnavailable) {
		t.Fatalf("stale host binding admitted: %v", err)
	}
	if action.HostActionBinding(&skill.BoundAction{Binding: bound.Binding}) != nil {
		t.Fatal("a stored binding was treated as host supplied")
	}
}
