package runtime

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/google/uuid"
)

// A Space groups a scope's agents, teams and conversations under a name and a
// look. Every scope has exactly one Default space; it is created on demand,
// cannot be deleted, and receives the conversations of deleted spaces.
var (
	ErrSpaceNotFound         = errors.New("space not found")
	ErrInvalidSpace          = errors.New("invalid space")
	ErrDefaultSpaceProtected = errors.New("the Default space cannot be deleted")
	// ErrTeamAgentOutsideSpace refuses a Team roster Agent that is not in
	// every space the Team is in.
	ErrTeamAgentOutsideSpace = errors.New("a team's agents must be in its space")
	// ErrSpaceAgentNeededByTeam refuses taking an Agent out of a space when a
	// Team in that space cannot lose it.
	ErrSpaceAgentNeededByTeam = errors.New("a team in this space needs the agent")
)

const (
	DefaultSpaceName  = "Default"
	maxSpaceNameRunes = 60
)

// SpaceLook names a space's theme. The looks themselves live in the client,
// so the runtime only checks the name is a short lowercase slug; a client
// that doesn't know a look shows its default.
type SpaceLook string

// DefaultSpaceLook is the look the Default space starts with.
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

// DefaultSpaceOwner owns every scope's Default space. It is no person, so
// nobody edits the Default space as its owner.
const DefaultSpaceOwner = "system"

