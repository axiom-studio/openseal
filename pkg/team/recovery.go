package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
)

var ErrParticipationRecoveryNotRequired = errors.New("team participation recovery is not required")

// RecoverParticipationRequest is the explicit operator escape hatch for a
// legacy Team whose deployed roster has no role allowed to speak. It is not an
// Agent self-amendment: the host must authenticate and authorize the actor.
type RecoverParticipationRequest struct {
	Scope                      capability.ScopeReference `json:"scope"`
	DeploymentID               string                    `json:"deploymentId"`
	BaseVersion                string                    `json:"baseVersion"`
	ExpectedDeploymentRevision int64                     `json:"expectedDeploymentRevision"`
	RoleID                     string                    `json:"roleId"`
	ActorType                  string                    `json:"actorType"`
	ActorID                    string                    `json:"actorId"`
	Reason                     string                    `json:"reason"`
	IdempotencyKey             string                    `json:"idempotencyKey"`
}

type ParticipationRecoveryResult struct {
	Amendment  *DefinitionAmendment            `json:"amendment"`
	Deployment *Deployment                     `json:"deployment"`
	Activation *workforce.DefinitionActivation `json:"activation"`
	Replayed   bool                            `json:"replayed"`
}

// RecoverParticipation records an exact, approved recovery amendment and
// atomically activates its immutable candidate. The narrow operation can only
// turn one assigned role from listening/disabled to active, and only while the
// deployed Team has no speaking roster member.
func (r *Registry) RecoverParticipation(ctx context.Context, req RecoverParticipationRequest) (*ParticipationRecoveryResult, error) {
	if r == nil || r.store == nil || r.agents == nil {
		return nil, errors.New("team registry and Agent resolver are required")
	}
	req.DeploymentID, req.BaseVersion, req.RoleID = strings.TrimSpace(req.DeploymentID), strings.TrimSpace(req.BaseVersion), strings.TrimSpace(req.RoleID)
	req.ActorType, req.ActorID, req.Reason = strings.TrimSpace(req.ActorType), strings.TrimSpace(req.ActorID), strings.TrimSpace(req.Reason)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if strings.TrimSpace(req.Scope.Kind) == "" || strings.TrimSpace(req.Scope.ID) == "" || req.DeploymentID == "" ||
		req.BaseVersion == "" || req.ExpectedDeploymentRevision < 1 || req.RoleID == "" || req.ActorType == "" ||
		req.ActorID == "" || strings.EqualFold(req.ActorType, "agent") || req.Reason == "" || len(req.Reason) > 1000 ||
		req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 || strings.ContainsAny(req.IdempotencyKey, "\r\n") {
		return nil, errors.New("scoped Team recovery requires a base version, revision, assigned role, non-Agent actor, reason, and idempotency key")
	}

	deployment, err := r.store.GetTeamDeployment(ctx, req.Scope, req.DeploymentID)
	if err != nil {
		return nil, err
	}
	base, err := r.store.GetTeamDefinition(ctx, deployment.DefinitionID, req.BaseVersion)
	if err != nil {
		return nil, err
	}
	candidate, err := participationRecoveryCandidate(base, req, r.now().UTC())
	if err != nil {
		return nil, err
	}
	requestDigest := AmendmentRequestDigest(req.DeploymentID, base.Digest, candidate.Digest, req.ActorType, req.ActorID, req.Reason, nil)
	amendmentID := participationRecoveryAmendmentID(req)
	if existing, lookupErr := r.store.GetTeamAmendment(ctx, req.Scope, amendmentID); lookupErr == nil {
		if existing.IdempotencyKey != req.IdempotencyKey || existing.RequestDigest != requestDigest {
			return nil, ErrIdempotencyConflict
		}
		return r.resumeParticipationRecovery(ctx, req, existing)
	} else if !errors.Is(lookupErr, ErrAmendmentNotFound) {
		return nil, lookupErr
	}

	if deployment.Revision != req.ExpectedDeploymentRevision || deployment.ActiveVersion != req.BaseVersion {
		return nil, ErrRevisionConflict
	}
	if deployment.Status != DeploymentActive {
		return nil, errors.New("only an active Team can recover channel participation")
	}
	if teamHasSpeakingRosterRole(deployment, base) {
		return nil, ErrParticipationRecoveryNotRequired
	}
	if !teamRosterUsesRole(deployment, req.RoleID) {
		return nil, errors.New("participation recovery role must be assigned in the deployed roster")
	}

	now := r.now().UTC()
	amendment := &DefinitionAmendment{
		ID: amendmentID, Scope: req.Scope, DeploymentID: deployment.ID, DefinitionID: base.ID,
		BaseVersion: base.Version, BaseDigest: base.Digest, Candidate: *candidate,
		Changes: teamDefinitionChanges(base, candidate), RiskWidening: true,
		ProposerType: req.ActorType, ProposerID: req.ActorID, Rationale: req.Reason,
		IdempotencyKey: req.IdempotencyKey, RequestDigest: requestDigest,
		Status: AmendmentApproved, Decision: &AmendmentDecision{
			Approved: true, ActorType: req.ActorType, ActorID: req.ActorID, Reason: req.Reason, DecidedAt: now,
		},
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := amendment.Validate(); err != nil {
		return nil, err
	}
	if err := r.store.CreateTeamAmendment(ctx, amendment); err != nil {
		existing, lookupErr := r.store.GetTeamAmendment(ctx, req.Scope, amendmentID)
		if lookupErr != nil || existing.IdempotencyKey != req.IdempotencyKey || existing.RequestDigest != requestDigest {
			return nil, err
		}
		return r.resumeParticipationRecovery(ctx, req, existing)
	}
	return r.resumeParticipationRecovery(ctx, req, amendment)
}

func (r *Registry) resumeParticipationRecovery(ctx context.Context, req RecoverParticipationRequest, amendment *DefinitionAmendment) (*ParticipationRecoveryResult, error) {
	if amendment.Status == AmendmentActivated {
		deployment, err := r.store.GetTeamDeployment(ctx, req.Scope, req.DeploymentID)
		if err != nil {
			return nil, err
		}
		if deployment.ActiveVersion != amendment.Candidate.Version || deployment.Revision != req.ExpectedDeploymentRevision+1 {
			return nil, ErrRevisionConflict
		}
		activations, err := r.store.ListTeamDefinitionActivations(ctx, req.Scope, req.DeploymentID)
		if err != nil {
			return nil, err
		}
		for index := range activations {
			if activations[index].ID == amendment.ActivationID {
				activation := activations[index]
				return &ParticipationRecoveryResult{Amendment: amendment, Deployment: deployment, Activation: &activation, Replayed: true}, nil
			}
		}
		return nil, errors.New("team participation recovery activation audit is unavailable")
	}
	if amendment.Status != AmendmentApproved || amendment.Revision != 1 {
		return nil, errors.New("team participation recovery is not activatable")
	}
	activated, deployment, activation, err := r.ActivateAmendment(
		ctx, req.Scope, amendment.ID, amendment.Revision, req.ActorType, req.ActorID, req.Reason,
	)
	if err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			latest, lookupErr := r.store.GetTeamAmendment(ctx, req.Scope, amendment.ID)
			if lookupErr == nil && latest.Status == AmendmentActivated {
				return r.resumeParticipationRecovery(ctx, req, latest)
			}
		}
		return nil, err
	}
	return &ParticipationRecoveryResult{Amendment: activated, Deployment: deployment, Activation: activation}, nil
}

