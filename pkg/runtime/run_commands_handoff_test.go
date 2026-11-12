package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type handoffCommandTestStore interface {
	models.KernelStore
	models.CollaborationKernelStore
}

func forHandoffCommandStores(t *testing.T, test func(*testing.T, handoffCommandTestStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "handoff.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		test(t, store)
	})
}

type handoffCommandFixture struct {
	t             *testing.T
	store         handoffCommandTestStore
	scope         models.Scope
	now           time.Time
	portfolio     *portfolio.PortfolioService
	collaboration *CollaborationService
	commands      *agentrun.RunCommandService
	sourceID      string
	childID       string
	requestID     string
}

func newHandoffCommandFixture(t *testing.T, store handoffCommandTestStore, kind models.AgentRequestKind, teamOwned, review bool) *handoffCommandFixture {
	t.Helper()
	return newHandoffCommandFixtureWithParent(t, store, kind, teamOwned, review, "", true)
}

func newHandoffCommandFixtureWithParent(t *testing.T, store handoffCommandTestStore, kind models.AgentRequestKind, teamOwned, review bool, parentID string, accept bool) *handoffCommandFixture {
	t.Helper()
	f := &handoffCommandFixture{
		t: t, store: store, scope: models.Scope{Kind: "tenant", ID: "handoff-ownership"},
		now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
	}
	clock := models.WithClock(func() time.Time { return f.now })
	f.portfolio, f.collaboration, f.commands = portfolio.NewPortfolioService(store, clock), NewCollaborationService(store, clock), agentrun.NewRunCommandService(store, clock)
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}
	if teamOwned {
		owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "delivery-team"}
		f.collaboration.teams = collaborationTeamStoreStub{
			deployment: &kernelteam.Deployment{
				ID: owner.ID, Scope: capability.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID},
				DefinitionID: owner.ID, ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 1,
			},
			definition: &kernelteam.Definition{ID: owner.ID, Version: "1", Delegation: kernelteam.DelegationPolicy{
				MaximumDepth: 3, MaximumConcurrent: 3, AllowPeerDelegation: true,
				RequireAcceptance: true, RequireCompletionReview: review,
			}},
		}
	}
	source, err := f.portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: f.scope, ParentRunID: parentID, Owner: owner, AssignedAgentID: "developer", Goal: "Build the feature", Source: models.RunSourceManual,
		Checkpoint: map[string]interface{}{"sourceProgress": "implementation done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.sourceID = source.ID
	created, err := f.collaboration.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
		Scope: f.scope, Kind: kind, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
		Goal:      "Prepare the launch follow-up", IdempotencyKey: "launch-handoff",
		AcceptanceCriteria: map[string]interface{}{"required": "one follow-up"},
		ChildCheckpoint:    map[string]interface{}{"recipientProgress": "accepted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.requestID = created.Request.ID
	if !accept {
		return f
	}
	accepted, err := f.collaboration.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{
		Scope: f.scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: created.Request.Recipient,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.childID, f.requestID = accepted.Child.ID, accepted.Request.ID
	return f
}

func (f *handoffCommandFixture) run(id string) *models.AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.t.Context(), f.scope, id)
	if err != nil || run == nil {
		f.t.Fatalf("load run %s: %#v, %v", id, run, err)
	}
	return run
}

func (f *handoffCommandFixture) request() *models.AgentRequest {
	f.t.Helper()
	request, err := f.store.GetAgentRequest(f.t.Context(), f.scope, f.requestID)
	if err != nil || request == nil {
		f.t.Fatalf("load request: %#v, %v", request, err)
	}
	return request
}

