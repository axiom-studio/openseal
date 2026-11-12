package models

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

var (
	ErrSpaceNotFound         = errors.New("space not found")
	ErrInvalidSpace          = errors.New("invalid space")
	ErrDefaultSpaceProtected = errors.New("a default space cannot be deleted")
)

const maxSpaceNameRunes = 60

// SpaceLook names a space's theme. The looks themselves live in the client,
// so the runtime only checks the name is a short lowercase slug; a client
// that doesn't know a look shows its default.
type SpaceLook string

// DefaultSpaceLook is the look a host can give a space made without one.
const DefaultSpaceLook SpaceLook = "tidepool"

var spaceLookPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

func (l SpaceLook) Valid() bool { return spaceLookPattern.MatchString(string(l)) }

// SpaceAccess is who may see a Space and what is in it.
type SpaceAccess string

const (
	SpaceAccessOwner    SpaceAccess = "owner"    // only the person who owns it
	SpaceAccessInvited  SpaceAccess = "invited"  // the owner and the people added to it
	SpaceAccessEveryone SpaceAccess = "everyone" // every person in the tenant
)

func (a SpaceAccess) Valid() bool {
	return a == SpaceAccessOwner || a == SpaceAccessInvited || a == SpaceAccessEveryone
}

type Space struct {
	ID       string    `json:"id"`
	Scope    Scope     `json:"scope"`
	Name     string    `json:"name"`
	Look     SpaceLook `json:"look"`
	Position int       `json:"position"`
	// IsDefault marks the owner's default space in the scope: the first one
	// they created. The store sets it; an owner has at most one per scope.
	IsDefault bool        `json:"isDefault"`
	Access    SpaceAccess `json:"access"`
	// OwnerID is the principal id of the person who owns the space. A
	// single-user host may leave it empty; the empty owner is an owner like
	// any other.
	OwnerID   string    `json:"ownerId"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *Space) Validate() error {
	if s == nil {
		return fmt.Errorf("%w: space is required", ErrInvalidSpace)
	}
	if err := s.Scope.Validate(); err != nil {
		return err
	}
	if !IsValidOpaqueIdentifier(s.ID, 128) {
		return fmt.Errorf("%w: space id must be a portable opaque identifier", ErrInvalidSpace)
	}
	if err := ValidateSpaceName(s.Name); err != nil {
		return err
	}
	if !s.Look.Valid() {
		return fmt.Errorf("%w: invalid look %q", ErrInvalidSpace, s.Look)
	}
	if !s.Access.Valid() {
		return fmt.Errorf("%w: access must be owner, invited or everyone", ErrInvalidSpace)
	}
	if s.Position < 0 || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || len(s.OwnerID) > 128 {
		return fmt.Errorf("%w: position, owner and timestamps are invalid", ErrInvalidSpace)
	}
	return nil
}

func ValidateSpaceName(name string) error {
	if name != strings.TrimSpace(name) || name == "" || utf8.RuneCountInString(name) > maxSpaceNameRunes {
		return fmt.Errorf("%w: name must be 1-%d characters", ErrInvalidSpace, maxSpaceNameRunes)
	}
	return nil
}

type SpaceMemberType string

const (
	SpaceMemberAgent SpaceMemberType = "agent"
	SpaceMemberTeam  SpaceMemberType = "team"
)

func (t SpaceMemberType) Valid() bool { return t == SpaceMemberAgent || t == SpaceMemberTeam }

// SpaceMember places an Agent or Team deployment in a space. A deployment can
// be a member of several spaces.
type SpaceMember struct {
	SpaceID  string          `json:"spaceId"`
	Type     SpaceMemberType `json:"type"`
	ID       string          `json:"id"`
	Position int             `json:"position"`
	AddedAt  time.Time       `json:"addedAt"`
}

func (m *SpaceMember) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: member is required", ErrInvalidSpace)
	}
	if !m.Type.Valid() {
		return fmt.Errorf("%w: member type must be agent or team", ErrInvalidSpace)
	}
	if !IsValidOpaqueIdentifier(m.SpaceID, 128) || !IsValidOpaqueIdentifier(m.ID, 128) || m.Position < 0 || m.AddedAt.IsZero() {
		return fmt.Errorf("%w: member space, id, position and time are invalid", ErrInvalidSpace)
	}
	return nil
}

// SpacePerson is a person added to a space; with access "invited" they see
// it. Agents and Teams in a space are SpaceMembers.
type SpacePerson struct {
	SpaceID string    `json:"spaceId"`
	UserID  string    `json:"userId"`
	AddedBy string    `json:"addedBy"`
	AddedAt time.Time `json:"addedAt"`
}

func (p *SpacePerson) Validate() error {
	if p == nil {
		return fmt.Errorf("%w: person is required", ErrInvalidSpace)
	}
	if !IsValidOpaqueIdentifier(p.SpaceID, 128) || !IsValidOpaqueIdentifier(p.UserID, 128) || len(p.AddedBy) > 128 || p.AddedAt.IsZero() {
		return fmt.Errorf("%w: person space, user, adder and time are invalid", ErrInvalidSpace)
	}
	return nil
}

// SpacePatch carries the optional changes of an UpdateSpace call. Position
// moves the space to that index in the scope's ordering, renumbering the rest.
type SpacePatch struct {
	Name     *string
	Look     *SpaceLook
	Position *int
}

// SpaceStore persists spaces and their members. Stores also resolve a
// conversation's space on create when the caller leaves it empty: the owner
// deployment's first space by space position, else the default space of the
// person who created the conversation, else ErrNoDefaultSpace.
type SpaceStore interface {
	// CreateSpace stores the space at the end of the scope. In the same
	// transaction it becomes its owner's default space when that owner has
	// none in the scope; the caller's IsDefault is ignored.
	CreateSpace(ctx context.Context, space *Space) (*Space, error)
	// DefaultSpace returns the owner's default space in the scope, or
	// ErrNoDefaultSpace.
	DefaultSpace(ctx context.Context, scope Scope, ownerID string) (*Space, error)
	GetSpace(ctx context.Context, scope Scope, spaceID string) (*Space, error)
	ListSpaces(ctx context.Context, scope Scope) ([]*Space, error)
	UpdateSpace(ctx context.Context, scope Scope, spaceID string, patch SpacePatch, now time.Time) (*Space, error)
	// DeleteSpace moves the space's conversations, and the members that are
	// in no other space, to the default space of the deleted space's owner,
	// then drops the space with its members and people. A default space
	// returns ErrDefaultSpaceProtected; an owner without a default space
	// returns ErrNoDefaultSpace. Either way nothing changes.
	DeleteSpace(ctx context.Context, scope Scope, spaceID string, now time.Time) error
	ListSpaceMembers(ctx context.Context, scope Scope, spaceID string) ([]*SpaceMember, error)
	// PutSpaceMember is idempotent: an existing member is returned unchanged.
	// The same write adds each of with that is not in the space yet.
	PutSpaceMember(ctx context.Context, scope Scope, member *SpaceMember, with ...*SpaceMember) (*SpaceMember, bool, error)
	DeleteSpaceMember(ctx context.Context, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error
	// EnsureMemberHasSpace adds a deployment that is in no space to the
	// owner's default space. It returns the new membership, nil when the
	// deployment already had a space, or ErrNoDefaultSpace when the owner
	// has no default space.
	EnsureMemberHasSpace(ctx context.Context, scope Scope, ownerID string, memberType SpaceMemberType, memberID string, now time.Time) (*SpaceMember, error)
	// ListScopeSpaceMembers returns every space membership of the scope.
	ListScopeSpaceMembers(ctx context.Context, scope Scope) ([]*SpaceMember, error)
	SetSpaceAccess(ctx context.Context, scope Scope, spaceID string, access SpaceAccess, now time.Time) (*Space, error)
	// SetSpaceOwner hands the space to ownerID. The space stops being a
	// default and a new owner who has a default keeps it; in the same
	// transaction an owner of the two left without one gets their oldest
	// space not shared with everyone (else their oldest) as default.
	SetSpaceOwner(ctx context.Context, scope Scope, spaceID, ownerID string, now time.Time) (*Space, error)
	// SetSpaceAccessState writes the space's whole access in one
	// transaction. An owner change is SetSpaceOwner's, defaults included.
	SetSpaceAccessState(ctx context.Context, scope Scope, spaceID string, state SpaceAccessState, now time.Time) (*Space, error)
	// PutSpacePerson is idempotent: an existing person is returned unchanged.
	PutSpacePerson(ctx context.Context, scope Scope, person *SpacePerson) (*SpacePerson, bool, error)
	DeleteSpacePerson(ctx context.Context, scope Scope, spaceID, userID string) error
	ListSpacePeople(ctx context.Context, scope Scope, spaceID string) ([]*SpacePerson, error)
	// VisibleSpaces returns, by position, the ids of the spaces the person
	// may see: the ones they own, the ones with access "everyone", and the
	// "invited" ones they were added to. An empty userID owns nothing.
	VisibleSpaces(ctx context.Context, scope Scope, userID string) ([]string, error)
}

// SpaceRosterStore is a store holding both spaces and Teams. It takes an Agent
// out of a space and stores the roster changes of that space's Teams in one
// transaction.
type SpaceRosterStore interface {
	kernelteam.Store
	DeleteSpaceAgentWithRosters(ctx context.Context, scope Scope, spaceID, agentID string, changes []*kernelteam.RosterChange) error
}

// SpaceAccessState is the whole of who may see a space. An empty OwnerID
// leaves the owner as it is. People becomes the space's whole list of
// people: those already on it keep when and by whom they were added.
type SpaceAccessState struct {
	OwnerID string
	Access  SpaceAccess
	People  []*SpacePerson
}

func CloneSpace(space *Space) *Space {
	if space == nil {
		return nil
	}
	cloned := *space
	return &cloned
}

func CloneSpaceMember(member *SpaceMember) *SpaceMember {
	if member == nil {
		return nil
	}
	cloned := *member
	return &cloned
}

// SpacePositionsAfterMove returns the new position of every space after moving spaceID to
// index position (clamped) within the ordered list.
func SpacePositionsAfterMove(ordered []*Space, spaceID string, position int) map[string]int {
	rest := make([]*Space, 0, len(ordered))
	var moved *Space
	for _, space := range ordered {
		if space.ID == spaceID {
			moved = space
			continue
		}
		rest = append(rest, space)
	}
	if moved == nil {
		return nil
	}
	if position > len(rest) {
		position = len(rest)
	}
	result := append(append(append(make([]*Space, 0, len(ordered)), rest[:position]...), moved), rest[position:]...)
	positions := make(map[string]int, len(result))
	for index, space := range result {
		positions[space.ID] = index
	}
	return positions
}

// ApplySpacePatch applies name and look; position is handled by SpacePositionsAfterMove.
func ApplySpacePatch(space *Space, patch SpacePatch, now time.Time) {
	if patch.Name != nil {
		space.Name = *patch.Name
	}
	if patch.Look != nil {
		space.Look = *patch.Look
	}
	space.UpdatedAt = now
}

var (
	// ErrNoDefaultSpace reports that the owner has no default space in the
	// scope: they have not created a space there yet.
	ErrNoDefaultSpace = errors.New("this owner has no default space")
)
