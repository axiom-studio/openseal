package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/team"
)

// Placement contains target-owned identities and authority bindings. It is
// deliberately separate from the signed portable artifact.
type Placement struct {
	Agents     map[string]agent.BundlePlacement `json:"agents"`
	Teams      map[string]TeamPlacement         `json:"teams,omitempty"`
	Objectives map[string]string                `json:"objectives,omitempty"`
	Runbooks   map[string]string                `json:"runbooks,omitempty"`
}

type TeamPlacement struct {
	DeploymentID string `json:"deploymentId"`
}

type Requirement struct {
	ResourceType string `json:"resourceType"`
	ResourceKey  string `json:"resourceKey"`
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	Required     bool   `json:"required"`
	Resolved     bool   `json:"resolved"`
	Message      string `json:"message,omitempty"`
}

type InstallationPreview struct {
	BundleDigest string        `json:"bundleDigest"`
	Ready        bool          `json:"ready"`
	Requirements []Requirement `json:"requirements,omitempty"`
}

type InstallationRequest struct {
	Bundle         *Bundle
	TrustPolicy    TrustPolicy
	Scope          capability.ScopeReference
	Placement      Placement
	ActorType      string
	ActorID        string
	Reason         string
	IdempotencyKey string
}

// InstallationPlan is the complete desired state submitted to one atomic
// target transaction. Implementations must never apply only a prefix.
type InstallationPlan struct {
	BundleID       string                       `json:"bundleId"`
	BundleVersion  string                       `json:"bundleVersion"`
	BundleDigest   string                       `json:"bundleDigest"`
	Scope          capability.ScopeReference    `json:"scope"`
	Agents         []AgentInstallation          `json:"agents,omitempty"`
	Teams          []TeamInstallation           `json:"teams,omitempty"`
	Objectives     []*runtime.Objective         `json:"objectives,omitempty"`
	Runbooks       []*runtime.RunbookActivation `json:"runbooks,omitempty"`
	ActorType      string                       `json:"actorType"`
	ActorID        string                       `json:"actorId"`
	Reason         string                       `json:"reason"`
	IdempotencyKey string                       `json:"idempotencyKey"`
	PlanDigest     string                       `json:"planDigest"`
}

type AgentInstallation struct {
	Key  string                        `json:"key"`
	Plan *agent.BundleInstallationPlan `json:"plan"`
}

type TeamInstallation struct {
	Key        string           `json:"key"`
	Definition *team.Definition `json:"definition"`
	Deployment *team.Deployment `json:"deployment"`
}

type InstallationReceipt struct {
	BundleID       string    `json:"bundleId"`
	BundleVersion  string    `json:"bundleVersion"`
	BundleDigest   string    `json:"bundleDigest"`
	PlanDigest     string    `json:"planDigest"`
	IdempotencyKey string    `json:"idempotencyKey"`
	AppliedAt      time.Time `json:"appliedAt"`
}

// InstallationStore is intentionally one method: the transaction boundary is
// the whole workforce, not an Agent, Team, Objective, or Runbook at a time.
type InstallationStore interface {
	ApplyWorkforceBundle(context.Context, *InstallationPlan) (*InstallationReceipt, error)
}

func PreviewInstallation(bundle *Bundle, placement Placement) (*InstallationPreview, error) {
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	preview := &InstallationPreview{BundleDigest: bundle.Digest, Ready: true}
	appendRequirement := func(value Requirement) {
		preview.Requirements = append(preview.Requirements, value)
		if value.Required && !value.Resolved {
			preview.Ready = false
		}
	}
	for _, item := range bundle.Agents {
		selected, present := placement.Agents[item.Key]
		if !present {
			appendRequirement(Requirement{ResourceType: "agent", ResourceKey: item.Key, Kind: "placement", ID: "target", Required: true, Message: "Choose a target Agent identity and environment."})
			continue
		}
		result, err := agent.PreviewBundleInstallation(item.Artifact, selected)
		if err != nil {
			return nil, fmt.Errorf("preview workforce bundle Agent %s: %w", item.Key, err)
		}
		for _, requirement := range result.Requirements {
			appendRequirement(Requirement{ResourceType: "agent", ResourceKey: item.Key, Kind: string(requirement.Kind), ID: requirement.ID, Required: requirement.Required, Resolved: requirement.Resolved, Message: requirement.Message})
		}
	}
	for _, item := range bundle.Teams {
		selected := strings.TrimSpace(placement.Teams[item.Key].DeploymentID)
		appendRequirement(Requirement{ResourceType: "team", ResourceKey: item.Key, Kind: "deployment", ID: "target", Required: true, Resolved: selected != "", Message: "Choose a target Team identity."})
	}
	for _, item := range bundle.Objectives {
		selected := strings.TrimSpace(placement.Objectives[item.Key])
		appendRequirement(Requirement{ResourceType: "objective", ResourceKey: item.Key, Kind: "identity", ID: "target", Required: true, Resolved: selected != "", Message: "Choose a target Objective identity."})
	}
	for _, item := range bundle.Runbooks {
		selected := strings.TrimSpace(placement.Runbooks[item.Key])
		appendRequirement(Requirement{ResourceType: "runbook", ResourceKey: item.Key, Kind: "activation", ID: "target", Required: true, Resolved: selected != "", Message: "Choose a target Runbook activation identity."})
	}
	if err := validatePlacementIdentities(bundle, placement); err != nil {
		return nil, err
	}
	sort.Slice(preview.Requirements, func(i, j int) bool {
		left, right := preview.Requirements[i], preview.Requirements[j]
		return left.ResourceType+"\x00"+left.ResourceKey+"\x00"+left.Kind+"\x00"+left.ID < right.ResourceType+"\x00"+right.ResourceKey+"\x00"+right.Kind+"\x00"+right.ID
	})
	return preview, nil
}

