package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const SkillReferenceUpgradeAPIVersion = "openseal.io/skill-reference-upgrade/v1"

var (
	ErrSkillReferenceUpgradeUnavailable = errors.New("skill reference upgrade service is not configured")
	ErrSkillReferenceUpgradeApproval    = errors.New("skill reference upgrade requires approval")
	// ErrSkillReferenceUpgradeCurrent reports that the binding already has the
	// target identity, typically because a group upgrade moved it with a
	// sibling. Callers converging on a version treat it as done.
	ErrSkillReferenceUpgradeCurrent = errors.New("target identity is already active")
)

type PlanSkillReferenceUpgradeRequest struct {
	Scope            models.Scope `json:"scope"`
	DeploymentID     string       `json:"deploymentId"`
	BindingID        string       `json:"bindingId"`
	ToVersion        string       `json:"toVersion"`
	ToSourceIdentity string       `json:"toSourceIdentity,omitempty"`
	// GrantActions names actions to add to the moved bindings when the target
	// version introduces them. Only kernel Skills accept it; the plan keeps the
	// granted actions that are new in the target as AddedActions.
	GrantActions []string `json:"grantActions,omitempty"`
}

type ApplySkillReferenceUpgradeRequest struct {
	Plan     *models.SkillReferenceUpgradePlan     `json:"plan"`
	Actor    models.ActivityActor                  `json:"actor"`
	Reason   string                                `json:"reason"`
	Approval *models.SkillReferenceUpgradeApproval `json:"approval,omitempty"`
}

type SkillReferenceUpgradeService struct {
	store   models.SkillReferenceUpgradeStore
	catalog *skill.Catalog
	now     func() time.Time
}

func NewSkillReferenceUpgradeService(store models.SkillReferenceUpgradeStore, catalog *skill.Catalog, opts ...models.ServiceOption) *SkillReferenceUpgradeService {
	return &SkillReferenceUpgradeService{store: store, catalog: catalog, now: models.ClockFrom(opts)}
}

