package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// roleSlotTeamRows are stored Teams in the role-slot shape: "solo" gives each
// member a role of its own (what HelloSeal built), "shared" has one role held
// by two members, and both name approver roles.
var roleSlotTeamRows = map[string]map[string]string{
	"team_definitions": {
		"solo/0": `{"id":"solo","version":"0","displayName":"Solo","purpose":"Help","roles":[
			{"id":"maya-1","displayName":"Maya","purpose":"Owns pricing","minimumMembers":1,"maximumMembers":1}],
			"routing":{"notes":"Maya first","leadRoleId":"maya-1"},"approvals":{"maximumRisk":"write"},"digest":"sha256:solo-0"}`,
		"solo/1": `{"id":"solo","version":"1","displayName":"Solo","purpose":"Help","roles":[
			{"id":"maya-1","displayName":"Maya","purpose":"Owns pricing","minimumMembers":1,"maximumMembers":1,"requiredDefinitionIds":["maya-definition"],"channelParticipation":"active"},
			{"id":"otto-2","displayName":"Otto","purpose":"Owns press","minimumMembers":1,"maximumMembers":1,"requiredDefinitionIds":["otto-definition"],"channelParticipation":"observe_only"}],
			"approvals":{"maximumRisk":"write","approverRoleIds":["otto-2"]},
			"amendments":{"agentMayPropose":true,"allowedFields":["purpose","roles"]},"digest":"sha256:solo-1"}`,
		"shared/1": `{"id":"shared","version":"1","displayName":"Shared","purpose":"Analyze","roles":[
			{"id":"analyst","displayName":"Analyst","purpose":"Analyze evidence","minimumMembers":2,"requiredSkillIds":["research"]},
			{"id":"lead","displayName":"Lead","purpose":"Coordinate"}],
			"approvals":{"maximumRisk":"read","approverRoleIds":["analyst"]},"digest":"sha256:shared-1"}`,
	},
	"team_deployments": {
		"solo": `{"id":"solo","scope":{"kind":"tenant","id":"one"},"definitionId":"solo","activeVersion":"1","status":"active","revision":3,"roster":[
			{"id":"maya-1","roleId":"maya-1","agentDeploymentId":"maya","displayName":"Maya","owns":["pricing"]},
			{"id":"otto-2","roleId":"otto-2","agentDeploymentId":"otto"}]}`,
		"shared": `{"id":"shared","scope":{"kind":"tenant","id":"one"},"definitionId":"shared","activeVersion":"1","status":"active","revision":1,"roster":[
			{"id":"member-1","roleId":"analyst","agentDeploymentId":"a1","displayName":"Analyst 1"},
			{"id":"member-2","roleId":"analyst","agentDeploymentId":"a2","displayName":"Analyst 2"},
			{"id":"member-3","roleId":"lead","agentDeploymentId":"l1"}]}`,
	},
	"team_definition_amendments": {
		"amendment": `{"id":"amendment","definitionId":"solo","deploymentId":"solo","baseVersion":"1","baseDigest":"sha256:solo-1","candidate":{"id":"solo","version":"2","displayName":"Solo","purpose":"Help more","roles":[
			{"id":"maya-1","displayName":"Maya","purpose":"Owns pricing"},{"id":"otto-2","displayName":"Otto","purpose":"Owns press"}],
			"approvals":{"maximumRisk":"write","approverRoleIds":["otto-2"]}},"status":"activated","revision":1}`,
	},
	"agent_requests": {
		"to-role": `{"id":"to-role","recipient":{"type":"team","id":"shared"},"semanticRole":"analyst","candidates":[
			{"agentDeploymentId":"a1","rosterAssignmentId":"member-1","roleId":"analyst","requiredSkillIds":["research"],"capacityLimit":1,"activeRuns":0,"availableSlots":1},
			{"agentDeploymentId":"a2","rosterAssignmentId":"member-2","roleId":"analyst","requiredSkillIds":["research"],"capacityLimit":1,"activeRuns":0,"availableSlots":1}]}`,
		"to-own-role": `{"id":"to-own-role","recipient":{"type":"team","id":"solo"},"semanticRole":"maya-1"}`,
		"to-agent":    `{"id":"to-agent","recipient":{"type":"agent","id":"a1"},"semanticRole":"analyst"}`,
	},
}

