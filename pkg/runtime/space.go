package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/google/uuid"
)

type CreateSpaceRequest struct {
	Scope   models.Scope
	Name    string
	Look    models.SpaceLook
	OwnerID string
}

// SetSpaceAccessStateRequest is the whole of who may see a space: its owner,
// its access and the people added to it. An empty OwnerID leaves the owner as
// it is, for an owner the host cannot name yet.
type SetSpaceAccessStateRequest struct {
	Scope   models.Scope
	SpaceID string
	OwnerID string
	Access  models.SpaceAccess
	People  []string
	AddedBy string
}

type AddSpacePersonRequest struct {
	Scope   models.Scope
	SpaceID string
	UserID  string
	AddedBy string
}

type UpdateSpaceRequest struct {
	Scope    models.Scope
	SpaceID  string
	Name     *string
	Look     *models.SpaceLook
	Position *int
}

type PutSpaceMemberRequest struct {
	Scope    models.Scope
	SpaceID  string
	Type     models.SpaceMemberType
	MemberID string
}

type DeleteSpaceMemberRequest struct {
	Scope    models.Scope
	SpaceID  string
	Type     models.SpaceMemberType
	MemberID string
	// ActorType and ActorID sign the Team updates that removing an Agent
	// makes.
	ActorType string
	ActorID   string
	// ConfirmTeams accepts what removing an Agent does to the Teams in the
	// space that list it; without it such a removal changes nothing and
	// returns *SpaceAgentRemovalNeedsConfirmation.
	ConfirmTeams bool
}

type SpaceTeamOutcome string

const (
	// SpaceTeamLeaves: the Agent leaves the Team's roster.
	SpaceTeamLeaves SpaceTeamOutcome = "leaves"
	// SpaceTeamArchived: the Team cannot lose the Agent and is archived.
	SpaceTeamArchived SpaceTeamOutcome = "archived"
)

// SpaceAgentRemovalTeam is what taking an Agent out of a space does to one
// Team there. Remaining counts the roster without the Agent.
type SpaceAgentRemovalTeam struct {
	ID          string           `json:"id"`
	DisplayName string           `json:"displayName"`
	Outcome     SpaceTeamOutcome `json:"outcome"`
	Remaining   int              `json:"remaining"`
}

// SpaceAgentRemovalNeedsConfirmation is the error of an unconfirmed removal
// that would change Teams. Nothing was changed.
type SpaceAgentRemovalNeedsConfirmation struct {
	Teams []SpaceAgentRemovalTeam
}

func (e *SpaceAgentRemovalNeedsConfirmation) Error() string {
	return fmt.Sprintf("removing the agent changes %d team(s) in this space and needs confirmation", len(e.Teams))
}

type SpaceService struct {
	store   models.SpaceStore
	teams   *kernelteam.Registry
	rosters models.SpaceRosterStore
	now     func() time.Time
	newID   func() string
}

func NewSpaceService(store models.SpaceStore, opts ...models.ServiceOption) *SpaceService {
	return &SpaceService{store: store, now: models.ClockFrom(opts), newID: uuid.NewString}
}

// UseTeams ties Team rosters to spaces: a Team's Agents are in every space
// the Team is in. A Team added to a space brings its Agents, and an Agent
// that leaves a space leaves that space's Teams. The registry must write to
// this service's store, so both changes commit together.
func (s *SpaceService) UseTeams(teams *kernelteam.Registry) error {
	store, ok := s.store.(models.SpaceRosterStore)
	if !ok || !teams.UsesStore(store) {
		return errors.New("spaces and Teams must share one store")
	}
	s.teams, s.rosters = teams, store
	return nil
}

// ListSpaces returns every space of the scope by position. It creates none.
func (s *SpaceService) ListSpaces(ctx context.Context, scope models.Scope) ([]*models.Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListSpaces(ctx, scope)
}

