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
	"testing"
	"time"
)

func TestSessionLock_AcquireRelease(t *testing.T) {
	lock := NewSessionLock()

	// Should acquire successfully
	if !lock.Acquire(1, "conv-1") {
		t.Error("should acquire lock for workflow 1")
	}

	// Should release successfully
	lock.Release(1)

	// Should be able to acquire again
	if !lock.Acquire(1, "conv-2") {
		t.Error("should acquire lock after release")
	}
	lock.Release(1)
}

func TestSessionLock_RejectSecondAcquire(t *testing.T) {
	lock := NewSessionLock()

	if !lock.Acquire(1, "conv-1") {
		t.Fatal("first acquire should succeed")
	}

	if lock.Acquire(1, "conv-2") {
		t.Error("second acquire should fail")
	}

	lock.Release(1)
}

func TestSessionLock_AutoExpires(t *testing.T) {
	lock := NewSessionLock()

	if !lock.Acquire(1, "conv-1") {
		t.Fatal("first acquire should succeed")
	}

	// Manually set the acquiredAt to 6 minutes ago to simulate expiry
	lock.mu.Lock()
	lock.locks[1].acquiredAt = time.Now().Add(-6 * time.Minute)
	lock.mu.Unlock()

	// Should be able to acquire after expiry (cleanup happens on acquire)
	if !lock.Acquire(2, "conv-2") {
		t.Error("should acquire lock for different workflow")
	}

	// Workflow 1 should be cleaned up
	lock.mu.Lock()
	_, exists := lock.locks[1]
	lock.mu.Unlock()

	if exists {
		t.Error("expired lock for workflow 1 should be cleaned up")
	}

	lock.Release(2)
}

func TestToolBridge_RegisterAndSubmitResult(t *testing.T) {
	lock := NewSessionLock()
	bridge := NewToolBridge(lock)

	// Register pending call
	ch := bridge.RegisterPendingCall("call-1")
	if ch == nil {
		t.Fatal("should return channel")
	}

	// Submit result
	result := ToolResult{CallID: "call-1", Status: "completed"}
	err := bridge.SubmitResult("call-1", result)
	if err != nil {
		t.Fatalf("should submit result without error: %v", err)
	}

	// Channel should have received the result
	select {
	case r := <-ch:
		if r.Status != "completed" {
			t.Errorf("expected completed status, got %s", r.Status)
		}
	default:
		t.Error("channel should have received result")
	}
}

func TestToolBridge_WaitForResult(t *testing.T) {
	lock := NewSessionLock()
	bridge := NewToolBridge(lock)

	// Start waiting in goroutine
	resultCh := make(chan ToolResult, 1)
	go func() {
		result, err := bridge.WaitForResult("call-1", 100*time.Millisecond)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		resultCh <- result
	}()

	// Submit result after short delay
	time.Sleep(10 * time.Millisecond)
	bridge.SubmitResult("call-1", ToolResult{CallID: "call-1", Status: "completed"})

	// Should receive result
	select {
	case result := <-resultCh:
		if result.Status != "completed" {
			t.Errorf("expected completed, got %s", result.Status)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("timed out waiting for result")
	}
}

func TestToolBridge_WaitForResultTimeout(t *testing.T) {
	lock := NewSessionLock()
	bridge := NewToolBridge(lock)

	result, err := bridge.WaitForResult("call-timeout", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != "failed" {
		t.Errorf("expected failed status on timeout, got %s", result.Status)
	}
	if result.Error == nil || result.Error.Code != "TIMEOUT" {
		t.Error("expected TIMEOUT error")
	}
}

func TestToolBridge_GetPendingCallCount(t *testing.T) {
	lock := NewSessionLock()
	bridge := NewToolBridge(lock)

	if bridge.GetPendingCallCount() != 0 {
		t.Error("expected 0 pending calls initially")
	}

	bridge.RegisterPendingCall("call-1")
	bridge.RegisterPendingCall("call-2")

	if bridge.GetPendingCallCount() != 2 {
		t.Errorf("expected 2 pending calls, got %d", bridge.GetPendingCallCount())
	}
}

func TestSessionLock_SecondSessionReceives409(t *testing.T) {
	lock := NewSessionLock()

	workflowID := 42
	if !lock.Acquire(workflowID, "conv-first") {
		t.Fatal("first acquire should succeed")
	}

	locked, activeConv := lock.IsLocked(workflowID)
	if !locked {
		t.Fatal("workflow should be locked")
	}
	if activeConv != "conv-first" {
		t.Errorf("expected active conversation conv-first, got %s", activeConv)
	}

	if lock.Acquire(workflowID, "conv-second") {
		t.Error("second acquire should fail")
	}

	lock.Release(workflowID)
}

func TestSessionLock_AutoExpiresAfter5Minutes(t *testing.T) {
	lock := NewSessionLock()

	if !lock.Acquire(1, "conv-1") {
		t.Fatal("first acquire should succeed")
	}

	lock.mu.Lock()
	lock.locks[1].acquiredAt = time.Now().Add(-6 * time.Minute)
	lock.mu.Unlock()

	locked, _ := lock.IsLocked(1)
	if locked {
		t.Error("lock should have expired")
	}

	if !lock.Acquire(1, "conv-2") {
		t.Error("should be able to acquire after expiry")
	}
	lock.Release(1)
}

func TestSessionLock_ReleasesOnConversationEnd(t *testing.T) {
	lock := NewSessionLock()

	if !lock.Acquire(1, "conv-1") {
		t.Fatal("first acquire should succeed")
	}

	lock.Release(1)

	locked, _ := lock.IsLocked(1)
	if locked {
		t.Error("lock should be released")
	}

	if !lock.Acquire(1, "conv-2") {
		t.Error("should be able to acquire after release")
	}
	lock.Release(1)
}