type Space struct {
	ID        string      `json:"id"`
	Scope     Scope       `json:"scope"`
	Name      string      `json:"name"`
	Look      SpaceLook   `json:"look"`
	Position  int         `json:"position"`
	IsDefault bool        `json:"isDefault"`
	Access    SpaceAccess `json:"access"`
	// OwnerID is the principal id of the person who owns the space.
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
	if !validOpaqueIdentifier(s.ID, 128) {
		return fmt.Errorf("%w: space id must be a portable opaque identifier", ErrInvalidSpace)
	}
	if err := validateSpaceName(s.Name); err != nil {
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
	if !validOpaqueIdentifier(p.SpaceID, 128) || !validOpaqueIdentifier(p.UserID, 128) || len(p.AddedBy) > 128 || p.AddedAt.IsZero() {
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
// conversation's space on create when the caller leaves it empty: the owner's
// first space by space position, else the scope's Default space.
type SpaceStore interface {
	EnsureDefaultSpace(ctx context.Context, scope Scope, now time.Time) (*Space, bool, error)
	CreateSpace(ctx context.Context, space *Space) (*Space, error)
	GetSpace(ctx context.Context, scope Scope, spaceID string) (*Space, error)
	ListSpaces(ctx context.Context, scope Scope) ([]*Space, error)
	UpdateSpace(ctx context.Context, scope Scope, spaceID string, patch SpacePatch, now time.Time) (*Space, error)
	// DeleteSpace moves the space's conversations to Default, drops its
	// members and people, and adds any deployment left in no space to Default.
	DeleteSpace(ctx context.Context, scope Scope, spaceID string, now time.Time) error
	ListSpaceMembers(ctx context.Context, scope Scope, spaceID string) ([]*SpaceMember, error)
	// PutSpaceMember is idempotent: an existing member is returned unchanged.
	// The same write adds each of with that is not in the space yet.
	PutSpaceMember(ctx context.Context, scope Scope, member *SpaceMember, with ...*SpaceMember) (*SpaceMember, bool, error)
	DeleteSpaceMember(ctx context.Context, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error
	// EnsureMemberHasSpace adds a deployment that is in no space to the
	// scope's Default space, creating Default when needed. It returns the
	// new membership, or nil when the deployment already had a space.
	EnsureMemberHasSpace(ctx context.Context, scope Scope, memberType SpaceMemberType, memberID string, now time.Time) (*SpaceMember, error)
	// ListScopeSpaceMembers returns every space membership of the scope.
	ListScopeSpaceMembers(ctx context.Context, scope Scope) ([]*SpaceMember, error)
	SetSpaceAccess(ctx context.Context, scope Scope, spaceID string, access SpaceAccess, now time.Time) (*Space, error)
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

type CreateSpaceRequest struct {
	Scope   Scope
	Name    string
	Look    SpaceLook
	OwnerID string
}

type AddSpacePersonRequest struct {
	Scope   Scope
	SpaceID string
	UserID  string
	AddedBy string
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

type DeleteSpaceMemberRequest struct {
	Scope    Scope
	SpaceID  string
	Type     SpaceMemberType
	MemberID string
	// ActorType and ActorID sign the Team roster updates that removing an
	// Agent makes.
	ActorType string
	ActorID   string
}

type SpaceService struct {
	store   SpaceStore
	teams   *kernelteam.Registry
	rosters SpaceRosterStore
	now     func() time.Time
	newID   func() string
}

func NewSpaceService(store SpaceStore) *SpaceService {
	return &SpaceService{store: store, now: time.Now, newID: uuid.NewString}
}

// UseTeams ties Team rosters to spaces: a Team's Agents are in every space
// the Team is in. A Team added to a space brings its Agents, and an Agent
// that leaves a space leaves that space's Teams. The registry must write to
// this service's store, so both changes commit together.
func (s *SpaceService) UseTeams(teams *kernelteam.Registry) error {
	store, ok := s.store.(SpaceRosterStore)
	if !ok || !teams.UsesStore(store) {
		return errors.New("spaces and Teams must share one store")
	}
	s.teams, s.rosters = teams, store
	return nil
}

// ListSpaces ensures the scope's Default space and returns every space by
// position.
func (s *SpaceService) ListSpaces(ctx context.Context, scope Scope) ([]*Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if _, _, err := s.store.EnsureDefaultSpace(ctx, scope, s.now().UTC()); err != nil {
		return nil, err
	}
	return s.store.ListSpaces(ctx, scope)
}

// EnsureDefaultSpace returns the scope's Default space and whether this call
// created it.
func (s *SpaceService) EnsureDefaultSpace(ctx context.Context, scope Scope) (*Space, bool, error) {
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.EnsureDefaultSpace(ctx, scope, s.now().UTC())
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
		Access: SpaceAccessOwner, OwnerID: strings.TrimSpace(req.OwnerID), CreatedAt: now, UpdatedAt: now,
	}
	if err := space.Validate(); err != nil {
		return nil, err
	}
	if _, _, err := s.store.EnsureDefaultSpace(ctx, req.Scope, now); err != nil {
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
	spaceID = strings.TrimSpace(spaceID)
	var teams []string
	if s.teams != nil {
		members, err := s.store.ListSpaceMembers(ctx, scope, spaceID)
		if err != nil {
			return err
		}
		for _, member := range members {
			if member.Type == SpaceMemberTeam {
				teams = append(teams, member.ID)
			}
		}
	}
	if err := s.store.DeleteSpace(ctx, scope, spaceID, s.now().UTC()); err != nil || len(teams) == 0 {
		return err
	}
	// A Team that fell back to Default brings its Agents there, like a Team
	// added to any space.
	defaultID := DefaultSpaceID(scope)
	members, err := s.store.ListSpaceMembers(ctx, scope, defaultID)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.Type != SpaceMemberTeam || !slices.Contains(teams, member.ID) {
			continue
		}
		if _, _, err := s.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: defaultID, Type: SpaceMemberTeam, MemberID: member.ID}); err != nil {
			return err
		}
	}
	return nil
}

func (s *SpaceService) ListSpaceMembers(ctx context.Context, scope Scope, spaceID string) ([]*SpaceMember, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListSpaceMembers(ctx, scope, strings.TrimSpace(spaceID))
}

// PutSpaceMember adds the member at the end of the space; the host must have
// verified the deployment belongs to the scope. A Team brings the Agents on
// its roster.
func (s *SpaceService) PutSpaceMember(ctx context.Context, req PutSpaceMemberRequest) (*SpaceMember, bool, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, false, err
	}
	now := s.now().UTC()
	member := &SpaceMember{SpaceID: strings.TrimSpace(req.SpaceID), Type: req.Type, ID: strings.TrimSpace(req.MemberID), AddedAt: now}
	if err := member.Validate(); err != nil {
		return nil, false, err
	}
	var agents []*SpaceMember
	if member.Type == SpaceMemberTeam && s.teams != nil {
		team, err := s.teams.GetDeployment(ctx, agentBuilderScope(req.Scope), member.ID)
		if err != nil && !errors.Is(err, kernelteam.ErrDeploymentNotFound) {
			return nil, false, err
		}
		if team != nil {
			for _, assignment := range team.Roster {
				agent := &SpaceMember{SpaceID: member.SpaceID, Type: SpaceMemberAgent, ID: assignment.AgentDeploymentID, AddedAt: now}
				// A legacy scoped id ("tenant/7/x:live") cannot be a space member.
				if agent.Validate() == nil {
					agents = append(agents, agent)
				}
			}
		}
	}
	return s.store.PutSpaceMember(ctx, req.Scope, member, agents...)
}

// DeleteSpaceMember takes the member out of the space. An Agent also leaves
// the rosters of the Teams in that space: the Team registry prepares each
// roster update and the store commits them with the removal. When a Team
// cannot lose the Agent, nothing changes and the error names the Team.
func (s *SpaceService) DeleteSpaceMember(ctx context.Context, req DeleteSpaceMemberRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return err
	}
	if !req.Type.Valid() {
		return fmt.Errorf("%w: member type must be agent or team", ErrInvalidSpace)
	}
	spaceID, memberID := strings.TrimSpace(req.SpaceID), strings.TrimSpace(req.MemberID)
	var changes []*kernelteam.RosterChange
	if req.Type == SpaceMemberAgent && s.teams != nil {
		var err error
		if changes, err = s.agentRemovalRosterChanges(ctx, req.Scope, spaceID, memberID, req.ActorType, req.ActorID); err != nil {
			return err
		}
	}
	if len(changes) == 0 {
		return s.store.DeleteSpaceMember(ctx, req.Scope, spaceID, req.Type, memberID)
	}
	return s.rosters.DeleteSpaceAgentWithRosters(ctx, req.Scope, spaceID, memberID, changes)
}

// agentRemovalRosterChanges prepares the roster update of every Team in the
// space that lists the Agent. An Agent that is not in the space changes none.
func (s *SpaceService) agentRemovalRosterChanges(ctx context.Context, scope Scope, spaceID, agentID, actorType, actorID string) ([]*kernelteam.RosterChange, error) {
	members, err := s.store.ListSpaceMembers(ctx, scope, spaceID)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(members, func(member *SpaceMember) bool { return member.Type == SpaceMemberAgent && member.ID == agentID }) {
		return nil, nil
	}
	space, err := s.store.GetSpace(ctx, scope, spaceID)
	if err != nil {
		return nil, err
	}
	var changes []*kernelteam.RosterChange
	for _, member := range members {
		if member.Type != SpaceMemberTeam {
			continue
		}
		change, err := s.teams.PrepareAgentRemoval(ctx, agentBuilderScope(scope), member.ID, agentID, actorType, actorID,
			fmt.Sprintf("Agent %s left the space %s", agentID, space.Name))
		switch {
		case errors.Is(err, kernelteam.ErrRosterMinimum):
			return nil, fmt.Errorf("%w: removing agent %q from space %q would leave team %s invalid: %v",
				ErrSpaceAgentNeededByTeam, agentID, space.Name, s.teamLabel(ctx, scope, member.ID), err)
		case errors.Is(err, kernelteam.ErrDeploymentNotFound):
		case err != nil:
			return nil, err
		case change != nil:
			changes = append(changes, change)
		}
	}
	return changes, nil
}

// teamLabel is `"name" (id)` for an error message, or the quoted id alone.
func (s *SpaceService) teamLabel(ctx context.Context, scope Scope, teamID string) string {
	if team, err := s.teams.GetDeployment(ctx, agentBuilderScope(scope), teamID); err == nil {
		if definition, err := s.teams.GetDefinition(ctx, team.DefinitionID, team.ActiveVersion); err == nil {
			return fmt.Sprintf("%q (%s)", definition.DisplayName, teamID)
		}
	}
	return fmt.Sprintf("%q", teamID)
}

// CheckSpaceAgents refuses Agents that are not members of the space.
func (s *SpaceService) CheckSpaceAgents(ctx context.Context, scope Scope, spaceID string, agentIDs []string) error {
	space, err := s.store.GetSpace(ctx, scope, strings.TrimSpace(spaceID))
	if err != nil {
		return err
	}
	members, err := s.store.ListSpaceMembers(ctx, scope, space.ID)
	if err != nil {
		return err
	}
	return agentsInSpace(space, members, agentIDs)
}

// CheckTeamAgents refuses Agents that are not in every space the Team is in.
func (s *SpaceService) CheckTeamAgents(ctx context.Context, scope Scope, teamID string, agentIDs []string) error {
	if len(agentIDs) == 0 {
		return nil
	}
	spaces, err := s.store.ListSpaces(ctx, scope)
	if err != nil {
		return err
	}
	for _, space := range spaces {
		members, err := s.store.ListSpaceMembers(ctx, scope, space.ID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(members, func(member *SpaceMember) bool { return member.Type == SpaceMemberTeam && member.ID == teamID }) {
			continue
		}
		if err := agentsInSpace(space, members, agentIDs); err != nil {
			return err
		}
	}
	return nil
}

func agentsInSpace(space *Space, members []*SpaceMember, agentIDs []string) error {
	for _, agentID := range agentIDs {
		if !slices.ContainsFunc(members, func(member *SpaceMember) bool { return member.Type == SpaceMemberAgent && member.ID == agentID }) {
			return fmt.Errorf("%w: agent %q is not in space %q", ErrTeamAgentOutsideSpace, agentID, space.Name)
		}
	}
	return nil
}

// EnsureMemberHasSpace places a newly created Agent or Team deployment in
// Default unless it already belongs to a space, so no deployment is left in
// no space.
func (s *SpaceService) EnsureMemberHasSpace(ctx context.Context, scope Scope, memberType SpaceMemberType, memberID string) (*SpaceMember, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	memberID = strings.TrimSpace(memberID)
	if !memberType.Valid() || !validOpaqueIdentifier(memberID, 128) {
		return nil, fmt.Errorf("%w: member type and id are invalid", ErrInvalidSpace)
	}
	return s.store.EnsureMemberHasSpace(ctx, scope, memberType, memberID, s.now().UTC())
}

// ListScopeSpaceMembers returns every space membership of the scope in one
// read: which spaces each Agent and Team is in.
func (s *SpaceService) ListScopeSpaceMembers(ctx context.Context, scope Scope) ([]*SpaceMember, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListScopeSpaceMembers(ctx, scope)
}

// SetSpaceAccess changes who may see the space. The people added to it stay,
// so a space that returns to "invited" has them again.
func (s *SpaceService) SetSpaceAccess(ctx context.Context, scope Scope, spaceID string, access SpaceAccess) (*Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !access.Valid() {
		return nil, fmt.Errorf("%w: access must be owner, invited or everyone", ErrInvalidSpace)
	}
	return s.store.SetSpaceAccess(ctx, scope, strings.TrimSpace(spaceID), access, s.now().UTC())
}

// AddSpacePerson adds a person to the space; the host must have verified
// they belong to the scope.
func (s *SpaceService) AddSpacePerson(ctx context.Context, req AddSpacePersonRequest) (*SpacePerson, bool, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, false, err
	}
	person := &SpacePerson{
		SpaceID: strings.TrimSpace(req.SpaceID), UserID: strings.TrimSpace(req.UserID),
		AddedBy: strings.TrimSpace(req.AddedBy), AddedAt: s.now().UTC(),
	}
	if err := person.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.PutSpacePerson(ctx, req.Scope, person)
}

func (s *SpaceService) RemoveSpacePerson(ctx context.Context, scope Scope, spaceID, userID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return s.store.DeleteSpacePerson(ctx, scope, strings.TrimSpace(spaceID), strings.TrimSpace(userID))
}

func (s *SpaceService) ListSpacePeople(ctx context.Context, scope Scope, spaceID string) ([]*SpacePerson, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListSpacePeople(ctx, scope, strings.TrimSpace(spaceID))
}

// VisibleSpaces returns the ids of the spaces the person may see. Tenant
// admins get no exception: an "owner" space is seen by its owner alone.
func (s *SpaceService) VisibleSpaces(ctx context.Context, scope Scope, userID string) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.VisibleSpaces(ctx, scope, strings.TrimSpace(userID))
}

