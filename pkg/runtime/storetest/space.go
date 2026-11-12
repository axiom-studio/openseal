package storetest

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

// OwnedSpace is the space runtime's SpaceService.CreateSpace builds for
// ownerID at at: only the owner has access. The owner's first space in the
// scope becomes their default when the store creates it.
func OwnedSpace(scope models.Scope, name, ownerID string, at time.Time) *models.Space {
	return &models.Space{
		ID: uuid.NewString(), Scope: scope, Name: name, Look: models.DefaultSpaceLook,
		Access: models.SpaceAccessOwner, OwnerID: ownerID, CreatedAt: at, UpdatedAt: at,
	}
}
