package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type fixedOutreachActionReader struct {
	call     *ActionCall
	approval *ApprovalCheckpoint
}

func (r *fixedOutreachActionReader) GetActionCall(context.Context, Scope, string) (*ActionCall, error) {
	return r.call, nil
}

func (r *fixedOutreachActionReader) GetApproval(context.Context, Scope, string) (*ApprovalCheckpoint, error) {
	return r.approval, nil
}

func TestOutreachLifecyclePreservesEvidenceIdentityApprovalAndReceipt(t *testing.T) {
	store := NewMemoryStore(100)
	scope := Scope{Kind: "tenant", ID: "research"}
	initiative, runs := seedExecutableMonitorInitiative(t, store, scope)
	sources := NewSourceMonitorService(store, store, store, store)
	ingested, err := sources.Ingest(context.Background(), sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-1", "thread-7", "Setup is confusing"))
	if err != nil {
		t.Fatal(err)
	}
	actionReader := &fixedOutreachActionReader{}
	service := NewOutreachService(store, store, store, actionReader)
	service.now = func() time.Time { return time.Date(2026, 7, 13, 16, 0, 0, 0, time.UTC) }
	service.newID = func() string { return "outreach-generated" }
	disclosure := "Disclosure: I work on OpenSeal and am seeking product feedback."
	body := "What part of setup was hardest for you? " + disclosure
	thread := &OutreachThread{
		Scope: scope, InitiativeID: initiative.ID, SourceObservationID: ingested.Observation.ID, MonitorID: "monitor-a",
		StableSourceID: "thread-7", TargetURI: ingested.Observation.SourceURI, Owner: initiative.Owner, AssignedAgentID: "researcher",
		SourcePolicyRef: "approved-forums", ApprovalPolicyRef: "review-outreach",
		Identity: OutreachIdentity{ProfileRef: "profile:openseal-research", DisplayName: "OpenSeal Research", Affiliation: "OpenSeal", Disclosure: disclosure},
		Messages: []OutreachMessage{{ID: "message-1", Direction: OutreachMessageOutbound, Intent: OutreachIntentRequestFeedback, Body: body, Status: OutreachMessageDraft,
			Capability: &OutreachCapability{SkillID: "forum-outreach", SkillVersion: "1.0.0", Action: "reply", TargetArgument: "threadUrl", BodyArgument: "message", Arguments: map[string]interface{}{"threadUrl": ingested.Observation.SourceURI, "message": body}}}},
	}
	created, event, err := service.Create(context.Background(), CreateOutreachThreadRequest{Thread: thread, IdempotencyKey: "draft-1", Actor: ActivityActor{Type: "user", ID: "operator"}})
	if err != nil || event == nil || created.ID != "outreach-generated" || created.Messages[0].Status != OutreachMessageDraft {
		t.Fatalf("created=%#v event=%#v err=%v", created, event, err)
	}
	actionReader.call = &ActionCall{ID: "action-reply", Scope: scope, RunID: "run-outreach", DeploymentID: "researcher", SkillID: "forum-outreach", SkillVersion: "1.0.0", Action: "reply",
		Status: ActionCallStatusWaitingApproval, SideEffect: skill.SideEffectExternal, Arguments: map[string]interface{}{"threadUrl": ingested.Observation.SourceURI, "message": body},
		EvidenceRefs: []string{ingested.Observation.ID}, IdempotencyKey: "outreach:message-1", ApprovalID: "approval-reply"}
	actionReader.approval = &ApprovalCheckpoint{ID: "approval-reply", Scope: scope, RunID: "run-outreach", ActionCallID: "action-reply", EvidenceRefs: []string{ingested.Observation.ID}}
	thread.ID = "different-id"
	replayed, replayEvent, err := service.Create(context.Background(), CreateOutreachThreadRequest{Thread: thread, IdempotencyKey: "draft-1"})
	if err != nil || replayEvent != nil || replayed.ID != created.ID {
		t.Fatalf("replayed=%#v event=%#v err=%v", replayed, replayEvent, err)
	}
	linked, approvalEvent, err := service.LinkAction(context.Background(), scope, created.ID, LinkOutreachActionRequest{
		ExpectedRevision: 1, MessageID: "message-1", RunID: "run-outreach", ActionCallID: "action-reply", ApprovalID: "approval-reply",
		Actor: ActivityActor{Type: "worker", ID: "agent-worker"},
	})
	if err != nil || linked.Messages[0].Status != OutreachMessagePendingApproval || approvalEvent.EventType != "outreach.approval_requested" {
		t.Fatalf("linked=%#v event=%#v err=%v", linked, approvalEvent, err)
	}
	actionReader.call.Status = ActionCallStatusSucceeded
	actionReader.approval.Status = ApprovalStatusApproved
	digest := sha256.Sum256([]byte(body))
	receipt := OutreachReceipt{Provider: "forum", ExternalID: "reply-99", ExternalURI: "https://forum.example/threads/thread-7#reply-99", Digest: "sha256:" + hex.EncodeToString(digest[:]), DeliveredAt: service.now()}
	delivered, deliveryEvent, err := service.RecordDelivery(context.Background(), scope, created.ID, RecordOutreachDeliveryRequest{ExpectedRevision: 2, MessageID: "message-1", ActionCallID: "action-reply", Receipt: receipt, Actor: ActivityActor{Type: "worker", ID: "action-worker"}})
	if err != nil || delivered.Messages[0].Status != OutreachMessageDelivered || deliveryEvent.EventType != "outreach.message_delivered" || delivered.Revision != 3 {
		t.Fatalf("delivered=%#v event=%#v err=%v", delivered, deliveryEvent, err)
	}
	replayedDelivery, deliveryReplayEvent, err := service.RecordDelivery(context.Background(), scope, created.ID, RecordOutreachDeliveryRequest{ExpectedRevision: 2, MessageID: "message-1", ActionCallID: "action-reply", Receipt: receipt})
	if err != nil || deliveryReplayEvent != nil || replayedDelivery.Revision != 3 {
		t.Fatalf("delivery replay=%#v event=%#v err=%v", replayedDelivery, deliveryReplayEvent, err)
	}
	if _, err := service.Get(context.Background(), Scope{Kind: "tenant", ID: "foreign"}, created.ID); err != ErrOutreachThreadNotFound {
		t.Fatalf("cross-tenant get=%v", err)
	}
	activity, err := store.ListActivity(context.Background(), ActivityFilter{Scope: scope, Descending: true, Limit: 20})
	if err != nil {
		t.Fatalf("activity=%#v err=%v", activity, err)
	}
	outreachEvents := 0
	for _, activityEvent := range activity {
		if strings.HasPrefix(activityEvent.EventType, "outreach.") {
			outreachEvents++
		}
		if strings.Contains(activityEvent.Summary, body) {
			t.Fatalf("reviewed body leaked into compact activity: %#v", activityEvent)
		}
	}
	if outreachEvents != 3 {
		t.Fatalf("outreach activity count=%d events=%#v", outreachEvents, activity)
	}
}

func TestOutreachRejectsCovertIdentityAndDriftedEvidence(t *testing.T) {
	store := NewMemoryStore(100)
	scope := Scope{Kind: "tenant", ID: "research"}
	initiative, runs := seedExecutableMonitorInitiative(t, store, scope)
	sources := NewSourceMonitorService(store, store, store, store)
	ingested, err := sources.Ingest(context.Background(), sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-1", "thread-7", "Setup is confusing"))
	if err != nil {
		t.Fatal(err)
	}
	disclosure := "Disclosure: I work on OpenSeal."
	body := "Can you explain the setup issue?"
	fixture := &OutreachThread{
		ID: "outreach-1", Scope: scope, InitiativeID: initiative.ID, SourceObservationID: ingested.Observation.ID, MonitorID: "monitor-a",
		StableSourceID: "thread-7", TargetURI: ingested.Observation.SourceURI, Owner: initiative.Owner, AssignedAgentID: "researcher",
		SourcePolicyRef: "approved-forums", ApprovalPolicyRef: "review-outreach",
		Identity: OutreachIdentity{ProfileRef: "profile:research", DisplayName: "Research", Affiliation: "OpenSeal", Disclosure: disclosure},
		Messages: []OutreachMessage{{ID: "message-1", Direction: OutreachMessageOutbound, Intent: OutreachIntentClarify, Body: body, Status: OutreachMessageDraft,
			Capability: &OutreachCapability{SkillID: "forum", SkillVersion: "1", Action: "reply", TargetArgument: "url", BodyArgument: "body", Arguments: map[string]interface{}{"url": ingested.Observation.SourceURI, "body": body}}}},
	}
	service := NewOutreachService(store, store, store, store)
	if _, _, err := service.Create(context.Background(), CreateOutreachThreadRequest{Thread: fixture}); err == nil || !strings.Contains(err.Error(), "identity disclosure") {
		t.Fatalf("covert draft error=%v", err)
	}
	fixture.Messages[0].Body += " " + disclosure
	fixture.Messages[0].Capability.Arguments["body"] = fixture.Messages[0].Body
	fixture.TargetURI = "https://forum.example/threads/another"
	fixture.Messages[0].Capability.Arguments["url"] = fixture.TargetURI
	if _, _, err := service.Create(context.Background(), CreateOutreachThreadRequest{Thread: fixture}); err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("drifted evidence error=%v", err)
	}
}

func TestOutreachSQLiteStoreIsScopedCASAndRestartDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outreach.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	now := time.Date(2026, 7, 13, 16, 0, 0, 0, time.UTC)
	disclosure := "Disclosure: I work on OpenSeal."
	body := "Could you share more detail? " + disclosure
	thread := &OutreachThread{
		ID: "thread-1", Scope: scope, InitiativeID: "initiative-1", SourceObservationID: "observation-1", MonitorID: "monitor-1",
		StableSourceID: "source-1", TargetURI: "https://forum.example/threads/source-1", Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-1"},
		AssignedAgentID: "agent-1", SourcePolicyRef: "approved-forum", ApprovalPolicyRef: "review-outreach",
		Identity: OutreachIdentity{ProfileRef: "profile-1", DisplayName: "Research", Affiliation: "OpenSeal", Disclosure: disclosure}, Status: OutreachThreadOpen,
		Messages: []OutreachMessage{{ID: "message-1", Direction: OutreachMessageOutbound, Intent: OutreachIntentClarify, Body: body, Status: OutreachMessageDraft,
			Capability: &OutreachCapability{SkillID: "forum", SkillVersion: "1", Action: "reply", TargetArgument: "url", BodyArgument: "body", Arguments: map[string]interface{}{"url": "https://forum.example/threads/source-1", "body": body}}, CreatedAt: now, UpdatedAt: now}},
		Revision: 1, CreatedAt: now, UpdatedAt: now, IdempotencyKeyHash: "hash-1", CreationFingerprint: "fingerprint-1",
	}
	event := outreachEvent(thread, &thread.Messages[0], "outreach.thread_created", "Outreach thread drafted", ActivityActor{Type: "user", ID: "operator"}, ActivityVisibilityScope, now)
	if _, err := store.CreateOutreachThreadWithEvent(context.Background(), thread, event); err != nil {
		t.Fatal(err)
	}
	updated := cloneOutreachThread(thread)
	updated.Messages[0].Status, updated.Messages[0].RunID, updated.Messages[0].ActionCallID = OutreachMessageReady, "run-1", "action-1"
	updated.Messages[0].UpdatedAt = now.Add(time.Minute)
	updated.Revision, updated.UpdatedAt = 2, now.Add(time.Minute)
	updateEvent := outreachEvent(updated, &updated.Messages[0], "outreach.action_linked", "Outreach message ready for governed delivery", ActivityActor{Type: "worker", ID: "worker"}, ActivityVisibilityScope, updated.UpdatedAt)
	if _, err := store.UpdateOutreachThreadWithEvent(context.Background(), updated, 1, updateEvent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateOutreachThreadWithEvent(context.Background(), updated, 1, updateEvent); err != ErrOutreachThreadConflict {
		t.Fatalf("stale update=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.GetOutreachThread(context.Background(), scope, thread.ID)
	listed, listErr := store.ListOutreachThreads(context.Background(), OutreachThreadFilter{Scope: scope, InitiativeID: thread.InitiativeID, Limit: 10})
	if err != nil || listErr != nil || got.Revision != 2 || got.Messages[0].ActionCallID != "action-1" || len(listed) != 1 {
		t.Fatalf("got=%#v listed=%#v err=%v listErr=%v", got, listed, err, listErr)
	}
	if _, err := store.GetOutreachThread(context.Background(), Scope{Kind: "tenant", ID: "tenant-b"}, thread.ID); err != ErrOutreachThreadNotFound {
		t.Fatalf("cross-tenant get=%v", err)
	}
}
