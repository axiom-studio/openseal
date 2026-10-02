package runtime

import (
	"context"
	"database/sql"
)

const externalConversationGatewayAuthorityMigrationVersion int64 = 48

// migrateExternalConversationGatewayAuthority narrows legacy gateway authority
// from uniquely matching reviewed endpoint records. Ambiguous or unbound
// installations remain unpinned and retain endpoint-only routing. It never
// learns tenant/account ownership from incoming provider traffic.
func (s *PostgresStore) migrateExternalConversationGatewayAuthority(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, externalConversationGatewayAuthorityMigrationVersion)
	if err != nil || applied {
		return err
	}
	if err := s.migrateWorkflowSourceCallbackLookup(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS external_conversation_gateways_deployment_idx
		ON `+s.table("external_conversation_gateways")+` (scope_kind,scope_id,(payload#>>'{gateway,deploymentId}'),status,updated_at DESC,id)`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `WITH authorities AS (
		SELECT g.scope_kind,g.scope_id,g.id,
			MIN(e.payload->>'installationId') AS installation_id,
			MIN(COALESCE(e.payload->>'applicationId','')) AS application_id
		FROM `+s.table("external_conversation_gateways")+` g
		JOIN `+s.table("external_conversation_endpoints")+` e
			ON e.scope_kind=g.scope_kind AND e.scope_id=g.scope_id AND e.provider=g.provider
			AND e.payload->>'deploymentId'=g.payload#>>'{gateway,deploymentId}'
			AND e.payload#>>'{adapter,bindingId}'=g.payload#>>'{gateway,adapter,bindingId}'
			AND e.payload#>>'{adapter,adapterId}'=g.payload#>>'{gateway,adapter,adapterId}'
			AND e.payload#>>'{adapter,skillId}'=g.payload#>>'{gateway,adapter,skillId}'
			AND COALESCE(e.payload#>>'{adapter,sourceIdentity}','')=COALESCE(g.payload#>>'{gateway,adapter,sourceIdentity}','')
		LEFT JOIN `+s.table("skill_bindings")+` b ON b.scope_kind=g.scope_kind AND b.scope_id=g.scope_id
			AND b.deployment_id=g.payload#>>'{gateway,deploymentId}' AND b.id=g.payload#>>'{gateway,adapter,bindingId}'
		WHERE g.scope_kind<>'platform' AND g.status<>'retired' AND e.status='active'
			AND COALESCE(g.payload#>>'{gateway,installationId}','')=''
			AND COALESCE(e.payload->>'installationId','')<>''
			AND (e.payload#>>'{adapter,bindingRevision}')::bigint<=COALESCE(b.revision,(g.payload#>>'{gateway,adapter,bindingRevision}')::bigint)
		GROUP BY g.scope_kind,g.scope_id,g.id
		HAVING COUNT(DISTINCT jsonb_build_array(e.payload->>'installationId',COALESCE(e.payload->>'applicationId','')))=1
	), pinned AS (
		SELECT a.*,GREATEST(g.updated_at,transaction_timestamp()) AS at
		FROM authorities a JOIN `+s.table("external_conversation_gateways")+` g
			ON g.scope_kind=a.scope_kind AND g.scope_id=a.scope_id AND g.id=a.id
	)
	UPDATE `+s.table("external_conversation_gateways")+` g
	SET revision=g.revision+1,updated_at=p.at,
		payload=g.payload || jsonb_build_object(
			'gateway',(g.payload->'gateway') || jsonb_build_object('installationId',p.installation_id,'applicationId',p.application_id),
			'revision',g.revision+1,'updatedAt',p.at,
			'lifecycle',COALESCE(g.payload->'lifecycle','[]'::jsonb) || jsonb_build_array(jsonb_build_object(
				'revision',g.revision+1,'action','updated',
				'actor',jsonb_build_object('type','system','id','gateway-authority-migration'),
				'reason','Pin provider installation from unique reviewed endpoint authority','at',p.at)))
	FROM pinned p WHERE g.scope_kind=p.scope_kind AND g.scope_id=p.scope_id AND g.id=p.id`)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version,name) VALUES ($1,'pin verified conversation gateway installation authority')`, externalConversationGatewayAuthorityMigrationVersion)
	return err
}
