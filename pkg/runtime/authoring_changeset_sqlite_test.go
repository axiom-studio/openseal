package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestSQLiteWorkforceChangeSetsAreConcurrentRestartSafeAndScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	value := testWorkforceChangeSet(scope, "change-one")
	var created, replayed atomic.Int32
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, replay, err := store.CreateChangeSet(context.Background(), value, "intent-one", "request-one")
			if err != nil || result == nil || result.ID != value.ID {
				t.Errorf("create = %#v, replay = %t, err = %v", result, replay, err)
				return
			}
			if replay {
				replayed.Add(1)
			} else {
				created.Add(1)
			}
		}()
	}
	wait.Wait()
	if created.Load() != 1 || replayed.Load() != 1 {
		t.Fatalf("created = %d, replayed = %d", created.Load(), replayed.Load())
	}
	if _, _, err := store.GetChangeSetByIdempotency(context.Background(), scope, "intent-one", "different"); !errors.Is(err, authoring.ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	if _, err := store.GetChangeSet(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "two"}, value.ID); !errors.Is(err, authoring.ErrChangeSetNotFound) {
		t.Fatalf("cross-scope read = %v", err)
	}
	updated := *value
	updated.Status, updated.Revision = authoring.ChangeSetReady, 2
	updated.UpdatedAt = value.UpdatedAt.Add(time.Minute)
	updated.ApprovalDecisions = []authoring.ChangeSetApprovalDecision{{ID: "decision", EvaluationID: "evaluation", PolicyID: "production", Role: "operator", Approved: true, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, DecidedAt: updated.UpdatedAt}}
	if persisted, err := store.UpdateChangeSet(context.Background(), &updated, 1); err != nil || persisted.Revision != 2 {
		t.Fatalf("update = %#v, err = %v", persisted, err)
	}
	stale := updated
	stale.Status, stale.Revision = authoring.ChangeSetRejected, 3
	if _, err := store.UpdateChangeSet(context.Background(), &stale, 1); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale update = %v", err)
	}
	modifiedCandidate := updated
	modifiedCandidate.CandidateDigest, modifiedCandidate.Revision = "changed", 3
	if _, err := store.UpdateChangeSet(context.Background(), &modifiedCandidate, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("candidate mutation = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetChangeSet(context.Background(), scope, value.ID)
	if err != nil || restored.CandidateDigest != value.CandidateDigest || restored.Status != authoring.ChangeSetReady || restored.Revision != 2 || len(restored.ApprovalDecisions) != 1 || restored.ApprovalDecisions[0].ID != "decision" {
		t.Fatalf("restored = %#v, err = %v", restored, err)
	}
}

func TestSQLiteAtomicWorkforceApplyPersistsWholeAggregateAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status = authoring.ChangeSetApplied
	applied.Revision = 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: value.CandidateDigest, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(context.Background(), applied, 2)
	if err != nil || result.Status != authoring.ChangeSetApplied || len(result.ApplyReceipt.Resources) != 6 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err = store.GetDefinition(context.Background(), "agent", "1"); err != nil {
		t.Fatal(err)
	}
	if deployment, err := store.GetDeployment(context.Background(), value.Scope, "agent-live"); err != nil || deployment.ActiveVersion != "1" {
		t.Fatalf("deployment=%#v err=%v", deployment, err)
	}
	if teamDeployment, err := store.GetTeamDeployment(context.Background(), value.Scope, "team-live"); err != nil || len(teamDeployment.Roster) != 1 {
		t.Fatalf("team=%#v err=%v", teamDeployment, err)
	}
	objectives, err := store.ListObjectives(context.Background(), ObjectiveFilter{Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}})
	if err != nil || len(objectives) != 2 {
		t.Fatalf("objectives=%d err=%v", len(objectives), err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetChangeSet(context.Background(), value.Scope, value.ID)
	if err != nil || restored.ApplyReceipt == nil || restored.ApplyReceipt.ID != "receipt" {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
}

func TestSQLiteWorkforceApplyAcceptsCanonicalAgentEventAssignment(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	value := testApplicableWorkforceChangeSet()
	canonicalAgentID := "tenant/one/agent"
	value.Result.Candidate.Agents[0].ID = canonicalAgentID
	value.Result.Candidate.Agents[0].ObjectiveTemplates[0].EventRules = map[string]interface{}{
		"version": "1",
		"rules": []interface{}{map[string]interface{}{
			"id": "warning", "eventType": "kubernetes.warning", "source": "kubernetes:cluster:1",
			"attributes": map[string]interface{}{"namespace": "operations"}, "assignedAgentId": canonicalAgentID,
		}},
	}
	value.Result.Candidate.Team.ObjectiveTemplates[0].EventRules = map[string]interface{}{
		"version": "1",
		"rules": []interface{}{map[string]interface{}{
			"id": "team-warning", "eventType": "kubernetes.warning", "source": "kubernetes:cluster:1",
			"attributes": map[string]interface{}{"namespace": "operations"}, "assignedAgentId": canonicalAgentID,
		}},
	}
	value.Result.Candidate.Team.Roles[0].RequiredDefinitionIDs = []string{canonicalAgentID}
	value.Result.Candidate.Assignments[0].AgentDefinitionID = canonicalAgentID
	value.Placement.AgentDeploymentIDs = map[string]string{canonicalAgentID: "agent-live"}
	delete(value.Placement.Objectives, authoring.WorkforceObjectiveKey("agent", "agent", "agent-goal"))
	value.Placement.Objectives[authoring.WorkforceObjectiveKey("agent", canonicalAgentID, "agent-goal")] = authoring.ObjectivePlacement{ID: "objective:agent"}
	if _, _, err = store.CreateChangeSet(context.Background(), value, "canonical-event", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := appliedRuntimeChangeSet(value, "receipt", "apply", value.UpdatedAt.Add(time.Minute))
	if _, err = store.ApplyChangeSet(context.Background(), applied, 2); err != nil {
		t.Fatal(err)
	}
	objective, err := store.GetObjective(context.Background(), Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, "objective:agent")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := DecodeObjectiveEventRules(objective.EventRules)
	if err != nil || len(rules.Rules) != 1 || rules.Rules[0].AssignedAgentID != "agent-live" {
		t.Fatalf("deployed event assignment = %#v, %v", rules, err)
	}
	if reviewed := value.Result.Candidate.Agents[0].ObjectiveTemplates[0].EventRules["rules"].([]interface{})[0].(map[string]interface{})["assignedAgentId"]; reviewed != canonicalAgentID {
		t.Fatalf("reviewed candidate assignment mutated to %#v", reviewed)
	}
	teamObjective, err := store.GetObjective(context.Background(), Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, "objective:team")
	if err != nil {
		t.Fatal(err)
	}
	teamRules, err := DecodeObjectiveEventRules(teamObjective.EventRules)
	if err != nil || len(teamRules.Rules) != 1 || teamRules.Rules[0].AssignedAgentID != "agent-live" {
		t.Fatalf("deployed Team event assignment = %#v, %v", teamRules, err)
	}
}

func TestMaterializeObjectiveEventRulesTranslatesInitiativeContext(t *testing.T) {
	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Initiative = &authoring.InitiativeBlueprint{ID: "research-program"}
	value.Placement.InitiativeID = "initiative:live"
	template := workforce.ObjectiveTemplate{ID: "monitor", EventRules: map[string]interface{}{
		"version": "1",
		"rules": []interface{}{map[string]interface{}{
			"id": "observation", "eventType": "source.observed", "assignedAgentId": "agent",
			"runTemplate": map[string]interface{}{"context": map[string]interface{}{"initiativeId": "research-program"}},
		}},
	}}
	materialized, err := materializeObjectiveEventRules(value, template, map[string]string{"agent": "agent-live"})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := DecodeObjectiveEventRules(materialized)
	if err != nil || len(rules.Rules) != 1 {
		t.Fatalf("materialized rules = %#v, %v", rules, err)
	}
	if rules.Rules[0].AssignedAgentID != "agent-live" || rules.Rules[0].RunTemplate.Context["initiativeId"] != "initiative:live" {
		t.Fatalf("materialized event rule = %#v", rules.Rules[0])
	}
	original := template.EventRules["rules"].([]interface{})[0].(map[string]interface{})
	if original["assignedAgentId"] != "agent" || original["runTemplate"].(map[string]interface{})["context"].(map[string]interface{})["initiativeId"] != "research-program" {
		t.Fatalf("reviewed event template mutated: %#v", original)
	}
}

func TestSQLiteWorkforceApplyMaterializesExecutableSkillBindings(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(context.Background(), &skill.Definition{
		ID: "research", Version: "1.0.0", Name: "Research", Prompt: &skill.PromptModule{Instructions: "Preserve cited evidence."},
		BindingConfigSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"sourceId"}, "properties": map[string]interface{}{"sourceId": map[string]interface{}{"type": "integer", "minimum": 1}}},
	}); err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "research", PromptRequired: true}}
	value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"research"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"research": {ID: "research", Version: "1.0.0", PromptAvailable: true, BindingConfigSchema: map[string]interface{}{"type": "object"}},
	}}
	value.Placement.BindingConfigs = map[string]map[string]map[string]interface{}{"agent": {"research": {"sourceId": 17}}}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: value.CandidateDigest, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(context.Background(), applied, 2)
	if err != nil {
		t.Fatal(err)
	}
	prompts, err := catalog.ListModelPrompts(context.Background(), value.Scope, "agent-live")
	if err != nil || len(prompts) != 1 || prompts[0].SkillID != "research" {
		t.Fatalf("prompts=%#v error=%v", prompts, err)
	}
	bindings, err := store.ListSkillBindings(context.Background(), value.Scope, "agent-live")
	if err != nil || len(bindings) != 1 || bindings[0].Config["sourceId"] != float64(17) {
		t.Fatalf("binding config=%#v error=%v", bindings, err)
	}
	found := false
	for _, resource := range result.ApplyReceipt.Resources {
		found = found || resource.Kind == "skill_binding" && resource.ID == "workforce:agent-live:research"
	}
	if !found {
		t.Fatalf("receipt resources=%#v", result.ApplyReceipt.Resources)
	}
}

