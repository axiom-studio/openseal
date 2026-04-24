package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LocalFileStore implements FileStore using local filesystem
type LocalFileStore struct {
	mu      sync.RWMutex
	baseDir string
	files   map[string]*fileMetadata
	ttl     time.Duration
}

// fileMetadata stores internal metadata about stored files
type fileMetadata struct {
	FileId    string
	RunId     string
	Filename  string
	Path      string
	Size      int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewLocalFileStore creates a new local file store
func NewLocalFileStore(baseDir string, ttl time.Duration) (*LocalFileStore, error) {
	if baseDir == "" {
		baseDir = filepath.Join(os.TempDir(), "openseal-files")
	}
	if ttl == 0 {
		ttl = 1 * time.Hour
	}

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	store := &LocalFileStore{
		baseDir: baseDir,
		files:   make(map[string]*fileMetadata),
		ttl:     ttl,
	}

	return store, nil
}

// Store saves a file and returns the file ID
func (s *LocalFileStore) Store(ctx context.Context, runId string, filename string, content []byte) (string, error) {
	fileId := generateFileId()
	filePath := filepath.Join(s.baseDir, fileId)

	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	now := time.Now()
	metadata := &fileMetadata{
		FileId:    fileId,
		RunId:     runId,
		Filename:  filename,
		Path:      filePath,
		Size:      int64(len(content)),
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}

	s.mu.Lock()
	s.files[fileId] = metadata
	s.mu.Unlock()

	return fileId, nil
}

// Get retrieves a file by its ID
func (s *LocalFileStore) Get(ctx context.Context, fileId string) ([]byte, error) {
	s.mu.RLock()
	metadata, ok := s.files[fileId]
	s.mu.RUnlock()

	if !ok {
		// Try to find by file path directly
		filePath := filepath.Join(s.baseDir, fileId)
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("file not found: %s", fileId)
		}
		return content, nil
	}

	// Check if file has expired
	if time.Now().After(metadata.ExpiresAt) {
		return nil, fmt.Errorf("file expired: %s", fileId)
	}

	content, err := os.ReadFile(metadata.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	return content, nil
}

// Delete removes a file by its ID
func (s *LocalFileStore) Delete(ctx context.Context, fileId string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	metadata, ok := s.files[fileId]
	if !ok {
		// Try direct path deletion
		filePath := filepath.Join(s.baseDir, fileId)
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := os.Remove(metadata.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	delete(s.files, fileId)
	return nil
}

// Cleanup removes files older than the specified TTL
func (s *LocalFileStore) Cleanup(ctx context.Context, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ttl > 0 {
		s.ttl = ttl
	}

	now := time.Now()
	var expired []string

	for fileId, metadata := range s.files {
		if now.After(metadata.ExpiresAt) {
			expired = append(expired, fileId)
		}
	}

	for _, fileId := range expired {
		if metadata, ok := s.files[fileId]; ok {
			os.Remove(metadata.Path)
			delete(s.files, fileId)
		}
	}

	return nil
}

// DeleteByRunId removes all files associated with a run
func (s *LocalFileStore) DeleteByRunId(runId string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var toDelete []string
	for fileId, metadata := range s.files {
		if metadata.RunId == runId {
			toDelete = append(toDelete, fileId)
		}
	}

	for _, fileId := range toDelete {
		if metadata, ok := s.files[fileId]; ok {
			os.Remove(metadata.Path)
			delete(s.files, fileId)
		}
	}

	return nil
}

// GetReader returns a reader for the file content
func (s *LocalFileStore) GetReader(fileId string) (io.ReadCloser, error) {
	s.mu.RLock()
	metadata, ok := s.files[fileId]
	s.mu.RUnlock()

	if !ok {
		filePath := filepath.Join(s.baseDir, fileId)
		return os.Open(filePath)
	}

	return os.Open(metadata.Path)
}

// StoreReader saves data from a reader and returns the file ID
func (s *LocalFileStore) StoreReader(ctx context.Context, runId string, filename string, reader io.Reader) (string, error) {
	fileId := generateFileId()
	filePath := filepath.Join(s.baseDir, fileId)

	f, err := os.Create(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer f.Close()

	size, err := io.Copy(f, reader)
	if err != nil {
		os.Remove(filePath)
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	now := time.Now()
	metadata := &fileMetadata{
		FileId:    fileId,
		RunId:     runId,
		Filename:  filename,
		Path:      filePath,
		Size:      size,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}

	s.mu.Lock()
	s.files[fileId] = metadata
	s.mu.Unlock()

	return fileId, nil
}

// StoreWithMime stores a file with mime type (for compatibility with cortex interface)
func (s *LocalFileStore) StoreWithMime(ctx context.Context, runId string, filename string, mimeType string, content []byte) (string, error) {
	return s.Store(ctx, runId, filename, content)
}

// generateFileId generates a unique file ID
func generateFileId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// Ensure LocalFileStore implements FileStore
var _ FileStore = (*LocalFileStore)(nil)