func (f *handoffCommandFixture) childOf(parentID, agentID string) *models.AgentRun {
	f.t.Helper()
	run, err := f.portfolio.CreateAgentRun(f.t.Context(), models.CreateAgentRunRequest{
		Scope: f.scope, ParentRunID: parentID, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: agentID},
		AssignedAgentID: agentID, Goal: "Owned follow-up", Source: models.RunSourceManual,
		Checkpoint: map[string]interface{}{"ownedProgress": "not started"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.run(run.ID)
}

func (f *handoffCommandFixture) transition(id string, status models.AgentRunStatus, output map[string]interface{}) *models.AgentRun {
	f.t.Helper()
	run := f.run(id)
	activity := runtimeactivity.NewRunActivityService(f.store, f.store, models.WithClock(func() time.Time { return f.now }))
	updated, _, err := activity.TransitionRun(f.t.Context(), f.scope, id, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, Output: output, LeaseOwner: run.LeaseOwner,
		Actor: models.ActivityActor{Type: "test", ID: "handoff-worker"}, Summary: "Advance fixture run",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return updated
}

func (f *handoffCommandFixture) claimChild() *models.AgentRun {
	f.t.Helper()
	run, err := f.store.ClaimNextAgentRun(f.t.Context(), models.AgentRunClaim{
		Scope: f.scope, WorkerID: "marketing-worker", AssignedAgentID: "marketing", Now: f.now,
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || run == nil || run.ID != f.childID || run.Status != models.AgentRunStatusRunning {
		f.t.Fatalf("claim transferred child: %#v, %v", run, err)
	}
	return f.run(run.ID)
}

func (f *handoffCommandFixture) assertUnchanged(before *models.AgentRun) {
	f.t.Helper()
	if after := f.run(before.ID); !reflect.DeepEqual(before, after) {
		f.t.Fatalf("run %s changed during source cleanup:\nbefore=%#v\nafter=%#v", before.ID, before, after)
	}
}

func (f *handoffCommandFixture) assertCanceled(id string) {
	f.t.Helper()
	run := f.run(id)
	if run.Status != models.AgentRunStatusCanceled || run.LeaseOwner != "" || run.LeaseExpiresAt != nil {
		f.t.Fatalf("owned run was not canceled and released: %#v", run)
	}
}

func (f *handoffCommandFixture) events(id string) []*models.ActivityEvent {
	f.t.Helper()
	events, err := f.store.ListActivity(f.t.Context(), models.ActivityFilter{Scope: f.scope, RunID: id, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	return events
}

// These mutations deliberately corrupt one durable authority fact at a time.
// They use the canonical CAS write contracts, rather than changing the pointer
// passed to CascadeTerminalRun or bypassing a backend's durable records.
func (f *handoffCommandFixture) mutateRequest(mutate func(*models.AgentRequest)) {
	f.t.Helper()
	request := f.request()
	revision := request.Revision
	mutate(request)
	request.Revision++
	request.UpdatedAt = f.now
	_, err := f.store.CoordinateAgentRequest(f.t.Context(), models.AgentRequestCoordinationRecord{
		Request: request, ExpectedRequestRevision: revision,
		Event: &models.ActivityEvent{
			ID: "corrupt-request-proof", Scope: request.Scope, RunID: request.SourceRunID,
			EventType: "collaboration.test_mutated", Actor: models.ActivityActor{Type: "test", ID: "fixture"},
			Summary: "Corrupt one durable handoff proof", Severity: models.ActivitySeverityInfo,
			Visibility: models.ActivityVisibilityScope, CreatedAt: f.now,
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *handoffCommandFixture) mutateRun(id string, mutate func(*models.AgentRun)) {
	f.t.Helper()
	run := f.run(id)
	revision := run.Revision
	mutate(run)
	run.Revision++
	run.UpdatedAt = f.now
	_, err := f.store.UpdateAgentRunWithEvent(f.t.Context(), run, revision, &models.ActivityEvent{
		ID: "corrupt-run-proof", Scope: run.Scope, RunID: run.ID, EventType: "run.test_mutated",
		Actor: models.ActivityActor{Type: "test", ID: "fixture"}, Summary: "Corrupt one durable handoff proof",
		Severity: models.ActivitySeverityInfo, Visibility: models.ActivityVisibilityScope, CreatedAt: f.now,
	}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestRunCommandsHandoffTransfersCancellationOwnershipAndCompletes(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "queued"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
				f := newHandoffCommandFixture(t, store, models.AgentRequestKindHandoff, false, false)
				if running {
					f.claimChild()
				}
				source, child := f.run(f.sourceID), f.run(f.childID)
				grandchild := f.childOf(child.ID, "marketing-helper")
				sibling := f.childOf(source.ID, "developer-helper")
				requestBefore := f.request()
				if source.Status != models.AgentRunStatusCompleted || child.ParentRunID != source.ID ||
					child.RootRunID != source.RootRunID || child.Owner.ID != "marketing" || child.Source != models.RunSourceHandoff {
					t.Fatalf("invalid accepted handoff fixture: source=%#v child=%#v", source, child)
				}
				if err := f.commands.CascadeTerminalRun(t.Context(), source); err != nil {
					t.Fatal(err)
				}
				f.assertUnchanged(source)
				f.assertUnchanged(child)
				f.assertUnchanged(grandchild)
				f.assertCanceled(sibling.ID)
				if !reflect.DeepEqual(requestBefore, f.request()) {
					t.Fatal("source cleanup changed the handoff receipt")
				}
				siblingAfter, siblingEvents := f.run(sibling.ID), f.events(sibling.ID)
				if err := f.commands.CascadeTerminalRun(t.Context(), source); err != nil {
					t.Fatal(err)
				}
				f.assertUnchanged(child)
				f.assertUnchanged(grandchild)
				f.assertUnchanged(siblingAfter)
				if !reflect.DeepEqual(siblingEvents, f.events(sibling.ID)) {
					t.Fatal("repeated source cleanup added cancellation activity")
				}
				if !running {
					f.claimChild()
				}
				f.transition(child.ID, models.AgentRunStatusCompleted, map[string]interface{}{"followUp": "Publish the launch note"})
				resolved, err := f.collaboration.ResolveTerminalAgentRequestChild(t.Context(), f.run(child.ID))
				if err != nil || resolved == nil || resolved.Request.Status != models.AgentRequestStatusCompleted {
					t.Fatalf("artifact-free completion: %#v, %v", resolved, err)
				}
				request := f.request()
				if len(request.Artifacts) != 0 || request.CompletionSummary != "Publish the launch note" || request.AcceptanceEvidence["runOutput"] == nil {
					t.Fatalf("handoff completion receipt: %#v", request)
				}
				completedSource, completedChild := f.run(source.ID), f.run(child.ID)
				if completedChild.ParentRunID != source.ID || completedChild.RootRunID != source.RootRunID || completedSource.Status != models.AgentRunStatusCompleted {
					t.Fatal("completion lost transferred lineage or revived source")
				}
				if err := f.commands.CascadeTerminalRun(t.Context(), completedSource); err != nil {
					t.Fatal(err)
				}
				f.assertUnchanged(completedSource)
				f.assertUnchanged(completedChild)
				f.assertUnchanged(grandchild)
			})
		})
	}
}

func TestRunCommandsHandoffDirectCancellationStillOwnsSubtree(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		f := newHandoffCommandFixture(t, store, models.AgentRequestKindHandoff, false, false)
		child := f.claimChild()
		grandchild := f.childOf(child.ID, "marketing-helper")
		source := f.run(f.sourceID)
		if _, err := f.commands.CommandAgentRun(t.Context(), agentrun.AgentRunCommandRequest{
			Scope: f.scope, RunID: child.ID, ExpectedRevision: child.Revision, Kind: agentrun.AgentRunCommandCancel,
			Actor: models.ActivityActor{Type: "user", ID: "recipient"},
		}); err != nil {
			t.Fatal(err)
		}
		f.assertCanceled(child.ID)
		f.assertCanceled(grandchild.ID)
		f.assertUnchanged(source)
		childAfter, descendantAfter := f.run(child.ID), f.run(grandchild.ID)
		if err := f.commands.CascadeTerminalRun(t.Context(), source); err != nil {
			t.Fatal(err)
		}
		if err := f.commands.CascadeTerminalRun(t.Context(), childAfter); err != nil {
			t.Fatal(err)
		}
		f.assertUnchanged(childAfter)
		f.assertUnchanged(descendantAfter)
	})
}

func TestRunCommandsHandoffRequiresExactDurableTransferProof(t *testing.T) {
	tests := []struct {
		name    string
		request func(*models.AgentRequest)
		source  func(*models.AgentRun)
		child   func(*models.AgentRun)
	}{
		{name: "requested", request: func(r *models.AgentRequest) { r.Status = models.AgentRequestStatusPending }},
		{name: "rejected", request: func(r *models.AgentRequest) { r.Status = models.AgentRequestStatusRejected }},
		{name: "canceled", request: func(r *models.AgentRequest) { r.Status = models.AgentRequestStatusCanceled }},
		{name: "failed", request: func(r *models.AgentRequest) { r.Status = models.AgentRequestStatusFailed }},
		{name: "unaccepted", request: func(r *models.AgentRequest) { r.AcceptedAt = nil }},
		{name: "zero acceptance time", request: func(r *models.AgentRequest) { r.AcceptedAt = &time.Time{} }},
		{name: "ordinary request", request: func(r *models.AgentRequest) { r.Kind = models.AgentRequestKindRequest }},
		{name: "wrong source", request: func(r *models.AgentRequest) { r.SourceRunID = "another-source" }},
		{name: "wrong child", request: func(r *models.AgentRequest) { r.ChildRunID = "another-child" }},
		{name: "wrong requester", request: func(r *models.AgentRequest) { r.Requester.ID = "another-requester" }},
		{name: "wrong recipient", request: func(r *models.AgentRequest) { r.Recipient.ID = "another-recipient" }},
		{name: "wrong assignment", request: func(r *models.AgentRequest) { r.AssignedAgentID = "another-agent" }},
		{name: "no assignment", request: func(r *models.AgentRequest) { r.AssignedAgentID = "" }},
		{name: "group owned", request: func(r *models.AgentRequest) {
			r.DependencyGroupID, r.DependencyID = "owned-group", "owned-edge"
		}},
		{name: "review policy", request: func(r *models.AgentRequest) {
			r.DelegationPolicy = &models.AgentRequestDelegationPolicy{
				TeamDeploymentID: "team", TeamDefinitionID: "team", TeamDefinitionVersion: "1",
				DelegationDepth: 1, RequireCompletionReview: true,
			}
		}},
		{name: "missing request", source: func(r *models.AgentRun) { r.Output["handoffRequestId"] = "missing-request" }},
		{name: "wrong output child", source: func(r *models.AgentRun) { r.Output["childRunId"] = "another-child" }},
		{name: "non-handoff child", child: func(r *models.AgentRun) { r.Source = models.RunSourceRequest }},
		{name: "wrong child root", child: func(r *models.AgentRun) { r.RootRunID = "another-root" }},
		{name: "wrong child owner", child: func(r *models.AgentRun) { r.Owner.ID = "another-owner" }},
		{name: "failed source", source: func(r *models.AgentRun) { r.Status = models.AgentRunStatusFailed }},
		{name: "canceled source", source: func(r *models.AgentRun) { r.Status = models.AgentRunStatusCanceled }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
				f := newHandoffCommandFixture(t, store, models.AgentRequestKindHandoff, false, false)
				grandchild := f.childOf(f.childID, "marketing-helper")
				if tc.request != nil {
					f.mutateRequest(tc.request)
				}
				if tc.source != nil {
					f.mutateRun(f.sourceID, tc.source)
				}
				if tc.child != nil {
					f.mutateRun(f.childID, tc.child)
				}
				source, receipt := f.run(f.sourceID), f.request()
				if err := f.commands.CascadeTerminalRun(t.Context(), source); err != nil {
					t.Fatal(err)
				}
				f.assertCanceled(f.childID)
				f.assertCanceled(grandchild.ID)
				f.assertUnchanged(source)
				if !reflect.DeepEqual(receipt, f.request()) {
					t.Fatal("cleanup rewrote invalid request proof")
				}
			})
		})
	}
}

func TestRunCommandsHandoffIgnoresForgedCallerFacts(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		f := newHandoffCommandFixture(t, store, models.AgentRequestKindHandoff, false, false)
		child := f.run(f.childID)
		// A stale caller can omit the receipt or supply another status; neither
		// changes the authoritative completed source's transferred ownership.
		forged := f.run(f.sourceID)
		forged.Status, forged.Output = models.AgentRunStatusCanceled, nil
		if err := f.commands.CascadeTerminalRun(t.Context(), forged); err != nil {
			t.Fatal(err)
		}
		f.assertUnchanged(child)
		// Conversely, an invented receipt on the pointer cannot protect work
		// after the durable source no longer has that proof.
		f.mutateRun(f.sourceID, func(r *models.AgentRun) { r.Output = nil })
		forged = f.run(f.sourceID)
		forged.Output = map[string]interface{}{"handoffRequestId": f.requestID, "childRunId": f.childID}
		if err := f.commands.CascadeTerminalRun(t.Context(), forged); err != nil {
			t.Fatal(err)
		}
		f.assertCanceled(f.childID)
	})
}

