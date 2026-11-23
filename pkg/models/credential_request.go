package models

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CredentialRequestStatusPending   = "pending"
	CredentialRequestStatusResolved  = "resolved"
	CredentialRequestStatusDismissed = "dismissed"
)

var ErrCredentialRequestConflict = errors.New("credential request revision conflict")

var ErrInvalidCredentialRequest = errors.New("invalid credential request")

// Bounds of a credential request's named fields.
const (
	MaxCredentialRequestFields      = 20
	MaxCredentialRequestFieldName   = 64
	MaxCredentialRequestServiceName = 80
)

// CredentialRequestField is one field a request asks the person to fill, for
// a credential type whose fields the request names. Secure fields are hidden
// as they are typed. It never carries a value.
type CredentialRequestField struct {
	Name   string `json:"name"`
	Secure bool   `json:"secure"`
}

// CredentialRequest is a user interaction for one missing credential of a
// host credential type. The host decides what evidence completes it (the
// saved credential the person picks); the kernel only records the outcome
// and wakes the Run.
type CredentialRequest struct {
	ID    string `json:"id"`
	Scope Scope  `json:"scope"`
	// Owner is the umbrella of the requesting chat; DeploymentID is Owner.ID
	// and AgentID the acting member, as on SkillSetupRequest.
	Owner            ObjectiveOwner `json:"owner"`
	DeploymentID     string         `json:"deploymentId"`
	AgentID          string         `json:"agentId"`
	ConversationID   string         `json:"conversationId"`
	TriggerMessageID string         `json:"triggerMessageId"`
	RunID            string         `json:"runId"`
	ActionCallID     string         `json:"actionCallId"`
	// CredentialType is a host credential type id from the host's
	// requestable types. Service names what the credential is for, as shown
	// to the person (for example Alpaca).
	CredentialType string                   `json:"credentialType"`
	Service        string                   `json:"service,omitempty"`
	Website        string                   `json:"website,omitempty"`
	Fields         []CredentialRequestField `json:"fields,omitempty"`
	Reason         string                   `json:"reason"`
	Status         string                   `json:"status"`
	Revision       int64                    `json:"revision"`
	CreatedAt      time.Time                `json:"createdAt"`
	UpdatedAt      time.Time                `json:"updatedAt"`
	ResolvedBy     string                   `json:"resolvedBy,omitempty"`
	// Credential is the saved credential that resolved the request.
	Credential *CredentialRequestCredential `json:"credential,omitempty"`
}

// CredentialRequestCredential names the host credential the person saved or
// picked for a request. It is a reference, never a value.
type CredentialRequestCredential struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (r *CredentialRequest) Validate() error {
	if r == nil || r.Scope.Validate() != nil || r.Revision < 1 || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) ||
		validateInteractionUmbrella(r.Owner, r.DeploymentID, r.AgentID, false) != nil {
		return ErrInvalidCredentialRequest
	}
	for _, value := range []string{r.ID, r.DeploymentID, r.ConversationID, r.TriggerMessageID, r.RunID, r.ActionCallID} {
		if !IsValidOpaqueIdentifier(value, 256) {
			return ErrInvalidCredentialRequest
		}
	}
	if strings.TrimSpace(r.Reason) == "" || utf8.RuneCountInString(r.Reason) > 1000 {
		return ErrInvalidCredentialRequest
	}
	if !IsValidOpaqueIdentifier(r.CredentialType, 128) || utf8.RuneCountInString(r.Service) > MaxCredentialRequestServiceName || r.Service != strings.TrimSpace(r.Service) {
		return ErrInvalidCredentialRequest
	}
	if r.Website != "" {
		if origin, err := NormalizeCredentialRequestWebsite(r.Website); err != nil || origin != r.Website {
			return ErrInvalidCredentialRequest
		}
	}
	if ValidateCredentialRequestFields(r.Fields) != nil {
		return ErrInvalidCredentialRequest
	}
	switch r.Status {
	case CredentialRequestStatusPending:
		if r.ResolvedBy != "" || r.Credential != nil {
			return ErrInvalidCredentialRequest
		}
	case CredentialRequestStatusResolved:
		if strings.TrimSpace(r.ResolvedBy) == "" || r.Credential == nil || !IsValidOpaqueIdentifier(r.Credential.ID, 256) || strings.TrimSpace(r.Credential.Name) == "" {
			return ErrInvalidCredentialRequest
		}
	case CredentialRequestStatusDismissed:
		if strings.TrimSpace(r.ResolvedBy) == "" || r.Credential != nil {
			return ErrInvalidCredentialRequest
		}
	default:
		return ErrInvalidCredentialRequest
	}
	return nil
}

// ValidateCredentialRequestFields checks named fields: each a unique,
// trimmed, non-empty name.
func ValidateCredentialRequestFields(fields []CredentialRequestField) error {
	if len(fields) > MaxCredentialRequestFields {
		return errors.New("a credential request names at most 20 fields")
	}
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		if field.Name == "" || field.Name != strings.TrimSpace(field.Name) || utf8.RuneCountInString(field.Name) > MaxCredentialRequestFieldName {
			return errors.New("every field needs a name of at most 64 characters")
		}
		if seen[strings.ToLower(field.Name)] {
			return errors.New("field names must be different")
		}
		seen[strings.ToLower(field.Name)] = true
	}
	return nil
}

// SameCredentialAsked reports whether two requests ask for the same
// credential: one card answers both.
func SameCredentialAsked(a, b *CredentialRequest) bool {
	return a.CredentialType == b.CredentialType && a.Website == b.Website && strings.EqualFold(a.Service, b.Service)
}

// NormalizeCredentialRequestWebsite returns the exact http(s) origin
// (scheme://host[:port]) of a website, rejecting paths, queries and userinfo.
func NormalizeCredentialRequestWebsite(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("website must be an http(s) origin such as https://www.example.com")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("website must be an http(s) origin such as https://www.example.com")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "" || (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return scheme + "://" + host, nil
	}
	return scheme + "://" + host + ":" + port, nil
}

type CredentialRequestStore interface {
	GetCredentialRequest(context.Context, Scope, string) (*CredentialRequest, error)
	ListCredentialRequests(context.Context, Scope, string, string) ([]*CredentialRequest, error)
	SaveCredentialRequest(context.Context, *CredentialRequest, int64) error
}

func CloneCredentialRequest(r *CredentialRequest) *CredentialRequest {
	if r == nil {
		return nil
	}
	c := *r
	c.Fields = append([]CredentialRequestField(nil), r.Fields...)
	if r.Credential != nil {
		credential := *r.Credential
		c.Credential = &credential
	}
	return &c
}
