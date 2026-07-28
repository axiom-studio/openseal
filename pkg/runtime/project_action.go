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
	ProjectManagementSkillID      = "openseal.projects"
	ProjectManagementSkillVersion = "1.0.2"
	ProjectActionCreate           = "create"
	ProjectActionUpdate           = "update"
	ProjectActionPause            = "pause"
	ProjectManagementEndpoint     = "kernel://projects"
)

// ProjectPortfolioStore is the portable persistence boundary required to
// validate and mutate Projects and their canonical Objective references.
type ProjectPortfolioStore interface {
	ProjectStore
	PortfolioStore
}

// ProjectKernelStore adds the Run and Action lifecycle required by the
// governed dispatcher. It is implemented by the built-in kernel stores.
type ProjectKernelStore interface {
	KernelStore
	ProjectStore
}

// ProjectManagementSkill exposes project composition as typed, governed
// kernel actions. Scope and owner are intentionally absent: both are derived
// from the durable conversation Run and cannot be supplied by the model.
func ProjectManagementSkill() *skill.Definition {
	mutable := projectMutableSchema()
	create := cloneMap(mutable)
	delete(create, "projectId")
	delete(create, "expectedRevision")
	// A SourceMonitor requires an already-known Project identity in its
	// Objective-owned Runbook input. It is attached through update after creation.
	delete(create, "sourceMonitors")
	return &skill.Definition{
		ID: ProjectManagementSkillID, Version: ProjectManagementSkillVersion,
		Name: "Projects", Description: "Propose governed changes to the current Agent or Team's multi-objective projects.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: ProjectManagementEndpoint},
		Actions: map[string]skill.Action{
			ProjectActionCreate: projectSkillAction(ProjectActionCreate, "Propose a new draft Project that coordinates objectives owned by this Agent or Team.", create, []interface{}{"title", "purpose", "objectiveRefs"}),
			ProjectActionUpdate: projectSkillAction(ProjectActionUpdate, "Propose changes to an existing Project owned by this Agent or Team using its current revision.", mutable, []interface{}{"projectId", "expectedRevision"}),
			ProjectActionPause: projectSkillAction(ProjectActionPause, "Propose pausing an existing Project owned by this Agent or Team using its current revision.", map[string]interface{}{
				"projectId":        map[string]interface{}{"type": "string", "minLength": 1},
				"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
			}, []interface{}{"projectId", "expectedRevision"}),
		},
	}
}

func projectSkillAction(name, description string, properties map[string]interface{}, required []interface{}) skill.Action {
	return skill.Action{
		Name: name, Description: description, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
		Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties, "required": required},
		OutputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"resourceType": map[string]interface{}{"type": "string", "const": "project"},
				"operation":    map[string]interface{}{"type": "string", "enum": []interface{}{ProjectActionCreate, ProjectActionUpdate, ProjectActionPause}},
				"created":      map[string]interface{}{"type": "boolean"},
				"project":      map[string]interface{}{"type": "object"},
			},
			"required": []interface{}{"resourceType", "operation", "created", "project"},
		},
	}
}

func projectMutableSchema() map[string]interface{} {
	stringArray := func() map[string]interface{} {
		return map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "uniqueItems": true}
	}
	resourceRefs := map[string]interface{}{"type": "array", "items": projectResourceReferenceSchema(), "uniqueItems": true}
	return map[string]interface{}{
		"projectId":        map[string]interface{}{"type": "string", "minLength": 1},
		"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
		"title":            map[string]interface{}{"type": "string", "minLength": 1},
		"purpose":          map[string]interface{}{"type": "string", "minLength": 1},
		"agentRefs":        cloneMap(resourceRefs),
		"teamRefs":         cloneMap(resourceRefs),
		"objectiveRefs":    stringArray(),
		"runRefs":          stringArray(),
		"milestones":       map[string]interface{}{"type": "array", "items": projectMilestoneSchema()},
		"hypotheses":       map[string]interface{}{"type": "array", "items": projectHypothesisSchema()},
		"sourceMonitors":   map[string]interface{}{"type": "array", "items": projectSourceMonitorSchema()},
		"deliverables":     map[string]interface{}{"type": "array", "items": projectDeliverableSchema()},
		"budget":           projectBudgetSchema(),
		"policy":           map[string]interface{}{"type": "object"},
	}
}

func projectResourceReferenceSchema() map[string]interface{} {
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

func projectMilestoneSchema() map[string]interface{} {
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

func projectHypothesisSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"id": map[string]interface{}{"type": "string", "minLength": 1}, "statement": map[string]interface{}{"type": "string", "minLength": 1},
			"confidence":   map[string]interface{}{"type": "number", "minimum": 0, "maximum": 1},
			"evidenceRefs": map[string]interface{}{"type": "array", "items": projectResourceReferenceSchema(), "uniqueItems": true},
			"status":       map[string]interface{}{"type": "string", "enum": []interface{}{string(HypothesisOpen), string(HypothesisSupported), string(HypothesisContradicted), string(HypothesisInconclusive)}},
			"updatedAt":    map[string]interface{}{"type": "string", "format": "date-time"},
		},
		"required": []interface{}{"id", "statement", "confidence", "status", "updatedAt"},
	}
}

