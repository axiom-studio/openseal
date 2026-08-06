package bundle

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/team"
)

func TestWorkforceBundleIsDeterministicSignedAndSecretFree(t *testing.T) {
	bundle := representativeBundle(t)
	firstDigest := bundle.Digest
	if err := bundle.Seal(); err != nil {
		t.Fatal(err)
	}
	if bundle.Digest != firstDigest || !strings.HasPrefix(bundle.Digest, "sha256:") {
		t.Fatalf("bundle digest is not deterministic: %q != %q", bundle.Digest, firstDigest)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := Sign(bundle, "release-key", privateKey); err != nil {
		t.Fatal(err)
	}
	verification, err := Verify(bundle, TrustPolicy{RequireSignature: true, TrustedKeys: map[string]ed25519.PublicKey{"release-key": publicKey}})
	if err != nil || !verification.Trusted || len(verification.ValidSignatureKeys) != 1 {
		t.Fatalf("verification = %#v, %v", verification, err)
	}
	encoded, err := EncodeYAML(bundle)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeYAML(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Digest != bundle.Digest || len(restored.Signatures) != 1 {
		t.Fatalf("YAML round trip changed signed bundle: %#v", restored)
	}
	if _, err := Verify(restored, TrustPolicy{RequireSignature: true, TrustedKeys: map[string]ed25519.PublicKey{"release-key": publicKey}}); err != nil {
		t.Fatal(err)
	}

	tampered, err := clone(bundle)
	if err != nil {
		t.Fatal(err)
	}
	tampered.Objectives[0].Goal = "silently changed"
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("portable-content tampering was accepted: %v", err)
	}
	signatureTampered, _ := clone(bundle)
	value, _ := base64.RawStdEncoding.DecodeString(signatureTampered.Signatures[0].Value)
	value[0] ^= 0xff
	signatureTampered.Signatures[0].Value = base64.RawStdEncoding.EncodeToString(value)
	if _, err := Verify(signatureTampered, TrustPolicy{}); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("signature tampering was accepted: %v", err)
	}

	secret, _ := clone(bundle)
	secret.Signatures = nil
	secret.Objectives[0].Constraints = map[string]interface{}{"api_token": "should-never-export"}
	if err := secret.Seal(); err == nil || !strings.Contains(err.Error(), "secret-shaped") {
		t.Fatalf("secret-shaped Objective content was accepted: %v", err)
	}
}

