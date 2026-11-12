package collaboration

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func TestCollaborationReservesTeamDelegationCapacityAtomically(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (models.CollaborationKernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.CollaborationKernelStore, func()) {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "delegation-capacity.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, closeStore := tc.open(t)
			defer closeStore()
			ctx := context.Background()
			scope := models.Scope{Kind: "tenant", ID: "atomic"}
			portfolio := portfolio.NewPortfolioService(store)
			sources := make([]*models.AgentRun, 2)
			for index := range sources {
				var err error
				sources[index], err = portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
					Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, AssignedAgentID: "agent",
					Goal: "Coordinate", Source: models.RunSourceManual, IdempotencyKey: fmt.Sprintf("source-%d", index),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			teamStore := collaborationTeamStoreStub{
				deployment: &kernelteam.Deployment{
					ID: "team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
					DefinitionID: "team-definition", ActiveVersion: "1", Revision: 1,
				},
				definition: &kernelteam.Definition{ID: "team-definition", Version: "1", Delegation: kernelteam.DelegationPolicy{
					MaximumDepth: 2, MaximumConcurrent: 1, AllowPeerDelegation: true,
				}},
			}
			start := make(chan struct{})
			errorsByIndex := make([]error, len(sources))
			var group sync.WaitGroup
			for index, source := range sources {
				group.Add(1)
				go func(index int, source *models.AgentRun) {
					defer group.Done()
					<-start
					service := NewCollaborationService(store)
					service.teams = teamStore
					_, errorsByIndex[index] = service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
						Scope: scope, Kind: models.AgentRequestKindRequest,
						Requester:   models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "agent"},
						Recipient:   models.CollaborationParty{Type: models.OwnerTypeAgent, ID: fmt.Sprintf("reviewer-%d", index)},
						SourceRunID: source.ID, Goal: "Review", IdempotencyKey: fmt.Sprintf("request-%d", index),
					})
				}(index, source)
			}
			close(start)
			group.Wait()
			succeeded, capacityRejected := 0, 0
			for _, err := range errorsByIndex {
				switch {
				case err == nil:
					succeeded++
				case errors.Is(err, models.ErrAgentRequestAssignment) && strings.Contains(err.Error(), "reaching maximum 1"):
					capacityRejected++
				default:
					t.Fatalf("unexpected concurrent creation error: %v", err)
				}
			}
			if succeeded != 1 || capacityRejected != 1 {
				t.Fatalf("concurrent results: success=%d capacity=%d errors=%v", succeeded, capacityRejected, errorsByIndex)
			}
		})
	}
}

