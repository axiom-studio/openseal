package source

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
)

const LifecycleAPIVersion = "openseal.source-policy/v1"

var (
	ErrPolicyVersionNotFound = errors.New("source policy version not found")
	ErrPolicyVersionExists   = errors.New("source policy versions are immutable")
	ErrPolicyNotFound        = errors.New("source policy lifecycle not found")
	ErrPolicyRevision        = errors.New("source policy revision conflict")
	ErrPolicyRevoked         = errors.New("source policy is revoked")
)

type LifecycleState string

const (
	LifecycleActive  LifecycleState = "active"
	LifecycleRevoked LifecycleState = "revoked"
)

// Lifecycle is the governed mutable pointer to immutable Policy versions. It
// contains no credentials and can be safely projected into operator clients.
type Lifecycle struct {
	APIVersion      string                    `json:"apiVersion"`
	Scope           capability.ScopeReference `json:"scope"`
	PolicyID        string                    `json:"policyId"`
	ActiveVersion   string                    `json:"activeVersion"`
	PreviousVersion string                    `json:"previousVersion,omitempty"`
	State           LifecycleState            `json:"state"`
	Revision        int64                     `json:"revision"`
	CreatedAt       time.Time                 `json:"createdAt"`
	UpdatedAt       time.Time                 `json:"updatedAt"`
}

type LifecycleEvent struct {
	APIVersion     string                    `json:"apiVersion"`
	ID             string                    `json:"id"`
	Scope          capability.ScopeReference `json:"scope"`
	PolicyID       string                    `json:"policyId"`
	FromVersion    string                    `json:"fromVersion,omitempty"`
	ToVersion      string                    `json:"toVersion,omitempty"`
	FromState      LifecycleState            `json:"fromState,omitempty"`
	ToState        LifecycleState            `json:"toState"`
	PolicyRevision int64                     `json:"policyRevision"`
	ActorType      string                    `json:"actorType"`
	ActorID        string                    `json:"actorId"`
	Reason         string                    `json:"reason"`
	CreatedAt      time.Time                 `json:"createdAt"`
}

type RegisterVersionRequest struct {
	Scope     capability.ScopeReference `json:"scope"`
	Policy    Policy                    `json:"policy"`
	ActorType string                    `json:"actorType"`
	ActorID   string                    `json:"actorId"`
	Reason    string                    `json:"reason"`
}

// PolicyVersion records immutable policy content with credential-free
// registration provenance. Lifecycle activation never mutates this record.
type PolicyVersion struct {
	APIVersion   string                    `json:"apiVersion"`
	Scope        capability.ScopeReference `json:"scope"`
	Policy       *Policy                   `json:"policy"`
	ActorType    string                    `json:"actorType"`
	ActorID      string                    `json:"actorId"`
	Reason       string                    `json:"reason"`
	RegisteredAt time.Time                 `json:"registeredAt"`
}

type ActivateRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	PolicyID         string                    `json:"policyId"`
	Version          string                    `json:"version"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	ActorType        string                    `json:"actorType"`
	ActorID          string                    `json:"actorId"`
	Reason           string                    `json:"reason"`
}

type RevokeRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	PolicyID         string                    `json:"policyId"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	ActorType        string                    `json:"actorType"`
	ActorID          string                    `json:"actorId"`
	Reason           string                    `json:"reason"`
}

type LifecycleResult struct {
	APIVersion string          `json:"apiVersion"`
	Lifecycle  *Lifecycle      `json:"lifecycle"`
	Policy     *Policy         `json:"policy,omitempty"`
	Event      *LifecycleEvent `json:"event"`
}

type LifecycleDetail struct {
	APIVersion string     `json:"apiVersion"`
	Lifecycle  *Lifecycle `json:"lifecycle"`
	Policy     *Policy    `json:"policy,omitempty"`
}

type PolicyVersionList struct {
	APIVersion string           `json:"apiVersion"`
	Items      []*PolicyVersion `json:"items"`
}

type PolicyVersionResult struct {
	APIVersion string         `json:"apiVersion"`
	Version    *PolicyVersion `json:"version"`
}

type LifecycleList struct {
	APIVersion string       `json:"apiVersion"`
	Items      []*Lifecycle `json:"items"`
}

type LifecycleEventList struct {
	APIVersion string            `json:"apiVersion"`
	Items      []*LifecycleEvent `json:"items"`
}

type LifecycleStore interface {
	RegisterVersion(context.Context, *PolicyVersion) error
	GetVersion(context.Context, capability.ScopeReference, string, string) (*PolicyVersion, error)
	ListVersions(context.Context, capability.ScopeReference, string) ([]*PolicyVersion, error)
	GetLifecycle(context.Context, capability.ScopeReference, string) (*Lifecycle, error)
	ListLifecycles(context.Context, capability.ScopeReference) ([]*Lifecycle, error)
	ApplyLifecycle(context.Context, *Lifecycle, int64, LifecycleEvent) error
	ListLifecycleEvents(context.Context, capability.ScopeReference, string) ([]*LifecycleEvent, error)
}

