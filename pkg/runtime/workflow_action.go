package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

const (
	RunbookActionCreateWorkflow   = "create_workflow"
	RunbookActionWorkflowSources  = "workflow_sources"
	RunbookActionInspectWorkflows = "inspect_workflows"
	deferredWorkflowContextKey    = "deferredWorkflow"
)

// WorkflowSourceCatalog resolves only enabled, governed adapter bindings. The
// action never accepts a caller-supplied tenant, deployment, or event source.
type WorkflowSourceCatalog interface {
	ListBindings(context.Context, skill.ScopeReference, string) ([]*skill.Binding, error)
	ResolveConversationAdapterBinding(context.Context, skill.ScopeReference, string, string, string) (*skill.BoundConversationAdapter, error)
	ResolveCallbackAdapterBinding(context.Context, skill.ScopeReference, string, string, string) (*skill.BoundCallbackAdapter, error)
}

type WorkflowTeamCatalog interface {
	GetDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error)
}

type WorkflowEventSource struct {
	BindingID             string                       `json:"bindingId"`
	AdapterID             string                       `json:"adapterId"`
	AdapterKind           string                       `json:"adapterKind"`
	SkillID               string                       `json:"skillId"`
	SourceIdentity        string                       `json:"sourceIdentity,omitempty"`
	BindingRevision       int64                        `json:"bindingRevision"`
	Name                  string                       `json:"name"`
	Provider              string                       `json:"provider"`
	EventTypes            []string                     `json:"eventTypes"`
	SubjectDescription    string                       `json:"subjectDescription"`
	CorrelationAttributes []string                     `json:"correlationAttributes,omitempty"`
	Available             bool                         `json:"available"`
	UnavailableReason     string                       `json:"unavailableReason,omitempty"`
	Installations         []WorkflowSourceInstallation `json:"installations,omitempty"`
}

type WorkflowSourceInstallation struct {
	InstallationID string `json:"installationId"`
	ApplicationID  string `json:"applicationId,omitempty"`
}

type workflowInstalledSource struct {
	installation    WorkflowSourceInstallation
	eventTypes      []string
	provider        string
	skillID         string
	sourceIdentity  string
	bindingRevision int64
}

func workflowSourceKey(kind, bindingID, adapterID string) string {
	return kind + "\x00" + bindingID + "\x00" + adapterID
}

// Read the scoped installation registry once per discovery request, then join
// by exact deployment/binding/adapter. This avoids one registry scan per binding.
func workflowInstalledSources(ctx context.Context, store runbookActionStore, run *AgentRun) (map[string][]workflowInstalledSource, error) {
	result := map[string][]workflowInstalledSource{}
	if gateways, ok := store.(ExternalConversationGatewayStore); ok {
		for offset := 0; ; offset += 100 {
			page, err := gateways.ListExternalConversationGateways(ctx, ExternalConversationGatewayFilter{Scope: run.Scope, DeploymentID: run.AssignedAgentID, Statuses: []ExternalConversationGatewayStatus{ExternalConversationGatewayActive}, Limit: 100, Offset: offset})
			if err != nil {
				return nil, err
			}
			for _, entry := range page {
				gateway := entry.Gateway
				if gateway.DeploymentID != run.AssignedAgentID || gateway.InstallationID == "" {
					continue
				}
				key := workflowSourceKey("conversation", gateway.Adapter.BindingID, gateway.Adapter.AdapterID)
				result[key] = append(result[key], workflowInstalledSource{installation: WorkflowSourceInstallation{InstallationID: gateway.InstallationID, ApplicationID: gateway.ApplicationID}, provider: gateway.Provider, skillID: gateway.Adapter.SkillID, sourceIdentity: gateway.Adapter.SourceIdentity, bindingRevision: gateway.Adapter.BindingRevision})
			}
			if len(page) < 100 {
				break
			}
		}
	}
	if callbacks, ok := store.(CallbackRegistrationStore); ok {
		for offset := 0; ; offset += 100 {
			page, err := callbacks.ListCallbackRegistrations(ctx, CallbackRegistrationFilter{Scope: run.Scope, DeploymentID: run.AssignedAgentID, Statuses: []CallbackRegistrationStatus{CallbackRegistrationActive}, Limit: 100, Offset: offset})
			if err != nil {
				return nil, err
			}
			for _, entry := range page {
				if entry.DeploymentID != run.AssignedAgentID || entry.Owner != run.Owner {
					continue
				}
				eventTypes := []string{}
				for _, subscription := range entry.Subscriptions {
					if !slices.Contains(eventTypes, subscription.EventType) {
						eventTypes = append(eventTypes, subscription.EventType)
					}
				}
				key := workflowSourceKey("callback", entry.Adapter.BindingID, entry.Adapter.AdapterID)
				result[key] = append(result[key], workflowInstalledSource{eventTypes: eventTypes, provider: entry.Provider, skillID: entry.Adapter.SkillID, sourceIdentity: entry.Adapter.SourceIdentity, bindingRevision: entry.Adapter.BindingRevision})
			}
			if len(page) < 100 {
				break
			}
		}
	}
	return result, nil
}

