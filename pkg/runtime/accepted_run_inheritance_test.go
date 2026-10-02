package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/google/uuid"
)

func TestAcceptedRunInheritanceSameMethodForkRetainsPinAndBranchCheckpoint(t *testing.T) {
	forAcceptedRunInheritanceStores(t, func(t *testing.T, kernel acceptedRunInheritanceStore) {
		parent, pin := newAcceptedRunInheritanceParent(t, kernel, true)
		preparer := &acceptedRunInheritancePreparer{prepare: func(*CreateAgentRunRequest) error {
			return errors.New("active deployment has changed and new admission refuses the old method")
		}}
		coordinator := NewRunForkCoordinator(kernel)
		coordinator.SetAcceptedRunExecutionPreparer(preparer)
		spoof := *pin
		spoof.DefinitionVersion, spoof.RunbookVersion = "unaccepted", "unaccepted"
		request := acceptedRunInheritanceForkRequest(parent)
		request.Branches = []RunForkBranch{
			{ID: "first", Goal: "Continue first accepted branch", Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "read"}}, Budget: &BudgetPolicy{MaxTurns: 2}},
			{ID: "second", AssignedAgentID: parent.AssignedAgentID, Entrypoint: parent.Entrypoint, Goal: "Continue second accepted branch", Context: map[string]interface{}{AcceptedRunExecutionContextKey: &spoof, "branchInput": "preserved"}, Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "read-new"}}, Budget: &BudgetPolicy{MaxTurns: 3}},
		}
		created, err := coordinator.Create(t.Context(), request)
		if err != nil || created == nil || len(created.Children) != 2 || preparer.calls != 0 {
			t.Fatalf("same-method fork consulted mutable admission: %#v, %v; preparations=%d", created, err, preparer.calls)
		}
		for index, child := range created.Children {
			actual, err := AcceptedRunExecutionForRun(child)
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
		secondPin, err := AcceptedRunExecutionForRun(created.Children[1])
		if err != nil || !reflect.DeepEqual(pin, secondPin) {
			t.Fatalf("siblings shared mutable accepted dependencies: %#v, %v", secondPin, err)
		}
		persistedParent, err := kernel.GetAgentRun(t.Context(), parent.Scope, parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		persistedPin, err := AcceptedRunExecutionForRun(persistedParent)
		if err != nil || !reflect.DeepEqual(pin, persistedPin) {
			t.Fatalf("child mutation changed the parent's accepted identity: %#v, %v", persistedPin, err)
		}
		replayed, err := coordinator.Create(t.Context(), request)
		if err != nil || replayed == nil || !replayed.DependencyGroup.Replayed || len(replayed.Children) != 2 || preparer.calls != 0 {
			t.Fatalf("accepted fork replay consulted current deployment: %#v, %v", replayed, err)
		}
		for _, child := range replayed.Children {
			actual, err := AcceptedRunExecutionForRun(child)
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
						coordinator := NewRunForkCoordinator(wrapped)
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
						if result != nil || !errors.Is(err, ErrAcceptedRunExecution) || wrapped.groupWrites != 0 {
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
				coordinator := NewRunForkCoordinator(wrapped)
				request := acceptedRunInheritanceForkRequest(parent)
				request.Branches[0].AssignedAgentID, request.Branches[0].Entrypoint = parent.AssignedAgentID, parent.Entrypoint
				request.Branches[0].Context = map[string]interface{}{AcceptedRunExecutionContextKey: parentPin, "input": "retained"}
				if change == "different_agent" {
					request.Branches[0].AssignedAgentID = "other-agent"
				} else {
					request.Branches[0].Entrypoint = "another-method"
				}
				preparer := &acceptedRunInheritancePreparer{prepare: func(child *CreateAgentRunRequest) error {
					if wrapped.groupWrites != 0 || child.Scope != parent.Scope || child.AssignedAgentID != request.Branches[0].AssignedAgentID || child.Entrypoint != request.Branches[0].Entrypoint || child.Context[AcceptedRunExecutionContextKey] != nil {
						t.Fatalf("authoritative preparation saw forged input or occurred after persistence: %#v", child)
					}
					children, err := kernel.ListAgentRuns(t.Context(), AgentRunFilter{Scope: parent.Scope, ParentRunID: parent.ID, Limit: 10})
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
				pin, err := AcceptedRunExecutionForRun(result.Children[0])
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
				service := NewCollaborationService(wrapped)
				created := createAcceptedRunInheritanceRequestGroup(t, service, parent, map[string]interface{}{"runbookEntrypoint": "inspect", AcceptedRunExecutionContextKey: parentPin})
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
				result, err := service.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{Scope: parent.Scope, RequestID: pending.ID, ExpectedRevision: pending.Revision, Decision: AgentRequestDecisionAccept, Principal: pending.Recipient})
				if result != nil || !errors.Is(err, ErrAcceptedRunExecution) || wrapped.responseWrites != 0 {
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
				service := NewCollaborationService(wrapped)
				shared := map[string]interface{}{"input": "retained", AcceptedRunExecutionContextKey: parentPin}
				if delivery.invocation {
					shared[RunbookInvocationContextKey] = map[string]interface{}{"entrypoint": "inspect", "summary": "Requested target method"}
				} else {
					shared["runbookEntrypoint"] = "inspect"
				}
				created := createAcceptedRunInheritanceRequestGroup(t, service, parent, shared)
				pending := created.Requests[0]
				preparer := &acceptedRunInheritancePreparer{prepare: func(child *CreateAgentRunRequest) error {
					if wrapped.responseWrites != 0 || child.Scope != parent.Scope || child.AssignedAgentID != pending.Recipient.ID || child.Entrypoint != "inspect" || child.Context[AcceptedRunExecutionContextKey] != nil {
						t.Fatalf("delegation preparation was late or used untrusted method identity: %#v", child)
					}
					setAcceptedRunInheritancePin(child)
					return nil
				}}
				var child *AgentRun
				if !delivery.inbox {
					service.SetAcceptedRunExecutionPreparer(preparer)
					accepted, err := service.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{Scope: parent.Scope, RequestID: pending.ID, ExpectedRevision: pending.Revision, Decision: AgentRequestDecisionAccept, Principal: pending.Recipient})
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
					if stored.AcceptancePolicy != AgentRequestAcceptancePreauthorized {
						t.Fatalf("preauthorized fixture policy = %s", stored.AcceptancePolicy)
					}
					inbox, err := NewAgentRequestInboxReconciler(wrapped)
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
				pin, err := AcceptedRunExecutionForRun(child)
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
		preparer := &acceptedRunInheritancePreparer{prepare: func(*CreateAgentRunRequest) error { return ErrAcceptedRunExecution }}
		coordinator := NewRunForkCoordinator(kernel)
		coordinator.SetAcceptedRunExecutionPreparer(preparer)
		request := acceptedRunInheritanceForkRequest(parent)
		request.Branches = []RunForkBranch{
			{ID: "reason", Goal: "Reason over evidence", Mode: runbook.DelegateReason, Checkpoint: map[string]interface{}{}, Budget: &BudgetPolicy{MaxTurns: 2}},
			{ID: "generic", AssignedAgentID: "other-agent", Goal: "Generic child work", Context: map[string]interface{}{AcceptedRunExecutionContextKey: parentPin}, Checkpoint: map[string]interface{}{}, Budget: &BudgetPolicy{MaxTurns: 2}},
		}
		created, err := coordinator.Create(t.Context(), request)
		if err != nil || created == nil || len(created.Children) != 2 || preparer.calls != 0 {
			t.Fatalf("reason-only work required method admission: %#v, %v", created, err)
		}
		for _, child := range created.Children {
			pin, err := AcceptedRunExecutionForRun(child)
			if err != nil || pin != nil || child.Entrypoint != "" {
				t.Fatalf("generic work inherited an executable pin: %#v, %v", pin, err)
			}
		}
		otherParent, _ := newAcceptedRunInheritanceParent(t, kernel, false)
		service := NewCollaborationService(kernel)
		service.SetAcceptedRunExecutionPreparer(preparer)
		group := createAcceptedRunInheritanceRequestGroup(t, service, otherParent, map[string]interface{}{
			AcceptedRunExecutionContextKey: parentPin,
			"triggerInput":                 map[string]interface{}{"runbookDefinitionId": parentPin.RunbookID, "runbookDefinitionVersion": parentPin.RunbookVersion, "runbookEntrypoint": parentPin.Entrypoint, "entrypoint": parentPin.Entrypoint},
		})
		pending := group.Requests[0]
		accepted, err := service.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{Scope: otherParent.Scope, RequestID: pending.ID, ExpectedRevision: pending.Revision, Decision: AgentRequestDecisionAccept, Principal: pending.Recipient})
		if err != nil || accepted == nil || preparer.calls != 0 {
			t.Fatalf("generic delegated work invoked current method admission: %#v, %v", accepted, err)
		}
		pin, err := AcceptedRunExecutionForRun(accepted.Child)
		if err != nil || pin != nil || accepted.Child.Entrypoint != "" {
			t.Fatalf("generic delegation retained a supplied accepted pin: %#v, %v", pin, err)
		}
	})
}

func TestAcceptedRunInheritanceHelperClonesCallerContextAndDependencies(t *testing.T) {
	forAcceptedRunInheritanceStores(t, func(t *testing.T, kernel acceptedRunInheritanceStore) {
		parent, pin := newAcceptedRunInheritanceParent(t, kernel, false)
		request := CreateAgentRunRequest{Scope: parent.Scope, AssignedAgentID: parent.AssignedAgentID, Entrypoint: parent.Entrypoint, Context: parent.Context}
		if err := prepareAcceptedChildRun(t.Context(), parent, &request, nil, true); err != nil {
			t.Fatal(err)
		}
		mutateAcceptedRunInheritanceDependency(t, request.Context, "mutated-child")
		actual, err := AcceptedRunExecutionForRun(parent)
		if err != nil || !reflect.DeepEqual(pin, actual) {
			t.Fatalf("helper aliased or stripped the parent's trusted pin: %#v, %v", actual, err)
		}
	})
}

type acceptedRunInheritanceStore interface {
	KernelStore
	RunForkStore
	CollaborationKernelStore
	AgentRequestInboxStore
}

func forAcceptedRunInheritanceStores(t *testing.T, test func(*testing.T, acceptedRunInheritanceStore)) {
	t.Helper()
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store, ok := kernel.(acceptedRunInheritanceStore)
		if !ok {
			t.Fatal("accepted-child fixture requires fork, collaboration, and inbox contracts")
		}
		test(t, store)
	})
}