func TestWorkforceAuthoringRejectsSkillRequirementsWithoutAuthorityBeforeApply(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "skill-slack", VersionConstraint: "1.0.0"}}
	value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"skill-slack"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"skill-slack": {
			ID: "skill-slack", Version: "1.0.0", Actions: []string{"slack-send-message"},
			MaximumRisk: capability.RiskLevelExternal,
		},
	}}
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{
		"agent": {"skill-slack": capability.NewSkillIdentity("skill-slack", "1.0.0", "")},
	}

	issues, err := store.ValidateChangeSetReadiness(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Code != "skill_binding_materialization_failed" ||
		!strings.Contains(issues[0].Message, "must enable a prompt or explicitly allow actions") {
		t.Fatalf("readiness issues = %#v", issues)
	}
	if _, err := materializeWorkforceSkillBindings(value, value.Result.Candidate.Agents[0], "agent-live", false); err == nil {
		t.Fatal("empty Skill authority materialized")
	}
}

func TestInactiveWorkforceBindingDefersExactExecutionCredentialUntilActivation(t *testing.T) {
	value := testApplicableWorkforceChangeSet()
	definition := value.Result.Candidate.Agents[0]
	definition.SkillRequirements = []agent.SkillRequirement{{
		SkillID: "posture", VersionConstraint: "1.0.0", RequiredActions: []string{"execute"},
	}}
	definition.Authority.AllowedSkillIDs = []string{"posture"}
	definition.Authority.MaximumRisk = capability.RiskLevelExternal
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"posture": {
			ID: "posture", Version: "1.0.0", Actions: []string{"execute"},
			MaximumRisk: capability.RiskLevelExternal,
			Credentials: []authoring.SkillCredential{{
				Name: "TOOLWEB_API_KEY", Kind: "environment-secret", Actions: []string{"execute"},
			}},
		},
	}}
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{
		definition.ID: {"posture": capability.NewSkillIdentity("posture", "1.0.0", "")},
	}

	inactive, err := materializeWorkforceSkillBindings(value, definition, "agent-live", false)
	if err != nil || len(inactive) != 1 || !inactive[0].Disabled || len(inactive[0].Credentials) != 0 {
		t.Fatalf("inactive deferred binding=%#v error=%v", inactive, err)
	}
	if _, err = materializeWorkforceSkillBindings(value, definition, "agent-live", true); err == nil ||
		!strings.Contains(err.Error(), "TOOLWEB_API_KEY") {
		t.Fatalf("active missing exact credential error=%v", err)
	}

	reference := capability.CredentialReference{Kind: "environment-secret", ID: "credential://toolweb"}
	value.Placement.CredentialReferences = map[string]map[string]capability.CredentialReference{
		definition.ID: {"TOOLWEB_API_KEY": reference},
	}
	active, err := materializeWorkforceSkillBindings(value, definition, "agent-live", true)
	if err != nil || len(active) != 1 || active[0].Disabled ||
		active[0].Credentials["TOOLWEB_API_KEY"] != reference {
		t.Fatalf("active exact credential binding=%#v error=%v", active, err)
	}
}

func TestWorkforceReadinessAcceptsOnlyExactReviewedSkillInstallation(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	value := testApplicableWorkforceChangeSet()
	definition := value.Result.Candidate.Agents[0]
	definition.SkillRequirements = []agent.SkillRequirement{{
		SkillID: "posture", VersionConstraint: "1.0.0", RequiredActions: []string{"execute"},
	}}
	definition.Authority.AllowedSkillIDs = []string{"posture"}
	definition.Authority.MaximumRisk = capability.RiskLevelExternal
	const (
		source         = "https://clawhub.ai::posture"
		runtimeVersion = "1.0.0+source.abc"
		reference      = "listing:42"
	)
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"posture": {
			ID: "posture", Version: "1.0.0", SourceIdentity: source,
			Actions: []string{"execute"}, MaximumRisk: capability.RiskLevelExternal,
			Readiness: authoring.SkillReadinessNeedsInstallation,
			Compatibility: []authoring.SkillCompatibility{{
				Requirement: "installation", Compatible: false, Reference: reference,
			}},
		},
	}}
	identity := capability.NewSkillIdentity("posture", runtimeVersion, source)
	value.Placement.SkillSourceIdentities = map[string]map[string]string{
		definition.ID: {"posture": source},
	}
	value.Placement.SkillSourceVersions = map[string]map[string]string{
		definition.ID: {"posture": runtimeVersion},
	}
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{
		definition.ID: {"posture": identity},
	}
	value.Placement.PlannedSkillInstallations = []authoring.SkillInstallationIntent{{
		SkillID: "posture", Version: "1.0.0", SourceIdentity: source, Reference: reference,
	}}

	issues, err := store.ValidateChangeSetReadiness(context.Background(), value)
	if err != nil || len(issues) != 0 {
		t.Fatalf("exact reviewed installation readiness=%#v error=%v", issues, err)
	}
	value.Placement.PlannedSkillInstallations[0].Reference = "listing:forged"
	issues, err = store.ValidateChangeSetReadiness(context.Background(), value)
	if err != nil || len(issues) != 1 || issues[0].Code != "skill_binding_definition_unavailable" {
		t.Fatalf("forged installation readiness=%#v error=%v", issues, err)
	}
}

func TestSkillBindingStoresRejectMalformedAuthority(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	binding := &skill.Binding{
		ID: "invalid", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent",
		SkillID: "skill-slack", SkillVersion: "1.0.0", MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}
	if err := store.SaveSkillBinding(context.Background(), binding, 0); err == nil ||
		!strings.Contains(err.Error(), "must enable a prompt or explicitly allow actions") {
		t.Fatalf("save malformed binding = %v", err)
	}
}