// memberTeamRows is what migration 85 makes of roleSlotTeamRows.
var memberTeamRows = map[string]map[string]string{
	"team_definitions": {
		"solo/0":   `{"id":"solo","version":"0","displayName":"Solo","purpose":"Help","routing":{"notes":"Maya first"},"approvals":{"maximumRisk":"write"},"digest":"sha256:solo-0"}`,
		"solo/1":   `{"id":"solo","version":"1","displayName":"Solo","purpose":"Help","approvals":{"maximumRisk":"write"},"amendments":{"agentMayPropose":true,"allowedFields":["purpose","members"]},"digest":"sha256:solo-1"}`,
		"shared/1": `{"id":"shared","version":"1","displayName":"Shared","purpose":"Analyze","approvals":{"maximumRisk":"read"},"digest":"sha256:shared-1"}`,
	},
	"team_deployments": {
		"solo": `{"id":"solo","scope":{"kind":"tenant","id":"one"},"definitionId":"solo","activeVersion":"1","status":"active","revision":3,"roster":[
			{"id":"maya-1","agentDeploymentId":"maya","displayName":"Maya","purpose":"Owns pricing","channelParticipation":"active","owns":["pricing"]},
			{"id":"otto-2","agentDeploymentId":"otto","displayName":"Otto","purpose":"Owns press","channelParticipation":"observe_only","approver":true}]}`,
		"shared": `{"id":"shared","scope":{"kind":"tenant","id":"one"},"definitionId":"shared","activeVersion":"1","status":"active","revision":1,"roster":[
			{"id":"member-1","agentDeploymentId":"a1","displayName":"Analyst 1","purpose":"Analyze evidence","requiredSkillIds":["research"],"approver":true},
			{"id":"member-2","agentDeploymentId":"a2","displayName":"Analyst 2","purpose":"Analyze evidence","requiredSkillIds":["research"],"approver":true},
			{"id":"member-3","agentDeploymentId":"l1","displayName":"Lead","purpose":"Coordinate","lead":true}]}`,
	},
	"team_definition_amendments": {
		"amendment": `{"id":"amendment","definitionId":"solo","deploymentId":"solo","baseVersion":"1","baseDigest":"sha256:solo-1","candidate":{"id":"solo","version":"2","displayName":"Solo","purpose":"Help more",
			"approvals":{"maximumRisk":"write"}},"status":"activated","revision":1}`,
	},
	"agent_requests": {
		"to-role": `{"id":"to-role","recipient":{"type":"team","id":"shared"},"semanticRole":"member-1","candidates":[
			{"agentDeploymentId":"a1","rosterAssignmentId":"member-1","requiredSkillIds":["research"],"capacityLimit":1,"activeRuns":0,"availableSlots":1},
			{"agentDeploymentId":"a2","rosterAssignmentId":"member-2","requiredSkillIds":["research"],"capacityLimit":1,"activeRuns":0,"availableSlots":1}]}`,
		"to-own-role": roleSlotTeamRows["agent_requests"]["to-own-role"],
		"to-agent":    roleSlotTeamRows["agent_requests"]["to-agent"],
	},
}

