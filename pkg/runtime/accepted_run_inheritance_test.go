package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/google/uuid"
)

func TestAcceptedRunInheritanceSameMethodForkRetainsPinAndBranchCheckpoint(t *testing.T) {
	forAcceptedRunInheritanceStores(t, func(t *testing.T, kernel acceptedRunInheritanceStore) {
		parent, pin := newAcceptedRunInheritanceParent(t, kernel, true)
		preparer := &acceptedRunInheritancePreparer{prepare: func(*models.CreateAgentRunRequest) error {
			return errors.New("active deployment has changed and new admission refuses the old method")
		}}
		coordinator := agentrun.NewRunForkCoordinator(kernel)
		coordinator.SetAcceptedRunExecutionPreparer(preparer)
		spoof := *pin
		spoof.DefinitionVersion, spoof.RunbookVersion = "unaccepted", "unaccepted"
		request := acceptedRunInheritanceForkRequest(parent)
		request.Branches = []agentrun.RunForkBranch{
			{ID: "first", Goal: "Continue first accepted branch", Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "read"}}, Budget: &models.BudgetPolicy{MaxTurns: 2}},
			{ID: "second", AssignedAgentID: parent.AssignedAgentID, Entrypoint: parent.Entrypoint, Goal: "Continue second accepted branch", Context: map[string]interface{}{models.AcceptedRunExecutionContextKey: &spoof, "branchInput": "preserved"}, Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "read-new"}}, Budget: &models.BudgetPolicy{MaxTurns: 3}},
		}
		created, err := coordinator.Create(t.Context(), request)
		if err != nil || created == nil || len(created.Children) != 2 || preparer.calls != 0 {
			t.Fatalf("same-method fork consulted mutable admission: %#v, %v; preparations=%d", created, err, preparer.calls)
		}
		for index, child := range created.Children {
			actual, err := models.AcceptedRunExecutionForRun(child)
			if err != nil || !reflect.DeepEqual(pin, actual) || child.Entrypoint != parent.Entrypoint {
				t.Fatalf("fork child lost immutable execution identity: %#v, %v", actual, err)
			}
			checkpoint, ok := child.Checkpoint["runbook"].(map[string]interface{})
			if !ok || checkpoint["current"] != request.Branches[index].Checkpoint["runbook"].(map[string]interface{})["current"] {
				t.Fatalf("inheritance replaced a branch's starting step: %#v", child.Checkpoint)
			}
		}
		if created.Children[1].Context["branchInput"] != "preserved" {
			t.Fatal("inheritance discarded explicit child input")
		}
		mutateAcceptedRunInheritanceDependency(t, created.Children[0].Context, "caller-mutated")
		secondPin, err := models.AcceptedRunExecutionForRun(created.Children[1])
		if err != nil || !reflect.DeepEqual(pin, secondPin) {
			t.Fatalf("siblings shared mutable accepted dependencies: %#v, %v", secondPin, err)
		}
		persistedParent, err := kernel.GetAgentRun(t.Context(), parent.Scope, parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		persistedPin, err := models.AcceptedRunExecutionForRun(persistedParent)
		if err != nil || !reflect.DeepEqual(pin, persistedPin) {
			t.Fatalf("child mutation changed the parent's accepted identity: %#v, %v", persistedPin, err)
		}
		replayed, err := coordinator.Create(t.Context(), request)
		if err != nil || replayed == nil || !replayed.DependencyGroup.Replayed || len(replayed.Children) != 2 || preparer.calls != 0 {
			t.Fatalf("accepted fork replay consulted current deployment: %#v, %v", replayed, err)
		}
		for _, child := range replayed.Children {
			actual, err := models.AcceptedRunExecutionForRun(child)
			if err != nil || !reflect.DeepEqual(pin, actual) {
				t.Fatalf("replayed child was repinned or shared caller mutation: %#v, %v", actual, err)
			}
		}
	})
}

