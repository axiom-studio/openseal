package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestKernelResolvedActionArgumentResolverUsesDurableAgentSession(t *testing.T) {
	action := skill.Action{
		Name: "observe", InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"sessionId": map[string]interface{}{
					"type": "string", skill.SchemaExtensionKernelResolved: true,
					skill.SchemaExtensionKernelSource: skill.KernelSourceAgentSessionID,
				},
			},
			"required": []interface{}{"sessionId"},
		},
	}
	resolver := KernelResolvedActionArgumentResolver{}
	tests := []struct {
		name, assignedAgentID, expected string
	}{
		{name: "portable identifier", assignedAgentID: "rowan_1", expected: "rowan_1"},
		{name: "canonical resource identifier", assignedAgentID: "agent:0087af2af836ce1dc02dd387c10ea713", expected: "agent-2d16e5356edce2efcb563628564cec25"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, handled, err := resolver.ResolveActionProposalArguments(context.Background(), ActionProposalValidationInput{
				Run:       &AgentRun{AssignedAgentID: test.assignedAgentID},
				Bound:     &skill.BoundAction{Action: action},
				Arguments: map[string]interface{}{"sessionId": "model-spoofed"},
			})
			if err != nil || !handled || resolved["sessionId"] != test.expected {
				t.Fatalf("resolved = %#v handled=%v err=%v", resolved, handled, err)
			}
		})
	}
}

func TestKernelResolvedActionArgumentResolverRequiresAssignedAgent(t *testing.T) {
	action := skill.Action{Name: "observe", InputSchema: map[string]interface{}{
		"properties": map[string]interface{}{"sessionId": map[string]interface{}{
			"type": "string", skill.SchemaExtensionKernelResolved: true,
			skill.SchemaExtensionKernelSource: skill.KernelSourceAgentSessionID,
		}},
	}}
	_, _, err := (KernelResolvedActionArgumentResolver{}).ResolveActionProposalArguments(context.Background(), ActionProposalValidationInput{
		Run: &AgentRun{Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}}, Bound: &skill.BoundAction{Action: action},
	})
	if err == nil {
		t.Fatal("unassigned Team action received an Agent session")
	}
}
