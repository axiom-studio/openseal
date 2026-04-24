package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockStreamingExecutor implements StreamingExecutor for testing
type mockStreamingExecutor struct {
	streamingSupported bool
	streamUpdates      []StreamUpdate
	finalOutput        map[string]interface{}
}

func (m *mockStreamingExecutor) Type() string {
	return "mock-streaming"
}

func (m *mockStreamingExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	return &StepResult{
		Output: m.finalOutput,
	}, nil
}

func (m *mockStreamingExecutor) SupportsStreaming(config map[string]interface{}) bool {
	return m.streamingSupported
}

func (m *mockStreamingExecutor) ExecuteStreaming(ctx context.Context, step *StepDefinition, resolver TemplateResolver, onStream StreamCallback) (*StepResult, error) {
	// Emit all stream updates
	for _, update := range m.streamUpdates {
		if onStream != nil {
			onStream(&update)
		}
	}

	return &StepResult{
		Output: m.finalOutput,
	}, nil
}

func TestStreamUpdate_Structure(t *testing.T) {
	update := &StreamUpdate{
		Partial:  "hello",
		Progress: 0.5,
	}

	if update.Partial != "hello" {
		t.Errorf("expected Partial to be 'hello', got %v", update.Partial)
	}

	if update.Progress != 0.5 {
		t.Errorf("expected Progress to be 0.5, got %v", update.Progress)
	}
}

func TestStreamingExecutor_Interface(t *testing.T) {
	// Verify mockStreamingExecutor implements StreamingExecutor
	var _ StreamingExecutor = &mockStreamingExecutor{}

	executor := &mockStreamingExecutor{
		streamingSupported: true,
		streamUpdates: []StreamUpdate{
			{Partial: "Hello", Progress: 0.25},
			{Partial: " World", Progress: 0.5},
			{Partial: "!", Progress: 1.0},
		},
		finalOutput: map[string]interface{}{
			"response": "Hello World!",
		},
	}

	// Test SupportsStreaming
	if !executor.SupportsStreaming(nil) {
		t.Error("expected SupportsStreaming to return true")
	}

	// Test ExecuteStreaming
	var receivedUpdates []StreamUpdate
	onStream := func(update *StreamUpdate) {
		receivedUpdates = append(receivedUpdates, *update)
	}

	result, err := executor.ExecuteStreaming(context.Background(), &StepDefinition{}, nil, onStream)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if len(receivedUpdates) != 3 {
		t.Errorf("expected 3 stream updates, got %d", len(receivedUpdates))
	}

	if receivedUpdates[0].Partial != "Hello" {
		t.Errorf("expected first partial to be 'Hello', got %v", receivedUpdates[0].Partial)
	}

	if receivedUpdates[2].Progress != 1.0 {
		t.Errorf("expected final progress to be 1.0, got %v", receivedUpdates[2].Progress)
	}

	if result.Output["response"] != "Hello World!" {
		t.Errorf("expected output response to be 'Hello World!', got %v", result.Output["response"])
	}
}

func TestStreamingExecutor_Disabled(t *testing.T) {
	executor := &mockStreamingExecutor{
		streamingSupported: false,
	}

	if executor.SupportsStreaming(nil) {
		t.Error("expected SupportsStreaming to return false")
	}
}

func TestNodeUpdate_Structure(t *testing.T) {
	update := &NodeUpdate{
		NodeId:   "node-1",
		NodeName: "AI Node",
		NodeType: "ai",
		Status:   "streaming",
		Input:    map[string]interface{}{"prompt": "test"},
		Partial:  "partial output",
		Progress: 0.75,
	}

	if update.NodeId != "node-1" {
		t.Errorf("expected NodeId 'node-1', got %s", update.NodeId)
	}

	if update.Status != "streaming" {
		t.Errorf("expected Status 'streaming', got %s", update.Status)
	}

	if update.Partial != "partial output" {
		t.Errorf("expected Partial 'partial output', got %v", update.Partial)
	}

	if update.Progress != 0.75 {
		t.Errorf("expected Progress 0.75, got %v", update.Progress)
	}
}

