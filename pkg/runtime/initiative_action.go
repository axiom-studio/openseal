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
	InitiativeManagementSkillID      = "openseal.initiatives"
	InitiativeManagementSkillVersion = "1.0.2"
	InitiativeActionCreate           = "create"
	InitiativeActionUpdate           = "update"
	InitiativeActionPause            = "pause"
	InitiativeManagementEndpoint     = "kernel://initiatives"
)

// InitiativePortfolioStore is the portable persistence boundary required to
// validate and mutate Initiatives and their canonical Objective references.
type InitiativePortfolioStore interface {
	InitiativeStore
	PortfolioStore
}

// InitiativeKernelStore adds the Run and Action lifecycle required by the
// governed dispatcher. It is implemented by the built-in kernel stores.
type InitiativeKernelStore interface {
	KernelStore
	InitiativeStore
}

// InitiativeManagementSkill exposes project composition as typed, governed
// kernel actions. Scope and owner are intentionally absent: both are derived
// from the durable conversation Run and cannot be supplied by the model.
func InitiativeManagementSkill() *skill.Definition {
	mutable := initiativeMutableSchema()
	create := cloneMap(mutable)
	delete(create, "initiativeId")
	delete(create, "expectedRevision")
	// A SourceMonitor requires an already-known Initiative identity in its
	// Objective-owned Runbook input. It is attached through update after creation.
	delete(create, "sourceMonitors")
	return &skill.Definition{
		ID: InitiativeManagementSkillID, Version: InitiativeManagementSkillVersion,
		Name: "Initiatives", Description: "Propose governed changes to the current Agent or Team's multi-objective initiatives.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: InitiativeManagementEndpoint},
		Actions: map[string]skill.Action{
			InitiativeActionCreate: initiativeSkillAction(InitiativeActionCreate, "Propose a new draft Initiative that coordinates objectives owned by this Agent or Team.", create, []interface{}{"title", "purpose", "objectiveRefs"}),
			InitiativeActionUpdate: initiativeSkillAction(InitiativeActionUpdate, "Propose changes to an existing Initiative owned by this Agent or Team using its current revision.", mutable, []interface{}{"initiativeId", "expectedRevision"}),
			InitiativeActionPause: initiativeSkillAction(InitiativeActionPause, "Propose pausing an existing Initiative owned by this Agent or Team using its current revision.", map[string]interface{}{
				"initiativeId":     map[string]interface{}{"type": "string", "minLength": 1},
				"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
			}, []interface{}{"initiativeId", "expectedRevision"}),
		},
	}
}

func initiativeSkillAction(name, description string, properties map[string]interface{}, required []interface{}) skill.Action {
	return skill.Action{
		Name: name, Description: description, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
		Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties, "required": required},
		OutputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"resourceType": map[string]interface{}{"type": "string", "const": "initiative"},
				"operation":    map[string]interface{}{"type": "string", "enum": []interface{}{InitiativeActionCreate, InitiativeActionUpdate, InitiativeActionPause}},
				"created":      map[string]interface{}{"type": "boolean"},
				"initiative":   map[string]interface{}{"type": "object"},
			},
			"required": []interface{}{"resourceType", "operation", "created", "initiative"},
		},
	}
}

func initiativeMutableSchema() map[string]interface{} {
	stringArray := func() map[string]interface{} {
		return map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "uniqueItems": true}
	}
	resourceRefs := map[string]interface{}{"type": "array", "items": initiativeResourceReferenceSchema(), "uniqueItems": true}
	return map[string]interface{}{
		"initiativeId":     map[string]interface{}{"type": "string", "minLength": 1},
		"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
		"title":            map[string]interface{}{"type": "string", "minLength": 1},
		"purpose":          map[string]interface{}{"type": "string", "minLength": 1},
		"agentRefs":        cloneMap(resourceRefs),
		"teamRefs":         cloneMap(resourceRefs),
		"objectiveRefs":    stringArray(),
		"runRefs":          stringArray(),
		"milestones":       map[string]interface{}{"type": "array", "items": initiativeMilestoneSchema()},
		"hypotheses":       map[string]interface{}{"type": "array", "items": initiativeHypothesisSchema()},
		"sourceMonitors":   map[string]interface{}{"type": "array", "items": initiativeSourceMonitorSchema()},
		"deliverables":     map[string]interface{}{"type": "array", "items": initiativeDeliverableSchema()},
		"budget":           initiativeBudgetSchema(),
		"policy":           map[string]interface{}{"type": "object"},
	}
}

func initiativeResourceReferenceSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"kind": map[string]interface{}{"type": "string", "enum": []interface{}{
				string(ResourceKindAgentDefinition), string(ResourceKindAgentDeployment), string(ResourceKindTeamDefinition),
				string(ResourceKindTeamDeployment), string(ResourceKindArtifact), string(ResourceKindEvidence),
			}},
			"id":       map[string]interface{}{"type": "string", "minLength": 1},
			"version":  map[string]interface{}{"type": "string", "minLength": 1},
			"revision": map[string]interface{}{"type": "integer", "minimum": 1},
		},
		"required": []interface{}{"kind", "id"},
	}
}

func initiativeMilestoneSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"id": map[string]interface{}{"type": "string", "minLength": 1}, "title": map[string]interface{}{"type": "string", "minLength": 1},
			"status":        map[string]interface{}{"type": "string", "enum": []interface{}{string(MilestonePending), string(MilestoneInProgress), string(MilestoneCompleted), string(MilestoneBlocked), string(MilestoneCanceled)}},
			"objectiveRefs": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "uniqueItems": true},
			"dueAt":         map[string]interface{}{"type": "string", "format": "date-time"},
			"completedAt":   map[string]interface{}{"type": "string", "format": "date-time"},
		},
		"required": []interface{}{"id", "title", "status"},
	}
}

func initiativeHypothesisSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"id": map[string]interface{}{"type": "string", "minLength": 1}, "statement": map[string]interface{}{"type": "string", "minLength": 1},
			"confidence":   map[string]interface{}{"type": "number", "minimum": 0, "maximum": 1},
			"evidenceRefs": map[string]interface{}{"type": "array", "items": initiativeResourceReferenceSchema(), "uniqueItems": true},
			"status":       map[string]interface{}{"type": "string", "enum": []interface{}{string(HypothesisOpen), string(HypothesisSupported), string(HypothesisContradicted), string(HypothesisInconclusive)}},
			"updatedAt":    map[string]interface{}{"type": "string", "format": "date-time"},
		},
		"required": []interface{}{"id", "statement", "confidence", "status", "updatedAt"},
	}
}

func initiativeSourceMonitorSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"id": map[string]interface{}{"type": "string", "minLength": 1}, "objectiveId": map[string]interface{}{"type": "string", "minLength": 1},
			"assignedAgentId": map[string]interface{}{"type": "string", "minLength": 1}, "skillId": map[string]interface{}{"type": "string", "minLength": 1},
			"skillVersion": map[string]interface{}{"type": "string", "minLength": 1}, "action": map[string]interface{}{"type": "string", "minLength": 1},
			"sourcePolicyRef": map[string]interface{}{"type": "string", "minLength": 1},
			"deduplication":   map[string]interface{}{"type": "string", "enum": []interface{}{string(SourceMonitorDeduplicateStableSource), string(SourceMonitorDeduplicateContentDigest), string(SourceMonitorDeduplicateStableSourceAndContent)}},
		},
		"required": []interface{}{"id", "objectiveId", "assignedAgentId", "skillId", "skillVersion", "action", "sourcePolicyRef", "deduplication"},
	}
}

func initiativeDeliverableSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"id": map[string]interface{}{"type": "string", "minLength": 1}, "title": map[string]interface{}{"type": "string", "minLength": 1},
			"status":        map[string]interface{}{"type": "string", "enum": []interface{}{string(DeliverablePlanned), string(DeliverableInProgress), string(DeliverableReview), string(DeliverableDelivered), string(DeliverableCanceled)}},
			"artifactRefs":  map[string]interface{}{"type": "array", "items": initiativeResourceReferenceSchema(), "uniqueItems": true},
			"objectiveRefs": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "uniqueItems": true},
			"dueAt":         map[string]interface{}{"type": "string", "format": "date-time"},
		},
		"required": []interface{}{"id", "title", "status"},
	}
}

func initiativeBudgetSchema() map[string]interface{} {
	positive := func() map[string]interface{} { return map[string]interface{}{"type": "integer", "minimum": 1} }
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"maxAttempts": positive(), "maxTurns": positive(), "maxInputTokens": positive(), "maxOutputTokens": positive(),
			"maxTotalTokens": positive(), "maxCostMicros": positive(), "maxDurationMs": positive(), "maxActions": positive(),
			"warningPermille": map[string]interface{}{"type": "integer", "minimum": 0, "maximum": 1000},
		},
	}
}

// InitiativeActionValidator performs ownership, reference, lifecycle, and CAS
// checks before a mutation can become an approval request.
type InitiativeActionValidator struct{ store InitiativePortfolioStore }

func NewInitiativeActionValidator(store InitiativePortfolioStore) (*InitiativeActionValidator, error) {
	if store == nil {
		return nil, errors.New("initiative portfolio store is required")
	}
	return &InitiativeActionValidator{store: store}, nil
}

func (v *InitiativeActionValidator) ResolveActionProposalArguments(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if !isInitiativeAction(input.Bound) || input.Bound.Action.Name == InitiativeActionCreate {
		return nil, false, nil
	}
	arguments := cloneMap(input.Arguments)
	if _, supplied := arguments["expectedRevision"]; supplied {
		return arguments, true, nil
	}
	if v == nil || v.store == nil || input.Run == nil {
		return nil, true, errors.New("initiative action validator is not configured")
	}
	target, _ := arguments["initiativeId"].(string)
	target = strings.TrimSpace(target)
	if target == "" {
		return arguments, true, nil
	}
	current, err := NewInitiativeService(v.store, v.store).Get(ctx, input.Run.Scope, target)
	if err != nil {
		return nil, true, err
	}
	arguments["expectedRevision"] = current.Revision
	return arguments, true, nil
}

func (v *InitiativeActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isInitiativeAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.store == nil || input.Run == nil {
		return nil, errors.New("initiative action validator is not configured")
	}
	if err := input.Run.Owner.Validate(); err != nil {
		return nil, fmt.Errorf("initiative action run owner: %w", err)
	}
	preview := map[string]interface{}{"resourceType": "initiative", "operation": input.Bound.Action.Name, "owner": input.Run.Owner, "changes": cloneMap(input.Arguments)}
	service := NewInitiativeService(v.store, v.store)
	if input.Bound.Action.Name == InitiativeActionCreate {
		args, err := decodeInitiativeCreateArguments(input.Arguments)
		if err != nil {
			return nil, err
		}
		candidate := args.initiative(input.Run.Scope, input.Run.Owner)
		candidate.ID = "initiative-validation"
		candidate.Revision = 1
		if err := validateInitiativeCandidate(ctx, service, candidate); err != nil {
			return nil, err
		}
		preview["resultingStatus"] = InitiativeStatusDraft
		return preview, nil
	}
	args, err := decodeInitiativeUpdateArguments(input.Arguments)
	if err != nil {
		return nil, err
	}
	current, err := service.Get(ctx, input.Run.Scope, args.InitiativeID)
	if err != nil {
		return nil, err
	}
	if current.Owner != input.Run.Owner {
		return nil, errors.New("initiative action target is not owned by the conversation Agent or Team")
	}
	if current.Revision != args.ExpectedRevision {
		return nil, ErrInitiativeConflict
	}
	preview["initiativeId"] = current.ID
	preview["expectedRevision"] = current.Revision
	preview["current"] = map[string]interface{}{"revision": current.Revision, "title": current.Title, "purpose": current.Purpose, "status": current.Status, "objectiveRefs": current.ObjectiveRefs}
	if input.Bound.Action.Name == InitiativeActionPause {
		if current.Status != InitiativeStatusDraft && current.Status != InitiativeStatusActive {
			return nil, fmt.Errorf("initiative cannot be paused from %s", current.Status)
		}
		preview["changes"] = map[string]interface{}{"status": InitiativeStatusPaused}
		return preview, nil
	}
	if !args.hasChanges() {
		return nil, ErrInitiativeNoChanges
	}
	candidate := applyInitiativeArguments(cloneInitiative(current), args)
	if err := validateInitiativeCandidate(ctx, service, candidate); err != nil {
		return nil, err
	}
	return preview, nil
}