func projectWorkflowSourceInstallation(source *WorkflowEventSource, installed []workflowInstalledSource) {
	verified := make([]workflowInstalledSource, 0, len(installed))
	for _, value := range installed {
		if value.provider == source.Provider && value.skillID == source.SkillID && value.sourceIdentity == source.SourceIdentity && value.bindingRevision >= 1 && value.bindingRevision <= source.BindingRevision {
			verified = append(verified, value)
		}
	}
	installed = verified
	source.Available = len(installed) > 0
	if !source.Available {
		source.UnavailableReason = "No active authenticated ingress registration is installed for this exact Agent binding and adapter"
		return
	}
	if source.AdapterKind == "conversation" {
		for _, entry := range installed {
			if !slices.Contains(source.Installations, entry.installation) {
				source.Installations = append(source.Installations, entry.installation)
			}
		}
		return
	}
	eventTypes := []string{}
	for _, entry := range installed {
		for _, eventType := range entry.eventTypes {
			if slices.Contains(source.EventTypes, eventType) && !slices.Contains(eventTypes, eventType) {
				eventTypes = append(eventTypes, eventType)
			}
		}
	}
	slices.Sort(eventTypes)
	source.EventTypes = eventTypes
	if len(eventTypes) == 0 {
		source.Available = false
		source.UnavailableReason = "Active callback registrations do not subscribe to any declared event type"
	}
}

type workflowCreateArguments struct {
	Title       string                 `json:"title"`
	Goal        string                 `json:"goal"`
	BindingID   string                 `json:"bindingId"`
	AdapterID   string                 `json:"adapterId"`
	AdapterKind string                 `json:"adapterKind"`
	EventType   string                 `json:"eventType"`
	Subject     string                 `json:"subject"`
	Attributes  map[string]interface{} `json:"attributes,omitempty"`
	Deadline    string                 `json:"deadline"`
}

