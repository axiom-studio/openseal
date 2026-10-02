package runtime

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyWorkflowSubjectEvidenceKeepsBothRecipientsAfterFailedLatestAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := workflowEvidenceStore(t, kind)
			run, catalog, bound, args := workflowActionDefinitionFixture(t, store, RunbookManagementSkillLegacy130(), true)
			run = persistWorkflowEvidenceCall(t, store, workflowEvidenceCall(run, "send-first-person", "D-first-room"))
			run = persistWorkflowEvidenceCall(t, store, workflowEvidenceCall(run, "send-second-person", "D-second-room"))
			failed := workflowEvidenceCall(run, "send-failed", "D-failed-room")
			failed.Status = ActionCallStatusFailed
			run = persistWorkflowEvidenceCall(t, store, failed)
			points := pointWorkflowEvidenceStore(store)
			for _, recipient := range []struct {
				subject, participant, callID string
				reads                        []string
			}{
				{subject: "D-first-room", participant: "person-first", callID: "wait-first", reads: []string{"send-failed", "send-second-person", "send-first-person"}},
				{subject: "D-second-room", participant: "person-second", callID: "wait-second", reads: []string{"send-failed", "send-second-person"}},
			} {
				candidate := cloneMap(args)
				candidate["subject"] = recipient.subject
				candidate["attributes"] = map[string]interface{}{"externalParticipantId": recipient.participant}
				points.lookups = nil
				result, err := dispatchWorkflowForTest(t, points, catalog, run, bound, candidate, recipient.callID)
				if err != nil || !reflect.DeepEqual(points.lookups, recipient.reads) || points.scans != 0 {
					t.Fatalf("recipient %s workflow=%#v reads=%v error=%v", recipient.subject, result, points.lookups, err)
				}
				wait, err := store.(RunEventWaitStore).GetRunEventWait(t.Context(), run.Scope, result["workflowId"].(string), "initial")
				if err != nil || wait.Spec.Subject != recipient.subject || wait.Spec.Attributes["externalParticipantId"] != recipient.participant {
					t.Fatalf("recipient subject or participant changed: %#v error=%v", wait, err)
				}
			}
			candidate := cloneMap(args)
			candidate["subject"] = "D-failed-room"
			if _, err := dispatchWorkflowForTest(t, points, catalog, run, bound, candidate, "failed-receipt-wait"); err == nil {
				t.Fatal("failed latest action supplied conversation proof")
			}
			workflows, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: run.Scope, Kind: RunKindAgentWork})
			if err != nil || len(workflows) != 2 {
				t.Fatalf("recipient workflows=%#v error=%v", workflows, err)
			}
		})
	}
}

