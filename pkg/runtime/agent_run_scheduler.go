package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
)

type AgentRunClaimRequest struct {
	Scope                             Scope
	Kind                              RunKind
	WorkerID                          string
	AssignedAgentID                   string
	LeaseDuration                     time.Duration
	AgingInterval                     time.Duration
	MaxActiveForAgent                 int
	MaxActiveForOwner                 int
	MaxActiveForObjective             int
	MaxActiveForConcurrencyKey        int
	ConversationTaskForegroundReserve int
}

type AgentRunScheduler struct {
	store AgentRunScheduleStore
	now   func() time.Time
}

func NewAgentRunScheduler(store AgentRunScheduleStore) *AgentRunScheduler {
	return &AgentRunScheduler{store: store, now: time.Now}
}

func (s *AgentRunScheduler) ClaimNext(ctx context.Context, req AgentRunClaimRequest) (*AgentRun, error) {
	decision, err := s.ClaimNextDecision(ctx, req)
	if err != nil || decision == nil {
		return nil, err
	}
	return decision.Run, nil
}

func (s *AgentRunScheduler) ClaimNextDecision(ctx context.Context, req AgentRunClaimRequest) (*AgentRunAdmissionDecision, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("agent run scheduler is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		return nil, errors.New("worker id is required")
	}
	if req.LeaseDuration <= 0 {
		req.LeaseDuration = 30 * time.Second
	}
	if req.AgingInterval <= 0 {
		req.AgingInterval = time.Minute
	}
	claim := AgentRunClaim{
		Scope: req.Scope, Kind: req.Kind, WorkerID: req.WorkerID, AssignedAgentID: req.AssignedAgentID,
		Now: s.now(), LeaseDuration: req.LeaseDuration, AgingInterval: req.AgingInterval,
		MaxActiveForAgent: req.MaxActiveForAgent, MaxActiveForOwner: req.MaxActiveForOwner,
		MaxActiveForObjective: req.MaxActiveForObjective, MaxActiveForConcurrencyKey: req.MaxActiveForConcurrencyKey,
		ConversationTaskForegroundReserve: req.ConversationTaskForegroundReserve,
	}
	if store, ok := s.store.(AgentRunAdmissionStore); ok {
		return store.ClaimNextAgentRunWithDecision(ctx, claim)
	}
	run, err := s.store.ClaimNextAgentRun(ctx, claim)
	if err != nil {
		return nil, err
	}
	outcome := AgentRunAdmissionIdle
	if run != nil {
		outcome = AgentRunAdmissionClaimed
	}
	return &AgentRunAdmissionDecision{Outcome: outcome, Run: run, EvaluatedAt: claim.Now}, nil
}

// Inspect returns the current portfolio admission decision without acquiring a
// lease. It is an observational projection for operators and user interfaces;
// only ClaimNextDecision grants execution authority.
func (s *AgentRunScheduler) Inspect(ctx context.Context, req AgentRunClaimRequest) (*AgentRunAdmissionDecision, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("agent run scheduler is not configured")
	}
	portfolio, ok := s.store.(PortfolioStore)
	if !ok {
		return nil, errors.New("agent run admission inspection is unavailable")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if req.AgingInterval <= 0 {
		req.AgingInterval = time.Minute
	}
	claim := AgentRunClaim{
		Scope: req.Scope, Kind: req.Kind, WorkerID: "admission-inspector", Now: s.now(), LeaseDuration: time.Second,
		AgingInterval: req.AgingInterval, AssignedAgentID: req.AssignedAgentID,
		MaxActiveForAgent: req.MaxActiveForAgent, MaxActiveForOwner: req.MaxActiveForOwner,
		MaxActiveForObjective: req.MaxActiveForObjective, MaxActiveForConcurrencyKey: req.MaxActiveForConcurrencyKey,
		ConversationTaskForegroundReserve: req.ConversationTaskForegroundReserve,
	}
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	runs := make([]*AgentRun, 0)
	filterKind := req.Kind
	if claim.ConversationTaskForegroundReserve > 0 {
		filterKind = "" // Capacity is shared with foreground conversation claims.
	}
	for offset := 0; ; offset += 500 {
		page, err := portfolio.ListAgentRuns(ctx, AgentRunFilter{
			Scope: req.Scope, Kind: filterKind, AssignedAgentID: req.AssignedAgentID,
			Statuses: []AgentRunStatus{AgentRunStatusQueued, AgentRunStatusRunning}, Limit: 500, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		runs = append(runs, page...)
		if len(page) < 500 {
			break
		}
	}
	if claim.ConversationTaskForegroundReserve > 0 {
		tasks, ok := s.store.(ConversationTaskStore)
		conversations, conversationOK := s.store.(ConversationStore)
		if !ok || !conversationOK {
			return nil, errors.New("conversation task admission proof is unavailable")
		}
		if err := prepareConversationTaskAdmission(ctx, &claim, runs, conversationTaskAdmissionLookup{
			Task: tasks.FindConversationTaskByWorkRunID, Run: portfolio.GetAgentRun,
			Conversation: conversations.GetConversation, Message: conversations.GetChannelMessage,
			Request: func(ctx context.Context, scope Scope, id string) (*AgentRequest, error) {
				if requests, ok := s.store.(interface {
					GetAgentRequest(context.Context, Scope, string) (*AgentRequest, error)
				}); ok {
					return requests.GetAgentRequest(ctx, scope, id)
				}
				return nil, nil
			},
			Round: func(ctx context.Context, scope Scope, conversationID, key string) (*ParticipationRound, error) {
				result, err := conversations.FindParticipationRoundByIdempotencyKey(ctx, scope, conversationID, key)
				if err != nil || result == nil {
					return nil, err
				}
				return result.Round, nil
			},
		}); err != nil {
			return nil, err
		}
	}
	objectives := make(map[string]*Objective)
	for offset := 0; ; offset += 500 {
		page, err := portfolio.ListObjectives(ctx, ObjectiveFilter{Scope: req.Scope, IncludeRetired: true, Limit: 500, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, objective := range page {
			objectives[objective.ID] = objective
		}
		if len(page) < 500 {
			break
		}
	}
	selected, decision := evaluateAgentRunAdmission(runs, objectives, claim)
	if selected != nil {
		decision.Run = cloneAgentRun(selected)
	}
	return decision, nil
}

func (s *AgentRunScheduler) RenewLease(ctx context.Context, scope Scope, runID, workerID string, leaseDuration time.Duration) (*AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("agent run scheduler is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(workerID) == "" {
		return nil, errors.New("run id and worker id are required")
	}
	if leaseDuration <= 0 {
		leaseDuration = 30 * time.Second
	}
	return s.store.RenewAgentRunLease(ctx, scope, runID, workerID, s.now(), leaseDuration)
}

func runAttemptBudgetExceeded(run *AgentRun) bool {
	return run != nil && run.Budget != nil && run.Budget.MaxAttempts > 0 && run.BudgetUsage.Attempts > run.Budget.MaxAttempts
}
