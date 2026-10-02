package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const skillRuntimeMaintenanceWakeType = "skill_runtime_maintenance"
const skillRuntimeMaintenanceCheckpointKey = "_kernelSkillRuntimeMaintenance"

type skillRuntimeMaintenanceWaiterStore interface {
	ListCompletedSkillRuntimeMaintenanceWaiters(context.Context, Scope, int) ([]*AgentRun, error)
}

func (s *MemoryStore) refreshMemorySkillRuntimeMaintenanceWaiterLocked(runKey string) {
	if s.skillRuntimeMaintenanceWaiters == nil {
		s.skillRuntimeMaintenanceWaiters = make(map[string]map[string]struct{})
	}
	if s.skillRuntimeMaintenanceWaiterKeys == nil {
		s.skillRuntimeMaintenanceWaiterKeys = make(map[string]string)
	}
	if old := s.skillRuntimeMaintenanceWaiterKeys[runKey]; old != "" {
		delete(s.skillRuntimeMaintenanceWaiters[old], runKey)
		if len(s.skillRuntimeMaintenanceWaiters[old]) == 0 {
			delete(s.skillRuntimeMaintenanceWaiters, old)
		}
		delete(s.skillRuntimeMaintenanceWaiterKeys, runKey)
	}
	run := s.agentRuns[runKey]
	if run == nil || run.Status != AgentRunStatusWaitingForDependency || run.WakeCondition == nil || run.WakeCondition.Type != skillRuntimeMaintenanceWakeType {
		return
	}
	key := skillRuntimeMaintenanceLockKey(run.Scope, run.WakeCondition.Reference)
	if s.skillRuntimeMaintenanceWaiters[key] == nil {
		s.skillRuntimeMaintenanceWaiters[key] = make(map[string]struct{})
	}
	s.skillRuntimeMaintenanceWaiters[key][runKey] = struct{}{}
	s.skillRuntimeMaintenanceWaiterKeys[runKey] = key
}

func (s *MemoryStore) ListCompletedSkillRuntimeMaintenanceWaiters(_ context.Context, scope Scope, limit int) ([]*AgentRun, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		return nil, ErrSkillRuntimeMaintenanceConflict
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*AgentRun, 0, limit)
	for key, waiters := range s.skillRuntimeMaintenanceWaiters {
		g := s.skillRuntimeMaintenance[key]
		if g == nil || g.Scope != scope || g.Active {
			continue
		}
		for runKey := range waiters {
			run := s.agentRuns[runKey]
			if run == nil || run.Scope != scope || run.Status != AgentRunStatusWaitingForDependency {
				continue
			}
			at := sort.Search(len(result), func(i int) bool { return result[i].ID >= run.ID })
			if at >= limit {
				continue
			}
			result = append(result, nil)
			copy(result[at+1:], result[at:])
			result[at] = run
			if len(result) > limit {
				result = result[:limit]
			}
		}
	}
	for i, r := range result {
		result[i] = cloneAgentRun(r)
	}
	return result, nil
}

