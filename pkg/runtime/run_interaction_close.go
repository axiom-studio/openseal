package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
)

// TerminalRunInteractionCloser is the kernel actor recorded on interaction
// requests closed because their Run ended.
const TerminalRunInteractionCloser = "kernel:run-terminal"

// closeTerminalRunInteractionRequests dismisses the in-chat requests a
// terminal Run left open, so their cards leave the chat: every pending
// credential request (it only resumes its own Run, which can no longer
// continue) and, when the Run was canceled or failed, every pending Skill
// setup request. A completed foreground reply may legitimately finish with a
// setup form the user completes later, so those stay. Idempotent; a request
// the user settles concurrently keeps the user's outcome.
func closeTerminalRunInteractionRequests(ctx context.Context, store ActionStore, run *AgentRun) error {
	if store == nil || run == nil || !isTerminalAgentRunStatus(run.Status) {
		return nil
	}
	credentials, _ := store.(CredentialRequestStore)
	setups, _ := store.(SkillSetupRequestStore)
	closeSetups := setups != nil && (run.Status == AgentRunStatusCanceled || run.Status == AgentRunStatusFailed)
	if credentials == nil && !closeSetups {
		return nil
	}
	calls, err := store.ListActionCalls(ctx, ActionFilter{Scope: run.Scope, RunID: run.ID, Status: []ActionCallStatus{ActionCallStatusSucceeded}, Limit: 1000})
	if err != nil {
		return err
	}
	var errs []error
	for _, call := range calls {
		if call == nil || call.SkillID != SkillManagementSkillID {
			continue
		}
		switch {
		case call.Action == SkillActionRequestCredential && credentials != nil:
			errs = append(errs, dismissRunCredentialRequest(ctx, credentials, run, call))
		case call.Action == SkillActionRequestSetup && closeSetups:
			errs = append(errs, dismissRunSkillSetupRequest(ctx, setups, run, call))
		}
	}
	return errors.Join(errs...)
}

func interactionRequestID(call *ActionCall, key string) string {
	value, _ := call.Output[key].(map[string]interface{})
	id, _ := value["id"].(string)
	return strings.TrimSpace(id)
}

func dismissRunCredentialRequest(ctx context.Context, store CredentialRequestStore, run *AgentRun, call *ActionCall) error {
	id := interactionRequestID(call, "credentialRequest")
	if id == "" {
		return nil
	}
	current, err := store.GetCredentialRequest(ctx, run.Scope, id)
	if err != nil || current == nil || current.RunID != run.ID || current.Status != CredentialRequestStatusPending {
		return err
	}
	expected := current.Revision
	current.Status, current.ResolvedBy = CredentialRequestStatusDismissed, TerminalRunInteractionCloser
	current.Revision++
	current.UpdatedAt = time.Now().UTC()
	if err := store.SaveCredentialRequest(ctx, current, expected); errors.Is(err, ErrCredentialRequestConflict) {
		return nil
	} else {
		return err
	}
}

func dismissRunSkillSetupRequest(ctx context.Context, store SkillSetupRequestStore, run *AgentRun, call *ActionCall) error {
	id := interactionRequestID(call, "setupRequest")
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
	if err := store.SaveSkillSetupRequest(ctx, current, expected); errors.Is(err, ErrSkillSetupConflict) {
		return nil
	} else {
		return err
	}
}