// CanEditSpace reports whether the person may change the space itself (name,
// look, position, delete): its owner only. A missing space is false.
func (s *SpaceService) CanEditSpace(ctx context.Context, scope Scope, spaceID, userID string) (bool, error) {
	if err := scope.Validate(); err != nil {
		return false, err
	}
	space, err := s.store.GetSpace(ctx, scope, strings.TrimSpace(spaceID))
	if errors.Is(err, ErrSpaceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	userID = strings.TrimSpace(userID)
	return userID != "" && space.OwnerID == userID, nil
}

// CanManageSpace reports whether the person may change the space's access
// and people: its owner, or a tenant admin who can see it.
func (s *SpaceService) CanManageSpace(ctx context.Context, scope Scope, spaceID, userID string, isAdmin bool) (bool, error) {
	owner, err := s.CanEditSpace(ctx, scope, spaceID, userID)
	if err != nil || owner || !isAdmin {
		return owner, err
	}
	visible, err := s.store.VisibleSpaces(ctx, scope, strings.TrimSpace(userID))
	if err != nil {
		return false, err
	}
	return slices.Contains(visible, strings.TrimSpace(spaceID)), nil
}

// DefaultSpaceID is the deterministic id of a scope's Default space. The
// PostgreSQL backfill computes the same value with md5().
func DefaultSpaceID(scope Scope) string {
	digest := md5.Sum([]byte(scope.Kind + ":" + scope.ID))
	return "default-" + hex.EncodeToString(digest[:])
}

func newDefaultSpace(scope Scope, now time.Time) *Space {
	return &Space{
		ID: DefaultSpaceID(scope), Scope: scope, Name: DefaultSpaceName, Look: DefaultSpaceLook,
		Position: 0, IsDefault: true, Access: SpaceAccessEveryone, OwnerID: DefaultSpaceOwner, CreatedAt: now, UpdatedAt: now,
	}
}

func cloneSpace(space *Space) *Space {
	if space == nil {
		return nil
	}
	cloned := *space
	return &cloned
}

// spaceVisibleTo is the rule VisibleSpaces applies to one space; invited
// reports whether the person was added to it.
func spaceVisibleTo(space *Space, userID string, invited bool) bool {
	return (userID != "" && space.OwnerID == userID) || space.Access == SpaceAccessEveryone ||
		(space.Access == SpaceAccessInvited && invited)
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