func (s *SkillReferenceUpgradeService) Plan(ctx context.Context, req PlanSkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradePlan, error) {
	if s == nil || s.store == nil || s.catalog == nil {
		return nil, ErrSkillReferenceUpgradeUnavailable
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	req.DeploymentID = strings.TrimSpace(req.DeploymentID)
	req.BindingID = strings.TrimSpace(req.BindingID)
	req.ToVersion = strings.TrimSpace(req.ToVersion)
	req.ToSourceIdentity = strings.TrimSpace(req.ToSourceIdentity)
	if req.DeploymentID == "" || req.BindingID == "" || req.ToVersion == "" {
		return nil, fmt.Errorf("%w: deployment, binding, and target version are required", models.ErrSkillReferenceUpgradeInvalid)
	}
	scope := skill.ScopeReference{Kind: req.Scope.Kind, ID: req.Scope.ID}
	bindings, err := s.store.ListSkillBindings(ctx, scope, req.DeploymentID)
	if err != nil {
		return nil, err
	}
	var current *skill.Binding
	for _, candidate := range bindings {
		if candidate != nil && candidate.ID == req.BindingID {
			current = cloneUpgradeBinding(candidate)
			break
		}
	}
	if current == nil {
		return nil, skill.ErrBindingNotFound
	}
	if current.Disabled {
		return nil, fmt.Errorf("%w: disabled binding cannot be upgraded", models.ErrSkillReferenceUpgradeInvalid)
	}
	if current.SkillVersion == req.ToVersion && current.SourceIdentity == req.ToSourceIdentity {
		return nil, fmt.Errorf("%w: %w", models.ErrSkillReferenceUpgradeInvalid, ErrSkillReferenceUpgradeCurrent)
	}
	target, err := exactUpgradeDefinition(ctx, s.catalog, current.SkillID, req.ToVersion, req.ToSourceIdentity)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("%w: exact target Skill definition is not installed", models.ErrSkillReferenceUpgradeInvalid)
	}
	targetSource := skill.DefinitionSourceIdentity(target)
	previous, err := exactUpgradeDefinition(ctx, s.catalog, current.SkillID, current.SkillVersion, current.SourceIdentity)
	if err != nil {
		return nil, err
	}
	if previous == nil {
		return nil, fmt.Errorf("%w: current Skill definition is not installed", models.ErrSkillReferenceUpgradeInvalid)
	}
	addedActions, err := grantedUpgradeActions(req.GrantActions, previous, target)
	if err != nil {
		return nil, err
	}
	// Every enabled binding at the current identity moves together. A binding
	// at the same version from another publisher cannot be told apart by
	// version-keyed references, so that remains a refusal.
	members := []*skill.Binding{current}
	for _, candidate := range bindings {
		if candidate == nil || candidate.Disabled || candidate.ID == current.ID || candidate.SkillID != current.SkillID ||
			candidate.SkillVersion != current.SkillVersion {
			continue
		}
		if candidate.SourceIdentity != current.SourceIdentity {
			return nil, fmt.Errorf("%w: durable references are ambiguous across bindings %s and %s", models.ErrSkillReferenceUpgradeInvalid, current.ID, candidate.ID)
		}
		members = append(members, cloneUpgradeBinding(candidate))
	}
	sort.Slice(members[1:], func(i, j int) bool { return members[1+i].ID < members[1+j].ID })
	siblings := make([]models.SkillReferenceBindingRevision, 0, len(members)-1)
	for _, member := range members[1:] {
		siblings = append(siblings, models.SkillReferenceBindingRevision{ID: member.ID, ExpectedRevision: member.Revision})
	}
	unionActions := make([]string, 0)
	seenActions := map[string]bool{}
	removedActions := make([]string, 0)
	seenRemoved := map[string]bool{}
	for _, member := range members {
		candidateBinding := cloneUpgradeBinding(member)
		candidateBinding.SkillVersion = target.Version
		candidateBinding.SourceIdentity = targetSource
		for _, action := range retainTargetActions(candidateBinding, target) {
			if !seenRemoved[action] {
				seenRemoved[action] = true
				removedActions = append(removedActions, action)
			}
		}
		addBindingActions(candidateBinding, addedActions)
		if err := s.catalog.ValidateBindingCandidate(ctx, candidateBinding); err != nil {
			return nil, fmt.Errorf("%w: target binding contract for %s: %v", models.ErrSkillReferenceUpgradeInvalid, member.ID, err)
		}
		for _, action := range candidateBinding.AllowedActions {
			if !seenActions[action] {
				seenActions[action] = true
				unionActions = append(unionActions, action)
			}
		}
	}

	conversationEndpoints, err := listUpgradeConversationEndpoints(ctx, s.store, req.Scope)
	if err != nil {
		return nil, err
	}
	conversationImpacts := make([]models.SkillReferenceConversationEndpointImpact, 0)
	for _, endpoint := range conversationEndpoints {
		if endpoint == nil || endpoint.DeploymentID != req.DeploymentID ||
			!upgradeConversationReferenceMatchesAny(endpoint.Adapter, members) {
			continue
		}
		adapter, ok := target.ConversationAdapters[endpoint.Adapter.AdapterID]
		if !ok || adapter.Provider != endpoint.Provider || !slices.Contains(adapter.EndpointModes, endpoint.Mode) ||
			endpoint.Policy.Validate(endpoint.Mode, adapter.Features) != nil {
			return nil, fmt.Errorf("%w: conversation endpoint %s is incompatible with target adapter %s", models.ErrSkillReferenceUpgradeInvalid, endpoint.ID, endpoint.Adapter.AdapterID)
		}
		conversationImpacts = append(conversationImpacts, models.SkillReferenceConversationEndpointImpact{
			ID: endpoint.ID, ExpectedRevision: endpoint.Revision, AdapterID: endpoint.Adapter.AdapterID,
		})
	}
	callbackRegistrations, err := listUpgradeCallbackRegistrations(ctx, s.store, req.Scope)
	if err != nil {
		return nil, err
	}
	callbackImpacts := make([]models.SkillReferenceCallbackRegistrationImpact, 0)
	for _, registration := range callbackRegistrations {
		if registration == nil || registration.DeploymentID != req.DeploymentID ||
			!upgradeCallbackReferenceMatchesAny(registration.Adapter, members) {
			continue
		}
		adapter, ok := target.CallbackAdapters[registration.Adapter.AdapterID]
		if !ok || adapter.Provider != registration.Provider || strings.TrimSpace(adapter.Transport.IngressEndpoint) == "" ||
			!callbackSubscriptionsSupported(registration.Subscriptions, adapter.EventTypes) {
			return nil, fmt.Errorf("%w: callback registration %s is incompatible with target adapter %s", models.ErrSkillReferenceUpgradeInvalid, registration.ID, registration.Adapter.AdapterID)
		}
		callbackImpacts = append(callbackImpacts, models.SkillReferenceCallbackRegistrationImpact{
			ID: registration.ID, ExpectedRevision: registration.Revision, AdapterID: registration.Adapter.AdapterID,
		})
	}

	objectiveImpacts := make([]models.SkillReferenceObjectiveImpact, 0)
	referencedActions := make(map[string]bool)
	sort.Strings(removedActions)
	sort.Slice(objectiveImpacts, func(i, j int) bool { return objectiveImpacts[i].ID < objectiveImpacts[j].ID })
	sort.Slice(conversationImpacts, func(i, j int) bool { return conversationImpacts[i].ID < conversationImpacts[j].ID })
	sort.Slice(callbackImpacts, func(i, j int) bool { return callbackImpacts[i].ID < callbackImpacts[j].ID })

	findings := compareUpgradeContracts(previous, target, unionActions, referencedActions)
	for _, member := range members {
		findings = append(findings, compareUpgradeAdapterContracts(previous, target, member)...)
	}
	if current.SourceIdentity != targetSource {
		findings = append(findings, models.SkillReferenceUpgradeFinding{Code: "source_identity_changed", From: current.SourceIdentity, To: targetSource, Message: "Skill publisher identity changed"})
	}
	plan := &models.SkillReferenceUpgradePlan{
		APIVersion: SkillReferenceUpgradeAPIVersion, Scope: req.Scope, DeploymentID: req.DeploymentID,
		BindingID: current.ID, ExpectedBindingRevision: current.Revision, Siblings: siblings,
		From:       models.SkillReferenceIdentity{ID: current.SkillID, Version: current.SkillVersion, SourceIdentity: current.SourceIdentity},
		To:         models.SkillReferenceIdentity{ID: target.ID, Version: target.Version, SourceIdentity: targetSource},
		Objectives: objectiveImpacts, ConversationEndpoints: conversationImpacts,
		CallbackRegistrations: callbackImpacts, RemovedActions: removedActions, AddedActions: addedActions, Findings: findings,
		ApprovalRequired: len(findings) > 0, GeneratedAt: s.now().UTC(),
	}
	plan.Digest, err = skillReferenceUpgradeDigest(plan)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func (s *SkillReferenceUpgradeService) Apply(ctx context.Context, req ApplySkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradeReceipt, error) {
	if req.Plan == nil || strings.TrimSpace(req.Plan.Digest) == "" {
		return nil, fmt.Errorf("%w: reviewed plan is required", models.ErrSkillReferenceUpgradeInvalid)
	}
	if strings.TrimSpace(req.Actor.Type) == "" || strings.TrimSpace(req.Actor.ID) == "" || strings.TrimSpace(req.Reason) == "" {
		return nil, fmt.Errorf("%w: actor and reason are required", models.ErrSkillReferenceUpgradeInvalid)
	}
	current, err := s.Plan(ctx, PlanSkillReferenceUpgradeRequest{
		Scope: req.Plan.Scope, DeploymentID: req.Plan.DeploymentID, BindingID: req.Plan.BindingID,
		ToVersion: req.Plan.To.Version, ToSourceIdentity: req.Plan.To.SourceIdentity, GrantActions: req.Plan.AddedActions,
	})
	if err != nil {
		return nil, err
	}
	if current.Digest != req.Plan.Digest {
		return nil, models.ErrSkillReferenceUpgradeConflict
	}
	if current.ApprovalRequired {
		if req.Approval == nil || strings.TrimSpace(req.Approval.Principal.Type) == "" ||
			strings.TrimSpace(req.Approval.Principal.ID) == "" || strings.TrimSpace(req.Approval.Reason) == "" {
			return nil, ErrSkillReferenceUpgradeApproval
		}
	}
	now := s.now().UTC()
	scope := skill.ScopeReference{Kind: current.Scope.Kind, ID: current.Scope.ID}
	bindings, err := s.store.ListSkillBindings(ctx, scope, current.DeploymentID)
	if err != nil {
		return nil, err
	}
	var binding *skill.Binding
	for _, candidate := range bindings {
		if candidate != nil && candidate.ID == current.BindingID {
			binding = cloneUpgradeBinding(candidate)
			break
		}
	}
	if binding == nil || binding.Revision != current.ExpectedBindingRevision {
		return nil, models.ErrSkillReferenceUpgradeConflict
	}
	upgradeBinding(binding, current, current.ExpectedBindingRevision, req, now)
	movedBindings := map[string]*skill.Binding{binding.ID: binding}
	previousRevisions := map[string]int64{binding.ID: current.ExpectedBindingRevision}
	siblingBindings := make([]*skill.Binding, 0, len(current.Siblings))
	for _, expected := range current.Siblings {
		var sibling *skill.Binding
		for _, candidate := range bindings {
			if candidate != nil && candidate.ID == expected.ID {
				sibling = cloneUpgradeBinding(candidate)
				break
			}
		}
		if sibling == nil || sibling.Revision != expected.ExpectedRevision {
			return nil, models.ErrSkillReferenceUpgradeConflict
		}
		upgradeBinding(sibling, current, expected.ExpectedRevision, req, now)
		movedBindings[sibling.ID] = sibling
		previousRevisions[sibling.ID] = expected.ExpectedRevision
		siblingBindings = append(siblingBindings, sibling)
	}

	objectiveCandidates := make([]models.SkillReferenceObjectiveMutation, 0, len(current.Objectives))
	activityIDs := make([]string, 0, len(current.Objectives))
	conversationCandidates := make([]models.SkillReferenceConversationEndpointMutation, 0, len(current.ConversationEndpoints))
	for _, impact := range current.ConversationEndpoints {
		endpoint, loadErr := s.store.GetExternalConversationEndpoint(ctx, current.Scope, impact.ID)
		if loadErr != nil || endpoint == nil || endpoint.Revision != impact.ExpectedRevision {
			return nil, models.ErrSkillReferenceUpgradeConflict
		}
		owner := movedBindings[endpoint.Adapter.BindingID]
		if owner == nil || !upgradeConversationReferenceMatches(endpoint.Adapter, bindingBeforeUpgrade(owner, current, previousRevisions[owner.ID])) {
			return nil, models.ErrSkillReferenceUpgradeConflict
		}
		next := models.CloneExternalConversationEndpoint(endpoint)
		next.Adapter.SkillVersion = current.To.Version
		next.Adapter.SourceIdentity = current.To.SourceIdentity
		next.Adapter.BindingRevision = owner.Revision
		next.Revision++
		next.UpdatedAt = now
		conversationCandidates = append(conversationCandidates, models.SkillReferenceConversationEndpointMutation{Value: next, ExpectedRevision: impact.ExpectedRevision})
	}
	callbackCandidates := make([]models.SkillReferenceCallbackRegistrationMutation, 0, len(current.CallbackRegistrations))
	for _, impact := range current.CallbackRegistrations {
		registration, loadErr := s.store.GetCallbackRegistration(ctx, current.Scope, impact.ID)
		if loadErr != nil || registration == nil || registration.Revision != impact.ExpectedRevision {
			return nil, models.ErrSkillReferenceUpgradeConflict
		}
		owner := movedBindings[registration.Adapter.BindingID]
		if owner == nil || !upgradeCallbackReferenceMatches(registration.Adapter, bindingBeforeUpgrade(owner, current, previousRevisions[owner.ID])) {
			return nil, models.ErrSkillReferenceUpgradeConflict
		}
		next := models.CloneCallbackRegistration(registration)
		next.Adapter.SkillVersion = current.To.Version
		next.Adapter.SourceIdentity = current.To.SourceIdentity
		next.Adapter.BindingRevision = owner.Revision
		next.Revision++
		next.UpdatedAt = now
		next.Lifecycle = append(next.Lifecycle, models.CallbackRegistrationLifecycleEntry{
			Revision: next.Revision, Action: models.CallbackRegistrationUpdated, Actor: req.Actor, Reason: req.Reason, At: now,
		})
		callbackCandidates = append(callbackCandidates, models.SkillReferenceCallbackRegistrationMutation{Value: next, ExpectedRevision: impact.ExpectedRevision})
	}
	receipt := &models.SkillReferenceUpgradeReceipt{
		APIVersion: SkillReferenceUpgradeAPIVersion, PlanDigest: current.Digest, Scope: current.Scope,
		DeploymentID: current.DeploymentID, BindingID: current.BindingID, BindingRevision: binding.Revision, Siblings: current.Siblings,
		From: current.From, To: current.To, Objectives: current.Objectives,
		ConversationEndpoints: current.ConversationEndpoints, CallbackRegistrations: current.CallbackRegistrations,
		Actor: req.Actor, Reason: req.Reason, Approval: req.Approval, ActivityIDs: activityIDs, AppliedAt: now,
	}
	if err := s.store.ApplySkillReferenceUpgrade(ctx, &models.SkillReferenceUpgradeMutation{
		Plan: current, Binding: binding, Siblings: siblingBindings, Objectives: objectiveCandidates,
		ConversationEndpoints: conversationCandidates, CallbackRegistrations: callbackCandidates, Receipt: receipt,
	}); err != nil {
		return nil, err
	}
	return receipt, nil
}

// Connector references follow the stable binding within the same executable
// identity. Account configuration updates can advance its revision before the
// next Skill upgrade; those older references must move with it as well. The
// caller verifies scope, deployment and adapter compatibility independently.
func upgradeConversationReferenceMatches(ref models.ExternalConversationAdapterReference, binding *skill.Binding) bool {
	return binding != nil && ref.SkillID == binding.SkillID && ref.SkillVersion == binding.SkillVersion &&
		ref.SourceIdentity == binding.SourceIdentity && ref.BindingID == binding.ID &&
		ref.BindingRevision >= 1 && ref.BindingRevision <= binding.Revision
}

func bindingBeforeUpgrade(binding *skill.Binding, plan *models.SkillReferenceUpgradePlan, expectedRevision int64) *skill.Binding {
	previous := cloneUpgradeBinding(binding)
	previous.SkillVersion = plan.From.Version
	previous.SourceIdentity = plan.From.SourceIdentity
	previous.Revision = expectedRevision
	return previous
}

// upgradeBinding moves one binding to the plan's target identity and records
// the upgrade in its lifecycle with that binding's own prior revision.
func upgradeBinding(binding *skill.Binding, plan *models.SkillReferenceUpgradePlan, expectedRevision int64, req ApplySkillReferenceUpgradeRequest, now time.Time) {
	binding.SkillVersion = plan.To.Version
	binding.SourceIdentity = plan.To.SourceIdentity
	dropBindingActions(binding, plan.RemovedActions)
	addBindingActions(binding, plan.AddedActions)
	binding.Revision++
	// Bindings created by an atomic workforce apply predate the management
	// lifecycle until their first explicit mutation. Establish the lifecycle
	// clock at that boundary instead of persisting an updated entry alongside a
	// zero creation timestamp, which would make the binding unreadable.
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = now
	}
	binding.UpdatedAt = now
	binding.Lifecycle = append(binding.Lifecycle, capability.BindingLifecycleEntry{
		Revision: binding.Revision, Action: capability.BindingLifecycleUpdated,
		Actor: capability.BindingActor{Type: req.Actor.Type, ID: req.Actor.ID}, Reason: req.Reason, At: now,
		SkillUpgrade: &capability.BindingSkillUpgradeProvenance{
			From:                    capability.NewSkillIdentity(plan.From.ID, plan.From.Version, plan.From.SourceIdentity),
			To:                      capability.NewSkillIdentity(plan.To.ID, plan.To.Version, plan.To.SourceIdentity),
			ExpectedBindingRevision: expectedRevision, PlanDigest: plan.Digest,
		},
	})
}