type acceptedRunInheritancePreparer struct {
	calls   int
	prepare func(*CreateAgentRunRequest) error
}

func (p *acceptedRunInheritancePreparer) PrepareAcceptedRunExecution(_ context.Context, request *CreateAgentRunRequest) error {
	p.calls++
	return p.prepare(request)
}

type acceptedRunInheritanceWriteStore struct {
	acceptedRunInheritanceStore
	groupWrites    int
	responseWrites int
}

func (s *acceptedRunInheritanceWriteStore) CreateRunDependencyGroup(ctx context.Context, record RunDependencyGroupCreateRecord) (*RunDependencyResult, error) {
	s.groupWrites++
	return s.acceptedRunInheritanceStore.CreateRunDependencyGroup(ctx, record)
}

func (s *acceptedRunInheritanceWriteStore) RespondAgentRequest(ctx context.Context, record AgentRequestResponseRecord) ([]*ActivityEvent, error) {
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
		change func(*CreateAgentRunRequest)
	}{
		{"missing_pin", func(request *CreateAgentRunRequest) { delete(request.Context, AcceptedRunExecutionContextKey) }},
		{"foreign_scope", func(request *CreateAgentRunRequest) {
			request.Context[AcceptedRunExecutionContextKey].(*AcceptedRunExecution).Scope.ID = "foreign-tenant"
		}},
		{"foreign_deployment", func(request *CreateAgentRunRequest) {
			request.Context[AcceptedRunExecutionContextKey].(*AcceptedRunExecution).DeploymentID = "foreign-agent"
		}},
		{"foreign_entrypoint", func(request *CreateAgentRunRequest) {
			request.Context[AcceptedRunExecutionContextKey].(*AcceptedRunExecution).Entrypoint = "foreign-method"
		}},
		{"malformed_method", func(request *CreateAgentRunRequest) {
			request.Context[AcceptedRunExecutionContextKey].(*AcceptedRunExecution).RunbookVersion = ""
		}},
		{"mutated_scope", func(request *CreateAgentRunRequest) {
			request.Scope.ID = "foreign-tenant"
			setAcceptedRunInheritancePin(request)
		}},
		{"mutated_deployment", func(request *CreateAgentRunRequest) {
			request.AssignedAgentID = "foreign-agent"
			setAcceptedRunInheritancePin(request)
		}},
		{"mutated_entrypoint", func(request *CreateAgentRunRequest) {
			request.Entrypoint = "foreign-method"
			setAcceptedRunInheritancePin(request)
		}},
	} {
		result = append(result, acceptedRunInheritanceRejection{name: invalid.name, preparer: func() *acceptedRunInheritancePreparer {
			return &acceptedRunInheritancePreparer{prepare: func(request *CreateAgentRunRequest) error {
				setAcceptedRunInheritancePin(request)
				invalid.change(request)
				return nil
			}}
		}})
	}
	return result
}

