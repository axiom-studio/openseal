package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	ObjectiveManagementSkillID      = "openseal.objectives"
	ObjectiveManagementSkillVersion = "1.0.1"
	ObjectiveActionCreate           = "create"
	ObjectiveActionUpdate           = "update"
	ObjectiveActionPause            = "pause"
	ObjectiveManagementEndpoint     = "kernel://objectives"
)

// ObjectiveManagementSkill is the portable, model-visible contract for
// managing the Objective portfolio owned by the current Agent or Team. Owner
// is deliberately absent from every input schema: the kernel derives it from
// the durable conversation Run.
func ObjectiveManagementSkill() *skill.Definition {
	fields := objectiveMutableSchema()
	createFields := cloneMap(fields)
	delete(createFields, "objectiveId")
	delete(createFields, "expectedRevision")
	updateFields := cloneMap(fields)
	return &skill.Definition{
		ID: ObjectiveManagementSkillID, Version: ObjectiveManagementSkillVersion,
		Name: "Objectives", Description: "Propose governed changes to the current Agent or Team's objectives.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: ObjectiveManagementEndpoint},
		Actions: map[string]skill.Action{
			ObjectiveActionCreate: objectiveSkillAction(ObjectiveActionCreate, "Propose a new draft objective for this Agent or Team. The change may require human approval.", createFields, []interface{}{"title", "goal"}),
			ObjectiveActionUpdate: objectiveSkillAction(ObjectiveActionUpdate, "Propose changes to an existing objective owned by this Agent or Team using its current revision.", updateFields, []interface{}{"objectiveId", "expectedRevision"}),
			ObjectiveActionPause: objectiveSkillAction(ObjectiveActionPause, "Propose pausing an existing objective owned by this Agent or Team using its current revision.", map[string]interface{}{
				"objectiveId":      map[string]interface{}{"type": "string", "minLength": 1},
				"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
			}, []interface{}{"objectiveId", "expectedRevision"}),
		},
	}
}

func objectiveSkillAction(name, description string, properties map[string]interface{}, required []interface{}) skill.Action {
	return skill.Action{
		Name: name, Description: description, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
		Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties, "required": required},
		OutputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"resourceType": map[string]interface{}{"type": "string", "const": "objective"},
				"operation":    map[string]interface{}{"type": "string", "enum": []interface{}{ObjectiveActionCreate, ObjectiveActionUpdate, ObjectiveActionPause}},
				"created":      map[string]interface{}{"type": "boolean"},
				"objective":    map[string]interface{}{"type": "object"},
			},
			"required": []interface{}{"resourceType", "operation", "created", "objective"},
		},
	}
}

func objectiveMutableSchema() map[string]interface{} {
	return map[string]interface{}{
		"objectiveId":      map[string]interface{}{"type": "string", "minLength": 1},
		"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
		"title":            map[string]interface{}{"type": "string", "minLength": 1},
		"goal":             map[string]interface{}{"type": "string", "minLength": 1},
		"priority":         map[string]interface{}{"type": "integer", "minimum": 0},
		"cadence":          map[string]interface{}{"type": "object"},
		"eventRules":       map[string]interface{}{"type": "object"},
		"budget":           map[string]interface{}{"type": "object"},
		"constraints":      map[string]interface{}{"type": "object"},
		"successCriteria":  map[string]interface{}{"type": "object"},
	}
}

// ObjectiveActionValidator enforces ownership, target existence, lifecycle,
// and CAS before an Objective mutation can become an approval request.
type ObjectiveActionValidator struct{ store PortfolioStore }

func NewObjectiveActionValidator(store PortfolioStore) (*ObjectiveActionValidator, error) {
	if store == nil {
		return nil, errors.New("portfolio store is required")
	}
	return &ObjectiveActionValidator{store: store}, nil
}

func (v *ObjectiveActionValidator) ResolveActionProposalArguments(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if !isObjectiveAction(input.Bound) || input.Bound.Action.Name == ObjectiveActionCreate {
		return nil, false, nil
	}
	arguments := cloneMap(input.Arguments)
	if _, supplied := arguments["expectedRevision"]; supplied {
		return arguments, true, nil
	}
	if v == nil || v.store == nil || input.Run == nil {
		return nil, true, errors.New("objective action validator is not configured")
	}
	target, _ := arguments["objectiveId"].(string)
	target = strings.TrimSpace(target)
	if target == "" {
		return arguments, true, nil
	}
	objective, err := v.store.GetObjective(ctx, input.Run.Scope, target)
	if err != nil {
		return nil, true, err
	}
	if objective == nil {
		return nil, true, ErrObjectiveNotFound
	}
	arguments["expectedRevision"] = objective.Revision
	return arguments, true, nil
}

