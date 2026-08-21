package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestBindingActionArgumentResolverOverridesModelWithTrustedSessionValues(t *testing.T) {
	resolver := BindingActionArgumentResolver{}
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
	run := &AgentRun{Context: map[string]interface{}{RunContextSessionKey: map[string]interface{}{
		sessionContextIDKey:             "session-123",
		sessionContextVerifiedClaimsKey: map[string]interface{}{"userId": "customer-42"},
	}}}
	arguments, handled, err := resolver.ResolveActionProposalArguments(context.Background(), ActionProposalValidationInput{
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
	resolver := BindingActionArgumentResolver{}
	bound := &skill.BoundAction{Action: skill.Action{Name: "lookup"}, Binding: &skill.Binding{
		ArgumentBindings: map[string]map[string]skill.BindingArgumentValue{"lookup": {
			"userId": {Source: skill.BindingArgumentVerifiedClaim, Claim: "userId"},
		}},
	}}
	_, _, err := resolver.ResolveActionProposalArguments(context.Background(), ActionProposalValidationInput{
		Run:   &AgentRun{Context: map[string]interface{}{RunContextSessionKey: map[string]interface{}{sessionContextIDKey: "session-123"}}},
		Bound: bound,
	})
	if err == nil {
		t.Fatal("expected missing verified claim to fail closed")
	}
}
