package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// connectAccountParked holds a call before dispatch until someone connects
// the account its card asks for.
type connectAccountParked struct{ request *models.SkillSetupRequest }

func (e *connectAccountParked) Error() string {
	return skillbinding.ConnectAccountRequiredPrefix + e.request.Reason
}

// credentialAvailabilityBox lets a host install its checker after workers
// exist.
type credentialAvailabilityBox struct{ checker CredentialAvailabilityChecker }

// SetCredentialAvailabilityChecker installs the host's pre-dispatch
// credential check on a worker used outside a pool (the inline loop worker).
func (w *ActionWorker) SetCredentialAvailabilityChecker(checker CredentialAvailabilityChecker) {
	w.setCredentialAvailabilityChecker(checker)
}

func (w *ActionWorker) setCredentialAvailabilityChecker(checker CredentialAvailabilityChecker) {
	if w != nil {
		w.availability.Store(&credentialAvailabilityBox{checker: checker})
	}
}

func (w *ActionWorker) credentialAvailabilityChecker() CredentialAvailabilityChecker {
	if box := w.availability.Load(); box != nil {
		return box.checker
	}
	return nil
}

// checkCredentialAvailability is the pre-dispatch gate for an action with
// credentials. It returns the run's umbrella, or the failure to persist: a
// plain error, a person_unavailable failure, or a connect_account park.
func (w *ActionWorker) checkCredentialAvailability(ctx context.Context, run *models.AgentRun, call *models.ActionCall, bound *skill.BoundAction) (models.ObjectiveOwner, error) {
	umbrella, err := models.RunUmbrella(ctx, w.teams, run)
	if err != nil {
		if !errors.Is(err, models.ErrRunUmbrellaUnavailable) {
			err = fmt.Errorf("%w: %v", models.ErrRunUmbrellaUnavailable, err)
		}
		return models.ObjectiveOwner{}, err
	}
	if skillbinding.CallUsesPersonCredential(call) && run.ActingUser == "" {
		return umbrella, skillbinding.NewPersonUnavailableError(bound, umbrella)
	}
	checker := w.credentialAvailabilityChecker()
	if checker == nil {
		return umbrella, nil
	}
	availability, err := checker.CheckCredentialAvailability(ctx, CredentialAvailabilityRequest{
		Scope: call.Scope, Umbrella: umbrella, ActingUser: run.ActingUser, DeploymentID: call.DeploymentID, BindingID: call.BindingID,
		References: maps.Clone(call.CredentialRefs), Requirements: skillbinding.ActionCredentialRequirements(bound),
	})
	if err != nil {
		return umbrella, err
	}
	var person, shared []string
	for slot, gap := range availability.Gaps {
		switch gap {
		case CredentialGapNoPerson:
			return umbrella, skillbinding.NewPersonUnavailableError(bound, umbrella)
		case CredentialGapPersonSignIn:
			person = append(person, slot)
		case CredentialGapSharedSignIn:
			shared = append(shared, slot)
		default:
			return umbrella, fmt.Errorf("credential availability reported an unknown gap for %s", slot)
		}
	}
	mode, slots := "", []string(nil)
	switch {
	case len(person) > 0:
		mode, slots = models.SkillSetupModePerPerson, person
	case len(shared) > 0:
		mode, slots = models.SkillSetupModeShared, shared
	default:
		return umbrella, nil
	}
	request, err := skillbinding.CreateConnectAccountRequest(ctx, w.store, run, call, bound, umbrella, skillbinding.ConnectAccountSlots(bound, slots), mode)
	if err != nil {
		return umbrella, err
	}
	return umbrella, &connectAccountParked{request: request}
}
