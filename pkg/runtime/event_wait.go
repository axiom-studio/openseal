package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

const (
	MaximumRunEventWaitDuration = 30 * 24 * time.Hour
	RunEventRetention           = 35 * 24 * time.Hour
	runEventWaitCheckpointKey   = "lastEventWait"
)

var (
	ErrInvalidRunEventWait  = errors.New("invalid run event wait")
	ErrRunEventConflict     = errors.New("run event identity conflict")
	ErrRunEventWaitNotFound = errors.New("run event wait not found")
)

// RunEventWaitSpec is a bounded, connector-neutral expectation. Source is an
// opaque kernel-owned connection identity, never a provider-supplied tenant.
// Key distinguishes successive waits in the same execution, including loops.
// Attributes are exact scalar matches against authenticated observations.
type RunEventWaitSpec struct {
	Key        string                 `json:"key"`
	Type       string                 `json:"type"`
	Source     string                 `json:"source"`
	Subject    string                 `json:"subject"`
	Attributes map[string]interface{} `json:"attributes,omitempty"`
	After      time.Time              `json:"after"`
	Deadline   time.Time              `json:"deadline"`
}

// Keep exact scalar selector values when a Run or Turn is read back through
// ordinary JSON decoding. Changing all checkpoint numbers would alter unrelated
// contracts, so precision preservation is local to the event wait contract.
func (s *RunEventWaitSpec) UnmarshalJSON(data []byte) error {
	type plain RunEventWaitSpec
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*s = RunEventWaitSpec(value)
	return nil
}

func (s *RunEventWaitSpec) Validate() error {
	if s == nil || !validOpaqueIdentifier(s.Key, 256) || !validateEventSelector(s.Type) || s.Type == "*" || !validOpaqueIdentifier(s.Source, 256) || !validExternalConversationReference(s.Subject, 512) {
		return ErrInvalidRunEventWait
	}
	if s.After.IsZero() || !runEventNanoTimeValid(s.After) || !runEventNanoTimeValid(s.Deadline) || !s.Deadline.After(s.After) || s.Deadline.Sub(s.After) > MaximumRunEventWaitDuration || len(s.Attributes) > 16 {
		return ErrInvalidRunEventWait
	}
	if err := validateEventAttributes(s.Attributes); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRunEventWait, err)
	}
	for _, value := range s.Attributes {
		switch value.(type) {
		case string, bool, int, int32, int64, float32, float64, json.Number:
		default:
			return fmt.Errorf("%w: event attributes must be scalars", ErrInvalidRunEventWait)
		}
		if !runEventScalarBounded(value) {
			return fmt.Errorf("%w: event numeric precision or exponent exceeds limits", ErrInvalidRunEventWait)
		}
	}
	encoded, err := json.Marshal(s.Attributes)
	if err != nil || len(encoded) > 8192 {
		return ErrInvalidRunEventWait
	}
	return nil
}

type RunEventWaitStatus string

const (
	RunEventWaitPending  RunEventWaitStatus = "pending"
	RunEventWaitPaused   RunEventWaitStatus = "paused"
	RunEventWaitMatched  RunEventWaitStatus = "matched"
	RunEventWaitTimedOut RunEventWaitStatus = "timed_out"
	RunEventWaitCanceled RunEventWaitStatus = "canceled"
)

type RunEventWait struct {
	Scope           Scope              `json:"scope"`
	RunID           string             `json:"runId"`
	AssignedAgentID string             `json:"assignedAgentId,omitempty"`
	Spec            RunEventWaitSpec   `json:"spec"`
	Status          RunEventWaitStatus `json:"status"`
	AvailableAt     *time.Time         `json:"availableAt,omitempty"`
	LeaseOwner      string             `json:"-"`
	LeaseExpiresAt  *time.Time         `json:"-"`
	ResolvedAt      *time.Time         `json:"resolvedAt,omitempty"`
	EventID         string             `json:"eventId,omitempty"`
}

