package modelstep

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Test-only access to model step internals for the external model step
// tests, which use fixtures shared with tests of other packages
// (modelsteptest) and so cannot be in-package tests.

const (
	RenderOpenToolText              = renderOpenToolText
	RenderMissingToolText           = renderMissingToolText
	CompactionSummaryPreamble       = compactionSummaryPreamble
	HostToolUnusableResultMessage   = hostToolUnusableResultMessage
	HostToolUnavailableMessage      = hostToolUnavailableMessage
	HostToolCouldNotRunMessage      = hostToolCouldNotRunMessage
	CapabilityLookupUnavailableText = capabilityLookupUnavailableText
	CapabilityLoadedNext            = capabilityLoadedNext
	CapabilityNotLoadedReason       = capabilityNotLoadedReason
	CapabilitySearchDefaultLimit    = capabilitySearchDefaultLimit
	CapabilityInvocationToolCallID  = capabilityInvocationToolCallID
)

func RenderImageUnavailableText(mediaType string) string {
	return renderImageUnavailableText(mediaType)
}

func CompactionSummaryRequest(step models.ModelStepRequest) models.ModelStepRequest {
	return compactionSummaryRequest(step)
}

func CompactionChunkSummaryRequest(step models.ModelStepRequest, entries []models.SessionEntryView) models.ModelStepRequest {
	return compactionChunkSummaryRequest(step, entries)
}

func CarriedToolsAfter(previous *models.CompactionEntry, summarized []*models.SessionEntry) []models.SurfacedTool {
	return carriedToolsAfter(previous, summarized)
}

func (s *CapabilitySet) DiscoveryID(id string) string { return s.discoveryID(id) }

func (s *CapabilitySet) Closure(id string) []string { return s.closure(id) }

func (s *CapabilitySet) ToolCost(id string) int { return s.toolCost(id) }

func (s *CapabilitySet) DefinitionCost(id string) int { return s.definitionCost(id) }

// CapabilityGroupCount is the number of groups the directory can list.
func (s *CapabilitySet) CapabilityGroupCount() int { return len(s.capabilityGroups()) }

// SetSleep replaces the dispatcher's retry wait.
func (d *HostToolDispatcher) SetSleep(sleep func(context.Context, time.Duration) error) {
	d.sleep = sleep
}