func TestCollaborationCompletionReviewSurvivesPortableStoreRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "completion-review.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "review"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, AssignedAgentID: "author",
		Goal: "Produce reviewed work", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	teamStore := collaborationTeamStoreStub{
		deployment: &kernelteam.Deployment{
			ID: "team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
			DefinitionID: "team", ActiveVersion: "1", Revision: 1,
		},
		definition: &kernelteam.Definition{ID: "team", Version: "1", Delegation: kernelteam.DelegationPolicy{
			MaximumDepth: 2, MaximumConcurrent: 2, AllowPeerDelegation: true, RequireAcceptance: true, RequireCompletionReview: true,
		}},
	}
	service := NewCollaborationService(store)
	service.teams = teamStore
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "author"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "specialist"}, SourceRunID: source.ID,
		Goal: "Produce result", IdempotencyKey: "portable-review",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: created.Request.Recipient,
	})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := service.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
		Scope: scope, RequestID: accepted.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: accepted.Request.Recipient,
		Summary: "Portable result", CompletionKey: "portable-completion",
	})
	if err != nil || submitted.Request.Status != models.AgentRequestStatusCompletionReview {
		t.Fatalf("submitted review = %#v, %v", submitted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	reviewed, err := NewCollaborationService(restarted).ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: submitted.Request.ID, ExpectedRevision: submitted.Request.Revision,
		ExpectedChildRevision: submitted.Child.Revision, Principal: submitted.Request.Requester,
		Approve: true, Summary: "Restarted reviewer approved", IdempotencyKey: "portable-review-decision",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Request.Status != models.AgentRequestStatusCompleted || reviewed.Source.Status != models.AgentRunStatusQueued {
		t.Fatalf("review after restart = %#v / %#v", reviewed.Request, reviewed.Source)
	}
}

func TestCollaborationRequestLifecycleAcrossPortableStores(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (models.CollaborationKernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.CollaborationKernelStore, func()) {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "collaboration.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, closeStore := tc.open(t)
			defer closeStore()
			ctx := context.Background()
			scope := models.Scope{Kind: "tenant", ID: "acme"}
			portfolio := portfolio.NewPortfolioService(store)
			source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "product"}, AssignedAgentID: "developer",
				Goal: "Ship the release", Source: models.RunSourceManual,
				Context: map[string]interface{}{"sourceOnly": true, "privateCredentialRef": "binding:must-not-transfer"},
				Budget:  &models.BudgetPolicy{MaxTotalTokens: 1000}, Policy: map[string]interface{}{"risk": "guarded"},
			})
			if err != nil {
				t.Fatal(err)
			}
			service := NewCollaborationService(store)
			create := CreateAgentRequestRequest{
				Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
				Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
				Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
				Goal:      "Turn the release into a launch brief", SemanticRole: "launch-editor",
				AcceptanceCriteria: map[string]interface{}{"format": "markdown"},
				ArtifactRequirements: []models.ArtifactRequirement{{
					Name: "launch-brief", Type: "document", Required: true,
					Schema: map[string]interface{}{
						"type": "object", "required": []interface{}{"wordCount"},
						"properties": map[string]interface{}{"wordCount": map[string]interface{}{"type": "integer", "minimum": 1}},
					},
				}},
				SharedContext:    map[string]interface{}{"release": "2026.07"},
				ChildCheckpoint:  map[string]interface{}{"phase": "draft", "audience": "platform-engineering"},
				ConversationRefs: []string{"team:gtm:42"},
				IdempotencyKey:   "release-launch-brief",
				BudgetAllocation: &models.BudgetPolicy{MaxTotalTokens: 500},
			}
			created, err := service.CreateAgentRequest(ctx, create)
			if err != nil {
				t.Fatal(err)
			}
			if created.Request.Status != models.AgentRequestStatusPending || created.Source == nil ||
				created.Source.Status != models.AgentRunStatusWaitingForAgent || created.Source.WakeCondition == nil ||
				created.Source.WakeCondition.Type != "agent_request_decision" ||
				created.Source.WakeCondition.Reference != created.Request.ID ||
				len(created.Events) != 1 || created.Events[0].TeamID != "product" || created.Events[0].Visibility != models.ActivityVisibilityTeam {
				t.Fatalf("created request = %#v, events = %#v", created.Request, created.Events)
			}
			duplicate, err := service.CreateAgentRequest(ctx, create)
			if err != nil || duplicate.Request.ID != created.Request.ID || len(duplicate.Events) != 0 {
				t.Fatalf("idempotent request = %#v, %v", duplicate, err)
			}
			conflictingIntent := create
			conflictingIntent.SharedContext = map[string]interface{}{"release": "different"}
			if _, err := service.CreateAgentRequest(ctx, conflictingIntent); !errors.Is(err, models.ErrAgentRequestIdempotency) {
				t.Fatalf("conflicting request intent error = %v", err)
			}
			listed, err := service.ListAgentRequests(ctx, models.AgentRequestFilter{Scope: scope, Recipient: &create.Recipient})
			if err != nil || len(listed) != 1 || listed[0].ID != created.Request.ID {
				t.Fatalf("recipient requests = %#v, %v", listed, err)
			}

			question, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1,
				Decision: AgentRequestDecisionRequestClarification, Principal: create.Recipient,
				Message: "Which audience should this prioritize?",
			})
			if err != nil || question.Request.Status != models.AgentRequestStatusClarificationRequested || question.Child != nil {
				t.Fatalf("clarification request = %#v, %v", question, err)
			}
			clarified, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 2,
				Decision: AgentRequestDecisionProvideClarification, Principal: create.Requester,
				Message: "Prioritize existing platform engineering customers.",
			})
			if err != nil || clarified.Request.Status != models.AgentRequestStatusPending || clarified.Request.Revision != 3 {
				t.Fatalf("clarification response = %#v, %v", clarified, err)
			}
			accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 3,
				Decision: AgentRequestDecisionAccept, Principal: create.Recipient, Message: "I will draft and return the brief.",
			})
			if err != nil {
				t.Fatal(err)
			}
			if accepted.Request.Status != models.AgentRequestStatusAccepted || accepted.Child == nil || accepted.Child.ParentRunID != source.ID || accepted.Child.RootRunID != source.RootRunID {
				t.Fatalf("accepted request = %#v", accepted)
			}
			if accepted.Request.AcceptedAt == nil || accepted.Request.ResolvedAt != nil {
				t.Fatalf("accepted request timestamps = %#v", accepted.Request)
			}
			if accepted.Child.Owner != source.Owner || accepted.Child.AssignedAgentID != "marketing" || accepted.Child.Source != models.RunSourceRequest {
				t.Fatalf("child ownership = %#v", accepted.Child)
			}
			if accepted.Child.Context["sourceOnly"] != nil || accepted.Child.Context["privateCredentialRef"] != nil || accepted.Child.Context["release"] != "2026.07" {
				t.Fatalf("child context leaked source state: %#v", accepted.Child.Context)
			}
			if accepted.Child.Checkpoint["phase"] != "draft" || accepted.Child.Checkpoint["audience"] != "platform-engineering" {
				t.Fatalf("child checkpoint = %#v", accepted.Child.Checkpoint)
			}
			persistedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
			if err != nil || persistedSource.Status != models.AgentRunStatusWaitingForDependency || persistedSource.WakeCondition == nil || persistedSource.WakeCondition.Reference != created.Request.ID {
				t.Fatalf("source after acceptance = %#v, %v", persistedSource, err)
			}
			child, err := portfolio.GetAgentRun(ctx, scope, accepted.Child.ID)
			if err != nil || child.AssignedAgentID != "marketing" {
				t.Fatalf("persisted child = %#v, %v", child, err)
			}
			_, err = artifactcatalog.NewArtifactCatalog(store).Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: &models.Artifact{
				ID: "artifact-launch-brief", Version: 1, Scope: scope, Name: "launch-brief.md",
				Type: "document", MediaType: "text/markdown", ContentRef: "artifact-store:launch-brief-v1",
				Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SizeBytes: 2048,
				Classification: models.ArtifactClassificationInternal, Metadata: map[string]interface{}{"wordCount": 420},
				Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: "marketing"}, RunID: child.ID, RequestID: created.Request.ID},
				Evidence:   []models.EvidenceLink{{Relation: models.EvidenceRelationSupports, TargetKind: models.EvidenceTargetClaim, TargetRef: "review:editorial-7"}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			completion := CompleteAgentRequestRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: accepted.Request.Revision,
				ExpectedChildRevision: child.Revision, Principal: create.Recipient,
				Actor:              models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
				Summary:            "Delivered the launch brief with audience-specific messaging.",
				AcceptanceEvidence: map[string]interface{}{"format": "markdown", "review": "passed"},
				Artifacts: []models.ArtifactReference{{
					ID: "artifact-launch-brief", Version: 1, RequirementName: "launch-brief",
				}},
				CompletionKey: "complete-launch-brief-v1",
			}
			completed, err := service.CompleteAgentRequest(ctx, completion)
			if err != nil {
				t.Fatal(err)
			}
			if completed.Request.Status != models.AgentRequestStatusCompleted || completed.Request.CompletedAt == nil || completed.Source.Status != models.AgentRunStatusQueued || completed.Child.Status != models.AgentRunStatusCompleted {
				t.Fatalf("completed request = %#v", completed)
			}
			if len(completed.Request.Artifacts) != 1 || completed.Request.Artifacts[0].ContentRef != "artifact-store:launch-brief-v1" || completed.Source.WakeCondition != nil {
				t.Fatalf("completion artifacts/source = %#v / %#v", completed.Request.Artifacts, completed.Source)
			}
			idempotentCompletion, err := service.CompleteAgentRequest(ctx, completion)
			if err != nil || idempotentCompletion.Request.Revision != completed.Request.Revision || len(idempotentCompletion.Events) != 0 {
				t.Fatalf("idempotent completion = %#v, %v", idempotentCompletion, err)
			}
			conflictingCompletion := completion
			conflictingCompletion.Summary = "A different result under the same key"
			if _, err := service.CompleteAgentRequest(ctx, conflictingCompletion); !errors.Is(err, models.ErrAgentRequestIdempotency) {
				t.Fatalf("conflicting completion error = %v", err)
			}
			activity, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, TeamID: "product", Descending: true, Limit: 20})
			if err != nil || len(activity) != 7 || activity[0].EventType != "collaboration.completed" {
				t.Fatalf("team activity = %#v, %v", activity, err)
			}
		})
	}
}

