package activity

import (
	"errors"
	"net/mail"
	"net/url"
	"strings"
	"unicode"

	"github.com/axiom-studio/openseal/pkg/models"
)

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
	if _, err := CanonicalExternalOperationResource(i.Resource); err != nil {
		return err
	}
	if !models.IsValidOpaqueIdentifier(i.Operation, 256) {
		return errors.New("external operation name must be a bounded opaque identifier")
	}
	return nil
}

// CanonicalExternalOperationResource returns the canonical form of an
// external operation resource: a channel handle, an HTTP(S) URL, a single
// mailbox or an opaque identifier. It fails for anything else.
func CanonicalExternalOperationResource(value string) (string, error) {
	rawValue := value
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
		if models.IsValidOpaqueIdentifier(handle, 2040) {
			return "channel-handle:" + strings.ToLower(handle), nil
		}
	}
	parsed, err := url.Parse(value)
	if err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) {
		if err := models.ValidatePublicEvidenceURL(value); err != nil {
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
	// A single mailbox is a stable target for externally delivered messages.
	// Consume every mailto prefix here so malformed mailto values cannot fall
	// through to the intentionally broader opaque-identifier grammar.
	if strings.HasPrefix(strings.ToLower(value), "mailto:") || strings.ContainsRune(value, '@') {
		mailbox := value
		if strings.HasPrefix(strings.ToLower(mailbox), "mailto:") {
			mailbox = mailbox[len("mailto:"):]
		}
		invalid := errors.New("external operation resource must be an HTTP(S) URL, a single email address, or opaque identifier")
		if len(mailbox)+len("mailto:") > 2048 || strings.ContainsAny(mailbox, "%?#/\\") || strings.IndexFunc(rawValue, unicode.IsControl) >= 0 || strings.IndexFunc(mailbox, unicode.IsSpace) >= 0 {
			return "", invalid
		}
		address, err := mail.ParseAddress(mailbox)
		if err != nil || address.Name != "" || address.Address != mailbox {
			return "", invalid
		}
		at := strings.LastIndexByte(mailbox, '@')
		return "mailto:" + mailbox[:at+1] + strings.ToLower(mailbox[at+1:]), nil
	}
	if !models.IsValidOpaqueIdentifier(value, 2048) {
		return "", errors.New("external operation resource must be an HTTP(S) URL, a single email address, or opaque identifier")
	}
	return value, nil
}

// ComputeExternalOperationDigest returns the digest that identifies one
// occurrence of an external operation for its owner, or "" when identity is
// nil.
func ComputeExternalOperationDigest(scope models.Scope, owner models.ObjectiveOwner, objectiveID string, identity *ExternalOperationIdentity, idempotencyKey string) (string, error) {
	if identity == nil {
		return "", nil
	}
	if err := identity.Validate(); err != nil {
		return "", err
	}
	resource, _ := CanonicalExternalOperationResource(identity.Resource)
	operation := strings.ToLower(strings.TrimSpace(identity.Operation))
	occurrence := strings.TrimSpace(idempotencyKey)
	ownerKey := strings.TrimSpace(objectiveID)
	if ownerKey == "" {
		ownerKey = string(owner.Type) + "\x00" + strings.TrimSpace(owner.ID)
	}
	return models.SHA256DigestString(scope.Kind + "\x00" + scope.ID + "\x00" + ownerKey + "\x00" + resource + "\x00" + operation + "\x00" + occurrence), nil
}