// RunEventReceipt records a verified observation. Ordinary caller-authored
// /events envelopes must not enter this inbox. Only authenticated adapters or
// trusted host acquisition publish through this internal contract.
type RunEventReceipt struct {
	Event      EventEnvelope `json:"event"`
	ReceivedAt time.Time     `json:"receivedAt"`
	Digest     string        `json:"digest"`
}

type ClaimRunEventWaitsRequest struct {
	Scope         Scope
	WorkerID      string
	Now           time.Time
	LeaseDuration time.Duration
	Limit         int
}

func (r ClaimRunEventWaitsRequest) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(r.WorkerID, 256) || r.Now.IsZero() || r.LeaseDuration <= 0 || r.LeaseDuration > 5*time.Minute || r.Limit < 1 || r.Limit > 100 {
		return ErrInvalidRunEventWait
	}
	return nil
}

type ProcessRunEventWaitRequest struct {
	Scope          Scope
	RunID          string
	Key            string
	WorkerID       string
	Now            time.Time
	LeaseExpiresAt time.Time
}

func (r ProcessRunEventWaitRequest) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(r.RunID, 128) || !validOpaqueIdentifier(r.Key, 256) || !validOpaqueIdentifier(r.WorkerID, 256) || r.Now.IsZero() || r.LeaseExpiresAt.IsZero() {
		return ErrInvalidRunEventWait
	}
	return nil
}

type RunEventWaitResult struct {
	Wait     *RunEventWait  `json:"wait"`
	Run      *AgentRun      `json:"run,omitempty"`
	Event    *EventEnvelope `json:"event,omitempty"`
	Activity *ActivityEvent `json:"activity,omitempty"`
}

// Implementations atomically fence the lease, check authoritative Run state,
// consume an observation once per Run, save its result, and enqueue the same Run.
// Publishing schedules only indexed matching waits; idle waits do no polling.
type RunEventWaitStore interface {
	PublishRunEvent(context.Context, *RunEventReceipt) (bool, error)
	ListRunEventWaitWorkScopes(context.Context, time.Time, int) ([]Scope, error)
	ClaimRunEventWaits(context.Context, ClaimRunEventWaitsRequest) ([]*RunEventWait, error)
	ProcessRunEventWait(context.Context, ProcessRunEventWaitRequest) (*RunEventWaitResult, error)
	GetRunEventWait(context.Context, Scope, string, string) (*RunEventWait, error)
	PruneRunEvents(context.Context, Scope, time.Time, int) (int, error)
	PruneExpiredRunEvents(context.Context, time.Time, int) (int, error)
}

type RunEventWaitService struct {
	store RunEventWaitStore
	now   func() time.Time
}

func NewRunEventWaitService(store RunEventWaitStore) *RunEventWaitService {
	return &RunEventWaitService{store: store, now: time.Now}
}
func (s *RunEventWaitService) Publish(ctx context.Context, event EventEnvelope) (bool, error) {
	if s == nil || s.store == nil {
		return false, errors.New("run event inbox is not configured")
	}
	return s.PublishReceivedAt(ctx, event, s.now().UTC())
}

// PublishReceivedAt preserves the acquisition time of a durably accepted
// observation. Dispatch retries must not turn an on-time event into a late one.
// This is an internal trusted-acquisition API, never caller-supplied input.
func (s *RunEventWaitService) PublishReceivedAt(ctx context.Context, event EventEnvelope, receivedAt time.Time) (bool, error) {
	if s == nil || s.store == nil {
		return false, errors.New("run event inbox is not configured")
	}
	receipt, err := newRunEventReceipt(event, receivedAt)
	if err != nil {
		return false, err
	}
	return s.store.PublishRunEvent(ctx, receipt)
}

