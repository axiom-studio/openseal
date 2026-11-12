package runtime

import (
	"encoding/json"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func teamRegistryPayloads(deployment *kernelteam.Deployment, activation workforce.DefinitionActivation) (string, string, error) {
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