func projectSourceMonitorSchema() map[string]interface{} {
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

func projectDeliverableSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"id": map[string]interface{}{"type": "string", "minLength": 1}, "title": map[string]interface{}{"type": "string", "minLength": 1},
			"status":        map[string]interface{}{"type": "string", "enum": []interface{}{string(DeliverablePlanned), string(DeliverableInProgress), string(DeliverableReview), string(DeliverableDelivered), string(DeliverableCanceled)}},
			"artifactRefs":  map[string]interface{}{"type": "array", "items": projectResourceReferenceSchema(), "uniqueItems": true},
			"objectiveRefs": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "uniqueItems": true},
			"dueAt":         map[string]interface{}{"type": "string", "format": "date-time"},
		},
		"required": []interface{}{"id", "title", "status"},
	}
}

func projectBudgetSchema() map[string]interface{} {
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

// ProjectActionValidator performs ownership, reference, lifecycle, and CAS
// checks before a mutation can become an approval request.
type ProjectActionValidator struct{ store ProjectPortfolioStore }

func NewProjectActionValidator(store ProjectPortfolioStore) (*ProjectActionValidator, error) {
	if store == nil {
		return nil, errors.New("project portfolio store is required")
	}
	return &ProjectActionValidator{store: store}, nil
}

func (v *ProjectActionValidator) ResolveActionProposalArguments(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if !isProjectAction(input.Bound) || input.Bound.Action.Name == ProjectActionCreate {
		return nil, false, nil
	}
	arguments := cloneMap(input.Arguments)
	if _, supplied := arguments["expectedRevision"]; supplied {
		return arguments, true, nil
	}
	if v == nil || v.store == nil || input.Run == nil {
		return nil, true, errors.New("project action validator is not configured")
	}
	target, _ := arguments["projectId"].(string)
	target = strings.TrimSpace(target)
	if target == "" {
		return arguments, true, nil
	}
	current, err := NewProjectService(v.store, v.store).Get(ctx, input.Run.Scope, target)
	if err != nil {
		return nil, true, err
	}
	arguments["expectedRevision"] = current.Revision
	return arguments, true, nil
}

func (v *ProjectActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isProjectAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.store == nil || input.Run == nil {
		return nil, errors.New("project action validator is not configured")
	}
	if err := input.Run.Owner.Validate(); err != nil {
		return nil, fmt.Errorf("project action run owner: %w", err)
	}
	preview := map[string]interface{}{"resourceType": "project", "operation": input.Bound.Action.Name, "owner": input.Run.Owner, "changes": cloneMap(input.Arguments)}
	service := NewProjectService(v.store, v.store)
	if input.Bound.Action.Name == ProjectActionCreate {
		args, err := decodeProjectCreateArguments(input.Arguments)
		if err != nil {
			return nil, err
		}
		candidate := args.project(input.Run.Scope, input.Run.Owner)
		candidate.ID = "project-validation"
		candidate.Revision = 1
		if err := validateProjectCandidate(ctx, service, candidate); err != nil {
			return nil, err
		}
		preview["resultingStatus"] = ProjectStatusDraft
		return preview, nil
	}
	args, err := decodeProjectUpdateArguments(input.Arguments)
	if err != nil {
		return nil, err
	}
	current, err := service.Get(ctx, input.Run.Scope, args.ProjectID)
	if err != nil {
		return nil, err
	}
	if current.Owner != input.Run.Owner {
		return nil, errors.New("project action target is not owned by the conversation Agent or Team")
	}
	if current.Revision != args.ExpectedRevision {
		return nil, ErrProjectConflict
	}
	preview["projectId"] = current.ID
	preview["expectedRevision"] = current.Revision
	preview["current"] = map[string]interface{}{"revision": current.Revision, "title": current.Title, "purpose": current.Purpose, "status": current.Status, "objectiveRefs": current.ObjectiveRefs}
	if input.Bound.Action.Name == ProjectActionPause {
		if current.Status != ProjectStatusDraft && current.Status != ProjectStatusActive {
			return nil, fmt.Errorf("project cannot be paused from %s", current.Status)
		}
		preview["changes"] = map[string]interface{}{"status": ProjectStatusPaused}
		return preview, nil
	}
	if !args.hasChanges() {
		return nil, ErrProjectNoChanges
	}
	candidate := applyProjectArguments(cloneProject(current), args)
	if err := validateProjectCandidate(ctx, service, candidate); err != nil {
		return nil, err
	}
	return preview, nil
}

// ProjectActionDispatcher executes approved Project actions and composes
// with the existing external and Objective dispatchers through fallback.
type ProjectActionDispatcher struct {
	store    ProjectKernelStore
	fallback ActionDispatcher
}

func NewProjectActionDispatcher(store ProjectKernelStore, fallback ActionDispatcher) (*ProjectActionDispatcher, error) {
	if store == nil {
		return nil, errors.New("project kernel store is required")
	}
	return &ProjectActionDispatcher{store: store, fallback: fallback}, nil
}

func (d *ProjectActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if !isProjectAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.store == nil || input.Call == nil {
		return nil, errors.New("project action dispatcher is not configured")
	}
	run, err := d.store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil || run == nil {
		if err == nil {
			err = ErrRunNotFound
		}
		return nil, err
	}
	service := NewProjectService(d.store, d.store)
	actor := ActivityActor{Type: "agent", ID: run.AssignedAgentID}
	if actor.ID == "" {
		actor = ActivityActor{Type: string(run.Owner.Type), ID: run.Owner.ID}
	}
	var project *Project
	created := false
	switch input.Bound.Action.Name {
	case ProjectActionCreate:
		args, decodeErr := decodeProjectCreateArguments(input.Arguments)
		if decodeErr != nil {
			return nil, decodeErr
		}
		var event *ActivityEvent
		project, event, err = service.Create(ctx, CreateProjectRequest{Project: args.project(run.Scope, run.Owner), IdempotencyKey: input.Call.IdempotencyKey, Actor: actor, Visibility: ActivityVisibilityScope})
		created = event != nil
	case ProjectActionUpdate, ProjectActionPause:
		args, decodeErr := decodeProjectUpdateArguments(input.Arguments)
		if decodeErr != nil {
			return nil, decodeErr
		}
		current, getErr := service.Get(ctx, run.Scope, args.ProjectID)
		if getErr != nil {
			return nil, getErr
		}
		if current.Owner != run.Owner {
			return nil, errors.New("project action target is not owned by the conversation Agent or Team")
		}
		if input.Bound.Action.Name == ProjectActionPause && current.Status != ProjectStatusDraft && current.Status != ProjectStatusActive {
			if current.Status != ProjectStatusPaused || current.Revision != args.ExpectedRevision+1 {
				return nil, fmt.Errorf("project cannot be paused from %s", current.Status)
			}
		}
		request := args.updateRequest(actor)
		if input.Bound.Action.Name == ProjectActionPause {
			paused := ProjectStatusPaused
			request.Status = &paused
		}
		project, _, err = service.Patch(ctx, run.Scope, args.ProjectID, request)
		if errors.Is(err, ErrProjectConflict) {
			latest, getLatestErr := service.Get(ctx, run.Scope, args.ProjectID)
			if getLatestErr == nil && projectActionAlreadyApplied(latest, args, input.Bound.Action.Name) {
				project, err = latest, nil
			}
		}
	default:
		err = errors.New("unsupported project action")
	}
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"resourceType": "project", "operation": input.Bound.Action.Name, "created": created, "project": project}, nil
}

