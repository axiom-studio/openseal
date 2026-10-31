package runtime

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// A Space groups a scope's agents, teams and conversations under a name and a
// look. Every scope has exactly one Personal space; it is created on demand,
// cannot be deleted, and receives the conversations of deleted spaces.
var (
	ErrSpaceNotFound          = errors.New("space not found")
	ErrInvalidSpace           = errors.New("invalid space")
	ErrPersonalSpaceProtected = errors.New("the Personal space cannot be deleted")
)

const (
	PersonalSpaceName = "Personal"
	maxSpaceNameRunes = 60
)

type SpaceLook string

const (
	SpaceLookTidepool  SpaceLook = "tidepool"
	SpaceLookHarbor    SpaceLook = "harbor"
	SpaceLookGraphite  SpaceLook = "graphite"
	SpaceLookChalk     SpaceLook = "chalk"
	SpaceLookCritter   SpaceLook = "critter"
	SpaceLookBitcraft  SpaceLook = "bitcraft"
	SpaceLookAfterglow SpaceLook = "afterglow"

	PersonalSpaceLook = SpaceLookTidepool
)

func (l SpaceLook) Valid() bool {
	switch l {
	case SpaceLookTidepool, SpaceLookHarbor, SpaceLookGraphite, SpaceLookChalk, SpaceLookCritter, SpaceLookBitcraft, SpaceLookAfterglow:
		return true
	}
	return false
}

