package agentbuilder

import (
	"context"
	"errors"
	"fmt"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
)

type agentChannelActionArguments struct {
	ExpectedDeploymentRevision int64     `json:"expectedDeploymentRevision"`
	ExpectedEndpointRevision   int64     `json:"expectedEndpointRevision"`
	EndpointID                 string    `json:"endpointId"`
	MessageSelection           *string   `json:"messageSelection,omitempty"`
	ReplyMode                  *string   `json:"replyMode,omitempty"`
	IgnoreBots                 *bool     `json:"ignoreBots,omitempty"`
	Purposes                   *[]string `json:"purposes,omitempty"`
	Status                     *string   `json:"status,omitempty"`
	Rationale                  string    `json:"rationale"`
}

func (d *AgentBehaviorActionDispatcher) listChannels(ctx context.Context, scope models.Scope, deploymentID string) (map[string]interface{}, error) {
	if d.endpoints == nil {
		return nil, errors.New("agent channel management is not configured")
	}
	deployment, err := d.agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID)
	if err != nil {
		return nil, err
	}
	definition, err := d.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}
	endpoints, err := d.endpoints.List(ctx, models.ExternalConversationEndpointFilter{Scope: scope, Owner: &owner, Limit: 500})
	if err != nil {
		return nil, err
	}
	channels := make([]map[string]interface{}, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint == nil {
			continue
		}
		channel := map[string]interface{}{
			"endpointId": endpoint.ID, "name": endpoint.Name, "provider": endpoint.Provider,
			"mode": endpoint.Mode, "address": endpoint.Address, "status": endpoint.Status,
			"revision": endpoint.Revision, "ingressRoute": endpoint.IngressRoute,
			"adapter": endpoint.Adapter, "handler": endpoint.Handler, "policy": endpoint.Policy,
			"configurable": endpoint.Status != models.ExternalConversationEndpointRetired,
		}
		if endpoint.Status == models.ExternalConversationEndpointRetired {
			channel["configurationBlocker"] = "This channel is retired and cannot be configured or reactivated. Use an active or paused channel."
		}
		if route := findAgentChannelRoute(definition, endpoint.ID); route != nil {
			channel["workflow"] = route
		}
		channels = append(channels, channel)
	}
	return map[string]interface{}{"resourceType": "agent_channels", "channels": channels}, nil
}