func TestCollaborationHandoffTransfersOwnership(t *testing.T) {
	t.Parallel()
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship feature", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	service.teams = activeCollaborationTeam(scope, "gtm",
		kernelteam.RosterAssignment{ID: "marketer", AgentDeploymentID: "marketing-agent"},
		kernelteam.RosterAssignment{ID: "marketer-two", AgentDeploymentID: "marketing-agent-two"})
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindHandoff, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "gtm"}, Goal: "Own launch and lead follow-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1,
		Decision: AgentRequestDecisionAccept, Principal: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "gtm"},
	}); err == nil || !strings.Contains(err.Error(), "explicit assigned Agent") {
		t.Fatalf("missing Team assignment error = %v", err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1,
		Decision: AgentRequestDecisionAccept, Principal: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marketing-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Request.AssignedAgentID != "marketing-agent" || accepted.Child.Owner != (models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "gtm"}) || accepted.Child.AssignedAgentID != "marketing-agent" || accepted.Child.Source != models.RunSourceHandoff {
		t.Fatalf("handoff child = %#v", accepted.Child)
	}
	persistedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
	if err != nil || persistedSource.Status != models.AgentRunStatusCompleted || persistedSource.CompletedAt == nil {
		t.Fatalf("handoff source = %#v, %v", persistedSource, err)
	}
	activity, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, TeamID: "gtm", Descending: true, Limit: 10})
	if err != nil || len(activity) != 3 {
		t.Fatalf("handoff team activity = %#v, %v", activity, err)
	}
}

