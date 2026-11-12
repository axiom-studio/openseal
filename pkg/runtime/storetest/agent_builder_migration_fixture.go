package storetest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// The agent builder cutover fixture both store migrations are tested with: a
// v75 change-set lineage and the leaf drafts the migration keeps from it.

const (
	cutoverCreatePrompt = "Create the profile of exactly one personal agent from this request: keep it a single agent. Request: Help me plan trips"
	cutoverAmendPrompt  = "Revise the draft. Request: Make it friendlier"
)

// CutoverChangeSetRow is a retired change-set row as a v75 store held it.
type CutoverChangeSetRow struct {
	ID, ParentID, Status, mode, prompt string
	agentName                          string
}

// Payload is the row's stored payload in scope.
func (r CutoverChangeSetRow) Payload(t *testing.T, scope models.Scope) string {
	t.Helper()
	value := map[string]interface{}{
		"id": r.ID, "scope": scope, "mode": r.mode, "prompt": r.prompt, "status": r.Status, "profileOnly": true,
		"actor": map[string]string{"type": "user", "id": "user-7"},
		"result": map[string]interface{}{"candidate": map[string]interface{}{"agents": []map[string]interface{}{{
			"id": "agent-" + r.ID, "version": "1.0.0", "displayName": r.agentName, "purpose": "Plans trips\nend to end",
			"systemPrompt": "Plan trips carefully.", "personality": "Warm", "operatingPrinciples": []string{"Ask first", "Ask first", " "},
			"authority": map[string]string{"maximumRisk": "read"},
		}}}},
		"placement": map[string]interface{}{"agentDeploymentIds": map[string]string{"agent-" + r.ID: "deployment-" + r.ID}},
	}
	if r.ParentID != "" {
		value["parentId"] = r.ParentID
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// CutoverLineageRows: a create→amend lineage (leaf: the amend), an applied
// one, a review draft with a failed revision (leaf: the review draft) and a
// rejected one.
var CutoverLineageRows = []CutoverChangeSetRow{
	{ID: "cs-create", Status: "ready", mode: "create", prompt: cutoverCreatePrompt, agentName: "Trip planner"},
	{ID: "cs-amend", ParentID: "cs-create", Status: "ready", mode: "amend", prompt: cutoverAmendPrompt, agentName: "Friendly trip planner"},
	{ID: "cs-applied", Status: "applied", mode: "create", prompt: "Request: Old agent", agentName: "Old"},
	{ID: "cs-review", Status: "review", mode: "create", prompt: "Request: Watch my inbox", agentName: "Inbox watcher"},
	{ID: "cs-failed", ParentID: "cs-review", Status: "failed", mode: "amend", prompt: "Request: broken", agentName: "Broken"},
	{ID: "cs-rejected", Status: "rejected", mode: "create", prompt: "Request: no", agentName: "No"},
}

// BuilderCutoverScope is the scope the lineage is stored in.
var BuilderCutoverScope = models.Scope{Kind: "tenant", ID: "cutover"}

// AssertCutoverLeafDrafts checks the two leaves of CutoverLineageRows.
func AssertCutoverLeafDrafts(t *testing.T, drafts []models.AgentBuilderCutoverDraft) {
	t.Helper()
	if len(drafts) != 2 || drafts[0].ID != "cs-amend" || drafts[1].ID != "cs-review" {
		t.Fatalf("leaf drafts = %#v; want cs-amend and cs-review", drafts)
	}
	amend, review := drafts[0], drafts[1]
	if !amend.Readable || amend.Scope != BuilderCutoverScope || amend.IdempotencyKey != "key-cs-amend" || amend.ActorType != "user" || amend.ActorID != "user-7" || amend.Mode != "amend" {
		t.Fatalf("amend draft = %#v", amend)
	}
	if !reflect.DeepEqual(amend.CutoverPrompts, []string{cutoverCreatePrompt}) || len(review.CutoverPrompts) != 0 {
		t.Fatalf("cutover prompts = %#v / %#v", amend.CutoverPrompts, review.CutoverPrompts)
	}
	if amend.Agent == nil || amend.Agent.DisplayName != "Friendly trip planner" || amend.Placement.AgentDeploymentIDs["agent-cs-amend"] != "deployment-cs-amend" {
		t.Fatalf("amend profile = %#v placement = %#v", amend.Agent, amend.Placement)
	}
	request := amend.StartRequest("gpt-6-luna", "")
	if request.Prompt != "Help me plan trips\nMake it friendlier" || request.IdempotencyKey != "cutover-cs-amend" ||
		request.Requester != (models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user-7"}) {
		t.Fatalf("amend start request = %#v", request)
	}
	if request.Parameters.Name != "Friendly trip planner" || request.Parameters.Model != "gpt-6-luna" || request.Parameters.Kind != models.AgentBuilderKindAgent ||
		!reflect.DeepEqual(request.Parameters.Draft, &models.AgentBuilderDraftProfile{Purpose: "Plans trips end to end", SystemPrompt: "Plan trips carefully.", Personality: "Warm", OperatingPrinciples: []string{"Ask first"}}) {
		t.Fatalf("amend parameters = %#v draft %#v", request.Parameters, request.Parameters.Draft)
	}
	if err := request.Parameters.Validate(); err != nil {
		t.Fatalf("amend parameters refused: %v", err)
	}
	if strings.Contains(request.Prompt, "Friendly trip planner") || strings.Contains(request.Prompt, "gpt-6-luna") {
		t.Fatalf("name or model leaked into the content: %q", request.Prompt)
	}
	if got := review.StartRequest("", "space-1"); got.Prompt != "Watch my inbox" || got.SpaceID != "space-1" || got.Parameters.Model != "" {
		t.Fatalf("review start request = %#v", got)
	}
}
