package runtime

import (
	"context"
	"encoding/json"
	"github.com/axiom-studio/openseal/pkg/skill"
	"strings"
	"testing"
)

func TestSetupListReportsSavedBindingAccessWithoutCredentials(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		ctx := context.Background()
		store := NewMemoryStore()
		r := setupRequestFixture()
		catalog := skillActionCatalog(t, ctx, r.Scope, r.DeploymentID)
		b := &skill.Binding{ID: "saved", Revision: 1, Scope: skill.ScopeReference{Kind: r.Scope.Kind, ID: r.Scope.ID}, DeploymentID: r.DeploymentID, SkillID: r.SkillID, SkillVersion: r.SkillVersion, AllowedActions: []string{"read"}, EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://private-account"}}}
		if err := catalog.Bind(ctx, b); err != nil {
			t.Fatal(err)
		}
		if disabled {
			_, err := catalog.DisableBinding(ctx, skill.DisableBindingRequest{Scope: b.Scope, DeploymentID: b.DeploymentID, BindingID: b.ID, ExpectedRevision: 1, Actor: skill.BindingActor{Type: "user", ID: "owner"}, Reason: "Disconnect"})
			if err != nil {
				t.Fatal(err)
			}
		}
		r.Status = "resolved"
		r.ResolvedBindingID = b.ID
		r.ResolvedBindingRevision = 1
		r.ResolvedBy = "owner"
		if err := store.SaveSkillSetupRequest(ctx, r, 0); err != nil {
			t.Fatal(err)
		}
		d, err := NewSkillBindingActionDispatcher(store, catalog, nil)
		if err != nil {
			t.Fatal(err)
		}
		run := &AgentRun{ID: r.RunID, Scope: r.Scope, Kind: RunKindConversation,
			Context: map[string]interface{}{conversationRunContextConversationID: r.ConversationID, conversationRunContextTriggerID: r.TriggerMessageID}}
		for _, missing := range []string{conversationRunContextConversationID, conversationRunContextTriggerID} {
			unproved := cloneAgentRun(run)
			delete(unproved.Context, missing)
			if result, err := d.listSkillSetupRequests(ctx, unproved, r.DeploymentID); err == nil || result != nil {
				t.Fatalf("setup access escaped an incomplete conversation origin (%s): %#v %v", missing, result, err)
			}
		}
		result, err := d.listSkillSetupRequests(ctx, run, r.DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		item := result["requests"].([]interface{})[0].(map[string]interface{})
		access := item["bindingAccess"].(map[string]interface{})
		if access["enabled"] != !disabled || access["promptEnabled"] != !disabled {
			t.Fatalf("access=%v", access)
		}
		actions := access["enabledActions"].([]string)
		if (!disabled && len(actions) != 1) || (disabled && len(actions) != 0) {
			t.Fatalf("actions=%v", actions)
		}
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), "private-account") || strings.Contains(string(encoded), "credentials") {
			t.Fatalf("account details exposed: %s", encoded)
		}
		mismatched := setupRequestFixture()
		mismatched.ID = "request-other-version"
		mismatched.ActionCallID = "call-other-version"
		mismatched.SkillVersion = "different"
		mismatched.Status = "resolved"
		mismatched.ResolvedBindingID = b.ID
		mismatched.ResolvedBindingRevision = 1
		mismatched.ResolvedBy = "owner"
		if err := store.SaveSkillSetupRequest(ctx, mismatched, 0); err != nil {
			t.Fatal(err)
		}
		result, err = d.listSkillSetupRequests(ctx, run, r.DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, value := range result["requests"].([]interface{}) {
			item := value.(map[string]interface{})
			if item["id"] == mismatched.ID {
				found = true
				if _, ok := item["bindingAccess"]; ok {
					t.Fatal("access attributed to a different skill version")
				}
			}
		}
		if !found {
			t.Fatal("mismatched request was omitted")
		}
	}
}
