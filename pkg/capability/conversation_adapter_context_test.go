package capability

import "testing"

func TestConversationAdapterContextHistoryIsAnOptionalDeclaredFeature(t *testing.T) {
	features, err := normalizeConversationFeatures([]ConversationAdapterFeature{
		ConversationFeatureThreads, ConversationFeatureContextHistory, ConversationFeatureContextHistory,
	})
	if err != nil || len(features) != 2 || features[0] != ConversationFeatureContextHistory {
		t.Fatalf("history feature = %#v, %v", features, err)
	}
}
