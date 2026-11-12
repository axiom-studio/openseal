package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type ActionExecutionCatalog interface {
	Resolve(context.Context, skill.ScopeReference, string, string, string, string, ...skill.BindingReference) (*skill.BoundAction, error)
	ValidateInput(context.Context, *skill.BoundAction, map[string]interface{}) error
	ValidateOutput(context.Context, *skill.BoundAction, map[string]interface{}) error
}

type ActionDispatchInput struct {
	Call *ActionCall
	// Run is the durable, kernel-owned execution context for the call. It is
	// deliberately unavailable to model arguments. Dispatchers use it to keep
	// authority ownership (for example, a Team Skill binding) separate from the
	// roster Agent that supplies execution placement.
	Run         *AgentRun
	Bound       *skill.BoundAction
	Arguments   map[string]interface{}
	Credentials map[string]string
	// CredentialLease and CredentialReferences are the remote-resolution path.
	// They are mutually exclusive with plaintext Credentials.
	CredentialLease      *SignedActionCredentialLease
	CredentialReferences map[string]skill.CredentialReference
}

type ActionDispatcher interface {
	DispatchAction(context.Context, ActionDispatchInput) (map[string]interface{}, error)
}

type ActionProposalValidationInput struct {
	Run       *AgentRun
	Bound     *skill.BoundAction
	Arguments map[string]interface{}
	// History reads this Run's governed action calls. nil means none.
	History RunActionHistory
	// IdempotencyKey is the proposed call's key, trimmed. A validator may
	// derive the ID of what the call creates from it (create_task's
	// activation), so the approval names that resource before it exists.
	IdempotencyKey string
}

func actionBudgetReservationID(actionID string) string {
	return "action:" + actionID
}
