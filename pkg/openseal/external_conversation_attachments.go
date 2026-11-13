package openseal

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
)

type ExternalConversationAttachment = models.ExternalConversationAttachment
type ExternalConversationAttachmentContent = externalconversation.AttachmentContent
type ExternalConversationAttachmentHost = externalconversation.AttachmentHost
type ExternalConversationAttachmentHostRequest = externalconversation.AttachmentHostRequest

const MaximumExternalConversationAttachmentBytes = externalconversation.MaximumExternalConversationAttachmentBytes

// MaximumExternalDeliveryAttachmentBytes bounds the files one reply sends out.
const MaximumExternalDeliveryAttachmentBytes = externalconversation.MaximumExternalDeliveryAttachmentBytes