func TestAcceptedRunInheritanceRejectsUnpreparedOrInvalidForkBeforeAtomicWrite(t *testing.T) {
	for _, change := range []string{"different_agent", "different_entrypoint"} {
		t.Run(change, func(t *testing.T) {
			for _, rejection := range acceptedRunInheritanceRejections() {
				t.Run(rejection.name, func(t *testing.T) {
					forAcceptedRunInheritanceStores(t, func(t *testing.T, kernel acceptedRunInheritanceStore) {
						parent, _ := newAcceptedRunInheritanceParent(t, kernel, true)
						beforeEvents := acceptedRunInheritanceEvents(t, kernel, parent)
						wrapped := &acceptedRunInheritanceWriteStore{acceptedRunInheritanceStore: kernel}
						coordinator := agentrun.NewRunForkCoordinator(wrapped)
						preparer := rejection.preparer()
						if preparer != nil {
							coordinator.SetAcceptedRunExecutionPreparer(preparer)
						}
						request := acceptedRunInheritanceForkRequest(parent)
						request.Branches[0].AssignedAgentID, request.Branches[0].Entrypoint = parent.AssignedAgentID, parent.Entrypoint
						if change == "different_agent" {
							request.Branches[0].AssignedAgentID = "other-agent"
						} else {
							request.Branches[0].Entrypoint = "another-method"
						}
						result, err := coordinator.Create(t.Context(), request)
						if result != nil || !errors.Is(err, models.ErrAcceptedRunExecution) || wrapped.groupWrites != 0 {
							t.Fatalf("unsafe child reached atomic fork acceptance: %#v, %v; writes=%d", result, err, wrapped.groupWrites)
						}
						if preparer != nil && preparer.calls != 1 {
							t.Fatalf("authoritative preparation calls = %d", preparer.calls)
						}
						assertAcceptedRunInheritanceNoMutation(t, kernel, parent, beforeEvents)
						group, err := kernel.FindRunDependencyGroupByIdempotencyKey(t.Context(), parent.Scope, "fork:"+parent.ID+":"+request.ForkID)
						if err != nil || group != nil {
							t.Fatalf("rejected child left a dependency group: %#v, %v", group, err)
						}
					})
				})
			}
		})
	}
}

func TestAcceptedRunInheritancePreparesDifferentAgentOrMethodBeforeForkPersistence(t *testing.T) {
	for _, change := range []string{"different_agent", "different_entrypoint"} {
		t.Run(change, func(t *testing.T) {
			forAcceptedRunInheritanceStores(t, func(t *testing.T, kernel acceptedRunInheritanceStore) {
				parent, parentPin := newAcceptedRunInheritanceParent(t, kernel, true)
				wrapped := &acceptedRunInheritanceWriteStore{acceptedRunInheritanceStore: kernel}
				coordinator := agentrun.NewRunForkCoordinator(wrapped)
				request := acceptedRunInheritanceForkRequest(parent)
				request.Branches[0].AssignedAgentID, request.Branches[0].Entrypoint = parent.AssignedAgentID, parent.Entrypoint
				request.Branches[0].Context = map[string]interface{}{models.AcceptedRunExecutionContextKey: parentPin, "input": "retained"}
				if change == "different_agent" {
					request.Branches[0].AssignedAgentID = "other-agent"
				} else {
					request.Branches[0].Entrypoint = "another-method"
				}
				preparer := &acceptedRunInheritancePreparer{prepare: func(child *models.CreateAgentRunRequest) error {
					if wrapped.groupWrites != 0 || child.Scope != parent.Scope || child.AssignedAgentID != request.Branches[0].AssignedAgentID || child.Entrypoint != request.Branches[0].Entrypoint || child.Context[models.AcceptedRunExecutionContextKey] != nil {
						t.Fatalf("authoritative preparation saw forged input or occurred after persistence: %#v", child)
					}
					children, err := kernel.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: parent.Scope, ParentRunID: parent.ID, Limit: 10})
					if err != nil || len(children) != 0 {
						t.Fatalf("child was persisted before authoritative admission: %#v, %v", children, err)
					}
					setAcceptedRunInheritancePin(child)
					return nil
				}}
				coordinator.SetAcceptedRunExecutionPreparer(preparer)
				result, err := coordinator.Create(t.Context(), request)
				if err != nil || result == nil || len(result.Children) != 1 || preparer.calls != 1 || wrapped.groupWrites != 1 {
					t.Fatalf("prepared method was not atomically accepted: %#v, %v; preparations=%d writes=%d", result, err, preparer.calls, wrapped.groupWrites)
				}
				pin, err := models.AcceptedRunExecutionForRun(result.Children[0])
				if err != nil || pin == nil || pin.DefinitionVersion != "2" || pin.RunbookVersion != "2" || pin.DeploymentID != request.Branches[0].AssignedAgentID || pin.Entrypoint != request.Branches[0].Entrypoint || result.Children[0].Context["input"] != "retained" {
					t.Fatalf("child did not retain the newly prepared exact identity: %#v, %v", pin, err)
				}
			})
		})
	}
}

