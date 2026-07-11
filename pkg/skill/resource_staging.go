package skill

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// ResourceStageRequest is the portable handoff from skill activation to a
// host-owned sandbox/resource adapter. It contains metadata only; source bytes
// remain behind the adapter's trusted ResourceContentProvider boundary.
type ResourceStageRequest struct {
	Scope        ScopeReference        `json:"scope"`
	DeploymentID string                `json:"deploymentId"`
	BindingID    string                `json:"bindingId"`
	SkillID      string                `json:"skillId"`
	SkillVersion string                `json:"skillVersion"`
	SourceDigest string                `json:"sourceDigest"`
	Resources    []capability.Resource `json:"resources"`
}

// ResourceStage is an immutable, non-secret description of materialized skill
// resources. Root is trusted only after activation validates it for the host OS.
type ResourceStage struct {
	Root     string `json:"root"`
	Revision string `json:"revision"`
	Adapter  string `json:"adapter"`
}

// ResourceStager is implemented by local, container, remote-node, or other
// governed hosts. OpenSeal activation consumes the same result for every host.
type ResourceStager interface {
	StageResources(context.Context, ResourceStageRequest) (*ResourceStage, error)
}

// ResourceContentProvider keeps retained source artifacts behind a trusted
// adapter boundary. Implementations must resolve by immutable source digest and
// normalized relative path; callers never receive a model-visible source blob.
type ResourceContentProvider interface {
	ReadResource(context.Context, string, string) ([]byte, error)
}

// ValidateResourceStageRequest lets host adapters fail before reading any
// retained source content.
func ValidateResourceStageRequest(request ResourceStageRequest) error {
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" ||
		strings.TrimSpace(request.DeploymentID) == "" || strings.TrimSpace(request.BindingID) == "" ||
		strings.TrimSpace(request.SkillID) == "" || strings.TrimSpace(request.SkillVersion) == "" {
		return errors.New("resource stage scope, deployment, binding, skill, and version are required")
	}
	if len(request.Resources) == 0 {
		return errors.New("resource stage requires at least one declared resource")
	}
	if strings.TrimSpace(request.SourceDigest) == "" {
		return errors.New("resource stage source digest is required")
	}
	digest, err := hex.DecodeString(strings.TrimSpace(request.SourceDigest))
	if err != nil || len(digest) != 32 {
		return errors.New("resource stage requires a SHA-256 source digest")
	}
	return nil
}
