package commands

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	kernelbundle "github.com/axiom-studio/openseal/pkg/bundle"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestBundleCommandValidatesInspectsDiffsAndPlansUpgrade(t *testing.T) {
	current := commandWorkforceBundle(t, "1.0.0")
	target := commandWorkforceBundle(t, "1.1.0")
	target.Metadata.Description = "Reviewed update"
	if err := target.Seal(); err != nil {
		t.Fatal(err)
	}
	currentYAML, err := kernelbundle.EncodeYAML(current)
	if err != nil {
		t.Fatal(err)
	}
	targetYAML, err := kernelbundle.EncodeYAML(target)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"current.yaml": currentYAML, "target.yaml": targetYAML}
	read := func(path string) ([]byte, error) { return files[path], nil }
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"validate", "current.yaml"}, `"valid": true`},
		{[]string{"inspect", "current.yaml"}, `"agents": 1`},
		{[]string{"diff", "current.yaml", "target.yaml"}, `"type": "modified"`},
		{[]string{"plan-upgrade", "current.yaml", "target.yaml"}, `"rollbackTargetDigest"`},
	} {
		var output bytes.Buffer
		if err := runBundleCommand(&output, test.args, read); err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if !strings.Contains(output.String(), test.want) {
			t.Fatalf("%v output=%s", test.args, output.String())
		}
	}
}

func commandWorkforceBundle(t *testing.T, version string) *kernelbundle.Bundle {
	t.Helper()
	now := time.Unix(1, 0).UTC()
	definition := &agent.AgentDefinition{ID: "portable/researcher", Version: version, DisplayName: "Researcher", Purpose: "Research questions", SystemPrompt: "Research carefully.", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}}
	deployment := &agent.AgentDeployment{ID: "agent:source", Scope: capability.ScopeReference{Kind: "tenant", ID: "source"}, DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: agent.RolloutActive, Environment: "default", Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	artifact, err := agent.ExportBundle(agent.BundleExportRequest{Definition: definition, Deployment: deployment, Metadata: agent.BundleMetadata{ID: "researcher", Version: version, DisplayName: "Researcher"}, Manifest: agent.ManifestMetadata{ID: "researcher", Version: version, DisplayName: "Researcher"}})
	if err != nil {
		t.Fatal(err)
	}
	bundle := kernelbundle.New(kernelbundle.Metadata{ID: "research-workforce", Version: version, DisplayName: "Research Workforce"})
	bundle.Agents = []kernelbundle.Agent{{Key: "researcher", Artifact: artifact}}
	if err := bundle.Seal(); err != nil {
		t.Fatal(err)
	}
	return bundle
}
