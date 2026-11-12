package runtime

import (
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeArtifact(payload string) (*models.Artifact, error) {
	var artifact models.Artifact
	if err := json.Unmarshal([]byte(payload), &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}