// retainTargetActions drops from a binding every allowed action that the
// target Skill version no longer defines, together with that action's argument
// restrictions and bindings, and returns the dropped names. A removed action
// cannot run on the target, so following the newest version simply stops
// granting it instead of refusing the upgrade.
func retainTargetActions(binding *skill.Binding, target *skill.Definition) []string {
	removed := make([]string, 0)
	for _, action := range binding.AllowedActions {
		if _, ok := target.Actions[action]; !ok {
			removed = append(removed, action)
		}
	}
	dropBindingActions(binding, removed)
	return removed
}

// grantedUpgradeActions returns the granted actions that the target defines and
// the from version did not, sorted. Only kernel Skills accept grants: their
// actions are platform features the platform decides to hand out, while a
// published Skill's new actions are for its binding owner to enable.
func grantedUpgradeActions(grant []string, previous, target *skill.Definition) ([]string, error) {
	if len(grant) == 0 {
		return nil, nil
	}
	if target.Transport.Kind != "kernel" {
		return nil, fmt.Errorf("%w: only kernel Skills accept granted actions", models.ErrSkillReferenceUpgradeInvalid)
	}
	seen := make(map[string]bool, len(grant))
	added := make([]string, 0, len(grant))
	for _, action := range grant {
		action = strings.TrimSpace(action)
		if action == "" || seen[action] {
			continue
		}
		seen[action] = true
		if _, inTarget := target.Actions[action]; !inTarget {
			continue
		}
		if _, inPrevious := previous.Actions[action]; inPrevious {
			continue
		}
		added = append(added, action)
	}
	if len(added) == 0 {
		return nil, nil
	}
	sort.Strings(added)
	return added, nil
}