func TestRunCommandsHandoffCannotForgeTerminalParentOrForeignScope(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		f := newHandoffCommandFixture(t, store, models.AgentRequestKindHandoff, false, false)
		transferredBefore := f.run(f.childID)
		live := f.childOf(f.sourceID, "live-owner")
		owned := f.childOf(live.ID, "owned-agent")
		before := f.events(owned.ID)
		forged := models.CloneAgentRun(live)
		forged.Status = models.AgentRunStatusCompleted
		if err := f.commands.CascadeTerminalRun(t.Context(), forged); !errors.Is(err, models.ErrInvalidRunTransition) {
			t.Fatalf("forged terminal status: %v", err)
		}
		f.assertUnchanged(live)
		f.assertUnchanged(owned)
		forged = f.run(f.sourceID)
		forged.Scope.ID = "another-tenant"
		if err := f.commands.CascadeTerminalRun(t.Context(), forged); !errors.Is(err, models.ErrRunNotFound) {
			t.Fatalf("foreign scoped parent: %v", err)
		}
		f.assertUnchanged(transferredBefore)
		if !reflect.DeepEqual(before, f.events(owned.ID)) {
			t.Fatal("rejected forged parent changed owned activity")
		}
	})
}

func TestRunCommandsHandoffOrdinaryDelegationAndReviewRemainOwned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   models.AgentRequestKind
		review bool
	}{{"ordinary request", models.AgentRequestKindRequest, false}, {"handoff awaiting review", models.AgentRequestKindHandoff, true}} {
		t.Run(tc.name, func(t *testing.T) {
			forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
				f := newHandoffCommandFixture(t, store, tc.kind, tc.review, tc.review)
				grandchild := f.childOf(f.childID, "marketing-helper")
				if source := f.run(f.sourceID); source.Status != models.AgentRunStatusWaitingForDependency {
					t.Fatalf("source should retain ownership while waiting: %#v", source)
				}
				f.transition(f.sourceID, models.AgentRunStatusCanceled, nil)
				forged := f.run(f.sourceID)
				forged.Status = models.AgentRunStatusCompleted
				forged.Output = map[string]interface{}{"handoffRequestId": f.requestID, "childRunId": f.childID}
				if err := f.commands.CascadeTerminalRun(t.Context(), forged); err != nil {
					t.Fatal(err)
				}
				f.assertCanceled(f.childID)
				f.assertCanceled(grandchild.ID)
			})
		})
	}
}

