package team

import (
	"context"
	"errors"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestRegistryComposesScopedAgentDeploymentsAndActivatesImmutableVersions(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agents := kernelagent.NewRegistry()
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher", Version: "1.0.0", DisplayName: "Researcher", Purpose: "Find evidence", SystemPrompt: "Find and cite evidence.",
		SkillRequirements: []kernelagent.SkillRequirement{{SkillID: "web-research"}},
		Authority:         kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, AllowedSkillIDs: []string{"web-research"}, MaxConcurrentRuns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "researcher-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2},
	}, "user", "operator", "compose Team")
	if err != nil {
		t.Fatal(err)
	}

	registry := NewRegistry(agents)
	definition := validDefinition()
	definition.Roles[0].RequiredSkillIDs = []string{"web-research"}
	registered, err := registry.RegisterDefinition(ctx, definition)
	if err != nil || registered.Digest == "" {
		t.Fatalf("registered definition = %#v, err = %v", registered, err)
	}
	if _, err := registry.RegisterDefinition(ctx, definition); err == nil {
		t.Fatal("Team definition versions must be immutable")
	}
	deployment, activation, err := registry.CreateDeployment(ctx, &Deployment{
		ID: "market-team", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version,
		Roster:       []RosterAssignment{{ID: "primary-researcher", RoleID: "researcher", AgentDeploymentID: agentDeployment.ID}},
		Restrictions: DeploymentRestrictions{MaximumRisk: capability.RiskLevelWrite, MaximumConcurrency: 2}, Status: DeploymentActive,
	}, "user", "operator", "initial Team composition")
	if err != nil || deployment.Revision != 1 || activation.ToVersion != "1.0.0" {
		t.Fatalf("created deployment = %#v, activation = %#v, err = %v", deployment, activation, err)
	}
	if _, err := registry.GetDeployment(ctx, capability.ScopeReference{Kind: "tenant", ID: "other"}, deployment.ID); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("cross-scope deployment lookup err = %v", err)
	}
	proposed := cloneDeployment(deployment)
	proposed.Status = DeploymentPaused
	proposed.Roster[0].DisplayName = "Evidence lead"
	if _, _, err := registry.UpdateDeployment(ctx, proposed, 99, "user", "operator", "pause for review"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale deployment update err = %v", err)
	}
	updatedComposition, compositionActivation, err := registry.UpdateDeployment(ctx, proposed, deployment.Revision, "user", "operator", "pause for review")
	if err != nil || updatedComposition.Status != DeploymentPaused || updatedComposition.Roster[0].DisplayName != "Evidence lead" ||
		updatedComposition.Revision != 2 || compositionActivation.FromVersion != registered.Version || compositionActivation.ToVersion != registered.Version {
		t.Fatalf("updated composition = %#v, activation = %#v, err = %v", updatedComposition, compositionActivation, err)
	}
	deployment = updatedComposition

	next := validDefinition()
	next.Version = "1.1.0"
	next.Roles = append(next.Roles, RoleSlot{ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review evidence"})
	next.OperatingPrinciples = []string{"Preserve evidence provenance"}
	if _, err := registry.RegisterDefinition(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ActivateDefinition(ctx, scope, deployment.ID, next.Version, 99, "user", "operator", "stale"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale activation err = %v", err)
	}
	proposed = cloneDeployment(deployment)
	proposed.ActiveVersion = next.Version
	proposed.Roster = append(proposed.Roster, RosterAssignment{ID: "reviewer", RoleID: "reviewer", AgentDeploymentID: "reviewer-one"})
	reviewerDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "reviewer", Version: "1", DisplayName: "Reviewer", Purpose: "Review evidence", SystemPrompt: "Review evidence.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "reviewer-one", Scope: scope, DefinitionID: reviewerDefinition.ID, ActiveVersion: reviewerDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "reviewer roster"); err != nil {
		t.Fatal(err)
	}
	updated, nextActivation, err := registry.UpdateDeployment(ctx, proposed, deployment.Revision, "user", "operator", "reviewed definition and roster")
	if err != nil || updated.ActiveVersion != "1.1.0" || updated.Revision != 3 || nextActivation.FromVersion != "1.0.0" {
		t.Fatalf("updated = %#v, activation = %#v, err = %v", updated, nextActivation, err)
	}
	activations, err := registry.ListActivations(ctx, scope, deployment.ID)
	if err != nil || len(activations) != 3 || activations[1].DeploymentRevision != 2 || activations[2].DeploymentRevision != 3 {
		t.Fatalf("activations = %#v, err = %v", activations, err)
	}
}

func TestRegistryRejectsMissingOrUnderqualifiedRosterAgentsAndAuthorityWidening(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agents := kernelagent.NewRegistry()
	registry := NewRegistry(agents)
	definition := validDefinition()
	definition.Roles[0].RequiredSkillIDs = []string{"web-research"}
	registered, err := registry.RegisterDefinition(ctx, definition)
	if err != nil {
		t.Fatal(err)
	}
	base := &Deployment{
		ID: "market-team", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version,
		Roster: []RosterAssignment{{ID: "researcher", RoleID: "researcher", AgentDeploymentID: "missing"}}, Status: DeploymentActive,
	}
	if _, _, err := registry.CreateDeployment(ctx, base, "user", "operator", ""); err == nil {
		t.Fatal("missing Agent deployment should fail closed")
	}
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "writer", Version: "1", DisplayName: "Writer", Purpose: "Write summaries", SystemPrompt: "Write concise summaries.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "writer-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "test")
	if err != nil {
		t.Fatal(err)
	}
	base.Roster[0].AgentDeploymentID = agentDeployment.ID
	if _, _, err := registry.CreateDeployment(ctx, base, "user", "operator", ""); err == nil {
		t.Fatal("underqualified Agent deployment should fail closed")
	}
	definition.Version = "1.0.1"
	definition.Roles[0].RequiredSkillIDs = nil
	registered, err = registry.RegisterDefinition(ctx, definition)
	if err != nil {
		t.Fatal(err)
	}
	base.DefinitionID, base.ActiveVersion = registered.ID, registered.Version
	base.Restrictions.MaximumRisk = capability.RiskLevelProduction
	if _, _, err := registry.CreateDeployment(ctx, base, "user", "operator", ""); err == nil {
		t.Fatal("authority widening should fail closed")
	}
}

