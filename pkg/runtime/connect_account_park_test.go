package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionWorkerParksPersonGapOnCardAndRetries(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Now().UTC()
	catalog, scope := personActionCatalog(t)
	run := claimedPersonChatRun(t, store, scope, now, connectAccountTestUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	if proposal.Call.ExternalOperationDigest == "" {
		t.Fatal("test needs an external operation")
	}
	connected := false
	var checks []CredentialAvailabilityRequest
	var resolutions []CredentialResolutionRequest
	dispatches := 0
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(_ context.Context, req CredentialResolutionRequest) (map[string]string, error) {
		resolutions = append(resolutions, req)
		return map[string]string{"google": "token"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		return map[string]interface{}{"sent": true}, nil
	}))
	worker.SetCredentialAvailabilityChecker(availabilityFunc(func(_ context.Context, req CredentialAvailabilityRequest) (CredentialAvailability, error) {
		checks = append(checks, req)
		if connected {
			return CredentialAvailability{}, nil
		}
		return CredentialAvailability{Gaps: map[string]CredentialGap{"google": CredentialGapPersonSignIn}}, nil
	}))
	result := executePersonAction(t, worker, scope, proposal)
	call := result.Call
	if dispatches != 0 || call.Status != ActionCallStatusFailed || call.FailurePhase != ActionFailureBeforeDispatch ||
		!strings.HasPrefix(call.Error, "connect_account_required: ") || call.ExternalOperationDigest != "" || !isParkedConnectAccountCall(call) {
		t.Fatalf("parked call = %#v", call)
	}
	if len(checks) != 1 || checks[0].ActingUser != connectAccountTestUser || checks[0].Umbrella != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"}) ||
		checks[0].BindingID != "mail-binding" || checks[0].References["google"].ID != skill.PersonCredentialReferenceID || checks[0].Requirements["google"].OAuth2 == nil {
		t.Fatalf("availability request = %#v", checks)
	}
	card, err := store.GetSkillSetupRequest(context.Background(), scope, "connect-account:"+call.ID)
	if err != nil || card == nil {
		t.Fatalf("card = %#v, %v", card, err)
	}
	if card.Kind != SkillSetupKindConnectAccount || card.Mode != "per_person" || card.RequestedFor != connectAccountTestUser || card.Status != "pending" ||
		card.ConversationID != "conversation-1" || card.RunID != run.ID || len(card.Slots) != 1 || card.Slots[0].Name != "google" ||
		card.Reason != "This Agent uses each person's own Google account." {
		t.Fatalf("card = %#v", card)
	}
	// The worker leaves the Run to its loop, which parks it on the card at
	// step end.
	stored, err := store.GetAgentRun(context.Background(), scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != AgentRunStatusRunning || stored.FailureExplanation != nil {
		t.Fatalf("a parked call changed its Run: %s %#v", stored.Status, stored.FailureExplanation)
	}
	parker := newGovernedParker(store, nil, nil)
	wake, summary, err := parker.governedCardWait(context.Background(), stored, call)
	if err != nil || wake == nil || wake.Type != ConnectAccountWakeType || wake.Reference != card.ID || summary != connectAccountWaitSummary(card) {
		t.Fatalf("card wait = %#v %q %v", wake, summary, err)
	}

	// The person connects; the run resumes and the model asks again in a new
	// step for the same external operation.
	connected = true
	card.Status, card.ResolvedBy = "resolved", connectAccountTestUser
	card.ResolvedBindingID, card.ResolvedBindingRevision = "mail-binding", 1
	card.Revision++
	if err := store.SaveSkillSetupRequest(context.Background(), card, card.Revision-1); err != nil {
		t.Fatal(err)
	}
	if wake, _, err := parker.governedCardWait(context.Background(), stored, call); err != nil || wake != nil {
		t.Fatalf("a resolved card still parks: %#v %v", wake, err)
	}
	retry := proposePersonAction(t, store, catalog, scope, stored, now, "step-2", "send-1-retry", nil)
	if !retry.Created || retry.Call.ID == call.ID || retry.Call.Status != ActionCallStatusReady || retry.Call.DuplicateOfActionCallID != "" {
		t.Fatalf("retry not admitted: %#v", retry.Call)
	}
	result = executePersonAction(t, worker, scope, retry)
	if result.Call.ID != retry.Call.ID || result.Call.Status != ActionCallStatusSucceeded || dispatches != 1 {
		t.Fatalf("retry = %#v dispatches=%d", result.Call, dispatches)
	}
	last := resolutions[len(resolutions)-1]
	if last.ActingUser != connectAccountTestUser || last.Umbrella.ID != "release-agent" || last.Requirements["google"].OAuth2 == nil {
		t.Fatalf("resolution request = %#v", last)
	}
}
