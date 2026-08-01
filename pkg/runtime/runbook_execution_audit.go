package runtime

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

// RunbookExecutionAuditStore is the portable read boundary required to
// reconstruct one Runbook occurrence. Implementations already persist these
// canonical facts; the audit service creates no parallel execution state.
type RunbookExecutionAuditStore interface {
	GetAgentRun(context.Context, Scope, string) (*AgentRun, error)
	ListAgentRuns(context.Context, AgentRunFilter) ([]*AgentRun, error)
	GetRunbookActivation(context.Context, Scope, string) (*RunbookActivation, error)
	ListAgentTurns(context.Context, AgentTurnFilter) ([]*AgentTurn, error)
	ListActionCalls(context.Context, ActionFilter) ([]*ActionCall, error)
	ListApprovals(context.Context, ApprovalFilter) ([]*ApprovalCheckpoint, error)
	ListArtifacts(context.Context, ArtifactFilter) ([]*Artifact, error)
	ListAgentRequests(context.Context, AgentRequestFilter) ([]*AgentRequest, error)
}

type RunbookExecutionAuditRun struct {
	ID               string         `json:"id"`
	ParentRunID      string         `json:"parentRunId,omitempty"`
	RootRunID        string         `json:"rootRunId,omitempty"`
	ObjectiveID      string         `json:"objectiveId,omitempty"`
	AssignedAgentID  string         `json:"assignedAgentId,omitempty"`
	ActivationID     string         `json:"activationId"`
	Entrypoint       string         `json:"entrypoint,omitempty"`
	Goal             string         `json:"goal"`
	Source           RunSource      `json:"source"`
	Status           AgentRunStatus `json:"status"`
	Attempt          int            `json:"attempt"`
	Error            string         `json:"error,omitempty"`
	CreatedAt        time.Time      `json:"createdAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
	StartedAt        *time.Time     `json:"startedAt,omitempty"`
	CompletedAt      *time.Time     `json:"completedAt,omitempty"`
	WakeCondition    *WakeCondition `json:"wakeCondition,omitempty"`
	LastAppliedTurn  int64          `json:"lastAppliedTurn"`
	BudgetUsage      BudgetUsage    `json:"budgetUsage,omitempty"`
	BudgetState      BudgetState    `json:"budgetState,omitempty"`
	PendingApprovals int            `json:"pendingApprovals,omitempty"`
	PendingChildRuns int            `json:"pendingChildRuns,omitempty"`
}

type RunbookTurnAudit struct {
	ID            string          `json:"id"`
	Sequence      int64           `json:"sequence"`
	Status        AgentTurnStatus `json:"status"`
	OutputSummary string          `json:"outputSummary,omitempty"`
	Error         string          `json:"error,omitempty"`
	Usage         TurnUsage       `json:"usage,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	StartedAt     time.Time       `json:"startedAt"`
	CompletedAt   *time.Time      `json:"completedAt,omitempty"`
}

type RunbookActionAudit struct {
	ID           string                 `json:"id"`
	TurnID       string                 `json:"turnId,omitempty"`
	SkillID      string                 `json:"skillId"`
	SkillVersion string                 `json:"skillVersion"`
	Action       string                 `json:"action"`
	Status       ActionCallStatus       `json:"status"`
	Risk         string                 `json:"risk,omitempty"`
	SideEffect   string                 `json:"sideEffect,omitempty"`
	ApprovalID   string                 `json:"approvalId,omitempty"`
	Attempt      int                    `json:"attempt"`
	MaxAttempts  int                    `json:"maxAttempts"`
	EvidenceRefs []string               `json:"evidenceRefs,omitempty"`
	Arguments    map[string]interface{} `json:"arguments,omitempty"`
	Result       map[string]interface{} `json:"result,omitempty"`
	Error        string                 `json:"error,omitempty"`
	CreatedAt    time.Time              `json:"createdAt"`
	UpdatedAt    time.Time              `json:"updatedAt"`
	StartedAt    *time.Time             `json:"startedAt,omitempty"`
	CompletedAt  *time.Time             `json:"completedAt,omitempty"`
}

