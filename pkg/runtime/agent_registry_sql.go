package runtime

import (
	"encoding/json"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
)

func registryPayloads(deployment *kernelagent.AgentDeployment, activation kernelagent.DefinitionActivation) (string, string, error) {
	deploymentPayload, err := json.Marshal(deployment)
	if err != nil {
		return "", "", err
	}
	activationPayload, err := json.Marshal(activation)
	if err != nil {
		return "", "", err
	}
	return string(deploymentPayload), string(activationPayload), nil
}