func TestAcceptedRunInheritanceRejectsInvalidDelegationBeforeGroupAcceptance(t *testing.T) {
	for _, rejection := range acceptedRunInheritanceRejections() {
		t.Run(rejection.name, func(t *testing.T) {
			forAcceptedRunInheritanceStores(t, func(t *testing.T, kernel acceptedRunInheritanceStore) {
				parent, parentPin := newAcceptedRunInheritanceParent(t, kernel, false)
				wrapped := &acceptedRunInheritanceWriteStore{acceptedRunInheritanceStore: kernel}
				service := collaboration.NewService(wrapped)
				created := createAcceptedRunInheritanceRequestGroup(t, service, parent, map[string]interface{}{"runbookEntrypoint": "inspect", models.AcceptedRunExecutionContextKey: parentPin})
				before, err := kernel.GetAgentRun(t.Context(), parent.Scope, parent.ID)
				if err != nil {
					t.Fatal(err)
				}
				beforeEvents := acceptedRunInheritanceEvents(t, kernel, parent)
				beforeGroup, err := kernel.GetRunDependencyGroup(t.Context(), parent.Scope, created.Group.Group.ID)
				if err != nil {
					t.Fatal(err)
				}
				beforeEdges, err := kernel.ListRunDependencies(t.Context(), parent.Scope, beforeGroup.ID)
				if err != nil {
					t.Fatal(err)
				}
				preparer := rejection.preparer()
				if preparer != nil {
					service.SetAcceptedRunExecutionPreparer(preparer)
				}
				pending, err := kernel.GetAgentRequest(t.Context(), parent.Scope, created.Requests[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				result, err := service.RespondAgentRequest(t.Context(), collaboration.RespondAgentRequestRequest{Scope: parent.Scope, RequestID: pending.ID, ExpectedRevision: pending.Revision, Decision: collaboration.AgentRequestDecisionAccept, Principal: pending.Recipient})
				if result != nil || !errors.Is(err, models.ErrAcceptedRunExecution) || wrapped.responseWrites != 0 {
					t.Fatalf("invalid delegated method reached atomic acceptance: %#v, %v; writes=%d", result, err, wrapped.responseWrites)
				}
				if preparer != nil && preparer.calls != 1 {
					t.Fatalf("delegation preparations = %d", preparer.calls)
				}
				assertAcceptedRunInheritanceNoMutation(t, kernel, before, beforeEvents)
				storedRequest, err := kernel.GetAgentRequest(t.Context(), parent.Scope, pending.ID)
				if err != nil || !reflect.DeepEqual(pending, storedRequest) {
					t.Fatalf("failed preparation accepted or rewrote its pending request: %#v, %v", storedRequest, err)
				}
				group, err := kernel.GetRunDependencyGroup(t.Context(), parent.Scope, beforeGroup.ID)
				if err != nil || !reflect.DeepEqual(beforeGroup, group) {
					t.Fatalf("failed preparation changed dependency fan-in: %#v, %v", group, err)
				}
				edges, err := kernel.ListRunDependencies(t.Context(), parent.Scope, beforeGroup.ID)
				if err != nil || !reflect.DeepEqual(beforeEdges, edges) {
					t.Fatalf("failed preparation changed dependency edges: %#v, %v", edges, err)
				}
			})
		})
	}
}

func TestAcceptedRunInheritanceDelegationPreparesRequestedEntrypointAndInboxPropagatesPreparer(t *testing.T) {
	for _, delivery := range []struct {
		name       string
		inbox      bool
		invocation bool
	}{
		{name: "recipient_accepts"},
		{name: "preauthorized_inbox", inbox: true},
		{name: "recipient_accepts_invocation_target", invocation: true},
		{name: "preauthorized_inbox_invocation_target", inbox: true, invocation: true},
	} {
		t.Run(delivery.name, func(t *testing.T) {
			forAcceptedRunInheritanceStores(t, func(t *testing.T, kernel acceptedRunInheritanceStore) {
				parent, parentPin := newAcceptedRunInheritanceParent(t, kernel, false)
				wrapped := &acceptedRunInheritanceWriteStore{acceptedRunInheritanceStore: kernel}
				service := collaboration.NewService(wrapped)
				shared := map[string]interface{}{"input": "retained", models.AcceptedRunExecutionContextKey: parentPin}
				if delivery.invocation {
					shared[agentrun.RunbookInvocationContextKey] = map[string]interface{}{"entrypoint": "inspect", "summary": "Requested target method"}
				} else {
					shared["runbookEntrypoint"] = "inspect"
				}
				created := createAcceptedRunInheritanceRequestGroup(t, service, parent, shared)
				pending := created.Requests[0]
				preparer := &acceptedRunInheritancePreparer{prepare: func(child *models.CreateAgentRunRequest) error {
					if wrapped.responseWrites != 0 || child.Scope != parent.Scope || child.AssignedAgentID != pending.Recipient.ID || child.Entrypoint != "inspect" || child.Context[models.AcceptedRunExecutionContextKey] != nil {
						t.Fatalf("delegation preparation was late or used untrusted method identity: %#v", child)
					}
					setAcceptedRunInheritancePin(child)
					return nil
				}}
				var child *models.AgentRun
				if !delivery.inbox {
					service.SetAcceptedRunExecutionPreparer(preparer)
					accepted, err := service.RespondAgentRequest(t.Context(), collaboration.RespondAgentRequestRequest{Scope: parent.Scope, RequestID: pending.ID, ExpectedRevision: pending.Revision, Decision: collaboration.AgentRequestDecisionAccept, Principal: pending.Recipient})
					if err != nil || accepted == nil {
						t.Fatalf("delegation acceptance = %#v, %v", accepted, err)
					}
					child = accepted.Child
				} else {
					// A preauthorized request is accepted by the same configured
					// collaboration path, rather than bypassing method admission.
					stored, err := kernel.GetAgentRequest(t.Context(), parent.Scope, pending.ID)
					if err != nil {
						t.Fatal(err)
					}
					if stored.AcceptancePolicy != models.AgentRequestAcceptancePreauthorized {
						t.Fatalf("preauthorized fixture policy = %s", stored.AcceptancePolicy)
					}
					inbox, err := collaboration.NewAgentRequestInboxReconciler(wrapped)
					if err != nil {
						t.Fatal(err)
					}
					inbox.SetAcceptedRunExecutionPreparer(preparer)
					accepted, err := inbox.ReconcileWorker(t.Context(), parent.Scope, pending.Recipient.ID)
					if err != nil || accepted == nil || accepted.RequestsAccepted != 1 {
						t.Fatalf("inbox did not prepare accepted work: %#v, %v", accepted, err)
					}
					stored, err = kernel.GetAgentRequest(t.Context(), parent.Scope, pending.ID)
					if err != nil {
						t.Fatal(err)
					}
					child, err = kernel.GetAgentRun(t.Context(), parent.Scope, stored.ChildRunID)
					if err != nil {
						t.Fatal(err)
					}
				}
				pin, err := models.AcceptedRunExecutionForRun(child)
				if err != nil || pin == nil || pin.Entrypoint != "inspect" || pin.DeploymentID != pending.Recipient.ID || pin.DefinitionVersion != "2" || pin.RunbookVersion != "2" || child.Context["input"] != "retained" || preparer.calls != 1 || wrapped.responseWrites != 1 {
					t.Fatalf("delegated child lost authoritative requested identity: %#v, %v; preparations=%d writes=%d", pin, err, preparer.calls, wrapped.responseWrites)
				}
			})
		})
	}
}

func TestAcceptedRunInheritanceReasonOnlyForkAndGenericDelegationStayUnpinned(t *testing.T) {
	forAcceptedRunInheritanceStores(t, func(t *testing.T, kernel acceptedRunInheritanceStore) {
		parent, parentPin := newAcceptedRunInheritanceParent(t, kernel, true)
		preparer := &acceptedRunInheritancePreparer{prepare: func(*models.CreateAgentRunRequest) error { return models.ErrAcceptedRunExecution }}
		coordinator := agentrun.NewRunForkCoordinator(kernel)
		coordinator.SetAcceptedRunExecutionPreparer(preparer)
		request := acceptedRunInheritanceForkRequest(parent)
		request.Branches = []agentrun.RunForkBranch{
			{ID: "reason", Goal: "Reason over evidence", Mode: runbook.DelegateReason, Checkpoint: map[string]interface{}{}, Budget: &models.BudgetPolicy{MaxTurns: 2}},
			{ID: "generic", AssignedAgentID: "other-agent", Goal: "Generic child work", Context: map[string]interface{}{models.AcceptedRunExecutionContextKey: parentPin}, Checkpoint: map[string]interface{}{}, Budget: &models.BudgetPolicy{MaxTurns: 2}},
		}
		created, err := coordinator.Create(t.Context(), request)
		if err != nil || created == nil || len(created.Children) != 2 || preparer.calls != 0 {
			t.Fatalf("reason-only work required method admission: %#v, %v", created, err)
		}
		for _, child := range created.Children {
			pin, err := models.AcceptedRunExecutionForRun(child)
			if err != nil || pin != nil || child.Entrypoint != "" {
				t.Fatalf("generic work inherited an executable pin: %#v, %v", pin, err)
			}
		}
		otherParent, _ := newAcceptedRunInheritanceParent(t, kernel, false)
		service := collaboration.NewService(kernel)
		service.SetAcceptedRunExecutionPreparer(preparer)
		group := createAcceptedRunInheritanceRequestGroup(t, service, otherParent, map[string]interface{}{
			models.AcceptedRunExecutionContextKey: parentPin,
			"triggerInput":                        map[string]interface{}{"runbookDefinitionId": parentPin.RunbookID, "runbookDefinitionVersion": parentPin.RunbookVersion, "runbookEntrypoint": parentPin.Entrypoint, "entrypoint": parentPin.Entrypoint},
		})
		pending := group.Requests[0]
		accepted, err := service.RespondAgentRequest(t.Context(), collaboration.RespondAgentRequestRequest{Scope: otherParent.Scope, RequestID: pending.ID, ExpectedRevision: pending.Revision, Decision: collaboration.AgentRequestDecisionAccept, Principal: pending.Recipient})
		if err != nil || accepted == nil || preparer.calls != 0 {
			t.Fatalf("generic delegated work invoked current method admission: %#v, %v", accepted, err)
		}
		pin, err := models.AcceptedRunExecutionForRun(accepted.Child)
		if err != nil || pin != nil || accepted.Child.Entrypoint != "" {
			t.Fatalf("generic delegation retained a supplied accepted pin: %#v, %v", pin, err)
		}
	})
}

func TestAcceptedRunInheritanceHelperClonesCallerContextAndDependencies(t *testing.T) {
	forAcceptedRunInheritanceStores(t, func(t *testing.T, kernel acceptedRunInheritanceStore) {
		parent, pin := newAcceptedRunInheritanceParent(t, kernel, false)
		request := models.CreateAgentRunRequest{Scope: parent.Scope, AssignedAgentID: parent.AssignedAgentID, Entrypoint: parent.Entrypoint, Context: parent.Context}
		if err := agentrun.PrepareAcceptedChildRun(t.Context(), parent, &request, nil, true); err != nil {
			t.Fatal(err)
		}
		mutateAcceptedRunInheritanceDependency(t, request.Context, "mutated-child")
		actual, err := models.AcceptedRunExecutionForRun(parent)
		if err != nil || !reflect.DeepEqual(pin, actual) {
			t.Fatalf("helper aliased or stripped the parent's trusted pin: %#v, %v", actual, err)
		}
	})
}

type acceptedRunInheritanceStore interface {
	models.KernelStore
	models.RunForkStore
	models.CollaborationKernelStore
	models.AgentRequestInboxStore
}

func forAcceptedRunInheritanceStores(t *testing.T, test func(*testing.T, acceptedRunInheritanceStore)) {
	t.Helper()
	actiontest.ForEachStore(t, func(t *testing.T, kernel models.KernelStore) {
		store, ok := kernel.(acceptedRunInheritanceStore)
		if !ok {
			t.Fatal("accepted-child fixture requires fork, collaboration, and inbox contracts")
		}
		test(t, store)
	})
}

type acceptedRunInheritancePreparer struct {
	calls   int
	prepare func(*models.CreateAgentRunRequest) error
}

func (p *acceptedRunInheritancePreparer) PrepareAcceptedRunExecution(_ context.Context, request *models.CreateAgentRunRequest) error {
	p.calls++
	return p.prepare(request)
}

type acceptedRunInheritanceWriteStore struct {
	acceptedRunInheritanceStore
	groupWrites    int
	responseWrites int
}

func (s *acceptedRunInheritanceWriteStore) CreateRunDependencyGroup(ctx context.Context, record models.RunDependencyGroupCreateRecord) (*models.RunDependencyResult, error) {
	s.groupWrites++
	return s.acceptedRunInheritanceStore.CreateRunDependencyGroup(ctx, record)
}

func (s *acceptedRunInheritanceWriteStore) RespondAgentRequest(ctx context.Context, record models.AgentRequestResponseRecord) ([]*models.ActivityEvent, error) {
	s.responseWrites++
	return s.acceptedRunInheritanceStore.RespondAgentRequest(ctx, record)
}

type acceptedRunInheritanceRejection struct {
	name     string
	preparer func() *acceptedRunInheritancePreparer
}

func acceptedRunInheritanceRejections() []acceptedRunInheritanceRejection {
	result := []acceptedRunInheritanceRejection{{name: "missing_preparer", preparer: func() *acceptedRunInheritancePreparer { return nil }}}
	for _, invalid := range []struct {
		name   string
		change func(*models.CreateAgentRunRequest)
	}{
		{"missing_pin", func(request *models.CreateAgentRunRequest) {
			delete(request.Context, models.AcceptedRunExecutionContextKey)
		}},
		{"foreign_scope", func(request *models.CreateAgentRunRequest) {
			request.Context[models.AcceptedRunExecutionContextKey].(*models.AcceptedRunExecution).Scope.ID = "foreign-tenant"
		}},
		{"foreign_deployment", func(request *models.CreateAgentRunRequest) {
			request.Context[models.AcceptedRunExecutionContextKey].(*models.AcceptedRunExecution).DeploymentID = "foreign-agent"
		}},
		{"foreign_entrypoint", func(request *models.CreateAgentRunRequest) {
			request.Context[models.AcceptedRunExecutionContextKey].(*models.AcceptedRunExecution).Entrypoint = "foreign-method"
		}},
		{"malformed_method", func(request *models.CreateAgentRunRequest) {
			request.Context[models.AcceptedRunExecutionContextKey].(*models.AcceptedRunExecution).RunbookVersion = ""
		}},
		{"mutated_scope", func(request *models.CreateAgentRunRequest) {
			request.Scope.ID = "foreign-tenant"
			setAcceptedRunInheritancePin(request)
		}},
		{"mutated_deployment", func(request *models.CreateAgentRunRequest) {
			request.AssignedAgentID = "foreign-agent"
			setAcceptedRunInheritancePin(request)
		}},
		{"mutated_entrypoint", func(request *models.CreateAgentRunRequest) {
			request.Entrypoint = "foreign-method"
			setAcceptedRunInheritancePin(request)
		}},
	} {
		result = append(result, acceptedRunInheritanceRejection{name: invalid.name, preparer: func() *acceptedRunInheritancePreparer {
			return &acceptedRunInheritancePreparer{prepare: func(request *models.CreateAgentRunRequest) error {
				setAcceptedRunInheritancePin(request)
				invalid.change(request)
				return nil
			}}
		}})
	}
	return result
}

func setAcceptedRunInheritancePin(request *models.CreateAgentRunRequest) {
	if request.Context == nil {
		request.Context = map[string]interface{}{}
	}
	request.Context[models.AcceptedRunExecutionContextKey] = &models.AcceptedRunExecution{
		Scope: request.Scope, DeploymentID: request.AssignedAgentID, DefinitionID: "new-agent-definition", DefinitionVersion: "2",
		RunbookID: "new-method", RunbookVersion: "2", Entrypoint: request.Entrypoint,
		SkillDependencies: []models.SkillRuntimeReference{{SkillID: "reader", SkillVersion: "2.0.0", SourceIdentity: "native::reader"}},
	}
}

func newAcceptedRunInheritanceParent(t *testing.T, kernel models.KernelStore, claimed bool) (*models.AgentRun, *models.AcceptedRunExecution) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "accepted-inheritance-" + uuid.NewString()}
	deployment, definition := acceptedMethodFixture(scope)
	definition.Runbook.Steps["read"] = runbook.Step{Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reader", SkillVersion: "1.0.0", Action: "read", ResultPath: "/result", Next: "read-new"}}
	definition.Runbook.Steps["read-new"] = runbook.Step{Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reader", SkillVersion: "1.1.0", Action: "read", ResultPath: "/new", Next: "done"}}
	pin, err := models.NewAcceptedRunExecution(scope, deployment, definition, "manual")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := portfolio.NewService(kernel).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deployment.ID}, AssignedAgentID: deployment.ID,
		Entrypoint: pin.Entrypoint, Goal: "Continue accepted work", Source: models.RunSourceManual,
		Context:    map[string]interface{}{models.AcceptedRunExecutionContextKey: pin, "projectId": "accepted-project"},
		Plan:       map[string]interface{}{"runbook": map[string]interface{}{"id": pin.RunbookID, "version": pin.RunbookVersion}},
		Checkpoint: map[string]interface{}{"committed": "preserved"}, Budget: &models.BudgetPolicy{MaxTurns: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		parent, err = kernel.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: scope, WorkerID: "accepted-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || parent == nil {
			t.Fatalf("claim accepted parent = %#v, %v", parent, err)
		}
	}
	parent, err = kernel.GetAgentRun(t.Context(), scope, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return parent, pin
}

func acceptedRunInheritanceForkRequest(parent *models.AgentRun) agentrun.CreateRunForkRequest {
	return agentrun.CreateRunForkRequest{Scope: parent.Scope, SourceRunID: parent.ID, ExpectedSourceRevision: parent.Revision, WorkerID: "accepted-worker", ForkID: uuid.NewString(),
		Policy:                 models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Branches:               []agentrun.RunForkBranch{{ID: "branch", Goal: "Execute accepted work", Checkpoint: map[string]interface{}{}, Budget: &models.BudgetPolicy{MaxTurns: 2}}},
		ContinuationCheckpoint: map[string]interface{}{"committed": "preserved", "waiting": "fan-in"}}
}

func createAcceptedRunInheritanceRequestGroup(t *testing.T, service *collaboration.Service, parent *models.AgentRun, shared map[string]interface{}) *collaboration.AgentRequestGroupResult {
	t.Helper()
	created, err := service.CreateAgentRequestGroup(t.Context(), collaboration.CreateAgentRequestGroupRequest{
		Scope: parent.Scope, SourceRunID: parent.ID, ExpectedSourceRevision: parent.Revision,
		Requester:      models.CollaborationParty{Type: models.OwnerTypeAgent, ID: parent.AssignedAgentID},
		Policy:         models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Requests:       []collaboration.AgentRequestGroupSpec{{DependencyID: "accepted-method", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"}, Goal: "Inspect shared evidence", SharedContext: shared, ChildCheckpoint: map[string]interface{}{"input": "preserved"}, BudgetAllocation: &models.BudgetPolicy{MaxTurns: 2}, AcceptancePolicy: models.AgentRequestAcceptancePreauthorized}},
		IdempotencyKey: uuid.NewString(), Actor: models.ActivityActor{Type: "agent", ID: parent.AssignedAgentID}, Visibility: models.ActivityVisibilityScope,
	})
	if err != nil || created == nil || len(created.Requests) != 1 {
		t.Fatalf("create pending delegated method = %#v, %v", created, err)
	}
	return created
}

func acceptedRunInheritanceEvents(t *testing.T, kernel models.KernelStore, parent *models.AgentRun) []*models.ActivityEvent {
	t.Helper()
	events, err := kernel.ListActivity(t.Context(), models.ActivityFilter{Scope: parent.Scope, RunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func assertAcceptedRunInheritanceNoMutation(t *testing.T, kernel models.KernelStore, parent *models.AgentRun, events []*models.ActivityEvent) {
	t.Helper()
	actual, err := kernel.GetAgentRun(t.Context(), parent.Scope, parent.ID)
	if err != nil || !reflect.DeepEqual(parent, actual) {
		t.Fatalf("failed child preparation changed parent state or budget: before=%#v after=%#v, %v", parent, actual, err)
	}
	children, err := kernel.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: parent.Scope, ParentRunID: parent.ID, Limit: 10})
	if err != nil || len(children) != 0 {
		t.Fatalf("failed child preparation persisted children: %#v, %v", children, err)
	}
	if actualEvents := acceptedRunInheritanceEvents(t, kernel, parent); !reflect.DeepEqual(events, actualEvents) {
		t.Fatalf("failed child preparation appended activity: before=%#v after=%#v", events, actualEvents)
	}
}

func mutateAcceptedRunInheritanceDependency(t *testing.T, contextValue map[string]interface{}, version string) {
	t.Helper()
	switch raw := contextValue[models.AcceptedRunExecutionContextKey].(type) {
	case *models.AcceptedRunExecution:
		raw.SkillDependencies[0].SkillVersion = version
	case map[string]interface{}:
		dependencies, ok := raw["skillDependencies"].([]interface{})
		if !ok || len(dependencies) == 0 {
			t.Fatalf("accepted dependencies are not materialized: %#v", raw)
		}
		dependencies[0].(map[string]interface{})["skillVersion"] = version
	default:
		t.Fatalf("unsupported accepted identity representation: %T", raw)
	}
}
