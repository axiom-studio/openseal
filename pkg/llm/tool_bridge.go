/*
 * Copyright (c) 2025. Axiom Studio
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package llm

import (
	"fmt"
	"sync"
	"time"
)

// SessionLock manages concurrent AI sessions per workflow
type SessionLock struct {
	mu    sync.Mutex
	locks map[int]*sessionInfo
}

type sessionInfo struct {
	conversationID string
	acquiredAt     time.Time
}

const sessionLockTimeout = 5 * time.Minute

// NewSessionLock creates a new session lock manager
func NewSessionLock() *SessionLock {
	return &SessionLock{
		locks: make(map[int]*sessionInfo),
	}
}

// Acquire tries to get a lock for a workflow. Returns false if already locked.
func (sl *SessionLock) Acquire(workflowID int, conversationID string) bool {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	// Clean up expired locks
	sl.cleanup()

	if info, exists := sl.locks[workflowID]; exists {
		// Already locked
		_ = info
		return false
	}

	sl.locks[workflowID] = &sessionInfo{
		conversationID: conversationID,
		acquiredAt:     time.Now(),
	}
	return true
}

// IsLocked checks if a workflow is currently locked.
// Returns true and the conversationID if locked, false otherwise.
func (sl *SessionLock) IsLocked(workflowID int) (bool, string) {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	sl.cleanup()

	if info, exists := sl.locks[workflowID]; exists {
		return true, info.conversationID
	}
	return false, ""
}

// TryAcquire atomically checks if a workflow is locked and acquires the lock if not.
// Returns (success, activeConversationID). If success is true, the lock was acquired.
// If success is false and activeConversationID is non-empty, another conversation holds the lock.
func (sl *SessionLock) TryAcquire(workflowID int, conversationID string) (bool, string) {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	sl.cleanup()

	if info, exists := sl.locks[workflowID]; exists {
		if conversationID != "" && conversationID == info.conversationID {
			return true, ""
		}
		return false, info.conversationID
	}

	sl.locks[workflowID] = &sessionInfo{
		conversationID: conversationID,
		acquiredAt:     time.Now(),
	}
	return true, ""
}

// Release releases a lock for a workflow
func (sl *SessionLock) Release(workflowID int) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	delete(sl.locks, workflowID)
}

func (sl *SessionLock) cleanup() {
	now := time.Now()
	for workflowID, info := range sl.locks {
		if now.Sub(info.acquiredAt) > sessionLockTimeout {
			delete(sl.locks, workflowID)
		}
	}
}

// ToolBridge manages tool call routing between backend and frontend
type ToolBridge struct {
	sessionLock  *SessionLock
	pendingCalls map[string]chan ToolResult
	mu           sync.RWMutex
}

// NewToolBridge creates a new tool bridge
func NewToolBridge(sessionLock *SessionLock) *ToolBridge {
	return &ToolBridge{
		sessionLock:  sessionLock,
		pendingCalls: make(map[string]chan ToolResult),
	}
}

// RegisterPendingCall registers a tool call waiting for frontend result
func (tb *ToolBridge) RegisterPendingCall(callID string) chan ToolResult {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	ch := make(chan ToolResult, 1)
	tb.pendingCalls[callID] = ch
	return ch
}

// SubmitResult submits a tool result from the frontend
func (tb *ToolBridge) SubmitResult(callID string, result ToolResult) error {
	tb.mu.RLock()
	ch, exists := tb.pendingCalls[callID]
	tb.mu.RUnlock()

	if !exists {
		return fmt.Errorf("no pending call found for call_id: %s", callID)
	}

	select {
	case ch <- result:
		// Clean up
		tb.mu.Lock()
		delete(tb.pendingCalls, callID)
		tb.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("result channel is full")
	}
}

// WaitForResult waits for a tool result with timeout
func (tb *ToolBridge) WaitForResult(callID string, timeout time.Duration) (ToolResult, error) {
	ch := tb.RegisterPendingCall(callID)

	select {
	case result := <-ch:
		return result, nil
	case <-time.After(timeout):
		tb.mu.Lock()
		delete(tb.pendingCalls, callID)
		tb.mu.Unlock()
		return ToolResult{
			CallID: callID,
			Status: "failed",
			Error: &ToolError{
				Code:    "TIMEOUT",
				Message: fmt.Sprintf("Tool result not received within %v", timeout),
			},
		}, nil
	}
}

func (tb *ToolBridge) CleanupPendingCall(callID string) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	delete(tb.pendingCalls, callID)
}

// GetPendingCallCount returns number of pending tool calls
func (tb *ToolBridge) GetPendingCallCount() int {
	tb.mu.RLock()
	defer tb.mu.RUnlock()
	return len(tb.pendingCalls)
}

// GetSessionLock returns the session lock manager
func (tb *ToolBridge) GetSessionLock() *SessionLock {
	return tb.sessionLock
}