func TestAIExecutor_SupportsStreaming(t *testing.T) {
	executor := NewAIExecutor()

	tests := []struct {
		name     string
		config   map[string]interface{}
		expected bool
	}{
		{
			name:     "default provider (openai)",
			config:   map[string]interface{}{},
			expected: true,
		},
		{
			name:     "openai provider",
			config:   map[string]interface{}{"provider": "openai"},
			expected: true,
		},
		{
			name:     "anthropic provider",
			config:   map[string]interface{}{"provider": "anthropic"},
			expected: true,
		},
		{
			name:     "openai-compatible provider",
			config:   map[string]interface{}{"provider": "openai-compatible"},
			expected: true,
		},
		{
			name:     "ollama provider (not supported)",
			config:   map[string]interface{}{"provider": "ollama"},
			expected: false,
		},
		{
			name:     "google provider (not supported)",
			config:   map[string]interface{}{"provider": "google"},
			expected: false,
		},
		{
			name:     "streaming explicitly disabled",
			config:   map[string]interface{}{"provider": "openai", "stream": false},
			expected: false,
		},
		{
			name:     "streaming explicitly enabled",
			config:   map[string]interface{}{"provider": "openai", "stream": true},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := executor.SupportsStreaming(tt.config)
			if result != tt.expected {
				t.Errorf("SupportsStreaming(%v) = %v, expected %v", tt.config, result, tt.expected)
			}
		})
	}
}

func TestAIExecutor_ImplementsStreamingExecutor(t *testing.T) {
	executor := NewAIExecutor()

	// Verify AIExecutor implements StreamingExecutor
	_, ok := interface{}(executor).(StreamingExecutor)
	if !ok {
		t.Error("AIExecutor should implement StreamingExecutor interface")
	}
}

func TestNodeUpdateFn_WithStreaming(t *testing.T) {
	var receivedUpdate *NodeUpdate

	callback := func(update *NodeUpdate) {
		receivedUpdate = update
	}

	// Simulate a streaming update
	callback(&NodeUpdate{
		NodeId:   "ai-node-1",
		NodeName: "My AI Node",
		NodeType: "ai",
		Status:   "streaming",
		Input:    map[string]interface{}{"prompt": "Hello"},
		Partial:  "Streaming token",
		Progress: 0.3,
	})

	if receivedUpdate == nil {
		t.Fatal("expected to receive an update")
	}

	if receivedUpdate.Status != "streaming" {
		t.Errorf("expected status 'streaming', got %s", receivedUpdate.Status)
	}

	if receivedUpdate.Partial != "Streaming token" {
		t.Errorf("expected partial 'Streaming token', got %v", receivedUpdate.Partial)
	}
}

func TestCodeExecutor_SupportsStreaming(t *testing.T) {
	// CodeExecutor without k8s client should not support streaming
	executorNoClient := &CodeExecutor{
		k8sClient: nil,
	}

	if executorNoClient.SupportsStreaming(nil) {
		t.Error("CodeExecutor without k8s client should not support streaming")
	}

	if executorNoClient.SupportsStreaming(map[string]interface{}{"stream": true}) {
		t.Error("CodeExecutor without k8s client should not support streaming even when enabled")
	}

	// Test streaming disabled in config
	tests := []struct {
		name     string
		config   map[string]interface{}
		expected bool
	}{
		{
			name:     "streaming explicitly disabled",
			config:   map[string]interface{}{"stream": false},
			expected: false,
		},
		{
			name:     "default config (no k8s client)",
			config:   map[string]interface{}{},
			expected: false, // No k8s client
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := executorNoClient.SupportsStreaming(tt.config)
			if result != tt.expected {
				t.Errorf("SupportsStreaming(%v) = %v, expected %v", tt.config, result, tt.expected)
			}
		})
	}
}

func TestCodeExecutor_ImplementsStreamingExecutor(t *testing.T) {
	executor := &CodeExecutor{}

	// Verify CodeExecutor implements StreamingExecutor
	_, ok := interface{}(executor).(StreamingExecutor)
	if !ok {
		t.Error("CodeExecutor should implement StreamingExecutor interface")
	}
}

