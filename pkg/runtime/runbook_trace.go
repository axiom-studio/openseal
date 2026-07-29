package runtime

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

const (
	runbookTraceCheckpointKey = "runbookTrace"
	runbookTraceRevision      = 1
	maximumRunbookTraceVisits = 16384
)

type RunbookStepTraceStatus string

type RunbookDataAvailability string

const (
	RunbookStepTraceRunning   RunbookStepTraceStatus = "running"
	RunbookStepTraceWaiting   RunbookStepTraceStatus = "waiting"
	RunbookStepTraceSucceeded RunbookStepTraceStatus = "succeeded"
	RunbookStepTraceFailed    RunbookStepTraceStatus = "failed"

	RunbookDataAvailable RunbookDataAvailability = "available"
	RunbookDataMissing   RunbookDataAvailability = "missing"
)

// RunbookDataObservation records what an operator may safely know about a
// referenced value at the instant a node reads or writes it. Values, object
// keys, digests, and content never enter the trace; presence and coarse shape
// are sufficient to explain whether data actually reached the node.
type RunbookDataObservation struct {
	Ref          string                  `json:"ref"`
	Availability RunbookDataAvailability `json:"availability"`
	Shape        string                  `json:"shape,omitempty"`
	Count        int                     `json:"count,omitempty"`
}

// RunbookStepTrace is the credential-free, operator-facing record of one
// visit to one immutable Runbook node. InputRefs and OutputRefs describe data
// lineage by JSON Pointer; values remain in their governed stores and are not
// copied into the audit trail.
type RunbookStepTrace struct {
	Sequence     int64                    `json:"sequence"`
	StepID       string                   `json:"stepId"`
	StepKind     runbook.StepKind         `json:"stepKind"`
	StepName     string                   `json:"stepName,omitempty"`
	Visit        int                      `json:"visit"`
	Status       RunbookStepTraceStatus   `json:"status"`
	TurnID       string                   `json:"turnId,omitempty"`
	InputRefs    []string                 `json:"inputRefs,omitempty"`
	OutputRefs   []string                 `json:"outputRefs,omitempty"`
	Inputs       []RunbookDataObservation `json:"inputs,omitempty"`
	Outputs      []RunbookDataObservation `json:"outputs,omitempty"`
	SelectedNext string                   `json:"selectedNext,omitempty"`
	ActionCallID string                   `json:"actionCallId,omitempty"`
	ApprovalID   string                   `json:"approvalId,omitempty"`
	Summary      string                   `json:"summary,omitempty"`
	Error        string                   `json:"error,omitempty"`
	StartedAt    time.Time                `json:"startedAt"`
	CompletedAt  *time.Time               `json:"completedAt,omitempty"`
}

type RunbookExecutionTrace struct {
	Revision     int                `json:"revision"`
	NextSequence int64              `json:"nextSequence"`
	Entries      []RunbookStepTrace `json:"entries"`
}

func (t *RunbookExecutionTrace) Validate() error {
	if t == nil || t.Revision != runbookTraceRevision || t.NextSequence < 1 || len(t.Entries) > maximumRunbookTraceVisits {
		return errors.New("Runbook execution trace metadata is invalid")
	}
	last := int64(0)
	for _, entry := range t.Entries {
		if entry.Sequence <= last || strings.TrimSpace(entry.StepID) == "" || entry.Visit < 1 || entry.StartedAt.IsZero() {
			return errors.New("Runbook execution trace entry is invalid")
		}
		if err := validateRunbookDataObservations(entry.Inputs); err != nil {
			return err
		}
		if err := validateRunbookDataObservations(entry.Outputs); err != nil {
			return err
		}
		if !runbookObservationRefsMatch(entry.InputRefs, entry.Inputs) || !runbookObservationRefsMatch(entry.OutputRefs, entry.Outputs) {
			return errors.New("Runbook data observations do not match the node contract")
		}
		switch entry.Status {
		case RunbookStepTraceRunning, RunbookStepTraceWaiting:
			if entry.CompletedAt != nil {
				return errors.New("active Runbook trace entry cannot be completed")
			}
		case RunbookStepTraceSucceeded, RunbookStepTraceFailed:
			if entry.CompletedAt == nil || entry.CompletedAt.Before(entry.StartedAt) {
				return errors.New("terminal Runbook trace entry requires a valid completion time")
			}
		default:
			return errors.New("Runbook execution trace status is invalid")
		}
		last = entry.Sequence
	}
	if t.NextSequence <= last {
		return errors.New("Runbook execution trace sequence is invalid")
	}
	return nil
}