func workflowCreationDigest(arguments map[string]interface{}) (string, error) {
	var args workflowCreateArguments
	if err := decodeScheduledTaskArguments(arguments, &args); err != nil {
		return "", err
	}
	args.Title, args.Goal = strings.TrimSpace(args.Title), strings.TrimSpace(args.Goal)
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func isWorkflowActionName(name string) bool {
	return name == RunbookActionCreateWorkflow || name == RunbookActionWorkflowSources || name == RunbookActionInspectWorkflows
}

func workflowCreateAction() skill.Action {
	return skill.Action{Name: RunbookActionCreateWorkflow,
		Description: "Register a durable follow-up for an authenticated external event, then continue the current chat. Use workflow_sources to select an enabled event adapter. Supply the exact resource/conversation subject and participant/thread selectors; the current Agent executes the self-contained goal when a match arrives or the deadline expires, and reports here. Register before sending an external request when the subject is known. If delivery first supplies the subject, register immediately after its receipt; the durable inbox retains fast replies from the original human request boundary. For multiple recipients create one follow-up per recipient. This does not enable missing connector access, create polling, send the initial request, or promise services without an event source. Do not create another workflow in the goal. Existing Run tools inspect, pause, resume or cancel the returned workflowId.",
		Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"title", "goal", "bindingId", "adapterId", "adapterKind", "eventType", "subject", "deadline"}, "properties": map[string]interface{}{
			"title":       map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 240},
			"goal":        map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 16000, "description": "Self-contained work to perform on a matched event or timeout, including the destination and requested result. Event data is supplied by the runtime; credentials and new workflow creation are forbidden."},
			"bindingId":   map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256},
			"adapterId":   map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128},
			"adapterKind": map[string]interface{}{"type": "string", "enum": []interface{}{"conversation", "callback"}},
			"eventType":   map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256},
			"subject":     map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 512, "description": "Exact external conversation ID for conversation adapters, or normalized resource subject for callbacks. Wildcards are not supported."},
			"attributes":  map[string]interface{}{"type": "object", "maxProperties": 10, "additionalProperties": map[string]interface{}{"type": []interface{}{"string", "number", "boolean"}}, "description": "Exact non-secret scalar correlation selectors. For messages use externalParticipantId and/or externalThreadId. Tenant, deployment, binding and adapter authority are server-owned."},
			"deadline":    map[string]interface{}{"type": "string", "format": "date-time", "description": "Explicit RFC3339 deadline, at most 30 days after the originating request. Clarify an unspecified or ambiguous deadline."},
		}},
		OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"workflowId", "run", "replayed"}, "properties": map[string]interface{}{"workflowId": map[string]interface{}{"type": "string"}, "run": map[string]interface{}{"type": "object"}, "replayed": map[string]interface{}{"type": "boolean"}}},
	}
}

func workflowSourcesAction() skill.Action {
	return skill.Action{Name: RunbookActionWorkflowSources, Description: "List the current Agent's enabled, authenticated event adapter capabilities for durable follow-ups. Select exact bindingId, adapterId, adapterKind, eventType and subject from these capabilities; an account with only read/write actions cannot be watched until an event adapter is enabled. This lists adapter permission, not delivery health or arbitrary provider polling.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
		InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{}},
		OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"sources"}, "properties": map[string]interface{}{"sources": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}}}},
	}
}

func workflowInspectAction() skill.Action {
	return skill.Action{Name: RunbookActionInspectWorkflows, Description: "Inspect durable event-driven follow-ups owned by this Agent or Team, including their Run status, revision, wait selectors and matched result. Use workflowId for one execution or page with offset. Returned Run IDs work with existing Run pause, resume and cancel controls.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
		InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"workflowId": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128}, "offset": map[string]interface{}{"type": "integer", "minimum": 0}}},
		OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"workflows"}, "properties": map[string]interface{}{"workflows": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}}, "nextOffset": map[string]interface{}{"type": "integer"}}},
	}
}

func workflowActionOwner(ctx context.Context, teams WorkflowTeamCatalog, run *AgentRun, bound *skill.BoundAction) error {
	if run == nil || bound == nil || bound.Binding == nil || run.AssignedAgentID == "" || (bound.Binding.DeploymentID != run.AssignedAgentID && !(run.Owner.Type == OwnerTypeTeam && bound.Binding.DeploymentID == run.Owner.ID)) || bound.Binding.Scope.Kind != run.Scope.Kind || bound.Binding.Scope.ID != run.Scope.ID || (run.Owner.Type == OwnerTypeAgent && run.Owner.ID != run.AssignedAgentID) {
		return errors.New("workflow action requires its current Agent's scoped binding")
	}
	if run.Owner.Type == OwnerTypeTeam {
		if teams == nil {
			return errors.New("workflow creation requires the current authorized Team roster")
		}
		scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
		deployment, err := teams.GetDeployment(ctx, scope, run.Owner.ID)
		if err != nil {
			return err
		}
		if deployment == nil || deployment.ID != run.Owner.ID || deployment.Scope != scope || deployment.Status != kernelteam.DeploymentActive || !teamRosterContainsAgent(deployment, run.AssignedAgentID) {
			return errors.New("workflow Agent is not a member of the current active Team")
		}
	}
	return nil
}

