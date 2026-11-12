package runtime

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAskUserOptionsDecodeLimits(t *testing.T) {
	option := func(label string) string { return `{"label":"` + label + `"}` }
	options := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = option(string(rune('A' + i)))
		}
		return `[` + strings.Join(parts, ",") + `]`
	}
	for name, raw := range map[string]string{
		"one option":                    `{"question":"Which?","options":` + options(1) + `}`,
		"seven options":                 `{"question":"Which?","options":` + options(7) + `}`,
		"empty label":                   `{"question":"Which?","options":[{"label":"  "},{"label":"B"}]}`,
		"long label":                    `{"question":"Which?","options":[{"label":"` + strings.Repeat("a", 81) + `"},{"label":"B"}]}`,
		"long description":              `{"question":"Which?","options":[{"label":"A","description":"` + strings.Repeat("d", 241) + `"},{"label":"B"}]}`,
		"duplicate labels after trim":   `{"question":"Which?","options":[{"label":"A"},{"label":" A "}]}`,
		"unknown option field":          `{"question":"Which?","options":[{"label":"A","id":"x"},{"label":"B"}]}`,
		"allowMultiple without options": `{"question":"Which?","allowMultiple":true}`,
		"allowOther without options":    `{"question":"Which?","allowOther":true}`,
	} {
		if _, err := (askUserHandler{}).decode(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: decode accepted it", name)
		}
	}
	value, err := (askUserHandler{}).decode(json.RawMessage(`{"question":"Which?","options":[{"label":" A ","description":" first "},{"label":"` +
		strings.Repeat("b", 80) + `"}],"allowMultiple":true,"allowOther":true}`))
	if err != nil {
		t.Fatal(err)
	}
	args := value.(*askUserArgs)
	want := &ChannelQuestion{AllowMultiple: true, AllowOther: true, Options: []ChannelQuestionOption{
		{ID: "o1", Label: "A", Description: "first"}, {ID: "o2", Label: strings.Repeat("b", 80)},
	}}
	if got := args.ChannelQuestion(); !reflect.DeepEqual(got, want) {
		t.Fatalf("question = %#v", got)
	}
	if plain, err := (askUserHandler{}).decode(json.RawMessage(`{"question":"Which?","options":[]}`)); err != nil ||
		plain.(*askUserArgs).ChannelQuestion() != nil {
		t.Fatalf("empty options = %#v %v", plain, err)
	}
	spec := (askUserHandler{}).spec(ControlAuthority{})
	properties := spec.InputSchema["properties"].(map[string]interface{})
	for _, key := range []string{"options", "allowMultiple", "allowOther"} {
		if properties[key] == nil {
			t.Fatalf("schema lacks %s", key)
		}
	}
	if !strings.Contains(spec.Description, "Offer options when the answer is a choice.") {
		t.Fatalf("description = %q", spec.Description)
	}
}

const askUserOptionsArguments = `{"question":"` + askUserTestQuestion + `","allowMultiple":true,` +
	`"options":[{"label":"Pricing"},{"label":"Docs","description":"The developer docs"},{"label":"Blog"}]}`

func TestAskUserOptionsPostedAndAnsweredWithSelectedOptions(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := claimedForegroundRun(t, store)
		call := newAskUserCallWithArguments(t, store, f.run, "foreground-worker", json.RawMessage(askUserOptionsArguments))
		result := call.settle(t)
		if result.Kind != ControlParked {
			t.Fatalf("settle = %#v", result)
		}
		f.run = call.park(t, result.Park)
		// The reconciler replays the same request, options included.
		question := f.question(t)
		want := &ChannelQuestion{AllowMultiple: true, Options: []ChannelQuestionOption{
			{ID: "o1", Label: "Pricing"}, {ID: "o2", Label: "Docs", Description: "The developer docs"}, {ID: "o3", Label: "Blog"},
		}}
		if !reflect.DeepEqual(question.Question, want) || question.Content != askUserTestQuestion {
			t.Fatalf("question = %#v", question.Question)
		}

		answer := f.post(t, PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentAnswer,
			Content: "Blog, Pricing", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o3", "o1"}, IdempotencyKey: "answer"})
		if !reflect.DeepEqual(answer.SelectedOptionIDs, []string{"o3", "o1"}) {
			t.Fatalf("answer = %#v", answer)
		}
		resumed, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID)
		if err != nil || resumed == nil || resumed.Run.ID != f.run.ID {
			t.Fatalf("answer did not wake the Run: %#v %v", resumed, err)
		}
		receipt := resumed.Run.PendingInterventions[len(resumed.Run.PendingInterventions)-1].ConversationAnswer
		if receipt == nil || !reflect.DeepEqual(receipt.SelectedOptionIDs, []string{"o3", "o1"}) {
			t.Fatalf("receipt = %#v", receipt)
		}
		resume, _ := call.resume(t)
		if !resume.Resolved || call.entry.State != ToolEntryCompleted {
			t.Fatalf("resume = %#v", resume)
		}
		value := askUserResultValue(t, call.entry)
		if value["status"] != "answered" || value["answer"] != "Blog, Pricing" || value["messageId"] != answer.ID ||
			!reflect.DeepEqual(value["selectedOptions"], []interface{}{"Pricing", "Blog"}) {
			t.Fatalf("result = %#v", value)
		}
	})
}

