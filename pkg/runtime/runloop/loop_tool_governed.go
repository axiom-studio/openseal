package runloop

import (
	"context"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
)

func (r *stepToolRun) governedCall(i int) (GovernedToolCall, error) {
	entry := r.entry(i)
	directive, err := loopEntryDirective(entry)
	if err != nil {
		return GovernedToolCall{}, err
	}
	run := r.run()
	return GovernedToolCall{
		Run: run, Step: r.current, WorkerID: r.in.WorkerID, Binding: r.binding,
		ToolCallID: entry.ToolCallID, Name: entry.Tool.Name, Arguments: entry.Tool.Arguments,
		Directive: directive, Narration: r.narration,
	}, nil
}

// runGoverned proposes and executes a governed call that has no action call
// yet, after the image rejection and live browser guards.
func (r *stepToolRun) runGoverned(ctx context.Context, i int) error {
	entry, action := r.entry(i), r.tools[i].Action
	if entry.State == models.ToolEntryPending && entry.ActionCallID == "" {
		prior, err := priorImageProviderRejection(ctx, r.x.cfg.Actions, r.in.Run.Scope, r.in.Run.ID, imageGenerationTarget{
			SkillID: action.SkillID, Action: action.Action, BindingID: action.BindingID, BindingRevision: action.BindingRevision,
		})
		if err != nil {
			return err
		}
		if prior != nil {
			result, guard := imageProviderRejectionRefusal(prior)
			return r.refuse(ctx, i, result, guard)
		}
		if strings.HasPrefix(action.Name, control.LiveBrowserActionPrefix) {
			views, err := r.governedViews(ctx)
			if err != nil {
				return err
			}
			if result, refused := control.RefuseLiveBrowserWhileRateLimited(views, action.Name); refused {
				return r.refuse(ctx, i, result, nil)
			}
		}
	}
	if entry.State == models.ToolEntryPending {
		if err := r.transition(ctx, i, models.ToolEntryRunning, nil, nil, "", nil, false); err != nil {
			return err
		}
	}
	call, err := r.governedCall(i)
	if err != nil {
		return err
	}
	var outcome GovernedToolOutcome
	if r.entry(i).ActionCallID == "" && entry.State == models.ToolEntryPending {
		outcome, err = r.x.cfg.Governed.Execute(ctx, call)
	} else {
		outcome, err = r.x.cfg.Governed.Resume(ctx, call, r.entry(i), r.run().PendingInterventions)
	}
	if err != nil {
		return err
	}
	return r.applyGoverned(ctx, i, outcome)
}

// resumeGoverned settles an open governed entry that already has (or may
// have) an action call.
func (r *stepToolRun) resumeGoverned(ctx context.Context, i int) error {
	call, err := r.governedCall(i)
	if err != nil {
		return err
	}
	outcome, err := r.x.cfg.Governed.Resume(ctx, call, r.entry(i), r.run().PendingInterventions)
	if err != nil {
		return err
	}
	return r.applyGoverned(ctx, i, outcome)
}

func (r *stepToolRun) governedViews(ctx context.Context) ([]control.GovernedToolView, error) {
	r.viewOnce.Do(func() {
		path := r.in.Path
		if path == nil && r.sessionID() != "" {
			path, r.viewErr = r.x.cfg.Sessions.LoadAgentSessionPath(ctx, r.in.Run.Scope, r.sessionID())
			if r.viewErr != nil {
				return
			}
		}
		r.views, r.viewErr = control.LoadGovernedToolViews(ctx, r.x.cfg.Actions, r.in.Run.Scope, path, r.in.Run.ID)
	})
	return r.views, r.viewErr
}

