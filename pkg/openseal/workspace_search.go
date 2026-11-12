package openseal

import (
	"github.com/axiom-studio/openseal/pkg/models"
)

// CanViewChannelMessage applies the same audience rule as channel reads.
func CanViewChannelMessage(message *ChannelMessage, viewer ConversationViewer) bool {
	return models.CanViewChannelMessage(message, viewer)
}
