package runtime

import (
	"testing"
)

func TestActiveConversationRunsIncludeOnlyExactIndependentTaskThreadAndActor(t *testing.T) {
	store := newTestKernelStore(t)
	source, turn, conversation, trigger := settledTaskSourceFixture(t, store)
	if _, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(t, store, source, turn)); err != nil {
		t.Fatal(err)
	}
	runner := &ConversationRunTurnRunner{portfolio: store}
	active, err := runner.activeConversationRuns(t.Context(), conversation, trigger)
	if err != nil || len(active) != 1 || active[0].Revision < 1 || !slicesContainsRunCommand(active[0].AvailableControls, RunActionCancel) {
		t.Fatalf("task controls absent from authoritative thread snapshot: %#v %v", active, err)
	}
	// Provider channels scope controls to their exact thread; ordinary app
	// conversations retain task visibility for later messages in the same chat.
	conversation.Origin = &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "external-channel"}
	for _, change := range []func(*ChannelMessage){
		func(message *ChannelMessage) { message.Sender.ID = "other-user" },
		func(message *ChannelMessage) { message.ThreadRootID = "other-thread" },
	} {
		other := cloneChannelMessage(trigger)
		change(other)
		active, err := runner.activeConversationRuns(t.Context(), conversation, other)
		if err != nil || len(active) != 0 {
			t.Fatalf("task leaked across actor or thread: %#v %v", active, err)
		}
	}
}