func TestCodeExecutor_ParseAndEmitStreamUpdates(t *testing.T) {
	executor := &CodeExecutor{}

	tests := []struct {
		name           string
		content        string
		expectedCount  int
		expectedData   []interface{}
		expectedProgress []float64
	}{
		{
			name:           "single emit",
			content:        `---AGENT_STREAM---{"data": "hello", "progress": 0.5}`,
			expectedCount:  1,
			expectedData:   []interface{}{"hello"},
			expectedProgress: []float64{0.5},
		},
		{
			name: "multiple emits",
			content: `some log output
---AGENT_STREAM---{"data": {"step": 1}, "progress": 0.25}
more logs
---AGENT_STREAM---{"data": {"step": 2}, "progress": 0.5}
---AGENT_STREAM---{"data": {"step": 3}, "progress": 0.75}`,
			expectedCount:  3,
			expectedProgress: []float64{0.25, 0.5, 0.75},
		},
		{
			name:          "no stream markers",
			content:       "just regular log output\nno markers here",
			expectedCount: 0,
		},
		{
			name:          "invalid json after marker",
			content:       "---AGENT_STREAM---{invalid json}",
			expectedCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var updates []StreamUpdate
			onStream := func(update *StreamUpdate) {
				updates = append(updates, *update)
			}

			executor.parseAndEmitStreamUpdates(tt.content, onStream)

			if len(updates) != tt.expectedCount {
				t.Errorf("expected %d updates, got %d", tt.expectedCount, len(updates))
			}

			if tt.expectedData != nil {
				for i, expectedData := range tt.expectedData {
					if i < len(updates) && updates[i].Partial != expectedData {
						t.Errorf("update %d: expected data %v, got %v", i, expectedData, updates[i].Partial)
					}
				}
			}

			if tt.expectedProgress != nil {
				for i, expectedProgress := range tt.expectedProgress {
					if i < len(updates) && updates[i].Progress != expectedProgress {
						t.Errorf("update %d: expected progress %v, got %v", i, expectedProgress, updates[i].Progress)
					}
				}
			}
		})
	}
}

// ============================================================================
// Streaming Pipeline Tests (emit with backpressure)
// ============================================================================