// addBindingActions grants each action the binding does not already allow.
func addBindingActions(binding *skill.Binding, actions []string) {
	for _, action := range actions {
		allowed := false
		for _, existing := range binding.AllowedActions {
			if existing == action {
				allowed = true
				break
			}
		}
		if !allowed {
			binding.AllowedActions = append(binding.AllowedActions, action)
		}
	}
}

func dropBindingActions(binding *skill.Binding, actions []string) {
	if len(actions) == 0 {
		return
	}
	drop := make(map[string]bool, len(actions))
	for _, action := range actions {
		drop[action] = true
	}
	kept := make([]string, 0, len(binding.AllowedActions))
	for _, action := range binding.AllowedActions {
		if !drop[action] {
			kept = append(kept, action)
		}
	}
	binding.AllowedActions = kept
	for action := range drop {
		delete(binding.ArgumentRestrictions, action)
		delete(binding.ArgumentBindings, action)
	}
	if len(binding.ArgumentRestrictions) == 0 {
		binding.ArgumentRestrictions = nil
	}
	if len(binding.ArgumentBindings) == 0 {
		binding.ArgumentBindings = nil
	}
}

func upgradeConversationReferenceMatchesAny(ref models.ExternalConversationAdapterReference, members []*skill.Binding) bool {
	for _, member := range members {
		if upgradeConversationReferenceMatches(ref, member) {
			return true
		}
	}
	return false
}