func TestAgentRequestSourceLifecycleIsEnforced(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		run  *models.AgentRun
		want bool
	}{
		{name: "queued", run: &models.AgentRun{Status: models.AgentRunStatusQueued}, want: true},
		{name: "planning", run: &models.AgentRun{Status: models.AgentRunStatusPlanning}, want: true},
		{name: "running", run: &models.AgentRun{Status: models.AgentRunStatusRunning}, want: true},
		{name: "already waiting", run: &models.AgentRun{Status: models.AgentRunStatusRunning, WakeCondition: &models.WakeCondition{Type: "event"}}},
		{name: "paused", run: &models.AgentRun{Status: models.AgentRunStatusPaused}},
		{name: "completed", run: &models.AgentRun{Status: models.AgentRunStatusCompleted}},
		{name: "failed", run: &models.AgentRun{Status: models.AgentRunStatusFailed}},
		{name: "canceled", run: &models.AgentRun{Status: models.AgentRunStatusCanceled}},
		{name: "missing run"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CanStartAgentRequestFromRun(test.run); got != test.want {
				t.Fatalf("CanStartAgentRequestFromRun() = %t, want %t", got, test.want)
			}
		})
	}

	store := kerneltest.NewStore(t)
	portfolio := portfolio.NewPortfolioService(store)
	activity := runtimeactivity.NewRunActivityService(store, store)
	service := NewCollaborationService(store)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}
	recipient := models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"}

	terminal, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Already shipped", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _, err = activity.TransitionRun(ctx, scope, terminal.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: terminal.Revision, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _, err = activity.TransitionRun(ctx, scope, terminal.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: terminal.Revision, Status: models.AgentRunStatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []models.AgentRequestKind{models.AgentRequestKindRequest, models.AgentRequestKindHandoff, models.AgentRequestKindEscalation} {
		_, createErr := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
			Scope: scope, Kind: kind, SourceRunID: terminal.ID,
			Requester: models.CollaborationParty(owner), Recipient: recipient, Goal: "Start follow-up",
		})
		if !errors.Is(createErr, models.ErrInvalidAgentRequestState) || !strings.Contains(createErr.Error(), "completed") {
			t.Fatalf("create %s from terminal source error = %v", kind, createErr)
		}
	}

	active, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Ship next release", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: active.ID,
		Requester: models.CollaborationParty(owner), Recipient: recipient, Goal: "Prepare the release brief",
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err = portfolio.GetAgentRun(ctx, scope, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	active, _, err = activity.TransitionRun(ctx, scope, active.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: active.Revision, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = activity.TransitionRun(ctx, scope, active.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: active.Revision, Status: models.AgentRunStatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: recipient,
	})
	if !errors.Is(err, models.ErrInvalidAgentRequestState) || !strings.Contains(err.Error(), "completed") {
		t.Fatalf("accept after source completion error = %v", err)
	}
	restored, err := service.GetAgentRequest(ctx, scope, created.Request.ID)
	if err != nil || restored.Status != models.AgentRequestStatusPending || restored.ChildRunID != "" {
		t.Fatalf("request mutated after rejected acceptance: %#v, %v", restored, err)
	}
}

