package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func workflowEvidenceStore(t *testing.T, kind string) KernelStore {
	t.Helper()
	if kind == "memory" {
		return NewMemoryStore()
	}
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "subject-evidence.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func workflowEvidenceCall(run *AgentRun, id, subject string) *ActionCall {
	now := time.Now().UTC()
	return &ActionCall{ID: id, Scope: run.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID,
		BindingID: "event-account", BindingRevision: 1, SkillID: "test-events", SkillVersion: "1.0.0", Action: "send_message",
		Status: ActionCallStatusSucceeded, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
		MaxAttempts: 1, Revision: 1, CreatedAt: now, UpdatedAt: now, AvailableAt: now, CompletedAt: &now,
		Arguments: map[string]interface{}{"participant": "U-recipient"},
		Output:    map[string]interface{}{"receipt": map[string]interface{}{"conversationId": subject}, "target": "U-recipient", "unrelatedPrivateOutput": "never-project-this", "credentials": map[string]interface{}{"token": "never-project-secret"}},
	}
}

func persistWorkflowEvidenceCall(t *testing.T, store KernelStore, call *ActionCall) *AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(t.Context(), call.Scope, call.RunID)
	if err != nil {
		t.Fatal(err)
	}
	updated := cloneAgentRun(run)
	updated.Revision++
	updated.UpdatedAt = call.UpdatedAt
	if updated.Checkpoint == nil {
		updated.Checkpoint = map[string]interface{}{}
	}
	updated.Checkpoint = checkpointTerminalAction(updated.Checkpoint, call, nil)
	result, err := store.CreateActionProposal(t.Context(), ActionProposalRecord{Call: call, Run: updated, ExpectedRunRevision: run.Revision,
		Event: &ActivityEvent{ID: "evidence-" + call.ID, Scope: call.Scope, RunID: call.RunID, EventType: "action.succeeded", Summary: "Persist connector receipt", CreatedAt: call.CreatedAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Run
}

type workflowEvidencePointStore struct {
	runbookActionStore
	ExternalConversationGatewayStore
	RunEventWaitStore
	lookups []string
	scans   int
}

func (s *workflowEvidencePointStore) GetActionCall(ctx context.Context, scope Scope, id string) (*ActionCall, error) {
	s.lookups = append(s.lookups, id)
	return s.runbookActionStore.GetActionCall(ctx, scope, id)
}

func (s *workflowEvidencePointStore) ListActionCalls(context.Context, ActionFilter) ([]*ActionCall, error) {
	s.scans++
	return nil, errors.New("workflow evidence must not scan action history")
}

func pointWorkflowEvidenceStore(store KernelStore) *workflowEvidencePointStore {
	return &workflowEvidencePointStore{runbookActionStore: store.(runbookActionStore), ExternalConversationGatewayStore: store.(ExternalConversationGatewayStore), RunEventWaitStore: store.(RunEventWaitStore)}
}

func TestWorkflowConversationSubjectRequiresDurableProviderReceiptAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := workflowEvidenceStore(t, kind)
			run, catalog, bound, args := workflowActionFixture(t, store, true)
			run = persistWorkflowEvidenceCall(t, store, workflowEvidenceCall(run, "delivered-dm", "D-real-conversation"))
			args["subjectEvidenceActionCallId"] = "delivered-dm"
			args["subject"] = "U-recipient"
			points := pointWorkflowEvidenceStore(store)
			if _, err := dispatchWorkflowForTest(t, points, catalog, run, bound, args, "wrong-dm-subject"); err == nil || !strings.Contains(err.Error(), "exactly equal") {
				t.Fatalf("participant ID was accepted as conversation subject: %v", err)
			}
			if values, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: run.Scope, Kind: RunKindAgentWork}); err != nil || len(values) != 0 {
				t.Fatalf("invalid proof created workflows: %#v, %v", values, err)
			}
			if !reflect.DeepEqual(points.lookups, []string{"delivered-dm"}) || points.scans != 0 {
				t.Fatalf("creation evidence reads=%v scans=%d", points.lookups, points.scans)
			}

			// A verified reply is buffered before creation. Correcting the subject
			// to the actual durable delivery receipt consumes that same reply.
			provider := &workflowContinuationProvider{event: NormalizedExternalConversationEvent{ID: "early-dm-reply", Type: capability.ConversationEventMessageReceived,
				ExternalConversationID: "D-real-conversation", ExternalMessageID: "reply-message", ExternalParticipantID: "U-recipient", Text: "Ready", Direct: true,
				OrderingKey: "D-real-conversation:reply-message", OccurredAt: time.Now().UTC().Add(time.Second)}}
			gateway, err := store.(ExternalConversationGatewayStore).GetExternalConversationGateway(t.Context(), run.Scope, "event-gateway")
			if err != nil {
				t.Fatal(err)
			}
			transport := NewExternalConversationTransportService(store.(ExternalConversationStore), catalog)
			received, err := transport.NormalizeExternalConversationRegisteredGatewayIngress(t.Context(), ExternalConversationPublicIngressRequest{Route: gateway.IngressRoute,
				Method: http.MethodPost, Headers: map[string][]string{"X-Test-Signature": {"verified"}}, Body: []byte(`{"event":"reply"}`)}, provider)
			if err != nil || received == nil || received.Response.StatusCode != http.StatusOK || provider.calls != 1 {
				t.Fatalf("buffered verified reply=%#v error=%v", received, err)
			}
			args["subject"] = "D-real-conversation"
			args["attributes"] = map[string]interface{}{"externalParticipantId": "U-recipient"}
			points.lookups = nil
			result, err := dispatchWorkflowForTest(t, points, catalog, run, bound, args, "correct-dm-subject")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(points.lookups, []string{"delivered-dm"}) || points.scans != 0 || provider.calls != 1 {
				t.Fatalf("creation reads=%v scans=%d provider calls=%d", points.lookups, points.scans, provider.calls)
			}
			work := result["run"].(*AgentRun)
			worker, err := NewRunEventWaitWorker(store.(RunEventWaitStore), RunEventWaitWorkerOptions{WorkerID: "subject-proof", LeaseDuration: time.Minute, BatchSize: 10, Concurrency: 1})
			if err != nil {
				t.Fatal(err)
			}
			worker.now = func() time.Time { return provider.event.OccurredAt.Add(time.Second) }
			if count, err := worker.ProcessScope(t.Context(), run.Scope); err != nil || count != 1 {
				t.Fatalf("buffered event resolution=%d error=%v", count, err)
			}
			wait, err := store.(RunEventWaitStore).GetRunEventWait(t.Context(), run.Scope, work.ID, "initial")
			if err != nil || wait.Status != RunEventWaitMatched || wait.Spec.Subject != "D-real-conversation" || wait.EventID != "early-dm-reply" {
				t.Fatalf("matched workflow=%#v error=%v", wait, err)
			}
		})
	}
}