func (v *ObjectiveActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isObjectiveAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.store == nil || input.Run == nil {
		return nil, errors.New("objective action validator is not configured")
	}
	if err := input.Run.Owner.Validate(); err != nil {
		return nil, fmt.Errorf("objective action run owner: %w", err)
	}
	preview := map[string]interface{}{"resourceType": "objective", "operation": input.Bound.Action.Name, "owner": input.Run.Owner, "changes": cloneMap(input.Arguments)}
	if input.Bound.Action.Name == ObjectiveActionCreate {
		preview["resultingStatus"] = ObjectiveStatusDraft
		return preview, validateObjectiveCreateArguments(input.Arguments)
	}
	target, expectedRevision, err := objectiveTarget(input.Arguments)
	if err != nil {
		return nil, err
	}
	objective, err := v.store.GetObjective(ctx, input.Run.Scope, target)
	if err != nil {
		return nil, err
	}
	if objective == nil {
		return nil, ErrObjectiveNotFound
	}
	if objective.Owner != input.Run.Owner {
		return nil, errors.New("objective action target is not owned by the conversation Agent or Team")
	}
	if objective.Revision != expectedRevision {
		return nil, ErrRevisionConflict
	}
	if input.Bound.Action.Name == ObjectiveActionUpdate {
		var args objectiveUpdateArguments
		if err := decodeObjectiveArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		if !args.hasChanges() {
			return nil, errors.New("objective update requires at least one changed field")
		}
		candidate := cloneObjective(objective)
		applyObjectiveUpdate(candidate, args.updateRequest(ActivityActor{}))
		if err := candidate.Validate(); err != nil {
			return nil, err
		}
	}
	if input.Bound.Action.Name == ObjectiveActionPause && !canTransitionObjective(objective.Status, ObjectiveStatusPaused) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidObjectiveTransition, objective.Status, ObjectiveStatusPaused)
	}
	preview["objectiveId"] = objective.ID
	preview["expectedRevision"] = expectedRevision
	preview["current"] = map[string]interface{}{"revision": objective.Revision, "title": objective.Title, "goal": objective.Goal, "status": objective.Status, "priority": objective.Priority}
	if input.Bound.Action.Name == ObjectiveActionPause {
		preview["changes"] = map[string]interface{}{"status": ObjectiveStatusPaused}
	}
	return preview, nil
}

// ObjectiveActionDispatcher executes the approved kernel action. Non-objective
// calls are delegated, allowing one worker to compose kernel and external
// Skills without a parallel execution runtime.
type ObjectiveActionDispatcher struct {
	store    KernelStore
	fallback ActionDispatcher
}

func NewObjectiveActionDispatcher(store KernelStore, fallback ActionDispatcher) (*ObjectiveActionDispatcher, error) {
	if store == nil {
		return nil, errors.New("kernel store is required")
	}
	return &ObjectiveActionDispatcher{store: store, fallback: fallback}, nil
}

func (d *ObjectiveActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if !isObjectiveAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.store == nil || input.Call == nil {
		return nil, errors.New("objective action dispatcher is not configured")
	}
	run, err := d.store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil || run == nil {
		if err == nil {
			err = ErrRunNotFound
		}
		return nil, err
	}
	service := NewPortfolioService(d.store)
	actor := ActivityActor{Type: "agent", ID: run.AssignedAgentID}
	if actor.ID == "" {
		actor = ActivityActor{Type: "team", ID: run.Owner.ID}
	}
	var objective *Objective
	created := false
	switch input.Bound.Action.Name {
	case ObjectiveActionCreate:
		var args objectiveCreateArguments
		if err := decodeObjectiveArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		result, err := service.CreateObjectiveIdempotent(ctx, CreateObjectiveRequest{
			Scope: run.Scope, Owner: run.Owner, Title: args.Title, Goal: args.Goal, Status: ObjectiveStatusDraft, Priority: args.Priority,
			Cadence: args.Cadence, EventRules: args.EventRules, Budget: args.Budget, Constraints: args.Constraints,
			SuccessCriteria: args.SuccessCriteria, IdempotencyKey: input.Call.IdempotencyKey, Actor: actor, Visibility: ActivityVisibilityScope,
		})
		if err != nil {
			return nil, err
		}
		objective, created = result.Objective, result.Created
	case ObjectiveActionUpdate, ObjectiveActionPause:
		var args objectiveUpdateArguments
		if err := decodeObjectiveArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		current, err := service.GetObjective(ctx, run.Scope, args.ObjectiveID)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, ErrObjectiveNotFound
		}
		if current.Owner != run.Owner {
			return nil, errors.New("objective action target is not owned by the conversation Agent or Team")
		}
		request := args.updateRequest(actor)
		if input.Bound.Action.Name == ObjectiveActionPause {
			paused := ObjectiveStatusPaused
			request.Status = &paused
		}
		objective, err = service.UpdateObjective(ctx, run.Scope, args.ObjectiveID, request)
		if errors.Is(err, ErrRevisionConflict) {
			latest, getErr := service.GetObjective(ctx, run.Scope, args.ObjectiveID)
			if getErr == nil && objectiveActionAlreadyApplied(latest, args, input.Bound.Action.Name) {
				objective, err = latest, nil
			}
		}
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unsupported objective action")
	}
	return map[string]interface{}{"resourceType": "objective", "operation": input.Bound.Action.Name, "created": created, "objective": objective}, nil
}

