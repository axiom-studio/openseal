package runtime

import (
	"errors"
	"net/url"
	"strings"
)

var ErrExternalOperationClaimed = errors.New("external operation is already claimed")

// ExternalOperationIdentity is the stable, capability-independent identity of
// an externally observable mutation. Resource names what is changed and
// Operation names the intended effect. It deliberately excludes transient DOM
// references, model wording, Skill versions, and Run IDs so a later Run can
// reuse the original durable receipt.
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

func computeExternalOperationDigest(scope Scope, owner ObjectiveOwner, objectiveID string, identity *ExternalOperationIdentity) (string, error) {
	if identity == nil {
		return "", nil
	}
	if err := identity.Validate(); err != nil {
		return "", err
	}
	resource, _ := canonicalExternalOperationResource(identity.Resource)
	operation := strings.ToLower(strings.TrimSpace(identity.Operation))
	ownerKey := strings.TrimSpace(objectiveID)
	if ownerKey == "" {
		ownerKey = string(owner.Type) + "\x00" + strings.TrimSpace(owner.ID)
	}
	return hashString(scope.Kind + "\x00" + scope.ID + "\x00" + ownerKey + "\x00" + resource + "\x00" + operation), nil
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