func TestWorkflowSubjectEvidenceRejectsUntrustedReceiptsAcrossStores(t *testing.T) {
	changes := []string{"missing-proof", "missing-call", "failed", "foreign-run", "foreign-scope", "foreign-deployment", "foreign-binding", "old-revision", "future-revision", "foreign-skill", "foreign-version", "undeclared-action", "revoked-action", "empty-subject", "whitespace-subject", "control-subject", "oversized-subject", "non-string-subject", "array-path", "wrong-path", "padded-input"}
	for _, kind := range []string{"memory", "sqlite"} {
		for _, change := range changes {
			t.Run(kind+"/"+change, func(t *testing.T) {
				store := workflowEvidenceStore(t, kind)
				run, catalog, bound, args := workflowActionFixture(t, store, true)
				call := workflowEvidenceCall(run, "candidate-receipt", "D-real-conversation")
				args["subject"], args["subjectEvidenceActionCallId"] = "D-real-conversation", call.ID
				switch change {
				case "missing-proof":
					delete(args, "subjectEvidenceActionCallId")
				case "missing-call":
					args["subjectEvidenceActionCallId"] = "invented-receipt"
				case "failed":
					call.Status = ActionCallStatusFailed
				case "foreign-run", "foreign-scope":
					scope := run.Scope
					if change == "foreign-scope" {
						scope.ID = "another-tenant"
					}
					foreign, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: run.Owner, AssignedAgentID: run.AssignedAgentID, Goal: "Other request", Source: RunSourceChat, Context: cloneMap(run.Context)})
					if err != nil {
						t.Fatal(err)
					}
					call.Scope, call.RunID = foreign.Scope, foreign.ID
				case "foreign-deployment":
					call.DeploymentID = "another-agent"
				case "foreign-binding":
					call.BindingID = "another-account"
				case "old-revision":
					adapter, err := catalog.ResolveConversationAdapterBinding(t.Context(), bound.Binding.Scope, run.AssignedAgentID, "event-account", "messages")
					if err != nil {
						t.Fatal(err)
					}
					binding := *adapter.Binding
					binding.MaximumRisk = skill.RiskLevelWrite
					if _, err := catalog.UpsertBinding(t.Context(), skill.UpsertBindingRequest{Binding: &binding, ExpectedRevision: binding.Revision, Actor: skill.BindingActor{Type: "test", ID: "operator"}, Reason: "Revise reviewed connector binding"}); err != nil {
						t.Fatal(err)
					}
				case "future-revision":
					call.BindingRevision = 2
				case "foreign-skill":
					call.SkillID = "another-connector"
				case "foreign-version":
					call.SkillVersion = "2.0.0"
				case "undeclared-action":
					call.Action = "unrelated"
				case "revoked-action":
					adapter, err := catalog.ResolveConversationAdapterBinding(t.Context(), bound.Binding.Scope, run.AssignedAgentID, "event-account", "messages")
					if err != nil {
						t.Fatal(err)
					}
					binding := *adapter.Binding
					binding.AllowedActions = []string{"read_conversation", "unrelated"}
					updated, err := catalog.UpsertBinding(t.Context(), skill.UpsertBindingRequest{Binding: &binding, ExpectedRevision: binding.Revision, Actor: skill.BindingActor{Type: "test", ID: "operator"}, Reason: "Revoke delivery"})
					if err != nil {
						t.Fatal(err)
					}
					call.BindingRevision = updated.Revision
				case "empty-subject":
					call.Output["receipt"].(map[string]interface{})["conversationId"] = ""
				case "whitespace-subject":
					call.Output["receipt"].(map[string]interface{})["conversationId"] = " D-real-conversation "
				case "control-subject":
					call.Output["receipt"].(map[string]interface{})["conversationId"] = "D-real\nconversation"
				case "oversized-subject":
					call.Output["receipt"].(map[string]interface{})["conversationId"] = strings.Repeat("D", 513)
				case "non-string-subject":
					call.Output["receipt"].(map[string]interface{})["conversationId"] = 123
				case "array-path":
					call.Output["receipt"] = []interface{}{map[string]interface{}{"conversationId": "D-real-conversation"}}
				case "wrong-path":
					call.Output = map[string]interface{}{"channel": "D-real-conversation"}
				case "padded-input":
					args["subject"] = " D-real-conversation "
				}
				persisted := persistWorkflowEvidenceCall(t, store, call)
				if call.Scope == run.Scope && call.RunID == run.ID {
					run = persisted
				}
				validator, err := NewRunbookActionValidator(store.(runbookActionStore))
				if err != nil {
					t.Fatal(err)
				}
				validator.SetWorkflowCatalog(catalog)
				if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args}); err == nil {
					t.Fatal("untrusted subject receipt passed proposal validation")
				}
				if _, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "reject-"+change); err == nil {
					t.Fatal("untrusted subject receipt created a workflow")
				}
				values, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: run.Scope, Kind: RunKindAgentWork})
				if err != nil || len(values) != 0 {
					t.Fatalf("rejected receipt created workflows: %#v error=%v", values, err)
				}
				if change != "missing-proof" && change != "missing-call" && change != "padded-input" {
					sources, err := workflowSources(t.Context(), store.(runbookActionStore), catalog, run, call.ID)
					if err != nil || len(sources) != 1 || len(sources[0].KnownSubjects) != 0 {
						t.Fatalf("untrusted receipt projected into discovery: %#v error=%v", sources, err)
					}
				}
			})
		}
	}
}

