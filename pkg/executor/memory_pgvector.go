/*
 * Copyright (c) 2025. Axiom Studio
 */

package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

// PGVectorStore implements MemoryStore using PostgreSQL with pgvector
type PGVectorStore struct {
	connStr   string
	tableName string
	db        *sql.DB
	mu        sync.RWMutex
	connPool  map[string]*sql.DB
}

// NewPGVectorStore creates a new pgvector-backed memory store
func NewPGVectorStore(connStr, tableName string) (*PGVectorStore, error) {
	if tableName == "" {
		tableName = "agent_memories"
	}
	store := &PGVectorStore{
		connStr:   connStr,
		tableName: tableName,
		connPool:  make(map[string]*sql.DB),
	}

	db, err := store.getConnection(connStr)
	if err != nil {
		return nil, err
	}

	store.db = db
	return store, nil
}

// getConnection gets or creates a database connection
func (s *PGVectorStore) getConnection(connStr string) (*sql.DB, error) {
	s.mu.RLock()
	if db, ok := s.connPool[connStr]; ok {
		s.mu.RUnlock()
		return db, nil
	}
	s.mu.RUnlock()

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	s.mu.Lock()
	s.connPool[connStr] = db
	s.mu.Unlock()

	return db, nil
}

// EnsureTable creates the memories table if it doesn't exist
func (s *PGVectorStore) EnsureTable(ctx context.Context) error {
	// Enable pgvector extension
	_, err := s.db.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	if err != nil {
		// Extension might already exist or not available, continue
	}

	// Create table
	query := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			key TEXT NOT NULL,
			content TEXT NOT NULL,
			embedding vector(1536),
			scope TEXT NOT NULL DEFAULT 'session',
			session_id TEXT,
			user_id TEXT,
			metadata JSONB,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW(),
			UNIQUE(key, scope, session_id)
		);
		CREATE INDEX IF NOT EXISTS idx_%s_scope ON %s(scope);
		CREATE INDEX IF NOT EXISTS idx_%s_session ON %s(session_id);
		CREATE INDEX IF NOT EXISTS idx_%s_embedding ON %s USING ivfflat (embedding vector_cosine_ops);
	`, s.tableName, s.tableName, s.tableName, s.tableName, s.tableName, s.tableName, s.tableName)

	_, err = s.db.ExecContext(ctx, query)
	return err
}

// Save stores a memory record
func (s *PGVectorStore) Save(ctx context.Context, record *MemoryRecord) error {
	query := fmt.Sprintf(`
		INSERT INTO %s (id, key, content, embedding, scope, session_id, user_id, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (key, scope, session_id) DO UPDATE SET
			content = EXCLUDED.content,
			embedding = EXCLUDED.embedding,
			metadata = EXCLUDED.metadata,
			updated_at = EXCLUDED.updated_at
		RETURNING id
	`, s.tableName)

	metadataJSON, _ := json.Marshal(record.Metadata)
	id := record.ID
	if id == "" {
		id = generateKeyFromContent(record.Key + record.Scope + record.SessionID)
	}

	now := time.Now()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}

	err := s.db.QueryRowContext(ctx, query,
		id,
		record.Key,
		record.Content,
		pgVectorLiteral(record.Embedding),
		record.Scope,
		record.SessionID,
		record.UserID,
		metadataJSON,
		record.CreatedAt,
		now,
	).Scan(&id)

	if err != nil {
		return fmt.Errorf("failed to save memory: %w", err)
	}

	record.ID = id
	record.UpdatedAt = now
	return nil
}

// Recall searches for similar memories
func (s *PGVectorStore) Recall(ctx context.Context, query string, embedding []float64, scope, sessionID, userID string, limit int) ([]MemorySearchResult, error) {
	sqlQuery := fmt.Sprintf(`
		SELECT id, key, content, scope, session_id, user_id, metadata,
		       1 - (embedding <=> $1) as similarity,
		       created_at, updated_at
		FROM %s
		WHERE ($2 = '' OR scope = $2)
		  AND ($3 = '' OR session_id = $3 OR scope = 'global')
		  AND ($4 = '' OR user_id = $4 OR scope IN ('global', 'session'))
		ORDER BY embedding <=> $1
		LIMIT $5
	`, s.tableName)

	rows, err := s.db.QueryContext(ctx, sqlQuery, pgVectorLiteral(embedding), scope, sessionID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to recall memories: %w", err)
	}
	defer rows.Close()

	results := make([]MemorySearchResult, 0)
	for rows.Next() {
		var r MemorySearchResult
		var metadataJSON []byte

		if err := rows.Scan(&r.ID, &r.Key, &r.Content, &r.Scope, &r.SessionID, &r.UserID, &metadataJSON, &r.Similarity, &r.CreatedAt, &r.UpdatedAt); err != nil {
			continue
		}

		json.Unmarshal(metadataJSON, &r.Metadata)
		results = append(results, r)
	}

	return results, nil
}

// Forget deletes memories matching the criteria
func (s *PGVectorStore) Forget(ctx context.Context, key, scope, sessionID, userID string) (int, error) {
	query := fmt.Sprintf("DELETE FROM %s WHERE 1=1", s.tableName)
	params := []interface{}{}
	paramIdx := 1

	if key != "" {
		query += fmt.Sprintf(" AND key = $%d", paramIdx)
		params = append(params, key)
		paramIdx++
	}
	if scope != "" {
		query += fmt.Sprintf(" AND scope = $%d", paramIdx)
		params = append(params, scope)
		paramIdx++
	}
	query += fmt.Sprintf(" AND (session_id = $%d OR user_id = $%d OR scope = 'global')", paramIdx, paramIdx+1)
	params = append(params, sessionID, userID)

	result, err := s.db.ExecContext(ctx, query, params...)
	if err != nil {
		return 0, fmt.Errorf("failed to forget memory: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	return int(rowsAffected), nil
}

// List returns all memories matching the criteria
func (s *PGVectorStore) List(ctx context.Context, scope, sessionID, userID string, limit int) ([]MemoryRecord, error) {
	query := fmt.Sprintf(`
		SELECT id, key, content, scope, session_id, user_id, metadata, created_at, updated_at
		FROM %s
		WHERE ($1 = '' OR scope = $1)
		  AND ($2 = '' OR session_id = $2 OR scope = 'global')
		  AND ($3 = '' OR user_id = $3 OR scope IN ('global', 'session'))
		ORDER BY updated_at DESC
		LIMIT $4
	`, s.tableName)

	rows, err := s.db.QueryContext(ctx, query, scope, sessionID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list memories: %w", err)
	}
	defer rows.Close()

	records := make([]MemoryRecord, 0)
	for rows.Next() {
		var r MemoryRecord
		var metadataJSON []byte

		if err := rows.Scan(&r.ID, &r.Key, &r.Content, &r.Scope, &r.SessionID, &r.UserID, &metadataJSON, &r.CreatedAt, &r.UpdatedAt); err != nil {
			continue
		}

		json.Unmarshal(metadataJSON, &r.Metadata)
		records = append(records, r)
	}

	return records, nil
}

// Close cleans up resources
func (s *PGVectorStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, db := range s.connPool {
		db.Close()
	}
	s.connPool = make(map[string]*sql.DB)
	s.db = nil
	return nil
}

// pgVectorLiteral converts float slice to pgvector literal
func pgVectorLiteral(v []float64) string {
	return fmt.Sprintf("[%s]", floatSliceToString(v))
}

// floatSliceToString converts float slice to comma-separated string
func floatSliceToString(v []float64) string {
	result := ""
	for i, f := range v {
		if i > 0 {
			result += ","
		}
		result += fmt.Sprintf("%f", f)
	}
	return result
}