package agentrun

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// TerminalRunInteractionCloser is the kernel actor recorded on interaction
// requests closed because their Run ended.
const TerminalRunInteractionCloser = "kernel:run-terminal"

// closeTerminalRunInteractionRequests dismisses the in-chat requests a
// terminal Run left open, so their cards leave the chat: every pending
// credential request (it only resumes its own Run, which can no longer
// continue) and, when the Run was canceled, every pending Skill setup request.
// A setup form outlives a Run that completed or failed: connecting the app is
// still what the person needs, and its receipt starts a fresh Run. A failure
// after the form went up (a model error, a restart) used to close it before
// the person could fill it in. Idempotent; a request the user settles
// concurrently keeps the user's outcome.
func closeTerminalRunInteractionRequests(ctx context.Context, store models.ActionStore, run *models.AgentRun) error {
	if store == nil || run == nil || !models.IsTerminalAgentRunStatus(run.Status) {
		return nil
	}
	credentials, _ := store.(models.CredentialRequestStore)
	setups, _ := store.(models.SkillSetupRequestStore)
	closeSetups := setups != nil && run.Status == models.AgentRunStatusCanceled
	if credentials == nil && !closeSetups {
		return nil
	}
	calls, err := store.ListActionCalls(ctx, models.ActionFilter{Scope: run.Scope, RunID: run.ID, Status: []models.ActionCallStatus{models.ActionCallStatusSucceeded}, Limit: 1000})
	if err != nil {
		return err
	}
	var errs []error
	for _, call := range calls {
		if call == nil || call.SkillID != models.SkillManagementSkillID {
			continue
		}
		switch {
		case call.Action == SkillActionRequestCredential && credentials != nil:
			errs = append(errs, dismissRunCredentialRequest(ctx, credentials, run, call))
		case (call.Action == SkillActionRequestSetup || call.Action == SkillActionRequestConnection) && closeSetups:
			errs = append(errs, dismissRunSkillSetupRequest(ctx, setups, run, call))
		}
	}
	return errors.Join(errs...)
}

func InteractionRequestID(call *models.ActionCall, key string) string {
	value, _ := call.Output[key].(map[string]interface{})
	id, _ := value["id"].(string)
	return strings.TrimSpace(id)
}

func dismissRunCredentialRequest(ctx context.Context, store models.CredentialRequestStore, run *models.AgentRun, call *models.ActionCall) error {
	id := InteractionRequestID(call, "credentialRequest")
	if id == "" {
		return nil
	}
	current, err := store.GetCredentialRequest(ctx, run.Scope, id)
	if err != nil || current == nil || current.RunID != run.ID || current.Status != models.CredentialRequestStatusPending {
		return err
	}
	expected := current.Revision
	current.Status, current.ResolvedBy = models.CredentialRequestStatusDismissed, TerminalRunInteractionCloser
	current.Revision++
	current.UpdatedAt = time.Now().UTC()
	if err := store.SaveCredentialRequest(ctx, current, expected); errors.Is(err, models.ErrCredentialRequestConflict) {
		return nil
	} else {
		return err
	}
}

func dismissRunSkillSetupRequest(ctx context.Context, store models.SkillSetupRequestStore, run *models.AgentRun, call *models.ActionCall) error {
	id := InteractionRequestID(call, "setupRequest")
	if id == "" {
		return nil
	}
	current, err := store.GetSkillSetupRequest(ctx, run.Scope, id)
	if err != nil || current == nil || current.RunID != run.ID || current.Status != "pending" {
		return err
	}
	expected := current.Revision
	current.Status, current.ResolvedBy = "dismissed", TerminalRunInteractionCloser
	current.Revision++
	current.UpdatedAt = time.Now().UTC()
	if err := store.SaveSkillSetupRequest(ctx, current, expected); errors.Is(err, models.ErrSkillSetupConflict) {
		return nil
	} else {
		return err
	}
}
