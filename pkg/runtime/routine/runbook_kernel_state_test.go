package routine

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestRunbookKernelStatePayloadNamesStepAndTraceDelta(t *testing.T) {
	before := map[string]interface{}{"runbook": map[string]interface{}{"current": "fetch"}}
	after := map[string]interface{}{"runbook": map[string]interface{}{"current": " report "}}
	trace := &RunbookExecutionTrace{Revision: runbookTraceRevision, NextSequence: 2, Entries: []RunbookStepTrace{
		{Sequence: 1, StepID: "fetch", StepKind: runbook.StepAction, Visit: 1, Status: RunbookStepTraceRunning, StartedAt: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)},
	}}
	if err := encodeRunbookTrace(after, trace); err != nil {
		t.Fatal(err)
	}
	payload := (*RunbookStepProducer)(nil).KernelStatePayload(before, after)
	if payload["runbookStep"] != "report" {
		t.Fatalf("runbookStep = %#v", payload["runbookStep"])
	}
	delta, _ := payload["runbookTraceDelta"].([]RunbookStepTrace)
	if len(delta) != 1 || delta[0].StepID != "fetch" {
		t.Fatalf("runbookTraceDelta = %#v", payload["runbookTraceDelta"])
	}
	if unchanged := (*RunbookStepProducer)(nil).KernelStatePayload(after, after); unchanged["runbookTraceDelta"] != nil {
		t.Fatalf("unchanged trace payload = %#v", unchanged)
	}
	if empty := (*RunbookStepProducer)(nil).KernelStatePayload(nil, map[string]interface{}{"phase": "done"}); len(empty) != 0 {
		t.Fatalf("non-runbook state payload = %#v", empty)
	}
}