func TestSQLiteWorkforceEvaluationValidatesExactSkillAuthorityBeforeReady(t *testing.T) {
	for _, test := range []struct {
		name        string
		definition  *skill.Definition
		catalogID   string
		action      string
		catalogRisk capability.RiskLevel
		credentials []authoring.SkillCredential
		references  map[string]capability.CredentialReference
		wantStatus  authoring.ChangeSetStatus
		wantCode    string
	}{
		{
			name:      "external summarize action is blocked by read-only Agent authority",
			catalogID: "clawhub-summarize", action: "execute", catalogRisk: capability.RiskLevelExternal,
			definition: &skill.Definition{
				ID: "summarize", Version: "1.0.0+source.aaaa", Name: "Summarize",
				Source:    &skill.SourceProvenance{Identity: "https://clawhub.ai::@alice/summarize", Format: "openclaw.skill.v1"},
				Transport: skill.TransportReference{Kind: "tool", Endpoint: "summarize"},
				Actions:   map[string]skill.Action{"execute": {Name: "execute", Description: "Summarize evidence", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, InputSchema: map[string]interface{}{"type": "object"}, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported}},
			},
			wantStatus: authoring.ChangeSetBlocked, wantCode: "skill_binding_action_risk_exceeded",
		},
		{
			name:      "read-only Reddit action reaches ready",
			catalogID: "clawhub-reddit", action: "read", catalogRisk: capability.RiskLevelRead,
			definition: &skill.Definition{
				ID: "reddit.reader", Version: "2.0.0+source.bbbb", Name: "Reddit Reader",
				Source:    &skill.SourceProvenance{Identity: "https://clawhub.ai::@alice/reddit", Format: "openclaw.skill.v1"},
				Transport: skill.TransportReference{Kind: "tool", Endpoint: "reddit_read"},
				Actions:   map[string]skill.Action{"read": {Name: "read", Description: "Read Reddit posts", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, InputSchema: map[string]interface{}{"type": "object"}, Credentials: []skill.CredentialRequirement{{Name: "reddit", Kind: "reddit-oauth"}}, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported}},
			},
			credentials: []authoring.SkillCredential{{Name: "reddit", Kind: "reddit-oauth", Actions: []string{"read"}}},
			references:  map[string]capability.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://tenant/reddit"}},
			wantStatus:  authoring.ChangeSetReady,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := skill.NewCatalogWithStore(store).Register(ctx, test.definition); err != nil {
				t.Fatal(err)
			}
			value := testApplicableWorkforceChangeSet()
			value.Status, value.Revision = authoring.ChangeSetReview, 1
			value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: test.catalogID, VersionConstraint: "1.0.0", RequiredActions: []string{test.action}}}
			value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{test.catalogID}
			value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
				test.catalogID: {ID: test.catalogID, Version: "1.0.0", Actions: []string{test.action}, MaximumRisk: test.catalogRisk, Credentials: test.credentials},
			}}
			identity := capability.NewSkillIdentity(test.definition.ID, test.definition.Version, test.definition.Source.Identity)
			value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{"agent": {test.catalogID: identity}}
			if test.references != nil {
				value.Placement.CredentialReferences = map[string]map[string]capability.CredentialReference{"agent": test.references}
			}
			if _, _, err := store.CreateChangeSet(ctx, value, "create-"+test.name, "digest"); err != nil {
				t.Fatal(err)
			}
			compiler, err := authoring.NewCompiler(&countedWorkforceGenerator{})
			if err != nil {
				t.Fatal(err)
			}
			service, err := authoring.NewChangeSetService(compiler, store)
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := service.SubmitEvaluation(ctx, authoring.SubmitChangeSetEvaluationRequest{
				Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: value.Revision, CandidateDigest: value.CandidateDigest,
				Allowed: true, Actor: authoring.ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow",
			})
			if err != nil || result.Status != test.wantStatus {
				t.Fatalf("evaluation = %#v, err = %v", result, err)
			}
			if test.wantCode == "" {
				if len(result.Result.Validation) != 0 || !result.Result.Valid {
					t.Fatalf("valid Reddit readiness = %#v", result.Result)
				}
				return
			}
			if len(result.Result.Validation) != 1 || result.Result.Validation[0].Code != test.wantCode ||
				!strings.Contains(result.Result.Validation[0].Message, "requires external risk") || result.Result.Valid ||
				result.Lifecycle[len(result.Lifecycle)-1].Reason != "binding_validation_failed" {
				t.Fatalf("blocked readiness = %#v", result)
			}
		})
	}
}

func TestSQLiteWorkforceApplyRequiresExactSourceForCollidingSkills(t *testing.T) {
	for _, test := range []struct {
		name           string
		sourceIdentity string
		wantAmbiguous  bool
	}{
		{name: "exact", sourceIdentity: "clawhub::@alice/research"},
		{name: "ambiguous", wantAmbiguous: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			catalog := skill.NewCatalogWithStore(store)
			for _, identity := range []string{"clawhub::@alice/research", "clawhub::@bob/research"} {
				if err := catalog.Register(ctx, &skill.Definition{
					ID: "research", Version: "1.0.0", Name: "Research",
					Source: &skill.SourceProvenance{Identity: identity, Format: "openclaw.skill.v1"},
					Prompt: &skill.PromptModule{Instructions: "Preserve cited evidence."},
				}); err != nil {
					t.Fatal(err)
				}
			}
			value := testApplicableWorkforceChangeSet()
			value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "research", PromptRequired: true}}
			value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"research"}
			value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
				"research": {ID: "research", Version: "1.0.0", PromptAvailable: true},
			}}
			if test.sourceIdentity != "" {
				value.Placement.SkillSourceIdentities = map[string]map[string]string{"agent": {"research": test.sourceIdentity}}
				value.Placement.SkillSourceVersions = map[string]map[string]string{"agent": {"research": "1.0.0"}}
			}
			if _, _, err := store.CreateChangeSet(ctx, value, "create", "digest"); err != nil {
				t.Fatal(err)
			}
			applied := appliedRuntimeChangeSet(value, "receipt", "apply", value.UpdatedAt.Add(time.Minute))
			result, err := store.ApplyChangeSet(ctx, applied, 2)
			if test.wantAmbiguous {
				if !errors.Is(err, skill.ErrDefinitionAmbiguous) {
					t.Fatalf("ambiguous apply = %#v, %v", result, err)
				}
				if _, err := store.GetDefinition(ctx, "agent", "1"); !errors.Is(err, agent.ErrDefinitionNotFound) {
					t.Fatalf("ambiguous apply leaked Agent state: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			bindings, err := store.ListSkillBindings(ctx, value.Scope, "agent-live")
			if err != nil || len(bindings) != 1 || bindings[0].SourceIdentity != test.sourceIdentity {
				t.Fatalf("exact workforce binding = %#v, %v", bindings, err)
			}
			prompts, err := catalog.ListModelPrompts(ctx, value.Scope, "agent-live")
			if err != nil || len(prompts) != 1 || prompts[0].BindingID != bindings[0].ID {
				t.Fatalf("exact model prompts = %#v, %v", prompts, err)
			}
			encoded, _ := json.Marshal(prompts)
			if strings.Contains(string(encoded), test.sourceIdentity) {
				t.Fatalf("model prompt leaked source identity: %s", encoded)
			}
		})
	}
}

