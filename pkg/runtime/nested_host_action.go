package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	// NestedHostActionLease is how long a host holds a host call between
	// acquisitions. Each acquisition renews it.
	NestedHostActionLease = 10 * time.Minute
)

var (
	// ErrNestedHostActionNotReady reports a host call that may not run yet:
	// it waits for approval, or another claimant holds it.
	ErrNestedHostActionNotReady = errors.New("host call is not ready to run")
	// ErrNestedHostActionClosed reports a host call that has settled (denied,
	// finished or canceled) and can no longer be acquired.
	ErrNestedHostActionClosed    = errors.New("host call is closed")
	errNestedHostActionAbandoned = errors.New("the host performing this call stopped before it finished")
	errNestedHostActionNotDone   = errors.New("the host stopped before performing this call")
)

// NestedHostActionRequest asks to govern work the host performs during an
// in-flight Turn with the authority of one Skill action the Turn offers.
// SkillID and Action name that action; the Turn must offer it from exactly
// one binding, as the model would see it. Arguments are that action's input
// and are validated, governed and shown for approval as such.
type NestedHostActionRequest struct {
	Scope     models.Scope           `json:"scope"`
	RunID     string                 `json:"runId"`
	TurnID    string                 `json:"turnId"`
	SkillID   string                 `json:"skillId"`
	Action    string                 `json:"action"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
	// OriginRef groups calls in activity, such as the command they ran under.
	OriginRef string `json:"originRef,omitempty"`
	// IdempotencyKey identifies this request within the Turn.
	IdempotencyKey string `json:"idempotencyKey"`
	// Summary describes the host's work in activity and approvals.
	Summary string `json:"summary,omitempty"`
}

// NestedHostActionGrant is what a host holds to perform one host call: the
// claimed call and a signed, single-use credential lease for it. The lease
// carries opaque credential references only; the host resolves them.
type NestedHostActionGrant struct {
	Call  *models.ActionCall                  `json:"call"`
	Lease *models.SignedActionCredentialLease `json:"lease"`
}

// NestedHostActionOutcome is how a host call ended. Performed says whether
// any of the work reached the provider. Output is recorded on the call and
// must carry no credential.
type NestedHostActionOutcome struct {
	Scope        models.Scope
	RunID        string
	TurnID       string
	ActionCallID string
	Performed    bool
	Output       map[string]interface{}
	Error        string
	// Summary describes the finished work in activity.
	Summary string
}

// ProposeNestedHost records one host call for an in-flight Turn, governed as
// the named offered action. An allowed call is ready for its host (see
// AcquireNestedHostAction); a call that needs review waits for its approval;
// a denied call is returned denied.
func (c *ActionCoordinator) ProposeNestedHost(ctx context.Context, req NestedHostActionRequest) (*NestedActionResult, error) {
	skillID, action := strings.TrimSpace(req.SkillID), strings.TrimSpace(req.Action)
	if skillID == "" || action == "" {
		return nil, errors.New("a host call names the offered Skill action it acts as")
	}
	return c.proposeNested(ctx, NestedActionRequest{
		Scope: req.Scope, RunID: req.RunID, TurnID: req.TurnID, Capability: skillID + "/" + action, Arguments: req.Arguments,
		Origin: models.ActionOriginVM, OriginRef: req.OriginRef, IdempotencyKey: req.IdempotencyKey, Summary: req.Summary,
	}, models.ActionExecutionHost, func(binding *agentrun.TurnRunnerBinding) (*capability.ModelAction, error) {
		return selectOfferedSkillAction(binding.ModelActions, skillID, action)
	})
}

// selectOfferedSkillAction finds the one definition action a Turn offers as
// skillID's action, or reports that the Turn does not offer it.
func selectOfferedSkillAction(actions []capability.ModelAction, skillID, action string) (*capability.ModelAction, error) {
	var selected *capability.ModelAction
	for index := range actions {
		candidate := &actions[index]
		if candidate.SkillID != skillID || candidate.Action != action || candidate.BindingTool != nil {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("%w: more than one binding offers %s %s", ErrNestedActionNotAuthorized, skillID, action)
		}
		selected = candidate
	}
	if selected == nil {
		return nil, fmt.Errorf("%w: the turn does not offer %s %s", ErrNestedActionNotAuthorized, skillID, action)
	}
	return selected, nil
}

// hostCall returns the host call actionCallID of the in-flight Turn and the
// Turn's authority.
func (c *ActionCoordinator) hostCall(ctx context.Context, scope models.Scope, runID, turnID, actionCallID string) (*nestedActionAuthority, *models.ActionCall, error) {
	if c == nil || c.actions == nil {
		return nil, nil, errors.New("action coordinator is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, nil, err
	}
	authority := c.nested.lookup(scope, turnID)
	if authority == nil || authority.runID != runID {
		return nil, nil, models.ErrNestedTurnNotActive
	}
	call, err := c.actions.GetActionCall(ctx, scope, actionCallID)
	if err != nil {
		return nil, nil, err
	}
	if call == nil || call.RunID != runID || call.TurnID != turnID || call.Origin != models.ActionOriginVM || call.Execution != models.ActionExecutionHost {
		return nil, nil, models.ErrActionNotFound
	}
	return authority, call, nil
}

func (c *ActionCoordinator) hostWorker() (*ActionWorker, error) {
	store, ok := c.actions.(models.KernelStore)
	if !ok {
		return nil, errors.New("host calls need a kernel store")
	}
	return &ActionWorker{store: store, now: c.now, newID: c.newID}, nil
}

// AcquireNestedHostAction claims a ready host call for the Turn's host, or
// renews the host's hold on it, and issues a fresh credential lease for it
// through issuer. A host acquires once per piece of work (for example each
// HTTP request of one git operation); every lease is single-use and expires
// within MaximumActionCredentialLeaseTTL.
func (c *ActionCoordinator) AcquireNestedHostAction(ctx context.Context, scope models.Scope, runID, turnID, actionCallID string, issuer ActionCredentialLeaseIssuer) (*NestedHostActionGrant, error) {
	if issuer == nil {
		return nil, errors.New("host calls need a credential lease issuer")
	}
	authority, call, err := c.hostCall(ctx, scope, runID, turnID, actionCallID)
	if err != nil {
		return nil, err
	}
	worker, err := c.hostWorker()
	if err != nil {
		return nil, err
	}
	now := c.now().UTC()
	switch {
	case call.Status == models.ActionCallStatusReady:
		claimed, err := worker.store.ClaimNextAction(ctx, models.ActionClaim{Scope: scope, WorkerID: authority.workerID, Now: now, LeaseDuration: NestedHostActionLease, OwnActionID: call.ID})
		if err != nil {
			return nil, err
		}
		if claimed == nil {
			return nil, ErrNestedHostActionNotReady
		}
		call = claimed
	case call.Status == models.ActionCallStatusRunning && call.LeaseOwner == authority.workerID && call.LeaseExpiresAt != nil && call.LeaseExpiresAt.After(now):
		if call, err = worker.store.RenewActionLease(ctx, scope, call.ID, authority.workerID, now, NestedHostActionLease); err != nil {
			return nil, err
		}
	case call.Status == models.ActionCallStatusWaitingApproval, call.Status == models.ActionCallStatusRunning:
		return nil, ErrNestedHostActionNotReady
	default:
		return nil, fmt.Errorf("%w: %s", ErrNestedHostActionClosed, call.Status)
	}
	run, err := worker.store.GetAgentRun(ctx, scope, runID)
	if err != nil {
		return nil, err
	}
	live, err := nestedActionTurnLive(ctx, worker.store, run, call, now)
	if err != nil {
		return nil, err
	}
	if !live {
		// The Turn has ended: nothing more may run under it.
		if run != nil {
			_, _ = worker.persistOutcome(ctx, call, nil, nil, nil, nil, authority.workerID, false)
		}
		return nil, models.ErrNestedTurnNotActive
	}
	selection := []skill.BindingReference(nil)
	if call.BindingID != "" || call.BindingRevision != 0 {
		selection = append(selection, skill.BindingReference{ID: call.BindingID, Revision: call.BindingRevision})
	}
	bound, err := c.catalog.Resolve(skill.WithRunActor(ctx, models.RunSkillActor(run)), skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
		call.DeploymentID, call.SkillID, call.SkillVersion, call.Action, selection...)
	if err != nil {
		return nil, err
	}
	if len(call.CredentialRefs) == 0 {
		return nil, errors.New("the host call's action uses no credential")
	}
	transport, err := boundActionToolTransport(bound)
	if err != nil {
		return nil, fmt.Errorf("host call credential lease transport: %w", err)
	}
	lease, err := issuer.IssueActionCredentialLease(ctx, ActionCredentialLeaseIssueRequest{
		Call: models.CloneActionCall(call), Run: models.CloneAgentRun(run), References: maps.Clone(call.CredentialRefs), Transport: transport,
	})
	if err != nil {
		return nil, err
	}
	if err := MatchActionCredentialLeaseReferences(lease, call, run, transport); err != nil {
		return nil, err
	}
	return &NestedHostActionGrant{Call: models.CloneActionCall(call), Lease: lease}, nil
}

// FinishNestedHostAction records how the host's work on a host call ended
// and releases the call. A call still ready is closed without having run.
func (c *ActionCoordinator) FinishNestedHostAction(ctx context.Context, outcome NestedHostActionOutcome) (*models.ActionCall, error) {
	authority, call, err := c.hostCall(ctx, outcome.Scope, outcome.RunID, outcome.TurnID, outcome.ActionCallID)
	if err != nil {
		return nil, err
	}
	worker, err := c.hostWorker()
	if err != nil {
		return nil, err
	}
	worker.hostSummary = strings.TrimSpace(outcome.Summary)
	now := c.now().UTC()
	if call.Status == models.ActionCallStatusReady {
		claimed, err := worker.store.ClaimNextAction(ctx, models.ActionClaim{Scope: outcome.Scope, WorkerID: authority.workerID, Now: now, LeaseDuration: time.Minute, OwnActionID: call.ID})
		if err != nil {
			return nil, err
		}
		if claimed == nil {
			return nil, ErrNestedHostActionNotReady
		}
		call, outcome.Performed = claimed, false
	}
	switch {
	case call.Status == models.ActionCallStatusRunning && call.LeaseOwner == authority.workerID:
	case call.Status == models.ActionCallStatusWaitingApproval || call.Status == models.ActionCallStatusRunning:
		return nil, ErrNestedHostActionNotReady
	default:
		return nil, fmt.Errorf("%w: %s", ErrNestedHostActionClosed, call.Status)
	}
	var executionErr error
	if message := strings.TrimSpace(outcome.Error); message != "" {
		executionErr = errors.New(message)
	} else if !outcome.Performed {
		executionErr = errNestedHostActionNotDone
	}
	output := maps.Clone(outcome.Output)
	if executionErr != nil {
		output = nil
	}
	result, err := worker.persistOutcome(ctx, call, nil, nil, output, executionErr, authority.workerID, outcome.Performed)
	if err != nil {
		return nil, err
	}
	return result.Call, nil
}

func boundActionToolTransport(bound *skill.BoundAction) (string, error) {
	if bound == nil || bound.Definition == nil {
		return "", errors.New("bound action is required")
	}
	transport := bound.Definition.Transport
	if bound.Action.Transport != nil {
		transport = *bound.Action.Transport
	}
	endpoint := strings.TrimSpace(transport.Endpoint)
	if transport.Kind != "tool" || endpoint == "" {
		return "", fmt.Errorf("unsupported tool transport %q", transport.Kind)
	}
	return endpoint, nil
}
