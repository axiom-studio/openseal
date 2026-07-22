package hostadapter

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/outreach"
	opensealruntime "github.com/axiom-studio/openseal/pkg/runtime"
)

// OutreachWebhookExecutor is a temporary Atlas workflow adapter over
// OpenSeal's canonical governed transport. TLS, SSRF, redirect, idempotency,
// policy-decision, and receipt semantics live only in OpenSeal.
type OutreachWebhookExecutor struct {
	invoker          opensealruntime.ToolInvoker
	configurationErr error
}

func NewOutreachWebhookExecutor() *OutreachWebhookExecutor {
	invoker, err := outreach.NewWebhookInvoker(outreach.TransportInvocationAuthorizer{})
	return &OutreachWebhookExecutor{invoker: invoker, configurationErr: err}
}

func (e *OutreachWebhookExecutor) Type() string { return outreach.SkillID }

func (e *OutreachWebhookExecutor) Execute(ctx context.Context, step *StepDefinition, _ TemplateResolver) (*StepResult, error) {
	if e == nil || e.invoker == nil || e.configurationErr != nil || step == nil || step.Config == nil {
		return nil, errors.New("outreach webhook executor is not configured")
	}
	actionCallID, _ := step.Config[outreach.ActionCallIDTransportKey].(string)
	runID, _ := step.Config[outreach.RunIDTransportKey].(string)
	deploymentID, _ := step.Config[outreach.DeploymentIDTransportKey].(string)
	if strings.TrimSpace(actionCallID) == "" || strings.TrimSpace(runID) == "" || strings.TrimSpace(deploymentID) == "" {
		return nil, errors.New("outreach transport requires ActionCall, Run, and deployment identity")
	}
	output, err := e.invoker.InvokeTool(ctx, opensealruntime.ToolInvocation{
		Name: outreach.SkillID, DeploymentID: deploymentID, SkillID: outreach.SkillID, SkillVersion: outreach.SkillVersion,
		Action: outreach.PostReply, ActionCallID: actionCallID, RunID: runID, Arguments: cloneMap(step.Config),
	})
	if err != nil {
		return nil, err
	}
	return &StepResult{Output: output}, nil
}
