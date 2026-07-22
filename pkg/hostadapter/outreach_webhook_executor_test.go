package hostadapter

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/outreach"
	opensealruntime "github.com/axiom-studio/openseal/pkg/runtime"
)

func TestOutreachWebhookExecutorAdaptsExactCanonicalInvocation(t *testing.T) {
	var invocation opensealruntime.ToolInvocation
	executor := &OutreachWebhookExecutor{invoker: opensealruntime.ToolInvokerFunc(func(_ context.Context, got opensealruntime.ToolInvocation) (map[string]interface{}, error) {
		invocation = got
		return map[string]interface{}{"outreachReceipt": map[string]interface{}{"provider": "test"}}, nil
	})}
	config := map[string]interface{}{
		"targetUri": "https://hooks.example.com/replies/thread-1", "body": "reviewed",
		outreach.ActionCallIDTransportKey: "action-1", outreach.RunIDTransportKey: "run-1", outreach.DeploymentIDTransportKey: "researcher",
	}
	result, err := executor.Execute(t.Context(), &StepDefinition{Config: config}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Name != outreach.SkillID || invocation.SkillID != outreach.SkillID || invocation.SkillVersion != outreach.SkillVersion ||
		invocation.Action != outreach.PostReply || invocation.ActionCallID != "action-1" || invocation.RunID != "run-1" || invocation.DeploymentID != "researcher" ||
		invocation.Arguments["body"] != "reviewed" || result.Output["outreachReceipt"] == nil {
		t.Fatalf("invocation=%#v result=%#v", invocation, result)
	}
	invocation.Arguments["body"] = "mutated"
	if config["body"] != "reviewed" {
		t.Fatal("adapter leaked mutable config into canonical invocation")
	}
}

func TestOutreachWebhookExecutorFailsClosedWithoutCanonicalIdentity(t *testing.T) {
	executor := &OutreachWebhookExecutor{invoker: opensealruntime.ToolInvokerFunc(func(context.Context, opensealruntime.ToolInvocation) (map[string]interface{}, error) {
		t.Fatal("incomplete invocation reached OpenSeal")
		return nil, nil
	})}
	base := map[string]interface{}{outreach.ActionCallIDTransportKey: "action-1", outreach.RunIDTransportKey: "run-1", outreach.DeploymentIDTransportKey: "researcher"}
	for _, missing := range []string{outreach.ActionCallIDTransportKey, outreach.RunIDTransportKey, outreach.DeploymentIDTransportKey} {
		config := cloneMap(base)
		delete(config, missing)
		if _, err := executor.Execute(t.Context(), &StepDefinition{Config: config}, nil); err == nil {
			t.Fatalf("missing %s was accepted", missing)
		}
	}
}