type projectCreateArguments struct {
	Title         string                 `json:"title"`
	Purpose       string                 `json:"purpose"`
	AgentRefs     []ResourceReference    `json:"agentRefs,omitempty"`
	TeamRefs      []ResourceReference    `json:"teamRefs,omitempty"`
	ObjectiveRefs []string               `json:"objectiveRefs"`
	RunRefs       []string               `json:"runRefs,omitempty"`
	Milestones    []ProjectMilestone     `json:"milestones,omitempty"`
	Hypotheses    []ProjectHypothesis    `json:"hypotheses,omitempty"`
	Deliverables  []ProjectDeliverable   `json:"deliverables,omitempty"`
	Budget        *BudgetPolicy          `json:"budget,omitempty"`
	Policy        map[string]interface{} `json:"policy,omitempty"`
}

func (a projectCreateArguments) project(scope Scope, owner ObjectiveOwner) *Project {
	return &Project{Scope: scope, Owner: owner, Title: a.Title, Purpose: a.Purpose, Status: ProjectStatusDraft, AgentRefs: a.AgentRefs, TeamRefs: a.TeamRefs, ObjectiveRefs: a.ObjectiveRefs, RunRefs: a.RunRefs, Milestones: a.Milestones, Hypotheses: a.Hypotheses, Deliverables: a.Deliverables, Budget: a.Budget, Policy: a.Policy}
}