func participationRecoveryCandidate(base *Definition, req RecoverParticipationRequest, now time.Time) (*Definition, error) {
	if base == nil || base.Version != req.BaseVersion {
		return nil, ErrDefinitionNotFound
	}
	candidate := cloneDefinition(base)
	digest := participationRecoveryDigest(req)
	suffix := ".recovery." + digest[:12]
	prefix := base.Version
	if len(prefix)+len(suffix) > 128 {
		prefix = prefix[:128-len(suffix)]
	}
	candidate.Version = prefix + suffix
	candidate.Provenance.Source = "openseal-team-participation-recovery"
	candidate.Provenance.DerivedFrom = base.Digest
	candidate.Provenance.CreatedBy = req.ActorType + ":" + req.ActorID
	found := false
	for index := range candidate.Roles {
		if candidate.Roles[index].ID == req.RoleID {
			candidate.Roles[index].ChannelParticipation = RoleChannelActive
			found = true
		}
	}
	if !found {
		return nil, errors.New("participation recovery role is not declared by the Team definition")
	}
	return PrepareDefinition(candidate, now)
}

func participationRecoveryDigest(req RecoverParticipationRequest) string {
	payload, _ := json.Marshal(struct {
		Scope                      capability.ScopeReference
		DeploymentID               string
		BaseVersion                string
		ExpectedDeploymentRevision int64
		RoleID                     string
		IdempotencyKey             string
	}{req.Scope, req.DeploymentID, req.BaseVersion, req.ExpectedDeploymentRevision, req.RoleID, req.IdempotencyKey})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func participationRecoveryAmendmentID(req RecoverParticipationRequest) string {
	seed := strings.Join([]string{"openseal", "team-participation-recovery", req.Scope.Kind, req.Scope.ID, req.DeploymentID, req.IdempotencyKey}, "\x00")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

func teamHasSpeakingRosterRole(deployment *Deployment, definition *Definition) bool {
	roles := make(map[string]RoleChannelParticipation, len(definition.Roles))
	for _, role := range definition.Roles {
		roles[role.ID] = role.ChannelParticipation
	}
	for _, assignment := range deployment.Roster {
		if participation := roles[assignment.RoleID]; participation == "" || participation == RoleChannelActive {
			return true
		}
	}
	return false
}

func teamRosterUsesRole(deployment *Deployment, roleID string) bool {
	for _, assignment := range deployment.Roster {
		if assignment.RoleID == roleID {
			return true
		}
	}
	return false
}
