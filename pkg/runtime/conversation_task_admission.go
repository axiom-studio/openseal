package runtime

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/lib/pq"
)

func validateConversationTaskForegroundReserve(reserve, maximum int) error {
	if reserve < 0 || reserve > 0 && (maximum <= 0 || reserve >= maximum) {
		return errors.New("conversation task foreground reserve must be nonnegative and smaller than the agent capacity")
	}
	return nil
}

// These lookups must share the claim's store lock or transaction. Neither a
// caller's context hint nor a root id alone grants task admission authority.
type conversationTaskAdmissionLookup struct {
	task         func(context.Context, Scope, string) (*ConversationTask, error)
	run          func(context.Context, Scope, string) (*AgentRun, error)
	conversation func(context.Context, Scope, string) (*Conversation, error)
	message      func(context.Context, Scope, string, string) (*ChannelMessage, error)
}

func prepareConversationTaskAdmission(ctx context.Context, claim *AgentRunClaim, runs []*AgentRun, lookup conversationTaskAdmissionLookup) error {
	claim.conversationTaskRuns = nil
	claim.invalidConversationTaskRuns = nil
	if claim.ConversationTaskForegroundReserve == 0 {
		claim.conversationTaskActiveRuns = nil
		return nil
	}
	if claim.conversationTaskActiveRuns == nil {
		claim.conversationTaskActiveRuns = make(map[string]bool)
	}
	claim.conversationTaskRuns = make(map[string]bool)
	claim.invalidConversationTaskRuns = make(map[string]bool)
	tasks := make(map[string]*ConversationTask)
	checked := make(map[string]bool)
	persistedRuns := make(map[string]*AgentRun)
	for _, run := range runs {
		if run != nil && run.Scope == claim.Scope {
			persistedRuns[run.ID] = run
		}
	}
	getRun := func(id string) (*AgentRun, error) {
		if run, ok := persistedRuns[id]; ok {
			return run, nil
		}
		run, err := lookup.run(ctx, claim.Scope, id)
		if errors.Is(err, ErrRunNotFound) {
			err = nil
		}
		if err == nil {
			persistedRuns[id] = run
		}
		return run, err
	}
	for _, candidate := range runs {
		if candidate == nil || candidate.Scope != claim.Scope {
			continue
		}
		if candidate.Status == AgentRunStatusRunning && candidate.LeaseExpiresAt != nil && candidate.LeaseExpiresAt.After(claim.Now) {
			if _, known := claim.conversationTaskActiveRuns[candidate.ID]; known {
				continue // PostgreSQL diagnostics batch this metadata in SQL.
			}
		} else if !agentRunEligible(candidate, *claim) {
			continue
		}
		rootID := candidate.RootRunID
		if rootID == "" {
			rootID = candidate.ID
		}
		if !checked[rootID] {
			task, err := lookup.task(ctx, claim.Scope, rootID)
			if err != nil {
				return err
			}
			tasks[rootID], checked[rootID] = task, true
		}
		task := tasks[rootID]
		_, hint := candidate.Context[ConversationTaskContextKey]
		taskLike := hint || strings.HasPrefix(candidate.ConcurrencyKey, "task:") || task != nil
		if candidate.Status == AgentRunStatusRunning && candidate.LeaseExpiresAt != nil && candidate.LeaseExpiresAt.After(claim.Now) {
			claim.conversationTaskActiveRuns[candidate.ID] = taskLike
			continue // Counting a reserved marker can only restrict admission.
		}
		if !taskLike {
			continue
		}
		valid := task != nil && task.Validate() == nil && task.Scope == claim.Scope && task.WorkRunID == rootID &&
			task.ID == conversationTaskID(task.Scope, task.SourceRunID, task.TaskKey)
		if valid {
			root, err := getRun(rootID)
			if err != nil {
				return err
			}
			valid = validConversationTaskAdmissionRoot(root, task)
		}
		if valid {
			source, err := getRun(task.SourceRunID)
			if err != nil {
				return err
			}
			conversation, err := lookup.conversation(ctx, claim.Scope, task.ConversationID)
			if err != nil {
				return err
			}
			message, err := lookup.message(ctx, claim.Scope, task.ConversationID, task.SourceMessageID)
			if err != nil {
				return err
			}
			valid = validConversationTaskAdmissionSource(source, conversation, message, task)
		}
		if valid {
			valid = false
			seen := make(map[string]bool)
			current := candidate
			for depth := 0; depth < 64 && current != nil; depth++ {
				if seen[current.ID] || current.Scope != task.Scope || current.Owner != task.Owner || current.AssignedAgentID != task.TargetAgentID ||
					current.RootRunID != task.WorkRunID || normalizeRunKind(current.Kind) != RunKindAgentWork {
					break
				}
				seen[current.ID] = true
				if current.ID == task.WorkRunID {
					valid = true
					break
				}
				if current.ParentRunID == "" || current.Source != RunSourceFork {
					break
				}
				if hinted, exists := current.Context[ConversationTaskContextKey]; exists && hinted != task.ID {
					break
				}
				var err error
				current, err = getRun(current.ParentRunID)
				if err != nil {
					return err
				}
			}
		}
		if valid {
			claim.conversationTaskRuns[candidate.ID] = true
		} else {
			claim.invalidConversationTaskRuns[candidate.ID] = true
		}
	}
	return nil
}

