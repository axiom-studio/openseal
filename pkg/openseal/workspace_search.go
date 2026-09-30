package openseal

import "github.com/axiom-studio/openseal/pkg/runtime"

// CanViewChannelMessage applies the same audience rule as channel reads.
func CanViewChannelMessage(message *ChannelMessage, viewer ConversationViewer) bool {
	return runtime.CanViewChannelMessage(message, viewer)
}