func upgradeCallbackReferenceMatchesAny(ref models.CallbackAdapterReference, members []*skill.Binding) bool {
	for _, member := range members {
		if upgradeCallbackReferenceMatches(ref, member) {
			return true
		}
	}
	return false
}

func upgradeCallbackReferenceMatches(ref models.CallbackAdapterReference, binding *skill.Binding) bool {
	return binding != nil && ref.SkillID == binding.SkillID && ref.SkillVersion == binding.SkillVersion &&
		ref.SourceIdentity == binding.SourceIdentity && ref.BindingID == binding.ID &&
		ref.BindingRevision >= 1 && ref.BindingRevision <= binding.Revision
}

func callbackSubscriptionsSupported(subscriptions []models.CallbackSubscription, eventTypes []string) bool {
	supported := make(map[string]bool, len(eventTypes))
	for _, eventType := range eventTypes {
		supported[eventType] = true
	}
	for _, subscription := range subscriptions {
		if !supported[subscription.EventType] {
			return false
		}
	}
	return true
}

func listUpgradeConversationEndpoints(ctx context.Context, store models.ExternalConversationEndpointStore, scope models.Scope) ([]*models.ExternalConversationEndpoint, error) {
	result := make([]*models.ExternalConversationEndpoint, 0)
	for offset := 0; ; offset += 500 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := store.ListExternalConversationEndpoints(ctx, models.ExternalConversationEndpointFilter{
			Scope: scope, Statuses: []models.ExternalConversationEndpointStatus{models.ExternalConversationEndpointActive, models.ExternalConversationEndpointPaused}, Limit: 500, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		if len(page) > 500 {
			return nil, fmt.Errorf("%w: conversation reference page exceeds its requested limit", models.ErrSkillReferenceUpgradeInvalid)
		}
		result = append(result, page...)
		if len(page) < 500 {
			return result, nil
		}
	}
}

func listUpgradeCallbackRegistrations(ctx context.Context, store models.CallbackRegistrationStore, scope models.Scope) ([]*models.CallbackRegistration, error) {
	result := make([]*models.CallbackRegistration, 0)
	for offset := 0; ; offset += 500 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := store.ListCallbackRegistrations(ctx, models.CallbackRegistrationFilter{
			Scope: scope, Statuses: []models.CallbackRegistrationStatus{models.CallbackRegistrationActive, models.CallbackRegistrationPaused}, Limit: 500, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		if len(page) > 500 {
			return nil, fmt.Errorf("%w: callback reference page exceeds its requested limit", models.ErrSkillReferenceUpgradeInvalid)
		}
		result = append(result, page...)
		if len(page) < 500 {
			return result, nil
		}
	}
}

func referenceOwnedOrAssigned(owner models.ObjectiveOwner, assignedID, deploymentID string) bool {
	return owner.ID == deploymentID || assignedID == deploymentID
}

func exactUpgradeDefinition(ctx context.Context, catalog *skill.Catalog, id, version, sourceIdentity string) (*skill.Definition, error) {
	if strings.TrimSpace(sourceIdentity) == "" {
		return catalog.GetDefinition(ctx, id, version)
	}
	return catalog.GetDefinitionVariant(ctx, id, version, sourceIdentity)
}

func skillReferenceUpgradeActivityPayload(plan *models.SkillReferenceUpgradePlan, reason string) map[string]interface{} {
	return map[string]interface{}{
		"planDigest":   plan.Digest,
		"bindingId":    plan.BindingID,
		"deploymentId": plan.DeploymentID,
		"from":         map[string]interface{}{"id": plan.From.ID, "version": plan.From.Version, "sourceIdentity": plan.From.SourceIdentity},
		"to":           map[string]interface{}{"id": plan.To.ID, "version": plan.To.Version, "sourceIdentity": plan.To.SourceIdentity},
		"reason":       reason,
	}
}

func compareUpgradeContracts(previous, target *skill.Definition, bindingActions []string, referenced map[string]bool) []models.SkillReferenceUpgradeFinding {
	actions := make(map[string]bool, len(bindingActions)+len(referenced))
	for _, action := range bindingActions {
		actions[action] = true
	}
	for action := range referenced {
		actions[action] = true
	}
	names := make([]string, 0, len(actions))
	for action := range actions {
		names = append(names, action)
	}
	sort.Strings(names)
	findings := make([]models.SkillReferenceUpgradeFinding, 0)
	if len(previous.Requirements.Storage) != len(target.Requirements.Storage) || len(previous.Requirements.Storage) > 0 && !reflect.DeepEqual(previous.Requirements.Storage, target.Requirements.Storage) {
		findings = append(findings, models.SkillReferenceUpgradeFinding{
			Code: "storage_contract_changed", Message: "Skill storage requirements changed and need an explicit migration review",
		})
	}
	for _, name := range names {
		before, beforeOK := previous.Actions[name]
		after, afterOK := target.Actions[name]
		if !beforeOK || !afterOK {
			continue
		}
		if after.Risk.Rank() > before.Risk.Rank() {
			findings = append(findings, models.SkillReferenceUpgradeFinding{
				Code: "risk_increased", Subject: name, From: string(before.Risk), To: string(after.Risk), Message: fmt.Sprintf("%s risk changes from %s to %s", name, before.Risk, after.Risk),
			})
		}
		if !compatibleUpgradeInputSchema(before.InputSchema, after.InputSchema) || !reflect.DeepEqual(before.OutputSchema, after.OutputSchema) ||
			!reflect.DeepEqual(before.Credentials, after.Credentials) || before.SideEffect != after.SideEffect ||
			before.Idempotency != after.Idempotency || before.ExternalOperationPolicy != after.ExternalOperationPolicy ||
			!reflect.DeepEqual(before.Transport, after.Transport) || !reflect.DeepEqual(previous.Transport, target.Transport) {
			findings = append(findings, models.SkillReferenceUpgradeFinding{
				Code: "action_contract_changed", Subject: name, Message: fmt.Sprintf("%s action contract changed", name),
			})
		}
	}
	return append(findings, compareUpgradeConfigContracts(previous, target)...)
}

// compareUpgradeConfigContracts reports operator-config changes that can
// invalidate stored values or change who receives them. Adding an optional or
// defaulted field, and changing labels, descriptions or setup text, keep every
// stored value valid and need no review. A Skill's first config declaration
// needs no review either: its values are migrated from platform settings, so
// agents see no behavior change.
func compareUpgradeConfigContracts(previous, target *skill.Definition) []models.SkillReferenceUpgradeFinding {
	findings := make([]models.SkillReferenceUpgradeFinding, 0)
	if previous.Config == nil {
		return findings
	}
	var beforeScopes, afterScopes []skill.SkillConfigScope
	var beforeFields, afterFields []skill.SkillConfigField
	if previous.Config != nil {
		beforeScopes, beforeFields = previous.Config.Scopes, previous.Config.Fields
	}
	if target.Config != nil {
		afterScopes, afterFields = target.Config.Scopes, target.Config.Fields
	}
	changed := func(subject, from, to, message string) {
		findings = append(findings, models.SkillReferenceUpgradeFinding{Code: "config_contract_changed", Subject: subject, From: from, To: to, Message: message})
	}
	if !reflect.DeepEqual(configScopeStrings(beforeScopes), configScopeStrings(afterScopes)) {
		changed("config.scopes", strings.Join(configScopeStrings(beforeScopes), ","), strings.Join(configScopeStrings(afterScopes), ","), "Skill config scopes changed")
	}
	before := make(map[string]skill.SkillConfigField, len(beforeFields))
	for _, field := range beforeFields {
		before[field.Key] = field
	}
	after := make(map[string]skill.SkillConfigField, len(afterFields))
	for _, field := range afterFields {
		after[field.Key] = field
	}
	keys := make([]string, 0, len(before)+len(after))
	for key := range before {
		keys = append(keys, key)
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		old, hadOld := before[key]
		updated, hasNew := after[key]
		switch {
		case !hasNew:
			changed(key, string(old.Type), "", fmt.Sprintf("config field %s was removed", key))
		case !hadOld:
			if configFieldMandatory(updated) && updated.Default == nil {
				changed(key, "", string(updated.Type), fmt.Sprintf("config field %s was added as required without a default", key))
			}
		default:
			if old.Type != updated.Type {
				changed(key, string(old.Type), string(updated.Type), fmt.Sprintf("config field %s type changes from %s to %s", key, old.Type, updated.Type))
			}
			if removed := removedConfigEnumValues(old.Enum, updated.Enum); len(removed) > 0 {
				changed(key, strings.Join(old.Enum, ","), strings.Join(updated.Enum, ","), fmt.Sprintf("config field %s enum values removed: %s", key, strings.Join(removed, ",")))
			}
			if !configBoundEqual(old.Min, updated.Min) {
				changed(key+".min", configBoundString(old.Min), configBoundString(updated.Min), fmt.Sprintf("config field %s minimum changed", key))
			}
			if !configBoundEqual(old.Max, updated.Max) {
				changed(key+".max", configBoundString(old.Max), configBoundString(updated.Max), fmt.Sprintf("config field %s maximum changed", key))
			}
			if old.Pattern != updated.Pattern {
				changed(key+".pattern", old.Pattern, updated.Pattern, fmt.Sprintf("config field %s pattern changed", key))
			}
			oldScopes := configScopeStrings(old.EffectiveScopes(beforeScopes))
			newScopes := configScopeStrings(updated.EffectiveScopes(afterScopes))
			if !reflect.DeepEqual(oldScopes, newScopes) {
				changed(key, strings.Join(oldScopes, ","), strings.Join(newScopes, ","), fmt.Sprintf("config field %s scopes changed", key))
			}
			if old.EffectiveAudience() != updated.EffectiveAudience() {
				changed(key, string(old.EffectiveAudience()), string(updated.EffectiveAudience()), fmt.Sprintf("config field %s audience changes from %s to %s", key, old.EffectiveAudience(), updated.EffectiveAudience()))
			}
			if updated.Default == nil && configFieldMandatory(updated) &&
				(!configFieldMandatory(old) || old.Default != nil || !reflect.DeepEqual(old.RequiredWhen, updated.RequiredWhen) || old.Required != updated.Required) {
				changed(key, "", "", fmt.Sprintf("config field %s became required without a default", key))
			}
		}
	}
	return findings
}

// removedConfigEnumValues returns values of before that after no longer
// accepts. Adding values keeps every stored value valid.
func removedConfigEnumValues(before, after []string) []string {
	kept := make(map[string]bool, len(after))
	for _, value := range after {
		kept[value] = true
	}
	removed := make([]string, 0)
	for _, value := range before {
		if !kept[value] {
			removed = append(removed, value)
		}
	}
	return removed
}

func configBoundEqual(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func configBoundString(bound *int64) string {
	if bound == nil {
		return ""
	}
	return strconv.FormatInt(*bound, 10)
}

func configFieldMandatory(field skill.SkillConfigField) bool {
	return field.Required || len(field.RequiredWhen) > 0
}

func configScopeStrings(scopes []skill.SkillConfigScope) []string {
	values := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		values = append(values, string(scope))
	}
	sort.Strings(values)
	return values
}

// Descriptive text can change without changing an adapter's authority. Every
// executable adapter field, including receipt projections and credential
// selection, must remain identical for an unattended upgrade.
func compareUpgradeAdapterContracts(previous, target *skill.Definition, binding *skill.Binding) []models.SkillReferenceUpgradeFinding {
	findings := make([]models.SkillReferenceUpgradeFinding, 0)
	conversationIDs := append([]string(nil), binding.EnabledConversationAdapters...)
	sort.Strings(conversationIDs)
	for _, id := range conversationIDs {
		before, beforeOK := previous.ConversationAdapters[id]
		after, afterOK := target.ConversationAdapters[id]
		before.Name, before.Description, after.Name, after.Description = "", "", "", ""
		// Feature additions retain the old interface; removing one breaks it.
		if featureSubset(before.Features, after.Features) {
			before.Features, after.Features = nil, nil
		}

		if !beforeOK || !afterOK || !reflect.DeepEqual(before, after) {
			findings = append(findings, models.SkillReferenceUpgradeFinding{Code: "conversation_adapter_contract_changed", Subject: id, Message: fmt.Sprintf("%s conversation adapter contract changed", id)})
		}
	}
	callbackIDs := append([]string(nil), binding.EnabledCallbackAdapters...)
	sort.Strings(callbackIDs)
	for _, id := range callbackIDs {
		before, beforeOK := previous.CallbackAdapters[id]
		after, afterOK := target.CallbackAdapters[id]
		before.Name, before.Description, after.Name, after.Description = "", "", "", ""
		if !beforeOK || !afterOK || !reflect.DeepEqual(before, after) {
			findings = append(findings, models.SkillReferenceUpgradeFinding{Code: "callback_adapter_contract_changed", Subject: id, Message: fmt.Sprintf("%s callback adapter contract changed", id)})
		}
	}
	return findings
}

func featureSubset(before, after []capability.ConversationAdapterFeature) bool {
	for _, feature := range before {
		found := false
		for _, candidate := range after {
			if candidate == feature {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Only optional properties added to a closed object are automatically compatible.
// Existing constraints (including required fields and cross-field rules) must
// remain identical. Open objects could previously accept values rejected by a
// newly declared property, so they deliberately retain exact comparison.
func compatibleUpgradeInputSchema(before, after map[string]interface{}) bool {
	if reflect.DeepEqual(before, after) {
		return true
	}
	if before["type"] != "object" || after["type"] != "object" || before["additionalProperties"] != false || after["additionalProperties"] != false {
		return false
	}
	oldProps, ok := before["properties"].(map[string]interface{})
	if !ok {
		return false
	}
	newProps, ok := after["properties"].(map[string]interface{})
	if !ok || len(newProps) < len(oldProps) {
		return false
	}
	// Reject cross-property applicators; adding a named property can alter their
	// meaning even when their JSON remains unchanged.
	for key := range before {
		switch key {
		case "type", "properties", "required", "additionalProperties", "title", "description", "$comment", "minProperties", "maxProperties":
		default:
			return false
		}
	}
	for key, value := range oldProps {
		if !reflect.DeepEqual(value, newProps[key]) {
			return false
		}
	}
	oldRest, newRest := make(map[string]interface{}), make(map[string]interface{})
	for key, value := range before {
		if key != "properties" {
			oldRest[key] = value
		}
	}
	for key, value := range after {
		if key != "properties" {
			newRest[key] = value
		}
	}
	return reflect.DeepEqual(oldRest, newRest)
}

func skillReferenceUpgradeDigest(plan *models.SkillReferenceUpgradePlan) (string, error) {
	copy := *plan
	copy.Digest = ""
	copy.GeneratedAt = time.Time{}
	payload, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func cloneUpgradeBinding(value *skill.Binding) *skill.Binding {
	if value == nil {
		return nil
	}
	payload, _ := json.Marshal(value)
	var copy skill.Binding
	_ = json.Unmarshal(payload, &copy)
	return &copy
}
