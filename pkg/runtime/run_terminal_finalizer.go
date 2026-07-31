package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

const runResourcesReleasedEvent = "run.resources_released"

// RunTerminalFinalizer releases temporary capability resources after a Run
// reaches any terminal state. Implementations must be idempotent because the
// worker reconciles terminal Runs after process restarts.
type RunTerminalFinalizer interface {
	FinalizeRun(context.Context, *AgentRun) error
}

type RunTerminalFinalizerFunc func(context.Context, *AgentRun) error

func (f RunTerminalFinalizerFunc) FinalizeRun(ctx context.Context, run *AgentRun) error {
	return f(ctx, run)
}

// ActionRunTerminalFinalizer executes Skill-declared finalizer actions for
// every succeeded resource-acquisition action in the Run. Finalizer arguments
// are projected deterministically from same-named action outputs, falling back
// to persisted non-secret inputs. The model never participates in cleanup.
type ActionRunTerminalFinalizer struct {
	store      KernelStore
	catalog    ActionExecutionCatalog
	dispatcher ActionDispatcher
	now        func() time.Time
	newID      func() string
}

func NewActionRunTerminalFinalizer(store KernelStore, catalog ActionExecutionCatalog, dispatcher ActionDispatcher) *ActionRunTerminalFinalizer {
	return &ActionRunTerminalFinalizer{store: store, catalog: catalog, dispatcher: dispatcher, now: time.Now, newID: uuid.NewString}
}

func (f *ActionRunTerminalFinalizer) FinalizeRun(ctx context.Context, run *AgentRun) error {
	if f == nil || f.store == nil || f.catalog == nil || f.dispatcher == nil {
		return errors.New("Run terminal finalizer is not configured")
	}
	if run == nil || !isTerminalAgentRunStatus(run.Status) {
		return errors.New("only terminal Runs can be finalized")
	}
	events, err := f.store.ListActivity(ctx, ActivityFilter{
		Scope: run.Scope, RunID: run.ID, EventTypes: []string{runResourcesReleasedEvent}, Limit: 1,
	})
	if err != nil {
		return err
	}
	if len(events) > 0 {
		return nil
	}
	calls, err := f.store.ListActionCalls(ctx, ActionFilter{
		Scope: run.Scope, RunID: run.ID, Status: []ActionCallStatus{ActionCallStatusSucceeded}, Limit: 1000,
	})
	if err != nil {
		return err
	}
	finalized := 0
	for index := len(calls) - 1; index >= 0; index-- {
		call := calls[index]
		if call == nil {
			continue
		}
		selection := []skill.BindingReference(nil)
		if call.BindingID != "" || call.BindingRevision != 0 {
			selection = append(selection, skill.BindingReference{ID: call.BindingID, Revision: call.BindingRevision})
		}
		acquisition, err := f.catalog.Resolve(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID},
			call.DeploymentID, call.SkillID, call.SkillVersion, call.Action, selection...)
		if err != nil {
			return fmt.Errorf("resolve acquisition action %s: %w", call.ID, err)
		}
		finalizerName := strings.TrimSpace(acquisition.Action.FinalizerAction)
		if finalizerName == "" {
			continue
		}
		finalizer, err := f.catalog.Resolve(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID},
			call.DeploymentID, call.SkillID, call.SkillVersion, finalizerName, selection...)
		if err != nil {
			return fmt.Errorf("resolve finalizer for action %s: %w", call.ID, err)
		}
		arguments := terminalFinalizerArguments(finalizer.Action.InputSchema, call)
		if err := f.catalog.ValidateInput(ctx, finalizer, arguments); err != nil {
			return fmt.Errorf("project finalizer inputs for action %s: %w", call.ID, err)
		}
		finalizerCall := &ActionCall{
			ID: f.newID(), Scope: run.Scope, RunID: run.ID, DeploymentID: call.DeploymentID,
			BindingID: call.BindingID, BindingRevision: call.BindingRevision,
			SkillID: call.SkillID, SkillVersion: call.SkillVersion, Action: finalizerName,
			Status: ActionCallStatusRunning, Risk: finalizer.Action.Risk, SideEffect: finalizer.Action.SideEffect,
			Arguments: arguments, MaxAttempts: 1, Attempt: 1, AvailableAt: f.now().UTC(),
			Revision: 1, CreatedAt: f.now().UTC(), UpdatedAt: f.now().UTC(),
		}
		output, err := f.dispatcher.DispatchAction(ctx, ActionDispatchInput{
			Call: finalizerCall, Run: cloneAgentRun(run), Bound: finalizer, Arguments: cloneMap(arguments),
		})
		if err != nil {
			return fmt.Errorf("execute finalizer %s for action %s: %w", finalizerName, call.ID, err)
		}
		if err := f.catalog.ValidateOutput(ctx, finalizer, output); err != nil {
			return fmt.Errorf("validate finalizer %s for action %s: %w", finalizerName, call.ID, err)
		}
		finalized++
	}
	_, err = f.store.AppendActivity(ctx, &ActivityEvent{
		ID: f.newID(), Scope: run.Scope, RunID: run.ID, AgentID: run.AssignedAgentID,
		ObjectiveID: run.ObjectiveID, TeamID: teamIDForRun(run),
		EventType: runResourcesReleasedEvent, Severity: ActivitySeverityInfo,
		Summary:    "Released temporary resources owned by the terminal Run",
		Actor:      ActivityActor{Type: "runtime", ID: "run-finalizer"},
		Visibility: ActivityVisibilityScope, CreatedAt: f.now().UTC(),
		Payload: map[string]interface{}{"finalizersExecuted": finalized, "terminalStatus": run.Status},
	})
	return err
}

func terminalFinalizerArguments(schema map[string]interface{}, call *ActionCall) map[string]interface{} {
	result := make(map[string]interface{})
	properties, _ := schema["properties"].(map[string]interface{})
	for name := range properties {
		if call.Output != nil {
			if value, ok := call.Output[name]; ok {
				result[name] = cloneTerminalFinalizerValue(value)
				continue
			}
		}
		if call.Arguments != nil {
			if value, ok := call.Arguments[name]; ok {
				result[name] = cloneTerminalFinalizerValue(value)
			}
		}
	}
	return result
}

func cloneTerminalFinalizerValue(value interface{}) interface{} {
	return cloneMap(map[string]interface{}{"value": value})["value"]
}
