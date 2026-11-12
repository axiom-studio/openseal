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
	CredentialRequestKindWebsiteLogin = "website_login"
	CredentialRequestKindPaymentCard  = "payment_card"
)

const (
	CredentialRequestStatusPending   = "pending"
	CredentialRequestStatusResolved  = "resolved"
	CredentialRequestStatusDismissed = "dismissed"
)

var ErrCredentialRequestConflict = errors.New("credential request revision conflict")

var ErrInvalidCredentialRequest = errors.New("invalid credential request")

// CredentialRequest is a user interaction for one missing credential. The
// host decides what evidence completes it (a matching vault credential saved
// after the request); the kernel only records the outcome and wakes the Run.
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
	Kind             string         `json:"kind"`
	Website          string         `json:"website,omitempty"`
	Reason           string         `json:"reason"`
	Status           string         `json:"status"`
	Revision         int64          `json:"revision"`
	CreatedAt        time.Time      `json:"createdAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
	ResolvedBy       string         `json:"resolvedBy,omitempty"`
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
	switch r.Kind {
	case CredentialRequestKindWebsiteLogin:
		if origin, err := NormalizeCredentialRequestWebsite(r.Website); err != nil || origin != r.Website {
			return ErrInvalidCredentialRequest
		}
	case CredentialRequestKindPaymentCard:
		if r.Website != "" {
			if origin, err := NormalizeCredentialRequestWebsite(r.Website); err != nil || origin != r.Website {
				return ErrInvalidCredentialRequest
			}
		}
	default:
		return ErrInvalidCredentialRequest
	}
	switch r.Status {
	case CredentialRequestStatusPending:
		if r.ResolvedBy != "" {
			return ErrInvalidCredentialRequest
		}
	case CredentialRequestStatusResolved, CredentialRequestStatusDismissed:
		if strings.TrimSpace(r.ResolvedBy) == "" {
			return ErrInvalidCredentialRequest
		}
	default:
		return ErrInvalidCredentialRequest
	}
	return nil
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
	return &c
}
