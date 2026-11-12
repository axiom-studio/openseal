package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// interactionUmbrellaMigrationActor dismisses a pending request whose asking
// member is no longer on its conversation's roster.
const interactionUmbrellaMigrationActor = "system:umbrella-migration"

type interactionUmbrellaRow struct {
	scopeKind, scopeID, id, deploymentID, conversationID, status, payload string
}

// StampInteractionUmbrellas moves setup and credential requests to their
// conversation's umbrella. A pending request gets owner = the conversation's
// owner, deployment_id = its ID and agentId = the member that asked, or is
// dismissed when that member is no longer on the roster. Settled requests
// without an owner are history and are deleted. Rows that already carry an
// owner are left alone, so the step is safe to replay.
func StampInteractionUmbrellas(ctx context.Context, db ExecQuerier, table func(string) string, placeholder func(int) string) error {
	sqlite := placeholder(1) == "?"
	unstamped, payloadValue := `NOT (payload ? 'owner')`, placeholder(1)+`::jsonb`
	if sqlite {
		unstamped, payloadValue = `json_type(payload, '$.owner') IS NULL`, placeholder(1)
	}
	for _, name := range []string{"skill_setup_requests", "credential_requests"} {
		if _, err := db.ExecContext(ctx, `DELETE FROM `+table(name)+` WHERE status <> 'pending' AND `+unstamped); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, `SELECT scope_kind, scope_id, id, deployment_id, conversation_id, status, payload FROM `+table(name)+` WHERE `+unstamped)
		if err != nil {
			return err
		}
		var pending []interactionUmbrellaRow
		for rows.Next() {
			var row interactionUmbrellaRow
			if err := rows.Scan(&row.scopeKind, &row.scopeID, &row.id, &row.deploymentID, &row.conversationID, &row.status, &row.payload); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, row)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, row := range pending {
			owner, onRoster, err := interactionConversationUmbrella(ctx, db, table, placeholder, row)
			if err != nil {
				return err
			}
			var document map[string]interface{}
			if err := json.Unmarshal([]byte(row.payload), &document); err != nil {
				return fmt.Errorf("%s %s: %w", name, row.id, err)
			}
			if owner.Validate() != nil {
				// A request whose conversation is gone cannot be answered.
				if _, err := db.ExecContext(ctx, `DELETE FROM `+table(name)+` WHERE scope_kind = `+placeholder(1)+` AND scope_id = `+placeholder(2)+` AND id = `+placeholder(3), row.scopeKind, row.scopeID, row.id); err != nil {
					return err
				}
				continue
			}
			document["owner"] = owner
			document["deploymentId"] = owner.ID
			document["agentId"] = row.deploymentID
			status := row.status
			if !onRoster {
				status = "dismissed"
				document["status"], document["resolvedBy"] = status, interactionUmbrellaMigrationActor
				document["revision"] = jsonNumber(document["revision"]) + 1
				document["updatedAt"] = time.Now().UTC()
			}
			encoded, err := json.Marshal(document)
			if err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, `UPDATE `+table(name)+` SET payload = `+payloadValue+`, deployment_id = `+placeholder(2)+`, status = `+placeholder(3)+`, revision = `+placeholder(4)+`
				WHERE scope_kind = `+placeholder(5)+` AND scope_id = `+placeholder(6)+` AND id = `+placeholder(7),
				string(encoded), owner.ID, status, int64(jsonNumber(document["revision"])), row.scopeKind, row.scopeID, row.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonNumber(value interface{}) float64 {
	number, _ := value.(float64)
	return number
}

// interactionConversationUmbrella returns the owner of row's conversation and
// whether the member that asked is still on its roster.
func interactionConversationUmbrella(ctx context.Context, db ExecQuerier, table func(string) string, placeholder func(int) string, row interactionUmbrellaRow) (models.ObjectiveOwner, bool, error) {
	var owner models.ObjectiveOwner
	rows, err := db.QueryContext(ctx, `SELECT owner_type, owner_id FROM `+table("conversations")+` WHERE scope_kind = `+placeholder(1)+` AND scope_id = `+placeholder(2)+` AND id = `+placeholder(3), row.scopeKind, row.scopeID, row.conversationID)
	if err != nil {
		return owner, false, err
	}
	var ownerType string
	if rows.Next() {
		if err := rows.Scan(&ownerType, &owner.ID); err != nil {
			rows.Close()
			return owner, false, err
		}
	}
	if err := rows.Close(); err != nil {
		return owner, false, err
	}
	owner.Type = models.OwnerType(ownerType)
	switch owner.Type {
	case models.OwnerTypeAgent:
		return owner, owner.ID == row.deploymentID, nil
	case models.OwnerTypeTeam:
		payloadText := "payload::text"
		if placeholder(1) == "?" {
			payloadText = "payload"
		}
		teams, err := db.QueryContext(ctx, `SELECT `+payloadText+` FROM `+table("team_deployments")+` WHERE scope_kind = `+placeholder(1)+` AND scope_id = `+placeholder(2)+` AND id = `+placeholder(3), row.scopeKind, row.scopeID, owner.ID)
		if err != nil {
			return owner, false, err
		}
		defer teams.Close()
		if !teams.Next() {
			return owner, false, teams.Err()
		}
		var payload string
		if err := teams.Scan(&payload); err != nil {
			return owner, false, err
		}
		var deployment struct {
			Roster []struct {
				AgentDeploymentID string `json:"agentDeploymentId"`
			} `json:"roster"`
		}
		if err := json.Unmarshal([]byte(payload), &deployment); err != nil {
			return owner, false, err
		}
		for _, assignment := range deployment.Roster {
			if assignment.AgentDeploymentID == row.deploymentID {
				return owner, true, nil
			}
		}
		return owner, false, nil
	}
	return models.ObjectiveOwner{}, false, nil
}
