package executor

import (
	"context"
	"fmt"
	"sync"
)

// StreamItem represents a single item in a streaming pipeline
type StreamItem struct {
	Data     interface{}   `json:"data"`
	Progress float64       `json:"progress,omitempty"`
	Index    int           `json:"index"`
	Done     chan struct{} `json:"-"` // Signals when item has been processed (for backpressure)
	Error    error         `json:"-"` // Error from processing (if any)
}

// StreamItemProcessor is called to process each streamed item
// It should execute the downstream node(s) with the item data as input
type StreamItemProcessor func(ctx context.Context, data interface{}) error

// StreamChannel is a channel for inter-node streaming data flow
type StreamChannel struct {
	items     chan StreamItem
	done      chan struct{}
	closed    bool
	mu        sync.Mutex
	itemsSent int
	processor StreamItemProcessor // Called to process each item
	ctx       context.Context     // Context for processing
}

// NewStreamChannel creates a new stream channel with the given buffer size
// bufferSize of 0 means unbuffered (synchronous/blocking)
// bufferSize > 0 allows that many items to queue before blocking
func NewStreamChannel(bufferSize int) *StreamChannel {
	return &StreamChannel{
		items: make(chan StreamItem, bufferSize),
		done:  make(chan struct{}),
	}
}

// SetProcessor sets the processor function and starts processing items
// The processor is called for each item, and the item's Done channel is closed when complete
func (s *StreamChannel) SetProcessor(ctx context.Context, processor StreamItemProcessor) {
	s.mu.Lock()
	s.processor = processor
	s.ctx = ctx
	s.mu.Unlock()

	// Start processing goroutine
	go s.processItems()
}

// processItems reads items from the channel and processes them
func (s *StreamChannel) processItems() {
	for item := range s.items {
		if s.processor != nil && s.ctx != nil {
			// Process the item (execute downstream nodes)
			err := s.processor(s.ctx, item.Data)
			item.Error = err
		}
		// Signal that processing is complete
		if item.Done != nil {
			close(item.Done)
		}
	}
}

// Send sends an item to the stream. Blocks if buffer is full (backpressure).
// Returns false if the stream is closed.
func (s *StreamChannel) Send(item StreamItem) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.itemsSent++
	item.Index = s.itemsSent
	s.mu.Unlock()

	select {
	case s.items <- item:
		return true
	case <-s.done:
		return false
	}
}

// Receive returns a receive-only channel for consuming items
func (s *StreamChannel) Receive() <-chan StreamItem {
	return s.items
}

// Close closes the stream, signaling no more items will be sent
func (s *StreamChannel) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.items)
		close(s.done)
	}
}

// IsClosed returns true if the stream has been closed
func (s *StreamChannel) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// ItemsSent returns the number of items sent through this stream
func (s *StreamChannel) ItemsSent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.itemsSent
}

// StreamRegistry manages active streams for running pipelines
type StreamRegistry struct {
	streams map[string]*StreamChannel // key: "runId:sourceNodeId"
	mu      sync.RWMutex
}

// NewStreamRegistry creates a new stream registry
func NewStreamRegistry() *StreamRegistry {
	return &StreamRegistry{
		streams: make(map[string]*StreamChannel),
	}
}

// GetOrCreate gets an existing stream or creates a new one
func (r *StreamRegistry) GetOrCreate(runId int, nodeId string, bufferSize int) *StreamChannel {
	key := streamKey(runId, nodeId)
	r.mu.Lock()
	defer r.mu.Unlock()

	if stream, exists := r.streams[key]; exists {
		return stream
	}

	stream := NewStreamChannel(bufferSize)
	r.streams[key] = stream
	return stream
}

// Get gets an existing stream, returns nil if not found
func (r *StreamRegistry) Get(runId int, nodeId string) *StreamChannel {
	key := streamKey(runId, nodeId)
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.streams[key]
}

// Remove removes a stream from the registry
func (r *StreamRegistry) Remove(runId int, nodeId string) {
	key := streamKey(runId, nodeId)
	r.mu.Lock()
	defer r.mu.Unlock()
	if stream, exists := r.streams[key]; exists {
		stream.Close()
		delete(r.streams, key)
	}
}

// CleanupRun removes all streams for a given run
func (r *StreamRegistry) CleanupRun(runId int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := streamKeyPrefix(runId)
	for key, stream := range r.streams {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			stream.Close()
			delete(r.streams, key)
		}
	}
}

func streamKey(runId int, nodeId string) string {
	return fmt.Sprintf("%d:%s", runId, nodeId)
}

func streamKeyPrefix(runId int) string {
	return fmt.Sprintf("%d:", runId)
}

// Global stream registry singleton
var globalStreamRegistry = NewStreamRegistry()

// GetGlobalStreamRegistry returns the global stream registry
func GetGlobalStreamRegistry() *StreamRegistry {
	return globalStreamRegistry
}

// StreamProducer is implemented by executors that can produce streaming output
type StreamProducer interface {
	// SupportsStreamingOutput returns true if this executor can stream output
	SupportsStreamingOutput(config map[string]interface{}) bool
}