func TestWorkforceBundleInspectDiffAndUpgradePlan(t *testing.T) {
	current := representativeBundle(t)
	inspection, err := Inspect(current)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Agents != 1 || inspection.Teams != 1 || inspection.Objectives != 1 || inspection.Runbooks != 1 || len(inspection.Owners) != 1 {
		t.Fatalf("inspection omitted resource topology: %#v", inspection)
	}
	target, err := clone(current)
	if err != nil {
		t.Fatal(err)
	}
	target.Metadata.Version = "1.1.0"
	target.Objectives[0].Goal = "Publish reviewed research every weekday"
	if err := target.Seal(); err != nil {
		t.Fatal(err)
	}
	diff, err := Compare(current, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 2 || diff.Changes[0].ResourceType != "metadata" || diff.Changes[1].ResourceType != "objective" || diff.Changes[1].Type != ChangeModified {
		t.Fatalf("unexpected semantic diff: %#v", diff)
	}
	plan, err := PlanUpgrade(current, target)
	if err != nil {
		t.Fatal(err)
	}
	again, err := PlanUpgrade(current, target)
	if err != nil {
		t.Fatal(err)
	}
	if plan.IdempotencyKey != again.IdempotencyKey || plan.RollbackTargetDigest != current.Digest || plan.ToDigest != target.Digest {
		t.Fatalf("upgrade plan is not deterministic or rollback-safe: %#v %#v", plan, again)
	}
	different := representativeBundle(t)
	different.Metadata.ID = "another-workforce"
	if err := different.Seal(); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanUpgrade(current, different); err == nil {
		t.Fatal("cross-identity upgrade was accepted")
	}
}

func TestWorkforceBundleRejectsBrokenReferenceClosure(t *testing.T) {
	bundle := representativeBundle(t)
	bundle.Runbooks[0].AssignedAgentKey = "missing"
	digest, err := contentDigest(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Digest = digest
	if err := bundle.Validate(); err == nil || !strings.Contains(err.Error(), "existing owner, Objective, and assigned Agent") {
		t.Fatalf("broken Runbook reference was accepted: %v", err)
	}
}

func representativeBundle(t *testing.T) *Bundle {
	t.Helper()
	schedule := runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "manual", ObjectiveID: "research", Schedule: &runbook.Schedule{Cron: "0 0 9 * * *", Timezone: "UTC"}}
	definition := &agent.AgentDefinition{
		ID: "source/researcher", Version: "1.0.0", DisplayName: "Researcher", Purpose: "Research product questions",
		SystemPrompt: "Research carefully and preserve citations.", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "daily-research", Version: "1.0.0", Name: "Daily research",
			Entrypoints: map[string]string{"manual": "finish"}, Triggers: map[string]runbook.Trigger{"daily": schedule},
			Steps: map[string]runbook.Step{"finish": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
		},
	}
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	deployment := &agent.AgentDeployment{
		ID: "agent:source-researcher", Scope: capability.ScopeReference{Kind: "tenant", ID: "source"}, DefinitionID: definition.ID,
		ActiveVersion: definition.Version, RolloutStatus: agent.RolloutActive, Environment: "default",
		Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	agentArtifact, err := agent.ExportBundle(agent.BundleExportRequest{
		Definition: definition, Deployment: deployment,
		Metadata: agent.BundleMetadata{ID: "researcher", Version: "1.0.0", DisplayName: "Researcher"},
		Manifest: agent.ManifestMetadata{ID: "researcher", Version: "1.0.0", DisplayName: "Researcher"},
	})
	if err != nil {
		t.Fatal(err)
	}
	teamDefinition := &team.Definition{
		ID: "research-team", Version: "1.0.0", DisplayName: "Research Team", Purpose: "Produce reviewed research",
		Roles:        []team.RoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Gather evidence", MinimumMembers: 1, MaximumMembers: 1, ChannelParticipation: team.RoleChannelActive}},
		Coordination: team.CoordinationPolicy{MaximumSpeakersPerRound: 1, QuietByDefault: true, RequireRoleRelevance: true, SuppressDuplicateContent: true},
		Approvals:    team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	}
	result := New(Metadata{ID: "research-workforce", Version: "1.0.0", DisplayName: "Research Workforce"})
	result.Compatibility.RequiredCapabilities = []CapabilityRequirement{{ID: "agent-runs", Version: 2}, {ID: "objectives", Version: 1}}
	result.Agents = []Agent{{Key: "researcher", Artifact: agentArtifact}}
	result.Teams = []Team{{
		Key: "research-team", Definition: teamDefinition,
		Deployment: TeamDeployment{Roster: []RosterAssignment{{ID: "primary", RoleID: "researcher", AgentKey: "researcher"}}},
	}}
	result.Objectives = []Objective{{
		Key: "research", Owner: OwnerReference{Kind: OwnerTeam, Key: "research-team"}, Title: "Daily research", Goal: "Publish reviewed research",
		Status: runtime.ObjectiveStatusActive, Priority: 10, ExecutionPolicy: &runtime.ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
	}}
	result.Runbooks = []RunbookActivation{{
		Key: "daily-research", Owner: OwnerReference{Kind: OwnerTeam, Key: "research-team"}, ObjectiveKey: "research", AssignedAgentKey: "researcher",
		DefinitionID: "daily-research", DefinitionVersion: "1.0.0", TriggerID: "daily", Trigger: schedule,
		MaximumConcurrent: 1, Status: runtime.RunbookActivationActive,
	}}
	if err := result.Seal(); err != nil {
		t.Fatal(err)
	}
	return result
}
