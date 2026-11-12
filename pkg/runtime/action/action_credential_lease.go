package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type CredentialLeaseSigner interface {
	SignActionCredentialLease(context.Context, []byte) (models.ActionCredentialLeaseSignature, error)
}

type CredentialLeaseSignatureVerifier interface {
	VerifyActionCredentialLease(context.Context, []byte, models.ActionCredentialLeaseSignature) error
}

func SignActionCredentialLease(ctx context.Context, lease models.ActionCredentialLease, signer CredentialLeaseSigner) (*models.SignedActionCredentialLease, error) {
	if signer == nil {
		return nil, errors.New("action credential lease signer is required")
	}
	payload, err := canonicalActionCredentialLeasePayload(lease)
	if err != nil {
		return nil, err
	}
	signature, err := signer.SignActionCredentialLease(ctx, payload)
	if err != nil {
		return nil, err
	}
	if !models.IsValidLeaseIdentifier(signature.Algorithm, 128) || !models.IsValidLeaseIdentifier(signature.KeyID, 256) || len(signature.Value) < 16 {
		return nil, fmt.Errorf("%w: signature metadata is incomplete", models.ErrActionCredentialLeaseInvalid)
	}
	return &models.SignedActionCredentialLease{Lease: lease, Signature: cloneActionCredentialLeaseSignature(signature)}, nil
}

type CredentialLeaseValidationRequest struct {
	Envelope      *models.SignedActionCredentialLease
	TenantID      string
	Scope         models.Scope
	TrustedIssuer string
	Audience      string
	Now           time.Time
	Transport     string
	// CredentialFields is the exact host-derived field allowlist for the
	// current action definition. It contains field names only.
	CredentialFields map[string][]string
	// HostBinding is the current host-supplied binding of the lease, which
	// the host re-resolved for the lease's run (skill.Catalog.GetHostBinding).
	// It is set only for a binding with no stored row.
	HostBinding *skill.Binding
}

type CredentialLeaseValidator struct {
	verifier CredentialLeaseSignatureVerifier
	redeemer models.ActionCredentialLeaseRedeemer
}

func NewCredentialLeaseValidator(verifier CredentialLeaseSignatureVerifier, redeemer models.ActionCredentialLeaseRedeemer) (*CredentialLeaseValidator, error) {
	if verifier == nil || redeemer == nil {
		return nil, errors.New("action credential lease signature verifier and atomic redeemer are required")
	}
	return &CredentialLeaseValidator{verifier: verifier, redeemer: redeemer}, nil
}

// Validate authenticates the signed envelope, checks the receiving tenant,
// scope, issuer, audience, and expiry, then atomically revalidates durable
// authority and consumes the nonce through the redeemer. Redemption is last so
// malformed or unauthenticated traffic cannot burn a valid lease.
func (v *CredentialLeaseValidator) Validate(ctx context.Context, request CredentialLeaseValidationRequest) (*models.ActionCredentialLease, error) {
	if v == nil || v.verifier == nil || v.redeemer == nil || request.Envelope == nil {
		return nil, errors.New("action credential lease validator is not configured")
	}
	lease := request.Envelope.Lease
	lease.Credentials = models.CloneActionCredentialFieldReferences(lease.Credentials)
	payload, err := canonicalActionCredentialLeasePayload(lease)
	if err != nil {
		return nil, err
	}
	if lease.TenantID != strings.TrimSpace(request.TenantID) || lease.Scope != request.Scope || lease.Issuer != strings.TrimSpace(request.TrustedIssuer) || lease.Audience != strings.TrimSpace(request.Audience) {
		return nil, models.ErrActionCredentialLeaseMismatch
	}
	now := models.CanonicalLeaseTime(request.Now)
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if now.Before(lease.IssuedAt) || !now.Before(lease.ExpiresAt) || !now.Before(lease.ActionLease.ExpiresAt) {
		return nil, models.ErrActionCredentialLeaseExpired
	}
	if err := validateActionCredentialLeaseSignature(request.Envelope.Signature); err != nil {
		return nil, err
	}
	if err := v.verifier.VerifyActionCredentialLease(ctx, payload, cloneActionCredentialLeaseSignature(request.Envelope.Signature)); err != nil {
		return nil, fmt.Errorf("verify action credential lease signature: %w", err)
	}
	if err := v.redeemer.RedeemActionCredentialLease(ctx, models.ActionCredentialLeaseRedemptionRequest{
		Lease: lease, Transport: request.Transport, CredentialFields: models.CloneActionCredentialFields(request.CredentialFields),
		HostBinding: request.HostBinding,
	}); err != nil {
		return nil, fmt.Errorf("redeem action credential lease: %w", err)
	}
	result := lease
	result.Credentials = models.CloneActionCredentialFieldReferences(lease.Credentials)
	return &result, nil
}

// MatchActionCredentialLeaseReferences lets the kernel verify that an issuer
// did not substitute or add an opaque reference before dispatch. Exact fields
// remain host-authorized by MatchActionCredentialLease at resolution time.
func MatchActionCredentialLeaseReferences(envelope *models.SignedActionCredentialLease, call *models.ActionCall, run *models.AgentRun, transport string) error {
	if envelope == nil {
		return models.ErrActionCredentialLeaseMismatch
	}
	lease := envelope.Lease
	if err := lease.Validate(); err != nil {
		return err
	}
	if err := validateActionCredentialLeaseSignature(envelope.Signature); err != nil {
		return err
	}
	if err := models.CheckActionCredentialLeaseCore(lease, call, run, transport); err != nil {
		return err
	}
	if len(lease.Credentials) != len(call.CredentialRefs) {
		return models.ErrActionCredentialLeaseMismatch
	}
	for _, credential := range lease.Credentials {
		reference, ok := call.CredentialRefs[credential.Name]
		if !ok || reference != credential.Reference {
			return models.ErrActionCredentialLeaseMismatch
		}
	}
	return nil
}

func canonicalActionCredentialLeasePayload(lease models.ActionCredentialLease) ([]byte, error) {
	if err := lease.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(lease)
}

func validateActionCredentialLeaseSignature(signature models.ActionCredentialLeaseSignature) error {
	if !models.IsValidLeaseIdentifier(signature.Algorithm, 128) || !models.IsValidLeaseIdentifier(signature.KeyID, 256) || len(signature.Value) < 16 {
		return fmt.Errorf("%w: signature metadata is incomplete", models.ErrActionCredentialLeaseInvalid)
	}
	return nil
}

func cloneActionCredentialLeaseSignature(value models.ActionCredentialLeaseSignature) models.ActionCredentialLeaseSignature {
	value.Value = append([]byte(nil), value.Value...)
	return value
}

// CloneSignedActionCredentialLease copies a signed lease, including its
// credential references and signature.
func CloneSignedActionCredentialLease(value *models.SignedActionCredentialLease) *models.SignedActionCredentialLease {
	if value == nil {
		return nil
	}
	result := *value
	result.Lease.Credentials = models.CloneActionCredentialFieldReferences(value.Lease.Credentials)
	result.Signature = cloneActionCredentialLeaseSignature(value.Signature)
	return &result
}
