package models

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"maps"
	"net/url"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

var (
	ErrEmbedInstallationNotFound = errors.New("embed installation not found")
	ErrEmbedSessionNotFound      = errors.New("embed session not found")
	ErrEmbedRevisionConflict     = errors.New("embed installation revision conflict")
	ErrEmbedRouteConflict        = errors.New("embed public route already exists")
	ErrEmbedSessionUnauthorized  = errors.New("embed session capability is invalid")
	ErrEmbedLimitExceeded        = errors.New("embed session message limit is exhausted")
)

type EmbedInstallationStatus string

const (
	EmbedInstallationActive  EmbedInstallationStatus = "active"
	EmbedInstallationPaused  EmbedInstallationStatus = "paused"
	EmbedInstallationRevoked EmbedInstallationStatus = "revoked"
)

type EmbedIdentityMode string

const (
	EmbedIdentityAnonymous EmbedIdentityMode = "anonymous"
	EmbedIdentitySigned    EmbedIdentityMode = "signed"
)

type EmbedIdentityPolicy struct {
	Mode           EmbedIdentityMode `json:"mode"`
	Issuer         string            `json:"issuer,omitempty"`
	Audience       string            `json:"audience,omitempty"`
	KeyID          string            `json:"keyId,omitempty"`
	PublicKey      string            `json:"publicKey,omitempty"`
	RequiredClaims []string          `json:"requiredClaims,omitempty"`
}

type EmbedActionGrant struct {
	BindingID string `json:"bindingId"`
	Action    string `json:"action"`
}

type EmbedPermissionPolicy struct {
	AllowTextChat     bool               `json:"allowTextChat"`
	ActionGrants      []EmbedActionGrant `json:"actionGrants,omitempty"`
	MaximumActionRisk skill.RiskLevel    `json:"maximumActionRisk"`
	MaximumMessages   int                `json:"maximumMessages"`
	SessionTTLSeconds int64              `json:"sessionTtlSeconds"`
	MaximumInputBytes int                `json:"maximumInputBytes"`
}