// DefaultSpace returns the owner's default space in the scope: the first
// space they created there. An owner with no space gets ErrNoDefaultSpace.
func (s *SpaceService) DefaultSpace(ctx context.Context, scope models.Scope, ownerID string) (*models.Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.DefaultSpace(ctx, scope, strings.TrimSpace(ownerID))
}

func (s *SpaceService) GetSpace(ctx context.Context, scope models.Scope, spaceID string) (*models.Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.GetSpace(ctx, scope, strings.TrimSpace(spaceID))
}

// CreateSpace creates the owner's space. Their first space in the scope is
// their default space.
func (s *SpaceService) CreateSpace(ctx context.Context, req CreateSpaceRequest) (*models.Space, error) {
	now := s.now().UTC()
	space := &models.Space{
		ID: s.newID(), Scope: req.Scope, Name: strings.TrimSpace(req.Name), Look: req.Look,
		Access: models.SpaceAccessOwner, OwnerID: strings.TrimSpace(req.OwnerID), CreatedAt: now, UpdatedAt: now,
	}
	if err := space.Validate(); err != nil {
		return nil, err
	}
	return s.store.CreateSpace(ctx, space)
}

func (s *SpaceService) UpdateSpace(ctx context.Context, req UpdateSpaceRequest) (*models.Space, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	patch := models.SpacePatch{Look: req.Look, Position: req.Position}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if err := models.ValidateSpaceName(name); err != nil {
			return nil, err
		}
		patch.Name = &name
	}
	if req.Look != nil && !req.Look.Valid() {
		return nil, fmt.Errorf("%w: invalid look %q", models.ErrInvalidSpace, *req.Look)
	}
	if req.Position != nil && *req.Position < 0 {
		return nil, fmt.Errorf("%w: position cannot be negative", models.ErrInvalidSpace)
	}
	return s.store.UpdateSpace(ctx, req.Scope, strings.TrimSpace(req.SpaceID), patch, s.now().UTC())
}

// DeleteSpace deletes a space that is not a default space. Its conversations,
// and the Agents and Teams that are in no other space, move to the default
// space of its owner.
func (s *SpaceService) DeleteSpace(ctx context.Context, scope models.Scope, spaceID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	spaceID = strings.TrimSpace(spaceID)
	var teams []string
	var target *models.Space
	if s.teams != nil {
		space, err := s.store.GetSpace(ctx, scope, spaceID)
		if err != nil {
			return err
		}
		if space.IsDefault {
			return models.ErrDefaultSpaceProtected
		}
		if target, err = s.store.DefaultSpace(ctx, scope, space.OwnerID); err != nil {
			return err
		}
		members, err := s.store.ListSpaceMembers(ctx, scope, spaceID)
		if err != nil {
			return err
		}
		for _, member := range members {
			if member.Type == models.SpaceMemberTeam {
				teams = append(teams, member.ID)
			}
		}
	}
	if err := s.store.DeleteSpace(ctx, scope, spaceID, s.now().UTC()); err != nil || len(teams) == 0 {
		return err
	}
	// A Team that moved to the owner's default space brings its Agents there,
	// like a Team added to any space.
	defaultID := target.ID
	members, err := s.store.ListSpaceMembers(ctx, scope, defaultID)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.Type != models.SpaceMemberTeam || !slices.Contains(teams, member.ID) {
			continue
		}
		if _, _, err := s.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: defaultID, Type: models.SpaceMemberTeam, MemberID: member.ID}); err != nil {
			return err
		}
	}
	return nil
}

func (s *SpaceService) ListSpaceMembers(ctx context.Context, scope models.Scope, spaceID string) ([]*models.SpaceMember, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListSpaceMembers(ctx, scope, strings.TrimSpace(spaceID))
}