func (d *AgentBehaviorActionDispatcher) configureChannel(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun) (map[string]interface{}, error) {
	args, deployment, definition, endpoint, candidate, changes, err := resolveAgentChannelAction(ctx, d.agents, d.endpoints, run, input.Arguments)
	if errors.Is(err, kernelagent.ErrRevisionConflict) {
		return d.replayedChannelResult(ctx, input.Call, run)
	}
	if err != nil {
		return nil, err
	}
	candidateVersion := agentBehaviorActionCandidateVersion(definition.Version, input.Call.ID)
	if deployment.ActiveVersion == candidateVersion {
		return d.replayedChannelResult(ctx, input.Call, run)
	}
	candidate.Version = candidateVersion
	updateRequest, err := agentChannelEndpointUpdate(endpoint, candidate, args)
	if err != nil {
		return nil, err
	}
	preparedEndpoint, err := d.endpoints.PrepareUpdate(ctx, endpoint.Scope, endpoint.ID, updateRequest)
	if err != nil {
		return nil, err
	}
	definitionChanges := map[string]interface{}{}
	for key, value := range changes {
		if key == "channels" || key == "authority.approvalDestinations" {
			definitionChanges[key] = value
		}
	}
	var amendment *kernelagent.DefinitionAmendment
	var activation *kernelagent.DefinitionActivation
	if len(definitionChanges) > 0 {
		additionalAllowedFields := make([]string, 0, len(definitionChanges))
		for field := range definitionChanges {
			definitionField := agentDefinitionAmendmentField(field)
			if !hasExactAgentActionString(definition.Amendments.AllowedFields, definitionField) && !hasExactAgentActionString(additionalAllowedFields, definitionField) {
				additionalAllowedFields = append(additionalAllowedFields, definitionField)
			}
		}
		if len(additionalAllowedFields) > 0 {
			if _, approvalErr := approvedAgentBehaviorCheckpoint(ctx, d.store, input.Call); approvalErr != nil {
				return nil, approvalErr
			}
		}
		amendment, err = d.agents.ProposeAmendment(ctx, kernelagent.ProposeAmendmentRequest{
			Scope: capability.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}, DeploymentID: deployment.ID,
			Candidate: candidate, ProposerType: "agent", ProposerID: deployment.ID,
			Rationale: strings.TrimSpace(args.Rationale), EvidenceRefs: append([]string(nil), input.Call.EvidenceRefs...),
			IdempotencyKey: input.Call.ID, ExpectedDeploymentRevision: deployment.Revision,
			AdditionalAllowedFields: additionalAllowedFields,
		})
		if err != nil {
			return nil, fmt.Errorf("propose Agent channel amendment with allowed fields %v: %w", additionalAllowedFields, err)
		}
		actorType, actorID := "agent", deployment.ID
		if input.Call.ApprovalID != "" {
			approval, approvalErr := approvedAgentBehaviorCheckpoint(ctx, d.store, input.Call)
			if approvalErr != nil {
				return nil, approvalErr
			}
			actorType, actorID = approval.DecisionBy.Type, approval.DecisionBy.ID
		}
		if amendment.Status == kernelagent.AmendmentAwaitingApproval {
			approval, approvalErr := approvedAgentBehaviorCheckpoint(ctx, d.store, input.Call)
			if approvalErr != nil {
				return nil, approvalErr
			}
			amendment, err = d.agents.ResolveAmendmentFromGovernedApproval(ctx, kernelagent.ResolveAmendmentRequest{
				Scope: capability.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}, AmendmentID: amendment.ID,
				ExpectedRevision: amendment.Revision, Approved: true, ActorType: approval.DecisionBy.Type,
				ActorID: approval.DecisionBy.ID, Reason: approval.DecisionReason,
			})
			if err != nil {
				return nil, err
			}
		}
		if amendment.Status != kernelagent.AmendmentReady && amendment.Status != kernelagent.AmendmentApproved {
			return nil, fmt.Errorf("agent channel amendment is not ready after governed action approval: %s", amendment.Status)
		}
		amendment, deployment, activation, err = d.agents.ActivateAmendmentWithCommit(
			ctx, capability.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}, amendment.ID, amendment.Revision,
			actorType, actorID, "Approved Agent channel workflow change from conversation",
			d.endpoints.ChannelActivationCommit(preparedEndpoint, endpoint.Revision, args.ExpectedDeploymentRevision),
		)
		if err != nil {
			return nil, err
		}
		return agentChannelActionResult(amendment, deployment, activation, preparedEndpoint, false), nil
	}
	updated, err := d.convergeChannelEndpoint(ctx, endpoint, candidate, args)
	if err != nil {
		return nil, err
	}
	return agentChannelActionResult(amendment, deployment, activation, updated, false), nil
}

func agentDefinitionAmendmentField(change string) string {
	if strings.HasPrefix(change, "authority.") {
		return "authority"
	}
	return change
}

func (d *AgentBehaviorActionDispatcher) convergeChannelEndpoint(ctx context.Context, endpoint *models.ExternalConversationEndpoint, definition *kernelagent.AgentDefinition, args agentChannelActionArguments) (*models.ExternalConversationEndpoint, error) {
	request, err := agentChannelEndpointUpdate(endpoint, definition, args)
	if err != nil {
		return nil, err
	}
	if request.Handler == nil && request.Policy == nil && request.Status == nil {
		return endpoint, nil
	}
	return d.endpoints.Update(ctx, endpoint.Scope, endpoint.ID, request)
}