func TestSQLiteWorkforceReadinessBlocksExistingAgentDeploymentIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	occupier := testApplicableWorkforceChangeSet()
	occupier.ID = "occupier"
	if _, _, err = store.CreateChangeSet(ctx, occupier, "occupier", "occupier"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApplyChangeSet(
		ctx,
		appliedRuntimeChangeSet(occupier, "occupier-receipt", "occupier-apply", occupier.UpdatedAt.Add(time.Minute)),
		occupier.Revision,
	); err != nil {
		t.Fatal(err)
	}

	candidate := testAgentDeploymentIdentityCollisionChangeSet("collision-review")
	candidate.Status, candidate.Revision = authoring.ChangeSetReview, 1
	if _, _, err = store.CreateChangeSet(ctx, candidate, "collision-review", "collision-review"); err != nil {
		t.Fatal(err)
	}
	compiler, err := authoring.NewCompiler(&countedWorkforceGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := authoring.NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := service.SubmitEvaluation(ctx, authoring.SubmitChangeSetEvaluationRequest{
		Scope: candidate.Scope, ChangeSetID: candidate.ID, ExpectedRevision: candidate.Revision,
		CandidateDigest: candidate.CandidateDigest, Allowed: true,
		Actor: authoring.ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow-collision",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != authoring.ChangeSetBlocked || result.Result.Valid || len(result.Result.Validation) != 1 {
		t.Fatalf("colliding readiness result = %#v", result)
	}
	issue := result.Result.Validation[0]
	if issue.Code != "agent_deployment_identity_conflict" ||
		issue.Path != "placement.agentDeploymentIds.agent-collision" ||
		!strings.Contains(issue.Message, `"Analyst Agent"`) ||
		!strings.Contains(issue.Message, `"agent-live"`) ||
		!strings.Contains(issue.Message, "amend the existing Agent") {
		t.Fatalf("colliding readiness issue = %#v", issue)
	}
}

func TestSQLiteAtomicWorkforceApplyMapsAgentDeploymentIdentityRaceWithoutPartialState(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	candidate := testAgentDeploymentIdentityCollisionChangeSet("collision-race")
	if _, _, err = store.CreateChangeSet(ctx, candidate, "collision-race", "collision-race"); err != nil {
		t.Fatal(err)
	}
	occupier := testApplicableWorkforceChangeSet()
	occupier.ID = "race-occupier"
	if _, _, err = store.CreateChangeSet(ctx, occupier, "race-occupier", "race-occupier"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApplyChangeSet(
		ctx,
		appliedRuntimeChangeSet(occupier, "occupier-receipt", "occupier-apply", occupier.UpdatedAt.Add(time.Minute)),
		occupier.Revision,
	); err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyChangeSet(
		ctx,
		appliedRuntimeChangeSet(candidate, "collision-receipt", "collision-apply", candidate.UpdatedAt.Add(2*time.Minute)),
		candidate.Revision,
	)
	if !errors.Is(err, authoring.ErrChangeSetPlacementConflict) {
		t.Fatalf("colliding apply error = %v", err)
	}
	if message := strings.ToLower(err.Error()); strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "agent_deployments_pkey") ||
		strings.Contains(message, "sql") {
		t.Fatalf("colliding apply leaked storage detail: %v", err)
	}
	if _, err = store.GetDefinition(ctx, "agent-collision", "1"); !errors.Is(err, agent.ErrDefinitionNotFound) {
		t.Fatalf("colliding apply leaked Agent definition: %v", err)
	}
	if _, err = store.GetTeamDefinition(ctx, "team-collision-race", "1"); !errors.Is(err, team.ErrDefinitionNotFound) {
		t.Fatalf("colliding apply leaked Team definition: %v", err)
	}
	current, err := store.GetChangeSet(ctx, candidate.Scope, candidate.ID)
	if err != nil || current.Status != authoring.ChangeSetReady || current.ApplyReceipt != nil {
		t.Fatalf("colliding ChangeSet = %#v, err = %v", current, err)
	}
}

func TestSQLiteWorkforceApplyBindsImmutableSourceVersionBehindDeclaredContract(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	const (
		identity         = "https://clawhub.ai::@alice/research"
		immutableVersion = "1.0.0+source.0123456789ab"
	)
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "research", Version: immutableVersion, Name: "Research",
		Source: &skill.SourceProvenance{Identity: identity, Format: "openclaw.skill.v1", ResolvedVersion: "1.0.0"},
		Prompt: &skill.PromptModule{Instructions: "Preserve immutable evidence."},
	}); err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "research", VersionConstraint: "1.0.0", PromptRequired: true}}
	value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"research"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"research": {ID: "research", Version: "1.0.0", PromptAvailable: true},
	}}
	value.Placement.SkillSourceIdentities = map[string]map[string]string{"agent": {"research": identity}}
	value.Placement.SkillSourceVersions = map[string]map[string]string{"agent": {"research": immutableVersion}}
	if _, _, err := store.CreateChangeSet(ctx, value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := appliedRuntimeChangeSet(value, "receipt", "apply", value.UpdatedAt.Add(time.Minute))
	if _, err := store.ApplyChangeSet(ctx, applied, 2); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListSkillBindings(ctx, value.Scope, "agent-live")
	if err != nil || len(bindings) != 1 || bindings[0].SkillVersion != immutableVersion || bindings[0].SourceIdentity != identity {
		t.Fatalf("immutable source binding = %#v, %v", bindings, err)
	}
	prompts, err := catalog.ListModelPrompts(ctx, value.Scope, "agent-live")
	if err != nil || len(prompts) != 1 || prompts[0].Version != immutableVersion {
		t.Fatalf("immutable source prompt = %#v, %v", prompts, err)
	}
}

