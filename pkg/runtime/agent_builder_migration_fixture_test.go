package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The agent builder cutover fixture both store migrations are tested with: a
// v75 change-set lineage and the leaf drafts the migration keeps from it. It
// is shared by the SQLite and Postgres migration tests, so it moves to the
// store test package (storetest) when those tests move to their stores.

const (
	cutoverCreatePrompt = "Create the profile of exactly one personal agent from this request: keep it a single agent. Request: Help me plan trips"
	cutoverAmendPrompt  = "Revise the draft. Request: Make it friendlier"
)

// cutoverChangeSetRow is a retired change-set row as a v75 store held it.
type cutoverChangeSetRow struct {
	id, parentID, status, mode, prompt string
	agentName                          string
}

func (r cutoverChangeSetRow) payload(t *testing.T, scope Scope) string {
	t.Helper()
	value := map[string]interface{}{
		"id": r.id, "scope": scope, "mode": r.mode, "prompt": r.prompt, "status": r.status, "profileOnly": true,
		"actor": map[string]string{"type": "user", "id": "user-7"},
		"result": map[string]interface{}{"candidate": map[string]interface{}{"agents": []map[string]interface{}{{
			"id": "agent-" + r.id, "version": "1.0.0", "displayName": r.agentName, "purpose": "Plans trips\nend to end",
			"systemPrompt": "Plan trips carefully.", "personality": "Warm", "operatingPrinciples": []string{"Ask first", "Ask first", " "},
			"authority": map[string]string{"maximumRisk": "read"},
		}}}},
		"placement": map[string]interface{}{"agentDeploymentIds": map[string]string{"agent-" + r.id: "deployment-" + r.id}},
	}
	if r.parentID != "" {
		value["parentId"] = r.parentID
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// cutoverLineageRows: a create→amend lineage (leaf: the amend), an applied
// one, a review draft with a failed revision (leaf: the review draft) and a
// rejected one.
var cutoverLineageRows = []cutoverChangeSetRow{
	{id: "cs-create", status: "ready", mode: "create", prompt: cutoverCreatePrompt, agentName: "Trip planner"},
	{id: "cs-amend", parentID: "cs-create", status: "ready", mode: "amend", prompt: cutoverAmendPrompt, agentName: "Friendly trip planner"},
	{id: "cs-applied", status: "applied", mode: "create", prompt: "Request: Old agent", agentName: "Old"},
	{id: "cs-review", status: "review", mode: "create", prompt: "Request: Watch my inbox", agentName: "Inbox watcher"},
	{id: "cs-failed", parentID: "cs-review", status: "failed", mode: "amend", prompt: "Request: broken", agentName: "Broken"},
	{id: "cs-rejected", status: "rejected", mode: "create", prompt: "Request: no", agentName: "No"},
}

var builderCutoverScope = Scope{Kind: "tenant", ID: "cutover"}

// assertCutoverLeafDrafts checks the two leaves of cutoverLineageRows.
func assertCutoverLeafDrafts(t *testing.T, drafts []AgentBuilderCutoverDraft) {
	t.Helper()
	if len(drafts) != 2 || drafts[0].ID != "cs-amend" || drafts[1].ID != "cs-review" {
		t.Fatalf("leaf drafts = %#v; want cs-amend and cs-review", drafts)
	}
	amend, review := drafts[0], drafts[1]
	if !amend.Readable || amend.Scope != builderCutoverScope || amend.IdempotencyKey != "key-cs-amend" || amend.ActorType != "user" || amend.ActorID != "user-7" || amend.Mode != "amend" {
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
		request.Requester != (ConversationParticipant{Type: ConversationParticipantUser, ID: "user-7"}) {
		t.Fatalf("amend start request = %#v", request)
	}
	if request.Parameters.Name != "Friendly trip planner" || request.Parameters.Model != "gpt-6-luna" || request.Parameters.Kind != AgentBuilderKindAgent ||
		!reflect.DeepEqual(request.Parameters.Draft, &AgentBuilderDraftProfile{Purpose: "Plans trips end to end", SystemPrompt: "Plan trips carefully.", Personality: "Warm", OperatingPrinciples: []string{"Ask first"}}) {
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
