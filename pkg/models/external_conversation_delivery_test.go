package models

import (
	"testing"
)

func TestExternalConversationFileDescriptorsRejectDuplicatesAndInvalidMetadata(t *testing.T) {
	for _, files := range [][]ExternalConversationAttachment{{{ID: "F1", Name: "ok"}, {ID: "F1", Name: "duplicate"}}, {{ID: "F1", Name: "bad\nname"}}, {{ID: "F1", Name: "ok", SizeBytes: -1}}, {{ID: "F1", Name: "ok", MediaType: "bad mime"}}} {
		if ValidateExternalAttachments(files) == nil {
			t.Fatalf("accepted invalid descriptors %#v", files)
		}
	}
}
