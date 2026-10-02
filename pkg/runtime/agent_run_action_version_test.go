package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestCompiledActionVersionCannotMaterializeAgainstNewerBinding(t *testing.T) {
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	claimed := claimedActionRun(t, store, scope, time.Now().UTC(), "worker")
	modelActions, err := catalog.ListModelActions(t.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "release-agent")
	if err != nil || len(modelActions) != 1 {
		t.Fatalf("current authorized actions: %#v %v", modelActions, err)
	}
	policyCalled := false
	actions := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		policyCalled = true
		return ActionPolicyDecision{Disposition: ActionDispositionDeny, Reason: "No external execution in this test"}, nil
	}))
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, errors.New("not needed")
	}), nil, AgentRunWorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	pool.SetActionCoordinator(actions)
	turn := &AgentTurn{ID: "compiled-old-method-turn", RequestedActions: []TurnAction{{Type: "skill_action", Capability: "release.deploy", ExpectedSkillVersion: "0.9.0", Summary: "Deploy using the accepted method", InputRef: "/inputs/deploy"}}, ContinuationCheckpoint: map[string]interface{}{"inputs": map[string]interface{}{"deploy": map[string]interface{}{"environment": "staging"}}}}
	for _, authorized := range [][]capability.ModelAction{modelActions, nil} {
		_, err = pool.materializeTurnAction(t.Context(), "worker", claimed, turn, &TurnRunnerBinding{DeploymentID: "release-agent", ModelActions: authorized})
		if !errors.Is(err, ErrRunSkillDependencyUnavailable) || !strings.Contains(err.Error(), "0.9.0") || policyCalled {
			t.Fatalf("old compiled method reached unavailable or newer authorization: error=%v policyCalled=%v", err, policyCalled)
		}
	}
	calls, err := store.ListActionCalls(t.Context(), ActionFilter{Scope: scope, RunID: claimed.ID})
	if err != nil || len(calls) != 0 {
		t.Fatalf("version mismatch accepted a durable call: %#v %v", calls, err)
	}
	current, err := store.GetAgentRun(t.Context(), scope, claimed.ID)
	if err != nil || current.Revision != claimed.Revision || !reflect.DeepEqual(current.Checkpoint, claimed.Checkpoint) {
		t.Fatalf("version mismatch committed continuation state: %#v %v", current, err)
	}
}

func TestTurnActionVersionConstraintPreservesExactBindingAmbiguity(t *testing.T) {
	actions := []capability.ModelAction{{Name: "release.deploy", Version: "1.0.0", BindingID: "account-a", BindingRevision: 1}, {Name: "release.deploy", Version: "2.0.0", BindingID: "account-b", BindingRevision: 1}}
	if selected, err := selectTurnModelAction(actions, TurnAction{Capability: "release.deploy", ExpectedSkillVersion: "1.0.0"}); selected != nil || err == nil {
		t.Fatalf("version constraint chose an account without exact binding: %#v %v", selected, err)
	}
	for _, expected := range []string{"", "1.0.0"} {
		selected, err := selectTurnModelAction(actions, TurnAction{Capability: "release.deploy", BindingID: "account-a", BindingRevision: 1, ExpectedSkillVersion: expected})
		if err != nil || selected == nil || selected.BindingID != "account-a" {
			t.Fatalf("valid exact binding was rejected: %#v %v", selected, err)
		}
	}
}
