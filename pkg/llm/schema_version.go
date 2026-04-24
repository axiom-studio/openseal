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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
)

type SchemaVersionTracker struct {
	mu                sync.RWMutex
	conversationStart map[string]string
	currentVersion    string
	versionCache      map[string]*NodeTypeDefinition
}

func NewSchemaVersionTracker() *SchemaVersionTracker {
	return &SchemaVersionTracker{
		conversationStart: make(map[string]string),
		versionCache:      make(map[string]*NodeTypeDefinition),
	}
}

func ComputeSchemaVersion(nodeTypes map[string]*NodeTypeDefinition) string {
	if len(nodeTypes) == 0 {
		return "empty"
	}

	keys := make([]string, 0, len(nodeTypes))
	for k := range nodeTypes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, key := range keys {
		nt := nodeTypes[key]
		h.Write([]byte(key))
		h.Write([]byte(nt.Type))
		h.Write([]byte(nt.Name))
		h.Write([]byte(nt.Category))
		h.Write([]byte(nt.Description))

		for _, field := range nt.ConfigFields {
			h.Write([]byte(field.Name))
			h.Write([]byte(field.Type))
			h.Write([]byte(field.Description))
		}
	}

	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (t *SchemaVersionTracker) RecordConversationStart(conversationID string, nodeTypes map[string]*NodeTypeDefinition) {
	t.mu.Lock()
	defer t.mu.Unlock()

	version := ComputeSchemaVersion(nodeTypes)
	t.conversationStart[conversationID] = version
	t.currentVersion = version

	for nodeType, def := range nodeTypes {
		t.versionCache[nodeType] = def
	}
}

func (t *SchemaVersionTracker) CheckDrift(conversationID string, currentNodeTypes map[string]*NodeTypeDefinition) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	startVersion, exists := t.conversationStart[conversationID]
	if !exists {
		return false
	}

	currentVersion := ComputeSchemaVersion(currentNodeTypes)
	return currentVersion != startVersion
}

func (t *SchemaVersionTracker) GetDriftWarning(conversationID string, currentNodeTypes map[string]*NodeTypeDefinition) string {
	if !t.CheckDrift(conversationID, currentNodeTypes) {
		return ""
	}

	t.mu.RLock()
	startVersion := t.conversationStart[conversationID]
	t.mu.RUnlock()

	currentVersion := ComputeSchemaVersion(currentNodeTypes)

	changed := t.findChangedNodeTypes(currentNodeTypes)
	var changedList string
	if len(changed) > 0 {
		changedList = fmt.Sprintf("\nChanged node types: %v", changed)
	}

	return fmt.Sprintf(
		"⚠️ SCHEMA DRIFT DETECTED: Node schemas have changed since this conversation started.\n"+
			"Schema version at conversation start: %s\n"+
			"Current schema version: %s%s\n"+
			"Please refresh your understanding of the changed node types before making further modifications.",
		startVersion[:8],
		currentVersion[:8],
		changedList,
	)
}

func (t *SchemaVersionTracker) findChangedNodeTypes(currentNodeTypes map[string]*NodeTypeDefinition) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var changed []string
	for nodeType, currentDef := range currentNodeTypes {
		cachedDef, exists := t.versionCache[nodeType]
		if !exists {
			changed = append(changed, nodeType+" (new)")
			continue
		}
		if cachedDef.Description != currentDef.Description ||
			len(cachedDef.ConfigFields) != len(currentDef.ConfigFields) {
			changed = append(changed, nodeType)
		}
	}

	for nodeType := range t.versionCache {
		if _, exists := currentNodeTypes[nodeType]; !exists {
			changed = append(changed, nodeType+" (removed)")
		}
	}

	sort.Strings(changed)
	return changed
}

func (t *SchemaVersionTracker) RefreshSchemas(nodeTypes map[string]*NodeTypeDefinition) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	version := ComputeSchemaVersion(nodeTypes)
	t.currentVersion = version

	t.versionCache = make(map[string]*NodeTypeDefinition)
	for nodeType, def := range nodeTypes {
		t.versionCache[nodeType] = def
	}

	return version
}

func (t *SchemaVersionTracker) GetCurrentVersion() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.currentVersion
}

func (t *SchemaVersionTracker) GetConversationStartVersion(conversationID string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.conversationStart[conversationID]
}

func (t *SchemaVersionTracker) ClearConversation(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conversationStart, conversationID)
}