func agentChannelEndpointUpdate(endpoint *models.ExternalConversationEndpoint, definition *kernelagent.AgentDefinition, args agentChannelActionArguments) (externalconversation.UpdateExternalConversationEndpointRequest, error) {
	request := externalconversation.UpdateExternalConversationEndpointRequest{ExpectedRevision: endpoint.Revision}
	route := findAgentChannelRoute(definition, endpoint.ID)
	if route == nil {
		return request, errors.New("Agent definition does not contain the channel route")
	}
	handler := models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: endpoint.DeploymentID}
	policy := models.ExternalConversationPolicy{
		MessageSelection: models.ExternalConversationMessageSelection(route.MessageSelection),
		ReplyMode:        models.ExternalConversationReplyMode(route.ReplyMode), IgnoreBots: route.IgnoreBots,
	}
	status := endpoint.Status
	if args.Status != nil {
		status = models.ExternalConversationEndpointStatus(strings.TrimSpace(*args.Status))
	}
	if endpoint.Handler != handler {
		request.Handler = &handler
	}
	if endpoint.Policy != policy {
		request.Policy = &policy
	}
	if endpoint.Status != status {
		request.Status = &status
	}
	return request, nil
}

func resolveAgentChannelAction(
	ctx context.Context,
	agents *kernelagent.Registry,
	endpoints *externalconversation.EndpointService,
	run *models.AgentRun,
	arguments map[string]interface{},
) (agentChannelActionArguments, *kernelagent.AgentDeployment, *kernelagent.AgentDefinition, *models.ExternalConversationEndpoint, *kernelagent.AgentDefinition, map[string]interface{}, error) {
	var args agentChannelActionArguments
	arguments, err := withoutRemovedChannelTrigger(arguments)
	if err != nil {
		return args, nil, nil, nil, nil, nil, err
	}
	if err := decodeAgentBehaviorArguments(arguments, &args); err != nil {
		return args, nil, nil, nil, nil, nil, err
	}
	if endpoints == nil || args.ExpectedDeploymentRevision < 1 || args.ExpectedEndpointRevision < 1 ||
		strings.TrimSpace(args.EndpointID) == "" || strings.TrimSpace(args.Rationale) == "" {
		return args, nil, nil, nil, nil, nil, errors.New("existing endpoint, expected revisions, and rationale are required")
	}
	deploymentID, err := agentBehaviorDeploymentID(run)
	if err != nil {
		return args, nil, nil, nil, nil, nil, err
	}
	scope := capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	deployment, err := agents.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return args, nil, nil, nil, nil, nil, err
	}
	if deployment.Revision != args.ExpectedDeploymentRevision {
		return args, nil, nil, nil, nil, nil, kernelagent.ErrRevisionConflict
	}
	definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return args, nil, nil, nil, nil, nil, err
	}
	if !definition.Amendments.AgentMayPropose {
		return args, nil, nil, nil, nil, nil, skillbinding.ErrAgentBehaviorFixed
	}
	endpoint, err := endpoints.Get(ctx, run.Scope, args.EndpointID)
	if err != nil {
		return args, nil, nil, nil, nil, nil, err
	}
	if endpoint.Revision != args.ExpectedEndpointRevision {
		return args, nil, nil, nil, nil, nil, models.ErrExternalConversationConflict
	}
	if endpoint.DeploymentID != deploymentID || endpoint.Owner.Type != models.OwnerTypeAgent || endpoint.Owner.ID != deploymentID {
		return args, nil, nil, nil, nil, nil, errors.New("agent channel endpoint is not owned by the current Agent")
	}
	if endpoint.Status == models.ExternalConversationEndpointRetired {
		return args, nil, nil, nil, nil, nil, fmt.Errorf("%w: retired channels cannot be configured or reactivated; use an active or paused channel", models.ErrInvalidExternalConversation)
	}
	candidate := cloneAgentDefinitionForAction(definition)
	route := findAgentChannelRoute(candidate, endpoint.ID)
	if route == nil {
		candidate.Channels = append(candidate.Channels, kernelagent.ChannelRoute{
			EndpointID: endpoint.ID, MessageSelection: string(endpoint.Policy.MessageSelection),
			ReplyMode: string(endpoint.Policy.ReplyMode), IgnoreBots: endpoint.Policy.IgnoreBots,
			Purposes: []string{"conversation"},
		})
		route = &candidate.Channels[len(candidate.Channels)-1]
	}
	if args.MessageSelection != nil {
		route.MessageSelection = strings.TrimSpace(*args.MessageSelection)
	}
	if args.ReplyMode != nil {
		route.ReplyMode = strings.TrimSpace(*args.ReplyMode)
	}
	if args.IgnoreBots != nil {
		route.IgnoreBots = *args.IgnoreBots
	}
	if args.Purposes != nil {
		route.Purposes = models.TrimmedUniqueStrings(*args.Purposes)
	}
	syncAgentApprovalDestination(candidate, endpoint.ID, hasExactAgentActionString(route.Purposes, "approvals"))
	if args.Status != nil {
		status := models.ExternalConversationEndpointStatus(strings.TrimSpace(*args.Status))
		if status != models.ExternalConversationEndpointActive && status != models.ExternalConversationEndpointPaused {
			return args, nil, nil, nil, nil, nil, errors.New("agent channel status must be active or paused")
		}
	}
	if err := candidate.Validate(); err != nil {
		return args, nil, nil, nil, nil, nil, err
	}
	updateRequest, err := agentChannelEndpointUpdate(endpoint, candidate, args)
	if err != nil {
		return args, nil, nil, nil, nil, nil, err
	}
	if _, err := endpoints.PrepareUpdate(ctx, run.Scope, endpoint.ID, updateRequest); err != nil {
		return args, nil, nil, nil, nil, nil, err
	}
	changes := map[string]interface{}{}
	if !equalJSON(definition.Channels, candidate.Channels) {
		changes["channels"] = append([]kernelagent.ChannelRoute(nil), candidate.Channels...)
	}
	if !equalJSON(definition.Authority.ApprovalDestinations, candidate.Authority.ApprovalDestinations) {
		changes["authority.approvalDestinations"] = append([]kernelagent.ApprovalDestination(nil), candidate.Authority.ApprovalDestinations...)
	}
	if args.Status != nil && endpoint.Status != models.ExternalConversationEndpointStatus(strings.TrimSpace(*args.Status)) {
		changes["channelStatus"] = strings.TrimSpace(*args.Status)
	}
	if len(changes) == 0 {
		return args, nil, nil, nil, nil, nil, errors.New("agent channel amendment requires at least one changed field")
	}
	return args, deployment, definition, endpoint, candidate, changes, nil
}