// applyGoverned applies a governed outcome to its tool entry with one
// transition (through running when the edge needs it).
func (r *stepToolRun) applyGoverned(ctx context.Context, i int, outcome GovernedToolOutcome) error {
	r.adoptRun(outcome.Run)
	if exhausted := GovernedBudgetExhaustion(outcome); exhausted != nil {
		r.mu.Lock()
		r.budget = exhausted
		r.mu.Unlock()
	}
	entry := r.entry(i)
	state := outcome.State
	if !state.Valid() {
		return fmt.Errorf("governed outcome of %s has state %q", entry.ToolCallID, state)
	}
	if state == models.ToolEntryRunning {
		if entry.State != models.ToolEntryRunning {
			if err := r.transition(ctx, i, models.ToolEntryRunning, nil, nil, outcome.ActionCallID, nil, false); err != nil {
				return err
			}
		}
		r.mu.Lock()
		r.held = true
		if outcome.RetryAt != nil && (r.retryAt == nil || outcome.RetryAt.Before(*r.retryAt)) {
			at := outcome.RetryAt.UTC()
			r.retryAt = &at
		}
		r.mu.Unlock()
		return nil
	}
	if entry.State == state && state.Awaiting() {
		// Still waiting on the same target: nothing to write.
		r.recordAwaiting(outcome)
		return nil
	}
	if !models.ValidToolEntryTransition(entry.State, state) {
		if err := r.transition(ctx, i, models.ToolEntryRunning, nil, nil, outcome.ActionCallID, nil, true); err != nil {
			return err
		}
	}
	result := outcome.Result
	if result != nil {
		bounded, err := models.BoundToolResult(ctx, r.x.cfg.Output, toolOutputInvocation(r.in.Run, entry.Tool.Name, r.tools[i].Action.SkillID, r.tools[i].Action.Action, outcome.ActionCallID), *result)
		if err != nil {
			return err
		}
		result = &bounded
	}
	if err := r.transition(ctx, i, state, result, outcome.Wait, outcome.ActionCallID, nil, true); err != nil {
		return err
	}
	if state.Awaiting() {
		r.recordAwaiting(outcome)
		return nil
	}
	if r.tools[i].Action.SkillID == models.SkillManagementSkillID && state == models.ToolEntryCompleted {
		r.mu.Lock()
		r.setupIdx = append(r.setupIdx, i)
		r.mu.Unlock()
	}
	return nil
}

func (r *stepToolRun) recordAwaiting(outcome GovernedToolOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.governed = append(r.governed, outcome)
	r.held = true
}

// planPark is the step-end park: the governed park plan wins (approvals over
// cards), then a control park, then a wait for a foreign call lease. nil
// with settled == true means a wait target was decided meanwhile: do not
// park; the next step resumes the entries.
func (r *stepToolRun) planPark(ctx context.Context) (park *agentrun.LoopPark, settled bool, err error) {
	r.mu.Lock()
	governed := append([]GovernedToolOutcome(nil), r.governed...)
	controlPark, retryAt := r.park, r.retryAt
	r.mu.Unlock()
	if len(governed) > 0 {
		plan, err := r.x.cfg.Governed.PlanPark(ctx, r.run(), governed)
		if err != nil {
			return nil, false, err
		}
		if err := r.applyReplaced(ctx, plan); err != nil {
			return nil, false, err
		}
		if plan.Settled {
			return nil, true, nil
		}
		if plan.Park != nil {
			return &agentrun.LoopPark{Status: plan.Park.Status, Wake: plan.Park.Wake, Summary: plan.Park.Summary}, false, nil
		}
	}
	if controlPark != nil {
		return controlPark, false, nil
	}
	if retryAt != nil {
		at := *retryAt
		return &agentrun.LoopPark{
			Status: models.AgentRunStatusSleeping, Wake: &models.WakeCondition{Type: "timer", WakeAt: &at},
			Summary: "Waiting for a tool call that is still running elsewhere",
		}, false, nil
	}
	return nil, false, nil
}

// applyReplaced writes the outcomes a park plan replaced (held approvals
// superseded by a newer message) to their entries.
func (r *stepToolRun) applyReplaced(ctx context.Context, plan GovernedParkPlan) error {
	r.adoptRun(plan.Run)
	for _, replaced := range plan.Replaced {
		for i := range r.entries {
			if r.entry(i).ToolCallID == replaced.ToolCallID && r.tools[i].Route == loopRouteGoverned {
				if err := r.applyGoverned(ctx, i, replaced); err != nil {
					return err
				}
			}
		}
	}
	// A replaced outcome is no longer awaiting.
	r.mu.Lock()
	kept := r.governed[:0]
	for _, outcome := range r.governed {
		replaced := false
		for _, other := range plan.Replaced {
			if other.ToolCallID == outcome.ToolCallID {
				replaced = true
			}
		}
		if !replaced {
			kept = append(kept, outcome)
		}
	}
	r.governed = kept
	r.mu.Unlock()
	return nil
}