func TestSQLiteWorkforceApplyResolvesCatalogAliasIntoExactTeamGrantAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const (
		catalogID        = "clawhub-aHR0cHM6Ly9jbGF3aHViLmFpL0BhbGljZS9zdW1tYXJpemU"
		definitionID     = "summarize"
		immutableVersion = "1.0.0+source.0123456789ab"
		sourceIdentity   = "https://clawhub.ai::@alice/summarize"
	)
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(ctx, &skill.Definition{
		ID: definitionID, Version: immutableVersion, Name: "Summarize",
		Source:    &skill.SourceProvenance{Identity: sourceIdentity, Format: "openclaw.skill.v1", ResolvedVersion: "1.0.0"},
		Prompt:    &skill.PromptModule{Instructions: "Summarize evidence without changing it."},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: "process"},
		Actions: map[string]skill.Action{"execute": {
			Name: "execute", Description: "Summarize evidence", InputSchema: map[string]interface{}{"type": "object"},
			Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	agentDefinition := value.Result.Candidate.Agents[0]
	agentDefinition.SkillRequirements = []agent.SkillRequirement{{SkillID: catalogID, VersionConstraint: "1.0.0", RequiredActions: []string{"execute"}}}
	agentDefinition.Authority.AllowedSkillIDs = []string{catalogID}
	value.Result.Candidate.Team.Roles[0].RequiredSkillIDs = []string{catalogID}
	value.Result.Candidate.Team.Roles[0].SkillGrants = []team.RoleSkillGrant{{
		SkillID: catalogID, SkillVersion: "1.0.0", AllowedActions: []string{"execute"}, EnablePrompt: true, MaximumRisk: capability.RiskLevelRead,
	}}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		catalogID: {ID: catalogID, Version: "1.0.0", Actions: []string{"execute"}, PromptAvailable: true, MaximumRisk: capability.RiskLevelRead},
	}}
	identity := capability.NewSkillIdentity(definitionID, immutableVersion, sourceIdentity)
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{"agent": {catalogID: identity}}
	if _, _, err := store.CreateChangeSet(ctx, value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := appliedRuntimeChangeSet(value, "receipt", "apply", value.UpdatedAt.Add(time.Minute))
	if _, err := store.ApplyChangeSet(ctx, applied, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	agents := agent.NewRegistryWithStore(restored)
	teams := team.NewRegistryWithStore(restored, agents)
	storedAgent, err := agents.GetDefinition(ctx, "agent", "1")
	if err != nil || len(storedAgent.SkillRequirements) != 1 || storedAgent.SkillRequirements[0].SkillID != definitionID ||
		len(storedAgent.Authority.AllowedSkillIDs) != 1 || storedAgent.Authority.AllowedSkillIDs[0] != definitionID {
		t.Fatalf("resolved Agent definition = %#v, %v", storedAgent, err)
	}
	storedTeam, err := teams.GetDefinition(ctx, "team", "1")
	if err != nil || len(storedTeam.Roles[0].SkillGrants) != 1 {
		t.Fatalf("resolved Team definition = %#v, %v", storedTeam, err)
	}
	grant := storedTeam.Roles[0].SkillGrants[0]
	if grant.CatalogID != catalogID || !grant.ExactIdentity().Equal(identity) || storedTeam.Roles[0].RequiredSkillIDs[0] != definitionID {
		t.Fatalf("exact persisted Team grant = %#v", grant)
	}
	bindings, err := restored.ListSkillBindings(ctx, value.Scope, "agent-live")
	if err != nil || len(bindings) != 1 || bindings[0].EnablePrompt || !capability.NewSkillIdentity(bindings[0].SkillID, bindings[0].SkillVersion, bindings[0].SourceIdentity).Equal(identity) {
		t.Fatalf("exact persisted binding = %#v, %v", bindings, err)
	}
	teamBindings, err := restored.ListSkillBindings(ctx, value.Scope, "team-live")
	if err != nil || len(teamBindings) != 1 || teamBindings[0].DeploymentID != "team-live" ||
		!capability.NewSkillIdentity(teamBindings[0].SkillID, teamBindings[0].SkillVersion, teamBindings[0].SourceIdentity).Equal(identity) ||
		len(teamBindings[0].AllowedActions) != 1 || teamBindings[0].AllowedActions[0] != "execute" || !teamBindings[0].EnablePrompt {
		t.Fatalf("exact persisted Team binding = %#v, %v", teamBindings, err)
	}
	foundTeamBinding := false
	for _, resource := range applied.ApplyReceipt.Resources {
		foundTeamBinding = foundTeamBinding || resource.Kind == "skill_binding" && resource.ID == "workforce:team-live:"+catalogID
	}
	if !foundTeamBinding {
		t.Fatalf("Team binding missing from apply receipt: %#v", applied.ApplyReceipt.Resources)
	}
}

func TestSQLiteAtomicWorkforceApplyMaterializesInitiativeAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	registerInitiativeSourceSkill(t, store)
	value := testInitiativeWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, value, "create-initiative", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-initiative", IdempotencyKey: "apply-initiative", CandidateDigest: value.CandidateDigest, Actor: value.Actor, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(ctx, applied, 2)
	if err != nil || len(result.ApplyReceipt.Resources) != 8 || result.ApplyReceipt.Activation != authoring.WorkforceActivationActive {
		t.Fatalf("apply result=%#v err=%v", result, err)
	}
	initiative, err := store.GetInitiative(ctx, Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.Placement.InitiativeID)
	if err != nil || initiative.Status != InitiativeStatusActive || initiative.Owner != (ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-live"}) || initiative.Revision != 1 ||
		len(initiative.ObjectiveRefs) != 1 || initiative.ObjectiveRefs[0] != "objective:team" || len(initiative.SourceMonitors) != 1 ||
		initiative.SourceMonitors[0].AssignedAgentID != "agent-live" || initiative.SourceMonitors[0].ObjectiveID != "objective:team" ||
		len(initiative.Milestones) != 1 || len(initiative.Hypotheses) != 1 || len(initiative.Deliverables) != 1 {
		t.Fatalf("Initiative=%#v err=%v", initiative, err)
	}
	objectives, err := store.ListObjectives(ctx, ObjectiveFilter{Scope: initiative.Scope})
	if err != nil {
		t.Fatal(err)
	}
	var monitorObjective *Objective
	for _, objective := range objectives {
		if objective.ID == "objective:team" {
			monitorObjective = objective
		}
	}
	if monitorObjective == nil || monitorObjective.Cadence == nil || monitorObjective.Cadence.AssignedAgentID != "agent-live" ||
		monitorObjective.Cadence.RunTemplate.Context["initiativeId"] != initiative.ID ||
		monitorObjective.Cadence.RunTemplate.Capability.SkillVersion != "1.2.3" {
		t.Fatalf("materialized monitor Objective=%#v", monitorObjective)
	}
	found := false
	for _, resource := range result.ApplyReceipt.Resources {
		found = found || resource.Kind == "initiative" && resource.ID == initiative.ID && resource.Revision == 1
	}
	if !found {
		t.Fatalf("Initiative missing from receipt: %#v", result.ApplyReceipt.Resources)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetInitiative(ctx, initiative.Scope, initiative.ID)
	if err != nil || restored.Revision != 1 || restored.SourceMonitors[0] != initiative.SourceMonitors[0] || restored.CreationFingerprint == "" || restored.IdempotencyKeyHash == "" {
		t.Fatalf("restored Initiative=%#v err=%v", restored, err)
	}
}

func TestSQLiteAtomicWorkforceApplyHonorsInactiveCommitmentWithoutScheduling(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	registerInitiativeSourceSkill(t, store)
	value := testInitiativeWorkforceChangeSet()
	value.Result.Candidate.Activation = authoring.WorkforceActivationInactive
	value.Result.Commitments.Activation = authoring.ActivationCommitmentInactive
	value.Placement.CredentialReferences = map[string]map[string]capability.CredentialReference{
		"agent": {"MODEL_PROVIDER": {Kind: "credential", ID: "model-one"}},
	}
	if _, _, err = store.CreateChangeSet(ctx, value, "create-inactive", "digest-inactive"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-inactive", IdempotencyKey: "apply-inactive", CandidateDigest: value.CandidateDigest, Activation: authoring.WorkforceActivationInactive, Actor: value.Actor, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(ctx, applied, 2)
	if err != nil || result.ApplyReceipt.Activation != authoring.WorkforceActivationInactive {
		t.Fatalf("inactive apply result=%#v err=%v", result, err)
	}
	agents := agent.NewRegistryWithStore(store)
	teams := team.NewRegistryWithStore(store, agents)
	agentDeployment, err := agents.GetDeployment(ctx, value.Scope, "agent-live")
	if err != nil || agentDeployment.RolloutStatus != agent.RolloutPending {
		t.Fatalf("inactive Agent deployment=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err := teams.GetDeployment(ctx, value.Scope, "team-live")
	if err != nil || teamDeployment.Status != team.DeploymentDraft {
		t.Fatalf("inactive Team deployment=%#v err=%v", teamDeployment, err)
	}
	if activations, err := agents.ListActivations(ctx, value.Scope, agentDeployment.ID); err != nil || len(activations) != 0 {
		t.Fatalf("inactive Agent activations=%#v err=%v", activations, err)
	}
	if activations, err := teams.ListActivations(ctx, value.Scope, teamDeployment.ID); err != nil || len(activations) != 0 {
		t.Fatalf("inactive Team activations=%#v err=%v", activations, err)
	}
	bindings, err := store.ListSkillBindings(ctx, value.Scope, agentDeployment.ID)
	if err != nil || len(bindings) != 1 || !bindings[0].Disabled {
		t.Fatalf("inactive Skill bindings=%#v err=%v", bindings, err)
	}
	objectives, err := store.ListObjectives(ctx, ObjectiveFilter{Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}})
	if err != nil || len(objectives) != 2 {
		t.Fatalf("inactive Objectives=%#v err=%v", objectives, err)
	}
	for _, objective := range objectives {
		if objective.Status != ObjectiveStatusDraft {
			t.Fatalf("Objective %s status=%s", objective.ID, objective.Status)
		}
	}
	initiative, err := store.GetInitiative(ctx, Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.Placement.InitiativeID)
	if err != nil || initiative.Status != InitiativeStatusDraft {
		t.Fatalf("inactive Initiative=%#v err=%v", initiative, err)
	}
	schedule, err := NewObjectiveScheduler(store).ReconcileScope(ctx, initiative.Scope, 10)
	if err != nil || schedule.Examined != 0 || schedule.Scheduled != 0 {
		t.Fatalf("inactive schedule=%#v err=%v", schedule, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: initiative.Scope})
	if err != nil || len(runs) != 0 {
		t.Fatalf("inactive Runs=%#v err=%v", runs, err)
	}

	compiler, err := authoring.NewCompiler(testAuthoringGenerator(t))
	if err != nil {
		t.Fatal(err)
	}
	changeSets, err := authoring.NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	catalog := result.Catalog
	catalog.SourcePolicies = map[string]authoring.SourcePolicyCapability{
		"approved-communities": {
			Reference: "approved-communities",
			Sources:   []authoring.SourcePolicySourceCapability{{Host: "community.example"}},
		},
	}
	catalog.AgentCredentialRequirements = []authoring.AgentCredentialRequirement{{
		BindingKey: "MODEL_PROVIDER", DisplayName: "Model provider",
		Prompt: "Choose the model provider this Agent may use.", RequiredForActivation: true,
	}}
	activation, _, err := changeSets.PrepareActivation(ctx, authoring.PrepareChangeSetActivationRequest{
		Scope: result.Scope, ChangeSetID: result.ID, ExpectedRevision: result.Revision, CandidateDigest: result.CandidateDigest,
		Catalog: catalog, Reason: "Start the reviewed workforce", Actor: result.Actor, IdempotencyKey: "prepare-activation",
	})
	if err != nil || activation.Status != authoring.ChangeSetReview ||
		activation.Placement.AgentExpectedRevisions["agent"] != agentDeployment.Revision ||
		activation.Placement.TeamExpectedRevision != teamDeployment.Revision {
		t.Fatalf("activation ChangeSet=%#v err=%v", activation, err)
	}
	reviewed, _, err := changeSets.SubmitEvaluation(ctx, authoring.SubmitChangeSetEvaluationRequest{
		Scope: activation.Scope, ChangeSetID: activation.ID, ExpectedRevision: activation.Revision, CandidateDigest: activation.CandidateDigest,
		Allowed: true, Actor: authoring.ChangeSetActor{Type: "policy_evaluator", ID: "test"}, IdempotencyKey: "evaluate-activation",
	})
	if err != nil || reviewed.Status != authoring.ChangeSetReady {
		t.Fatalf("reviewed activation=%#v err=%v", reviewed, err)
	}
	activated, _, err := changeSets.Apply(ctx, authoring.ApplyChangeSetRequest{
		Scope: reviewed.Scope, ChangeSetID: reviewed.ID, ExpectedRevision: reviewed.Revision, CandidateDigest: reviewed.CandidateDigest,
		Reason: "Activate reviewed workforce", Actor: reviewed.Actor, IdempotencyKey: "apply-activation",
	})
	if err != nil || activated.ApplyReceipt == nil || activated.ApplyReceipt.Activation != authoring.WorkforceActivationActive {
		t.Fatalf("activated ChangeSet=%#v err=%v", activated, err)
	}
	agentDeployment, err = agents.GetDeployment(ctx, value.Scope, "agent-live")
	if err != nil || agentDeployment.RolloutStatus != agent.RolloutActive || agentDeployment.Revision != 2 ||
		agentDeployment.Credentials["MODEL_PROVIDER"].ID != "model-one" {
		t.Fatalf("activated Agent=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err = teams.GetDeployment(ctx, value.Scope, "team-live")
	if err != nil || teamDeployment.Status != team.DeploymentActive || teamDeployment.Revision != 2 {
		t.Fatalf("activated Team=%#v err=%v", teamDeployment, err)
	}
	bindings, err = store.ListSkillBindings(ctx, value.Scope, agentDeployment.ID)
	if err != nil || len(bindings) != 1 || bindings[0].Disabled {
		t.Fatalf("activated Skill bindings=%#v err=%v", bindings, err)
	}
	objectives, err = store.ListObjectives(ctx, ObjectiveFilter{Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}})
	if err != nil || len(objectives) != 2 {
		t.Fatalf("activated Objectives=%#v err=%v", objectives, err)
	}
	for _, objective := range objectives {
		if objective.Status != ObjectiveStatusActive || objective.Revision != 2 {
			t.Fatalf("activated Objective %s=%#v", objective.ID, objective)
		}
	}
	initiative, err = store.GetInitiative(ctx, Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.Placement.InitiativeID)
	if err != nil || initiative.Status != InitiativeStatusActive || initiative.Revision != 2 {
		t.Fatalf("activated Initiative=%#v err=%v", initiative, err)
	}
}

func TestSQLiteAtomicWorkforceInitiativeAmendUsesCASWithoutPartialState(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	registerInitiativeSourceSkill(t, store)
	created := testInitiativeWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create-initiative", "create"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status, first.Revision = authoring.ChangeSetApplied, 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, first, 2); err != nil {
		t.Fatal(err)
	}

	stale := initiativeAmendChangeSet(created, "amend-stale", 99)
	if _, _, err = store.CreateChangeSet(ctx, stale, "amend-stale", "amend-stale"); err != nil {
		t.Fatal(err)
	}
	staleApply := appliedRuntimeChangeSet(stale, "receipt-stale", "apply-stale", first.UpdatedAt.Add(time.Minute))
	if _, err = store.ApplyChangeSet(ctx, staleApply, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale Initiative apply=%v", err)
	}
	if _, err = store.GetDefinition(ctx, "agent", "2"); !errors.Is(err, agent.ErrDefinitionNotFound) {
		t.Fatalf("stale Initiative apply leaked Agent definition: %v", err)
	}
	current, err := store.GetInitiative(ctx, Scope{Kind: "tenant", ID: "one"}, created.Placement.InitiativeID)
	if err != nil || current.Revision != 1 || current.Title != "Research program" {
		t.Fatalf("Initiative after stale apply=%#v err=%v", current, err)
	}

	amend := initiativeAmendChangeSet(created, "amend-valid", 1)
	amend.Result.Candidate.Activation = authoring.WorkforceActivationInactive
	amend.Result.Commitments.Activation = authoring.ActivationCommitmentInactive
	if _, _, err = store.CreateChangeSet(ctx, amend, "amend-valid", "amend-valid"); err != nil {
		t.Fatal(err)
	}
	validApply := appliedRuntimeChangeSet(amend, "receipt-amend", "apply-amend", first.UpdatedAt.Add(2*time.Minute))
	if _, err = store.ApplyChangeSet(ctx, validApply, 2); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetInitiative(ctx, Scope{Kind: "tenant", ID: "one"}, created.Placement.InitiativeID)
	if err != nil || updated.Revision != 2 || updated.Title != "Research program v2" || updated.Status != InitiativeStatusDraft || !updated.CreatedAt.Equal(current.CreatedAt) || updated.IdempotencyKeyHash != current.IdempotencyKeyHash {
		t.Fatalf("amended Initiative=%#v err=%v", updated, err)
	}
	agents := agent.NewRegistryWithStore(store)
	teams := team.NewRegistryWithStore(store, agents)
	agentDeployment, err := agents.GetDeployment(ctx, created.Scope, "agent-live")
	if err != nil || agentDeployment.RolloutStatus != agent.RolloutPaused || agentDeployment.Revision != 2 {
		t.Fatalf("inactive amended Agent=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err := teams.GetDeployment(ctx, created.Scope, "team-live")
	if err != nil || teamDeployment.Status != team.DeploymentPaused || teamDeployment.Revision != 2 {
		t.Fatalf("inactive amended Team=%#v err=%v", teamDeployment, err)
	}
	if activations, err := agents.ListActivations(ctx, created.Scope, agentDeployment.ID); err != nil || len(activations) != 1 {
		t.Fatalf("inactive amended Agent activations=%#v err=%v", activations, err)
	}
	if activations, err := teams.ListActivations(ctx, created.Scope, teamDeployment.ID); err != nil || len(activations) != 1 {
		t.Fatalf("inactive amended Team activations=%#v err=%v", activations, err)
	}
}

func TestSQLiteAtomicWorkforceApplyConcurrentRetryHasOneReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	primary, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	ctx := context.Background()
	ready := testApplicableWorkforceChangeSet()
	if _, _, err = primary.CreateChangeSet(ctx, ready, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	candidate := cloneRuntimeChangeSet(ready)
	candidate.Status = authoring.ChangeSetApplied
	candidate.Revision = 3
	candidate.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: ready.CandidateDigest, Actor: ready.Actor, AppliedAt: ready.UpdatedAt.Add(time.Minute)}
	candidate.UpdatedAt = candidate.ApplyReceipt.AppliedAt
	stores := []*SQLiteStore{primary, replica}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(store *SQLiteStore) {
			defer wg.Done()
			_, err := store.ApplyChangeSet(ctx, cloneRuntimeChangeSet(candidate), 2)
			errs <- err
		}(store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	restored, err := primary.GetChangeSet(ctx, ready.Scope, ready.ID)
	if err != nil || restored.ApplyReceipt == nil || restored.ApplyReceipt.ID != "receipt" {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
}

func TestSQLiteAtomicWorkforceAmendRejectsStaleRevisionWithoutPartialState(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	created := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create", "create"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(created)
	applied.Status = authoring.ChangeSetApplied
	applied.Revision = 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, applied, 2); err != nil {
		t.Fatal(err)
	}
	amend := testApplicableWorkforceChangeSet()
	amend.ID = "amend"
	amend.Mode = authoring.ModeAmend
	amend.ParentID = created.ID
	amend.CandidateDigest = "candidate-2"
	amend.Result.Candidate.Agents[0].Version = "2"
	amend.Result.Candidate.Team.Version = "2"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 99}
	amend.Placement.TeamExpectedRevision = 1
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	if _, _, err = store.CreateChangeSet(ctx, amend, "amend", "amend"); err != nil {
		t.Fatal(err)
	}
	attempt := cloneRuntimeChangeSet(amend)
	attempt.Status = authoring.ChangeSetApplied
	attempt.Revision = 3
	attempt.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-amend", IdempotencyKey: "apply-amend", CandidateDigest: amend.CandidateDigest, Actor: amend.Actor, AppliedAt: amend.UpdatedAt.Add(time.Minute)}
	attempt.UpdatedAt = attempt.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, attempt, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale apply=%v", err)
	}
	if _, err = store.GetDefinition(ctx, "agent", "2"); !errors.Is(err, agent.ErrDefinitionNotFound) {
		t.Fatalf("partial Agent definition=%v", err)
	}
	if _, err = store.GetTeamDefinition(ctx, "team", "2"); !errors.Is(err, team.ErrDefinitionNotFound) {
		t.Fatalf("partial Team definition=%v", err)
	}
	current, err := store.GetChangeSet(ctx, amend.Scope, amend.ID)
	if err != nil || current.Status != authoring.ChangeSetReady {
		t.Fatalf("change set=%#v err=%v", current, err)
	}
}

func TestSQLiteAtomicWorkforceAmendActivatesNewVersionsTogether(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	created := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create", "create"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status = authoring.ChangeSetApplied
	first.Revision = 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, first, 2); err != nil {
		t.Fatal(err)
	}
	amend := testApplicableWorkforceChangeSet()
	amend.ID = "amend"
	amend.Mode = authoring.ModeAmend
	amend.ParentID = created.ID
	amend.CandidateDigest = "candidate-2"
	amend.Result.Candidate.Agents[0].Version = "2"
	amend.Result.Candidate.Team.Version = "2"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	amend.Placement.TeamExpectedRevision = 1
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	if _, _, err = store.CreateChangeSet(ctx, amend, "amend", "amend"); err != nil {
		t.Fatal(err)
	}
	second := cloneRuntimeChangeSet(amend)
	second.Status = authoring.ChangeSetApplied
	second.Revision = 3
	second.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-amend", IdempotencyKey: "apply-amend", CandidateDigest: amend.CandidateDigest, Actor: amend.Actor, AppliedAt: first.UpdatedAt.Add(time.Minute)}
	second.UpdatedAt = second.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, second, 2); err != nil {
		t.Fatal(err)
	}
	agentDeployment, err := store.GetDeployment(ctx, created.Scope, "agent-live")
	if err != nil || agentDeployment.ActiveVersion != "2" || agentDeployment.PreviousVersion != "1" || agentDeployment.Revision != 2 {
		t.Fatalf("Agent deployment=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err := store.GetTeamDeployment(ctx, created.Scope, "team-live")
	if err != nil || teamDeployment.ActiveVersion != "2" || teamDeployment.Revision != 2 {
		t.Fatalf("Team deployment=%#v err=%v", teamDeployment, err)
	}
	if versions, err := store.ListDefinitionVersions(ctx, "agent"); err != nil || len(versions) != 2 {
		t.Fatalf("Agent versions=%d err=%v", len(versions), err)
	}
	if versions, err := store.ListTeamDefinitionVersions(ctx, "team"); err != nil || len(versions) != 2 {
		t.Fatalf("Team versions=%d err=%v", len(versions), err)
	}
	objectives, err := store.ListObjectives(ctx, ObjectiveFilter{Scope: Scope{Kind: "tenant", ID: "one"}})
	if err != nil || len(objectives) != 2 || objectives[0].Revision != 2 || objectives[1].Revision != 2 {
		t.Fatalf("objective portfolio=%#v err=%v", objectives, err)
	}
}

func TestSQLiteAtomicWorkforceAmendCreatesUnappliedResourcesAndPreservesUnrelatedBindings(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	catalog := skill.NewCatalogWithStore(store)
	if err = catalog.Register(ctx, &skill.Definition{ID: "unrelated", Version: "1", Name: "Unrelated", Prompt: &skill.PromptModule{Instructions: "Remain bound."}}); err != nil {
		t.Fatal(err)
	}
	unrelated := &skill.Binding{ID: "workforce:other-live:unrelated", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "other-live", SkillID: "unrelated", SkillVersion: "1", EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1}
	if err = catalog.Bind(ctx, unrelated); err != nil {
		t.Fatal(err)
	}

	parent := testApplicableWorkforceChangeSet()
	parent.ID = "rejected-parent"
	parent.Status = authoring.ChangeSetRejected
	if _, _, err = store.CreateChangeSet(ctx, parent, "parent", "parent"); err != nil {
		t.Fatal(err)
	}
	recovered := testApplicableWorkforceChangeSet()
	recovered.ID = "recovered"
	recovered.ParentID = parent.ID
	recovered.Mode = authoring.ModeAmend
	if _, _, err = store.CreateChangeSet(ctx, recovered, "recovered", "recovered"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(recovered)
	applied.Status = authoring.ChangeSetApplied
	applied.Revision = 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-recovered", IdempotencyKey: "apply-recovered", CandidateDigest: recovered.CandidateDigest, Actor: recovered.Actor, AppliedAt: recovered.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(ctx, applied, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deployment, err := store.GetDeployment(ctx, recovered.Scope, "agent-live"); err != nil || deployment.Revision != 1 {
		t.Fatalf("Agent deployment=%#v err=%v", deployment, err)
	}
	if deployment, err := store.GetTeamDeployment(ctx, recovered.Scope, "team-live"); err != nil || deployment.Revision != 1 {
		t.Fatalf("Team deployment=%#v err=%v", deployment, err)
	}
	if prompts, err := catalog.ListModelPrompts(ctx, unrelated.Scope, unrelated.DeploymentID); err != nil || len(prompts) != 1 {
		t.Fatalf("unrelated prompts=%#v err=%v", prompts, err)
	}
	if result.ApplyReceipt == nil || len(result.ApplyReceipt.Resources) != 6 {
		t.Fatalf("receipt=%#v", result.ApplyReceipt)
	}
}

func TestSQLiteAtomicWorkforceAmendSupportsMixedCreateAndUpdatePlacements(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	created := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create-mixed", "create-mixed"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status, first.Revision = authoring.ChangeSetApplied, 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create-mixed", IdempotencyKey: "apply-create-mixed", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, first, 2); err != nil {
		t.Fatal(err)
	}

	amend := testApplicableWorkforceChangeSet()
	amend.ID, amend.ParentID, amend.Mode, amend.CandidateDigest = "mixed", created.ID, authoring.ModeAmend, "candidate-mixed"
	newAgent := &agent.AgentDefinition{ID: "reviewer", Version: "1", DisplayName: "Reviewer", Purpose: "Review", SystemPrompt: "Review the work", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}}
	amend.Result.Candidate.Agents = append(amend.Result.Candidate.Agents, newAgent)
	amend.Result.Candidate.Team.Version = "2"
	amend.Result.Candidate.Team.Roles = append(amend.Result.Candidate.Team.Roles, team.RoleSlot{ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review", MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{"reviewer"}})
	amend.Result.Candidate.Assignments = append(amend.Result.Candidate.Assignments, authoring.Assignment{ID: "reviewer", RoleID: "reviewer", AgentDefinitionID: "reviewer"})
	amend.Placement.AgentDeploymentIDs["reviewer"] = "reviewer-live"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	amend.Placement.TeamExpectedRevision = 1
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	if _, _, err = store.CreateChangeSet(ctx, amend, "mixed", "mixed"); err != nil {
		t.Fatal(err)
	}
	second := cloneRuntimeChangeSet(amend)
	second.Status, second.Revision = authoring.ChangeSetApplied, 3
	second.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-mixed", IdempotencyKey: "apply-mixed", CandidateDigest: amend.CandidateDigest, Actor: amend.Actor, AppliedAt: first.UpdatedAt.Add(time.Minute)}
	second.UpdatedAt = second.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, second, 2); err != nil {
		t.Fatal(err)
	}
	if deployment, err := store.GetDeployment(ctx, amend.Scope, "agent-live"); err != nil || deployment.Revision != 2 {
		t.Fatalf("updated Agent=%#v err=%v", deployment, err)
	}
	if deployment, err := store.GetDeployment(ctx, amend.Scope, "reviewer-live"); err != nil || deployment.Revision != 1 {
		t.Fatalf("new Agent=%#v err=%v", deployment, err)
	}
}

func testApplicableWorkforceChangeSet() *authoring.ChangeSet {
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agentDefinition := &agent.AgentDefinition{ID: "agent", Version: "1", DisplayName: "Agent", Purpose: "Work", SystemPrompt: "Do the work", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}, ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "agent-goal", Title: "Agent goal", Goal: "Finish agent work"}}}
	teamDefinition := &team.Definition{ID: "team", Version: "1", DisplayName: "Team", Purpose: "Work together", Roles: []team.RoleSlot{{ID: "worker", DisplayName: "Worker", Purpose: "Work", MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{"agent"}}}, Coordination: team.CoordinationPolicy{}, Approvals: team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead}, ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "team-goal", Title: "Team goal", Goal: "Finish team work"}}}
	return &authoring.ChangeSet{ID: "change", Scope: scope, Mode: authoring.ModeCreate, Prompt: "Create", PromptDigest: "prompt", CandidateDigest: "candidate", Result: authoring.CompileResult{Candidate: authoring.WorkforceCandidate{Agents: []*agent.AgentDefinition{agentDefinition}, Team: teamDefinition, Assignments: []authoring.Assignment{{ID: "worker", RoleID: "worker", AgentDefinitionID: "agent"}}}, Valid: true}, Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "team-live", AgentDeploymentIDs: map[string]string{"agent": "agent-live"}, Objectives: map[string]authoring.ObjectivePlacement{authoring.WorkforceObjectiveKey("agent", "agent", "agent-goal"): {ID: "objective:agent"}, authoring.WorkforceObjectiveKey("team", "team", "team-goal"): {ID: "objective:team"}}, Environment: "test"}, Status: authoring.ChangeSetReady, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, Revision: 2, CreatedAt: now, UpdatedAt: now}
}

