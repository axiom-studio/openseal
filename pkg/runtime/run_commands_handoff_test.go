package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type handoffCommandTestStore interface {
	KernelStore
	CollaborationKernelStore
}

func forHandoffCommandStores(t *testing.T, test func(*testing.T, handoffCommandTestStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, NewMemoryStore()) })
	t.Run("sqlite", func(t *testing.T) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "handoff.db"))
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
	scope         Scope
	now           time.Time
	portfolio     *PortfolioService
	collaboration *CollaborationService
	commands      *RunCommandService
	sourceID      string
	childID       string
	requestID     string
}

func newHandoffCommandFixture(t *testing.T, store handoffCommandTestStore, kind AgentRequestKind, teamOwned, review bool) *handoffCommandFixture {
	t.Helper()
	return newHandoffCommandFixtureWithParent(t, store, kind, teamOwned, review, "", true)
}

func newHandoffCommandFixtureWithParent(t *testing.T, store handoffCommandTestStore, kind AgentRequestKind, teamOwned, review bool, parentID string, accept bool) *handoffCommandFixture {
	t.Helper()
	f := &handoffCommandFixture{
		t: t, store: store, scope: Scope{Kind: "tenant", ID: "handoff-ownership"},
		now:       time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		portfolio: NewPortfolioService(store), collaboration: NewCollaborationService(store), commands: NewRunCommandService(store),
	}
	f.portfolio.now = func() time.Time { return f.now }
	f.collaboration.now = func() time.Time { return f.now }
	f.commands.now = func() time.Time { return f.now }
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}
	if teamOwned {
		owner = ObjectiveOwner{Type: OwnerTypeTeam, ID: "delivery-team"}
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
	source, err := f.portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: f.scope, ParentRunID: parentID, Owner: owner, AssignedAgentID: "developer", Goal: "Build the feature", Source: RunSourceManual,
		Checkpoint: map[string]interface{}{"sourceProgress": "implementation done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.sourceID = source.ID
	created, err := f.collaboration.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
		Scope: f.scope, Kind: kind, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
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

func (f *handoffCommandFixture) run(id string) *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.t.Context(), f.scope, id)
	if err != nil || run == nil {
		f.t.Fatalf("load run %s: %#v, %v", id, run, err)
	}
	return run
}

func (f *handoffCommandFixture) request() *AgentRequest {
	f.t.Helper()
	request, err := f.store.GetAgentRequest(f.t.Context(), f.scope, f.requestID)
	if err != nil || request == nil {
		f.t.Fatalf("load request: %#v, %v", request, err)
	}
	return request
}