func setAcceptedRunInheritancePin(request *CreateAgentRunRequest) {
	if request.Context == nil {
		request.Context = map[string]interface{}{}
	}
	request.Context[AcceptedRunExecutionContextKey] = &AcceptedRunExecution{
		Scope: request.Scope, DeploymentID: request.AssignedAgentID, DefinitionID: "new-agent-definition", DefinitionVersion: "2",
		RunbookID: "new-method", RunbookVersion: "2", Entrypoint: request.Entrypoint,
		SkillDependencies: []SkillRuntimeReference{{SkillID: "reader", SkillVersion: "2.0.0", SourceIdentity: "native::reader"}},
	}
}

func newAcceptedRunInheritanceParent(t *testing.T, kernel KernelStore, claimed bool) (*AgentRun, *AcceptedRunExecution) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "accepted-inheritance-" + uuid.NewString()}
	deployment, definition := acceptedMethodFixture(scope)
	definition.Runbook.Steps["read"] = runbook.Step{Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reader", SkillVersion: "1.0.0", Action: "read", ResultPath: "/result", Next: "read-new"}}
	definition.Runbook.Steps["read-new"] = runbook.Step{Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reader", SkillVersion: "1.1.0", Action: "read", ResultPath: "/new", Next: "done"}}
	pin, err := NewAcceptedRunExecution(scope, deployment, definition, "manual")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := NewPortfolioService(kernel).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: deployment.ID}, AssignedAgentID: deployment.ID,
		Entrypoint: pin.Entrypoint, Goal: "Continue accepted work", Source: RunSourceManual,
		Context:    map[string]interface{}{AcceptedRunExecutionContextKey: pin, "projectId": "accepted-project"},
		Plan:       map[string]interface{}{"runbook": map[string]interface{}{"id": pin.RunbookID, "version": pin.RunbookVersion}},
		Checkpoint: map[string]interface{}{"committed": "preserved"}, Budget: &BudgetPolicy{MaxTurns: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		parent, err = kernel.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "accepted-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
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

func acceptedRunInheritanceForkRequest(parent *AgentRun) CreateRunForkRequest {
	return CreateRunForkRequest{Scope: parent.Scope, SourceRunID: parent.ID, ExpectedSourceRevision: parent.Revision, WorkerID: "accepted-worker", ForkID: uuid.NewString(),
		Policy:                 RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Branches:               []RunForkBranch{{ID: "branch", Goal: "Execute accepted work", Checkpoint: map[string]interface{}{}, Budget: &BudgetPolicy{MaxTurns: 2}}},
		ContinuationCheckpoint: map[string]interface{}{"committed": "preserved", "waiting": "fan-in"}}
}

func createAcceptedRunInheritanceRequestGroup(t *testing.T, service *CollaborationService, parent *AgentRun, shared map[string]interface{}) *AgentRequestGroupResult {
	t.Helper()
	created, err := service.CreateAgentRequestGroup(t.Context(), CreateAgentRequestGroupRequest{
		Scope: parent.Scope, SourceRunID: parent.ID, ExpectedSourceRevision: parent.Revision,
		Requester:      CollaborationParty{Type: OwnerTypeAgent, ID: parent.AssignedAgentID},
		Policy:         RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Requests:       []AgentRequestGroupSpec{{DependencyID: "accepted-method", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"}, Goal: "Inspect shared evidence", SharedContext: shared, ChildCheckpoint: map[string]interface{}{"input": "preserved"}, BudgetAllocation: &BudgetPolicy{MaxTurns: 2}, AcceptancePolicy: AgentRequestAcceptancePreauthorized}},
		IdempotencyKey: uuid.NewString(), Actor: ActivityActor{Type: "agent", ID: parent.AssignedAgentID}, Visibility: ActivityVisibilityScope,
	})
	if err != nil || created == nil || len(created.Requests) != 1 {
		t.Fatalf("create pending delegated method = %#v, %v", created, err)
	}
	return created
}

func acceptedRunInheritanceEvents(t *testing.T, kernel KernelStore, parent *AgentRun) []*ActivityEvent {
	t.Helper()
	events, err := kernel.ListActivity(t.Context(), ActivityFilter{Scope: parent.Scope, RunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func assertAcceptedRunInheritanceNoMutation(t *testing.T, kernel KernelStore, parent *AgentRun, events []*ActivityEvent) {
	t.Helper()
	actual, err := kernel.GetAgentRun(t.Context(), parent.Scope, parent.ID)
	if err != nil || !reflect.DeepEqual(parent, actual) {
		t.Fatalf("failed child preparation changed parent state or budget: before=%#v after=%#v, %v", parent, actual, err)
	}
	children, err := kernel.ListAgentRuns(t.Context(), AgentRunFilter{Scope: parent.Scope, ParentRunID: parent.ID, Limit: 10})
	if err != nil || len(children) != 0 {
		t.Fatalf("failed child preparation persisted children: %#v, %v", children, err)
	}
	if actualEvents := acceptedRunInheritanceEvents(t, kernel, parent); !reflect.DeepEqual(events, actualEvents) {
		t.Fatalf("failed child preparation appended activity: before=%#v after=%#v", events, actualEvents)
	}
}

func mutateAcceptedRunInheritanceDependency(t *testing.T, contextValue map[string]interface{}, version string) {
	t.Helper()
	switch raw := contextValue[AcceptedRunExecutionContextKey].(type) {
	case *AcceptedRunExecution:
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
