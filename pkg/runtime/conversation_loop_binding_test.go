package runtime

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestConversationLoopBindingSetsSeams(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("trigger", "user-1", "Hi")
		run := f.startRun(trigger)
		binding := f.bind(run, &chatLoopProducer{})
		if binding.Session == nil || binding.Session.Revision == 0 || binding.Session.Kind != AgentSessionKindConversation {
			t.Fatalf("lane session not opened: %#v", binding.Session)
		}
		if _, ok := binding.Final.(conversationObservedFinal); !ok || binding.ToolCheck == nil || binding.AfterTool == nil || len(binding.Preparers) != 2 {
			t.Fatalf("seams = final %T check %v after %v preparers %d", binding.Final, binding.ToolCheck != nil, binding.AfterTool != nil, len(binding.Preparers))
		}
		if reflect.TypeOf(binding.Preparers[0]) != reflect.TypeOf(newRunStartPreparer(nil, nil)) {
			t.Fatalf("the run-start preparer must stay first: %T", binding.Preparers[0])
		}
		authority := binding.Authority
		if !authority.CanReact || !authority.CanAskUser || authority.ExternalChannelReply || authority.finalMode() != FinalModeDefault {
			t.Fatalf("authority = %#v", authority)
		}
		want := conversationSystemSections(f.conversation, run, false)
		if len(binding.Prompt.Sections) != len(want) || binding.Prompt.Sections[0].ID != want[0].ID {
			t.Fatalf("prompt sections = %#v", binding.Prompt.Sections)
		}
		// A second claim binds the same session and byte-identical sections.
		again := f.bind(run, &chatLoopProducer{})
		a, _ := json.Marshal(binding.Prompt.Sections)
		b, _ := json.Marshal(again.Prompt.Sections)
		if again.Session.ID != binding.Session.ID || string(a) != string(b) {
			t.Fatal("rebinding changed the session or the system sections")
		}
		// apply only narrows: an authority that offers no ask or react tool
		// (Deterministic, decision, review) does not gain one, and a decision
		// or review authority gets no chat flags.
		chat := ConversationLoopAuthority{CanAskConversationQuestion: true, CanReact: true, ExternalChannelReply: true, ApprovalMode: ConversationApprovalAuto}
		if got := chat.apply(ControlAuthority{AgentID: "agent-1"}); got.CanAskUser || got.CanReact || !got.ExternalChannelReply || got.ApprovalMode != ConversationApprovalAuto {
			t.Fatalf("deterministic authority = %#v", got)
		}
		if got := chat.apply(ControlAuthority{AgentID: "agent-1", FinalMode: FinalModeDecision}); got.CanAskUser || got.CanReact || got.ExternalChannelReply || got.ApprovalMode != "" {
			t.Fatalf("decision authority = %#v", got)
		}
		if got := (ConversationLoopAuthority{}).apply(ControlAuthority{CanAskUser: true, CanReact: true}); got.CanAskUser || got.CanReact {
			t.Fatalf("a chat without the ask right kept it: %#v", got)
		}
	})
}
