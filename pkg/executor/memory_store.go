/*
 * Copyright (c) 2025. Axiom Studio
 */

package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"sync"
	"time"
)

// MemoryStore defines the interface for memory storage backends
type MemoryStore interface {
	Save(ctx context.Context, record *MemoryRecord) error
	Recall(ctx context.Context, query string, embedding []float64, scope, sessionID, userID string, limit int) ([]MemorySearchResult, error)
	Forget(ctx context.Context, key, scope, sessionID, userID string) (int, error)
	List(ctx context.Context, scope, sessionID, userID string, limit int) ([]MemoryRecord, error)
	Close() error
}

// MemorySearchResult represents a search result with similarity score
type MemorySearchResult struct {
	MemoryRecord
	Similarity float64
}

// InMemoryStore implements an in-memory vector store for testing
type InMemoryStore struct {
	memories map[string]*MemoryRecord
	mu       sync.RWMutex
}

// NewInMemoryStore creates a new in-memory memory store
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		memories: make(map[string]*MemoryRecord),
	}
}

// Save stores a memory record
func (s *InMemoryStore) Save(ctx context.Context, record *MemoryRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Generate ID if not provided
	if record.ID == "" {
		hash := sha256.Sum256([]byte(record.Key + record.Scope + record.SessionID))
		record.ID = hex.EncodeToString(hash[:16])
	}

	now := time.Now()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now

	s.memories[record.ID] = record
	return nil
}

// Recall searches for similar memories using cosine similarity
func (s *InMemoryStore) Recall(ctx context.Context, query string, embedding []float64, scope, sessionID, userID string, limit int) ([]MemorySearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	results := make([]MemorySearchResult, 0)

	for _, mem := range s.memories {
		// Filter by scope
		if scope != "" && mem.Scope != scope {
			continue
		}

		// Filter by access permissions
		if mem.Scope == "session" && mem.SessionID != sessionID {
			continue
		}
		if mem.Scope == "user" && mem.UserID != userID {
			continue
		}

		// Calculate cosine similarity
		similarity := cosineSimilarity(embedding, mem.Embedding)

		results = append(results, MemorySearchResult{
			MemoryRecord: *mem,
			Similarity:   similarity,
		})
	}

	// Sort by similarity (descending)
	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})

	// Limit results
	if limit > 0 && limit < len(results) {
		results = results[:limit]
	}

	return results, nil
}

// Forget deletes memories matching the criteria
func (s *InMemoryStore) Forget(ctx context.Context, key, scope, sessionID, userID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	deleted := 0
	for id, mem := range s.memories {
		// Match key if provided
		if key != "" && mem.Key != key {
			continue
		}

		// Match scope if provided
		if scope != "" && mem.Scope != scope {
			continue
		}

		// Check permissions
		if mem.Scope == "session" && mem.SessionID != sessionID {
			continue
		}
		if mem.Scope == "user" && mem.UserID != userID {
			continue
		}

		delete(s.memories, id)
		deleted++
	}

	return deleted, nil
}

// List returns all memories matching the criteria
func (s *InMemoryStore) List(ctx context.Context, scope, sessionID, userID string, limit int) ([]MemoryRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	results := make([]MemoryRecord, 0)

	for _, mem := range s.memories {
		// Filter by scope
		if scope != "" && mem.Scope != scope {
			continue
		}

		// Filter by access permissions
		if mem.Scope == "session" && mem.SessionID != sessionID {
			continue
		}
		if mem.Scope == "user" && mem.UserID != userID {
			continue
		}

		results = append(results, *mem)
	}

	// Sort by updated_at (descending)
	sort.Slice(results, func(i, j int) bool {
		return results[i].UpdatedAt.After(results[j].UpdatedAt)
	})

	// Limit results
	if limit > 0 && limit < len(results) {
		results = results[:limit]
	}

	return results, nil
}

// Close cleans up resources
func (s *InMemoryStore) Close() error {
	s.mu.Lock()
	s.memories = make(map[string]*MemoryRecord)
	s.mu.Unlock()
	return nil
}

// cosineSimilarity calculates cosine similarity between two vectors
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}

	dotProduct := 0.0
	normA := 0.0
	normB := 0.0

	for i := 0; i < len(a); i++ {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

// MemoryRecord represents a stored memory
type MemoryRecord struct {
	ID        string
	Key       string
	Content   string
	Embedding []float64
	Scope     string
	SessionID string
	UserID    string
	Metadata  map[string]interface{}
	CreatedAt time.Time
	UpdatedAt time.Time
}
