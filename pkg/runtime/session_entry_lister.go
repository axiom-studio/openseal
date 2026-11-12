package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
)

type loopSessionEntryLister interface {
	ListAgentSessionEntries(ctx context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error)
}