// PutSpaceMember adds the member at the end of the space; the host must have
// verified the deployment belongs to the scope. A Team brings the Agents on
// its roster.
func (s *SpaceService) PutSpaceMember(ctx context.Context, req PutSpaceMemberRequest) (*models.SpaceMember, bool, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, false, err
	}
	now := s.now().UTC()
	member := &models.SpaceMember{SpaceID: strings.TrimSpace(req.SpaceID), Type: req.Type, ID: strings.TrimSpace(req.MemberID), AddedAt: now}
	if err := member.Validate(); err != nil {
		return nil, false, err
	}
	var agents []*models.SpaceMember
	if member.Type == models.SpaceMemberTeam && s.teams != nil {
		team, err := s.teams.GetDeployment(ctx, models.ScopeReference(req.Scope), member.ID)
		if err != nil && !errors.Is(err, kernelteam.ErrDeploymentNotFound) {
			return nil, false, err
		}
		if team != nil {
			for _, assignment := range team.Roster {
				agent := &models.SpaceMember{SpaceID: member.SpaceID, Type: models.SpaceMemberAgent, ID: assignment.AgentDeploymentID, AddedAt: now}
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
// the rosters of the Teams in that space, and a Team that cannot lose it is
// archived: the Team registry prepares each update and the store commits them
// with the removal. That needs req.ConfirmTeams; without it nothing changes
// and the error is a *SpaceAgentRemovalNeedsConfirmation listing the Teams.
func (s *SpaceService) DeleteSpaceMember(ctx context.Context, req DeleteSpaceMemberRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return err
	}
	if !req.Type.Valid() {
		return fmt.Errorf("%w: member type must be agent or team", models.ErrInvalidSpace)
	}
	spaceID, memberID := strings.TrimSpace(req.SpaceID), strings.TrimSpace(req.MemberID)
	if req.Type != models.SpaceMemberAgent || s.teams == nil {
		return s.store.DeleteSpaceMember(ctx, req.Scope, spaceID, req.Type, memberID)
	}
	teams, changes, err := s.agentRemovalTeamChanges(ctx, req.Scope, spaceID, memberID, req.ActorType, req.ActorID)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return s.store.DeleteSpaceMember(ctx, req.Scope, spaceID, req.Type, memberID)
	}
	if !req.ConfirmTeams {
		return &SpaceAgentRemovalNeedsConfirmation{Teams: teams}
	}
	return s.rosters.DeleteSpaceAgentWithRosters(ctx, req.Scope, spaceID, memberID, changes)
}

// agentRemovalTeamChanges prepares the update of every Team in the space that
// lists the Agent: its roster without the Agent or, when that would break the
// Team's minimum, the same archiving update a retire makes. An Agent that is
// not in the space changes none.
func (s *SpaceService) agentRemovalTeamChanges(ctx context.Context, scope models.Scope, spaceID, agentID, actorType, actorID string) ([]SpaceAgentRemovalTeam, []*kernelteam.RosterChange, error) {
	members, err := s.store.ListSpaceMembers(ctx, scope, spaceID)
	if err != nil {
		return nil, nil, err
	}
	if !slices.ContainsFunc(members, func(member *models.SpaceMember) bool {
		return member.Type == models.SpaceMemberAgent && member.ID == agentID
	}) {
		return nil, nil, nil
	}
	space, err := s.store.GetSpace(ctx, scope, spaceID)
	if err != nil {
		return nil, nil, err
	}
	reason := fmt.Sprintf("Agent %s left the space %s", agentID, space.Name)
	var teams []SpaceAgentRemovalTeam
	var changes []*kernelteam.RosterChange
	for _, member := range members {
		if member.Type != models.SpaceMemberTeam {
			continue
		}
		team, err := s.teams.GetDeployment(ctx, models.ScopeReference(scope), member.ID)
		if errors.Is(err, kernelteam.ErrDeploymentNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		outcome := SpaceTeamLeaves
		change, err := s.teams.PrepareAgentRemoval(ctx, models.ScopeReference(scope), team.ID, agentID, actorType, actorID, reason)
		if errors.Is(err, kernelteam.ErrRosterMinimum) {
			outcome = SpaceTeamArchived
			archived := *team
			archived.Status = kernelteam.DeploymentArchived
			change, err = s.teams.PrepareDeploymentUpdate(ctx, &archived, team.Revision, actorType, actorID, reason)
		}
		if err != nil {
			return nil, nil, err
		}
		if change == nil {
			continue
		}
		definition, err := s.teams.GetDefinition(ctx, team.DefinitionID, team.ActiveVersion)
		if err != nil {
			return nil, nil, err
		}
		teams = append(teams, SpaceAgentRemovalTeam{ID: team.ID, DisplayName: definition.DisplayName, Outcome: outcome, Remaining: len(team.Roster) - 1})
		changes = append(changes, change)
	}
	return teams, changes, nil
}

// CheckSpaceAgents refuses Agents that are not members of the space.
func (s *SpaceService) CheckSpaceAgents(ctx context.Context, scope models.Scope, spaceID string, agentIDs []string) error {
	space, err := s.store.GetSpace(ctx, scope, strings.TrimSpace(spaceID))
	if err != nil {
		return err
	}
	members, err := s.store.ListSpaceMembers(ctx, scope, space.ID)
	if err != nil {
		return err
	}
	return models.CheckAgentsInSpace(space, members, agentIDs)
}

// CheckTeamAgents refuses Agents that are not in every space the Team is in.
func (s *SpaceService) CheckTeamAgents(ctx context.Context, scope models.Scope, teamID string, agentIDs []string) error {
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
		if !slices.ContainsFunc(members, func(member *models.SpaceMember) bool {
			return member.Type == models.SpaceMemberTeam && member.ID == teamID
		}) {
			continue
		}
		if err := models.CheckAgentsInSpace(space, members, agentIDs); err != nil {
			return err
		}
	}
	return nil
}

// EnsureMemberHasSpace places an Agent or Team deployment that is in no space
// in the default space of ownerID, the person who created it. It returns nil
// when the deployment already has a space and ErrNoDefaultSpace when that
// person has no default space; the deployment then stays in no space.
func (s *SpaceService) EnsureMemberHasSpace(ctx context.Context, scope models.Scope, ownerID string, memberType models.SpaceMemberType, memberID string) (*models.SpaceMember, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	memberID = strings.TrimSpace(memberID)
	if !memberType.Valid() || !models.IsValidOpaqueIdentifier(memberID, 128) {
		return nil, fmt.Errorf("%w: member type and id are invalid", models.ErrInvalidSpace)
	}
	return s.store.EnsureMemberHasSpace(ctx, scope, strings.TrimSpace(ownerID), memberType, memberID, s.now().UTC())
}

// ListScopeSpaceMembers returns every space membership of the scope in one
// read: which spaces each Agent and Team is in.
func (s *SpaceService) ListScopeSpaceMembers(ctx context.Context, scope models.Scope) ([]*models.SpaceMember, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListScopeSpaceMembers(ctx, scope)
}

// SetSpaceAccess changes who may see the space. The people added to it stay,
// so a space that returns to "invited" has them again.
func (s *SpaceService) SetSpaceAccess(ctx context.Context, scope models.Scope, spaceID string, access models.SpaceAccess) (*models.Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !access.Valid() {
		return nil, fmt.Errorf("%w: access must be owner, invited or everyone", models.ErrInvalidSpace)
	}
	return s.store.SetSpaceAccess(ctx, scope, strings.TrimSpace(spaceID), access, s.now().UTC())
}

// SetSpaceOwner hands the space to another owner; the host must have verified
// they belong to the scope. A new owner who has a default space keeps it;
// an owner of the two left without one gets one as models.SpaceStore says.
func (s *SpaceService) SetSpaceOwner(ctx context.Context, scope models.Scope, spaceID, ownerID string) (*models.Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	ownerID = strings.TrimSpace(ownerID)
	if len(ownerID) > 128 {
		return nil, fmt.Errorf("%w: owner is invalid", models.ErrInvalidSpace)
	}
	return s.store.SetSpaceOwner(ctx, scope, strings.TrimSpace(spaceID), ownerID, s.now().UTC())
}

// SetSpaceAccessState replaces the space's owner, access and people in one
// write; the host must have verified the people belong to the scope. A new
// owner moves defaults as SetSpaceOwner does. Repeating it changes nothing.
func (s *SpaceService) SetSpaceAccessState(ctx context.Context, req SetSpaceAccessStateRequest) (*models.Space, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !req.Access.Valid() {
		return nil, fmt.Errorf("%w: access must be owner, invited or everyone", models.ErrInvalidSpace)
	}
	ownerID, spaceID, now := strings.TrimSpace(req.OwnerID), strings.TrimSpace(req.SpaceID), s.now().UTC()
	if ownerID != "" && !models.IsValidOpaqueIdentifier(ownerID, 128) {
		return nil, fmt.Errorf("%w: owner must be a person's id", models.ErrInvalidSpace)
	}
	state := models.SpaceAccessState{OwnerID: ownerID, Access: req.Access, People: make([]*models.SpacePerson, 0, len(req.People))}
	for _, userID := range req.People {
		person := &models.SpacePerson{SpaceID: spaceID, UserID: strings.TrimSpace(userID), AddedBy: strings.TrimSpace(req.AddedBy), AddedAt: now}
		if err := person.Validate(); err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(state.People, func(existing *models.SpacePerson) bool { return existing.UserID == person.UserID }) {
			state.People = append(state.People, person)
		}
	}
	return s.store.SetSpaceAccessState(ctx, req.Scope, spaceID, state, now)
}

// AddSpacePerson adds a person to the space; the host must have verified
// they belong to the scope.
func (s *SpaceService) AddSpacePerson(ctx context.Context, req AddSpacePersonRequest) (*models.SpacePerson, bool, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, false, err
	}
	person := &models.SpacePerson{
		SpaceID: strings.TrimSpace(req.SpaceID), UserID: strings.TrimSpace(req.UserID),
		AddedBy: strings.TrimSpace(req.AddedBy), AddedAt: s.now().UTC(),
	}
	if err := person.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.PutSpacePerson(ctx, req.Scope, person)
}