// RunbookTraceFromCheckpoint returns a detached validated trace projection.
func RunbookTraceFromCheckpoint(checkpoint map[string]interface{}) (*RunbookExecutionTrace, error) {
	trace := &RunbookExecutionTrace{Revision: runbookTraceRevision, NextSequence: 1, Entries: []RunbookStepTrace{}}
	if checkpoint == nil || checkpoint[runbookTraceCheckpointKey] == nil {
		return trace, nil
	}
	encoded, err := json.Marshal(checkpoint[runbookTraceCheckpointKey])
	if err != nil {
		return nil, errors.New("decode Runbook execution trace")
	}
	if err := json.Unmarshal(encoded, trace); err != nil {
		return nil, errors.New("decode Runbook execution trace")
	}
	if err := trace.Validate(); err != nil {
		return nil, err
	}
	return trace, nil
}

func encodeRunbookTrace(checkpoint map[string]interface{}, trace *RunbookExecutionTrace) error {
	if checkpoint == nil || trace == nil {
		return errors.New("Runbook trace checkpoint is required")
	}
	if err := trace.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		return err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return err
	}
	checkpoint[runbookTraceCheckpointKey] = raw
	return nil
}

func beginRunbookStepTrace(checkpoint map[string]interface{}, definition *runbook.Definition, stepID, turnID string, now time.Time) (int64, error) {
	trace, err := RunbookTraceFromCheckpoint(checkpoint)
	if err != nil {
		return 0, err
	}
	if len(trace.Entries) >= maximumRunbookTraceVisits {
		return 0, errors.New("Runbook execution trace visit limit exceeded")
	}
	step, ok := definition.Steps[stepID]
	if !ok {
		return 0, errors.New("Runbook trace step is not defined")
	}
	visit := 1
	for _, entry := range trace.Entries {
		if entry.StepID == stepID && entry.Visit >= visit {
			visit = entry.Visit + 1
		}
	}
	sequence := trace.NextSequence
	trace.NextSequence++
	trace.Entries = append(trace.Entries, RunbookStepTrace{
		Sequence: sequence, StepID: stepID, StepKind: step.Kind, StepName: strings.TrimSpace(step.Name), Visit: visit,
		Status: RunbookStepTraceRunning, TurnID: strings.TrimSpace(turnID), InputRefs: runbookStepInputRefs(step),
		OutputRefs: runbookStepOutputRefs(stepID, step), StartedAt: now.UTC(),
	})
	entry := &trace.Entries[len(trace.Entries)-1]
	entry.Inputs = observeRunbookData(checkpoint, entry.InputRefs)
	entry.Outputs = observeRunbookData(checkpoint, entry.OutputRefs)
	return sequence, encodeRunbookTrace(checkpoint, trace)
}

func updateRunbookStepTrace(checkpoint map[string]interface{}, sequence int64, status RunbookStepTraceStatus, next, summary, runError string, metadata map[string]string, now time.Time) error {
	trace, err := RunbookTraceFromCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	for index := range trace.Entries {
		entry := &trace.Entries[index]
		if entry.Sequence != sequence {
			continue
		}
		entry.Status = status
		entry.SelectedNext = strings.TrimSpace(next)
		entry.Summary = strings.TrimSpace(summary)
		entry.Error = strings.TrimSpace(runError)
		entry.ActionCallID = strings.TrimSpace(metadata["actionCallId"])
		entry.ApprovalID = strings.TrimSpace(metadata["approvalId"])
		entry.Outputs = observeRunbookData(checkpoint, entry.OutputRefs)
		if status == RunbookStepTraceSucceeded || status == RunbookStepTraceFailed {
			completed := now.UTC()
			entry.CompletedAt = &completed
		} else {
			entry.CompletedAt = nil
		}
		return encodeRunbookTrace(checkpoint, trace)
	}
	return errors.New("Runbook trace entry is not found")
}

