package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

func migrateExternalConversationGatewayAuthoritySQLite(db *sql.DB) error {
	ctx := context.Background()
	conn, err := beginImmediateSQLite(ctx, db)
	if err != nil {
		return err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS external_conversation_gateways_deployment_idx
		ON external_conversation_gateways (scope_kind,scope_id,json_extract(payload,'$.gateway.deploymentId'),status,updated_at DESC,id)`); err != nil {
		return err
	}
	// MIN selects one actual reviewed endpoint after the group has proven
	// every candidate has the same installation/application identity.
	rows, err := conn.QueryContext(ctx, `SELECT g.payload,MIN(e.payload),COALESCE(b.revision,json_extract(g.payload,'$.gateway.adapter.bindingRevision'))
		FROM external_conversation_gateways g JOIN external_conversation_endpoints e
			ON e.scope_kind=g.scope_kind AND e.scope_id=g.scope_id AND e.provider=g.provider
			AND json_extract(e.payload,'$.deploymentId')=json_extract(g.payload,'$.gateway.deploymentId')
			AND json_extract(e.payload,'$.adapter.bindingId')=json_extract(g.payload,'$.gateway.adapter.bindingId')
			AND json_extract(e.payload,'$.adapter.adapterId')=json_extract(g.payload,'$.gateway.adapter.adapterId')
			AND json_extract(e.payload,'$.adapter.skillId')=json_extract(g.payload,'$.gateway.adapter.skillId')
			AND COALESCE(json_extract(e.payload,'$.adapter.sourceIdentity'),'')=COALESCE(json_extract(g.payload,'$.gateway.adapter.sourceIdentity'),'')
		LEFT JOIN skill_bindings b ON b.scope_kind=g.scope_kind AND b.scope_id=g.scope_id
			AND b.deployment_id=json_extract(g.payload,'$.gateway.deploymentId') AND b.id=json_extract(g.payload,'$.gateway.adapter.bindingId')
		WHERE g.scope_kind<>'platform' AND g.status<>'retired' AND e.status='active'
			AND COALESCE(json_extract(g.payload,'$.gateway.installationId'),'')=''
			AND COALESCE(json_extract(e.payload,'$.installationId'),'')<>''
			AND json_extract(e.payload,'$.adapter.bindingRevision')<=COALESCE(b.revision,json_extract(g.payload,'$.gateway.adapter.bindingRevision'))
		GROUP BY g.scope_kind,g.scope_id,g.id
		HAVING COUNT(DISTINCT json_array(json_extract(e.payload,'$.installationId'),COALESCE(json_extract(e.payload,'$.applicationId'),'')))=1`)
	if err != nil {
		return err
	}
	updates := make([]*ExternalConversationGatewayRegistration, 0)
	now := time.Now().UTC()
	for rows.Next() {
		var gatewayJSON, endpointJSON []byte
		var bindingRevision int64
		if err := rows.Scan(&gatewayJSON, &endpointJSON, &bindingRevision); err != nil {
			rows.Close()
			return err
		}
		var gateway ExternalConversationGatewayRegistration
		var endpoint ExternalConversationEndpoint
		if err := json.Unmarshal(gatewayJSON, &gateway); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(endpointJSON, &endpoint); err != nil {
			rows.Close()
			return err
		}
		updated, changed, err := backfilledGatewayAuthority(&gateway, []*ExternalConversationEndpoint{&endpoint}, now, bindingRevision)
		if err != nil {
			rows.Close()
			return err
		}
		if changed {
			updates = append(updates, updated)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, gateway := range updates {
		payload, err := json.Marshal(gateway)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE external_conversation_gateways SET revision=?,updated_at=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
			gateway.Revision, gateway.UpdatedAt, string(payload), gateway.Gateway.Scope.Kind, gateway.Gateway.Scope.ID, gateway.ID, gateway.Revision-1); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