// ErrChannelTriggerRemoved refuses a configure_channel call from a 1.5.0
// binding that still sets a channel trigger.
var ErrChannelTriggerRemoved = errors.New("Channel triggers were removed")

// withoutRemovedChannelTrigger drops the trigger argument that 1.5.0 bindings
// may still send. An empty value means ordinary Agent conversation, which is
// the only behavior left; any other value is refused.
func withoutRemovedChannelTrigger(arguments map[string]interface{}) (map[string]interface{}, error) {
	value, ok := arguments["trigger"]
	if !ok {
		return arguments, nil
	}
	if trigger, isString := value.(string); !isString || strings.TrimSpace(trigger) != "" {
		return nil, ErrChannelTriggerRemoved
	}
	kept := make(map[string]interface{}, len(arguments)-1)
	for key, item := range arguments {
		if key != "trigger" {
			kept[key] = item
		}
	}
	return kept, nil
}

func findAgentChannelRoute(definition *kernelagent.AgentDefinition, endpointID string) *kernelagent.ChannelRoute {
	if definition == nil {
		return nil
	}
	for index := range definition.Channels {
		if definition.Channels[index].EndpointID == endpointID {
			return &definition.Channels[index]
		}
	}
	return nil
}

func syncAgentApprovalDestination(definition *kernelagent.AgentDefinition, endpointID string, enabled bool) {
	destinations := make([]kernelagent.ApprovalDestination, 0, len(definition.Authority.ApprovalDestinations)+1)
	found := false
	for _, destination := range definition.Authority.ApprovalDestinations {
		if destination.EndpointID == endpointID {
			found = true
			if !enabled {
				continue
			}
		}
		destinations = append(destinations, destination)
	}
	if enabled && !found {
		destinations = append(destinations, kernelagent.ApprovalDestination{EndpointID: endpointID})
	}
	definition.Authority.ApprovalDestinations = destinations
}