func TestRegistryGovernsEvaluatesApprovesAndAtomicallyActivatesTeamAmendment(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agents := kernelagent.NewRegistry()
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Find evidence", SystemPrompt: "Find evidence.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "researcher-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "Team roster")
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(agents)
	definition := validDefinition()
	definition.Evaluations = []workforce.EvaluationCriterion{{ID: "coordination", Description: "Coordination remains calm", Required: true}}
	definition.Amendments = workforce.AmendmentPolicy{
		AgentMayPropose: true, AllowedFields: []string{"purpose", "coordination"}, RequiresApproval: true,
		ApproverPrincipals: []string{"user:admin"},
	}
	registered, err := registry.RegisterDefinition(ctx, definition)
	if err != nil {
		t.Fatal(err)
	}
	deployment, _, err := registry.CreateDeployment(ctx, &Deployment{
		ID: "market-team", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version,
		Roster: []RosterAssignment{{ID: "researcher", RoleID: "researcher", AgentDeploymentID: agentDeployment.ID}},
		Status: DeploymentActive,
	}, "user", "admin", "initial Team")
	if err != nil {
		t.Fatal(err)
	}
	candidate := cloneDefinition(registered)
	candidate.Version = "1.1.0"
	candidate.Purpose = "Discover and validate evidence-backed needs"
	amendment, err := registry.ProposeAmendment(ctx, ProposeAmendmentRequest{
		Scope: scope, DeploymentID: deployment.ID, Candidate: candidate, ProposerType: "agent", ProposerID: agentDeployment.ID,
		Rationale: "Validation reduces false conclusions", EvidenceRefs: []string{"artifact:evaluation-1"},
	})
	if err != nil || amendment.Status != AmendmentEvaluating || len(amendment.Changes) != 1 || amendment.Changes[0].Field != "purpose" {
		t.Fatalf("proposed amendment = %#v, err = %v", amendment, err)
	}
	listed, err := registry.ListAmendments(ctx, scope, deployment.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != amendment.ID {
		t.Fatalf("listed amendments = %#v, err = %v", listed, err)
	}
	foreign, err := registry.ListAmendments(ctx, capability.ScopeReference{Kind: "tenant", ID: "other"}, deployment.ID)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("cross-scope amendments = %#v, err = %v", foreign, err)
	}
	evaluated, err := registry.SubmitAmendmentEvaluation(ctx, SubmitAmendmentEvaluationRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision,
		Evaluations: []AmendmentEvaluation{{CriterionID: "coordination", Passed: true, Summary: "No coordination regression", EvidenceRefs: []string{"artifact:eval-result"}}},
	})
	if err != nil || evaluated.Status != AmendmentAwaitingApproval {
		t.Fatalf("evaluated amendment = %#v, err = %v", evaluated, err)
	}
	if _, err := registry.ResolveAmendment(ctx, ResolveAmendmentRequest{Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: evaluated.Revision, Approved: true, ActorType: "user", ActorID: "intruder"}); err == nil {
		t.Fatal("ineligible principal approved Team amendment")
	}
	approved, err := registry.ResolveAmendment(ctx, ResolveAmendmentRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: evaluated.Revision, Approved: true, ActorType: "user", ActorID: "admin", Reason: "reviewed evidence",
	})
	if err != nil || approved.Status != AmendmentApproved {
		t.Fatalf("approved amendment = %#v, err = %v", approved, err)
	}
	activated, updated, activation, err := registry.ActivateAmendment(ctx, scope, amendment.ID, approved.Revision, "user", "admin", "approved Team behavior")
	if err != nil || activated.Status != AmendmentActivated || updated.ActiveVersion != candidate.Version || updated.Revision != 2 || activation.FromVersion != registered.Version {
		t.Fatalf("activated amendment = %#v, deployment = %#v, activation = %#v, err = %v", activated, updated, activation, err)
	}
	stored, err := registry.GetDefinition(ctx, registered.ID, candidate.Version)
	if err != nil || stored.Purpose != candidate.Purpose || stored.Provenance.DerivedFrom != registered.Digest {
		t.Fatalf("stored candidate = %#v, err = %v", stored, err)
	}
}