func workflowBoundSource(ctx context.Context, catalog WorkflowSourceCatalog, run *AgentRun, bindingID, adapterID, kind string) (*WorkflowEventSource, error) {
	if catalog == nil {
		return nil, errors.New("workflow event catalog is unavailable")
	}
	scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	base := WorkflowEventSource{BindingID: bindingID, AdapterID: adapterID, AdapterKind: kind}
	switch kind {
	case "conversation":
		bound, err := catalog.ResolveConversationAdapterBinding(ctx, scope, run.AssignedAgentID, bindingID, adapterID)
		if err != nil {
			return nil, fmt.Errorf("enabled conversation event source is unavailable: %w", err)
		}
		if bound == nil || bound.Binding == nil || bound.Binding.Disabled {
			return nil, errors.New("conversation event source is unavailable")
		}
		base.Name, base.Provider, base.EventTypes = bound.Adapter.Name, bound.Adapter.Provider, slices.Clone(bound.Adapter.InboundEventTypes)
		base.SkillID, base.BindingRevision = bound.Definition.ID, bound.Binding.Revision
		base.SourceIdentity = bound.Binding.SourceIdentity
		base.SubjectDescription = "Exact external conversation ID"
		base.CorrelationAttributes = []string{"externalParticipantId", "externalThreadId", "externalMessageId", "direct", "installationId", "applicationId"}
	case "callback":
		bound, err := catalog.ResolveCallbackAdapterBinding(ctx, scope, run.AssignedAgentID, bindingID, adapterID)
		if err != nil {
			return nil, fmt.Errorf("enabled callback event source is unavailable: %w", err)
		}
		if bound == nil || bound.Binding == nil || bound.Binding.Disabled {
			return nil, errors.New("callback event source is unavailable")
		}
		base.Name, base.Provider, base.EventTypes = bound.Adapter.Name, bound.Adapter.Provider, slices.Clone(bound.Adapter.EventTypes)
		base.SkillID, base.BindingRevision = bound.Definition.ID, bound.Binding.Revision
		base.SourceIdentity = bound.Binding.SourceIdentity
		base.SubjectDescription = "Exact resource subject supplied by the authenticated callback adapter; subjectless events cannot satisfy a wait"
	default:
		return nil, errors.New("workflow adapter kind must be conversation or callback")
	}
	return &base, nil
}

func workflowSources(ctx context.Context, store runbookActionStore, catalog WorkflowSourceCatalog, run *AgentRun) ([]*WorkflowEventSource, error) {
	if catalog == nil {
		return nil, errors.New("workflow event catalog is unavailable")
	}
	bindings, err := catalog.ListBindings(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, run.AssignedAgentID)
	if err != nil {
		return nil, err
	}
	result := make([]*WorkflowEventSource, 0)
	installed, err := workflowInstalledSources(ctx, store, run)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if binding == nil || binding.Disabled {
			continue
		}
		for _, kind := range []string{"conversation", "callback"} {
			adapters := binding.EnabledConversationAdapters
			if kind == "callback" {
				adapters = binding.EnabledCallbackAdapters
			}
			for _, adapterID := range adapters {
				source, err := workflowBoundSource(ctx, catalog, run, binding.ID, adapterID, kind)
				if err != nil {
					return nil, err
				}
				projectWorkflowSourceInstallation(source, installed[workflowSourceKey(kind, binding.ID, adapterID)])
				result = append(result, source)
			}
		}
	}
	return result, nil
}