func newRunEventReceipt(event EventEnvelope, receivedAt time.Time) (*RunEventReceipt, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(event.Subject) == "" {
		return nil, ErrInvalidRunEventWait
	}
	digest, err := runEventObservationDigest(event)
	if err != nil {
		return nil, err
	}
	receipt := &RunEventReceipt{Event: event, ReceivedAt: receivedAt.UTC(), Digest: digest}
	if err := validateRunEventReceipt(receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

// Actor identifies the host registration that acquired an event. Two reviewed
// registrations can observe the same provider event for the same binding; that
// provenance does not change the immutable observation or its deduplication ID.
func runEventObservationDigest(event EventEnvelope) (string, error) {
	event.Actor = ActivityActor{}
	data, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// RunEventBindingSource is the stable authenticated namespace shared by callback
// and conversation adapters. Revision/version are rechecked by acquisition and
// action execution, not encoded into an identity that changes on every update.
func RunEventBindingSource(deploymentID, bindingID, adapterID string) string {
	sum := sha256.Sum256([]byte(deploymentID + "\x00" + bindingID + "\x00" + adapterID))
	return "binding:" + hex.EncodeToString(sum[:])
}

func validateRunEventReceipt(r *RunEventReceipt) error {
	if r == nil || r.ReceivedAt.IsZero() || len(r.Digest) != 64 {
		return ErrInvalidRunEventWait
	}
	if err := r.Event.Validate(); err != nil {
		return err
	}
	if !runEventNanoTimeValid(r.ReceivedAt) || !runEventNanoTimeValid(r.Event.OccurredAt) {
		return ErrInvalidRunEventWait
	}
	for _, value := range r.Event.Attributes {
		if !runEventScalarBounded(value) {
			return ErrInvalidRunEventWait
		}
	}
	if strings.HasPrefix(r.Event.Source, "binding:") {
		deployment, _ := r.Event.Attributes["deploymentId"].(string)
		binding, _ := r.Event.Attributes["bindingId"].(string)
		adapter, _ := r.Event.Attributes["adapterId"].(string)
		if deployment == "" || binding == "" || adapter == "" || r.Event.Source != RunEventBindingSource(deployment, binding, adapter) {
			return ErrInvalidRunEventWait
		}
	}
	digest, err := runEventObservationDigest(r.Event)
	if err != nil {
		return err
	}
	if r.Digest != digest {
		return ErrRunEventConflict
	}
	return nil
}

func runEventNanoTimeValid(at time.Time) bool {
	return !at.IsZero() && time.Unix(0, at.UnixNano()).Equal(at)
}

func runEventWaitForRun(run *AgentRun) (*RunEventWait, error) {
	if run == nil {
		return nil, nil
	}
	condition := run.WakeCondition
	if run.Status == AgentRunStatusPaused {
		condition = run.PausedWakeCondition
	}
	if condition == nil || condition.EventWait == nil {
		return nil, nil
	}
	if run.Status != AgentRunStatusWaitingForEvent && run.Status != AgentRunStatusPaused {
		return nil, nil
	}
	spec := condition.EventWait
	if condition.Type != "event" {
		return nil, ErrInvalidRunEventWait
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	status := RunEventWaitPending
	available := run.UpdatedAt
	if available.IsZero() {
		available = run.CreatedAt
	}
	wait := &RunEventWait{Scope: run.Scope, RunID: run.ID, AssignedAgentID: run.AssignedAgentID, Spec: *cloneRunEventWaitSpec(spec), Status: status, AvailableAt: &available}
	if run.Status == AgentRunStatusPaused {
		wait.Status = RunEventWaitPaused
		wait.AvailableAt = nil
	}
	return wait, nil
}

func sameRunEventWaitSpec(a, b *RunEventWaitSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Key != b.Key || a.Type != b.Type || a.Source != b.Source || a.Subject != b.Subject || !a.After.Equal(b.After) || !a.Deadline.Equal(b.Deadline) || len(a.Attributes) != len(b.Attributes) {
		return false
	}
	for key, expected := range a.Attributes {
		actual, exists := b.Attributes[key]
		if !exists || !runEventScalarEqual(expected, actual) {
			return false
		}
	}
	return true
}
func cloneRunEventWaitSpec(s *RunEventWaitSpec) *RunEventWaitSpec {
	if s == nil {
		return nil
	}
	copy := *s
	copy.Attributes = cloneMap(s.Attributes)
	return &copy
}
func cloneRunEventWait(w *RunEventWait) *RunEventWait {
	if w == nil {
		return nil
	}
	copy := *w
	copy.Spec = *cloneRunEventWaitSpec(&w.Spec)
	if w.AvailableAt != nil {
		at := *w.AvailableAt
		copy.AvailableAt = &at
	}
	if w.LeaseExpiresAt != nil {
		at := *w.LeaseExpiresAt
		copy.LeaseExpiresAt = &at
	}
	if w.ResolvedAt != nil {
		at := *w.ResolvedAt
		copy.ResolvedAt = &at
	}
	return &copy
}

func runEventWaitMatches(wait *RunEventWait, receipt *RunEventReceipt) bool {
	s := wait.Spec
	e := receipt.Event
	if wait.Scope != e.Scope || s.Source != e.Source || s.Type != e.Type || s.Subject != e.Subject || e.OccurredAt.Before(s.After) || receipt.ReceivedAt.After(s.Deadline) {
		return false
	}
	if strings.HasPrefix(s.Source, "binding:") {
		deployment, _ := e.Attributes["deploymentId"].(string)
		binding, _ := e.Attributes["bindingId"].(string)
		adapter, _ := e.Attributes["adapterId"].(string)
		if deployment == "" || deployment != wait.AssignedAgentID || binding == "" || adapter == "" || s.Source != RunEventBindingSource(deployment, binding, adapter) {
			return false
		}
	}
	for key, expected := range s.Attributes {
		actual, exists := e.Attributes[key]
		if !exists {
			return false
		}
		if !runEventScalarEqual(expected, actual) {
			return false
		}
	}
	return true
}

func runEventScalarEqual(a, b interface{}) bool {
	if !runEventScalarBounded(a) || !runEventScalarBounded(b) {
		return false
	}
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	if ex != nil || ey != nil {
		return false
	}
	if len(x) > 0 && len(y) > 0 && ((x[0] >= '0' && x[0] <= '9') || x[0] == '-') && ((y[0] >= '0' && y[0] <= '9') || y[0] == '-') {
		left, okLeft := new(big.Rat).SetString(string(x))
		right, okRight := new(big.Rat).SetString(string(y))
		return okLeft && okRight && left.Cmp(right) == 0
	}
	return string(x) == string(y)
}

func runEventScalarBounded(value interface{}) bool {
	number, ok := value.(json.Number)
	if !ok {
		return true
	}
	data := string(number)
	if len(data) > 4096 {
		return false
	}
	if _, err := json.Marshal(number); err != nil {
		return false
	}
	if at := strings.IndexAny(data, "eE"); at >= 0 {
		exponent, err := strconv.ParseInt(data[at+1:], 10, 32)
		if err != nil || exponent < -4096 || exponent > 4096 {
			return false
		}
	}
	return true
}

func restoreRunEventWaitCheckpoint(payload []byte, run *AgentRun) error {
	if run.Checkpoint == nil || (run.Checkpoint[runEventWaitCheckpointKey] == nil && run.Checkpoint["runbook"] == nil) {
		return nil
	}
	var encoded struct {
		Checkpoint map[string]json.RawMessage `json:"checkpoint"`
	}
	if err := json.Unmarshal(payload, &encoded); err != nil {
		return err
	}
	data := encoded.Checkpoint[runEventWaitCheckpointKey]
	if len(data) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value interface{}
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		run.Checkpoint[runEventWaitCheckpointKey] = value
	}
	if state, ok := run.Checkpoint["runbook"].(map[string]interface{}); ok {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(encoded.Checkpoint["runbook"], &raw); err != nil {
			return err
		}
		if spec := raw["waitSpec"]; len(spec) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(spec))
			decoder.UseNumber()
			var value interface{}
			if err := decoder.Decode(&value); err != nil {
				return err
			}
			state["waitSpec"] = value
		}
	}
	return nil
}

// prepareRunEventWaitResolution is deterministic and shared by every store.
func prepareRunEventWaitResolution(run *AgentRun, wait *RunEventWait, receipt *RunEventReceipt, now time.Time) (*AgentRun, *RunEventWait, *ActivityEvent, error) {
	if run.Scope != wait.Scope || run.ID != wait.RunID {
		return nil, nil, nil, ErrInvalidScope
	}
	updated := cloneAgentRun(run)
	resolved := cloneRunEventWait(wait)
	resolved.LeaseOwner = ""
	resolved.LeaseExpiresAt = nil
	resolved.AvailableAt = nil
	if run.Status != AgentRunStatusWaitingForEvent || run.WakeCondition == nil || !sameRunEventWaitSpec(run.WakeCondition.EventWait, &wait.Spec) {
		resolved.Status = RunEventWaitCanceled
		if run.Status == AgentRunStatusPaused && run.PausedWakeCondition != nil && sameRunEventWaitSpec(run.PausedWakeCondition.EventWait, &wait.Spec) {
			resolved.Status = RunEventWaitPaused
		}
		return nil, resolved, nil, nil
	}
	if receipt == nil && now.Before(wait.Spec.Deadline) {
		at := wait.Spec.Deadline
		resolved.AvailableAt = &at
		return nil, resolved, nil, nil
	}
	result := map[string]interface{}{"key": wait.Spec.Key, "status": string(RunEventWaitTimedOut)}
	resolved.Status = RunEventWaitTimedOut
	if receipt != nil {
		if !runEventWaitMatches(wait, receipt) {
			return nil, nil, nil, ErrInvalidRunEventWait
		}
		resolved.Status = RunEventWaitMatched
		resolved.EventID = receipt.Event.ID
		result["status"] = string(RunEventWaitMatched)
		result["event"] = eventContext(receipt.Event)
	}
	resolved.ResolvedAt = &now
	if updated.Checkpoint == nil {
		updated.Checkpoint = map[string]interface{}{}
	}
	updated.Checkpoint[runEventWaitCheckpointKey] = result
	updated.Status = AgentRunStatusQueued
	updated.WakeCondition = nil
	updated.Revision++
	updated.UpdatedAt = now
	updated.AvailableAt = now
	updated.QueueEnteredAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.LastWakeSignalID = "event-wait:" + wait.Spec.Key + ":" + string(resolved.Status)
	activity := &ActivityEvent{ID: runEventWaitActivityID(wait), Scope: run.Scope, RunID: run.ID, AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, TeamID: teamIDForRun(run), ParentRunID: run.ParentRunID, EventType: "run.event_wait_resolved", Severity: ActivitySeverityInfo, Visibility: ActivityVisibilityScope, Actor: ActivityActor{Type: "event", ID: wait.Spec.Source}, Summary: "External event wait " + string(resolved.Status), CreatedAt: now, Payload: map[string]interface{}{"waitKey": wait.Spec.Key, "status": string(resolved.Status), "eventId": resolved.EventID}}
	return updated, resolved, activity, nil
}
func runEventWaitActivityID(wait *RunEventWait) string {
	sum := sha256.Sum256([]byte(wait.Scope.Kind + "\x00" + wait.Scope.ID + "\x00" + wait.RunID + "\x00" + wait.Spec.Key))
	return "event-wait:" + hex.EncodeToString(sum[:])
}