func testAgentDeploymentIdentityCollisionChangeSet(id string) *authoring.ChangeSet {
	value := testApplicableWorkforceChangeSet()
	value.ID = id
	value.CandidateDigest = "candidate-" + id
	agentDefinition := value.Result.Candidate.Agents[0]
	agentDefinition.ID = "agent-collision"
	agentDefinition.DisplayName = "Analyst Agent"
	teamDefinition := value.Result.Candidate.Team
	teamDefinition.ID = "team-" + id
	teamDefinition.Roles[0].RequiredDefinitionIDs = []string{agentDefinition.ID}
	value.Result.Candidate.Assignments[0].AgentDefinitionID = agentDefinition.ID
	value.Placement.TeamDeploymentID = "team-" + id + "-live"
	value.Placement.AgentDeploymentIDs = map[string]string{agentDefinition.ID: "agent-live"}
	value.Placement.Objectives = map[string]authoring.ObjectivePlacement{
		authoring.WorkforceObjectiveKey("agent", agentDefinition.ID, "agent-goal"): {
			ID: "objective:" + id + ":agent",
		},
		authoring.WorkforceObjectiveKey("team", teamDefinition.ID, "team-goal"): {
			ID: "objective:" + id + ":team",
		},
	}
	return value
}

func testInitiativeWorkforceChangeSet() *authoring.ChangeSet {
	value := testApplicableWorkforceChangeSet()
	agentDefinition := value.Result.Candidate.Agents[0]
	agentDefinition.SkillRequirements = []agent.SkillRequirement{{SkillID: "community-source", VersionConstraint: "1.2.3", RequiredActions: []string{"observe"}}}
	agentDefinition.Authority.AllowedSkillIDs = []string{"community-source"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"community-source": {ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}, MaximumRisk: capability.RiskLevelRead},
	}}
	teamObjective := &value.Result.Candidate.Team.ObjectiveTemplates[0]
	teamObjective.Cadence = map[string]interface{}{
		"type": "interval", "intervalSeconds": int64(3600), "assignedAgentId": "agent", "maximumConcurrent": 1,
		"runBudget": map[string]interface{}{"maxTurns": int64(2), "maxActions": int64(1), "maxDurationMs": int64(60000)},
		"runTemplate": map[string]interface{}{
			"entrypoint": "monitor",
			"context":    map[string]interface{}{"initiativeId": "research-program", "sourceMonitorId": "community-listening"},
			"policy":     map[string]interface{}{"sourcePolicyRef": "approved-communities"},
			"capability": map[string]interface{}{"skillId": "community-source", "skillVersion": "1.2.3", "action": "observe", "inputs": map[string]interface{}{"query": "agent runtime pain points"}},
		},
	}
	teamObjectiveRef := authoring.WorkforceObjectiveKey(authoring.InitiativeOwnerTeam, "team", "team-goal")
	value.Result.Candidate.Initiative = &authoring.InitiativeBlueprint{
		ID: "research-program", Title: "Research program", Purpose: "Continuously understand user pain points",
		Owner:         authoring.InitiativeOwnerReference{Type: authoring.InitiativeOwnerTeam, DefinitionID: "team"},
		ObjectiveRefs: []string{teamObjectiveRef},
		Milestones:    []authoring.InitiativeMilestoneBlueprint{{ID: "baseline", Title: "Establish baseline", ObjectiveRefs: []string{teamObjectiveRef}}},
		Hypotheses:    []authoring.InitiativeHypothesisBlueprint{{ID: "setup-friction", Statement: "Setup friction limits adoption", Confidence: 0.5}},
		SourceMonitors: []authoring.InitiativeSourceMonitorBlueprint{{
			ID: "community-listening", ObjectiveRef: teamObjectiveRef, AssignedAgentDefinitionID: "agent", SkillID: "community-source", SkillVersion: "1.2.3", Action: "observe",
			SourcePolicyRef: "approved-communities", Deduplication: authoring.InitiativeDeduplicateStableSourceAndContent,
		}},
		Deliverables: []authoring.InitiativeDeliverableBlueprint{{ID: "cited-report", Title: "Cited report", ObjectiveRefs: []string{teamObjectiveRef}}},
		Policy:       map[string]interface{}{"outreachApproval": "required"},
	}
	value.Placement.InitiativeID = "initiative:research"
	return value
}

