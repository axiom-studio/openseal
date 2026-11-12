package storetest

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// SpaceConversationStore is a store with spaces and conversations.
type SpaceConversationStore interface {
	models.SpaceStore
	models.ConversationStore
}

// LegacyDefaultSpaceOwner and LegacyDefaultSpaceID describe the one space
// per scope that the model before per-owner default spaces kept. Only tests
// of the migrations from that model use them.
const LegacyDefaultSpaceOwner = "system"

func LegacyDefaultSpaceID(scope models.Scope) string {
	digest := md5.Sum([]byte(scope.Kind + ":" + scope.ID))
	return "default-" + hex.EncodeToString(digest[:])
}

// AssertSpacesBackfilled checks a database the spaces migration backfilled:
// the legacy Default space of scope with its Agent and Team members and its
// conversation, and the legacy space of the Agent-only scope.
func AssertSpacesBackfilled(t *testing.T, store SpaceConversationStore, scope, agentOnly models.Scope, conversationID string) {
	t.Helper()
	ctx := context.Background()
	legacyID := LegacyDefaultSpaceID(scope)
	listed, err := store.ListSpaces(ctx, scope)
	if err != nil || len(listed) != 1 || listed[0].ID != legacyID || !listed[0].IsDefault || listed[0].OwnerID != LegacyDefaultSpaceOwner ||
		listed[0].Name != "Default" || listed[0].Look != models.DefaultSpaceLook || listed[0].Position != 0 {
		t.Fatalf("backfilled spaces = %+v, err = %v", listed, err)
	}
	members, err := store.ListSpaceMembers(ctx, scope, legacyID)
	if err != nil || len(members) != 3 {
		t.Fatalf("backfilled members = %+v, err = %v", members, err)
	}
	want := []models.SpaceMember{{Type: models.SpaceMemberAgent, ID: "helper"}, {Type: models.SpaceMemberAgent, ID: "writer"}, {Type: models.SpaceMemberTeam, ID: "crew"}}
	for index, member := range members {
		if member.Type != want[index].Type || member.ID != want[index].ID || member.Position != index {
			t.Fatalf("member %d = %+v", index, member)
		}
	}
	conversation, err := store.GetConversation(ctx, scope, conversationID)
	if err != nil || conversation == nil || conversation.SpaceID != legacyID {
		t.Fatalf("backfilled conversation = %+v, err = %v", conversation, err)
	}
	listedConversations, err := store.ListConversations(ctx, models.ConversationFilter{Scope: scope, SpaceID: legacyID})
	if err != nil || len(listedConversations) != 1 {
		t.Fatalf("space filter after backfill = %d, err = %v", len(listedConversations), err)
	}
	if solo, err := store.ListSpaceMembers(ctx, agentOnly, LegacyDefaultSpaceID(agentOnly)); err != nil || len(solo) != 1 || solo[0].ID != "solo" {
		t.Fatalf("agent-only scope = %+v, err = %v", solo, err)
	}
}

// AssertSpaceAccessBackfilled checks a migrated database that had the legacy
// Default space and one space a person made. Migration 86 then leaves the
// legacy space as it is and makes the person's space their default, and
// migration 87 opens the legacy space to everyone again.
func AssertSpaceAccessBackfilled(t *testing.T, store models.SpaceStore, scope models.Scope, workID string) {
	t.Helper()
	ctx := context.Background()
	listed, err := store.ListSpaces(ctx, scope)
	if err != nil || len(listed) != 2 {
		t.Fatalf("spaces = %+v, err = %v", listed, err)
	}
	if listed[0].ID != LegacyDefaultSpaceID(scope) || listed[0].Access != models.SpaceAccessEveryone || listed[0].OwnerID != LegacyDefaultSpaceOwner ||
		!listed[0].IsDefault || listed[0].Name != "Default" {
		t.Fatalf("Default = %+v", listed[0])
	}
	if listed[1].ID != workID || listed[1].Access != models.SpaceAccessOwner || listed[1].OwnerID != "7" || !listed[1].IsDefault {
		t.Fatalf("work = %+v", listed[1])
	}
	if owned, err := store.DefaultSpace(ctx, scope, "7"); err != nil || owned.ID != workID {
		t.Fatalf("default space of the person = %+v, err = %v", owned, err)
	}
	if people, err := store.ListSpacePeople(ctx, scope, workID); err != nil || len(people) != 0 {
		t.Fatalf("people = %+v, err = %v", people, err)
	}
	// Only the space no person owns is shared; another person sees just it.
	if visible, err := store.VisibleSpaces(ctx, scope, "8"); err != nil || len(visible) != 1 || visible[0] != LegacyDefaultSpaceID(scope) {
		t.Fatalf("visible to another person = %v, err = %v", visible, err)
	}
}