func CompileInstallation(request InstallationRequest) (*InstallationPlan, error) {
	if _, err := Verify(request.Bundle, request.TrustPolicy); err != nil {
		return nil, fmt.Errorf("verify workforce bundle installation: %w", err)
	}
	preview, err := PreviewInstallation(request.Bundle, request.Placement)
	if err != nil {
		return nil, err
	}
	if !preview.Ready {
		return nil, errors.New("workforce bundle installation has unresolved target requirements")
	}
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" || strings.TrimSpace(request.ActorType) == "" || strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, errors.New("workforce bundle installation requires scope, actor, and idempotency key")
	}
	plan := &InstallationPlan{
		BundleID: request.Bundle.Metadata.ID, BundleVersion: request.Bundle.Metadata.Version, BundleDigest: request.Bundle.Digest,
		Scope: request.Scope, ActorType: strings.TrimSpace(request.ActorType), ActorID: strings.TrimSpace(request.ActorID),
		Reason: strings.TrimSpace(request.Reason), IdempotencyKey: strings.TrimSpace(request.IdempotencyKey),
	}
	for _, item := range request.Bundle.Agents {
		compiled, err := agent.CompileBundleInstallation(agent.BundleInstallationRequest{
			Bundle: item.Artifact, Scope: request.Scope, Placement: request.Placement.Agents[item.Key], ActorType: plan.ActorType, ActorID: plan.ActorID,
			Reason: plan.Reason, IdempotencyKey: childIdempotencyKey(plan.IdempotencyKey, "agent", item.Key),
		})
		if err != nil {
			return nil, fmt.Errorf("compile workforce bundle Agent %s: %w", item.Key, err)
		}
		plan.Agents = append(plan.Agents, AgentInstallation{Key: item.Key, Plan: compiled})
	}
	for _, item := range request.Bundle.Teams {
		definition, err := cloneTeamDefinition(item.Definition)
		if err != nil {
			return nil, fmt.Errorf("clone workforce bundle Team %s: %w", item.Key, err)
		}
		roster := make([]team.RosterAssignment, 0, len(item.Deployment.Roster))
		for _, assignment := range item.Deployment.Roster {
			roster = append(roster, team.RosterAssignment{ID: assignment.ID, RoleID: assignment.RoleID, AgentDeploymentID: request.Placement.Agents[assignment.AgentKey].DeploymentID, DisplayName: assignment.DisplayName})
		}
		deployment := &team.Deployment{ID: request.Placement.Teams[item.Key].DeploymentID, Scope: request.Scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, Roster: roster, Restrictions: item.Deployment.Restrictions, Status: team.DeploymentActive, Revision: 1, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
		if err := deployment.Validate(definition); err != nil {
			return nil, fmt.Errorf("compile workforce bundle Team %s: %w", item.Key, err)
		}
		plan.Teams = append(plan.Teams, TeamInstallation{Key: item.Key, Definition: definition, Deployment: deployment})
	}
	for _, item := range request.Bundle.Objectives {
		ownerID := targetOwnerID(item.Owner, request.Placement)
		objective := &runtime.Objective{ID: request.Placement.Objectives[item.Key], Scope: runtime.Scope{Kind: request.Scope.Kind, ID: request.Scope.ID}, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerType(item.Owner.Kind), ID: ownerID}, Title: item.Title, Goal: item.Goal, Status: item.Status, Priority: item.Priority, ExecutionPolicy: item.ExecutionPolicy, Budget: item.Budget, BudgetAllocations: item.BudgetAllocations, Constraints: item.Constraints, SuccessCriteria: item.SuccessCriteria, Revision: 1, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
		if err := objective.Validate(); err != nil {
			return nil, fmt.Errorf("compile workforce bundle Objective %s: %w", item.Key, err)
		}
		objective, err = cloneJSON(objective)
		if err != nil {
			return nil, fmt.Errorf("clone workforce bundle Objective %s: %w", item.Key, err)
		}
		plan.Objectives = append(plan.Objectives, objective)
	}
	for _, item := range request.Bundle.Runbooks {
		activation := &runtime.RunbookActivation{ID: request.Placement.Runbooks[item.Key], Scope: runtime.Scope{Kind: request.Scope.Kind, ID: request.Scope.ID}, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerType(item.Owner.Kind), ID: targetOwnerID(item.Owner, request.Placement)}, ObjectiveID: request.Placement.Objectives[item.ObjectiveKey], AssignedAgentID: request.Placement.Agents[item.AssignedAgentKey].DeploymentID, DefinitionID: item.DefinitionID, DefinitionVersion: item.DefinitionVersion, TriggerID: item.TriggerID, Trigger: item.Trigger, Input: item.Input, Policy: item.Policy, Budget: item.Budget, MaximumConcurrent: item.MaximumConcurrent, Status: item.Status, Revision: 1, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
		if err := activation.Validate(); err != nil {
			return nil, fmt.Errorf("compile workforce bundle Runbook %s: %w", item.Key, err)
		}
		activation, err = cloneJSON(activation)
		if err != nil {
			return nil, fmt.Errorf("clone workforce bundle Runbook %s: %w", item.Key, err)
		}
		plan.Runbooks = append(plan.Runbooks, activation)
	}
	canonicalizeInstallationPlan(plan)
	digest, err := installationPlanDigest(plan)
	if err != nil {
		return nil, err
	}
	plan.PlanDigest = digest
	return plan, nil
}

