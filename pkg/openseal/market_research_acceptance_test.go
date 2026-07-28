package openseal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactstore "github.com/axiom-studio/openseal/pkg/artifact"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/delivery"
	"github.com/axiom-studio/openseal/pkg/document"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

const (
	researchTeamID       = "market-research-team"
	researchInitiativeID = "competitor-research"
	researcherID         = "source-researcher"
	analystID            = "research-analyst"
	publisherID          = "report-publisher"
)

func TestMarketResearchInitiativeSurvivesRestartAndDeliversReviewedReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	root := t.TempDir()
	databasePath := filepath.Join(root, "kernel.db")
	contentRoot := filepath.Join(root, "artifacts")
	scope := Scope{Kind: "tenant", ID: "research-acceptance"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: researchTeamID}
	secret := "acceptance-email-secret-must-never-persist"

	store, engine := openResearchAcceptanceEngine(t, databasePath, scope, nil, nil)
	objectives := composeResearchWorkforce(t, ctx, engine, scope, owner)
	sourceRun := mustCreateRun(t, ctx, engine, CreateAgentRunRequest{
		Scope: scope, ObjectiveID: objectives["monitor"].ID, Owner: owner, AssignedAgentID: researcherID,
		Goal: "Monitor approved public discussions", Source: RunSourceSchedule,
		Context: map[string]interface{}{"initiativeId": researchInitiativeID, "sourceMonitorId": "forums"},
	})
	initiative, _, err := engine.CreateInitiative(ctx, CreateInitiativeRequest{
		Initiative: &Initiative{
			ID: researchInitiativeID, Scope: scope, Title: "Competitor user research",
			Purpose: "Identify evidence-backed user pain points and deliver a reviewed report",
			Status:  InitiativeStatusActive, Owner: owner,
			AgentRefs: []InitiativeResourceReference{
				{Kind: InitiativeResourceAgentDeployment, ID: researcherID},
				{Kind: InitiativeResourceAgentDeployment, ID: analystID},
				{Kind: InitiativeResourceAgentDeployment, ID: publisherID},
			},
			TeamRefs:      []InitiativeResourceReference{{Kind: InitiativeResourceTeamDeployment, ID: researchTeamID}},
			ObjectiveRefs: []string{objectives["monitor"].ID, objectives["report"].ID, objectives["deliver"].ID},
			RunRefs:       []string{sourceRun.ID},
			SourceMonitors: []InitiativeSourceMonitorReference{{
				ID: "forums", ObjectiveID: objectives["monitor"].ID, AssignedAgentID: researcherID,
				SkillID: "public-forum-reader", SkillVersion: "1.0.0", Action: "search",
				SourcePolicyRef: "approved-public-forums", Deduplication: InitiativeSourceDeduplicateStableSourceAndContent,
			}},
			Milestones: []InitiativeMilestone{{
				ID: "evidence", Title: "Collect evidence", Status: InitiativeMilestoneInProgress,
				ObjectiveRefs: []string{objectives["monitor"].ID},
			}, {
				ID: "report", Title: "Deliver reviewed report", Status: InitiativeMilestonePending,
				ObjectiveRefs: []string{objectives["report"].ID, objectives["deliver"].ID},
			}},
			Deliverables: []InitiativeDeliverable{{
				ID: "weekly-report", Title: "Cited market-research report", Status: InitiativeDeliverablePlanned,
				ObjectiveRefs: []string{objectives["report"].ID, objectives["deliver"].ID},
			}},
		},
		IdempotencyKey: "research-initiative", Actor: ActivityActor{Type: "user", ID: "operator"},
	})
	if err != nil {
		t.Fatal(err)
	}

	observations := make([]*SourceObservation, 0, 2)
	for index, finding := range []struct {
		stableID string
		uri      string
		summary  string
	}{
		{stableID: "thread-101", uri: "https://forum.example/threads/101", summary: "Users struggle to understand initial credential configuration."},
		{stableID: "thread-204", uri: "https://forum.example/threads/204", summary: "Users value restart recovery but want clearer progress reporting."},
	} {
		digest := sha256.Sum256([]byte(finding.summary))
		result, ingestErr := engine.IngestSourceObservation(ctx, IngestSourceObservationRequest{
			Scope: scope, InitiativeID: initiative.ID, MonitorID: "forums",
			ExpectedCheckpointRevision: int64(index), Cursor: "page-" + string(rune('1'+index)),
			StableSourceID: finding.stableID, SourceURI: finding.uri,
			ContentDigest: "sha256:" + hex.EncodeToString(digest[:]), Summary: finding.summary,
			ObservedAt: time.Date(2026, 7, 24, 12, index, 0, 0, time.UTC),
			RunID:      sourceRun.ID, AgentID: researcherID, SkillID: "public-forum-reader",
			SkillVersion: "1.0.0", Action: "search", ActionCallID: "search-" + finding.stableID,
			Metadata: map[string]interface{}{"confidence": 0.9},
		})
		if ingestErr != nil {
			t.Fatal(ingestErr)
		}
		observations = append(observations, result.Observation)
	}
	mustCompleteRun(t, ctx, engine, sourceRun, "Source monitoring completed")

	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, engine = openResearchAcceptanceEngine(t, databasePath, scope, nil, nil)
	checkpoint, err := engine.GetSourceMonitorCheckpoint(ctx, scope, initiative.ID, "forums")
	if err != nil || checkpoint.Revision != 2 || checkpoint.ObservationCount != 2 {
		t.Fatalf("checkpoint=%#v err=%v", checkpoint, err)
	}
	persistedEvidence, err := engine.ListSourceObservations(ctx, SourceObservationFilter{
		Scope: scope, InitiativeID: initiative.ID, Limit: 10,
	})
	if err != nil || len(persistedEvidence) != 2 {
		t.Fatalf("evidence=%#v err=%v", persistedEvidence, err)
	}

	reportRun := mustCreateRun(t, ctx, engine, CreateAgentRunRequest{
		Scope: scope, ObjectiveID: objectives["report"].ID, Owner: owner, AssignedAgentID: analystID,
		Goal: "Synthesize a cited report", Source: RunSourceObjective,
		Context: map[string]interface{}{"initiativeId": initiative.ID},
	})
	reportBody := "Finding 1: " + observations[0].Summary + "\nSource: " + observations[0].SourceURI +
		"\n\nFinding 2: " + observations[1].Summary + "\nSource: " + observations[1].SourceURI
	rendered, err := (document.PlainTextPDFRenderer{}).RenderPDF(document.Report{
		Title: "Competitor user research", Body: reportBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := artifactstore.NewLocalStore(contentRoot)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := content.Put(ctx, ArtifactContentWrite{
		Scope: scope, MediaType: "application/pdf", Reader: bytes.NewReader(rendered.Bytes), SizeBytes: int64(len(rendered.Bytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	reportArtifact, err := engine.RegisterArtifact(ctx, RegisterArtifactRequest{Artifact: &Artifact{
		ID: "competitor-research-report", Version: 1, Scope: scope, Name: "competitor-research.pdf",
		Type: "report", MediaType: "application/pdf", ContentRef: stored.ContentRef, Digest: stored.Digest,
		SizeBytes: stored.SizeBytes, Classification: ArtifactClassInternal,
		Provenance: ArtifactProvenance{
			Producer: ActivityActor{Type: "agent", ID: analystID}, Owner: &owner,
			RunID: reportRun.ID, ObjectiveID: objectives["report"].ID,
		},
		Evidence: []ArtifactEvidenceLink{
			{Relation: ArtifactEvidenceCites, TargetKind: ArtifactEvidenceTargetExternalSource, TargetRef: observations[0].SourceURI, Summary: observations[0].Summary},
			{Relation: ArtifactEvidenceCites, TargetKind: ArtifactEvidenceTargetExternalSource, TargetRef: observations[1].SourceURI, Summary: observations[1].Summary},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mustCompleteRun(t, ctx, engine, reportRun, "Cited PDF report generated")

	deliveryRun := mustCreateRun(t, ctx, engine, CreateAgentRunRequest{
		Scope: scope, ObjectiveID: objectives["deliver"].ID, Owner: owner, AssignedAgentID: publisherID,
		Goal: "Deliver the reviewed report", Source: RunSourceObjective,
		Context: map[string]interface{}{"initiativeId": initiative.ID},
	})
	claimed, err := engine.ClaimNextAgentRun(ctx, AgentRunClaimRequest{
		Scope: scope, WorkerID: "publisher-worker", AssignedAgentID: publisherID, LeaseDuration: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != deliveryRun.ID {
		t.Fatalf("delivery claim=%#v err=%v", claimed, err)
	}
	proposal, err := engine.ProposeAction(ctx, ProposeActionRequest{
		Scope: scope, RunID: deliveryRun.ID, WorkerID: "publisher-worker",
		DeploymentID: researchTeamID,
		SkillID:      delivery.SkillID, SkillVersion: delivery.SkillVersion, Action: delivery.SendEmail,
		Arguments: map[string]interface{}{
			"to": []interface{}{"research@example.com"}, "subject": "Competitor user research",
			"body":         "The reviewed, cited report is attached.",
			"artifactRefs": []interface{}{map[string]interface{}{"id": reportArtifact.Artifact.ID, "version": 1}},
		},
		IdempotencyKey: "deliver-research-report",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval ||
		proposal.Run.Status != AgentRunStatusWaitingForApproval {
		t.Fatalf("delivery proposal=%#v", proposal)
	}

	runRefs := []string{sourceRun.ID, reportRun.ID, deliveryRun.ID}
	deliverables := []InitiativeDeliverable{{
		ID: "weekly-report", Title: "Cited market-research report", Status: InitiativeDeliverableReview,
		ObjectiveRefs: []string{objectives["report"].ID, objectives["deliver"].ID},
		ArtifactRefs:  []InitiativeResourceReference{{Kind: InitiativeResourceArtifact, ID: reportArtifact.Artifact.ID, Revision: 1}},
	}}
	initiative, _, err = engine.UpdateInitiative(ctx, scope, initiative.ID, UpdateInitiativeRequest{
		ExpectedRevision: initiative.Revision, RunRefs: &runRefs, Deliverables: &deliverables,
		Checkpoint: map[string]interface{}{"phase": "awaiting_delivery_approval", "evidenceCount": 2},
		Actor:      ActivityActor{Type: "agent", ID: analystID},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	resolver := CredentialResolverFunc(func(_ context.Context, request CredentialResolutionRequest) (map[string]string, error) {
		if request.References[delivery.EmailCredentialName].ID != "opaque://research/email-delivery" {
			t.Fatalf("unexpected credential reference: %#v", request.References)
		}
		return map[string]string{delivery.EmailCredentialName: secret}, nil
	})
	dispatcher := ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
		if input.Credentials[delivery.EmailCredentialName] != secret {
			t.Fatal("delivery dispatcher did not receive the ephemeral credential")
		}
		return map[string]interface{}{
			"receiptId": "delivery-receipt-1", "status": "accepted", "recipientCount": 1,
			"deliveredAt": "2026-07-24T12:30:00Z", "artifactRefs": input.Arguments["artifactRefs"],
		}, nil
	})
	store, engine = openResearchAcceptanceEngine(t, databasePath, scope, resolver, dispatcher)
	resolution, err := engine.ResolveApproval(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "operator-approved-delivery", Approve: true,
		Principal: ApprovalPrincipal{Type: "role", ID: "operator"}, Reason: "Evidence and recipients reviewed",
	})
	if err != nil || resolution.Call.Status != ActionCallStatusReady {
		t.Fatalf("approval resolution=%#v err=%v", resolution, err)
	}
	engine.Start(ctx)
	engine.WakeActionWorkers()
	waitForResearchAcceptance(t, ctx, func() bool {
		call, loadErr := engine.GetActionCall(ctx, scope, proposal.Call.ID)
		return loadErr == nil && call.Status == ActionCallStatusSucceeded
	}, "approved delivery action")
	engine.Stop()
	currentDeliveryRun, err := engine.GetAgentRun(ctx, scope, deliveryRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustCompleteRun(t, ctx, engine, currentDeliveryRun, "Reviewed report accepted by delivery provider")

	deliverables[0].Status = InitiativeDeliverableDelivered
	milestones := []InitiativeMilestone{
		{ID: "evidence", Title: "Collect evidence", Status: InitiativeMilestoneCompleted, ObjectiveRefs: []string{objectives["monitor"].ID}},
		{ID: "report", Title: "Deliver reviewed report", Status: InitiativeMilestoneCompleted, ObjectiveRefs: []string{objectives["report"].ID, objectives["deliver"].ID}},
	}
	completed := InitiativeStatusCompleted
	initiative, _, err = engine.UpdateInitiative(ctx, scope, initiative.ID, UpdateInitiativeRequest{
		ExpectedRevision: initiative.Revision, Status: &completed, Milestones: &milestones, Deliverables: &deliverables,
		Checkpoint: map[string]interface{}{"phase": "completed", "deliveryReceiptId": "delivery-receipt-1"},
		Actor:      ActivityActor{Type: "agent", ID: publisherID},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertResearchAcceptance(t, ctx, engine, scope, initiative.ID, reportArtifact.Artifact.ID, proposal.Call.ID)
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	databaseBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(databaseBytes, []byte(secret)) {
		t.Fatal("resolved delivery credential was persisted")
	}
}

func openResearchAcceptanceEngine(
	t *testing.T,
	path string,
	scope Scope,
	resolver CredentialResolver,
	dispatcher ActionDispatcher,
) (*runtime.SQLiteStore, *Engine) {
	t.Helper()
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	options := []Option{WithPersistentStore(store)}
	if dispatcher != nil {
		options = append(options, WithActionWorkers(ActionWorkerConfig{
			Scope: scope, Concurrency: 1, PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second,
		}, resolver, dispatcher))
	}
	engine, err := New(options...)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	return store, engine
}

func composeResearchWorkforce(
	t *testing.T,
	ctx context.Context,
	engine *Engine,
	scope Scope,
	owner ObjectiveOwner,
) map[string]*Objective {
	t.Helper()
	deploymentScope := SkillScope{Kind: scope.Kind, ID: scope.ID}
	for _, agent := range []struct {
		id      string
		purpose string
	}{
		{id: researcherID, purpose: "Collect provenance-linked evidence from approved sources"},
		{id: analystID, purpose: "Synthesize evidence into cited durable reports"},
		{id: publisherID, purpose: "Deliver reviewed artifacts through governed capabilities"},
	} {
		definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
			ID: agent.id, Version: "1", DisplayName: strings.ReplaceAll(agent.id, "-", " "), Purpose: agent.purpose,
			SystemPrompt: "Use authoritative capability results, preserve citations, and request approval for external effects.",
			Authority:    AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, MaxConcurrentRuns: 2},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = engine.CreateAgentDeployment(ctx, &AgentDeployment{
			ID: agent.id, Scope: deploymentScope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: AgentRolloutActive, Environment: "acceptance",
			Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 2},
		}, "user", "operator", "compose research Team"); err != nil {
			t.Fatal(err)
		}
	}
	roles := []TeamRoleSlot{
		{ID: "researcher", DisplayName: "Researcher", Purpose: "Collect evidence", MinimumMembers: 1},
		{ID: "analyst", DisplayName: "Analyst", Purpose: "Synthesize cited findings", MinimumMembers: 1},
		{
			ID: "publisher", DisplayName: "Publisher", Purpose: "Deliver reviewed reports", MinimumMembers: 1,
			SkillGrants: []TeamRoleSkillGrant{{
				SkillID: delivery.SkillID, SkillVersion: delivery.SkillVersion,
				AllowedActions: []string{delivery.SendEmail}, MaximumRisk: SkillRiskExternal,
			}},
		},
	}
	teamDefinition, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: researchTeamID, Version: "1", DisplayName: "Market Research Team",
		Purpose: "Monitor approved sources and deliver evidence-backed reports",
		Roles:   roles,
		Coordination: TeamCoordinationPolicy{
			QuietByDefault: true, RequireRoleRelevance: true, MaximumSpeakersPerRound: 2,
		},
		Approvals: TeamApprovalPolicy{MaximumRisk: capability.RiskLevelExternal, ApproverRoleIDs: []string{"publisher"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = engine.CreateTeamDeployment(ctx, &TeamDeployment{
		ID: researchTeamID, Scope: deploymentScope, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
		Status: TeamDeploymentActive,
		Roster: []TeamRosterAssignment{
			{ID: "researcher", RoleID: "researcher", AgentDeploymentID: researcherID},
			{ID: "analyst", RoleID: "analyst", AgentDeploymentID: analystID},
			{ID: "publisher", RoleID: "publisher", AgentDeploymentID: publisherID},
		},
		Restrictions: TeamDeploymentRestrictions{
			AllowedSkillIDs: []string{delivery.SkillID}, MaximumRisk: SkillRiskExternal,
		},
	}, "user", "operator", "compose research Team"); err != nil {
		t.Fatal(err)
	}
	if err = engine.RegisterSkill(ctx, delivery.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err = engine.BindSkill(ctx, &SkillBinding{
		ID: "research-email", Scope: deploymentScope, DeploymentID: researchTeamID,
		SkillID: delivery.SkillID, SkillVersion: delivery.SkillVersion,
		AllowedActions: []string{delivery.SendEmail}, MaximumRisk: SkillRiskExternal,
		Credentials: map[string]SkillCredentialReference{
			delivery.EmailCredentialName: {Kind: delivery.EmailCredentialKind, ID: "opaque://research/email-delivery"},
		},
		Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	objectives := map[string]*Objective{}
	for _, value := range []struct {
		key, title, goal string
	}{
		{key: "monitor", title: "Monitor public discussions", goal: "Collect retained, deduplicated evidence from approved sources"},
		{key: "report", title: "Synthesize cited report", goal: "Produce a durable PDF report linked to source evidence"},
		{key: "deliver", title: "Deliver reviewed report", goal: "Request approval and deliver the report through an authorized Skill"},
	} {
		request := CreateObjectiveRequest{
			Scope: scope, Owner: owner, Title: value.title, Goal: value.goal, Status: ObjectiveStatusActive,
		}
		objective, createErr := engine.CreateObjective(ctx, request)
		if createErr != nil {
			t.Fatal(createErr)
		}
		objectives[value.key] = objective
		if value.key == "monitor" {
			_, createErr = engine.CreateRunbookActivation(ctx, CreateRunbookActivationRequest{
				ID: "forums-monitor-runbook", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: researcherID,
				DefinitionID: "market-research", DefinitionVersion: "1.0.0", TriggerID: "forums",
				Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}, Entrypoint: "monitor"},
				Input:   map[string]interface{}{"initiativeId": researchInitiativeID, "sourceMonitorId": "forums", "query": "competitor pain points"},
				Policy:  map[string]interface{}{"sourcePolicyRef": "approved-public-forums"}, Status: RunbookActivationActive,
			})
			if createErr != nil {
				t.Fatal(createErr)
			}
		}
	}
	return objectives
}

func mustCreateRun(t *testing.T, ctx context.Context, engine *Engine, request CreateAgentRunRequest) *AgentRun {
	t.Helper()
	run, err := engine.CreateAgentRun(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func mustCompleteRun(t *testing.T, ctx context.Context, engine *Engine, run *AgentRun, summary string) {
	t.Helper()
	current, err := engine.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status == AgentRunStatusQueued {
		current, _, err = engine.TransitionAgentRun(ctx, current.Scope, current.ID, RunTransitionRequest{
			ExpectedRevision: current.Revision, Status: AgentRunStatusRunning,
			Summary: "Work started", Actor: ActivityActor{Type: "agent", ID: current.AssignedAgentID},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if current.Status != AgentRunStatusRunning {
		t.Fatalf("run %s status=%s before completion", current.ID, current.Status)
	}
	if _, _, err = engine.TransitionAgentRun(ctx, current.Scope, current.ID, RunTransitionRequest{
		ExpectedRevision: current.Revision, Status: AgentRunStatusCompleted, Summary: summary,
		Actor: ActivityActor{Type: "agent", ID: current.AssignedAgentID},
	}); err != nil {
		t.Fatal(err)
	}
}

func waitForResearchAcceptance(t *testing.T, ctx context.Context, condition func() bool, label string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", label, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertResearchAcceptance(
	t *testing.T,
	ctx context.Context,
	engine *Engine,
	scope Scope,
	initiativeID string,
	artifactID string,
	actionID string,
) {
	t.Helper()
	initiative, err := engine.GetInitiative(ctx, scope, initiativeID)
	if err != nil || initiative.Status != InitiativeStatusCompleted || len(initiative.RunRefs) != 3 ||
		len(initiative.Deliverables) != 1 || initiative.Deliverables[0].Status != InitiativeDeliverableDelivered {
		t.Fatalf("initiative=%#v err=%v", initiative, err)
	}
	artifact, err := engine.GetArtifact(ctx, scope, artifactID, 1)
	if err != nil || artifact.MediaType != "application/pdf" || len(artifact.Evidence) != 2 ||
		artifact.Provenance.Owner == nil || *artifact.Provenance.Owner != initiative.Owner {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
	approvalValues, err := engine.ListApprovals(ctx, ApprovalFilter{Scope: scope, RunID: initiative.RunRefs[2]})
	if err != nil || len(approvalValues) != 1 || approvalValues[0].Status != ApprovalStatusApproved {
		t.Fatalf("approvals=%#v err=%v", approvalValues, err)
	}
	call, err := engine.GetActionCall(ctx, scope, actionID)
	if err != nil || call.Status != ActionCallStatusSucceeded || call.Output["receiptId"] != "delivery-receipt-1" {
		t.Fatalf("delivery call=%#v err=%v", call, err)
	}
	observations, err := engine.ListSourceObservations(ctx, SourceObservationFilter{
		Scope: scope, InitiativeID: initiative.ID, Limit: 10,
	})
	if err != nil || len(observations) != 2 {
		t.Fatalf("observations=%#v err=%v", observations, err)
	}
	for _, runID := range initiative.RunRefs {
		run, loadErr := engine.GetAgentRun(ctx, scope, runID)
		if loadErr != nil || run.Status != AgentRunStatusCompleted || run.Owner != initiative.Owner {
			t.Fatalf("run=%#v err=%v", run, loadErr)
		}
	}
}
