package openseal

import (
	"context"
	"slices"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
)

// Cards are never reused across runs, so two runs in one conversation can
// each ask for the same thing. Settling one settles its pending siblings (the
// same conversation, kind, target and person) to the same outcome, and every
// sibling's own run resumes. Sibling settlement is best effort: a sibling
// that changed concurrently keeps its own outcome.

// sameSkillSetupTarget reports whether two setup-store requests ask the same
// person for the same thing.
func sameSkillSetupTarget(left, right *models.SkillSetupRequest) bool {
	if left.Kind != right.Kind || left.RequestedFor != right.RequestedFor || left.Scope != right.Scope ||
		left.DeploymentID != right.DeploymentID || left.ConversationID != right.ConversationID {
		return false
	}
	switch left.Kind {
	case models.SkillSetupKindConnectAccount:
		return left.BindingID == right.BindingID && slices.Equal(connectAccountSlotNames(left), connectAccountSlotNames(right))
	case models.SkillSetupKindConnectApp:
		return left.App != nil && right.App != nil && skillbinding.ConnectAppKey(*left.App) == skillbinding.ConnectAppKey(*right.App)
	}
	return false
}

func connectAccountSlotNames(r *models.SkillSetupRequest) []string {
	names := make([]string, 0, len(r.Slots))
	for _, slot := range r.Slots {
		names = append(names, slot.Name)
	}
	slices.Sort(names)
	return names
}

// settleSkillSetupSiblings settles the pending siblings of a settled
// connect_account or connect_app request and resumes their runs with
// resume.
func (e *Engine) settleSkillSetupSiblings(ctx context.Context, settled *models.SkillSetupRequest, resume func(context.Context, *models.SkillSetupRequest) (bool, error)) {
	if settled == nil || settled.Status == "pending" {
		return
	}
	requests, err := e.store.ListSkillSetupRequests(ctx, settled.Scope, settled.DeploymentID, settled.ConversationID)
	if err != nil {
		return
	}
	for _, sibling := range requests {
		if sibling == nil || sibling.ID == settled.ID || sibling.Status != "pending" || !sameSkillSetupTarget(settled, sibling) {
			continue
		}
		expected := sibling.Revision
		sibling.Status, sibling.ResolvedBy = settled.Status, settled.ResolvedBy
		sibling.ResolvedBindingID, sibling.ResolvedBindingRevision = settled.ResolvedBindingID, settled.ResolvedBindingRevision
		sibling.Revision++
		sibling.UpdatedAt = time.Now().UTC()
		if e.store.SaveSkillSetupRequest(ctx, sibling, expected) != nil {
			continue
		}
		_, _ = resume(ctx, sibling)
	}
}

// settleCredentialRequestSiblings settles the pending credential requests of
// the same conversation, kind and website as settled and resumes their runs.
func (e *Engine) settleCredentialRequestSiblings(ctx context.Context, settled *models.CredentialRequest) {
	if settled == nil || settled.Status == models.CredentialRequestStatusPending {
		return
	}
	requests, err := e.store.ListCredentialRequests(ctx, settled.Scope, settled.DeploymentID, settled.ConversationID)
	if err != nil {
		return
	}
	for _, sibling := range requests {
		if sibling == nil || sibling.ID == settled.ID || sibling.Status != models.CredentialRequestStatusPending ||
			sibling.Kind != settled.Kind || sibling.Website != settled.Website {
			continue
		}
		expected := sibling.Revision
		sibling.Status, sibling.ResolvedBy = settled.Status, settled.ResolvedBy
		sibling.Revision++
		sibling.UpdatedAt = time.Now().UTC()
		if e.store.SaveCredentialRequest(ctx, sibling, expected) != nil {
			continue
		}
		_, _ = e.continueCredentialRequestRun(ctx, sibling)
	}
}