// InitiativeActionDispatcher executes approved Initiative actions and composes
// with the existing external and Objective dispatchers through fallback.
type InitiativeActionDispatcher struct {
	store    InitiativeKernelStore
	fallback ActionDispatcher
}

func NewInitiativeActionDispatcher(store InitiativeKernelStore, fallback ActionDispatcher) (*InitiativeActionDispatcher, error) {
	if store == nil {
		return nil, errors.New("initiative kernel store is required")
	}
	return &InitiativeActionDispatcher{store: store, fallback: fallback}, nil
}

func (d *InitiativeActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if !isInitiativeAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.store == nil || input.Call == nil {
		return nil, errors.New("initiative action dispatcher is not configured")
	}
	run, err := d.store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil || run == nil {
		if err == nil {
			err = ErrRunNotFound
		}
		return nil, err
	}
	service := NewInitiativeService(d.store, d.store)
	actor := ActivityActor{Type: "agent", ID: run.AssignedAgentID}
	if actor.ID == "" {
		actor = ActivityActor{Type: string(run.Owner.Type), ID: run.Owner.ID}
	}
	var initiative *Initiative
	created := false
	switch input.Bound.Action.Name {
	case InitiativeActionCreate:
		args, decodeErr := decodeInitiativeCreateArguments(input.Arguments)
		if decodeErr != nil {
			return nil, decodeErr
		}
		var event *ActivityEvent
		initiative, event, err = service.Create(ctx, CreateInitiativeRequest{Initiative: args.initiative(run.Scope, run.Owner), IdempotencyKey: input.Call.IdempotencyKey, Actor: actor, Visibility: ActivityVisibilityScope})
		created = event != nil
	case InitiativeActionUpdate, InitiativeActionPause:
		args, decodeErr := decodeInitiativeUpdateArguments(input.Arguments)
		if decodeErr != nil {
			return nil, decodeErr
		}
		current, getErr := service.Get(ctx, run.Scope, args.InitiativeID)
		if getErr != nil {
			return nil, getErr
		}
		if current.Owner != run.Owner {
			return nil, errors.New("initiative action target is not owned by the conversation Agent or Team")
		}
		if input.Bound.Action.Name == InitiativeActionPause && current.Status != InitiativeStatusDraft && current.Status != InitiativeStatusActive {
			if current.Status != InitiativeStatusPaused || current.Revision != args.ExpectedRevision+1 {
				return nil, fmt.Errorf("initiative cannot be paused from %s", current.Status)
			}
		}
		request := args.updateRequest(actor)
		if input.Bound.Action.Name == InitiativeActionPause {
			paused := InitiativeStatusPaused
			request.Status = &paused
		}
		initiative, _, err = service.Patch(ctx, run.Scope, args.InitiativeID, request)
		if errors.Is(err, ErrInitiativeConflict) {
			latest, getLatestErr := service.Get(ctx, run.Scope, args.InitiativeID)
			if getLatestErr == nil && initiativeActionAlreadyApplied(latest, args, input.Bound.Action.Name) {
				initiative, err = latest, nil
			}
		}
	default:
		err = errors.New("unsupported initiative action")
	}
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"resourceType": "initiative", "operation": input.Bound.Action.Name, "created": created, "initiative": initiative}, nil
}

