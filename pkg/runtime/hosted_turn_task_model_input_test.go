package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestHostedTurnModelInputProjectsTrustedConversationTasks(t *testing.T) {
	trusted := &HostedConversationTaskContext{
		ConversationID: "conversation", ThreadRootID: "thread", CanStart: true,
		Tasks: []ConversationTaskSnapshot{{
			TaskID: "task", WorkRunID: "work-run", Goal: "Prepare the analysis", Status: "running", Revision: 1,
			CreatedAt: time.Date(2026, time.October, 4, 6, 30, 0, 0, time.UTC),
		}},
	}
	request := HostedTurnRequest{
		ConversationTasks: trusted,
		InputContext: map[string]interface{}{"conversationTasks": map[string]interface{}{
			"conversationId": "forged", "canStart": false,
		}},
		ContinuationCheckpoint: map[string]interface{}{"conversationTasks": map[string]interface{}{
			"conversationId": "forged-checkpoint", "canStart": false,
		}},
	}
	encoded, err := MarshalHostedTurnModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	var projected HostedTurnModelInput
	if err := json.Unmarshal(encoded, &projected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projected.ConversationTasks, trusted) {
		t.Fatalf("trusted task context lost or overwritten: %#v", projected.ConversationTasks)
	}
	projected.ConversationTasks.Tasks[0].Goal = "changed"
	if trusted.Tasks[0].Goal != "Prepare the analysis" {
		t.Fatal("model projection mutated trusted task state")
	}
	withoutTasks, err := EstimateHostedTurnInputTokens(HostedTurnRequest{})
	if err != nil {
		t.Fatal(err)
	}
	withTasks, err := EstimateHostedTurnInputTokens(HostedTurnRequest{ConversationTasks: trusted})
	if err != nil || withTasks <= withoutTasks {
		t.Fatalf("trusted task context was not reserved in model input: without=%d with=%d err=%v", withoutTasks, withTasks, err)
	}
}

func TestHostedTurnModelInputDoesNotInferTaskAuthorityFromUntrustedState(t *testing.T) {
	encoded, err := MarshalHostedTurnModelInput(HostedTurnRequest{
		InputContext:           map[string]interface{}{"conversationTasks": map[string]interface{}{"canStart": true}},
		ContinuationCheckpoint: map[string]interface{}{"conversationTasks": map[string]interface{}{"canStart": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]interface{}
	if err := json.Unmarshal(encoded, &projected); err != nil {
		t.Fatal(err)
	}
	if _, exists := projected["conversationTasks"]; exists {
		t.Fatalf("untrusted data became task authority: %#v", projected["conversationTasks"])
	}
}

func TestHostedConversationTaskContextClonePreservesSnapshotIsolation(t *testing.T) {
	original := &HostedConversationTaskContext{
		ConversationID: "conversation", CanStart: true,
		Tasks: []ConversationTaskSnapshot{{TaskID: "task", Goal: "Do work", Status: "running"}},
	}
	cloned := cloneHostedConversationTaskContext(original)
	cloned.CanStart = false
	cloned.Tasks[0].Goal = "changed"
	if !original.CanStart || original.Tasks[0].Goal != "Do work" {
		t.Fatal("cloned task authority aliased the durable source")
	}
	if cloneHostedConversationTaskContext(nil) != nil {
		t.Fatal("missing task context became authority")
	}
}

func TestFinalFailureExplanationRemovesConversationTaskAuthority(t *testing.T) {
	request := HostedTurnRequest{ConversationTasks: &HostedConversationTaskContext{ConversationID: "conversation", CanStart: true}}
	projectFinalFailureExplanation(&request)
	if request.ConversationTasks != nil {
		t.Fatal("failure explanation retained task admission authority")
	}
	run := &AgentRun{Checkpoint: checkpointFinalFailureExplanation(nil, "action", "The request failed.")}
	outcome := &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, ProposedTask: &TurnTaskProposal{TaskKey: "task", Goal: "Do more work", Acknowledgment: "I will start."}}
	if err := validateFinalFailureExplanationOutcome(run, outcome); err == nil {
		t.Fatal("failure explanation bypassed the terminal task boundary")
	}
}

func TestAgentRequestReviewTurnsRejectConversationTaskProposals(t *testing.T) {
	for _, kind := range []string{"intake", "completion"} {
		t.Run(kind, func(t *testing.T) {
			inner := TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: &TurnTaskProposal{
					TaskKey: "escape", Goal: "Do more work", Acknowledgment: "I will start.",
				}}, nil
			})
			var runner TurnRunner = &agentRequestDecisionTurnRunner{inner: inner}
			if kind == "completion" {
				runner = &agentRequestCompletionReviewTurnRunner{inner: inner}
			}
			if outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{}); err == nil || outcome != nil {
				t.Fatalf("review escaped its authority: %#v %v", outcome, err)
			}
		})
	}
}
