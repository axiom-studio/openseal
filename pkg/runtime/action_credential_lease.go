package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	ActionCredentialLeaseVersion    = "openseal.action-credential-lease/v2"
	MaximumActionCredentialLeaseTTL = 5 * time.Minute
)

var (
	ErrActionCredentialLeaseInvalid  = errors.New("action credential lease is invalid")
	ErrActionCredentialLeaseExpired  = errors.New("action credential lease is expired")
	ErrActionCredentialLeaseMismatch = errors.New("action credential lease does not match the expected authority")
	ErrActionCredentialLeaseReplay   = errors.New("action credential lease was already consumed")
)

// ActionLeaseIdentity binds credential resolution to one immutable snapshot of
// a claimed ActionCall. Renewing, releasing, or reclaiming the durable action
// lease changes its revision, worker, expiry, or attempt and therefore changes
// this identity.
type ActionLeaseIdentity struct {
	ID                 string    `json:"id"`
	ActionCallRevision int64     `json:"actionCallRevision"`
	Attempt            int       `json:"attempt"`
	WorkerID           string    `json:"workerId"`
	ExpiresAt          time.Time `json:"expiresAt"`
}

// ActionCredentialFieldReference names the exact fields a host may resolve
// from one opaque credential reference. It contains identifiers only—never a
// resolved value. Fields are canonical, sorted, and unique.
type ActionCredentialFieldReference struct {
	Name      string                    `json:"name"`
	Reference skill.CredentialReference `json:"reference"`
	Fields    []string                  `json:"fields"`
}

// ActionCredentialLease is the complete product-neutral authority statement
// for one just-in-time credential resolution. It is intentionally map-free so
// its canonical JSON representation can be signed consistently by hosts.
type ActionCredentialLease struct {
	Version         string                           `json:"version"`
	TenantID        string                           `json:"tenantId"`
	Scope           Scope                            `json:"scope"`
	RunID           string                           `json:"runId"`
	ActionCallID    string                           `json:"actionCallId"`
	ActionLease     ActionLeaseIdentity              `json:"actionLease"`
	SkillID         string                           `json:"skillId"`
	SkillVersion    string                           `json:"skillVersion"`
	Action          string                           `json:"action"`
	Transport       string                           `json:"transport"`
	BindingOwnerID  string                           `json:"bindingOwnerId"`
	BindingID       string                           `json:"bindingId"`
	BindingRevision int64                            `json:"bindingRevision"`
	AssignedAgentID string                           `json:"assignedAgentId"`
	Credentials     []ActionCredentialFieldReference `json:"credentials"`
	Issuer          string                           `json:"issuer"`
	Audience        string                           `json:"audience"`
	IssuedAt        time.Time                        `json:"issuedAt"`
	ExpiresAt       time.Time                        `json:"expiresAt"`
	Nonce           string                           `json:"nonce"`
}

