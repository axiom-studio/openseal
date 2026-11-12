package conversation

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
)

type LoopSessionEntryLister interface {
	ListAgentSessionEntries(ctx context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error)
}
