package skillbinding

import (
	"errors"
	"fmt"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
)

// ErrAgentBehaviorFixed rejects a self-amendment by an Agent whose definition
// does not allow Agent-proposed amendments, such as the platform assistant.
// No correction can change that policy, so the attempt ends with a fixed reply.
var ErrAgentBehaviorFixed = errors.New("agent definition policy does not allow Agent-proposed amendments")

// AgentDefinitionPolicyFixed reports a definition no Agent may amend, such as
// the platform assistant: Agent proposals are off and no field is open.
func AgentDefinitionPolicyFixed(definition *kernelagent.AgentDefinition) bool {
	return !definition.Amendments.AgentMayPropose && len(definition.Amendments.AllowedFields) == 0
}

// AgentBuilderTargetError explains a missing target Agent to the model and
// returns any other lookup error unchanged.
func AgentBuilderTargetError(agentID string, err error) error {
	if errors.Is(err, kernelagent.ErrDeploymentNotFound) {
		return fmt.Errorf("agent %q does not exist; use list_agents for the exact agentId", strings.TrimSpace(agentID))
	}
	return err
}