func TestRunCommandsHandoffRecognizesTeamAssignedRequester(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		f := newHandoffCommandFixture(t, store, models.AgentRequestKindHandoff, true, false)
		source, child := f.run(f.sourceID), f.run(f.childID)
		grandchild := f.childOf(child.ID, "marketing-helper")
		if source.Owner.Type != models.OwnerTypeTeam || f.request().Requester.Type != models.OwnerTypeAgent {
			t.Fatal("fixture must exercise a Team-owned source with its assigned agent requesting the handoff")
		}
		if err := f.commands.CascadeTerminalRun(t.Context(), source); err != nil {
			t.Fatal(err)
		}
		f.assertUnchanged(source)
		f.assertUnchanged(child)
		f.assertUnchanged(grandchild)
	})
}

func TestRunCommandsHandoffReceiptIsScopeBound(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		f := newHandoffCommandFixture(t, store, models.AgentRequestKindHandoff, false, false)
		foreignScope := models.Scope{Kind: f.scope.Kind, ID: "another-tenant"}
		foreignSource, err := f.portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
			Scope: foreignScope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"},
			AssignedAgentID: "developer", Goal: "Foreign feature", Source: models.RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		foreignRequest, err := f.collaboration.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
			Scope: foreignScope, Kind: models.AgentRequestKindHandoff, SourceRunID: foreignSource.ID,
			Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
			Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
			Goal:      "Foreign launch", IdempotencyKey: "launch-handoff",
		})
		if err != nil {
			t.Fatal(err)
		}
		accepted, err := f.collaboration.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{
			Scope: foreignScope, RequestID: foreignRequest.Request.ID, ExpectedRevision: foreignRequest.Request.Revision,
			Decision: AgentRequestDecisionAccept, Principal: foreignRequest.Request.Recipient,
		})
		if err != nil {
			t.Fatal(err)
		}
		beforeForeignSource, err := store.GetAgentRun(t.Context(), foreignScope, accepted.Source.ID)
		if err != nil {
			t.Fatal(err)
		}
		beforeForeignChild, err := store.GetAgentRun(t.Context(), foreignScope, accepted.Child.ID)
		if err != nil {
			t.Fatal(err)
		}
		beforeForeignRequest, err := store.GetAgentRequest(t.Context(), foreignScope, accepted.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.mutateRun(f.sourceID, func(r *models.AgentRun) {
			r.Output["handoffRequestId"] = accepted.Request.ID
		})
		if err := f.commands.CascadeTerminalRun(t.Context(), f.run(f.sourceID)); err != nil {
			t.Fatal(err)
		}
		f.assertCanceled(f.childID)
		afterForeignSource, err := store.GetAgentRun(t.Context(), foreignScope, accepted.Source.ID)
		if err != nil {
			t.Fatal(err)
		}
		afterForeignChild, err := store.GetAgentRun(t.Context(), foreignScope, accepted.Child.ID)
		if err != nil {
			t.Fatal(err)
		}
		afterForeignRequest, err := store.GetAgentRequest(t.Context(), foreignScope, accepted.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(beforeForeignSource, afterForeignSource) || !reflect.DeepEqual(beforeForeignChild, afterForeignChild) ||
			!reflect.DeepEqual(beforeForeignRequest, afterForeignRequest) {
			t.Fatal("cross-scope proof lookup changed foreign work")
		}
	})
}