func TestLegacyWorkflowReceiptHintsRetainRecipientsWithinBoundedHistoryAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := workflowEvidenceStore(t, kind)
			run, catalog, _, _ := workflowActionDefinitionFixture(t, store, RunbookManagementSkillLegacy130(), true)
			run = persistWorkflowEvidenceCall(t, store, workflowEvidenceCall(run, "old-send", "D-old"))
			run = persistWorkflowEvidenceCall(t, store, workflowEvidenceCall(run, "new-send", "D-new"))
			reader := workflowEvidenceCall(run, "read-proof", "R-known")
			reader.Action = "read_conversation"
			run = persistWorkflowEvidenceCall(t, store, reader)
			source, err := workflowBoundSource(t.Context(), catalog, run, "event-account", "messages", "conversation")
			if err != nil {
				t.Fatal(err)
			}
			// A large unrelated last result is not read from ActionStore. The
			// bridge considers only canonical history identity metadata.
			run.Checkpoint["lastAction"] = map[string]interface{}{"actionCallId": "large-browser-call", "bindingId": "browser-account", "action": "screenshot", "result": strings.Repeat("x", 1<<20)}
			points := pointWorkflowEvidenceStore(store)
			if err := requireLegacyWorkflowSubjectEvidence(t.Context(), points, run, source, "D-old"); err != nil || !reflect.DeepEqual(points.lookups, []string{"read-proof", "new-send", "old-send"}) || points.scans != 0 {
				t.Fatalf("earlier recipient receipt lost its proof: reads=%v scans=%d error=%v", points.lookups, points.scans, err)
			}
			points.lookups = nil
			if err := requireLegacyWorkflowSubjectEvidence(t.Context(), points, run, source, "D-new"); err != nil || !reflect.DeepEqual(points.lookups, []string{"read-proof", "new-send"}) {
				t.Fatalf("newest declared receipt proof: reads=%v error=%v", points.lookups, err)
			}
			points.lookups = nil
			if err := requireLegacyWorkflowSubjectEvidence(t.Context(), points, run, source, "R-known"); err != nil || !reflect.DeepEqual(points.lookups, []string{"read-proof"}) {
				t.Fatalf("newest read receipt proof: reads=%v error=%v", points.lookups, err)
			}
			// Neither a model-projected subject nor a guessed ID establishes
			// evidence. Earlier durable receipts stay available within the same
			// fixed budget, while a guessed receipt supplies no subject facts.
			history := run.Checkpoint[actionHistoryCheckpointKey].([]interface{})
			history[len(history)-1].(map[string]interface{})["actionCallId"] = "invented-read"
			history[len(history)-1].(map[string]interface{})["result"] = map[string]interface{}{"receipt": map[string]interface{}{"conversationId": "R-known"}}
			points.lookups = nil
			if err := requireLegacyWorkflowSubjectEvidence(t.Context(), points, run, source, "R-known"); err == nil || !reflect.DeepEqual(points.lookups, []string{"invented-read", "new-send", "old-send"}) {
				t.Fatalf("checkpoint output manufactured proof: reads=%v error=%v", points.lookups, err)
			}
			// Even a malformed checkpoint cannot make bridge work proportional
			// to an unbounded history or pull old action outputs into memory.
			oversized := []interface{}{map[string]interface{}{"actionCallId": "new-send", "bindingId": "event-account", "action": "send_message"}}
			for i := 0; i < maximumActionHistoryEntries+1; i++ {
				oversized = append(oversized, map[string]interface{}{"actionCallId": "large-browser-call", "bindingId": "browser-account", "action": "screenshot"})
			}
			run.Checkpoint[actionHistoryCheckpointKey] = oversized
			points.lookups = nil
			if err := requireLegacyWorkflowSubjectEvidence(t.Context(), points, run, source, "D-new"); err == nil || len(points.lookups) != 0 || points.scans != 0 {
				t.Fatalf("unbounded or unrelated history fetched actions: reads=%v scans=%d error=%v", points.lookups, points.scans, err)
			}
			// A distinct lastAction plus 16 qualifying history hints still
			// incurs at most 16 scoped reads, and repeated IDs are deduplicated.
			hints := make([]interface{}, maximumActionHistoryEntries)
			for i := range hints {
				hints[i] = map[string]interface{}{"actionCallId": fmt.Sprintf("candidate-%d", i), "bindingId": "event-account", "action": "send_message"}
			}
			run.Checkpoint[actionHistoryCheckpointKey] = hints
			run.Checkpoint["lastAction"] = map[string]interface{}{"actionCallId": "distinct-latest", "bindingId": "event-account", "action": "send_message"}
			points.lookups = nil
			if err := requireLegacyWorkflowSubjectEvidence(t.Context(), points, run, source, "D-new"); err == nil || len(points.lookups) != maximumActionHistoryEntries || points.lookups[0] != "distinct-latest" || points.lookups[len(points.lookups)-1] != "candidate-1" {
				t.Fatalf("legacy bridge exceeded read cap: reads=%v error=%v", points.lookups, err)
			}
			for i := range hints {
				hints[i] = run.Checkpoint["lastAction"]
			}
			points.lookups = nil
			if err := requireLegacyWorkflowSubjectEvidence(t.Context(), points, run, source, "D-new"); err == nil || !reflect.DeepEqual(points.lookups, []string{"distinct-latest"}) {
				t.Fatalf("duplicate legacy hints fetched repeatedly: reads=%v error=%v", points.lookups, err)
			}
		})
	}
}

func TestWorkflowDiscoverySkipsUnrelatedActionPayloadsAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		for _, declaresProof := range []bool{false, true} {
			name := kind + "/legacy-source"
			if declaresProof {
				name = kind + "/evidence-source"
			}
			t.Run(name, func(t *testing.T) {
				store := workflowEvidenceStore(t, kind)
				run, catalog, _, _ := workflowActionFixture(t, store, declaresProof)
				if declaresProof {
					run = persistWorkflowEvidenceCall(t, store, workflowEvidenceCall(run, "explicit-proof", "D-known"))
				}
				run.Checkpoint = map[string]interface{}{"lastAction": map[string]interface{}{"actionCallId": "large-browser-call", "bindingId": "browser-account", "action": "screenshot", "result": strings.Repeat("x", 1<<20)}}
				points := pointWorkflowEvidenceStore(store)
				sources, err := workflowSources(t.Context(), points, catalog, run)
				if err != nil || len(sources) != 1 || len(points.lookups) != 0 || points.scans != 0 || len(sources[0].KnownSubjects) != 0 {
					t.Fatalf("unrelated latest action fetched: sources=%#v reads=%v error=%v", sources, points.lookups, err)
				}
				// An explicit candidate may still load a full durable record; the
				// API bounds lookup count rather than underlying payload bytes.
				sources, err = workflowSources(t.Context(), points, catalog, run, "explicit-proof")
				if err != nil {
					t.Fatal(err)
				}
				if declaresProof {
					if !reflect.DeepEqual(points.lookups, []string{"explicit-proof"}) || len(sources[0].KnownSubjects) != 1 {
						t.Fatalf("explicit durable proof reads=%v subjects=%#v", points.lookups, sources[0].KnownSubjects)
					}
				} else if len(points.lookups) != 0 {
					t.Fatalf("source without proof declarations fetched receipt: %v", points.lookups)
				}
			})
		}
	}
}