func registerInitiativeSourceSkill(t *testing.T, store skill.CatalogStore) {
	t.Helper()
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(context.Background(), &skill.Definition{
		ID: "community-source", Version: "1.2.3", Name: "Community source", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://community.invalid"},
		Actions: map[string]skill.Action{"observe": {
			Name: "observe", Description: "Observe a permitted community source", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
			Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func initiativeAmendChangeSet(created *authoring.ChangeSet, id string, initiativeRevision int64) *authoring.ChangeSet {
	amend := testInitiativeWorkforceChangeSet()
	amend.ID, amend.ParentID, amend.Mode, amend.CandidateDigest = id, created.ID, authoring.ModeAmend, "candidate-"+id
	amend.Result.Candidate.Agents[0].Version = "2"
	amend.Result.Candidate.Team.Version = "2"
	amend.Result.Candidate.Initiative.Title = "Research program v2"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	amend.Placement.TeamExpectedRevision = 1
	amend.Placement.InitiativeExpectedRevision = initiativeRevision
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	return amend
}

func appliedRuntimeChangeSet(value *authoring.ChangeSet, receiptID, key string, at time.Time) *authoring.ChangeSet {
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision, applied.UpdatedAt = authoring.ChangeSetApplied, 3, at
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: receiptID, IdempotencyKey: key, CandidateDigest: value.CandidateDigest, Actor: value.Actor, AppliedAt: at}
	return applied
}

func cloneRuntimeChangeSet(value *authoring.ChangeSet) *authoring.ChangeSet {
	payload, _ := json.Marshal(value)
	var result authoring.ChangeSet
	_ = json.Unmarshal(payload, &result)
	return &result
}

func testWorkforceChangeSet(scope capability.ScopeReference, id string) *authoring.ChangeSet {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &authoring.ChangeSet{
		ID: id, Scope: scope, Mode: authoring.ModeCreate, Prompt: "Create a Team", PromptDigest: "prompt",
		CandidateDigest: "candidate", Result: authoring.CompileResult{Valid: true},
		Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "team-live"}, Status: authoring.ChangeSetReview,
		Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