// rolledBackTeamRows is what rolling migration 85 back makes of
// memberTeamRows: every member holds a role of its own, in every version.
var rolledBackTeamRows = func() map[string]map[string]string {
	soloRoles := `"roles":[
		{"id":"maya-1","displayName":"Maya","purpose":"Owns pricing","minimumMembers":1,"maximumMembers":1,"channelParticipation":"active"},
		{"id":"otto-2","displayName":"Otto","purpose":"Owns press","minimumMembers":1,"maximumMembers":1,"channelParticipation":"observe_only"}]`
	return map[string]map[string]string{
		"team_definitions": {
			"solo/0": `{"id":"solo","version":"0","displayName":"Solo","purpose":"Help",` + soloRoles + `,"routing":{"notes":"Maya first"},"approvals":{"maximumRisk":"write","approverRoleIds":["otto-2"]},"digest":"sha256:solo-0"}`,
			"solo/1": `{"id":"solo","version":"1","displayName":"Solo","purpose":"Help",` + soloRoles + `,"approvals":{"maximumRisk":"write","approverRoleIds":["otto-2"]},"amendments":{"agentMayPropose":true,"allowedFields":["purpose","roles"]},"digest":"sha256:solo-1"}`,
			"shared/1": `{"id":"shared","version":"1","displayName":"Shared","purpose":"Analyze","roles":[
				{"id":"member-1","displayName":"Analyst 1","purpose":"Analyze evidence","minimumMembers":1,"maximumMembers":1,"requiredSkillIds":["research"]},
				{"id":"member-2","displayName":"Analyst 2","purpose":"Analyze evidence","minimumMembers":1,"maximumMembers":1,"requiredSkillIds":["research"]},
				{"id":"member-3","displayName":"Lead","purpose":"Coordinate","minimumMembers":1,"maximumMembers":1}],
				"routing":{"leadRoleId":"member-3"},"approvals":{"maximumRisk":"read","approverRoleIds":["member-1","member-2"]},"digest":"sha256:shared-1"}`,
		},
		"team_deployments": {
			"solo": `{"id":"solo","scope":{"kind":"tenant","id":"one"},"definitionId":"solo","activeVersion":"1","status":"active","revision":3,"roster":[
				{"id":"maya-1","roleId":"maya-1","agentDeploymentId":"maya","displayName":"Maya","owns":["pricing"]},
				{"id":"otto-2","roleId":"otto-2","agentDeploymentId":"otto","displayName":"Otto"}]}`,
			"shared": `{"id":"shared","scope":{"kind":"tenant","id":"one"},"definitionId":"shared","activeVersion":"1","status":"active","revision":1,"roster":[
				{"id":"member-1","roleId":"member-1","agentDeploymentId":"a1","displayName":"Analyst 1"},
				{"id":"member-2","roleId":"member-2","agentDeploymentId":"a2","displayName":"Analyst 2"},
				{"id":"member-3","roleId":"member-3","agentDeploymentId":"l1","displayName":"Lead"}]}`,
		},
		"team_definition_amendments": {
			"amendment": `{"id":"amendment","definitionId":"solo","deploymentId":"solo","baseVersion":"1","baseDigest":"sha256:solo-1","candidate":{"id":"solo","version":"2","displayName":"Solo","purpose":"Help more",` + soloRoles + `,
				"approvals":{"maximumRisk":"write","approverRoleIds":["otto-2"]}},"status":"activated","revision":1}`,
		},
		"agent_requests": {
			"to-role": `{"id":"to-role","recipient":{"type":"team","id":"shared"},"semanticRole":"member-1","candidates":[
				{"agentDeploymentId":"a1","rosterAssignmentId":"member-1","roleId":"member-1","requiredSkillIds":["research"],"capacityLimit":1,"activeRuns":0,"availableSlots":1},
				{"agentDeploymentId":"a2","rosterAssignmentId":"member-2","roleId":"member-2","requiredSkillIds":["research"],"capacityLimit":1,"activeRuns":0,"availableSlots":1}]}`,
			"to-own-role": roleSlotTeamRows["agent_requests"]["to-own-role"],
			"to-agent":    roleSlotTeamRows["agent_requests"]["to-agent"],
		},
	}
}()