type handoffAfterChildSnapshotStore struct {
	handoffCommandTestStore
	parentID string
	after    func()
	called   bool
}

func (s *handoffAfterChildSnapshotStore) ListAgentRuns(ctx context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	runs, err := s.handoffCommandTestStore.ListAgentRuns(ctx, filter)
	if err == nil && filter.ParentRunID == s.parentID && !s.called {
		s.called = true
		s.after()
	}
	return runs, err
}

func TestRunCommandsHandoffAcceptanceDuringRecursiveDiscoveryUsesCanonicalSource(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		portfolio := portfolio.NewPortfolioService(store)
		scope := models.Scope{Kind: "tenant", ID: "handoff-ownership"}
		outer, err := portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
			Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "coordinator"}, AssignedAgentID: "coordinator",
			Goal: "Coordinate the delivery", Source: models.RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		f := newHandoffCommandFixtureWithParent(t, store, models.AgentRequestKindHandoff, false, false, outer.ID, false)
		if f.run(f.sourceID).Status != models.AgentRunStatusWaitingForAgent {
			t.Fatal("unaccepted handoff source must be waiting for its recipient")
		}
		f.transition(outer.ID, models.AgentRunStatusRunning, nil)
		outer = f.transition(outer.ID, models.AgentRunStatusCompleted, nil)
		var sourceAfterAcceptance, childAfterAcceptance, ownedAfterAcceptance *models.AgentRun
		var requestAfterAcceptance *models.AgentRequest
		wrapped := &handoffAfterChildSnapshotStore{handoffCommandTestStore: store, parentID: outer.ID}
		wrapped.after = func() {
			request := f.request()
			accepted, err := f.collaboration.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{
				Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
				Decision: AgentRequestDecisionAccept, Principal: request.Recipient,
			})
			if err != nil {
				t.Fatal(err)
			}
			f.childID = accepted.Child.ID
			sourceAfterAcceptance, childAfterAcceptance = f.run(f.sourceID), f.run(f.childID)
			ownedAfterAcceptance = f.childOf(f.childID, "marketing-helper")
			requestAfterAcceptance = f.request()
		}
		commands := agentrun.NewRunCommandService(wrapped, models.WithClock(func() time.Time { return f.now }))
		if err := commands.CascadeTerminalRun(t.Context(), outer); err != nil {
			t.Fatal(err)
		}
		if !wrapped.called {
			t.Fatal("acceptance interleaving was not exercised")
		}
		f.assertUnchanged(outer)
		f.assertUnchanged(sourceAfterAcceptance)
		f.assertUnchanged(childAfterAcceptance)
		f.assertUnchanged(ownedAfterAcceptance)
		if !reflect.DeepEqual(requestAfterAcceptance, f.request()) {
			t.Fatal("recursive cleanup changed the accepted request")
		}
	})
}