func agentChannelCurrentValues(definition *kernelagent.AgentDefinition, endpoint *models.ExternalConversationEndpoint) map[string]interface{} {
	current := map[string]interface{}{
		"endpointId": endpoint.ID, "status": endpoint.Status, "handler": endpoint.Handler, "policy": endpoint.Policy,
	}
	if route := findAgentChannelRoute(definition, endpoint.ID); route != nil {
		current["workflow"] = route
	}
	return current
}

func (d *AgentBehaviorActionDispatcher) replayedChannelResult(ctx context.Context, call *models.ActionCall, run *models.AgentRun) (map[string]interface{}, error) {
	deploymentID, err := agentBehaviorDeploymentID(run)
	if err != nil {
		return nil, err
	}
	scope := capability.ScopeReference{Kind: call.Scope.Kind, ID: call.Scope.ID}
	deployment, err := d.agents.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	expectedVersion := agentBehaviorActionCandidateVersion("", call.ID)
	if !strings.HasSuffix(deployment.ActiveVersion, expectedVersion) {
		return nil, kernelagent.ErrRevisionConflict
	}
	amendment, err := findAgentBehaviorAmendment(ctx, d.agents, call.Scope, deploymentID, deployment.ActiveVersion)
	if err != nil || amendment == nil || amendment.Status != kernelagent.AmendmentActivated {
		if err == nil {
			err = kernelagent.ErrRevisionConflict
		}
		return nil, err
	}
	activations, err := d.agents.ListActivations(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	var activation *kernelagent.DefinitionActivation
	for index := range activations {
		if activations[index].ToVersion == deployment.ActiveVersion {
			value := activations[index]
			activation = &value
			break
		}
	}
	if activation == nil {
		return nil, kernelagent.ErrRevisionConflict
	}
	definition, err := d.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	var args agentChannelActionArguments
	if err := decodeAgentBehaviorArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	endpoint, err := d.endpoints.Get(ctx, call.Scope, args.EndpointID)
	if err != nil {
		return nil, err
	}
	if endpoint.Revision != args.ExpectedEndpointRevision+1 || endpoint.Status == models.ExternalConversationEndpointRetired || endpoint.DeploymentID != deploymentID || endpoint.Owner.Type != models.OwnerTypeAgent || endpoint.Owner.ID != deploymentID {
		return nil, models.ErrExternalConversationConflict
	}
	request, err := agentChannelEndpointUpdate(endpoint, definition, args)
	if err != nil {
		return nil, err
	}
	// A replay reports the committed result. It must never overwrite channel
	// changes made after approval or try to repair a legacy partial activation.
	if request.Handler != nil || request.Policy != nil || request.Status != nil {
		return nil, models.ErrExternalConversationConflict
	}
	return agentChannelActionResult(amendment, deployment, activation, endpoint, true), nil
}

func agentChannelActionResult(
	amendment *kernelagent.DefinitionAmendment,
	deployment *kernelagent.AgentDeployment,
	activation *kernelagent.DefinitionActivation,
	endpoint *models.ExternalConversationEndpoint,
	replayed bool,
) map[string]interface{} {
	return map[string]interface{}{
		"resourceType": models.AgentBehaviorResourceType, "operation": models.AgentActionConfigureChannel, "replayed": replayed,
		"amendment": amendment, "deployment": deployment, "activation": activation, "endpoint": endpoint,
	}
}