func (s *SpaceService) RemoveSpacePerson(ctx context.Context, scope models.Scope, spaceID, userID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return s.store.DeleteSpacePerson(ctx, scope, strings.TrimSpace(spaceID), strings.TrimSpace(userID))
}

func (s *SpaceService) ListSpacePeople(ctx context.Context, scope models.Scope, spaceID string) ([]*models.SpacePerson, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListSpacePeople(ctx, scope, strings.TrimSpace(spaceID))
}

// VisibleSpaces returns the ids of the spaces the person may see. Tenant
// admins get no exception: an "owner" space is seen by its owner alone.
func (s *SpaceService) VisibleSpaces(ctx context.Context, scope models.Scope, userID string) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.VisibleSpaces(ctx, scope, strings.TrimSpace(userID))
}

// CanEditSpace reports whether the person may change the space itself (name,
// look, position, delete): its owner only. A missing space is false.
func (s *SpaceService) CanEditSpace(ctx context.Context, scope models.Scope, spaceID, userID string) (bool, error) {
	if err := scope.Validate(); err != nil {
		return false, err
	}
	space, err := s.store.GetSpace(ctx, scope, strings.TrimSpace(spaceID))
	if errors.Is(err, models.ErrSpaceNotFound) {
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
func (s *SpaceService) CanManageSpace(ctx context.Context, scope models.Scope, spaceID, userID string, isAdmin bool) (bool, error) {
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
