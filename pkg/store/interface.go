package store

import (
	"context"
	"time"
)

// FileStore defines the interface for storing and retrieving files.
// Implementations can be local filesystem, S3, or any other storage backend.
type FileStore interface {
	// Store saves a file and returns the file ID.
	Store(ctx context.Context, runId string, filename string, content []byte) (string, error)
	// Get retrieves a file by its ID.
	Get(ctx context.Context, fileId string) ([]byte, error)
	// Delete removes a file by its ID.
	Delete(ctx context.Context, fileId string) error
	// Cleanup removes files older than the specified TTL.
	Cleanup(ctx context.Context, ttl time.Duration) error
}
