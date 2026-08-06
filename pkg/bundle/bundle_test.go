package bundle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
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

func TestWorkforceBundlePreviewCompileAndAtomicImportRoundTrip(t *testing.T) {
	artifact := representativeBundle(t)
	placement := representativePlacement(artifact)
	preview, err := PreviewInstallation(artifact, placement)
	if err != nil || !preview.Ready || len(preview.Requirements) != 4 {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	request := InstallationRequest{
		Bundle: artifact, Scope: capability.ScopeReference{Kind: "tenant", ID: "target"}, Placement: placement,
		ActorType: "user", ActorID: "operator", Reason: "Import reviewed workforce", IdempotencyKey: "import-research-v1",
	}
	firstPlan, err := CompileInstallation(request)
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := CompileInstallation(request)
	if err != nil {
		t.Fatal(err)
	}
	if firstPlan.PlanDigest != secondPlan.PlanDigest || len(firstPlan.Agents) != 1 || len(firstPlan.Teams) != 1 || len(firstPlan.Objectives) != 1 || len(firstPlan.Runbooks) != 1 {
		t.Fatalf("installation plan is incomplete or non-deterministic: %#v", firstPlan)
	}
	if firstPlan.Teams[0].Deployment.Roster[0].AgentDeploymentID != "agent:target-researcher" || firstPlan.Runbooks[0].ObjectiveID != "objective:target-research" {
		t.Fatalf("portable references were not resolved to target identities: %#v %#v", firstPlan.Teams[0], firstPlan.Runbooks[0])
	}

	store := &memoryInstallationStore{}
	receipt, err := Install(context.Background(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Install(context.Background(), store, request)
	if err != nil || again.PlanDigest != receipt.PlanDigest || store.applies != 1 {
		t.Fatalf("idempotent import = %#v, applies=%d, err=%v", again, store.applies, err)
	}
	if len(store.agents) != 1 || len(store.teams) != 1 || len(store.objectives) != 1 || len(store.runbooks) != 1 {
		t.Fatalf("empty-store round trip omitted resources: %#v", store)
	}

	failing := &memoryInstallationStore{fail: true}
	if _, err := Install(context.Background(), failing, request); err == nil {
		t.Fatal("target failure was accepted")
	}
	if len(failing.agents)+len(failing.teams)+len(failing.objectives)+len(failing.runbooks) != 0 {
		t.Fatalf("failed atomic import leaked partial resources: %#v", failing)
	}
}

func TestWorkforceBundleInstallationRequiresCompleteUniquePlacement(t *testing.T) {
	artifact := representativeBundle(t)
	placement := representativePlacement(artifact)
	delete(placement.Objectives, "research")
	preview, err := PreviewInstallation(artifact, placement)
	if err != nil || preview.Ready {
		t.Fatalf("incomplete placement preview = %#v, %v", preview, err)
	}
	placement = representativePlacement(artifact)
	placement.Runbooks["daily-research"] = placement.Objectives["research"]
	// Identity uniqueness is per resource kind, so an Objective and Runbook may
	// intentionally share a host-local string. Duplicate identities within the
	// same kind remain rejected.
	duplicate := *artifact
	duplicate.Objectives = append([]Objective(nil), artifact.Objectives...)
	duplicate.Objectives = append(duplicate.Objectives, duplicate.Objectives[0])
	duplicate.Objectives[1].Key = "secondary"
	if err := duplicate.Seal(); err != nil {
		t.Fatal(err)
	}
	placement.Objectives["secondary"] = placement.Objectives["research"]
	if _, err := PreviewInstallation(&duplicate, placement); err == nil || !strings.Contains(err.Error(), "reuses objective identity") {
		t.Fatalf("duplicate target identity was accepted: %v", err)
	}
}

func TestWorkforceBundleInstallationEnforcesTargetTrustPolicy(t *testing.T) {
	artifact := representativeBundle(t)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := Sign(artifact, "release-key", privateKey); err != nil {
		t.Fatal(err)
	}
	request := InstallationRequest{Bundle: artifact, TrustPolicy: TrustPolicy{RequireSignature: true, TrustedKeys: map[string]ed25519.PublicKey{"other-key": publicKey}}, Scope: capability.ScopeReference{Kind: "tenant", ID: "target"}, Placement: representativePlacement(artifact), ActorType: "user", ActorID: "operator", IdempotencyKey: "trusted-import"}
	if _, err := CompileInstallation(request); err == nil || !strings.Contains(err.Error(), "trusted key") {
		t.Fatalf("untrusted installation was accepted: %v", err)
	}
	request.TrustPolicy.TrustedKeys = map[string]ed25519.PublicKey{"release-key": publicKey}
	if _, err := CompileInstallation(request); err != nil {
		t.Fatalf("trusted installation was rejected: %v", err)
	}
}

func representativePlacement(value *Bundle) Placement {
	agentPlacement := agent.BundlePlacement{DeploymentID: "agent:target-researcher", Environment: "default", Runtime: map[string]capability.SkillIdentity{}, Skills: map[string]agent.BundleSkillPlacement{}, Credentials: map[string]capability.CredentialReference{}, Endpoints: map[string]agent.BundleEndpointPlacement{}, Callbacks: map[string]agent.BundleCallbackPlacement{}}
	for _, requirement := range value.Agents[0].Artifact.Runtime {
		agentPlacement.Runtime[requirement.RequirementID] = requirement.Identity
	}
	for _, requirement := range value.Agents[0].Artifact.Skills {
		agentPlacement.Skills[requirement.RequirementID] = agent.BundleSkillPlacement{Identity: requirement.Identity, BindingID: "binding:" + requirement.RequirementID}
	}
	return Placement{
		Agents: map[string]agent.BundlePlacement{"researcher": agentPlacement}, Teams: map[string]TeamPlacement{"research-team": {DeploymentID: "team:target-research"}},
		Objectives: map[string]string{"research": "objective:target-research"}, Runbooks: map[string]string{"daily-research": "activation:target-daily-research"},
	}
}

type memoryInstallationStore struct {
	fail       bool
	applies    int
	receipt    *InstallationReceipt
	agents     []AgentInstallation
	teams      []TeamInstallation
	objectives []*runtime.Objective
	runbooks   []*runtime.RunbookActivation
}

func (s *memoryInstallationStore) ApplyWorkforceBundle(_ context.Context, plan *InstallationPlan) (*InstallationReceipt, error) {
	if s.receipt != nil {
		if s.receipt.IdempotencyKey != plan.IdempotencyKey || s.receipt.PlanDigest != plan.PlanDigest {
			return nil, errors.New("idempotency conflict")
		}
		copy := *s.receipt
		return &copy, nil
	}
	if s.fail {
		return nil, errors.New("simulated transaction failure")
	}
	receipt := &InstallationReceipt{BundleID: plan.BundleID, BundleVersion: plan.BundleVersion, BundleDigest: plan.BundleDigest, PlanDigest: plan.PlanDigest, IdempotencyKey: plan.IdempotencyKey, AppliedAt: time.Unix(2, 0).UTC()}
	// One assignment models the commit point of a transactional target store.
	s.agents, s.teams, s.objectives, s.runbooks = append([]AgentInstallation(nil), plan.Agents...), append([]TeamInstallation(nil), plan.Teams...), append([]*runtime.Objective(nil), plan.Objectives...), append([]*runtime.RunbookActivation(nil), plan.Runbooks...)
	s.receipt, s.applies = receipt, s.applies+1
	copy := *receipt
	return &copy, nil
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