type Space struct {
	ID         string    `json:"id"`
	Scope      Scope     `json:"scope"`
	Name       string    `json:"name"`
	Look       SpaceLook `json:"look"`
	Position   int       `json:"position"`
	IsPersonal bool      `json:"isPersonal"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func (s *Space) Validate() error {
	if s == nil {
		return fmt.Errorf("%w: space is required", ErrInvalidSpace)
	}
	if err := s.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(s.ID, 128) {
		return fmt.Errorf("%w: space id must be a portable opaque identifier", ErrInvalidSpace)
	}
	if err := validateSpaceName(s.Name); err != nil {
		return err
	}
	if !s.Look.Valid() {
		return fmt.Errorf("%w: invalid look %q", ErrInvalidSpace, s.Look)
	}
	if s.Position < 0 || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || len(s.CreatedBy) > 128 {
		return fmt.Errorf("%w: position, creator and timestamps are invalid", ErrInvalidSpace)
	}
	return nil
}

func validateSpaceName(name string) error {
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
	if !validOpaqueIdentifier(m.SpaceID, 128) || !validOpaqueIdentifier(m.ID, 128) || m.Position < 0 || m.AddedAt.IsZero() {
		return fmt.Errorf("%w: member space, id, position and time are invalid", ErrInvalidSpace)
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
// conversation's space on create when the caller leaves it empty: the owner's
// first space by space position, else the scope's Personal space.
type SpaceStore interface {
	EnsurePersonalSpace(ctx context.Context, scope Scope, now time.Time) (*Space, bool, error)
	CreateSpace(ctx context.Context, space *Space) (*Space, error)
	GetSpace(ctx context.Context, scope Scope, spaceID string) (*Space, error)
	ListSpaces(ctx context.Context, scope Scope) ([]*Space, error)
	UpdateSpace(ctx context.Context, scope Scope, spaceID string, patch SpacePatch, now time.Time) (*Space, error)
	// DeleteSpace moves the space's conversations to Personal, drops its
	// members, and adds any deployment left in no space to Personal.
	DeleteSpace(ctx context.Context, scope Scope, spaceID string, now time.Time) error
	ListSpaceMembers(ctx context.Context, scope Scope, spaceID string) ([]*SpaceMember, error)
	// PutSpaceMember is idempotent: an existing member is returned unchanged.
	PutSpaceMember(ctx context.Context, scope Scope, member *SpaceMember) (*SpaceMember, bool, error)
	DeleteSpaceMember(ctx context.Context, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error
}

type CreateSpaceRequest struct {
	Scope     Scope
	Name      string
	Look      SpaceLook
	CreatedBy string
}

type UpdateSpaceRequest struct {
	Scope    Scope
	SpaceID  string
	Name     *string
	Look     *SpaceLook
	Position *int
}

type PutSpaceMemberRequest struct {
	Scope    Scope
	SpaceID  string
	Type     SpaceMemberType
	MemberID string
}

type SpaceService struct {
	store SpaceStore
	now   func() time.Time
	newID func() string
}

func NewSpaceService(store SpaceStore) *SpaceService {
	return &SpaceService{store: store, now: time.Now, newID: uuid.NewString}
}

// ListSpaces ensures the scope's Personal space and returns every space by
// position.
func (s *SpaceService) ListSpaces(ctx context.Context, scope Scope) ([]*Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if _, _, err := s.store.EnsurePersonalSpace(ctx, scope, s.now().UTC()); err != nil {
		return nil, err
	}
	return s.store.ListSpaces(ctx, scope)
}

// EnsurePersonalSpace returns the scope's Personal space and whether this call
// created it.
func (s *SpaceService) EnsurePersonalSpace(ctx context.Context, scope Scope) (*Space, bool, error) {
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.EnsurePersonalSpace(ctx, scope, s.now().UTC())
}

func (s *SpaceService) GetSpace(ctx context.Context, scope Scope, spaceID string) (*Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.GetSpace(ctx, scope, strings.TrimSpace(spaceID))
}

func (s *SpaceService) CreateSpace(ctx context.Context, req CreateSpaceRequest) (*Space, error) {
	now := s.now().UTC()
	space := &Space{
		ID: s.newID(), Scope: req.Scope, Name: strings.TrimSpace(req.Name), Look: req.Look,
		CreatedBy: strings.TrimSpace(req.CreatedBy), CreatedAt: now, UpdatedAt: now,
	}
	if err := space.Validate(); err != nil {
		return nil, err
	}
	if _, _, err := s.store.EnsurePersonalSpace(ctx, req.Scope, now); err != nil {
		return nil, err
	}
	return s.store.CreateSpace(ctx, space)
}

func (s *SpaceService) UpdateSpace(ctx context.Context, req UpdateSpaceRequest) (*Space, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	patch := SpacePatch{Look: req.Look, Position: req.Position}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if err := validateSpaceName(name); err != nil {
			return nil, err
		}
		patch.Name = &name
	}
	if req.Look != nil && !req.Look.Valid() {
		return nil, fmt.Errorf("%w: invalid look %q", ErrInvalidSpace, *req.Look)
	}
	if req.Position != nil && *req.Position < 0 {
		return nil, fmt.Errorf("%w: position cannot be negative", ErrInvalidSpace)
	}
	return s.store.UpdateSpace(ctx, req.Scope, strings.TrimSpace(req.SpaceID), patch, s.now().UTC())
}

func (s *SpaceService) DeleteSpace(ctx context.Context, scope Scope, spaceID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return s.store.DeleteSpace(ctx, scope, strings.TrimSpace(spaceID), s.now().UTC())
}

func (s *SpaceService) ListSpaceMembers(ctx context.Context, scope Scope, spaceID string) ([]*SpaceMember, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListSpaceMembers(ctx, scope, strings.TrimSpace(spaceID))
}

// PutSpaceMember adds the member at the end of the space; the host must have
// verified the deployment belongs to the scope.
func (s *SpaceService) PutSpaceMember(ctx context.Context, req PutSpaceMemberRequest) (*SpaceMember, bool, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, false, err
	}
	member := &SpaceMember{SpaceID: strings.TrimSpace(req.SpaceID), Type: req.Type, ID: strings.TrimSpace(req.MemberID), AddedAt: s.now().UTC()}
	if err := member.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.PutSpaceMember(ctx, req.Scope, member)
}

func (s *SpaceService) DeleteSpaceMember(ctx context.Context, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !memberType.Valid() {
		return fmt.Errorf("%w: member type must be agent or team", ErrInvalidSpace)
	}
	return s.store.DeleteSpaceMember(ctx, scope, strings.TrimSpace(spaceID), memberType, strings.TrimSpace(memberID))
}

// PersonalSpaceID is the deterministic id of a scope's Personal space. The
// PostgreSQL backfill computes the same value with md5().
func PersonalSpaceID(scope Scope) string {
	digest := md5.Sum([]byte(scope.Kind + ":" + scope.ID))
	return "personal-" + hex.EncodeToString(digest[:])
}

func newPersonalSpace(scope Scope, now time.Time) *Space {
	return &Space{
		ID: PersonalSpaceID(scope), Scope: scope, Name: PersonalSpaceName, Look: PersonalSpaceLook,
		Position: 0, IsPersonal: true, CreatedBy: "system", CreatedAt: now, UpdatedAt: now,
	}
}

func cloneSpace(space *Space) *Space {
	if space == nil {
		return nil
	}
	cloned := *space
	return &cloned
}

func cloneSpaceMember(member *SpaceMember) *SpaceMember {
	if member == nil {
		return nil
	}
	cloned := *member
	return &cloned
}

func sortSpaces(spaces []*Space) {
	sort.SliceStable(spaces, func(i, j int) bool {
		if spaces[i].Position != spaces[j].Position {
			return spaces[i].Position < spaces[j].Position
		}
		if !spaces[i].CreatedAt.Equal(spaces[j].CreatedAt) {
			return spaces[i].CreatedAt.Before(spaces[j].CreatedAt)
		}
		return spaces[i].ID < spaces[j].ID
	})
}

// moveSpace returns the new position of every space after moving spaceID to
// index position (clamped) within the ordered list.
func moveSpace(ordered []*Space, spaceID string, position int) map[string]int {
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

// applySpacePatch applies name and look; position is handled by moveSpace.
func applySpacePatch(space *Space, patch SpacePatch, now time.Time) {
	if patch.Name != nil {
		space.Name = *patch.Name
	}
	if patch.Look != nil {
		space.Look = *patch.Look
	}
	space.UpdatedAt = now
}

func sortSpaceMembers(members []*SpaceMember) {
	sort.SliceStable(members, func(i, j int) bool {
		if members[i].Position != members[j].Position {
			return members[i].Position < members[j].Position
		}
		if !members[i].AddedAt.Equal(members[j].AddedAt) {
			return members[i].AddedAt.Before(members[j].AddedAt)
		}
		if members[i].Type != members[j].Type {
			return members[i].Type < members[j].Type
		}
		return members[i].ID < members[j].ID
	})
}
