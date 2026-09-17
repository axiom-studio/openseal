package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestHostedTurnFormPreservesBindingWithDuplicateCapabilityNames(t *testing.T) {
	actions := []capability.ModelAction{
		{Name: "openseal.presentation.publish_surface", BindingID: "bundled:presentation", BindingRevision: 2},
		{Name: "openseal.presentation.publish_surface", BindingID: "workforce:presentation", BindingRevision: 1},
	}
	response, err := CompileHostedTurnForm(HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion,
		NextRunStatus: AgentRunStatusRunning,
		ProposedAction: &HostedTurnActionForm{
			Capability: actions[0].Name, Summary: "Publish the illustrative graph",
			IdempotencyKey: "graph", Arguments: map[string]interface{}{},
		},
	}, actions)
	if err != nil {
		t.Fatal(err)
	}
	runner := &HostedTurnRunner{config: HostedTurnRunnerConfig{Actions: actions}}
	if _, err := runner.reuseSucceededAction(*response.ProposedAction, response.ContinuationCheckpoint); err != nil {
		t.Fatalf("compiled proposal became ambiguous: %v", err)
	}
}