type initiativeCreateArguments struct {
	Title         string                  `json:"title"`
	Purpose       string                  `json:"purpose"`
	AgentRefs     []ResourceReference     `json:"agentRefs,omitempty"`
	TeamRefs      []ResourceReference     `json:"teamRefs,omitempty"`
	ObjectiveRefs []string                `json:"objectiveRefs"`
	RunRefs       []string                `json:"runRefs,omitempty"`
	Milestones    []InitiativeMilestone   `json:"milestones,omitempty"`
	Hypotheses    []InitiativeHypothesis  `json:"hypotheses,omitempty"`
	Deliverables  []InitiativeDeliverable `json:"deliverables,omitempty"`
	Budget        *BudgetPolicy           `json:"budget,omitempty"`
	Policy        map[string]interface{}  `json:"policy,omitempty"`
}

func (a initiativeCreateArguments) initiative(scope Scope, owner ObjectiveOwner) *Initiative {
	return &Initiative{Scope: scope, Owner: owner, Title: a.Title, Purpose: a.Purpose, Status: InitiativeStatusDraft, AgentRefs: a.AgentRefs, TeamRefs: a.TeamRefs, ObjectiveRefs: a.ObjectiveRefs, RunRefs: a.RunRefs, Milestones: a.Milestones, Hypotheses: a.Hypotheses, Deliverables: a.Deliverables, Budget: a.Budget, Policy: a.Policy}
}