type RunbookApprovalAudit struct {
	ID                string                 `json:"id"`
	ActionCallID      string                 `json:"actionCallId"`
	Status            ApprovalStatus         `json:"status"`
	Summary           string                 `json:"summary"`
	PolicyReason      string                 `json:"policyReason,omitempty"`
	ProposedAction    map[string]interface{} `json:"proposedAction,omitempty"`
	EligibleApprovers []ApprovalPrincipal    `json:"eligibleApprovers,omitempty"`
	DecisionBy        *ApprovalPrincipal     `json:"decisionBy,omitempty"`
	DecisionReason    string                 `json:"decisionReason,omitempty"`
	ExpiresAt         time.Time              `json:"expiresAt"`
	CreatedAt         time.Time              `json:"createdAt"`
	UpdatedAt         time.Time              `json:"updatedAt"`
	DecidedAt         *time.Time             `json:"decidedAt,omitempty"`
	Deliveries        []RunbookDeliveryAudit `json:"deliveries,omitempty"`
}

// RunbookDeliveryAudit projects the provider-neutral request/update delivery
// facts associated with an approval. It contains routing and outcome metadata,
// never provider credentials or message content duplicated from the canonical
// conversation.
type RunbookDeliveryAudit struct {
	ID                string                                   `json:"id"`
	Phase             string                                   `json:"phase"`
	EndpointID        string                                   `json:"endpointId"`
	EndpointName      string                                   `json:"endpointName,omitempty"`
	Provider          string                                   `json:"provider,omitempty"`
	Address           string                                   `json:"address,omitempty"`
	IngressRoute      string                                   `json:"ingressRoute,omitempty"`
	Adapter           ExternalConversationAdapterReference     `json:"adapter"`
	Operation         capability.ConversationDeliveryOperation `json:"operation"`
	Status            ExternalConversationDeliveryStatus       `json:"status"`
	Attempt           int                                      `json:"attempt"`
	MaximumAttempts   int                                      `json:"maximumAttempts"`
	ProviderMessageID string                                   `json:"providerMessageId,omitempty"`
	ErrorCode         string                                   `json:"errorCode,omitempty"`
	Summary           string                                   `json:"summary,omitempty"`
	CreatedAt         time.Time                                `json:"createdAt"`
	UpdatedAt         time.Time                                `json:"updatedAt"`
	DeliveredAt       time.Time                                `json:"deliveredAt,omitempty"`
}

type RunbookArtifactAudit struct {
	ID             string                      `json:"id"`
	Version        int64                       `json:"version"`
	Name           string                      `json:"name"`
	Type           string                      `json:"type,omitempty"`
	MediaType      string                      `json:"mediaType,omitempty"`
	Classification ArtifactClassification      `json:"classification"`
	Availability   ArtifactContentAvailability `json:"availability,omitempty"`
	Digest         string                      `json:"digest"`
	SizeBytes      int64                       `json:"sizeBytes"`
	TurnID         string                      `json:"turnId,omitempty"`
	ActionID       string                      `json:"actionId,omitempty"`
	CreatedAt      time.Time                   `json:"createdAt"`
}