// seedRoleSlotTeams inserts roleSlotTeamRows. query adapts "?" placeholders
// and bare table names to the database.
func seedRoleSlotTeams(t *testing.T, db *sql.DB, query func(string) string) {
	t.Helper()
	now := time.Now().UTC()
	exec := func(statement string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(query(statement), args...); err != nil {
			t.Fatal(err)
		}
	}
	for key, payload := range roleSlotTeamRows["team_definitions"] {
		id, version, _ := strings.Cut(key, "/")
		exec(`INSERT INTO team_definitions (id, version, digest, created_at, payload) VALUES (?, ?, ?, ?, ?)`, id, version, "sha256:"+id+"-"+version, now, payload)
	}
	for id, payload := range roleSlotTeamRows["team_deployments"] {
		exec(`INSERT INTO team_deployments (scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES ('tenant', 'one', ?, ?, '1', 1, ?, ?)`, id, id, now, payload)
	}
	// An amendment still in flight would also get its digests recomputed by
	// the connect-once migration on every SQLite open.
	for id, payload := range roleSlotTeamRows["team_definition_amendments"] {
		exec(`INSERT INTO team_definition_amendments (id, scope_kind, scope_id, deployment_id, status, revision, updated_at, payload) VALUES (?, 'tenant', 'one', 'solo', 'activated', 1, ?, ?)`, id, now, payload)
	}
	for id, payload := range roleSlotTeamRows["agent_requests"] {
		exec(`INSERT INTO agent_requests (scope_kind, scope_id, id, kind, status, requester_type, requester_id, recipient_type, recipient_id, source_run_id, revision, created_at, updated_at, payload)
			VALUES ('tenant', 'one', ?, 'request', 'pending', 'agent', 'lead', 'team', 'shared', ?, 1, ?, ?, ?)`, id, "run-"+id, now, now, payload)
	}
}

// requireTeamRows compares every stored Team payload, and each definition's
// digest column, with want.
func requireTeamRows(t *testing.T, db *sql.DB, query func(string) string, stage string, want map[string]map[string]string) {
	t.Helper()
	for table, rows := range want {
		key, payload := "id", "payload"
		if table == "team_definitions" {
			key = "id || '/' || version"
		}
		for id, expected := range rows {
			var stored string
			if err := db.QueryRow(query(`SELECT `+payload+` FROM `+table+` WHERE `+key+` = ?`), id).Scan(&stored); err != nil {
				t.Fatalf("%s: %s %s: %v", stage, table, id, err)
			}
			var got, wanted interface{}
			if err := json.Unmarshal([]byte(stored), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(expected), &wanted); err != nil {
				t.Fatalf("%s %s: %v", table, id, err)
			}
			if !reflect.DeepEqual(got, wanted) {
				t.Fatalf("%s: %s %s =\n%s\nwant\n%s", stage, table, id, stored, expected)
			}
			if table == "team_definitions" {
				var digest string
				if err := db.QueryRow(query(`SELECT digest FROM team_definitions WHERE `+key+` = ?`), id).Scan(&digest); err != nil || digest != "sha256:"+strings.ReplaceAll(id, "/", "-") {
					t.Fatalf("%s: definition %s digest = %q, %v", stage, id, digest, err)
				}
			}
		}
	}
}

// requireMemberTeamsLoad reads the migrated Teams through the store.
func requireMemberTeamsLoad(t *testing.T, store kernelteam.Store) {
	t.Helper()
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	shared, err := store.GetTeamDeployment(ctx, scope, "shared")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := store.GetTeamDefinition(ctx, "shared", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.Validate(definition); err != nil {
		t.Fatalf("migrated Team is invalid: %v", err)
	}
	if got := fmt.Sprintf("%+v", shared.Roster); got != "[{ID:member-1 AgentDeploymentID:a1 DisplayName:Analyst 1 Purpose:Analyze evidence ChannelParticipation: RequiredSkillIDs:[research] Approver:true Lead:false Owns:[]} "+
		"{ID:member-2 AgentDeploymentID:a2 DisplayName:Analyst 2 Purpose:Analyze evidence ChannelParticipation: RequiredSkillIDs:[research] Approver:true Lead:false Owns:[]} "+
		"{ID:member-3 AgentDeploymentID:l1 DisplayName:Lead Purpose:Coordinate ChannelParticipation: RequiredSkillIDs:[] Approver:false Lead:true Owns:[]}]" {
		t.Fatalf("shared roster = %s", got)
	}
	solo, err := store.GetTeamDeployment(ctx, scope, "solo")
	if err != nil || len(solo.Roster) != 2 || solo.Roster[1].ChannelParticipation != kernelteam.ChannelObserveOnly || !solo.Roster[1].Approver || solo.Roster[0].Approver {
		t.Fatalf("solo = %+v, %v", solo, err)
	}
}
