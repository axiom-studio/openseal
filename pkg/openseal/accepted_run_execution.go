package openseal

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// PrepareAcceptedRunExecution replaces any request-supplied continuation pin
// with the immutable method resolved through this tenant's active deployment.
// Scheduled and manual Run creators use the same authoritative boundary.
func (e *Engine) PrepareAcceptedRunExecution(ctx context.Context, req *runtime.CreateAgentRunRequest) error {
	if e == nil || req == nil {
		return runtime.ErrAcceptedRunExecution
	}
	req.Context = maps.Clone(req.Context)
	delete(req.Context, runtime.AcceptedRunExecutionContextKey)
	entrypoint := strings.TrimSpace(req.Entrypoint)
	if entrypoint == "" {
		return nil
	}
	if req.Scope.Validate() != nil || req.Owner.Validate() != nil {
		return runtime.ErrAcceptedRunExecution
	}
	deployment, err := e.GetAgentDeployment(ctx, skill.ScopeReference(req.Scope), strings.TrimSpace(req.AssignedAgentID))
	if err != nil || deployment == nil {
		return fmt.Errorf("resolve accepted Run method: deployment is unavailable")
	}
	definition, err := e.GetAgentDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition == nil || definition.Runbook == nil {
		return fmt.Errorf("resolve accepted Run method: active immutable definition is unavailable")
	}
	// An activation's previously reviewed exact method must not silently become
	// the active deployment's different method when a new occurrence starts.
	_, idSupplied := req.Context["runbookDefinitionId"]
	_, versionSupplied := req.Context["runbookDefinitionVersion"]
	if idSupplied != versionSupplied {
		return runtime.ErrAcceptedRunExecution
	}
	if id, present := req.Context["runbookDefinitionId"]; present && id != definition.Runbook.ID {
		return runtime.ErrAcceptedRunExecution
	}
	if version, present := req.Context["runbookDefinitionVersion"]; present && version != definition.Runbook.Version {
		return runtime.ErrAcceptedRunExecution
	}
	if raw, present := req.Plan["runbook"]; present {
		plan, ok := raw.(map[string]interface{})
		if !ok || plan["id"] != definition.Runbook.ID || plan["version"] != definition.Runbook.Version {
			return runtime.ErrAcceptedRunExecution
		}
	}
	pin, err := runtime.NewAcceptedRunExecution(req.Scope, deployment, definition, entrypoint)
	if err != nil {
		return err
	}
	pin.DerivedRunbookIdentity = !idSupplied && !versionSupplied
	if req.Context == nil {
		req.Context = make(map[string]interface{})
	}
	req.Entrypoint = entrypoint
	req.Context[runtime.AcceptedRunExecutionContextKey] = pin
	req.Context["runbookDefinitionId"], req.Context["runbookDefinitionVersion"] = pin.RunbookID, pin.RunbookVersion
	return nil
}

var _ runtime.AcceptedRunExecutionPreparer = (*Engine)(nil)