func TestStreamChannel_BasicSendReceive(t *testing.T) {
	stream := NewStreamChannel(10)
	defer stream.Close()

	// Send an item
	item := StreamItem{
		Data:     map[string]interface{}{"key": "value"},
		Progress: 0.5,
		Index:    0,
	}
	stream.Send(item)

	// Receive it
	select {
	case received := <-stream.Receive():
		if received.Progress != 0.5 {
			t.Errorf("expected progress 0.5, got %v", received.Progress)
		}
		data := received.Data.(map[string]interface{})
		if data["key"] != "value" {
			t.Errorf("expected key=value, got %v", data["key"])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for item")
	}
}

func TestStreamChannel_Backpressure(t *testing.T) {
	stream := NewStreamChannel(1)
	defer stream.Close()

	var processedOrder []int
	var mu sync.Mutex

	// Set up processor that simulates slow processing
	stream.SetProcessor(context.Background(), func(ctx context.Context, data interface{}) error {
		// Simulate processing time
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		processedOrder = append(processedOrder, data.(int))
		mu.Unlock()
		return nil
	})

	// Send items with Done channels for backpressure
	var wg sync.WaitGroup
	sendOrder := []int{1, 2, 3}

	for _, val := range sendOrder {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			done := make(chan struct{})
			item := StreamItem{
				Data: v,
				Done: done,
			}
			stream.Send(item)
			// Wait for processing (backpressure)
			<-done
		}(val)
		// Small delay to ensure ordering
		time.Sleep(10 * time.Millisecond)
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	if len(processedOrder) != 3 {
		t.Errorf("expected 3 items processed, got %d", len(processedOrder))
	}

	// Items should be processed in order due to sequential sending
	for i, expected := range sendOrder {
		if i < len(processedOrder) && processedOrder[i] != expected {
			t.Errorf("item %d: expected %d, got %d", i, expected, processedOrder[i])
		}
	}
}

func TestStreamChannel_ProcessorReceivesData(t *testing.T) {
	stream := NewStreamChannel(5)
	defer stream.Close()

	var receivedData []interface{}
	var mu sync.Mutex

	stream.SetProcessor(context.Background(), func(ctx context.Context, data interface{}) error {
		mu.Lock()
		receivedData = append(receivedData, data)
		mu.Unlock()
		return nil
	})

	// Send test data
	testData := []interface{}{
		map[string]interface{}{"row": 1, "name": "Alice"},
		map[string]interface{}{"row": 2, "name": "Bob"},
		map[string]interface{}{"row": 3, "name": "Charlie"},
	}

	for i, data := range testData {
		done := make(chan struct{})
		stream.Send(StreamItem{
			Data:  data,
			Index: i,
			Done:  done,
		})
		<-done // Wait for processing
	}

	mu.Lock()
	defer mu.Unlock()

	if len(receivedData) != 3 {
		t.Fatalf("expected 3 items, got %d", len(receivedData))
	}

	// Verify data integrity
	for i, data := range receivedData {
		m := data.(map[string]interface{})
		expectedRow := i + 1
		if row, ok := m["row"].(int); ok {
			if row != expectedRow {
				t.Errorf("item %d: expected row %d, got %d", i, expectedRow, row)
			}
		}
	}
}

func TestStreamChannel_CloseStopsProcessing(t *testing.T) {
	stream := NewStreamChannel(5)

	var processedCount int32

	stream.SetProcessor(context.Background(), func(ctx context.Context, data interface{}) error {
		atomic.AddInt32(&processedCount, 1)
		return nil
	})

	// Send a few items
	for i := 0; i < 3; i++ {
		done := make(chan struct{})
		stream.Send(StreamItem{Data: i, Done: done})
		<-done
	}

	// Close the stream
	stream.Close()

	// Give goroutines time to finish
	time.Sleep(50 * time.Millisecond)

	count := atomic.LoadInt32(&processedCount)
	if count != 3 {
		t.Errorf("expected 3 items processed before close, got %d", count)
	}
}

func TestStreamRegistry_GetOrCreate(t *testing.T) {
	registry := NewStreamRegistry()

	// First call creates new stream
	stream1 := registry.GetOrCreate(1, "node-a", 5)
	if stream1 == nil {
		t.Fatal("expected stream to be created")
	}

	// Second call returns same stream
	stream2 := registry.GetOrCreate(1, "node-a", 5)
	if stream1 != stream2 {
		t.Error("expected same stream instance")
	}

	// Different key creates different stream
	stream3 := registry.GetOrCreate(1, "node-b", 5)
	if stream1 == stream3 {
		t.Error("expected different stream for different node")
	}

	stream4 := registry.GetOrCreate(2, "node-a", 5)
	if stream1 == stream4 {
		t.Error("expected different stream for different run")
	}
}

func TestStreamRegistry_Get(t *testing.T) {
	registry := NewStreamRegistry()

	// Get non-existent stream
	stream := registry.Get(999, "nonexistent")
	if stream != nil {
		t.Error("expected nil for non-existent stream")
	}

	// Create and then get
	created := registry.GetOrCreate(1, "node-1", 5)
	retrieved := registry.Get(1, "node-1")
	if created != retrieved {
		t.Error("Get should return the same stream that was created")
	}
}

func TestStreamRegistry_Remove(t *testing.T) {
	registry := NewStreamRegistry()

	// Create a stream
	stream := registry.GetOrCreate(1, "node-1", 5)
	if stream == nil {
		t.Fatal("expected stream to be created")
	}

	// Remove it
	registry.Remove(1, "node-1")

	// Should be gone
	retrieved := registry.Get(1, "node-1")
	if retrieved != nil {
		t.Error("stream should be removed")
	}

	// Creating again should give new instance
	newStream := registry.GetOrCreate(1, "node-1", 5)
	if newStream == stream {
		t.Error("expected new stream instance after remove")
	}
}

func TestStreamRegistry_CleanupRun(t *testing.T) {
	registry := NewStreamRegistry()

	// Create multiple streams for same run
	registry.GetOrCreate(1, "node-a", 5)
	registry.GetOrCreate(1, "node-b", 5)
	registry.GetOrCreate(1, "node-c", 5)
	registry.GetOrCreate(2, "node-a", 5) // Different run

	// Remove all for run 1
	registry.CleanupRun(1)

	// Run 1 streams should be gone
	if registry.Get(1, "node-a") != nil {
		t.Error("node-a should be removed")
	}
	if registry.Get(1, "node-b") != nil {
		t.Error("node-b should be removed")
	}
	if registry.Get(1, "node-c") != nil {
		t.Error("node-c should be removed")
	}

	// Run 2 stream should still exist
	if registry.Get(2, "node-a") == nil {
		t.Error("run 2 stream should still exist")
	}
}

func TestStreamProducer_CodeExecutor(t *testing.T) {
	executor := &CodeExecutor{}

	// Verify CodeExecutor implements StreamProducer
	_, ok := interface{}(executor).(StreamProducer)
	if !ok {
		t.Error("CodeExecutor should implement StreamProducer interface")
	}

	// Test SupportsStreamingOutput
	if !executor.SupportsStreamingOutput(nil) {
		t.Error("CodeExecutor should support streaming output by default")
	}

	if !executor.SupportsStreamingOutput(map[string]interface{}{}) {
		t.Error("CodeExecutor should support streaming output with empty config")
	}
}

func TestGlobalStreamRegistry(t *testing.T) {
	registry := GetGlobalStreamRegistry()
	if registry == nil {
		t.Fatal("global stream registry should not be nil")
	}

	// Should return same instance
	registry2 := GetGlobalStreamRegistry()
	if registry != registry2 {
		t.Error("should return same global registry instance")
	}
}