type ActionCredentialLeaseSignature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"keyId"`
	Value     []byte `json:"value"`
}

type SignedActionCredentialLease struct {
	Lease     ActionCredentialLease          `json:"lease"`
	Signature ActionCredentialLeaseSignature `json:"signature"`
}

type CreateActionCredentialLeaseRequest struct {
	TenantID         string
	Call             *ActionCall
	Run              *AgentRun
	CredentialFields map[string][]string
	Transport        string
	Issuer           string
	Audience         string
	IssuedAt         time.Time
	ExpiresAt        time.Time
	Nonce            string
}

// NewActionCredentialLease derives every authority field from the durable Run
// and currently claimed ActionCall. Credential workers supply Transport from
// the already resolved BoundAction; callers cannot substitute Skill, binding,
// owner, or Agent identity.
func NewActionCredentialLease(request CreateActionCredentialLeaseRequest) (*ActionCredentialLease, error) {
	call, run := request.Call, request.Run
	if call == nil || run == nil || call.Status != ActionCallStatusRunning || strings.TrimSpace(call.LeaseOwner) == "" || call.LeaseExpiresAt == nil {
		return nil, fmt.Errorf("%w: a running, claimed ActionCall and its Run are required", ErrActionCredentialLeaseInvalid)
	}
	if call.Scope != run.Scope || call.RunID != run.ID || strings.TrimSpace(run.AssignedAgentID) == "" {
		return nil, fmt.Errorf("%w: ActionCall and Run authority do not match", ErrActionCredentialLeaseMismatch)
	}
	issuedAt, expiresAt := canonicalLeaseTime(request.IssuedAt), canonicalLeaseTime(request.ExpiresAt)
	actionExpiry := canonicalLeaseTime(*call.LeaseExpiresAt)
	if expiresAt.After(actionExpiry) {
		return nil, fmt.Errorf("%w: credential expiry exceeds the durable action lease", ErrActionCredentialLeaseInvalid)
	}
	credentials, err := actionCredentialFields(call.CredentialRefs, request.CredentialFields)
	if err != nil {
		return nil, err
	}
	actionLease := ActionLeaseIdentity{
		ActionCallRevision: call.Revision, Attempt: call.Attempt, WorkerID: strings.TrimSpace(call.LeaseOwner), ExpiresAt: actionExpiry,
	}
	actionLease.ID = computeActionLeaseIdentity(call.Scope, call.ID, call.RunID, actionLease)
	lease := &ActionCredentialLease{
		Version: ActionCredentialLeaseVersion, TenantID: strings.TrimSpace(request.TenantID), Scope: call.Scope,
		RunID: call.RunID, ActionCallID: call.ID, ActionLease: actionLease,
		SkillID: call.SkillID, SkillVersion: call.SkillVersion, Action: call.Action, Transport: strings.TrimSpace(request.Transport),
		BindingOwnerID: call.DeploymentID, BindingID: call.BindingID, BindingRevision: call.BindingRevision,
		AssignedAgentID: run.AssignedAgentID, Credentials: credentials,
		Issuer: strings.TrimSpace(request.Issuer), Audience: strings.TrimSpace(request.Audience), IssuedAt: issuedAt, ExpiresAt: expiresAt, Nonce: strings.TrimSpace(request.Nonce),
	}
	if err := lease.Validate(); err != nil {
		return nil, err
	}
	return lease, nil
}

func (l *ActionCredentialLease) Validate() error {
	if l == nil || l.Version != ActionCredentialLeaseVersion || l.Scope.Validate() != nil {
		return fmt.Errorf("%w: version and scope are required", ErrActionCredentialLeaseInvalid)
	}
	identifiers := []string{l.TenantID, l.RunID, l.ActionCallID, l.ActionLease.ID, l.ActionLease.WorkerID, l.SkillID, l.SkillVersion, l.Action, l.Transport, l.BindingOwnerID, l.BindingID, l.AssignedAgentID, l.Issuer, l.Audience, l.Nonce}
	for _, value := range identifiers {
		if !validLeaseIdentifier(value, 512) {
			return fmt.Errorf("%w: an identity field is empty or malformed", ErrActionCredentialLeaseInvalid)
		}
	}
	if len(l.Nonce) < 16 {
		return fmt.Errorf("%w: nonce must contain at least 16 characters", ErrActionCredentialLeaseInvalid)
	}
	if l.Scope.Kind == "tenant" && l.Scope.ID != l.TenantID {
		return fmt.Errorf("%w: tenant does not match scope", ErrActionCredentialLeaseMismatch)
	}
	if l.BindingRevision < 1 || l.ActionLease.ActionCallRevision < 1 || l.ActionLease.Attempt < 1 || l.IssuedAt.IsZero() || l.ExpiresAt.IsZero() || l.ActionLease.ExpiresAt.IsZero() || !l.ExpiresAt.After(l.IssuedAt) || l.ExpiresAt.After(l.ActionLease.ExpiresAt) || l.ExpiresAt.Sub(l.IssuedAt) > MaximumActionCredentialLeaseTTL {
		return fmt.Errorf("%w: revisions, attempt, and expiry are invalid", ErrActionCredentialLeaseInvalid)
	}
	if len(l.Credentials) == 0 {
		return fmt.Errorf("%w: at least one credential field reference is required", ErrActionCredentialLeaseInvalid)
	}
	previous := ""
	for _, credential := range l.Credentials {
		if !validLeaseIdentifier(credential.Name, 128) || !validLeaseIdentifier(credential.Reference.Kind, 128) || !validOpaqueCredentialReference(credential.Reference.ID, 512) || credential.Name <= previous || len(credential.Fields) == 0 {
			return fmt.Errorf("%w: credential references must be sorted, unique, and opaque", ErrActionCredentialLeaseInvalid)
		}
		previous = credential.Name
		previousField := ""
		for _, field := range credential.Fields {
			if !validCredentialField(field) || field <= previousField {
				return fmt.Errorf("%w: credential fields must be sorted and unique", ErrActionCredentialLeaseInvalid)
			}
			previousField = field
		}
	}
	expectedID := computeActionLeaseIdentity(l.Scope, l.ActionCallID, l.RunID, ActionLeaseIdentity{
		ActionCallRevision: l.ActionLease.ActionCallRevision, Attempt: l.ActionLease.Attempt, WorkerID: l.ActionLease.WorkerID, ExpiresAt: l.ActionLease.ExpiresAt,
	})
	if l.ActionLease.ID != expectedID {
		return fmt.Errorf("%w: action lease identity does not match its immutable fields", ErrActionCredentialLeaseMismatch)
	}
	return nil
}

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
}

// ActionCredentialLeaseRedemptionRequest contains the host-authorized inputs
// required to atomically revalidate and consume an already signature-verified
// credential lease. CredentialFields contains field names only; resolved
// credential values must never cross this persistence boundary.
type ActionCredentialLeaseRedemptionRequest struct {
	Lease ActionCredentialLease
	// Transport and CredentialFields are derived by the trusted host from the
	// exact current action definition. They are rechecked against durable
	// ActionCall and SkillBinding authority inside the store transaction.
	Transport        string
	CredentialFields map[string][]string
}

// ActionCredentialLeaseRedeemer is the durable, product-neutral
// single-use authority boundary for credential resolution. Implementations
// must revalidate current action, Run, and Skill binding authority and consume
// the nonce in one transaction before any credential value is resolved.
type ActionCredentialLeaseRedeemer interface {
	RedeemActionCredentialLease(context.Context, ActionCredentialLeaseRedemptionRequest) error
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
	}); err != nil {
		return nil, fmt.Errorf("redeem action credential lease: %w", err)
	}
	result := lease
	result.Credentials = cloneActionCredentialFieldReferences(lease.Credentials)
	return &result, nil
}

// MatchActionCredentialLease rechecks a signed lease against current durable
// state and the host's exact field allowlist. It is suitable for an
// ActionCredentialLeaseRedeemer implementation inside its atomic boundary.
func MatchActionCredentialLease(lease ActionCredentialLease, call *ActionCall, run *AgentRun, transport string, credentialFields map[string][]string) error {
	if err := lease.Validate(); err != nil {
		return err
	}
	if err := matchActionCredentialLeaseCore(lease, call, run, transport); err != nil {
		return err
	}
	expectedFields, err := actionCredentialFields(call.CredentialRefs, credentialFields)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(lease.Credentials, expectedFields) {
		return ErrActionCredentialLeaseMismatch
	}
	return nil
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

func matchActionCredentialLeaseCore(lease ActionCredentialLease, call *ActionCall, run *AgentRun, transport string) error {
	if call == nil || run == nil || call.Status != ActionCallStatusRunning || call.LeaseExpiresAt == nil || call.Scope != run.Scope || call.RunID != run.ID {
		return ErrActionCredentialLeaseMismatch
	}
	// A heartbeat may monotonically advance the durable revision and expiry
	// while the same worker still owns the same attempt. The signed identity
	// remains the immutable issuance snapshot; reclaim changes attempt/owner or
	// status and therefore still fails closed.
	if lease.Scope != call.Scope || lease.RunID != call.RunID || lease.ActionCallID != call.ID ||
		call.Revision < lease.ActionLease.ActionCallRevision || lease.ActionLease.Attempt != call.Attempt || lease.ActionLease.WorkerID != strings.TrimSpace(call.LeaseOwner) || canonicalLeaseTime(*call.LeaseExpiresAt).Before(lease.ActionLease.ExpiresAt) ||
		lease.SkillID != call.SkillID || lease.SkillVersion != call.SkillVersion || lease.Action != call.Action || lease.Transport != strings.TrimSpace(transport) || lease.BindingOwnerID != call.DeploymentID ||
		lease.BindingID != call.BindingID || lease.BindingRevision != call.BindingRevision || lease.AssignedAgentID != run.AssignedAgentID {
		return ErrActionCredentialLeaseMismatch
	}
	return nil
}

func actionCredentialFields(references map[string]skill.CredentialReference, fields map[string][]string) ([]ActionCredentialFieldReference, error) {
	if len(references) == 0 || len(fields) != len(references) {
		return nil, fmt.Errorf("%w: exact fields are required for every credential reference", ErrActionCredentialLeaseMismatch)
	}
	names := make([]string, 0, len(references))
	for name := range references {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]ActionCredentialFieldReference, 0, len(names))
	for _, name := range names {
		selected, ok := fields[name]
		if !ok || len(selected) == 0 {
			return nil, fmt.Errorf("%w: credential %s has no exact field selection", ErrActionCredentialLeaseMismatch, name)
		}
		selected = append([]string(nil), selected...)
		for index := range selected {
			selected[index] = strings.TrimSpace(selected[index])
		}
		sort.Strings(selected)
		result = append(result, ActionCredentialFieldReference{Name: strings.TrimSpace(name), Reference: references[name], Fields: selected})
	}
	for name := range fields {
		if _, ok := references[name]; !ok {
			return nil, fmt.Errorf("%w: unbound credential %s was requested", ErrActionCredentialLeaseMismatch, name)
		}
	}
	return result, nil
}

func canonicalActionCredentialLeasePayload(lease ActionCredentialLease) ([]byte, error) {
	if err := lease.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(lease)
}

func computeActionLeaseIdentity(scope Scope, actionCallID, runID string, lease ActionLeaseIdentity) string {
	canonical := struct {
		Scope              Scope     `json:"scope"`
		ActionCallID       string    `json:"actionCallId"`
		RunID              string    `json:"runId"`
		ActionCallRevision int64     `json:"actionCallRevision"`
		Attempt            int       `json:"attempt"`
		WorkerID           string    `json:"workerId"`
		ExpiresAt          time.Time `json:"expiresAt"`
	}{scope, actionCallID, runID, lease.ActionCallRevision, lease.Attempt, lease.WorkerID, canonicalLeaseTime(lease.ExpiresAt)}
	encoded, _ := json.Marshal(canonical)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
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

func cloneActionCredentialFieldReferences(values []ActionCredentialFieldReference) []ActionCredentialFieldReference {
	result := make([]ActionCredentialFieldReference, len(values))
	for index, value := range values {
		result[index] = value
		result[index].Fields = append([]string(nil), value.Fields...)
	}
	return result
}

func cloneActionCredentialFields(values map[string][]string) map[string][]string {
	if values == nil {
		return nil
	}
	result := make(map[string][]string, len(values))
	for name, fields := range values {
		result[name] = append([]string(nil), fields...)
	}
	return result
}

func canonicalLeaseTime(value time.Time) time.Time { return value.UTC().Round(0) }

func validLeaseIdentifier(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

// validOpaqueCredentialReference validates a host-owned reference without
// interpreting its provider-specific identifier. Credential-store and connection names
// may legitimately contain spaces; signed JSON already preserves those bytes
// without ambiguity. Leading/trailing whitespace and control characters remain
// invalid so references retain a single canonical representation.
func validOpaqueCredentialReference(value string, maximum int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validCredentialField(value string) bool {
	if !validLeaseIdentifier(value, 256) {
		return false
	}
	for _, character := range value {
		if !(unicode.IsLetter(character) || unicode.IsDigit(character) || strings.ContainsRune("._/-[]", character)) {
			return false
		}
	}
	return true
}
