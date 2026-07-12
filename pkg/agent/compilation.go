package agent

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type CompilationStatus string

const (
	CompilationClean  CompilationStatus = "clean"
	CompilationFailed CompilationStatus = "failed"
)

// CompilationSource identifies the immutable authoring input without retaining
// host-specific graph data in the portable kernel.
type CompilationSource struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// CompilationDiagnostic is safe operator-facing compiler output. Path and
// NodeID allow a host UI to link back to its own authoring representation.
type CompilationDiagnostic struct {
	NodeID  string `json:"nodeId,omitempty"`
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DefinitionCompilation is an immutable, tenant-scoped record of translating
// an authoring source into a native Agent definition. Failed candidates remain
// observable without becoming executable definitions.
type DefinitionCompilation struct {
	ID               string                    `json:"id"`
	Scope            capability.ScopeReference `json:"scope"`
	DeploymentID     string                    `json:"deploymentId"`
	DefinitionID     string                    `json:"definitionId"`
	CandidateVersion string                    `json:"candidateVersion"`
	Source           CompilationSource         `json:"source"`
	TargetDigest     string                    `json:"targetDigest,omitempty"`
	Status           CompilationStatus         `json:"status"`
	Diagnostics      []CompilationDiagnostic   `json:"diagnostics,omitempty"`
	CreatedAt        time.Time                 `json:"createdAt"`
}

func (c *DefinitionCompilation) Validate() error {
	if c == nil {
		return errors.New("agent definition compilation is required")
	}
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Scope.Kind) == "" || strings.TrimSpace(c.Scope.ID) == "" ||
		strings.TrimSpace(c.DeploymentID) == "" || strings.TrimSpace(c.DefinitionID) == "" || strings.TrimSpace(c.CandidateVersion) == "" ||
		strings.TrimSpace(c.Source.Kind) == "" || strings.TrimSpace(c.Source.ID) == "" || strings.TrimSpace(c.Source.Version) == "" || strings.TrimSpace(c.Source.Digest) == "" || c.CreatedAt.IsZero() {
		return errors.New("compilation identity, scope, source, candidate version, and creation time are required")
	}
	if c.Status != CompilationClean && c.Status != CompilationFailed {
		return errors.New("compilation status must be clean or failed")
	}
	if c.Status == CompilationClean && (strings.TrimSpace(c.TargetDigest) == "" || len(c.Diagnostics) != 0) {
		return errors.New("clean compilation requires a target digest and no diagnostics")
	}
	if c.Status == CompilationFailed && (strings.TrimSpace(c.TargetDigest) != "" || len(c.Diagnostics) == 0) {
		return errors.New("failed compilation requires diagnostics and no target digest")
	}
	for _, diagnostic := range c.Diagnostics {
		if strings.TrimSpace(diagnostic.Path) == "" || strings.TrimSpace(diagnostic.Code) == "" || strings.TrimSpace(diagnostic.Message) == "" {
			return errors.New("compilation diagnostics require path, code, and message")
		}
		if len(diagnostic.Path) > 1024 || len(diagnostic.Code) > 256 || len(diagnostic.Message) > 4096 || len(diagnostic.NodeID) > 512 {
			return errors.New("compilation diagnostic exceeds portable size limits")
		}
	}
	return nil
}

func cloneCompilation(value *DefinitionCompilation) *DefinitionCompilation {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Diagnostics = append([]CompilationDiagnostic(nil), value.Diagnostics...)
	return &clone
}

func sortCompilations(values []*DefinitionCompilation) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].ID > values[j].ID
		}
		return values[i].CreatedAt.After(values[j].CreatedAt)
	})
}
