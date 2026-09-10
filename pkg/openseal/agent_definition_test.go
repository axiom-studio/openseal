package openseal

import (
	"context"
	"slices"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
)

func TestEngineListAgentDeploymentsFiltersWithinScope(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	definition, err := engine.RegisterAgentDefinition(t.Context(), &AgentDefinition{
		ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate safely", SystemPrompt: "Inspect before acting.",
		Authority: AgentAuthorityPolicy{MaximumRisk: SkillRiskRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := SkillScope{Kind: "tenant", ID: "one"}
	statuses := []AgentRolloutStatus{AgentRolloutPending, AgentRolloutActive, AgentRolloutDegraded, AgentRolloutPaused, AgentRolloutRetired}
	for _, tenant := range []string{"one", "two"} {
		for _, status := range statuses {
			if _, _, err := engine.CreateAgentDeployment(t.Context(), &AgentDeployment{
				ID: string(status), Scope: SkillScope{Kind: "tenant", ID: tenant}, DefinitionID: definition.ID, ActiveVersion: definition.Version,
				RolloutStatus: status, Environment: "production", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
			}, "user", "admin", "test"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, excluded := range [][]AgentRolloutStatus{nil, {AgentRolloutRetired}, {AgentRolloutRetired, AgentRolloutPaused}, statuses, nil} {
		deployments, err := engine.ListAgentDeployments(t.Context(), AgentDeploymentFilter{Scope: scope, ExcludeStatuses: excluded})
		if err != nil || len(deployments) != len(statuses)-len(excluded) {
			t.Fatalf("exclude %v: got %d deployments, err %v", excluded, len(deployments), err)
		}
		for _, deployment := range deployments {
			if deployment.Scope != scope || slices.Contains(excluded, deployment.RolloutStatus) {
				t.Fatalf("exclude %v: unexpected deployment %#v", excluded, deployment)
			}
		}
	}
	for _, filter := range []AgentDeploymentFilter{
		{}, {Scope: SkillScope{Kind: "tenant"}}, {Scope: SkillScope{ID: "one"}},
		{Scope: scope, ExcludeStatuses: []AgentRolloutStatus{"deleted"}},
	} {
		if _, err := engine.ListAgentDeployments(t.Context(), filter); err == nil {
			t.Fatalf("invalid filter accepted: %#v", filter)
		}
	}
	if deployment, err := engine.GetAgentDeployment(t.Context(), scope, "retired"); err != nil || deployment.RolloutStatus != AgentRolloutRetired {
		t.Fatalf("retired deployment inaccessible: %#v, %v", deployment, err)
	}
}

func TestEngineExposesVersionedAgentDefinitionLifecycle(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	definition := func(version string) *AgentDefinition {
		return &AgentDefinition{
			ID: "marketer", Version: version, DisplayName: "Marketer", Purpose: "Turn product work into demand",
			SystemPrompt:      "Create accurate, useful marketing material and follow up respectfully.",
			SkillRequirements: []AgentSkillRequirement{{SkillID: "publisher"}},
			Authority:         AgentAuthorityPolicy{MaximumRisk: SkillRiskExternal, AllowedSkillIDs: []string{"publisher"}, MaxConcurrentRuns: 3},
			Amendments:        AgentAmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"systemPrompt"}, RequiresApproval: true, ApproverPrincipals: []string{"user:admin"}},
		}
	}
	ctx := context.Background()
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if _, err := engine.RegisterAgentDefinition(ctx, definition(version)); err != nil {
			t.Fatal(err)
		}
	}
	scope := SkillScope{Kind: "tenant", ID: "one"}
	deployment, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "marketing-prod:live", Scope: scope, DefinitionID: "marketer", ActiveVersion: "1.0.0",
		RolloutStatus: AgentRolloutActive, Environment: "production", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 2},
	}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	controlChannels, err := engine.ListConversations(ctx, ConversationFilter{
		Scope: Scope{Kind: scope.Kind, ID: scope.ID}, Owner: &ObjectiveOwner{Type: OwnerTypeAgent, ID: deployment.ID},
	})
	if err != nil || len(controlChannels) != 2 {
		t.Fatalf("Agent control channel = %#v, %v", controlChannels, err)
	}
	channelsByOrigin := make(map[ConversationReferenceKind]*Conversation, len(controlChannels))
	for _, conversation := range controlChannels {
		if conversation.Origin != nil {
			channelsByOrigin[conversation.Origin.Kind] = conversation
		}
	}
	control := channelsByOrigin[ConversationReferenceAgentControl]
	if control == nil || control.Title != "Agent control" || control.Origin.ID != agentControlReferenceID(deployment.ID) {
		t.Fatalf("Agent control channel = %#v", control)
	}
	approvals := channelsByOrigin[ConversationReferenceAgentApprovals]
	if approvals == nil || approvals.Title != "Approvals" ||
		approvals.Origin.ID != AgentApprovalsConversationReferenceID(ObjectiveOwner{Type: OwnerTypeAgent, ID: deployment.ID}) {
		t.Fatalf("Agent approvals channel = %#v", approvals)
	}
	if _, err := engine.GetAgentDeployment(ctx, scope, deployment.ID); err != nil {
		t.Fatal(err)
	}
	reconciledChannels, err := engine.ListConversations(ctx, ConversationFilter{
		Scope: Scope{Kind: scope.Kind, ID: scope.ID}, Owner: &ObjectiveOwner{Type: OwnerTypeAgent, ID: deployment.ID},
	})
	if err != nil || len(reconciledChannels) != 2 {
		t.Fatalf("Agent control channel reconciliation duplicated channels: %#v, %v", reconciledChannels, err)
	}
	deployed, activation, err := engine.ActivateAgentDefinition(ctx, scope, deployment.ID, "1.1.0", deployment.Revision, "user", "admin", "evaluation passed")
	if err != nil || deployed.ActiveVersion != "1.1.0" || activation.FromVersion != "1.0.0" {
		t.Fatalf("public rollout = %#v %#v, %v", deployed, activation, err)
	}
	placement := *deployed
	placement.RolloutStatus = AgentRolloutPaused
	placement.Credentials = map[string]SkillCredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "vault://tenant-one/provider"}}
	deployed, placementAudit, err := engine.UpdateAgentDeployment(ctx, &placement, deployed.Revision, "system", "reconciler", "place model provider")
	if err != nil || deployed.RolloutStatus != AgentRolloutPaused || deployed.Credentials["MODEL_PROVIDER"].ID != "vault://tenant-one/provider" ||
		placementAudit.ChangeKind != DeploymentChangeConfigurationUpdated || placementAudit.FromVersion != placementAudit.ToVersion {
		t.Fatalf("public placement update = %#v %#v, %v", deployed, placementAudit, err)
	}
	history, err := engine.ListAgentDefinitionActivations(ctx, scope, deployment.ID)
	if err != nil || len(history) != 3 || history[2].ChangeKind != DeploymentChangeConfigurationUpdated {
		t.Fatalf("public activation history = %#v, %v", history, err)
	}
	compilation, err := engine.RecordAgentDefinitionCompilation(ctx, &AgentDefinitionCompilation{
		ID: "marketing-source-1.1.0", Scope: scope, DeploymentID: deployment.ID, DefinitionID: "marketer", CandidateVersion: "1.1.0",
		Source:       AgentCompilationSource{Kind: "prompt", ID: "marketing-source", Version: "1.1.0", Digest: "sha256:source"},
		TargetDigest: "sha256:marketer-1.1.0", Status: AgentCompilationClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	compilations, err := engine.ListAgentDefinitionCompilations(ctx, scope, deployment.ID)
	if err != nil || len(compilations) != 1 || compilations[0].ID != compilation.ID {
		t.Fatalf("public compilations = %#v, %v", compilations, err)
	}
	candidate := definition("1.2.0")
	candidate.SystemPrompt = "Create accurate marketing, cite evidence, and follow up respectfully."
	amendment, err := engine.ProposeAgentDefinitionAmendment(ctx, ProposeAgentAmendmentRequest{
		Scope: scope, DeploymentID: deployment.ID, Candidate: candidate, ProposerType: "agent", ProposerID: deployment.ID,
		Rationale: "Make evidence requirements explicit.",
	})
	if err != nil || amendment.Status != AgentAmendmentAwaitingApproval {
		t.Fatalf("public amendment proposal = %#v, %v", amendment, err)
	}
	approved, err := engine.ResolveAgentDefinitionAmendment(ctx, ResolveAgentAmendmentRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Approved: true, ActorType: "user", ActorID: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	activated, amendedDeployment, _, err := engine.ActivateAgentDefinitionAmendment(ctx, scope, amendment.ID, approved.Revision, "user", "admin", "approved")
	if err != nil || activated.Status != AgentAmendmentActivated || amendedDeployment.ActiveVersion != "1.2.0" {
		t.Fatalf("public amendment activation = %#v %#v, %v", activated, amendedDeployment, err)
	}
}

func TestAgentStandingAuthorityAllowsOnlyExactReviewedActionScope(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := SkillScope{Kind: "tenant", ID: "one"}
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "reddit-agent", Version: "1.0.0", DisplayName: "Reddit Agent", Purpose: "Contribute useful comments", SystemPrompt: "Be useful.",
		SkillRequirements: []AgentSkillRequirement{{SkillID: "skill-browser", RequiredActions: []string{"camoufox-fill", "camoufox-commit"}}},
		Authority: AgentAuthorityPolicy{
			MaximumRisk: SkillRiskExternal, AllowedSkillIDs: []string{"skill-browser"}, MaxConcurrentRuns: 1, RequireApprovalAt: SkillRiskWrite,
			StandingGrants: []AgentStandingActionGrant{
				{ID: "prepare-comment", SkillID: "skill-browser", Action: "camoufox-fill"},
				{ID: "publish-reddit-comment", SkillID: "skill-browser", Action: "camoufox-commit", ExternalOperation: "comment:create", ResourcePrefix: "https://old.reddit.com/"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "reddit-agent", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "reviewed standing authority")
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-one", Scope: Scope{Kind: scope.Kind, ID: scope.ID}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "reddit-agent"}, AssignedAgentID: "reddit-agent"}
	bound := func(action string) *BoundSkillAction {
		return &BoundSkillAction{
			Definition: &SkillDefinition{ID: "skill-browser", Version: "2.0.6"},
			Action:     SkillAction{Name: action, Risk: SkillRiskExternal, SideEffect: SkillSideEffectExternal},
			Binding:    &SkillBinding{ID: "browser-binding", SkillID: "skill-browser", SkillVersion: "2.0.6"},
		}
	}

	prepare, err := engine.evaluateAgentActionAuthority(ctx, ActionPolicyInput{Run: run, Bound: bound("camoufox-fill")})
	if err != nil || prepare.Disposition != ActionDispositionAllow {
		t.Fatalf("preparatory grant = %#v, %v", prepare, err)
	}
	publish, err := engine.evaluateAgentActionAuthority(ctx, ActionPolicyInput{
		Run: run, Bound: bound("camoufox-commit"),
		ExternalOperation: &ExternalOperationIdentity{Resource: "https://old.reddit.com/r/woodworking/comments/abc/topic/def", Operation: "comment:create"},
	})
	if err != nil || publish.Disposition != ActionDispositionAllow {
		t.Fatalf("publish grant = %#v, %v", publish, err)
	}
	outside, err := engine.evaluateAgentActionAuthority(ctx, ActionPolicyInput{
		Run: run, Bound: bound("camoufox-commit"),
		ExternalOperation: &ExternalOperationIdentity{Resource: "https://example.com/topics/def", Operation: "comment:create"},
	})
	if err != nil || outside.Disposition != ActionDispositionRequireApproval {
		t.Fatalf("outside target = %#v, %v", outside, err)
	}
	wrongOperation, err := engine.evaluateAgentActionAuthority(ctx, ActionPolicyInput{
		Run: run, Bound: bound("camoufox-commit"),
		ExternalOperation: &ExternalOperationIdentity{Resource: "https://old.reddit.com/r/woodworking/comments/abc/topic/def", Operation: "post:delete"},
	})
	if err != nil || wrongOperation.Disposition != ActionDispositionRequireApproval {
		t.Fatalf("wrong operation = %#v, %v", wrongOperation, err)
	}
}

func TestAgentApprovalDeliverySurvivesSharedSkillBinding(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := SkillScope{Kind: "tenant", ID: "one"}
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "reviewer", Version: "1.0.0", DisplayName: "Reviewer", Purpose: "Review governed work", SystemPrompt: "Review carefully.",
		SkillRequirements: []AgentSkillRequirement{{SkillID: "shared-publisher", RequiredActions: []string{"publish"}}},
		Authority: AgentAuthorityPolicy{
			MaximumRisk: SkillRiskExternal, AllowedSkillIDs: []string{"shared-publisher"}, MaxConcurrentRuns: 1,
			ApprovalDestinations: []agent.ApprovalDestination{{EndpointID: "conversation-endpoint:slack-approvals"}},
			StandingGrants:       []AgentStandingActionGrant{{ID: "publish", SkillID: "shared-publisher", Action: "publish"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "reviewer", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "reviewed approval delivery")
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-one", Scope: Scope{Kind: scope.Kind, ID: scope.ID}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "reviewer"}, AssignedAgentID: "reviewer"}
	decision, err := engine.evaluateAgentActionAuthority(ctx, ActionPolicyInput{
		Run: run,
		Bound: &BoundSkillAction{
			Definition: &SkillDefinition{ID: "shared-publisher", Version: "1.0.0"},
			Action:     SkillAction{Name: "publish", Risk: SkillRiskExternal, SideEffect: SkillSideEffectExternal},
			Binding:    &SkillBinding{ID: "shared-binding", DeploymentID: "team:shared", SkillID: "shared-publisher", SkillVersion: "1.0.0"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != ActionDispositionRequireApproval {
		t.Fatalf("shared binding disposition = %s, want require_approval", decision.Disposition)
	}
	if len(decision.ApprovalDestinations) != 1 || decision.ApprovalDestinations[0].EndpointID != "conversation-endpoint:slack-approvals" {
		t.Fatalf("approval destinations = %#v", decision.ApprovalDestinations)
	}
}

func TestAgentApprovalThresholdAllowsPreparationAndReviewsExternalEffects(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := SkillScope{Kind: "tenant", ID: "one"}
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "threshold-agent", Version: "1.0.0", DisplayName: "Threshold Agent", Purpose: "Prepare then publish", SystemPrompt: "Act within authority.",
		SkillRequirements: []AgentSkillRequirement{{SkillID: "browser", RequiredActions: []string{"fill", "commit"}}},
		Authority: AgentAuthorityPolicy{
			MaximumRisk: SkillRiskExternal, AllowedSkillIDs: []string{"browser"}, MaxConcurrentRuns: 1,
			RequireApprovalAt:    SkillRiskExternal,
			ApprovalDestinations: []agent.ApprovalDestination{{EndpointID: "conversation-endpoint:review"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "threshold-agent", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "reviewed threshold"); err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-one", Scope: Scope{Kind: scope.Kind, ID: scope.ID}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "threshold-agent"}, AssignedAgentID: "threshold-agent"}
	bound := func(name string, risk SkillRiskLevel, effect SkillSideEffect) *BoundSkillAction {
		return &BoundSkillAction{
			Definition: &SkillDefinition{ID: "browser", Version: "1"},
			Action:     SkillAction{Name: name, Risk: risk, SideEffect: effect},
			Binding:    &SkillBinding{ID: "browser-binding", DeploymentID: "threshold-agent", SkillID: "browser", SkillVersion: "1"},
		}
	}
	prepare, err := engine.evaluateAgentActionAuthority(ctx, ActionPolicyInput{Run: run, Bound: bound("fill", SkillRiskWrite, SkillSideEffectWrite)})
	if err != nil || prepare.Disposition != ActionDispositionAllow {
		t.Fatalf("prepare decision = %#v, %v", prepare, err)
	}
	publish, err := engine.evaluateAgentActionAuthority(ctx, ActionPolicyInput{Run: run, Bound: bound("commit", SkillRiskExternal, SkillSideEffectExternal)})
	if err != nil || publish.Disposition != ActionDispositionRequireApproval {
		t.Fatalf("publish decision = %#v, %v", publish, err)
	}
	if len(publish.ApprovalDestinations) != 1 || publish.ApprovalDestinations[0].EndpointID != "conversation-endpoint:review" {
		t.Fatalf("publish destinations = %#v", publish.ApprovalDestinations)
	}
}

func TestAgentStandingAuthorityRejectsUndeclaredOrUnboundedGrants(t *testing.T) {
	base := func() *AgentDefinition {
		return &AgentDefinition{
			ID: "agent", Version: "1.0.0", DisplayName: "Agent", Purpose: "Test grants", SystemPrompt: "Act within authority.",
			SkillRequirements: []AgentSkillRequirement{{SkillID: "browser", RequiredActions: []string{"commit"}}},
			Authority:         AgentAuthorityPolicy{MaximumRisk: SkillRiskExternal, MaxConcurrentRuns: 1},
		}
	}
	for _, test := range []struct {
		name  string
		grant AgentStandingActionGrant
	}{
		{name: "undeclared action", grant: AgentStandingActionGrant{ID: "fill", SkillID: "browser", Action: "fill"}},
		{name: "partial external scope", grant: AgentStandingActionGrant{ID: "commit", SkillID: "browser", Action: "commit", ExternalOperation: "comment:create"}},
		{name: "credential-bearing prefix", grant: AgentStandingActionGrant{ID: "commit", SkillID: "browser", Action: "commit", ExternalOperation: "comment:create", ResourcePrefix: "https://user:pass@example.com/"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := base()
			candidate.Authority.StandingGrants = []AgentStandingActionGrant{test.grant}
			if err := candidate.Validate(); err == nil {
				t.Fatal("expected invalid standing grant")
			}
		})
	}
}
