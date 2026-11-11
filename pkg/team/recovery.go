package team

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
)

var ErrParticipationRecoveryNotRequired = errors.New("team participation recovery is not required")

// RecoverParticipationRequest is the explicit operator escape hatch for a
// Team whose roster has no member allowed to speak. It is not an Agent
// action: the host must authenticate and authorize the actor.
type RecoverParticipationRequest struct {
	Scope                      capability.ScopeReference `json:"scope"`
	DeploymentID               string                    `json:"deploymentId"`
	ExpectedDeploymentRevision int64                     `json:"expectedDeploymentRevision"`
	MemberID                   string                    `json:"memberId"`
	ActorType                  string                    `json:"actorType"`
	ActorID                    string                    `json:"actorId"`
	Reason                     string                    `json:"reason"`
	IdempotencyKey             string                    `json:"idempotencyKey"`
}

type ParticipationRecoveryResult struct {
	Deployment *Deployment                     `json:"deployment"`
	Activation *workforce.DefinitionActivation `json:"activation"`
	Replayed   bool                            `json:"replayed"`
}

// RecoverParticipation turns one roster member from listening or disabled to
// active, and only while the Team has no speaking member. The activation
// entry is the audit record; its id comes from the idempotency key, so a retry
// returns the first result.
func (r *Registry) RecoverParticipation(ctx context.Context, req RecoverParticipationRequest) (*ParticipationRecoveryResult, error) {
	if r == nil || r.store == nil || r.agents == nil {
		return nil, errors.New("team registry and Agent resolver are required")
	}
	req.DeploymentID, req.MemberID = strings.TrimSpace(req.DeploymentID), strings.TrimSpace(req.MemberID)
	req.ActorType, req.ActorID, req.Reason = strings.TrimSpace(req.ActorType), strings.TrimSpace(req.ActorID), strings.TrimSpace(req.Reason)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if strings.TrimSpace(req.Scope.Kind) == "" || strings.TrimSpace(req.Scope.ID) == "" || req.DeploymentID == "" ||
		req.ExpectedDeploymentRevision < 1 || req.MemberID == "" || req.ActorType == "" ||
		req.ActorID == "" || strings.EqualFold(req.ActorType, "agent") || req.Reason == "" || len(req.Reason) > 1000 ||
		req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 || strings.ContainsAny(req.IdempotencyKey, "\r\n") {
		return nil, errors.New("scoped Team recovery requires a revision, roster member, non-Agent actor, reason, and idempotency key")
	}
	deployment, err := r.store.GetTeamDeployment(ctx, req.Scope, req.DeploymentID)
	if err != nil {
		return nil, err
	}
	activationID := participationRecoveryActivationID(req)
	activations, err := r.store.ListTeamDefinitionActivations(ctx, req.Scope, req.DeploymentID)
	if err != nil {
		return nil, err
	}
	if index := slices.IndexFunc(activations, func(activation workforce.DefinitionActivation) bool { return activation.ID == activationID }); index >= 0 {
		first := activations[index]
		if first.DeploymentRevision != req.ExpectedDeploymentRevision+1 || first.ActorType != req.ActorType || first.ActorID != req.ActorID || first.Reason != req.Reason {
			return nil, ErrIdempotencyConflict
		}
		if deployment.Revision != first.DeploymentRevision {
			return nil, ErrRevisionConflict
		}
		return &ParticipationRecoveryResult{Deployment: deployment, Activation: &first, Replayed: true}, nil
	}
	if deployment.Revision != req.ExpectedDeploymentRevision {
		return nil, ErrRevisionConflict
	}
	if deployment.Status != DeploymentActive {
		return nil, errors.New("only an active Team can recover channel participation")
	}
	if HasSpeakingMember(deployment) {
		return nil, ErrParticipationRecoveryNotRequired
	}
	proposed := cloneDeployment(deployment)
	index := slices.IndexFunc(proposed.Roster, func(assignment RosterAssignment) bool { return assignment.ID == req.MemberID })
	if index < 0 {
		return nil, errors.New("participation recovery member must be on the Team roster")
	}
	proposed.Roster[index].ChannelParticipation = ChannelActive
	change, err := r.PrepareDeploymentUpdate(ctx, proposed, req.ExpectedDeploymentRevision, req.ActorType, req.ActorID, req.Reason)
	if err != nil {
		return nil, err
	}
	change.Activation.ID = activationID
	if err := r.store.UpdateTeamDeployment(ctx, change.Deployment, req.ExpectedDeploymentRevision, change.Activation); err != nil {
		return nil, err
	}
	return &ParticipationRecoveryResult{Deployment: cloneDeployment(change.Deployment), Activation: &change.Activation}, nil
}

func participationRecoveryActivationID(req RecoverParticipationRequest) string {
	seed := strings.Join([]string{
		"openseal", "team-participation-recovery", req.Scope.Kind, req.Scope.ID, req.DeploymentID, req.IdempotencyKey,
	}, "\x00")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

// HasSpeakingMember reports whether any roster member may answer in the Team
// channel.
func HasSpeakingMember(deployment *Deployment) bool {
	return slices.ContainsFunc(deployment.Roster, func(assignment RosterAssignment) bool {
		return assignment.ChannelParticipation == "" || assignment.ChannelParticipation == ChannelActive
	})
}
