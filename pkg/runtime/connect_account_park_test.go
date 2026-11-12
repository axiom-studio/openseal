package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionWorkerParksPersonGapOnCardAndRetries(t *testing.T) {
	store := conversationtest.NewStore(t)
	now := time.Now().UTC()
	catalog, scope := actiontest.PersonCatalog(t)
	run := actiontest.ClaimedPersonChatRun(t, store, scope, now, actiontest.ActingUser)
	proposal := proposeLoopPersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	if proposal.Call.ExternalOperationDigest == "" {
		t.Fatal("test needs an external operation")
	}
	connected := false
	var checks []action.CredentialAvailabilityRequest
	var resolutions []action.CredentialResolutionRequest
	dispatches := 0
	worker := action.NewActionWorker(store, catalog, action.CredentialResolverFunc(func(_ context.Context, req action.CredentialResolutionRequest) (map[string]string, error) {
		resolutions = append(resolutions, req)
		return map[string]string{"google": "token"}, nil
	}), action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		return map[string]interface{}{"sent": true}, nil
	}))
	worker.SetCredentialAvailabilityChecker(actiontest.AvailabilityFunc(func(_ context.Context, req action.CredentialAvailabilityRequest) (action.CredentialAvailability, error) {
		checks = append(checks, req)
		if connected {
			return action.CredentialAvailability{}, nil
		}
		return action.CredentialAvailability{Gaps: map[string]action.CredentialGap{"google": action.CredentialGapPersonSignIn}}, nil
	}))
	result := executeLoopPersonAction(t, worker, scope, proposal)
	call := result.Call
	if dispatches != 0 || call.Status != models.ActionCallStatusFailed || call.FailurePhase != models.ActionFailureBeforeDispatch ||
		!strings.HasPrefix(call.Error, "connect_account_required: ") || call.ExternalOperationDigest != "" || !skillbinding.IsParkedConnectAccountCall(call) {
		t.Fatalf("parked call = %#v", call)
	}
	if len(checks) != 1 || checks[0].ActingUser != actiontest.ActingUser || checks[0].Umbrella != (models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"}) ||
		checks[0].BindingID != "mail-binding" || checks[0].References["google"].ID != skill.PersonCredentialReferenceID || checks[0].Requirements["google"].OAuth2 == nil {
		t.Fatalf("availability request = %#v", checks)
	}
	card, err := store.GetSkillSetupRequest(context.Background(), scope, "connect-account:"+call.ID)
	if err != nil || card == nil {
		t.Fatalf("card = %#v, %v", card, err)
	}
	if card.Kind != models.SkillSetupKindConnectAccount || card.Mode != "per_person" || card.RequestedFor != actiontest.ActingUser || card.Status != "pending" ||
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
	if stored.Status != models.AgentRunStatusRunning || stored.FailureExplanation != nil {
		t.Fatalf("a parked call changed its Run: %s %#v", stored.Status, stored.FailureExplanation)
	}
	parker := newGovernedParker(store, nil, nil)
	wake, summary, err := parker.governedCardWait(context.Background(), stored, call)
	if err != nil || wake == nil || wake.Type != models.ConnectAccountWakeType || wake.Reference != card.ID || summary != skillbinding.ConnectAccountWaitSummary(card) {
		t.Fatalf("card wait = %#v %q %v", wake, summary, err)
	}

	// The person connects; the run resumes and the model asks again in a new
	// step for the same external operation.
	connected = true
	card.Status, card.ResolvedBy = "resolved", actiontest.ActingUser
	card.ResolvedBindingID, card.ResolvedBindingRevision = "mail-binding", 1
	card.Revision++
	if err := store.SaveSkillSetupRequest(context.Background(), card, card.Revision-1); err != nil {
		t.Fatal(err)
	}
	if wake, _, err := parker.governedCardWait(context.Background(), stored, call); err != nil || wake != nil {
		t.Fatalf("a resolved card still parks: %#v %v", wake, err)
	}
	retry := proposeLoopPersonAction(t, store, catalog, scope, stored, now, "step-2", "send-1-retry", nil)
	if !retry.Created || retry.Call.ID == call.ID || retry.Call.Status != models.ActionCallStatusReady || retry.Call.DuplicateOfActionCallID != "" {
		t.Fatalf("retry not admitted: %#v", retry.Call)
	}
	result = executeLoopPersonAction(t, worker, scope, retry)
	if result.Call.ID != retry.Call.ID || result.Call.Status != models.ActionCallStatusSucceeded || dispatches != 1 {
		t.Fatalf("retry = %#v dispatches=%d", result.Call, dispatches)
	}
	last := resolutions[len(resolutions)-1]
	if last.ActingUser != actiontest.ActingUser || last.Umbrella.ID != "release-agent" || last.Requirements["google"].OAuth2 == nil {
		t.Fatalf("resolution request = %#v", last)
	}
}