type objectiveCreateArguments struct {
	Title           string                 `json:"title"`
	Goal            string                 `json:"goal"`
	Priority        int                    `json:"priority,omitempty"`
	Cadence         *ObjectiveCadence      `json:"cadence,omitempty"`
	EventRules      map[string]interface{} `json:"eventRules,omitempty"`
	Budget          *BudgetPolicy          `json:"budget,omitempty"`
	Constraints     map[string]interface{} `json:"constraints,omitempty"`
	SuccessCriteria map[string]interface{} `json:"successCriteria,omitempty"`
}

type objectiveUpdateArguments struct {
	ObjectiveID      string                 `json:"objectiveId"`
	ExpectedRevision int64                  `json:"expectedRevision"`
	Title            *string                `json:"title,omitempty"`
	Goal             *string                `json:"goal,omitempty"`
	Priority         *int                   `json:"priority,omitempty"`
	Cadence          *ObjectiveCadence      `json:"cadence,omitempty"`
	EventRules       map[string]interface{} `json:"eventRules,omitempty"`
	Budget           *BudgetPolicy          `json:"budget,omitempty"`
	Constraints      map[string]interface{} `json:"constraints,omitempty"`
	SuccessCriteria  map[string]interface{} `json:"successCriteria,omitempty"`
}

func (a objectiveUpdateArguments) hasChanges() bool {
	return a.Title != nil || a.Goal != nil || a.Priority != nil || a.Cadence != nil || a.EventRules != nil || a.Budget != nil || a.Constraints != nil || a.SuccessCriteria != nil
}

func (a objectiveUpdateArguments) updateRequest(actor ActivityActor) UpdateObjectiveRequest {
	return UpdateObjectiveRequest{
		ExpectedRevision: a.ExpectedRevision, Title: a.Title, Goal: a.Goal, Priority: a.Priority, Cadence: a.Cadence,
		EventRules: a.EventRules, Budget: a.Budget, Constraints: a.Constraints, SuccessCriteria: a.SuccessCriteria,
		Actor: actor, Visibility: ActivityVisibilityScope, Summary: "Objective changed through conversation",
	}
}

func validateObjectiveCreateArguments(arguments map[string]interface{}) error {
	var args objectiveCreateArguments
	if err := decodeObjectiveArguments(arguments, &args); err != nil {
		return err
	}
	probe := &Objective{Scope: Scope{Kind: "validation", ID: "validation"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "validation"}, Title: args.Title, Goal: args.Goal, Status: ObjectiveStatusDraft, Priority: args.Priority, Cadence: args.Cadence, EventRules: args.EventRules, Budget: args.Budget, Constraints: args.Constraints, SuccessCriteria: args.SuccessCriteria, Revision: 1}
	return probe.Validate()
}

func objectiveTarget(arguments map[string]interface{}) (string, int64, error) {
	var args objectiveUpdateArguments
	if err := decodeObjectiveArguments(arguments, &args); err != nil {
		return "", 0, err
	}
	if strings.TrimSpace(args.ObjectiveID) == "" || args.ExpectedRevision < 1 {
		return "", 0, errors.New("objectiveId and expectedRevision are required")
	}
	return args.ObjectiveID, args.ExpectedRevision, nil
}

func decodeObjectiveArguments(arguments map[string]interface{}, target interface{}) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid objective action: %w", err)
	}
	return nil
}

func objectiveActionAlreadyApplied(objective *Objective, args objectiveUpdateArguments, action string) bool {
	if objective == nil || objective.Revision != args.ExpectedRevision+1 {
		return false
	}
	if action == ObjectiveActionPause {
		return objective.Status == ObjectiveStatusPaused
	}
	return (args.Title == nil || objective.Title == *args.Title) && (args.Goal == nil || objective.Goal == *args.Goal) &&
		(args.Priority == nil || objective.Priority == *args.Priority) && (args.Cadence == nil || reflect.DeepEqual(objective.Cadence, args.Cadence)) &&
		(args.EventRules == nil || reflect.DeepEqual(objective.EventRules, args.EventRules)) && (args.Budget == nil || reflect.DeepEqual(objective.Budget, args.Budget)) &&
		(args.Constraints == nil || reflect.DeepEqual(objective.Constraints, args.Constraints)) && (args.SuccessCriteria == nil || reflect.DeepEqual(objective.SuccessCriteria, args.SuccessCriteria))
}

func isObjectiveAction(bound *skill.BoundAction) bool {
	if bound == nil || bound.Definition == nil || bound.Definition.ID != ObjectiveManagementSkillID || bound.Definition.Version != ObjectiveManagementSkillVersion {
		return false
	}
	switch bound.Action.Name {
	case ObjectiveActionCreate, ObjectiveActionUpdate, ObjectiveActionPause:
		return true
	}
	return false
}