func replaceRunbookStepTraceOutputs(checkpoint map[string]interface{}, sequence int64, outputs []RunbookDataObservation) error {
	if err := validateRunbookDataObservations(outputs); err != nil {
		return err
	}
	trace, err := RunbookTraceFromCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	for index := range trace.Entries {
		if trace.Entries[index].Sequence != sequence {
			continue
		}
		trace.Entries[index].Outputs = append([]RunbookDataObservation(nil), outputs...)
		return encodeRunbookTrace(checkpoint, trace)
	}
	return errors.New("Runbook trace entry is not found")
}

func observeRunbookData(checkpoint map[string]interface{}, refs []string) []RunbookDataObservation {
	observations := make([]RunbookDataObservation, 0, len(refs))
	for _, ref := range refs {
		observation := RunbookDataObservation{Ref: ref, Availability: RunbookDataMissing}
		value, err := getRunbookPointer(checkpoint, ref)
		if err == nil {
			observation = observeRunbookValue(ref, value)
		}
		observations = append(observations, observation)
	}
	return observations
}

func observeRunbookValue(ref string, value interface{}) RunbookDataObservation {
	observation := RunbookDataObservation{Ref: ref, Availability: RunbookDataAvailable}
	switch typed := value.(type) {
	case nil:
		observation.Shape = "null"
	case map[string]interface{}:
		observation.Shape = "object"
		observation.Count = len(typed)
	case []interface{}:
		observation.Shape = "array"
		observation.Count = len(typed)
	case string:
		observation.Shape = "string"
	case bool:
		observation.Shape = "boolean"
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		observation.Shape = "number"
	default:
		observation.Shape = "value"
	}
	return observation
}

func validateRunbookDataObservations(values []RunbookDataObservation) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value.Ref) == "" || value.Count < 0 {
			return errors.New("Runbook data observation is invalid")
		}
		if _, exists := seen[value.Ref]; exists {
			return errors.New("Runbook data observation is duplicated")
		}
		seen[value.Ref] = struct{}{}
		switch value.Availability {
		case RunbookDataAvailable:
			if strings.TrimSpace(value.Shape) == "" {
				return errors.New("available Runbook data observation requires a shape")
			}
		case RunbookDataMissing:
			if value.Shape != "" || value.Count != 0 {
				return errors.New("missing Runbook data observation cannot describe a value")
			}
		default:
			return errors.New("Runbook data observation availability is invalid")
		}
	}
	return nil
}

func runbookObservationRefsMatch(refs []string, observations []RunbookDataObservation) bool {
	// Revision-one traces persisted before observations existed remain readable;
	// once observations are present they must cover the declared refs exactly.
	if len(observations) == 0 {
		return true
	}
	observed := make([]string, 0, len(observations))
	for _, observation := range observations {
		observed = append(observed, observation.Ref)
	}
	return reflect.DeepEqual(uniqueSortedTraceRefs(refs), uniqueSortedTraceRefs(observed))
}

func pendingRunbookStepTrace(checkpoint map[string]interface{}, stepID string) (int64, error) {
	trace, err := RunbookTraceFromCheckpoint(checkpoint)
	if err != nil {
		return 0, err
	}
	for index := len(trace.Entries) - 1; index >= 0; index-- {
		entry := trace.Entries[index]
		if entry.StepID == stepID && (entry.Status == RunbookStepTraceRunning || entry.Status == RunbookStepTraceWaiting) {
			return entry.Sequence, nil
		}
	}
	return 0, errors.New("pending Runbook trace entry is not found")
}