func validConversationTaskAdmissionRoot(root *AgentRun, task *ConversationTask) bool {
	return root != nil && root.Scope == task.Scope && root.ID == task.WorkRunID && root.ParentRunID == "" && root.RootRunID == root.ID &&
		root.Owner == task.Owner && root.AssignedAgentID == task.TargetAgentID && root.Kind == RunKindAgentWork && root.Source == RunSourceChat &&
		root.ConcurrencyKey == "task:"+task.ID && root.ObjectiveID == "" && root.Context[ConversationTaskContextKey] == task.ID &&
		root.Context[conversationRunContextConversationID] == task.ConversationID && root.Context[conversationRunContextTriggerID] == task.SourceMessageID
}

func validConversationTaskAdmissionSource(source *AgentRun, conversation *Conversation, message *ChannelMessage, task *ConversationTask) bool {
	if source == nil || conversation == nil || message == nil || source.Scope != task.Scope || source.ID != task.SourceRunID ||
		source.Kind != RunKindConversation || source.Source != RunSourceChat || source.ParentRunID != "" || source.RootRunID != source.ID ||
		source.Owner != task.Owner || source.AssignedAgentID != task.TargetAgentID || source.Context[conversationRunContextConversationID] != task.ConversationID ||
		source.Context[conversationRunContextTriggerID] != task.SourceMessageID || conversation.Scope != task.Scope || conversation.ID != task.ConversationID ||
		conversation.Owner != task.Owner || message.Scope != task.Scope || message.ID != task.SourceMessageID || message.ConversationID != task.ConversationID {
		return false
	}
	actor := message.Sender
	if actor.Type == ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	thread := message.ThreadRootID
	if thread == "" {
		thread = message.ID
	}
	return actor == task.AuthenticatedActor && thread == task.ThreadRootID
}

func (s *MemoryStore) prepareConversationTaskAdmissionLocked(ctx context.Context, claim *AgentRunClaim, runs []*AgentRun) error {
	return prepareConversationTaskAdmission(ctx, claim, runs, conversationTaskAdmissionLookup{
		task: func(_ context.Context, scope Scope, id string) (*ConversationTask, error) {
			return s.conversationTasks[s.conversationTaskWorkRuns[portfolioKey(scope, id)]], nil
		},
		run: func(_ context.Context, scope Scope, id string) (*AgentRun, error) {
			return s.agentRuns[portfolioKey(scope, id)], nil
		},
		conversation: func(_ context.Context, scope Scope, id string) (*Conversation, error) {
			return s.conversations[conversationStoreKey(scope, id)], nil
		},
		message: func(_ context.Context, scope Scope, conversationID, id string) (*ChannelMessage, error) {
			return s.channelMessageIDs[channelMessageStoreKey(scope, conversationID, id)], nil
		},
	})
}

