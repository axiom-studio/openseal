package models

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
	Version         string              `json:"version"`
	TenantID        string              `json:"tenantId"`
	Scope           Scope               `json:"scope"`
	RunID           string              `json:"runId"`
	ActionCallID    string              `json:"actionCallId"`
	ActionLease     ActionLeaseIdentity `json:"actionLease"`
	SkillID         string              `json:"skillId"`
	SkillVersion    string              `json:"skillVersion"`
	Action          string              `json:"action"`
	Transport       string              `json:"transport"`
	BindingOwnerID  string              `json:"bindingOwnerId"`
	BindingID       string              `json:"bindingId"`
	BindingRevision int64               `json:"bindingRevision"`
	AssignedAgentID string              `json:"assignedAgentId"`
	// ActingUser is the run's ActingUser at issue; redemption requires it
	// to still match the run.
	ActingUser  string                           `json:"actingUser,omitempty"`
	Credentials []ActionCredentialFieldReference `json:"credentials"`
	Issuer      string                           `json:"issuer"`
	Audience    string                           `json:"audience"`
	IssuedAt    time.Time                        `json:"issuedAt"`
	ExpiresAt   time.Time                        `json:"expiresAt"`
	Nonce       string                           `json:"nonce"`
}

func (l *ActionCredentialLease) Validate() error {
	if l == nil || l.Version != ActionCredentialLeaseVersion || l.Scope.Validate() != nil {
		return fmt.Errorf("%w: version and scope are required", ErrActionCredentialLeaseInvalid)
	}
	identifiers := []string{l.TenantID, l.RunID, l.ActionCallID, l.ActionLease.ID, l.ActionLease.WorkerID, l.SkillID, l.SkillVersion, l.Action, l.Transport, l.BindingOwnerID, l.BindingID, l.AssignedAgentID, l.Issuer, l.Audience, l.Nonce}
	for _, value := range identifiers {
		if !IsValidLeaseIdentifier(value, 512) {
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
		if !IsValidLeaseIdentifier(credential.Name, 128) || !IsValidLeaseIdentifier(credential.Reference.Kind, 128) || !validOpaqueCredentialReference(credential.Reference.ID, 512) || credential.Name <= previous || len(credential.Fields) == 0 {
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
	expectedID := ComputeActionLeaseIdentity(l.Scope, l.ActionCallID, l.RunID, ActionLeaseIdentity{
		ActionCallRevision: l.ActionLease.ActionCallRevision, Attempt: l.ActionLease.Attempt, WorkerID: l.ActionLease.WorkerID, ExpiresAt: l.ActionLease.ExpiresAt,
	})
	if l.ActionLease.ID != expectedID {
		return fmt.Errorf("%w: action lease identity does not match its immutable fields", ErrActionCredentialLeaseMismatch)
	}
	return nil
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
	// HostBinding replaces the stored binding row for a host-supplied
	// binding; a stored row with the same identity then fails closed.
	HostBinding *skill.Binding
}

// ActionCredentialLeaseRedeemer is the durable, product-neutral
// single-use authority boundary for credential resolution. Implementations
// must revalidate current action, Run, and Skill binding authority and consume
// the nonce in one transaction before any credential value is resolved.
type ActionCredentialLeaseRedeemer interface {
	RedeemActionCredentialLease(context.Context, ActionCredentialLeaseRedemptionRequest) error
}

func CheckActionCredentialLeaseCore(lease ActionCredentialLease, call *ActionCall, run *AgentRun, transport string) error {
	if call == nil || run == nil || call.Status != ActionCallStatusRunning || call.LeaseExpiresAt == nil || call.Scope != run.Scope || call.RunID != run.ID {
		return ErrActionCredentialLeaseMismatch
	}
	// A heartbeat may monotonically advance the durable revision and expiry
	// while the same worker still owns the same attempt. The signed identity
	// remains the immutable issuance snapshot; reclaim changes attempt/owner or
	// status and therefore still fails closed.
	if lease.Scope != call.Scope || lease.RunID != call.RunID || lease.ActionCallID != call.ID ||
		call.Revision < lease.ActionLease.ActionCallRevision || lease.ActionLease.Attempt != call.Attempt || lease.ActionLease.WorkerID != strings.TrimSpace(call.LeaseOwner) || CanonicalLeaseTime(*call.LeaseExpiresAt).Before(lease.ActionLease.ExpiresAt) ||
		lease.SkillID != call.SkillID || lease.SkillVersion != call.SkillVersion || lease.Action != call.Action || lease.Transport != strings.TrimSpace(transport) || lease.BindingOwnerID != call.DeploymentID ||
		lease.BindingID != call.BindingID || lease.BindingRevision != call.BindingRevision || lease.AssignedAgentID != run.AssignedAgentID ||
		lease.ActingUser != run.ActingUser {
		return ErrActionCredentialLeaseMismatch
	}
	// A per-person account acts only for the person a run acts for.
	if run.ActingUser == "" {
		for _, reference := range call.CredentialRefs {
			if skill.IsPersonCredentialReference(reference) {
				return ErrActionCredentialLeaseMismatch
			}
		}
	}
	return nil
}

func ActionCredentialFieldReferences(references map[string]skill.CredentialReference, fields map[string][]string) ([]ActionCredentialFieldReference, error) {
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

func ComputeActionLeaseIdentity(scope Scope, actionCallID, runID string, lease ActionLeaseIdentity) string {
	canonical := struct {
		Scope              Scope     `json:"scope"`
		ActionCallID       string    `json:"actionCallId"`
		RunID              string    `json:"runId"`
		ActionCallRevision int64     `json:"actionCallRevision"`
		Attempt            int       `json:"attempt"`
		WorkerID           string    `json:"workerId"`
		ExpiresAt          time.Time `json:"expiresAt"`
	}{scope, actionCallID, runID, lease.ActionCallRevision, lease.Attempt, lease.WorkerID, CanonicalLeaseTime(lease.ExpiresAt)}
	encoded, _ := json.Marshal(canonical)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func CloneActionCredentialFieldReferences(values []ActionCredentialFieldReference) []ActionCredentialFieldReference {
	result := make([]ActionCredentialFieldReference, len(values))
	for index, value := range values {
		result[index] = value
		result[index].Fields = append([]string(nil), value.Fields...)
	}
	return result
}

func CloneActionCredentialFields(values map[string][]string) map[string][]string {
	if values == nil {
		return nil
	}
	result := make(map[string][]string, len(values))
	for name, fields := range values {
		result[name] = append([]string(nil), fields...)
	}
	return result
}

func CanonicalLeaseTime(value time.Time) time.Time { return value.UTC().Round(0) }

func IsValidLeaseIdentifier(value string, maximum int) bool {
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
	if !IsValidLeaseIdentifier(value, 256) {
		return false
	}
	for _, character := range value {
		if !(unicode.IsLetter(character) || unicode.IsDigit(character) || strings.ContainsRune("._/-[]", character)) {
			return false
		}
	}
	return true
}

var (
	ErrActionCredentialLeaseReplay = errors.New("action credential lease was already consumed")
)

// MatchActionCredentialLease rechecks a signed lease against current durable
// state and the host's exact field allowlist. It is suitable for an
// ActionCredentialLeaseRedeemer implementation inside its atomic boundary.
func MatchActionCredentialLease(lease ActionCredentialLease, call *ActionCall, run *AgentRun, transport string, credentialFields map[string][]string) error {
	if err := lease.Validate(); err != nil {
		return err
	}
	if err := CheckActionCredentialLeaseCore(lease, call, run, transport); err != nil {
		return err
	}
	expectedFields, err := ActionCredentialFieldReferences(call.CredentialRefs, credentialFields)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(lease.Credentials, expectedFields) {
		return ErrActionCredentialLeaseMismatch
	}
	return nil
}

// CheckActionCredentialLeaseUmbrella requires the redeemed binding, stored
// or supplied by the host, to belong to the run's umbrella: the Agent of a
// 1:1 run or the Team of a Team run. A run delegated to another Agent has no
// umbrella and redeems nothing.
func CheckActionCredentialLeaseUmbrella(lease ActionCredentialLease, run *AgentRun, binding *skill.Binding) error {
	actor := RunSkillActor(run)
	if binding == nil || !actor.Valid() || binding.DeploymentID != lease.BindingOwnerID || binding.DeploymentID != actor.Umbrella.ID {
		return ErrActionCredentialLeaseMismatch
	}
	return nil
}

func ActionCredentialFieldSelectionDigest(fields map[string][]string) (string, error) {
	type fieldSelection struct {
		Name   string   `json:"name"`
		Fields []string `json:"fields"`
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	selections := make([]fieldSelection, 0, len(names))
	for _, name := range names {
		selected := append([]string(nil), fields[name]...)
		sort.Strings(selected)
		selections = append(selections, fieldSelection{Name: name, Fields: selected})
	}
	payload, err := json.Marshal(selections)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func ActionCredentialLeaseNonceDigest(lease ActionCredentialLease) [sha256.Size]byte {
	return sha256.Sum256([]byte(lease.Issuer + "\x00" + lease.Audience + "\x00" + lease.Nonce))
}

func CheckCurrentActionCredentialBinding(lease ActionCredentialLease, call *ActionCall, binding *skill.Binding) error {
	if call == nil || binding == nil || binding.Disabled ||
		binding.Scope.Kind != lease.Scope.Kind || binding.Scope.ID != lease.Scope.ID ||
		binding.DeploymentID != lease.BindingOwnerID || binding.ID != lease.BindingID || binding.Revision != lease.BindingRevision ||
		binding.SkillID != lease.SkillID || binding.SkillVersion != lease.SkillVersion ||
		!slices.Contains(binding.AllowedActions, lease.Action) ||
		riskLevelRank(call.Risk) < 0 || riskLevelRank(binding.MaximumRisk) < riskLevelRank(call.Risk) ||
		!credentialReferencesContainExact(binding.Credentials, call.CredentialRefs) {
		return ErrActionCredentialLeaseMismatch
	}
	return nil
}

// credentialReferencesContainExact permits a binding to carry credentials for
// other actions or adapters while requiring every credential selected by the
// durable ActionCall to match exactly. The trusted host independently derives
// the selected names from the immutable action contract and MatchActionCredentialLease
// rejects any extra or missing ActionCall credential.
func credentialReferencesContainExact(available, selected map[string]skill.CredentialReference) bool {
	if len(selected) == 0 || len(available) < len(selected) {
		return false
	}
	for name, reference := range selected {
		if available[name] != reference {
			return false
		}
	}
	return true
}

func riskLevelRank(value skill.RiskLevel) int {
	switch value {
	case skill.RiskLevelRead:
		return 0
	case skill.RiskLevelWrite:
		return 1
	case skill.RiskLevelExternal:
		return 2
	case skill.RiskLevelProduction:
		return 3
	case skill.RiskLevelDestructive:
		return 4
	default:
		return -1
	}
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
	issuedAt, expiresAt := CanonicalLeaseTime(request.IssuedAt), CanonicalLeaseTime(request.ExpiresAt)
	actionExpiry := CanonicalLeaseTime(*call.LeaseExpiresAt)
	if expiresAt.After(actionExpiry) {
		return nil, fmt.Errorf("%w: credential expiry exceeds the durable action lease", ErrActionCredentialLeaseInvalid)
	}
	credentials, err := ActionCredentialFieldReferences(call.CredentialRefs, request.CredentialFields)
	if err != nil {
		return nil, err
	}
	actionLease := ActionLeaseIdentity{
		ActionCallRevision: call.Revision, Attempt: call.Attempt, WorkerID: strings.TrimSpace(call.LeaseOwner), ExpiresAt: actionExpiry,
	}
	actionLease.ID = ComputeActionLeaseIdentity(call.Scope, call.ID, call.RunID, actionLease)
	lease := &ActionCredentialLease{
		Version: ActionCredentialLeaseVersion, TenantID: strings.TrimSpace(request.TenantID), Scope: call.Scope,
		RunID: call.RunID, ActionCallID: call.ID, ActionLease: actionLease,
		SkillID: call.SkillID, SkillVersion: call.SkillVersion, Action: call.Action, Transport: strings.TrimSpace(request.Transport),
		BindingOwnerID: call.DeploymentID, BindingID: call.BindingID, BindingRevision: call.BindingRevision,
		AssignedAgentID: run.AssignedAgentID, ActingUser: run.ActingUser, Credentials: credentials,
		Issuer: strings.TrimSpace(request.Issuer), Audience: strings.TrimSpace(request.Audience), IssuedAt: issuedAt, ExpiresAt: expiresAt, Nonce: strings.TrimSpace(request.Nonce),
	}
	if err := lease.Validate(); err != nil {
		return nil, err
	}
	return lease, nil
}