func TestCollaborationRejectsCredentialTransferAndUnauthorizedRequesters(t *testing.T) {
	t.Parallel()
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "work", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	base := CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"}, Goal: "draft launch",
	}
	unsafe := base
	unsafe.SharedContext = map[string]interface{}{"nested": map[string]interface{}{"api_token": "secret"}}
	if _, err := service.CreateAgentRequest(ctx, unsafe); !errors.Is(err, models.ErrUnsafeSharedContext) {
		t.Fatalf("unsafe context error = %v", err)
	}
	unsafeCheckpoint := base
	unsafeCheckpoint.ChildCheckpoint = map[string]interface{}{"nested": map[string]interface{}{"accessToken": "secret"}}
	if _, err := service.CreateAgentRequest(ctx, unsafeCheckpoint); !errors.Is(err, models.ErrUnsafeSharedContext) {
		t.Fatalf("unsafe child checkpoint error = %v", err)
	}
	unauthorized := base
	unauthorized.Requester = models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "intruder"}
	if _, err := service.CreateAgentRequest(ctx, unauthorized); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("unauthorized requester error = %v", err)
	}
}

func TestCollaborationCompletionRejectsInvalidAuthorityArtifactsAndEvidence(t *testing.T) {
	t.Parallel()
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "developer",
		Goal: "Launch", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	service.teams = activeCollaborationTeam(scope, "marketing", kernelteam.RosterAssignment{ID: "editor", AgentDeploymentID: "marketing-editor"})
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "gtm"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "marketing"}, Goal: "Publish launch brief",
		AcceptanceCriteria: map[string]interface{}{"reviewed": true},
		ArtifactRequirements: []models.ArtifactRequirement{{
			Name: "brief", Type: "document", Required: true,
			Schema: map[string]interface{}{"type": "object", "required": []interface{}{"reviewed"}, "properties": map[string]interface{}{"reviewed": map[string]interface{}{"const": true}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1, Decision: AgentRequestDecisionAccept,
		Principal: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Request.AssignedAgentID != "marketing-editor" || accepted.Child.AssignedAgentID != "marketing-editor" {
		t.Fatalf("automatic unique-role assignment = %#v", accepted)
	}
	for _, artifact := range []*models.Artifact{
		{
			ID: "brief-1", Version: 1, Scope: scope, Name: "brief.md", Type: "document",
			ContentRef: "artifact-store:brief-1", Digest: collaborationDigest("brief-1"), Classification: models.ArtifactClassificationInternal,
			Metadata:   map[string]interface{}{"reviewed": true},
			Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: "marketing-editor"}, RunID: accepted.Child.ID, RequestID: created.Request.ID},
		},
		{
			ID: "brief-bad-schema", Version: 1, Scope: scope, Name: "brief.md", Type: "document",
			ContentRef: "artifact-store:brief-bad-schema", Digest: collaborationDigest("brief-bad-schema"), Classification: models.ArtifactClassificationInternal,
			Metadata:   map[string]interface{}{"reviewed": false},
			Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: "marketing-editor"}, RunID: accepted.Child.ID, RequestID: created.Request.ID},
		},
	} {
		if _, err := artifactcatalog.NewArtifactCatalog(store).Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: artifact}); err != nil {
			t.Fatal(err)
		}
	}
	base := CompleteAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "marketing"},
		Actor: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing-editor"}, Summary: "Brief ready",
		AcceptanceEvidence: map[string]interface{}{"reviewed": true}, CompletionKey: "brief-v1",
		Artifacts: []models.ArtifactReference{{
			ID: "brief-1", Version: 1, RequirementName: "brief",
		}},
	}
	unauthorized := base
	unauthorized.Principal = models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing-editor"}
	if _, err := service.CompleteAgentRequest(ctx, unauthorized); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("unauthorized completion error = %v", err)
	}
	missingEvidence := base
	missingEvidence.AcceptanceEvidence = nil
	if _, err := service.CompleteAgentRequest(ctx, missingEvidence); err == nil || !strings.Contains(err.Error(), "acceptance evidence") {
		t.Fatalf("missing evidence error = %v", err)
	}
	unsafeURL := base
	unsafeURL.Artifacts = models.CloneArtifactReferences(base.Artifacts)
	unsafeURL.Artifacts[0].ContentRef = "https://store.test/brief?token=secret"
	if _, err := service.CompleteAgentRequest(ctx, unsafeURL); err == nil || !strings.Contains(err.Error(), "only id, version") {
		t.Fatalf("unsafe artifact reference error = %v", err)
	}
	badSchema := base
	badSchema.Artifacts = models.CloneArtifactReferences(base.Artifacts)
	badSchema.Artifacts[0].ID = "brief-bad-schema"
	if _, err := service.CompleteAgentRequest(ctx, badSchema); err == nil || !strings.Contains(err.Error(), "JSON schema") {
		t.Fatalf("artifact schema error = %v", err)
	}
	tamperedMetadata := base
	tamperedMetadata.Artifacts = models.CloneArtifactReferences(base.Artifacts)
	tamperedMetadata.Artifacts[0].Metadata = map[string]interface{}{"reviewed": true}
	if _, err := service.CompleteAgentRequest(ctx, tamperedMetadata); err == nil || !strings.Contains(err.Error(), "only id, version") {
		t.Fatalf("tampered artifact metadata error = %v", err)
	}
	unregistered := base
	unregistered.Artifacts = []models.ArtifactReference{{ID: "missing", Version: 1, RequirementName: "brief"}}
	if _, err := service.CompleteAgentRequest(ctx, unregistered); !errors.Is(err, artifactcatalog.ErrArtifactNotFound) {
		t.Fatalf("unregistered artifact error = %v", err)
	}
	completed, err := service.CompleteAgentRequest(ctx, base)
	if err != nil || completed.Request.Status != models.AgentRequestStatusCompleted || completed.Request.Artifacts[0].ID != "brief-1" {
		t.Fatalf("valid Team completion = %#v, %v", completed, err)
	}
}

func TestSQLiteCollaborationSurvivesRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "collaboration-restart.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "work", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewCollaborationService(store).CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "analyst"}, Goal: "measure launch impact",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := NewCollaborationService(reopened).GetAgentRequest(ctx, scope, created.Request.ID)
	if err != nil || restored.Goal != "measure launch impact" || restored.Revision != 1 {
		t.Fatalf("restored request = %#v, %v", restored, err)
	}
}

func TestTerminalFailedHandoffChildResolvesRequestAndSource(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Build the feature", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindHandoff, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"}, Goal: "Publish the launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := runtimeactivity.NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, scope, accepted.Child.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: accepted.Child.Revision, Status: models.AgentRunStatusRunning,
		Summary: "Delegated run started", Actor: models.ActivityActor{Type: "worker", ID: "worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := activity.TransitionRun(ctx, scope, accepted.Child.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusFailed, Error: "provider exhausted retries",
		Summary: "Delegated run failed", Actor: models.ActivityActor{Type: "worker", ID: "worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveTerminalAgentRequestChild(ctx, failed)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Request.Status != models.AgentRequestStatusFailed || resolved.Request.ResolutionReason != "provider exhausted retries" {
		t.Fatalf("resolved request = %#v", resolved.Request)
	}
	refreshedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	results, _ := refreshedSource.Output["collaborationResults"].(map[string]interface{})
	result, _ := results[created.Request.ID].(map[string]interface{})
	if refreshedSource.Status != models.AgentRunStatusCompleted || result["status"] != string(models.AgentRequestStatusFailed) ||
		result["reason"] != "provider exhausted retries" {
		t.Fatalf("source=%#v result=%#v", refreshedSource, result)
	}
}