func TestRunCommandsHandoffGroupedReceiptDoesNotTransferOwnershipAfterQuorum(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		f := &handoffCommandFixture{
			t: t, store: store, scope: models.Scope{Kind: "tenant", ID: "handoff-ownership"},
			now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		}
		clock := models.WithClock(func() time.Time { return f.now })
		f.portfolio, f.collaboration, f.commands = portfolio.NewPortfolioService(store, clock), NewCollaborationService(store, clock), agentrun.NewRunCommandService(store, clock)
		source, err := f.portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
			Scope: f.scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
			Goal: "Accept the first adequate result", Source: models.RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.sourceID = source.ID
		coordinator := portfolio.NewDependencyCoordinator(store, models.WithClock(func() time.Time { return f.now }))
		group, err := coordinator.CreateRunDependencyGroup(t.Context(), models.CreateRunDependencyGroupRequest{
			ID: "owned-group", Scope: f.scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
			Policy: models.RunDependencyPolicy{Mode: models.FanInModeQuorum, Quorum: 1, FailureMode: models.DependencyFailureWait},
			Dependencies: []models.RunDependencySpec{
				{ID: "handoff-edge", RequestID: "handoff-request", Kind: models.RunDependencyKindAgentRequest},
				{ID: "winner-edge", RequestID: "winner-request", Kind: models.RunDependencyKindAgentRequest},
			},
			Actor: models.ActivityActor{Type: "test", ID: "coordinator"},
		})
		if err != nil {
			t.Fatal(err)
		}
		created, err := f.collaboration.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
			ID: "handoff-request", Scope: f.scope, Kind: models.AgentRequestKindHandoff, SourceRunID: source.ID,
			Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
			Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"}, Goal: "Prepare a launch candidate",
			DependencyGroupID: group.Group.ID, DependencyID: "handoff-edge", IdempotencyKey: "group-owned-handoff",
		})
		if err != nil {
			t.Fatal(err)
		}
		accepted, err := f.collaboration.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{
			Scope: f.scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
			Decision: AgentRequestDecisionAccept, Principal: created.Request.Recipient,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.childID, f.requestID = accepted.Child.ID, accepted.Request.ID
		grandchild := f.childOf(f.childID, "marketing-helper")
		waiting := f.run(f.sourceID)
		if waiting.Status != models.AgentRunStatusWaitingForDependency || waiting.Output["handoffRequestId"] != f.requestID {
			t.Fatalf("group acceptance must retain its source wait and receipt: %#v", waiting)
		}
		joined, err := coordinator.ResolveRunDependency(t.Context(), portfolio.ResolveRunDependencyRequest{
			Scope: f.scope, GroupID: group.Group.ID, DependencyID: "winner-edge", ExpectedDependencyRevision: 1,
			State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"summary": "another candidate won"},
			Actor: models.ActivityActor{Type: "test", ID: "coordinator"},
		})
		if err != nil || joined.Source.Status != models.AgentRunStatusQueued || joined.Group.Status != models.RunDependencyGroupSatisfied {
			t.Fatalf("quorum result: %#v, %v", joined, err)
		}
		f.transition(f.sourceID, models.AgentRunStatusRunning, nil)
		completed := f.transition(f.sourceID, models.AgentRunStatusCompleted, nil)
		if completed.Output["handoffRequestId"] != f.requestID || f.request().Status != models.AgentRequestStatusAccepted {
			t.Fatal("completed source must still carry the accepted grouped handoff receipt")
		}
		if err := f.commands.CascadeTerminalRun(t.Context(), completed); err != nil {
			t.Fatal(err)
		}
		f.assertCanceled(f.childID)
		f.assertCanceled(grandchild.ID)
		f.assertUnchanged(completed)
	})
}