func Install(ctx context.Context, store InstallationStore, request InstallationRequest) (*InstallationReceipt, error) {
	if store == nil {
		return nil, errors.New("workforce bundle installation store is required")
	}
	plan, err := CompileInstallation(request)
	if err != nil {
		return nil, err
	}
	receipt, err := store.ApplyWorkforceBundle(ctx, plan)
	if err != nil {
		return nil, err
	}
	if receipt == nil || receipt.BundleID != plan.BundleID || receipt.BundleVersion != plan.BundleVersion || receipt.BundleDigest != plan.BundleDigest || receipt.PlanDigest != plan.PlanDigest || receipt.IdempotencyKey != plan.IdempotencyKey || receipt.AppliedAt.IsZero() {
		return nil, errors.New("workforce bundle installation store returned an invalid receipt")
	}
	return receipt, nil
}

func validatePlacementIdentities(bundle *Bundle, placement Placement) error {
	seen := map[string]string{}
	add := func(kind, key, id string) error {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil
		}
		identity := kind + "\x00" + id
		if previous := seen[identity]; previous != "" {
			return fmt.Errorf("workforce bundle placement reuses %s identity %s for %s and %s", kind, id, previous, key)
		}
		seen[identity] = key
		return nil
	}
	for _, item := range bundle.Agents {
		if err := add("agent", item.Key, placement.Agents[item.Key].DeploymentID); err != nil {
			return err
		}
	}
	for _, item := range bundle.Teams {
		if err := add("team", item.Key, placement.Teams[item.Key].DeploymentID); err != nil {
			return err
		}
	}
	for _, item := range bundle.Objectives {
		if err := add("objective", item.Key, placement.Objectives[item.Key]); err != nil {
			return err
		}
	}
	for _, item := range bundle.Runbooks {
		if err := add("runbook", item.Key, placement.Runbooks[item.Key]); err != nil {
			return err
		}
	}
	return nil
}

func targetOwnerID(owner OwnerReference, placement Placement) string {
	if owner.Kind == OwnerAgent {
		return placement.Agents[owner.Key].DeploymentID
	}
	return placement.Teams[owner.Key].DeploymentID
}

func childIdempotencyKey(parent, kind, key string) string {
	digest := sha256.Sum256([]byte(parent + "\x00" + kind + "\x00" + key))
	return parent + ":" + hex.EncodeToString(digest[:8])
}

func canonicalizeInstallationPlan(plan *InstallationPlan) {
	sort.Slice(plan.Agents, func(i, j int) bool { return plan.Agents[i].Key < plan.Agents[j].Key })
	sort.Slice(plan.Teams, func(i, j int) bool { return plan.Teams[i].Key < plan.Teams[j].Key })
	sort.Slice(plan.Objectives, func(i, j int) bool { return plan.Objectives[i].ID < plan.Objectives[j].ID })
	sort.Slice(plan.Runbooks, func(i, j int) bool { return plan.Runbooks[i].ID < plan.Runbooks[j].ID })
}

func installationPlanDigest(plan *InstallationPlan) (string, error) {
	copy := *plan
	copy.PlanDigest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("encode workforce bundle installation plan: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func cloneTeamDefinition(value *team.Definition) (*team.Definition, error) {
	return cloneJSON(value)
}

func cloneJSON[T any](value *T) (*T, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var copy T
	if err := json.Unmarshal(encoded, &copy); err != nil {
		return nil, err
	}
	return &copy, nil
}
