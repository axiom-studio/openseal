package runtime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

var ErrInvalidRunTerminalReport = errors.New("invalid terminal Run reporting operation")
var ErrRunTerminalReportNotFound = errors.New("terminal Run report not found")

// RunTerminalReport is an immutable delivery intent captured in the same
// transaction as a terminal Run. Reporting never re-executes a model or action.
// Acknowledged rows retain their identity but release the duplicated snapshot.
type RunTerminalReport struct {
	Scope            Scope          `json:"scope"`
	RunID            string         `json:"runId"`
	TerminalRevision int64          `json:"terminalRevision"`
	Status           AgentRunStatus `json:"status"`
	Run              *AgentRun      `json:"run,omitempty"`
	AvailableAt      time.Time      `json:"availableAt"`
	Attempts         int            `json:"attempts"`
	LeaseOwner       string         `json:"-"`
	LeaseExpiresAt   *time.Time     `json:"-"`
	DeliveredAt      *time.Time     `json:"deliveredAt,omitempty"`
}

// A nil Scope is reserved for trusted host workers draining the indexed global
// queue. User-facing callers must supply their authorized Scope.
type RunTerminalReportingClaim struct {
	Scope         *Scope
	WorkerID      string
	Now           time.Time
	LeaseDuration time.Duration
	Limit         int
}

func (r RunTerminalReportingClaim) Validate() error {
	if r.Scope != nil {
		if err := r.Scope.Validate(); err != nil {
			return err
		}
	}
	if !validOpaqueIdentifier(r.WorkerID, 256) || r.Now.IsZero() || r.LeaseDuration <= 0 || r.LeaseDuration > 5*time.Minute || r.Limit < 1 || r.Limit > 100 {
		return ErrInvalidRunTerminalReport
	}
	return nil
}

type RunTerminalReportingCompletion struct {
	Scope            Scope
	RunID            string
	TerminalRevision int64
	Status           AgentRunStatus
	WorkerID         string
	LeaseExpiresAt   time.Time
	Now              time.Time
}

func (r RunTerminalReportingCompletion) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if r.TerminalRevision < 0 || !validOpaqueIdentifier(r.RunID, 128) || !isTerminalAgentRunStatus(r.Status) || !validOpaqueIdentifier(r.WorkerID, 256) || r.Now.IsZero() || r.LeaseExpiresAt.IsZero() {
		return ErrInvalidRunTerminalReport
	}
	return nil
}

type RunTerminalReportingRetry struct {
	RunTerminalReportingCompletion
	AvailableAt time.Time
}

func (r RunTerminalReportingRetry) Validate() error {
	if err := r.RunTerminalReportingCompletion.Validate(); err != nil {
		return err
	}
	if !r.AvailableAt.After(r.Now) || r.AvailableAt.Sub(r.Now) > time.Hour {
		return ErrInvalidRunTerminalReport
	}
	return nil
}

// Stores enqueue without a second transaction or best-effort callback on every
// canonical Run write. Claims are bounded and operate only on pending intents;
// lease tokens fence replicas and acknowledgment is separate from projection.
type RunTerminalReportingStore interface {
	ClaimRunTerminalReports(context.Context, RunTerminalReportingClaim) ([]*RunTerminalReport, error)
	CompleteRunTerminalReport(context.Context, RunTerminalReportingCompletion) error
	RetryRunTerminalReport(context.Context, RunTerminalReportingRetry) error
	GetRunTerminalReport(context.Context, Scope, string, AgentRunStatus) (*RunTerminalReport, error)
}

func runNeedsTerminalReporting(run *AgentRun) bool {
	if run == nil || !isTerminalAgentRunStatus(run.Status) {
		return false
	}
	if failedConversationReportCandidate(run) {
		return true
	}
	rootID, _ := run.Context[runReportingContextRootRunID].(string)
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	if strings.TrimSpace(rootID) != run.ID || strings.TrimSpace(conversationID) == "" || strings.TrimSpace(triggerID) == "" {
		return false
	}
	milestone := runbook.ReportingFailed
	if run.Status == AgentRunStatusCompleted {
		milestone = runbook.ReportingCompleted
	}
	return runReportsMilestone(run, milestone)
}

func cloneRunTerminalReport(report *RunTerminalReport) *RunTerminalReport {
	if report == nil {
		return nil
	}
	copy := *report
	copy.Run = cloneAgentRun(report.Run)
	if report.LeaseExpiresAt != nil {
		at := *report.LeaseExpiresAt
		copy.LeaseExpiresAt = &at
	}
	if report.DeliveredAt != nil {
		at := *report.DeliveredAt
		copy.DeliveredAt = &at
	}
	return &copy
}
