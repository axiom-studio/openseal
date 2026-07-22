// Package store provides bounded, short-lived transport for execution inputs.
// Durable user-visible output belongs in the artifact package.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultTTL      = time.Hour
	defaultMaxBytes = int64(100 << 20)
)

var (
	ErrNotFound = errors.New("temporary execution file not found")
	ErrExpired  = errors.New("temporary execution file expired")
	ErrTooLarge = errors.New("temporary execution file exceeds size limit")
)

// Config controls a local temporary execution store. Root is private to the
// process and must not be served as a general filesystem namespace.
type Config struct {
	Root     string
	TTL      time.Duration
	MaxBytes int64
}

// StoredFile is safe transport metadata. Path information is never exposed.
type StoredFile struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	MimeType  string    `json:"mimeType"`
	Size      int64     `json:"size"`
	RunID     string    `json:"runId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// FileStore is the portable contract for short-lived execution handoff.
// Implementations must treat IDs as opaque, enforce expiry, and bound writes.
type FileStore interface {
	Store(context.Context, []byte, string, string) (*StoredFile, error)
	StoreReader(context.Context, io.Reader, string, string) (*StoredFile, error)
	Get(context.Context, string) (*StoredFile, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
	DeleteByRunID(context.Context, string) error
	AssociateWithRun(context.Context, string, string) error
	Cleanup(context.Context) (int, error)
}

type fileRecord struct {
	metadata StoredFile
	path     string
}

// LocalFileStore stores opaque execution inputs under a private directory.
// Its index is intentionally process-local: callers needing durable output
// must publish an Artifact instead.
type LocalFileStore struct {
	mu       sync.RWMutex
	root     string
	ttl      time.Duration
	maxBytes int64
	files    map[string]fileRecord
	now      func() time.Time
	newID    func() (string, error)
}

var _ FileStore = (*LocalFileStore)(nil)

func NewLocalFileStore(config Config) (*LocalFileStore, error) {
	root := strings.TrimSpace(config.Root)
	if root == "" {
		root = filepath.Join(os.TempDir(), "openseal-execution-files")
	}
	ttl := config.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	maxBytes := config.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create temporary execution store: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure temporary execution store: %w", err)
	}
	return &LocalFileStore{
		root: root, ttl: ttl, maxBytes: maxBytes, files: make(map[string]fileRecord),
		now: time.Now, newID: generateFileID,
	}, nil
}

func (s *LocalFileStore) Store(ctx context.Context, data []byte, filename, mimeType string) (*StoredFile, error) {
	if int64(len(data)) > s.maxBytes {
		return nil, ErrTooLarge
	}
	return s.StoreReader(ctx, bytes.NewReader(data), filename, mimeType)
}

func (s *LocalFileStore) StoreReader(ctx context.Context, reader io.Reader, filename, mimeType string) (*StoredFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := s.newID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(s.root, id)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create temporary execution file: %w", err)
	}
	limited := &io.LimitedReader{R: reader, N: s.maxBytes + 1}
	size, copyErr := io.Copy(file, &contextReader{ctx: ctx, reader: limited})
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || size > s.maxBytes {
		_ = os.Remove(path)
		switch {
		case copyErr != nil:
			return nil, fmt.Errorf("write temporary execution file: %w", copyErr)
		case closeErr != nil:
			return nil, fmt.Errorf("close temporary execution file: %w", closeErr)
		default:
			return nil, ErrTooLarge
		}
	}
	now := s.now().UTC()
	metadata := StoredFile{
		ID: id, Filename: safeFilename(filename), MimeType: safeMimeType(mimeType), Size: size,
		CreatedAt: now, ExpiresAt: now.Add(s.ttl),
	}
	s.mu.Lock()
	s.files[id] = fileRecord{metadata: metadata, path: path}
	s.mu.Unlock()
	return copyMetadata(metadata), nil
}

func (s *LocalFileStore) Get(ctx context.Context, id string) (*StoredFile, error) {
	record, err := s.record(ctx, id)
	if err != nil {
		return nil, err
	}
	return copyMetadata(record.metadata), nil
}

func (s *LocalFileStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	record, err := s.record(ctx, id)
	if err != nil {
		return nil, err
	}
	reader, err := os.Open(record.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open temporary execution file: %w", err)
	}
	return reader, nil
}

func (s *LocalFileStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.files[id]
	if !ok {
		return nil
	}
	if err := remove(record.path); err != nil {
		return err
	}
	delete(s.files, id)
	return nil
}

func (s *LocalFileStore) DeleteByRunID(ctx context.Context, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, record := range s.files {
		if record.metadata.RunID != runID {
			continue
		}
		if err := remove(record.path); err != nil {
			return err
		}
		delete(s.files, id)
	}
	return nil
}

func (s *LocalFileStore) AssociateWithRun(ctx context.Context, id, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.files[id]
	if !ok {
		return ErrNotFound
	}
	if !s.now().Before(record.metadata.ExpiresAt) {
		_ = remove(record.path)
		delete(s.files, id)
		return ErrExpired
	}
	record.metadata.RunID = runID
	s.files[id] = record
	return nil
}

// Cleanup synchronously removes expired entries and returns the count. Hosts
// choose their own lifecycle and may call it periodically without hidden
// background goroutines surviving shutdown.
func (s *LocalFileStore) Cleanup(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	removed := 0
	for id, record := range s.files {
		if now.Before(record.metadata.ExpiresAt) {
			continue
		}
		if err := remove(record.path); err != nil {
			return removed, err
		}
		delete(s.files, id)
		removed++
	}
	return removed, nil
}

func (s *LocalFileStore) record(ctx context.Context, id string) (fileRecord, error) {
	if err := ctx.Err(); err != nil {
		return fileRecord{}, err
	}
	if len(id) != 32 {
		return fileRecord{}, ErrNotFound
	}
	s.mu.RLock()
	record, ok := s.files[id]
	s.mu.RUnlock()
	if !ok {
		return fileRecord{}, ErrNotFound
	}
	if !s.now().Before(record.metadata.ExpiresAt) {
		_ = s.Delete(context.Background(), id)
		return fileRecord{}, ErrExpired
	}
	return record, nil
}

func generateFileID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate temporary execution file identity: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func safeFilename(filename string) string {
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." {
		return "file"
	}
	return filename
}

func safeMimeType(mimeType string) string {
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" || strings.ContainsAny(mimeType, "\r\n") {
		return "application/octet-stream"
	}
	return mimeType
}

func remove(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete temporary execution file: %w", err)
	}
	return nil
}

func copyMetadata(metadata StoredFile) *StoredFile {
	copy := metadata
	return &copy
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
