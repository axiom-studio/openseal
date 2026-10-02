package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunEventWaitOpaqueSubjectValidation(t *testing.T) {
	for _, subject := range []string{
		"resources/approvals/one",
		"room@conference.example",
		"opaque+reference=value",
		"会話/東京@provider+one",
		strings.Repeat("x", 512),
	} {
		spec := eventWaitContractSpec("reply", subject, eventWaitContractEpoch)
		if err := spec.Validate(); err != nil {
			t.Errorf("canonical opaque subject %q rejected: %v", subject, err)
		}
		if spec.Subject != subject {
			t.Errorf("opaque subject changed: got %q, want %q", spec.Subject, subject)
		}
	}
	for _, subject := range []string{
		"", " ", " subject", "subject ", "\u00a0subject", "subject\u00a0",
		"sub\x00ject", "sub\tject", "sub\nject", "sub\rject", "sub\x1fject", "sub\x7fject",
		strings.Repeat("x", 513),
	} {
		spec := eventWaitContractSpec("reply", subject, eventWaitContractEpoch)
		if err := spec.Validate(); !errors.Is(err, ErrInvalidRunEventWait) {
			t.Errorf("invalid subject %q accepted: %v", subject, err)
		}
	}
	for _, field := range []string{"key", "source"} {
		spec := eventWaitContractSpec("reply", "resources/one@provider", eventWaitContractEpoch)
		if field == "key" {
			spec.Key = "keys/one"
		} else {
			spec.Source = "sources/one"
		}
		if err := spec.Validate(); !errors.Is(err, ErrInvalidRunEventWait) {
			t.Errorf("opaque subject validation relaxed %s identity: %v", field, err)
		}
	}
}

func TestRunEventWaitOpaqueSubjectExactMatch(t *testing.T) {
	for _, test := range []struct {
		name, eventType, subject string
	}{
		{"resource", "approval.decided", "resources/approvals/item+one@東京"},
		{"conversation", "conversation.message.received", "room@conference.example/会話+one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
				scope := Scope{Kind: "tenant", ID: "opaque-subject-" + test.name}
				spec := eventWaitContractSpec("reply", test.subject, eventWaitContractEpoch)
				spec.Type = test.eventType
				run := eventWaitContractCreate(t, fixture.store, scope, spec, spec.After)
				eventWaitContractProcess(t, fixture.store, scope, spec.After, 0)
				now := spec.After.Add(time.Minute)
				wrong := eventWaitContractEvent(scope, spec, "adjacent-subject", now)
				wrong.Subject += "-adjacent"
				eventWaitContractPublish(t, fixture.store, wrong, now)
				eventWaitContractProcess(t, fixture.store, scope, now, 0)
				if fixture.reopen != nil {
					fixture.store = fixture.reopen()
				}
				stored, err := fixture.store.GetAgentRun(t.Context(), scope, run.ID)
				if err != nil || stored == nil || stored.Status != AgentRunStatusWaitingForEvent || stored.Revision != run.Revision || stored.Checkpoint[runEventWaitCheckpointKey] != nil {
					t.Fatalf("adjacent opaque subject resumed wait: run=%#v, err=%v", stored, err)
				}
				if stored.WakeCondition == nil || stored.WakeCondition.EventWait == nil || !sameRunEventWaitSpec(stored.WakeCondition.EventWait, &spec) {
					t.Fatalf("durable wait changed its exact subject or authority: %#v", stored.WakeCondition)
				}
				matched := eventWaitContractEvent(scope, spec, "exact-subject", now)
				eventWaitContractPublish(t, fixture.store, matched, now)
				eventWaitContractProcess(t, fixture.store, scope, now, 1)
				eventWaitContractProcess(t, fixture.store, scope, now, 0)
				resumed, err := fixture.store.GetAgentRun(t.Context(), scope, run.ID)
				if err != nil || resumed == nil || resumed.Status != AgentRunStatusQueued || resumed.Revision != run.Revision+1 {
					t.Fatalf("exact opaque subject did not resume once: run=%#v, err=%v", resumed, err)
				}
				resolution, ok := resumed.Checkpoint[runEventWaitCheckpointKey].(map[string]interface{})
				if !ok || resolution["status"] != string(RunEventWaitMatched) || resolution["key"] != spec.Key {
					t.Fatalf("wrong opaque wait outcome: %#v", resolution)
				}
				observed, ok := resolution["event"].(map[string]interface{})
				if !ok || observed["id"] != matched.ID || observed["subject"] != test.subject || observed["source"] != spec.Source {
					t.Fatalf("exact opaque observation changed: %#v", observed)
				}
			})
		})
	}
}
