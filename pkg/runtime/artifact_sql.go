package runtime

import (
	"encoding/json"
)

func decodeArtifact(payload string) (*Artifact, error) {
	var artifact Artifact
	if err := json.Unmarshal([]byte(payload), &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}