type LifecycleService struct {
	store LifecycleStore
	now   func() time.Time
	newID func() string
}

func NewLifecycleService(store LifecycleStore) (*LifecycleService, error) {
	if store == nil {
		return nil, errors.New("source policy lifecycle store is required")
	}
	return &LifecycleService{store: store, now: time.Now, newID: uuid.NewString}, nil
}

func (s *LifecycleService) RegisterVersion(ctx context.Context, request RegisterVersionRequest) (*PolicyVersion, error) {
	if err := validateScope(request.Scope); err != nil {
		return nil, err
	}
	if err := validateActorReason(request.ActorType, request.ActorID, request.Reason); err != nil {
		return nil, err
	}
	policy := clonePolicy(&request.Policy)
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if !policy.Enabled {
		return nil, errors.New("registered source policy versions must be enabled; use lifecycle revocation to remove authority")
	}
	for _, policySource := range policy.Sources {
		if len(policySource.PathPrefixes) == 0 || len(policySource.Methods) == 0 {
			return nil, errors.New("governed source policy versions require explicit path prefixes and methods for every source")
		}
	}
	record := &PolicyVersion{APIVersion: LifecycleAPIVersion, Scope: request.Scope, Policy: policy,
		ActorType: strings.TrimSpace(request.ActorType), ActorID: strings.TrimSpace(request.ActorID),
		Reason: strings.TrimSpace(request.Reason), RegisteredAt: s.now().UTC()}
	if err := s.store.RegisterVersion(ctx, record); err != nil {
		return nil, err
	}
	return clonePolicyVersion(record), nil
}

func (s *LifecycleService) Activate(ctx context.Context, request ActivateRequest) (*LifecycleResult, error) {
	if err := validateScope(request.Scope); err != nil {
		return nil, err
	}
	if err := validatePolicyReference(request.PolicyID, request.Version); err != nil {
		return nil, err
	}
	if request.ExpectedRevision < 0 {
		return nil, errors.New("expectedRevision cannot be negative")
	}
	if err := validateActorReason(request.ActorType, request.ActorID, request.Reason); err != nil {
		return nil, err
	}
	version, err := s.store.GetVersion(ctx, request.Scope, request.PolicyID, request.Version)
	if err != nil {
		return nil, err
	}
	policy := version.Policy
	if err := policy.Validate(); err != nil || !policy.Enabled {
		if err != nil {
			return nil, fmt.Errorf("stored source policy is invalid: %w", err)
		}
		return nil, errors.New("disabled source policy version cannot be activated")
	}

	current, err := s.store.GetLifecycle(ctx, request.Scope, request.PolicyID)
	if err != nil && !errors.Is(err, ErrPolicyNotFound) {
		return nil, err
	}
	if errors.Is(err, ErrPolicyNotFound) {
		current = nil
	}
	if current == nil && request.ExpectedRevision != 0 {
		return nil, ErrPolicyRevision
	}
	if current != nil && current.Revision != request.ExpectedRevision {
		return nil, ErrPolicyRevision
	}
	if current != nil && current.State == LifecycleActive && current.ActiveVersion == request.Version {
		return nil, errors.New("source policy version is already active")
	}

	now := s.now().UTC()
	next := &Lifecycle{APIVersion: LifecycleAPIVersion, Scope: request.Scope, PolicyID: request.PolicyID,
		ActiveVersion: request.Version, State: LifecycleActive, Revision: 1, CreatedAt: now, UpdatedAt: now}
	event := LifecycleEvent{APIVersion: LifecycleAPIVersion, ID: s.newID(), Scope: request.Scope, PolicyID: request.PolicyID,
		ToVersion: request.Version, ToState: LifecycleActive, PolicyRevision: 1, ActorType: strings.TrimSpace(request.ActorType),
		ActorID: strings.TrimSpace(request.ActorID), Reason: strings.TrimSpace(request.Reason), CreatedAt: now}
	if current != nil {
		next.PreviousVersion, next.Revision, next.CreatedAt = current.ActiveVersion, current.Revision+1, current.CreatedAt
		event.FromVersion, event.FromState, event.PolicyRevision = current.ActiveVersion, current.State, next.Revision
	}
	if err := s.store.ApplyLifecycle(ctx, next, request.ExpectedRevision, event); err != nil {
		return nil, err
	}
	return &LifecycleResult{APIVersion: LifecycleAPIVersion, Lifecycle: cloneLifecycle(next), Policy: clonePolicy(policy), Event: cloneEvent(&event)}, nil
}