func TestAskUserWithoutOptionsResultHasNoSelectedOptions(t *testing.T) {
	store := newTestKernelStore(t)
	f := claimedForegroundRun(t, store)
	parked, call := askUserParked(t, store, f.run, "foreground-worker", askUserTestQuestion)
	f.run = parked
	question := f.question(t)
	if question.Question != nil {
		t.Fatalf("free-text question carries options: %#v", question.Question)
	}
	answer := f.post(t, PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentAnswer,
		Content: "The pricing page.", ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
	if _, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID); err != nil {
		t.Fatal(err)
	}
	call.resume(t)
	if value := askUserResultValue(t, call.entry); value["answer"] != "The pricing page." || value["selectedOptions"] != nil {
		t.Fatalf("result = %#v", value)
	}
}

func TestPostChannelMessageRefusesInvalidSelectedOptions(t *testing.T) {
	store := newTestKernelStore(t)
	f := claimedForegroundRun(t, store)
	single := `{"question":"` + askUserTestQuestion + `","options":[{"label":"Pricing"},{"label":"Docs"}]}`
	call := newAskUserCallWithArguments(t, store, f.run, "foreground-worker", json.RawMessage(single))
	result := call.settle(t)
	f.run = call.park(t, result.Park)
	question := f.question(t)

	post := func(req PostChannelMessageRequest) error {
		current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		req.Scope, req.ConversationID, req.ExpectedRevision = current.Scope, current.ID, current.Revision
		req.Sender, req.Intent, req.Audience = f.trigger.Sender, MessageIntentAnswer, ConversationAudience{Kind: ConversationAudienceChannel}
		_, err = f.service.PostChannelMessage(t.Context(), req)
		return err
	}
	for name, req := range map[string]PostChannelMessageRequest{
		"unknown id":           {Content: "Blog", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o9"}},
		"two ids, one allowed": {Content: "Pricing, Docs", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o1", "o2"}},
		"duplicate ids":        {Content: "Pricing", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o1", "o1"}},
		"no question target":   {Content: "Pricing", SelectedOptionIDs: []string{"o1"}},
		"target offers none":   {Content: "Pricing", ReplyToMessageID: f.trigger.ID, SelectedOptionIDs: []string{"o1"}},
	} {
		req.IdempotencyKey = "bad-" + strings.ReplaceAll(name, " ", "-")
		if err := post(req); !errors.Is(err, ErrInvalidConversation) {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
	// One option by reply id, and free text with nothing selected, are fine.
	if err := post(PostChannelMessageRequest{Content: "Docs", ReplyToMessageID: question.ID, SelectedOptionIDs: []string{"o2"}, IdempotencyKey: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := post(PostChannelMessageRequest{Content: "Something else", ResolvesMessageID: question.ID, IdempotencyKey: "free"}); err != nil {
		t.Fatal(err)
	}

	// Options belong only on a question.
	current, _ := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	_, err := f.service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
		Sender: f.trigger.Sender, Intent: MessageIntentAnswer, Content: "Pick one", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		Question: &ChannelQuestion{Options: []ChannelQuestionOption{{ID: "o1", Label: "A"}, {ID: "o2", Label: "B"}}}, IdempotencyKey: "not-a-question",
	})
	if !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("options on an answer: err = %v", err)
	}
}

func TestAgentBuilderConversationOriginValidates(t *testing.T) {
	if err := (ConversationReference{Kind: ConversationReferenceAgentBuilder, ID: "k"}).Validate(); err != nil {
		t.Fatal(err)
	}
	store := newTestKernelStore(t)
	seedTestSpace(t, store, Scope{Kind: "organization", ID: "builder"})
	service := NewConversationService(store)
	origin := &ConversationReference{Kind: ConversationReferenceAgentBuilder, ID: "k"}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: Scope{Kind: "organization", ID: "builder"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "builder-agent"},
		Title: "New agent", Origin: origin, IdempotencyKey: "agent-builder:k",
	})
	if err != nil || conversation.Origin == nil || *conversation.Origin != *origin {
		t.Fatalf("conversation = %#v %v", conversation, err)
	}
}