func (s *SQLiteStore) prepareConversationTaskAdmissionConn(ctx context.Context, conn *sql.Conn, claim *AgentRunClaim, runs []*AgentRun) error {
	return prepareConversationTaskAdmission(ctx, claim, runs, conversationTaskAdmissionLookup{
		task: func(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
			var payload string
			err := conn.QueryRowContext(ctx, `SELECT payload FROM conversation_tasks WHERE scope_kind = ? AND scope_id = ? AND work_run_id = ?`, scope.Kind, scope.ID, id).Scan(&payload)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return decodeConversationTaskSQL(payload)
		},
		run: func(ctx context.Context, scope Scope, id string) (*AgentRun, error) {
			return getSQLiteAgentRun(ctx, conn, scope, id)
		},
		conversation: func(ctx context.Context, scope Scope, id string) (*Conversation, error) {
			return getSQLiteConversation(ctx, conn, scope, id)
		},
		message: func(ctx context.Context, scope Scope, conversationID, id string) (*ChannelMessage, error) {
			return getSQLiteChannelMessage(ctx, conn, scope, conversationID, id)
		},
	})
}

// This predicate only restricts capacity. Canonical execution authority is
// checked afterward against the selected candidate's stored source and lineage.
func (s *PostgresStore) conversationTaskLikeSQL(alias string) string {
	return `COALESCE(` + alias + `.payload->'context', '{}'::jsonb) ? 'openseal.conversationTaskId'
		OR COALESCE(` + alias + `.payload->>'concurrencyKey', '') LIKE 'task:%'
		OR EXISTS (SELECT 1 FROM ` + s.table("conversation_tasks") + ` AS admission_task
			WHERE admission_task.scope_kind = ` + alias + `.scope_kind AND admission_task.scope_id = ` + alias + `.scope_id
			AND admission_task.work_run_id = ` + alias + `.root_run_id OFFSET 0)`
}

func (s *PostgresStore) preparePostgresConversationTaskAdmission(ctx context.Context, tx *sql.Tx, claim *AgentRunClaim, runs []*AgentRun) error {
	return prepareConversationTaskAdmission(ctx, claim, runs, conversationTaskAdmissionLookup{
		task: func(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
			var payload string
			err := tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("conversation_tasks")+` WHERE scope_kind = $1 AND scope_id = $2 AND work_run_id = $3`, scope.Kind, scope.ID, id).Scan(&payload)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return decodeConversationTaskSQL(payload)
		},
		run: func(ctx context.Context, scope Scope, id string) (*AgentRun, error) {
			return s.getPostgresAgentRunTx(ctx, tx, scope, id, true)
		},
		conversation: func(ctx context.Context, scope Scope, id string) (*Conversation, error) {
			return s.getPostgresConversation(ctx, tx, scope, id, false)
		},
		message: func(ctx context.Context, scope Scope, conversationID, id string) (*ChannelMessage, error) {
			return s.getPostgresChannelMessage(ctx, tx, scope, conversationID, id, false)
		},
	})
}