func TestRegistryRecoversLegacyListeningOnlyTeamThroughAuditedImmutableAmendment(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agents := kernelagent.NewRegistry()
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "reviewer", Version: "1", DisplayName: "Reviewer", Purpose: "Review evidence", SystemPrompt: "Review evidence.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "reviewer-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "legacy Team roster")
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(agents)
	definition := validDefinition()
	definition.Roles[0] = RoleSlot{
		ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review evidence",
		MinimumMembers: 1, MaximumMembers: 1, ChannelParticipation: RoleChannelObserveOnly,
	}
	definition.Approvals.ApproverRoleIDs = []string{"reviewer"}
	// This intentionally represents a legacy definition with no amendment
	// policy. Normal self-amendment must remain unavailable while an authorized
	// operator can still recover the primary channel.
	definition.Amendments = workforce.AmendmentPolicy{}
	registered, err := registry.RegisterDefinition(ctx, definition)
	if err != nil {
		t.Fatal(err)
	}
	deployment, _, err := registry.CreateDeployment(ctx, &Deployment{
		ID: "legacy-review-team", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version,
		Roster: []RosterAssignment{{ID: "reviewer", RoleID: "reviewer", AgentDeploymentID: agentDeployment.ID}},
		Status: DeploymentActive,
	}, "user", "admin", "legacy Team")
	if err != nil {
		t.Fatal(err)
	}
	request := RecoverParticipationRequest{
		Scope: scope, DeploymentID: deployment.ID, BaseVersion: deployment.ActiveVersion,
		ExpectedDeploymentRevision: deployment.Revision, RoleID: "reviewer",
		ActorType: "user", ActorID: "admin", Reason: "Restore one reviewed speaking role", IdempotencyKey: "recover-reviewer-v1",
	}
	agentRequest := request
	agentRequest.ActorType, agentRequest.ActorID = "agent", agentDeployment.ID
	if _, err := registry.RecoverParticipation(ctx, agentRequest); err == nil {
		t.Fatal("Agent must not use the operator recovery path")
	}
	foreign := request
	foreign.Scope.ID = "other"
	if _, err := registry.RecoverParticipation(ctx, foreign); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("cross-scope recovery err = %v", err)
	}
	undeclared := request
	undeclared.RoleID, undeclared.IdempotencyKey = "missing", "recover-missing-role"
	if _, err := registry.RecoverParticipation(ctx, undeclared); err == nil {
		t.Fatal("undeclared recovery role must fail closed")
	}
	recovered, err := registry.RecoverParticipation(ctx, request)
	if err != nil || recovered.Replayed || recovered.Deployment.Revision != 2 ||
		recovered.Deployment.ActiveVersion == registered.Version || recovered.Activation.FromVersion != registered.Version ||
		recovered.Amendment.Status != AmendmentActivated || !recovered.Amendment.RiskWidening ||
		recovered.Amendment.Decision == nil || !recovered.Amendment.Decision.Approved ||
		len(recovered.Amendment.Changes) != 1 || recovered.Amendment.Changes[0].Field != "roles" {
		t.Fatalf("recovery = %#v, err = %v", recovered, err)
	}
	stored, err := registry.GetDefinition(ctx, registered.ID, recovered.Deployment.ActiveVersion)
	if err != nil || stored.Roles[0].ChannelParticipation != RoleChannelActive ||
		stored.Provenance.Source != "openseal-team-participation-recovery" || stored.Provenance.DerivedFrom != registered.Digest ||
		stored.Amendments.AgentMayPropose || stored.Amendments.RequiresApproval || len(stored.Amendments.AllowedFields) != 0 ||
		len(stored.Amendments.ApproverPrincipals) != 0 {
		t.Fatalf("recovered definition = %#v, err = %v", stored, err)
	}
	replay, err := registry.RecoverParticipation(ctx, request)
	if err != nil || !replay.Replayed || replay.Activation.ID != recovered.Activation.ID || replay.Deployment.Revision != 2 {
		t.Fatalf("recovery replay = %#v, err = %v", replay, err)
	}
	conflict := request
	conflict.Reason = "A different reviewed command"
	if _, err := registry.RecoverParticipation(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict err = %v", err)
	}
	stale := request
	stale.IdempotencyKey = "new-stale-command"
	if _, err := registry.RecoverParticipation(ctx, stale); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale recovery err = %v", err)
	}
	alreadyActive := request
	alreadyActive.BaseVersion = recovered.Deployment.ActiveVersion
	alreadyActive.ExpectedDeploymentRevision = recovered.Deployment.Revision
	alreadyActive.IdempotencyKey = "already-active"
	if _, err := registry.RecoverParticipation(ctx, alreadyActive); !errors.Is(err, ErrParticipationRecoveryNotRequired) {
		t.Fatalf("already-active recovery err = %v", err)
	}
}
