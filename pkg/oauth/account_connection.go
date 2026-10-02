package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type accountConnectionLister interface {
	ListConnections(context.Context, capability.ScopeReference) ([]*Connection, error)
}

// SameAccountConnection compares provider-verified identity, never display names
// or email strings. Owners and resources remain separate authorization domains.
func SameAccountConnection(a, b *Connection) bool {
	return a != nil && b != nil && a.ExternalIdentity.AccountID != "" &&
		a.Grant.Subject == capability.OAuth2SubjectUser && b.Grant.Subject == capability.OAuth2SubjectUser &&
		a.Scope == b.Scope && a.Owner == b.Owner && a.Kind == b.Kind &&
		a.Grant.Provider == b.Grant.Provider && a.Grant.Resource == b.Grant.Resource &&
		a.ExternalIdentity.AccountID == b.ExternalIdentity.AccountID
}

func (s *Service) authorizationAccountConnection(ctx context.Context, session *AuthorizationSession, grant capability.OAuth2GrantSummary, identity ExternalIdentity) (string, *Connection, error) {
	candidate := &Connection{Scope: session.Scope, Owner: session.Owner, Kind: session.Kind, Grant: grant, ExternalIdentity: identity}
	current, err := s.store.GetConnection(ctx, session.Scope, session.ConnectionID)
	if err != nil && !errors.Is(err, ErrConnectionNotFound) {
		return "", nil, err
	}
	if current != nil {
		if current.Owner != session.Owner || current.Kind != session.Kind || current.Grant.Provider != grant.Provider || current.Grant.Subject != grant.Subject ||
			current.Grant.Resource != grant.Resource || (current.ExternalIdentity.AccountID != "" && current.ExternalIdentity.AccountID != identity.AccountID) {
			return "", nil, errors.New("OAuth reconfiguration must use the same verified account and owner")
		}
		return current.ID, current, nil
	}
	if grant.Subject != capability.OAuth2SubjectUser || identity.AccountID == "" {
		return session.ConnectionID, nil, nil
	}
	if lister, ok := s.store.(accountConnectionLister); ok {
		connections, listErr := lister.ListConnections(ctx, session.Scope)
		if listErr != nil {
			return "", nil, listErr
		}
		// Reuse an existing active account before a revoked historical account,
		// then use creation order for a stable choice among legacy duplicates.
		sort.SliceStable(connections, func(i, j int) bool {
			a, b := connections[i], connections[j]
			if a == nil || b == nil {
				return b == nil && a != nil
			}
			if (a.Status == ConnectionActive) != (b.Status == ConnectionActive) {
				return a.Status == ConnectionActive
			}
			if a.CreatedAt.Equal(b.CreatedAt) {
				return a.ID < b.ID
			}
			return a.CreatedAt.Before(b.CreatedAt)
		})
		for _, connection := range connections {
			if SameAccountConnection(candidate, connection) {
				return connection.ID, connection, nil
			}
		}
	}
	// Concurrent first authorizations converge on one row. Include every
	// authorization domain in the hash and exclude mutable scopes/display names.
	key, _ := json.Marshal([]string{session.Scope.Kind, session.Scope.ID, string(session.Owner.Kind), session.Owner.ID,
		session.Kind, grant.Provider, string(grant.Subject), grant.Resource, identity.AccountID})
	digest := sha256.Sum256(key)
	id := grant.Provider + "-account-" + hex.EncodeToString(digest[:])
	current, err = s.store.GetConnection(ctx, session.Scope, id)
	if errors.Is(err, ErrConnectionNotFound) {
		return id, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if !SameAccountConnection(candidate, current) {
		return "", nil, ErrRevisionConflict
	}
	return id, current, nil
}