func listCompletedMaintenanceWaiters(ctx context.Context, q maintenanceSQLQuery, runs, gates string, scope Scope, limit int, postgres bool) ([]*AgentRun, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		return nil, ErrSkillRuntimeMaintenanceConflict
	}
	query := `SELECT r.payload FROM ` + runs + ` r JOIN ` + gates + ` m ON m.scope_kind=r.scope_kind AND m.scope_id=r.scope_id AND m.skill_id=json_extract(r.payload,'$.wakeCondition.reference')
		WHERE r.scope_kind=? AND r.scope_id=? AND r.status='waiting_for_dependency' AND json_extract(r.payload,'$.wakeCondition.type')='skill_runtime_maintenance' AND m.active=0 ORDER BY r.id LIMIT ?`
	if postgres {
		query = `SELECT r.payload FROM ` + runs + ` r JOIN ` + gates + ` m ON m.scope_kind=r.scope_kind AND m.scope_id=r.scope_id AND m.skill_id=r.payload->'wakeCondition'->>'reference'
		WHERE r.scope_kind=$1 AND r.scope_id=$2 AND r.status='waiting_for_dependency' AND r.payload->'wakeCondition'->>'type'='skill_runtime_maintenance' AND m.active=FALSE ORDER BY r.id LIMIT $3`
	}
	rows, err := q.QueryContext(ctx, query, scope.Kind, scope.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*AgentRun, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var run AgentRun
		if err := json.Unmarshal([]byte(payload), &run); err != nil {
			return nil, err
		}
		if run.Scope != scope || run.Status != AgentRunStatusWaitingForDependency || run.WakeCondition == nil || run.WakeCondition.Type != skillRuntimeMaintenanceWakeType {
			return nil, ErrSkillRuntimeMaintenanceConflict
		}
		result = append(result, &run)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ListCompletedSkillRuntimeMaintenanceWaiters(ctx context.Context, scope Scope, limit int) ([]*AgentRun, error) {
	return listCompletedMaintenanceWaiters(ctx, s.db, "agent_runs", "skill_runtime_maintenance", scope, limit, false)
}
func (s *PostgresStore) ListCompletedSkillRuntimeMaintenanceWaiters(ctx context.Context, scope Scope, limit int) ([]*AgentRun, error) {
	return listCompletedMaintenanceWaiters(ctx, s.db, s.table("agent_runs"), s.table("skill_runtime_maintenance"), scope, limit, true)
}

func (s *AgentRunWakeService) wakeCompletedSkillRuntimeMaintenances(ctx context.Context, scope Scope, at time.Time) (*WakeResult, error) {
	store, ok := s.portfolio.(skillRuntimeMaintenanceWaiterStore)
	if !ok {
		return &WakeResult{}, nil
	}
	runs, err := store.ListCompletedSkillRuntimeMaintenanceWaiters(ctx, scope, 200)
	if err != nil {
		return nil, err
	}
	result := &WakeResult{Runs: make([]WokenRun, 0, len(runs))}
	for _, run := range runs {
		checkpoint := cloneMap(run.Checkpoint)
		delete(checkpoint, skillRuntimeMaintenanceCheckpointKey)
		id := fmt.Sprintf("skill-runtime-ready:%s:%d", run.ID, run.Revision)
		woken, event, err := s.activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusQueued, Checkpoint: checkpoint,
			Summary: "Skill runtime is ready; continuing with current authorized tools", EventType: "run.skill_runtime_ready",
			Actor: ActivityActor{Type: "scheduler", ID: "skill-runtime-maintenance"}, WakeSignalID: id, OccurredAt: &at,
		})
		if errors.Is(err, ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result.Runs = append(result.Runs, WokenRun{Run: woken, Event: event})
	}
	return result, nil
}

func (p *AgentRunWorkerPool) parkSkillRuntimeMaintenance(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, cause error) bool {
	var held *SkillRuntimeMaintenanceError
	if !errors.As(cause, &held) {
		return false
	}
	checkpoint := cloneMap(run.Checkpoint)
	if checkpoint == nil {
		checkpoint = make(map[string]interface{})
	}
	checkpoint[skillRuntimeMaintenanceCheckpointKey] = map[string]interface{}{"skillId": held.Maintenance.SkillID, "operationId": held.Maintenance.OperationID}
	turnID := ""
	if turn != nil {
		turnID = turn.ID
	}
	_, _, err := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusWaitingForDependency, LeaseOwner: workerID, Checkpoint: checkpoint,
		WakeCondition: &WakeCondition{Type: skillRuntimeMaintenanceWakeType, Reference: held.Maintenance.SkillID},
		Summary:       "Waiting for the Skill runtime upgrade to finish", EventType: "run.skill_runtime_waiting",
		Actor: ActivityActor{Type: "worker", ID: workerID}, TurnID: turnID, CausationID: turnID,
	})
	if err != nil {
		p.logger.Warnw("failed to park Run for Skill runtime maintenance", "runId", run.ID, "error", err)
	}
	return true
}
