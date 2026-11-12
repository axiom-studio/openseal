package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type ActionCredentialLeaseSigner interface {
	SignActionCredentialLease(context.Context, []byte) (ActionCredentialLeaseSignature, error)
}

type ActionCredentialLeaseSignatureVerifier interface {
	VerifyActionCredentialLease(context.Context, []byte, ActionCredentialLeaseSignature) error
}

func SignActionCredentialLease(ctx context.Context, lease ActionCredentialLease, signer ActionCredentialLeaseSigner) (*SignedActionCredentialLease, error) {
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
	if !validLeaseIdentifier(signature.Algorithm, 128) || !validLeaseIdentifier(signature.KeyID, 256) || len(signature.Value) < 16 {
		return nil, fmt.Errorf("%w: signature metadata is incomplete", ErrActionCredentialLeaseInvalid)
	}
	return &SignedActionCredentialLease{Lease: lease, Signature: cloneActionCredentialLeaseSignature(signature)}, nil
}

type ActionCredentialLeaseValidationRequest struct {
	Envelope      *SignedActionCredentialLease
	TenantID      string
	Scope         Scope
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

type ActionCredentialLeaseValidator struct {
	verifier ActionCredentialLeaseSignatureVerifier
	redeemer ActionCredentialLeaseRedeemer
}

func NewActionCredentialLeaseValidator(verifier ActionCredentialLeaseSignatureVerifier, redeemer ActionCredentialLeaseRedeemer) (*ActionCredentialLeaseValidator, error) {
	if verifier == nil || redeemer == nil {
		return nil, errors.New("action credential lease signature verifier and atomic redeemer are required")
	}
	return &ActionCredentialLeaseValidator{verifier: verifier, redeemer: redeemer}, nil
}

// Validate authenticates the signed envelope, checks the receiving tenant,
// scope, issuer, audience, and expiry, then atomically revalidates durable
// authority and consumes the nonce through the redeemer. Redemption is last so
// malformed or unauthenticated traffic cannot burn a valid lease.
func (v *ActionCredentialLeaseValidator) Validate(ctx context.Context, request ActionCredentialLeaseValidationRequest) (*ActionCredentialLease, error) {
	if v == nil || v.verifier == nil || v.redeemer == nil || request.Envelope == nil {
		return nil, errors.New("action credential lease validator is not configured")
	}
	lease := request.Envelope.Lease
	lease.Credentials = cloneActionCredentialFieldReferences(lease.Credentials)
	payload, err := canonicalActionCredentialLeasePayload(lease)
	if err != nil {
		return nil, err
	}
	if lease.TenantID != strings.TrimSpace(request.TenantID) || lease.Scope != request.Scope || lease.Issuer != strings.TrimSpace(request.TrustedIssuer) || lease.Audience != strings.TrimSpace(request.Audience) {
		return nil, ErrActionCredentialLeaseMismatch
	}
	now := canonicalLeaseTime(request.Now)
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if now.Before(lease.IssuedAt) || !now.Before(lease.ExpiresAt) || !now.Before(lease.ActionLease.ExpiresAt) {
		return nil, ErrActionCredentialLeaseExpired
	}
	if err := validateActionCredentialLeaseSignature(request.Envelope.Signature); err != nil {
		return nil, err
	}
	if err := v.verifier.VerifyActionCredentialLease(ctx, payload, cloneActionCredentialLeaseSignature(request.Envelope.Signature)); err != nil {
		return nil, fmt.Errorf("verify action credential lease signature: %w", err)
	}
	if err := v.redeemer.RedeemActionCredentialLease(ctx, ActionCredentialLeaseRedemptionRequest{
		Lease: lease, Transport: request.Transport, CredentialFields: cloneActionCredentialFields(request.CredentialFields),
		HostBinding: request.HostBinding,
	}); err != nil {
		return nil, fmt.Errorf("redeem action credential lease: %w", err)
	}
	result := lease
	result.Credentials = cloneActionCredentialFieldReferences(lease.Credentials)
	return &result, nil
}

// MatchActionCredentialLeaseReferences lets the kernel verify that an issuer
// did not substitute or add an opaque reference before dispatch. Exact fields
// remain host-authorized by MatchActionCredentialLease at resolution time.
func MatchActionCredentialLeaseReferences(envelope *SignedActionCredentialLease, call *ActionCall, run *AgentRun, transport string) error {
	if envelope == nil {
		return ErrActionCredentialLeaseMismatch
	}
	lease := envelope.Lease
	if err := lease.Validate(); err != nil {
		return err
	}
	if err := validateActionCredentialLeaseSignature(envelope.Signature); err != nil {
		return err
	}
	if err := matchActionCredentialLeaseCore(lease, call, run, transport); err != nil {
		return err
	}
	if len(lease.Credentials) != len(call.CredentialRefs) {
		return ErrActionCredentialLeaseMismatch
	}
	for _, credential := range lease.Credentials {
		reference, ok := call.CredentialRefs[credential.Name]
		if !ok || reference != credential.Reference {
			return ErrActionCredentialLeaseMismatch
		}
	}
	return nil
}

func canonicalActionCredentialLeasePayload(lease ActionCredentialLease) ([]byte, error) {
	if err := lease.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(lease)
}

func validateActionCredentialLeaseSignature(signature ActionCredentialLeaseSignature) error {
	if !validLeaseIdentifier(signature.Algorithm, 128) || !validLeaseIdentifier(signature.KeyID, 256) || len(signature.Value) < 16 {
		return fmt.Errorf("%w: signature metadata is incomplete", ErrActionCredentialLeaseInvalid)
	}
	return nil
}

func cloneActionCredentialLeaseSignature(value ActionCredentialLeaseSignature) ActionCredentialLeaseSignature {
	value.Value = append([]byte(nil), value.Value...)
	return value
}

func cloneSignedActionCredentialLease(value *SignedActionCredentialLease) *SignedActionCredentialLease {
	if value == nil {
		return nil
	}
	result := *value
	result.Lease.Credentials = cloneActionCredentialFieldReferences(value.Lease.Credentials)
	result.Signature = cloneActionCredentialLeaseSignature(value.Signature)
	return &result
}