func (s *LifecycleService) Revoke(ctx context.Context, request RevokeRequest) (*LifecycleResult, error) {
	if err := validateScope(request.Scope); err != nil {
		return nil, err
	}
	if err := validatePolicyID(request.PolicyID); err != nil {
		return nil, errors.New("source policy id is required")
	}
	if request.ExpectedRevision < 1 {
		return nil, errors.New("expectedRevision must be positive")
	}
	if err := validateActorReason(request.ActorType, request.ActorID, request.Reason); err != nil {
		return nil, err
	}
	current, err := s.store.GetLifecycle(ctx, request.Scope, request.PolicyID)
	if err != nil {
		return nil, err
	}
	if current.Revision != request.ExpectedRevision {
		return nil, ErrPolicyRevision
	}
	if current.State == LifecycleRevoked {
		return nil, ErrPolicyRevoked
	}
	now := s.now().UTC()
	next := cloneLifecycle(current)
	next.State, next.Revision, next.UpdatedAt = LifecycleRevoked, current.Revision+1, now
	event := LifecycleEvent{APIVersion: LifecycleAPIVersion, ID: s.newID(), Scope: request.Scope, PolicyID: request.PolicyID,
		FromVersion: current.ActiveVersion, ToVersion: current.ActiveVersion, FromState: current.State, ToState: LifecycleRevoked,
		PolicyRevision: next.Revision, ActorType: strings.TrimSpace(request.ActorType), ActorID: strings.TrimSpace(request.ActorID),
		Reason: strings.TrimSpace(request.Reason), CreatedAt: now}
	if err := s.store.ApplyLifecycle(ctx, next, request.ExpectedRevision, event); err != nil {
		return nil, err
	}
	return &LifecycleResult{APIVersion: LifecycleAPIVersion, Lifecycle: cloneLifecycle(next), Event: cloneEvent(&event)}, nil
}

func (s *LifecycleService) ResolveActive(ctx context.Context, scope capability.ScopeReference, policyID string) (*Policy, *Lifecycle, error) {
	if err := validateScope(scope); err != nil {
		return nil, nil, err
	}
	current, err := s.store.GetLifecycle(ctx, scope, strings.TrimSpace(policyID))
	if err != nil {
		return nil, nil, err
	}
	if current.State != LifecycleActive {
		return nil, cloneLifecycle(current), ErrPolicyRevoked
	}
	version, err := s.store.GetVersion(ctx, scope, current.PolicyID, current.ActiveVersion)
	if err != nil {
		return nil, cloneLifecycle(current), err
	}
	policy := version.Policy
	if err := policy.Validate(); err != nil || !policy.Enabled {
		if err == nil {
			err = errors.New("stored source policy version is disabled")
		}
		return nil, cloneLifecycle(current), fmt.Errorf("active source policy fails closed: %w", err)
	}
	return clonePolicy(policy), cloneLifecycle(current), nil
}

func (s *LifecycleService) GetLifecycle(ctx context.Context, scope capability.ScopeReference, policyID string) (*Lifecycle, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	value, err := s.store.GetLifecycle(ctx, scope, strings.TrimSpace(policyID))
	return cloneLifecycle(value), err
}

func (s *LifecycleService) ListLifecycles(ctx context.Context, scope capability.ScopeReference) ([]*Lifecycle, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	return s.store.ListLifecycles(ctx, scope)
}

func (s *LifecycleService) ListVersions(ctx context.Context, scope capability.ScopeReference, policyID string) ([]*PolicyVersion, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	return s.store.ListVersions(ctx, scope, strings.TrimSpace(policyID))
}

func (s *LifecycleService) GetVersion(ctx context.Context, scope capability.ScopeReference, policyID, version string) (*PolicyVersion, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	if err := validatePolicyReference(policyID, version); err != nil {
		return nil, err
	}
	value, err := s.store.GetVersion(ctx, scope, strings.TrimSpace(policyID), strings.TrimSpace(version))
	return clonePolicyVersion(value), err
}

func (s *LifecycleService) ListEvents(ctx context.Context, scope capability.ScopeReference, policyID string) ([]*LifecycleEvent, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	return s.store.ListLifecycleEvents(ctx, scope, strings.TrimSpace(policyID))
}

type MemoryLifecycleStore struct {
	mu         sync.RWMutex
	versions   map[string]*PolicyVersion
	lifecycles map[string]*Lifecycle
	events     map[string][]*LifecycleEvent
}

func NewMemoryLifecycleStore() *MemoryLifecycleStore {
	return &MemoryLifecycleStore{versions: map[string]*PolicyVersion{}, lifecycles: map[string]*Lifecycle{}, events: map[string][]*LifecycleEvent{}}
}

