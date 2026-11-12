package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestBindingActionArgumentResolverOverridesModelWithTrustedSessionValues(t *testing.T) {
	resolver := skillbinding.BindingActionArgumentResolver{}
	bound := &skill.BoundAction{
		Action: skill.Action{Name: "lookup"},
		Binding: &skill.Binding{ArgumentBindings: map[string]map[string]skill.BindingArgumentValue{
			"lookup": {
				"account":   {Source: skill.BindingArgumentLiteral, Literal: "support"},
				"sessionId": {Source: skill.BindingArgumentSessionID},
				"userId":    {Source: skill.BindingArgumentVerifiedClaim, Claim: "userId"},
			},
		}},
	}
	run := &models.AgentRun{Context: map[string]interface{}{agentrun.RunContextSessionKey: map[string]interface{}{
		skillbinding.SessionContextIDKey:             "session-123",
		skillbinding.SessionContextVerifiedClaimsKey: map[string]interface{}{"userId": "customer-42"},
	}}}
	arguments, handled, err := resolver.ResolveActionProposalArguments(context.Background(), agentrun.ActionProposalValidationInput{
		Run: run, Bound: bound, Arguments: map[string]interface{}{"query": "open orders", "userId": "model-forged"},
	})
	if err != nil || !handled {
		t.Fatalf("resolve = %#v, %v, %v", arguments, handled, err)
	}
	if arguments["account"] != "support" || arguments["sessionId"] != "session-123" || arguments["userId"] != "customer-42" || arguments["query"] != "open orders" {
		t.Fatalf("resolved arguments = %#v", arguments)
	}
}

func TestBindingActionArgumentResolverRejectsMissingVerifiedClaim(t *testing.T) {
	resolver := skillbinding.BindingActionArgumentResolver{}
	bound := &skill.BoundAction{Action: skill.Action{Name: "lookup"}, Binding: &skill.Binding{
		ArgumentBindings: map[string]map[string]skill.BindingArgumentValue{"lookup": {
			"userId": {Source: skill.BindingArgumentVerifiedClaim, Claim: "userId"},
		}},
	}}
	_, _, err := resolver.ResolveActionProposalArguments(context.Background(), agentrun.ActionProposalValidationInput{
		Run:   &models.AgentRun{Context: map[string]interface{}{agentrun.RunContextSessionKey: map[string]interface{}{skillbinding.SessionContextIDKey: "session-123"}}},
		Bound: bound,
	})
	if err == nil {
		t.Fatal("expected missing verified claim to fail closed")
	}
}