type projectUpdateArguments struct {
	ProjectID        string                    `json:"projectId"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	Title            *string                   `json:"title,omitempty"`
	Purpose          *string                   `json:"purpose,omitempty"`
	AgentRefs        *[]ResourceReference      `json:"agentRefs,omitempty"`
	TeamRefs         *[]ResourceReference      `json:"teamRefs,omitempty"`
	ObjectiveRefs    *[]string                 `json:"objectiveRefs,omitempty"`
	RunRefs          *[]string                 `json:"runRefs,omitempty"`
	Milestones       *[]ProjectMilestone       `json:"milestones,omitempty"`
	Hypotheses       *[]ProjectHypothesis      `json:"hypotheses,omitempty"`
	SourceMonitors   *[]SourceMonitorReference `json:"sourceMonitors,omitempty"`
	Deliverables     *[]ProjectDeliverable     `json:"deliverables,omitempty"`
	Budget           *BudgetPolicy             `json:"budget,omitempty"`
	Policy           map[string]interface{}    `json:"policy,omitempty"`
}

func (a projectUpdateArguments) hasChanges() bool {
	return a.Title != nil || a.Purpose != nil || a.AgentRefs != nil || a.TeamRefs != nil || a.ObjectiveRefs != nil || a.RunRefs != nil || a.Milestones != nil || a.Hypotheses != nil || a.SourceMonitors != nil || a.Deliverables != nil || a.Budget != nil || a.Policy != nil
}

func (a projectUpdateArguments) updateRequest(actor ActivityActor) UpdateProjectRequest {
	return UpdateProjectRequest{ExpectedRevision: a.ExpectedRevision, Title: a.Title, Purpose: a.Purpose, AgentRefs: a.AgentRefs, TeamRefs: a.TeamRefs, ObjectiveRefs: a.ObjectiveRefs, RunRefs: a.RunRefs, Milestones: a.Milestones, Hypotheses: a.Hypotheses, SourceMonitors: a.SourceMonitors, Deliverables: a.Deliverables, Budget: a.Budget, Policy: a.Policy, Actor: actor, Visibility: ActivityVisibilityScope}
}

func applyProjectArguments(candidate *Project, args projectUpdateArguments) *Project {
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

func decodeProjectCreateArguments(arguments map[string]interface{}) (projectCreateArguments, error) {
	var args projectCreateArguments
	return args, decodeProjectArguments(arguments, &args)
}

func decodeProjectUpdateArguments(arguments map[string]interface{}) (projectUpdateArguments, error) {
	var args projectUpdateArguments
	if err := decodeProjectArguments(arguments, &args); err != nil {
		return args, err
	}
	if strings.TrimSpace(args.ProjectID) == "" || args.ExpectedRevision < 1 {
		return args, errors.New("projectId and expectedRevision are required")
	}
	return args, nil
}

func decodeProjectArguments(arguments map[string]interface{}, target interface{}) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid project action: %w", err)
	}
	return nil
}

func validateProjectCandidate(ctx context.Context, service *ProjectService, candidate *Project) error {
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidProject, err)
	}
	if err := service.validateObjectives(ctx, candidate); err != nil {
		return err
	}
	return service.validateSourceMonitors(ctx, candidate)
}

func projectActionAlreadyApplied(i *Project, args projectUpdateArguments, action string) bool {
	if i == nil || i.Revision != args.ExpectedRevision+1 {
		return false
	}
	if action == ProjectActionPause {
		return i.Status == ProjectStatusPaused
	}
	return (args.Title == nil || i.Title == *args.Title) && (args.Purpose == nil || i.Purpose == *args.Purpose) &&
		(args.AgentRefs == nil || reflect.DeepEqual(i.AgentRefs, *args.AgentRefs)) && (args.TeamRefs == nil || reflect.DeepEqual(i.TeamRefs, *args.TeamRefs)) &&
		(args.ObjectiveRefs == nil || reflect.DeepEqual(i.ObjectiveRefs, *args.ObjectiveRefs)) && (args.RunRefs == nil || reflect.DeepEqual(i.RunRefs, *args.RunRefs)) &&
		(args.Milestones == nil || reflect.DeepEqual(i.Milestones, *args.Milestones)) && (args.Hypotheses == nil || reflect.DeepEqual(i.Hypotheses, *args.Hypotheses)) &&
		(args.SourceMonitors == nil || reflect.DeepEqual(i.SourceMonitors, *args.SourceMonitors)) && (args.Deliverables == nil || reflect.DeepEqual(i.Deliverables, *args.Deliverables)) &&
		(args.Budget == nil || reflect.DeepEqual(i.Budget, args.Budget)) && (args.Policy == nil || reflect.DeepEqual(i.Policy, args.Policy))
}

func isProjectAction(bound *skill.BoundAction) bool {
	if bound == nil || bound.Definition == nil || bound.Definition.ID != ProjectManagementSkillID || bound.Definition.Version != ProjectManagementSkillVersion {
		return false
	}
	switch bound.Action.Name {
	case ProjectActionCreate, ProjectActionUpdate, ProjectActionPause:
		return true
	}
	return false
}
