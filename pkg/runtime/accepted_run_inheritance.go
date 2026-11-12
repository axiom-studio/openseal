package runtime

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

// prepareAcceptedChildRun separates continuation of an already accepted method
// from admission of a new method. Caller context is never an execution pin.
func prepareAcceptedChildRun(ctx context.Context, parent *models.AgentRun, request *models.CreateAgentRunRequest, preparer models.AcceptedRunExecutionPreparer, inherit bool) error {
	if parent == nil || request == nil || request.Scope != parent.Scope {
		return models.ErrAcceptedRunExecution
	}
	request.Context = maps.Clone(request.Context)
	delete(request.Context, models.AcceptedRunExecutionContextKey)
	if request.Entrypoint == "" {
		return nil
	}
	if inherit {
		pin, err := models.AcceptedRunExecutionForRun(parent)
		if err != nil || pin == nil || request.AssignedAgentID != parent.AssignedAgentID || request.Entrypoint != parent.Entrypoint {
			return models.ErrAcceptedRunExecution
		}
		if request.Context == nil {
			request.Context = make(map[string]interface{})
		}
		// AcceptedRunExecutionForRun decoded a fresh value, including its exact
		// dependency slice, so siblings cannot mutate each other's identity.
		request.Context[models.AcceptedRunExecutionContextKey] = pin
		request.Context["runbookDefinitionId"] = pin.RunbookID
		request.Context["runbookDefinitionVersion"] = pin.RunbookVersion
		request.Context["runbookEntrypoint"] = pin.Entrypoint
		return nil
	}
	if preparer == nil {
		return fmt.Errorf("%w: child method admission is not configured", models.ErrAcceptedRunExecution)
	}
	scope, assignedAgentID, entrypoint := request.Scope, request.AssignedAgentID, request.Entrypoint
	if err := preparer.PrepareAcceptedRunExecution(ctx, request); err != nil {
		return err
	}
	if request.Scope != scope || request.AssignedAgentID != assignedAgentID || request.Entrypoint != entrypoint {
		return models.ErrAcceptedRunExecution
	}
	pin, err := models.AcceptedRunExecutionForRun(&models.AgentRun{Scope: scope, AssignedAgentID: assignedAgentID, Entrypoint: entrypoint, Context: request.Context})
	if err != nil || pin == nil {
		return models.ErrAcceptedRunExecution
	}
	// Store the validated copy, rather than retaining preparer-owned pointers.
	request.Context = maps.Clone(request.Context)
	request.Context[models.AcceptedRunExecutionContextKey] = pin
	return nil
}

// requestedAcceptedChildEntrypoint reads only the requested target method.
// triggerInput describes the source method and cannot select a target method.
func requestedAcceptedChildEntrypoint(values map[string]interface{}) (string, error) {
	entrypoint := ""
	read := func(value interface{}) error {
		text, ok := value.(string)
		if !ok || !models.IsValidOpaqueIdentifier(text, 128) || text != strings.TrimSpace(text) || entrypoint != "" && entrypoint != text {
			return models.ErrAcceptedRunExecution
		}
		entrypoint = text
		return nil
	}
	if value, supplied := values["runbookEntrypoint"]; supplied {
		if err := read(value); err != nil {
			return "", err
		}
	}
	if raw, supplied := values[RunbookInvocationContextKey]; supplied {
		invocation, ok := raw.(map[string]interface{})
		if !ok {
			return "", models.ErrAcceptedRunExecution
		}
		if value, supplied := invocation["entrypoint"]; supplied {
			if err := read(value); err != nil {
				return "", err
			}
		}
	}
	mode := ""
	switch value := values[DelegationModeContextKey].(type) {
	case string:
		mode = value
	case runbook.DelegateMode:
		mode = string(value)
	}
	if mode == string(runbook.DelegateReason) && entrypoint != "" {
		return "", models.ErrAcceptedRunExecution
	}
	return entrypoint, nil
}

// SetAcceptedRunExecutionPreparer configures authoritative admission for new
// child methods. Configure before starting workers; same-method continuations
// inherit the already accepted immutable snapshot independently.
func (c *RunForkCoordinator) SetAcceptedRunExecutionPreparer(preparer models.AcceptedRunExecutionPreparer) {
	if c != nil {
		c.executionPreparer = preparer
	}
}
