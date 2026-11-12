package models

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

var (
	ErrSkillReferenceUpgradeInvalid  = errors.New("skill reference upgrade is invalid")
	ErrSkillReferenceUpgradeConflict = errors.New("skill reference upgrade revision conflict")
	ErrSkillReferenceUpgradeBusy     = errors.New("skill reference upgrade is waiting for outstanding work to finish")
)

type SkillReferenceIdentity struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
}

type SkillReferenceObjectiveReference struct {
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	Action string `json:"action"`
}

type SkillReferenceObjectiveImpact struct {
	ID               string                             `json:"id"`
	ExpectedRevision int64                              `json:"expectedRevision"`
	References       []SkillReferenceObjectiveReference `json:"references"`
}

type SkillReferenceConversationEndpointImpact struct {
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expectedRevision"`
	AdapterID        string `json:"adapterId"`
}

type SkillReferenceCallbackRegistrationImpact struct {
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expectedRevision"`
	AdapterID        string `json:"adapterId"`
}

// SkillReferenceBindingRevision names another binding of the same Skill and
// version that moves in the same upgrade as the plan's primary binding.
type SkillReferenceBindingRevision struct {
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expectedRevision"`
}

// SkillReferenceUpgradeFinding is one contract change a human approves before
// the upgrade applies. Subject names the changed action or adapter; From and
// To carry the old and new value when the change has a single one (a risk).
type SkillReferenceUpgradeFinding struct {
	Code    string `json:"code"`
	Subject string `json:"subject,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Message string `json:"message"`
}

// SkillReferenceUpgradePlan is a portable, reviewable projection of every
// durable reference that must move together. Historical Runs and ActionCalls
// are deliberately absent and remain immutable.
type SkillReferenceUpgradePlan struct {
	APIVersion              string `json:"apiVersion"`
	Scope                   Scope  `json:"scope"`
	DeploymentID            string `json:"deploymentId"`
	BindingID               string `json:"bindingId"`
	ExpectedBindingRevision int64  `json:"expectedBindingRevision"`
	// Siblings are every other enabled binding of the deployment that grants
	// the same Skill identity. Durable references name a Skill version, not a
	// binding, so they can move only when every binding at that version moves
	// with them; a single-binding upgrade would leave them unattributable.
	Siblings              []SkillReferenceBindingRevision            `json:"siblings,omitempty"`
	From                  SkillReferenceIdentity                     `json:"from"`
	To                    SkillReferenceIdentity                     `json:"to"`
	Objectives            []SkillReferenceObjectiveImpact            `json:"objectives,omitempty"`
	ConversationEndpoints []SkillReferenceConversationEndpointImpact `json:"conversationEndpoints,omitempty"`
	CallbackRegistrations []SkillReferenceCallbackRegistrationImpact `json:"callbackRegistrations,omitempty"`
	// RemovedActions are allowed actions of the moved bindings that the target
	// version no longer defines. The upgrade drops them from every binding it
	// moves; dropping a grant needs no approval.
	RemovedActions []string `json:"removedActions,omitempty"`
	// AddedActions are actions the request granted that the target version
	// defines and the from version did not. The upgrade adds them to every
	// binding it moves; adding them needs no approval by itself. Actions the
	// from version already defined are never added, so an action the owner
	// removed from a binding stays removed.
	AddedActions     []string                       `json:"addedActions,omitempty"`
	Findings         []SkillReferenceUpgradeFinding `json:"findings,omitempty"`
	ApprovalRequired bool                           `json:"approvalRequired"`
	Digest           string                         `json:"digest"`
	GeneratedAt      time.Time                      `json:"generatedAt"`
}

type SkillReferenceUpgradeApproval struct {
	Principal ActivityActor `json:"principal"`
	Reason    string        `json:"reason"`
}

type SkillReferenceUpgradeReceipt struct {
	APIVersion            string                                     `json:"apiVersion"`
	PlanDigest            string                                     `json:"planDigest"`
	Scope                 Scope                                      `json:"scope"`
	DeploymentID          string                                     `json:"deploymentId"`
	BindingID             string                                     `json:"bindingId"`
	BindingRevision       int64                                      `json:"bindingRevision"`
	Siblings              []SkillReferenceBindingRevision            `json:"siblings,omitempty"`
	From                  SkillReferenceIdentity                     `json:"from"`
	To                    SkillReferenceIdentity                     `json:"to"`
	Objectives            []SkillReferenceObjectiveImpact            `json:"objectives,omitempty"`
	ConversationEndpoints []SkillReferenceConversationEndpointImpact `json:"conversationEndpoints,omitempty"`
	CallbackRegistrations []SkillReferenceCallbackRegistrationImpact `json:"callbackRegistrations,omitempty"`
	Actor                 ActivityActor                              `json:"actor"`
	Reason                string                                     `json:"reason"`
	Approval              *SkillReferenceUpgradeApproval             `json:"approval,omitempty"`
	ActivityIDs           []string                                   `json:"activityIds,omitempty"`
	AppliedAt             time.Time                                  `json:"appliedAt"`
}

type SkillReferenceObjectiveMutation struct {
	Value            *Objective
	ExpectedRevision int64
	Event            *ActivityEvent
}

type SkillReferenceConversationEndpointMutation struct {
	Value            *ExternalConversationEndpoint
	ExpectedRevision int64
}

type SkillReferenceCallbackRegistrationMutation struct {
	Value            *CallbackRegistration
	ExpectedRevision int64
}

// SkillReferenceUpgradeMutation is validated and materialized by the portable
// service. Store implementations must apply all records in one transaction or
// leave every record unchanged.
type SkillReferenceUpgradeMutation struct {
	Plan                  *SkillReferenceUpgradePlan
	Binding               *skill.Binding
	Siblings              []*skill.Binding
	Objectives            []SkillReferenceObjectiveMutation
	ConversationEndpoints []SkillReferenceConversationEndpointMutation
	CallbackRegistrations []SkillReferenceCallbackRegistrationMutation
	Receipt               *SkillReferenceUpgradeReceipt
}

// skillReferenceUpgradeMember is one binding an upgrade moves, with the
// revision it must have before the upgrade.
type skillReferenceUpgradeMember struct {
	Binding          *skill.Binding
	ExpectedRevision int64
}

// Members lists the primary binding and its siblings in plan order.
func (m *SkillReferenceUpgradeMutation) Members() []skillReferenceUpgradeMember {
	members := []skillReferenceUpgradeMember{{Binding: m.Binding, ExpectedRevision: m.Plan.ExpectedBindingRevision}}
	for index, sibling := range m.Siblings {
		members = append(members, skillReferenceUpgradeMember{Binding: sibling, ExpectedRevision: m.Plan.Siblings[index].ExpectedRevision})
	}
	return members
}

type SkillReferenceUpgradeStore interface {
	skill.CatalogStore
	PortfolioStore
	ExternalConversationEndpointStore
	CallbackRegistrationStore
	ApplySkillReferenceUpgrade(context.Context, *SkillReferenceUpgradeMutation) error
}

func ValidateSkillReferenceUpgradeMutation(mutation *SkillReferenceUpgradeMutation) error {
	if mutation == nil || mutation.Plan == nil || mutation.Binding == nil || mutation.Receipt == nil ||
		mutation.Plan.Digest == "" || mutation.Receipt.PlanDigest != mutation.Plan.Digest ||
		mutation.Binding.ID != mutation.Plan.BindingID ||
		mutation.Binding.DeploymentID != mutation.Plan.DeploymentID ||
		mutation.Binding.Revision != mutation.Plan.ExpectedBindingRevision+1 ||
		len(mutation.Objectives) != len(mutation.Plan.Objectives) ||
		len(mutation.ConversationEndpoints) != len(mutation.Plan.ConversationEndpoints) ||
		len(mutation.CallbackRegistrations) != len(mutation.Plan.CallbackRegistrations) ||
		len(mutation.Siblings) != len(mutation.Plan.Siblings) {
		return ErrSkillReferenceUpgradeInvalid
	}
	for index, sibling := range mutation.Siblings {
		expected := mutation.Plan.Siblings[index]
		if sibling == nil || sibling.ID != expected.ID || sibling.ID == mutation.Plan.BindingID ||
			sibling.DeploymentID != mutation.Plan.DeploymentID || sibling.Revision != expected.ExpectedRevision+1 ||
			sibling.SkillID != mutation.Plan.To.ID || sibling.SkillVersion != mutation.Plan.To.Version ||
			sibling.SourceIdentity != mutation.Plan.To.SourceIdentity {
			return ErrSkillReferenceUpgradeInvalid
		}
	}
	objectiveRevisions := make(map[string]int64, len(mutation.Plan.Objectives))
	for _, impact := range mutation.Plan.Objectives {
		objectiveRevisions[impact.ID] = impact.ExpectedRevision
	}
	for _, item := range mutation.Objectives {
		if item.Value == nil || item.Event == nil || item.ExpectedRevision != objectiveRevisions[item.Value.ID] ||
			item.Value.Revision != item.ExpectedRevision+1 || item.Value.Scope != mutation.Plan.Scope {
			return ErrSkillReferenceUpgradeInvalid
		}
		if err := item.Value.Validate(); err != nil {
			return fmt.Errorf("%w: objective %s: %v", ErrSkillReferenceUpgradeInvalid, item.Value.ID, err)
		}
		if err := item.Event.Validate(); err != nil {
			return fmt.Errorf("%w: objective activity: %v", ErrSkillReferenceUpgradeInvalid, err)
		}
	}
	endpointRevisions := make(map[string]int64, len(mutation.Plan.ConversationEndpoints))
	for _, impact := range mutation.Plan.ConversationEndpoints {
		endpointRevisions[impact.ID] = impact.ExpectedRevision
	}
	for _, item := range mutation.ConversationEndpoints {
		if item.Value == nil || item.ExpectedRevision != endpointRevisions[item.Value.ID] ||
			item.Value.Revision != item.ExpectedRevision+1 || item.Value.Scope != mutation.Plan.Scope {
			return ErrSkillReferenceUpgradeInvalid
		}
		if err := item.Value.Validate(); err != nil {
			return fmt.Errorf("%w: conversation endpoint %s: %v", ErrSkillReferenceUpgradeInvalid, item.Value.ID, err)
		}
	}
	callbackRevisions := make(map[string]int64, len(mutation.Plan.CallbackRegistrations))
	for _, impact := range mutation.Plan.CallbackRegistrations {
		callbackRevisions[impact.ID] = impact.ExpectedRevision
	}
	for _, item := range mutation.CallbackRegistrations {
		if item.Value == nil || item.ExpectedRevision != callbackRevisions[item.Value.ID] ||
			item.Value.Revision != item.ExpectedRevision+1 || item.Value.Scope != mutation.Plan.Scope {
			return ErrSkillReferenceUpgradeInvalid
		}
		if err := item.Value.Validate(); err != nil {
			return fmt.Errorf("%w: callback registration %s: %v", ErrSkillReferenceUpgradeInvalid, item.Value.ID, err)
		}
	}
	return nil
}