func (s *MemoryLifecycleStore) RegisterVersion(_ context.Context, version *PolicyVersion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := versionKey(version.Scope, version.Policy.ID, version.Policy.Version)
	if s.versions[key] != nil {
		return ErrPolicyVersionExists
	}
	s.versions[key] = clonePolicyVersion(version)
	return nil
}

func (s *MemoryLifecycleStore) GetVersion(_ context.Context, scope capability.ScopeReference, id, version string) (*PolicyVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.versions[versionKey(scope, id, version)]
	if value == nil {
		return nil, ErrPolicyVersionNotFound
	}
	return clonePolicyVersion(value), nil
}

func (s *MemoryLifecycleStore) ListVersions(_ context.Context, scope capability.ScopeReference, id string) ([]*PolicyVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]*PolicyVersion, 0)
	for key, value := range s.versions {
		if strings.HasPrefix(key, scopeKey(scope)+"\x00"+strings.TrimSpace(id)+"\x00") {
			items = append(items, clonePolicyVersion(value))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Policy.Version < items[j].Policy.Version })
	return items, nil
}

func (s *MemoryLifecycleStore) GetLifecycle(_ context.Context, scope capability.ScopeReference, id string) (*Lifecycle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.lifecycles[policyKey(scope, id)]
	if value == nil {
		return nil, ErrPolicyNotFound
	}
	return cloneLifecycle(value), nil
}

func (s *MemoryLifecycleStore) ListLifecycles(_ context.Context, scope capability.ScopeReference) ([]*Lifecycle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]*Lifecycle, 0)
	for key, value := range s.lifecycles {
		if strings.HasPrefix(key, scopeKey(scope)+"\x00") {
			items = append(items, cloneLifecycle(value))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].PolicyID < items[j].PolicyID })
	return items, nil
}

func (s *MemoryLifecycleStore) ApplyLifecycle(_ context.Context, next *Lifecycle, expectedRevision int64, event LifecycleEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := policyKey(next.Scope, next.PolicyID)
	current := s.lifecycles[key]
	if (current == nil && expectedRevision != 0) || (current != nil && current.Revision != expectedRevision) {
		return ErrPolicyRevision
	}
	s.lifecycles[key] = cloneLifecycle(next)
	s.events[key] = append(s.events[key], cloneEvent(&event))
	return nil
}

func (s *MemoryLifecycleStore) ListLifecycleEvents(_ context.Context, scope capability.ScopeReference, id string) ([]*LifecycleEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := s.events[policyKey(scope, id)]
	items := make([]*LifecycleEvent, len(values))
	for i, value := range values {
		items[i] = cloneEvent(value)
	}
	return items, nil
}

func validateScope(scope capability.ScopeReference) error {
	if strings.TrimSpace(scope.Kind) == "" || len(scope.Kind) > 64 || strings.TrimSpace(scope.ID) == "" || len(scope.ID) > 256 {
		return errors.New("source policy scope kind and id are required")
	}
	return nil
}

func validateActorReason(actorType, actorID, reason string) error {
	if strings.TrimSpace(actorType) == "" || len(actorType) > 64 || strings.TrimSpace(actorID) == "" || len(actorID) > 256 ||
		strings.TrimSpace(reason) == "" || len(reason) > 1024 {
		return errors.New("source policy lifecycle actor and reason are required")
	}
	return nil
}

func validatePolicyReference(id, version string) error {
	if validatePolicyID(id) != nil || strings.TrimSpace(version) == "" || len(version) > 128 {
		return errors.New("source policy id and version are required")
	}
	return nil
}

func validatePolicyID(id string) error {
	if strings.TrimSpace(id) == "" || len(id) > 128 {
		return errors.New("source policy id is required")
	}
	return nil
}

func scopeKey(scope capability.ScopeReference) string {
	return strings.TrimSpace(scope.Kind) + "\x00" + strings.TrimSpace(scope.ID)
}
func policyKey(scope capability.ScopeReference, id string) string {
	return scopeKey(scope) + "\x00" + strings.TrimSpace(id)
}
func versionKey(scope capability.ScopeReference, id, version string) string {
	return policyKey(scope, id) + "\x00" + strings.TrimSpace(version)
}

func clonePolicy(value *Policy) *Policy {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Sources = make([]PolicySource, len(value.Sources))
	for i, item := range value.Sources {
		copy.Sources[i] = item
		copy.Sources[i].PathPrefixes = append([]string(nil), item.PathPrefixes...)
		copy.Sources[i].Methods = append([]string(nil), item.Methods...)
	}
	if value.Outreach != nil {
		outreach := *value.Outreach
		copy.Outreach = &outreach
	}
	return &copy
}

func clonePolicyVersion(value *PolicyVersion) *PolicyVersion {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Policy = clonePolicy(value.Policy)
	return &copy
}

func cloneLifecycle(value *Lifecycle) *Lifecycle {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneEvent(value *LifecycleEvent) *LifecycleEvent {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
