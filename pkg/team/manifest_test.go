package team

import (
	"encoding/json"
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
		Spec: ManifestSpec{Roles: []ManifestRoleSlot{{
			ID: "researcher", DisplayName: "Researcher", Purpose: "Collect evidence",
			MinimumMembers: 1, MaximumMembers: 2, RequiredDefinitionIDs: []string{"researcher"},
		}}},
	}
	definition, err := CompileManifest(manifest, "catalog-market-research", workforce.DefinitionProvenance{
		Source: "marketplace", Reference: "listing:42", CreatedBy: "service:installer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if definition.ID != "catalog-market-research" ||
		definition.Coordination.MaximumSpeakersPerRound != 2 || !definition.Coordination.QuietByDefault ||
		!definition.Coordination.RequireRoleRelevance || !definition.Coordination.SuppressDuplicateContent ||
		!definition.Delegation.RequireAcceptance || !definition.Delegation.RequireCompletionReview ||
		definition.SharedContext.Retention != 7*24*time.Hour || definition.SharedContext.MaximumBytes != 1<<20 ||
		!definition.SharedContext.AllowMemberRead || !definition.SharedContext.AllowMemberWrite ||
		definition.Approvals.MaximumRisk != capability.RiskLevelRead ||
		definition.Roles[0].ChannelParticipation != RoleChannelActive {
		t.Fatalf("compiled definition=%#v", definition)
	}
	manifest.Spec.Roles[0].RequiredDefinitionIDs[0] = "mutated"
	if definition.Roles[0].RequiredDefinitionIDs[0] != "researcher" {
		t.Fatal("compiled definition aliases mutable manifest state")
	}
}

func TestCompileManifestPreservesExplicitCollaborationPolicy(t *testing.T) {
	no := false
	yes := true
	manifest := &Manifest{
		APIVersion: ManifestAPIVersion, Kind: ManifestKind,
		Metadata: ManifestMetadata{ID: "review", Version: "2.0.0", DisplayName: "Review"},
		Spec: ManifestSpec{
			Roles: []ManifestRoleSlot{{
				ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review",
				ChannelParticipation: RoleChannelObserveOnly,
				SkillGrants: []ManifestRoleSkillGrant{{
					SkillID: "source", SkillVersion: "1.0.0", AllowedActions: []string{"read"},
					MaximumRisk: capability.RiskLevelRead,
				}},
			}},
			Coordination: ManifestCoordinationPolicy{
				MaximumSpeakersPerRound: 5, QuietByDefault: &no,
				RequireRoleRelevance: &no, SuppressDuplicateContent: &no,
			},
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
	if definition.Coordination.QuietByDefault || definition.Coordination.RequireRoleRelevance ||
		definition.Coordination.SuppressDuplicateContent || !definition.Delegation.AllowPeerDelegation ||
		definition.Delegation.RequireAcceptance || definition.Delegation.RequireCompletionReview ||
		definition.SharedContext.AllowMemberWrite || definition.SharedContext.Retention != 24*time.Hour {
		t.Fatalf("explicit policy was not preserved: %#v", definition)
	}
	if definition.Roles[0].SkillGrants[0].RuntimeIdentity != nil || definition.Roles[0].SkillGrants[0].CatalogID != "" {
		t.Fatal("portable Team grant gained a host-owned runtime identity")
	}
}

func TestDecodeManifestYAMLIsStrictAndExcludesPlacement(t *testing.T) {
	data := []byte(`apiVersion: openseal.dev/team/v1alpha1
kind: Team
metadata:
  id: research
  version: 1.0.0
  displayName: Research
spec:
  roles:
    - id: researcher
      displayName: Researcher
      purpose: Collect evidence
      minimumMembers: 1
      requiredDefinitionIds: [researcher]
  sharedContext:
    retention: 48h
`)
	manifest, err := DecodeManifestYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata.ID != "research" || manifest.Spec.Roles[0].RequiredDefinitionIDs[0] != "researcher" {
		t.Fatalf("manifest=%#v", manifest)
	}
	for _, field := range []string{"roster", "credentials", "environment", "deploymentId"} {
		invalid := strings.Replace(string(data), "spec:\n", "spec:\n  "+field+": forbidden\n", 1)
		if _, err = DecodeManifestYAML([]byte(invalid)); err == nil {
			t.Fatalf("host-owned field %s was accepted", field)
		}
	}
	retiredMode := strings.Replace(string(data), "  sharedContext:\n", "  coordination:\n    mode: peer\n  sharedContext:\n", 1)
	if _, err = DecodeManifestYAML([]byte(retiredMode)); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("retired coordination mode was accepted: %v", err)
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
		Spec: ManifestSpec{Roles: []ManifestRoleSlot{{
			ID: "researcher", DisplayName: "Researcher", Purpose: "Research",
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