func TestWorkflowSubjectEvidenceRetainsOpaqueProviderIDsAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		for _, subject := range []string{"room+opaque=42", "会話-123", "workspace/room@server", "same-participant-and-room"} {
			t.Run(kind+"/"+subject, func(t *testing.T) {
				store := workflowEvidenceStore(t, kind)
				run, catalog, bound, args := workflowActionFixture(t, store, true)
				call := workflowEvidenceCall(run, "opaque-receipt", subject)
				call.Action = "read_conversation"
				run = persistWorkflowEvidenceCall(t, store, call)
				args["subject"], args["subjectEvidenceActionCallId"] = subject, call.ID
				args["attributes"] = map[string]interface{}{"externalParticipantId": subject}
				result, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "opaque-proof")
				if err != nil {
					t.Fatal(err)
				}
				wait, err := store.(RunEventWaitStore).GetRunEventWait(t.Context(), run.Scope, result["workflowId"].(string), "initial")
				if err != nil || wait.Spec.Subject != subject || wait.Spec.Attributes["externalParticipantId"] != subject {
					t.Fatalf("opaque subject or participant was rewritten: %#v, %v", wait, err)
				}
			})
		}
	}
}

func TestWorkflowSourceDiscoveryProjectsBoundedDurableSubjectEvidenceAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := workflowEvidenceStore(t, kind)
			run, catalog, bound, _ := workflowActionFixture(t, store, true)
			for i, subject := range []string{"D-first", "D-explicit", "D-latest"} {
				id := []string{"first-call", "explicit-call", "latest-call"}[i]
				run = persistWorkflowEvidenceCall(t, store, workflowEvidenceCall(run, id, subject))
			}
			// The model checkpoint can contain arbitrary results and history. It
			// chooses only one ID; durable action output supplies the actual subject.
			latest := run.Checkpoint["lastAction"].(map[string]interface{})
			latest["result"] = map[string]interface{}{"receipt": map[string]interface{}{"conversationId": "invented-conversation"}}
			run.Checkpoint["actionHistory"] = []interface{}{map[string]interface{}{"actionCallId": "first-call", "result": map[string]interface{}{"conversationId": "invented-history"}}}
			points := pointWorkflowEvidenceStore(store)
			sources, err := workflowSources(t.Context(), points, catalog, run, "explicit-call")
			if err != nil || len(sources) != 1 {
				t.Fatalf("sources=%#v error=%v", sources, err)
			}
			source := sources[0]
			want := []WorkflowKnownSubject{{ExternalConversationID: "D-explicit", ActionCallID: "explicit-call"}, {ExternalConversationID: "D-latest", ActionCallID: "latest-call"}}
			if !source.EvidenceRequired || source.SkillVersion != "1.0.0" || len(source.SubjectEvidence) != 2 || !reflect.DeepEqual(source.KnownSubjects, want) {
				t.Fatalf("source proof projection=%#v", source)
			}
			if !reflect.DeepEqual(points.lookups, []string{"explicit-call", "latest-call"}) || points.scans != 0 {
				t.Fatalf("discovery lookups=%v scans=%d", points.lookups, points.scans)
			}
			encoded, err := json.Marshal(sources)
			if err != nil || strings.Contains(string(encoded), "never-project") || strings.Contains(string(encoded), "invented") || strings.Contains(string(encoded), "D-first") || strings.Contains(string(encoded), "credentials") {
				t.Fatalf("discovery leaked undeclared output or history: %s, %v", encoded, err)
			}
			points.lookups = nil
			if _, err := workflowSources(t.Context(), points, catalog, run, "latest-call"); err != nil || !reflect.DeepEqual(points.lookups, []string{"latest-call"}) {
				t.Fatalf("candidate deduplication reads=%v error=%v", points.lookups, err)
			}
			latest["actionCallId"] = "invented-call"
			points.lookups = nil
			sources, err = workflowSources(t.Context(), points, catalog, run)
			if err != nil || len(sources[0].KnownSubjects) != 0 || !reflect.DeepEqual(points.lookups, []string{"invented-call"}) {
				t.Fatalf("checkpoint invented evidence: %#v reads=%v error=%v", sources, points.lookups, err)
			}
			// Schema and runtime action input both accept one optional receipt.
			sourcesBound, err := catalog.Resolve(t.Context(), bound.Binding.Scope, run.AssignedAgentID, bound.Definition.ID, bound.Definition.Version, RunbookActionWorkflowSources)
			if err != nil || catalog.ValidateInput(t.Context(), sourcesBound, map[string]interface{}{"subjectEvidenceActionCallId": "explicit-call"}) != nil {
				t.Fatalf("discovery input contract: %v", err)
			}
		})
	}
}
