package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const AcceptedRunExecutionContextKey = "acceptedRunExecution"
const MaximumAcceptedRunSkillDependencies = 1024
const maximumAcceptedRunbookSteps = 4096

var ErrAcceptedRunExecution = errors.New("accepted Run execution identity is unavailable or invalid")

// AcceptedRunExecution is server-authored continuation identity, not a grant.
// Credentials, current live bindings and action policy remain independently
// authoritative when an accepted method resumes.
type AcceptedRunExecution struct {
	Scope                  Scope                   `json:"scope"`
	DeploymentID           string                  `json:"deploymentId"`
	DefinitionID           string                  `json:"definitionId"`
	DefinitionVersion      string                  `json:"definitionVersion"`
	RunbookID              string                  `json:"runbookId"`
	RunbookVersion         string                  `json:"runbookVersion"`
	Entrypoint             string                  `json:"entrypoint"`
	SkillDependencies      []SkillRuntimeReference `json:"skillDependencies"`
	DerivedRunbookIdentity bool                    `json:"derivedRunbookIdentity,omitempty"`
}

type AcceptedRunExecutionPreparer interface {
	PrepareAcceptedRunExecution(context.Context, *CreateAgentRunRequest) error
}

func AcceptedRunExecutionForRun(run *AgentRun) (*AcceptedRunExecution, error) {
	if run == nil {
		return nil, ErrAcceptedRunExecution
	}
	raw, present := run.Context[AcceptedRunExecutionContextKey]
	if !present {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, ErrAcceptedRunExecution
	}
	var pin AcceptedRunExecution
	if err := json.Unmarshal(encoded, &pin); err != nil {
		return nil, ErrAcceptedRunExecution
	}
	if pin.Scope != run.Scope || pin.DeploymentID != run.AssignedAgentID || pin.Entrypoint != run.Entrypoint || pin.Scope.Validate() != nil || len(pin.SkillDependencies) > MaximumAcceptedRunSkillDependencies {
		return nil, ErrAcceptedRunExecution
	}
	for _, value := range []string{pin.DeploymentID, pin.DefinitionID, pin.DefinitionVersion, pin.RunbookID, pin.RunbookVersion, pin.Entrypoint} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || strings.ContainsAny(value, "\r\n\t") {
			return nil, ErrAcceptedRunExecution
		}
	}
	seen := map[SkillRuntimeReference]bool{}
	for _, dependency := range pin.SkillDependencies {
		if (SkillRuntimeUsageFilter{Scope: pin.Scope, SkillID: dependency.SkillID, SkillVersion: dependency.SkillVersion}).Validate() != nil || seen[dependency] {
			return nil, ErrAcceptedRunExecution
		}
		seen[dependency] = true
	}
	return &pin, nil
}