func runbookTraceDelta(before, after map[string]interface{}) []RunbookStepTrace {
	previous, previousErr := RunbookTraceFromCheckpoint(before)
	current, currentErr := RunbookTraceFromCheckpoint(after)
	if previousErr != nil || currentErr != nil {
		return nil
	}
	bySequence := make(map[int64]RunbookStepTrace, len(previous.Entries))
	for _, entry := range previous.Entries {
		bySequence[entry.Sequence] = entry
	}
	delta := make([]RunbookStepTrace, 0)
	for _, entry := range current.Entries {
		if prior, exists := bySequence[entry.Sequence]; !exists || !reflect.DeepEqual(prior, entry) {
			delta = append(delta, entry)
		}
	}
	return delta
}

func runbookStepInputRefs(step runbook.Step) []string {
	refs := make([]string, 0)
	addValue := func(value runbook.Value) { refs = append(refs, runbookValueRefs(value)...) }
	switch step.Kind {
	case runbook.StepAction:
		for _, value := range step.Action.Arguments {
			addValue(value)
		}
	case runbook.StepDelegate:
		addValue(step.Delegate.AgentID)
		addValue(step.Delegate.Goal)
		for _, value := range step.Delegate.Context {
			addValue(value)
		}
	case runbook.StepDecision:
		for _, candidate := range step.Decision.Cases {
			refs = append(refs, runbookPredicateRefs(candidate.When)...)
		}
	case runbook.StepTransform:
		for _, value := range step.Transform.Assignments {
			addValue(value)
		}
	case runbook.StepForEach:
		addValue(step.ForEach.Items)
	case runbook.StepEnd:
		for _, value := range step.End.Outputs {
			addValue(value)
		}
	}
	return uniqueSortedTraceRefs(refs)
}

func runbookStepOutputRefs(stepID string, step runbook.Step) []string {
	refs := make([]string, 0)
	switch step.Kind {
	case runbook.StepAction:
		refs = append(refs, step.Action.ResultPath)
	case runbook.StepDelegate:
		refs = append(refs, step.Delegate.ResultPath)
	case runbook.StepTransform:
		for pointer := range step.Transform.Assignments {
			refs = append(refs, pointer)
		}
	case runbook.StepForEach:
		refs = append(refs, "/loop/"+escapeRunbookPointer(step.ForEach.ItemName))
	case runbook.StepFork:
		refs = append(refs, "/forkResults/"+escapeRunbookPointer(stepID))
	case runbook.StepEnd:
		for name := range step.End.Outputs {
			refs = append(refs, "/output/"+escapeRunbookPointer(name))
		}
	}
	return uniqueSortedTraceRefs(refs)
}

func runbookTraceMetadata(values map[string]interface{}) map[string]string {
	metadata := map[string]string{}
	for _, key := range []string{"actionCallId", "approvalId"} {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			metadata[key] = strings.TrimSpace(value)
		}
	}
	return metadata
}

func runbookValueRefs(value runbook.Value) []string {
	refs := make([]string, 0, 1+len(value.Template))
	if strings.TrimSpace(value.Ref) != "" {
		refs = append(refs, strings.TrimSpace(value.Ref))
	}
	for _, segment := range value.Template {
		if strings.TrimSpace(segment.Ref) != "" {
			refs = append(refs, strings.TrimSpace(segment.Ref))
		}
	}
	return refs
}

func runbookPredicateRefs(predicate runbook.Predicate) []string {
	refs := make([]string, 0)
	if predicate.Left != nil {
		refs = append(refs, runbookValueRefs(*predicate.Left)...)
	}
	if predicate.Right != nil {
		refs = append(refs, runbookValueRefs(*predicate.Right)...)
	}
	for _, operand := range predicate.Operands {
		refs = append(refs, runbookPredicateRefs(operand)...)
	}
	return refs
}

func uniqueSortedTraceRefs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
