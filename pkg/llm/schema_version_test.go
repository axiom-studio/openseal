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
	"strings"
	"testing"
)

func TestSchemaDrift_NoDriftWhenSchemasUnchanged(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	nodeTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
		"http":    {Type: "http", Name: "HTTP Request", Category: "action", Description: "Make HTTP requests"},
	}

	tracker.RecordConversationStart("conv-1", nodeTypes)

	drifted := tracker.CheckDrift("conv-1", nodeTypes)
	if drifted {
		t.Error("should not detect drift when schemas are unchanged")
	}
}

func TestSchemaDrift_DetectedWhenSchemasChanged(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	originalTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	tracker.RecordConversationStart("conv-1", originalTypes)

	modifiedTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "Updated webhook trigger with new features"},
	}

	drifted := tracker.CheckDrift("conv-1", modifiedTypes)
	if !drifted {
		t.Error("should detect drift when schema description changed")
	}
}

func TestSchemaDrift_DetectedWhenNewNodeAdded(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	originalTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	tracker.RecordConversationStart("conv-1", originalTypes)

	modifiedTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
		"slack":   {Type: "slack", Name: "Slack", Category: "action", Description: "Send Slack messages"},
	}

	drifted := tracker.CheckDrift("conv-1", modifiedTypes)
	if !drifted {
		t.Error("should detect drift when new node type added")
	}
}

func TestSchemaDrift_DetectedWhenNodeRemoved(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	originalTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
		"http":    {Type: "http", Name: "HTTP Request", Category: "action", Description: "Make HTTP requests"},
	}

	tracker.RecordConversationStart("conv-1", originalTypes)

	modifiedTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	drifted := tracker.CheckDrift("conv-1", modifiedTypes)
	if !drifted {
		t.Error("should detect drift when node type removed")
	}
}

func TestSchemaDrift_DetectedWhenConfigFieldsChanged(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	originalTypes := map[string]*NodeTypeDefinition{
		"webhook": {
			Type:        "webhook",
			Name:        "Webhook",
			Category:    "trigger",
			Description: "HTTP webhook trigger",
			ConfigFields: []ConfigFieldDefinition{
				{Name: "path", Type: "string", Description: "Webhook path"},
			},
		},
	}

	tracker.RecordConversationStart("conv-1", originalTypes)

	modifiedTypes := map[string]*NodeTypeDefinition{
		"webhook": {
			Type:        "webhook",
			Name:        "Webhook",
			Category:    "trigger",
			Description: "HTTP webhook trigger",
			ConfigFields: []ConfigFieldDefinition{
				{Name: "path", Type: "string", Description: "Webhook path"},
				{Name: "method", Type: "string", Description: "HTTP method"},
			},
		},
	}

	drifted := tracker.CheckDrift("conv-1", modifiedTypes)
	if !drifted {
		t.Error("should detect drift when config fields changed")
	}
}

func TestSchemaDrift_WarningReturnedOnDrift(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	originalTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	tracker.RecordConversationStart("conv-1", originalTypes)

	modifiedTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "Updated webhook"},
	}

	warning := tracker.GetDriftWarning("conv-1", modifiedTypes)
	if warning == "" {
		t.Error("should return warning message when drift detected")
	}

	if !strings.Contains(warning, "SCHEMA DRIFT DETECTED") {
		t.Error("warning should contain 'SCHEMA DRIFT DETECTED'")
	}

	if !strings.Contains(warning, "Schema version at conversation start") {
		t.Error("warning should contain conversation start version")
	}

	if !strings.Contains(warning, "Current schema version") {
		t.Error("warning should contain current schema version")
	}
}

func TestSchemaDrift_NoWarningWhenNoDrift(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	nodeTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	tracker.RecordConversationStart("conv-1", nodeTypes)

	warning := tracker.GetDriftWarning("conv-1", nodeTypes)
	if warning != "" {
		t.Errorf("should return empty warning when no drift, got: %s", warning)
	}
}

func TestSchemaDrift_RefreshSchemas(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	originalTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	tracker.RecordConversationStart("conv-1", originalTypes)
	startVersion := tracker.GetCurrentVersion()

	modifiedTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "Updated webhook"},
	}

	newVersion := tracker.RefreshSchemas(modifiedTypes)

	if newVersion == startVersion {
		t.Error("refresh should return different version after schema change")
	}

	if tracker.GetCurrentVersion() != newVersion {
		t.Error("current version should match refreshed version")
	}
}

func TestSchemaDrift_ComputeSchemaVersionDeterministic(t *testing.T) {
	nodeTypes1 := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
		"http":    {Type: "http", Name: "HTTP Request", Category: "action", Description: "Make HTTP requests"},
	}

	nodeTypes2 := map[string]*NodeTypeDefinition{
		"http":    {Type: "http", Name: "HTTP Request", Category: "action", Description: "Make HTTP requests"},
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	v1 := ComputeSchemaVersion(nodeTypes1)
	v2 := ComputeSchemaVersion(nodeTypes2)

	if v1 != v2 {
		t.Error("schema version should be deterministic regardless of map iteration order")
	}
}

func TestSchemaDrift_ComputeSchemaVersionEmpty(t *testing.T) {
	version := ComputeSchemaVersion(map[string]*NodeTypeDefinition{})
	if version != "empty" {
		t.Errorf("empty node types should return 'empty', got: %s", version)
	}
}

func TestSchemaDrift_ClearConversation(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	nodeTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	tracker.RecordConversationStart("conv-1", nodeTypes)

	if tracker.GetConversationStartVersion("conv-1") == "" {
		t.Error("conversation start version should be set")
	}

	tracker.ClearConversation("conv-1")

	if tracker.GetConversationStartVersion("conv-1") != "" {
		t.Error("conversation start version should be cleared")
	}
}

func TestSchemaDrift_NoDriftForUnknownConversation(t *testing.T) {
	tracker := NewSchemaVersionTracker()

	nodeTypes := map[string]*NodeTypeDefinition{
		"webhook": {Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	drifted := tracker.CheckDrift("unknown-conv", nodeTypes)
	if drifted {
		t.Error("should not detect drift for unknown conversation")
	}
}