// A host can satisfy the published command-store contract without exposing
// the underlying backend's additional collaboration read capability.
type handoffRunCommandStoreOnly struct{ models.RunCommandStore }

func TestRunCommandsHandoffUnavailableReceiptReaderFailsClosed(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		f := newHandoffCommandFixture(t, store, models.AgentRequestKindHandoff, false, false)
		source, child := f.run(f.sourceID), f.run(f.childID)
		grandchild := f.childOf(child.ID, "marketing-helper")
		receipt := f.request()
		beforeEvents := map[string][]*models.ActivityEvent{
			source.ID: f.events(source.ID), child.ID: f.events(child.ID), grandchild.ID: f.events(grandchild.ID),
		}
		commands := agentrun.NewRunCommandService(handoffRunCommandStoreOnly{RunCommandStore: store}, models.WithClock(func() time.Time { return f.now }))
		if err := commands.CascadeTerminalRun(t.Context(), source); !errors.Is(err, models.ErrInvalidRunTransition) {
			t.Fatalf("unavailable scoped receipt reader: %v", err)
		}
		f.assertUnchanged(source)
		f.assertUnchanged(child)
		f.assertUnchanged(grandchild)
		if !reflect.DeepEqual(receipt, f.request()) {
			t.Fatal("failed ownership check changed the accepted receipt")
		}
		for id, before := range beforeEvents {
			if !reflect.DeepEqual(before, f.events(id)) {
				t.Fatalf("failed ownership check changed activity for run %s", id)
			}
		}

		// The missing optional reader must not disable cleanup for a normal
		// completed parent with no handoff receipt candidate.
		ordinary := f.childOf(source.ID, "ordinary-owner")
		owned := f.childOf(ordinary.ID, "ordinary-helper")
		f.transition(ordinary.ID, models.AgentRunStatusRunning, nil)
		ordinary = f.transition(ordinary.ID, models.AgentRunStatusCompleted, nil)
		if err := commands.CascadeTerminalRun(t.Context(), ordinary); err != nil {
			t.Fatal(err)
		}
		f.assertUnchanged(ordinary)
		f.assertCanceled(owned.ID)
		f.assertUnchanged(source)
		f.assertUnchanged(child)
		f.assertUnchanged(grandchild)
	})
}

// Keep the fixture's composed backend contract checked without widening the
// production RunCommandStore interface for a test-only handoff dependency.
var _ handoffCommandTestStore = (*sqlite.Store)(nil)