func resolveWorkflowCreate(ctx context.Context, store runbookActionStore, catalog WorkflowSourceCatalog, run *AgentRun, arguments map[string]interface{}) (*workflowCreateArguments, *ScheduledAgentTask, *RunEventWaitSpec, error) {
	var args workflowCreateArguments
	if err := decodeScheduledTaskArguments(arguments, &args); err != nil {
		return nil, nil, nil, err
	}
	args.Title, args.Goal = strings.TrimSpace(args.Title), strings.TrimSpace(args.Goal)
	if run == nil || run.Kind != RunKindConversation || run.CreatedAt.IsZero() || args.Title == "" || len(args.Title) > 240 {
		return nil, nil, nil, errors.New("workflow creation requires a bounded title and the originating human conversation")
	}
	if err := authoring.ValidateAuthoringPrompt(args.Title); err != nil {
		return nil, nil, nil, err
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	messageID, _ := run.Context[conversationRunContextTriggerID].(string)
	task := &ScheduledAgentTask{Goal: args.Goal, ConversationID: conversationID, SourceMessageID: messageID}
	if _, err := scheduledTaskConversation(ctx, store, run.Scope, run.Owner, task); err != nil {
		return nil, nil, nil, err
	}
	if !validOpaqueIdentifier(args.BindingID, 256) || !validOpaqueIdentifier(args.AdapterID, 128) {
		return nil, nil, nil, errors.New("workflow requires exact enabled binding and adapter IDs")
	}
	source, err := workflowBoundSource(ctx, catalog, run, args.BindingID, args.AdapterID, args.AdapterKind)
	if err != nil {
		return nil, nil, nil, err
	}
	installed, err := workflowInstalledSources(ctx, store, run)
	if err != nil {
		return nil, nil, nil, err
	}
	projectWorkflowSourceInstallation(source, installed[workflowSourceKey(args.AdapterKind, args.BindingID, args.AdapterID)])
	if !source.Available {
		return nil, nil, nil, errors.New(source.UnavailableReason)
	}
	if !slices.Contains(source.EventTypes, args.EventType) {
		return nil, nil, nil, errors.New("selected adapter does not declare this event type")
	}
	deadline, err := time.Parse(time.RFC3339Nano, args.Deadline)
	if err != nil {
		return nil, nil, nil, errors.New("workflow requires an explicit RFC3339 deadline")
	}
	attrs := cloneMap(args.Attributes)
	if len(attrs) > 10 {
		return nil, nil, nil, errors.New("workflow correlation permits at most ten caller selectors")
	}
	if attrs == nil {
		attrs = map[string]interface{}{}
	}
	for _, key := range []string{"scope", "tenantId", "deploymentId", "bindingId", "adapterId"} {
		if _, exists := attrs[key]; exists {
			return nil, nil, nil, errors.New("workflow authority selectors are server-owned")
		}
	}
	if args.AdapterKind == "conversation" {
		installationID, _ := attrs["installationId"].(string)
		applicationID, _ := attrs["applicationId"].(string)
		matches := []WorkflowSourceInstallation{}
		for _, entry := range source.Installations {
			if (installationID == "" || installationID == entry.InstallationID) && (applicationID == "" || applicationID == entry.ApplicationID) {
				matches = append(matches, entry)
			}
		}
		if len(matches) != 1 {
			return nil, nil, nil, errors.New("workflow must select exactly one active authenticated installation")
		}
		attrs["installationId"] = matches[0].InstallationID
		if matches[0].ApplicationID != "" {
			attrs["applicationId"] = matches[0].ApplicationID
		}
		if strings.HasPrefix(args.EventType, "conversation.message.") {
			participant, _ := attrs["externalParticipantId"].(string)
			thread, _ := attrs["externalThreadId"].(string)
			if strings.TrimSpace(participant) == "" && strings.TrimSpace(thread) == "" {
				return nil, nil, nil, errors.New("message follow-ups require an exact participant or thread selector")
			}
			if value, exists := attrs["participantIsBot"]; exists && value != false {
				return nil, nil, nil, errors.New("message follow-ups cannot match bot messages")
			}
			attrs["participantIsBot"] = false
		}
		if value, exists := attrs["externalConversationId"]; exists && value != args.Subject {
			return nil, nil, nil, errors.New("conversation selector must match the exact event subject")
		}
	}
	attrs["deploymentId"], attrs["bindingId"], attrs["adapterId"] = run.AssignedAgentID, args.BindingID, args.AdapterID
	spec := &RunEventWaitSpec{Key: "initial", Type: args.EventType, Source: RunEventBindingSource(run.AssignedAgentID, args.BindingID, args.AdapterID), Subject: args.Subject, Attributes: attrs, After: run.CreatedAt, Deadline: deadline}
	if err := spec.Validate(); err != nil {
		return nil, nil, nil, err
	}
	return &args, task, spec, nil
}

func validateWorkflowAction(ctx context.Context, store runbookActionStore, catalog WorkflowSourceCatalog, teams WorkflowTeamCatalog, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if err := workflowActionOwner(ctx, teams, input.Run, input.Bound); err != nil {
		return nil, err
	}
	if input.Bound.Action.Name != RunbookActionCreateWorkflow {
		return nil, nil
	}
	if _, ok := store.(RunEventWaitStore); !ok {
		return nil, errors.New("durable workflow event storage is unavailable")
	}
	args, task, spec, err := resolveWorkflowCreate(ctx, store, catalog, input.Run, input.Arguments)
	if err != nil {
		return nil, err
	}
	if !spec.Deadline.After(time.Now()) {
		return nil, errors.New("workflow deadline must be in the future")
	}
	return map[string]interface{}{"operation": RunbookActionCreateWorkflow, "title": args.Title, "goal": task.Goal, "conversationId": task.ConversationID, "eventWait": spec}, nil
}

func dispatchWorkflowAction(ctx context.Context, store runbookActionStore, catalog WorkflowSourceCatalog, teams WorkflowTeamCatalog, input ActionDispatchInput) (map[string]interface{}, error) {
	run, err := store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.Scope != input.Run.Scope || run.ID != input.Run.ID || input.Bound.Binding == nil || input.Call.DeploymentID != input.Bound.Binding.DeploymentID {
		return nil, errors.New("workflow execution does not match its persisted Agent run")
	}
	if err := workflowActionOwner(ctx, teams, run, input.Bound); err != nil {
		return nil, err
	}
	switch input.Bound.Action.Name {
	case RunbookActionWorkflowSources:
		var args struct{}
		if err := decodeScheduledTaskArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		sources, err := workflowSources(ctx, store, catalog, run)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"sources": sources}, nil
	case RunbookActionInspectWorkflows:
		return inspectWorkflowRuns(ctx, store, run, input.Arguments)
	case RunbookActionCreateWorkflow:
		if input.Call.ID == "" || input.Call.IdempotencyKey == "" {
			return nil, errors.New("workflow creation requires action idempotency")
		}
		if _, ok := store.(RunEventWaitStore); !ok {
			return nil, errors.New("durable workflow event storage is unavailable")
		}
		key := "conversation-workflow:" + input.Call.ID
		id := runIDForIdempotencyKey(run.Scope, key)
		requestDigest, err := workflowCreationDigest(input.Arguments)
		if err != nil {
			return nil, err
		}
		persisted, err := store.GetAgentRun(ctx, run.Scope, id)
		if err != nil {
			return nil, err
		}
		// Recover a committed creation before consulting mutable connector state.
		// Revoked ingress cannot turn an accepted action retry into a new request.
		if persisted != nil {
			if persisted.Owner != run.Owner || persisted.AssignedAgentID != run.AssignedAgentID || persisted.Context["workflowSourceRunId"] != run.ID || persisted.Context["workflowActionId"] != input.Call.ID || persisted.Context["workflowRequestDigest"] != requestDigest {
				return nil, ErrRunIdempotency
			}
			return map[string]interface{}{"workflowId": persisted.ID, "run": persisted, "replayed": true}, nil
		}
		args, task, spec, err := resolveWorkflowCreate(ctx, store, catalog, run, input.Arguments)
		if err != nil {
			return nil, err
		}
		if !spec.Deadline.After(time.Now()) {
			return nil, errors.New("workflow deadline must be in the future")
		}
		values := map[string]interface{}{conversationRunContextConversationID: task.ConversationID, conversationRunContextTriggerID: task.SourceMessageID, runReportingContextRootRunID: id, runReportingContextMilestones: []interface{}{string(runbook.ReportingCompleted), string(runbook.ReportingFailed)}, scheduledTaskContextKey: true, deferredWorkflowContextKey: true, "workflowTitle": args.Title, "workflowSourceRunId": run.ID, "workflowActionId": input.Call.ID, "workflowRequestDigest": requestDigest}
		req := CreateAgentRunRequest{Scope: run.Scope, Kind: RunKindAgentWork, Owner: run.Owner, AssignedAgentID: run.AssignedAgentID, ConcurrencyKey: "workflow:" + id, Goal: task.Goal, Source: RunSourceRequest, Context: values, WakeCondition: &WakeCondition{Type: "event", Reference: spec.Key, EventWait: spec}, Budget: cloneBudgetPolicy(run.Budget), IdempotencyKey: key, Actor: ActivityActor{Type: "agent", ID: run.AssignedAgentID}, Visibility: ActivityVisibilityScope}
		commands := NewRunCommandService(store)
		existing, err := commands.FindCreatedAgentRun(ctx, req)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return map[string]interface{}{"workflowId": existing.Run.ID, "run": existing.Run, "replayed": true}, nil
		}
		created, err := commands.CreateAgentRun(ctx, req)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"workflowId": created.Run.ID, "run": created.Run, "replayed": created.Event == nil}, nil
	}
	return nil, errors.New("unsupported workflow action")
}

