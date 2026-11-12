package runtime

import (
	"context"
)

// markRunFiles records which of a Run's saved files its reply handed over and
// which it did not. It is a no-op for a store that keeps no such record.
func markRunFiles(ctx context.Context, store ArtifactStore, scope Scope, saved []savedArtifact, handedOver []ConversationReference) error {
	marker, ok := store.(ArtifactWorkingFileStore)
	if !ok || len(saved) == 0 {
		return nil
	}
	handed := make(map[string]bool, len(handedOver))
	for _, ref := range handedOver {
		if ref.Kind == ConversationReferenceArtifact {
			handed[ref.ID] = true
		}
	}
	seen := make(map[string]bool, len(saved))
	var working, results []string
	for _, file := range saved {
		id := file.Ref.ID
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if handed[id] {
			results = append(results, id)
		} else {
			working = append(working, id)
		}
	}
	return marker.MarkArtifactFiles(ctx, scope, working, results)
}
