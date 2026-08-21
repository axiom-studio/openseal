package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

var (
	ErrEmbedInstallationNotFound = errors.New("embed installation not found")
	ErrEmbedSessionNotFound      = errors.New("embed session not found")
	ErrEmbedRevisionConflict     = errors.New("embed installation revision conflict")
	ErrEmbedRouteConflict        = errors.New("embed public route already exists")
	ErrEmbedOriginDenied         = errors.New("embed origin is not allowed")
	ErrEmbedSessionUnauthorized  = errors.New("embed session capability is invalid")
	ErrEmbedAuthentication       = errors.New("embed host authentication is required")
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
	Revision       int64                   `json:"revision"`
	CreatedAt      time.Time               `json:"createdAt"`
	UpdatedAt      time.Time               `json:"updatedAt"`
}

func (i *EmbedInstallation) Validate() error {
	if i == nil || i.Scope.Validate() != nil || !validOpaqueIdentifier(i.ID, 128) ||
		!validOpaqueIdentifier(i.DeploymentID, 256) || strings.TrimSpace(i.Name) == "" || len(i.Name) > 160 ||
		!validOpaqueIdentifier(i.PublicRoute, 128) || i.Revision < 1 || i.CreatedAt.IsZero() || i.UpdatedAt.IsZero() {
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
		if !validOpaqueIdentifier(claim, 128) || seen[claim] || secretLikeSessionKey(claim) {
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
		if !validOpaqueIdentifier(strings.TrimSpace(grant.BindingID), 256) || !validOpaqueIdentifier(strings.TrimSpace(grant.Action), 128) || seen[key] {
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
	if s == nil || s.Scope.Validate() != nil || !validOpaqueIdentifier(s.ID, 128) || !validOpaqueIdentifier(s.InstallationID, 128) ||
		!validOpaqueIdentifier(s.ConversationID, 128) || s.MessageCount < 0 || s.ExpiresAt.IsZero() || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() ||
		len(s.CapabilityHash) != sha256.Size*2 {
		return errors.New("embed session identity, capability, limits, and timestamps are invalid")
	}
	if s.Status != EmbedSessionActive && s.Status != EmbedSessionExpired && s.Status != EmbedSessionRevoked {
		return errors.New("embed session status is invalid")
	}
	if _, err := normalizeEmbedOrigin(s.Origin); err != nil {
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

type CreateEmbedInstallationRequest struct {
	ID             string
	Scope          Scope
	DeploymentID   string
	Name           string
	AllowedOrigins []string
	Identity       EmbedIdentityPolicy
	Permissions    EmbedPermissionPolicy
}

type UpdateEmbedInstallationRequest struct {
	Scope            Scope
	ID               string
	ExpectedRevision int64
	Name             string
	Status           EmbedInstallationStatus
	AllowedOrigins   []string
	Identity         EmbedIdentityPolicy
	Permissions      EmbedPermissionPolicy
}

type OpenEmbedSessionRequest struct {
	PublicRoute    string
	Origin         string
	VerifiedClaims map[string]interface{}
}

type OpenEmbedSessionResult struct {
	Installation *EmbedInstallation `json:"installation"`
	Session      *EmbedSession      `json:"session"`
	Capability   string             `json:"capability"`
}

type EmbedSessionService struct {
	store         EmbedStore
	conversations *ConversationService
	now           func() time.Time
	newID         func() string
	randomToken   func() (string, error)
}

func NewEmbedSessionService(store EmbedStore, conversations ConversationStore) (*EmbedSessionService, error) {
	if store == nil || conversations == nil {
		return nil, errors.New("embed and conversation stores are required")
	}
	return &EmbedSessionService{store: store, conversations: NewConversationService(conversations), now: time.Now, newID: uuid.NewString, randomToken: randomEmbedToken}, nil
}

func (s *EmbedSessionService) CreateInstallation(ctx context.Context, req CreateEmbedInstallationRequest) (*EmbedInstallation, error) {
	now := s.now().UTC()
	route, err := s.randomToken()
	if err != nil {
		return nil, err
	}
	installation := &EmbedInstallation{
		ID: strings.TrimSpace(req.ID), Scope: req.Scope, DeploymentID: strings.TrimSpace(req.DeploymentID), Name: strings.TrimSpace(req.Name),
		PublicRoute: route, Status: EmbedInstallationActive, AllowedOrigins: normalizeEmbedOrigins(req.AllowedOrigins),
		Identity: req.Identity, Permissions: req.Permissions, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if installation.ID == "" {
		installation.ID = s.newID()
	}
	if err := installation.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.CreateEmbedInstallation(ctx, installation); err != nil {
		return nil, err
	}
	return cloneEmbedInstallation(installation), nil
}

func (s *EmbedSessionService) GetInstallation(ctx context.Context, scope Scope, id string) (*EmbedInstallation, error) {
	return s.store.GetEmbedInstallation(ctx, scope, strings.TrimSpace(id))
}

func (s *EmbedSessionService) ListInstallations(ctx context.Context, filter EmbedInstallationFilter) ([]*EmbedInstallation, error) {
	return s.store.ListEmbedInstallations(ctx, filter)
}

func (s *EmbedSessionService) UpdateInstallation(ctx context.Context, req UpdateEmbedInstallationRequest) (*EmbedInstallation, error) {
	current, err := s.store.GetEmbedInstallation(ctx, req.Scope, strings.TrimSpace(req.ID))
	if err != nil {
		return nil, err
	}
	if current.Revision != req.ExpectedRevision || current.Status == EmbedInstallationRevoked && req.Status != EmbedInstallationRevoked {
		return nil, ErrEmbedRevisionConflict
	}
	next := cloneEmbedInstallation(current)
	next.Name, next.Status = strings.TrimSpace(req.Name), req.Status
	next.AllowedOrigins, next.Identity, next.Permissions = normalizeEmbedOrigins(req.AllowedOrigins), req.Identity, req.Permissions
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.UpdateEmbedInstallation(ctx, next, current.Revision); err != nil {
		return nil, err
	}
	return cloneEmbedInstallation(next), nil
}

func (s *EmbedSessionService) OpenSession(ctx context.Context, req OpenEmbedSessionRequest) (*OpenEmbedSessionResult, error) {
	installation, err := s.store.GetEmbedInstallationByRoute(ctx, strings.TrimSpace(req.PublicRoute))
	if err != nil {
		return nil, err
	}
	origin, err := authorizeEmbedOrigin(installation, req.Origin)
	if err != nil {
		return nil, err
	}
	if installation.Status != EmbedInstallationActive {
		return nil, ErrEmbedSessionUnauthorized
	}
	claims := cloneMap(req.VerifiedClaims)
	if installation.Identity.Mode == EmbedIdentitySigned {
		if len(claims) == 0 {
			return nil, ErrEmbedAuthentication
		}
		for _, claim := range installation.Identity.RequiredClaims {
			if _, ok := claims[claim]; !ok {
				return nil, fmt.Errorf("%w: required claim %s is missing", ErrEmbedAuthentication, claim)
			}
		}
	} else if len(claims) != 0 {
		return nil, errors.New("anonymous embed cannot accept verified claims")
	}
	if err := ValidateCredentialFreeContext(claims); err != nil {
		return nil, err
	}
	token, err := s.randomToken()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	id := s.newID()
	session := &EmbedSession{
		ID: id, Scope: installation.Scope, InstallationID: installation.ID, ConversationID: "embed-" + id,
		Status: EmbedSessionActive, Origin: origin, ExpiresAt: now.Add(time.Duration(installation.Permissions.SessionTTLSeconds) * time.Second),
		CreatedAt: now, UpdatedAt: now, CapabilityHash: hashEmbedCapability(token), VerifiedClaims: claims,
	}
	if err := session.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.CreateEmbedSession(ctx, session); err != nil {
		return nil, err
	}
	if _, _, err := s.conversations.CreateConversation(ctx, CreateConversationRequest{
		ID: session.ConversationID, Scope: session.Scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: installation.DeploymentID},
		Title: "Embedded chat", Origin: &ConversationReference{Kind: ConversationReferenceEmbedSession, ID: session.ID},
		IdempotencyKey: "embed-session:" + session.ID,
	}); err != nil {
		return nil, err
	}
	return &OpenEmbedSessionResult{Installation: cloneEmbedInstallation(installation), Session: cloneEmbedSession(session), Capability: token}, nil
}

func (s *EmbedSessionService) Authenticate(ctx context.Context, route, sessionID, capabilityToken, origin string) (*EmbedInstallation, *EmbedSession, error) {
	installation, err := s.store.GetEmbedInstallationByRoute(ctx, strings.TrimSpace(route))
	if err != nil {
		return nil, nil, err
	}
	if installation.Status != EmbedInstallationActive {
		return nil, nil, ErrEmbedSessionUnauthorized
	}
	normalizedOrigin, err := authorizeEmbedOrigin(installation, origin)
	if err != nil {
		return nil, nil, err
	}
	session, err := s.store.GetEmbedSession(ctx, installation.Scope, strings.TrimSpace(sessionID))
	if err != nil {
		return nil, nil, err
	}
	now := s.now().UTC()
	if session.InstallationID != installation.ID || session.Status != EmbedSessionActive || !session.ExpiresAt.After(now) || session.Origin != normalizedOrigin ||
		subtle.ConstantTimeCompare([]byte(session.CapabilityHash), []byte(hashEmbedCapability(capabilityToken))) != 1 {
		return nil, nil, ErrEmbedSessionUnauthorized
	}
	return installation, session, nil
}

func (s *EmbedSessionService) ConsumeMessage(ctx context.Context, installation *EmbedInstallation, session *EmbedSession) (*EmbedSession, error) {
	if installation == nil || session == nil {
		return nil, ErrEmbedSessionUnauthorized
	}
	return s.store.ConsumeEmbedSessionMessage(ctx, session.Scope, session.ID, installation.Permissions.MaximumMessages, s.now().UTC())
}

func (s *EmbedSessionService) ResolveConversationSessionContext(ctx context.Context, scope Scope, conversationID string) (map[string]interface{}, error) {
	session, err := s.store.GetEmbedSessionByConversation(ctx, scope, strings.TrimSpace(conversationID))
	if errors.Is(err, ErrEmbedSessionNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	installation, err := s.store.GetEmbedInstallation(ctx, scope, session.InstallationID)
	if err != nil {
		return nil, err
	}
	grants := make([]interface{}, 0, len(installation.Permissions.ActionGrants))
	for _, grant := range installation.Permissions.ActionGrants {
		grants = append(grants, map[string]interface{}{"bindingId": grant.BindingID, "action": grant.Action})
	}
	return map[string]interface{}{
		sessionContextIDKey:             session.ID,
		sessionContextVerifiedClaimsKey: cloneMap(session.VerifiedClaims),
		"authority":                     map[string]interface{}{"actionGrants": grants, "maximumActionRisk": string(installation.Permissions.MaximumActionRisk)},
	}, nil
}

func validateEmbedOrigins(values []string) error {
	if len(values) == 0 || len(values) > 100 {
		return errors.New("embed installation requires between 1 and 100 allowed origins")
	}
	seen := map[string]bool{}
	for _, value := range values {
		normalized, err := normalizeEmbedOrigin(value)
		if err != nil || seen[normalized] {
			return errors.New("embed allowed origins must be unique exact HTTP(S) origins")
		}
		seen[normalized] = true
	}
	return nil
}

func normalizeEmbedOrigins(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		normalized, err := normalizeEmbedOrigin(value)
		if err == nil {
			result = append(result, normalized)
		} else {
			result = append(result, strings.TrimSpace(value))
		}
	}
	sort.Strings(result)
	return result
}

func normalizeEmbedOrigin(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", errors.New("embed origin must be an exact HTTP(S) origin")
	}
	parsed.Path = ""
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func authorizeEmbedOrigin(installation *EmbedInstallation, origin string) (string, error) {
	normalized, err := normalizeEmbedOrigin(origin)
	if err != nil {
		return "", ErrEmbedOriginDenied
	}
	for _, allowed := range installation.AllowedOrigins {
		if normalized == allowed {
			return normalized, nil
		}
	}
	return "", ErrEmbedOriginDenied
}

func randomEmbedToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func hashEmbedCapability(value string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(digest[:])
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

func cloneEmbedInstallation(value *EmbedInstallation) *EmbedInstallation {
	if value == nil {
		return nil
	}
	copy := *value
	copy.AllowedOrigins = append([]string(nil), value.AllowedOrigins...)
	copy.Identity.RequiredClaims = append([]string(nil), value.Identity.RequiredClaims...)
	copy.Permissions.ActionGrants = append([]EmbedActionGrant(nil), value.Permissions.ActionGrants...)
	return &copy
}

func cloneEmbedSession(value *EmbedSession) *EmbedSession {
	if value == nil {
		return nil
	}
	copy := *value
	copy.VerifiedClaims = cloneMap(value.VerifiedClaims)
	return &copy
}