func TestTerminalChildCompletesArtifactBearingRequestFromRegisteredOutput(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "artifact-delegation"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship the release", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"}, Goal: "Create the launch report",
		ArtifactRequirements: []models.ArtifactRequirement{{Name: "launch-report", Type: "report", Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = artifactcatalog.NewArtifactCatalog(store).Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: &models.Artifact{
		ID: "launch-report", Version: 1, Scope: scope, Name: "launch-report.pdf", Type: "report", MediaType: "application/pdf",
		ContentRef: "artifact-store:launch-report", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes: 4096, Classification: models.ArtifactClassificationInternal,
		Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: "marketing"}, RunID: accepted.Child.ID, RequestID: created.Request.ID},
	}})
	if err != nil {
		t.Fatal(err)
	}
	activity := runtimeactivity.NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, scope, accepted.Child.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: accepted.Child.Revision, Status: models.AgentRunStatusRunning,
		Summary: "Creating report", Actor: models.ActivityActor{Type: "worker", ID: "worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completedChild, _, err := activity.TransitionRun(ctx, scope, accepted.Child.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusCompleted,
		Summary: "Report created", Actor: models.ActivityActor{Type: "worker", ID: "worker-1"},
		Output: map[string]interface{}{
			"summary":      "Delivered the launch report.",
			"artifactRefs": []interface{}{map[string]interface{}{"id": "launch-report", "version": 1, "requirementName": "launch-report"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveTerminalAgentRequestChild(ctx, completedChild)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Request.Status != models.AgentRequestStatusCompleted || len(resolved.Request.Artifacts) != 1 ||
		resolved.Request.Artifacts[0].ID != "launch-report" || resolved.Request.Artifacts[0].ContentRef != "artifact-store:launch-report" ||
		resolved.Source.Status != models.AgentRunStatusQueued {
		t.Fatalf("artifact-bearing resolution = %#v", resolved)
	}
}