// Reserve diagnostics inspect a bounded candidate page and only relevant live
// claims. Historical and queued portfolio payloads are never loaded wholesale.
func (s *PostgresStore) explainPostgresConversationTaskAdmission(ctx context.Context, tx *sql.Tx, claim AgentRunClaim) (*AgentRunAdmissionDecision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM `+s.table("agent_runs")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND status IN ($3, $4)
		AND ($5 = '' OR assigned_agent_id = $5) AND ($6 = '' OR COALESCE(payload->>'kind', 'agent_work') = $6)
		AND ((status = $3 AND available_at <= $7 AND (lease_expires_at IS NULL OR lease_expires_at <= $7))
			OR (status = $4 AND (lease_expires_at IS NULL OR lease_expires_at <= $7)))
		ORDER BY queue_entered_at, id LIMIT 32`, claim.Scope.Kind, claim.Scope.ID, AgentRunStatusQueued, AgentRunStatusRunning, claim.AssignedAgentID, claim.Kind, claim.Now)
	if err != nil {
		return nil, err
	}
	candidates := make([]*AgentRun, 0)
	agents, owners, objectives, concurrency := make([]string, 0), make([]string, 0), make([]string, 0), make([]string, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return nil, err
		}
		run, err := decodeAgentRun(payload)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, run)
		agents = append(agents, run.AssignedAgentID)
		if claim.MaxActiveForOwner > 0 {
			owners = append(owners, agentRunOwnerSchedulingKey(run.Owner))
		}
		if run.ObjectiveID != "" {
			objectives = append(objectives, run.ObjectiveID)
		}
		if claim.MaxActiveForConcurrencyKey > 0 && run.ConcurrencyKey != "" {
			concurrency = append(concurrency, run.ConcurrencyKey)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	claim.conversationTaskActiveRuns = make(map[string]bool)
	runs := append([]*AgentRun(nil), candidates...)
	rows, err = tx.QueryContext(ctx, `SELECT active.payload, (`+s.conversationTaskLikeSQL("active")+`) FROM `+s.table("agent_runs")+` AS active
		WHERE active.scope_kind = $1 AND active.scope_id = $2 AND active.status = $3 AND active.lease_expires_at > $4
		AND (active.assigned_agent_id = ANY($5) OR
			((active.payload->'owner'->>'type') || chr(31) || (active.payload->'owner'->>'id')) = ANY($6) OR
			active.objective_id = ANY($7) OR COALESCE(active.payload->>'concurrencyKey', '') = ANY($8))`,
		claim.Scope.Kind, claim.Scope.ID, AgentRunStatusRunning, claim.Now, pq.Array(agents), pq.Array(owners), pq.Array(objectives), pq.Array(concurrency))
	if err != nil {
		return nil, err
	}
	heldIDs := make(map[string]bool)
	for rows.Next() {
		var payload string
		var taskLike bool
		if err := rows.Scan(&payload, &taskLike); err != nil {
			rows.Close()
			return nil, err
		}
		run, err := decodeAgentRun(payload)
		if err != nil {
			rows.Close()
			return nil, err
		}
		heldIDs[run.ID] = true
		claim.conversationTaskActiveRuns[run.ID] = taskLike
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// Avoid counting a held candidate twice when it also appears in the page.
	runs = runs[len(candidates):]
	for _, candidate := range candidates {
		if !heldIDs[candidate.ID] {
			runs = append(runs, candidate)
		}
	}
	if err := s.preparePostgresConversationTaskAdmission(ctx, tx, &claim, runs); err != nil {
		return nil, err
	}
	policy := make(map[string]*Objective)
	if len(objectives) > 0 {
		rows, err = tx.QueryContext(ctx, `SELECT payload FROM `+s.table("objectives")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = ANY($3)`, claim.Scope.Kind, claim.Scope.ID, pq.Array(objectives))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return nil, err
			}
			objective, err := decodeObjective(payload)
			if err != nil {
				rows.Close()
				return nil, err
			}
			policy[objective.ID] = objective
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	_, decision := evaluateAgentRunAdmission(runs, policy, claim)
	// This is a bounded observation. A later candidate may be ready, but this
	// diagnostic never grants authority or expands the selection batch.
	if decision.Outcome == AgentRunAdmissionReady {
		decision.Outcome = AgentRunAdmissionBackpressured
	}
	return decision, nil
}

// The original indexed capacity query is shared with the query-plan regression.
func (s *PostgresStore) agentRunAdmissionSQL(taskAdmissionSQL string) string {
	return `SELECT candidate.payload FROM ` + s.table("agent_runs") + ` AS candidate
		LEFT JOIN ` + s.table("objectives") + ` AS objective
		ON objective.scope_kind = candidate.scope_kind AND objective.scope_id = candidate.scope_id AND objective.id = candidate.objective_id
		WHERE candidate.scope_kind = $1 AND candidate.scope_id = $2
		AND ($3 = '' OR candidate.assigned_agent_id = $3)
		AND ($9 = '' OR COALESCE(candidate.payload->>'kind', 'agent_work') = $9)
		AND ((candidate.status = $4 AND candidate.available_at <= $6 AND (candidate.lease_expires_at IS NULL OR candidate.lease_expires_at <= $6))
		  OR (candidate.status = $5 AND (candidate.lease_expires_at IS NULL OR candidate.lease_expires_at <= $6)))
		AND ($7 = 0 OR candidate.assigned_agent_id = '' OR (
			SELECT COUNT(*) FROM ` + s.table("agent_runs") + ` AS active
			WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
			AND active.assigned_agent_id = candidate.assigned_agent_id AND active.status = $5
			AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
		) < $7)
		AND ($10 = 0 OR COALESCE(candidate.payload->>'concurrencyKey', '') = '' OR (
			SELECT COUNT(*) FROM ` + s.table("agent_runs") + ` AS active
			WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
			AND COALESCE(active.payload->>'concurrencyKey', '') = COALESCE(candidate.payload->>'concurrencyKey', '')
			AND active.status = $5 AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
		) < $10)
		AND ($11 = 0 OR (
			SELECT COUNT(*) FROM ` + s.table("agent_runs") + ` AS active
			WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
			AND active.payload->'owner' = candidate.payload->'owner'
			AND active.status = $5 AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
		) < $11)
		AND (candidate.objective_id = '' OR
			(CASE
				WHEN $12 > 0 AND COALESCE(NULLIF(objective.payload->'executionPolicy'->>'maximumConcurrentRuns', '')::integer, 0) > 0
					THEN LEAST($12, (objective.payload->'executionPolicy'->>'maximumConcurrentRuns')::integer)
				WHEN $12 > 0 THEN $12
				ELSE COALESCE(NULLIF(objective.payload->'executionPolicy'->>'maximumConcurrentRuns', '')::integer, 0)
			END) = 0 OR (
			SELECT COUNT(*) FROM ` + s.table("agent_runs") + ` AS active
			WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
			AND active.objective_id = candidate.objective_id
			AND active.status = $5 AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
		) < (CASE
			WHEN $12 > 0 AND COALESCE(NULLIF(objective.payload->'executionPolicy'->>'maximumConcurrentRuns', '')::integer, 0) > 0
				THEN LEAST($12, (objective.payload->'executionPolicy'->>'maximumConcurrentRuns')::integer)
			WHEN $12 > 0 THEN $12
			ELSE COALESCE(NULLIF(objective.payload->'executionPolicy'->>'maximumConcurrentRuns', '')::integer, 0)
		END))
		AND NOT EXISTS (
			SELECT 1
			FROM jsonb_each_text(COALESCE(candidate.payload->'resourceRequirements', '{}'::jsonb)) AS requirement(resource, quantity)
			WHERE COALESCE(objective.payload->'executionPolicy'->'resourceCapacities', '{}'::jsonb) ? requirement.resource
			AND COALESCE((
				SELECT SUM(COALESCE(NULLIF(active.payload->'resourceRequirements'->>requirement.resource, '')::integer, 0))
				FROM ` + s.table("agent_runs") + ` AS active
				WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
				AND active.objective_id = candidate.objective_id
				AND active.status = $5 AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
			), 0) + requirement.quantity::integer >
				COALESCE(NULLIF(objective.payload->'executionPolicy'->'resourceCapacities'->>requirement.resource, '')::integer, 0)
		)
		` + taskAdmissionSQL + `
		ORDER BY candidate.priority + FLOOR(GREATEST(EXTRACT(EPOCH FROM ($6 - candidate.queue_entered_at)), 0) / $8) DESC,
			candidate.deadline ASC NULLS LAST, candidate.queue_entered_at ASC, candidate.id ASC
		FOR UPDATE OF candidate SKIP LOCKED LIMIT 1`
}

func (s *PostgresStore) conversationTaskAdmissionSQL() string {
	return `AND NOT (candidate.id = ANY($14))
				AND (NOT (` + s.conversationTaskLikeSQL("candidate") + `) OR (
					SELECT COUNT(*) FROM ` + s.table("agent_runs") + ` AS task_active
					WHERE task_active.scope_kind = candidate.scope_kind AND task_active.scope_id = candidate.scope_id
					AND task_active.assigned_agent_id = candidate.assigned_agent_id AND task_active.status = $5
					AND task_active.lease_expires_at IS NOT NULL AND task_active.lease_expires_at > $6
					AND (` + s.conversationTaskLikeSQL("task_active") + `)
				) < $13)`
}