// NewAcceptedRunExecution traverses only the typed immutable method graph,
// including alternate branches and loop bodies. It never scans arbitrary input,
// previous Runs or mutable model plans for executable identity.
func NewAcceptedRunExecution(scope Scope, deployment *kernelagent.AgentDeployment, definition *kernelagent.AgentDefinition, entrypoint string) (*AcceptedRunExecution, error) {
	if deployment == nil || definition == nil || definition.Runbook == nil || deployment.Scope != skill.ScopeReference(scope) || deployment.ID == "" || definition.ID != deployment.DefinitionID || definition.Version != deployment.ActiveVersion {
		return nil, ErrAcceptedRunExecution
	}
	method := definition.Runbook
	start, exists := method.Entrypoints[entrypoint]
	if !exists || len(method.Steps) > maximumAcceptedRunbookSteps {
		return nil, ErrAcceptedRunExecution
	}
	pin := &AcceptedRunExecution{Scope: scope, DeploymentID: deployment.ID, DefinitionID: definition.ID, DefinitionVersion: definition.Version, RunbookID: method.ID, RunbookVersion: method.Version, Entrypoint: entrypoint, SkillDependencies: []SkillRuntimeReference{}}
	visited := make(map[string]bool)
	queued := map[string]bool{start: true}
	dependencies := make(map[SkillRuntimeReference]bool)
	pending := []string{start}
	edges := 0
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[id] {
			continue
		}
		visited[id] = true
		step, ok := method.Steps[id]
		if !ok {
			return nil, fmt.Errorf("%w: method step %s is missing", ErrAcceptedRunExecution, id)
		}
		if step.Kind == runbook.StepAction && step.Action != nil {
			dependency := SkillRuntimeReference{SkillID: step.Action.SkillID, SkillVersion: step.Action.SkillVersion}
			dependencies[dependency] = true
			if len(dependencies) > MaximumAcceptedRunSkillDependencies {
				return nil, ErrAcceptedRunExecution
			}
		}
		if (step.Decision != nil && len(step.Decision.Cases) > maximumAcceptedRunbookSteps) || (step.Fork != nil && len(step.Fork.Branches) > maximumAcceptedRunbookSteps) {
			return nil, ErrAcceptedRunExecution
		}
		successors := acceptedRunbookSuccessors(method, step)
		edges += len(successors)
		if edges > maximumAcceptedRunbookSteps*4 {
			return nil, ErrAcceptedRunExecution
		}
		for _, successor := range successors {
			if !queued[successor] {
				queued[successor] = true
				pending = append(pending, successor)
			}
		}
	}
	for dependency := range dependencies {
		pin.SkillDependencies = append(pin.SkillDependencies, dependency)
	}
	sort.Slice(pin.SkillDependencies, func(i, j int) bool {
		a, b := pin.SkillDependencies[i], pin.SkillDependencies[j]
		if a.SkillID != b.SkillID {
			return a.SkillID < b.SkillID
		}
		return a.SkillVersion < b.SkillVersion
	})
	probe := &AgentRun{Scope: scope, AssignedAgentID: deployment.ID, Entrypoint: entrypoint, Context: map[string]interface{}{AcceptedRunExecutionContextKey: pin}}
	if _, err := AcceptedRunExecutionForRun(probe); err != nil {
		return nil, err
	}
	return pin, nil
}

func acceptedRunbookSuccessors(method *runbook.Definition, step runbook.Step) []string {
	switch step.Kind {
	case runbook.StepAction:
		if step.Action != nil {
			return []string{step.Action.Next}
		}
	case runbook.StepDelegate:
		if step.Delegate != nil {
			return []string{step.Delegate.Next}
		}
	case runbook.StepTransform:
		if step.Transform != nil {
			return []string{step.Transform.Next}
		}
	case runbook.StepWait:
		if step.Wait != nil {
			out := []string{step.Wait.Next}
			if step.Wait.TimeoutNext != "" {
				out = append(out, step.Wait.TimeoutNext)
			}
			return out
		}
	case runbook.StepDecision:
		if step.Decision != nil {
			out := []string{}
			for _, c := range step.Decision.Cases {
				out = append(out, c.Next)
			}
			if step.Decision.Default != "" {
				out = append(out, step.Decision.Default)
			}
			return out
		}
	case runbook.StepFork:
		if step.Fork != nil {
			out := []string{step.Fork.Join}
			for _, target := range step.Fork.Branches {
				out = append(out, target)
			}
			return out
		}
	case runbook.StepJoin:
		if step.Join != nil {
			return []string{step.Join.Next}
		}
	case runbook.StepForEach:
		if step.ForEach != nil {
			return []string{step.ForEach.Body, step.ForEach.Next}
		}
	case runbook.StepLoopReturn:
		if step.LoopReturn != nil {
			if parent, ok := method.Steps[step.LoopReturn.ForEach]; ok && parent.ForEach != nil {
				return []string{parent.ForEach.Next}
			}
		}
	}
	return nil
}
