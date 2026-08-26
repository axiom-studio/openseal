package runtime

import (
	"errors"
	"net/url"
	"strings"
)

var ErrExternalOperationClaimed = errors.New("external operation is already claimed")

// ExternalOperationIdentity is the stable, capability-independent identity of
// an externally observable mutation. Resource names what is changed and
// Operation names the intended effect. The ActionCall idempotency key supplies
// the caller-owned occurrence identity: retries of one occurrence reuse its
// durable receipt, while a later reviewed occurrence may intentionally perform
// the same operation against the same resource again. Transient DOM references,
// Skill versions, and Run IDs never participate in this identity.
type ExternalOperationIdentity struct {
	Resource  string `json:"resource"`
	Operation string `json:"operation"`
}

func (i ExternalOperationIdentity) Validate() error {
	if _, err := canonicalExternalOperationResource(i.Resource); err != nil {
		return err
	}
	if !validOpaqueIdentifier(i.Operation, 256) {
		return errors.New("external operation name must be a bounded opaque identifier")
	}
	return nil
}

func canonicalExternalOperationResource(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("external operation resource must be between 1 and 2048 characters")
	}
	// Provider channel handles are stable human-facing targets, but their '#'
	// prefix is a URL fragment marker and therefore deliberately excluded from
	// generic opaque identifiers. Canonicalize the handle into an unambiguous
	// opaque resource before applying the generic validation. This lets an Agent
	// truthfully identify a Slack-style target such as #agents without treating
	// it as a malformed URL or requiring it to know the provider's internal ID.
	if strings.HasPrefix(value, "#") {
		handle := strings.TrimSpace(strings.TrimPrefix(value, "#"))
		if validOpaqueIdentifier(handle, 2040) {
			return "channel-handle:" + strings.ToLower(handle), nil
		}
	}
	parsed, err := url.Parse(value)
	if err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) {
		if err := validatePublicEvidenceURL(value); err != nil {
			return "", errors.New("external operation resource URL is invalid or credential-bearing")
		}
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Fragment = ""
		if parsed.Path == "" {
			parsed.Path = "/"
		}
		parsed.RawQuery = parsed.Query().Encode()
		return parsed.String(), nil
	}
	if !validOpaqueIdentifier(value, 2048) {
		return "", errors.New("external operation resource must be an HTTP(S) URL or opaque identifier")
	}
	return value, nil
}

func computeExternalOperationDigest(scope Scope, owner ObjectiveOwner, objectiveID string, identity *ExternalOperationIdentity, idempotencyKey string) (string, error) {
	if identity == nil {
		return "", nil
	}
	if err := identity.Validate(); err != nil {
		return "", err
	}
	resource, _ := canonicalExternalOperationResource(identity.Resource)
	operation := strings.ToLower(strings.TrimSpace(identity.Operation))
	occurrence := strings.TrimSpace(idempotencyKey)
	ownerKey := strings.TrimSpace(objectiveID)
	if ownerKey == "" {
		ownerKey = string(owner.Type) + "\x00" + strings.TrimSpace(owner.ID)
	}
	return hashString(scope.Kind + "\x00" + scope.ID + "\x00" + ownerKey + "\x00" + resource + "\x00" + operation + "\x00" + occurrence), nil
}

// externalOperationLockKey is safe to bind as PostgreSQL TEXT. The canonical
// digest is scope-local, but advisory locks share a database-wide namespace;
// hashing the scope tuple preserves that separation without sending NUL
// delimiters (which PostgreSQL text values cannot encode) across the driver.
func externalOperationLockKey(scope Scope, digest string) string {
	return hashString(scope.Kind + "\x00" + scope.ID + "\x00" + digest)
}

func externalOperationProtects(status ActionCallStatus) bool {
	switch status {
	case ActionCallStatusReady, ActionCallStatusWaitingApproval, ActionCallStatusRunning,
		ActionCallStatusSucceeded, ActionCallStatusCompensating, ActionCallStatusCompensated:
		return true
	default:
		return false
	}
}

func externalOperationReceiptComplete(status ActionCallStatus) bool {
	return status == ActionCallStatusSucceeded || status == ActionCallStatusCompensated
}

// ExternalOperationConflictError identifies the prior authoritative receipt.
// The attempted proposal is not persisted.
type ExternalOperationConflictError struct {
	Prior *ActionCall
}

func (e *ExternalOperationConflictError) Error() string { return ErrExternalOperationClaimed.Error() }
func (e *ExternalOperationConflictError) Unwrap() error { return ErrExternalOperationClaimed }