type EmbedInstallation struct {
	ID             string                  `json:"id"`
	Scope          Scope                   `json:"scope"`
	DeploymentID   string                  `json:"deploymentId"`
	Name           string                  `json:"name"`
	PublicRoute    string                  `json:"publicRoute"`
	Status         EmbedInstallationStatus `json:"status"`
	AllowedOrigins []string                `json:"allowedOrigins"`
	Identity       EmbedIdentityPolicy     `json:"identity"`
	Permissions    EmbedPermissionPolicy   `json:"permissions"`
	// CreatedBy is the person who created the installation, when the host
	// names one. Its chats go to their default space while the Agent is in
	// no space.
	CreatedBy string    `json:"createdBy,omitempty"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (i *EmbedInstallation) Validate() error {
	if i == nil || i.Scope.Validate() != nil || !IsValidOpaqueIdentifier(i.ID, 128) ||
		!IsValidOpaqueIdentifier(i.DeploymentID, 256) || strings.TrimSpace(i.Name) == "" || len(i.Name) > 160 ||
		!IsValidOpaqueIdentifier(i.PublicRoute, 128) || i.Revision < 1 || i.CreatedAt.IsZero() || i.UpdatedAt.IsZero() {
		return errors.New("embed installation identity, owner, route, revision, and timestamps are required")
	}
	if i.Status != EmbedInstallationActive && i.Status != EmbedInstallationPaused && i.Status != EmbedInstallationRevoked {
		return errors.New("embed installation status is invalid")
	}
	if err := validateEmbedOrigins(i.AllowedOrigins); err != nil {
		return err
	}
	if err := i.Identity.Validate(); err != nil {
		return err
	}
	return i.Permissions.Validate()
}

func (p EmbedIdentityPolicy) Validate() error {
	switch p.Mode {
	case EmbedIdentityAnonymous:
		if p.Issuer != "" || p.Audience != "" || p.KeyID != "" || p.PublicKey != "" || len(p.RequiredClaims) != 0 {
			return errors.New("anonymous embed identity cannot declare signed-token settings")
		}
	case EmbedIdentitySigned:
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(p.PublicKey))
		if strings.TrimSpace(p.Issuer) == "" || strings.TrimSpace(p.Audience) == "" || strings.TrimSpace(p.KeyID) == "" || err != nil || len(decoded) != 32 {
			return errors.New("signed embed identity requires issuer, audience, key id, and an Ed25519 public key")
		}
	default:
		return errors.New("embed identity mode is invalid")
	}
	seen := map[string]bool{}
	for _, claim := range p.RequiredClaims {
		claim = strings.TrimSpace(claim)
		if !IsValidOpaqueIdentifier(claim, 128) || seen[claim] || secretLikeSessionKey(claim) {
			return errors.New("embed required claims must be unique, non-secret identifiers")
		}
		seen[claim] = true
	}
	return nil
}

func (p EmbedPermissionPolicy) Validate() error {
	if !p.AllowTextChat || p.MaximumMessages < 1 || p.MaximumMessages > 10000 ||
		p.SessionTTLSeconds < 60 || p.SessionTTLSeconds > int64((30*24*time.Hour)/time.Second) ||
		p.MaximumInputBytes < 1 || p.MaximumInputBytes > 1024*1024 {
		return errors.New("embed permissions require text chat and bounded message, TTL, and input limits")
	}
	if p.MaximumActionRisk != skill.RiskLevelRead && p.MaximumActionRisk != skill.RiskLevelWrite &&
		p.MaximumActionRisk != skill.RiskLevelExternal && p.MaximumActionRisk != skill.RiskLevelProduction &&
		p.MaximumActionRisk != skill.RiskLevelDestructive {
		return errors.New("embed maximum action risk is invalid")
	}
	seen := map[string]bool{}
	for _, grant := range p.ActionGrants {
		key := strings.TrimSpace(grant.BindingID) + "\x00" + strings.TrimSpace(grant.Action)
		if !IsValidOpaqueIdentifier(strings.TrimSpace(grant.BindingID), 256) || !IsValidOpaqueIdentifier(strings.TrimSpace(grant.Action), 128) || seen[key] {
			return errors.New("embed action grants must be unique exact binding/action pairs")
		}
		seen[key] = true
	}
	return nil
}

type EmbedSessionStatus string

const (
	EmbedSessionActive  EmbedSessionStatus = "active"
	EmbedSessionExpired EmbedSessionStatus = "expired"
	EmbedSessionRevoked EmbedSessionStatus = "revoked"
)

type EmbedSession struct {
	ID             string                 `json:"id"`
	Scope          Scope                  `json:"scope"`
	InstallationID string                 `json:"installationId"`
	ConversationID string                 `json:"conversationId"`
	Status         EmbedSessionStatus     `json:"status"`
	Origin         string                 `json:"origin"`
	MessageCount   int                    `json:"messageCount"`
	ExpiresAt      time.Time              `json:"expiresAt"`
	CreatedAt      time.Time              `json:"createdAt"`
	UpdatedAt      time.Time              `json:"updatedAt"`
	CapabilityHash string                 `json:"-"`
	VerifiedClaims map[string]interface{} `json:"-"`
}

func (s *EmbedSession) Validate() error {
	if s == nil || s.Scope.Validate() != nil || !IsValidOpaqueIdentifier(s.ID, 128) || !IsValidOpaqueIdentifier(s.InstallationID, 128) ||
		!IsValidOpaqueIdentifier(s.ConversationID, 128) || s.MessageCount < 0 || s.ExpiresAt.IsZero() || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() ||
		len(s.CapabilityHash) != sha256.Size*2 {
		return errors.New("embed session identity, capability, limits, and timestamps are invalid")
	}
	if s.Status != EmbedSessionActive && s.Status != EmbedSessionExpired && s.Status != EmbedSessionRevoked {
		return errors.New("embed session status is invalid")
	}
	if _, err := NormalizeEmbedOrigin(s.Origin); err != nil {
		return err
	}
	return ValidateCredentialFreeContext(s.VerifiedClaims)
}

type EmbedInstallationFilter struct {
	Scope        Scope
	DeploymentID string
}

type EmbedStore interface {
	CreateEmbedInstallation(context.Context, *EmbedInstallation) error
	GetEmbedInstallation(context.Context, Scope, string) (*EmbedInstallation, error)
	GetEmbedInstallationByRoute(context.Context, string) (*EmbedInstallation, error)
	ListEmbedInstallations(context.Context, EmbedInstallationFilter) ([]*EmbedInstallation, error)
	UpdateEmbedInstallation(context.Context, *EmbedInstallation, int64) error
	CreateEmbedSession(context.Context, *EmbedSession) error
	GetEmbedSession(context.Context, Scope, string) (*EmbedSession, error)
	GetEmbedSessionByConversation(context.Context, Scope, string) (*EmbedSession, error)
	ConsumeEmbedSessionMessage(context.Context, Scope, string, int, time.Time) (*EmbedSession, error)
}

func validateEmbedOrigins(values []string) error {
	if len(values) == 0 || len(values) > 100 {
		return errors.New("embed installation requires between 1 and 100 allowed origins")
	}
	seen := map[string]bool{}
	for _, value := range values {
		normalized, err := NormalizeEmbedOrigin(value)
		if err != nil || seen[normalized] {
			return errors.New("embed allowed origins must be unique exact HTTP(S) origins")
		}
		seen[normalized] = true
	}
	return nil
}

func NormalizeEmbedOrigin(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", errors.New("embed origin must be an exact HTTP(S) origin")
	}
	parsed.Path = ""
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func secretLikeSessionKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(key))
	for _, fragment := range []string{"password", "secret", "token", "apikey", "privatekey", "credential"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func CloneEmbedInstallation(value *EmbedInstallation) *EmbedInstallation {
	if value == nil {
		return nil
	}
	copy := *value
	copy.AllowedOrigins = append([]string(nil), value.AllowedOrigins...)
	copy.Identity.RequiredClaims = append([]string(nil), value.Identity.RequiredClaims...)
	copy.Permissions.ActionGrants = append([]EmbedActionGrant(nil), value.Permissions.ActionGrants...)
	return &copy
}

func CloneEmbedSession(value *EmbedSession) *EmbedSession {
	if value == nil {
		return nil
	}
	copy := *value
	copy.VerifiedClaims = maps.Clone(value.VerifiedClaims)
	return &copy
}