type initiativeUpdateArguments struct {
	InitiativeID     string                    `json:"initiativeId"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	Title            *string                   `json:"title,omitempty"`
	Purpose          *string                   `json:"purpose,omitempty"`
	AgentRefs        *[]ResourceReference      `json:"agentRefs,omitempty"`
	TeamRefs         *[]ResourceReference      `json:"teamRefs,omitempty"`
	ObjectiveRefs    *[]string                 `json:"objectiveRefs,omitempty"`
	RunRefs          *[]string                 `json:"runRefs,omitempty"`
	Milestones       *[]InitiativeMilestone    `json:"milestones,omitempty"`
	Hypotheses       *[]InitiativeHypothesis   `json:"hypotheses,omitempty"`
	SourceMonitors   *[]SourceMonitorReference `json:"sourceMonitors,omitempty"`
	Deliverables     *[]InitiativeDeliverable  `json:"deliverables,omitempty"`
	Budget           *BudgetPolicy             `json:"budget,omitempty"`
	Policy           map[string]interface{}    `json:"policy,omitempty"`
}

func (a initiativeUpdateArguments) hasChanges() bool {
	return a.Title != nil || a.Purpose != nil || a.AgentRefs != nil || a.TeamRefs != nil || a.ObjectiveRefs != nil || a.RunRefs != nil || a.Milestones != nil || a.Hypotheses != nil || a.SourceMonitors != nil || a.Deliverables != nil || a.Budget != nil || a.Policy != nil
}

func (a initiativeUpdateArguments) updateRequest(actor ActivityActor) UpdateInitiativeRequest {
	return UpdateInitiativeRequest{ExpectedRevision: a.ExpectedRevision, Title: a.Title, Purpose: a.Purpose, AgentRefs: a.AgentRefs, TeamRefs: a.TeamRefs, ObjectiveRefs: a.ObjectiveRefs, RunRefs: a.RunRefs, Milestones: a.Milestones, Hypotheses: a.Hypotheses, SourceMonitors: a.SourceMonitors, Deliverables: a.Deliverables, Budget: a.Budget, Policy: a.Policy, Actor: actor, Visibility: ActivityVisibilityScope}
}

func applyInitiativeArguments(candidate *Initiative, args initiativeUpdateArguments) *Initiative {
	if args.Title != nil {
		candidate.Title = *args.Title
	}
	if args.Purpose != nil {
		candidate.Purpose = *args.Purpose
	}
	if args.AgentRefs != nil {
		candidate.AgentRefs = *args.AgentRefs
	}
	if args.TeamRefs != nil {
		candidate.TeamRefs = *args.TeamRefs
	}
	if args.ObjectiveRefs != nil {
		candidate.ObjectiveRefs = *args.ObjectiveRefs
	}
	if args.RunRefs != nil {
		candidate.RunRefs = *args.RunRefs
	}
	if args.Milestones != nil {
		candidate.Milestones = *args.Milestones
	}
	if args.Hypotheses != nil {
		candidate.Hypotheses = *args.Hypotheses
	}
	if args.SourceMonitors != nil {
		candidate.SourceMonitors = *args.SourceMonitors
	}
	if args.Deliverables != nil {
		candidate.Deliverables = *args.Deliverables
	}
	if args.Budget != nil {
		candidate.Budget = args.Budget
	}
	if args.Policy != nil {
		candidate.Policy = args.Policy
	}
	return candidate
}

func decodeInitiativeCreateArguments(arguments map[string]interface{}) (initiativeCreateArguments, error) {
	var args initiativeCreateArguments
	return args, decodeInitiativeArguments(arguments, &args)
}

func decodeInitiativeUpdateArguments(arguments map[string]interface{}) (initiativeUpdateArguments, error) {
	var args initiativeUpdateArguments
	if err := decodeInitiativeArguments(arguments, &args); err != nil {
		return args, err
	}
	if strings.TrimSpace(args.InitiativeID) == "" || args.ExpectedRevision < 1 {
		return args, errors.New("initiativeId and expectedRevision are required")
	}
	return args, nil
}

func decodeInitiativeArguments(arguments map[string]interface{}, target interface{}) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid initiative action: %w", err)
	}
	return nil
}

func validateInitiativeCandidate(ctx context.Context, service *InitiativeService, candidate *Initiative) error {
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInitiative, err)
	}
	if err := service.validateObjectives(ctx, candidate); err != nil {
		return err
	}
	return service.validateSourceMonitors(ctx, candidate)
}

func initiativeActionAlreadyApplied(i *Initiative, args initiativeUpdateArguments, action string) bool {
	if i == nil || i.Revision != args.ExpectedRevision+1 {
		return false
	}
	if action == InitiativeActionPause {
		return i.Status == InitiativeStatusPaused
	}
	return (args.Title == nil || i.Title == *args.Title) && (args.Purpose == nil || i.Purpose == *args.Purpose) &&
		(args.AgentRefs == nil || reflect.DeepEqual(i.AgentRefs, *args.AgentRefs)) && (args.TeamRefs == nil || reflect.DeepEqual(i.TeamRefs, *args.TeamRefs)) &&
		(args.ObjectiveRefs == nil || reflect.DeepEqual(i.ObjectiveRefs, *args.ObjectiveRefs)) && (args.RunRefs == nil || reflect.DeepEqual(i.RunRefs, *args.RunRefs)) &&
		(args.Milestones == nil || reflect.DeepEqual(i.Milestones, *args.Milestones)) && (args.Hypotheses == nil || reflect.DeepEqual(i.Hypotheses, *args.Hypotheses)) &&
		(args.SourceMonitors == nil || reflect.DeepEqual(i.SourceMonitors, *args.SourceMonitors)) && (args.Deliverables == nil || reflect.DeepEqual(i.Deliverables, *args.Deliverables)) &&
		(args.Budget == nil || reflect.DeepEqual(i.Budget, args.Budget)) && (args.Policy == nil || reflect.DeepEqual(i.Policy, args.Policy))
}

func isInitiativeAction(bound *skill.BoundAction) bool {
	if bound == nil || bound.Definition == nil || bound.Definition.ID != InitiativeManagementSkillID || bound.Definition.Version != InitiativeManagementSkillVersion {
		return false
	}
	switch bound.Action.Name {
	case InitiativeActionCreate, InitiativeActionUpdate, InitiativeActionPause:
		return true
	}
	return false
}