type RunbookChildRunAudit struct {
	ID              string         `json:"id"`
	StepID          string         `json:"stepId,omitempty"`
	AssignedAgentID string         `json:"assignedAgentId,omitempty"`
	Goal            string         `json:"goal"`
	Source          RunSource      `json:"source"`
	Status          AgentRunStatus `json:"status"`
	Error           string         `json:"error,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	StartedAt       *time.Time     `json:"startedAt,omitempty"`
	CompletedAt     *time.Time     `json:"completedAt,omitempty"`
}

type RunbookNodeExecutionAudit struct {
	StepID     string                       `json:"stepId"`
	StepKind   runbook.StepKind             `json:"stepKind"`
	StepName   string                       `json:"stepName,omitempty"`
	Status     RunbookStepTraceStatus       `json:"status,omitempty"`
	VisitCount int                          `json:"visitCount"`
	Visits     []RunbookVisitExecutionAudit `json:"visits,omitempty"`
}

// RunbookVisitExecutionAudit is the atomic operator-facing unit. Records are
// attached to an exact node visit so retries and loops never blur together.
type RunbookVisitExecutionAudit struct {
	Trace     RunbookStepTrace       `json:"trace"`
	Turns     []RunbookTurnAudit     `json:"turns,omitempty"`
	Actions   []RunbookActionAudit   `json:"actions,omitempty"`
	Approvals []RunbookApprovalAudit `json:"approvals,omitempty"`
	Artifacts []RunbookArtifactAudit `json:"artifacts,omitempty"`
	ChildRuns []RunbookChildRunAudit `json:"childRuns,omitempty"`
}

type RunbookDataLineage struct {
	Origin       string                  `json:"origin"`
	FromStepID   string                  `json:"fromStepId,omitempty"`
	FromSequence int64                   `json:"fromSequence,omitempty"`
	ToStepID     string                  `json:"toStepId"`
	ToSequence   int64                   `json:"toSequence"`
	Ref          string                  `json:"ref"`
	Availability RunbookDataAvailability `json:"availability"`
}

type RunbookExecutionAudit struct {
	Run                 RunbookExecutionAuditRun    `json:"run"`
	Runbook             *RunbookDetail              `json:"runbook"`
	Trace               *RunbookExecutionTrace      `json:"trace"`
	Nodes               []RunbookNodeExecutionAudit `json:"nodes"`
	Lineage             []RunbookDataLineage        `json:"lineage,omitempty"`
	UnassignedTurns     []RunbookTurnAudit          `json:"unassignedTurns,omitempty"`
	UnassignedActions   []RunbookActionAudit        `json:"unassignedActions,omitempty"`
	UnassignedApprovals []RunbookApprovalAudit      `json:"unassignedApprovals,omitempty"`
	UnassignedArtifacts []RunbookArtifactAudit      `json:"unassignedArtifacts,omitempty"`
	UnassignedChildren  []RunbookChildRunAudit      `json:"unassignedChildren,omitempty"`
}

type RunbookExecutionAuditService struct {
	store   RunbookExecutionAuditStore
	catalog RunbookDefinitionCatalog
}

func NewRunbookExecutionAuditService(store RunbookExecutionAuditStore, catalog RunbookDefinitionCatalog) *RunbookExecutionAuditService {
	return &RunbookExecutionAuditService{store: store, catalog: catalog}
}

func (s *RunbookExecutionAuditService) Get(ctx context.Context, scope Scope, runID string) (*RunbookExecutionAudit, error) {
	if s == nil || s.store == nil || s.catalog == nil {
		return nil, errors.New("Runbook execution audit is not configured")
	}
	run, err := s.store.GetAgentRun(ctx, scope, strings.TrimSpace(runID))
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	activationID, _ := run.Context["runbookActivationId"].(string)
	activationID = strings.TrimSpace(activationID)
	if activationID == "" {
		return nil, errors.New("Agent Run is not linked to a Runbook activation")
	}
	detail, err := ResolveRunbookDetail(ctx, s.store, s.catalog, scope, activationID)
	if err != nil {
		return nil, err
	}
	trace, err := RunbookTraceFromCheckpoint(run.Checkpoint)
	if err != nil {
		return nil, err
	}
	turns, err := s.listTurns(ctx, scope, run.ID)
	if err != nil {
		return nil, err
	}
	actions, err := s.listActions(ctx, scope, run.ID)
	if err != nil {
		return nil, err
	}
	approvals, err := s.listApprovals(ctx, scope, run.ID)
	if err != nil {
		return nil, err
	}
	artifacts, err := s.listArtifacts(ctx, scope, run.ID)
	if err != nil {
		return nil, err
	}
	children, err := s.listChildren(ctx, scope, run.ID)
	if err != nil {
		return nil, err
	}
	requests, err := s.listRequests(ctx, scope, run.ID)
	if err != nil {
		return nil, err
	}
	approvalDeliveries, err := s.listApprovalDeliveries(ctx, scope, approvals)
	if err != nil {
		return nil, err
	}

	result := &RunbookExecutionAudit{
		Run: projectRunbookAuditRun(run, activationID), Runbook: detail, Trace: trace,
		Nodes: buildRunbookNodeAudits(detail.Definition, trace), Lineage: buildRunbookLineage(trace),
	}
	attachRunbookTurns(result, turns)
	attachRunbookActions(result, actions, approvals)
	attachRunbookApprovalDeliveries(result, approvalDeliveries)
	attachRunbookArtifacts(result, artifacts)
	attachRunbookChildren(result, children, requests, turns, run)
	for _, approval := range approvals {
		if approval.Status == ApprovalStatusPending {
			result.Run.PendingApprovals++
		}
	}
	for _, child := range children {
		if child != nil && !isTerminalAgentRunStatus(child.Status) {
			result.Run.PendingChildRuns++
		}
	}
	return result, nil
}

type runbookDeliveryAuditStore interface {
	ListExternalConversationDeliveries(context.Context, ExternalConversationDeliveryFilter) ([]*ExternalConversationDelivery, error)
	GetExternalConversationEndpoint(context.Context, Scope, string) (*ExternalConversationEndpoint, error)
}

func (s *RunbookExecutionAuditService) listApprovalDeliveries(ctx context.Context, scope Scope, approvals []*ApprovalCheckpoint) (map[string][]RunbookDeliveryAudit, error) {
	store, ok := s.store.(runbookDeliveryAuditStore)
	if !ok || len(approvals) == 0 {
		return nil, nil
	}
	endpoints := map[string]*ExternalConversationEndpoint{}
	result := map[string][]RunbookDeliveryAudit{}
	for _, approval := range approvals {
		if approval == nil {
			continue
		}
		deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
			Scope: scope, CorrelationKind: "approval", CorrelationID: approval.ID, Limit: 1000,
		})
		if err != nil {
			return nil, err
		}
		for _, delivery := range deliveries {
			if delivery == nil || delivery.Correlation == nil {
				continue
			}
			endpoint, exists := endpoints[delivery.EndpointID]
			if !exists {
				endpoint, _ = store.GetExternalConversationEndpoint(ctx, scope, delivery.EndpointID)
				endpoints[delivery.EndpointID] = endpoint
			}
			audit := RunbookDeliveryAudit{
				ID: delivery.ID, Phase: delivery.Correlation.Phase, EndpointID: delivery.EndpointID, Adapter: delivery.Adapter, Operation: delivery.Operation,
				Status: delivery.Status, Attempt: delivery.Attempt, MaximumAttempts: delivery.MaximumAttempts,
				ProviderMessageID: delivery.ProviderMessageID, ErrorCode: delivery.ErrorCode, Summary: delivery.Summary,
				CreatedAt: delivery.CreatedAt, UpdatedAt: delivery.UpdatedAt, DeliveredAt: delivery.DeliveredAt,
			}
			if endpoint != nil {
				audit.EndpointName, audit.Provider, audit.Address, audit.IngressRoute = endpoint.Name, endpoint.Provider, endpoint.Address, endpoint.IngressRoute
			}
			result[approval.ID] = append(result[approval.ID], audit)
		}
	}
	return result, nil
}

func projectRunbookAuditRun(run *AgentRun, activationID string) RunbookExecutionAuditRun {
	return RunbookExecutionAuditRun{
		ID: run.ID, ParentRunID: run.ParentRunID, RootRunID: run.RootRunID, ObjectiveID: run.ObjectiveID,
		AssignedAgentID: run.AssignedAgentID, ActivationID: activationID, Entrypoint: run.Entrypoint,
		Goal: run.Goal, Source: run.Source, Status: run.Status, Attempt: run.Attempt, Error: run.Error,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
		WakeCondition: cloneWakeCondition(run.WakeCondition), LastAppliedTurn: run.LastAppliedTurn,
		BudgetUsage: run.BudgetUsage, BudgetState: run.BudgetState,
	}
}

func buildRunbookNodeAudits(definition *runbook.Definition, trace *RunbookExecutionTrace) []RunbookNodeExecutionAudit {
	ids := make([]string, 0, len(definition.Steps))
	for id := range definition.Steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]RunbookNodeExecutionAudit, 0, len(ids))
	byID := map[string]int{}
	for _, id := range ids {
		step := definition.Steps[id]
		byID[id] = len(result)
		result = append(result, RunbookNodeExecutionAudit{StepID: id, StepKind: step.Kind, StepName: strings.TrimSpace(step.Name)})
	}
	for _, visit := range trace.Entries {
		index, ok := byID[visit.StepID]
		if !ok {
			continue
		}
		result[index].Visits = append(result[index].Visits, RunbookVisitExecutionAudit{Trace: visit})
		result[index].VisitCount++
		result[index].Status = visit.Status
	}
	return result
}

func buildRunbookLineage(trace *RunbookExecutionTrace) []RunbookDataLineage {
	if trace == nil {
		return nil
	}
	result := make([]RunbookDataLineage, 0)
	for index, consumer := range trace.Entries {
		for _, input := range consumer.Inputs {
			flow := RunbookDataLineage{
				Origin: "run_input", ToStepID: consumer.StepID, ToSequence: consumer.Sequence,
				Ref: input.Ref, Availability: input.Availability,
			}
			for producerIndex := index - 1; producerIndex >= 0; producerIndex-- {
				producer := trace.Entries[producerIndex]
				matched := false
				for _, output := range producer.Outputs {
					if output.Availability != RunbookDataAvailable {
						continue
					}
					if input.Ref == output.Ref || strings.HasPrefix(input.Ref, strings.TrimRight(output.Ref, "/")+"/") {
						flow.Origin = "node"
						flow.FromStepID = producer.StepID
						flow.FromSequence = producer.Sequence
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			result = append(result, flow)
		}
	}
	return result
}

func (s *RunbookExecutionAuditService) listTurns(ctx context.Context, scope Scope, runID string) ([]*AgentTurn, error) {
	const pageSize = 100
	result := make([]*AgentTurn, 0)
	var after int64
	for {
		page, err := s.store.ListAgentTurns(ctx, AgentTurnFilter{Scope: scope, RunID: runID, AfterSequence: after, Limit: pageSize})
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < pageSize {
			return result, nil
		}
		after = page[len(page)-1].Sequence
	}
}

func (s *RunbookExecutionAuditService) listActions(ctx context.Context, scope Scope, runID string) ([]*ActionCall, error) {
	const pageSize = 100
	result := make([]*ActionCall, 0)
	for offset := 0; ; offset += pageSize {
		page, err := s.store.ListActionCalls(ctx, ActionFilter{Scope: scope, RunID: runID, Limit: pageSize, Offset: offset})
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < pageSize {
			return result, nil
		}
	}
}

func (s *RunbookExecutionAuditService) listApprovals(ctx context.Context, scope Scope, runID string) ([]*ApprovalCheckpoint, error) {
	const pageSize = 100
	result := make([]*ApprovalCheckpoint, 0)
	for offset := 0; ; offset += pageSize {
		page, err := s.store.ListApprovals(ctx, ApprovalFilter{Scope: scope, RunID: runID, Limit: pageSize, Offset: offset})
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < pageSize {
			return result, nil
		}
	}
}

func (s *RunbookExecutionAuditService) listArtifacts(ctx context.Context, scope Scope, runID string) ([]*Artifact, error) {
	const pageSize = 100
	result := make([]*Artifact, 0)
	for offset := 0; ; offset += pageSize {
		page, err := s.store.ListArtifacts(ctx, ArtifactFilter{Scope: scope, ProducerRunID: runID, Limit: pageSize, Offset: offset})
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < pageSize {
			return result, nil
		}
	}
}

func (s *RunbookExecutionAuditService) listChildren(ctx context.Context, scope Scope, runID string) ([]*AgentRun, error) {
	const pageSize = 100
	result := make([]*AgentRun, 0)
	for offset := 0; ; offset += pageSize {
		page, err := s.store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, ParentRunID: runID, Limit: pageSize, Offset: offset})
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < pageSize {
			return result, nil
		}
	}
}

func (s *RunbookExecutionAuditService) listRequests(ctx context.Context, scope Scope, runID string) ([]*AgentRequest, error) {
	const pageSize = 100
	result := make([]*AgentRequest, 0)
	for offset := 0; ; offset += pageSize {
		page, err := s.store.ListAgentRequests(ctx, AgentRequestFilter{Scope: scope, SourceRunID: runID, Limit: pageSize, Offset: offset})
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < pageSize {
			return result, nil
		}
	}
}

func attachRunbookTurns(result *RunbookExecutionAudit, turns []*AgentTurn) {
	for _, turn := range turns {
		if turn == nil {
			continue
		}
		value := RunbookTurnAudit{
			ID: turn.ID, Sequence: turn.Sequence, Status: turn.Status, OutputSummary: turn.OutputSummary,
			Error: turn.Error, Usage: turn.Usage, CreatedAt: turn.CreatedAt, StartedAt: turn.StartedAt, CompletedAt: turn.CompletedAt,
		}
		matched := false
		for nodeIndex := range result.Nodes {
			for visitIndex := range result.Nodes[nodeIndex].Visits {
				visit := &result.Nodes[nodeIndex].Visits[visitIndex]
				if visit.Trace.TurnID == turn.ID {
					visit.Turns = append(visit.Turns, value)
					matched = true
				}
			}
		}
		if !matched {
			result.UnassignedTurns = append(result.UnassignedTurns, value)
		}
	}
}

func attachRunbookActions(result *RunbookExecutionAudit, actions []*ActionCall, approvals []*ApprovalCheckpoint) {
	visitByTurn, visitByAction := runbookAuditVisitIndexes(result)
	for _, call := range actions {
		if call == nil {
			continue
		}
		visit, ok := visitByAction[call.ID]
		if !ok {
			visit, ok = visitByTurn[call.TurnID]
		}
		if !ok {
			result.UnassignedActions = append(result.UnassignedActions, projectRunbookActionAudit(call))
			continue
		}
		visitByAction[call.ID] = visit
		entry := runbookAuditVisit(result, visit)
		entry.Actions = append(entry.Actions, projectRunbookActionAudit(call))
	}
	for _, approval := range approvals {
		if approval == nil {
			continue
		}
		visit, ok := visitByAction[approval.ActionCallID]
		if !ok {
			result.UnassignedApprovals = append(result.UnassignedApprovals, projectRunbookApprovalAudit(approval))
			continue
		}
		entry := runbookAuditVisit(result, visit)
		entry.Approvals = append(entry.Approvals, projectRunbookApprovalAudit(approval))
	}
}

func attachRunbookApprovalDeliveries(result *RunbookExecutionAudit, deliveries map[string][]RunbookDeliveryAudit) {
	if result == nil || len(deliveries) == 0 {
		return
	}
	for nodeIndex := range result.Nodes {
		for visitIndex := range result.Nodes[nodeIndex].Visits {
			for approvalIndex := range result.Nodes[nodeIndex].Visits[visitIndex].Approvals {
				approval := &result.Nodes[nodeIndex].Visits[visitIndex].Approvals[approvalIndex]
				approval.Deliveries = append([]RunbookDeliveryAudit(nil), deliveries[approval.ID]...)
			}
		}
	}
	for index := range result.UnassignedApprovals {
		result.UnassignedApprovals[index].Deliveries = append([]RunbookDeliveryAudit(nil), deliveries[result.UnassignedApprovals[index].ID]...)
	}
}

func attachRunbookArtifacts(result *RunbookExecutionAudit, artifacts []*Artifact) {
	visitByTurn, visitByAction := runbookAuditVisitIndexes(result)
	for _, artifact := range artifacts {
		if artifact == nil {
			continue
		}
		value := projectRunbookArtifactAudit(artifact)
		visit, ok := visitByAction[artifact.Provenance.ActionID]
		if !ok {
			visit, ok = visitByTurn[artifact.Provenance.TurnID]
		}
		if ok {
			entry := runbookAuditVisit(result, visit)
			entry.Artifacts = append(entry.Artifacts, value)
		} else {
			result.UnassignedArtifacts = append(result.UnassignedArtifacts, value)
		}
	}
}

func attachRunbookChildren(result *RunbookExecutionAudit, children []*AgentRun, requests []*AgentRequest, turns []*AgentTurn, parent *AgentRun) {
	stepByChild := map[string]string{}
	for _, child := range children {
		if child == nil {
			continue
		}
		if forkChild, ok := child.Checkpoint["forkChild"].(map[string]interface{}); ok {
			if stepID, _ := forkChild["forkId"].(string); strings.TrimSpace(stepID) != "" {
				stepByChild[child.ID] = strings.TrimSpace(stepID)
			}
		}
	}
	requestByID := make(map[string]*AgentRequest, len(requests))
	for _, request := range requests {
		if request != nil {
			requestByID[request.ID] = request
		}
	}
	for _, turn := range turns {
		if turn == nil || turn.RequestedDelegation == nil {
			continue
		}
		proposal := turn.RequestedDelegation
		key := strings.Join([]string{"delegation", parent.ID, proposal.StepID}, ":")
		requestID := stableCollaborationID(parent.Scope, key, "request")
		if request := requestByID[requestID]; request != nil && request.ChildRunID != "" {
			stepByChild[request.ChildRunID] = proposal.StepID
		}
	}
	for _, child := range children {
		if child == nil {
			continue
		}
		value := projectRunbookChildAudit(child, stepByChild[child.ID])
		if visit, ok := runbookAuditVisitForChild(result, value.StepID, child.CreatedAt); ok {
			entry := runbookAuditVisit(result, visit)
			entry.ChildRuns = append(entry.ChildRuns, value)
		} else {
			result.UnassignedChildren = append(result.UnassignedChildren, value)
		}
	}
}

type runbookAuditVisitRef struct {
	node  int
	visit int
}

func runbookAuditVisit(result *RunbookExecutionAudit, ref runbookAuditVisitRef) *RunbookVisitExecutionAudit {
	return &result.Nodes[ref.node].Visits[ref.visit]
}

func runbookAuditVisitIndexes(result *RunbookExecutionAudit) (map[string]runbookAuditVisitRef, map[string]runbookAuditVisitRef) {
	byTurn := map[string]runbookAuditVisitRef{}
	byAction := map[string]runbookAuditVisitRef{}
	for nodeIndex := range result.Nodes {
		for visitIndex := range result.Nodes[nodeIndex].Visits {
			visit := result.Nodes[nodeIndex].Visits[visitIndex].Trace
			ref := runbookAuditVisitRef{node: nodeIndex, visit: visitIndex}
			// A Turn may execute several deterministic nodes. Its proposed action
			// belongs to the final visit reached before the Turn yielded.
			if visit.TurnID != "" {
				if current, exists := byTurn[visit.TurnID]; !exists || runbookAuditVisit(result, current).Trace.Sequence < visit.Sequence {
					byTurn[visit.TurnID] = ref
				}
			}
			if visit.ActionCallID != "" {
				byAction[visit.ActionCallID] = ref
			}
			for _, action := range result.Nodes[nodeIndex].Visits[visitIndex].Actions {
				byAction[action.ID] = ref
			}
		}
	}
	return byTurn, byAction
}

func runbookAuditVisitForChild(result *RunbookExecutionAudit, stepID string, createdAt time.Time) (runbookAuditVisitRef, bool) {
	var selected runbookAuditVisitRef
	found := false
	for nodeIndex := range result.Nodes {
		if result.Nodes[nodeIndex].StepID != stepID {
			continue
		}
		for visitIndex := range result.Nodes[nodeIndex].Visits {
			trace := result.Nodes[nodeIndex].Visits[visitIndex].Trace
			if trace.StartedAt.After(createdAt) {
				continue
			}
			if !found || runbookAuditVisit(result, selected).Trace.Sequence < trace.Sequence {
				selected = runbookAuditVisitRef{node: nodeIndex, visit: visitIndex}
				found = true
			}
		}
	}
	return selected, found
}

func projectRunbookActionAudit(call *ActionCall) RunbookActionAudit {
	arguments, _ := sanitizePersistedApprovalValue(call.Arguments).(map[string]interface{})
	result, _ := sanitizePersistedApprovalValue(call.Output).(map[string]interface{})
	return RunbookActionAudit{
		ID: call.ID, TurnID: call.TurnID, SkillID: call.SkillID, SkillVersion: call.SkillVersion, Action: call.Action,
		Status: call.Status, Risk: string(call.Risk), SideEffect: string(call.SideEffect), ApprovalID: call.ApprovalID,
		Attempt: call.Attempt, MaxAttempts: call.MaxAttempts, EvidenceRefs: append([]string(nil), call.EvidenceRefs...),
		Arguments: arguments, Result: result, Error: call.Error,
		CreatedAt: call.CreatedAt, UpdatedAt: call.UpdatedAt, StartedAt: call.StartedAt, CompletedAt: call.CompletedAt,
	}
}

func projectRunbookApprovalAudit(approval *ApprovalCheckpoint) RunbookApprovalAudit {
	proposedAction, _ := sanitizePersistedApprovalValue(approval.ProposedAction).(map[string]interface{})
	return RunbookApprovalAudit{
		ID: approval.ID, ActionCallID: approval.ActionCallID, Status: approval.Status, Summary: approval.Summary,
		PolicyReason: approval.PolicyReason, ProposedAction: proposedAction,
		EligibleApprovers: append([]ApprovalPrincipal(nil), approval.EligibleApprovers...),
		DecisionBy:        approval.DecisionBy, DecisionReason: approval.DecisionReason, ExpiresAt: approval.ExpiresAt,
		CreatedAt: approval.CreatedAt, UpdatedAt: approval.UpdatedAt, DecidedAt: approval.DecidedAt,
	}
}

func projectRunbookArtifactAudit(artifact *Artifact) RunbookArtifactAudit {
	return RunbookArtifactAudit{
		ID: artifact.ID, Version: artifact.Version, Name: artifact.Name, Type: artifact.Type, MediaType: artifact.MediaType,
		Classification: artifact.Classification, Availability: artifact.ContentAvailability, Digest: artifact.Digest,
		SizeBytes: artifact.SizeBytes, TurnID: artifact.Provenance.TurnID, ActionID: artifact.Provenance.ActionID, CreatedAt: artifact.CreatedAt,
	}
}

func projectRunbookChildAudit(child *AgentRun, stepID string) RunbookChildRunAudit {
	return RunbookChildRunAudit{
		ID: child.ID, StepID: stepID, AssignedAgentID: child.AssignedAgentID, Goal: child.Goal, Source: child.Source,
		Status: child.Status, Error: child.Error, CreatedAt: child.CreatedAt, StartedAt: child.StartedAt, CompletedAt: child.CompletedAt,
	}
}
