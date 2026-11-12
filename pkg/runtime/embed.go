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
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/google/uuid"
)

var (
	ErrEmbedOriginDenied   = errors.New("embed origin is not allowed")
	ErrEmbedAuthentication = errors.New("embed host authentication is required")
)

type CreateEmbedInstallationRequest struct {
	ID             string
	Scope          Scope
	DeploymentID   string
	Name           string
	AllowedOrigins []string
	Identity       EmbedIdentityPolicy
	Permissions    EmbedPermissionPolicy
	// CreatedBy is the person creating the installation.
	CreatedBy string
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

func NewEmbedSessionService(store EmbedStore, conversations ConversationStore, opts ...models.ServiceOption) (*EmbedSessionService, error) {
	if store == nil || conversations == nil {
		return nil, errors.New("embed and conversation stores are required")
	}
	return &EmbedSessionService{store: store, conversations: NewConversationService(conversations, opts...), now: models.ClockFrom(opts), newID: uuid.NewString, randomToken: randomEmbedToken}, nil
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
		Identity: req.Identity, Permissions: req.Permissions, CreatedBy: strings.TrimSpace(req.CreatedBy), Revision: 1, CreatedAt: now, UpdatedAt: now,
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
	claims := maps.Clone(req.VerifiedClaims)
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
		CreatedBy:      installation.CreatedBy,
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
		skillbinding.SessionContextIDKey:             session.ID,
		skillbinding.SessionContextVerifiedClaimsKey: maps.Clone(session.VerifiedClaims),
		"authority": map[string]interface{}{"actionGrants": grants, "maximumActionRisk": string(installation.Permissions.MaximumActionRisk)},
	}, nil
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
