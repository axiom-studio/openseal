package team

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestCompileManifestBuildsPortableTeamWithCalmDefaults(t *testing.T) {
	manifest := &Manifest{
		APIVersion: ManifestAPIVersion,
		Kind:       ManifestKind,
		Metadata: ManifestMetadata{
			ID: "market-research", Version: "1.0.0", DisplayName: "Market Research",
			Description: "Discover evidence-backed customer needs",
		},
		Spec: ManifestSpec{Members: []ManifestMember{
			{ID: "researcher", DisplayName: "Researcher", Purpose: "Collect evidence", AgentDefinitionID: "researcher", Approver: true, Lead: true},
			{ID: "observer", DisplayName: "Observer", Purpose: "Watch", AgentDefinitionID: "observer", ChannelParticipation: ChannelObserveOnly},
		}},
	}
	definition, err := CompileManifest(manifest, "catalog-market-research", workforce.DefinitionProvenance{
		Source: "marketplace", Reference: "listing:42", CreatedBy: "service:installer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if definition.ID != "catalog-market-research" ||
		!definition.Delegation.RequireAcceptance || !definition.Delegation.RequireCompletionReview ||
		definition.SharedContext.Retention != 7*24*time.Hour || definition.SharedContext.MaximumBytes != 1<<20 ||
		!definition.SharedContext.AllowMemberRead || !definition.SharedContext.AllowMemberWrite ||
		definition.Approvals.MaximumRisk != capability.RiskLevelRead {
		t.Fatalf("compiled definition=%#v", definition)
	}
	roster, err := ManifestRoster(manifest, map[string]string{"researcher": "agent-1", "observer": "agent-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(roster) != 2 || !reflect.DeepEqual(roster[0], RosterAssignment{
		ID: "researcher", AgentDeploymentID: "agent-1", DisplayName: "Researcher", Purpose: "Collect evidence",
		ChannelParticipation: ChannelActive, Approver: true, Lead: true,
	}) || roster[1].ChannelParticipation != ChannelObserveOnly || roster[1].AgentDeploymentID != "agent-2" {
		t.Fatalf("roster=%#v", roster)
	}
	if _, err := ManifestRoster(manifest, map[string]string{"researcher": "agent-1"}); err == nil {
		t.Fatal("a member without an Agent deployment was accepted")
	}
}

func TestCompileManifestRefusesTheRoleSlotVersion(t *testing.T) {
	data := []byte(`apiVersion: openseal.dev/team/v1alpha1
kind: Team
metadata:
  id: research
  version: 1.0.0
  displayName: Research
spec:
  members:
    - id: researcher
      displayName: Researcher
      purpose: Collect evidence
      agentDefinitionId: researcher
`)
	manifest, err := DecodeManifestYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileManifest(manifest, "", workforce.DefinitionProvenance{}); err == nil ||
		!strings.Contains(err.Error(), "v1alpha1") || !strings.Contains(err.Error(), "spec.members") {
		t.Fatalf("role-slot manifest version: %v", err)
	}
	roles := strings.Replace(strings.Replace(string(data), "v1alpha1", "v1alpha2", 1), "  members:", "  roles:", 1)
	if _, err := DecodeManifestYAML([]byte(roles)); err == nil || !strings.Contains(err.Error(), "roles") {
		t.Fatalf("spec.roles was accepted: %v", err)
	}
	manifest.APIVersion = ManifestAPIVersion
	manifest.Spec.Members = nil
	if _, err := CompileManifest(manifest, "", workforce.DefinitionProvenance{}); err == nil {
		t.Fatal("a manifest without members was accepted")
	}
}

func TestCompileManifestPreservesExplicitCollaborationPolicy(t *testing.T) {
	no := false
	yes := true
	manifest := &Manifest{
		APIVersion: ManifestAPIVersion, Kind: ManifestKind,
		Metadata: ManifestMetadata{ID: "review", Version: "2.0.0", DisplayName: "Review"},
		Spec: ManifestSpec{
			Members: []ManifestMember{{
				ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review", AgentDefinitionID: "reviewer",
				ChannelParticipation: ChannelObserveOnly,
			}},
			Routing: RoutingPolicy{Notes: "  Reviewer owns release notes  "},
			Delegation: ManifestDelegationPolicy{
				MaximumDepth: 5, MaximumConcurrent: 8, AllowPeerDelegation: &yes,
				RequireAcceptance: &no, RequireCompletionReview: &no,
			},
			SharedContext: ManifestSharedContextPolicy{
				Retention: "24h", MaximumBytes: 2048, AllowMemberRead: &yes, AllowMemberWrite: &no,
			},
			Approvals: ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
		},
	}
	definition, err := CompileManifest(manifest, "", workforce.DefinitionProvenance{})
	if err != nil {
		t.Fatal(err)
	}
	if definition.Routing.Notes != "Reviewer owns release notes" || !definition.Delegation.AllowPeerDelegation ||
		definition.Delegation.RequireAcceptance || definition.Delegation.RequireCompletionReview ||
		definition.SharedContext.AllowMemberWrite || definition.SharedContext.Retention != 24*time.Hour {
		t.Fatalf("explicit policy was not preserved: %#v", definition)
	}
}

func TestDecodeManifestYAMLIsStrictAndExcludesPlacement(t *testing.T) {
	data := []byte(`apiVersion: openseal.dev/team/v1alpha2
kind: Team
metadata:
  id: research
  version: 1.0.0
  displayName: Research
spec:
  members:
    - id: researcher
      displayName: Researcher
      purpose: Collect evidence
      agentDefinitionId: researcher
  sharedContext:
    retention: 48h
`)
	manifest, err := DecodeManifestYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata.ID != "research" || manifest.Spec.Members[0].AgentDefinitionID != "researcher" {
		t.Fatalf("manifest=%#v", manifest)
	}
	for _, field := range []string{"roster", "credentials", "environment", "deploymentId"} {
		invalid := strings.Replace(string(data), "spec:\n", "spec:\n  "+field+": forbidden\n", 1)
		if _, err = DecodeManifestYAML([]byte(invalid)); err == nil {
			t.Fatalf("host-owned field %s was accepted", field)
		}
	}
	retired := strings.Replace(string(data), "  sharedContext:\n", "  coordination:\n    maximumSpeakersPerRound: 2\n  sharedContext:\n", 1)
	if _, err = DecodeManifestYAML([]byte(retired)); err == nil || !strings.Contains(err.Error(), "coordination") {
		t.Fatalf("retired coordination policy was accepted: %v", err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"runtimeIdentity", "catalogId", "credentials", "roster"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("portable Team manifest leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestCompileManifestRejectsInvalidRetentionAndIdentity(t *testing.T) {
	base := &Manifest{
		APIVersion: ManifestAPIVersion, Kind: ManifestKind,
		Metadata: ManifestMetadata{ID: "research", Version: "1.0.0", DisplayName: "Research"},
		Spec: ManifestSpec{Members: []ManifestMember{{
			ID: "researcher", DisplayName: "Researcher", Purpose: "Research", AgentDefinitionID: "researcher",
		}}},
	}
	base.Spec.SharedContext.Retention = "forever"
	if _, err := CompileManifest(base, "", workforce.DefinitionProvenance{}); err == nil {
		t.Fatal("invalid retention was accepted")
	}
	base.Spec.SharedContext.Retention = ""
	base.Metadata.ID = "Not Portable"
	if _, err := CompileManifest(base, "", workforce.DefinitionProvenance{}); err == nil {
		t.Fatal("invalid identity was accepted")
	}
}