func (f *handoffCommandFixture) childOf(parentID, agentID string) *AgentRun {
	f.t.Helper()
	run, err := f.portfolio.CreateAgentRun(f.t.Context(), CreateAgentRunRequest{
		Scope: f.scope, ParentRunID: parentID, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID},
		AssignedAgentID: agentID, Goal: "Owned follow-up", Source: RunSourceManual,
		Checkpoint: map[string]interface{}{"ownedProgress": "not started"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.run(run.ID)
}

func (f *handoffCommandFixture) transition(id string, status AgentRunStatus, output map[string]interface{}) *AgentRun {
	f.t.Helper()
	run := f.run(id)
	activity := NewRunActivityService(f.store, f.store)
	activity.now = func() time.Time { return f.now }
	updated, _, err := activity.TransitionRun(f.t.Context(), f.scope, id, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, Output: output, LeaseOwner: run.LeaseOwner,
		Actor: ActivityActor{Type: "test", ID: "handoff-worker"}, Summary: "Advance fixture run",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return updated
}

func (f *handoffCommandFixture) claimChild() *AgentRun {
	f.t.Helper()
	run, err := f.store.ClaimNextAgentRun(f.t.Context(), AgentRunClaim{
		Scope: f.scope, WorkerID: "marketing-worker", AssignedAgentID: "marketing", Now: f.now,
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || run == nil || run.ID != f.childID || run.Status != AgentRunStatusRunning {
		f.t.Fatalf("claim transferred child: %#v, %v", run, err)
	}
	return f.run(run.ID)
}

func (f *handoffCommandFixture) assertUnchanged(before *AgentRun) {
	f.t.Helper()
	if after := f.run(before.ID); !reflect.DeepEqual(before, after) {
		f.t.Fatalf("run %s changed during source cleanup:\nbefore=%#v\nafter=%#v", before.ID, before, after)
	}
}

func (f *handoffCommandFixture) assertCanceled(id string) {
	f.t.Helper()
	run := f.run(id)
	if run.Status != AgentRunStatusCanceled || run.LeaseOwner != "" || run.LeaseExpiresAt != nil {
		f.t.Fatalf("owned run was not canceled and released: %#v", run)
	}
}

func (f *handoffCommandFixture) events(id string) []*ActivityEvent {
	f.t.Helper()
	events, err := f.store.ListActivity(f.t.Context(), ActivityFilter{Scope: f.scope, RunID: id, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	return events
}

// These mutations deliberately corrupt one durable authority fact at a time.
// They use the canonical CAS write contracts, rather than changing the pointer
// passed to CascadeTerminalRun or bypassing a backend's durable records.
func (f *handoffCommandFixture) mutateRequest(mutate func(*AgentRequest)) {
	f.t.Helper()
	request := f.request()
	revision := request.Revision
	mutate(request)
	request.Revision++
	request.UpdatedAt = f.now
	_, err := f.store.CoordinateAgentRequest(f.t.Context(), AgentRequestCoordinationRecord{
		Request: request, ExpectedRequestRevision: revision,
		Event: &ActivityEvent{
			ID: "corrupt-request-proof", Scope: request.Scope, RunID: request.SourceRunID,
			EventType: "collaboration.test_mutated", Actor: ActivityActor{Type: "test", ID: "fixture"},
			Summary: "Corrupt one durable handoff proof", Severity: ActivitySeverityInfo,
			Visibility: ActivityVisibilityScope, CreatedAt: f.now,
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *handoffCommandFixture) mutateRun(id string, mutate func(*AgentRun)) {
	f.t.Helper()
	run := f.run(id)
	revision := run.Revision
	mutate(run)
	run.Revision++
	run.UpdatedAt = f.now
	_, err := f.store.UpdateAgentRunWithEvent(f.t.Context(), run, revision, &ActivityEvent{
		ID: "corrupt-run-proof", Scope: run.Scope, RunID: run.ID, EventType: "run.test_mutated",
		Actor: ActivityActor{Type: "test", ID: "fixture"}, Summary: "Corrupt one durable handoff proof",
		Severity: ActivitySeverityInfo, Visibility: ActivityVisibilityScope, CreatedAt: f.now,
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
				f := newHandoffCommandFixture(t, store, AgentRequestKindHandoff, false, false)
				if running {
					f.claimChild()
				}
				source, child := f.run(f.sourceID), f.run(f.childID)
				grandchild := f.childOf(child.ID, "marketing-helper")
				sibling := f.childOf(source.ID, "developer-helper")
				requestBefore := f.request()
				if source.Status != AgentRunStatusCompleted || child.ParentRunID != source.ID ||
					child.RootRunID != source.RootRunID || child.Owner.ID != "marketing" || child.Source != RunSourceHandoff {
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
				f.transition(child.ID, AgentRunStatusCompleted, map[string]interface{}{"followUp": "Publish the launch note"})
				resolved, err := f.collaboration.ResolveTerminalAgentRequestChild(t.Context(), f.run(child.ID))
				if err != nil || resolved == nil || resolved.Request.Status != AgentRequestStatusCompleted {
					t.Fatalf("artifact-free completion: %#v, %v", resolved, err)
				}
				request := f.request()
				if len(request.Artifacts) != 0 || request.CompletionSummary != "Publish the launch note" || request.AcceptanceEvidence["runOutput"] == nil {
					t.Fatalf("handoff completion receipt: %#v", request)
				}
				completedSource, completedChild := f.run(source.ID), f.run(child.ID)
				if completedChild.ParentRunID != source.ID || completedChild.RootRunID != source.RootRunID || completedSource.Status != AgentRunStatusCompleted {
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
		f := newHandoffCommandFixture(t, store, AgentRequestKindHandoff, false, false)
		child := f.claimChild()
		grandchild := f.childOf(child.ID, "marketing-helper")
		source := f.run(f.sourceID)
		if _, err := f.commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{
			Scope: f.scope, RunID: child.ID, ExpectedRevision: child.Revision, Kind: AgentRunCommandCancel,
			Actor: ActivityActor{Type: "user", ID: "recipient"},
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
		request func(*AgentRequest)
		source  func(*AgentRun)
		child   func(*AgentRun)
	}{
		{name: "requested", request: func(r *AgentRequest) { r.Status = AgentRequestStatusPending }},
		{name: "rejected", request: func(r *AgentRequest) { r.Status = AgentRequestStatusRejected }},
		{name: "canceled", request: func(r *AgentRequest) { r.Status = AgentRequestStatusCanceled }},
		{name: "failed", request: func(r *AgentRequest) { r.Status = AgentRequestStatusFailed }},
		{name: "unaccepted", request: func(r *AgentRequest) { r.AcceptedAt = nil }},
		{name: "zero acceptance time", request: func(r *AgentRequest) { r.AcceptedAt = &time.Time{} }},
		{name: "ordinary request", request: func(r *AgentRequest) { r.Kind = AgentRequestKindRequest }},
		{name: "wrong source", request: func(r *AgentRequest) { r.SourceRunID = "another-source" }},
		{name: "wrong child", request: func(r *AgentRequest) { r.ChildRunID = "another-child" }},
		{name: "wrong requester", request: func(r *AgentRequest) { r.Requester.ID = "another-requester" }},
		{name: "wrong recipient", request: func(r *AgentRequest) { r.Recipient.ID = "another-recipient" }},
		{name: "wrong assignment", request: func(r *AgentRequest) { r.AssignedAgentID = "another-agent" }},
		{name: "no assignment", request: func(r *AgentRequest) { r.AssignedAgentID = "" }},
		{name: "group owned", request: func(r *AgentRequest) {
			r.DependencyGroupID, r.DependencyID = "owned-group", "owned-edge"
		}},
		{name: "review policy", request: func(r *AgentRequest) {
			r.DelegationPolicy = &AgentRequestDelegationPolicy{
				TeamDeploymentID: "team", TeamDefinitionID: "team", TeamDefinitionVersion: "1",
				DelegationDepth: 1, RequireCompletionReview: true,
			}
		}},
		{name: "missing request", source: func(r *AgentRun) { r.Output["handoffRequestId"] = "missing-request" }},
		{name: "wrong output child", source: func(r *AgentRun) { r.Output["childRunId"] = "another-child" }},
		{name: "non-handoff child", child: func(r *AgentRun) { r.Source = RunSourceRequest }},
		{name: "wrong child root", child: func(r *AgentRun) { r.RootRunID = "another-root" }},
		{name: "wrong child owner", child: func(r *AgentRun) { r.Owner.ID = "another-owner" }},
		{name: "failed source", source: func(r *AgentRun) { r.Status = AgentRunStatusFailed }},
		{name: "canceled source", source: func(r *AgentRun) { r.Status = AgentRunStatusCanceled }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
				f := newHandoffCommandFixture(t, store, AgentRequestKindHandoff, false, false)
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
		f := newHandoffCommandFixture(t, store, AgentRequestKindHandoff, false, false)
		child := f.run(f.childID)
		// A stale caller can omit the receipt or supply another status; neither
		// changes the authoritative completed source's transferred ownership.
		forged := f.run(f.sourceID)
		forged.Status, forged.Output = AgentRunStatusCanceled, nil
		if err := f.commands.CascadeTerminalRun(t.Context(), forged); err != nil {
			t.Fatal(err)
		}
		f.assertUnchanged(child)
		// Conversely, an invented receipt on the pointer cannot protect work
		// after the durable source no longer has that proof.
		f.mutateRun(f.sourceID, func(r *AgentRun) { r.Output = nil })
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
		f := newHandoffCommandFixture(t, store, AgentRequestKindHandoff, false, false)
		transferredBefore := f.run(f.childID)
		live := f.childOf(f.sourceID, "live-owner")
		owned := f.childOf(live.ID, "owned-agent")
		before := f.events(owned.ID)
		forged := cloneAgentRun(live)
		forged.Status = AgentRunStatusCompleted
		if err := f.commands.CascadeTerminalRun(t.Context(), forged); !errors.Is(err, ErrInvalidRunTransition) {
			t.Fatalf("forged terminal status: %v", err)
		}
		f.assertUnchanged(live)
		f.assertUnchanged(owned)
		forged = f.run(f.sourceID)
		forged.Scope.ID = "another-tenant"
		if err := f.commands.CascadeTerminalRun(t.Context(), forged); !errors.Is(err, ErrRunNotFound) {
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
		kind   AgentRequestKind
		review bool
	}{{"ordinary request", AgentRequestKindRequest, false}, {"handoff awaiting review", AgentRequestKindHandoff, true}} {
		t.Run(tc.name, func(t *testing.T) {
			forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
				f := newHandoffCommandFixture(t, store, tc.kind, tc.review, tc.review)
				grandchild := f.childOf(f.childID, "marketing-helper")
				if source := f.run(f.sourceID); source.Status != AgentRunStatusWaitingForDependency {
					t.Fatalf("source should retain ownership while waiting: %#v", source)
				}
				f.transition(f.sourceID, AgentRunStatusCanceled, nil)
				forged := f.run(f.sourceID)
				forged.Status = AgentRunStatusCompleted
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
		f := newHandoffCommandFixture(t, store, AgentRequestKindHandoff, true, false)
		source, child := f.run(f.sourceID), f.run(f.childID)
		grandchild := f.childOf(child.ID, "marketing-helper")
		if source.Owner.Type != OwnerTypeTeam || f.request().Requester.Type != OwnerTypeAgent {
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
		f := newHandoffCommandFixture(t, store, AgentRequestKindHandoff, false, false)
		foreignScope := Scope{Kind: f.scope.Kind, ID: "another-tenant"}
		foreignSource, err := f.portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{
			Scope: foreignScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"},
			AssignedAgentID: "developer", Goal: "Foreign feature", Source: RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		foreignRequest, err := f.collaboration.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
			Scope: foreignScope, Kind: AgentRequestKindHandoff, SourceRunID: foreignSource.ID,
			Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
			Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
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
		f.mutateRun(f.sourceID, func(r *AgentRun) {
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

func (s *handoffAfterChildSnapshotStore) ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	runs, err := s.handoffCommandTestStore.ListAgentRuns(ctx, filter)
	if err == nil && filter.ParentRunID == s.parentID && !s.called {
		s.called = true
		s.after()
	}
	return runs, err
}

func TestRunCommandsHandoffAcceptanceDuringRecursiveDiscoveryUsesCanonicalSource(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		portfolio := NewPortfolioService(store)
		scope := Scope{Kind: "tenant", ID: "handoff-ownership"}
		outer, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{
			Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "coordinator"}, AssignedAgentID: "coordinator",
			Goal: "Coordinate the delivery", Source: RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		f := newHandoffCommandFixtureWithParent(t, store, AgentRequestKindHandoff, false, false, outer.ID, false)
		if f.run(f.sourceID).Status != AgentRunStatusWaitingForAgent {
			t.Fatal("unaccepted handoff source must be waiting for its recipient")
		}
		f.transition(outer.ID, AgentRunStatusRunning, nil)
		outer = f.transition(outer.ID, AgentRunStatusCompleted, nil)
		var sourceAfterAcceptance, childAfterAcceptance, ownedAfterAcceptance *AgentRun
		var requestAfterAcceptance *AgentRequest
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
		commands := NewRunCommandService(wrapped)
		commands.now = func() time.Time { return f.now }
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
			t: t, store: store, scope: Scope{Kind: "tenant", ID: "handoff-ownership"},
			now:       time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
			portfolio: NewPortfolioService(store), collaboration: NewCollaborationService(store), commands: NewRunCommandService(store),
		}
		f.portfolio.now = func() time.Time { return f.now }
		f.collaboration.now = func() time.Time { return f.now }
		f.commands.now = func() time.Time { return f.now }
		source, err := f.portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{
			Scope: f.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
			Goal: "Accept the first adequate result", Source: RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.sourceID = source.ID
		coordinator := NewDependencyCoordinator(store)
		coordinator.now = func() time.Time { return f.now }
		group, err := coordinator.CreateRunDependencyGroup(t.Context(), CreateRunDependencyGroupRequest{
			ID: "owned-group", Scope: f.scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
			Policy: RunDependencyPolicy{Mode: FanInModeQuorum, Quorum: 1, FailureMode: DependencyFailureWait},
			Dependencies: []RunDependencySpec{
				{ID: "handoff-edge", RequestID: "handoff-request", Kind: RunDependencyKindAgentRequest},
				{ID: "winner-edge", RequestID: "winner-request", Kind: RunDependencyKindAgentRequest},
			},
			Actor: ActivityActor{Type: "test", ID: "coordinator"},
		})
		if err != nil {
			t.Fatal(err)
		}
		created, err := f.collaboration.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
			ID: "handoff-request", Scope: f.scope, Kind: AgentRequestKindHandoff, SourceRunID: source.ID,
			Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
			Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"}, Goal: "Prepare a launch candidate",
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
		if waiting.Status != AgentRunStatusWaitingForDependency || waiting.Output["handoffRequestId"] != f.requestID {
			t.Fatalf("group acceptance must retain its source wait and receipt: %#v", waiting)
		}
		joined, err := coordinator.ResolveRunDependency(t.Context(), ResolveRunDependencyRequest{
			Scope: f.scope, GroupID: group.Group.ID, DependencyID: "winner-edge", ExpectedDependencyRevision: 1,
			State: RunDependencyStateSatisfied, Result: map[string]interface{}{"summary": "another candidate won"},
			Actor: ActivityActor{Type: "test", ID: "coordinator"},
		})
		if err != nil || joined.Source.Status != AgentRunStatusQueued || joined.Group.Status != RunDependencyGroupSatisfied {
			t.Fatalf("quorum result: %#v, %v", joined, err)
		}
		f.transition(f.sourceID, AgentRunStatusRunning, nil)
		completed := f.transition(f.sourceID, AgentRunStatusCompleted, nil)
		if completed.Output["handoffRequestId"] != f.requestID || f.request().Status != AgentRequestStatusAccepted {
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
type handoffRunCommandStoreOnly struct{ RunCommandStore }

func TestRunCommandsHandoffUnavailableReceiptReaderFailsClosed(t *testing.T) {
	forHandoffCommandStores(t, func(t *testing.T, store handoffCommandTestStore) {
		f := newHandoffCommandFixture(t, store, AgentRequestKindHandoff, false, false)
		source, child := f.run(f.sourceID), f.run(f.childID)
		grandchild := f.childOf(child.ID, "marketing-helper")
		receipt := f.request()
		beforeEvents := map[string][]*ActivityEvent{
			source.ID: f.events(source.ID), child.ID: f.events(child.ID), grandchild.ID: f.events(grandchild.ID),
		}
		commands := NewRunCommandService(handoffRunCommandStoreOnly{RunCommandStore: store})
		commands.now = func() time.Time { return f.now }
		if err := commands.CascadeTerminalRun(t.Context(), source); !errors.Is(err, ErrInvalidRunTransition) {
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
		f.transition(ordinary.ID, AgentRunStatusRunning, nil)
		ordinary = f.transition(ordinary.ID, AgentRunStatusCompleted, nil)
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
var _ handoffCommandTestStore = (*MemoryStore)(nil)
var _ handoffCommandTestStore = (*SQLiteStore)(nil)