func inspectWorkflowRuns(ctx context.Context, store runbookActionStore, run *AgentRun, arguments map[string]interface{}) (map[string]interface{}, error) {
	var args struct {
		WorkflowID string `json:"workflowId"`
		Offset     int    `json:"offset"`
	}
	if err := decodeScheduledTaskArguments(arguments, &args); err != nil {
		return nil, err
	}
	if args.Offset < 0 {
		return nil, errors.New("invalid workflow offset")
	}
	result := make([]*AgentRun, 0)
	if args.WorkflowID != "" {
		if args.Offset != 0 {
			return nil, errors.New("workflowId cannot be combined with a paging offset")
		}
		current, err := store.GetAgentRun(ctx, run.Scope, args.WorkflowID)
		if err != nil {
			return nil, err
		}
		if current == nil || current.Owner != run.Owner || current.AssignedAgentID != run.AssignedAgentID || current.Context[deferredWorkflowContextKey] != true {
			return nil, errors.New("workflow is unavailable to this Agent")
		}
		return map[string]interface{}{"workflows": []*AgentRun{current}}, nil
	}
	values, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: run.Scope, Owner: &run.Owner, AssignedAgentID: run.AssignedAgentID, Kind: RunKindAgentWork, Limit: 50, Offset: args.Offset})
	if err != nil {
		return nil, err
	}
	for _, current := range values {
		if current.Context[deferredWorkflowContextKey] == true {
			result = append(result, current)
		}
	}
	output := map[string]interface{}{"workflows": result}
	if len(values) == 50 {
		output["nextOffset"] = args.Offset + 50
	}
	return output, nil
}
